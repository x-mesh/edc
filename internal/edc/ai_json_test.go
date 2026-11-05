package edc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// aiClaudeReferenceDelta는 encoding/json으로 줄 전체를 읽던 이전 구현이다. aiJSONReader가 같은 값을 읽는지 비교하는 기준이다.
func aiClaudeReferenceDelta(line []byte) (time.Time, aiUsage, bool) {
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
	return record.Timestamp, aiUsage{Requests: 1, Input: raw.Input + raw.CacheCreation, Output: raw.Output, Cache: raw.CacheRead}, true
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

// 본문 문자열과 도구 입력에 든 "usage"는 message.usage가 아니다. 줄이 깨졌거나 값의 형식이 다르면 세지 않는다.
func TestAIClaudeUsageDeltaMatchesEncodingJSON(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-11-05T01:02:03.456Z","requestId":"r1","message":{"id":"m1","usage":{"input_tokens":3,"cache_creation_input_tokens":4,"cache_read_input_tokens":5,"output_tokens":6}}}`,
		` { "timestamp" : "2026-11-05T01:02:03Z" , "message" : { "usage" : { "input_tokens" : 7 } } } `,
		`{"timestamp":"2026-11-05T01:02:03+09:00","requestId":"r1","message":{"id":"m\"1","usage":{"input_tokens":1,"server_tool_use":{"web_search_requests":0},"service_tier":"standard"}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"content":[{"type":"text","text":"a \"usage\":{\"input_tokens\":99} \\\\"}],"usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"content":[{"type":"tool_use","input":{"usage":{"input_tokens":77}}}],"usage":{"input_tokens":1}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":{"input_tokens":1}},"timestamp":"2026-11-06T01:02:03Z"}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":null}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":null,"usage":{"input_tokens":1}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":"text with \"usage\""}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":{"input_tokens":"1"}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":{"input_tokens":1.5}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":{"input_tokens":1}}} trailing`,
		`{"timestamp":"2026-11-05T01:02:03Z","message":{"usage":{"input_tokens":1}}`,
		`{"timestamp":"bad","message":{"usage":{"input_tokens":1}}}`,
		`{"timestamp":null,"message":{"usage":{"input_tokens":1}}}`,
	}
	for _, line := range lines {
		wantAt, wantUsage, wantOK := aiClaudeReferenceDelta([]byte(line))
		gotAt, gotUsage, gotOK := aiClaudeUsageDelta([]byte(line), map[string]aiSeenMessage{})
		if gotOK != wantOK || gotUsage != wantUsage || !gotAt.Equal(wantAt) {
			t.Errorf("%s\n got %v %+v %v\nwant %v %+v %v", line, gotAt, gotUsage, gotOK, wantAt, wantUsage, wantOK)
		}
	}
}

func TestAICodexRecordMatchesEncodingJSON(t *testing.T) {
	lines := []string{
		`{"timestamp":"2026-11-05T01:02:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":13},"last_token_usage":{"input_tokens":1}}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","payload":{"type":"token_count","info":null}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","payload":{"type":"token_count","info":{}}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","payload":{"type":"agent_message","message":"token_count"}}`,
		`{"timestamp":"2026-11-05T01:02:03Z","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":1e3}}}}`,
	}
	for _, line := range lines {
		wantAt, wantTotal, wantOK := aiCodexReferenceRecord([]byte(line))
		record, ok := parseAICodexRecord([]byte(line))
		gotOK := ok && record.kind == "token_count" && record.hasInfo
		if gotOK != wantOK || (gotOK && (record.total != wantTotal || !record.timestamp.Equal(wantAt))) {
			t.Errorf("%s\n got %v %+v %v\nwant %v %+v %v", line, record.timestamp, record.total, gotOK, wantAt, wantTotal, wantOK)
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
		aiClaudeReferenceDelta(line)
	}
}
