package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeSvcUnit(t *testing.T) {
	valid := map[string]string{
		"nginx":              "nginx.service",
		"nginx.service":      "nginx.service",
		"getty@tty1":         "getty@tty1.service",
		"dbus-org.x_y:z":     "dbus-org.x_y:z.service",
		"systemd-journald":   "systemd-journald.service",
		"user@1000.service":  "user@1000.service",
		"app.v2":             "app.v2.service",
		"Some_Mixed-Case.01": "Some_Mixed-Case.01.service",
	}
	for input, want := range valid {
		got, err := normalizeSvcUnit(input)
		if err != nil || got != want {
			t.Errorf("normalizeSvcUnit(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-p", "--help", "a b", "x;y", "*", "ng?nx", "a/b", "$(id)", "foo.timer", "foo.socket", "foo.target", "foo.mount", "foo.path", "foo.slice", "foo.scope", "foo.device", "foo.swap", "foo.automount", ".service", "-x.service"} {
		if got, err := normalizeSvcUnit(input); err == nil {
			t.Errorf("normalizeSvcUnit(%q) = %q, want an error", input, got)
		}
	}
}

func TestParseSvcShow(t *testing.T) {
	show := parseSvcShow("Id=app.service\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=4321\nNRestarts=3\nActiveEnterTimestampMonotonic=5000000\nControlGroup=/system.slice/app.service\nExtra=a=b=c\n")
	if show.ID != "app.service" || show.LoadState != "loaded" || show.ActiveState != "active" || show.SubState != "running" || show.Result != "success" {
		t.Fatalf("show = %+v", show)
	}
	if show.MainPID != 4321 || show.NRestarts == nil || *show.NRestarts != 3 || show.ActiveEnterMonotonic != 5000000 || show.ControlGroup != "/system.slice/app.service" {
		t.Fatalf("show = %+v", show)
	}
	// 오래된 systemd에는 NRestarts가 없다. 값 안의 '='은 첫 '='에서만 나눠야 남는다.
	old := parseSvcShow("Id=a=b.service\nMainPID=0\nActiveEnterTimestampMonotonic=x\n")
	if old.ID != "a=b.service" || old.MainPID != 0 || old.NRestarts != nil || old.ActiveEnterMonotonic != 0 {
		t.Fatalf("old = %+v", old)
	}
}

func TestSvcUptime(t *testing.T) {
	active := svcShow{ActiveState: "active", ActiveEnterMonotonic: 2_000_000}
	if got, ok := svcUptime(active, 5*time.Second, true); !ok || got != 3*time.Second {
		t.Fatalf("uptime = %v, %v", got, ok)
	}
	cases := map[string]struct {
		show     svcShow
		now      time.Duration
		nowKnown bool
	}{
		"monotonic 0":       {svcShow{ActiveState: "active"}, 5 * time.Second, true},
		"later than now":    {active, time.Second, true},
		"clock unknown":     {active, 5 * time.Second, false},
		"not active (prev)": {svcShow{ActiveState: "failed", ActiveEnterMonotonic: 1}, 5 * time.Second, true},
	}
	for name, test := range cases {
		if got, ok := svcUptime(test.show, test.now, test.nowKnown); ok {
			t.Errorf("%s: uptime = %v, want unknown", name, got)
		}
	}
}

func TestSvcCgroupMember(t *testing.T) {
	const group = "/system.slice/foo.service"
	cases := []struct {
		name, data, group string
		want              bool
	}{
		{"v2 equal", "0::/system.slice/foo.service\n", group, true},
		{"v2 child", "0::/system.slice/foo.service/worker\n", group, true},
		{"v2 prefix collision", "0::/system.slice/foo.service-x\n", group, false},
		{"v2 other", "0::/system.slice/bar.service\n", group, false},
		{"v1", "12:pids:/system.slice/foo.service\n1:name=systemd:/system.slice/foo.service\n", group, true},
		{"v1 other", "12:pids:/system.slice/foo.service\n1:name=systemd:/user.slice\n", group, false},
		{"hybrid root unified", "1:name=systemd:/system.slice/foo.service\n0::/\n", group, true},
		{"hybrid prefers unified", "1:name=systemd:/system.slice/bar.service\n0::/system.slice/foo.service\n", group, true},
		{"empty group", "0::/system.slice/foo.service\n", "", false},
		{"root group", "0::/system.slice/foo.service\n", "/", false},
		{"empty data", "", group, false},
	}
	for _, test := range cases {
		if got := svcCgroupMember(test.data, test.group); got != test.want {
			t.Errorf("%s: member = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestFilterSvcJournal(t *testing.T) {
	hint := "Hint: You are currently not seeing messages from other users and the system.\n      Users in groups 'adm', 'systemd-journal' can see all messages.\n"
	hidden := filterSvcJournal("-- No entries --\n", hint)
	if !hidden.Hidden || len(hidden.Lines) != 0 {
		t.Fatalf("hidden = %+v", hidden)
	}
	empty := filterSvcJournal("-- No entries --\n", "")
	if empty.Hidden || len(empty.Lines) != 0 {
		t.Fatalf("empty = %+v", empty)
	}
	long := strings.Repeat("가", 200)
	journal := filterSvcJournal("-- Logs begin at Mon 2026-10-12 --\n2026-10-12T01:00:00+0000 host app[1]: one\n\n"+long+"\n", "")
	if journal.Hidden || len(journal.Lines) != 2 || journal.Lines[0] != "2026-10-12T01:00:00+0000 host app[1]: one" {
		t.Fatalf("journal = %+v", journal)
	}
	if got := []rune(journal.Lines[1]); len(got) != svcJournalLineRunes+1 || got[len(got)-1] != '…' {
		t.Fatalf("long line has %d runes", len(got))
	}
	many := filterSvcJournal("1\n2\n3\n4\n5\n6\n7\n", "")
	if len(many.Lines) != svcJournalLines || many.Lines[0] != "3" {
		t.Fatalf("many = %v", many.Lines)
	}
}

func svcUint(value uint64) *uint64 { return &value }
func svcInt(value int) *int        { return &value }

func svcAvailableFD(used int, soft uint64) topFDLimit {
	return topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: svcInt(used), Soft: svcUint(soft)}
}

func svcDeniedFD() topFDLimit {
	return topFDLimit{topLimitStatus: topLimitStatus{Status: "permission_denied", Reason: "open /proc/1/fd: permission denied"}, Soft: svcUint(1024)}
}

func svcRunningFacts() svcFacts {
	return svcFacts{
		Unit: "app.service", Root: true, Systemd: true,
		Show:   svcShow{ID: "app.service", LoadState: "loaded", ActiveState: "active", SubState: "running", Result: "success", MainPID: 100, NRestarts: svcUint(0), ControlGroup: "/system.slice/app.service"},
		Uptime: 90 * time.Minute, UptimeKnown: true,
		Processes: []topProcess{{PID: 101, CPU: 3, RSS: 2 << 20, Command: "worker"}, {PID: 100, CPU: 1, RSS: 4 << 20, Command: "app"}},
		FD:        map[int]topFDLimit{100: svcAvailableFD(10, 1024), 101: svcAvailableFD(20, 1024)},
		Listen: []listenSocket{
			{Proto: "tcp", Address: "0.0.0.0", Port: 8080, PID: "101", Process: "worker"},
			{Proto: "tcp", Address: "127.0.0.1", Port: 5432, PID: "999", Process: "postgres"},
			{Proto: "unix", Address: "/run/app.sock", PID: "100", Process: "app"},
		},
	}
}

func svcByID(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, result := range results {
		if result.Probe == "svc."+id {
			return result
		}
	}
	t.Fatalf("svc.%s가 없다: %+v", id, results)
	return Result{}
}

func TestEvaluateSvcHealthyUnit(t *testing.T) {
	results := evaluateSvc(svcRunningFacts())
	if len(results) != 5 {
		t.Fatalf("results = %d", len(results))
	}
	for _, result := range results {
		if result.Status != StatusPass {
			t.Errorf("%s = %s: %s", result.Probe, result.Status, result.Summary)
		}
	}
	state := svcByID(t, results, "state")
	if !strings.Contains(state.Summary, "active (running)") || !strings.Contains(state.Summary, "1h 30m") {
		t.Fatalf("state = %q", state.Summary)
	}
	processes := svcByID(t, results, "processes")
	if !strings.Contains(processes.Evidence[0].Value, "(main)") || !strings.HasPrefix(strings.TrimSpace(processes.Evidence[0].Value), "100") {
		t.Fatalf("main process is not first: %q", processes.Evidence[0].Value)
	}
	listen := svcByID(t, results, "listen")
	if !strings.Contains(listen.Summary, "tcp 0.0.0.0:8080") || strings.Contains(listen.Summary, "5432") || strings.Contains(listen.Evidence[0].Value, "/run/app.sock") {
		t.Fatalf("listen = %q / %q", listen.Summary, listen.Evidence[0].Value)
	}
}

func TestEvaluateSvcStateStatuses(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*svcShow)
		status Status
	}{
		{"active", func(*svcShow) {}, StatusPass},
		{"restarted", func(show *svcShow) { show.NRestarts = svcUint(2) }, StatusWarn},
		{"restarts unknown", func(show *svcShow) { show.NRestarts = nil }, StatusPass},
		{"failed", func(show *svcShow) { show.ActiveState, show.SubState, show.Result = "failed", "failed", "exit-code" }, StatusFail},
		{"inactive", func(show *svcShow) { show.ActiveState, show.SubState = "inactive", "dead" }, StatusWarn},
		{"activating", func(show *svcShow) { show.ActiveState, show.SubState = "activating", "auto-restart" }, StatusWarn},
		{"masked", func(show *svcShow) { show.LoadState = "masked" }, StatusFail},
		{"bad-setting", func(show *svcShow) { show.LoadState = "bad-setting" }, StatusFail},
	}
	for _, test := range cases {
		facts := svcRunningFacts()
		test.edit(&facts.Show)
		state := svcByID(t, evaluateSvc(facts), "state")
		if state.Status != test.status {
			t.Errorf("%s: %s, want %s (%s)", test.name, state.Status, test.status, state.Summary)
		}
	}
	facts := svcRunningFacts()
	facts.Show.NRestarts, facts.UptimeKnown = nil, false
	if state := svcByID(t, evaluateSvc(facts), "state"); !strings.Contains(state.Summary, "up -, - restarts") {
		t.Fatalf("unknown values are not '-': %q", state.Summary)
	}
}

func TestEvaluateSvcWithoutSystemdIsOneSkip(t *testing.T) {
	results := evaluateSvc(svcFacts{Unit: "app.service"})
	if len(results) != 1 || results[0].Status != StatusSkip || results[0].Probe != "svc" {
		t.Fatalf("results = %+v", results)
	}
}

func TestEvaluateSvcShowFailureCarriesOutput(t *testing.T) {
	results := evaluateSvc(svcFacts{Unit: "app.service", Systemd: true, ShowOutput: "Failed to connect to bus: No such file or directory", ShowErr: errors.New("exit status 1")})
	if len(results) != 1 || results[0].Status != StatusFail || !strings.Contains(results[0].Summary, "Failed to connect to bus") || results[0].Error == nil {
		t.Fatalf("results = %+v", results)
	}
}

func TestEvaluateSvcNotRunningSkipsProcessViews(t *testing.T) {
	facts := svcRunningFacts()
	facts.Show.ActiveState, facts.Show.SubState, facts.Show.MainPID, facts.Show.ControlGroup = "inactive", "dead", 0, ""
	facts.Processes, facts.FD, facts.Listen = nil, map[int]topFDLimit{}, nil
	results := evaluateSvc(facts)
	for _, id := range []string{"processes", "fd", "listen"} {
		if result := svcByID(t, results, id); result.Status != StatusSkip {
			t.Errorf("%s = %s: %s", id, result.Status, result.Summary)
		}
	}
}

func TestEvaluateSvcCapsRowsButCountsAll(t *testing.T) {
	facts := svcRunningFacts()
	facts.Processes = nil
	for index := range 12 {
		facts.Processes = append(facts.Processes, topProcess{PID: 200 + index, CPU: float64(20 - index), RSS: 1 << 20, Command: "w"})
	}
	// main process는 CPU가 가장 낮아도 화면에 남아야 한다.
	facts.Processes = append(facts.Processes, topProcess{PID: 100, CPU: 0, RSS: 1 << 20, Command: "app"})
	facts.FD = map[int]topFDLimit{}
	for _, process := range svcRows(facts.Processes, 100) {
		facts.FD[process.PID] = svcAvailableFD(1, 1024)
	}
	processes := svcByID(t, evaluateSvc(facts), "processes")
	rows := strings.Split(processes.Evidence[0].Value, "\n")
	if len(rows) != svcMaxRows || !strings.Contains(rows[0], "(main)") {
		t.Fatalf("rows = %q", rows)
	}
	if processes.Metrics["count"] != 13 || processes.Metrics["rss_bytes"] != uint64(13<<20) || !strings.HasPrefix(processes.Summary, "13 processes") {
		t.Fatalf("summary = %q, metrics = %v", processes.Summary, processes.Metrics)
	}
}

func TestEvaluateSvcFDStatuses(t *testing.T) {
	cases := []struct {
		name   string
		limit  topFDLimit
		status Status
		want   string
	}{
		{"low", svcAvailableFD(100, 1024), StatusPass, "100/1024"},
		{"at 80%", svcAvailableFD(820, 1024), StatusWarn, "820/1024"},
		{"unlimited", topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: svcInt(5), Unlimited: true}, StatusPass, "no fd limit"},
		{"soft 0", svcAvailableFD(5, 0), StatusPass, "no fd limit"},
		{"denied", svcDeniedFD(), StatusSkip, "needs root"},
		{"error", topFDLimit{topLimitStatus: topLimitStatus{Status: "error", Reason: "invalid Max open files row"}, Used: svcInt(3)}, StatusSkip, "cannot read"},
	}
	for _, test := range cases {
		facts := svcRunningFacts()
		facts.Processes = facts.Processes[1:]
		facts.FD = map[int]topFDLimit{100: test.limit}
		fd := svcByID(t, evaluateSvc(facts), "fd")
		if fd.Status != test.status || !strings.Contains(fd.Summary, test.want) {
			t.Errorf("%s: %s %q, want %s with %q", test.name, fd.Status, fd.Summary, test.status, test.want)
		}
	}
	// 읽은 process 중 하나가 한계에 가까우면 읽지 못한 process가 있어도 경고하고, 읽지 못했다는 사실을 남긴다.
	facts := svcRunningFacts()
	facts.FD = map[int]topFDLimit{100: svcDeniedFD(), 101: svcAvailableFD(900, 1000)}
	fd := svcByID(t, evaluateSvc(facts), "fd")
	if fd.Status != StatusWarn || len(fd.Warnings) != 1 || !strings.Contains(fd.Warnings[0], "needs root") {
		t.Fatalf("mixed = %+v", fd)
	}
}

// R8: 권한 때문에 읽지 못한 원천은 통과가 아니라 skip이다.
func TestEvaluateSvcNeverPassesUnreadableSources(t *testing.T) {
	facts := svcRunningFacts()
	facts.Root = false
	facts.FD = map[int]topFDLimit{100: svcDeniedFD(), 101: svcDeniedFD()}
	// root가 아니면 소켓에 주인이 붙지 않아 목록이 비어 보인다.
	facts.Listen = []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 8080}}
	facts.JournalErr = errSvcNeedsRoot
	results := evaluateSvc(facts)
	for _, id := range []string{"fd", "listen", "journal"} {
		result := svcByID(t, results, id)
		if result.Status != StatusSkip || !strings.Contains(result.Summary, "needs root") {
			t.Errorf("%s = %s %q, want a needs-root skip", id, result.Status, result.Summary)
		}
	}
	facts = svcRunningFacts()
	facts.ListenErr = errors.New("netlink: operation not supported")
	if listen := svcByID(t, evaluateSvc(facts), "listen"); listen.Status != StatusSkip || !strings.Contains(listen.Summary, "operation not supported") {
		t.Fatalf("listen error = %+v", listen)
	}
}

func TestEvaluateSvcJournal(t *testing.T) {
	facts := svcRunningFacts()
	facts.Journal = svcJournal{Lines: []string{"a: boom", "b: crash"}}
	journal := svcByID(t, evaluateSvc(facts), "journal")
	if journal.Status != StatusWarn || !strings.Contains(journal.Summary, "b: crash") {
		t.Fatalf("journal = %+v", journal)
	}
	facts.Journal, facts.JournalErr = svcJournal{}, fmt.Errorf("journalctl: %w", context.DeadlineExceeded)
	if journal := svcByID(t, evaluateSvc(facts), "journal"); journal.Status != StatusSkip || !strings.Contains(journal.Summary, "deadline") {
		t.Fatalf("timeout = %+v", journal)
	}
}

type svcFakeSources struct {
	listenCalls int
}

func (fake *svcFakeSources) sources(euid int, show string, journal func(context.Context) (string, string, error)) svcSources {
	return svcSources{
		euid:    euid,
		systemd: func() bool { return true },
		show:    func(context.Context, string) (string, error) { return show, nil },
		processes: func(context.Context, string, int) ([]topProcess, error) {
			return []topProcess{{PID: 100, CPU: 1, RSS: 1 << 20, Command: "app"}}, nil
		},
		fdLimit: func(int) topFDLimit {
			if euid == 0 {
				return svcAvailableFD(1, 1024)
			}
			return svcDeniedFD()
		},
		listen: func() ([]listenSocket, error) {
			fake.listenCalls++
			return nil, nil
		},
		journal: func(ctx context.Context, _ string) (string, string, error) { return journal(ctx) },
		monotonic: func() (time.Duration, bool) {
			return 10 * time.Second, true
		},
	}
}

const svcActiveShow = "Id=app.service\nLoadState=loaded\nActiveState=active\nSubState=running\nResult=success\nMainPID=100\nNRestarts=0\nActiveEnterTimestampMonotonic=4000000\nControlGroup=/system.slice/app.service\n"

func TestCollectSvcFactsJournalSources(t *testing.T) {
	hint := func(context.Context) (string, string, error) {
		return "-- No entries --\n", "Hint: You are currently not seeing messages from other users and the system.\n", nil
	}
	fake := &svcFakeSources{}
	facts := collectSvcFacts(context.Background(), "app.service", fake.sources(1000, svcActiveShow, hint))
	if !errors.Is(facts.JournalErr, errSvcNeedsRoot) {
		t.Fatalf("journal hint without root = %v", facts.JournalErr)
	}
	if fake.listenCalls != 0 {
		t.Fatal("소켓 주인을 읽을 수 없는데 소켓을 모았다")
	}
	if !facts.UptimeKnown || facts.Uptime != 6*time.Second {
		t.Fatalf("uptime = %v, %v", facts.Uptime, facts.UptimeKnown)
	}

	missing := func(context.Context) (string, string, error) {
		return "", "", &exec.Error{Name: "journalctl", Err: exec.ErrNotFound}
	}
	facts = collectSvcFacts(context.Background(), "app.service", (&svcFakeSources{}).sources(0, svcActiveShow, missing))
	if journal := svcByID(t, evaluateSvc(facts), "journal"); journal.Status != StatusSkip {
		t.Fatalf("missing journalctl = %+v", journal)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hang := func(ctx context.Context) (string, string, error) { return "", "", ctx.Err() }
	facts = collectSvcFacts(ctx, "app.service", (&svcFakeSources{}).sources(0, svcActiveShow, hang))
	if !errors.Is(facts.JournalErr, context.Canceled) {
		t.Fatalf("canceled journal = %v", facts.JournalErr)
	}

	failed := func(context.Context) (string, string, error) {
		return "", "Failed to open journal: Bad message\n", errors.New("exit status 1")
	}
	facts = collectSvcFacts(context.Background(), "app.service", (&svcFakeSources{}).sources(0, svcActiveShow, failed))
	if facts.JournalErr == nil || !strings.Contains(facts.JournalErr.Error(), "Bad message") {
		t.Fatalf("failed journal = %v", facts.JournalErr)
	}

	fake = &svcFakeSources{}
	facts = collectSvcFacts(context.Background(), "app.service", fake.sources(0, svcActiveShow, hint))
	if facts.JournalErr != nil || fake.listenCalls != 1 {
		t.Fatalf("root: journal err %v, listen calls %d", facts.JournalErr, fake.listenCalls)
	}
}

func svcReadReport(t *testing.T, path string) Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestRunSvcExitCodes(t *testing.T) {
	quiet := func(context.Context) (string, string, error) { return "-- No entries --\n", "", nil }
	sources := (&svcFakeSources{}).sources(0, svcActiveShow, quiet)
	for _, args := range [][]string{nil, {"a", "b"}, {"--", "-p"}, {"x;y"}, {"foo.timer"}} {
		if code := runSvcWith(args, "test", sources, true); code != 2 {
			t.Errorf("runSvcWith(%q) = %d, want 2", args, code)
		}
	}

	output := filepath.Join(t.TempDir(), "report.json")
	if code := runSvcWith([]string{"--json", output, "app"}, "test", svcSources{}, false); code != 0 {
		t.Fatalf("unsupported exit = %d", code)
	}
	if report := svcReadReport(t, output); len(report.Results) != 1 || report.Results[0].Status != StatusSkip {
		t.Fatalf("unsupported = %+v", report.Results)
	}

	noSystemd := sources
	noSystemd.systemd = func() bool { return false }
	if code := runSvcWith([]string{"--json", output, "app"}, "test", noSystemd, true); code != 0 {
		t.Fatalf("no systemd exit = %d", code)
	}
	if report := svcReadReport(t, output); len(report.Results) != 1 || report.Results[0].Status != StatusSkip {
		t.Fatalf("no systemd = %+v", report.Results)
	}

	notFound := (&svcFakeSources{}).sources(0, "Id=nope.service\nLoadState=not-found\nActiveState=inactive\n", quiet)
	if code := runSvcWith([]string{"--json", output, "nope"}, "test", notFound, true); code != 1 {
		t.Fatalf("not found exit = %d", code)
	}

	if code := runSvcWith([]string{"--json", output, "app.service"}, "test", sources, true); code != 0 {
		t.Fatalf("healthy exit = %d", code)
	}
	if report := svcReadReport(t, output); len(report.Results) != 5 || report.Target["unit"] != "app.service" {
		t.Fatalf("healthy = %+v", report)
	}
}
