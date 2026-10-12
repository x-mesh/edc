//go:build !darwin && !linux

package edc

import (
	"fmt"
	"runtime"
)

func collectListenSockets(listenFamilies) ([]listenSocket, error) {
	return nil, fmt.Errorf("listening sockets are not readable on %s", runtime.GOOS)
}
