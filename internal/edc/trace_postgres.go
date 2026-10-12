package edc

import (
	"bytes"
	"encoding/binary"
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
			switch {
			case stream.untyped && (length < 8 || length > postgresStartupLimit):
				return false
			case !stream.untyped && (length < 4 || length > postgresMaxLength || !postgresTypeValid(frontend, typ)):
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
		if size == 0 || !postgresCleanStart(data, size, false) {
			return
		}
		conn.response = postgresStream{synced: true}
		// 경계를 잃은 동안 건너뛴 응답 record는 앞 요청의 꼬리일 수 있다. 그 뒤에 들어온 단위만 이 record부터 짝짓는다.
		// 남는 위험: pipeline client가 잘린 응답의 꼬리를 받기 전에 새 요청을 보내고, 그 꼬리가 깨끗한 record 하나로
		// 오면 그 꼬리의 ReadyForQuery가 새 요청을 닫는다. 이 경우 응답 시간이 틀린다.
		// 건너뛸 ReadyForQuery가 건너뛴 record에 있었는지 모르므로 skip도 비운다.
		call.dropBefore(seen)
		conn.skip = 0
	}
	if !conn.response.feed(data, size, record.bootTimeNS, false, call.backend) {
		call.lose(false)
	}
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
	conn.queue, conn.skip = nil, 0
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
			other.queue = nil
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
		conn.startup, conn.copyIn = false, false
		if len(message.body) == 1 && message.body[0] == 'I' {
			// 트랜잭션 밖에서는 이름 있는 portal도 모두 닫힌다.
			call.tracker.portals = max(0, call.tracker.portals-len(conn.portals))
			conn.portals = nil
		}
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
	if unit.rowsSeen || unit.rows > 0 {
		rows := unit.rows
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
