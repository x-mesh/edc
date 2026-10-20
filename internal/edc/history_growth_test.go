package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyGrowthDB는 하루 간격의 스냅샷을 기록한다. used는 / 마운트의 사용량(전체 1000), data는 /var 아래 lib/data의 크기다.
func historyGrowthDB(t *testing.T, start time.Time, used, data []uint64) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	for index := range used {
		report := snapshotHistoryReport(start.Add(time.Duration(index)*24*time.Hour), used[index], data[index])
		if err := recordSnapshot(path, report, time.Hour, []string{"/var"}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHistoryGrowthEstimatesWhenADiskFills(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db := historyGrowthDB(t, start, []uint64{100, 300, 500}, []uint64{1000, 3000, 5000})
	now := start.Add(3 * 24 * time.Hour)
	growth, err := collectHistoryGrowth(db, historyGrowthOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(growth.Disks) != 1 || growth.Disks[0].Mount != "/" || growth.Disks[0].Samples != 3 || growth.Disks[0].UsedBytes != 500 || growth.Disks[0].TotalBytes != 1000 {
		t.Fatalf("disks = %+v", growth.Disks)
	}
	disk := growth.Disks[0]
	if disk.BytesPerDay == nil || math.Abs(*disk.BytesPerDay-200) > 1e-6 || disk.FullInDays == nil || math.Abs(*disk.FullInDays-2.5) > 1e-6 {
		t.Fatalf("rate %v, full in %v", disk.BytesPerDay, disk.FullInDays)
	}
	if len(growth.Dirs) != 1 || growth.Dirs[0].Root != "/var" || growth.Dirs[0].Snapshots != 3 || growth.Dirs[0].Note != "" {
		t.Fatalf("dirs = %+v", growth.Dirs)
	}
	if deltas := growth.Dirs[0].Directories; len(deltas) != 1 || deltas[0].Path != "lib/data" || deltas[0].Before != 1000 || deltas[0].After != 5000 {
		t.Fatalf("deltas = %+v", deltas)
	}

	var text bytes.Buffer
	writeHistoryGrowth(&text, growth, time.UTC)
	for _, want := range []string{"Range: 2026-10-06 12:00 to 2026-10-13 12:00", "FULL IN (ESTIMATE)", "+200 B/day", "about 3 days",
		"du /var: 2026-10-10 12:00 → 2026-10-12 12:00 (2.0 days, 3 snapshots)", "lib/data", "+2.0 KB/day", "total 1010 B → 4.9 KB (+3.9 KB)"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	var lines bytes.Buffer
	if err := writeHistoryGrowthJSON(&lines, growth); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(lines.String()), "\n") {
		var row struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, row.Kind)
	}
	if strings.Join(kinds, " ") != "disk directories" {
		t.Fatalf("JSON kinds = %q", kinds)
	}
}

func TestHistoryGrowthDoesNotEstimateWithoutGrowthOrSamples(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	shrinking := historyGrowthDB(t, start, []uint64{500, 400, 300}, []uint64{3000, 2000, 1000})
	growth, err := collectHistoryGrowth(shrinking, historyGrowthOptions{}, start.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if disk := growth.Disks[0]; disk.BytesPerDay == nil || *disk.BytesPerDay >= 0 || disk.FullInDays != nil {
		t.Fatalf("a shrinking disk: rate %v, full in %v", disk.BytesPerDay, disk.FullInDays)
	}
	var text bytes.Buffer
	writeHistoryGrowth(&text, growth, time.UTC)
	if !strings.Contains(text.String(), "-100 B/day") || !strings.Contains(text.String(), "-2.0 KB") {
		t.Errorf("shrinking text:\n%s", text.String())
	}

	// --to가 둘째 스냅샷 뒤면 점이 둘이라 기울기를 내지 않는다.
	two, err := collectHistoryGrowth(shrinking, historyGrowthOptions{to: start.Add(25 * time.Hour)}, start.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if disk := two.Disks[0]; disk.Samples != 2 || disk.BytesPerDay != nil || disk.FullInDays != nil {
		t.Fatalf("two samples: %+v", disk)
	}
	text.Reset()
	writeHistoryGrowth(&text, two, time.UTC)
	if !strings.Contains(text.String(), "too few samples (2)") {
		t.Errorf("two samples text:\n%s", text.String())
	}

	one, err := collectHistoryGrowth(shrinking, historyGrowthOptions{from: start.Add(47 * time.Hour)}, start.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Dirs) != 1 || one.Dirs[0].Snapshots != 1 || !strings.Contains(one.Dirs[0].Note, "one snapshot") {
		t.Fatalf("one snapshot: %+v", one.Dirs)
	}
}

func TestHistoryGrowthRootLimitsToOnePath(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db := historyGrowthDB(t, start, []uint64{100, 300}, []uint64{1000, 3000})
	growth, err := collectHistoryGrowth(db, historyGrowthOptions{root: "/home"}, start.Add(2*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(growth.Disks) != 0 || len(growth.Dirs) != 0 {
		t.Fatalf("--root /home = %+v", growth)
	}
	var text bytes.Buffer
	writeHistoryGrowth(&text, growth, time.UTC)
	if !strings.Contains(text.String(), "No disk or du snapshots in this range") {
		t.Errorf("empty text:\n%s", text.String())
	}
	if growth, err := collectHistoryGrowth(db, historyGrowthOptions{root: "/var"}, start.Add(2*24*time.Hour)); err != nil || len(growth.Disks) != 0 || len(growth.Dirs) != 1 {
		t.Fatalf("--root /var = %+v, %v", growth, err)
	}
}

func TestHistoryGrowthSkipsPartialScans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for index, data := range []uint64{1000, 9000, 3000} {
		report := snapshotHistoryReport(start.Add(time.Duration(index)*24*time.Hour), 100, data)
		if index == 0 {
			for position := range report.Results {
				if report.Results[position].Probe == duProbeID {
					report.Results[position].Metrics["partial"] = true
				}
			}
		}
		if err := recordSnapshot(path, report, time.Hour, []string{"/var"}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	growth, err := collectHistoryGrowth(db, historyGrowthOptions{}, start.Add(3*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if dirs := growth.Dirs[0]; dirs.Snapshots != 2 || len(dirs.Directories) != 1 || dirs.Directories[0].Before != 9000 || dirs.Directories[0].After != 3000 {
		t.Fatalf("a partial first scan must be skipped: %+v", dirs)
	}
}

func TestHistoryGrowthWindowDefaultsToSevenDays(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	from, to := historyGrowthWindowOf(historyGrowthOptions{}, now)
	if !to.Equal(now) || !from.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("default = %s .. %s", from, to)
	}
	end := now.Add(-24 * time.Hour)
	from, to = historyGrowthWindowOf(historyGrowthOptions{to: end}, now)
	if !to.Equal(end) || !from.Equal(end.Add(-7*24*time.Hour)) {
		t.Errorf("--to only = %s .. %s", from, to)
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	for _, args := range [][]string{{"--from", "yesterday", missing}, {missing, "extra"}} {
		if code := runHistoryGrowth(args); code != 2 {
			t.Errorf("%q: exit code %d, want 2", args, code)
		}
	}
}

func TestHistoryGrowthNamesWhyItDoesNotEstimate(t *testing.T) {
	long := "/private/var/folders/w6/" + strings.Repeat("x", 60)
	growth := historyGrowth{Disks: []historyGrowthDisk{
		{Mount: "/", Samples: 3, UsedBytes: 10, TotalBytes: 20},
		{Mount: long, Samples: 2, UsedBytes: 10, TotalBytes: 20},
	}}
	var text bytes.Buffer
	writeHistoryGrowth(&text, growth, time.UTC)
	if !strings.Contains(text.String(), "under 1 hour of records") || !strings.Contains(text.String(), "too few samples (2)") {
		t.Errorf("reasons:\n%s", text.String())
	}
	if strings.Contains(text.String(), long) || !strings.Contains(text.String(), "…") {
		t.Errorf("a long mount must be cut:\n%s", text.String())
	}
}
