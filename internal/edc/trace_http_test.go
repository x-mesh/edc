package edc

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/net/http2/hpack"
)

func http2TestFrame(typeID, flags byte, stream uint32, payload []byte) []byte {
	frame := make([]byte, 9, 9+len(payload))
	frame[0], frame[1], frame[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	frame[3], frame[4] = typeID, flags
	binary.BigEndian.PutUint32(frame[5:9], stream)
	return append(frame, payload...)
}

// http2TestPacket은 h2c 연결에서 첫 레코드 뒤에 오는 레코드다. BPF는 h2c 연결 안에서 이어지는 레코드만 내고, offset에
// 그 방향의 위치를 싣는다.
func http2TestPacket(payload []byte, at uint64, sent bool, offset int) httpPacket {
	packet := httpTestPacket(string(payload), at, sent)
	packet.offset, packet.continued = uint32(offset), true
	return packet
}

func http2TestHeaders(fields ...hpack.HeaderField) []byte {
	var data bytes.Buffer
	encoder := hpack.NewEncoder(&data)
	for _, field := range fields {
		_ = encoder.WriteField(field)
	}
	return data.Bytes()
}

func TestHTTP2TracePairsMultiplexedStreams(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	request1 := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":scheme", Value: "http"}, hpack.HeaderField{Name: ":authority", Value: "api.example"}, hpack.HeaderField{Name: ":path", Value: "/one?q=secret"})
	request3 := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "POST"}, hpack.HeaderField{Name: ":scheme", Value: "http"}, hpack.HeaderField{Name: ":authority", Value: "api.example"}, hpack.HeaderField{Name: ":path", Value: "/three"})
	preface := append(append(append([]byte{}, http2Preface...), http2TestFrame(4, 0, 0, nil)...), http2TestFrame(1, 4, 1, request1)...)
	events, claimed := tracker.http2Events(httpTestPacket(string(preface), 1_000_000, true), 0)
	if !claimed || len(events) != 1 || events[0].Method != "GET" || events[0].Path != "/one" {
		t.Fatalf("first request = %#v, %t", events, claimed)
	}
	events, claimed = tracker.http2Events(http2TestPacket(http2TestFrame(1, 4, 3, request3), 2_000_000, true, len(preface)), 0)
	if !claimed || len(events) != 1 || events[0].Method != "POST" {
		t.Fatalf("second request = %#v, %t", events, claimed)
	}
	response := http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "204"})
	events, claimed = tracker.http2Events(http2TestPacket(http2TestFrame(1, 4, 3, response), 5_000_000, false, 0), 0)
	if !claimed || len(events) != 1 || events[0].Status != 204 || events[0].Path != "/three" || events[0].LatencyMS == nil || *events[0].LatencyMS != 3 {
		t.Fatalf("response = %#v, %t", events, claimed)
	}
}

func TestHTTP2TraceJoinsSplitFramesAndContinuation(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	block := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/split"})
	frames := append(http2TestFrame(1, 0, 1, block[:1]), http2TestFrame(9, 4, 1, block[1:])...)
	first := append(append([]byte{}, http2Preface...), frames[:12]...)
	if events, claimed := tracker.http2Events(httpTestPacket(string(first), 1, true), 0); !claimed || len(events) != 0 {
		t.Fatalf("first piece = %#v, %t", events, claimed)
	}
	if events, claimed := tracker.http2Events(http2TestPacket(frames[12:], 2, true, len(first)), 0); !claimed || len(events) != 1 || events[0].Path != "/split" {
		t.Fatalf("second piece = %#v, %t", events, claimed)
	}
}

func TestHTTP2TraceJoinsSplitPreface(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	if events, claimed := tracker.http2Events(httpTestPacket("PRI * HTTP/2.0\r\n", 1, true), 0); !claimed || len(events) != 0 {
		t.Fatalf("preface prefix = %#v, %t", events, claimed)
	}
	block := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/preface"})
	rest := append([]byte("\r\nSM\r\n\r\n"), http2TestFrame(1, 4, 1, block)...)
	if events, claimed := tracker.http2Events(http2TestPacket(rest, 2, true, len("PRI * HTTP/2.0\r\n")), 0); !claimed || len(events) != 1 || events[0].Path != "/preface" {
		t.Fatalf("preface rest = %#v, %t", events, claimed)
	}
}

func TestHTTP2TraceDropsResetStreams(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	block := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/reset"})
	request := append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, block)...)
	if events, _ := tracker.http2Events(httpTestPacket(string(request), 1, true), 0); len(events) != 1 {
		t.Fatalf("request = %#v", events)
	}
	reset := http2TestFrame(3, 0, 1, []byte{0, 0, 0, 8})
	if events, claimed := tracker.http2Events(http2TestPacket(reset, 2, false, 0), 0); !claimed || len(events) != 0 {
		t.Fatalf("reset = %#v, %t", events, claimed)
	}
	if len(tracker.h2Pending) != 0 || tracker.h2Size != 0 {
		t.Fatalf("pending after reset = %#v, %d", tracker.h2Pending, tracker.h2Size)
	}
}

// 잃은 byte가 있으면 그 방향은 더 읽지 않는다. frame 경계와 HPACK 표를 되찾을 수 없다.
func TestHTTP2TraceStopsAfterAGap(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	block := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/first"})
	first := append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, block)...)
	if events, _ := tracker.http2Events(httpTestPacket(string(first), 1, true), 0); len(events) != 1 {
		t.Fatalf("first = %#v", events)
	}
	next := http2TestFrame(1, 4, 3, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/after"}))
	// BPF가 쓰기 하나를 다 넘기지 못하면 다음 레코드는 기다리던 위치보다 뒤에서 시작한다.
	if events, claimed := tracker.http2Events(http2TestPacket(next, 2, true, len(first)+100), 0); !claimed || len(events) != 0 {
		t.Fatalf("after a gap = %#v, %t", events, claimed)
	}
	if events, claimed := tracker.http2Events(http2TestPacket(next, 3, true, len(first)+100+len(next)), 0); claimed || len(events) != 0 {
		t.Fatalf("after the stop = %#v, %t", events, claimed)
	}
}

// non-blocking socket은 송신 버퍼가 차면 일부만 보내고 나머지를 다시 쓴다. BPF는 시작할 때 길이를 다 넘기므로 다시 쓴
// byte가 같은 TCP 순번으로 다시 온다. 겹친 앞부분은 버리고 이어지는 frame만 읽는다.
func TestHTTP2TraceSkipsResentBytes(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	headers := func(path string) []byte {
		return http2TestFrame(1, 4, 1, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: path}))
	}
	const seq = 1_000_000
	first := append(append([]byte{}, http2Preface...), headers("/one")...)
	start := http2TestPacket(first, 1, true, seq)
	start.continued = false
	if events, _ := tracker.http2Events(start, 0); len(events) != 1 || events[0].Path != "/one" {
		t.Fatalf("first = %#v", events)
	}
	resent := append(append([]byte{}, first[len(first)-10:]...), headers("/two")...)
	if events, claimed := tracker.http2Events(http2TestPacket(resent, 2, true, seq+len(first)-10), 0); !claimed || len(events) != 1 || events[0].Path != "/two" {
		t.Fatalf("resent = %#v, %t", events, claimed)
	}
	if events, claimed := tracker.http2Events(http2TestPacket(first[:20], 3, true, seq), 0); !claimed || len(events) != 0 {
		t.Fatalf("whole resend = %#v, %t", events, claimed)
	}
	if events, _ := tracker.http2Events(http2TestPacket(headers("/three"), 4, true, seq+len(first)+len(headers("/two"))), 0); len(events) != 1 || events[0].Path != "/three" {
		t.Fatalf("after the resend = %#v", events)
	}
}

// 닫힘 레코드를 놓친 사이에 새 연결이 같은 socket 주소를 쓰면 시작 레코드가 온다. 남은 h2c 상태를 버리고 새로 읽어야
// 새 연결의 요청이 사라지지 않는다.
func TestHTTP2TraceDropsStaleStateOnANewStart(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	block := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/old"})
	if events, _ := tracker.http2Events(httpTestPacket(string(append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, block)...)), 1, true), 0); len(events) != 1 {
		t.Fatalf("old connection = %#v", events)
	}
	if events, claimed := tracker.http2Events(httpTestPacket("GET /new HTTP/1.1\r\nHost: x\r\n\r\n", 2, true), 0); claimed || len(events) != 0 {
		t.Fatalf("HTTP/1 start = %#v, %t", events, claimed)
	}
	if len(tracker.http2) != 0 || len(tracker.h2Pending) != 0 || tracker.h2Size != 0 {
		t.Fatalf("state left = %d directions, %#v pending, size %d", len(tracker.http2), tracker.h2Pending, tracker.h2Size)
	}
	block = http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/again"})
	if events, _ := tracker.http2Events(httpTestPacket(string(append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, block)...)), 3, true), 0); len(events) != 1 || events[0].Path != "/again" {
		t.Fatalf("new h2c connection = %#v", events)
	}
}

// 앞 연결에서 한 방향만 멈췄어도 시작 레코드가 오면 남은 반대 방향까지 버린다. 남기면 새 연결의 그 방향 레코드를 앞
// 연결의 위치와 HPACK 표로 읽는다.
func TestHTTP2TraceDropsAStaleDirectionLeftAlone(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	tracker.http2Events(httpTestPacket(string(http2Preface), 1, true), 0)
	delete(tracker.http2, httpStreamKey{socket: 1, sent: true})
	if events, claimed := tracker.http2Events(httpTestPacket("GET /new HTTP/1.1\r\nHost: x\r\n\r\n", 2, true), 0); claimed || len(events) != 0 {
		t.Fatalf("HTTP/1 start = %#v, %t", events, claimed)
	}
	if len(tracker.http2) != 0 {
		t.Fatalf("directions left = %d", len(tracker.http2))
	}
}

// 수신 위치는 BPF가 0부터 센다. preface와 함께 만든 수신 방향은 0에서 시작하므로 앞 레코드를 잃으면 멈춘다.
func TestHTTP2TraceStartsTheReceivedDirectionAtZero(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	start := http2TestPacket(append(append([]byte{}, http2Preface...), http2TestFrame(4, 0, 0, nil)...), 1, true, 5000)
	start.continued = false
	tracker.http2Events(start, 0)
	response := http2TestFrame(1, 4, 1, http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "200"}))
	if events, claimed := tracker.http2Events(http2TestPacket(response, 2, false, 9), 0); !claimed || len(events) != 0 {
		t.Fatalf("received after a gap = %#v, %t", events, claimed)
	}
}

// HPACK 오류 뒤에는 동적 표가 상대와 어긋나므로 그 방향은 더 읽지 않는다.
func TestHTTP2TraceStopsAfterAnHPACKError(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	broken := append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, []byte{0x80})...)
	if events, claimed := tracker.http2Events(httpTestPacket(string(broken), 1, true), 0); !claimed || len(events) != 0 {
		t.Fatalf("broken block = %#v, %t", events, claimed)
	}
	valid := http2TestFrame(1, 4, 3, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/later"}))
	if events, _ := tracker.http2Events(http2TestPacket(valid, 2, true, len(broken)), 0); len(events) != 0 {
		t.Fatalf("after an HPACK error = %#v", events)
	}
}

// "PRI "로 시작했지만 preface가 아니면 두 방향 모두 h2c로 보지 않고, 그 조각을 HTTP/1 경로로 돌려준다.
func TestHTTP2TraceGivesBackAFalsePreface(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	start := "PRI * HTTP/2.0\r\n"
	if _, claimed := tracker.http2Events(httpTestPacket(start, 1, true), 0); !claimed {
		t.Fatal("the preface start was not held")
	}
	if events, claimed := tracker.http2Events(http2TestPacket([]byte("Host: example\r\n"), 2, true, len(start)), 0); claimed || len(events) != 0 {
		t.Fatalf("false preface = %#v, %t", events, claimed)
	}
	if len(tracker.http2) != 0 {
		t.Fatalf("directions left = %d", len(tracker.http2))
	}
}

// 응답을 받지 못한 HTTP/2 요청도 HTTP/1처럼 상한에서 비운다. socket이 닫히면 그 socket의 요청만 지운다.
func TestHTTP2TraceCapsPendingRequests(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	request := []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":path", Value: "/x"}}
	if _, ok := tracker.http2HeaderEvent(httpTestPacket("", 1, true), 1, request, 0); !ok {
		t.Fatal("request was not read")
	}
	tracker.h2Size = traceHTTPPendingLimit
	other := httpTestPacket("", 2, true)
	other.socket = 2
	if _, ok := tracker.http2HeaderEvent(other, 1, request, 0); !ok {
		t.Fatal("request was not read")
	}
	if tracker.h2Size != 1 || len(tracker.h2Pending) != 1 || len(tracker.h2Pending[2]) != 1 {
		t.Fatalf("pending at the limit = %#v, %d", tracker.h2Pending, tracker.h2Size)
	}
	tracker.forget(2)
	if tracker.h2Size != 0 || len(tracker.h2Pending) != 0 {
		t.Fatalf("pending after forget = %#v, %d", tracker.h2Pending, tracker.h2Size)
	}
}

func TestParseHTTPRequestReadsTheRequestLineAndHost(t *testing.T) {
	method, target, host, ok := parseHTTPRequest([]byte("GET /search?q=secret HTTP/1.1\r\nUser-Agent: curl\r\nHOST: Example.COM:8080\r\nCookie: a=b\r\n\r\n"))
	if !ok || method != "GET" || target != "/search?q=secret" || host != "example.com:8080" {
		t.Fatalf("request = %q %q %q %t", method, target, host, ok)
	}
	// body 조각이 method로 시작해도 요청 줄 형식이 아니면 HTTP가 아니다.
	for _, payload := range []string{"GET ready\r\n", "GETS / HTTP/1.1\r\n", "GET / HTTP/2\r\n", "GET / HTTP/1.1", "CONNECT example.com:443 HTTP/1.1\r\n"} {
		if _, _, _, ok := parseHTTPRequest([]byte(payload)); ok {
			t.Fatalf("%q was read as a request", payload)
		}
	}
	for payload, want := range map[string]int{"HTTP/1.1 200 OK\r\n": 200, "HTTP/1.0 404\r\n": 404, "HTTP/1.1 100 Continue\r\n": 100} {
		if status, ok := parseHTTPStatus([]byte(payload)); !ok || status != want {
			t.Fatalf("%q status = %d, %t", payload, status, ok)
		}
	}
	for _, payload := range []string{"HTTP/2 200 OK\r\n", "HTTP/1.1 2000 OK\r\n", "HTTP/1.1 abc OK\r\n", "HTTP/1.1 700 X\r\n"} {
		if status, ok := parseHTTPStatus([]byte(payload)); ok {
			t.Fatalf("%q was read as status %d", payload, status)
		}
	}
	for target, want := range map[[2]string][2]string{
		{"/a/b?token=x#top", ""}:              {"", "/a/b"},
		{"http://Proxy.Example/p?x=1", ""}:    {"proxy.example", "/p"},
		{"http://ignored/p", "given.example"}: {"given.example", "/p"},
	} {
		if host, path := traceHTTPTarget(target[0], target[1]); host != want[0] || path != want[1] {
			t.Fatalf("traceHTTPTarget(%q, %q) = %q, %q", target[0], target[1], host, path)
		}
	}
}

func httpTestPacket(payload string, at uint64, sent bool) httpPacket {
	return httpPacket{bootTimeNS: at, pid: 7, process: "curl", sent: sent, socket: 1, source: "127.0.0.1:40000", destination: "127.0.0.1:8080", payload: []byte(payload)}
}

func TestHTTPTrackerMatchesResponsesInOrder(t *testing.T) {
	tracker := newHTTPTracker(traceClientSide, false, false)
	var events []captureEvent
	for _, packet := range []httpPacket{
		httpTestPacket("GET /a?x=1 HTTP/1.1\r\nHost: api.example\r\n\r\n", 1_000_000, true),
		httpTestPacket("POST /b HTTP/1.1\r\nHost: api.example\r\n\r\n", 2_000_000, true),
		httpTestPacket("HTTP/1.1 100 Continue\r\n\r\n", 2_500_000, false),
		httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 3_000_000, false),
		httpTestPacket("HTTP/1.1 503 Service Unavailable\r\n\r\n", 6_000_000, false),
		httpTestPacket("HTTP/1.1 204 No Content\r\n\r\n", 7_000_000, false),
	} {
		event, ok := tracker.event(packet, 0)
		if !ok {
			t.Fatalf("packet %q made no event", packet.payload)
		}
		events = append(events, event)
	}
	request, interim, first, second, stray := events[0], events[2], events[3], events[4], events[5]
	if request.Event != traceHTTPRequestEvent || request.Method != "GET" || request.Path != "/a" || request.Target != "api.example" || request.Protocol != "http" {
		t.Fatalf("request = %#v", request)
	}
	// 1xx는 요청을 꺼내지 않는다. 최종 응답이 오래된 요청부터 짝짓는다.
	if interim.Status != 100 || interim.LatencyMS != nil || interim.Path != "/a" {
		t.Fatalf("interim = %#v", interim)
	}
	if first.Event != "http_2xx" || first.Path != "/a" || first.LatencyMS == nil || *first.LatencyMS != 2 || first.answered != 1 {
		t.Fatalf("first response = %#v", first)
	}
	if second.Event != "http_5xx" || second.Method != "POST" || second.Path != "/b" || *second.LatencyMS != 4 {
		t.Fatalf("second response = %#v", second)
	}
	if stray.LatencyMS != nil || stray.Path != "" || stray.Target != "127.0.0.1:8080" {
		t.Fatalf("response without a request = %#v", stray)
	}
	if destination, label := traceHTTPScrollLabels(second); destination != "client: POST api.example/b (127.0.0.1:8080)" || label != "http_5xx 503 4.0ms" {
		t.Fatalf("labels = %q, %q", destination, label)
	}
	// client 쪽 tracker는 서버가 받은 요청을 버린다.
	if event, ok := tracker.event(httpTestPacket("GET / HTTP/1.1\r\n\r\n", 8_000_000, false), 0); ok {
		t.Fatalf("client tracker kept a received request: %#v", event)
	}

	summarizer := newHTTPTraceSummarizer()
	for _, event := range append(events, captureEvent{Protocol: "http", Side: traceClientSide, Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/a"}) {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(httpTraceReport)
	if report.Requests != 3 || report.Responses != 4 || report.ServerErrors != 1 || report.Unanswered != 1 || report.Paths[0].Path != "/a" || report.Paths[0].Requests != 2 {
		t.Fatalf("report = %#v", report)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"requests":3`, `"paths":[`, `"statuses":{"100":1,"200":1}`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	// event 보기는 요청과 응답을 다른 행에 둔다. 응답을 받은 요청은 http_request 행에서 응답 없음으로 세지 않는다.
	for _, group := range summarizeTraceGroups("http", traceGroupByEvent, events, captureSummary{}, time.Second, "", "").Groups {
		if group.Group == traceHTTPRequestEvent && group.HTTP.Unanswered != 0 {
			t.Fatalf("event view http_request unanswered = %d", group.HTTP.Unanswered)
		}
	}
}

func TestHTTPServerSideTimesTheServer(t *testing.T) {
	tracker := newHTTPTracker(traceServerSide, false, false)
	request, ok := tracker.event(httpTestPacket("GET /health HTTP/1.1\r\n\r\n", 1_000_000, false), 0)
	if !ok || request.Side != traceServerSide || request.Target != "" || request.Path != "/health" {
		t.Fatalf("server request = %#v, %t", request, ok)
	}
	response, ok := tracker.event(httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 1_300_000, true), 0)
	if !ok || response.Side != traceServerSide || response.LatencyMS == nil || *response.LatencyMS < 0.29 || *response.LatencyMS > 0.31 {
		t.Fatalf("server response = %#v, %t", response, ok)
	}
	if event, ok := tracker.event(httpTestPacket("GET / HTTP/1.1\r\n\r\n", 2_000_000, true), 0); ok {
		t.Fatalf("server tracker kept a sent request: %#v", event)
	}
}

func TestTraceHTTPPayloadHidesSecretsAndEscapesControls(t *testing.T) {
	for _, test := range []struct{ name, payload, want string }{
		{"request with body",
			"POST /api?token=abc HTTP/1.1\r\nHost: x\r\nauthorization: Bearer s3cret\r\nCOOKIE: a=b\r\nContent-Type: application/json\r\n\r\n{\"k\":\"한글\"}\x1b[31m",
			"POST /api?token=abc HTTP/1.1\r\nHost: x\r\nauthorization: ***\r\nCOOKIE: ***\r\nContent-Type: application/json\r\n\r\n{\"k\":\"한글\"}\\x1b[31m"},
		// 512바이트에서 잘린 마지막 header 줄은 CRLF가 없어도 가린다.
		{"cut in a header", "GET / HTTP/1.1\r\nHost: x\r\nProxy-Authorization: Basic YWxh", "GET / HTTP/1.1\r\nHost: x\r\nProxy-Authorization: ***"},
		// 콜론 앞 공백은 HTTP 규칙 위반이라 서버가 거부하지만, 값은 이미 보냈으므로 가린다.
		{"space before the colon", "GET / HTTP/1.1\r\nHost: x\r\nAuthorization : Basic YWxh\r\n\r\n", "GET / HTTP/1.1\r\nHost: x\r\nAuthorization : ***\r\n\r\n"},
		{"response", "HTTP/1.1 200 OK\r\nSet-Cookie: id=1\r\n\r\nok\t\xff\x7f\xc2\x9b\n", "HTTP/1.1 200 OK\r\nSet-Cookie: ***\r\n\r\nok\t\\xff\\x7f\\xc2\\x9b\n"},
		{"cookie text in the body", "HTTP/1.1 200 OK\r\n\r\nCookie: visible", "HTTP/1.1 200 OK\r\n\r\nCookie: visible"},
		// --tls로 푼 HTTPS에는 API key를 따로 보내는 header가 많다.
		{"api key headers", "POST / HTTP/1.1\r\nX-Api-Key: sk-1\r\napi-key: 2\r\nX-Goog-Api-Key: 3\r\nX-Amz-Security-Token: 4\r\n\r\n",
			"POST / HTTP/1.1\r\nX-Api-Key: ***\r\napi-key: ***\r\nX-Goog-Api-Key: ***\r\nX-Amz-Security-Token: ***\r\n\r\n"},
	} {
		if got := traceHTTPPayload([]byte(test.payload), false); got != test.want {
			t.Fatalf("%s: payload = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestTraceHTTPPayloadLineShowsTheBodyOrTheHeaders(t *testing.T) {
	for payload, want := range map[string]string{
		"POST / HTTP/1.1\r\nHost: x\r\n\r\n{\"k\":1}\r\nnext\tline": "body {\"k\":1} ↵ next line",
		"GET / HTTP/1.1\r\nHost: x\r\nAccept: */*\r\n\r\n":          "headers Host: x · Accept: */*",
		"GET / HTTP/1.1\r\nHost: x\r\nUser-Agent: cu":               "headers Host: x · User-Agent: cu",
		"HTTP/1.1 204 No Content\r\n\r\n":                           "headers -",
	} {
		if got := traceHTTPPayloadLine(payload); got != want {
			t.Fatalf("line for %q = %q, want %q", payload, got, want)
		}
	}
}

// kernel은 해제한 socket의 주소를 새 socket에 다시 쓴다. 응답을 놓친 요청이 남아 있으면 새 연결의 응답이 그 요청과
// 짝지어지고, 응답 시간도 앞 연결의 요청부터 잰다.
func TestHTTPTrackerForgetsAClosedSocket(t *testing.T) {
	tracker := newHTTPTracker(traceClientSide, false, false)
	if _, ok := tracker.event(httpTestPacket("GET /old HTTP/1.1\r\nHost: api.example\r\n\r\n", 1_000_000, true), 0); !ok {
		t.Fatal("request made no event")
	}
	tracker.forget(1)
	tracker.forget(42)
	if len(tracker.pending) != 0 || tracker.size != 0 {
		t.Fatalf("pending = %#v, size %d", tracker.pending, tracker.size)
	}
	tracker.event(httpTestPacket("GET /new HTTP/1.1\r\nHost: api.example\r\n\r\n", 5_000_000, true), 0)
	response, ok := tracker.event(httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 6_000_000, false), 0)
	if !ok || response.Path != "/new" || response.LatencyMS == nil || *response.LatencyMS != 1 || tracker.size != 0 {
		t.Fatalf("response = %#v, size %d", response, tracker.size)
	}
}

func TestHTTPTrackerAddsThePayloadOnlyWhenAsked(t *testing.T) {
	packet := httpTestPacket("GET / HTTP/1.1\r\nHost: x\r\nCookie: a=b\r\n\r\n", 1_000_000, true)
	if event, _ := newHTTPTracker(traceClientSide, false, false).event(packet, 0); event.Payload != "" {
		t.Fatalf("payload without --payload: %q", event.Payload)
	}
	event, _ := newHTTPTracker(traceClientSide, true, false).event(packet, 0)
	if event.Payload != "GET / HTTP/1.1\r\nHost: x\r\nCookie: ***\r\n\r\n" {
		t.Fatalf("payload = %q", event.Payload)
	}
	encoded, err := json.Marshal(event)
	if err != nil || !strings.Contains(string(encoded), `"payload":"GET / HTTP/1.1\r\nHost: x\r\nCookie: ***\r\n\r\n"`) {
		t.Fatalf("raw event = %s (%v)", encoded, err)
	}
}

func TestHTTPTraceScreenPutsThePayloadUnderItsEvent(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadHead}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	request := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "POST", Target: "api.example", Path: "/a", Payload: "POST /a HTTP/1.1\r\nHost: api.example\r\n\r\n{\"k\":1}"}
	plain := captureEvent{Protocol: "http", Event: "http_2xx", Process: "curl", Target: "api.example", Path: "/a", Status: 200}
	model.events, model.width = []captureEvent{request, request, plain}, 120
	// 3줄이면 마지막 event와 그 앞 event의 두 줄이 들어간다. 그 앞 요청은 두 줄이 다 들어가지 않으므로 빼야 한다.
	for height, want := range map[int][]string{6: {"POST api.example/a", "↳ body {\"k\":1}", "http_2xx 200"}, 5: {"http_2xx 200", ""}} {
		model.height = height
		rows := traceScreenRows(model)
		if len(rows) != len(want) {
			t.Fatalf("height %d rows = %q", height, rows)
		}
		for index, text := range want {
			if !strings.Contains(rows[index], text) || (text == "" && rows[index] != "") {
				t.Fatalf("height %d row %d = %q, want %q", height, index, rows[index], text)
			}
		}
	}
}

func TestTracePayloadOptionNeedsHTTPEvents(t *testing.T) {
	for _, args := range [][]string{{"tcp", "--payload"}, {"dns", "--payload"}, {"http", "--payload", "--json", "-"}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
}

func TestTraceEscapeTextKeepsCleanTextAndEscapesAnywhere(t *testing.T) {
	long := strings.Repeat("x", 3000)
	for input, want := range map[string]string{
		long:                  long,
		"\x1b" + long:         `\x1b` + long,
		long + "\x07":         long + `\x07`,
		"a\r\n\tb":            "a\r\n\tb",
		"한\x00글\xe2\x82":      `한\x00글\xe2\x82`,
		long + "\xc2\x9b[31m": long + `\xc2\x9b[31m`,
	} {
		if got := traceEscapeText([]byte(input)); got != want {
			t.Fatalf("traceEscapeText(%q) = %q, want %q", input[:min(len(input), 20)], got[:min(len(got), 40)], want[:min(len(want), 40)])
		}
	}
}

func TestTracePortOptionNeedsHTTPAndAPort(t *testing.T) {
	for _, args := range [][]string{{"tcp", "--port", "80"}, {"dns", "--port", "53"}, {"http", "--port", "0"}, {"http", "--port", "65536"}, {"http", "--port", "-1"}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
	if scope := (tcpTraceOptions{side: traceServerSide, port: 8080}).scope("http"); scope.port != 8080 || !scope.server {
		t.Fatalf("scope = %+v", scope)
	}
}

// proxyTestEvents는 proxy가 받은 요청(socket 2)을 upstream에 보내고(socket 1) 응답을 돌려주는 한 번의 흐름이다.
func proxyTestEvents(t *testing.T, tracker *httpTracker) []captureEvent {
	t.Helper()
	packet := func(payload string, at uint64, sent, server bool) httpPacket {
		packet := httpTestPacket(payload, at, sent)
		packet.process = "nginx"
		if server {
			packet.socket, packet.source, packet.destination = 2, "10.0.0.1:9900", "203.0.113.7:50000"
		}
		return packet
	}
	var events []captureEvent
	for _, packet := range []httpPacket{
		packet("GET /admin/chain HTTP/1.1\r\nHost: node.example:9900\r\n\r\n", 1_000_000, false, true),
		packet("GET /admin/chain HTTP/1.1\r\nHost: localhost:9000\r\n\r\n", 1_100_000, true, false),
		packet("HTTP/1.1 200 OK\r\n\r\n", 2_100_000, false, false),
		packet("HTTP/1.1 200 OK\r\n\r\n", 2_500_000, true, true),
	} {
		if event, ok := tracker.event(packet, 0); ok {
			events = append(events, event)
		}
	}
	return events
}

func TestHTTPTrackerShowsBothSidesByDefault(t *testing.T) {
	events := proxyTestEvents(t, newHTTPTracker("", false, false))
	if len(events) != 4 {
		t.Fatalf("events = %#v", events)
	}
	received, sent, answer, reply := events[0], events[1], events[2], events[3]
	if received.Side != traceServerSide || sent.Side != traceClientSide || answer.Side != traceClientSide || reply.Side != traceServerSide {
		t.Fatalf("sides = %q %q %q %q", received.Side, sent.Side, answer.Side, reply.Side)
	}
	// 쪽마다 socket이 달라 짝이 섞이지 않는다. client 쪽은 upstream 응답까지, 서버 쪽은 받은 요청에 답할 때까지 잰다.
	if answer.LatencyMS == nil || *answer.LatencyMS != 1 || answer.Target != "localhost:9000" || reply.LatencyMS == nil || *reply.LatencyMS != 1.5 || reply.Target != "node.example:9900" {
		t.Fatalf("answer = %#v, reply = %#v", answer, reply)
	}
	for _, test := range []struct {
		event captureEvent
		want  string
	}{
		{received, "server: GET node.example:9900/admin/chain (203.0.113.7:50000)"},
		{sent, "client: GET localhost:9000/admin/chain (127.0.0.1:8080)"},
		{captureEvent{Protocol: "http", Side: traceServerSide, Event: "http_2xx"}, "server: -"},
	} {
		if destination, _ := traceHTTPScrollLabels(test.event); destination != test.want {
			t.Fatalf("destination label = %q, want %q", destination, test.want)
		}
	}
	for side, want := range map[string]int{traceClientSide: 2, traceServerSide: 2} {
		if got := len(proxyTestEvents(t, newHTTPTracker(side, false, false))); got != want {
			t.Fatalf("--side %s events = %d, want %d", side, got, want)
		}
	}
}

func TestHTTPTraceSummarySplitsTheSides(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	events := proxyTestEvents(t, newHTTPTracker("", false, false))
	summarizer := newHTTPTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(httpTraceReport)
	if report.Side != "" || report.Client == nil || report.Server == nil || report.Client.Requests != 1 || report.Server.Requests != 1 || len(report.Paths) != 2 {
		t.Fatalf("report = %#v", report)
	}
	if *report.Client.LatencyAvgMS != 1 || *report.Server.LatencyAvgMS != 1.5 {
		t.Fatalf("latency client %v, server %v", *report.Client.LatencyAvgMS, *report.Server.LatencyAvgMS)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"client":{"requests":1`, `"server":{"requests":1`, `"side":"server","method":"GET","host":"node.example:9900"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) })
	for _, text := range []string{"Client side (requests that this host sent):\n  Requests: 1\n", "Server side (requests that local servers received):\n", "\nSIDE\tMETHOD\t", "\nserver\tGET\tnode.example:9900\t/admin/chain\t1\t"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	// 한 쪽만 있으면 지금까지와 같은 요약을 쓴다.
	serverOnly := newHTTPTraceSummarizer()
	serverOnly.observe(events[0])
	serverOnly.observe(events[3])
	single := serverOnly.summarize(captureSummary{}, time.Second).(httpTraceReport)
	if single.Side != traceServerSide || single.Client != nil || single.Server != nil {
		t.Fatalf("server-only report = %#v", single)
	}
	if output := traceCaptureOutput(t, &os.Stdout, func() { single.print(false) }); !strings.HasPrefix(output, "HTTP server trace: ") || strings.Contains(output, "SIDE") || !strings.Contains(output, "\nRequests: 1\n") {
		t.Fatalf("server-only summary = %q", output)
	}
}

func TestHTTPTraceGroupsKeepTheSidesApart(t *testing.T) {
	events := proxyTestEvents(t, newHTTPTracker("", false, false))
	processes := summarizeTraceGroups("http", traceGroupByProcess, events, captureSummary{}, time.Second, "", "")
	if processes.Side != "" || len(processes.Groups) != 2 {
		t.Fatalf("process groups = %#v", processes)
	}
	for _, group := range processes.Groups {
		want, label := 1.0, "nginx"
		if group.Server {
			want, label = 1.5, "nginx (server)"
		}
		if group.Group != "nginx" || group.HTTP == nil || group.HTTP.Requests != 1 || *group.HTTP.LatencyAvgMS != want {
			t.Fatalf("process group = %#v", group)
		}
		if value := traceGroupDisplayValue(traceGroupByProcess, group); value != label {
			t.Fatalf("group label = %q, want %q", value, label)
		}
	}
	if processes.Groups[0].Server == processes.Groups[1].Server {
		t.Fatalf("both process groups have server = %t", processes.Groups[0].Server)
	}
	// event 보기에서 서버가 답한 요청은 서버 쪽 http_request 행에서 응답 없음으로 세지 않는다.
	for _, group := range summarizeTraceGroups("http", traceGroupByEvent, events, captureSummary{}, time.Second, "", "").Groups {
		if group.Group == traceHTTPRequestEvent && group.HTTP.Unanswered != 0 {
			t.Fatalf("event view %q server=%t unanswered = %d", group.Group, group.Server, group.HTTP.Unanswered)
		}
	}
	if report := summarizeTraceGroups("http", traceGroupByProcess, events[:1], captureSummary{}, time.Second, "", ""); report.Side != traceServerSide {
		t.Fatalf("server-only group report side = %q", report.Side)
	}
}

func TestHTTPTraceGroupsByPath(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	orphan := httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 7_000_000, false)
	orphan.socket = 3
	var events []captureEvent
	for _, packet := range []httpPacket{
		httpTestPacket("GET /a?token=x HTTP/1.1\r\nHost: api.example\r\n\r\n", 1_000_000, true),
		httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 2_000_000, false),
		httpTestPacket("POST /a HTTP/1.1\r\nHost: other.example\r\n\r\n", 3_000_000, true),
		httpTestPacket("HTTP/1.1 503 Service Unavailable\r\n\r\n", 4_000_000, false),
		httpTestPacket("GET /b HTTP/1.1\r\nHost: api.example\r\n\r\n", 5_000_000, true),
		httpTestPacket(string(tlsHandBuiltClientHello("api.example", "h2")), 6_000_000, true),
		orphan,
	} {
		event, ok := tracker.event(packet, 0)
		if !ok {
			t.Fatalf("packet %q made no event", packet.payload)
		}
		events = append(events, event)
	}
	groups := map[string]traceGroupSummary{}
	for _, group := range summarizeTraceGroups("http", traceGroupByPath, events, captureSummary{}, time.Second, "", "").Groups {
		groups[group.Group] = group
	}
	if len(groups) != 3 {
		t.Fatalf("path groups = %#v", groups)
	}
	// 응답은 짝지은 요청의 path를 물려받아, 요청과 같은 행에서 응답 없음이 빠진다. host와 method가 달라도 한 행이다.
	if http := groups["/a"].HTTP; http == nil || http.Requests != 2 || http.Responses != 2 || http.ServerErrors != 1 || http.Unanswered != 0 {
		t.Fatalf("/a group = %#v", http)
	}
	if http := groups["/b"].HTTP; http == nil || http.Requests != 1 || http.Unanswered != 1 {
		t.Fatalf("/b group = %#v", http)
	}
	// path를 모르는 ClientHello와 짝 없는 응답은 - 행에 모은다.
	if group := groups["-"]; group.Events != 2 || group.HTTP == nil || group.HTTP.Requests != 0 || group.HTTP.Responses != 1 {
		t.Fatalf("- group = %#v, http %#v", group, group.HTTP)
	}
	sides := summarizeTraceGroups("http", traceGroupByPath, proxyTestEvents(t, newHTTPTracker("", false, false)), captureSummary{}, time.Second, "", "")
	if len(sides.Groups) != 2 || sides.Groups[0].Server == sides.Groups[1].Server {
		t.Fatalf("proxy path groups = %#v", sides.Groups)
	}
	for _, group := range sides.Groups {
		label := "/admin/chain"
		if group.Server {
			label += traceServerSuffix
		}
		if value := traceGroupDisplayValue(traceGroupByPath, group); value != label || group.HTTP.Requests != 1 || group.HTTP.Unanswered != 0 {
			t.Fatalf("proxy path group %q = %#v", value, group)
		}
	}
}

func TestTraceGroupsPutTheClientRowBeforeTheServerRowOfTheSameName(t *testing.T) {
	events := proxyTestEvents(t, newHTTPTracker("", false, false))
	// group은 map에 모으므로 순서가 실행마다 다르다. 여러 번 만들어 매번 같은 순서인지 본다.
	for range 32 {
		for _, view := range []string{traceGroupByProcess, traceGroupByPath} {
			groups := summarizeTraceGroups("http", view, events, captureSummary{}, time.Second, "", "").Groups
			if len(groups) != 2 || groups[0].Server || !groups[1].Server {
				t.Fatalf("%s groups = %#v", view, groups)
			}
		}
	}
}

func TestOnlyHTTPTraceHasThePathView(t *testing.T) {
	if views := traceGroupViews("http"); views[len(views)-1] != traceGroupByPath {
		t.Fatalf("http views = %q", views)
	}
	if help := traceScreenHelp("http"); !strings.Contains(help, "e event  u path  g scroll") {
		t.Fatalf("http help = %q", help)
	}
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	next, _ := model.updateKey(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if next.(traceScreenModel).groupBy != traceGroupByPath {
		t.Fatal("u did not switch the http trace to the path view")
	}
	if got := nextTraceGroup(traceGroupViews("http"), traceGroupByPath, 1); got != "" {
		t.Fatalf("tab after path = %q, want the event scroll", got)
	}
	for _, protocol := range []string{"tcp", "udp", "dns", "mysql", "socket", "drop"} {
		if slices.Contains(traceGroupViews(protocol), traceGroupByPath) {
			t.Fatalf("%s has the path view", protocol)
		}
	}
	model = newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	if next, _ := model.updateKey(tea.KeyPressMsg{Code: 'u', Text: "u"}); next.(traceScreenModel).groupBy != "" {
		t.Fatal("u switched the tcp trace to the path view")
	}
	stderr := traceCaptureOutput(t, &os.Stderr, func() {
		if code := runTrace([]string{"tcp", "--group-by", traceGroupByPath}); code != 2 {
			t.Fatalf("trace tcp --group-by path exit = %d, want 2", code)
		}
	})
	if !strings.Contains(stderr, "--group-by path") {
		t.Fatalf("trace tcp --group-by path stderr = %q", stderr)
	}
}

func TestTraceLabelNamesAChosenHTTPSide(t *testing.T) {
	for _, test := range []struct{ protocol, side, want string }{
		{"http", "", "http"},
		{"http", traceClientSide, "http --side client"},
		{"http", traceServerSide, "http --side server"},
		{"dns", traceClientSide, "dns"},
		{"dns", traceServerSide, "dns --side server"},
	} {
		if got := traceLabel(test.protocol, test.side); got != test.want {
			t.Fatalf("traceLabel(%q, %q) = %q, want %q", test.protocol, test.side, got, test.want)
		}
	}
	if scope := (tcpTraceOptions{}).scope("http"); scope.side != "" || scope.server {
		t.Fatalf("default scope = %+v", scope)
	}
	if code := runTrace([]string{"http", "--side", "both"}); code != 2 {
		t.Fatalf("trace http --side both exit = %d, want 2", code)
	}
}

// tlsTestClientHello는 Go crypto/tls가 보내는 첫 record다. 서버 없이 ClientHello만 받고 연결을 닫는다.
func tlsTestClientHello(t *testing.T, serverName string, protocols ...string) []byte {
	t.Helper()
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		defer client.Close()
		_ = tls.Client(client, &tls.Config{ServerName: serverName, NextProtos: protocols, InsecureSkipVerify: serverName == ""}).Handshake()
	}()
	record := make([]byte, tlsRecordHeaderSize)
	if _, err := io.ReadFull(server, record); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(record[3])<<8|int(record[4]))
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	return append(record, body...)
}

// tlsHandBuiltClientHello는 SNI와 ALPN만 있는 ClientHello record다. 원격이 보낸 제어 문자를 시험한다.
func tlsHandBuiltClientHello(serverName string, protocols ...string) []byte {
	vector := func(size int, data []byte) []byte {
		prefix := make([]byte, size)
		for index := range size {
			prefix[index] = byte(len(data) >> (8 * (size - 1 - index)))
		}
		return append(prefix, data...)
	}
	extension := func(kind int, data []byte) []byte {
		return append([]byte{byte(kind >> 8), byte(kind)}, vector(2, data)...)
	}
	var names []byte
	for _, protocol := range protocols {
		names = append(names, vector(1, []byte(protocol))...)
	}
	extensions := append(extension(tlsExtensionServerName, vector(2, append([]byte{0}, vector(2, []byte(serverName))...))), extension(tlsExtensionALPN, vector(2, names))...)
	body := append([]byte{3, 3}, make([]byte, 32)...)
	body = append(body, 0, 0, 2, 0x13, 0x01, 1, 0)
	body = append(body, vector(2, extensions)...)
	handshake := append([]byte{tlsClientHelloType}, vector(3, body)...)
	return append([]byte{tlsHandshakeRecord, 3, 1}, vector(2, handshake)...)
}

func TestParseTLSClientHelloReadsTheServerNameAndALPN(t *testing.T) {
	record := tlsTestClientHello(t, "API.Example.test", "h2", "http/1.1")
	for name, test := range map[string]struct {
		payload []byte
		record  bool
	}{"record": {record, true}, "handshake without the record header": {record[tlsRecordHeaderSize:], false}} {
		hello, ok := parseTLSClientHello(test.payload, test.record)
		if !ok || hello.serverName != "api.example.test" || strings.Join(hello.alpn, ",") != "h2,http/1.1" {
			t.Fatalf("%s: hello = %+v, %t (%d bytes)", name, hello, ok, len(test.payload))
		}
	}
	// BPF가 앞부분만 읽어 확장이 잘려도 ClientHello다. 읽은 데까지만 쓴다.
	if hello, ok := parseTLSClientHello(record[:100], true); !ok || hello.serverName != "" {
		t.Fatalf("cut hello = %+v, %t", hello, ok)
	}
	if hello, ok := parseTLSClientHello(tlsTestClientHello(t, ""), true); !ok || hello.serverName != "" || hello.alpn != nil {
		t.Fatalf("hello without SNI = %+v, %t", hello, ok)
	}
	serverHello := slices.Clone(record)
	serverHello[tlsRecordHeaderSize] = 0x02
	badVersion := slices.Clone(record)
	badVersion[2] = 0x09
	for name, payload := range map[string][]byte{"ServerHello": serverHello, "record version": badVersion, "HTTP": []byte("GET / HTTP/1.1\r\n\r\n"), "header only": record[:tlsRecordHeaderSize]} {
		if hello, ok := parseTLSClientHello(payload, true); ok {
			t.Fatalf("%s was read as a ClientHello: %+v", name, hello)
		}
	}
	hello, ok := parseTLSClientHello(tlsHandBuiltClientHello("evil\x1b[31m.example", "h2\x07"), true)
	if !ok || hello.serverName != `evil\x1b[31m.example` || strings.Join(hello.alpn, ",") != `h2\x07` {
		t.Fatalf("hand-built hello = %+v, %t", hello, ok)
	}
}

func TestHTTPTrackerShowsTLSClientHellos(t *testing.T) {
	record := tlsHandBuiltClientHello("api.example", "h2", "http/1.1")
	sent := httpTestPacket(string(record), 1_000_000, true)
	received := httpTestPacket(string(record), 1_000_000, false)
	split := httpTestPacket(string(record[tlsRecordHeaderSize:]), 1_000_000, false)
	split.tlsHandshake = true
	tracker := newHTTPTracker("", true, false)
	for _, test := range []struct {
		packet httpPacket
		side   string
	}{{sent, traceClientSide}, {received, traceServerSide}, {split, traceServerSide}} {
		event, ok := tracker.event(test.packet, 0)
		if !ok || event.Event != traceTLSHelloEvent || event.Side != test.side || event.Target != "api.example" || strings.Join(event.ALPN, ",") != "h2,http/1.1" || event.Payload != "" || event.Method != "" {
			t.Fatalf("%s hello = %#v, %t", test.side, event, ok)
		}
	}
	event, _ := tracker.event(sent, 0)
	if destination, label := traceHTTPScrollLabels(event); destination != "client: api.example (127.0.0.1:8080)" || label != "tls_hello h2" {
		t.Fatalf("labels = %q, %q", destination, label)
	}
	if _, ok := newHTTPTracker(traceServerSide, false, false).event(sent, 0); ok {
		t.Fatal("server-side tracker kept a sent ClientHello")
	}
	// ClientHello는 응답을 기다리지 않는다.
	if len(tracker.pending) != 0 {
		t.Fatalf("pending = %#v", tracker.pending)
	}
	noName := httpTestPacket(string(tlsHandBuiltClientHello("")), 1_000_000, true)
	if event, ok := tracker.event(noName, 0); !ok || event.Target != "127.0.0.1:8080" {
		t.Fatalf("hello without SNI = %#v, %t", event, ok)
	}
	// record 머리 없이 온 조각이 ClientHello가 아니면 HTTP로도 읽지 않는다.
	other := httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 1_000_000, false)
	other.tlsHandshake = true
	if event, ok := tracker.event(other, 0); ok {
		t.Fatalf("a TLS handshake piece was read as HTTP: %#v", event)
	}
}

func TestHTTPMessagesPassTLSClientHellosThrough(t *testing.T) {
	record := tlsHandBuiltClientHello("api.example", "h2")
	starts := httpSplitStarts{}
	for _, packet := range []httpPacket{httpTestPacket(string(record[:tlsRecordHeaderSize+3]), 1, false), httpTestPacket(string(record), 1, true)} {
		if joined, ok := starts.join(packet); !ok || len(joined.payload) != len(packet.payload) || len(starts) != 0 {
			t.Fatalf("join = %+v, %t, held %d", joined, ok, len(starts))
		}
	}
	messages := newTestHTTPMessages(false, httpMessageMax)
	events := messages.add(httpTestPacket(string(record), 1_000_000, true), 0, time.Unix(100, 0))
	if len(events) != 1 || events[0].Event != traceTLSHelloEvent || len(messages.open) != 0 {
		t.Fatalf("events = %#v, open %d", events, len(messages.open))
	}
}

func TestHTTPSplitStartsJoinTLSHandshakeAcrossRecvmsgReads(t *testing.T) {
	record := tlsHandBuiltClientHello("api.example", "h2")
	starts := httpSplitStarts{}
	var joined httpPacket
	for index, payload := range [][]byte{record[tlsRecordHeaderSize : tlsRecordHeaderSize+1], record[tlsRecordHeaderSize+1 : tlsRecordHeaderSize+12], record[tlsRecordHeaderSize+12:]} {
		packet := httpTestPacket(string(payload), uint64(index+1), false)
		packet.tlsHandshake = index == 0
		if index != 0 {
			packet.continued = true
			packet.offset = uint32(1 + len(record[tlsRecordHeaderSize+1:tlsRecordHeaderSize+12]))
			if index == 1 {
				packet.offset = 1
			}
		}
		var ok bool
		joined, ok = starts.join(packet)
		if index < 2 && ok {
			t.Fatalf("piece %d came out early: %#v", index, joined)
		}
		if index == 2 && (!ok || !joined.tlsHandshake || string(joined.payload) != string(record[tlsRecordHeaderSize:])) {
			t.Fatalf("joined = %#v, %t", joined, ok)
		}
	}
	event, ok := newHTTPTracker(traceServerSide, false, false).event(joined, 0)
	if !ok || event.Event != traceTLSHelloEvent || event.Target != "api.example" {
		t.Fatalf("event = %#v, %t", event, ok)
	}
}

func TestHTTPSplitStartsJoinSyntheticHeaderAfterEveryPrefixSplit(t *testing.T) {
	record := tlsHandBuiltClientHello("split.example", "h2", "http/1.1")
	handshake := record[tlsRecordHeaderSize:]
	for prefix := 1; prefix <= tlsRecordHeaderSize+tlsHandshakeHeaderSize-1; prefix++ {
		starts := httpSplitStarts{}
		header := httpTestPacket(string(handshake[:tlsHandshakeHeaderSize]), 1, false)
		header.tlsHandshake = true
		if packet, ok := starts.join(header); ok {
			t.Fatalf("prefix %d emitted synthetic header: %#v", prefix, packet)
		}
		body := httpChunk(string(handshake[tlsHandshakeHeaderSize:]), 2, false, tlsHandshakeHeaderSize)
		packet, ok := starts.join(body)
		if !ok || string(packet.payload) != string(handshake) {
			t.Fatalf("prefix %d joined = %#v, %t", prefix, packet, ok)
		}
		event, ok := newHTTPTracker(traceServerSide, false, false).event(packet, 0)
		if !ok || event.Target != "split.example" || strings.Join(event.ALPN, ",") != "h2,http/1.1" {
			t.Fatalf("prefix %d event = %#v, %t", prefix, event, ok)
		}
	}
}

func TestHTTPSplitStartsWaitForTheDeclaredClientHelloLength(t *testing.T) {
	record := tlsHandBuiltClientHello("late.example", "h2", "http/1.1")
	handshake := record[tlsRecordHeaderSize:]
	starts := httpSplitStarts{}
	cut := len(handshake) - 3
	first := httpTestPacket(string(handshake[:cut]), 1, false)
	first.tlsHandshake = true
	if packet, ok := starts.join(first); ok {
		t.Fatalf("incomplete ClientHello = %#v", packet)
	}
	second := httpChunk(string(handshake[cut:]), 2, false, uint32(cut))
	packet, ok := starts.join(second)
	if !ok || !packet.tlsHandshake || string(packet.payload) != string(handshake) {
		t.Fatalf("joined = %#v, %t", packet, ok)
	}
	event, ok := newHTTPTracker(traceServerSide, false, false).event(packet, 0)
	if !ok || event.Target != "late.example" || strings.Join(event.ALPN, ",") != "h2,http/1.1" {
		t.Fatalf("event = %#v, %t", event, ok)
	}
}

func TestTLSHandshakeCompleteRejectsMalformedLengths(t *testing.T) {
	for name, payload := range map[string][]byte{
		"short":                  {tlsClientHelloType, 0, 0},
		"declared after payload": {tlsClientHelloType, 0, 0, 5, 3, 3},
		"other handshake":        {2, 0, 0, 0},
	} {
		if tlsHandshakeComplete(payload) {
			t.Fatalf("%s was complete", name)
		}
	}
}

func TestHTTPSplitStartsEmitsTruncatedClientHelloAtTheCaptureCap(t *testing.T) {
	record := tlsHandBuiltClientHello("capped.example", "h2")
	handshake := append([]byte(nil), record[tlsRecordHeaderSize:]...)
	declared := httpPayloadHead
	handshake[1] = byte(declared >> 16)
	handshake[2] = byte(declared >> 8)
	handshake[3] = byte(declared)
	handshake = append(handshake, make([]byte, httpPayloadHead-len(handshake))...)
	starts := httpSplitStarts{}
	packet := httpTestPacket(string(handshake), 1, false)
	packet.tlsHandshake = true
	joined, ok := starts.join(packet)
	if !ok || len(joined.payload) != httpPayloadHead {
		t.Fatalf("capped ClientHello = %#v, %t", joined, ok)
	}
	event, ok := newHTTPTracker(traceServerSide, false, false).event(joined, 0)
	if !ok || event.Event != traceTLSHelloEvent || event.Target != "capped.example" || strings.Join(event.ALPN, ",") != "h2" {
		t.Fatalf("event = %#v, %t", event, ok)
	}
	packet.payload = packet.payload[:httpPayloadHead-1]
	if joined, ok := (httpSplitStarts{}).join(packet); ok {
		t.Fatalf("short capped ClientHello = %#v", joined)
	}
}

func TestHTTPTraceSummaryCountsTLSConnectionsApart(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	tracker := newHTTPTracker("", false, false)
	var events []captureEvent
	for _, packet := range []httpPacket{
		httpTestPacket(string(tlsHandBuiltClientHello("api.example", "h2", "http/1.1")), 1_000_000, true),
		httpTestPacket(string(tlsHandBuiltClientHello("api.example", "h2", "http/1.1")), 2_000_000, true),
		httpTestPacket("GET /a HTTP/1.1\r\nHost: plain.example\r\n\r\n", 3_000_000, true),
		httpTestPacket("HTTP/1.1 200 OK\r\n\r\n", 4_000_000, false),
	} {
		event, ok := tracker.event(packet, 0)
		if !ok {
			t.Fatalf("packet %q made no event", packet.payload)
		}
		events = append(events, event)
	}
	summarizer := newHTTPTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(httpTraceReport)
	if report.Requests != 1 || report.Responses != 1 || report.TLSConnections != 2 || len(report.Paths) != 1 || len(report.TLS) != 1 || report.TLS[0].Connections != 2 || report.TLS[0].Host != "api.example" {
		t.Fatalf("report = %#v", report)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"tls_connections":2`, `"tls":[{"side":"client","host":"api.example","alpn":["h2","http/1.1"],"connections":2,"processes":["curl"]}]`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) })
	for _, text := range []string{"\nRequests: 1\n", "\nTLS connections: 2 (HTTPS, only the ClientHello is plain text)\n", "\nclient\tapi.example\th2,http/1.1\t2\tcurl\n"} {
		if !strings.Contains(output, text) {
			t.Fatalf("summary %q does not contain %q", output, text)
		}
	}
	// group 보기에서 ClientHello는 event로만 세고 응답으로 세지 않는다.
	for _, group := range summarizeTraceGroups("http", traceGroupByTarget, events, captureSummary{}, time.Second, "", "").Groups {
		if group.Group == "api.example" && (group.Events != 2 || group.HTTP.Requests != 0 || group.HTTP.Responses != 0) {
			t.Fatalf("TLS group = %#v, http %#v", group, group.HTTP)
		}
	}
	// TLS가 없으면 JSON과 요약은 지금까지와 같다.
	plain := newHTTPTraceSummarizer()
	plain.observe(events[2])
	data, _ = json.Marshal(plain.summarize(captureSummary{}, time.Second))
	if strings.Contains(string(data), "tls") {
		t.Fatalf("report without TLS = %s", data)
	}
}

// tlsTestPacket은 --tls가 OpenSSL에서 읽은 평문 레코드다.
func tlsTestPacket(payload string, at uint64, sent bool) httpPacket {
	packet := httpTestPacket(payload, at, sent)
	packet.decrypted = true
	return packet
}

// --tls 평문은 평문 HTTP처럼 짝지어지고 tls로 표시된다. 같은 socket의 ClientHello도 그대로 보인다.
func TestHTTPTrackerPairsTLSPlaintext(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	hello, ok := tracker.event(httpTestPacket(string(tlsHandBuiltClientHello("api.example", "http/1.1")), 1_000_000, true), 0)
	if !ok || hello.Event != traceTLSHelloEvent || hello.TLS {
		t.Fatalf("ClientHello = %#v, %t", hello, ok)
	}
	request, ok := tracker.event(tlsTestPacket("GET /v1/users?token=x HTTP/1.1\r\nHost: api.example\r\n\r\n", 2_000_000, true), 0)
	if !ok || request.Event != traceHTTPRequestEvent || !request.TLS || request.Target != "api.example" || request.Path != "/v1/users" {
		t.Fatalf("request = %#v, %t", request, ok)
	}
	response, ok := tracker.event(tlsTestPacket("HTTP/1.1 200 OK\r\n\r\n", 5_000_000, false), 0)
	if !ok || response.Status != 200 || !response.TLS || response.Path != "/v1/users" || response.LatencyMS == nil || *response.LatencyMS != 3 {
		t.Fatalf("response = %#v, %t", response, ok)
	}
	if _, label := traceHTTPScrollLabels(response); label != "http_2xx tls 200 3.0ms" {
		t.Fatalf("label = %q", label)
	}
	// 평문이 TLS record처럼 시작해도 ClientHello로 읽지 않는다. 평문은 이미 복호화된 byte다.
	if event, ok := tracker.event(tlsTestPacket(string(tlsHandBuiltClientHello("x.example", "h2")), 6_000_000, true), 0); ok {
		t.Fatalf("plaintext ClientHello bytes = %#v", event)
	}
}

func TestHTTPTrackerMarksHTTP2OverTLSOnce(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	preface := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x12\x04\x00"
	server := tlsTestPacket(preface, 2_000_000, false)
	server.socket = 2
	tracker := newHTTPTracker("", false, false)
	var events []captureEvent
	for _, packet := range []httpPacket{tlsTestPacket(preface, 1_000_000, true), server} {
		// 수집 루프처럼 h2c 처리를 먼저 거친다. --tls 평문은 h2c로 읽지 않는다.
		if _, claimed := tracker.http2Events(packet, 0); claimed {
			t.Fatalf("h2c claimed TLS plaintext %q", packet.payload)
		}
		event, ok := tracker.event(packet, 0)
		if !ok || event.Event != traceHTTP2UnparsedEvent || !event.TLS {
			t.Fatalf("preface = %#v, %t", event, ok)
		}
		events = append(events, event)
	}
	if events[0].Side != traceClientSide || events[1].Side != traceServerSide {
		t.Fatalf("sides = %s, %s", events[0].Side, events[1].Side)
	}
	if _, ok := newHTTPTracker(traceClientSide, false, false).event(server, 0); ok {
		t.Fatal("--side client must hide the server preface")
	}
	// 암호문은 preface일 수 없다. 같은 byte가 와도 HTTP/2로 읽지 않는다.
	if _, ok := tracker.event(httpTestPacket(preface, 3_000_000, true), 0); ok {
		t.Fatal("a ciphertext packet must not be read as an HTTP/2 preface")
	}
	// --payload=all의 조립기는 HTTP/2 frame을 따라가지 않으므로 message를 열지 않는다.
	messages := newHTTPMessages(newHTTPTracker("", false, false), httpMessageMax, false)
	if got := messages.add(tlsTestPacket(preface, 1_000_000, true), 0, time.Now()); len(got) != 1 || len(messages.open) != 0 {
		t.Fatalf("messages = %#v, open %d", got, len(messages.open))
	}
	summarizer := newHTTPTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(httpTraceReport)
	if report.Requests != 0 || report.Responses != 0 || len(report.Paths) != 0 || report.HTTP2Unparsed != 2 {
		t.Fatalf("report = %#v", report)
	}
	if data, _ := json.Marshal(report); !strings.Contains(string(data), `"http2_unparsed":2`) {
		t.Fatalf("report JSON = %s", data)
	}
	if output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) }); !strings.Contains(output, "\nHTTP/2 connections (not parsed): 2\n") {
		t.Fatalf("summary = %q", output)
	}
}

// node처럼 SSL 호출 안에서 socket을 쓰지 않는 program의 평문은 주소 없이 보이고, BPF가 준 id로 짝지어진다.
func TestHTTPTraceShowsTLSPlaintextWithoutAnAddress(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	unmapped := func(payload string, at uint64, sent bool) httpPacket {
		packet := tlsTestPacket(payload, at, sent)
		packet.socket, packet.source, packet.destination, packet.process = 7<<32|0x1234, "", "", "node"
		return packet
	}
	tracker := newHTTPTracker("", false, false)
	request, ok := tracker.event(unmapped("GET /orders HTTP/1.1\r\nHost: shop.example\r\n\r\n", 1_000_000, true), 0)
	if !ok || request.Target != "shop.example" || request.Source != "" || request.Destination != "" || !request.TLS {
		t.Fatalf("request = %#v, %t", request, ok)
	}
	response, ok := tracker.event(unmapped("HTTP/1.1 201 Created\r\n\r\n", 4_000_000, false), 0)
	if !ok || response.LatencyMS == nil || *response.LatencyMS != 3 || response.Path != "/orders" {
		t.Fatalf("response = %#v, %t", response, ok)
	}
	summarizer := newHTTPTraceSummarizer()
	summarizer.observe(request)
	summarizer.observe(response)
	// BPF가 --port 때문에 버린 3건과 주소 없이 보인 2건을 합친다.
	report := summarizer.summarize(captureSummary{TLSUnmapped: 3}, time.Second).(httpTraceReport)
	if report.TLSUnmapped != 5 || report.Requests != 1 {
		t.Fatalf("report = %#v", report)
	}
	if output := traceCaptureOutput(t, &os.Stdout, func() { report.print(false) }); !strings.Contains(output, "\nTLS plaintext without an address: 5\n") {
		t.Fatalf("summary = %q", output)
	}
	// SSL_free의 끝 레코드가 응답을 놓친 요청을 지운다. 지우지 않으면 같은 id를 다시 쓴 SSL 객체의 응답과 짝지어진다.
	if _, ok := tracker.event(unmapped("GET /lost HTTP/1.1\r\nHost: shop.example\r\n\r\n", 5_000_000, true), 0); !ok {
		t.Fatal("second request made no event")
	}
	tracker.forget(7<<32 | 0x1234)
	if response, ok := tracker.event(unmapped("HTTP/1.1 200 OK\r\n\r\n", 9_000_000, false), 0); !ok || response.LatencyMS != nil {
		t.Fatalf("response after forget = %#v, %t", response, ok)
	}
}

// 평문과 같은 socket의 암호문은 조각을 따로 잇는다. 섞이면 암호문이 평문 요청의 뒷부분으로 붙는다.
func TestHTTPSplitStartsKeepTLSPlaintextApart(t *testing.T) {
	starts := httpSplitStarts{}
	if _, ok := starts.join(tlsTestPacket("GET /long", 1, true)); ok {
		t.Fatal("an open first line must wait for the next fragment")
	}
	cipher := httpTestPacket("\x17\x03\x03\x00\x20", 2, true)
	cipher.continued, cipher.offset = true, 9
	if joined, ok := starts.join(cipher); !ok || !joined.continued || string(joined.payload) != string(cipher.payload) {
		t.Fatalf("ciphertext joined the plaintext: %#v, %t", joined, ok)
	}
	rest := tlsTestPacket(" HTTP/1.1\r\nHost: a.example\r\n\r\n", 3, true)
	rest.continued, rest.offset = true, 9
	joined, ok := starts.join(rest)
	if !ok || joined.continued || !joined.decrypted || !strings.HasPrefix(string(joined.payload), "GET /long HTTP/1.1\r\n") {
		t.Fatalf("plaintext join = %#v, %t", joined, ok)
	}
	starts.join(tlsTestPacket("GET /next", 4, true))
	starts.forget(1)
	if len(starts) != 0 {
		t.Fatalf("forget left %d fragments", len(starts))
	}
}

func TestTraceTLSOptionNeedsHTTP(t *testing.T) {
	for _, args := range [][]string{{"tcp", "--tls"}, {"mysql", "--tls"}, {"dns", "--tls=/usr/lib/libssl.so.3"}, {"http", "--tls="}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
	var mode traceTLSMode
	for value, want := range map[string]traceTLSMode{"true": traceTLSAuto, "/opt/app/libssl.so.3": "/opt/app/libssl.so.3", "false": ""} {
		if err := mode.Set(value); err != nil || mode != want {
			t.Fatalf("Set(%q) = %q, %v", value, mode, err)
		}
	}
	finder := &traceTLSFinder{targets: []traceTLSTarget{{path: "/lib/libssl.so.3"}}}
	if scope := (tcpTraceOptions{tls: traceTLSAuto, tlsFinder: finder}).scope("http"); scope.tls != traceTLSAuto || scope.tlsFinder != finder {
		t.Fatalf("scope = %+v", scope)
	}
}
