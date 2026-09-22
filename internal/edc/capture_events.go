package edc

import (
	"sort"
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
	Event       string `json:"event"`
	Protocol    string `json:"protocol"`
	PID         uint32 `json:"pid"`
	Process     string `json:"process"`
	Target      string `json:"target,omitempty"`
	CgroupID    uint64 `json:"cgroup_id"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	OldState    string `json:"old_state,omitempty"`
	NewState    string `json:"new_state,omitempty"`
	Bytes       uint64 `json:"bytes"`
	LostEvents  uint64 `json:"lost_events,omitempty"`
}

type captureSummary struct {
	TimestampNS uint64 `json:"timestamp_ns"`
	Event       string `json:"event"`
	EventCount  uint64 `json:"event_count"`
	LostEvents  uint64 `json:"lost_events"`
}

type tcpTraceConnection struct {
	Process         string `json:"process"`
	PID             uint32 `json:"pid"`
	Source          string `json:"source,omitempty"`
	Destination     string `json:"destination,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	Result          string `json:"result"`
	ConnectMS       int64  `json:"connect_ms"`
	Retransmissions uint64 `json:"retransmissions"`
	Reset           bool   `json:"reset"`
	traceTraffic
}

type tcpTraceReport struct {
	DurationMS      int64                `json:"duration_ms"`
	Attempts        int                  `json:"attempts"`
	Established     int                  `json:"established"`
	Incomplete      int                  `json:"incomplete"`
	Retransmissions uint64               `json:"retransmissions"`
	Resets          int                  `json:"resets"`
	LostEvents      uint64               `json:"lost_events"`
	Connections     []tcpTraceConnection `json:"connections"`
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
	connections := make(map[uint64]*tcpTraceConnection)
	firstSeen := make(map[uint64]uint64)
	result := tcpTraceReport{DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents}
	for _, event := range events {
		if traceProtocol(event) != "tcp" || !traceEventMatches(event, process, destination) {
			continue
		}
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
		if connection.Source == "" {
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
				connection.ConnectMS = int64(event.TimestampNS-firstSeen[event.SocketID]) / int64(time.Millisecond)
			}
		case "tcp_retransmit":
			connection.Retransmissions++
		case "tcp_send_reset", "tcp_receive_reset":
			connection.Reset = true
			connection.Result = "reset"
		case "tcp_close":
			if connection.Result == "incomplete" {
				connection.Result = "closed"
			}
		}
		result.traceTraffic.observe(event)
	}
	result.Connections = make([]tcpTraceConnection, 0, len(connections))
	for _, connection := range connections {
		connection.traceTraffic.finalize(duration)
		result.Connections = append(result.Connections, *connection)
		result.Attempts++
		result.Retransmissions += connection.Retransmissions
		if connection.Reset {
			result.Resets++
		}
		if connection.Result == "established" {
			result.Established++
		} else {
			result.Incomplete++
		}
	}
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

func captureEventName(oldState, newState uint32) string {
	if newState == 2 && oldState == 10 {
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
