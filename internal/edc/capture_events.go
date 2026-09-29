package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc captureEvents capture_events_bpf.c -- -I./bpf

import (
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
	LostEvents   uint64 `json:"lost_events,omitempty"`
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
	traceTraffic
}

type tcpTraceReport struct {
	DurationMS      int64                `json:"duration_ms"`
	Attempts        int                  `json:"attempts"`
	Established     int                  `json:"established"`
	Incomplete      int                  `json:"incomplete"`
	Retransmissions *uint64              `json:"retransmissions"`
	Resets          *int                 `json:"resets"`
	LostEvents      uint64               `json:"lost_events"`
	Connections     []tcpTraceConnection `json:"connections"`
	// ConnectionsOmitted는 합계에는 들어갔지만 행을 남기지 않은 끝난 연결의 수다.
	ConnectionsOmitted int `json:"connections_omitted"`
	traceTraffic
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
	yes         bool
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
	traffic   traceTraffic
}

type tcpTraceTotals struct {
	attempts        int
	established     int
	incomplete      int
	retransmissions uint64
	resets          int
}

func (totals *tcpTraceTotals) add(connection *tcpTraceConnection) {
	totals.attempts++
	totals.retransmissions += connection.retransmissions
	if connection.reset {
		totals.resets++
	}
	if connection.Result == "established" {
		totals.established++
	} else {
		totals.incomplete++
	}
}

func newTCPTraceSummarizer() *tcpTraceSummarizer {
	return &tcpTraceSummarizer{active: make(map[uint64]*tcpTraceConnection), firstSeen: make(map[uint64]uint64)}
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
		connection = &tcpTraceConnection{Process: event.Process, PID: event.PID, Source: event.Source, Destination: event.Destination, Result: "incomplete"}
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
	switch event.Event {
	case "tcp_send", "tcp_receive":
		connection.traceTraffic.observe(event)
	case "tcp_connect", "tcp_accept":
		connection.Result = "established"
		if firstSeen[event.SocketID] > 0 && event.TimestampNS >= firstSeen[event.SocketID] {
			connection.connectMS = int64(event.TimestampNS-firstSeen[event.SocketID]) / int64(time.Millisecond)
		}
	case "tcp_retransmit":
		connection.retransmissions++
	case "tcp_send_reset", "tcp_receive_reset":
		connection.reset = true
		connection.Result = "reset"
	case "tcp_close":
		connection.closed = true
		if connection.Result == "incomplete" {
			connection.Result = "closed"
		}
	case "tcp_destroy":
		summarizer.finish(event.SocketID)
	}
}

// finish는 끝난 연결을 합계에 넣고, 최근 tcpTraceConnectionLimit개만 행으로 남긴다.
func (summarizer *tcpTraceSummarizer) finish(socket uint64) {
	connection := summarizer.active[socket]
	delete(summarizer.active, socket)
	delete(summarizer.firstSeen, socket)
	summarizer.totals.add(connection)
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
	result.Connections = make([]tcpTraceConnection, 0, len(summarizer.finished)+len(summarizer.active))
	result.Connections = append(result.Connections, summarizer.finished...)
	for _, connection := range summarizer.active {
		totals.add(connection)
		result.Connections = append(result.Connections, *connection)
	}
	for index := range result.Connections {
		connection := &result.Connections[index]
		connection.traceTraffic.finalize(duration)
		connection.ConnectMS = traceObserved(connection.connectMS)
		connection.Retransmissions = traceObserved(connection.retransmissions)
		connection.Reset = traceObserved(connection.reset)
	}
	result.Attempts, result.Established, result.Incomplete = totals.attempts, totals.established, totals.incomplete
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
