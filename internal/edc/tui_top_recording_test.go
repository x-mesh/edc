package edc

import (
	"encoding/json"
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

func TestTopAutoRecordKeepsBacklogAndStopsAfterCalmTail(t *testing.T) {
	model := topFixtureModel(nil)
	model.recordPath = filepath.Join(t.TempDir(), "history.db")
	model.autoRecord = &topAutoRecord{}
	feed := func(from, to int, load float64) {
		for second := from; second <= to; second++ {
			sample := topSampleAt(second)
			sample.Load1 = load
			model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
		}
	}
	feed(1, 400, 1)
	if model.recorder != nil {
		t.Fatal("calm samples started a recording")
	}
	// fixture host는 8코어라 load 20은 2×8을 넘는다.
	feed(401, 401, 20)
	recorder := model.recorder
	if recorder == nil || model.autoRecord.trigger != "load 20 > 2×8" {
		t.Fatalf("load spike did not start an auto recording: trigger=%q", model.autoRecord.trigger)
	}
	t.Cleanup(func() { recorder.Close("finished") })
	// Record의 queue는 historyQueueSize칸이다. 시험은 sample을 기다리지 않고 넣으므로 backlog를 다 쓸 때까지 기다린다.
	waitHistorySampleCount(t, model.recordPath, 301)
	feed(402, 403, 20)
	if line := strings.Join(model.statusLines(), "\n"); !strings.Contains(line, "auto load 20 > 2×8 · REC") {
		t.Fatalf("status lines do not show the trigger: %q", line)
	}
	for second := 404; second < 464; second += historyQueueSize / 2 {
		feed(second, min(second+historyQueueSize/2-1, 463), 1)
		waitHistorySampleCount(t, model.recordPath, 301+min(second+historyQueueSize/2-1, 463)-401)
	}
	if model.recorder == nil {
		t.Fatal("recording stopped before the calm tail ended")
	}
	feed(464, 464, 1)
	if model.recorder != nil || model.autoRecord.trigger != "" {
		t.Fatalf("recording did not stop after the calm tail: calm=%v trigger=%q rec=%v", model.autoRecord.calmSince, model.autoRecord.trigger, model.recorder != nil)
	}
	db, err := openHistoryDB(model.recordPath, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload string
	var started int64
	var count int
	if err := db.QueryRow("SELECT payload, started_ns, sample_count FROM runs").Scan(&payload, &started, &count); err != nil {
		t.Fatal(err)
	}
	// 5분 버퍼(101~401초)에 이후 기록(402~464초)을 더한다. run은 첫 sample의 구간 시작인 100초에 시작한다.
	var run historyRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		t.Fatal(err)
	}
	if count != 301+63 || started != time.Unix(100, 0).UnixNano() || run.Trigger != "load 20 > 2×8" {
		t.Fatalf("run count=%d started=%d payload=%s", count, started, payload)
	}
	rows, err := loadHistoryTop(db, historyOptions{})
	if err != nil || len(rows) != count {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	first := rows[0].Time
	for _, row := range rows {
		if row.Time.Before(first) {
			first = row.Time
		}
	}
	if !first.Equal(time.Unix(101, 0)) {
		t.Fatalf("first saved sample is at %v", first)
	}
}

func TestTopAutoRecordStaysOutOfManualRecording(t *testing.T) {
	model := topFixtureModel(nil)
	model.recordPath = filepath.Join(t.TempDir(), "history.db")
	model.autoRecord = &topAutoRecord{}
	model.previous.TakenAt = time.Now().Add(-time.Second)
	model = topAfter(t, model, topKey("w"))
	manual := model.recorder
	t.Cleanup(func() { manual.Close("finished") })
	sample := topSampleAt(1)
	sample.TakenAt, sample.Load1 = model.recordFrom.Add(time.Second), 20
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
	if model.recorder != manual || model.autoRecord.trigger != "" || len(model.autoRecord.samples) != 0 {
		t.Fatal("auto recording replaced or buffered during a manual recording")
	}
}

func waitHistorySampleCount(t *testing.T, path string, want int) {
	t.Helper()
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := db.QueryRow("SELECT sample_count FROM runs ORDER BY started_ns DESC LIMIT 1").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("history has %d samples, want %d", count, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTopAutoRecordStaysStoppedAfterWUntilTheConditionClears(t *testing.T) {
	model := topFixtureModel(nil)
	model.recordPath = filepath.Join(t.TempDir(), "history.db")
	model.autoRecord = &topAutoRecord{}
	feed := func(second int, load float64) {
		sample := topSampleAt(second)
		sample.Load1 = load
		model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: sample})
	}
	feed(1, 20)
	if model.recorder == nil {
		t.Fatal("load spike did not start an auto recording")
	}
	model = topAfter(t, model, topKey("w"))
	for second := 2; second <= 5; second++ {
		feed(second, 20)
	}
	if model.recorder != nil {
		t.Fatal("auto recording reopened while the user stopped it")
	}
	feed(6, 1)
	feed(7, 20)
	recorder := model.recorder
	if recorder == nil {
		t.Fatal("auto recording did not start again after the condition cleared")
	}
	t.Cleanup(func() { recorder.Close("finished") })
}

func TestTopAutoRecordStatusKeepsHelpAt80Columns(t *testing.T) {
	model := topFixtureModel(nil)
	model.autoRecord = &topAutoRecord{trigger: "load 131 > 2×16"}
	model.record = func(historyTopSample) error { return nil }
	model.recordCount = 12345
	lines := model.statusLines()
	for _, line := range lines {
		if width := topDisplayWidth(ansi.Strip(line)); width > topTableWidth {
			t.Fatalf("status line is %d wide: %q", width, line)
		}
	}
	if joined := ansi.Strip(strings.Join(lines, "\n")); !strings.Contains(joined, "load 131 > 2×16") || !strings.Contains(joined, "? help") {
		t.Fatalf("status lines = %q", joined)
	}
}
