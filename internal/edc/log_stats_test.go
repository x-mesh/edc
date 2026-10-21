package edc

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogHistoryRollupAddsUpAcrossPrunes(t *testing.T) {
	testRollup := func(t *testing.T, now time.Time) {
		t.Helper()
		root := indexTestRoot(t)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		day := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -2)
		exit := 0
		row := func(minute int, outcome logHistoryOutcome, duration time.Duration) logHistoryAttempt {
			started := day.Add(time.Duration(minute) * time.Minute)
			return logHistoryAttempt{Key: "k", CWD: "/a", Started: started, Ended: started.Add(duration), Duration: duration, Outcome: outcome, Exit: &exit, Resources: &logHistoryResources{MaxRSS: uint64(minute) << 20, CPU: duration, cpu: true}}
		}
		if err := saveLogHistoryRollup(root, []logHistoryAttempt{row(1, historyOutcomeSuccess, time.Second), row(2, historyOutcomeFail, 5*time.Second)}); err != nil {
			t.Fatal(err)
		}
		if err := saveLogHistoryRollup(root, []logHistoryAttempt{row(3, historyOutcomeSuccess, 3*time.Second)}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, logStatsName))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("stats file: %v %v", info, err)
		}
		archive, err := loadLogHistoryArchive(root, historyTrendStart(now))
		if err != nil || len(archive["k"]) != 1 {
			t.Fatalf("archive %v err %v", archive, err)
		}
		got := archive["k"][0]
		if got.Runs != 3 || got.Failed != 1 || got.Successes != 2 || got.DurationSum != 4*time.Second || got.DurationMax != 3*time.Second || got.MemoryMax != 3<<20 || got.CPUSum != 9*time.Second {
			t.Fatalf("merged day: %+v", got)
		}
		group := logHistoryGroup{Key: "k", Archive: archive["k"], Rows: []logHistoryAttempt{row(10, historyOutcomeSuccess, 2*time.Second)}}
		trend := historyTrend(group, now, false)
		if !strings.Contains(trend, T("cli.log_history.summary_fail", 1)) || !strings.Contains(trend, "2s") || strings.Count(trend, "█") != 1 {
			t.Fatalf("trend %q", trend)
		}
		if historyTrend(logHistoryGroup{Rows: []logHistoryAttempt{row(0, historyOutcomeSuccess, time.Second)}}, now.AddDate(0, 0, 30), false) != "" {
			t.Fatal("runs outside the window produced a trend")
		}
	}
	for _, at := range []struct {
		name           string
		hour           int
		minute, second int
	}{
		{name: "before midnight", hour: 23, minute: 58, second: 30},
		{name: "after midnight", hour: 0, minute: 0, second: 30},
	} {
		t.Run(at.name, func(t *testing.T) {
			previous := time.Local
			t.Cleanup(func() { time.Local = previous })
			time.Local = time.FixedZone("rollup-test", 0)
			now := time.Date(2026, 10, 21, at.hour, at.minute, at.second, 0, time.Local)
			testRollup(t, now)
		})
	}
	testRollup(t, time.Now())
}

func TestLogPruneKeepsADailySummaryOfDeletedRuns(t *testing.T) {
	root := indexTestRoot(t)
	old := writeIndexRun(t, root, "a", time.Now().Add(-26*time.Hour))
	var stdout, stderr strings.Builder
	code := runLogWithStreams([]string{"prune", "--older-than", "1d", "--yes"}, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 0 || stderr.String() != "" {
		t.Fatalf("prune: %d %q %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old run not deleted")
	}
	snapshot := loadDefaultHistory(t, nil)
	key, _ := commandKey([]string{"make", "a"})
	if len(snapshot.Rows) != 0 || len(snapshot.Archive[key]) != 1 || snapshot.Archive[key][0].Runs != 1 {
		t.Fatalf("archive after prune: rows=%d archive=%v notices=%v", len(snapshot.Rows), snapshot.Archive, snapshot.Notices)
	}
}

func TestLogStatsOfAnotherSchemaIsKeptAndOnlyHidden(t *testing.T) {
	root := indexTestRoot(t)
	writeIndexRun(t, root, "a", time.Now().Add(-26*time.Hour))
	path := filepath.Join(root, logStatsName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE foreign_data (x); PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	snapshot := loadDefaultHistory(t, nil)
	if len(snapshot.Rows) != 1 || len(snapshot.Notices) != 1 || !strings.Contains(snapshot.Notices[0], T("cli.log_history.stats_unavailable", "")[:8]) {
		t.Fatalf("rows=%d notices=%v", len(snapshot.Rows), snapshot.Notices)
	}
	var stdout, stderr strings.Builder
	code := runLogWithStreams([]string{"prune", "--older-than", "1d", "--yes"}, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 0 || !strings.Contains(stderr.String(), T("cli.log_prune.stats_failed", "")[:10]) || !strings.Contains(stdout.String(), T("cli.log_history.delete_done", 1, 1, "")[:3]) {
		t.Fatalf("prune with unusable stats: %d %q %q", code, stdout.String(), stderr.String())
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatal("stats file of another schema was removed or emptied")
	}
}
