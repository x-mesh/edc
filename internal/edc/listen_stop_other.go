//go:build !linux && !darwin

package edc

import "time"

func openListenStopProcess(int, string, time.Time) (listenStopProcess, error) {
	return nil, errListenStopUnsupported
}
