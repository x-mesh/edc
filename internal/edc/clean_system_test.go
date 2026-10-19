package edc

import (
	"context"
	"fmt"
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

func TestCleanJournaldDetailFollowsTheSystemdPrecedence(t *testing.T) {
	root := t.TempDir()
	etcMain := filepath.Join(root, "etc/journald.conf")
	vendorMain := filepath.Join(root, "usr/journald.conf")
	etcDrop, runDrop, vendorDrop := filepath.Join(root, "etc/journald.conf.d"), filepath.Join(root, "run/journald.conf.d"), filepath.Join(root, "usr/journald.conf.d")
	now := time.Now()
	cleanTestWriteFile(t, etcMain, "[Journal]\n#SystemMaxUse=\nSystemMaxUse=2G\nSystemKeepFree=1G\nStorage=persistent\n[Other]\nMaxRetentionSec=1\n", now)
	cleanTestWriteFile(t, vendorMain, "[Journal]\nSystemMaxUse=9G\nMaxRetentionSec=9\n", now)
	// 같은 이름은 /etc가 /usr/lib을 가린다. 남은 파일은 디렉터리와 상관없이 이름 순으로 적용된다.
	cleanTestWriteFile(t, filepath.Join(vendorDrop, "50-limit.conf"), "[Journal]\nSystemMaxUse=1G\n", now)
	cleanTestWriteFile(t, filepath.Join(etcDrop, "50-limit.conf"), "[Journal]\nSystemMaxUse=500M\n", now)
	cleanTestWriteFile(t, filepath.Join(vendorDrop, "10-first.conf"), "[Journal]\nSystemMaxUse=300M\nMaxRetentionSec=1month\n", now)
	cleanTestWriteFile(t, filepath.Join(runDrop, "90-last.conf"), "[Journal]\nSystemKeepFree=\n", now)
	cleanTestWriteFile(t, filepath.Join(etcDrop, "ignored.txt"), "[Journal]\nSystemMaxFileSize=9G\n", now)
	// 50-limit.conf(/etc)가 SystemMaxUse=500M으로 최종 값을 정하고, 90-last.conf가 SystemKeepFree를 비워 되돌린다.
	got := cleanJournaldDetail([]string{etcMain, vendorMain}, []string{etcDrop, runDrop, vendorDrop})()
	if got != "journald.conf sets SystemMaxUse=500M, MaxRetentionSec=1month" {
		t.Fatalf("detail = %q", got)
	}
	// /etc에 주 설정이 없으면 /usr/lib의 주 설정을 읽고, drop-in이 그 위에 덮인다.
	got = cleanJournaldDetail([]string{filepath.Join(root, "missing.conf"), vendorMain}, []string{vendorDrop})()
	if got != "journald.conf sets SystemMaxUse=1G, MaxRetentionSec=1month" {
		t.Fatalf("vendor main = %q", got)
	}
}

func TestCleanJournaldDetailLetsADevNullLinkMaskADropIn(t *testing.T) {
	root := t.TempDir()
	etcDrop, vendorDrop := filepath.Join(root, "etc"), filepath.Join(root, "usr")
	cleanTestWriteFile(t, filepath.Join(vendorDrop, "50-limit.conf"), "[Journal]\nSystemMaxUse=1G\n", time.Now())
	if err := os.MkdirAll(etcDrop, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", filepath.Join(etcDrop, "50-limit.conf")); err != nil {
		t.Fatal(err)
	}
	if got := cleanJournaldDetail(nil, []string{etcDrop, vendorDrop})(); got != "journald.conf sets no size or retention limit" {
		t.Fatalf("masked = %q", got)
	}
	if got := cleanJournaldDetail([]string{filepath.Join(root, "missing.conf")}, nil)(); got != "journald.conf sets no size or retention limit" {
		t.Fatalf("no file = %q", got)
	}
}

func TestCleanLogrotatePatternsFollowIncludes(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	root := filepath.Dir(paths.logrotateConf)
	extraDir, extraFile, loop := filepath.Join(root, "extra.d"), filepath.Join(root, "single.conf"), filepath.Join(root, "loop.conf")
	cleanTestWriteFile(t, paths.logrotateConf, "weekly\ninclude "+extraDir+"\ninclude "+extraFile+"\ninclude relative.d\ninclude "+filepath.Join(root, "missing")+"\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(extraDir, "app"), "/var/log/app/*.log {\n daily\n include "+loop+"\n}\n/var/log/app2.log {\n}\ninclude "+loop+"\n", time.Now())
	cleanTestWriteFile(t, filepath.Join(extraDir, "app.dpkg-old"), "/var/log/stale.log {\n}\n", time.Now())
	cleanTestWriteFile(t, extraFile, "/var/log/single.log {\n}\n", time.Now())
	cleanTestWriteFile(t, loop, "include "+paths.logrotateConf+"\n/var/log/loop.log {\n}\ninclude "+loop+"\n", time.Now())
	patterns, known := cleanLogrotatePatterns(context.Background(), paths)
	if !known {
		t.Fatal("configuration reported unreadable")
	}
	for path, source := range map[string]string{"/var/log/app/a.log": filepath.Join(extraDir, "app"), "/var/log/app2.log": filepath.Join(extraDir, "app"),
		"/var/log/single.log": extraFile, "/var/log/loop.log": loop} {
		if got := cleanMatchLogrotate(patterns, path); got != source {
			t.Errorf("%s matched %q, want %q", path, got, source)
		}
	}
	if got := cleanMatchLogrotate(patterns, "/var/log/stale.log"); got != "" {
		t.Errorf("a package backup file was read: %q", got)
	}
	if _, known := cleanLogrotatePatterns(context.Background(), cleanSystemPaths{logrotateConf: filepath.Join(root, "none"), logrotateDir: filepath.Join(root, "none.d")}); known {
		t.Error("missing configuration reported known")
	}
}

func TestCleanLogrotatePatternsSkipAFileOverTheSizeLimit(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.logrotateConf), 0o755); err != nil {
		t.Fatal(err)
	}
	cleanTestSparse(t, paths.logrotateConf, cleanLogrotateFileMax+1)
	if patterns, known := cleanLogrotatePatterns(context.Background(), paths); known || len(patterns) != 0 {
		t.Fatalf("a file over the limit was read: known %v patterns %v", known, patterns)
	}
}

func TestCleanSystemReadsStopWhenTheContextIsCancelled(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	cleanTestWriteFile(t, paths.logrotateConf, "/var/log/a.log {\n}\n", time.Now())
	for index := 0; index < 5; index++ {
		cleanTestSparse(t, filepath.Join(paths.snapFiles, fmt.Sprintf("app%d_1.snap", index)), 1<<20)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if patterns, known := cleanLogrotatePatterns(ctx, paths); known || len(patterns) != 0 {
		t.Errorf("logrotate read after cancel: known %v patterns %v", known, patterns)
	}
	if revisions, unknown := cleanFindSnapRevisions(ctx, paths); len(revisions) != 0 || unknown != 0 {
		t.Errorf("snaps read after cancel: %v unknown %d", revisions, unknown)
	}
}

func TestCleanSnapRevisionsCountAnUnreadableCurrentLinkOncePerSnap(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	for _, name := range []string{"core20_1.snap", "core20_2.snap", "core20_3.snap", "lxd_7.snap"} {
		cleanTestSparse(t, filepath.Join(paths.snapFiles, name), 1<<20)
	}
	if err := os.MkdirAll(filepath.Join(paths.snapMounts, "lxd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("9", filepath.Join(paths.snapMounts, "lxd", "current")); err != nil {
		t.Fatal(err)
	}
	revisions, unknown := cleanFindSnapRevisions(context.Background(), paths)
	if unknown != 1 || len(revisions) != 1 || revisions[0].Name != "lxd" {
		t.Fatalf("unknown %d revisions %+v", unknown, revisions)
	}
}

func TestCleanSystemScanLimitsTheSnapRevisionsItLists(t *testing.T) {
	paths := cleanTestSystemPaths(t)
	for index := 0; index < cleanSystemLimit+2; index++ {
		cleanTestSparse(t, filepath.Join(paths.snapFiles, fmt.Sprintf("app_%d.snap", index+1)), int64(index+1)<<20)
	}
	if err := os.MkdirAll(filepath.Join(paths.snapMounts, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("999", filepath.Join(paths.snapMounts, "app", "current")); err != nil {
		t.Fatal(err)
	}
	cleanTestUseProc(t, t.TempDir())
	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	if section.SnapsTotal != cleanSystemLimit+2 || len(section.Snaps) != cleanSystemLimit || section.SnapsOmitted != 2 {
		t.Fatalf("total %d listed %d omitted %d", section.SnapsTotal, len(section.Snaps), section.SnapsOmitted)
	}
	if section.Snaps[0].Bytes < section.Snaps[1].Bytes {
		t.Fatal("snap revisions are not sorted by size")
	}
	if text := formatCleanSystem(section, false, 0, cleanStyle(false)); !strings.Contains(text, "2 more revisions") {
		t.Fatalf("output lacks the omitted count:\n%s", text)
	}
}

func TestCleanSystemScanOffersNoLogCommandWhenAProcessCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every fd directory")
	}
	paths := cleanTestSystemPaths(t)
	cleanTestSparse(t, filepath.Join(paths.logs, "big.log"), cleanLargeLogBytes)
	proc := cleanTestProc(t, map[string][]string{"9": nil})
	fdDir := filepath.Join(proc, "9", "fd")
	if err := os.Chmod(fdDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fdDir, 0o755) })
	cleanTestUseProc(t, proc)
	section := cleanSystemScan(context.Background(), cleanDeps{}, "linux", paths)
	log := section.Logs[0]
	if log.OpenFiles == nil || *log.OpenFiles != 0 || log.OpenFilesComplete || log.Cleanup != "" {
		t.Fatalf("log = %+v", log)
	}
	text := formatCleanSystem(section, true, 0, cleanStyle(false))
	if !strings.Contains(text, "no open file found, but some processes could not be read") || strings.Contains(text, "to clean") {
		t.Fatalf("output = \n%s", text)
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
