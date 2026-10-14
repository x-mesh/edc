package edc

import (
	"context"
	"encoding/json"
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
	traceReclaimUsage  = "edc trace reclaim [--slow 10ms] [--process <list>] [--group-by kind]"
	reclaimGroupByKind = "kind"
	reclaimTraceRows   = 20
)

// reclaimKinds는 BPF의 enum reclaim_kind 순서다.
var reclaimKinds = []string{"direct", "memcg", "throttle", "compact"}

func reclaimKindName(kind uint32) string {
	if int(kind) < len(reclaimKinds) {
		return reclaimKinds[kind]
	}
	return "kind_" + strconv.FormatUint(uint64(kind), 10)
}

type reclaimTraceOptions struct {
	duration     time.Duration
	pids         []uint32
	comms        []string
	containerRef string
	container    *traceContainer
	groupBy      string
	jsonPath     string
	slow         time.Duration
	raw          bool
}

// reclaimEvent는 --slow 이상 멈춘 한 번이다. Reclaimed는 회수한 byte이고, throttle과 compact에는 없다.
type reclaimEvent struct {
	TimestampNS uint64  `json:"timestamp_ns"`
	Event       string  `json:"event"`
	PID         uint32  `json:"pid"`
	TID         uint32  `json:"tid"`
	Process     string  `json:"process"`
	CgroupID    uint64  `json:"cgroup_id"`
	Kind        string  `json:"kind"`
	LatencyMS   float64 `json:"latency_ms"`
	Reclaimed   *uint64 `json:"reclaimed_bytes,omitempty"`
}

func (event reclaimEvent) line() string {
	text := fmt.Sprintf("%s  %s %d/%d  %s  %s", time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000000"), event.Process, event.PID, event.TID, event.Kind, syscallTime(event.LatencyMS*float64(time.Millisecond)))
	if event.Reclaimed != nil {
		text += "  reclaimed " + traceBytes(*event.Reclaimed)
	}
	return text + fmt.Sprintf("  cgroup %d", event.CgroupID)
}

// reclaimSample은 BPF map의 항목 하나다. Reclaimed는 page 수다.
type reclaimSample struct {
	Process   string
	CgroupID  uint64
	Kind      string
	Count     uint64
	SumNS     uint64
	MaxNS     uint64
	Reclaimed uint64
	Hist      topProbeHist
}

type reclaimSummary struct {
	// Event는 --raw의 마지막 줄에만 붙는다. 앞의 event 줄과 구분한다.
	Event        string  `json:"event,omitempty"`
	DurationMS   int64   `json:"duration_ms"`
	Stalls       uint64  `json:"stalls"`
	TotalMS      float64 `json:"total_ms"`
	StatsEntries int     `json:"stats_entries"`
	StatsFull    uint64  `json:"stats_full"`
	SelfExcluded bool    `json:"self_excluded"`
	// Unavailable은 kernel에 없어 붙이지 못한 종류와 그 이유다. 그 종류는 결과에 없다.
	Unavailable map[string]string `json:"unavailable,omitempty"`
	SlowMS      *float64          `json:"slow_ms,omitempty"`
	Events      uint64            `json:"events,omitempty"`
	SampledOut  uint64            `json:"sampled_out,omitempty"`
	RingLost    uint64            `json:"ring_lost,omitempty"`
}

type reclaimCollection struct {
	samples []reclaimSample
	summary reclaimSummary
}

// reclaimReportRow의 P95MS는 histogram bucket의 위쪽 경계다. 실제 p95는 그 값보다 작다.
type reclaimReportRow struct {
	Process   string   `json:"process,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	CgroupID  *uint64  `json:"cgroup_id,omitempty"`
	Stalls    uint64   `json:"stalls"`
	TotalMS   float64  `json:"total_ms"`
	AverageMS float64  `json:"avg_ms"`
	P95MS     *float64 `json:"p95_ms,omitempty"`
	MaxMS     float64  `json:"max_ms"`
	// ReclaimedBytes는 direct와 memcg 회수에만 있다.
	ReclaimedBytes uint64 `json:"reclaimed_bytes"`
	hist           topProbeHist
}

type reclaimReport struct {
	GroupBy string             `json:"group_by,omitempty"`
	Rows    []reclaimReportRow `json:"rows"`
	Summary reclaimSummary     `json:"summary"`
}

func runTraceReclaim(args []string) int {
	options := reclaimTraceOptions{}
	var process string
	set := flag.NewFlagSet("trace reclaim", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.groupBy, "group-by", "", "process, cgroup, or kind")
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.DurationVar(&options.slow, "slow", 0, T("command.trace.option.slow"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace reclaim"))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	switch options.groupBy {
	case "", traceGroupByProcess, traceGroupByCgroup, reclaimGroupByKind:
	default:
		fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_group_by"))
		return 2
	}
	pids, comms, err := parseSyscallProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.syscall_process", err))
		return 2
	}
	options.pids, options.comms = pids, comms
	if options.slow < 0 {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_range"))
		return 2
	}
	switch {
	case options.raw && options.jsonPath != "":
		fmt.Fprintln(os.Stderr, T("cli.trace.raw_json_conflict"))
		return 2
	case options.raw && options.groupBy != "":
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_raw_conflict"))
		return 2
	case options.raw && options.slow == 0:
		fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_raw_slow"))
		return 2
	}
	if err := reclaimTracePrerequisites(); err != nil {
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
			fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_running_for", options.duration))
		} else {
			fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_running"))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var onEvent func(reclaimEvent) error
	encoder := json.NewEncoder(os.Stdout)
	switch {
	case options.raw:
		onEvent = func(event reclaimEvent) error { return encoder.Encode(event) }
	case options.jsonPath == "" && options.slow > 0:
		onEvent = func(event reclaimEvent) error {
			_, err := fmt.Fprintln(os.Stdout, event.line())
			return err
		}
	}
	collection, err := collectReclaimStats(options, ctx.Done(), onEvent)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_failed", err))
		return 1
	}
	report := reclaimReportFor(collection, options.groupBy, uint64(os.Getpagesize()))
	if options.slow > 0 {
		slow := float64(options.slow) / float64(time.Millisecond)
		report.Summary.SlowMS = &slow
	}
	// 알림은 stderr로 가므로 --json -의 stdout을 바꾸지 않는다.
	notices := func() {
		kinds := make([]string, 0, len(report.Summary.Unavailable))
		for kind := range report.Summary.Unavailable {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_unavailable", kind, report.Summary.Unavailable[kind]))
		}
		if report.Summary.StatsFull > 0 {
			fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_map_full", report.Summary.StatsFull))
		}
		if !report.Summary.SelfExcluded {
			fmt.Fprintln(os.Stderr, T("cli.trace.reclaim_self_included"))
		}
	}
	switch {
	case options.raw:
		notices()
		summary := report.Summary
		summary.Event = "reclaim_summary"
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case options.jsonPath != "":
		notices()
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	if report.Summary.Events > 0 {
		fmt.Fprintln(os.Stdout)
	}
	report.print(os.Stdout)
	notices()
	return 0
}

func reclaimGroupKey(sample reclaimSample, groupBy string) string {
	switch groupBy {
	case traceGroupByProcess:
		return sample.Process
	case traceGroupByCgroup:
		return strconv.FormatUint(sample.CgroupID, 10)
	case reclaimGroupByKind:
		return sample.Kind
	}
	return sample.Process + "\x00" + sample.Kind
}

func reclaimReportFor(collection reclaimCollection, groupBy string, pageSize uint64) reclaimReport {
	report := reclaimReport{GroupBy: groupBy, Summary: collection.summary, Rows: []reclaimReportRow{}}
	report.Summary.StatsEntries = len(collection.samples)
	rows := map[string]*reclaimReportRow{}
	maxNS := map[string]uint64{}
	sumNS := map[string]uint64{}
	var totalNS uint64
	for _, sample := range collection.samples {
		report.Summary.Stalls += sample.Count
		totalNS += sample.SumNS
		key := reclaimGroupKey(sample, groupBy)
		row := rows[key]
		if row == nil {
			row = &reclaimReportRow{}
			switch groupBy {
			case traceGroupByProcess:
				row.Process = sample.Process
			case traceGroupByCgroup:
				cgroupID := sample.CgroupID
				row.CgroupID = &cgroupID
			case reclaimGroupByKind:
				row.Kind = sample.Kind
			default:
				row.Process, row.Kind = sample.Process, sample.Kind
			}
			rows[key] = row
		}
		row.Stalls += sample.Count
		sumNS[key] += sample.SumNS
		if sample.MaxNS > maxNS[key] {
			maxNS[key] = sample.MaxNS
		}
		row.ReclaimedBytes += sample.Reclaimed * pageSize
		for index, count := range sample.Hist {
			row.hist[index] += count
		}
	}
	report.Summary.TotalMS = float64(totalNS) / float64(time.Millisecond)
	for key, row := range rows {
		row.TotalMS = float64(sumNS[key]) / float64(time.Millisecond)
		row.MaxMS = float64(maxNS[key]) / float64(time.Millisecond)
		if row.Stalls > 0 {
			row.AverageMS = row.TotalMS / float64(row.Stalls)
		}
		if p95, ok := topProbePercentileMS(row.hist, 0.95); ok {
			row.P95MS = &p95
		}
		report.Rows = append(report.Rows, *row)
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		left, right := report.Rows[i], report.Rows[j]
		if left.TotalMS != right.TotalMS {
			return left.TotalMS > right.TotalMS
		}
		return strings.Join(left.cells(groupBy), "\x00") < strings.Join(right.cells(groupBy), "\x00")
	})
	return report
}

func (row reclaimReportRow) cells(groupBy string) []string {
	switch groupBy {
	case traceGroupByProcess:
		return []string{emptyAs(row.Process, "-")}
	case traceGroupByCgroup:
		if row.CgroupID == nil {
			return []string{"-"}
		}
		return []string{strconv.FormatUint(*row.CgroupID, 10)}
	case reclaimGroupByKind:
		return []string{row.Kind}
	}
	return []string{emptyAs(row.Process, "-"), row.Kind}
}

func (report reclaimReport) print(out io.Writer) {
	summary := report.Summary
	fmt.Fprintf(out, "RECLAIM TRACE  duration %s  stalls %d  stalled %s\n", syscallTime(float64(summary.DurationMS)*float64(time.Millisecond)), summary.Stalls, syscallTime(summary.TotalMS*float64(time.Millisecond)))
	var titles []string
	switch report.GroupBy {
	case traceGroupByProcess:
		titles = []string{"PROCESS"}
	case traceGroupByCgroup:
		titles = []string{"CGROUP"}
	case reclaimGroupByKind:
		titles = []string{"KIND"}
	default:
		titles = []string{"PROCESS", "KIND"}
	}
	shown := report.Rows[:min(len(report.Rows), reclaimTraceRows)]
	widths := make([]int, len(titles))
	for index, title := range titles {
		widths[index] = len(title)
		for _, row := range shown {
			widths[index] = max(widths[index], len(row.cells(report.GroupBy)[index]))
		}
	}
	header := ""
	for index, title := range titles {
		header += fmt.Sprintf("%-*s  ", widths[index], title)
	}
	fmt.Fprintf(out, "%s%8s %9s %9s %9s %9s %10s\n", header, "STALLS", "TOTAL", "AVG", "P95", "MAX", "RECLAIMED")
	for _, row := range shown {
		line := ""
		for index, cell := range row.cells(report.GroupBy) {
			line += fmt.Sprintf("%-*s  ", widths[index], cell)
		}
		average, maximum := row.AverageMS, row.MaxMS
		reclaimed := "-"
		if row.ReclaimedBytes > 0 {
			reclaimed = traceBytes(row.ReclaimedBytes)
		}
		fmt.Fprintf(out, "%s%8d %9s %9s %9s %9s %10s\n", line, row.Stalls, syscallTime(row.TotalMS*float64(time.Millisecond)), syscallMS(&average, ""), syscallMS(row.P95MS, "<"), syscallMS(&maximum, ""), reclaimed)
	}
	if hidden := len(report.Rows) - len(shown); hidden > 0 {
		fmt.Fprintf(out, "... %d more rows in --json\n", hidden)
	}
	fmt.Fprintf(out, "entries %d  stats_full %d", summary.StatsEntries, summary.StatsFull)
	if summary.SlowMS != nil {
		fmt.Fprintf(out, "  events %d  sampled_out %d  ring_lost %d", summary.Events, summary.SampledOut, summary.RingLost)
	}
	fmt.Fprintln(out)
}
