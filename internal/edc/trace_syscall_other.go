//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func syscallTracePrerequisites() error {
	return fmt.Errorf("trace syscall requires Linux, not %s", runtime.GOOS)
}

func collectSyscallStats(syscallTraceOptions, <-chan struct{}) (syscallCollection, error) {
	return syscallCollection{}, syscallTracePrerequisites()
}
