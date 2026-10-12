//go:build !linux

package edc

import (
	"fmt"
	"os"
	"runtime"
)

func execTracePrerequisites() error {
	return fmt.Errorf("trace exec requires Linux, not %s", runtime.GOOS)
}

func runTraceExec([]string) int {
	fmt.Fprintf(os.Stderr, "trace exec requires Linux, not %s\n", runtime.GOOS)
	return 3
}
