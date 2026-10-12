package edc

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const (
	chHTTPPort       = 8123
	chHTTPVersion269 = "26.9.16.11"
	chHTTPVersion263 = "26.3.48.2"
)

type chHTTPFixtureRecord struct {
	version      string
	run          int
	server, sent bool
	size         int
	payload      []byte
}

// loadClickHouseHTTPFixture는 T1이 잰 record를 읽는다. 앞 4096 byte가 아니라 64 byte만 남긴 record는 모자란 부분을 x로 채운다.
func loadClickHouseHTTPFixture(t *testing.T, name string) []chHTTPFixtureRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "clickhouse", "http", name+".records"))
	if err != nil {
		t.Fatal(err)
	}
	var records []chHTTPFixtureRecord
	version, run := "", -1
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "# version:"):
			version, run = strings.Fields(line)[2], -1
		case strings.HasPrefix(line, "# run "):
			run++
		case strings.HasPrefix(line, "#"), strings.TrimSpace(line) == "":
		default:
			fields := strings.Fields(line)
			size, err := strconv.Atoi(fields[2])
			if len(fields) != 4 || err != nil {
				t.Fatalf("%s: bad record line %.60q", name, line)
			}
			payload, err := hex.DecodeString(fields[3])
			if err != nil {
				t.Fatal(err)
			}
			for len(payload) < min(size, 4096) {
				payload = append(payload, 'x')
			}
			records = append(records, chHTTPFixtureRecord{version: version, run: run, server: fields[0] == "server", sent: fields[1] == "send", size: size, payload: payload})
		}
	}
	return records
}

func chHTTPRecord(socket uint64, server, sent bool, at uint64, payload []byte, size int) mysqlRecord {
	record := mysqlRecord{bootTimeNS: at, pid: 9, process: "clickhouse", socket: socket, server: server, sent: sent, size: size, payload: payload}
	if server {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:8123", "127.0.0.1:40000", chHTTPPort, 40000
	} else {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:40000", "127.0.0.1:8123", 40000, chHTTPPort
	}
	return record
}

// replayClickHouseHTTP는 한 version과 쪽의 record를 run마다 새 tracker에 순서대로 먹인다. 시각은 record 번호 x 1 ms다.
func replayClickHouseHTTP(records []chHTTPFixtureRecord, version string, server, showSecrets bool) map[int][]captureEvent {
	trackers := map[int]*clickHouseTracker{}
	events := map[int][]captureEvent{}
	for i, record := range records {
		if record.version != version || record.server != server {
			continue
		}
		if trackers[record.run] == nil {
			trackers[record.run] = newClickHouseTracker("", showSecrets, chHTTPPort)
		}
		events[record.run] = append(events[record.run], trackers[record.run].events(chHTTPRecord(uint64(record.run+1), server, record.sent, uint64(i+1)*1_000_000, record.payload, record.size), 0)...)
	}
	return events
}

// chHTTPTokens는 event 순서를 짧게 쓴다. Q는 query, E<status>는 end, X<code>/<status>는 exception이다.
func chHTTPTokens(events []captureEvent) string {
	var tokens []string
	for _, event := range events {
		switch info := event.ClickHouse; event.Event {
		case clickHouseEventQuery:
			tokens = append(tokens, "Q")
		case clickHouseEventEnd:
			tokens = append(tokens, fmt.Sprintf("E%d", info.HTTPStatus))
		case clickHouseEventException:
			tokens = append(tokens, fmt.Sprintf("X%d/%d", info.ErrorCode, info.HTTPStatus))
		default:
			tokens = append(tokens, "?"+event.Event)
		}
	}
	return strings.Join(tokens, " ")
}

// 표의 값은 T1 fixture를 재생한 결과를 http_boundaries.txt의 분기와 맞춰 본 것이다. 서버 쪽은 모든 응답이 끝까지 걸어지고,
// client 쪽은 큰 읽기 안의 크기 줄을 잃으면 Q만 남는다(C-b). 응답 중간 오류는 표지가 record 앞 4096 byte에 보이는 만큼만 잡힌다(E-a).
func TestClickHouseHTTPReplaysFixtures(t *testing.T) {
	for _, want := range []struct {
		name string
		run  int
		// 26.9 server, 26.9 client, 26.3 server, 26.3 client
		server269, client269, server263, client263 string
	}{
		{"async_wait0", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"async_wait1", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"auth_headers", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"auth_headers", 1, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"auth_headers", 2, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"big", 0, "Q E200", "Q", "Q E200", "Q"},
		{"big", 1, "Q E200", "Q", "Q E200", "Q"},
		{"chunked_request", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"compress1", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"error", 0, "Q X60/404", "Q X60/404", "Q X60/404", "Q X60/404"},
		{"expect_continue", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"gzip", 0, "Q E200", "Q", "Q E200", "Q"},
		{"head_ping", 0, "", "", "", ""},
		{"http10", 0, "Q", "Q", "Q", "Q"},
		{"insert_format", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"insert_values", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"keepalive_ping", 0, "Q E200 Q E200", "Q E200 Q E200", "Q E200 Q E200", "Q E200 Q E200"},
		// 한 send에 요청 둘: 서버는 첫 요청만 답하므로 둘째는 응답 없이 남는다.
		{"keepalive_ping", 1, "Q Q E200", "Q Q E200", "Q Q E200", "Q Q E200"},
		{"keepalive_ping", 2, "Q E200 Q E200", "Q Q E200 E200", "Q E200 Q E200", "Q Q E200 E200"},
		{"midstream", 0, "Q X395/200", "Q X395/200", "Q X395/200", "Q X395/200"},
		// 느린 reader가 26.9에서는 표지를 657295 byte 안쪽에서 읽어 앞 4096 byte 밖이다.
		{"midstream", 1, "Q X395/200", "Q", "Q X395/200", "Q X395/200"},
		{"midstream_gzip", 0, "Q X395/500", "Q X395/500", "Q X395/500", "Q X395/500"},
		{"midstream_gzip", 1, "Q X395/500", "Q", "Q X395/500", "Q X395/500"},
		{"midstream_gzip", 2, "Q", "Q", "Q", "Q"},
		{"post_body", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"query_and_body", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"rs_insert", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"rs_select", 0, "Q E200", "Q", "Q E200", "Q"},
		{"rs_select_midstream_error", 0, "Q X395/500", "Q X395/500", "Q X395/500", "Q X395/500"},
		// 압축 stream 안의 표지는 26.3(LZ4)의 서버 쪽에서만 literal로 보인다.
		{"rs_select_midstream_error", 1, "Q", "Q", "Q X395/200", "Q"},
		{"rs_select_midstream_error", 2, "Q", "Q", "Q X395/200", "Q"},
		{"rs_select_midstream_error", 3, "Q", "Q", "Q X395/200", "Q"},
		{"rs_select_midstream_error", 4, "Q", "Q", "Q X395/200", "Q"},
		{"sleep", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"small", 0, "Q E200", "Q E200", "Q E200", "Q E200"},
		{"wait_end", 0, "Q E200", "Q", "Q E200", "Q"},
	} {
		records := loadClickHouseHTTPFixture(t, want.name)
		for _, tc := range []struct {
			version string
			server  bool
			want    string
		}{
			{chHTTPVersion269, true, want.server269}, {chHTTPVersion269, false, want.client269},
			{chHTTPVersion263, true, want.server263}, {chHTTPVersion263, false, want.client263},
		} {
			side := map[bool]string{true: "server", false: "client"}[tc.server]
			if got := chHTTPTokens(replayClickHouseHTTP(records, tc.version, tc.server, false)[want.run]); got != tc.want {
				t.Errorf("%s run %d %s %s = %q, want %q", want.name, want.run, tc.version, side, got, tc.want)
			}
		}
	}
}

func TestClickHouseHTTPFixtureEventFields(t *testing.T) {
	for _, version := range []string{chHTTPVersion269, chHTTPVersion263} {
		for _, server := range []bool{true, false} {
			replay := func(name string) []captureEvent {
				return replayClickHouseHTTP(loadClickHouseHTTPFixture(t, name), version, server, false)[0]
			}
			events := replay("small")
			query, end := events[0], events[1]
			if query.Event != clickHouseEventQuery || query.ClickHouse.SQL != "SELECT 1" || query.ClickHouse.User != "edc" || query.ClickHouse.Interface != "http" || query.Protocol != "clickhouse" {
				t.Errorf("%s server=%v small query = %#v", version, server, query.ClickHouse)
			}
			if end.ClickHouse.HTTPStatus != 200 || end.ClickHouse.SQL != "SELECT 1" || end.ClickHouse.QueryID == "" || end.ClickHouse.Interface != "http" || end.answered != 1 || end.LatencyMS == nil {
				t.Errorf("%s server=%v small end = %#v", version, server, end)
			}
			// 요청 첫 record부터 종료 chunk record까지가 latency다.
			if *end.LatencyMS <= 0 || end.TimestampNS-query.TimestampNS != uint64(*end.LatencyMS*1e6) {
				t.Errorf("%s server=%v latency = %v, query at %d end at %d", version, server, *end.LatencyMS, query.TimestampNS, end.TimestampNS)
			}

			exception := replay("error")[1].ClickHouse
			if exception.ErrorCode != 60 || exception.HTTPStatus != 404 || !strings.HasPrefix(exception.Message, "Code: 60. DB::Exception:") || exception.ErrorName != "UNKNOWN_TABLE" {
				t.Errorf("%s server=%v error = %#v", version, server, exception)
			}
			// 오류가 응답 중간에 오면 상태는 200이고 code는 오류 블록의 "Code: N."에서 읽는다.
			if middle := replay("midstream")[1].ClickHouse; middle.ErrorCode != 395 || middle.HTTPStatus != 200 || !strings.HasPrefix(middle.Message, "Code: 395. DB::Exception:") {
				t.Errorf("%s server=%v midstream = %#v", version, server, middle)
			}
			if sql := replay("query_and_body")[0].ClickHouse.SQL; sql != "SELECT 42\n FORMAT JSONCompact" {
				t.Errorf("query_and_body sql = %q", sql)
			}
			if sql := replay("insert_values")[0].ClickHouse.SQL; sql != "INSERT INTO t VALUES" {
				t.Errorf("insert_values sql = %q", sql)
			}
			if sql := replay("insert_format")[0].ClickHouse; sql.SQL != "INSERT INTO t FORMAT JSONEachRow" || sql.SQLTruncated {
				t.Errorf("insert_format = %#v", sql)
			}
			// 운영 모양: clickhouse-rs INSERT는 SQL이 URL에 있고 body는 압축 frame이다.
			insert := replay("rs_insert")[0].ClickHouse
			if insert.SQL != "INSERT INTO t FORMAT RowBinary" || insert.SQLTruncated || !insert.Compressed || insert.Database != "default" || insert.User != "edc" {
				t.Errorf("%s server=%v rs_insert = %#v", version, server, insert)
			}
			// 운영 모양: SELECT는 body가 SQL이고 응답이 압축 frame이다.
			if sel := replay("rs_select")[0].ClickHouse; sel.SQL != "SELECT number, toString(number) FROM numbers(100000)" || sel.Compressed || sel.Database != "default" {
				t.Errorf("rs_select = %#v", sel)
			}
			if events := replay("head_ping"); len(events) != 0 {
				t.Errorf("head_ping events = %#v", events)
			}
		}
	}
}

// rs_insert의 요청은 4096 byte 캡처 안에서 끝까지 걸을 수 없다. 그래도 query event는 URL에서 나오고, 같은 연결의 다음 요청이
// 새 record의 요청 줄로 시작하면 다시 맞춰 짝을 짓는다.
func TestClickHouseHTTPResyncsAfterRsInsert(t *testing.T) {
	for _, version := range []string{chHTTPVersion269, chHTTPVersion263} {
		for _, server := range []bool{true, false} {
			var joined []chHTTPFixtureRecord
			for _, name := range []string{"rs_insert", "small", "rs_select", "sleep"} {
				for _, record := range loadClickHouseHTTPFixture(t, name) {
					if record.version == version && record.server == server {
						record.run = 0
						joined = append(joined, record)
					}
				}
			}
			events := replayClickHouseHTTP(joined, version, server, false)[0]
			want := "Q E200 Q E200 Q E200 Q E200"
			if !server {
				want = "Q E200 Q E200 Q Q E200"
			}
			// client 쪽 rs_select는 큰 읽기 안의 크기 줄을 잃어 응답이 없고, 그 뒤 sleep 요청은 다음 상태 줄에서 다시 맞는다.
			if got := chHTTPTokens(events); got != want {
				t.Errorf("%s server=%v = %q, want %q", version, server, got, want)
			}
			var sqls []string
			for _, event := range events {
				if event.Event == clickHouseEventQuery {
					sqls = append(sqls, event.ClickHouse.SQL)
				}
			}
			if strings.Join(sqls, "|") != "INSERT INTO t FORMAT RowBinary|SELECT 1|SELECT number, toString(number) FROM numbers(100000)|SELECT sleep(1)" {
				t.Errorf("%s server=%v sqls = %q", version, server, sqls)
			}
		}
	}
}

type chHTTPConn struct {
	t       *testing.T
	tracker *clickHouseTracker
	server  bool
	at      uint64
}

func newCHHTTPConn(t *testing.T, server, showSecrets bool) *chHTTPConn {
	return &chHTTPConn{t: t, tracker: newClickHouseTracker("", showSecrets, chHTTPPort), server: server}
}

func (conn *chHTTPConn) feed(request bool, payload string, size int) []captureEvent {
	conn.at += 1_000_000
	return conn.tracker.events(chHTTPRecord(1, conn.server, request != conn.server, conn.at, []byte(payload), size), 0)
}

func (conn *chHTTPConn) req(payload string) []captureEvent {
	return conn.feed(true, payload, len(payload))
}
func (conn *chHTTPConn) resp(payload string) []captureEvent {
	return conn.feed(false, payload, len(payload))
}

// respond는 서버가 보내는 모양대로 상태 줄, header, chunk 크기 줄, data, CRLF, 종료 chunk를 record마다 따로 먹인다.
func (conn *chHTTPConn) respond(status, headers string, chunks ...string) []captureEvent {
	events := conn.resp(status + "\r\n")
	events = append(events, conn.resp(headers+"\r\n")...)
	for _, chunk := range chunks {
		events = append(events, conn.resp(fmt.Sprintf("%x\r\n", len(chunk)))...)
		events = append(events, conn.resp(chunk)...)
		events = append(events, conn.resp("\r\n")...)
	}
	return append(events, conn.resp("0\r\n\r\n")...)
}

const chHTTPChunked = "Transfer-Encoding: chunked\r\nX-ClickHouse-Query-Id: q-1\r\n"

func chHTTPGet(query string, headers ...string) string {
	return "GET /?query=" + query + " HTTP/1.1\r\nHost: h\r\n" + strings.Join(headers, "") + "\r\n"
}

func chHTTPPost(url, body string, headers ...string) string {
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: h\r\nContent-Length: %d\r\n%s\r\n%s", url, len(body), strings.Join(headers, ""), body)
}

func TestClickHouseHTTPHeaderSplitAcrossRecords(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	basic := base64.StdEncoding.EncodeToString([]byte("alice:" + clickHouseTestPassword))
	pieces := []string{
		"GET /?pass", "word=" + clickHouseTestPassword + "&query=SELECT%20", "1&session_id=sess-secret HTTP/1.1\r\nHo",
		"st: h\r\nX-ClickHouse-Key: key-", "secret\r\nCookie: cookie-sec", "ret\r\nAuthorization: Basic " + basic + "\r\n\r\n",
	}
	var events []captureEvent
	for _, piece := range pieces[:len(pieces)-1] {
		events = append(events, conn.req(piece)...)
		assertClickHouseHTTPNoSecret(t, conn.tracker, "after "+piece, clickHouseTestPassword, "key-secret", "sess-secret", "cookie-secret", basic)
	}
	if len(events) != 0 {
		t.Fatalf("query before the headers ended: %#v", events)
	}
	events = conn.req(pieces[len(pieces)-1])
	if len(events) != 1 || events[0].ClickHouse.SQL != "SELECT 1" || events[0].ClickHouse.User != "alice" {
		t.Fatalf("events = %#v", events)
	}
	assertClickHouseHTTPNoSecret(t, conn.tracker, "after the request", clickHouseTestPassword, "key-secret", "sess-secret", "cookie-secret", basic)
	if got := chHTTPTokens(conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "1\n")); got != "E200" {
		t.Fatalf("response = %q", got)
	}

	// Authorization 줄이 record 사이에서 끊기면 이어 읽지 않으므로 user를 알 수 없고, 비밀값도 남지 않는다.
	conn = newCHHTTPConn(t, true, false)
	conn.req("GET /?query=SELECT%201 HTTP/1.1\r\nAuthorization: Basic " + basic[:6])
	assertClickHouseHTTPNoSecret(t, conn.tracker, "split authorization", clickHouseTestPassword, basic[:6])
	events = conn.req(basic[6:] + "\r\n\r\n")
	if len(events) != 1 || events[0].ClickHouse.User != "" {
		t.Fatalf("split authorization events = %#v", events)
	}
}

func TestClickHouseHTTPReadsSeveralRequestsInOneRecord(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	body := "SELECT 7"
	events := conn.req(chHTTPPost("/", body) + chHTTPGet("SELECT%202") + "GET /ping HTTP/1.1\r\n\r\n")
	if got := chHTTPTokens(events); got != "Q Q" || events[0].ClickHouse.SQL != "SELECT 7" || events[1].ClickHouse.SQL != "SELECT 2" {
		t.Fatalf("events = %q %#v", got, events)
	}
	var ends []string
	for i := 0; i < 3; i++ {
		for _, event := range conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x\n") {
			ends = append(ends, event.ClickHouse.SQL)
		}
	}
	if strings.Join(ends, "|") != "SELECT 7|SELECT 2" {
		t.Fatalf("answered = %q", ends)
	}
}

func TestClickHouseHTTPExpectContinue(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	head := "POST /?query=SELECT%201 HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\nContent-Length: 20\r\n\r\n"
	if got := chHTTPTokens(conn.req(head)); got != "" {
		t.Fatalf("query before the body = %q", got)
	}
	if got := chHTTPTokens(conn.resp("HTTP/1.1 100 Continue\r\n\r\n")); got != "" {
		t.Fatalf("100 Continue = %q", got)
	}
	events := conn.req(" FORMAT JSONCompact\n")
	if got := chHTTPTokens(events); got != "Q" || events[0].ClickHouse.SQL != "SELECT 1\n FORMAT JSONCompact" {
		t.Fatalf("after the body = %q %#v", got, events)
	}
	if got := chHTTPTokens(conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")); got != "E200" {
		t.Fatalf("final = %q", got)
	}
}

func TestClickHouseHTTPNonQueryRequestsKeepOrder(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	var sqls []string
	collect := func(events []captureEvent) {
		for _, event := range events {
			sqls = append(sqls, event.Event[len("clickhouse_"):]+":"+event.ClickHouse.SQL)
		}
	}
	collect(conn.req(chHTTPGet("SELECT%201")))
	collect(conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "1\n"))
	collect(conn.req("GET /ping HTTP/1.1\r\nHost: h\r\n\r\n"))
	collect(conn.respond("HTTP/1.1 200 OK", "Content-Length: 4\r\n", "Ok.\n"))
	collect(conn.req("HEAD /ping HTTP/1.1\r\nHost: h\r\n\r\n"))
	collect(conn.resp("HTTP/1.1 200 OK\r\n"))
	collect(conn.resp("Content-Length: 4\r\nConnection: Keep-Alive\r\n\r\n"))
	collect(conn.req(chHTTPGet("SELECT%201")))
	collect(conn.respond("HTTP/1.1 500 Internal Server Error", chHTTPChunked+"X-ClickHouse-Exception-Code: 159\r\n", "Code: 159. DB::Exception: Timeout exceeded (TIMEOUT_EXCEEDED)\n"))
	want := "query:SELECT 1|end:SELECT 1|query:SELECT 1|exception:SELECT 1"
	if got := strings.Join(sqls, "|"); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// 응답이 오기 전에 같은 SQL이 두 번 오면 응답은 요청 순서대로 짝이 지어지고 latency는 각 요청의 첫 record부터 잰다.
func TestClickHouseHTTPPairsRepeatedSQLInOrder(t *testing.T) {
	conn := newCHHTTPConn(t, false, false)
	conn.req(chHTTPGet("SELECT%201"))
	conn.req(chHTTPGet("SELECT%201"))
	first := conn.resp("HTTP/1.1 200 OK\r\n" + chHTTPChunked + "\r\n1\r\nx\r\n0\r\n\r\n")
	second := conn.resp("HTTP/1.1 200 OK\r\n" + chHTTPChunked + "\r\n1\r\nx\r\n0\r\n\r\n")
	// 요청은 1번째와 2번째 record에서 시작했고, 응답 record는 3번째와 4번째다.
	if len(first) != 1 || len(second) != 1 || first[0].Event != clickHouseEventEnd || second[0].Event != clickHouseEventEnd {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
	if *first[0].LatencyMS != 2 || *second[0].LatencyMS != 2 {
		t.Fatalf("latency %v and %v", *first[0].LatencyMS, *second[0].LatencyMS)
	}
}

func TestClickHouseHTTPRequestBodies(t *testing.T) {
	gz := "\x1f\x8b\x08\x00binary"
	for _, tc := range []struct {
		name, request string
		sql           string
		truncated     bool
		compressed    bool
	}{
		{"post body", chHTTPPost("/", "SELECT 1 FORMAT JSON"), "SELECT 1 FORMAT JSON", false, false},
		{"query and body", chHTTPPost("/?query=SELECT+42", " FORMAT JSONCompact"), "SELECT 42\n FORMAT JSONCompact", false, false},
		{"query without body", chHTTPGet("SELECT+%2B1"), "SELECT +1", false, false},
		{"repeated query", "GET /?query=SELECT+1&query=SELECT+2 HTTP/1.1\r\n\r\n", "SELECT 1", false, false},
		{"chunked body", "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n6\r\nSELECT\r\n3\r\n 5 \r\n0\r\n\r\n", "SELECT 5", false, false},
		{"insert values in body", chHTTPPost("/", "INSERT INTO t (a, b) VALUES (1, 'secret')"), "INSERT INTO t (a, b) VALUES", false, false},
		{"insert format in body", chHTTPPost("/", "insert into t format JSONEachRow\n{\"b\":\"row-data\"}\n"), "insert into t format JSONEachRow", false, false},
		{"insert format in query", chHTTPPost("/?query=INSERT%20INTO%20t%20FORMAT%20CSV", "1,row-data\n"), "INSERT INTO t FORMAT CSV", false, false},
		{"insert with a comment", chHTTPPost("/", "/* VALUES */ INSERT INTO t -- FORMAT x\n VALUES (1)"), "/* VALUES */ INSERT INTO t -- FORMAT x\n VALUES", false, false},
		{"content encoding", chHTTPPost("/?query=SELECT+1", gz, "Content-Encoding: gzip\r\n"), "SELECT 1", true, true},
		{"decompress", chHTTPPost("/?decompress=1", gz), "", true, true},
		{"decompress insert", chHTTPPost("/?decompress=1&query=INSERT+INTO+t+FORMAT+RowBinary", gz), "INSERT INTO t FORMAT RowBinary", false, true},
		{"multipart", chHTTPPost("/?query=SELECT+1", "--b\r\nContent-Disposition: form-data\r\n\r\nSELECT 2\r\n--b--", "Content-Type: multipart/form-data; boundary=b\r\n"), "SELECT 1", true, false},
		{"user and database", "GET /?query=SELECT+1&user=url-user&database=url-db&query_id=url-id HTTP/1.1\r\nX-ClickHouse-User: header-user\r\n\r\n", "SELECT 1", false, false},
	} {
		conn := newCHHTTPConn(t, true, false)
		events := append(conn.req(tc.request), conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")...)
		if len(events) != 2 || events[0].Event != clickHouseEventQuery || events[1].Event != clickHouseEventEnd {
			t.Errorf("%s: events = %#v", tc.name, events)
			continue
		}
		info := events[0].ClickHouse
		if info.SQL != tc.sql || info.SQLTruncated != tc.truncated || info.Compressed != tc.compressed {
			t.Errorf("%s: sql=%q truncated=%v compressed=%v, want %q %v %v", tc.name, info.SQL, info.SQLTruncated, info.Compressed, tc.sql, tc.truncated, tc.compressed)
		}
		if strings.Contains(info.SQL, "row-data") || strings.Contains(info.SQL, "secret") {
			t.Errorf("%s: data leaked into %q", tc.name, info.SQL)
		}
	}
	conn := newCHHTTPConn(t, true, false)
	if info := conn.req("GET /?query=SELECT+1&user=url-user&database=url-db&query_id=url-id HTTP/1.1\r\nX-ClickHouse-User: header-user\r\nX-ClickHouse-Database: header-db\r\n\r\n")[0].ClickHouse; info.User != "header-user" || info.Database != "header-db" || info.QueryID != "url-id" {
		t.Errorf("priority = %#v", info)
	}
	conn = newCHHTTPConn(t, true, false)
	if info := conn.req("GET /?query=SELECT+1&user=url-user HTTP/1.1\r\nAuthorization: Basic " + base64.StdEncoding.EncodeToString([]byte("basic-user:pw")) + "\r\n\r\n")[0].ClickHouse; info.User != "url-user" {
		t.Errorf("url user = %#v", info)
	}
	conn = newCHHTTPConn(t, true, false)
	if info := conn.req("GET /?query=SELECT+1 HTTP/1.1\r\nAuthorization: Basic " + base64.StdEncoding.EncodeToString([]byte("basic-user:pw")) + "\r\n\r\n")[0].ClickHouse; info.User != "basic-user" {
		t.Errorf("basic user = %#v", info)
	}
	conn = newCHHTTPConn(t, true, false)
	if info := conn.req("GET /?query=SELECT+1 HTTP/1.1\r\nAuthorization: Negotiate abc\r\n\r\n")[0].ClickHouse; info.User != "" {
		t.Errorf("negotiate user = %#v", info)
	}
}

func TestClickHouseHTTPRecognizesQueryTargets(t *testing.T) {
	for _, tc := range []struct {
		request string
		query   bool
	}{
		{"GET /?query=SELECT+1 HTTP/1.1\r\n\r\n", true},
		{"GET /query?query=SELECT+1 HTTP/1.1\r\n\r\n", true},
		{"HEAD /?query=SELECT+1 HTTP/1.1\r\n\r\n", true},
		{"POST / HTTP/1.1\r\nContent-Length: 8\r\n\r\nSELECT 1", true},
		{"OPTIONS / HTTP/1.1\r\nContent-Length: 8\r\n\r\nSELECT 1", true},
		{"POST /?database=d HTTP/1.1\r\nContent-Length: 8\r\n\r\nSELECT 1", true},
		{"GET / HTTP/1.1\r\n\r\n", false},
		{"GET /ping HTTP/1.1\r\n\r\n", false},
		{"GET /play HTTP/1.1\r\n\r\n", false},
		{"GET /replicas_status HTTP/1.1\r\n\r\n", false},
		{"PUT /?query=SELECT+1 HTTP/1.1\r\n\r\n", false},
		{"POST /ping HTTP/1.1\r\nContent-Length: 8\r\n\r\nSELECT 1", false},
	} {
		conn := newCHHTTPConn(t, true, false)
		events := conn.req(tc.request)
		events = append(events, conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")...)
		if got := len(events) > 0; got != tc.query {
			t.Errorf("%.30q: events = %#v, want query = %v", tc.request, events, tc.query)
		}
	}
}

// URL이 4096 byte를 넘는 요청은 header 끝을 볼 수 없다. SQL은 URL의 보이는 부분에서 얻고 sql_truncated를 표시하며, 같은
// 연결의 다음 요청은 새 record의 요청 줄에서 다시 맞는다.
func TestClickHouseHTTPLongURLKeepsNextRequest(t *testing.T) {
	url := "GET /?password=" + clickHouseTestPassword + "&query=SELECT+1+--+" + strings.Repeat("x", 5000) + " HTTP/1.1\r\nHost: h\r\n\r\n"
	for _, server := range []bool{true, false} {
		conn := newCHHTTPConn(t, server, false)
		events := conn.feed(true, url[:4096], len(url))
		if len(events) != 1 || !events[0].ClickHouse.SQLTruncated || !strings.HasPrefix(events[0].ClickHouse.SQL, "SELECT 1 -- xxx") {
			t.Fatalf("server=%v long url = %#v", server, events)
		}
		assertClickHouseHTTPNoSecret(t, conn.tracker, "long url", clickHouseTestPassword)
		for _, event := range events {
			assertClickHouseHTTPEventNoSecret(t, event, clickHouseTestPassword)
		}
		conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")
		if next := conn.req(chHTTPGet("SELECT%202")); len(next) != 1 {
			t.Fatalf("server=%v next request = %#v", server, next)
		}
		if got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x"); len(got) != 1 || got[0].ClickHouse.SQL != "SELECT 2" || got[0].Event != clickHouseEventEnd {
			t.Fatalf("server=%v next request = %#v", server, got)
		}
	}
}

func TestClickHouseHTTPResponseBoundaryLoss(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\n" + chHTTPChunked + "\r\n"
	// 크기 줄이 캡처 밖에 있거나 크기 줄 자리에 data가 오면 경계를 잃고, 틀린 완료나 latency를 내지 않는다.
	for _, tc := range []struct {
		name, record string
		size         int
	}{
		{"size line past the capture", head + "32\r\n" + strings.Repeat("x", 50) + "\r\n", len(head) + 4 + 50 + 2 + 3000},
		{"data where a size line belongs", head + "2\r\nab" + "zz\r\n", -1},
		{"not hexadecimal", head + "zz\r\nab", -1},
		{"size line without crlf", head + "5\n", -1},
	} {
		conn := newCHHTTPConn(t, false, false)
		conn.req(chHTTPGet("SELECT%201"))
		size := tc.size
		if size < 0 {
			size = len(tc.record)
		}
		events := conn.feed(false, tc.record, size)
		events = append(events, conn.resp("0\r\n\r\n")...)
		if len(events) != 0 {
			t.Errorf("%s: events = %#v", tc.name, events)
		}
		// 다음 요청은 다음 상태 줄에서 다시 맞는다.
		conn.req(chHTTPGet("SELECT%202"))
		got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")
		if len(got) != 1 || got[0].ClickHouse.SQL != "SELECT 2" || got[0].Event != clickHouseEventEnd {
			t.Errorf("%s: next = %#v", tc.name, got)
		}
	}
}

func TestClickHouseHTTPMidstreamExceptionNeedsTheHeaderTag(t *testing.T) {
	tag := "abcdefghijklmnop"
	block := func(use string) string {
		return "\r\n__exception__\r\n" + use + "\r\nCode: 395. DB::Exception: boom (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO)\n123 " + use + "\r\n__exception__\r\n"
	}
	headers := chHTTPChunked + "X-ClickHouse-Exception-Tag: " + tag + "\r\n"
	for _, tc := range []struct {
		name, data string
		want       string
	}{
		{"same tag", block(tag), "Q X395/200"},
		{"other tag", block("zzzzzzzzzzzzzzzz"), "Q"},
		{"marker without a tag", "__exception__\r\nCode: 1.", "Q"},
	} {
		for _, server := range []bool{true, false} {
			conn := newCHHTTPConn(t, server, false)
			events := conn.req(chHTTPGet("SELECT%201"))
			events = append(events, conn.resp("HTTP/1.1 200 OK\r\n"+headers+"\r\n")...)
			events = append(events, conn.resp(fmt.Sprintf("%x\r\n", len(tc.data)))...)
			events = append(events, conn.resp(tc.data)...)
			if got := chHTTPTokens(events); got != tc.want {
				t.Errorf("%s server=%v = %q, want %q", tc.name, server, got, tc.want)
			}
			if tc.want != "Q" {
				if info := events[1].ClickHouse; info.ErrorName != "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO" || !strings.HasSuffix(info.Message, "(FUNCTION_THROW_IF_VALUE_IS_NON_ZERO)") {
					t.Errorf("%s: exception = %#v", tc.name, info)
				}
			}
		}
	}
}

func TestClickHouseHTTPResponsesWithoutALengthStayUnanswered(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	conn.req(chHTTPGet("SELECT%201"))
	events := conn.resp("HTTP/1.0 200 OK\r\nConnection: Close\r\n\r\n")
	events = append(events, conn.resp("1\n")...)
	if len(events) != 0 {
		t.Fatalf("http/1.0 events = %#v", events)
	}
	conn.req(chHTTPGet("SELECT%202"))
	if got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x"); len(got) != 1 || got[0].ClickHouse.SQL != "SELECT 2" {
		t.Fatalf("next = %#v", got)
	}
	// Content-Length 응답은 길이에 닿는 record가 완료다.
	conn.req(chHTTPGet("SELECT%203"))
	conn.resp("HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nab")
	if got := conn.resp("cdef"); len(got) != 1 || got[0].Event != clickHouseEventEnd {
		t.Fatalf("content-length end = %#v", got)
	}
}

func TestClickHouseHTTPStartsInTheMiddleOfAConnection(t *testing.T) {
	for _, server := range []bool{true, false} {
		conn := newCHHTTPConn(t, server, false)
		// 요청을 보지 못한 응답은 짝이 없어서 event 없이 끝까지 소비한다.
		if got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "HTTP/1.1 200 OK\r\nbody"); len(got) != 0 {
			t.Fatalf("response without request = %#v", got)
		}
		// 요청의 중간 record(요청 줄이 아닌 body)는 무시한다.
		if got := conn.req("xxxx body of an unseen request"); len(got) != 0 {
			t.Fatalf("body record = %#v", got)
		}
		conn.req(chHTTPGet("SELECT%204"))
		got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")
		if len(got) != 1 || got[0].ClickHouse.SQL != "SELECT 4" || got[0].Event != clickHouseEventEnd {
			t.Fatalf("server=%v next = %#v", server, got)
		}
	}
}

func TestClickHouseHTTPDispatchesByPort(t *testing.T) {
	request := []byte(chHTTPGet("SELECT%201"))
	for _, tc := range []struct {
		name     string
		httpPort uint16
		server   bool
		local    uint16
		peer     uint16
		http     bool
	}{
		{"server on the http port", 8123, true, 8123, 40000, true},
		{"client on the http port", 8123, false, 40000, 8123, true},
		{"server on the native port", 8123, true, 9000, 40000, false},
		{"client on the native port", 8123, false, 40000, 9000, false},
		{"http port off", 0, true, 8123, 40000, false},
		{"no port in the record", 8123, true, 0, 0, false},
		{"server with the http port only as the peer", 8123, true, 9000, 8123, false},
		{"custom port", 18123, false, 40000, 18123, true},
	} {
		tracker := newClickHouseTracker("", false, tc.httpPort)
		record := mysqlRecord{bootTimeNS: 1, socket: 1, server: tc.server, sent: !tc.server, size: len(request), payload: request, localPort: tc.local, peerPort: tc.peer}
		events := tracker.events(record, 0)
		conn := tracker.conns[1]
		if got := conn.http != nil; got != tc.http {
			t.Errorf("%s: http state = %v, want %v", tc.name, got, tc.http)
		}
		if tc.http != (len(events) == 1) {
			t.Errorf("%s: events = %#v", tc.name, events)
		}
	}
}

func TestClickHouseHTTPLimits(t *testing.T) {
	t.Run("queue", func(t *testing.T) {
		conn := newCHHTTPConn(t, false, false)
		var events []captureEvent
		for i := 0; i < clickHouseHTTPQueueLimit+1; i++ {
			events = append(events, conn.req(chHTTPGet(fmt.Sprintf("SELECT%%20%d", i)))...)
		}
		// 17번째 요청이 오면 가장 오래된 요청(0)을 응답 없이 버린다. 응답은 1부터 짝이 지어진다.
		if len(events) != 17 {
			t.Fatalf("events = %d", len(events))
		}
		if got := conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x"); got[0].ClickHouse.SQL != "SELECT 1" {
			t.Fatalf("first answer = %#v", got)
		}
		if size := len(conn.tracker.conns[1].http.queue); size != clickHouseHTTPQueueLimit-1 {
			t.Fatalf("queue = %d", size)
		}
	})
	t.Run("header line", func(t *testing.T) {
		// 값을 읽는 header가 16 KiB를 넘으면 그 요청의 경계를 잃는다. 이때까지 아는 SQL로 query event를 한 번 내고 보관한
		// byte를 비운다.
		conn := newCHHTTPConn(t, true, false)
		events := conn.req("GET /?query=SELECT%201 HTTP/1.1\r\nX-ClickHouse-User: ")
		for i := 0; i < 5; i++ {
			events = append(events, conn.req(strings.Repeat("a", 4000))...)
		}
		if len(events) != 1 || events[0].ClickHouse.SQL != "SELECT 1" {
			t.Fatalf("events = %#v", events)
		}
		if size := len(conn.tracker.conns[1].http.request.line.buf); size != 0 {
			t.Fatalf("buffered %d bytes after the limit", size)
		}
		next := conn.req(chHTTPGet("SELECT%202"))
		if len(next) != 1 || next[0].ClickHouse.SQL != "SELECT 2" {
			t.Fatalf("next = %#v", next)
		}
		// 읽지 않는 header는 보관하지 않으므로 길이에 상한이 필요 없다.
		conn = newCHHTTPConn(t, true, false)
		conn.req("GET /?query=SELECT%201 HTTP/1.1\r\nX-Long: ")
		for i := 0; i < 5; i++ {
			conn.req(strings.Repeat("a", 4000))
		}
		if events := conn.req("\r\n\r\n"); len(events) != 1 {
			t.Fatalf("events after a long skipped header = %#v", events)
		}
	})
	t.Run("sockets", func(t *testing.T) {
		tracker := newClickHouseTracker("", false, chHTTPPort)
		request := []byte(chHTTPGet("SELECT%201"))
		for socket := uint64(1); socket <= clickHouseSocketLimit+1; socket++ {
			tracker.events(chHTTPRecord(socket, true, false, socket, request, len(request)), 0)
		}
		if len(tracker.conns) != 1 {
			t.Fatalf("sockets = %d", len(tracker.conns))
		}
	})
}

func TestClickHouseHTTPForgetSocketReportsUndecidedQueries(t *testing.T) {
	conn := newCHHTTPConn(t, true, false)
	head := "POST /?query=SELECT%201 HTTP/1.1\r\nContent-Length: 100\r\n\r\n"
	if events := conn.req(head); len(events) != 0 {
		t.Fatalf("events = %#v", events)
	}
	events := conn.tracker.forgetSocket(1)
	if len(events) != 1 || events[0].ClickHouse.SQL != "SELECT 1" || !events[0].ClickHouse.SQLTruncated || events[0].TimestampNS != 1_000_000 {
		t.Fatalf("forget = %#v", events)
	}
	if _, ok := conn.tracker.conns[1]; ok {
		t.Fatal("socket not forgotten")
	}
	conn.tracker.forgetSocket(1)
}

func TestClickHouseHTTPMasksLiteralsUnlessAsked(t *testing.T) {
	request := chHTTPGet("SELECT%20'literal-secret'")
	for _, show := range []bool{false, true} {
		conn := newCHHTTPConn(t, true, show)
		events := conn.req(request)
		events = append(events, conn.respond("HTTP/1.1 200 OK", chHTTPChunked, "x")...)
		want := "SELECT '?'"
		if show {
			want = "SELECT 'literal-secret'"
		}
		for _, event := range events {
			if event.ClickHouse.SQL != want {
				t.Errorf("show=%v %s sql = %q, want %q", show, event.Event, event.ClickHouse.SQL, want)
			}
		}
	}
}

func TestClickHouseHTTPHeadResponseHasNoBody(t *testing.T) {
	conn := newCHHTTPConn(t, false, false)
	conn.req("HEAD /?query=SELECT%201 HTTP/1.1\r\n\r\n")
	events := conn.resp("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 4\r\n\r\n")
	if len(events) != 1 || events[0].Event != clickHouseEventEnd {
		t.Fatalf("events = %#v", events)
	}
	conn.req(chHTTPGet("SELECT%202"))
	if got := conn.respond("HTTP/1.1 304 Not Modified", ""); len(got) != 1 {
		// 304는 header 끝이 완료이고 이어지는 byte는 상태 줄이 아니라서 버려진다.
		t.Fatalf("304 = %#v", got)
	}
}

func TestClickHouseHTTPInsertCut(t *testing.T) {
	for _, tc := range []struct {
		sql   string
		final bool
		want  string
		ok    bool
	}{
		{"INSERT INTO t VALUES (1)", true, "INSERT INTO t VALUES", true},
		{"insert into t values(1)", true, "insert into t values", true},
		{"INSERT INTO t FORMAT JSONEachRow\n{}", true, "INSERT INTO t FORMAT JSONEachRow", true},
		{"INSERT INTO t FORMAT Row", false, "", false},
		{"INSERT INTO t FORMAT Row", true, "INSERT INTO t FORMAT Row", true},
		{"INSERT INTO t FORMAT", true, "", false},
		{"INSERT INTO t SELECT 'VALUES (1)'", true, "", false},
		{"INSERT INTO `values` SELECT \"FORMAT x\"", true, "", false},
		{"INSERT INTO t SELECT 1 -- VALUES\n", true, "", false},
		{"INSERT INTO t SELECT * FROM format_table", true, "", false},
		{"  /* c */ INSERT INTO t VALUES (1)", true, "  /* c */ INSERT INTO t VALUES", true},
		{"SELECT 1 FORMAT JSON", true, "", false},
		{"INSERT", true, "", false},
		{"", true, "", false},
	} {
		end, ok := clickHouseInsertCut([]byte(tc.sql), tc.final)
		if ok != tc.ok || (ok && tc.sql[:end] != tc.want) {
			t.Errorf("cut(%q, %v) = %q %v, want %q %v", tc.sql, tc.final, tc.sql[:end], ok, tc.want, tc.ok)
		}
	}
}

func TestClickHouseHTTPChunkWalker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
		want   []httpWalk
	}{
		{"whole", []string{"3\r\nabc\r\n0\r\n\r\n"}, []httpWalk{httpWalkDone}},
		{"split everywhere", []string{"3", "\r", "\nab", "c", "\r", "\n0", "\r\n", "\r\n"}, []httpWalk{httpWalkMore, httpWalkMore, httpWalkMore, httpWalkMore, httpWalkMore, httpWalkMore, httpWalkMore, httpWalkDone}},
		{"extension", []string{"3;ext=1\r\nabc\r\n0\r\n\r\n"}, []httpWalk{httpWalkDone}},
		{"trailer", []string{"0\r\nX-T: 1\r\n\r\n"}, []httpWalk{httpWalkDone}},
		{"bad size", []string{"3x\r\nabc"}, []httpWalk{httpWalkLost}},
		{"bare lf", []string{"3\nabc"}, []httpWalk{httpWalkLost}},
		{"missing crlf", []string{"3\r\nabcXY0\r\n"}, []httpWalk{httpWalkLost}},
		{"long size line", []string{strings.Repeat("0", 70)}, []httpWalk{httpWalkLost}},
		// ParseInt는 부호를 받아들인다. 음수 크기는 slice 경계를 뒤집어 panic을 냈다.
		{"negative size", []string{"-1\r\nabc"}, []httpWalk{httpWalkLost}},
		{"signed size", []string{"+3\r\nabc\r\n0\r\n\r\n"}, []httpWalk{httpWalkLost}},
	} {
		var chunks clickHouseHTTPChunks
		var got []httpWalk
		for _, piece := range tc.pieces {
			// visit을 넘겨야 실제 경로처럼 chunk data를 잘라 본다.
			result, _ := chunks.step([]byte(piece), len(piece), 0, func([]byte, bool) bool { return false })
			got = append(got, result)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 비밀값은 event와 tracker 상태 어디에도 남지 않는다. 상태는 pointer를 따라 모든 문자열과 byte(남은 용량 포함)를 훑는다.
func TestClickHouseHTTPKeepsSecretsOut(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("alice:" + clickHouseTestPassword))
	secrets := []string{clickHouseTestPassword, basic, "key-secret", "quota-secret", "param-secret", "session-secret", "cookie-secret", "setting-secret"}
	request := "POST /?password=" + clickHouseTestPassword + "&quota_key=quota-secret&param_x=param-secret&session_id=session-secret&max_threads=setting-secret&query=SELECT+1&user=bob HTTP/1.1\r\n" +
		"Host: h\r\nX-ClickHouse-Key: key-secret\r\nX-ClickHouse-Quota: quota-secret\r\nCookie: cookie-secret\r\nAuthorization: Basic " + basic + "\r\nContent-Length: 0\r\n\r\n"
	for _, show := range []bool{false, true} {
		for _, server := range []bool{true, false} {
			conn := newCHHTTPConn(t, server, show)
			var events []captureEvent
			events = append(events, conn.req(request)...)
			assertClickHouseHTTPNoSecret(t, conn.tracker, "after the request", secrets...)
			events = append(events, conn.respond("HTTP/1.1 200 OK", chHTTPChunked+"Set-Cookie: cookie-secret\r\n", "x")...)
			assertClickHouseHTTPNoSecret(t, conn.tracker, "after the response", secrets...)
			if len(events) != 2 || events[0].ClickHouse.User != "bob" {
				t.Fatalf("events = %#v", events)
			}
			for _, event := range events {
				assertClickHouseHTTPEventNoSecret(t, event, secrets...)
			}
			// 같은 요청을 조각으로 먹이면 조각 사이의 상태에도 남지 않는다.
			split := newCHHTTPConn(t, server, show)
			for i := 0; i < len(request); i += 7 {
				for _, event := range split.req(request[i:min(i+7, len(request))]) {
					assertClickHouseHTTPEventNoSecret(t, event, secrets...)
				}
				assertClickHouseHTTPNoSecret(t, split.tracker, fmt.Sprintf("after byte %d", i), secrets...)
			}
		}
	}
	for _, name := range []string{"auth_headers", "rs_select", "rs_insert"} {
		records := loadClickHouseHTTPFixture(t, name)
		for _, show := range []bool{false, true} {
			for _, version := range []string{chHTTPVersion269, chHTTPVersion263} {
				for _, server := range []bool{true, false} {
					for _, events := range replayClickHouseHTTP(records, version, server, show) {
						for _, event := range events {
							assertClickHouseHTTPEventNoSecret(t, event, clickHouseTestPassword, "ZWRjOmVkYy1maXh0dXJl")
						}
					}
				}
			}
		}
	}
}

func assertClickHouseHTTPEventNoSecret(t *testing.T, event captureEvent, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(raw), fmt.Sprintf("%#v", *event.ClickHouse)} {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("event holds %q: %.200s", secret, text)
			}
		}
	}
}

// assertClickHouseHTTPNoSecret은 tracker가 가리키는 모든 값을 훑어 비밀값이 없음을 확인한다.
func assertClickHouseHTTPNoSecret(t *testing.T, tracker *clickHouseTracker, when string, secrets ...string) {
	t.Helper()
	var texts []string
	var walk func(value reflect.Value, seen map[uintptr]bool)
	walk = func(value reflect.Value, seen map[uintptr]bool) {
		switch value.Kind() {
		case reflect.Pointer:
			if !value.IsNil() && !seen[value.Pointer()] {
				seen[value.Pointer()] = true
				walk(value.Elem(), seen)
			}
		case reflect.Interface:
			if !value.IsNil() {
				walk(value.Elem(), seen)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				walk(value.Field(i), seen)
			}
		case reflect.Map:
			for iter := value.MapRange(); iter.Next(); {
				walk(iter.Key(), seen)
				walk(iter.Value(), seen)
			}
		case reflect.Slice:
			if value.Type().Elem().Kind() == reflect.Uint8 {
				texts = append(texts, string(value.Slice(0, value.Cap()).Bytes()))
				return
			}
			for i := 0; i < value.Cap(); i++ {
				walk(value.Slice(0, value.Cap()).Index(i), seen)
			}
		case reflect.String:
			texts = append(texts, value.String())
		}
	}
	walk(reflect.ValueOf(tracker), map[uintptr]bool{})
	for _, text := range texts {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("%s: tracker state holds %q in %.80q", when, secret, text)
			}
		}
	}
}
