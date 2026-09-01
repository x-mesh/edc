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
	for _, args := range [][]string{nil, {"-v"}, {"--verbose"}} {
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
