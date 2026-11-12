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
		case "left":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
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
	if err := model.setFilter("login"); err != nil {
		t.Fatal(err)
	}
	if shown := model.shown(); len(shown) != 1 {
		t.Fatalf("filter shows %d lines", len(shown))
	}
	_ = model.setFilter("")
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

// filter를 걸면 통계 패널은 filter에 맞는 줄만 센다. 머리 줄의 events는 보관한 줄 전체다.
func TestViewScreenPanelCountsFilteredLines(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 30)
	model = viewPress(model, "/", "u", "s", "e", "r", "s", "enter")
	stats := model.panelStats()
	if stats.lines != 3 || stats.paths["/api/users"] != 3 || len(stats.paths) != 1 || stats.classes[2] != 2 || stats.classes[4] != 1 || stats.classes[5] != 0 || stats.failures != 0 {
		t.Fatalf("filtered stats %+v", stats)
	}
	joined := strings.Join(model.screen(false), "\n")
	if !strings.Contains(joined, "STATS  filtered · last 10,000 lines") || strings.Contains(joined, "/api/orders") || strings.Contains(joined, "5xx") || !strings.Contains(joined, "events 3 / 6") || !strings.Contains(joined, "events    3 / 6") || !strings.Contains(joined, " 67%") || !strings.Contains(joined, " 33%") {
		t.Fatalf("filtered panel:\n%s", joined)
	}
	model.ingest([]string{
		`192.0.2.1 - - [26/Oct/2026:10:00:06 +0900] "GET /api/users HTTP/1.1" 500 1 "-" "x"`,
		`192.0.2.1 - - [26/Oct/2026:10:00:07 +0900] "GET /api/orders HTTP/1.1" 200 1 "-" "x"`,
	}, time.Now(), true)
	if stats := model.panelStats(); stats.lines != 4 || stats.failures != 1 || stats.clients["192.0.2.1"] != 1 || stats.rate(time.Now()) != 0.1 {
		t.Fatalf("new lines: filtered stats %+v rate %.2f", stats, stats.rate(time.Now()))
	}
	model = viewPress(model, "esc")
	if stats := model.panelStats(); model.filter != "" || stats.lines != 8 || stats.paths["/api/users"] != 4 || stats.failures != 2 {
		t.Fatalf("esc must show all lines again: %+v", stats)
	}
}

// 보관한 줄이 밀려나면 filter 통계에서도 빠진다.
func TestViewScreenFilteredStatsForgetEvictedLines(t *testing.T) {
	model := newViewScreenModel("app", 3)
	model.ingest([]string{"2026-10-26 10:00:00 ERROR score boom", "2026-10-26 10:00:01 INFO score ok"}, time.Now(), false)
	model = viewPress(model, "/", "s", "c", "o", "r", "e", "enter")
	model.ingest([]string{"2026-10-26 10:00:02 INFO other", "2026-10-26 10:00:03 INFO other"}, time.Now(), false)
	if stats := model.panelStats(); stats.lines != 1 || stats.failures != 0 || stats.lastError.text != "" || stats.levels["info"] != 1 {
		t.Fatalf("filtered stats after eviction %+v", stats)
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
			if err := model.setFilter(filter); err != nil {
				t.Fatal(err)
			}
			if rows := model.screen(false); len(rows) != max(height, len(model.header(false))) {
				t.Fatalf("height %d filter %q: %d rows", height, filter, len(rows))
			}
		}
	}
}

func TestViewScreenPanelWidthFitsTheContent(t *testing.T) {
	// 통계가 제목뿐이면 패널은 제목 폭이고, 구분선은 그만큼 오른쪽으로 간다.
	plain := newViewScreenModel("plain", viewKeepLines)
	plain.width, plain.height = 200, 20
	plain.ingest([]string{"hello", "world"}, time.Now(), false)
	if width := plain.panelWidth(); width != len("STATS  last 10,000 lines") {
		t.Fatalf("plain panel width %d", width)
	}
	rows := plain.screen(false)
	if at := strings.Index(rows[2], " │ "); at != 200-plain.panelWidth()-3 {
		t.Fatalf("separator at %d in %q", at, rows[2])
	}
	// 상위 path가 길면 viewPanelWidth까지만 넓어지고, 마지막 오류도 그 안에서 줄을 바꾼다.
	access := viewScreenWith(t, "access.log", 200, 30)
	access.ingest([]string{`192.0.2.1 - - [26/Oct/2026:10:00:09 +0900] "GET /api/` + strings.Repeat("x", 60) + ` HTTP/1.1" 500 1 "-" "x"`}, time.Now(), false)
	if width := access.panelWidth(); width != viewPanelWidth {
		t.Fatalf("access panel width %d", width)
	}
	// syslog 로그는 상위 프로그램과 client를 보인다.
	auth := viewScreenWith(t, "auth.log", 200, 40)
	joined := strings.Join(auth.screen(false), "\n")
	if !strings.Contains(joined, "TOP PROGRAMS") || !strings.Contains(joined, "sshd-session") || !strings.Contains(joined, "TOP CLIENTS") || !strings.Contains(joined, "192.0.2.10") || !strings.Contains(joined, "error ") {
		t.Fatalf("auth panel:\n%s", joined)
	}
	if auth.stats.failures != 2 || auth.stats.levels["error"] != 2 {
		t.Fatalf("auth failures %d levels %v", auth.stats.failures, auth.stats.levels)
	}
	narrow := strings.Join(viewScreenWith(t, "auth.log", 90, 40).screen(false), "\n")
	if !strings.Contains(narrow, "top programs  sshd-session 5") || !strings.Contains(narrow, "top clients  192.0.2.10 2") {
		t.Fatalf("narrow auth panel:\n%s", narrow)
	}
	// goloop 로그는 module이 상위 프로그램이고, level 글자가 LEVEL에 센다.
	goloop := viewScreenWith(t, "goloop.log", 200, 40)
	joined = strings.Join(goloop.screen(false), "\n")
	if !strings.Contains(joined, "TOP PROGRAMS") || !strings.Contains(joined, "CS") || goloop.stats.levels["debug"] != 5 || goloop.stats.levels["fatal"] != 1 || goloop.stats.failures != 2 {
		t.Fatalf("goloop panel levels %v failures %d:\n%s", goloop.stats.levels, goloop.stats.failures, joined)
	}
}

func viewHostLines(clients ...string) []string {
	var raw []string
	for index, client := range clients {
		raw = append(raw, fmt.Sprintf(`int.example.com %s %s - - [27/Oct/2026:16:43:%02d +0900] "GET /board/%d HTTP/1.1" 200 %d "-" "curl"`, client, client, index, index, 100+index))
	}
	return raw
}

// 보관한 줄에서 값이 모두 같은 열은 compact 보기에서 숨기고, 머리 줄이 무엇을 숨겼는지 알린다.
func TestViewScreenCompactHidesConstantColumns(t *testing.T) {
	model := newViewScreenModel("access.log", viewKeepLines)
	model.width, model.height = 100, 12
	model.ingest(viewHostLines("203.0.113.9", "203.0.113.9", "203.0.113.9"), time.Now(), false)
	screen := strings.Join(model.screen(false), "\n")
	if strings.Contains(screen, "int.example.com 203") || !strings.Contains(screen, "16:43:02  200  GET     102  /board/2") {
		t.Fatalf("compact rows:\n%s", screen)
	}
	wide := model
	wide.width = 200
	if header := strings.Join(wide.header(false), "\n"); !strings.Contains(header, "hidden: host int.example.com · client 203.0.113.9 · date 27/Oct/2026") {
		t.Fatalf("header must name the hidden columns:\n%s", header)
	}
	// client가 여럿이면 그 열은 보인다.
	model.ingest(viewHostLines("198.51.100.4"), time.Now(), false)
	screen = strings.Join(model.screen(false), "\n")
	if !strings.Contains(screen, "198.51.100.4  16:43:00") || !strings.Contains(screen, "203.0.113.9   16:43:02") {
		t.Fatalf("varying client must show:\n%s", screen)
	}
	// c를 누르면 원문으로 돌아간다.
	model = viewPress(model, "c")
	if screen = strings.Join(model.screen(false), "\n"); !strings.Contains(screen, "int.example.com 203.0.113.9 203.0.113.9 - -") {
		t.Fatalf("c must show the raw lines:\n%s", screen)
	}
	model.width = 200
	if strings.Contains(strings.Join(model.header(false), "\n"), "hidden:") {
		t.Fatal("the raw view hides nothing")
	}
}

// ←/→는 목록을 가로로 민다. 잘린 오른쪽을 볼 수 있다.
func TestViewScreenScrollsSideways(t *testing.T) {
	model := newViewScreenModel("app", viewKeepLines)
	model.width, model.height = 40, 8
	model.ingest([]string{"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"}, time.Now(), false)
	if screen := strings.Join(model.screen(false), "\n"); !strings.Contains(screen, "0123456789abc") {
		t.Fatalf("start:\n%s", screen)
	}
	model = viewPress(model, "right")
	screen := strings.Join(model.screen(false), "\n")
	if strings.Contains(screen, "0123456789") || !strings.Contains(screen, "abcdefghij") {
		t.Fatalf("after right:\n%s", screen)
	}
	model = viewPress(model, "left", "left")
	if model.scroll != 0 {
		t.Fatalf("scroll %d", model.scroll)
	}
}

// filter를 바꾸면 이미 받은 줄의 받은 시각으로 초당 줄 수를 바로 다시 센다. 화면을 열 때 읽은 줄은 세지 않는다.
func TestViewScreenFilteredRateCountsLinesBeforeTheFilter(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 30)
	now := time.Now()
	model.ingest([]string{
		`192.0.2.1 - - [26/Oct/2026:10:00:06 +0900] "GET /api/users HTTP/1.1" 200 1 "-" "x"`,
		`192.0.2.1 - - [26/Oct/2026:10:00:07 +0900] "GET /api/users HTTP/1.1" 500 1 "-" "x"`,
		`192.0.2.1 - - [26/Oct/2026:10:00:08 +0900] "GET /api/orders HTTP/1.1" 200 1 "-" "x"`,
	}, now, true)
	if err := model.setFilter("path:/api/users"); err != nil {
		t.Fatal(err)
	}
	if rate := model.panelStats().rate(now); rate != 0.2 {
		t.Fatalf("filtered rate %.2f", rate)
	}
	// 형식을 다시 정해 줄을 다시 읽어도 받은 시각은 남는다.
	model.formatFixed, model.format = false, viewFormatPlain
	model.redetect(nil)
	if model.format != viewFormatAccess {
		t.Fatalf("redetect format %q", model.format)
	}
	if rate := model.panelStats().rate(now); rate != 0.2 {
		t.Fatalf("rate after redetect %.2f", rate)
	}
	if rate := model.panelStats().rate(now.Add(viewRateWindow + 2*time.Second)); rate != 0 {
		t.Fatalf("old arrivals leave the window: %.2f", rate)
	}
}

// filter를 걸고 패널에서 조건을 더한 뒤 Esc를 누르면 한 단계씩 되돌아간다. 걸린 filter는 패널에 다 보인다.
func TestViewScreenEscStepsBackThroughFilters(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 30)
	model = viewPress(model, "/", "u", "s", "e", "r", "s", "enter", "tab", "enter")
	if model.filter != "users status:2xx" {
		t.Fatalf("filter %q", model.filter)
	}
	model = viewPress(model, "esc")
	screen := strings.Join(model.screen(false), "\n")
	if model.picking || model.filter != "users status:2xx" || !strings.Contains(screen, "filter    users status:2xx") || !strings.Contains(model.header(false)[0], "filter users status:2xx") {
		t.Fatalf("esc leaves the panel and shows the filter: %q\n%s", model.filter, screen)
	}
	model = viewPress(model, "esc")
	if model.filter != "users" || model.panelStats().lines != 3 {
		t.Fatalf("esc goes back to the filter before: %q", model.filter)
	}
	model = viewPress(model, "esc")
	if model.filter != "" || strings.Contains(strings.Join(model.screen(false), "\n"), "filter    ") {
		t.Fatalf("esc goes back to all lines: %q", model.filter)
	}
	if model = viewPress(model, "esc"); model.filter != "" {
		t.Fatalf("nothing to undo: %q", model.filter)
	}
}

// 패널 안에서는 Backspace가 마지막에 더한 조건을 되돌리고, 패널에 그대로 남는다.
func TestViewScreenBackspaceUndoesThePick(t *testing.T) {
	model := viewPress(viewScreenWith(t, "access.log", 140, 30), "tab", "enter", "down", "enter")
	if model.filter != "status:2xx path:/api/users" {
		t.Fatalf("filter %q picks %v", model.filter, model.panelPicks())
	}
	model = viewPress(model, "backspace")
	if !model.picking || model.filter != "status:2xx" {
		t.Fatalf("backspace undoes one pick: picking %v filter %q", model.picking, model.filter)
	}
	model = viewPress(model, "backspace", "backspace")
	if !model.picking || model.filter != "" {
		t.Fatalf("backspace goes back to all lines: %q", model.filter)
	}
}

// 패널보다 긴 filter는 잘리지 않고 줄을 바꾼다. 좁은 화면에서는 통계 첫 줄에 보인다.
func TestViewScreenShowsALongFilter(t *testing.T) {
	model := viewScreenWith(t, "access.log", 140, 30)
	long := "path:/api/* -path:/api/orders?id=7 -client:192.0.2.77 -status:4xx"
	if err := model.applyFilter(long); err != nil {
		t.Fatal(err)
	}
	panel, _ := model.panelColumn(model.panelWidth(), false)
	joined := strings.ReplaceAll(strings.Join(panel, ""), " ", "")
	if !strings.Contains(joined, strings.ReplaceAll(long, " ", "")) {
		t.Fatalf("the panel must show the whole filter:\n%s", strings.Join(panel, "\n"))
	}
	narrow := viewScreenWith(t, "access.log", 90, 30)
	_ = narrow.applyFilter("users")
	if rows, _ := narrow.panelRows(90, false); !strings.Contains(rows[0], "filter users") {
		t.Fatalf("narrow stats: %q", rows[0])
	}
}
