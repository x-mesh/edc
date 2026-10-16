package edc

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	traceClickHouseDefaultPort = 9000
	// traceClickHouseDefaultHTTPPort는 ClickHouse HTTP interface의 기본 port다.
	traceClickHouseDefaultHTTPPort = 8123
	clickHouseEventPrefix          = "clickhouse_"
	clickHouseEventConnect         = "clickhouse_connect"
	clickHouseEventHello           = "clickhouse_hello"
	clickHouseEventQuery           = "clickhouse_query"
	clickHouseEventEnd             = "clickhouse_end"
	clickHouseEventException       = "clickhouse_exception"
	clickHouseEventChunked         = "clickhouse_chunked"
	clickHouseCommandConnect       = "connect"
	clickHouseCommandQuery         = "query"
	clickHouseInterfaceNative      = "native"
	clickHouseInterfaceHTTP        = "http"
	// clickHouseSQLLimit은 event의 SQL 상한이다.
	clickHouseSQLLimit = 1024
	// clickHouseMessageLimit은 exception message의 상한이다.
	clickHouseMessageLimit = 1024
	// clickHouseTextLimit은 query id처럼 짧은 값의 상한이다.
	clickHouseTextLimit = 256
	// clickHouseShapeLimit은 요약 행의 SQL shape 길이 상한이다.
	clickHouseShapeLimit = 256
	// clickHouseSocketLimit을 넘으면 모든 연결의 상태를 비운다. 비운 뒤 이어지는 응답은 짝 없이 사라진다.
	clickHouseSocketLimit = 65536
	// clickHouseStringLimit은 String 하나의 길이로 받아들이는 값의 상한이다. 그보다 크면 다른 byte 열로 본다.
	clickHouseStringLimit = 1 << 24
	// clickHouseColumnLimit과 clickHouseRowLimit은 Native block의 column 수와 행 수로 받아들이는 값의 상한이다.
	clickHouseColumnLimit = 4096
	clickHouseRowLimit    = 1 << 31
	// clickHouseFrameSize는 압축 frame 하나가 담는 압축 전 byte의 최댓값이다. 이 크기로 꽉 찬 frame 뒤에는 같은 block의
	// frame이 이어진다.
	clickHouseFrameSize = 1 << 20
	// clickHouseFrameHeader는 checksum 16 byte, method 1 byte, 압축 크기와 압축 전 크기 각 4 byte다.
	clickHouseFrameHeader = 25
	// clickHouseFrameLimit은 frame의 압축 크기로 받아들이는 값의 상한이다.
	clickHouseFrameLimit = 1 << 28
	// clickHouseSettingLimit은 Query 하나에서 읽는 setting과 parameter의 수 상한이다.
	clickHouseSettingLimit = 1 << 12
	clickHouseVectorLimit  = 1 << 16
)

// revision 값은 ClickHouse의 DBMS_MIN_REVISION_WITH_* 상수다. 각 필드는 Hello에서 읽은 두 쪽 revision의 작은 값이
// 이 값 이상일 때 wire에 실린다.
const (
	clickHouseRevSettingFlags      = 54429
	clickHouseRevInterserverSecret = 54441
	clickHouseRevAddendum          = 54458
	clickHouseRevChunked           = 54470
	clickHouseRevParallelReplicas  = 54471
	clickHouseRevExternalRoles     = 54472
	clickHouseRevQuotaKey          = 54060
	clickHouseRevClientInfo        = 54032
	clickHouseRevStartTime         = 54449
	clickHouseRevDistributedDepth  = 54448
	clickHouseRevVersionPatch      = 54401
	clickHouseRevOpenTelemetry     = 54442
	clickHouseRevReplicaInfo       = 54453
	clickHouseRevScriptInfo        = 54475
	clickHouseRevJWT               = 54476
	clickHouseRevClientAgent       = 54485
	clickHouseRevInternal          = 54486
	clickHouseRevCurrentRoles      = 54488
	clickHouseRevQueryParameters   = 54459
	clickHouseRevCustomColumn      = 54454
	clickHouseRevProgressBytes     = 54463
	clickHouseRevProgressWritten   = 54420
	clickHouseRevProgressElapsed   = 54460
	clickHouseRevProfileAggregate  = 54469
	clickHouseRevBucketVector      = 54480
	clickHouseRevCompressedLog     = 54481
	clickHouseRevStringWithSize    = 54492
	// clickHouseMinRevision은 setting을 flag가 붙은 String으로 쓰는 첫 revision이다. 그 아래는 해석하지 않는다.
	clickHouseMinRevision = clickHouseRevSettingFlags
	// clickHouseMaxRevision은 소스에서 확인한 가장 큰 DBMS_TCP_PROTOCOL_VERSION이다. 그 위 client는 필드를 알 수 없다.
	clickHouseMaxRevision = 54494
)

const (
	clickHouseClientHello = 0
	clickHouseClientQuery = 1

	clickHouseServerHello         = 0
	clickHouseServerData          = 1
	clickHouseServerException     = 2
	clickHouseServerProgress      = 3
	clickHouseServerPong          = 4
	clickHouseServerEndOfStream   = 5
	clickHouseServerProfileInfo   = 6
	clickHouseServerTotals        = 7
	clickHouseServerExtremes      = 8
	clickHouseServerTablesStatus  = 9
	clickHouseServerLog           = 10
	clickHouseServerTableColumns  = 11
	clickHouseServerPartUUIDs     = 12
	clickHouseServerReadTask      = 13
	clickHouseServerProfileEvents = 14
	clickHouseServerTimezone      = 17
	clickHouseServerSSHChallenge  = 18
	clickHouseServerLastKnown     = 18

	// 압축 frame의 method byte다.
	clickHouseMethodNone = 0x02
	clickHouseMethodLZ4  = 0x82
	clickHouseMethodZSTD = 0x90

	// clickHouseInterfaceTCP는 ClientInfo의 interface 값이다. native 접속은 항상 TCP다.
	clickHouseInterfaceTCP = 1
	// clickHouseErrorCodeLimit은 경계를 모르는 흐름에서 Exception으로 받아들이는 code의 상한이다.
	clickHouseErrorCodeLimit = 1 << 16
	// server의 TCPHandler::receiveQuery와 같이 QueryProcessingStage::MAX(5) 미만과 QueryPlan(7)만 받는다.
	// QueryPlan의 plan은 다음 packet으로 오므로 Query packet의 나머지 배치는 같다.
	clickHouseStageMax       = 5
	clickHouseStageQueryPlan = 7
)

// traceClickHouseEvent는 ClickHouse event에만 붙는 값이다. 응답 event에는 답한 요청의 command, SQL 같은 값을 함께 붙인다.
// password, quota_key, setting과 parameter 값, interserver secret, ClientInfo의 host 이름과 JWT는 읽어도 담지 않는다.
type traceClickHouseEvent struct {
	Command       string  `json:"command,omitempty"`
	QueryID       string  `json:"query_id,omitempty"`
	SQL           string  `json:"sql,omitempty"`
	SQLTruncated  bool    `json:"sql_truncated,omitempty"`
	ClientName    string  `json:"client_name,omitempty"`
	ClientVersion string  `json:"client_version,omitempty"`
	Revision      uint64  `json:"revision,omitempty"`
	ServerName    string  `json:"server_name,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	User          string  `json:"user,omitempty"`
	Database      string  `json:"database,omitempty"`
	Compressed    bool    `json:"compressed,omitempty"`
	Chunked       bool    `json:"chunked,omitempty"`
	ErrorCode     int32   `json:"error_code,omitempty"`
	ErrorName     string  `json:"error_name,omitempty"`
	Message       string  `json:"message,omitempty"`
	Rows          *uint64 `json:"rows,omitempty"`
	EndInferred   bool    `json:"end_inferred,omitempty"`
	// Interface는 질의가 들어온 interface다. native는 TCP 9000, http는 HTTP 8123이다. HTTPStatus는 HTTP 응답 event에만 붙는다.
	Interface  string `json:"interface"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// firstCompressed는 연결에서 압축 질의를 처음 보낸 query event에만 붙는다. 압축은 질의마다 정해져서 연결 수를 셀 때 쓴다.
	firstCompressed bool
}

type clickHouseError uint8

const (
	clickHouseOK clickHouseError = iota
	// clickHouseShort는 필요한 byte가 캡처한 앞부분 밖에 있다는 뜻이다.
	clickHouseShort
	// clickHouseBad는 구조가 맞지 않는다는 뜻이다.
	clickHouseBad
	// clickHouseUnsupported는 구조는 맞지만 크기를 알 수 없는 type이라 건너뛸 수 없다는 뜻이다.
	clickHouseUnsupported
)

// clickHouseReader는 record 앞부분에서 wire 값을 읽는다. 처음 실패한 오류를 남기고 이후 읽기는 모두 실패한다.
// 값은 복사해서 돌려주지 않고 record의 byte를 그대로 보여 주므로, 쓰는 쪽이 필요한 만큼만 문자열로 바꾼다.
type clickHouseReader struct {
	data []byte
	size int
	pos  int
	err  clickHouseError
	// crossing은 값이 record 끝을 넘어 다음 record로 이어질 수 있다는 뜻이다. 응답 방향은 write 하나가 1 MiB 경계에서
	// 끊기므로 그렇다. 요청 방향은 write 하나에 message가 모두 있어서 넘으면 구조 오류다.
	crossing bool
}

func (reader *clickHouseReader) fail(err clickHouseError) {
	if reader.err == clickHouseOK {
		reader.err = err
	}
}

func (reader *clickHouseReader) take(n int) []byte {
	if reader.err != clickHouseOK {
		return nil
	}
	switch {
	case n < 0 || (!reader.crossing && n > reader.size-reader.pos):
		reader.fail(clickHouseBad)
		return nil
	case reader.pos+n > len(reader.data):
		reader.fail(clickHouseShort)
		return nil
	}
	value := reader.data[reader.pos : reader.pos+n]
	reader.pos += n
	return value
}

func (reader *clickHouseReader) u8() byte {
	if value := reader.take(1); value != nil {
		return value[0]
	}
	return 0
}

// varuint는 LEB128 정수를 읽는다. 10 byte를 넘으면 구조 오류다.
func (reader *clickHouseReader) varuint() uint64 {
	var value uint64
	for shift := uint(0); shift <= 63; shift += 7 {
		next := reader.take(1)
		if next == nil {
			return 0
		}
		if shift == 63 && next[0] > 1 {
			reader.fail(clickHouseBad)
			return 0
		}
		value |= uint64(next[0]&0x7f) << shift
		if next[0] < 0x80 {
			return value
		}
	}
	reader.fail(clickHouseBad)
	return 0
}

func (reader *clickHouseReader) length() int {
	n := reader.varuint()
	if reader.err == clickHouseOK && n > clickHouseStringLimit {
		reader.fail(clickHouseBad)
	}
	return int(n)
}

// str은 String 하나를 읽는다.
func (reader *clickHouseReader) str() []byte {
	n := reader.length()
	if reader.err != clickHouseOK {
		return nil
	}
	return reader.take(n)
}

// partial은 String 하나를 읽되 값이 캡처 밖으로 이어지면 보이는 앞부분만 돌려준다. 읽는 위치는 값 뒤로 옮긴다.
func (reader *clickHouseReader) partial() (text []byte, complete bool) {
	n := reader.length()
	if reader.err != clickHouseOK {
		return nil, false
	}
	if !reader.crossing && n > reader.size-reader.pos {
		reader.fail(clickHouseBad)
		return nil, false
	}
	have := max(0, min(n, len(reader.data)-reader.pos))
	text = reader.data[reader.pos : reader.pos+have]
	reader.pos += n
	return text, have == n
}

// skip은 읽지 않고 n byte를 건너뛴다. 건너뛴 곳이 캡처 밖이어도 위치만 옮기고, 그 뒤 읽기가 실패한다.
func (reader *clickHouseReader) skip(n uint64) {
	if reader.err != clickHouseOK {
		return
	}
	if n > 1<<40 {
		reader.fail(clickHouseBad)
		return
	}
	reader.pos += int(n)
}

type clickHousePhase uint8

// 새 socket은 clickHouseNone이고 Hello를 본 연결만 해석한다. 연결 중간에 trace를 시작한 경우다.
const (
	clickHouseNone clickHousePhase = iota
	clickHouseAwaitServer
	clickHouseAwaitAddendum
	clickHouseReady
	// clickHouseDead는 chunked, 알 수 없는 revision, 읽지 못한 Addendum처럼 더 해석하지 않는 연결이다.
	clickHouseDead
)

// clickHouseFlow는 응답 방향 byte 흐름에서 packet 경계를 따라간다. 경계를 모르면 synced가 false다.
type clickHouseFlow struct {
	synced bool
	// skip은 지금 요소에서 아직 지나가지 않은 byte 수다. frame은 skip 뒤에 압축 frame header가 온다는 뜻이다.
	skip  int
	frame bool
	rows  *uint64
}

type clickHousePending struct {
	at   uint64
	info traceClickHouseEvent
}

type clickHouseConn struct {
	phase clickHousePhase
	// user와 database는 client Hello에서 읽은 값이다. 질의 event에도 붙인다.
	user, database                 string
	clientRevision, serverRevision int
	// revision은 두 쪽 revision 중 작은 값이다. 두 쪽이 각자 상대의 revision에 맞춰 필드를 쓰기 때문이다.
	revision       int
	compressed     bool
	compressedSeen bool
	pending        *clickHousePending
	flow           clickHouseFlow
	// http는 HTTP interface socket의 상태다. native socket에는 없다.
	http *clickHouseHTTPConn
}

// clickHouseTracker는 socket마다 Hello부터 질의와 응답을 따라가며 짝짓는다. side는 tracker가 볼 쪽이고 비어 있으면
// 두 쪽을 모두 본다.
type clickHouseTracker struct {
	side        string
	showSecrets bool
	// httpPort가 0이 아니면 이 port의 socket을 HTTP로 읽는다. 0이면 모두 native다.
	httpPort uint16
	conns    map[uint64]*clickHouseConn
}

func newClickHouseTracker(side string, showSecrets bool, httpPort uint16) *clickHouseTracker {
	return &clickHouseTracker{side: side, showSecrets: showSecrets, httpPort: httpPort, conns: map[uint64]*clickHouseConn{}}
}

// forgetSocket은 socket의 상태를 지운다. HTTP 요청 중 SQL이 정해지지 않아 query event를 내지 못한 것은 여기서 낸다.
func (tracker *clickHouseTracker) forgetSocket(socket uint64) []captureEvent {
	conn := tracker.conns[socket]
	delete(tracker.conns, socket)
	if conn == nil || conn.http == nil {
		return nil
	}
	var out []captureEvent
	for _, request := range conn.http.queue {
		if !request.skip && !request.emitted {
			out = append(out, tracker.httpQueryEvent(request))
		}
	}
	return out
}

// httpRecord는 record가 HTTP interface의 것인지 알려 준다. 서버 쪽은 로컬 port가, client 쪽은 상대 port가 HTTP port일 때다.
func (tracker *clickHouseTracker) httpRecord(record mysqlRecord) bool {
	if tracker.httpPort == 0 {
		return false
	}
	if record.server {
		return record.localPort == tracker.httpPort
	}
	return record.peerPort == tracker.httpPort
}

// clickHouseCall은 record 하나를 처리하는 동안의 값이다.
type clickHouseCall struct {
	tracker *clickHouseTracker
	conn    *clickHouseConn
	record  mysqlRecord
	side    string
	offset  int64
	out     []captureEvent
	// parsed는 이번 record에서 구조 검사를 통과한 packet 수다.
	parsed int
}

func (tracker *clickHouseTracker) events(record mysqlRecord, clockOffset int64) []captureEvent {
	side := traceClientSide
	if record.server {
		side = traceServerSide
	}
	if tracker.side != "" && side != tracker.side {
		return nil
	}
	conn := tracker.conns[record.socket]
	if conn == nil {
		if len(tracker.conns) >= clickHouseSocketLimit {
			clear(tracker.conns)
		}
		conn = &clickHouseConn{}
		tracker.conns[record.socket] = conn
	}
	if tracker.httpRecord(record) {
		if conn.http == nil {
			conn.http = &clickHouseHTTPConn{}
		}
		call := &clickHouseHTTPCall{tracker: tracker, conn: conn.http, record: record, side: side, offset: clockOffset}
		if record.server != record.sent {
			call.request()
		} else {
			call.response()
		}
		return call.out
	}
	call := &clickHouseCall{tracker: tracker, conn: conn, record: record, side: side, offset: clockOffset}
	// client는 요청을 보내고 응답을 받는다. 서버는 요청을 받고 응답을 보낸다.
	if record.server != record.sent {
		call.request()
	} else {
		call.response()
	}
	return call.out
}

func (call *clickHouseCall) event(name string, length int, info *traceClickHouseEvent) captureEvent {
	info.Interface = clickHouseInterfaceNative
	return clickHouseEventAt(call.record, call.side, call.offset, name, length, info)
}

// clickHouseEventAt은 record가 가리키는 socket과 process, record 시각으로 event를 만든다.
func clickHouseEventAt(record mysqlRecord, side string, offset int64, name string, length int, info *traceClickHouseEvent) captureEvent {
	return captureEvent{
		SocketID: record.socket, TimestampNS: uint64(int64(record.bootTimeNS) + offset), BootTimeNS: record.bootTimeNS, Event: name, Protocol: "clickhouse", Side: side,
		PID: record.pid, Process: record.process, CgroupID: record.cgroupID, Source: record.source, Destination: record.destination, Bytes: uint64(length), ClickHouse: info,
	}
}

// clickHouseText는 event에 담을 짧은 값이다. 제어 문자를 escape하고 상한에서 자른다.
func clickHouseText(raw []byte, limit int) string {
	return traceEscapeText([]byte(traceTrimText(string(raw), limit)))
}

// sqlText는 event에 붙일 SQL이다. 문자열 literal은 --show-secrets가 아니면 가린다.
func (tracker *clickHouseTracker) sqlText(raw []byte, truncated bool) (string, bool) {
	if !tracker.showSecrets {
		raw = clickHouseMaskSQL(raw)
	}
	text := string(raw)
	if len(text) > clickHouseSQLLimit {
		text, truncated = traceTrimText(text, clickHouseSQLLimit), true
	}
	return traceEscapeText([]byte(text)), truncated
}

// request는 요청 방향 record 하나를 읽는다.
func (call *clickHouseCall) request() {
	conn, record := call.conn, call.record
	data := record.payload
	if len(data) == 0 {
		return
	}
	if conn.phase == clickHouseAwaitAddendum && call.addendum() {
		return
	}
	if data[0] == clickHouseClientHello && call.clientHello() {
		return
	}
	if conn.phase == clickHouseAwaitAddendum {
		// chunked 여부를 모르면 이어지는 byte의 경계도 모른다.
		conn.phase = clickHouseDead
		return
	}
	if conn.phase == clickHouseReady && data[0] == clickHouseClientQuery {
		call.query(0)
	}
}

func clickHousePrintable(value []byte) bool {
	for _, char := range value {
		if char < 0x20 || char >= 0x7f {
			return false
		}
	}
	return true
}

// clientHello는 요청 방향 record가 client Hello이면 새 연결로 시작하고 true를 돌려준다. 같은 socket 주소를 다시 쓴
// 연결의 앞 상태는 버린다. password는 길이만 읽고 건너뛴다. 어디에도 저장하지 않는다.
func (call *clickHouseCall) clientHello() bool {
	record := call.record
	reader := clickHouseReader{data: record.payload, size: record.size}
	reader.varuint()
	name := reader.str()
	major, minor, revision := reader.varuint(), reader.varuint(), reader.varuint()
	database, user := reader.str(), reader.str()
	passwordLength := reader.varuint()
	if reader.err != clickHouseOK || !clickHousePrintable(name) {
		return false
	}
	left := reader.size - reader.pos
	if passwordLength > uint64(left) || (reader.size == len(reader.data) && int(passwordLength) != left) {
		return false
	}
	conn := call.conn
	*conn = clickHouseConn{}
	if revision < clickHouseMinRevision || revision > clickHouseMaxRevision {
		conn.phase = clickHouseDead
		return true
	}
	conn.phase, conn.clientRevision = clickHouseAwaitServer, int(revision)
	conn.user, conn.database = clickHouseText(user, clickHouseTextLimit), clickHouseText(database, clickHouseTextLimit)
	info := traceClickHouseEvent{Command: clickHouseCommandConnect, ClientName: clickHouseText(name, clickHouseTextLimit), ClientVersion: fmt.Sprintf("%d.%d", major, minor), Revision: revision, User: conn.user, Database: conn.database}
	conn.pending = &clickHousePending{at: record.bootTimeNS, info: info}
	call.out = append(call.out, call.event(clickHouseEventConnect, record.size, &info))
	return true
}

// addendum은 server Hello 뒤 첫 요청 방향 record가 Addendum이면 읽고 true를 돌려준다. chunked framing이 정해지면 그
// 연결은 더 읽지 않는다.
func (call *clickHouseCall) addendum() bool {
	conn, record := call.conn, call.record
	reader := clickHouseReader{data: record.payload, size: record.size}
	reader.str()
	var send, receive []byte
	if conn.revision >= clickHouseRevChunked {
		send, receive = reader.str(), reader.str()
	}
	if conn.revision >= clickHouseRevParallelReplicas {
		reader.varuint()
	}
	// server는 Addendum과 Query를 recv 한 번으로 받을 수 있다. Addendum 뒤에 Query packet이 이어지면 이어서 읽는다.
	joined := reader.err == clickHouseOK && reader.pos < len(record.payload) && record.payload[reader.pos] == clickHouseClientQuery
	if reader.err != clickHouseOK || (reader.pos != record.size && !joined) {
		return false
	}
	conn.phase = clickHouseReady
	if string(send) == "chunked" || string(receive) == "chunked" {
		conn.phase, conn.pending = clickHouseDead, nil
		info := traceClickHouseEvent{Command: clickHouseCommandConnect, User: conn.user, Database: conn.database, Chunked: true}
		call.out = append(call.out, call.event(clickHouseEventChunked, reader.pos, &info))
		return true
	}
	if joined {
		call.query(reader.pos)
	}
	return true
}

// query는 Query packet을 읽는다. 구조가 맞지 않으면 event 없이 record를 버린다. 필드를 추측해서 맞추지 않는다.
func (call *clickHouseCall) query(start int) {
	conn, record := call.conn, call.record
	reader := clickHouseReader{data: record.payload, size: record.size, pos: start}
	info := traceClickHouseEvent{Command: clickHouseCommandQuery, User: conn.user, Database: conn.database, Revision: uint64(conn.revision)}
	fields := call.queryFields(&reader, &info)
	// 캡처 밖에서 끝난 것은 잘린 것이고, record 안에서 끝난 것은 구조 오류다.
	if reader.err == clickHouseBad || (reader.err != clickHouseOK && len(record.payload) >= record.size) {
		return
	}
	info.SQLTruncated = true
	if fields.reached {
		info.SQL, info.SQLTruncated = call.tracker.sqlText(fields.text, !fields.complete)
	}
	conn.compressed = fields.compression == 1
	info.Compressed = conn.compressed
	if conn.compressed && !conn.compressedSeen {
		conn.compressedSeen, info.firstCompressed = true, true
	}
	conn.pending = &clickHousePending{at: record.bootTimeNS, info: info}
	// 압축 여부를 읽지 못했으면 응답이 압축 frame인지 모르므로 경계를 모르는 상태로 시작한다.
	conn.flow = clickHouseFlow{synced: fields.compression >= 0}
	call.out = append(call.out, call.event(clickHouseEventQuery, record.size-start, &info))
}

type clickHouseQueryFields struct {
	// text는 query text에서 캡처한 앞부분이다. reached는 query text의 길이까지 읽었다는 뜻이고, complete는 text가 전부라는 뜻이다.
	text     []byte
	reached  bool
	complete bool
	// compression은 읽지 못했으면 -1이다.
	compression int
}

// queryFields는 Query packet을 query text와 parameter 끝까지 읽는다. setting과 parameter의 이름은 읽지만 값은 건너뛴다.
func (call *clickHouseCall) queryFields(reader *clickHouseReader, info *traceClickHouseEvent) clickHouseQueryFields {
	revision, fields := call.conn.revision, clickHouseQueryFields{compression: -1}
	reader.varuint()
	if id := reader.str(); reader.err == clickHouseOK {
		info.QueryID = clickHouseText(id, clickHouseTextLimit)
	}
	if revision >= clickHouseRevClientInfo {
		call.clientInfo(reader, info)
	}
	reader.pairs()
	if revision >= clickHouseRevExternalRoles {
		reader.str()
	}
	if revision >= clickHouseRevInterserverSecret {
		reader.str()
	}
	if stage := reader.varuint(); stage >= clickHouseStageMax && stage != clickHouseStageQueryPlan {
		reader.fail(clickHouseBad)
	}
	if value := reader.varuint(); reader.err == clickHouseOK {
		if value > 1 {
			reader.fail(clickHouseBad)
		} else {
			fields.compression = int(value)
		}
	}
	fields.text, fields.complete = reader.partial()
	if fields.reached = reader.err == clickHouseOK; fields.reached && fields.complete && revision >= clickHouseRevQueryParameters {
		reader.pairs()
	}
	return fields
}

// pairs는 이름이 빈 값으로 끝나는 setting이나 parameter 목록을 읽는다. 값은 건너뛴다.
func (reader *clickHouseReader) pairs() {
	for count := 0; reader.err == clickHouseOK; count++ {
		if count > clickHouseSettingLimit {
			reader.fail(clickHouseBad)
			return
		}
		if len(reader.str()) == 0 {
			return
		}
		reader.varuint()
		reader.str()
	}
}

// clientInfo는 ClientInfo를 읽는다. 질의를 보낸 client의 이름과 version만 담는다. 사용자 이름, client host 이름,
// quota_key, JWT는 건너뛰기만 한다.
func (call *clickHouseCall) clientInfo(reader *clickHouseReader, info *traceClickHouseEvent) {
	revision := call.conn.revision
	if reader.u8() == 0 {
		return
	}
	reader.str()
	reader.str()
	reader.str()
	if revision >= clickHouseRevStartTime {
		reader.skip(8)
	}
	if reader.u8() != clickHouseInterfaceTCP && reader.err == clickHouseOK {
		reader.fail(clickHouseBad)
	}
	reader.str()
	reader.str()
	name := reader.str()
	major, minor := reader.varuint(), reader.varuint()
	reader.varuint()
	if revision >= clickHouseRevQuotaKey {
		reader.str()
	}
	if revision >= clickHouseRevDistributedDepth {
		reader.varuint()
	}
	patch := uint64(0)
	if revision >= clickHouseRevVersionPatch {
		patch = reader.varuint()
	}
	if reader.err == clickHouseOK {
		info.ClientName = clickHouseText(name, clickHouseTextLimit)
		info.ClientVersion = fmt.Sprintf("%d.%d", major, minor)
		if revision >= clickHouseRevVersionPatch {
			info.ClientVersion += "." + strconv.FormatUint(patch, 10)
		}
	}
	if revision >= clickHouseRevOpenTelemetry {
		switch reader.u8() {
		case 0:
		case 1:
			reader.skip(24)
			reader.str()
			reader.u8()
		default:
			reader.fail(clickHouseBad)
		}
	}
	if revision >= clickHouseRevReplicaInfo {
		reader.varuint()
		reader.varuint()
		reader.varuint()
	}
	if revision >= clickHouseRevScriptInfo {
		reader.varuint()
		reader.varuint()
	}
	if revision >= clickHouseRevJWT {
		switch reader.u8() {
		case 0:
		case 1:
			reader.str()
		default:
			reader.fail(clickHouseBad)
		}
	}
	if revision >= clickHouseRevClientAgent {
		reader.str()
	}
	if revision >= clickHouseRevInternal {
		reader.u8()
	}
	if revision >= clickHouseRevCurrentRoles {
		switch reader.u8() {
		case 0:
		case 1:
			count := reader.varuint()
			if count > clickHouseVectorLimit {
				reader.fail(clickHouseBad)
			}
			for range count {
				reader.str()
			}
		default:
			reader.fail(clickHouseBad)
		}
	}
}

// respond는 응답 event를 만든다. 답한 요청의 command와 SQL, 응답 시간을 붙이고 pending을 비운다.
func (call *clickHouseCall) respond(name string, info traceClickHouseEvent) {
	conn, pending := call.conn, call.conn.pending
	request := pending.info
	info.Command, info.QueryID, info.SQL, info.SQLTruncated, info.Compressed = request.Command, request.QueryID, request.SQL, request.SQLTruncated, request.Compressed
	info.User, info.Database = cmp.Or(info.User, request.User), cmp.Or(info.Database, request.Database)
	info.ClientName, info.ClientVersion = request.ClientName, request.ClientVersion
	event := call.event(name, call.record.size, &info)
	event.LatencyMS, event.answered = traceSpan(pending.at, call.record.bootTimeNS), 1
	conn.pending = nil
	conn.flow = clickHouseFlow{}
	call.out = append(call.out, event)
}

// response는 응답 방향 record 하나를 읽는다.
func (call *clickHouseCall) response() {
	conn, record := call.conn, call.record
	if conn.pending == nil || record.size == 0 {
		return
	}
	switch conn.phase {
	case clickHouseAwaitServer:
		call.serverHello()
	case clickHouseReady:
		call.flowResponse()
	}
}

// serverHello는 client Hello의 응답으로 server Hello나 Exception을 읽는다.
func (call *clickHouseCall) serverHello() {
	conn, record := call.conn, call.record
	reader := clickHouseReader{data: record.payload, size: record.size, crossing: true}
	switch reader.varuint(); record.payload[0] {
	case clickHouseServerHello:
		name := reader.str()
		major, minor, revision := reader.varuint(), reader.varuint(), reader.varuint()
		if reader.err != clickHouseOK || !clickHousePrintable(name) || revision == 0 {
			return
		}
		conn.serverRevision = int(min(revision, 1<<31))
		conn.revision = min(conn.clientRevision, conn.serverRevision)
		call.respond(clickHouseEventHello, traceClickHouseEvent{ServerName: clickHouseText(name, clickHouseTextLimit), ServerVersion: fmt.Sprintf("%d.%d", major, minor), Revision: revision})
		switch {
		case conn.revision < clickHouseMinRevision:
			conn.phase = clickHouseDead
		case conn.revision >= clickHouseRevAddendum:
			conn.phase = clickHouseAwaitAddendum
		default:
			conn.phase = clickHouseReady
		}
	case clickHouseServerException:
		reader.pos = 1
		if info, ok := call.exceptionFields(&reader, true); ok {
			conn.phase = clickHouseDead
			call.respond(clickHouseEventException, info)
		}
	}
}

type clickHouseWalk uint8

const (
	// clickHouseWalkMore는 record를 끝까지 걸었고 다음 record가 packet 경계에서 이어진다는 뜻이다.
	clickHouseWalkMore clickHouseWalk = iota
	// clickHouseWalkDone은 질의가 끝나 응답 event를 냈다는 뜻이다.
	clickHouseWalkDone
	// clickHouseWalkLost는 구조는 맞지만 다음 byte가 캡처 밖이거나 크기를 알 수 없어 경계를 잃었다는 뜻이다.
	clickHouseWalkLost
	clickHouseWalkBad
)

// flowResponse는 질의가 끝나기 전의 응답 방향 record를 packet 단위로 걷는다. 경계를 모르면 Exception이나 마지막 write의
// 모양을 찾는다.
func (call *clickHouseCall) flowResponse() {
	conn, record := call.conn, call.record
	flow := &conn.flow
	data := record.payload
	pos := 0
	switch {
	case !flow.synced && len(data) == 0:
		return
	case !flow.synced && record.size == 1 && data[0] == clickHouseServerEndOfStream:
		call.finish(false)
		return
	case !flow.synced && data[0] == clickHouseServerException:
		reader := clickHouseReader{data: data, size: record.size, pos: 1, crossing: true}
		if info, ok := call.exceptionFields(&reader, true); ok {
			call.respond(clickHouseEventException, info)
		}
		return
	case !flow.synced:
		if !clickHouseFinalStart(data[0]) {
			return
		}
		*flow = clickHouseFlow{synced: true, rows: flow.rows}
	case flow.skip > 0:
		pos = min(flow.skip, record.size)
		flow.skip -= pos
		if flow.skip > 0 {
			return
		}
	}
	// 마지막 write는 packet 경계에서 시작하고 ProfileInfo, Totals, Extremes 중 하나로 시작한다.
	candidate := pos == 0 && !flow.frame && len(data) > 0 && clickHouseFinalStart(data[0])
	switch call.walk(pos) {
	case clickHouseWalkLost:
		// 마지막 write의 EndOfStream이 캡처 밖이다. 구조 검사를 통과한 packet이 둘 이상일 때만 끝났다고 본다.
		if candidate && call.parsed >= 2 {
			call.finish(true)
			return
		}
		*flow = clickHouseFlow{rows: flow.rows}
	case clickHouseWalkBad:
		*flow = clickHouseFlow{rows: flow.rows}
	}
}

func clickHouseFinalStart(packet byte) bool {
	return packet == clickHouseServerProfileInfo || packet == clickHouseServerTotals || packet == clickHouseServerExtremes
}

// finish는 EndOfStream으로 질의가 끝났음을 알린다. inferred는 EndOfStream을 보지 못하고 마지막 write의 모양으로 정했다는 뜻이다.
func (call *clickHouseCall) finish(inferred bool) {
	info := traceClickHouseEvent{Rows: call.conn.flow.rows, EndInferred: inferred}
	call.respond(clickHouseEventEnd, info)
}

// walk는 record의 pos부터 packet을 하나씩 걷는다. packet이 record보다 크면 남은 길이를 flow에 두고 다음 record에서 건너뛴다.
func (call *clickHouseCall) walk(pos int) clickHouseWalk {
	conn, record := call.conn, call.record
	flow := &conn.flow
	data, size := record.payload, record.size
	call.parsed = 0
	for pos < size {
		if pos >= len(data) {
			return clickHouseWalkLost
		}
		reader := clickHouseReader{data: data, size: size, pos: pos, crossing: true}
		if flow.frame {
			if result := call.frame(&reader); result != clickHouseWalkMore {
				return result
			}
		} else if result := call.packet(&reader); result != clickHouseWalkMore {
			return result
		}
		pos = reader.pos
		if pos > size {
			flow.skip, pos = pos-size, size
		}
	}
	return clickHouseWalkMore
}

// frame은 압축 frame 하나의 header를 읽고 frame 전체를 건너뛴다. 압축 전 크기가 꽉 찬 frame 뒤에는 같은 block의 frame이 이어진다.
func (call *clickHouseCall) frame(reader *clickHouseReader) clickHouseWalk {
	header := reader.take(clickHouseFrameHeader)
	if reader.err != clickHouseOK {
		return clickHouseWalkLost
	}
	method := header[16]
	compressed, plain := binary.LittleEndian.Uint32(header[17:]), binary.LittleEndian.Uint32(header[21:])
	valid := (method == clickHouseMethodNone || method == clickHouseMethodLZ4 || method == clickHouseMethodZSTD) &&
		compressed >= 9 && compressed <= clickHouseFrameLimit && plain <= clickHouseFrameSize &&
		(method != clickHouseMethodNone || compressed == plain+9)
	if !valid {
		return clickHouseWalkBad
	}
	call.conn.flow.frame = plain == clickHouseFrameSize
	reader.pos += int(compressed) - 9
	return clickHouseWalkMore
}

// packet은 응답 방향 packet 하나를 읽는다.
func (call *clickHouseCall) packet(reader *clickHouseReader) clickHouseWalk {
	conn := call.conn
	flow, revision := &conn.flow, conn.revision
	packet := reader.varuint()
	if reader.err != clickHouseOK {
		return clickHouseWalkLost
	}
	switch packet {
	case clickHouseServerEndOfStream:
		call.finish(false)
		return clickHouseWalkDone
	case clickHouseServerException:
		info, ok := call.exceptionFields(reader, false)
		if !ok {
			return clickHouseWalkLost
		}
		call.respond(clickHouseEventException, info)
		return clickHouseWalkDone
	case clickHouseServerPong:
	case clickHouseServerProgress:
		count := 3
		if revision >= clickHouseRevProgressBytes {
			count++
		}
		if revision >= clickHouseRevProgressWritten {
			count += 2
		}
		if revision >= clickHouseRevProgressElapsed {
			count++
		}
		for range count {
			reader.varuint()
		}
	case clickHouseServerProfileInfo:
		rows := reader.varuint()
		reader.varuint()
		reader.varuint()
		call.boolean(reader)
		reader.varuint()
		call.boolean(reader)
		if revision >= clickHouseRevProfileAggregate {
			call.boolean(reader)
			reader.varuint()
		}
		if reader.err == clickHouseOK {
			flow.rows = &rows
		}
	case clickHouseServerData, clickHouseServerTotals, clickHouseServerExtremes, clickHouseServerLog, clickHouseServerProfileEvents:
		reader.str()
		compressed := conn.compressed
		if packet == clickHouseServerLog || packet == clickHouseServerProfileEvents {
			compressed = compressed && revision >= clickHouseRevCompressedLog
		}
		switch {
		case reader.err != clickHouseOK:
		case compressed:
			flow.frame = true
		default:
			call.block(reader)
		}
	case clickHouseServerTableColumns:
		if conn.compressed && revision >= clickHouseRevCompressedLog {
			flow.frame = true
		} else {
			reader.str()
			reader.str()
		}
	case clickHouseServerTablesStatus, clickHouseServerPartUUIDs, clickHouseServerReadTask, clickHouseServerTimezone, clickHouseServerSSHChallenge:
		// 본문 구조를 확인하지 못한 packet이라 크기를 모른다.
		return clickHouseWalkLost
	default:
		return clickHouseWalkBad
	}
	switch reader.err {
	case clickHouseShort, clickHouseUnsupported:
		return clickHouseWalkLost
	case clickHouseBad:
		return clickHouseWalkBad
	}
	call.parsed++
	return clickHouseWalkMore
}

func (call *clickHouseCall) boolean(reader *clickHouseReader) {
	if reader.u8() > 1 {
		reader.fail(clickHouseBad)
	}
}

// block은 압축하지 않은 Native block을 읽고 reader의 위치를 block 끝으로 옮긴다. block 안은 총 길이 필드가 없어서 column마다
// type을 알아야 건너뛸 수 있다. 행이 있는 column은 크기를 아는 type만 건너뛴다.
func (call *clickHouseCall) block(reader *clickHouseReader) {
	revision := call.conn.revision
	for reader.err == clickHouseOK {
		field := reader.varuint()
		if field == 0 {
			break
		}
		switch {
		case field == 1:
			reader.u8()
		case field == 2:
			reader.skip(4)
		case field == 3 && revision >= clickHouseRevBucketVector:
			count := reader.varuint()
			if count > clickHouseVectorLimit {
				reader.fail(clickHouseBad)
			}
			reader.skip(4 * count)
		default:
			reader.fail(clickHouseBad)
		}
	}
	columns, rows := reader.varuint(), reader.varuint()
	if reader.err == clickHouseOK && (columns > clickHouseColumnLimit || rows > clickHouseRowLimit) {
		reader.fail(clickHouseBad)
	}
	for range columns {
		if reader.err != clickHouseOK {
			return
		}
		reader.str()
		kind := reader.str()
		if revision >= clickHouseRevCustomColumn && reader.u8() != 0 {
			reader.fail(clickHouseUnsupported)
		}
		if rows > 0 {
			call.columnData(reader, string(kind), rows)
		}
	}
}

// columnData는 행이 있는 column의 데이터를 건너뛴다.
func (call *clickHouseCall) columnData(reader *clickHouseReader, kind string, rows uint64) {
	if reader.err != clickHouseOK {
		return
	}
	if width := clickHouseFixedWidth(kind); width > 0 {
		reader.skip(rows * uint64(width))
		return
	}
	if kind != "String" || call.conn.revision < clickHouseRevStringWithSize {
		// 이 revision의 String 배치는 확인하지 못했다. 행마다 길이를 읽어야 해서 건너뛰지 않는다.
		reader.fail(clickHouseUnsupported)
		return
	}
	// 새 배치는 행마다 끝 offset을 UInt64로 먼저 쓰고 문자를 잇는다. 마지막 offset이 문자 전체의 길이다.
	last := reader.pos + 8*int(rows-1)
	if last+8 > len(reader.data) {
		reader.fail(clickHouseShort)
		return
	}
	total := binary.LittleEndian.Uint64(reader.data[last:])
	if total > 1<<40 {
		reader.fail(clickHouseBad)
		return
	}
	reader.skip(8*rows + total)
}

// clickHouseFixedWidth는 행마다 크기가 같은 type의 byte 수다. 모르는 type이면 0이다.
func clickHouseFixedWidth(kind string) int {
	switch kind {
	case "UInt8", "Int8":
		return 1
	case "UInt16", "Int16":
		return 2
	case "UInt32", "Int32", "DateTime":
		return 4
	case "UInt64", "Int64":
		return 8
	}
	switch {
	case strings.HasPrefix(kind, "DateTime('"):
		return 4
	case strings.HasPrefix(kind, "Enum8("):
		return 1
	case strings.HasPrefix(kind, "Enum16("):
		return 2
	}
	return 0
}

// exceptionFields는 Exception packet의 본문을 읽는다. 앞 packet을 걷지 않은 흐름에서는 strict로 이름과 끝 위치를 확인해
// Exception이 아닌 byte를 잘못 읽지 않게 한다.
func (call *clickHouseCall) exceptionFields(reader *clickHouseReader, strict bool) (traceClickHouseEvent, bool) {
	code := reader.take(4)
	if code == nil {
		return traceClickHouseEvent{}, false
	}
	name, nameComplete := reader.partial()
	message, _ := reader.partial()
	if reader.err == clickHouseBad {
		return traceClickHouseEvent{}, false
	}
	info := traceClickHouseEvent{ErrorCode: int32(binary.LittleEndian.Uint32(code)), ErrorName: clickHouseText(name, clickHouseTextLimit), Message: clickHouseText(message, clickHouseMessageLimit)}
	if !strict {
		return info, true
	}
	valid := info.ErrorCode > 0 && info.ErrorCode <= clickHouseErrorCodeLimit && nameComplete && len(name) > 0 && len(name) <= 128 && clickHousePrintable(name) && reader.pos <= reader.size
	if valid {
		// stack trace와 중첩 여부까지 캡처 안이면 Exception은 write의 마지막 packet이어야 한다.
		reader.str()
		nested := reader.u8()
		switch reader.err {
		case clickHouseOK:
			valid = nested <= 1 && reader.pos == reader.size
		case clickHouseShort:
		default:
			valid = false
		}
	}
	return info, valid
}

// clickHouseLineEnd는 i부터 줄 끝까지의 위치다.
func clickHouseLineEnd(sql []byte, i int) int {
	if end := bytes.IndexByte(sql[i:], '\n'); end >= 0 {
		return i + end
	}
	return len(sql)
}

// clickHouseCommentEnd는 i에서 주석이 시작하면 그 끝을, 아니면 i를 돌려준다. --와 //는 줄 끝까지, #은 뒤에 공백이나 !가
// 올 때만 줄 끝까지, /* */는 중첩할 수 있다. 닫히지 않은 주석은 끝까지다.
func clickHouseCommentEnd(sql []byte, i int) int {
	next := byte(0)
	if i+1 < len(sql) {
		next = sql[i+1]
	}
	switch {
	case sql[i] == '-' && next == '-', sql[i] == '/' && next == '/':
		return clickHouseLineEnd(sql, i)
	case sql[i] == '#' && i+1 < len(sql) && (next <= ' ' || next == '!'):
		return clickHouseLineEnd(sql, i)
	case sql[i] == '/' && next == '*':
		for depth, j := 1, i+2; j < len(sql); j++ {
			switch {
			case sql[j] == '/' && j+1 < len(sql) && sql[j+1] == '*':
				depth, j = depth+1, j+1
			case sql[j] == '*' && j+1 < len(sql) && sql[j+1] == '/':
				if depth--; depth == 0 {
					return j + 2
				}
				j++
			}
		}
		return len(sql)
	}
	return i
}

// clickHouseUnicodeQuote는 i가 U+2018, U+2019, U+201C, U+201D 중 하나의 시작이면 어느 것인지(마지막 byte)를 돌려준다.
func clickHouseUnicodeQuote(sql []byte, i int) byte {
	if i+2 < len(sql) && sql[i] == 0xe2 && sql[i+1] == 0x80 && sql[i+2] >= 0x98 && sql[i+2] <= 0x9d && sql[i+2] != 0x9a && sql[i+2] != 0x9b {
		return sql[i+2]
	}
	return 0
}

// clickHouseUnicodeEnd는 i에서 시작하는 유니코드 따옴표 literal의 끝과 닫혔는지를 돌려준다. 여는 따옴표가 U+2018이면
// U+2019가, U+201C이면 U+201D가 닫는다.
func clickHouseUnicodeEnd(sql []byte, i int) (int, bool) {
	closer := sql[i+2] + 1
	for i += 3; i < len(sql); i++ {
		switch {
		case sql[i] == '\\':
			i++
		case i+2 < len(sql) && sql[i] == 0xe2 && sql[i+1] == 0x80 && sql[i+2] == closer:
			return i + 3, true
		}
	}
	return len(sql), false
}

// clickHouseHeredoc는 i가 $tag$ 또는 $$로 시작하면 여는 표지의 길이를 돌려준다. 식별자 안의 $는 표지가 아니다.
func clickHouseHeredoc(sql []byte, i int) int {
	if sql[i] != '$' || (i > 0 && mysqlIdentifierByte(sql[i-1])) {
		return 0
	}
	end := i + 1
	for end < len(sql) && (sql[end] == '_' || (sql[end] >= 'a' && sql[end] <= 'z') || (sql[end] >= 'A' && sql[end] <= 'Z') || (end > i+1 && sql[end] >= '0' && sql[end] <= '9')) {
		end++
	}
	if end < len(sql) && sql[end] == '$' {
		return end + 1 - i
	}
	return 0
}

// clickHouseHeredocEnd는 표지가 tag인 heredoc의 내용이 끝나는 위치(닫는 표지 시작)와 닫혔는지다.
func clickHouseHeredocEnd(sql []byte, start int, tag []byte) (int, bool) {
	if end := bytes.Index(sql[start:], tag); end >= 0 {
		return start + end, true
	}
	return len(sql), false
}

// clickHouseMaskSQL은 문자열 literal 안을 ?로 바꾼다. '...', x'...', b'...', 유니코드 따옴표, $tag$...$tag$가 literal이고
// 닫히지 않은 literal은 끝까지 가린다. 큰따옴표와 backtick identifier, 주석 안의 따옴표는 literal을 열지 않는다.
func clickHouseMaskSQL(sql []byte) []byte {
	out := make([]byte, 0, len(sql))
	for i := 0; i < len(sql); {
		switch quote := clickHouseUnicodeQuote(sql, i); {
		case sql[i] == '\'':
			end, closed := mysqlLiteralEnd(sql, i)
			out = append(out, '\'', '?')
			if closed {
				out = append(out, '\'')
			}
			i = end
		case sql[i] == '"' || sql[i] == '`':
			end, _ := mysqlLiteralEnd(sql, i)
			out = append(out, sql[i:end]...)
			i = end
		case quote == 0x98:
			end, closed := clickHouseUnicodeEnd(sql, i)
			out = append(out, "‘?"...)
			if closed {
				out = append(out, "’"...)
			}
			i = end
		case quote == 0x9c:
			end, _ := clickHouseUnicodeEnd(sql, i)
			out = append(out, sql[i:end]...)
			i = end
		default:
			if width := clickHouseHeredoc(sql, i); width > 0 {
				tag := sql[i : i+width]
				end, closed := clickHouseHeredocEnd(sql, i+width, tag)
				out = append(out, tag...)
				out = append(out, '?')
				if closed {
					out = append(out, tag...)
					end += width
				}
				i = end
				continue
			}
			if end := clickHouseCommentEnd(sql, i); end > i {
				out = append(out, sql[i:end]...)
				i = end
				continue
			}
			out = append(out, sql[i])
			i++
		}
	}
	return out
}

// clickHouseNumberEnd는 i에서 시작하는 숫자 token의 끝이다. 0x나 0b로 시작하는 정수와 소수, 지수를 읽고, 식별자 byte가
// 이어지면 숫자가 아니라 식별자라서 0을 돌려준다.
func clickHouseNumberEnd(text []byte, i int) int {
	end := i
	if text[i] == '0' && i+2 < len(text) && (text[i+1] == 'x' || text[i+1] == 'X' || text[i+1] == 'b' || text[i+1] == 'B') && clickHouseHexDigit(text[i+2]) {
		for end = i + 2; end < len(text) && clickHouseHexDigit(text[end]); end++ {
		}
	} else {
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if end+1 < len(text) && text[end] == '.' && text[end+1] >= '0' && text[end+1] <= '9' {
			for end++; end < len(text) && text[end] >= '0' && text[end] <= '9'; end++ {
			}
		}
		if end+1 < len(text) && (text[end] == 'e' || text[end] == 'E') {
			exponent := end + 1
			if text[exponent] == '+' || text[exponent] == '-' {
				exponent++
			}
			if exponent < len(text) && text[exponent] >= '0' && text[exponent] <= '9' {
				for end = exponent; end < len(text) && text[end] >= '0' && text[end] <= '9'; end++ {
				}
			}
		}
	}
	if end < len(text) && mysqlIdentifierByte(text[end]) {
		return 0
	}
	return end
}

func clickHouseHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

// traceClickHouseShape는 SQL에서 값을 뺀 모양이다. 문자열과 숫자 literal을 ?로, 연속 공백을 한 칸으로 바꾼다. identifier 안의
// 숫자는 그대로 둔다. 가린 SQL과 원문이 같은 shape를 만든다.
func traceClickHouseShape(sql string) string {
	text := []byte(sql)
	var out strings.Builder
	space := false
	for i := 0; i < len(text); {
		value := text[i]
		if value == ' ' || value == '\t' || value == '\n' || value == '\r' {
			space = true
			i++
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		switch quote := clickHouseUnicodeQuote(text, i); {
		case value == '\'':
			end, _ := mysqlLiteralEnd(text, i)
			out.WriteByte('?')
			i = end
		case value == '"' || value == '`':
			end, _ := mysqlLiteralEnd(text, i)
			out.Write(text[i:end])
			i = end
		case quote == 0x98:
			end, _ := clickHouseUnicodeEnd(text, i)
			out.WriteByte('?')
			i = end
		case quote == 0x9c:
			end, _ := clickHouseUnicodeEnd(text, i)
			out.Write(text[i:end])
			i = end
		case value >= '0' && value <= '9':
			if end := clickHouseNumberEnd(text, i); end > 0 {
				out.WriteByte('?')
				i = end
				continue
			}
			end := i
			for ; end < len(text) && mysqlIdentifierByte(text[end]); end++ {
			}
			out.Write(text[i:end])
			i = end
		case mysqlIdentifierByte(value) && value != '$':
			end := i
			for end < len(text) && mysqlIdentifierByte(text[end]) {
				end++
			}
			out.Write(text[i:end])
			i = end
		default:
			if width := clickHouseHeredoc(text, i); width > 0 {
				end, closed := clickHouseHeredocEnd(text, i+width, text[i:i+width])
				out.WriteByte('?')
				if closed {
					end += width
				}
				i = end
				continue
			}
			if end := clickHouseCommentEnd(text, i); end > i {
				out.Write(text[i:end])
				i = end
				continue
			}
			out.WriteByte(value)
			i++
		}
	}
	shape := out.String()
	if len(shape) > clickHouseShapeLimit {
		shape = traceTrimText(shape, clickHouseShapeLimit) + "..."
	}
	return shape
}

// clickHouseScrollLabelSQL은 스크롤 행에 쓰는 SQL의 길이 상한이다. 전체는 --raw와 상세 보기에 있다.
const clickHouseScrollLabelSQL = 200

func clickHouseResponseEvent(name string) bool {
	return name == clickHouseEventHello || name == clickHouseEventEnd || name == clickHouseEventException
}

// traceClickHouseCounts는 ClickHouse group 행과 요약이 함께 쓰는 값이다. 연결 응답(clickhouse_hello)은 질의 통계에 넣지
// 않는다. Compressed connections는 압축 질의를 한 번이라도 보낸 연결의 수다.
type traceClickHouseCounts struct {
	Queries               uint64   `json:"queries"`
	Responses             uint64   `json:"responses"`
	Errors                uint64   `json:"errors"`
	Unanswered            uint64   `json:"unanswered"`
	LatencyAvgMS          *float64 `json:"latency_avg_ms"`
	LatencyMaxMS          *float64 `json:"latency_max_ms"`
	Connections           uint64   `json:"connections,omitempty"`
	CompressedConnections uint64   `json:"compressed_connections,omitempty"`
	ChunkedConnections    uint64   `json:"chunked_connections,omitempty"`
	answered              uint64
	latency               traceSpans
}

func (counts *traceClickHouseCounts) observe(event captureEvent) {
	info := event.ClickHouse
	switch event.Event {
	case clickHouseEventConnect:
		counts.Connections++
	case clickHouseEventChunked:
		counts.ChunkedConnections++
	case clickHouseEventQuery:
		counts.Queries++
		if info != nil && info.firstCompressed {
			counts.CompressedConnections++
		}
	case clickHouseEventEnd, clickHouseEventException:
		counts.Responses++
		if event.Event == clickHouseEventException {
			counts.Errors++
		}
		if info != nil && info.Command == clickHouseCommandQuery {
			counts.answered += event.answered
			counts.latency.observe(event.LatencyMS)
		}
	}
}

func (counts traceClickHouseCounts) finished() traceClickHouseCounts {
	counts.Unanswered = counts.Queries - min(counts.Queries, counts.answered)
	counts.LatencyAvgMS, counts.LatencyMaxMS = counts.latency.summary()
	return counts
}

func traceClickHouseGroupSummary(counts *traceClickHouseCounts) *traceClickHouseCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

// traceClickHouseGroupColumns는 group 행의 ClickHouse 열이다. counts는 group 행에서 ClickHouse 값을 꺼낸다.
func traceClickHouseGroupColumns(counts func(traceGroupSummary) *traceClickHouseCounts) []traceGroupColumn {
	column := func(screen, report string, width int, value func(traceClickHouseCounts) string) traceGroupColumn {
		return traceGroupColumn{screenTitle: screen, reportTitle: report, width: width, value: func(group traceGroupSummary) string {
			if values := counts(group); values != nil {
				return value(*values)
			}
			return "-"
		}}
	}
	return []traceGroupColumn{
		column("CMD", "QUERIES", 5, func(counts traceClickHouseCounts) string { return strconv.FormatUint(counts.Queries, 10) }),
		column("RSP", "RESPONSES", 5, func(counts traceClickHouseCounts) string { return strconv.FormatUint(counts.Responses, 10) }),
		column("ERR", "ERRORS", 4, func(counts traceClickHouseCounts) string { return strconv.FormatUint(counts.Errors, 10) }),
		column("NOANS", "UNANSWERED", 5, func(counts traceClickHouseCounts) string { return strconv.FormatUint(counts.Unanswered, 10) }),
		column("AVGms", "AVG_MS", 6, func(counts traceClickHouseCounts) string { return traceLatency(counts.LatencyAvgMS, "") }),
		column("MAXms", "MAX_MS", 6, func(counts traceClickHouseCounts) string { return traceLatency(counts.LatencyMaxMS, "") }),
	}
}

// traceClickHouseLine은 줄바꿈과 tab을 공백으로 바꿔 한 줄로 만든다.
func traceClickHouseLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(text)
}

// traceClickHouseCommandText는 event가 어떤 명령인지 한 줄로 쓴다. 응답 event도 답한 요청의 명령을 쓴다.
func traceClickHouseCommandText(event captureEvent) string {
	info := event.ClickHouse
	if info.Command == clickHouseCommandConnect {
		return emptyAs(strings.Join(slices.DeleteFunc([]string{info.User, info.Database, info.ClientName}, func(value string) bool { return value == "" }), " "), clickHouseCommandConnect)
	}
	sql := traceClickHouseLine(traceTrimText(info.SQL, clickHouseScrollLabelSQL))
	if len(info.SQL) > clickHouseScrollLabelSQL {
		sql += "..."
	}
	return emptyAs(sql, info.Command)
}

// traceClickHouseResultText는 응답 event의 결과를 쓴다.
func traceClickHouseResultText(event captureEvent) string {
	info := event.ClickHouse
	switch event.Event {
	case clickHouseEventException:
		if info.Interface == clickHouseInterfaceHTTP {
			return strings.TrimSpace(fmt.Sprintf("%d HTTP %d %s", info.ErrorCode, info.HTTPStatus, traceClickHouseLine(info.Message)))
		}
		return strings.TrimSpace(fmt.Sprintf("%d (%s) %s", info.ErrorCode, emptyAs(info.ErrorName, "-"), traceClickHouseLine(info.Message)))
	case clickHouseEventHello:
		return strings.TrimSpace("hello " + info.ServerName + " " + info.ServerVersion)
	}
	result := "ok"
	if info.Interface == clickHouseInterfaceHTTP {
		result += fmt.Sprintf(" %d", info.HTTPStatus)
		if info.Rows != nil {
			result += fmt.Sprintf(" %d rows", *info.Rows)
		}
	} else if info.Rows != nil {
		result = fmt.Sprintf("%d rows", *info.Rows)
	}
	if info.EndInferred {
		result += " (inferred)"
	}
	return result
}

// traceClickHouseScrollLabels는 목적지 칸에 쪽, 명령, 응답이면 결과를, event 칸에 event 이름과 응답 시간을 쓴다. 한 목록에
// 두 쪽이 섞이므로 쪽을 앞에 붙인다. 글자로 붙여서 전체 화면의 / 필터로 server나 client를 찾을 수 있다.
func traceClickHouseScrollLabels(event captureEvent) (string, string) {
	label := event.Event
	if event.LatencyMS != nil {
		label += " " + traceLatency(event.LatencyMS, "ms")
	}
	if event.ClickHouse == nil {
		return emptyAs(event.Destination, "-"), label
	}
	text := traceClickHouseCommandText(event)
	switch {
	case clickHouseResponseEvent(event.Event):
		text = "↳ " + traceClickHouseResultText(event)
	case event.Event == clickHouseEventChunked:
		text = "chunked"
	}
	if event.ClickHouse.Interface == clickHouseInterfaceHTTP {
		text = "http: " + text
	}
	if event.Side != "" {
		text = event.Side + ": " + text
	}
	return text, label
}

type clickHouseTraceKey struct {
	side    string
	iface   string
	command string
	shape   string
}

type clickHouseTraceRow struct {
	Side      string   `json:"side,omitempty"`
	Interface string   `json:"interface"`
	Command   string   `json:"command"`
	Shape     string   `json:"shape"`
	Processes []string `json:"processes,omitempty"`
	traceClickHouseCounts
}

type clickHouseTraceReport struct {
	Side       string               `json:"side,omitempty"`
	DurationMS int64                `json:"duration_ms"`
	LostEvents uint64               `json:"lost_events"`
	Statements []clickHouseTraceRow `json:"statements"`
	traceClickHouseCounts
	// Client와 Server는 두 쪽이 섞인 trace의 쪽별 합계다. 두 쪽의 응답 시간은 뜻이 달라 합친 평균은 뜻이 없다.
	Client *traceClickHouseCounts `json:"client,omitempty"`
	Server *traceClickHouseCounts `json:"server,omitempty"`
}

type clickHouseTraceRowStats struct {
	processes map[string]struct{}
	counts    traceClickHouseCounts
}

// clickHouseTraceSummarizer는 --group-by 없이 끝난 ClickHouse trace를 쪽, 명령, SQL shape마다 한 행으로 묶는다.
type clickHouseTraceSummarizer struct {
	rows   map[clickHouseTraceKey]*clickHouseTraceRowStats
	counts traceClickHouseCounts
	sides  map[string]*traceClickHouseCounts
}

func newClickHouseTraceSummarizer() *clickHouseTraceSummarizer {
	return &clickHouseTraceSummarizer{rows: map[clickHouseTraceKey]*clickHouseTraceRowStats{}, sides: map[string]*traceClickHouseCounts{}}
}

// clickHouseTraceShape는 요약 행의 SQL 자리다. SQL이 없는 명령은 그 사정을 쓴다.
func clickHouseTraceShape(info *traceClickHouseEvent) string {
	switch {
	case info.Command == clickHouseCommandConnect:
		return "(connect)"
	case info.SQL == "":
		return "(no SQL)"
	}
	return traceClickHouseLine(traceClickHouseShape(info.SQL))
}

func (summarizer *clickHouseTraceSummarizer) observe(event captureEvent) {
	if event.ClickHouse == nil {
		return
	}
	summarizer.counts.observe(event)
	if event.Side != "" {
		counts := summarizer.sides[event.Side]
		if counts == nil {
			counts = &traceClickHouseCounts{}
			summarizer.sides[event.Side] = counts
		}
		counts.observe(event)
	}
	key := clickHouseTraceKey{side: event.Side, iface: event.ClickHouse.Interface, command: event.ClickHouse.Command, shape: clickHouseTraceShape(event.ClickHouse)}
	stats := summarizer.rows[key]
	if stats == nil {
		stats = &clickHouseTraceRowStats{processes: map[string]struct{}{}}
		summarizer.rows[key] = stats
	}
	if event.Process != "" {
		stats.processes[event.Process] = struct{}{}
	}
	stats.counts.observe(event)
}

func (summarizer *clickHouseTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := clickHouseTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, traceClickHouseCounts: summarizer.counts.finished()}
	if len(summarizer.sides) == 1 {
		for side := range summarizer.sides {
			report.Side = side
		}
	} else if len(summarizer.sides) > 1 {
		report.Client, report.Server = traceClickHouseGroupSummary(summarizer.sides[traceClientSide]), traceClickHouseGroupSummary(summarizer.sides[traceServerSide])
	}
	report.Statements = make([]clickHouseTraceRow, 0, len(summarizer.rows))
	for key, stats := range summarizer.rows {
		report.Statements = append(report.Statements, clickHouseTraceRow{Side: key.side, Interface: key.iface, Command: key.command, Shape: key.shape, Processes: slices.Sorted(maps.Keys(stats.processes)), traceClickHouseCounts: stats.counts.finished()})
	}
	sort.Slice(report.Statements, func(i, j int) bool {
		left, right := report.Statements[i], report.Statements[j]
		switch {
		case left.Queries != right.Queries:
			return left.Queries > right.Queries
		case left.Connections != right.Connections:
			return left.Connections > right.Connections
		case left.Shape != right.Shape:
			return left.Shape < right.Shape
		case left.Command != right.Command:
			return left.Command < right.Command
		case left.Side != right.Side:
			return left.Side < right.Side
		}
		return left.Interface < right.Interface
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. ClickHouse 요약은 SQL 모양마다 한 행이라 같은 표를 쓴다.
func (report clickHouseTraceReport) print(bool) {
	title := "ClickHouse trace"
	if report.Side == traceServerSide {
		title = "ClickHouse server trace"
	}
	out := os.Stdout
	style := newTraceSummaryStyle(out)
	style.line(out, style.title(title, (time.Duration(report.DurationMS)*time.Millisecond).String()))
	io.WriteString(out, "\n")
	mixed := report.Client != nil && report.Server != nil
	if mixed {
		for _, side := range []struct {
			heading string
			counts  *traceClickHouseCounts
		}{{"Client side (queries that this host sent):", report.Client}, {"Server side (queries that local servers received):", report.Server}} {
			style.line(out, style.subheading(side.heading))
			printClickHouseCounts(style, "  ", *side.counts)
			io.WriteString(out, "\n")
		}
	} else {
		printClickHouseCounts(style, "", report.traceClickHouseCounts)
	}
	style.fields(out, "", "Lost events", fmt.Sprint(report.LostEvents))
	if len(report.Statements) == 0 {
		return
	}
	interfaces := map[string]struct{}{}
	for _, row := range report.Statements {
		interfaces[row.Interface] = struct{}{}
	}
	mixedInterfaces := len(interfaces) > 1
	leading := func(side, iface string) []string {
		var cells []string
		if mixed {
			cells = append(cells, side)
		}
		if mixedInterfaces {
			cells = append(cells, iface)
		}
		return cells
	}
	table := traceTable{titles: append(leading("SIDE", "INTERFACE"), "COMMAND", "SQL", "COUNT", "ERRORS", "UNANSWERED", "AVG", "MAX", "PROCESS")}
	for _, row := range report.Statements {
		count := row.Queries
		if row.Command == clickHouseCommandConnect {
			count = row.Connections
		}
		table.add(append(leading(emptyAs(row.Side, "-"), row.Interface), row.Command, row.Shape, fmt.Sprint(count), fmt.Sprint(row.Errors), fmt.Sprint(row.Unanswered), traceLatency(row.LatencyAvgMS, "ms"), traceLatency(row.LatencyMaxMS, "ms"), emptyAs(strings.Join(row.Processes, ","), "-"))...)
	}
	io.WriteString(out, "\n")
	style.writeTable(out, table)
}

func printClickHouseCounts(style traceSummaryStyle, indent string, counts traceClickHouseCounts) {
	style.fields(os.Stdout, indent,
		"Queries", fmt.Sprint(counts.Queries),
		"Responses", fmt.Sprint(counts.Responses),
		"Errors", fmt.Sprint(counts.Errors),
		"Unanswered", fmt.Sprint(counts.Unanswered),
		"Latency avg", traceLatency(counts.LatencyAvgMS, "ms"),
		"Latency max", traceLatency(counts.LatencyMaxMS, "ms"),
		"Connections", fmt.Sprint(counts.Connections),
		"Compressed connections", fmt.Sprint(counts.CompressedConnections),
		"Chunked connections", fmt.Sprint(counts.ChunkedConnections),
	)
}
