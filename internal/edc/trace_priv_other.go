//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func privTracePrerequisites() error {
	return fmt.Errorf("trace priv requires Linux, not %s", runtime.GOOS)
}

func collectPrivEvents(privTraceOptions, <-chan struct{}, func(privEvent) error) (privCollection, error) {
	return privCollection{}, privTracePrerequisites()
}
