package edc

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func traceScreenKey(model traceScreenModel, keys ...string) traceScreenModel {
	for _, key := range keys {
		code := map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter, "esc": tea.KeyEscape, "end": tea.KeyEnd, "home": tea.KeyHome, "space": tea.KeySpace}[key]
		press := tea.KeyPressMsg{Code: code}
		if code == 0 {
			press = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		next, _ := model.Update(press)
		model = next.(traceScreenModel)
	}
	return model
}

func TestTraceScreenSelectsAnEventAndShowsAllOfIt(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 12
	body := strings.Repeat("Z", 6000)
	events := []captureEvent{
		{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "GET", Target: "api.example", Path: "/a", Payload: "GET /a HTTP/1.1\r\nHost: api.example\r\n\r\n"},
		{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "POST", Target: "api.example", Path: "/b", Payload: "POST /b HTTP/1.1\r\nHost: api.example\r\n\r\n" + body},
		{Protocol: "http", Event: "http_2xx", Process: "curl", Target: "api.example", Path: "/b", Status: 200, Payload: "HTTP/1.1 200 OK\r\n\r\nok"},
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	if got := len(model.events[1].Payload); got != httpPayloadHead {
		t.Fatalf("the list kept %d bytes of the payload, want %d", got, httpPayloadHead)
	}

	model = traceScreenKey(model, "up", "up")
	if model.selected != 1 {
		t.Fatalf("selected = %d, want the POST request", model.selected)
	}
	if header := traceScreenHeader(model)[0]; !strings.Contains(header, "paused, 1 newer") {
		t.Fatalf("header = %q", header)
	}
	// 화면에 자리가 남으면 고른 event 아래에 더 새 event가 온다.
	rows := strings.Join(traceScreenRows(model), "\n")
	if selected, newer := strings.Index(rows, "POST api.example/b"), strings.Index(rows, "http_2xx"); selected < 0 || newer < selected {
		t.Fatalf("rows = %q", rows)
	}

	model = traceScreenKey(model, "enter")
	if model.detail == nil {
		t.Fatal("enter did not open the detail view")
	}
	text := strings.Join(model.detail.lines, "")
	if strings.Count(text, "Z") != len(body) || !strings.Contains(text, `"event": "http_request"`) || !strings.Contains(text, `"path": "/b"`) {
		t.Fatalf("the detail view does not have the whole message: %d Z, %q", strings.Count(text, "Z"), text[:min(len(text), 300)])
	}
	for _, line := range model.detail.lines {
		if liveWidth(line) > model.width {
			t.Fatalf("detail line is %d wide: %q", liveWidth(line), line)
		}
	}
	page := model.height - 3
	model = traceScreenKey(model, "end")
	if model.detail.offset != len(model.detail.lines)-page {
		t.Fatalf("end offset = %d, want %d", model.detail.offset, len(model.detail.lines)-page)
	}
	if view := traceScreenDetailView(model); len(view) != model.height || !strings.Contains(view[0], "detail") {
		t.Fatalf("detail view = %q", view)
	}
	model = traceScreenKey(model, "home", "esc")
	if model.detail != nil || model.selected != 1 {
		t.Fatalf("esc: detail %v, selected %d", model.detail, model.selected)
	}

	// q는 상세 보기에서는 돌아가기만 하고, 목록에서 End를 누르면 다시 새 event를 따라간다.
	model = traceScreenKey(model, "enter", "q")
	if model.detail != nil || model.stopping {
		t.Fatalf("q in the detail view: detail %v, stopping %t", model.detail, model.stopping)
	}
	model = traceScreenKey(model, "down", "down", "end")
	if model.selected != -1 || !strings.Contains(traceScreenHeader(model)[0], "live") {
		t.Fatalf("end: selected %d", model.selected)
	}
}

func TestTraceScreenSelectionFollowsTheFilterAndKeptEvents(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.first = 100
	model.events = []captureEvent{{Process: "curl"}, {Process: "wget"}, {Process: "curl"}, {Process: "wget"}}
	model.filter = "curl"
	if got := model.moveSelection(-1, 1); got != 102 {
		t.Fatalf("up from live = %d, want 102", got)
	}
	model.selected = 102
	if got := model.moveSelection(-1, 5); got != 100 {
		t.Fatalf("page up = %d, want the oldest match 100", got)
	}
	if got := model.moveSelection(1, 1); got != 102 {
		t.Fatalf("down at the newest match = %d, want 102", got)
	}
	// filter를 새로 적용하면 고른 것을 푼다.
	model.selected = 102
	model.filtering = true
	model.input.SetValue("curl")
	next, _ := model.updateKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(traceScreenModel)
	if model.selected != -1 {
		t.Fatalf("applying a filter kept the selection %d", model.selected)
	}
	// Enter는 고른 event가 없으면 가장 최근 event를 고른다.
	model.selected = -1
	model.openDetail()
	if model.selected != 102 || model.detail == nil {
		t.Fatalf("enter from live: selected %d", model.selected)
	}
}

func TestTraceFullPayloadsKeepOnlyRecentLongPayloads(t *testing.T) {
	payloads := traceFullPayloads{minimum: httpPayloadHead}
	payloads.keep(1, "short")
	if _, ok := payloads.get(1); ok {
		t.Fatal("a payload that the list keeps whole was stored again")
	}
	// 같은 문자열을 여러 번 넣어도 메모리는 한 벌이라 시험이 가볍다.
	big := strings.Repeat("y", 5<<20)
	for number := 10; number < 24; number++ {
		payloads.keep(number, big)
	}
	if _, ok := payloads.get(10); ok || payloads.bytes > traceScreenPayloadBytes {
		t.Fatalf("the oldest payload is still there, bytes %d", payloads.bytes)
	}
	if _, ok := payloads.get(23); !ok {
		t.Fatal("the newest payload is gone")
	}
	payloads.drop(20)
	if _, ok := payloads.get(19); ok || len(payloads.order) != 4 {
		t.Fatalf("drop kept older payloads: %v", payloads.order)
	}

	// 전체 payload가 없는 오래된 event는 앞 4KiB만 있다고 알린다.
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.events = []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Payload: traceTrimText("POST / HTTP/1.1\r\n\r\n"+big, httpPayloadHead)}}
	detail := model.buildDetail(0)
	if !strings.Contains(strings.Join(detail.lines, "\n"), "only the first 4096 bytes") {
		t.Fatal("the detail view does not say that the payload is cut")
	}
}

func TestTraceWrapLine(t *testing.T) {
	if got := traceWrapLine("abcdefg", 3); strings.Join(got, "|") != "abc|def|g" {
		t.Fatalf("ascii = %q", got)
	}
	// 한글은 두 칸이라 4칸에 두 글자씩 들어간다.
	if got := traceWrapLine("한글한글한", 4); strings.Join(got, "|") != "한글|한글|한" {
		t.Fatalf("wide = %q", got)
	}
	if got := traceWrapLine("", 4); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestTraceFitCutsLongLinesLikeBefore(t *testing.T) {
	slow := func(value string, width int) string {
		if liveWidth(value) <= width {
			return value
		}
		return strings.TrimRight(slowTruncateLine(value, width), "\n")
	}
	for _, value := range []string{"", "short", strings.Repeat("a", 72), strings.Repeat("a", 73), strings.Repeat("x", 1200), strings.Repeat("한글", 100), "한a글b" + strings.Repeat("c", 200)} {
		for _, width := range []int{10, 72, 110} {
			if got, want := traceFit(value, width), slow(value, width); got != want {
				t.Fatalf("traceFit(%d chars, %d) = %q, want %q", len(value), width, got, want)
			}
		}
	}
	// 4KiB 본문 한 줄에 200ms가 넘게 걸리던 것을 막는다. 느린 CI를 생각해 상한을 넉넉히 둔다.
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Payload: "POST / HTTP/1.1\r\n\r\n" + strings.Repeat(`{"sku": "A-1"}, `, 256)}
	started := time.Now()
	for range 100 {
		formatTraceScreenPayload(event, 110)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("100 payload rows took %s", elapsed)
	}
}

func TestTraceScreenFilterMatchesTheShownDestination(t *testing.T) {
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "POST", Target: "127.0.0.1:18090", Path: "/orders", Destination: "127.0.0.1:18090"}
	for filter, want := range map[string]bool{"orders": true, "post 127.0.0.1:18090/ord": true, "curl": true, "/missing": false} {
		if got := traceEventMatchesText(event, filter); got != want {
			t.Fatalf("filter %q = %t, want %t", filter, got, want)
		}
	}
}

func traceGzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestTraceScreenScopeCollectsHTTPPayloads(t *testing.T) {
	scope := traceScreenScope("http", tcpTraceOptions{})
	if !scope.payload || scope.payloadAll || !scope.showSecrets || !scope.keepGzip {
		t.Fatalf("http screen scope = %+v", scope)
	}
	if scope := traceScreenScope("http", tcpTraceOptions{payload: tracePayloadAll}); !scope.payloadAll {
		t.Fatalf("--payload=all was lost: %+v", scope)
	}
	if scope := traceScreenScope("tcp", tcpTraceOptions{}); scope.payload || scope.showSecrets || scope.keepGzip {
		t.Fatalf("tcp screen scope = %+v", scope)
	}
}

func TestTraceScreenKeysShowPayloadsAndSecrets(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 120, 12
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "GET", Target: "api.example", Path: "/me", Payload: "GET /me HTTP/1.1\r\nHost: api.example\r\nAuthorization: Bearer t0ken\r\n\r\n"}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
	model = next.(traceScreenModel)
	rows := strings.Join(traceScreenRows(model), "\n")
	if strings.Contains(rows, "↳") {
		t.Fatalf("payload lines are on without --payload: %q", rows)
	}
	model = traceScreenKey(model, "v")
	rows = strings.Join(traceScreenRows(model), "\n")
	if !strings.Contains(rows, "Authorization: ***") || strings.Contains(rows, "t0ken") {
		t.Fatalf("v did not show a masked payload line: %q", rows)
	}
	model = traceScreenKey(model, "m")
	if rows := strings.Join(traceScreenRows(model), "\n"); !strings.Contains(rows, "Bearer t0ken") || !strings.Contains(traceScreenHeader(model)[0], "secrets shown") {
		t.Fatalf("m did not show the secret: %q", rows)
	}
	model = traceScreenKey(model, "m", "enter")
	if text := strings.Join(model.detail.lines, "\n"); !strings.Contains(text, "Authorization: ***") {
		t.Fatalf("the detail view shows the secret: %q", text)
	}
	model = traceScreenKey(model, "m")
	if text := strings.Join(model.detail.lines, "\n"); !strings.Contains(text, "Bearer t0ken") || !strings.Contains(traceScreenDetailView(model)[0], "secrets shown") {
		t.Fatalf("m in the detail view did not show the secret: %q", text)
	}
}

func TestTraceScreenFollowShowsTheNewestEvent(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 12
	send := func(path string) {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "x", Path: path}}})
		model = next.(traceScreenModel)
	}
	send("/one")
	model = traceScreenKey(model, "f")
	if model.detail == nil || !model.follow || model.detail.number != 0 {
		t.Fatalf("f did not open the newest event: %+v", model.detail)
	}
	send("/two")
	if model.detail.number != 1 || !strings.Contains(strings.Join(model.detail.lines, "\n"), `"/two"`) {
		t.Fatalf("follow did not move to the new event: %d", model.detail.number)
	}
	if !strings.Contains(traceScreenDetailView(model)[0], "follow") {
		t.Fatal("the detail header does not say follow")
	}
	// f를 다시 누르면 보던 event에서 멈추고, 새 event가 와도 그대로 둔다.
	model = traceScreenKey(model, "f")
	send("/three")
	if model.follow || model.detail.number != 1 || model.selected != 1 {
		t.Fatalf("stopped follow: follow %t, number %d, selected %d", model.follow, model.detail.number, model.selected)
	}
	model = traceScreenKey(model, "f", "esc")
	if model.detail != nil || model.follow || model.selected != -1 {
		t.Fatalf("esc from follow: detail %v, follow %t, selected %d", model.detail, model.follow, model.selected)
	}
}

func TestTraceScreenDecodesGzipBodies(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 30
	body := `{"message":"hello from a gzip body"}`
	compressed := traceGzipBytes(t, []byte(body))
	raw := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: "+strconv.Itoa(len(compressed))+"\r\n\r\n"), compressed...)
	chunked := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nTransfer-Encoding: chunked\r\n\r\n"), []byte(fmt.Sprintf("%x\r\n", 10))...)
	chunked = append(append(chunked, compressed[:10]...), []byte(fmt.Sprintf("\r\n%x\r\n", len(compressed)-10))...)
	chunked = append(append(chunked, compressed[10:]...), "\r\n0\r\n\r\n"...)
	var long strings.Builder
	for index := range 4000 {
		fmt.Fprintf(&long, "partial %d %x, ", index, index*7919)
	}
	cut := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"), traceGzipBytes(t, []byte(long.String()))[:2000]...)
	var events []captureEvent
	for _, message := range [][]byte{raw, chunked, cut, []byte("HTTP/1.1 200 OK\r\n\r\nplain")} {
		event := captureEvent{Protocol: "http", Event: "http_2xx", Status: 200, Payload: traceHTTPPayload(message, true)}
		if httpGzipped(message) {
			event.gzipped = message
		}
		events = append(events, event)
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	model.decode = true
	for number, want := range []string{body, body, "partial 5 9aab", ""} {
		text := strings.Join(model.buildDetail(number).raw, "\n")
		if !strings.Contains(text, want) {
			t.Fatalf("event %d: decoded text does not have %q: %q", number, want, text[:min(len(text), 400)])
		}
		note := map[int]string{0: "bytes decoded", 1: "bytes decoded", 2: "was not captured", 3: "not gzip"}[number]
		if !strings.Contains(text, note) {
			t.Fatalf("event %d: note %q missing: %q", number, note, text)
		}
	}
	// 풀린 크기에는 상한이 있다.
	bomb := append([]byte("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"), traceGzipBytes(t, make([]byte, traceGunzipLimit+1024))...)
	if text, note := traceGunzipMessage(bomb); len(text) > 4*traceGunzipLimit+200 || !strings.Contains(note, "cut at") {
		t.Fatalf("gzip bomb: %d bytes, note %q", len(text), note)
	}
}

func TestHTTPTrackerKeepsRawBytesOfGzipMessagesWhenAsked(t *testing.T) {
	message := "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: 3\r\n\r\n\x1f\x8b\x08"
	tracker := newHTTPTracker(traceClientSide, true, false)
	if event, _ := tracker.event(httpTestPacket(message, 1, false), 0); event.gzipped != nil {
		t.Fatal("kept raw bytes without keepGzip")
	}
	tracker.keepGzip = true
	if event, _ := tracker.event(httpTestPacket(message, 2, false), 0); string(event.gzipped) != message {
		t.Fatalf("head mode raw = %q", event.gzipped)
	}
	if event, _ := tracker.event(httpTestPacket("HTTP/1.1 200 OK\r\n\r\nplain", 3, false), 0); event.gzipped != nil {
		t.Fatal("kept raw bytes of a message without gzip")
	}
	messages := newHTTPMessages(tracker, httpMessageMax, false)
	if events := messages.add(httpTestPacket(message, 4, false), 0, time.Unix(100, 0)); len(events) != 1 || string(events[0].gzipped) != message {
		t.Fatalf("--payload=all raw = %v", events)
	}
}

func TestTraceScreenKeepsAFullListWhileItWaits(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 13
	batch := func(from, count int) []captureEvent {
		events := make([]captureEvent, count)
		for index := range events {
			events[index] = captureEvent{Protocol: "tcp", Event: "tcp_send", Process: fmt.Sprintf("p%d", from+index), Destination: "10.0.0.1:80"}
		}
		return events
	}
	next, _ := model.Update(traceEventMsg{events: batch(0, 20)})
	model = traceScreenKey(next.(traceScreenModel), "up")
	// 멈춘 동안 event가 목록 상한을 넘게 쌓이면 고른 event가 가장 오래된 event로 밀린다. 그래도 화면은 가득 찬다.
	for from := 20; from < 20+traceScreenEventLimit+500; from += traceEventBatchLimit {
		next, _ = model.Update(traceEventMsg{events: batch(from, traceEventBatchLimit)})
		model = next.(traceScreenModel)
	}
	if model.selected != model.first {
		t.Fatalf("selected %d, first %d", model.selected, model.first)
	}
	rows := traceScreenRows(model)
	filled := 0
	for _, row := range rows {
		if strings.TrimSpace(row) != "" {
			filled++
		}
	}
	if filled != model.height-3 || !strings.HasPrefix(rows[0], fmt.Sprintf("p%d ", model.first)) {
		t.Fatalf("%d of %d rows are filled, first row %q", filled, model.height-3, rows[0])
	}
}

func TestTraceScreenEscAndLFollowAgain(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.events = []captureEvent{{Process: "a"}, {Process: "b"}, {Process: "c"}}
	model.filter = "b"
	// Esc는 먼저 선택을 풀고, 한 번 더 누르면 filter를 지운다.
	model = traceScreenKey(model, "up")
	if model.selected != 1 {
		t.Fatalf("selected = %d", model.selected)
	}
	model = traceScreenKey(model, "esc")
	if model.selected != -1 || model.filter != "b" {
		t.Fatalf("first esc: selected %d, filter %q", model.selected, model.filter)
	}
	model = traceScreenKey(model, "esc")
	if model.filter != "" {
		t.Fatalf("second esc kept the filter %q", model.filter)
	}
	model = traceScreenKey(model, "up", "up", "l")
	if model.selected != -1 {
		t.Fatalf("l did not follow again: %d", model.selected)
	}
	// b와 space는 PgUp, PgDn과 같다.
	model = traceScreenKey(model, "up", "b")
	if model.selected != 0 {
		t.Fatalf("b = %d, want the oldest", model.selected)
	}
	model = traceScreenKey(model, "space")
	if model.selected != 2 {
		t.Fatalf("space = %d, want the newest", model.selected)
	}
}
