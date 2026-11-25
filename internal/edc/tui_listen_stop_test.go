package edc

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type fakeListenStop struct {
	stopped, closed int
	err             error
}

func (process *fakeListenStop) stop() error { process.stopped++; return process.err }

func (process *fakeListenStop) close() { process.closed++ }

func listenStopFixture(t *testing.T) (listenBrowser, *fakeListenStop, *[]string) {
	t.Helper()
	model := listenBrowserFixture(t)
	process := &fakeListenStop{}
	var opened []string
	model.openStop = func(pid int, name string, listed time.Time) (listenStopProcess, error) {
		if listed != model.listed || listed.IsZero() {
			t.Errorf("openStop got list time %v, want %v", listed, model.listed)
		}
		opened = append(opened, strconv.Itoa(pid)+" "+name)
		return process, nil
	}
	return model, process, &opened
}

func TestListenBrowserStopCancelsUnlessYesIsChosen(t *testing.T) {
	for _, key := range []string{"enter", "n", "esc", "q", "k", "j", "l", "S", "r"} {
		t.Run(key, func(t *testing.T) {
			model, process, opened := listenStopFixture(t)
			model, cmd := listenBrowserKey(model, "S")
			if cmd != nil || model.stop == nil || strings.Join(*opened, ",") != "100 api-server" {
				t.Fatalf("S must open a confirmation for the selected PID: %v", *opened)
			}
			if view := model.View().Content; !strings.Contains(view, "SIGTERM") || !strings.Contains(view, "8080/tcp") {
				t.Fatalf("confirmation missing from view:\n%s", view)
			}
			model, cmd = listenBrowserKey(model, key)
			if cmd != nil || model.stop != nil || process.stopped != 0 || process.closed != 1 {
				t.Fatalf("%s must cancel without a signal: cmd=%v stopped=%d closed=%d", key, cmd != nil, process.stopped, process.closed)
			}
			if model.notice != T("listenbrowse.stop_cancelled") || model.hideLoopback || model.cursor != 0 || model.loading {
				t.Fatalf("%s acted on the list while cancelling: notice=%q", key, model.notice)
			}
		})
	}
}

func TestListenBrowserStopSendsSIGTERMAndReportsTheSockets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []tea.KeyPressMsg
		remains bool
		want    string
	}{
		{"y-closed", []tea.KeyPressMsg{topKey("y")}, false, "listenbrowse.stop_closed"},
		{"arrow-enter-open", []tea.KeyPressMsg{{Code: tea.KeyLeft}, topKey("enter")}, true, "listenbrowse.stop_still_open"},
		{"tab-enter-closed", []tea.KeyPressMsg{topKey("tab"), topKey("enter")}, false, "listenbrowse.stop_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, process, _ := listenStopFixture(t)
			model, _ = listenBrowserKey(model, "S")
			var cmd tea.Cmd
			for _, key := range tc.keys {
				next, command := model.Update(key)
				model, cmd = next.(listenBrowser), command
			}
			if cmd == nil || model.stop != nil {
				t.Fatal("yes must schedule the signal")
			}
			done := cmd()
			if process.stopped != 1 || process.closed != 1 {
				t.Fatalf("stopped=%d closed=%d", process.stopped, process.closed)
			}
			next, tick := model.Update(done)
			model = next.(listenBrowser)
			if tick == nil || model.notice != T("listenbrowse.stop_sent", "api-server", 100) {
				t.Fatalf("notice=%q", model.notice)
			}
			next, load := model.Update(listenStopCheckMsg{pid: 100})
			model = next.(listenBrowser)
			if load == nil || !model.loading {
				t.Fatal("the check must reload the sockets")
			}
			sockets := []listenSocket{{Proto: "tcp", Port: 5432, Address: "127.0.0.1", Process: "postgres", PID: "200"}}
			if tc.remains {
				sockets = append(sockets, listenSocket{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "100"})
			}
			next, _ = model.Update(listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": sockets}}, at: time.Now()})
			model = next.(listenBrowser)
			if model.notice != T(tc.want, 100) || model.stopped != 0 {
				t.Fatalf("notice=%q", model.notice)
			}
		})
	}
}

func TestListenBrowserStopFromDetailReturnsWhenTheRowIsGone(t *testing.T) {
	model, _, opened := listenStopFixture(t)
	model, _ = listenBrowserKey(model, "down")
	model, load := listenBrowserKey(model, "enter")
	next, _ := model.Update(load())
	model = next.(listenBrowser)
	model, _ = listenBrowserKey(model, "S")
	if !strings.Contains(model.View().Content, "SIGTERM") || strings.Join(*opened, ",") != "200 postgres" {
		t.Fatalf("detail confirmation: %v\n%s", *opened, model.View().Content)
	}
	model, cmd := listenBrowserKey(model, "y")
	next, _ = model.Update(cmd())
	next, _ = next.(listenBrowser).Update(listenStopCheckMsg{pid: 200})
	model = next.(listenBrowser)
	next, _ = model.Update(listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": []listenSocket{
		{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "100"},
	}}}})
	model = next.(listenBrowser)
	if model.detail != nil || model.notice != T("listenbrowse.stop_closed", 200) {
		t.Fatalf("detail=%v notice=%q", model.detail != nil, model.notice)
	}
}

func TestListenBrowserStopRefusesWithoutOpeningTheProcess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*listenBrowser)
		want  string
	}{
		{"redact", func(model *listenBrowser) { model.redact = true }, T("listenbrowse.stop_redacted")},
		{"container", func(model *listenBrowser) {
			model.items[0].Sockets[0].Container = &listenContainer{Name: "web"}
		}, T("listenbrowse.stop_container", "web")},
		{"no-owner", func(model *listenBrowser) { model.items[0].Sockets[0].PID = "" }, T("listenbrowse.no_owner")},
		{"init", func(model *listenBrowser) { model.items[0].Sockets[0].PID = "1" }, T("listenbrowse.stop_refused", 1)},
		{"self", func(model *listenBrowser) { model.items[0].Sockets[0].PID = strconv.Itoa(os.Getpid()) }, T("listenbrowse.stop_refused", os.Getpid())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := listenBrowserFixture(t)
			model.openStop = func(int, string, time.Time) (listenStopProcess, error) {
				t.Fatal("refused row opened the process")
				return nil, nil
			}
			tc.setup(&model)
			model, cmd := listenBrowserKey(model, "S")
			if cmd != nil || model.stop != nil || model.notice != tc.want {
				t.Fatalf("notice=%q", model.notice)
			}
		})
	}
}

func TestListenBrowserStopReportsOpenAndSignalErrors(t *testing.T) {
	for _, err := range []error{errListenStopPermission, errListenStopChanged, errListenStopGone, errListenStopUnsupported} {
		model := listenBrowserFixture(t)
		model.openStop = func(int, string, time.Time) (listenStopProcess, error) { return nil, err }
		model, _ = listenBrowserKey(model, "S")
		if model.stop != nil || model.notice != listenStopMessage(100, err) {
			t.Fatalf("%v: notice=%q", err, model.notice)
		}
	}
	model, process, _ := listenStopFixture(t)
	process.err = errListenStopPermission
	model, _ = listenBrowserKey(model, "S")
	model, cmd := listenBrowserKey(model, "y")
	next, tick := model.Update(cmd())
	if tick != nil || next.(listenBrowser).notice != T("listenbrowse.stop_permission", 100) || next.(listenBrowser).stopped != 0 {
		t.Fatalf("notice=%q", next.(listenBrowser).notice)
	}
}

func TestListenBrowserStopQuitsOnCtrlCAndClosesTheProcess(t *testing.T) {
	model, process, _ := listenStopFixture(t)
	model, _ = listenBrowserKey(model, "S")
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil || process.closed != 1 || process.stopped != 0 {
		t.Fatalf("ctrl+c: closed=%d stopped=%d", process.closed, process.stopped)
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c must quit")
	}
}

func TestListenBrowserStopPortsListsEveryPortOfThePID(t *testing.T) {
	model := listenBrowserFixture(t)
	model.sockets = []listenSocket{
		{Proto: "tcp", Port: 80, PID: "7"}, {Proto: "tcp", Port: 80, Address: "::", PID: "7"},
		{Proto: "tcp", Port: 443, PID: "7"}, {Proto: "unix", Address: "/run/a.sock", PID: "7"}, {Proto: "tcp", Port: 22, PID: "8"},
	}
	if got := model.stopPorts(7); got != "80/tcp, 443/tcp, unix" {
		t.Fatalf("ports=%q", got)
	}
}

func TestListenBrowserStopCapsThePortList(t *testing.T) {
	model := listenBrowserFixture(t)
	model.sockets = []listenSocket{
		{Proto: "tcp", Port: 80, PID: "7"}, {Proto: "tcp", Port: 443, PID: "7"}, {Proto: "unix", Address: "/run/a.sock", PID: "7"},
		{Proto: "udp", Port: 53, PID: "7"}, {Proto: "tcp", Port: 8443, PID: "7"},
	}
	if got := model.stopPorts(7); got != "80/tcp, 443/tcp, unix +2" {
		t.Fatalf("ports=%q", got)
	}
}

func TestListenBrowserStopShowsTheChoiceAtTheNarrowestWidth(t *testing.T) {
	model, _, _ := listenStopFixture(t)
	for port := 9000; port < 9010; port++ {
		model.sockets = append(model.sockets, listenSocket{Proto: "tcp", Port: port, Address: "0.0.0.0", Process: "api-server", PID: "100"})
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 48, Height: 8})
	model, _ = listenBrowserKey(next.(listenBrowser), "S")
	view := model.View().Content
	lines := strings.Split(view, "\n")
	choice := liveIdleBar + " " + confirmYesLabel() + " " + "  " + liveSelectedBar + " " + confirmNoLabel()
	if len(lines) > 8 || !strings.Contains(view, choice) || !strings.Contains(view, "┌ "+T("listenbrowse.stop_title")) {
		t.Fatalf("the box must fit 48x8 and keep both answers, with no selected:\n%s", view)
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > 48 {
			t.Fatalf("line wider than the screen: %q", line)
		}
	}
	if !strings.Contains(model.stop.question, "+") {
		t.Fatalf("ports must be capped: %q", model.stop.question)
	}
}

func TestListenBrowserStopCancelsWhenTheScreenGetsTooSmall(t *testing.T) {
	model, process, _ := listenStopFixture(t)
	model, _ = listenBrowserKey(model, "S")
	next, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	model = next.(listenBrowser)
	if model.stop != nil || process.closed != 1 {
		t.Fatal("a hidden confirmation must be cancelled")
	}
	model, cmd := listenBrowserKey(model, "S")
	if model.stop != nil || cmd != nil {
		t.Fatal("S must not open a confirmation that the size notice hides")
	}
	if _, cmd = listenBrowserKey(model, "y"); cmd != nil || process.stopped != 0 {
		t.Fatal("y sent a signal on a small screen")
	}
}

func TestListenBrowserStopTargetsThePIDThatPSelected(t *testing.T) {
	model, _, opened := listenStopFixture(t)
	model.sockets = append(model.sockets, listenSocket{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "101"})
	model.rebuild()
	model, _ = listenBrowserKey(model, "p")
	model, _ = listenBrowserKey(model, "S")
	if strings.Join(*opened, ",") != "101 api-server" || model.stop == nil || model.stop.pid != 101 {
		t.Fatalf("S must use the PID that p selected: %v", *opened)
	}
	if !strings.Contains(model.View().Content, T("listenbrowse.stop_one_of", 2)) {
		t.Fatalf("the box must say the row has other PIDs:\n%s", model.View().Content)
	}
}

func TestListenBrowserStopIgnoresARefreshStartedBeforeTheSignal(t *testing.T) {
	model, _, _ := listenStopFixture(t)
	model, _ = listenBrowserKey(model, "r")
	earlier := model.seq
	model, _ = listenBrowserKey(model, "S")
	model, cmd := listenBrowserKey(model, "y")
	next, _ := model.Update(cmd())
	model = next.(listenBrowser)
	still := []listenSocket{{Proto: "tcp", Port: 8080, Address: "0.0.0.0", Process: "api-server", PID: "100"}}
	next, _ = model.Update(listenBrowserListMsg{seq: earlier, result: Result{Status: StatusPass, Metrics: map[string]interface{}{"sockets": still}}})
	if model = next.(listenBrowser); model.notice != T("listenbrowse.stop_sent", "api-server", 100) {
		t.Fatalf("a read from before the signal decided the result: %q", model.notice)
	}
}

func TestListenBrowserKeyLinesKeepQuitVisibleAt80Columns(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		model := listenBrowserFixture(t)
		next, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		model = next.(listenBrowser)
		list := strings.Split(model.View().Content, "\n")[1]
		model, load := listenBrowserKey(model, "enter")
		next, _ = model.Update(load())
		detail := strings.Split(next.(listenBrowser).View().Content, "\n")[1]
		for i, line := range []string{list, detail} {
			var quit string
			for _, group := range liveHintSeparator.Split(T([]string{"listenbrowse.keys", "listenbrowse.detail_keys"}[i]), -1) {
				if strings.HasPrefix(group, "q ") {
					quit = group
				}
			}
			if quit == "" || !strings.Contains(line, quit) {
				t.Errorf("%s: %q is cut at 80 columns: %q", language, quit, line)
			}
		}
	}
}

func TestListenBrowserStopDimsTheBackground(t *testing.T) {
	model, _, _ := listenStopFixture(t)
	model.color = true
	if !liveRich(model.color) {
		t.Skip("rich output is off in this environment")
	}
	// 상자가 가운데에 겹치므로, 위쪽 줄이 상자 밖에 남게 목록을 화면 높이만큼 채운다.
	for port := 9000; port < 9030; port++ {
		model.sockets = append(model.sockets, listenSocket{Proto: "tcp", Port: port, Address: "127.0.0.1", Process: "worker", PID: strconv.Itoa(port)})
	}
	model.rebuild()
	before := strings.Split(model.View().Content, "\n")
	model, _ = listenBrowserKey(model, "S")
	after := strings.Split(model.View().Content, "\n")
	for _, index := range []int{0, 1, 3} {
		if want := richDim(ansi.Strip(before[index])); after[index] != want {
			t.Fatalf("line %d must be the same text in gray:\n got %q\nwant %q", index, after[index], want)
		}
	}
	if !strings.Contains(model.View().Content, richPaint(" "+T("listenbrowse.stop_title")+" ", richMagenta, true)) {
		t.Fatal("the box title must stay highlighted")
	}
}

func TestListenBrowserStopBoxFitsTheScreenWithALongName(t *testing.T) {
	model, _, _ := listenStopFixture(t)
	long := strings.Repeat("very-long-process-name-", 8)
	model.items[0].Sockets[0].Process = long
	model.sockets = append(model.sockets, listenSocket{Proto: "tcp", Port: 8080, Address: "::", Process: long, PID: "101"})
	model.rebuild()
	model.openStop = func(int, string, time.Time) (listenStopProcess, error) { return &fakeListenStop{}, nil }
	next, _ := model.Update(tea.WindowSizeMsg{Width: 48, Height: 8})
	model, _ = listenBrowserKey(next.(listenBrowser), "S")
	box := model.stopBox()
	if len(box) > 8 || !strings.Contains(strings.Join(box, "\n"), confirmNoLabel()) || !strings.HasPrefix(box[len(box)-1], "└") {
		t.Fatalf("box must fit 8 rows and keep the answers and the border:\n%s", strings.Join(box, "\n"))
	}
}
