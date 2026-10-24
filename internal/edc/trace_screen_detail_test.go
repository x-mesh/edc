package edc

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func traceScreenKey(model traceScreenModel, keys ...string) traceScreenModel {
	for _, key := range keys {
		code := map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter, "esc": tea.KeyEscape, "end": tea.KeyEnd, "home": tea.KeyHome, "space": tea.KeySpace}[key]
		press := tea.KeyPressMsg{Code: code}
		if code == 0 {
			press = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
		}
		model = traceScreenApply(model, press)
	}
	return model
}

func traceScreenApply(model traceScreenModel, msg tea.Msg) traceScreenModel {
	next, cmd := model.Update(msg)
	model = next.(traceScreenModel)
	if model.detail == nil && !model.split {
		return model
	}
	return traceScreenRunDetailCmd(model, cmd)
}

func traceScreenEventWithDetail(model traceScreenModel, msg tea.Msg) traceScreenModel {
	next, cmd := model.Update(msg)
	model = next.(traceScreenModel)
	if cmd == nil {
		return model
	}
	result := cmd()
	if batch, ok := result.(tea.BatchMsg); ok {
		for _, child := range batch[1:] {
			if built, ok := child().(traceDetailBuiltMsg); ok {
				next, pending := model.Update(built)
				model = traceScreenRunDetailCmd(next.(traceScreenModel), pending)
			}
		}
	}
	return model
}

func traceScreenRunDetailCmd(model traceScreenModel, cmd tea.Cmd) traceScreenModel {
	for range 8 {
		if cmd == nil {
			return model
		}
		result := cmd()
		switch value := result.(type) {
		case traceDetailBuiltMsg:
			next, nextCmd := model.Update(value)
			model, cmd = next.(traceScreenModel), nextCmd
		case tea.BatchMsg:
			cmd = nil
			children := value
			if len(children) > 1 {
				children = children[1:]
			}
			for _, child := range children {
				if built := child(); built != nil {
					if detail, ok := built.(traceDetailBuiltMsg); ok {
						next, nextCmd := model.Update(detail)
						model, cmd = next.(traceScreenModel), nextCmd
					}
				}
			}
		default:
			return model
		}
	}
	return model
}

func TestTraceScreenSelectsAnEventAndShowsAllOfIt(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll, payloadExplicit: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
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
	if header := traceScreenHeader(model)[0]; !strings.Contains(header, "live · pinned #2 · 1 new") {
		t.Fatalf("header = %q", header)
	}
	// 화면에 자리가 남으면 고른 event 아래에 더 새 event가 온다.
	rows := strings.Join(traceScreenRows(model), "\n")
	if selected, newer := strings.Index(rows, "POST http://api.example/b"), strings.Index(rows, "http_2xx"); selected < 0 || newer < selected {
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

func TestTraceScreenStoppingViewShowsCleanupFeedbackImmediately(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	stopCalls := 0
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), func() { stopCalls++ })
	model.width, model.height = 80, 24
	next, _ := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	model = next.(traceScreenModel)
	view := model.View().Content
	if !model.stopping || stopCalls != 1 || !strings.Contains(view, "Stopping safely") || !strings.Contains(view, "Waiting for capture cleanup and final results") {
		t.Fatalf("stopping view = %q, stop calls %d, stopping %t", view, stopCalls, model.stopping)
	}
}

func TestTraceScreenPayloadStartsVisibleAndKeepsScrollAcrossSettings(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll, payloadExplicit: true, showSecrets: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 80, 12
	event := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "POST", Target: "api.example", Path: "/long", Payload: "POST /long HTTP/1.1\r\nAuthorization: Bearer fixture-token\r\n\r\n" + strings.Repeat("body ", 900)}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
	model = traceScreenKey(next.(traceScreenModel), "enter")
	if model.detail.payloadStart < 0 || model.detail.offset != model.detail.payloadStart || !strings.Contains(strings.Join(traceScreenDetailView(model), "\n"), "POST /long HTTP/1.1") {
		t.Fatalf("first detail page did not start at its visible payload: %#v", model.detail)
	}
	model = traceScreenKey(model, "j", "j")
	if model.detail.offset != model.detail.payloadStart+2 {
		t.Fatalf("detail scroll did not move: offset %d payload %d", model.detail.offset, model.detail.payloadStart)
	}
	scrollOffset := model.detail.offset
	next, cmd := model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = next.(traceScreenModel)
	if !model.detail.preserveOffset || model.detail.restoreOffset != scrollOffset || model.detail.offset != 0 || !strings.Contains(strings.Join(traceScreenDetailView(model), "\n"), "preparing detail…") {
		t.Fatalf("m shell did not preserve scroll: %#v", model.detail)
	}
	model = traceScreenRunDetailCmd(model, cmd)
	expectedOffset := min(scrollOffset, max(0, len(model.detail.lines)-traceScreenDetailPage(model)))
	if model.detail.offset != expectedOffset || strings.Contains(strings.Join(model.detail.lines, "\n"), "fixture-token") {
		t.Fatalf("m did not keep the masked scroll position: offset %d payload %d", model.detail.offset, model.detail.payloadStart)
	}
	scrollOffset = expectedOffset
	model = traceScreenKey(model, "z")
	expectedOffset = min(scrollOffset, max(0, len(model.detail.lines)-traceScreenDetailPage(model)))
	if model.detail.offset != expectedOffset {
		t.Fatalf("z changed the scroll position: offset %d payload %d", model.detail.offset, model.detail.payloadStart)
	}
	scrollOffset = expectedOffset
	next, cmd = model.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	model = traceScreenRunDetailCmd(next.(traceScreenModel), cmd)
	expectedOffset = min(scrollOffset, max(0, len(model.detail.lines)-traceScreenDetailPage(model)))
	if model.detail.offset != expectedOffset {
		t.Fatalf("resize changed the scroll position: offset %d payload %d", model.detail.offset, model.detail.payloadStart)
	}
	model = traceScreenKey(model, "esc")
	if model.detail != nil || model.selected != 0 {
		t.Fatalf("Esc lost the selected target: detail %#v selected %d", model.detail, model.selected)
	}
}

func TestTraceScreenUpsertsProgressivePayloadByMessageID(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll, payloadExplicit: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 12
	initialPayload := "POST /stream HTTP/1.1\r\nHost: api.example\r\n\r\n" + strings.Repeat("a", httpPayloadHead+10)
	initial := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Method: "POST", Path: "/stream", Target: "api.example", Payload: initialPayload, MessageID: 7}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{initial}})
	model = next.(traceScreenModel)
	model = traceScreenKey(model, "enter")
	firstNumber := model.detail.number
	updatedPayload := initialPayload + strings.Repeat("b", 100)
	update := initial
	update.Payload, update.Update, update.Bytes, update.ByteDelta = updatedPayload, true, uint64(len(updatedPayload)), 100
	model = traceScreenEventWithDetail(model, traceEventMsg{events: []captureEvent{update}})
	if len(model.events) != 1 || model.received != 1 || model.detail == nil || model.detail.number != firstNumber || !strings.Contains(strings.Join(model.detail.lines, ""), strings.Repeat("b", 100)) {
		t.Fatalf("upsert state = events %d, received %d, detail %#v", len(model.events), model.received, model.detail)
	}
	if len(model.payloads.order) != 1 || model.payloads.bytes != len(updatedPayload) || model.payloads.order[0] != firstNumber {
		t.Fatalf("payload index = %#v, bytes %d", model.payloads.order, model.payloads.bytes)
	}
	model.first++
	model.events = model.events[1:]
	model.arrivals = model.arrivals[1:]
	model.payloads.drop(model.first)
	model.messageNumbers = map[uint64]int{}
	next, _ = model.Update(traceEventMsg{events: []captureEvent{update}})
	model = next.(traceScreenModel)
	if len(model.events) != 0 {
		t.Fatalf("evicted message was restored: %#v", model.events)
	}
}

func TestTCPDiagnosticsAppearInScrollAndDetailAtAllWidths(t *testing.T) {
	event := captureEvent{Protocol: "tcp", Event: "tcp_sample", Process: "api", Source: "127.0.0.1:40000", Destination: "127.0.0.1:443", TCPValid: 1 | 4 | 32 | 64, RTTUS: 25, CWND: 10, Lost: 2, ZeroWindow: 1}
	for _, width := range []int{40, 80, 120} {
		line := formatTraceScreenEventLine(event, width)
		if liveWidth(line) > width || !strings.Contains(line, "sample") {
			t.Fatalf("width %d line = %q", width, line)
		}
		model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
		model.width, model.height = width, 12
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = traceScreenKey(next.(traceScreenModel), "enter")
		text := strings.Join(model.detail.raw, "\n")
		if !strings.Contains(text, `"rtt_us": 25`) || !strings.Contains(text, `"cwnd": 10`) || !strings.Contains(text, `"lost": 2`) || !strings.Contains(text, `"zero_window": 1`) {
			t.Fatalf("width %d detail = %q", width, text)
		}
	}
}

func TestTCPAcceptScrollLabelShowsLatencyAndQueue(t *testing.T) {
	event := captureEvent{Protocol: "tcp", Event: "tcp_app_accept", Destination: "127.0.0.1:443", AcceptLatencyNS: 1250000, AcceptQueueUsed: 2, AcceptQueueMax: 32}
	_, label := traceScrollLabels(event)
	if label != "app accept=1.250ms q=2/32" {
		t.Fatalf("label = %q", label)
	}
}

func TestTraceScreenPairsMySQLCommandAndResponseForDisplay(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 120, 10
	model.events = []captureEvent{
		{SocketID: 7, Protocol: "mysql", Event: mysqlEventConnect, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandConnect}},
		{SocketID: 7, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "SELECT 1"}},
		{SocketID: 7, Protocol: "mysql", Event: mysqlEventResult, Side: traceServerSide, LatencyMS: func() *float64 { value := 0.4; return &value }(), MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "SELECT 1", Columns: 2}},
	}
	rows := traceVisibleScreenRows(model)
	if len(rows) != 2 || rows[1].response != 2 {
		t.Fatalf("display rows = %+v", rows)
	}
	_, destination, label, ok := model.displayRowLabels(rows[1])
	if !ok || destination != "server: SELECT 1 → 2 columns" || label != "mysql_result 0.4ms" {
		t.Fatalf("display row = %q, %q, %t", destination, label, ok)
	}
	model.filter = "columns"
	if !model.displayRowMatches(rows[1]) {
		t.Fatal("response text did not match the paired row")
	}
	model.selected = 2
	model.normalizeSelection()
	if model.selected != 1 {
		t.Fatalf("selected response = %d, want primary 1", model.selected)
	}
}

func TestTraceScreenIncrementalMySQLRowsMatchReference(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	query := func(socket uint64, sql string) captureEvent {
		return captureEvent{SocketID: socket, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}}
	}
	result := func(socket uint64, sql string) captureEvent {
		return captureEvent{SocketID: socket, Protocol: "mysql", Event: mysqlEventResult, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}}
	}
	events := []captureEvent{query(1, "first"), query(1, "first"), result(1, "first"), result(1, "first"), query(2, "second"), result(2, "second")}
	for _, event := range events {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
		if got, want := traceVisibleScreenRows(model), traceMySQLReferenceRows(model.events, model.first, 0); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
	}
}

func TestTraceScreenIncrementalMySQLRowsMatchReferenceAfterEviction(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	query := func(socket uint64, sql string) captureEvent {
		return captureEvent{SocketID: socket, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}}
	}
	result := func(socket uint64, sql string) captureEvent {
		return captureEvent{SocketID: socket, Protocol: "mysql", Event: mysqlEventResult, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}}
	}
	events := make([]captureEvent, 0, traceScreenEventLimit+3)
	events = append(events, query(1, "paired-primary"), result(1, "paired-primary"), query(2, "pending"), result(3, "standalone"))
	for len(events) < traceScreenEventLimit+3 {
		events = append(events, captureEvent{Protocol: "mysql", Event: mysqlEventTLS, MySQL: &traceMySQLEvent{TLS: true}})
	}
	for _, event := range events {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
	}
	if got, want := traceVisibleScreenRows(model), traceMySQLReferenceRows(model.events, model.first, 0); !slices.Equal(got, want) {
		t.Fatalf("rows after eviction = %+v, want %+v", got[:min(5, len(got))], want[:min(5, len(want))])
	}
	for number := range model.mysqlRowIndex {
		if number < model.first {
			t.Fatalf("stale row index %d before %d", number, model.first)
		}
	}
	for number := range model.mysqlPendingKey {
		if number < model.first {
			t.Fatalf("stale pending index %d before %d", number, model.first)
		}
	}
}

func TestTraceScreenMySQLPendingKeysDoNotAccumulate(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	for index := 0; index < traceScreenEventLimit+100; index++ {
		sql := fmt.Sprintf("SELECT %d", index)
		latency := 1.0
		next, _ := model.Update(traceEventMsg{events: []captureEvent{
			{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}},
			{SocketID: uint64(index), Protocol: "mysql", Event: mysqlEventResult, LatencyMS: &latency, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: sql}},
		}})
		model = next.(traceScreenModel)
	}
	if len(model.mysqlPending) != 0 || len(model.mysqlPendingKey) != 0 {
		t.Fatalf("pending keys = %d, pending numbers = %d", len(model.mysqlPending), len(model.mysqlPendingKey))
	}
}

func TestTraceScreenMySQLDetailIncludesPairedResponse(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := 1.25
	next, _ := model.Update(traceEventMsg{events: []captureEvent{
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "SELECT 1"}},
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventError, LatencyMS: &latency, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "SELECT 1", ErrorCode: 1064, SQLState: "42000", Message: "bad syntax"}},
	}})
	model = next.(traceScreenModel)
	before := slices.Clone(model.events)
	detail := model.buildDetail(0)
	text := strings.Join(detail.raw, "\n")
	if !strings.Contains(text, "response:") || !strings.Contains(text, "result: 1064 (42000) bad syntax") || !strings.Contains(text, "latency: 1.2ms") {
		t.Fatalf("detail = %q", text)
	}
	if !slices.EqualFunc(model.events, before, func(a, b captureEvent) bool { return reflect.DeepEqual(a, b) }) {
		t.Fatal("detail changed source events")
	}
}

func TestTraceScreenMySQLHeaderCountsOnlyShownPairs(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{slow: time.Millisecond}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := 2.0
	next, _ := model.Update(traceEventMsg{events: []captureEvent{
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "shown"}},
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventResult, LatencyMS: &latency, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "shown"}},
		{SocketID: 2, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "pending"}},
		{SocketID: 3, Protocol: "mysql", Event: mysqlEventResult, LatencyMS: &latency, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "standalone"}},
		{SocketID: 4, Protocol: "mysql", Event: mysqlEventTLS, MySQL: &traceMySQLEvent{TLS: true}},
	}})
	model = next.(traceScreenModel)
	model.filter = "shown"
	header := ansi.Strip(strings.Join(traceScreenHeader(model), "\n"))
	if !strings.Contains(header, "raw events 5") || !strings.Contains(header, "shown pairs 1") || !strings.Contains(header, "slow 1ms") {
		t.Fatalf("header = %q", header)
	}
}

func TestTraceScreenFiltersMySQLPairsBySlowLatency(t *testing.T) {
	model := newTraceScreenModel("mysql", tcpTraceOptions{slow: 1500 * time.Microsecond}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := func(value float64) *float64 { return &value }
	model.events = []captureEvent{
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "below"}},
		{SocketID: 1, Protocol: "mysql", Event: mysqlEventResult, Side: traceServerSide, LatencyMS: latency(1.499), MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "below"}},
		{SocketID: 2, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "exact"}},
		{SocketID: 2, Protocol: "mysql", Event: mysqlEventResult, Side: traceServerSide, LatencyMS: latency(1.5), MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "exact"}},
		{SocketID: 3, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceClientSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "above"}},
		{SocketID: 3, Protocol: "mysql", Event: mysqlEventResult, Side: traceClientSide, LatencyMS: latency(1.501), MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "above"}},
		{SocketID: 4, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "pending"}},
		{SocketID: 5, Protocol: "mysql", Event: mysqlEventPrefix + mysqlCommandQuery, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "missing"}},
		{SocketID: 5, Protocol: "mysql", Event: mysqlEventResult, Side: traceServerSide, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "missing"}},
		{SocketID: 6, Protocol: "mysql", Event: mysqlEventResult, Side: traceServerSide, LatencyMS: latency(4), MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "unmatched"}},
		{SocketID: 7, Protocol: "mysql", Event: mysqlEventTLS, Side: traceClientSide, MySQL: &traceMySQLEvent{TLS: true}},
	}
	rows := traceVisibleScreenRows(model)
	if len(rows) != 2 || rows[0] != (traceScreenRow{primary: 2, response: 3}) || rows[1] != (traceScreenRow{primary: 4, response: 5}) {
		t.Fatalf("slow display rows = %+v", rows)
	}
}

func traceVisibleScreenRows(model traceScreenModel) []traceScreenRow {
	rows := make([]traceScreenRow, 0, len(model.displayRows()))
	for _, row := range model.displayRows() {
		if !row.hidden && (model.slow == 0 || model.displayRowIsSlow(row)) {
			rows = append(rows, row)
		}
	}
	return rows
}

func TestTraceMySQLDetailUsesReadableFieldsAndSQLLines(t *testing.T) {
	latency := 0.4
	event := captureEvent{TimestampNS: 1_700_000_000_123_000_000, BootTimeNS: 987, Bytes: 42, Event: mysqlEventPrefix + mysqlCommandQuery, Protocol: "mysql", Side: traceServerSide, Process: "mysqld", Source: "127.0.0.1:3306", Destination: "127.0.0.1:40000", LatencyMS: &latency, MySQL: &traceMySQLEvent{Command: mysqlCommandQuery, SQL: "WITH EmployeeOrder AS (\n\tSELECT sid\n) SELECT * FROM EmployeeOrder"}}
	detail := newTraceDetail(event, "", nil, 0, 120)
	text := strings.Join(detail.lines, "\n")
	if strings.Contains(text, "timestamp_ns") || strings.Contains(text, "boot_time_ns") || !strings.Contains(text, "sql:\nWITH EmployeeOrder AS (\n    SELECT sid") {
		t.Fatalf("detail = %q", text)
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
	for number := 10; number < 14; number++ {
		payloads.keep(number, big)
	}
	if _, ok := payloads.get(10); !ok || payloads.bytes != 20<<20 {
		t.Fatalf("payload index lost its oldest value, bytes %d", payloads.bytes)
	}
	if _, ok := payloads.get(13); !ok {
		t.Fatal("the newest payload is missing")
	}
	payloads.drop(12)
	if _, ok := payloads.get(11); ok || len(payloads.order) != 2 {
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

func TestTraceScreenCountsPayloadAndGzipBytesTogether(t *testing.T) {
	model := traceScreenModel{payloads: traceFullPayloads{minimum: httpPayloadHead}, gzipped: traceFullPayloads{minimum: httpPayloadHead}}
	model.keepEventPayload(0, captureEvent{Payload: strings.Repeat("p", 40<<20), gzipped: []byte(strings.Repeat("g", 30<<20))})
	if total := model.payloads.bytes + model.gzipped.bytes; total > traceScreenPayloadBytes {
		t.Fatalf("stored %d full bytes, limit %d", total, traceScreenPayloadBytes)
	}
	if _, ok := model.payloads.get(0); ok {
		t.Fatal("oldest full payload survived the shared byte limit")
	}
	if _, ok := model.gzipped.get(0); ok {
		t.Fatal("gzip source was not evicted with the same message payload")
	}
	if !model.payloadEvicted[0] {
		t.Fatal("full payload eviction was not recorded")
	}
}

func TestTraceScreenKeepsFullPayloadsWithinTheLimitAsEventsArrive(t *testing.T) {
	const size = 10 << 20
	payload := strings.Repeat("p", size)
	model := traceScreenModel{payloads: traceFullPayloads{minimum: httpPayloadHead}, gzipped: traceFullPayloads{minimum: httpPayloadHead}}
	const events = 40
	for number := 0; number < events; number++ {
		model.keepEventPayload(number, captureEvent{Payload: payload})
		if total := model.payloads.bytes + model.gzipped.bytes; total > traceScreenPayloadBytes {
			t.Fatalf("after event %d stored %d full bytes, limit %d", number, total, traceScreenPayloadBytes)
		}
	}
	if total := model.payloads.bytes; total <= traceScreenPayloadBytes-size {
		t.Fatalf("stored only %d full bytes, the limit %d leaves room for more", total, traceScreenPayloadBytes)
	}
	if _, ok := model.payloads.get(events - 1); !ok || model.payloadEvicted[events-1] {
		t.Fatal("the newest full payload was evicted")
	}
	if _, ok := model.payloads.get(0); ok || !model.payloadEvicted[0] {
		t.Fatal("the oldest full payload was not evicted")
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
	for filter, want := range map[string]bool{"orders": true, "post http://127.0.0.1:18090/ord": true, "curl": true, "/missing": false} {
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
	if !scope.payload || !scope.payloadAll || !scope.showSecrets || !scope.keepGzip {
		t.Fatalf("http screen scope = %+v", scope)
	}
	if scope := traceScreenScope("http", tcpTraceOptions{payload: tracePayloadAll}); !scope.payloadAll {
		t.Fatalf("--payload=all was lost: %+v", scope)
	}
	if scope := traceScreenScope("http", tcpTraceOptions{payloadExplicit: true}); scope.payload || scope.payloadAll || scope.keepGzip {
		t.Fatalf("--payload=false was lost: %+v", scope)
	}
	if scope := traceScreenScope("tcp", tcpTraceOptions{}); scope.payload || scope.showSecrets || scope.keepGzip {
		t.Fatalf("tcp screen scope = %+v", scope)
	}
}

func TestTraceScreenHidesDefaultPayloadRowsButKeepsExplicitRows(t *testing.T) {
	defaultModel := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	if defaultModel.payloadLines {
		t.Fatal("default HTTP payload rows are visible")
	}
	explicitModel := newTraceScreenModel("http", tcpTraceOptions{payload: tracePayloadAll, payloadExplicit: true}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	if !explicitModel.payloadLines {
		t.Fatal("explicit --payload=all rows are hidden")
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
	if text := strings.Join(model.detail.lines, "\n"); !strings.Contains(text, "Bearer t0ken") || !strings.Contains(traceScreenDetailView(model)[0], "secrets shown") || !strings.Contains(traceScreenDetailView(model)[1], "m secrets") {
		t.Fatalf("m in the detail view did not show the secret: %q", text)
	}
}

func TestTraceScreenFollowShowsTheNewestEvent(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 100, 12
	send := func(path string, complete bool) {
		if complete {
			model = traceScreenEventWithDetail(model, traceEventMsg{events: []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "x", Path: path}}})
		} else {
			next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "http", Event: traceHTTPRequestEvent, Method: "GET", Target: "x", Path: path}}})
			model = next.(traceScreenModel)
		}
	}
	send("/one", false)
	model = traceScreenKey(model, "f")
	if model.detail == nil || !model.follow || model.detail.number != 0 {
		t.Fatalf("f did not open the newest event: %+v", model.detail)
	}
	send("/two", true)
	if model.detail.number != 1 || !strings.Contains(strings.Join(model.detail.lines, "\n"), `"/two"`) {
		t.Fatalf("follow did not move to the new event: %d", model.detail.number)
	}
	if !strings.Contains(traceScreenDetailView(model)[0], "follow") || !strings.Contains(traceScreenDetailView(model)[1], "f follow") {
		t.Fatal("the detail header and hint do not say follow")
	}
	// f를 다시 누르면 보던 event에서 멈추고, 새 event가 와도 그대로 둔다.
	model = traceScreenKey(model, "f")
	send("/three", false)
	if model.follow || model.detail.number != 1 || model.selected != 1 {
		t.Fatalf("stopped follow: follow %t, number %d, selected %d", model.follow, model.detail.number, model.selected)
	}
	model = traceScreenKey(model, "f", "esc")
	if model.detail != nil || model.follow || model.selected != -1 {
		t.Fatalf("esc from follow: detail %v, follow %t, selected %d", model.detail, model.follow, model.selected)
	}
}

func TestTraceScreenEvictionKeepsOnlyVisibleSelection(t *testing.T) {
	model := newTraceScreenModel("http", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.filter, model.selected = "keep", 0
	model.events = make([]captureEvent, traceScreenEventLimit)
	model.arrivals = make([]time.Time, traceScreenEventLimit)
	model.events[1] = captureEvent{Protocol: "http", Event: "keep"}
	next, _ := model.Update(traceEventMsg{events: []captureEvent{{Protocol: "http", Event: "drop"}}})
	model = next.(traceScreenModel)
	if model.selected != model.first {
		t.Fatalf("selected = %d, first = %d", model.selected, model.first)
	}
	model.selected = model.first
	next, _ = model.Update(traceEventMsg{events: []captureEvent{{Protocol: "http", Event: "drop"}}})
	model = next.(traceScreenModel)
	if model.selected != -1 || model.preview != nil {
		t.Fatalf("hidden selection = %d, preview = %#v", model.selected, model.preview)
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
	if filled != model.height-3 || !strings.Contains(rows[0], fmt.Sprintf("p%d ", model.first)) {
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

func TestTraceSelectedEventUsesTheChipBackground(t *testing.T) {
	plain := traceSelectedEventStatus("curl  GET /", 20, false, "", "")
	if !strings.HasPrefix(plain, "▸ ") || strings.Contains(plain, liveReverse) {
		t.Fatalf("plain selection = %q", plain)
	}
	colored := traceSelectedEventStatus("curl  GET /", 20, true, "", "")
	if strings.Contains(colored, liveReverse) || liveWidth(colored) != 20 || !strings.HasPrefix(ansi.Strip(colored), "▌ curl  GET /") || !strings.Contains(colored, "48;2;45;28;62") {
		t.Fatalf("colored selection = %q", colored)
	}
}

// traceTestSGR는 lipgloss가 팔레트 색을 칠할 때 내는 24비트 SGR 인자다. lipgloss는 색 수를 줄이지 않고 Bubble Tea가 줄인다.
func traceTestSGR(rgb richRGB, background bool) string {
	lead := "38"
	if background {
		lead = "48"
	}
	return fmt.Sprintf("%s;2;%d;%d;%d", lead, rgb.r, rgb.g, rgb.b)
}

func traceTestColor(code string) string { return traceTestSGR(richPalette[code].rgb, false) }

// traceScreenLines는 같은 model을 색 없이 한 번, 색으로 한 번 그린다.
func traceScreenLines(t *testing.T, model traceScreenModel) (plain, colored []string) {
	t.Setenv("NO_COLOR", "1")
	plain = append(traceScreenHeader(model), traceScreenRows(model)...)
	t.Setenv("NO_COLOR", "")
	colored = append(traceScreenHeader(model), traceScreenRows(model)...)
	return plain, colored
}

func TestTraceScreenColorKeepsThePlainTextAndWidth(t *testing.T) {
	events := []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Process: "curl", Destination: "203.0.113.10:443", Source: "10.0.0.2:5000", TimestampNS: 1},
		{Protocol: "tcp", Event: "tcp_send_reset", Process: "curl", Destination: "203.0.113.10:443", Source: "10.0.0.2:5000", TimestampNS: 2},
		{Protocol: "tcp", Event: "tcp_retransmit", Process: "a-very-long-process-name", Destination: "203.0.113.20:443", Target: "very-long-service-name.internal.example.com", Source: "10.0.0.2:5001", TimestampNS: 3},
	}
	for _, groupBy := range []string{"", traceGroupByTarget, traceGroupByEvent} {
		for _, width := range []int{40, 80, 150, 220} {
			model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: groupBy}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
			model.width, model.height, model.events = width, 8, events
			// 두 번 그리는 사이에 E/s가 바뀌지 않도록 창을 1초로 고정한다.
			model.started, model.duration = time.Now().Add(-time.Hour), time.Second
			plain, colored := traceScreenLines(t, model)
			for index := range plain {
				if ansi.Strip(colored[index]) != strings.TrimRight(plain[index], " ") && ansi.Strip(colored[index]) != plain[index] {
					t.Fatalf("%q width %d line %d:\nplain   %q\ncolored %q", groupBy, width, index, plain[index], ansi.Strip(colored[index]))
				}
				if liveWidth(colored[index]) > width {
					t.Fatalf("%q width %d line %d is %d columns: %q", groupBy, width, index, liveWidth(colored[index]), colored[index])
				}
			}
		}
	}
}

func TestTraceScreenColorsOnlyMeaningfulText(t *testing.T) {
	model := newTraceScreenModel("tcp", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 150, 6
	model.events = []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Process: "curl", Destination: "203.0.113.10:443", Source: "10.0.0.2:5000"},
		{Protocol: "tcp", Event: "tcp_send_reset", Process: "curl", Destination: "203.0.113.10:443", Source: "10.0.0.2:5000"},
		{Protocol: "tcp", Event: "tcp_retransmit", Process: "curl", Destination: "203.0.113.10:443", Source: "10.0.0.2:5000"},
	}
	_, lines := traceScreenLines(t, model)
	title := tracePaint("edc trace tcp", richMagenta, true)
	if !strings.HasPrefix(lines[0], title) || !strings.Contains(lines[0], traceTestColor(richMagenta)) {
		t.Fatalf("title = %q", lines[0])
	}
	if !strings.Contains(lines[1], tracePaint("tab", richCyan, true)) || !strings.Contains(lines[1], traceDim(" view")) {
		t.Fatalf("key hints = %q", lines[1])
	}
	if lines[2] != traceDim(ansi.Strip(lines[2])) {
		t.Fatalf("column titles = %q, want gray", lines[2])
	}
	rows := map[string]string{}
	for _, row := range lines[3:] {
		for _, name := range []string{"tcp_connect", "tcp_send_reset", "tcp_retransmit"} {
			if strings.Contains(row, name) {
				rows[name] = row
			}
		}
	}
	if strings.Contains(rows["tcp_connect"], traceTestColor(richRed)) || strings.Contains(rows["tcp_connect"], traceTestColor(richYellow)) || !strings.Contains(rows["tcp_connect"], traceTestColor(richGray)+"m10.0.0.2:5000") {
		t.Fatalf("plain event row = %q", rows["tcp_connect"])
	}
	if !strings.Contains(rows["tcp_send_reset"], tracePaint(liveCell("tcp_send_reset", traceScrollEventWidth), richRed, false)) {
		t.Fatalf("reset row = %q", rows["tcp_send_reset"])
	}
	if !strings.Contains(rows["tcp_retransmit"], tracePaint(liveCell("tcp_retransmit", traceScrollEventWidth), richYellow, false)) {
		t.Fatalf("retransmit row = %q", rows["tcp_retransmit"])
	}
	// 줄 단위 출력과 같은 규칙이다. HTTP는 status code의 색이다.
	for event, want := range map[*captureEvent]string{
		{Protocol: "http", Event: "http_2xx", Status: 200}: richGreen,
		{Protocol: "http", Event: "http_4xx", Status: 404}: richYellow,
		{Protocol: "http", Event: "http_5xx", Status: 503}: richRed,
		{Protocol: "drop", Event: "NO_SOCKET"}:             richRed,
		{Protocol: "mysql", Event: mysqlEventError}:        richRed,
		{Protocol: "udp", Event: "udp_send"}:               "",
	} {
		if got := traceEventSeverity(*event); got != want {
			t.Fatalf("%s %s severity = %q, want %q", event.Protocol, event.Event, got, want)
		}
	}
}

func TestTraceGroupScreenColorsProblemColumns(t *testing.T) {
	setTraceKernelEvents(t, true)
	model := newTraceScreenModel("tcp", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 200, 6
	model.events = []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Target: "ok.example"},
		{Protocol: "tcp", Event: "tcp_send_reset", Target: "bad.example"},
	}
	_, lines := traceScreenLines(t, model)
	var good, bad string
	for _, row := range lines[3:] {
		switch {
		case strings.Contains(row, "ok.example"):
			good = row
		case strings.Contains(row, "bad.example"):
			bad = row
		}
	}
	if strings.Contains(good, traceTestColor(richRed)) || !strings.Contains(bad, tracePaint("   1", richRed, false)) || !strings.Contains(bad, tracePaint(" tcp_send_reset", richRed, false)) {
		t.Fatalf("group rows:\n%q\n%q", good, bad)
	}
	if strings.Contains(bad, traceTestColor(richRed)+"mbad.example") {
		t.Fatalf("the group label keeps the default color: %q", bad)
	}
}

func clickHouseScreenEvent(socket uint64, name, side, sql string, latency *float64, info traceClickHouseEvent) captureEvent {
	info.Command, info.SQL = cmp.Or(info.Command, clickHouseCommandQuery), sql
	return captureEvent{SocketID: socket, Protocol: "clickhouse", Event: name, Side: side, LatencyMS: latency, ClickHouse: &info}
}

func TestTraceScreenPairsClickHouseQueryAndResponseForDisplay(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 120, 10
	latency := func(value float64) *float64 { return &value }
	rows := uint64(3)
	model.events = []captureEvent{
		clickHouseScreenEvent(7, clickHouseEventConnect, traceServerSide, "", nil, traceClickHouseEvent{Command: clickHouseCommandConnect}),
		clickHouseScreenEvent(7, clickHouseEventHello, traceServerSide, "", latency(0.2), traceClickHouseEvent{Command: clickHouseCommandConnect, ServerName: "ClickHouse", ServerVersion: "26.9"}),
		clickHouseScreenEvent(7, clickHouseEventQuery, traceServerSide, "SELECT 1", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(7, clickHouseEventEnd, traceServerSide, "SELECT 1", latency(0.4), traceClickHouseEvent{Rows: &rows}),
		clickHouseScreenEvent(7, clickHouseEventChunked, traceServerSide, "", nil, traceClickHouseEvent{Command: clickHouseCommandConnect, Chunked: true}),
	}
	visible := traceVisibleScreenRows(model)
	if len(visible) != 3 || visible[0].response != 1 || visible[1].response != 3 || visible[2].response != -1 {
		t.Fatalf("display rows = %+v", visible)
	}
	_, destination, label, ok := model.displayRowLabels(visible[1])
	if !ok || destination != "server: SELECT 1 → 3 rows" || label != "clickhouse_end 0.4ms" {
		t.Fatalf("display row = %q, %q, %t", destination, label, ok)
	}
	model.filter = "3 rows"
	if !model.displayRowMatches(visible[1]) {
		t.Fatal("response text did not match the paired row")
	}
	model.selected = 3
	model.normalizeSelection()
	if model.selected != 2 {
		t.Fatalf("selected response = %d, want primary 2", model.selected)
	}
}

func clickHouseScreenSequence(socketBase uint64) []captureEvent {
	query := func(socket uint64, sql string) captureEvent {
		return clickHouseScreenEvent(socket, clickHouseEventQuery, traceServerSide, sql, nil, traceClickHouseEvent{})
	}
	end := func(socket uint64, sql string) captureEvent {
		return clickHouseScreenEvent(socket, clickHouseEventEnd, traceServerSide, sql, nil, traceClickHouseEvent{EndInferred: true})
	}
	return []captureEvent{query(socketBase, "first"), query(socketBase, "first"), end(socketBase, "first"), end(socketBase, "first"), query(socketBase+1, "second"), end(socketBase+1, "second")}
}

func TestTraceScreenIncrementalClickHouseRowsMatchReference(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	for _, event := range clickHouseScreenSequence(1) {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
		if got, want := traceVisibleScreenRows(model), traceMySQLReferenceRows(model.events, model.first, 0); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
	}
}

func TestTraceScreenIncrementalClickHouseRowsMatchReferenceAfterEviction(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	events := make([]captureEvent, 0, traceScreenEventLimit+3)
	events = append(events,
		clickHouseScreenEvent(1, clickHouseEventQuery, traceServerSide, "paired-primary", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(1, clickHouseEventEnd, traceServerSide, "paired-primary", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(2, clickHouseEventQuery, traceServerSide, "pending", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(3, clickHouseEventEnd, traceServerSide, "standalone", nil, traceClickHouseEvent{}))
	for len(events) < traceScreenEventLimit+3 {
		events = append(events, clickHouseScreenEvent(4, clickHouseEventChunked, traceClientSide, "", nil, traceClickHouseEvent{Command: clickHouseCommandConnect, Chunked: true}))
	}
	for _, event := range events {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
	}
	if got, want := traceVisibleScreenRows(model), traceMySQLReferenceRows(model.events, model.first, 0); !slices.Equal(got, want) {
		t.Fatalf("rows after eviction = %+v, want %+v", got[:min(5, len(got))], want[:min(5, len(want))])
	}
	for number := range model.mysqlPendingKey {
		if number < model.first {
			t.Fatalf("stale pending index %d before %d", number, model.first)
		}
	}
}

func TestTraceScreenFiltersClickHousePairsBySlowLatency(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{slow: 1500 * time.Microsecond}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := func(value float64) *float64 { return &value }
	pair := func(socket uint64, sql, response string, value *float64, info traceClickHouseEvent) []captureEvent {
		return []captureEvent{
			clickHouseScreenEvent(socket, clickHouseEventQuery, traceServerSide, sql, nil, traceClickHouseEvent{}),
			clickHouseScreenEvent(socket, response, traceServerSide, sql, value, info),
		}
	}
	model.events = slices.Concat(
		pair(1, "below", clickHouseEventEnd, latency(1.499), traceClickHouseEvent{}),
		pair(2, "exact", clickHouseEventEnd, latency(1.5), traceClickHouseEvent{}),
		pair(3, "inferred", clickHouseEventEnd, latency(1.501), traceClickHouseEvent{EndInferred: true}),
		pair(4, "failed", clickHouseEventException, latency(9), traceClickHouseEvent{ErrorCode: 60, ErrorName: "UNKNOWN_TABLE"}),
		[]captureEvent{clickHouseScreenEvent(5, clickHouseEventQuery, traceServerSide, "pending", nil, traceClickHouseEvent{})},
		pair(6, "missing", clickHouseEventEnd, nil, traceClickHouseEvent{}),
		[]captureEvent{clickHouseScreenEvent(7, clickHouseEventEnd, traceServerSide, "unmatched", latency(4), traceClickHouseEvent{})},
		[]captureEvent{clickHouseScreenEvent(8, clickHouseEventChunked, traceClientSide, "", nil, traceClickHouseEvent{Command: clickHouseCommandConnect, Chunked: true})},
	)
	rows := traceVisibleScreenRows(model)
	want := []traceScreenRow{{primary: 2, response: 3}, {primary: 4, response: 5}, {primary: 6, response: 7}}
	if !slices.Equal(rows, want) {
		t.Fatalf("slow display rows = %+v, want %+v", rows, want)
	}
	for _, row := range rows {
		if !model.displayRowMatches(row) {
			t.Fatalf("row %+v does not match", row)
		}
	}
	for _, row := range model.displayRows() {
		if (row.primary == 0 || row.primary == 8 || row.primary == 9 || row.primary == 11 || row.primary == 12) && model.displayRowMatches(row) {
			t.Fatalf("row %+v is shown", row)
		}
	}
}

func TestTraceScreenClickHouseHeaderCountsOnlyShownPairs(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{slow: time.Millisecond}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := 2.0
	next, _ := model.Update(traceEventMsg{events: []captureEvent{
		clickHouseScreenEvent(1, clickHouseEventQuery, traceServerSide, "shown", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(1, clickHouseEventEnd, traceServerSide, "shown", &latency, traceClickHouseEvent{}),
		clickHouseScreenEvent(2, clickHouseEventQuery, traceServerSide, "pending", nil, traceClickHouseEvent{}),
	}})
	model = next.(traceScreenModel)
	header := ansi.Strip(strings.Join(traceScreenHeader(model), "\n"))
	if !strings.Contains(header, "raw events 3") || !strings.Contains(header, "shown pairs 1") || !strings.Contains(header, "slow 1ms") {
		t.Fatalf("header = %q", header)
	}
}

func TestTraceScreenClickHouseDetailIncludesPairedResponse(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := 1.25
	rows := uint64(10)
	next, _ := model.Update(traceEventMsg{events: []captureEvent{
		clickHouseScreenEvent(1, clickHouseEventQuery, traceServerSide, "SELECT 1\nFROM t", nil, traceClickHouseEvent{QueryID: "q-1", User: "edc", Database: "db"}),
		clickHouseScreenEvent(1, clickHouseEventEnd, traceServerSide, "SELECT 1\nFROM t", &latency, traceClickHouseEvent{Rows: &rows, EndInferred: true}),
		clickHouseScreenEvent(2, clickHouseEventQuery, traceServerSide, "SELECT * FROM missing", nil, traceClickHouseEvent{}),
		clickHouseScreenEvent(2, clickHouseEventException, traceServerSide, "SELECT * FROM missing", &latency, traceClickHouseEvent{ErrorCode: 60, ErrorName: "UNKNOWN_TABLE", Message: "no such table"}),
	}})
	model = next.(traceScreenModel)
	before := slices.Clone(model.events)
	text := strings.Join(model.buildDetail(0).raw, "\n")
	for _, want := range []string{"query_id: q-1", "user: edc", "database: db", "sql:\nSELECT 1\nFROM t", "response:", "event: clickhouse_end", "result: 10 rows (inferred)", "latency: 1.2ms"} {
		if !strings.Contains(text, want) {
			t.Fatalf("detail lacks %q: %q", want, text)
		}
	}
	text = strings.Join(model.buildDetail(2).raw, "\n")
	if !strings.Contains(text, "result: 60 (UNKNOWN_TABLE) no such table") || !strings.Contains(text, "error: 60 (UNKNOWN_TABLE) no such table") {
		t.Fatalf("exception detail = %q", text)
	}
	if !slices.EqualFunc(model.events, before, func(a, b captureEvent) bool { return reflect.DeepEqual(a, b) }) {
		t.Fatal("detail changed source events")
	}
}

func clickHouseHTTPScreenEvent(socket uint64, name, sql string, latency *float64, info traceClickHouseEvent) captureEvent {
	info.Interface = clickHouseInterfaceHTTP
	return clickHouseScreenEvent(socket, name, traceServerSide, sql, latency, info)
}

func TestTraceScreenClickHouseHTTPLabelsAndMixedList(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.width, model.height = 140, 10
	latency := func(value float64) *float64 { return &value }
	rows := uint64(3)
	model.events = []captureEvent{
		clickHouseScreenEvent(1, clickHouseEventQuery, traceServerSide, "SELECT 1", nil, traceClickHouseEvent{Interface: clickHouseInterfaceNative}),
		clickHouseScreenEvent(1, clickHouseEventEnd, traceServerSide, "SELECT 1", latency(0.4), traceClickHouseEvent{Interface: clickHouseInterfaceNative, Rows: &rows}),
		clickHouseHTTPScreenEvent(2, clickHouseEventQuery, "SELECT 2", nil, traceClickHouseEvent{}),
		clickHouseHTTPScreenEvent(2, clickHouseEventEnd, "SELECT 2", latency(1.5), traceClickHouseEvent{HTTPStatus: 200}),
		clickHouseHTTPScreenEvent(2, clickHouseEventQuery, "SELECT * FROM missing", nil, traceClickHouseEvent{}),
		clickHouseHTTPScreenEvent(2, clickHouseEventException, "SELECT * FROM missing", latency(2), traceClickHouseEvent{HTTPStatus: 404, ErrorCode: 60, Message: "Table missing\ndoes not exist"}),
	}
	visible := traceVisibleScreenRows(model)
	if len(visible) != 3 {
		t.Fatalf("display rows = %+v", visible)
	}
	for i, want := range []string{"server: SELECT 1 → 3 rows", "server: http: SELECT 2 → ok 200", "server: http: SELECT * FROM missing → 60 HTTP 404 Table missing does not exist"} {
		if _, destination, _, ok := model.displayRowLabels(visible[i]); !ok || destination != want {
			t.Fatalf("display row %d = %q, %t, want %q", i, destination, ok, want)
		}
	}
	destination, _ := traceClickHouseScrollLabels(model.events[3])
	if destination != "server: http: ↳ ok 200" {
		t.Fatalf("response scroll label = %q", destination)
	}
	destination, _ = traceClickHouseScrollLabels(model.events[1])
	if destination != "server: ↳ 3 rows" {
		t.Fatalf("native response scroll label = %q", destination)
	}
	text := strings.Join(model.buildDetail(2).raw, "\n")
	for _, want := range []string{"interface: http", "response:", "http_status: 200", "result: ok 200"} {
		if !strings.Contains(text, want) {
			t.Fatalf("http detail lacks %q: %q", want, text)
		}
	}
	text = strings.Join(model.buildDetail(4).raw, "\n")
	if !strings.Contains(text, "http_status: 404") || !strings.Contains(text, "error: 60 (-) Table missing") {
		t.Fatalf("http exception detail = %q", text)
	}
	if text = strings.Join(model.buildDetail(0).raw, "\n"); !strings.Contains(text, "interface: native") || strings.Contains(text, "http_status") {
		t.Fatalf("native detail = %q", text)
	}
	if text = strings.Join(traceClickHouseDetailLines(model.events[3]), "\n"); !strings.Contains(text, "http_status: 200") {
		t.Fatalf("response detail = %q", text)
	}
}

func TestTraceScreenClickHouseHTTPKeepAlivePairsRequestsInOrder(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := func(value float64) *float64 { return &value }
	events := []captureEvent{
		clickHouseHTTPScreenEvent(5, clickHouseEventQuery, "SELECT 1", nil, traceClickHouseEvent{}),
		clickHouseHTTPScreenEvent(5, clickHouseEventEnd, "SELECT 1", latency(1), traceClickHouseEvent{HTTPStatus: 200}),
		clickHouseHTTPScreenEvent(5, clickHouseEventQuery, "SELECT 1", nil, traceClickHouseEvent{}),
		clickHouseHTTPScreenEvent(5, clickHouseEventException, "SELECT 1", latency(2), traceClickHouseEvent{HTTPStatus: 500, ErrorCode: 241, Message: "memory"}),
		clickHouseHTTPScreenEvent(5, clickHouseEventQuery, "SELECT 1", nil, traceClickHouseEvent{}),
	}
	for _, event := range events {
		next, _ := model.Update(traceEventMsg{events: []captureEvent{event}})
		model = next.(traceScreenModel)
		if got, want := traceVisibleScreenRows(model), traceMySQLReferenceRows(model.events, model.first, 0); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
	}
	want := []traceScreenRow{{primary: 0, response: 1}, {primary: 2, response: 3}, {primary: 4, response: -1}}
	if got := traceVisibleScreenRows(model); !slices.Equal(got, want) {
		t.Fatalf("keep-alive rows = %+v, want %+v", got, want)
	}
	if _, destination, _, _ := model.displayRowLabels(want[1]); destination != "server: http: SELECT 1 → 241 HTTP 500 memory" {
		t.Fatalf("second request = %q", destination)
	}
}

func TestTraceScreenClickHouseHTTPSlowBoundary(t *testing.T) {
	model := newTraceScreenModel("clickhouse", tcpTraceOptions{slow: 1500 * time.Microsecond}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	latency := func(value float64) *float64 { return &value }
	pair := func(socket uint64, sql string, value *float64) []captureEvent {
		return []captureEvent{
			clickHouseHTTPScreenEvent(socket, clickHouseEventQuery, sql, nil, traceClickHouseEvent{}),
			clickHouseHTTPScreenEvent(socket, clickHouseEventEnd, sql, value, traceClickHouseEvent{HTTPStatus: 200}),
		}
	}
	model.events = slices.Concat(
		pair(1, "below", latency(1.499)),
		pair(2, "exact", latency(1.5)),
		[]captureEvent{clickHouseHTTPScreenEvent(3, clickHouseEventQuery, "unanswered", nil, traceClickHouseEvent{})},
	)
	want := []traceScreenRow{{primary: 2, response: 3}}
	if got := traceVisibleScreenRows(model); !slices.Equal(got, want) {
		t.Fatalf("slow rows = %+v, want %+v", got, want)
	}
}
