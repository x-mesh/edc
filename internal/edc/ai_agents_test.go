package edc

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// testAgentLines는 Claude Code subagent 기록의 줄 모양이다. 실제 기록에서 필요한 키만 남겼다.
var testAgentLines = []string{
	`{"type":"user","timestamp":"2026-11-17T00:38:49Z","cwd":"/work/edc","message":{"role":"user","content":"Plan the split of tm_agent.rs"}}`,
	`{"type":"attachment","timestamp":"2026-11-17T00:38:50Z","attachment":{"type":"skill_listing"}}`,
	`{"type":"api-request","timestamp":"2026-11-17T00:38:51Z"}`,
	// api-request-blob은 보낸 대화의 사본이라 같은 내용이 다시 나와도 활동이 아니다.
	`{"type":"api-request-blob","message":{"content":[{"type":"text","text":"copy"},{"type":"tool_use","id":"x","name":"Bash","input":{}}]}}`,
	`{"type":"assistant","timestamp":"2026-11-17T00:38:53Z","message":{"stop_reason":"tool_use","content":[{"type":"thinking","thinking":"secret plan"}]}}`,
	`{"type":"assistant","timestamp":"2026-11-17T00:38:53Z","message":{"stop_reason":"tool_use","content":[{"type":"text","text":"Read the repo\nfirst."}]}}`,
	`{"type":"assistant","timestamp":"2026-11-17T00:38:56Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"cd /work && wc -l src/tm_agent.rs","description":"Count lines"}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/work/src/tm_agent.rs"}}]}}`,
	// 병렬 호출의 결과는 순서가 바뀌어 온다. 실패한 쪽 이름은 ID로 찾는다.
	`{"type":"user","timestamp":"2026-11-17T00:38:57Z","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"File does not exist."},{"type":"tool_result","tool_use_id":"t1","content":"12000"}]}}`,
	`{"type":"user","timestamp":"2026-11-17T00:38:58Z","isMeta":true,"message":{"content":"Base directory for this skill"}}`,
	`{"type":"api-request","timestamp":"2026-11-17T00:38:59Z"}`,
	`{"type":"assistant","timestamp":"2026-11-17T00:39:10Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Done: three modules."}]}}`,
}

func testAgentSteps(t *testing.T, lines []string) ([]aiAgentStep, *aiAgentParser) {
	t.Helper()
	parser := newAIAgentParser()
	var steps []aiAgentStep
	for _, line := range lines {
		steps = append(steps, parser.line([]byte(line))...)
	}
	return steps, parser
}

func TestAIAgentParserSteps(t *testing.T) {
	steps, parser := testAgentSteps(t, testAgentLines)
	var got []string
	for _, step := range steps {
		got = append(got, aiAgentStepText(step))
	}
	want := []string{
		"▶ Plan the split of tm_agent.rs",
		"· Read the repo first.",
		"Bash Count lines · cd /work && wc -l src/tm_agent.rs",
		"Read /work/src/tm_agent.rs",
		"✗ Read File does not exist.",
		"· Done: three modules.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("steps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !parser.stopped || parser.cwd != "/work/edc" || !parser.started.Equal(time.Date(2026, 11, 17, 0, 38, 49, 0, time.UTC)) {
		t.Fatalf("stopped=%v cwd=%q started=%v", parser.stopped, parser.cwd, parser.started)
	}
	for _, step := range steps {
		if strings.Contains(step.Text, "secret") {
			t.Fatalf("thinking text leaked: %q", step.Text)
		}
	}
}

// 응답이 도구 호출로 끝났거나 다음 요청이 시작되면 실행 중이다. SendMessage로 이어 준 지시도 다시 실행 중으로 만든다.
func TestAIAgentParserStopped(t *testing.T) {
	for _, test := range []struct {
		name  string
		lines []string
		want  bool
	}{
		{"end_turn", testAgentLines, true},
		{"waiting for a tool", testAgentLines[:7], false},
		{"request after end_turn", append(slices.Clone(testAgentLines), `{"type":"api-request","timestamp":"2026-11-17T00:40:00Z"}`), false},
		{"resumed", append(slices.Clone(testAgentLines), `{"type":"user","timestamp":"2026-11-17T00:40:00Z","message":{"content":"Also check the tests"}}`), false},
		{"attachment after end_turn", append(slices.Clone(testAgentLines), `{"type":"attachment","timestamp":"2026-11-17T00:40:00Z"}`), true},
	} {
		if _, parser := testAgentSteps(t, test.lines); parser.stopped != test.want {
			t.Errorf("%s: stopped = %v, want %v", test.name, parser.stopped, test.want)
		}
	}
}

// 쓰는 중인 마지막 줄은 다음 읽기로 미룬다. 64 KiB를 넘는 줄도 읽는다.
func TestReadAIAgentLinesWaitsForTheLineEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-a1.jsonl")
	long := `{"type":"api-request-blob","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 600<<10) + `"}]}}`
	half := testAgentLines[5][:40]
	if err := os.WriteFile(path, []byte(testAgentLines[0]+"\n"+long+"\n"+half), 0o600); err != nil {
		t.Fatal(err)
	}
	parser := newAIAgentParser()
	var steps []aiAgentStep
	emit := func(step aiAgentStep) { steps = append(steps, step) }
	offset, err := readAIAgentLines(path, 0, parser, emit)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len(testAgentLines[0]) + 1 + len(long) + 1); offset != want || len(steps) != 1 {
		t.Fatalf("offset = %d, want %d; steps = %d, want 1", offset, want, len(steps))
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(testAgentLines[5][40:] + "\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := readAIAgentLines(path, offset, parser, emit); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[1].Text != "Read the repo first." {
		t.Fatalf("steps after the line ended = %+v", steps)
	}
}

func testAgentFile(t *testing.T, claudeDir, session, id string, lines []string, meta string, modified time.Time) string {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", "-work-edc", session, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if meta != "" {
		if err := os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func testShellLaunch(id, toolUse, description, output string, at time.Time) []string {
	stamp := at.UTC().Format(time.RFC3339)
	return []string{
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"go test ./...","description":%q,"run_in_background":true}}]}}`, stamp, toolUse, description),
		fmt.Sprintf(`{"type":"user","timestamp":%q,"toolUseResult":{"stdout":"","backgroundTaskId":%q},"message":{"content":[{"tool_use_id":%q,"type":"tool_result","content":"Command running in background with ID: %s. Output is being written to: %s. You will be notified when it completes."}]}}`, stamp, id, toolUse, id, output),
	}
}

func testNotification(id, status string, at time.Time) string {
	return fmt.Sprintf(`{"type":"queue-operation","timestamp":%q,"content":"<task-notification>\n<task-id>%s</task-id>\n<status>%s</status>\n<summary>done</summary>\n</task-notification>"}`, at.UTC().Format(time.RFC3339), id, status)
}

func testSessionFile(t *testing.T, claudeDir, session string, lines []string, modified time.Time) string {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", "-work-edc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func testWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 지금 도는 것만 세션별로 묶는다. shell은 시작 기록 뒤에 완료 알림도 출력 끝의 종료 줄도 없으면 돈다.
// agent는 마지막 응답이 도구 호출로 끝났거나, 멈췄어도 띄운 shell이 돌면 돈다. 질문의 답을 기다리는 세션은 일하는 중이 아니다.
func TestAITaskReaderListsOnlyRunningWork(t *testing.T) {
	claudeDir, outputs := t.TempDir(), t.TempDir()
	now := time.Now()
	start := now.Add(-5 * time.Minute)
	running, finished, trailed, agentShell := filepath.Join(outputs, "b1.output"), filepath.Join(outputs, "b2.output"), filepath.Join(outputs, "b3.output"), filepath.Join(outputs, "b9.output")
	testWrite(t, running, "compiling\rstep 1\rstep 2\n")
	testWrite(t, finished, "ok\n")
	testWrite(t, trailed, "ok\n[exited with code 0]\n")
	testWrite(t, agentShell, "")
	session := "0c03d81e-4cf3-4976-9aac-e837162f0118"
	lines := []string{testAgentLines[0]}
	lines = append(lines, testShellLaunch("b1", "u1", "run the tests", running, start)...)
	lines = append(lines, testShellLaunch("b2", "u2", "build", finished, start)...)
	lines = append(lines, testNotification("b2", "completed", start.Add(time.Minute)))
	lines = append(lines, testShellLaunch("b3", "u3", "vet", trailed, start)...)
	lines = append(lines, testAgentLines[len(testAgentLines)-1])
	testSessionFile(t, claudeDir, session, lines, now.Add(-time.Minute))
	asking := `{"type":"assistant","timestamp":"2026-11-17T00:40:00Z","cwd":"/work/other","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"q1","name":"AskUserQuestion","input":{}}]}}`
	testSessionFile(t, claudeDir, "11111111-0000-0000-0000-000000000000", []string{testAgentLines[0], asking}, now)
	meta := `{"agentType":"Explore","description":"find the parser","model":"fable"}`
	testAgentFile(t, claudeDir, session, "a111", testAgentLines, "", now)
	testAgentFile(t, claudeDir, session, "a222", testAgentLines[:7], meta, now.Add(-10*time.Second))
	waiting := append(slices.Clone(testAgentLines[:len(testAgentLines)-1]), testShellLaunch("b9", "u9", "wait for CI", agentShell, start)...)
	testAgentFile(t, claudeDir, session, "a333", append(waiting, testAgentLines[len(testAgentLines)-1]), "", now.Add(-time.Minute))

	tasks, err := newAITaskReaderWithout().list(claudeDir, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, task := range tasks {
		got = append(got, fmt.Sprintf("%s %s %s %d", task.Kind, task.ID, task.State, task.Depth))
	}
	want := []string{"session " + session + " idle 0", "agent a222 running 1", "agent a333 running 1", "shell b9 running 2", "shell b1 running 1"}
	if !slices.Equal(got, want) {
		t.Fatalf("tasks =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	shell := tasks[4]
	if shell.Description != "run the tests" || shell.Output != running || shell.Step == nil || shell.Step.Text != "step 2" || shell.Cwd != "/work/edc" {
		t.Fatalf("shell = %+v step = %+v", shell, shell.Step)
	}
	if agent := tasks[1]; agent.Type != "Explore" || agent.Description != "find the parser" || agent.Step == nil || agent.Step.Tool != "Read" {
		t.Fatalf("agent = %+v", agent)
	}
	if child := tasks[3]; child.Parent != "a333" || child.Session != session {
		t.Fatalf("agent shell = %+v", child)
	}
}

// 강제로 멈춘 agent는 killed 알림을 받는다. 그 뒤에 기록이 늘지 않았으면 마지막 응답이 도구 호출이어도 돌지 않는다.
func TestAITaskReaderSkipsKilledAgent(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Now()
	session := "22222222-0000-0000-0000-000000000000"
	launch := `{"type":"user","timestamp":"2026-11-17T00:38:00Z","toolUseResult":{"status":"async_launched","agentId":"a555","description":"scan"},"message":{"content":[{"tool_use_id":"x","type":"tool_result","content":"Async agent launched"}]}}`
	testAgentFile(t, claudeDir, session, "a555", testAgentLines[:7], "", now.Add(-time.Minute))
	testSessionFile(t, claudeDir, session, []string{testAgentLines[0], launch, testNotification("a555", "killed", now), testAgentLines[len(testAgentLines)-1]}, now)
	tasks, err := newAITaskReaderWithout().list(claudeDir, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want none", tasks)
	}
}

func newAITaskReaderWithout() *aiTaskReader {
	return &aiTaskReader{transcripts: map[string]*aiTranscript{}}
}

func TestMatchAITasks(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "agent-a5f19b44038a50323.jsonl")
	testWrite(t, transcript, "")
	output := filepath.Join(dir, "a5f19b44038a50323.output")
	if err := os.Symlink(transcript, output); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(transcript)
	if err != nil {
		t.Fatal(err)
	}
	shellOutput := filepath.Join(dir, "bjfk162ua.output")
	candidates := []aiTask{
		{Kind: "session", ID: "e9c56a9e-0803-4a9b-ade5-35508444fd9d", Session: "e9c56a9e-0803-4a9b-ade5-35508444fd9d"},
		{Kind: "agent", ID: "a5f19b44038a50323", Session: "e9c56a9e-0803-4a9b-ade5-35508444fd9d", Transcript: resolved},
		{Kind: "agent", ID: "a850fae5c00cedc1e", Session: "54635a48-537d-4fcd-9798-3f164ae0d63a"},
		{Kind: "shell", ID: "bjfk162ua", Session: "6ede073f-b5dc-47b4-838e-4a71457112f8", Output: shellOutput},
	}
	for _, test := range []struct {
		query string
		want  []string
	}{
		{"a5f19b", []string{"a5f19b44038a50323"}},
		{"agent-a850fae5c00cedc1e.jsonl", []string{"a850fae5c00cedc1e"}},
		{"54635a48", []string{"a850fae5c00cedc1e"}},
		{output, []string{"a5f19b44038a50323"}},
		{shellOutput, []string{"bjfk162ua"}},
		{"bjfk", []string{"bjfk162ua"}},
		{"e9c56a9e-0803-4a9b-ade5-35508444fd9d", []string{"e9c56a9e-0803-4a9b-ade5-35508444fd9d"}},
		{"e9c56a9e", []string{"e9c56a9e-0803-4a9b-ade5-35508444fd9d", "a5f19b44038a50323"}},
		{"5363c301", nil},
	} {
		var got []string
		for _, task := range matchAITasks(candidates, test.query) {
			got = append(got, task.ID)
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("match %q = %q, want %q", test.query, got, test.want)
		}
	}
}

// shell 활동은 출력 줄이다. 진행 막대는 마지막 내용만 남기고, 종료 줄이 붙으면 상태 줄을 쓰고 더 읽지 않는다.
func TestAITaskLogFollowsShellUntilItEnds(t *testing.T) {
	claudeDir, outputs := t.TempDir(), t.TempDir()
	now := time.Now()
	output := filepath.Join(outputs, "b1.output")
	testWrite(t, output, "start\n10%\r50%\r")
	session := "33333333-0000-0000-0000-000000000000"
	testSessionFile(t, claudeDir, session, append([]string{testAgentLines[0]}, testShellLaunch("b1", "u1", "build", output, now)...), now)
	reader := newAITaskReaderWithout()
	log := newAITaskLog(aiTask{Kind: "shell", ID: "b1", Output: output})
	steps, err := log.read(reader, claudeDir, now)
	if err != nil || len(steps) != 1 || steps[0].Text != "start" {
		t.Fatalf("first read = %+v, %v", steps, err)
	}
	testWrite(t, output, "start\n10%\r50%\r100%\n\x1b[31mfail\x1b[0m\n[exited with code 1]\n")
	steps, _ = log.read(reader, claudeDir, now)
	var got []string
	for _, step := range steps {
		got = append(got, aiAgentStepText(step))
	}
	if want := []string{"│ 100%", "│ fail", "│ [exited with code 1]", "■ failed"}; !slices.Equal(got, want) {
		t.Fatalf("steps = %q, want %q", got, want)
	}
	if steps, _ := log.read(reader, claudeDir, now); steps != nil || !log.done {
		t.Fatalf("read after the end = %+v, done = %v", steps, log.done)
	}
}

// 화면은 연 작업의 기록만 붙이고, 읽는 사이에 다른 작업으로 바꾸면 앞선 읽기를 버린다. 연 작업이 끝나 목록에서 빠져도 화면을 유지한다.
func TestAIAgentsModelLog(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Now()
	testAgentFile(t, claudeDir, "s1", "a111", testAgentLines[:8], "", now)
	testSessionFile(t, claudeDir, "s1", []string{testAgentLines[0], testAgentLines[len(testAgentLines)-1]}, now)
	model := aiAgentsModel{feed: &aiAgentsFeed{claudeDir: claudeDir, reader: newAITaskReaderWithout()}, width: 120, height: 30}
	next, _ := model.Update(model.feed.read(now, nil, 0))
	model = next.(aiAgentsModel)
	if model.selected != "session/s1" || len(model.tasks) != 2 {
		t.Fatalf("selected = %q tasks = %+v", model.selected, model.tasks)
	}
	model.move(1)
	next, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(aiAgentsModel)
	if model.target == nil || model.target.ID != "a111" {
		t.Fatalf("target = %+v", model.target)
	}
	next, _ = model.Update(aiAgentsReadMsg{key: "agent/other", steps: []aiAgentStep{{Kind: "text", Text: "stale"}}, generation: model.generation, at: now})
	model = next.(aiAgentsModel)
	if len(model.steps) != 0 {
		t.Fatalf("steps from another task = %+v", model.steps)
	}
	next, _ = model.Update(model.feed.read(now, model.target, model.generation))
	model = next.(aiAgentsModel)
	if len(model.steps) != 5 {
		t.Fatalf("steps = %+v", model.steps)
	}
	if view := model.View().Content; !strings.Contains(view, "✗ Read File does not exist.") {
		t.Fatalf("log view =\n%s", view)
	}
	// agent가 끝나 목록에서 빠져도 연 화면은 그대로 읽는다.
	path := filepath.Join(claudeDir, "projects", "-work-edc", "s1", "subagents", "agent-a111.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(strings.Join(testAgentLines[8:], "\n") + "\n")
	file.Close()
	next, _ = model.Update(model.feed.read(now, model.target, model.generation))
	model = next.(aiAgentsModel)
	if len(model.tasks) != 0 || len(model.steps) != 7 || model.steps[6].Kind != "stopped" {
		t.Fatalf("tasks = %d steps = %+v", len(model.tasks), model.steps)
	}
	next, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if model = next.(aiAgentsModel); model.target != nil || model.steps != nil {
		t.Fatalf("esc kept the log: target=%v steps=%d", model.target, len(model.steps))
	}
}

// Enter는 새 읽기를 곧바로 건다. 앞선 세대의 결과는 다음 읽기를 걸지 않아 읽기가 하나만 이어진다.
// 둘이 이어지면 두 goroutine이 reader의 map을 함께 고쳐 프로세스가 끝났다.
func TestAIAgentsModelKeepsOneReadChain(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Now()
	testAgentFile(t, claudeDir, "s1", "a111", testAgentLines[:7], "", now)
	testSessionFile(t, claudeDir, "s1", []string{testAgentLines[0], testAgentLines[len(testAgentLines)-1]}, now)
	model := aiAgentsModel{feed: &aiAgentsFeed{claudeDir: claudeDir, reader: newAITaskReaderWithout()}, width: 120, height: 30}
	stale := model.feed.read(now, nil, model.generation)
	next, cmd := model.Update(stale)
	if model = next.(aiAgentsModel); cmd == nil {
		t.Fatal("the first read did not schedule the next one")
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: tea.KeyEscape}, {Code: tea.KeyEnter}} {
		next, _ = model.Update(key)
		model = next.(aiAgentsModel)
	}
	if _, cmd := model.Update(stale); cmd != nil {
		t.Fatal("a read from an earlier generation scheduled another read")
	}
	if _, cmd := model.Update(model.feed.read(now, model.target, model.generation)); cmd == nil {
		t.Fatal("the current read did not schedule the next one")
	}
}

// 위로 올린 채 기록을 처음부터 다시 받아도 화면이 범위 밖을 자르지 않는다.
func TestAIAgentsModelClampsScrollAfterReset(t *testing.T) {
	target := aiTask{Kind: "agent", ID: "a111"}
	model := aiAgentsModel{target: &target, width: 120, height: 10}
	for range 30 {
		model.steps = append(model.steps, aiAgentStep{Kind: "text", Text: "old"})
	}
	model.scroll = 20
	next, _ := model.Update(aiAgentsReadMsg{key: "agent/a111", reset: true, steps: []aiAgentStep{{Kind: "text", Text: "new"}}})
	model = next.(aiAgentsModel)
	if model.scroll != 0 || len(model.steps) != 1 {
		t.Fatalf("scroll = %d, steps = %d after a reset", model.scroll, len(model.steps))
	}
	_ = model.View()
}

// 백그라운드 작업 완료 알림은 태그와 출력 경로가 앞에 와서 160자로 자르면 무엇이 끝났는지가 잘린다. 요약 한 줄로 바꾼다.
func TestAIPromptTextSummarizesTaskNotifications(t *testing.T) {
	full := "<task-notification>\n<task-id>bsyvo8wbn</task-id>\n<tool-use-id>toolu_01A9</tool-use-id>\n<output-file>/private/tmp/claude-501/x/tasks/bsyvo8wbn.output</output-file>\n<status>completed</status>\n<summary>Background command \"go test &amp;&amp; vet\" completed (exit code 0)</summary>\n</task-notification>"
	for _, test := range []struct{ name, prompt, want string }{
		{"full", full, `↩ Background command "go test && vet" completed (exit code 0)`},
		{"after the system preamble", "[SYSTEM NOTIFICATION - NOT USER INPUT]\nThis is an automated event.\n\n" + full, `↩ Background command "go test && vet" completed (exit code 0)`},
		{"clipped by an older edc", aiHookClip(full, false), "↩ task bsyvo8wbn"},
		{"plain prompt", "fix the build", "fix the build"},
	} {
		if got := aiPromptText(test.prompt); got != test.want {
			t.Errorf("%s: %q, want %q", test.name, got, test.want)
		}
	}
	event, ok := parseAIHookInput("claude", []byte(`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":`+strconv.Quote(full)+`}`), time.Now())
	if !ok || event.Detail != `↩ Background command "go test && vet" completed (exit code 0)` {
		t.Fatalf("hook detail = %q", event.Detail)
	}
}

// 색은 글자를 바꾸지 않는다. 색을 지우면 원래 설명과 같다.
func TestAIPaintKeepsTheText(t *testing.T) {
	for _, text := range []string{"53s · Bash: grep -n x Makefile", "✗2 thinking · after Read: /a/b", "↩ task b1 completed", ""} {
		if got := ansi.Strip(aiPaintActivity(text)); got != text {
			t.Errorf("activity %q painted to %q", text, got)
		}
	}
	for _, line := range [][2]string{{"model", "claude-opus-5-5 · feat/gk-dash · bypassPermissions"}, {"turn", "1m05s · 2 tools · 1 failed"}, {"tokens", "in 463K · out 246K · cache 66.5M · last in 489K (cache 99%) out 679 · 3.9K/min"}} {
		if got := ansi.Strip(aiPaintInfo(line[0], line[1], 48)); got != line[1] {
			t.Errorf("%s %q painted to %q", line[0], line[1], got)
		}
	}
	for _, step := range []aiAgentStep{{Kind: "tool", Tool: "Bash", Text: "run · go test"}, {Kind: "output", Text: "ok"}, {Kind: "failed", Tool: "Read", Text: "missing"}} {
		if got := ansi.Strip(aiPaintStep(step)); got != aiAgentStepText(step) {
			t.Errorf("step %+v painted to %q", step, got)
		}
	}
}

// 세션 줄 아래에 그 세션의 agent와 shell을 들여 쓴다. 목록이 화면보다 길어도 고른 세션 줄이 보인다.
func TestAISessionsRowsShowRunningChildren(t *testing.T) {
	now := time.Now()
	tasks := []aiTask{
		{Kind: "session", ID: "s1", Session: "s1"},
		{Kind: "agent", ID: "a111111111", Session: "s1", Depth: 1, Type: "Explore", Description: "find", Started: now.Add(-time.Minute)},
		{Kind: "shell", ID: "b9", Session: "s1", Parent: "a111111111", Depth: 2, Description: "wait", Started: now.Add(-30 * time.Second), Step: &aiAgentStep{Kind: "output", Text: "50%"}},
		{Kind: "shell", ID: "b1", Session: "s2", Depth: 1, Description: "tests", Started: now.Add(-time.Second)},
	}
	sessions := []aiSession{{Tool: "claude", Session: "s1", State: aiStateWorking, Since: now}, {Tool: "codex", Session: "s2", Since: now}, {Tool: "claude", Session: "s3", Since: now}}
	attachAITasks(sessions, tasks)
	if len(sessions[0].Tasks) != 2 || sessions[1].Tasks != nil || sessions[2].Tasks != nil {
		t.Fatalf("tasks = %+v / %+v / %+v", sessions[0].Tasks, sessions[1].Tasks, sessions[2].Tasks)
	}
	// 본 대화가 쉬어도 띄운 작업이 돌면 working으로 올라온다. 입력 필요는 그대로 둔다.
	waiting := []aiSession{
		{Tool: "claude", Session: "w0", State: aiStateWaiting, Reason: "done", Detail: "last reply", Since: now.Add(-time.Hour)},
		{Tool: "claude", Session: "s1", State: aiStateWaiting, Reason: "done", Detail: "kicked off the tests", Since: now},
		{Tool: "claude", Session: "s3", State: aiStateNeeds, Reason: "question", Since: now},
	}
	attachAITasks(waiting, append(slices.Clone(tasks), aiTask{Kind: "shell", ID: "b2", Session: "s3", Depth: 1}))
	if waiting[0].Session != "s3" || waiting[0].State != aiStateNeeds || waiting[1].Session != "s1" || waiting[1].State != aiStateWorking ||
		waiting[1].Reason != "background" || waiting[2].State != aiStateWaiting {
		t.Fatalf("sessions = %+v", waiting)
	}
	if detail := aiSessionDetail(waiting[1], now); detail != "kicked off the tests" {
		t.Fatalf("background detail = %q", detail)
	}
	if got := aiChildRow(sessions[0].Tasks[1], now, 0, false); got != "      └ $ shell          b9           30s wait  │ 50%" {
		t.Fatalf("child row = %q", got)
	}
	model := aiSessionsModel{sessions: sessions, now: now, width: 120, selected: aiSessionKey(sessions[2])}
	if model.rowCount() != 5 {
		t.Fatalf("row count = %d", model.rowCount())
	}
	rows := model.rows(2)
	if len(rows) != 2 || !strings.Contains(rows[1], liveSelectedBar) || strings.Contains(rows[0], liveSelectedBar) {
		t.Fatalf("rows = %q", rows)
	}
	if plain := ansi.Strip(strings.Join(model.rows(5), "\n")); !strings.Contains(plain, "└ ◆ Explore") || !strings.Contains(plain, "   └ $ shell") {
		t.Fatalf("rows =\n%s", plain)
	}
}

// 상태와 이유를 합친 문구는 모든 언어에서 상태 칸에 들어간다. 넘치면 뒤 열이 밀린다.
func TestAISessionStateTextFitsTheColumn(t *testing.T) {
	previous := activeLang
	t.Cleanup(func() { activeLang = previous })
	reasons := map[aiSessionState][]string{
		aiStateNeeds:   {"permission", "input", "question", "ask"},
		aiStateFailed:  {"error"},
		aiStateWorking: {"", "background"},
		aiStateWaiting: {"started", "done", "idle"},
		aiStateEnded:   {"exited", "stale"},
	}
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for state, list := range reasons {
			for _, reason := range list {
				text := aiSessionStateText(aiSession{State: state, Reason: reason})
				if width := liveWidth(text); width > aiSessionStateWidth {
					t.Errorf("%s %s/%s: %q is %d columns, more than %d", language, state, reason, text, width, aiSessionStateWidth)
				}
			}
		}
	}
}

// 새 세션 기록은 프로젝트 디렉터리의 수정 시각으로 다음 읽기에 바로 잡는다. 창 밖의 기록은 aiTaskSweep마다만 다시 보고,
// 그 사이에 다시 이어 쓴 기록은 다음 전체 확인에서 잡는다.
func TestAITaskReaderSessionFilesNoticesNewAndResumedSessions(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Now()
	old := testSessionFile(t, claudeDir, "old", []string{testAgentLines[0]}, now.Add(-48*time.Hour))
	reader := newAITaskReaderWithout()
	names := func(files []aiTaskFile) []string {
		var got []string
		for _, file := range files {
			got = append(got, filepath.Base(file.path))
		}
		slices.Sort(got)
		return got
	}
	if got := names(reader.sessionFiles(claudeDir, now)); got != nil {
		t.Fatalf("first read = %q, want none", got)
	}
	dir := filepath.Dir(old)
	testSessionFile(t, claudeDir, "new", []string{testAgentLines[0]}, now)
	// 같은 초 안에 만들어도 디렉터리 수정 시각이 바뀌었다고 보게 한다.
	if err := os.Chtimes(dir, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := names(reader.sessionFiles(claudeDir, now.Add(time.Second))); !slices.Equal(got, []string{"new.jsonl"}) {
		t.Fatalf("after a new session = %q", got)
	}
	if err := os.Chtimes(old, now, now); err != nil {
		t.Fatal(err)
	}
	if got := names(reader.sessionFiles(claudeDir, now.Add(2*time.Second))); !slices.Equal(got, []string{"new.jsonl"}) {
		t.Fatalf("before the sweep = %q, want only new.jsonl", got)
	}
	if got := names(reader.sessionFiles(claudeDir, now.Add(aiTaskSweep+time.Second))); !slices.Equal(got, []string{"new.jsonl", "old.jsonl"}) {
		t.Fatalf("after the sweep = %q", got)
	}
}

// agent 시작 결과는 "working in the background" 안내를 함께 담는다. API 요청 사본 줄은 같은 표시 문구를 담아도 건너뛴다.
func TestAITranscriptIndexFindsAgentLaunchesAndSkipsRequestCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := []string{
		`{"parentUuid":"p","type":"user","timestamp":"2026-11-17T01:43:04Z","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a6a7","description":"wait"},"message":{"content":[{"tool_use_id":"x","type":"tool_result","content":"Async agent launched successfully. The agent is working in the background."}]}}`,
		`{"type":"api-request-blob","message":{"content":[{"type":"text","text":"copy"}]},"toolUseResult":{"backgroundTaskId":"bcopy"},"note":"\"run_in_background\":true <task-notification><task-id>a6a7</task-id><status>killed</status></task-notification>"}`,
		testNotification("a6a7", "completed", time.Date(2026, 11, 17, 1, 44, 0, 0, time.UTC)),
	}
	testWrite(t, path, strings.Join(lines, "\n")+"\n")
	transcript := &aiTranscript{launches: map[string]string{}, tasks: map[string]*aiBackground{}}
	transcript.index(path)
	if len(transcript.tasks) != 1 {
		t.Fatalf("tasks = %+v", transcript.tasks)
	}
	if task := transcript.tasks["a6a7"]; task == nil || task.kind != "agent" || task.status != "completed" {
		t.Fatalf("agent = %+v", task)
	}
}

// 따라가는 shell의 출력이 한 번에 많이 늘어도 aiTaskOutputTail까지만 읽고 건너뛴 크기를 알린다.
// 줄바꿈 없이 쌓이는 출력은 한 줄로 내보내 pending이 커지지 않는다.
func TestAITaskLogBoundsShellOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "b1.output")
	testWrite(t, output, "start\n")
	log := newAITaskLog(aiTask{Kind: "shell", ID: "b1", Output: output, Transcript: filepath.Join(t.TempDir(), "missing.jsonl")})
	reader := newAITaskReaderWithout()
	if steps, _ := log.read(reader, "", time.Now()); len(steps) != 1 || steps[0].Text != "start" {
		t.Fatalf("first read = %+v", steps)
	}
	file, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(strings.Repeat(strings.Repeat("x", 1023)+"\n", 1024) + "last line\n")
	steps, _ := log.read(reader, "", time.Now())
	if len(steps) == 0 || !strings.HasSuffix(steps[0].Text, "bytes skipped") || steps[len(steps)-1].Text != "last line" || len(steps) > int(aiTaskOutputTail/1024)+2 {
		t.Fatalf("after 1 MiB: %d steps, first %q, last %q", len(steps), steps[0].Text, steps[len(steps)-1].Text)
	}
	_, _ = file.WriteString(strings.Repeat("y", int(aiTaskOutputTail)+1))
	steps, _ = log.read(reader, "", time.Now())
	if int64(len(log.pending)) > aiTaskOutputTail {
		t.Fatalf("pending after a long line without newline = %d bytes", len(log.pending))
	}
	_, _ = file.WriteString(strings.Repeat("y", 10))
	file.Close()
	steps, _ = log.read(reader, "", time.Now())
	if len(steps) != 1 || len(log.pending) != 0 || len([]rune(steps[0].Text)) != aiShellLineLimit {
		t.Fatalf("after the line grew past the limit: %d steps, pending %d, line %d runes", len(steps), len(log.pending), len([]rune(steps[0].Text)))
	}
}

// 마지막 응답 한 줄이 aiActivityTail보다 길어도 그 줄의 stop_reason을 읽어 끝난 세션으로 본다.
func TestAITaskReaderReadsAFinalLineLongerThanTheTail(t *testing.T) {
	claudeDir := t.TempDir()
	now := time.Now()
	long := `{"type":"assistant","timestamp":"2026-11-17T00:39:10Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"` + strings.Repeat("r", int(aiActivityTail)+150<<10) + `"}]}}`
	testSessionFile(t, claudeDir, "s1", []string{testAgentLines[0], long}, now)
	tasks, err := newAITaskReaderWithout().list(claudeDir, now)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %+v, %v; want none", tasks, err)
	}
}

// hook이 끝났다고 기록한 세션은 띄운 shell도 목록에서 뺀다. edc ai sessions도 그 세션에 붙일 작업이 없다.
func TestAITaskReaderSkipsWorkOfEndedSessions(t *testing.T) {
	claudeDir, outputs := t.TempDir(), t.TempDir()
	now := time.Now()
	output := filepath.Join(outputs, "b1.output")
	testWrite(t, output, "")
	session := "44444444-0000-0000-0000-000000000000"
	lines := append([]string{testAgentLines[0]}, testShellLaunch("b1", "u1", "tests", output, now)...)
	testSessionFile(t, claudeDir, session, append(lines, testAgentLines[len(testAgentLines)-1]), now)
	events := filepath.Join(t.TempDir(), "ai-events.jsonl")
	for _, event := range []aiHookEvent{{At: now.Add(-time.Minute), Tool: "claude", Event: "SessionStart", Session: session}, {At: now, Tool: "claude", Event: "SessionEnd", Session: session}} {
		if err := appendAIHookEvent(events, event); err != nil {
			t.Fatal(err)
		}
	}
	reader := newAITaskReaderWithout()
	if tasks, _ := reader.list(claudeDir, now); len(tasks) != 2 {
		t.Fatalf("without hooks tasks = %+v, want the session and its shell", tasks)
	}
	reader = newAITaskReaderWithout()
	reader.hooks = &aiEventTail{path: events}
	tasks, _ := reader.list(claudeDir, now)
	if len(tasks) != 0 {
		t.Fatalf("tasks of an ended session = %+v", tasks)
	}
	sessions := []aiSession{{Tool: "claude", Session: session, State: aiStateEnded}}
	attachAITasks(sessions, tasks)
	if sessions[0].State != aiStateEnded || sessions[0].Tasks != nil {
		t.Fatalf("ended session = %+v", sessions[0])
	}
}
