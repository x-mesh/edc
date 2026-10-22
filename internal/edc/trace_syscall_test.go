package edc

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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
	for _, want := range []string{"cron     read", "3 ERESTART_RESTARTBLOCK", "10.00ms", "<16.38ms", "... 4 more rows. Add --all to show every row, or use --json", "IN FLIGHT AT STOP", "epoll_wait                    2  900.00ms", "unmatched -", "stats 25/4096"} {
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
		{"--raw", "--no-files"},
		{"--"},
		{"--process", "api", "--", "ls"},
		{"--container", "web", "--", "ls"},
		{"--duration", "5s", "--", "ls"},
		{"--files", "--no-files"},
		{"--json", "-", "--", "ls"},
	} {
		captureTraceStderr(t, func() {
			if code := runTraceSyscall(args); code != 2 {
				t.Errorf("runTraceSyscall(%q) = %d, want 2", args, code)
			}
		})
	}
}

func TestParseSyscallNamesKeepsBothTablesOnAmd64(t *testing.T) {
	native, compat, err := parseSyscallNames("amd64", "openat, GETPPID")
	if err != nil || !reflect.DeepEqual(native, []uint32{257, 110}) || !reflect.DeepEqual(compat, []uint32{295, 64}) {
		t.Fatalf("amd64 = %v %v %v", native, compat, err)
	}
	native, compat, err = parseSyscallNames("arm64", "openat")
	if err != nil || !reflect.DeepEqual(native, []uint32{56}) || compat != nil {
		t.Fatalf("arm64 = %v %v %v", native, compat, err)
	}
	// arm64에는 open이 없고 openat만 있다.
	if _, _, err := parseSyscallNames("arm64", "open"); err == nil || !strings.Contains(err.Error(), `"open"`) {
		t.Fatalf("arm64 open = %v", err)
	}
	if _, _, err := parseSyscallNames("amd64", "openat,,read"); err == nil {
		t.Fatal("an empty item was accepted")
	}
}

func TestSyscallEventLineShowsTheResultAndThePath(t *testing.T) {
	for _, test := range []struct {
		event syscallEvent
		want  string
	}{
		{syscallEvent{PID: 7, TID: 9, Process: "api", Syscall: "openat", Return: -2, Errno: "ENOENT", LatencyMS: 0.0123, Path: "/etc/app.conf"}, "  api 7/9  openat  ENOENT  12.3us  /etc/app.conf"},
		{syscallEvent{PID: 7, TID: 7, Process: "api", Syscall: "read", Return: 512, LatencyMS: 25}, "  api 7/7  read  512  25.00ms"},
		{syscallEvent{PID: 7, TID: 7, Process: "api", Syscall: "statx", Return: -2, Errno: "ENOENT", LatencyMS: 0.001, Path: "/tmp/\x1b[2Jx", PathTruncated: true}, `  statx  ENOENT  1.0us  /tmp/\x1b[2Jx…`},
		{syscallEvent{PID: 7, TID: 7, Process: "api", Syscall: "unlinkat", Return: -14, Errno: "EFAULT", LatencyMS: 0.001, PathUnreadable: true}, "  EFAULT  1.0us  (path unreadable)"},
	} {
		if line := test.event.line(); !strings.HasSuffix(line, test.want) {
			t.Errorf("line = %q, want suffix %q", line, test.want)
		}
	}
}

func TestSyscallReportPrintsEventCountsOnlyWithEvents(t *testing.T) {
	collection := syscallTestCollection()
	var out bytes.Buffer
	syscallReportFor(collection, "").print(&out)
	if strings.Contains(out.String(), "sampled_out") {
		t.Fatalf("event counts without events:\n%s", out.String())
	}
	report := syscallReportFor(collection, "")
	report.Summary.ErrorFilter, report.Summary.Events, report.Summary.SampledOut = "all", 12, 3
	out.Reset()
	report.print(&out)
	if !strings.Contains(out.String(), "events 12  sampled_out 3  ring_lost 0") {
		t.Fatalf("no event counts:\n%s", out.String())
	}
}

func TestRunTraceSyscallRejectsBadEventOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--syscall", "no_such_call"},
		{"--slow", "-1ms"},
		{"--raw", "--errors", "all", "--json", "out.json"},
		{"--raw", "--slow", "1ms", "--group-by", "syscall"},
	} {
		captureTraceStderr(t, func() {
			if code := runTraceSyscall(args); code != 2 {
				t.Errorf("runTraceSyscall(%q) = %d, want 2", args, code)
			}
		})
	}
}

func TestSyscallLiveViewFitsTheScreenHeight(t *testing.T) {
	// 표가 가득 차는 수집 결과: 행 40개, 진행 중인 호출 서로 다른 15가지, 파일 30개.
	var collection syscallCollection
	for index := range 40 {
		collection.samples = append(collection.samples, syscallSample{Process: "api", Syscall: "call_" + strconv.Itoa(index), Calls: 1, Timed: 1, SumNS: uint64(index + 1)})
	}
	for index := range 15 {
		collection.inFlight = append(collection.inFlight, syscallInFlightSample{Process: "api", Syscall: "wait_" + strconv.Itoa(index), ElapsedNS: uint64(index + 1)})
	}
	var table syscallFileTable
	for index := range 30 {
		table.add(syscallEvent{Syscall: "openat", Path: "/f/" + strconv.Itoa(index), Action: "write"})
	}
	for _, test := range []struct {
		height                int
		files                 bool
		rows, inFlight, shown int
	}{
		{24, false, 11, 2, 0},
		{60, false, syscallTraceRows, 8, 0},
		{8, false, 1, 0, 0},
		{24, true, 8, 0, 4},
		{40, true, 12, 4, 8},
		{60, true, syscallTraceRows, 8, 14},
	} {
		view := syscallLiveView(test.height, test.files, 0)
		if view.rows != test.rows || view.inFlight != test.inFlight || view.files != test.shown || !view.live {
			t.Errorf("height %d files %t: view = %+v, want rows %d, in-flight %d, files %d", test.height, test.files, view, test.rows, test.inFlight, test.shown)
		}
		report := syscallReportFor(collection, "")
		if test.files {
			report.Files, report.Summary.Files = table.report(), true
		}
		var output bytes.Buffer
		report.render(&output, view)
		// 마지막 줄은 비워 둬야 표를 다시 그릴 때 화면이 스크롤하지 않는다.
		if lines := strings.Count(output.String(), "\n"); lines > test.height-1 && test.height >= 24 {
			t.Errorf("height %d files %t: %d lines overflow the screen:\n%s", test.height, test.files, lines, output.String())
		}
	}
}

func TestSyscallLiveRenderShowsRateAndCtrlC(t *testing.T) {
	var output bytes.Buffer
	syscallReportFor(syscallTestCollection(), "").render(&output, syscallView{rows: 5, inFlight: 5, live: true, rate: 1234.4})
	for _, want := range []string{"rate 1234/s", "IN FLIGHT", "Ctrl-C stops and prints the final table"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("live output lacks %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "AT STOP") || strings.Contains(output.String(), "--json") {
		t.Errorf("live output uses the final-table wording:\n%s", output.String())
	}
	var final bytes.Buffer
	syscallReportFor(syscallTestCollection(), "").print(&final)
	if strings.Contains(final.String(), "Ctrl-C") || strings.Contains(final.String(), "rate") {
		t.Errorf("final output carries live wording:\n%s", final.String())
	}
}

func TestSyscallLiveWriterRedrawsWithTheCallRate(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "live")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	write := newSyscallSnapshotHandler(file, "", nil, nil, true)
	first := syscallTestCollection()
	first.summary.DurationMS = 1000
	write(first)
	second := syscallTestCollection()
	second.summary.DurationMS = 3000
	second.samples[0].Calls += 2000
	write(second)
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if got := strings.Count(text, syscallLiveHome); got != 2 || strings.Count(text, syscallLiveEraseBelow) != 2 {
		t.Errorf("frames = %d, want 2 that start at Home and erase below:\n%s", got, text)
	}
	// 화면을 먼저 지우면 깜빡인다. 프레임은 지우지 않고 덮어쓴다.
	if strings.Contains(text, syscallLiveClear) {
		t.Errorf("a frame clears the screen before it draws:\n%q", text)
	}
	if !strings.Contains(text, syscallLiveEraseLine+"\n") {
		t.Errorf("the lines do not erase their old tail:\n%q", text)
	}
	if !strings.Contains(text, "1000/s") {
		t.Errorf("second frame lacks a rate of 1000/s (2000 calls over 2s):\n%s", text)
	}
}

func TestTraceCommandExitCodeFollowsTheShell(t *testing.T) {
	for _, test := range []struct {
		script string
		want   int
	}{{"exit 0", 0}, {"exit 3", 3}, {"kill -9 $$", 137}} {
		command := &traceCommand{waitErr: exec.Command("sh", "-c", test.script).Run()}
		if got := command.exitCode(); got != test.want {
			t.Errorf("%q: exit code = %d, want %d", test.script, got, test.want)
		}
	}
	if got := (&traceCommand{waitErr: errors.New("wait failed")}).exitCode(); got != 1 {
		t.Errorf("a wait error gave %d, want 1", got)
	}
}

func TestTraceCommandDoesNotStartAfterAnInterrupt(t *testing.T) {
	command := &traceCommand{argv: []string{"true"}, path: "/bin/true", done: make(chan struct{})}
	command.interrupt(syscall.SIGINT)
	if err := command.start(nil); !errors.Is(err, errTraceCommandInterrupted) {
		t.Fatalf("start after an interrupt = %v", err)
	}
}

func TestPrepareTraceCommandRejectsAMissingCommand(t *testing.T) {
	if _, err := prepareTraceCommand([]string{"edc-no-such-command"}); err == nil {
		t.Fatal("a missing command was accepted")
	}
}

func TestSyscallFileActionFollowsTheOpenFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		kind uint8
		want string
	}{
		{"openat", 0, "read"},
		{"openat", syscallOpenWrite, "write"},
		{"openat", syscallOpenWrite | syscallOpenCreate, "write"},
		{"openat", syscallOpenWrite | syscallOpenCreate | syscallOpenExcl, "create"},
		{"open", syscallOpenCreate, "create"},
		{"creat", syscallOpenWrite | syscallOpenCreate | syscallOpenTrunc, "write"},
		{"openat2", syscallOpenUnknown, "other"},
		{"truncate", 0, "write"},
		{"unlinkat", 0, "delete"},
		{"rmdir", 0, "delete"},
		{"renameat2", 0, "rename"},
		{"mkdir", 0, "create"},
		{"symlinkat", 0, "create"},
		{"fchmodat", 0, "other"},
		{"statx", 0, "other"},
		{"faccessat2", 0, "other"},
		{"read", 0, ""},
		{"getdents64", 0, ""},
	} {
		if got := syscallFileAction(test.name, test.kind); got != test.want {
			t.Errorf("syscallFileAction(%q, %d) = %q, want %q", test.name, test.kind, got, test.want)
		}
	}
}

// 파일 표에 넣는 호출은 모두 경로를 읽어야 한다. 경로 표에 빠지면 BPF가 경로 없는 event를 내서 표가 비어 보인다.
func TestSyscallFileCallsAllReadAPath(t *testing.T) {
	for name := range syscallFileCalls {
		if _, ok := syscallPathArgs[name]; !ok {
			t.Errorf("%s is a file call without a path argument", name)
		}
	}
}

func TestSyscallFileTableCountsAttemptsAndErrors(t *testing.T) {
	var table syscallFileTable
	for _, event := range []syscallEvent{
		{Syscall: "openat", Path: "/etc/app.conf", Action: "read", Errno: "ENOENT"},
		{Syscall: "openat", Path: "/etc/app.conf", Action: "read", Errno: "ENOENT"},
		{Syscall: "openat", Path: "/tmp/out", Action: "create"},
		{Syscall: "write", Path: "/ignored", Action: ""},
		{Syscall: "unlinkat", Path: "/tmp/out", Action: "delete"},
		{Syscall: "openat", PathUnreadable: true, Action: "read"},
		{Syscall: "openat", Path: "/very/long", PathTruncated: true, Action: "write"},
	} {
		table.add(event)
	}
	rows := table.report()
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	byPath := map[string]syscallFileRow{}
	for _, row := range rows {
		byPath[row.Path] = row
	}
	conf := byPath["/etc/app.conf"]
	if conf.Read != 2 || conf.Errors != 2 || conf.errnoText() != "2 ENOENT" {
		t.Errorf("conf row = %+v", conf)
	}
	if out := byPath["/tmp/out"]; out.Create != 1 || out.Delete != 1 {
		t.Errorf("out row = %+v", out)
	}
	paths := map[string]bool{}
	for _, row := range rows {
		paths[row.Path] = true
	}
	if !paths[syscallUnreadablePath] || !paths["/very/long…"] || paths["/ignored"] {
		t.Errorf("paths = %v", paths)
	}
}

func TestSyscallFilesTablePrintsAfterTheCallTable(t *testing.T) {
	report := syscallReportFor(syscallTestCollection(), "")
	var table syscallFileTable
	table.add(syscallEvent{Syscall: "openat", Path: "/etc/app.conf", Action: "read", Errno: "ENOENT"})
	report.Files = table.report()
	report.Summary.Files = true
	var output bytes.Buffer
	report.print(&output)
	text := output.String()
	if !strings.Contains(text, "FILES: WRITE includes opens that create the file") || !strings.Contains(text, "1 ENOENT /etc/app.conf") {
		t.Errorf("no file table:\n%s", text)
	}
	if strings.Index(text, "FILES") < strings.Index(text, "IN FLIGHT") || !strings.Contains(text, "events") {
		t.Errorf("the file table is out of order or the event counters are missing:\n%s", text)
	}
	report.Summary.RingLost = 3
	output.Reset()
	report.print(&output)
	if !strings.Contains(output.String(), "the table can be incomplete: edc did not record 3 events") {
		t.Errorf("lost events are not reported:\n%s", output.String())
	}
	var without bytes.Buffer
	syscallReportFor(syscallTestCollection(), "").print(&without)
	if strings.Contains(without.String(), "FILES") {
		t.Errorf("a trace without --files prints a file table:\n%s", without.String())
	}
}

func TestSyscallAllShowsEveryRow(t *testing.T) {
	var collection syscallCollection
	for index := range 30 {
		collection.samples = append(collection.samples, syscallSample{Process: "api", Syscall: "call_" + strconv.Itoa(index), Calls: 1, Timed: 1, SumNS: uint64(index + 1)})
	}
	var table syscallFileTable
	for index := range 30 {
		table.add(syscallEvent{Syscall: "openat", Path: "/f/" + strconv.Itoa(index), Action: "read"})
	}
	report := syscallReportFor(collection, "")
	report.Files = table.report()
	var limited, all bytes.Buffer
	report.render(&limited, syscallFinalView(false))
	report.render(&all, syscallFinalView(true))
	if !strings.Contains(limited.String(), "... 10 more rows. Add --all to show every row, or use --json") || strings.Count(limited.String(), "more rows") != 2 {
		t.Errorf("limited output lacks the hint for both tables:\n%s", limited.String())
	}
	if strings.Contains(all.String(), "more rows") || !strings.Contains(all.String(), "call_0 ") || !strings.Contains(all.String(), "/f/29") {
		t.Errorf("--all output still hides rows:\n%s", all.String())
	}
}

func TestSyscallFileTableSortsChangedPathsFirst(t *testing.T) {
	var table syscallFileTable
	for range 50 {
		table.add(syscallEvent{Syscall: "openat", Path: "/lib/libc.so.6", Action: "read"})
	}
	table.add(syscallEvent{Syscall: "statx", Path: "/etc/missing.conf", Action: "other", Errno: "ENOENT"})
	table.add(syscallEvent{Syscall: "openat", Path: "/var/log/app.log", Action: "write"})
	table.add(syscallEvent{Syscall: "unlinkat", Path: "/tmp/x", Action: "delete"})
	table.add(syscallEvent{Syscall: "unlinkat", Path: "/tmp/x", Action: "delete"})
	var paths []string
	for _, row := range table.report() {
		paths = append(paths, row.Path)
	}
	if want := []string{"/tmp/x", "/var/log/app.log", "/etc/missing.conf", "/lib/libc.so.6"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("order = %v, want %v", paths, want)
	}
}

func TestSyscallFileTableStopsAddingPathsAtTheLimit(t *testing.T) {
	var table syscallFileTable
	for index := range syscallFilePathMax + 5 {
		table.add(syscallEvent{Syscall: "openat", Path: "/f/" + strconv.Itoa(index), Action: "read"})
	}
	table.add(syscallEvent{Syscall: "openat", Path: "/f/0", Action: "read"})
	rows := table.report()
	if len(rows) != syscallFilePathMax || table.dropped != 5 {
		t.Fatalf("rows = %d, dropped = %d", len(rows), table.dropped)
	}
	var counted uint64
	for _, row := range rows {
		if row.Path == "/f/0" {
			counted = row.Read
		}
	}
	if counted != 2 {
		t.Errorf("a path that is already in the table stopped counting: %d", counted)
	}
	report := syscallReportFor(syscallTestCollection(), "")
	report.Files, report.FilesDropped, report.Summary.Files = rows, table.dropped, true
	var output bytes.Buffer
	report.print(&output)
	if !strings.Contains(output.String(), "the table holds the first 10000 paths: 5 file events of other paths are not in it") {
		t.Errorf("no notice about the path limit:\n%s", output.String())
	}
}
