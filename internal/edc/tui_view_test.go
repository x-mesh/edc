package edc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func viewScreenWith(t *testing.T, fixture string, width, height int) viewScreenModel {
	t.Helper()
	model := newViewScreenModel(fixture, viewKeepLines)
	model.width, model.height = width, height
	model.ingest(readViewFixture(t, fixture), time.Now(), false)
	return model
}

func viewPress(model viewScreenModel, keys ...string) viewScreenModel {
	for _, key := range keys {
		var msg tea.KeyPressMsg
		switch key {
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "end":
			msg = tea.KeyPressMsg{Code: tea.KeyEnd}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		default:
			msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		next, _ := model.Update(msg)
		model = next.(viewScreenModel)
	}
	return model
}

func TestViewScreenPausesAndFollowsAgain(t *testing.T) {
	model := viewScreenWith(t, "access.log", 100, 20)
	model = viewPress(model, "up")
	if model.selected != model.last() {
		t.Fatalf("up selects the newest line: selected %d last %d", model.selected, model.last())
	}
	model = viewPress(model, "up")
	paused := model.selected
	model.receive(viewDataMsg{event: viewFollowEvent{data: []byte("192.0.2.1 - - [26/Oct/2026:10:00:06 +0900] \"GET / HTTP/1.1\" 200 1\n")}}, time.Now())
	if model.selected != paused {
		t.Fatalf("a new line moved the paused list: %d -> %d", paused, model.selected)
	}
	if header := strings.Join(model.header(false), "\n"); !strings.Contains(header, "paused · 2 new") {
		t.Fatalf("header must count the new lines: %q", header)
	}
	model = viewPress(model, "end")
	if model.selected != -1 {
		t.Fatalf("end follows again: %d", model.selected)
	}
	model = viewPress(model, "up", "esc")
	if model.selected != -1 {
		t.Fatalf("esc follows again: %d", model.selected)
	}
}

func TestViewScreenKeepsOnlyTheNewestLines(t *testing.T) {
	model := newViewScreenModel("app", 5)
	var raw []string
	for index := range 12 {
		raw = append(raw, fmt.Sprintf("2026-10-26 10:00:%02d ERROR failure %d", index, index))
	}
	model.ingest(raw, time.Now(), true)
	if len(model.lines) != 5 || model.first != 7 || model.stats.lines != 5 || model.stats.levels["error"] != 5 {
		t.Fatalf("kept %d lines from %d, stats %+v", len(model.lines), model.first, model.stats)
	}
	if rate := model.stats.rate(time.Now()); rate != 1.2 {
		t.Fatalf("rate %.2f", rate)
	}
}

func TestViewScreenPlacesThePanelByWidth(t *testing.T) {
	wide := viewScreenWith(t, "access.log", 140, 30).screen(false)
	if !strings.Contains(strings.Join(wide, "\n"), " │ STATS  last 10,000 lines") || !strings.Contains(strings.Join(wide, "\n"), "5xx") {
		t.Fatalf("wide screen needs the side panel:\n%s", strings.Join(wide, "\n"))
	}
	narrow := viewScreenWith(t, "access.log", 90, 30).screen(false)
	joined := strings.Join(narrow, "\n")
	if strings.Contains(joined, " │ ") || !strings.Contains(joined, "stats  last 10,000 lines") || !strings.Contains(joined, "top paths  /api/users 3") {
		t.Fatalf("narrow screen needs the panel below the list:\n%s", joined)
	}
	// level이 error이고 status가 502인 JSON 줄은 오류 하나다.
	if json := viewScreenWith(t, "app.jsonl", 90, 30); json.stats.failures != 1 {
		t.Fatalf("json errors %d", json.stats.failures)
	}
	if len(narrow) != 30 || len(wide) != 30 {
		t.Fatalf("screen heights %d and %d", len(narrow), len(wide))
	}
	hidden := viewPress(viewScreenWith(t, "access.log", 90, 30), "i").screen(false)
	if strings.Contains(strings.Join(hidden, "\n"), "stats  last") {
		t.Fatal("i hides the panel")
	}
}

func TestViewScreenDetailShowsStackTrace(t *testing.T) {
	model := viewScreenWith(t, "app.log", 100, 30)
	model.selected = 2
	model = viewPress(model, "enter")
	joined := strings.Join(model.detailView(false), "\n")
	for _, want := range []string{"level    ERROR", "message  [http-7] c.e.Api - request failed", "java.lang.IllegalStateException", "Caused by: java.io.IOException", "... 12 more"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("detail misses %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "user reported") {
		t.Fatal("detail must stop at the next event")
	}
	model = viewPress(model, "esc")
	if model.detail != -1 {
		t.Fatal("esc closes the detail")
	}
}

func TestViewScreenFilterAndReset(t *testing.T) {
	model := viewScreenWith(t, "access.log", 100, 20)
	model.filter = "login"
	if shown := model.shown(); len(shown) != 1 {
		t.Fatalf("filter shows %d lines", len(shown))
	}
	model.filter = ""
	model.receive(viewDataMsg{event: viewFollowEvent{reset: viewResetReplaced}}, time.Now())
	if last, _ := model.line(model.last()); !last.marker || !strings.Contains(last.text, "file replaced") {
		t.Fatalf("rotation marker: %+v", last)
	}
	// stack trace 줄은 앞 event에 속하므로, 9줄짜리 app.log는 event 4개다.
	if header := viewScreenWith(t, "app.log", 100, 20).header(false)[0]; !strings.Contains(header, "events 4") {
		t.Fatalf("header must count events: %q", header)
	}
	if model.stats.lines != 6 {
		t.Fatalf("the marker must not count as a line: %d", model.stats.lines)
	}
}

func TestViewScreenDetectsFormatAfterEmptyStart(t *testing.T) {
	model := newViewScreenModel("app", viewKeepLines)
	model.receive(viewDataMsg{event: viewFollowEvent{data: []byte("{\"level\":\"error\",\"msg\":\"boom\"}\n")}}, time.Now())
	if model.format != viewFormatJSON || model.stats.failures != 1 {
		t.Fatalf("format %q errors %d", model.format, model.stats.failures)
	}
}

// 한 줄이 아주 길거나 줄바꿈 없이 계속 쓰여도 보관하는 글자에 상한이 있다.
func TestViewScreenBoundsLongLinesAndPendingBytes(t *testing.T) {
	model := newViewScreenModel("app", viewKeepLines)
	model.receive(viewDataMsg{event: viewFollowEvent{data: []byte(strings.Repeat("a", viewStoreLimit*2) + "\n")}}, time.Now())
	if last, _ := model.line(model.last()); len(last.text) > viewStoreLimit {
		t.Fatalf("stored %d bytes of one line", len(last.text))
	}
	chunk := []byte(strings.Repeat("b", 256<<10))
	for range 8 {
		model.receive(viewDataMsg{event: viewFollowEvent{data: chunk}}, time.Now())
	}
	if len(model.pending) > viewPendingLimit {
		t.Fatalf("pending grew to %d bytes", len(model.pending))
	}
}

// trace 상세 화면처럼 q는 목록으로 돌아간다.
func TestViewScreenDetailQGoesBack(t *testing.T) {
	model := viewPress(viewScreenWith(t, "app.log", 100, 30), "enter")
	next, cmd := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	model = next.(viewScreenModel)
	if model.detail != -1 || cmd != nil {
		t.Fatalf("q in the detail must go back: detail %d, quit %v", model.detail, cmd != nil)
	}
}

// 통계는 보관한 줄만 센다. 마지막 오류가 밀려나면 LAST ERROR도 비운다.
func TestViewScreenForgetsEvictedLastError(t *testing.T) {
	model := newViewScreenModel("app", 3)
	model.ingest([]string{"2026-10-26 10:00:00 ERROR boom", "2026-10-26 10:00:01 INFO ok"}, time.Now(), false)
	if model.stats.lastError.text == "" {
		t.Fatal("the error is still kept")
	}
	model.ingest([]string{"2026-10-26 10:00:02 INFO ok", "2026-10-26 10:00:03 INFO ok"}, time.Now(), false)
	if model.stats.failures != 0 || model.stats.lastError.text != "" {
		t.Fatalf("evicted error stays: failures %d last %q", model.stats.failures, model.stats.lastError.text)
	}
}

// 목록이 화면을 다 채운 뒤 줄바꿈 없는 줄이 오면 목록에 한 줄이 더 붙는다. 넓은 화면에서도 목록은 화면 높이에 맞춰
// 잘리고, 오른쪽 패널과 줄 수가 같아야 한다.
func TestViewScreenSidePanelWithFullListAndPartialLine(t *testing.T) {
	for _, height := range []int{1, 2, 3, 10, 40} {
		model := newViewScreenModel("app", viewKeepLines)
		model.width, model.height = 140, height
		var raw []string
		for index := range 50 {
			raw = append(raw, fmt.Sprintf("2026-10-26 10:00:%02d ERROR failure %d", index%60, index))
		}
		model.ingest(raw, time.Now(), false)
		model.receive(viewDataMsg{event: viewFollowEvent{data: []byte("2026-10-26 10:01:00 ERROR partial")}}, time.Now())
		if len(model.pending) == 0 {
			t.Fatal("the last line must stay pending")
		}
		for _, filter := range []string{"", "partial"} {
			model.filter = filter
			if rows := model.screen(false); len(rows) != max(height, len(model.header(false))) {
				t.Fatalf("height %d filter %q: %d rows", height, filter, len(rows))
			}
		}
	}
}
