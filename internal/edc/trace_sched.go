//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type sched_record -type sched_counters schedEvents trace_sched_bpf.c -- -I./bpf

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
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

const schedMinimumLatency = time.Millisecond

type schedTraceOptions struct {
	duration     time.Duration
	slow         time.Duration
	process      string
	containerRef string
	container    *traceContainer
	groupBy      string
	raw          bool
	jsonPath     string
	detail       bool
}

type schedEvent struct {
	TimestampNS       uint64  `json:"timestamp_ns"`
	BootTimeNS        uint64  `json:"boot_time_ns"`
	Event             string  `json:"event"`
	PID               uint32  `json:"pid"`
	Process           string  `json:"process"`
	CgroupID          uint64  `json:"cgroup_id"`
	CgroupUnavailable bool    `json:"cgroup_unavailable"`
	LatencyMS         float64 `json:"latency_ms"`
	OffCPUClass       string  `json:"offcpu_class,omitempty"`
}

type schedTraceFinishedMsg struct {
	summary schedSummary
	err     error
}

func schedEventMatches(options schedTraceOptions, event schedEvent) bool {
	return options.container == nil || options.container.cgroups[event.CgroupID]
}

type schedSummary struct {
	TimestampNS           uint64  `json:"timestamp_ns"`
	Event                 string  `json:"event"`
	EventCount            uint64  `json:"event_count"`
	LostEvents            uint64  `json:"lost_events"`
	MapFull               uint64  `json:"map_full"`
	Unmatched             uint64  `json:"unmatched"`
	RepeatedWakeups       uint64  `json:"repeated_wakeups"`
	OmittedBelowThreshold uint64  `json:"omitted_below_threshold"`
	MinimumLatencyMS      float64 `json:"minimum_latency_ms"`
}

type schedStats struct{ values []float64 }

func (s *schedStats) add(value float64) { s.values = append(s.values, value) }
func (s schedStats) report() (uint64, *float64, *float64, *float64) {
	if len(s.values) == 0 {
		return 0, nil, nil, nil
	}
	sort.Float64s(s.values)
	var total float64
	for _, value := range s.values {
		total += value
	}
	average, maximum := total/float64(len(s.values)), s.values[len(s.values)-1]
	p95 := s.values[(len(s.values)*95+99)/100-1]
	return uint64(len(s.values)), &average, &p95, &maximum
}

type schedReportRow struct {
	Group    string     `json:"group"`
	Wakeup   schedStats `json:"-"`
	Runqueue schedStats `json:"-"`
	OffCPU   schedStats `json:"-"`
}
type schedReportValue struct {
	Samples   uint64   `json:"samples"`
	AverageMS *float64 `json:"avg_ms"`
	P95MS     *float64 `json:"p95_ms"`
	MaxMS     *float64 `json:"max_ms"`
}
type schedReport struct {
	DurationMS int64                  `json:"duration_ms"`
	GroupBy    string                 `json:"group_by,omitempty"`
	Rows       []schedReportOutputRow `json:"rows"`
	Summary    schedSummary           `json:"summary"`
}
type schedReportOutputRow struct {
	Group    string           `json:"group"`
	Wakeup   schedReportValue `json:"wakeup"`
	Runqueue schedReportValue `json:"runqueue"`
	OffCPU   schedReportValue `json:"offcpu"`
}

func schedReportValueFor(stats schedStats) schedReportValue {
	n, avg, p95, max := stats.report()
	return schedReportValue{n, avg, p95, max}
}
func (r schedReport) print() {
	fmt.Printf("SCHEDULER TRACE  duration %dms  minimum %gms\n", r.DurationMS, r.Summary.MinimumLatencyMS)
	fmt.Printf("%-20s %8s %8s %8s %8s  %8s %8s %8s %8s  %8s %8s %8s %8s\n", "GROUP", "WAKE N", "AVG", "P95", "MAX", "RUN N", "AVG", "P95", "MAX", "OFF N", "AVG", "P95", "MAX")
	for _, row := range r.Rows {
		fmt.Printf("%-20s %8d %8s %8s %8s  %8d %8s %8s %8s  %8d %8s %8s %8s\n", row.Group, row.Wakeup.Samples, schedMS(row.Wakeup.AverageMS), schedMS(row.Wakeup.P95MS), schedMS(row.Wakeup.MaxMS), row.Runqueue.Samples, schedMS(row.Runqueue.AverageMS), schedMS(row.Runqueue.P95MS), schedMS(row.Runqueue.MaxMS), row.OffCPU.Samples, schedMS(row.OffCPU.AverageMS), schedMS(row.OffCPU.P95MS), schedMS(row.OffCPU.MaxMS))
	}
	fmt.Printf("events %d  ring_lost %d  map_full %d  unmatched %d  repeated_wakeup %d  below_threshold %d\n", r.Summary.EventCount, r.Summary.LostEvents, r.Summary.MapFull, r.Summary.Unmatched, r.Summary.RepeatedWakeups, r.Summary.OmittedBelowThreshold)
}
func schedMS(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}

func runTraceSched(args []string) int {
	options := schedTraceOptions{}
	set := flag.NewFlagSet("trace sched", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, "trace duration")
	set.DurationVar(&options.slow, "slow", schedMinimumLatency, "minimum span latency with a unit, such as 20us, 500us, or 2ms")
	set.StringVar(&options.process, "process", "", "process name")
	set.StringVar(&options.containerRef, "container", "", "container name or ID")
	set.StringVar(&options.groupBy, "group-by", "", "process, cgroup, or event")
	set.BoolVar(&options.raw, "raw", false, "write events as JSON lines")
	set.StringVar(&options.jsonPath, "json", "", "write summary JSON")
	set.BoolVar(&options.detail, "detail", false, "show detailed rows")
	set.BoolVar(&options.detail, "d", false, "show detailed rows")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 || options.duration < 0 || options.duration > maxCaptureDuration || (options.raw && options.jsonPath != "") {
		return 2
	}
	if options.slow <= 0 {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_range"))
		return 2
	}
	if options.groupBy != "" && options.groupBy != "process" && options.groupBy != "cgroup" && options.groupBy != "event" {
		fmt.Fprintln(os.Stderr, "trace sched --group-by must be process, cgroup, or event")
		return 2
	}
	if err := schedTracePrerequisites(); err != nil {
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
		return runSchedTraceScreen(options)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	rows := map[string]*schedReportRow{}
	var written uint64
	encoder := json.NewEncoder(os.Stdout)
	summary, err := collectSchedEvents(options.duration, options.slow, func(event schedEvent) error {
		if options.process != "" && event.Process != options.process {
			return nil
		}
		if !schedEventMatches(options, event) {
			return nil
		}
		if options.raw {
			written++
			return encoder.Encode(event)
		}
		group := event.Process
		if options.groupBy == "event" {
			group = event.Event
		}
		if options.groupBy == "cgroup" {
			group = strconv.FormatUint(event.CgroupID, 10)
		}
		if group == "" {
			group = "-"
		}
		row := rows[group]
		if row == nil {
			row = &schedReportRow{Group: group}
			rows[group] = row
		}
		switch event.Event {
		case "sched_wakeup":
			row.Wakeup.add(event.LatencyMS)
		case "sched_runqueue":
			row.Runqueue.add(event.LatencyMS)
		case "sched_offcpu":
			row.OffCPU.add(event.LatencyMS)
		}
		return nil
	}, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, "trace sched failed:", err)
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
	report := schedReport{DurationMS: time.Since(started).Milliseconds(), GroupBy: options.groupBy, Summary: summary}
	for _, row := range rows {
		report.Rows = append(report.Rows, schedReportOutputRow{row.Group, schedReportValueFor(row.Wakeup), schedReportValueFor(row.Runqueue), schedReportValueFor(row.OffCPU)})
	}
	sort.Slice(report.Rows, func(i, j int) bool { return report.Rows[i].Group < report.Rows[j].Group })
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

func runSchedTraceScreen(options schedTraceOptions) int {
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCollect := func() { stopOnce.Do(func() { close(stop) }) }
	model := newSchedTraceScreenModel(stopCollect)
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	result := make(chan schedTraceFinishedMsg, 1)
	var rows []schedEvent
	go func() {
		summary, err := collectSchedEvents(options.duration, options.slow, func(event schedEvent) error {
			if options.process != "" && event.Process != options.process {
				return nil
			}
			if !schedEventMatches(options, event) {
				return nil
			}
			rows = append(rows, event)
			program.Send(schedTraceEventMsg{events: []schedEvent{event}})
			return nil
		}, stop)
		result <- schedTraceFinishedMsg{summary: summary, err: err}
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
		fmt.Fprintln(os.Stderr, "trace sched failed:", finished.err)
		return 1
	}
	if _, ok := final.(schedTraceScreenModel); !ok {
		return 1
	}
	report := schedReport{DurationMS: options.duration.Milliseconds(), GroupBy: options.groupBy, Summary: finished.summary}
	if report.DurationMS == 0 {
		report.DurationMS = 1
	}
	groups := map[string]*schedReportRow{}
	for _, event := range rows {
		key := event.Process
		if options.groupBy == traceGroupByEvent {
			key = event.Event
		}
		if options.groupBy == "cgroup" {
			key = strconv.FormatUint(event.CgroupID, 10)
		}
		if key == "" {
			key = "-"
		}
		row := groups[key]
		if row == nil {
			row = &schedReportRow{Group: key}
			groups[key] = row
		}
		switch event.Event {
		case "sched_wakeup":
			row.Wakeup.add(event.LatencyMS)
		case "sched_runqueue":
			row.Runqueue.add(event.LatencyMS)
		case "sched_offcpu":
			row.OffCPU.add(event.LatencyMS)
		}
	}
	for _, row := range groups {
		report.Rows = append(report.Rows, schedReportOutputRow{Group: row.Group, Wakeup: schedReportValueFor(row.Wakeup), Runqueue: schedReportValueFor(row.Runqueue), OffCPU: schedReportValueFor(row.OffCPU)})
	}
	report.print()
	return 0
}

var schedTracepoints = []string{"sched_wakeup", "sched_wakeup_new", "sched_switch", "sched_process_exit"}

func schedTracePrerequisites() error {
	return traceBPFPrerequisites("trace sched", schedKernelSupported)
}

func schedKernelSupported(spec *btf.Spec) error {
	for _, required := range []struct {
		name    string
		members []string
	}{
		{"task_struct", []string{"start_boottime", "cgroups"}},
		{"css_set", []string{"dfl_cgrp"}},
		{"cgroup", []string{"kn"}},
		{"kernfs_node", []string{"id"}},
	} {
		var value *btf.Struct
		if err := spec.TypeByName(required.name, &value); err != nil {
			return fmt.Errorf("kernel BTF has no type %s", required.name)
		}
		if missing := schedMissingBTFMembers(value.Members, required.members); missing != "" {
			return fmt.Errorf("kernel BTF has no field %s.%s", required.name, missing)
		}
	}
	return traceTracepointsAvailable(spec, schedTracepoints)
}

func schedMissingBTFMembers(members []btf.Member, required []string) string {
	for _, name := range required {
		found := false
		for _, member := range members {
			if member.Name == name {
				found = true
				break
			}
		}
		if !found {
			return name
		}
	}
	return ""
}

func collectSchedEvents(duration, slow time.Duration, onEvent func(schedEvent) error, stop <-chan struct{}) (schedSummary, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return schedSummary{}, err
	}
	spec, err := loadSchedEvents()
	if err != nil {
		return schedSummary{}, err
	}
	var vars schedEventsVariableSpecs
	if err := spec.Assign(&vars); err != nil {
		return schedSummary{}, err
	}
	if err := vars.MinimumLatencyNs.Set(uint64(slow)); err != nil {
		return schedSummary{}, err
	}
	clockOffset, err := captureClockOffset()
	if err != nil {
		return schedSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	objects := schedEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return schedSummary{}, err
	}
	defer objects.Close()
	links := make([]link.Link, 0, 4)
	defer func() {
		for _, attached := range links {
			_ = attached.Close()
		}
	}()
	for _, hook := range []struct {
		name    string
		program *ebpf.Program
	}{{"sched_wakeup", objects.SchedWakeup}, {"sched_wakeup_new", objects.SchedWakeupNew}, {"sched_switch", objects.SchedSwitch}, {"sched_process_exit", objects.SchedProcessExit}} {
		attached, err := link.AttachTracing(link.TracingOptions{Program: hook.program})
		if err != nil {
			return schedSummary{}, fmt.Errorf("attach tp_btf/%s: %w", hook.name, err)
		}
		links = append(links, attached)
	}
	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return schedSummary{}, err
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
			return schedSummary{}, err
		}
		event, ok := parseSchedRecord(record.RawSample, clockOffset)
		if !ok {
			continue
		}
		count++
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return schedSummary{}, err
			}
		}
	}
	summary := schedSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: count, MinimumLatencyMS: float64(slow) / float64(time.Millisecond)}
	var counters schedEventsSchedCounters
	if err := objects.Counters.Lookup(uint32(0), &counters); err == nil {
		summary.LostEvents = counters.LostEvents
		summary.MapFull = counters.MapFull
		summary.Unmatched = counters.Unmatched
		summary.RepeatedWakeups = counters.RepeatedWakeups
		summary.OmittedBelowThreshold = counters.BelowThreshold
	}
	return summary, nil
}

const schedRecordSize = 56

func parseSchedRecord(sample []byte, clockOffset int64) (schedEvent, bool) {
	if len(sample) < schedRecordSize {
		return schedEvent{}, false
	}
	eventNames := []string{"sched_wakeup", "sched_runqueue", "sched_offcpu"}
	kind := int(sample[52])
	if kind >= len(eventNames) {
		return schedEvent{}, false
	}
	bootTime := binary.LittleEndian.Uint64(sample[0:8])
	return schedEvent{
		TimestampNS: uint64(int64(bootTime) + clockOffset),
		BootTimeNS:  bootTime,
		Event:       eventNames[kind],
		CgroupID:    binary.LittleEndian.Uint64(sample[16:24]),
		PID:         binary.LittleEndian.Uint32(sample[32:36]),
		Process:     string(bytesTrimNUL(sample[36:52])),
		LatencyMS:   float64(binary.LittleEndian.Uint64(sample[8:16])) / float64(time.Millisecond),
		OffCPUClass: schedOffCPUClass(sample[52], sample[53]),
	}, true
}

func schedOffCPUClass(kind, value byte) string {
	if kind != 2 {
		return ""
	}
	if value == 1 {
		return "preempted"
	}
	return "voluntary"
}

func bytesTrimNUL(value []byte) []byte {
	for i, b := range value {
		if b == 0 {
			return value[:i]
		}
	}
	return value
}
