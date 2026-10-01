package edc

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	traceGroupBySource  = "source"
	traceGroupByTarget  = "target"
	traceGroupByPort    = "port"
	traceGroupByProcess = "process"
	traceGroupByEvent   = "event"
)

// traceProtocolSpec은 trace가 protocol마다 다르게 보여 주는 부분이다. 수집, filter, group 묶기는 protocol과 상관없이 같다.
type traceProtocolSpec struct {
	// ansiColor는 스크롤 출력의 ANSI 색 번호이고 screenColor는 전체 화면에서 쓰는 같은 색이다.
	ansiColor   string
	screenColor string
	// groupColumns는 group 행에서 traffic 열 뒤, LAST 앞에 붙는 열이다.
	groupColumns []traceGroupColumn
	// hideTraffic은 byte 열과 traffic 줄을 뺀다. DNS는 질의 수와 응답 시간이 byte보다 쓸모 있다.
	hideTraffic bool
	// hiddenViews는 이 protocol에서 의미가 없는 group 보기다. DNS 서버 port는 늘 53이라 port 보기는 한 행뿐이다.
	hiddenViews []string
	// linuxOnly는 macOS가 관측하지 못하는 protocol이다. macOS의 network statistics는 payload를 주지 않는다.
	linuxOnly bool
	// scrollLabels는 스크롤 행의 목적지 칸과 event 칸이다. nil이면 목적지와 event 이름을 쓴다.
	scrollLabels func(event captureEvent) (string, string)
	// serverSide는 --side server로 로컬 서버 쪽을 볼 수 있는 protocol이다.
	serverSide bool
	// prerequisites는 수집 전에 확인할 조건이다. nil이면 확인할 것이 없다. ARP는 netlink만 쓰므로 eBPF 권한이 필요 없다.
	prerequisites func() error
	newSummarizer func() traceSummarizer
}

type traceProtocolView struct {
	selectorLabel string
}

type traceProtocolRunner func([]string) int

type traceProtocolRegistration struct {
	name string
	view traceProtocolView
	spec traceProtocolSpec
	run  traceProtocolRunner
}

// traceGroupColumn은 group 행의 protocol 전용 열이다. 전체 화면은 좁은 title을, 끝난 뒤 출력하는 표는 긴 title을 쓴다.
type traceGroupColumn struct {
	screenTitle string
	reportTitle string
	width       int
	value       func(group traceGroupSummary) string
}

// traceSummarizer는 --group-by 없이 끝난 trace의 기본 요약을 쌓는다. 행의 단위가 protocol마다 다르다.
type traceSummarizer interface {
	observe(event captureEvent)
	summarize(summary captureSummary, duration time.Duration) traceReport
}

// traceReport의 print는 detail이 false면 process와 상대별로 묶은 행을, true면 연결이나 flow마다 한 행을 쓴다.
type traceReport interface{ print(detail bool) }

var traceProtocolRegistry = []traceProtocolRegistration{
	{name: "tcp", view: traceProtocolView{selectorLabel: "tcp"}, spec: traceProtocolSpec{
		ansiColor: "36", screenColor: "#22d3ee",
		groupColumns: []traceGroupColumn{
			{screenTitle: "CON", reportTitle: "CONNECT", width: 3, value: func(group traceGroupSummary) string { return strconv.FormatUint(group.Connect, 10) }},
			{screenTitle: "RET", reportTitle: "RETRANS", width: 3, value: func(group traceGroupSummary) string { return traceOptional(group.Retransmissions, "%d") }},
			{screenTitle: "RST", reportTitle: "RESET", width: 3, value: func(group traceGroupSummary) string { return traceOptional(group.Resets, "%d") }},
		},
		prerequisites: captureEventsPrerequisites, newSummarizer: func() traceSummarizer { return newTCPTraceSummarizer() },
	}},
	{name: "udp", view: traceProtocolView{selectorLabel: "udp"}, spec: traceProtocolSpec{ansiColor: "35", screenColor: "#c084fc", prerequisites: captureEventsPrerequisites, newSummarizer: func() traceSummarizer { return newUDPTraceSummarizer() }}},
	{name: "dns", view: traceProtocolView{selectorLabel: "dns"}, spec: traceProtocolSpec{
		ansiColor: "32", screenColor: "#4ade80", groupColumns: traceDNSGroupColumns, hideTraffic: true, hiddenViews: []string{traceGroupByPort}, linuxOnly: true,
		scrollLabels: traceDNSScrollLabels, serverSide: true, prerequisites: captureEventsPrerequisites, newSummarizer: func() traceSummarizer { return newDNSTraceSummarizer() },
	}},
	{name: "arp", view: traceProtocolView{selectorLabel: "arp"}, spec: traceProtocolSpec{
		ansiColor: "34", screenColor: "#60a5fa", groupColumns: traceNeighborGroupColumns, hideTraffic: true, hiddenViews: []string{traceGroupByPort, traceGroupByProcess},
		scrollLabels: traceNeighborScrollLabels, newSummarizer: func() traceSummarizer { return newNeighborTraceSummarizer("arp") },
	}},
	{name: "ndp", view: traceProtocolView{selectorLabel: "ndp"}, spec: traceProtocolSpec{
		ansiColor: "34", screenColor: "#60a5fa", groupColumns: traceNeighborGroupColumns, hideTraffic: true, hiddenViews: []string{traceGroupByPort, traceGroupByProcess},
		scrollLabels: traceNeighborScrollLabels, newSummarizer: func() traceSummarizer { return newNeighborTraceSummarizer("ndp") },
	}},
	// 평문 HTTP/1.x만 본다. TLS 안의 HTTP는 kernel에서 암호문이다.
	{name: "http", view: traceProtocolView{selectorLabel: "http"}, spec: traceProtocolSpec{
		ansiColor: "95", screenColor: "#f472b6", groupColumns: traceHTTPGroupColumns, hideTraffic: true, linuxOnly: true,
		scrollLabels: traceHTTPScrollLabels, serverSide: true, prerequisites: httpTracePrerequisites, newSummarizer: func() traceSummarizer { return newHTTPTraceSummarizer() },
	}},
	// 평문 MySQL만 본다. TLS 안의 packet은 kernel에서 암호문이다. 색은 다른 protocol이 쓰지 않는 값이다.
	{name: "mysql", view: traceProtocolView{selectorLabel: "mysql"}, spec: traceProtocolSpec{
		ansiColor: "94", screenColor: "#818cf8", groupColumns: traceMySQLGroupColumns, hideTraffic: true, linuxOnly: true,
		scrollLabels: traceMySQLScrollLabels, serverSide: true, prerequisites: httpTracePrerequisites, newSummarizer: func() traceSummarizer { return newMySQLTraceSummarizer() },
	}},
	{name: "io", view: traceProtocolView{selectorLabel: "io"}, spec: traceProtocolSpec{ansiColor: "33", screenColor: "#fbbf24", linuxOnly: true, prerequisites: ioTracePrerequisites}, run: runIOTrace},
	// unix socket 파일 하나를 본다. 목적지는 늘 그 경로라서 port와 target 보기는 한 행뿐이다. source는 상대 process다.
	{name: "socket", view: traceProtocolView{selectorLabel: "socket"}, spec: traceProtocolSpec{
		ansiColor: "38;5;208", screenColor: "#fb923c", hiddenViews: []string{traceGroupByPort, traceGroupByTarget}, linuxOnly: true,
		scrollLabels: traceSocketScrollLabels, prerequisites: socketTracePrerequisites, newSummarizer: func() traceSummarizer { return newSocketTraceSummarizer() },
	}},
	// kernel이 버린 패킷이다. event 칸은 버린 이유다. process는 패킷에 local socket이 있을 때만 붙는다.
	{name: "drop", view: traceProtocolView{selectorLabel: "drop"}, spec: traceProtocolSpec{
		ansiColor: "91", screenColor: "#f87171", hideTraffic: true, linuxOnly: true,
		scrollLabels: traceDropScrollLabels, prerequisites: dropTracePrerequisites, newSummarizer: func() traceSummarizer { return newDropTraceSummarizer() },
	}},
}

var traceProtocols = traceProtocolSpecs(traceProtocolRegistry)

func traceProtocolSpecs(registry []traceProtocolRegistration) map[string]traceProtocolSpec {
	specs := make(map[string]traceProtocolSpec, len(registry))
	for _, protocol := range registry {
		if protocol.name == "" {
			panic("trace protocol name is required")
		}
		if _, exists := specs[protocol.name]; exists {
			panic("trace protocol is already registered: " + protocol.name)
		}
		specs[protocol.name] = protocol.spec
	}
	return specs
}

func registerTraceProtocol(protocol traceProtocolRegistration) {
	if protocol.name == "" {
		panic("trace protocol name is required")
	}
	if _, exists := traceProtocols[protocol.name]; exists {
		panic("trace protocol is already registered: " + protocol.name)
	}
	traceProtocolRegistry = append(traceProtocolRegistry, protocol)
	traceProtocols[protocol.name] = protocol.spec
}

func traceProtocolRegistrationFor(name string) (traceProtocolRegistration, bool) {
	for _, protocol := range traceProtocolRegistry {
		if protocol.name == name {
			return protocol, true
		}
	}
	return traceProtocolRegistration{}, false
}

func traceProtocolSelectItems() []selectItem {
	items := make([]selectItem, 0, len(traceProtocolRegistry))
	for _, protocol := range traceProtocolRegistry {
		label := protocol.view.selectorLabel
		if label == "" {
			label = protocol.name
		}
		items = append(items, selectItem{label: label, value: protocol.name})
	}
	return items
}

func traceProtocolPrerequisites(protocol string) error {
	if check := traceProtocols[protocol].prerequisites; check != nil {
		return check()
	}
	return nil
}

// traceGroupViews는 protocol이 쓰는 보기를 Tab 순서로 돌려준다. 첫 값은 event 스크롤이다.
func traceGroupViews(protocol string) []string {
	hidden := traceProtocols[protocol].hiddenViews
	return slices.DeleteFunc(slices.Clone(traceGroupCycle), func(view string) bool { return slices.Contains(hidden, view) })
}

func traceScrollLabels(event captureEvent) (string, string) {
	if labels := traceProtocols[traceProtocol(event)].scrollLabels; labels != nil {
		return labels(event)
	}
	return traceEventDestinationLabel(event), event.Event
}

// traceScope는 collector가 모을 범위다. server는 --side server로 로컬 서버 쪽을 볼 때 켠다.
type traceScope struct {
	protocol string
	server   bool
	// side는 --side 값이다. trace http는 비어 있으면 두 쪽을 모두 본다. trace dns는 server만 쓴다.
	side string
	// payload가 꺼져 있으면 message 앞부분을 event에 붙이지 않는다. 화면은 event를 최대 10,000건 보관한다.
	payload bool
	// payloadAll은 --payload=all이다. 이어지는 조각까지 받아 message 전체를 event에 붙인다.
	payloadAll  bool
	showSecrets bool
	// keepGzip이면 gzip message의 원본 byte를 event에 붙인다. 전체 화면의 상세 보기가 본문을 풀 때 쓴다.
	keepGzip bool
	port     uint16
	// socketPath는 trace socket이 볼 unix socket 파일이다.
	socketPath string
	// dropReasons는 trace drop이 event로 볼 이유 이름이다. 비어 있으면 모든 이유를 본다.
	dropReasons []string
}

func (options tcpTraceOptions) scope(protocol string) traceScope {
	return traceScope{protocol: protocol, server: options.side == traceServerSide, side: options.side, payload: options.payload != "", payloadAll: options.payload == tracePayloadAll,
		showSecrets: options.showSecrets, port: uint16(options.port), socketPath: options.socketPath, dropReasons: splitDropReasons(options.dropReasons)}
}

// tracePayloadMode는 --payload 값이다. 값 없이 쓰면 message마다 앞 4KiB를, all이면 message 전체를 본다.
type tracePayloadMode string

const (
	tracePayloadHead tracePayloadMode = "head"
	tracePayloadAll  tracePayloadMode = "all"
)

func (mode *tracePayloadMode) String() string {
	if mode == nil {
		return ""
	}
	return string(*mode)
}

func (mode *tracePayloadMode) Set(value string) error {
	switch value {
	case "true":
		*mode = tracePayloadHead
	case "false":
		*mode = ""
	case string(tracePayloadAll):
		*mode = tracePayloadAll
	default:
		return errors.New(T("cli.trace.payload_value"))
	}
	return nil
}

// IsBoolFlag가 있어야 flag package가 값 없는 --payload를 받는다. 그래서 값은 --payload=all처럼 붙여 써야 한다.
func (mode *tracePayloadMode) IsBoolFlag() bool { return true }

// traceLabel은 화면 머리글에 쓰는 trace 이름이다. 서버 쪽 trace는 client 쪽과 같은 event 이름을 쓰므로 머리글로 구분한다.
// trace http는 기본으로 두 쪽을 모두 보므로, client만 고른 것도 머리글에 쓴다.
func traceLabel(protocol, side string) string {
	if side == traceServerSide || (side == traceClientSide && (protocol == "http" || protocol == "mysql")) {
		return protocol + " --side " + side
	}
	return protocol
}

// traceClientSide와 traceServerSide는 --side 값이다. 서버 쪽 event와 요약에는 Side가 server로 붙는다. HTTP event에는
// client 쪽에도 Side가 붙는다. 한 trace에 두 쪽이 섞이기 때문이다.
const (
	traceClientSide = "client"
	traceServerSide = "server"
)

// traceSpan은 두 monotonic 시각 사이의 밀리초다. CPU마다 읽은 시각이 조금 어긋나도 음수로 내지 않는다.
func traceSpan(start, end uint64) *float64 {
	span := float64(end-min(end, start)) / float64(time.Millisecond)
	return &span
}

// traceSpans는 시간 값의 합, 개수, 최댓값이다.
type traceSpans struct {
	total   float64
	count   uint64
	maximum float64
}

func (spans *traceSpans) observe(value *float64) {
	if value == nil {
		return
	}
	spans.total += *value
	spans.count++
	if *value > spans.maximum {
		spans.maximum = *value
	}
}

func (spans traceSpans) summary() (*float64, *float64) {
	if spans.count == 0 {
		return nil, nil
	}
	average, maximum := spans.total/float64(spans.count), spans.maximum
	return &average, &maximum
}

// traceLatency는 밀리초 값을 소수 한 자리로 쓴다. 값이 없으면 -다.
func traceLatency(latency *float64, unit string) string {
	if latency == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%s", *latency, unit)
}

// traceResultCounts는 결과별 개수를 많은 순서로 잇는다. 예: "noerror 3, nxdomain 1", "200 5, 404 1".
func traceResultCounts(results map[string]uint64) string {
	names := slices.Collect(maps.Keys(results))
	sort.Slice(names, func(i, j int) bool {
		if results[names[i]] != results[names[j]] {
			return results[names[i]] > results[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d", name, results[name]))
	}
	return emptyAs(strings.Join(parts, ", "), "-")
}

func knownTraceProtocol(protocol string) bool {
	_, ok := traceProtocols[protocol]
	return ok
}

func (summarizer *tcpTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	return summarizer.report(summary, duration)
}

func (report tcpTraceReport) print(detail bool) { printTCPTraceReport(report, detail) }

func (summarizer *udpTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	return summarizer.report(summary, duration)
}

func (report udpTraceReport) print(detail bool) { printUDPTraceReport(report, detail) }

func (report traceGroupReport) hideTraffic() bool { return traceProtocols[report.Protocol].hideTraffic }

var traceIsTerminal = isTerminal
var runTraceProtocolSelector = selectTraceProtocol

func runTrace(args []string) int {
	if len(args) == 0 {
		if !traceIsTerminal(os.Stdin) || !traceIsTerminal(os.Stdout) {
			traceUsage()
			return 2
		}
		protocol, err := runTraceProtocolSelector(os.Stdin, os.Stdout, traceProtocolSelectItems())
		if err != nil {
			traceUsage()
			return 2
		}
		args = []string{protocol}
	}
	registration, ok := traceProtocolRegistrationFor(args[0])
	if !ok {
		traceUsage()
		return 2
	}
	if registration.run != nil {
		return registration.run(args[1:])
	}
	return runTraceProtocol(args)
}

func traceUsage() {
	fmt.Fprintln(os.Stderr, T("cli.usage", "edc trace <tcp|udp|dns|arp|ndp|http|drop|mysql|io> [options]"))
	fmt.Fprintln(os.Stderr, T("cli.usage", traceSocketUsage))
}

func runTraceProtocol(args []string) int {
	options := tcpTraceOptions{}
	set := flag.NewFlagSet("trace "+args[0], flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	set.DurationVar(&options.duration, "duration", 0, T("command.trace.option.duration"))
	set.DurationVar(&options.slow, "slow", 0, T("command.trace.option.slow"))
	set.StringVar(&options.jsonPath, "json", "", T("option.json"))
	set.BoolVar(&options.raw, "raw", false, T("command.trace.option.raw"))
	set.BoolVar(&options.live, "live", false, T("command.trace.option.live"))
	set.StringVar(&options.groupBy, "group-by", "", T("command.trace.option.group_by"))
	set.StringVar(&options.process, "process", "", T("command.trace.option.process"))
	set.StringVar(&options.destination, "destination", "", T("command.trace.option.destination"))
	set.BoolVar(&options.detail, "detail", false, T("command.trace.option.detail"))
	set.BoolVar(&options.detail, "d", false, T("command.trace.option.detail"))
	set.BoolVar(&options.yes, "yes", false, T("command.trace.option.yes"))
	// 기본값은 비워 둔다. trace http는 두 쪽을 모두, trace dns는 client 쪽만 본다.
	set.StringVar(&options.side, "side", "", T("command.trace.option.side"))
	set.Var(&options.payload, "payload", T("command.trace.option.payload"))
	set.BoolVar(&options.showSecrets, "show-secrets", false, T("command.trace.option.show_secrets"))
	set.IntVar(&options.port, "port", 0, T("command.trace.option.port"))
	set.StringVar(&options.containerRef, "container", "", T("command.trace.option.container"))
	set.StringVar(&options.dropReasons, "reason", "", T("command.trace.option.reason"))
	if err := set.Parse(args[1:]); err != nil {
		return 2
	}
	if args[0] == "socket" {
		// flag package는 첫 위치 인자에서 멈춘다. 경로 뒤의 option도 받도록 경로를 꺼내고 나머지를 다시 읽는다.
		if set.NArg() == 0 {
			fmt.Fprintln(os.Stderr, T("cli.usage", traceSocketUsage))
			return 2
		}
		options.socketPath = set.Arg(0)
		if err := set.Parse(set.Args()[1:]); err != nil {
			return 2
		}
		if set.NArg() != 0 {
			fmt.Fprintln(os.Stderr, T("cli.usage", traceSocketUsage))
			return 2
		}
	} else if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace "+args[0]))
		return 2
	}
	if options.duration < 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	slowSet := false
	set.Visit(func(option *flag.Flag) { slowSet = slowSet || option.Name == "slow" })
	if slowSet && options.slow <= 0 {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_range"))
		return 2
	}
	if slowSet && args[0] != "mysql" {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_protocol"))
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
	if options.groupBy != "" && !slices.Contains(traceGroupViews(args[0]), options.groupBy) {
		fmt.Fprintln(os.Stderr, T("cli.trace.group_by_protocol", options.groupBy, args[0]))
		return 2
	}
	if options.side != "" && options.side != traceClientSide && options.side != traceServerSide {
		fmt.Fprintln(os.Stderr, T("cli.trace.side_range"))
		return 2
	}
	if options.side == traceServerSide && !traceProtocols[args[0]].serverSide {
		fmt.Fprintln(os.Stderr, T("cli.trace.side_protocol", args[0]))
		return 2
	}
	if options.payload != "" && args[0] != "http" && args[0] != "socket" {
		fmt.Fprintln(os.Stderr, T("cli.trace.payload_protocol", args[0]))
		return 2
	}
	// --json은 요약만 쓰므로 event에 붙인 payload가 어디에도 나오지 않는다.
	if options.payload != "" && options.jsonPath != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.payload_json_conflict"))
		return 2
	}
	if options.showSecrets && options.payload == "" && args[0] != "mysql" {
		fmt.Fprintln(os.Stderr, T("cli.trace.show_secrets_payload"))
		return 2
	}
	// socket의 payload는 형식을 몰라 가리지 않는다. 가릴 것이 없으므로 받지 않는다.
	if options.showSecrets && args[0] == "socket" {
		fmt.Fprintln(os.Stderr, T("cli.trace.show_secrets_protocol", args[0]))
		return 2
	}
	portSet := false
	set.Visit(func(option *flag.Flag) { portSet = portSet || option.Name == "port" })
	if portSet && (options.port < 1 || options.port > 65535) {
		fmt.Fprintln(os.Stderr, T("cli.trace.port_range"))
		return 2
	}
	if portSet && args[0] != "http" && args[0] != "mysql" {
		fmt.Fprintln(os.Stderr, T("cli.trace.port_protocol", args[0]))
		return 2
	}
	// ARP와 NDP event는 netlink에서 와서 process가 없으므로 process 보기도 없다. cgroup ID도 없다.
	if options.dropReasons != "" && args[0] != "drop" {
		fmt.Fprintln(os.Stderr, T("cli.trace.reason_protocol", args[0]))
		return 2
	}
	if options.containerRef != "" && slices.Contains(traceProtocols[args[0]].hiddenViews, traceGroupByProcess) {
		fmt.Fprintln(os.Stderr, T("cli.trace.container_protocol", args[0]))
		return 2
	}
	if traceProtocols[args[0]].linuxOnly && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, T("cli.trace.protocol_linux_only", args[0], runtime.GOOS))
		return 3
	}
	if options.containerRef != "" && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, T("cli.trace.container_linux_only", runtime.GOOS))
		return 3
	}
	if args[0] == "socket" {
		if err := validateSocketTarget(options.socketPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if slowSet && (options.raw || options.jsonPath != "" || options.groupBy != "" || !traceIsTerminal(os.Stdin) || !traceIsTerminal(os.Stdout)) {
		fmt.Fprintln(os.Stderr, T("cli.trace.slow_interactive"))
		return 2
	}
	if err := traceProtocolPrerequisites(args[0]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	// 이유 이름은 kernel BTF에서 읽으므로 prerequisite 뒤에 맞춘다.
	if args[0] == "drop" {
		if err := validateDropReasons(splitDropReasons(options.dropReasons)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	// 권한이 없으면 docker 권한 오류보다 capability 안내가 먼저 보이도록 prerequisite 뒤에 푼다.
	if options.containerRef != "" {
		container, code, err := resolveTraceContainer(options.containerRef)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return code
		}
		options.container = container
	}
	if !options.raw && options.jsonPath == "" && traceIsTerminal(os.Stdin) && traceIsTerminal(os.Stdout) {
		return runTraceScreen(args[0], options)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	started := time.Now()
	var summary captureSummary
	var err error
	var encoder *json.Encoder
	var written uint64
	aggregate := newTraceAggregate(args[0], options.groupBy)
	if options.raw {
		encoder = json.NewEncoder(os.Stdout)
	} else if options.jsonPath == "" && options.groupBy == "" {
		printTraceEventHeader(traceLabel(args[0], options.side))
	}
	summary, err = collectTraceEventsLive(options.scope(args[0]), options.duration, func(event captureEvent) error {
		if traceProtocol(event) != args[0] {
			return nil
		}
		if !options.matches(event) {
			return nil
		}
		if options.raw {
			written++
			return encoder.Encode(event)
		}
		aggregate.observe(event)
		if options.jsonPath == "" && options.groupBy == "" {
			printTraceEvent(event, true, options.payload == tracePayloadAll)
		}
		return nil
	}, ctx.Done())
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", err))
		return 1
	}
	if options.raw {
		// collector는 다른 protocol의 event도 받아 센다. 요약의 event 수는 이 출력에 쓴 줄 수와 같아야 한다.
		summary.EventCount = written
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
		report := aggregate.groups[options.groupBy].report(summary, traceDuration)
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
	report := aggregate.summary.summarize(summary, traceDuration)
	if options.jsonPath != "" {
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	report.print(options.detail)
	return 0
}

func validTraceGroupBy(groupBy string) bool {
	return groupBy == "" || groupBy == traceGroupBySource || groupBy == traceGroupByTarget || groupBy == traceGroupByPort || groupBy == traceGroupByProcess || groupBy == traceGroupByEvent
}

// traceEventDestinationLabel은 목적지 뒤에 명령줄에서 얻은 target을 붙인다. 두 이벤트 화면이 같은 표시를 쓴다.
func traceEventDestinationLabel(event captureEvent) string {
	destination := event.Destination
	if event.Target != "" {
		destination += " (" + event.Target + ")"
	}
	if destination == "" {
		destination = "-"
	}
	return destination
}

// full이면 --payload=all이라 message 전체를 event 아래 여러 줄로 쓴다. 한 줄 요약은 긴 message를 읽기 어렵다.
func printTraceEvent(event captureEvent, color, full bool) {
	destination, name := traceScrollLabels(event)
	process := event.Process
	if process == "" {
		process = "-"
	}
	protocol := traceProtocol(event)
	line := fmt.Sprintf("%-16s %-32s %-20s %s", process, destination, name, event.Source)
	fmt.Fprintln(os.Stdout, traceColorLine(line, protocol, event.Event, color))
	switch {
	case event.Payload == "":
	case full:
		for _, payloadLine := range traceHTTPPayloadBlock(event) {
			fmt.Fprintf(os.Stdout, "%-16s │ %s\n", "", payloadLine)
		}
	default:
		fmt.Fprintf(os.Stdout, "%-16s ↳ %s\n", "", tracePayloadSummary(event))
	}
}

// tracePayloadSummary는 event 아래 한 줄에 쓰는 payload다. socket의 payload는 형식을 몰라 HTTP처럼 머리와 본문으로
// 나누지 않는다.
func tracePayloadSummary(event captureEvent) string {
	if traceProtocol(event) == "socket" {
		return traceSocketPayloadLine(event)
	}
	return traceHTTPPayloadSummary(event)
}

func printTraceEventHeader(protocol string) {
	fmt.Fprintln(os.Stdout, "PROCESS          DESTINATION                       EVENT                SOURCE")
	// 줄 단위 출력은 키 입력을 받지 않는다. 전체 화면에만 있는 / filter와 q quit은 안내하지 않는다.
	fmt.Fprintf(os.Stdout, "trace %s  ·  Ctrl-C stop\n", protocol)
}

func traceColorLine(line, protocol, event string, color bool) string {
	if !color || os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stdout) {
		return line
	}
	colorValue := traceProtocols[protocol].ansiColor
	if strings.Contains(event, "reset") || strings.HasSuffix(event, "_"+traceNeighborMACChange) || event == "http_5xx" {
		colorValue = "31"
	} else if strings.Contains(event, "retransmit") || strings.Contains(event, "fail") {
		colorValue = "33"
	}
	return "\033[" + colorValue + "m" + line + "\033[0m"
}

func printTCPTraceReport(report tcpTraceReport, detail bool) {
	fmt.Fprintf(os.Stdout, "TCP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Attempts: %d\nEstablished: %d\nIncomplete: %d\nExisting: %d\nRetransmissions: %s\nResets: %s\nTX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\nLost events: %d\n", report.Attempts, report.Established, report.Incomplete, report.Existing, traceOptional(report.Retransmissions, "%d"), traceOptional(report.Resets, "%d"), traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic), report.LostEvents)
	if !detail {
		printTCPTraceGroups(report.groups)
		return
	}
	if len(report.Connections) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPROCESS\tDESTINATION\tRESULT\tCONNECT\tTX\tRX\tRETRANS\tRESET")
	for _, connection := range report.Connections {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", connection.Process, traceDestinationLabel(connection), connection.Result, traceOptional(connection.ConnectMS, "%dms"), traceBytes(connection.TXBytes), traceBytes(connection.RXBytes), traceOptional(connection.Retransmissions, "%d"), traceOptional(connection.Reset, "%t"))
	}
	if report.ConnectionsOmitted > 0 {
		fmt.Fprintf(os.Stdout, "%d earlier closed connections are not listed. The totals include them.\n", report.ConnectionsOmitted)
	}
}

func printTCPTraceGroups(rows []tcpTraceGroupRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPROCESS\tDESTINATION\tCONNS\tEST\tINC\tEXIST\tCONNECT\tTX\tRX\tRETRANS\tRESET")
	for _, row := range rows {
		connect := "-"
		if row.connectCount > 0 {
			connect = traceOptional(traceObserved(row.connectTotalMS/int64(row.connectCount)), "%dms")
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", row.process, traceSummaryPeerLabel(row.peer, row.hostname, row.server), row.connections, row.established, row.incomplete, row.existing, connect, traceBytes(row.TXBytes), traceBytes(row.RXBytes), traceOptional(traceObserved(row.retransmissions), "%d"), traceOptional(traceObserved(row.resets), "%d"))
	}
}

func traceSummaryPeerLabel(peer, hostname string, server bool) string {
	switch {
	case server:
		return peer + traceServerSuffix
	case hostname != "":
		return fmt.Sprintf("%s (%s)", peer, hostname)
	default:
		return peer
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
	// groups는 텍스트 요약의 기본 행이다. JSON은 flow별 행만 쓴다.
	groups []udpTraceGroupRow
	traceTraffic
}

// udpTraceGroupRow는 같은 process가 같은 상대와 주고받은 flow를 묶은 행이다. socket이 달라도 합치고, 서버
// socket은 client 포트가 매번 달라서 local 서비스로 묶는다.
type udpTraceGroupRow struct {
	process  string
	peer     string
	hostname string
	server   bool
	sent     uint64
	received uint64
	traceTraffic
}

func summarizeUDPTrace(events []captureEvent, summary captureSummary, duration time.Duration, process, destination string) udpTraceReport {
	summarizer := newUDPTraceSummarizer()
	for _, event := range events {
		if traceProtocol(event) != "udp" || !traceEventMatches(event, process, destination) {
			continue
		}
		summarizer.observe(event)
	}
	return summarizer.report(summary, duration)
}

// 연결하지 않은 UDP socket 하나가 여러 곳과 주고받으므로 socket만으로 묶으면 목적지가 섞인다.
type udpFlowKey struct {
	socket      uint64
	destination string
}

type udpTraceSummarizer struct {
	flows    map[udpFlowKey]*udpTraceFlow
	sent     int
	received int
	traffic  traceTraffic
}

func newUDPTraceSummarizer() *udpTraceSummarizer {
	return &udpTraceSummarizer{flows: make(map[udpFlowKey]*udpTraceFlow)}
}

func (summarizer *udpTraceSummarizer) observe(event captureEvent) {
	key := udpFlowKey{socket: event.SocketID, destination: event.Destination}
	flow := summarizer.flows[key]
	if flow == nil {
		flow = &udpTraceFlow{Process: event.Process, PID: event.PID, Source: event.Source, Destination: event.Destination, Hostname: event.Target}
		summarizer.flows[key] = flow
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
	if flow.Hostname == "" {
		flow.Hostname = event.Target
	}
	switch event.Event {
	case "udp_send":
		flow.Sent += traceEventPackets(event)
		summarizer.sent += int(traceEventPackets(event))
	case "udp_receive":
		flow.Received += traceEventPackets(event)
		summarizer.received += int(traceEventPackets(event))
	}
	flow.traceTraffic.observe(event)
	summarizer.traffic.observe(event)
}

func (summarizer *udpTraceSummarizer) report(summary captureSummary, duration time.Duration) udpTraceReport {
	result := udpTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, Sent: summarizer.sent, Received: summarizer.received, traceTraffic: summarizer.traffic}
	result.Datagrams = result.Sent + result.Received
	result.Flows = make([]udpTraceFlow, 0, len(summarizer.flows))
	for _, stored := range summarizer.flows {
		flow := *stored
		flow.traceTraffic.finalize(duration)
		result.Flows = append(result.Flows, flow)
	}
	result.traceTraffic.finalize(duration)
	sortUDPTraceFlows(result.Flows)
	result.groups = groupUDPTraceFlows(result.Flows, duration)
	return result
}

func groupUDPTraceFlows(flows []udpTraceFlow, duration time.Duration) []udpTraceGroupRow {
	type key struct {
		process string
		peer    string
		server  bool
	}
	groups := map[key]*udpTraceGroupRow{}
	for _, flow := range flows {
		peer, server := traceSummaryPeer(flow.Source, flow.Destination)
		row := groups[key{flow.Process, peer, server}]
		if row == nil {
			row = &udpTraceGroupRow{process: flow.Process, peer: peer, server: server}
			groups[key{flow.Process, peer, server}] = row
		}
		// 서버 행의 target은 한 client의 이름이라 행 전체를 설명하지 못한다.
		if row.hostname == "" && !server {
			row.hostname = flow.Hostname
		}
		row.sent += flow.Sent
		row.received += flow.Received
		row.TXBytes += flow.TXBytes
		row.RXBytes += flow.RXBytes
	}
	rows := make([]udpTraceGroupRow, 0, len(groups))
	for _, row := range groups {
		row.traceTraffic.finalize(duration)
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].process != rows[j].process {
			return rows[i].process < rows[j].process
		}
		if rows[i].peer != rows[j].peer {
			return rows[i].peer < rows[j].peer
		}
		return !rows[i].server && rows[j].server
	})
	return rows
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

func printUDPTraceReport(report udpTraceReport, detail bool) {
	fmt.Fprintf(os.Stdout, "UDP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Datagrams: %d\nSent: %d\nReceived: %d\nTX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\nLost events: %d\n", report.Datagrams, report.Sent, report.Received, traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic), report.LostEvents)
	if !detail {
		if len(report.groups) == 0 {
			return
		}
		fmt.Fprintln(os.Stdout, "\nPROCESS\tDESTINATION\tSENT\tRECEIVED\tTX\tRX")
		for _, row := range report.groups {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%d\t%s\t%s\n", row.process, traceSummaryPeerLabel(row.peer, row.hostname, row.server), row.sent, row.received, traceBytes(row.TXBytes), traceBytes(row.RXBytes))
		}
		return
	}
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

// traceGroup은 destination과 process를 집합으로 모은다. 목록에서 중복을 찾으면 서버 행처럼 상대가 많은
// group에서 event 수와 상대 수의 곱만큼 비교해, 긴 trace를 끝낼 때 요약이 수십 초 걸렸다.
type traceGroup struct {
	Group           string
	Server          bool
	Destinations    map[string]struct{}
	Processes       map[string]struct{}
	Events          uint64
	Tx              uint64
	Rx              uint64
	Connect         uint64
	Retransmissions uint64
	Resets          uint64
	LastEvent       string
	DNS             *traceDNSCounts
	Neighbor        *traceNeighborCounts
	HTTP            *traceHTTPCounts
	MySQL           *traceMySQLCounts
	traceTraffic
}

type traceGroupSummary struct {
	Group           string   `json:"group"`
	Server          bool     `json:"server,omitempty"`
	Destinations    []string `json:"destinations,omitempty"`
	Processes       []string `json:"processes,omitempty"`
	Events          uint64   `json:"events"`
	Rate            float64  `json:"rate_per_second"`
	Tx              uint64   `json:"tx"`
	Rx              uint64   `json:"rx"`
	Connect         uint64   `json:"connect"`
	Retransmissions *uint64  `json:"retransmissions"`
	Resets          *uint64  `json:"resets"`
	LastEvent       string   `json:"last_event,omitempty"`
	// DNS는 DNS group에만 있다. TCP와 UDP group의 JSON에는 나오지 않는다.
	DNS *traceDNSCounts `json:"dns,omitempty"`
	// Neighbor는 ARP와 NDP group에만, HTTP는 HTTP group에만 있다.
	Neighbor *traceNeighborCounts `json:"neighbor,omitempty"`
	HTTP     *traceHTTPCounts     `json:"http,omitempty"`
	MySQL    *traceMySQLCounts    `json:"mysql,omitempty"`
	traceTraffic
}

type traceGroupReport struct {
	Protocol   string              `json:"protocol"`
	Side       string              `json:"side,omitempty"`
	GroupBy    string              `json:"group_by"`
	DurationMS int64               `json:"duration_ms"`
	Events     uint64              `json:"events"`
	Rate       float64             `json:"rate_per_second"`
	LostEvents uint64              `json:"lost_events"`
	Groups     []traceGroupSummary `json:"groups"`
	traceTraffic
}

// traceAggregate는 trace가 끝난 뒤 보여 줄 요약을 event가 올 때마다 쌓는다. event를 모두 두면 긴 trace에서
// 메모리가 event 수만큼 늘었다. view가 빈 문자열이면 protocol의 기본 요약을, 아니면 그 group 요약을 쌓는다.
type traceAggregate struct {
	summary traceSummarizer
	groups  map[string]*traceGroupSummarizer
}

func newTraceAggregate(protocol string, views ...string) *traceAggregate {
	aggregate := &traceAggregate{groups: make(map[string]*traceGroupSummarizer)}
	for _, view := range views {
		if view == "" {
			aggregate.summary = traceProtocols[protocol].newSummarizer()
			continue
		}
		aggregate.groups[view] = newTraceGroupSummarizer(protocol, view)
	}
	return aggregate
}

func (aggregate *traceAggregate) observe(event captureEvent) {
	if aggregate.summary != nil {
		aggregate.summary.observe(event)
	}
	for _, groups := range aggregate.groups {
		groups.observe(event)
	}
}

func summarizeTraceGroups(protocol, groupBy string, events []captureEvent, summary captureSummary, duration time.Duration, process, destination string) traceGroupReport {
	summarizer := newTraceGroupSummarizer(protocol, groupBy)
	for _, event := range events {
		if traceProtocol(event) != protocol || !traceEventMatches(event, process, destination) {
			continue
		}
		summarizer.observe(event)
	}
	return summarizer.report(summary, duration)
}

type traceGroupSummarizer struct {
	protocol string
	side     string
	// mixedSides는 두 쪽의 event를 모두 본 것이다. 그러면 보고서의 쪽을 비운다.
	mixedSides bool
	groupBy    string
	groups     map[string]*traceGroup
	events     uint64
	traffic    traceTraffic
}

func newTraceGroupSummarizer(protocol, groupBy string) *traceGroupSummarizer {
	return &traceGroupSummarizer{protocol: protocol, groupBy: groupBy, groups: make(map[string]*traceGroup)}
}

func (summarizer *traceGroupSummarizer) observe(event captureEvent) {
	// socket event는 한 trace에 두 쪽이 섞인다. 보고서의 쪽은 dns와 http에만 있고, 두 쪽이 섞인 http trace에는 없다.
	if event.Side != "" && traceProtocol(event) != "socket" {
		if summarizer.side == "" && !summarizer.mixedSides {
			summarizer.side = event.Side
		} else if summarizer.side != event.Side {
			summarizer.side, summarizer.mixedSides = "", true
		}
	}
	key, server := traceGroupKey(event, summarizer.groupBy)
	// 로컬 서버가 받은 HTTP 요청은 같은 process나 target이 보낸 요청과 다른 행에 둔다. 서버의 응답 시간은 처리 시간만
	// 재므로, 섞으면 평균이 뜻을 잃는다.
	if (event.Protocol == "http" || event.Protocol == "mysql") && event.Side == traceServerSide {
		server = true
	}
	// 서버 행과 target 없는 client 행이 같은 주소일 수 있다. 섞이지 않도록 map key만 구분한다.
	mapKey := key
	if server {
		mapKey += "\x00server"
	}
	group := summarizer.groups[mapKey]
	if group == nil {
		group = &traceGroup{Group: key, Server: server, Destinations: map[string]struct{}{}, Processes: map[string]struct{}{}}
		summarizer.groups[mapKey] = group
	}
	observeTraceGroup(group, event)
	// event 보기는 질의를 dns_query 행에, 응답을 결과 행에 둔다. 응답이 답한 질의를 dns_query 행에서 빼지 않으면
	// 그 행의 질의가 모두 응답 없음으로 보인다. 다른 보기는 질의와 응답이 같은 행에 들어간다.
	if summarizer.groupBy == traceGroupByEvent && event.answered > 0 {
		if queries := summarizer.groups[traceDNSQueryEvent]; queries != nil && queries.DNS != nil {
			queries.DNS.answered += event.answered
		}
		requestKey := traceHTTPRequestEvent
		if event.MySQL != nil {
			requestKey = mysqlEventPrefix + event.MySQL.Command
		}
		if server {
			requestKey += "\x00server"
		}
		if requests := summarizer.groups[requestKey]; requests != nil && requests.HTTP != nil {
			requests.HTTP.answered += event.answered
		}
		if requests := summarizer.groups[requestKey]; requests != nil && requests.MySQL != nil {
			requests.MySQL.answered += event.answered
		}
	}
	summarizer.events++
	summarizer.traffic.observe(event)
}

func (summarizer *traceGroupSummarizer) report(summary captureSummary, duration time.Duration) traceGroupReport {
	result := traceGroupReport{Protocol: summarizer.protocol, Side: summarizer.side, GroupBy: summarizer.groupBy, DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, Events: summarizer.events, traceTraffic: summarizer.traffic}
	result.Rate = traceGroupRate(result.Events, duration)
	result.traceTraffic.finalize(duration)
	result.Groups = make([]traceGroupSummary, 0, len(summarizer.groups))
	for _, group := range summarizer.groups {
		traffic := group.traceTraffic
		traffic.finalize(duration)
		result.Groups = append(result.Groups, traceGroupSummary{
			Group: group.Group, Server: group.Server, Destinations: slices.Sorted(maps.Keys(group.Destinations)), Processes: slices.Sorted(maps.Keys(group.Processes)),
			Events: group.Events, Rate: traceGroupRate(group.Events, duration), Tx: group.Tx, Rx: group.Rx, Connect: group.Connect,
			Retransmissions: traceObserved(group.Retransmissions), Resets: traceObserved(group.Resets), LastEvent: group.LastEvent, DNS: traceDNSGroupSummary(group.DNS), Neighbor: traceNeighborGroupSummary(group.Neighbor), HTTP: traceHTTPGroupSummary(group.HTTP), MySQL: traceMySQLGroupSummary(group.MySQL), traceTraffic: traffic,
		})
	}
	if summarizer.groupBy == traceGroupByPort {
		sort.Slice(result.Groups, func(i, j int) bool { return tracePortGroupLess(result.Groups[i], result.Groups[j]) })
	} else {
		sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].Group < result.Groups[j].Group })
	}
	return result
}

func traceGroupKey(event captureEvent, groupBy string) (string, bool) {
	if groupBy == traceGroupByEvent {
		if event.Event == "" {
			return "-", false
		}
		return event.Event, false
	}
	if groupBy == traceGroupByProcess {
		if event.Process == "" {
			return "-", false
		}
		return event.Process, false
	}
	if groupBy == traceGroupBySource {
		if event.Source == "" {
			return "-", false
		}
		// source port는 연결마다 OS가 새로 고르는 ephemeral port라서, 포함하면 같은 host의 연결이
		// 모두 다른 group이 된다.
		if host, _, err := net.SplitHostPort(event.Source); err == nil {
			return host, false
		}
		return event.Source, false
	}
	if groupBy == traceGroupByPort {
		return tracePortGroupKey(event)
	}
	if event.Target != "" {
		return event.Target, false
	}
	// 서버 socket의 상대는 client마다 새 ephemeral port라서, 상대 주소로 묶으면 요청마다 행이 생긴다.
	low, high := traceEphemeralPortRange()
	if service, ok := traceServerService(event, low, high); ok {
		return service, true
	}
	if event.Destination != "" {
		return event.Destination, false
	}
	return "-", false
}

// traceEphemeralPortRange는 OS가 client socket에 고르는 port 범위다.
var traceEphemeralPortRange = sync.OnceValues(func() (int, int) {
	if low, high, ok := readEphemeralPortRange(); ok {
		return low, high
	}
	// /proc/sys를 읽지 못하는 컨테이너 등에서는 Linux 기본 범위로 판정한다.
	return 32768, 60999
})

// traceListeningPorts는 이 host에서 연결을 기다리는 TCP port다. 처음 쓸 때 한 번 읽는다.
var traceListeningPorts = sync.OnceValue(readListeningTCPPorts)

// traceServerService는 로컬 port가 ephemeral 범위 밖이고 상대 port가 범위 안이면 서버 쪽으로 보고 로컬
// 주소를 돌려준다. 양쪽 모두 범위 안(P2P)이거나 모두 밖(NTP 123↔123)이면 판정하지 않는다. TCP는 로컬 port가
// 범위 밖의 listen port이면 상대 port와 관계없이 서버 쪽이다.
func traceServerService(event captureEvent, low, high int) (string, bool) {
	local, ok := traceAddressPort(event.Source)
	if !ok {
		return "", false
	}
	peer, ok := traceAddressPort(event.Destination)
	if !ok {
		return "", false
	}
	ephemeral := func(port int) bool { return port >= low && port <= high }
	// NAT를 거친 client는 상대 port가 범위 밖일 수 있어서, 로컬 port가 이 host의 TCP listen port면 서버 쪽으로 본다.
	// 범위 안의 listen port는 빼는데, trace는 모든 network namespace를 보고 다른 namespace의 client가 같은 port를 쓸 수 있다.
	if traceProtocol(event) == "tcp" && !ephemeral(local) && traceListeningPorts()[local] {
		return event.Source, true
	}
	if ephemeral(local) || !ephemeral(peer) {
		return "", false
	}
	return event.Source, true
}

// tracePortGroupKey는 상대의 서비스 포트로 묶는다. 서버 socket은 상대 포트가 client마다 달라서 로컬 포트로 묶는다.
func tracePortGroupKey(event captureEvent) (string, bool) {
	low, high := traceEphemeralPortRange()
	if service, ok := traceServerService(event, low, high); ok {
		port, _ := traceAddressPort(service)
		return strconv.Itoa(port), true
	}
	if port, ok := traceAddressPort(event.Destination); ok {
		return strconv.Itoa(port), false
	}
	return "-", false
}

// tracePortGroupLess는 포트를 숫자 순서로 놓는다. 문자열 순서면 443이 3478 뒤에 온다.
func tracePortGroupLess(left, right traceGroupSummary) bool {
	leftPort, leftErr := strconv.Atoi(left.Group)
	rightPort, rightErr := strconv.Atoi(right.Group)
	if (leftErr == nil) != (rightErr == nil) {
		return leftErr != nil
	}
	if leftPort != rightPort {
		return leftPort < rightPort
	}
	return !left.Server && right.Server
}

func traceAddressPort(address string) (int, bool) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0, false
	}
	value, err := strconv.Atoi(port)
	return value, err == nil && value > 0
}

func observeTraceGroup(group *traceGroup, event captureEvent) {
	group.Events++
	if event.Destination != "" {
		group.Destinations[event.Destination] = struct{}{}
	}
	if event.Process != "" {
		group.Processes[event.Process] = struct{}{}
	}
	switch event.Event {
	case "udp_send":
		group.Tx += traceEventPackets(event)
	case "udp_receive":
		group.Rx += traceEventPackets(event)
	case "tcp_connect", "tcp_accept", "socket_connect", "socket_accept":
		group.Connect++
	case "tcp_retransmit":
		group.Retransmissions++
	case "tcp_send_reset", "tcp_receive_reset":
		group.Resets++
	}
	switch event.Protocol {
	case "dns":
		if group.DNS == nil {
			group.DNS = &traceDNSCounts{}
		}
		group.DNS.observe(event)
	case "arp", "ndp":
		if group.Neighbor == nil {
			group.Neighbor = &traceNeighborCounts{}
		}
		group.Neighbor.observe(event)
	case "http":
		if group.HTTP == nil {
			group.HTTP = &traceHTTPCounts{}
		}
		group.HTTP.observe(event)
	case "mysql":
		if group.MySQL == nil {
			group.MySQL = &traceMySQLCounts{}
		}
		group.MySQL.observe(event)
	}
	group.traceTraffic.observe(event)
	group.LastEvent = event.Event
}

func traceGroupRate(events uint64, duration time.Duration) float64 {
	seconds := duration.Seconds()
	if seconds <= 0 {
		return float64(events)
	}
	return float64(events) / seconds
}

func traceGroupDisplayValue(groupBy string, group traceGroupSummary) string {
	// source와 event group에는 destination이 여럿 섞이므로 첫 destination 하나만 붙이면 그 group이 한 곳으로만
	// 간 것처럼 보인다. target group은 이름이 가리키는 주소를 보여 주려고 괄호를 붙인다. port group은 상대가
	// 하나면 그 주소를, 여럿이면 상대 수를 붙인다.
	if group.Server {
		// 서버 행의 destination은 client port들이라, 하나만 붙이면 한 client만 쓴 것처럼 보인다.
		return group.Group + traceServerSuffix
	}
	if group.DNS != nil {
		// DNS event의 destination은 DNS 서버라서 이름 뒤에 붙이면 그 이름이 가리키는 주소처럼 읽힌다.
		return group.Group
	}
	if groupBy == traceGroupByPort && group.Group == "-" {
		return group.Group
	}
	if groupBy == traceGroupByPort && len(group.Destinations) > 1 {
		return fmt.Sprintf("%s (%d peers)", group.Group, len(group.Destinations))
	}
	if (groupBy != traceGroupByTarget && groupBy != traceGroupByPort) || len(group.Destinations) == 0 || group.Destinations[0] == group.Group {
		return group.Group
	}
	return fmt.Sprintf("%s (%s)", group.Group, group.Destinations[0])
}

func traceGroupLabel(groupBy string) string {
	return strings.ToUpper(groupBy)
}

func printTraceGroupReport(report traceGroupReport) {
	title := strings.ToUpper(report.Protocol)
	if report.Side == traceServerSide {
		title += " server"
	}
	fmt.Fprintf(os.Stdout, "%s trace grouped by %s: %s\n\n", title, report.GroupBy, (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Events: %d\n%s groups: %d\nEvent rate: %.1f/s\n", report.Events, report.GroupBy, len(report.Groups), report.Rate)
	if !report.hideTraffic() {
		fmt.Fprintf(os.Stdout, "TX: %s\nRX: %s\nTotal: %s\nTraffic rate: %s\n", traceBytes(report.TXBytes), traceBytes(report.RXBytes), traceBytes(report.TotalBytes), traceTrafficRate(report.traceTraffic))
	}
	if report.Protocol == "arp" || report.Protocol == "ndp" {
		fmt.Fprintf(os.Stdout, "Lost events: %s\n", traceNeighborLost(report.LostEvents))
	} else {
		fmt.Fprintf(os.Stdout, "Lost events: %d\n", report.LostEvents)
	}
	if len(report.Groups) == 0 {
		return
	}
	columns := traceProtocols[report.Protocol].groupColumns
	titles := []string{traceGroupLabel(report.GroupBy), "EVENTS", "EVENT/s"}
	if !report.hideTraffic() {
		titles = append(titles, "TX", "RX", "TOTAL", "B/s", "Mbps")
	}
	for _, column := range columns {
		titles = append(titles, column.reportTitle)
	}
	fmt.Fprintf(os.Stdout, "\n%s\n", strings.Join(append(titles, "LAST"), "\t"))
	for _, group := range report.Groups {
		values := []string{traceGroupDisplayValue(report.GroupBy, group), strconv.FormatUint(group.Events, 10), fmt.Sprintf("%.1f", group.Rate)}
		if !report.hideTraffic() {
			values = append(values, traceBytes(group.TXBytes), traceBytes(group.RXBytes), traceBytes(group.TotalBytes), traceBytes(uint64(group.BytesPerSecond)), fmt.Sprintf("%.3f", group.MegabitsPerSecond))
		}
		for _, column := range columns {
			values = append(values, column.value(group))
		}
		fmt.Fprintf(os.Stdout, "%s\n", strings.Join(append(values, group.LastEvent), "\t"))
	}
}

// traceObserved는 이 platform이 관측하지 못하는 값을 nil로 바꾼다. 0으로 두면 관측한 0과 구분되지 않는다.
func traceObserved[T any](value T) *T {
	if !traceKernelEvents {
		return nil
	}
	return &value
}

func traceOptional[T any](value *T, format string) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf(format, *value)
}

// traceEventPackets는 event 하나가 나타내는 packet 수다. eBPF event는 호출 한 번이고, counter에서 만든
// event는 구간 동안의 packet 수를 담는다.
func traceEventPackets(event captureEvent) uint64 {
	if event.Packets == 0 {
		return 1
	}
	return event.Packets
}

func traceBytes(bytes uint64) string {
	const kib = 1024
	const mib = 1024 * kib
	const gib = 1024 * mib
	if bytes >= gib {
		return fmt.Sprintf("%.1fGiB", float64(bytes)/gib)
	}
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
