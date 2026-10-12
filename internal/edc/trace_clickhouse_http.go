package edc

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	// clickHouseHTTPQueueLimit은 socket마다 응답을 기다리는 요청의 상한이다. 넘으면 가장 오래된 요청을 응답 없이 버린다.
	clickHouseHTTPQueueLimit = 16
	// clickHouseHTTPLineLimit은 record 사이에 걸친 미완성 줄을 들고 있는 상한이다.
	clickHouseHTTPLineLimit = 16 << 10
	// clickHouseHTTPChunkLineLimit은 chunk 크기 줄과 trailer 줄의 상한이다.
	clickHouseHTTPChunkLineLimit = 64
	// clickHouseHTTPBodyLimit은 SQL을 만들려고 모으는 요청 body의 상한이다.
	clickHouseHTTPBodyLimit = 4096
	clickHouseHTTPNameLimit = 64
	clickHouseHTTPTagLength = 16
)

var clickHouseErrorNamePattern = regexp.MustCompile(`\(([A-Z][A-Z0-9_]{2,})\)`)

// clickHouseHTTPURLKeys는 URL query string에서 값을 읽는 key다. 나머지(password, quota_key, session_id, param_*, setting)는
// 값을 읽지도 보관하지도 않는다.
var clickHouseHTTPURLKeys = []string{"query", "user", "database", "query_id", "decompress"}

// clickHouseHTTPRequestHeader와 clickHouseHTTPResponseHeader는 값을 읽는 header다. 나머지(X-ClickHouse-Key,
// X-ClickHouse-Quota, Cookie)는 줄째로 건너뛰어 record 사이에 걸쳐도 메모리에 남지 않는다.
func clickHouseHTTPRequestHeader(name string) bool {
	switch name {
	case "content-length", "transfer-encoding", "content-type", "content-encoding", "x-clickhouse-user", "x-clickhouse-database", "x-clickhouse-query-id", "authorization":
		return true
	}
	return false
}

func clickHouseHTTPResponseHeader(name string) bool {
	switch name {
	case "content-length", "transfer-encoding", "x-clickhouse-exception-code", "x-clickhouse-exception-tag", "x-clickhouse-query-id":
		return true
	}
	return false
}

type clickHouseHTTPLineMode uint8

const (
	httpLineIdle clickHouseHTTPLineMode = iota
	// httpLineWhole은 요청 줄이나 상태 줄을 통째로 보관한다.
	httpLineWhole
	// httpLineName은 header 이름을 읽는 중이다. ':'를 보기 전에는 값이 없다.
	httpLineName
	// httpLineValue는 값을 읽는 header를 보관하는 중이다.
	httpLineValue
	// httpLineSkip은 값을 읽지 않는 header다. LF까지 보관 없이 버린다.
	httpLineSkip
)

type clickHouseHTTPLineResult uint8

const (
	httpLineMore clickHouseHTTPLineResult = iota
	// httpLineDone은 보관하는 줄 하나를 끝까지 읽었다는 뜻이다.
	httpLineDone
	// httpLineSkipped는 값을 읽지 않는 header 줄 하나를 지났다는 뜻이다.
	httpLineSkipped
	// httpLineLost는 줄의 끝이 캡처 밖이거나 형식이 틀리거나 상한을 넘었다는 뜻이다.
	httpLineLost
)

// clickHouseHTTPLine은 record 사이에 걸친 줄 하나를 이어 읽는다. 줄이 record 하나에 들어 있으면 복사하지 않고 record를
// 그대로 돌려준다. 이어 읽는 줄에는 비밀값이 들어갈 수 있으므로 보관하는 부분을 줄인다. header는 이름이 허용 목록에 있을
// 때만 보관하고, 요청 줄은 허용한 URL parameter만 남긴다.
type clickHouseHTTPLine struct {
	buf  []byte
	mode clickHouseHTTPLineMode
	// dropPair는 요청 줄에서 값을 보관하지 않는 parameter가 record 끝에서 끊겼다는 뜻이다. 다음 '&'나 공백까지 버린다.
	dropPair bool
}

func (line *clickHouseHTTPLine) reset() {
	clear(line.buf)
	line.buf, line.mode, line.dropPair = line.buf[:0], httpLineIdle, false
}

// release는 돌려준 줄을 다 쓴 뒤 보관한 byte를 지운다.
func (line *clickHouseHTTPLine) release() { line.reset() }

func clickHouseHTTPHeaderName(raw []byte) string {
	return strings.ToLower(strings.TrimSpace(string(raw)))
}

// read는 data[pos:]에서 줄 하나를 이어 읽는다. whole이면 요청 줄이나 상태 줄이고, 아니면 header다. 돌려주는 줄은 다음
// release나 reset까지만 유효하다.
func (line *clickHouseHTTPLine) read(data []byte, size, pos int, whole bool, allow func(string) bool) (clickHouseHTTPLineResult, []byte, int) {
	cut := len(data) < size
	for {
		visible := data[pos:]
		lf := bytes.IndexByte(visible, '\n')
		switch line.mode {
		case httpLineIdle:
			if lf >= 0 {
				text := visible[:lf+1]
				return line.classify(text, whole, allow), text, pos + lf + 1
			}
			line.begin(visible, whole, allow)
			if cut || len(line.buf) > clickHouseHTTPLineLimit {
				return line.lost(len(data))
			}
			return httpLineMore, nil, size
		case httpLineSkip:
			if lf < 0 {
				if cut {
					return line.lost(len(data))
				}
				return httpLineMore, nil, size
			}
			line.reset()
			return httpLineSkipped, nil, pos + lf + 1
		case httpLineName:
			stop := bytes.IndexAny(visible, ":\n")
			if stop < 0 {
				line.buf = append(line.buf, visible...)
				if cut || len(line.buf) > clickHouseHTTPNameLimit {
					return line.lost(len(data))
				}
				return httpLineMore, nil, size
			}
			if visible[stop] == '\n' {
				line.buf = append(line.buf, visible[:stop+1]...)
				text := line.buf
				if string(text) == "\r\n" {
					line.mode = httpLineIdle
					return httpLineDone, text, pos + stop + 1
				}
				return line.lost(len(data))
			}
			name := clickHouseHTTPHeaderName(slices.Concat(line.buf, visible[:stop]))
			if !allow(name) {
				line.reset()
				line.mode = httpLineSkip
			} else {
				line.buf = append(line.buf, visible[:stop+1]...)
				line.mode = httpLineValue
			}
			pos += stop + 1
		case httpLineWhole, httpLineValue:
			if line.dropPair {
				if end := bytes.IndexAny(visible, "& "); end >= 0 {
					pos, line.dropPair = pos+end, false
					continue
				}
				if cut {
					return line.lost(len(data))
				}
				return httpLineMore, nil, size
			}
			if lf >= 0 {
				if len(line.buf)+lf+1 > clickHouseHTTPLineLimit {
					return line.lost(len(data))
				}
				line.buf = append(line.buf, visible[:lf+1]...)
				line.mode = httpLineIdle
				return httpLineDone, line.buf, pos + lf + 1
			}
			line.buf = append(line.buf, visible...)
			line.retain()
			if cut || len(line.buf) > clickHouseHTTPLineLimit {
				return line.lost(len(data))
			}
			return httpLineMore, nil, size
		}
	}
}

// lost는 줄을 잃었다고 알린다. 요청 줄이면 보이는 앞부분을 함께 돌려주고, 쓴 뒤에 release해야 한다.
func (line *clickHouseHTTPLine) lost(end int) (clickHouseHTTPLineResult, []byte, int) {
	if line.mode == httpLineWhole {
		return httpLineLost, line.buf, end
	}
	line.reset()
	return httpLineLost, nil, end
}

// classify는 끝까지 읽은 줄을 보관할지 정한다. 줄 전체가 record 안에 있으므로 보관 없이 판단한다.
func (line *clickHouseHTTPLine) classify(text []byte, whole bool, allow func(string) bool) clickHouseHTTPLineResult {
	if whole {
		return httpLineDone
	}
	colon := bytes.IndexByte(text, ':')
	if colon < 0 {
		if string(text) == "\r\n" {
			return httpLineDone
		}
		return httpLineLost
	}
	if allow(clickHouseHTTPHeaderName(text[:colon])) {
		return httpLineDone
	}
	return httpLineSkipped
}

// begin은 record 끝에서 끊긴 줄을 시작한다. 보관해도 되는 앞부분만 남긴다.
func (line *clickHouseHTTPLine) begin(visible []byte, whole bool, allow func(string) bool) {
	if whole {
		line.mode, line.buf = httpLineWhole, append(line.buf[:0], visible...)
		line.retain()
		return
	}
	colon := bytes.IndexByte(visible, ':')
	switch {
	case colon < 0:
		line.mode, line.buf = httpLineName, append(line.buf[:0], visible...)
		if len(line.buf) > clickHouseHTTPNameLimit {
			line.reset()
			line.mode = httpLineSkip
		}
	case !allow(clickHouseHTTPHeaderName(visible[:colon])):
		line.mode = httpLineSkip
	default:
		line.mode, line.buf = httpLineValue, append(line.buf[:0], visible...)
		line.retain()
	}
}

// retain은 record 끝에서 끊긴 줄에서 비밀값이 될 수 있는 부분을 지운다. Authorization 값은 끝까지 모아야 풀 수 있어서
// 이어 읽지 않고 버리고, 요청 줄은 허용한 parameter만 남긴다.
func (line *clickHouseHTTPLine) retain() {
	switch line.mode {
	case httpLineValue:
		if len(line.buf) >= len("authorization:") && strings.EqualFold(string(line.buf[:len("authorization:")]), "authorization:") {
			line.reset()
			line.mode = httpLineSkip
		}
	case httpLineWhole:
		line.scrubTarget()
	}
}

// scrubTarget은 끊긴 요청 줄의 query string에서 허용한 parameter만 남긴다. 끊긴 마지막 parameter는 허용한 key로 이어질
// 수 있을 때만 남기고, 아니면 다음 구분자까지 버린다.
func (line *clickHouseHTTPLine) scrubTarget() {
	question := bytes.IndexByte(line.buf, '?')
	if question < 0 {
		return
	}
	query, tail, complete := line.buf[question+1:], []byte(nil), false
	if space := bytes.IndexByte(query, ' '); space >= 0 {
		query, tail, complete = query[:space], query[space:], true
	}
	var kept []string
	pairs := bytes.Split(query, []byte("&"))
	for i, pair := range pairs {
		key, _, hasValue := bytes.Cut(pair, []byte("="))
		open := i == len(pairs)-1 && !complete
		keep := slices.Contains(clickHouseHTTPURLKeys, string(key))
		if open && !hasValue {
			keep = slices.ContainsFunc(clickHouseHTTPURLKeys, func(name string) bool { return strings.HasPrefix(name, string(key)) })
		}
		switch {
		case keep:
			kept = append(kept, string(pair))
		case open:
			line.dropPair = true
		}
	}
	scrubbed := append(slices.Clone(line.buf[:question+1]), strings.Join(kept, "&")...)
	scrubbed = append(scrubbed, tail...)
	clear(line.buf)
	line.buf = append(line.buf[:0], scrubbed...)
	clear(scrubbed)
}

type httpChunkState uint8

const (
	httpChunkSize httpChunkState = iota
	httpChunkData
	httpChunkCRLF
	httpChunkTrailer
)

type httpWalk uint8

const (
	// httpWalkMore는 record를 다 읽었고 message가 이어진다는 뜻이다.
	httpWalkMore httpWalk = iota
	// httpWalkDone은 종료 chunk와 trailer 끝을 지났다는 뜻이다.
	httpWalkDone
	// httpWalkLost는 크기 줄이 캡처 밖이거나 형식이 틀려 경계를 잃었다는 뜻이다.
	httpWalkLost
	// httpWalkStopped는 visit이 걷기를 멈추게 했다는 뜻이다.
	httpWalkStopped
)

// clickHouseHTTPChunks는 chunked body의 크기 줄, data, CRLF, 종료 chunk를 크기 계산으로 따라간다. data는 record 크기로
// 건너뛰므로 캡처 밖이어도 된다. 크기 줄과 CRLF가 캡처 밖이면 경계를 잃는다.
type clickHouseHTTPChunks struct {
	state httpChunkState
	line  []byte
	left  int64
	crlf  int
	// count는 지금까지 시작한 chunk 수다. 1이면 첫 chunk를 읽는 중이다.
	count int
}

func (chunks *clickHouseHTTPChunks) reset() { *chunks = clickHouseHTTPChunks{} }

// httpChunkSizeOf는 "<16진수>[;확장]CRLF" 형식을 확인한 뒤에만 크기를 돌려준다.
func httpChunkSizeOf(line []byte) (int64, bool) {
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return 0, false
	}
	digits, _, _ := bytes.Cut(line[:len(line)-2], []byte(";"))
	if len(digits) == 0 || len(digits) > 15 {
		return 0, false
	}
	size, err := strconv.ParseInt(string(digits), 16, 64)
	return size, err == nil
}

// step은 data[pos:]를 읽는다. visit은 chunk data의 보이는 부분을 받고, cut이면 그 뒤가 캡처 밖이다. true를 돌려주면 멈춘다.
func (chunks *clickHouseHTTPChunks) step(data []byte, size, pos int, visit func(piece []byte, cut bool) bool) (httpWalk, int) {
	for pos < size {
		switch chunks.state {
		case httpChunkSize, httpChunkTrailer:
			if pos >= len(data) {
				return httpWalkLost, pos
			}
			lf := bytes.IndexByte(data[pos:], '\n')
			end := len(data)
			if lf >= 0 {
				end = pos + lf + 1
			}
			if len(chunks.line)+end-pos > clickHouseHTTPChunkLineLimit {
				return httpWalkLost, pos
			}
			chunks.line = append(chunks.line, data[pos:end]...)
			if lf < 0 {
				if len(data) < size {
					return httpWalkLost, pos
				}
				return httpWalkMore, size
			}
			pos = end
			line := chunks.line
			chunks.line = chunks.line[:0]
			if chunks.state == httpChunkTrailer {
				if string(line) == "\r\n" {
					return httpWalkDone, pos
				}
				continue
			}
			length, ok := httpChunkSizeOf(line)
			if !ok {
				return httpWalkLost, pos
			}
			chunks.count++
			if length == 0 {
				chunks.state = httpChunkTrailer
			} else {
				chunks.state, chunks.left = httpChunkData, length
			}
		case httpChunkData:
			n := min(chunks.left, int64(size-pos))
			end := pos + int(n)
			if visit != nil {
				low, high := min(pos, len(data)), min(end, len(data))
				if visit(data[low:high], high < end) {
					return httpWalkStopped, end
				}
			}
			chunks.left -= n
			pos = end
			if chunks.left == 0 {
				chunks.state, chunks.crlf = httpChunkCRLF, 0
			}
		case httpChunkCRLF:
			for chunks.crlf < 2 && pos < size {
				if pos < len(data) && data[pos] != "\r\n"[chunks.crlf] {
					return httpWalkLost, pos
				}
				chunks.crlf++
				pos++
			}
			if chunks.crlf == 2 {
				chunks.state = httpChunkSize
			}
		}
	}
	return httpWalkMore, size
}

type httpRequestState uint8

const (
	// httpRequestLine은 요청 경계다. 새 socket은 이 상태로 시작하지만, 첫 record가 method로 시작하지 않으면(연결 중간부터
	// 본 body 같은 것) 바로 경계를 잃은 흐름이 된다.
	httpRequestLine httpRequestState = iota
	httpRequestHeaders
	httpRequestLength
	httpRequestChunked
	// httpRequestUnsynced는 경계를 모르는 흐름이다. record가 완전한 요청 줄로 시작할 때만 다시 맞춘다.
	httpRequestUnsynced
)

type httpResponseState uint8

const (
	httpResponseUnsynced httpResponseState = iota
	// httpResponseIdle은 message 경계다. 상태 줄로 시작하는 byte만 받는다.
	httpResponseIdle
	httpResponseStatus
	httpResponseHeaders
	httpResponseLength
	httpResponseChunked
	// httpResponseLost는 body의 경계를 잃은 응답이다. 요청은 이미 대기열에서 뺐고, 응답 중간 오류 표지만 계속 찾는다.
	httpResponseLost
)

type clickHouseHTTPRequest struct {
	// origin은 요청의 첫 record다. event의 시각과 process, latency의 시작이다. payload는 담지 않는다.
	origin mysqlRecord
	side   string
	offset int64
	method string
	// skip은 쿼리가 아닌 요청이다. event 없이 응답만 소비한다.
	skip bool
	head bool

	length                                               int64
	chunked                                              bool
	multipart                                            bool
	encoded                                              bool
	headerUser, headerDatabase, headerQueryID, basicUser string

	hasQuery                         bool
	query                            []byte
	urlUser, urlDatabase, urlQueryID string
	decompress                       bool
	// urlComplete가 거짓이면 요청 줄의 뒤가 캡처 밖이다.
	urlComplete bool

	body       []byte
	bodyGap    bool
	bodyCapped bool
	bodyDone   bool
	headerDone bool
	emitted    bool
	info       traceClickHouseEvent
}

// bodyUsed는 body를 SQL의 일부로 읽는지 알려 준다. 압축이나 multipart body는 풀지 않는다.
func (request *clickHouseHTTPRequest) bodyUsed() bool {
	return !request.multipart && !request.encoded && !request.decompress
}

// statement는 지금까지 아는 SQL이다. query parameter 뒤에 '\n'을 두고 body를 잇는다. INSERT는 data가 시작하기 전까지만
// 남기고, 그렇게 잘랐으면 true를 돌려준다.
func (request *clickHouseHTTPRequest) statement() ([]byte, bool) {
	var text []byte
	if request.hasQuery {
		text = append(text, request.query...)
		text = append(text, '\n')
	}
	text = append(text, request.body...)
	tailOpen := request.bodyUsed() && len(request.body) > 0 && !request.bodyDone && !request.bodyGap && !request.bodyCapped
	if end, ok := clickHouseInsertCut(text, !tailOpen); ok {
		return text[:end], true
	}
	return bytes.TrimRight(text, " \t\r\n"), false
}

// ready는 SQL이 더 바뀌지 않는다고 알려 준다.
func (request *clickHouseHTTPRequest) ready() bool {
	if request.emitted || request.skip {
		return false
	}
	if request.headerDone && !request.bodyUsed() {
		return true
	}
	if request.bodyDone || request.bodyGap || request.bodyCapped || len(request.body) >= clickHouseHTTPBodyLimit {
		return true
	}
	_, cut := request.statement()
	return cut
}

type clickHouseHTTPRequestFlow struct {
	state   httpRequestState
	line    clickHouseHTTPLine
	current *clickHouseHTTPRequest
	left    int64
	chunks  clickHouseHTTPChunks
}

type clickHouseHTTPResponseFlow struct {
	state httpResponseState
	line  clickHouseHTTPLine
	// current는 지금 읽는 응답이 답하는 요청이다. 짝이 없으면 nil이고 그 응답은 event 없이 소비한다.
	current *clickHouseHTTPRequest
	status  int
	chunked bool
	length  int64
	// queryID, tag, code는 응답 header 값이다. tag는 응답 중간 오류 블록과 맞출 표지다.
	queryID string
	tag     string
	code    int32
	left    int64
	chunks  clickHouseHTTPChunks
	message []byte
}

func (flow *clickHouseHTTPResponseFlow) reset() {
	line := flow.line
	line.reset()
	*flow = clickHouseHTTPResponseFlow{line: line, length: -1}
}

type clickHouseHTTPConn struct {
	request  clickHouseHTTPRequestFlow
	response clickHouseHTTPResponseFlow
	// queue는 응답을 기다리는 요청이다. 응답은 요청 순서대로 온다.
	queue []*clickHouseHTTPRequest
}

type clickHouseHTTPCall struct {
	tracker *clickHouseTracker
	conn    *clickHouseHTTPConn
	record  mysqlRecord
	side    string
	offset  int64
	out     []captureEvent
}

func (conn *clickHouseHTTPConn) push(request *clickHouseHTTPRequest) *clickHouseHTTPRequest {
	var dropped *clickHouseHTTPRequest
	if len(conn.queue) >= clickHouseHTTPQueueLimit {
		dropped = conn.queue[0]
		conn.queue = slices.Delete(conn.queue, 0, 1)
	}
	conn.queue = append(conn.queue, request)
	return dropped
}

// pop은 request가 대기열 맨 앞이면 뺀다. 앞에서 밀려난 요청이면 그대로 둔다.
func (conn *clickHouseHTTPConn) pop(request *clickHouseHTTPRequest) {
	if len(conn.queue) > 0 && conn.queue[0] == request {
		conn.queue = slices.Delete(conn.queue, 0, 1)
	}
}

// parseHTTPRequestLine은 "METHOD SP target SP HTTP/1.x CRLF"를 나눈다.
func parseHTTPRequestLine(line []byte) (method string, target []byte, ok bool) {
	if !bytes.HasSuffix(line, []byte("\r\n")) {
		return "", nil, false
	}
	line = line[:len(line)-2]
	first := bytes.IndexByte(line, ' ')
	last := bytes.LastIndexByte(line, ' ')
	if first <= 0 || last <= first+1 || !slices.Contains(traceHTTPMethods, string(line[:first])) {
		return "", nil, false
	}
	if version := string(line[last+1:]); version != "HTTP/1.1" && version != "HTTP/1.0" {
		return "", nil, false
	}
	target = line[first+1 : last]
	if bytes.ContainsAny(target, " \t\r\n") {
		return "", nil, false
	}
	return string(line[:first]), target, true
}

// httpMethodStart는 data가 알려진 method와 공백으로 시작하거나 그 앞부분인지 알려 준다.
func httpMethodStart(data []byte) bool {
	for _, method := range traceHTTPMethods {
		word := method + " "
		if (len(data) >= len(word) && string(data[:len(word)]) == word) || (len(data) < len(word) && strings.HasPrefix(word, string(data))) {
			return true
		}
	}
	return false
}

// httpRequestStart는 unsynced 흐름의 record가 요청으로 시작하는지 본다. 완전한 요청 줄이거나, 요청 줄이 캡처 밖으로
// 이어지는 긴 URL이어야 한다.
func httpRequestStart(data []byte, size int) bool {
	if !httpMethodStart(data) {
		return false
	}
	if lf := bytes.IndexByte(data, '\n'); lf >= 0 {
		_, _, ok := parseHTTPRequestLine(data[:lf+1])
		return ok
	}
	if len(data) >= size {
		return false
	}
	first := bytes.IndexByte(data, ' ')
	return first > 0 && !slices.ContainsFunc(data[first+1:], func(char byte) bool { return char <= ' ' || char == 0x7f })
}

func httpQueryTarget(method string, target []byte) bool {
	withQuery := bytes.HasPrefix(target, []byte("?")) || bytes.HasPrefix(target, []byte("/?")) || bytes.HasPrefix(target, []byte("/query?"))
	switch method {
	case "GET", "HEAD":
		return withQuery
	case "POST", "OPTIONS":
		return withQuery || string(target) == "/"
	}
	return false
}

func httpPercentDecode(value []byte) []byte {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] == '+':
			out = append(out, ' ')
		case value[i] == '%' && i+2 < len(value) && clickHouseHexDigit(value[i+1]) && clickHouseHexDigit(value[i+2]):
			digit, _ := strconv.ParseUint(string(value[i+1:i+3]), 16, 8)
			out = append(out, byte(digit))
			i += 2
		default:
			out = append(out, value[i])
		}
	}
	return out
}

// parseTarget은 허용한 URL parameter의 값만 읽는다.
func (request *clickHouseHTTPRequest) parseTarget(target []byte) {
	_, query, found := bytes.Cut(target, []byte("?"))
	if !found {
		return
	}
	for len(query) > 0 {
		var pair []byte
		pair, query, _ = bytes.Cut(query, []byte("&"))
		key, value, _ := bytes.Cut(pair, []byte("="))
		if !slices.Contains(clickHouseHTTPURLKeys, string(key)) {
			continue
		}
		decoded := httpPercentDecode(value)
		switch string(key) {
		case "query":
			if !request.hasQuery {
				request.hasQuery, request.query = true, decoded
			}
		case "user":
			request.urlUser = cmpOrText(request.urlUser, decoded)
		case "database":
			request.urlDatabase = cmpOrText(request.urlDatabase, decoded)
		case "query_id":
			request.urlQueryID = cmpOrText(request.urlQueryID, decoded)
		case "decompress":
			request.decompress = string(decoded) == "1"
		}
	}
}

func cmpOrText(current string, value []byte) string {
	if current != "" {
		return current
	}
	return clickHouseText(value, clickHouseTextLimit)
}

// newRequest는 요청 줄에서 요청을 만들어 대기열에 넣는다. complete가 거짓이면 target 뒤가 캡처 밖이다.
func (call *clickHouseHTTPCall) newRequest(method string, target []byte, complete bool) *clickHouseHTTPRequest {
	origin := call.record
	origin.payload = nil
	request := &clickHouseHTTPRequest{origin: origin, side: call.side, offset: call.offset, method: method, head: method == "HEAD",
		skip: !httpQueryTarget(method, target), length: -1, urlComplete: complete}
	if !request.skip {
		request.parseTarget(target)
	}
	if dropped := call.conn.push(request); dropped != nil {
		call.emitQuery(dropped)
	}
	return request
}

func (call *clickHouseHTTPCall) emitQuery(request *clickHouseHTTPRequest) {
	if request != nil && !request.skip && !request.emitted {
		call.out = append(call.out, call.tracker.httpQueryEvent(request))
	}
}

func (call *clickHouseHTTPCall) emitIfReady(request *clickHouseHTTPRequest) {
	if request != nil && request.ready() {
		call.emitQuery(request)
	}
}

// httpQueryEvent는 요청의 query event를 만든다. 요청의 SQL은 이 시점에 정해지고 body 같은 원본은 버린다.
func (tracker *clickHouseTracker) httpQueryEvent(request *clickHouseHTTPRequest) captureEvent {
	request.emitted = true
	text, cut := request.statement()
	complete := cut || (request.urlComplete && request.headerDone && request.bodyUsed() && request.bodyDone && !request.bodyGap && !request.bodyCapped)
	sql, truncated := tracker.sqlText(text, !complete)
	request.info = traceClickHouseEvent{Command: clickHouseCommandQuery, SQL: sql, SQLTruncated: truncated,
		User:     cmpOrString(request.headerUser, request.urlUser, request.basicUser),
		Database: cmpOrString(request.headerDatabase, request.urlDatabase),
		QueryID:  cmpOrString(request.headerQueryID, request.urlQueryID), Compressed: request.encoded || request.decompress}
	clear(request.body)
	clear(request.query)
	request.body, request.query = nil, nil
	info := request.info
	return clickHouseEventAt(request.origin, request.side, request.offset, clickHouseEventQuery, request.origin.size, withHTTPInterface(&info))
}

func withHTTPInterface(info *traceClickHouseEvent) *traceClickHouseEvent {
	info.Interface = clickHouseInterfaceHTTP
	return info
}

func cmpOrString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// request는 요청 방향 record 하나를 읽는다. record 하나에 요청 여럿이나 요청 일부만 있어도 처음부터 끝까지 읽는다.
func (call *clickHouseHTTPCall) request() {
	record, flow := call.record, &call.conn.request
	data, size := record.payload, record.size
	if size == 0 {
		return
	}
	pos := 0
	if flow.state == httpRequestUnsynced {
		if !httpRequestStart(data, size) {
			return
		}
		flow.state = httpRequestLine
		flow.line.reset()
	}
	for pos < size {
		switch flow.state {
		case httpRequestLine, httpRequestHeaders:
			if pos >= len(data) || (flow.state == httpRequestLine && flow.line.mode == httpLineIdle && !httpMethodStart(data[pos:])) {
				call.requestLost()
				return
			}
			result, text, next := flow.line.read(data, size, pos, flow.state == httpRequestLine, clickHouseHTTPRequestHeader)
			pos = next
			if flow.state == httpRequestLine {
				switch result {
				case httpLineDone:
					method, target, ok := parseHTTPRequestLine(text)
					if !ok {
						flow.line.release()
						call.requestLost()
						return
					}
					flow.current = call.newRequest(method, target, true)
					flow.line.release()
					flow.state = httpRequestHeaders
				case httpLineLost:
					call.truncatedRequest(text)
					return
				default:
					return
				}
				continue
			}
			switch result {
			case httpLineDone:
				if string(text) == "\r\n" {
					flow.line.release()
					call.endRequestHeaders()
				} else if !call.requestHeader(text) {
					flow.line.release()
					call.requestLost()
					return
				} else {
					flow.line.release()
				}
			case httpLineLost:
				call.requestLost()
				return
			case httpLineMore:
				return
			}
		case httpRequestLength:
			n := min(flow.left, int64(size-pos))
			call.collectBody(data, pos, int(n))
			flow.left -= n
			pos += int(n)
			if flow.left == 0 {
				call.endRequestBody()
			}
		case httpRequestChunked:
			result, next := flow.chunks.step(data, size, pos, func(piece []byte, cut bool) bool {
				call.collect(flow.current, piece, cut)
				return false
			})
			pos = next
			switch result {
			case httpWalkDone:
				call.endRequestBody()
			case httpWalkLost:
				if flow.current != nil {
					flow.current.bodyGap = true
				}
				call.requestLost()
				return
			}
		default:
			return
		}
	}
}

// requestLost는 요청 흐름의 경계를 잃은 것이다. SQL이 아직 정해지지 않았으면 아는 만큼으로 query event를 내고, 요청은
// 응답을 기다리는 대기열에 그대로 둔다. 다음 record가 요청 줄로 시작할 때 다시 맞춘다.
func (call *clickHouseHTTPCall) requestLost() {
	flow := &call.conn.request
	if flow.current != nil {
		call.emitQuery(flow.current)
	}
	flow.state, flow.current = httpRequestUnsynced, nil
	flow.line.reset()
	flow.chunks.reset()
}

// truncatedRequest는 요청 줄이 캡처 밖으로 이어지는 긴 URL이다. 보이는 앞부분의 parameter로 query event를 낸다.
func (call *clickHouseHTTPCall) truncatedRequest(partial []byte) {
	flow := &call.conn.request
	if first := bytes.IndexByte(partial, ' '); first > 0 && slices.Contains(traceHTTPMethods, string(partial[:first])) {
		request := call.newRequest(string(partial[:first]), partial[first+1:], false)
		flow.current = request
	}
	flow.line.release()
	call.requestLost()
}

// requestHeader는 요청 header 한 줄을 읽는다. 형식이 틀리면 거짓이다.
func (call *clickHouseHTTPCall) requestHeader(text []byte) bool {
	request := call.conn.request.current
	if request == nil || !bytes.HasSuffix(text, []byte("\r\n")) {
		return false
	}
	name, value, _ := bytes.Cut(text[:len(text)-2], []byte(":"))
	value = bytes.TrimSpace(value)
	switch clickHouseHTTPHeaderName(name) {
	case "content-length":
		length, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil || length < 0 {
			return false
		}
		request.length = length
	case "transfer-encoding":
		request.chunked = strings.Contains(strings.ToLower(string(value)), "chunked")
	case "content-type":
		request.multipart = strings.HasPrefix(strings.ToLower(string(value)), "multipart/")
	case "content-encoding":
		request.encoded = len(value) > 0 && !strings.EqualFold(string(value), "identity")
	case "x-clickhouse-user":
		request.headerUser = clickHouseText(value, clickHouseTextLimit)
	case "x-clickhouse-database":
		request.headerDatabase = clickHouseText(value, clickHouseTextLimit)
	case "x-clickhouse-query-id":
		request.headerQueryID = clickHouseText(value, clickHouseTextLimit)
	case "authorization":
		request.basicUser = httpBasicUser(value)
	}
	return true
}

// httpBasicUser는 "Basic <base64>"에서 ':' 앞의 user만 돌려준다. password가 든 byte는 바로 지운다.
func httpBasicUser(value []byte) string {
	scheme, credentials, found := bytes.Cut(value, []byte(" "))
	if !found || !strings.EqualFold(string(scheme), "basic") {
		return ""
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(credentials)))
	n, err := base64.StdEncoding.Decode(decoded, bytes.TrimSpace(credentials))
	defer clear(decoded)
	if err != nil {
		return ""
	}
	user, _, _ := bytes.Cut(decoded[:n], []byte(":"))
	return clickHouseText(user, clickHouseTextLimit)
}

// endRequestHeaders는 header가 끝났을 때 body를 어떻게 건너뛸지 정한다.
func (call *clickHouseHTTPCall) endRequestHeaders() {
	flow := &call.conn.request
	request := flow.current
	if request == nil {
		call.requestLost()
		return
	}
	request.headerDone = true
	switch {
	case request.chunked:
		flow.state = httpRequestChunked
		flow.chunks.reset()
	case request.length > 0:
		flow.state, flow.left = httpRequestLength, request.length
	default:
		request.bodyDone = true
		flow.state, flow.current = httpRequestLine, nil
	}
	call.emitIfReady(request)
}

func (call *clickHouseHTTPCall) endRequestBody() {
	flow := &call.conn.request
	if flow.current != nil {
		flow.current.bodyDone = true
		call.emitIfReady(flow.current)
	}
	flow.state, flow.current = httpRequestLine, nil
}

// collectBody는 Content-Length body에서 record가 담은 n byte 중 보이는 부분을 모은다.
func (call *clickHouseHTTPCall) collectBody(data []byte, pos, n int) {
	low, high := min(pos, len(data)), min(pos+n, len(data))
	call.collect(call.conn.request.current, data[low:high], high < pos+n)
}

// collect는 SQL이 될 body의 앞부분을 이어진 byte로만 모은다. 캡처 밖으로 끊긴 곳 뒤는 모으지 않는다.
func (call *clickHouseHTTPCall) collect(request *clickHouseHTTPRequest, piece []byte, cut bool) {
	if request == nil || request.skip || request.emitted || !request.bodyUsed() || request.bodyGap || request.bodyCapped {
		return
	}
	if room := clickHouseHTTPBodyLimit - len(request.body); len(piece) > room {
		piece, request.bodyCapped = piece[:room], true
	}
	request.body = append(request.body, piece...)
	if cut {
		request.bodyGap = true
	}
	call.emitIfReady(request)
}

// httpStatusStart는 data가 "HTTP/1.x NNN"으로 시작하는지 알려 준다.
func httpStatusStart(data []byte) (int, bool) {
	if len(data) < 12 || string(data[:7]) != "HTTP/1." || (data[7] != '0' && data[7] != '1') || data[8] != ' ' {
		return 0, false
	}
	code, err := strconv.Atoi(string(data[9:12]))
	if err != nil || code < 100 || code > 599 || (len(data) > 12 && data[12] != ' ' && data[12] != '\r') {
		return 0, false
	}
	return code, true
}

// response는 응답 방향 record 하나를 읽는다.
func (call *clickHouseHTTPCall) response() {
	record, flow := call.record, &call.conn.response
	data, size := record.payload, record.size
	pos := 0
	for pos < size {
		switch flow.state {
		case httpResponseUnsynced, httpResponseIdle, httpResponseLost:
			if pos >= len(data) || (flow.state != httpResponseIdle && pos > 0) {
				flow.state = httpResponseUnsynced
				return
			}
			code, ok := httpStatusStart(data[pos:])
			if !ok {
				if flow.state == httpResponseLost && call.lostException(data) {
					return
				}
				if flow.state != httpResponseLost {
					flow.state = httpResponseUnsynced
				}
				return
			}
			flow.reset()
			flow.status = code
			if len(call.conn.queue) > 0 {
				flow.current = call.conn.queue[0]
			}
			flow.state = httpResponseStatus
		case httpResponseStatus, httpResponseHeaders:
			if pos >= len(data) {
				call.responseLost()
				return
			}
			result, text, next := flow.line.read(data, size, pos, flow.state == httpResponseStatus, clickHouseHTTPResponseHeader)
			pos = next
			switch result {
			case httpLineMore:
				return
			case httpLineLost:
				call.responseLost()
				return
			case httpLineSkipped:
				continue
			}
			if flow.state == httpResponseStatus {
				flow.line.release()
				flow.state = httpResponseHeaders
				if flow.status >= 200 {
					call.emitQuery(flow.current)
				}
				continue
			}
			if string(text) == "\r\n" {
				flow.line.release()
				if !call.endResponseHeaders() {
					return
				}
			} else {
				call.responseHeader(text)
				flow.line.release()
			}
		case httpResponseLength:
			n := min(flow.left, int64(size-pos))
			low, high := min(pos, len(data)), min(pos+int(n), len(data))
			call.collectMessage(data[low:high])
			flow.left -= n
			pos += int(n)
			if flow.left == 0 {
				call.completeResponse()
			}
		case httpResponseChunked:
			result, next := flow.chunks.step(data, size, pos, call.visitResponse)
			pos = next
			switch result {
			case httpWalkDone:
				call.completeResponse()
			case httpWalkLost:
				call.responseLost()
				return
			case httpWalkStopped:
				return
			}
		default:
			return
		}
	}
}

// responseLost는 응답의 경계를 잃은 것이다. 요청은 응답 없이 끝난 것으로 두고(unanswered) 틀린 latency를 내지 않는다.
// 다음 상태 줄로 시작하는 record에서 다시 맞춘다.
func (call *clickHouseHTTPCall) responseLost() {
	flow := &call.conn.response
	if flow.current != nil {
		call.conn.pop(flow.current)
	}
	if flow.current == nil || flow.tag == "" || flow.state < httpResponseLength {
		flow.reset()
		flow.state = httpResponseUnsynced
		return
	}
	flow.state, flow.chunked, flow.chunks, flow.message = httpResponseLost, false, clickHouseHTTPChunks{}, nil
}

// lostException은 경계를 잃은 응답의 record에서 응답 중간 오류 표지를 찾는다. 표지가 header의 tag와 함께 record 앞부분에
// 보이면 오류를 내고 참을 돌려준다.
func (call *clickHouseHTTPCall) lostException(data []byte) bool {
	flow := &call.conn.response
	marker := []byte("__exception__\r\n" + flow.tag)
	index := bytes.Index(data, marker)
	if index < 0 {
		return false
	}
	call.midstreamException(data[index+len(marker):])
	return true
}

func (call *clickHouseHTTPCall) responseHeader(text []byte) {
	flow := &call.conn.response
	if !bytes.HasSuffix(text, []byte("\r\n")) {
		return
	}
	name, value, _ := bytes.Cut(text[:len(text)-2], []byte(":"))
	value = bytes.TrimSpace(value)
	switch clickHouseHTTPHeaderName(name) {
	case "content-length":
		if length, err := strconv.ParseInt(string(value), 10, 64); err == nil && length >= 0 {
			flow.length = length
		}
	case "transfer-encoding":
		flow.chunked = strings.Contains(strings.ToLower(string(value)), "chunked")
	case "x-clickhouse-exception-code":
		if code, err := strconv.ParseInt(string(value), 10, 32); err == nil {
			flow.code = int32(code)
		}
	case "x-clickhouse-exception-tag":
		if len(value) == clickHouseHTTPTagLength && !slices.ContainsFunc(value, func(char byte) bool {
			return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9')
		}) {
			flow.tag = string(value)
		}
	case "x-clickhouse-query-id":
		flow.queryID = clickHouseText(value, clickHouseTextLimit)
	}
}

// endResponseHeaders는 응답 header가 끝났을 때 body를 어떻게 건너뛸지 정한다. 이어서 읽을 수 있으면 참이다.
func (call *clickHouseHTTPCall) endResponseHeaders() bool {
	flow := &call.conn.response
	request := flow.current
	switch {
	case flow.status < 200:
		// 1xx는 중간 응답이다. 요청은 대기열에 두고 다음 상태 줄을 기다린다.
		flow.reset()
		flow.state = httpResponseIdle
	case (request != nil && request.head) || flow.status == 204 || flow.status == 304:
		call.completeResponse()
	case flow.chunked:
		flow.state = httpResponseChunked
		flow.chunks.reset()
	case flow.length == 0:
		call.completeResponse()
	case flow.length > 0:
		flow.state, flow.left = httpResponseLength, flow.length
	default:
		// 길이를 알 수 없는 응답(HTTP/1.0)은 연결이 닫혀야 끝난다. 닫힌 시각은 완료 시각이 아니라서 응답 없이 둔다.
		call.responseLost()
		return false
	}
	return true
}

func (call *clickHouseHTTPCall) collectMessage(piece []byte) {
	flow := &call.conn.response
	if flow.status >= 400 && flow.current != nil && len(flow.message) < clickHouseMessageLimit {
		flow.message = append(flow.message, piece[:min(len(piece), clickHouseMessageLimit-len(flow.message))]...)
	}
}

// visitResponse는 chunk data의 보이는 부분을 본다. 응답 중간 오류 블록은 header의 tag와 같은 tag가 뒤따르는 표지로만
// 알아본다. data에 우연히 나온 표지는 tag가 달라 걸러진다.
func (call *clickHouseHTTPCall) visitResponse(piece []byte, cut bool) bool {
	flow := &call.conn.response
	if flow.chunks.count == 1 {
		call.collectMessage(piece)
	}
	if flow.tag == "" {
		return false
	}
	marker := []byte("__exception__\r\n" + flow.tag)
	index := bytes.Index(piece, marker)
	if index < 0 {
		return false
	}
	call.midstreamException(piece[index+len(marker):])
	return true
}

var clickHouseCodePattern = regexp.MustCompile(`^Code: (\d{1,9})\.`)

// midstreamException은 응답이 이미 시작된 뒤의 오류 블록에서 clickhouse_exception을 낸다. 블록은 "\r\n<message>\n<길이> <tag>"
// 형태이고 message가 압축되어 읽을 수 없으면 code와 message를 비워 둔다. 오류 뒤에 종료 chunk는 오지 않는다.
func (call *clickHouseHTTPCall) midstreamException(rest []byte) {
	flow := &call.conn.response
	info := traceClickHouseEvent{}
	rest = bytes.TrimPrefix(rest, []byte("\r\n"))
	if match := clickHouseCodePattern.FindSubmatch(rest); match != nil {
		code, _ := strconv.ParseInt(string(match[1]), 10, 32)
		info.ErrorCode = int32(code)
		message := rest
		if tail := bytes.Index(message, []byte(" "+flow.tag)); tail >= 0 {
			message = message[:tail]
			if newline := bytes.LastIndexByte(message, '\n'); newline >= 0 {
				message = message[:newline]
			}
		}
		// 압축 stream 안의 block은 "Code: N."만 literal로 남고 나머지는 압축되어 읽을 수 없다.
		if !slices.ContainsFunc(message, func(char byte) bool { return (char < 0x20 && char != '\n' && char != '\t') || char == 0x7f }) {
			info.Message = clickHouseText(message, clickHouseMessageLimit)
			info.ErrorName = clickHouseErrorName(info.Message)
		}
	}
	call.respond(clickHouseEventException, info)
	flow.reset()
	flow.state = httpResponseUnsynced
}

func clickHouseErrorName(message string) string {
	if match := clickHouseErrorNamePattern.FindStringSubmatch(message); match != nil {
		return match[1]
	}
	return ""
}

// completeResponse는 응답 하나가 끝났을 때 event를 내고 다음 응답을 기다린다.
func (call *clickHouseHTTPCall) completeResponse() {
	flow := &call.conn.response
	info := traceClickHouseEvent{}
	name := clickHouseEventEnd
	if flow.status >= 400 {
		name = clickHouseEventException
		info.ErrorCode = flow.code
		if bytes.HasPrefix(flow.message, []byte("Code: ")) {
			info.Message = clickHouseText(flow.message, clickHouseMessageLimit)
			info.ErrorName = clickHouseErrorName(info.Message)
		}
	}
	call.respond(name, info)
	flow.reset()
	flow.state = httpResponseIdle
}

// respond는 응답 event를 낸다. 답한 요청의 command, SQL, user, database를 복사해 TUI가 요청과 짝지을 수 있게 하고, 요청이
// 시작한 때부터 이 record까지를 latency로 한다.
func (call *clickHouseHTTPCall) respond(name string, info traceClickHouseEvent) {
	flow := &call.conn.response
	request := flow.current
	if request == nil {
		return
	}
	call.conn.pop(request)
	if request.skip {
		return
	}
	call.emitQuery(request)
	asked := request.info
	info.Command, info.SQL, info.SQLTruncated, info.Compressed = asked.Command, asked.SQL, asked.SQLTruncated, asked.Compressed
	info.User, info.Database = asked.User, asked.Database
	info.QueryID, info.HTTPStatus = cmpOrString(flow.queryID, asked.QueryID), flow.status
	event := clickHouseEventAt(call.record, call.side, call.offset, name, call.record.size, withHTTPInterface(&info))
	event.LatencyMS, event.answered = traceSpan(request.origin.bootTimeNS, call.record.bootTimeNS), 1
	call.out = append(call.out, event)
}

// clickHouseInsertCut은 INSERT 문에서 data가 시작하기 전까지의 길이를 돌려준다. 문자열과 주석 밖의 VALUES나 FORMAT <이름>
// 바로 뒤다. final이 거짓이면 text 끝에서 끝나는 단어는 아직 이어질 수 있어서 받지 않는다.
func clickHouseInsertCut(sql []byte, final bool) (int, bool) {
	skip := func(i int) int {
		for i < len(sql) {
			if sql[i] <= ' ' {
				i++
			} else if end := clickHouseCommentEnd(sql, i); end > i {
				i = end
			} else {
				break
			}
		}
		return i
	}
	word := func(i int) int {
		for i < len(sql) && mysqlIdentifierByte(sql[i]) {
			i++
		}
		return i
	}
	closed := func(end int) bool { return final || end < len(sql) }
	start := skip(0)
	if end := word(start); end == start || !strings.EqualFold(string(sql[start:end]), "INSERT") || end == len(sql) {
		return 0, false
	}
	for i := word(start); i < len(sql); {
		switch {
		case sql[i] == '\'' || sql[i] == '"' || sql[i] == '`':
			i, _ = mysqlLiteralEnd(sql, i)
		case clickHouseCommentEnd(sql, i) > i:
			i = clickHouseCommentEnd(sql, i)
		case mysqlIdentifierByte(sql[i]):
			end := word(i)
			switch {
			case strings.EqualFold(string(sql[i:end]), "VALUES") && closed(end):
				return end, true
			case strings.EqualFold(string(sql[i:end]), "FORMAT"):
				if name := skip(end); word(name) > name && closed(word(name)) {
					return word(name), true
				}
			}
			i = end
		default:
			i++
		}
	}
	return 0, false
}
