package edc

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type stubModel struct {
	messages []string
	quitOn   string
}

type stubMsg struct{ name string }

func (model stubModel) Init() tea.Cmd { return nil }

func (model stubModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	value, ok := msg.(stubMsg)
	if !ok {
		return model, nil
	}
	model.messages = append(model.messages, value.name)
	if value.name == model.quitOn {
		return model, tea.Quit
	}
	return model, nil
}

func (model stubModel) View() tea.View { return tea.NewView(strings.Join(model.messages, ",") + "\n") }

func headlessOptions() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithoutRenderer(), tea.WithInput(nil), tea.WithoutSignalHandler()}
}

func TestStartLiveProgramSendsAndFinishes(t *testing.T) {
	exited := make(chan struct{})
	live, err := startLiveProgram(stubModel{quitOn: "done"}, func() { close(exited) }, headlessOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	live.send(stubMsg{name: "first"})
	live.send(stubMsg{name: "second"})
	model, err := live.finish(stubMsg{name: "done"})
	if err != nil {
		t.Fatal(err)
	}
	final, ok := model.(stubModel)
	if !ok {
		t.Fatalf("model = %#v", model)
	}
	if strings.Join(final.messages, ",") != "first,second,done" {
		t.Fatalf("messages = %#v", final.messages)
	}
	<-exited
	// finish 뒤의 send는 event loop가 없으므로 block하지 않아야 한다.
	live.send(stubMsg{name: "late"})
}

func TestStartLiveProgramReportsStartFailure(t *testing.T) {
	if _, err := startLiveProgram(nil, nil, headlessOptions()...); err == nil {
		t.Fatal("nil model must fail to start")
	} else if !strings.Contains(err.Error(), "InitialModel") {
		t.Fatalf("error = %v", err)
	}
}

func TestNilLiveProgramIsNoop(t *testing.T) {
	var live *liveProgram
	live.send(stubMsg{name: "ignored"})
	if model, err := live.finish(stubMsg{name: "ignored"}); model != nil || err != nil {
		t.Fatalf("nil program returned %#v, %v", model, err)
	}
}

func TestLiveTerminalRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if liveTerminal() {
		t.Fatal("NO_COLOR must disable the live screen")
	}
}

func TestLiveCellKeepsColorAndWidth(t *testing.T) {
	colored := terminalStatus(StatusPass, true)
	cell := liveCell(colored, 10)
	if !strings.Contains(cell, "\033[32m") {
		t.Fatalf("padding dropped the color: %q", cell)
	}
	if liveWidth(cell) != 10 {
		t.Fatalf("cell width = %d, want 10", liveWidth(cell))
	}
	if liveWidth(colored) != 4 {
		t.Fatalf("colored status width = %d, want 4", liveWidth(colored))
	}
}
