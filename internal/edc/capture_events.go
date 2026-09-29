package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc captureEvents capture_events_bpf.c -- -I./bpf

import (
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

type captureEventsOptions struct {
	duration time.Duration
	output   string
	yes      bool
}

type captureEvent struct {
	SocketID    uint64 `json:"-"`
	TimestampNS uint64 `json:"timestamp_ns"`
	BootTimeNS  uint64 `json:"boot_time_ns"`
	Event       string `json:"event"`
	Protocol    string `json:"protocol"`
	PID         uint32 `json:"pid"`
	Process     string `json:"process"`
	Target      string `json:"target,omitempty"`
	// TargetSource는 target을 명령줄(command), 이 프로세스의 DNS 응답(dns), resolver 캐시(resolver-cache),
	// macOS가 연결에 기록한 이름(system) 중 어디서 얻었는지 알린다. 주소를 여러 이름이 공유하면 dns와
	// resolver-cache 이름이 틀릴 수 있다.
	TargetSource string `json:"target_source,omitempty"`
	CgroupID     uint64 `json:"cgroup_id"`
	Source       string `json:"source,omitempty"`
	Destination  string `json:"destination,omitempty"`
	OldState     string `json:"old_state,omitempty"`
	NewState     string `json:"new_state,omitempty"`
	Bytes        uint64 `json:"bytes"`
	Packets      uint64 `json:"packets,omitempty"`
	// QueryType, Answers, LatencyMS는 DNS event에만 붙는다. LatencyMS는 kernel이 질의를 보낸 때부터 process가
	// 응답을 읽은 때까지라서 process가 응답을 늦게 읽으면 그만큼 길어진다.
	QueryType string   `json:"query_type,omitempty"`
	Answers   []string `json:"answers,omitempty"`
	LatencyMS *float64 `json:"latency_ms,omitempty"`
	// NetworkMS는 client가 질의를 보낸 때부터 응답이 socket 수신 큐에 들어간 때까지다. ReadDelayMS는 받은 message가
	// 수신 큐에 들어간 때부터 process가 읽은 때까지다. client 쪽은 응답에, 서버 쪽은 질의에 붙는다.
	NetworkMS   *float64 `json:"network_ms,omitempty"`
	ReadDelayMS *float64 `json:"read_delay_ms,omitempty"`
	// MAC과 OldMAC은 ARP event에만 붙는다. OldMAC은 MAC이 바뀌었을 때 이전 값이다.
	MAC    string `json:"mac,omitempty"`
	OldMAC string `json:"old_mac,omitempty"`
	// Side는 로컬 DNS 서버가 받은 질의와 보낸 응답에만 server로 붙는다.
	Side       string `json:"side,omitempty"`
	LostEvents uint64 `json:"lost_events,omitempty"`
	// dnsAnswered는 DNS 응답이 답한 질의 수다. 응답 전에 같은 질의를 다시 보냈으면 1보다 크다.
	dnsAnswered uint64
}

type captureSummary struct {
	TimestampNS uint64 `json:"timestamp_ns"`
	Event       string `json:"event"`
	EventCount  uint64 `json:"event_count"`
	LostEvents  uint64 `json:"lost_events"`
}

type tcpTraceConnection struct {
	Process         string  `json:"process"`
	PID             uint32  `json:"pid"`
	Source          string  `json:"source,omitempty"`
	Destination     string  `json:"destination,omitempty"`
	Hostname        string  `json:"hostname,omitempty"`
	Result          string  `json:"result"`
	ConnectMS       *int64  `json:"connect_ms"`
	Retransmissions *uint64 `json:"retransmissions"`
	Reset           *bool   `json:"reset"`
	connectMS       int64
	retransmissions uint64
	reset           bool
	closed          bool
	established     bool
	handshake       bool
	listener        bool
	portSeen        bool
	portless        bool
	traceTraffic
}

// result는 행의 결과다. 연결된 뒤 RST가 와도 established로 둔다. RST 여부는 Reset이 따로 보여 준다.
// 핸드셰이크를 보지 못한 연결은 trace 전부터 열려 있던 것이다.
func (connection *tcpTraceConnection) result() string {
	switch {
	case connection.established:
		return "established"
	case connection.unbound():
		return "failed"
	case !connection.handshake:
		return "existing"
	case connection.closed || connection.reset:
		return "failed"
	default:
		return "incomplete"
	}
}

// unbound는 local port를 한 번도 갖지 못한 socket이다. connect()가 SYN_SENT 전에 실패하면(경로가 없는 IPv6 등)
// kernel이 port를 되돌려서 destroy만 남는다. 주소가 없는 event만 본 socket은 근거가 없어 여기에 넣지 않는다.
func (connection *tcpTraceConnection) unbound() bool {
	return connection.portless && !connection.portSeen && !connection.handshake && !connection.established
}

// counted는 행과 합계에 넣을 socket이다. listen socket과, connect하지 않고 닫힌 socket은 연결이 아니다.
func (connection *tcpTraceConnection) counted() bool {
	return !connection.listener && !(connection.unbound() && traceAddressUnspecified(connection.Destination))
}

func traceAddressUnspecified(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address == ""
	}
	parsed, err := netip.ParseAddr(host)
	return err == nil && parsed.IsUnspecified()
}

func tcpTraceHandshakeState(state string) bool {
	return state == "SYN_SENT" || state == "SYN_RECV"
}

type tcpTraceReport struct {
	DurationMS      int64                `json:"duration_ms"`
	Attempts        int                  `json:"attempts"`
	Established     int                  `json:"established"`
	Incomplete      int                  `json:"incomplete"`
	Existing        int                  `json:"existing"`
	Retransmissions *uint64              `json:"retransmissions"`
	Resets          *int                 `json:"resets"`
	LostEvents      uint64               `json:"lost_events"`
	Connections     []tcpTraceConnection `json:"connections"`
	// ConnectionsOmitted는 합계에는 들어갔지만 행을 남기지 않은 끝난 연결의 수다.
	ConnectionsOmitted int `json:"connections_omitted"`
	// groups는 텍스트 요약의 기본 행이다. JSON은 연결별 행만 쓴다.
	groups []tcpTraceGroupRow
	traceTraffic
}

// tcpTraceGroupRow는 같은 process가 같은 상대와 맺은 연결을 묶은 행이다. 서버 연결의 상대는 client 포트가
// 매번 달라서 local 서비스로 묶는다.
type tcpTraceGroupRow struct {
	process         string
	peer            string
	hostname        string
	server          bool
	connections     int
	established     int
	incomplete      int
	existing        int
	resets          int
	retransmissions uint64
	connectTotalMS  int64
	connectCount    int
	traceTraffic
}

type tcpTraceGroupKey struct {
	process string
	peer    string
	server  bool
}

// traceSummaryPeer는 묶음 행의 상대와 서버 여부다. 그룹 보기의 서버 판정과 같다.
func traceSummaryPeer(source, destination string) (string, bool) {
	low, high := traceEphemeralPortRange()
	if service, ok := traceServerService(captureEvent{Source: source, Destination: destination}, low, high); ok {
		return service, true
	}
	return destination, false
}

func addTCPTraceGroup(groups map[tcpTraceGroupKey]*tcpTraceGroupRow, connection *tcpTraceConnection) {
	peer, server := traceSummaryPeer(connection.Source, connection.Destination)
	key := tcpTraceGroupKey{process: connection.Process, peer: peer, server: server}
	row := groups[key]
	if row == nil {
		row = &tcpTraceGroupRow{process: connection.Process, peer: peer, server: server}
		groups[key] = row
	}
	// 서버 행의 target은 한 client의 이름이라 행 전체를 설명하지 못한다.
	if row.hostname == "" && !server {
		row.hostname = connection.Hostname
	}
	row.connections++
	switch connection.Result {
	case "established":
		row.established++
	case "existing":
		row.existing++
	default:
		row.incomplete++
	}
	if connection.reset {
		row.resets++
	}
	row.retransmissions += connection.retransmissions
	if connection.established {
		row.connectTotalMS += connection.connectMS
		row.connectCount++
	}
	row.TXBytes += connection.TXBytes
	row.RXBytes += connection.RXBytes
}

func sortTCPTraceGroups(rows []tcpTraceGroupRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].process != rows[j].process {
			return rows[i].process < rows[j].process
		}
		if rows[i].peer != rows[j].peer {
			return rows[i].peer < rows[j].peer
		}
		return !rows[i].server && rows[j].server
	})
}

type traceTraffic struct {
	TXBytes           uint64  `json:"tx_bytes"`
	RXBytes           uint64  `json:"rx_bytes"`
	TotalBytes        uint64  `json:"total_bytes"`
	BytesPerSecond    float64 `json:"bytes_per_second"`
	BitsPerSecond     float64 `json:"bits_per_second"`
	MegabitsPerSecond float64 `json:"megabits_per_second"`
}

func (traffic *traceTraffic) observe(event captureEvent) {
	switch event.Event {
	case "tcp_send", "udp_send":
		traffic.TXBytes += event.Bytes
	case "tcp_receive", "udp_receive":
		traffic.RXBytes += event.Bytes
	}
}

func (traffic *traceTraffic) finalize(duration time.Duration) {
	traffic.TotalBytes = traffic.TXBytes + traffic.RXBytes
	seconds := duration.Seconds()
	if seconds <= 0 {
		return
	}
	traffic.BytesPerSecond = float64(traffic.TotalBytes) / seconds
	traffic.BitsPerSecond = traffic.BytesPerSecond * 8
	traffic.MegabitsPerSecond = traffic.BitsPerSecond / 1_000_000
}

type tcpTraceOptions struct {
	duration    time.Duration
	jsonPath    string
	raw         bool
	live        bool
	groupBy     string
	process     string
	destination string
	detail      bool
	yes         bool
	// side는 trace dns가 볼 쪽이다. client는 이 host의 조회, server는 로컬 DNS 서버가 받은 질의다.
	side string
}

func traceProtocol(event captureEvent) string {
	if event.Protocol != "" {
		return event.Protocol
	}
	return "tcp"
}

func captureEventTypeName(eventType uint32, protocol uint16) (string, string) {
	switch eventType {
	case 2:
		return "tcp_retransmit", "tcp"
	case 3:
		return "tcp_send_reset", "tcp"
	case 4:
		return "tcp_receive_reset", "tcp"
	case 5:
		return "tcp_destroy", "tcp"
	case 6:
		if protocol == 17 {
			return "udp_send", "udp"
		}
		return "tcp_send", "tcp"
	case 7:
		if protocol == 17 {
			return "udp_receive", "udp"
		}
		return "tcp_receive", "tcp"
	default:
		return "tcp_event", "tcp"
	}
}

func summarizeTCPTrace(events []captureEvent, summary captureSummary, duration time.Duration, process, destination string) tcpTraceReport {
	summarizer := newTCPTraceSummarizer()
	for _, event := range events {
		if traceProtocol(event) != "tcp" || !traceEventMatches(event, process, destination) {
			continue
		}
		summarizer.observe(event)
	}
	return summarizer.report(summary, duration)
}

// tcpTraceConnectionLimit는 요약에 행으로 남길 끝난 연결의 수다. 합계는 모든 연결로 센다. 짧은 연결이 많으면
// 연결이 분당 수십만 개라, 모두 남기면 메모리와 종료 시 출력이 연결 수만큼 커진다.
const tcpTraceConnectionLimit = 1000

// tcpTraceSummarizer는 event를 하나씩 받아 TCP 요약을 쌓는다. 연결은 socket의 수명마다 나눈다. SocketID는
// kernel의 socket 주소라서, socket이 없어진 뒤 새 socket이 같은 값을 다시 쓴다.
type tcpTraceSummarizer struct {
	active    map[uint64]*tcpTraceConnection
	firstSeen map[uint64]uint64
	finished  []tcpTraceConnection
	oldest    int
	omitted   int
	totals    tcpTraceTotals
	groups    map[tcpTraceGroupKey]*tcpTraceGroupRow
	traffic   traceTraffic
}

// tcpTraceTotals에서 attempts는 trace 중에 핸드셰이크를 본 연결이고, established와 incomplete의 합이다.
// trace 전부터 열려 있던 연결은 existing으로 따로 센다.
type tcpTraceTotals struct {
	attempts        int
	established     int
	incomplete      int
	existing        int
	retransmissions uint64
	resets          int
}

func (totals *tcpTraceTotals) add(connection *tcpTraceConnection) {
	totals.retransmissions += connection.retransmissions
	if connection.reset {
		totals.resets++
	}
	switch connection.Result {
	case "established":
		totals.attempts++
		totals.established++
	case "existing":
		totals.existing++
	default:
		totals.attempts++
		totals.incomplete++
	}
}

func newTCPTraceSummarizer() *tcpTraceSummarizer {
	return &tcpTraceSummarizer{active: make(map[uint64]*tcpTraceConnection), firstSeen: make(map[uint64]uint64), groups: make(map[tcpTraceGroupKey]*tcpTraceGroupRow)}
}

// tcpTraceConnectionStarts는 새 socket의 수명이 시작되는 전이다. connect()와 서버의 새 연결이 여기서 시작한다.
func tcpTraceConnectionStarts(event captureEvent) bool {
	return event.Event == "tcp_state" && (event.OldState == "CLOSE" && event.NewState == "SYN_SENT" || event.OldState == "LISTEN" && event.NewState == "SYN_RECV")
}

func (summarizer *tcpTraceSummarizer) observe(event captureEvent) {
	summarizer.traffic.observe(event)
	// socket 없이 보낸 RST처럼 SocketID가 0인 event는 연결이 아니다. 행을 만들면 모두 한 행에 쌓인다.
	if event.SocketID == 0 {
		return
	}
	// tcp_destroy를 놓쳤어도, 닫힌 socket 주소에 새 연결이 시작되면 다른 socket이다.
	if connection := summarizer.active[event.SocketID]; connection != nil && connection.closed && tcpTraceConnectionStarts(event) {
		summarizer.finish(event.SocketID)
	}
	connections, firstSeen := summarizer.active, summarizer.firstSeen
	connection := connections[event.SocketID]
	if connection == nil {
		connection = &tcpTraceConnection{Process: event.Process, PID: event.PID, Source: event.Source, Destination: event.Destination}
		connections[event.SocketID] = connection
		firstSeen[event.SocketID] = event.TimestampNS
	}
	if connection.Process == "" && event.Process != "" {
		connection.Process = event.Process
	}
	if connection.PID == 0 && event.PID != 0 {
		connection.PID = event.PID
	}
	if connection.Hostname == "" && event.Target != "" {
		connection.Hostname = event.Target
	}
	// connect()의 첫 전이는 port를 배정하기 전이라 source port가 0이다. 뒤 event의 source로 바꾼다.
	if connection.Source == "" || strings.HasSuffix(connection.Source, ":0") && event.Source != "" && !strings.HasSuffix(event.Source, ":0") {
		connection.Source = event.Source
	}
	if connection.Destination == "" {
		connection.Destination = event.Destination
	}
	// SYN 재전송과 거부된 connect의 close도 이전 상태가 SYN_SENT라서, trace 전에 시작한 시도까지 잡힌다.
	if tcpTraceHandshakeState(event.OldState) || tcpTraceHandshakeState(event.NewState) {
		connection.handshake = true
	}
	// listen socket은 연결이 아니다. 서버의 새 연결을 알리는 LISTEN→SYN_RECV는 자식 socket의 event다.
	if event.NewState == "LISTEN" || event.OldState == "LISTEN" && event.NewState == "CLOSE" {
		connection.listener = true
	}
	if event.Source != "" {
		if strings.HasSuffix(event.Source, ":0") {
			connection.portless = true
		} else {
			connection.portSeen = true
		}
	}
	switch event.Event {
	case "tcp_send", "tcp_receive":
		connection.traceTraffic.observe(event)
	case "tcp_connect", "tcp_accept":
		connection.established = true
		connection.handshake = true
		if firstSeen[event.SocketID] > 0 && event.TimestampNS >= firstSeen[event.SocketID] {
			connection.connectMS = int64(event.TimestampNS-firstSeen[event.SocketID]) / int64(time.Millisecond)
		}
	case "tcp_retransmit":
		connection.retransmissions++
	case "tcp_send_reset", "tcp_receive_reset":
		connection.reset = true
	case "tcp_close":
		connection.closed = true
	case "tcp_destroy":
		connection.closed = true
		summarizer.finish(event.SocketID)
	}
}

// finish는 끝난 연결을 합계에 넣고, 최근 tcpTraceConnectionLimit개만 행으로 남긴다.
func (summarizer *tcpTraceSummarizer) finish(socket uint64) {
	connection := summarizer.active[socket]
	delete(summarizer.active, socket)
	delete(summarizer.firstSeen, socket)
	if !connection.counted() {
		return
	}
	connection.Result = connection.result()
	summarizer.totals.add(connection)
	// 묶음 행은 끝난 연결마다 쌓아서, 연결별 행에서 빠진 연결도 들어간다.
	addTCPTraceGroup(summarizer.groups, connection)
	if len(summarizer.finished) < tcpTraceConnectionLimit {
		summarizer.finished = append(summarizer.finished, *connection)
		return
	}
	summarizer.finished[summarizer.oldest] = *connection
	summarizer.oldest = (summarizer.oldest + 1) % tcpTraceConnectionLimit
	summarizer.omitted++
}

func (summarizer *tcpTraceSummarizer) report(summary captureSummary, duration time.Duration) tcpTraceReport {
	result := tcpTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, ConnectionsOmitted: summarizer.omitted, traceTraffic: summarizer.traffic}
	totals := summarizer.totals
	groups := make(map[tcpTraceGroupKey]*tcpTraceGroupRow, len(summarizer.groups))
	for key, stored := range summarizer.groups {
		row := *stored
		groups[key] = &row
	}
	result.Connections = make([]tcpTraceConnection, 0, len(summarizer.finished)+len(summarizer.active))
	result.Connections = append(result.Connections, summarizer.finished...)
	for _, stored := range summarizer.active {
		if !stored.counted() {
			continue
		}
		connection := *stored
		connection.Result = connection.result()
		totals.add(&connection)
		addTCPTraceGroup(groups, &connection)
		result.Connections = append(result.Connections, connection)
	}
	result.groups = make([]tcpTraceGroupRow, 0, len(groups))
	for _, row := range groups {
		row.traceTraffic.finalize(duration)
		result.groups = append(result.groups, *row)
	}
	sortTCPTraceGroups(result.groups)
	for index := range result.Connections {
		connection := &result.Connections[index]
		connection.traceTraffic.finalize(duration)
		// 연결되지 않은 행에 0ms를 쓰면 바로 연결된 것처럼 보인다.
		connection.ConnectMS = nil
		if connection.established {
			connection.ConnectMS = traceObserved(connection.connectMS)
		}
		connection.Retransmissions = traceObserved(connection.retransmissions)
		connection.Reset = traceObserved(connection.reset)
	}
	result.Attempts, result.Established, result.Incomplete, result.Existing = totals.attempts, totals.established, totals.incomplete, totals.existing
	result.Retransmissions = traceObserved(totals.retransmissions)
	result.Resets = traceObserved(totals.resets)
	result.traceTraffic.finalize(duration)
	sort.Slice(result.Connections, func(i, j int) bool {
		left, right := result.Connections[i], result.Connections[j]
		if left.Process != right.Process {
			return left.Process < right.Process
		}
		if left.Destination != right.Destination {
			return left.Destination < right.Destination
		}
		return left.Source < right.Source
	})
	return result
}

func traceEventMatches(event captureEvent, process, destination string) bool {
	if process != "" && event.Process != process {
		return false
	}
	return destination == "" || event.Destination == destination || event.Target == destination
}

// captureEventName은 TCP 상태 전이를 이름 붙인다. 상태 값은 kernel의 enum과 같다:
// 1 ESTABLISHED, 2 SYN_SENT, 3 SYN_RECV, 7 CLOSE, 10 LISTEN.
func captureEventName(oldState, newState uint32) string {
	// accept는 새로 만든 소켓이 SYN_RECV에서 ESTABLISHED로 바뀌는 전이다. 듣고 있던
	// 소켓 자체는 LISTEN에 그대로 남으므로 LISTEN→SYN_SENT는 나타나지 않는다.
	if newState == 1 && oldState == 3 {
		return "tcp_accept"
	}
	if newState == 1 && oldState == 2 {
		return "tcp_connect"
	}
	if newState == 7 {
		return "tcp_close"
	}
	return "tcp_state"
}

func tcpStateName(state uint32) string {
	switch state {
	case 1:
		return "ESTABLISHED"
	case 2:
		return "SYN_SENT"
	case 3:
		return "SYN_RECV"
	case 4:
		return "FIN_WAIT1"
	case 5:
		return "FIN_WAIT2"
	case 6:
		return "TIME_WAIT"
	case 7:
		return "CLOSE"
	case 8:
		return "CLOSE_WAIT"
	case 9:
		return "LAST_ACK"
	case 10:
		return "LISTEN"
	case 11:
		return "CLOSING"
	case 12:
		return "NEW_SYN_RECV"
	default:
		return "UNKNOWN"
	}
}
