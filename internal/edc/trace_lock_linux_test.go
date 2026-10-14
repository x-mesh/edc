//go:build linux

package edc

import (
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// lockTestWord는 helper가 기다리는 futex다. 깨우는 thread가 없으므로 기다림마다 시간 초과로 돌아온다.
var lockTestWord uint32

const lockTestFutexWaits = 4 * 50

// TestTraceLockHelper는 아래 시험이 띄우는 process다. futex는 lockTestWord에서 정해진 횟수만큼 기다리고, mmap은 thread
// 여러 개가 같은 mm의 mmap_lock을 두고 다툰다.
func TestTraceLockHelper(t *testing.T) {
	var group sync.WaitGroup
	switch os.Getenv("EDC_LOCK_HELPER") {
	case "futex":
		for range 4 {
			group.Add(1)
			go func() {
				defer group.Done()
				runtime.LockOSThread()
				timeout := syscall.Timespec{Nsec: int64(time.Millisecond)}
				for range lockTestFutexWaits / 4 {
					// FUTEX_WAIT_PRIVATE(128)다.
					syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(&lockTestWord)), 128, 0, uintptr(unsafe.Pointer(&timeout)), 0, 0)
				}
			}()
		}
	case "mmap":
		end := time.Now().Add(1500 * time.Millisecond)
		for range 8 {
			group.Add(1)
			go func() {
				defer group.Done()
				runtime.LockOSThread()
				for time.Now().Before(end) {
					region, err := syscall.Mmap(-1, 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
					if err != nil {
						panic(err)
					}
					for index := 0; index < len(region); index += 4096 {
						region[index] = 1
					}
					_ = syscall.Munmap(region)
				}
			}()
		}
	case "mutex":
		lockTestSleepingMutex(t)
	default:
		t.Skip("helper process for the trace lock tests")
	}
	group.Wait()
}

// lockTestMutexRounds는 helper가 pipe mutex에서 잠드는 횟수다. BPF가 thread마다 쌓는 깊이 4보다 커야 한다.
const lockTestMutexRounds = 8

// userfaultfd의 ioctl 번호다. _IOWR(0xAA, nr, size)라 amd64와 arm64에서 같다.
const (
	lockTestUffdioAPI      = 0xC018AA3F
	lockTestUffdioRegister = 0xC020AA00
	lockTestUffdioZeropage = 0xC020AA04
)

// lockTestSleepingMutex는 한 thread가 pipe mutex를 쥔 채 userfaultfd page의 fault에서 멈추게 한다. 그동안 다른 thread가
// 같은 pipe에 쓰면 mutex의 주인이 잠들어 있으므로 spin을 그만두고 잠든다. 잠드는 thread는 매번 같은 thread다.
func lockTestSleepingMutex(t *testing.T) {
	uffd, _, errno := syscall.Syscall(unix.SYS_USERFAULTFD, syscall.O_CLOEXEC, 0, 0)
	if errno != 0 {
		t.Skipf("userfaultfd: %v", errno)
	}
	defer syscall.Close(int(uffd))
	ioctl := func(request uintptr, argument unsafe.Pointer) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uffd, request, uintptr(argument)); errno != 0 {
			t.Fatalf("ioctl 0x%x: %v", request, errno)
		}
	}
	api := [3]uint64{0xAA, 0, 0}
	ioctl(lockTestUffdioAPI, unsafe.Pointer(&api))
	var pipe [2]int
	if err := syscall.Pipe2(pipe[:], syscall.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	go func() {
		buffer := make([]byte, 1<<16)
		for {
			if count, err := syscall.Read(pipe[0], buffer); count <= 0 || err != nil {
				return
			}
		}
	}()
	trigger, written := make(chan struct{}), make(chan struct{})
	go func() {
		runtime.LockOSThread()
		block := make([]byte, 16)
		for range trigger {
			_, _ = syscall.Write(pipe[1], block)
			written <- struct{}{}
		}
	}()
	for range lockTestMutexRounds {
		region, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			t.Fatal(err)
		}
		start := uint64(uintptr(unsafe.Pointer(&region[0])))
		// UFFDIO_REGISTER_MODE_MISSING(1)이다. page를 채우기 전까지 그 page를 읽는 kernel 복사가 멈춘다.
		register := [4]uint64{start, 4096, 1, 0}
		ioctl(lockTestUffdioRegister, unsafe.Pointer(&register))
		faulted := make(chan struct{})
		go func() {
			syscall.Syscall(syscall.SYS_WRITE, uintptr(pipe[1]), uintptr(start), 4096)
			close(faulted)
		}()
		time.Sleep(50 * time.Millisecond)
		trigger <- struct{}{}
		time.Sleep(50 * time.Millisecond)
		zero := [4]uint64{start, 4096, 0, 0}
		ioctl(lockTestUffdioZeropage, unsafe.Pointer(&zero))
		<-faulted
		<-written
		_ = syscall.Munmap(region)
	}
	close(trigger)
}

// mutex의 느린 길은 contention_begin을 spin 전과 잠들기 전에 두 번 부르고 contention_end는 한 번 부른다. 같은 thread가
// 잠드는 mutex 기다림을 여러 번 겪어도 기다림의 깊이가 차지 않고, 기다림마다 한 번씩 세는지 본다.
func TestTraceLockCountsEachSleepingMutexWaitOnce(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace lock needs BPF capabilities")
	}
	if err := lockTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	type result struct {
		collection lockCollection
		err        error
	}
	finished := make(chan result, 1)
	go func() {
		collection, err := collectLockStats(lockTraceOptions{comms: []string{strings.TrimSpace(string(comm))}, kind: lockKindKernel}, stop)
		finished <- result{collection, err}
	}()
	time.Sleep(time.Second)
	command := exec.Command(os.Args[0], "-test.run=^TestTraceLockHelper$", "-test.count=1", "-test.v")
	command.Env = append(os.Environ(), "EDC_LOCK_HELPER=mutex")
	output, helperErr := command.CombinedOutput()
	close(stop)
	done := <-finished
	if helperErr != nil {
		t.Fatalf("helper: %v\n%s", helperErr, output)
	}
	if done.err != nil {
		if strings.Contains(done.err.Error(), "no lock hook to attach") {
			t.Skip(done.err)
		}
		t.Fatal(done.err)
	}
	if strings.Contains(string(output), "--- SKIP") {
		t.Skipf("helper skipped:\n%s", output)
	}
	report := lockReportFor(done.collection)
	if report.Summary.DepthFull != 0 || report.Summary.Unmatched != 0 {
		t.Errorf("depth_full %d unmatched %d, want 0", report.Summary.DepthFull, report.Summary.Unmatched)
	}
	var waits uint64
	for _, row := range report.Kernel {
		if row.Type == "mutex" && strings.Contains(row.Caller, "pipe_write") {
			waits += row.Waits
		}
	}
	if waits < lockTestMutexRounds {
		t.Errorf("mutex waits in pipe_write = %d, want at least %d: %+v", waits, lockTestMutexRounds, report.Kernel)
	}
	t.Logf("mutex waits in pipe_write %d", waits)
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. futex 기다림의 수가 helper가 기다린 수와 같은지, 그 주소에 변수 이름이
// 붙는지, kernel lock 경합에 lock 종류와 부른 함수가 붙는지 본다. 5.19 전 kernel에는 contention tracepoint가 없다.
func TestTraceLockCountsFutexAndKernelWaits(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace lock needs BPF capabilities")
	}
	if err := lockTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	stop := make(chan struct{})
	type result struct {
		collection lockCollection
		err        error
	}
	finished := make(chan result, 1)
	var pids []uint32
	commands := []*exec.Cmd{}
	for _, mode := range []string{"futex", "mmap"} {
		command := exec.Command(os.Args[0], "-test.run=^TestTraceLockHelper$", "-test.count=1")
		command.Env = append(os.Environ(), "EDC_LOCK_HELPER="+mode)
		commands = append(commands, command)
	}
	// helper를 먼저 멈춘 채로 띄울 수 없으므로, 이름으로 거른다. 시험 binary와 helper는 같은 이름이라 시험 process 자신의
	// 기다림도 섞이지만, lockTestWord는 helper만 기다린다.
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		collection, err := collectLockStats(lockTraceOptions{comms: []string{strings.TrimSpace(string(comm))}}, stop)
		finished <- result{collection, err}
	}()
	time.Sleep(time.Second)
	for _, command := range commands {
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("helper: %v\n%s", err, output)
		}
		pids = append(pids, uint32(command.ProcessState.Pid()))
	}
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	report := lockReportFor(done.collection)
	// go test가 직접 build한 binary는 -s로 심볼표가 없어 변수 이름 대신 file 이름과 주소가 붙는다. 비 PIE라 helper의 주소도
	// 이 process의 주소와 같다.
	want := fmt.Sprintf("0x%x", uintptr(unsafe.Pointer(&lockTestWord)))
	if executable, err := os.Executable(); err == nil {
		if file, err := elf.Open(executable); err == nil {
			if _, err := file.Symbols(); err == nil {
				want = ".lockTestWord"
			}
			file.Close()
		}
	}
	var futexWaits uint64
	for _, row := range report.Futex {
		if row.PID == pids[0] && strings.HasSuffix(row.Lock, want) {
			futexWaits += row.Waits
			if !strings.Contains(row.Caller, "Syscall6") {
				t.Errorf("futex caller = %q", row.Caller)
			}
		}
	}
	if futexWaits != lockTestFutexWaits {
		t.Errorf("waits on %s = %d, want %d: %+v", want, futexWaits, lockTestFutexWaits, report.Futex)
	}
	if reason, missing := report.Summary.Unavailable[lockKindKernel]; missing {
		t.Logf("kernel lock waits not checked: %s", reason)
		return
	}
	var rwsem uint64
	for _, row := range report.Kernel {
		if strings.HasPrefix(row.Type, "rwsem:") && row.Caller != "-" && !strings.HasPrefix(row.Caller, "bpf_") {
			rwsem += row.Waits
		}
	}
	if rwsem == 0 {
		t.Errorf("no rwsem wait with a caller: %+v", report.Kernel)
	}
	t.Logf("futex waits %d, rwsem waits %d, kernel rows %d", futexWaits, rwsem, len(report.Kernel))
}
