package edc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseCrontabReadsSystemAndUserFormats(t *testing.T) {
	system := strings.Join([]string{
		"SHELL=/bin/sh",
		"# comment",
		"17 *\t* * *\troot\tcd / && run-parts --report /etc/cron.hourly",
		"@reboot   deploy   /opt/app/start.sh  --quiet",
		"5 4 * *",
	}, "\n")
	jobs := parseCrontab("/etc/crontab", system, "", true)
	if len(jobs) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Name != "/etc/crontab:3" || jobs[0].Schedule != "17 * * * *" || jobs[0].User != "root" || jobs[0].Command != "cd / && run-parts --report /etc/cron.hourly" {
		t.Fatalf("first = %+v", jobs[0])
	}
	if jobs[1].Schedule != "@reboot" || jobs[1].User != "deploy" || jobs[1].Command != "/opt/app/start.sh  --quiet" {
		t.Fatalf("reboot = %+v", jobs[1])
	}
	user := parseCrontab("/var/spool/cron/crontabs/alice", "MAILTO=\"\"\n*/5 * * * * /home/alice/sync.sh", "alice", false)
	if len(user) != 1 || user[0].User != "alice" || user[0].Schedule != "*/5 * * * *" || user[0].Command != "/home/alice/sync.sh" {
		t.Fatalf("user = %+v", user)
	}
}

func TestReadCrontabsShowsHostPaths(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/crontab", "0 1 * * * root /bin/backup\n")
	write("etc/cron.d/.placeholder", "")
	write("etc/cron.d/app", "30 2 * * * app /opt/app/nightly\n")
	write("etc/cron.daily/logrotate", "#!/bin/sh\n")
	write("var/spool/cron/crontabs/alice", "@hourly /home/alice/pull\n")
	source := readCrontabs(root)
	var names []string
	for _, job := range source.Jobs {
		names = append(names, job.Name)
	}
	want := []string{"/etc/crontab:1", "/etc/cron.d/app:1", "/var/spool/cron/crontabs/alice:1", "/etc/cron.daily/logrotate"}
	if !slices.Equal(names, want) || source.Err != nil {
		t.Fatalf("names = %v, err = %v", names, source.Err)
	}
	if empty := readCrontabs(t.TempDir()); empty.Err == nil {
		t.Fatal("a host without crontab must skip")
	}
}

func TestTimerJobsJoinTheServiceResult(t *testing.T) {
	timers := parseSystemctlShowBlocks(strings.Join([]string{
		"Unit=backup.service\nNextElapseUSecRealtime=Wed 2026-11-04 05:24:28 KST\nLastTriggerUSec=Tue 2026-11-03 05:24:00 KST\nId=backup.timer",
		"Unit=fresh.service\nNextElapseUSecRealtime=Wed 2026-11-04 01:00:00 KST\nLastTriggerUSec=n/a\nId=fresh.timer",
		"Unit=ok.service\nNextElapseUSecRealtime=\nLastTriggerUSec=Tue 2026-11-03 01:00:00 KST\nId=ok.timer",
	}, "\n\n"))
	services := parseSystemctlShowBlocks("Result=exit-code\nExecMainStatus=2\nId=backup.service\n\nResult=success\nExecMainStatus=0\nId=fresh.service\n\nResult=success\nId=ok.service\n")
	location := time.FixedZone("KST", 9*60*60)
	jobs := timerJobs(timers, services, location)
	if !jobs[0].Failed || jobs[0].Result != "exit-code (status 2)" || jobs[0].Inspect != "journalctl -u backup.service -n 20 --no-pager" {
		t.Fatalf("failed = %+v", jobs[0])
	}
	if want := time.Date(2026, 11, 4, 5, 24, 28, 0, location); !jobs[0].Next.Equal(want) {
		t.Fatalf("next = %s, want %s", jobs[0].Next, want)
	}
	// 한 번도 돌지 않은 service의 success는 결과가 아니다.
	if jobs[1].Failed || jobs[1].Result != "" || !jobs[1].Last.IsZero() {
		t.Fatalf("never run = %+v", jobs[1])
	}
	if jobs[2].Result != "success" || !jobs[2].Next.IsZero() {
		t.Fatalf("ok = %+v", jobs[2])
	}
	if got := parseTimerUnits("apt-daily.timer loaded active waiting Daily apt\nfoo.service loaded active running x\n"); !slices.Equal(got, []string{"apt-daily.timer"}) {
		t.Fatalf("timers = %v", got)
	}
}

func TestLaunchdJobsReadScheduleAndLastStatus(t *testing.T) {
	plists := []map[string]any{
		{"Label": "com.example.updater", "StartInterval": int64(3600), "ProgramArguments": []any{"/opt/up", "--check"}},
		{"Label": "com.example.report", "Program": "/opt/report", "StartCalendarInterval": map[string]any{"Hour": int64(18), "Minute": int64(8)}},
		{"Label": "com.example.twice", "Program": "/opt/twice", "StartCalendarInterval": []any{map[string]any{"Hour": int64(1)}, map[string]any{"Weekday": int64(0), "Hour": int64(2)}}},
		{"Label": "com.example.signal", "Program": "/opt/sig", "StartInterval": int64(60)},
		{"Label": "com.example.agent", "Program": "/opt/agent", "KeepAlive": true},
	}
	var jobs []scheduledJob
	for _, plist := range plists {
		if job, ok := launchdJob("/Library/LaunchAgents/x.plist", plist); ok {
			jobs = append(jobs, job)
		}
	}
	if len(jobs) != 4 {
		t.Fatalf("a job without a schedule must be left out: %+v", jobs)
	}
	if jobs[0].Schedule != T("jobs.launchd.interval", "1h") || !strings.HasPrefix(jobs[0].Command, "/opt/up --check\n") {
		t.Fatalf("interval = %+v", jobs[0])
	}
	if jobs[1].Schedule != "Hour=18 Minute=8" || jobs[2].Schedule != "Hour=1; Weekday=0 Hour=2" {
		t.Fatalf("calendar = %q, %q", jobs[1].Schedule, jobs[2].Schedule)
	}
	applyLaunchctlStatus(jobs, "PID\tStatus\tLabel\n-\t78\tcom.example.updater\n123\t0\tcom.example.report\n-\t-9\tcom.example.signal\n")
	if !jobs[0].Failed || jobs[0].Result != "exit 78" {
		t.Fatalf("exit 78 = %+v", jobs[0])
	}
	if jobs[1].Failed || jobs[2].Result != T("jobs.launchd.not_loaded") || jobs[3].Failed || jobs[3].Result != "signal 9" {
		t.Fatalf("status = %+v", jobs)
	}
}

func TestJobsResultShowsOnlyFailuresUnlessVerbose(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	source := jobsSource{Probe: jobsProbeTimers, Jobs: []scheduledJob{
		{Name: "ok.timer", Next: now.Add(30 * time.Second), Result: "success"},
		{Name: "bad.timer", Failed: true, Result: "exit-code", Inspect: "journalctl -u bad.service -n 20 --no-pager"},
	}}
	result := jobsResult(source, now, false)
	if result.Status != StatusFail || len(result.Evidence) != 1 || result.Evidence[0].Label != "bad.timer" || result.Next != "journalctl -u bad.service -n 20 --no-pager" {
		t.Fatalf("result = %+v", result)
	}
	verbose := jobsResult(source, now, true)
	if len(verbose.Evidence) != 2 || verbose.Evidence[0].Label != "bad.timer" || !strings.Contains(verbose.Evidence[1].Value, T("jobs.next", "30s")) {
		t.Fatalf("verbose = %+v", verbose.Evidence)
	}
	source.Jobs = source.Jobs[:1]
	if healthy := jobsResult(source, now, false); healthy.Status != StatusPass || len(healthy.Evidence) != 1 {
		t.Fatalf("healthy = %+v", healthy)
	}
}
