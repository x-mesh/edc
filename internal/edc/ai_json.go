package edc

import (
	"bytes"
	"encoding/json"
	"math"
	"time"
)

// aiJSONReader는 대화 기록 한 줄에서 쓰는 필드만 읽는다. encoding/json은 도구 출력이 든 긴 문자열까지 한 글자씩 검사하고
// 디코딩해서, 첫 scan 시간 대부분이 쓰지 않는 값에 들었다. 문자열 안은 SIMD로 구현된 bytes.IndexByte로 건너뛴다.
// 구조와 숫자, literal의 문법은 encoding/json처럼 검사해서 깨진 줄은 세지 않는다. 건너뛰는 문자열 안의 제어 문자와 escape는
// 검사하지 않는다. 그것까지 보려면 문자열을 한 글자씩 읽어야 해서 건너뛰는 이점이 사라진다.
type aiJSONReader struct {
	data []byte
	pos  int
	// depth는 지금 읽는 객체와 배열의 중첩 깊이다. 실패하면 줄 전체를 버리므로 성공한 경우에만 줄인다.
	depth int
}

func (reader *aiJSONReader) peek() byte {
	for reader.pos < len(reader.data) {
		switch char := reader.data[reader.pos]; char {
		case ' ', '\t', '\n', '\r':
			reader.pos++
		default:
			return char
		}
	}
	return 0
}

func (reader *aiJSONReader) end() bool {
	return reader.peek() == 0 && reader.pos == len(reader.data)
}

// str은 따옴표를 뺀 문자열 내용이다. escape는 풀지 않는다.
func (reader *aiJSONReader) str() ([]byte, bool) {
	if reader.peek() != '"' {
		return nil, false
	}
	start := reader.pos + 1
	end := start
	for {
		next := bytes.IndexByte(reader.data[end:], '"')
		if next < 0 {
			return nil, false
		}
		end += next
		slashes := 0
		for at := end - 1; at >= start && reader.data[at] == '\\'; at-- {
			slashes++
		}
		if slashes%2 == 0 {
			break
		}
		end++
	}
	reader.pos = end + 1
	return reader.data[start:end], true
}

// aiJSONMaxDepth는 encoding/json이 받는 중첩 깊이의 한도다.
const aiJSONMaxDepth = 10000

func (reader *aiJSONReader) skip() bool {
	switch reader.peek() {
	case '"':
		_, ok := reader.str()
		return ok
	case '{':
		return reader.object(reader.skipField)
	case '[':
		return reader.array(reader.skip)
	case 't':
		return reader.literal("true")
	case 'f':
		return reader.literal("false")
	case 'n':
		return reader.literal("null")
	}
	return reader.number() != nil
}

func (reader *aiJSONReader) skipField([]byte) bool {
	return reader.skip()
}

func (reader *aiJSONReader) enter() bool {
	reader.depth++
	return reader.depth <= aiJSONMaxDepth
}

func (reader *aiJSONReader) literal(word string) bool {
	if !bytes.HasPrefix(reader.data[reader.pos:], []byte(word)) {
		return false
	}
	reader.pos += len(word)
	return true
}

// number는 JSON 숫자 하나를 읽고 그 글자를 돌려준다. 문법에 맞지 않으면 nil이다. 앞의 공백은 peek이 건너뛴 뒤다.
func (reader *aiJSONReader) number() []byte {
	data, start := reader.data, reader.pos
	at := start
	if at < len(data) && data[at] == '-' {
		at++
	}
	switch {
	case at < len(data) && data[at] == '0':
		at++
	case at < len(data) && data[at] >= '1' && data[at] <= '9':
		at = aiJSONDigits(data, at)
	default:
		return nil
	}
	if at < len(data) && data[at] == '.' {
		next := aiJSONDigits(data, at+1)
		if next == at+1 {
			return nil
		}
		at = next
	}
	if at < len(data) && (data[at] == 'e' || data[at] == 'E') {
		at++
		if at < len(data) && (data[at] == '+' || data[at] == '-') {
			at++
		}
		next := aiJSONDigits(data, at)
		if next == at {
			return nil
		}
		at = next
	}
	reader.pos = at
	return data[start:at]
}

func aiJSONDigits(data []byte, at int) int {
	for at < len(data) && data[at] >= '0' && data[at] <= '9' {
		at++
	}
	return at
}

func (reader *aiJSONReader) array(item func() bool) bool {
	if reader.peek() != '[' || !reader.enter() {
		return false
	}
	reader.pos++
	if reader.peek() == ']' {
		reader.pos++
		reader.depth--
		return true
	}
	for {
		if !item() {
			return false
		}
		switch reader.peek() {
		case ',':
			reader.pos++
		case ']':
			reader.pos++
			reader.depth--
			return true
		default:
			return false
		}
	}
}

// object는 key마다 field를 부른다. field는 값을 끝까지 읽어야 한다. escape가 든 key는 풀어서 넘긴다.
func (reader *aiJSONReader) object(field func(key []byte) bool) bool {
	if reader.peek() != '{' || !reader.enter() {
		return false
	}
	reader.pos++
	if reader.peek() == '}' {
		reader.pos++
		reader.depth--
		return true
	}
	for {
		key, ok := reader.str()
		if !ok {
			return false
		}
		if bytes.IndexByte(key, '\\') >= 0 {
			var name string
			if json.Unmarshal(reader.data[reader.pos-len(key)-2:reader.pos], &name) != nil {
				return false
			}
			key = []byte(name)
		}
		if reader.peek() != ':' {
			return false
		}
		reader.pos++
		if !field(key) {
			return false
		}
		switch reader.peek() {
		case ',':
			reader.pos++
		case '}':
			reader.pos++
			reader.depth--
			return true
		default:
			return false
		}
	}
}

// null은 encoding/json처럼 null을 값이 없는 것으로 읽는다.
func (reader *aiJSONReader) null() bool {
	if reader.peek() == 'n' && bytes.HasPrefix(reader.data[reader.pos:], []byte("null")) {
		reader.pos += 4
		return true
	}
	return false
}

func (reader *aiJSONReader) integer(target *int64) bool {
	if reader.null() {
		return true
	}
	text := reader.number()
	if text == nil {
		return false
	}
	negative := text[0] == '-'
	if negative {
		text = text[1:]
	}
	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	var value uint64
	for _, char := range text {
		// 소수와 지수는 encoding/json도 int64로 읽지 않는다.
		if char < '0' || char > '9' {
			return false
		}
		digit := uint64(char - '0')
		if value > (limit-digit)/10 {
			return false
		}
		value = value*10 + digit
	}
	if negative {
		*target = -int64(value)
	} else {
		*target = int64(value)
	}
	return true
}

func (reader *aiJSONReader) text(target *string) bool {
	if reader.null() {
		return true
	}
	start := reader.pos
	content, ok := reader.str()
	if !ok {
		return false
	}
	if bytes.IndexByte(content, '\\') < 0 {
		*target = string(content)
		return true
	}
	return json.Unmarshal(reader.data[start:reader.pos], target) == nil
}

func (reader *aiJSONReader) timestamp(target *time.Time) bool {
	if reader.null() {
		return true
	}
	content, ok := reader.str()
	if !ok {
		return false
	}
	text := string(content)
	if bytes.IndexByte(content, '\\') >= 0 && json.Unmarshal(reader.data[reader.pos-len(content)-2:reader.pos], &text) != nil {
		return false
	}
	value, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return false
	}
	*target = value
	return true
}

type aiClaudeRecord struct {
	timestamp            time.Time
	requestID, messageID string
	hasUsage             bool
	input, cacheCreation int64
	cacheRead, output    int64
}

func parseAIClaudeRecord(line []byte) (aiClaudeRecord, bool) {
	var record aiClaudeRecord
	reader := aiJSONReader{data: line}
	usage := func(key []byte) bool {
		switch string(key) {
		case "input_tokens":
			return reader.integer(&record.input)
		case "cache_creation_input_tokens":
			return reader.integer(&record.cacheCreation)
		case "cache_read_input_tokens":
			return reader.integer(&record.cacheRead)
		case "output_tokens":
			return reader.integer(&record.output)
		}
		return reader.skip()
	}
	message := func(key []byte) bool {
		switch string(key) {
		case "id":
			return reader.text(&record.messageID)
		case "usage":
			if reader.null() {
				record.hasUsage = false
				return true
			}
			record.hasUsage = true
			return reader.object(usage)
		}
		return reader.skip()
	}
	ok := reader.object(func(key []byte) bool {
		switch string(key) {
		case "timestamp":
			return reader.timestamp(&record.timestamp)
		case "requestId":
			return reader.text(&record.requestID)
		case "message":
			return reader.null() || reader.object(message)
		}
		return reader.skip()
	})
	return record, ok && reader.end()
}

type aiCodexRecord struct {
	timestamp time.Time
	kind      string
	hasInfo   bool
	total     aiCodexTotals
}

func parseAICodexRecord(line []byte) (aiCodexRecord, bool) {
	var record aiCodexRecord
	reader := aiJSONReader{data: line}
	totals := func(key []byte) bool {
		switch string(key) {
		case "input_tokens":
			return reader.integer(&record.total.Input)
		case "cached_input_tokens":
			return reader.integer(&record.total.Cached)
		case "output_tokens":
			return reader.integer(&record.total.Output)
		case "total_tokens":
			return reader.integer(&record.total.Total)
		}
		return reader.skip()
	}
	info := func(key []byte) bool {
		if string(key) == "total_token_usage" {
			return reader.null() || reader.object(totals)
		}
		return reader.skip()
	}
	payload := func(key []byte) bool {
		switch string(key) {
		case "type":
			return reader.text(&record.kind)
		case "info":
			if reader.null() {
				record.hasInfo = false
				return true
			}
			record.hasInfo = true
			return reader.object(info)
		}
		return reader.skip()
	}
	ok := reader.object(func(key []byte) bool {
		switch string(key) {
		case "timestamp":
			return reader.timestamp(&record.timestamp)
		case "payload":
			return reader.null() || reader.object(payload)
		}
		return reader.skip()
	})
	return record, ok && reader.end()
}
