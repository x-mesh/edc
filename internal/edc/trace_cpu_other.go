//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func cpuTracePrerequisites() error {
	return fmt.Errorf("trace cpu requires Linux, not %s", runtime.GOOS)
}

func collectCPUStacks(cpuTraceOptions, <-chan struct{}) ([]cpuStack, cpuSummary, error) {
	return nil, cpuSummary{}, cpuTracePrerequisites()
}
