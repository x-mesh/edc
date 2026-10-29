package edc

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyPruneFixture는 지울 실행과 남길 실행을 섞은 DB다. 오래된 snapshot, 최근 snapshot, 오래된 top, 오래전에 시작했지만
// 방금 표본을 쓴 top이 있다.
func historyPruneFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "h.db")
	now := time.Now()
	ids := map[string]string{}
	for name, started := range map[string]time.Time{"old_snapshot": now.Add(-40 * 24 * time.Hour), "new_snapshot": now.Add(-time.Hour)} {
		report := snapshotHistoryReport(started, 100, 1000)
		if err := recordSnapshot(path, report, time.Hour, nil); err != nil {
			t.Fatal(err)
		}
		ids[name] = report.Run.ID
	}
	for _, name := range []string{"old_top", "running_top"} {
		recorder := topHistoryFixture(t, path)
		if err := recorder.Record(historyFixtureSample(1, 50, topProcess{PID: 7, Command: "nginx"})); err != nil {
			t.Fatal(err)
		}
		ids[name] = recorder.runID
		if err := recorder.Close("finished"); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	longAgo := now.Add(-20 * 24 * time.Hour).UnixNano()
	oldSnapshot := now.Add(-40 * 24 * time.Hour).UnixNano()
	statements := []struct {
		query string
		args  []any
	}{
		{"UPDATE runs SET started_ns = ?, ended_ns = ? WHERE id = ?", []any{oldSnapshot, oldSnapshot, ids["old_snapshot"]}},
		{"UPDATE snapshot_results SET time_ns = ? WHERE run_id = ?", []any{oldSnapshot, ids["old_snapshot"]}},
		{"UPDATE runs SET started_ns = ?, ended_ns = ? WHERE id IN (?, ?)", []any{longAgo, longAgo, ids["old_top"], ids["running_top"]}},
		{"UPDATE top_samples SET time_ns = ? WHERE run_id = ?", []any{longAgo, ids["old_top"]}},
		{"UPDATE top_samples SET time_ns = ? WHERE run_id = ?", []any{now.Add(-time.Minute).UnixNano(), ids["running_top"]}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return path, ids
}

func runHistoryPruneTest(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := runHistoryPrune(args, strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func historyRunIDs(t *testing.T, path string) map[string]bool {
	t.Helper()
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT id FROM runs")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids[id] = true
	}
	return ids
}

func TestHistoryPruneDeletesIdleRunsAndTheirRows(t *testing.T) {
	path, ids := historyPruneFixture(t)
	before, _ := os.ReadFile(path)
	code, stdout, stderr := runHistoryPruneTest("--older-than", "7d", "--dry-run", path)
	if code != 0 || stderr != "" || !strings.Contains(stdout, T("history.prune.dry_plan", 2, 1, 1, 5)) || !strings.Contains(stdout, T("history.prune.dry_note")) {
		t.Fatalf("dry run: %d %q %q", code, stdout, stderr)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("--dry-run changed the database")
	}
	if code, _, stderr := runHistoryPruneTest("--older-than", "7d", path); code != 2 || !strings.Contains(stderr, T("history.prune.yes_required")) {
		t.Fatalf("deleted without a terminal or --yes: %d %q", code, stderr)
	}
	code, stdout, stderr = runHistoryPruneTest("--older-than", "7d", "--yes", path)
	if code != 0 || stderr != "" || !strings.Contains(stdout, T("history.prune.done", 2, 5)) {
		t.Fatalf("prune: %d %q %q", code, stdout, stderr)
	}
	left := historyRunIDs(t, path)
	if left[ids["old_snapshot"]] || left[ids["old_top"]] || !left[ids["new_snapshot"]] || !left[ids["running_top"]] {
		t.Fatalf("runs left: %v ids %v", left, ids)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range historyRunTables {
		for _, removed := range []string{ids["old_snapshot"], ids["old_top"]} {
			if count := snapshotHistoryCount(t, db, "SELECT count(*) FROM "+table+" WHERE run_id = ?", removed); count != 0 {
				t.Fatalf("%s kept %d rows of a deleted run", table, count)
			}
		}
	}
	if count := snapshotHistoryCount(t, db, "SELECT count(*) FROM snapshot_metrics WHERE run_id = ?", ids["new_snapshot"]); count == 0 {
		t.Fatal("rows of a kept run were deleted")
	}
}

func TestHistoryPruneFiltersByCommandAndVacuums(t *testing.T) {
	path, ids := historyPruneFixture(t)
	code, stdout, _ := runHistoryPruneTest("--older-than", "7d", "--command", "snapshot", "--yes", "--vacuum", path)
	if code != 0 || !strings.Contains(stdout, T("history.prune.done", 1, 3)) || !strings.Contains(stdout, strings.Split(T("history.prune.vacuumed", "", ""), " ")[0]) {
		t.Fatalf("snapshot prune: %d %q", code, stdout)
	}
	left := historyRunIDs(t, path)
	if left[ids["old_snapshot"]] || !left[ids["old_top"]] {
		t.Fatalf("--command snapshot touched the top run: %v", left)
	}
	if code, stdout, _ := runHistoryPruneTest("--older-than", "90d", path); code != 0 || !strings.Contains(stdout, T("history.prune.none")) {
		t.Fatalf("nothing to prune: %d %q", code, stdout)
	}
}

func TestHistoryPruneNeverCreatesADatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if code, _, _ := runHistoryPruneTest("--older-than", "1d", "--yes", missing); code != 1 {
		t.Fatalf("code %d for a missing database", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("prune created a database")
	}
	for _, args := range [][]string{{}, {"--older-than", "0d"}, {"--older-than", "1d", "--command", "ls"}} {
		if code, _, _ := runHistoryPruneTest(args...); code != 2 {
			t.Fatalf("%v accepted", args)
		}
	}
}

// 이전 edc가 만든 DB에는 스냅샷 표가 없다. 개수를 세기만 하는 --dry-run은 그 DB를 바꾸지 않아야 한다.
func TestHistoryPruneCountsADatabaseWithoutSnapshotTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-40 * 24 * time.Hour).UnixNano()
	for _, statement := range append(historyV1Statements(), fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID), "PRAGMA user_version=1",
		fmt.Sprintf(`INSERT INTO runs(id, command, hostname, started_ns, ended_ns, status, payload) VALUES ('old', 'top', 'fixture', %d, %d, 'finished', '{}')`, long, long)) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(path)
	if code, stdout, stderr := runHistoryPruneTest("--older-than", "7d", "--dry-run", path); code != 0 || !strings.Contains(stdout, T("history.prune.dry_plan", 1, 1, 0, 0)) {
		t.Fatalf("dry run on an old database: %d %q %q", code, stdout, stderr)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("--dry-run changed an old database")
	}
	if code, _, stderr := runHistoryPruneTest("--older-than", "7d", "--yes", path); code != 0 || stderr != "" || len(historyRunIDs(t, path)) != 0 {
		t.Fatalf("prune on an old database: %d %q", code, stderr)
	}
}

// --retain은 기록하면서 오래 쉰 top 실행만 지운다. snapshot 실행과 최근에 기록한 실행은 남긴다.
func TestTopRetainDeletesOnlyIdleTopRuns(t *testing.T) {
	path, ids := historyPruneFixture(t)
	if code := runTop([]string{"--interval", "200ms", "--count", "1", "--no-header", "-w", path, "--retain", "7d"}, "test"); code != 0 {
		t.Fatalf("top --retain returned %d", code)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, kept := range map[string]bool{"old_top": false, "running_top": true, "old_snapshot": true, "new_snapshot": true} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM runs WHERE id = ?", ids[name]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (count == 1) != kept {
			t.Errorf("%s kept = %v, want %v", name, count == 1, kept)
		}
	}
	var orphans int
	if err := db.QueryRow("SELECT count(*) FROM top_process_samples WHERE run_id = ?", ids["old_top"]).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("process rows of the deleted run = %d, %v", orphans, err)
	}
}

func TestTopRetainRejectsBadAgeAndNoDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	for _, args := range [][]string{
		{"--count", "1", "--retain", "7d"},
		{"--count", "1", "-w", path, "--retain", "soon"},
	} {
		if code := runTop(args, "test"); code != 2 {
			t.Errorf("top %v returned %d, want 2", args, code)
		}
	}
	if err := pruneHistoryRetention(path, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retention must not create a database: %v", err)
	}
}

// 샘플 간격이 보존 기간보다 길면 기록 중인 실행의 마지막 샘플도 보존 기간보다 오래되어 지워질 수 있다. 실제 기준은
// 샘플 두 번 사이보다 짧아지지 않는다. 대시보드는 실행 중에 간격을 topMaxInterval까지 늘릴 수 있다.
func TestHistoryRetentionAgeOutlastsTwoSamples(t *testing.T) {
	for _, tt := range []struct{ retain, interval, want time.Duration }{
		{7 * 24 * time.Hour, time.Second, 7 * 24 * time.Hour},
		{time.Minute, time.Second, 2 * topMaxInterval},
		{time.Hour, 2 * time.Hour, 4 * time.Hour},
	} {
		if got := historyRetentionAge(tt.retain, tt.interval); got != tt.want {
			t.Errorf("retain %s, interval %s: age %s, want %s", tt.retain, tt.interval, got, tt.want)
		}
	}
}
