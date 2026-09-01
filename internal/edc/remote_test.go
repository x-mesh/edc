package edc

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRemoteRunner struct {
	mu      sync.Mutex
	calls   []string
	results map[string]remoteCommandResult
	delay   time.Duration
	active  int
	maxSeen int
}

func (runner *fakeRemoteRunner) Run(_ context.Context, target, command string, _ time.Duration, stream io.Writer) remoteCommandResult {
	key := target + "|" + command
	runner.mu.Lock()
	runner.calls = append(runner.calls, key)
	result := runner.results[key]
	runner.active++
	if runner.active > runner.maxSeen {
		runner.maxSeen = runner.active
	}
	runner.mu.Unlock()
	if runner.delay > 0 {
		time.Sleep(runner.delay)
	}
	if stream != nil {
		_, _ = io.WriteString(stream, result.Output)
	}
	runner.mu.Lock()
	runner.active--
	runner.mu.Unlock()
	return result
}

func TestRemoteRunSequentialAndContinue(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one-alias"}, {Name: "two", Target: "two-alias"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "gk", Command: "gk update", Verify: "gk --version", Timeout: time.Minute}, {Name: "xm", Command: "xm update", Verify: "xm --version", Timeout: time.Minute}}}
	runner := &fakeRemoteRunner{results: map[string]remoteCommandResult{
		"one-alias|gk --version": {ExitCode: 1, Err: errors.New("not installed")},
	}}
	results := executeRemoteRecipe(context.Background(), hosts, recipe, runner)
	wantCalls := []string{"one-alias|gk update", "one-alias|gk --version", "two-alias|gk update", "two-alias|gk --version", "two-alias|xm update", "two-alias|xm --version"}
	if len(runner.calls) != len(wantCalls) {
		t.Fatalf("calls = %#v", runner.calls)
	}
	for index := range wantCalls {
		if runner.calls[index] != wantCalls[index] {
			t.Fatalf("calls = %#v", runner.calls)
		}
	}
	if len(results) != 4 || results[0].Status != StatusFail || results[1].Status != StatusSkip || results[2].Status != StatusPass || exitCode(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
}

func TestRemoteTaggedStepsRunOnMatchingHostsOnly(t *testing.T) {
	hosts := []remoteHost{{Name: "server", Target: "server", Tags: []string{"linux"}}, {Name: "laptop", Target: "laptop", Tags: []string{"mac"}}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "git-kit", Command: "git-kit update", Verify: "git-kit --version", Timeout: time.Minute},
		{Name: "brew", Command: "brew upgrade", Verify: "brew --version", Timeout: time.Minute, Tags: []string{"mac"}},
	}}
	runner := &fakeRemoteRunner{results: map[string]remoteCommandResult{}}
	results := executeRemoteRecipe(context.Background(), hosts, recipe, runner)
	want := []string{"remote.server.git-kit", "remote.laptop.git-kit", "remote.laptop.brew"}
	if len(results) != len(want) {
		t.Fatalf("results = %#v", results)
	}
	for index, probe := range want {
		if results[index].Probe != probe {
			t.Fatalf("probe %d = %q, want %q", index, results[index].Probe, probe)
		}
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "server|brew") {
			t.Fatalf("tagged step reached the wrong host: %#v", runner.calls)
		}
	}
}

func TestRemoteStepWithoutVerifyUsesCommandResult(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{
		{Name: "brew", Command: "brew update", Timeout: time.Minute},
		{Name: "broken", Command: "missing", Timeout: time.Minute},
	}}
	runner := &fakeRemoteRunner{results: map[string]remoteCommandResult{"one|missing": {ExitCode: 127, Err: errors.New("not found")}}}
	results := executeRemoteRecipe(context.Background(), hosts, recipe, runner)
	if len(runner.calls) != 2 {
		t.Fatalf("verify must not run: %#v", runner.calls)
	}
	if results[0].Status != StatusPass || results[0].Metrics["verify_status"] != "none" {
		t.Fatalf("result = %#v", results[0])
	}
	if !strings.Contains(results[0].Summary, "command를 실행했습니다") {
		t.Fatalf("summary = %q", results[0].Summary)
	}
	if results[1].Status != StatusFail {
		t.Fatalf("command failure must fail the step: %#v", results[1])
	}
}

func TestRemoteResultsStreamWhileRunning(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "gk", Command: "gk update", Verify: "gk --version", Timeout: time.Minute}, {Name: "xm", Command: "xm update", Verify: "xm --version", Timeout: time.Minute}}}
	runner := &fakeRemoteRunner{results: map[string]remoteCommandResult{"one|xm update": {ExitCode: 1, Err: errors.New("boom")}}}
	var output strings.Builder
	display := newRemoteDisplay(&output, remoteDisplayOptions{results: true})
	results := executeRemoteRecipeWithOptions(context.Background(), hosts, recipe, runner, 1, display)
	display.Close()
	text := output.String()
	passIndex := strings.Index(text, "PASS  one.gk")
	failIndex := strings.Index(text, "FAIL  one.xm")
	if passIndex < 0 || failIndex < passIndex {
		t.Fatalf("streamed results = %q", text)
	}
	if strings.Contains(text, "pass  ·") || strings.Contains(text, "ERROR  one.xm") {
		t.Fatalf("streamed output must carry result lines only: %q", text)
	}
	var tail strings.Builder
	printRemoteTail(&tail, results, false)
	if strings.Contains(tail.String(), "PASS  one.gk") {
		t.Fatalf("final output repeats result lines: %q", tail.String())
	}
	if !strings.Contains(tail.String(), "ERROR  one.xm") || !strings.Contains(tail.String(), "1 pass") {
		t.Fatalf("final output = %q", tail.String())
	}
}

func TestRemoteExecOutputLimit(t *testing.T) {
	buffer := &remoteLimitedBuffer{limit: 4}
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("write = %d, %v", written, err)
	}
	if buffer.String() != "abcd" || !buffer.Truncated() {
		t.Fatalf("buffer = %q, truncated = %v", buffer.String(), buffer.Truncated())
	}
}

func TestRemoteRunRejectsNonTerminalAndPartialFlags(t *testing.T) {
	if code := runRemoteRun(nil, "test"); code != 2 {
		t.Fatalf("non-terminal no-argument exit code = %d", code)
	}
	if code := runRemoteRun([]string{"--inventory", "inventory.yaml"}, "test"); code != 2 {
		t.Fatalf("partial flag exit code = %d", code)
	}
}

func TestRemoteInteractiveFlagDetection(t *testing.T) {
	for _, args := range [][]string{nil, {"-v"}, {"--verbose"}, {"-f"}, {"--force"}, {"-f", "-v"}} {
		if !onlyRemoteInteractiveFlags(args) {
			t.Fatalf("args %v must keep interactive mode", args)
		}
	}
	if onlyRemoteInteractiveFlags([]string{"-v", "--parallel", "2"}) {
		t.Fatal("non-global flags must use explicit mode")
	}
}

func TestRemoteExecutionErrorClassification(t *testing.T) {
	sshError := remoteExecutionError("command", time.Minute, remoteCommandResult{ExitCode: 255, Err: errors.New("authentication failed")})
	if sshError.Kind != "ssh" {
		t.Fatalf("ssh error = %#v", sshError)
	}
	timeoutError := remoteExecutionError("verify", time.Minute, remoteCommandResult{ExitCode: -1, TimedOut: true, Err: context.DeadlineExceeded})
	if timeoutError.Kind != "timeout" || !strings.Contains(timeoutError.Message, "verify timeout") {
		t.Fatalf("timeout error = %#v", timeoutError)
	}
}

func TestRemoteRunParallelHostsAndStableResults(t *testing.T) {
	hosts := []remoteHost{{Name: "one", Target: "one"}, {Name: "two", Target: "two"}, {Name: "three", Target: "three"}}
	recipe := remoteRecipe{Name: "daily", Steps: []remoteStep{{Name: "update", Command: "update", Verify: "verify", Timeout: time.Second}}}
	runner := &fakeRemoteRunner{results: map[string]remoteCommandResult{}, delay: 20 * time.Millisecond}
	results := executeRemoteRecipeWithOptions(context.Background(), hosts, recipe, runner, 2, nil)
	if runner.maxSeen != 2 {
		t.Fatalf("max concurrency = %d", runner.maxSeen)
	}
	for index, host := range hosts {
		if results[index].Metrics["host"] != host.Name {
			t.Fatalf("results are not in inventory order: %#v", results)
		}
	}
}
