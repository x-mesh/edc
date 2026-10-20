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

type historyProcessFixture struct {
	pid     int
	started string
	command string
	rss     float64
}

type historyProcessFixtureDB struct {
	t  *testing.T
	db *sql.DB
}

func newHistoryProcessFixtureDB(t *testing.T) (*historyProcessFixtureDB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, run := range []struct{ id, payload string }{{"filtered", `{"process_filter":"pg"}`}, {"unfiltered", `{"process_filter":""}`}} {
		if _, err := db.Exec(`INSERT INTO runs(id, command, hostname, started_ns, status, payload) VALUES (?, 'top', 'fixture', 0, 'finished', ?)`, run.id, run.payload); err != nil {
			t.Fatal(err)
		}
	}
	return &historyProcessFixtureDB{t: t, db: db}, path
}

func (fixture *historyProcessFixtureDB) sample(run string, at time.Time, processes ...historyProcessFixture) {
	fixture.t.Helper()
	result, err := fixture.db.Exec("INSERT INTO top_samples(run_id, time_ns, payload) VALUES (?, ?, '{}')", run, at.UnixNano())
	if err != nil {
		fixture.t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	for _, process := range processes {
		var started any
		if process.started != "" {
			started = process.started
		}
		if _, err := fixture.db.Exec(`INSERT INTO top_process_samples(sample_id, run_id, time_ns, started, command, command_folded, payload, pid, rss_bytes)
			VALUES (?, ?, ?, ?, ?, ?, '{}', ?, ?)`, id, run, at.UnixNano(), started, process.command, strings.ToLower(process.command), process.pid, process.rss); err != nil {
			fixture.t.Fatal(err)
		}
	}
}

func historyProcessTrendFixture(t *testing.T) (*sql.DB, time.Time) {
	t.Helper()
	fixture, _ := newHistoryProcessFixtureDB(t)
	nine := time.Date(2026, 10, 20, 9, 0, 0, 0, historyTrendZone)
	a, b, c := "2026-10-19T00:00:00Z", "2026-10-19T01:00:00Z", "2026-10-20T00:02:30Z"
	fixture.sample("filtered", nine, historyProcessFixture{10, a, "pg", 100}, historyProcessFixture{11, b, "pg", 50}, historyProcessFixture{99, "", "nginx", 5})
	fixture.sample("filtered", nine.Add(time.Minute), historyProcessFixture{10, a, "pg", 110}, historyProcessFixture{11, b, "pg", 60})
	fixture.sample("filtered", nine.Add(2*time.Minute), historyProcessFixture{99, "", "nginx", 5})
	fixture.sample("filtered", nine.Add(3*time.Minute), historyProcessFixture{12, c, "pg", 120})
	// 프로세스를 읽지 못한 표본은 seen의 분모가 아니다.
	fixture.sample("filtered", nine.Add(30*time.Minute))
	fixture.sample("unfiltered", nine.Add(time.Hour), historyProcessFixture{12, c, "pg", 130})
	return fixture.db, nine
}

func TestHistoryProcessTrendSumsMatchingProcessesPerSample(t *testing.T) {
	db, nine := historyProcessTrendFixture(t)
	filter, _ := parseTopProcessFilter("PG")
	var buckets []historyTrendBucket
	summary, err := historyTrendBuckets(db, historyTrendOptions{metric: "rss_bytes", by: "hour", process: filter}, historyTrendZone, func(bucket historyTrendBucket) error {
		buckets = append(buckets, bucket)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Unfiltered || len(buckets) != 2 {
		t.Fatalf("summary = %+v, buckets = %+v", summary, buckets)
	}
	first := buckets[0]
	if !first.Start.Equal(nine) || first.Count != 3 || first.Samples != 4 || first.Procs != 3 || first.Min != 120 || first.Max != 170 || first.Process != "PG" {
		t.Fatalf("first = %+v", first)
	}
	if first.Restarts == nil || *first.Restarts != 1 {
		t.Fatalf("pid 12 replaced 10 and 11 after a sample without pg: %+v", first.Restarts)
	}
	if second := buckets[1]; second.Count != 1 || second.Samples != 1 || second.Restarts != nil {
		t.Fatalf("an unfiltered run cannot count restarts: %+v", second)
	}

	var text, notices bytes.Buffer
	if err := writeHistoryTrend(&text, &notices, db, historyTrendOptions{metric: "rss_bytes", by: "hour", process: filter}, false, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "rss_bytes of PG by hour") || !strings.Contains(lines[1], "SEEN") ||
		!strings.Contains(lines[2], "75%") || !strings.Contains(lines[3], " - ") {
		t.Fatalf("text = %q", text.String())
	}
	if !strings.Contains(notices.String(), "edc top -w --process PG") {
		t.Fatalf("notices = %q", notices.String())
	}
}

func TestHistoryProcessTrendEachAndPID(t *testing.T) {
	db, _ := historyProcessTrendFixture(t)
	filter, _ := parseTopProcessFilter("pg")
	var lines bytes.Buffer
	if err := writeHistoryTrend(&lines, &bytes.Buffer{}, db, historyTrendOptions{metric: "rss_bytes", by: "hour", process: filter, each: true}, true, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	var rows []historyTrendBucket
	for _, line := range strings.Split(strings.TrimSpace(lines.String()), "\n") {
		var row historyTrendBucket
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if len(rows) != 4 || rows[0].PID != 10 || rows[0].Count != 2 || rows[0].Samples != 4 || rows[1].PID != 11 || rows[2].PID != 12 || rows[3].PID != 12 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Started == nil || !rows[0].Started.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("started = %v", rows[0].Started)
	}

	byPID, _ := parseTopProcessFilter("11")
	var buckets []historyTrendBucket
	if _, err := historyTrendBuckets(db, historyTrendOptions{metric: "rss_bytes", by: "day", process: byPID}, historyTrendZone, func(bucket historyTrendBucket) error {
		buckets = append(buckets, bucket)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Count != 2 || buckets[0].Samples != 5 || buckets[0].Max != 60 {
		t.Fatalf("pid 11 = %+v", buckets)
	}

	missing, _ := parseTopProcessFilter("redis")
	var empty bytes.Buffer
	if err := writeHistoryTrend(&empty, &bytes.Buffer{}, db, historyTrendOptions{metric: "rss_bytes", by: "day", process: missing}, false, historyTrendZone); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty.String(), "No samples of redis") {
		t.Fatalf("empty = %q", empty.String())
	}
}

func TestHistoryGrowthProcessesFlagsSteadyRSSGrowth(t *testing.T) {
	fixture, _ := newHistoryProcessFixtureDB(t)
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	const mb = 1 << 20
	for index := 0; index <= 12; index++ {
		at := start.Add(time.Duration(index) * time.Hour)
		processes := []historyProcessFixture{{20, "2026-10-19T00:00:00Z", "worker", float64(100*mb + index*10*mb)}, {21, "2026-10-19T00:00:00Z", "worker", 50 * mb}}
		if index < 2 {
			processes = append(processes, historyProcessFixture{22, "", "worker", 10 * mb})
		}
		fixture.sample("filtered", at, processes...)
	}
	filter, _ := parseTopProcessFilter("worker")
	growth, err := collectHistoryGrowth(fixture.db, historyGrowthOptions{process: filter}, start.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(growth.Processes) != 3 {
		t.Fatalf("processes = %+v", growth.Processes)
	}
	leaky, flat, few := growth.Processes[0], growth.Processes[1], growth.Processes[2]
	if leaky.PID != 20 || !leaky.LeakSuspect || leaky.BytesPerDay == nil || math.Abs(*leaky.BytesPerDay-240*mb) > 1 || *leaky.Fit < 0.99 || leaky.Samples != 13 {
		t.Fatalf("leaky = %+v", leaky)
	}
	if flat.PID != 21 || flat.LeakSuspect || *flat.BytesPerDay != 0 {
		t.Fatalf("flat = %+v", flat)
	}
	if few.PID != 22 || few.BytesPerDay != nil || historyGrowthProcessNote(few) != "too few samples (2)" {
		t.Fatalf("few = %+v", few)
	}

	var text bytes.Buffer
	writeHistoryGrowth(&text, growth, time.UTC)
	if !strings.Contains(text.String(), "possible leak (fit 1.00)") || !strings.Contains(text.String(), "+240.0 MB/day") || strings.Contains(text.String(), "without --process") {
		t.Fatalf("text = %q", text.String())
	}
}

func TestHistoryProcessNamesOrdersBySamples(t *testing.T) {
	fixture, _ := newHistoryProcessFixtureDB(t)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fixture.sample("filtered", at, historyProcessFixture{1, "", "nginx", 1}, historyProcessFixture{2, "", "Postgres", 1}, historyProcessFixture{3, "", "a,b", 1})
	fixture.sample("filtered", at.Add(time.Second), historyProcessFixture{2, "", "Postgres", 1}, historyProcessFixture{4, "", "postgres", 1})
	names, err := historyProcessNames(fixture.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0].Samples != 3 || !strings.EqualFold(names[0].Name, "postgres") || names[1].Name != "nginx" {
		t.Fatalf("names = %+v", names)
	}
}

func TestHistoryProcessTrendCountsRestartsAcrossSamples(t *testing.T) {
	fixture, _ := newHistoryProcessFixtureDB(t)
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, historyTrendZone)
	a, b, c, d := historyProcessFixture{1, "a", "app", 1}, historyProcessFixture{2, "b", "app", 1}, historyProcessFixture{3, "c", "app", 1}, historyProcessFixture{4, "d", "app", 1}
	// a가 끝난 뒤 다음 표본에서 c가 시작한다. 그 뒤 d가 더 뜬 것은 바뀐 프로세스가 아니라 늘어난 프로세스다.
	for index, processes := range [][]historyProcessFixture{{a, b}, {b}, {b, c}, {b, c, d}} {
		fixture.sample("filtered", at.Add(time.Duration(index)*time.Minute), processes...)
	}
	filter, _ := parseTopProcessFilter("app")
	var buckets []historyTrendBucket
	if _, err := historyTrendBuckets(fixture.db, historyTrendOptions{metric: "rss_bytes", by: "day", process: filter}, historyTrendZone, func(bucket historyTrendBucket) error {
		buckets = append(buckets, bucket)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Restarts == nil || *buckets[0].Restarts != 1 || buckets[0].Procs != 4 {
		t.Fatalf("buckets = %+v", buckets)
	}
}
