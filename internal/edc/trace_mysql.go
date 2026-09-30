package edc

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	traceMySQLDefaultPort = 3306
	mysqlEventPrefix      = "mysql_"
	// mysqlPacketKeep은 packet 하나에서 앞부분으로 보관하는 byte 수다. BPF가 요청 방향에서 넘기는 상한과 같다.
	mysqlPacketKeep = 4096
	// mysqlSQLLimit은 event와 저장한 prepared statement의 SQL 상한이다.
	mysqlSQLLimit = 1024
	// mysqlShapeLimit은 요약 행의 SQL shape 길이 상한이다.
	mysqlShapeLimit = 256
	// 아래 세 상한은 넘으면 모두 비운다. 비운 뒤 이어지는 응답은 짝 없이 사라지고, execute는 statement id만 보인다.
	mysqlSocketLimit    = 65536
	mysqlStatementLimit = 65536
	mysqlWaitingLimit   = 1024
	// mysqlMaxPacket은 packet 길이 필드의 최댓값이다. 이 길이면 다음 packet이 같은 message를 잇는다.
	mysqlMaxPacket = 0xffffff
	// mysqlColumnLimit은 result set의 column 수로 받아들이는 값의 상한이다. 그보다 크면 다른 packet으로 본다.
	mysqlColumnLimit = 4096
)

// capability flag 값은 dev.mysql.com의 Capabilities Flags 문서와 같다.
const (
	mysqlCapConnectWithDB    = 0x8
	mysqlCapCompress         = 0x20
	mysqlCapProtocol41       = 0x200
	mysqlCapSSL              = 0x800
	mysqlCapSecureConnection = 0x8000
	mysqlCapLenencClientData = 0x200000
	mysqlCapQueryAttributes  = 0x8000000
)

const (
	mysqlComQuit          = 0x01
	mysqlComQuery         = 0x03
	mysqlComStmtPrepare   = 0x16
	mysqlComStmtExecute   = 0x17
	mysqlComStmtSendLong  = 0x18
	mysqlComStmtClose     = 0x19
	mysqlEventConnect     = "mysql_connect"
	mysqlEventTLS         = "mysql_tls"
	mysqlEventOK          = "mysql_ok"
	mysqlEventError       = "mysql_error"
	mysqlEventResult      = "mysql_result"
	mysqlCommandConnect   = "connect"
	mysqlCommandQuery     = "query"
	mysqlCommandPrepare   = "prepare"
	mysqlCommandExecute   = "execute"
	mysqlMinGreetingBytes = 32
)

// mysqlKnownCommands는 재동기에서 packet 경계로 받는 명령 byte다. 바이너리 traffic을 명령으로 잘못 읽지 않도록 흔한 것만 둔다.
var mysqlKnownCommands = []byte{0x01, 0x02, 0x03, 0x04, 0x09, 0x0e, 0x11, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1f}

// traceMySQLEvent는 MySQL event에만 붙는 값이다. 응답 event에는 답한 요청의 command와 SQL을 함께 붙인다.
type traceMySQLEvent struct {
	Command       string  `json:"command,omitempty"`
	SQL           string  `json:"sql,omitempty"`
	SQLTruncated  bool    `json:"sql_truncated,omitempty"`
	StatementID   uint32  `json:"statement_id,omitempty"`
	AffectedRows  *uint64 `json:"affected_rows,omitempty"`
	Columns       uint64  `json:"columns,omitempty"`
	ErrorCode     uint16  `json:"error_code,omitempty"`
	SQLState      string  `json:"sql_state,omitempty"`
	Message       string  `json:"message,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	User          string  `json:"user,omitempty"`
	Database      string  `json:"database,omitempty"`
	TLS           bool    `json:"tls,omitempty"`
	Compressed    bool    `json:"compressed,omitempty"`
}

// mysqlRecord는 kernel이 TCP로 주고받은 읽기나 쓰기 하나의 앞부분이다. size는 그 읽기나 쓰기의 전체 byte 수이고,
// payload는 앞부분만 담으므로 size보다 짧을 수 있다. server는 로컬 port가 MySQL port인 쪽이다.
type mysqlRecord struct {
	bootTimeNS  uint64
	pid         uint32
	cgroupID    uint64
	process     string
	socket      uint64
	source      string
	destination string
	sent        bool
	server      bool
	size        int
	payload     []byte
}

// mysqlPacket은 3-byte 길이와 1-byte sequence id가 붙은 packet이다. payload는 앞부분이고 truncated는 그 뒤를 보지
// 못했다는 뜻이다. at은 packet의 첫 byte를 담은 읽기나 쓰기의 시각이다.
type mysqlPacket struct {
	seq       byte
	length    int
	payload   []byte
	truncated bool
	at        uint64
}

// mysqlStream은 한 방향의 byte 흐름에서 packet 경계를 따라간다. 읽기 하나에 packet이 여럿이거나, header와 payload가
// 다른 읽기로 나뉘거나, packet이 캡처한 앞부분보다 커도 size로 남은 길이를 건너뛰어 경계를 지킨다.
type mysqlStream struct {
	header    [4]byte
	headerLen int
	seq       byte
	length    int
	left      int
	held      []byte
	delivered bool
	at        uint64
	// full은 앞 packet이 최대 길이라 지금 packet이 같은 message를 잇는다는 뜻이다.
	full bool
}

func (stream *mysqlStream) reset() { *stream = mysqlStream{} }

// seed는 header를 따로 읽은 뒤 payload를 읽는 흐름의 시작이다. header 읽기는 이미 지나갔다.
func (stream *mysqlStream) seed(length int, at uint64) {
	stream.reset()
	stream.headerLen, stream.length, stream.left, stream.at = 4, length, length, at
}

// waiting은 packet을 마저 받으려고 byte를 쥐고 있는 상태다. 건너뛰는 중인 packet은 쥔 것이 없다.
func (stream *mysqlStream) waiting() bool {
	return (stream.headerLen > 0 && stream.headerLen < 4) || (stream.headerLen == 4 && !stream.delivered)
}

func mysqlLength(header []byte) int {
	return int(header[0]) | int(header[1])<<8 | int(header[2])<<16
}

// feed는 record 하나를 이어서 읽고 packet마다 deliver를 부른다. deliver가 false를 돌려주면 멈추고 흐름을 비운다.
// header가 캡처한 앞부분 밖이라 packet 경계를 잃으면 false를 돌려준다.
func (stream *mysqlStream) feed(data []byte, size int, at uint64, deliver func(mysqlPacket) bool) bool {
	size = max(size, len(data))
	pos, used := 0, 0
	for used < size {
		if stream.headerLen < 4 {
			if pos >= len(data) {
				stream.reset()
				return false
			}
			if stream.headerLen == 0 {
				stream.at = at
			}
			n := copy(stream.header[stream.headerLen:], data[pos:])
			stream.headerLen, pos, used = stream.headerLen+n, pos+n, used+n
			if stream.headerLen < 4 {
				continue
			}
			stream.full = stream.length == mysqlMaxPacket
			stream.length, stream.seq = mysqlLength(stream.header[:]), stream.header[3]
			stream.left, stream.held, stream.delivered = stream.length, stream.held[:0], false
			if stream.length == 0 {
				more := deliver(mysqlPacket{seq: stream.seq, at: stream.at})
				stream.headerLen = 0
				if !more {
					stream.reset()
					return true
				}
			}
			continue
		}
		n := min(stream.left, size-used)
		seen := max(0, min(n, len(data)-pos))
		if !stream.delivered {
			want := min(stream.length, mysqlPacketKeep)
			take := min(seen, want-len(stream.held))
			var payload []byte
			if len(stream.held) == 0 && take == want {
				payload = data[pos : pos+take]
			} else {
				stream.held = append(stream.held, data[pos:pos+take]...)
				payload = stream.held
			}
			if len(payload) == want || seen < n {
				stream.delivered = true
				if !deliver(mysqlPacket{seq: stream.seq, length: stream.length, payload: payload, truncated: len(payload) < stream.length, at: stream.at}) {
					stream.reset()
					return true
				}
			}
		}
		pos, used, stream.left = pos+seen, used+n, stream.left-n
		if stream.left == 0 {
			stream.headerLen, stream.delivered = 0, false
		}
	}
	return true
}

type mysqlPhase uint8

// 새 socket은 mysqlCommand이고 packet 경계를 모르는 상태로 시작한다. 연결 중간에 trace를 시작한 경우다.
const (
	mysqlCommand mysqlPhase = iota
	mysqlGreeting
	mysqlAuth
	// mysqlDead는 TLS, 압축, 인증 실패처럼 더 해석하지 않는 연결이다. 다음 greeting이 오면 새로 시작한다.
	mysqlDead
)

// mysqlBareHeader는 payload 없이 header만 읽은 record다. 다음 record가 그 payload인지 확인할 때 쓴다.
type mysqlBareHeader struct {
	length int
	seq    byte
	at     uint64
	ok     bool
}

type mysqlPending struct {
	at           uint64
	command      string
	sql          string
	sqlTruncated bool
	statementID  uint32
	// silent는 event 없이 첫 응답 packet만 소비하는 명령이다.
	silent bool
}

type mysqlStatement struct {
	sql       string
	truncated bool
}

type mysqlConn struct {
	phase          mysqlPhase
	synced         bool
	request        mysqlStream
	response       mysqlStream
	requestHeader  mysqlBareHeader
	responseHeader mysqlBareHeader
	version        string
	serverCaps     uint32
	serverKnown    bool
	clientCaps     uint32
	clientKnown    bool
	compressed     bool
	// lastSeq는 마지막 요청 packet의 sequence id다. 응답의 첫 packet은 이 값보다 1 크다.
	lastSeq        byte
	pending        *mysqlPending
	statements     map[uint32]mysqlStatement
	requestWaiting bool
	responseWait   bool
}

// attributes는 CLIENT_QUERY_ATTRIBUTES를 협상했는지다. handshake를 못 봤으면 known이 false다.
func (conn *mysqlConn) attributes() (on, known bool) {
	if !conn.clientKnown {
		return false, false
	}
	return conn.clientCaps&mysqlCapQueryAttributes != 0 && (!conn.serverKnown || conn.serverCaps&mysqlCapQueryAttributes != 0), true
}

// mysqlTracker는 socket마다 두 방향의 packet을 따라가며 명령과 응답을 짝짓는다. side는 tracker가 볼 쪽이고 비어 있으면
// 두 쪽을 모두 본다.
type mysqlTracker struct {
	side        string
	showSecrets bool
	conns       map[uint64]*mysqlConn
	waiting     int
	statements  int
}

func newMySQLTracker(side string, showSecrets bool) *mysqlTracker {
	return &mysqlTracker{side: side, showSecrets: showSecrets, conns: map[uint64]*mysqlConn{}}
}

// mysqlCall은 record 하나를 처리하는 동안의 값이다.
type mysqlCall struct {
	tracker *mysqlTracker
	conn    *mysqlConn
	record  mysqlRecord
	side    string
	offset  int64
	out     []captureEvent
}

func (tracker *mysqlTracker) events(record mysqlRecord, clockOffset int64) []captureEvent {
	side := traceClientSide
	if record.server {
		side = traceServerSide
	}
	if tracker.side != "" && side != tracker.side {
		return nil
	}
	conn := tracker.conns[record.socket]
	if conn == nil {
		if len(tracker.conns) >= mysqlSocketLimit {
			clear(tracker.conns)
			tracker.waiting, tracker.statements = 0, 0
		}
		conn = &mysqlConn{}
		tracker.conns[record.socket] = conn
	}
	call := &mysqlCall{tracker: tracker, conn: conn, record: record, side: side, offset: clockOffset}
	// client는 요청을 보내고 응답을 받는다. 서버는 요청을 받고 응답을 보낸다.
	if record.server != record.sent {
		call.request()
	} else {
		call.response()
	}
	tracker.account(conn)
	return call.out
}

// account는 packet을 기다리는 stream 수를 센다. 상한을 넘으면 모두 비운다. 요청 흐름은 경계를 다시 찾아야 한다.
func (tracker *mysqlTracker) account(conn *mysqlConn) {
	request, response := conn.request.waiting(), conn.response.waiting()
	tracker.waiting += mysqlCount(request) - mysqlCount(conn.requestWaiting) + mysqlCount(response) - mysqlCount(conn.responseWait)
	conn.requestWaiting, conn.responseWait = request, response
	if tracker.waiting <= mysqlWaitingLimit {
		return
	}
	for _, conn := range tracker.conns {
		conn.request.reset()
		conn.response.reset()
		conn.requestWaiting, conn.responseWait = false, false
		conn.synced = conn.synced && conn.phase != mysqlCommand
	}
	tracker.waiting = 0
}

func mysqlCount(on bool) int {
	if on {
		return 1
	}
	return 0
}

// forget은 같은 socket 주소로 새 연결이 시작될 때 앞 연결의 상태를 버린다.
func (tracker *mysqlTracker) forget(conn *mysqlConn) {
	tracker.statements -= len(conn.statements)
	tracker.waiting -= mysqlCount(conn.requestWaiting) + mysqlCount(conn.responseWait)
	*conn = mysqlConn{}
}

func mysqlKnownCommand(command byte) bool {
	return bytes.IndexByte(mysqlKnownCommands, command) >= 0
}

// mysqlCleanCommand는 record 하나가 seq 0의 명령 packet 하나와 정확히 같은지다. 부분 송신이나 잃은 record 뒤에도
// 경계를 되찾는 기준이다.
func mysqlCleanCommand(data []byte, size int) bool {
	return len(data) >= 5 && data[3] == 0 && mysqlKnownCommand(data[4]) && mysqlLength(data) >= 1 && 4+mysqlLength(data) == size
}

func mysqlPrintable(value byte) bool {
	return (value >= 0x20 && value < 0x7f) || value == '\t' || value == '\n' || value == '\r'
}

// mysqlLenenc는 length-encoded integer와 그 byte 수를 읽는다. 읽지 못하면 byte 수가 0이다.
func mysqlLenenc(data []byte) (uint64, int) {
	if len(data) == 0 {
		return 0, 0
	}
	width := 0
	switch data[0] {
	case 0xfc:
		width = 3
	case 0xfd:
		width = 4
	case 0xfe:
		width = 9
	default:
		if data[0] < 0xfb {
			return uint64(data[0]), 1
		}
		return 0, 0
	}
	if len(data) < width {
		return 0, 0
	}
	var value [8]byte
	copy(value[:], data[1:width])
	return binary.LittleEndian.Uint64(value[:]), width
}

// request는 요청 방향 record 하나를 읽는다.
func (call *mysqlCall) request() {
	conn, record := call.conn, call.record
	if conn.phase == mysqlDead {
		return
	}
	switch {
	case mysqlCleanCommand(record.payload, record.size):
		conn.phase, conn.synced = mysqlCommand, true
		conn.request.reset()
		conn.requestHeader = mysqlBareHeader{}
	case conn.phase == mysqlAuth:
		// 인증 중 client packet에는 password 재료가 있다. 읽지 않는다.
		return
	case conn.phase == mysqlCommand && !conn.synced && !call.resync():
		return
	}
	deliver := call.commandPacket
	if conn.phase == mysqlGreeting {
		deliver = call.handshakePacket
	}
	if !conn.request.feed(record.payload, record.size, record.bootTimeNS, deliver) {
		conn.synced = false
	}
}

// resync는 경계를 모르는 요청 방향 record가 명령의 시작인지 본다. 시작이면 흐름을 그 자리에 맞추고 true를 돌려준다.
func (call *mysqlCall) resync() bool {
	conn, record := call.conn, call.record
	data, size := record.payload, record.size
	bare := conn.requestHeader
	conn.requestHeader = mysqlBareHeader{}
	switch {
	case bare.ok && size == bare.length && len(data) > 0 && mysqlKnownCommand(data[0]):
		// header를 따로 읽은 뒤 payload를 읽는 흐름이다.
		conn.request.seed(bare.length, bare.at)
	case len(data) == 4 && size == 4 && data[3] == 0 && mysqlLength(data) >= 1:
		conn.requestHeader = mysqlBareHeader{length: mysqlLength(data), at: record.bootTimeNS, ok: true}
		return false
	case mysqlBigCommandStart(data, size):
		conn.request.reset()
	default:
		return false
	}
	conn.synced = true
	return true
}

// mysqlBigCommandStart는 record보다 큰 SQL 명령의 시작이다. 뒤 byte를 보지 못하므로 SQL 첫 byte가 출력 가능해야 한다.
func mysqlBigCommandStart(data []byte, size int) bool {
	if len(data) < 6 || data[3] != 0 || 4+mysqlLength(data) <= size || (data[4] != mysqlComQuery && data[4] != mysqlComStmtPrepare) {
		return false
	}
	return mysqlPrintable(data[5]) || (data[4] == mysqlComQuery && len(data) >= 8 && data[5] == 0 && data[6] == 1 && mysqlPrintable(data[7]))
}

func (call *mysqlCall) event(name string, at uint64, length int, info *traceMySQLEvent) captureEvent {
	record := call.record
	return captureEvent{
		SocketID: record.socket, TimestampNS: uint64(int64(at) + call.offset), BootTimeNS: at, Event: name, Protocol: "mysql", Side: call.side,
		PID: record.pid, Process: record.process, CgroupID: record.cgroupID, Source: record.source, Destination: record.destination, Bytes: uint64(length), MySQL: info,
	}
}

// sqlText는 event에 붙일 SQL이다. 문자열 literal은 --show-secrets가 아니면 가린다.
func (tracker *mysqlTracker) sqlText(raw []byte, truncated bool) (string, bool) {
	if !tracker.showSecrets {
		raw = mysqlMaskSQL(raw)
	}
	text := string(raw)
	if len(text) > mysqlSQLLimit {
		text, truncated = traceTrimText(text, mysqlSQLLimit), true
	}
	return traceEscapeText([]byte(text)), truncated
}

// commandPacket은 요청 방향 packet을 명령으로 읽는다.
func (call *mysqlCall) commandPacket(packet mysqlPacket) bool {
	conn := call.conn
	if conn.request.full {
		conn.lastSeq = packet.seq
		return true
	}
	if packet.seq != 0 || len(packet.payload) == 0 || !mysqlKnownCommand(packet.payload[0]) {
		conn.synced = false
		return false
	}
	command, payload := packet.payload[0], packet.payload
	conn.lastSeq, conn.pending = 0, nil
	conn.response.reset()
	var info traceMySQLEvent
	pending := &mysqlPending{at: packet.at}
	switch command {
	case mysqlComQuit, mysqlComStmtSendLong:
		return true
	case mysqlComStmtClose:
		if len(payload) >= 5 {
			id := binary.LittleEndian.Uint32(payload[1:5])
			if _, ok := conn.statements[id]; ok {
				delete(conn.statements, id)
				call.tracker.statements--
			}
		}
		return true
	case mysqlComQuery:
		info.Command = mysqlCommandQuery
		if start, ok := conn.querySQL(payload); ok {
			info.SQL, info.SQLTruncated = call.tracker.sqlText(payload[start:], packet.truncated)
		}
	case mysqlComStmtPrepare:
		info.Command = mysqlCommandPrepare
		info.SQL, info.SQLTruncated = call.tracker.sqlText(payload[1:], packet.truncated)
	case mysqlComStmtExecute:
		info.Command = mysqlCommandExecute
		if len(payload) >= 5 {
			info.StatementID = binary.LittleEndian.Uint32(payload[1:5])
			statement := conn.statements[info.StatementID]
			info.SQL, info.SQLTruncated = statement.sql, statement.truncated
		}
	default:
		pending.silent = true
		conn.pending = pending
		return true
	}
	pending.command, pending.sql, pending.sqlTruncated, pending.statementID = info.Command, info.SQL, info.SQLTruncated, info.StatementID
	conn.pending = pending
	call.out = append(call.out, call.event(mysqlEventPrefix+info.Command, packet.at, packet.length, &info))
	return true
}

// querySQL은 COM_QUERY payload에서 SQL의 시작 위치를 찾는다. query attribute가 있으면 parameter_count와
// parameter_set_count가 SQL 앞에 온다. 값이 있는 attribute는 해석하지 않으므로 SQL도 보이지 않는다.
func (conn *mysqlConn) querySQL(payload []byte) (int, bool) {
	on, known := conn.attributes()
	switch {
	case on:
		count, first := mysqlLenenc(payload[1:])
		_, second := mysqlLenenc(payload[min(1+first, len(payload)):])
		return 1 + first + second, first > 0 && second > 0 && count == 0
	case !known && len(payload) >= 3 && payload[1] == 0 && payload[2] == 1:
		return 3, true
	}
	return 1, true
}

// mysqlParseGreeting은 응답 방향 record가 server의 첫 packet(protocol 10)인지 본다. header를 따로 읽은 경우는 앞 record의
// header를 함께 본다.
func mysqlParseGreeting(record mysqlRecord, bare mysqlBareHeader) (version string, caps uint32, known, ok bool) {
	data := record.payload
	var payload []byte
	switch {
	case len(data) >= 5 && data[3] == 0 && record.size == 4+mysqlLength(data):
		payload = data[4:]
	case bare.ok && bare.seq == 0 && record.size == bare.length:
		payload = data
	}
	if len(payload) < mysqlMinGreetingBytes || payload[0] != 10 {
		return "", 0, false, false
	}
	end := bytes.IndexByte(payload[1:], 0)
	if end <= 0 || mysqlNotPrintable(payload[1:1+end]) {
		return "", 0, false, false
	}
	offset := 1 + end + 1 + 4 + 8 + 1
	if len(payload) >= offset+7 {
		caps, known = uint32(binary.LittleEndian.Uint16(payload[offset:]))|uint32(binary.LittleEndian.Uint16(payload[offset+5:]))<<16, true
		if caps&mysqlCapProtocol41 == 0 {
			return "", 0, false, false
		}
	}
	return string(payload[1 : 1+end]), caps, known, true
}

func mysqlNotPrintable(data []byte) bool {
	for _, value := range data {
		if value < 0x20 || value >= 0x7f {
			return true
		}
	}
	return false
}

// response는 응답 방향 record 하나를 읽는다.
func (call *mysqlCall) response() {
	conn, record := call.conn, call.record
	bare := conn.responseHeader
	conn.responseHeader = mysqlBareHeader{}
	if len(record.payload) == 4 && record.size == 4 {
		conn.responseHeader = mysqlBareHeader{length: mysqlLength(record.payload), seq: record.payload[3], at: record.bootTimeNS, ok: true}
	}
	// greeting은 새 연결의 시작이다. socket 주소를 다시 쓴 연결은 앞 상태를 버린다.
	if version, caps, known, ok := mysqlParseGreeting(record, bare); ok {
		call.tracker.forget(conn)
		conn.phase, conn.synced, conn.version, conn.serverCaps, conn.serverKnown = mysqlGreeting, true, version, caps, known
		return
	}
	switch {
	case conn.phase == mysqlAuth:
		conn.response.feed(record.payload, record.size, record.bootTimeNS, call.authPacket)
	case conn.phase == mysqlCommand && conn.pending != nil:
		conn.response.feed(record.payload, record.size, record.bootTimeNS, call.commandResponse)
	}
}

// handshakePacket은 HandshakeResponse41과 SSLRequest를 읽는다.
func (call *mysqlCall) handshakePacket(packet mysqlPacket) bool {
	conn := call.conn
	conn.phase = mysqlDead
	payload := packet.payload
	if packet.seq != 1 || packet.length < 32 || len(payload) < 32 {
		return false
	}
	caps := binary.LittleEndian.Uint32(payload)
	if caps&mysqlCapProtocol41 == 0 {
		return false
	}
	conn.clientCaps, conn.clientKnown = caps, true
	if caps&mysqlCapSSL != 0 && packet.length == 32 {
		info := traceMySQLEvent{TLS: true, ServerVersion: traceEscapeText([]byte(conn.version))}
		call.out = append(call.out, call.event(mysqlEventTLS, packet.at, packet.length, &info))
		return false
	}
	user, rest, ok := bytes.Cut(payload[32:], []byte{0})
	if !ok {
		return false
	}
	// auth 응답은 password에서 나온 값이라 길이만 읽고 건너뛴다. 어디에도 저장하지 않는다.
	switch {
	case caps&mysqlCapLenencClientData != 0:
		size, width := mysqlLenenc(rest)
		if width == 0 || size > uint64(len(rest)-width) {
			return false
		}
		rest = rest[width+int(size):]
	case caps&mysqlCapSecureConnection != 0:
		if len(rest) == 0 || int(rest[0]) > len(rest)-1 {
			return false
		}
		rest = rest[1+int(rest[0]):]
	default:
		_, rest, ok = bytes.Cut(rest, []byte{0})
		if !ok {
			return false
		}
	}
	var database []byte
	if caps&mysqlCapConnectWithDB != 0 {
		if database, _, ok = bytes.Cut(rest, []byte{0}); !ok {
			return false
		}
	}
	conn.compressed = caps&mysqlCapCompress != 0 && (!conn.serverKnown || conn.serverCaps&mysqlCapCompress != 0)
	info := traceMySQLEvent{Command: mysqlCommandConnect, User: traceEscapeText(user), Database: traceEscapeText(database), ServerVersion: traceEscapeText([]byte(conn.version)), Compressed: conn.compressed}
	call.out = append(call.out, call.event(mysqlEventConnect, packet.at, packet.length, &info))
	conn.pending = &mysqlPending{at: packet.at, command: mysqlCommandConnect}
	conn.phase = mysqlAuth
	conn.response.reset()
	return false
}

// authPacket은 인증 중 응답 방향 packet을 읽는다. auth switch(0xfe)와 more data(0x01)는 중간 packet이고, 마지막
// OK나 ERR가 mysql_connect의 응답이다.
func (call *mysqlCall) authPacket(packet mysqlPacket) bool {
	conn := call.conn
	if len(packet.payload) == 0 {
		return true
	}
	switch packet.payload[0] {
	case 0x00:
		if packet.length < 7 {
			return true
		}
		call.respond(packet, conn.pending, mysqlEventOK, &traceMySQLEvent{})
		conn.phase = mysqlCommand
		if conn.compressed {
			conn.phase = mysqlDead
		}
	case 0xff:
		info, ok := mysqlError(packet)
		if !ok {
			return true
		}
		call.respond(packet, conn.pending, mysqlEventError, &info)
		conn.phase = mysqlDead
	default:
		return true
	}
	conn.pending = nil
	conn.request.reset()
	conn.synced = true
	return false
}

// commandResponse는 명령 뒤 응답 방향의 첫 packet을 그 명령과 짝짓는다. 모양이 맞지 않으면 짝을 버린다.
func (call *mysqlCall) commandResponse(packet mysqlPacket) bool {
	conn := call.conn
	pending := conn.pending
	conn.pending = nil
	if pending == nil || pending.silent || packet.seq != conn.lastSeq+1 || len(packet.payload) == 0 {
		return false
	}
	payload := packet.payload
	switch {
	case payload[0] == 0x00 && packet.length >= 7:
		var info traceMySQLEvent
		if pending.command == mysqlCommandPrepare {
			if len(payload) < 5 {
				return false
			}
			info.StatementID = binary.LittleEndian.Uint32(payload[1:5])
			call.remember(info.StatementID, mysqlStatement{sql: pending.sql, truncated: pending.sqlTruncated})
		} else {
			rows, width := mysqlLenenc(payload[1:])
			if width == 0 || 1+width > packet.length {
				return false
			}
			info.AffectedRows = &rows
		}
		call.respond(packet, pending, mysqlEventOK, &info)
	case payload[0] == 0xff:
		if info, ok := mysqlError(packet); ok {
			call.respond(packet, pending, mysqlEventError, &info)
		}
	case payload[0] == 0xfe && packet.length < 9:
		call.respond(packet, pending, mysqlEventOK, &traceMySQLEvent{})
	default:
		columns, width := mysqlLenenc(payload)
		if width == packet.length && columns >= 1 && columns <= mysqlColumnLimit {
			call.respond(packet, pending, mysqlEventResult, &traceMySQLEvent{Columns: columns})
		}
	}
	return false
}

// respond는 응답 event를 만든다. 답한 요청이 있으면 그 명령과 SQL, 응답 시간을 붙인다.
func (call *mysqlCall) respond(packet mysqlPacket, pending *mysqlPending, name string, info *traceMySQLEvent) {
	event := call.event(name, packet.at, packet.length, info)
	if pending != nil {
		info.Command, info.SQL, info.SQLTruncated = pending.command, pending.sql, pending.sqlTruncated
		info.StatementID = cmp.Or(info.StatementID, pending.statementID)
		event.LatencyMS, event.answered = traceSpan(pending.at, packet.at), 1
	}
	call.out = append(call.out, event)
}

func mysqlError(packet mysqlPacket) (traceMySQLEvent, bool) {
	payload := packet.payload
	if packet.length < 3 || len(payload) < 3 {
		return traceMySQLEvent{}, false
	}
	info := traceMySQLEvent{ErrorCode: binary.LittleEndian.Uint16(payload[1:3])}
	message := payload[3:]
	// CLIENT_PROTOCOL_41이면 '#'과 SQLSTATE 5자가 온다. handshake를 못 봤어도 이 모양으로 알 수 있다.
	if len(payload) >= 9 && payload[3] == '#' && !mysqlNotPrintable(payload[4:9]) {
		info.SQLState, message = string(payload[4:9]), payload[9:]
	}
	info.Message = traceEscapeText(message)
	return info, true
}

// remember는 prepare한 statement의 SQL을 둔다. 모든 연결의 합계가 상한을 넘으면 모두 비운다.
func (call *mysqlCall) remember(id uint32, statement mysqlStatement) {
	conn, tracker := call.conn, call.tracker
	if _, ok := conn.statements[id]; !ok {
		if tracker.statements >= mysqlStatementLimit {
			for _, other := range tracker.conns {
				other.statements = nil
			}
			tracker.statements = 0
		}
		tracker.statements++
	}
	if conn.statements == nil {
		conn.statements = map[uint32]mysqlStatement{}
	}
	conn.statements[id] = statement
}

// mysqlLiteralEnd는 i에서 시작하는 따옴표 literal의 끝과 닫혔는지를 돌려준다. backslash escape와 두 번 쓴 따옴표를 처리한다.
func mysqlLiteralEnd(sql []byte, i int) (int, bool) {
	quote := sql[i]
	for i++; i < len(sql); i++ {
		switch {
		case sql[i] == '\\':
			i++
		case sql[i] == quote && i+1 < len(sql) && sql[i+1] == quote:
			i++
		case sql[i] == quote:
			return i + 1, true
		}
	}
	return len(sql), false
}

// mysqlCommentEnd는 i에서 주석이 시작하면 그 끝을, 아니면 i를 돌려준다. /*!와 /*+는 MySQL이 SQL로 읽으므로 주석이 아니다.
func mysqlCommentEnd(sql []byte, i int) int {
	lineEnd := func() int {
		if end := bytes.IndexByte(sql[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(sql)
	}
	switch {
	case sql[i] == '#':
		return lineEnd()
	case sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-' && (i+2 == len(sql) || sql[i+2] <= ' '):
		return lineEnd()
	case sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' && !(i+2 < len(sql) && (sql[i+2] == '!' || sql[i+2] == '+')):
		if end := bytes.Index(sql[i+2:], []byte("*/")); end >= 0 {
			return i + 2 + end + 2
		}
		return len(sql)
	}
	return i
}

// mysqlBacktickEnd는 backtick identifier의 끝이다. 두 번 쓴 backtick은 identifier 안의 backtick이다.
func mysqlBacktickEnd(sql []byte, i int) int {
	for i++; i < len(sql); i++ {
		if sql[i] == '`' {
			if i+1 < len(sql) && sql[i+1] == '`' {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}

// mysqlMaskSQL은 따옴표 literal 안을 ?로 바꾼다. 닫히지 않은 literal은 끝까지 가린다. 주석과 backtick identifier 안의
// 따옴표는 literal을 열지 않는다.
func mysqlMaskSQL(sql []byte) []byte {
	out := make([]byte, 0, len(sql))
	for i := 0; i < len(sql); {
		switch {
		case sql[i] == '\'' || sql[i] == '"':
			end, closed := mysqlLiteralEnd(sql, i)
			out = append(out, sql[i], '?')
			if closed {
				out = append(out, sql[i])
			}
			i = end
		case sql[i] == '`':
			end := mysqlBacktickEnd(sql, i)
			out = append(out, sql[i:end]...)
			i = end
		default:
			if end := mysqlCommentEnd(sql, i); end > i {
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

func mysqlIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= 0x80 || (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

// traceMySQLShape는 SQL에서 값을 뺀 모양이다. 문자열과 숫자 literal을 ?로, 연속 공백을 한 칸으로 바꾼다. identifier 안의
// 숫자는 그대로 둔다. 가린 SQL과 원문이 같은 shape를 만든다.
func traceMySQLShape(sql string) string {
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
		switch {
		case value == '\'' || value == '"':
			end, _ := mysqlLiteralEnd(text, i)
			out.WriteByte('?')
			i = end
		case value == '`':
			end := mysqlBacktickEnd(text, i)
			out.Write(text[i:end])
			i = end
		case value >= '0' && value <= '9':
			end := i
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
			if end < len(text) && mysqlIdentifierByte(text[end]) {
				for ; end < len(text) && mysqlIdentifierByte(text[end]); end++ {
				}
				out.Write(text[i:end])
			} else {
				out.WriteByte('?')
			}
			i = end
		case mysqlIdentifierByte(value):
			end := i
			for end < len(text) && mysqlIdentifierByte(text[end]) {
				end++
			}
			out.Write(text[i:end])
			i = end
		default:
			if end := mysqlCommentEnd(text, i); end > i {
				out.Write(text[i:end])
				i = end
				continue
			}
			out.WriteByte(value)
			i++
		}
	}
	shape := out.String()
	if len(shape) > mysqlShapeLimit {
		shape = traceTrimText(shape, mysqlShapeLimit) + "..."
	}
	return shape
}

// mysqlScrollLabelSQL은 스크롤 행에 쓰는 SQL의 길이 상한이다. 전체는 --raw와 상세 보기에 있다.
const mysqlScrollLabelSQL = 200

func mysqlResponseEvent(name string) bool {
	return name == mysqlEventOK || name == mysqlEventError || name == mysqlEventResult
}

// traceMySQLCounts는 MySQL group 행과 요약이 함께 쓰는 값이다. mysql_tls는 요청으로 세지 않고 연결로만 센다.
type traceMySQLCounts struct {
	Commands              uint64   `json:"commands"`
	Responses             uint64   `json:"responses"`
	Errors                uint64   `json:"errors"`
	Unanswered            uint64   `json:"unanswered"`
	LatencyAvgMS          *float64 `json:"latency_avg_ms"`
	LatencyMaxMS          *float64 `json:"latency_max_ms"`
	Connections           uint64   `json:"connections,omitempty"`
	TLSConnections        uint64   `json:"tls_connections,omitempty"`
	CompressedConnections uint64   `json:"compressed_connections,omitempty"`
	answered              uint64
	latency               traceSpans
}

func (counts *traceMySQLCounts) observe(event captureEvent) {
	switch {
	case event.Event == mysqlEventTLS:
		counts.Connections++
		counts.TLSConnections++
	case mysqlResponseEvent(event.Event):
		counts.Responses++
		if event.Event == mysqlEventError {
			counts.Errors++
		}
		counts.answered += event.answered
		counts.latency.observe(event.LatencyMS)
	default:
		counts.Commands++
		if event.Event == mysqlEventConnect {
			counts.Connections++
			if event.MySQL != nil && event.MySQL.Compressed {
				counts.CompressedConnections++
			}
		}
	}
}

func (counts traceMySQLCounts) finished() traceMySQLCounts {
	counts.Unanswered = counts.Commands - min(counts.Commands, counts.answered)
	counts.LatencyAvgMS, counts.LatencyMaxMS = counts.latency.summary()
	return counts
}

func traceMySQLGroupSummary(counts *traceMySQLCounts) *traceMySQLCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

func traceMySQLColumn(value func(counts traceMySQLCounts) string) func(traceGroupSummary) string {
	return func(group traceGroupSummary) string {
		if group.MySQL == nil {
			return "-"
		}
		return value(*group.MySQL)
	}
}

var traceMySQLGroupColumns = []traceGroupColumn{
	{screenTitle: "CMD", reportTitle: "COMMANDS", width: 5, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return strconv.FormatUint(counts.Commands, 10) })},
	{screenTitle: "RSP", reportTitle: "RESPONSES", width: 5, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return strconv.FormatUint(counts.Responses, 10) })},
	{screenTitle: "ERR", reportTitle: "ERRORS", width: 4, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return strconv.FormatUint(counts.Errors, 10) })},
	{screenTitle: "NOANS", reportTitle: "UNANSWERED", width: 5, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return strconv.FormatUint(counts.Unanswered, 10) })},
	{screenTitle: "AVGms", reportTitle: "AVG_MS", width: 6, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return traceLatency(counts.LatencyAvgMS, "") })},
	{screenTitle: "MAXms", reportTitle: "MAX_MS", width: 6, value: traceMySQLColumn(func(counts traceMySQLCounts) string { return traceLatency(counts.LatencyMaxMS, "") })},
}

// traceMySQLLine은 줄바꿈과 tab을 공백으로 바꿔 한 줄로 만든다.
func traceMySQLLine(text string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(text)
}

// traceMySQLCommandText는 event가 어떤 명령인지 한 줄로 쓴다. 응답 event도 답한 요청의 명령을 쓴다.
func traceMySQLCommandText(event captureEvent) string {
	info := event.MySQL
	sql := traceMySQLLine(traceTrimText(info.SQL, mysqlScrollLabelSQL))
	if len(info.SQL) > mysqlScrollLabelSQL {
		sql += "..."
	}
	switch {
	case event.Event == mysqlEventTLS:
		return "tls"
	case info.Command == mysqlCommandConnect:
		return emptyAs(strings.Join(slices.DeleteFunc([]string{info.User, info.Database, info.ServerVersion}, func(value string) bool { return value == "" }), " "), mysqlCommandConnect)
	case info.Command == mysqlCommandPrepare:
		return strings.TrimSpace("prepare " + sql)
	case info.Command == mysqlCommandExecute:
		return strings.TrimSpace(fmt.Sprintf("execute #%d %s", info.StatementID, sql))
	}
	return emptyAs(sql, info.Command)
}

// traceMySQLResultText는 응답 event의 결과를 쓴다.
func traceMySQLResultText(event captureEvent) string {
	info := event.MySQL
	switch event.Event {
	case mysqlEventError:
		return strings.TrimSpace(fmt.Sprintf("%d (%s) %s", info.ErrorCode, emptyAs(info.SQLState, "-"), traceMySQLLine(info.Message)))
	case mysqlEventResult:
		return fmt.Sprintf("%d columns", info.Columns)
	}
	switch {
	case info.AffectedRows != nil:
		return fmt.Sprintf("%d affected", *info.AffectedRows)
	case info.StatementID != 0 && info.Command == mysqlCommandPrepare:
		return fmt.Sprintf("statement #%d", info.StatementID)
	}
	return "ok"
}

// traceMySQLScrollLabels는 목적지 칸에 쪽, 명령, 응답이면 결과를, event 칸에 event 이름과 응답 시간을 쓴다. 한 목록에
// 두 쪽이 섞이므로 쪽을 앞에 붙인다. 글자로 붙여서 전체 화면의 / 필터로 server나 client를 찾을 수 있다.
func traceMySQLScrollLabels(event captureEvent) (string, string) {
	label := event.Event
	if event.LatencyMS != nil {
		label += " " + traceLatency(event.LatencyMS, "ms")
	}
	if event.MySQL == nil {
		return emptyAs(event.Destination, "-"), label
	}
	text := traceMySQLCommandText(event)
	if mysqlResponseEvent(event.Event) {
		text += " · " + traceMySQLResultText(event)
	}
	if event.Side != "" {
		text = event.Side + ": " + text
	}
	return text, label
}

type mysqlTraceKey struct {
	side    string
	command string
	shape   string
}

type mysqlTraceRow struct {
	Side      string   `json:"side,omitempty"`
	Command   string   `json:"command"`
	Shape     string   `json:"shape"`
	Processes []string `json:"processes,omitempty"`
	traceMySQLCounts
}

type mysqlTraceReport struct {
	Side       string          `json:"side,omitempty"`
	DurationMS int64           `json:"duration_ms"`
	LostEvents uint64          `json:"lost_events"`
	Statements []mysqlTraceRow `json:"statements"`
	traceMySQLCounts
	// Client와 Server는 두 쪽이 섞인 trace의 쪽별 합계다. 두 쪽의 응답 시간은 뜻이 달라 합친 평균은 뜻이 없다.
	Client *traceMySQLCounts `json:"client,omitempty"`
	Server *traceMySQLCounts `json:"server,omitempty"`
}

type mysqlTraceRowStats struct {
	processes map[string]struct{}
	counts    traceMySQLCounts
}

// mysqlTraceSummarizer는 --group-by 없이 끝난 MySQL trace를 쪽, 명령, SQL shape마다 한 행으로 묶는다.
type mysqlTraceSummarizer struct {
	rows   map[mysqlTraceKey]*mysqlTraceRowStats
	counts traceMySQLCounts
	sides  map[string]*traceMySQLCounts
}

func newMySQLTraceSummarizer() *mysqlTraceSummarizer {
	return &mysqlTraceSummarizer{rows: map[mysqlTraceKey]*mysqlTraceRowStats{}, sides: map[string]*traceMySQLCounts{}}
}

// mysqlTraceShape는 요약 행의 SQL 자리다. SQL이 없는 명령은 그 사정을 쓴다.
func mysqlTraceShape(info *traceMySQLEvent) string {
	switch {
	case info.Command == mysqlCommandConnect:
		return "(connect)"
	case info.Command == mysqlCommandExecute && info.SQL == "":
		return "(statement not seen)"
	case info.SQL == "":
		return "(no SQL)"
	}
	return traceMySQLLine(traceMySQLShape(info.SQL))
}

func (summarizer *mysqlTraceSummarizer) observe(event captureEvent) {
	if event.MySQL == nil {
		return
	}
	summarizer.counts.observe(event)
	if event.Side != "" {
		counts := summarizer.sides[event.Side]
		if counts == nil {
			counts = &traceMySQLCounts{}
			summarizer.sides[event.Side] = counts
		}
		counts.observe(event)
	}
	// TLS 연결은 명령이 없어서 합계에만 센다.
	if event.Event == mysqlEventTLS {
		return
	}
	key := mysqlTraceKey{side: event.Side, command: event.MySQL.Command, shape: mysqlTraceShape(event.MySQL)}
	stats := summarizer.rows[key]
	if stats == nil {
		stats = &mysqlTraceRowStats{processes: map[string]struct{}{}}
		summarizer.rows[key] = stats
	}
	if event.Process != "" {
		stats.processes[event.Process] = struct{}{}
	}
	stats.counts.observe(event)
}

func (summarizer *mysqlTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := mysqlTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, traceMySQLCounts: summarizer.counts.finished()}
	if len(summarizer.sides) == 1 {
		for side := range summarizer.sides {
			report.Side = side
		}
	} else if len(summarizer.sides) > 1 {
		report.Client, report.Server = traceMySQLGroupSummary(summarizer.sides[traceClientSide]), traceMySQLGroupSummary(summarizer.sides[traceServerSide])
	}
	report.Statements = make([]mysqlTraceRow, 0, len(summarizer.rows))
	for key, stats := range summarizer.rows {
		report.Statements = append(report.Statements, mysqlTraceRow{Side: key.side, Command: key.command, Shape: key.shape, Processes: slices.Sorted(maps.Keys(stats.processes)), traceMySQLCounts: stats.counts.finished()})
	}
	sort.Slice(report.Statements, func(i, j int) bool {
		left, right := report.Statements[i], report.Statements[j]
		switch {
		case left.Commands != right.Commands:
			return left.Commands > right.Commands
		case left.Shape != right.Shape:
			return left.Shape < right.Shape
		case left.Command != right.Command:
			return left.Command < right.Command
		}
		return left.Side < right.Side
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. MySQL 요약은 SQL 모양마다 한 행이라 같은 표를 쓴다.
func (report mysqlTraceReport) print(bool) {
	title := "MySQL trace"
	if report.Side == traceServerSide {
		title = "MySQL server trace"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n\n", title, (time.Duration(report.DurationMS) * time.Millisecond).String())
	mixed := report.Client != nil && report.Server != nil
	if mixed {
		for _, side := range []struct {
			heading string
			counts  *traceMySQLCounts
		}{{"Client side (commands that this host sent):", report.Client}, {"Server side (commands that local servers received):", report.Server}} {
			fmt.Fprintln(os.Stdout, side.heading)
			printMySQLCounts("  ", *side.counts)
			fmt.Fprintln(os.Stdout)
		}
	} else {
		printMySQLCounts("", report.traceMySQLCounts)
	}
	fmt.Fprintf(os.Stdout, "Lost events: %d\n", report.LostEvents)
	if len(report.Statements) == 0 {
		return
	}
	sideColumn := func(value string) string {
		if !mixed {
			return ""
		}
		return value + "\t"
	}
	fmt.Fprintln(os.Stdout, "\n"+sideColumn("SIDE")+"COMMAND\tSQL\tCOUNT\tERRORS\tUNANSWERED\tAVG\tMAX\tPROCESS")
	for _, row := range report.Statements {
		fmt.Fprintf(os.Stdout, "%s%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n", sideColumn(emptyAs(row.Side, "-")), row.Command, row.Shape, row.Commands, row.Errors, row.Unanswered, traceLatency(row.LatencyAvgMS, "ms"), traceLatency(row.LatencyMaxMS, "ms"), emptyAs(strings.Join(row.Processes, ","), "-"))
	}
}

func printMySQLCounts(indent string, counts traceMySQLCounts) {
	for _, line := range []string{
		fmt.Sprintf("Commands: %d", counts.Commands),
		fmt.Sprintf("Responses: %d", counts.Responses),
		fmt.Sprintf("Errors: %d", counts.Errors),
		fmt.Sprintf("Unanswered: %d", counts.Unanswered),
		"Latency avg: " + traceLatency(counts.LatencyAvgMS, "ms"),
		"Latency max: " + traceLatency(counts.LatencyMaxMS, "ms"),
		fmt.Sprintf("Connections: %d", counts.Connections),
		fmt.Sprintf("TLS connections: %d", counts.TLSConnections),
		fmt.Sprintf("Compressed connections: %d", counts.CompressedConnections),
	} {
		fmt.Fprintln(os.Stdout, indent+line)
	}
}
