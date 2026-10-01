package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type io_record -type io_counters ioEvents trace_io_bpf.c -- -I./bpf

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	traceGroupByDevice = "device"
	traceGroupByCgroup = "cgroup"
	ioLatencyThreshold = time.Millisecond
)

// ioTraceOptions is kept separate from tcpTraceOptions. The registry owner maps
// the common trace flags to this type when it registers trace io.
type ioTraceOptions struct {
	duration  time.Duration
	process   string
	container *traceContainer
	device    string
	groupBy   string
	raw       bool
	jsonPath  string
	detail    bool
}

type ioEvent struct {
	TimestampNS uint64   `json:"timestamp_ns"`
	BootTimeNS  uint64   `json:"boot_time_ns"`
	Event       string   `json:"event"`
	Operation   string   `json:"operation"`
	Device      string   `json:"device"`
	Major       uint32   `json:"major"`
	Minor       uint32   `json:"minor"`
	Bytes       uint64   `json:"bytes"`
	PID         uint32   `json:"pid"`
	Process     string   `json:"process"`
	CgroupID    uint64   `json:"cgroup_id"`
	Attribution string   `json:"attribution"`
	QueueMS     *float64 `json:"queue_ms,omitempty"`
	ServiceMS   *float64 `json:"service_ms,omitempty"`
	TotalMS     *float64 `json:"total_ms,omitempty"`
}

type ioSummary struct {
	TimestampNS         uint64 `json:"timestamp_ns"`
	Event               string `json:"event"`
	EventCount          uint64 `json:"event_count"`
	Bytes               uint64 `json:"bytes"`
	ReadOps             uint64 `json:"read_ops"`
	WriteOps            uint64 `json:"write_ops"`
	LostEvents          uint64 `json:"lost_events"`
	MapFull             uint64 `json:"map_full"`
	UnmatchedCompletion uint64 `json:"unmatched_completion"`
	Incomplete          uint64 `json:"incomplete"`
	Requeue             uint64 `json:"requeue"`
}

type ioLatency struct {
	AverageMS *float64 `json:"average_ms,omitempty"`
	P95MS     *float64 `json:"p95_ms,omitempty"`
	MaxMS     *float64 `json:"max_ms,omitempty"`
}

type ioReport struct {
	ioSummary
	GroupBy string        `json:"group_by,omitempty"`
	Queue   ioLatency     `json:"queue"`
	Service ioLatency     `json:"service"`
	Total   ioLatency     `json:"total"`
	Rows    []ioReportRow `json:"rows,omitempty"`
}

type ioReportRow struct {
	Group    string    `json:"group"`
	Ops      uint64    `json:"ops"`
	Bytes    uint64    `json:"bytes"`
	ReadOps  uint64    `json:"read_ops"`
	WriteOps uint64    `json:"write_ops"`
	Queue    ioLatency `json:"queue"`
	Service  ioLatency `json:"service"`
	Total    ioLatency `json:"total"`
}

func validIOGroupBy(value string) bool {
	return value == "" || value == traceGroupByDevice || value == traceGroupByProcess || value == traceGroupByCgroup || value == traceGroupByEvent
}

func ioDevice(major, minor uint32) string { return fmt.Sprintf("%d:%d", major, minor) }

func ioReportFor(events []ioEvent, summary ioSummary) ioReport {
	report := ioReport{ioSummary: summary}
	queue, service, total := make([]float64, 0, len(events)), make([]float64, 0, len(events)), make([]float64, 0, len(events))
	for _, event := range events {
		report.Bytes += event.Bytes
		if event.Operation == "read" {
			report.ReadOps++
		} else if event.Operation == "write" {
			report.WriteOps++
		}
		if event.QueueMS != nil {
			queue = append(queue, *event.QueueMS)
		}
		if event.ServiceMS != nil {
			service = append(service, *event.ServiceMS)
		}
		if event.TotalMS != nil {
			total = append(total, *event.TotalMS)
		}
	}
	report.Queue, report.Service, report.Total = ioLatencyFor(queue), ioLatencyFor(service), ioLatencyFor(total)
	return report
}

func ioGroupedReportFor(events []ioEvent, summary ioSummary, groupBy string) ioReport {
	report := ioReportFor(events, summary)
	report.GroupBy = groupBy
	groups := map[string][]ioEvent{}
	for _, event := range events {
		key := event.Event
		switch groupBy {
		case traceGroupByDevice:
			key = event.Device
		case traceGroupByProcess:
			key = event.Process
		case traceGroupByCgroup:
			key = fmt.Sprint(event.CgroupID)
		}
		groups[key] = append(groups[key], event)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		items := groups[key]
		aggregate := ioReportFor(items, ioSummary{})
		report.Rows = append(report.Rows, ioReportRow{Group: key, Ops: uint64(len(items)), Bytes: aggregate.Bytes, ReadOps: aggregate.ReadOps, WriteOps: aggregate.WriteOps, Queue: aggregate.Queue, Service: aggregate.Service, Total: aggregate.Total})
	}
	return report
}

func ioLatencyFor(values []float64) ioLatency {
	if len(values) == 0 {
		return ioLatency{}
	}
	sort.Float64s(values)
	var sum float64
	for _, value := range values {
		sum += value
	}
	average, p95, maximum := sum/float64(len(values)), values[(len(values)*95+99)/100-1], values[len(values)-1]
	return ioLatency{AverageMS: &average, P95MS: &p95, MaxMS: &maximum}
}

func (options ioTraceOptions) matches(event ioEvent) bool {
	if options.process != "" && !strings.Contains(event.Process, options.process) {
		return false
	}
	if options.device != "" && event.Device != options.device {
		return false
	}
	return options.container == nil || options.container.cgroups[event.CgroupID]
}

func runIOTrace(args []string) int {
	options := ioTraceOptions{}
	set := flag.NewFlagSet("trace io", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, "trace duration")
	set.StringVar(&options.process, "process", "", "process name")
	set.StringVar(&options.device, "device", "", "block device major:minor")
	set.StringVar(&options.groupBy, "group-by", "", "device, process, cgroup, or event")
	set.BoolVar(&options.raw, "raw", false, "write JSON lines")
	set.StringVar(&options.jsonPath, "json", "", "write JSON report")
	set.BoolVar(&options.detail, "detail", false, "show detail")
	set.BoolVar(&options.detail, "d", false, "show detail")
	container := set.String("container", "", "Docker container")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace io"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	if options.raw && options.jsonPath != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.raw_json_conflict"))
		return 2
	}
	if !validIOGroupBy(options.groupBy) {
		fmt.Fprintln(os.Stderr, T("cli.trace.io_group_by"))
		return 2
	}
	if options.raw && options.groupBy != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_raw_conflict"))
		return 2
	}
	if *container != "" {
		resolved, code, err := resolveTraceContainer(*container)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return code
		}
		options.container = resolved
	}
	if err := ioTracePrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if !options.raw && options.jsonPath == "" && traceIsTerminal(os.Stdin) && traceIsTerminal(os.Stdout) {
		return runIOTraceScreen(options)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	events := make([]ioEvent, 0)
	var encoder *json.Encoder
	if options.raw {
		encoder = json.NewEncoder(os.Stdout)
	}
	summary, err := collectIOEvents(options, func(event ioEvent) error {
		if options.raw {
			return encoder.Encode(event)
		}
		events = append(events, event)
		return nil
	}, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.io_failed", err))
		return 1
	}
	if options.raw {
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	report := ioReportFor(events, summary)
	if options.groupBy != "" {
		report = ioGroupedReportFor(events, summary, options.groupBy)
	}
	if options.jsonPath != "" {
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	printIOReport(report, events, options.detail)
	return 0
}

func printIOReport(report ioReport, events []ioEvent, detail bool) {
	fmt.Printf("I/O trace\n\nOps: %d\nBytes: %s\nReads: %d\nWrites: %d\nQueue avg/p95/max: %s / %s / %s\nService avg/p95/max: %s / %s / %s\nTotal avg/p95/max: %s / %s / %s\nLost events: %d\nMap full: %d\nUnmatched completion: %d\nIncomplete: %d\nRequeue: %d\n", report.EventCount, traceBytes(report.Bytes), report.ReadOps, report.WriteOps, traceLatency(report.Queue.AverageMS, "ms"), traceLatency(report.Queue.P95MS, "ms"), traceLatency(report.Queue.MaxMS, "ms"), traceLatency(report.Service.AverageMS, "ms"), traceLatency(report.Service.P95MS, "ms"), traceLatency(report.Service.MaxMS, "ms"), traceLatency(report.Total.AverageMS, "ms"), traceLatency(report.Total.P95MS, "ms"), traceLatency(report.Total.MaxMS, "ms"), report.LostEvents, report.MapFull, report.UnmatchedCompletion, report.Incomplete, report.Requeue)
	for _, row := range report.Rows {
		fmt.Printf("%s ops=%d bytes=%s read=%d write=%d queue=%s/%s/%s service=%s/%s/%s total=%s/%s/%s\n", row.Group, row.Ops, traceBytes(row.Bytes), row.ReadOps, row.WriteOps, traceLatency(row.Queue.AverageMS, "ms"), traceLatency(row.Queue.P95MS, "ms"), traceLatency(row.Queue.MaxMS, "ms"), traceLatency(row.Service.AverageMS, "ms"), traceLatency(row.Service.P95MS, "ms"), traceLatency(row.Service.MaxMS, "ms"), traceLatency(row.Total.AverageMS, "ms"), traceLatency(row.Total.P95MS, "ms"), traceLatency(row.Total.MaxMS, "ms"))
	}
	if detail {
		for _, event := range events {
			fmt.Println(ioDetail(event))
		}
	}
}
