package edc

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// execTestRecord는 trace_exec_bpf.c의 exec_record와 같은 자리에 값을 둔다. 순서가 어긋나면 kernel에서 온 기록을 잘못 읽는다.
func execTestRecord(kind, flags byte, pid, ppid, sender, uid uint32, code, signal int32, duration uint64, comm, other string, args []byte) []byte {
	record := make([]byte, execRecordSize)
	binary.LittleEndian.PutUint64(record[0:8], 1_000)
	binary.LittleEndian.PutUint64(record[8:16], 77)
	binary.LittleEndian.PutUint64(record[16:24], duration)
	binary.LittleEndian.PutUint32(record[24:28], pid)
	binary.LittleEndian.PutUint32(record[28:32], ppid)
	binary.LittleEndian.PutUint32(record[32:36], sender)
	binary.LittleEndian.PutUint32(record[36:40], uid)
	binary.LittleEndian.PutUint32(record[40:44], uint32(code))
	binary.LittleEndian.PutUint32(record[44:48], uint32(signal))
	copy(record[48:64], comm)
	copy(record[64:80], other)
	record[80], record[81] = kind, flags
	binary.LittleEndian.PutUint16(record[82:84], uint16(len(args)))
	copy(record[84:], args)
	return record
}

func TestParseExecRecordReadsEachKind(t *testing.T) {
	exec, ok := parseExecRecord(execTestRecord(0, 1, 4242, 1, 0, 1000, 0, 0, 0, "java", "systemd", []byte("java\x00-jar\x00app.jar\x00")), 5)
	if !ok || exec.Event != execEventExec || exec.PID != 4242 || exec.PPID != 1 || exec.Parent != "systemd" || exec.Process != "java" || exec.UID != 1000 || exec.CgroupID != 77 || exec.TimestampNS != 1_005 || exec.BootTimeNS != 1_000 {
		t.Fatalf("exec = %+v %v", exec, ok)
	}
	if exec.Args != "java -jar app.jar" || !exec.ArgsTruncated || exec.ExitCode != nil || exec.LifetimeMS != nil || exec.SenderPID != nil {
		t.Fatalf("exec fields = %+v", exec)
	}
	exit, ok := parseExecRecord(execTestRecord(1, 2, 4242, 1, 0, 1000, 1, 11, uint64(1500*time.Millisecond), "java", "systemd", nil), 0)
	if !ok || exit.Event != execEventExit || exit.ExitCode == nil || *exit.ExitCode != 1 || exit.Signal != "SIGSEGV" || !exit.CoreDumped || exit.LifetimeMS == nil || *exit.LifetimeMS != 1500 {
		t.Fatalf("exit = %+v %v", exit, ok)
	}
	clean, _ := parseExecRecord(execTestRecord(1, 0, 7, 1, 0, 0, 0, 0, 10, "cron", "systemd", nil), 0)
	if clean.Signal != "" || clean.CoreDumped || *clean.ExitCode != 0 {
		t.Fatalf("clean exit = %+v", clean)
	}
	sig, ok := parseExecRecord(execTestRecord(2, 4, 4242, 0, 1, 0, 15, 1, 0, "java", "systemd", nil), 0)
	if !ok || sig.Event != execEventSignal || sig.Signal != "SIGTERM" || sig.SenderPID == nil || *sig.SenderPID != 1 || sig.Sender != "systemd" || !sig.FromKernel || sig.Result != "ignored" {
		t.Fatalf("signal = %+v %v", sig, ok)
	}
	unknown, _ := parseExecRecord(execTestRecord(2, 0, 1, 0, 2, 0, 34, 9, 0, "a", "b", nil), 0)
	if unknown.Signal != "SIG34" || unknown.Result != "result_9" {
		t.Fatalf("unknown signal = %+v", unknown)
	}
	killed, _ := parseExecRecord(execTestRecord(1, 8, 4242, 1, 0, 0, 0, 9, 10, "java", "systemd", nil), 0)
	if !killed.OOMKilled || killed.Signal != "SIGKILL" {
		t.Fatalf("oom exit = %+v", killed)
	}
	// 5.15의 mark_victim은 victim의 pid만 준다. 이름과 메모리 칸은 비고, OOM을 일으킨 task만 sender로 온다.
	bare, ok := parseExecRecord(execTestRecord(3, 0, 4243, 0, 900, 0, 0, 0, 0, "", "stress", nil), 0)
	if !ok || bare.Event != execEventOOM || bare.PID != 4243 || bare.Process != "" || bare.SenderPID == nil || *bare.SenderPID != 900 || bare.Sender != "stress" || !bare.FromKernel || bare.AnonRSSKB != nil || bare.OOMScoreAdj != nil {
		t.Fatalf("oom without detail = %+v %v", bare, ok)
	}
	detailed := execTestRecord(3, 16, 4243, 0, 900, 1000, -500, 0, 0, "java", "stress", nil)
	for index, value := range []uint64{4_000_000, 2_000_000, 3_000, 512} {
		binary.LittleEndian.PutUint64(detailed[344+8*index:], value)
	}
	oom, ok := parseExecRecord(detailed, 0)
	if !ok || oom.Process != "java" || oom.UID != 1000 || *oom.TotalVMKB != 4_000_000 || *oom.AnonRSSKB != 2_000_000 || *oom.FileRSSKB != 3_000 || *oom.ShmemRSSKB != 512 || *oom.OOMScoreAdj != -500 {
		t.Fatalf("oom with detail = %+v %v", oom, ok)
	}
	for _, bad := range [][]byte{nil, make([]byte, execRecordSize-1), execTestRecord(4, 0, 1, 1, 0, 0, 0, 0, 0, "x", "y", nil)} {
		if event, ok := parseExecRecord(bad, 0); ok {
			t.Fatalf("read a bad record: %+v", event)
		}
	}
	long := execTestRecord(0, 0, 1, 1, 0, 0, 0, 0, 0, "x", "y", nil)
	binary.LittleEndian.PutUint16(long[82:84], execArgsMax+1)
	if _, ok := parseExecRecord(long, 0); ok {
		t.Fatal("accepted args_len beyond the buffer")
	}
}

func TestExecArgsTextJoinsArgvAndEscapesControlBytes(t *testing.T) {
	if got := execArgsText([]byte("sh\x00-c\x00echo\x1b[0m\x00\x00")); got != `sh -c echo\x1b[0m` {
		t.Fatalf("args = %q", got)
	}
	if got := execArgsText([]byte("\x00\x00")); got != "" {
		t.Fatalf("empty args = %q", got)
	}
}

func TestParseExecSignalsAcceptsNamesNumbersAllAndNone(t *testing.T) {
	mask, err := parseExecSignals("")
	if err != nil || mask&(1<<9) == 0 || mask&(1<<15) == 0 || mask&(1<<17) != 0 || mask&(1<<14) != 0 {
		t.Fatalf("default mask = %b, %v", mask, err)
	}
	mask, err = parseExecSignals("term, SIGKILL,12")
	if err != nil || mask != 1<<15|1<<9|1<<12 {
		t.Fatalf("mask = %b, %v", mask, err)
	}
	if mask, err := parseExecSignals("all"); err != nil || mask&1 != 0 || mask&(1<<63) == 0 || mask&(1<<17) == 0 {
		t.Fatalf("all = %b, %v", mask, err)
	}
	if mask, err := parseExecSignals("none"); err != nil || mask != 0 {
		t.Fatalf("none = %b, %v", mask, err)
	}
	for _, bad := range []string{"SIGNOPE", "0", "64", "term,"} {
		if _, err := parseExecSignals(bad); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}

func TestExecAggregatorCountsShortLivedAndSignals(t *testing.T) {
	code1, code0 := int32(1), int32(0)
	short, long := 120.0, 90_000.0
	sender := uint32(1)
	events := []execEvent{
		{Event: execEventExec, Process: "curl", Parent: "cron", PID: 10},
		{Event: execEventExit, Process: "curl", Parent: "cron", PID: 10, ExitCode: &code1, LifetimeMS: &short},
		{Event: execEventExec, Process: "curl", Parent: "cron", PID: 11},
		{Event: execEventExit, Process: "curl", Parent: "cron", PID: 11, ExitCode: &code0, LifetimeMS: &short},
		{Event: execEventExit, Process: "java", Parent: "systemd", PID: 20, ExitCode: &code0, Signal: "SIGTERM", LifetimeMS: &long},
		{Event: execEventSignal, Process: "java", PID: 20, Signal: "SIGTERM", Sender: "systemd", SenderPID: &sender, Result: "delivered"},
		{Event: execEventSignal, Process: "java", PID: 20, Signal: "SIGTERM", Sender: "systemd", SenderPID: &sender, Result: "delivered"},
		{Event: execEventSignal, Process: "java", PID: 20, Signal: "SIGKILL", Sender: "kworker", FromKernel: true, Result: "delivered"},
		{Event: execEventExit, Process: "java", Parent: "systemd", PID: 21, ExitCode: &code0, Signal: "SIGKILL", OOMKilled: true, LifetimeMS: &long},
		{Event: execEventOOM, Process: "java", PID: 21, Sender: "java", SenderPID: &sender, FromKernel: true},
	}
	aggregator := newExecAggregator("")
	for _, event := range events {
		aggregator.add(event)
	}
	report := aggregator.report(0, execSummary{EventCount: 8})
	if report.DurationMS != 1 || len(report.Rows) != 2 || report.Rows[0].Group != "curl" {
		t.Fatalf("report = %+v", report)
	}
	curl := report.Rows[0]
	if curl.Execs != 2 || curl.Exits != 2 || curl.NonZeroExits != 1 || curl.ShortLived != 2 || curl.Signaled != 0 || *curl.AvgLifetimeMS != 120 || *curl.MaxLifetimeMS != 120 {
		t.Fatalf("curl = %+v", curl)
	}
	java := report.Rows[1]
	if java.Execs != 0 || java.Exits != 2 || java.Signaled != 2 || java.OOMKilled != 1 || java.ShortLived != 0 || *java.AvgLifetimeMS != 90_000 {
		t.Fatalf("java = %+v", java)
	}
	if len(report.Signals) != 2 || report.Signals[0].Count != 2 || report.Signals[0].Sender != "systemd" || report.Signals[0].Target != "java" || report.Signals[1].Sender != "kernel" || !report.Signals[1].FromKernel {
		t.Fatalf("signals = %+v", report.Signals)
	}
	if len(report.OOMs) != 1 || report.OOMs[0].PID != 21 {
		t.Fatalf("ooms = %+v", report.OOMs)
	}
	byParent := newExecAggregator(execGroupByParent)
	for _, event := range events {
		byParent.add(event)
	}
	parents := byParent.report(time.Second, execSummary{})
	if len(parents.Rows) != 4 || parents.Rows[0].Group != "cron" || parents.Rows[0].Execs != 2 {
		t.Fatalf("by parent = %+v", parents.Rows)
	}
	byEvent := newExecAggregator(traceGroupByEvent)
	for _, event := range events {
		byEvent.add(event)
	}
	if rows := byEvent.report(time.Second, execSummary{}).Rows; len(rows) != 4 || rows[0].Group != "exit" || rows[0].Exits != 4 {
		t.Fatalf("by event = %+v", rows)
	}
}

func TestExecEventDetailAndProcessFilter(t *testing.T) {
	code := int32(137)
	life := 2500.0
	sender := uint32(99)
	zero, adj := int32(0), int32(-500)
	anon, file, shmem := uint64(2*1024*1024), uint64(3*1024), uint64(0)
	cases := []struct {
		event execEvent
		want  string
	}{
		{execEvent{Event: execEventExec, PPID: 1, Parent: "systemd", Args: "nginx -g daemon off;", ArgsTruncated: true}, "ppid 1 systemd · nginx -g daemon off; …"},
		{execEvent{Event: execEventExit, ExitCode: &code, Signal: "SIGKILL", CoreDumped: true, LifetimeMS: &life}, "code 137 · SIGKILL · core · after 2.5s"},
		{execEvent{Event: execEventSignal, Signal: "SIGTERM", Sender: "systemd", SenderPID: &sender, Result: "delivered"}, "SIGTERM ← systemd(99) · delivered"},
		{execEvent{Event: execEventSignal, Signal: "SIGSEGV", Sender: "app", SenderPID: &sender, FromKernel: true, Result: "delivered"}, "SIGSEGV ← kernel via app(99) · delivered"},
		{execEvent{Event: execEventSignal, Signal: "SIGKILL", Result: "delivered"}, "SIGKILL ← kernel · delivered"},
		{execEvent{Event: execEventExit, ExitCode: &zero, Signal: "SIGKILL", OOMKilled: true, LifetimeMS: &life}, "code 0 · SIGKILL · oom-killed · after 2.5s"},
		{execEvent{Event: execEventOOM, Sender: "stress", SenderPID: &sender}, "oom victim ← stress(99)"},
		{execEvent{Event: execEventOOM, Sender: "stress", SenderPID: &sender, AnonRSSKB: &anon, FileRSSKB: &file, ShmemRSSKB: &shmem, OOMScoreAdj: &adj}, "oom victim ← stress(99) · anon 2.0GiB file 3.0MiB shmem 0B · adj -500"},
	}
	for _, test := range cases {
		if got := execEventDetail(test.event); got != test.want {
			t.Errorf("%s detail = %q, want %q", test.event.Event, got, test.want)
		}
	}
	event := execEvent{Process: "nginx", PID: 4242}
	for filter, want := range map[string]bool{"": true, "nginx": true, "4242": true, "java, nginx": true, "java": false, "424": false} {
		if got := execProcessMatches(filter, event); got != want {
			t.Errorf("filter %q = %v, want %v", filter, got, want)
		}
	}
	for ms, want := range map[float64]string{0.25: "0.25ms", 12: "12ms", 1500: "1.5s"} {
		if got := execLifetimeText(ms); got != want {
			t.Errorf("lifetime %v = %q, want %q", ms, got, want)
		}
	}
	if !strings.Contains(execSignalName(31), "SYS") || execSignalName(40) != "SIG40" {
		t.Fatal("signal names")
	}
}

func TestExecOOMJoinerMarksAnExitThatCameBeforeItsOOM(t *testing.T) {
	start := time.Unix(100, 0)
	joiner := &execOOMJoiner{}
	killed := execEvent{Event: execEventExit, PID: 42, Signal: "SIGKILL"}
	if ready := joiner.add(killed, start); len(ready) != 0 {
		t.Fatalf("SIGKILL exit was not held: %+v", ready)
	}
	other := execEvent{Event: execEventExec, PID: 7}
	if ready := joiner.add(other, start.Add(time.Millisecond)); len(ready) != 1 || ready[0].PID != 7 {
		t.Fatalf("unrelated event was held: %+v", ready)
	}
	if ready := joiner.add(execEvent{Event: execEventOOM, PID: 99}, start.Add(2*time.Millisecond)); len(ready) != 1 || ready[0].Event != execEventOOM {
		t.Fatalf("OOM of another PID joined the exit: %+v", ready)
	}
	ready := joiner.add(execEvent{Event: execEventOOM, PID: 42}, start.Add(3*time.Millisecond))
	if len(ready) != 2 || ready[0].Event != execEventOOM || ready[1].Event != execEventExit || !ready[1].OOMKilled {
		t.Fatalf("want OOM then the OOM-killed exit, got %+v", ready)
	}
	if _, ok := joiner.nextDeadline(); ok {
		t.Fatal("joined exit is still held")
	}
}

func TestExecOOMJoinerReleasesHeldExitsInOrder(t *testing.T) {
	start := time.Unix(100, 0)
	joiner := &execOOMJoiner{}
	for _, event := range []execEvent{
		{Event: execEventExit, PID: 1},
		{Event: execEventExit, PID: 2, Signal: "SIGTERM"},
		{Event: execEventExit, PID: 3, Signal: "SIGKILL", OOMKilled: true},
	} {
		if ready := joiner.add(event, start); len(ready) != 1 {
			t.Fatalf("exit %d was held: %+v", event.PID, ready)
		}
	}
	joiner.add(execEvent{Event: execEventExit, PID: 10, Signal: "SIGKILL"}, start)
	joiner.add(execEvent{Event: execEventExit, PID: 11, Signal: "SIGKILL"}, start.Add(10*time.Millisecond))
	if deadline, ok := joiner.nextDeadline(); !ok || !deadline.Equal(start.Add(execOOMHold)) {
		t.Fatalf("next deadline = %v %v", deadline, ok)
	}
	if ready := joiner.flush(start.Add(execOOMHold-time.Nanosecond), false); len(ready) != 0 {
		t.Fatalf("exit released before its deadline: %+v", ready)
	}
	ready := joiner.add(execEvent{Event: execEventExec, PID: 20}, start.Add(execOOMHold))
	if len(ready) != 2 || ready[0].PID != 10 || ready[1].PID != 20 || ready[0].OOMKilled {
		t.Fatalf("want the expired exit before the new event, got %+v", ready)
	}
	if ready := joiner.flush(start, true); len(ready) != 1 || ready[0].PID != 11 {
		t.Fatalf("final flush = %+v", ready)
	}
}

func TestExecOOMJoinerClearsReleasedExits(t *testing.T) {
	start := time.Unix(100, 0)
	joiner := &execOOMJoiner{}
	for i := uint32(0); i < 4; i++ {
		joiner.add(execEvent{Event: execEventExit, PID: 10 + i, Signal: "SIGKILL", Process: "worker"}, start.Add(time.Duration(i)*time.Millisecond))
	}
	backing := joiner.held
	ready := joiner.add(execEvent{Event: execEventOOM, PID: 12}, start.Add(4*time.Millisecond))
	if len(ready) != 2 || ready[1].PID != 12 || !ready[1].OOMKilled {
		t.Fatalf("joined = %+v", ready)
	}
	if backing[3].event.Process != "" {
		t.Fatalf("joined exit left a copy in the array: %+v", backing[3])
	}
	ready = joiner.flush(start.Add(execOOMHold+time.Millisecond), false)
	if len(ready) != 2 || ready[0].PID != 10 || ready[1].PID != 11 {
		t.Fatalf("expired = %+v", ready)
	}
	if backing[0].event.Process != "" || backing[1].event.Process != "" {
		t.Fatalf("released exits stay in the array: %+v", backing[:2])
	}
	if deadline, ok := joiner.nextDeadline(); !ok || !deadline.Equal(start.Add(3*time.Millisecond+execOOMHold)) {
		t.Fatalf("next deadline = %v %v", deadline, ok)
	}
	if ready := joiner.flush(start.Add(time.Hour), false); len(ready) != 1 || ready[0].PID != 13 || joiner.held != nil {
		t.Fatalf("last flush = %+v, held %+v", ready, joiner.held)
	}
}
