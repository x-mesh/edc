package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestParseAIClaudeActivityFollowsTheLastStep(t *testing.T) {
	use := `{"type":"assistant","timestamp":"2026-10-29T06:40:20Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cargo test"}}]}}`
	result := `{"type":"user","timestamp":"2026-10-29T06:40:50Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	text := `{"type":"assistant","timestamp":"2026-10-29T06:40:55Z","message":{"content":[{"type":"text","text":"Tests pass."}]}}`

	activity, ok := parseAIClaudeActivity([]byte(use+"\n"), new(aiSessionInfo))
	if !ok || activity.Kind != "tool" || !activity.Running || activity.Text != "Bash: cargo test" {
		t.Fatalf("running tool %+v", activity)
	}
	activity, _ = parseAIClaudeActivity([]byte(use+"\n"+result+"\n"), new(aiSessionInfo))
	if activity.Running || aiActivityText(&activity, time.Now()) != "thinking · after Bash: cargo test" {
		t.Fatalf("finished tool %+v", activity)
	}
	activity, _ = parseAIClaudeActivity([]byte(use+"\n"+result+"\n"+text+"\n"), new(aiSessionInfo))
	if activity.Kind != "text" || activity.Text != "Tests pass." {
		t.Fatalf("text %+v", activity)
	}
}

func TestParseAICodexActivityReadsTheExecCommand(t *testing.T) {
	call := `{"timestamp":"2026-10-29T06:41:42Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"c1",` +
		`"input":"const r = await tools.exec_command({cmd:\"go test ./... \\\"-run\\\" X\", yield_time_ms: 1000});"}}`
	output := `{"timestamp":"2026-10-29T06:41:45Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"ok"}}`
	activity, ok := parseAICodexActivity([]byte(call+"\n"), new(aiSessionInfo))
	if !ok || !activity.Running || activity.Text != `exec_command: go test ./... "-run" X` {
		t.Fatalf("codex command %+v", activity)
	}
	activity, _ = parseAICodexActivity([]byte(call+"\n"+output+"\n"), new(aiSessionInfo))
	if activity.Running {
		t.Fatalf("codex output must end the call %+v", activity)
	}
	patch := `{"timestamp":"2026-10-29T06:41:37Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"c2",` +
		`"input":"const p = await tools.apply_patch(\"*** Begin Patch\\n*** Add File: /w/a.go\\n+package a\");"}}`
	if activity, _ = parseAICodexActivity([]byte(patch+"\n"), new(aiSessionInfo)); activity.Text != "apply_patch: /w/a.go" {
		t.Fatalf("codex patch %+v", activity)
	}
}

func TestAIActivityReaderSkipsACutFirstLineAndCaches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	padding := strings.Repeat("x", int(aiActivityTail))
	line := `{"type":"assistant","timestamp":"2026-10-29T06:40:20Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/w/a.go"}}]}}`
	if err := os.WriteFile(path, []byte(padding+"\n"+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := aiActivityReader{}
	sessions := []aiSession{{Tool: "claude", State: aiStateWorking, Transcript: path, Task: "fix the bug"}, {Tool: "claude", State: aiStateWaiting, Transcript: path}}
	reader.annotate(sessions, time.Now())
	if sessions[0].Activity == nil || sessions[0].Activity.Text != "Read: /w/a.go" || sessions[1].Activity != nil {
		t.Fatalf("annotate %+v", sessions)
	}
	if _, ok := reader[path]; !ok {
		t.Fatal("the reader must cache the transcript")
	}
	sessions[0].Activity = nil
	if aiSessionDetail(sessions[0], time.Now()) != "fix the bug" {
		t.Fatal("a working session without activity shows its task")
	}
}

func TestAIActivityReaderCountsClaudeTokensOnceAndKeepsCounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	first := `{"type":"assistant","timestamp":"2026-10-29T06:40:20Z","gitBranch":"develop","requestId":"r1","message":{"id":"m1","model":"claude-opus-5-5",` +
		`"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":5}}}`
	// 같은 응답의 다음 줄은 output만 늘어난 값을 다시 적는다.
	again := strings.Replace(first, `"output_tokens":5`, `"output_tokens":50`, 1)
	mode := `{"type":"user","timestamp":"2026-10-29T06:40:21Z","permissionMode":"bypassPermissions","message":{"content":"next"}}`
	if err := os.WriteFile(path, []byte(first+"\n"+again+"\n"+mode+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := aiActivityReader{}
	_, _, info := reader.read("claude", path)
	if info.Model != "claude-opus-5-5" || info.Branch != "develop" || info.Mode != "bypassPermissions" || info.Context != 1110 {
		t.Fatalf("info %+v", info)
	}
	if info.Tokens.Input != 110 || info.Tokens.Output != 50 || info.Tokens.Cache != 1000 || info.Tokens.Requests != 1 {
		t.Fatalf("one response must count once: %+v", info.Tokens)
	}
	second := strings.NewReplacer(`"m1"`, `"m2"`, `"r1"`, `"r2"`).Replace(first)
	appendAIFixture(t, path, second+"\n")
	if _, _, info = reader.read("claude", path); info.Tokens.Requests != 2 || info.Tokens.Output != 55 {
		t.Fatalf("new lines must add to the total: %+v", info.Tokens)
	}
}

func TestParseAICodexActivityReadsTheSessionInfo(t *testing.T) {
	data := `{"timestamp":"2026-10-29T06:41:00Z","type":"turn_context","payload":{"model":"gpt-5.6-sol","effort":"xhigh","approval_policy":"never","sandbox_policy":{"type":"read-only"}}}
{"timestamp":"2026-10-29T06:41:10Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":41519,"cached_input_tokens":1519,"output_tokens":2214,"total_tokens":43733},"last_token_usage":{"input_tokens":9500,"output_tokens":10},"model_context_window":950000}}}
`
	info := aiSessionInfo{}
	parseAICodexActivity([]byte(data), &info)
	if info.Model != "gpt-5.6-sol" || info.Effort != "xhigh" || info.Mode != "never" || info.Sandbox != "read-only" {
		t.Fatalf("codex settings %+v", info)
	}
	if info.Tokens.Input != 40000 || info.Tokens.Cache != 1519 || info.Context != 9500 || info.ContextWindow != 950000 {
		t.Fatalf("codex tokens %+v", info)
	}
	lines := aiSessionInfoLines(aiSession{Tool: "codex", Info: &info}, time.Now())
	if len(lines) < 2 || lines[1] != [2]string{"context", "9.5K / 950K (1%)"} {
		t.Fatalf("info lines %q", lines)
	}
}

func TestParseAIActivityCountsTheTurnAndItsFailures(t *testing.T) {
	claude := `{"type":"user","timestamp":"2026-10-29T06:40:00Z","message":{"content":"old prompt"}}
{"type":"assistant","timestamp":"2026-10-29T06:40:01Z","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"false"}}]}}
{"type":"user","timestamp":"2026-10-29T06:40:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"a","is_error":true,"content":"old failure"}]}}
{"type":"user","timestamp":"2026-10-29T06:41:00Z","message":{"content":"new prompt"}}
{"type":"assistant","timestamp":"2026-10-29T06:41:01Z","message":{"content":[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"go test"}},{"type":"tool_use","id":"c","name":"Read","input":{"file_path":"/w/a.go"}}]}}
{"type":"user","timestamp":"2026-10-29T06:41:09Z","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":[{"type":"text","text":"Exit code 1\n--- FAIL: TestX"}]}]}}
`
	info := aiSessionInfo{}
	parseAIClaudeActivity([]byte(claude), &info)
	if info.TurnTools != 2 || info.TurnFailed != 1 || info.TurnPartial || !strings.HasPrefix(info.LastError, "Exit code 1 --- FAIL") {
		t.Fatalf("claude turn %+v", info)
	}

	codex := `{"timestamp":"2026-10-29T06:41:00Z","type":"event_msg","payload":{"type":"task_started","model_context_window":828400}}
{"timestamp":"2026-10-29T06:41:01Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"tools.exec_command({cmd:\"make\"})"}}
{"timestamp":"2026-10-29T06:41:05Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":[{"type":"input_text","text":"{\"exit_code\":2,\"output\":\"make: *** [check] Error 2\"}"}]}}
`
	info = aiSessionInfo{}
	parseAICodexActivity([]byte(codex), &info)
	if info.TurnTools != 1 || info.TurnFailed != 1 || info.ContextWindow != 828400 || info.LastError != "exit 2 · make: *** [check] Error 2" {
		t.Fatalf("codex turn %+v", info)
	}

	partial := aiSessionInfo{}
	parseAIClaudeActivity([]byte(strings.SplitN(claude, "\n", 2)[1]), &partial)
	if !partial.TurnPartial && partial.TurnTools == 0 {
		t.Fatalf("a turn without its start is partial %+v", partial)
	}
}

func TestKeepAISessionInfoFillsWhatTheTailLost(t *testing.T) {
	info := aiSessionInfo{TurnTools: 3}
	keepAISessionInfo(&info, aiSessionInfo{Model: "claude-opus-5-5", Context: 722292, Branch: "develop", Last: aiUsage{Input: 10}})
	if info.Model != "claude-opus-5-5" || info.Context != 722292 || info.Branch != "develop" || info.Last.Input != 10 || info.TurnTools != 3 {
		t.Fatalf("kept %+v", info)
	}
}

func TestAITokenRateComparesTheLastFiveMinutesWithTheSession(t *testing.T) {
	now := time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC)
	points := []aiTokenPoint{
		{at: now.Add(-60 * time.Minute), total: 10_000},
		{at: now.Add(-6 * time.Minute), total: 60_000},
		{at: now.Add(-time.Minute), total: 160_000},
	}
	rate, fast := aiTokenRate(points, now)
	if rate != 20_000 || !fast {
		t.Fatalf("rate %d fast %v", rate, fast)
	}
	if rate, fast = aiTokenRate(points[:2], now); rate != 0 || fast {
		t.Fatalf("no new tokens in the window: %d %v", rate, fast)
	}
}

func TestAISessionContextEstimatesTheClaudeWindow(t *testing.T) {
	large := aiSession{Tool: "claude", Info: &aiSessionInfo{Context: 722292}}
	if text, percent := aiSessionContext(large); text != "722K 72%" || percent != 72 {
		t.Fatalf("large %q %d", text, percent)
	}
	small := aiSession{Tool: "claude", Info: &aiSessionInfo{Context: 95293}}
	if text, percent := aiSessionContext(small); text != "95K" || percent != -1 {
		t.Fatalf("small %q %d", text, percent)
	}
	if aiContextBar(72) != "▰▰▰▰▱" || aiContextBar(5) != "▱▱▱▱▱" {
		t.Fatalf("bar %q %q", aiContextBar(72), aiContextBar(5))
	}
	head := aiSessionHead(large, time.Now())
	line := aiSessionFit(head, strings.Repeat("긴 설명 ", 40), large, 140, true)
	if width := liveWidth(line); width != 140 || !strings.HasSuffix(ansi.Strip(line), "722K 72%") {
		t.Fatalf("fit width %d: %q", width, ansi.Strip(line))
	}
}
