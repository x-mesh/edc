package edc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEditAIHooksAddsAndRemovesOnlyEdcEntries(t *testing.T) {
	const command = "/opt/edc/bin/edc ai hook claude"
	settings := `{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"~/.claude/hooks/other.sh","timeout":10}]}],` +
		`"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"gk guard"}]}]},"env":{"A":"1"}}`

	installed, changed, err := editAIHooks([]byte(settings), "claude", command, true)
	if err != nil || !reflect.DeepEqual(changed, aiHookEvents["claude"]) {
		t.Fatalf("install changed %v, %v", changed, err)
	}
	var parsed struct {
		Model string
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Command string }
		}
	}
	if err := json.Unmarshal(installed, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Model != "opus" || len(parsed.Hooks["PreToolUse"]) != 1 || parsed.Hooks["Stop"][0].Hooks[0].Command != "~/.claude/hooks/other.sh" {
		t.Fatalf("install changed other settings:\n%s", installed)
	}
	if stop := parsed.Hooks["Stop"]; len(stop) != 2 || stop[1].Hooks[0].Command != command {
		t.Fatalf("Stop must keep the old hook and add edc last:\n%s", installed)
	}
	if strings.Index(string(installed), `"model"`) > strings.Index(string(installed), `"env"`) {
		t.Fatalf("install must keep key order:\n%s", installed)
	}

	_, again, err := editAIHooks(installed, "claude", command, true)
	if err != nil || len(again) != 0 {
		t.Fatalf("a second install must not add again: %v, %v", again, err)
	}

	removed, changed, err := editAIHooks(installed, "claude", command, false)
	if err != nil || !reflect.DeepEqual(changed, aiHookEvents["claude"]) {
		t.Fatalf("uninstall changed %v, %v", changed, err)
	}
	var want, got any
	_ = json.Unmarshal([]byte(settings), &want)
	_ = json.Unmarshal(removed, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("uninstall must restore the settings:\n%s", removed)
	}
}

func TestEditAIHooksRemovesEdcFromASharedGroup(t *testing.T) {
	settings := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"},{"type":"command","command":"'/Applications/My Tools/edc' ai hook codex"}]}]}}`
	removed, changed, err := editAIHooks([]byte(settings), "codex", "edc ai hook codex", false)
	if err != nil || !reflect.DeepEqual(changed, []string{"Stop"}) {
		t.Fatalf("uninstall changed %v, %v", changed, err)
	}
	if strings.Contains(string(removed), "ai hook") || !strings.Contains(string(removed), `"other"`) {
		t.Fatalf("uninstall must drop only the edc hook:\n%s", removed)
	}
}

func TestEditAIHooksCreatesHooksInAnEmptyFile(t *testing.T) {
	installed, changed, err := editAIHooks(nil, "codex", "edc ai hook codex", true)
	if err != nil || len(changed) != len(aiHookEvents["codex"]) {
		t.Fatalf("install changed %v, %v", changed, err)
	}
	removed, _, err := editAIHooks(installed, "codex", "edc ai hook codex", false)
	if err != nil || strings.TrimSpace(string(removed)) != `{
  "hooks": {}
}` {
		t.Fatalf("uninstall left %q, %v", removed, err)
	}
}

func TestParseAIHookInputKeepsOnlyShortDetails(t *testing.T) {
	now := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	long := strings.Repeat("cargo test --workspace ", 20)
	event, ok := parseAIHookInput("claude", []byte(`{"session_id":"s1","hook_event_name":"PermissionRequest","cwd":"/w/edc",`+
		`"tool_name":"Bash","tool_input":{"command":"`+long+`","description":"run tests"}}`), now)
	if !ok || event.Detail != aiHookClip("Bash: "+long, false) || len([]rune(event.Detail)) != aiHookDetailLimit {
		t.Fatalf("permission detail %q", event.Detail)
	}

	event, _ = parseAIHookInput("claude", []byte(`{"session_id":"s1","hook_event_name":"Stop",`+
		`"last_assistant_message":"I changed two files.\n\nShall I run the full suite?"}`), now)
	if event.Kind != "question" || !strings.HasSuffix(event.Detail, "run the full suite?") {
		t.Fatalf("stop event %+v", event)
	}

	event, _ = parseAIHookInput("claude", []byte(`{"session_id":"s1","hook_event_name":"PermissionRequest","tool_name":"AskUserQuestion",`+
		`"tool_input":{"questions":[{"question":"Which target version?","options":[]}]}}`), now)
	if event.Kind != "ask" || event.Detail != "Which target version?" {
		t.Fatalf("ask event %+v", event)
	}
	if folded := foldAIHookEvents([]aiHookEvent{event}); folded["claude/s1"].Reason != "ask" {
		t.Fatalf("ask session %+v", folded["claude/s1"])
	}

	if _, ok := parseAIHookInput("claude", []byte(`{"hook_event_name":"Stop"}`), now); ok {
		t.Fatal("an event without a session must be dropped")
	}
}

func TestFoldAIHookEventsFollowsTheLatestEvent(t *testing.T) {
	start := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	events := []aiHookEvent{
		{At: at(0), Tool: "claude", Event: "SessionStart", Session: "a", PID: 10, TTY: "ttys001", Transcript: "/t/a.jsonl"},
		{At: at(1), Tool: "claude", Event: "UserPromptSubmit", Session: "a"},
		{At: at(5), Tool: "claude", Event: "PermissionRequest", Session: "a", Detail: "Bash: rm -rf target"},
		{At: at(6), Tool: "claude", Event: "Notification", Session: "a", Kind: "permission_prompt", Detail: "Claude needs your permission"},
		{At: at(0), Tool: "codex", Event: "UserPromptSubmit", Session: "b"},
		{At: at(9), Tool: "codex", Event: "Stop", Session: "b", Kind: "question", Detail: "merge now?"},
		{At: at(0), Tool: "claude", Event: "UserPromptSubmit", Session: "c", PID: 0},
		{At: at(2), Tool: "claude", Event: "StopFailure", Session: "c", Detail: "overloaded"},
		{At: at(0), Tool: "claude", Event: "SessionStart", Session: "d", PID: 20},
		{At: at(3), Tool: "claude", Event: "SessionEnd", Session: "d"},
	}
	folded := foldAIHookEvents(events)
	if a := folded["claude/a"]; a.State != aiStateNeeds || a.Detail != "Bash: rm -rf target" || a.TTY != "ttys001" || !a.Since.Equal(at(5)) {
		t.Fatalf("session a %+v", a)
	}
	if b := folded["codex/b"]; b.State != aiStateNeeds || b.Reason != "question" {
		t.Fatalf("session b %+v", b)
	}
	if c := folded["claude/c"]; c.State != aiStateFailed {
		t.Fatalf("session c %+v", c)
	}

	alive := func(int) bool { return true }
	unchanged := func(string) (time.Time, bool) { return at(5), true }
	sessions := selectAISessions(folded, at(60), false, false, alive, unchanged)
	var order []string
	for _, session := range sessions {
		order = append(order, session.Session)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c"}) {
		t.Fatalf("order %v must put needs first, oldest first, and drop ended", order)
	}
}

func TestSettleAISessionSeesApprovalAndExit(t *testing.T) {
	since := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	session := aiSession{State: aiStateNeeds, Reason: "permission", Since: since, Last: since, PID: 10, Transcript: "/t/a.jsonl"}
	written := func(string) (time.Time, bool) { return since.Add(30 * time.Second), true }
	settleAISession(&session, since.Add(time.Minute), func(int) bool { return true }, written)
	if session.State != aiStateWorking {
		t.Fatalf("a transcript written after the request means it was answered: %+v", session)
	}

	session = aiSession{State: aiStateNeeds, Reason: "question", Since: since, Last: since, PID: 10, Transcript: "/t/a.jsonl"}
	settleAISession(&session, since.Add(time.Minute), func(int) bool { return true }, written)
	if session.State != aiStateNeeds {
		t.Fatalf("a question stays open until the next prompt: %+v", session)
	}

	settleAISession(&session, since.Add(time.Minute), func(int) bool { return false }, written)
	if session.State != aiStateEnded {
		t.Fatalf("a gone process ends the session: %+v", session)
	}
}

func TestAppendAndReadAIHookEventsRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), aiEventsName)
	first := aiHookEvent{At: time.Unix(1, 0).UTC(), Tool: "claude", Event: "SessionStart", Session: "a"}
	if err := appendAIHookEvent(path, first); err != nil {
		t.Fatal(err)
	}
	appendAIFixture(t, path, strings.Repeat("\n", aiEventsMax))
	second := aiHookEvent{At: time.Unix(2, 0).UTC(), Tool: "claude", Event: "Stop", Session: "a"}
	if err := appendAIHookEvent(path, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("a full file must move to .1: %v", err)
	}
	events, err := readAIHookEvents(path)
	if err != nil || len(events) != 2 || events[0].Event != "SessionStart" || events[1].Event != "Stop" {
		t.Fatalf("events %+v, %v", events, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("events file mode %v", info.Mode().Perm())
	}
}

func TestAIEventTailReadsOnlyNewLinesAndFollowsRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), aiEventsName)
	event := func(name string) aiHookEvent {
		return aiHookEvent{At: time.Unix(1, 0).UTC(), Tool: "claude", Event: name, Session: "a"}
	}
	tail := &aiEventTail{path: path}
	if err := tail.read(); err != nil || len(tail.events) != 0 {
		t.Fatalf("a missing file is empty: %v, %v", tail.events, err)
	}
	if err := appendAIHookEvent(path, event("SessionStart")); err != nil {
		t.Fatal(err)
	}
	appendAIFixture(t, path, `{"at":"1970-01-01T00:00:02Z","tool":"claude","event":"Stop","session":"a"`)
	if err := tail.read(); err != nil || len(tail.events) != 1 {
		t.Fatalf("a line without its newline must wait: %+v, %v", tail.events, err)
	}
	appendAIFixture(t, path, "}\n")
	if err := tail.read(); err != nil || len(tail.events) != 2 || tail.events[1].Event != "Stop" {
		t.Fatalf("the finished line must arrive once: %+v, %v", tail.events, err)
	}

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := appendAIHookEvent(path, event("UserPromptSubmit")); err != nil {
		t.Fatal(err)
	}
	if err := tail.read(); err != nil || len(tail.events) != 3 || tail.events[2].Event != "UserPromptSubmit" {
		t.Fatalf("after rotation both files must be read once: %+v, %v", tail.events, err)
	}
}

func TestAISessionsModelAlertsOnlyOnNewRequests(t *testing.T) {
	start := time.Now().Add(-time.Minute).UTC()
	request := aiHookEvent{At: start, Tool: "codex", Event: "Stop", Session: "b", Kind: "question", Detail: "merge?"}
	model := aiSessionsModel{tail: &aiEventTail{}, alerted: map[string]time.Time{}, now: time.Now()}
	model.events = []aiHookEvent{request}
	if model.refresh() {
		t.Fatal("requests that exist when the screen opens must not ring")
	}
	model.loaded = true
	if model.refresh() {
		t.Fatal("the same request must not ring again")
	}
	later := request
	later.At = start.Add(30 * time.Second)
	model.events = append(model.events, later)
	if !model.refresh() {
		t.Fatal("a new request must ring")
	}
	if model.selected != "codex/b" {
		t.Fatalf("the first session must be selected: %q", model.selected)
	}
}
