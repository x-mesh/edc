package edc

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestParseObserveInterval(t *testing.T) {
	for _, input := range []string{"0.1", "100ms", "1"} {
		interval, err := parseObserveInterval(input)
		if err != nil || interval < 100*time.Millisecond {
			t.Fatalf("interval %q = %s, %v", input, interval, err)
		}
	}
	if interval, _ := parseObserveInterval("0.1"); interval != 100*time.Millisecond {
		t.Fatalf("0.1 seconds = %s", interval)
	}
	for _, input := range []string{"0.05", "50ms", "nope", "NaN"} {
		if _, err := parseObserveInterval(input); err == nil {
			t.Errorf("accepted interval %q", input)
		}
	}
}

func TestWatchSampleIncludesStatusAndBodySize(t *testing.T) {
	result := Result{Status: StatusPass, DurationMS: 12, Metrics: map[string]interface{}{"status_code": 204, "bytes_read": int64(1234), "peer_ip": "192.0.2.10", "dns_ms": int64(2), "resolved_host": "example.test", "resolved_addresses": []string{"192.0.2.10"}}}
	sample := watchSampleOf(result)
	line := formatWatchSample(sample)
	if sample.HTTPStatus != 204 || sample.BodyBytes != 1234 || sample.DNSMS == nil || *sample.DNSMS != 2 || len(sample.DNSAddresses) != 1 || !strings.Contains(line, "HTTP 204") || !strings.Contains(line, "1234 bytes") || !strings.Contains(line, "DNS 2ms") {
		t.Fatalf("sample = %#v, line = %q", sample, line)
	}
}

func TestStreamWatchStopsOnDuration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Millisecond)
	defer cancel()
	var output strings.Builder
	code := streamWatch(ctx, &output, 100*time.Millisecond, time.Second, false, false, func(context.Context) Result {
		return Result{Status: StatusPass, Metrics: map[string]interface{}{"status_code": 200, "bytes_read": int64(42)}}
	})
	if code != 0 || !strings.Contains(output.String(), "HTTP 200") || !strings.Contains(output.String(), "42 bytes") || !strings.Contains(output.String(), "samples") {
		t.Fatalf("watch exit = %d, output = %q", code, output.String())
	}
}

func TestStreamWatchJSONRedactsPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var output strings.Builder
	code := streamWatch(ctx, &output, 100*time.Millisecond, time.Second, true, true, func(context.Context) Result {
		return Result{Status: StatusPass, DurationMS: 12, Metrics: map[string]interface{}{"status_code": 200, "bytes_read": int64(42), "peer_ip": "192.0.2.10"}}
	})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if code != 0 || len(lines) < 2 || strings.Contains(output.String(), "192.0.2.10") {
		t.Fatalf("JSON watch exit = %d, output = %q", code, output.String())
	}
	var sample watchSample
	if err := json.Unmarshal([]byte(lines[0]), &sample); err != nil || sample.Type != "sample" || !strings.Contains(sample.PeerIP, "<ip:") {
		t.Fatalf("JSON sample = %#v, error = %v", sample, err)
	}
	var summary watchSummary
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil || summary.MinMS != 12 || summary.AvgMS != 12 || summary.P95MS != 12 || summary.MaxMS != 12 {
		t.Fatalf("JSON summary = %#v, error = %v", summary, err)
	}
}

func TestStreamWatchFailureAffectsExitAndDuration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var output strings.Builder
	code := streamWatch(ctx, &output, 100*time.Millisecond, time.Second, true, false, func(context.Context) Result {
		return Result{Status: StatusFail, DurationMS: 12, Metrics: map[string]interface{}{"status_code": 503}, Error: &DiagnosticError{Kind: "status", Message: "HTTP 503"}}
	})
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var summary watchSummary
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &summary); err != nil || code != 1 || summary.Fail == 0 || summary.LongestFailureMS == 0 {
		t.Fatalf("exit = %d, summary = %#v, error = %v", code, summary, err)
	}
}

func TestDirectDNSRowReadsRecordsAndTTL(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(writer dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(request)
		if request.Question[0].Qtype == dns.TypeA {
			record, _ := dns.NewRR("example.test. 60 IN A 192.0.2.10")
			response.Answer = append(response.Answer, record)
		}
		_ = writer.WriteMsg(response)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	row := directDNSRow(context.Background(), "example.test", packet.LocalAddr().String())
	if row.Status != "ok" || len(row.IPv4) != 1 || row.IPv4[0] != "192.0.2.10" || row.TTLSeconds == nil || *row.TTLSeconds != 60 {
		t.Fatalf("DNS row = %#v", row)
	}
	other := row
	other.TTLSeconds = nil
	if dnsRowSignature(row) != dnsRowSignature(other) {
		t.Fatal("TTL aging must not count as a different answer")
	}
}

func TestResolverFlagsRequireIPAddress(t *testing.T) {
	var resolvers resolverFlags
	if err := resolvers.Set("127.0.0.1:5353"); err != nil || len(resolvers) != 1 {
		t.Fatalf("resolvers = %#v, error = %v", resolvers, err)
	}
	if err := resolvers.Set("resolver.example"); err == nil {
		t.Fatal("hostname resolver was accepted")
	}
}

func TestListenSocketDiffIncludesProcessRestart(t *testing.T) {
	before := []listenSocket{{Proto: "tcp", Address: "127.0.0.1:8080", Port: 8080, Process: "app", PID: "1"}}
	after := []listenSocket{{Proto: "tcp", Address: "127.0.0.1:8080", Port: 8080, Process: "app", PID: "2"}, {Proto: "tcp", Address: "127.0.0.1:9090", Port: 9090, Process: "other", PID: "3"}}
	opened, closed := diffListenSockets(before, after)
	if len(opened) != 2 || len(closed) != 1 || closed[0].PID != "1" {
		t.Fatalf("opened = %#v, closed = %#v", opened, closed)
	}
}

func TestListenWatchJSONRedactsSocket(t *testing.T) {
	var output strings.Builder
	event := listenWatchEvent{Type: "open", Socket: listenSocket{Proto: "tcp", Address: "192.0.2.10:8080", Port: 8080}}
	if err := writeListenWatchJSON(&output, event, true); err != nil || strings.Contains(output.String(), "192.0.2.10") {
		t.Fatalf("JSON event = %q, error = %v", output.String(), err)
	}
}

func TestListenWatchHighlightsOnlyTerminalChangeText(t *testing.T) {
	event := listenWatchEvent{Type: "open", Time: time.Date(2026, time.September, 24, 0, 0, 0, 0, time.UTC), Socket: listenSocket{Proto: "tcp", Address: "192.0.2.10:8080", Process: "app", PID: "42"}}
	highlighted := formatListenWatchEvent(event, true, true)
	if !strings.HasPrefix(highlighted, liveReverse) || !strings.HasSuffix(highlighted, liveReset) || strings.Contains(highlighted, "192.0.2.10") || !strings.Contains(highlighted, "<ip:") {
		t.Fatalf("highlighted event = %q", highlighted)
	}
	plain := formatListenWatchEvent(event, false, false)
	if strings.Contains(plain, liveReverse) || strings.Contains(plain, liveReset) || !strings.Contains(plain, "192.0.2.10") {
		t.Fatalf("plain event = %q", plain)
	}
}
