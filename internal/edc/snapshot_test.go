package edc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snapshotResultsByProbe(results []Result) map[string]Result {
	indexed := map[string]Result{}
	for _, result := range results {
		indexed[result.Probe] = result
	}
	return indexed
}

func TestCollectSnapshotSkipsASectionThatNeverReturns(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	sections := []snapshotSection{
		{name: "info.host", run: func(context.Context) []Result { return []Result{{Probe: "info.host", Status: StatusPass}} }},
		{name: "info.disk", run: func(context.Context) []Result { <-block; return []Result{{Probe: "info.disk:/", Status: StatusPass}} }},
		{name: "listen", run: func(context.Context) []Result { return []Result{{Probe: "listen", Status: StatusPass}} }},
	}
	budget, grace := 50*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	started := time.Now()
	results := collectSnapshot(ctx, sections, grace)
	if elapsed := time.Since(started); elapsed > budget+grace+200*time.Millisecond {
		t.Fatalf("collectSnapshot waited %s, want about %s", elapsed, budget+grace)
	}
	indexed := snapshotResultsByProbe(results)
	if len(results) != 3 {
		t.Fatalf("results = %+v, want one per section", results)
	}
	skipped, ok := indexed["info.disk"]
	if !ok || skipped.Status != StatusSkip || skipped.Summary != T("snapshot.skip.timeout", "info.disk", budget) {
		t.Fatalf("info.disk = %+v, want a timeout skip", skipped)
	}
	if indexed["info.host"].Status != StatusPass || indexed["listen"].Status != StatusPass {
		t.Fatalf("results = %+v, the other sections must stay", results)
	}
}

func TestCollectSnapshotKeepsResultsReturnedDuringTheGrace(t *testing.T) {
	sections := []snapshotSection{
		{name: "audit", run: func(ctx context.Context) []Result {
			<-ctx.Done()
			time.Sleep(10 * time.Millisecond)
			return []Result{{Probe: "audit.net.exposed", Status: StatusSkip, Summary: "partial"}}
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results := collectSnapshot(ctx, sections, time.Second)
	if len(results) != 1 || results[0].Probe != "audit.net.exposed" || results[0].Summary != "partial" {
		t.Fatalf("results = %+v, want the section's own results", results)
	}
}

func TestCollectSnapshotKeepsAFailedSectionAndTheOthers(t *testing.T) {
	started := time.Now()
	sections := []snapshotSection{
		{name: "info.memory", run: func(context.Context) []Result {
			return []Result{snapshotMemoryResult(started, infoMemory{}, errors.New("meminfo unreadable"))}
		}},
		{name: "info.host", run: func(context.Context) []Result {
			return []Result{snapshotHostResult(started, hostDetails{Cores: 2}, nil)}
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	indexed := snapshotResultsByProbe(collectSnapshot(ctx, sections, 0))
	failed := indexed["info.memory"]
	if failed.Status != StatusFail || failed.Error == nil || failed.Error.Kind != "snapshot" || !strings.Contains(failed.Summary, "meminfo unreadable") {
		t.Fatalf("info.memory = %+v, want a fail Result of kind snapshot", failed)
	}
	if indexed["info.host"].Status != StatusPass {
		t.Fatalf("info.host = %+v, want pass", indexed["info.host"])
	}
}

func TestSnapshotConvertersReturnFailOnError(t *testing.T) {
	started := time.Now()
	err := errors.New("boom")
	results := []Result{
		snapshotHostResult(started, hostDetails{}, err),
		snapshotMemoryResult(started, infoMemory{}, err),
		snapshotProcessResult(started, nil, err),
	}
	results = append(results, snapshotDiskResults(started, nil, nil, err)...)
	for index, probe := range []string{"info.host", "info.memory", "info.processes", "info.disk"} {
		result := results[index]
		if result.Probe != probe || result.Status != StatusFail || result.Error == nil || result.Error.Kind != "snapshot" {
			t.Errorf("result %d = %+v, want a fail Result for %s", index, result, probe)
		}
	}
}

func TestSnapshotHostResultHoldsScalarMetrics(t *testing.T) {
	details := hostDetails{OS: "Ubuntu", Version: "24.04", Release: "6.8.0", Machine: "x86_64", Model: "Xeon", Cores: 4,
		Uptime: 90 * time.Minute, Load: [3]float64{0.5, 0.25, 0.1}, MemoryTotal: 8 << 30, SwapUsed: 1 << 20, SwapTotal: 2 << 30,
		RLimitSoft: 1024, RLimitHard: 4096}
	result := snapshotHostResult(time.Now(), details, nil)
	if result.Status != StatusPass {
		t.Fatalf("status = %s, want pass", result.Status)
	}
	if result.Metrics["uptime_seconds"] != int64(5400) {
		t.Errorf("uptime_seconds = %v, want 5400", result.Metrics["uptime_seconds"])
	}
	// report diff는 저장된 JSON을 읽어 비교하므로 왕복한 값이 scalar여야 한다.
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	result = Result{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"os", "version", "release", "machine", "model", "cores", "uptime_seconds", "load1", "load5", "load15",
		"memory_total_bytes", "swap_used_bytes", "swap_total_bytes", "nofile_soft", "nofile_hard"} {
		value, ok := result.Metrics[key]
		if !ok || !isScalarMetric(value) {
			t.Errorf("metric %s = %#v, want a scalar", key, value)
		}
	}
}

func TestSnapshotMemoryResultWarnsAtTheDiskLimit(t *testing.T) {
	for _, test := range []struct {
		available uint64
		want      Status
	}{{1010, StatusPass}, {1000, StatusWarn}} {
		// total 10000에서 available 1010은 89.9%, 1000은 90%다.
		result := snapshotMemoryResult(time.Now(), infoMemory{Total: 10000, Available: test.available}, nil)
		if result.Status != test.want {
			t.Errorf("available %d: status = %s (%v%%), want %s", test.available, result.Status, result.Metrics["used_percent"], test.want)
		}
		if result.Metrics["used_bytes"] != uint64(10000)-test.available {
			t.Errorf("used_bytes = %v", result.Metrics["used_bytes"])
		}
	}
}

func TestSnapshotPressureResultCoversMissingFacts(t *testing.T) {
	started := time.Now()
	none := snapshotPressureResult(started, infoSignals{})
	if none.Status != StatusSkip || none.Summary != T("snapshot.skip.pressure_unavailable") {
		t.Fatalf("no PSI or OOM = %+v, want a skip", none)
	}
	zero, one := uint64(0), uint64(1)
	oomOnly := snapshotPressureResult(started, infoSignals{OOMKills: &zero})
	if oomOnly.Status != StatusPass || oomOnly.Metrics["oom_kills"] != uint64(0) || len(oomOnly.Warnings) != 1 {
		t.Fatalf("OOM only = %+v, want pass with a PSI warning", oomOnly)
	}
	if _, ok := oomOnly.Metrics["psi_cpu_some_avg10"]; ok {
		t.Fatalf("OOM only = %+v, must not invent PSI values", oomOnly)
	}
	killed := snapshotPressureResult(started, infoSignals{OOMKills: &one, Pressure: []float64{0, 0, 0}})
	if killed.Status != StatusWarn || len(killed.Warnings) != 0 {
		t.Fatalf("one OOM kill = %+v, want warn", killed)
	}
	pressured := snapshotPressureResult(started, infoSignals{OOMKills: &zero, Pressure: []float64{1, 10, 2}})
	if pressured.Status != StatusWarn || pressured.Metrics["psi_memory_some_avg10"] != 10.0 {
		t.Fatalf("PSI 10 = %+v, want warn", pressured)
	}
	calm := snapshotPressureResult(started, infoSignals{OOMKills: &zero, Pressure: []float64{1, 9.9, 2}})
	if calm.Status != StatusPass {
		t.Fatalf("PSI 9.9 = %+v, want pass", calm)
	}
}

func TestSnapshotDiskResultsSetStatusAndKeepOnePerMount(t *testing.T) {
	disks := []diskDetails{
		{Mount: "/", Device: "/dev/sda1", Used: 899, Total: 1000, Percent: 89.9},
		{Mount: "/data", Device: "/dev/sdb1", Used: 900, Total: 1000, Percent: 90},
		{Mount: "/ro", Device: "/dev/sdc1", Used: 1, Total: 1000, Percent: 0.1},
		{Mount: "/full", Device: "/dev/sdd1", Used: 1, Total: 1000, Percent: 0.1},
		{Mount: "/data", Device: "/dev/sde1", Used: 100, Total: 1000, Percent: 10},
	}
	signals := map[string]diskSignal{"/ro": {ReadOnly: true}, "/full": {InodesUsed: 80, InodesTotal: 100}, "/": {InodesUsed: 1, InodesTotal: 100}}
	results := snapshotDiskResults(time.Now(), disks, signals, nil)
	if len(results) != 4 {
		t.Fatalf("results = %+v, want one per mount", results)
	}
	indexed := snapshotResultsByProbe(results)
	for probe, want := range map[string]Status{"info.disk:/": StatusPass, "info.disk:/data": StatusPass, "info.disk:/ro": StatusWarn, "info.disk:/full": StatusWarn} {
		if got := indexed[probe].Status; got != want {
			t.Errorf("%s = %s, want %s", probe, got, want)
		}
	}
	if indexed["info.disk:/data"].Metrics["device"] != "/dev/sde1" {
		t.Errorf("/data = %+v, want the last df row", indexed["info.disk:/data"])
	}
	if _, ok := indexed["info.disk:/ro"].Metrics["inodes_used_percent"]; ok {
		t.Errorf("/ro has inodes_used_percent without inode counts")
	}
	at90 := snapshotDiskResults(time.Now(), []diskDetails{{Mount: "/", Percent: 90}}, nil, nil)
	if at90[0].Status != StatusWarn {
		t.Errorf("90%% = %s, want warn", at90[0].Status)
	}
}

func TestSnapshotProcessResultKeepsTheTopFive(t *testing.T) {
	var processes []topProcess
	for pid := 1; pid <= 8; pid++ {
		processes = append(processes, topProcess{PID: pid, RSS: uint64(pid) << 20, Command: "proc", Threads: 2})
	}
	result := snapshotProcessResult(time.Now(), processes, nil)
	if result.Metrics["count"] != 8 || result.Metrics["threads"] != 16 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	lines := strings.Split(result.Evidence[0].Value, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "PID 8 ") {
		t.Fatalf("evidence lines = %q, want five, largest RSS first", lines)
	}
}

func TestWriteSnapshotFileCreatesMode0600AndRefusesExisting(t *testing.T) {
	directory := t.TempDir()
	report := buildReport("test", time.Now(), nil, []Result{{Probe: "info.host", Status: StatusPass}}, false)
	path := filepath.Join(directory, "a.json")
	if err := writeSnapshotFile(path, report, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	before, _ := os.ReadFile(path)
	err = writeSnapshotFile(path, report, false)
	if !errors.Is(err, errSnapshotExists) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second write err = %v, want errSnapshotExists naming --force", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the refused write changed the file")
	}
}

func TestWriteSnapshotFileRefusesASymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.json")
	dangling := filepath.Join(directory, "dangling.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	report := buildReport("test", time.Now(), nil, nil, false)
	for _, path := range []string{link, dangling} {
		if err := writeSnapshotFile(path, report, false); !errors.Is(err, errSnapshotExists) {
			t.Errorf("%s: err = %v, want errSnapshotExists", path, err)
		}
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("target = %q, the symlink target changed", data)
	}
	if _, err := os.Lstat(filepath.Join(directory, "missing")); !os.IsNotExist(err) {
		t.Fatalf("the dangling symlink target was created: %v", err)
	}
	if err := writeSnapshotFile(link, report, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("link after --force = %v %v, want a 0600 regular file", info, err)
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("target = %q, --force wrote through the symlink", data)
	}
}

func TestWriteSnapshotFileForceReplaces0644With0600(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "a.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := buildReport("test", time.Now(), nil, []Result{{Probe: "info.host", Status: StatusPass}}, false)
	if err := writeSnapshotFile(path, report, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := loadReport(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, the temp file stayed", len(entries))
	}
}

func TestWriteSnapshotFileLeavesNothingOnError(t *testing.T) {
	directory := t.TempDir()
	report := buildReport("test", time.Now(), nil, nil, false)
	missing := filepath.Join(directory, "missing", "a.json")
	for _, force := range []bool{false, true} {
		if err := writeSnapshotFile(missing, report, force); err == nil || errors.Is(err, errSnapshotExists) {
			t.Errorf("force=%v: err = %v, want a write error", force, err)
		}
	}
	target := filepath.Join(directory, "dir.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotFile(target, report, true); err == nil {
		t.Fatal("--force over a directory succeeded")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, a temp file stayed", len(entries))
	}
}

func TestSnapshotReportsRoundTripThroughReportDiff(t *testing.T) {
	directory := t.TempDir()
	started := time.Now()
	disks := func(used uint64) []diskDetails {
		return []diskDetails{{Mount: "/", Device: "/dev/sda1", Used: used, Total: 1000, Percent: float64(used) / 10}}
	}
	first := append([]Result{snapshotMemoryResult(started, infoMemory{Total: 1000, Available: 500}, nil)}, snapshotDiskResults(started, disks(100), nil, nil)...)
	second := append([]Result{snapshotMemoryResult(started, infoMemory{Total: 1000, Available: 400}, nil)}, snapshotDiskResults(started, disks(300), nil, nil)...)
	sortResults(first)
	sortResults(second)
	since := map[string]interface{}{"since": (6 * time.Hour).String()}
	before, after := filepath.Join(directory, "before.json"), filepath.Join(directory, "after.json")
	if err := writeSnapshotFile(before, buildReport("test", started, since, first, false), false); err != nil {
		t.Fatal(err)
	}
	if err := writeSnapshotFile(after, buildReport("test", started, since, second, false), false); err != nil {
		t.Fatal(err)
	}
	loadedBefore, err := loadReport(before)
	if err != nil {
		t.Fatal(err)
	}
	loadedAfter, err := loadReport(after)
	if err != nil {
		t.Fatal(err)
	}
	diff := diffReports(before, loadedBefore, after, loadedAfter)
	found := map[string]float64{}
	for _, entry := range diff.Entries {
		for _, metric := range entry.Metrics {
			if metric.Key == "used_bytes" && metric.Delta != nil {
				found[entry.Probe] = *metric.Delta
			}
		}
	}
	if found["info.memory"] != 100 || found["info.disk:/"] != 200 {
		t.Fatalf("used_bytes deltas = %v, entries = %+v", found, diff.Entries)
	}
}

func TestSnapshotRedactedReportStillLoads(t *testing.T) {
	hostname, _ := os.Hostname()
	results := []Result{
		snapshotHostResult(time.Now(), hostDetails{Model: hostname, Cores: 1}, nil),
		{Probe: "listen", Status: StatusPass, Summary: "10.0.0.1:22", Metrics: map[string]interface{}{"address": "10.0.0.1"}},
	}
	sortResults(results)
	path := filepath.Join(t.TempDir(), snapshotDefaultPath(hostname, time.Now(), true))
	if err := writeSnapshotFile(path, buildReport("test", time.Now(), map[string]interface{}{"since": "6h0m0s"}, results, true), false); err != nil {
		t.Fatal(err)
	}
	report, err := loadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Redaction.Enabled || strings.Contains(report.Results[0].Summary, "10.0.0.1") {
		t.Fatalf("report = %+v, want a redacted report", report)
	}
}

func TestSnapshotDefaultPathSanitizesTheHost(t *testing.T) {
	started := time.Date(2026, 10, 12, 6, 54, 24, 0, time.FixedZone("KST", 9*3600))
	for _, test := range []struct {
		host   string
		redact bool
		want   string
	}{
		{"web-01.example", false, "edc-snapshot-web-01.example-20261011T215424Z.json"},
		{"my host/../x", false, "edc-snapshot-my-host-..-x-20261011T215424Z.json"},
		{"web-01", true, "edc-snapshot-20261011T215424Z.json"},
		{"", false, "edc-snapshot-20261011T215424Z.json"},
	} {
		if got := snapshotDefaultPath(test.host, started, test.redact); got != test.want {
			t.Errorf("snapshotDefaultPath(%q, %v) = %q, want %q", test.host, test.redact, got, test.want)
		}
	}
}
