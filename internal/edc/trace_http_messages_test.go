package edc

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func httpChunk(payload string, at uint64, sent bool, offset uint32) httpPacket {
	packet := httpTestPacket(payload, at, sent)
	packet.continued, packet.offset = true, offset
	return packet
}

func newTestHTTPMessages(server bool, limit int) *httpMessages {
	side := traceClientSide
	if server {
		side = traceServerSide
	}
	return newHTTPMessages(newHTTPTracker(side, false, false), limit, false)
}

func TestHTTPMessagesJoinABodyFromLaterWrites(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	now := time.Unix(100, 0)
	head := "POST /upload HTTP/1.1\r\nHost: api.example\r\nCookie: a=b\r\nContent-Length: 10\r\n\r\n"
	if events := messages.add(httpTestPacket(head, 1_000_000, true), 0, now); len(events) != 0 {
		t.Fatalf("request came out before its body: %#v", events)
	}
	if events := messages.add(httpChunk("01234", 1_100_000, true, uint32(len(head))), 0, now); len(events) != 0 {
		t.Fatalf("request came out with half of its body: %#v", events)
	}
	events := messages.add(httpChunk("56789", 1_200_000, true, uint32(len(head)+5)), 0, now)
	if len(events) != 1 || events[0].Event != traceHTTPRequestEvent || events[0].PayloadTruncated || events[0].Bytes != uint64(len(head)+10) {
		t.Fatalf("events = %#v", events)
	}
	if want := strings.Replace(head, "a=b", "***", 1) + "0123456789"; events[0].Payload != want {
		t.Fatalf("payload = %q", events[0].Payload)
	}
	// 응답은 요청과 짝지어져 응답 시간이 첫 조각 시각부터 잰다.
	response := messages.add(httpTestPacket("HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\nok", 3_000_000, false), 0, now)
	if len(response) != 1 || response[0].Status != 201 || response[0].LatencyMS == nil || *response[0].LatencyMS != 2 || !strings.HasSuffix(response[0].Payload, "\r\n\r\nok") {
		t.Fatalf("response = %#v", response)
	}
}

func TestHTTPMessagesProgressAfterHeadersWithStableIDs(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	messages.progressive = true
	now := time.Unix(100, 0)
	first := "POST /stream HTTP/1.1\r\nHost: api."
	if events := messages.add(httpTestPacket(first, 1_000_000, true), 0, now); len(events) != 0 {
		t.Fatalf("partial headers escaped: %#v", events)
	}
	head := "example\r\nContent-Length: 10\r\n\r\nabc"
	initial := messages.add(httpChunk(head, 1_100_000, true, uint32(len(first))), 0, now.Add(10*time.Millisecond))
	if len(initial) != 1 || initial[0].Update || initial[0].MessageID == 0 || initial[0].Target != "api.example" || initial[0].Path != "/stream" || !strings.HasSuffix(initial[0].Payload, "\r\n\r\nabc") {
		t.Fatalf("initial snapshot = %#v", initial)
	}
	id := initial[0].MessageID
	if repeated := messages.expire(now.Add(400 * time.Millisecond)); len(repeated) != 0 {
		t.Fatalf("unchanged snapshot repeated: %#v", repeated)
	}
	update := messages.add(httpChunk("d", 1_200_000, true, uint32(len(first)+len(head))), 0, now.Add(300*time.Millisecond))
	if len(update) != 1 || !update[0].Update || update[0].Final || update[0].MessageID != id || update[0].Bytes != uint64(len(first)+len(head)+1) || update[0].ByteDelta != 1 {
		t.Fatalf("update snapshot = %#v", update)
	}
	final := messages.add(httpTestPacket("GET /next HTTP/1.1\r\nHost: api.example\r\n\r\n", 2_000_000, true), 0, now.Add(400*time.Millisecond))
	if len(final) != 2 || !final[0].Update || !final[0].Final || final[0].MessageID != id || !final[0].PayloadTruncated || !strings.HasSuffix(final[0].Payload, "\r\n\r\nabcd") {
		t.Fatalf("final snapshot = %#v", final)
	}
}

func TestHTTPMessagesKeepLongStreamsOpenAcrossIdleSweeps(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	messages.progressive = true
	start := time.Unix(100, 0)
	first := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\npart"
	initial := messages.add(httpTestPacket(first, 1_000_000, false), 0, start)
	if len(initial) != 1 || initial[0].MessageID == 0 || initial[0].Final {
		t.Fatalf("initial SSE snapshot = %#v", initial)
	}
	if events := messages.expire(start.Add(2 * time.Second)); len(events) != 0 || len(messages.open) != 1 {
		t.Fatalf("idle sweep closed SSE: events %#v, open %d", events, len(messages.open))
	}
	update := messages.add(httpChunk("\nevent: next\n\n", 2_000_000, false, uint32(len(first))), 0, start.Add(3*time.Second))
	if len(update) != 1 || !update[0].Update || update[0].MessageID != initial[0].MessageID || !strings.HasSuffix(update[0].Payload, "event: next\n\n") {
		t.Fatalf("SSE body update = %#v", update)
	}
	final := messages.flush()
	if len(final) != 1 || !final[0].Final || !final[0].PayloadTruncated || final[0].MessageID != initial[0].MessageID {
		t.Fatalf("unfinished SSE final = %#v", final)
	}
}

func TestHTTP1AndHTTP2OpenPayloadsShareThe64MiBBudget(t *testing.T) {
	tracker := newHTTPTracker("", false, false)
	messages := newHTTPMessages(tracker, httpMessageMax, false)
	tracker.http1Messages = messages
	var evicted []captureEvent
	for socket := uint64(1); socket <= httpMessageOpenLimit+1; socket++ {
		head := "POST /large HTTP/1.1\r\nHost: api.example\r\nContent-Length: " + strconv.Itoa(httpMessageMax) + "\r\n\r\n"
		body := strings.Repeat("a", httpMessageMax-len(head)-1)
		packet := httpTestPacket(head+body, socket*1_000_000, true)
		packet.socket = socket
		evicted = append(evicted, messages.add(packet, 0, time.Unix(int64(socket), 0))...)
	}
	fields := http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":authority", Value: "api.example"}, hpack.HeaderField{Name: ":path", Value: "/stream"})
	start := append(append(append([]byte{}, http2Preface...), http2TestFrame(4, 0, 0, nil)...), http2TestFrame(1, 4, 1, fields)...)
	packet := tlsTestPacket(string(start), 100_000_000, true)
	packet.socket = 1000
	tracker.h2PayloadLimit = httpMessageMax
	tracker.http2Events(packet, 0)
	data := http2TestFrame(0, 0, 1, []byte(strings.Repeat("b", 1024)))
	packet = http2TLSTestPacket(data, 101_000_000, true, len(start))
	packet.socket = 1000
	tracker.http2Events(packet, 0)
	if len(messages.open)+len(tracker.h2Payload) > httpMessageOpenLimit || messages.bytes+tracker.h2PayloadBytes > httpMessageOpenBytes {
		t.Fatalf("combined open state: HTTP/1 %d messages/%d bytes, HTTP/2 %d streams/%d bytes", len(messages.open), messages.bytes, len(tracker.h2Payload), tracker.h2PayloadBytes)
	}
	if _, stillOpen := messages.open[httpStreamKey{socket: 1, sent: true}]; stillOpen || len(evicted) == 0 || !evicted[0].PayloadTruncated {
		t.Fatalf("oldest HTTP/1 message was not evicted as partial: %d prior final events", len(evicted))
	}
}

func TestHTTPProgressiveTrafficMatchesFinalEventsWithoutDuplicateCounts(t *testing.T) {
	messageLength := 10_001
	head := "POST /upload HTTP/1.1\r\nHost: api.example\r\nContent-Length: " + strconv.Itoa(messageLength) + "\r\n\r\n"
	start := time.Unix(100, 0)
	tracker := newHTTPTracker("", false, false)
	messages := newHTTPMessages(tracker, httpMessageMax, false)
	messages.progressive = true
	packet := httpTestPacket(head+"a", 1_000_000, true)
	progressive := messages.add(packet, 0, start)
	for index := 0; index < 100; index++ {
		chunk := strings.Repeat(string(rune('b'+index%20)), 100)
		progressive = append(progressive, messages.add(httpChunk(chunk, uint64(1_100_000+index), true, uint32(len(head)+1+index*100)), 0, start.Add(time.Duration(index+1)*300*time.Millisecond))...)
	}
	response := httpTestPacket("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", 2_000_000, false)
	progressive = append(progressive, messages.add(response, 0, start.Add(31*time.Second))...)
	if len(progressive) != 102 || !progressive[0].Update && progressive[0].ByteDelta != progressive[0].Bytes || !progressive[len(progressive)-2].Update || !progressive[len(progressive)-2].Final {
		t.Fatalf("progressive event count or markers = %d, last %#v", len(progressive), progressive[len(progressive)-2:])
	}
	progressiveHTTP := newHTTPTraceSummarizer()
	progressiveGroups := newTraceGroupSummarizer("http", traceGroupByPath)
	var progressiveTraffic traceTraffic
	for _, event := range progressive {
		progressiveHTTP.observe(event)
		progressiveGroups.observe(event)
		progressiveTraffic.observeHTTP(event)
	}
	progressiveReport := progressiveHTTP.summarize(captureSummary{}, time.Second).(httpTraceReport)
	requestBytes := progressive[0].ByteDelta + sumHTTPByteDeltas(progressive[1:len(progressive)-1])
	responseBytes := progressive[len(progressive)-1].ByteDelta
	if progressiveReport.Requests != 1 || progressiveReport.Responses != 1 || progressiveReport.LatencyAvgMS == nil || progressiveReport.TXBytes != requestBytes || progressiveReport.RXBytes != responseBytes {
		t.Fatalf("progressive report = %+v", progressiveReport)
	}
	group := slices.Collect(maps.Values(progressiveGroups.groups))[0]
	if group.HTTP == nil || group.HTTP.Requests != 1 || group.HTTP.Responses != 1 || group.TXBytes != requestBytes || group.RXBytes != responseBytes {
		t.Fatalf("progressive group = %+v", group)
	}

	rawTracker := newHTTPTracker("", false, false)
	rawMessages := newHTTPMessages(rawTracker, httpMessageMax, false)
	rawMessages.add(packet, 0, start)
	rawFinal := rawMessages.add(httpChunk(strings.Repeat("x", messageLength-1), 1_100_000, true, uint32(len(head)+1)), 0, start)
	rawFinal = append(rawFinal, rawMessages.add(response, 0, start.Add(31*time.Second))...)
	if len(rawFinal) != 2 {
		t.Fatalf("raw final events = %#v", rawFinal)
	}
	var rawTraffic traceTraffic
	for _, event := range rawFinal {
		rawTraffic.observeHTTP(event)
	}
	if rawTraffic.TXBytes != progressiveTraffic.TXBytes || rawTraffic.RXBytes != progressiveTraffic.RXBytes || rawFinal[0].Bytes != progressive[0].Bytes+sumHTTPByteDeltas(progressive[1:len(progressive)-1]) {
		t.Fatalf("raw traffic %+v, progressive traffic %+v", rawTraffic, progressiveTraffic)
	}
}

func TestHTTPMessagesStopFlushPreservesAggregateCountsAndBytes(t *testing.T) {
	const contentLength = 4
	head := "POST /stop HTTP/1.1\r\nHost: api.example\r\nAuthorization: Bearer stop-secret\r\nContent-Length: " + strconv.Itoa(contentLength) + "\r\n\r\n"
	start := time.Unix(100, 0)
	tracker := newHTTPTracker(traceClientSide, false, false)
	messages := newHTTPMessages(tracker, httpMessageMax, false)
	messages.progressive = true
	aggregate := newTraceAggregate("http", traceGroupByPath)
	var sumBytes uint64
	observe := func(events []captureEvent) {
		for _, event := range events {
			aggregate.observe(event)
			sumBytes += event.ByteDelta
		}
	}

	initial := messages.add(httpTestPacket(head+"a", 1_000_000, true), 0, start)
	if len(initial) != 1 || initial[0].Update || initial[0].Final || initial[0].ByteDelta != uint64(len(head)+1) {
		t.Fatalf("initial snapshot = %#v", initial)
	}
	if strings.Contains(initial[0].Payload, "stop-secret") {
		t.Fatalf("secret leaked from initial snapshot: %q", initial[0].Payload)
	}
	observe(initial)

	if events := messages.add(httpChunk("b", 1_100_000, true, uint32(len(head)+1)), 0, start.Add(10*time.Millisecond)); len(events) != 0 {
		t.Fatalf("partial update escaped before stop: %#v", events)
	}
	final := messages.flush()
	if len(final) != 1 || !final[0].Final || !final[0].Update || !final[0].PayloadTruncated || final[0].ByteDelta != 1 || final[0].Bytes != uint64(len(head)+2) {
		t.Fatalf("stop flush = %#v", final)
	}
	if strings.Contains(final[0].Payload, "stop-secret") {
		t.Fatalf("secret leaked from final snapshot: %q", final[0].Payload)
	}
	observe(final)
	if len(messages.open) != 0 || messages.bytes != 0 {
		t.Fatalf("open messages after flush = %d, retained bytes = %d", len(messages.open), messages.bytes)
	}

	group := aggregate.groups[traceGroupByPath].report(captureSummary{}, time.Second)
	expectedBytes := uint64(len(head) + 2)
	if group.Events != 1 || sumBytes != expectedBytes || group.TXBytes != expectedBytes || group.RXBytes != 0 {
		t.Fatalf("aggregate events=%d sumBytes=%d tx=%d rx=%d, want 1/%d/%d/0", group.Events, sumBytes, group.TXBytes, group.RXBytes, expectedBytes, expectedBytes)
	}
}

func sumHTTPByteDeltas(events []captureEvent) uint64 {
	var total uint64
	for _, event := range events {
		total += event.ByteDelta
	}
	return total
}

func TestHTTPMessagesSplitHeaderDoesNotSnapshotPipelinedRequest(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	messages.progressive = true
	now := time.Unix(100, 0)
	first := "POST /first HTTP/1.1\r\nHost: api."
	if events := messages.add(httpTestPacket(first, 1_000_000, true), 0, now); len(events) != 0 {
		t.Fatalf("partial header emitted: %#v", events)
	}
	rest := "example\r\nContent-Length: 1\r\n\r\nxGET /next HTTP/1.1\r\nAuthorization: Bearer secret\r\n\r\n"
	events := messages.add(httpChunk(rest, 1_100_000, true, uint32(len(first))), 0, now.Add(10*time.Millisecond))
	if len(events) != 1 || events[0].Event != traceHTTPRequestEvent || events[0].Target != "api.example" || !events[0].Final || events[0].Update || !strings.HasSuffix(events[0].Payload, "\r\n\r\nx") || strings.Contains(events[0].Payload, "Bearer secret") || strings.Contains(events[0].Payload, "/next") {
		t.Fatalf("pipelined payload escaped message boundary: %#v", events)
	}
}

func TestHTTPMessagesFindTheEndOfEachFraming(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	now := time.Unix(100, 0)
	messages.add(httpTestPacket("HEAD /h HTTP/1.1\r\nHost: x\r\n\r\n", 1_000_000, true), 0, now)
	// HEAD의 응답은 Content-Length가 있어도 본문이 없다.
	head := messages.add(httpTestPacket("HTTP/1.1 200 OK\r\nContent-Length: 99\r\n\r\n", 2_000_000, false), 0, now)
	if len(head) != 1 || head[0].PayloadTruncated {
		t.Fatalf("HEAD response = %#v", head)
	}
	messages.add(httpTestPacket("GET /c HTTP/1.1\r\nHost: x\r\n\r\n", 3_000_000, true), 0, now)
	chunked := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nwiki\r\n"
	if events := messages.add(httpTestPacket(chunked, 4_000_000, false), 0, now); len(events) != 0 {
		t.Fatalf("chunked response ended early: %#v", events)
	}
	events := messages.add(httpChunk("5;x=y\r\npedia\r\n0\r\nTrailer: v\r\n\r\n", 4_100_000, false, uint32(len(chunked))), 0, now)
	if len(events) != 1 || events[0].PayloadTruncated || !strings.HasSuffix(events[0].Payload, "pedia\r\n0\r\nTrailer: v\r\n\r\n") {
		t.Fatalf("chunked response = %#v", events)
	}
}

func TestHTTPMessagesMarkMessagesThatAreNotWhole(t *testing.T) {
	now := time.Unix(100, 0)
	head := "POST /a HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n"

	// 조각 하나를 잃으면 구멍을 건너뛰어 잇지 않는다.
	messages := newTestHTTPMessages(false, httpMessageMax)
	messages.add(httpTestPacket(head+"abc", 1_000_000, true), 0, now)
	lost := messages.add(httpChunk("zzz", 1_100_000, true, uint32(len(head)+10)), 0, now)
	if len(lost) != 1 || !lost[0].PayloadTruncated || !strings.HasSuffix(lost[0].Payload, "\r\n\r\nabc") {
		t.Fatalf("lost chunk = %#v", lost)
	}

	// 상한에 닿으면 기다리지 않고 잘린 message로 낸다.
	messages = newTestHTTPMessages(false, len(head)+20)
	capped := messages.add(httpTestPacket(head+strings.Repeat("x", 40), 1_000_000, true), 0, now)
	if len(capped) != 1 || !capped[0].PayloadTruncated || capped[0].Bytes != uint64(len(head)+20) {
		t.Fatalf("capped = %#v", capped)
	}

	// 서버가 본문을 다 읽기 전에 응답하면 요청은 잘린 채로 응답보다 먼저 나온다.
	messages = newTestHTTPMessages(false, httpMessageMax)
	messages.add(httpTestPacket(head+"abc", 1_000_000, true), 0, now)
	early := messages.add(httpTestPacket("HTTP/1.1 413 Payload Too Large\r\nContent-Length: 0\r\n\r\n", 2_000_000, false), 0, now)
	if len(early) != 2 || early[0].Event != traceHTTPRequestEvent || !early[0].PayloadTruncated || early[1].Status != 413 || early[1].PayloadTruncated {
		t.Fatalf("early response = %#v", early)
	}

	// trace가 끝나면 남은 message를 잘린 것으로 낸다.
	messages = newTestHTTPMessages(false, httpMessageMax)
	messages.add(httpTestPacket(head, 1_000_000, true), 0, now)
	if flushed := messages.flush(); len(flushed) != 1 || !flushed[0].PayloadTruncated || len(messages.open) != 0 || messages.bytes != 0 {
		t.Fatalf("flush = %#v, open %d, bytes %d", flushed, len(messages.open), messages.bytes)
	}
}

func TestHTTPMessagesWaitForResponsesWithoutALength(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	now := time.Unix(100, 0)
	messages.add(httpTestPacket("GET / HTTP/1.0\r\nHost: x\r\n\r\n", 1_000_000, true), 0, now)
	if events := messages.add(httpTestPacket("HTTP/1.0 200 OK\r\n\r\nhello", 2_000_000, false), 0, now); len(events) != 0 {
		t.Fatalf("response without a length came out early: %#v", events)
	}
	messages.add(httpChunk(" world", 2_100_000, false, uint32(len("HTTP/1.0 200 OK\r\n\r\nhello"))), 0, now.Add(time.Second/2))
	if events := messages.expire(now.Add(time.Second)); len(events) != 0 {
		t.Fatalf("expired while data still arrived: %#v", events)
	}
	events := messages.expire(now.Add(2 * time.Second))
	if len(events) != 1 || !events[0].PayloadTruncated || !strings.HasSuffix(events[0].Payload, "hello world") {
		t.Fatalf("expired response = %#v", events)
	}
}

func TestHTTPMessagesKeepTheRequestOpenAcrossAnInterimResponse(t *testing.T) {
	messages := newTestHTTPMessages(false, httpMessageMax)
	now := time.Unix(100, 0)
	head := "POST /a HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 4\r\n\r\n"
	messages.add(httpTestPacket(head, 1_000_000, true), 0, now)
	if interim := messages.add(httpTestPacket("HTTP/1.1 100 Continue\r\n\r\n", 1_500_000, false), 0, now); len(interim) != 0 {
		t.Fatalf("interim came out before the request: %#v", interim)
	}
	// 1xx는 요청 event 바로 뒤에 나온다.
	events := messages.add(httpChunk("body", 1_600_000, true, uint32(len(head))), 0, now)
	if len(events) != 2 || events[0].PayloadTruncated || !strings.HasSuffix(events[0].Payload, "\r\n\r\nbody") || events[1].Status != 100 {
		t.Fatalf("events = %#v", events)
	}
}

func TestHTTPMessagesShowSecretsWhenAsked(t *testing.T) {
	messages := newHTTPMessages(newHTTPTracker(traceClientSide, false, true), httpMessageMax, true)
	events := messages.add(httpTestPacket("GET / HTTP/1.1\r\nAuthorization: Bearer t0ken\r\n\r\n", 1_000_000, true), 0, time.Unix(100, 0))
	if len(events) != 1 || !strings.Contains(events[0].Payload, "Bearer t0ken") {
		t.Fatalf("events = %#v", events)
	}
	if got := traceHTTPPayload([]byte("GET / HTTP/1.1\r\nCookie: a=b\r\n\r\n"), true); !strings.Contains(got, "Cookie: a=b") {
		t.Fatalf("payload with secrets = %q", got)
	}
}

func TestTraceHTTPPayloadBlockAndTrim(t *testing.T) {
	event := captureEvent{Payload: "POST / HTTP/1.1\r\nHost: x\r\n\r\n{\"k\":1}\n", PayloadTruncated: true}
	lines := traceHTTPPayloadBlock(event)
	if strings.Join(lines, "|") != "POST / HTTP/1.1|Host: x||{\"k\":1}|[truncated]" {
		t.Fatalf("block = %q", lines)
	}
	if got := traceHTTPPayloadSummary(event); !strings.HasPrefix(got, "[truncated] body ") {
		t.Fatalf("summary = %q", got)
	}
	// 한글은 3바이트라 4바이트에서 자르면 둘째 글자 앞에서 끊어야 한다.
	if got := traceTrimText("한글", 4); got != "한" {
		t.Fatalf("trim = %q", got)
	}
}

func TestTracePayloadAllOptions(t *testing.T) {
	for _, args := range [][]string{{"http", "--payload=bogus"}, {"http", "--payload=false", "--show-secrets"}, {"http", "--payload", "all"}, {"http", "--payload=all", "--json", "-"}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
	scope := (tcpTraceOptions{payload: tracePayloadAll, showSecrets: true}).scope("http")
	if !scope.payload || !scope.payloadAll || !scope.showSecrets {
		t.Fatalf("scope = %+v", scope)
	}
	if scope := (tcpTraceOptions{payload: tracePayloadHead}).scope("http"); !scope.payload || scope.payloadAll {
		t.Fatalf("head scope = %+v", scope)
	}
}

func TestHTTPSplitStartsJoinARequestLineReadInTwoParts(t *testing.T) {
	starts := httpSplitStarts{}
	// caddy는 요청의 첫 14 byte를 먼저 읽는다.
	if _, ok := starts.join(httpTestPacket("GET /strace-pr", 1_000_000, false)); ok {
		t.Fatal("a start without the end of its first line came out")
	}
	joined, ok := starts.join(httpChunk("obe HTTP/1.1\r\nHost: edc-proxy:8080\r\n\r\n", 1_100_000, false, 14))
	if !ok || joined.continued || joined.bootTimeNS != 1_000_000 || len(starts) != 0 {
		t.Fatalf("joined = %+v, ok %v, held %d", joined, ok, len(starts))
	}
	event, ok := newHTTPTracker(traceServerSide, false, false).event(joined, 0)
	if !ok || event.Path != "/strace-probe" || event.Target != "edc-proxy:8080" || event.Side != traceServerSide {
		t.Fatalf("event = %+v", event)
	}

	status, _ := starts.join(httpTestPacket("HTTP/1.1 20", 2_000_000, true))
	if status.payload != nil {
		t.Fatalf("a cut status line came out: %q", status.payload)
	}
	if joined, ok := starts.join(httpChunk("0 OK\r\n\r\n", 2_100_000, true, 11)); !ok || string(joined.payload) != "HTTP/1.1 200 OK\r\n\r\n" {
		t.Fatalf("status = %q, %v", joined.payload, ok)
	}
}

// 연결이 끝나면 기다리던 첫 조각을 지운다. 같은 주소를 쓰는 새 연결의 조각이 같은 위치로 와도 잇지 않는다.
func TestHTTPSplitStartsForgetAClosedSocket(t *testing.T) {
	starts := httpSplitStarts{}
	if _, ok := starts.join(httpTestPacket("GET /strace-pr", 1_000_000, false)); ok {
		t.Fatal("a start without the end of its first line came out")
	}
	starts.forget(1)
	if len(starts) != 0 {
		t.Fatalf("held = %d", len(starts))
	}
	piece, ok := starts.join(httpChunk("obe HTTP/1.1\r\n\r\n", 2_000_000, false, 14))
	if !ok || !piece.continued || string(piece.payload) != "obe HTTP/1.1\r\n\r\n" {
		t.Fatalf("piece = %+v, %t", piece, ok)
	}
}

func TestHTTPSplitStartsPassOtherPackets(t *testing.T) {
	starts := httpSplitStarts{}
	whole := httpTestPacket("GET / HTTP/1.1\r\n\r\n", 1, true)
	if got, ok := starts.join(whole); !ok || string(got.payload) != string(whole.payload) {
		t.Fatalf("a whole start = %q, %v", got.payload, ok)
	}
	// --payload=all의 본문 조각은 기다리는 첫 조각이 없으면 그대로 간다.
	if got, ok := starts.join(httpChunk("body", 2, true, 18)); !ok || !got.continued {
		t.Fatalf("a body chunk = %+v, %v", got, ok)
	}
	// 사이 조각을 잃으면 잇지 않는다.
	starts.join(httpTestPacket("GET /a", 3, true))
	if _, ok := starts.join(httpChunk(" HTTP/1.1\r\n\r\n", 4, true, 9)); ok || len(starts) != 0 {
		t.Fatalf("a chunk after a gap was joined, held %d", len(starts))
	}
	// 새 message가 시작하면 기다리던 첫 조각은 버린다.
	starts.join(httpTestPacket("GET /a", 5, true))
	if got, ok := starts.join(httpTestPacket("GET /b HTTP/1.1\r\n\r\n", 6, true)); !ok || !strings.HasPrefix(string(got.payload), "GET /b") || len(starts) != 0 {
		t.Fatalf("new start = %q, %v, held %d", got.payload, ok, len(starts))
	}
	// 첫 줄이 4KiB 안에 끝나지 않으면 더 기다리지 않는다.
	starts.join(httpTestPacket("GET /", 8, true))
	if got, ok := starts.join(httpChunk(strings.Repeat("a", httpPayloadHead), 9, true, 5)); !ok || len(got.payload) != httpPayloadHead+5 || len(starts) != 0 {
		t.Fatalf("a long first line: %d bytes, %v, held %d", len(got.payload), ok, len(starts))
	}
	for socket := range uint64(httpSplitLimit + 1) {
		packet := httpTestPacket("GET /a", 7, true)
		packet.socket = socket
		starts.join(packet)
	}
	if len(starts) > httpSplitLimit {
		t.Fatalf("held %d starts, limit %d", len(starts), httpSplitLimit)
	}
}

func TestHTTPMessagesTakeARequestWhoseFirstLineWasSplit(t *testing.T) {
	starts, messages, now := httpSplitStarts{}, newHTTPMessages(newHTTPTracker(traceServerSide, false, false), httpMessageMax, false), time.Unix(100, 0)
	var events []captureEvent
	first, second := "POST /uplo", "ad HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\n\r\nab"
	for _, packet := range []httpPacket{
		httpTestPacket(first, 1_000_000, false),
		httpChunk(second, 1_100_000, false, uint32(len(first))),
		httpChunk("cd", 1_200_000, false, uint32(len(first+second))),
	} {
		if packet, ok := starts.join(packet); ok {
			events = append(events, messages.add(packet, 0, now)...)
		}
	}
	if len(events) != 1 || events[0].Path != "/upload" || events[0].PayloadTruncated || !strings.HasSuffix(events[0].Payload, "\r\n\r\nabcd") {
		t.Fatalf("events = %#v", events)
	}
}
