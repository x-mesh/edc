//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func runCaptureEvents(captureEventsOptions) int {
	fmt.Printf("%s\n", T("cli.capture.events_linux_only", runtime.GOOS))
	return 2
}

func captureEventsPrerequisites() error {
	return fmt.Errorf("%s", T("cli.trace.linux_only", runtime.GOOS))
}
