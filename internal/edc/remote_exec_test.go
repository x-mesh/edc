package edc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteExecArgumentsAndOutputLimit(t *testing.T) {
	script := writeRemoteExecutable(t, "#!/bin/sh\nprintf '%s\n' \"$@\"\n")
	runner := sshRemoteRunner{executable: script, connectTimeout: 1500 * time.Millisecond, outputLimit: 256}
	result := runner.Run(context.Background(), "server-alias", "gk update", time.Second, nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	for _, value := range []string{"BatchMode=yes", "ConnectTimeout=2", "server-alias", "${SHELL:-/bin/sh}", " -ic ", "gk update"} {
		if !strings.Contains(result.Output, value) {
			t.Fatalf("output %q does not contain %q", result.Output, value)
		}
	}

	limited := sshRemoteRunner{executable: script, connectTimeout: time.Second, outputLimit: 4}
	result = limited.Run(context.Background(), "server-alias", "gk update", time.Second, nil)
	if result.Output != "-o\nB" || !result.Truncated {
		t.Fatalf("limited result = %#v", result)
	}
}

func TestRemoteShellCommandLoadsStartupAndHidesItsOutput(t *testing.T) {
	shell := writeRemoteExecutable(t, "#!/bin/sh\nprintf 'startup output\n'\nprintf 'startup error\n' >&2\nexec /bin/sh -c \"$2\"\n")
	process := exec.Command("/bin/sh", "-c", remoteShellCommand(`printf '%s\n' "command output with ' quote"`))
	process.Env = append(os.Environ(), "SHELL="+shell)
	output, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v: %s", err, output)
	}
	if string(output) != "command output with ' quote\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestRemoteExecTimeoutAndMissingExecutable(t *testing.T) {
	script := writeRemoteExecutable(t, "#!/bin/sh\nexec sleep 2\n")
	runner := sshRemoteRunner{executable: script, connectTimeout: time.Second, outputLimit: 64}
	result := runner.Run(context.Background(), "server-alias", "gk update", 20*time.Millisecond, nil)
	if !result.TimedOut || result.Err == nil {
		t.Fatalf("result = %#v", result)
	}

	missing := sshRemoteRunner{executable: filepath.Join(t.TempDir(), "missing-ssh"), connectTimeout: time.Second, outputLimit: 64}
	result = missing.Run(context.Background(), "server-alias", "gk update", time.Second, nil)
	if result.ExitCode != -1 || result.Err == nil || !strings.Contains(result.Err.Error(), "ssh executable") {
		t.Fatalf("result = %#v", result)
	}
}

func writeRemoteExecutable(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-ssh")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
