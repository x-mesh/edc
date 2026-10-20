package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var historyTrendZone = time.FixedZone("KST", 9*3600)

func historyTrendTopDB(t *testing.T, samples map[time.Time]any) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs(id, command, hostname, started_ns, status, payload) VALUES ('top1', 'top', 'fixture', 0, 'finished', '{}')`); err != nil {
		t.Fatal(err)
	}
	for at, value := range samples {
		if _, err := db.Exec("INSERT INTO top_samples(run_id, time_ns, payload, memory_pct) VALUES ('top1', ?, '{}', ?)", at.UnixNano(), value); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	read, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { read.Close() })
	return read, path
}

func historyTrendCollect(t *testing.T, db *sql.DB, options historyTrendOptions) []historyTrendBucket {
	t.Helper()
	var buckets []historyTrendBucket
	if err := historyTrendBuckets(db, options, historyTrendZone, func(bucket historyTrendBucket) error {
		buckets = append(buckets, bucket)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return buckets
}

func TestHistoryTrendGroupsTopSamplesByLocalHour(t *testing.T) {
	nine := time.Date(2026, 10, 20, 9, 0, 0, 0, historyTrendZone)
	samples := map[time.Time]any{nine.Add(70 * time.Minute): 50.0, nine.Add(30 * time.Minute): nil}
	for index := 1; index <= 20; index++ {
		samples[nine.Add(time.Duration(index)*time.Minute)] = float64(index)
	}
	db, _ := historyTrendTopDB(t, samples)
	buckets := historyTrendCollect(t, db, historyTrendOptions{metric: "memory_pct", by: "hour"})
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v", buckets)
	}
	first := buckets[0]
	if !first.Start.Equal(nine) || first.Count != 20 || first.Min != 1 || first.Avg != 10.5 || first.P95 != 19 || first.Max != 20 {
		t.Fatalf("first = %+v (a NULL value must not count)", first)
	}
	if !buckets[1].Start.Equal(nine.Add(time.Hour)) || buckets[1].Count != 1 || buckets[1].P95 != 50 {
		t.Fatalf("second = %+v", buckets[1])
	}

	limited := historyTrendCollect(t, db, historyTrendOptions{metric: "memory_pct", by: "day", from: nine.Add(10 * time.Minute), to: nine.Add(time.Hour)})
	if len(limited) != 1 || limited[0].Count != 11 || limited[0].Min != 10 {
		t.Fatalf("--from includes and --to excludes: %+v", limited)
	}
	if other := historyTrendCollect(t, db, historyTrendOptions{metric: "memory_pct", by: "day", run: "other"}); len(other) != 0 {
		t.Fatalf("--run other = %+v", other)
	}

	var text bytes.Buffer
	if err := writeHistoryTrend(&text, db, historyTrendOptions{metric: "memory_pct", by: "hour"}, false, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory_pct by hour, local time +09:00", "START", "2026-10-20 09:00", "10.5%", "19.0%", "2026-10-20 10:00"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, text.String())
		}
	}
	var lines bytes.Buffer
	if err := writeHistoryTrend(&lines, db, historyTrendOptions{metric: "memory_pct", by: "hour"}, true, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	var decoded historyTrendBucket
	if err := json.Unmarshal(bytes.SplitN(lines.Bytes(), []byte("\n"), 2)[0], &decoded); err != nil || decoded.P95 != 19 || decoded.By != "hour" || decoded.Probe != "" {
		t.Fatalf("JSON line = %s, err %v", lines.String(), err)
	}
	var empty bytes.Buffer
	if err := writeHistoryTrend(&empty, db, historyTrendOptions{metric: "memory_pct", by: "day", run: "other"}, false, historyTrendZone); err != nil || strings.TrimSpace(empty.String()) != "No values in this range" {
		t.Fatalf("empty = %q, err %v", empty.String(), err)
	}
}

func TestHistoryBucketStartUsesLocalDaysAndMondays(t *testing.T) {
	for _, test := range []struct {
		at, want time.Time
		by       string
	}{
		{time.Date(2026, 10, 20, 23, 59, 0, 0, historyTrendZone), time.Date(2026, 10, 20, 0, 0, 0, 0, historyTrendZone), "day"},
		{time.Date(2026, 10, 20, 14, 59, 0, 0, time.UTC), time.Date(2026, 10, 20, 0, 0, 0, 0, historyTrendZone), "day"},
		{time.Date(2026, 10, 20, 15, 1, 0, 0, time.UTC), time.Date(2026, 10, 21, 0, 0, 0, 0, historyTrendZone), "day"},
		{time.Date(2026, 10, 18, 22, 0, 0, 0, historyTrendZone), time.Date(2026, 10, 12, 0, 0, 0, 0, historyTrendZone), "week"},
		{time.Date(2026, 10, 19, 0, 0, 0, 0, historyTrendZone), time.Date(2026, 10, 19, 0, 0, 0, 0, historyTrendZone), "week"},
		{time.Date(2026, 10, 20, 9, 45, 0, 0, historyTrendZone), time.Date(2026, 10, 20, 9, 0, 0, 0, historyTrendZone), "hour"},
	} {
		if got := historyBucketStart(test.at, test.by, historyTrendZone); !got.Equal(test.want) {
			t.Errorf("%s %s: got %s, want %s", test.by, test.at, got, test.want)
		}
	}
}

func TestHistoryTrendReadsSnapshotMetricsAndNamesWhatExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	day := time.Date(2026, 10, 19, 12, 0, 0, 0, historyTrendZone)
	for index, used := range []uint64{100, 300, 500} {
		if err := recordSnapshot(path, snapshotHistoryReport(day.Add(time.Duration(index)*12*time.Hour), used, 1000), time.Hour, []string{"/var"}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	buckets := historyTrendCollect(t, db, historyTrendOptions{probe: "info.disk:/", metric: "used_bytes", by: "day"})
	if len(buckets) != 2 || buckets[0].Count != 1 || buckets[0].Max != 100 || buckets[1].Count != 2 || buckets[1].Min != 300 || buckets[1].Max != 500 {
		t.Fatalf("buckets = %+v", buckets)
	}
	var text bytes.Buffer
	if err := writeHistoryTrend(&text, db, historyTrendOptions{probe: "du.scan", metric: "bytes", by: "week"}, false, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "du.scan bytes by week") || !strings.Contains(text.String(), "1010 B") {
		t.Errorf("du trend:\n%s", text.String())
	}
	err = historyTrendBuckets(db, historyTrendOptions{probe: "info.disk:/", metric: "free_bytes", by: "day"}, historyTrendZone, func(historyTrendBucket) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "used_bytes") || !strings.Contains(err.Error(), "total_bytes") {
		t.Errorf("unknown key: %v", err)
	}
	err = historyTrendBuckets(db, historyTrendOptions{probe: "info.disk:/data", metric: "used_bytes", by: "day"}, historyTrendZone, func(historyTrendBucket) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "info.disk:/,") {
		t.Errorf("unknown probe: %v", err)
	}
}

func TestHistoryTrendSaysWhenNoSnapshotIsRecorded(t *testing.T) {
	db, _ := historyTrendTopDB(t, nil)
	err := historyTrendBuckets(db, historyTrendOptions{probe: "info.memory", metric: "used_bytes", by: "day"}, historyTrendZone, func(historyTrendBucket) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "snapshot --write") {
		t.Fatalf("a database with top runs only: %v", err)
	}
}

func TestHistoryTrendCLIRejectsWrongOptionsBeforeOpening(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	for _, args := range [][]string{
		{"--by", "month", "--metric", "memory_pct", missing},
		{missing},
		{"--metric", "used_bytes", missing},
		{"--metric", "memory_pct", "--from", "yesterday", missing},
		{"--metric", "memory_pct", missing, "extra"},
	} {
		if code := runHistoryTrend(args); code != 2 {
			t.Errorf("%q: exit code %d, want 2", args, code)
		}
	}
	if code := runHistoryTrend([]string{"--metric", "memory_pct", missing}); code != 1 {
		t.Errorf("a missing database: exit code %d, want 1", code)
	}
}
