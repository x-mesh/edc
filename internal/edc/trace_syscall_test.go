package edc

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSyscallProcessFilter(t *testing.T) {
	pids, comms, err := parseSyscallProcessFilter(" 42, nginx ,a-process-name-longer-than-comm")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pids, []uint32{42}) || !reflect.DeepEqual(comms, []string{"nginx", "a-process-name-"}) {
		t.Fatalf("pids = %v, comms = %q", pids, comms)
	}
	if pids, comms, err := parseSyscallProcessFilter(" "); err != nil || pids != nil || comms != nil {
		t.Fatalf("empty filter = %v %v %v", pids, comms, err)
	}
	if _, _, err := parseSyscallProcessFilter("nginx,,api"); err == nil {
		t.Fatal("an empty item was accepted")
	}
}

func TestSyscallSelfTGIDNeedsTheHostPIDNamespace(t *testing.T) {
	if tgid, ok := syscallSelfTGID(auditHostPIDNamespace, 1234); !ok || tgid != 1234 {
		t.Fatalf("host namespace = %d %t", tgid, ok)
	}
	if tgid, ok := syscallSelfTGID("pid:[4026532201]", 1234); ok || tgid != 0 {
		t.Fatalf("container namespace = %d %t", tgid, ok)
	}
	if _, ok := syscallSelfTGID("", 1234); ok {
		t.Fatal("an unreadable namespace was treated as the host namespace")
	}
}

func TestSyscallNamesFollowTheArchitecture(t *testing.T) {
	for _, test := range []struct {
		arch   string
		nr     uint32
		compat bool
		want   string
	}{
		{"amd64", 110, false, "getppid"},
		{"amd64", 64, false, "semget"},
		// 32비트 호환 호출은 같은 번호라도 i386 표의 이름이다.
		{"amd64", 64, true, "getppid (32-bit)"},
		{"arm64", 173, false, "getppid"},
		{"arm64", 9999, false, "syscall_9999"},
		{"riscv64", 1, false, "syscall_1"},
	} {
		if got := syscallName(test.arch, test.nr, test.compat); got != test.want {
			t.Errorf("syscallName(%s, %d, %t) = %q, want %q", test.arch, test.nr, test.compat, got, test.want)
		}
	}
	for _, test := range []struct {
		arch, name string
		want       uint32
	}{
		{"amd64", "exit_group", 231}, {"arm64", "exit_group", 94}, {"386", "exit", 1},
	} {
		if got, ok := syscallNumber(test.arch, test.name); !ok || got != test.want {
			t.Errorf("syscallNumber(%s, %s) = %d %t, want %d", test.arch, test.name, got, ok, test.want)
		}
	}
	if _, ok := syscallNumber("amd64", "no_such_call"); ok {
		t.Error("an unknown name has a number")
	}
}

func syscallTestHist(buckets map[int]uint64) topProbeHist {
	var hist topProbeHist
	for index, count := range buckets {
		hist[index] = count
	}
	return hist
}

func syscallTestCollection() syscallCollection {
	return syscallCollection{
		samples: []syscallSample{
			{Process: "api", CgroupID: 7, Syscall: "read", Calls: 3, Timed: 3, SumNS: uint64(3 * time.Millisecond), MaxNS: uint64(2 * time.Millisecond), Hist: syscallTestHist(map[int]uint64{9: 2, 10: 1})},
			{Process: "api", CgroupID: 8, Syscall: "read", Calls: 1, Timed: 1, SumNS: uint64(time.Millisecond), MaxNS: uint64(time.Millisecond), Hist: syscallTestHist(map[int]uint64{9: 1})},
			{Process: "api", CgroupID: 7, Syscall: "openat", Calls: 4, Timed: 4, Errors: 3, SumNS: 4000, MaxNS: 2000, Hist: syscallTestHist(map[int]uint64{0: 4}), Errnos: map[string]uint64{"ENOENT": 2, "EACCES": 1}},
			{Process: "cron", CgroupID: 9, Syscall: "read", Calls: 2, Timed: 2, SumNS: uint64(10 * time.Millisecond), MaxNS: uint64(9 * time.Millisecond), Hist: syscallTestHist(map[int]uint64{13: 2})},
			{Process: "cron", CgroupID: 9, Syscall: "exit_group", Calls: 1},
		},
		inFlight: []syscallInFlightSample{
			{Process: "api", Syscall: "epoll_wait", ElapsedNS: uint64(400 * time.Millisecond)},
			{Process: "api", Syscall: "epoll_wait", ElapsedNS: uint64(900 * time.Millisecond)},
			{Process: "cron", Syscall: "read", ElapsedNS: uint64(100 * time.Millisecond)},
		},
		summary: syscallSummary{DurationMS: 2000, StatsLimit: 4096, SelfExcluded: true},
	}
}

func TestSyscallReportMergesProcessesAndSortsByTotalTime(t *testing.T) {
	report := syscallReportFor(syscallTestCollection(), "")
	var order []string
	for _, row := range report.Rows {
		order = append(order, row.Process+" "+row.Syscall)
	}
	if want := []string{"cron read", "api read", "api openat", "cron exit_group"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("rows = %q, want %q", order, want)
	}
	read := report.Rows[1]
	if read.Calls != 4 || read.TotalMS != 4 || read.AverageMS == nil || *read.AverageMS != 1 || read.MaxMS == nil || *read.MaxMS != 2 || read.CgroupID != nil {
		t.Fatalf("api read = %+v", read)
	}
	// 4건 중 p95는 4번째 값이 든 bucket 10, 위쪽 경계 2^11 us다.
	if read.P95MS == nil || *read.P95MS != 2.048 {
		t.Fatalf("api read p95 = %v", read.P95MS)
	}
	if openat := report.Rows[2]; openat.errnoText() != "3 ENOENT" {
		t.Fatalf("openat errors = %q", openat.errnoText())
	}
	exit := report.Rows[3]
	if exit.Calls != 1 || exit.AverageMS != nil || exit.MaxMS != nil || exit.P95MS != nil {
		t.Fatalf("exit_group has a duration: %+v", exit)
	}
	if report.Summary.Calls != 11 || report.Summary.Errors != 3 || report.Summary.StatsEntries != 5 || report.Summary.InFlight != 3 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if want := []syscallInFlightRow{{Process: "api", Syscall: "epoll_wait", Threads: 2, LongestMS: 900}, {Process: "cron", Syscall: "read", Threads: 1, LongestMS: 100}}; !reflect.DeepEqual(report.InFlight, want) {
		t.Fatalf("in flight = %+v", report.InFlight)
	}
}

func TestSyscallReportGroupsByOneColumn(t *testing.T) {
	bySyscall := syscallReportFor(syscallTestCollection(), traceGroupBySyscall)
	if len(bySyscall.Rows) != 3 || bySyscall.Rows[0].Syscall != "read" || bySyscall.Rows[0].Calls != 6 || bySyscall.Rows[0].Process != "" {
		t.Fatalf("by syscall = %+v", bySyscall.Rows)
	}
	byCgroup := syscallReportFor(syscallTestCollection(), traceGroupByCgroup)
	if len(byCgroup.Rows) != 3 || byCgroup.Rows[0].CgroupID == nil || *byCgroup.Rows[0].CgroupID != 9 || byCgroup.Rows[0].Calls != 3 {
		t.Fatalf("by cgroup = %+v", byCgroup.Rows)
	}
	byProcess := syscallReportFor(syscallTestCollection(), traceGroupByProcess)
	if len(byProcess.Rows) != 2 || byProcess.Rows[0].Process != "cron" || byProcess.Rows[1].Errnos["ENOENT"] != 2 {
		t.Fatalf("by process = %+v", byProcess.Rows)
	}
}

func TestSyscallReportPrintsAlignedRows(t *testing.T) {
	collection := syscallTestCollection()
	for index := 0; index < syscallTraceRows; index++ {
		collection.samples = append(collection.samples, syscallSample{Process: "batch", Syscall: "syscall_" + strings.Repeat("x", index), Calls: 1, Timed: 1, SumNS: 10})
	}
	collection.samples[2].Errnos = map[string]uint64{"ERESTART_RESTARTBLOCK": 3}
	var out bytes.Buffer
	syscallReportFor(collection, "").print(&out)
	text := out.String()
	lines := strings.Split(text, "\n")
	header, first := lines[1], lines[2]
	if !strings.HasPrefix(header, "PROCESS  SYSCALL") || strings.Index(header, "CALLS")+len("CALLS") != strings.Index(first, "2")+1 {
		t.Fatalf("columns do not line up:\n%s", text)
	}
	for _, want := range []string{"cron     read", "3 ERESTART_RESTARTBLOCK", "10.00ms", "<16.38ms", "... 4 more rows in --json", "IN FLIGHT AT STOP", "epoll_wait                    2  900.00ms", "unmatched -", "stats 25/4096"} {
		if !strings.Contains(text, want) {
			t.Errorf("output has no %q:\n%s", want, text)
		}
	}
}

func TestSyscallTimeUsesTheUnitOfTheValue(t *testing.T) {
	for ns, want := range map[float64]string{850: "850ns", 1500: "1.5us", 2.5e6: "2.50ms", 1.5e9: "1.50s"} {
		if got := syscallTime(ns); got != want {
			t.Errorf("syscallTime(%v) = %q, want %q", ns, got, want)
		}
	}
}

func TestRunTraceSyscallRejectsBadOptionsBeforeTracing(t *testing.T) {
	for _, args := range [][]string{
		{"--group-by", "event"},
		{"--duration", "2m"},
		{"--process", "api,,cron"},
		{"extra"},
		{"--raw"},
	} {
		captureTraceStderr(t, func() {
			if code := runTraceSyscall(args); code != 2 {
				t.Errorf("runTraceSyscall(%q) = %d, want 2", args, code)
			}
		})
	}
}
