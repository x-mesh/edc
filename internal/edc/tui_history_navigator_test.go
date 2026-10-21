package edc

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestHistoryNavigatorPushesRunChildrenAndRestoresRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 20, topProcess{PID: 1})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	data, err := historyListReviewData(db, historyOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := newHistoryListBrowser(path, data)
	navigator := newHistoryNavigator(db, path, root)
	for _, width := range []int{80, 120, 180} {
		next, _ := navigator.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		navigator = next.(historyNavigator)
		for _, line := range strings.Split(navigator.View().Content, "\n") {
			if liveWidth(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
	}
	for _, key := range []string{"t", "esc", "o", "esc"} {
		next, _ := navigator.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		navigator = next.(historyNavigator)
	}
	if len(navigator.stack) != 1 || navigator.current().(historyReviewBrowser).selected != 0 {
		t.Fatalf("stack did not restore root: %+v", navigator)
	}
	if _, cmd := navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); cmd == nil {
		t.Fatal("Esc at root did not quit")
	}
}

func TestHistoryNavigatorRunListOverviewAndInlineDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 20, topProcess{PID: 1})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	data, err := historyListReviewData(db, historyOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := newHistoryListBrowser(path, data)
	navigator := newHistoryNavigator(db, path, root)
	next, _ := navigator.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	navigator = next.(historyNavigator)
	overview, ok := navigator.current().(historyOverviewBrowser)
	if !ok || !overview.hosted || overview.options.run != recorder.runID {
		t.Fatalf("overview=%+v", navigator.current())
	}
	next, _ = navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	navigator = next.(historyNavigator)
	if len(navigator.stack) != 1 || navigator.current().(historyReviewBrowser).selected != 0 {
		t.Fatal("Esc did not restore run list")
	}
	next, _ = navigator.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	navigator = next.(historyNavigator)
	if !navigator.current().(historyReviewBrowser).expanded {
		t.Fatal("i did not open inline detail")
	}
}

func TestHistoryRunListHostedFooterOmitsGenericEnter(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		data := historyReviewData{entries: []historyReviewEntry{{id: "run", command: "top", cells: make([]string, 6)}}}
		direct := newHistoryListBrowser("history.db", data)
		direct.color = false
		directHelp := strings.Join(direct.helpLines(), "\n")
		hosted := newHistoryNavigator(nil, "history.db", direct).current().(historyReviewBrowser)
		help := strings.Join(hosted.helpLines(), "\n")
		lines := strings.Split(help, "\n")
		if directHelp == help || strings.Contains(lines[0], "Enter ") || !strings.Contains(help, T("history.list_browser.navigator_help")) {
			t.Fatalf("%s direct=%q hosted=%q", language, directHelp, help)
		}
		hosted.data.entries[0].command = historySnapshotCommand
		snapshotHelp := strings.Join(hosted.helpLines(), "\n")
		if strings.Contains(snapshotHelp, "Enter Details") || !strings.Contains(snapshotHelp, T("history.list_browser.snapshot_navigator_help")) {
			t.Fatalf("%s snapshot=%q", language, snapshotHelp)
		}
	}
}

func TestHistoryNavigatorPropagatesSizeAndClosesChildDetailBeforePop(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{entries: []historyReviewEntry{{id: "run", cells: make([]string, 6)}}})
	navigator := newHistoryNavigator(nil, "history.db", root)
	next, _ := navigator.Update(tea.WindowSizeMsg{Width: 120, Height: 22})
	navigator = next.(historyNavigator)
	child := newHistoryTopBrowser([]historyTopSample{{RunID: "run"}}, "history.db")
	navigator.stack = append(navigator.stack, child)
	next, _ = navigator.Update(tea.WindowSizeMsg{Width: 120, Height: 22})
	navigator = next.(historyNavigator)
	if got := navigator.current().(historySampleBrowser).width; got != 120 {
		t.Fatalf("child width=%d", got)
	}
	child = navigator.current().(historySampleBrowser)
	child.expanded = true
	navigator.stack[len(navigator.stack)-1] = child
	next, _ = navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	navigator = next.(historyNavigator)
	if len(navigator.stack) != 2 || navigator.current().(historySampleBrowser).expanded {
		t.Fatal("Esc did not close child detail first")
	}
}

func TestHistoryNavigatorDeliversRootTickWhileChildOpen(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{entries: []historyReviewEntry{{id: "run", cells: make([]string, 6)}}})
	root.load = func() (historyReviewData, error) { return historyReviewData{}, nil }
	child := newHistoryTopBrowser([]historyTopSample{{RunID: "run"}}, "history.db")
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, child)
	next, cmd := navigator.Update(historyReviewTickMsg{seq: root.seq})
	navigator = next.(historyNavigator)
	if cmd == nil {
		t.Fatal("root tick did not re-arm while child open")
	}
	if _, ok := navigator.stack[0].(historyReviewBrowser); !ok {
		t.Fatal("root was not retained")
	}
}

func TestHistoryNavigatorKeepsRootStateWhenChildHandlesKeys(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{entries: []historyReviewEntry{{id: "run", cells: make([]string, 6)}, {id: "other", cells: make([]string, 6)}}})
	child := newHistoryTopBrowser([]historyTopSample{{RunID: "run"}}, "history.db")
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, child)
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyDown}, {Code: tea.KeyEnter}} {
		next, command := navigator.Update(key)
		if command != nil {
			if _, ok := command().(tea.QuitMsg); ok {
				t.Fatal("child key quit navigator")
			}
		}
		navigator = next.(historyNavigator)
	}
	root = navigator.stack[0].(historyReviewBrowser)
	if root.selected != 0 || root.expanded {
		t.Fatalf("hidden root changed: selected=%d expanded=%v", root.selected, root.expanded)
	}
	child = navigator.current().(historySampleBrowser)
	if !child.expanded {
		t.Fatal("Enter did not open child detail")
	}
	next, command := navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if command != nil {
		if _, ok := command().(tea.QuitMsg); ok {
			t.Fatal("Esc in child detail quit navigator")
		}
	}
	navigator = next.(historyNavigator)
	if len(navigator.stack) != 2 || navigator.current().(historySampleBrowser).expanded {
		t.Fatal("Esc did not close child detail")
	}
}

func TestHistoryNavigatorSnapshotShowsNoticeWithoutChild(t *testing.T) {
	data := historyReviewData{entries: []historyReviewEntry{{id: "snapshot", command: historySnapshotCommand, cells: []string{"translated snapshot", "", "", "", "", ""}}}}
	root := newHistoryListBrowser("history.db", data)
	navigator := newHistoryNavigator(nil, "history.db", root)
	next, _ := navigator.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	navigator = next.(historyNavigator)
	root = navigator.current().(historyReviewBrowser)
	if len(navigator.stack) != 1 || root.notice != T("history.list_browser.snapshot_navigation") {
		t.Fatalf("snapshot navigation=%+v", root)
	}
}

func TestHistoryNavigatorKeepsRunListPauseKey(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{entries: []historyReviewEntry{{id: "run", command: "top", cells: make([]string, 6)}}})
	root.load = func() (historyReviewData, error) { return historyReviewData{}, nil }
	navigator := newHistoryNavigator(nil, "history.db", root)
	next, _ := navigator.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	navigator = next.(historyNavigator)
	if !navigator.current().(historyReviewBrowser).paused {
		t.Fatal("p did not pause the run list")
	}
}

func TestHistoryNavigatorLinksSampleAndGrowthEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 20, topProcess{PID: 1, Command: "worker"})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	samples, err := loadHistoryTop(db, historyOptions{run: recorder.runID})
	if err != nil {
		t.Fatal(err)
	}
	top := newHistoryTopBrowser(samples, path)
	if child, ok, err := top.navigate("o", db, path); err != nil || !ok || !child.(historySampleBrowser).processMode || len(child.(historySampleBrowser).processes) == 0 {
		t.Fatalf("top link: %T %v %v", child, ok, err)
	}
	growth := historyGrowth{Processes: []historyGrowthProcess{{PID: 1, Command: "worker"}}, Disks: []historyGrowthDisk{{Mount: "/"}}}
	model := newHistoryGrowthNavigator(nil, "history.db", historyGrowthOptions{}, growth)
	if child, ok, err := model.navigate("t", nil, "history.db"); err != nil || !ok || child == nil {
		t.Fatalf("growth process link: %T %v %v", child, ok, err)
	}
	model.selected = 1
	if child, ok, err := model.navigate("t", nil, "history.db"); err != nil || !ok || child == nil {
		t.Fatalf("growth disk link: %T %v %v", child, ok, err)
	}
}

func TestHistoryProcessChildIgnoresParentTopLoad(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{})
	top := newHistoryTopBrowser([]historyTopSample{{RunID: "run"}}, "history.db")
	process := newHistoryProcessBrowser([]historyProcessSample{{RunID: "run"}}, "history.db")
	top.seq, process.seq = 1, 1
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, top, process)
	next, _ := navigator.Update(historySamplesLoadedMsg{seq: 1, samples: []historyTopSample{{RunID: "run"}}})
	navigator = next.(historyNavigator)
	top = navigator.stack[1].(historySampleBrowser)
	if len(top.samples) != 1 {
		t.Fatalf("parent top load was not applied: %+v", top.samples)
	}
	process = navigator.current().(historySampleBrowser)
	if len(process.processes) != 1 {
		t.Fatalf("navigator parent top load cleared process child: %+v", process.processes)
	}
}

func TestHistoryNavigatorReviewTickUsesOwner(t *testing.T) {
	hidden := newHistoryListBrowser("history.db", historyReviewData{summary: "hidden"})
	visible := newHistoryGrowthNavigator(nil, "history.db", historyGrowthOptions{}, historyGrowth{})
	hidden.seq, visible.seq = 1, 1
	hidden.load = func() (historyReviewData, error) { return historyReviewData{}, nil }
	visible.load = func() (historyReviewData, error) { return historyReviewData{}, nil }
	if _, command := hidden.Update(historyReviewTickMsg{owner: visible.owner, seq: visible.seq}); command != nil {
		t.Fatal("hidden review accepted visible tick")
	}
	navigator := newHistoryNavigator(nil, "history.db", hidden)
	navigator.stack = append(navigator.stack, visible)
	next, cmd := navigator.Update(historyReviewTickMsg{owner: visible.owner, seq: visible.seq})
	navigator = next.(historyNavigator)
	if cmd == nil || navigator.stack[0].(historyReviewBrowser).data.summary != "hidden" {
		t.Fatal("visible tick changed hidden review browser")
	}
}

func TestHistoryNavigatorReviewLoadUsesOwner(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{summary: "root"})
	growth := newHistoryGrowthNavigator(nil, "history.db", historyGrowthOptions{}, historyGrowth{})
	root.seq, growth.seq = 1, 1
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, growth)
	next, _ := navigator.Update(historyReviewLoadedMsg{owner: growth.owner, seq: 1, data: historyReviewData{summary: "growth"}})
	navigator = next.(historyNavigator)
	if navigator.stack[0].(historyReviewBrowser).data.summary != "root" {
		t.Fatal("growth load replaced run list data")
	}
	if navigator.current().(historyReviewBrowser).data.summary != "growth" {
		t.Fatal("growth did not accept its own load")
	}
}

func TestHistoryNavigatorSampleTickUsesOwner(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{})
	hidden := newHistoryTopBrowser([]historyTopSample{{RunID: "hidden"}}, "history.db")
	visible := newHistoryProcessBrowser([]historyProcessSample{{RunID: "visible"}}, "history.db")
	hidden.seq, visible.seq = 1, 1
	hidden.load = func() ([]historyTopSample, error) { return []historyTopSample{{RunID: "changed"}}, nil }
	visible.loadProcesses = func() ([]historyProcessSample, error) { return []historyProcessSample{{RunID: "fresh"}}, nil }
	if _, command := hidden.Update(historySamplesTickMsg{owner: visible.owner, seq: visible.seq}); command != nil {
		t.Fatal("hidden sample browser accepted visible tick")
	}
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, hidden, visible)
	next, command := navigator.Update(historySamplesTickMsg{owner: visible.owner, seq: visible.seq})
	navigator = next.(historyNavigator)
	if command == nil || navigator.stack[1].(historySampleBrowser).samples[0].RunID != "hidden" {
		t.Fatal("visible tick changed hidden sample browser")
	}
}

func TestHistoryNavigatorKeepsFilterInputKeysInSampleBrowser(t *testing.T) {
	root := newHistoryListBrowser("history.db", historyReviewData{})
	child := newHistoryTopBrowser(historyTopSamples(1), "history.db")
	child.loadWith = func(historyOptions) ([]historyTopSample, error) { return historyTopSamples(1), nil }
	navigator := newHistoryNavigator(nil, "history.db", root)
	navigator.stack = append(navigator.stack, child)
	next, _ := navigator.Update(tea.KeyPressMsg{Code: '/'})
	navigator = next.(historyNavigator)
	next, command := navigator.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if command != nil {
		t.Fatal("q quit while filter input was active")
	}
	navigator = next.(historyNavigator)
	current := navigator.current().(historySampleBrowser)
	if !current.filterEditing || current.filterText != "q" {
		t.Fatalf("filter input = editing:%t text:%q", current.filterEditing, current.filterText)
	}
	next, _ = navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	navigator = next.(historyNavigator)
	if len(navigator.stack) != 2 || navigator.current().(historySampleBrowser).filterEditing {
		t.Fatal("first Esc did not close only the filter input")
	}
	next, _ = navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	navigator = next.(historyNavigator)
	if len(navigator.stack) != 1 {
		t.Fatal("second Esc did not pop the sample browser")
	}
}
