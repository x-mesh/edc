package edc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// aiClaudeReferenceDelta는 encoding/json으로 줄 전체를 읽던 이전 aiClaudeUsageDelta다. aiJSONReader가 같은 값을 읽는지 비교하는 기준이다.
func aiClaudeReferenceDelta(line []byte, seen map[string]aiSeenMessage) (time.Time, aiUsage, bool) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return time.Time{}, aiUsage{}, false
	}
	var record struct {
		Timestamp time.Time `json:"timestamp"`
		RequestID string    `json:"requestId"`
		Message   struct {
			ID    string `json:"id"`
			Usage *struct {
				Input         int64 `json:"input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				Output        int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &record) != nil || record.Message.Usage == nil || record.Timestamp.IsZero() {
		return time.Time{}, aiUsage{}, false
	}
	raw := record.Message.Usage
	usage := aiUsage{Requests: 1, Input: raw.Input + raw.CacheCreation, Output: raw.Output, Cache: raw.CacheRead}
	if record.Message.ID == "" {
		return record.Timestamp, usage, true
	}
	key := record.Message.ID + ":" + record.RequestID
	previous, ok := seen[key]
	if !ok {
		seen[key] = aiSeenMessage{at: record.Timestamp, usage: usage}
		return record.Timestamp, usage, true
	}
	growth := aiUsage{
		Input:  aiGrowth(previous.usage.Input, usage.Input),
		Output: aiGrowth(previous.usage.Output, usage.Output),
		Cache:  aiGrowth(previous.usage.Cache, usage.Cache),
	}
	if growth.empty() {
		return time.Time{}, aiUsage{}, false
	}
	previous.usage.Input, previous.usage.Output, previous.usage.Cache = previous.usage.Input+growth.Input, previous.usage.Output+growth.Output, previous.usage.Cache+growth.Cache
	seen[key] = previous
	return previous.at, growth, true
}

func aiCodexReferenceRecord(line []byte) (time.Time, aiCodexTotals, bool) {
	var record struct {
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Info *struct {
				Total aiCodexTotals `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil || record.Payload.Type != "token_count" || record.Payload.Info == nil {
		return time.Time{}, aiCodexTotals{}, false
	}
	return record.Timestamp, record.Payload.Info.Total, true
}

// aiJSONEscape는 글자 하나를 JSON의 유니코드 escape로 쓴다.
func aiJSONEscape(char rune) string {
	return fmt.Sprintf("\\u%04x", char)
}

// aiCompareClaudeLines는 lines를 차례로 두 구현에 넣는다. seen을 줄 사이에 이어 쓰므로 같은 응답을 한 번만 세는 경로도 비교한다.
func aiCompareClaudeLines(t *testing.T, lines []string) {
	t.Helper()
	referenceSeen, seen := map[string]aiSeenMessage{}, map[string]aiSeenMessage{}
	for _, line := range lines {
		wantAt, wantUsage, wantOK := aiClaudeReferenceDelta([]byte(line), referenceSeen)
		gotAt, gotUsage, gotOK := aiClaudeUsageDelta([]byte(line), seen)
		if gotOK != wantOK || gotUsage != wantUsage || !gotAt.Equal(wantAt) {
			t.Errorf("%.200s\n got %v %+v %v\nwant %v %+v %v", line, gotAt, gotUsage, gotOK, wantAt, wantUsage, wantOK)
		}
	}
}

// 본문 문자열과 도구 입력에 든 "usage"는 message.usage가 아니다. escape로 쓴 이름과 값은 풀어서 읽고, 문법이 틀린 줄은
// 건너뛰는 값에서 틀렸어도 세지 않는다.
func TestAIClaudeUsageDeltaMatchesEncodingJSON(t *testing.T) {
	const at = `"timestamp":"2026-11-05T01:02:03Z"`
	counted := func(extra string) string {
		return `{` + at + `,"message":{"usage":{"input_tokens":1}},` + extra + `}`
	}
	tokens := func(value string) string {
		return `{` + at + `,"message":{"usage":{"input_tokens":` + value + `}}}`
	}
	nested := func(depth int) string {
		return counted(`"x":` + strings.Repeat("[", depth) + strings.Repeat("]", depth))
	}
	lines := []string{
		`{"timestamp":"2026-11-05T01:02:03.456Z","requestId":"r1","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5,"output_tokens":6}}}`,
		` { "timestamp" : "2026-11-05T01:02:03Z" , "message" : { "usage" : { "input_tokens" : 7 } } } `,
		`{"timestamp":"2026-11-05T01:02:03+09:00","requestId":"r` + aiJSONEscape('2') + `","message":{"id":"m\"2","usage":{"input_tokens":1,"server_tool_use":{"web_search_requests":0},"service_tier":"standard"}}}`,
		`{` + at + `,"message":{"content":[{"type":"text","text":"a \"usage\":{\"input_tokens\":99} \\\\"}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{` + at + `,"message":{"content":[{"type":"tool_use","input":{"usage":{"input_tokens":77}}}],"usage":{"input_tokens":1}}}`,
		`{` + at + `,"message":{"usage":{"input_tokens":1}},"timestamp":"2026-11-06T01:02:03Z"}`,
		`{"time` + aiJSONEscape('s') + `tamp":"2026-11-05T01:02:03Z","m` + aiJSONEscape('e') + `ssage":{"usage":{"input` + aiJSONEscape('_') + `tokens":5}}}`,
		counted(`"x":[true,false,null,-1.5e+3,0,0.25,1E-2,{"a":[]},"s",{}]`),
		tokens("9223372036854775807"),
		tokens("-9223372036854775808"),
		tokens("-0"),
		tokens("null"),
		nested(aiJSONMaxDepth - 1),

		`{` + at + `,"message":{"usage":null}}`,
		`{` + at + `,"message":null,"usage":{"input_tokens":1}}`,
		`{` + at + `,"message":"text with \"usage\""}`,
		tokens(`"1"`), tokens("1.5"), tokens("1e3"), tokens("01"), tokens("-01"), tokens("9223372036854775808"), tokens("true"), tokens("-"),
		counted(`"x":[1,]`), counted(`"x":tru`), counted(`"x":nul`), counted(`"x":nullx`), counted(`"x":{"a" 1}`), counted(`"x":[}`),
		counted(`"x":{"a":1,}`), counted(`"x":1 "y":2`), counted(`"x":01`), counted(`"x":-`), counted(`"x":1.`), counted(`"x":.5`),
		counted(`"x":1e`), counted(`"x":+1`), counted(`"x":"a\"`),
		nested(aiJSONMaxDepth),
		`{` + at + `,"message":{"usage":{"input_tokens":1}}} trailing`,
		`{` + at + `,"message":{"usage":{"input_tokens":1}}`,
		`{"timestamp":"bad","message":{"usage":{"input_tokens":1}}}`,
		`{"timestamp":null,"message":{"usage":{"input_tokens":1}}}`,
	}
	for _, line := range lines {
		aiCompareClaudeLines(t, []string{line})
	}
}

// escape로 쓴 timestamp는 encoding/json과 비교하지 않는다. encoding/json은 Go 1.25와 1.26에서 이 값을 읽지 않고 1.27에서는
// 읽어서, 비교 기준이 Go 버전마다 다르다. JSON 문자열로 유효한 값이므로 aiJSONReader는 어느 버전에서나 읽는다.
func TestAIClaudeUsageDeltaReadsAnEscapedTimestamp(t *testing.T) {
	line := `{"timestamp":"2026-11-05T01:02:03` + aiJSONEscape('Z') + `","message":{"usage":{"input_tokens":1}}}`
	at, usage, ok := aiClaudeUsageDelta([]byte(line), map[string]aiSeenMessage{})
	want := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	if !ok || !at.Equal(want) || usage != (aiUsage{Requests: 1, Input: 1}) {
		t.Fatalf("%s\n got %v %+v %v\nwant %v %+v true", line, at, usage, ok, want, aiUsage{Requests: 1, Input: 1})
	}
}

// 같은 응답은 message.id와 requestId가 모두 같아야 한 번만 센다. 늘어난 token은 처음 기록한 시각에 더한다.
func TestAIClaudeUsageDeltaMatchesEncodingJSONAcrossLines(t *testing.T) {
	line := func(at, id, request string, output int) string {
		return fmt.Sprintf(`{"timestamp":"%s","requestId":"%s","message":{"id":"%s","usage":{"input_tokens":10,"cache_read_input_tokens":100,"output_tokens":%d}}}`, at, request, id, output)
	}
	aiCompareClaudeLines(t, []string{
		line("2026-11-05T01:00:00Z", "m1", "r1", 1),
		line("2026-11-05T01:00:05Z", "m1", "r1", 50),
		line("2026-11-05T01:00:06Z", "m1", "r1", 50),
		line("2026-11-05T01:00:07Z", "m1", "r2", 50),
		line("2026-11-05T01:00:08Z", "m"+aiJSONEscape('1'), "r1", 60),
		line("2026-11-05T01:00:09Z", "", "r1", 5),
		line("2026-11-05T01:00:10Z", "", "r1", 5),
	})
}

func TestAICodexRecordMatchesEncodingJSON(t *testing.T) {
	const at = `"timestamp":"2026-11-05T01:02:03Z"`
	total := func(value string) string {
		return `{` + at + `,"payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":` + value + `}}}}`
	}
	lines := []string{
		`{` + at + `,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":13},"last_token_usage":{"input_tokens":1}}}}`,
		`{` + at + `,"payload":{"type":"token` + aiJSONEscape('_') + `count","info":{"total` + aiJSONEscape('_') + `token_usage":{"total_tokens":7}}}}`,
		`{` + at + `,"payload":{"type":"token_count","info":null}}`,
		`{` + at + `,"payload":{"type":"token_count","info":{}}}`,
		`{` + at + `,"payload":{"type":"agent_message","message":"token_count"}}`,
		total("1e3"), total("013"), total("13"),
		`{` + at + `,"payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":13},"rate_limits":[1,]}}}`,
	}
	for _, line := range lines {
		wantAt, wantTotal, wantOK := aiCodexReferenceRecord([]byte(line))
		record, ok := parseAICodexRecord([]byte(line))
		gotOK := ok && record.kind == "token_count" && record.hasInfo
		if gotOK != wantOK || (gotOK && (record.total != wantTotal || !record.timestamp.Equal(wantAt))) {
			t.Errorf("%.200s\n got %v %+v %v\nwant %v %+v %v", line, record.timestamp, record.total, gotOK, wantAt, wantTotal, wantOK)
		}
	}
}

// aiClaudeBenchmarkLine은 도구 출력이 든 응답 줄이다. 기록 대부분이 이런 긴 문자열이다.
func aiClaudeBenchmarkLine() []byte {
	text := strings.Repeat(`func main() {\n\tfmt.Println(\"hello\")\n}\n`, 1000)
	return []byte(`{"parentUuid":"p","type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"` + text +
		`"}],"usage":{"input_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5,"output_tokens":6,"service_tier":"standard"}},` +
		`"requestId":"r1","timestamp":"2026-11-05T01:02:03.456Z"}`)
}

func BenchmarkAIClaudeUsageDelta(b *testing.B) {
	line := aiClaudeBenchmarkLine()
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		aiClaudeUsageDelta(line, map[string]aiSeenMessage{})
	}
}

func BenchmarkAIClaudeUsageDeltaEncodingJSON(b *testing.B) {
	line := aiClaudeBenchmarkLine()
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		aiClaudeReferenceDelta(line, map[string]aiSeenMessage{})
	}
}
