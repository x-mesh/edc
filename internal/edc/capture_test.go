package edc

import (
	"encoding/json"
	"errors"
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

func TestSummarizeTCPTrace(t *testing.T) {
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
	if report.Attempts != 2 || report.Established != 1 || report.Incomplete != 1 || report.Retransmissions != 1 || report.Resets != 1 || report.LostEvents != 3 {
		t.Fatalf("report = %#v", report)
	}
	var established, reset *tcpTraceConnection
	for index := range report.Connections {
		switch report.Connections[index].Result {
		case "established":
			established = &report.Connections[index]
		case "reset":
			reset = &report.Connections[index]
		}
	}
	if established == nil || established.ConnectMS != 41 || established.Retransmissions != 1 {
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
	if reset == nil || !reset.Reset {
		t.Fatalf("reset connection = %#v", reset)
	}
	filtered := summarizeTCPTrace(events, captureSummary{}, time.Second, "bot", "")
	if filtered.Attempts != 1 || filtered.Connections[0].Process != "bot" {
		t.Fatalf("filtered report = %#v", filtered)
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
	if naver.Group != "naver.com" || naver.Events != 3 || naver.Connect != 1 || naver.Retransmissions != 1 || naver.Resets != 1 || naver.Rate != 3 {
		t.Fatalf("naver group = %#v", naver)
	}
	if len(naver.Destinations) != 1 || len(naver.Processes) != 1 {
		t.Fatalf("naver dimensions = %#v", naver)
	}
	source := summarizeTraceGroups("tcp", traceGroupBySource, events, captureSummary{}, time.Second, "", "")
	if source.GroupBy != traceGroupBySource || len(source.Groups) != 3 || source.Groups[0].Group != "-" || source.Groups[1].Group != "10.0.0.2:41000" || source.Groups[1].Connect != 1 || source.Groups[1].Retransmissions != 1 || source.Groups[1].Resets != 1 {
		t.Fatalf("TCP source groups = %#v", source)
	}
	udp := summarizeTraceGroups("udp", traceGroupByTarget, events, captureSummary{}, 2*time.Second, "", "")
	if udp.Events != 3 || len(udp.Groups) != 1 || udp.Groups[0].Tx != 2 || udp.Groups[0].Rx != 1 {
		t.Fatalf("UDP groups = %#v", udp)
	}
	udpSource := summarizeTraceGroups("udp", traceGroupBySource, events, captureSummary{}, time.Second, "", "")
	if len(udpSource.Groups) != 2 || udpSource.Groups[0].Group != "10.0.0.2:53000" || udpSource.Groups[0].Tx != 1 || udpSource.Groups[0].Rx != 1 || udpSource.Groups[1].Group != "10.0.0.3:53001" || udpSource.Groups[1].Tx != 1 || udpSource.Groups[1].Rx != 0 {
		t.Fatalf("UDP source groups = %#v", udpSource)
	}
}

func TestTraceScreenGroupKeys(t *testing.T) {
	for _, initial := range []string{"", traceGroupBySource, traceGroupByTarget} {
		model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: initial}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		if model.groupBy != initial {
			t.Fatalf("initial group mode = %q, want %q", model.groupBy, initial)
		}
		for _, transition := range []struct{ key, want string }{{"s", traceGroupBySource}, {"s", traceGroupBySource}, {"t", traceGroupByTarget}, {"t", traceGroupByTarget}, {"g", ""}, {"g", ""}} {
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

func TestTraceGroupByModes(t *testing.T) {
	for groupBy, want := range map[string]bool{"": true, traceGroupBySource: true, traceGroupByTarget: true, "invalid": false} {
		if got := validTraceGroupBy(groupBy); got != want {
			t.Fatalf("validTraceGroupBy(%q) = %t, want %t", groupBy, got, want)
		}
	}
}

func TestCaptureCommandUsesTcpdumpPath(t *testing.T) {
	previous := captureLookPath
	previousEuid := captureGeteuid
	defer func() {
		captureLookPath = previous
		captureGeteuid = previousEuid
	}()
	captureGeteuid = func() int { return 0 }
	captureLookPath = func(name string) (string, error) {
		if name == "tcpdump" {
			return "/usr/sbin/tcpdump", nil
		}
		return "", errors.New("not found")
	}

	path, args, err := captureCommand([]string{"-i", "eth0"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/usr/sbin/tcpdump" || strings.Join(args, " ") != "-i eth0" {
		t.Fatalf("capture command = %q %q", path, args)
	}
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
