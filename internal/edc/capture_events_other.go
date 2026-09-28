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
