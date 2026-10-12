package edc

import (
	"bytes"
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
	tracePostgresDefaultPort = 5432
	postgresEventPrefix      = "postgres_"
	postgresEventConnect     = "postgres_connect"
	postgresEventTLS         = "postgres_tls"
	postgresEventQuery       = "postgres_query"
	postgresEventResult      = "postgres_result"
	postgresEventError       = "postgres_error"
	postgresCommandConnect   = "connect"
	postgresCommandQuery     = "query"
	postgresCommandExecute   = "execute"
	postgresCommandPrepare   = "prepare"
	postgresCommandFunction  = "function"
	// postgresSQLLimit은 event와 저장한 prepared statement의 SQL 상한이다. mysqlSQLLimit과 같다.
	postgresSQLLimit = 1024
	// postgresMessageLimit은 ErrorResponse의 message와 detail 상한이다.
	postgresMessageLimit = 1024
	// 아래 세 상한은 연결 수와 모든 연결의 statement, portal 합계 상한이다. 넘으면 모두 비운다. 비운 뒤 이어지는 응답은 짝
	// 없이 사라지고, execute는 SQL 없이 보인다.
	postgresSocketLimit    = 65536
	postgresStatementLimit = 65536
	postgresPortalLimit    = 65536
	// postgresPendingLimit은 ReadyForQuery를 기다리는 요청 단위의 모든 연결 합계 상한이다. 연결마다 두면 연결 상한과 곱해져
	// 메모리가 커진다.
	postgresPendingLimit = 1024
	// postgresSQLKeep은 Query와 Parse에서 가리기 전에 보는 SQL byte 수다. 가린 뒤 postgresSQLLimit으로 자른다.
	postgresSQLKeep = 4096
	// postgresNameLimit은 statement, portal, 사용자, database 이름과 tag의 상한이다. 서버는 이름을 63 byte로 자른다.
	postgresNameLimit = 64
	// postgresErrorKeep은 ErrorResponse에서 보는 byte 수다. message와 detail 상한을 담을 만큼이다.
	postgresErrorKeep = 4096
	// postgresStartupKeep은 StartupMessage에서 보는 byte 수다. postgresStartupLimit은 서버가 받는 길이 상한이다.
	postgresStartupKeep  = 2048
	postgresStartupLimit = 10000
	// postgresMaxLength는 message 길이 필드로 받아들이는 값의 상한이다. 서버의 할당 상한(1 GB - 1)과 같다.
	postgresMaxLength = 0x3fffffff
	// postgresTailCapture는 BPF가 응답 방향 쓰기나 읽기의 끝에서 더 읽는 byte 수다. ReadyForQuery(6 byte)와 그 앞의
	// CommandComplete를 담는다. 큰 결과는 앞부분만으로 경계를 잃어도 이 끝부분으로 응답 시간을 잰다.
	postgresTailCapture = 64
)

// protocol 상수는 PostgreSQL 문서의 Message Formats와 같다. 시작 message의 code는 길이 뒤 4 byte다.
const (
	postgresCodeSSL    = 80877103
	postgresCodeGSS    = 80877104
	postgresCodeCancel = 80877102
	// postgresFrontendTypes와 postgresBackendTypes는 방향마다 받아들이는 message type이다.
	postgresFrontendTypes = "BCDEFHPQSXcdfp"
	postgresBackendTypes  = "123ACDEGHIKNRSTVWZcdnstv"
)

// tracePostgresEvent는 PostgreSQL event에만 붙는 값이다. 응답 event에는 답한 요청의 command와 SQL을 함께 붙인다.
type tracePostgresEvent struct {
	Command       string  `json:"command,omitempty"`
	SQL           string  `json:"sql,omitempty"`
	SQLTruncated  bool    `json:"sql_truncated,omitempty"`
	Statement     string  `json:"statement,omitempty"`
	Executes      int     `json:"executes,omitempty"`
	User          string  `json:"user,omitempty"`
	Database      string  `json:"database,omitempty"`
	ServerVersion string  `json:"server_version,omitempty"`
	Encryption    string  `json:"encryption,omitempty"`
	Tag           string  `json:"tag,omitempty"`
	Rows          *uint64 `json:"rows,omitempty"`
	Severity      string  `json:"severity,omitempty"`
	SQLState      string  `json:"sql_state,omitempty"`
	Message       string  `json:"message,omitempty"`
	Detail        string  `json:"detail,omitempty"`
}

// postgresMessage는 message 하나다. typ이 0이면 type byte가 없는 시작 message(StartupMessage, SSLRequest 등)다. body는
// 앞부분이고 record payload를 가리킬 수 있으므로 deliver 밖에 두지 않는다. truncated는 body 뒤를 보지 못했다는 뜻이다.
type postgresMessage struct {
	typ       byte
	length    int
	body      []byte
	truncated bool
	at        uint64
}

// postgresStream은 한 방향의 byte 흐름에서 message 경계를 따라간다. 캡처한 앞부분보다 큰 message는 record의 size로
// 남은 길이를 건너뛰어 경계를 지킨다. 두 record에 걸친 header는 최대 4 byte를 쥐고 잇는다.
type postgresStream struct {
	synced bool
	// untyped는 다음 message가 type byte 없는 시작 message라는 뜻이다. 연결 처음과 SSLRequest에 'N'을 받은 뒤다.
	untyped   bool
	header    [5]byte
	headerLen int
	inBody    bool
	typ       byte
	length    int
	left      int
	want      int
	hold      bool
	held      []byte
	delivered bool
	at        uint64
	// lostAt은 feed가 경계를 잃을 때까지 지나간 byte 수이고, garbage는 캡처 안의 header가 맞지 않아 잃었다는 뜻이다.
	lostAt  int
	garbage bool
}

// postgresKeep은 message에서 볼 body byte 수와, 그 앞부분이 다음 record로 이어질 때 쥐고 기다릴지를 정한다. 비밀이 섞일
// 수 있는 body(SQL, Bind, ErrorResponse, 시작 message)는 record를 넘어 쥐지 않고 그 record에서 본 만큼만 넘긴다.
// password, SASL, Bind 값, CopyData, FunctionCall 인자, BackendKeyData는 0 byte만 보고 길이로 건너뛴다.
func postgresKeep(frontend bool, typ byte, length int) (int, bool) {
	if typ == 0 {
		return min(length, postgresStartupKeep), false
	}
	if frontend {
		switch typ {
		case 'Q':
			return min(length, postgresSQLKeep), false
		case 'P':
			return min(length, postgresSQLKeep+postgresNameLimit+1), false
		case 'B':
			return min(length, 2*(postgresNameLimit+1)), false
		case 'E', 'D', 'C':
			return min(length, postgresNameLimit+2), true
		case 'F':
			return min(length, 4), true
		}
		return 0, false
	}
	switch typ {
	case 'E':
		return min(length, postgresErrorKeep), false
	case 'C':
		return min(length, postgresNameLimit+1), true
	case 'S':
		return min(length, 2*(postgresNameLimit+1)), true
	case 'Z':
		return min(length, 1), true
	}
	return 0, false
}

func (stream *postgresStream) reset() { *stream = postgresStream{} }

// feed는 record 하나를 이어서 읽고 message마다 deliver를 부른다. deliver가 false를 돌려주면 그 record를 더 읽지 않는다.
// header가 캡처한 앞부분 밖에 있거나 길이와 type이 맞지 않으면 경계를 잃었다는 뜻으로 false를 돌려준다.
func (stream *postgresStream) feed(data []byte, size int, at uint64, frontend bool, deliver func(postgresMessage) bool) bool {
	size = max(size, len(data))
	pos, used := 0, 0
	for used < size {
		if !stream.inBody {
			need := 5
			if stream.untyped {
				need = 4
			}
			if pos >= len(data) {
				stream.lostAt = used
				return false
			}
			if stream.headerLen == 0 {
				stream.at = at
			}
			n := copy(stream.header[stream.headerLen:need], data[pos:])
			stream.headerLen, pos, used = stream.headerLen+n, pos+n, used+n
			if stream.headerLen < need {
				continue
			}
			typ, raw := byte(0), stream.header[:4]
			if !stream.untyped {
				typ, raw = stream.header[0], stream.header[1:5]
			}
			length := int(int32(binary.BigEndian.Uint32(raw)))
			if stream.untyped && (length < 8 || length > postgresStartupLimit) || !stream.untyped && (length < 4 || length > postgresMaxLength || !postgresTypeValid(frontend, typ)) {
				stream.lostAt, stream.garbage = used, true
				return false
			}
			stream.typ, stream.length, stream.left = typ, length-4, length-4
			stream.want, stream.hold = postgresKeep(frontend, typ, stream.length)
			stream.inBody, stream.held, stream.delivered = true, nil, false
		}
		n := min(stream.left, size-used)
		seen := max(0, min(n, len(data)-pos))
		if !stream.delivered {
			take := min(seen, stream.want-len(stream.held))
			body := data[pos : pos+take]
			if len(stream.held) > 0 || (stream.hold && take < stream.want && seen == n && n < stream.left) {
				stream.held = append(stream.held, body...)
				body = stream.held
			}
			// 다 모았거나, 이 message의 byte 일부를 캡처하지 못했거나, 쥐지 않는 message가 다음 record로 이어지면 넘긴다.
			if len(body) == stream.want || seen < n || (!stream.hold && n < stream.left) {
				stream.delivered = true
				message := postgresMessage{typ: stream.typ, length: stream.length, body: body, truncated: len(body) < stream.length, at: stream.at}
				stream.held = nil
				if !deliver(message) {
					return true
				}
			}
		}
		pos, used, stream.left = pos+seen, used+n, stream.left-n
		if stream.left == 0 {
			stream.inBody, stream.headerLen, stream.held = false, 0, nil
		}
	}
	return true
}

func postgresTypeValid(frontend bool, typ byte) bool {
	types := postgresBackendTypes
	if frontend {
		types = postgresFrontendTypes
	}
	return typ != 0 && bytes.IndexByte([]byte(types), typ) >= 0
}

// postgresCleanStart는 경계를 모르는 방향의 record가 message 시작에서 시작하는지 본다. 첫 byte부터 header를 이어 걸어
// 모든 header가 맞아야 하고, record를 다 캡처했으면 걸음이 size에서 정확히 끝나야 한다. 다 캡처하지 못했으면 header가
// 둘 이상 맞아야 한다. DataRow 안의 byte나 암호문이 우연히 type처럼 보여도 받아들이지 않기 위해서다.
func postgresCleanStart(data []byte, size int, frontend bool) bool {
	size = max(size, len(data))
	pos, headers := 0, 0
	for pos < size {
		if pos+5 > len(data) {
			return size > len(data) && headers >= 2
		}
		length := int(int32(binary.BigEndian.Uint32(data[pos+1 : pos+5])))
		if !postgresTypeValid(frontend, data[pos]) || length < 4 || length > postgresMaxLength {
			return false
		}
		headers++
		pos += 1 + length
	}
	if pos == size {
		return headers > 0
	}
	return size > len(data) && headers >= 2
}

// postgresUntypedStart는 record 하나가 type byte 없는 시작 message 하나와 정확히 같은지 본다. 연결의 첫 요청 record에만 쓴다.
func postgresUntypedStart(data []byte, size int) bool {
	if len(data) < 8 {
		return false
	}
	length, code := int(int32(binary.BigEndian.Uint32(data[:4]))), binary.BigEndian.Uint32(data[4:8])
	switch {
	case length != size:
		return false
	case code == postgresCodeSSL || code == postgresCodeGSS:
		return length == 8
	case code == postgresCodeCancel:
		// protocol 3.2(PostgreSQL 18)는 cancel key를 256 byte까지 늘린다.
		return length >= 16 && length <= 12+256
	}
	return code>>16 == 3 && length > 8 && length <= postgresStartupLimit
}

// postgresUnit은 ReadyForQuery 하나가 끝내는 요청 단위다. Query 하나, Sync로 닫힌 extended batch 하나, FunctionCall 하나,
// 또는 시작 message다. 응답 쪽 값은 head일 때 모은다.
type postgresUnit struct {
	at    uint64
	bytes int
	info  tracePostgresEvent
	// silent는 Parse도 Execute도 없는 batch다. event 없이 ReadyForQuery 하나만 소비한다.
	silent bool
	// mark는 이 단위를 줄에 넣을 때까지 본 응답 방향 record 수다. 응답 방향 재동기에서 이 단위 뒤에 건너뛴 응답 record가
	// 있었는지 가리는 데 쓴다.
	mark          uint64
	responseBytes int
	rows          uint64
	rowsSeen      bool
	tag           string
	failed        bool
	severity      string
	sqlState      string
	message       string
	detail        string
	parsed        bool
	prepareSQL    string
	prepareCut    bool
	prepareName   string
	// lost는 응답 방향이 이 단위의 응답 중에 경계를 잃었다는 뜻이다. 그 사이의 DataRow는 세지 못했다.
	lost bool
}

type postgresStatement struct {
	sql       string
	truncated bool
}

type postgresConn struct {
	request, response postgresStream
	requestRecords    uint64
	responseRecords   uint64
	// startup은 StartupMessage를 보고 첫 ReadyForQuery를 아직 못 본 상태다. 이때 ErrorResponse는 연결을 끝낸다.
	startup bool
	// answer는 SSLRequest('S')나 GSSENCRequest('G') 뒤 서버의 1 byte 답을 기다린다는 뜻이다.
	answer    byte
	encrypted bool
	// closed는 CancelRequest처럼 더 읽을 message가 없는 연결이다.
	closed bool
	// strings는 ParameterStatus로 받은 standard_conforming_strings다. 연결 중간부터 보면 모른다.
	strings postgresStrings
	// copyIn은 extended batch가 CopyInResponse를 받았다는 뜻이고, copySync는 그 COPY가 CopyDone이나 CopyFail로 끝났다는
	// 뜻이다. 서버는 copy-in 중의 Sync를 무시하고 CopyDone 뒤의 Sync에만 ReadyForQuery를 하나 보낸다. COPY 중 오류가 나도
	// Sync까지 버린 뒤 하나만 보낸다. 이 Sync들로 단위를 더 만들면 뒤 응답이 모두 한 칸씩 밀린다.
	copyIn        bool
	copySync      bool
	serverVersion string
	batch         *postgresUnit
	queue         []*postgresUnit
	// skip은 상한을 넘어 줄에서 버린 단위의 수다. 그 수만큼의 ReadyForQuery는 뒤 단위를 닫지 않고 건너뛴다.
	skip       int
	statements map[string]postgresStatement
	portals    map[string]string
	// lost는 단위를 기다리는 동안 응답 방향의 경계를 잃었다는 뜻이고, lostMark는 그 응답 record의 번호다. 그때 기다리던 단위
	// 하나만 응답의 끝에 보인 ReadyForQuery로 닫는다. blind는 그 뒤 끝을 보지 못한 응답 record가 있었거나 캡처 안에서 경계가
	// 틀렸다는 뜻이다. 그러면 ReadyForQuery가 보이지 않게 지나갔을 수 있다.
	lost     bool
	lostMark uint64
	blind    bool
	// tailWant는 앞부분만 캡처한 마지막 응답 record의 size다. 그 record의 끝부분 record가 오면 0이 된다.
	tailWant int
	// resyncNext는 응답의 끝이 ReadyForQuery였다는 뜻이다. 서버는 ReadyForQuery 뒤에 출력을 비우므로 다음 응답 record는 message
	// 경계에서 시작한다.
	resyncNext bool
}

// postgresTracker는 socket마다 두 방향의 message를 따라가며 요청 단위와 ReadyForQuery를 짝짓는다. side는 tracker가 볼
// 쪽이고 비어 있으면 두 쪽을 모두 본다.
type postgresTracker struct {
	side        string
	showSecrets bool
	conns       map[uint64]*postgresConn
	pending     int
	statements  int
	portals     int
}

func newPostgresTracker(side string, showSecrets bool) *postgresTracker {
	return &postgresTracker{side: side, showSecrets: showSecrets, conns: map[uint64]*postgresConn{}}
}

// postgresCall은 record 하나를 처리하는 동안의 값이다.
type postgresCall struct {
	tracker *postgresTracker
	conn    *postgresConn
	record  mysqlRecord
	side    string
	offset  int64
	out     []captureEvent
}

func (tracker *postgresTracker) events(record mysqlRecord, clockOffset int64) []captureEvent {
	side := traceClientSide
	if record.server {
		side = traceServerSide
	}
	if tracker.side != "" && side != tracker.side {
		return nil
	}
	conn := tracker.conns[record.socket]
	if conn == nil {
		if len(tracker.conns) >= postgresSocketLimit {
			clear(tracker.conns)
			tracker.pending, tracker.statements, tracker.portals = 0, 0, 0
		}
		conn = &postgresConn{}
		tracker.conns[record.socket] = conn
	}
	call := &postgresCall{tracker: tracker, conn: conn, record: record, side: side, offset: clockOffset}
	// client는 요청을 보내고 응답을 받는다. 서버는 요청을 받고 응답을 보낸다.
	if record.server != record.sent {
		call.request()
	} else {
		call.response()
	}
	return call.out
}

func (tracker *postgresTracker) forgetSocket(socket uint64) {
	conn := tracker.conns[socket]
	if conn == nil {
		return
	}
	tracker.pending = max(0, tracker.pending-len(conn.queue))
	tracker.statements = max(0, tracker.statements-len(conn.statements))
	tracker.portals = max(0, tracker.portals-len(conn.portals))
	delete(tracker.conns, socket)
}

func (call *postgresCall) event(name string, at uint64, length int, info *tracePostgresEvent) captureEvent {
	record := call.record
	return captureEvent{
		SocketID: record.socket, TimestampNS: uint64(int64(at) + call.offset), BootTimeNS: at, Event: name, Protocol: "postgres", Side: call.side,
		PID: record.pid, Process: record.process, CgroupID: record.cgroupID, Source: record.source, Destination: record.destination, Bytes: uint64(length),
		Postgres: info,
	}
}

// request는 요청 방향 record 하나를 읽는다.
func (call *postgresCall) request() {
	conn, record := call.conn, call.record
	first := conn.requestRecords == 0
	conn.requestRecords++
	if conn.encrypted || conn.closed {
		return
	}
	data, size := record.payload, max(record.size, len(record.payload))
	if !conn.request.synced {
		switch {
		case first && len(data) >= 2 && data[0] == 0x16 && data[1] == 0x03:
			// sslnegotiation=direct는 SSLRequest 없이 TLS ClientHello부터 보낸다.
			call.encrypt(record.bootTimeNS, size, "ssl")
			return
		case first && postgresUntypedStart(data, size):
			conn.request = postgresStream{synced: true, untyped: true}
			if conn.responseRecords == 0 {
				conn.response = postgresStream{synced: true}
			}
		case postgresCleanStart(data, size, true):
			conn.request = postgresStream{synced: true}
		default:
			return
		}
	}
	if !conn.request.feed(data, size, record.bootTimeNS, true, call.frontend) {
		call.lose(true)
	}
}

// response는 응답 방향 record 하나를 읽는다.
func (call *postgresCall) response() {
	conn, record := call.conn, call.record
	seen := conn.responseRecords
	conn.responseRecords++
	if conn.encrypted || conn.closed {
		return
	}
	data, size := record.payload, max(record.size, len(record.payload))
	// 앞 record의 끝부분 record가 오지 않았으면 그 끝은 보지 못한 것이다.
	if conn.tailWant != 0 {
		conn.blind = true
	}
	conn.tailWant = 0
	if len(data) < size {
		conn.tailWant = size
	}
	if conn.answer != 0 {
		kind := conn.answer
		conn.answer = 0
		switch {
		case len(data) == 0:
			call.lose(false)
			return
		case data[0] == 'S' || data[0] == 'G':
			name := "ssl"
			if kind == 'G' {
				name = "gssapi"
			}
			call.encrypt(record.bootTimeNS, size, name)
			return
		case data[0] == 'N':
			data, size = data[1:], size-1
		}
	}
	if !conn.response.synced {
		switch {
		case conn.resyncNext:
			conn.resyncNext = false
			call.resync(seen)
		case conn.lost && len(data) == size && call.closeAtEnd(record.bootTimeNS, data, size):
			return
		case size != 0 && postgresCleanStart(data, size, false):
			if call.keepLost() {
				// 경계를 잃은 뒤 모든 응답 record의 끝을 보았고 어느 끝도 ReadyForQuery가 아니었다. 서버는 ReadyForQuery마다
				// 출력을 비우므로 기다리던 단위는 아직 끝나지 않았다. 큰 결과 중간의 쓰기가 우연히 message 경계에서 시작한 경우다.
				// 남는 위험: client 쪽 읽기는 ReadyForQuery 뒤에 바로 온 NotificationResponse까지 한 번에 읽을 수 있다. 그러면
				// ReadyForQuery가 끝에 보이지 않아 이 단위가 다음 요청의 ReadyForQuery로 닫힌다.
				conn.response, conn.lost = postgresStream{synced: true}, false
			} else {
				call.resync(seen)
			}
		default:
			call.countLost(size)
			return
		}
	}
	if !conn.response.feed(data, size, record.bootTimeNS, false, call.backend) {
		call.loseResponse(seen, size-conn.response.lostAt, conn.response.garbage)
	}
}

// resync는 이 record부터 응답 방향의 경계를 다시 따라간다. 경계를 잃은 동안 건너뛴 응답 record는 앞 요청의 꼬리일 수 있다. 그
// 뒤에 들어온 단위만 이 record부터 짝짓는다.
// 남는 위험: pipeline client가 잘린 응답의 꼬리를 받기 전에 새 요청을 보내고, 그 꼬리가 깨끗한 record 하나로 오면 그 꼬리의
// ReadyForQuery가 새 요청을 닫는다. 이 경우 응답 시간이 틀린다.
// 건너뛸 ReadyForQuery가 건너뛴 record에 있었는지 모르므로 skip도 비운다.
func (call *postgresCall) resync(seen uint64) {
	conn := call.conn
	conn.response = postgresStream{synced: true}
	call.dropBefore(seen)
	conn.skip, conn.lost, conn.blind = 0, false, false
}

// lostHead는 경계를 잃을 때 기다리던 단위가 줄에 하나만 남아 있으면 그 단위다. 이때 응답 방향에 오는 다음 ReadyForQuery는 그
// 단위의 것이다. 둘 이상이면 보지 못한 byte에 어느 단위의 ReadyForQuery가 있었는지 모른다.
func (call *postgresCall) lostHead() *postgresUnit {
	conn := call.conn
	if !conn.lost || conn.skip != 0 || len(conn.queue) != 1 || conn.queue[0].mark > conn.lostMark {
		return nil
	}
	return conn.queue[0]
}

func (call *postgresCall) keepLost() bool {
	return !call.conn.blind && call.lostHead() != nil
}

// countLost는 경계를 잃은 동안 건너뛴 응답 record의 byte를 기다리던 단위에 더한다.
func (call *postgresCall) countLost(size int) {
	if head := call.lostHead(); head != nil {
		head.responseBytes += size
	}
}

// loseResponse는 응답 방향의 header가 캡처 밖이거나 맞지 않아 경계를 잃은 때다. 기다리던 단위는 줄에 남긴다. 응답의 끝이
// 보이면 closeAtEnd가 닫고, 경계를 다시 찾으면 resync가 그 전 단위를 버린다. rest는 이 record에서 읽지 못한 byte 수다.
func (call *postgresCall) loseResponse(seen uint64, rest int, garbage bool) {
	conn := call.conn
	conn.response = postgresStream{}
	if len(conn.queue) == 0 {
		return
	}
	conn.lost, conn.lostMark, conn.blind = true, seen, garbage
	if conn.skip == 0 {
		head := conn.queue[0]
		head.lost = true
		head.responseBytes += max(rest, 0)
	}
}

// postgresEndsReady는 data가 ReadyForQuery로 끝나는지와 그 트랜잭션 상태를 돌려준다.
func postgresEndsReady(data []byte) (byte, bool) {
	n := len(data)
	if n < 6 || !bytes.Equal(data[n-6:n-1], []byte{'Z', 0, 0, 0, 5}) {
		return 0, false
	}
	status := data[n-1]
	return status, status == 'I' || status == 'T' || status == 'E'
}

// postgresEndTag는 끝의 ReadyForQuery 바로 앞에서 끝나는 온전한 CommandComplete의 tag다. 가장 앞에서 시작하는 후보를 고른다.
func postgresEndTag(data []byte) (string, bool) {
	end := len(data) - 6
	for i := 0; i+6 <= end; i++ {
		if data[i] != 'C' || int(binary.BigEndian.Uint32(data[i+1:i+5])) != end-i-1 || data[end-1] != 0 {
			continue
		}
		tag := data[i+5 : end-1]
		if len(tag) == 0 || len(tag) > postgresNameLimit || tag[0] < 'A' || tag[0] > 'Z' || bytes.ContainsFunc(tag, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
			continue
		}
		return string(tag), true
	}
	return "", false
}

// postgresEndError는 끝의 ReadyForQuery 바로 앞에서 끝나는 ErrorResponse의 body다. 온전한 message가 보이면 field도 돌려주고,
// 앞부분이 보이지 않아도 field 목록의 끝(NUL 둘)이 보이면 failed만 참이다. CommandComplete의 tag는 NUL 하나로 끝난다.
func postgresEndError(data []byte) (body []byte, failed bool) {
	end := len(data) - 6
	if end < 2 || data[end-1] != 0 || data[end-2] != 0 {
		return nil, false
	}
	for i := 0; i+6 <= end; i++ {
		if data[i] == 'E' && int(binary.BigEndian.Uint32(data[i+1:i+5])) == end-i-1 && data[i+5] >= 'A' && data[i+5] <= 'Z' {
			return data[i+5 : end], true
		}
	}
	return nil, true
}

// postgresTagRows는 경계를 잃은 단위의 행 수다. DataRow를 다 세지 못했으므로 행 수를 담는 SELECT와 FETCH tag에서만 읽는다.
func postgresTagRows(tag string) (uint64, bool) {
	command, count, ok := strings.Cut(tag, " ")
	if !ok || command != "SELECT" && command != "FETCH" {
		return 0, false
	}
	rows, err := strconv.ParseUint(count, 10, 64)
	return rows, err == nil
}

// ready는 ReadyForQuery가 바꾸는 연결 상태다.
func (call *postgresCall) ready(status byte) {
	conn := call.conn
	conn.startup, conn.copyIn = false, false
	if status == 'I' {
		// 트랜잭션 밖에서는 이름 있는 portal도 모두 닫힌다.
		call.tracker.portals = max(0, call.tracker.portals-len(conn.portals))
		conn.portals = nil
	}
}

// closeAtEnd는 경계를 잃은 응답의 끝이 ReadyForQuery일 때 기다리던 단위를 at에 닫는다. data는 응답 record의 끝이다. 끝까지
// 캡처한 record이거나 끝부분 record다. 닫으면 다음 응답 record부터 경계를 다시 따라간다. 단위가 하나가 아니면 닫지 않는다.
// 남는 위험: DataRow 값이 우연히 ReadyForQuery와 같은 6 byte로 쓰기를 끝내면 결과가 끝나기 전에 닫는다.
func (call *postgresCall) closeAtEnd(at uint64, data []byte, size int) bool {
	conn := call.conn
	status, ok := postgresEndsReady(data)
	head := call.lostHead()
	if !ok || head == nil {
		return false
	}
	head.responseBytes += size
	if tag, ok := postgresEndTag(data); ok {
		head.tag = postgresName([]byte(tag))
	} else if body, failed := postgresEndError(data); failed && !head.failed {
		// field가 끝부분 밖이면 오류라는 것만 안다.
		head.failed = true
		if body != nil {
			head.severity, head.sqlState, head.message, head.detail = call.errorFields(body)
		}
	}
	call.ready(status)
	call.finish(head, at)
	conn.response, conn.lost, conn.blind, conn.resyncNext = postgresStream{}, false, false, true
	return true
}

// tailEvents는 끝부분 record 하나를 읽는다. BPF는 앞부분 record를 낸 직후 같은 쓰기나 읽기의 마지막 byte를 이 record로 넘긴다.
// 경계를 따라가는 중이면 끝부분은 message 본문 안이라 읽지 않는다. 경계를 잃었으면 끝이 ReadyForQuery인지만 본다.
func (tracker *postgresTracker) tailEvents(record mysqlRecord, clockOffset int64) []captureEvent {
	side := traceClientSide
	if record.server {
		side = traceServerSide
	}
	conn := tracker.conns[record.socket]
	if tracker.side != "" && side != tracker.side || conn == nil || record.server != record.sent || conn.encrypted || conn.closed {
		return nil
	}
	if conn.tailWant == 0 || record.size != conn.tailWant {
		return nil
	}
	conn.tailWant = 0
	if conn.response.synced || conn.resyncNext {
		return nil
	}
	call := &postgresCall{tracker: tracker, conn: conn, record: record, side: side, offset: clockOffset}
	if call.closeAtEnd(record.bootTimeNS, record.payload, 0) {
		return call.out
	}
	if status, ok := postgresEndsReady(record.payload); ok {
		// 기다리던 단위를 알 수 없으므로 닫지 않는다. 다음 응답 record에서 경계를 다시 찾고 그 전 단위를 버린다.
		call.ready(status)
		conn.resyncNext = true
	}
	return call.out
}

// lose는 header가 캡처 밖이라 그 방향의 경계를 잃은 때다. ReadyForQuery를 볼 수 없으므로 기다리던 단위를 모두 버린다.
// 버린 단위는 응답 event도 응답 시간도 없이 응답 없음으로 남는다. 요청 방향이면 Sync 전의 batch도 버린다.
// 남는 위험: 요청 방향에서 잃은 부분에 Sync나 Query가 있었고 client가 그 응답 전에 새 요청을 pipeline으로 보내면, 잃은
// 요청의 ReadyForQuery가 새 요청을 닫는다.
func (call *postgresCall) lose(request bool) {
	conn := call.conn
	call.dropAll()
	if request {
		conn.batch = nil
		conn.request = postgresStream{}
	} else {
		conn.response = postgresStream{}
	}
}

// dropBefore는 mark가 seen이 아닌 단위, 곧 seen번째 응답 record 전에 다른 응답 record가 지나간 단위를 버린다.
func (call *postgresCall) dropBefore(seen uint64) {
	conn := call.conn
	kept := conn.queue[:0]
	for _, unit := range conn.queue {
		if unit.mark == seen {
			kept = append(kept, unit)
		}
	}
	call.tracker.pending = max(0, call.tracker.pending-(len(conn.queue)-len(kept)))
	clear(conn.queue[len(kept):])
	conn.queue = kept
}

// dropAll은 줄의 단위를 모두 버린다. 건너뛸 ReadyForQuery 수도 더는 알 수 없으므로 비운다.
func (call *postgresCall) dropAll() {
	conn := call.conn
	call.tracker.pending = max(0, call.tracker.pending-len(conn.queue))
	conn.queue, conn.skip, conn.lost, conn.blind = nil, 0, false, false
}

// encrypt는 TLS나 GSSAPI 암호화가 시작된 연결이다. 뒤 record는 읽을 수 없으므로 forgetSocket까지 모두 무시한다.
func (call *postgresCall) encrypt(at uint64, size int, kind string) {
	conn := call.conn
	call.dropAll()
	conn.batch, conn.encrypted = nil, true
	call.out = append(call.out, call.event(postgresEventTLS, at, size, &tracePostgresEvent{Encryption: kind, Message: "encrypted traffic is not readable"}))
}

// enqueue는 ReadyForQuery를 기다리는 단위를 줄에 넣고, silent가 아니면 요청 event를 낸다. 모든 연결의 합계가 상한을
// 넘으면 모든 연결의 줄을 비운다. 비운 단위는 응답 없음으로 남고, 그 수를 skip에 더해 그 단위들의 ReadyForQuery가 뒤
// 단위를 닫지 않게 한다. 응답 방향의 경계는 그대로 따라간다.
func (call *postgresCall) enqueue(unit *postgresUnit, name string) {
	tracker, conn := call.tracker, call.conn
	if tracker.pending >= postgresPendingLimit {
		for _, other := range tracker.conns {
			other.skip += len(other.queue)
			other.queue, other.lost = nil, false
		}
		tracker.pending = 0
	}
	unit.mark = conn.responseRecords
	conn.queue = append(conn.queue, unit)
	tracker.pending++
	if !unit.silent {
		info := unit.info
		call.out = append(call.out, call.event(name, unit.at, unit.bytes, &info))
	}
}

func (conn *postgresConn) openBatch(at uint64) *postgresUnit {
	if conn.batch == nil {
		conn.batch = &postgresUnit{at: at}
	}
	return conn.batch
}

// postgresCString은 data에서 NUL로 끝나는 문자열과 그 뒤 위치를 돌려준다. NUL이 없으면 ok가 거짓이다.
func postgresCString(data []byte) ([]byte, []byte, bool) {
	end := bytes.IndexByte(data, 0)
	if end < 0 {
		return data, nil, false
	}
	return data[:end], data[end+1:], true
}

// postgresName은 이름 하나를 상한으로 자르고 제어 문자를 escape한 사본이다.
func postgresName(raw []byte) string {
	return traceEscapeText([]byte(traceTrimText(string(raw), postgresNameLimit)))
}

// remember는 Parse한 statement의 가린 SQL을 둔다. 모든 연결의 합계가 상한을 넘으면 모두 비운다.
func (call *postgresCall) remember(name string, statement postgresStatement) {
	conn, tracker := call.conn, call.tracker
	if _, ok := conn.statements[name]; !ok {
		if tracker.statements >= postgresStatementLimit {
			for _, other := range tracker.conns {
				other.statements = nil
			}
			tracker.statements = 0
		}
		tracker.statements++
	}
	if conn.statements == nil {
		conn.statements = map[string]postgresStatement{}
	}
	conn.statements[name] = statement
}

func (call *postgresCall) forgetStatement(name string) {
	conn := call.conn
	if _, ok := conn.statements[name]; ok {
		delete(conn.statements, name)
		call.tracker.statements = max(0, call.tracker.statements-1)
	}
}

// bindPortal은 portal이 가리키는 statement 이름만 둔다. parameter 값은 읽지 않는다. 모든 연결의 합계가 상한을 넘으면 모두
// 비운다. 이름 있는 portal은 트랜잭션이 끝나야 닫히므로 연결마다 세면 연결 상한과 곱해져 메모리가 커진다.
func (call *postgresCall) bindPortal(portal, statement string) {
	conn, tracker := call.conn, call.tracker
	if _, ok := conn.portals[portal]; !ok {
		if tracker.portals >= postgresPortalLimit {
			for _, other := range tracker.conns {
				other.portals = nil
			}
			tracker.portals = 0
		}
		tracker.portals++
	}
	if conn.portals == nil {
		conn.portals = map[string]string{}
	}
	conn.portals[portal] = statement
}

func (call *postgresCall) forgetPortal(name string) {
	conn := call.conn
	if _, ok := conn.portals[name]; ok {
		delete(conn.portals, name)
		call.tracker.portals = max(0, call.tracker.portals-1)
	}
}

// frontend는 요청 방향 message 하나를 읽는다.
func (call *postgresCall) frontend(message postgresMessage) bool {
	conn, tracker := call.conn, call.tracker
	if message.typ == 0 {
		return call.startMessage(message)
	}
	size := 5 + message.length
	switch message.typ {
	case 'Q':
		conn.batch = nil
		unit := &postgresUnit{at: message.at, bytes: size, info: tracePostgresEvent{Command: postgresCommandQuery}}
		sql, _, _ := postgresCString(message.body)
		unit.info.SQL, unit.info.SQLTruncated = tracker.sqlText(sql, message.truncated, conn.strings)
		call.enqueue(unit, postgresEventQuery)
	case 'F':
		conn.batch = nil
		call.enqueue(&postgresUnit{at: message.at, bytes: size, info: tracePostgresEvent{Command: postgresCommandFunction}}, postgresEventQuery)
	case 'P':
		batch := conn.openBatch(message.at)
		batch.bytes += size
		name, rest, ok := postgresCString(message.body)
		if !ok {
			return true
		}
		sql, _, closed := postgresCString(rest)
		var statement postgresStatement
		statement.sql, statement.truncated = tracker.sqlText(sql, !closed, conn.strings)
		call.remember(postgresName(name), statement)
		if !batch.parsed {
			batch.parsed, batch.prepareSQL, batch.prepareCut, batch.prepareName = true, statement.sql, statement.truncated, postgresName(name)
		}
	case 'B':
		conn.openBatch(message.at).bytes += size
		portal, rest, ok := postgresCString(message.body)
		statement, _, closed := postgresCString(rest)
		if ok && closed {
			call.bindPortal(postgresName(portal), postgresName(statement))
		}
	case 'E':
		batch := conn.openBatch(message.at)
		batch.bytes += size
		batch.info.Executes++
		if portal, _, ok := postgresCString(message.body); ok && batch.info.Executes == 1 {
			if name, bound := conn.portals[postgresName(portal)]; bound {
				statement := conn.statements[name]
				batch.info.SQL, batch.info.SQLTruncated, batch.info.Statement = statement.sql, statement.truncated, name
			}
		}
	case 'C':
		conn.openBatch(message.at).bytes += size
		if len(message.body) >= 1 {
			if name, _, ok := postgresCString(message.body[1:]); ok {
				switch message.body[0] {
				case 'S':
					call.forgetStatement(postgresName(name))
				case 'P':
					call.forgetPortal(postgresName(name))
				}
			}
		}
	case 'D', 'H':
		conn.openBatch(message.at).bytes += size
	case 'c', 'f':
		if conn.copyIn {
			conn.copyIn, conn.copySync = false, true
		}
	case 'S':
		call.forgetPortal("")
		if conn.copyIn || conn.copySync {
			conn.copySync, conn.batch = false, nil
			return true
		}
		batch := conn.openBatch(message.at)
		conn.batch = nil
		batch.bytes += size
		switch {
		case batch.info.Executes > 0:
			batch.info.Command = postgresCommandExecute
		case batch.parsed:
			batch.info.Command, batch.info.SQL, batch.info.SQLTruncated, batch.info.Statement = postgresCommandPrepare, batch.prepareSQL, batch.prepareCut, batch.prepareName
		default:
			batch.silent = true
		}
		call.enqueue(batch, postgresEventQuery)
	case 'X':
		conn.batch = nil
	}
	return true
}

// startMessage는 type byte 없는 시작 message를 읽는다. StartupMessage에서는 user와 database만 꺼낸다. options와
// application_name 같은 다른 값은 읽지 않는다. CancelRequest의 key는 읽지 않는다.
func (call *postgresCall) startMessage(message postgresMessage) bool {
	conn := call.conn
	if len(message.body) < 4 {
		conn.closed = true
		return false
	}
	switch code := binary.BigEndian.Uint32(message.body[:4]); {
	case code == postgresCodeSSL:
		conn.answer = 'S'
		return true
	case code == postgresCodeGSS:
		conn.answer = 'G'
		return true
	case code>>16 != 3:
		conn.closed = true
		return false
	}
	conn.request.untyped, conn.startup = false, true
	info := tracePostgresEvent{Command: postgresCommandConnect}
	for rest := message.body[4:]; len(rest) > 0 && rest[0] != 0; {
		key, after, ok := postgresCString(rest)
		if !ok {
			break
		}
		value, next, ok := postgresCString(after)
		if !ok {
			break
		}
		switch string(key) {
		case "user":
			info.User = postgresName(value)
		case "database":
			info.Database = postgresName(value)
		}
		rest = next
	}
	if info.Database == "" {
		// 서버는 database가 없으면 user 이름의 database를 연다.
		info.Database = info.User
	}
	call.enqueue(&postgresUnit{at: message.at, bytes: 4 + message.length, info: info}, postgresEventConnect)
	return true
}

// backend는 응답 방향 message 하나를 읽는다. 응답 값은 줄의 첫 단위에 모으고 ReadyForQuery가 그 단위를 닫는다.
// NoticeResponse, NotificationResponse, ParameterStatus는 단위를 닫지 않는다.
func (call *postgresCall) backend(message postgresMessage) bool {
	conn := call.conn
	var head *postgresUnit
	if len(conn.queue) > 0 && conn.skip == 0 {
		head = conn.queue[0]
		head.responseBytes += 5 + message.length
	}
	switch message.typ {
	case 'Z':
		status := byte(0)
		if len(message.body) == 1 {
			status = message.body[0]
		}
		call.ready(status)
		switch {
		case conn.skip > 0:
			conn.skip--
		case head != nil:
			call.finish(head, message.at)
		}
	case 'C':
		if head != nil {
			tag, _, _ := postgresCString(message.body)
			head.tag = postgresName(tag)
		}
	case 'D':
		if head != nil {
			head.rows++
		}
	case 'T':
		if head != nil {
			head.rowsSeen = true
		}
	case 'G':
		conn.copyIn = head != nil && !head.silent && head.info.Command == postgresCommandExecute
	case 'E':
		severity, state, text, detail := call.errorFields(message.body)
		if head == nil {
			return true
		}
		if !head.failed {
			head.failed, head.severity, head.sqlState, head.message, head.detail = true, severity, state, text, detail
		}
		// 인증 중 오류와 FATAL은 ReadyForQuery 없이 연결을 끝낸다.
		if conn.startup || severity == "FATAL" || severity == "PANIC" {
			call.finish(head, message.at)
		}
	case 'S':
		name, rest, ok := postgresCString(message.body)
		value, _, closed := postgresCString(rest)
		if !ok || !closed {
			return true
		}
		switch string(name) {
		case "server_version":
			conn.serverVersion = postgresName(value)
		case "standard_conforming_strings":
			conn.strings = postgresStringsOn
			if string(value) == "off" {
				conn.strings = postgresStringsOff
			}
		}
	}
	return true
}

// errorFields는 ErrorResponse에서 severity, SQLSTATE, message를 꺼낸다. detail은 행 값을 담을 수 있어 --show-secrets일
// 때만 꺼낸다. hint, where, internal query 같은 다른 field는 읽지 않는다.
func (call *postgresCall) errorFields(body []byte) (severity, state, text, detail string) {
	localized := ""
	for len(body) > 0 && body[0] != 0 {
		kind := body[0]
		value, rest, ok := postgresCString(body[1:])
		switch kind {
		case 'S':
			localized = postgresName(value)
		case 'V':
			severity = postgresName(value)
		case 'C':
			state = postgresName(value)
		case 'M':
			text = traceEscapeText([]byte(traceTrimText(string(value), postgresMessageLimit)))
		case 'D':
			if call.tracker.showSecrets {
				detail = traceEscapeText([]byte(traceTrimText(string(value), postgresMessageLimit)))
			}
		}
		if !ok {
			break
		}
		body = rest
	}
	if severity == "" {
		severity = localized
	}
	return severity, state, text, detail
}

// finish는 단위를 줄에서 빼고 응답 event를 낸다. 응답 시간은 단위의 첫 요청 record부터 이 record까지다.
func (call *postgresCall) finish(unit *postgresUnit, at uint64) {
	conn := call.conn
	conn.queue[0] = nil
	conn.queue = conn.queue[1:]
	call.tracker.pending = max(0, call.tracker.pending-1)
	if unit.silent {
		return
	}
	info := unit.info
	name := postgresEventResult
	if unit.failed {
		name = postgresEventError
		info.Severity, info.SQLState, info.Message, info.Detail = unit.severity, unit.sqlState, unit.message, unit.detail
	} else {
		info.Tag = unit.tag
		if info.Command == postgresCommandConnect {
			info.ServerVersion = conn.serverVersion
		}
	}
	rows, rowsKnown := unit.rows, unit.rowsSeen || unit.rows > 0
	if unit.lost {
		rows, rowsKnown = postgresTagRows(unit.tag)
	}
	if rowsKnown {
		info.Rows = &rows
	}
	event := call.event(name, at, unit.responseBytes, &info)
	event.LatencyMS, event.answered = traceSpan(unit.at, at), 1
	call.out = append(call.out, event)
}

// postgresStrings는 연결의 standard_conforming_strings다. 꺼져 있으면 '...' 안의 backslash가 escape다.
type postgresStrings uint8

const (
	postgresStringsUnknown postgresStrings = iota
	postgresStringsOn
	postgresStringsOff
)

// sqlText는 event에 붙일 SQL이다. 문자열 literal은 --show-secrets가 아니면 가린다.
func (tracker *postgresTracker) sqlText(raw []byte, truncated bool, strings postgresStrings) (string, bool) {
	if !tracker.showSecrets {
		raw = postgresMask(raw, strings)
	}
	text := string(raw)
	if len(text) > postgresSQLLimit {
		text, truncated = traceTrimText(text, postgresSQLLimit), true
	}
	return traceEscapeText([]byte(text)), truncated
}

// postgresIdentifierByte는 PostgreSQL identifier를 잇는 byte다. $는 첫 byte가 아닐 때만 identifier에 들어간다.
func postgresIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= 0x80 || (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

func postgresTagStart(value byte) bool {
	return value == '_' || value >= 0x80 || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

// postgresLiteralEnd는 i의 따옴표에서 시작하는 문자열의 끝과 닫혔는지를 돌려준다. 두 번 쓴 따옴표는 문자열 안의
// 따옴표다. backslash는 E'...'와 standard_conforming_strings=off일 때만 escape다.
func postgresLiteralEnd(sql []byte, i int, backslash bool) (int, bool) {
	for i++; i < len(sql); i++ {
		switch {
		case backslash && sql[i] == '\\':
			i++
		case sql[i] == '\'' && i+1 < len(sql) && sql[i+1] == '\'':
			i++
		case sql[i] == '\'':
			return i + 1, true
		}
	}
	return len(sql), false
}

// postgresQuotedEnd는 "..." identifier의 끝이다. 두 번 쓴 따옴표는 identifier 안의 따옴표다.
func postgresQuotedEnd(sql []byte, i int) int {
	for i++; i < len(sql); i++ {
		if sql[i] == '"' {
			if i+1 < len(sql) && sql[i+1] == '"' {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}

// postgresCommentEnd는 i에서 주석이 시작하면 그 끝을, 아니면 i를 돌려준다. PostgreSQL의 /* */는 중첩된다.
func postgresCommentEnd(sql []byte, i int) int {
	switch {
	case sql[i] == '-' && i+1 < len(sql) && sql[i+1] == '-':
		if end := bytes.IndexByte(sql[i:], '\n'); end >= 0 {
			return i + end
		}
		return len(sql)
	case sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*':
		depth := 0
		for j := i; j+1 < len(sql); {
			switch {
			case sql[j] == '/' && sql[j+1] == '*':
				depth, j = depth+1, j+2
			case sql[j] == '*' && sql[j+1] == '/':
				depth, j = depth-1, j+2
				if depth == 0 {
					return j
				}
			default:
				j++
			}
		}
		return len(sql)
	}
	return i
}

// postgresDollarTag는 i의 $에서 시작하는 dollar quote 여는 tag($$나 $tag$)의 끝을 돌려준다. $1 같은 parameter와
// identifier 안의 $는 tag가 아니다.
func postgresDollarTag(sql []byte, i int) (int, bool) {
	if i > 0 && postgresIdentifierByte(sql[i-1]) {
		return 0, false
	}
	j := i + 1
	if j < len(sql) && sql[j] == '$' {
		return j + 1, true
	}
	if j >= len(sql) || !postgresTagStart(sql[j]) {
		return 0, false
	}
	for j++; j < len(sql) && sql[j] != '$'; j++ {
		if !postgresIdentifierByte(sql[j]) {
			return 0, false
		}
	}
	if j >= len(sql) {
		return 0, false
	}
	return j + 1, true
}

// postgresMaskSQL은 문자열 상수와 dollar quote 본문을 ?로 바꾼다. 닫히지 않은 것은 끝까지 가린다. "..." identifier,
// 주석, 숫자, $1 parameter는 그대로 둔다. E'...', U&'...', B'...', X'...', N'...'은 앞 글자를 남기고 본문만 가린다.
// MySQL과 달리 standard_conforming_strings=on이면 '...' 안의 backslash는 escape가 아니다.
func postgresMaskSQL(sql []byte, standardStrings bool) []byte {
	if standardStrings {
		return postgresMask(sql, postgresStringsOn)
	}
	return postgresMask(sql, postgresStringsOff)
}

// postgresMask는 postgresMaskSQL과 같고, standard_conforming_strings를 모를 때도 가린다.
func postgresMask(sql []byte, strings postgresStrings) []byte {
	out := make([]byte, 0, len(sql))
	literal := func(i int, backslash bool) int {
		end, closed := postgresLiteralEnd(sql, i, backslash)
		// 설정을 모르는데 '...' 안에 backslash가 있으면 두 해석에서 literal의 끝이 다를 수 있다. 앞부분은 두 해석이 같으므로
		// 그 literal부터 끝까지 가린다.
		if strings == postgresStringsUnknown && !backslash && bytes.IndexByte(sql[i+1:end], '\\') >= 0 {
			end, closed = len(sql), false
		}
		out = append(out, '\'', '?')
		if closed {
			out = append(out, '\'')
		}
		return end
	}
	for i := 0; i < len(sql); {
		value := sql[i]
		tokenStart := i == 0 || !postgresIdentifierByte(sql[i-1])
		next := func(n int) byte {
			if i+n < len(sql) {
				return sql[i+n]
			}
			return 0
		}
		switch {
		case value == '\'':
			i = literal(i, strings == postgresStringsOff)
		case tokenStart && (value == 'E' || value == 'e') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, true)
		case tokenStart && (value == 'U' || value == 'u') && next(1) == '&' && next(2) == '\'':
			out = append(out, value, '&')
			i = literal(i+2, false)
		case tokenStart && (value == 'B' || value == 'b' || value == 'X' || value == 'x') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, false)
		case tokenStart && (value == 'N' || value == 'n') && next(1) == '\'':
			out = append(out, value)
			i = literal(i+1, strings == postgresStringsOff)
		case value == '"':
			end := postgresQuotedEnd(sql, i)
			out = append(out, sql[i:end]...)
			i = end
		case value == '$':
			tagEnd, ok := postgresDollarTag(sql, i)
			if !ok {
				out = append(out, value)
				i++
				continue
			}
			tag := sql[i:tagEnd]
			out = append(out, tag...)
			out = append(out, '?')
			if end := bytes.Index(sql[tagEnd:], tag); end >= 0 {
				out = append(out, tag...)
				i = tagEnd + end + len(tag)
			} else {
				i = len(sql)
			}
		case tokenStart && postgresIdentifierByte(value):
			// identifier와 숫자는 통째로 넘긴다. 그래야 abcE'..'의 E를 접두어로 읽지 않는다.
			end := i + 1
			for end < len(sql) && postgresIdentifierByte(sql[end]) {
				end++
			}
			out = append(out, sql[i:end]...)
			i = end
		default:
			if end := postgresCommentEnd(sql, i); end > i {
				out = append(out, sql[i:end]...)
				i = end
				continue
			}
			out = append(out, value)
			i++
		}
	}
	return out
}

// postgresScrollLabelSQL은 스크롤 행에 쓰는 SQL의 길이 상한이다. 전체는 --raw와 상세 보기에 있다.
const postgresScrollLabelSQL = 200

func postgresResponseEvent(name string) bool {
	return name == postgresEventResult || name == postgresEventError
}

// tracePostgresShape는 SQL에서 값을 뺀 모양이다. 문자열, dollar quote, 숫자 literal을 ?로, 연속 공백을 한 칸으로 바꾼다.
// $1 parameter와 identifier 안의 숫자는 그대로 둔다. --show-secrets로 본 SQL과 가린 SQL이 같은 shape를 만든다.
func tracePostgresShape(sql string) string {
	text := postgresMask([]byte(sql), postgresStringsUnknown)
	digit := func(value byte) bool { return value >= '0' && value <= '9' }
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
		case value == '\'':
			end, _ := postgresLiteralEnd(text, i, false)
			out.WriteByte('?')
			i = end
		case value == '"':
			end := postgresQuotedEnd(text, i)
			out.Write(text[i:end])
			i = end
		case value == '$' && i+1 < len(text) && digit(text[i+1]):
			end := i + 1
			for end < len(text) && digit(text[end]) {
				end++
			}
			out.Write(text[i:end])
			i = end
		case digit(value):
			end := i
			for end < len(text) && (digit(text[end]) || text[end] == '.') {
				end++
			}
			if end+1 < len(text) && (text[end] == 'e' || text[end] == 'E') && (digit(text[end+1]) || text[end+1] == '+' || text[end+1] == '-') {
				for end += 2; end < len(text) && digit(text[end]); end++ {
				}
			}
			if end < len(text) && postgresIdentifierByte(text[end]) {
				for ; end < len(text) && postgresIdentifierByte(text[end]); end++ {
				}
				out.Write(text[i:end])
			} else {
				out.WriteByte('?')
			}
			i = end
		case postgresIdentifierByte(value):
			end := i
			for end < len(text) && postgresIdentifierByte(text[end]) {
				end++
			}
			out.Write(text[i:end])
			i = end
		default:
			if end := postgresCommentEnd(text, i); end > i {
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

// tracePostgresCounts는 PostgreSQL group 행과 요약이 함께 쓰는 값이다. postgres_tls는 요청으로 세지 않고 연결로만 센다.
type tracePostgresCounts struct {
	Commands       uint64   `json:"commands"`
	Responses      uint64   `json:"responses"`
	Errors         uint64   `json:"errors"`
	Unanswered     uint64   `json:"unanswered"`
	LatencyAvgMS   *float64 `json:"latency_avg_ms"`
	LatencyMaxMS   *float64 `json:"latency_max_ms"`
	Connections    uint64   `json:"connections,omitempty"`
	TLSConnections uint64   `json:"tls_connections,omitempty"`
	answered       uint64
	latency        traceSpans
}

func (counts *tracePostgresCounts) observe(event captureEvent) {
	switch {
	case event.Event == postgresEventTLS:
		counts.Connections++
		counts.TLSConnections++
	case postgresResponseEvent(event.Event):
		counts.Responses++
		if event.Event == postgresEventError {
			counts.Errors++
		}
		counts.answered += event.answered
		counts.latency.observe(event.LatencyMS)
	default:
		counts.Commands++
		if event.Event == postgresEventConnect {
			counts.Connections++
		}
	}
}

func (counts tracePostgresCounts) finished() tracePostgresCounts {
	counts.Unanswered = counts.Commands - min(counts.Commands, counts.answered)
	counts.LatencyAvgMS, counts.LatencyMaxMS = counts.latency.summary()
	return counts
}

func tracePostgresGroupSummary(counts *tracePostgresCounts) *tracePostgresCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

func tracePostgresColumn(value func(counts tracePostgresCounts) string) func(traceGroupSummary) string {
	return func(group traceGroupSummary) string {
		if group.Postgres == nil {
			return "-"
		}
		return value(*group.Postgres)
	}
}

var tracePostgresGroupColumns = []traceGroupColumn{
	{screenTitle: "CMD", reportTitle: "COMMANDS", width: 5, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return strconv.FormatUint(counts.Commands, 10) })},
	{screenTitle: "RSP", reportTitle: "RESPONSES", width: 5, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return strconv.FormatUint(counts.Responses, 10) })},
	{screenTitle: "ERR", reportTitle: "ERRORS", width: 4, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return strconv.FormatUint(counts.Errors, 10) })},
	{screenTitle: "NOANS", reportTitle: "UNANSWERED", width: 5, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return strconv.FormatUint(counts.Unanswered, 10) })},
	{screenTitle: "AVGms", reportTitle: "AVG_MS", width: 6, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return traceLatency(counts.LatencyAvgMS, "") })},
	{screenTitle: "MAXms", reportTitle: "MAX_MS", width: 6, value: tracePostgresColumn(func(counts tracePostgresCounts) string { return traceLatency(counts.LatencyMaxMS, "") })},
}

// tracePostgresCommandText는 event가 어떤 요청 단위인지 한 줄로 쓴다. 응답 event도 답한 요청의 명령을 쓴다.
func tracePostgresCommandText(event captureEvent) string {
	info := event.Postgres
	sql := traceMySQLLine(traceTrimText(info.SQL, postgresScrollLabelSQL))
	if len(info.SQL) > postgresScrollLabelSQL {
		sql += "..."
	}
	switch {
	case event.Event == postgresEventTLS:
		return "tls " + info.Encryption
	case info.Command == postgresCommandConnect:
		return emptyAs(strings.Join(slices.DeleteFunc([]string{info.User, info.Database}, func(value string) bool { return value == "" }), " "), postgresCommandConnect)
	case info.Command == postgresCommandPrepare || info.Command == postgresCommandExecute:
		return strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{info.Command, info.Statement, sql}, func(value string) bool { return value == "" }), " "))
	}
	return emptyAs(sql, info.Command)
}

// tracePostgresResultText는 응답 event의 결과를 쓴다.
func tracePostgresResultText(event captureEvent) string {
	info := event.Postgres
	if event.Event == postgresEventError {
		return strings.TrimSpace(fmt.Sprintf("%s %s %s", emptyAs(info.Severity, "-"), emptyAs(info.SQLState, "-"), traceMySQLLine(info.Message)))
	}
	switch {
	case info.Command == postgresCommandConnect:
		return strings.TrimSpace("ready " + info.ServerVersion)
	case info.Tag != "":
		return info.Tag
	case info.Rows != nil:
		return fmt.Sprintf("%d rows", *info.Rows)
	}
	return "ok"
}

// tracePostgresScrollLabels는 목적지 칸에 쪽, 명령, 응답이면 결과를, event 칸에 event 이름과 응답 시간을 쓴다.
func tracePostgresScrollLabels(event captureEvent) (string, string) {
	label := event.Event
	if event.LatencyMS != nil {
		label += " " + traceLatency(event.LatencyMS, "ms")
	}
	if event.Postgres == nil {
		return emptyAs(event.Destination, "-"), label
	}
	text := tracePostgresCommandText(event)
	if postgresResponseEvent(event.Event) {
		text = "↳ " + tracePostgresResultText(event)
	}
	if event.Side != "" {
		text = event.Side + ": " + text
	}
	return text, label
}

type postgresTraceKey struct {
	side    string
	command string
	shape   string
}

type postgresTraceRow struct {
	Side      string   `json:"side,omitempty"`
	Command   string   `json:"command"`
	Shape     string   `json:"shape"`
	Processes []string `json:"processes,omitempty"`
	tracePostgresCounts
}

type postgresTraceReport struct {
	Side       string             `json:"side,omitempty"`
	DurationMS int64              `json:"duration_ms"`
	LostEvents uint64             `json:"lost_events"`
	Statements []postgresTraceRow `json:"statements"`
	tracePostgresCounts
	// Client와 Server는 두 쪽이 섞인 trace의 쪽별 합계다. 두 쪽의 응답 시간은 뜻이 달라 합친 평균은 뜻이 없다.
	Client *tracePostgresCounts `json:"client,omitempty"`
	Server *tracePostgresCounts `json:"server,omitempty"`
}

type postgresTraceRowStats struct {
	processes map[string]struct{}
	counts    tracePostgresCounts
}

// postgresTraceSummarizer는 --group-by 없이 끝난 PostgreSQL trace를 쪽, 명령, SQL shape마다 한 행으로 묶는다.
type postgresTraceSummarizer struct {
	rows   map[postgresTraceKey]*postgresTraceRowStats
	counts tracePostgresCounts
	sides  map[string]*tracePostgresCounts
}

func newPostgresTraceSummarizer() *postgresTraceSummarizer {
	return &postgresTraceSummarizer{rows: map[postgresTraceKey]*postgresTraceRowStats{}, sides: map[string]*tracePostgresCounts{}}
}

// postgresTraceShape는 요약 행의 SQL 자리다. SQL이 없는 요청은 그 사정을 쓴다.
func postgresTraceShape(info *tracePostgresEvent) string {
	switch {
	case info.Command == postgresCommandConnect:
		return "(connect)"
	case info.Command == postgresCommandFunction:
		return "(function call)"
	case info.Command == postgresCommandExecute && info.SQL == "":
		return "(statement not seen)"
	case info.SQL == "":
		return "(no SQL)"
	}
	return traceMySQLLine(tracePostgresShape(info.SQL))
}

func (summarizer *postgresTraceSummarizer) observe(event captureEvent) {
	if event.Postgres == nil {
		return
	}
	summarizer.counts.observe(event)
	if event.Side != "" {
		counts := summarizer.sides[event.Side]
		if counts == nil {
			counts = &tracePostgresCounts{}
			summarizer.sides[event.Side] = counts
		}
		counts.observe(event)
	}
	// 암호화한 연결은 요청이 없어서 합계에만 센다.
	if event.Event == postgresEventTLS {
		return
	}
	key := postgresTraceKey{side: event.Side, command: event.Postgres.Command, shape: postgresTraceShape(event.Postgres)}
	stats := summarizer.rows[key]
	if stats == nil {
		stats = &postgresTraceRowStats{processes: map[string]struct{}{}}
		summarizer.rows[key] = stats
	}
	if event.Process != "" {
		stats.processes[event.Process] = struct{}{}
	}
	stats.counts.observe(event)
}

func (summarizer *postgresTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := postgresTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, tracePostgresCounts: summarizer.counts.finished()}
	if len(summarizer.sides) == 1 {
		for side := range summarizer.sides {
			report.Side = side
		}
	} else if len(summarizer.sides) > 1 {
		report.Client, report.Server = tracePostgresGroupSummary(summarizer.sides[traceClientSide]), tracePostgresGroupSummary(summarizer.sides[traceServerSide])
	}
	report.Statements = make([]postgresTraceRow, 0, len(summarizer.rows))
	for key, stats := range summarizer.rows {
		report.Statements = append(report.Statements, postgresTraceRow{Side: key.side, Command: key.command, Shape: key.shape, Processes: slices.Sorted(maps.Keys(stats.processes)), tracePostgresCounts: stats.counts.finished()})
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

// -d는 연결마다 한 행을 쓰는 option이다. PostgreSQL 요약은 SQL 모양마다 한 행이라 같은 표를 쓴다.
func (report postgresTraceReport) print(bool) {
	title := "PostgreSQL trace"
	if report.Side == traceServerSide {
		title = "PostgreSQL server trace"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n\n", title, (time.Duration(report.DurationMS) * time.Millisecond).String())
	mixed := report.Client != nil && report.Server != nil
	if mixed {
		for _, side := range []struct {
			heading string
			counts  *tracePostgresCounts
		}{{"Client side (requests that this host sent):", report.Client}, {"Server side (requests that local servers received):", report.Server}} {
			fmt.Fprintln(os.Stdout, side.heading)
			printPostgresCounts("  ", *side.counts)
			fmt.Fprintln(os.Stdout)
		}
	} else {
		printPostgresCounts("", report.tracePostgresCounts)
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

func printPostgresCounts(indent string, counts tracePostgresCounts) {
	for _, line := range []string{
		fmt.Sprintf("Commands: %d", counts.Commands),
		fmt.Sprintf("Responses: %d", counts.Responses),
		fmt.Sprintf("Errors: %d", counts.Errors),
		fmt.Sprintf("Unanswered: %d", counts.Unanswered),
		"Latency avg: " + traceLatency(counts.LatencyAvgMS, "ms"),
		"Latency max: " + traceLatency(counts.LatencyMaxMS, "ms"),
		fmt.Sprintf("Connections: %d", counts.Connections),
		fmt.Sprintf("TLS connections: %d", counts.TLSConnections),
	} {
		fmt.Fprintln(os.Stdout, indent+line)
	}
}

func tracePostgresDetailLines(event captureEvent) []string {
	info := event.Postgres
	lines := []string{
		"event: " + event.Event,
		"time: " + traceEventClock(event),
		"side: " + emptyAs(event.Side, "-"),
		"process: " + emptyAs(event.Process, "-"),
		"source: " + emptyAs(event.Source, "-"),
		"destination: " + emptyAs(event.Destination, "-"),
		"bytes: " + strconv.FormatUint(event.Bytes, 10),
	}
	for _, field := range [][2]string{
		{"command", info.Command}, {"statement", info.Statement}, {"user", info.User}, {"database", info.Database},
		{"server_version", info.ServerVersion}, {"encryption", info.Encryption}, {"tag", info.Tag},
	} {
		if field[1] != "" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	if event.LatencyMS != nil {
		lines = append(lines, "latency: "+traceLatency(event.LatencyMS, "ms"))
	}
	if info.Executes > 1 {
		lines = append(lines, "executes: "+strconv.Itoa(info.Executes))
	}
	if info.Rows != nil {
		lines = append(lines, "rows: "+strconv.FormatUint(*info.Rows, 10))
	}
	if info.Severity != "" || info.SQLState != "" {
		lines = append(lines, "error: "+tracePostgresResultText(event))
	} else if event.Event == postgresEventTLS && info.Message != "" {
		lines = append(lines, "note: "+info.Message)
	}
	if info.Detail != "" {
		lines = append(lines, "detail: "+info.Detail)
	}
	if info.SQL != "" {
		lines = append(lines, "", "sql:")
		lines = append(lines, strings.Split(info.SQL, "\n")...)
		if info.SQLTruncated {
			lines = append(lines, "(truncated)")
		}
	}
	return lines
}
