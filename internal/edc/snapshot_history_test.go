package edc

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func snapshotHistoryReport(started time.Time, used, dataBytes uint64) Report {
	disks := []diskDetails{{Mount: "/", Device: "/dev/sda1", Used: used, Total: 1000, Percent: float64(used) / 10}}
	results := append([]Result{snapshotMemoryResult(started, infoMemory{Total: 1000, Available: 1000 - used}, nil)}, snapshotDiskResults(started, disks, nil, nil)...)
	results = append(results, Result{Probe: duProbeID, Status: StatusPass, Metrics: map[string]interface{}{
		"path": "/var", "bytes": dataBytes + 10, "complete": true,
		"largest_dirs": duLargestList{Complete: true, Dirs: []duLargeDir{{Path: "lib/data", Bytes: dataBytes, Entries: 3}, {Path: "log", Bytes: 10, Entries: 1}}},
	}})
	sortResults(results)
	return buildReport("test", started, map[string]interface{}{"since": (6 * time.Hour).String()}, results, false)
}

func snapshotHistoryCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSnapshotRecordsExportAndDiffLikeAFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "history.db")
	started := time.Now().Add(-time.Hour)
	reports := []Report{snapshotHistoryReport(started, 100, 1000), snapshotHistoryReport(started.Add(time.Minute), 300, 5000)}
	for _, report := range reports {
		if err := recordSnapshot(path, report, 6*time.Hour, []string{"/var"}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := snapshotHistoryCount(t, db, "SELECT count(*) FROM snapshot_metrics WHERE probe='info.disk:/' AND key='used_bytes'"); got != 2 {
		t.Fatalf("used_bytes rows = %d", got)
	}
	var grown int64
	if err := db.QueryRow("SELECT max(bytes)-min(bytes) FROM du_dirs WHERE root='/var' AND path='lib/data'").Scan(&grown); err != nil || grown != 4000 {
		t.Fatalf("lib/data grew %d, err %v", grown, err)
	}

	files := make([]string, len(reports))
	loaded := make([]Report, len(reports))
	for index, report := range reports {
		exported, err := exportSnapshot(db, report.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(exported.Results) != len(report.Results) || exported.Run.ID != report.Run.ID || exported.Tool.Version != "test" {
			t.Fatalf("exported = %+v", exported)
		}
		files[index] = filepath.Join(directory, fmt.Sprintf("%d.json", index))
		if err := writeSnapshotFile(files[index], exported, false); err != nil {
			t.Fatal(err)
		}
		if loaded[index], err = loadReport(files[index]); err != nil {
			t.Fatal(err)
		}
	}
	diff := diffReports(files[0], loaded[0], files[1], loaded[1])
	found := map[string]float64{}
	for _, entry := range diff.Entries {
		for _, metric := range entry.Metrics {
			if metric.Key == "used_bytes" && metric.Delta != nil {
				found[entry.Probe] = *metric.Delta
			}
		}
	}
	if found["info.memory"] != 200 || found["info.disk:/"] != 200 {
		t.Fatalf("used_bytes deltas = %v", found)
	}

	var list bytes.Buffer
	if err := printHistory(&list, db, "list", historyOptions{}, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(list.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "COMMAND") || strings.Fields(lines[1])[1] != "snapshot" || strings.Fields(lines[1])[6] != "3" {
		t.Fatalf("list:\n%s", list.String())
	}
	rows := historyJSONRows(t, db, "list", historyOptions{})
	if len(rows) != 2 || rows[0]["report"] != nil || string(rows[0]["du_paths"]) != `["/var"]` {
		t.Fatalf("list JSON = %s", mustHistoryJSON(t, rows))
	}
}

func TestSnapshotExportNamesTheWrongRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	recorder, err := newTopRecorder(path, "test", hostDetails{Hostname: "fixture"}, time.Second, "", "", false)
	if err != nil {
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
	if _, err := exportSnapshot(db, "missing"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing run: %v", err)
	}
	if _, err := exportSnapshot(db, recorder.runID); err == nil || !strings.Contains(err.Error(), "top") {
		t.Errorf("top run: %v", err)
	}
}

// 이전 edc가 만든 DB는 쓰기로 열 때 스냅샷 표만 더하고 버전 1을 유지한다. 읽기로 열면 파일을 바꾸지 않는다.
func TestHistoryMigratesAVersion1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := append(historyV1Statements(),
		fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID), "PRAGMA user_version=1",
		`INSERT INTO runs(id, command, hostname, started_ns, status, payload) VALUES ('old', 'top', 'fixture', 1, 'finished', '{"run_id":"old","command":"top"}')`)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(path)

	read, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exportSnapshot(read, "old"); err == nil || !strings.Contains(err.Error(), "snapshot --write") {
		t.Errorf("export from a version 1 database: %v", err)
	}
	if rows := historyJSONRows(t, read, "list", historyOptions{}); len(rows) != 1 {
		t.Errorf("version 1 runs = %s", mustHistoryJSON(t, rows))
	}
	read.Close()
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("reading changed a version 1 database")
	}

	if err := recordSnapshot(path, snapshotHistoryReport(time.Now(), 100, 1000), time.Hour, nil); err != nil {
		t.Fatal(err)
	}
	migrated, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	// 이전 edc는 application_id와 user_version 1만 확인한다. 둘이 그대로여야 이전 top -w가 이 DB를 계속 연다.
	if version := snapshotHistoryCount(t, migrated, "PRAGMA user_version"); version != 1 {
		t.Fatalf("user_version = %d; an earlier edc opens only version 1", version)
	}
	if application := snapshotHistoryCount(t, migrated, "PRAGMA application_id"); application != historyApplicationID {
		t.Fatalf("application_id = %d", application)
	}
	if runs := snapshotHistoryCount(t, migrated, "SELECT count(*) FROM runs"); runs != 2 {
		t.Fatalf("runs = %d, the old top run must stay", runs)
	}
}

func TestHistoryAddsOverviewIndexesToAnExistingSnapshotDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := append(historyV1Statements(),
		`CREATE TABLE snapshot_metrics (id INTEGER PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), time_ns INTEGER NOT NULL, probe TEXT NOT NULL, key TEXT NOT NULL, value REAL NOT NULL)`,
		`CREATE TABLE du_dirs (id INTEGER PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), time_ns INTEGER NOT NULL, root TEXT NOT NULL, path TEXT NOT NULL, bytes INTEGER NOT NULL)`,
		fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID), "PRAGMA user_version=1")
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	migrated, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for _, name := range []string{"snapshot_metrics_run_time", "du_dirs_run_time", "process_sample"} {
		if snapshotHistoryCount(t, migrated, "SELECT count(*) FROM sqlite_schema WHERE type='index' AND name=?", name) != 1 {
			t.Fatalf("missing index %s", name)
		}
	}
	for _, test := range []struct {
		query, index string
	}{
		{"SELECT * FROM snapshot_metrics WHERE run_id=? ORDER BY time_ns", "snapshot_metrics_run_time"},
		{"SELECT * FROM du_dirs WHERE run_id=? ORDER BY time_ns", "du_dirs_run_time"},
		{"SELECT s.id FROM top_samples s LEFT JOIN top_process_samples p ON p.sample_id=s.id WHERE s.run_id=?", "process_sample"},
	} {
		rows, err := migrated.Query("EXPLAIN QUERY PLAN "+test.query, "run")
		if err != nil {
			t.Fatal(err)
		}
		used := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			used = used || strings.Contains(detail, test.index)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !used {
			t.Fatalf("query did not use %s", test.index)
		}
	}
}

func TestHistoryWriteOpenDoesNotMigrateWhenOverviewIndexesExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	lock, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer lock.Exec("ROLLBACK")

	opened, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
}

func TestHistoryWriteOpenRestoresOnlyMissingOverviewIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP INDEX du_dirs_run_time"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	restored, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, name := range []string{"snapshot_metrics_run_time", "du_dirs_run_time", "process_sample"} {
		var count int
		if err := restored.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='index' AND name=?", name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s: count=%d err=%v", name, count, err)
		}
	}
}

func TestSnapshotNamesEachDuPathWhenThereAreSeveral(t *testing.T) {
	var names []string
	for _, section := range snapshotSections(time.Hour, []string{"/var", "/home"}, time.Minute) {
		if isDuProbe(section.name) {
			names = append(names, section.name)
		}
	}
	if strings.Join(names, " ") != "du.scan:/var du.scan:/home" {
		t.Fatalf("du sections = %q", names)
	}
	var one []string
	for _, section := range snapshotSections(time.Hour, []string{"/var"}, time.Minute) {
		if isDuProbe(section.name) {
			one = append(one, section.name)
		}
	}
	if strings.Join(one, " ") != duProbeID {
		t.Fatalf("one path = %q, want the old name", one)
	}
	var paths snapshotPaths
	if err := paths.Set(" "); err == nil {
		t.Error("an empty --du was accepted")
	}
}

// 두 writer가 스냅샷 표가 없는 DB를 동시에 처음 열어도 둘 다 열려야 한다. 늦은 쪽이 표를 다시 만들려다 실패하면 cron의 top -w나
// snapshot -w가 그 회차를 잃는다.
func TestHistoryWritersOpenTogetherWhileAddingTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(historyV1Statements(), fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID), "PRAGMA user_version=1") {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	errs := make(chan error, 16)
	for range 16 {
		go func() {
			db, err := openHistoryDB(path, true)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	for range 16 {
		if err := <-errs; err != nil {
			t.Fatalf("a writer failed while another added the tables: %v", err)
		}
	}
}
