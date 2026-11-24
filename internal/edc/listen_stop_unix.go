//go:build linux || darwin

package edc

import (
	"errors"
	"syscall"
)

func listenStopSyscallError(err error) error {
	switch {
	case errors.Is(err, syscall.ESRCH):
		return errListenStopGone
	case errors.Is(err, syscall.EPERM):
		return errListenStopPermission
	}
	return err
}
