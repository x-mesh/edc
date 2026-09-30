package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func traceScreenKey(model traceScreenModel, keys ...string) traceScreenModel {
	for _, key := range keys {
		code := map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter, "esc": tea.KeyEscape, "end": tea.KeyEnd, "home": tea.KeyHome}[key]
		press := tea.KeyPressMsg{Code: code}
		if code == 0 {
			press = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		next, _ := model.updateKey(press)
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
	// 고른 event가 맨 아래에 오고, 그보다 새 event는 보이지 않는다.
	rows := strings.Join(traceScreenRows(model), "\n")
	if !strings.Contains(rows, "POST api.example/b") || strings.Contains(rows, "http_2xx") {
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
	var payloads traceFullPayloads
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

	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Payload: traceTrimText("POST / HTTP/1.1\r\n\r\n"+big, httpPayloadHead)}
	detail := newTraceDetail(event, "", false, 80)
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
		return strings.TrimRight(truncateLine(value, width), "\n")
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
