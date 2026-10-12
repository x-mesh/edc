package edc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func changesTouch(t *testing.T, root, path string, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func changesKinds(events []changeEvent) string {
	var parts []string
	for _, event := range events {
		parts = append(parts, event.Kind+" "+event.Name+" "+event.Detail)
	}
	return strings.Join(parts, "; ")
}

func TestParseDpkgLogKeepsPackageActionsOnly(t *testing.T) {
	log := strings.Join([]string{
		"2026-10-12 09:00:00 startup archives unpack",
		"2026-10-12 09:00:01 install curl:amd64 <none> 8.5.0-2",
		"2026-10-12 09:00:02 status half-installed curl:amd64 8.5.0-2",
		"2026-10-12 09:00:03 configure curl:amd64 8.5.0-2 <none>",
		"2026-10-12 09:00:04 trigproc man-db:amd64 2.12.0-4 <none>",
		"2026-10-12 09:00:05 upgrade libc6:amd64 2.39-0ubuntu8.3 2.39-0ubuntu8.4",
		"2026-10-12 09:00:06 remove vim:amd64 2:9.1.0016-1 <none>",
		"2026-10-12 09:00:07 purge vim:amd64 2:9.1.0016-1 <none>",
		"2026-10-12 09:00:08 frobnicate x:amd64 1 2",
		"2026-10-12 09:00",
		"garbage line",
		"2026-10-11 08:00:00 install old:amd64 <none> 1.0",
	}, "\n")
	cutoff := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	events, err := parseDpkgLog(strings.NewReader(log), time.UTC, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	want := "install curl:amd64 8.5.0-2; upgrade libc6:amd64 2.39-0ubuntu8.3 -> 2.39-0ubuntu8.4; remove vim:amd64 2:9.1.0016-1; purge vim:amd64 2:9.1.0016-1"
	if got := changesKinds(events); got != want {
		t.Fatalf("events = %q", got)
	}
}

func TestParseDpkgLogUsesLocalTime(t *testing.T) {
	seoul := time.FixedZone("KST", 9*3600)
	events, err := parseDpkgLog(strings.NewReader("2026-10-12 09:00:01 install curl:amd64 <none> 8.5.0-2\n"), seoul, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %v, err = %v", events, err)
	}
	if !events[0].Time.Equal(time.Date(2026, 10, 12, 0, 0, 1, 0, time.UTC)) {
		t.Fatalf("시각을 현지 시각으로 읽지 않았다: %s", events[0].Time)
	}
}

func TestParseDnfRPMLogAcceptsBothOffsetsAndSkipsOldHalves(t *testing.T) {
	log := strings.Join([]string{
		"2026-10-12T01:00:00Z SUBDEBUG Installed: jq-1.6-17.el9.x86_64",
		"2026-10-12T02:00:00+0000 SUBDEBUG Upgrade: bash-5.1.8-9.el9.x86_64",
		"2026-10-12T02:00:00+0000 SUBDEBUG Upgraded: bash-5.1.8-6.el9.x86_64",
		"2026-10-12T03:00:00+00:00 SUBDEBUG Downgrade: openssl-1:3.0.7-24.el9.x86_64",
		"2026-10-12T03:00:00+0000 SUBDEBUG Downgraded: openssl-1:3.0.7-27.el9.x86_64",
		"2026-10-12T04:00:00+0000 SUBDEBUG Reinstalled: tzdata-2024a-1.el9.noarch",
		"2026-10-12T05:00:00+0000 SUBDEBUG Erase: nano-5.6.1-5.el9.x86_64",
		"2026-10-12T05:00:00+0000 SUBDEBUG Obsoleted: thing-1-1.el9.x86_64",
		"2026-10-12T05:00:00+0000 INFO --- logging initialized ---",
		"2026-10-11T05:00:00+0000 SUBDEBUG Installed: old-1-1.el9.x86_64",
	}, "\n")
	cutoff := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	events, err := parseDnfRPMLog(strings.NewReader(log), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	want := "install jq 1.6-17.el9; upgrade bash 5.1.8-9.el9; downgrade openssl 1:3.0.7-24.el9; reinstall tzdata 2024a-1.el9; remove nano 5.6.1-5.el9"
	if got := changesKinds(events); got != want {
		t.Fatalf("events = %q", got)
	}
}

func TestParseYumLogInfersYearAcrossNewYear(t *testing.T) {
	now := time.Date(2027, 1, 2, 12, 0, 0, 0, time.UTC)
	log := "Dec 31 23:00:00 Updated: bash-4.2.46-35.el7_9.x86_64\nJan  2 10:00:00 Installed: jq-1.6-2.el7.x86_64\nJan  2 11:00:00 Erased: nano\nJan  2 11:30:00 Frobbed: x\n"
	events, err := parseYumLog(strings.NewReader(log), now, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %q", changesKinds(events))
	}
	if !events[0].Time.Equal(time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC)) {
		t.Fatalf("12월 줄을 지난해로 읽지 않았다: %s", events[0].Time)
	}
	if got := changesKinds(events); got != "upgrade bash 4.2.46-35.el7_9; install jq 1.6-2.el7; remove nano " {
		t.Fatalf("events = %q", got)
	}
}

func TestPackageLogFilesWarnsAboutCompressedSiblings(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	changesTouch(t, root, "/var/log/dpkg.log", "", now)
	changesTouch(t, root, "/var/log/dpkg.log.1", "", now.Add(-2*time.Hour))
	changesTouch(t, root, "/var/log/dpkg.log.2.gz", "", now.Add(-3*time.Hour))
	changesTouch(t, root, "/var/log/dpkg.log.3.gz", "", now.Add(-72*time.Hour))
	changesTouch(t, root, "/var/log/dpkg.log.4", "", now.Add(-72*time.Hour))
	changesTouch(t, root, "/var/log/dpkg.logx", "", now)
	files, compressed, err := packageLogFiles(root, "/var/log/dpkg.log", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "/var/log/dpkg.log,/var/log/dpkg.log.1" {
		t.Fatalf("files = %v", files)
	}
	if strings.Join(compressed, ",") != "/var/log/dpkg.log.2.gz" {
		t.Fatalf("compressed = %v", compressed)
	}
}

func TestCollectPackageChangesReadsRotatedLogsAndWarns(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	stamp := func(at time.Time) string { return at.Format("2006-01-02 15:04:05") }
	changesTouch(t, root, "/var/log/dpkg.log", stamp(now.Add(-time.Hour))+" install curl:amd64 <none> 8.5.0-2\n", now)
	changesTouch(t, root, "/var/log/dpkg.log.1", stamp(now.Add(-3*time.Hour))+" upgrade libc6:amd64 1 2\n", now.Add(-2*time.Hour))
	changesTouch(t, root, "/var/log/dpkg.log.2.gz", "binary", now.Add(-5*time.Hour))
	sources := changesSources{root: root, now: now}
	result := changesResult("packages", now, collectPackageChanges(sources, now.Add(-24*time.Hour)))
	if result.Status != StatusPass || result.Metrics["count"] != 2 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != T("changes.warning.compressed", "/var/log/dpkg.log.2.gz") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	events := result.Metrics["events"].([]changeEvent)
	if events[0].Name != "curl:amd64" || events[0].Source != "dpkg" {
		t.Fatalf("events = %+v", events)
	}
}

func TestCollectPackageChangesWithoutLogsIsSkipNotPass(t *testing.T) {
	result := changesResult("packages", time.Now(), collectPackageChanges(changesSources{root: t.TempDir(), now: time.Now()}, time.Now().Add(-time.Hour)))
	if result.Status != StatusSkip {
		t.Fatalf("로그가 없는데 %s로 보였다", result.Status)
	}
	if result.Summary != T("changes.skip.none_found", "/var/log/dpkg.log, /var/log/dnf.rpm.log, /var/log/yum.log") {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestCollectPackageChangesUnreadableLogIsNeedsRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root는 권한과 무관하게 읽는다")
	}
	root := t.TempDir()
	changesTouch(t, root, "/var/log/dnf.rpm.log", "", time.Now())
	if err := os.Chmod(filepath.Join(root, "/var/log/dnf.rpm.log"), 0); err != nil {
		t.Fatal(err)
	}
	result := changesResult("packages", time.Now(), collectPackageChanges(changesSources{root: root, now: time.Now()}, time.Now().Add(-time.Hour)))
	if result.Status != StatusSkip || result.Summary != T("changes.skip.needs_root", "/var/log/dnf.rpm.log") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectEtcChangesFindsRecentFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	changesTouch(t, root, "/etc/hosts", "", now.Add(-time.Hour))
	changesTouch(t, root, "/etc/old.conf", "", now.Add(-72*time.Hour))
	changesTouch(t, root, "/etc/ssh/sshd_config", "", now.Add(-2*time.Hour))
	// 서로를 가리키는 고리와 큰 트리를 가리키는 링크다. 따라가면 끝나지 않거나 /etc 밖을 돈다.
	if err := os.Symlink("../loop", filepath.Join(root, "/etc/ssh/loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ssh", filepath.Join(root, "/etc/loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(root, "/etc/rootfs")); err != nil {
		t.Fatal(err)
	}
	result := collectEtcChanges(root, now.Add(-24*time.Hour), changesEtcEntryBudget)
	if result.skip != "" || result.truncated {
		t.Fatalf("result = %+v", result)
	}
	sortChangeEvents(result.events)
	names := map[string]string{}
	for _, event := range result.events {
		names[event.Name] = event.Detail
	}
	if _, ok := names["/etc/hosts"]; !ok {
		t.Fatalf("events = %+v", result.events)
	}
	if _, ok := names["/etc/old.conf"]; ok {
		t.Fatal("창 밖의 파일을 넣었다")
	}
	if names["/etc/rootfs"] != "symlink" || names["/etc/loop"] != "symlink" {
		t.Fatalf("symlink를 자기 mtime으로 넣지 않았다: %+v", result.events)
	}
	for name := range names {
		if !strings.HasPrefix(name, "/etc/") {
			t.Fatalf("/etc 밖을 걸었다: %s", name)
		}
	}
}

func TestCollectEtcChangesStopsAtBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	for index := 0; index < 10; index++ {
		changesTouch(t, root, fmt.Sprintf("/etc/f%02d", index), "", now)
	}
	source := collectEtcChanges(root, now.Add(-time.Hour), 4)
	if !source.truncated || len(source.events) != 4 {
		t.Fatalf("source = %+v", source)
	}
	result := changesResult("etc", now, source)
	if result.Status != StatusWarn || result.Summary != T("changes.summary.etc_truncated", 4, 4) {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectEtcChangesCountsUnreadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root는 권한과 무관하게 읽는다")
	}
	root := t.TempDir()
	changesTouch(t, root, "/etc/hosts", "", time.Now())
	private := filepath.Join(root, "/etc/ssl/private")
	if err := os.MkdirAll(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(private, 0o755) })
	result := changesResult("etc", time.Now(), collectEtcChanges(root, time.Now().Add(-time.Hour), changesEtcEntryBudget))
	if result.Status != StatusPass || len(result.Warnings) != 1 || result.Warnings[0] != T("changes.warning.unreadable_dirs", 1) {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectEtcChangesUnreadableEtcIsSkip(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root는 권한과 무관하게 읽는다")
	}
	root := t.TempDir()
	etc := filepath.Join(root, "etc")
	if err := os.Mkdir(etc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(etc, 0o755) })
	result := changesResult("etc", time.Now(), collectEtcChanges(root, time.Now().Add(-time.Hour), changesEtcEntryBudget))
	if result.Status != StatusSkip || result.Summary != T("changes.skip.needs_root", "/etc") {
		t.Fatalf("result = %+v", result)
	}
}

// 읽지 못한 원천이 "바뀐 것 없음"으로 보이면 사고 직전의 변화를 놓친다. 원천마다 skip과 이유가 나와야 한다.
func TestEvaluateChangesUnreadableSourcesAreSkipWithReason(t *testing.T) {
	now := time.Now()
	sources := changesSources{
		root: t.TempDir(),
		now:  now,
		systemctl: func(context.Context, ...string) ([]byte, error) {
			return nil, errors.New("exec: \"systemctl\": executable file not found in $PATH")
		},
		bootTime: func() (time.Time, error) { return now.Add(-48 * time.Hour), nil },
	}
	results := evaluateChanges(context.Background(), sources, 24*time.Hour)
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	for _, result := range results {
		if result.Status != StatusSkip || result.Summary == "" {
			t.Fatalf("%s = %s %q", result.Probe, result.Status, result.Summary)
		}
		if _, ok := result.Metrics["events"]; ok {
			t.Fatalf("%s가 빈 목록을 담았다", result.Probe)
		}
	}
	if !strings.Contains(results[2].Summary, "executable file not found") {
		t.Fatalf("services summary = %q", results[2].Summary)
	}
	if len(changesEventsOf(results)) != 0 {
		t.Fatal("skip인 원천에서 변화가 나왔다")
	}
}

func TestCollectServiceChangesWithoutSystemdIsSkip(t *testing.T) {
	result := changesResult("services", time.Now(), collectServiceChanges(context.Background(), changesSources{now: time.Now()}, time.Now().Add(-time.Hour)))
	if result.Status != StatusSkip || result.Summary != T("changes.skip.no_systemd") {
		t.Fatalf("result = %+v", result)
	}
}

func fakeSystemctl(manager, units, show string) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == "list-units":
			return []byte(units), nil
		case args[0] == "show" && len(args) == 2:
			return []byte(manager), nil
		default:
			return []byte(show), nil
		}
	}
}

func TestCollectServiceChangesFoldsBootAndKeepsLaterStarts(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	boot := now.Add(-2 * time.Hour)
	units := "ssh.service loaded active running OpenSSH\n● nginx.service loaded failed failed nginx\ncron.service loaded active running cron\nghost.service not-found inactive dead ghost\nblank.service loaded active running blank\n"
	show := strings.Join([]string{
		"Id=ssh.service", "ExecMainStartTimestampMonotonic=5000000", "NRestarts=0", "",
		"Id=nginx.service", "ExecMainStartTimestampMonotonic=3600000000", "NRestarts=3", "",
		"Id=cron.service", "ExecMainStartTimestampMonotonic=8000000", "NRestarts=0", "",
		"Id=ghost.service", "ExecMainStartTimestampMonotonic=0", "NRestarts=0", "",
		"Id=blank.service", "ExecMainStartTimestampMonotonic=", "Unknown=x", "Description=Bläh",
	}, "\n")
	sources := changesSources{
		now:       now,
		systemctl: fakeSystemctl("FinishTimestampMonotonic=10000000\n", units, show),
		bootTime:  func() (time.Time, error) { return boot, nil },
	}
	result := changesResult("services", now, collectServiceChanges(context.Background(), sources, now.Add(-24*time.Hour)))
	if result.Status != StatusPass || result.Summary != T("changes.summary.services", 2, 5) {
		t.Fatalf("result = %+v", result)
	}
	events := result.Metrics["events"].([]changeEvent)
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Kind != "service" || events[0].Name != "nginx.service" || !events[0].Time.Equal(boot.Add(time.Hour)) || events[0].Detail != T("changes.detail.restarts", 3) {
		t.Fatalf("service = %+v", events[0])
	}
	if events[1].Kind != "boot" || !events[1].Time.Equal(boot) || events[1].Detail != T("changes.detail.boot", 2) {
		t.Fatalf("boot = %+v", events[1])
	}
}

func TestCollectServiceChangesOutsideBootWindowDoesNotFold(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	boot := now.Add(-48 * time.Hour)
	show := "Id=ssh.service\nExecMainStartTimestampMonotonic=5000000\n\nId=nginx.service\nExecMainStartTimestampMonotonic=165600000000\nNRestarts=0\n"
	sources := changesSources{
		now:       now,
		systemctl: fakeSystemctl("FinishTimestampMonotonic=10000000\n", "ssh.service a b c\nnginx.service a b c\n", show),
		bootTime:  func() (time.Time, error) { return boot, nil },
	}
	source := collectServiceChanges(context.Background(), sources, now.Add(-24*time.Hour))
	if got := changesKinds(source.events); got != "service nginx.service " {
		t.Fatalf("events = %q", got)
	}
}

func TestParseProcStatBtime(t *testing.T) {
	at, err := parseProcStatBtime([]byte("cpu  1 2 3\nbtime 1760000000\nprocesses 5\n"))
	if err != nil || at.Unix() != 1760000000 {
		t.Fatalf("at = %v, err = %v", at, err)
	}
	if _, err := parseProcStatBtime([]byte("cpu 1\n")); err == nil {
		t.Fatal("btime 없이 성공했다")
	}
}

func TestSortChangeEventsNewestFirstThenKindThenName(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	events := []changeEvent{
		{Time: at, Kind: "upgrade", Name: "b"},
		{Time: at.Add(time.Minute), Kind: "file", Name: "/etc/z"},
		{Time: at, Kind: "install", Name: "z"},
		{Time: at, Kind: "upgrade", Name: "a"},
	}
	sortChangeEvents(events)
	if got := changesKinds(events); got != "file /etc/z ; install z ; upgrade a ; upgrade b " {
		t.Fatalf("order = %q", got)
	}
}

func TestFormatChangesTableCapsRows(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	var events []changeEvent
	for index := 0; index < 60; index++ {
		events = append(events, changeEvent{Time: at.Add(-time.Duration(index) * time.Minute), Kind: "file", Name: fmt.Sprintf("/etc/f%02d", index)})
	}
	table := formatChangesTable(events, false)
	lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
	if len(lines) != 1+50+1 || lines[len(lines)-1] != T("changes.more", 10) {
		t.Fatalf("lines = %d, last = %q", len(lines), lines[len(lines)-1])
	}
	if !strings.HasPrefix(lines[1], "2026-10-12 00:00:00") {
		t.Fatalf("first row = %q", lines[1])
	}
	for _, line := range lines {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("마지막 열을 채웠다: %q", line)
		}
	}
	all := strings.Split(strings.TrimSuffix(formatChangesTable(events, true), "\n"), "\n")
	if len(all) != 61 {
		t.Fatalf("-v rows = %d", len(all))
	}
	if formatChangesTable(nil, false) != "" {
		t.Fatal("변화가 없는데 표를 그렸다")
	}
}
