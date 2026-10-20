package edc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// indexTestRoot points the default log directory at a temporary home, so the
// tests never touch the index of the user running them.
func indexTestRoot(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	root := defaultLogDirectory()
	if root == "" {
		t.Skip("no default log directory on this platform")
	}
	return root
}

func writeIndexRun(t *testing.T, root, name string, started time.Time) string {
	t.Helper()
	path := filepath.Join(root, "make", name+".log")
	writeHistoryFixture(t, path, historyFixture([]string{"make", name}, "/tmp", started, "exit", "1s", 0, 1))
	return path
}

func loadDefaultHistory(t *testing.T, cache logHistoryCache) logHistorySnapshot {
	t.Helper()
	started := time.Now()
	snapshot, err := loadLogHistory(logHistoryOptions{}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > logIndexBudget+3*time.Second {
		t.Fatalf("history took %v", elapsed)
	}
	return snapshot
}

func TestLogIndexBuildsAndFollowsTheLogs(t *testing.T) {
	root := indexTestRoot(t)
	now := time.Now()
	writeIndexRun(t, root, "a", now.Add(-3*time.Hour))
	removed := writeIndexRun(t, root, "b", now.Add(-2*time.Hour))
	snapshot := loadDefaultHistory(t, nil)
	if len(snapshot.Rows) != 2 || len(snapshot.Notices) != 0 || len(snapshot.Issues) != 0 {
		t.Fatalf("first build: rows=%d notices=%v issues=%v", len(snapshot.Rows), snapshot.Notices, snapshot.Issues)
	}
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(filepath.Join(root, logIndexName+suffix))
		if suffix != "" && os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("index file %q: %v %v", suffix, info, err)
		}
	}
	cache := logHistoryCache{}
	writeIndexRun(t, root, "c", now.Add(-time.Hour))
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	snapshot = loadDefaultHistory(t, cache)
	if len(snapshot.Rows) != 2 || snapshot.Rows[0].Command[1] != "c" || snapshot.Rows[1].Command[1] != "a" {
		t.Fatalf("incremental sync: %+v", snapshot.Rows)
	}
	again := loadDefaultHistory(t, cache)
	if len(again.Rows) != 2 || again.Rows[0].Started != snapshot.Rows[0].Started {
		t.Fatal("cached index rows differ")
	}
}

func TestLogIndexReadsTheLogsWhenTheBudgetRunsOut(t *testing.T) {
	root := indexTestRoot(t)
	for index, name := range []string{"a", "b", "c"} {
		writeIndexRun(t, root, name, time.Now().Add(-time.Duration(index+1)*time.Hour))
	}
	budget := logIndexBudget
	logIndexBudget = 0
	snapshot := loadDefaultHistory(t, nil)
	logIndexBudget = budget
	if len(snapshot.Rows) != 3 || len(snapshot.Notices) != 1 || !strings.Contains(snapshot.Notices[0], T("cli.log_history.index_building", 0, 3)[:10]) {
		t.Fatalf("rows=%d notices=%v", len(snapshot.Rows), snapshot.Notices)
	}
	if snapshot = loadDefaultHistory(t, nil); len(snapshot.Rows) != 3 || len(snapshot.Notices) != 0 {
		t.Fatalf("index did not finish: notices=%v", snapshot.Notices)
	}
}

func TestLogIndexMovesADamagedIndexAside(t *testing.T) {
	root := indexTestRoot(t)
	writeIndexRun(t, root, "a", time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(root, logIndexName), []byte(strings.Repeat("not a database ", 512)), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := loadDefaultHistory(t, nil)
	if len(snapshot.Rows) != 1 || len(snapshot.Notices) != 1 {
		t.Fatalf("damaged index: rows=%d notices=%v", len(snapshot.Rows), snapshot.Notices)
	}
	moved, _ := filepath.Glob(filepath.Join(root, logIndexName+".corrupt-*"))
	if len(moved) != 1 {
		t.Fatalf("damaged index not moved aside: %v (%s)", moved, snapshot.Notices[0])
	}
	if snapshot = loadDefaultHistory(t, nil); len(snapshot.Rows) != 1 || len(snapshot.Notices) != 0 {
		t.Fatalf("index not built again: %v", snapshot.Notices)
	}
}

func TestLogIndexReplacesAnotherSchema(t *testing.T) {
	root := indexTestRoot(t)
	writeIndexRun(t, root, "a", time.Now().Add(-time.Hour))
	loadDefaultHistory(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(root, logIndexName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if snapshot := loadDefaultHistory(t, nil); len(snapshot.Rows) != 1 || len(snapshot.Notices) != 0 {
		t.Fatalf("schema 99: rows=%d notices=%v", len(snapshot.Rows), snapshot.Notices)
	}
}

func TestLogIndexDoesNotWaitForAnotherWriter(t *testing.T) {
	root := indexTestRoot(t)
	writeIndexRun(t, root, "a", time.Now().Add(-2*time.Hour))
	loadDefaultHistory(t, nil)
	db, err := sql.Open("sqlite", filepath.Join(root, logIndexName)+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer connection.ExecContext(context.Background(), "ROLLBACK")
	writeIndexRun(t, root, "b", time.Now().Add(-time.Hour))
	started := time.Now()
	snapshot := loadDefaultHistory(t, nil)
	if len(snapshot.Rows) != 2 || len(snapshot.Notices) != 1 || time.Since(started) > 2*time.Second {
		t.Fatalf("held lock: rows=%d notices=%v took %v", len(snapshot.Rows), snapshot.Notices, time.Since(started))
	}
}

func TestLogIndexFallsBackInAReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := indexTestRoot(t)
	writeIndexRun(t, root, "a", time.Now().Add(-time.Hour))
	if err := os.Chmod(root, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(root, 0700) })
	var stdout, stderr strings.Builder
	code := runLogWithStreams([]string{"history"}, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "make a") || !strings.Contains(stderr.String(), T("cli.log_history.index_unavailable", "")[:10]) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestLogIndexMarksRunningAttemptsOnEveryRead(t *testing.T) {
	root := indexTestRoot(t)
	full := historyFixture([]string{"make", "run"}, "/tmp", time.Now().Add(-time.Minute), "exit", "1s", 0, 1)
	path := filepath.Join(root, "make", "run.log")
	writeHistoryFixture(t, path, full[:strings.Index(full, "=== edc log end")])
	lock := lockHistoryFixture(t, path+".lock")
	cache := logHistoryCache{}
	if snapshot := loadDefaultHistory(t, cache); len(snapshot.Rows) != 1 || snapshot.Rows[0].Outcome != historyOutcomeRunning || len(snapshot.Notices) != 0 {
		t.Fatalf("running: %+v %v", snapshot.Rows, snapshot.Notices)
	}
	unlockLogFile(lock)
	if snapshot := loadDefaultHistory(t, cache); snapshot.Rows[0].Outcome != historyOutcomeUnknown {
		t.Fatalf("cached rows kept RUNNING after the lock was released: %s", snapshot.Rows[0].Outcome)
	}
}

func TestLogIndexKeepsResourcesAndDamagedState(t *testing.T) {
	exit := 3
	row := logHistoryAttempt{Key: "k", CWD: "/tmp", Path: "/x.log", Display: "full", Outcome: historyOutcomeFail, Command: []string{"make"}, Started: time.Unix(10, 5).UTC(), Ended: time.Unix(12, 0).UTC(), Duration: time.Second, Exit: &exit, Attempt: 2, Ordinal: 4, damaged: true, open: true,
		Resources: &logHistoryResources{CPU: time.Second, MaxRSS: 7, Samples: 2, MemPeak: 9, CPUPeak: 50, DiskRead: 1, Scope: logResourceScopeExec, Container: "abc", cpu: true, disk: true}}
	got := newLogIndexRow(row).attempt()
	if got.Key != row.Key || !got.damaged || !got.open || *got.Exit != 3 || got.Ordinal != 4 || !got.Resources.cpu || !got.Resources.disk || got.Resources.Container != "abc" || !got.Started.Equal(row.Started) {
		t.Fatalf("round trip: %+v %+v", got, got.Resources)
	}
}
