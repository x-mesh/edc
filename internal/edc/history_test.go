package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func topHistoryFixture(t *testing.T, path string) *topRecorder {
	t.Helper()
	recorder, err := newTopRecorder(path, "test", hostDetails{Hostname: "fixture", Cores: 8, MemoryTotal: 1024}, time.Second, "", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close("finished") })
	return recorder
}

func historyFixtureSample(seconds int, cpu float64, process topProcess) historyTopSample {
	previous := resourceSnapshot{TakenAt: time.Date(2026, 10, 5, 0, 0, seconds-1, 0, time.UTC)}
	current := resourceSnapshot{TakenAt: previous.TakenAt.Add(time.Second), ProcessesValid: true, ProcessesAt: previous.TakenAt.Add(500 * time.Millisecond), Processes: []topProcess{process}}
	return newHistoryTopSample(hostDetails{Hostname: "fixture", Cores: 8}, previous, current, resourceRate{CPUUser: cpu - 10, CPUSystem: 10, MemoryPercent: 92, CoreCPU: []float64{cpu, 0}}, "nginx")
}

func historyJSONRows(t *testing.T, db *sql.DB, command string, options historyOptions) []map[string]json.RawMessage {
	t.Helper()
	var output bytes.Buffer
	if err := printHistory(&output, db, command, options, true); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var rows []map[string]json.RawMessage
	for {
		var row map[string]json.RawMessage
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestHistoryRecordsAppendAndSearch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.db")
	recorder := topHistoryFixture(t, path)
	started := time.Date(2026, 10, 4, 0, 0, 0, 123456789, time.UTC)
	disk := 123.0
	first := historyFixtureSample(1, 90, topProcess{PID: 7, Started: started, Command: "NGINX 작업자", CPU: 120, RSS: 1024, DiskValid: true, DiskRead: disk})
	second := historyFixtureSample(2, 40, topProcess{PID: 7, Started: started.Add(100 * time.Millisecond), Command: "other", CPU: 3, RSS: 2048})
	for _, sample := range []historyTopSample{first, second} {
		if err := recorder.Record(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	other := topHistoryFixture(t, path)
	if err := other.Record(historyFixtureSample(3, 80, topProcess{PID: 8, Command: "작업자"})); err != nil {
		t.Fatal(err)
	}
	if err := other.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runs := historyJSONRows(t, db, "list", historyOptions{})
	if len(runs) != 2 || string(runs[0]["run_id"]) != strconv.Quote(other.runID) || string(runs[1]["sample_count"]) != "2" || string(runs[1]["status"]) != `"finished"` || runs[1]["ended_at"] == nil {
		t.Fatalf("runs = %s", mustHistoryJSON(t, runs))
	}
	min, max := 90.0, 90.0
	rows := historyJSONRows(t, db, "top", historyOptions{run: recorder.runID, metric: "cpu_pct", min: &min, max: &max})
	if len(rows) != 1 || string(rows[0]["cpu_pct"]) != "90" || string(rows[0]["window_s"]) != "1" || string(rows[0]["core_cpu_pct"]) != "[90,0]" || string(rows[0]["process_filter"]) != `"nginx"` || rows[0]["process_observed_at"] == nil {
		t.Fatalf("samples = %s", mustHistoryJSON(t, rows))
	}
	rows = historyJSONRows(t, db, "top", historyOptions{from: first.Time, to: second.Time})
	if len(rows) != 1 || string(rows[0]["time"]) != strconv.Quote(first.Time.Format(time.RFC3339Nano)) {
		t.Fatalf("time range = %s", mustHistoryJSON(t, rows))
	}
	filter, _ := parseTopProcessFilter("nginx,8")
	rows = historyJSONRows(t, db, "process", historyOptions{process: filter})
	if len(rows) != 2 || string(rows[1]["cpu_pct"]) != "120" || string(rows[1]["started"]) != strconv.Quote(started.Format(time.RFC3339Nano)) {
		t.Fatalf("process rows = %s", mustHistoryJSON(t, rows))
	}
	unicodeFilter, _ := parseTopProcessFilter("작업자")
	rows = historyJSONRows(t, db, "process", historyOptions{process: unicodeFilter})
	if len(rows) != 2 {
		t.Fatalf("unicode rows = %s", mustHistoryJSON(t, rows))
	}
	filter, _ = parseTopProcessFilter("7")
	rows = historyJSONRows(t, db, "process", historyOptions{process: filter})
	if len(rows) != 2 || bytes.Equal(rows[0]["started"], rows[1]["started"]) {
		t.Fatalf("PID reuse = %s", mustHistoryJSON(t, rows))
	}
	min = disk
	rows = historyJSONRows(t, db, "process", historyOptions{metric: "disk_read_bytes_per_s", min: &min, max: &min})
	if len(rows) != 1 || string(rows[0]["pid"]) != "7" {
		t.Fatalf("disk rows = %s", mustHistoryJSON(t, rows))
	}
	rows = historyJSONRows(t, db, "top", historyOptions{limit: 1})
	if len(rows) != 1 || string(rows[0]["time"]) != strconv.Quote(first.Time.Add(2*time.Second).Format(time.RFC3339Nano)) {
		t.Fatalf("limit = %s", mustHistoryJSON(t, rows))
	}
	zero := 0.0
	if rows := historyJSONRows(t, db, "top", historyOptions{metric: "disk_busy_pct", min: &zero}); len(rows) != 0 {
		t.Fatal("unsupported metrics matched zero")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file permission: %v, %v", info, err)
	}
	var text bytes.Buffer
	if err := printHistory(&text, db, "process", historyOptions{metric: "disk_read_bytes_per_s", min: &min}, false); err != nil || !strings.Contains(text.String(), "disk_read_bytes_per_s") || !strings.Contains(text.String(), "123") {
		t.Fatalf("metric table: %q, %v", text.String(), err)
	}
}

func mustHistoryJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHistoryReadWhileRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	recorder := topHistoryFixture(t, path)
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := recorder.Record(historyFixtureSample(1, 90, topProcess{PID: 7})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := historyJSONRows(t, db, "top", historyOptions{})
		if len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sample was not committed while recording")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := db.Exec("DELETE FROM top_samples"); err == nil {
		t.Fatal("history reader was writable")
	}
	runs := historyJSONRows(t, db, "list", historyOptions{})
	if string(runs[0]["status"]) != `"unfinished"` || runs[0]["ended_at"] != nil {
		t.Fatalf("active run must not look finished: %s", mustHistoryJSON(t, runs))
	}
}

func TestHistoryFailedSampleRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.db")
	recorder := topHistoryFixture(t, path)
	if _, err := recorder.db.Exec("CREATE TRIGGER reject_process BEFORE INSERT ON top_process_samples BEGIN SELECT RAISE(ABORT, 'injected failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record(historyFixtureSample(1, 90, topProcess{PID: 7})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.failed:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not report failure")
	}
	if err := recorder.Close("finished"); err == nil {
		t.Fatal("writer failure was lost on close")
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(historyJSONRows(t, db, "top", historyOptions{})) != 0 || len(historyJSONRows(t, db, "process", historyOptions{})) != 0 {
		t.Fatal("failed transaction left partial samples")
	}
	runs := historyJSONRows(t, db, "list", historyOptions{})
	if string(runs[0]["status"]) != `"error"` || string(runs[0]["sample_count"]) != "0" {
		t.Fatalf("failed run = %s", mustHistoryJSON(t, runs))
	}
}

func TestHistoryQueueOverflowFails(t *testing.T) {
	recorder := &topRecorder{queue: make(chan []byte, 1), failed: make(chan struct{})}
	sample := historyFixtureSample(1, 90, topProcess{PID: 7})
	if err := recorder.Record(sample); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Record(sample); err == nil {
		t.Fatal("full queue silently dropped a sample")
	}
	select {
	case <-recorder.failed:
	default:
		t.Fatal("overflow did not notify observer")
	}
}

func TestHistoryRejectsUnrelatedAndFutureDatabase(t *testing.T) {
	for _, statement := range []string{"CREATE TABLE unrelated (value TEXT)", "PRAGMA application_id=1162101576; PRAGMA user_version=2"} {
		path := filepath.Join(t.TempDir(), "foreign.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		db.Close()
		before, _ := os.ReadFile(path)
		for _, write := range []bool{true, false} {
			if opened, err := openHistoryDB(path, write); err == nil {
				opened.Close()
				t.Fatal("foreign schema was accepted")
			}
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("foreign database was modified")
		}
	}
	path := filepath.Join(t.TempDir(), "missing.db")
	if db, err := openHistoryDB(path, false); err == nil {
		db.Close()
		t.Fatal("missing database was accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read created a database")
	}
}

func TestHistoryCLIRejectsInvalidArgumentsBeforeOpeningFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	for _, args := range [][]string{
		{"top", "--metric", "cpu_pct; DROP TABLE runs", "--min", "0", path},
		{"top", "--min", "0", path}, {"top", "--metric", "cpu_pct", path},
		{"top", "--metric", "cpu_pct", "--min", "NaN", path},
		{"top", "--metric", "cpu_pct", "--min", "2", "--max", "1", path},
		{"top", "--from", "yesterday", path},
		{"top", "--from", "2026-10-05T00:00:02Z", "--to", "2026-10-05T00:00:01Z", path},
		{"top", "--limit", "-1", path}, {"process", "--process", "x,", path},
		{"list", "--metric", "cpu_pct", path}, {"top", "--json", path, path},
	} {
		if code := runHistory(args); code != 2 {
			t.Errorf("%v returned %d", args, code)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid arguments created a database")
	}
}

func TestTopRecordingUsesDashboardSamplesAndSkipsBaselines(t *testing.T) {
	model := topFixtureModel(nil)
	var recorded []historyTopSample
	model.record = func(sample historyTopSample) error { recorded = append(recorded, sample); return nil }
	filter, _ := parseTopProcessFilter("worker")
	model.processFilter = filter
	model = topAfter(t, model, topSampleMsg{snapshot: topSampleAt(1)})
	model = topAfter(t, model, topSampleMsg{seq: model.seq + 1, snapshot: topSampleAt(2)})
	model = topAfter(t, model, topSampleMsg{seq: model.seq, err: errors.New("temporary")})
	if len(recorded) != 1 || recorded[0].ProcessFilter != "worker" || recorded[0].WindowS != 1 || !recorded[0].Time.Equal(model.rows[0].at) || recorded[0].MemoryPct != model.rows[0].rate.MemoryPercent {
		t.Fatalf("recorded = %#v", recorded)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'p', Text: "p"})
	model = topAfter(t, model, topSampleMsg{seq: model.seq - 1, snapshot: topSampleAt(30)})
	model = topAfter(t, model, tea.KeyPressMsg{Code: 'p', Text: "p"})
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: topSampleAt(100)})
	if len(recorded) != 1 {
		t.Fatal("paused/stale sample or resume baseline was recorded")
	}
	filter, _ = parseTopProcessFilter("other")
	model.processFilter = filter
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: topSampleAt(101)})
	if len(recorded) != 2 || recorded[1].ProcessFilter != "other" || recorded[1].WindowS != 1 {
		t.Fatalf("resumed samples = %#v", recorded)
	}
	model.record = func(historyTopSample) error { return errors.New("write failed") }
	final, cmd := model.Update(topSampleMsg{seq: model.seq, snapshot: topSampleAt(102)})
	if final.(topModel).recordingErr == nil || cmd == nil {
		t.Fatal("write failure did not stop dashboard")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("write failure did not request terminal restoration")
	}
	final, cmd = model.Update(topRecordingErrorMsg{err: errors.New("background failure")})
	if final.(topModel).recordingErr == nil || cmd == nil {
		t.Fatal("background failure was ignored")
	}
}

func TestTopWriteAndJSONTogether(t *testing.T) {
	dir := t.TempDir()
	path, jsonPath := filepath.Join(dir, "samples.db"), filepath.Join(dir, "samples.jsonl")
	defer processSampler.setFilter(topProcessFilter{})
	if code := runTop([]string{"--interval", "200ms", "--count", "2", "--process", strconv.Itoa(os.Getpid()), "--json", jsonPath, "-w", path}, "test"); code != 0 {
		t.Fatalf("top returned %d", code)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := historyJSONRows(t, db, "top", historyOptions{})
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(rows) != 2 || len(lines) != 2 {
		t.Fatalf("DB rows %d, JSON rows %d", len(rows), len(lines))
	}
	for index, line := range lines {
		var sample map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			t.Fatal(err)
		}
		if sample["run_id"] != nil || sample["cpu_pct"] != nil || sample["core_cpu_pct"] != nil {
			t.Fatal("recording changed the existing JSON schema")
		}
		for _, name := range []string{"time", "cpu_user_pct", "memory_pct"} {
			if !bytes.Equal(sample[name], rows[1-index][name]) {
				t.Fatalf("sample %d field %s differs", index, name)
			}
		}
	}
	if code := runHistory([]string{"process", "--json", filepath.Join(dir, "history.jsonl"), path}); code != 0 {
		t.Fatalf("history returned %d", code)
	}
}

func TestHistoryOutputCannotOverwriteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.db")
	recorder := topHistoryFixture(t, path)
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	alias := path + ".alias"
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{path, alias, path + "-wal", path + "-shm"} {
		if code := runTop([]string{"--count", "1", "--write", path, "--json", output}, "test"); code != 2 {
			t.Fatalf("top allowed overwrite of %s", output)
		}
		if code := runHistory([]string{"top", "--json", output, path}); code != 2 {
			t.Fatalf("history allowed overwrite of %s", output)
		}
	}
	if db, err := openHistoryDB(path, false); err != nil {
		t.Fatal("database was damaged")
	} else {
		db.Close()
	}
}

func TestHistoryTerminalTextEscapesControls(t *testing.T) {
	value := historyTerminalText("worker\tcolumn\nline\r\x1b[2J")
	if strings.ContainsAny(value, "\t\n\r\x1b") || !strings.Contains(value, `\x1b`) {
		t.Fatalf("unsafe terminal text: %q", value)
	}
}

func TestHistoryRejectsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, fifo, "", "-"} {
		for _, write := range []bool{false, true} {
			if db, err := openHistoryDB(path, write); err == nil {
				db.Close()
				t.Fatalf("accepted special path %q", path)
			}
		}
	}
}

func TestHistoryPreservesMissingAndEmptyProcessSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "availability.db")
	recorder := topHistoryFixture(t, path)
	previous := resourceSnapshot{TakenAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	current := resourceSnapshot{TakenAt: previous.TakenAt.Add(time.Second), NetMissing: true, DiskMissing: true, SwapMissing: true}
	missing := newHistoryTopSample(hostDetails{Hostname: "fixture"}, previous, current, resourceRate{}, "")
	if err := recorder.Record(missing); err != nil {
		t.Fatal(err)
	}
	current.TakenAt = current.TakenAt.Add(time.Second)
	current.ProcessesValid = true
	empty := newHistoryTopSample(hostDetails{Hostname: "fixture"}, previous, current, resourceRate{}, "absent")
	if err := recorder.Record(empty); err != nil {
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
	rows := historyJSONRows(t, db, "top", historyOptions{})
	if len(rows) != 2 || string(rows[0]["processes"]) != "[]" || string(rows[0]["processes_valid"]) != "true" || rows[1]["processes"] != nil || string(rows[1]["processes_valid"]) != "false" {
		t.Fatalf("availability lost: %s", mustHistoryJSON(t, rows))
	}
	zero := 0.0
	for _, name := range []string{"net_in_bytes_per_s", "disk_read_bytes_per_s", "swap_out_bytes_per_s", "psi_cpu_some_avg10_pct"} {
		if rows := historyJSONRows(t, db, "top", historyOptions{metric: name, min: &zero, max: &zero}); len(rows) != 0 {
			t.Fatalf("unmeasured %s matched zero", name)
		}
	}
}

func TestHistoryKeepsCgroupAndEBPFDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "details.db")
	recorder := topHistoryFixture(t, path)
	stats := &topProbeStats{Window: time.Second, Source: topProbeSourceEBPF, Measured: 1, RunqCount: 2, RunqSumNS: 4_000_000, RunqHist: &topProbeHist{}, IO: &topProbeIO{Count: 3, Bytes: 4096}}
	limits := &topProcessLimits{Cgroup: topCgroupLimits{topLimitStatus: topLimitStatus{Status: "supported"}, Path: "/worker.slice"}}
	sample := historyFixtureSample(1, 90, topProcess{PID: 7, Probe: stats, Limits: limits})
	sample.ProcessTotal = newTopProcessTotalSample(topProcessTotal{Count: 70, CPU: 500, Probe: stats})
	if err := recorder.Record(sample); err != nil {
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
	rows := historyJSONRows(t, db, "top", historyOptions{})
	if len(rows) != 1 || !bytes.Equal(rows[0]["process_total"], mustHistoryJSON(t, sample.ProcessTotal)) {
		t.Fatalf("total lost: %s", mustHistoryJSON(t, rows))
	}
	rows = historyJSONRows(t, db, "process", historyOptions{})
	if len(rows) != 1 || !bytes.Equal(rows[0]["limits"], mustHistoryJSON(t, limits)) || !bytes.Equal(rows[0]["ebpf"], mustHistoryJSON(t, (*sample.Processes)[0].EBPF)) {
		t.Fatalf("details lost: %s", mustHistoryJSON(t, rows))
	}
}

func TestTopWriteAllowsProcessFilterInTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "table.db")
	defer processSampler.setFilter(topProcessFilter{})
	if code := runTop([]string{"--interval", "200ms", "--count", "2", "--process", strconv.Itoa(os.Getpid()), "--no-header", "--write", path}, "test"); code != 0 {
		t.Fatalf("table recording returned %d", code)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := historyJSONRows(t, db, "top", historyOptions{})
	if len(rows) != 2 || string(rows[0]["process_filter"]) != strconv.Quote(strconv.Itoa(os.Getpid())) {
		t.Fatalf("filtered table recording = %s", mustHistoryJSON(t, rows))
	}
}

func TestTopOptionalWritePathDoesNotConsumeOtherOptions(t *testing.T) {
	for _, row := range []struct {
		args        []string
		path        string
		json        string
		defaultPath bool
	}{
		{args: []string{"-w"}, defaultPath: true},
		{args: []string{"--write", "--count", "2"}, defaultPath: true},
		{args: []string{"--count", "2", "-w", "--detail"}, defaultPath: true},
		{args: []string{"-w", "incident.db"}, path: "incident.db"},
		{args: []string{"--write=incident.db"}, path: "incident.db"},
		{args: []string{"-w=-named.db"}, path: "-named.db"},
		{args: []string{"-w", "--write", "incident.db"}, path: "incident.db"},
		{args: []string{"--write", "incident.db", "-w"}, defaultPath: true},
		{args: []string{"--json", "-w"}, json: "-w"},
		{args: []string{"--json=-w", "--write"}, json: "-w", defaultPath: true},
		{args: []string{"--process", "--write"}},
		{args: []string{"--", "-w"}},
	} {
		set := flag.NewFlagSet("top", flag.ContinueOnError)
		path := set.String("write", "", "")
		set.StringVar(path, "w", "", "")
		jsonPath := set.String("json", "", "")
		set.Int("count", 0, "")
		set.String("process", "", "")
		set.Bool("detail", false, "")
		args, useDefault := normalizeTopWriteArgs(row.args, set)
		if err := set.Parse(args); err != nil {
			t.Fatal(err)
		}
		if useDefault != row.defaultPath || *path != row.path || *jsonPath != row.json {
			t.Errorf("%v: default=%t path=%q json=%q", row.args, useDefault, *path, *jsonPath)
		}
	}
}

func TestTopSplitOptionalListDoesNotConsumeOtherOptions(t *testing.T) {
	for _, row := range []struct {
		args    []string
		split   string
		path    string
		defPath bool
		detail  bool
		count   int
		narg    int
	}{
		{args: []string{"--split"}, split: "all"},
		{args: []string{"--split", "mem,disk"}, split: "mem,disk"},
		{args: []string{"--split", "-d"}, split: "all", detail: true},
		{args: []string{"--split", "--count", "2"}, split: "all", count: 2},
		{args: []string{"--split=mem"}, split: "mem"},
		{args: []string{"--split="}, split: ""},
		{args: []string{"-w", "--split", "mem"}, split: "mem", defPath: true},
		{args: []string{"--split", "mem", "--split", "disk"}, split: "disk"},
		{args: []string{"--split", "-"}, split: "all", narg: 1},
	} {
		set := flag.NewFlagSet("top", flag.ContinueOnError)
		set.SetOutput(io.Discard)
		split := set.String("split", "", "")
		path := set.String("write", "", "")
		set.StringVar(path, "w", "", "")
		detail := set.Bool("d", false, "")
		count := set.Int("count", 0, "")
		args, useDefault := normalizeTopWriteArgs(row.args, set)
		if err := set.Parse(args); err != nil {
			t.Fatalf("%v: %v", row.args, err)
		}
		if *split != row.split || useDefault != row.defPath || *path != row.path || *detail != row.detail || *count != row.count || set.NArg() != row.narg {
			t.Errorf("%v: split=%q default=%t path=%q d=%t count=%d narg=%d", row.args, *split, useDefault, *path, *detail, *count, set.NArg())
		}
	}
}

func TestTopSplitRejectsNonDashboardUse(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig = edcConfig{}
	pid := strconv.Itoa(os.Getpid())
	for _, args := range [][]string{
		{"--split", "--count", "1"},
		{"--split=", "--count", "1"},
		{"--split", "mem,bogus", "--count", "1"},
		{"--split", "mem,mem", "--count", "1"},
		{"--split", "none", "--count", "1"},
		{"--split", "mem", "--process", pid, "--count", "1"},
	} {
		if code := runTop(args, "test"); code != 2 {
			t.Errorf("runTop(%v) = %d, want 2", args, code)
		}
	}
	existing := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(existing, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runTop([]string{"--split", "--json", existing}, "test"); code != 2 {
		t.Fatalf("--split --json = %d, want 2", code)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "keep\n" {
		t.Fatalf("json file = %q, %v; want it untouched", data, err)
	}
	database := filepath.Join(t.TempDir(), "never.db")
	if code := runTop([]string{"--count", "1", "-w", database, "--split"}, "test"); code != 2 {
		t.Fatalf("--split with -w = %d, want 2", code)
	}
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was created before the split check: %v", err)
	}
	activeConfig.Defaults.Top.Split = stringPointer("mem")
	if code := runTop([]string{"--interval", "200ms", "--count", "1", "--json", filepath.Join(t.TempDir(), "out.jsonl")}, "test"); code != 0 {
		t.Fatalf("configured split blocked a JSON run: %d", code)
	}
}

func TestDefaultHistoryPathFollowsStateDirectory(t *testing.T) {
	for _, row := range []struct{ goos, home, state, want string }{
		{"linux", "/users/one", "", "/users/one/.local/state/edc/history.db"},
		{"darwin", "/users/one", "", "/users/one/Library/Application Support/edc/history.db"},
		{"linux", "/users/one", "/state", "/state/edc/history.db"},
		{"darwin", "/users/one", "/state", "/state/edc/history.db"},
		{"linux", "/users/one", "relative", "/users/one/.local/state/edc/history.db"},
		{"darwin", "/users/one", "relative", "/users/one/Library/Application Support/edc/history.db"},
		{"linux", "", "/state", "/state/edc/history.db"},
		{"linux", "", "relative", ""},
	} {
		if got := defaultHistoryPathFor(row.goos, row.home, row.state); got != row.want {
			t.Errorf("default path for %+v: %q", row, got)
		}
	}
}

func TestTopBareWriteUsesDefaultAndHistoryReadsIt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	path, err := defaultHistoryPath()
	if err != nil {
		t.Fatal(err)
	}
	if code := runHistory([]string{"list"}); code != 1 {
		t.Fatalf("missing default database returned %d", code)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("history created the default directory")
	}
	if code := runTop([]string{"--interval", "200ms", "--count", "1", "--json", filepath.Join(t.TempDir(), "unsaved.jsonl")}, "test"); code != 0 {
		t.Fatalf("top without recording returned %d", code)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("top without -w created the default directory")
	}
	for _, args := range [][]string{
		{"-w", "--interval", "200ms", "--count", "1", "--json", filepath.Join(t.TempDir(), "samples.jsonl")},
		{"--interval", "200ms", "--count", "1", "--json", filepath.Join(t.TempDir(), "samples.jsonl"), "--write"},
	} {
		if code := runTop(args, "test"); code != 0 {
			t.Fatalf("bare write returned %d", code)
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runs := historyJSONRows(t, db, "list", historyOptions{})
	if len(runs) != 2 || string(runs[0]["sample_count"]) != "1" || string(runs[1]["sample_count"]) != "1" {
		t.Fatalf("default database did not append: %s", mustHistoryJSON(t, runs))
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("default directory permissions: %v %v", directory, err)
	}
	output := filepath.Join(t.TempDir(), "history.jsonl")
	if code := runHistory([]string{"top", "--json", output}); code != 0 {
		t.Fatalf("default history returned %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil || bytes.Count(data, []byte("\n")) != 2 {
		t.Fatalf("default history output: %q %v", data, err)
	}
	if code := runTop([]string{"-w", "--json", path, "--count", "1"}, "test"); code != 2 {
		t.Fatal("default DB could be overwritten by JSON")
	}
	if code := runTop([]string{"--write=", "--count", "1"}, "test"); code != 2 {
		t.Fatal("explicit empty path was treated as a default")
	}
}
