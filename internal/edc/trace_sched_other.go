//go:build !linux

package edc

import (
	"fmt"
	"os"
	"runtime"
)

func runTraceSched([]string) int {
	fmt.Fprintf(os.Stderr, "trace sched requires Linux, not %s\n", runtime.GOOS)
	return 3
}
