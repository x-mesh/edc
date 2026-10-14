package edc

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	traceLockUsage = "edc trace lock [--kind kernel|futex] [--process <list>] [--duration 10s]"
	lockKindKernel = "kernel"
	lockKindFutex  = "futex"
	lockTraceRows  = 20
)

// lockFlags는 lock:contention_begin의 flags다(include/trace/events/lock.h의 LCB_F_*).
const (
	lockFlagSpin   = 1 << 0
	lockFlagRead   = 1 << 1
	lockFlagWrite  = 1 << 2
	lockFlagRT     = 1 << 3
	lockFlagPerCPU = 1 << 4
	lockFlagMutex  = 1 << 5
)

// lockPrimitivePrefixes는 kernel stack에서 lock을 잡는 함수와, tracepoint에서 BPF를 부르는 함수의 접두사다. 이 함수들을
// 건너뛴 첫 frame이 lock을 잡으려던 함수다.
var lockPrimitivePrefixes = []string{
	"bpf_prog_", "bpf_trace_run", "__bpf_trace_", "__traceiter_",
	"_raw_spin_lock", "_raw_read_lock", "_raw_write_lock", "__raw_spin_lock", "__raw_read_lock", "__raw_write_lock", "do_raw_spin_lock", "queued_spin_lock_slowpath", "native_queued_spin_lock_slowpath",
	"queued_read_lock_slowpath", "queued_write_lock_slowpath", "__pv_queued_spin_lock_slowpath",
	"__mutex_lock", "mutex_lock", "__mutex_lock_slowpath", "mutex_lock_interruptible", "mutex_lock_killable", "mutex_lock_io",
	"rwsem_down_", "down_read", "down_write", "__down_read", "__down_write", "rt_mutex", "__rt_mutex", "rt_spin_lock",
	"percpu_down_", "__percpu_down_", "lock_sock", "__lock_sock", "lock_contended",
}

type lockTraceOptions struct {
	duration     time.Duration
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	jsonPath     string
	kind         string
}

// lockSample은 BPF map의 항목 하나다. Frames는 가장 안쪽 함수가 먼저 온다. kernel lock이면 kernel stack, futex이면 사용자
// stack이다. TGID는 futex에만 있다.
type lockSample struct {
	Kind     string
	Lock     uint64
	LockName string
	Flags    uint32
	Process  string
	TGID     uint32
	CgroupID uint64
	Frames   []string
	Count    uint64
	SumNS    uint64
	MaxNS    uint64
	Hist     topProbeHist
}

type lockSummary struct {
	DurationMS   int64   `json:"duration_ms"`
	Waits        uint64  `json:"waits"`
	TotalMS      float64 `json:"total_ms"`
	StatsEntries int     `json:"stats_entries"`
	StatsFull    uint64  `json:"stats_full"`
	// DepthFull은 한 task 안에서 네 번보다 깊게 겹친 contention이다. Unmatched는 시작을 보지 못한 끝이다.
	DepthFull     uint64            `json:"depth_full"`
	Unmatched     uint64            `json:"unmatched"`
	StackLost     uint64            `json:"stack_lost"`
	FreshLost     uint64            `json:"fresh_lost"`
	SelfExcluded  bool              `json:"self_excluded"`
	KernelSymbols bool              `json:"kernel_symbols"`
	Unavailable   map[string]string `json:"unavailable,omitempty"`
}

type lockCollection struct {
	samples []lockSample
	summary lockSummary
}

// lockReportRow는 kernel lock이면 (lock, 종류, 부른 함수), futex이면 (process, 주소)로 묶은 행이다. Caller는 시간이 가장
// 많은 stack에서 고른다.
type lockReportRow struct {
	Kind      string            `json:"kind"`
	Lock      string            `json:"lock"`
	Type      string            `json:"type,omitempty"`
	Caller    string            `json:"caller"`
	Process   string            `json:"process,omitempty"`
	PID       uint32            `json:"pid,omitempty"`
	Processes map[string]uint64 `json:"processes,omitempty"`
	Waits     uint64            `json:"waits"`
	TotalMS   float64           `json:"total_ms"`
	AverageMS float64           `json:"avg_ms"`
	P95MS     *float64          `json:"p95_ms,omitempty"`
	MaxMS     float64           `json:"max_ms"`
	hist      topProbeHist
	callers   map[string]uint64
}

type lockReport struct {
	Kernel  []lockReportRow `json:"kernel"`
	Futex   []lockReportRow `json:"futex"`
	Summary lockSummary     `json:"summary"`
}

func runTraceLock(args []string) int {
	options := lockTraceOptions{}
	var process string
	set := flag.NewFlagSet("trace lock", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.StringVar(&options.kind, "kind", "", T("command.trace.option.lock_kind"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace lock"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	switch options.kind {
	case "", lockKindKernel, lockKindFutex:
	default:
		fmt.Fprintln(os.Stderr, T("cli.trace.lock_kind"))
		return 2
	}
	pids, comms, err := parseSyscallProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_process", err))
		return 2
	}
	options.pids, options.comms = pids, comms
	if err := lockTracePrerequisites(); err != nil {
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
	if traceIsTerminal(os.Stderr) {
		if options.duration > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.lock_running_for", options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.lock_running"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	collection, err := collectLockStats(options, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.lock_failed", err))
		return 1
	}
	report := lockReportFor(collection)
	if options.jsonPath != "" {
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		report.print(os.Stdout)
	}
	lockNotices(os.Stderr, report.Summary)
	return 0
}

// lockNotices는 stderr로 가므로 --json -의 stdout을 바꾸지 않는다.
func lockNotices(out io.Writer, summary lockSummary) {
	kinds := make([]string, 0, len(summary.Unavailable))
	for kind := range summary.Unavailable {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintln(out, T("cli.trace.lock_unavailable", kind, summary.Unavailable[kind]))
	}
	if summary.StatsFull > 0 {
		fmt.Fprintln(out, T("cli.trace.lock_map_full", summary.StatsFull))
	}
	if !summary.KernelSymbols {
		fmt.Fprintln(out, T("cli.trace.cpu_no_kallsyms"))
	}
	if !summary.SelfExcluded {
		fmt.Fprintln(out, T("cli.trace.cpu_self_included"))
	}
}

// lockTypeName은 perf lock contention이 쓰는 이름이다.
func lockTypeName(flags uint32) string {
	switch {
	case flags&lockFlagPerCPU != 0 && flags&lockFlagWrite != 0:
		return "pcpu-sem:W"
	case flags&lockFlagPerCPU != 0:
		return "pcpu-sem:R"
	case flags&lockFlagRT != 0:
		return "rtmutex"
	case flags&lockFlagMutex != 0:
		return "mutex"
	case flags&lockFlagSpin != 0 && flags&lockFlagWrite != 0:
		return "rwlock:W"
	case flags&lockFlagSpin != 0 && flags&lockFlagRead != 0:
		return "rwlock:R"
	case flags&lockFlagSpin != 0:
		return "spinlock"
	case flags&lockFlagWrite != 0:
		return "rwsem:W"
	case flags&lockFlagRead != 0:
		return "rwsem:R"
	}
	return fmt.Sprintf("flags:0x%x", flags)
}

// lockKernelCaller는 lock 함수와 tracepoint를 건너뛴 첫 kernel frame이다.
func lockKernelCaller(frames []string) string {
	for _, frame := range frames {
		primitive := false
		for _, prefix := range lockPrimitivePrefixes {
			if strings.HasPrefix(frame, prefix) {
				primitive = true
				break
			}
		}
		if !primitive {
			return frame
		}
	}
	return "-"
}

// lockUserCaller는 사용자 stack의 안쪽 두 frame이다. 가장 안쪽은 흔히 libc나 runtime의 futex wrapper다.
func lockUserCaller(frames []string) string {
	switch len(frames) {
	case 0:
		return "-"
	case 1:
		return frames[0]
	}
	return frames[0] + " ← " + frames[1]
}

func lockLabel(sample lockSample) string {
	switch {
	case sample.LockName == "":
		return fmt.Sprintf("0x%x", sample.Lock)
	case strings.HasPrefix(sample.LockName, "["):
		return fmt.Sprintf("%s 0x%x", sample.LockName, sample.Lock)
	}
	return sample.LockName
}

func lockReportFor(collection lockCollection) lockReport {
	report := lockReport{Summary: collection.summary, Kernel: []lockReportRow{}, Futex: []lockReportRow{}}
	report.Summary.StatsEntries = len(collection.samples)
	rows := map[string]*lockReportRow{}
	var order []string
	var totalNS uint64
	maxNS, sumNS := map[string]uint64{}, map[string]uint64{}
	for _, sample := range collection.samples {
		report.Summary.Waits += sample.Count
		totalNS += sample.SumNS
		row := lockReportRow{Kind: sample.Kind, Lock: lockLabel(sample)}
		caller := ""
		var key string
		if sample.Kind == lockKindKernel {
			row.Type = lockTypeName(sample.Flags)
			caller = lockKernelCaller(sample.Frames)
			key = strings.Join([]string{sample.Kind, row.Lock, row.Type, caller}, "\x00")
		} else {
			row.Process, row.PID = sample.Process, sample.TGID
			caller = lockUserCaller(sample.Frames)
			key = strings.Join([]string{sample.Kind, fmt.Sprint(sample.TGID), sample.Process, row.Lock}, "\x00")
		}
		existing := rows[key]
		if existing == nil {
			row.callers, row.Processes = map[string]uint64{}, map[string]uint64{}
			existing = &row
			rows[key] = existing
			order = append(order, key)
		}
		existing.Waits += sample.Count
		sumNS[key] += sample.SumNS
		if sample.MaxNS > maxNS[key] {
			maxNS[key] = sample.MaxNS
		}
		existing.callers[caller] += sample.SumNS
		if sample.Kind == lockKindKernel {
			existing.Processes[emptyAs(sample.Process, "-")] += sample.Count
		}
		for index, count := range sample.Hist {
			existing.hist[index] += count
		}
	}
	report.Summary.TotalMS = float64(totalNS) / float64(time.Millisecond)
	for _, key := range order {
		row := rows[key]
		row.TotalMS = float64(sumNS[key]) / float64(time.Millisecond)
		row.MaxMS = float64(maxNS[key]) / float64(time.Millisecond)
		if row.Waits > 0 {
			row.AverageMS = row.TotalMS / float64(row.Waits)
		}
		if p95, ok := topProbePercentileMS(row.hist, 0.95); ok {
			row.P95MS = &p95
		}
		var best uint64
		for caller, ns := range row.callers {
			if row.Caller == "" || ns > best || (ns == best && caller < row.Caller) {
				row.Caller, best = caller, ns
			}
		}
		if len(row.Processes) == 0 {
			row.Processes = nil
		}
		if row.Kind == lockKindKernel {
			report.Kernel = append(report.Kernel, *row)
		} else {
			report.Futex = append(report.Futex, *row)
		}
	}
	for _, rows := range [][]lockReportRow{report.Kernel, report.Futex} {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].TotalMS != rows[j].TotalMS {
				return rows[i].TotalMS > rows[j].TotalMS
			}
			return rows[i].Lock+rows[i].Caller < rows[j].Lock+rows[j].Caller
		})
	}
	return report
}

// lockTopProcess는 kernel lock을 가장 많이 기다린 process 이름이다.
func lockTopProcess(processes map[string]uint64) string {
	best, name := uint64(0), "-"
	for candidate, count := range processes {
		if count > best || (count == best && candidate < name) {
			best, name = count, candidate
		}
	}
	return name
}

func (report lockReport) print(out io.Writer) {
	summary := report.Summary
	fmt.Fprintf(out, "LOCK TRACE  duration %s  waits %d  waited %s\n", syscallTime(float64(summary.DurationMS)*float64(time.Millisecond)), summary.Waits, syscallTime(summary.TotalMS*float64(time.Millisecond)))
	if _, missing := summary.Unavailable[lockKindKernel]; !missing {
		fmt.Fprintf(out, "%-28s %-10s %-28s %-16s %8s %9s %9s %9s\n", "KERNEL LOCK", "TYPE", "CALLER", "PROCESS", "WAITS", "TOTAL", "AVG", "MAX")
		for index, row := range report.Kernel {
			if index == lockTraceRows {
				fmt.Fprintf(out, "... %d more rows in --json\n", len(report.Kernel)-lockTraceRows)
				break
			}
			fmt.Fprintf(out, "%-28s %-10s %-28s %-16s %8d %9s %9s %9s\n", cpuShorten(row.Lock, 28), row.Type, cpuShorten(row.Caller, 28), cpuShorten(lockTopProcess(row.Processes), 16), row.Waits, syscallTime(row.TotalMS*float64(time.Millisecond)), syscallTime(row.AverageMS*float64(time.Millisecond)), syscallTime(row.MaxMS*float64(time.Millisecond)))
		}
	}
	if _, missing := summary.Unavailable[lockKindFutex]; !missing {
		fmt.Fprintf(out, "%-16s %8s %-28s %-36s %8s %9s %9s %9s\n", "PROCESS", "PID", "FUTEX", "CODE", "WAITS", "TOTAL", "AVG", "MAX")
		for index, row := range report.Futex {
			if index == lockTraceRows {
				fmt.Fprintf(out, "... %d more rows in --json\n", len(report.Futex)-lockTraceRows)
				break
			}
			fmt.Fprintf(out, "%-16s %8d %-28s %-36s %8d %9s %9s %9s\n", cpuShorten(row.Process, 16), row.PID, cpuShorten(row.Lock, 28), cpuShorten(row.Caller, 36), row.Waits, syscallTime(row.TotalMS*float64(time.Millisecond)), syscallTime(row.AverageMS*float64(time.Millisecond)), syscallTime(row.MaxMS*float64(time.Millisecond)))
		}
	}
	fmt.Fprintf(out, "entries %d  stats_full %d  depth_full %d  unmatched %d  stack_lost %d\n", summary.StatsEntries, summary.StatsFull, summary.DepthFull, summary.Unmatched, summary.StackLost)
}
