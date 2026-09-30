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
	traceDNSQueryEvent      = "dns_query"
	traceDNSTruncatedEvent  = "dns_truncated"
	traceDNSTCPConnectEvent = "dns_tcp_connect"
	traceDNSTCPFailEvent    = "dns_tcp_fail"
	traceDNSTCPAcceptEvent  = "dns_tcp_accept"
	traceDNSHeaderSize      = 12
	// traceDNSPendingLimit은 응답을 기다리는 질의 수의 상한이다. 응답이 오지 않는 질의가 쌓여도 메모리를 제한한다.
	traceDNSPendingLimit = 65536
	// traceDNSTruncatedLimit은 TCP로 다시 물을 이름을 기억하는 수의 상한이다.
	traceDNSTruncatedLimit = 1024
)

// dnsPacket은 kernel이 port 53으로 주고받은 DNS message 하나다. source는 로컬 쪽, destination은 상대 쪽이다.
type dnsPacket struct {
	bootTimeNS  uint64
	arrivalNS   uint64
	pid         uint32
	cgroupID    uint64
	process     string
	sent        bool
	tcp         bool
	socket      uint64
	source      string
	destination string
	payload     []byte
}

// dnsQueryKey의 remote는 client 쪽이면 DNS 서버, 서버 쪽이면 질의한 client다.
type dnsQueryKey struct {
	server    bool
	localPort string
	remote    string
	id        uint16
}

type dnsPendingQuery struct {
	bootTimeNS uint64
	name       string
	queryType  string
}

// dnsTruncatedKey는 잘린 응답이 오간 process와 상대 host다. client 쪽은 응답을 받은 process와 서버, 서버 쪽은 응답을 보낸
// client다. 그 client가 TCP로 다시 물으면 이 이름을 붙인다. 서버 쪽 accept event는 process를 모를 수 있어 pid를 0으로 둔다.
type dnsTruncatedKey struct {
	pid  uint32
	peer string
}

// dnsQueryTracker는 응답을 같은 로컬 port, 상대, transaction ID의 질의와 짝지어 응답 시간을 잰다. client 쪽은 질의를
// 보낸 때부터 응답을 읽은 때까지, 서버 쪽은 질의를 읽은 때부터 응답을 보낸 때까지다.
// 응답 하나는 같은 key로 다시 보낸 질의까지 모두 답한 것으로 보고, 응답 시간은 처음 보낸 질의부터 잰다.
// server는 tracker가 볼 쪽이다. 다른 쪽 message는 짝도 기억하지 않는다. --side server에서도 client 쪽 레코드는
// kernel에서 오므로, 기록하면 쓰지 않을 질의와 잘린 응답이 map을 채운다.
type dnsQueryTracker struct {
	server    bool
	pending   map[dnsQueryKey][]dnsPendingQuery
	size      int
	truncated map[dnsTruncatedKey]dnsPendingQuery
}

func newDNSQueryTracker(server bool) *dnsQueryTracker {
	return &dnsQueryTracker{server: server, pending: map[dnsQueryKey][]dnsPendingQuery{}, truncated: map[dnsTruncatedKey]dnsPendingQuery{}}
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
	_, localPort, _ := net.SplitHostPort(packet.source)
	_, remotePort, _ := net.SplitHostPort(packet.destination)
	// client는 port 53으로 질의를 보내고 port 53에서 응답을 받는다. 서버는 로컬 port 53으로 질의를 받고 응답을 보낸다.
	// QR bit와 방향, port가 어느 쪽에도 맞지 않으면 뺀다.
	query := flags&0x8000 == 0
	var server bool
	switch {
	case remotePort == "53" && query == packet.sent:
	case localPort == "53" && query != packet.sent:
		server = true
	default:
		return captureEvent{}, false
	}
	if server != tracker.server {
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
	if server {
		event.Side = traceServerSide
	}
	// 받은 message만 수신 큐 시각이 있다. client 쪽은 응답을, 서버 쪽은 질의를 받는다.
	if !packet.sent && packet.arrivalNS != 0 {
		event.ReadDelayMS = traceSpan(packet.arrivalNS, packet.bootTimeNS)
	}
	key := dnsQueryKey{server: server, localPort: localPort, remote: packet.destination, id: id}
	if query {
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
		event.LatencyMS = traceSpan(queries[0].bootTimeNS, packet.bootTimeNS)
		if !packet.sent && packet.arrivalNS != 0 {
			event.NetworkMS = traceSpan(queries[0].bootTimeNS, packet.arrivalNS)
		}
		event.answered = uint64(len(queries))
		if event.Target == "" {
			event.Target, event.QueryType = queries[0].name, queries[0].queryType
		}
	}
	// TC bit는 응답이 UDP에 다 들어가지 않았다는 뜻이다. client는 같은 서버에 TCP로 다시 묻는다.
	if flags&0x0200 != 0 {
		event.Event = traceDNSTruncatedEvent
		if len(tracker.truncated) >= traceDNSTruncatedLimit {
			clear(tracker.truncated)
		}
		tracker.truncated[tracker.truncatedKey(packet.pid, packet.destination)] = dnsPendingQuery{name: event.Target, queryType: event.QueryType}
	}
	return event, true
}

// tcpEvent는 port 53의 TCP 연결을 DNS event로 바꾼다. client 쪽은 port 53으로 가는 연결과 실패를, 서버 쪽은 로컬 port 53이
// 받은 연결을 본다. TCP로 주고받는 DNS message는 읽지 않는다.
func (tracker *dnsQueryTracker) tcpEvent(event captureEvent) (captureEvent, bool) {
	if traceProtocol(event) != "tcp" {
		return captureEvent{}, false
	}
	name, port := "", 0
	switch {
	case tracker.server && event.Event == "tcp_accept":
		name, port = traceDNSTCPAcceptEvent, traceDNSPort(event.Source)
	case !tracker.server && event.Event == "tcp_connect":
		name, port = traceDNSTCPConnectEvent, traceDNSPort(event.Destination)
	case !tracker.server && event.Event == "tcp_close" && event.OldState == "SYN_SENT":
		name, port = traceDNSTCPFailEvent, traceDNSPort(event.Destination)
	}
	if port != 53 {
		return captureEvent{}, false
	}
	dnsEvent := captureEvent{
		TimestampNS: event.TimestampNS, BootTimeNS: event.BootTimeNS, Event: name, Protocol: "dns", PID: event.PID, Process: event.Process,
		CgroupID: event.CgroupID, Source: event.Source, Destination: event.Destination, Transport: "tcp",
	}
	if tracker.server {
		dnsEvent.Side = traceServerSide
	}
	key := tracker.truncatedKey(event.PID, event.Destination)
	if query, ok := tracker.truncated[key]; ok {
		delete(tracker.truncated, key)
		dnsEvent.Target, dnsEvent.QueryType = query.name, query.queryType
	}
	return dnsEvent, true
}

func (tracker *dnsQueryTracker) truncatedKey(pid uint32, peer string) dnsTruncatedKey {
	if tracker.server {
		pid = 0
	}
	return dnsTruncatedKey{pid: pid, peer: traceDNSHost(peer)}
}

func traceDNSPort(address string) int {
	port, _ := traceAddressPort(address)
	return port
}

func traceDNSHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// dnsTCPStreamLimit은 길이만 읽고 message를 기다리는 조각 수의 상한이다. 닫힌 연결의 조각이 쌓여도 메모리를 제한한다.
const dnsTCPStreamLimit = 4096

type dnsTCPStreamKey struct {
	socket uint64
	sent   bool
}

// dnsTCPStreams는 TCP로 주고받은 조각에서 DNS message를 꺼낸다. DNS over TCP는 message 앞에 2바이트 길이를 붙인다.
// 길이와 message를 따로 읽거나 쓰는 program이 많아, 길이만 온 조각은 연결과 방향마다 기억했다가 다음 조각을 message로 본다.
type dnsTCPStreams struct {
	pending map[dnsTCPStreamKey]int
}

func newDNSTCPStreams() *dnsTCPStreams {
	return &dnsTCPStreams{pending: map[dnsTCPStreamKey]int{}}
}

func (streams *dnsTCPStreams) forgetSocket(socket uint64) {
	delete(streams.pending, dnsTCPStreamKey{socket: socket, sent: true})
	delete(streams.pending, dnsTCPStreamKey{socket: socket, sent: false})
}

// messages는 조각 하나에 든 DNS message다. BPF는 조각의 앞 capacity byte만 읽으므로, 잘린 message는 잘린 채로 돌려준다.
// 뒤의 해석이 header로 결과를 읽는다.
func (streams *dnsTCPStreams) messages(key dnsTCPStreamKey, chunk []byte, capacity int) [][]byte {
	if expected, ok := streams.pending[key]; ok {
		delete(streams.pending, key)
		// 앞 조각이 길이였고 이 조각이 그 길이면 message 본문이다. 길이가 맞지 않으면 이 조각을 새로 읽는다.
		if len(chunk) >= traceDNSHeaderSize && (len(chunk) == expected || (len(chunk) == capacity && expected > capacity)) {
			return [][]byte{chunk}
		}
	}
	var messages [][]byte
	for len(chunk) >= 2 {
		length := int(binary.BigEndian.Uint16(chunk[0:2]))
		if len(chunk) == 2 {
			if len(streams.pending) >= dnsTCPStreamLimit {
				clear(streams.pending)
			}
			streams.pending[key] = length
			break
		}
		body := chunk[2:]
		if length < traceDNSHeaderSize || len(body) < traceDNSHeaderSize {
			break
		}
		if len(body) <= length {
			messages = append(messages, body)
			break
		}
		messages = append(messages, body[:length])
		chunk = body[length:]
	}
	return messages
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

// traceDNSError는 조회가 실패한 결과다. 잘린 응답과 TCP 연결은 TCP로 다시 묻는 과정이라 오류로 세지 않는다.
func traceDNSError(event string) bool {
	switch event {
	case traceDNSQueryEvent, "dns_noerror", "dns_nodata", traceDNSTruncatedEvent, traceDNSTCPConnectEvent, traceDNSTCPAcceptEvent:
		return false
	}
	return true
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
	Queries    uint64 `json:"queries"`
	Answers    uint64 `json:"answers"`
	Errors     uint64 `json:"errors"`
	Unanswered uint64 `json:"unanswered"`
	// TCPConnections는 port 53의 TCP 연결이다. client 쪽은 연결 시도, 서버 쪽은 받은 연결이다. 실패한 연결은 Errors에도 들어간다.
	TCPConnections uint64   `json:"tcp_connections"`
	LatencyAvgMS   *float64 `json:"latency_avg_ms"`
	LatencyMaxMS   *float64 `json:"latency_max_ms"`
	NetworkAvgMS   *float64 `json:"network_avg_ms"`
	NetworkMaxMS   *float64 `json:"network_max_ms"`
	ReadDelayAvgMS *float64 `json:"read_delay_avg_ms"`
	ReadDelayMaxMS *float64 `json:"read_delay_max_ms"`
	answered       uint64
	latency        traceSpans
	network        traceSpans
	readDelay      traceSpans
}

func (counts *traceDNSCounts) observe(event captureEvent) {
	counts.readDelay.observe(event.ReadDelayMS)
	switch event.Event {
	case traceDNSQueryEvent:
		counts.Queries++
		return
	case traceDNSTCPConnectEvent, traceDNSTCPFailEvent, traceDNSTCPAcceptEvent:
		counts.TCPConnections++
		if traceDNSError(event.Event) {
			counts.Errors++
		}
		return
	}
	counts.Answers++
	if traceDNSError(event.Event) {
		counts.Errors++
	}
	counts.answered += event.answered
	counts.latency.observe(event.LatencyMS)
	counts.network.observe(event.NetworkMS)
}

// finished는 보여 줄 값을 채운 복사본이다. 쌓은 값은 그대로 두어 같은 요약을 다시 만들 수 있다. 전체 화면은
// 최근 event만 두므로 질의가 창 밖으로 밀린 응답이 있어 응답 없음을 0 밑으로 내리지 않는다.
func (counts traceDNSCounts) finished() traceDNSCounts {
	counts.Unanswered = counts.Queries - min(counts.Queries, counts.answered)
	counts.LatencyAvgMS, counts.LatencyMaxMS = counts.latency.summary()
	counts.NetworkAvgMS, counts.NetworkMaxMS = counts.network.summary()
	counts.ReadDelayAvgMS, counts.ReadDelayMaxMS = counts.readDelay.summary()
	return counts
}

func traceDNSGroupSummary(counts *traceDNSCounts) *traceDNSCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
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
	{screenTitle: "AVGms", reportTitle: "AVG_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceLatency(counts.LatencyAvgMS, "") })},
	{screenTitle: "MAXms", reportTitle: "MAX_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceLatency(counts.LatencyMaxMS, "") })},
	{screenTitle: "NETms", reportTitle: "NET_AVG_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceLatency(counts.NetworkAvgMS, "") })},
	{screenTitle: "RDms", reportTitle: "READ_AVG_MS", width: 6, value: traceDNSColumn(func(counts traceDNSCounts) string { return traceLatency(counts.ReadDelayAvgMS, "") })},
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
		label += " " + traceLatency(event.LatencyMS, "ms")
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
	Side       string            `json:"side,omitempty"`
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
	side    string
}

func newDNSTraceSummarizer() *dnsTraceSummarizer {
	return &dnsTraceSummarizer{names: map[dnsTraceNameKey]*dnsTraceNameStats{}, results: map[string]uint64{}}
}

func (summarizer *dnsTraceSummarizer) observe(event captureEvent) {
	if event.Side != "" {
		summarizer.side = event.Side
	}
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
	report := dnsTraceReport{Side: summarizer.side, DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, Results: maps.Clone(summarizer.results), traceDNSCounts: summarizer.counts.finished()}
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
	title := "DNS trace"
	if report.Side == traceServerSide {
		title = "DNS server trace"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n\n", title, (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Queries: %d\nAnswers: %d\nErrors: %d\nUnanswered: %d\nTCP connections: %d\n", report.Queries, report.Answers, report.Errors, report.Unanswered, report.TCPConnections)
	fmt.Fprintf(os.Stdout, "Latency avg: %s\nLatency max: %s\nNetwork avg: %s\nNetwork max: %s\nRead delay avg: %s\nRead delay max: %s\nLost events: %d\n", traceLatency(report.LatencyAvgMS, "ms"), traceLatency(report.LatencyMaxMS, "ms"), traceLatency(report.NetworkAvgMS, "ms"), traceLatency(report.NetworkMaxMS, "ms"), traceLatency(report.ReadDelayAvgMS, "ms"), traceLatency(report.ReadDelayMaxMS, "ms"), report.LostEvents)
	if len(report.Names) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nNAME\tTYPE\tQUERIES\tANSWERS\tRESULTS\tUNANSWERED\tAVG\tMAX\tNET\tREAD\tPROCESS")
	for _, name := range report.Names {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", name.Name, emptyAs(name.Type, "-"), name.Queries, name.Answers, traceResultCounts(name.Results), name.Unanswered, traceLatency(name.LatencyAvgMS, "ms"), traceLatency(name.LatencyMaxMS, "ms"), traceLatency(name.NetworkAvgMS, "ms"), traceLatency(name.ReadDelayAvgMS, "ms"), emptyAs(strings.Join(name.Processes, ","), "-"))
	}
}
