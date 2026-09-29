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

func collectTraceEvents(time.Duration) ([]captureEvent, captureSummary, error) {
	return nil, captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

func collectTraceEventsLive(traceScope, time.Duration, func(captureEvent) error, <-chan struct{}) (captureSummary, error) {
	return captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

func captureEventsPrerequisites() error {
	return fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}
