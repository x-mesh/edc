package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	traceGroupBySource = "source"
	traceGroupByTarget = "target"
)

func runTrace(args []string) int {
	if len(args) == 0 || (args[0] != "tcp" && args[0] != "udp") {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc trace <tcp|udp> [options]"))
		return 2
	}
	options := tcpTraceOptions{}
	set := flag.NewFlagSet("trace "+args[0], flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	set.BoolVar(&options.live, "live", false, T("command.trace.option.live"))
	set.StringVar(&options.groupBy, "group-by", "", T("command.trace.option.group_by"))
	set.StringVar(&options.process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.destination, "destination", "", T("command.trace.option.destination"))
	set.BoolVar(&options.yes, "yes", false, T("command.trace.option.yes"))
	if err := set.Parse(args[1:]); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace "+args[0]))
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
	if !validTraceGroupBy(options.groupBy) {
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_range"))
		return 2
	}
	if options.raw && options.groupBy != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_raw_conflict"))
		return 2
	}
	if err := captureEventsPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if !options.raw && options.jsonPath == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		return runTraceScreen(args[0], options)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	started := time.Now()
	var events []captureEvent
	var summary captureSummary
	var err error
	var encoder *json.Encoder
	if options.raw {
		encoder = json.NewEncoder(os.Stdout)
	} else if options.jsonPath == "" && options.groupBy == "" {
		printTraceEventHeader(args[0])
	}
	events, summary, err = collectTraceEventsLive(options.duration, func(event captureEvent) error {
		if traceProtocol(event) != args[0] {
			return nil
		}
		if !traceEventMatches(event, options.process, options.destination) {
			return nil
		}
		if options.raw {
			return encoder.Encode(event)
		}
		if options.jsonPath == "" && options.groupBy == "" {
			printTraceEvent(event, true)
		}
		return nil
	}, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", err))
		return 1
	}
	if options.raw {
		if err := encoder.Encode(summary); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	traceDuration := options.duration
	if traceDuration == 0 || ctx.Err() != nil {
		traceDuration = time.Since(started)
	}
	if options.groupBy != "" {
		report := summarizeTraceGroups(args[0], options.groupBy, events, summary, traceDuration, options.process, options.destination)
		if options.jsonPath != "" {
			if err := writeJSONOutput(options.jsonPath, report); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		}
		printTraceGroupReport(report)
		return 0
	}
	if args[0] == "udp" {
		report := summarizeUDPTrace(events, summary, traceDuration, options.process, options.destination)
		if options.jsonPath != "" {
			if err := writeJSONOutput(options.jsonPath, report); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			return 0
		}
		printUDPTraceReport(report)
		return 0
	}
	report := summarizeTCPTrace(events, summary, traceDuration, options.process, options.destination)
	if options.jsonPath != "" {
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	printTCPTraceReport(report)
	return 0
}

func validTraceGroupBy(groupBy string) bool {
	return groupBy == "" || groupBy == traceGroupBySource || groupBy == traceGroupByTarget
}

func printTraceEvent(event captureEvent, color bool) {
	destination := event.Destination
	if event.Target != "" {
		destination += " (" + event.Target + ")"
	}
	if destination == "" {
		destination = "-"
	}
	process := event.Process
	if process == "" {
		process = "-"
	}
	protocol := traceProtocol(event)
	line := fmt.Sprintf("%-16s %-32s %-20s %s", process, destination, event.Event, event.Source)
	fmt.Fprintln(os.Stdout, traceColorLine(line, protocol, event.Event, color))
}

func printTraceEventHeader(protocol string) {
	fmt.Fprintln(os.Stdout, "PROCESS          DESTINATION                       EVENT                SOURCE")
	fmt.Fprintf(os.Stdout, "trace %s  ·  Ctrl-C stop  ·  / filter  ·  q quit\n", protocol)
}

func traceColorLine(line, protocol, event string, color bool) string {
	if !color || os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stdout) {
		return line
	}
	colorValue := "36"
	if protocol == "udp" {
		colorValue = "35"
	}
	if strings.Contains(event, "reset") {
		colorValue = "31"
	} else if strings.Contains(event, "retransmit") || strings.Contains(event, "fail") {
		colorValue = "33"
	}
	return "\033[" + colorValue + "m" + line + "\033[0m"
}

func printTCPTraceReport(report tcpTraceReport) {
	fmt.Fprintf(os.Stdout, "TCP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Attempts: %d\nEstablished: %d\nIncomplete: %d\nRetransmissions: %d\nResets: %d\nTX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\nLost events: %d\n", report.Attempts, report.Established, report.Incomplete, report.Retransmissions, report.Resets, traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic), report.LostEvents)
	if len(report.Connections) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPROCESS\tDESTINATION\tRESULT\tCONNECT\tTX\tRX\tRETRANS\tRESET")
	for _, connection := range report.Connections {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%dms\t%s\t%s\t%d\t%t\n", connection.Process, traceDestinationLabel(connection), connection.Result, connection.ConnectMS, traceBytes(connection.TXBytes), traceBytes(connection.RXBytes), connection.Retransmissions, connection.Reset)
	}
}

type udpTraceFlow struct {
	Process     string `json:"process"`
	PID         uint32 `json:"pid"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Sent        uint64 `json:"sent"`
	Received    uint64 `json:"received"`
	traceTraffic
}

type udpTraceReport struct {
	DurationMS int64          `json:"duration_ms"`
	Datagrams  int            `json:"datagrams"`
	Sent       int            `json:"sent"`
	Received   int            `json:"received"`
	LostEvents uint64         `json:"lost_events"`
	Flows      []udpTraceFlow `json:"flows"`
	traceTraffic
}

func summarizeUDPTrace(events []captureEvent, summary captureSummary, duration time.Duration, process, destination string) udpTraceReport {
	flows := make(map[uint64]*udpTraceFlow)
	result := udpTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents}
	for _, event := range events {
		if traceProtocol(event) != "udp" || !traceEventMatches(event, process, destination) {
			continue
		}
		flow := flows[event.SocketID]
		if flow == nil {
			flow = &udpTraceFlow{Process: event.Process, PID: event.PID, Source: event.Source, Destination: event.Destination, Hostname: event.Target}
			flows[event.SocketID] = flow
		}
		if flow.Process == "" {
			flow.Process = event.Process
		}
		if flow.PID == 0 {
			flow.PID = event.PID
		}
		if flow.Source == "" {
			flow.Source = event.Source
		}
		if flow.Destination == "" {
			flow.Destination = event.Destination
		}
		if flow.Hostname == "" {
			flow.Hostname = event.Target
		}
		switch event.Event {
		case "udp_send":
			flow.Sent++
			result.Sent++
		case "udp_receive":
			flow.Received++
			result.Received++
		}
		flow.traceTraffic.observe(event)
		result.traceTraffic.observe(event)
	}
	result.Datagrams = result.Sent + result.Received
	result.Flows = make([]udpTraceFlow, 0, len(flows))
	for _, flow := range flows {
		flow.traceTraffic.finalize(duration)
		result.Flows = append(result.Flows, *flow)
	}
	result.traceTraffic.finalize(duration)
	sortUDPTraceFlows(result.Flows)
	return result
}

func sortUDPTraceFlows(flows []udpTraceFlow) {
	sort.Slice(flows, func(i, j int) bool {
		if flows[i].Process != flows[j].Process {
			return flows[i].Process < flows[j].Process
		}
		if flows[i].Destination != flows[j].Destination {
			return flows[i].Destination < flows[j].Destination
		}
		return flows[i].Source < flows[j].Source
	})
}

func printUDPTraceReport(report udpTraceReport) {
	fmt.Fprintf(os.Stdout, "UDP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Datagrams: %d\nSent: %d\nReceived: %d\nTX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\nLost events: %d\n", report.Datagrams, report.Sent, report.Received, traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic), report.LostEvents)
	if len(report.Flows) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPROCESS\tDESTINATION\tSENT\tRECEIVED\tTX\tRX")
	for _, flow := range report.Flows {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%d\t%s\t%s\n", flow.Process, traceUDPFlowDestinationLabel(flow), flow.Sent, flow.Received, traceBytes(flow.TXBytes), traceBytes(flow.RXBytes))
	}
}

func traceUDPFlowDestinationLabel(flow udpTraceFlow) string {
	if flow.Hostname == "" {
		return flow.Destination
	}
	return fmt.Sprintf("%s (%s)", flow.Destination, flow.Hostname)
}

func traceDestinationLabel(connection tcpTraceConnection) string {
	if connection.Hostname == "" {
		return connection.Destination
	}
	return fmt.Sprintf("%s (%s)", connection.Destination, connection.Hostname)
}

type traceGroup struct {
	Group           string
	Destinations    []string
	Processes       []string
	Events          uint64
	Tx              uint64
	Rx              uint64
	Connect         uint64
	Retransmissions uint64
	Resets          uint64
	LastEvent       string
	traceTraffic
}

type traceGroupSummary struct {
	Group           string   `json:"group"`
	Destinations    []string `json:"destinations,omitempty"`
	Processes       []string `json:"processes,omitempty"`
	Events          uint64   `json:"events"`
	Rate            float64  `json:"rate_per_second"`
	Tx              uint64   `json:"tx"`
	Rx              uint64   `json:"rx"`
	Connect         uint64   `json:"connect"`
	Retransmissions uint64   `json:"retransmissions"`
	Resets          uint64   `json:"resets"`
	LastEvent       string   `json:"last_event,omitempty"`
	traceTraffic
}

type traceGroupReport struct {
	Protocol   string              `json:"protocol"`
	GroupBy    string              `json:"group_by"`
	DurationMS int64               `json:"duration_ms"`
	Events     uint64              `json:"events"`
	Rate       float64             `json:"rate_per_second"`
	LostEvents uint64              `json:"lost_events"`
	Groups     []traceGroupSummary `json:"groups"`
	traceTraffic
}

func summarizeTraceGroups(protocol, groupBy string, events []captureEvent, summary captureSummary, duration time.Duration, process, destination string) traceGroupReport {
	groups := make(map[string]*traceGroup)
	result := traceGroupReport{Protocol: protocol, GroupBy: groupBy, DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents}
	for _, event := range events {
		if traceProtocol(event) != protocol || !traceEventMatches(event, process, destination) {
			continue
		}
		key := traceGroupKey(event, groupBy)
		group := groups[key]
		if group == nil {
			group = &traceGroup{Group: key}
			groups[key] = group
		}
		observeTraceGroup(group, event)
		result.Events++
		result.traceTraffic.observe(event)
	}
	result.Rate = traceGroupRate(result.Events, duration)
	result.traceTraffic.finalize(duration)
	result.Groups = make([]traceGroupSummary, 0, len(groups))
	for _, group := range groups {
		group.traceTraffic.finalize(duration)
		result.Groups = append(result.Groups, traceGroupSummary{
			Group: group.Group, Destinations: append([]string(nil), group.Destinations...), Processes: append([]string(nil), group.Processes...),
			Events: group.Events, Rate: traceGroupRate(group.Events, duration), Tx: group.Tx, Rx: group.Rx, Connect: group.Connect,
			Retransmissions: group.Retransmissions, Resets: group.Resets, LastEvent: group.LastEvent, traceTraffic: group.traceTraffic,
		})
	}
	for index := range result.Groups {
		sort.Strings(result.Groups[index].Destinations)
		sort.Strings(result.Groups[index].Processes)
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].Group < result.Groups[j].Group })
	return result
}

func traceGroupKey(event captureEvent, groupBy string) string {
	if groupBy == traceGroupBySource {
		if event.Source == "" {
			return "-"
		}
		// source port는 연결마다 OS가 새로 고르는 ephemeral port라서, 포함하면 같은 host의 연결이
		// 모두 다른 group이 된다.
		if host, _, err := net.SplitHostPort(event.Source); err == nil {
			return host
		}
		return event.Source
	}
	if event.Target != "" {
		return event.Target
	}
	if event.Destination != "" {
		return event.Destination
	}
	return "-"
}

func observeTraceGroup(group *traceGroup, event captureEvent) {
	group.Events++
	if event.Destination != "" {
		group.Destinations = appendUniqueTraceValue(group.Destinations, event.Destination)
	}
	if event.Process != "" {
		group.Processes = appendUniqueTraceValue(group.Processes, event.Process)
	}
	switch event.Event {
	case "udp_send":
		group.Tx++
	case "udp_receive":
		group.Rx++
	case "tcp_connect", "tcp_accept":
		group.Connect++
	case "tcp_retransmit":
		group.Retransmissions++
	case "tcp_send_reset", "tcp_receive_reset":
		group.Resets++
	}
	group.traceTraffic.observe(event)
	group.LastEvent = event.Event
}

func appendUniqueTraceValue(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func traceGroupRate(events uint64, duration time.Duration) float64 {
	seconds := duration.Seconds()
	if seconds <= 0 {
		return float64(events)
	}
	return float64(events) / seconds
}

func traceGroupDisplayValue(group traceGroupSummary) string {
	if len(group.Destinations) == 0 || group.Destinations[0] == group.Group {
		return group.Group
	}
	return fmt.Sprintf("%s (%s)", group.Group, group.Destinations[0])
}

func traceGroupLabel(groupBy string) string {
	return strings.ToUpper(groupBy)
}

func printTraceGroupReport(report traceGroupReport) {
	fmt.Fprintf(os.Stdout, "%s trace grouped by %s: %s\n\n", strings.ToUpper(report.Protocol), report.GroupBy, (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Events: %d\n%s groups: %d\nEvent rate: %.1f/s\nTX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\nLost events: %d\n", report.Events, report.GroupBy, len(report.Groups), report.Rate, traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic), report.LostEvents)
	if len(report.Groups) == 0 {
		return
	}
	if report.Protocol == "udp" {
		fmt.Fprintf(os.Stdout, "\n%s\tEVENTS\tEVENT/s\tTX\tRX\tTOTAL\tB/s\tMbps\tLAST\n", traceGroupLabel(report.GroupBy))
		for _, group := range report.Groups {
			fmt.Fprintf(os.Stdout, "%s\t%d\t%.1f\t%s\t%s\t%s\t%s\t%.3f\t%s\n", traceGroupDisplayValue(group), group.Events, group.Rate, traceBytes(group.TXBytes), traceBytes(group.RXBytes), traceBytes(group.TotalBytes), traceBytes(uint64(group.BytesPerSecond)), group.MegabitsPerSecond, group.LastEvent)
		}
		return
	}
	fmt.Fprintf(os.Stdout, "\n%s\tEVENTS\tEVENT/s\tTX\tRX\tTOTAL\tB/s\tMbps\tCONNECT\tRETRANS\tRESET\tLAST\n", traceGroupLabel(report.GroupBy))
	for _, group := range report.Groups {
		fmt.Fprintf(os.Stdout, "%s\t%d\t%.1f\t%s\t%s\t%s\t%s\t%.3f\t%d\t%d\t%d\t%s\n", traceGroupDisplayValue(group), group.Events, group.Rate, traceBytes(group.TXBytes), traceBytes(group.RXBytes), traceBytes(group.TotalBytes), traceBytes(uint64(group.BytesPerSecond)), group.MegabitsPerSecond, group.Connect, group.Retransmissions, group.Resets, group.LastEvent)
	}
}

func traceBytes(bytes uint64) string {
	const kib = 1024
	const mib = 1024 * kib
	if bytes >= mib {
		return fmt.Sprintf("%.1fMiB", float64(bytes)/mib)
	}
	if bytes >= kib {
		return fmt.Sprintf("%.1fKiB", float64(bytes)/kib)
	}
	return fmt.Sprintf("%dB", bytes)
}

func traceTrafficRate(traffic traceTraffic) string {
	return fmt.Sprintf("%.1f B/s · %.1f bps · %.3f Mbps", traffic.BytesPerSecond, traffic.BitsPerSecond, traffic.MegabitsPerSecond)
}
