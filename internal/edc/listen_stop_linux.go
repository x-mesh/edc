//go:build linux

package edc

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// linuxListenStop은 열어 둔 /proc/<pid> 디렉터리다. 이 fd는 연 때의 process에 묶여서, 그 process가 끝나면 PID를 다시 쓴
// process의 파일을 이 fd로 열 수 없다. Linux 5.1부터 pidfd_send_signal은 이 fd를 pidfd로 받으므로, 보내는 시그널도 연 때의
// process에만 닿는다.
type linuxListenStop struct {
	dir *os.File
	pid int
}

func openListenStopProcess(pid int, name string, listed time.Time) (listenStopProcess, error) {
	return openLinuxListenStop("/proc", pid, name, listed)
}

func openLinuxListenStop(procRoot string, pid int, name string, listed time.Time) (listenStopProcess, error) {
	dir, err := os.OpenFile(filepath.Join(procRoot, strconv.Itoa(pid)), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errListenStopGone
		}
		return nil, err
	}
	process := &linuxListenStop{dir: dir, pid: pid}
	comm, err := process.read("comm")
	if err != nil {
		dir.Close()
		return nil, err
	}
	stat, err := process.read("stat")
	if err != nil {
		dir.Close()
		return nil, err
	}
	parsed, ok := parseLinuxProcessStat(pid, stat)
	boot := readAuditBootTime(filepath.Join(procRoot, "stat"))
	if !ok || boot.IsZero() {
		dir.Close()
		return nil, errors.New("cannot read the process start time")
	}
	// 목록을 읽은 뒤 PID를 다시 쓴 process는 목록보다 늦게 시작했다. 이름이 같은 process(python3, node)도 이것으로 거른다.
	// btime은 초 단위라 시작 시각이 1초 안쪽으로 이르게 계산될 수 있다. tick을 먼저 Duration으로 바꾸면 오래 켠 호스트에서 넘친다.
	started := boot.Add(time.Duration(float64(parsed.StartTicks) / linuxClockTicks * float64(time.Second)))
	if strings.TrimSpace(comm) != name || started.After(listed) {
		dir.Close()
		return nil, errListenStopChanged
	}
	// 확인 상자를 띄우기 전에 권한을 본다. signal 0은 아무것도 보내지 않고 권한과 존재만 검사한다.
	if err := process.signal(0); err != nil {
		dir.Close()
		return nil, err
	}
	return process, nil
}

func (process *linuxListenStop) read(name string) (string, error) {
	fd, err := syscall.Openat(int(process.dir.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", listenStopSyscallError(err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return "", listenStopSyscallError(err)
	}
	return string(data), nil
}

func (process *linuxListenStop) signal(signal syscall.Signal) error {
	err := unix.PidfdSendSignal(int(process.dir.Fd()), signal, nil, 0)
	if !errors.Is(err, syscall.ENOSYS) {
		return listenStopSyscallError(err)
	}
	// Linux 5.1보다 오래된 커널에는 pidfd_send_signal이 없다. fd로 stat을 열 수 있으면 연 때의 process가 아직 PID를 쥐고
	// 있다. 이미 끝나 거두기만 기다리는 zombie는 곧 PID를 내놓으므로 끝난 것으로 본다. 살아 있는 process가 확인과 kill 사이에
	// 끝나고, 거둬지고, PID가 한 바퀴 돌아 다시 쓰일 틈은 남는다.
	stat, err := process.read("stat")
	if err != nil {
		return err
	}
	if parsed, ok := parseLinuxProcessStat(process.pid, stat); !ok || parsed.State == "Z" {
		return errListenStopGone
	}
	return listenStopSyscallError(syscall.Kill(process.pid, signal))
}

func (process *linuxListenStop) stop() error { return process.signal(syscall.SIGTERM) }

func (process *linuxListenStop) close() { process.dir.Close() }
