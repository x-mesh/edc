package edc

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestCaptureSupportedOS(t *testing.T) {
	for goos, want := range map[string]bool{"darwin": true, "linux": true, "freebsd": false, "windows": false} {
		if got := captureSupportedOS(goos); got != want {
			t.Fatalf("captureSupportedOS(%q) = %t, want %t", goos, got, want)
		}
	}
}

func TestCaptureEventNames(t *testing.T) {
	cases := []struct {
		oldState uint32
		newState uint32
		want     string
	}{
		// accept는 새 소켓이 SYN_RECV(3)에서 ESTABLISHED(1)로 바뀌는 전이다.
		{3, 1, "tcp_accept"},
		{2, 1, "tcp_connect"},
		{1, 7, "tcp_close"},
		{1, 8, "tcp_state"},
	}
	for _, test := range cases {
		if got := captureEventName(test.oldState, test.newState); got != test.want {
			t.Fatalf("captureEventName(%d, %d) = %q, want %q", test.oldState, test.newState, got, test.want)
		}
	}
}

func TestCaptureLengthEventTypesUseProtocol(t *testing.T) {
	cases := []struct {
		eventType uint32
		protocol  uint16
		wantName  string
		wantProto string
	}{
		{6, 6, "tcp_send", "tcp"},
		{7, 6, "tcp_receive", "tcp"},
		{6, 17, "udp_send", "udp"},
		{7, 17, "udp_receive", "udp"},
	}
	for _, test := range cases {
		name, protocol := captureEventTypeName(test.eventType, test.protocol)
		if name != test.wantName || protocol != test.wantProto {
			t.Fatalf("captureEventTypeName(%d, %d) = %q, %q", test.eventType, test.protocol, name, protocol)
		}
	}
}

func TestCaptureEventJSONIncludesBytes(t *testing.T) {
	encoded, err := json.Marshal(captureEvent{Event: "tcp_send", Bytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "\"bytes\":512") {
		t.Fatalf("event JSON = %s", encoded)
	}
}

func TestCaptureSummaryIncludesCounts(t *testing.T) {
	encoded, err := json.Marshal(captureSummary{Event: "capture_summary"})
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	if !strings.Contains(output, "\"event_count\":0") || !strings.Contains(output, "\"lost_events\":0") {
		t.Fatalf("summary = %s", output)
	}
}

func setTraceKernelEvents(t *testing.T, value bool) {
	t.Helper()
	previous := traceKernelEvents
	traceKernelEvents = value
	t.Cleanup(func() { traceKernelEvents = previous })
}

// setTraceEphemeralPortRange는 서버 응답 판정을 실행하는 host의 port 범위와 떼어 놓는다. macOS와 Linux는
// 기본 범위가 다르다.
func setTraceEphemeralPortRange(t *testing.T, low, high int) {
	t.Helper()
	previous := traceEphemeralPortRange
	traceEphemeralPortRange = func() (int, int) { return low, high }
	t.Cleanup(func() { traceEphemeralPortRange = previous })
}

func TestSummarizeTCPTrace(t *testing.T) {
	setTraceKernelEvents(t, true)
	base := uint64(time.Second)
	events := []captureEvent{
		{SocketID: 1, TimestampNS: base, Event: "tcp_state", Process: "worker", Source: "10.0.0.2:41000", Destination: "203.0.113.10:443", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 1, TimestampNS: base + 41*uint64(time.Millisecond), Event: "tcp_connect", Process: "swapper/0", PID: 0, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443", Target: "example.com"},
		{SocketID: 1, TimestampNS: base + 42*uint64(time.Millisecond), Event: "tcp_retransmit", Destination: "203.0.113.10:443"},
		{SocketID: 1, TimestampNS: base + 43*uint64(time.Millisecond), Event: "tcp_send", Bytes: 1200, Destination: "203.0.113.10:443"},
		{SocketID: 1, TimestampNS: base + 44*uint64(time.Millisecond), Event: "tcp_receive", Bytes: 800, Destination: "203.0.113.10:443"},
		{SocketID: 2, TimestampNS: base, Event: "tcp_state", Process: "bot", Source: "10.0.0.2:41001", Destination: "203.0.113.20:443", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 2, TimestampNS: base + 83*uint64(time.Millisecond), Event: "tcp_receive_reset", Process: "bot", Destination: "203.0.113.20:443"},
	}
	report := summarizeTCPTrace(events, captureSummary{LostEvents: 3}, 15*time.Second, "", "")
	if report.Attempts != 2 || report.Established != 1 || report.Incomplete != 1 || traceOptional(report.Retransmissions, "%d") != "1" || traceOptional(report.Resets, "%d") != "1" || report.LostEvents != 3 {
		t.Fatalf("report = %#v", report)
	}
	var established, reset *tcpTraceConnection
	for index := range report.Connections {
		switch report.Connections[index].Result {
		case "established":
			established = &report.Connections[index]
		case "failed":
			reset = &report.Connections[index]
		}
	}
	if established == nil || traceOptional(established.ConnectMS, "%d") != "41" || traceOptional(established.Retransmissions, "%d") != "1" {
		t.Fatalf("established connection = %#v", established)
	}
	if established.TXBytes != 1200 || established.RXBytes != 800 || established.TotalBytes != 2000 {
		t.Fatalf("established traffic = %#v", established.traceTraffic)
	}
	if report.TXBytes != 1200 || report.RXBytes != 800 || report.TotalBytes != 2000 || report.BytesPerSecond != 2000.0/15 || report.BitsPerSecond != 16000.0/15 || report.MegabitsPerSecond != 0.016/15 {
		t.Fatalf("TCP traffic = %#v", report.traceTraffic)
	}
	if established.Hostname != "example.com" || traceDestinationLabel(*established) != "203.0.113.10:443 (example.com)" {
		t.Fatalf("hostname = %#v", established)
	}
	if reset == nil || traceOptional(reset.Reset, "%t") != "true" {
		t.Fatalf("reset connection = %#v", reset)
	}
	filtered := summarizeTCPTrace(events, captureSummary{}, time.Second, "bot", "")
	if filtered.Attempts != 1 || filtered.Connections[0].Process != "bot" {
		t.Fatalf("filtered report = %#v", filtered)
	}
}

func TestSummarizeTCPTraceSplitsSocketLives(t *testing.T) {
	setTraceKernelEvents(t, true)
	ms := uint64(time.Millisecond)
	events := []captureEvent{
		// kernel이 socket 주소 7을 두 연결에 차례로 쓴다.
		{SocketID: 7, TimestampNS: 1 * ms, Event: "tcp_state", Process: "curl", Source: "10.0.0.2:0", Destination: "203.0.113.10:443", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 7, TimestampNS: 3 * ms, Event: "tcp_connect", Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{SocketID: 7, TimestampNS: 4 * ms, Event: "tcp_send", Bytes: 100, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{SocketID: 7, TimestampNS: 5 * ms, Event: "tcp_close", Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{SocketID: 7, TimestampNS: 6 * ms, Event: "tcp_destroy", Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{SocketID: 7, TimestampNS: 7 * ms, Event: "tcp_state", Process: "bot", Source: "10.0.0.2:0", Destination: "203.0.113.20:80", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 7, TimestampNS: 8 * ms, Event: "tcp_receive_reset", Source: "10.0.0.2:41001", Destination: "203.0.113.20:80"},
		{SocketID: 7, TimestampNS: 9 * ms, Event: "tcp_close", Source: "10.0.0.2:41001", Destination: "203.0.113.20:80"},
		// tcp_destroy를 놓친 socket 8에 새 연결이 시작된다.
		{SocketID: 8, TimestampNS: 10 * ms, Event: "tcp_connect", Process: "agent", Source: "10.0.0.2:41002", Destination: "203.0.113.30:443"},
		{SocketID: 8, TimestampNS: 11 * ms, Event: "tcp_close", Source: "10.0.0.2:41002", Destination: "203.0.113.30:443"},
		{SocketID: 8, TimestampNS: 12 * ms, Event: "tcp_state", Process: "agent", Source: "10.0.0.2:0", Destination: "203.0.113.30:443", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 8, TimestampNS: 14 * ms, Event: "tcp_connect", Source: "10.0.0.2:41003", Destination: "203.0.113.30:443"},
		// socket 없이 보낸 RST는 연결 행을 만들지 않는다.
		{SocketID: 0, TimestampNS: 15 * ms, Event: "tcp_send_reset"},
	}
	report := summarizeTCPTrace(events, captureSummary{}, time.Second, "", "")
	if report.Attempts != 4 || report.Established != 3 || report.Incomplete != 1 || traceOptional(report.Resets, "%d") != "1" || report.ConnectionsOmitted != 0 || len(report.Connections) != 4 {
		t.Fatalf("report = %#v", report)
	}
	rows := map[string]tcpTraceConnection{}
	for _, connection := range report.Connections {
		rows[connection.Process+" "+connection.Source] = connection
	}
	curl, bot := rows["curl 10.0.0.2:41000"], rows["bot 10.0.0.2:41001"]
	if curl.Result != "established" || curl.TXBytes != 100 || traceOptional(curl.ConnectMS, "%d") != "2" || bot.Result != "failed" || bot.TXBytes != 0 {
		t.Fatalf("rows = %#v", rows)
	}
	if _, ok := rows["agent 10.0.0.2:41002"]; !ok {
		t.Fatalf("first agent connection is missing: %#v", rows)
	}
	if second, ok := rows["agent 10.0.0.2:41003"]; !ok || traceOptional(second.ConnectMS, "%d") != "2" {
		t.Fatalf("second agent connection = %#v", rows)
	}
}

func TestSummarizeTCPTraceKeepsRecentConnectionRows(t *testing.T) {
	setTraceKernelEvents(t, true)
	events := []captureEvent{}
	for index := range tcpTraceConnectionLimit + 5 {
		socket := uint64(index + 1)
		source := fmt.Sprintf("10.0.0.2:%d", 30000+index)
		events = append(events,
			captureEvent{SocketID: socket, TimestampNS: uint64(index*10 + 1), Event: "tcp_connect", Process: "probe", Source: source, Destination: "127.0.0.1:8080"},
			captureEvent{SocketID: socket, TimestampNS: uint64(index*10 + 2), Event: "tcp_destroy", Source: source, Destination: "127.0.0.1:8080"},
		)
	}
	events = append(events, captureEvent{SocketID: 99999, TimestampNS: 1 << 40, Event: "tcp_send", Bytes: 10, Process: "open", Source: "10.0.0.2:50000", Destination: "127.0.0.1:9090"})
	report := summarizeTCPTrace(events, captureSummary{}, time.Second, "", "")
	// 송수신만 보인 연결은 trace 전부터 열려 있던 것이라 attempts가 아니라 existing으로 센다.
	if report.Attempts != tcpTraceConnectionLimit+5 || report.Established != tcpTraceConnectionLimit+5 || report.Incomplete != 0 || report.Existing != 1 {
		t.Fatalf("totals = %d attempts, %d established, %d incomplete, %d existing", report.Attempts, report.Established, report.Incomplete, report.Existing)
	}
	// 행은 진행 중인 연결과 최근에 끝난 연결만 남는다. 처음 끝난 다섯 연결이 빠진다.
	if len(report.Connections) != tcpTraceConnectionLimit+1 || report.ConnectionsOmitted != 5 {
		t.Fatalf("rows = %d, omitted = %d", len(report.Connections), report.ConnectionsOmitted)
	}
	for _, connection := range report.Connections {
		if connection.Source == "10.0.0.2:30000" || connection.Source == "10.0.0.2:30004" {
			t.Fatalf("an early connection is still listed: %#v", connection)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"connections_omitted":5`) {
		t.Fatalf("report JSON misses connections_omitted: %.200s", encoded)
	}
}

func TestSummarizeTCPTraceClassifiesResults(t *testing.T) {
	setTraceKernelEvents(t, true)
	events := []captureEvent{
		// 연결된 뒤 RST로 끝나도 established다.
		{SocketID: 1, TimestampNS: 1, Event: "tcp_state", Process: "probe", Source: "10.0.0.2:0", Destination: "127.0.0.1:8080", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 1, TimestampNS: 2, Event: "tcp_connect", Source: "10.0.0.2:40001", Destination: "127.0.0.1:8080", OldState: "SYN_SENT", NewState: "ESTABLISHED"},
		{SocketID: 1, TimestampNS: 3, Event: "tcp_receive_reset", Source: "10.0.0.2:40001", Destination: "127.0.0.1:8080", OldState: "ESTABLISHED"},
		{SocketID: 1, TimestampNS: 4, Event: "tcp_close", Source: "10.0.0.2:40001", Destination: "127.0.0.1:8080", OldState: "ESTABLISHED", NewState: "CLOSE"},
		// 닫힌 port로 connect하면 RST를 받고 닫힌다.
		{SocketID: 2, TimestampNS: 5, Event: "tcp_state", Process: "refused", Source: "10.0.0.2:0", Destination: "127.0.0.1:1", OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 2, TimestampNS: 6, Event: "tcp_receive_reset", Source: "10.0.0.2:40002", Destination: "127.0.0.1:1", OldState: "SYN_SENT"},
		{SocketID: 2, TimestampNS: 7, Event: "tcp_close", Source: "10.0.0.2:40002", Destination: "127.0.0.1:1", OldState: "SYN_SENT", NewState: "CLOSE"},
		// macOS는 거부된 connect를 처음 볼 때 SYN_SENT에서 닫힌 것으로만 보고한다.
		{SocketID: 3, TimestampNS: 8, Event: "tcp_close", Process: "mac", Source: "10.0.0.2:40003", Destination: "192.0.2.9:443", OldState: "SYN_SENT", NewState: "CLOSE"},
		// trace 전에 보낸 SYN을 재전송하고 있고, trace가 끝날 때까지 응답이 없다.
		{SocketID: 4, TimestampNS: 9, Event: "tcp_retransmit", Source: "10.0.0.2:40004", Destination: "192.0.2.1:443", OldState: "SYN_SENT"},
		// trace 전부터 열려 있던 연결이다.
		{SocketID: 5, TimestampNS: 10, Event: "tcp_send", Bytes: 50, Process: "sshd", Source: "10.0.0.2:22", Destination: "10.0.0.9:50000"},
		// listen socket은 행이 아니다. 자식 socket의 LISTEN→SYN_RECV는 서버의 새 연결이다.
		{SocketID: 6, TimestampNS: 11, Event: "tcp_state", Process: "server", Source: "10.0.0.2:8080", OldState: "CLOSE", NewState: "LISTEN"},
		{SocketID: 7, TimestampNS: 12, Event: "tcp_state", Source: "10.0.0.2:8080", Destination: "10.0.0.9:50001", OldState: "LISTEN", NewState: "SYN_RECV"},
		{SocketID: 7, TimestampNS: 13, Event: "tcp_accept", Process: "server", Source: "10.0.0.2:8080", Destination: "10.0.0.9:50001", OldState: "SYN_RECV", NewState: "ESTABLISHED"},
		{SocketID: 6, TimestampNS: 14, Event: "tcp_close", Source: "10.0.0.2:8080", OldState: "LISTEN", NewState: "CLOSE"},
		// 경로가 없는 IPv6 connect는 SYN_SENT 전에 실패하고, kernel이 port를 되돌린 destroy만 남는다.
		{SocketID: 8, TimestampNS: 15, Event: "tcp_destroy", Source: "[::]:0", Destination: "[2001:db8::1]:0"},
		// connect하지 않고 닫은 socket은 연결이 아니다.
		{SocketID: 9, TimestampNS: 16, Event: "tcp_destroy", Source: "0.0.0.0:0", Destination: "0.0.0.0:0"},
		// 주소 없이 RST만 보낸 socket은 판단할 근거가 없어 existing으로 둔다.
		{SocketID: 10, TimestampNS: 17, Event: "tcp_send_reset", Process: "quiet"},
	}
	report := summarizeTCPTrace(events, captureSummary{}, time.Second, "", "")
	results := map[uint64]string{}
	byDestination := map[string]tcpTraceConnection{}
	for _, connection := range report.Connections {
		byDestination[connection.Destination] = connection
	}
	for socket, destination := range map[uint64]string{1: "127.0.0.1:8080", 2: "127.0.0.1:1", 3: "192.0.2.9:443", 4: "192.0.2.1:443", 5: "10.0.0.9:50000", 7: "10.0.0.9:50001", 8: "[2001:db8::1]:0", 10: ""} {
		results[socket] = byDestination[destination].Result
	}
	want := map[uint64]string{1: "established", 2: "failed", 3: "failed", 4: "incomplete", 5: "existing", 7: "established", 8: "failed", 10: "existing"}
	if !reflect.DeepEqual(results, want) || len(report.Connections) != 8 {
		t.Fatalf("results = %v, want %v; rows = %d", results, want, len(report.Connections))
	}
	if _, ok := byDestination["0.0.0.0:0"]; ok {
		t.Fatalf("an unconnected socket has a row: %#v", byDestination["0.0.0.0:0"])
	}
	if report.Attempts != 6 || report.Established != 2 || report.Incomplete != 4 || report.Existing != 2 || report.Attempts != report.Established+report.Incomplete {
		t.Fatalf("totals = %d attempts, %d established, %d incomplete, %d existing", report.Attempts, report.Established, report.Incomplete, report.Existing)
	}
	if reset := byDestination["127.0.0.1:8080"]; traceOptional(reset.Reset, "%t") != "true" || reset.ConnectMS == nil {
		t.Fatalf("established and reset connection = %#v", reset)
	}
	// 연결되지 않은 행에는 연결 시간이 없다.
	for _, destination := range []string{"127.0.0.1:1", "192.0.2.1:443", "10.0.0.9:50000"} {
		if byDestination[destination].ConnectMS != nil {
			t.Fatalf("%s has a connect time: %#v", destination, byDestination[destination])
		}
	}
}

func TestSummarizeUDPTraceSplitsOneSocketByDestination(t *testing.T) {
	events := []captureEvent{
		{SocketID: 1, Protocol: "udp", Event: "udp_send", Bytes: 10, Destination: "127.0.0.1:19999"},
		{SocketID: 1, Protocol: "udp", Event: "udp_send", Bytes: 20, Destination: "127.0.0.1:19998"},
		{SocketID: 1, Protocol: "udp", Event: "udp_receive", Bytes: 30, Destination: "127.0.0.1:19999"},
	}
	report := summarizeUDPTrace(events, captureSummary{}, time.Second, "", "")
	if len(report.Flows) != 2 {
		t.Fatalf("flows = %#v, want one flow for each destination", report.Flows)
	}
	for _, flow := range report.Flows {
		if flow.Destination == "127.0.0.1:19999" && (flow.Sent != 1 || flow.Received != 1 || flow.TotalBytes != 40) {
			t.Fatalf("19999 flow = %#v", flow)
		}
		if flow.Destination == "127.0.0.1:19998" && (flow.Sent != 1 || flow.Received != 0 || flow.TotalBytes != 20) {
			t.Fatalf("19998 flow = %#v", flow)
		}
	}
}

func TestSummarizeUDPTrace(t *testing.T) {
	events := []captureEvent{
		{SocketID: 1, Protocol: "udp", Event: "udp_send", Bytes: 120, Process: "agent", PID: 7, Source: "10.0.0.2:53000", Destination: "203.0.113.53:53", Target: "dns.example"},
		{SocketID: 1, Protocol: "udp", Event: "udp_receive", Bytes: 80, Process: "agent", Destination: "203.0.113.53:53"},
		{SocketID: 2, Protocol: "tcp", Event: "tcp_connect", Process: "agent", Destination: "203.0.113.10:443"},
	}
	report := summarizeUDPTrace(events, captureSummary{LostEvents: 2}, 3*time.Second, "", "")
	if report.Datagrams != 2 || report.Sent != 1 || report.Received != 1 || report.LostEvents != 2 || len(report.Flows) != 1 {
		t.Fatalf("UDP report = %#v", report)
	}
	if got := traceUDPFlowDestinationLabel(report.Flows[0]); got != "203.0.113.53:53 (dns.example)" {
		t.Fatalf("UDP destination = %q", got)
	}
	if report.TXBytes != 120 || report.RXBytes != 80 || report.TotalBytes != 200 || report.BytesPerSecond != 200.0/3 || report.BitsPerSecond != 1600.0/3 || report.MegabitsPerSecond != 0.0016/3 {
		t.Fatalf("UDP traffic = %#v", report.traceTraffic)
	}
	if flow := report.Flows[0]; flow.TXBytes != 120 || flow.RXBytes != 80 || flow.TotalBytes != 200 {
		t.Fatalf("UDP flow traffic = %#v", flow.traceTraffic)
	}
}

func TestSummarizeUDPTraceCountsCounterPackets(t *testing.T) {
	events := []captureEvent{
		{SocketID: 1, Protocol: "udp", Event: "udp_send", Bytes: 640, Packets: 5, Destination: "203.0.113.53:53"},
		{SocketID: 1, Protocol: "udp", Event: "udp_receive", Bytes: 256, Packets: 2, Destination: "203.0.113.53:53"},
	}
	report := summarizeUDPTrace(events, captureSummary{}, time.Second, "", "")
	if report.Datagrams != 7 || report.Sent != 5 || report.Received != 2 || report.Flows[0].Sent != 5 || report.Flows[0].Received != 2 {
		t.Fatalf("UDP report = %#v", report)
	}
	groups := summarizeTraceGroups("udp", traceGroupByTarget, events, captureSummary{}, time.Second, "", "")
	if groups.Groups[0].Tx != 5 || groups.Groups[0].Rx != 2 {
		t.Fatalf("UDP groups = %#v", groups.Groups)
	}
}

// counter로 만든 trace에는 RST, 연결 지연, 재전송 횟수가 없다. 0으로 쓰면 관측한 0과 구분되지 않는다.
func TestTraceMarksUnobservedValues(t *testing.T) {
	setTraceKernelEvents(t, false)
	t.Setenv("NO_COLOR", "1")
	events := []captureEvent{
		{SocketID: 1, Protocol: "tcp", Event: "tcp_connect", Process: "curl", Destination: "203.0.113.10:443"},
		{SocketID: 1, Protocol: "tcp", Event: "tcp_retransmit", Bytes: 1448, Process: "curl", Destination: "203.0.113.10:443"},
		{SocketID: 1, Protocol: "tcp", Event: "tcp_send", Bytes: 1200, Packets: 2, Process: "curl", Destination: "203.0.113.10:443"},
	}
	report := summarizeTCPTrace(events, captureSummary{}, time.Second, "", "")
	connection := report.Connections[0]
	if report.Retransmissions != nil || report.Resets != nil || connection.ConnectMS != nil || connection.Retransmissions != nil || connection.Reset != nil || connection.Result != "established" {
		t.Fatalf("report = %#v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"retransmissions\":null", "\"resets\":null", "\"connect_ms\":null", "\"reset\":null", "\"tx_bytes\":1200"} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("report JSON missing %s: %s", field, encoded)
		}
	}
	previousOutput := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	printTCPTraceReport(report)
	os.Stdout = previousOutput
	write.Close()
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Retransmissions: -\n", "Resets: -\n", "\testablished\t-\t1.2KiB\t0B\t-\t-\n"} {
		if !strings.Contains(string(output), line) {
			t.Fatalf("report output missing %q:\n%s", line, output)
		}
	}
	group := summarizeTraceGroups("tcp", traceGroupByTarget, events, captureSummary{}, time.Second, "", "").Groups[0]
	if group.Retransmissions != nil || group.Resets != nil || group.Connect != 1 {
		t.Fatalf("group = %#v", group)
	}
	layout := traceGroupLayout{labelWidth: 14, byteWidth: 6, bytes: traceBytes}
	if row := formatTraceGroupScreenRow("tcp", traceGroupByTarget, group, 0, layout); !strings.Contains(row, "   1   -   - tcp_send") {
		t.Fatalf("screen row = %q", row)
	}
}

func TestTraceSourceGroupIgnoresEphemeralPort(t *testing.T) {
	report := summarizeTraceGroups("tcp", traceGroupBySource, []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_connect", Source: "10.0.0.2:41001", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_connect", Source: "[2001:db8::2]:41002", Destination: "[2001:db8::10]:443"},
		{Protocol: "tcp", Event: "tcp_connect", Source: "[2001:db8::2]:41003", Destination: "[2001:db8::10]:443"},
	}, captureSummary{}, time.Second, "", "")
	if len(report.Groups) != 2 || report.Groups[0].Group != "10.0.0.2" || report.Groups[0].Connect != 2 || report.Groups[1].Group != "2001:db8::2" || report.Groups[1].Connect != 2 {
		t.Fatalf("source groups = %#v", report.Groups)
	}
}

func TestTraceScreenRateUsesRetainedWindow(t *testing.T) {
	model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	for index := 0; index <= traceScreenEventLimit; index++ {
		updated, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "tcp", Event: "tcp_connect", Target: "example.com"}}})
		model = updated.(traceScreenModel)
	}
	if !model.truncated || len(model.events) != traceScreenEventLimit || len(model.arrivals) != traceScreenEventLimit {
		t.Fatalf("window = truncated %t, %d events, %d arrivals", model.truncated, len(model.events), len(model.arrivals))
	}

	now := time.Now()
	model.started = now.Add(-time.Hour)
	model.arrivals[0] = now.Add(-10 * time.Second)
	if got := model.windowDuration(now); got != 10*time.Second {
		t.Fatalf("window duration = %s, want 10s", got)
	}
	model.truncated = false
	if got := model.windowDuration(now); got != time.Hour {
		t.Fatalf("untruncated duration = %s, want 1h", got)
	}
	model.duration = 30 * time.Minute
	if got := model.windowDuration(now); got != 30*time.Minute {
		t.Fatalf("capped duration = %s, want 30m", got)
	}
}

func TestTraceScreenShowsBusiestGroupsFirst(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 120, 20
	for index := 0; index < 40; index++ {
		model.events = append(model.events, captureEvent{Protocol: "tcp", Event: "tcp_send", Bytes: 100, Target: fmt.Sprintf("h%02d.example", index)})
	}
	// 이름순으로 맨 앞인 group에 traffic이 가장 많다. 예전에는 뒤쪽만 남겨서 이 group이 보이지 않았다.
	model.events = append(model.events, captureEvent{Protocol: "tcp", Event: "tcp_send", Bytes: 5000, Target: "h00.example"})

	rows := traceScreenRows(model)
	if len(rows) != model.height-3 {
		t.Fatalf("rows = %d, want %d", len(rows), model.height-3)
	}
	if !strings.HasPrefix(rows[0], "h00.example ") {
		t.Fatalf("first row = %q", rows[0])
	}
}

func TestTraceTrafficZeroDurationHasZeroRates(t *testing.T) {
	report := summarizeTraceGroups("udp", traceGroupBySource, []captureEvent{
		{Protocol: "udp", Event: "udp_send", Bytes: 500, Source: "10.0.0.2:53"},
		{Protocol: "udp", Event: "udp_receive", Bytes: 250, Source: "10.0.0.2:53"},
	}, captureSummary{}, 0, "", "")
	if report.TotalBytes != 750 || report.BytesPerSecond != 0 || report.BitsPerSecond != 0 || report.MegabitsPerSecond != 0 {
		t.Fatalf("zero-duration report = %#v", report.traceTraffic)
	}
	if len(report.Groups) != 1 || report.Groups[0].TotalBytes != 750 || report.Groups[0].BytesPerSecond != 0 || report.Groups[0].BitsPerSecond != 0 || report.Groups[0].MegabitsPerSecond != 0 {
		t.Fatalf("zero-duration group = %#v", report.Groups)
	}
}

func TestTraceTrafficJSONFields(t *testing.T) {
	report := summarizeTraceGroups("tcp", traceGroupByTarget, []captureEvent{
		{Event: "tcp_send", Bytes: 1000, Target: "example.com"},
		{Event: "tcp_receive", Bytes: 500, Target: "example.com"},
	}, captureSummary{}, time.Second, "", "")
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"tx_bytes\":1000", "\"rx_bytes\":500", "\"total_bytes\":1500", "\"bytes_per_second\":1500", "\"bits_per_second\":12000", "\"megabits_per_second\":0.012"} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("report JSON missing %s: %s", field, encoded)
		}
	}
}

func TestTraceEventMatchesText(t *testing.T) {
	event := captureEvent{Process: "curl", Target: "naver.com", Destination: "203.0.113.10:80", Event: "tcp_connect"}
	for _, filter := range []string{"curl", "NAVER.COM", "203.0.113.10:80", "connect", ""} {
		if !traceEventMatchesText(event, filter) {
			t.Fatalf("filter %q did not match", filter)
		}
	}
	if traceEventMatchesText(event, "udp") {
		t.Fatal("unrelated filter matched")
	}
}

func TestSummarizeTraceGroupsByDimension(t *testing.T) {
	setTraceKernelEvents(t, true)
	base := uint64(time.Second)
	events := []captureEvent{
		{Protocol: "tcp", TimestampNS: base, Event: "tcp_connect", Process: "curl", Source: "10.0.0.2:41000", Target: "naver.com", Destination: "223.130.200.219:80"},
		{Protocol: "tcp", TimestampNS: base + uint64(time.Millisecond), Event: "tcp_retransmit", Process: "curl", Source: "10.0.0.2:41000", Target: "naver.com", Destination: "223.130.200.219:80"},
		{Protocol: "tcp", TimestampNS: base + 2*uint64(time.Millisecond), Event: "tcp_receive_reset", Process: "curl", Source: "10.0.0.2:41000", Target: "naver.com", Destination: "223.130.200.219:80"},
		{Protocol: "tcp", TimestampNS: base + 3*uint64(time.Millisecond), Event: "tcp_connect", Process: "agent", Source: "10.0.0.3:41001", Target: "example.com", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", TimestampNS: base + 4*uint64(time.Millisecond), Event: "tcp_connect", Process: "unknown", Target: "missing.example", Destination: "203.0.113.30:443"},
		{Protocol: "udp", TimestampNS: base + 5*uint64(time.Millisecond), Event: "udp_send", Process: "resolver", Source: "10.0.0.2:53000", Target: "dns.example", Destination: "203.0.113.53:53"},
		{Protocol: "udp", TimestampNS: base + 6*uint64(time.Millisecond), Event: "udp_receive", Process: "resolver", Source: "10.0.0.2:53000", Target: "dns.example", Destination: "203.0.113.53:53"},
		{Protocol: "udp", TimestampNS: base + 7*uint64(time.Millisecond), Event: "udp_send", Process: "resolver", Source: "10.0.0.3:53001", Target: "dns.example", Destination: "203.0.113.53:53"},
	}
	report := summarizeTraceGroups("tcp", traceGroupByTarget, events, captureSummary{LostEvents: 4}, time.Second, "", "")
	if report.Events != 5 || len(report.Groups) != 3 || report.LostEvents != 4 {
		t.Fatalf("TCP groups = %#v", report)
	}
	naver := report.Groups[2]
	if naver.Group != "naver.com" || naver.Events != 3 || naver.Connect != 1 || traceOptional(naver.Retransmissions, "%d") != "1" || traceOptional(naver.Resets, "%d") != "1" || naver.Rate != 3 {
		t.Fatalf("naver group = %#v", naver)
	}
	if len(naver.Destinations) != 1 || len(naver.Processes) != 1 {
		t.Fatalf("naver dimensions = %#v", naver)
	}
	source := summarizeTraceGroups("tcp", traceGroupBySource, events, captureSummary{}, time.Second, "", "")
	if source.GroupBy != traceGroupBySource || len(source.Groups) != 3 || source.Groups[0].Group != "-" || source.Groups[1].Group != "10.0.0.2" || source.Groups[1].Connect != 1 || traceOptional(source.Groups[1].Retransmissions, "%d") != "1" || traceOptional(source.Groups[1].Resets, "%d") != "1" {
		t.Fatalf("TCP source groups = %#v", source)
	}
	udp := summarizeTraceGroups("udp", traceGroupByTarget, events, captureSummary{}, 2*time.Second, "", "")
	if udp.Events != 3 || len(udp.Groups) != 1 || udp.Groups[0].Tx != 2 || udp.Groups[0].Rx != 1 {
		t.Fatalf("UDP groups = %#v", udp)
	}
	udpSource := summarizeTraceGroups("udp", traceGroupBySource, events, captureSummary{}, time.Second, "", "")
	if len(udpSource.Groups) != 2 || udpSource.Groups[0].Group != "10.0.0.2" || udpSource.Groups[0].Tx != 1 || udpSource.Groups[0].Rx != 1 || udpSource.Groups[1].Group != "10.0.0.3" || udpSource.Groups[1].Tx != 1 || udpSource.Groups[1].Rx != 0 {
		t.Fatalf("UDP source groups = %#v", udpSource)
	}
}

func TestTraceScreenGroupKeys(t *testing.T) {
	for _, initial := range []string{"", traceGroupBySource, traceGroupByTarget, traceGroupByPort, traceGroupByProcess, traceGroupByEvent} {
		model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: initial}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		if model.groupBy != initial {
			t.Fatalf("initial group mode = %q, want %q", model.groupBy, initial)
		}
		for _, transition := range []struct{ key, want string }{{"s", traceGroupBySource}, {"s", traceGroupBySource}, {"t", traceGroupByTarget}, {"t", traceGroupByTarget}, {"p", traceGroupByPort}, {"p", traceGroupByPort}, {"c", traceGroupByProcess}, {"c", traceGroupByProcess}, {"e", traceGroupByEvent}, {"e", traceGroupByEvent}, {"g", ""}, {"g", ""}} {
			next, _ := model.Update(tea.KeyPressMsg{Code: rune(transition.key[0]), Text: transition.key})
			model = next.(traceScreenModel)
			if model.groupBy != transition.want {
				t.Fatalf("%q from %q = %q, want %q", transition.key, initial, model.groupBy, transition.want)
			}
		}
	}
	model := newTraceScreenModel("udp", tcpTraceOptions{groupBy: traceGroupBySource}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	if header := strings.Join(traceScreenHeader(model), "\n"); !strings.Contains(header, "SOURCE") || !strings.Contains(header, "grouped by source") {
		t.Fatalf("source header = %q", header)
	}
}

func TestSummarizeTraceGroupsByEvent(t *testing.T) {
	setTraceKernelEvents(t, true)
	report := summarizeTraceGroups("tcp", traceGroupByEvent, []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_connect", Destination: "203.0.113.20:443"},
		{Protocol: "tcp", Event: "tcp_retransmit", Destination: "203.0.113.10:443"},
		{Protocol: "tcp"},
	}, captureSummary{}, time.Second, "", "")
	if len(report.Groups) != 3 || report.Groups[0].Group != "-" || report.Groups[1].Group != "tcp_connect" || report.Groups[1].Connect != 2 || report.Groups[2].Group != "tcp_retransmit" || traceOptional(report.Groups[2].Retransmissions, "%d") != "1" {
		t.Fatalf("event groups = %#v", report.Groups)
	}
	if got := traceGroupDisplayValue(traceGroupByEvent, report.Groups[1]); got != "tcp_connect" {
		t.Fatalf("event display value = %q, want only the event name", got)
	}
}

func TestTraceGroupDisplayValueAddsDestinationOnlyForTargets(t *testing.T) {
	group := traceGroupSummary{Group: "10.0.0.2", Destinations: []string{"203.0.113.10:443", "203.0.113.20:443"}}
	if got := traceGroupDisplayValue(traceGroupBySource, group); got != "10.0.0.2" {
		t.Fatalf("source display value = %q, want only the source host", got)
	}
	target := traceGroupSummary{Group: "example.com", Destinations: []string{"203.0.113.10:443"}}
	if got := traceGroupDisplayValue(traceGroupByTarget, target); got != "example.com (203.0.113.10:443)" {
		t.Fatalf("target display value = %q", got)
	}
}

func TestTraceScreenWidensGroupColumn(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	target := "very-long-service-name.internal.example.com"
	for _, protocol := range []string{"tcp", "udp"} {
		model := newTraceScreenModel(protocol, tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.events = []captureEvent{{Protocol: protocol, Event: protocol + "_send", Bytes: 100, Target: target}}

		model.width, model.height = 200, 10
		row := traceScreenRows(model)[0]
		if !strings.HasPrefix(row, target+" ") {
			t.Fatalf("%s wide row = %q, want the full target", protocol, row)
		}
		columns := traceScreenHeader(model)[2]
		if strings.Index(columns, "EVT")+len("EVT") != strings.Index(row, "    1 ")+len("    1") {
			t.Fatalf("%s header and row are not aligned:\n%q\n%q", protocol, columns, row)
		}
		if width := liveWidth(row); width > model.width {
			t.Fatalf("%s wide row is %d columns, want at most %d", protocol, width, model.width)
		}

		model.width = 80
		for _, line := range append(traceScreenHeader(model), traceScreenRows(model)...) {
			if liveWidth(strings.TrimRight(line, "\n")) > model.width {
				t.Fatalf("%s narrow line = %q", protocol, line)
			}
		}
		if row := traceScreenRows(model)[0]; strings.Contains(row, "\n") {
			t.Fatalf("%s narrow row wraps: %q", protocol, row)
		}
		if row := traceScreenRows(model)[0]; !strings.HasPrefix(row, target[:traceGroupMinLabelWidth-1]) || strings.Contains(row, target) {
			t.Fatalf("%s narrow row = %q, want the old 14-column label", protocol, row)
		}
	}
}

func TestTraceScreenGroupColumnsStayAligned(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	field := regexp.MustCompile(`\S+`)
	for _, protocol := range []string{"tcp", "udp"} {
		for _, width := range []int{80, 200} {
			model := newTraceScreenModel(protocol, tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
			model.width, model.height = width, 10
			// 경과 시간을 1ms로 두면 Mbps가 1000을 넘는다. 예전에는 이 값이 5칸을 넘어 뒤 열을 밀었다.
			model.started = time.Now().Add(-time.Millisecond)
			// 열마다 길이가 다른 값을 넣는다. 예전에는 195.3KiB 같은 값이 6칸을 넘어 뒤 열을 밀었다.
			model.events = []captureEvent{
				{Protocol: protocol, Event: protocol + "_send", Bytes: 200000, Target: "example.com"},
				{Protocol: protocol, Event: protocol + "_receive", Bytes: 5, Target: "example.com"},
			}
			header := field.FindAllStringIndex(traceScreenHeader(model)[2], -1)
			row := field.FindAllStringIndex(traceScreenRows(model)[0], -1)
			columns := 8
			if protocol == "tcp" {
				columns = 11
			}
			if len(header) < columns || len(row) < columns {
				t.Fatalf("%s width %d: header %d fields, row %d fields", protocol, width, len(header), len(row))
			}
			// 첫 열은 왼쪽, 숫자 열은 오른쪽 정렬이다. LAST는 폭이 모자라면 잘리므로 보지 않는다.
			for index := 1; index < columns-1; index++ {
				if header[index][1] != row[index][1] {
					t.Fatalf("%s width %d column %d ends at %d in the header and %d in the row\n%q\n%q", protocol, width, index, header[index][1], row[index][1], traceScreenHeader(model)[2], traceScreenRows(model)[0])
				}
			}
		}
	}
}

func TestTraceBytesUsesGiBForLargeValues(t *testing.T) {
	for bytes, want := range map[uint64]string{0: "0B", 1023: "1023B", 1024: "1.0KiB", 1<<20 - 1: "1024.0KiB", 1 << 20: "1.0MiB", 1<<30 - 1: "1024.0MiB", 1 << 30: "1.0GiB", 10000 << 20: "9.8GiB", 9999 << 30: "9999.0GiB"} {
		got := traceBytes(bytes)
		if got != want || len(got) > traceGroupWideByteWidth {
			t.Fatalf("traceBytes(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestTraceCompactBytesFitsNarrowColumn(t *testing.T) {
	for bytes, want := range map[uint64]string{0: "0B", 999: "999B", 1000: "1.0K", 10188: "9.9K", 10189: "10K", 200000: "195K", 1023487: "999K", 1023488: "1.0M", 5 << 30: "5.0G"} {
		got := traceCompactBytes(bytes)
		if got != want || len(got) > traceGroupNarrowByteWidth {
			t.Fatalf("traceCompactBytes(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func TestTraceScreenRatesFitTheirColumns(t *testing.T) {
	for rate, want := range map[float64]string{0: "0.0", 9999.9: "9999.9", 9999.95: "10000", 999999.4: "999999", 999999.5: "1000k"} {
		if got := traceScreenEventRate(rate); got != want || len(got) > 6 {
			t.Fatalf("traceScreenEventRate(%v) = %q, want %q", rate, got, want)
		}
	}
	for megabits, want := range map[float64]string{0.37: "0.37", 99.99: "99.99", 99.995: "100.0", 999.9: "999.9", 999.95: "1000", 99999.4: "99999", 99999.5: "100G"} {
		if got := traceScreenMegabits(megabits); got != want || len(got) > 5 {
			t.Fatalf("traceScreenMegabits(%v) = %q, want %q", megabits, got, want)
		}
	}
}

func TestTraceServerService(t *testing.T) {
	for _, test := range []struct {
		name                string
		source, destination string
		want                string
	}{
		{"resolver reply", "127.0.0.53:53", "127.0.0.1:41022", "127.0.0.53:53"},
		{"ssh server", "100.83.200.248:22", "100.65.168.177:52406", "100.83.200.248:22"},
		{"ipv6 server", "[::1]:53", "[::1]:41000", "[::1]:53"},
		{"resolver client", "127.0.0.1:41022", "127.0.0.53:53", ""},
		{"both ephemeral", "20.20.0.50:41641", "61.74.181.17:35585", ""},
		{"both service ports", "20.20.0.50:123", "203.0.113.1:123", ""},
		{"no peer port", "127.0.0.1:18080", "0.0.0.0:0", ""},
		{"no address", "", "", ""},
	} {
		got, ok := traceServerService(captureEvent{Source: test.source, Destination: test.destination}, 32768, 60999)
		if got != test.want || ok != (test.want != "") {
			t.Fatalf("%s: traceServerService = %q, %t, want %q", test.name, got, ok, test.want)
		}
	}
}

func TestSummarizeTraceGroupsCollectsServerReplies(t *testing.T) {
	setTraceEphemeralPortRange(t, 32768, 60999)
	events := []captureEvent{
		{Protocol: "udp", Event: "udp_send", Bytes: 100, Source: "127.0.0.53:53", Destination: "127.0.0.1:41022"},
		{Protocol: "udp", Event: "udp_send", Bytes: 100, Source: "127.0.0.53:53", Destination: "127.0.0.1:41023"},
		{Protocol: "udp", Event: "udp_receive", Bytes: 40, Source: "127.0.0.53:53", Destination: "127.0.0.1:41024"},
		{Protocol: "udp", Event: "udp_send", Bytes: 40, Source: "127.0.0.1:41025", Destination: "127.0.0.53:53"},
		{Protocol: "udp", Event: "udp_send", Bytes: 40, Source: "127.0.0.1:41026", Destination: "127.0.0.53:53", Target: "n1.example.com"},
		{Protocol: "udp", Event: "udp_send", Bytes: 70, Source: "127.0.0.53:53", Destination: "127.0.0.1:41027", Target: "named.example.com"},
	}
	report := summarizeTraceGroups("udp", traceGroupByTarget, events, captureSummary{}, time.Second, "", "")
	byLabel := map[string]traceGroupSummary{}
	for _, group := range report.Groups {
		byLabel[traceGroupDisplayValue(traceGroupByTarget, group)] = group
	}
	server, ok := byLabel["127.0.0.53:53 (server)"]
	if !ok || !server.Server || server.Events != 3 || server.TotalBytes != 240 {
		t.Fatalf("server group = %#v, groups = %v", server, byLabel)
	}
	// target 없는 client 행은 같은 주소여도 서버 행과 섞이지 않는다.
	if client, ok := byLabel["127.0.0.53:53"]; !ok || client.Server || client.Events != 1 {
		t.Fatalf("client group = %#v, groups = %v", client, byLabel)
	}
	// target이 있으면 서버 쪽이어도 target으로 묶는다.
	if _, ok := byLabel["named.example.com (127.0.0.1:41027)"]; !ok {
		t.Fatalf("named group is missing: %v", byLabel)
	}
	if len(report.Groups) != 4 {
		t.Fatalf("groups = %d, want 4: %v", len(report.Groups), byLabel)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"server":true`) != 1 {
		t.Fatalf("JSON must mark only the server group: %s", encoded)
	}
}

func TestTraceGroupFitLabelKeepsServerMark(t *testing.T) {
	server := traceGroupSummary{Group: "127.0.0.53:53", Server: true, Destinations: []string{"127.0.0.1:41022"}}
	client := traceGroupSummary{Group: "127.0.0.1:19999", Destinations: []string{"127.0.0.1:19999"}}
	if got := traceGroupFitLabel(traceGroupByTarget, server, 40); got != "127.0.0.53:53 (server)" {
		t.Fatalf("wide server label = %q", got)
	}
	// 14칸에서는 주소에 5칸만 남는다. 서비스를 가리키는 port를 남겨야 서버 행끼리 구분된다.
	for group, want := range map[string]string{"127.0.0.53:53": "…:53 (server)", "127.0.0.1:19999": "…9999 (server)"} {
		server.Group = group
		got := traceGroupFitLabel(traceGroupByTarget, server, traceGroupMinLabelWidth)
		if got != want || liveWidth(got) > traceGroupMinLabelWidth {
			t.Fatalf("narrow server label for %s = %q, want %q", group, got, want)
		}
	}
	if got := traceGroupFitLabel(traceGroupByTarget, client, 10); strings.Contains(got, "server") || liveWidth(got) > 10 {
		t.Fatalf("narrow client label = %q", got)
	}
}

func TestTraceScreenTabCyclesGroupViews(t *testing.T) {
	model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	press := func(key tea.KeyPressMsg) {
		next, _ := model.Update(key)
		model = next.(traceScreenModel)
	}
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	for _, want := range []string{traceGroupBySource, traceGroupByTarget, traceGroupByPort, traceGroupByProcess, traceGroupByEvent, ""} {
		press(tab)
		if model.groupBy != want {
			t.Fatalf("tab = %q, want %q", model.groupBy, want)
		}
	}
	back := tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	for _, want := range []string{traceGroupByEvent, traceGroupByProcess, traceGroupByPort, traceGroupByTarget, traceGroupBySource, ""} {
		press(back)
		if model.groupBy != want {
			t.Fatalf("shift+tab = %q, want %q", model.groupBy, want)
		}
	}
	// 단축키로 옮긴 뒤에도 Tab은 그 자리에서 이어서 간다.
	press(tea.KeyPressMsg{Code: 't', Text: "t"})
	press(tab)
	if model.groupBy != traceGroupByPort {
		t.Fatalf("tab after t = %q, want %q", model.groupBy, traceGroupByPort)
	}
	// 필터를 입력하는 동안 Tab은 보기를 바꾸지 않는다.
	press(tea.KeyPressMsg{Code: '/', Text: "/"})
	press(tab)
	if model.groupBy != traceGroupByPort || !model.filtering {
		t.Fatalf("tab while filtering = %q, filtering %t", model.groupBy, model.filtering)
	}
}

func TestTraceScrollRowsShowTheTarget(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	event := captureEvent{Protocol: "tcp", Event: "tcp_connect", Process: "curl", Destination: "104.18.11.61:443", Source: "20.20.0.50:41022", Target: "jinwoo.rgrg.im"}
	for _, width := range []int{80, 150, 220} {
		model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height = width, 6
		model.events = []captureEvent{event}
		header := traceScreenHeader(model)[2]
		row := traceScreenRows(model)[0]
		if strings.Contains(row, "\n") || liveWidth(row) > width {
			t.Fatalf("width %d row = %q", width, row)
		}
		// 목적지 칸이 넓어져도 헤더의 EVENT와 SOURCE는 행의 값과 같은 칸에서 시작한다. "…"가 여러 바이트라 칸으로 센다.
		column := func(line, value string) int { return liveWidth(line[:strings.Index(line, value)]) }
		if column(header, "EVENT") != column(row, "tcp_connect") || column(header, "SOURCE") != column(row, "20.20.0.") {
			t.Fatalf("width %d header and row are not aligned:\n%q\n%q", width, header, row)
		}
		if width >= 150 && !strings.Contains(row, "104.18.11.61:443 (jinwoo.rgrg.im)") {
			t.Fatalf("width %d row misses the target: %q", width, row)
		}
		if width == 80 && (!strings.Contains(row, "104.18.11.61:443 (jinwoo.rgrg") || !strings.Contains(row, "…")) {
			t.Fatalf("narrow row must cut the label in one line: %q", row)
		}
	}
}

func TestWaitTraceMessageBatchesQueuedEvents(t *testing.T) {
	eventCh := make(chan captureEvent, traceEventBatchLimit+10)
	resultCh := make(chan traceFinishedMsg, 1)
	for index := 0; index < traceEventBatchLimit+10; index++ {
		eventCh <- captureEvent{Protocol: "tcp", Event: "tcp_send"}
	}
	first, ok := waitTraceMessage(eventCh, resultCh)().(traceEventMsg)
	if !ok || len(first.events) != traceEventBatchLimit {
		t.Fatalf("first batch = %d events, want %d", len(first.events), traceEventBatchLimit)
	}
	second, ok := waitTraceMessage(eventCh, resultCh)().(traceEventMsg)
	if !ok || len(second.events) != 10 {
		t.Fatalf("second batch = %d events, want 10", len(second.events))
	}
	resultCh <- traceFinishedMsg{}
	if _, ok := waitTraceMessage(eventCh, resultCh)().(traceFinishedMsg); !ok {
		t.Fatal("an empty queue must return the finished result")
	}

	model := newTraceScreenModel("tcp", tcpTraceOptions{}, eventCh, resultCh, nil)
	next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "tcp"}, {Protocol: "udp"}, {Protocol: "tcp"}}})
	model = next.(traceScreenModel)
	if model.received != 3 || len(model.events) != 2 || len(model.arrivals) != 2 {
		t.Fatalf("batch update: received %d, kept %d events and %d arrivals", model.received, len(model.events), len(model.arrivals))
	}
}

func TestTraceScreenRowsShowTheNewestMatchingEvents(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 120, 8
	for index := 0; index < 50; index++ {
		process := "curl"
		if index%2 == 1 {
			process = "wget"
		}
		model.events = append(model.events, captureEvent{Protocol: "tcp", Event: "tcp_send", Process: process, Destination: fmt.Sprintf("10.0.0.%d:443", index)})
	}
	check := func(want []string) {
		t.Helper()
		rows := traceScreenRows(model)
		if len(rows) != model.height-3 {
			t.Fatalf("rows = %d, want %d", len(rows), model.height-3)
		}
		for index, destination := range want {
			if !strings.Contains(rows[index], destination) {
				t.Fatalf("row %d = %q, want %s", index, rows[index], destination)
			}
		}
	}
	// 가장 최근 event가 맨 아래에 오고, 그 위로 시간 순서를 지킨다.
	check([]string{"10.0.0.45:443", "10.0.0.46:443", "10.0.0.47:443", "10.0.0.48:443", "10.0.0.49:443"})
	model.filter = "wget"
	check([]string{"10.0.0.41:443", "10.0.0.43:443", "10.0.0.45:443", "10.0.0.47:443", "10.0.0.49:443"})
	model.filter = "nothing-matches"
	for _, row := range traceScreenRows(model) {
		if row != "" {
			t.Fatalf("row without a match = %q", row)
		}
	}
}

// BPF_CORE_READ_INTO는 읽을 크기를 sizeof(*dst)로 정한다. 배열 필드를 이름으로 넘기면 첫 원소 1바이트만 읽어,
// TCP 송수신 event의 IPv6 주소가 첫 바이트만 남았다. 대상은 항상 주소로 넘긴다.
func TestBPFCoreReadIntoTakesAnAddress(t *testing.T) {
	source, err := os.ReadFile("capture_events_bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`BPF_CORE_READ_INTO\(\s*([^,]+),`).FindAllStringSubmatch(string(source), -1)
	if len(calls) == 0 {
		t.Fatal("no BPF_CORE_READ_INTO call found")
	}
	for _, call := range calls {
		if !strings.HasPrefix(strings.TrimSpace(call[1]), "&") {
			t.Errorf("BPF_CORE_READ_INTO(%s, ...) must take the address of its target", call[1])
		}
	}
}

func TestBPFLengthEventKeepsClosedSocketPort(t *testing.T) {
	source, err := os.ReadFile("capture_events_bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`if \(!event->sport\) \{\s*event->sport = bpf_ntohs\(BPF_CORE_READ\(\(struct inet_sock \*\)sk, inet_sport\)\);`).Match(source) {
		t.Fatal("TCP send and receive events must read inet_sport when skc_num is cleared on close")
	}
}

func TestSummarizeTraceGroupsByPort(t *testing.T) {
	setTraceEphemeralPortRange(t, 32768, 60999)
	events := []captureEvent{
		{Protocol: "udp", Event: "udp_send", Bytes: 20, Source: "20.20.0.50:41641", Destination: "102.67.165.185:3478"},
		{Protocol: "udp", Event: "udp_receive", Bytes: 16, Source: "20.20.0.50:41641", Destination: "102.67.165.185:3478"},
		{Protocol: "udp", Event: "udp_send", Bytes: 20, Source: "20.20.0.50:41641", Destination: "157.180.28.32:3478", Target: "derp.example.com"},
		{Protocol: "udp", Event: "udp_send", Bytes: 1088, Source: "20.20.0.50:41641", Destination: "61.74.181.17:35585"},
		{Protocol: "udp", Event: "udp_send", Bytes: 43, Source: "20.20.0.50:43360", Destination: "20.20.1.1:53"},
		{Protocol: "udp", Event: "udp_send", Bytes: 32, Source: "127.0.0.53:53", Destination: "127.0.0.1:47732"},
		{Protocol: "udp", Event: "udp_send", Bytes: 48, Source: "127.0.0.53:53", Destination: "127.0.0.1:50414"},
		{Protocol: "udp", Event: "udp_send", Bytes: 10, Source: "127.0.0.1:18080", Destination: "0.0.0.0:0"},
		{Protocol: "udp", Event: "udp_send", Bytes: 30, Source: "20.20.0.50:40000", Destination: "203.0.113.5:443"},
	}
	report := summarizeTraceGroups("udp", traceGroupByPort, events, captureSummary{}, time.Second, "", "")
	labels := []string{}
	for _, group := range report.Groups {
		labels = append(labels, traceGroupDisplayValue(traceGroupByPort, group))
	}
	// target이 있어도 port로 묶고, 포트는 숫자 순서로 놓는다. 서버 행은 같은 포트의 client 행 뒤에 온다.
	want := []string{"-", "53 (20.20.1.1:53)", "53 (server)", "443 (203.0.113.5:443)", "3478 (2 peers)", "35585 (61.74.181.17:35585)"}
	if !slices.Equal(labels, want) {
		t.Fatalf("port groups = %q, want %q", labels, want)
	}
	if stun := report.Groups[4]; stun.Events != 3 || stun.TotalBytes != 56 {
		t.Fatalf("3478 group = %#v", stun)
	}
	if server := report.Groups[2]; !server.Server || server.Events != 2 {
		t.Fatalf("53 server group = %#v", server)
	}
}

func TestBPFSocketOwnerClearsUnknownOwner(t *testing.T) {
	source, err := os.ReadFile("capture_events_bpf.c")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`if \(!owner\) \{\s*event->pid = 0;\s*event->cgroup_id = 0;\s*__builtin_memset\(event->comm, 0, sizeof\(event->comm\)\);\s*return;`).Match(source) {
		t.Fatal("apply_sock_owner must clear pid, cgroup, and comm when the socket owner is unknown")
	}
}

func TestSummarizeTraceGroupsByProcess(t *testing.T) {
	setTraceKernelEvents(t, true)
	events := []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Process: "curl", PID: 100, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_retransmit", Process: "curl", PID: 101, Source: "10.0.0.2:41001", Destination: "203.0.113.20:80"},
		{Protocol: "tcp", Event: "tcp_connect", Process: "agent", PID: 200, Source: "10.0.0.2:41002", Destination: "203.0.113.30:443"},
		{Protocol: "tcp", Event: "tcp_receive_reset", Source: "10.0.0.2:41003", Destination: "203.0.113.40:443"},
	}
	report := summarizeTraceGroups("tcp", traceGroupByProcess, events, captureSummary{}, time.Second, "", "")
	labels := []string{}
	for _, group := range report.Groups {
		labels = append(labels, traceGroupDisplayValue(traceGroupByProcess, group))
	}
	// PID가 달라도 이름이 같으면 한 행이다. --process 필터와 같은 기준이다.
	if want := []string{"-", "agent", "curl"}; !slices.Equal(labels, want) {
		t.Fatalf("process groups = %q, want %q", labels, want)
	}
	if curl := report.Groups[2]; curl.Events != 2 || curl.Connect != 1 || traceOptional(curl.Retransmissions, "%d") != "1" || len(curl.Destinations) != 2 {
		t.Fatalf("curl group = %#v", curl)
	}
}

func TestTraceAggregateMatchesSummaries(t *testing.T) {
	events := []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", SocketID: 1, TimestampNS: 1_000_000, Process: "curl", PID: 10, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443", Target: "example.com"},
		{Protocol: "tcp", Event: "tcp_send", SocketID: 1, TimestampNS: 3_000_000, Process: "curl", PID: 10, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443", Bytes: 100},
		{Protocol: "tcp", Event: "tcp_retransmit", SocketID: 1, TimestampNS: 4_000_000, Source: "10.0.0.2:41000", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_accept", SocketID: 2, TimestampNS: 5_000_000, Process: "sshd", PID: 20, Source: "10.0.0.2:22", Destination: "10.0.0.9:50000"},
		{Protocol: "tcp", Event: "tcp_receive", SocketID: 2, TimestampNS: 6_000_000, Process: "sshd", PID: 20, Source: "10.0.0.2:22", Destination: "10.0.0.9:50000", Bytes: 40},
		{Protocol: "tcp", Event: "tcp_send_reset", SocketID: 3, TimestampNS: 7_000_000},
		{Protocol: "udp", Event: "udp_send", SocketID: 4, TimestampNS: 8_000_000, Process: "dig", PID: 30, Source: "10.0.0.2:53000", Destination: "8.8.8.8:53", Bytes: 30},
		{Protocol: "udp", Event: "udp_receive", SocketID: 4, TimestampNS: 9_000_000, Process: "dig", PID: 30, Source: "10.0.0.2:53000", Destination: "8.8.8.8:53", Bytes: 60},
	}
	for _, protocol := range []string{"tcp", "udp"} {
		aggregate := newTraceAggregate(protocol, traceGroupCycle...)
		for _, event := range events {
			if traceProtocol(event) == protocol {
				aggregate.observe(event)
			}
		}
		summary := captureSummary{LostEvents: 2}
		for _, view := range traceGroupCycle {
			var got, want any
			switch {
			case view != "":
				got, want = aggregate.groups[view].report(summary, time.Second), summarizeTraceGroups(protocol, view, events, summary, time.Second, "", "")
			case protocol == "udp":
				got, want = aggregate.udp.report(summary, time.Second), summarizeUDPTrace(events, summary, time.Second, "", "")
			default:
				got, want = aggregate.tcp.report(summary, time.Second), summarizeTCPTrace(events, summary, time.Second, "", "")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s view %q: aggregate = %#v, summary = %#v", protocol, view, got, want)
			}
		}
		// report는 쌓은 값을 바꾸지 않는다. 같은 요약을 다시 불러도 같은 값이어야 한다.
		if protocol == "tcp" && !reflect.DeepEqual(aggregate.tcp.report(summary, time.Second), aggregate.tcp.report(summary, time.Second)) {
			t.Fatal("tcp report changed on the second call")
		}
	}
}

func TestTraceGroupByModes(t *testing.T) {
	for groupBy, want := range map[string]bool{"": true, traceGroupBySource: true, traceGroupByTarget: true, traceGroupByPort: true, traceGroupByProcess: true, traceGroupByEvent: true, "invalid": false} {
		if got := validTraceGroupBy(groupBy); got != want {
			t.Fatalf("validTraceGroupBy(%q) = %t, want %t", groupBy, got, want)
		}
	}
}

func TestCaptureCommandIgnoresPATH(t *testing.T) {
	fake := t.TempDir()
	for _, name := range []string{"tcpdump", "sudo"} {
		if err := os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", fake)
	stubCaptureExecutables(t, "/usr/bin/tcpdump", "/usr/bin/sudo")

	captureGeteuid = func() int { return 1000 }
	path, args, err := captureCommand([]string{"-i", "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/usr/bin/sudo" || strings.Join(args, " ") != "/usr/bin/tcpdump -i eth0" {
		t.Fatalf("capture command = %q %q", path, args)
	}

	captureGeteuid = func() int { return 0 }
	path, args, err = captureCommand([]string{"-i", "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/usr/bin/tcpdump" || strings.Join(args, " ") != "-i eth0" {
		t.Fatalf("root capture command = %q %q", path, args)
	}
}

func TestCaptureCommandPrefersSbinTcpdump(t *testing.T) {
	stubCaptureExecutables(t, "/usr/sbin/tcpdump", "/usr/bin/tcpdump")
	captureGeteuid = func() int { return 0 }

	path, _, err := captureCommand(nil)
	if err != nil || path != "/usr/sbin/tcpdump" {
		t.Fatalf("capture command = %q, %v", path, err)
	}
}

func TestCaptureCommandReportsMissingBinaries(t *testing.T) {
	stubCaptureExecutables(t)
	captureGeteuid = func() int { return 1000 }
	if _, _, err := captureCommand(nil); err == nil || !strings.Contains(err.Error(), "/usr/sbin/tcpdump") {
		t.Fatalf("missing tcpdump error = %v", err)
	}

	stubCaptureExecutables(t, "/usr/sbin/tcpdump")
	if _, _, err := captureCommand(nil); err == nil || !strings.Contains(err.Error(), "/usr/bin/sudo") {
		t.Fatalf("missing sudo error = %v", err)
	}
}

func stubCaptureExecutables(t *testing.T, present ...string) {
	t.Helper()
	previous, previousEuid := captureExecutable, captureGeteuid
	t.Cleanup(func() { captureExecutable, captureGeteuid = previous, previousEuid })
	available := map[string]bool{}
	for _, path := range present {
		available[path] = true
	}
	captureExecutable = func(path string) bool { return available[path] }
}

func TestCapturePlanDetailListsEveryCondition(t *testing.T) {
	// wide 문자가 섞이는 언어까지 돌려야 열 정렬을 실제로 잰다.
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range supportedLanguages {
		setLanguage(language)
		t.Run(language, func(t *testing.T) { assertCapturePlanDetail(t) })
	}
}

func assertCapturePlanDetail(t *testing.T) {
	t.Helper()
	plan := capturePlan{interfaceName: "en0", duration: 15 * time.Second, count: 500, outputPath: "/tmp/incident.pcap"}
	detail := plan.detail()
	if !strings.HasPrefix(detail, T("cli.capture.plan_title")+"\n") || !strings.Contains(detail, capturePayloadWarning()) {
		t.Fatalf("detail = %q", detail)
	}
	// 한글 label이 섞여도 값 열이 같은 자리에서 시작해야 한다.
	rows := map[string]string{"interface": "en0", "duration": "15s", "packet limit": "500", "filter": "(none)", "output": "/tmp/incident.pcap", T("cli.capture.label.privilege"): T("cli.capture.privilege_sudo")}
	column := -1
	for _, line := range strings.Split(detail, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		label, value := "", ""
		for candidate, expected := range rows {
			if strings.HasPrefix(line, "  "+candidate) {
				label, value = candidate, expected
				break
			}
		}
		if label == "" {
			t.Fatalf("unexpected row %q", line)
		}
		if !strings.HasSuffix(line, value) {
			t.Fatalf("row %q does not end with %q", line, value)
		}
		start := liveWidth(strings.TrimSuffix(line, value))
		if column >= 0 && start != column {
			t.Fatalf("row %q starts its value at column %d, want %d", line, start, column)
		}
		column = start
		delete(rows, label)
	}
	if len(rows) != 0 {
		t.Fatalf("missing rows: %#v", rows)
	}
	filtered := capturePlan{interfaceName: "en0", filter: "host 203.0.113.10", privileged: true}
	if !strings.Contains(filtered.detail(), "host 203.0.113.10") {
		t.Fatalf("filter row = %q", filtered.detail())
	}
	if !strings.Contains(filtered.detail(), T("cli.capture.privilege_root")) {
		t.Fatalf("privilege row = %q", filtered.detail())
	}
}

// 확인 화면은 답을 고른 뒤에도 계획을 남겨야 tcpdump 출력 위에 조건이 보인다.
func TestCaptureConfirmKeepsPlanAfterAnswer(t *testing.T) {
	plan := capturePlan{interfaceName: "en0", duration: time.Second, count: 10, outputPath: "/tmp/a.pcap"}
	model := newDetailedConfirmModel(plan.detail(), T("cli.capture.confirm"), false)
	if !strings.Contains(model.View().Content, "en0") {
		t.Fatalf("view = %q", model.View().Content)
	}
	answered, _ := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	answeredModel := answered.(confirmModel)
	final := answeredModel.View().Content
	if !strings.Contains(final, "interface") || !strings.Contains(final, "en0") || !strings.Contains(final, T("cli.capture.confirm")+" "+confirmYesLabel()+"\n") {
		t.Fatalf("final view = %q", final)
	}
	// 확인 전후 화면 높이가 같아야 이전 줄이 남지 않는다.
	if liveLineCount(final) != liveLineCount(model.View().Content) {
		t.Fatalf("frame height changed: %d → %d", liveLineCount(model.View().Content), liveLineCount(final))
	}
}

func TestCaptureConfirmTextFallback(t *testing.T) {
	// terminal이 아니면 계획을 그대로 출력하고 y/N을 읽는다.
	if !strings.Contains(capturePlan{interfaceName: "en0"}.detail(), capturePayloadWarning()) {
		t.Fatal("plain fallback must keep the payload warning")
	}
}

func TestCaptureSelectItemsNamesTheDefaultRoute(t *testing.T) {
	interfaces := []interfaceDetails{
		{Name: "bridge100", Address: "192.168.139.3"},
		{Name: "en0", Address: "192.168.1.92", Gateway: "192.168.1.1"},
		// 주소가 둘인 interface는 목록에 두 번 나온다.
		{Name: "en0", Address: "10.0.0.5"},
	}
	items := captureSelectItems(interfaces, "en0")
	if len(items) != 2 {
		t.Fatalf("주소가 여럿인 interface는 한 줄이어야 한다: %#v", items)
	}
	if items[0].value != "bridge100" || items[1].value != "en0" {
		t.Fatalf("고르는 값은 interface 이름이어야 한다: %#v", items)
	}
	if !strings.Contains(items[1].label, "192.168.1.92") {
		t.Fatalf("이름만으로는 어느 것인지 알 수 없다: %q", items[1].label)
	}
	if !strings.Contains(items[1].label, T("cli.capture.default_route")) {
		t.Fatalf("기본 경로 interface에 표시가 없다: %q", items[1].label)
	}
	if strings.Contains(items[0].label, T("cli.capture.default_route")) {
		t.Fatalf("기본 경로가 아닌 interface에 표시가 붙었다: %q", items[0].label)
	}
	// 이름 열 폭을 맞춰야 주소를 세로로 훑을 수 있다.
	first, second := strings.Index(items[0].label, "192.168.139.3"), strings.Index(items[1].label, "192.168.1.92")
	if first != second {
		t.Fatalf("주소가 같은 열에서 시작하지 않는다: %q / %q", items[0].label, items[1].label)
	}
}
