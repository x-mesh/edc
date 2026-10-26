package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// check 루틴의 사용자 명령은 설정 파일에 적힌 셸 명령이다. 결과는 종료 코드로만 판정한다.

const (
	checkCommandDefaultTimeout = 10 * time.Second
	// 명령 출력은 마지막 몇 줄만 보인다. 큰 출력을 모두 메모리에 두지 않는다.
	checkCommandOutputLimit = 4 << 10
	// sh가 끝난 뒤 sh가 띄운 process가 출력 pipe를 쥐고 있어도 이만큼만 기다린다.
	checkCommandWaitDelay = time.Second
)

type checkCommandOutcome struct {
	Name, Run, Level string
	Timeout          time.Duration
	ExitCode         int
	Output           string
	TimedOut         bool
	Err              error
	Duration         time.Duration
}

// checkTailBuffer는 출력의 끝부분만 남긴다. 실패한 명령의 원인은 대개 마지막 줄에 있다.
type checkTailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
	// cut은 앞부분을 버렸는지다. 버린 뒤 첫 줄은 중간에서 잘렸으므로 보이지 않는다.
	cut bool
}

func (buffer *checkTailBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, data...)
	if over := len(buffer.data) - buffer.limit; over > 0 {
		buffer.data = append(buffer.data[:0], buffer.data[over:]...)
		buffer.cut = true
	}
	return len(data), nil
}

func (buffer *checkTailBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	text := string(buffer.data)
	if buffer.cut {
		if _, rest, found := strings.Cut(text, "\n"); found {
			text = rest
		}
	}
	return text
}

func checkCommandTimeout(command checkCommandConfig) time.Duration {
	if command.Timeout != nil {
		return command.Timeout.Duration
	}
	return checkCommandDefaultTimeout
}

func runCheckCommand(parent context.Context, command checkCommandConfig) checkCommandOutcome {
	timeout := checkCommandTimeout(command)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := time.Now()
	output := &checkTailBuffer{limit: checkCommandOutputLimit}
	process := exec.CommandContext(ctx, "/bin/sh", "-c", command.Run)
	process.Stdout, process.Stderr = output, output
	configureLogProcess(process)
	// timeout이 나면 sh만이 아니라 sh가 띄운 process까지 끝낸다. sh만 끝내면 sleep 같은 자식이 남는다.
	process.Cancel = func() error { return signalLogProcess(process, syscall.SIGKILL) }
	process.WaitDelay = checkCommandWaitDelay
	err := process.Run()
	// sh가 끝나도 sh가 띄운 process가 남을 수 있다. 점검 명령이 남긴 process를 두지 않도록 group에 남은 것을 끝낸다.
	// group이 비어 있으면 ESRCH이고 signalLogProcess가 무시한다.
	if process.Process != nil {
		_ = signalLogProcess(process, syscall.SIGKILL)
	}
	// 명령은 성공했는데 자식이 pipe를 놓지 않은 경우다. 판정은 명령의 종료 코드를 따른다.
	if errors.Is(err, exec.ErrWaitDelay) && process.ProcessState != nil && process.ProcessState.Success() {
		err = nil
	}
	outcome := checkCommandOutcome{Name: command.Name, Run: command.Run, Level: command.Level, Timeout: timeout,
		Output: output.String(), Duration: time.Since(started), Err: err, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	var exitError *exec.ExitError
	switch {
	case errors.As(err, &exitError):
		outcome.ExitCode = exitError.ExitCode()
	case err != nil:
		outcome.ExitCode = -1
	}
	return outcome
}

// checkConfigSafe는 사용자 명령을 실행해도 되는 설정 파일인지다. sudo edc check는 root의 설정을 읽으므로, 다른 사용자가 바꿀 수
// 있는 파일의 명령을 실행하면 그 사용자가 root 권한으로 명령을 실행할 수 있다. info는 내용을 읽은 바로 그 파일의 fstat이다.
// 경로를 다시 stat하면 읽은 뒤 바꿔치기한 파일을 검사하게 된다. 상위 디렉터리도 보는 것은 디렉터리에 쓸 수 있으면 파일을 바꿀
// 수 있기 때문이다.
func checkConfigSafe(path string, info fs.FileInfo) error {
	if err := checkOwnedSafely(path, info); err != nil {
		return err
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	for {
		dirInfo, err := os.Stat(directory)
		if err != nil {
			return err
		}
		if err := checkOwnedSafely(directory, dirInfo); err != nil {
			return err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

// checkOwnedSafely는 root나 실행한 사용자만 바꿀 수 있는지다. sticky bit가 있는 디렉터리(/tmp)는 남의 파일을 지우거나 바꿀 수 없다.
func checkOwnedSafely(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 && !(info.IsDir() && info.Mode()&fs.ModeSticky != 0) {
		return fmt.Errorf(T("check.cmd.config_writable"), path, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf(T("check.cmd.config_owner"), path, stat.Uid)
	}
	return nil
}

func checkCommandResult(outcome checkCommandOutcome) Result {
	id := "cmd." + outcome.Name
	lines := strings.Split(strings.TrimRight(outcome.Output, "\n"), "\n")
	if len(lines) > checkEvidenceLines {
		lines = lines[len(lines)-checkEvidenceLines:]
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	var result Result
	switch {
	case outcome.Err == nil:
		result = checkResult(id, StatusPass, T("check.cmd.ok", outcome.Duration.Round(time.Millisecond)))
	case outcome.TimedOut:
		result = checkResult(id, StatusFail, T("check.cmd.timeout", outcome.Timeout))
	default:
		status := StatusFail
		if outcome.Level == "warn" {
			status = StatusWarn
		}
		summary := T("check.cmd.failed", outcome.ExitCode)
		if last != "" {
			summary += ": " + last
		}
		result = checkResult(id, status, summary)
	}
	if result.Status != StatusPass {
		result.Next = outcome.Run
		if strings.TrimSpace(outcome.Output) != "" {
			result.Evidence = []Evidence{{Label: T("check.evidence.output"), Value: strings.Join(lines, "\n")}}
		}
		if result.Status == StatusFail {
			result.Error = &DiagnosticError{Kind: "check", Message: result.Summary}
		}
	}
	result.DurationMS = outcome.Duration.Milliseconds()
	return result
}
