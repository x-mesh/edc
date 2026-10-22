//go:build !linux

package edc

import (
	"fmt"
	"os/exec"
	"runtime"
)

func syscallTracePrerequisites() error {
	return fmt.Errorf("trace syscall requires Linux, not %s", runtime.GOOS)
}

func collectSyscallStats(syscallTraceOptions, <-chan struct{}, func(syscallEvent) error) (syscallCollection, error) {
	return syscallCollection{}, syscallTracePrerequisites()
}

// parseSyscallErrnos는 Linux errno 이름을 읽는다. 다른 OS의 번호는 Linux와 달라 받기만 하고, 실행은 사전 점검에서 멈춘다.
func parseSyscallErrnos(string) (bool, []uint32, error) { return false, nil, nil }

func createTraceCommandCgroup() (*traceCommandCgroup, error) {
	return nil, syscallTracePrerequisites()
}

func setTraceCommandCgroup(*exec.Cmd, *traceCommandCgroup) {}

func probeTraceCommandCgroup(*traceCommandCgroup) error { return syscallTracePrerequisites() }
