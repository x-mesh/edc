package edc

import (
	"encoding/binary"
	"fmt"
	"io"
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
	tcp       bool
	socket    uint64
	server    bool
	localPort string
	remote    string
	id        uint16
}

type dnsPendingQuery struct {
	pid        uint32
	process    string
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
	key := dnsQueryKey{tcp: packet.tcp, socket: packet.socket, server: server, localPort: localPort, remote: packet.destination, id: id}
	if query {
		event.Event = traceDNSQueryEvent
		tracker.remember(key, dnsPendingQuery{pid: packet.pid, process: packet.process, bootTimeNS: packet.bootTimeNS, name: event.Target, queryType: event.QueryType})
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
		if event.PID == 0 {
			owner := queries[0]
			for _, query := range queries[1:] {
				if query.pid != owner.pid {
					owner.pid = 0
					owner.process = ""
					break
				}
			}
			event.PID, event.Process = owner.pid, owner.process
		}
		event.LatencyMS = traceSpan(queries[0].bootTimeNS, packet.bootTimeNS)
		if !packet.sent && packet.arrivalNS != 0 {
			event.NetworkMS = traceSpan(queries[0].bootTimeNS, packet.arrivalNS)
		}
		event.Answered = uint64(len(queries))
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
	counts.answered += event.Answered
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

// dnsTraceProcess는 한 이름을 물은 process다. relay는 이 호스트의 resolver가 다른 program의 조회를 상위 서버로 넘긴 질의다.
type dnsTraceProcess struct {
	name  string
	pid   uint32
	relay bool
}

type dnsTraceNameStats struct {
	processes map[dnsTraceProcess]struct{}
	servers   map[string]struct{}
	addresses map[string]struct{}
	results   map[string]uint64
	counts    traceDNSCounts
}

type dnsTraceName struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
	// BaseName은 resolver가 검색 도메인을 붙여 다시 물은 이름의 원래 이름이다. 표는 이 행을 원래 이름 아래에 들여 쓴다.
	BaseName  string            `json:"base_name,omitempty"`
	Processes []string          `json:"processes,omitempty"`
	PIDs      []uint32          `json:"pids,omitempty"`
	Relays    []string          `json:"relays,omitempty"`
	Servers   []string          `json:"servers,omitempty"`
	Addresses []string          `json:"addresses,omitempty"`
	Results   map[string]uint64 `json:"results,omitempty"`
	traceDNSCounts
	processLabel string
}

// dnsRepeatedFailure는 같은 process가 같은 이름에 같은 실패 응답을 거의 같은 간격으로 받은 것이다. 이름을 풀지 못한 program이
// 몇 초마다 다시 묻는 모양이다.
type dnsRepeatedFailure struct {
	PID        uint32  `json:"pid"`
	Process    string  `json:"process"`
	Name       string  `json:"name"`
	Type       string  `json:"type,omitempty"`
	Result     string  `json:"result"`
	Count      uint64  `json:"count"`
	IntervalMS float64 `json:"interval_ms"`
}

type dnsTraceReport struct {
	Side             string               `json:"side,omitempty"`
	DurationMS       int64                `json:"duration_ms"`
	LostEvents       uint64               `json:"lost_events"`
	Results          map[string]uint64    `json:"results"`
	Names            []dnsTraceName       `json:"names"`
	RepeatedFailures []dnsRepeatedFailure `json:"repeated_failures,omitempty"`
	traceDNSCounts
}

// dnsTraceLocal은 이 호스트의 DNS 설정이다. 실시간 trace만 채운다. trace show는 다른 호스트에서 녹화한 파일도 읽으므로 비워 둔다.
type dnsTraceLocal struct {
	// resolvers는 port 53을 듣는 process다. client 쪽에서 이 process가 보낸 질의는 다른 program의 조회를 상위 서버로 넘긴 것이다.
	resolvers map[uint32]bool
	// search는 resolv.conf의 검색 도메인이다. resolver는 풀리지 않은 이름에 이 도메인을 붙여 다시 묻는다.
	search []string
}

// readDNSTraceLocal은 listen 소켓과 resolv.conf를 읽는다. 읽지 못한 값은 비우고, 그러면 요약은 중계와 검색 도메인을 가리지 않는다.
func readDNSTraceLocal() dnsTraceLocal {
	local := dnsTraceLocal{resolvers: map[uint32]bool{}}
	if sockets, err := collectListenSockets(listenFamilies{TCP: true, UDP: true}); err == nil {
		for _, socket := range sockets {
			if pid, err := strconv.ParseUint(socket.PID, 10, 32); err == nil && socket.Port == 53 {
				local.resolvers[uint32(pid)] = true
			}
		}
	}
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, domain := range parseResolvConf(string(data)).Search {
			if domain = traceDNSName(domain); domain != "" {
				local.search = append(local.search, domain)
			}
		}
	}
	return local
}

// useLocalDNS는 실시간 DNS trace의 요약에 이 호스트의 resolver와 검색 도메인을 넣는다.
func (aggregate *traceAggregate) useLocalDNS() {
	if summarizer, ok := aggregate.summary.(*dnsTraceSummarizer); ok {
		summarizer.local = readDNSTraceLocal()
	}
}

const (
	// dnsRepeatMinFailures는 되풀이로 보는 최소 실패 수다. 간격이 둘은 있어야 일정한지 견줄 수 있다.
	dnsRepeatMinFailures = 3
	// dnsTraceShownPIDs는 PROCESS 칸에 이름마다 적는 PID 수다. 짧게 사는 process가 많이 물어도 칸이 넘치지 않는다.
	dnsTraceShownPIDs = 3
)

type dnsFailureKey struct {
	pid       uint32
	process   string
	name      string
	queryType string
	result    string
}

// dnsFailureRun은 실패 시각의 처음과 끝, 가장 짧고 긴 간격이다. 시각을 모두 두지 않아 긴 trace에서도 메모리가 늘지 않는다.
type dnsFailureRun struct {
	count, first, last, minGap, maxGap uint64
}

func (run *dnsFailureRun) observe(at uint64) {
	if run.count == 0 {
		run.first, run.last, run.count = at, at, 1
		return
	}
	// event 순서가 바뀌어 오면 간격을 0으로 센다. 그러면 일정하다고 보지 않는다.
	gap := at - min(at, run.last)
	if run.count == 1 || gap < run.minGap {
		run.minGap = gap
	}
	if gap > run.maxGap {
		run.maxGap = gap
	}
	if at > run.last {
		run.last = at
	}
	run.count++
}

// regular는 실패가 거의 같은 간격으로 되풀이됐는지 본다. 가장 긴 간격이 가장 짧은 간격보다 20% 넘게 길면 backoff로 늘어나는
// retry나 우연히 겹친 실패로 본다.
func (run dnsFailureRun) regular() bool {
	return run.count >= dnsRepeatMinFailures && run.minGap > 0 && run.maxGap*5 <= run.minGap*6
}

// dnsTraceSummarizer는 --group-by 없이 끝난 DNS trace를 조회한 이름과 레코드 종류마다 한 행으로 묶는다.
type dnsTraceSummarizer struct {
	names    map[dnsTraceNameKey]*dnsTraceNameStats
	failures map[dnsFailureKey]*dnsFailureRun
	results  map[string]uint64
	counts   traceDNSCounts
	side     string
	local    dnsTraceLocal
}

func newDNSTraceSummarizer() *dnsTraceSummarizer {
	return &dnsTraceSummarizer{names: map[dnsTraceNameKey]*dnsTraceNameStats{}, failures: map[dnsFailureKey]*dnsFailureRun{}, results: map[string]uint64{}}
}

func (summarizer *dnsTraceSummarizer) observe(event captureEvent) {
	if event.Side != "" {
		summarizer.side = event.Side
	}
	key := dnsTraceNameKey{name: emptyAs(event.Target, "-"), queryType: event.QueryType}
	stats := summarizer.names[key]
	if stats == nil {
		stats = &dnsTraceNameStats{processes: map[dnsTraceProcess]struct{}{}, servers: map[string]struct{}{}, addresses: map[string]struct{}{}, results: map[string]uint64{}}
		summarizer.names[key] = stats
	}
	relay := event.Side == "" && summarizer.local.resolvers[event.PID]
	if event.Process != "" {
		stats.processes[dnsTraceProcess{name: event.Process, pid: event.PID, relay: relay}] = struct{}{}
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
		// 서버 쪽은 한 서버 process가 여러 client에 답하므로 되풀이를 client 쪽에서만 본다. 중계는 원래 물은 program의 되풀이를 따라 한다.
		if traceDNSError(event.Event) && event.Side == "" && event.PID != 0 && !relay {
			failure := dnsFailureKey{pid: event.PID, process: event.Process, name: key.name, queryType: key.queryType, result: result}
			run := summarizer.failures[failure]
			if run == nil {
				run = &dnsFailureRun{}
				summarizer.failures[failure] = run
			}
			run.observe(event.TimestampNS)
		}
	}
	stats.counts.observe(event)
	summarizer.counts.observe(event)
}

func (summarizer *dnsTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := dnsTraceReport{Side: summarizer.side, DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, Results: maps.Clone(summarizer.results), traceDNSCounts: summarizer.counts.finished()}
	report.Names = make([]dnsTraceName, 0, len(summarizer.names))
	for key, stats := range summarizer.names {
		processes, pids, relays := map[string]struct{}{}, map[uint32]struct{}{}, map[string]struct{}{}
		for process := range stats.processes {
			processes[process.name] = struct{}{}
			if process.pid != 0 {
				pids[process.pid] = struct{}{}
			}
			if process.relay {
				relays[process.name] = struct{}{}
			}
		}
		report.Names = append(report.Names, dnsTraceName{
			Name: key.name, Type: key.queryType, Processes: slices.Sorted(maps.Keys(processes)), PIDs: slices.Sorted(maps.Keys(pids)), Relays: slices.Sorted(maps.Keys(relays)),
			Servers: slices.Sorted(maps.Keys(stats.servers)), Addresses: slices.Sorted(maps.Keys(stats.addresses)), Results: maps.Clone(stats.results), traceDNSCounts: stats.counts.finished(),
			processLabel: traceDNSProcessLabel(stats.processes),
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
	// 서버 쪽 질의는 다른 호스트의 client가 보낸 것이라 이 호스트의 검색 도메인이 맞지 않는다.
	search := summarizer.local.search
	if summarizer.side == traceServerSide {
		search = nil
	}
	report.Names = traceDNSGroupSearchNames(report.Names, search)
	report.RepeatedFailures = summarizer.repeatedFailures(search)
	return report
}

func (summarizer *dnsTraceSummarizer) repeatedFailures(search []string) []dnsRepeatedFailure {
	var failures []dnsRepeatedFailure
	for key, run := range summarizer.failures {
		if !run.regular() {
			continue
		}
		// 검색 도메인을 붙인 이름은 원래 이름과 같은 조회의 일부다. 원래 이름도 되풀이됐으면 원래 이름만 적는다.
		base, _ := traceDNSSearchBase(key.name, search, func(base string) bool {
			parent := key
			parent.name = base
			run := summarizer.failures[parent]
			return run != nil && run.regular()
		})
		if base != "" {
			continue
		}
		failures = append(failures, dnsRepeatedFailure{
			PID: key.pid, Process: key.process, Name: key.name, Type: key.queryType, Result: key.result, Count: run.count,
			IntervalMS: float64(run.last-run.first) / float64(run.count-1) / float64(time.Millisecond),
		})
	}
	sort.Slice(failures, func(i, j int) bool {
		left, right := failures[i], failures[j]
		if left.Count != right.Count {
			return left.Count > right.Count
		}
		if left.PID != right.PID {
			return left.PID < right.PID
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Type != right.Type {
			return left.Type < right.Type
		}
		return left.Result < right.Result
	})
	return failures
}

// traceDNSProcessLabel은 process 이름마다 PID를 붙이고, 중계한 resolver는 "via" 뒤에 둔다. 한 이름의 PID가 많으면 앞의
// dnsTraceShownPIDs개와 남은 수만 적는다.
func traceDNSProcessLabel(processes map[dnsTraceProcess]struct{}) string {
	type owner struct {
		name  string
		relay bool
	}
	pids := map[owner][]uint32{}
	for process := range processes {
		key := owner{name: process.name, relay: process.relay}
		list := pids[key]
		if process.pid != 0 {
			list = append(list, process.pid)
		}
		pids[key] = list
	}
	var origins, relays []string
	for _, key := range slices.SortedFunc(maps.Keys(pids), func(left, right owner) int { return strings.Compare(left.name, right.name) }) {
		label := key.name
		if list := pids[key]; len(list) > 0 {
			slices.Sort(list)
			parts := make([]string, 0, dnsTraceShownPIDs+1)
			for _, pid := range list[:min(len(list), dnsTraceShownPIDs)] {
				parts = append(parts, strconv.FormatUint(uint64(pid), 10))
			}
			if extra := len(list) - dnsTraceShownPIDs; extra > 0 {
				parts = append(parts, "+"+strconv.Itoa(extra))
			}
			label += "(" + strings.Join(parts, ",") + ")"
		}
		if key.relay {
			relays = append(relays, label)
		} else {
			origins = append(origins, label)
		}
	}
	label := strings.Join(origins, ",")
	if len(relays) > 0 {
		label = strings.TrimSpace(label + " via " + strings.Join(relays, ","))
	}
	return emptyAs(label, "-")
}

// traceDNSSearchBase는 name이 검색 도메인을 붙인 이름이면 원래 이름과 그 도메인의 순서를 돌려준다. 원래 이름도 seen이어야 한다.
// 검색 도메인 안의 host(web.loopvm)를 원래 이름 없이 물었으면 따로 둔다. 도메인이 겹치면 긴 쪽을 쓴다.
func traceDNSSearchBase(name string, search []string, seen func(string) bool) (string, int) {
	base, rank := "", -1
	for index, domain := range search {
		rest, ok := strings.CutSuffix(name, "."+domain)
		if ok && rest != "" && (rank < 0 || len(domain) > len(search[rank])) && seen(rest) {
			base, rank = rest, index
		}
	}
	return base, rank
}

// traceDNSGroupSearchNames는 검색 도메인을 붙여 다시 물은 이름을 원래 이름 바로 아래로 옮긴다. resolver가 도메인을 시도하는
// 순서를 따른다.
func traceDNSGroupSearchNames(names []dnsTraceName, search []string) []dnsTraceName {
	if len(search) == 0 {
		return names
	}
	seen := map[dnsTraceNameKey]bool{}
	for _, name := range names {
		seen[dnsTraceNameKey{name: name.Name, queryType: name.Type}] = true
	}
	baseOf := func(name, queryType string) (string, int) {
		return traceDNSSearchBase(name, search, func(base string) bool { return seen[dnsTraceNameKey{name: base, queryType: queryType}] })
	}
	type child struct {
		name dnsTraceName
		rank int
	}
	children := map[dnsTraceNameKey][]child{}
	top := make([]dnsTraceName, 0, len(names))
	for _, name := range names {
		// 원래 이름도 다른 이름 아래로 가면 이 행이 놓일 곳이 없다. 원래 이름이 맨 위 행일 때만 묶는다.
		if base, rank := baseOf(name.Name, name.Type); base != "" {
			if parent, _ := baseOf(base, name.Type); parent == "" {
				name.BaseName = base
				key := dnsTraceNameKey{name: base, queryType: name.Type}
				children[key] = append(children[key], child{name: name, rank: rank})
				continue
			}
		}
		top = append(top, name)
	}
	grouped := make([]dnsTraceName, 0, len(names))
	for _, name := range top {
		grouped = append(grouped, name)
		list := children[dnsTraceNameKey{name: name.Name, queryType: name.Type}]
		slices.SortStableFunc(list, func(left, right child) int { return left.rank - right.rank })
		for _, item := range list {
			grouped = append(grouped, item.name)
		}
	}
	return grouped
}

// print는 detail과 상관없이 같다. DNS 요약은 이미 이름과 record 종류마다 한 행이고, 질의마다의 행은 두지 않는다.
func (report dnsTraceReport) print(bool) {
	title := "DNS trace"
	if report.Side == traceServerSide {
		title = "DNS server trace"
	}
	out := os.Stdout
	style := newTraceSummaryStyle(out)
	style.line(out, style.title(title, (time.Duration(report.DurationMS)*time.Millisecond).String()))
	io.WriteString(out, "\n")
	style.fields(out, "", "Queries", fmt.Sprint(report.Queries), "Answers", fmt.Sprint(report.Answers), "Errors", fmt.Sprint(report.Errors), "Unanswered", fmt.Sprint(report.Unanswered), "TCP connections", fmt.Sprint(report.TCPConnections),
		"Latency avg", traceLatency(report.LatencyAvgMS, "ms"), "Latency max", traceLatency(report.LatencyMaxMS, "ms"), "Network avg", traceLatency(report.NetworkAvgMS, "ms"), "Network max", traceLatency(report.NetworkMaxMS, "ms"),
		"Read delay avg", traceLatency(report.ReadDelayAvgMS, "ms"), "Read delay max", traceLatency(report.ReadDelayMaxMS, "ms"), "Lost events", fmt.Sprint(report.LostEvents))
	if len(report.Names) == 0 {
		return
	}
	table := traceTable{titles: []string{"NAME", "TYPE", "QUERIES", "ANSWERS", "RESULTS", "UNANSWERED", "AVG", "MAX", "NET", "READ", "PROCESS"}}
	for _, name := range report.Names {
		label := name.Name
		if name.BaseName != "" {
			label = "  " + label
		}
		table.add(label, emptyAs(name.Type, "-"), fmt.Sprint(name.Queries), fmt.Sprint(name.Answers), traceResultCounts(name.Results), fmt.Sprint(name.Unanswered), traceLatency(name.LatencyAvgMS, "ms"), traceLatency(name.LatencyMaxMS, "ms"), traceLatency(name.NetworkAvgMS, "ms"), traceLatency(name.ReadDelayAvgMS, "ms"), emptyAs(name.processLabel, emptyAs(strings.Join(name.Processes, ","), "-")))
	}
	io.WriteString(out, "\n")
	style.writeTable(out, table)
	report.printRepeatedFailures(out, style)
}

// printRepeatedFailures는 같은 process가 일정한 간격으로 같은 실패를 받은 이름을 쓴다. NEXT는 그 process를 볼 명령이다.
func (report dnsTraceReport) printRepeatedFailures(out io.Writer, style traceSummaryStyle) {
	if len(report.RepeatedFailures) == 0 {
		return
	}
	io.WriteString(out, "\n")
	style.line(out, style.subheading("REPEATED FAILURES")+style.dim(": the same process got the same failed answer at a steady interval"))
	table := traceTable{titles: []string{"PROCESS", "NAME", "TYPE", "RESULT", "COUNT", "EVERY", "NEXT"}}
	for _, failure := range report.RepeatedFailures {
		table.add(fmt.Sprintf("%s(%d)", emptyAs(failure.Process, "-"), failure.PID), failure.Name, emptyAs(failure.Type, "-"), failure.Result, fmt.Sprint(failure.Count), traceDNSInterval(failure.IntervalMS), fmt.Sprintf("edc proc %d", failure.PID))
	}
	style.writeTable(out, table)
}

// traceDNSInterval은 되풀이 간격이다. 1초 이상은 0.1초 단위로 쓴다.
func traceDNSInterval(ms float64) string {
	if ms >= 1000 {
		return strconv.FormatFloat(ms/1000, 'f', 1, 64) + "s"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + "ms"
}
