package edc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type remoteCommandResult struct {
	ExitCode  int
	Output    string
	Truncated bool
	TimedOut  bool
	Duration  time.Duration
	Err       error
}

type remoteCommandRunner interface {
	Run(context.Context, string, string, time.Duration, io.Writer) remoteCommandResult
	Upload(context.Context, string, string, string, string, time.Duration, io.Writer) remoteCommandResult
}

type sshRemoteRunner struct {
	executable     string
	scpExecutable  string
	connectTimeout time.Duration
	outputLimit    int
}

func (runner sshRemoteRunner) Run(parent context.Context, target, command string, timeout time.Duration, stream io.Writer) remoteCommandResult {
	started := time.Now()
	executable := runner.executable
	if executable == "" {
		executable = "ssh"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return remoteCommandResult{ExitCode: -1, Duration: time.Since(started), Err: fmt.Errorf("%s: %w", T("remote.error.ssh_not_found"), err)}
	}
	connectSeconds := int((runner.connectTimeout + time.Second - 1) / time.Second)
	args := []string{"-o", "BatchMode=yes", "-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds), target, remoteShellCommand(command)}
	return runner.runExecutable(parent, path, args, timeout, stream)
}

func (runner sshRemoteRunner) Upload(parent context.Context, target, source, destination, mode string, timeout time.Duration, stream io.Writer) remoteCommandResult {
	started := time.Now()
	executable := runner.scpExecutable
	if executable == "" {
		executable = "scp"
	}
	path, err := exec.LookPath(executable)
	if err != nil {
		return remoteCommandResult{ExitCode: -1, Duration: time.Since(started), Err: fmt.Errorf("%s: %w", T("remote.error.scp_not_found"), err)}
	}
	connectSeconds := int((runner.connectTimeout + time.Second - 1) / time.Second)
	args := []string{"-o", "BatchMode=yes", "-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds), expandLocalUploadSource(source), target + ":" + destination}
	result := runner.runExecutable(parent, path, args, timeout, stream)
	if result.Err != nil || mode == "" {
		return result
	}
	chmod := runner.Run(parent, target, "chmod "+mode+" "+quoteRemotePath(destination), timeout, stream)
	result.Output += chmod.Output
	result.Truncated = result.Truncated || chmod.Truncated
	result.TimedOut = result.TimedOut || chmod.TimedOut
	result.Duration = time.Since(started)
	result.ExitCode = chmod.ExitCode
	result.Err = chmod.Err
	return result
}

func expandLocalUploadSource(source string) string {
	if source != "~" && !strings.HasPrefix(source, "~/") {
		return source
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return source
	}
	if source == "~" {
		return home
	}
	return home + strings.TrimPrefix(source, "~")
}

func quoteRemotePath(path string) string {
	if path == "~" {
		return path
	}
	if strings.HasPrefix(path, "~/") {
		return "~/" + quoteRemoteShell(strings.TrimPrefix(path, "~/"))
	}
	if strings.HasPrefix(path, "~") {
		slash := strings.IndexByte(path, '/')
		if slash > 1 && remotePathUser(path[1:slash]) {
			return path[:slash+1] + quoteRemoteShell(path[slash+1:])
		}
	}
	return quoteRemoteShell(path)
}

func remotePathUser(value string) bool {
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return value != ""
}

func (runner sshRemoteRunner) runExecutable(parent context.Context, path string, args []string, timeout time.Duration, stream io.Writer) remoteCommandResult {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	buffer := &remoteLimitedBuffer{limit: runner.outputLimit}
	var output io.Writer = buffer
	if stream != nil {
		output = io.MultiWriter(buffer, stream)
	}
	process := exec.CommandContext(ctx, path, args...)
	process.Stdout = output
	process.Stderr = output
	err := process.Run()
	result := remoteCommandResult{ExitCode: 0, Output: buffer.String(), Truncated: buffer.Truncated(), Duration: time.Since(started), Err: err}
	if err == nil {
		return result
	}
	result.ExitCode = -1
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
	}
	result.TimedOut = ctx.Err() == context.DeadlineExceeded
	return result
}

func remoteShellCommand(command string) string {
	inner := "trap - DEBUG 2>/dev/null || true; unset PROMPT_COMMAND; exec 1>&3 2>&4; " + command
	return "exec 3>&1 4>&2; shell=${SHELL:-/bin/sh}; \"$shell\" -ic " + quoteRemoteShell(inner) + " >/dev/null 2>/dev/null"
}

func quoteRemoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type remoteLimitedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *remoteLimitedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	written := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(data) > remaining {
		_, _ = buffer.buffer.Write(data[:remaining])
		buffer.truncated = true
		return written, nil
	}
	_, _ = buffer.buffer.Write(data)
	return written, nil
}

func (buffer *remoteLimitedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func (buffer *remoteLimitedBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}
