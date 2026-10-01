//go:build linux

package edc

import (
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"
	"unsafe"

	tea "charm.land/bubbletea/v2"
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
	if !strings.Contains(text, "if(off->klass) emit(&next_key,off->cgroup_id,off->comm,1,0,off->timestamp_ns)") {
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
	for _, args := range [][]string{{"--group-by", "target"}, {"--destination", "127.0.0.1:1"}, {"--side", "server"}, {"--port", "80"}, {"--payload"}, {"--reason", "x"}} {
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
	next, _ = model.updateKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !next.(schedTraceScreenModel).detail {
		t.Fatal("Enter did not open detail")
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
