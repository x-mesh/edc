package edc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runSupervisedLog(t *testing.T, flags []string, mode string, arguments ...string) (int, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "job.log")
	args := append([]string{"--output", path}, flags...)
	args = append(args, "--")
	args = append(args, logHelperCommand(mode, arguments...)...)
	var stdout, stderr bytes.Buffer
	code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(content), stderr.String()
}

func TestLogBuiltInDefaultsRecordBothStreamsAndContext(t *testing.T) {
	restore := activeConfig
	defer func() { activeConfig = restore }()
	activeConfig = edcConfig{}
	code, content, stderr := runSupervisedLog(t, nil, "emit")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"stream=both", "stdout-data", "stderr-data", "cwd=", "pid=", "executable=", "status=exit exit=0"} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %s: %s", want, content)
		}
	}
	options, ok := parseLogOptions([]string{"--", "job"}, io.Discard)
	if !ok || options.output != "" || options.stream != "both" || options.timeout != 0 || options.restart != "never" || options.maxSizeMB != 10 || options.keepFiles != 3 {
		t.Fatalf("defaults=%+v, valid=%v", options, ok)
	}
}

func TestLogRestartRecoversAndStopsAtTheConfiguredLimit(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "attempts")
	code, content, stderr := runSupervisedLog(t, []string{"--restart", "on-failure", "--restart-delay", "0s"}, "count", counter)
	if code != 0 || stderr != "" || strings.Count(content, "=== edc log start time=") != 3 || strings.Count(content, "=== edc log restart") != 2 {
		t.Fatalf("code=%d stderr=%q content=%q", code, stderr, content)
	}
	code, content, _ = runSupervisedLog(t, []string{"--restart", "on-failure", "--max-restarts", "2", "--restart-delay", "0s"}, "exit7")
	if code != 7 || strings.Count(content, "=== edc log start time=") != 3 {
		t.Fatalf("bounded restart code=%d log=%q", code, content)
	}
	code, content, _ = runSupervisedLog(t, []string{"--restart", "on-failure"}, "emit")
	if code != 0 || strings.Count(content, "=== edc log start time=") != 1 {
		t.Fatal("a successful job was retried")
	}
	code, content, _ = runSupervisedLog(t, []string{"--restart", "always", "--max-restarts", "1", "--restart-delay", "0s"}, "emit")
	if code != 0 || strings.Count(content, "=== edc log start time=") != 2 {
		t.Fatal("always restart did not include successful exits")
	}
	code, content, _ = runSupervisedLog(t, []string{"--restart", "on-failure", "--max-restarts", "0"}, "exit7")
	if code != 7 || strings.Count(content, "=== edc log start time=") != 1 {
		t.Fatal("zero restart limit was ignored")
	}
}

func TestLogTimeoutEscalatesAndBoundsInheritedOutputPipes(t *testing.T) {
	for _, mode := range []string{"wait-signal", "ignore-term", "grandchild"} {
		t.Run(mode, func(t *testing.T) {
			started := time.Now()
			code, content, stderr := runSupervisedLog(t, []string{"--timeout", "300ms", "--kill-after", "50ms"}, mode)
			if code != 124 || stderr != "" || !strings.Contains(content, "status=timeout exit=124") || time.Since(started) > 3*time.Second {
				t.Fatalf("code=%d stderr=%q duration=%s content=%s", code, stderr, time.Since(started), content)
			}
			if mode != "wait-signal" && strings.Contains(content, "ready") && !strings.Contains(content, "forced_kill=true") {
				t.Fatalf("SIGKILL escalation missing: %s", content)
			}
		})
	}
}

func TestLogStartupFailureRecordsCauseAndDoesNotRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-command")
	logPath := filepath.Join(t.TempDir(), "job.log")
	var errors bytes.Buffer
	args := []string{"--output", logPath, "--restart", "always", "--restart-delay", "0s", "--", path}
	code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &errors})
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 || !strings.Contains(string(data), path) || !strings.Contains(string(data), "cause=") || strings.Contains(string(data), "=== edc log restart") {
		t.Fatalf("start error code=%d log=%s", code, data)
	}
}

func TestLogSupervisorWrapperProcess(t *testing.T) {
	for index, arg := range os.Args {
		if arg == "--edc-log-supervisor" {
			os.Exit(runLogWithStreams(os.Args[index+1:], logStreams{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}))
		}
	}
}

func TestLogExternalStopPreventsRestartDuringRunAndDelay(t *testing.T) {
	for _, mode := range []string{"ignore-term", "exit7"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job.log")
			args := []string{"-test.run=^TestLogSupervisorWrapperProcess$", "--", "--edc-log-supervisor", "--output", path, "--restart", "always", "--restart-delay", "30s", "--kill-after", "50ms", "--"}
			args = append(args, logHelperCommand(mode)...)
			process := exec.Command(os.Args[0], args...)
			var stderr bytes.Buffer
			process.Stderr = &stderr
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
			marker := "ready"
			if mode == "exit7" {
				marker = "=== edc log restart"
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				data, _ := os.ReadFile(path)
				if strings.Contains(string(data), marker) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("wrapper did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := process.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- process.Wait() }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("stop signal did not cancel the run or delay")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if process.ProcessState.ExitCode() != 143 || strings.Count(string(data), "=== edc log start time=") != 1 || !strings.Contains(string(data), "signal=SIGTERM") {
				t.Fatalf("exit=%d stderr=%q log=%s", process.ProcessState.ExitCode(), stderr.String(), data)
			}
			if mode == "ignore-term" && !strings.Contains(string(data), "forced_kill=true") {
				t.Fatal("a ready child that ignores SIGTERM was not forcibly terminated")
			}
		})
	}
}

func TestLogRotationDuringCommandRetainsExitStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	args := []string{"--output", path, "--max-size", "1", "--keep-files", "1", "--"}
	args = append(args, logHelperCommand("burst")...)
	code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard})
	data, err := os.ReadFile(path)
	if code != 0 || err != nil || !strings.Contains(string(data), "status=exit exit=0") {
		t.Fatalf("exit=%d err=%v", code, err)
	}
	for _, name := range []string{path, path + ".edc.1"} {
		info, err := os.Stat(name)
		if err != nil || info.Size() > logMiB {
			t.Fatalf("rotation %s info=%v err=%v", name, info, err)
		}
	}
	if _, err := os.Stat(path + ".edc.2"); !os.IsNotExist(err) {
		t.Fatal("retention limit was ignored")
	}
}

func TestLogRejectsInvalidSupervisionBeforeItExecutes(t *testing.T) {
	for _, flag := range [][]string{{"--max-size", "-1"}, {"--max-size", fmt.Sprint(logMaxSizeMB + 1)}, {"--keep-files", "0"}, {"--keep-files", "101"}, {"--restart", "yes"}, {"--max-restarts", "-1"}, {"--restart-delay", "-1s"}, {"--timeout", "-1s"}, {"--kill-after", "0s"}} {
		args := append(append([]string(nil), flag...), "--", "job")
		if _, ok := parseLogOptions(args, io.Discard); ok {
			t.Fatalf("accepted flags %v", flag)
		}
	}
}

func TestLogRotationFailureStopsTheChildWithoutRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.Mkdir(path+".edc.1", 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--output", path, "--max-size", "1", "--restart", "always", "--restart-delay", "0s", "--kill-after", "50ms", "--"}
	args = append(args, logHelperCommand("burst-wait")...)
	var stderr bytes.Buffer
	started := time.Now()
	code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr})
	if code != 2 || stderr.Len() == 0 || time.Since(started) > 3*time.Second {
		t.Fatalf("code=%d stderr=%q duration=%s", code, stderr.String(), time.Since(started))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "=== edc log start time=") != 1 {
		t.Fatal("log write failure caused a retry")
	}
}

func TestLogSharedOutputUsesTheSameLockAcrossRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	type result struct {
		code   int
		stderr string
	}
	done := make(chan result, 2)
	run := func(label string) {
		args := []string{"--output", path, "--max-size", "1", "--keep-files", "10", "--"}
		args = append(args, logHelperCommand("burst-label", label)...)
		var stderr bytes.Buffer
		code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr})
		done <- result{code, stderr.String()}
	}
	go run("first")
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "first-begin") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go run("second")
	for range 2 {
		select {
		case result := <-done:
			if result.code != 0 {
				t.Fatalf("code=%d stderr=%s", result.code, result.stderr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shared commands did not complete")
		}
	}
	var content strings.Builder
	for index := 10; index >= 0; index-- {
		name := path
		if index > 0 {
			name += fmt.Sprintf(".edc.%d", index)
		}
		data, err := os.ReadFile(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		content.Write(data)
	}
	text := content.String()
	firstEnd, secondBegin := strings.Index(text, "first-end"), strings.Index(text, "second-begin")
	if firstEnd < 0 || secondBegin < firstEnd || strings.Count(text, "=== edc log start time=") != 2 || strings.Count(text, "=== edc log end") != 2 {
		t.Fatal("rotation lost or mixed command output")
	}
}

func TestLogRetriesChildSignalsAndTimeoutsWithVisibleLimit(t *testing.T) {
	for _, test := range []struct {
		mode  string
		flags []string
		code  int
	}{
		{"signal", nil, 143},
		{"wait-signal", []string{"--timeout", "100ms", "--kill-after", "50ms"}, 124},
	} {
		flags := append([]string{"--restart", "on-failure", "--max-restarts", "1", "--restart-delay", "0s"}, test.flags...)
		code, content, _ := runSupervisedLog(t, flags, test.mode)
		if code != test.code || strings.Count(content, "=== edc log start time=") != 2 || !strings.Contains(content, "reason=restart_limit attempts=2") {
			t.Fatalf("mode=%s code=%d log=%s", test.mode, code, content)
		}
	}
}

func TestLogTimeoutKillsGroupMembersWithoutRecordedOutput(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	code, content, stderr := runSupervisedLog(t, []string{"--timeout", "700ms", "--kill-after", "50ms"}, "closed-output-child", ready)
	if !strings.Contains(content, "forced_kill=true") {
		_, tail, ok := strings.Cut(content, "grandchild-pid=")
		var pid int
		if ok {
			_, _ = fmt.Sscanf(tail, "%d", &pid)
		}
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Fatalf("a child without output survived group timeout: code=%d stderr=%q log=%s", code, stderr, content)
	}
	if code != 124 || stderr != "" || !strings.Contains(content, "ready") {
		t.Fatalf("code=%d stderr=%q log=%s", code, stderr, content)
	}
}

func TestLogFailurePolicyStopsRemainingGroupBeforeReturning(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job.log")
	args := []string{"--output", path, "--restart", "on-failure", "--max-restarts", "0", "--kill-after", "50ms", "--"}
	args = append(args, logHelperCommand("failed-with-child", filepath.Join(root, "ready"))...)
	done := make(chan int, 1)
	go func() {
		done <- runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard})
	}()
	select {
	case code := <-done:
		data, err := os.ReadFile(path)
		if code != 7 || err != nil || !strings.Contains(string(data), "forced_kill=true") {
			t.Fatalf("code=%d err=%v log=%s", code, err, data)
		}
	case <-time.After(2 * time.Second):
		data, _ := os.ReadFile(path)
		_, tail, ok := strings.Cut(string(data), "grandchild-pid=")
		var pid int
		if ok {
			_, _ = fmt.Sscanf(tail, "%d", &pid)
		}
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("failure policy waited forever for a remaining group member's output")
	}
}
