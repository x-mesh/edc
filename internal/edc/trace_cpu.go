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
	traceCPUUsage = "edc trace cpu [--frequency 99] [--process <filter>] [--folded <path>]"
	// cpuDefaultFrequency는 99Hz다. 100Hz처럼 timer tick과 같은 주기로 재면 tick에 맞춰 도는 일만 보이거나 빠진다.
	cpuDefaultFrequency = 99
	cpuMaxFrequency     = 1000
	cpuProcessRows      = 10
	cpuFunctionRows     = 20
	// cpuKernelSuffix는 flame graph 도구가 kernel frame에 붙이는 표시다.
	cpuKernelSuffix = "_[k]"
)

type cpuTraceOptions struct {
	duration     time.Duration
	frequency    int
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	jsonPath     string
	foldedPath   string
}

// cpuStack은 (process, 사용자 stack, kernel stack) 하나와 그 샘플 수다. frame은 가장 안쪽 함수가 먼저 온다.
type cpuStack struct {
	TGID     uint32
	Process  string
	CgroupID uint64
	User     []string
	Kernel   []string
	Samples  uint64
}

// cpuSummary의 Samples는 CPU마다 perf event가 program을 부른 수다. Idle, Self, Filtered를 뺀 나머지가 Profiled이고,
// 표의 비율은 Profiled에 대한 값이다.
type cpuSummary struct {
	DurationMS           int64   `json:"duration_ms"`
	FrequencyHz          int     `json:"frequency_hz"`
	CPUs                 int     `json:"cpus"`
	Samples              uint64  `json:"samples"`
	Idle                 uint64  `json:"idle"`
	BusyPercent          float64 `json:"busy_percent"`
	Self                 uint64  `json:"self"`
	SelfExcluded         bool    `json:"self_excluded"`
	Filtered             uint64  `json:"filtered"`
	Profiled             uint64  `json:"profiled"`
	Stacks               int     `json:"stacks"`
	CountsFull           uint64  `json:"counts_full"`
	StacksFull           uint64  `json:"stacks_full"`
	UserStackMissing     uint64  `json:"user_stack_missing"`
	UserStackCollision   uint64  `json:"user_stack_collision"`
	KernelStackEmpty     uint64  `json:"kernel_stack_empty"`
	KernelStackCollision uint64  `json:"kernel_stack_collision"`
	// ShallowUserSamples는 사용자 stack이 frame 하나뿐인 샘플이다. frame pointer 없이 build한 program에서 많다.
	ShallowUserSamples uint64 `json:"shallow_user_samples"`
	KernelSymbols      bool   `json:"kernel_symbols"`
	// FreshLost는 새 process 알림을 ring에 넣지 못한 수다. 그 process는 다음 1초 훑기에서 읽고, 그 전에 끝났으면 이름이 빈다.
	FreshLost uint64 `json:"fresh_lost"`
}

type cpuProcessRow struct {
	Process string  `json:"process"`
	PID     uint32  `json:"pid"`
	Samples uint64  `json:"samples"`
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
	Summary   cpuSummary       `json:"summary"`
}

func runTraceCPU(args []string) int {
	options := cpuTraceOptions{}
	var process string
	set := flag.NewFlagSet("trace cpu", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.IntVar(&options.frequency, "frequency", cpuDefaultFrequency, T("command.trace.option.frequency"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.StringVar(&options.foldedPath, "folded", "", T("command.trace.option.folded"))
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
	if err := cpuTracePrerequisites(); err != nil {
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
		if err := writeCPUFolded(options.foldedPath, stacks); err != nil {
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
	if lost := summary.CountsFull + summary.StacksFull; lost > 0 {
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
	var profiled uint64
	for _, stack := range stacks {
		profiled += stack.Samples
		processes[processKey{stack.TGID, stack.Process}] += stack.Samples
		if len(stack.User) == 1 {
			report.Summary.ShallowUserSamples += stack.Samples
		}
		// kernel frame이 있으면 그 샘플은 kernel 안에서 멈춘 것이다. 가장 안쪽 frame이 self다.
		switch {
		case len(stack.Kernel) > 0:
			function(stack.Kernel[0], true).Self += stack.Samples
		case len(stack.User) > 0:
			function(stack.User[0], false).Self += stack.Samples
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
					function(name, frames.kernel).Total += stack.Samples
				}
			}
		}
	}
	report.Summary.Profiled = profiled
	for key, samples := range processes {
		report.Processes = append(report.Processes, cpuProcessRow{Process: key.name, PID: key.pid, Samples: samples, Percent: cpuPercent(samples, profiled)})
	}
	sort.Slice(report.Processes, func(i, j int) bool {
		left, right := report.Processes[i], report.Processes[j]
		if left.Samples != right.Samples {
			return left.Samples > right.Samples
		}
		return left.PID < right.PID
	})
	for _, row := range functions {
		row.SelfPercent, row.TotalPercent = cpuPercent(row.Self, profiled), cpuPercent(row.Total, profiled)
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

func (report cpuReport) print(out io.Writer) {
	summary := report.Summary
	fmt.Fprintf(out, "CPU PROFILE  duration %s  %d Hz  %d CPUs  samples %d  busy %.1f%%  profiled %d\n", time.Duration(summary.DurationMS)*time.Millisecond, summary.FrequencyHz, summary.CPUs, summary.Samples, summary.BusyPercent, summary.Profiled)
	fmt.Fprintf(out, "%-24s %8s %8s %7s\n", "PROCESS", "PID", "SAMPLES", "SHARE")
	for index, row := range report.Processes {
		if index == cpuProcessRows {
			fmt.Fprintf(out, "… %d more processes\n", len(report.Processes)-cpuProcessRows)
			break
		}
		fmt.Fprintf(out, "%-24s %8d %8d %6.1f%%\n", emptyAs(row.Process, "-"), row.PID, row.Samples, row.Percent)
	}
	fmt.Fprintf(out, "%-48s %8s %7s %8s %7s\n", "FUNCTION", "SELF", "SELF%", "TOTAL", "TOTAL%")
	for index, row := range report.Functions {
		if index == cpuFunctionRows {
			break
		}
		name := row.Function
		if row.Kernel {
			name += " [k]"
		}
		fmt.Fprintf(out, "%-48s %8d %6.1f%% %8d %6.1f%%\n", cpuShorten(name, 48), row.Self, row.SelfPercent, row.Total, row.TotalPercent)
	}
	fmt.Fprintf(out, "stacks %d  idle %d  self %d  filtered %d  counts_full %d  stacks_full %d  user_missing %d  collisions %d  shallow_user %d\n",
		summary.Stacks, summary.Idle, summary.Self, summary.Filtered, summary.CountsFull, summary.StacksFull, summary.UserStackMissing, summary.UserStackCollision+summary.KernelStackCollision, summary.ShallowUserSamples)
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
// frame 뒤에 kernel frame이 온다. 같은 줄로 접히는 stack은 수를 더한다.
func cpuFoldedLines(stacks []cpuStack) []string {
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
		counts[strings.Join(frames, ";")] += stack.Samples
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

func writeCPUFolded(path string, stacks []cpuStack) error {
	text := strings.Join(cpuFoldedLines(stacks), "\n")
	if text != "" {
		text += "\n"
	}
	if path == "-" {
		_, err := io.WriteString(os.Stdout, text)
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
