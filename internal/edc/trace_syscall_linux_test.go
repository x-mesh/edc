//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf/ringbuf"
)

const (
	syscallHelperMarker = "--edc-trace-syscall-helper"
	syscallHelperCalls  = 1000
	syscallHelperPause  = 30 * time.Millisecond
	syscallHelperPath   = "/nonexistent/edc-trace-syscall"
)

// syscallHelperExactPath는 event에 남는 최대 길이인 255 byte 경로이고, syscallHelperLongPath는 그보다 길어 잘린다.
// 한 component가 255 byte를 넘으면 ENAMETOOLONG이 나므로 component를 나눈다. 잘린 긴 경로가 255 byte 경로와 같아지지
// 않도록 다른 글자로 채운다.
var (
	syscallHelperExactPath = syscallHelperPathOfLength(255, "x")
	syscallHelperLongPath  = syscallHelperPathOfLength(300, "y")
)

func syscallHelperPathOfLength(length int, fill string) string {
	path := "/nonexistent"
	for len(path) < length {
		path += "/" + strings.Repeat(fill, min(200, length-len(path)-1))
	}
	return path
}

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

// TestTraceSyscallHelperProcess는 root 시험이 띄우는 자식 process다. stdin에서 'r'을 읽을 때마다 getppid를 정해진
// 수만큼 부르고 없는 파일을 한 번 연다. 's'를 읽으면 30ms 자고, 'x'를 읽으면 끝난다.
func TestTraceSyscallHelperProcess(t *testing.T) {
	if os.Args[len(os.Args)-1] != syscallHelperMarker {
		return
	}
	buffer := make([]byte, 1)
	for {
		if n, err := syscall.Read(0, buffer); n != 1 || err != nil || buffer[0] == 'x' {
			os.Exit(0)
		}
		if buffer[0] == 's' {
			pause := syscall.NsecToTimespec(int64(syscallHelperPause))
			_ = syscall.Nanosleep(&pause, nil)
			continue
		}
		if buffer[0] == 'p' {
			for _, path := range []string{syscallHelperExactPath, syscallHelperLongPath} {
				if fd, err := syscall.Open(path, syscall.O_RDONLY, 0); err == nil {
					syscall.Close(fd)
				}
			}
			continue
		}
		for range syscallHelperCalls {
			syscall.Getppid()
		}
		if fd, err := syscall.Open(syscallHelperPath, syscall.O_RDONLY, 0); err == nil {
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
func traceSyscallHelpers(t *testing.T, options syscallTraceOptions, helpers []syscallHelper, finish func(), onEvent func(syscallEvent) error) syscallCollection {
	t.Helper()
	stop := make(chan struct{})
	type result struct {
		collection syscallCollection
		err        error
	}
	finished := make(chan result, 1)
	go func() {
		collection, err := collectSyscallStats(options, stop, onEvent)
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
	}, nil)
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
		collection, err := collectSyscallStats(syscallTraceOptions{duration: 1500 * time.Millisecond}, stop, nil)
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
	collection := traceSyscallHelpers(t, syscallTraceOptions{pids: []uint32{helper.pid()}}, []syscallHelper{helper}, nil, nil)
	counted := map[string]uint64{}
	for _, sample := range collection.samples {
		counted[sample.Syscall] += sample.Calls
	}
	if counted["getppid (32-bit)"] < syscallHelperCalls || counted["semget"] != 0 {
		t.Errorf("calls = %v", counted)
	}
}

func TestSyscallEventLayoutMatchesTheParser(t *testing.T) {
	var record syscallEventsSyscallEvent
	for name, offsets := range map[string][2]uintptr{
		"timestamp_ns": {unsafe.Offsetof(record.TimestampNs), 0},
		"latency_ns":   {unsafe.Offsetof(record.LatencyNs), 8},
		"ret":          {unsafe.Offsetof(record.Ret), 16},
		"tgid":         {unsafe.Offsetof(record.Tgid), 24},
		"tid":          {unsafe.Offsetof(record.Tid), 28},
		"nr":           {unsafe.Offsetof(record.Nr), 32},
		"path_state":   {unsafe.Offsetof(record.PathState), 36},
		"comm":         {unsafe.Offsetof(record.Comm), 40},
		"path":         {unsafe.Offsetof(record.Path), 56},
		"end":          {unsafe.Sizeof(record), syscallEventSize},
	} {
		if offsets[0] != offsets[1] {
			t.Errorf("%s is at %d in the BPF struct, parseSyscallEvent reads %d", name, offsets[0], offsets[1])
		}
	}
}

func TestParseSyscallEventReadsTheResultAndThePath(t *testing.T) {
	openat, _ := syscallNumber(runtime.GOARCH, "openat")
	record := func(ret int64, state byte, path string) []byte {
		data := make([]byte, syscallEventSize)
		binary.NativeEndian.PutUint64(data[0:8], 1000)
		binary.NativeEndian.PutUint64(data[8:16], 2500)
		binary.NativeEndian.PutUint64(data[16:24], uint64(ret))
		binary.NativeEndian.PutUint32(data[24:28], 7)
		binary.NativeEndian.PutUint32(data[28:32], 9)
		binary.NativeEndian.PutUint32(data[32:36], openat)
		data[36] = state
		copy(data[40:56], "api")
		copy(data[56:], path)
		return data
	}
	event, ok := parseSyscallEvent(record(-2, 1, "/etc/app.conf"), 500)
	want := syscallEvent{TimestampNS: 1500, Event: "syscall", PID: 7, TID: 9, Process: "api", Syscall: "openat", Return: -2, Errno: "ENOENT", LatencyMS: 0.0025, Path: "/etc/app.conf", Action: "read"}
	if !ok || event != want {
		t.Fatalf("event = %+v, want %+v", event, want)
	}
	// open flags와 event 이유는 37번째와 38번째 byte에 있다. 쓰기와 생성으로 열면 write이고, O_EXCL이 더해지면 create다.
	flagged := record(3, 1, "/var/log/app.log")
	flagged[37], flagged[38] = syscallOpenWrite|syscallOpenCreate, syscallReasonFile|syscallReasonLine
	if event, _ := parseSyscallEvent(flagged, 0); event.Action != "write" || event.Reason != syscallReasonFile|syscallReasonLine || event.OpenKind != syscallOpenWrite|syscallOpenCreate {
		t.Fatalf("flagged = %+v", event)
	}
	flagged[37] |= syscallOpenExcl
	if event, _ := parseSyscallEvent(flagged, 0); event.Action != "create" {
		t.Fatalf("exclusive = %+v", event)
	}
	if event, _ := parseSyscallEvent(record(3, 3, "/very/long"), 0); event.Errno != "" || event.Return != 3 || !event.PathTruncated || event.Path != "/very/long" {
		t.Fatalf("truncated = %+v", event)
	}
	if event, _ := parseSyscallEvent(record(-14, 2, ""), 0); !event.PathUnreadable || event.Path != "" {
		t.Fatalf("unreadable = %+v", event)
	}
	if _, ok := parseSyscallEvent(make([]byte, syscallEventSize-1), 0); ok {
		t.Fatal("a short record was read")
	}
}

func TestParseSyscallErrnosTakesLinuxNames(t *testing.T) {
	if all, errnos, err := parseSyscallErrnos(" ALL "); !all || errnos != nil || err != nil {
		t.Fatalf("all = %t %v %v", all, errnos, err)
	}
	if all, errnos, err := parseSyscallErrnos("enoent, EACCES,ERESTARTSYS"); all || !reflect.DeepEqual(errnos, []uint32{2, 13, 512}) || err != nil {
		t.Fatalf("names = %t %v %v", all, errnos, err)
	}
	if _, _, err := parseSyscallErrnos("ENOENT,ENOPE"); err == nil {
		t.Fatal("an unknown errno was accepted")
	}
}

// endlessSyscallEvents는 쓰는 속도보다 빨리 들어오는 event다. ring이 비지 않아 Read가 마감을 돌려주지 않는다.
type endlessSyscallEvents struct{ reads int }

func (source *endlessSyscallEvents) Read() (ringbuf.Record, error) {
	source.reads++
	if source.reads > 10000 {
		return ringbuf.Record{}, errors.New("events were still read after the trace stopped")
	}
	return ringbuf.Record{RawSample: make([]byte, syscallEventSize)}, nil
}

func TestReadSyscallEventsStopsWhileEventsKeepComing(t *testing.T) {
	source := &endlessSyscallEvents{}
	var count uint64
	if err := readSyscallEvents(source, 0, nil, &count, func() bool { return count >= 3 }); err != nil {
		t.Fatal(err)
	}
	if count != 3 || source.reads != 3 {
		t.Fatalf("count = %d, reads = %d", count, source.reads)
	}
}

// event의 경로는 255 byte까지 남는다. 정확히 255 byte인 경로는 다 남았으므로 잘린 것이 아니고, 더 긴 경로만 잘린 것이다.
// BPF helper는 두 경우 모두 같은 길이를 돌려주므로 kernel 안에서 다음 byte로 가른다(#269).
func TestTraceSyscallMarksOnlyLongerPathsAsTruncated(t *testing.T) {
	requireSyscallTrace(t)
	helper, err := startSyscallHelper(t, exec.Command(os.Args[0], "-test.run=^TestTraceSyscallHelperProcess$", "--", syscallHelperMarker))
	if err != nil {
		t.Fatal(err)
	}
	syscalls, compat, err := parseSyscallNames(runtime.GOARCH, "openat")
	if err != nil {
		t.Fatal(err)
	}
	options := syscallTraceOptions{pids: []uint32{helper.pid()}, syscalls: syscalls, compatSyscalls: compat, errorsAll: true}
	var exact, long []syscallEvent
	traceSyscallHelpers(t, options, []syscallHelper{helper}, func() {
		helper.send(t, 'p')
		time.Sleep(200 * time.Millisecond)
	}, func(event syscallEvent) error {
		switch {
		case event.Path == syscallHelperExactPath:
			exact = append(exact, event)
		case strings.HasPrefix(syscallHelperLongPath, event.Path) && len(event.Path) == len(syscallHelperExactPath):
			long = append(long, event)
		}
		return nil
	})
	if len(exact) == 0 || exact[0].PathTruncated {
		t.Errorf("255-byte path events = %+v", exact)
	}
	if len(long) == 0 || !long[0].PathTruncated {
		t.Errorf("300-byte path events = %+v", long)
	}
}

// --errors와 --slow의 event, 그리고 --syscall이 kernel 안에서 맞는지 본다. 경로는 복귀 시점의 register에서 읽으므로
// CI의 arm64 runner가 arm64 register 배치를 함께 확인한다.
func TestTraceSyscallPrintsFailedAndSlowCalls(t *testing.T) {
	requireSyscallTrace(t)
	helper, err := startSyscallHelper(t, exec.Command(os.Args[0], "-test.run=^TestTraceSyscallHelperProcess$", "--", syscallHelperMarker))
	if err != nil {
		t.Fatal(err)
	}
	syscalls, compat, err := parseSyscallNames(runtime.GOARCH, "openat,nanosleep")
	if err != nil {
		t.Fatal(err)
	}
	_, errnos, err := parseSyscallErrnos("ENOENT")
	if err != nil {
		t.Fatal(err)
	}
	options := syscallTraceOptions{pids: []uint32{helper.pid()}, syscalls: syscalls, compatSyscalls: compat, slow: syscallHelperPause * 2 / 3, errnos: errnos}
	var events []syscallEvent
	collection := traceSyscallHelpers(t, options, []syscallHelper{helper}, func() {
		helper.send(t, 's')
		time.Sleep(syscallHelperPause + 100*time.Millisecond)
	}, func(event syscallEvent) error {
		events = append(events, event)
		return nil
	})
	for _, sample := range collection.samples {
		if sample.Syscall != "openat" && sample.Syscall != "nanosleep" {
			t.Errorf("a call outside --syscall was counted: %+v", sample)
		}
	}
	failed, slept := false, false
	for _, event := range events {
		if event.PID != helper.pid() {
			t.Errorf("an event of another process: %+v", event)
		}
		if event.Errno != "" && event.Errno != "ENOENT" {
			t.Errorf("an errno outside --errors: %+v", event)
		}
		failed = failed || event.Syscall == "openat" && event.Errno == "ENOENT" && event.Path == syscallHelperPath
		slept = slept || event.Syscall == "nanosleep" && event.LatencyMS >= float64(options.slow)/float64(time.Millisecond)
	}
	if !failed || !slept {
		t.Errorf("failed openat %t, slow nanosleep %t, events %+v", failed, slept, events)
	}
	if collection.summary.Events != uint64(len(events)) || collection.summary.RingLost != 0 {
		t.Errorf("summary = %+v, events %d", collection.summary, len(events))
	}
}

// onSnapshot은 trace를 떼지 않고 liveInterval마다 지금까지의 값을 받아야 한다. 값은 줄지 않고, 마지막 수집 결과가 이어받는다.
func TestTraceSyscallSendsSnapshotsWhileRunning(t *testing.T) {
	requireSyscallTrace(t)
	var snapshots []syscallCollection
	options := syscallTraceOptions{
		duration:     1300 * time.Millisecond,
		liveInterval: 250 * time.Millisecond,
		onSnapshot:   func(collection syscallCollection) { snapshots = append(snapshots, collection) },
	}
	stop := make(chan struct{})
	collection, err := collectSyscallStats(options, stop, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) < 4 {
		t.Fatalf("snapshots = %d, want one at the start and at least 3 more in 1.3s at 250ms", len(snapshots))
	}
	if first := snapshots[0].summary.DurationMS; first > 100 {
		t.Errorf("the first snapshot came after %dms, want it right after the programs attach", first)
	}
	total := func(collection syscallCollection) (calls uint64) {
		for _, sample := range collection.samples {
			calls += sample.Calls
		}
		return calls
	}
	previous := uint64(0)
	for index, snapshot := range snapshots {
		if calls := total(snapshot); calls < previous {
			t.Errorf("snapshot %d went back from %d to %d calls", index, previous, calls)
		} else {
			previous = calls
		}
		if index > 0 && snapshot.summary.DurationMS <= 0 {
			t.Errorf("snapshot %d has no duration: %+v", index, snapshot.summary)
		}
	}
	if final := total(collection); final < previous {
		t.Errorf("final calls %d are below the last snapshot %d", final, previous)
	}
}

// 명령은 trace가 붙은 뒤에 시작하므로, 몇 ms에 끝나는 명령과 그 자식의 호출도 첫 번째부터 세어야 한다.
func traceSyscallCommand(t *testing.T, argv []string, force bool, onEvent func(syscallEvent) error) (syscallCollection, int) {
	t.Helper()
	requireSyscallTrace(t)
	command, err := prepareTraceCommand(argv)
	if err != nil {
		t.Fatal(err)
	}
	defer command.close()
	options := syscallTraceOptions{files: onEvent != nil}
	if force || command.cgroup == nil {
		// PID 필터로 대신하는 경로를 시험한다.
		if command.cgroup != nil {
			command.cgroup.remove()
			command.cgroup = nil
		}
		options.waitForPID = true
	} else {
		options.container = command.container()
	}
	options.onAttached = func(allowPID func(uint32) error) error { return command.start(allowPID) }
	collection, err := collectSyscallStats(options, command.done, onEvent)
	if err != nil {
		t.Fatal(err)
	}
	return collection, command.exitCode()
}

func syscallCallsOf(collection syscallCollection, name string) (calls uint64) {
	for _, sample := range collection.samples {
		if sample.Syscall == name {
			calls += sample.Calls
		}
	}
	return calls
}

func TestTraceSyscallCommandCountsChildrenInTheCgroup(t *testing.T) {
	// sh가 /bin/true를 3번 자식으로 띄운다. execve는 sh와 자식 3개를 합쳐 4번 이상이어야 한다.
	collection, code := traceSyscallCommand(t, []string{"sh", "-c", "/bin/true; /bin/true; /bin/true; exit 5"}, false, nil)
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	if got := syscallCallsOf(collection, "execve"); got < 4 {
		t.Errorf("execve calls = %d, want at least 4 (the shell and three children)", got)
	}
	if collection.summary.Unmatched != nil {
		t.Errorf("a cgroup-filtered trace counted unmatched calls: %+v", collection.summary)
	}
}

func TestTraceSyscallCommandFallsBackToThePIDFilter(t *testing.T) {
	collection, code := traceSyscallCommand(t, []string{"sh", "-c", "exit 7"}, true, nil)
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
	if got := syscallCallsOf(collection, "exit_group"); got == 0 {
		t.Errorf("the command was not counted: %+v", collection.samples)
	}
	if collection.summary.Unmatched != nil {
		t.Errorf("a PID-filtered trace counted unmatched calls: %+v", collection.summary)
	}
}

func TestTraceSyscallCommandRemovesItsCgroup(t *testing.T) {
	requireSyscallTrace(t)
	command, err := prepareTraceCommand([]string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if command.cgroup == nil {
		t.Skip("cgroup v2 is not available: " + command.fallback)
	}
	dir := command.cgroup.dir
	command.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cgroup %s remains: %v", dir, err)
	}
}

func TestTraceSyscallCommandListsTheFilesItTouched(t *testing.T) {
	dir := t.TempDir()
	script := fmt.Sprintf("cat /nonexistent-edc-file 2>/dev/null; echo data > %[1]s/out; mv %[1]s/out %[1]s/moved; rm %[1]s/moved; mkdir %[1]s/d", dir)
	var table syscallFileTable
	collection, code := traceSyscallCommand(t, []string{"sh", "-c", script}, false, func(event syscallEvent) error {
		if event.Reason&syscallReasonFile != 0 {
			table.add(event)
		}
		return nil
	})
	if code != 0 {
		t.Errorf("exit code = %d", code)
	}
	rows := map[string]syscallFileRow{}
	for _, row := range table.report() {
		rows[row.Path] = row
	}
	// cat이 open으로 찾든 stat으로 먼저 확인하든 실패한 시도가 한 번 보여야 한다.
	if row := rows["/nonexistent-edc-file"]; row.total() != 1 || row.errnoText() != "1 ENOENT" {
		t.Errorf("missing file row = %+v", row)
	}
	// echo >는 O_CREAT로 열지만 O_EXCL이 없어 새로 만들었는지 알 수 없으므로 write다.
	if row := rows[dir+"/out"]; row.Write != 1 || row.Rename != 1 {
		t.Errorf("out row = %+v (want one write and one rename)", row)
	}
	if row := rows[dir+"/moved"]; row.Delete != 1 {
		t.Errorf("moved row = %+v (want one delete)", row)
	}
	if row := rows[dir+"/d"]; row.Create != 1 {
		t.Errorf("d row = %+v (want one create)", row)
	}
	if collection.summary.RingLost != 0 || collection.summary.SampledOut != 0 {
		t.Errorf("file events were lost: %+v", collection.summary)
	}
}

// SIGPIPE나 SIGKILL로 죽은 edc는 cgroup을 지우지 못한다. 다음 실행이 주인이 없는 빈 cgroup만 지워야 한다.
func TestTraceSyscallCommandSweepsStaleCgroups(t *testing.T) {
	requireSyscallTrace(t)
	probe, err := createTraceCommandCgroup()
	if err != nil {
		t.Skip(err)
	}
	root := filepath.Dir(probe.dir)
	probe.remove()
	stale := filepath.Join(root, traceCommandCgroupPrefix+"4194300-stale")
	alive := filepath.Join(root, traceCommandCgroupPrefix+strconv.Itoa(os.Getpid())+"-alive")
	for _, dir := range []string{stale, alive} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	defer os.Remove(alive)
	defer os.Remove(stale)
	cgroup, err := createTraceCommandCgroup()
	if err != nil {
		t.Fatal(err)
	}
	defer cgroup.remove()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the cgroup of a dead edc remains: %v", err)
	}
	if _, err := os.Stat(alive); err != nil {
		t.Errorf("the cgroup of a live edc was removed: %v", err)
	}
}

// event 모드에서도 표를 다시 그려야 한다. 파일 event가 끊이지 않는 동안에도 마찬가지다.
func TestTraceSyscallRedrawsWhileFileEventsArrive(t *testing.T) {
	requireSyscallTrace(t)
	var snapshots int
	var events uint64
	options := syscallTraceOptions{
		duration:     1300 * time.Millisecond,
		liveInterval: 250 * time.Millisecond,
		files:        true,
		onSnapshot:   func(syscallCollection) { snapshots++ },
	}
	stop := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := collectSyscallStats(options, stop, func(event syscallEvent) error {
			events++
			return nil
		})
		finished <- err
	}()
	// 이 test 자신의 openat은 edc가 빼지만, 자식이 계속 여는 파일은 센다.
	opener := exec.Command("sh", "-c", "while :; do : < /etc/hostname; done")
	if err := opener.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opener.Process.Kill(); _ = opener.Wait() }()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if snapshots < 4 {
		t.Errorf("snapshots = %d, want one at the start and at least 3 more in 1.3s at 250ms", snapshots)
	}
	if events == 0 {
		t.Error("no file events arrived")
	}
}
