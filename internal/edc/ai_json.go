package edc

import (
	"bytes"
	"encoding/json"
	"time"
)

// aiJSONReader는 대화 기록 한 줄에서 쓰는 필드만 읽는다. encoding/json은 도구 출력이 든 긴 문자열까지 한 글자씩 검사하고
// 디코딩해서, 첫 scan 시간 대부분이 쓰지 않는 값에 들었다. 문자열 안은 SIMD로 구현된 bytes.IndexByte로 건너뛴다.
// 숫자와 literal의 문법은 검사하지 않는다. 기록 파일은 도구가 쓴 JSON이고, 읽는 값은 형식을 따로 확인한다.
type aiJSONReader struct {
	data []byte
	pos  int
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

func (reader *aiJSONReader) skip() bool {
	switch reader.peek() {
	case 0:
		return false
	case '"':
		_, ok := reader.str()
		return ok
	case '{', '[':
		depth := 0
		for reader.pos < len(reader.data) {
			switch reader.data[reader.pos] {
			case '"':
				if _, ok := reader.str(); !ok {
					return false
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					reader.pos++
					return true
				}
			}
			reader.pos++
		}
		return false
	default:
		start := reader.pos
		for reader.pos < len(reader.data) {
			switch reader.data[reader.pos] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return reader.pos > start
			}
			reader.pos++
		}
		return reader.pos > start
	}
}

// object는 key마다 field를 부른다. field는 값을 끝까지 읽어야 한다.
func (reader *aiJSONReader) object(field func(key []byte) bool) bool {
	if reader.peek() != '{' {
		return false
	}
	reader.pos++
	if reader.peek() == '}' {
		reader.pos++
		return true
	}
	for {
		key, ok := reader.str()
		if !ok || reader.peek() != ':' {
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
	start := reader.pos
	if !reader.skip() {
		return false
	}
	text := reader.data[start:reader.pos]
	negative := len(text) > 0 && text[0] == '-'
	if negative {
		text = text[1:]
	}
	// 18자리까지는 int64를 넘지 않는다. token 수가 그보다 크면 기록이 잘못된 것이다.
	if len(text) == 0 || len(text) > 18 {
		return false
	}
	var value int64
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
		value = value*10 + int64(char-'0')
	}
	if negative {
		value = -value
	}
	*target = value
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
	value, err := time.Parse(time.RFC3339, string(content))
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
