package edc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanTestSparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func cleanTestSystemPaths(t *testing.T) cleanSystemPaths {
	t.Helper()
	root := t.TempDir()
	return cleanSystemPaths{logs: filepath.Join(root, "var/log"), logrotateConf: filepath.Join(root, "etc/logrotate.conf"),
		logrotateDir: filepath.Join(root, "etc/logrotate.d"), snapFiles: filepath.Join(root, "var/lib/snapd/snaps"), snapMounts: filepath.Join(root, "snap")}
}

func TestCleanJournaldDetailReportsTheConfiguredLimits(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "journald.conf")
	dropIn := filepath.Join(dir, "journald.conf.d")
	cleanTestWriteFile(t, main, "[Journal]\n#SystemMaxUse=\nSystemMaxUse=2G\nStorage=persistent\n[Other]\nMaxRetentionSec=1\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(dropIn, "10-size.conf"), "[Journal]\nSystemMaxUse=500M\nMaxRetentionSec=1month\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(dropIn, "ignored.txt"), "[Journal]\nSystemKeepFree=9G\n", time.Now())
	got := cleanJournaldDetail(main, dropIn)()
	if got != "journald.conf sets SystemMaxUse=500M, MaxRetentionSec=1month" {
		t.Fatalf("detail = %q", got)
	}
	if none := cleanJournaldDetail(filepath.Join(dir, "missing.conf"))(); none != "journald.conf sets no size or retention limit" {
		t.Fatalf("no limit = %q", none)
	}
}

func TestCleanParseLogrotateReadsRuleHeadersOnly(t *testing.T) {
	text := `# global
weekly
include /etc/logrotate.d
olddir /var/log/old

/var/log/syslog
/var/log/mail.*
/var/log/other.log {
    rotate 7
    postrotate
        /usr/lib/rsyslog/rsyslog-rotate
    endscript
}
/var/log/a.log /var/log/b.log {
    daily
}
`
	var globs []string
	for _, pattern := range cleanParseLogrotate(text, "/etc/logrotate.d/x") {
		globs = append(globs, pattern.glob)
	}
	for _, expected := range []string{"/var/log/syslog", "/var/log/mail.*", "/var/log/a.log", "/var/log/b.log"} {
		found := false
		for _, glob := range globs {
			found = found || glob == expected
		}
		if !found {
			t.Errorf("pattern %q missing from %v", expected, globs)
		}
	}
	for _, glob := range globs {
		if strings.Contains(glob, "rsyslog-rotate") || glob == "/etc/logrotate.d" || glob == "/var/log/old" {
			t.Errorf("a line inside a rule or an option became a pattern: %q", glob)
		}
	}
	patterns := cleanParseLogrotate(text, "src")
	if got := cleanMatchLogrotate(patterns, "/var/log/mail.err"); got != "src" {
		t.Errorf("mail.err matched %q", got)
	}
	if got := cleanMatchLogrotate(patterns, "/var/log/system_check.log"); got != "" {
		t.Errorf("an unlisted log matched %q", got)
	}
}

func TestCleanSystemScanExplainsLargeLogsAndSnapRevisions(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	big, rotated := filepath.Join(paths.logs, "system_check.log"), filepath.Join(paths.logs, "syslog")
	cleanTestSparse(t, big, cleanLargeLogBytes+1)
	cleanTestSparse(t, rotated, cleanLargeLogBytes+2<<20)
	cleanTestSparse(t, filepath.Join(paths.logs, "small.log"), 1<<20)
	cleanTestSparse(t, filepath.Join(paths.logs, "journal", "huge.journal"), 3*cleanLargeLogBytes)
	cleanTestWriteFile(t, paths.logrotateConf, "weekly\ninclude /etc/logrotate.d\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(paths.logrotateDir, "rsyslog"), "/var/unused\n"+rotated+" {\n weekly\n}\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(paths.logrotateDir, "rsyslog.dpkg-old"), big+" {\n}\n", time.Now())
	cleanTestUseProc(t, cleanTestProc(t, map[string][]string{"42": {big, big + " (deleted)", "/dev/null"}}))

	cleanTestSparse(t, filepath.Join(paths.snapFiles, "core20_2866.snap"), 60<<20)
	cleanTestSparse(t, filepath.Join(paths.snapFiles, "core20_2889.snap"), 61<<20)
	cleanTestSparse(t, filepath.Join(paths.snapFiles, "lxd_40575.snap"), 90<<20)
	cleanTestSparse(t, filepath.Join(paths.snapFiles, "ghost_1.snap"), 1<<20)
	cleanTestSparse(t, filepath.Join(paths.snapFiles, "partial.tmp"), 1<<20)
	for name, current := range map[string]string{"core20": "2889", "lxd": "40990"} {
		if err := os.MkdirAll(filepath.Join(paths.snapMounts, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(current, filepath.Join(paths.snapMounts, name, "current")); err != nil {
			t.Fatal(err)
		}
	}

	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	if section.Status != cleanMeasured || !section.LogrotateKnown || section.LogsTotal != 2 || len(section.Logs) != 2 {
		t.Fatalf("section = %+v", section)
	}
	first, second := section.Logs[0], section.Logs[1]
	if first.Path != rotated || first.Logrotate != filepath.Join(paths.logrotateDir, "rsyslog") || first.OpenFiles == nil || *first.OpenFiles != 0 ||
		first.Cleanup != "rm "+cleanShellQuote(rotated) {
		t.Fatalf("rotated log = %+v", first)
	}
	if second.Path != big || second.Logrotate != "" || second.OpenFiles == nil || *second.OpenFiles != 2 || second.Cleanup != "truncate -s 0 "+cleanShellQuote(big) {
		t.Fatalf("unrotated log = %+v", second)
	}
	if section.SnapsTotal != 2 || section.SnapsUnknown != 1 || len(section.Snaps) != 2 {
		t.Fatalf("snaps = %+v unknown %d", section.Snaps, section.SnapsUnknown)
	}
	if snap := section.Snaps[0]; snap.Name != "lxd" || snap.Revision != "40575" || snap.Current != "40990" || snap.Cleanup != "snap remove lxd --revision=40575" {
		t.Fatalf("largest snap = %+v", snap)
	}
	if snap := section.Snaps[1]; snap.Name != "core20" || snap.Revision != "2866" {
		t.Fatalf("second snap = %+v", snap)
	}
}

func TestCleanSystemScanOffersNoCommandWhenOpenFilesAreUnknown(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	cleanTestSparse(t, filepath.Join(paths.logs, "big.log"), cleanLargeLogBytes)
	cleanTestUseProc(t, filepath.Join(t.TempDir(), "missing"))
	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	log := section.Logs[0]
	if log.OpenFiles != nil || log.Cleanup != "" || section.LogrotateKnown {
		t.Fatalf("log = %+v logrotate known %v", log, section.LogrotateKnown)
	}
	text := formatCleanSystem(section, true, 0, cleanStyle(false))
	for _, want := range []string{"open files unknown", "no logrotate configuration could be read"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "to clean") {
		t.Errorf("a command was shown although the open files are unknown:\n%s", text)
	}
}

func TestCleanSystemScanStaysQuietWithNothingToReport(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	if got := cleanSystemScan(context.Background(), cleanDeps{}, "darwin", paths); got.Status != "" {
		t.Fatalf("darwin = %+v", got)
	}
	cleanTestUseProc(t, t.TempDir())
	if got := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths); got.Status != cleanAbsent {
		t.Fatalf("empty = %+v", got)
	}
	if text := formatCleanSystem(cleanSystem{Status: cleanAbsent}, true, 0, cleanStyle(false)); text != "" {
		t.Fatalf("absent section printed %q", text)
	}
}

func TestCleanSystemScanLimitsTheFilesItLists(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	for index := 0; index < cleanSystemLimit+2; index++ {
		cleanTestSparse(t, filepath.Join(paths.logs, "log"+string(rune('a'+index))), cleanLargeLogBytes+int64(index)<<20)
	}
	cleanTestUseProc(t, t.TempDir())
	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	if section.LogsTotal != cleanSystemLimit+2 || len(section.Logs) != cleanSystemLimit || section.LogsOmitted != 2 || section.LogsOmittedBytes == 0 {
		t.Fatalf("total %d listed %d omitted %d bytes %d", section.LogsTotal, len(section.Logs), section.LogsOmitted, section.LogsOmittedBytes)
	}
	if section.Logs[0].Bytes < section.Logs[1].Bytes {
		t.Fatal("logs are not sorted by size")
	}
}

func TestFormatCleanSystemShowsTheEvidenceAndOnlyShowsCommandsWithVerbose(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	big := filepath.Join(paths.logs, "system_check.log")
	cleanTestSparse(t, big, cleanLargeLogBytes+1)
	cleanTestSparse(t, filepath.Join(paths.snapFiles, "lxd_1.snap"), 5<<20)
	if err := os.MkdirAll(filepath.Join(paths.snapMounts, "lxd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("2", filepath.Join(paths.snapMounts, "lxd", "current")); err != nil {
		t.Fatal(err)
	}
	cleanTestWriteFile(t, paths.logrotateConf, "weekly\n", time.Now())
	cleanTestUseProc(t, t.TempDir())
	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	plain := formatCleanSystem(section, false, 0, cleanStyle(false))
	for _, want := range []string{"Large logs and unused snap revisions", "log files of 100.0 MB or more: 1", big, "no process has it open",
		"no logrotate rule matches", "snap revisions that are not the current one: 1", "lxd revision 1 (current 2)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("output lacks %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "to clean") {
		t.Errorf("commands shown without -v:\n%s", plain)
	}
	verbose := formatCleanSystem(section, true, 0, cleanStyle(false))
	for _, want := range []string{"to clean, run rm " + cleanShellQuote(big) + " (edc does not run it)", "to clean, run snap remove lxd --revision=1 (edc does not run it)"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose output lacks %q:\n%s", want, verbose)
		}
	}
}

// cleanTestIsolateSystem은 clean이 호스트의 /var와 /etc 대신 빈 임시 폴더를 읽게 한다. 호스트마다 결과가 달라지지 않게 한다.
func cleanTestIsolateSystem(t *testing.T) {
	t.Helper()
	oldJournal, oldApt, oldDefaults := cleanJournalPath, cleanAptPath, cleanSystemDefaults
	root := t.TempDir()
	cleanJournalPath, cleanAptPath = filepath.Join(root, "journal"), filepath.Join(root, "apt")
	cleanSystemDefaults = cleanSystemPaths{logs: filepath.Join(root, "log"), logrotateConf: filepath.Join(root, "logrotate.conf"),
		logrotateDir: filepath.Join(root, "logrotate.d"), snapFiles: filepath.Join(root, "snaps"), snapMounts: filepath.Join(root, "snap")}
	t.Cleanup(func() { cleanJournalPath, cleanAptPath, cleanSystemDefaults = oldJournal, oldApt, oldDefaults })
}
