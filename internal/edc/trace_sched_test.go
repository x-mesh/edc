//go:build linux

package edc

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/cilium/ebpf/btf"
)

func schedSample(kind, offcpu byte) []byte {
	sample := make([]byte, schedRecordSize)
	binary.LittleEndian.PutUint64(sample[0:8], 1234)
	binary.LittleEndian.PutUint64(sample[8:16], 2_500_000)
	binary.LittleEndian.PutUint64(sample[16:24], 9876)
	binary.LittleEndian.PutUint32(sample[32:36], 42)
	copy(sample[36:52], "worker")
	sample[52], sample[53] = kind, offcpu
	binary.LittleEndian.PutUint16(sample[54:56], schedSyscallNone)
	return sample
}

func TestSchedBPFSeparatesWakeupAndPreemptionLifecycles(t *testing.T) {
	source, err := os.ReadFile("trace_sched_bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "emit(next_pid,wake->comm,1,0,wake->timestamp_ns)") {
		t.Fatal("wakeup lifecycle also emits runqueue")
	}
	if !strings.Contains(text, "if(off->klass) emit(&next_key,off->cgroup_id,off->comm,1,0,off->timestamp_ns,syscall_nr)") {
		t.Fatal("preempted off-CPU lifecycle does not emit runqueue")
	}
	if strings.Contains(text, "else { struct sched_counters*c=counter(); if(c)__sync_fetch_and_add(&c->unmatched,1); } struct offcpu") {
		t.Fatal("wakeup map miss increments unmatched")
	}
}

func TestSchedRecordGeneratedLayoutMatchesParser(t *testing.T) {
	var record schedEventsSchedRecord
	if size := unsafe.Sizeof(record); size != schedRecordSize {
		t.Fatalf("generated record size = %d, parser size = %d", size, schedRecordSize)
	}
	if got := unsafe.Offsetof(record.CgroupId); got != 16 {
		t.Fatalf("cgroup_id offset = %d", got)
	}
	if got := unsafe.Offsetof(record.Pid); got != 32 {
		t.Fatalf("pid offset = %d", got)
	}
	if got := unsafe.Offsetof(record.Kind); got != 52 {
		t.Fatalf("kind offset = %d", got)
	}
	if got := unsafe.Offsetof(record.SyscallNr); got != 54 {
		t.Fatalf("syscall_nr offset = %d", got)
	}
}

func TestParseSchedRecordNamesTheSyscall(t *testing.T) {
	futex, _ := syscallNumber(runtime.GOARCH, "futex")
	sample := schedSample(2, 0)
	binary.LittleEndian.PutUint16(sample[54:56], uint16(futex))
	if event, _ := parseSchedRecord(sample, 0); event.Syscall != "futex" {
		t.Fatalf("syscall = %q", event.Syscall)
	}
	binary.LittleEndian.PutUint16(sample[54:56], uint16(futex)|schedSyscallCompat)
	if event, _ := parseSchedRecord(sample, 0); event.Syscall != syscallName(runtime.GOARCH, futex, true) {
		t.Fatalf("compat syscall = %q", event.Syscall)
	}
	if event, _ := parseSchedRecord(schedSample(2, 0), 0); event.Syscall != "" {
		t.Fatalf("no syscall = %q", event.Syscall)
	}
	for _, test := range []struct {
		event   schedEvent
		groupBy string
		want    string
	}{
		{schedEvent{Process: "api", Syscall: "futex"}, schedGroupBySyscall, "futex"},
		{schedEvent{Process: "api"}, schedGroupBySyscall, schedNoSyscall},
		{schedEvent{Process: "api", Syscall: "futex"}, "", "api"},
		{schedEvent{Event: "sched_offcpu"}, traceGroupByEvent, "sched_offcpu"},
		{schedEvent{CgroupID: 7}, "cgroup", "7"},
	} {
		if got := schedGroupKey(test.event, test.groupBy); got != test.want {
			t.Errorf("schedGroupKey(%+v, %q) = %q, want %q", test.event, test.groupBy, got, test.want)
		}
	}
}

func TestSchedMissingBTFMembers(t *testing.T) {
	members := []btf.Member{{Name: "start_boottime"}, {Name: "cgroups"}}
	if missing := schedMissingBTFMembers(members, []string{"start_boottime", "cgroups"}); missing != "" {
		t.Fatalf("missing = %q", missing)
	}
	if missing := schedMissingBTFMembers(members, []string{"start_boottime", "comm", "cgroups"}); missing != "comm" {
		t.Fatalf("missing = %q", missing)
	}
}

func TestParseSchedRecordKeepsZeroCgroupUnavailable(t *testing.T) {
	event, ok := parseSchedRecord(schedSample(2, 1), 1_000)
	if !ok || event.Event != "sched_offcpu" || event.PID != 42 || event.Process != "worker" || event.LatencyMS != 2.5 || event.CgroupID != 9876 || event.CgroupUnavailable || event.OffCPUClass != "preempted" {
		t.Fatalf("event = %#v, ok = %t", event, ok)
	}
	if event.TimestampNS != 2_234 || event.BootTimeNS != 1234 {
		t.Fatalf("timestamp = %d, boot time = %d", event.TimestampNS, event.BootTimeNS)
	}
	if event, ok := parseSchedRecord(schedSample(0, 1), 0); !ok || event.OffCPUClass != "" {
		t.Fatalf("event = %#v, ok = %t", event, ok)
	}
	if _, ok := parseSchedRecord(make([]byte, schedRecordSize-1), 0); ok {
		t.Fatal("short sample parsed")
	}
}

func TestSchedStatsReportsP95AndMaximum(t *testing.T) {
	stats := schedStats{values: []float64{3, 1, 2, 100}}
	n, average, p95, maximum := stats.report()
	if n != 4 || *average != 26.5 || *p95 != 100 || *maximum != 100 {
		t.Fatalf("n=%d avg=%v p95=%v max=%v", n, average, p95, maximum)
	}
}

func TestSchedContainerFilterUsesCgroupID(t *testing.T) {
	options := schedTraceOptions{container: &traceContainer{cgroups: map[uint64]bool{42: true}}}
	if !schedEventMatches(options, schedEvent{CgroupID: 42}) || schedEventMatches(options, schedEvent{CgroupID: 43}) {
		t.Fatal("container cgroup filter did not select only configured cgroups")
	}
}

func TestRunTraceSchedRejectsUnsupportedFiltersBeforePrivilegeCheck(t *testing.T) {
	for _, args := range [][]string{{"--group-by", "target"}, {"--destination", "127.0.0.1:1"}, {"--side", "server"}, {"--port", "80"}, {"--payload"}, {"--reason", "x"}, {"--slow", "0"}, {"--slow", "-1ms"}, {"--slow", "1"}} {
		stderr := captureTraceStderr(t, func() {
			if code := runTraceSched(args); code != 2 {
				t.Fatalf("args=%q code=%d", args, code)
			}
		})
		if stderr == "" && len(args) < 2 {
			t.Fatalf("args=%q stderr is empty", args)
		}
	}
}

func TestSchedReportShowsLossCounters(t *testing.T) {
	report := schedReport{DurationMS: 10, Summary: schedSummary{MinimumLatencyMS: 1, EventCount: 2, LostEvents: 3, MapFull: 4, Unmatched: 5, RepeatedWakeups: 6, OmittedBelowThreshold: 7}}
	output := captureSchedStdout(t, report.print)
	for _, value := range []string{"ring_lost 3", "map_full 4", "unmatched 5", "repeated_wakeup 6", "below_threshold 7"} {
		if !strings.Contains(output, value) {
			t.Fatalf("output %q misses %q", output, value)
		}
	}
}

func TestSchedTraceScreenViewsAndKeys(t *testing.T) {
	model := newSchedTraceScreenModel(nil)
	model.events = []schedEvent{{Event: "sched_offcpu", PID: 42, Process: "worker", LatencyMS: 2.5, OffCPUClass: "voluntary"}}
	for _, width := range []int{40, 80, 120} {
		model.width, model.height = width, 12
		if view := model.View().Content; !strings.Contains(view, "edc trace sched") || !strings.Contains(view, "worker") {
			t.Fatalf("width %d view = %q", width, view)
		}
	}
	next, _ := model.updateKey(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if got := next.(schedTraceScreenModel).groupBy; got != traceGroupByProcess {
		t.Fatalf("group = %q", got)
	}
	next, _ = next.(schedTraceScreenModel).updateKey(tea.KeyPressMsg{Code: 'e', Text: "e"})
	if got := next.(schedTraceScreenModel).groupBy; got != traceGroupByEvent {
		t.Fatalf("group = %q", got)
	}
	next, _ = next.(schedTraceScreenModel).updateKey(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if got := next.(schedTraceScreenModel).groupBy; got != "cgroup" {
		t.Fatalf("group = %q", got)
	}
	next, _ = next.(schedTraceScreenModel).updateKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if got := next.(schedTraceScreenModel).groupBy; got != schedGroupBySyscall {
		t.Fatalf("tab after cgroup = %q", got)
	}
	next, _ = model.updateKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	grouped := next.(schedTraceScreenModel)
	grouped.events = append(grouped.events, schedEvent{Event: "sched_offcpu", Process: "worker", Syscall: "futex"})
	if got, view := grouped.groupBy, grouped.View().Content; got != schedGroupBySyscall || !strings.Contains(view, "futex") || !strings.Contains(view, schedNoSyscall+" ") {
		t.Fatalf("group = %q, view = %q", got, view)
	}
	next, _ = model.updateKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !next.(schedTraceScreenModel).detail {
		t.Fatal("Enter did not open detail")
	}
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. 기다린 구간에 syscall 이름이 붙는지 본다. helper는 stdin의 read에서
// 기다리고, sleep은 nanosleep 계열에서 잔다. 이름은 사용자 register에서 읽으므로 arm64 runner가 arm64 배치를 확인한다.
func TestTraceSchedNamesTheSyscallOfAWait(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace sched needs BPF and perf capabilities")
	}
	if err := schedTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	helper, err := startSyscallHelper(t, exec.Command(os.Args[0], "-test.run=^TestTraceSyscallHelperProcess$", "--", syscallHelperMarker))
	if err != nil {
		t.Fatal(err)
	}
	comm := syscallTestComm()
	stop := make(chan struct{})
	reads, sleeps := 0, 0
	finished := make(chan error, 1)
	go func() {
		_, err := collectSchedEvents(0, time.Millisecond, func(event schedEvent) error {
			if event.Event != "sched_offcpu" {
				return nil
			}
			if event.Process == comm && event.Syscall == "read" {
				reads++
			}
			if event.Process == "sleep" && strings.HasSuffix(event.Syscall, "nanosleep") {
				sleeps++
			}
			return nil
		}, stop)
		finished <- err
	}()
	// program을 붙인 때를 알 수 없으므로 2초 남짓 여러 번 기다리게 한다.
	for range 10 {
		helper.send(t, 'r')
		if err := exec.Command("sleep", "0.05").Run(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(stop)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if reads == 0 || sleeps == 0 {
		t.Errorf("read waits %d, sleep waits %d", reads, sleeps)
	}
}

func captureSchedStdout(t *testing.T, run func()) string {
	t.Helper()
	previous := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = previous
	value, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(value)
}

func TestSchedReportColorsOnlyTheHeaderAndLosses(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	avg, p95 := 1.25, 4.0
	report := schedReport{DurationMS: 2000, Rows: []schedReportOutputRow{{Group: "api", Wakeup: schedReportValue{Samples: 3, AverageMS: &avg, P95MS: &p95, MaxMS: &p95}}},
		Summary: schedSummary{EventCount: 3, LostEvents: 2, MinimumLatencyMS: 0.5}}
	setTraceSummaryTerminal(t, false, 0)
	plain := traceCaptureOutput(t, &os.Stdout, report.print)
	want := "SCHEDULER TRACE  duration 2000ms  minimum 0.5ms\n" +
		"GROUP                  WAKE N      AVG      P95      MAX     RUN N      AVG      P95      MAX     OFF N      AVG      P95      MAX\n" +
		"api                         3      1.2      4.0      4.0         0        -        -        -         0        -        -        -\n" +
		"events 3  ring_lost 2  map_full 0  unmatched 0  repeated_wakeup 0  below_threshold 0\n"
	if plain != want {
		t.Fatalf("plain =\n%q\nwant\n%q", plain, want)
	}
	setTraceSummaryTerminal(t, true, 400)
	colored := traceCaptureOutput(t, &os.Stdout, report.print)
	if ansi.Strip(colored) != plain {
		t.Fatalf("colored text =\n%q", ansi.Strip(colored))
	}
	for _, part := range []string{"\x1b[1;35mSCHEDULER TRACE\x1b[0m", "\x1b[90mGROUP ", "\x1b[90mring_lost\x1b[0m \x1b[33m2\x1b[0m", "\x1b[90mmap_full\x1b[0m 0"} {
		if !strings.Contains(colored, richExpect(part)) {
			t.Errorf("colored misses %q:\n%q", richExpect(part), colored)
		}
	}
}
