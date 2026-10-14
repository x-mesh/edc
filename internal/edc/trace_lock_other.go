//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func lockTracePrerequisites() error {
	return fmt.Errorf("trace lock requires Linux, not %s", runtime.GOOS)
}

func collectLockStats(lockTraceOptions, <-chan struct{}) (lockCollection, error) {
	return lockCollection{}, lockTracePrerequisites()
}
