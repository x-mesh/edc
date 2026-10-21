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
	for _, key := range []string{"t", "esc", "p", "esc"} {
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
