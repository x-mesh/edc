//go:build linux

package edc

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
	"unsafe"

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
		{"", tcpTracepoints, append(append(append([]string{}, udpSend...), "fentry/skb_consume_udp"), tcpAccept...)},
		// TCP도 DNS 응답으로 target 이름을 지으므로 skb_consume_udp를 붙인다.
		{"tcp", tcpTracepoints, append([]string{"fentry/skb_consume_udp"}, tcpAccept...)},
		{"udp", []string{}, append(append([]string{}, udpSend...), "fentry/skb_consume_udp")},
		// DNS 질의는 UDP 송신 hook이, 응답은 skb_consume_udp가 읽는다.
		{"dns", []string{}, append(append([]string{}, udpSend...), "fentry/skb_consume_udp")},
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
	binary.LittleEndian.PutUint32(sample[24:28], uint32(len(payload)))
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
	if !ok || packet.pid != 42 || packet.query {
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
		"len":         {unsafe.Offsetof(record.Len), 24},
		"family":      {unsafe.Offsetof(record.Family), 28},
		"direction":   {unsafe.Offsetof(record.Direction), 30},
		"sport":       {unsafe.Offsetof(record.Sport), 32},
		"dport":       {unsafe.Offsetof(record.Dport), 34},
		"source":      {unsafe.Offsetof(record.Source), 36},
		"destination": {unsafe.Offsetof(record.Destination), 52},
		"comm":        {unsafe.Offsetof(record.Comm), 68},
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
	binary.LittleEndian.PutUint16(sample[28:30], 2)
	sample[30] = dnsRecordQuery
	binary.LittleEndian.PutUint16(sample[32:34], 41000)
	binary.LittleEndian.PutUint16(sample[34:36], 53)
	copy(sample[36:40], []byte{10, 0, 0, 2})
	copy(sample[52:56], []byte{127, 0, 0, 53})
	copy(sample[68:84], "dig")
	packet, ok := parseDNSRecord(sample)
	if !ok || !packet.query || packet.bootTimeNS != 1_000 || packet.cgroupID != 99 || packet.process != "dig" || packet.source != "10.0.0.2:41000" || packet.destination != "127.0.0.53:53" {
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
