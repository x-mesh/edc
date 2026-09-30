package edc

import (
	"strings"
	"testing"
)

func TestTraceScreenSplitShowsTheSelectedMessage(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 60, 20
	body := strings.Repeat("Z", 3000)
	events := []captureEvent{
		{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl", Method: "POST", Target: "api.example", Path: "/b", Payload: "POST /b HTTP/1.1\r\nAuthorization: Bearer t0ken\r\n\r\n" + body},
		{Protocol: "http", Event: "http_2xx", Process: "curl", Target: "api.example", Path: "/b", Status: 200, Payload: "HTTP/1.1 200 OK\r\n\r\nok"},
	}
	next, _ := model.Update(traceEventMsg{events: events})
	model = traceScreenKey(next.(traceScreenModel), "i")

	rows := traceScreenRows(model)
	list, _, _ := traceSplitHeights(model.height)
	if len(rows) != model.height-3 || !strings.HasPrefix(rows[list], "── curl  api.example/b  http_2xx") || !strings.Contains(strings.Join(rows[list:], "\n"), "HTTP/1.1 200 OK") {
		t.Fatalf("with nothing selected the preview must show the newest event: %q", rows)
	}
	for _, row := range rows {
		if liveWidth(row) > model.width {
			t.Fatalf("row is %d wide: %q", liveWidth(row), row)
		}
	}

	model = traceScreenKey(model, "up", "up")
	preview := strings.Join(traceScreenRows(model)[list:], "\n")
	if !strings.Contains(preview, "POST /b HTTP/1.1") || strings.Contains(preview, "t0ken") || !strings.HasPrefix(preview, "── more below  ·  curl") {
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
