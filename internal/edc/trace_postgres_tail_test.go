package edc

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// BPF의 앞부분 상한이다. 쓰기는 iov 경로라 MYSQL_REQUEST_SIZE-1, 읽기는 buffer 경로라 응답 방향 상한(4096)까지 읽는다.
const (
	pgSendHead = 4095
	pgRecvHead = 4096
	pgTail     = postgresTailCapture
)

// pgBPFRecords는 한 쪽 record를 BPF가 넘기는 모양으로 바꾼다. 앞부분을 상한에서 자르고, 응답 방향 record가 앞부분보다 길면
// tails일 때 끝 pgTail byte를 같은 시각의 끝부분 record로 뒤에 붙인다.
type pgBPFRecord struct {
	record mysqlRecord
	tail   bool
}

func pgBPFRecords(records []pgFixtureRecord, socket uint64, server, tails bool, start uint64) []pgBPFRecord {
	var out []pgBPFRecord
	for i, fixture := range records {
		if fixture.server != server {
			continue
		}
		at := start + uint64(i+1)*1_000_000
		head := pgRecvHead
		if fixture.sent {
			head = pgSendHead
		}
		payload := fixture.payload[:min(len(fixture.payload), head)]
		out = append(out, pgBPFRecord{record: pgRecord(socket, server, fixture.sent, at, payload, fixture.size)})
		if tails && fixture.server == fixture.sent && fixture.size > len(payload) {
			out = append(out, pgBPFRecord{record: pgRecord(socket, server, fixture.sent, at, fixture.payload[len(fixture.payload)-min(pgTail, fixture.size):], fixture.size), tail: true})
		}
	}
	return out
}

func pgFeedBPF(tracker *postgresTracker, records []pgBPFRecord) []captureEvent {
	var events []captureEvent
	for _, bpf := range records {
		if bpf.tail {
			events = append(events, tracker.tailEvents(bpf.record, 0)...)
		} else {
			events = append(events, tracker.events(bpf.record, 0)...)
		}
	}
	return events
}

func pgRun(records []pgFixtureRecord, run int) []pgFixtureRecord {
	var out []pgFixtureRecord
	for _, record := range records {
		if record.run == run {
			out = append(out, record)
		}
	}
	return out
}

// pgFollowUp은 같은 연결에서 이어지는 작은 Query 하나다. 경계를 다시 찾았으면 짝이 맞아야 한다.
func pgFollowUp(tracker *postgresTracker, socket uint64, server bool, at uint64) []captureEvent {
	request := pgQuery("SELECT 7")
	response := bytes.Join([][]byte{pgRowDescription(), pgRow("7"), pgComplete("SELECT 1"), pgReady()}, nil)
	events := tracker.events(pgRecord(socket, server, !server, at, request, len(request)), 0)
	return append(events, tracker.events(pgRecord(socket, server, server, at+500_000, response, len(response)), 0)...)
}

// boundaries.txt "Large result verdicts" run 0: 앞부분만 캡처하면 rec3(첫 8192 byte 쓰기)에서 경계를 잃고, ReadyForQuery는 마지막
// record의 끝 6 byte다. 끝부분 record가 있으면 응답 시간과 tag를 얻고, 뒤 Query의 짝도 맞는다. 없으면 이전처럼 응답이 없다.
func TestPostgresTailRecordsAnswerLargeResults(t *testing.T) {
	run := pgRun(loadPostgresFixture(t, "large"), 0)
	for _, server := range []bool{true, false} {
		for _, tails := range []bool{true, false} {
			tracker := newPostgresTracker("", false)
			events := pgFeedBPF(tracker, pgBPFRecords(run, 1, server, tails, 0))
			want := "C r Q rSELECT_1000"
			if !tails {
				want = "C r Q"
			}
			if got := pgTokens(events); got != want {
				t.Fatalf("server=%v tails=%v = %q, want %q", server, tails, got, want)
			}
			if tails {
				query, result := events[2], events[3]
				var last uint64
				for i, record := range run {
					if record.server == server && record.server == record.sent {
						last = uint64(i+1) * 1_000_000
					}
				}
				if result.LatencyMS == nil || *result.LatencyMS != float64(last-query.BootTimeNS)/1e6 || result.BootTimeNS != last {
					t.Fatalf("server=%v latency %v at %d, want %d - %d", server, result.LatencyMS, result.BootTimeNS, last, query.BootTimeNS)
				}
				info := result.Postgres
				if info.Tag != "SELECT 1000" || info.Rows == nil || *info.Rows != 1000 || info.SQL != "SELECT repeat('?',100) FROM generate_series(1,1000)" || result.answered != 1 {
					t.Fatalf("server=%v result = %s", server, pgJSON(t, events[3:]))
				}
				// 응답 byte는 경계를 잃은 뒤의 record까지 더한다. 서버 쪽은 쓰기를 모두 더한 값과 같다.
				total, request := 0, false
				for i := len(run) - 1; i >= 0 && !request; i-- {
					switch record := run[i]; {
					case record.server != server || i > 0 && total == 0 && record.server != record.sent:
					case record.server != record.sent:
						request = true
					default:
						total += record.size
					}
				}
				if server && result.Bytes != uint64(total) {
					t.Fatalf("response bytes = %d, want %d", result.Bytes, total)
				}
			}
			follow := pgFollowUp(tracker, 1, server, 1_000_000_000)
			if pgTokens(follow) != "Q rSELECT_1" {
				t.Fatalf("server=%v tails=%v follow-up = %q", server, tails, pgTokens(follow))
			}
			pgCheckPairs(t, "large+follow-up", append(events, follow...))
		}
	}
}

// tail-only run(large 1-3)은 결과 중간에서 처음 보는 연결이라 기다리는 단위가 없다. 끝부분 record의 ReadyForQuery로 응답을
// 지어내지 않는다. 다음 record부터 경계를 따라가므로 뒤 Query는 짝을 찾는다.
func TestPostgresTailOnlyRunsInventNothing(t *testing.T) {
	records := loadPostgresFixture(t, "large")
	for run := 1; run <= 3; run++ {
		for _, server := range []bool{true, false} {
			tracker := newPostgresTracker("", false)
			bpf := pgBPFRecords(pgRun(records, run), 1, server, true, 0)
			tails := 0
			for _, record := range bpf {
				if record.tail {
					tails++
				}
			}
			events := pgFeedBPF(tracker, bpf)
			if tails == 0 || len(events) != 0 {
				t.Fatalf("run %d server=%v: %d tails, events %q", run, server, tails, pgTokens(events))
			}
			if conn := tracker.conns[1]; conn == nil || !conn.resyncNext {
				t.Fatalf("run %d server=%v: the tail did not mark the next record as a boundary", run, server)
			}
			follow := pgFollowUp(tracker, 1, server, 1_000_000_000)
			if got := pgTokens(follow); got != "Q rSELECT_1" || *follow[1].LatencyMS != 0.5 {
				t.Fatalf("run %d server=%v follow-up = %q", run, server, got)
			}
		}
	}
}

// pgBigResult는 100 byte 값의 DataRow n개인 결과를 8192 byte 쓰기로 나눈 응답 record다.
func pgBigResult(n int) [][]byte {
	result := bytes.Join([][]byte{pgRowDescription(), bytes.Repeat(pgRow(strings.Repeat("x", 100)), n), pgComplete("SELECT " + strconv.Itoa(n)), pgReady()}, nil)
	var writes [][]byte
	for len(result) > 8192 {
		writes, result = append(writes, result[:8192]), result[8192:]
	}
	return append(writes, result)
}

// sendBPF는 응답 쓰기 하나를 앞부분 record와, 앞부분보다 길면 끝부분 record로 보낸다.
func (conn *pgTestConn) sendBPF(data []byte, tails bool) []captureEvent {
	head := pgSendHead
	if !conn.server {
		head = pgRecvHead
	}
	events := conn.send(false, data, head)
	if tails && len(data) > head {
		tail := data[len(data)-min(pgTail, len(data)):]
		events = append(events, conn.tracker.tailEvents(pgRecord(conn.socket, conn.server, conn.server, conn.at, tail, len(data)), 0)...)
	}
	return events
}

// 큰 결과 중간의 쓰기가 우연히 message 경계에서 시작하면 그 record에서 경계를 다시 찾는다. 경계를 잃은 뒤 모든 쓰기의 끝을
// 보았고 ReadyForQuery가 없었으면 기다리던 Query를 버리지 않는다. 끝부분 record가 없으면 끝을 못 봤으므로 이전처럼 버린다.
func TestPostgresTailKeepsTheQueryAcrossAnAlignedWrite(t *testing.T) {
	writes := pgBigResult(2000)
	aligned := 0
	stream := bytes.Join(writes, nil)
	starts := map[int]bool{}
	for pos := 0; pos+5 <= len(stream); {
		starts[pos] = true
		pos += 1 + int(uint32(stream[pos+1])<<24|uint32(stream[pos+2])<<16|uint32(stream[pos+3])<<8|uint32(stream[pos+4]))
	}
	for i := 1; i < len(writes)-1; i++ {
		if starts[i*8192] {
			aligned++
		}
	}
	if aligned == 0 {
		t.Fatal("no write starts at a message boundary; pick another row count")
	}
	for _, server := range []bool{true, false} {
		for _, tails := range []bool{true, false} {
			conn := newPGTestConn(server, false)
			conn.login(t)
			events := conn.req(pgQuery("SELECT big"))
			for _, write := range writes {
				events = append(events, conn.sendBPF(write, tails)...)
			}
			events = append(events, conn.req(pgQuery("SELECT 2"))...)
			events = append(events, conn.resp(pgRowDescription(), pgRow("2"), pgComplete("SELECT 1"), pgReady())...)
			want := "Q rSELECT_2000 Q rSELECT_1"
			if !tails {
				want = "Q Q rSELECT_1"
			}
			if got := pgTokens(events); got != want {
				t.Fatalf("server=%v tails=%v = %q, want %q (%d aligned writes)", server, tails, got, want, aligned)
			}
			pgCheckPairs(t, "aligned", events)
			if tails && *events[1].Postgres.Rows != 2000 {
				t.Fatalf("rows = %d", *events[1].Postgres.Rows)
			}
		}
	}
}

// 경계를 따라가는 중에는 끝부분 record를 읽지 않는다. DataRow 값이 ReadyForQuery와 같은 6 byte로 쓰기를 끝내도 Query를 닫지
// 않고, 실제 ReadyForQuery가 온 record에서 닫는다.
func TestPostgresTailInsideADataRowIsIgnoredWhileFramingHolds(t *testing.T) {
	value := append(bytes.Repeat([]byte("v"), 9000), 'Z', 0, 0, 0, 5, 'I')
	value = append(value, bytes.Repeat([]byte("w"), 3000)...)
	result := bytes.Join([][]byte{pgRowDescription(), pgRow(string(value)), pgComplete("SELECT 1"), pgReady()}, nil)
	cut := bytes.Index(result, []byte{'Z', 0, 0, 0, 5, 'I'}) + 6
	for _, server := range []bool{true, false} {
		conn := newPGTestConn(server, false)
		conn.login(t)
		events := conn.req(pgQuery("SELECT big"))
		if more := conn.sendBPF(result[:cut], true); len(more) != 0 {
			t.Fatalf("server=%v: the DataRow tail closed the query: %s", server, pgJSON(t, more))
		}
		if !conn.tracker.conns[1].response.synced {
			t.Fatalf("server=%v: framing was lost inside one DataRow", server)
		}
		events = append(events, conn.sendBPF(result[cut:], true)...)
		if got := pgTokens(events); got != "Q rSELECT_1" || events[1].BootTimeNS != conn.at || *events[1].Postgres.Rows != 1 {
			t.Fatalf("server=%v = %q %s", server, got, pgJSON(t, events))
		}
	}
}

// 경계를 잃을 때 기다리던 단위가 둘이면 끝의 ReadyForQuery가 어느 단위의 것인지 모른다. 닫지 않고, 뒤 Query부터 다시 짝짓는다.
func TestPostgresTailDoesNotGuessBetweenPipelinedUnits(t *testing.T) {
	writes := pgBigResult(300)
	small := bytes.Join([][]byte{pgRowDescription(), pgRow("2"), pgComplete("SELECT 1"), pgReady()}, nil)
	last := append(append([]byte{}, writes[len(writes)-1]...), small...)
	for _, server := range []bool{true, false} {
		conn := newPGTestConn(server, false)
		conn.login(t)
		events := conn.req(pgQuery("SELECT big"), pgQuery("SELECT 2"))
		for _, write := range writes[:len(writes)-1] {
			events = append(events, conn.sendBPF(write, true)...)
		}
		events = append(events, conn.sendBPF(last, true)...)
		if got := pgTokens(events); got != "Q Q" {
			t.Fatalf("server=%v pipelined = %q %s", server, got, pgJSON(t, events))
		}
		follow := pgFollowUp(conn.tracker, 1, server, conn.at+1_000_000)
		if got := pgTokens(follow); got != "Q rSELECT_1" {
			t.Fatalf("server=%v follow-up = %q", server, got)
		}
		pgCheckPairs(t, "pipelined", append(events, follow...))
	}
}

// 경계를 잃은 뒤 끝까지 캡처한 작은 마지막 쓰기도 끝이 보인다. 끝부분 record 없이 그 끝의 ReadyForQuery로 닫는다. 오류로 끝난
// 결과는 CommandComplete가 없어 tag가 없다.
func TestPostgresVisibleEndClosesAfterLostFraming(t *testing.T) {
	rows := bytes.Repeat(pgRow(strings.Repeat("x", 100)), 100)
	head := append(pgRowDescription(), rows[:8192-len(pgRowDescription())]...)
	rest := append(append([]byte{}, rows[8192-len(pgRowDescription()):]...), pgError("ERROR", "22012", "division by zero", "")...)
	rest = append(rest, pgReady()...)
	if len(rest) > pgSendHead {
		t.Fatalf("last write is %d bytes, want it inside the head", len(rest))
	}
	for _, server := range []bool{true, false} {
		conn := newPGTestConn(server, false)
		conn.login(t)
		events := conn.req(pgQuery("SELECT 1/(n-100) FROM t"))
		events = append(events, conn.sendBPF(head, false)...)
		events = append(events, conn.sendBPF(rest, false)...)
		if got := pgTokens(events); got != "Q e22012" || events[1].BootTimeNS != conn.at || events[1].Postgres.Rows != nil || events[1].Postgres.Message != "division by zero" {
			t.Fatalf("server=%v = %q %s", server, got, pgJSON(t, events))
		}
	}
}

// 끝부분 record에는 긴 ErrorResponse의 앞이 보이지 않는다. field 목록의 끝이 보이면 오류로 닫고 field는 비운다.
func TestPostgresTailOfAnErrorClosesAsAnError(t *testing.T) {
	writes := pgBigResult(300)
	failure := append(pgError("ERROR", "22012", "division by zero "+strings.Repeat("m", 80), ""), pgReady()...)
	// 마지막 쓰기는 앞부분보다 길어야 끝부분 record가 생긴다.
	last := bytes.Join(writes[len(writes)-2:], nil)
	last = append(last[:bytes.LastIndexByte(last, 'C')], failure...)
	for _, server := range []bool{true, false} {
		conn := newPGTestConn(server, false)
		conn.login(t)
		events := conn.req(pgQuery("SELECT big"))
		for _, write := range append(writes[:len(writes)-2:len(writes)-2], last) {
			events = append(events, conn.sendBPF(write, true)...)
		}
		if got := pgTokens(events); got != "Q e" || events[1].LatencyMS == nil || events[1].Postgres.Tag != "" || events[1].Postgres.Rows != nil {
			t.Fatalf("server=%v = %q %s", server, got, pgJSON(t, events))
		}
	}
}

func TestPostgresEndTagAndReady(t *testing.T) {
	end := bytes.Join([][]byte{[]byte("xxxxC"), pgComplete("SELECT 100000"), pgReady()}, nil)
	if tag, ok := postgresEndTag(end); !ok || tag != "SELECT 100000" {
		t.Fatalf("tag = %q, %t", tag, ok)
	}
	if status, ok := postgresEndsReady(end); !ok || status != 'I' {
		t.Fatalf("ready = %q, %t", status, ok)
	}
	for _, data := range [][]byte{pgReady()[:5], append(pgReady()[:5], 'X'), append([]byte("x"), pgMsg('Z', []byte{'I', 'I'})...)} {
		if _, ok := postgresEndsReady(data); ok {
			t.Fatalf("%q read as ReadyForQuery", data)
		}
	}
	if _, ok := postgresEndTag(append([]byte("abc"), pgReady()...)); ok {
		t.Fatal("tag without CommandComplete")
	}
	if _, ok := postgresEndTag(append(pgComplete("select\x01"), pgReady()...)); ok {
		t.Fatal("a tag with a control byte was accepted")
	}
}
