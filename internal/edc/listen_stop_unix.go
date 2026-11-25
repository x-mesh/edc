//go:build linux || darwin

package edc

import (
	"errors"
	"syscall"
)

func listenStopSyscallError(err error) error {
	switch {
	// 연 /proc/<pid> fd에서 파일을 열 때 process가 끝났으면 ENOENT나 ESRCH가 온다.
	case errors.Is(err, syscall.ESRCH), errors.Is(err, syscall.ENOENT):
		return errListenStopGone
	case errors.Is(err, syscall.EPERM):
		return errListenStopPermission
	}
	return err
}
