package edc

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const (
	traceDNSQueryEvent = "dns_query"
	traceDNSHeaderSize = 12
	// traceDNSPendingLimit은 응답을 기다리는 질의 수의 상한이다. 응답이 오지 않는 질의가 쌓여도 메모리를 제한한다.
	traceDNSPendingLimit = 65536
)

// dnsPacket은 kernel이 넘긴 DNS 질의나 응답 하나다. source는 로컬 쪽, destination은 DNS 서버다.
type dnsPacket struct {
	bootTimeNS  uint64
	pid         uint32
	cgroupID    uint64
	process     string
	query       bool
	source      string
	destination string
	payload     []byte
}

type dnsQueryKey struct {
	localPort string
	server    string
	id        uint16
}

type dnsPendingQuery struct {
	bootTimeNS uint64
	name       string
	queryType  string
}

// dnsQueryTracker는 응답을 같은 로컬 port, 서버, transaction ID의 질의와 짝지어 응답 시간을 잰다.
// 응답 하나는 같은 key로 다시 보낸 질의까지 모두 답한 것으로 보고, 응답 시간은 처음 보낸 질의부터 잰다.
type dnsQueryTracker struct {
	pending map[dnsQueryKey][]dnsPendingQuery
	size    int
}

func newDNSQueryTracker() *dnsQueryTracker {
	return &dnsQueryTracker{pending: map[dnsQueryKey][]dnsPendingQuery{}}
}

func (tracker *dnsQueryTracker) remember(key dnsQueryKey, query dnsPendingQuery) {
	if tracker.size >= traceDNSPendingLimit {
		// 이때 버린 질의의 응답은 응답 시간 없이 보인다.
		clear(tracker.pending)
		tracker.size = 0
	}
	tracker.pending[key] = append(tracker.pending[key], query)
	tracker.size++
}

func (tracker *dnsQueryTracker) event(packet dnsPacket, clockOffset int64) (captureEvent, bool) {
	if len(packet.payload) < traceDNSHeaderSize {
		return captureEvent{}, false
	}
	id := binary.BigEndian.Uint16(packet.payload[0:2])
	flags := binary.BigEndian.Uint16(packet.payload[2:4])
	// 질의 hook은 port 53으로 보낸 것을, 응답 hook은 port 53에서 받은 것을 넘긴다. QR bit가 방향과 다르면
	// 이 host의 DNS 서버가 port 53 client와 주고받은 것이라 client 쪽 trace에서 뺀다.
	if (flags&0x8000 != 0) == packet.query {
		return captureEvent{}, false
	}
	event := captureEvent{
		TimestampNS: uint64(int64(packet.bootTimeNS) + clockOffset),
		BootTimeNS:  packet.bootTimeNS,
		Protocol:    "dns",
		PID:         packet.pid,
		Process:     packet.process,
		CgroupID:    packet.cgroupID,
		Source:      packet.source,
		Destination: packet.destination,
		Bytes:       uint64(len(packet.payload)),
	}
	var message dns.Msg
	parsed := message.Unpack(packet.payload) == nil && len(message.Question) > 0
	if parsed {
		event.Target = traceDNSName(message.Question[0].Name)
		event.QueryType = dns.Type(message.Question[0].Qtype).String()
	}
	_, localPort, _ := net.SplitHostPort(packet.source)
	key := dnsQueryKey{localPort: localPort, server: packet.destination, id: id}
	if packet.query {
		event.Event = traceDNSQueryEvent
		tracker.remember(key, dnsPendingQuery{bootTimeNS: packet.bootTimeNS, name: event.Target, queryType: event.QueryType})
		return event, true
	}
	// 응답이 BPF가 읽는 1024바이트보다 길면 해석하지 못한다. header의 RCODE와 ANCOUNT로 결과를 정하고, 이름은 질의에서 가져온다.
	rcode, answers := int(flags&0xF), int(binary.BigEndian.Uint16(packet.payload[6:8]))
	if parsed {
		rcode = message.Rcode
		event.Answers = dnsAnswerAddresses(message)
	}
	event.Event = traceDNSAnswerEvent(rcode, answers)
	if queries := tracker.pending[key]; len(queries) > 0 {
		delete(tracker.pending, key)
		tracker.size -= len(queries)
		latency := float64(packet.bootTimeNS-min(packet.bootTimeNS, queries[0].bootTimeNS)) / float64(time.Millisecond)
		event.LatencyMS = &latency
		event.dnsAnswered = uint64(len(queries))
		if event.Target == "" {
			event.Target, event.QueryType = queries[0].name, queries[0].queryType
		}
	}
	return event, true
}

func traceDNSName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// traceDNSAnswerEvent는 응답을 RCODE로 이름 붙인다. 주소 없이 성공한 응답은 오류가 아니라서 nodata로 따로 둔다.
func traceDNSAnswerEvent(rcode, answers int) string {
	if rcode == dns.RcodeSuccess && answers == 0 {
		return "dns_nodata"
	}
	if name, ok := dns.RcodeToString[rcode]; ok {
		return "dns_" + strings.ToLower(name)
	}
	return "dns_rcode" + strconv.Itoa(rcode)
}

func traceDNSError(event string) bool {
	return event != traceDNSQueryEvent && event != "dns_noerror" && event != "dns_nodata"
}

func dnsAnswerAddresses(message dns.Msg) []string {
	var addresses []string
	for _, answer := range message.Answer {
		var ip net.IP
		switch record := answer.(type) {
		case *dns.A:
			ip = record.A
		case *dns.AAAA:
			ip = record.AAAA
		default:
			continue
		}
		if address, ok := netip.AddrFromSlice(ip); ok {
			addresses = append(addresses, address.Unmap().String())
		}
	}
	return addresses
}

// traceDNSCounts는 DNS 질의와 응답의 합계다. DNS group 행과 요약이 함께 쓴다.
type traceDNSCounts struct {
	Queries      uint64   `json:"queries"`
	Answers      uint64   `json:"answers"`
	Errors       uint64   `json:"errors"`
	Unanswered   uint64   `json:"unanswered"`
	LatencyAvgMS *float64 `json:"latency_avg_ms"`
	LatencyMaxMS *float64 `json:"latency_max_ms"`
	answered     uint64
	latencyTotal float64
	latencies    uint64
	latencyMax   float64
}

func (counts *traceDNSCounts) observe(event captureEvent) {
	if event.Event == traceDNSQueryEvent {
		counts.Queries++
		return
	}
	counts.Answers++
	if traceDNSError(event.Event) {
		counts.Errors++
	}
	counts.answered += event.dnsAnswered
	if event.LatencyMS != nil {
		counts.latencyTotal += *event.LatencyMS
		counts.latencies++
		if *event.LatencyMS > counts.latencyMax {
			counts.latencyMax = *event.LatencyMS
		}
	}
}

// finished는 보여 줄 값을 채운 복사본이다. 쌓은 값은 그대로 두어 같은 요약을 다시 만들 수 있다. 전체 화면은
// 최근 event만 두므로 질의가 창 밖으로 밀린 응답이 있어 응답 없음을 0 밑으로 내리지 않는다.
func (counts traceDNSCounts) finished() traceDNSCounts {
	counts.Unanswered = counts.Queries - min(counts.Queries, counts.answered)
	if counts.latencies > 0 {
		average, maximum := counts.latencyTotal/float64(counts.latencies), counts.latencyMax
		counts.LatencyAvgMS, counts.LatencyMaxMS = &average, &maximum
	}
	return counts
}

func traceDNSGroupSummary(counts *traceDNSCounts) *traceDNSCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

func traceDNSLatency(latency *float64, unit string) string {
	if latency == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%s", *latency, unit)
}

func traceDNSColumn(value func(counts traceDNSCounts) string) func(traceGroupSummary) string {
	return func(group traceGroupSummary) string {
		if group.DNS == nil {
			return "-"
		}
		return value(*group.DNS)
	}
}

var traceDNSGroupColumns = []traceGroupColumn{
	{screenTitle: "QRY", reportTitle: "QUERIES", width: 5, value: traceDNSColumn(func(counts traceDNSCounts) string { return strconv.FormatUint(counts.Queries, 10) })},
	{screenTitle: "ANS", reportTitle: "ANSWERS", width: 5, value: traceDNSColumn(func(counts traceDNSCounts) string { return strconv.FormatUint(counts.Answers, 10) })},
	{screenTitle: "ERR", reportTitle: "ERRORS", width: 4, value: traceDNSColumn(func(counts traceDNSCounts) string { return strconv.FormatUint(counts.Errors, 10) })},
	{screenTitle: "NOANS", reportTitle: "UNANSWERED", width: 5, value: traceDNSColumn(func(counts traceDNSCounts) string { return strconv.FormatUint(counts.Unanswered, 10) })},
	{screenTitle: "AVGms", reportTitle: "AVG_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceDNSLatency(counts.LatencyAvgMS, "") })},
	{screenTitle: "MAXms", reportTitle: "MAX_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceDNSLatency(counts.LatencyMaxMS, "") })},
}

// traceDNSScrollLabels는 스크롤 행의 목적지 칸에 조회한 이름, 레코드 종류, 서버를, event 칸에 결과와 응답 시간을 쓴다.
func traceDNSScrollLabels(event captureEvent) (string, string) {
	name := emptyAs(event.Target, "-")
	if event.QueryType != "" {
		name += " " + event.QueryType
	}
	if event.Destination != "" {
		name += " (" + event.Destination + ")"
	}
	label := event.Event
	if event.LatencyMS != nil {
		label += " " + traceDNSLatency(event.LatencyMS, "ms")
	}
	return name, label
}

type dnsTraceNameKey struct {
	name      string
	queryType string
}

type dnsTraceNameStats struct {
	processes map[string]struct{}
	servers   map[string]struct{}
	addresses map[string]struct{}
	results   map[string]uint64
	counts    traceDNSCounts
}

type dnsTraceName struct {
	Name      string            `json:"name"`
	Type      string            `json:"type,omitempty"`
	Processes []string          `json:"processes,omitempty"`
	Servers   []string          `json:"servers,omitempty"`
	Addresses []string          `json:"addresses,omitempty"`
	Results   map[string]uint64 `json:"results,omitempty"`
	traceDNSCounts
}

type dnsTraceReport struct {
	DurationMS int64             `json:"duration_ms"`
	LostEvents uint64            `json:"lost_events"`
	Results    map[string]uint64 `json:"results"`
	Names      []dnsTraceName    `json:"names"`
	traceDNSCounts
}

// dnsTraceSummarizer는 --group-by 없이 끝난 DNS trace를 조회한 이름과 레코드 종류마다 한 행으로 묶는다.
type dnsTraceSummarizer struct {
	names   map[dnsTraceNameKey]*dnsTraceNameStats
	results map[string]uint64
	counts  traceDNSCounts
}

func newDNSTraceSummarizer() *dnsTraceSummarizer {
	return &dnsTraceSummarizer{names: map[dnsTraceNameKey]*dnsTraceNameStats{}, results: map[string]uint64{}}
}

func (summarizer *dnsTraceSummarizer) observe(event captureEvent) {
	key := dnsTraceNameKey{name: emptyAs(event.Target, "-"), queryType: event.QueryType}
	stats := summarizer.names[key]
	if stats == nil {
		stats = &dnsTraceNameStats{processes: map[string]struct{}{}, servers: map[string]struct{}{}, addresses: map[string]struct{}{}, results: map[string]uint64{}}
		summarizer.names[key] = stats
	}
	if event.Process != "" {
		stats.processes[event.Process] = struct{}{}
	}
	if event.Destination != "" {
		stats.servers[event.Destination] = struct{}{}
	}
	for _, address := range event.Answers {
		stats.addresses[address] = struct{}{}
	}
	if event.Event != traceDNSQueryEvent {
		result := strings.TrimPrefix(event.Event, "dns_")
		stats.results[result]++
		summarizer.results[result]++
	}
	stats.counts.observe(event)
	summarizer.counts.observe(event)
}

func (summarizer *dnsTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := dnsTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, Results: maps.Clone(summarizer.results), traceDNSCounts: summarizer.counts.finished()}
	report.Names = make([]dnsTraceName, 0, len(summarizer.names))
	for key, stats := range summarizer.names {
		report.Names = append(report.Names, dnsTraceName{
			Name: key.name, Type: key.queryType, Processes: slices.Sorted(maps.Keys(stats.processes)), Servers: slices.Sorted(maps.Keys(stats.servers)),
			Addresses: slices.Sorted(maps.Keys(stats.addresses)), Results: maps.Clone(stats.results), traceDNSCounts: stats.counts.finished(),
		})
	}
	sort.Slice(report.Names, func(i, j int) bool {
		left, right := report.Names[i], report.Names[j]
		if left.Queries != right.Queries {
			return left.Queries > right.Queries
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.Type < right.Type
	})
	return report
}

// print는 detail과 상관없이 같다. DNS 요약은 이미 이름과 record 종류마다 한 행이고, 질의마다의 행은 두지 않는다.
func (report dnsTraceReport) print(bool) {
	fmt.Fprintf(os.Stdout, "DNS trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Queries: %d\nAnswers: %d\nErrors: %d\nUnanswered: %d\nLatency avg: %s\nLatency max: %s\nLost events: %d\n", report.Queries, report.Answers, report.Errors, report.Unanswered, traceDNSLatency(report.LatencyAvgMS, "ms"), traceDNSLatency(report.LatencyMaxMS, "ms"), report.LostEvents)
	if len(report.Names) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nNAME\tTYPE\tQUERIES\tANSWERS\tRESULTS\tUNANSWERED\tAVG\tMAX\tPROCESS")
	for _, name := range report.Names {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\t%s\n", name.Name, emptyAs(name.Type, "-"), name.Queries, name.Answers, traceDNSResults(name.Results), name.Unanswered, traceDNSLatency(name.LatencyAvgMS, "ms"), traceDNSLatency(name.LatencyMaxMS, "ms"), emptyAs(strings.Join(name.Processes, ","), "-"))
	}
}

// traceDNSResults는 결과를 많은 순서로 잇는다. 예: "noerror 3, nxdomain 1".
func traceDNSResults(results map[string]uint64) string {
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
