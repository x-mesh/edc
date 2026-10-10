//go:build !linux && !darwin

package edc

import (
	"fmt"
	"runtime"
)

func startTopProcessProbe() (topProbeObserver, func(), error) {
	return nil, nil, fmt.Errorf("top --detail requires Linux, not %s", runtime.GOOS)
}
