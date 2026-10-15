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
	traceCPUUsage = "edc trace cpu [--frequency N | --off-cpu] [--process <list>] [--folded <path>]"
	// cpuDefaultFrequency는 99Hz다. 100Hz처럼 timer tick과 같은 주기로 재면 tick에 맞춰 도는 일만 보이거나 빠진다.
	cpuDefaultFrequency = 99
	cpuMaxFrequency     = 1000
	cpuProcessRows      = 10
	cpuFunctionRows     = 20
	// cpuKernelSuffix는 flame graph 도구가 kernel frame에 붙이는 표시다.
	cpuKernelSuffix = "_[k]"
	cpuWaitRows     = 20
)

// cpuSyscallPrefixes는 syscall마다 있는 kernel 진입 함수의 접두사다. off-CPU stack에서 이 frame이 잠든 syscall의 이름을 준다.
var cpuSyscallPrefixes = []string{"__x64_sys_", "__ia32_sys_", "__x32_compat_sys_", "__ia32_compat_sys_", "__arm64_sys_", "__arm64_compat_sys_", "__se_sys_", "__do_sys_", "ksys_"}

// cpuFaultFrames는 page fault 처리 함수다. syscall이 아니라 fault에서 잠든 stack에 있다.
var cpuFaultFrames = map[string]bool{"exc_page_fault": true, "do_user_addr_fault": true, "do_page_fault": true, "do_mem_abort": true, "el0_da": true, "el0_ia": true}

type cpuTraceOptions struct {
	duration     time.Duration
	frequency    int
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	jsonPath     string
	foldedPath   string
	offCPU       bool
}

// cpuStack은 (process, 사용자 stack, kernel stack) 하나와 그 샘플 수다. frame은 가장 안쪽 함수가 먼저 온다.
type cpuStack struct {
	TGID     uint32
	Process  string
	CgroupID uint64
	User     []string
	Kernel   []string
	// Samples는 on-CPU에서 샘플 수이고, off-CPU에서 잠든 횟수다. OffCPUNS는 off-CPU에서 잠든 시간의 합이다.
	Samples  uint64
	OffCPUNS uint64
}

// cpuSummary의 Samples는 CPU마다 perf event가 program을 부른 수다. Idle, Self, Filtered를 뺀 나머지가 Profiled이고,
// 표의 비율은 Profiled에 대한 값이다.
type cpuSummary struct {
	DurationMS int64 `json:"duration_ms"`
	// FrequencyHz와 BusyPercent는 on-CPU 샘플링에만 있다. --off-cpu는 샘플링하지 않으므로 두 칸을 내지 않는다.
	FrequencyHz          int      `json:"frequency_hz,omitempty"`
	CPUs                 int      `json:"cpus"`
	Samples              uint64   `json:"samples"`
	Idle                 uint64   `json:"idle"`
	BusyPercent          *float64 `json:"busy_percent,omitempty"`
	Self                 uint64   `json:"self"`
	SelfExcluded         bool     `json:"self_excluded"`
	Filtered             uint64   `json:"filtered"`
	Profiled             uint64   `json:"profiled"`
	Stacks               int      `json:"stacks"`
	CountsFull           uint64   `json:"counts_full"`
	StacksFull           uint64   `json:"stacks_full"`
	UserStackMissing     uint64   `json:"user_stack_missing"`
	UserStackCollision   uint64   `json:"user_stack_collision"`
	KernelStackEmpty     uint64   `json:"kernel_stack_empty"`
	KernelStackCollision uint64   `json:"kernel_stack_collision"`
	// ShallowUserSamples는 사용자 stack이 frame 하나뿐인 샘플이다. frame pointer 없이 build한 program에서 많다.
	ShallowUserSamples uint64 `json:"shallow_user_samples"`
	KernelSymbols      bool   `json:"kernel_symbols"`
	// FreshLost는 새 process 알림을 ring에 넣지 못한 수다. 그 process는 다음 1초 훑기에서 읽고, 그 전에 끝났으면 이름이 빈다.
	FreshLost uint64 `json:"fresh_lost"`
	// OffCPU는 --off-cpu로 잰 결과다. 그때 Profiled는 잠든 횟수이고 OffCPUMS는 잠든 시간의 합이다.
	OffCPU bool `json:"off_cpu,omitempty"`
	// OffCPUMS는 --off-cpu에서만 있다. 잠든 시간이 0이어도 칸을 낸다.
	OffCPUMS         *float64 `json:"off_cpu_ms,omitempty"`
	OffCPUStartsFull uint64   `json:"off_cpu_starts_full,omitempty"`
}

// cpuProcessRow의 Samples는 off-CPU에서 잠든 횟수이고, 그때 Percent는 잠든 시간의 비율이다.
type cpuProcessRow struct {
	Process string   `json:"process"`
	PID     uint32   `json:"pid"`
	Samples uint64   `json:"samples"`
	Percent float64  `json:"percent"`
	TimeMS  *float64 `json:"time_ms,omitempty"`
}

// cpuWaitRow는 off-CPU에서 잠든 자리 하나다. Code는 사용자 stack의 안쪽 두 frame이고, Kernel은 잠든 syscall 이름이나
// page_fault, 둘 다 아니면 가장 바깥 kernel frame이다.
type cpuWaitRow struct {
	Process string  `json:"process"`
	Code    string  `json:"code"`
	Kernel  string  `json:"kernel"`
	Blocks  uint64  `json:"blocks"`
	TimeMS  float64 `json:"time_ms"`
	AvgMS   float64 `json:"avg_ms"`
	Percent float64 `json:"percent"`
}

// cpuFunctionRow의 Self는 그 함수가 가장 안쪽에 있던 샘플이고, Total은 stack 어디에든 있던 샘플이다.
type cpuFunctionRow struct {
	Function     string  `json:"function"`
	Kernel       bool    `json:"kernel"`
	Self         uint64  `json:"self"`
	SelfPercent  float64 `json:"self_percent"`
	Total        uint64  `json:"total"`
	TotalPercent float64 `json:"total_percent"`
}

type cpuReport struct {
	Processes []cpuProcessRow  `json:"processes"`
	Functions []cpuFunctionRow `json:"functions"`
	Waits     []cpuWaitRow     `json:"waits,omitempty"`
	Summary   cpuSummary       `json:"summary"`
}

func runTraceCPU(args []string) int {
	options := cpuTraceOptions{}
	var process string
	set := newFlagSet("trace cpu")
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.IntVar(&options.frequency, "frequency", cpuDefaultFrequency, T("command.trace.option.frequency"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.StringVar(&options.foldedPath, "folded", "", T("command.trace.option.folded"))
	set.BoolVar(&options.offCPU, "off-cpu", false, T("command.trace.option.off_cpu"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace cpu"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	if options.frequency < 1 || options.frequency > cpuMaxFrequency {
		fmt.Fprintln(os.Stderr, T("cli.trace.cpu_frequency", cpuMaxFrequency))
		return 2
	}
	if options.foldedPath == "-" && options.jsonPath == "-" {
		fmt.Fprintln(os.Stderr, T("cli.trace.cpu_folded_json"))
		return 2
	}
	pids, comms, err := parseSyscallProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_process", err))
		return 2
	}
	options.pids, options.comms = pids, comms
	frequencySet := false
	set.Visit(func(flag *flag.Flag) { frequencySet = frequencySet || flag.Name == "frequency" })
	if options.offCPU && frequencySet {
		fmt.Fprintln(os.Stderr, T("cli.trace.cpu_off_frequency"))
		return 2
	}
	prerequisites := cpuTracePrerequisites
	if options.offCPU {
		prerequisites = cpuOffCPUPrerequisites
	}
	if err := prerequisites(); err != nil {
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
	if traceIsTerminal(os.Stderr) && options.offCPU {
		fmt.Fprintln(os.Stderr, T("cli.trace.cpu_off_running"))
	} else if traceIsTerminal(os.Stderr) {
		if options.duration > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.cpu_running_for", options.frequency, options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.cpu_running", options.frequency))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stacks, summary, err := collectCPUStacks(options, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.cpu_failed", err))
		return 1
	}
	report := cpuReportFor(stacks, summary)
	if options.foldedPath != "" {
		if err := writeCPUFolded(options.foldedPath, stacks, options.offCPU); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	// --folded -가 stdout을 쓰면 표는 출력하지 않는다. folded 줄 사이에 표가 섞이면 flame graph 도구가 읽지 못한다.
	switch {
	case options.jsonPath != "":
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	case options.foldedPath != "-":
		report.print(os.Stdout)
	}
	cpuNotices(os.Stderr, report.Summary)
	return 0
}

// cpuNotices는 stderr로 가므로 --json -의 stdout을 바꾸지 않는다. 결과를 덜 믿어야 하는 이유만 알린다.
func cpuNotices(out io.Writer, summary cpuSummary) {
	if lost := summary.CountsFull + summary.StacksFull + summary.OffCPUStartsFull; lost > 0 {
		fmt.Fprintln(out, T("cli.trace.cpu_map_full", lost))
	}
	if collisions := summary.UserStackCollision + summary.KernelStackCollision; collisions > 0 {
		fmt.Fprintln(out, T("cli.trace.cpu_collisions", collisions))
	}
	if !summary.KernelSymbols {
		fmt.Fprintln(out, T("cli.trace.cpu_no_kallsyms"))
	}
	// 절반이 넘으면 우연이 아니다. frame pointer 없는 program의 사용자 stack은 가장 안쪽 함수 하나만 남는다.
	if summary.Profiled > 0 && summary.ShallowUserSamples*2 > summary.Profiled {
		fmt.Fprintln(out, T("cli.trace.cpu_shallow", cpuPercent(summary.ShallowUserSamples, summary.Profiled)))
	}
	if !summary.SelfExcluded {
		fmt.Fprintln(out, T("cli.trace.cpu_self_included"))
	}
}

func cpuPercent(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) * 100 / float64(whole)
}

func cpuKernelName(name string) string { return name + cpuKernelSuffix }

// cpuBusyPercent는 idle이 아닌 샘플을 그 시간에 받았어야 할 샘플 수와 견준다. 전체 샘플을 분모로 쓰지 않는 것은 쉬는 CPU의
// 샘플이 빠지는 VM이 있기 때문이다. GitHub runner에서는 쉬는 CPU가 받아야 할 샘플의 약 57%만 받았고, 바쁜 CPU는 모두
// 받았다. 전체 샘플을 분모로 쓰면 그만큼 바쁜 비율이 커진다.
func cpuBusyPercent(busy uint64, frequency int, elapsed time.Duration, cpus int) float64 {
	expected := float64(frequency) * elapsed.Seconds() * float64(cpus)
	if expected <= 0 {
		return 0
	}
	return min(float64(busy)*100/expected, 100)
}

func cpuReportFor(stacks []cpuStack, summary cpuSummary) cpuReport {
	report := cpuReport{Summary: summary, Processes: []cpuProcessRow{}, Functions: []cpuFunctionRow{}}
	report.Summary.Stacks = len(stacks)
	type processKey struct {
		pid  uint32
		name string
	}
	processes := map[processKey]uint64{}
	functions := map[string]*cpuFunctionRow{}
	function := func(name string, kernel bool) *cpuFunctionRow {
		key := name
		if kernel {
			key = cpuKernelName(name)
		}
		row := functions[key]
		if row == nil {
			row = &cpuFunctionRow{Function: name, Kernel: kernel}
			functions[key] = row
		}
		return row
	}
	// off-CPU에서는 횟수 대신 잠든 시간이 무게다. 표의 비율도 시간의 비율이다.
	weight := func(stack cpuStack) uint64 {
		if summary.OffCPU {
			return stack.OffCPUNS
		}
		return stack.Samples
	}
	var profiled, total uint64
	blocks := map[processKey]uint64{}
	waits := map[[3]string]*cpuWaitRow{}
	for _, stack := range stacks {
		profiled += stack.Samples
		total += weight(stack)
		processes[processKey{stack.TGID, stack.Process}] += weight(stack)
		blocks[processKey{stack.TGID, stack.Process}] += stack.Samples
		if len(stack.User) == 1 {
			report.Summary.ShallowUserSamples += stack.Samples
		}
		if summary.OffCPU {
			key := [3]string{emptyAs(stack.Process, "-"), cpuWaitCode(stack.User), cpuWaitKernel(stack.Kernel)}
			row := waits[key]
			if row == nil {
				row = &cpuWaitRow{Process: key[0], Code: key[1], Kernel: key[2]}
				waits[key] = row
			}
			row.Blocks += stack.Samples
			row.TimeMS += float64(stack.OffCPUNS) / float64(time.Millisecond)
		}
		// kernel frame이 있으면 그 샘플은 kernel 안에서 멈춘 것이다. 가장 안쪽 frame이 self다.
		switch {
		case len(stack.Kernel) > 0:
			function(stack.Kernel[0], true).Self += weight(stack)
		case len(stack.User) > 0:
			function(stack.User[0], false).Self += weight(stack)
		}
		// 재귀 함수는 한 stack에 여러 번 나오지만 Total에는 한 번만 센다.
		seen := map[string]bool{}
		for _, frames := range []struct {
			names  []string
			kernel bool
		}{{stack.User, false}, {stack.Kernel, true}} {
			for _, name := range frames.names {
				key := name
				if frames.kernel {
					key = cpuKernelName(name)
				}
				if !seen[key] {
					seen[key] = true
					function(name, frames.kernel).Total += weight(stack)
				}
			}
		}
	}
	report.Summary.Profiled = profiled
	for key, value := range processes {
		row := cpuProcessRow{Process: key.name, PID: key.pid, Samples: blocks[key], Percent: cpuPercent(value, total)}
		if summary.OffCPU {
			ms := float64(value) / float64(time.Millisecond)
			row.TimeMS = &ms
		}
		report.Processes = append(report.Processes, row)
	}
	if summary.OffCPU {
		ms := float64(total) / float64(time.Millisecond)
		report.Summary.OffCPUMS = &ms
		for _, row := range waits {
			if row.Blocks > 0 {
				row.AvgMS = row.TimeMS / float64(row.Blocks)
			}
			row.Percent = cpuPercent(uint64(row.TimeMS*float64(time.Millisecond)), total)
			report.Waits = append(report.Waits, *row)
		}
		sort.Slice(report.Waits, func(i, j int) bool {
			left, right := report.Waits[i], report.Waits[j]
			if left.TimeMS != right.TimeMS {
				return left.TimeMS > right.TimeMS
			}
			return left.Process+left.Code+left.Kernel < right.Process+right.Code+right.Kernel
		})
	}
	sort.Slice(report.Processes, func(i, j int) bool {
		left, right := report.Processes[i], report.Processes[j]
		if left.Percent != right.Percent {
			return left.Percent > right.Percent
		}
		if left.Samples != right.Samples {
			return left.Samples > right.Samples
		}
		return left.PID < right.PID
	})
	for _, row := range functions {
		row.SelfPercent, row.TotalPercent = cpuPercent(row.Self, total), cpuPercent(row.Total, total)
		report.Functions = append(report.Functions, *row)
	}
	sort.Slice(report.Functions, func(i, j int) bool {
		left, right := report.Functions[i], report.Functions[j]
		if left.Self != right.Self {
			return left.Self > right.Self
		}
		if left.Total != right.Total {
			return left.Total > right.Total
		}
		return left.Function < right.Function
	})
	return report
}

// cpuWaitCode는 사용자 stack의 안쪽 두 frame이다. 가장 안쪽은 흔히 libc나 runtime의 wrapper라서 부른 함수까지 보여 준다.
func cpuWaitCode(user []string) string {
	switch len(user) {
	case 0:
		return "-"
	case 1:
		return user[0]
	}
	return user[0] + " ← " + user[1]
}

// cpuWaitKernel은 바깥쪽부터 kernel frame을 보아 syscall 진입 함수의 이름을 고른다. syscall이 아니면 page fault인지 보고,
// 그것도 아니면 가장 바깥 frame을 쓴다.
func cpuWaitKernel(kernel []string) string {
	for index := len(kernel) - 1; index >= 0; index-- {
		for _, prefix := range cpuSyscallPrefixes {
			if name, found := strings.CutPrefix(kernel[index], prefix); found {
				return name
			}
		}
	}
	for _, frame := range kernel {
		if cpuFaultFrames[frame] {
			return "page_fault"
		}
	}
	if len(kernel) > 0 {
		return kernel[len(kernel)-1]
	}
	return "-"
}

func (report cpuReport) print(out io.Writer) {
	if report.Summary.OffCPU {
		report.printOffCPU(out)
		return
	}
	style := newTraceSummaryStyle(out)
	summary := report.Summary
	style.line(out, style.headline("CPU PROFILE", traceStat{"duration", (time.Duration(summary.DurationMS) * time.Millisecond).String()}, traceStat{"", fmt.Sprintf("%d Hz", summary.FrequencyHz)}, traceStat{"", fmt.Sprintf("%d CPUs", summary.CPUs)},
		traceStat{"samples", fmt.Sprint(summary.Samples)}, traceStat{"busy", fmt.Sprintf("%.1f%%", cpuOptional(summary.BusyPercent))}, traceStat{"profiled", fmt.Sprint(summary.Profiled)}))
	style.line(out, style.dim(fmt.Sprintf("%-24s %8s %8s %7s", "PROCESS", "PID", "SAMPLES", "SHARE")))
	for index, row := range report.Processes {
		if index == cpuProcessRows {
			style.line(out, style.hint(fmt.Sprintf("… %d more processes", len(report.Processes)-cpuProcessRows)))
			break
		}
		style.printf(out, "%-24s %8d %8d %6.1f%s\n", emptyAs(row.Process, "-"), row.PID, row.Samples, row.Percent, style.dim("%"))
	}
	style.line(out, style.dim(fmt.Sprintf("%-48s %8s %7s %8s %7s", "FUNCTION", "SELF", "SELF%", "TOTAL", "TOTAL%")))
	for index, row := range report.Functions {
		if index == cpuFunctionRows {
			break
		}
		name := cpuShorten(row.Function, 48)
		if row.Kernel {
			name = cpuShorten(row.Function+" [k]", 48)
		}
		cell := fmt.Sprintf("%-48s", name)
		if row.Kernel && style.color && strings.HasSuffix(name, " [k]") {
			cell = strings.TrimSuffix(name, "[k]") + style.dim("[k]") + cell[len(name):]
		}
		style.printf(out, "%s %8d %6.1f%s %8d %6.1f%s\n", cell, row.Self, row.SelfPercent, style.dim("%"), row.Total, row.TotalPercent, style.dim("%"))
	}
	style.line(out, style.stats(traceStat{"stacks", fmt.Sprint(summary.Stacks)}, traceStat{"idle", fmt.Sprint(summary.Idle)}, traceStat{"self", fmt.Sprint(summary.Self)}, traceStat{"filtered", fmt.Sprint(summary.Filtered)},
		traceStat{"counts_full", fmt.Sprint(summary.CountsFull)}, traceStat{"stacks_full", fmt.Sprint(summary.StacksFull)}, traceStat{"user_missing", fmt.Sprint(summary.UserStackMissing)},
		traceStat{"collisions", fmt.Sprint(summary.UserStackCollision + summary.KernelStackCollision)}, traceStat{"shallow_user", fmt.Sprint(summary.ShallowUserSamples)}))
}

func (report cpuReport) printOffCPU(out io.Writer) {
	style := newTraceSummaryStyle(out)
	summary := report.Summary
	style.line(out, style.headline("OFF-CPU PROFILE", traceStat{"duration", (time.Duration(summary.DurationMS) * time.Millisecond).String()}, traceStat{"blocks", fmt.Sprint(summary.Profiled)}, traceStat{"off-cpu", cpuMilliseconds(cpuOptional(summary.OffCPUMS))}))
	style.line(out, style.dim(fmt.Sprintf("%-24s %8s %8s %10s %7s", "PROCESS", "PID", "BLOCKS", "OFF-CPU", "SHARE")))
	for index, row := range report.Processes {
		if index == cpuProcessRows {
			style.line(out, style.hint(fmt.Sprintf("… %d more processes", len(report.Processes)-cpuProcessRows)))
			break
		}
		style.printf(out, "%-24s %8d %8d %10s %6.1f%s\n", emptyAs(row.Process, "-"), row.PID, row.Samples, style.measure(cpuMilliseconds(*row.TimeMS)), row.Percent, style.dim("%"))
	}
	style.line(out, style.dim(fmt.Sprintf("%-16s %-40s %-16s %8s %10s %9s %7s", "PROCESS", "CODE", "KERNEL", "BLOCKS", "OFF-CPU", "AVG", "SHARE")))
	for index, row := range report.Waits {
		if index == cpuWaitRows {
			break
		}
		style.printf(out, "%-16s %-40s %-16s %8d %10s %9s %6.1f%s\n", cpuShorten(row.Process, 16), cpuShorten(row.Code, 40), cpuShorten(row.Kernel, 16), row.Blocks, style.measure(cpuMilliseconds(row.TimeMS)), style.measure(cpuMilliseconds(row.AvgMS)), row.Percent, style.dim("%"))
	}
	style.line(out, style.stats(traceStat{"stacks", fmt.Sprint(summary.Stacks)}, traceStat{"self", fmt.Sprint(summary.Self)}, traceStat{"filtered", fmt.Sprint(summary.Filtered)}, traceStat{"counts_full", fmt.Sprint(summary.CountsFull)},
		traceStat{"starts_full", fmt.Sprint(summary.OffCPUStartsFull)}, traceStat{"stacks_full", fmt.Sprint(summary.StacksFull)}, traceStat{"user_missing", fmt.Sprint(summary.UserStackMissing)},
		traceStat{"collisions", fmt.Sprint(summary.UserStackCollision + summary.KernelStackCollision)}))
}

func cpuOptional(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// cpuMilliseconds는 ms 값을 크기에 맞는 단위로 쓴다.
func cpuMilliseconds(ms float64) string {
	switch {
	case ms >= 1000:
		return fmt.Sprintf("%.2fs", ms/1000)
	case ms >= 1:
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.0fus", ms*1000)
}

// cpuShorten은 긴 C++과 Go 이름의 앞을 줄인다. 함수 이름은 끝 쪽이 구분에 더 쓸모 있다.
func cpuShorten(name string, width int) string {
	runes := []rune(name)
	if len(runes) <= width {
		return name
	}
	return "…" + string(runes[len(runes)-width+1:])
}

// cpuFoldedLines는 flame graph 도구가 읽는 folded 형식이다. 한 줄이 "process;바깥 함수;…;안쪽 함수 수"이고, 사용자
// frame 뒤에 kernel frame이 온다. 같은 줄로 접히는 stack은 수를 더한다. offCPU이면 수는 잠든 시간의 마이크로초다.
func cpuFoldedLines(stacks []cpuStack, offCPU bool) []string {
	counts := map[string]uint64{}
	for _, stack := range stacks {
		frames := []string{cpuFoldedFrame(emptyAs(stack.Process, "-"))}
		for index := len(stack.User) - 1; index >= 0; index-- {
			frames = append(frames, cpuFoldedFrame(stack.User[index]))
		}
		for index := len(stack.Kernel) - 1; index >= 0; index-- {
			frames = append(frames, cpuFoldedFrame(stack.Kernel[index])+cpuKernelSuffix)
		}
		if len(frames) == 1 {
			frames = append(frames, "[unknown]")
		}
		value := stack.Samples
		if offCPU {
			value = stack.OffCPUNS / uint64(time.Microsecond)
		}
		if value > 0 {
			counts[strings.Join(frames, ";")] += value
		}
	}
	lines := make([]string, 0, len(counts))
	for line := range counts {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		if counts[lines[i]] != counts[lines[j]] {
			return counts[lines[i]] > counts[lines[j]]
		}
		return lines[i] < lines[j]
	})
	for index, line := range lines {
		lines[index] = fmt.Sprintf("%s %d", line, counts[line])
	}
	return lines
}

// cpuFoldedFrame은 folded 형식의 구분자인 ';'와 줄 끝의 수를 가르는 공백을 이름에서 바꾼다.
func cpuFoldedFrame(name string) string {
	return strings.NewReplacer(";", ":", " ", "_", "\n", "_").Replace(name)
}

func writeCPUFolded(path string, stacks []cpuStack, offCPU bool) error {
	text := strings.Join(cpuFoldedLines(stacks, offCPU), "\n")
	if text != "" {
		text += "\n"
	}
	if path == "-" {
		_, err := io.WriteString(os.Stdout, text)
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
