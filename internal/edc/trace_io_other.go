//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func ioTracePrerequisites() error { return fmt.Errorf("trace io requires Linux, not %s", runtime.GOOS) }
