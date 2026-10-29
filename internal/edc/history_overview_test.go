package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestHistoryOverviewAggregatesRunAndPlainOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	for index, cpu := range []float64{10, 20, 30} {
		sample := historyFixtureSample(index+1, cpu, topProcess{PID: 7, Started: time.Unix(1, 0), Command: "worker", CPU: cpu, RSS: uint64(index+1) << 20})
		if err := recorder.Record(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	view, err := loadHistoryOverview(db, historyOverviewOptions{run: recorder.runID, from: from, to: from.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if view.Runs != 1 || view.Samples != 3 || len(view.Metrics) == 0 || len(view.Processes) != 1 || view.Processes[0].Peak != 3<<20 || view.Processes[0].Restarts != 0 {
		t.Fatalf("overview = %+v", view)
	}
	if metric := view.Metrics[0]; metric.Metric != "cpu_pct" || metric.Avg != 20 || metric.P95 != 30 || metric.Max != 30 {
		t.Fatalf("cpu metric = %+v", metric)
	}
	var plain bytes.Buffer
	writeHistoryOverview(&plain, view, time.UTC)
	for _, want := range []string{T("history.overview.run"), T("history.overview.metrics"), T("history.overview.processes", T("history.overview.memory")), "worker"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("missing %q: %s", want, plain.String())
		}
	}
}

func TestHistoryOverviewNearestRankP95(t *testing.T) {
	for count, want := range map[int]float64{1: 1, 2: 2, 20: 19, 21: 20} {
		t.Run(fmt.Sprintf("n=%d", count), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			recorder := topHistoryFixture(t, path)
			for index := count - 1; index >= 0; index-- {
				cpu := float64(index + 1)
				sample := historyFixtureSample(count-index, cpu, topProcess{PID: 1, Command: "worker", CPU: cpu, RSS: 1})
				sample.RunID = recorder.runID
				payload, err := json.Marshal(sample)
				if err != nil {
					t.Fatal(err)
				}
				// Record queues only historyQueueSize samples; write directly so a slow CI writer cannot overflow it.
				if err := recorder.writeSamples([][]byte{payload}); err != nil {
					t.Fatal(err)
				}
			}
			if err := recorder.Close("finished"); err != nil {
				t.Fatal(err)
			}
			db, err := openHistoryDB(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			view, err := loadHistoryOverview(db, historyOverviewOptions{run: recorder.runID})
			if err != nil {
				t.Fatal(err)
			}
			for _, metric := range view.Metrics {
				if metric.Metric == "cpu_pct" {
					if metric.P95 != want {
						t.Fatalf("p95 = %v, want %v", metric.P95, want)
					}
					return
				}
			}
			t.Fatal("cpu_pct metric missing")
		})
	}
}

func TestHistoryOverviewUsesStoredRunDuration(t *testing.T) {
	started := time.Unix(10, 0).UTC()
	ended := started.Add(2 * time.Hour)
	var output bytes.Buffer
	writeHistoryOverview(&output, historyOverview{Run: &historyRun{ID: "run", Command: "top", Hostname: "host", StartedAt: started, EndedAt: &ended, IntervalS: 1}, From: started, To: started.Add(time.Minute)}, time.UTC)
	if !strings.Contains(output.String(), "2h00m") {
		t.Fatalf("duration output: %s", output.String())
	}
}

func TestHistoryOverviewLabelsDoNotExposeTranslationKeys(t *testing.T) {
	for _, language := range []string{"en", "ko", "ja"} {
		previous := currentLanguage()
		setLanguage(language)
		if got := historyOverviewMemoryLabel("RSS"); strings.Contains(got, "history.") {
			t.Fatalf("%s memory label = %q", language, got)
		}
		if got := historyOverviewMetricLabel("cpu_pct"); strings.Contains(got, "history.") {
			t.Fatalf("%s metric label = %q", language, got)
		}
		setLanguage(previous)
	}
}

func TestHistoryOverviewMatchesLogsByStartTimeOnly(t *testing.T) {
	previous := historyOverviewLogLoader
	t.Cleanup(func() { historyOverviewLogLoader = previous })
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	historyOverviewLogLoader = func() (logHistorySnapshot, error) {
		return logHistorySnapshot{Rows: []logHistoryAttempt{
			{Command: []string{"make", "test"}, Started: from.Add(time.Second), Outcome: historyOutcomeSuccess, Duration: time.Second},
			{Command: []string{"outside"}, Started: from.Add(time.Minute), Outcome: historyOutcomeFail, Duration: time.Second},
		}}, nil
	}
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
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
	view, err := loadHistoryOverview(db, historyOverviewOptions{run: recorder.runID, from: from, to: from.Add(10 * time.Second)})
	if err != nil || len(view.Logs) != 1 || view.Logs[0].Command != "make test" || view.Logs[0].Outcome != historyOutcome(logHistoryAttempt{Outcome: historyOutcomeSuccess}) {
		t.Fatalf("logs %+v err %v", view.Logs, err)
	}
}

func TestHistoryOverviewRelatedTablesFitPlainWidth(t *testing.T) {
	var output bytes.Buffer
	view := historyOverview{
		From: time.Unix(0, 0), To: time.Unix(1, 0),
		Disks: []historyOverviewDisk{{Mount: strings.Repeat("/long/path", 20), UsedBytes: 1, TotalBytes: 2}},
		Logs:  []historyOverviewLog{{Command: strings.Repeat("very long command ", 20), Started: time.Unix(0, 0), Outcome: historyOutcome(logHistoryAttempt{Outcome: historyOutcomeSuccess}), Duration: 11997 * time.Microsecond}},
	}
	writeHistoryOverview(&output, view, time.UTC)
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		if width := ansi.StringWidth(line); width > historyOverviewPlainWidth {
			t.Fatalf("width %d: %q", width, line)
		}
	}
	if !strings.Contains(output.String(), "12ms") {
		t.Fatalf("duration output: %s", output.String())
	}
}

func TestHistoryOverviewLoadsSnapshotDisks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec("INSERT INTO runs(id,command,hostname,started_ns,status,payload) VALUES ('snapshot','snapshot','host',?,'finished','{}')", from.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO snapshot_metrics(run_id,time_ns,probe,key,value) VALUES ('snapshot',?,?,?,?),( 'snapshot',?,?,?,?)", from.UnixNano(), "info.disk:/", "used_bytes", 10.0, from.UnixNano(), "info.disk:/", "total_bytes", 100.0); err != nil {
		t.Fatal(err)
	}
	view, err := loadHistoryOverview(db, historyOverviewOptions{from: from, to: from.Add(time.Minute)})
	if err != nil || len(view.Disks) != 1 || view.Disks[0].Mount != "/" || view.Disks[0].UsedBytes != 10 {
		t.Fatalf("disks %+v err %v", view.Disks, err)
	}
}

func TestHistoryOverviewWindowAndMissingRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
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
	if _, err := loadHistoryOverview(db, historyOverviewOptions{run: "missing"}); err == nil || !strings.Contains(err.Error(), "No matching") {
		t.Fatalf("missing = %v", err)
	}
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	view, err := loadHistoryOverview(db, historyOverviewOptions{from: from, to: from.Add(time.Minute)})
	if err != nil || view.Runs != 1 || view.Samples != 1 {
		t.Fatalf("window = %+v, %v", view, err)
	}
}

func TestHistoryOverviewWritesOneJSONObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "overview.json")
	if code := runHistory([]string{"overview", "--run", recorder.runID, "--json", output, path}); code != 0 {
		t.Fatalf("overview code = %d", code)
	}
	data, err := os.ReadFile(output)
	var decoded historyOverview
	if err != nil || json.Unmarshal(data, &decoded) != nil || decoded.Run == nil || decoded.Runs != 1 || decoded.Samples != 1 || decoded.Metrics == nil || decoded.Processes == nil || decoded.Disks == nil || decoded.Logs == nil {
		t.Fatalf("json = %q err %v", data, err)
	}
}

func TestHistoryOverviewRejectsBadCLIInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"overview", "--unknown", path},
		{"overview", path, path},
		{"overview", "--from", "bad", path},
		{"overview", "--from", "2026-10-06T00:00:00Z", "--to", "2026-10-05T00:00:00Z", path},
	} {
		if code := runHistory(args); code != 2 {
			t.Fatalf("%v code=%d", args, code)
		}
	}
}

func TestHistoryOverviewFallsBackToRSSOnOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(oldHistoryV1Statements(), fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID), "PRAGMA user_version=1") {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	run := historyRun{ID: "old", Command: "top", Hostname: "host", StartedAt: started, IntervalS: 1}
	payload, _ := json.Marshal(run)
	if _, err := db.Exec("INSERT INTO runs(id,command,hostname,started_ns,ended_ns,status,sample_count,payload) VALUES (?,?,?,?,?,?,?,?)", run.ID, run.Command, run.Hostname, started.UnixNano(), started.Add(time.Second).UnixNano(), "finished", 1, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO top_samples(id,run_id,time_ns,payload,cpu_pct) VALUES (1,?,?,?,1)", run.ID, started.UnixNano(), "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO top_process_samples(sample_id,run_id,time_ns,command,command_folded,payload,pid,rss_bytes,cpu_pct) VALUES (1,?,?,?,?,?,?,?,?)", run.ID, started.UnixNano(), "rss", "rss", "{}", 1, 42, 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	reader, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	view, err := loadHistoryOverview(reader, historyOverviewOptions{run: run.ID})
	if err != nil || view.Memory != "RSS" || len(view.Processes) != 1 || view.Processes[0].Peak != 42 {
		t.Fatalf("overview %+v err %v", view, err)
	}
}

func TestHistoryOverviewJSONStdoutIsOneObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	var code int
	text := traceCaptureOutput(t, &os.Stdout, func() { code = runHistory([]string{"overview", "--run", recorder.runID, "--json", "-", path}) })
	var decoded historyOverview
	if code != 0 || json.Unmarshal([]byte(text), &decoded) != nil || decoded.Run == nil || strings.Count(text, "\n") != 1 {
		t.Fatalf("code %d json %q", code, text)
	}
}

func TestHistoryOverviewJSONChmodFailureKeepsTarget(t *testing.T) {
	previous := historyOverviewTempChmod
	t.Cleanup(func() { historyOverviewTempChmod = previous })
	historyOverviewTempChmod = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "overview.json")
	if err := os.WriteFile(target, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := runHistory([]string{"overview", "--run", recorder.runID, "--json", target, path}); code == 0 {
		t.Fatal("chmod failure succeeded")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "existing" {
		t.Fatalf("target %q err %v", got, err)
	}
	if files, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".edc-overview-*")); err != nil || len(files) != 0 {
		t.Fatalf("temp files %v err %v", files, err)
	}
}

func TestHistoryOverviewWarningsKeepCoreData(t *testing.T) {
	previous := historyOverviewLogLoader
	t.Cleanup(func() { historyOverviewLogLoader = previous })
	historyOverviewLogLoader = func() (logHistorySnapshot, error) { return logHistorySnapshot{}, errors.New("log unavailable") }
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Record(historyFixtureSample(1, 10, topProcess{PID: 1, Command: "one", RSS: 1})); err != nil {
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
	view, err := loadHistoryOverview(db, historyOverviewOptions{run: recorder.runID})
	if err != nil || len(view.Metrics) == 0 || len(view.Processes) == 0 || !strings.Contains(strings.Join(view.Warnings, " "), "log unavailable") {
		t.Fatalf("view %+v err %v", view, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(encoded), `"warnings":["`) || strings.Contains(string(encoded), `"Warnings"`) {
		t.Fatalf("json warnings key: %s", encoded)
	}
	var plain bytes.Buffer
	writeHistoryOverview(&plain, view, time.UTC)
	if !strings.Contains(plain.String(), "log unavailable") {
		t.Fatalf("plain %q", plain.String())
	}
}

func TestHistoryOverviewRunWindowIntersection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	run := historyRun{ID: "run", Command: "top", Hostname: "host", StartedAt: from}
	payload, _ := json.Marshal(run)
	if _, err := db.Exec("INSERT INTO runs(id,command,hostname,started_ns,ended_ns,status,payload) VALUES (?,?,?,?,?,?,?)", run.ID, run.Command, run.Hostname, from.UnixNano(), from.Add(10*time.Minute).UnixNano(), "finished", payload); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		at := from.Add(time.Duration(index*5) * time.Minute)
		if _, err := db.Exec("INSERT INTO top_samples(run_id,time_ns,payload,cpu_pct) VALUES (?,?,?,?)", run.ID, at.UnixNano(), "{}", index+1); err != nil {
			t.Fatal(err)
		}
	}
	windowFrom, windowTo := from.Add(4*time.Minute), from.Add(6*time.Minute)
	view, err := loadHistoryOverview(db, historyOverviewOptions{run: run.ID, from: windowFrom, to: windowTo})
	if err != nil || !view.From.Equal(windowFrom) || !view.To.Equal(windowTo) || view.Runs != 1 || view.Samples != 1 {
		t.Fatalf("view %+v err %v", view, err)
	}
	var plain bytes.Buffer
	writeHistoryOverview(&plain, view, time.UTC)
	if !strings.Contains(plain.String(), windowFrom.Format(time.DateTime)) || !strings.Contains(plain.String(), "samples 1") {
		t.Fatalf("plain %q", plain.String())
	}
	encoded, err := json.Marshal(view)
	var decoded historyOverview
	if err != nil || json.Unmarshal(encoded, &decoded) != nil || !decoded.From.Equal(windowFrom) || !decoded.To.Equal(windowTo) || decoded.Runs != 1 || decoded.Samples != 1 {
		t.Fatalf("json %s err %v", encoded, err)
	}
}

func TestHistoryOverviewRejectsDisjointRunWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	run := historyRun{ID: "run", Command: "top", Hostname: "host", StartedAt: from}
	payload, _ := json.Marshal(run)
	if _, err := db.Exec("INSERT INTO runs(id,command,hostname,started_ns,status,payload) VALUES (?,?,?,?,?,?)", run.ID, run.Command, run.Hostname, from.UnixNano(), "finished", payload); err != nil {
		t.Fatal(err)
	}
	for _, minute := range []int{0, 5} {
		if _, err := db.Exec("INSERT INTO top_samples(run_id,time_ns,payload,cpu_pct) VALUES (?,?,?,?)", run.ID, from.Add(time.Duration(minute)*time.Minute).UnixNano(), "{}", 1); err != nil {
			t.Fatal(err)
		}
	}
	spanEnd := from.Add(5*time.Minute + time.Nanosecond)
	for _, window := range []historyOverviewOptions{
		{run: run.ID, from: from.Add(-time.Minute), to: from},
		{run: run.ID, from: spanEnd, to: spanEnd.Add(time.Minute)},
	} {
		if _, err := loadHistoryOverview(db, window); !errors.Is(err, errHistoryOverviewDisjoint) || !strings.Contains(err.Error(), "Run samples") {
			t.Fatalf("window %+v err %v", window, err)
		}
	}
}
