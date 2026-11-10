package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTunnelScreenDoesNotSaveWhenTheStartFails(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	runner.failure = errors.New("Permission denied (publickey)")
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	form := model.(tunnelScreen).form
	form.host, form.kind, form.port, form.name = "jump1", tunnelSocks, "1080", "proxy"
	screen = model.(tunnelScreen)
	screen.form = form
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	final := model.(tunnelScreen)
	if !final.failed || !strings.Contains(final.notice, "Permission denied") {
		t.Fatalf("notice = %q, failed %v", final.notice, final.failed)
	}
	if _, err := os.Stat(final.environment.file); err == nil {
		t.Error("a failed start must not leave a saved definition")
	}
	if _, saved := final.environment.specs["proxy"]; saved {
		t.Errorf("specs = %+v", final.environment.specs)
	}
}

func TestTunnelScreenReportsASaveFailureAfterAStartedTunnel(t *testing.T) {
	screen, runner := newTestTunnelScreen(t)
	dir := screen.environment.file
	if err := os.MkdirAll(dir, 0o755); err != nil { // 파일 자리에 디렉터리를 두어 저장이 실패하게 한다
		t.Fatal(err)
	}
	var model tea.Model = screen
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'n', Text: "n"})
	form := model.(tunnelScreen).form
	form.host, form.kind, form.port, form.name = "jump1", tunnelSocks, "1080", "proxy"
	screen = model.(tunnelScreen)
	screen.form = form
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	final := model.(tunnelScreen)
	if len(runner.started) != 1 || !final.failed || !strings.Contains(final.notice, "proxy") {
		t.Fatalf("started %d, failed %v, notice %q", len(runner.started), final.failed, final.notice)
	}
	if records := final.environment.manager.store.load(); len(records) != 1 {
		t.Fatalf("the running tunnel must stay recorded: %+v", records)
	}
}

func TestTunnelHostChoicesReadSSHConfigAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host *\n  ServerAliveInterval 30\nHost bastion jump1\n  HostName 10.0.0.9\nhost  web-*  !skip  db\nMatch all\n  User x\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tunnelHostChoices(map[string]string{"jump1": "ops@10.0.0.5", "alpha": "a@h"}), ",")
	if got != "alpha,jump1,bastion,db" {
		t.Errorf("choices = %s", got)
	}
}

func openTunnelForm(t *testing.T, hosts ...string) (tunnelScreen, *fakeTunnelRunner) {
	t.Helper()
	screen, runner := newTestTunnelScreen(t)
	screen.mode = tunnelModeForm
	screen.form = screen.newForm()
	screen.form.hosts, screen.form.host = hosts, hosts[0]
	return screen, runner
}

func TestTunnelFormStartsFromOneLineTypedInTheHostField(t *testing.T) {
	screen, runner := openTunnelForm(t, "alpha", "jump1")
	screen.form.save = false
	model := typeTunnelText(screen, "jump1 6000:cache:6379")
	view := ansi.Strip(model.View().Content)
	for _, want := range []string{"jump1 6000:cache:6379", "ssh -N -L 127.0.0.1:6000:cache:6379 ops@10.0.0.5", "127.0.0.1:6000 ──ssh──▶ jump1 ──▶ cache:6379"} {
		if !strings.Contains(view, want) {
			t.Errorf("the form must show what the line means, misses %q:\n%s", want, view)
		}
	}
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	runTunnelCmd(t, model, command)
	if len(runner.started) != 1 || runner.started[0].Host != "jump1" || runner.started[0].Port != 6000 || runner.started[0].Destination != "cache:6379" {
		t.Fatalf("started = %+v", runner.started)
	}
}

func TestTunnelFormTabMovesTheLineIntoTheFields(t *testing.T) {
	screen, _ := openTunnelForm(t, "alpha")
	model := typeTunnelText(screen, "ssh -R 8080:localhost:3000 jump1")
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyTab})
	form := model.(tunnelScreen).form
	if form.host != "jump1" || form.kind != tunnelReverse || form.port != "8080" || form.destination != "localhost:3000" || form.field != tunnelFieldKind {
		t.Fatalf("form = %+v", form)
	}
}

func TestTunnelFormTakesAPastedSSHCommand(t *testing.T) {
	screen, runner := openTunnelForm(t, "alpha")
	screen.form.save = false
	var model tea.Model = screen
	model, _ = model.Update(tea.PasteMsg{Content: "ssh -N -L 5432:db.internal:5432 jump1\n"})
	if host := model.(tunnelScreen).form.host; host != "ssh -N -L 5432:db.internal:5432 jump1" {
		t.Fatalf("a paste must replace the prefilled host and drop the line break: %q", host)
	}
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	runTunnelCmd(t, model, command)
	if len(runner.started) != 1 || runner.started[0].Port != 5432 || runner.started[0].Destination != "db.internal:5432" {
		t.Fatalf("started = %+v", runner.started)
	}
}

func TestTunnelFormKeepsALineItCannotCarry(t *testing.T) {
	screen, runner := openTunnelForm(t, "alpha")
	model := typeTunnelText(screen, "ssh -p 2222 -L 5432:db:5432 jump1")
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "-p") || !strings.Contains(view, "~/.ssh/config") {
		t.Errorf("the form must say why before submit:\n%s", view)
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyTab}, {Code: tea.KeyEnter}, {Code: 't', Mod: tea.ModCtrl}} {
		next, command := tunnelKey(model, key)
		form := next.(tunnelScreen).form
		if command != nil || form.field != tunnelFieldHost || form.problem == "" || form.host != "ssh -p 2222 -L 5432:db:5432 jump1" {
			t.Fatalf("%s must stay on the line with a message: %+v", key.String(), form)
		}
	}
	if len(runner.started) != 0 {
		t.Fatal("nothing must start")
	}
}

func TestTunnelFormFiltersHostsAsYouType(t *testing.T) {
	hosts := []string{"alpha"}
	for index := range 60 {
		hosts = append(hosts, "web-"+string(rune('a'+index/26))+string(rune('a'+index%26)))
	}
	hosts = append(hosts, "db-prod", "db-stage")
	screen, _ := openTunnelForm(t, hosts...)
	if view := ansi.Strip(screen.View().Content); !strings.Contains(view, "[alpha]") || !strings.Contains(view, "+55") {
		t.Errorf("the form must show the candidates and how many more there are:\n%s", view)
	}
	model := typeTunnelText(screen, "DB")
	form := model.(tunnelScreen).form
	if form.host != "DB" || strings.Join(form.hostMatches(), ",") != "db-prod,db-stage" {
		t.Fatalf("typing must replace the prefilled host and filter every candidate: host %q, matches %v", form.host, form.hostMatches())
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "db-prod  db-stage") || strings.Contains(view, "web-") {
		t.Errorf("only the matches must show:\n%s", view)
	}
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if host := model.(tunnelScreen).form.host; host != "db-prod" {
		t.Fatalf("right must pick the first match: %q", host)
	}
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if host := model.(tunnelScreen).form.host; host != "db-prod" {
		t.Fatalf("the choice must cycle inside the matches: %q", host)
	}
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "[db-prod]  db-stage") {
		t.Errorf("the picked match must be marked:\n%s", view)
	}
	model = typeTunnelText(model, " db.internal:5432")
	if _, spec := model.(tunnelScreen).form.spec(); spec.Host != "db-prod" || spec.Remote != "db.internal:5432" {
		t.Fatalf("a picked host must take the rest of the line: %+v", spec)
	}
}

func TestTunnelFormLeftFromTypedTextPicksTheLastMatch(t *testing.T) {
	screen, _ := openTunnelForm(t, "alpha", "db-prod", "db-stage")
	model := typeTunnelText(screen, "db")
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyLeft})
	if host := model.(tunnelScreen).form.host; host != "db-stage" {
		t.Fatalf("host = %q", host)
	}
}

func TestTunnelScreenSavesARunningOneTimeTunnelWithS(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	manager := screen.environment.manager
	if _, err := manager.up(context.Background(), "adhoc", tunnelSpec{Host: "jump1", Local: "auto", Remote: "cache:6379"}); err != nil {
		t.Fatal(err)
	}
	updated, _ := screen.Update(tunnelRowsMsg(manager.rows(context.Background(), screen.environment.specs)))
	screen = updated.(tunnelScreen)
	if first := strings.SplitN(ansi.Strip(screen.View().Content), "\n", 2)[0]; !strings.Contains(first, "s save") {
		t.Errorf("a one-time row must offer save: %q", first)
	}
	model, command := tunnelKey(screen, tea.KeyPressMsg{Code: 's', Text: "s"})
	model = runTunnelCmd(t, model, command)
	final := model.(tunnelScreen)
	if final.failed || final.environment.specs["adhoc"] != (tunnelSpec{Host: "jump1", Local: "auto", Remote: "cache:6379"}) {
		t.Fatalf("notice %q, specs %+v", final.notice, final.environment.specs)
	}
	data, err := os.ReadFile(final.environment.file)
	if err != nil || !strings.Contains(string(data), "local: auto") {
		t.Fatalf("file = %q, err %v", data, err)
	}
	if records := manager.store.load(); len(records) != 1 {
		t.Fatalf("saving must not touch the running tunnel: %+v", records)
	}
	final.cursor = 1
	if _, command := tunnelKey(final, tea.KeyPressMsg{Code: 's', Text: "s"}); command != nil {
		t.Error("s on a saved row must do nothing")
	}
}

func TestTunnelScreenReportsWhyAOneTimeTunnelWasNotSaved(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, screen tunnelScreen)
		want    string
		keeps   bool
	}{
		{"the tunnel stopped before the save", func(t *testing.T, screen tunnelScreen) {
			if _, err := screen.environment.manager.down(context.Background(), "adhoc"); err != nil {
				t.Fatal(err)
			}
		}, T("tunnel.error.not_running", "adhoc"), false},
		{"the definition file cannot be written", func(t *testing.T, screen tunnelScreen) {
			if err := os.MkdirAll(screen.environment.file, 0o755); err != nil { // 파일 자리에 디렉터리를 두어 저장이 실패하게 한다
				t.Fatal(err)
			}
		}, "tunnels.yaml", true},
		{"the saved file cannot be read back", func(t *testing.T, screen tunnelScreen) {
			broken := "tunnels:\n  broken:\n    host: jump1\n    local: not-a-port\n    remote: db:5432\n"
			if err := os.WriteFile(screen.environment.file, []byte(broken), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "broken", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			screen, _ := newTestTunnelScreen(t)
			manager := screen.environment.manager
			if _, err := manager.up(context.Background(), "adhoc", tunnelSpec{Host: "jump1", Local: "auto", Remote: "cache:6379"}); err != nil {
				t.Fatal(err)
			}
			updated, _ := screen.Update(tunnelRowsMsg(manager.rows(context.Background(), screen.environment.specs)))
			screen = updated.(tunnelScreen)
			model, command := tunnelKey(screen, tea.KeyPressMsg{Code: 's', Text: "s"})
			test.prepare(t, screen)
			final := runTunnelCmd(t, model, command).(tunnelScreen)
			if !final.failed || final.busy || !strings.Contains(final.notice, test.want) {
				t.Fatalf("failed %v, busy %v, notice %q, want %q", final.failed, final.busy, final.notice, test.want)
			}
			if _, saved := final.environment.specs["adhoc"]; saved {
				t.Errorf("a failed save must not mark the tunnel as saved: %+v", final.environment.specs)
			}
			if records := manager.store.load(); test.keeps && len(records) != 1 {
				t.Errorf("a failed save must leave the running tunnel: %+v", records)
			}
		})
	}
}

func TestTunnelScreenSavesOnlyARunningUnsavedTunnelWithS(t *testing.T) {
	screen, _ := newTestTunnelScreen(t)
	for _, test := range []struct {
		row  tunnelRow
		want bool
	}{
		{tunnelRow{Name: "adhoc", State: tunnelStateUp}, true},
		{tunnelRow{Name: "adhoc", State: tunnelStateStale}, true},
		{tunnelRow{Name: "adhoc", State: tunnelStateDown}, false},
		{tunnelRow{Name: "adhoc", State: tunnelStateUp, Defined: true}, false},
	} {
		screen.rows, screen.cursor = []tunnelRow{test.row}, 0
		model, command := tunnelKey(screen, tea.KeyPressMsg{Code: 's', Text: "s"})
		if (command != nil) != test.want || model.(tunnelScreen).busy != test.want {
			t.Errorf("s on %+v: command %v, busy %v, want %v", test.row, command != nil, model.(tunnelScreen).busy, test.want)
		}
	}
}

func newQuickTunnelScreen(t *testing.T) (tunnelScreen, *fakeTunnelRunner) {
	t.Helper()
	screen, runner := newTestTunnelScreen(t)
	screen.quick, screen.mode = true, tunnelModeForm
	screen.form = screen.newForm()
	screen.form.hosts, screen.form.host = []string{"jump1"}, "jump1"
	return screen, runner
}

func TestTunnelQuickFormStartsOneTunnelAndQuitsWithoutSaving(t *testing.T) {
	screen, runner := newQuickTunnelScreen(t)
	if screen.form.save || screen.Init() != nil {
		t.Fatal("the quick form must default to a one-time tunnel and must not poll the list")
	}
	model := typeTunnelText(screen, "jump1 6000:cache:6379")
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, T("tunnel.form.starting")) {
		t.Errorf("the form must show that it is starting:\n%s", view)
	}
	if next, ignored := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter}); ignored != nil || next.(tunnelScreen).done != nil {
		t.Fatal("a second enter while starting must not start again")
	}
	if command == nil {
		t.Fatal("want a start command")
	}
	model, quit := model.Update(command())
	final := model.(tunnelScreen)
	if quit == nil || final.done == nil || len(runner.started) != 1 {
		t.Fatalf("done %+v, started %+v", final.done, runner.started)
	}
	if _, isQuit := quit().(tea.QuitMsg); !isQuit {
		t.Fatal("a started tunnel must close the form")
	}
	text := final.done.notice + "\n" + strings.Join(final.done.details, "\n")
	for _, want := range []string{"127.0.0.1:6000", "$ ssh -N -L 127.0.0.1:6000:cache:6379"} {
		if !strings.Contains(text, want) {
			t.Errorf("the result misses %q: %q", want, text)
		}
	}
	if _, err := os.Stat(final.environment.file); err == nil {
		t.Error("the quick form must not save by default")
	}
}

func TestTunnelQuickFormCancelsTheStartAndWaitsForItsResult(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{
		{"the tunnel came up anyway", nil},
		{"the start stopped", errors.New("signal: killed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			screen, runner := newQuickTunnelScreen(t)
			runner.failure = test.failure
			cancelled := 0
			screen.cancel = func() { cancelled++ }
			model := typeTunnelText(screen, "jump1 6000:cache:6379")
			model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
			model, quit := tunnelKey(model, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			if quit != nil || cancelled != 1 {
				t.Fatalf("ctrl+c while starting must cancel the start and wait: quit %v, cancelled %d", quit != nil, cancelled)
			}
			model, quit = model.Update(command())
			final := model.(tunnelScreen)
			if quit == nil {
				t.Fatal("the form must close when the cancelled start reports")
			}
			if test.failure == nil && (final.done == nil || final.aborted != nil) {
				t.Fatalf("a tunnel that came up must be reported: %+v", final)
			}
			if test.failure != nil && (final.done != nil || final.aborted == nil) {
				t.Fatalf("a stopped start must be reported as an error: %+v", final)
			}
		})
	}
}

func TestTunnelQuickFormKeepsTheInputWhenTheStartFails(t *testing.T) {
	screen, runner := newQuickTunnelScreen(t)
	runner.failure = errors.New("Permission denied (publickey)")
	model := typeTunnelText(screen, "jump1 6000:cache:6379")
	model, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	model, quit := model.Update(command())
	final := model.(tunnelScreen)
	if quit != nil || final.done != nil || final.mode != tunnelModeForm || final.busy {
		t.Fatalf("a failed start must stay in the form: %+v", final)
	}
	if final.form.host != "jump1" || final.form.port != "6000" || final.form.destination != "cache:6379" || !strings.Contains(final.form.problem, "Permission denied") {
		t.Fatalf("form = %+v", final.form)
	}
	_, quit = tunnelKey(final, tea.KeyPressMsg{Code: tea.KeyEscape})
	if quit == nil {
		t.Fatal("esc must close the quick form")
	}
}

func TestTunnelFormTimeLimitReachesTheTunnelAndSurvivesARestart(t *testing.T) {
	screen, runner := openTunnelForm(t, "jump1")
	var timed []time.Duration
	screen.environment.manager.expire = func(_ tunnelStore, record tunnelRecord) error {
		timed = append(timed, record.TTL)
		return nil
	}
	screen.now = screen.environment.manager.now
	screen.form.destination, screen.form.name, screen.form.field = "db.internal:5432", "timed", tunnelFieldTTL
	if view := ansi.Strip(screen.View().Content); !strings.Contains(view, "time limit") || !strings.Contains(view, "stops by itself") {
		t.Errorf("the form must explain the time limit:\n%s", view)
	}
	model := typeTunnelText(screen, "soon")
	blocked, command := tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil || !strings.Contains(blocked.(tunnelScreen).form.problem, "soon") {
		t.Fatalf("an unreadable time limit must keep the form: %+v", blocked.(tunnelScreen).form)
	}
	model, _ = tunnelKey(model, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	model = typeTunnelText(model, "30m")
	model, command = tunnelKey(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	model = runTunnelCmd(t, model, command)
	final := model.(tunnelScreen)
	if len(runner.started) != 1 || len(timed) != 1 || timed[0] != 30*time.Minute {
		t.Fatalf("started %d, timers %v, notice %q", len(runner.started), timed, final.notice)
	}
	rows := final.environment.manager.rows(context.Background(), final.environment.specs)
	model, _ = final.Update(tunnelRowsMsg(rows))
	final = model.(tunnelScreen)
	if view := ansi.Strip(final.View().Content); !strings.Contains(view, "expires") || !strings.Contains(view, "30m0s") {
		t.Errorf("the list must show the time that remains:\n%s", view)
	}
	for index, row := range final.rows {
		if row.Name == "timed" {
			final.cursor = index
		}
	}
	model, command = tunnelKey(final, tea.KeyPressMsg{Code: 'r', Text: "r"})
	runTunnelCmd(t, model, command)
	if len(runner.started) != 2 || len(timed) != 2 || timed[1] != 30*time.Minute {
		t.Fatalf("a restart must keep the time limit: started %d, timers %v", len(runner.started), timed)
	}
}
