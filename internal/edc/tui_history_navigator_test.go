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
	if len(navigator.stack) != 1 || root.notice == "" {
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
