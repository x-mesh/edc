package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeMacEventsReport(t *testing.T, dir, name, header, body string, modified time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(header+"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestMacEventsReadJetsamAndPanicReportsInTheWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("KST", 9*3600))
	// 실제 JetsamEvent는 process 1000여 개를 담는다. bufio 버퍼(4KB)보다 큰 본문으로 첫 줄이 덮어써지지 않는지도 본다.
	idle := strings.Repeat(`{"name":"idle","pid":1,"rpages":1},`, 400)
	jetsam := `{"largestProcess":"OrbStack Helper","memoryStatus":{"pageSize":16384},"processes":[` + idle + `{"name":"stickersd","pid":65914,"rpages":1033,"reason":"per-process-limit"}]}`
	writeMacEventsReport(t, dir, "JetsamEvent-2026-10-29-003245.ips", `{"bug_type":"298","timestamp":"2026-10-29 00:32:45.00 +0900"}`, jetsam, now.Add(-11*time.Hour))
	writeMacEventsReport(t, dir, "panic-full-2026-10-29-100000.panic", `{"bug_type":"210","timestamp":"2026-10-29 10:00:00.00 +0900"}`, `{"panicString":"panic(cpu 3 caller 0x1): watchdog timeout\nmore"}`, now.Add(-2*time.Hour))
	writeMacEventsReport(t, dir, "Safari-2026-10-29-110000.ips", `{"bug_type":"309","timestamp":"2026-10-29 11:00:00.00 +0900"}`, `{"processes":[{"name":"x","reason":"crash"}]}`, now.Add(-time.Hour))
	writeMacEventsReport(t, dir, "JetsamEvent-2026-10-20-000000.ips", `{"bug_type":"298","timestamp":"2026-10-20 00:00:00.00 +0900"}`, jetsam, now.Add(-9*24*time.Hour))

	facts := collectMacEventsFacts([]string{dir, filepath.Join(dir, "Retired")}, now, 24*time.Hour)
	if facts.Err != nil || len(facts.Warnings) != 0 || len(facts.Signals) != 2 {
		t.Fatalf("facts = %+v", facts)
	}
	results := evaluateMacEvents(facts)
	byProbe := map[string]Result{}
	for _, result := range results {
		byProbe[result.Probe] = result
	}
	if result := byProbe["events.jetsam"]; result.Status != StatusWarn || !strings.Contains(result.Summary, "stickersd (per-process-limit)") ||
		!strings.Contains(result.Evidence[0].Value, "stickersd pid 65914 killed by jetsam: per-process-limit, resident 0.02 GB, largest process OrbStack Helper") {
		t.Fatalf("jetsam = %+v", result)
	}
	if result := byProbe["events.panic"]; result.Status != StatusFail || result.Error == nil || !strings.Contains(result.Summary, "watchdog timeout") || strings.Contains(result.Summary, "more") {
		t.Fatalf("panic = %+v", result)
	}
	if result := byProbe["events.window"]; result.Status != StatusPass || !strings.Contains(result.Evidence[0].Value, "jetsam       stickersd") {
		t.Fatalf("window = %+v", result)
	}
}

func TestMacEventsSkipWhenTheReportDirectoryIsUnreadable(t *testing.T) {
	facts := collectMacEventsFacts([]string{filepath.Join(t.TempDir(), "missing")}, time.Now(), time.Hour)
	results := evaluateMacEvents(facts)
	if len(results) != 1 || results[0].Status != StatusSkip || results[0].Probe != "events.window" {
		t.Fatalf("results = %+v", results)
	}
	quiet := evaluateMacEvents(collectMacEventsFacts([]string{t.TempDir()}, time.Now(), time.Hour))
	for _, result := range quiet {
		if result.Status != StatusPass {
			t.Fatalf("empty report directory = %+v", quiet)
		}
	}
}
