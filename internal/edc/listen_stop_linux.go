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
var pidfdSendSignal = unix.PidfdSendSignal

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
	// tick을 먼저 Duration으로 바꾸면 오래 켠 호스트에서 넘친다.
	started := boot.Add(time.Duration(float64(parsed.StartTicks) / linuxClockTicks * float64(time.Second)))
	if strings.TrimSpace(comm) != name || !startedBeforeList(started, listed) {
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

// listenStopStartSlack은 btime이 초 단위로 내림되어 계산한 시작 시각이 실제보다 최대 1초 이른 만큼이다.
const listenStopStartSlack = time.Second

// startedBeforeList는 process가 목록을 읽기 전에 시작했는지 본다. 목록을 읽은 뒤 PID를 다시 쓴 process는 이름이 같아도
// (python3, node) 목록보다 늦게 시작했으므로 이것으로 거른다. 계산한 시작 시각이 이르게 나올 수 있어서 그만큼 여유를 더해 비교한다.
// 그래서 목록을 읽기 직전 1초 안에 시작한 process는 거절하고, 그 반대로 틀리는 일은 없다.
func startedBeforeList(started, listed time.Time) bool {
	return !started.Add(listenStopStartSlack).After(listed)
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
	err := pidfdSendSignal(int(process.dir.Fd()), signal, nil, 0)
	if errors.Is(err, syscall.ENOSYS) {
		// Linux 5.1보다 오래된 커널에는 pidfd_send_signal이 없다. kill(2)로 대신하면 확인한 process가 끝나고 PID를 다시 쓴
		// 다른 process에 시그널이 갈 틈이 남으므로, 대체 경로 없이 중지를 지원하지 않는다고 알린다.
		return errListenStopUnsupported
	}
	return listenStopSyscallError(err)
}

func (process *linuxListenStop) stop() error { return process.signal(syscall.SIGTERM) }

func (process *linuxListenStop) close() { process.dir.Close() }
