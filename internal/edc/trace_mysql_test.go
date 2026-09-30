package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	mysqlTestServerCaps = mysqlCapProtocol41 | mysqlCapSecureConnection | mysqlCapConnectWithDB | mysqlCapSSL | mysqlCapCompress | mysqlCapLenencClientData | 0x80000 | mysqlCapQueryAttributes
	mysqlTestClientCaps = mysqlCapProtocol41 | mysqlCapSecureConnection | mysqlCapConnectWithDB | mysqlCapLenencClientData | 0x80000
)

func mysqlFrame(seq byte, payload []byte) []byte {
	length := len(payload)
	return append([]byte{byte(length), byte(length >> 8), byte(length >> 16), seq}, payload...)
}

// mysqlTestRecord는 한 쪽이 본 record다. 요청 방향은 client가 보내고 server가 받는 쪽이다.
func mysqlTestRecord(socket uint64, server, request bool, at uint64, data []byte) mysqlRecord {
	return mysqlRecord{bootTimeNS: at, pid: 7, process: "mysql", socket: socket, source: "127.0.0.1:40000", destination: "127.0.0.1:3306",
		sent: request != server, server: server, size: len(data), payload: data}
}

type mysqlTestConn struct {
	t       *testing.T
	tracker *mysqlTracker
	socket  uint64
	server  bool
}

func newMySQLTestConn(t *testing.T, server bool, side string, showSecrets bool) *mysqlTestConn {
	return &mysqlTestConn{t: t, tracker: newMySQLTracker(side, showSecrets), socket: 1, server: server}
}

func (conn *mysqlTestConn) send(request bool, at uint64, data []byte) []captureEvent {
	return conn.tracker.events(mysqlTestRecord(conn.socket, conn.server, request, at, data), 0)
}

func (conn *mysqlTestConn) request(at uint64, data []byte) []captureEvent {
	return conn.send(true, at, data)
}
func (conn *mysqlTestConn) response(at uint64, data []byte) []captureEvent {
	return conn.send(false, at, data)
}

func mysqlOnly(t *testing.T, events []captureEvent) captureEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	return events[0]
}

func mysqlQueryPayload(sql string) []byte { return append([]byte{mysqlComQuery}, sql...) }

func mysqlOKFrame(seq byte, rows byte) []byte {
	return mysqlFrame(seq, []byte{0, rows, 0, 2, 0, 0, 0})
}

func mysqlErrorFrame(seq byte, code uint16, state, message string) []byte {
	payload := binary.LittleEndian.AppendUint16([]byte{0xff}, code)
	return mysqlFrame(seq, append(append(payload, '#'), state+message...))
}

func mysqlGreetingFrame(version string, caps uint32) []byte {
	payload := append([]byte{10}, version...)
	payload = append(payload, 0, 1, 0, 0, 0)
	payload = append(payload, make([]byte, 8)...)
	payload = append(payload, 0)
	payload = binary.LittleEndian.AppendUint16(payload, uint16(caps))
	payload = append(payload, 0xff, 2, 0)
	payload = binary.LittleEndian.AppendUint16(payload, uint16(caps>>16))
	payload = append(payload, 21)
	payload = append(payload, make([]byte, 10)...)
	payload = append(payload, make([]byte, 13)...)
	payload = append(payload, "mysql_native_password\x00"...)
	return mysqlFrame(0, payload)
}

func mysqlHandshakeFrame(caps uint32, user, database string, auth []byte) []byte {
	payload := binary.LittleEndian.AppendUint32(nil, caps)
	payload = append(payload, make([]byte, 4)...)
	payload = append(payload, 0xff)
	payload = append(payload, make([]byte, 23)...)
	payload = append(append(payload, user...), 0)
	payload = append(append(payload, byte(len(auth))), auth...)
	if caps&mysqlCapConnectWithDB != 0 {
		payload = append(append(payload, database...), 0)
	}
	return mysqlFrame(1, append(payload, "caching_sha2_password\x00"...))
}

// mysqlHandshake는 greeting부터 인증 OK까지를 흘리고 그 동안 나온 event를 돌려준다.
func (conn *mysqlTestConn) handshake(clientCaps uint32) []captureEvent {
	events := conn.response(100, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	events = append(events, conn.request(200, mysqlHandshakeFrame(clientCaps, "root", "edc", []byte("hunter2-scramble-bytes")))...)
	more := append(mysqlFrame(2, []byte{1, 3}), mysqlOKFrame(3, 0)...)
	return append(events, conn.response(1_200, more)...)
}

func mysqlJSON(t *testing.T, events []captureEvent) string {
	t.Helper()
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestMySQLTrackerPairsAQueryOnBothSides(t *testing.T) {
	for _, server := range []bool{false, true} {
		conn := newMySQLTestConn(t, server, "", false)
		request := mysqlOnly(t, conn.request(1_000_000, mysqlFrame(0, mysqlQueryPayload("UPDATE t SET name = 'bob' WHERE id = 7"))))
		side := map[bool]string{false: traceClientSide, true: traceServerSide}[server]
		if request.Event != "mysql_query" || request.Protocol != "mysql" || request.Side != side || request.MySQL.SQL != "UPDATE t SET name = '?' WHERE id = 7" || request.MySQL.Command != "query" {
			t.Fatalf("request = %#v %#v", request, request.MySQL)
		}
		response := mysqlOnly(t, conn.response(4_000_000, mysqlOKFrame(1, 3)))
		if response.Event != "mysql_ok" || response.Side != side || response.answered != 1 || response.LatencyMS == nil || *response.LatencyMS != 3 || response.MySQL.Command != "query" || response.MySQL.SQL != request.MySQL.SQL || response.MySQL.AffectedRows == nil || *response.MySQL.AffectedRows != 3 {
			t.Fatalf("response = %#v %#v", response, response.MySQL)
		}
		// 다음 응답 packet은 이미 짝지은 명령과 다시 짝짓지 않는다.
		if events := conn.response(5_000_000, mysqlFrame(2, []byte{2})); len(events) != 0 {
			t.Fatalf("stray response = %#v", events)
		}
	}
}

func TestMySQLTrackerReadsAffectedRowsZeroAndResultsAndErrors(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	conn.request(0, mysqlFrame(0, mysqlQueryPayload("DELETE FROM t")))
	zero := mysqlOnly(t, conn.response(1_000_000, mysqlOKFrame(1, 0)))
	if zero.MySQL.AffectedRows == nil || *zero.MySQL.AffectedRows != 0 || !strings.Contains(mysqlJSON(t, []captureEvent{zero}), `"affected_rows":0`) {
		t.Fatalf("zero rows = %#v", zero.MySQL)
	}
	conn.request(2_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT id, name FROM t")))
	result := mysqlOnly(t, conn.response(3_000_000, append(mysqlFrame(1, []byte{2}), mysqlFrame(2, []byte("column definition"))...)))
	if result.Event != "mysql_result" || result.MySQL.Columns != 2 {
		t.Fatalf("result = %#v", result.MySQL)
	}
	conn.request(4_000_000, mysqlFrame(0, mysqlQueryPayload("INSERT INTO t VALUES (1, 'x')")))
	failure := mysqlOnly(t, conn.response(5_000_000, mysqlErrorFrame(1, 1062, "23000", "Duplicate entry '1' for key 't.PRIMARY'")))
	if failure.Event != "mysql_error" || failure.MySQL.ErrorCode != 1062 || failure.MySQL.SQLState != "23000" || failure.MySQL.Message != "Duplicate entry '1' for key 't.PRIMARY'" || failure.answered != 1 {
		t.Fatalf("error = %#v", failure.MySQL)
	}
	// handshake를 보지 못해도 SQLSTATE가 없는 ERR는 message만 읽는다.
	conn.request(6_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT 1")))
	old := mysqlOnly(t, conn.response(7_000_000, mysqlFrame(1, append([]byte{0xff, 0x28, 0x04}, "Too many"...))))
	if old.MySQL.ErrorCode != 1064 || old.MySQL.SQLState != "" || old.MySQL.Message != "Too many" {
		t.Fatalf("error without state = %#v", old.MySQL)
	}
	conn.request(8_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT 2")))
	eof := mysqlOnly(t, conn.response(9_000_000, mysqlFrame(1, []byte{0xfe, 0, 0, 2, 0})))
	if eof.Event != "mysql_ok" || eof.MySQL.AffectedRows != nil {
		t.Fatalf("eof = %#v", eof.MySQL)
	}
}

func TestMySQLTrackerJoinsAHeaderReadSeparatelyFromItsPayload(t *testing.T) {
	// mysqld와 libmysqlclient는 4 byte header를 먼저 읽고 payload를 따로 읽는다.
	conn := newMySQLTestConn(t, true, "", false)
	frame := mysqlFrame(0, mysqlQueryPayload("SELECT 'a'"))
	if events := conn.request(1_000_000, frame[:4]); len(events) != 0 {
		t.Fatalf("header read = %#v", events)
	}
	request := mysqlOnly(t, conn.request(1_000_100, frame[4:]))
	if request.MySQL.SQL != "SELECT '?'" || request.BootTimeNS != 1_000_000 {
		t.Fatalf("request = %#v at %d", request.MySQL, request.BootTimeNS)
	}
	answer := mysqlFrame(1, []byte{1})
	conn.response(3_000_000, answer[:4])
	response := mysqlOnly(t, conn.response(3_000_050, answer[4:]))
	if response.Event != "mysql_result" || response.LatencyMS == nil || *response.LatencyMS != 2 {
		t.Fatalf("response = %#v", response)
	}
}

func TestMySQLTrackerReadsAPayloadInSeveralReads(t *testing.T) {
	conn := newMySQLTestConn(t, true, "", false)
	conn.handshake(mysqlTestClientCaps)
	frame := mysqlFrame(0, mysqlQueryPayload("SELECT 1, 2, 3"))
	conn.request(0, frame[:4])
	if events := conn.request(1, frame[4:9]); len(events) != 0 {
		t.Fatalf("first part = %#v", events)
	}
	if request := mysqlOnly(t, conn.request(2, frame[9:])); request.MySQL.SQL != "SELECT 1, 2, 3" {
		t.Fatalf("request = %#v", request.MySQL)
	}
}

func TestMySQLTrackerReadsSeveralPacketsInOneRead(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	events := conn.handshake(mysqlTestClientCaps)
	// auth more data와 OK가 한 읽기에 왔다.
	if len(events) != 2 || events[0].Event != "mysql_connect" || events[1].Event != "mysql_ok" || events[1].MySQL.Command != "connect" || events[1].answered != 1 {
		t.Fatalf("handshake events = %#v", events)
	}
	// COM_STMT_CLOSE 뒤에 명령이 한 쓰기로 이어져도 둘 다 읽는다.
	closing := mysqlFrame(0, []byte{mysqlComStmtClose, 9, 0, 0, 0})
	events = conn.request(2_000, append(closing, mysqlFrame(0, mysqlQueryPayload("SELECT 1"))...))
	if len(events) != 1 || events[0].Event != "mysql_query" {
		t.Fatalf("close and query = %#v", events)
	}
}

func TestMySQLTrackerReadsTheHandshakeWithoutKeepingAuthBytes(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	events := conn.handshake(mysqlTestClientCaps)
	connect, answer := events[0], events[1]
	if connect.MySQL.User != "root" || connect.MySQL.Database != "edc" || connect.MySQL.ServerVersion != "8.4.11" || connect.MySQL.Compressed || connect.Event != "mysql_connect" {
		t.Fatalf("connect = %#v", connect.MySQL)
	}
	if answer.LatencyMS == nil || *answer.LatencyMS != 0.001 || answer.MySQL.AffectedRows != nil {
		t.Fatalf("answer = %#v", answer)
	}
	// auth switch와 public key 교환을 지나 마지막 ERR가 mysql_connect의 응답이다.
	failing := newMySQLTestConn(t, true, "", false)
	failing.response(100, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	failing.request(200, mysqlHandshakeFrame(mysqlTestClientCaps, "app", "", []byte("another-password-bytes")))
	failing.response(300, mysqlFrame(2, append([]byte{0xfe}, "caching_sha2_password\x00abcdefgh"...)))
	failing.request(400, mysqlFrame(3, []byte("client auth data")))
	failing.response(500, mysqlFrame(4, append([]byte{1}, "-----BEGIN PUBLIC KEY-----"...)))
	denied := mysqlOnly(t, failing.response(600, mysqlErrorFrame(6, 1045, "28000", "Access denied for user 'app'@'host'")))
	if denied.Event != "mysql_error" || denied.MySQL.Command != "connect" || denied.MySQL.ErrorCode != 1045 || denied.MySQL.SQLState != "28000" {
		t.Fatalf("denied = %#v", denied.MySQL)
	}
	encoded := mysqlJSON(t, append(events, denied))
	for _, secret := range []string{"hunter2", "scramble", "another-password", "client auth data", "caching_sha2", "BEGIN PUBLIC"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("event JSON contains %q: %s", secret, encoded)
		}
	}
	if events := failing.request(700, mysqlFrame(0, mysqlQueryPayload("SELECT 1"))); len(events) != 0 {
		t.Fatalf("a command after the failure = %#v", events)
	}
}

func TestMySQLTrackerStopsAtASSLRequest(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	conn.response(100, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	sslRequest := binary.LittleEndian.AppendUint32(nil, mysqlTestClientCaps|mysqlCapSSL)
	sslRequest = append(sslRequest, make([]byte, 28)...)
	tls := mysqlOnly(t, conn.request(200, mysqlFrame(1, sslRequest)))
	if tls.Event != "mysql_tls" || !tls.MySQL.TLS || tls.MySQL.ServerVersion != "8.4.11" {
		t.Fatalf("tls = %#v", tls.MySQL)
	}
	// TLS 안의 byte는 뒤에 명령처럼 보여도 해석하지 않는다.
	if events := conn.request(300, append([]byte{0x17, 3, 3, 0, 40}, bytes.Repeat([]byte{7}, 40)...)); len(events) != 0 {
		t.Fatalf("tls bytes = %#v", events)
	}
	if events := conn.request(400, mysqlFrame(0, mysqlQueryPayload("SELECT 1"))); len(events) != 0 {
		t.Fatalf("query after tls = %#v", events)
	}
	if events := conn.response(500, mysqlOKFrame(1, 0)); len(events) != 0 {
		t.Fatalf("response after tls = %#v", events)
	}
}

func TestMySQLTrackerStopsAfterACompressedHandshake(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	events := conn.handshake(mysqlTestClientCaps | mysqlCapCompress)
	if len(events) != 2 || !events[0].MySQL.Compressed || events[1].Event != "mysql_ok" {
		t.Fatalf("handshake = %#v", events)
	}
	if events := conn.request(2_000, append([]byte{5, 0, 0, 0, 0, 0, 0, 0, 0}, 0x03, 'S')); len(events) != 0 {
		t.Fatalf("compressed bytes = %#v", events)
	}
	if events := conn.request(3_000, mysqlFrame(0, mysqlQueryPayload("SELECT 1"))); len(events) != 0 {
		t.Fatalf("query on a compressed connection = %#v", events)
	}
}

func TestMySQLTrackerStartsAgainOnANewGreeting(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	conn.response(100, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	conn.request(200, mysqlFrame(1, append(binary.LittleEndian.AppendUint32(nil, mysqlTestClientCaps|mysqlCapSSL), make([]byte, 28)...)))
	// 같은 socket 주소를 다시 쓴 연결이다.
	if events := conn.handshake(mysqlTestClientCaps); len(events) != 2 {
		t.Fatalf("second connection = %#v", events)
	}
	// greeting을 header와 payload로 나눠 읽어도 새 연결이다.
	greeting := mysqlGreetingFrame("8.4.11", mysqlTestServerCaps)
	conn.response(10_000, greeting[:4])
	conn.response(10_001, greeting[4:])
	if events := conn.request(10_100, mysqlHandshakeFrame(mysqlTestClientCaps, "root", "", []byte("x"))); len(events) != 1 || events[0].Event != "mysql_connect" {
		t.Fatalf("connect after a split greeting = %#v", events)
	}
}

func TestMySQLTrackerRestoresThePreparedSQLForExecute(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	prepare := mysqlOnly(t, conn.request(1_000_000, mysqlFrame(0, append([]byte{mysqlComStmtPrepare}, "SELECT id FROM t WHERE name = ?"...))))
	if prepare.Event != "mysql_prepare" || prepare.MySQL.SQL != "SELECT id FROM t WHERE name = ?" {
		t.Fatalf("prepare = %#v", prepare.MySQL)
	}
	prepared := mysqlOnly(t, conn.response(2_000_000, mysqlFrame(1, []byte{0, 5, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0})))
	if prepared.Event != "mysql_ok" || prepared.MySQL.StatementID != 5 || prepared.MySQL.AffectedRows != nil || prepared.MySQL.Command != "prepare" {
		t.Fatalf("prepared = %#v", prepared.MySQL)
	}
	execute := mysqlOnly(t, conn.request(3_000_000, mysqlFrame(0, []byte{mysqlComStmtExecute, 5, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 254, 0, 3, 'b', 'o', 'b'})))
	if execute.Event != "mysql_execute" || execute.MySQL.StatementID != 5 || execute.MySQL.SQL != "SELECT id FROM t WHERE name = ?" || strings.Contains(mysqlJSON(t, []captureEvent{execute}), "bob") {
		t.Fatalf("execute = %#v", execute.MySQL)
	}
	failed := mysqlOnly(t, conn.response(4_000_000, mysqlErrorFrame(1, 1062, "23000", "Duplicate entry")))
	if failed.MySQL.Command != "execute" || failed.MySQL.StatementID != 5 || failed.MySQL.SQL != execute.MySQL.SQL {
		t.Fatalf("failed execute = %#v", failed.MySQL)
	}
	// COM_STMT_CLOSE는 응답이 없고 statement를 지운다. 다음 응답은 다음 명령과 짝이 된다.
	if events := conn.request(5_000_000, mysqlFrame(0, []byte{mysqlComStmtClose, 5, 0, 0, 0})); len(events) != 0 {
		t.Fatalf("close = %#v", events)
	}
	if conn.tracker.statements != 0 {
		t.Fatalf("statements = %d", conn.tracker.statements)
	}
	after := mysqlOnly(t, conn.request(6_000_000, mysqlFrame(0, []byte{mysqlComStmtExecute, 5, 0, 0, 0, 0, 1, 0, 0, 0})))
	if after.MySQL.StatementID != 5 || after.MySQL.SQL != "" {
		t.Fatalf("execute after close = %#v", after.MySQL)
	}
}

func TestMySQLTrackerReadsQueryAttributesInBothShapes(t *testing.T) {
	// handshake를 못 본 연결이라 payload의 00 01로 알아낸다.
	conn := newMySQLTestConn(t, false, "", false)
	unknown := mysqlOnly(t, conn.request(0, mysqlFrame(0, []byte{mysqlComQuery, 0, 1, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'})))
	if unknown.MySQL.SQL != "SELECT 1" {
		t.Fatalf("attributes without a handshake = %#v", unknown.MySQL)
	}
	// handshake에서 협상했으면 parameter_count를 읽어 건너뛴다.
	negotiated := newMySQLTestConn(t, false, "", false)
	negotiated.handshake(mysqlTestClientCaps | mysqlCapQueryAttributes)
	event := mysqlOnly(t, negotiated.request(5_000, mysqlFrame(0, []byte{mysqlComQuery, 0, 1, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '2'})))
	if event.MySQL.SQL != "SELECT 2" {
		t.Fatalf("negotiated attributes = %#v", event.MySQL)
	}
	// parameter_count가 0보다 크면 SQL을 해석하지 않는다. 명령만 보인다.
	withValues := mysqlOnly(t, negotiated.request(6_000, mysqlFrame(0, []byte{mysqlComQuery, 1, 1, 0, 8, 1, 'a', 0, 1, 'v', 'S', 'E', 'L'})))
	if withValues.Event != "mysql_query" || withValues.MySQL.SQL != "" {
		t.Fatalf("parameters = %#v", withValues.MySQL)
	}
	// 협상하지 않은 연결의 SQL은 00 01로 시작해도 그대로 둔다.
	plain := newMySQLTestConn(t, false, "", false)
	plain.handshake(mysqlTestClientCaps)
	if event := mysqlOnly(t, plain.request(5_000, mysqlFrame(0, mysqlQueryPayload("SELECT 3")))); event.MySQL.SQL != "SELECT 3" {
		t.Fatalf("plain = %#v", event.MySQL)
	}
}

func TestMySQLTrackerFindsPacketBoundariesAfterAMidConnectionStart(t *testing.T) {
	conn := newMySQLTestConn(t, true, "", false)
	// 연결 중간에서 시작했으면 packet 중간이나 TLS 안의 byte가 온다.
	for _, garbage := range [][]byte{
		bytes.Repeat([]byte{0x41}, 80),
		{0x17, 3, 3, 0, 32, 1, 2, 3, 4, 5},
		{9, 0, 0, 5, 3, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'},
		{3, 0, 0, 0, 0xee, 1, 2},
	} {
		if events := conn.request(1, garbage); len(events) != 0 {
			t.Fatalf("%q made events %#v", garbage, events)
		}
	}
	// 응답도 명령을 보기 전에는 짝짓지 않는다.
	if events := conn.response(2, mysqlOKFrame(1, 0)); len(events) != 0 {
		t.Fatalf("response before a command = %#v", events)
	}
	request := mysqlOnly(t, conn.request(1_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT 42"))))
	response := mysqlOnly(t, conn.response(2_000_000, mysqlOKFrame(1, 0)))
	if request.MySQL.SQL != "SELECT 42" || response.MySQL.SQL != "SELECT 42" {
		t.Fatalf("pair = %#v %#v", request.MySQL, response.MySQL)
	}
	if events := conn.request(3_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT 43"))); len(events) != 1 {
		t.Fatalf("synced command = %#v", events)
	}
}

func TestMySQLTrackerFindsABoundaryFromASplitHeaderRead(t *testing.T) {
	conn := newMySQLTestConn(t, true, "", false)
	frame := mysqlFrame(0, mysqlQueryPayload("SELECT 7"))
	conn.request(1, bytes.Repeat([]byte{0x55}, 30))
	if events := conn.request(2, frame[:4]); len(events) != 0 {
		t.Fatalf("header = %#v", events)
	}
	if event := mysqlOnly(t, conn.request(3, frame[4:])); event.MySQL.SQL != "SELECT 7" {
		t.Fatalf("query = %#v", event.MySQL)
	}
}

func TestMySQLTrackerFindsABoundaryAtTheStartOfALargeQuery(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	sql := "INSERT INTO t VALUES " + strings.Repeat("(1, 'abcdefghij'),", 2000)
	frame := mysqlFrame(0, mysqlQueryPayload(sql))
	record := mysqlTestRecord(1, false, true, 1_000_000, frame[:mysqlPacketKeep])
	record.size = len(frame)
	events := conn.tracker.events(record, 0)
	event := mysqlOnly(t, events)
	if !event.MySQL.SQLTruncated || len(event.MySQL.SQL) != mysqlSQLLimit || !strings.HasPrefix(event.MySQL.SQL, "INSERT INTO t VALUES (1, '?'),") {
		t.Fatalf("large query = %d bytes, truncated %t", len(event.MySQL.SQL), event.MySQL.SQLTruncated)
	}
	if response := mysqlOnly(t, conn.response(2_000_000, mysqlOKFrame(1, 200))); response.MySQL.SQL != event.MySQL.SQL {
		t.Fatalf("response = %#v", response.MySQL)
	}
}

func TestMySQLTrackerSkipsThePartOfAPacketItDidNotCapture(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	handshake := conn.handshake(mysqlTestClientCaps)
	if len(handshake) != 2 {
		t.Fatalf("handshake = %#v", handshake)
	}
	sql := "SELECT " + strings.Repeat("a", 20000)
	frame := mysqlFrame(0, mysqlQueryPayload(sql))
	first := mysqlTestRecord(1, false, true, 10_000, frame[:mysqlPacketKeep])
	first.size = 16384
	request := mysqlOnly(t, conn.tracker.events(first, 0))
	if !request.MySQL.SQLTruncated {
		t.Fatalf("request = %#v", request.MySQL)
	}
	// 나머지 byte는 size만큼 건너뛴다. 뒤 record의 내용은 보지 않는다.
	rest := mysqlTestRecord(1, false, true, 10_100, bytes.Repeat([]byte{0, 0, 0, 0, 3}, 700))
	rest.size = len(frame) - 16384
	if events := conn.tracker.events(rest, 0); len(events) != 0 {
		t.Fatalf("rest = %#v", events)
	}
	if response := mysqlOnly(t, conn.response(20_000, mysqlOKFrame(1, 0))); response.answered != 1 {
		t.Fatalf("response = %#v", response)
	}
	if event := mysqlOnly(t, conn.request(30_000, mysqlFrame(0, mysqlQueryPayload("SELECT 2")))); event.MySQL.SQL != "SELECT 2" {
		t.Fatalf("next command = %#v", event.MySQL)
	}
}

func TestMySQLTrackerCountsSequenceIdsOfA16MiBCommand(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	head := append([]byte{0xff, 0xff, 0xff, 0, mysqlComQuery}, "INSERT INTO t VALUES ('x')"...)
	first := mysqlTestRecord(1, false, true, 1_000, head)
	first.size = 4 + mysqlMaxPacket
	if event := mysqlOnly(t, conn.tracker.events(first, 0)); event.Event != "mysql_query" {
		t.Fatalf("first = %#v", event)
	}
	// 이어지는 packet은 새 명령이 아니다. 응답 seq는 마지막 packet보다 1 크다.
	if events := conn.request(2_000, mysqlFrame(1, []byte("tail"))); len(events) != 0 {
		t.Fatalf("continuation = %#v", events)
	}
	if events := conn.response(3_000, mysqlOKFrame(1, 1)); len(events) != 0 {
		t.Fatalf("response with the first seq = %#v", events)
	}
	conn.request(4_000, mysqlFrame(0, mysqlQueryPayload("SELECT 1")))
	conn.response(5_000, mysqlOKFrame(1, 0))
	long := newMySQLTestConn(t, false, "", false)
	long.tracker.events(first, 0)
	long.request(2_000, mysqlFrame(1, []byte("tail")))
	if response := mysqlOnly(t, long.response(3_000, mysqlOKFrame(2, 1))); response.answered != 1 {
		t.Fatalf("response = %#v", response)
	}
}

func TestMySQLTrackerDoesNotPairAResponseThatDoesNotFit(t *testing.T) {
	for name, response := range map[string][]byte{
		"wrong seq":        mysqlOKFrame(3, 0),
		"seq zero":         mysqlOKFrame(0, 0),
		"short OK":         mysqlFrame(1, []byte{0, 1}),
		"bad lenenc":       mysqlFrame(1, []byte{0, 0xff, 0, 0, 0, 0, 0}),
		"lenenc past end":  mysqlFrame(1, []byte{0, 0xfe, 0, 0, 0, 0, 0}),
		"zero columns":     mysqlFrame(1, []byte{0}),
		"huge columns":     mysqlFrame(1, []byte{0xfc, 0x01, 0x20}),
		"columns and data": mysqlFrame(1, []byte{2, 3, 4}),
		"short ERR":        mysqlFrame(1, []byte{0xff, 1}),
		"local infile":     mysqlFrame(1, []byte{0xfb, 'f'}),
		"long EOF":         mysqlFrame(1, []byte{0xfe, 1, 2, 3, 4, 5, 6, 7, 8}),
	} {
		conn := newMySQLTestConn(t, false, "", false)
		conn.request(0, mysqlFrame(0, mysqlQueryPayload("SELECT 1")))
		if events := conn.response(1, response); len(events) != 0 {
			t.Fatalf("%s made events %#v", name, events)
		}
		// 버린 짝의 뒤에 오는 응답은 다음 명령을 기다린다.
		if events := conn.response(2, mysqlOKFrame(1, 0)); len(events) != 0 {
			t.Fatalf("%s: a second response paired: %#v", name, events)
		}
	}
}

func TestMySQLTrackerConsumesTheResponseOfCommandsWithoutEvents(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	for _, command := range [][]byte{{0x0e}, append([]byte{0x02}, "edc"...), {0x11, 'r', 0}} {
		if events := conn.request(0, mysqlFrame(0, command)); len(events) != 0 {
			t.Fatalf("command %x = %#v", command, events)
		}
		if events := conn.response(1, mysqlOKFrame(1, 0)); len(events) != 0 {
			t.Fatalf("response to %x = %#v", command, events)
		}
	}
	// QUIT은 응답이 없다. 앞에 짝을 못 찾은 명령이 있어도 버리고, 그 뒤 응답은 다음 명령과 짝이 된다.
	conn.request(2, mysqlFrame(0, mysqlQueryPayload("SELECT 'left'")))
	if events := conn.request(3, mysqlFrame(0, []byte{mysqlComQuit})); len(events) != 0 {
		t.Fatalf("quit = %#v", events)
	}
	if events := conn.response(4, mysqlOKFrame(1, 0)); len(events) != 0 {
		t.Fatalf("response after quit = %#v", events)
	}
	conn.request(5, mysqlFrame(0, mysqlQueryPayload("SELECT 5")))
	if response := mysqlOnly(t, conn.response(6, mysqlOKFrame(1, 0))); response.MySQL.SQL != "SELECT 5" {
		t.Fatalf("response = %#v", response.MySQL)
	}
	// 새 명령이 오면 짝을 찾지 못한 앞 명령은 응답 없음으로 남는다.
	conn.request(7, mysqlFrame(0, mysqlQueryPayload("SELECT 6")))
	conn.request(8, mysqlFrame(0, mysqlQueryPayload("SELECT 7")))
	if response := mysqlOnly(t, conn.response(9, mysqlOKFrame(1, 0))); response.MySQL.SQL != "SELECT 7" {
		t.Fatalf("response = %#v", response.MySQL)
	}
}

func TestMySQLTrackerShowsTheSideThatWasAskedFor(t *testing.T) {
	for _, side := range []string{traceClientSide, traceServerSide} {
		for _, server := range []bool{false, true} {
			conn := newMySQLTestConn(t, server, side, false)
			events := conn.request(0, mysqlFrame(0, mysqlQueryPayload("SELECT 1")))
			if want := (side == traceServerSide) == server; (len(events) == 1) != want {
				t.Fatalf("side %s server %t = %#v", side, server, events)
			}
		}
	}
}

func TestMySQLMaskHidesStringLiterals(t *testing.T) {
	for sql, want := range map[string]string{
		`SELECT * FROM t WHERE a = 'secret' AND b = "also"`:     `SELECT * FROM t WHERE a = '?' AND b = "?"`,
		`SELECT 'it''s', 'a\'b', "say ""hi"""`:                  `SELECT '?', '?', "?"`,
		`SELECT 'open literal AND more`:                         `SELECT '?`,
		`SELECT 1 -- it's 'a comment'` + "\nFROM t WHERE x='y'": "SELECT 1 -- it's 'a comment'\nFROM t WHERE x='?'",
		`SELECT 1 # 'x'` + "\n, 'z'":                            "SELECT 1 # 'x'\n, '?'",
		`SELECT /* don't */ 'v' /* "q" */`:                      `SELECT /* don't */ '?' /* "q" */`,
		"SELECT `it's` FROM `a``b` WHERE c = 'd'":               "SELECT `it's` FROM `a``b` WHERE c = '?'",
		`SELECT /*! STRAIGHT_JOIN 'x' */ 1`:                     `SELECT /*! STRAIGHT_JOIN '?' */ 1`,
		`SELECT /*+ SET_VAR(a='b') */ 1`:                        `SELECT /*+ SET_VAR(a='?') */ 1`,
		`CREATE USER 'a'@'%' IDENTIFIED BY 'pw'`:                `CREATE USER '?'@'?' IDENTIFIED BY '?'`,
		`SELECT 1 --'x'`:                                        `SELECT 1 --'?'`,
		`SELECT /* unclosed 'x'`:                                `SELECT /* unclosed 'x'`,
	} {
		if got := string(mysqlMaskSQL([]byte(sql))); got != want {
			t.Fatalf("mask(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestMySQLTrackerMasksByDefaultAndShowsSecretsOnRequest(t *testing.T) {
	sql := "CREATE USER 'edc_app'@'%' IDENTIFIED BY 'edc-secret-pw-7'"
	masked := mysqlOnly(t, newMySQLTestConn(t, false, "", false).request(0, mysqlFrame(0, mysqlQueryPayload(sql))))
	if strings.Contains(mysqlJSON(t, []captureEvent{masked}), "edc-secret") || masked.MySQL.SQL != "CREATE USER '?'@'?' IDENTIFIED BY '?'" {
		t.Fatalf("masked = %#v", masked.MySQL)
	}
	shown := mysqlOnly(t, newMySQLTestConn(t, false, "", true).request(0, mysqlFrame(0, mysqlQueryPayload(sql))))
	if shown.MySQL.SQL != sql {
		t.Fatalf("shown = %#v", shown.MySQL)
	}
	// prepare한 SQL도 execute에 그대로 붙으므로 같은 규칙이다.
	conn := newMySQLTestConn(t, false, "", false)
	conn.request(0, mysqlFrame(0, append([]byte{mysqlComStmtPrepare}, "SELECT 'secret-in-prepare'"...)))
	conn.response(1, mysqlFrame(1, []byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}))
	execute := mysqlOnly(t, conn.request(2, mysqlFrame(0, []byte{mysqlComStmtExecute, 1, 0, 0, 0, 0, 1, 0, 0, 0})))
	if strings.Contains(mysqlJSON(t, []captureEvent{execute}), "secret-in-prepare") {
		t.Fatalf("execute = %#v", execute.MySQL)
	}
}

func TestMySQLTrackerEscapesControlCharacters(t *testing.T) {
	conn := newMySQLTestConn(t, false, "", false)
	request := mysqlOnly(t, conn.request(0, mysqlFrame(0, mysqlQueryPayload("SELECT 1 -- \x1b[31mred"))))
	if strings.Contains(request.MySQL.SQL, "\x1b") || !strings.Contains(request.MySQL.SQL, `\x1b[31mred`) {
		t.Fatalf("sql = %q", request.MySQL.SQL)
	}
	failed := mysqlOnly(t, conn.response(1, mysqlErrorFrame(1, 1064, "42000", "bad \x1b[0m")))
	if strings.Contains(failed.MySQL.Message, "\x1b") {
		t.Fatalf("message = %q", failed.MySQL.Message)
	}
	greeting := newMySQLTestConn(t, false, "", false)
	greeting.response(0, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	connect := mysqlOnly(t, greeting.request(1, mysqlHandshakeFrame(mysqlTestClientCaps, "ro\x1bot", "d\x07b", []byte("x"))))
	if strings.ContainsAny(connect.MySQL.User+connect.MySQL.Database, "\x1b\x07") {
		t.Fatalf("connect = %#v", connect.MySQL)
	}
}

func TestMySQLShapeIgnoresValuesAndWhitespace(t *testing.T) {
	raw := "SELECT  t1.id,\n  col2 FROM t1 WHERE name = 'alice' AND age > 30 AND ratio < 1.5e-3 AND `a1` = \"x\" -- 1 'y'\n"
	masked := string(mysqlMaskSQL([]byte(raw)))
	want := "SELECT t1.id, col2 FROM t1 WHERE name = ? AND age > ? AND ratio < ? AND `a1` = ? -- 1 'y'"
	if got := traceMySQLShape(raw); got != want || traceMySQLShape(masked) != want {
		t.Fatalf("shape(raw) = %q\nshape(masked) = %q\nwant %q", got, traceMySQLShape(masked), want)
	}
	for _, sql := range []string{"SELECT 'it''s', 'a\\'b'", "INSERT INTO t VALUES ('open", `SELECT "a" , 'b'  , 12`, "SELECT 0x1F, 1st, t.5"} {
		if traceMySQLShape(sql) != traceMySQLShape(string(mysqlMaskSQL([]byte(sql)))) {
			t.Fatalf("shape differs after masking %q: %q and %q", sql, traceMySQLShape(sql), traceMySQLShape(string(mysqlMaskSQL([]byte(sql)))))
		}
	}
	if long := traceMySQLShape("SELECT " + strings.Repeat("a, ", 500)); len(long) > mysqlShapeLimit+3 || !strings.HasSuffix(long, "...") {
		t.Fatalf("long shape has %d bytes", len(long))
	}
}

func TestMySQLTrackerStopsAtItsLimits(t *testing.T) {
	// 응답을 기다리지 않는 stream이 상한을 넘으면 모두 비운다.
	waiting := newMySQLTestConn(t, true, "", false)
	for socket := uint64(1); socket <= mysqlWaitingLimit+1; socket++ {
		waiting.tracker.events(mysqlTestRecord(socket, true, true, 0, []byte{5, 0, 0, 0}), 0)
	}
	if waiting.tracker.waiting != 0 {
		t.Fatalf("waiting = %d", waiting.tracker.waiting)
	}
	// prepare한 statement가 상한을 넘으면 모두 비운다. 이후 execute는 id만 보인다.
	statements := newMySQLTestConn(t, false, "", false)
	for id := uint32(1); id <= mysqlStatementLimit+1; id++ {
		statements.request(0, mysqlFrame(0, append([]byte{mysqlComStmtPrepare}, "SELECT ?"...)))
		statements.response(1, mysqlFrame(1, append(binary.LittleEndian.AppendUint32([]byte{0}, id), make([]byte, 7)...)))
	}
	if got := statements.tracker.statements; got != 1 {
		t.Fatalf("statements = %d", got)
	}
	if event := mysqlOnly(t, statements.request(2, mysqlFrame(0, []byte{mysqlComStmtExecute, 1, 0, 0, 0, 0, 1, 0, 0, 0}))); event.MySQL.SQL != "" {
		t.Fatalf("execute after the clear = %#v", event.MySQL)
	}
	// 추적하는 socket이 상한을 넘으면 모두 비운다.
	sockets := newMySQLTracker("", false)
	for socket := uint64(1); socket <= mysqlSocketLimit+1; socket++ {
		sockets.events(mysqlTestRecord(socket, false, true, 0, []byte{1}), 0)
	}
	if len(sockets.conns) != 1 {
		t.Fatalf("conns = %d", len(sockets.conns))
	}
}

// mysqlBothSidesEvents는 같은 host의 client와 server가 본 한 번의 흐름이다. client는 응답 없는 query 하나를 더 보낸다.
func mysqlBothSidesEvents(t *testing.T) []captureEvent {
	t.Helper()
	client := newMySQLTestConn(t, false, "", false)
	server := &mysqlTestConn{t: t, tracker: client.tracker, socket: 2, server: true}
	var events []captureEvent
	events = append(events, client.request(1_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT * FROM t WHERE id = 7 AND name = 'a'")))...)
	events = append(events, server.request(1_100_000, mysqlFrame(0, mysqlQueryPayload("SELECT * FROM t WHERE id = 7 AND name = 'a'")))...)
	events = append(events, server.response(2_600_000, mysqlFrame(1, []byte{2}))...)
	events = append(events, client.response(3_000_000, mysqlFrame(1, []byte{2}))...)
	events = append(events, client.request(4_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT * FROM t WHERE id = 9 AND name = 'b'")))...)
	events = append(events, client.response(6_000_000, mysqlErrorFrame(1, 1064, "42000", "syntax"))...)
	events = append(events, client.request(7_000_000, mysqlFrame(0, mysqlQueryPayload("DELETE FROM t")))...)
	events = append(events, client.request(8_000_000, mysqlFrame(0, []byte{mysqlComStmtExecute, 3, 0, 0, 0, 0, 1, 0, 0, 0}))...)
	return events
}

func TestMySQLTraceLabelAndPortOption(t *testing.T) {
	for _, test := range []struct{ side, want string }{{"", "mysql"}, {traceClientSide, "mysql --side client"}, {traceServerSide, "mysql --side server"}} {
		if got := traceLabel("mysql", test.side); got != test.want {
			t.Fatalf("traceLabel(mysql, %q) = %q, want %q", test.side, got, test.want)
		}
	}
	if scope := (tcpTraceOptions{port: 3307, showSecrets: true}).scope("mysql"); scope.port != 3307 || !scope.showSecrets || scope.protocol != "mysql" {
		t.Fatalf("scope = %+v", scope)
	}
	for _, args := range [][]string{{"mysql", "--port", "65536"}, {"mysql", "--payload"}, {"mysql", "--side", "both"}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
	if !traceProtocols["mysql"].serverSide || !traceProtocols["mysql"].linuxOnly {
		t.Fatal("trace mysql must be Linux only and allow --side server")
	}
}

func TestMySQLScrollLabelsShowSideCommandAndResult(t *testing.T) {
	events := mysqlBothSidesEvents(t)
	for index, want := range [][2]string{
		{"client: SELECT * FROM t WHERE id = 7 AND name = '?'", "mysql_query"},
		{"server: SELECT * FROM t WHERE id = 7 AND name = '?'", "mysql_query"},
		{"server: SELECT * FROM t WHERE id = 7 AND name = '?' · 2 columns", "mysql_result 1.5ms"},
		{"client: SELECT * FROM t WHERE id = 7 AND name = '?' · 2 columns", "mysql_result 2.0ms"},
		{"client: SELECT * FROM t WHERE id = 9 AND name = '?'", "mysql_query"},
		{"client: SELECT * FROM t WHERE id = 9 AND name = '?' · 1064 (42000) syntax", "mysql_error 2.0ms"},
		{"client: DELETE FROM t", "mysql_query"},
		{"client: execute #3", "mysql_execute"},
	} {
		if destination, label := traceScrollLabels(events[index]); destination != want[0] || label != want[1] {
			t.Fatalf("labels[%d] = %q, %q; want %q, %q", index, destination, label, want[0], want[1])
		}
	}
	long := captureEvent{Protocol: "mysql", Event: "mysql_query", Side: traceClientSide, MySQL: &traceMySQLEvent{Command: "query", SQL: "SELECT\n\t" + strings.Repeat("x", 300)}}
	if destination, _ := traceScrollLabels(long); strings.ContainsAny(destination, "\n\t") || len(destination) > len("client: SELECT ")+mysqlScrollLabelSQL+len("...") {
		t.Fatalf("long label = %q", destination)
	}
	connect := captureEvent{Protocol: "mysql", Event: "mysql_connect", Side: traceServerSide, MySQL: &traceMySQLEvent{Command: "connect", User: "root", Database: "edc", ServerVersion: "8.4.11"}}
	tls := captureEvent{Protocol: "mysql", Event: "mysql_tls", Side: traceClientSide, MySQL: &traceMySQLEvent{TLS: true, ServerVersion: "8.4.11"}}
	if destination, _ := traceScrollLabels(connect); destination != "server: root edc 8.4.11" {
		t.Fatalf("connect label = %q", destination)
	}
	if destination, _ := traceScrollLabels(tls); destination != "client: tls" {
		t.Fatalf("tls label = %q", destination)
	}
}

func TestMySQLGroupViewsKeepSidesApartAndCountUnansweredCommands(t *testing.T) {
	events := mysqlBothSidesEvents(t)
	for _, group := range summarizeTraceGroups("mysql", traceGroupByEvent, events, captureSummary{}, time.Second, "", "").Groups {
		if group.Group != "mysql_query" {
			continue
		}
		// client 쪽은 세 번 물었고 둘이 응답을 받았다. server 쪽은 하나에 답했다. 응답은 결과 event의 행에 센다.
		want := traceMySQLCounts{Commands: 3, Unanswered: 1}
		if group.Server {
			want = traceMySQLCounts{Commands: 1}
		}
		got := *group.MySQL
		if got.Commands != want.Commands || got.Responses != 0 || got.Unanswered != want.Unanswered {
			t.Fatalf("query row (server %t) = %+v", group.Server, got)
		}
	}
	report := summarizeTraceGroups("mysql", traceGroupByProcess, events, captureSummary{}, time.Second, "", "")
	if report.Side != "" || len(report.Groups) != 2 || report.Groups[0].Server == report.Groups[1].Server {
		t.Fatalf("process groups = %#v", report.Groups)
	}
	var titles []string
	for _, column := range traceProtocols["mysql"].groupColumns {
		titles = append(titles, column.screenTitle)
	}
	if strings.Join(titles, " ") != "CMD RSP ERR NOANS AVGms MAXms" {
		t.Fatalf("columns = %q", titles)
	}
	if only := summarizeTraceGroups("mysql", traceGroupByProcess, events[1:3], captureSummary{}, time.Second, "", ""); only.Side != traceServerSide {
		t.Fatalf("server-only side = %q", only.Side)
	}
}

func TestMySQLTraceSummaryCountsSidesAndShapes(t *testing.T) {
	events := mysqlBothSidesEvents(t)
	events = append(events, captureEvent{Protocol: "mysql", Event: "mysql_tls", Side: traceClientSide, MySQL: &traceMySQLEvent{TLS: true}},
		captureEvent{Protocol: "mysql", Event: "mysql_connect", Side: traceClientSide, MySQL: &traceMySQLEvent{Command: "connect", Compressed: true}})
	summarizer := newMySQLTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 2}, time.Second).(mysqlTraceReport)
	if report.Commands != 6 || report.Responses != 3 || report.Errors != 1 || report.Unanswered != 3 || report.LostEvents != 2 || report.Connections != 2 || report.TLSConnections != 1 || report.CompressedConnections != 1 {
		t.Fatalf("report = %+v", report.traceMySQLCounts)
	}
	if report.Client == nil || report.Server == nil || report.Client.Commands != 5 || report.Server.Commands != 1 || *report.Server.LatencyAvgMS != 1.5 {
		t.Fatalf("sides = %+v %+v", report.Client, report.Server)
	}
	var shapes []string
	for _, row := range report.Statements {
		shapes = append(shapes, row.Side+" "+row.Command+" "+row.Shape)
	}
	// 값이 다른 두 query는 한 행이다. prepare를 못 본 execute와 connect는 자기 행이 있다.
	want := []string{"client query SELECT * FROM t WHERE id = ? AND name = ?", "client connect (connect)", "client execute (statement not seen)", "client query DELETE FROM t", "server query SELECT * FROM t WHERE id = ? AND name = ?"}
	if strings.Join(shapes, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", shapes, want)
	}
	if first := report.Statements[0]; first.Commands != 2 || first.Errors != 1 || first.Unanswered != 0 || len(first.Processes) != 1 || first.Processes[0] != "mysql" {
		t.Fatalf("first row = %+v", first)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"client":{"commands":5`, `"server":{"commands":1`, `"statements":[{"side":"client","command":"query","shape":"SELECT * FROM t WHERE id = ? AND name = ?"`, `"lost_events":2`, `"tls_connections":1`, `"latency_avg_ms":`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(true) })
	for _, text := range []string{"Client side (commands that this host sent):\n  Commands: 5\n", "Server side (commands that local servers received):\n", "Lost events: 2\n", "\nSIDE\tCOMMAND\tSQL\tCOUNT\tERRORS\tUNANSWERED\tAVG\tMAX\tPROCESS\n", "\nserver\tquery\tSELECT * FROM t WHERE id = ? AND name = ?\t1\t0\t0\t"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	// 한 쪽만 있으면 SIDE 칸과 쪽별 합계가 없다.
	single := newMySQLTraceSummarizer()
	single.observe(events[1])
	single.observe(events[2])
	one := single.summarize(captureSummary{}, time.Second).(mysqlTraceReport)
	if one.Side != traceServerSide || one.Client != nil || one.Server != nil {
		t.Fatalf("server-only report = %#v", one)
	}
	if output := traceCaptureOutput(t, &os.Stdout, func() { one.print(false) }); !strings.HasPrefix(output, "MySQL server trace: ") || strings.Contains(output, "SIDE") || !strings.Contains(output, "\nCommands: 1\n") {
		t.Fatalf("server-only summary = %q", output)
	}
}
