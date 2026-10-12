package edc

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctrTestNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func ctrInspectLine(status string, restarts int, oom bool, exitCode int, health string) string {
	started := ctrTestNow.Add(-90 * time.Minute).Format(time.RFC3339Nano)
	finished := "0001-01-01T00:00:00Z"
	pid := 100
	if status != "running" {
		finished = ctrTestNow.Add(-time.Minute).Format(time.RFC3339Nano)
		pid = 0
	}
	return strings.Join([]string{itoa(pid), "/web", status, started, finished, itoa(restarts), boolText(oom), itoa(exitCode), "nginx:1.27", "0123456789abcdef0123456789abcdef", health}, "|")
}

func itoa(value int) string { return strconv.Itoa(value) }

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func ctrTestSources(inspect string, inspectErr error) ctrSources {
	return ctrSources{
		euid:    0,
		docker:  func() bool { return true },
		inspect: func(context.Context, string) (string, error) { return inspect, inspectErr },
		cgroupPath: func(pid int) (string, error) {
			return "/system.slice/docker-0123.scope", nil
		},
		limits: func(path string) ctrLimits {
			used, maximum, oom, kill := uint64(512<<20), uint64(1<<30), uint64(0), uint64(0)
			return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
				Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
				Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: &oom, OOMKill: &kill},
				CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 1000, Throttled: 10, ThrottledUS: 5000},
				Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 12, Max: 4096}}
		},
		processes: func(context.Context, string, int) ([]topProcess, error) {
			return []topProcess{{PID: 101, CPU: 3, RSS: 2 << 20, Command: "nginx"}, {PID: 100, CPU: 1, RSS: 4 << 20, Command: "nginx"}}, nil
		},
		fdLimit: func(int) topFDLimit { return svcAvailableFD(10, 1024) },
		listen: func() ([]listenSocket, error) {
			return []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 8080, PID: "101", Process: "nginx"}, {Proto: "tcp", Address: "127.0.0.1", Port: 5432, PID: "999", Process: "postgres"}}, nil
		},
		logs: func(context.Context, string, int) (string, error) {
			return "2026-10-12T11:59:00Z info ready\n2026-10-12T11:59:30Z [error] upstream timed out\n2026-10-12T11:59:40Z info ok\n", nil
		},
		now: func() time.Time { return ctrTestNow },
	}
}

func ctrByID(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, result := range results {
		if result.Probe == "ctr."+id {
			return result
		}
	}
	t.Fatalf("ctr.%s가 없다: %+v", id, results)
	return Result{}
}

func TestParseCtrInspect(t *testing.T) {
	inspect, err := parseCtrInspect("4321|/web|running|2026-10-12T10:30:00.123456789Z|0001-01-01T00:00:00Z|2|true|137|nginx:1.27|abcdef0123456789|healthy\n")
	if err != nil || inspect.PID != 4321 || inspect.Name != "web" || inspect.Status != "running" || inspect.RestartCount != 2 || !inspect.OOMKilled || inspect.ExitCode != 137 || inspect.Image != "nginx:1.27" || inspect.ID != "abcdef0123456789" || inspect.Health != "healthy" {
		t.Fatalf("inspect = %+v, %v", inspect, err)
	}
	if inspect.StartedAt.Year() != 2026 || !inspect.FinishedAt.IsZero() {
		t.Fatalf("times = %v %v", inspect.StartedAt, inspect.FinishedAt)
	}
	for _, bad := range []string{"", "x|y", "p|/web|running|t|t|0|false|0|img|id|", "4321|/web|running|t|t|r|false|0|img|id|"} {
		if _, err := parseCtrInspect(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	for input, valid := range map[string]bool{"web": true, "my_app-1.2/x": true, "0123abcd": true, "": false, "-x": false, "a b": false, "a;b": false, "$(id)": false} {
		if _, err := normalizeCtrReference(input); (err == nil) != valid {
			t.Errorf("reference %q valid = %v, want %v", input, err == nil, valid)
		}
	}
}

func TestFilterCtrLogKeepsTheLastErrorLines(t *testing.T) {
	var lines []string
	for index := range 8 {
		lines = append(lines, "ok line", "ERROR number "+itoa(index))
	}
	log := filterCtrLog(strings.Join(lines, "\n") + "\n\n")
	if log.Scanned != 16 || len(log.Lines) != ctrLogShown || log.Lines[ctrLogShown-1] != "ERROR number 7" || log.Lines[0] != "ERROR number 3" {
		t.Fatalf("log = %+v", log)
	}
	if clean := filterCtrLog("info started\nwarning nothing\n"); clean.Scanned != 2 || len(clean.Lines) != 0 {
		t.Fatalf("clean = %+v", clean)
	}
	// "errors" 같은 낱말 일부는 맞추지 않지만 Traceback과 OOM은 맞춘다.
	if mixed := filterCtrLog("no errors here\nTraceback (most recent call last):\nOut of memory: killed\n"); len(mixed.Lines) != 2 {
		t.Fatalf("mixed = %+v", mixed)
	}
}

func TestEvaluateCtrRunningContainer(t *testing.T) {
	facts := collectCtrFacts(context.Background(), "web", ctrTestSources(ctrInspectLine("running", 0, false, 0, "healthy"), nil))
	results := evaluateCtr(facts)
	if len(results) != 6 {
		t.Fatalf("results = %d: %+v", len(results), results)
	}
	for _, result := range results {
		if !strings.HasPrefix(result.Probe, "ctr.") {
			t.Errorf("probe %s is not ctr.*", result.Probe)
		}
	}
	state := ctrByID(t, results, "state")
	if state.Status != StatusPass || !strings.Contains(state.Summary, "running (healthy), up 1h 30m, 0 restarts, image nginx:1.27") || state.Metrics["id"] != "0123456789ab" {
		t.Fatalf("state = %+v", state)
	}
	processes := ctrByID(t, results, "processes")
	if processes.Status != StatusPass || !strings.Contains(processes.Evidence[0].Value, "(main)") {
		t.Fatalf("processes = %+v", processes)
	}
	limits := ctrByID(t, results, "limits")
	if limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 512.0MiB / 1.0GiB (50%) · OOM kills 0 · CPU throttled 10/1000 periods (1%) · pids 12 / 4096 (0%)") {
		t.Fatalf("limits = %+v", limits)
	}
	if listen := ctrByID(t, results, "listen"); listen.Status != StatusPass || !strings.Contains(listen.Summary, "tcp 0.0.0.0:8080") || strings.Contains(listen.Summary, "5432") {
		t.Fatalf("listen = %+v", listen)
	}
	log := ctrByID(t, results, "log")
	if log.Status != StatusWarn || !strings.Contains(log.Summary, "1 error lines in the last 3 log lines") || !strings.Contains(log.Evidence[0].Value, "upstream timed out") {
		t.Fatalf("log = %+v", log)
	}
}

func TestEvaluateCtrStateAndLimitStatuses(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		status Status
		text   string
	}{
		{"restarted", ctrInspectLine("running", 2, false, 0, ""), StatusWarn, "2 restarts"},
		{"oom killed", ctrInspectLine("running", 0, true, 0, ""), StatusWarn, "OOM killed"},
		{"unhealthy", ctrInspectLine("running", 0, false, 0, "unhealthy"), StatusWarn, "running (unhealthy)"},
		{"exited", ctrInspectLine("exited", 0, false, 1, ""), StatusFail, "exited, exit code 1"},
		{"paused", ctrInspectLine("paused", 0, false, 0, ""), StatusWarn, "paused"},
	}
	for _, test := range cases {
		facts := collectCtrFacts(context.Background(), "web", ctrTestSources(test.line, nil))
		state := ctrByID(t, evaluateCtr(facts), "state")
		if state.Status != test.status || !strings.Contains(state.Summary, test.text) {
			t.Errorf("%s: %s %q, want %s containing %q", test.name, state.Status, state.Summary, test.status, test.text)
		}
	}
	// 멈춘 container는 process 관련 결과가 skip이고, 로그는 여전히 읽는다.
	stopped := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources(ctrInspectLine("exited", 0, false, 1, ""), nil)))
	for _, id := range []string{"processes", "limits", "fd", "listen"} {
		if result := ctrByID(t, stopped, id); result.Status != StatusSkip || !strings.Contains(result.Summary, "container has no running process") {
			t.Errorf("stopped %s = %+v", id, result)
		}
	}
	if log := ctrByID(t, stopped, "log"); log.Status != StatusWarn {
		t.Errorf("stopped log = %+v", log)
	}
	// 한도에 가까우면 warn이다.
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.limits = func(path string) ctrLimits {
		used, maximum, oom, kill := uint64(950<<20), uint64(1000<<20), uint64(1), uint64(1)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: &oom, OOMKill: &kill},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 100, Throttled: 40},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 95, Max: 100}}
	}
	limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits")
	if limits.Status != StatusWarn || len(limits.Warnings) != 4 || limits.Metrics["memory_percent"] != 95.0 || limits.Metrics["oom_kills"] != uint64(1) {
		t.Fatalf("near limits = %+v", limits)
	}
	// 한도가 없으면 그렇게 적고 경고하지 않는다.
	sources.limits = func(path string) ctrLimits {
		used := uint64(1 << 20)
		return ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path,
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Unlimited: true},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unsupported", Reason: "no memory.events.local"}},
			CPU:    ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}},
			Pids:   ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 3, Unlimited: true}}
	}
	if limits := ctrByID(t, evaluateCtr(collectCtrFacts(context.Background(), "web", sources)), "limits"); limits.Status != StatusPass || !strings.Contains(limits.Summary, "memory 1.0MiB / no limit · OOM kills - · CPU throttled no limit · pids 3 / no limit") {
		t.Fatalf("unlimited = %+v", limits)
	}
}

func TestEvaluateCtrUnreadableSourcesSkip(t *testing.T) {
	sources := ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil)
	sources.cgroupPath = func(int) (string, error) { return "", errors.New("open /proc/100/cgroup: no such file") }
	sources.logs = func(context.Context, string, int) (string, error) {
		return "permission denied", errors.New("exit status 1")
	}
	results := evaluateCtr(collectCtrFacts(context.Background(), "web", sources))
	for _, id := range []string{"processes", "limits", "log"} {
		if result := ctrByID(t, results, id); result.Status != StatusSkip {
			t.Errorf("%s = %+v, want skip", id, result)
		}
	}
	noDocker := ctrTestSources("", nil)
	noDocker.docker = func() bool { return false }
	if results := evaluateCtr(collectCtrFacts(context.Background(), "web", noDocker)); len(results) != 1 || results[0].Status != StatusSkip || results[0].Probe != "ctr" {
		t.Fatalf("no docker = %+v", results)
	}
	failed := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources("Error response from daemon: something", errors.New("exit status 1"))))
	if len(failed) != 1 || failed[0].Status != StatusFail || failed[0].Error == nil || !strings.Contains(failed[0].Summary, "something") {
		t.Fatalf("inspect failure = %+v", failed)
	}
	// docker socket 권한이 없으면 container를 판정할 수 없다. fail이 아니라 skip이다.
	denied := evaluateCtr(collectCtrFacts(context.Background(), "web", ctrTestSources("permission denied while trying to connect to the docker API at unix:///var/run/docker.sock", errors.New("exit status 1"))))
	if len(denied) != 1 || denied[0].Status != StatusSkip || !strings.Contains(denied[0].Summary, "docker group") {
		t.Fatalf("denied = %+v", denied)
	}
}

func TestRunCtrExitCodes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	stdout := os.Stdout
	os.Stdout = devNull
	defer func() { os.Stdout = stdout }()
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 0 {
		t.Fatalf("running = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources(ctrInspectLine("exited", 0, false, 1, ""), nil), true); code != 1 {
		t.Fatalf("exited = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrTestSources("Error response from daemon: No such container: web", errors.New("exit status 1")), true); code != 1 {
		t.Fatalf("not found = %d", code)
	}
	if code := runCtrWith([]string{"web"}, "test", ctrSources{}, false); code != 0 {
		t.Fatalf("unsupported = %d", code)
	}
	for _, args := range [][]string{{}, {"-x"}, {"a", "b"}, {"--timeout", "x", "web"}} {
		if code := runCtrWith(args, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 2 {
			t.Errorf("args %q = %d, want 2", args, code)
		}
	}
	if code := runCtrWith([]string{"web", "-v"}, "test", ctrTestSources(ctrInspectLine("running", 0, false, 0, ""), nil), true); code != 0 {
		t.Fatalf("option after the name = %d", code)
	}
}
