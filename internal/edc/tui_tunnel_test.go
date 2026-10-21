package edc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func tunnelKey(model tea.Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) { return model.Update(key) }

func typeTunnelText(model tea.Model, text string) tea.Model {
	for _, character := range text {
		model, _ = tunnelKey(model, tea.KeyPressMsg{Code: character, Text: string(character)})
	}
	return model
}

func newTestTunnelScreen(t *testing.T) (tunnelScreen, *fakeTunnelRunner) {
	t.Helper()
	manager, runner := newTestTunnelManager(t)
	environment := tunnelEnvironment{manager: manager, specs: map[string]tunnelSpec{
		"db": {Host: "jump1", Local: "5432", Remote: "db.internal:5432"},
	}, file: filepath.Join(t.TempDir(), "tunnels.yaml")}
	screen := newTunnelScreen(environment, 5*1e9, false)
	rows := manager.rows(context.Background(), environment.specs)
	updated, _ := screen.Update(tunnelRowsMsg(rows))
	return updated.(tunnelScreen), runner
}

func runTunnelCmd(t *testing.T, model tea.Model, command tea.Cmd) tea.Model {
	t.Helper()
	if command == nil {
		t.Fatal("want a command")
	}
	updated, _ := model.Update(command())
	return updated
}

func TestTunnelScreenEnterStartsThenStopsTheSelectedTunnel(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	var model tea.Model = screen
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	if len(runner.started) != 1 || runner.started[0].Port != 5432 {
		t.Fatalf("started = %+v", runner.started)
	}
	if notice := model.(tunnelScreen).notice; !strings.Contains(notice, "5432") {
		t.Errorf("notice = %q", notice)
	}
	rows := model.(tunnelScreen).environment.manager.rows(context.Background(), model.(tunnelScreen).environment.specs)
	model, _ = model.Update(tunnelRowsMsg(rows))
	if state := model.(tunnelScreen).rows[0].State; state != tunnelStateUp {
		t.Fatalf("state = %s", state)
	}
	model, command = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	runTunnelCmd(t, model, command)
	if len(runner.stopped) != 1 {
		t.Fatalf("stopped = %v", runner.stopped)
	}
}

func TestTunnelScreenFormStartsOnceWhenSaveIsNo(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	if model.(tunnelScreen).mode != tunnelModeForm {
		t.Fatal("n must open the form")
	}
	form := model.(tunnelScreen).form
	form.host = ""
	screen = model.(tunnelScreen)
	screen.form = form
	model = screen
	model = typeTunnelText(model, "jump1")
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyTab})
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyTab})
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyBackspace})
	for range 4 {
		model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	model = typeTunnelText(model, "6000")
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyTab})
	model = typeTunnelText(model, "cache:6379")
	screen = model.(tunnelScreen)
	screen.form.save = false
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	if len(runner.started) != 1 || runner.started[0].Port != 6000 || runner.started[0].Destination != "cache:6379" {
		t.Fatalf("started = %+v", runner.started)
	}
	if _, err := os.Stat(model.(tunnelScreen).environment.file); err == nil {
		t.Error("save no must not write the definition file")
	}
}

func TestTunnelScreenEnterSavesTheDefinitionByDefaultAndStarts(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	form := model.(tunnelScreen).form
	form.host, form.kind, form.port, form.name = "jump1", tunnelSocks, "1080", "proxy"
	screen = model.(tunnelScreen)
	screen.form = form
	if !screen.form.save {
		t.Fatal("save must default to yes")
	}
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	if len(runner.started) != 1 {
		t.Fatalf("started = %+v", runner.started)
	}
	final := model.(tunnelScreen)
	if _, ok := final.environment.specs["proxy"]; !ok {
		t.Fatalf("specs = %+v", final.environment.specs)
	}
	data, err := os.ReadFile(final.environment.file)
	if err != nil || !strings.Contains(string(data), "socks: 1080") {
		t.Fatalf("file = %q, err %v", data, err)
	}
}

func TestTunnelScreenFormShowsAnInvalidInputWithoutLeaving(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil || model.(tunnelScreen).mode != tunnelModeForm || model.(tunnelScreen).form.problem == "" {
		t.Fatalf("an empty destination must keep the form open with a message: %+v", model.(tunnelScreen).form)
	}
	if len(runner.started) != 0 {
		t.Fatal("nothing must start")
	}
}

func TestTunnelScreenRemovesADefinitionAfterConfirmation(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	if err := saveTunnelDefinition(screen.environment.file, "db", screen.environment.specs["db"]); err != nil {
		t.Fatal(err)
	}
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if model.(tunnelScreen).mode != tunnelModeConfirm {
		t.Fatal("x must ask first")
	}
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = runTunnelCmd(t, model, command)
	if specs := model.(tunnelScreen).environment.specs; len(specs) != 0 {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestTunnelScreenFormHintsTheEmptyDestination(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	if view := ansi.Strip(screen.View().Content); !strings.Contains(view, "db.internal:5432") {
		t.Errorf("local form misses the destination hint:\n%s", view)
	}
	screen.form.kind = tunnelReverse
	if view := ansi.Strip(screen.View().Content); !strings.Contains(view, "localhost:3000") {
		t.Errorf("reverse form misses the local target hint:\n%s", view)
	}
}

func TestTunnelScreenFormExplainsTheActiveField(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.host, screen.form.destination = "jump1", "db.internal:5432"
	cases := []struct {
		field int
		kind  tunnelKind
		want  []string
	}{
		{tunnelFieldHost, tunnelLocal, []string{"~/.ssh/config", "127.0.0.1:auto ──ssh──▶ jump1 ──▶ db.internal:5432"}},
		{tunnelFieldKind, tunnelLocal, []string{"ssh -L", "ssh -R", "ssh -D"}},
		{tunnelFieldPort, tunnelReverse, []string{"GatewayPorts", "jump1 :auto ──ssh──▶ this machine"}},
		{tunnelFieldPort, tunnelSocks, []string{"socks5://127.0.0.1", "any destination"}},
		{tunnelFieldDestination, tunnelLocal, []string{"localhost:5432"}},
		{tunnelFieldName, tunnelLocal, []string{"32 letters"}},
	}
	for _, test := range cases {
		screen.form.field, screen.form.kind = test.field, test.kind
		view := ansi.Strip(screen.View().Content)
		for _, want := range test.want {
			if !strings.Contains(view, want) {
				t.Errorf("field %d kind %s misses %q:\n%s", test.field, test.kind, want, view)
			}
		}
	}
}

func TestTunnelScreenKindChangeKeepsThePortValid(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.field = tunnelFieldKind
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if form := model.(tunnelScreen).form; form.kind != tunnelReverse || form.port != "" {
		t.Fatalf("reverse must clear auto: %+v", form)
	}
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if form := model.(tunnelScreen).form; form.kind != tunnelSocks || form.port != tunnelAutoPort {
		t.Fatalf("socks must restore auto: %+v", form)
	}
}

func TestTunnelScreenViewsFitAndShowKeyHints(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	list := ansi.Strip(screen.View().Content)
	for _, want := range []string{"edc tunnel", "enter", "db", "127.0.0.1:5432", "down"} {
		if !strings.Contains(list, want) {
			t.Errorf("list view misses %q:\n%s", want, list)
		}
	}
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.destination = "db.internal:5432"
	form := ansi.Strip(screen.View().Content)
	for _, want := range []string{"new tunnel", "host", "local port", "ssh -N -L"} {
		if !strings.Contains(form, want) {
			t.Errorf("form view misses %q:\n%s", want, form)
		}
	}
}

func TestTunnelScreenCtrlTRunsTheCheckAndMarksOldResultsStale(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if command != nil || model.(tunnelScreen).form.problem == "" {
		t.Fatal("an incomplete form must not start a check")
	}
	form := model.(tunnelScreen).form
	form.host, form.destination = "jump1", "db.internal:5432"
	screen = model.(tunnelScreen)
	screen.form = form
	model, command = tunnelKey(screen, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if command == nil || !model.(tunnelScreen).form.checking {
		t.Fatal("ctrl+t must start the check")
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, T("tunnel.check.running")) {
		t.Errorf("view misses the progress line:\n%s", view)
	}
	model = runTunnelCmd(t, model, command)
	current := model.(tunnelScreen)
	if current.form.checking || len(current.form.checks) != 3 || len(runner.started) != 0 {
		t.Fatalf("form %+v, started %v", current.form, runner.started)
	}
	view := ansi.Strip(model.View().Content)
	for _, want := range []string{"✔ SSH login", "destination db.internal:5432", T("tunnel.check.summary_ok")} {
		if !strings.Contains(view, want) {
			t.Errorf("view misses %q:\n%s", want, view)
		}
	}
	current.form.field = tunnelFieldDestination
	var edited tea.Model = current
	edited = typeTunnelText(edited, "0")
	if view := ansi.Strip(edited.View().Content); !strings.Contains(view, T("tunnel.check.stale")) {
		t.Errorf("an edited form must mark the result stale:\n%s", view)
	}
}

func TestTunnelScreenSaveChoiceTogglesAndShowsWhereItSaves(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.field = tunnelFieldSave
	view := ansi.Strip(screen.View().Content)
	for _, want := range []string{"[yes] no", "saved to", "tunnels.yaml"} {
		if !strings.Contains(view, want) {
			t.Errorf("save view misses %q:\n%s", want, view)
		}
	}
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	view = ansi.Strip(model.View().Content)
	if !strings.Contains(view, "yes [no]") || !strings.Contains(view, T("tunnel.form.save_none")) {
		t.Errorf("save no view:\n%s", view)
	}
}

func TestTunnelScreenBlocksSavingOverAnExistingName(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.host, screen.form.destination, screen.form.name = "jump1", "db:5432", "db"
	view := ansi.Strip(screen.View().Content)
	if !strings.Contains(view, "already exists") {
		t.Errorf("the form must warn before submit:\n%s", view)
	}
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil || model.(tunnelScreen).form.problem == "" || len(runner.started) != 0 {
		t.Fatal("saving over an existing name must be blocked")
	}
	screen.form.save = false
	if _, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter}); command == nil {
		t.Error("a one-time start does not write the file, so the name may repeat")
	}
}

func TestTunnelScreenListShowsSavedMarkAndHintsForTheSelectedRow(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	screen.rows = append(screen.rows, tunnelRow{Name: "adhoc", Kind: tunnelLocal, Port: 6000, State: tunnelStateUp, Reach: tunnelReachOK})
	view := ansi.Strip(screen.View().Content)
	if !strings.Contains(view, "SAVED") && !strings.Contains(view, "saved") {
		t.Errorf("list misses the saved column:\n%s", view)
	}
	if !strings.Contains(view, "●") || !strings.Contains(view, "–") {
		t.Errorf("list must mark saved and unsaved rows:\n%s", view)
	}
	first := strings.SplitN(view, "\n", 2)[0]
	if !strings.Contains(first, "x remove definition") || !strings.Contains(first, "r restart") {
		t.Errorf("a saved row offers restart and remove: %q", first)
	}
	screen.cursor = 1
	first = strings.SplitN(ansi.Strip(screen.View().Content), "\n", 2)[0]
	if strings.Contains(first, "remove definition") || strings.Contains(first, "restart") || !strings.Contains(first, "enter") {
		t.Errorf("a one-time row offers only start/stop, new, and quit: %q", first)
	}
}
