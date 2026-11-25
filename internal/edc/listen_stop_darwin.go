//go:build darwin

package edc

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// darwinListenStop은 잡을 때의 시작 시각을 적어 둔다. macOS에는 pidfd가 없어서, 보내기 직전에 시작 시각을 다시 대조하고
// kill을 쓴다. 이미 끝난 zombie는 곧 PID를 내놓으므로 보내지 않는다. 살아 있는 process가 대조와 kill 사이에 끝나고,
// 거둬지고, PID가 한 바퀴 돌아 다시 쓰일 틈은 남는다.
type darwinListenStop struct {
	pid   int
	start unix.Timeval
}

func openListenStopProcess(pid int, name string, listed time.Time) (listenStopProcess, error) {
	start, _, ok := darwinProcessStart(pid)
	if !ok {
		return nil, errListenStopGone
	}
	// 목록을 읽은 뒤 PID를 다시 쓴 process는 목록보다 늦게 시작했거나 이름이 다르다.
	if darwinReadProcessOwner(int32(pid)).name != name || time.Unix(start.Sec, int64(start.Usec)*int64(time.Microsecond)).After(listed) {
		return nil, errListenStopChanged
	}
	process := &darwinListenStop{pid: pid, start: start}
	if err := process.signal(0); err != nil {
		return nil, err
	}
	return process, nil
}

func (process *darwinListenStop) signal(signal syscall.Signal) error {
	start, zombie, ok := darwinProcessStart(process.pid)
	if !ok || zombie {
		return errListenStopGone
	}
	if start != process.start {
		return errListenStopChanged
	}
	return listenStopSyscallError(syscall.Kill(process.pid, signal))
}

func (process *darwinListenStop) stop() error { return process.signal(syscall.SIGTERM) }

func (*darwinListenStop) close() {}
