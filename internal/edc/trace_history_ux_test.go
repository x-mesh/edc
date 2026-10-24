package edc

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTraceScreenStoppingTickKeepsOneChainAndStopsAfterResult(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	stopCalls := 0
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), func() { stopCalls++ })
	next, cmd := model.Update(traceStopMsg{})
	model = next.(traceScreenModel)
	if cmd == nil || stopCalls != 1 || model.stopTick != 1 {
		t.Fatalf("initial stop: cmd %t, stop calls %d, tick %d", cmd != nil, stopCalls, model.stopTick)
	}
	next, repeat := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	model = next.(traceScreenModel)
	if repeat != nil || stopCalls != 1 {
		t.Fatalf("repeat stop: cmd %t, stop calls %d", repeat != nil, stopCalls)
	}
	started := model.stopStarted
	next, tickCmd := model.Update(traceStopTickMsg{sequence: 1, at: started.Add(2 * time.Second)})
	model = next.(traceScreenModel)
	if tickCmd == nil || model.stopTick != 2 || model.stopElapsed != 2*time.Second || model.stopFrame != 1 {
		t.Fatalf("tick: cmd %t, tick %d, elapsed %s, frame %d", tickCmd != nil, model.stopTick, model.stopElapsed, model.stopFrame)
	}
	if view := model.View().Content; !strings.Contains(view, "Elapsed: 2s") || !strings.Contains(view, liveSpinnerFrames[1]) {
		t.Fatalf("updated stopping view = %q", view)
	}
	next, stale := model.Update(traceStopTickMsg{sequence: 1, at: started.Add(3 * time.Second)})
	model = next.(traceScreenModel)
	if stale != nil || model.stopElapsed != 2*time.Second {
		t.Fatalf("stale tick: cmd %t, elapsed %s", stale != nil, model.stopElapsed)
	}
	fixtureErr := errors.New("fixture failure")
	result := traceFinishedMsg{summary: captureSummary{EventCount: 7}, err: fixtureErr}
	next, finishCmd := model.Update(result)
	model = next.(traceScreenModel)
	if finishCmd == nil || model.finished == nil || model.finished.summary.EventCount != 7 || model.finished.err != fixtureErr {
		t.Fatalf("result was not preserved: cmd %t, finished %#v", finishCmd != nil, model.finished)
	}
	next, afterFinish := model.Update(traceStopTickMsg{sequence: model.stopTick, at: started.Add(4 * time.Second)})
	model = next.(traceScreenModel)
	if afterFinish != nil || model.stopElapsed != 2*time.Second {
		t.Fatalf("tick after result: cmd %t, elapsed %s", afterFinish != nil, model.stopElapsed)
	}
}

func TestTraceScreenStoppingViewCoversExistingModesAndNarrowWidth(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, mode := range []string{"list", "detail", "split", "group", "filter"} {
		for _, width := range []int{40, 80, 180} {
			model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
			model.width, model.height = width, 24
			switch mode {
			case "detail":
				model.detail = &traceDetail{title: strings.Repeat("payload", 100), lines: []string{strings.Repeat("payload", 100)}}
			case "split":
				model.split = true
				model.preview = &tracePreview{lines: []string{strings.Repeat("payload", 100)}}
			case "group":
				model.groupBy = traceGroupBySource
			case "filter":
				model.filtering = true
			}
			model.requestStop()
			view := model.View().Content
			if !strings.Contains(view, "Stopping safely") || !strings.Contains(view, "Waiting for capture cleanup") || strings.Contains(view, "payloadpayload") || strings.Contains(view, "q quit") {
				t.Fatalf("mode %s width %d view = %q", mode, width, view)
			}
			for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
				if liveWidth(line) > width {
					t.Fatalf("mode %s width %d line width %d: %q", mode, width, liveWidth(line), line)
				}
			}
		}
	}
}

func TestTraceHistoryUXPinnedSelectionKeepsCaptureLive(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 12
	next, _ := model.Update(traceEventMsg{events: []captureEvent{
		{Protocol: "http", Event: traceHTTPRequestEvent, Process: "curl"},
		{Protocol: "http", Event: "http_2xx", Process: "curl"},
	}})
	model = traceScreenKey(next.(traceScreenModel), "up")
	header := traceScreenHeader(model)[0]
	if !strings.Contains(header, "live · pinned #2 · 0 new") || strings.Contains(header, "paused") {
		t.Fatalf("pinned capture status = %q", header)
	}
	model = traceScreenKey(model, "enter")
	if view := strings.Join(traceScreenDetailView(model), "\n"); !strings.Contains(view, "capture live") || !strings.Contains(view, "target #2") {
		t.Fatalf("detail status does not identify capture and target: %q", view)
	}
}

func TestTraceHistoryUXSplitRuleUsesFullIndependentRow(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, width := range []int{80, 120, 180} {
		model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height, model.split = width, 24, true
		model.events = []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Process: strings.Repeat("very-long-process-", 20)}}
		model.first = 0
		model.refreshPreview()
		rows := traceScreenRows(model)
		list, _, ok := traceSplitHeights(model.height)
		if !ok || liveWidth(rows[list]) != width || strings.Trim(rows[list], "─") != "" {
			t.Fatalf("width %d split rule = %q", width, rows[list])
		}
		if liveWidth(rows[list+1]) > width {
			t.Fatalf("width %d preview header = %q", width, rows[list+1])
		}
	}
}

func TestTraceHistoryUXPreviewTitleUsesHTTPSSchemeForTLS(t *testing.T) {
	for _, test := range []struct {
		event captureEvent
		want  string
	}{{
		event: captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "fixture.example", Path: "/secure", TLS: true},
		want:  "GET https://fixture.example/secure",
	}, {
		event: captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "fixture.example", Path: "/plain"},
		want:  "GET http://fixture.example/plain",
	}, {
		event: captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "https://fixture.example", Path: "/secure"},
		want:  "GET https://fixture.example/secure",
	}} {
		if got := tracePreviewTitle(test.event); got != test.want {
			t.Fatalf("preview title = %q, want %q", got, test.want)
		}
	}
}

func TestTraceHistoryUXSplitAndDetailAccountForTLSNoticeRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, size := range []struct{ width, height int }{{80, 24}, {120, 24}, {180, 24}, {80, 40}, {120, 40}, {180, 40}} {
		model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height, model.split = size.width, size.height, true
		model.tlsNotices = []string{"OpenSSL peer metadata is unavailable"}
		model.events = []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/fixture", Payload: "GET /fixture HTTP/1.1\r\n\r\nbody"}}
		model.first = 0
		model.refreshPreview()
		header, rows := traceScreenHeader(model), traceScreenRows(model)
		if len(header)+len(rows) != size.height {
			t.Fatalf("%dx%d split height = %d + %d", size.width, size.height, len(header), len(rows))
		}
		list, _, ok := traceSplitHeights(size.height)
		list = min(list, max(0, size.height-len(header)))
		if !ok || liveWidth(rows[list]) != size.width || strings.Trim(rows[list], "─") != "" {
			t.Fatalf("%dx%d TLS split rule = %q", size.width, size.height, rows[list])
		}
		model.detail = model.buildDetail(0)
		if view := traceScreenDetailView(model); len(view) != size.height || !strings.Contains(view[3], "TLS:") {
			t.Fatalf("%dx%d TLS detail height or notice failed: %#v", size.width, size.height, view[:min(5, len(view))])
		}
	}
}

func TestTraceHistoryUXHiddenSmallScreenPreviewDoesNotScheduleWork(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 80, 8
	model.events = []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/fixture", Payload: "GET /fixture HTTP/1.1\r\n\r\nbody"}}
	model.payloadRevisions[0] = 1
	model = traceScreenKey(model, "i")
	if model.detailJobActive || model.preview != nil {
		t.Fatalf("small screen scheduled a hidden preview: active %t preview %#v", model.detailJobActive, model.preview)
	}
	next, _ := model.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	model = next.(traceScreenModel)
	if model.detailJobActive || model.preview != nil {
		t.Fatalf("unrelated key scheduled a hidden preview: active %t preview %#v", model.detailJobActive, model.preview)
	}
	next, cmd := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = next.(traceScreenModel)
	if !model.detailJobActive || model.preview == nil || cmd == nil {
		t.Fatalf("visible split did not schedule its current preview: active %t preview %#v cmdNil %t", model.detailJobActive, model.preview, cmd == nil)
	}
}

func TestTraceHistoryUXPreviewKeysUseCachedWrappedRows(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := traceScreenModel{
		protocol: "http", width: 80, height: 18, split: true,
		preview: &tracePreview{number: 0, width: 80, lines: []string{strings.Repeat("full-payload-", 1<<16)}, wrappedRows: 2, wrapped: []string{"cached row one", "cached row two"}, title: "GET http://api.example/fixture"},
	}
	if rows := strings.Join(traceScreenPreviewRows(model, 6), "\n"); !strings.Contains(rows, "cached row one") || strings.Contains(rows, "full-payload") {
		t.Fatalf("preview ignored its wrapped row cache: %q", rows)
	}
	model.scrollPreview(1)
	if model.previewOffset != 0 {
		t.Fatalf("J moved beyond cached rows: %d", model.previewOffset)
	}
}

func TestTraceHistoryUXSelectionColorsStayInsideTraceAndHistory(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "truecolor")
	line := traceSelectedEventStatus("curl  api.example  http_5xx  -", 80, true, richRed, "http_5xx")
	if liveWidth(line) != 80 || !strings.Contains(ansi.Strip(line), "▌ curl") || !strings.Contains(line, "38;2;255;84;96;48;2;45;28;62mhttp_5xx") {
		t.Fatalf("trace selection lost its bar, width, or red status: %q", line)
	}
	selected := historySelect(richPaint("Today", richGray, false)+"  "+richPaint("PASS", richGreen, false), true)
	background := "\033[1;97;" + richRGBCode(historySelectedBackground, true, true) + "m"
	if !strings.Contains(selected, background) || !strings.Contains(selected, liveSelectedBar+"\033[39m\033[97m") || !strings.Contains(selected, richSGR("PASS", richGreen)) {
		t.Fatalf("history selection lost its purple surface, bright text, or PASS color: %q", selected)
	}
	t.Setenv("NO_COLOR", "1")
	if value := historySelect("PASS", true); value != "▸ PASS" || strings.ContainsAny(value, "\033") {
		t.Fatalf("NO_COLOR history selection = %q", value)
	}
}

func TestTraceHistoryUXSelectedRowsKeepColumnsAndClippedHTTPFailureColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "truecolor")
	latency := 987.0
	for _, width := range []int{80, 120} {
		model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadHead, payloadExplicit: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height, model.payloadLines, model.selected = width, 12, true, 0
		model.events = []captureEvent{
			{Protocol: "http", Event: "http_5xx", Method: "GET", Target: "fixture.example", Path: "/failure", Status: 500, TLS: true, TLSVersion: "TLSv1.3", LatencyMS: &latency, Process: "failed-process", Payload: "HTTP/1.1 500 failed\r\n\r\nbody"},
			{Protocol: "http", Event: "http_2xx", Method: "GET", Target: "fixture.example", Path: "/pass", Status: 200, Process: "passing-process", Payload: "HTTP/1.1 200 OK\r\n\r\nbody"},
		}
		model.first = 0
		header := traceScreenHeader(model)[2]
		rows := traceScreenListRows(model, 8)
		selected, payload, unselected := "", "", ""
		for _, row := range rows {
			plain := ansi.Strip(row)
			if strings.HasPrefix(plain, "▌ ") {
				selected = row
			} else if selected != "" && strings.Contains(plain, "↳") {
				if payload == "" {
					payload = row
				}
			} else if strings.Contains(plain, "passing-process") {
				unselected = row
			}
		}
		if selected == "" || payload == "" || unselected == "" {
			t.Fatalf("width %d selected event or payload row missing: %#v", width, rows)
		}
		headerPlain := ansi.Strip(header)
		selectedVisual := ansi.Strip(selected)
		selectedPlain := strings.TrimPrefix(selectedVisual, "▌ ")
		cellStart := func(line, text string) int { return liveWidth(line[:strings.Index(line, text)]) }
		if cellStart(headerPlain, "PROCESS") != cellStart(selectedVisual, "failed-process") || cellStart(headerPlain, "DESTINATION") != cellStart(selectedVisual, "GET https://") {
			t.Fatalf("width %d selected cells shifted from headers: %q / %q", width, headerPlain, selectedVisual)
		}
		if cellStart(headerPlain, "PROCESS") != cellStart(ansi.Strip(unselected), "passing-process") || liveWidth(unselected) != width {
			t.Fatalf("width %d ordinary row did not keep the shared gutter: %q", width, unselected)
		}
		if !strings.Contains(selected, "38;2;255;84;96;48;2;45;28;62m") || !strings.Contains(selectedPlain, "http_5xx tls 500") {
			t.Fatalf("width %d clipped HTTP failure lost its semantic red color: %q", width, selected)
		}
		if width := liveWidth(selected); width > model.width {
			t.Fatalf("selected row width %d exceeds %d: %q", width, model.width, selected)
		}
		if strings.Index(headerPlain, "DESTINATION") != strings.Index(ansi.Strip(payload), "↳") {
			t.Fatalf("width %d payload did not align to the destination column: %q / %q", width, headerPlain, ansi.Strip(payload))
		}
	}
}

func TestTraceHistoryUXGzipHintNamesTheAvailableAction(t *testing.T) {
	model := traceScreenModel{
		protocol: "http",
		first:    0,
		events:   []captureEvent{{Protocol: "http", Event: "http_2xx", Payload: "HTTP/1.1 200 OK\r\n\r\nraw"}},
		payloads: traceFullPayloads{minimum: httpPayloadHead},
		gzipped:  traceFullPayloads{minimum: httpPayloadHead, byNumber: map[int]string{0: "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\n\r\n"}},
	}
	model.payloads.keep(0, model.events[0].Payload)
	_, _, notes := model.detailPayload(0)
	if !slicesContains(notes, "gzip compressed · z decompress") {
		t.Fatalf("raw gzip notes = %#v", notes)
	}
	model.decode = true
	_, _, notes = model.detailPayload(0)
	if !slicesContains(notes, "z raw") {
		t.Fatalf("decoded gzip notes = %#v", notes)
	}
}

func TestTraceHistoryUXProgressiveDetailCoalescesLatestRevision(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 16
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "POST", Target: "api.example", Path: "/stream", Payload: "POST /stream HTTP/1.1\r\n\r\nrevision-0", MessageID: 7}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
	model = next.(traceScreenModel)
	next, job := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(traceScreenModel)
	if !model.detailJobActive || model.detail == nil || !strings.Contains(strings.Join(model.detail.lines, "\n"), "preparing detail…") {
		t.Fatalf("enter did not publish a loading shell: %#v", model.detail)
	}
	for revision := 1; revision <= 20; revision++ {
		update := event
		update.Payload = "POST /stream HTTP/1.1\r\n\r\nrevision-" + itoa(revision)
		update.Update = true
		next, _ = model.Update(traceEventMsg{events: []captureEvent{update}})
		model = next.(traceScreenModel)
	}
	if !model.detailJobActive || model.detailJobPending == nil || model.detailJobPending.revision != 21 {
		t.Fatalf("pending detail is not the latest revision: active %t, pending %#v", model.detailJobActive, model.detailJobPending)
	}
	result := job()
	next, pendingJob := model.Update(result)
	model = next.(traceScreenModel)
	if model.detail == nil || model.detail.number != 0 || !model.detail.updating || pendingJob == nil {
		t.Fatalf("first completed snapshot did not show progress: detail %#v pending %v", model.detail, pendingJob != nil)
	}
	next, _ = model.Update(pendingJob())
	model = next.(traceScreenModel)
	if model.detail.revision != 21 || model.detail.updating || !strings.Contains(strings.Join(model.detail.lines, "\n"), "revision-20") {
		t.Fatalf("latest revision did not converge: %#v", model.detail)
	}
}

func TestTraceHistoryUXRapidMaskHidesOldDetailResult(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{showSecrets: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 16
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Payload: "GET / HTTP/1.1\r\nAuthorization: Bearer private-token\r\n\r\nbody"}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
	model = next.(traceScreenModel)
	next, unmaskedJob := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(traceScreenModel)
	next, _ = model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = next.(traceScreenModel)
	if strings.Contains(strings.Join(model.detail.lines, "\n"), "private-token") {
		t.Fatal("unmasked detail remained visible after m-off")
	}
	next, maskedJob := model.Update(unmaskedJob())
	model = next.(traceScreenModel)
	if maskedJob == nil {
		t.Fatal("m-off did not queue a masked replacement")
	}
	next, _ = model.Update(maskedJob())
	model = next.(traceScreenModel)
	text := strings.Join(model.detail.lines, "\n")
	if strings.Contains(text, "private-token") || !strings.Contains(text, "Authorization: ***") {
		t.Fatalf("late unmasked result replaced the masked detail: %q", text)
	}
}

func TestTraceHistoryUXFollowPublishesWhileTargetMoves(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 16
	first := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/0", Payload: "GET /0 HTTP/1.1\r\n\r\nfirst"}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{first}})
	model = next.(traceScreenModel)
	model.follow = true
	model.detail = model.detailShell(0, "preparing detail…")
	model.detailGeneration = 1
	firstJob := model.requestDetailBuild()
	events := make([]captureEvent, 100)
	for index := range events {
		events[index] = captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/" + itoa(index+1), Payload: "GET /" + itoa(index+1) + " HTTP/1.1\r\n\r\nlatest-" + itoa(index+1)}
	}
	next, _ = model.Update(traceEventMsg{events: events})
	model = next.(traceScreenModel)
	if !model.detailJobActive || model.detailJobPending == nil || model.detailJobPending.number != 100 {
		t.Fatalf("follow queue is not bounded at the latest target: active %t pending %#v", model.detailJobActive, model.detailJobPending)
	}
	next, latestJob := model.Update(firstJob())
	model = next.(traceScreenModel)
	if model.detail.number != 0 || !model.detail.updating || latestJob == nil {
		t.Fatalf("follow did not publish a completed snapshot while updating: detail %#v", model.detail)
	}
	next, _ = model.Update(latestJob())
	model = next.(traceScreenModel)
	if model.detail.number != 100 || model.detail.updating || !strings.Contains(strings.Join(model.detail.lines, "\n"), "latest-100") {
		t.Fatalf("follow did not converge on the newest target: %#v", model.detail)
	}
}

func TestTraceHistoryUXRapidModeChangesRejectOldMaskedPreview(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{showSecrets: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height, model.split = 80, 24, true
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/private", Payload: "GET /private HTTP/1.1\r\nAuthorization: Bearer old-secret\r\n\r\nbody"}
	model.events, model.payloadRevisions[0] = []captureEvent{event}, 1
	model.keepEventPayload(0, event)
	oldJob := model.requestPreviewBuild()
	if model.preview == nil || !model.detailJobActive || oldJob == nil {
		t.Fatal("split did not start its first preview job")
	}
	for _, key := range []string{"s", "m", "g", "i", "i"} {
		model = traceScreenKey(model, key)
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = next.(traceScreenModel)
	if model.preview == nil || model.preview.width != 120 || model.secrets || model.detailJobPending == nil || model.detailJobPending.kind != "preview" {
		t.Fatalf("latest preview shell/settings were not queued: preview %#v pending %#v secrets %t", model.preview, model.detailJobPending, model.secrets)
	}
	next, latestJob := model.Update(oldJob())
	model = next.(traceScreenModel)
	if strings.Contains(strings.Join(model.preview.lines, "\n"), "old-secret") || latestJob == nil {
		t.Fatalf("old unmasked preview was published or pending work was lost: %#v", model.preview)
	}
	next, _ = model.Update(latestJob())
	model = next.(traceScreenModel)
	text := strings.Join(model.preview.lines, "\n")
	if strings.Contains(text, "old-secret") || !strings.Contains(text, "Authorization: ***") || model.preview.width != 120 || model.preview.updating {
		t.Fatalf("rapid mode changes did not converge to masked width-120 preview: %#v", model.preview)
	}
}

func TestTraceHistoryUXManualPreviewTargetShowsImmediateShell(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height, model.split = 80, 24, true
	model.events = []captureEvent{
		{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/first", Payload: "GET /first HTTP/1.1\r\n\r\none"},
		{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "api.example", Path: "/second", Payload: "GET /second HTTP/1.1\r\n\r\ntwo"},
	}
	model.payloadRevisions[0], model.payloadRevisions[1] = 1, 1
	model.selected = 1
	oldJob := model.requestPreviewBuild()
	next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	model = next.(traceScreenModel)
	if model.preview == nil || model.preview.number != 0 || !strings.Contains(strings.Join(model.preview.lines, "\n"), "preparing preview…") || model.detailJobPending == nil || model.detailJobPending.number != 0 {
		t.Fatalf("manual B target did not replace shell/pending snapshot: preview %#v pending %#v old job nil %t", model.preview, model.detailJobPending, oldJob == nil)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
