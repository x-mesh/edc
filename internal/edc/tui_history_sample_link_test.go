package edc

import (
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestHistoryNavigatorLinksUseFixtureData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	process := topProcess{PID: 7, Command: "worker", Started: time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)}
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 20, process)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	if err := recordSnapshot(path, snapshotHistoryReport(time.Now(), 100, 1000), time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := historyListReviewData(db, historyOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := newHistoryListBrowser(path, list)
	for i, entry := range root.data.entries {
		if entry.id == recorder.runID {
			root.selected = i
		}
	}
	navigator := newHistoryNavigator(db, path, root)
	for _, key := range []tea.KeyPressMsg{{Code: 't', Text: "t"}, {Code: 'o', Text: "o"}} {
		next, _ := navigator.Update(key)
		navigator = next.(historyNavigator)
	}
	child, ok := navigator.current().(historySampleBrowser)
	if !ok || !child.processMode || len(child.processes) == 0 || child.processes[0].RunID != recorder.runID {
		t.Fatalf("process child=%+v", navigator.current())
	}
	next, _ := navigator.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	navigator = next.(historyNavigator)
	trend, ok := navigator.current().(historyTrendBrowser)
	if !ok || trend.options.identity == nil || trend.options.identity.pid != process.PID {
		t.Fatalf("trend child=%+v", navigator.current())
	}
}
