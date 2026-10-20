package edc

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestHistoryTrendChoicesComeFromTheRecords(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db, path := historyTrendTopDB(t, map[time.Time]any{start: 10.0})
	if err := recordSnapshot(path, snapshotHistoryReport(start, 100, 1000), time.Hour, []string{"/var"}); err != nil {
		t.Fatal(err)
	}
	sources, err := historyTrendSources(db)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, item := range sources {
		values = append(values, item.value)
	}
	if strings.Join(values, " ") != "top du.scan info.disk:/ info.memory" || sources[0].label != "top · host samples" {
		t.Fatalf("sources = %+v", sources)
	}
	if keys, err := historyTrendKeys(db, "info.memory"); err != nil || strings.Join(keys, " ") != "available_bytes total_bytes used_bytes used_percent" {
		t.Fatalf("info.memory keys = %q, %v", keys, err)
	}
	if keys, err := historyTrendKeys(db, historyTopSource); err != nil || len(keys) != len(historyHostMetrics) {
		t.Fatalf("top keys = %d, %v", len(keys), err)
	}

	empty, _ := historyTrendTopDB(t, nil)
	if sources, err := historyTrendSources(empty); err != nil || len(sources) != 0 {
		t.Fatalf("an empty database offers %+v, %v", sources, err)
	}
}

func TestHistoryExportChoicesListSnapshotsNewestFirst(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db, path := historyTrendTopDB(t, map[time.Time]any{start: 10.0})
	var ids []string
	for index := range 2 {
		report := snapshotHistoryReport(start.Add(time.Duration(index)*time.Hour), 100, 1000)
		ids = append(ids, report.Run.ID)
		if err := recordSnapshot(path, report, time.Hour, nil); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := historyExportRuns(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].value != ids[1] || runs[1].value != ids[0] || !strings.Contains(runs[0].label, ids[1]+"  3 results") {
		t.Fatalf("runs = %+v, ids %q", runs, ids)
	}
}

func TestHistoryEchoPutsTheChoiceBeforeThePath(t *testing.T) {
	got := historyEchoCommand("trend", []string{"--probe", "info.disk:/Volumes/Kiro CLI", "--metric", "used_bytes"}, []string{"--by", "week", "/tmp/my history.db"})
	want := "edc history trend --probe 'info.disk:/Volumes/Kiro CLI' --metric used_bytes --by week '/tmp/my history.db'"
	if got != want {
		t.Fatalf("echo = %s", got)
	}
	for _, test := range []struct {
		args []string
		want bool
	}{
		{[]string{"--probe", "x"}, true},
		{[]string{"-probe=x"}, true},
		{[]string{"--by", "day"}, false},
		{[]string{"--", "--probe"}, false},
	} {
		if got := historyHasFlag(test.args, "probe"); got != test.want {
			t.Errorf("%q: %v", test.args, got)
		}
	}
	// 터미널이 아닌 실행은 묻지 않고 usage로 끝난다.
	missing := filepath.Join(t.TempDir(), "missing.db")
	if code := runHistoryExport([]string{missing}); code != 2 {
		t.Errorf("export without --run outside a terminal: exit code %d, want 2", code)
	}
}

func TestSelectModelScrollsALongList(t *testing.T) {
	var items []selectItem
	for index := range 40 {
		items = append(items, selectItem{label: fmt.Sprintf("item-%02d", index), value: fmt.Sprint(index)})
	}
	model := newSelectModel("history.prompt.metric_title", "history.prompt.metric_label", items)
	model.color = false
	for range 30 {
		next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = next.(selectModel)
	}
	view := model.View().Content
	lines := strings.Split(strings.TrimRight(view, "\n"), "\n")
	if len(lines) != selectVisibleRows+2 || !strings.Contains(view, "31/40") || !strings.Contains(view, "item-30") || strings.Contains(view, "item-00") {
		t.Fatalf("view (%d lines):\n%s", len(lines), view)
	}
	if start, end := model.window(); start != 23 || end != 38 {
		t.Fatalf("window = %d..%d", start, end)
	}
	next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if start, _ := next.(selectModel).window(); start != 22 {
		t.Fatalf("window after up starts at %d", start)
	}
	short := newSelectModel("history.prompt.metric_title", "history.prompt.metric_label", items[:3])
	if short.frameHeight() != 4 || strings.Contains(short.View().Content, "/3") {
		t.Fatalf("a short list must not change: %q", short.View().Content)
	}
}
