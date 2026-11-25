//go:build linux

package edc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pidfd_send_signal은 Linux 5.1부터 /proc/<pid> 디렉터리 fd도 pidfd로 받는다. 이 시험이 실패하면 S는 ENOSYS가 아닌 오류로
// 멈추므로, 지원하는 커널에서 그 fd로 시그널이 실제로 나가는지 본다.
func TestPidfdSendSignalAcceptsTheProcDirectory(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	dir, err := os.OpenFile(filepath.Join("/proc", strconv.Itoa(child.Process.Pid)), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	err = unix.PidfdSendSignal(int(dir.Fd()), syscall.SIGTERM, nil, 0)
	if errors.Is(err, syscall.ENOSYS) {
		t.Skip("this kernel has no pidfd_send_signal")
	}
	if err != nil {
		t.Fatalf("pidfd_send_signal on /proc/<pid>: %v", err)
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("child did not end by SIGTERM: %v", err)
	}
}

func TestLinuxListenStopReportsAProcessThatExitsWhileOpening(t *testing.T) {
	root := t.TempDir()
	// 디렉터리는 열리지만 comm이 없는 것은 연 직후 process가 끝난 /proc/<pid>와 같다.
	if err := os.Mkdir(filepath.Join(root, "4242"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openLinuxListenStop(root, 4242, "x", time.Now()); !errors.Is(err, errListenStopGone) {
		t.Fatalf("want gone, got %v", err)
	}
}

func TestStartedBeforeListAllowsForTheBootTimeRounding(t *testing.T) {
	listed := time.Unix(1_000_000, 0)
	for _, test := range []struct {
		name    string
		started time.Time
		want    bool
	}{
		{"well before the list", listed.Add(-time.Minute), true},
		{"a full second before the list", listed.Add(-listenStopStartSlack), true},
		{"within the rounding of the list", listed.Add(-listenStopStartSlack + time.Millisecond), false},
		{"at the list", listed, false},
		{"after the list", listed.Add(time.Second), false},
	} {
		if got := startedBeforeList(test.started, listed); got != test.want {
			t.Errorf("%s: startedBeforeList = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestLinuxListenStopRefusesWhenTheKernelHasNoPidfdSendSignal(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	original := pidfdSendSignal
	defer func() { pidfdSendSignal = original }()
	pidfdSendSignal = func(int, syscall.Signal, *unix.Siginfo, int) error { return syscall.ENOSYS }
	process := &linuxListenStop{dir: dir, pid: os.Getpid()}
	for _, signal := range []syscall.Signal{0, syscall.SIGTERM} {
		if err := process.signal(signal); !errors.Is(err, errListenStopUnsupported) {
			t.Fatalf("signal %d without pidfd_send_signal = %v, want errListenStopUnsupported", signal, err)
		}
	}
}
