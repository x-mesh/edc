package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// trace show는 --raw로 저장한 JSONL을 다시 읽어, 살아 있는 trace가 끝날 때 내는 요약을 낸다. 현장에서 --raw로 남기고
// 나중에 다른 호스트에서 읽는 흐름이다. kernel이 필요 없어 macOS에서도 돌아간다.

const (
	// traceShowLineLimit은 한 줄의 상한이다. --payload=all의 event는 1 MiB까지 payload를 담는다.
	traceShowLineLimit = 8 << 20
	// traceRawSocketLimit은 --raw가 socket 번호를 기억하는 수다. 넘으면 비우고 다시 1부터 센다.
	traceRawSocketLimit = 1 << 20
)

// traceRawSockets는 kernel socket 주소를 1부터 세는 번호로 바꾼다. 주소는 kernel 포인터라 밖에 내지 않고, 번호는
// trace show가 같은 연결의 event를 다시 묶는 데 쓴다.
type traceRawSockets map[uint64]uint64

func (sockets traceRawSockets) number(socket uint64) uint64 {
	if socket == 0 {
		return 0
	}
	if number, ok := sockets[socket]; ok {
		return number
	}
	if len(sockets) >= traceRawSocketLimit {
		clear(sockets)
	}
	number := uint64(len(sockets) + 1)
	sockets[socket] = number
	return number
}

type traceShowOptions struct {
	groupBy     string
	process     string
	destination string
	jsonPath    string
	detail      bool
}

// traceShowFile은 파일에서 읽은 event다. 한 파일에는 trace 하나의 event만 있다.
type traceShowFile struct {
	protocol string
	events   []captureEvent
	execs    []execEvent
	summary  captureSummary
	execSum  execSummary
	first    uint64
	last     uint64
	lines    int
}

func (file *traceShowFile) duration() time.Duration {
	if file.last <= file.first {
		return time.Second
	}
	return time.Duration(file.last - file.first)
}

func runTraceShow(args []string) int {
	options := traceShowOptions{}
	set := newFlagSet("trace show")
	set.SetOutput(os.Stderr)
	set.StringVar(&options.groupBy, "group-by", "", T("command.trace.option.group_by"))
	set.StringVar(&options.process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.destination, "destination", "", T("command.trace.option.destination"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.BoolVar(&options.detail, "detail", false, T("command.trace.option.detail"))
	set.BoolVar(&options.detail, "d", false, T("command.trace.option.detail"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	usage := T("cli.usage", traceShowUsage)
	if set.NArg() == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if set.NArg() == 1 {
		path := set.Arg(0)
		file, err := openTraceShowFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("cli.trace.show_failed", path, err))
			return 2
		}
		return printTraceShow(file, options)
	}
	return printTraceShowComparison(set.Args(), options)
}

func openTraceShowFile(path string) (*traceShowFile, error) {
	if path == "-" {
		return readTraceShowFile(os.Stdin)
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	return readTraceShowFile(handle)
}

func printTraceShowComparison(paths []string, options traceShowOptions) int {
	if options.jsonPath != "" || options.groupBy != "" {
		fmt.Fprintln(os.Stderr, "trace show with multiple files accepts --process, --destination and --detail")
		return 2
	}
	type recording struct {
		path, protocol string
		first, last    uint64
		print          func()
	}
	var recordings []recording
	var overlapStart, overlapEnd uint64
	for _, path := range paths {
		if path == "-" {
			fmt.Fprintln(os.Stderr, "trace show with multiple files requires file paths")
			return 2
		}
		file, err := openTraceShowFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, T("cli.trace.show_failed", path, err))
			return 2
		}
		if file.first == 0 {
			overlapStart, overlapEnd = 0, 0
		} else if len(recordings) == 0 {
			overlapStart, overlapEnd = file.first, file.last
		} else if overlapStart != 0 {
			if file.first > overlapStart {
				overlapStart = file.first
			}
			overlapEnd = min(overlapEnd, file.last)
		}
		var printReport func()
		if file.protocol == traceShowExec {
			report := traceShowExecReport(file, options)
			printReport = report.print
		} else {
			aggregate, summary := traceShowAggregate(file, options)
			report := aggregate.summary.summarize(summary, file.duration())
			printReport = func() { report.print(options.detail) }
		}
		recordings = append(recordings, recording{path, file.protocol, file.first, file.last, printReport})
	}

	fmt.Fprintln(os.Stdout, "Recorded event ranges (UTC, before replay filters)")
	table := traceTable{titles: []string{"FILE", "TRACE", "FIRST EVENT", "LAST EVENT"}}
	for _, recording := range recordings {
		table.add(traceEscapeText([]byte(recording.path)), recording.protocol, traceShowTime(recording.first), traceShowTime(recording.last))
	}
	newTraceSummaryStyle(os.Stdout).writeTable(os.Stdout, table)
	switch {
	case overlapStart == 0:
		fmt.Fprintln(os.Stdout, "Shared event time range unavailable.")
	case overlapStart > overlapEnd:
		fmt.Fprintln(os.Stdout, "No shared event time range.")
	default:
		fmt.Fprintf(os.Stdout, "Shared event time range %s to %s. Each report covers its full recording. Compare timestamps across hosts using synchronized clocks.\n", traceShowTime(overlapStart), traceShowTime(overlapEnd))
	}
	for _, recording := range recordings {
		fmt.Fprintf(os.Stdout, "\n%q (%s)\n", recording.path, recording.protocol)
		recording.print()
	}
	return 0
}

func traceShowTime(ns uint64) string {
	if ns == 0 {
		return "unavailable"
	}
	return time.Unix(0, int64(ns)).UTC().Format(time.RFC3339Nano)
}

// readTraceShowFile은 JSONL을 한 줄씩 읽는다. protocol이 있는 줄은 네트워크·DB·socket·drop event이고, exec·exit·signal·oom은
// trace exec의 event다. 마지막 capture_summary 줄은 잃은 event 수를 준다.
func readTraceShowFile(reader io.Reader) (*traceShowFile, error) {
	file := &traceShowFile{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), traceShowLineLimit)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		file.lines++
		var head struct {
			Event    string `json:"event"`
			Protocol string `json:"protocol"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return nil, fmt.Errorf("line %d: %w", file.lines, err)
		}
		switch {
		case head.Event == "capture_summary":
			if file.protocol == traceShowExec {
				if err := json.Unmarshal(line, &file.execSum); err != nil {
					return nil, fmt.Errorf("line %d: %w", file.lines, err)
				}
				continue
			}
			if err := json.Unmarshal(line, &file.summary); err != nil {
				return nil, fmt.Errorf("line %d: %w", file.lines, err)
			}
		case head.Event == execEventExec || head.Event == execEventExit || head.Event == execEventSignal || head.Event == execEventOOM:
			if err := file.setProtocol(traceShowExec, file.lines); err != nil {
				return nil, err
			}
			var event execEvent
			if err := json.Unmarshal(line, &event); err != nil {
				return nil, fmt.Errorf("line %d: %w", file.lines, err)
			}
			file.observeTime(event.TimestampNS)
			file.execs = append(file.execs, event)
		case head.Protocol != "" && knownTraceProtocol(head.Protocol):
			if err := file.setProtocol(head.Protocol, file.lines); err != nil {
				return nil, err
			}
			var event captureEvent
			if err := json.Unmarshal(line, &event); err != nil {
				return nil, fmt.Errorf("line %d: %w", file.lines, err)
			}
			event.SocketID = event.Socket
			file.observeTime(event.TimestampNS)
			file.events = append(file.events, event)
		default:
			return nil, fmt.Errorf("line %d: not a trace event: %s", file.lines, traceShowPreview(line))
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("line %d is longer than %d bytes", file.lines+1, traceShowLineLimit)
		}
		return nil, err
	}
	if protocol := file.summary.TraceProtocol; knownTraceProtocol(protocol) {
		if err := file.setProtocol(protocol, file.lines); err != nil {
			return nil, err
		}
	}
	if file.protocol == "" {
		return nil, errors.New("no trace event in the file")
	}
	return file, nil
}

const (
	traceShowExec  = "exec"
	traceShowUsage = "edc trace show <raw.jsonl> [more.jsonl ...] [options]"
)

func (file *traceShowFile) setProtocol(protocol string, line int) error {
	if file.protocol == "" {
		file.protocol = protocol
	}
	if file.protocol != protocol {
		return fmt.Errorf("line %d: %s event in a %s trace", line, protocol, file.protocol)
	}
	return nil
}

func (file *traceShowFile) observeTime(at uint64) {
	if at == 0 {
		return
	}
	if file.first == 0 || at < file.first {
		file.first = at
	}
	if at > file.last {
		file.last = at
	}
}

func traceShowPreview(line []byte) string {
	text := string(line)
	if len(text) > 80 {
		text = text[:80] + "…"
	}
	return traceEscapeText([]byte(text))
}

func printTraceShow(file *traceShowFile, options traceShowOptions) int {
	if file.protocol == traceShowExec {
		return printTraceShowExec(file, options)
	}
	if !validTraceGroupBy(options.groupBy) {
		fmt.Fprintln(os.Stderr, "trace show --group-by must be source, target, port, process, event, or path")
		return 2
	}
	aggregate, summary := traceShowAggregate(file, options)
	duration := file.duration()
	if options.groupBy != "" {
		report := aggregate.groups[options.groupBy].report(summary, duration)
		if options.jsonPath != "" {
			return traceShowJSON(options.jsonPath, report)
		}
		aggregate.printGroupReport(report, summary, duration)
		return 0
	}
	report := aggregate.summary.summarize(summary, duration)
	if options.jsonPath != "" {
		return traceShowJSON(options.jsonPath, report)
	}
	report.print(options.detail)
	return 0
}

func traceShowAggregate(file *traceShowFile, options traceShowOptions) (*traceAggregate, captureSummary) {
	aggregate := newTraceAggregate(file.protocol, "", options.groupBy)
	var kept uint64
	for _, event := range file.events {
		if !traceEventMatches(event, options.process, options.destination) {
			continue
		}
		kept++
		aggregate.observe(event)
	}
	summary := file.summary
	summary.EventCount = kept
	return aggregate, summary
}

func printTraceShowExec(file *traceShowFile, options traceShowOptions) int {
	switch options.groupBy {
	case "", traceGroupByProcess, execGroupByParent, execGroupByCgroup, traceGroupByEvent:
	default:
		fmt.Fprintln(os.Stderr, "trace show --group-by must be process, parent, cgroup, or event for an exec trace")
		return 2
	}
	report := traceShowExecReport(file, options)
	if options.jsonPath != "" {
		return traceShowJSON(options.jsonPath, report)
	}
	report.print()
	return 0
}

func traceShowExecReport(file *traceShowFile, options traceShowOptions) execReport {
	aggregator := newExecAggregator(options.groupBy)
	summary := file.execSum
	summary.EventCount = 0
	for _, event := range file.execs {
		if !execProcessMatches(options.process, event) {
			continue
		}
		summary.EventCount++
		aggregator.add(event)
	}
	return aggregator.report(file.duration(), summary)
}

func traceShowJSON(path string, report interface{}) int {
	if err := writeJSONOutput(path, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}
