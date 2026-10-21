package edc

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func reviewAfter(t *testing.T, model historyReviewBrowser, messages ...tea.Msg) historyReviewBrowser {
	t.Helper()
	for _, message := range messages {
		next, _ := model.Update(message)
		model = next.(historyReviewBrowser)
	}
	return model
}

func reviewFixture() historyReviewData {
	var data historyReviewData
	data.summary, data.empty = "fixture range", "no records"
	for i := 0; i < 80; i++ {
		data.entries = append(data.entries, historyReviewEntry{id: fmt.Sprint(i), cells: []string{fmt.Sprintf("/var/data/%d", i), "+1 MB/day", "20 MB", "12", "note"}, details: []string{strings.Repeat("long path/", 12), "details", "observations"}})
	}
	return data
}

func TestHistoryReviewBrowserLayoutSelectionAndRefresh(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, color := range []bool{false, true} {
			model := newHistoryReviewBrowser("Growth", "history.db", []historyReviewColumn{{"TARGET", 0, 0}, {"CHANGE/DAY", 13, 0}, {"LATEST", 11, 64}, {"SAMPLES", 7, 84}, {"NOTE", 24, 120}}, reviewFixture())
			model.color = color
			for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 12}, {Width: 80, Height: 24}, {Width: 160, Height: 40}} {
				model = reviewAfter(t, model, size, tea.KeyPressMsg{Code: tea.KeyEnd})
				lines := strings.Split(model.View().Content, "\n")
				if len(lines) != size.Height || !strings.Contains(lines[len(lines)-1], "q") || model.view.YOffset() == 0 {
					t.Fatalf("invalid layout %s/%+v: %q", language, size, lines)
				}
				for _, line := range lines {
					if liveWidth(line) > size.Width || !color && strings.Contains(line, "\x1b[") {
						t.Fatalf("oversized or colored plain line: %q", line)
					}
				}
			}
			model = reviewAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
			if !model.expanded || !strings.Contains(model.View().Content, "details") {
				t.Fatal("details missing")
			}
			for _, line := range model.detailLines(42) {
				if strings.Contains(line, "\x1b[") && !strings.HasSuffix(line, richReset) {
					t.Fatal("wrapped title leaks color")
				}
			}
			id := model.data.entries[model.selected].id
			fresh := reviewFixture()
			fresh.entries = append(fresh.entries[1:], fresh.entries[:1]...)
			model = reviewAfter(t, model, historyReviewLoadedMsg{seq: model.seq, data: fresh})
			if model.data.entries[model.selected].id != id || !model.expanded {
				t.Fatal("refresh lost selection")
			}
			model = reviewAfter(t, model, historyReviewLoadedMsg{seq: model.seq, data: historyReviewData{empty: "no records"}})
			if model.expanded || model.selected != 0 || !strings.Contains(model.View().Content, "no records") {
				t.Fatal("empty refresh unsafe")
			}
		}
	}
}

func TestHistoryListBrowserRowsDetailsAndWidths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	first := topHistoryFixture(t, path)
	if err := first.Record(historyFixtureSample(1, 20, topProcess{PID: 1})); err != nil {
		t.Fatal(err)
	}
	if err := first.Close("finished"); err != nil {
		t.Fatal(err)
	}
	second := topHistoryFixture(t, path)
	if err := second.Record(historyFixtureSample(2, 30, topProcess{PID: 2})); err != nil {
		t.Fatal(err)
	}
	if err := second.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	data, err := historyListReviewData(db, historyOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.entries) != 2 || data.entries[0].id != second.runID || !strings.Contains(data.entries[0].cells[0], "Today") {
		t.Fatalf("list entries = %+v", data.entries)
	}
	model := newHistoryListBrowser(path, data)
	for _, width := range []int{80, 120, 180} {
		model = reviewAfter(t, model, tea.WindowSizeMsg{Width: width, Height: 24})
		for _, line := range strings.Split(model.View().Content, "\n") {
			if liveWidth(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
	view := model.View().Content
	if !model.expanded || !strings.Contains(view, second.runID) || !strings.Contains(view, T("history.list_browser.process", "—")) {
		t.Fatalf("details = %q", view)
	}
}

func TestHistoryPruneBrowserRequiresConfirmationAndDryRunNeverDeletes(t *testing.T) {
	var plan historyPrunePlan
	for i := 0; i < 40; i++ {
		plan.add(historyPruneEntry{run: historyRun{ID: fmt.Sprint(i), Command: "top", Hostname: "fixture", StartedAt: time.Now()}, lastActivity: time.Now().Add(-30 * 24 * time.Hour), samples: 5})
	}
	for _, dryRun := range []bool{false, true} {
		model := newHistoryPruneBrowser("history.db", historyPruneOptions{olderThan: 7 * 24 * time.Hour, dryRun: dryRun}, plan, time.Now())
		if model.Init() != nil {
			t.Fatal("prune should refresh only on request")
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'y', Text: "y"})
		if model.confirmed {
			t.Fatal("y without a deletion prompt confirmed deletion")
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: ' ', Text: " "})
		if len(model.selectedIDs()) != 39 {
			t.Fatal("Space did not uncheck selected run")
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"})
		if dryRun {
			model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'y', Text: "y"})
			if model.confirm || model.confirmed {
				t.Fatal("dry-run allowed deletion")
			}
			continue
		}
		if !model.confirm || model.confirmed {
			t.Fatal("d must open confirmation without deleting")
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'n', Text: "n"})
		if model.confirm || model.confirmed {
			t.Fatal("cancellation failed")
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"}, tea.KeyPressMsg{Code: 'y', Text: "y"})
		if !model.confirmed || len(selectedHistoryPrunePlan(model.data.plan, model.selectedIDs()).runs) != 39 {
			t.Fatal("confirmation lost chosen runs")
		}
	}
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 12}, {Width: 80, Height: 24}, {Width: 160, Height: 40}} {
			model := newHistoryPruneBrowser("history.db", historyPruneOptions{olderThan: time.Hour}, plan, time.Now())
			model = reviewAfter(t, model, size)
			lines := strings.Split(model.View().Content, "\n")
			if len(lines) != size.Height || !strings.Contains(lines[len(lines)-1], "q") {
				t.Fatalf("prune hints clipped: %q", lines)
			}
			model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"})
			if !strings.Contains(model.View().Content, "y") {
				t.Fatal("confirmation not visible")
			}
		}
	}
}

func TestHistoryGrowthReviewIncludesDirectoriesAndProcesses(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db := historyGrowthDB(t, start, []uint64{100, 300, 500}, []uint64{1000, 3000, 5000})
	growth, err := collectHistoryGrowth(db, historyGrowthOptions{}, start.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data := historyGrowthReviewData(growth)
	if len(data.entries) != 3 || !strings.Contains(data.entries[2].cells[0], "/var/lib/data") || data.entries[0].cells[1] != historyGrowthRate(200) {
		t.Fatalf("growth entries incomplete: %+v", data.entries)
	}
	process := historyGrowthProcess{PID: 12, Command: "/usr/bin/worker", Samples: 12, RSSBytes: 4096, BytesPerDay: new(float64), Unfiltered: true}
	data = historyGrowthReviewData(historyGrowth{Process: "worker", Processes: []historyGrowthProcess{process}})
	if len(data.entries) != 1 || !strings.Contains(data.entries[0].cells[0], "12 worker") || !strings.Contains(strings.Join(data.entries[0].details, "\n"), "top -w --process") {
		t.Fatal("process growth or observation coverage note missing")
	}
}

func TestHistoryPruneRechecksActivityAndSelectedRuns(t *testing.T) {
	path, ids := historyPruneFixture(t)
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	options := historyPruneOptions{olderThan: 7 * 24 * time.Hour}
	plan, err := planHistoryPrune(db, options, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE top_samples SET time_ns=? WHERE run_id=?", time.Now().UnixNano(), ids["old_top"]); err != nil {
		t.Fatal(err)
	}
	deleted, err := deleteEligibleHistoryRuns(db, options, plan.runs, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.runs) != 1 || deleted.runs[0] != ids["old_snapshot"] {
		t.Fatalf("deleted a newly active run: %+v", deleted)
	}
	left := historyRunIDs(t, path)
	if !left[ids["old_top"]] || !left[ids["new_snapshot"]] || !left[ids["running_top"]] {
		t.Fatal("unselected or recent runs were deleted")
	}
	path2, ids2 := historyPruneFixture(t)
	db2, err := openHistoryDB(path2, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	deleted, err = deleteEligibleHistoryRuns(db2, options, []string{ids2["old_top"]}, time.Now())
	if err != nil || len(deleted.runs) != 1 {
		t.Fatalf("subset deletion failed: %+v %v", deleted, err)
	}
	if !historyRunIDs(t, path2)[ids2["old_snapshot"]] {
		t.Fatal("unchecked eligible snapshot was deleted")
	}
}

func TestHistoryPruneRollsBackChildRowsOnFailure(t *testing.T) {
	path, _ := historyPruneFixture(t)
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	options := historyPruneOptions{olderThan: 7 * 24 * time.Hour}
	plan, err := planHistoryPrune(db, options, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := db.QueryRow("SELECT count(*) FROM snapshot_results").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_prune BEFORE DELETE ON runs BEGIN SELECT RAISE(ABORT, 'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteEligibleHistoryRuns(db, options, plan.runs, time.Now()); err == nil {
		t.Fatal("deletion failure was ignored")
	}
	if err := db.QueryRow("SELECT count(*) FROM snapshot_results").Scan(&after); err != nil || before != after {
		t.Fatal("child rows were not rolled back")
	}
	if len(historyRunIDs(t, filepath.Clean(path))) != 4 {
		t.Fatal("run deletion was not rolled back")
	}
}
