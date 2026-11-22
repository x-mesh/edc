package edc

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func recordingSummarySample(second int64, cpu, memory float64, pid int, rss *uint64) historyTopSample {
	sample := historyTopSample{topSample: topSample{Time: time.Unix(second, 0), CPUUser: cpu, MemoryPct: memory}, CPU: cpu, WindowS: 1}
	if pid > 0 {
		processes := []topProcessSample{{PID: pid, Command: "worker", RSSBytes: rss}}
		sample.Processes, sample.ProcessesValid = &processes, true
	}
	return sample
}

func recordingSummaryRecorder(t *testing.T, path, trigger string, samples []historyTopSample) *topRecorder {
	t.Helper()
	recorder, err := newTriggeredTopRecorder(path, "test", hostDetails{Hostname: "fixture", Cores: 4}, time.Second, "", "", false, trigger, samples)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close("finished") })
	return recorder
}

func TestTopRecordingSummaryObservesFiniteValuesAndAvailableRSS(t *testing.T) {
	missing := recordingSummarySample(10, math.NaN(), math.Inf(1), 7, nil)
	summary := topRecordingSummary{}.observe(missing)
	if summary.cpuKnown || summary.memoryKnown || summary.processKnown {
		t.Fatalf("missing data became known: %+v", summary)
	}
	zero := uint64(0)
	sample := recordingSummarySample(20, 0, 0, 7, &zero)
	summary = summary.observe(sample)
	if !summary.cpuKnown || !summary.memoryKnown || !summary.processKnown || summary.processRSS != 0 {
		t.Fatalf("observed zero lost: %+v", summary)
	}
	big := uint64(3 << 30)
	sample = recordingSummarySample(30, 90, 85, 8, &big)
	(*sample.Processes)[0].Command = "worker\x1b[2J\n" + strings.Repeat("한글", 100)
	summary = summary.observe(sample).observe(recordingSummarySample(25, 50, 40, 9, nil))
	if summary.memoryFirst != 0 || summary.memoryPeak != 85 || summary.memoryLast != 85 || summary.cpuPeak != 90 || summary.processPID != 8 || summary.samples != 4 {
		t.Fatalf("statistics: %+v", summary)
	}
	if strings.ContainsAny(summary.processCommand, "\x1b\n") || liveWidth(summary.processCommand) > topRecordingSummaryNameWidth {
		t.Fatalf("unsafe process label: %q", summary.processCommand)
	}
	notObserved := recordingSummarySample(40, 1, 20, 10, &big)
	notObserved.ProcessesValid = false
	summary = summary.observe(notObserved)
	if summary.processPID != 8 {
		t.Fatal("unreadable process sample replaced observed RSS")
	}
}

func TestTopRecordingSummaryBacklogAndQueuedSamplesMatchCommittedRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	small, big := uint64(1<<30), uint64(3<<30)
	backlog := []historyTopSample{recordingSummarySample(100, 10, 40, 7, &small), recordingSummarySample(101, 50, 70, 8, &big)}
	recorder := recordingSummaryRecorder(t, path, "memory pressure warn", backlog)
	if err := recorder.Record(recordingSummarySample(102, 25, 60, 7, &small)); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	summary := recorder.summary
	if summary.samples != 3 || summary.started.Unix() != 99 || summary.first.Unix() != 100 || summary.last.Unix() != 102 || summary.memoryFirst != 40 || summary.memoryPeak != 70 || summary.memoryLast != 60 || summary.cpuPeak != 50 || summary.processPID != 8 {
		t.Fatalf("summary: %+v", summary)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT sample_count FROM runs WHERE id=?", recorder.runID).Scan(&count); err != nil || count != summary.samples {
		t.Fatalf("saved=%d summary=%d err=%v", count, summary.samples, err)
	}
	if err := recorder.Close("error"); err != nil || !reflect.DeepEqual(summary, recorder.summary) {
		t.Fatal("duplicate close changed summary")
	}
}

func TestTopRecordingSummaryDoesNotCountRolledBackTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := recordingSummaryRecorder(t, path, "load spike", nil)
	if _, err := recorder.db.Exec("CREATE TRIGGER reject_summary BEFORE INSERT ON top_samples WHEN NEW.memory_pct>90 BEGIN SELECT RAISE(ABORT, 'rejected sample'); END"); err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for _, sample := range []historyTopSample{recordingSummarySample(10, 10, 40, 0, nil), recordingSummarySample(11, 80, 95, 0, nil)} {
		payload, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	if err := recorder.writeSamples(payloads); err == nil {
		t.Fatal("transaction should fail")
	}
	var rows int
	if err := recorder.db.QueryRow("SELECT count(*) FROM top_samples").Scan(&rows); err != nil || rows != 0 || recorder.summary.samples != 0 {
		t.Fatalf("rollback rows=%d summary=%d err=%v", rows, recorder.summary.samples, err)
	}
}

func TestTopRecordingSummaryReportsPartialWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := recordingSummaryRecorder(t, path, "load spike", nil)
	if _, err := recorder.db.Exec("CREATE TRIGGER reject_summary BEFORE INSERT ON top_samples WHEN NEW.memory_pct>90 BEGIN SELECT RAISE(ABORT, 'rejected sample'); END"); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []historyTopSample{recordingSummarySample(10, 10, 40, 0, nil), recordingSummarySample(11, 80, 95, 0, nil)} {
		if err := recorder.Record(sample); err != nil {
			t.Fatal(err)
		}
	}
	model := topFixtureModel(nil)
	model.attachRecorder(recorder)
	if err := model.finishRecording("finished"); err == nil {
		t.Fatal("failed writer reported success")
	}
	if model.recordingSummary == nil || model.recordingSummary.samples != 1 || model.recordingSummary.status != "error" || model.recordingsFinished != 1 {
		t.Fatalf("partial: %+v", model.recordingSummary)
	}
	var output strings.Builder
	printTopRecordingSummary(&output, model.recordingSummary, model.recordingsFinished, 80, false)
	if strings.Contains(output.String(), T("history.recording.summary.saved", recorder.runID)) || !strings.Contains(output.String(), "rejected sample") {
		t.Fatalf("failure mislabeled:\n%s", output.String())
	}
	if err := model.finishRecording("finished"); err != nil || model.recordingsFinished != 1 {
		t.Fatal("repeat finalization counted a run twice")
	}
}

func TestTopRecordingSummaryRetainsOnlyLatestAutomaticRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	model := topFixtureModel(nil)
	first := recordingSummaryRecorder(t, path, "first trigger", []historyTopSample{recordingSummarySample(10, 20, 50, 0, nil)})
	model.attachRecorder(first)
	if err := model.finishRecording("finished"); err != nil {
		t.Fatal(err)
	}
	second := recordingSummaryRecorder(t, path, "second trigger", []historyTopSample{recordingSummarySample(20, 70, 80, 0, nil)})
	model.attachRecorder(second)
	if err := model.finishRecording("finished"); err != nil {
		t.Fatal(err)
	}
	if model.recordingsFinished != 2 || model.recordingSummary.runID != second.runID || model.recordingSummary.cpuPeak != 70 || model.recordingSummary.samples != 1 {
		t.Fatalf("latest run: %+v", model.recordingSummary)
	}
	var output strings.Builder
	printTopRecordingSummary(&output, model.recordingSummary, model.recordingsFinished, 80, false)
	if !strings.Contains(strings.Join(strings.Fields(output.String()), " "), T("history.recording.summary.latest", 2)) || strings.Contains(output.String(), first.runID) {
		t.Fatalf("latest report:\n%s", output.String())
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs int
	if err := db.QueryRow("SELECT count(*) FROM runs").Scan(&runs); err != nil || runs != 2 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
	manual := recordingSummaryRecorder(t, path, "", nil)
	model.attachRecorder(manual)
	next, cmd := model.stopRecording()
	model = next.(topModel)
	if cmd != nil || model.notice != T("history.recording.stopped", model.recordCount) || model.recordingsFinished != 2 || model.recordingSummary.runID != second.runID {
		t.Fatal("manual stop replaced the automatic summary")
	}
}

func TestTopRecordingSummaryManualOnlyHasNoAutomaticReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := recordingSummaryRecorder(t, path, "", nil)
	model := topFixtureModel(nil)
	model.attachRecorder(recorder)
	if err := model.finishRecording("finished"); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	printTopRecordingSummary(&output, model.recordingSummary, model.recordingsFinished, 80, false)
	if model.recordingSummary != nil || model.recordingsFinished != 0 || output.Len() != 0 {
		t.Fatal("manual-only recording gained automatic output")
	}
}

func TestTopRecordingSummaryWStopMutesAutomaticRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := recordingSummaryRecorder(t, path, "load spike", []historyTopSample{recordingSummarySample(10, 60, 70, 0, nil)})
	model := topFixtureModel(nil)
	model.autoRecord = &topAutoRecord{trigger: "load spike"}
	model.attachRecorder(recorder)
	next, cmd := model.toggleRecording()
	model = next.(topModel)
	if cmd != nil || !model.autoRecord.muted || model.autoRecord.trigger != "" || model.recorder != nil || model.recordingSummary == nil || !strings.Contains(model.notice, recorder.runID) {
		t.Fatal("manual automatic-stop contract lost")
	}
}

func TestTopRecordingSummaryQuitAndCtrlCFinalization(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		t.Run(key.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			recorder := recordingSummaryRecorder(t, path, "load spike", []historyTopSample{recordingSummarySample(10, 60, 70, 0, nil)})
			model := topFixtureModel(func() (resourceSnapshot, error) { return topSampleAt(11), nil })
			model.attachRecorder(recorder)
			program := tea.NewProgram(model, headlessOptions()...)
			done := make(chan struct{})
			var final tea.Model
			var runErr error
			go func() { defer close(done); final, runErr = program.Run() }()
			program.Send(key)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				program.Kill()
				t.Fatal("quit timed out")
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			finished := final.(topModel)
			if err := finished.finishRecording("finished"); err != nil || finished.recorder != nil || finished.recordingSummary == nil || finished.recordingSummary.samples != 1 {
				t.Fatalf("finalization: %+v %v", finished.recordingSummary, err)
			}
		})
	}
}

func TestTopRecordingSummaryCloseFailureIsNotSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := recordingSummaryRecorder(t, path, "load spike", nil)
	if err := recorder.db.Close(); err != nil {
		t.Fatal(err)
	}
	model := topFixtureModel(nil)
	model.attachRecorder(recorder)
	if err := model.finishRecording("finished"); err == nil || model.recordingSummary == nil || model.recordingSummary.status != "error" {
		t.Fatal("DB close failure appeared successful")
	}
}

func TestTopRecordingSummaryRenderAndReviewQuoting(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	rss := uint64(3 << 30)
	sample := recordingSummarySample(100, 72, 85, 42, &rss)
	(*sample.Processes)[0].Command = "worker\x1b[2J"
	summary := (topRecordingSummary{runID: "test-run", path: "/tmp/a dir/it's.db", trigger: "memory pressure warn", started: time.Unix(99, 0), status: "finished"}).observe(sample)
	for _, width := range []int{48, 80, 140} {
		for _, color := range []bool{false, true} {
			var output strings.Builder
			printTopRecordingSummary(&output, &summary, 1, width, color)
			stripped := ansi.Strip(output.String())
			compact := strings.Join(strings.Fields(stripped), " ")
			for _, want := range []string{"test-run", "72.00%", "85.00%", "PID 42", "3.00 GB", "memory pressure warn"} {
				if !strings.Contains(compact, want) {
					t.Fatalf("missing %q: %s", want, stripped)
				}
			}
			for _, line := range strings.Split(stripped, "\n") {
				if !strings.HasPrefix(line, "edc history overview ") && liveWidth(line) > width {
					t.Fatalf("width %d line %q", width, line)
				}
			}
			if !color && strings.Contains(output.String(), "\x1b") {
				t.Fatal("plain report emitted control sequences")
			}
			if !strings.Contains(output.String(), topRecordingReviewCommand(summary)) {
				t.Fatal("review command was hard-wrapped")
			}
		}
	}
	for _, path := range []string{"/tmp/a dir/it's.db", "/tmp/$()'; echo bad; '/history.db", "/tmp/control\x1b\n.db"} {
		summary.path = path
		command := topRecordingReviewCommand(summary)
		if strings.Contains(command, "\x1b") {
			t.Fatal("review command emitted raw escape")
		}
		output, err := exec.CommandContext(context.Background(), "bash", "-c", "set -- "+command+"; printf '%s\\0' \"$@\"").Output()
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
		if len(args) != 6 || args[4] != summary.runID || args[5] != path {
			t.Fatalf("quoted args=%q", args)
		}
	}
}

func TestTopRecordingSummaryUnavailableSamplesAndExternalFailure(t *testing.T) {
	summary := topRecordingSummary{runID: "test-run", path: "/tmp/history.db", trigger: "load spike", status: "error", failure: errors.New("interrupted").Error()}
	var output strings.Builder
	printTopRecordingSummary(&output, &summary, 1, 80, false)
	for _, want := range []string{T("history.recording.summary.failed", summary.runID), T("history.recording.summary.no_samples"), T("history.recording.summary.process_unavailable"), "interrupted"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
}
