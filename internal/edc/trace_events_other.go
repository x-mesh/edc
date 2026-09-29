//go:build !linux && !darwin

package edc

import (
	"fmt"
	"runtime"
	"time"
)

var traceKernelEvents = true

func readEphemeralPortRange() (int, int, bool) {
	return 0, 0, false
}

func readListeningTCPPorts() map[int]bool {
	return nil
}

func collectTraceEvents(time.Duration) ([]captureEvent, captureSummary, error) {
	return nil, captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

func collectTraceEventsLive(traceScope, time.Duration, func(captureEvent) error, <-chan struct{}) (captureSummary, error) {
	return captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

func captureEventsPrerequisites() error {
	return fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

// httpTracePrerequisites는 protocol 표가 모든 platform에서 참조한다. trace http는 Linux 전용이라 여기까지 오지 않는다.
func httpTracePrerequisites() error {
	return captureEventsPrerequisites()
}
