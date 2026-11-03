package edc

import (
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/bpf"
)

const expectTestPort = 2142

func expectTestIPv4(source, destination string, sourcePort, destinationPort uint16, flags uint8, seq uint32, payload int) []byte {
	data := make([]byte, 40+payload)
	data[0] = 0x45
	binary.BigEndian.PutUint16(data[2:4], uint16(len(data)))
	data[8] = 64
	data[9] = 6
	copy(data[12:16], netip.MustParseAddr(source).AsSlice())
	copy(data[16:20], netip.MustParseAddr(destination).AsSlice())
	expectTestTCP(data[20:], sourcePort, destinationPort, flags, seq)
	return data
}

func expectTestIPv6(source, destination string, sourcePort, destinationPort uint16, flags uint8, seq uint32, payload int) []byte {
	data := make([]byte, 60+payload)
	data[0] = 0x60
	binary.BigEndian.PutUint16(data[4:6], uint16(20+payload))
	data[6] = 6
	data[7] = 64
	copy(data[8:24], netip.MustParseAddr(source).AsSlice())
	copy(data[24:40], netip.MustParseAddr(destination).AsSlice())
	expectTestTCP(data[40:], sourcePort, destinationPort, flags, seq)
	return data
}

func expectTestTCP(tcp []byte, sourcePort, destinationPort uint16, flags uint8, seq uint32) {
	binary.BigEndian.PutUint16(tcp[0:2], sourcePort)
	binary.BigEndian.PutUint16(tcp[2:4], destinationPort)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = 5 << 4
	tcp[13] = flags
}

func TestExpectFilterKeepsHandshakePacketsOnly(t *testing.T) {
	program, err := expectFilter(expectTestPort)
	if err != nil {
		t.Fatal(err)
	}
	instructions := make([]bpf.Instruction, len(program))
	for index, raw := range program {
		instructions[index] = raw.Disassemble()
	}
	vm, err := bpf.NewVM(instructions)
	if err != nil {
		t.Fatal(err)
	}
	udp := expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagSYN, 1, 0)
	udp[9] = 17
	fragment := expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagSYN, 1, 0)
	binary.BigEndian.PutUint16(fragment[6:8], 10)
	cases := []struct {
		name   string
		packet []byte
		keep   bool
	}{
		{"ipv4 SYN in", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagSYN, 1, 0), true},
		{"ipv4 SYN-ACK out", expectTestIPv4("10.0.0.2", "10.0.0.1", expectTestPort, 40000, tcpFlagSYN|tcpFlagACK, 1, 0), true},
		{"ipv4 RST out", expectTestIPv4("10.0.0.2", "10.0.0.1", expectTestPort, 40000, tcpFlagRST|tcpFlagACK, 0, 0), true},
		{"ipv4 pure ACK in", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagACK, 2, 0), true},
		{"ipv4 data in", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagACK, 2, 200), false},
		{"ipv4 ACK out", expectTestIPv4("10.0.0.2", "10.0.0.1", expectTestPort, 40000, tcpFlagACK, 2, 0), false},
		{"ipv4 other port", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, 22, tcpFlagSYN, 1, 0), false},
		{"ipv4 udp", udp, false},
		{"ipv4 fragment", fragment, false},
		{"ipv6 SYN in", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, expectTestPort, tcpFlagSYN, 1, 0), true},
		{"ipv6 SYN-ACK out", expectTestIPv6("2001:db8::2", "2001:db8::1", expectTestPort, 40000, tcpFlagSYN|tcpFlagACK, 1, 0), true},
		{"ipv6 pure ACK in", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, expectTestPort, tcpFlagACK, 2, 0), true},
		{"ipv6 data in", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, expectTestPort, tcpFlagACK, 2, 200), false},
		{"ipv6 other port", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, 443, tcpFlagSYN, 1, 0), false},
	}
	for _, test := range cases {
		kept, err := vm.Run(test.packet)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if (kept > 0) != test.keep {
			t.Errorf("%s: kept %d bytes, want keep=%v", test.name, kept, test.keep)
		}
		if kept > expectSnapLength {
			t.Errorf("%s: kept %d bytes, more than the header limit", test.name, kept)
		}
	}
}

func TestParseExpectPacketDirections(t *testing.T) {
	inbound, ok := parseExpectPacket(expectTestIPv4("100.64.0.9", "100.83.200.248", 53122, expectTestPort, tcpFlagSYN, 77, 0), expectTestPort, false)
	if !ok || inbound.remote.String() != "100.64.0.9:53122" || inbound.local.String() != "100.83.200.248:2142" || inbound.seq != 77 || inbound.flags != tcpFlagSYN {
		t.Fatalf("inbound = %+v, %v", inbound, ok)
	}
	outbound, ok := parseExpectPacket(expectTestIPv4("100.83.200.248", "100.64.0.9", expectTestPort, 53122, tcpFlagRST|tcpFlagACK, 0, 0), expectTestPort, true)
	if !ok || outbound.remote.String() != "100.64.0.9:53122" || outbound.local.String() != "100.83.200.248:2142" {
		t.Fatalf("outbound = %+v, %v", outbound, ok)
	}
	// 이 host가 밖으로 연 연결의 임시 port가 같아도 세지 않는다.
	if _, ok := parseExpectPacket(expectTestIPv4("10.0.0.2", "10.0.0.9", 50000, expectTestPort, tcpFlagSYN, 1, 0), expectTestPort, true); ok {
		t.Fatal("an outgoing SYN to the port is not a reply from this host")
	}
	v6, ok := parseExpectPacket(expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, expectTestPort, tcpFlagSYN, 5, 0), expectTestPort, false)
	if !ok || v6.remote.String() != "[2001:db8::1]:40000" {
		t.Fatalf("ipv6 = %+v, %v", v6, ok)
	}
	for name, data := range map[string][]byte{
		"empty":   {},
		"short":   expectTestIPv4("10.0.0.1", "10.0.0.2", 1, expectTestPort, tcpFlagSYN, 1, 0)[:30],
		"version": {0x50, 0, 0, 0},
	} {
		if _, ok := parseExpectPacket(data, expectTestPort, false); ok {
			t.Errorf("%s packet parsed", name)
		}
	}
}

func TestParseExpectFromAndPort(t *testing.T) {
	for input, want := range map[string]string{"10.85.1.2": "10.85.1.2/32", "10.85.0.0/16": "10.85.0.0/16", "10.85.3.4/16": "10.85.0.0/16", "2001:db8::1": "2001:db8::1/128", "::ffff:10.0.0.1": "10.0.0.1/32"} {
		prefix, err := parseExpectFrom(input)
		if err != nil || prefix.String() != want {
			t.Errorf("parseExpectFrom(%q) = %v, %v; want %s", input, prefix, err, want)
		}
	}
	for _, input := range []string{"host.example", "10.0.0.0/40", ""} {
		if _, err := parseExpectFrom(input); err == nil {
			t.Errorf("parseExpectFrom(%q) accepted", input)
		}
	}
	for _, input := range []string{"0", "65536", "http", "-1"} {
		if _, err := parseExpectPort(input); err == nil {
			t.Errorf("parseExpectPort(%q) accepted", input)
		}
	}
	if port, err := parseExpectPort("2142"); err != nil || port != expectTestPort {
		t.Fatalf("parseExpectPort = %d, %v", port, err)
	}
}

type expectScenario struct {
	session *expectSession
	events  []expectEvent
	now     time.Time
}

func newExpectScenario(from string) *expectScenario {
	scenario := &expectScenario{now: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)}
	var prefix netip.Prefix
	if from != "" {
		prefix, _ = parseExpectFrom(from)
	}
	scenario.session = newExpectSession(expectTestPort, prefix, func(event expectEvent) { scenario.events = append(scenario.events, event) })
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "tailscale0", Source: local.String(), RPFilter: &expectRPFilter{}}
	}
	return scenario
}

func (scenario *expectScenario) in(iface, remote string, flags uint8, seq uint32, after time.Duration) {
	scenario.now = scenario.now.Add(after)
	packet, ok := parseExpectPacket(expectTestIPv4(remote, "100.83.200.248", 53122, expectTestPort, flags, seq, 0), expectTestPort, false)
	if !ok {
		panic("bad test packet")
	}
	packet.at, packet.iface = scenario.now, iface
	scenario.session.packet(packet)
}

func (scenario *expectScenario) out(iface, remote string, flags uint8, seq uint32, after time.Duration) {
	scenario.now = scenario.now.Add(after)
	packet, ok := parseExpectPacket(expectTestIPv4("100.83.200.248", remote, expectTestPort, 53122, flags, seq, 0), expectTestPort, true)
	if !ok {
		panic("bad test packet")
	}
	packet.at, packet.iface = scenario.now, iface
	scenario.session.packet(packet)
}

func (scenario *expectScenario) kinds() string {
	kinds := make([]string, 0, len(scenario.events))
	for _, event := range scenario.events {
		kinds = append(kinds, event.kind)
	}
	return strings.Join(kinds, ",")
}

func (scenario *expectScenario) finish(stopReason string) (Result, []expectSource, int) {
	result, code := scenario.session.result(scenario.now.Add(-time.Minute), stopReason)
	sources, _ := result.Metrics["sources"].([]expectSource)
	return result, sources, code
}

func TestExpectClosedPortSendsRST(t *testing.T) {
	scenario := newExpectScenario("")
	for attempt := uint32(0); attempt < 3; attempt++ {
		scenario.in("tailscale0", "100.64.0.9", tcpFlagSYN, 1000+attempt, time.Second)
		scenario.out("tailscale0", "100.64.0.9", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
	}
	if got := scenario.kinds(); got != "syn,rst_out,syn,rst_out,syn,rst_out" {
		t.Fatalf("events = %s", got)
	}
	result, sources, code := scenario.finish("duration")
	if code != 1 || result.Status != StatusFail || len(sources) != 1 {
		t.Fatalf("code=%d status=%s sources=%+v", code, result.Status, sources)
	}
	source := sources[0]
	if source.Verdict != expectVerdictRST || source.Attempts != 3 || source.SYNIn != 3 || source.RSTOut != 3 || source.Interface != "tailscale0" {
		t.Fatalf("source = %+v", source)
	}
	if result.Metrics["rst_out"] != 3 || result.Metrics["verdict"] != expectVerdictRST || result.Next != "edc listen" {
		t.Fatalf("metrics = %+v next=%q", result.Metrics, result.Next)
	}
	if !strings.Contains(result.Summary, "100.64.0.9") || !strings.Contains(result.Summary, "RST 3") {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExpectHandshakeCompletes(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("tailscale0", "100.64.0.9", tcpFlagSYN, 500, 0)
	scenario.out("tailscale0", "100.64.0.9", tcpFlagSYN|tcpFlagACK, 9000, time.Millisecond)
	// 번호가 다른 ACK는 handshake의 마지막 ACK가 아니다.
	scenario.in("tailscale0", "100.64.0.9", tcpFlagACK, 999, 5*time.Millisecond)
	scenario.in("tailscale0", "100.64.0.9", tcpFlagACK, 501, 5*time.Millisecond)
	scenario.in("tailscale0", "100.64.0.9", tcpFlagACK, 501, 5*time.Millisecond)
	if got := scenario.kinds(); got != "syn,synack,established" {
		t.Fatalf("events = %s", got)
	}
	result, sources, code := scenario.finish("cancelled")
	if code != 0 || result.Status != StatusPass || sources[0].Handshakes != 1 || sources[0].Verdict != expectVerdictCompleted {
		t.Fatalf("code=%d status=%s sources=%+v", code, result.Status, sources)
	}
}

func TestExpectHandshakeSeenWithoutVisibleSYNACK(t *testing.T) {
	// IPsec policy로 SYN-ACK가 ESP로 나가면 SYN-ACK는 보이지 않지만 client의 ACK는 보인다.
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 41, 0)
	scenario.in("eth0", "10.85.1.2", tcpFlagACK, 42, 30*time.Millisecond)
	_, sources, code := scenario.finish("duration")
	if code != 0 || sources[0].Verdict != expectVerdictCompleted {
		t.Fatalf("code=%d sources=%+v", code, sources)
	}
}

func TestExpectSYNACKRetransmitsWithoutACK(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.out("eth0", "10.85.1.2", tcpFlagSYN|tcpFlagACK, 100, time.Millisecond)
	scenario.out("eth0", "10.85.1.2", tcpFlagSYN|tcpFlagACK, 100, time.Second)
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 10*time.Millisecond)
	scenario.out("eth0", "10.85.1.2", tcpFlagSYN|tcpFlagACK, 100, 2*time.Second)
	if got := scenario.kinds(); got != "syn,synack,synack_retransmit,syn_retransmit,synack_retransmit" {
		t.Fatalf("events = %s", got)
	}
	result, sources, code := scenario.finish("duration")
	source := sources[0]
	if code != 1 || source.Verdict != expectVerdictNoACK || source.SYNACKOut != 3 || source.SYNACKRetransmits != 2 || source.SYNRetransmits != 1 || source.Attempts != 1 {
		t.Fatalf("code=%d source=%+v", code, source)
	}
	if !strings.Contains(result.Summary, "SYN-ACK") {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExpectSamePacketOnTwoInterfacesIsNotARetransmit(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.in("br0", "10.85.1.2", tcpFlagSYN, 7, time.Millisecond)
	scenario.out("br0", "10.85.1.2", tcpFlagSYN|tcpFlagACK, 100, time.Millisecond)
	scenario.out("eth0", "10.85.1.2", tcpFlagSYN|tcpFlagACK, 100, time.Millisecond)
	if got := scenario.kinds(); got != "syn,synack" {
		t.Fatalf("events = %s", got)
	}
	_, sources, _ := scenario.finish("duration")
	// IP 계층이 받는 interface(bridge)가 도착 interface다.
	if sources[0].Interface != "br0" || sources[0].SYNRetransmits != 0 || sources[0].SYNACKRetransmits != 0 {
		t.Fatalf("source = %+v", sources[0])
	}
}

func TestExpectNewISNStartsANewAttempt(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 8000, 3*time.Second)
	_, sources, _ := scenario.finish("duration")
	if sources[0].Attempts != 2 || sources[0].SYNRetransmits != 0 || sources[0].Verdict != expectVerdictNoReply {
		t.Fatalf("source = %+v", sources[0])
	}
}

func TestExpectFromAndLocalFilters(t *testing.T) {
	scenario := newExpectScenario("10.85.0.0/16")
	scenario.session.isLocal = func(address netip.Addr) bool { return address.String() == "100.83.200.248" }
	scenario.in("eth0", "10.16.1.1", tcpFlagSYN, 7, 0)
	scenario.in("eth0", "10.85.9.9", tcpFlagSYN, 7, 0)
	packet, _ := parseExpectPacket(expectTestIPv4("10.85.9.9", "192.0.2.80", 53122, expectTestPort, tcpFlagSYN, 9, 0), expectTestPort, false)
	packet.iface, packet.at = "eth0", scenario.now
	scenario.session.packet(packet)
	result, sources, _ := scenario.finish("duration")
	if len(sources) != 1 || sources[0].Address != "10.85.9.9" || result.Metrics["forwarded_packets"] != 1 || result.Metrics["from"] != "10.85.0.0/16" {
		t.Fatalf("sources=%+v metrics=%+v", sources, result.Metrics)
	}
}

func TestExpectNoSYNExitCodes(t *testing.T) {
	for stopReason, want := range map[string]int{"duration": 2, "cancelled": 4} {
		scenario := newExpectScenario("10.85.1.2")
		scenario.session.start(expectDropTrace{Status: StatusSkip, Reason: "no BTF"}, scenario.now)
		result, sources, code := scenario.finish(stopReason)
		if code != want || len(sources) != 0 || result.Status != StatusFail || result.Metrics["observation_status"] != "no_syn" {
			t.Fatalf("%s: code=%d status=%s metrics=%+v", stopReason, code, result.Status, result.Metrics)
		}
		if result.Metrics["from_return"] == nil || scenario.kinds() != "route" {
			t.Fatalf("%s: the single --from address needs its return route: %+v events=%s", stopReason, result.Metrics, scenario.kinds())
		}
		if !strings.Contains(result.Evidence[len(result.Evidence)-1].Value, "no BTF") {
			t.Fatalf("%s: evidence = %+v", stopReason, result.Evidence)
		}
	}
}

func TestExpectDropBeforeSYNAttachesToAttempt(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:2142", "NO_SOCKET", "tcp_v4_rcv")
	scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:22", "NO_SOCKET", "tcp_v4_rcv")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, time.Millisecond)
	scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:2142", "NO_SOCKET", "tcp_v4_rcv")
	if got := scenario.kinds(); got != "syn,drop,drop" {
		t.Fatalf("events = %s", got)
	}
	result, sources, _ := scenario.finish("duration")
	if sources[0].DropReasons["NO_SOCKET"] != 2 || !strings.Contains(result.Summary, "NO_SOCKET 2") {
		t.Fatalf("source=%+v summary=%q", sources[0], result.Summary)
	}
}

func TestExpectIPsecPolicyHidesReply(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "eth0", IPsecPolicy: true}
	}
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	result, sources, code := scenario.finish("duration")
	if code != 1 || result.Status != StatusWarn || sources[0].Verdict != expectVerdictIPsecHidden {
		t.Fatalf("code=%d status=%s source=%+v", code, result.Status, sources[0])
	}
}

func TestExpectStrictRPFilterOnAnotherInterfaceWarns(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "eth1", Via: "10.0.0.1", RPFilter: &expectRPFilter{Interface: 1, All: 0, Effective: 1}}
	}
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.session.tick(scenario.now.Add(time.Second))
	if got := scenario.kinds(); got != "syn,route" {
		t.Fatalf("events = %s", got)
	}
	result, sources, _ := scenario.finish("duration")
	if matches := sources[0].Return.MatchesArrival; matches == nil || *matches {
		t.Fatalf("return = %+v", sources[0].Return)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "rp_filter") || !strings.Contains(result.Warnings[0], "eth1") {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

func TestExpectVerdictOrderPicksTheProblem(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.in("eth0", "10.85.1.2", tcpFlagACK, 8, time.Millisecond)
	scenario.in("eth0", "10.85.1.3", tcpFlagSYN, 7, 0)
	scenario.out("eth0", "10.85.1.3", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
	result, _, code := scenario.finish("duration")
	if code != 1 || result.Metrics["verdict"] != expectVerdictRST || !strings.Contains(result.Summary, "10.85.1.3") {
		t.Fatalf("code=%d verdict=%v summary=%q", code, result.Metrics["verdict"], result.Summary)
	}
}

func TestExpectReportRedactsAddresses(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("tailscale0", "100.64.0.9", tcpFlagSYN, 7, 0)
	scenario.out("tailscale0", "100.64.0.9", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
	result, _, _ := scenario.finish("duration")
	report := buildReport("test", scenario.now, map[string]interface{}{"input": "2142"}, []Result{result}, true)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "100.64.0.9") || strings.Contains(string(data), "100.83.200.248") {
		t.Fatalf("report keeps an address: %s", data)
	}
	if !strings.Contains(string(data), `"syn_in":1`) || !strings.Contains(string(data), `"probe":"tcp.expect"`) {
		t.Fatalf("report = %s", data)
	}
}

func TestFormatExpectEventLine(t *testing.T) {
	event := expectEvent{at: time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local), kind: expectEventSYNACKRetrans, iface: "eth0", remote: netip.MustParseAddrPort("10.85.1.2:53122"), local: netip.MustParseAddrPort("10.16.100.110:2142"), count: 3}
	line := formatExpectEvent(event, textStyle{})
	for _, want := range []string{"10:00:00.000", "SYN-ACK out", "eth0", "10.85.1.2:53122 → 10.16.100.110:2142", "3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q lacks %q", line, want)
		}
	}
}

func TestExpectDropsWithoutVisibleSYN(t *testing.T) {
	// IPsec transport mode로 복호화한 SYN은 packet socket에 보이지 않지만 kernel drop은 보인다.
	scenario := newExpectScenario("")
	scenario.session.ipsecInbound = func(netip.Prefix) bool { return true }
	scenario.session.drop(scenario.now, "10.99.1.1:35558", "10.99.1.2:2142", "NO_SOCKET", "tcp_v4_rcv")
	scenario.session.drop(scenario.now, "10.99.1.1:35560", "10.99.1.2:2142", "NOT_SPECIFIED", "tcp_v4_rcv")
	if got := scenario.kinds(); got != "" {
		t.Fatalf("a drop without a SYN is not shown live: %s", got)
	}
	result, sources, code := scenario.finish("duration")
	if code != 1 || len(sources) != 1 || sources[0].Verdict != expectVerdictUnseen || sources[0].Interface != "-" || sources[0].DropReasons["NO_SOCKET"] != 1 {
		t.Fatalf("code=%d sources=%+v", code, sources)
	}
	if result.Metrics["other_drops"].(map[string]int)["NOT_SPECIFIED"] != 1 {
		t.Fatalf("other drops = %+v", result.Metrics["other_drops"])
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "xfrm") {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

func TestExpectDropsAfterTheConnectionDoNotCountAsArrival(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.drop(scenario.now, "10.99.1.1:35560", "10.99.1.2:2142", "NOT_SPECIFIED", "tcp_v4_rcv")
	_, sources, code := scenario.finish("duration")
	if code != 2 || len(sources) != 0 {
		t.Fatalf("code=%d sources=%+v", code, sources)
	}
}

func TestExpectNoSocketBehindIPsecIsRST(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "eth0", IPsecPolicy: true}
	}
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:2142", "NO_SOCKET", "tcp_v4_rcv")
	result, sources, code := scenario.finish("duration")
	if code != 1 || sources[0].Verdict != expectVerdictRST || !strings.Contains(result.Summary, "NO_SOCKET") || strings.Contains(result.Summary, "RST 0") {
		t.Fatalf("code=%d source=%+v summary=%q", code, sources[0], result.Summary)
	}
}

func TestExpectNoSYNBehindIPsecTransportIsNotAFailure(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.ipsecInbound = func(netip.Prefix) bool { return true }
	result, _, code := scenario.finish("duration")
	if code != 2 || result.Status != StatusWarn || !strings.Contains(result.Summary, "IPsec") || len(result.Warnings) != 1 {
		t.Fatalf("code=%d status=%s summary=%q warnings=%q", code, result.Status, result.Summary, result.Warnings)
	}
}
