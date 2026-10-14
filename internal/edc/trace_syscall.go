package edc

//go:generate go run trace_syscall_names_gen.go

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	traceSyscallUsage   = "edc trace syscall [options]"
	traceGroupBySyscall = "syscall"
	// syscallTraceRows와 syscallInFlightRows는 표에 보이는 행 수다. --json에는 모든 행이 들어간다.
	syscallTraceRows    = 20
	syscallInFlightRows = 10
	// syscallCommMax는 kernel comm의 길이다. 16 byte 중 마지막은 NUL이라 이름은 15 byte까지 남는다.
	syscallCommMax = 15
)

type syscallTraceOptions struct {
	duration     time.Duration
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	groupBy      string
	jsonPath     string
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
	Summary  syscallSummary       `json:"summary"`
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
	set := flag.NewFlagSet("trace syscall", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.groupBy, "group-by", "", "process, syscall, or cgroup")
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
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
	if err := syscallTracePrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if options.containerRef != "" {
		container, code, err := resolveTraceContainer(options.containerRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return code
		}
		options.container = container
	}
	// 화면 없이 끝날 때 한 번 출력하므로, 터미널에서는 무엇을 기다리는지 알린다.
	if traceIsTerminal(os.Stderr) {
		if options.duration > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_running_for", options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_running"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	collection, err := collectSyscallStats(options, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_failed", err))
		return 1
	}
	report := syscallReportFor(collection, options.groupBy)
	// 두 알림은 stderr로 가므로 --json -의 stdout을 바꾸지 않는다. JSON만 받는 사용자도 빠진 호출을 알아야 한다.
	notices := func() {
		if full := report.Summary.StatsFull + report.Summary.StartsFull; full > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_map_full", full))
		}
		if !report.Summary.SelfExcluded {
			fmt.Fprintln(os.Stderr, T("cli.trace.syscall_self_included"))
		}
	}
	if options.jsonPath != "" {
		notices()
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	report.print(os.Stdout)
	notices()
	return 0
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

func (report syscallReport) print(out io.Writer) {
	fmt.Fprintf(out, "SYSCALL TRACE  duration %s  calls %d  errors %d\n", syscallTime(float64(report.Summary.DurationMS)*float64(time.Millisecond)), report.Summary.Calls, report.Summary.Errors)
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
	shown := report.Rows[:min(len(report.Rows), syscallTraceRows)]
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
	fmt.Fprintf(out, "%s%9s  %-*s %9s %9s %9s %9s\n", header, "CALLS", errorWidth, "ERRORS", "TOTAL", "AVG", "P95", "MAX")
	for _, row := range shown {
		line := ""
		for index, cell := range row.cells(report.GroupBy) {
			line += fmt.Sprintf("%-*s  ", widths[index], cell)
		}
		fmt.Fprintf(out, "%s%9d  %-*s %9s %9s %9s %9s\n", line, row.Calls, errorWidth, row.errnoText(), syscallTime(row.TotalMS*float64(time.Millisecond)), syscallMS(row.AverageMS, ""), syscallMS(row.P95MS, "<"), syscallMS(row.MaxMS, ""))
	}
	if hidden := len(report.Rows) - len(shown); hidden > 0 {
		fmt.Fprintf(out, "... %d more rows in --json\n", hidden)
	}
	if len(report.InFlight) > 0 {
		fmt.Fprintf(out, "\nIN FLIGHT AT STOP: calls that started during the trace and had not returned\n")
		fmt.Fprintf(out, "%-16s  %-22s %8s %9s\n", "PROCESS", "SYSCALL", "THREADS", "LONGEST")
		for _, row := range report.InFlight[:min(len(report.InFlight), syscallInFlightRows)] {
			fmt.Fprintf(out, "%-16s  %-22s %8d %9s\n", row.Process, row.Syscall, row.Threads, syscallTime(row.LongestMS*float64(time.Millisecond)))
		}
		if hidden := len(report.InFlight) - syscallInFlightRows; hidden > 0 {
			fmt.Fprintf(out, "... %d more rows in --json\n", hidden)
		}
	}
	summary := report.Summary
	unmatched := "-"
	if summary.Unmatched != nil {
		unmatched = strconv.FormatUint(*summary.Unmatched, 10)
	}
	fmt.Fprintf(out, "\nin_flight %d  unmatched %s  map_full %d  stats %d/%d\n", summary.InFlight, unmatched, summary.StatsFull+summary.StartsFull+summary.ErrnosFull, summary.StatsEntries, summary.StatsLimit)
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
