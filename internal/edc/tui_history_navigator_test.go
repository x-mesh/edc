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
