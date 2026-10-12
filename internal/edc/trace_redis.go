package edc

import (
	"bytes"
	"math"
	"strings"
)

const (
	traceRedisDefaultPort = 6379
	redisEventCommand     = "redis_command"
	redisEventReply       = "redis_reply"
	redisEventError       = "redis_error"
	redisEventPush        = "redis_push"
	// redisDepthLimit은 중첩 aggregate 깊이의 상한이다. 넘으면 framing을 잃은 것으로 본다.
	redisDepthLimit = 32
	// redisBulkLimit은 bulk 길이와 aggregate 원소 수로 받아들이는 값의 상한이다. Redis의 proto-max-bulk-len 기본값과 같다.
	redisBulkLimit = 512 << 20
	// redisLineKeep은 줄 하나에서 보관하는 byte 수의 상한이다. 길이 줄이 이보다 길면 framing을 잃은 것으로 본다.
	redisLineKeep = 1024
	// redisLengthLine은 길이 줄에 받아들이는 byte 수다. int64의 19자리와 부호면 충분하다.
	redisLengthLine = 20
	// redisInlineLimit은 inline 요청 줄의 상한이다. Redis의 PROTO_INLINE_MAX_SIZE와 같다.
	redisInlineLimit = 64 << 10
	// redisValueKeep은 값과 인자 preview 하나의 상한이고, redisArgKeep은 preview로 남기는 인자 수다.
	redisValueKeep = 64
	redisArgKeep   = 8
	// redisNameLimit은 명령 이름과 subcommand의 상한이다. 더 길면 명령 이름으로 보지 않는다.
	redisNameLimit = 32
	redisKeyKeep   = 128
	// redisHeadKeep은 pub/sub와 push의 종류와 channel 이름을 읽으려고 남기는 byte 수다.
	redisHeadKeep = 128
	redisWordKeep = 8
	// 아래 상한을 넘으면 그 연결의 대기 명령을 모두 버리고 framing을 잃은 것처럼 다시 맞춘다. 가장 오래된 것만 버리면 짝이 밀린다.
	redisPendingLimit = 4096
	redisPendingTotal = 65536
	// redisSocketLimit에 닿으면 모든 연결의 상태를 비운다.
	redisSocketLimit = 65536
)

// traceRedisEvent는 Redis event에만 붙는 값이다. 응답에는 답한 요청의 명령과 key도 붙인다. 인자와 값의 preview, 오류
// message는 --show-secrets일 때만 붙고, password가 실릴 수 있는 명령에는 그때도 붙지 않는다.
type traceRedisEvent struct {
	Command    string   `json:"command,omitempty"`
	Key        string   `json:"key,omitempty"`
	Args       int64    `json:"args,omitempty"`
	ArgPreview []string `json:"arg_preview,omitempty"`
	Inline     bool     `json:"inline,omitempty"`
	// Truncated는 명령 frame을 끝까지 읽지 못했다는 뜻이다. 그 명령의 응답은 짝짓지 않는다.
	Truncated  bool   `json:"truncated,omitempty"`
	Reply      string `json:"reply,omitempty"`
	Elements   *int64 `json:"elements,omitempty"`
	BulkLength *int64 `json:"bulk_length,omitempty"`
	Value      string `json:"value,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
	Message    string `json:"message,omitempty"`
	RESP       int    `json:"resp,omitempty"`
	PushKind   string `json:"push_kind,omitempty"`
	Channel    string `json:"channel,omitempty"`
}

// redisTraced는 captureEvent에 Redis 칸이 없어서 둘을 함께 넘긴다.
type redisTraced struct {
	event captureEvent
	redis *traceRedisEvent
}

// redisKeep은 응답 frame 하나에서 무엇을 남길지다. 응답이 어느 명령의 것인지는 tracker만 알므로 frame이 시작할 때 묻는다.
type redisKeep struct {
	value   bool
	message bool
	word    bool
	heads   bool
}

// redisHead는 최상위 aggregate의 앞 원소다. pub/sub message와 push의 종류, channel, 구독 수를 읽는 데만 쓴다.
type redisHead struct {
	kind    byte
	text    string
	integer int64
	number  bool
	null    bool
}

// redisCommand는 요청 frame에서 읽은 명령이다. 인자 byte는 이름으로 민감한지 정한 뒤에만 남긴다.
type redisCommand struct {
	name    string
	key     string
	args    int64
	preview []string
	inline  bool
	// sensitive는 응답의 값과 오류 message를 남기지 않는 명령이다.
	sensitive bool
	proto     int
	reply     string
	// channels는 구독 계열 명령의 channel 인자 수다. 확인 응답이 몇 개 올지 알려 준다.
	channels int64
}

// redisFrame은 최상위 RESP 값 하나다. at은 첫 byte를 담은 record의 시각이고 bytes는 건너뛴 부분까지 센 길이다.
type redisFrame struct {
	kind    byte
	null    bool
	attr    bool
	count   int64
	bytes   int64
	at      uint64
	value   string
	word    string
	class   string
	message string
	monitor bool
	heads   [3]redisHead
	command *redisCommand
	// lost는 요청 frame을 읽다가 framing을 잃었다는 뜻이다. 명령 이름까지만 알 수 있다.
	lost bool
}

// redisLevel은 읽는 중인 aggregate다. inAttr는 attribute이거나 attribute 안이라는 뜻이다.
type redisLevel struct {
	kind   byte
	inAttr bool
	left   int64
	index  int
}

const (
	redisStateType uint8 = iota
	redisStateLine
	redisStateBulk
	redisStateInline
)

// redisReader는 한 방향의 byte 흐름에서 RESP frame 경계를 따라간다. bulk 본문은 size로 남은 길이를 건너뛰므로 캡처한 앞부분보다
// 커도 경계를 지킨다. 줄은 캡처한 byte 안에서 끝나야 한다.
type redisReader struct {
	request     bool
	showSecrets bool
	keep        func(kind byte) redisKeep

	state   uint8
	cr      bool
	stack   []redisLevel
	inFrame bool
	frame   redisFrame
	policy  redisKeep

	// 지금 읽는 원소다. depth는 원소 위의 aggregate 수이고 attr는 attribute 안이라는 뜻이다.
	elemKind  byte
	elemDepth int
	elemIndex int
	elemAttr  bool
	elemKeep  int
	held      []byte
	lineLen   int
	negative  bool
	number    int64
	digits    int
	overflow  bool
	left      int64
	bulkLen   int64

	word      [redisWordKeep]byte
	wordLen   int
	class     [redisNameLimit]byte
	classLen  int
	classDone uint8
	shape     uint8

	command redisDecoder
}

// feed는 record 하나를 이어서 읽고 최상위 frame이 끝날 때마다 frame을 부른다. framing을 잃으면 false를 돌려주고 상태를 비운다.
func (r *redisReader) feed(data []byte, size int, at uint64, frame func(redisFrame)) bool {
	size = max(size, len(data))
	for pos := 0; pos < size; {
		if r.state == redisStateBulk {
			n := int(min(r.left, int64(size-pos)))
			if !r.bulk(data[min(pos, len(data)):min(pos+n, len(data))], n) {
				return r.lose(frame)
			}
			pos += n
			r.frame.bytes += int64(n)
			if r.left == 0 && !r.finishScalar(frame, r.held, r.bulkLen) {
				return r.lose(frame)
			}
			continue
		}
		if pos >= len(data) {
			return r.lose(frame)
		}
		b := data[pos]
		pos++
		ok := true
		switch r.state {
		case redisStateType:
			if !r.inFrame {
				r.inFrame, r.frame = true, redisFrame{at: at}
			}
			r.frame.bytes++
			ok = r.begin(b, frame)
		case redisStateLine:
			r.frame.bytes++
			ok = r.line(b, frame)
		case redisStateInline:
			r.frame.bytes++
			ok = r.inline(b, frame)
		}
		if !ok {
			return r.lose(frame)
		}
	}
	return true
}

// idle은 frame 경계에 있다는 뜻이다.
func (r *redisReader) idle() bool { return !r.inFrame }

// skipping은 bulk 본문을 건너뛰는 중이라는 뜻이다. 다음 record가 본문 중간에서 시작해도 경계를 지킨다.
func (r *redisReader) skipping() bool { return r.state == redisStateBulk }

func (r *redisReader) heldBytes() int { return len(r.held) }

// lose는 framing을 잃은 뒤 상태를 비운다. 요청 frame은 이름까지 읽었으면 lost frame으로 알린다.
func (r *redisReader) lose(frame func(redisFrame)) bool {
	if r.request && r.inFrame && r.command.name != "" && frame != nil {
		frame(redisFrame{kind: '*', at: r.frame.at, bytes: r.frame.bytes, command: r.command.result(r.state == redisStateInline), lost: true})
	}
	r.reset()
	return false
}

// reset은 쥔 byte를 용량 끝까지 지운다. 다시 쓰는 buffer에 앞 값이 남지 않게 한다.
func (r *redisReader) reset() {
	clear(r.held[:cap(r.held)])
	clear(r.stack[:cap(r.stack)])
	*r = redisReader{request: r.request, showSecrets: r.showSecrets, keep: r.keep, held: r.held[:0], stack: r.stack[:0]}
}

func redisTypeByte(b byte) bool {
	return strings.IndexByte("+-:_,#($!=*~>%|", b) >= 0
}

// begin은 원소의 type byte를 읽는다.
func (r *redisReader) begin(b byte, frame func(redisFrame)) bool {
	depth := len(r.stack)
	if r.request {
		switch {
		case depth == 0 && b != '*':
			r.state, r.lineLen = redisStateInline, 0
			r.command.start(-1, r.showSecrets)
			r.elemIndex, r.elemKeep = 0, -1
			return r.inline(b, frame)
		case depth == 0:
		case depth == 1 && b == '$':
		default:
			return false
		}
	} else if !redisTypeByte(b) {
		return false
	}
	r.elemKind, r.elemDepth, r.elemAttr = b, depth, b == '|'
	r.elemIndex = 0
	if depth > 0 {
		r.elemIndex = r.stack[depth-1].index
		r.elemAttr = r.elemAttr || r.stack[depth-1].inAttr
	}
	if depth == 0 && !r.elemAttr {
		r.frame.kind = b
		if r.keep != nil && !r.request {
			r.policy = r.keep(b)
		}
	}
	r.elemKeep = r.keepFor(b)
	r.state, r.cr, r.lineLen = redisStateLine, false, 0
	r.negative, r.number, r.digits, r.overflow = false, 0, 0, false
	r.wordLen, r.classLen, r.classDone, r.shape = 0, 0, 0, 0
	return true
}

// keepFor는 원소에서 남길 byte 수다. 0이면 byte를 보지 않고 길이만 센다.
func (r *redisReader) keepFor(b byte) int {
	if r.elemAttr {
		return 0
	}
	if r.request {
		if r.elemDepth == 1 {
			return r.command.want(r.elemIndex)
		}
		return 0
	}
	switch {
	case r.elemDepth == 0:
		switch {
		case b == '-' || b == '!':
			if r.policy.message {
				return redisLineKeep
			}
		case r.policy.value:
			return redisValueKeep
		}
	case r.elemDepth == 1 && r.policy.heads && r.elemIndex < len(r.frame.heads):
		switch r.elemIndex {
		case 0:
			return redisNameLimit
		case 1:
			return redisHeadKeep
		default:
			// message의 셋째 원소는 본문이다. pmessage일 때만 셋째가 channel이다.
			if r.frame.heads[0].text == "pmessage" {
				return redisHeadKeep
			}
		}
	}
	return 0
}

func (r *redisReader) hold(b byte) {
	if len(r.held) < r.elemKeep {
		r.grow(1)
		r.held = append(r.held, b)
	}
}

func (r *redisReader) holdBytes(p []byte) {
	if take := min(len(p), r.elemKeep-len(r.held)); take > 0 {
		r.grow(take)
		r.held = append(r.held, p[:take]...)
	}
}

// grow는 buffer를 늘릴 때 앞 buffer를 지운다. append가 버린 배열에 값이 남지 않게 한다.
func (r *redisReader) grow(n int) {
	if len(r.held)+n <= cap(r.held) {
		return
	}
	next := make([]byte, len(r.held), max(r.elemKeep, len(r.held)+n))
	copy(next, r.held)
	clear(r.held[:cap(r.held)])
	r.held = next
}

func (r *redisReader) dropHeld() {
	clear(r.held[:cap(r.held)])
	r.held = r.held[:0]
}

func redisLengthType(b byte) bool {
	return b == '$' || b == '!' || b == '=' || b == '*' || b == '~' || b == '>' || b == '%' || b == '|'
}

// line은 type byte 뒤의 줄을 읽는다. 길이 줄은 숫자만, 나머지는 CRLF 전까지 받는다.
func (r *redisReader) line(b byte, frame func(redisFrame)) bool {
	if r.cr {
		if b != '\n' {
			return false
		}
		r.cr = false
		return r.lineEnd(frame)
	}
	switch b {
	case '\r':
		r.cr = true
		return true
	case '\n':
		return false
	}
	r.lineLen++
	if redisLengthType(r.elemKind) {
		if r.lineLen > redisLengthLine {
			return false
		}
		return r.digit(b, r.lineLen == 1)
	}
	if r.elemKind == ':' && r.lineLen <= redisLengthLine {
		r.digit(b, r.lineLen == 1)
	}
	r.hold(b)
	r.classify(b)
	return true
}

// digit는 길이 줄과 정수의 숫자 하나를 읽는다. '?'(streamed 길이)와 '+' 부호는 받지 않는다.
func (r *redisReader) digit(b byte, first bool) bool {
	switch {
	case first && b == '-':
		r.negative = true
	case b >= '0' && b <= '9':
		if r.number > (math.MaxInt64-int64(b-'0'))/10 {
			r.overflow = true
		} else {
			r.number = r.number*10 + int64(b-'0')
		}
		r.digits++
	default:
		r.overflow = true
		return false
	}
	return true
}

// classify는 오류의 첫 대문자 단어(오류 class)와 MONITOR 줄 모양을 byte를 쌓지 않고 가린다.
func (r *redisReader) classify(b byte) {
	if (r.elemKind == '-' || r.elemKind == '!') && r.classDone == 0 {
		switch {
		case b >= 'A' && b <= 'Z' && r.classLen < len(r.class):
			r.class[r.classLen] = b
			r.classLen++
		case b == ' ' && r.classLen > 0:
			r.classDone = 1
		default:
			r.classDone = 2
		}
	}
	if r.elemKind == '+' {
		if r.wordLen < len(r.word) && r.policy.word {
			r.word[r.wordLen] = b
		}
		r.wordLen++
		// MONITOR 줄은 "<초>.<마이크로초> [" 로 시작한다.
		digit := b >= '0' && b <= '9'
		switch {
		case r.shape == 0 && digit, r.shape == 1 && digit:
			r.shape = 1
		case r.shape == 1 && b == '.':
			r.shape = 2
		case (r.shape == 2 || r.shape == 3) && digit:
			r.shape = 3
		case r.shape == 3 && b == ' ':
			r.shape = 4
		case r.shape == 4 && b == '[':
			r.shape = 5
		case r.shape != 5:
			r.shape = 6
		}
	}
}

func (r *redisReader) lineEnd(frame func(redisFrame)) bool {
	kind := r.elemKind
	if !redisLengthType(kind) {
		if kind == '_' && r.lineLen != 0 || kind == '#' && r.lineLen != 1 {
			return false
		}
		return r.scalar(frame)
	}
	if r.digits == 0 || r.overflow {
		return false
	}
	n := r.number
	if r.negative {
		if n != 1 || (kind != '$' && kind != '*') {
			return false
		}
		switch {
		case r.request && r.elemDepth == 1:
			return false
		case r.request:
			r.dropFrame()
			return true
		}
		r.frameNull()
		return r.finishScalar(frame, nil, -1)
	}
	if n > redisBulkLimit {
		return false
	}
	switch kind {
	case '$', '!', '=':
		r.state, r.left, r.bulkLen = redisStateBulk, n+2, n
		r.classLen, r.classDone = 0, 0
		return true
	}
	children := n
	if kind == '%' || kind == '|' {
		children = 2 * n
	}
	if r.elemDepth == 0 && !r.elemAttr {
		r.frame.count = n
		if r.request {
			if n == 0 {
				// Redis는 "*0"과 "*-1" 요청을 명령 없이 넘긴다.
				r.dropFrame()
				return true
			}
			r.command.start(n, r.showSecrets)
		}
	}
	if r.elemDepth == 1 && !r.elemAttr && r.policy.heads && r.elemIndex < len(r.frame.heads) {
		r.frame.heads[r.elemIndex] = redisHead{kind: kind, integer: n, number: true}
	}
	if children == 0 {
		return r.finish(frame)
	}
	if len(r.stack) >= redisDepthLimit {
		return false
	}
	r.stack = append(r.stack, redisLevel{kind: kind, inAttr: r.elemAttr, left: children})
	r.state = redisStateType
	return true
}

func (r *redisReader) frameNull() {
	if r.elemDepth == 0 && !r.elemAttr {
		r.frame.null = true
	}
}

// bulk는 bulk 본문 조각을 읽는다. visible은 캡처한 부분이고 n은 record에서 이 bulk가 차지하는 byte 수다. 본문 뒤 CRLF는
// 보일 때만 확인한다.
func (r *redisReader) bulk(visible []byte, n int) bool {
	offset := r.bulkLen + 2 - r.left
	payload, segment := visible[:0], int64(0)
	if offset < r.bulkLen {
		segment = min(int64(n), r.bulkLen-offset)
		payload = visible[:min(int64(len(visible)), segment)]
	}
	if r.elemKeep > 0 {
		r.holdBytes(payload)
		if int64(len(payload)) < segment {
			// 본문 일부를 보지 못했다. 뒤 record의 byte를 이어 붙이면 없던 값이 되므로 여기서 멈춘다.
			r.elemKeep = len(r.held)
		}
	}
	for _, b := range payload {
		if r.elemKind != '!' || r.classDone != 0 {
			break
		}
		r.classify(b)
	}
	for i := len(payload); i < len(visible); i++ {
		at := offset + int64(i)
		if at == r.bulkLen && visible[i] != '\r' || at == r.bulkLen+1 && visible[i] != '\n' {
			return false
		}
	}
	r.left -= int64(n)
	return true
}

// scalar는 줄로 끝나는 값이 끝났을 때다.
func (r *redisReader) scalar(frame func(redisFrame)) bool {
	if r.elemKind == '_' {
		r.frameNull()
	}
	return r.finishScalar(frame, r.held, int64(r.lineLen))
}

// finishScalar는 남긴 byte를 원소의 주인에게 넘기고 지운다.
func (r *redisReader) finishScalar(frame func(redisFrame), kept []byte, length int64) bool {
	switch {
	case r.elemAttr:
	case r.request && r.elemDepth == 1:
		r.command.done(r.elemIndex, kept, length)
	case !r.request && r.elemDepth == 0:
		r.topScalar(kept, length)
	case !r.request && r.elemDepth == 1 && r.policy.heads && r.elemIndex < len(r.frame.heads):
		head := redisHead{kind: r.elemKind, null: length < 0}
		if r.elemKeep > 0 && int64(len(kept)) == length {
			head.text = traceEscapeText(kept)
		}
		if r.elemKind == ':' && !r.overflow && r.digits > 0 {
			head.integer, head.number = r.number, true
			if r.negative {
				head.integer = -head.integer
			}
		}
		r.frame.heads[r.elemIndex] = head
	}
	r.dropHeld()
	return r.finish(frame)
}

func (r *redisReader) topScalar(kept []byte, length int64) {
	f := &r.frame
	switch r.elemKind {
	case '$', '=', '!':
		if length > 0 {
			f.count = length
		}
	}
	if r.elemKind == '-' || r.elemKind == '!' {
		if r.classLen > 0 && r.classDone != 2 {
			f.class = string(r.class[:r.classLen])
		}
		if r.policy.message && kept != nil {
			f.message = traceEscapeText(kept)
		}
		return
	}
	if r.elemKind == '+' {
		f.monitor = r.shape == 5
		if r.policy.word && r.wordLen <= len(r.word) {
			f.word = string(r.word[:r.wordLen])
		}
	}
	// MONITOR 줄은 다른 client의 명령이라 값으로도 남기지 않는다.
	if r.policy.value && !f.monitor && length >= 0 {
		f.value = traceEscapeText(kept)
	}
}

// finish는 원소 하나가 끝났을 때 위 aggregate의 남은 원소 수를 줄이고, 최상위 frame이 끝나면 frame을 부른다. attribute는
// 원소로 세지 않으므로 같은 자리에 진짜 값이 이어진다.
func (r *redisReader) finish(frame func(redisFrame)) bool {
	r.state = redisStateType
	r.dropHeld()
	attribute := r.elemKind == '|'
	for {
		depth := len(r.stack)
		if depth == 0 {
			if attribute {
				r.frame.attr = true
				return true
			}
			out := r.frame
			if r.request {
				out.command = r.command.result(false)
			}
			r.dropFrame()
			if frame != nil {
				frame(out)
			}
			return true
		}
		if attribute {
			return true
		}
		top := &r.stack[depth-1]
		top.left--
		top.index++
		if top.left > 0 {
			return true
		}
		attribute = top.kind == '|'
		r.stack[depth-1] = redisLevel{}
		r.stack = r.stack[:depth-1]
	}
}

func (r *redisReader) dropFrame() {
	r.inFrame, r.frame, r.policy = false, redisFrame{}, redisKeep{}
	r.word, r.wordLen, r.class, r.classLen = [redisWordKeep]byte{}, 0, [redisNameLimit]byte{}, 0
	r.state = redisStateType
	r.command.clear()
}

func redisSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\v' || b == '\f'
}

// inline은 '*'로 시작하지 않는 요청 줄이다. Redis처럼 공백으로 나누고 LF에서 끝낸다. 단어를 하나씩 넘기므로 AUTH의
// password는 이름을 본 뒤 쌓지 않는다.
func (r *redisReader) inline(b byte, frame func(redisFrame)) bool {
	r.lineLen++
	if r.lineLen > redisInlineLimit {
		return false
	}
	inWord := r.elemKeep >= 0
	switch {
	case b == '\n':
		if inWord {
			r.endWord()
		}
		if r.elemIndex == 0 {
			r.dropFrame()
			return true
		}
		r.command.argc(int64(r.elemIndex))
		out := r.frame
		out.kind, out.command = 'i', r.command.result(true)
		r.dropFrame()
		if frame != nil {
			frame(out)
		}
		return true
	case redisSpace(b):
		if inWord {
			r.endWord()
		}
	default:
		if !inWord {
			r.elemKeep, r.digits = r.command.want(r.elemIndex), 0
		}
		r.hold(b)
		r.digits++
	}
	return true
}

func (r *redisReader) endWord() {
	r.command.done(r.elemIndex, r.held, int64(r.digits))
	r.dropHeld()
	r.elemIndex++
	r.elemKeep, r.digits = -1, 0
}

// redisContainers는 둘째 인자가 subcommand인 명령이다.
var redisContainers = map[string]bool{
	"ACL": true, "CLIENT": true, "CLUSTER": true, "COMMAND": true, "CONFIG": true, "DEBUG": true, "FUNCTION": true,
	"LATENCY": true, "MEMORY": true, "MODULE": true, "OBJECT": true, "PUBSUB": true, "SCRIPT": true, "SLOWLOG": true,
	"XGROUP": true, "XINFO": true,
}

// redisKeyAt은 첫 key의 인자 위치다. 표에 없는 명령은 key를 보이지 않는다. EVAL처럼 key 위치가 값에 따라 바뀌는 명령은 넣지 않는다.
var redisKeyAt = func() map[string]int {
	keys := map[string]int{
		"OBJECT ENCODING": 2, "OBJECT FREQ": 2, "OBJECT IDLETIME": 2, "OBJECT REFCOUNT": 2, "MEMORY USAGE": 2,
		"XINFO STREAM": 2, "XINFO GROUPS": 2, "XINFO CONSUMERS": 2, "XGROUP CREATE": 2, "XGROUP DESTROY": 2,
		"XGROUP SETID": 2, "XGROUP CREATECONSUMER": 2, "XGROUP DELCONSUMER": 2, "BITOP": 2, "MIGRATE": 3,
	}
	for _, name := range []string{
		"APPEND", "BITCOUNT", "BITFIELD", "BITFIELD_RO", "BITPOS", "BLMOVE", "BLPOP", "BRPOP", "BRPOPLPUSH", "BZPOPMAX", "BZPOPMIN",
		"COPY", "DECR", "DECRBY", "DEL", "DUMP", "EXISTS", "EXPIRE", "EXPIREAT", "EXPIRETIME", "GEOADD", "GEODIST", "GEOHASH",
		"GEOPOS", "GEORADIUS", "GEORADIUSBYMEMBER", "GEOSEARCH", "GEOSEARCHSTORE", "GET", "GETBIT", "GETDEL", "GETEX", "GETRANGE",
		"GETSET", "HDEL", "HEXISTS", "HGET", "HGETALL", "HINCRBY", "HINCRBYFLOAT", "HKEYS", "HLEN", "HMGET", "HMSET", "HRANDFIELD",
		"HSCAN", "HSET", "HSETNX", "HSTRLEN", "HVALS", "INCR", "INCRBY", "INCRBYFLOAT", "LINDEX", "LINSERT", "LLEN", "LMOVE",
		"LPOP", "LPOS", "LPUSH", "LPUSHX", "LRANGE", "LREM", "LSET", "LTRIM", "MGET", "MOVE", "MSET", "MSETNX", "PERSIST",
		"PEXPIRE", "PEXPIREAT", "PEXPIRETIME", "PFADD", "PFCOUNT", "PFMERGE", "PSETEX", "PTTL", "RENAME", "RENAMENX", "RESTORE",
		"RPOP", "RPOPLPUSH", "RPUSH", "RPUSHX", "SADD", "SCARD", "SDIFF", "SDIFFSTORE", "SET", "SETBIT", "SETEX", "SETNX",
		"SETRANGE", "SINTER", "SINTERCARD", "SINTERSTORE", "SISMEMBER", "SMEMBERS", "SMISMEMBER", "SMOVE", "SORT", "SORT_RO",
		"SPOP", "SRANDMEMBER", "SREM", "SSCAN", "STRLEN", "SUBSTR", "SUNION", "SUNIONSTORE", "TOUCH", "TTL", "TYPE", "UNLINK",
		"WATCH", "XACK", "XADD", "XAUTOCLAIM", "XCLAIM", "XDEL", "XLEN", "XPENDING", "XRANGE", "XREVRANGE", "XSETID", "XTRIM",
		"ZADD", "ZCARD", "ZCOUNT", "ZDIFFSTORE", "ZINCRBY", "ZINTERSTORE", "ZLEXCOUNT", "ZMSCORE", "ZPOPMAX", "ZPOPMIN",
		"ZRANDMEMBER", "ZRANGE", "ZRANGEBYLEX", "ZRANGEBYSCORE", "ZRANGESTORE", "ZRANK", "ZREM", "ZREMRANGEBYLEX",
		"ZREMRANGEBYRANK", "ZREMRANGEBYSCORE", "ZREVRANGE", "ZREVRANGEBYLEX", "ZREVRANGEBYSCORE", "ZREVRANK", "ZSCAN",
		"ZSCORE", "ZUNIONSTORE",
	} {
		keys[name] = 1
	}
	return keys
}()

// redisSensitiveReplies는 응답 값과 오류 message에 password나 그 hash가 실릴 수 있는 명령이다.
var redisSensitiveReplies = map[string]bool{
	"AUTH": true, "HELLO": true, "MIGRATE": true, "CONFIG SET": true, "CONFIG GET": true, "ACL SETUSER": true,
	"ACL GETUSER": true, "ACL LIST": true, "ACL DRYRUN": true, "ACL LOG": true,
}

var redisSubscribeKinds = map[string]bool{
	"SUBSCRIBE": true, "PSUBSCRIBE": true, "SSUBSCRIBE": true, "UNSUBSCRIBE": true, "PUNSUBSCRIBE": true, "SUNSUBSCRIBE": true,
}

// redisDecoder는 요청 frame의 인자를 하나씩 받아 명령으로 읽는다. 인자 byte를 남기기 전에 want로 남길 길이를 정하므로
// 민감한 인자는 처음부터 쌓지 않는다.
type redisDecoder struct {
	showSecrets bool
	count       int64
	name        string
	container   bool
	key         string
	keyAt       int
	preview     []string
	// secretFrom 이후의 인자는 password나 ACL 규칙이다. helloSecret까지의 인자는 HELLO AUTH의 user와 password다.
	secretFrom  int
	helloSecret int
	proto       int
	reply       string
}

func (d *redisDecoder) start(count int64, showSecrets bool) {
	d.clear()
	d.count, d.showSecrets, d.secretFrom, d.keyAt = count, showSecrets, math.MaxInt, -1
}

func (d *redisDecoder) clear() {
	*d = redisDecoder{preview: d.preview[:0]}
	clear(d.preview[:cap(d.preview)])
}

func (d *redisDecoder) argc(count int64) { d.count = count }

func (d *redisDecoder) secret(index int) bool {
	return index >= d.secretFrom || (index > 0 && index <= d.helloSecret)
}

// want는 index번째 인자에서 남길 byte 수다.
func (d *redisDecoder) want(index int) int {
	switch {
	case index == 0:
		return redisNameLimit + 1
	case d.name == "" || d.secret(index):
		return 0
	case index == 1 && d.container:
		return redisNameLimit + 1
	case index == d.keyAt:
		return redisKeyKeep
	case d.name == "HELLO", d.name == "CLIENT REPLY" && index == 2, d.name == "MIGRATE" && index > 5:
		// 선택지 이름(AUTH, SETNAME)과 protocol 번호를 읽는다. 값은 아니다.
		return max(redisWordKeep, d.previewKeep())
	}
	return d.previewKeep()
}

func (d *redisDecoder) previewKeep() int {
	if d.showSecrets && len(d.preview) < redisArgKeep {
		return redisValueKeep
	}
	return 0
}

// redisName은 명령 이름으로 쓸 수 있는 byte인지 본다. 대문자로 바꾼 이름을 돌려준다.
func redisName(raw []byte, length int64) (string, bool) {
	if length < 1 || length > redisNameLimit || int64(len(raw)) != length {
		return "", false
	}
	name := make([]byte, len(raw))
	for i, b := range raw {
		switch {
		case b >= 'a' && b <= 'z':
			b -= 'a' - 'A'
		case b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-', b == '.', b == ':', b == '|':
		default:
			return "", false
		}
		name[i] = b
	}
	return string(name), true
}

// done은 index번째 인자가 끝났을 때다. kept는 want만큼 남긴 앞부분이고 length는 인자 전체 길이다.
func (d *redisDecoder) done(index int, kept []byte, length int64) {
	if index == 0 {
		name, ok := redisName(kept, length)
		if !ok {
			return
		}
		d.name, d.container = name, redisContainers[name]
		switch name {
		case "AUTH":
			d.secretFrom = 1
		}
		if at, ok := redisKeyAt[name]; ok && !d.container {
			d.keyAt = at
		}
		return
	}
	if d.name == "" || d.secret(index) {
		return
	}
	word := kept
	if int64(len(kept)) != length {
		word = nil
	}
	if index == 1 && d.container {
		sub, ok := redisName(word, length)
		if !ok {
			return
		}
		d.name += " " + sub
		switch d.name {
		case "CONFIG SET":
			d.secretFrom = 2
		case "ACL SETUSER":
			d.secretFrom = 3
		}
		if at, ok := redisKeyAt[d.name]; ok {
			d.keyAt = at
		}
		return
	}
	switch {
	case d.name == "HELLO" && index == 1:
		if len(word) == 1 && word[0] >= '2' && word[0] <= '3' {
			d.proto = int(word[0] - '0')
		}
	case d.name == "HELLO" && bytes.EqualFold(word, []byte("AUTH")):
		d.helloSecret = index + 2
	case d.name == "MIGRATE" && index > 5 && (bytes.EqualFold(word, []byte("AUTH")) || bytes.EqualFold(word, []byte("AUTH2"))):
		d.secretFrom = index + 1
	case d.name == "CLIENT REPLY" && index == 2:
		if sub, ok := redisName(word, length); ok {
			d.reply = sub
		}
	}
	if index == d.keyAt {
		d.key = traceEscapeText(kept)
	}
	if d.showSecrets && len(d.preview) < redisArgKeep && len(kept) > 0 {
		d.preview = append(d.preview, traceEscapeText(kept))
	}
}

// result는 읽은 명령을 돌려준다. 이름을 못 읽은 frame은 이름이 비어 있다.
func (d *redisDecoder) result(inline bool) *redisCommand {
	command := &redisCommand{name: d.name, key: d.key, inline: inline, proto: d.proto, reply: d.reply}
	if d.count > 0 {
		command.args = d.count - 1
	}
	if len(d.preview) > 0 {
		command.preview = append([]string(nil), d.preview...)
	}
	command.sensitive = redisSensitiveReplies[d.name]
	if redisSubscribeKinds[d.name] {
		command.channels = command.args
	}
	return command
}

type redisPending struct {
	at        uint64
	name      string
	key       string
	sensitive bool
	proto     int
	channels  int64
}

// redisConn은 socket 하나의 두 방향 상태다. 새 socket은 두 쪽 모두 경계를 모르는 상태로 시작한다. 연결 중간에 trace를
// 시작한 경우다.
type redisConn struct {
	request        redisReader
	response       redisReader
	requestSynced  bool
	responseSynced bool
	pending        []redisPending
	proto          int
	subscribed     bool
	monitor        bool
	multi          bool
	replyOff       bool
	skipNext       bool
	// subKind는 마지막으로 짝지은 구독 계열 확인의 종류이고, subExtra는 같은 명령의 확인이 더 올 수다. -1은 모른다는 뜻이다.
	subKind  string
	subExtra int64
}

func (conn *redisConn) resetModes() {
	conn.proto, conn.subscribed, conn.monitor, conn.multi, conn.replyOff, conn.skipNext = 2, false, false, false, false, false
	conn.subKind, conn.subExtra = "", 0
}

// redisTracker는 socket마다 요청과 응답 frame을 FIFO로 짝짓는다. side는 tracker가 볼 쪽이고 비어 있으면 두 쪽을 모두 본다.
type redisTracker struct {
	side        string
	showSecrets bool
	conns       map[uint64]*redisConn
	pending     int
}

func newRedisTracker(side string, showSecrets bool) *redisTracker {
	return &redisTracker{side: side, showSecrets: showSecrets, conns: map[uint64]*redisConn{}}
}

type redisCall struct {
	tracker *redisTracker
	conn    *redisConn
	record  mysqlRecord
	side    string
	offset  int64
	out     []redisTraced
}

func (tracker *redisTracker) events(record mysqlRecord, clockOffset int64) []redisTraced {
	side := traceClientSide
	if record.server {
		side = traceServerSide
	}
	if tracker.side != "" && side != tracker.side {
		return nil
	}
	conn := tracker.conns[record.socket]
	if conn == nil {
		if len(tracker.conns) >= redisSocketLimit {
			clear(tracker.conns)
			tracker.pending = 0
		}
		conn = tracker.newConn()
		tracker.conns[record.socket] = conn
	}
	call := &redisCall{tracker: tracker, conn: conn, record: record, side: side, offset: clockOffset}
	// client는 요청을 보내고 응답을 받는다. 서버는 요청을 받고 응답을 보낸다.
	if record.server != record.sent {
		call.request()
	} else {
		call.response()
	}
	return call.out
}

func (tracker *redisTracker) newConn() *redisConn {
	conn := &redisConn{}
	conn.request = redisReader{request: true, showSecrets: tracker.showSecrets}
	conn.response = redisReader{keep: func(kind byte) redisKeep { return tracker.keep(conn, kind) }}
	return conn
}

func (tracker *redisTracker) forgetSocket(socket uint64) {
	conn := tracker.conns[socket]
	if conn == nil {
		return
	}
	tracker.drop(conn, math.MaxUint64)
	delete(tracker.conns, socket)
}

// keep은 응답 frame이 시작할 때 무엇을 남길지 정한다. 맨 앞 대기 명령이 password를 다루거나 MONITOR 중이면 값을 남기지 않는다.
func (tracker *redisTracker) keep(conn *redisConn, kind byte) redisKeep {
	var head *redisPending
	if len(conn.pending) > 0 {
		head = &conn.pending[0]
	}
	sensitive := conn.monitor || (head != nil && head.sensitive)
	pubsub := kind == '*' && conn.proto != 3 && (conn.subscribed || (head != nil && redisSubscribeKinds[head.name]))
	return redisKeep{
		value:   tracker.showSecrets && !sensitive,
		message: tracker.showSecrets && !sensitive,
		word:    !conn.monitor,
		heads:   kind == '>' || pubsub,
	}
}

// drop은 시각이 at 이하인 대기 명령을 응답 없이 버린다. 버린 MONITOR와 구독 명령은 성공했을 수도 있으므로 그 모드를 켜 둔다.
// MONITOR 줄이나 pub/sub message가 응답으로 짝지어지지 않게 한다.
func (tracker *redisTracker) drop(conn *redisConn, at uint64) {
	kept := conn.pending[:0]
	for _, pending := range conn.pending {
		switch {
		case pending.at > at:
			kept = append(kept, pending)
			continue
		case pending.name == "MONITOR":
			conn.monitor = true
		case pending.name == "SUBSCRIBE" || pending.name == "PSUBSCRIBE" || pending.name == "SSUBSCRIBE":
			conn.subscribed = conn.proto != 3
		}
		tracker.pending--
	}
	clear(conn.pending[len(kept):cap(conn.pending)])
	conn.pending = kept
	tracker.pending = max(tracker.pending, 0)
}

// lose는 framing을 잃었을 때다. 대기 명령은 모두 응답 없음이 되고, 응답 쪽은 경계를 다시 찾을 때까지 record를 버린다.
func (call *redisCall) lose() {
	conn := call.conn
	call.tracker.drop(conn, math.MaxUint64)
	conn.responseSynced = false
	conn.response.reset()
}

func (call *redisCall) event(name string, at uint64, length int64, info *traceRedisEvent) redisTraced {
	record := call.record
	return redisTraced{event: captureEvent{
		SocketID: record.socket, TimestampNS: uint64(int64(at) + call.offset), BootTimeNS: at, Event: name, Protocol: "redis", Side: call.side,
		PID: record.pid, Process: record.process, CgroupID: record.cgroupID, Source: record.source, Destination: record.destination, Bytes: uint64(length),
	}, redis: info}
}

// redisKnownCommands는 inline 요청에서 경계를 다시 찾을 때 받는 이름이다. key 표와 subcommand 명령 밖에서 흔한 것만 둔다.
var redisKnownCommands = map[string]bool{
	"AUTH": true, "HELLO": true, "PING": true, "ECHO": true, "SELECT": true, "INFO": true, "MULTI": true, "EXEC": true,
	"DISCARD": true, "QUIT": true, "RESET": true, "MONITOR": true, "DBSIZE": true, "FLUSHDB": true, "FLUSHALL": true,
	"KEYS": true, "SCAN": true, "PUBLISH": true, "SPUBLISH": true, "EVAL": true, "EVALSHA": true, "FCALL": true,
	"TIME": true, "ROLE": true, "UNWATCH": true, "RANDOMKEY": true,
}

// redisRequestStart는 경계를 모르는 요청 record가 명령의 시작인지 본다. "*<숫자>\r\n$"이나 알려진 inline 명령 이름이어야 한다.
func redisRequestStart(data []byte) bool {
	if len(data) > 0 && data[0] == '*' {
		i := 1
		for i < len(data) && i <= redisLengthLine && data[i] >= '0' && data[i] <= '9' {
			i++
		}
		return i > 1 && len(data) >= i+3 && data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '$'
	}
	end := 0
	for end < len(data) && end <= redisNameLimit && !redisSpace(data[end]) && data[end] != '\n' {
		end++
	}
	if end == 0 || end >= len(data) {
		return false
	}
	name, ok := redisName(data[:end], int64(end))
	_, keyed := redisKeyAt[name]
	return ok && (keyed || redisContainers[name] || redisKnownCommands[name] || redisSubscribeKinds[name])
}

// request는 요청 방향 record 하나를 읽는다.
func (call *redisCall) request() {
	conn, record := call.conn, call.record
	if !conn.requestSynced {
		if !redisRequestStart(record.payload) {
			return
		}
		conn.request.reset()
		conn.requestSynced = true
	}
	if !conn.request.feed(record.payload, record.size, record.bootTimeNS, call.command) {
		// 요청을 놓쳤을 수 있다. 그 응답이 뒤 명령과 짝지어지지 않게 응답 쪽도 다시 맞춘다.
		conn.requestSynced = false
		call.lose()
	}
}

// command는 요청 frame 하나다. CLIENT REPLY OFF와 SKIP으로 응답이 없는 명령은 event만 내고 대기열에 넣지 않는다.
func (call *redisCall) command(f redisFrame) {
	conn, c := call.conn, f.command
	if c == nil {
		return
	}
	info := &traceRedisEvent{Command: c.name, Key: c.key, Args: c.args, ArgPreview: c.preview, Inline: c.inline, Truncated: f.lost, RESP: conn.proto}
	call.out = append(call.out, call.event(redisEventCommand, f.at, f.bytes, info))
	if f.lost {
		return
	}
	queue := true
	switch {
	case c.name == "CLIENT REPLY" && c.reply == "ON":
		conn.replyOff, conn.skipNext = false, false
	case c.name == "CLIENT REPLY" && c.reply == "OFF":
		conn.replyOff, queue = true, false
	case c.name == "CLIENT REPLY" && c.reply == "SKIP":
		conn.skipNext, queue = true, false
	case conn.replyOff:
		queue = false
	case conn.skipNext:
		conn.skipNext, queue = false, false
	}
	if !queue {
		return
	}
	if len(conn.pending) >= redisPendingLimit || call.tracker.pending >= redisPendingTotal {
		call.lose()
		return
	}
	conn.pending = append(conn.pending, redisPending{at: f.at, name: c.name, key: c.key, sensitive: c.sensitive, proto: c.proto, channels: c.channels})
	call.tracker.pending++
}

// response는 응답 방향 record 하나를 읽는다.
func (call *redisCall) response() {
	conn, record := call.conn, call.record
	if !conn.responseSynced {
		if !call.resync() {
			// 버린 record 안에 답이 있었을 수 있는 명령은 응답 없음으로 버린다.
			call.tracker.drop(conn, record.bootTimeNS)
			return
		}
		conn.response.reset()
		conn.responseSynced = true
	}
	if !conn.response.feed(record.payload, record.size, record.bootTimeNS, call.reply) {
		call.lose()
	}
}

// resync는 경계를 모르는 응답 record에서 짝짓기를 다시 시작할지 본다. record가 type byte로 시작하고, 캡처한 byte가 온전한
// frame들로 끝나거나 건너뛸 수 있는 bulk 안에서 끝나고, push가 아닌 frame 수가 대기 명령 수 이하여야 한다. 우연히 이 모양인
// 쓰레기 record는 다음 손실까지 짝을 틀리게 할 수 있다.
func (call *redisCall) resync() bool {
	data, size := call.record.payload, call.record.size
	if len(data) == 0 || !redisTypeByte(data[0]) || len(call.conn.pending) == 0 {
		return false
	}
	var trial redisReader
	count := 0
	if !trial.feed(data, size, 0, func(f redisFrame) {
		if f.kind != '>' {
			count++
		}
	}) {
		return false
	}
	if !trial.idle() {
		if !trial.skipping() {
			return false
		}
		count++
	}
	return count <= len(call.conn.pending)
}

// reply는 응답 방향 frame 하나다. RESP3 push, pub/sub message, MONITOR 줄은 대기 명령을 꺼내지 않는다.
func (call *redisCall) reply(f redisFrame) {
	conn := call.conn
	if f.kind == '>' {
		if kind := f.heads[0].text; f.heads[0].kind == '$' && redisSubscribeKinds[strings.ToUpper(kind)] {
			call.confirm(f, kind, true)
		} else {
			call.push(f, kind)
		}
		return
	}
	if f.kind == '+' && f.monitor && conn.monitor {
		return
	}
	if f.kind == '*' && conn.proto != 3 && f.heads[0].kind == '$' && (conn.subscribed || (len(conn.pending) > 0 && redisSubscribeKinds[conn.pending[0].name])) {
		switch kind := f.heads[0].text; {
		case kind == "message" || kind == "pmessage" || kind == "smessage":
			call.push(f, kind)
			return
		case redisSubscribeKinds[strings.ToUpper(kind)]:
			call.confirm(f, kind, false)
			return
		}
	}
	call.answer(f, "")
}

// confirm은 구독 계열 확인이다. 첫 확인이 그 명령의 응답이고, 같은 명령이 더 보낸 확인은 event 없이 넘긴다.
func (call *redisCall) confirm(f redisFrame, kind string, push bool) {
	conn := call.conn
	head := ""
	if len(conn.pending) > 0 {
		head = conn.pending[0].name
	}
	switch {
	case head == strings.ToUpper(kind) && !(conn.subKind == kind && conn.subExtra > 0):
		channels := conn.pending[0].channels
		call.answer(f, kind)
		conn.subKind, conn.subExtra = kind, channels-1
		if channels == 0 {
			conn.subExtra = -1
		}
	case conn.subKind == kind && conn.subExtra != 0:
		if conn.subExtra > 0 {
			conn.subExtra--
		}
	default:
		call.push(f, kind)
	}
	if push {
		return
	}
	if !strings.HasSuffix(kind, "unsubscribe") {
		conn.subscribed = true
	} else if count := f.heads[2]; count.number && count.integer == 0 {
		conn.subscribed, conn.subExtra = false, 0
	}
}

// push는 대기 명령과 짝짓지 않는 frame이다.
func (call *redisCall) push(f redisFrame, kind string) {
	info := &traceRedisEvent{Reply: redisReplyKind(f, false), PushKind: kind, Channel: redisChannel(f, kind), RESP: call.conn.proto}
	info.Elements = &f.count
	call.out = append(call.out, call.event(redisEventPush, f.at, f.bytes, info))
}

func redisChannel(f redisFrame, kind string) string {
	head := f.heads[1]
	if kind == "pmessage" {
		head = f.heads[2]
	}
	if head.kind == '$' || head.kind == '+' {
		return head.text
	}
	return ""
}

var redisReplyKinds = map[byte]string{
	'+': "simple", '-': "error", ':': "integer", '$': "bulk", '*': "array", '%': "map", '~': "set", '_': "null",
	',': "double", '#': "boolean", '(': "bignum", '=': "verbatim", '!': "error", '>': "push",
}

func redisReplyKind(f redisFrame, multi bool) string {
	switch {
	case f.null:
		return "null"
	case multi && f.kind == '+' && f.word == "QUEUED":
		return "queued"
	}
	return redisReplyKinds[f.kind]
}

// answer는 맨 앞 대기 명령의 응답이다. 대기 명령이 없으면 무엇에 대한 응답인지 모르므로 event를 내지 않는다.
func (call *redisCall) answer(f redisFrame, pushKind string) {
	conn := call.conn
	if len(conn.pending) == 0 {
		return
	}
	pending := conn.pending[0]
	conn.pending[0] = redisPending{}
	conn.pending = conn.pending[1:]
	call.tracker.pending--
	failed := f.kind == '-' || f.kind == '!'
	switch {
	case pending.name == "MULTI" && !failed:
		conn.multi = true
	case pending.name == "EXEC" || pending.name == "DISCARD":
		conn.multi = false
	case pending.name == "HELLO" && !failed && pending.proto != 0:
		conn.proto = pending.proto
	case pending.name == "RESET" && !failed:
		conn.resetModes()
	case pending.name == "MONITOR" && !failed:
		conn.monitor = true
	}
	info := &traceRedisEvent{Command: pending.name, Key: pending.key, Reply: redisReplyKind(f, conn.multi), Value: f.value, PushKind: pushKind, RESP: conn.proto}
	if pushKind != "" {
		info.Channel = redisChannel(f, pushKind)
	}
	name := redisEventReply
	switch {
	case failed:
		name, info.ErrorClass, info.Message = redisEventError, f.class, f.message
	case f.null:
	case f.kind == '$' || f.kind == '=':
		info.BulkLength = &f.count
	case redisLengthType(f.kind):
		info.Elements = &f.count
	}
	if f.kind == '!' {
		info.BulkLength = &f.count
	}
	traced := call.event(name, f.at, f.bytes, info)
	traced.event.LatencyMS, traced.event.answered = traceSpan(pending.at, f.at), 1
	call.out = append(call.out, traced)
}
