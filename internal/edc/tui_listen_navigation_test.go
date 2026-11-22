package edc

import (
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestListenBrowserTopNavigationPreservesSelectionAndDetail(t *testing.T) {
	model := listenBrowserFixture(t)
	model, _ = listenBrowserKey(model, "down")
	model, load := listenBrowserKey(model, "enter")
	next, _ := model.Update(load())
	model = next.(listenBrowser)
	model, _ = listenBrowserKey(model, "down")
	before := model.detail.offset
	var launched string
	model.openTop = func(pid string) tea.Cmd { launched = pid; return func() tea.Msg { return listenBrowserTopDoneMsg{} } }
	model, cmd := listenBrowserKey(model, "t")
	if launched != "200" || cmd == nil {
		t.Fatalf("wrong process: %s", launched)
	}
	next, _ = model.Update(cmd())
	model = next.(listenBrowser)
	socket, _ := model.selected()
	if socket.PID != "200" || model.detail == nil || model.detail.offset != before {
		t.Fatal("child view lost parent state")
	}
	next, _ = model.Update(listenBrowserTopDoneMsg{err: errors.New("child exited")})
	if !strings.Contains(next.(listenBrowser).notice, "child exited") {
		t.Fatal("child error not surfaced")
	}
}

func TestListenBrowserNavigationRejectsUnknownOwnerAndRedaction(t *testing.T) {
	for _, pid := range []string{"", "0", "-1", "1; echo test", "4294967295"} {
		if _, err := listenBrowserTopCommand("/path/to/edc", pid); err == nil {
			t.Fatalf("accepted %q", pid)
		}
	}
	command, err := listenBrowserTopCommand("/path with spaces/edc", "921")
	if err != nil || !slices.Equal(command.Args, []string{"/path with spaces/edc", "top", "--interactive", "--count", "0", "--json=", "--process", "921"}) {
		t.Fatalf("command: %v %v", command, err)
	}
	model := listenBrowserFixture(t)
	model.redact = true
	model.openTop = func(string) tea.Cmd { t.Fatal("redaction bypassed"); return nil }
	for _, key := range []string{"t", "x"} {
		updated, cmd := listenBrowserKey(model, key)
		if cmd != nil || updated.notice == "" {
			t.Fatal("redacted navigation must stop with an explanation")
		}
	}
	model.redact = false
	model.items[0].PID = ""
	updated, cmd := listenBrowserKey(model, "t")
	if cmd != nil || updated.notice == "" {
		t.Fatal("unknown owner launched a child")
	}
}

func TestListenBrowserTraceIsAnExplicitCommandHint(t *testing.T) {
	model := listenBrowserFixture(t)
	model, cmd := listenBrowserKey(model, "x")
	if cmd != nil || model.detail == nil {
		t.Fatal("trace hint must not execute anything")
	}
	text := strings.Join(model.detail.lines, "\n")
	if !strings.Contains(text, "edc trace cpu --process 100 --duration 15s") {
		t.Fatalf("hint: %s", text)
	}
	model, _ = listenBrowserKey(model, "esc")
	if model.detail != nil || model.cursor != 0 {
		t.Fatal("trace hint did not return to list")
	}
}

func TestTopInteractiveRejectsNonTerminalAndBatchModes(t *testing.T) {
	for _, args := range [][]string{{"--interactive", "--count", "1"}, {"--interactive", "--json", "-"}} {
		if code := runTop(args, "test"); code != 2 {
			t.Fatalf("%v exit %d", args, code)
		}
	}
}
