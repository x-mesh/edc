//go:build linux

package edc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// traceTLSWatchProc는 test용 /proc다. traceProcRoot와 탐색 주기를 바꾸고 test가 끝나면 되돌린다.
func traceTLSWatchProc(t *testing.T, checks []time.Duration, floor time.Duration) string {
	t.Helper()
	proc := t.TempDir()
	previousRoot, previousChecks, previousFloor, previousLibraries := traceProcRoot, traceTLSExecChecks, traceTLSRescanFloor, traceTLSHostLibraries
	traceProcRoot, traceTLSExecChecks, traceTLSRescanFloor, traceTLSHostLibraries = proc, checks, floor, nil
	t.Cleanup(func() {
		traceProcRoot, traceTLSExecChecks, traceTLSRescanFloor, traceTLSHostLibraries = previousRoot, previousChecks, previousFloor, previousLibraries
	})
	return proc
}

func writeTraceTLSProcFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTraceTLSWatch는 watchTraceTLS를 돌리고 attach에 넘어온 경로를 channel로 돌려준다. test가 끝나면 멈춘다.
func runTraceTLSWatch(t *testing.T, execs <-chan uint32) <-chan string {
	t.Helper()
	attached := make(chan string, 16)
	stop, done := make(chan struct{}), make(chan struct{})
	finder := &traceTLSFinder{seen: map[[2]uint64]bool{}, rescan: true}
	go func() {
		defer close(done)
		watchTraceTLS(finder, execs, func(target traceTLSTarget) bool { attached <- target.path; return true }, stop)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return attached
}

// exec 직후에는 동적 linker가 아직 libssl을 적재하지 않았을 수 있다. 다시 볼 때 적재한 libssl을 한 번만 붙이고, 같은
// 파일을 적재한 다른 process와 이미 끝난 process는 붙이지 않는다.
func TestWatchTraceTLSAttachesALibraryLoadedAfterExec(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	proc := traceTLSWatchProc(t, []time.Duration{0, 40 * time.Millisecond, 80 * time.Millisecond}, time.Hour)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeTraceTLSProcFile(t, filepath.Join(proc, "200", "maps"), "7f00-7f01 r-xp 00000000 08:03 1  /usr/bin/curl\n")
	copyTraceTLSFile(t, executable, filepath.Join(proc, "200", "exe"))
	execs := make(chan uint32, 4)
	attached := runTraceTLSWatch(t, execs)
	execs <- 201
	execs <- 200
	time.Sleep(10 * time.Millisecond)

	mapping := "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n"
	mapped := filepath.Join(proc, "200", "map_files", "7f00-7f01")
	copyTraceTLSFile(t, libssl, mapped)
	writeTraceTLSProcFile(t, filepath.Join(proc, "200", "maps"), mapping)
	select {
	case path := <-attached:
		if path != mapped {
			t.Fatalf("attached %s, want %s", path, mapped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the libssl that process 200 loaded after exec was not attached")
	}

	writeTraceTLSProcFile(t, filepath.Join(proc, "202", "maps"), mapping)
	if err := os.MkdirAll(filepath.Join(proc, "202", "map_files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(mapped, filepath.Join(proc, "202", "map_files", "7f00-7f01")); err != nil {
		t.Fatal(err)
	}
	execs <- 202
	select {
	case path := <-attached:
		t.Fatalf("attached the same file again: %s", path)
	case <-time.After(200 * time.Millisecond):
	}
}

// exec 알림을 받지 못해도 전체 탐색이 trace 중에 적재한 libssl을 찾는다.
func TestWatchTraceTLSRescansWithoutExecEvents(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	proc := traceTLSWatchProc(t, traceTLSExecChecks, 20*time.Millisecond)
	attached := runTraceTLSWatch(t, nil)
	writeTraceTLSProcFile(t, filepath.Join(proc, "300", "maps"), "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n")
	mapped := filepath.Join(proc, "300", "map_files", "7f00-7f01")
	copyTraceTLSFile(t, libssl, mapped)
	select {
	case path := <-attached:
		if path != mapped {
			t.Fatalf("attached %s, want %s", path, mapped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the full rescan did not attach the libssl")
	}
}

// 구독하면 exec한 process의 PID가 오고, stop을 닫으면 channel이 닫힌다. CAP_NET_ADMIN이 없는 CI runner에서는 구독은
// 되지만 exec 알림이 오지 않았다. trace http는 그 capability가 없으면 시작하지 않으므로 이때는 건너뛴다.
func TestTraceTLSExecEventsReportsAnExec(t *testing.T) {
	if capabilities, err := effectiveCapabilities(); err != nil || !capabilities[capNetAdmin] {
		t.Skip("trace http needs CAP_NET_ADMIN")
	}
	stop := make(chan struct{})
	execs, err := traceTLSExecEvents(stop)
	if err != nil {
		close(stop)
		t.Skipf("proc connector: %v", err)
	}
	if problem := traceTLSExecProblem(); problem != "" {
		close(stop)
		t.Skipf("no exec events here: %s", problem)
	}
	command := exec.Command("true")
	if err := command.Start(); err != nil {
		close(stop)
		t.Fatal(err)
	}
	pid := uint32(command.Process.Pid)
	_ = command.Wait()
	timeout := time.After(2 * time.Second)
	for found := false; !found; {
		select {
		case got := <-execs:
			found = got == pid
		case <-timeout:
			close(stop)
			t.Fatalf("no exec event for %d", pid)
		}
	}
	close(stop)
	for {
		select {
		case _, ok := <-execs:
			if !ok {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the exec channel did not close after stop")
		}
	}
}

// 고른 process가 붙이기 전에 끝나 경로가 사라지면, 같은 파일을 적재한 다른 process에서 다시 고른다.
func TestWatchTraceTLSRetriesAFileWhoseProcessEnded(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	proc := traceTLSWatchProc(t, traceTLSExecChecks, 20*time.Millisecond)
	writeTraceTLSProcFile(t, filepath.Join(proc, "400", "maps"), "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n")
	mapped := filepath.Join(proc, "400", "map_files", "7f00-7f01")
	copyTraceTLSFile(t, libssl, mapped)
	attached := make(chan bool, 4)
	stop, done := make(chan struct{}), make(chan struct{})
	finder := &traceTLSFinder{seen: map[[2]uint64]bool{}, rescan: true}
	tries := 0
	go func() {
		defer close(done)
		watchTraceTLS(finder, nil, func(traceTLSTarget) bool {
			tries++
			attached <- tries > 1
			return tries > 1
		}, stop)
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	for _, want := range []bool{false, true} {
		select {
		case got := <-attached:
			if got != want {
				t.Fatalf("attach result %t, want %t", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("the file was not tried again after a failed attach (want %t)", want)
		}
	}
	select {
	case <-attached:
		t.Fatal("tried a file again after it was attached")
	case <-time.After(100 * time.Millisecond):
	}
}

// 구독이 trace 중에 끝나면 watchTraceTLS가 알리고, stop을 닫아서 끝난 구독은 알리지 않는다.
func TestWatchTraceTLSReportsEndedExecEvents(t *testing.T) {
	traceTLSWatchProc(t, traceTLSExecChecks, time.Hour)
	for _, ended := range []bool{true, false} {
		execs, stop, done := make(chan uint32), make(chan struct{}), make(chan bool)
		go func() {
			done <- watchTraceTLS(&traceTLSFinder{seen: map[[2]uint64]bool{}, rescan: true}, execs, func(traceTLSTarget) bool { return true }, stop)
		}()
		if ended {
			close(execs)
			time.Sleep(20 * time.Millisecond)
			close(stop)
		} else {
			close(stop)
			close(execs)
		}
		if got := <-done; got != ended {
			t.Fatalf("closed execs before stop %t: reported %t", ended, got)
		}
	}
}

// 다른 PID namespace에서는 PID 1이 그 container의 init이라 network namespace를 비교할 수 없다.
func TestTraceTLSNamespaceProblem(t *testing.T) {
	for _, test := range []struct {
		pid, self, first, want string
	}{
		{traceInitPIDNamespace, "net:[4026531840]", "net:[4026531840]", ""},
		{traceInitPIDNamespace, "net:[4026532000]", "net:[4026531840]", "network namespace net:[4026532000]"},
		{"pid:[4026532100]", "net:[4026532000]", "net:[4026532000]", "PID namespace pid:[4026532100]"},
		{"", "net:[4026532000]", "", ""},
	} {
		readlink := func(path string) (string, error) {
			value := map[string]string{"/proc/self/ns/pid": test.pid, "/proc/self/ns/net": test.self, "/proc/1/ns/net": test.first}[path]
			if value == "" {
				return "", os.ErrPermission
			}
			return value, nil
		}
		if got := traceTLSNamespaceProblem(readlink); got != test.want {
			t.Fatalf("%+v: problem %q", test, got)
		}
	}
}
