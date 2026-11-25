//go:build linux || darwin

package edc

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestListenStopProcessSendsSIGTERMOnlyToTheNamedProcess(t *testing.T) {
	before := time.Now().Add(-time.Minute)
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	defer child.Process.Kill()
	listed := time.Now().Add(time.Second)
	if _, err := openListenStopProcess(pid, "not-sleep", listed); !errors.Is(err, errListenStopChanged) {
		t.Fatalf("a different name must be refused: %v", err)
	}
	// 목록보다 늦게 시작한 process는 이름이 같아도 PID를 다시 쓴 process로 본다.
	if _, err := openListenStopProcess(pid, "sleep", before); !errors.Is(err, errListenStopChanged) {
		t.Fatalf("a process that started after the list must be refused: %v", err)
	}
	process, err := openListenStopProcess(pid, "sleep", listed)
	if err != nil {
		t.Fatal(err)
	}
	defer process.close()
	if err := process.stop(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("child did not end by SIGTERM: %v", err)
	}
	if err := process.stop(); !errors.Is(err, errListenStopGone) {
		t.Fatalf("a reaped process must report gone: %v", err)
	}
	if _, err := openListenStopProcess(pid, "sleep", listed); !errors.Is(err, errListenStopGone) {
		t.Fatalf("opening a reaped process must report gone: %v", err)
	}
}
