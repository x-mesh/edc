package edc

import (
	"encoding/json"
	"os"
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
