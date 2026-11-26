package edc

import (
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// --stats-only 화면은 event를 목록에 두지 않는다. 수집 goroutine이 종료 요약용 누적 집계에 event를 넣고, 화면은 그 집계를
// tick마다 읽어 그린다. 그래서 메모리가 event 수가 아니라 그룹 수에 비례한다.

// traceStatsInterval은 통계 화면을 다시 그리는 간격이다. 초당 rate도 이 간격의 차이로 잰다.
const traceStatsInterval = time.Second

// traceStatsSource는 수집 goroutine이 쓰고 화면이 읽는 누적 집계다. 화면 model은 값으로 복사되므로 pointer로 둔다.
type traceStatsSource struct {
	mu        sync.Mutex
	aggregate *traceAggregate
	started   time.Time
}

func newTraceStatsSource(aggregate *traceAggregate, started time.Time) *traceStatsSource {
	return &traceStatsSource{aggregate: aggregate, started: started}
}

func (source *traceStatsSource) observe(event captureEvent) {
	source.mu.Lock()
	source.aggregate.observe(event)
	source.mu.Unlock()
}

// report는 view의 누적 보고서다. 보고서는 집계의 값을 복사해 만들므로 lock 밖에서 읽어도 된다.
func (source *traceStatsSource) report(view string, now time.Time) traceGroupReport {
	source.mu.Lock()
	defer source.mu.Unlock()
	groups := source.aggregate.groups[view]
	if groups == nil {
		return traceGroupReport{GroupBy: view}
	}
	return groups.report(captureSummary{}, now.Sub(source.started))
}

type traceStatsTickMsg struct{ at time.Time }

func traceStatsTick() tea.Cmd {
	return tea.Tick(traceStatsInterval, func(at time.Time) tea.Msg { return traceStatsTickMsg{at: at} })
}

// traceStatsCount는 rate를 잴 때 앞 tick과 비교하는 값이다.
type traceStatsCount struct {
	events, bytes uint64
}

// traceStatsSnapshot은 화면이 그리는 마지막 보고서다. rate는 누적 평균이 아니라 앞 snapshot과의 차이로 잰다.
// 누적 평균은 부하가 멈춰도 천천히 줄어서 지금 상태를 보여 주지 못한다.
type traceStatsSnapshot struct {
	view     string
	at       time.Time
	report   traceGroupReport
	counts   map[string]traceStatsCount
	total    traceStatsCount
	measured bool
}

func traceStatsGroupKey(group traceGroupSummary) string {
	if group.Server {
		return group.Group + "\x00server"
	}
	return group.Group
}

// nextTraceStatsSnapshot은 report에 앞 snapshot과의 차이로 rate를 넣는다. 보기가 바뀌었거나 앞 snapshot이 없으면 비교할 값이
// 없으므로 report의 누적 평균을 그대로 둔다.
func nextTraceStatsSnapshot(previous traceStatsSnapshot, view string, report traceGroupReport, at time.Time) traceStatsSnapshot {
	next := traceStatsSnapshot{view: view, at: at, report: report, counts: make(map[string]traceStatsCount, len(report.Groups))}
	next.total = traceStatsCount{events: report.Events, bytes: report.TXBytes + report.RXBytes}
	for _, group := range report.Groups {
		next.counts[traceStatsGroupKey(group)] = traceStatsCount{events: group.Events, bytes: group.TXBytes + group.RXBytes}
	}
	seconds := at.Sub(previous.at).Seconds()
	if previous.view != view || previous.counts == nil || seconds <= 0 {
		return next
	}
	next.measured = true
	next.report.Rate, next.report.traceTraffic = traceStatsRate(previous.total, next.total, seconds, report.traceTraffic)
	for index, group := range next.report.Groups {
		before := previous.counts[traceStatsGroupKey(group)]
		next.report.Groups[index].Rate, next.report.Groups[index].traceTraffic = traceStatsRate(before, next.counts[traceStatsGroupKey(group)], seconds, group.traceTraffic)
	}
	return next
}

func traceStatsRate(before, after traceStatsCount, seconds float64, traffic traceTraffic) (float64, traceTraffic) {
	// 그룹이 새로 생긴 tick에는 before가 0이다. 그 그룹의 event는 모두 이 간격에 온 것이다.
	events, bytes := after.events-min(before.events, after.events), after.bytes-min(before.bytes, after.bytes)
	traffic.BytesPerSecond = float64(bytes) / seconds
	traffic.BitsPerSecond = traffic.BytesPerSecond * 8
	traffic.MegabitsPerSecond = traffic.BitsPerSecond / 1_000_000
	return float64(events) / seconds, traffic
}

// refreshStats는 집계를 다시 읽는다. tick과 보기를 바꿀 때 부른다.
func (model *traceScreenModel) refreshStats(at time.Time) {
	if model.stats == nil {
		return
	}
	model.statsSnapshot = nextTraceStatsSnapshot(model.statsSnapshot, model.groupBy, model.stats.report(model.groupBy, at), at)
}

// traceStatsViews는 통계 화면이 돌아가며 보이는 보기다. event 목록이 없으므로 event 스크롤은 뺀다.
func traceStatsViews(protocol string) []string {
	var views []string
	for _, view := range traceGroupViews(protocol) {
		if view != "" {
			views = append(views, view)
		}
	}
	return views
}

// traceStatsHelp는 통계 화면의 키 안내다. event를 두지 않으므로 고르기, 상세 보기, 미리 보기, filter 키는 없다.
func traceStatsHelp(protocol string) string {
	keys := []string{"tab view"}
	for _, view := range traceStatsViews(protocol) {
		for key, keyView := range traceGroupKeyViews {
			if keyView == view {
				keys = append(keys, key+" "+view)
			}
		}
	}
	return strings.Join(append(keys, "q quit", "ctrl-c stop"), "  ")
}

// updateStatsKey는 통계 화면의 키다. 보기만 바꾸고 나머지는 event 목록이 있어야 하는 키라 무시한다.
func (model traceScreenModel) updateStatsKey(key string) (traceScreenModel, tea.Cmd) {
	views := traceStatsViews(model.protocol)
	next := model.groupBy
	switch key {
	case "tab":
		next = nextTraceGroup(views, model.groupBy, 1)
	case "shift+tab":
		next = nextTraceGroup(views, model.groupBy, -1)
	case "q":
		return model, model.requestStop()
	default:
		if view, ok := traceGroupKeyViews[key]; ok && view != "" && slices.Contains(views, view) {
			next = view
		}
	}
	if next != model.groupBy {
		model.groupBy = next
		model.refreshStats(time.Now())
	}
	return model, nil
}
