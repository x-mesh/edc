package edc

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// traceStatsTestEvents는 path마다 요청과 응답을 count번 주고받은 HTTP event다.
func traceStatsTestEvents(t *testing.T, paths []string, count int) []captureEvent {
	t.Helper()
	tracker := newHTTPTracker(traceClientSide, false, false)
	var events []captureEvent
	at := uint64(1_000_000)
	for round := 0; round < count; round++ {
		for _, path := range paths {
			for _, packet := range []httpPacket{
				httpTestPacket("GET "+path+" HTTP/1.1\r\nHost: api.example\r\n\r\n", at, true),
				httpTestPacket("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n", at+1_000_000, false),
			} {
				event, ok := tracker.event(packet, 0)
				if !ok {
					t.Fatalf("packet %q made no event", packet.payload)
				}
				events = append(events, event)
				at += 2_000_000
			}
		}
	}
	return events
}

func newTraceStatsTestModel(t *testing.T) (traceScreenModel, *traceStatsSource, time.Time) {
	t.Helper()
	started := time.Unix(1000, 0)
	stats := newTraceStatsSource(newTraceAggregate("http", traceGroupViews("http")...), started)
	model := newTraceScreenModel("http", tcpTraceOptions{groupBy: traceGroupByPath, statsOnly: true}, nil, nil, nil)
	model.stats = stats
	model.refreshStats(started)
	return model, stats, started
}

// rate는 누적 평균이 아니라 tick 사이의 차이다. 부하가 멈추면 다음 tick에서 0이 되어야 한다.
func TestTraceStatsRatesFollowTheLastInterval(t *testing.T) {
	model, stats, started := newTraceStatsTestModel(t)
	for _, event := range traceStatsTestEvents(t, []string{"/a"}, 10) {
		stats.observe(event)
	}
	model = traceScreenApply(model, traceStatsTickMsg{at: started.Add(2 * time.Second)})
	report := model.groupReport()
	if len(report.Groups) != 1 || report.Groups[0].Events != 20 || report.Groups[0].Rate != 10 || report.Rate != 10 {
		t.Fatalf("first interval: %+v", report)
	}
	model = traceScreenApply(model, traceStatsTickMsg{at: started.Add(3 * time.Second)})
	report = model.groupReport()
	if report.Groups[0].Events != 20 || report.Groups[0].Rate != 0 || report.Rate != 0 || report.Groups[0].BytesPerSecond != 0 {
		t.Fatalf("after the load stopped: %+v", report)
	}
	if http := report.Groups[0].HTTP; http == nil || http.Requests != 10 || http.Responses != 10 || http.Unanswered != 0 {
		t.Fatalf("cumulative HTTP counts: %+v", http)
	}
}

// 새로 생긴 그룹은 앞 tick에 값이 없다. 그 그룹의 event는 모두 이번 간격에 온 것이다.
func TestTraceStatsNewGroupRateCountsTheWholeInterval(t *testing.T) {
	model, stats, started := newTraceStatsTestModel(t)
	for _, event := range traceStatsTestEvents(t, []string{"/a"}, 2) {
		stats.observe(event)
	}
	model = traceScreenApply(model, traceStatsTickMsg{at: started.Add(time.Second)})
	for _, event := range traceStatsTestEvents(t, []string{"/b"}, 3) {
		stats.observe(event)
	}
	model = traceScreenApply(model, traceStatsTickMsg{at: started.Add(2 * time.Second)})
	rates := map[string]float64{}
	for _, group := range model.groupReport().Groups {
		rates[group.Group] = group.Rate
	}
	if rates["/a"] != 0 || rates["/b"] != 6 {
		t.Fatalf("rates = %v", rates)
	}
}

// 보기를 바꾸면 앞 snapshot과 그룹이 달라 차이를 잴 수 없다. 다음 tick까지는 누적 평균을 보인다.
func TestTraceStatsViewChangeShowsTheNewViewAtOnce(t *testing.T) {
	model, stats, started := newTraceStatsTestModel(t)
	for _, event := range traceStatsTestEvents(t, []string{"/a", "/b"}, 1) {
		stats.observe(event)
	}
	model = traceScreenApply(model, traceStatsTickMsg{at: started.Add(time.Second)})
	model = traceScreenKey(model, "c")
	report := model.groupReport()
	if model.groupBy != traceGroupByProcess || report.GroupBy != traceGroupByProcess || len(report.Groups) != 1 || report.Groups[0].Group != "curl" || report.Events != 4 {
		t.Fatalf("process view: groupBy=%q report=%+v", model.groupBy, report)
	}
}

// 통계 화면에는 event 목록이 없다. event 스크롤로 가는 g와 Tab, event가 있어야 하는 키는 아무것도 바꾸지 않는다.
func TestTraceStatsKeysStayOnGroupViews(t *testing.T) {
	model, _, _ := newTraceStatsTestModel(t)
	model = traceScreenKey(model, "g", "/", "enter", "i", "f", "v", "m", "up")
	if model.groupBy != traceGroupByPath || model.filtering || model.detail != nil || model.split || model.follow || model.payloadLines || model.secrets || model.selected != -1 {
		t.Fatalf("model after event keys: groupBy=%q filtering=%v detail=%v split=%v", model.groupBy, model.filtering, model.detail != nil, model.split)
	}
	seen := map[string]bool{}
	for range traceStatsViews("http") {
		model = traceScreenApply(model, tea.KeyPressMsg{Code: tea.KeyTab})
		if model.groupBy == "" {
			t.Fatal("Tab reached the event scroll")
		}
		seen[model.groupBy] = true
	}
	if len(seen) != len(traceStatsViews("http")) {
		t.Fatalf("Tab visited %v, want every view in %v", seen, traceStatsViews("http"))
	}
}

func TestTraceStatsHeaderNamesTheMode(t *testing.T) {
	model, _, _ := newTraceStatsTestModel(t)
	model.width = 160
	header := ansi.Strip(strings.Join(traceScreenHeader(model), "\n"))
	for _, want := range []string{"stats only", "tab view", "u path"} {
		if !strings.Contains(header, want) {
			t.Errorf("header lacks %q:\n%s", want, header)
		}
	}
	for _, other := range []string{"enter detail", "/ filter", "g scroll"} {
		if strings.Contains(header, other) {
			t.Errorf("header offers %q:\n%s", other, header)
		}
	}
}

// 수집 goroutine이 쓰는 동안 화면이 읽는다. go test -race로 돌린다.
func TestTraceStatsSourceIsSafeAcrossGoroutines(t *testing.T) {
	stats := newTraceStatsSource(newTraceAggregate("http", traceGroupViews("http")...), time.Unix(1000, 0))
	events := traceStatsTestEvents(t, []string{"/a", "/b", "/c"}, 50)
	var group sync.WaitGroup
	group.Go(func() {
		for _, event := range events {
			stats.observe(event)
		}
	})
	for index := 0; index < 50; index++ {
		report := stats.report(traceGroupByPath, time.Unix(1001, 0))
		for _, row := range report.Groups {
			_ = fmt.Sprint(row.Group, row.Events, row.HTTP)
		}
	}
	group.Wait()
	if report := stats.report(traceGroupByPath, time.Unix(1001, 0)); report.Events != uint64(len(events)) {
		t.Fatalf("events = %d, want %d", report.Events, len(events))
	}
}

// 통계 화면은 줄 출력의 --group-by처럼 수집한다. 전체 화면이 켜는 payload와 진행 중 message 수집을 하지 않는다.
func TestTraceStatsCaptureScopeMatchesGroupedLines(t *testing.T) {
	options := tcpTraceOptions{groupBy: traceGroupByPath, statsOnly: true, port: 8080}
	applyTraceHTTPDefaults(&options)
	screen := traceScreenCaptureScope("http", options)
	lines := options.scope("http")
	if screen.payload || screen.payloadAll || screen.http2Payload || screen.progressive || screen.keepGzip || screen.showSecrets {
		t.Fatalf("stats scope collects payload: %+v", screen)
	}
	if fmt.Sprintf("%+v", screen) != fmt.Sprintf("%+v", lines) {
		t.Fatalf("stats scope = %+v\nlines scope = %+v", screen, lines)
	}
	if full := traceScreenCaptureScope("http", tcpTraceOptions{groupBy: traceGroupByPath, port: 8080}); !full.payload || !full.progressive {
		t.Fatalf("the event screen scope changed: %+v", full)
	}
}

func TestTraceStatsOnlyRejectsOptionsThatNeedEvents(t *testing.T) {
	for _, args := range [][]string{
		{"tcp", "--stats-only"},
		{"http", "--stats-only", "--raw"},
		{"http", "--stats-only", "--payload"},
		{"http", "--stats-only", "--payload=all"},
		{"http", "--stats-only", "--show-secrets"},
	} {
		if code := runTraceProtocol(args); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
	}
}

func traceLimitTestEvent(side, path string) captureEvent {
	return captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Side: side, Method: "GET", Target: "api.example", Path: path, Process: "curl"}
}

// 그룹이 상한에 닿은 뒤 처음 보는 값은 (other) 행에 더한다. 이미 있는 그룹은 계속 제 행에 센다.
func TestTraceGroupLimitFoldsNewValuesIntoOther(t *testing.T) {
	summarizer := newTraceGroupSummarizer("http", traceGroupByPath)
	for index := 0; index < traceGroupLimit; index++ {
		summarizer.observe(traceLimitTestEvent(traceClientSide, fmt.Sprintf("/users/%d", index)))
	}
	for _, path := range []string{"/users/0", "/late/1", "/late/2"} {
		summarizer.observe(traceLimitTestEvent(traceClientSide, path))
	}
	summarizer.observe(traceLimitTestEvent(traceServerSide, "/late/3"))
	report := summarizer.report(captureSummary{}, time.Second)
	groups := map[string]traceGroupSummary{}
	var total uint64
	for _, group := range report.Groups {
		groups[traceStatsGroupKey(group)] = group
		total += group.Events
	}
	if len(report.Groups) != traceGroupLimit+2 || total != report.Events || report.Events != traceGroupLimit+4 {
		t.Fatalf("groups=%d total=%d events=%d", len(report.Groups), total, report.Events)
	}
	if groups["/users/0"].Events != 2 || groups[traceGroupOther].Events != 2 || groups[traceGroupOther+"\x00server"].Events != 1 {
		t.Fatalf("/users/0=%d other=%d other server=%d", groups["/users/0"].Events, groups[traceGroupOther].Events, groups[traceGroupOther+"\x00server"].Events)
	}
	if http := groups[traceGroupOther].HTTP; http == nil || http.Requests != 2 {
		t.Fatalf("other HTTP counts = %+v", http)
	}
}

func TestTraceHTTPSummaryRowsStopAtTheGroupLimit(t *testing.T) {
	summarizer := newHTTPTraceSummarizer()
	for index := 0; index < traceGroupLimit+5; index++ {
		summarizer.observe(traceLimitTestEvent(traceClientSide, fmt.Sprintf("/users/%d", index)))
	}
	summarizer.observe(traceLimitTestEvent(traceClientSide, "/users/1"))
	if len(summarizer.rows) != traceGroupLimit+1 {
		t.Fatalf("rows = %d, want %d", len(summarizer.rows), traceGroupLimit+1)
	}
	if other := summarizer.rows[httpTraceKey{side: traceClientSide, path: traceGroupOther}]; other == nil || other.counts.Requests != 5 {
		t.Fatalf("other row = %+v", other)
	}
	if row := summarizer.rows[httpTraceKey{side: traceClientSide, method: "GET", host: "api.example", path: "/users/1"}]; row == nil || row.counts.Requests != 2 {
		t.Fatalf("/users/1 row = %+v", row)
	}
}
