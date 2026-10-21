package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	var plain bytes.Buffer
	writeHistoryOverview(&plain, view, time.UTC)
	for _, want := range []string{T("history.overview.run"), T("history.overview.metrics"), T("history.overview.processes", T("history.overview.memory")), "worker"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("missing %q: %s", want, plain.String())
		}
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
	view, err := loadHistoryOverview(db, historyOverviewOptions{})
	if err != nil || view.Runs != 1 {
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
	if err != nil || !bytes.Contains(data, []byte("\"metrics\"")) || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("json = %q err %v", data, err)
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
