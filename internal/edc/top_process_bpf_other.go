//go:build !linux

package edc

import (
	"fmt"
	"runtime"
)

func startTopProcessBPF() (topBPFObserver, func(), error) {
	return nil, nil, fmt.Errorf("top --ebpf requires Linux, not %s", runtime.GOOS)
}
