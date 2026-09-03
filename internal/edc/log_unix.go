//go:build darwin || linux

package edc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func lockLogFile(file *os.File, signals <-chan os.Signal) (os.Signal, error) {
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case received := <-signals:
			if !timer.Stop() {
				<-timer.C
			}
			return received, nil
		case <-timer.C:
		}
	}
}

func unlockLogFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func configureLogProcess(process *exec.Cmd) {
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalLogProcess(process *exec.Cmd, received os.Signal) error {
	unixSignal, ok := received.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %v", received)
	}
	err := syscall.Kill(-process.Process.Pid, unixSignal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func unixSignalName(received syscall.Signal) string {
	switch received {
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGALRM:
		return "SIGALRM"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return fmt.Sprintf("SIG%d", received)
	}
}
