//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	syscallHelperMarker = "--edc-trace-syscall-helper"
	syscallHelperCalls  = 1000
)

func TestSyscallBPFStructsMatchTheParser(t *testing.T) {
	var record syscallEventsSyscallInFlight
	for name, offsets := range map[string][2]uintptr{
		"start_ns": {unsafe.Offsetof(record.StartNs), 0},
		"tgid":     {unsafe.Offsetof(record.Tgid), 8},
		"nr":       {unsafe.Offsetof(record.Nr), 12},
		"comm":     {unsafe.Offsetof(record.Comm), 16},
		"end":      {unsafe.Sizeof(record), syscallInFlightSize},
	} {
		if offsets[0] != offsets[1] {
			t.Errorf("%s is at %d in the BPF struct, parseSyscallInFlight reads %d", name, offsets[0], offsets[1])
		}
	}
	var stats syscallEventsSyscallStats
	if len(stats.Hist) != topProbeBuckets {
		t.Errorf("BPF histogram has %d buckets, the report reads %d", len(stats.Hist), topProbeBuckets)
	}
}

func TestSyscallStatsEntriesStayInTheMemoryBudget(t *testing.T) {
	for _, test := range []struct {
		cpus int
		want uint32
	}{{12, 12052}, {1, syscallStatsMaxEntries}, {512, syscallStatsMinEntries}, {0, syscallStatsMinEntries}} {
		if got := syscallStatsEntries(test.cpus, 232); got != test.want {
			t.Errorf("syscallStatsEntries(%d) = %d, want %d", test.cpus, got, test.want)
		}
	}
}

func TestSyscallExitNumbersFollowTheArchitecture(t *testing.T) {
	exit, group, compatExit, compatGroup := syscallExitNumbers("amd64")
	if exit != 60 || group != 231 || compatExit != 1|syscallCompatBit || compatGroup != 252|syscallCompatBit {
		t.Errorf("amd64 = %d %d %#x %#x", exit, group, compatExit, compatGroup)
	}
	exit, group, compatExit, compatGroup = syscallExitNumbers("arm64")
	if exit != 93 || group != 94 || compatExit != syscallNoNumber || compatGroup != syscallNoNumber {
		t.Errorf("arm64 = %d %d %#x %#x", exit, group, compatExit, compatGroup)
	}
}

func TestParseSyscallInFlightReadsEachRecord(t *testing.T) {
	record := func(start uint64, nr uint32, comm string) []byte {
		data := make([]byte, syscallInFlightSize)
		binary.NativeEndian.PutUint64(data[0:8], start)
		binary.NativeEndian.PutUint32(data[12:16], nr)
		copy(data[16:], comm)
		return data
	}
	read, _ := syscallNumber(runtime.GOARCH, "read")
	data := append(record(1000, read, "api"), record(5000, read|syscallCompatBit, "legacy")...)
	// 끝에 남은 조각은 기록이 아니다.
	data = append(data, 1, 2, 3)
	samples := parseSyscallInFlight(data, 3000)
	if len(samples) != 2 || samples[0] != (syscallInFlightSample{Process: "api", Syscall: "read", ElapsedNS: 2000}) {
		t.Fatalf("samples = %+v", samples)
	}
	if samples[1].Process != "legacy" || samples[1].ElapsedNS != 0 || samples[1].Syscall != syscallName(runtime.GOARCH, read, true) {
		t.Fatalf("compat sample = %+v", samples[1])
	}
}

func TestSyscallErrnoNamesIncludeKernelRestarts(t *testing.T) {
	for value, want := range map[uint32]string{2: "ENOENT", 516: "ERESTART_RESTARTBLOCK", 600: "errno 600"} {
		if got := syscallErrnoName(value); got != want {
			t.Errorf("syscallErrnoName(%d) = %q, want %q", value, got, want)
		}
	}
}

// TestTraceSyscallHelperProcess는 root 시험이 띄우는 자식 process다. stdin에서 한 byte를 읽을 때마다 getppid를
// 정해진 수만큼 부르고 없는 파일을 한 번 연다. 'x'를 읽으면 끝난다.
func TestTraceSyscallHelperProcess(t *testing.T) {
	if os.Args[len(os.Args)-1] != syscallHelperMarker {
		return
	}
	buffer := make([]byte, 1)
	for {
		if n, err := syscall.Read(0, buffer); n != 1 || err != nil || buffer[0] == 'x' {
			os.Exit(0)
		}
		for range syscallHelperCalls {
			syscall.Getppid()
		}
		if fd, err := syscall.Open("/nonexistent/edc-trace-syscall", syscall.O_RDONLY, 0); err == nil {
			syscall.Close(fd)
		}
	}
}

func requireSyscallTrace(t *testing.T) {
	t.Helper()
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace syscall needs BPF and perf capabilities")
	}
	if err := syscallTracePrerequisites(); err != nil {
		t.Skip(err)
	}
}

type syscallHelper struct {
	command *exec.Cmd
	input   io.WriteCloser
}

func startSyscallHelper(t *testing.T, command *exec.Cmd) (syscallHelper, error) {
	t.Helper()
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		return syscallHelper{}, err
	}
	t.Cleanup(func() {
		_ = input.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return syscallHelper{command: command, input: input}, nil
}

func (helper syscallHelper) pid() uint32 { return uint32(helper.command.Process.Pid) }

func (helper syscallHelper) send(t *testing.T, value byte) {
	t.Helper()
	if _, err := helper.input.Write([]byte{value}); err != nil {
		t.Fatal(err)
	}
}

// traceSyscallHelpers는 helper들을 대상으로 trace를 돌린다. program을 붙인 때를 알 수 없으므로 2초 동안 일을 여러 번
// 시킨다. 붙인 뒤의 한 번이면 시험에 충분하다. finish는 trace를 멈추기 직전에 부른다.
func traceSyscallHelpers(t *testing.T, options syscallTraceOptions, helpers []syscallHelper, finish func()) syscallCollection {
	t.Helper()
	stop := make(chan struct{})
	type result struct {
		collection syscallCollection
		err        error
	}
	finished := make(chan result, 1)
	go func() {
		collection, err := collectSyscallStats(options, stop)
		finished <- result{collection, err}
	}()
	for range 10 {
		for _, helper := range helpers {
			helper.send(t, 'r')
		}
		time.Sleep(200 * time.Millisecond)
	}
	if finish != nil {
		finish()
	}
	close(stop)
	done := <-finished
	if done.err != nil {
		t.Fatal(done.err)
	}
	return done.collection
}

func syscallTestComm() string {
	name := filepath.Base(os.Args[0])
	if len(name) > syscallCommMax {
		name = name[:syscallCommMax]
	}
	return name
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. runner kernel의 verifier가 syscallEvents를 받는지와 필터, 오류,
// 돌아오지 않는 호출, 진행 중인 호출을 함께 본다.
func TestTraceSyscallCountsTheSelectedProcesses(t *testing.T) {
	requireSyscallTrace(t)
	var helpers []syscallHelper
	for range 2 {
		helper, err := startSyscallHelper(t, exec.Command(os.Args[0], "-test.run=^TestTraceSyscallHelperProcess$", "--", syscallHelperMarker))
		if err != nil {
			t.Fatal(err)
		}
		helpers = append(helpers, helper)
	}
	options := syscallTraceOptions{pids: []uint32{helpers[0].pid(), helpers[1].pid()}}
	collection := traceSyscallHelpers(t, options, helpers, func() {
		// 첫 helper만 끝내 exit_group을 남기고, 둘째 helper는 read에서 기다리게 둔다.
		helpers[0].send(t, 'x')
		if err := helpers[0].command.Wait(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	})
	comm := syscallTestComm()
	bySyscall := map[string]syscallSample{}
	for _, sample := range collection.samples {
		if sample.Process != comm {
			t.Errorf("a process outside the filter was counted: %+v", sample)
		}
		bySyscall[sample.Syscall] = sample
	}
	if getppid := bySyscall["getppid"]; getppid.Calls < syscallHelperCalls || getppid.Timed != getppid.Calls || getppid.Errors != 0 {
		t.Errorf("getppid = %+v", getppid)
	}
	if openat := bySyscall["openat"]; openat.Errors == 0 || openat.Errnos["ENOENT"] == 0 {
		t.Errorf("openat = %+v", openat)
	}
	if exit := bySyscall["exit_group"]; exit.Calls == 0 || exit.Timed != 0 {
		t.Errorf("exit_group = %+v", exit)
	}
	waiting := false
	for _, sample := range collection.inFlight {
		waiting = waiting || sample.Process == comm && sample.Syscall == "read"
	}
	if !waiting {
		t.Errorf("the waiting helper is not in flight: %+v", collection.inFlight)
	}
	if collection.summary.Unmatched != nil || collection.summary.StatsFull != 0 || collection.summary.StartsFull != 0 {
		t.Errorf("summary = %+v", collection.summary)
	}
}

// edc 자신의 호출은 kernel 안에서 빠져야 한다. 필터 없이 host 전체를 1초 남짓 센다.
func TestTraceSyscallLeavesOutEdcItself(t *testing.T) {
	requireSyscallTrace(t)
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	if _, ok := syscallSelfTGID(namespace, os.Getpid()); !ok {
		t.Skip("edc counts itself outside the host PID namespace")
	}
	const calls = 200000
	stop := make(chan struct{})
	finished := make(chan syscallCollection, 1)
	go func() {
		collection, err := collectSyscallStats(syscallTraceOptions{duration: 1500 * time.Millisecond}, stop)
		if err != nil {
			t.Error(err)
		}
		finished <- collection
	}()
	time.Sleep(500 * time.Millisecond)
	for range calls {
		syscall.Getppid()
	}
	collection := <-finished
	comm := syscallTestComm()
	for _, sample := range collection.samples {
		if sample.Process == comm && sample.Syscall == "getppid" && sample.Calls >= calls {
			t.Errorf("the calls of edc itself were counted: %+v", sample)
		}
	}
	if !collection.summary.SelfExcluded || collection.summary.Unmatched == nil {
		t.Errorf("summary = %+v", collection.summary)
	}
}

// amd64 kernel은 32비트 program의 호출을 i386 번호로 받는다. 이름을 64비트 표에서 찾으면 getppid가 semget으로 보인다.
func TestTraceSyscallNamesCompatCalls(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("32-bit compat calls are checked on amd64")
	}
	requireSyscallTrace(t)
	binary := filepath.Join(t.TempDir(), "syscall-compat")
	build := exec.Command(traceTLSGoCommand(t), "build", "-o", binary, "testdata/syscall_compat.go")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=386", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the 386 program: %v\n%s", err, output)
	}
	helper, err := startSyscallHelper(t, exec.Command(binary, strconv.Itoa(syscallHelperCalls)))
	if errors.Is(err, syscall.ENOEXEC) {
		t.Skip("this kernel does not run 32-bit programs")
	}
	if err != nil {
		t.Fatal(err)
	}
	collection := traceSyscallHelpers(t, syscallTraceOptions{pids: []uint32{helper.pid()}}, []syscallHelper{helper}, nil)
	counted := map[string]uint64{}
	for _, sample := range collection.samples {
		counted[sample.Syscall] += sample.Calls
	}
	if counted["getppid (32-bit)"] < syscallHelperCalls || counted["semget"] != 0 {
		t.Errorf("calls = %v", counted)
	}
}
