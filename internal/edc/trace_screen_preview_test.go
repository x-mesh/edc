package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestTraceScreenSplitShowsTheSelectedMessage(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 60, 20
	body := strings.Repeat("Z", 3000)
	at := uint64(time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local).UnixNano())
	events := []captureEvent{
		{TimestampNS: at, Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "POST", Target: "api.example", Path: "/b", Payload: "POST /b HTTP/1.1\r\nAuthorization: Bearer t0ken\r\n\r\n" + body},
		{TimestampNS: at + uint64(100*time.Millisecond), Protocol: "http", Event: "http_2xx", Process: "curl", Target: "api.example", Path: "/b", Status: 200, Payload: "HTTP/1.1 200 OK\r\n\r\nok"},
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = traceScreenKey(next.(traceScreenModel), "i")

	rows := traceScreenRows(model)
	list, _, _ := traceSplitHeights(model.height)
	if len(rows) != model.height-3 || !strings.HasPrefix(rows[list], "── 12:00:00.100  curl  http://api.example/b  http_2xx") || !strings.Contains(strings.Join(rows[list:], "\n"), "HTTP/1.1 200 OK") {
		t.Fatalf("with nothing selected the preview must show the newest event: %q", rows)
	}
	for _, row := range rows {
		if liveWidth(row) > model.width {
			t.Fatalf("row is %d wide: %q", liveWidth(row), row)
		}
	}

	model = traceScreenKey(model, "up", "up")
	preview := strings.Join(traceScreenRows(model)[list:], "\n")
	if !strings.Contains(preview, "POST /b HTTP/1.1") || strings.Contains(preview, "t0ken") || !strings.HasPrefix(preview, "── more below  ·  12:00:00.000  curl") {
		t.Fatalf("preview of the selected request = %q", preview)
	}
	model = traceScreenKey(model, "J", "J")
	if preview := strings.Join(traceScreenRows(model)[list:], "\n"); model.previewOffset != 2 || !strings.HasPrefix(preview, "── line 3  ·  more below") || strings.Contains(preview, "POST /b") {
		t.Fatalf("J did not scroll the preview: offset %d", model.previewOffset)
	}
	// 가리기를 바꾸면 보던 위치를 두고, 다른 event를 고르면 맨 위부터 보인다.
	model = traceScreenKey(model, "m")
	if model.previewOffset != 2 || !strings.Contains(strings.Join(model.preview.lines, "\n"), "t0ken") {
		t.Fatalf("m: offset %d, lines %q", model.previewOffset, model.preview.lines)
	}
	model = traceScreenKey(model, "down")
	if model.previewOffset != 0 || model.preview.number != 1 {
		t.Fatalf("down: offset %d, preview of event %d", model.previewOffset, model.preview.number)
	}

	model = traceScreenKey(model, "i")
	if rows := traceScreenRows(model); model.preview != nil || strings.Contains(strings.Join(rows, "\n"), "── ") {
		t.Fatalf("i did not close the preview: %q", rows)
	}
}

func TestTraceScreenSplitScrollAndSmallScreens(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 80, 30
	next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "tcp", Event: "connect", Process: "curl", Target: "api.example"}}})
	model = traceScreenKey(next.(traceScreenModel), "i")
	// payload가 없는 event는 필드를 보인다. 창에 다 들어가면 J로 내려가지 않는다.
	if preview := strings.Join(model.preview.lines, "\n"); !strings.Contains(preview, `"event": "connect"`) {
		t.Fatalf("preview = %q", preview)
	}
	model = traceScreenKey(model, "J", "J", "J")
	if model.previewOffset != 0 {
		t.Fatalf("J moved past the end: offset %d", model.previewOffset)
	}
	model.height = 8
	if rows := traceScreenRows(model); len(rows) != model.height-3 || strings.Contains(strings.Join(rows, "\n"), "── ") {
		t.Fatalf("a small screen must show only the list: %q", rows)
	}
	if model = traceScreenKey(model, "J"); model.previewOffset != 0 {
		t.Fatalf("J on a small screen moved the hidden preview: offset %d", model.previewOffset)
	}
}

func TestTraceScreenClampsPreviewOffsetAfterResize(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 12, 20
	next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Payload: strings.Repeat("x", 400)}}})
	model = traceScreenKey(next.(traceScreenModel), "i")
	for range 100 {
		model = traceScreenKey(model, "J")
	}
	if model.previewOffset == 0 {
		t.Fatal("scroll did not move the preview")
	}
	next, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 8})
	model = next.(traceScreenModel)
	if model.previewOffset != 0 {
		t.Fatalf("preview offset after resize = %d", model.previewOffset)
	}
}

func TestTraceWrapWindow(t *testing.T) {
	lines := []string{"abcdef", "", "xy"}
	for _, test := range []struct {
		offset, count int
		want          string
		more          bool
	}{
		{0, 3, "abc|def|", true},
		{0, 4, "abc|def||xy", false},
		{1, 2, "def|", true},
		{2, 5, "|xy", false},
		{9, 2, "", false},
		{0, 0, "", false},
	} {
		got, more := traceWrapWindow(lines, 3, test.offset, test.count)
		if strings.Join(got, "|") != test.want || more != test.more {
			t.Fatalf("window(%d, %d) = %q, %v, want %q, %v", test.offset, test.count, got, more, test.want, test.more)
		}
	}
}

func TestTraceScreenShowsEventTimes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	at := time.Date(2026, 9, 30, 9, 5, 7, 123_000_000, time.Local)
	event := captureEvent{TimestampNS: uint64(at.UnixNano()), Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "GET", Target: "api.example", Path: "/a", Source: "127.0.0.1:40000", Payload: "GET /a HTTP/1.1\r\n\r\n"}
	if got := traceEventClock(event); got != "09:05:07.123" {
		t.Fatalf("clock = %q", got)
	}
	if got := traceEventClock(captureEvent{}); got != "-" {
		t.Fatalf("clock without a time = %q", got)
	}
	if title := newTraceDetail(event, "", nil, 0, 80).title; !strings.HasPrefix(title, "09:05:07.123  curl  GET http://api.example/a") {
		t.Fatalf("detail title = %q", title)
	}

	// TIME 칸은 넓은 화면에만 있고, 머리글과 행의 칸이 같은 곳에서 시작한다.
	for _, width := range []int{traceScrollTimeMinWidth - 1, traceScrollTimeMinWidth, 200} {
		model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadHead}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height = width, 8
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
		header, rows := traceScreenHeader(model)[2], traceScreenRows(model)
		if wide := width >= traceScrollTimeMinWidth; strings.HasPrefix(header, "TIME") != wide || strings.HasPrefix(rows[0], "09:05:07.123") != wide {
			t.Fatalf("width %d: header %q, row %q", width, header, rows[0])
		}
		for _, column := range []string{"DESTINATION", "EVENT", "SOURCE"} {
			value := map[string]string{"DESTINATION": "GET http://api.example/a", "EVENT": traceHTTPRequestEvent, "SOURCE": "127.0.0.1:40000"}[column]
			if strings.Index(header, column) != strings.Index(rows[0], value) {
				t.Fatalf("width %d: %s starts at %d in the header and %d in the row\n%q\n%q", width, column, strings.Index(header, column), strings.Index(rows[0], value), header, rows[0])
			}
		}
		if strings.Index(rows[1], "↳") != strings.Index(header, "DESTINATION") {
			t.Fatalf("width %d: payload line %q", width, rows[1])
		}
		if liveWidth(rows[0]) > width {
			t.Fatalf("width %d: row is %d wide", width, liveWidth(rows[0]))
		}
	}
}

func TestTraceDividerFillsTheLine(t *testing.T) {
	if got := traceDivider("── title", 20); got != "── title ───────────" || liveWidth(got) != 20 {
		t.Fatalf("divider = %q (%d)", got, liveWidth(got))
	}
	if got := traceDivider("── a long title that does not fit", 12); liveWidth(got) != 12 || strings.HasSuffix(got, "─") {
		t.Fatalf("cut divider = %q", got)
	}
}
