package edc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

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
	tracker := newHTTPTracker(false, false, false)
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
	if destination, label := traceHTTPScrollLabels(second); destination != "POST api.example/b (127.0.0.1:8080)" || label != "http_5xx 503 4.0ms" {
		t.Fatalf("labels = %q, %q", destination, label)
	}
	// client 쪽 tracker는 서버가 받은 요청을 버린다.
	if event, ok := tracker.event(httpTestPacket("GET / HTTP/1.1\r\n\r\n", 8_000_000, false), 0); ok {
		t.Fatalf("client tracker kept a received request: %#v", event)
	}

	summarizer := newHTTPTraceSummarizer()
	for _, event := range append(events, captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/a"}) {
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
	tracker := newHTTPTracker(true, false, false)
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

func TestHTTPTrackerAddsThePayloadOnlyWhenAsked(t *testing.T) {
	packet := httpTestPacket("GET / HTTP/1.1\r\nHost: x\r\nCookie: a=b\r\n\r\n", 1_000_000, true)
	if event, _ := newHTTPTracker(false, false, false).event(packet, 0); event.Payload != "" {
		t.Fatalf("payload without --payload: %q", event.Payload)
	}
	event, _ := newHTTPTracker(false, true, false).event(packet, 0)
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
