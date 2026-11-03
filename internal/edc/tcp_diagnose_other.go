//go:build !linux

package edc

import (
	"context"
	"runtime"
)

// startDiagnoseTrace는 eBPF가 없는 OS에서 trace를 건너뛴다. 판정은 연결 phase만으로 한다.
func startDiagnoseTrace(_ context.Context, destination string) *diagnoseTraceRun {
	run := newDiagnoseTraceRun(destination)
	reason := T("diagnose.trace.linux_only", runtime.GOOS)
	run.tcpSkip, run.dropSkip = reason, reason
	return run
}
