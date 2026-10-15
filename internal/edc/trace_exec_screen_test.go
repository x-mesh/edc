//go:build linux

package edc

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func pressExecScreenKeys(model execTraceScreenModel, keys ...tea.KeyPressMsg) (execTraceScreenModel, tea.Cmd) {
	var cmd tea.Cmd
	for _, key := range keys {
		var next tea.Model
		next, cmd = model.Update(key)
		model = next.(execTraceScreenModel)
	}
	return model, cmd
}

func execScreenText(text string) []tea.KeyPressMsg {
	keys := make([]tea.KeyPressMsg, 0, len(text))
	for _, value := range text {
		keys = append(keys, tea.KeyPressMsg{Code: value, Text: string(value)})
	}
	return keys
}

func TestExecTraceScreenSwitchesViews(t *testing.T) {
	model := newExecTraceScreenModel(nil)
	next, _ := model.Update(execTraceEventMsg{events: []execEvent{{Event: execEventExec, PID: 10, Process: "make", Parent: "bash", Args: "make all"}}})
	model = next.(execTraceScreenModel)
	for _, tc := range []struct {
		key    tea.KeyPressMsg
		header string
	}{
		{tea.KeyPressMsg{Code: 'p', Text: "p"}, "PROCESS"},
		{tea.KeyPressMsg{Code: 'a', Text: "a"}, "PARENT"},
		{tea.KeyPressMsg{Code: 'e', Text: "e"}, "EVENT"},
		{tea.KeyPressMsg{Code: 'c', Text: "c"}, "CGROUP"},
		{tea.KeyPressMsg{Code: 'g', Text: "g"}, "TIME"},
	} {
		model, _ = pressExecScreenKeys(model, tc.key)
		if content := model.View().Content; !strings.Contains(content, tc.header) {
			t.Fatalf("%s: view has no %s header:\n%s", tc.key.String(), tc.header, content)
		}
	}
	var order []string
	for range 5 {
		model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: tea.KeyTab})
		order = append(order, model.groupBy)
	}
	if got := strings.Join(order, ","); got != "process,parent,event,cgroup," {
		t.Fatalf("tab order = %q", got)
	}
}

func TestExecTraceScreenFilterSelectsAndOpensTheMatchingEvent(t *testing.T) {
	code := int32(1)
	model := newExecTraceScreenModel(nil)
	next, _ := model.Update(execTraceEventMsg{events: []execEvent{
		{Event: execEventExec, PID: 10, Process: "make", Parent: "bash", Args: "make all"},
		{Event: execEventExec, PID: 11, Process: "cc", Parent: "make", Args: "cc -c main.c"},
		{Event: execEventExit, PID: 11, Process: "cc", Parent: "make", ExitCode: &code},
	}})
	model = next.(execTraceScreenModel)
	model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: '/', Text: "/"})
	model, _ = pressExecScreenKeys(model, execScreenText("main.c")...)
	model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if content := model.View().Content; model.filter != "main.c" || !strings.Contains(content, "cc -c main.c") || strings.Contains(content, "make all") {
		t.Fatalf("filter %q view:\n%s", model.filter, content)
	}
	// 필터를 건 목록의 첫 줄을 고르고 연다. 상세는 화면에서 고른 줄이어야 한다.
	model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if content := model.View().Content; !model.detail || !strings.Contains(content, "args cc -c main.c") {
		t.Fatalf("detail view:\n%s", content)
	}
	model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if model.detail {
		t.Fatal("esc did not close the detail")
	}
	model, _ = pressExecScreenKeys(model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if model.filter != "" || model.selected != -1 || !strings.Contains(model.View().Content, "make all") {
		t.Fatalf("esc kept filter %q selected %d", model.filter, model.selected)
	}
}

func TestExecTraceScreenQuitStopsTheCapture(t *testing.T) {
	stopped := 0
	model, cmd := pressExecScreenKeys(newExecTraceScreenModel(func() { stopped++ }), tea.KeyPressMsg{Code: 'q', Text: "q"})
	if stopped != 1 || !model.stopping || cmd == nil {
		t.Fatalf("stopped %d stopping %v cmd %v", stopped, model.stopping, cmd != nil)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("q did not quit")
	}
	if content := model.View().Content; !strings.Contains(content, "stopping") {
		t.Fatalf("view after q:\n%s", content)
	}
}

func TestExecTraceScreenColorKeepsThePlainTextAndWidth(t *testing.T) {
	code := int32(1)
	model := newExecTraceScreenModel(nil)
	model.width = 60
	next, _ := model.Update(execTraceEventMsg{events: []execEvent{
		{Event: execEventExec, PID: 10, Process: "make", Parent: "bash", Args: "make all --jobs=8 --keep-going --output-sync=target"},
		{Event: execEventExit, PID: 11, Process: "cc", ExitCode: &code},
		{Event: execEventOOM, PID: 12, Process: "java"},
	}})
	model = next.(execTraceScreenModel)
	model.selected = 0
	t.Setenv("NO_COLOR", "1")
	plain := strings.Split(model.View().Content, "\n")
	t.Setenv("NO_COLOR", "")
	colored := strings.Split(model.View().Content, "\n")
	for index := range plain {
		if want := traceFit(plain[index], model.width); strings.TrimRight(ansi.Strip(colored[index]), " ") != strings.TrimRight(want, " ") {
			t.Fatalf("line %d:\nplain   %q\ncolored %q", index, want, ansi.Strip(colored[index]))
		}
		if liveWidth(colored[index]) > model.width {
			t.Fatalf("line %d is %d columns: %q", index, liveWidth(colored[index]), colored[index])
		}
	}
	if !strings.HasPrefix(colored[0], tracePaint("edc trace exec", richMagenta, true)) || !strings.Contains(colored[3], traceTestSGR(richChipBackground, true)) {
		t.Fatalf("title or selected row:\n%q\n%q", colored[0], colored[3])
	}
	if !strings.Contains(colored[4], tracePaint("exit  ", richYellow, false)) || !strings.Contains(colored[5], tracePaint("oom   ", richRed, false)) {
		t.Fatalf("exit and oom rows:\n%q\n%q", colored[4], colored[5])
	}
}
