package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func checkTestCommand(name, run string, timeout time.Duration, level string) checkCommandConfig {
	return checkCommandConfig{Name: name, Run: run, Timeout: &configDuration{Duration: timeout}, Level: level}
}

func TestRunCheckCommandJudgesTheExitCode(t *testing.T) {
	ctx := context.Background()
	if result := checkCommandResult(runCheckCommand(ctx, checkTestCommand("ok", "echo fine", 5*time.Second, ""))); result.Status != StatusPass || result.Probe != "check.cmd.ok" {
		t.Fatalf("exit 0 = %+v", result)
	}
	failed := checkCommandResult(runCheckCommand(ctx, checkTestCommand("bad", "echo first; echo 'connection refused' >&2; exit 3", 5*time.Second, "")))
	if failed.Status != StatusFail || failed.Summary != "exit 3: connection refused" || failed.Next == "" || len(failed.Evidence) != 1 {
		t.Fatalf("exit 3 = %+v", failed)
	}
	if result := checkCommandResult(runCheckCommand(ctx, checkTestCommand("soft", "exit 1", 5*time.Second, "warn"))); result.Status != StatusWarn {
		t.Fatalf("warn level = %+v", result)
	}
}

func TestRunCheckCommandStopsTheWholeProcessGroupOnTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	started := time.Now()
	outcome := runCheckCommand(context.Background(), checkTestCommand("slow", "sleep 30 & echo $! > "+pidFile+"; wait", 300*time.Millisecond, ""))
	if !outcome.TimedOut || time.Since(started) > 5*time.Second {
		t.Fatalf("outcome = %+v after %s", outcome, time.Since(started))
	}
	if result := checkCommandResult(outcome); result.Status != StatusFail || !strings.Contains(result.Summary, "300ms") {
		t.Fatalf("timeout result = %+v", result)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// sh가 띄운 sleep도 같은 process group이라 함께 끝나야 한다.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child %d still runs: %v", pid, err)
	}
}

func TestRunCheckCommandKeepsOnlyTheOutputLimit(t *testing.T) {
	outcome := runCheckCommand(context.Background(), checkTestCommand("loud", "i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done", 5*time.Second, ""))
	if outcome.Err != nil || len(outcome.Output) > checkCommandOutputLimit {
		t.Fatalf("output %d bytes, err %v", len(outcome.Output), outcome.Err)
	}
}

func TestCheckConfigSafeRefusesAFileOthersCanWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("lang = \"en\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkConfigSafe(path); err != nil {
		t.Fatalf("0600 config = %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := checkConfigSafe(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("0666 config = %v", err)
	}
}

func TestCheckRoutineAddsRulesFiltersChecksAndRunsCommands(t *testing.T) {
	config := checkConfig{
		AcceptUnits: []string{"a.service"},
		Limits:      checkLimitsConfig{Disk: []float64{70, 80}, Usage: []float64{50, 60}},
		Routines: map[string]checkRoutineConfig{"web": {
			Checks:       []string{"service", "disk"},
			AcceptUnits:  []string{"b.service"},
			RequireUnits: []string{"nginx.service"},
			Limits:       checkLimitsConfig{Disk: []float64{85, 95}},
			Accept:       []string{"cmd.soft"},
			Commands:     []checkCommandConfig{{Name: "health", Run: "true"}, {Name: "soft", Run: "false"}},
		}},
	}
	policy := newCheckPolicy(config, "web")
	if len(policy.acceptUnits) != 2 || policy.disk.warn != 85 || policy.usage.warn != 50 || len(policy.requireUnits) != 1 {
		t.Fatalf("policy = %+v", policy)
	}
	facts := checkFacts{
		Services: []svcListItem{{Unit: "nginx.service", LoadState: "loaded", ActiveState: "active"}},
		Disks:    []checkDisk{{Mount: "/", FSType: "ext4", Percent: 80}},
		Commands: []checkCommandOutcome{{Name: "health", Run: "true"}, {Name: "soft", Run: "false", ExitCode: 1, Err: errors.New("exit status 1")}},
	}
	results := evaluateCheck(facts, policy)
	var probes []string
	for _, result := range results {
		probes = append(probes, result.Probe+"="+string(result.Status))
	}
	want := "check.service=pass check.disk=pass check.cmd.health=pass check.cmd.soft=skip"
	if got := strings.Join(probes, " "); got != want {
		t.Fatalf("results = %s, want %s", got, want)
	}

	facts.CommandsErr = errors.New("config is unsafe")
	results = evaluateCheck(facts, policy)
	if last := results[len(results)-1]; last.Status != StatusSkip || !strings.Contains(last.Summary, "config is unsafe") {
		t.Fatalf("unsafe config = %+v", last)
	}
}

func TestCollectCheckFactsRunsCommandsOnlyFromASafeConfig(t *testing.T) {
	ran := 0
	sources := checkTestSources()
	sources.runCommand = func(_ context.Context, command checkCommandConfig) checkCommandOutcome {
		ran++
		return checkCommandOutcome{Name: command.Name}
	}
	commands := []checkCommandConfig{{Name: "one", Run: "true"}}
	if facts := collectCheckFacts(context.Background(), sources, commands); ran != 1 || len(facts.Commands) != 1 || facts.CommandsErr != nil {
		t.Fatalf("safe config ran %d, facts = %+v", ran, facts.Commands)
	}
	sources.configSafe = func() error { return errors.New("writable") }
	if facts := collectCheckFacts(context.Background(), sources, commands); ran != 1 || facts.CommandsErr == nil {
		t.Fatalf("unsafe config ran %d", ran)
	}
}

func TestRunCheckRejectsABadOrUnknownRoutine(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig = edcConfig{Check: checkConfig{Routines: map[string]checkRoutineConfig{"web": {}}}}
	for _, args := range [][]string{{"db"}, {"--routine", "db"}, {"web; rm -rf /"}, {"web", "--routine", "web"}, {"web", "db"}} {
		if code := runCheck(args, "test"); code != 2 {
			t.Errorf("%q: exit code %d, want 2", args, code)
		}
	}
}

func TestCheckGroupPassesTheRoutine(t *testing.T) {
	var command string
	run := func(_ context.Context, _ string, value string, _ time.Duration) remoteCapture {
		command = value
		return remoteCapture{Stdout: checkTestRemoteReport}
	}
	checkCollectHosts(context.Background(), []remoteHost{{Name: "alpha"}}, 1, 15*time.Second, "web", run)
	if command != "edc check --json - --timeout 15s web" {
		t.Fatalf("command = %q", command)
	}
}

func checkTestSources() checkSources {
	return checkSources{
		services:    func(context.Context) ([]svcListItem, error) { return nil, errSvcNoSystemd },
		processes:   func() ([]checkProcess, error) { return nil, nil },
		memory:      func() (infoMemory, error) { return infoMemory{Total: 100, Available: 50}, nil },
		disks:       func() ([]checkDisk, error) { return nil, nil },
		kernel:      func(context.Context, time.Duration) eventsFacts { return eventsFacts{} },
		clock:       func() *infoClock { return nil },
		network:     func() checkNetwork { return checkNetwork{} },
		containers:  func(context.Context) ([]ctrListItem, error) { return nil, errCtrNoDocker },
		fileHandles: func() (*infoFileHandles, error) { return nil, nil },
		reboot:      func() checkReboot { return checkReboot{} },
		configSafe:  func() error { return nil },
	}
}
