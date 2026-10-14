//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func reclaimTracePrerequisites() error {
	return fmt.Errorf("trace reclaim requires Linux, not %s", runtime.GOOS)
}

func collectReclaimStats(reclaimTraceOptions, <-chan struct{}, func(reclaimEvent) error) (reclaimCollection, error) {
	return reclaimCollection{}, reclaimTracePrerequisites()
}
