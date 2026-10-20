package edc

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeHistoryFixture(t *testing.T, path string, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func historyRowsOf(t *testing.T, paths ...string) []logHistoryAttempt {
	t.Helper()
	snapshot := collectLogHistory(paths, nil)
	if len(snapshot.Issues) != 0 {
		t.Fatalf("issues: %v", snapshot.Issues)
	}
	return snapshot.Rows
}

func TestLogHistoryDeletionKeepsSharedLockedAndChangedFamilies(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	a, b := []string{"make", "a"}, []string{"make", "b"}
	own := filepath.Join(dir, "own.log")
	shared := filepath.Join(dir, "shared.log")
	locked := filepath.Join(dir, "locked.log")
	grown := filepath.Join(dir, "grown.log")
	writeHistoryFixture(t, own+".edc.1", historyFixture(a, "/tmp", now.Add(-2*time.Hour), "exit", "1s", 0, 1))
	writeHistoryFixture(t, own, historyFixture(a, "/tmp", now.Add(-time.Hour), "exit", "1s", 0, 1))
	writeHistoryFixture(t, own+".lock", "")
	writeHistoryFixture(t, shared, historyFixture(a, "/tmp", now.Add(-3*time.Hour), "exit", "1s", 0, 1)+historyFixture(b, "/tmp", now.Add(-4*time.Hour), "exit", "1s", 0, 1))
	writeHistoryFixture(t, locked, historyFixture(a, "/tmp", now.Add(-5*time.Hour), "exit", "1s", 0, 1))
	lockHistoryFixture(t, locked+".lock")
	writeHistoryFixture(t, grown, historyFixture(a, "/tmp", now.Add(-6*time.Hour), "exit", "1s", 0, 1))
	var targets []logHistoryAttempt
	for _, row := range historyRowsOf(t, own, shared, locked, grown) {
		if row.Key != "" && row.Command[1] == "a" {
			targets = append(targets, row)
		}
	}
	plan := planLogHistoryDeletion(targets)
	if plan.Runs != 3 || plan.Files != 3 || plan.Shared != 1 || plan.Running != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	file, err := os.OpenFile(grown, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(historyFixture(a, "/tmp", now, "exit", "1s", 0, 1))
	file.Close()
	deleted, err := plan.execute()
	if err != nil || deleted.Runs != 2 || deleted.Files != 2 || deleted.Shared != 1 {
		t.Fatalf("deleted %+v err %v", deleted, err)
	}
	for _, gone := range []string{own, own + ".edc.1", own + ".lock"} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived", gone)
		}
	}
	for _, kept := range []string{shared, locked, grown} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s was deleted: %v", kept, err)
		}
	}
}

func TestLogHistoryDeletionRemovesOnlyEmptyCommandFoldersItMade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	root := defaultLogDirectory()
	if root == "" {
		t.Skip("no default log directory on this platform")
	}
	run := filepath.Join(root, "make", "run.log")
	writeHistoryFixture(t, run, historyFixture([]string{"make"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1))
	named := filepath.Join(home, "jobs", "job.log")
	writeHistoryFixture(t, named, historyFixture([]string{"make"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1))
	deleted, err := planLogHistoryDeletion(historyRowsOf(t, run, named)).execute()
	if err != nil || deleted.Runs != 2 {
		t.Fatalf("deleted %+v err %v", deleted, err)
	}
	if _, err := os.Stat(filepath.Dir(run)); !os.IsNotExist(err) {
		t.Fatal("empty command folder under the default directory survived")
	}
	if _, err := os.Stat(filepath.Dir(named)); err != nil {
		t.Fatal("a folder the user named was removed")
	}
}

func TestLogHistoryBrowserDeletesAfterConfirmation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.log")
	writeHistoryFixture(t, path, historyFixture([]string{"make", "test"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1))
	options := logHistoryOptions{File: path, Limit: 20}
	groups, _ := logHistoryGroups(collectLogHistory([]string{path}, nil), options)
	model := newLogHistoryBrowser(groups, options, nil)
	model.load = logHistoryLoaderFor(options, logHistoryCache{})
	model = historyBrowserAfter(t, model, "enter", "d")
	if model.confirm == nil || !strings.Contains(model.View().Content, T("cli.log_history.delete_confirm", 1, 1, formatSizeShort(model.confirm.Bytes))) {
		t.Fatalf("no confirmation: %q", model.View().Content)
	}
	model = historyBrowserAfter(t, model, "n")
	if model.confirm != nil {
		t.Fatal("n did not cancel")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cancel deleted the log")
	}
	model = historyBrowserAfter(t, model, "d")
	updated, cmd := pressKey(model, "y")
	model = updated.(logHistoryBrowser)
	if _, err := os.Stat(path); !os.IsNotExist(err) || cmd == nil || !strings.Contains(model.status, T("cli.log_history.delete_done", 1, 1, "")[:3]) {
		t.Fatalf("y did not delete: status %q", model.status)
	}
	updated, _ = model.Update(cmd())
	if model = updated.(logHistoryBrowser); len(model.groups) != 0 || model.detail {
		t.Fatal("the browser kept the deleted command")
	}
}

func TestLogPruneDeletesOldRunsOnly(t *testing.T) {
	for text, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "12h": 12 * time.Hour, "1.5d": 36 * time.Hour} {
		if got, err := parseLogAge(text); err != nil || got != want {
			t.Errorf("%s: %v %v", text, got, err)
		}
	}
	for _, text := range []string{"", "0d", "-1h", "abc", "3x"} {
		if _, err := parseLogAge(text); err == nil {
			t.Errorf("%q accepted", text)
		}
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "make", "old.log")
	recent := filepath.Join(dir, "make", "recent.log")
	other := filepath.Join(dir, "go", "old.log")
	writeHistoryFixture(t, old, historyFixture([]string{"make"}, "/tmp", time.Now().Add(-48*time.Hour), "exit", "1s", 0, 1))
	writeHistoryFixture(t, recent, historyFixture([]string{"make"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1))
	writeHistoryFixture(t, other, historyFixture([]string{"go"}, "/tmp", time.Now().Add(-48*time.Hour), "exit", "1s", 0, 1))
	run := func(args ...string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := runLogWithStreams(append([]string{"prune"}, args...), logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
		return code, stdout.String(), stderr.String()
	}
	if code, stdout, _ := run("--dir", dir, "--older-than", "1d", "--dry-run"); code != 0 || !strings.HasPrefix(stdout, T("cli.log_prune.dry_plan", 2, 2, formatSizeShort(0), "")[:5]) || !strings.Contains(stdout, T("cli.log_prune.dry_note")) || strings.Contains(stdout, old) || !strings.Contains(stdout, " make/") || !strings.Contains(stdout, " go/") || !strings.Contains(stdout, dir) {
		t.Fatalf("dry run summary: %d %q", code, stdout)
	}
	if code, stdout, _ := run("--dir", dir, "--older-than", "1d", "--dry-run", "-v"); code != 0 || !strings.Contains(stdout, old) || !strings.Contains(stdout, other) || strings.Contains(stdout, recent) {
		t.Fatalf("dry run -v: %d %q", code, stdout)
	}
	if code, _, stderr := run("--dir", dir, "--older-than", "1d"); code != 2 || !strings.Contains(stderr, T("cli.log_prune.yes_required")) {
		t.Fatalf("pruned without a terminal or --yes: %d %q", code, stderr)
	}
	if code, stdout, stderr := run("--dir", dir, "--older-than", "1d", "--command", "make", "--yes"); code != 0 || stderr != "" || !strings.Contains(stdout, T("cli.log_history.delete_done", 1, 1, "")[:3]) {
		t.Fatalf("prune: %d %q %q", code, stdout, stderr)
	}
	for path, want := range map[string]bool{old: false, recent: true, other: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s exists=%v", path, err == nil)
		}
	}
	if code, _, _ := run("--older-than", "1d", "--key", "xyz"); code != 2 {
		t.Fatal("a key with non-hex characters was accepted")
	}
}

func TestPruneSummaryGroupsCommandsAndNamesTheDirectoryOnce(t *testing.T) {
	root := "/logs"
	day := time.Date(2026, 10, 20, 12, 9, 0, 0, time.Local)
	row := func(argv []string, at time.Time) logHistoryAttempt {
		key, _ := commandKey(argv)
		return logHistoryAttempt{Command: argv, Key: key, Started: at}
	}
	var families []logHistoryDeleteFamily
	for index := 0; index < 3; index++ {
		families = append(families, logHistoryDeleteFamily{path: filepath.Join(root, "curl", time.Duration(index).String()+".log"), runs: 1, bytes: 100, rows: []logHistoryAttempt{row([]string{"curl", "a"}, day.Add(time.Duration(index)*time.Hour))}})
	}
	families = append(families, logHistoryDeleteFamily{path: filepath.Join(root, "curl", "x.log"), runs: 1, bytes: 100, rows: []logHistoryAttempt{row([]string{"curl", "b"}, day.Add(2*time.Hour+30*time.Minute))}})
	families = append(families, logHistoryDeleteFamily{path: "/srv/jobs/backup.log", runs: 2, bytes: 50, rows: []logHistoryAttempt{row([]string{"backup"}, day.AddDate(0, 0, -30)), row([]string{"backup"}, day)}})
	plan := logHistoryDeletion{families: families, Runs: 6, Files: 5, Bytes: 450, Shared: 1}
	lines := pruneSummary(plan, root, false, false)
	text := strings.Join(lines, "\n")
	if lines[0] != T("cli.log_prune.plan_total", 6, 5, formatSizeShort(450)) {
		t.Fatalf("header %q", lines[0])
	}
	if !strings.Contains(lines[1], "curl") || !strings.Contains(lines[1], T("cli.log_prune.runs", 4)) || !strings.Contains(lines[1], "2026-10-20 12:09 – 14:39") || !strings.Contains(lines[1], " /logs/curl/") || !strings.Contains(lines[1], T("cli.log_prune.argument_sets", 2)) {
		t.Fatalf("curl line %q", lines[1])
	}
	if !strings.Contains(lines[2], "2026-09-20 – 2026-10-20") || !strings.Contains(lines[2], " /srv/jobs/") {
		t.Fatalf("backup line %q", lines[2])
	}
	if strings.Contains(text, "x.log") || !strings.Contains(text, T("cli.log_history.delete_kept", 1, 0)) {
		t.Fatalf("summary listed files or lost the kept note:\n%s", text)
	}
	if verbose := strings.Join(pruneSummary(plan, root, true, false), "\n"); !strings.Contains(verbose, "x.log") {
		t.Fatal("-v did not list the files")
	}
	if under := pruneSummary(logHistoryDeletion{families: families[:4], Runs: 4, Files: 4, Bytes: 400}, root, false, false); !strings.Contains(under[0], root) || !strings.Contains(under[1], " curl/") {
		t.Fatalf("default root not used: %q", under)
	}
	var many []logHistoryDeleteFamily
	for index := 0; index < pruneSummaryLimit+3; index++ {
		name := fmt.Sprintf("cmd%02d", index)
		many = append(many, logHistoryDeleteFamily{path: filepath.Join(root, name, "a.log"), runs: 1, rows: []logHistoryAttempt{row([]string{name}, day)}})
	}
	if lines := pruneSummary(logHistoryDeletion{families: many, Runs: len(many)}, root, false, false); len(lines) != pruneSummaryLimit+2 || !strings.Contains(lines[len(lines)-1], T("cli.log_prune.more", 3)) {
		t.Fatalf("limit: %q", lines)
	}
}

func TestLogHistoryKeepsTheNewestFamiliesAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Duration(historyFamilyLimit+10) * time.Minute)
	var newestPath, oldestPath string
	for index := 0; index < historyFamilyLimit+10; index++ {
		// Names run against time so that name order and time order disagree.
		path := filepath.Join(dir, "curl", fmt.Sprintf("%05d.log", historyFamilyLimit+10-index))
		at := base.Add(time.Duration(index) * time.Minute)
		writeHistoryFixture(t, path, historyFixture([]string{"curl"}, "/tmp", at, "exit", "1s", 0, 1))
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldestPath = path
		}
		newestPath = path
	}
	paths, _, err := historyPaths(dir)
	if err != errHistoryScanLimit || len(paths) != historyFamilyLimit || paths[0] != newestPath || slices.Contains(paths, oldestPath) {
		t.Fatalf("err=%v len=%d first=%s", err, len(paths), paths[0])
	}
	snapshot, err := loadLogHistory(logHistoryOptions{Directory: dir}, nil)
	if err != nil || len(snapshot.Rows) != historyFamilyLimit || snapshot.Rows[0].Path != newestPath || len(snapshot.Issues) == 0 {
		t.Fatalf("snapshot: err=%v rows=%d issues=%v", err, len(snapshot.Rows), snapshot.Issues)
	}
}

func TestHistoryLogOpensTheLogHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	writeHistoryFixture(t, path, historyFixture([]string{"make", "test"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1))
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	code := runHistory([]string{"log", "--file", path})
	os.Stdout = stdout
	write.Close()
	output, _ := io.ReadAll(read)
	if code != 0 || !strings.Contains(string(output), T("cli.log_history.list_title")) || !strings.Contains(string(output), "make test") {
		t.Fatalf("code=%d output=%q", code, output)
	}
	if !slices.Contains(subcommandChoices("history"), "log") {
		t.Fatal("the history picker has no log entry")
	}
}

func TestLogPruneReachesTheOldestRunsOfALargeFolder(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-48 * time.Hour)
	var oldest string
	for index := 0; index < historyFamilyLimit+5; index++ {
		path := filepath.Join(dir, "curl", fmt.Sprintf("%05d.log", index))
		at := base.Add(time.Duration(index) * time.Minute)
		writeHistoryFixture(t, path, historyFixture([]string{"curl"}, "/tmp", at, "exit", "1s", 0, 1))
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldest = path
		}
	}
	var stdout, stderr strings.Builder
	code := runLogWithStreams([]string{"prune", "--dir", dir, "--older-than", "47h", "--dry-run", "-v"}, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	if code != 2 || !strings.Contains(stdout.String(), oldest) {
		t.Fatalf("prune did not reach the oldest run: code=%d %q", code, stdout.String()[:min(len(stdout.String()), 300)])
	}
}

func TestLogHistoryReadsAFolderWithMoreThanTenThousandEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 10,002 files")
	}
	dir := t.TempDir()
	folder := filepath.Join(dir, "curl")
	text := historyFixture([]string{"curl"}, "/tmp", time.Now().Add(-time.Hour), "exit", "1s", 0, 1)
	const runs = 5001
	for index := 0; index < runs; index++ {
		path := filepath.Join(folder, fmt.Sprintf("%05d.log", index))
		writeHistoryFixture(t, path, text)
		writeHistoryFixture(t, path+".lock", "")
	}
	snapshot, err := loadLogHistory(logHistoryOptions{Directory: dir}, nil)
	if err != nil || len(snapshot.Rows) != historyFamilyLimit {
		t.Fatalf("rows=%d err=%v issues=%v", len(snapshot.Rows), err, snapshot.Issues)
	}
	plan := planLogHistoryDeletion(snapshot.Rows[:3])
	if plan.Runs != 3 {
		t.Fatalf("deletion plan in a large folder: %+v", plan)
	}
}
