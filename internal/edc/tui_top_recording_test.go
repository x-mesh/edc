package edc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestTopRecordingShortcutStartsStopsAndRestarts(t *testing.T) {
	model := topFixtureModel(nil)
	model.recordPath = filepath.Join(t.TempDir(), "history.db")
	model.previous.TakenAt = time.Now().Add(-time.Second)
	model = topAfter(t, model, topKey("w"))
	first := model.recorder
	if first == nil || model.record == nil || model.recordFailure == nil {
		t.Fatal("w did not start recording")
	}
	t.Cleanup(func() { first.Close("finished") })
	start := model.recordFrom
	sample := topSampleAt(1)
	sample.TakenAt = start.Add(time.Second)
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
	if model.recordCount != 0 {
		t.Fatal("recorded a partial window that started before w")
	}
	sample = topSampleAt(2)
	sample.TakenAt = start.Add(2 * time.Second)
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
	if model.recordCount != 1 || !strings.Contains(strings.Join(model.statusLines(), "\n"), "REC") {
		t.Fatal("full sample or recording indicator missing")
	}
	model = topAfter(t, model, topKey("p"))
	sample.TakenAt = start.Add(20 * time.Second)
	model = topAfter(t, model, topSampleMsg{seq: model.seq - 1, snapshot: sample}, topKey("p"))
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
	if model.recordCount != 1 {
		t.Fatal("paused samples or the resume baseline were recorded")
	}
	sample.TakenAt = start.Add(21 * time.Second)
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample}, topKey("w"))
	if model.recorder != nil || model.record != nil || model.recordCount != 2 {
		t.Fatal("w did not stop recording")
	}
	db, err := openHistoryDB(model.recordPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := loadHistoryTop(db, historyOptions{})
	if err != nil || len(rows) != 2 || rows[0].WindowS != 1 {
		t.Fatalf("persisted samples=%+v err=%v", rows, err)
	}
	model = topAfter(t, model, topKey("w"))
	second := model.recorder
	if second == nil || second.runID == first.runID || model.recordCount != 0 {
		t.Fatal("restart did not create a fresh run")
	}
	t.Cleanup(func() { second.Close("finished") })
	next, cmd := model.Update(topRecordingErrorMsg{source: first, err: errors.New("old failure")})
	model = next.(topModel)
	if cmd != nil || model.recordingErr != nil || model.recorder != second {
		t.Fatal("a stopped recorder's failure affected the new run")
	}
	model = topAfter(t, model, topKey("w"))
	var runs int
	if err := db.QueryRow("SELECT count(*) FROM runs").Scan(&runs); err != nil || runs != 2 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func TestTopRecordingShortcutFailureKeepsDashboardOpen(t *testing.T) {
	model := topFixtureModel(nil)
	model.recordPath = filepath.Join(t.TempDir(), "missing", "history.db")
	next, cmd := model.Update(topKey("w"))
	model = next.(topModel)
	if cmd != nil || model.recorder != nil || model.record != nil || model.notice == "" {
		t.Fatal("failed recording startup should show an error without closing the dashboard")
	}
}

func TestTopDashboardTitleShowsRecordingBadge(t *testing.T) {
	model := topFixtureModel(nil)
	model.details = hostDetails{Hostname: "host", Cores: 8}
	model.width = 120
	if title := ansi.Strip(model.dashboardTitle()); strings.Contains(title, "REC") {
		t.Fatalf("idle title shows a recording badge: %q", title)
	}
	model.record = func(historyTopSample) error { return nil }
	steps := []struct {
		color, paused bool
		want, badge   string
	}{
		{false, false, "view all · [● REC] · live 🐰", ""},
		{false, true, "view all · [❚❚ REC] · live 🐰", ""},
		{true, false, "view all ·  ● REC  · live 🐰", richBadge("● REC", richRed)},
		{true, true, "view all ·  ❚❚ REC  · live 🐰", richBadge("❚❚ REC", richYellow)},
	}
	for _, step := range steps {
		model.limits.color, model.paused = step.color, step.paused
		raw := model.dashboardTitle()
		title := ansi.Strip(raw)
		if !strings.HasSuffix(title, step.want) || topDisplayWidth(title) != model.width || !strings.Contains(raw, step.badge) {
			t.Fatalf("color=%v paused=%v title = %q", step.color, step.paused, raw)
		}
	}
	model.width = 60
	model.limits.color, model.paused = false, false
	if title := ansi.Strip(model.dashboardTitle()); !strings.Contains(title, "[● REC] · live") {
		t.Fatalf("narrow title = %q", title)
	}
}
