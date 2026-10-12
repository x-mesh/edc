package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPostgresMaskSQLTable(t *testing.T) {
	for _, tc := range []struct {
		sql, want string
		standard  bool
	}{
		{`SELECT 'a''b', 1`, `SELECT '?', 1`, true},
		{`SELECT 'C:\' , 'x'`, `SELECT '?' , '?'`, true},
		{`SELECT E'a\'b', 'c'`, `SELECT E'?', '?'`, true},
		{`SELECT e'a\\', 'c'`, `SELECT e'?', '?'`, true},
		{`SELECT U&'d\0061t', u&'x'`, `SELECT U&'?', u&'?'`, true},
		{`SELECT B'1010', b'1', X'1F', x'2', N'nat', n'b'`, `SELECT B'?', b'?', X'?', x'?', N'?', n'?'`, true},
		{`SELECT $$it's$$, 'x'`, `SELECT $$?$$, '?'`, true},
		{`SELECT $tag$a $$ b$tag$, $_t9$x$_t9$`, `SELECT $tag$?$tag$, $_t9$?$_t9$`, true},
		{`SELECT $é$secret$é$`, `SELECT $é$?$é$`, true},
		{`SELECT $1, $2::text FROM t WHERE a = $10`, `SELECT $1, $2::text FROM t WHERE a = $10`, true},
		{`SELECT a$b$c, x$$y FROM t`, `SELECT a$b$c, x$$y FROM t`, true},
		{`SELECT $1abc$ 'x'`, `SELECT $1abc$ '?'`, true},
		{`SELECT 'open literal AND more`, `SELECT '?`, true},
		{`SELECT E'open \' more`, `SELECT E'?`, true},
		{`SELECT $t$open body`, `SELECT $t$?`, true},
		{`SELECT "it's", "a""b" FROM "T" WHERE c = 'd'`, `SELECT "it's", "a""b" FROM "T" WHERE c = '?'`, true},
		{"SELECT 1 -- it's 'a comment'\nFROM t WHERE x='y'", "SELECT 1 -- it's 'a comment'\nFROM t WHERE x='?'", true},
		{`SELECT 1 --'x'`, `SELECT 1 --'x'`, true},
		{`SELECT /* a /* it's */ 'x' */ 'y'`, `SELECT /* a /* it's */ 'x' */ '?'`, true},
		{`SELECT /* unclosed /* 'x' */`, `SELECT /* unclosed /* 'x' */`, true},
		{`SELECT 42, 1.5e10, 0x1F, 'v'`, `SELECT 42, 1.5e10, 0x1F, '?'`, true},
		{`SELECT typeE'x', name'y'`, `SELECT typeE'?', name'?'`, true},
		{`SELECT '비밀 値', 'ok'`, `SELECT '?', '?'`, true},
		{`SELECT 'a$$b', $$c$$`, `SELECT '?', $$?$$`, true},
		{`SELECT 'a\'b', 'c'`, `SELECT '?', '?'`, false},
		{`SELECT N'a\'b', 'c'`, `SELECT N'?', '?'`, false},
		{`SELECT U&'a\' , 'c'`, `SELECT U&'?' , '?'`, false},
		{`SELECT E'a\'b'`, `SELECT E'?'`, false},
		{``, ``, true},
		{`'`, `'?`, true},
		{`$`, `$`, true},
		{`$$`, `$$?`, true},
		{`E'`, `E'?`, true},
	} {
		if got := string(postgresMaskSQL([]byte(tc.sql), tc.standard)); got != tc.want {
			t.Errorf("mask(%q, standard=%v) = %q, want %q", tc.sql, tc.standard, got, tc.want)
		}
	}
}

// PostgreSQL 기본값(standard_conforming_strings=on)에서는 '...' 안의 backslash가 escape가 아니다. MySQL 규칙으로 가리면
// 뒤 literal이 드러난다.
func TestPostgresMaskSQLDoesNotReuseMySQLSemantics(t *testing.T) {
	sql := []byte(`SELECT 'C:\' , 'edc-literal-secret'`)
	if got := string(mysqlMaskSQL(sql)); !strings.Contains(got, "edc-literal-secret") {
		t.Fatalf("mysqlMaskSQL = %q, the premise of this test changed", got)
	}
	if got := string(postgresMaskSQL(sql, true)); strings.Contains(got, "edc-literal-secret") || got != `SELECT '?' , '?'` {
		t.Fatalf("postgresMaskSQL = %q", got)
	}
}

func TestPostgresSQLTextMasksCapsAndEscapes(t *testing.T) {
	masked := newPostgresTracker("", false)
	shown := newPostgresTracker("", true)
	sql := []byte("SELECT 'edc-literal-secret'\x01\x1b[31m")
	if got, truncated := masked.sqlText(sql, false, true); got != `SELECT '?'\x01\x1b[31m` || truncated {
		t.Fatalf("masked = %q, %v", got, truncated)
	}
	if got, _ := shown.sqlText(sql, false, true); got != `SELECT 'edc-literal-secret'\x01\x1b[31m` {
		t.Fatalf("shown = %q", got)
	}
	long := []byte("SELECT " + strings.Repeat("é", postgresSQLLimit))
	got, truncated := shown.sqlText(long, false, true)
	if !truncated || len(got) > postgresSQLLimit || !strings.HasPrefix(got, "SELECT é") || strings.HasSuffix(got, "\\x") {
		t.Fatalf("long = %d bytes, truncated %v, tail %q", len(got), truncated, got[len(got)-8:])
	}
	if _, truncated := masked.sqlText([]byte("SELECT 1"), true, true); !truncated {
		t.Fatal("a cut message must stay truncated")
	}
}

type pgFixtureRecord struct {
	run          int
	label        string
	server, sent bool
	size         int
	payload      []byte
}

// loadPostgresFixture는 T1이 VM에서 캡처한 record를 읽는다. 한 run이 한 연결이다.
func loadPostgresFixture(t testing.TB, name string) []pgFixtureRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "postgres", name+".records"))
	if err != nil {
		t.Fatal(err)
	}
	var records []pgFixtureRecord
	run, label := -1, ""
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "# run "):
			run, label = run+1, line
		case strings.HasPrefix(line, "#"), strings.TrimSpace(line) == "":
		default:
			fields := strings.Fields(line)
			if len(fields) != 4 {
				t.Fatalf("%s: bad record line %.60q", name, line)
			}
			size, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			payload, err := hex.DecodeString(fields[3])
			if err != nil {
				t.Fatal(err)
			}
			records = append(records, pgFixtureRecord{run: run, label: label, server: fields[0] == "server", sent: fields[1] == "send", size: size, payload: payload})
		}
	}
	return records
}

func pgRecord(socket uint64, server, sent bool, at uint64, payload []byte, size int) mysqlRecord {
	record := mysqlRecord{bootTimeNS: at, pid: 9, process: "postgres", socket: socket, server: server, sent: sent, size: size, payload: payload}
	if server {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:5432", "127.0.0.1:40000", tracePostgresDefaultPort, 40000
	} else {
		record.source, record.destination, record.localPort, record.peerPort = "127.0.0.1:40000", "127.0.0.1:5432", 40000, tracePostgresDefaultPort
	}
	return record
}

// replayPostgres는 한 쪽의 record를 run마다 새 tracker에 순서대로 먹인다. 시각은 fixture 안 record 번호 x 1 ms다.
// cut이 0보다 크면 payload를 앞 cut byte로 자른다(캡처 상한 흉내).
func replayPostgres(records []pgFixtureRecord, server, showSecrets bool, cut int) map[int][]postgresEvent {
	trackers := map[int]*postgresTracker{}
	events := map[int][]postgresEvent{}
	for i, record := range records {
		if record.server != server {
			continue
		}
		if trackers[record.run] == nil {
			trackers[record.run] = newPostgresTracker("", showSecrets)
		}
		payload := record.payload
		if cut > 0 && len(payload) > cut {
			payload = payload[:cut]
		}
		events[record.run] = append(events[record.run], trackers[record.run].events(pgRecord(uint64(record.run+1), server, record.sent, uint64(i+1)*1_000_000, payload, record.size), 0)...)
	}
	return events
}

// pgTokens는 event 순서를 짧게 쓴다. 요청은 command 첫 글자(대문자), 응답은 r<tag>나 e<SQLSTATE>, TLS는 tls/<encryption>이다.
func pgTokens(events []postgresEvent) string {
	var tokens []string
	for _, event := range events {
		info := event.Postgres
		switch event.Event {
		case postgresEventConnect, postgresEventQuery:
			tokens = append(tokens, strings.ToUpper(info.Command[:1]))
		case postgresEventResult:
			tokens = append(tokens, "r"+strings.ReplaceAll(info.Tag, " ", "_"))
		case postgresEventError:
			tokens = append(tokens, "e"+info.SQLState)
		case postgresEventTLS:
			tokens = append(tokens, "tls/"+info.Encryption)
		default:
			tokens = append(tokens, "?"+event.Event)
		}
	}
	return strings.Join(tokens, " ")
}

// 연결 topic이 몇 줄로 끝나도록 mysql/clickhouse tracker와 같은 모양을 고정한다.
var (
	_ func(string, bool) *postgresTracker                        = newPostgresTracker
	_ func(*postgresTracker, mysqlRecord, int64) []postgresEvent = (*postgresTracker).events
	_ func(*postgresTracker, uint64)                             = (*postgresTracker).forgetSocket
)

func pgCString(text string) []byte { return append([]byte(text), 0) }

func pgMsg(typ byte, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	out := []byte{typ, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(out[1:], uint32(len(body)+4))
	return append(out, body...)
}

func pgUntyped(code uint32, rest []byte) []byte {
	out := make([]byte, 8, 8+len(rest))
	binary.BigEndian.PutUint32(out, uint32(8+len(rest)))
	binary.BigEndian.PutUint32(out[4:], code)
	return append(out, rest...)
}

func pgStartup(params ...string) []byte {
	var rest []byte
	for _, param := range params {
		rest = append(rest, pgCString(param)...)
	}
	return pgUntyped(196608, append(rest, 0))
}

func pgInt32(value uint32) []byte { return binary.BigEndian.AppendUint32(nil, value) }
func pgInt16(value uint16) []byte { return binary.BigEndian.AppendUint16(nil, value) }

func pgQuery(sql string) []byte { return pgMsg('Q', pgCString(sql)) }
func pgParse(name, sql string) []byte {
	return pgMsg('P', pgCString(name), pgCString(sql), pgInt16(0))
}
func pgBind(portal, statement string, params ...string) []byte {
	parts := [][]byte{pgCString(portal), pgCString(statement), pgInt16(0), pgInt16(uint16(len(params)))}
	for _, param := range params {
		parts = append(parts, pgInt32(uint32(len(param))), []byte(param))
	}
	return pgMsg('B', append(parts, pgInt16(0))...)
}
func pgExecute(portal string) []byte { return pgMsg('E', pgCString(portal), pgInt32(0)) }
func pgSync() []byte                 { return pgMsg('S') }
func pgReady() []byte                { return pgMsg('Z', []byte{'I'}) }
func pgComplete(tag string) []byte   { return pgMsg('C', pgCString(tag)) }
func pgRow(value string) []byte {
	return pgMsg('D', pgInt16(1), pgInt32(uint32(len(value))), []byte(value))
}
func pgRowDescription() []byte {
	return pgMsg('T', pgInt16(1), pgCString("v"), pgInt32(0), pgInt16(0), pgInt32(25), pgInt16(0xffff), pgInt32(0xffffffff), pgInt16(0))
}
func pgError(severity, state, message, detail string) []byte {
	parts := [][]byte{{'S'}, pgCString(severity), {'V'}, pgCString(severity), {'C'}, pgCString(state), {'M'}, pgCString(message)}
	if detail != "" {
		parts = append(parts, []byte{'D'}, pgCString(detail))
	}
	parts = append(parts, []byte{'H'}, pgCString("edc-hint-secret"), []byte{'W'}, pgCString("edc-where-secret"), []byte{0})
	return pgMsg('E', parts...)
}
func pgParam(name, value string) []byte { return pgMsg('S', pgCString(name), pgCString(value)) }
func pgAuthOK() []byte                  { return pgMsg('R', pgInt32(0)) }
func pgKeyData() []byte                 { return pgMsg('K', pgInt32(4242), []byte("edc-key!")) }

// pgTestConn은 한 쪽의 한 연결이다. record마다 시각을 1 ms씩 늘린다.
type pgTestConn struct {
	tracker *postgresTracker
	socket  uint64
	server  bool
	at      uint64
}

func newPGTestConn(server, showSecrets bool) *pgTestConn {
	return &pgTestConn{tracker: newPostgresTracker("", showSecrets), socket: 1, server: server}
}

func (conn *pgTestConn) send(request bool, data []byte, captured int) []postgresEvent {
	conn.at += 1_000_000
	payload := data
	if captured >= 0 && captured < len(data) {
		payload = data[:captured]
	}
	return conn.tracker.events(pgRecord(conn.socket, conn.server, request != conn.server, conn.at, payload, len(data)), 0)
}

func (conn *pgTestConn) req(parts ...[]byte) []postgresEvent {
	return conn.send(true, bytes.Join(parts, nil), -1)
}
func (conn *pgTestConn) resp(parts ...[]byte) []postgresEvent {
	return conn.send(false, bytes.Join(parts, nil), -1)
}

// login은 startup부터 첫 ReadyForQuery까지 보내고 connect의 응답을 확인한다.
func (conn *pgTestConn) login(t *testing.T) {
	t.Helper()
	events := append(conn.req(pgStartup("user", "edc", "database", "db1", "options", "-c edc.x=edc-option-secret")),
		conn.resp(pgAuthOK(), pgParam("server_version", "16.15"), pgKeyData(), pgReady())...)
	if pgTokens(events) != "C r" || events[1].Postgres.ServerVersion != "16.15" || events[1].Postgres.User != "edc" || events[1].Postgres.Database != "db1" {
		t.Fatalf("login = %s %#v", pgTokens(events), events)
	}
}

func pgJSON(t testing.TB, events []postgresEvent) string {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// pgFixtures는 T1 fixture 전부다. 표의 run 순서는 fixture 파일의 "# run" 순서다.
var pgFixtures = []string{"startup_cleartext", "startup_scram", "simple", "error", "extended", "pipeline", "large", "ssl", "ssl_refused", "gssenc_refused", "copy"}

// 표의 값은 T1 fixture를 서버 쪽과 client 쪽에서 재생한 결과를 boundaries.txt의 message 순서와 맞춰 본 것이다. 두 쪽이
// 같아야 한다. 큰 결과의 tail-only run(large 1-3)은 결과 중간에서 처음 보는 연결이라 event가 없어야 한다.
func TestPostgresFixtureReplay(t *testing.T) {
	want := map[string][]string{
		"startup_cleartext": {"C r Q rSELECT_1"},
		"startup_scram":     {"C r Q rSELECT_1"},
		"simple":            {"C r Q rSELECT_1 Q rSELECT_1 Q rSELECT_1", "C r Q e57014", ""},
		"error":             {"C r Q e42601 Q rINSERT_0_1 Q e23505 Q e22012"},
		"extended":          {"C r E rSELECT_1", "C r", "C r P r E rSELECT_1 E rSELECT_1", "C r P r E rSELECT_1 E rSELECT_1 Q rBEGIN E r E rSELECT_1 Q rCOMMIT F r"},
		"pipeline":          {"C r", "C r E rSELECT_1 E rSELECT_1", "C r E E E rSELECT_1 e22012 rSELECT_1"},
		"large":             {"C r Q rSELECT_1000", "", "", ""},
		"ssl":               {"tls/ssl"},
		"ssl_refused":       {"C r Q rSELECT_1"},
		"gssenc_refused":    {"tls/gssapi", "C r Q rSELECT_1"},
		"copy":              {"C r Q rCOPY_2 Q rCOPY_4"},
	}
	for _, name := range pgFixtures {
		records := loadPostgresFixture(t, name)
		for _, server := range []bool{true, false} {
			events := replayPostgres(records, server, false, 0)
			for run, tokens := range want[name] {
				if got := pgTokens(events[run]); got != tokens {
					t.Errorf("%s server=%v run %d = %q, want %q", name, server, run, got, tokens)
				}
				requests, answered := 0, uint64(0)
				for _, event := range events[run] {
					switch event.Event {
					case postgresEventConnect, postgresEventQuery:
						requests++
					case postgresEventResult, postgresEventError:
						answered += event.answered
						if event.LatencyMS == nil || *event.LatencyMS <= 0 {
							t.Errorf("%s server=%v run %d: %s latency %v", name, server, run, event.Event, event.LatencyMS)
						}
					}
				}
				if int(answered) != requests {
					t.Errorf("%s server=%v run %d: %d requests, %d answered", name, server, run, requests, answered)
				}
			}
		}
	}
}

func TestPostgresFixtureReplayFields(t *testing.T) {
	pick := func(name string, run int, server bool) []postgresEvent {
		return replayPostgres(loadPostgresFixture(t, name), server, false, 0)[run]
	}
	for _, server := range []bool{true, false} {
		connect := pick("startup_scram", 0, server)[1].Postgres
		if connect.Command != postgresCommandConnect || connect.User != "edc" || connect.Database != "edc" || connect.ServerVersion != "16.15 (Ubuntu 16.15-1.pgdg24.04+2)" {
			t.Fatalf("connect = %#v", connect)
		}
		simple := pick("simple", 0, server)
		if simple[4].Postgres.SQL != "SELECT 1; SELECT 2" || *simple[5].Postgres.Rows != 2 || simple[7].Postgres.SQL != "SELECT '?' AS s" {
			t.Fatalf("simple = %s", pgJSON(t, simple))
		}
		large := pick("large", 0, server)[3].Postgres
		if large.Tag != "SELECT 1000" || *large.Rows != 1000 || large.SQL != "SELECT repeat('?',100) FROM generate_series(1,1000)" {
			t.Fatalf("large = %#v", large)
		}
		errs := pick("error", 0, server)
		duplicate, zero := errs[7].Postgres, errs[9].Postgres
		if duplicate.SQLState != "23505" || duplicate.Severity != "ERROR" || duplicate.Detail != "" || duplicate.SQL != "INSERT INTO u VALUES ('?')" ||
			duplicate.Message != `duplicate key value violates unique constraint \"u_v_key\"` && duplicate.Message != `duplicate key value violates unique constraint "u_v_key"` {
			t.Fatalf("duplicate = %#v", duplicate)
		}
		if zero.SQLState != "22012" || *zero.Rows != 499 {
			t.Fatalf("division by zero = %#v", zero)
		}
		prepared := pick("extended", 2, server)
		if p := prepared[2].Postgres; p.Command != postgresCommandPrepare || p.Statement != "P_0" || p.SQL != "SELECT length($1::text) AS n;" {
			t.Fatalf("prepare = %#v", p)
		}
		if e := prepared[4].Postgres; e.Command != postgresCommandExecute || e.Statement != "P_0" || e.Executes != 1 || e.SQL != "SELECT length($1::text) AS n;" {
			t.Fatalf("execute = %#v", e)
		}
		named := pick("extended", 3, server)
		if e := named[4].Postgres; e.Statement != "edc_stmt" || e.SQL != "SELECT length($1::text) AS n" {
			t.Fatalf("named execute = %#v", e)
		}
		if f := named[len(named)-2].Postgres; f.Command != postgresCommandFunction || f.SQL != "" {
			t.Fatalf("function = %#v", f)
		}
		pipeline := pick("pipeline", 2, server)
		for i, sql := range []string{"SELECT 1", "SELECT 1/0", "SELECT 4"} {
			if request, response := pipeline[2+i].Postgres, pipeline[5+i].Postgres; request.SQL != sql || response.SQL != sql {
				t.Fatalf("pipeline batch %d = %#v / %#v", i, request, response)
			}
		}
		if failed := pipeline[6].Postgres; failed.Executes != 2 || failed.SQLState != "22012" {
			t.Fatalf("failed batch = %#v", failed)
		}
		if tls := pick("ssl", 0, server)[0].Postgres; tls.Encryption != "ssl" || tls.Message == "" {
			t.Fatalf("tls = %#v", tls)
		}
	}
}

// boundaries.txt "Large result verdicts": 서버 send는 8192 byte 이하라 8192와 16384에서는 header를 잃지 않고, 1024와
// 4096에서는 rec3에서 잃는다. client read는 16384까지라 8192에서도 rec13에서 잃는다. 잃으면 응답 없음이어야 한다.
func TestPostgresTruncationMatchesBoundaries(t *testing.T) {
	records := loadPostgresFixture(t, "large")
	for _, tc := range []struct {
		server bool
		cut    int
		want   string
	}{
		{true, 1024, "C r Q"}, {true, 4096, "C r Q"}, {true, 4553, "C r Q"}, {true, 8192, "C r Q rSELECT_1000"}, {true, 16384, "C r Q rSELECT_1000"},
		{false, 1024, "C r Q"}, {false, 4096, "C r Q"}, {false, 8192, "C r Q"}, {false, 12826, "C r Q"}, {false, 16384, "C r Q rSELECT_1000"},
	} {
		if got := pgTokens(replayPostgres(records, tc.server, false, tc.cut)[0]); got != tc.want {
			t.Errorf("server=%v cut %d = %q, want %q", tc.server, tc.cut, got, tc.want)
		}
	}
	// 마지막 record만 자르면 ReadyForQuery 위치(boundaries.txt: 서버 4553/4559, client 12826/12832)가 그대로 갈림길이다.
	// tracker는 ReadyForQuery의 header 5 byte가 보이면 message를 알아보므로 status byte는 없어도 된다.
	for _, tc := range []struct {
		server bool
		cut    int
		want   string
	}{
		{true, 4553, "C r Q"}, {true, 4557, "C r Q"}, {true, 4558, "C r Q rSELECT_1000"}, {true, 4559, "C r Q rSELECT_1000"},
		{false, 12826, "C r Q"}, {false, 12830, "C r Q"}, {false, 12831, "C r Q rSELECT_1000"}, {false, 12832, "C r Q rSELECT_1000"},
	} {
		var side []pgFixtureRecord
		for _, record := range records {
			if record.run == 0 && record.server == tc.server {
				side = append(side, record)
			}
		}
		// 응답 방향은 서버가 보내고 client가 받는 쪽이다.
		last := -1
		for i, record := range side {
			if record.sent == tc.server {
				last = i
			}
		}
		cut := side[last]
		cut.payload = cut.payload[:tc.cut]
		side[last] = cut
		events := replayPostgres(side, tc.server, false, 0)[0]
		if got := pgTokens(events); got != tc.want {
			t.Errorf("last record server=%v cut %d = %q, want %q", tc.server, tc.cut, got, tc.want)
		}
		for _, event := range events[2:] {
			if event.Event != postgresEventQuery && tc.want == "C r Q" {
				t.Errorf("dropped unit must have no response, got %s", event.Event)
			}
		}
	}
}

// pgCheckPairs는 응답 event마다 아직 답을 받지 않은 가장 오래된 요청이 같은 command와 SQL인지 본다.
func pgCheckPairs(t *testing.T, label string, events []postgresEvent) {
	t.Helper()
	var open []*tracePostgresEvent
	for _, event := range events {
		switch event.Event {
		case postgresEventConnect, postgresEventQuery:
			open = append(open, event.Postgres)
		case postgresEventResult, postgresEventError:
			found := false
			for i, request := range open {
				if request.Command == event.Postgres.Command && request.SQL == event.Postgres.SQL {
					open, found = open[i+1:], true
					break
				}
			}
			if !found || event.LatencyMS == nil {
				t.Fatalf("%s: response %s %#v has no request", label, event.Event, event.Postgres)
			}
		}
	}
}

// 연결 중간부터 보는 경우를 run마다 앞 k개 record를 버려서 만든다. 응답은 언제나 앞선 요청과 짝이어야 하고, 마지막
// 요청이 남아 있으면 처음부터 본 재생처럼 답을 받아야 한다. large는 하나뿐인 query의 응답이 record 경계를 넘어 이어지므로
// (boundaries.txt Findings 1) 그 query부터 보는 연결은 응답 방향을 다시 맞출 record가 없다. 그 복구는
// TestPostgresMidStreamRecoversOnNextCleanQuery가 본다.
func TestPostgresMidStreamPrefixes(t *testing.T) {
	for _, name := range pgFixtures {
		records := loadPostgresFixture(t, name)
		runs := map[int][]pgFixtureRecord{}
		for _, record := range records {
			runs[record.run] = append(runs[record.run], record)
		}
		for run, all := range runs {
			for _, server := range []bool{true, false} {
				full := replayPostgres(all, server, false, 0)[run]
				for k := range len(all) + 1 {
					label := fmt.Sprintf("%s run %d server=%v drop %d", name, run, server, k)
					events := replayPostgres(all[k:], server, false, 0)[run]
					pgCheckPairs(t, label, events)
					lastFull, lastHere := pgLastRequest(full), pgLastRequest(events)
					if name != "large" && lastHere != nil && lastFull != nil && lastHere.Postgres.SQL == lastFull.Postgres.SQL && pgAnswered(full, lastFull) && !pgAnswered(events, lastHere) {
						t.Errorf("%s: the last request %q was not answered", label, lastHere.Postgres.SQL)
					}
				}
			}
		}
	}
}

func pgLastRequest(events []postgresEvent) *postgresEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event == postgresEventConnect || events[i].Event == postgresEventQuery {
			return &events[i]
		}
	}
	return nil
}

func pgAnswered(events []postgresEvent, request *postgresEvent) bool {
	after := false
	for i := range events {
		if &events[i] == request {
			after = true
			continue
		}
		if after && (events[i].Event == postgresEventResult || events[i].Event == postgresEventError) && events[i].Postgres.SQL == request.Postgres.SQL {
			return true
		}
	}
	return false
}

// pgFixtureSecrets는 fixture에 심은 값과 SCRAM에서 나온 값이다. SASL nonce와 proof, BackendKeyData의 key는 fixture
// byte에서 꺼낸다.
func pgFixtureSecrets(t *testing.T) []string {
	secrets := []string{"edc-fixture", "edc-param-secret", "edc-copy-secret", "edc-func-secret", "edc-option-secret"}
	for _, record := range loadPostgresFixture(t, "startup_scram") {
		data := string(record.payload)
		for _, marker := range []string{"n,,n=,r=", ",p="} {
			if at := strings.Index(data, marker); at >= 0 && record.sent && !record.server {
				value := data[at+len(marker):]
				if end := strings.IndexAny(value, ",\x00"); end >= 0 {
					value = value[:end]
				}
				secrets = append(secrets, value)
			}
		}
		if at := strings.Index(data, "K\x00\x00\x00\x0c"); at >= 0 && record.server && record.sent {
			secrets = append(secrets, data[at+5:at+13])
		}
	}
	if len(secrets) != 8 {
		t.Fatalf("SCRAM nonce, proof and key not found: %q", secrets)
	}
	return secrets
}

// 모든 fixture를 record 하나씩 먹이면서 매번 tracker 상태 전체와, 끝에 event JSON 전체를 훑는다. password, SASL 값,
// Bind 값, CopyData, FunctionCall 인자, cancel key는 --show-secrets와 상관없이 없어야 한다. SQL literal과 detail은
// --show-secrets일 때만 event에 보인다.
func TestPostgresTrackerKeepsSecretsOutOfEventsAndState(t *testing.T) {
	always := pgFixtureSecrets(t)
	for _, showSecrets := range []bool{false, true} {
		forbidden := always
		if !showSecrets {
			forbidden = append(slices.Clone(always), "edc-literal-secret", "edc-detail-secret")
		}
		var all []postgresEvent
		for _, name := range pgFixtures {
			for _, server := range []bool{true, false} {
				trackers := map[int]*postgresTracker{}
				for i, record := range loadPostgresFixture(t, name) {
					if record.server != server {
						continue
					}
					if trackers[record.run] == nil {
						trackers[record.run] = newPostgresTracker("", showSecrets)
					}
					tracker := trackers[record.run]
					all = append(all, tracker.events(pgRecord(uint64(record.run+1), server, record.sent, uint64(i+1)*1_000_000, record.payload, record.size), 0)...)
					for _, secret := range forbidden {
						if clickHouseStateHas(reflect.ValueOf(tracker), secret) {
							t.Fatalf("show=%v %s record %d: tracker state holds %q", showSecrets, name, i, secret)
						}
					}
				}
			}
		}
		raw := pgJSON(t, all)
		for _, secret := range forbidden {
			if strings.Contains(raw, secret) {
				t.Fatalf("show=%v: events hold %q", showSecrets, secret)
			}
		}
		if showSecrets && (!strings.Contains(raw, `"sql":"SELECT 'edc-literal-secret' AS s"`) || !strings.Contains(raw, `"detail":"Key (v)=(edc-detail-secret) already exists."`)) {
			t.Fatal("--show-secrets must show the literal in sql and the detail")
		}
	}
}

func TestPostgresEncryptionEmitsOneEventAndIgnoresTheRest(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		start      func(conn *pgTestConn) []postgresEvent
	}{
		{"ssl", "tls/ssl", func(conn *pgTestConn) []postgresEvent {
			return append(conn.req(pgUntyped(80877103, nil)), conn.resp([]byte{'S'})...)
		}},
		{"gssapi", "tls/gssapi", func(conn *pgTestConn) []postgresEvent {
			return append(conn.req(pgUntyped(80877104, nil)), conn.resp([]byte{'G'})...)
		}},
		{"direct", "tls/ssl", func(conn *pgTestConn) []postgresEvent {
			return conn.req([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'})
		}},
	} {
		for _, server := range []bool{true, false} {
			conn := newPGTestConn(server, false)
			events := tc.start(conn)
			// 암호문이 우연히 PostgreSQL message처럼 보여도 읽지 않는다.
			events = append(events, conn.req(pgQuery("SELECT 1"))...)
			events = append(events, conn.resp(pgRowDescription(), pgRow("1"), pgComplete("SELECT 1"), pgReady())...)
			events = append(events, conn.req(pgStartup("user", "edc"))...)
			if got := pgTokens(events); got != tc.want {
				t.Fatalf("%s server=%v = %q, want %q", tc.name, server, got, tc.want)
			}
		}
	}
	conn := newPGTestConn(true, false)
	events := append(conn.req(pgUntyped(80877103, nil)), conn.resp([]byte{'N'})...)
	events = append(events, conn.req(pgStartup("user", "edc"))...)
	events = append(events, conn.resp(pgAuthOK(), pgReady())...)
	if got := pgTokens(events); got != "C r" || events[0].Postgres.Database != "edc" {
		t.Fatalf("N answer = %q %#v", got, events)
	}
}

func TestPostgresCancelRequestKeepsNothing(t *testing.T) {
	for _, key := range []string{"eKey", strings.Repeat("edc-key3.2", 25)} {
		conn := newPGTestConn(true, true)
		if events := conn.req(pgUntyped(80877102, append(pgInt32(4242), key...))); len(events) != 0 {
			t.Fatalf("cancel = %#v", events)
		}
		if events := conn.req(pgQuery("SELECT 1")); len(events) != 0 {
			t.Fatalf("after cancel = %#v", events)
		}
		if clickHouseStateHas(reflect.ValueOf(conn.tracker), key) {
			t.Fatal("cancel key kept")
		}
	}
}

func TestPostgresStartupErrorAnswersConnect(t *testing.T) {
	conn := newPGTestConn(true, false)
	events := conn.req(pgStartup("user", "edc"))
	events = append(events, conn.resp(pgMsg('R', pgInt32(10), pgCString("SCRAM-SHA-256"), []byte{0}))...)
	events = append(events, conn.req(pgMsg('p', pgCString("SCRAM-SHA-256"), pgInt32(9), []byte("n,,n=,r=x")))...)
	events = append(events, conn.resp(pgError("FATAL", "28P01", `password authentication failed for user "edc"`, ""))...)
	if got := pgTokens(events); got != "C e28P01" || events[1].Postgres.Command != postgresCommandConnect || events[1].LatencyMS == nil || events[1].Postgres.Database != "edc" {
		t.Fatalf("auth failure = %q %s", got, pgJSON(t, events))
	}
}

func TestPostgresFramingAcrossRecords(t *testing.T) {
	conn := newPGTestConn(true, false)
	conn.login(t)
	query := pgQuery("SELECT 'edc-literal-secret', 2")
	// header가 두 record에 걸친다.
	events := append(conn.send(true, query[:3], -1), conn.send(true, query[3:], -1)...)
	// 응답의 CommandComplete가 header 중간과 tag 중간에서 나뉜다.
	response := bytes.Join([][]byte{pgRowDescription(), pgRow("x"), pgComplete("SELECT 1"), pgReady()}, nil)
	split := len(pgRowDescription()) + len(pgRow("x")) + 2
	events = append(events, conn.resp(response[:split])...)
	events = append(events, conn.resp(response[split:split+6])...)
	events = append(events, conn.resp(response[split+6:])...)
	if got := pgTokens(events); got != "Q rSELECT_1" || events[0].Postgres.SQL != "SELECT '?', 2" || *events[1].Postgres.Rows != 1 {
		t.Fatalf("split = %q %s", got, pgJSON(t, events))
	}
	// body가 캡처 밖이어도 길이로 건너뛰고 경계를 지킨다. SQL은 잘렸다고 표시한다.
	long := pgQuery("SELECT '" + strings.Repeat("x", 20000) + "'")
	events = conn.send(true, long, 16384)
	events = append(events, conn.req(pgQuery("SELECT 3"))...)
	events = append(events, conn.resp(pgComplete("SELECT 1"), pgReady(), pgComplete("SELECT 1"), pgReady())...)
	if got := pgTokens(events); got != "Q Q rSELECT_1 rSELECT_1" || !events[0].Postgres.SQLTruncated || events[1].Postgres.SQL != "SELECT 3" {
		t.Fatalf("long = %q %s", got, pgJSON(t, events))
	}
}

// 응답의 header가 캡처 밖이면 기다리던 단위는 응답 없음이 되고, 응답 방향은 깨끗한 record에서 다시 맞춘다.
func TestPostgresTruncatedResponseDropsPendingAndResyncs(t *testing.T) {
	big := bytes.Join([][]byte{pgRowDescription(), bytes.Repeat(pgRow(strings.Repeat("x", 100)), 100), pgComplete("SELECT 100"), pgReady()}, nil)
	conn := newPGTestConn(true, false)
	conn.login(t)
	events := conn.req(pgQuery("SELECT big"))
	events = append(events, conn.send(false, big, 4096)...)
	events = append(events, conn.req(pgQuery("SELECT 2"))...)
	events = append(events, conn.resp(pgRowDescription(), pgRow("2"), pgComplete("SELECT 1"), pgReady())...)
	if got := pgTokens(events); got != "Q Q rSELECT_1" || events[2].Postgres.SQL != "SELECT 2" {
		t.Fatalf("truncated = %q %s", got, pgJSON(t, events))
	}

	// pipeline client가 잘린 응답의 꼬리 전에 새 Query를 보낸 경우. 꼬리가 깨끗한 record 하나로 오면 그 ReadyForQuery가 새
	// Query를 닫는다(남는 위험). 그래도 짝은 많아야 하나이고 SQL은 새 Query의 것이다.
	conn = newPGTestConn(true, false)
	conn.login(t)
	head, tail := big[:len(big)-len(pgComplete("SELECT 100"))-len(pgReady())], bytes.Join([][]byte{pgComplete("SELECT 100"), pgReady()}, nil)
	events = conn.req(pgQuery("SELECT big"))
	events = append(events, conn.send(false, head, 4096)...)
	events = append(events, conn.req(pgQuery("SELECT new"))...)
	events = append(events, conn.resp(tail)...)
	events = append(events, conn.resp(pgRowDescription(), pgRow("n"), pgComplete("SELECT 1"), pgReady())...)
	pgCheckPairs(t, "pipelined tail", events)
	if responses := strings.Count(pgTokens(events), "r"); responses > 1 {
		t.Fatalf("pipelined tail = %q", pgTokens(events))
	}

	// 새 Query 뒤에 건너뛴 꼬리 record가 있으면 그 Query는 버린다. 다음 Query는 다시 짝을 찾는다.
	conn = newPGTestConn(true, false)
	conn.login(t)
	events = conn.req(pgQuery("SELECT big"))
	events = append(events, conn.send(false, head[:8192], 4096)...)
	events = append(events, conn.req(pgQuery("SELECT new"))...)
	events = append(events, conn.resp(head[8193:])...)
	events = append(events, conn.resp(tail)...)
	events = append(events, conn.resp(pgRowDescription(), pgRow("n"), pgComplete("SELECT 1"), pgReady())...)
	events = append(events, conn.req(pgQuery("SELECT next"))...)
	events = append(events, conn.resp(pgComplete("SELECT 0"), pgReady())...)
	if got := pgTokens(events); got != "Q Q Q rSELECT_0" || events[3].Postgres.SQL != "SELECT next" {
		t.Fatalf("skipped tail = %q %s", got, pgJSON(t, events))
	}
}

// 결과 중간에서 처음 보는 연결은 두 방향 모두 경계를 모른다. 응답을 지어내지 않고, 다음 깨끗한 Query부터 짝짓는다.
func TestPostgresMidStreamRecoversOnNextCleanQuery(t *testing.T) {
	rows := bytes.Repeat(pgRow(strings.Repeat("x", 100)), 200)
	for _, server := range []bool{true, false} {
		conn := newPGTestConn(server, false)
		events := conn.resp(rows[57:8249])
		events = append(events, conn.resp(append(rows[8249:], append(pgComplete("SELECT 200"), pgReady()...)...))...)
		events = append(events, conn.req(pgQuery("SELECT big"))...)
		events = append(events, conn.resp(pgRowDescription(), rows[:8000])...)
		events = append(events, conn.resp(rows[8000:], pgComplete("SELECT 200"), pgReady())...)
		events = append(events, conn.req(pgQuery("SELECT 1"))...)
		events = append(events, conn.resp(pgRowDescription(), pgRow("1"), pgComplete("SELECT 1"), pgReady())...)
		if got := pgTokens(events); got != "Q Q rSELECT_1" || events[2].Postgres.SQL != "SELECT 1" {
			t.Fatalf("server=%v mid-stream = %q %s", server, got, pgJSON(t, events))
		}
	}
}

func TestPostgresPendingOverflowKeepsPairsAligned(t *testing.T) {
	conn := newPGTestConn(true, false)
	conn.login(t)
	var batches, readies []byte
	for i := range postgresPendingLimit + 1 {
		batches = append(batches, bytes.Join([][]byte{pgParse("", fmt.Sprintf("SELECT %d", i)), pgBind("", ""), pgExecute(""), pgSync()}, nil)...)
		readies = append(readies, bytes.Join([][]byte{pgMsg('1'), pgMsg('2'), pgComplete("SELECT 1"), pgReady()}, nil)...)
	}
	requests := conn.req(batches)
	if len(requests) != postgresPendingLimit+1 || conn.tracker.pending > postgresPendingLimit {
		t.Fatalf("requests = %d, pending = %d", len(requests), conn.tracker.pending)
	}
	responses := conn.resp(readies)
	if len(responses) != 1 || responses[0].Postgres.SQL != fmt.Sprintf("SELECT %d", postgresPendingLimit) || conn.tracker.pending != 0 {
		t.Fatalf("responses = %s, pending = %d", pgJSON(t, responses), conn.tracker.pending)
	}
}

func TestPostgresStatementLimitClearsAll(t *testing.T) {
	conn := newPGTestConn(true, false)
	conn.login(t)
	var parses []byte
	for i := range postgresStatementLimit + 1 {
		parses = append(parses, pgParse(fmt.Sprintf("s%d", i), fmt.Sprintf("SELECT %d", i))...)
	}
	conn.req(parses, pgSync())
	conn.resp(pgReady())
	if conn.tracker.statements != 1 || len(conn.tracker.conns[1].statements) != 1 {
		t.Fatalf("statements = %d / %d", conn.tracker.statements, len(conn.tracker.conns[1].statements))
	}
	events := conn.req(pgBind("", "s0"), pgExecute(""), pgSync())
	events = append(events, conn.req(pgBind("", fmt.Sprintf("s%d", postgresStatementLimit)), pgExecute(""), pgSync())...)
	if events[0].Postgres.SQL != "" || events[0].Postgres.Statement != "s0" || events[1].Postgres.SQL != fmt.Sprintf("SELECT %d", postgresStatementLimit) {
		t.Fatalf("after clear = %s", pgJSON(t, events))
	}
	conn.req(pgMsg('C', []byte{'S'}, pgCString(fmt.Sprintf("s%d", postgresStatementLimit))), pgSync())
	if conn.tracker.statements != 0 {
		t.Fatalf("close statement left %d", conn.tracker.statements)
	}
}

func TestPostgresExtendedPortalsAndSilentBatches(t *testing.T) {
	conn := newPGTestConn(false, false)
	conn.login(t)
	events := conn.req(pgParse("", "SELECT $1::text"), pgBind("", "", "edc-param-secret"), pgExecute(""), pgSync())
	// 이름 없는 portal은 Sync에서 닫힌다. Bind 없는 Execute에는 SQL이 없다.
	events = append(events, conn.req(pgExecute(""), pgSync())...)
	// Parse도 Execute도 없는 batch는 event 없이 ReadyForQuery 하나를 소비한다.
	events = append(events, conn.req(pgMsg('C', []byte{'P'}, pgCString("p1")), pgSync())...)
	events = append(events, conn.req(pgQuery(""))...)
	events = append(events, conn.resp(pgMsg('1'), pgMsg('2'), pgRow("edc-param-secret"), pgComplete("SELECT 1"), pgReady())...)
	events = append(events, conn.resp(pgError("ERROR", "34000", `portal "" does not exist`, ""), pgReady())...)
	events = append(events, conn.resp(pgMsg('3'), pgReady())...)
	events = append(events, conn.resp(pgMsg('I'), pgReady())...)
	if got := pgTokens(events); got != "E E Q rSELECT_1 e34000 r" || events[1].Postgres.SQL != "" || events[2].Postgres.SQL != "" {
		t.Fatalf("portals = %q %s", got, pgJSON(t, events))
	}
	if strings.Contains(pgJSON(t, events), "edc-param-secret") || clickHouseStateHas(reflect.ValueOf(conn.tracker), "edc-param-secret") {
		t.Fatal("bind value kept")
	}
}

// NoticeResponse, NotificationResponse, ParameterStatus는 단위를 닫지 않는다. standard_conforming_strings=off는 뒤 SQL의
// 가림 규칙을 바꾼다.
func TestPostgresAsyncMessagesDoNotPop(t *testing.T) {
	conn := newPGTestConn(true, false)
	conn.login(t)
	notice := pgMsg('N', []byte{'S'}, pgCString("NOTICE"), []byte{'M'}, pgCString("edc-notice-secret"), []byte{0})
	notify := pgMsg('A', pgInt32(1), pgCString("chan"), pgCString("edc-payload-secret"))
	events := conn.resp(notify)
	events = append(events, conn.req(pgQuery("SET standard_conforming_strings = off"))...)
	events = append(events, conn.resp(notice, pgParam("standard_conforming_strings", "off"), notify, pgComplete("SET"))...)
	events = append(events, conn.resp(notice, pgReady())...)
	events = append(events, conn.req(pgQuery(`SELECT 'a\'edc-literal-secret', 1`))...)
	events = append(events, conn.resp(pgComplete("SELECT 1"), notify, pgReady())...)
	if got := pgTokens(events); got != "Q rSET Q rSELECT_1" || events[2].Postgres.SQL != `SELECT '?', 1` {
		t.Fatalf("async = %q %s", got, pgJSON(t, events))
	}
	for _, secret := range []string{"edc-notice-secret", "edc-payload-secret", "edc-literal-secret"} {
		if strings.Contains(pgJSON(t, events), secret) || clickHouseStateHas(reflect.ValueOf(conn.tracker), secret) {
			t.Fatalf("%s kept", secret)
		}
	}
}

func TestPostgresErrorFields(t *testing.T) {
	for _, showSecrets := range []bool{false, true} {
		conn := newPGTestConn(true, showSecrets)
		conn.login(t)
		events := conn.req(pgQuery("INSERT INTO u VALUES ('x')"))
		events = append(events, conn.resp(pgError("ERROR", "23505", "dup\x1b[31m "+strings.Repeat("m", 2000), "Key (v)=(edc-detail-secret) exists."), pgReady())...)
		info := events[1].Postgres
		raw := pgJSON(t, events)
		if info.Severity != "ERROR" || info.SQLState != "23505" || len(info.Message) > postgresMessageLimit+len(`\x1b`)-1 || !strings.HasPrefix(info.Message, `dup\x1b[31m`) {
			t.Fatalf("error = %#v", info)
		}
		if strings.Contains(raw, "edc-hint-secret") || strings.Contains(raw, "edc-where-secret") || (info.Detail != "") != showSecrets {
			t.Fatalf("show=%v error fields = %s", showSecrets, raw)
		}
	}
}

func TestPostgresBadLengthsLoseFramingWithoutPanic(t *testing.T) {
	for _, data := range [][]byte{
		{'Q', 0, 0, 0, 3},
		{'Q', 0xff, 0xff, 0xff, 0xff},
		{'Q', 0x40, 0, 0, 0},
		{'?', 0, 0, 0, 4},
		{0, 0, 0, 4, 0, 3, 0, 0},
	} {
		conn := newPGTestConn(true, false)
		conn.login(t)
		events := conn.req(pgQuery("SELECT 1"))
		events = append(events, conn.req(data)...)
		events = append(events, conn.resp(pgComplete("SELECT 1"), pgReady())...)
		if got := pgTokens(events); got != "Q" || conn.tracker.pending != 0 {
			t.Fatalf("%x = %q, pending %d", data, got, conn.tracker.pending)
		}
		events = conn.req(pgQuery("SELECT 2"))
		events = append(events, conn.resp(pgComplete("SELECT 1"), pgReady())...)
		if got := pgTokens(events); got != "Q rSELECT_1" {
			t.Fatalf("%x recovery = %q", data, got)
		}
	}
}

func TestPostgresSideFilterForgetAndSocketLimit(t *testing.T) {
	tracker := newPostgresTracker(traceServerSide, false)
	if events := tracker.events(pgRecord(1, false, true, 1, pgStartup("user", "edc"), len(pgStartup("user", "edc"))), 0); len(events) != 0 {
		t.Fatalf("client record on a server tracker = %#v", events)
	}
	startup := pgStartup("user", "edc")
	events := tracker.events(pgRecord(1, true, false, 1, startup, len(startup)), 0)
	if pgTokens(events) != "C" || tracker.pending != 1 {
		t.Fatalf("startup = %q pending %d", pgTokens(events), tracker.pending)
	}
	tracker.forgetSocket(1)
	if len(tracker.conns) != 0 || tracker.pending != 0 {
		t.Fatalf("forget left %d conns, %d pending", len(tracker.conns), tracker.pending)
	}
	for socket := range uint64(postgresSocketLimit + 1) {
		tracker.events(pgRecord(socket+10, true, false, 1, startup, len(startup)), 0)
	}
	if len(tracker.conns) > postgresSocketLimit || tracker.pending > postgresPendingLimit {
		t.Fatalf("conns %d, pending %d", len(tracker.conns), tracker.pending)
	}
}
