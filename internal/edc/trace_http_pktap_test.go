package edc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func pktapTestTCP(sent bool, sequence uint32, payload string) pktapTCPPacket {
	return pktapTCPPacket{httpPacket: httpPacket{bootTimeNS: 1_000_000, pid: 42, process: "fixture", source: "127.0.0.1:50000", destination: "127.0.0.1:8080", sent: sent, payload: []byte(payload)}, sequence: sequence}
}
func TestPKTAPHTTPSplitPipelineAndRetransmission(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	now := time.Now()
	first := "GET /one?secret=yes HTTP/1.1\r\nHost: example.com\r\nAuthorization: secret\r\n\r\n"
	second := "GET /two HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if got := tracker.add(pktapTestTCP(true, 100, first[:12]), now); len(got) != 0 {
		t.Fatal(got)
	}
	if got := tracker.add(pktapTestTCP(true, 100+uint32(len(first)), second), now); len(got) != 0 {
		t.Fatal(got)
	}
	got := tracker.add(pktapTestTCP(true, 112, first[12:]), now)
	if len(got) != 2 || got[0].Path != "/one" || got[1].Path != "/two" || bytes.Contains([]byte(got[0].Payload), []byte("Authorization: secret")) {
		t.Fatalf("requests: %#v", got)
	}
	if got := tracker.add(pktapTestTCP(true, 100, first+second), now); len(got) != 0 {
		t.Fatalf("retransmission: %#v", got)
	}
	reply := pktapTestTCP(false, 900, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nokHTTP/1.1 204 No Content\r\n\r\n")
	reply.bootTimeNS = 4_000_000
	got = tracker.add(reply, now)
	if len(got) != 2 || got[0].Path != "/one" || got[1].Path != "/two" || got[0].LatencyMS == nil || *got[0].LatencyMS != 3 {
		t.Fatalf("responses: %#v", got)
	}
}
func TestPKTAPHTTP2BothDirections(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	request := append(bytes.Clone(http2Preface), http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/h2"}))...)
	now := time.Now()
	got := tracker.add(pktapTestTCP(true, 100, string(request)), now)
	if len(got) != 1 || got[0].Path != "/h2" {
		t.Fatalf("request: %#v", got)
	}
	response := http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "204"}))
	got = tracker.add(pktapTestTCP(false, 800, string(response)), now)
	if len(got) != 1 || got[0].Status != 204 || got[0].Path != "/h2" {
		t.Fatalf("response: %#v", got)
	}
}
func TestPKTAPHTTPCloseAndHead(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	now := time.Now()
	tracker.add(pktapTestTCP(true, 10, "HEAD /head HTTP/1.1\r\nHost: example.com\r\n\r\n"), now)
	got := tracker.add(pktapTestTCP(false, 20, "HTTP/1.1 200 OK\r\nContent-Length: 900\r\n\r\n"), now)
	if len(got) != 1 || got[0].Path != "/head" {
		t.Fatalf("HEAD response: %#v", got)
	}
	tracker = newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	tracker.add(pktapTestTCP(true, 10, "GET /close HTTP/1.0\r\n\r\n"), now)
	packet := pktapTestTCP(false, 20, "HTTP/1.0 200 OK\r\n\r\nbody")
	packet.flags = 1
	got = tracker.add(packet, now)
	if len(got) != 1 || !bytes.Contains([]byte(got[0].Payload), []byte("body")) {
		t.Fatalf("close response: %#v", got)
	}
}
func pktapTestRecord(v2, ipv6, sent bool) []byte {
	length := 156
	if v2 {
		length = 64
	}
	ipSize := 20
	family := uint32(2)
	if ipv6 {
		ipSize = 40
		family = 30
	}
	payload := []byte("GET / HTTP/1.1\r\n\r\n")
	data := make([]byte, length+4+ipSize+20+len(payload))
	flags := uint32(1)
	if sent {
		flags = 2
	}
	if v2 {
		data[0] = byte(length)
		data[4] = 40
		binary.LittleEndian.PutUint16(data[8:], 4)
		binary.LittleEndian.PutUint32(data[16:], family)
		binary.LittleEndian.PutUint32(data[28:], 42)
		flags |= 0x80000
		copy(data[40:], "fixture\x00")
	} else {
		binary.LittleEndian.PutUint32(data, uint32(length))
		binary.LittleEndian.PutUint32(data[4:], 1)
		binary.LittleEndian.PutUint32(data[40:], family)
		binary.LittleEndian.PutUint32(data[44:], 4)
		binary.LittleEndian.PutUint32(data[52:], 42)
		copy(data[56:], "fixture\x00")
	}
	binary.LittleEndian.PutUint32(data[36:], flags)
	ip := data[length+4:]
	if ipv6 {
		ip[0], ip[6] = 0x60, 6
		binary.BigEndian.PutUint16(ip[4:], uint16(20+len(payload)))
		ip[23] = 1
		ip[39] = 2
	} else {
		ip[0], ip[9] = 0x45, 6
		binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
		copy(ip[12:], []byte{127, 0, 0, 1})
		copy(ip[16:], []byte{127, 0, 0, 2})
	}
	tcp := ip[ipSize:]
	binary.BigEndian.PutUint16(tcp, 50000)
	binary.BigEndian.PutUint16(tcp[2:], 8080)
	binary.BigEndian.PutUint32(tcp[4:], 123)
	tcp[12] = 0x50
	copy(tcp[20:], payload)
	return data
}
func TestDecodeHTTPPKTAP(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		for _, ipv6 := range []bool{false, true} {
			for _, sent := range []bool{false, true} {
				record := pktapTestRecord(v2, ipv6, sent)
				packet, ok := decodeHTTPPKTAP(record, 2_000_000, 1_000_000)
				if !ok || packet.pid != 42 || packet.process != "fixture" || packet.sent != sent || packet.sequence != 123 || packet.bootTimeNS != 1_000_000 || !pktapHTTPPortMatches(packet, 8080) {
					t.Fatalf("v2=%t ipv6=%t sent=%t: %#v %t", v2, ipv6, sent, packet, ok)
				}
				for n := 0; n < len(record); n++ {
					if _, ok := decodeHTTPPKTAP(record[:n], 0, 0); ok {
						t.Fatalf("accepted truncated record %d/%d", n, len(record))
					}
				}
			}
		}
	}
}
func TestPKTAPHTTPRegistry(t *testing.T) {
	if traceProtocols["http"].linuxOnly {
		t.Fatal("HTTP trace is unavailable on macOS")
	}
}

func TestPKTAPHTTPBoundsAndCleanup(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	request := "POST /large HTTP/1.1\r\nContent-Length: 2000000\r\n\r\n"
	tracker.add(pktapTestTCP(true, 10, request), now)
	tracker.add(pktapTestTCP(true, 10+uint32(len(request)), string(bytes.Repeat([]byte{'x'}, httpMessageMax))), now)
	if tracker.lost != 1 || tracker.buffered != 0 {
		t.Fatalf("overflow: lost=%d buffered=%d", tracker.lost, tracker.buffered)
	}
	tracker.sweep(now, true)
	if tracker.buffered != 0 || len(tracker.flows) != 0 {
		t.Fatalf("cleanup: %#v", tracker)
	}
}
func TestPKTAPHTTPServerAndSequenceWrap(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{side: traceServerSide}, 0)
	now := time.Now()
	request := "GET /server HTTP/1.1\r\nHost: example.com\r\n\r\n"
	tracker.add(pktapTestTCP(false, ^uint32(0)-3, request[:4]), now)
	got := tracker.add(pktapTestTCP(false, 0, request[4:]), now)
	if len(got) != 1 || got[0].Side != traceServerSide {
		t.Fatalf("server request: %#v", got)
	}
	got = tracker.add(pktapTestTCP(true, 80, "HTTP/1.1 204 No Content\r\n\r\n"), now)
	if len(got) != 1 || got[0].Path != "/server" || got[0].Side != traceServerSide {
		t.Fatalf("server response: %#v", got)
	}
	tracker.sweep(now, true)
	if tracker.buffered != 0 {
		t.Fatalf("buffered=%d", tracker.buffered)
	}
}
func FuzzDecodeHTTPPKTAP(f *testing.F) {
	f.Add(pktapTestRecord(false, false, true))
	f.Add(pktapTestRecord(true, true, false))
	f.Fuzz(func(t *testing.T, data []byte) { decodeHTTPPKTAP(data, 100, 0) })
}

func TestPKTAPHTTPQueueOverflowReleasesMemory(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	tracker.add(pktapTestTCP(true, 0, "GET "), now)
	for i := 0; i < 65; i++ {
		tracker.add(pktapTestTCP(true, uint32(100+i), "x"), now)
	}
	if tracker.lost != 1 || tracker.buffered != 0 {
		t.Fatalf("overflow: lost=%d buffered=%d", tracker.lost, tracker.buffered)
	}
	packet := pktapTestTCP(true, 999, "")
	packet.flags = 4
	tracker.add(packet, now)
	if len(tracker.flows) != 0 || tracker.buffered != 0 {
		t.Fatalf("reset left state: %#v", tracker)
	}
}
func TestPKTAPHTTPTLSFragments(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	hello := tlsTestClientHello(t, "example.com", "h2")
	now := time.Now()
	tracker.add(pktapTestTCP(true, 100, string(hello[:2])), now)
	got := tracker.add(pktapTestTCP(true, 102, string(hello[2:])), now)
	if len(got) != 1 || got[0].Event != traceTLSHelloEvent || got[0].Target != "example.com" {
		t.Fatalf("TLS hello: %#v", got)
	}
}

func TestPKTAPHTTPContiguousOverflowReleasesWaitingMemory(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	tracker.add(pktapTestTCP(true, 0, "G"), now)
	tracker.add(pktapTestTCP(true, 100, "waiting"), now)
	tracker.add(pktapTestTCP(true, 1, strings.Repeat("x", httpMessageMax)), now)
	if tracker.buffered != 0 || tracker.lost != 1 {
		t.Fatalf("buffered=%d lost=%d", tracker.buffered, tracker.lost)
	}
	for _, flow := range tracker.flows {
		for _, direction := range flow.directions {
			if direction.waitingBytes != 0 || len(direction.waiting) != 0 {
				t.Fatal("blocked flow retained waiting packets")
			}
		}
	}
}

func TestPKTAPHTTPConnectAndTraceRequests(t *testing.T) {
	for _, method := range []string{"CONNECT", "TRACE"} {
		t.Run(method, func(t *testing.T) {
			tracker := newPKTAPHTTPTracker(traceScope{}, 0)
			now := time.Now()
			target := "/trace"
			if method == "CONNECT" {
				target = "example.com:443"
			}
			request := method + " " + target + " HTTP/1.1\r\nHost: example.com\r\n\r\n"
			var events []captureEvent
			for i := range request {
				events = append(events, tracker.add(pktapTestTCP(true, uint32(i), request[i:i+1]), now)...)
			}
			if len(events) != 1 || events[0].Method != method || events[0].Path != target {
				t.Fatalf("requests=%#v", events)
			}
			events = tracker.add(pktapTestTCP(false, 0, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"), now)
			if len(events) != 1 || events[0].Method != method || events[0].Status != 403 {
				t.Fatalf("response=%#v", events)
			}
		})
	}
}

func TestPKTAPHTTP2FragmentedPrefaceAndFrames(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	request := append(bytes.Clone(http2Preface), http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/split"}))...)
	response := http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "204"}))
	for _, wire := range []struct {
		sent bool
		data []byte
	}{{true, request}, {false, response}} {
		var events []captureEvent
		for i := range wire.data {
			events = append(events, tracker.add(pktapTestTCP(wire.sent, uint32(i), string(wire.data[i:i+1])), now)...)
		}
		if len(events) != 1 || events[0].Path != "/split" || (!wire.sent && events[0].Status != 204) {
			t.Fatalf("sent=%t events=%#v", wire.sent, events)
		}
	}
	tracker.sweep(now, true)
	if tracker.buffered != 0 || len(tracker.flows) != 0 {
		t.Fatalf("buffered=%d flows=%d", tracker.buffered, len(tracker.flows))
	}
}
func TestPKTAPHTTPFlowLimitEvictsAndReleasesQueuedSegments(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	for i := 0; i < pktapHTTPFlowLimit; i++ {
		packet := pktapTestTCP(true, 0, "G")
		packet.pid = uint32(i + 1)
		tracker.add(packet, now.Add(time.Duration(i)))
		packet.sequence, packet.payload = 100, []byte("waiting")
		tracker.add(packet, now.Add(time.Duration(i)))
	}
	packet := pktapTestTCP(true, 0, "G")
	packet.pid = uint32(pktapHTTPFlowLimit + 1)
	tracker.add(packet, now.Add(time.Second))
	for key := range tracker.flows {
		if key.pid == 1 {
			t.Fatal("oldest flow survived eviction")
		}
	}
	if len(tracker.flows) != pktapHTTPFlowLimit || tracker.buffered != (pktapHTTPFlowLimit-1)*8+1 || tracker.lost == 0 {
		t.Fatalf("flows=%d buffered=%d lost=%d", len(tracker.flows), tracker.buffered, tracker.lost)
	}
	tracker.sweep(now.Add(time.Second), true)
	if tracker.buffered != 0 || len(tracker.flows) != 0 {
		t.Fatalf("cleanup buffered=%d flows=%d", tracker.buffered, len(tracker.flows))
	}
}
func TestPKTAPHTTPConnectSuccessEndsAtHeaders(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	tracker.add(pktapTestTCP(true, 0, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com\r\n\r\n"), now)
	events := tracker.add(pktapTestTCP(false, 0, "HTTP/1.1 200 Connection Established\r\n\r\n"), now)
	if len(events) != 1 || events[0].Method != "CONNECT" || events[0].Status != 200 {
		t.Fatalf("response=%#v", events)
	}
}
