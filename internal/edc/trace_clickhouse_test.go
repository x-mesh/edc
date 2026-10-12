package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const clickHouseTestPassword = "edc-fixture"

func clickHouseFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "clickhouse", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	var digits strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			digits.WriteString(line)
		}
	}
	data, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func chVar(value uint64) []byte { return binary.AppendUvarint(nil, value) }

func chStr(text string) []byte { return append(chVar(uint64(len(text))), text...) }

func chCat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// chBlock은 revision 54492의 비압축 Native block이다. type 별 data는 호출하는 쪽이 만든다.
type chColumn struct {
	name, kind string
	data       []byte
}

func chBlock(rows uint64, columns ...chColumn) []byte {
	block := chCat([]byte{1, 0, 2, 0xff, 0xff, 0xff, 0xff, 3, 0, 0}, chVar(uint64(len(columns))), chVar(rows))
	for _, column := range columns {
		block = chCat(block, chStr(column.name), chStr(column.kind), []byte{0}, column.data)
	}
	return block
}

// chPacket은 packet type과 table name 뒤에 body를 붙인다.
func chPacket(packet byte, body ...[]byte) []byte {
	return chCat(append([][]byte{{packet}, chStr("")}, body...)...)
}

func chEmptyData() []byte { return chPacket(1, chBlock(0)) }

func chProgress() []byte { return []byte{3, 0, 0, 0, 0, 0, 0, 0} }

func chProfileInfo(rows uint64) []byte {
	return chCat([]byte{6}, chVar(rows), chVar(1), chVar(0), []byte{0, 0, 0, 0, 0})
}

func chEnd() []byte { return []byte{5} }

func chFrame(method byte, plain int, body []byte) []byte {
	frame := make([]byte, 16, 25+len(body))
	frame = append(frame, method)
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(body)+9))
	frame = binary.LittleEndian.AppendUint32(frame, uint32(plain))
	return append(frame, body...)
}

func chException(code int32, name, message string) []byte {
	return chCat([]byte{2}, binary.LittleEndian.AppendUint32(nil, uint32(code)), chStr(name), chStr(message), chStr("stack"), []byte{0})
}

func chClientHello(revision uint64, user, password string) []byte {
	return chCat([]byte{0}, chStr("ClickHouse client"), chVar(26), chVar(9), chVar(revision), chStr(""), chStr(user), chStr(password))
}

func clickHouseTestRecord(socket uint64, server, request bool, at uint64, data []byte, size int) mysqlRecord {
	return mysqlRecord{bootTimeNS: at, pid: 9, process: "clickhouse", socket: socket, source: "127.0.0.1:40000", destination: "127.0.0.1:9000",
		sent: request != server, server: server, size: size, payload: data}
}

type clickHouseTestConn struct {
	t       *testing.T
	tracker *clickHouseTracker
	socket  uint64
	server  bool
}

func newClickHouseTestConn(t *testing.T, server bool, side string, showSecrets bool) *clickHouseTestConn {
	return &clickHouseTestConn{t: t, tracker: newClickHouseTracker(side, showSecrets, 0), socket: 1, server: server}
}

func (conn *clickHouseTestConn) send(request bool, at uint64, data []byte, size int) []captureEvent {
	return conn.tracker.events(clickHouseTestRecord(conn.socket, conn.server, request, at, data, size), 0)
}

func (conn *clickHouseTestConn) request(at uint64, data []byte) []captureEvent {
	return conn.send(true, at, data, len(data))
}

func (conn *clickHouseTestConn) response(at uint64, data []byte) []captureEvent {
	return conn.send(false, at, data, len(data))
}

// responseCut은 앞 keep byte만 캡처한 응답 record다.
func (conn *clickHouseTestConn) responseCut(at uint64, data []byte, keep int) []captureEvent {
	return conn.send(false, at, data[:min(keep, len(data))], len(data))
}

// handshake는 Hello, server Hello, Addendum을 흘리고 그동안 나온 event를 돌려준다.
func (conn *clickHouseTestConn) handshake() []captureEvent {
	events := conn.request(1_000_000, clickHouseFixture(conn.t, "client_hello"))
	events = append(events, conn.response(2_500_000, clickHouseFixture(conn.t, "server_hello"))...)
	return append(events, conn.request(3_000_000, clickHouseFixture(conn.t, "client_addendum"))...)
}

func clickHouseOnly(t *testing.T, events []captureEvent) captureEvent {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	return events[0]
}

func clickHouseJSON(t *testing.T, events []captureEvent) string {
	t.Helper()
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// clickHouseStateHas는 tracker가 쥔 문자열과 byte 열 어디에도 needle이 없는지 찾는다. 값을 복사하지 않는 코드라도 필드 하나가
// record의 byte를 가리키고 있으면 여기서 보인다.
func clickHouseStateHas(value reflect.Value, needle string) bool {
	switch value.Kind() {
	case reflect.String:
		return strings.Contains(value.String(), needle)
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return bytes.Contains(value.Bytes(), []byte(needle))
		}
		for index := range value.Len() {
			if clickHouseStateHas(value.Index(index), needle) {
				return true
			}
		}
	case reflect.Array:
		for index := range value.Len() {
			if clickHouseStateHas(value.Index(index), needle) {
				return true
			}
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			if clickHouseStateHas(key, needle) || clickHouseStateHas(value.MapIndex(key), needle) {
				return true
			}
		}
	case reflect.Pointer, reflect.Interface:
		return !value.IsNil() && clickHouseStateHas(value.Elem(), needle)
	case reflect.Struct:
		for index := range value.NumField() {
			if clickHouseStateHas(value.Field(index), needle) {
				return true
			}
		}
	}
	return false
}

func TestClickHouseTrackerPairsHandshakeQueryAndFinalWriteOnBothSides(t *testing.T) {
	for _, server := range []bool{false, true} {
		conn := newClickHouseTestConn(t, server, "", false)
		side := map[bool]string{false: traceClientSide, true: traceServerSide}[server]
		events := conn.handshake()
		if len(events) != 2 {
			t.Fatalf("handshake events = %#v", events)
		}
		connect, hello := events[0], events[1]
		if info := connect.ClickHouse; connect.Event != "clickhouse_connect" || connect.Protocol != "clickhouse" || connect.Side != side || info.Command != "connect" || info.ClientName != "ClickHouse client" || info.ClientVersion != "26.9" || info.Revision != 54492 || info.User != "edc" || info.Database != "" {
			t.Fatalf("connect = %#v %#v", connect, info)
		}
		if info := hello.ClickHouse; hello.Event != "clickhouse_hello" || hello.answered != 1 || hello.LatencyMS == nil || *hello.LatencyMS != 1.5 || info.Command != "connect" || info.ServerName != "ClickHouse" || info.ServerVersion != "26.9" || info.Revision != 54492 || info.User != "edc" {
			t.Fatalf("hello = %#v %#v", hello, info)
		}
		query := clickHouseOnly(t, conn.request(10_000_000, clickHouseFixture(t, "client_query")))
		if info := query.ClickHouse; query.Event != "clickhouse_query" || query.Side != side || info.Command != "query" || info.SQL != "SELECT sleep(1)" || info.SQLTruncated || info.Compressed || info.ClientName != "ClickHouse client" || info.ClientVersion != "26.9.16" || info.User != "edc" || info.Revision != 54492 {
			t.Fatalf("query = %#v %#v", query, query.ClickHouse)
		}
		end := clickHouseOnly(t, conn.response(1_010_000_000, clickHouseFixture(t, "server_select_final")))
		if info := end.ClickHouse; end.Event != "clickhouse_end" || end.answered != 1 || end.LatencyMS == nil || *end.LatencyMS != 1000 || info.Command != "query" || info.SQL != "SELECT sleep(1)" || info.EndInferred || info.Rows == nil || *info.Rows != 1 {
			t.Fatalf("end = %#v %#v", end, end.ClickHouse)
		}
		// 질의가 끝난 뒤의 byte는 짝 없이 사라진다.
		if events := conn.response(1_020_000_000, chEnd()); len(events) != 0 {
			t.Fatalf("stray response = %#v", events)
		}
		for _, text := range []string{"fixture-host", clickHouseTestPassword} {
			if strings.Contains(clickHouseJSON(t, append(append([]captureEvent{}, events...), query, end)), text) {
				t.Fatalf("events contain %q", text)
			}
		}
	}
}

func TestClickHouseTrackerSkipsCompressedBlocksAndCountsCompressedConnectionOnce(t *testing.T) {
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	first := clickHouseOnly(t, conn.request(10_000_000, clickHouseFixture(t, "client_query_compressed")))
	if !first.ClickHouse.Compressed || !first.ClickHouse.firstCompressed {
		t.Fatalf("first query = %#v", first.ClickHouse)
	}
	end := clickHouseOnly(t, conn.response(20_000_000, clickHouseFixture(t, "server_select_final_compressed")))
	if end.Event != "clickhouse_end" || end.ClickHouse.EndInferred || !end.ClickHouse.Compressed || end.ClickHouse.Rows == nil || *end.ClickHouse.Rows != 200000 {
		t.Fatalf("end = %#v", end.ClickHouse)
	}
	second := clickHouseOnly(t, conn.request(30_000_000, clickHouseFixture(t, "client_query_compressed")))
	if !second.ClickHouse.Compressed || second.ClickHouse.firstCompressed {
		t.Fatalf("second query = %#v", second.ClickHouse)
	}
}

func TestClickHouseTrackerKeepsPasswordOutOfEventsAndStateWithShowSecrets(t *testing.T) {
	hello := clickHouseFixture(t, "client_hello")
	if !bytes.Contains(hello, []byte(clickHouseTestPassword)) {
		t.Fatal("client_hello fixture lost its fake password")
	}
	for _, showSecrets := range []bool{false, true} {
		conn := newClickHouseTestConn(t, false, "", showSecrets)
		events := conn.handshake()
		events = append(events, conn.request(10_000_000, clickHouseFixture(t, "client_query"))...)
		events = append(events, conn.response(20_000_000, clickHouseFixture(t, "server_select_final"))...)
		if len(events) != 4 {
			t.Fatalf("events = %#v", events)
		}
		if strings.Contains(clickHouseJSON(t, events), clickHouseTestPassword) || clickHouseStateHas(reflect.ValueOf(conn.tracker), clickHouseTestPassword) {
			t.Fatalf("password found (show secrets %t)", showSecrets)
		}
		// 질의 중에도 상태에 없다.
		again := newClickHouseTestConn(t, false, "", showSecrets)
		again.handshake()
		again.request(10_000_000, clickHouseFixture(t, "client_query"))
		if clickHouseStateHas(reflect.ValueOf(again.tracker), clickHouseTestPassword) || clickHouseStateHas(reflect.ValueOf(again.tracker), "fixture-host") {
			t.Fatalf("state holds a secret (show secrets %t)", showSecrets)
		}
	}
}

func TestClickHouseTrackerInfersTheEndWhenEndOfStreamIsOutsideTheCapture(t *testing.T) {
	for _, tc := range []struct {
		fixture, query string
		keep           int
	}{{"server_select_final", "client_query", 1024}, {"server_select_final", "client_query", 600}, {"server_select_final_compressed", "client_query_compressed", 1024}} {
		for _, server := range []bool{false, true} {
			conn := newClickHouseTestConn(t, server, "", false)
			conn.handshake()
			conn.request(10_000_000, clickHouseFixture(t, tc.query))
			final := clickHouseFixture(t, tc.fixture)
			end := clickHouseOnly(t, conn.responseCut(30_000_000, final, tc.keep))
			if end.Event != "clickhouse_end" || !end.ClickHouse.EndInferred || end.ClickHouse.Rows == nil || end.LatencyMS == nil || *end.LatencyMS != 20 || end.answered != 1 {
				t.Fatalf("%s keep %d: end = %#v %#v", tc.fixture, tc.keep, end, end.ClickHouse)
			}
		}
	}
}

func TestClickHouseTrackerRefusesAFinalStartThatFailsTheStructureCheck(t *testing.T) {
	garbage := bytes.Repeat([]byte("x"), 200)
	for name, payload := range map[string][]byte{
		"byte 6 inside text":          chCat([]byte{6}, garbage),
		"valid ProfileInfo then text": chCat(chProfileInfo(1), garbage),
		"ProfileInfo only":            chProfileInfo(1),
		"bad boolean":                 chCat([]byte{6, 1, 1, 0, 9, 0, 0, 0, 0}, chProgress()),
		"Totals with an Array":        chCat(chPacket(7, chBlock(1, chColumn{"x", "Array(UInt8)", []byte{0}})), chProfileInfo(1)),
	} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		conn.request(10_000_000, clickHouseFixture(t, "client_query"))
		if events := conn.send(false, 20_000_000, payload, len(payload)+500); len(events) != 0 {
			t.Fatalf("%s: events = %#v", name, events)
		}
	}
}

func TestClickHouseTrackerReadsExceptionsCompleteOrCut(t *testing.T) {
	for _, keep := range []int{3117, 1024, 200} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		conn.request(10_000_000, clickHouseFixture(t, "client_query"))
		exception := clickHouseOnly(t, conn.responseCut(15_000_000, clickHouseFixture(t, "server_exception"), keep))
		info := exception.ClickHouse
		if exception.Event != "clickhouse_exception" || exception.answered != 1 || exception.LatencyMS == nil || *exception.LatencyMS != 5 || info.ErrorCode != 60 || info.ErrorName != "DB::Exception" || !strings.Contains(info.Message, "Unknown table") || info.Command != "query" || info.SQL != "SELECT sleep(1)" {
			t.Fatalf("keep %d: exception = %#v", keep, info)
		}
	}
	// 연결 거부도 Exception이다. connect의 응답이고 연결은 더 읽지 않는다.
	conn := newClickHouseTestConn(t, false, "", false)
	conn.request(1_000_000, clickHouseFixture(t, "client_hello"))
	exception := clickHouseOnly(t, conn.response(2_000_000, chException(516, "DB::Exception", "edc: Authentication failed")))
	if exception.Event != "clickhouse_exception" || exception.ClickHouse.Command != "connect" || exception.ClickHouse.ErrorCode != 516 || exception.answered != 1 {
		t.Fatalf("auth failure = %#v", exception.ClickHouse)
	}
	if events := conn.request(3_000_000, clickHouseFixture(t, "client_query")); len(events) != 0 {
		t.Fatalf("query after auth failure = %#v", events)
	}
}

func TestClickHouseTrackerAnswersADdlWithOneEndOfStreamByte(t *testing.T) {
	conn := newClickHouseTestConn(t, false, "", false)
	conn.handshake()
	conn.request(10_000_000, clickHouseFixture(t, "client_query"))
	end := clickHouseOnly(t, conn.response(14_000_000, clickHouseFixture(t, "server_ddl_final")))
	if end.Event != "clickhouse_end" || end.ClickHouse.Rows != nil || end.ClickHouse.EndInferred || *end.LatencyMS != 4 {
		t.Fatalf("ddl end = %#v", end.ClickHouse)
	}
}

func TestClickHouseTrackerWalksPacketsAcrossRecords(t *testing.T) {
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	conn.request(10_000_000, clickHouseFixture(t, "client_query"))
	events := conn.response(11_000_000, chCat(chEmptyData(), chProgress()))
	events = append(events, conn.response(12_000_000, chPacket(3, nil)[:0])...)
	if len(events) != 0 {
		t.Fatalf("events = %#v", events)
	}
	// 압축하지 않은 큰 block은 데이터를 읽지 않고 크기로 건너뛴다. 두 record에 걸쳐도 경계를 지킨다.
	rows := uint64(100000)
	data := chPacket(1, chBlock(rows, chColumn{"n", "UInt64", make([]byte, 0)}))
	skip := int(rows) * 8
	if events := conn.send(false, 13_000_000, data, len(data)+skip/2); len(events) != 0 {
		t.Fatalf("first half = %#v", events)
	}
	final := chCat(chProfileInfo(rows), chProgress(), chEnd())
	if events := conn.send(false, 14_000_000, nil, skip-skip/2); len(events) != 0 {
		t.Fatalf("second half = %#v", events)
	}
	end := clickHouseOnly(t, conn.response(15_000_000, final))
	if end.Event != "clickhouse_end" || end.ClickHouse.EndInferred || *end.ClickHouse.Rows != rows || *end.LatencyMS != 5 {
		t.Fatalf("end = %#v", end.ClickHouse)
	}
}

func TestClickHouseTrackerSkipsCompressedFramesAcrossRecords(t *testing.T) {
	conn := newClickHouseTestConn(t, false, "", false)
	conn.handshake()
	conn.request(10_000_000, clickHouseFixture(t, "client_query_compressed"))
	// 블록 하나가 frame 둘이다. 첫 frame은 압축 전 크기가 1 MiB라 같은 block이 이어진다.
	stream := chCat(chPacket(1), chFrame(clickHouseMethodZSTD, clickHouseFrameSize, bytes.Repeat([]byte{7}, 300)), chFrame(clickHouseMethodZSTD, 1000, bytes.Repeat([]byte{8}, 200)), chEnd())
	if events := conn.send(false, 11_000_000, stream[:60], 150); len(events) != 0 {
		t.Fatalf("first record = %#v", events)
	}
	end := clickHouseOnly(t, conn.send(false, 12_000_000, stream[150:], len(stream)-150))
	if end.Event != "clickhouse_end" || end.ClickHouse.EndInferred || *end.LatencyMS != 2 {
		t.Fatalf("end = %#v", end.ClickHouse)
	}
	// 둘째 frame의 압축 전 크기가 작으면 block이 끝난 것이므로 다음 byte는 packet이다. 틀린 method는 구조 오류다.
	conn = newClickHouseTestConn(t, false, "", false)
	conn.handshake()
	conn.request(10_000_000, clickHouseFixture(t, "client_query_compressed"))
	bad := chCat(chPacket(1), chFrame(0x55, 10, make([]byte, 20)), chEnd())
	if events := conn.response(11_000_000, bad); len(events) != 0 {
		t.Fatalf("bad method = %#v", events)
	}
}

func TestClickHouseTrackerLosesTheBoundaryOnTypesItCannotSkipAndRecoversAtTheNextFinalWrite(t *testing.T) {
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	conn.request(10_000_000, clickHouseFixture(t, "client_query"))
	array := chPacket(1, chBlock(1, chColumn{"a", "Array(UInt8)", []byte{0}}))
	if events := conn.response(11_000_000, chCat(chEmptyData(), array, chEnd())); len(events) != 0 {
		t.Fatalf("array block = %#v", events)
	}
	// 경계를 모르는 흐름에서는 packet 중간으로 보이는 byte를 읽지 않는다.
	if events := conn.response(12_000_000, chCat(chProgress(), chEnd())); len(events) != 0 {
		t.Fatalf("mid-flow record = %#v", events)
	}
	final := chCat(chProfileInfo(7), chProgress(), chEmptyData(), chProgress(), chEnd())
	end := clickHouseOnly(t, conn.responseCut(30_000_000, final, len(final)-1))
	if !end.ClickHouse.EndInferred || *end.ClickHouse.Rows != 7 || *end.LatencyMS != 20 {
		t.Fatalf("end = %#v", end.ClickHouse)
	}
	// 허용 목록 type은 행이 있어도 건너뛴다. 이름이 같은 type의 인자는 상관없다.
	conn.request(40_000_000, clickHouseFixture(t, "client_query"))
	listed := chPacket(1, chBlock(2, chColumn{"d", "DateTime('UTC')", make([]byte, 8)}, chColumn{"e", "Enum8('a' = 1)", []byte{1, 1}}, chColumn{"s", "String", chCat(binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(nil, 2), 5), []byte("abcde"))}))
	if end := clickHouseOnly(t, conn.response(50_000_000, chCat(listed, chEnd()))); end.ClickHouse.EndInferred {
		t.Fatalf("allowed types = %#v", end.ClickHouse)
	}
}

func TestClickHouseTrackerLeavesQueriesUnansweredWhenTheBoundaryIsLostOutsideTheCapture(t *testing.T) {
	conn := newClickHouseTestConn(t, false, "", false)
	var events []captureEvent
	events = append(events, conn.handshake()...)
	events = append(events, conn.request(10_000_000, clickHouseFixture(t, "client_query"))...)
	// client 읽기가 Data 중간에서 시작하거나, 읽기 끝이 캡처 밖이라 다음 packet 위치를 모른다.
	mid := bytes.Repeat([]byte("x"), 1024)
	events = append(events, conn.send(false, 11_000_000, mid, 907323)...)
	events = append(events, conn.send(false, 12_000_000, chEmptyData(), 5000)...)
	events = append(events, conn.send(false, 13_000_000, chCat(chProgress(), chProgress()), 3000)...)
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	// 새 Query가 오면 앞 질의는 응답 없이 버리고 새 질의가 짝지어진다.
	second := chCat(clickHouseFixture(t, "client_query")[:99], []byte{0x0f}, []byte("SELECT sleep(2)"))
	second = chCat(second, []byte{0}, chEmptyData())
	query := clickHouseOnly(t, conn.request(20_000_000, second))
	end := clickHouseOnly(t, conn.response(25_000_000, chCat(chProfileInfo(1), chProgress(), chEnd())))
	if query.ClickHouse.SQL != "SELECT sleep(2)" || end.ClickHouse.SQL != "SELECT sleep(2)" || *end.LatencyMS != 5 {
		t.Fatalf("second query = %#v, end = %#v", query.ClickHouse, end.ClickHouse)
	}
	summarizer := newClickHouseTraceSummarizer()
	for _, event := range append(events, query, end) {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(clickHouseTraceReport)
	if report.Queries != 2 || report.Responses != 1 || report.Unanswered != 1 {
		t.Fatalf("report = %+v", report.traceClickHouseCounts)
	}
}

func TestClickHouseTrackerIgnoresASocketWithoutHelloUntilTheNextHello(t *testing.T) {
	conn := newClickHouseTestConn(t, true, "", false)
	if events := conn.request(1_000_000, clickHouseFixture(t, "client_query")); len(events) != 0 {
		t.Fatalf("query without Hello = %#v", events)
	}
	if events := conn.response(2_000_000, clickHouseFixture(t, "server_select_final")); len(events) != 0 {
		t.Fatalf("response without Hello = %#v", events)
	}
	// 연결 중간에 시작해 Hello 응답만 본 경우도 revision을 모르면 해석하지 않는다.
	if events := conn.response(3_000_000, clickHouseFixture(t, "server_hello")); len(events) != 0 {
		t.Fatalf("server Hello alone = %#v", events)
	}
	conn.handshake()
	if clickHouseOnly(t, conn.request(10_000_000, clickHouseFixture(t, "client_query"))).Event != "clickhouse_query" {
		t.Fatal("query after Hello is not traced")
	}
	// 같은 socket 주소로 새 Hello가 오면 앞 질의는 버린다.
	conn.request(20_000_000, clickHouseFixture(t, "client_hello"))
	if events := conn.response(30_000_000, clickHouseFixture(t, "server_select_final")); len(events) != 0 {
		t.Fatalf("response of the dropped query = %#v", events)
	}
}

func TestClickHouseTrackerStopsReadingAChunkedConnection(t *testing.T) {
	conn := newClickHouseTestConn(t, false, "", false)
	conn.request(1_000_000, clickHouseFixture(t, "client_hello"))
	conn.response(2_000_000, clickHouseFixture(t, "server_hello"))
	addendum := chCat(chStr(""), chStr("chunked"), chStr("notchunked"), chVar(8))
	chunked := clickHouseOnly(t, conn.request(3_000_000, addendum))
	if chunked.Event != "clickhouse_chunked" || !chunked.ClickHouse.Chunked || chunked.ClickHouse.User != "edc" {
		t.Fatalf("chunked = %#v", chunked.ClickHouse)
	}
	if events := conn.request(4_000_000, clickHouseFixture(t, "client_query")); len(events) != 0 {
		t.Fatalf("query after chunked = %#v", events)
	}
	// Addendum을 읽지 못하면 chunked 여부를 모르므로 그 연결도 읽지 않는다.
	broken := newClickHouseTestConn(t, false, "", false)
	broken.request(1_000_000, clickHouseFixture(t, "client_hello"))
	broken.response(2_000_000, clickHouseFixture(t, "server_hello"))
	broken.request(3_000_000, []byte{0x0a, 'x'})
	if events := broken.request(4_000_000, clickHouseFixture(t, "client_query")); len(events) != 0 {
		t.Fatalf("query after a broken Addendum = %#v", events)
	}
}

func TestClickHouseTrackerReadsAQueryThatArrivesInTheSameRecordAsTheAddendum(t *testing.T) {
	// server는 Addendum과 Query를 recv 한 번(24 + 130 byte)으로 받을 때가 있다.
	for _, compressed := range []string{"client_query", "client_query_compressed"} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.request(1_000_000, clickHouseFixture(t, "client_hello"))
		conn.response(2_000_000, clickHouseFixture(t, "server_hello"))
		together := chCat(clickHouseFixture(t, "client_addendum"), clickHouseFixture(t, compressed))
		query := clickHouseOnly(t, conn.request(3_000_000, together))
		if info := query.ClickHouse; query.Event != "clickhouse_query" || info.SQL == "" || info.Compressed != (compressed == "client_query_compressed") || info.ClientName != "ClickHouse client" {
			t.Fatalf("%s: query = %#v %#v", compressed, query, query.ClickHouse)
		}
		end := clickHouseOnly(t, conn.response(1_003_000_000, clickHouseFixture(t, "server_ddl_final")))
		if end.Event != "clickhouse_end" || end.LatencyMS == nil || *end.LatencyMS != 1000 {
			t.Fatalf("%s: end = %#v", compressed, end)
		}
	}
	// 뒤에 붙은 byte가 Query가 아니면 이전처럼 그 연결을 읽지 않는다.
	broken := newClickHouseTestConn(t, true, "", false)
	broken.request(1_000_000, clickHouseFixture(t, "client_hello"))
	broken.response(2_000_000, clickHouseFixture(t, "server_hello"))
	if events := broken.request(3_000_000, chCat(clickHouseFixture(t, "client_addendum"), []byte{0x0a, 'x'})); len(events) != 0 {
		t.Fatalf("events after a trailing non-Query = %#v", events)
	}
	if events := broken.request(4_000_000, clickHouseFixture(t, "client_query")); len(events) != 0 {
		t.Fatalf("query after a trailing non-Query = %#v", events)
	}
	// chunked Addendum 뒤의 byte는 경계를 모르므로 Query로 읽지 않는다.
	chunked := newClickHouseTestConn(t, true, "", false)
	chunked.request(1_000_000, clickHouseFixture(t, "client_hello"))
	chunked.response(2_000_000, clickHouseFixture(t, "server_hello"))
	events := chunked.request(3_000_000, chCat(chStr(""), chStr("chunked"), chStr("notchunked"), chVar(8), clickHouseFixture(t, "client_query")))
	if len(events) != 1 || events[0].Event != "clickhouse_chunked" {
		t.Fatalf("chunked with a Query behind it = %#v", events)
	}
}

func TestClickHouseTrackerRejectsRevisionsItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		revision uint64
		events   int
	}{{54494, 1}, {54495, 0}, {1 << 40, 0}, {54428, 0}, {54429, 1}} {
		conn := newClickHouseTestConn(t, false, "", false)
		events := conn.request(1_000_000, chClientHello(tc.revision, "edc", "pw"))
		if len(events) != tc.events {
			t.Fatalf("revision %d: events = %#v", tc.revision, events)
		}
		conn.response(2_000_000, clickHouseFixture(t, "server_hello"))
		conn.request(3_000_000, clickHouseFixture(t, "client_addendum"))
		queries := conn.request(4_000_000, clickHouseFixture(t, "client_query"))
		if want := map[uint64]int{54494: 1, 54429: 0}[tc.revision]; len(queries) != want {
			// 54429는 Addendum이 없는 revision이라 Query를 읽지만 ClientInfo가 다르므로 구조 검사에서 걸러진다.
			t.Fatalf("revision %d: queries = %#v", tc.revision, queries)
		}
	}
	// Hello의 password 길이가 record와 맞지 않으면 Hello가 아니다.
	hello := chClientHello(54492, "edc", "pw")
	if events := newClickHouseTestConn(t, false, "", false).request(1_000_000, hello[:len(hello)-1]); len(events) != 0 {
		t.Fatalf("short password = %#v", events)
	}
}

func TestClickHouseTrackerReadsQueryTextAtTheCaptureBoundary(t *testing.T) {
	full := clickHouseFixture(t, "client_query")
	if full[98] != 0 || full[99] != 15 || string(full[100:115]) != "SELECT sleep(1)" {
		t.Fatalf("fixture layout changed: % x", full[96:116])
	}
	for _, tc := range []struct {
		keep      int
		sql       string
		truncated bool
	}{{130, "SELECT sleep(1)", false}, {115, "SELECT sleep(1)", false}, {106, "SELECT", true}, {100, "", true}, {99, "", true}, {98, "", true}, {40, "", true}, {3, "", true}} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		query := clickHouseOnly(t, conn.send(true, 10_000_000, full[:tc.keep], len(full)+400))
		if query.ClickHouse.SQL != tc.sql || query.ClickHouse.SQLTruncated != tc.truncated {
			t.Fatalf("keep %d: sql = %q truncated %t", tc.keep, query.ClickHouse.SQL, query.ClickHouse.SQLTruncated)
		}
	}
	// 캡처가 아니라 record가 잘렸다면 구조 오류다.
	for _, keep := range []int{3, 40, 98, 99, 100, 106} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		if events := conn.request(10_000_000, full[:keep]); len(events) != 0 {
			t.Fatalf("record cut at %d = %#v", keep, events)
		}
	}
	// 4096 byte를 넘는 SQL은 1024 byte까지 쓴다. 가리기는 보이는 앞부분에 한다.
	long := "SELECT '" + strings.Repeat("a", 6000) + "'"
	big := chCat(full[:99], chStr(long), []byte{0}, chEmptyData())
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	query := clickHouseOnly(t, conn.send(true, 10_000_000, big[:4096], len(big)))
	if !query.ClickHouse.SQLTruncated || query.ClickHouse.SQL != "SELECT '?" || strings.Contains(clickHouseJSON(t, []captureEvent{query}), "aaaa") {
		t.Fatalf("masked long sql = %q", query.ClickHouse.SQL)
	}
	conn = newClickHouseTestConn(t, true, "", true)
	conn.handshake()
	query = clickHouseOnly(t, conn.send(true, 10_000_000, big[:4096], len(big)))
	if !query.ClickHouse.SQLTruncated || len(query.ClickHouse.SQL) != clickHouseSQLLimit {
		t.Fatalf("shown long sql = %d bytes", len(query.ClickHouse.SQL))
	}
}

func TestClickHouseTrackerRejectsMalformedQueryFields(t *testing.T) {
	full := clickHouseFixture(t, "client_query")
	compressed := clickHouseFixture(t, "client_query_compressed")
	patch := func(data []byte, offset int, value byte) []byte {
		out := bytes.Clone(data)
		out[offset] = value
		return out
	}
	for name, data := range map[string][]byte{
		"compression 2":                   patch(compressed, 97, 2),
		"stage 5":                         patch(full, 97, 5),
		"stage 9":                         patch(full, 97, 9),
		"query id longer than the record": chCat([]byte{1, 0x7f}, full[2:]),
		"unterminated varuint":            chCat([]byte{1, 0}, bytes.Repeat([]byte{0xff}, 11), full[3:]),
		"interface HTTP":                  patch(full, 2+1+1+1+10+8, 2),
	} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		events := conn.request(10_000_000, data)
		if len(events) != 0 {
			t.Fatalf("%s: events = %#v", name, events)
		}
	}
	for _, stage := range []byte{4, 7} {
		conn := newClickHouseTestConn(t, true, "", false)
		conn.handshake()
		if events := conn.request(10_000_000, patch(full, 97, stage)); len(events) != 1 || events[0].Event != "clickhouse_query" {
			t.Fatalf("stage %d: events = %#v", stage, events)
		}
	}
	// Query가 아닌 byte로 시작하는 record는 읽지 않는다.
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	if events := conn.request(10_000_000, patch(full, 0, 2)); len(events) != 0 {
		t.Fatalf("data packet = %#v", events)
	}
}

func TestClickHouseTrackerEscapesControlCharactersInSQL(t *testing.T) {
	full := clickHouseFixture(t, "client_query")
	data := chCat(full[:99], chStr("SELECT 1 -- \x1b[31mred\x07"), []byte{0}, chEmptyData())
	conn := newClickHouseTestConn(t, true, "", false)
	conn.handshake()
	query := clickHouseOnly(t, conn.request(10_000_000, data))
	if strings.ContainsAny(query.ClickHouse.SQL, "\x1b\x07") || !strings.Contains(query.ClickHouse.SQL, `\x1b`) {
		t.Fatalf("sql = %q", query.ClickHouse.SQL)
	}
	message := clickHouseOnly(t, func() []captureEvent {
		conn.response(11_000_000, chCat(chEmptyData()))
		return conn.response(12_000_000, chException(60, "DB::Exception", "bad \x1b[2J table"))
	}())
	if strings.Contains(message.ClickHouse.Message, "\x1b") {
		t.Fatalf("message = %q", message.ClickHouse.Message)
	}
}

func TestClickHouseTrackerFiltersBySideAndForgetsSockets(t *testing.T) {
	conn := newClickHouseTestConn(t, true, traceClientSide, false)
	if events := conn.handshake(); len(events) != 0 {
		t.Fatalf("server record on a client tracker = %#v", events)
	}
	conn = newClickHouseTestConn(t, false, traceClientSide, false)
	if events := conn.handshake(); len(events) != 2 {
		t.Fatalf("client record on a client tracker = %#v", events)
	}
	conn.tracker.forgetSocket(conn.socket)
	if _, ok := conn.tracker.conns[conn.socket]; ok {
		t.Fatal("socket not forgotten")
	}
	conn.tracker.forgetSocket(99)
}

func TestClickHouseTrackerClearsEverySocketAtTheLimit(t *testing.T) {
	tracker := newClickHouseTracker("", false, 0)
	hello := clickHouseFixture(t, "client_hello")
	for socket := uint64(1); socket <= clickHouseSocketLimit; socket++ {
		tracker.events(clickHouseTestRecord(socket, false, true, socket, hello, len(hello)), 0)
	}
	if len(tracker.conns) != clickHouseSocketLimit {
		t.Fatalf("sockets = %d", len(tracker.conns))
	}
	tracker.events(clickHouseTestRecord(clickHouseSocketLimit+1, false, true, 1, hello, len(hello)), 0)
	if len(tracker.conns) != 1 {
		t.Fatalf("sockets after the limit = %d", len(tracker.conns))
	}
}

func TestClickHouseMaskSQLTable(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{`SELECT 'a''b', 'c\'d' FROM t`, `SELECT '?', '?' FROM t`},
		{`SELECT x'4142', b'0101', X'41'`, `SELECT x'?', b'?', X'?'`},
		{`SELECT ‘abc’, ‘q\’r’`, `SELECT ‘?’, ‘?’`},
		{`SELECT $$secret$$, $tag$it's 'x'$tag$, $a$ $b$ body $a$`, `SELECT $$?$$, $tag$?$tag$, $a$?$a$`},
		{`SELECT "it's", ` + "`a'b`" + `, “don't”`, `SELECT "it's", ` + "`a'b`" + `, “don't”`},
		{`SELECT 1 -- it's 'x'` + "\n" + `, 'y'`, `SELECT 1 -- it's 'x'` + "\n" + `, '?'`},
		{`SELECT 1 /* a /* 'nested' */ 'still comment' */ , 'z'`, `SELECT 1 /* a /* 'nested' */ 'still comment' */ , '?'`},
		{"SELECT 1 # it's\n, 'w'", "SELECT 1 # it's\n, '?'"},
		{"SELECT 1 #!it's\n, 'w'", "SELECT 1 #!it's\n, '?'"},
		{"SELECT 1 #'secret'", "SELECT 1 #'?'"},
		{"SELECT 1 // it's\n, 'v'", "SELECT 1 // it's\n, '?'"},
		{`SELECT 'open secret`, `SELECT '?`},
		{`SELECT $t$open secret`, `SELECT $t$?`},
		{`SELECT ‘open secret`, `SELECT ‘?`},
		{`SELECT /* open 'secret'`, `SELECT /* open 'secret'`},
		{`SELECT a$b$c, 'x'`, `SELECT a$b$c, '?'`},
		{`SELECT $1, 'x'`, `SELECT $1, '?'`},
		{`SELECT 1`, `SELECT 1`},
	} {
		if got := string(clickHouseMaskSQL([]byte(tc.sql))); got != tc.want {
			t.Errorf("mask(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestClickHouseShapeIgnoresValuesAndKeepsIdentifiers(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{"SELECT  *\nFROM t1   WHERE id = 42 AND name = 'bob'", "SELECT * FROM t1 WHERE id = ? AND name = ?"},
		{`SELECT 0x1F, 1.5e3, 2e-4, t_2, "col 7", ` + "`c8`", `SELECT ?, ?, ?, t_2, "col 7", ` + "`c8`"},
		{`SELECT $$x$$, ‘y’, "it's" FROM t`, `SELECT ?, ?, "it's" FROM t`},
		{`SELECT 1 /* c 2 */ -- d 3`, `SELECT ? /* c 2 */ -- d 3`},
		{`SELECT a$b FROM 1c`, `SELECT a$b FROM 1c`},
	} {
		if got := traceClickHouseShape(tc.sql); got != tc.want {
			t.Errorf("shape(%q) = %q, want %q", tc.sql, got, tc.want)
		}
		if masked := traceClickHouseShape(string(clickHouseMaskSQL([]byte(tc.sql)))); masked != traceClickHouseShape(tc.sql) {
			t.Errorf("masked shape(%q) = %q", tc.sql, masked)
		}
	}
	long := traceClickHouseShape("SELECT " + strings.Repeat("a", 400))
	if len(long) != clickHouseShapeLimit+len("...") || !strings.HasSuffix(long, "...") {
		t.Fatalf("long shape = %d bytes", len(long))
	}
}

func clickHouseSummaryEvents(t *testing.T) []captureEvent {
	t.Helper()
	var events []captureEvent
	for _, server := range []bool{false, true} {
		conn := newClickHouseTestConn(t, server, "", false)
		events = append(events, conn.handshake()...)
		events = append(events, conn.request(10_000_000, clickHouseFixture(t, "client_query"))...)
		events = append(events, conn.response(13_000_000, clickHouseFixture(t, "server_select_final"))...)
		events = append(events, conn.request(20_000_000, clickHouseFixture(t, "client_query_compressed"))...)
		events = append(events, conn.response(21_000_000, clickHouseFixture(t, "server_exception"))...)
		events = append(events, conn.request(30_000_000, clickHouseFixture(t, "client_query"))...)
	}
	events = append(events, captureEvent{Protocol: "clickhouse", Event: "clickhouse_chunked", Side: traceClientSide, ClickHouse: &traceClickHouseEvent{Command: "connect", Chunked: true, Interface: clickHouseInterfaceNative}})
	return events
}

func TestClickHouseTraceSummaryCountsSidesAndShapes(t *testing.T) {
	events := clickHouseSummaryEvents(t)
	summarizer := newClickHouseTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 2}, time.Second).(clickHouseTraceReport)
	if report.Queries != 6 || report.Responses != 4 || report.Errors != 2 || report.Unanswered != 2 || report.LostEvents != 2 || report.Connections != 2 || report.CompressedConnections != 2 || report.ChunkedConnections != 1 {
		t.Fatalf("report = %+v", report.traceClickHouseCounts)
	}
	if report.Client == nil || report.Server == nil || report.Client.Queries != 3 || report.Server.Queries != 3 || *report.Server.LatencyAvgMS != 2 || *report.Server.LatencyMaxMS != 3 {
		t.Fatalf("sides = %+v %+v", report.Client, report.Server)
	}
	var shapes []string
	for _, row := range report.Statements {
		shapes = append(shapes, row.Side+" "+row.Command+" "+row.Shape)
	}
	want := []string{"client query SELECT sleep(?)", "server query SELECT sleep(?)", "client connect (connect)", "server connect (connect)"}
	if strings.Join(shapes, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q, want %q", shapes, want)
	}
	if first := report.Statements[0]; first.Queries != 3 || first.Errors != 1 || first.Unanswered != 1 || len(first.Processes) != 1 || first.Processes[0] != "clickhouse" {
		t.Fatalf("first row = %+v", first)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"client":{"queries":3`, `"server":{"queries":3`, `"statements":[{"side":"client","interface":"native","command":"query","shape":"SELECT sleep(?)"`, `"lost_events":2`, `"chunked_connections":1`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(true) })
	for _, text := range []string{"Client side (queries that this host sent):\n  Queries: 3\n", "Server side (queries that local servers received):\n", "Lost events: 2\n", "  Chunked connections: 1\n", "\nSIDE\tCOMMAND\tSQL\tCOUNT\tERRORS\tUNANSWERED\tAVG\tMAX\tPROCESS\n", "\nclient\tquery\tSELECT sleep(?)\t3\t1\t1\t"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	single := newClickHouseTraceSummarizer()
	for _, event := range events[:6] {
		single.observe(event)
	}
	one := single.summarize(captureSummary{}, time.Second).(clickHouseTraceReport)
	if one.Side != traceClientSide || one.Client != nil || one.Server != nil {
		t.Fatalf("client-only report = %#v", one)
	}
	if output := traceCaptureOutput(t, &os.Stdout, func() { one.print(false) }); !strings.HasPrefix(output, "ClickHouse trace: ") || strings.Contains(output, "SIDE") || !strings.Contains(output, "\nQueries: 2\n") {
		t.Fatalf("client-only summary = %q", output)
	}
}

func TestClickHouseTraceSummaryShowsInterfaceColumnOnlyWhenMixed(t *testing.T) {
	httpEvent := func(name string, latency *float64, info traceClickHouseEvent) captureEvent {
		info.Command, info.SQL, info.Interface = clickHouseCommandQuery, "SELECT 1", clickHouseInterfaceHTTP
		return captureEvent{Protocol: "clickhouse", Event: name, Side: traceServerSide, LatencyMS: latency, ClickHouse: &info, answered: 1}
	}
	latency := 4.0
	events := []captureEvent{
		httpEvent(clickHouseEventQuery, nil, traceClickHouseEvent{}),
		httpEvent(clickHouseEventEnd, &latency, traceClickHouseEvent{HTTPStatus: 200}),
	}
	run := func(events []captureEvent) (clickHouseTraceReport, string) {
		summarizer := newClickHouseTraceSummarizer()
		for _, event := range events {
			summarizer.observe(event)
		}
		report := summarizer.summarize(captureSummary{}, time.Second).(clickHouseTraceReport)
		return report, traceCaptureOutput(t, &os.Stdout, func() { report.print(false) })
	}
	report, output := run(events)
	if strings.Contains(output, "INTERFACE") || !strings.Contains(output, "\nCOMMAND\tSQL\t") || !strings.Contains(output, "\nquery\tSELECT ?\t1\t0\t0\t4.0ms\t4.0ms\t-\n") {
		t.Fatalf("http-only summary = %q", output)
	}
	if data, _ := json.Marshal(report); !strings.Contains(string(data), `"interface":"http"`) {
		t.Fatalf("report JSON = %s", data)
	}
	native := clickHouseSummaryEvents(t)[:6]
	for i := range native {
		if native[i].Side == traceServerSide {
			native[i].Side = ""
		}
	}
	events[0].Side, events[1].Side = "", ""
	_, output = run(append(slices.Clone(native), events...))
	for _, text := range []string{"\nINTERFACE\tCOMMAND\tSQL\t", "\nnative\tquery\tSELECT sleep(?)\t", "\nhttp\tquery\tSELECT ?\t1\t0\t0\t4.0ms\t4.0ms\t-\n"} {
		if !strings.Contains(output, text) {
			t.Fatalf("mixed summary %q does not contain %q", output, text)
		}
	}
	_, output = run(append(clickHouseSummaryEvents(t), events...))
	if !strings.Contains(output, "\nSIDE\tINTERFACE\tCOMMAND\t") {
		t.Fatalf("side and interface summary = %q", output)
	}
}

func TestClickHouseGroupColumnsAndScrollLabels(t *testing.T) {
	var titles, values []string
	counts := &traceClickHouseCounts{Queries: 3, Responses: 2, Errors: 1, Unanswered: 1}
	average, maximum := 12.34, 100.0
	counts.LatencyAvgMS, counts.LatencyMaxMS = &average, &maximum
	type group = traceGroupSummary
	for _, column := range traceClickHouseGroupColumns(func(group) *traceClickHouseCounts { return counts }) {
		titles = append(titles, column.screenTitle)
		values = append(values, column.value(group{}))
	}
	if strings.Join(titles, " ") != "CMD RSP ERR NOANS AVGms MAXms" || strings.Join(values, " ") != "3 2 1 1 12.3 100.0" {
		t.Fatalf("columns = %q %q", titles, values)
	}
	for _, column := range traceClickHouseGroupColumns(func(group) *traceClickHouseCounts { return nil }) {
		if column.value(group{}) != "-" {
			t.Fatalf("empty column %q = %q", column.screenTitle, column.value(group{}))
		}
	}
	events := clickHouseSummaryEvents(t)
	for index, want := range [][2]string{
		{"client: edc ClickHouse client", "clickhouse_connect"},
		{"client: ↳ hello ClickHouse 26.9", "clickhouse_hello 1.5ms"},
		{"client: SELECT sleep(1)", "clickhouse_query"},
		{"client: ↳ 1 rows", "clickhouse_end 3.0ms"},
		{"client: SELECT sleep(1)", "clickhouse_query"},
		{"client: ↳ 60 (DB::Exception) DB::Exception: Unknown table expression identifier 'missing_table' in scope SELECT * FROM missing_table. Maybe you meant: ['missing_table']", "clickhouse_exception 1.0ms"},
	} {
		destination, label := traceClickHouseScrollLabels(events[index])
		if label != want[1] || (index != 5 && destination != want[0]) || (index == 5 && !strings.HasPrefix(destination, "client: ↳ 60 (DB::Exception) ")) {
			t.Fatalf("labels[%d] = %q, %q; want %q, %q", index, destination, label, want[0], want[1])
		}
	}
	inferred := captureEvent{Protocol: "clickhouse", Event: "clickhouse_end", Side: traceServerSide, ClickHouse: &traceClickHouseEvent{Command: "query", EndInferred: true}}
	if destination, _ := traceClickHouseScrollLabels(inferred); destination != "server: ↳ ok (inferred)" {
		t.Fatalf("inferred label = %q", destination)
	}
	long := captureEvent{Protocol: "clickhouse", Event: "clickhouse_query", Side: traceClientSide, ClickHouse: &traceClickHouseEvent{Command: "query", SQL: "SELECT\n\t" + strings.Repeat("x", 300)}}
	if destination, _ := traceClickHouseScrollLabels(long); strings.ContainsAny(destination, "\n\t") || len(destination) > len("client: SELECT ")+clickHouseScrollLabelSQL+len("...") {
		t.Fatalf("long label = %q", destination)
	}
	chunked := captureEvent{Protocol: "clickhouse", Event: "clickhouse_chunked", Side: traceClientSide, ClickHouse: &traceClickHouseEvent{Command: "connect", Chunked: true}}
	if destination, _ := traceClickHouseScrollLabels(chunked); destination != "client: chunked" {
		t.Fatalf("chunked label = %q", destination)
	}
}

func TestClickHouseNativeEventsCarryTheNativeInterface(t *testing.T) {
	conn := newClickHouseTestConn(t, false, "", false)
	events := conn.handshake()
	if len(events) != 2 {
		t.Fatalf("handshake events = %#v", events)
	}
	for _, event := range events {
		if event.ClickHouse.Interface != clickHouseInterfaceNative || event.ClickHouse.HTTPStatus != 0 {
			t.Errorf("%s = %#v", event.Event, event.ClickHouse)
		}
	}
	raw, err := json.Marshal(events[0])
	if err != nil || !strings.Contains(string(raw), `"interface":"native"`) || strings.Contains(string(raw), "http_status") {
		t.Fatalf("json = %s, %v", raw, err)
	}
}
