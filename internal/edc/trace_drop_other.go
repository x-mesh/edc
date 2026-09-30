//go:build !linux

package edc

import (
	"errors"
	"runtime"
)

// trace drop은 Linux만 지원한다. runTrace가 먼저 멈추므로 이 함수들은 다른 OS에서 build만 되게 한다.
func dropTracePrerequisites() error {
	return errors.New(T("cli.trace.protocol_linux_only", "drop", runtime.GOOS))
}

func validateDropReasons([]string) error { return nil }
