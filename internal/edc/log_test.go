package edc

import (
	"bytes"
	"errors"
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

const logHelperMarker = "--edc-log-helper"

// TestLogHelperProcess is started as a child test executable. Keeping signal
// behavior in a subprocess prevents the parent `go test` process from dying.
func TestLogHelperProcess(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == logHelperMarker {
			marker = index
			break
		}
	}
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}
	arguments := os.Args[marker+1:]
	switch arguments[0] {
	case "emit":
		fmt.Fprint(os.Stdout, "stdout-data")
		fmt.Fprint(os.Stderr, "stderr-data")
	case "stdin":
		_, _ = io.Copy(os.Stdout, os.Stdin)
	case "env-cwd":
		cwd, _ := os.Getwd()
		fmt.Fprintf(os.Stdout, "%s|%s", os.Getenv("EDC_LOG_TEST_VALUE"), cwd)
	case "exit7":
		os.Exit(7)
	case "signal":
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(time.Second)
		os.Exit(99)
	case "sleep":
		label := arguments[1]
		fmt.Fprintf(os.Stdout, "%s-begin\n", label)
		time.Sleep(175 * time.Millisecond)
		fmt.Fprintf(os.Stdout, "%s-end\n", label)
	case "wait-signal":
		fmt.Fprintln(os.Stdout, "ready")
		for {
			time.Sleep(time.Second)
		}
	default:
		os.Exit(98)
	}
	os.Exit(0)
}

func TestLogWrapperProcess(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == "--edc-log-wrapper" {
			marker = index
			break
		}
	}
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}
	path := os.Args[marker+1]
	args := []string{"--stream", "stdout", "--output", path, "--"}
	args = append(args, logHelperCommand("wait-signal")...)
	os.Exit(runLogWithStreams(args, logStreams{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}))
}

func logHelperCommand(mode string, arguments ...string) []string {
	command := []string{os.Args[0], "-test.run=^TestLogHelperProcess$", "--", logHelperMarker, mode}
	return append(command, arguments...)
}

func runLogTest(t *testing.T, stream, output string, stdin io.Reader, mode string, arguments ...string) (int, string, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := logHelperCommand(mode, arguments...)
	args := []string{"--stream", stream, "--output", output, "--"}
	args = append(args, command...)
	code := runLogWithStreams(args, logStreams{stdin: stdin, stdout: &stdout, stderr: &stderr})
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String(), string(content)
}

func TestLogCapturesOnlySelectedStream(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job.log")
			code, stdout, stderr, content := runLogTest(t, stream, path, strings.NewReader(""), "emit")
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			if stream == "stdout" {
				if stdout != "" || stderr != "stderr-data" || !strings.Contains(content, "stdout-data\n=== edc log end") || strings.Contains(content, "stderr-data") {
					t.Fatalf("stdout capture mismatch: stdout=%q stderr=%q log=%q", stdout, stderr, content)
				}
			} else if stdout != "stdout-data" || stderr != "" || !strings.Contains(content, "stderr-data\n=== edc log end") || strings.Contains(content, "stdout-data") {
				t.Fatalf("stderr capture mismatch: stdout=%q stderr=%q log=%q", stdout, stderr, content)
			}
			if !strings.Contains(content, "status=exit exit=0") || !strings.Contains(content, "duration=") {
				t.Fatalf("missing end status: %q", content)
			}
		})
	}
}

func TestLogPassesStdinEnvironmentAndWorkingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdin.log")
	code, _, stderr, content := runLogTest(t, "stdout", path, strings.NewReader("from-stdin\n"), "stdin")
	if code != 0 || stderr != "" || !strings.Contains(content, "from-stdin\n=== edc log end") {
		t.Fatalf("stdin: code=%d stderr=%q log=%q", code, stderr, content)
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDC_LOG_TEST_VALUE", "inherited")
	path = filepath.Join(t.TempDir(), "env.log")
	code, _, stderr, content = runLogTest(t, "stdout", path, strings.NewReader(""), "env-cwd")
	if code != 0 || stderr != "" || !strings.Contains(content, "inherited|"+cwd) {
		t.Fatalf("inheritance: code=%d stderr=%q log=%q", code, stderr, content)
	}
}

func TestLogAppendsAndPreservesFileModes(t *testing.T) {
	directory := t.TempDir()
	created := filepath.Join(directory, "created.log")
	if code, _, stderr, _ := runLogTest(t, "stdout", created, strings.NewReader(""), "emit"); code != 0 {
		t.Fatalf("created exit=%d stderr=%q", code, stderr)
	}
	info, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("created mode = %o", info.Mode().Perm())
	}

	existing := filepath.Join(directory, "existing.log")
	if err := os.WriteFile(existing, []byte("earlier\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o640); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, _, stderr, _ := runLogTest(t, "stdout", existing, strings.NewReader(""), "emit"); code != 0 {
			t.Fatalf("append exit=%d stderr=%q", code, stderr)
		}
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), "earlier\n") || strings.Count(string(content), "=== edc log start") != 2 || info.Mode().Perm() != 0o640 {
		t.Fatalf("append/mode mismatch: mode=%o log=%q", info.Mode().Perm(), content)
	}
}

func TestLogCreatesOnlyTheRecommendedParentDirectory(t *testing.T) {
	root := t.TempDir()
	recommended := filepath.Join(root, "state", "edc", "edc.log")
	if err := ensureRecommendedLogDirectoryFor(recommended, recommended); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(recommended)); err != nil || !info.IsDir() {
		t.Fatalf("recommended parent: info=%v err=%v", info, err)
	}
	custom := filepath.Join(root, "custom", "job.log")
	if err := ensureRecommendedLogDirectoryFor(custom, recommended); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(custom)); !os.IsNotExist(err) {
		t.Fatalf("custom parent was created: %v", err)
	}
}

func TestLogCommandDisplayModesAndASCII(t *testing.T) {
	base := logOptions{stream: "stderr", command: []string{"/tmp/실행 파일", "--token", "秘密"}}
	for _, row := range []struct {
		mode   string
		want   string
		absent string
	}{
		{"full", `command=["/tmp/\uc2e4\ud589 \ud30c\uc77c","--token","\u79d8\u5bc6"]`, ""},
		{"name", `command=["\uc2e4\ud589 \ud30c\uc77c"]`, "--token"},
		{"none", "stream=stderr ===", "command="},
	} {
		var output bytes.Buffer
		base.commandDisplay = row.mode
		if err := writeLogStart(&output, time.Now(), base); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), row.want) || (row.absent != "" && strings.Contains(output.String(), row.absent)) {
			t.Errorf("%s marker = %q", row.mode, output.String())
		}
		for _, value := range []byte(output.String()) {
			if value >= 0x80 {
				t.Fatalf("%s marker is not ASCII: %q", row.mode, output.String())
			}
		}
	}
}

func TestLogReturnsChildExitAndSignal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exit.log")
	code, _, _, content := runLogTest(t, "stdout", path, strings.NewReader(""), "exit7")
	if code != 7 || !strings.Contains(content, "status=exit exit=7") {
		t.Fatalf("non-zero: code=%d log=%q", code, content)
	}

	path = filepath.Join(t.TempDir(), "signal.log")
	code, _, _, content = runLogTest(t, "stdout", path, strings.NewReader(""), "signal")
	if code != 128+int(syscall.SIGTERM) || !strings.Contains(content, "status=signal signal=SIGTERM exit=143") {
		t.Fatalf("signal: code=%d log=%q", code, content)
	}
}

func TestLogForwardsSignalWithoutKillingTestProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forward.log")
	process := exec.Command(os.Args[0], "-test.run=^TestLogWrapperProcess$", "--", "--edc-log-wrapper", path)
	var stderr bytes.Buffer
	process.Stderr = &stderr
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		content, _ := os.ReadFile(path)
		if strings.Contains(string(content), "ready\n") {
			break
		}
		if time.Now().After(deadline) {
			_ = process.Process.Kill()
			_ = process.Wait()
			t.Fatalf("child did not become ready: stderr=%q log=%q", stderr.String(), content)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := process.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 143 {
		t.Fatalf("wrapper wait=%v exit=%d stderr=%q", err, process.ProcessState.ExitCode(), stderr.String())
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(content), "status=signal signal=SIGTERM exit=143") {
		t.Fatalf("forwarded signal log = %q", content)
	}
}

func TestLogLockWaitCanBeInterrupted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.log")
	first, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if interrupted, err := lockLogFile(first, make(chan os.Signal)); err != nil || interrupted != nil {
		t.Fatalf("first lock: signal=%v err=%v", interrupted, err)
	}
	defer unlockLogFile(first)
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	interrupted, err := lockLogFile(second, signals)
	if err != nil || interrupted != syscall.SIGTERM {
		t.Fatalf("waiting lock: signal=%v err=%v", interrupted, err)
	}
}

func TestLogRejectsInvalidArgumentsAndRecordsStartError(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "job.log")
	cases := [][]string{
		{"--output", validPath, "--", "echo"},
		{"--stream", "both", "--output", validPath, "--", "echo"},
		{"--stream", "stdout", "--", "echo"},
		{"--stream", "stdout", "--output", "-", "--", "echo"},
		{"--stream", "stdout", "--output", validPath, "--command-display", "secret", "--", "echo"},
		{"--stream", "stdout", "--output", validPath, "echo"},
		{"--stream", "stdout", "early", "--output", validPath, "--", "echo"},
		{"--stream", "stdout", "--output", validPath, "--"},
		{"--stream", "stdout", "--output", filepath.Join(t.TempDir(), "absent", "job.log"), "--", "echo"},
	}
	for index, args := range cases {
		var stderr bytes.Buffer
		if code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr}); code != 2 {
			t.Errorf("case %d exit=%d stderr=%q", index, code, stderr.String())
		}
	}

	path := filepath.Join(t.TempDir(), "start-error.log")
	missing := filepath.Join(t.TempDir(), "missing-command")
	var stderr bytes.Buffer
	code := runLogWithStreams([]string{"--stream", "stdout", "--output", path, "--", missing}, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 || !strings.Contains(string(content), "status=start_error exit=2") || stderr.Len() == 0 {
		t.Fatalf("start error: code=%d stderr=%q log=%q", code, stderr.String(), content)
	}
}

func TestLogCopyDrainsAfterWriteError(t *testing.T) {
	reader := strings.NewReader(strings.Repeat("payload", 10_000))
	want := errors.New("disk full")
	result := copyLogStream(errorWriter{err: want}, reader)
	if !errors.Is(result.writeErr, want) {
		t.Fatalf("write error = %v", result.writeErr)
	}
	if reader.Len() != 0 {
		t.Fatalf("reader still has %d bytes after write failure", reader.Len())
	}
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestLogSerializesConcurrentRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.log")
	type result struct {
		code   int
		stderr string
	}
	run := func(label string, done chan<- result) {
		var stderr bytes.Buffer
		args := []string{"--stream", "stdout", "--output", path, "--"}
		args = append(args, logHelperCommand("sleep", label)...)
		code := runLogWithStreams(args, logStreams{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr})
		done <- result{code: code, stderr: stderr.String()}
	}
	done := make(chan result, 2)
	go run("first", done)
	deadline := time.Now().Add(3 * time.Second)
	for {
		content, _ := os.ReadFile(path)
		if strings.Contains(string(content), "first-begin") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first command did not start: %q", content)
		}
		time.Sleep(10 * time.Millisecond)
	}
	go run("second", done)
	for range 2 {
		result := <-done
		if result.code != 0 {
			t.Fatalf("exit=%d stderr=%q", result.code, result.stderr)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	firstEnd := strings.Index(text, "first-end")
	secondBegin := strings.Index(text, "second-begin")
	if strings.Count(text, "=== edc log start") != 2 || strings.Count(text, "=== edc log end") != 2 || firstEnd < 0 || secondBegin < firstEnd {
		t.Fatalf("serialized log = %q", text)
	}
}
