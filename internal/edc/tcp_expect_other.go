//go:build !linux

package edc

import (
	"errors"
	"runtime"
)

// tcp expect는 AF_PACKET socket을 쓰므로 Linux만 지원한다. 다른 OS에서는 시작 전에 멈춘다.
func expectPrerequisites() error {
	return errors.New(T("expect.linux_only", runtime.GOOS))
}

func runExpectCapture(expectRun) error {
	return errors.New(T("expect.linux_only", runtime.GOOS))
}
