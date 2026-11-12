package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
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
	if sources[0].DropReasons["NO_SOCKET"] != 2 || !strings.Contains(result.Summary, "NO_SOCKET 2") || !strings.Contains(result.Evidence[len(result.Evidence)-1].Value, "10.85.1.2: NO_SOCKET 2") {
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

func TestExpectRouteLookupsStopAtTheSourceLimit(t *testing.T) {
	scenario := newExpectScenario("")
	lookups := 0
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		lookups++
		return expectReturnPath{Device: "eth0"}
	}
	for index := 0; index < expectSourceLimit+30; index++ {
		scenario.in("eth0", netip.AddrFrom4([4]byte{10, 85, byte(index / 250), byte(index%250 + 1)}).String(), tcpFlagSYN, 7, 0)
	}
	scenario.session.tick(scenario.now.Add(time.Second))
	result, sources, _ := scenario.finish("duration")
	// 실행 중 조회와 끝에 남기는 줄의 조회가 각각 상한을 넘지 않는다.
	if lookups > 2*expectSourceLimit || len(sources) != expectSourceLimit || result.Metrics["sources_omitted"] != 30 || sources[expectSourceLimit-1].Return == nil {
		t.Fatalf("lookups=%d sources=%d omitted=%v", lookups, len(sources), result.Metrics["sources_omitted"])
	}
}

func expectTestNet(cidr string) *net.IPNet {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return network
}

func TestExpectPolicyPortSelectors(t *testing.T) {
	remote, local := netip.MustParseAddr("10.85.1.2"), netip.MustParseAddr("10.16.100.110")
	out := func(srcPort, dstPort, priority int) expectPolicy {
		return expectPolicy{src: expectTestNet("10.16.100.0/24"), dst: expectTestNet("10.85.0.0/16"), srcPort: srcPort, dstPort: dstPort, priority: priority, allow: true, templates: 1}
	}
	cases := []struct {
		name     string
		policies []expectPolicy
		covered  bool
	}{
		{"all ports", []expectPolicy{out(0, 0, 100)}, true},
		{"own port", []expectPolicy{out(expectTestPort, 0, 100)}, true},
		{"another service", []expectPolicy{out(22, 0, 100)}, false},
		{"pinned remote port", []expectPolicy{out(0, 443, 100)}, false},
		// 다른 port의 bypass policy가 우선순위로 앞서도 이 port의 policy를 가리지 않는다.
		{"port bypass does not mask", []expectPolicy{{src: expectTestNet("10.16.100.0/24"), dst: expectTestNet("10.85.0.0/16"), srcPort: 22, priority: 10}, out(0, 0, 100)}, true},
		{"udp only", []expectPolicy{{dst: expectTestNet("10.85.0.0/16"), proto: 17, allow: true, templates: 1}}, false},
		{"inbound only", []expectPolicy{{inbound: true, src: expectTestNet("10.85.0.0/16"), allow: true, templates: 1, transport: true}}, false},
	}
	for _, test := range cases {
		if got := expectPolicyCoversReply(test.policies, remote, local, expectTestPort); got != test.covered {
			t.Errorf("%s: covered=%v, want %v", test.name, got, test.covered)
		}
	}
	from, _ := parseExpectFrom("10.85.1.2")
	in := func(srcPort, dstPort int) expectPolicy {
		return expectPolicy{inbound: true, src: expectTestNet("10.85.0.0/16"), allow: true, templates: 1, transport: true, srcPort: srcPort, dstPort: dstPort}
	}
	inbound := []struct {
		name    string
		policy  expectPolicy
		covered bool
	}{
		{"all ports", in(0, 0), true},
		{"own port", in(0, expectTestPort), true},
		{"another service", in(0, 22), false},
		{"pinned remote port", in(53122, 0), false},
		{"tunnel mode", expectPolicy{inbound: true, src: expectTestNet("10.85.0.0/16"), allow: true, templates: 1}, false},
		{"udp only", expectPolicy{inbound: true, allow: true, templates: 1, transport: true, proto: 17}, false},
	}
	for _, test := range inbound {
		if got := expectPolicyTransportInbound([]expectPolicy{test.policy}, from, expectTestPort); got != test.covered {
			t.Errorf("inbound %s: covered=%v, want %v", test.name, got, test.covered)
		}
	}
}

func TestExpectDropTraceLostEventsAreReported(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "eth0"}
	}
	scenario.session.start(expectDropTrace{Status: StatusPass}, scenario.now)
	scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
	scenario.session.dropLost = 12
	result, _, _ := scenario.finish("duration")
	trace, ok := result.Metrics["drop_trace"].(expectDropTrace)
	if !ok || trace.Status != StatusPass || trace.LostEvents != 12 {
		t.Fatalf("drop_trace = %+v", result.Metrics["drop_trace"])
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "12") {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

func TestExpectTrackingStopsAtTheLimits(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnPath{Device: "eth0"}
	}
	scenario.session.flowLimit, scenario.session.sourceLimit = 3, 3
	syn := func(remote string, port uint16) {
		packet, _ := parseExpectPacket(expectTestIPv4(remote, "100.83.200.248", port, expectTestPort, tcpFlagSYN, 7, 0), expectTestPort, false)
		packet.iface, packet.at = "eth0", scenario.now
		scenario.session.packet(packet)
	}
	// 한 출발지에서 시도 5개: 3개만 따라간다.
	for port := uint16(40000); port < 40005; port++ {
		syn("10.85.1.2", port)
	}
	if len(scenario.session.flows) != 3 || scenario.session.untracked != 2 {
		t.Fatalf("flows=%d untracked=%d", len(scenario.session.flows), scenario.session.untracked)
	}
	// 따라가던 시도가 쉬면 접히고, 빈 자리는 다시 쓴다. 출발지는 3개까지만 합계를 남긴다.
	scenario.now = scenario.now.Add(expectFlowIdle + expectSweepInterval)
	scenario.session.tick(scenario.now)
	for _, remote := range []string{"10.85.1.3", "10.85.1.4", "10.85.1.5", "10.85.1.6"} {
		syn(remote, 40000)
	}
	if len(scenario.session.flows) != 3 || scenario.session.untracked != 3 {
		t.Fatalf("flows=%d untracked=%d", len(scenario.session.flows), scenario.session.untracked)
	}
	// SYN 없이 쌓인 drop도 출발지를 늘리지 못하고 연결 시도 밖의 drop으로 남는다.
	scenario.session.drop(scenario.now, "10.85.9.9:1", "100.83.200.248:2142", "NO_SOCKET", "tcp_v4_rcv")
	scenario.now = scenario.now.Add(expectSweepInterval)
	result, sources, _ := scenario.finish("duration")
	// 시도 3개와 출발지 3개를 남기고, 넘친 시도 2개와 출발지 자리가 없어 접지 못한 시도 1개는 센다.
	if len(sources) != 3 || result.Metrics["untracked_attempts"] != 4 || result.Metrics["attempts"] != 5 {
		t.Fatalf("sources=%d metrics=%+v", len(sources), result.Metrics)
	}
	if result.Metrics["other_drops"].(map[string]int)["NO_SOCKET"] != 1 {
		t.Fatalf("other drops = %+v", result.Metrics["other_drops"])
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "4") {
		t.Fatalf("warnings = %q", result.Warnings)
	}
}

// 시작 머리말은 무엇을 기다리는지와 client에서 실행할 명령을 알려야 한다. SYN이 오기 전에는 화면이 멈춘 것처럼 보인다.
func TestExpectHeaderTellsWhatToRunOnTheClient(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	var out bytes.Buffer
	printer := expectLivePrinter{writer: &out, style: newTextStyle(false)}
	listener := expectListener{State: expectListenerNone}
	printer.header(expectHeader{port: 8000, dropTrace: expectDropTrace{Status: StatusPass}, listener: &listener})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("header = %q", out.String())
	}
	if !strings.Contains(lines[1], "DROP line") {
		t.Errorf("drop line must say what it means: %q", lines[1])
	}
	if !strings.Contains(lines[2], "listener: none") || !strings.Contains(lines[2], "RST") || !strings.Contains(lines[2], "--listen") {
		t.Errorf("listener line = %q", lines[2])
	}
	if !strings.Contains(lines[3], "port 8000") || !strings.Contains(lines[3], "edc tcp check --diagnose <this-host>:8000") {
		t.Errorf("waiting line = %q", lines[3])
	}
}

// listener는 edc listen과 같은 목록에서 이 port의 TCP socket만 고른다. 모든 주소에 묶인 것이 loopback보다 앞이다.
func TestExpectListenerState(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	sockets := []listenSocket{
		{Proto: "udp", Address: "0.0.0.0", Port: 8000, Process: "dnsmasq", PID: "10"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 8000, Process: "helper", PID: "11"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 8000, Process: "nginx", PID: "812"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 8080, Process: "other", PID: "13"},
	}
	listener := expectListenerState(sockets, nil, 8000)
	if listener.State != expectListenerPresent || listener.Process != "nginx" || listener.PID != 812 || listener.Address != "0.0.0.0:8000" || listener.Scope != "all" {
		t.Fatalf("listener = %+v", listener)
	}
	if text := describeExpectListener(listener); text != "listener: nginx (pid 812) on 0.0.0.0:8000" {
		t.Errorf("line = %q", text)
	}
	loopback := expectListenerState(sockets[:2], nil, 8000)
	if loopback.Scope != "loopback" || !strings.Contains(describeExpectListener(loopback), "loopback only") {
		t.Errorf("loopback = %+v, %q", loopback, describeExpectListener(loopback))
	}
	v6 := expectListenerState([]listenSocket{{Proto: "tcp", Address: "::", Port: 8000}}, nil, 8000)
	if v6.Address != "[::]:8000" || !strings.Contains(describeExpectListener(v6), "unknown process on [::]:8000") {
		t.Errorf("v6 = %+v, %q", v6, describeExpectListener(v6))
	}
	// 머리말에는 하나만 보이지만, SYN이 향한 주소를 받는지는 그 port의 listener 모두로 판단한다.
	if strings.Join(listener.bound, ",") != "127.0.0.1,0.0.0.0" || !loopback.accepts("127.0.0.1") || loopback.accepts("10.0.0.5") {
		t.Errorf("bound = %q, loopback bound = %q", listener.bound, loopback.bound)
	}
	if none := expectListenerState(sockets, nil, 9000); none.State != expectListenerNone {
		t.Errorf("none = %+v", none)
	}
	// 목록을 읽지 못해도 관측은 계속하고 모른다고만 남긴다.
	unknown := expectListenerState(nil, errors.New("sock_diag tcp: permission denied\nmore"), 8000)
	if unknown.State != expectListenerUnknown || unknown.Error != "sock_diag tcp: permission denied" {
		t.Errorf("unknown = %+v", unknown)
	}
}

// RST의 이유는 kernel drop 이유가 있을 때만 정한다. listener는 그 이유를 고칠 곳으로 바꾸는 데만 쓴다.
func TestExplainExpectRST(t *testing.T) {
	none := &expectListener{State: expectListenerNone}
	loopback := &expectListener{State: expectListenerPresent, Process: "docker-proxy", PID: 7, Address: "127.0.0.1:5432", Scope: "loopback", bound: []string{"127.0.0.1", "::1"}}
	other := &expectListener{State: expectListenerPresent, Process: "postgres", PID: 9, Address: "10.0.3.2:5432", Scope: "specific", bound: []string{"10.0.3.2"}}
	any4 := &expectListener{State: expectListenerPresent, Process: "nginx", Address: "0.0.0.0:5432", Scope: "all", bound: []string{"0.0.0.0"}}
	any6 := &expectListener{State: expectListenerPresent, Process: "nginx", Address: "[::]:5432", Scope: "all", bound: []string{"::"}}
	unknown := &expectListener{State: expectListenerUnknown, Error: "permission denied"}
	noSocket := map[string]int{"NO_SOCKET": 2}
	for _, tc := range []struct {
		name     string
		local    string
		drops    map[string]int
		listener *expectListener
		kind     string
	}{
		{"no listener", "10.0.0.5", noSocket, none, expectRSTNoListener},
		{"loopback only", "10.0.0.5", noSocket, loopback, expectRSTLoopbackOnly},
		{"other address", "10.0.0.5", noSocket, other, expectRSTOtherAddress},
		{"ipv4 any does not take ipv6", "2001:db8::5", noSocket, any4, expectRSTOtherAddress},
		// 받는 주소의 listener가 있었는데 socket이 없었다. 시작한 뒤에 바뀐 것일 수 있으므로 kernel이 본 것만 말한다.
		{"loopback to loopback", "127.0.0.1", noSocket, loopback, expectRSTNoSocket},
		{"ipv4 any takes ipv4", "10.0.0.5", noSocket, any4, expectRSTNoSocket},
		{"ipv6 any takes ipv4", "10.0.0.5", noSocket, any6, expectRSTNoSocket},
		{"unknown listener", "10.0.0.5", noSocket, unknown, expectRSTNoSocket},
		{"nil listener", "10.0.0.5", noSocket, nil, expectRSTNoSocket},
		{"firewall wins", "10.0.0.5", map[string]int{"NETFILTER_DROP": 1, "NO_SOCKET": 1}, loopback, expectRSTFirewall},
		// drop 이유가 없으면 방화벽을 뺄 근거가 없다. listener가 없다는 것만으로 이유를 정하지 않는다.
		{"no drop reasons", "10.0.0.5", nil, none, ""},
		{"no drop reasons with a loopback listener", "10.0.0.5", nil, loopback, ""},
	} {
		if cause := explainExpectRST(5432, tc.local, tc.drops, tc.listener); cause.kind != tc.kind {
			t.Errorf("%s: kind %q, want %q", tc.name, cause.kind, tc.kind)
		}
	}
}

// 판정은 그대로 rst이고, 요약과 다음 할 일이 이유를 말한다. listener가 이미 있으면 --listen을 권하지 않는다. 그때 --listen은
// exit 2로 멈춘다.
func TestExpectRSTSummaryNamesTheCause(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	refused := func(listener *expectListener, reason string) (Result, []string) {
		scenario := newExpectScenario("")
		scenario.session.listener = listener
		scenario.in("eth0", "10.85.1.2", tcpFlagSYN, 7, 0)
		if reason != "" {
			scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:2142", reason, "tcp_v4_rcv")
		}
		scenario.out("eth0", "10.85.1.2", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
		result, sources, _ := scenario.finish("duration")
		if len(sources) != 1 || sources[0].Verdict != expectVerdictRST {
			t.Fatalf("sources = %+v", sources)
		}
		return result, expectNextSteps(expectVerdictRST, scenario.session.rstCause, false, false, "")
	}
	type want struct {
		cause, next string
		summary     []string
		step        []string
		noStep      string
	}
	docker := &expectListener{State: expectListenerPresent, Process: "docker-proxy", PID: 7, Address: "127.0.0.1:2142", Scope: "loopback", bound: []string{"127.0.0.1"}}
	nginx := &expectListener{State: expectListenerPresent, Process: "nginx", PID: 8, Address: "10.0.3.2:2142", Scope: "specific", bound: []string{"10.0.3.2"}}
	for name, tc := range map[string]struct {
		listener *expectListener
		reason   string
		want     want
	}{
		"no listener": {&expectListener{State: expectListenerNone}, "NO_SOCKET", want{expectRSTNoListener, "edc listen",
			[]string{"RST 1 sent. No process listens on port 2142"}, []string{"start the service on port 2142", "--listen"}, ""}},
		"docker on loopback": {docker, "NO_SOCKET", want{expectRSTLoopbackOnly, "edc listen",
			[]string{"The listener takes 127.0.0.1:2142 only"}, []string{"-p 100.83.200.248:2142:…", "-p 2142:…"}, "--listen"}},
		"other address": {nginx, "NO_SOCKET", want{expectRSTOtherAddress, "edc listen",
			[]string{"The listener takes 10.0.3.2:2142 only"}, []string{"0.0.0.0 or 100.83.200.248", "connect the client to 10.0.3.2"}, "--listen"}},
		"firewall": {docker, "NETFILTER_DROP", want{expectRSTFirewall, "sudo nft list ruleset",
			[]string{"A host firewall rule rejects the SYNs"}, []string{"rejects port 2142", "nft list ruleset"}, "--listen"}},
		"listener without drop reasons": {docker, "", want{"", "edc listen",
			[]string{"A host firewall rejects the SYNs, or the listener on 127.0.0.1:2142 does not take this address"}, []string{"edc listen --tcp"}, "--listen"}},
	} {
		result, steps := refused(tc.listener, tc.reason)
		if cause, _ := result.Metrics["rst_cause"].(string); cause != tc.want.cause || result.Next != tc.want.next {
			t.Errorf("%s: rst_cause %q, next %q", name, cause, result.Next)
		}
		for _, text := range tc.want.summary {
			if !strings.Contains(result.Summary, text) {
				t.Errorf("%s: summary %q lacks %q", name, result.Summary, text)
			}
		}
		// 결과 줄은 요약을 160 byte에서 자른다. 이유가 잘리면 안 된다.
		if firstLine(result.Summary) != result.Summary {
			t.Errorf("%s: summary is cut on the result line: %q", name, result.Summary)
		}
		if tc.want.cause != "" && tc.want.cause != expectRSTFirewall && strings.Contains(result.Summary, "firewall") {
			t.Errorf("%s: NO_SOCKET rules out the firewall: %q", name, result.Summary)
		}
		for _, text := range tc.want.step {
			if !strings.Contains(steps[0], text) {
				t.Errorf("%s: step %q lacks %q", name, steps[0], text)
			}
		}
		if tc.want.noStep != "" && strings.Contains(steps[0], tc.want.noStep) {
			t.Errorf("%s: step %q must not offer %q", name, steps[0], tc.want.noStep)
		}
	}
}

// 실행 중 hint는 RST와 drop 이유가 모인 ROUTE 줄 뒤에 출발지마다 한 번 보인다. 요약과 같은 근거를 쓰므로 drop 이유가 없으면
// 보이지 않는다.
func TestExpectHintNamesTheRefusalOnce(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	attempt := func(scenario *expectScenario, seq uint32, reason string) {
		scenario.in("eth0", "10.85.1.2", tcpFlagSYN, seq, time.Second)
		if reason != "" {
			scenario.session.drop(scenario.now, "10.85.1.2:53122", "100.83.200.248:2142", reason, "tcp_v4_rcv")
		}
		scenario.out("eth0", "10.85.1.2", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
		scenario.session.tick(scenario.now.Add(time.Second))
	}
	scenario := newExpectScenario("")
	scenario.session.listener = &expectListener{State: expectListenerNone}
	attempt(scenario, 7, "NO_SOCKET")
	attempt(scenario, 8, "NO_SOCKET")
	if got := scenario.kinds(); got != "syn,drop,rst_out,route,hint,syn,drop,rst_out" {
		t.Fatalf("events = %s", got)
	}
	hint := scenario.events[4]
	line := formatExpectEvent(hint, textStyle{})
	for _, want := range []string{"HINT", "10.85.1.2 → 100.83.200.248:2142", "no process listens on port 2142", "(NO_SOCKET)", "connection refused"} {
		if !strings.Contains(line, want) {
			t.Errorf("hint %q lacks %q", line, want)
		}
	}
	loopback := newExpectScenario("")
	loopback.session.listener = &expectListener{State: expectListenerPresent, Process: "docker-proxy", Address: "127.0.0.1:2142", Scope: "loopback", bound: []string{"127.0.0.1"}}
	attempt(loopback, 7, "NO_SOCKET")
	if last := loopback.events[len(loopback.events)-1]; last.kind != expectEventHint || !strings.Contains(last.detail, "takes 127.0.0.1:2142 only") || !strings.Contains(last.detail, "SYN to 100.83.200.248") {
		t.Errorf("loopback hint = %+v", last)
	}
	// drop 추적이 꺼졌으면 RST를 누가 보냈는지 근거가 없다.
	silent := newExpectScenario("")
	silent.session.listener = &expectListener{State: expectListenerNone}
	attempt(silent, 7, "")
	if got := silent.kinds(); got != "syn,rst_out,route" {
		t.Errorf("events without a drop reason = %s", got)
	}
}

func TestExpectResultCarriesTheListener(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.listener = &expectListener{State: expectListenerNone}
	result, _, _ := scenario.finish("duration")
	data, err := json.Marshal(result.Metrics["listener"])
	if err != nil || string(data) != `{"state":"none"}` {
		t.Fatalf("listener metric = %s, %v", data, err)
	}
}

// 대기 줄의 예시 명령은 client가 실제로 넣을 주소를 쓴다. --from으로 돌아가는 경로의 출발 주소가 있으면 그것이다.
func TestExpectClientAddresses(t *testing.T) {
	interfaces := []interfaceDetails{
		{Name: "docker0", Address: "172.17.0.1"},
		{Name: "eth0", Address: "10.0.0.5", Gateway: "10.0.0.1"},
		{Name: "eth1", Address: "169.254.3.4"},
		{Name: "tailscale0", Address: "100.83.200.248"},
		{Name: "wg0", Address: "10.9.0.1"},
	}
	// container bridge는 이 host 안에서만 닿으므로 뒤로 간다.
	if got := strings.Join(expectClientAddresses("", interfaces), ","); got != "10.0.0.5,100.83.200.248,10.9.0.1" {
		t.Errorf("interfaces = %s", got)
	}
	if got := strings.Join(expectClientAddresses("100.83.200.248", interfaces), ","); got != "100.83.200.248" {
		t.Errorf("route source = %s", got)
	}
	if got := expectClientAddresses("", nil); len(got) != 0 {
		t.Errorf("no interfaces = %v", got)
	}
}

func TestExpectClientRouteSourceFollowsFrom(t *testing.T) {
	asked := []string{}
	for _, from := range []string{"100.64.0.9", "100.64.0.0/10", ""} {
		scenario := newExpectScenario(from)
		scenario.session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
			asked = append(asked, remote.String())
			return expectReturnPath{Device: "tailscale0", Source: "100.83.200.248"}
		}
		got := scenario.session.clientRouteSource()
		if from == "" && got != "" {
			t.Errorf("without --from the route source = %q", got)
		}
		if from != "" && got != "100.83.200.248" {
			t.Errorf("--from %s: route source = %q", from, got)
		}
	}
	if strings.Join(asked, ",") != "100.64.0.9,100.64.0.0" {
		t.Errorf("asked = %v", asked)
	}
	failed := newExpectScenario("100.64.0.9")
	failed.session.returnPath = func(netip.Addr, netip.Addr, string) expectReturnPath { return expectReturnPath{Error: "no route"} }
	if got := failed.session.clientRouteSource(); got != "" {
		t.Errorf("failed lookup = %q", got)
	}
}

func TestExpectHeaderShowsRealAddresses(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	var out bytes.Buffer
	printer := expectLivePrinter{writer: &out, style: newTextStyle(false)}
	printer.header(expectHeader{port: 8000, dropTrace: expectDropTrace{Status: StatusPass}, addresses: []string{"10.0.0.5", "2001:db8::5", "100.83.200.248"}})
	text := out.String()
	if !strings.Contains(text, "edc tcp check --diagnose 10.0.0.5:8000") || strings.Contains(text, "<this-host>") {
		t.Errorf("waiting line = %q", text)
	}
	if !strings.Contains(text, "other addresses of this host: 2001:db8::5, 100.83.200.248") {
		t.Errorf("other addresses = %q", text)
	}
	out.Reset()
	printer.redact = true
	printer.header(expectHeader{port: 8000, dropTrace: expectDropTrace{Status: StatusPass}, addresses: []string{"10.0.0.5"}})
	if strings.Contains(out.String(), "10.0.0.5") || strings.Contains(out.String(), "other addresses") {
		t.Errorf("redacted header = %q", out.String())
	}
}

func TestParseExpectStopOptions(t *testing.T) {
	options, code, ok := parseTCPExpectArgs([]string{"--until", "handshake", "--stop-after", "3", "2142"})
	if !ok || code != 0 || options.until != "handshake" || options.stopAfter != 3 || options.port != 2142 {
		t.Fatalf("options = %+v, %d, %v", options, code, ok)
	}
	for _, args := range [][]string{{"--until", "ack", "2142"}, {"--stop-after", "0", "2142"}, {"--stop-after", "-1", "2142"}, {"--stop-after", "x", "2142"}} {
		if _, code, ok := parseTCPExpectArgs(args); ok || code != 2 {
			t.Errorf("%v: code %d, ok %v", args, code, ok)
		}
	}
}

// --until syn은 첫 SYN 뒤 유예 시간만큼 더 보아 이 host의 답을 남긴다. 답이 남아야 판정이 rst나 completed가 된다.
func TestExpectUntilSYNWaitsForTheReply(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.until = "syn"
	scenario.in("tailscale0", "100.64.0.9", tcpFlagSYN, 100, 0)
	if scenario.session.stopDue(scenario.now) {
		t.Fatal("stopped before the reply could arrive")
	}
	scenario.out("tailscale0", "100.64.0.9", tcpFlagRST|tcpFlagACK, 0, time.Millisecond)
	scenario.in("tailscale0", "100.64.0.10", tcpFlagSYN, 200, 10*time.Millisecond)
	if scenario.session.stopDue(scenario.now.Add(500 * time.Millisecond)) {
		t.Fatal("a second SYN must not move the stop time")
	}
	if !scenario.session.stopDue(scenario.now.Add(expectStopGrace)) {
		t.Fatal("not stopped after the grace")
	}
	result, sources, code := scenario.finish(scenario.session.stoppedBy())
	verdicts := map[string]string{}
	for _, source := range sources {
		verdicts[source.Address] = source.Verdict
	}
	if result.Metrics["stop_reason"] != expectStopUntilSYN || verdicts["100.64.0.9"] != expectVerdictRST || code != 1 {
		t.Fatalf("stop_reason %v, sources %+v, code %d", result.Metrics["stop_reason"], sources, code)
	}
}

func TestExpectUntilHandshakeStopsAtOnce(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.until = "handshake"
	scenario.in("eth0", "10.0.0.9", tcpFlagSYN, 100, 0)
	scenario.out("eth0", "10.0.0.9", tcpFlagSYN|tcpFlagACK, 5000, time.Millisecond)
	if scenario.session.stopDue(scenario.now.Add(time.Hour)) {
		t.Fatal("stopped without a handshake")
	}
	scenario.in("eth0", "10.0.0.9", tcpFlagACK, 101, time.Millisecond)
	if !scenario.session.stopDue(scenario.now) {
		t.Fatal("not stopped at the handshake")
	}
	result, _, code := scenario.finish(scenario.session.stoppedBy())
	if result.Metrics["stop_reason"] != expectStopUntilHandshake || code != 0 {
		t.Fatalf("stop_reason %v, code %d", result.Metrics["stop_reason"], code)
	}
}

// --stop-after는 client의 첫 sequence가 다른 연결 시도를 센다. 재전송과 다른 interface에 보인 같은 packet은 세지 않는다.
func TestExpectStopAfterCountsAttempts(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.session.stopAfter = 2
	scenario.in("eth0", "10.0.0.9", tcpFlagSYN, 100, 0)
	scenario.in("eth1", "10.0.0.9", tcpFlagSYN, 100, time.Millisecond)
	scenario.in("eth1", "10.0.0.9", tcpFlagSYN, 100, time.Second)
	if scenario.session.stopDue(scenario.now.Add(time.Hour)) {
		t.Fatal("a retransmit counted as a second attempt")
	}
	scenario.in("eth0", "10.0.0.9", tcpFlagSYN, 900, time.Second)
	if scenario.session.stopDue(scenario.now) || !scenario.session.stopDue(scenario.now.Add(expectStopGrace)) {
		t.Fatal("stop-after must stop one grace after the second attempt")
	}
	result, _, _ := scenario.finish(scenario.session.stoppedBy())
	if result.Metrics["stop_reason"] != expectStopAfter || result.Metrics["attempts"] != 2 {
		t.Fatalf("metrics = %v", result.Metrics)
	}
}

// 끝난 뒤의 다음 단계는 판정마다 볼 곳과, client report와 합치는 정확한 명령이다.
func TestExpectNextSteps(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, verdict := range append([]string{expectVerdictNoSYN}, expectVerdictOrder...) {
			steps := expectNextSteps(verdict, expectRSTCause{}, false, false, "")
			if len(steps) != 2 || strings.Contains(steps[0], "expect.next") || strings.Contains(steps[1], "expect.next") {
				t.Errorf("%s %s: %q", language, verdict, steps)
			}
		}
	}
	setLanguage("en")
	if steps := expectNextSteps(expectVerdictNoSYN, expectRSTCause{}, false, false, "/tmp/target.json"); !strings.Contains(steps[0], "tunnel selector") || !strings.HasSuffix(steps[1], "edc report join <client.json> /tmp/target.json") {
		t.Errorf("file = %q", steps)
	}
	if steps := expectNextSteps(expectVerdictRST, expectRSTCause{}, false, false, "my target.json"); !strings.Contains(steps[0], "--listen") || !strings.HasSuffix(steps[1], "edc report join <client.json> 'my target.json'") {
		t.Errorf("quoted file = %q", steps)
	}
	for _, jsonPath := range []string{"", "-"} {
		if steps := expectNextSteps(expectVerdictCompleted, expectRSTCause{}, false, false, jsonPath); !strings.Contains(steps[1], "--json target.json") {
			t.Errorf("%q: %q", jsonPath, steps)
		}
	}
}

// --listen의 임시 listener는 연결을 받자마자 닫고, 이미 쓰는 port는 빼앗지 않으며, 닫으면 port를 돌려준다.
func TestExpectTemporaryListener(t *testing.T) {
	probe, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	probe.Close()
	temporary, err := openExpectListener(port)
	if err != nil {
		t.Fatal(err)
	}
	state := temporary.state()
	if state.State != expectListenerTemporary || state.Process != "edc" || state.PID == 0 || !strings.HasSuffix(state.Address, ":"+strconv.Itoa(int(port))) {
		t.Errorf("state = %+v", state)
	}
	connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Error("the temporary listener must close the connection without data")
	}
	connection.Close()
	if second, err := openExpectListener(port); err == nil {
		second.close()
		t.Fatal("a second listener took over the port")
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("busy port error = %v", err)
	}
	temporary.close()
	again, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
	if err != nil {
		t.Fatalf("the port is still held after close: %v", err)
	}
	again.Close()
}

func TestExpectNextStepWithTemporaryListener(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	if steps := expectNextSteps(expectVerdictCompleted, expectRSTCause{}, true, false, ""); !strings.Contains(steps[0], "service itself was not tested") {
		t.Errorf("steps = %q", steps)
	}
	if steps := expectNextSteps(expectVerdictRST, expectRSTCause{}, true, false, ""); strings.Contains(steps[0], "not tested") {
		t.Errorf("rst steps = %q", steps)
	}
	// IPsec transport mode가 SYN을 가릴 수 있으면 다음 단계도 오지 않았다고 단정하지 않는다.
	if steps := expectNextSteps(expectVerdictNoSYN, expectRSTCause{}, false, true, ""); !strings.Contains(steps[0], "ip -s xfrm state") || strings.Contains(steps[0], "does not reach") {
		t.Errorf("ipsec steps = %q", steps)
	}
}
