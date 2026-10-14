//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type exec_record -type exec_counters execEvents trace_exec_bpf.c -- -I./bpf

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

type execTraceOptions struct {
	duration     time.Duration
	process      string
	containerRef string
	container    *traceContainer
	groupBy      string
	signals      string
	signalMask   uint64
	raw          bool
	jsonPath     string
	detail       bool
}

type execTraceFinishedMsg struct {
	summary execSummary
	err     error
}

func execEventMatches(options execTraceOptions, event execEvent) bool {
	if !execProcessMatches(options.process, event) {
		return false
	}
	return options.container == nil || options.container.cgroups[event.CgroupID]
}

func runTraceExec(args []string) int {
	options := execTraceOptions{}
	set := flag.NewFlagSet("trace exec", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&options.process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.groupBy, "group-by", "", "process, parent, cgroup, or event")
	set.StringVar(&options.signals, "signal", "", T("command.trace.option.signal"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.BoolVar(&options.detail, "detail", false, T("command.trace.option.detail"))
	set.BoolVar(&options.detail, "d", false, T("command.trace.option.detail"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 || options.duration < 0 || options.duration > maxCaptureDuration || (options.raw && options.jsonPath != "") {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc trace exec [--duration 15s] [--process name] [--signal names|all|none] [--group-by process|parent|cgroup|event] [--raw | --json <path>]"))
		return 2
	}
	switch options.groupBy {
	case "", traceGroupByProcess, execGroupByParent, execGroupByCgroup, traceGroupByEvent:
	default:
		fmt.Fprintln(os.Stderr, "trace exec --group-by must be process, parent, cgroup, or event")
		return 2
	}
	mask, err := parseExecSignals(options.signals)
	if err != nil {
		fmt.Fprintln(os.Stderr, "trace exec --signal:", err)
		return 2
	}
	options.signalMask = mask
	if err := execTracePrerequisites(); err != nil {
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
	if !options.raw && options.jsonPath == "" && traceIsTerminal(os.Stdin) && traceIsTerminal(os.Stdout) {
		return runExecTraceScreen(options)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	aggregator := newExecAggregator(options.groupBy)
	var written uint64
	encoder := json.NewEncoder(os.Stdout)
	summary, err := collectExecEvents(options.duration, options.signalMask, func(event execEvent) error {
		if !execEventMatches(options, event) {
			return nil
		}
		if options.raw {
			written++
			return encoder.Encode(event)
		}
		aggregator.add(event)
		return nil
	}, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, "trace exec failed:", err)
		return 1
	}
	if options.raw {
		summary.EventCount = written
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	report := aggregator.report(time.Since(started), summary)
	if options.jsonPath != "" {
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	report.print()
	return 0
}

func runExecTraceScreen(options execTraceOptions) int {
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollect := func() { stopOnce.Do(func() { close(stop) }) }
	model := newExecTraceScreenModel(stopCollect)
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	result := make(chan execTraceFinishedMsg, 1)
	started := time.Now()
	aggregator := newExecAggregator(options.groupBy)
	var mutex sync.Mutex
	go func() {
		summary, err := collectExecEvents(options.duration, options.signalMask, func(event execEvent) error {
			if !execEventMatches(options, event) {
				return nil
			}
			mutex.Lock()
			aggregator.add(event)
			mutex.Unlock()
			program.Send(execTraceEventMsg{events: []execEvent{event}})
			return nil
		}, stop)
		result <- execTraceFinishedMsg{summary: summary, err: err}
		stopCollect()
		program.Quit()
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); stopCollect(); program.Quit() }()
	final, err := program.Run()
	stopCollect()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	finished := <-result
	if finished.err != nil {
		fmt.Fprintln(os.Stderr, "trace exec failed:", finished.err)
		return 1
	}
	if _, ok := final.(execTraceScreenModel); !ok {
		return 1
	}
	mutex.Lock()
	report := aggregator.report(time.Since(started), finished.summary)
	mutex.Unlock()
	report.print()
	return 0
}

var execTracepoints = []string{"sched_process_exec", "sched_process_exit", "signal_generate"}

func execTracePrerequisites() error {
	return traceBPFPrerequisites("trace exec", execKernelSupported)
}

func execKernelSupported(spec *btf.Spec) error {
	for _, required := range []struct {
		name    string
		members []string
	}{
		{"task_struct", []string{"real_parent", "exit_code", "start_boottime", "flags", "mm", "cgroups", "signal"}},
		{"signal_struct", []string{"oom_mm"}},
		{"mm_struct", []string{"arg_start", "arg_end"}},
		{"linux_binprm", []string{"filename"}},
		{"css_set", []string{"dfl_cgrp"}},
		{"cgroup", []string{"kn"}},
		{"kernfs_node", []string{"id"}},
		// kernel_siginfo의 si_code는 이름 없는 안쪽 struct에 있어 member 이름으로 찾지 못한다. type만 확인한다.
		{"kernel_siginfo", nil},
	} {
		var value *btf.Struct
		if err := spec.TypeByName(required.name, &value); err != nil {
			return fmt.Errorf("kernel BTF has no type %s", required.name)
		}
		if missing := execMissingBTFMembers(value.Members, required.members); missing != "" {
			return fmt.Errorf("kernel BTF has no field %s.%s", required.name, missing)
		}
	}
	// mark_victim은 tp_btf가 아니라 tracefs의 tracepoint로 붙인다. 5.15와 6.x의 인자 모양이 달라 tp_btf로는 둘을 함께 읽지 못한다.
	return traceTracepointsAvailable(spec, append(execTracepoints, "mark_victim"))
}

// execMissingBTFMembers는 이름 없는 안쪽 struct와 union도 들어가 본다. mm_struct는 field를 이름 없는 struct로 감싸므로
// arg_start가 바로 아래 member가 아니다. CO-RE relocation도 같은 길로 찾는다.
func execMissingBTFMembers(members []btf.Member, required []string) string {
	for _, name := range required {
		if !execHasBTFMember(members, name, 0) {
			return name
		}
	}
	return ""
}

func execHasBTFMember(members []btf.Member, name string, depth int) bool {
	if depth > 4 {
		return false
	}
	for _, member := range members {
		if member.Name == name {
			return true
		}
		if member.Name != "" {
			continue
		}
		switch inner := btf.UnderlyingType(member.Type).(type) {
		case *btf.Struct:
			if execHasBTFMember(inner.Members, name, depth+1) {
				return true
			}
		case *btf.Union:
			if execHasBTFMember(inner.Members, name, depth+1) {
				return true
			}
		}
	}
	return false
}

func collectExecEvents(duration time.Duration, signalMask uint64, onEvent func(execEvent) error, stop <-chan struct{}) (execSummary, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return execSummary{}, err
	}
	spec, err := loadExecEvents()
	if err != nil {
		return execSummary{}, err
	}
	var vars execEventsVariableSpecs
	if err := spec.Assign(&vars); err != nil {
		return execSummary{}, err
	}
	if err := vars.SignalMask.Set(signalMask); err != nil {
		return execSummary{}, err
	}
	clockOffset, err := captureClockOffset()
	if err != nil {
		return execSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	objects := execEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return execSummary{}, err
	}
	defer objects.Close()
	links := make([]link.Link, 0, 4)
	defer func() {
		for _, attached := range links {
			_ = attached.Close()
		}
	}()
	hooks := []struct {
		name    string
		program *ebpf.Program
	}{{"sched_process_exec", objects.ExecProcessExec}, {"sched_process_exit", objects.ExecProcessExit}}
	// signal_mask가 0이면 신호 tracepoint를 붙이지 않는다. 바쁜 호스트에서 신호는 초당 수천 번이다.
	if signalMask != 0 {
		hooks = append(hooks, struct {
			name    string
			program *ebpf.Program
		}{"signal_generate", objects.ExecSignalGenerate})
	}
	for _, hook := range hooks {
		attached, err := link.AttachTracing(link.TracingOptions{Program: hook.program})
		if err != nil {
			return execSummary{}, fmt.Errorf("attach tp_btf/%s: %w", hook.name, err)
		}
		links = append(links, attached)
	}
	oom, err := link.Tracepoint("oom", "mark_victim", objects.ExecOomMarkVictim, nil)
	if err != nil {
		return execSummary{}, fmt.Errorf("attach tracepoint/oom/mark_victim: %w", err)
	}
	links = append(links, oom)
	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return execSummary{}, err
	}
	defer reader.Close()
	done := make(chan struct{})
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = reader.Close()
			case <-done:
			}
		}()
	}
	defer close(done)
	deadline := time.Now().Add(duration)
	var count uint64
	stopped := false
	for duration == 0 || time.Now().Before(deadline) {
		next := time.Now().Add(time.Second)
		if duration > 0 && deadline.Before(next) {
			next = deadline
		}
		reader.SetDeadline(next)
		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if stop != nil {
				select {
				case <-stop:
					stopped = true
				default:
				}
			}
			if stopped || errors.Is(err, ringbuf.ErrClosed) {
				break
			}
			return execSummary{}, err
		}
		event, ok := parseExecRecord(record.RawSample, clockOffset)
		if !ok {
			continue
		}
		count++
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return execSummary{}, err
			}
		}
	}
	summary := execSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: count}
	var counters execEventsExecCounters
	if err := objects.Counters.Lookup(uint32(0), &counters); err == nil {
		summary.LostEvents = counters.LostEvents
		summary.FilteredSignals = counters.FilteredSignals
		summary.ArgsTruncated = counters.ArgsTruncated
	}
	return summary, nil
}
