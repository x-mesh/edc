//go:build !linux

package edc

import (
	"errors"
	"runtime"
)

// trace socket은 Linux만 지원한다. runTrace가 먼저 멈추므로 이 함수들은 다른 OS에서 build만 되게 한다.
func socketTracePrerequisites() error {
	return errors.New(T("cli.trace.protocol_linux_only", "socket", runtime.GOOS))
}

func socketPayloadSupported() bool { return false }

func validateSocketTarget(string) error {
	return errors.New(T("cli.trace.protocol_linux_only", "socket", runtime.GOOS))
}
