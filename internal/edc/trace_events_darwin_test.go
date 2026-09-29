//go:build darwin

package edc

import (
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// 아래 두 message는 macOS 27에서 ntstat-fixture process의 loopback socket을 받은 실제 SRC_UPDATE다.
// TCP descriptor가 xnu 헤더보다 긴 360 byte인 것도 그대로 담았다.
const (
	ntstatTCPFixture = "000000000000000016270000000200000200000000000000000000000000000001000000000000002c01000000000000" +
		"0100000000000000e8030000000000000000000000000000000000000000000000000000000000000000000000000000" +
		"000000000000000000000000000000000000000000000000000000000100000001000000200000002000000006000000" +
		"02000000000000004ebd6101000000000000000000000000d1918568961800007c59f468961800000000000000000000" +
		"000000000000000018301100000000000100000000000000000000000000000001000000040000002c3e020000000000" +
		"003a0600000000000000000040370600e08102000000000000000000b2ff0000000000001002e0da7f00000100000000" +
		"000000000000000000000000000000001002e0d97f000001000000000000000000000000000000000000000063756269" +
		"6300000000000000000000006e74737461742d6669787475726500000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000094dc9239c22035cebe8ebc416222b545f9275c5c" +
		"81c637889b0bb35334f6804a000000000000000000000000000000000000000000000000000000000000000000000000" +
		"000000000000000000000000fffffffff5010000000000000208000800000000"
	ntstatUDPFixture = "000000000000000016270000b00100000201000000000000000000000000000000000000000000000000000000000000" +
		"010000000000000040000000000000000000000000000000000000000000000000000000000000000000000000000000" +
		"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
		"04000000000000004ebd6101000000000000000000000000eaa5856896180000f146f468961800001830110000000000" +
		"010000000000000000000000000000001002f4177f000001000000000000000000000000000000000000000010020009" +
		"7f000001000000000000000000000000000000000000000001000000d0010c000000000000000000b2ff00006e747374" +
		"61742d666978747572650000000000000000000000000000000000000000000000000000000000000000000000000000" +
		"0000000000000000000000000000000094dc9239c22035cebe8ebc416222b545f9275c5c81c637889b0bb35334f6804a" +
		"0000000000000000000000000000000000000000000000000000000000000000fffffffff50100000210004000000000"
)

func ntstatFixture(t *testing.T, encoded string) []byte {
	t.Helper()
	message, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestDecodeNtstatUpdateFixtures(t *testing.T) {
	tcp, err := decodeNtstatUpdate(ntstatFixture(t, ntstatTCPFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := ntstatSource{
		ref: 2, protocol: "tcp", pid: 65458, process: "ntstat-fixture", state: darwinTCPEstablished,
		local: "127.0.0.1:57562", localPort: 57562, remote: "127.0.0.1:57561",
		counts: ntstatCounts{rxPackets: 1, rxBytes: 300, txPackets: 1, txBytes: 1000},
	}
	if tcp != want {
		t.Fatalf("TCP source = %#v, want %#v", tcp, want)
	}
	udp, err := decodeNtstatUpdate(ntstatFixture(t, ntstatUDPFixture))
	if err != nil {
		t.Fatal(err)
	}
	want = ntstatSource{
		ref: 258, protocol: "udp", pid: 65458, process: "ntstat-fixture",
		local: "127.0.0.1:62487", localPort: 62487, remote: "127.0.0.1:9",
		counts: ntstatCounts{txPackets: 1, txBytes: 64},
	}
	if udp != want {
		t.Fatalf("UDP source = %#v, want %#v", udp, want)
	}
}

func TestDecodeNtstatUpdateRejectsUnknownLayout(t *testing.T) {
	fixture := ntstatFixture(t, ntstatTCPFixture)
	truncated := fixture[:ntstatUpdateDescriptor+ntstatTCPDescriptorSize-1]
	badAddress := append([]byte(nil), fixture...)
	badAddress[ntstatUpdateDescriptor+124] = 0
	badState := append([]byte(nil), fixture...)
	badState[ntstatUpdateDescriptor+76] = 99
	for name, test := range map[string]struct {
		message []byte
		want    string
	}{
		"truncated":   {truncated, "TCP descriptor is 259 bytes"},
		"bad address": {badAddress, "tcp descriptor (360 bytes): sockaddr length 0"},
		"bad state":   {badState, "(360 bytes) has state 99"},
	} {
		if _, err := decodeNtstatUpdate(test.message); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s error = %v, want %q", name, err, test.want)
		}
	}
}

// ntstatExtendedFixture는 실제 SRC_UPDATE 뒤에 kernel과 같은 규칙으로 확장 항목 하나를 붙인다.
// header의 길이는 확장 크기이고, 데이터는 8 byte 경계까지 채운다.
func ntstatExtendedFixture(t *testing.T, encoded string, kind uint32, domain string) []byte {
	t.Helper()
	const domainInfoSize = 1056
	message := ntstatFixture(t, encoded)
	item := make([]byte, ntstatExtensionHeaderSize+domainInfoSize)
	binary.LittleEndian.PutUint32(item[0:], kind)
	binary.LittleEndian.PutUint32(item[4:], domainInfoSize)
	copy(item[ntstatExtensionHeaderSize:], domain)
	message = append(message, item...)
	binary.LittleEndian.PutUint32(message[8:], ntstatMsgSrcExtendedUpdate)
	binary.LittleEndian.PutUint16(message[12:], uint16(len(message)))
	return message
}

func TestDecodeNtstatExtendedUpdateReadsDomain(t *testing.T) {
	for name, encoded := range map[string]string{"tcp": ntstatTCPFixture, "udp": ntstatUDPFixture} {
		source, err := decodeNtstatUpdate(ntstatExtendedFixture(t, encoded, ntstatExtensionDomain, "api.example.com"))
		if err != nil || source.domain != "api.example.com" || source.process != "ntstat-fixture" || source.protocol != name {
			t.Fatalf("%s source = %#v, err = %v", name, source, err)
		}
	}
	if source, err := decodeNtstatUpdate(ntstatExtendedFixture(t, ntstatTCPFixture, 0, "")); err != nil || source.domain != "" {
		t.Fatalf("unknown extension source = %#v, err = %v", source, err)
	}
	truncated := ntstatExtendedFixture(t, ntstatTCPFixture, ntstatExtensionDomain, "api.example.com")
	truncated = truncated[:len(truncated)-8]
	binary.LittleEndian.PutUint16(truncated[12:], uint16(len(truncated)))
	if _, err := decodeNtstatUpdate(truncated); err == nil || !strings.Contains(err.Error(), "has no domain extension") {
		t.Fatalf("truncated extension error = %v", err)
	}
}

func TestDecodeNtstatUpdateReadsUserlandProviders(t *testing.T) {
	for provider, test := range map[uint32]struct {
		encoded  string
		protocol string
	}{
		ntstatProviderTCPUserland: {ntstatTCPFixture, "tcp"},
		ntstatProviderUDPUserland: {ntstatUDPFixture, "udp"},
		// QUIC descriptor는 TCP descriptor와 배치가 같지만 UDP flow로 보고한다.
		ntstatProviderQUICUserland: {ntstatTCPFixture, "udp"},
	} {
		message := ntstatFixture(t, test.encoded)
		binary.LittleEndian.PutUint32(message[ntstatUpdateProvider:], provider)
		source, err := decodeNtstatUpdate(message)
		if err != nil || source.protocol != test.protocol || source.pid != 65458 || source.process != "ntstat-fixture" {
			t.Fatalf("provider %d source = %#v, err = %v", provider, source, err)
		}
		if provider == ntstatProviderQUICUserland && (source.state != 0 || source.local != "127.0.0.1:57562" || source.remote != "127.0.0.1:57561") {
			t.Fatalf("QUIC source = %#v", source)
		}
	}
}

func TestNtstatTrackerKeepsDomainAfterFinalUpdate(t *testing.T) {
	tracker := newNtstatTracker()
	tracker.finishBaseline()
	tracker.added(5)
	source := ntstatTestSource(5, "tcp", darwinTCPEstablished, 50000, ntstatCounts{txPackets: 1, txBytes: 10})
	source.domain = "api.example.com"
	events := tracker.updated(source, traceStamp{})
	if ntstatEventNames(events) != "tcp_connect,tcp_send" || events[0].Target != "api.example.com" {
		t.Fatalf("events = %#v", events)
	}
	final := ntstatTestSource(5, "tcp", darwinTCPTimeWait, 50000, ntstatCounts{txPackets: 1, txBytes: 10})
	events = tracker.updated(final, traceStamp{})
	if ntstatEventNames(events) != "tcp_close" || events[0].Target != "api.example.com" {
		t.Fatalf("final events = %#v", events)
	}
}

func TestNtstatAddressMatchesLinuxFormat(t *testing.T) {
	sockaddr6 := func(address string, port uint16) []byte {
		sockaddr := make([]byte, 28)
		sockaddr[0], sockaddr[1] = 28, 30
		binary.BigEndian.PutUint16(sockaddr[2:], port)
		copy(sockaddr[8:24], net.ParseIP(address).To16())
		return sockaddr
	}
	for address, want := range map[string]string{"::1": "[::1]:53", "2001:db8::10": "[2001:db8::10]:53", "::ffff:192.0.2.1": "192.0.2.1:53"} {
		if got, port, err := ntstatAddress(sockaddr6(address, 53)); err != nil || got != want || port != 53 {
			t.Fatalf("ntstatAddress(%s) = %q, %d, %v, want %q", address, got, port, err, want)
		}
	}
}

func ntstatTestSource(ref uint64, protocol string, state uint32, localPort uint16, counts ntstatCounts) ntstatSource {
	return ntstatSource{ref: ref, protocol: protocol, pid: 42, process: "curl", state: state, local: "10.0.0.2:1", localPort: localPort, remote: "203.0.113.10:443", counts: counts}
}

func ntstatEventNames(events []captureEvent) string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Event)
	}
	return strings.Join(names, ",")
}

func TestNtstatTrackerUsesBaselineCounters(t *testing.T) {
	tracker := newNtstatTracker()
	stamp := traceStamp{wallNS: 10, bootNS: 5}
	tracker.added(1)
	if events := tracker.updated(ntstatTestSource(1, "tcp", darwinTCPEstablished, 50000, ntstatCounts{txPackets: 3, txBytes: 1000}), stamp); len(events) != 0 {
		t.Fatalf("baseline events = %#v", events)
	}
	tracker.finishBaseline()
	events := tracker.updated(ntstatTestSource(1, "tcp", darwinTCPEstablished, 50000, ntstatCounts{txPackets: 5, txBytes: 1500, retransmitBytes: 100}), stamp)
	if ntstatEventNames(events) != "tcp_send,tcp_retransmit" {
		t.Fatalf("events = %s", ntstatEventNames(events))
	}
	if events[0].Bytes != 500 || events[0].Packets != 2 || events[1].Bytes != 100 || events[0].TimestampNS != 10 || events[0].BootTimeNS != 5 || events[0].Process != "curl" {
		t.Fatalf("events = %#v", events)
	}
	if events := tracker.updated(ntstatTestSource(1, "tcp", darwinTCPEstablished, 50000, ntstatCounts{txPackets: 5, txBytes: 1500, retransmitBytes: 100}), stamp); len(events) != 0 {
		t.Fatalf("unchanged events = %#v", events)
	}
	// payload 없는 ACK만 오간 구간은 packet counter만 늘어난다.
	if events := tracker.updated(ntstatTestSource(1, "tcp", darwinTCPEstablished, 50000, ntstatCounts{rxPackets: 1, txPackets: 6, txBytes: 1500, retransmitBytes: 100}), stamp); len(events) != 0 {
		t.Fatalf("ACK-only events = %#v", events)
	}
	if events := tracker.removed(1, stamp); ntstatEventNames(events) != "tcp_close" || events[0].OldState != "ESTABLISHED" || events[0].NewState != "CLOSE" {
		t.Fatalf("removed events = %#v", events)
	}
}

func TestNtstatTrackerConnectionLifecycle(t *testing.T) {
	tracker := newNtstatTracker()
	stamp := traceStamp{}
	tracker.updated(ntstatTestSource(10, "tcp", darwinTCPListen, 8080, ntstatCounts{}), stamp)
	tracker.finishBaseline()

	tracker.added(2)
	events := tracker.updated(ntstatTestSource(2, "tcp", 2, 50000, ntstatCounts{}), stamp)
	if ntstatEventNames(events) != "tcp_state" || events[0].OldState != "" || events[0].NewState != "SYN_SENT" {
		t.Fatalf("SYN_SENT events = %#v", events)
	}
	events = tracker.updated(ntstatTestSource(2, "tcp", darwinTCPEstablished, 50000, ntstatCounts{txPackets: 1, txBytes: 10}), stamp)
	if ntstatEventNames(events) != "tcp_connect,tcp_send" || events[0].OldState != "SYN_SENT" || events[0].NewState != "ESTABLISHED" {
		t.Fatalf("connect events = %#v", events)
	}
	events = tracker.updated(ntstatTestSource(2, "tcp", darwinTCPTimeWait, 50000, ntstatCounts{txPackets: 1, txBytes: 10}), stamp)
	if ntstatEventNames(events) != "tcp_close" || events[0].OldState != "ESTABLISHED" {
		t.Fatalf("TIME_WAIT events = %#v", events)
	}
	if events := tracker.removed(2, stamp); len(events) != 0 {
		t.Fatalf("second close = %#v", events)
	}

	tracker.added(3)
	events = tracker.updated(ntstatTestSource(3, "tcp", darwinTCPEstablished, 8080, ntstatCounts{rxPackets: 1, rxBytes: 20}), stamp)
	if ntstatEventNames(events) != "tcp_accept,tcp_receive" || events[1].Bytes != 20 {
		t.Fatalf("accept events = %#v", events)
	}

	// 두 poll 사이에 연결과 종료가 모두 끝난 socket은 CLOSED로 처음 보인다.
	tracker.added(4)
	events = tracker.updated(ntstatTestSource(4, "tcp", darwinTCPClosed, 50001, ntstatCounts{rxPackets: 1, rxBytes: 300, txPackets: 1, txBytes: 100}), stamp)
	if ntstatEventNames(events) != "tcp_connect,tcp_send,tcp_receive,tcp_close" || events[0].NewState != "ESTABLISHED" || events[3].OldState != "ESTABLISHED" {
		t.Fatalf("short connection events = %#v", events)
	}
}

// Linux는 만들기만 하고 연결하지 않은 socket에 event를 내지 않고, 거부된 connect는 tcp_close 하나로 보고한다.
func TestNtstatTrackerIgnoresSocketsThatNeverConnect(t *testing.T) {
	unconnected := func(ref uint64, port uint16) ntstatSource {
		source := ntstatTestSource(ref, "tcp", darwinTCPClosed, port, ntstatCounts{})
		source.remote = "0.0.0.0:0"
		return source
	}
	tracker := newNtstatTracker()
	stamp := traceStamp{}
	tracker.updated(unconnected(20, 50010), stamp)
	tracker.finishBaseline()

	tracker.added(21)
	if events := tracker.updated(unconnected(21, 50011), stamp); len(events) != 0 {
		t.Fatalf("unconnected socket events = %#v", events)
	}
	if events := tracker.removed(21, stamp); len(events) != 0 {
		t.Fatalf("unconnected socket removal = %#v", events)
	}

	tracker.added(22)
	if events := tracker.updated(ntstatTestSource(22, "tcp", 2, 50012, ntstatCounts{}), stamp); ntstatEventNames(events) != "tcp_state" {
		t.Fatalf("SYN_SENT events = %#v", events)
	}
	events := tracker.updated(ntstatTestSource(22, "tcp", darwinTCPClosed, 50012, ntstatCounts{}), stamp)
	if ntstatEventNames(events) != "tcp_close" || events[0].OldState != "SYN_SENT" || events[0].NewState != "CLOSE" {
		t.Fatalf("refused connect events = %#v", events)
	}
	if events := tracker.removed(22, stamp); len(events) != 0 {
		t.Fatalf("refused connect removal = %#v", events)
	}

	// poll 사이에 끝난 거부된 connect는 CLOSED로 처음 보이지만, 상대 주소가 남는다.
	tracker.added(23)
	events = tracker.updated(ntstatTestSource(23, "tcp", darwinTCPClosed, 50013, ntstatCounts{}), stamp)
	if ntstatEventNames(events) != "tcp_close" || events[0].OldState != "SYN_SENT" || events[0].NewState != "CLOSE" {
		t.Fatalf("fast refused connect events = %#v", events)
	}

	// trace를 시작할 때 CLOSED였던 socket도 나중에 연결하면 connect와 close를 낸다.
	events = tracker.updated(ntstatTestSource(20, "tcp", darwinTCPEstablished, 50010, ntstatCounts{txPackets: 1, txBytes: 10}), stamp)
	if ntstatEventNames(events) != "tcp_connect,tcp_send" {
		t.Fatalf("baseline CLOSED socket events = %s", ntstatEventNames(events))
	}
	if events := tracker.removed(20, stamp); ntstatEventNames(events) != "tcp_close" {
		t.Fatalf("baseline CLOSED socket removal = %#v", events)
	}
}

func TestDecodeNtstatUpdateLeavesUnconnectedUDPDestinationEmpty(t *testing.T) {
	message := ntstatFixture(t, ntstatUDPFixture)
	remote := message[ntstatUpdateDescriptor+84 : ntstatUpdateDescriptor+112]
	for index := 2; index < 8; index++ {
		remote[index] = 0
	}
	source, err := decodeNtstatUpdate(message)
	if err != nil || source.remote != "" || source.local != "127.0.0.1:62487" {
		t.Fatalf("source = %#v, err = %v", source, err)
	}
	tcp, err := decodeNtstatUpdate(ntstatFixture(t, ntstatTCPFixture))
	if err != nil || tcp.remote != "127.0.0.1:57561" {
		t.Fatalf("TCP source = %#v, err = %v", tcp, err)
	}
}

func TestNtstatTrackerUDPCounters(t *testing.T) {
	tracker := newNtstatTracker()
	tracker.finishBaseline()
	tracker.added(7)
	events := tracker.updated(ntstatTestSource(7, "udp", 0, 53000, ntstatCounts{txPackets: 2, txBytes: 128, rxPackets: 1, rxBytes: 512}), traceStamp{})
	if ntstatEventNames(events) != "udp_send,udp_receive" || events[0].Packets != 2 || events[0].Bytes != 128 || events[1].Packets != 1 || events[1].Bytes != 512 || events[0].Protocol != "udp" {
		t.Fatalf("UDP events = %#v", events)
	}
	events = tracker.updated(ntstatTestSource(7, "udp", 0, 53000, ntstatCounts{txPackets: 3, txBytes: 128, rxPackets: 1, rxBytes: 512}), traceStamp{})
	if ntstatEventNames(events) != "udp_send" || events[0].Packets != 1 || events[0].Bytes != 0 {
		t.Fatalf("zero-length datagram events = %#v", events)
	}
	if events := tracker.removed(7, traceStamp{}); len(events) != 0 {
		t.Fatalf("UDP removal events = %#v", events)
	}
}

// 이 test는 실제 kernel의 ntstat을 읽는다. CI의 macOS에서 구조체 배치가 달라지면 여기서 실패해야 하므로,
// control socket을 열 수 없을 때만 건너뛴다.
func TestNtstatCollectorTracesLoopbackConnection(t *testing.T) {
	client, err := openNtstat()
	if err != nil {
		t.Skipf("ntstat is not available: %v", err)
	}
	defer client.close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var seen []captureEvent
	collector := &ntstatCollector{client: client, tracker: newNtstatTracker(), onEvent: func(event captureEvent) error {
		seen = append(seen, event)
		return nil
	}}
	if err := collector.start(); err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("accept timed out")
	}
	if _, err := conn.Write(make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(server, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write(make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	clientLocal := conn.LocalAddr().String()
	conn.Close()
	server.Close()

	names := map[string]bool{}
	var sent, received uint64
	accept := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !names["tcp_close"]; time.Sleep(200 * time.Millisecond) {
		if err := collector.poll(); err != nil {
			t.Fatal(err)
		}
		names, sent, received, accept = map[string]bool{}, 0, 0, false
		for _, event := range seen {
			if event.PID != uint32(os.Getpid()) {
				continue
			}
			if event.Source == listener.Addr().String() && event.Destination == clientLocal && event.Event == "tcp_accept" {
				accept = true
			}
			if event.Source != clientLocal {
				continue
			}
			names[event.Event] = true
			switch event.Event {
			case "tcp_send":
				sent += event.Bytes
			case "tcp_receive":
				received += event.Bytes
			}
		}
	}
	if !names["tcp_connect"] || !names["tcp_close"] || sent != 1000 || received != 300 || !accept {
		t.Fatalf("client events = %v, sent %d, received %d, accept %t; all events = %#v", names, sent, received, accept, seen)
	}
}

func TestCollectTraceEventsLiveStops(t *testing.T) {
	if err := captureEventsPrerequisites(); err != nil {
		t.Skipf("ntstat is not available: %v", err)
	}
	stop := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(stop) })
	started := time.Now()
	summary, err := collectTraceEventsLive(traceScope{protocol: "tcp"}, 0, nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Event != "capture_summary" || time.Since(started) > 5*time.Second {
		t.Fatalf("summary = %#v after %s", summary, time.Since(started))
	}
}
