//go:build linux

package edc

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/cilium/ebpf/btf"
	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

func TestCaptureEventAddressFormatting(t *testing.T) {
	var address [16]byte
	address[0], address[1], address[2], address[3] = 192, 0, 2, 10
	if got := formatCaptureAddress(2, address, 443); got != "192.0.2.10:443" {
		t.Fatalf("IPv4 address = %q", got)
	}
	address[15] = 1
	// 사람이 읽는 IPv6는 RFC 5952 축약형이다. 0이 이어진 구간을 ::로 줄인다.
	if got := formatCaptureAddress(10, address, 443); got != "[c000:20a::1]:443" {
		t.Fatalf("IPv6 address = %q", got)
	}
	if got := formatCaptureAddress(10, [16]byte{15: 1}, 18090); got != "[::1]:18090" {
		t.Fatalf("IPv6 loopback = %q", got)
	}
	if got := formatCaptureAddress(10, [16]byte{}, 0); got != "[::]:0" {
		t.Fatalf("IPv6 unspecified = %q", got)
	}
	// dual-stack socket이 IPv4와 주고받으면 kernel은 IPv4를 ::ffff:a.b.c.d로 담는다. 사람이 읽는 주소는 IPv4다.
	mapped := [16]byte{10: 0xff, 11: 0xff, 12: 20, 13: 20, 14: 0, 15: 50}
	if got := formatCaptureAddress(10, mapped, 6443); got != "20.20.0.50:6443" {
		t.Fatalf("IPv4-mapped address = %q", got)
	}
}

func TestTraceTargetFromArguments(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"curl", "https://naver.com/path"}, "naver.com"},
		{[]string{"curl", "naver.com"}, "naver.com"},
		{[]string{"curl", "http://127.0.0.1:8080/health"}, "127.0.0.1"},
		{[]string{"curl", "--fail", "https://example.com"}, "example.com"},
		{[]string{"gh", "run", "list", "--json", "headSha,status", "--jq", `.[] | select(.headSha=="255d") | "\(.status)"`}, ""},
		{[]string{"curl", "https://user:secret@example.com/x"}, "example.com"},
		{[]string{"curl", "-H", "Host: api.example.com", "https://10.0.0.5/"}, "10.0.0.5"},
		{[]string{"curl", "-o", "out.json", "https://example.com/"}, "example.com"},
		{[]string{"curl", "http://[::1]:8080/"}, "::1"},
		{[]string{"curl", "http://[::1]/"}, "::1"},
		{[]string{"curl", "http://localhost:8080/"}, "localhost"},
		{[]string{"curl", "https://example.com:https/"}, ""},
		{[]string{"ping", "2001:db8::1"}, "2001:db8::1"},
		{[]string{"nc", "example.com:443"}, "example.com"},
		{[]string{"ssh", "root@20.20.0.68"}, "root@20.20.0.68"},
		{[]string{"ssh", "root@server1"}, ""},
		{[]string{"getent", "hosts", "n1.example.com"}, "n1.example.com"},
		{[]string{"curl", "https://例え.jp/"}, "例え.jp"},
		{[]string{"ping", "1.2.3"}, ""},
		{[]string{"dig", "a..b"}, ""},
		{[]string{"dig", "bad-.example.com"}, ""},
		{[]string{"dig", "example.com."}, "example.com"},
		{[]string{"etcd", "--advertise-client-urls=https://20.20.0.50:2379", "--data-dir=/var/lib/etcd"}, ""},
		{[]string{"kube-apiserver", "--etcd-servers=https://127.0.0.1:2379", "--advertise-address=20.20.0.50"}, ""},
		{[]string{"curl", "--connect-timeout", "5", "https://example.com/"}, "example.com"},
	}
	for _, test := range cases {
		if got := traceTargetFromArguments(test.args); got != test.want {
			t.Fatalf("traceTargetFromArguments(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestCaptureAttachmentsFollowTheProtocol(t *testing.T) {
	tcpTracepoints := []string{"sock/inet_sock_set_state", "tcp/tcp_retransmit_skb", "tcp/tcp_send_reset", "tcp/tcp_receive_reset", "tcp/tcp_destroy_sock", "sock/sock_send_length", "sock/sock_recv_length"}
	udpSend := []string{"fentry/udp_send_skb", "fexit/udp_send_skb", "fentry/udp_v6_send_skb", "fexit/udp_v6_send_skb"}
	tcpAccept := []string{"fentry/inet_csk_accept", "fexit/tcp_create_openreq_child"}
	for _, test := range []struct {
		protocol    string
		tracepoints []string
		tracing     []string
	}{
		{"", tcpTracepoints, append(append(append(append([]string{}, udpSend...), "fentry/skb_consume_udp"), tcpAccept...), "fentry/__inet_stream_connect")},
		// TCP도 DNS 응답으로 target 이름을 지으므로 skb_consume_udp를 붙인다.
		{"tcp", tcpTracepoints, append(append([]string{"fentry/skb_consume_udp"}, tcpAccept...), "fentry/__inet_stream_connect")},
		{"udp", []string{}, append(append([]string{}, udpSend...), "fentry/skb_consume_udp")},
		// DNS 질의는 UDP 송신 hook이, 응답은 skb_consume_udp가, port 53 TCP 연결은 inet_sock_set_state가 알린다.
		// 수신 큐 hook은 DNS 응답 시간을 나누는 데만 쓴다.
		// DNS over TCP의 message는 HTTP와 같은 TCP 송수신 hook이 읽는다.
		{"dns", []string{"sock/inet_sock_set_state"}, append(append(append([]string{}, udpSend...), "fentry/__udp_enqueue_schedule_skb", "fentry/skb_consume_udp", "fentry/tcp_sendmsg", "fentry/tcp_recvmsg", "fexit/tcp_recvmsg"), tcpAccept...)},
		// HTTP는 TCP 송수신의 사용자 버퍼만 읽고 TCP 상태 변화는 쓰지 않는다.
		{"http", []string{}, []string{"fentry/skb_consume_udp", "fentry/tcp_sendmsg", "fentry/tcp_recvmsg", "fexit/tcp_recvmsg"}},
	} {
		tracepoints, tracing := captureAttachments(&captureEventsObjects{}, test.protocol)
		gotTracepoints := []string{}
		for _, hook := range tracepoints {
			gotTracepoints = append(gotTracepoints, hook.group+"/"+hook.name)
		}
		gotTracing := []string{}
		for _, hook := range tracing {
			gotTracing = append(gotTracing, hook.name)
		}
		if !slices.Equal(gotTracepoints, test.tracepoints) || !slices.Equal(gotTracing, test.tracing) {
			t.Fatalf("protocol %q: tracepoints %q, tracing %q; want %q, %q", test.protocol, gotTracepoints, gotTracing, test.tracepoints, test.tracing)
		}
	}
}

func TestCaptureEventFiltersFollowTheProtocol(t *testing.T) {
	for _, test := range []struct {
		scope traceScope
		want  captureEventFilter
	}{
		{traceScope{}, captureEventFilter{udpEvents: true, dnsSent: true}},
		{traceScope{protocol: "tcp"}, captureEventFilter{dnsSent: true}},
		{traceScope{protocol: "udp"}, captureEventFilter{udpEvents: true}},
		{traceScope{protocol: "dns"}, captureEventFilter{dnsSent: true, tcpStatePort: 53, dnsTCP: true}},
		{traceScope{protocol: "dns", server: true}, captureEventFilter{dnsSent: true, server: true, tcpStatePort: 53, dnsTCP: true}},
		{traceScope{protocol: "http"}, captureEventFilter{dnsSent: true, httpMessages: true}},
		{traceScope{protocol: "http", payload: true}, captureEventFilter{dnsSent: true, httpMessages: true, httpPayload: true}},
	} {
		if got := captureEventFilterFor(test.scope); got != test.want {
			t.Fatalf("scope %+v filter = %+v, want %+v", test.scope, got, test.want)
		}
	}
	// 변수 이름과 기본값은 BPF C 코드에 있다. 이름이 어긋나면 kernel에 불러오기 전에 여기서 실패한다.
	spec, err := loadCaptureEvents()
	if err != nil {
		t.Fatal(err)
	}
	var variables captureEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		t.Fatal(err)
	}
	var udpEvents, dnsSent, server uint8
	var tcpStatePort uint16
	var httpPayloadLimit uint32
	if err := errors.Join(variables.EmitUdpEvents.Get(&udpEvents), variables.EmitDnsSent.Get(&dnsSent), variables.EmitDnsServer.Get(&server), variables.TcpStatePort.Get(&tcpStatePort), variables.HttpPayloadLimit.Get(&httpPayloadLimit)); err != nil {
		t.Fatal(err)
	}
	// capture는 모든 event와 client 쪽 DNS 레코드를 받는다. 서버 쪽 레코드는 trace dns --side server만 켠다.
	// HTTP message는 --payload가 아니면 요청 줄과 Host가 들어가는 512바이트만 읽는다.
	if udpEvents != 1 || dnsSent != 1 || server != 0 || tcpStatePort != 0 || httpPayloadLimit != 512 {
		t.Fatalf("BPF defaults = %d, %d, %d, %d, %d", udpEvents, dnsSent, server, tcpStatePort, httpPayloadLimit)
	}
}

func TestSocketTargetCacheFillsAbortedDestroyAddresses(t *testing.T) {
	cache := newSocketTargetCache()
	for _, test := range []struct {
		name        string
		event       captureEvent
		source      string
		destination string
	}{
		// connect()의 첫 전이는 port를 배정하기 전이라 채우지도 기억하지도 않는다.
		{"syn sent", captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_state", Source: "127.0.0.1:0", Destination: "127.0.0.1:18099"}, "127.0.0.1:0", "127.0.0.1:18099"},
		{"send", captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_send", Source: "127.0.0.1:55452", Destination: "127.0.0.1:18099", Target: "api.example"}, "127.0.0.1:55452", "127.0.0.1:18099"},
		// SO_LINGER 0으로 닫으면 kernel이 local 주소와 상대 port를 지운 뒤 socket을 해제한다.
		{"aborted destroy", captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_destroy", Source: "0.0.0.0:55452", Destination: "127.0.0.1:0"}, "127.0.0.1:55452", "127.0.0.1:18099"},
		// 주소를 본 적이 없는 socket은 connect하지 못한 것이라 그대로 둔다.
		{"unbound destroy", captureEvent{Protocol: "tcp", SocketID: 8, Event: "tcp_destroy", Source: "[::]:0", Destination: "[2001:db8::1]:0"}, "[::]:0", "[2001:db8::1]:0"},
		{"udp", captureEvent{Protocol: "udp", SocketID: 9, Event: "udp_send", Source: "0.0.0.0:5353", Destination: "224.0.0.251:5353"}, "0.0.0.0:5353", "224.0.0.251:5353"},
	} {
		source, destination := cache.addresses(test.event)
		if source != test.source || destination != test.destination {
			t.Fatalf("%s: addresses = %s -> %s, want %s -> %s", test.name, source, destination, test.source, test.destination)
		}
		test.event.Source, test.event.Destination = source, destination
		target, _ := cache.target(test.event)
		if test.name == "aborted destroy" && target != "api.example" {
			t.Fatalf("aborted destroy lost the socket target: %q", target)
		}
	}
	// destroy 뒤에는 socket 주소를 새 socket이 다시 쓰므로 기억을 지운다.
	if source, _ := cache.addresses(captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_destroy", Source: "0.0.0.0:1", Destination: "127.0.0.1:0"}); source != "0.0.0.0:1" {
		t.Fatalf("socket 7 kept its addresses after destroy: %s", source)
	}
}

func TestCaptureEventsRunWritesEventsAfterSIGINT(t *testing.T) {
	previous := captureEventsCollect
	defer func() { captureEventsCollect = previous }()
	captureEventsCollect = func(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
		if stop == nil {
			t.Fatal("capture events run without a stop channel")
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stop:
		case <-time.After(5 * time.Second):
			t.Fatal("SIGINT did not close the stop channel")
		}
		events := []captureEvent{{Event: "connect"}, {Event: "close"}}
		return events, captureSummary{Event: "capture_summary", EventCount: uint64(len(events))}, nil
	}

	output := filepath.Join(t.TempDir(), "events.jsonl")
	if err := captureEventsRun(10*time.Minute, output); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		names = append(names, line.Event)
	}
	if got := len(names); got != 3 || names[0] != "connect" || names[1] != "close" || names[2] != "capture_summary" {
		t.Fatalf("output events = %q", names)
	}
}

func TestCaptureEventUsesWallClockTimestamp(t *testing.T) {
	raw := captureEventRaw{TimestampNS: 5 * uint64(time.Second), EventType: 1}
	offset := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC).UnixNano()
	event := raw.event(offset)
	if event.BootTimeNS != 5*uint64(time.Second) {
		t.Fatalf("boot_time_ns = %d", event.BootTimeNS)
	}
	if want := uint64(offset) + 5*uint64(time.Second); event.TimestampNS != want {
		t.Fatalf("timestamp_ns = %d, want %d", event.TimestampNS, want)
	}
}

func TestCaptureClockOffsetMatchesWallClock(t *testing.T) {
	offset, err := captureClockOffset()
	if err != nil {
		t.Fatal(err)
	}
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		t.Fatal(err)
	}
	wall := time.Unix(0, monotonic.Nano()+offset)
	if drift := time.Since(wall); drift < -time.Second || drift > time.Second {
		t.Fatalf("monotonic + offset = %s, drift %s", wall, drift)
	}
}

func TestCommandTargetCacheReusesLookupWithinTTL(t *testing.T) {
	lookups := map[uint32]int{}
	cache := newCommandTargetCache(func(pid uint32) string {
		lookups[pid]++
		return "example.com"
	})
	now := time.Now()
	for index := 0; index < 100; index++ {
		if got := cache.target(42, now.Add(time.Duration(index)*time.Millisecond)); got != "example.com" {
			t.Fatalf("target = %q", got)
		}
	}
	if lookups[42] != 1 {
		t.Fatalf("lookups within TTL = %d, want 1", lookups[42])
	}
	cache.target(42, now.Add(commandTargetTTL))
	if lookups[42] != 2 {
		t.Fatalf("lookups after TTL = %d, want 2", lookups[42])
	}
}

func ownerRecord(pid uint32, args string, size uint32) []byte {
	sample := make([]byte, ownerRecordArgsOffset+512)
	binary.LittleEndian.PutUint64(sample[0:8], 1)
	binary.LittleEndian.PutUint32(sample[8:12], ownerRecordType)
	binary.LittleEndian.PutUint32(sample[12:16], pid)
	binary.LittleEndian.PutUint32(sample[16:20], size)
	copy(sample[ownerRecordArgsOffset:], args)
	return sample
}

func TestParseOwnerAnnouncement(t *testing.T) {
	args := "curl\x00-s\x00https://example.com/\x00"
	owner, ok := parseOwnerAnnouncement(ownerRecord(42, args, uint32(len(args))))
	if !ok || owner.pid != 42 || !owner.readable || owner.target != "example.com" {
		t.Fatalf("owner = %#v, %t", owner, ok)
	}
	// size 뒤의 바이트는 ring buffer에 남은 이전 값일 수 있으므로 읽지 않는다.
	truncated, _ := parseOwnerAnnouncement(ownerRecord(42, args+"junk.example.org", uint32(len(args))))
	if truncated.target != "example.com" {
		t.Fatalf("target beyond size = %q", truncated.target)
	}
	unreadable, ok := parseOwnerAnnouncement(ownerRecord(42, args, 0))
	if !ok || unreadable.readable {
		t.Fatalf("unreadable record = %#v, %t", unreadable, ok)
	}
	event := make([]byte, 128)
	binary.LittleEndian.PutUint32(event[8:12], 6)
	if _, ok := parseOwnerAnnouncement(event); ok {
		t.Fatal("a send event was parsed as an owner announcement")
	}
}

func TestPIDTargetCacheStaysBounded(t *testing.T) {
	cache := newPIDTargetCache()
	for pid := uint32(1); pid <= pidTargetLimit; pid++ {
		cache.remember(pid, "example.com")
	}
	if target, ok := cache.target(pidTargetLimit); !ok || target != "example.com" {
		t.Fatalf("target = %q, %t", target, ok)
	}
	cache.remember(pidTargetLimit+1, "")
	if len(cache.entries) != 1 {
		t.Fatalf("entries after the limit = %d, want 1", len(cache.entries))
	}
	if target, ok := cache.target(pidTargetLimit + 1); !ok || target != "" {
		t.Fatalf("known empty target = %q, %t", target, ok)
	}
}

func dnsRecordSample(t *testing.T, pid uint32, message *dns.Msg) []byte {
	t.Helper()
	payload, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	sample := make([]byte, dnsRecordPayloadOffset+1024)
	binary.LittleEndian.PutUint32(sample[8:12], dnsRecordType)
	binary.LittleEndian.PutUint32(sample[12:16], pid)
	binary.LittleEndian.PutUint32(sample[40:44], uint32(len(payload)))
	copy(sample[dnsRecordPayloadOffset:], payload)
	return sample
}

func TestDNSAnswersNameTheAddressesThatAProcessResolved(t *testing.T) {
	question := new(dns.Msg)
	question.SetQuestion("API.Example.com.", dns.TypeA)
	answer := new(dns.Msg)
	answer.SetReply(question)
	for _, text := range []string{"api.example.com. 60 IN CNAME edge.cdn.example.net.", "edge.cdn.example.net. 60 IN A 203.0.113.10", "edge.cdn.example.net. 60 IN AAAA 2606:4700:10::6814:179a"} {
		record, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		answer.Answer = append(answer.Answer, record)
	}
	packet, ok := parseDNSRecord(dnsRecordSample(t, 42, answer))
	if !ok || packet.pid != 42 || packet.sent {
		t.Fatalf("parseDNSRecord = %#v, %t", packet, ok)
	}
	payload := packet.payload
	cache := newDNSNameCache()
	cache.rememberAnswer(packet.pid, dnsAnswerNames(payload))
	// CNAME을 거쳐도 프로그램이 물어본 이름을 쓰고, IPv6를 풀어 쓴 표기로 들어와도 같은 주소로 본다.
	for _, destination := range []string{"203.0.113.10:443", "[2606:4700:10:0:0:0:6814:179a]:443"} {
		if name, ok := cache.forProcess(42, destination); !ok || name != "api.example.com" {
			t.Fatalf("forProcess(%s) = %q, %t", destination, name, ok)
		}
	}
	if _, ok := cache.forProcess(7, "203.0.113.10:443"); ok {
		t.Fatal("another process must not get a process-level name")
	}
	if name, ok := cache.forAddress("203.0.113.10:443"); !ok || name.name != "api.example.com" || name.source != targetSourceDNS {
		t.Fatalf("forAddress = %#v, %t", name, ok)
	}
	if names := dnsAnswerNames(payload[:len(payload)-5]); names != nil {
		t.Fatalf("a cut answer must not be read: %v", names)
	}
	event := make([]byte, 128)
	binary.LittleEndian.PutUint32(event[8:12], 6)
	if _, ok := parseDNSRecord(event); ok {
		t.Fatal("a send event was parsed as a DNS record")
	}
}

// dnsRecordPayloadOffset과 parseDNSRecord의 offset은 BPF의 struct dns_record를 따른다. bpf2go가 만든 Go 구조체와
// 비교해 C 구조체를 바꾸고 이쪽을 고치지 않으면 여기서 실패한다.
func TestDNSRecordOffsetsMatchTheBPFStruct(t *testing.T) {
	var record captureEventsDnsRecord
	for name, offsets := range map[string][2]uintptr{
		"event_type":  {unsafe.Offsetof(record.EventType), 8},
		"pid":         {unsafe.Offsetof(record.Pid), 12},
		"cgroup_id":   {unsafe.Offsetof(record.CgroupId), 16},
		"arrival_ns":  {unsafe.Offsetof(record.ArrivalNs), 24},
		"skaddr":      {unsafe.Offsetof(record.Skaddr), 32},
		"len":         {unsafe.Offsetof(record.Len), 40},
		"family":      {unsafe.Offsetof(record.Family), 44},
		"direction":   {unsafe.Offsetof(record.Direction), 46},
		"transport":   {unsafe.Offsetof(record.Transport), 47},
		"sport":       {unsafe.Offsetof(record.Sport), 48},
		"dport":       {unsafe.Offsetof(record.Dport), 50},
		"source":      {unsafe.Offsetof(record.Source), 52},
		"destination": {unsafe.Offsetof(record.Destination), 68},
		"comm":        {unsafe.Offsetof(record.Comm), 84},
		"payload":     {unsafe.Offsetof(record.Payload), dnsRecordPayloadOffset},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseDNSRecord reads %d", name, offsets[0], offsets[1])
		}
	}
}

func TestParseDNSRecordReadsTheQuerySide(t *testing.T) {
	question := new(dns.Msg)
	question.SetQuestion("example.com.", dns.TypeAAAA)
	sample := dnsRecordSample(t, 7, question)
	binary.LittleEndian.PutUint64(sample[0:8], 1_000)
	binary.LittleEndian.PutUint64(sample[16:24], 99)
	binary.LittleEndian.PutUint64(sample[24:32], 900)
	binary.LittleEndian.PutUint64(sample[32:40], 0xabc)
	binary.LittleEndian.PutUint16(sample[44:46], 2)
	sample[46], sample[47] = dnsRecordSent, dnsRecordTCP
	binary.LittleEndian.PutUint16(sample[48:50], 41000)
	binary.LittleEndian.PutUint16(sample[50:52], 53)
	copy(sample[52:56], []byte{10, 0, 0, 2})
	copy(sample[68:72], []byte{127, 0, 0, 53})
	copy(sample[84:100], "dig")
	packet, ok := parseDNSRecord(sample)
	if !ok || !packet.sent || packet.bootTimeNS != 1_000 || packet.cgroupID != 99 || packet.arrivalNS != 900 || !packet.tcp || packet.socket != 0xabc || packet.process != "dig" || packet.source != "10.0.0.2:41000" || packet.destination != "127.0.0.53:53" {
		t.Fatalf("parseDNSRecord = %#v, %t", packet, ok)
	}
}

func TestResolverCacheNamesFollowCNAMEs(t *testing.T) {
	output := `Scope protocol=dns ifindex=212 ifname=tailscale0 DNSSEC=no DNSOverTLS=no
No entries.

Scope protocol=dns ifindex=3 ifname=eno2 DNSSEC=no DNSOverTLS=no
ctz.solidwallet.io IN A 104.18.27.64
api.anthropic.com IN AAAA 2607:6bc0::10
blob.example.windows.net IN CNAME blob.example.trafficmanager.net
blob.example.trafficmanager.net IN A 20.60.1.2
`
	names := resolverCacheNames(output)
	for address, want := range map[string]string{"104.18.27.64": "ctz.solidwallet.io", "2607:6bc0::10": "api.anthropic.com", "20.60.1.2": "blob.example.windows.net"} {
		if got := names[netip.MustParseAddr(address)]; got != want {
			t.Fatalf("names[%s] = %q, want %q", address, got, want)
		}
	}
}

func TestResolveTraceTargetOrder(t *testing.T) {
	cache := newDNSNameCache()
	cache.rememberAnswer(42, map[netip.Addr]string{netip.MustParseAddr("203.0.113.10"): "api.example.com"})
	cache.rememberAddress(netip.MustParseAddr("203.0.113.20"), "cached.example.com", targetSourceResolverCache)
	for _, test := range []struct {
		name           string
		event          captureEvent
		command        string
		target, source string
	}{
		{"own lookup beats the command line", captureEvent{PID: 42, Destination: "203.0.113.10:443"}, "app.py", "api.example.com", targetSourceDNS},
		{"command line beats another lookup", captureEvent{PID: 7, Destination: "203.0.113.10:443"}, "example.org", "example.org", targetSourceCommand},
		{"another lookup when the command has none", captureEvent{PID: 7, Destination: "203.0.113.10:443"}, "", "api.example.com", targetSourceDNS},
		{"resolver cache", captureEvent{PID: 7, Destination: "203.0.113.20:443"}, "", "cached.example.com", targetSourceResolverCache},
		{"nothing known", captureEvent{PID: 7, Destination: "203.0.113.30:443"}, "", "", ""},
		{"IP command target on its own address", captureEvent{PID: 7, Destination: "20.20.0.68:22"}, "20.20.0.68", "20.20.0.68", targetSourceCommand},
		{"IP command target elsewhere", captureEvent{PID: 7, Destination: "20.20.0.69:2380"}, "20.20.0.68", "", ""},
		{"user@IP command target on its own address", captureEvent{PID: 7, Destination: "[0:0:0:0:0:ffff:1414:44]:22"}, "root@20.20.0.68", "root@20.20.0.68", targetSourceCommand},
		{"user@IP command target elsewhere falls back to a DNS name", captureEvent{PID: 7, Destination: "203.0.113.20:443"}, "root@20.20.0.68", "cached.example.com", targetSourceResolverCache},
		{"host name command target stays process-wide", captureEvent{PID: 7, Destination: "127.0.0.1:8080"}, "example.org", "example.org", targetSourceCommand},
	} {
		target, source := resolveTraceTarget(test.event, test.command, cache)
		if target != test.target || source != test.source {
			t.Fatalf("%s: got %q/%q, want %q/%q", test.name, target, source, test.target, test.source)
		}
	}
}

func TestSocketTargetCacheKeepsTheTargetSource(t *testing.T) {
	cache := newSocketTargetCache()
	cache.target(captureEvent{Protocol: "tcp", SocketID: 9, Target: "api.example.com", TargetSource: targetSourceDNS})
	if target, source := cache.target(captureEvent{Protocol: "tcp", SocketID: 9, Event: "tcp_close"}); target != "api.example.com" || source != targetSourceDNS {
		t.Fatalf("carried target = %q/%q", target, source)
	}
}

func TestSocketTargetCacheKeepsTargetAfterProcessExit(t *testing.T) {
	cache := newSocketTargetCache()
	for _, step := range []struct {
		event captureEvent
		want  string
	}{
		{captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_connect", Target: "example.com"}, "example.com"},
		{captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_close"}, "example.com"},
		{captureEvent{Protocol: "tcp", SocketID: 8, Event: "tcp_close"}, ""},
		{captureEvent{Protocol: "udp", SocketID: 7, Event: "udp_send"}, ""},
		{captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_destroy"}, "example.com"},
		{captureEvent{Protocol: "tcp", SocketID: 7, Event: "tcp_connect"}, ""},
	} {
		if got, _ := cache.target(step.event); got != step.want {
			t.Fatalf("%s on socket %d = %q, want %q", step.event.Event, step.event.SocketID, got, step.want)
		}
	}
}

func TestSocketTargetCacheStaysBounded(t *testing.T) {
	cache := newSocketTargetCache()
	for socket := uint64(1); socket <= socketTargetLimit; socket++ {
		cache.target(captureEvent{Protocol: "tcp", SocketID: socket, Target: "example.com"})
	}
	cache.target(captureEvent{Protocol: "tcp", SocketID: socketTargetLimit + 1, Target: "example.com"})
	if len(cache.entries) != 1 {
		t.Fatalf("entries after the limit = %d, want 1", len(cache.entries))
	}
}

func TestCommandTargetCacheSweepsExpiredEntries(t *testing.T) {
	cache := newCommandTargetCache(func(uint32) string { return "" })
	now := time.Now()
	for pid := uint32(0); pid < commandTargetSweepSize; pid++ {
		cache.target(pid, now)
	}
	cache.target(commandTargetSweepSize, now.Add(commandTargetTTL))
	if len(cache.entries) != 1 {
		t.Fatalf("entries after sweep = %d, want 1", len(cache.entries))
	}
}

func neighborNetlinkMessage(messageType uint16, family byte, state uint16, attributes ...[]byte) syscall.NetlinkMessage {
	data := make([]byte, unix.SizeofNdMsg)
	data[0] = family
	binary.NativeEndian.PutUint32(data[4:8], 1)
	binary.NativeEndian.PutUint16(data[8:10], state)
	for _, attribute := range attributes {
		data = append(data, attribute...)
	}
	return syscall.NetlinkMessage{Header: syscall.NlMsghdr{Type: messageType}, Data: data}
}

func neighborNetlinkAttribute(kind uint16, value []byte) []byte {
	attribute := make([]byte, unix.SizeofRtAttr, (unix.SizeofRtAttr+len(value)+3)&^3)
	binary.NativeEndian.PutUint16(attribute[0:2], uint16(unix.SizeofRtAttr+len(value)))
	binary.NativeEndian.PutUint16(attribute[2:4], kind)
	attribute = append(attribute, value...)
	return attribute[:cap(attribute)]
}

func TestParseARPNeighborReadsNetlinkMessages(t *testing.T) {
	names := neighborInterfaceNames{1: "eth0"}
	destination := neighborNetlinkAttribute(unix.NDA_DST, []byte{192, 0, 2, 1})
	address := neighborNetlinkAttribute(unix.NDA_LLADDR, []byte{2, 0, 0, 0, 0, 1})
	neighbor, ok := parseNeighborMessage(neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET, unix.NUD_REACHABLE, destination, address), unix.AF_INET, names)
	if !ok || neighbor != (traceNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "REACHABLE"}) {
		t.Fatalf("new neighbor = %#v, %t", neighbor, ok)
	}
	if neighbor, ok := parseNeighborMessage(neighborNetlinkMessage(unix.RTM_DELNEIGH, unix.AF_INET, unix.NUD_FAILED, destination), unix.AF_INET, names); !ok || !neighbor.deleted || neighbor.state != "FAILED" || neighbor.mac != "" {
		t.Fatalf("deleted neighbor = %#v, %t", neighbor, ok)
	}
	for name, message := range map[string]syscall.NetlinkMessage{
		"noarp":      neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET, unix.NUD_NOARP, destination),
		"ipv6":       neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET6, unix.NUD_REACHABLE, neighborNetlinkAttribute(unix.NDA_DST, make([]byte, 16))),
		"no address": neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET, unix.NUD_REACHABLE, address),
		"route":      {Header: syscall.NlMsghdr{Type: unix.RTM_NEWROUTE}, Data: make([]byte, 64)},
	} {
		if neighbor, ok := parseNeighborMessage(message, unix.AF_INET, names); ok {
			t.Fatalf("%s message made a neighbor: %#v", name, neighbor)
		}
	}
	// NDP는 같은 알림에서 IPv6 항목만 읽는다.
	global := make([]byte, 16)
	copy(global, []byte{0x20, 0x01, 0x0d, 0xb8})
	global[15] = 1
	if neighbor, ok := parseNeighborMessage(neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET6, unix.NUD_STALE, neighborNetlinkAttribute(unix.NDA_DST, global), address), unix.AF_INET6, names); !ok || neighbor.ip != "2001:db8::1" || neighbor.state != "STALE" {
		t.Fatalf("IPv6 neighbor = %#v, %t", neighbor, ok)
	}
	if _, ok := parseNeighborMessage(neighborNetlinkMessage(unix.RTM_NEWNEIGH, unix.AF_INET, unix.NUD_REACHABLE, destination, address), unix.AF_INET6, names); ok {
		t.Fatal("an IPv4 neighbor was read for NDP")
	}
	for state, want := range map[uint16]string{unix.NUD_STALE: "STALE", unix.NUD_INCOMPLETE: "INCOMPLETE", unix.NUD_PERMANENT: "PERMANENT", 0: "NONE"} {
		if got := traceNeighborStateName(state); got != want {
			t.Fatalf("traceNeighborStateName(%#x) = %q, want %q", state, got, want)
		}
	}
}

// root는 SO_RCVBUFFORCE로, root가 아닌 CI는 EPERM 뒤의 SO_RCVBUF로 기본값보다 큰 buffer를 받는다.
func TestParseListeningTCPPortsReadsProcNetTCP(t *testing.T) {
	ports := map[int]bool{}
	// 127.0.0.1:10259는 LISTEN(0A)이고, 20.20.0.50:6443 ↔ 20.20.0.69:1956은 연결된(01) socket이다.
	parseListeningTCPPorts([]byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100007F:2813 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n"+
		"   1: 3200140A:192B 4500140A:07A4 01 00000000:00000000 02:000A7B2B 00000000     0        0 2 2 0000000000000000 20 4 30 10 -1\n"), ports)
	parseListeningTCPPorts([]byte("  sl  local_address                         remote_address                        st tx_queue rx_queue\n"+
		"   0: 00000000000000000000000000000000:192B 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1\n"), ports)
	parseListeningTCPPorts(nil, ports)
	if want := map[int]bool{10259: true, 6443: true}; !maps.Equal(ports, want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
}

func TestGrowNeighborReceiveBufferExceedsTheDefault(t *testing.T) {
	open := func() int {
		fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatalf("open netlink socket: %v", err)
		}
		t.Cleanup(func() { unix.Close(fd) })
		return fd
	}
	initial, err := unix.GetsockoptInt(open(), unix.SOL_SOCKET, unix.SO_RCVBUF)
	if err != nil {
		t.Fatalf("read default receive buffer: %v", err)
	}
	fd := open()
	if err := growNeighborReceiveBuffer(fd); err != nil {
		t.Fatalf("growNeighborReceiveBuffer: %v", err)
	}
	if grown, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF); err != nil || grown <= initial {
		t.Fatalf("receive buffer = %d, %v, want more than the default %d", grown, err, initial)
	}
}

func TestHTTPRecordOffsetsMatchTheBPFStruct(t *testing.T) {
	var record captureEventsHttpRecord
	for name, offsets := range map[string][2]uintptr{
		"event_type":  {unsafe.Offsetof(record.EventType), 8},
		"pid":         {unsafe.Offsetof(record.Pid), 12},
		"cgroup_id":   {unsafe.Offsetof(record.CgroupId), 16},
		"skaddr":      {unsafe.Offsetof(record.Skaddr), 24},
		"len":         {unsafe.Offsetof(record.Len), 32},
		"family":      {unsafe.Offsetof(record.Family), 36},
		"direction":   {unsafe.Offsetof(record.Direction), 38},
		"sport":       {unsafe.Offsetof(record.Sport), 40},
		"dport":       {unsafe.Offsetof(record.Dport), 42},
		"source":      {unsafe.Offsetof(record.Source), 44},
		"destination": {unsafe.Offsetof(record.Destination), 60},
		"comm":        {unsafe.Offsetof(record.Comm), 76},
		"payload":     {unsafe.Offsetof(record.Payload), httpRecordPayloadOffset},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseHTTPRecord reads %d", name, offsets[0], offsets[1])
		}
	}
	if len(record.Payload) != httpRecordPayloadMax {
		t.Fatalf("BPF HTTP_PAYLOAD_SIZE is %d, --payload asks for %d", len(record.Payload), httpRecordPayloadMax)
	}
	sample := make([]byte, httpRecordPayloadOffset+16)
	binary.LittleEndian.PutUint32(sample[8:12], httpRecordType)
	binary.LittleEndian.PutUint64(sample[24:32], 0xabc)
	binary.LittleEndian.PutUint32(sample[32:36], 4)
	binary.LittleEndian.PutUint16(sample[36:38], 2)
	sample[38] = httpRecordSent
	binary.LittleEndian.PutUint16(sample[40:42], 40000)
	binary.LittleEndian.PutUint16(sample[42:44], 80)
	copy(sample[44:48], []byte{10, 0, 0, 2})
	copy(sample[60:64], []byte{192, 0, 2, 1})
	copy(sample[httpRecordPayloadOffset:], "GET /")
	packet, ok := parseHTTPRecord(sample)
	if !ok || !packet.sent || packet.socket != 0xabc || packet.source != "10.0.0.2:40000" || packet.destination != "192.0.2.1:80" || string(packet.payload) != "GET " {
		t.Fatalf("parseHTTPRecord = %#v, %t", packet, ok)
	}
}

// ubuf와 __iov는 iov_iter 안의 이름 없는 union에 있다. 이 kernel BTF에서 찾을 수 있어야 trace http가 동작한다.
func TestHTTPKernelCheckFindsNestedIOVIterFields(t *testing.T) {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		t.Skipf("no kernel BTF: %v", err)
	}
	var iter *btf.Struct
	if err := spec.TypeByName("iov_iter", &iter); err != nil {
		t.Skipf("no iov_iter: %v", err)
	}
	if !btfHasMember(iter, "iov_offset") || btfHasMember(iter, "no_such_field") {
		t.Fatal("btfHasMember does not follow the struct members")
	}
	if !btfHasMember(iter, "ubuf") && !btfHasMember(iter, "iov") {
		t.Fatal("btfHasMember does not look into anonymous unions")
	}
}
