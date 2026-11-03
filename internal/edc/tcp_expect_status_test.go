package edc

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/bpf"
)

func TestExpectOtherSYNFilterKeepsOtherPortSYNsOnly(t *testing.T) {
	program, err := expectOtherSYNFilter(expectTestPort)
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
	fragment := expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, 22, tcpFlagSYN, 1, 0)
	fragment[6] = 0x00
	fragment[7] = 10
	cases := []struct {
		name   string
		packet []byte
		keep   bool
	}{
		{"ipv4 SYN to another port", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, 22, tcpFlagSYN, 1, 0), true},
		{"ipv4 SYN to this port", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, expectTestPort, tcpFlagSYN, 1, 0), false},
		{"ipv4 SYN-ACK", expectTestIPv4("10.0.0.2", "10.0.0.1", 22, 40000, tcpFlagSYN|tcpFlagACK, 1, 0), false},
		{"ipv4 ACK", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, 22, tcpFlagACK, 2, 0), false},
		{"ipv4 RST", expectTestIPv4("10.0.0.1", "10.0.0.2", 40000, 22, tcpFlagRST, 2, 0), false},
		{"ipv4 fragment", fragment, false},
		{"ipv6 SYN to another port", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, 443, tcpFlagSYN, 1, 0), true},
		{"ipv6 SYN to this port", expectTestIPv6("2001:db8::1", "2001:db8::2", 40000, expectTestPort, tcpFlagSYN, 1, 0), false},
		{"ipv6 SYN-ACK", expectTestIPv6("2001:db8::2", "2001:db8::1", 443, 40000, tcpFlagSYN|tcpFlagACK, 1, 0), false},
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
		remote, local, port, ok := parseExpectOtherSYN(test.packet, expectTestPort)
		if test.name != "ipv4 fragment" && ok != test.keep {
			t.Errorf("%s: parsed %v, want %v", test.name, ok, test.keep)
		}
		if ok && (!remote.IsValid() || !local.IsValid() || port == expectTestPort) {
			t.Errorf("%s: parsed %s → %s port %d", test.name, remote, local, port)
		}
	}
	if _, _, _, ok := parseExpectOtherSYN(fragment, expectTestPort); ok {
		t.Error("a later fragment has no TCP header")
	}
}

// 다른 port의 SYN은 port별 수만 남긴다. --from, loopback, 다른 host로 넘겨주는 packet은 세지 않는다.
func TestExpectCountsOtherPortSYNs(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	scenario := newExpectScenario("100.64.0.0/10")
	scenario.session.isLocal = func(address netip.Addr) bool { return address.String() != "10.9.9.9" }
	host := netip.MustParseAddr("100.83.200.248")
	scenario.session.otherSYN(netip.MustParseAddr("100.64.0.9"), host, 2142)
	if scenario.session.snapshot().OtherPorts != nil {
		t.Fatal("counted before the other-port socket opened")
	}
	scenario.session.countOtherPorts()
	for index := 0; index < 3; index++ {
		scenario.session.otherSYN(netip.MustParseAddr("100.64.0.9"), host, 2143)
	}
	for _, port := range []uint16{22, 80, 443, 8080, 9000, 9001} {
		scenario.session.otherSYN(netip.MustParseAddr("100.64.0.9"), host, port)
	}
	scenario.session.otherSYN(netip.MustParseAddr("100.64.0.9"), host, 80)
	scenario.session.otherSYN(netip.MustParseAddr("10.0.0.9"), host, 2143)
	scenario.session.otherSYN(netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.1"), 2143)
	scenario.session.otherSYN(netip.MustParseAddr("100.64.0.9"), netip.MustParseAddr("10.9.9.9"), 2143)
	if got := formatExpectPorts(scenario.session.snapshot().OtherPorts); got != "2143×3, 80×2, 22×1" {
		t.Errorf("status ports = %s", got)
	}
	result, _, _ := scenario.finish(expectStopDuration)
	data, _ := json.Marshal(result.Metrics["other_port_syns"])
	if string(data) != `{"2143":3,"22":1,"443":1,"80":2,"8080":1}` {
		t.Errorf("other_port_syns = %s", data)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "port 2142") || !strings.Contains(result.Warnings[0], "2143×3") {
		t.Errorf("warnings = %q", result.Warnings)
	}
}

func TestExpectLiveCounts(t *testing.T) {
	scenario := newExpectScenario("")
	scenario.in("eth0", "10.0.0.9", tcpFlagSYN, 100, 0)
	scenario.in("eth0", "10.0.0.9", tcpFlagSYN, 100, time.Second)
	scenario.out("eth0", "10.0.0.9", tcpFlagSYN|tcpFlagACK, 5000, time.Millisecond)
	scenario.in("eth0", "10.0.0.9", tcpFlagACK, 101, time.Millisecond)
	scenario.session.drop(scenario.now, "10.0.0.8:53122", "100.83.200.248:2142", "NO_SOCKET", "")
	counts := scenario.session.snapshot()
	if counts.SYN != 2 || counts.Handshakes != 1 || counts.Drops != 1 || counts.OtherPorts != nil {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestExpectStatusLine(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	width := 120
	status := newExpectStatus(&out, func() int { return width }, func() time.Time { return now })
	now = now.Add(12 * time.Second)
	status.update(expectLiveCounts{SYN: 3, Handshakes: 1, Drops: 2, OtherPorts: []expectPortCount{{2142, 3}, {22, 1}}})
	if got := status.line(width); got != "─ 00:12 · SYN 3 · handshakes 1 · drops 2 · other ports: 2142×3, 22×1" {
		t.Errorf("line = %q", got)
	}
	// 좁으면 다른 port를 먼저 뺀다.
	if got := status.line(45); got != "─ 00:12 · SYN 3 · handshakes 1 · drops 2" {
		t.Errorf("narrow line = %q", got)
	}
	// 실행 중 줄은 상태줄을 지우고 쓴 뒤 다시 그린다.
	out.Reset()
	status.Write([]byte("SYN in\n"))
	text := out.String()
	if !strings.HasPrefix(text, "\r\x1b[2KSYN in\n") || !strings.Contains(text, "SYN 3") {
		t.Errorf("write = %q", text)
	}
	out.Reset()
	status.close()
	status.Write([]byte("summary\n"))
	if out.String() != "\r\x1b[2Ksummary\n" {
		t.Errorf("after close = %q", out.String())
	}
}
