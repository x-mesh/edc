package edc

//go:generate go run trace_syscall_names_gen.go

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	traceSyscallUsage   = "edc trace syscall [options] [-- <command> [args...]]"
	traceGroupBySyscall = "syscall"
	// syscallTraceRows와 syscallInFlightRows는 표에 보이는 행 수다. --all은 모든 행을 보이고, --json에는 늘 모든 행이 들어간다.
	syscallTraceRows    = 20
	syscallInFlightRows = 10
	// syscallCommMax는 kernel comm의 길이다. 16 byte 중 마지막은 NUL이라 이름은 15 byte까지 남는다.
	syscallCommMax = 15
	// syscallLiveInterval은 실행 중인 표를 다시 그리는 간격이다. 읽는 값은 map의 항목 수만큼이라 짧아도 부담이 작다.
	syscallLiveInterval = time.Second
	syscallLiveHeight   = 24
)

type syscallTraceOptions struct {
	duration     time.Duration
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	groupBy      string
	jsonPath     string
	// syscalls는 --syscall로 고른 번호다. compat은 amd64의 32비트 호환 번호다.
	syscalls       []uint32
	compatSyscalls []uint32
	slow           time.Duration
	errors         string
	errorsAll      bool
	errnos         []uint32
	raw            bool
	// onSnapshot은 liveInterval마다 지금까지의 값을 받는다. event를 내지 않을 때만 쓰며, nil이면 끝날 때 한 번만 읽는다.
	onSnapshot   func(syscallCollection)
	liveInterval time.Duration
	// onAttached는 eBPF program을 붙인 직후에 한 번 불린다. 명령을 여기서 시작하므로 첫 호출부터 센다. allowPID로
	// PID 필터에 번호를 넣는다. waitForPID는 그 번호를 기다리는 동안에도 process 필터를 켠다.
	onAttached func(allowPID func(uint32) error) error
	waitForPID bool
	// files는 파일을 열거나 바꾼 호출을 event로 받아 경로별 표로 모은다. 명령을 실행할 때는 기본으로 켠다.
	files bool
	// all은 최종 표에서 행 수 제한을 푼다.
	all bool
	// snapshotInFlight는 snapshot마다 진행 중인 호출을 읽는다. task iterator를 돌려서 비싸므로, 화면에 그릴 때만 켠다.
	snapshotInFlight bool
}

func (options syscallTraceOptions) processFiltered() bool {
	return len(options.pids)+len(options.comms) > 0 || options.waitForPID
}

func (options syscallTraceOptions) eventsEnabled() bool {
	return options.linesEnabled() || options.files
}

// linesEnabled는 --slow나 --errors로 호출을 한 줄씩 출력하는지다.
func (options syscallTraceOptions) linesEnabled() bool {
	return options.slow > 0 || options.errorsAll || len(options.errnos) > 0
}

// syscallEvent는 --slow나 --errors에 맞은 호출 하나다. Return은 kernel이 돌려준 값이고, 실패하면 Errno에 이름이 있다.
type syscallEvent struct {
	TimestampNS    uint64  `json:"timestamp_ns"`
	Event          string  `json:"event"`
	PID            uint32  `json:"pid"`
	TID            uint32  `json:"tid"`
	Process        string  `json:"process"`
	Syscall        string  `json:"syscall"`
	Return         int64   `json:"return"`
	Errno          string  `json:"errno,omitempty"`
	LatencyMS      float64 `json:"latency_ms"`
	Path           string  `json:"path,omitempty"`
	PathTruncated  bool    `json:"path_truncated,omitempty"`
	PathUnreadable bool    `json:"path_unreadable,omitempty"`
	// Action은 파일 표에 넣는 호출이 파일에 한 일이다. read, write, create, delete, rename, other 중 하나다.
	Action string `json:"action,omitempty"`
	// OpenKind와 Reason은 BPF가 채운다. Reason은 event를 낸 이유로, 줄 출력(EVENT_LINE)과 파일 표(EVENT_FILE)를 나눈다.
	OpenKind uint8 `json:"-"`
	Reason   uint8 `json:"-"`
}

// line은 event 하나를 한 줄로 쓴다. 경로의 제어 문자는 터미널을 바꾸지 못하게 \xNN으로 쓴다.
func (event syscallEvent) line() string {
	result := event.Errno
	if result == "" {
		result = strconv.FormatInt(event.Return, 10)
	}
	text := fmt.Sprintf("%s  %s %d/%d  %s  %s  %s", time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000000"), event.Process, event.PID, event.TID, event.Syscall, result, syscallTime(event.LatencyMS*float64(time.Millisecond)))
	switch {
	case event.PathUnreadable:
		text += "  (path unreadable)"
	case event.Path != "":
		text += "  " + execPrintable([]byte(event.Path))
		if event.PathTruncated {
			text += "…"
		}
	}
	return text
}

// syscallSample은 BPF map에서 읽은 cgroup, process 이름, syscall 하나의 값이다. Timed는 돌아온 호출 수다.
type syscallSample struct {
	Process  string
	CgroupID uint64
	Syscall  string
	Calls    uint64
	Timed    uint64
	Errors   uint64
	SumNS    uint64
	MaxNS    uint64
	Hist     topProbeHist
	Errnos   map[string]uint64
}

// syscallInFlightSample은 trace가 끝날 때 아직 돌아오지 않은 호출 하나다. ElapsedNS는 그때까지 머문 시간이다.
type syscallInFlightSample struct {
	Process   string
	Syscall   string
	ElapsedNS uint64
}

type syscallSummary struct {
	// Event는 --raw의 마지막 줄에만 붙는다. 앞의 event 줄과 구분한다.
	Event      string `json:"event,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Calls      uint64 `json:"calls"`
	Errors     uint64 `json:"errors"`
	InFlight   uint64 `json:"in_flight"`
	// Unmatched는 시작 기록 없이 돌아온 호출이다. 필터가 있으면 세지 않아 nil이다.
	Unmatched    *uint64 `json:"unmatched,omitempty"`
	StatsEntries int     `json:"stats_entries"`
	StatsLimit   uint32  `json:"stats_limit"`
	StatsFull    uint64  `json:"stats_full"`
	StartsFull   uint64  `json:"starts_full"`
	ErrnosFull   uint64  `json:"errnos_full"`
	SelfExcluded bool    `json:"self_excluded"`
	// SlowMS와 ErrorFilter는 event를 낸 조건이다. 둘 다 없으면 event를 내지 않았다.
	SlowMS      *float64 `json:"slow_ms,omitempty"`
	ErrorFilter string   `json:"error_filter,omitempty"`
	// Files는 파일 표를 모았다는 뜻이다. 모은 때만 events 줄을 낸다.
	Files      bool   `json:"files,omitempty"`
	Events     uint64 `json:"events,omitempty"`
	SampledOut uint64 `json:"sampled_out,omitempty"`
	RingLost   uint64 `json:"ring_lost,omitempty"`
}

type syscallCollection struct {
	samples  []syscallSample
	inFlight []syscallInFlightSample
	summary  syscallSummary
}

type syscallReport struct {
	GroupBy  string               `json:"group_by,omitempty"`
	Rows     []syscallReportRow   `json:"rows"`
	InFlight []syscallInFlightRow `json:"in_flight"`
	Files    []syscallFileRow     `json:"files,omitempty"`
	// Events는 조건이 5초 이상 이어진 이상 징후다. 감지하지 않은 trace에서는 없다.
	Events []syscallAnomaly `json:"events,omitempty"`
	// FilesDropped는 경로가 syscallFilePathMax개를 넘어 기록하지 못한 파일 event의 수다.
	FilesDropped uint64         `json:"files_dropped,omitempty"`
	Summary      syscallSummary `json:"summary"`
}

// syscallReportRow의 P95MS는 histogram bucket의 위쪽 경계다. 실제 p95는 그 값보다 작다.
type syscallReportRow struct {
	Process   string            `json:"process,omitempty"`
	Syscall   string            `json:"syscall,omitempty"`
	CgroupID  *uint64           `json:"cgroup_id,omitempty"`
	Calls     uint64            `json:"calls"`
	Errors    uint64            `json:"errors"`
	Errnos    map[string]uint64 `json:"errnos,omitempty"`
	TotalMS   float64           `json:"total_ms"`
	AverageMS *float64          `json:"avg_ms,omitempty"`
	P95MS     *float64          `json:"p95_ms,omitempty"`
	MaxMS     *float64          `json:"max_ms,omitempty"`
	timed     uint64
	sumNS     uint64
	maxNS     uint64
	hist      topProbeHist
}

type syscallInFlightRow struct {
	Process   string  `json:"process"`
	Syscall   string  `json:"syscall"`
	Threads   int     `json:"threads"`
	LongestMS float64 `json:"longest_ms"`
}

func runTraceSyscall(args []string) int {
	options := syscallTraceOptions{}
	var process string
	set := newFlagSet("trace syscall")
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.groupBy, "group-by", "", "process, syscall, or cgroup")
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	var syscalls string
	set.StringVar(&syscalls, "syscall", "", T("command.trace.option.syscall"))
	set.DurationVar(&options.slow, "slow", 0, T("command.trace.option.slow"))
	set.StringVar(&options.errors, "errors", "", T("command.trace.option.errors"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	set.BoolVar(&options.all, "all", false, T("command.trace.option.all"))
	var files, noFiles bool
	set.BoolVar(&files, "files", false, T("command.trace.option.files"))
	set.BoolVar(&noFiles, "no-files", false, T("command.trace.option.no_files"))
	// "--" 뒤는 실행할 명령이다. flag 패키지는 "--"를 지우므로, 명령 없는 위치 인자와 구분하려고 먼저 자른다.
	var argv []string
	if index := slices.Index(args, "--"); index >= 0 {
		args, argv = args[:index], args[index+1:]
		if len(argv) == 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_command_empty"))
			return 2
		}
	}
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace syscall"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	switch options.groupBy {
	case "", traceGroupByProcess, traceGroupBySyscall, traceGroupByCgroup:
	default:
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_group_by"))
		return 2
	}
	pids, comms, err := parseSyscallProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_process", err))
		return 2
	}
	options.pids, options.comms = pids, comms
	if files && noFiles {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_files_conflict"))
		return 2
	}
	// --files는 기본값을 적어 두는 옵션이다. 끄려면 --no-files를 쓴다.
	options.files = !noFiles
	if len(argv) > 0 && options.jsonPath == "-" {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_command_json_stdout"))
		return 2
	}
	if len(argv) > 0 && (len(pids)+len(comms) > 0 || options.containerRef != "" || options.duration > 0) {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_command_conflict"))
		return 2
	}
	if strings.TrimSpace(syscalls) != "" {
		if options.syscalls, options.compatSyscalls, err = parseSyscallNames(runtime.GOARCH, syscalls); err != nil {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_names", err))
			return 2
		}
	}
	if options.slow < 0 {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_range"))
		return 2
	}
	if options.errorsAll, options.errnos, err = parseSyscallErrnos(options.errors); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_errors", err))
		return 2
	}
	switch {
	case options.raw && options.jsonPath != "":
		fmt.Fprintln(os.Stderr, T("cli.trace.raw_json_conflict"))
		return 2
	case options.raw && options.groupBy != "":
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_raw_conflict"))
		return 2
	case options.raw && !options.eventsEnabled():
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_raw_events"))
		return 2
	}
	if err := syscallTracePrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	var command *traceCommand
	if len(argv) > 0 {
		var err error
		if command, err = prepareTraceCommand(argv); err != nil {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_command_failed", argv[0], err))
			return 127
		}
		defer command.close()
		if container := command.container(); container != nil {
			options.container = container
		} else {
			options.waitForPID = true
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_command_pid_only", command.fallback))
		}
	}
	if options.containerRef != "" {
		container, code, err := resolveTraceContainer(options.containerRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return code
		}
		options.container = container
	}
	// event를 내거나 파일로 쓰지 않고 터미널에 그릴 때만 실행 중에 표를 다시 그린다. 그 밖에는 끝날 때 한 번 출력한다.
	live := !options.raw && options.jsonPath == "" && !options.linesEnabled() && traceIsTerminal(os.Stdout) && command == nil
	// 화면 없이 끝날 때 한 번 출력하므로, 터미널에서는 무엇을 기다리는지 알린다.
	if traceIsTerminal(os.Stderr) && !live && command == nil {
		if options.duration > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_running_for", options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_running"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// event는 도착하는 대로 쓴다. --json은 요약만 쓰므로 event 수만 센다.
	var onEvent func(syscallEvent) error
	encoder := json.NewEncoder(os.Stdout)
	var fileTable syscallFileTable
	switch {
	case options.raw:
		// 줄 출력을 고른 때는 그 조건에 맞는 event만 쓴다. 파일 event만 있으면 모두 쓴다.
		onEvent = func(event syscallEvent) error {
			if options.linesEnabled() && event.Reason&syscallReasonLine == 0 {
				return nil
			}
			return encoder.Encode(event)
		}
	case options.eventsEnabled():
		onEvent = func(event syscallEvent) error {
			if options.files && event.Reason&syscallReasonFile != 0 {
				fileTable.add(event)
			}
			if options.jsonPath == "" && event.Reason&syscallReasonLine != 0 {
				_, err := fmt.Fprintln(os.Stdout, event.line())
				return err
			}
			return nil
		}
	}
	stopTrace := ctx.Done()
	exitCode := func() int {
		if command != nil {
			return command.exitCode()
		}
		return 0
	}
	if command != nil {
		// 터미널의 Ctrl-C는 명령에도 가므로 edc는 명령이 끝날 때까지 trace를 이어 간다.
		stopTrace = command.done
		options.onAttached = func(allowPID func(uint32) error) error { return command.start(allowPID) }
		signals := make(chan os.Signal, 4)
		// SIGPIPE를 받아 두면 출력 파이프가 닫혀도 edc가 죽지 않고(쓰기는 EPIPE로 실패한다) cgroup을 지울 수 있다.
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE)
		defer signal.Stop(signals)
		go func() {
			for received := range signals {
				if received != syscall.SIGPIPE {
					command.interrupt(received)
				}
			}
		}()
	}
	// 줄 출력이나 --raw가 아니면 event 조건을 본다. 조건은 1초마다 읽은 snapshot의 증가분으로 계산한다.
	var detector *syscallEventDetector
	if !options.raw && !options.linesEnabled() {
		detector = newSyscallEventDetector()
		var snapshotFiles *syscallFileTable
		if options.files {
			snapshotFiles = &fileTable
		}
		options.liveInterval = syscallLiveInterval
		options.snapshotInFlight = live
		options.onSnapshot = newSyscallSnapshotHandler(os.Stdout, options.groupBy, snapshotFiles, detector, live)
	}
	collection, err := collectSyscallStats(options, stopTrace, onEvent)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_failed", err))
		return 1
	}
	report := syscallReportFor(collection, options.groupBy)
	if options.files {
		report.Files, report.FilesDropped, report.Summary.Files = fileTable.report(), fileTable.dropped, true
	}
	report.Events = detector.report()
	if options.slow > 0 {
		slow := float64(options.slow) / float64(time.Millisecond)
		report.Summary.SlowMS = &slow
	}
	if options.errorsAll || len(options.errnos) > 0 {
		report.Summary.ErrorFilter = strings.TrimSpace(options.errors)
	}
	// 두 알림은 stderr로 가므로 --json -의 stdout을 바꾸지 않는다. JSON만 받는 사용자도 빠진 호출을 알아야 한다.
	notices := func() {
		if full := report.Summary.StatsFull + report.Summary.StartsFull; full > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_map_full", full))
		}
		if !report.Summary.SelfExcluded {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_self_included"))
		}
	}
	if options.raw {
		notices()
		summary := report.Summary
		summary.Event = "syscall_summary"
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return exitCode()
	}
	if options.jsonPath != "" {
		notices()
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return exitCode()
	}
	if report.Summary.Events > 0 {
		fmt.Fprintln(os.Stdout)
	}
	if live {
		io.WriteString(os.Stdout, syscallLiveClear)
	}
	report.render(os.Stdout, syscallFinalView(options.all))
	notices()
	return exitCode()
}

// 표를 다시 그리는 제어 문자다. 대체 화면은 쓰지 않으므로, 끝난 뒤의 최종 표가 스크롤백에 남는다. 프레임은 화면을 지우지
// 않고 왼쪽 위(Home)부터 덮어쓴다. 줄마다 남은 칸을 지우고(EraseLine), 프레임 아래는 한 번에 지운다(EraseBelow).
// 화면을 먼저 지우면 지운 화면이 잠깐 보여서 깜빡인다.
const (
	syscallLiveHome       = "\x1b[H"
	syscallLiveEraseLine  = "\x1b[K"
	syscallLiveEraseBelow = "\x1b[J"
	syscallLiveClear      = syscallLiveHome + syscallLiveEraseBelow
)

// syscallLiveFrame은 표 한 장을 터미널에 쓸 바이트로 만든다. 한 번의 Write로 내보내면 터미널이 프레임을 중간까지만 그리지 않는다.
func syscallLiveFrame(report syscallReport, view syscallView) []byte {
	var table bytes.Buffer
	report.render(&table, view)
	frame := syscallLiveHome + strings.ReplaceAll(table.String(), "\n", syscallLiveEraseLine+"\n") + syscallLiveEraseBelow
	return []byte(frame)
}

// newSyscallSnapshotHandler는 snapshot마다 event 조건을 보고, live이면 표를 다시 그리는 함수다. 호출 수의 증가분을 지난
// snapshot과 비교해 초당 호출 수를 낸다. files와 detector는 nil일 수 있다.
func newSyscallSnapshotHandler(out *os.File, groupBy string, files *syscallFileTable, detector *syscallEventDetector, live bool) func(syscallCollection) {
	var previous uint64
	var previousMS int64
	return func(collection syscallCollection) {
		var fileRows []syscallFileRow
		if files != nil {
			fileRows = files.report()
		}
		if detector != nil {
			detector.observe(detector.now(), collection, fileRows)
		}
		if !live {
			return
		}
		report := syscallReportFor(collection, groupBy)
		if files != nil {
			report.Files, report.FilesDropped, report.Summary.Files = fileRows, files.dropped, true
		}
		report.Events = detector.report()
		eventRows := 0
		if detector != nil {
			eventRows = max(1, len(report.Events))
		}
		view := syscallLiveView(terminalHeight(out), files != nil, eventRows)
		if elapsed := report.Summary.DurationMS - previousMS; elapsed > 0 && report.Summary.Calls >= previous {
			view.rate = float64(report.Summary.Calls-previous) * 1000 / float64(elapsed)
		}
		previous, previousMS = report.Summary.Calls, report.Summary.DurationMS
		view.styleOut = out
		_, _ = out.Write(syscallLiveFrame(report, view))
	}
}

// syscallLiveView는 화면 높이에 맞게 표의 행 수를 정한다. 위 두 줄과 아래 세 줄(파일 표가 있으면 네 줄)은 고정이고, 마지막
// 줄은 비워 스크롤을 막는다. 통계 표는 숨긴 행 안내로 한 줄을, 진행 중인 호출 표와 파일 표는 빈 줄과 제목과 머리글과 안내로
// 네 줄을 더 쓴다. 남는 행이 16줄 아래이면 진행 중인 호출 표를 빼고, 파일 표는 남는 행의 5분의 2를 받는다.
func syscallLiveView(height int, files bool, events int) syscallView {
	free := height - 1 - 2 - 3 - 1
	view := syscallView{live: true}
	if events > 0 {
		view.events = min(events, syscallLiveEventRows)
		free -= view.events + 2
	}
	if files {
		free -= 1 + 4
	}
	if free >= 16 {
		view.inFlight = min(syscallInFlightRows, max(2, free/6))
		free -= view.inFlight + 4
	}
	if files {
		view.files = min(syscallFileRows, max(3, free*2/5))
		free -= view.files
	}
	view.rows = min(syscallTraceRows, max(1, free))
	return view
}

func terminalHeight(file *os.File) int {
	_, height, err := term.GetSize(int(file.Fd()))
	if err != nil || height <= 0 {
		return syscallLiveHeight
	}
	return height
}

// parseSyscallProcessFilter는 쉼표로 나눈 PID와 command 이름이다. kernel 안에서 비교하므로 이름은 대표 thread의 comm과
// 정확히 같아야 하고, kernel처럼 15 byte에서 자른다.
func parseSyscallProcessFilter(value string) ([]uint32, []string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil, nil
	}
	var pids []uint32
	var comms []string
	for _, term := range strings.Split(value, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return nil, nil, errTopProcessFilterEmpty
		}
		if pid, err := strconv.ParseUint(term, 10, 32); err == nil && pid > 0 {
			pids = append(pids, uint32(pid))
			continue
		}
		if len(term) > syscallCommMax {
			term = term[:syscallCommMax]
		}
		comms = append(comms, term)
	}
	return pids, comms, nil
}

// syscallSelfTGID는 BPF가 보는 edc의 tgid다. BPF는 init pid namespace의 번호를 보므로, edc가 그 namespace에
// 있을 때만 os.Getpid()와 같다. 다른 namespace에서는 0을 돌려주고 edc 자신을 제외하지 않는다.
func syscallSelfTGID(namespace string, pid int) (uint32, bool) {
	if namespace != auditHostPIDNamespace || pid <= 0 {
		return 0, false
	}
	return uint32(pid), true
}

// syscallName은 번호를 이름으로 바꾼다. 32비트 호환 호출은 386 표를 쓴다. 표에 없는 번호는 번호를 그대로 쓴다.
func syscallName(arch string, nr uint32, compat bool) string {
	table := syscallNameTables[arch]
	if compat {
		table = syscallNameTables["386"]
	}
	name := ""
	if int(nr) < len(table) {
		name = table[nr]
	}
	if name == "" {
		name = "syscall_" + strconv.FormatUint(uint64(nr), 10)
	}
	if compat {
		name += " (32-bit)"
	}
	return name
}

// syscallNumber는 표에서 이름의 번호를 찾는다. 없는 이름은 false다.
func syscallNumber(arch, name string) (uint32, bool) {
	for nr, candidate := range syscallNameTables[arch] {
		if candidate == name {
			return uint32(nr), true
		}
	}
	return 0, false
}

func syscallGroupKey(sample syscallSample, groupBy string) string {
	switch groupBy {
	case traceGroupByProcess:
		return sample.Process
	case traceGroupBySyscall:
		return sample.Syscall
	case traceGroupByCgroup:
		return strconv.FormatUint(sample.CgroupID, 10)
	}
	return sample.Process + "\x00" + sample.Syscall
}

func syscallReportFor(collection syscallCollection, groupBy string) syscallReport {
	report := syscallReport{GroupBy: groupBy, Summary: collection.summary, Rows: []syscallReportRow{}, InFlight: []syscallInFlightRow{}}
	report.Summary.StatsEntries = len(collection.samples)
	rows := map[string]*syscallReportRow{}
	for _, sample := range collection.samples {
		report.Summary.Calls += sample.Calls
		report.Summary.Errors += sample.Errors
		key := syscallGroupKey(sample, groupBy)
		row := rows[key]
		if row == nil {
			row = &syscallReportRow{}
			switch groupBy {
			case traceGroupByProcess:
				row.Process = sample.Process
			case traceGroupBySyscall:
				row.Syscall = sample.Syscall
			case traceGroupByCgroup:
				cgroupID := sample.CgroupID
				row.CgroupID = &cgroupID
			default:
				row.Process, row.Syscall = sample.Process, sample.Syscall
			}
			rows[key] = row
		}
		row.Calls += sample.Calls
		row.Errors += sample.Errors
		row.timed += sample.Timed
		row.sumNS += sample.SumNS
		if sample.MaxNS > row.maxNS {
			row.maxNS = sample.MaxNS
		}
		for index, count := range sample.Hist {
			row.hist[index] += count
		}
		for name, count := range sample.Errnos {
			if row.Errnos == nil {
				row.Errnos = map[string]uint64{}
			}
			row.Errnos[name] += count
		}
	}
	for _, row := range rows {
		row.TotalMS = float64(row.sumNS) / float64(time.Millisecond)
		if average, ok := topProbeAverageMS(row.sumNS, row.timed); ok {
			row.AverageMS = &average
			maximum := float64(row.maxNS) / float64(time.Millisecond)
			row.MaxMS = &maximum
		}
		if p95, ok := topProbePercentileMS(row.hist, 0.95); ok {
			row.P95MS = &p95
		}
		report.Rows = append(report.Rows, *row)
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		left, right := report.Rows[i], report.Rows[j]
		if left.sumNS != right.sumNS {
			return left.sumNS > right.sumNS
		}
		if left.Calls != right.Calls {
			return left.Calls > right.Calls
		}
		return left.label() < right.label()
	})
	inFlight := map[string]*syscallInFlightRow{}
	for _, sample := range collection.inFlight {
		report.Summary.InFlight++
		key := sample.Process + "\x00" + sample.Syscall
		row := inFlight[key]
		if row == nil {
			row = &syscallInFlightRow{Process: sample.Process, Syscall: sample.Syscall}
			inFlight[key] = row
		}
		row.Threads++
		if elapsed := float64(sample.ElapsedNS) / float64(time.Millisecond); elapsed > row.LongestMS {
			row.LongestMS = elapsed
		}
	}
	for _, row := range inFlight {
		report.InFlight = append(report.InFlight, *row)
	}
	sort.Slice(report.InFlight, func(i, j int) bool {
		left, right := report.InFlight[i], report.InFlight[j]
		if left.LongestMS != right.LongestMS {
			return left.LongestMS > right.LongestMS
		}
		return left.Process+left.Syscall < right.Process+right.Syscall
	})
	return report
}

func (row syscallReportRow) label() string {
	if row.CgroupID != nil {
		return strconv.FormatUint(*row.CgroupID, 10)
	}
	return row.Process + "\x00" + row.Syscall
}

// errnoText는 오류 수와 가장 많은 errno다. 예: "12 ENOENT".
func (row syscallReportRow) errnoText() string {
	if row.Errors == 0 {
		return "0"
	}
	top, count := "", uint64(0)
	for name, value := range row.Errnos {
		if value > count || value == count && name < top {
			top, count = name, value
		}
	}
	if top == "" {
		return strconv.FormatUint(row.Errors, 10)
	}
	return fmt.Sprintf("%d %s", row.Errors, top)
}

// syscallView는 표가 보이는 방식이다. live는 실행 중에 다시 그리는 표이고, rate는 지난 갱신 뒤 초당 호출 수다.
type syscallView struct {
	events int
	// styleOut은 색과 폭을 정하는 출력이다. 표를 buffer에 그릴 때 실제 터미널을 알려 준다.
	styleOut io.Writer
	rows     int
	inFlight int
	files    int
	live     bool
	rate     float64
}

func (report syscallReport) print(out io.Writer) {
	report.render(out, syscallFinalView(false))
}

// syscallFinalView는 끝난 뒤의 표다. all이면 행 수를 제한하지 않는다.
func syscallFinalView(all bool) syscallView {
	if all {
		return syscallView{rows: math.MaxInt, inFlight: math.MaxInt, files: math.MaxInt, events: math.MaxInt}
	}
	return syscallView{rows: syscallTraceRows, inFlight: syscallInFlightRows, files: syscallFileRows, events: syscallEventRows}
}

func (report syscallReport) render(out io.Writer, view syscallView) {
	styleOut := out
	if view.styleOut != nil {
		styleOut = view.styleOut
	}
	style := newTraceSummaryStyle(styleOut)
	stats := []traceStat{{"duration", syscallTime(float64(report.Summary.DurationMS) * float64(time.Millisecond))}, {"calls", fmt.Sprint(report.Summary.Calls)}, {"errors", fmt.Sprint(report.Summary.Errors)}}
	if view.live {
		stats = append(stats, traceStat{"rate", fmt.Sprintf("%.0f/s", view.rate)})
	}
	style.line(out, style.headline("SYSCALL TRACE", stats...))
	var titles []string
	switch report.GroupBy {
	case traceGroupByProcess:
		titles = []string{"PROCESS"}
	case traceGroupBySyscall:
		titles = []string{"SYSCALL"}
	case traceGroupByCgroup:
		titles = []string{"CGROUP"}
	default:
		titles = []string{"PROCESS", "SYSCALL"}
	}
	shown := report.Rows[:min(len(report.Rows), view.rows)]
	widths := make([]int, len(titles))
	for index, title := range titles {
		widths[index] = len(title)
		for _, row := range shown {
			widths[index] = max(widths[index], len(row.cells(report.GroupBy)[index]))
		}
	}
	errorWidth := len("ERRORS")
	for _, row := range shown {
		errorWidth = max(errorWidth, len(row.errnoText()))
	}
	header := ""
	for index, title := range titles {
		header += fmt.Sprintf("%-*s  ", widths[index], title)
	}
	style.line(out, style.dim(fmt.Sprintf("%s%9s  %-*s %9s %9s %9s %9s", header, "CALLS", errorWidth, "ERRORS", "TOTAL", "AVG", "P95", "MAX")))
	for _, row := range shown {
		line := ""
		for index, cell := range row.cells(report.GroupBy) {
			line += fmt.Sprintf("%-*s  ", widths[index], cell)
		}
		style.printf(out, "%s%9d  %-*s %9s %9s %9s %9s\n", line, row.Calls, errorWidth, style.count("ERRORS", row.errnoText()), style.measure(syscallTime(row.TotalMS*float64(time.Millisecond))), style.measure(syscallMS(row.AverageMS, "")), style.measure(syscallMS(row.P95MS, "<")), style.measure(syscallMS(row.MaxMS, "")))
	}
	if hidden := len(report.Rows) - len(shown); hidden > 0 {
		style.line(out, style.hint(syscallHiddenRows(hidden, view.live)))
	}
	// 실시간 화면은 event가 없어도 패널을 그린다. 감지가 켜져 있는지 알 수 있어야 한다.
	if view.events > 0 && (len(report.Events) > 0 || view.live) {
		report.printEvents(out, style, view.events, view.live)
	}
	if len(report.InFlight) > 0 && view.inFlight > 0 {
		io.WriteString(out, "\n")
		if view.live {
			style.line(out, style.subheading("IN FLIGHT")+style.dim(": calls that started during the trace and have not returned yet"))
		} else {
			style.line(out, style.subheading("IN FLIGHT AT STOP")+style.dim(": calls that started during the trace and had not returned"))
		}
		style.line(out, style.dim(fmt.Sprintf("%-16s  %-22s %8s %9s", "PROCESS", "SYSCALL", "THREADS", "LONGEST")))
		for _, row := range report.InFlight[:min(len(report.InFlight), view.inFlight)] {
			style.printf(out, "%-16s  %-22s %8d %9s\n", row.Process, row.Syscall, row.Threads, style.measure(syscallTime(row.LongestMS*float64(time.Millisecond))))
		}
		if hidden := len(report.InFlight) - view.inFlight; hidden > 0 {
			style.line(out, style.hint(syscallHiddenRows(hidden, view.live)))
		}
	}
	if len(report.Files) > 0 && view.files > 0 {
		report.printFiles(out, style, view.files)
	}
	summary := report.Summary
	unmatched := "-"
	if summary.Unmatched != nil {
		unmatched = strconv.FormatUint(*summary.Unmatched, 10)
	}
	io.WriteString(out, "\n")
	style.line(out, style.stats(traceStat{"in_flight", fmt.Sprint(summary.InFlight)}, traceStat{"unmatched", unmatched}, traceStat{"map_full", fmt.Sprint(summary.StatsFull + summary.StartsFull + summary.ErrnosFull)}, traceStat{"stats", fmt.Sprintf("%d/%d", summary.StatsEntries, summary.StatsLimit)}))
	if summary.SlowMS != nil || summary.ErrorFilter != "" || summary.Files {
		style.line(out, style.stats(traceStat{"events", fmt.Sprint(summary.Events)}, traceStat{"sampled_out", fmt.Sprint(summary.SampledOut)}, traceStat{"ring_lost", fmt.Sprint(summary.RingLost)}))
	}
	if view.live {
		style.line(out, style.hint("Ctrl-C stops and prints the final table"))
	}
}

// syscallHiddenRows는 표에 다 들지 않은 행과 모두 보는 방법을 알린다. 실행 중의 표는 화면 높이에 맞춰 줄이므로
// 끝난 뒤의 표에서 --all을 쓰라고 알린다.
func syscallHiddenRows(hidden int, live bool) string {
	if live {
		return fmt.Sprintf("... %d more rows. After Ctrl-C, --all shows every row", hidden)
	}
	return fmt.Sprintf("... %d more rows. Add --all to show every row, or use --json", hidden)
}

func (row syscallReportRow) cells(groupBy string) []string {
	switch groupBy {
	case traceGroupByProcess:
		return []string{row.Process}
	case traceGroupBySyscall:
		return []string{row.Syscall}
	case traceGroupByCgroup:
		return []string{row.label()}
	}
	return []string{row.Process, row.Syscall}
}

func syscallMS(value *float64, prefix string) string {
	if value == nil {
		return "-"
	}
	return prefix + syscallTime(*value*float64(time.Millisecond))
}

// syscallTime은 nanosecond를 크기에 맞는 단위로 쓴다. syscall은 ns부터 초까지 걸리므로 ms 한 단위로는 짧은 호출이 0으로 보인다.
func syscallTime(ns float64) string {
	switch {
	case ns < float64(time.Microsecond):
		return fmt.Sprintf("%.0fns", ns)
	case ns < float64(time.Millisecond):
		return fmt.Sprintf("%.1fus", ns/float64(time.Microsecond))
	case ns < float64(time.Second):
		return fmt.Sprintf("%.2fms", ns/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", ns/float64(time.Second))
}
