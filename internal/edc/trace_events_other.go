//go:build !linux

package edc

import (
	"fmt"
	"runtime"
	"time"
)

func collectTraceEvents(time.Duration) ([]captureEvent, captureSummary, error) {
	return nil, captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}

func collectTraceEventsLive(time.Duration, func(captureEvent) error, <-chan struct{}) ([]captureEvent, captureSummary, error) {
	return nil, captureSummary{}, fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}
