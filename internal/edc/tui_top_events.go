package edc

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// topEventOpenAfter는 경고가 이벤트로 남으려면 이어져야 하는 시간이다. 한두 샘플 튀는 값이 기록을 채우지 않는다.
	topEventOpenAfter = 5 * time.Second
	// topEventCloseAfter는 경고가 꺼진 뒤 이벤트를 닫기까지 기다리는 시간이다. 경고가 깜박여도 한 이벤트로 남는다.
	topEventCloseAfter = 5 * time.Second
	// topEventLimit은 보관하는 이벤트 수다. 넘치면 오래된 것부터 버린다.
	topEventLimit = 100
	// topEventPaneMinWidth는 아래 영역에 이벤트 칸을 두는 최소 폭이다. 시각과 경고 문구가 들어가야 한다.
	topEventPaneMinWidth = 24
	// topEventMinLines는 아래 영역 이벤트 칸의 최소 줄 수다. 제목과 이벤트 두 줄이 보인다.
	topEventMinLines = 3
	// topEventMergeWindow는 한 줄로 합치는 이벤트의 시작 간격이다. 한 장애가 load, iowait, await 같은 경고를 거의 함께 켠다.
	topEventMergeWindow = 5 * time.Second
	// topEventTextWidth는 이벤트 줄의 경고 문구 칸이다. "await 3570ms +4"처럼 합친 수까지 들어간다.
	topEventTextWidth = 16
	// topEventNameWidth는 원인 후보의 process 이름 폭이다.
	topEventNameWidth = 10
)

// topEvent는 signal 경고 하나가 켜져 있던 구간이다. 히스토리 행은 topDashboardHistory개만 남으므로,
// 그 시점의 원인 후보를 문자열로 함께 보관해 행이 사라진 뒤에도 무엇이 앞섰는지 남긴다.
type topEvent struct {
	kind        string
	start, last time.Time
	ended       bool
	level       topLevel
	score       float64
	text        string
	culprit     string
	view        topView
	// sinceStart는 edc가 처음 받은 샘플부터 켜져 있던 경고다. 실제 시작은 그보다 앞일 수 있다.
	sinceStart bool
}

// topEventLog는 경고를 이벤트로 바꾼다. pending은 아직 topEventOpenAfter를 채우지 못한 경고다.
type topEventLog struct {
	events  []topEvent
	pending map[string]topEvent
	// first는 처음 받은 샘플 시각이다. 이때 이미 켜진 경고는 시작 시각을 알 수 없다.
	first time.Time
}

// level은 경고 단계다. score는 값을 위험 임계치로 나눈 값이라 1 이상이면 위험이다.
func (item topSignalItem) level() topLevel {
	if item.score >= 1 {
		return topLevelDanger
	}
	return topLevelWarn
}

// raise는 더 심한 값이 오면 이벤트의 경고 문구와 원인 후보를 그 순간 것으로 바꾼다. 가장 나빴던 때를 남긴다.
func (event *topEvent) raise(item topSignalItem, culprit func(topView) string) {
	// 시작 직후에는 process 목록이 아직 없어 원인 후보가 빈다. 값이 더 나빠지지 않아도 목록이 생기면 채운다.
	if event.text != "" && event.culprit == "" {
		event.culprit = culprit(event.view)
	}
	level := item.level()
	if event.text != "" && (level < event.level || (level == event.level && item.score <= event.score)) {
		return
	}
	event.level, event.score, event.text, event.view = level, item.score, item.text, item.view
	event.culprit = culprit(item.view)
}

// observe는 한 샘플의 경고를 받는다. at은 샘플 시각이라 수집 간격이 바뀌어도 같은 기준으로 지속 시간을 잰다.
func (log *topEventLog) observe(at time.Time, items []topSignalItem, culprit func(topView) string) {
	if log.pending == nil {
		log.pending = map[string]topEvent{}
	}
	if log.first.IsZero() {
		log.first = at
	}
	active := make(map[string]bool, len(items))
	for _, item := range items {
		active[item.kind] = true
		if index := log.ongoing(item.kind); index >= 0 {
			log.events[index].last = at
			log.events[index].raise(item, culprit)
			continue
		}
		event, seen := log.pending[item.kind]
		if !seen {
			event = topEvent{kind: item.kind, start: at, sinceStart: at.Equal(log.first)}
		}
		event.last = at
		event.raise(item, culprit)
		if at.Sub(event.start) < topEventOpenAfter {
			log.pending[item.kind] = event
			continue
		}
		delete(log.pending, item.kind)
		log.events = append(log.events, event)
		if len(log.events) > topEventLimit {
			log.events = log.events[len(log.events)-topEventLimit:]
		}
	}
	// 경고가 끊기면 지속 시간을 처음부터 다시 잰다.
	for kind := range log.pending {
		if !active[kind] {
			delete(log.pending, kind)
		}
	}
	for index := range log.events {
		event := &log.events[index]
		if !event.ended && !active[event.kind] && at.Sub(event.last) >= topEventCloseAfter {
			event.ended = true
		}
	}
}

func (log *topEventLog) ongoing(kind string) int {
	for index := len(log.events) - 1; index >= 0; index-- {
		if log.events[index].kind == kind && !log.events[index].ended {
			return index
		}
	}
	return -1
}

// topEventCulprit은 경고를 자세히 보이는 보기의 기준으로 그 시점에 앞선 묶음과 process를 적는다.
// 원인 후보이지 확인된 원인은 아니다. network 경고는 process로 가릴 수 없어 비운다.
func topEventCulprit(row topDashboardRow, view topView) string {
	if !row.processesValid {
		return ""
	}
	switch view {
	case topViewNetwork:
		return ""
	case topViewPressure:
		view = topViewDisk
	}
	parts, processes := []string{}, row.processes
	if groups := topViewProcessGroups(row.processTotal.Groups, view); len(groups) > 0 {
		group := groups[0]
		text := topProcessName(group.Name, topEventNameWidth) + " (" + topProcessGroupDetail(group) + ")"
		if value := topEventGroupValue(group, view); value != "" {
			text += " " + value
		}
		parts = append(parts, text)
		// 묶음에 든 process를 다시 적으면 같은 이름이 두 번 나와 다른 후보가 가려진다.
		processes = []topProcess{}
		for _, process := range row.processes {
			if topProcessGroupName(process.Command) != group.Name {
				processes = append(processes, process)
			}
		}
	}
	if process, ok := topEventLeadingProcess(processes, view); ok {
		text := topProcessName(process.Command, topEventNameWidth)
		if value := topEventProcessValue(process, view); value != "" {
			text += " " + value
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " · ")
}

func topEventGroupValue(group topProcessGroup, view topView) string {
	switch view {
	case topViewDisk:
		// I/O를 기다리는 수는 이름 뒤 괄호에 이미 있다.
		if group.DiskValid && group.DiskRead+group.DiskWrite > 0 {
			return topEventIOValue(group.DiskRead, group.DiskWrite)
		}
		return ""
	case topViewMemory:
		return formatProcessGroupRSS(group.RSS)
	}
	return fmt.Sprintf("%.0f%%", group.CPU)
}

// topEventLeadingProcess는 보기의 순위 기준으로 가장 앞선 process다. 디스크는 I/O, 없으면 I/O를 기다리는 process다.
func topEventLeadingProcess(processes []topProcess, view topView) (topProcess, bool) {
	if len(processes) == 0 {
		return topProcess{}, false
	}
	switch view {
	case topViewDisk:
		ranked := append([]topProcess(nil), processes...)
		sort.SliceStable(ranked, func(i, j int) bool { return topProcessIOTotal(ranked[i]) > topProcessIOTotal(ranked[j]) })
		if topProcessIOTotal(ranked[0]) > 0 {
			return ranked[0], true
		}
		for _, process := range processes {
			if process.State == topProcessStateBlocked {
				return process, true
			}
		}
		return topProcess{}, false
	case topViewMemory:
		leading := processes[0]
		for _, process := range processes[1:] {
			if process.RSS > leading.RSS {
				leading = process
			}
		}
		return leading, true
	}
	leading := processes[0]
	for _, process := range processes[1:] {
		if process.CPU > leading.CPU {
			leading = process
		}
	}
	return leading, true
}

func topEventProcessValue(process topProcess, view topView) string {
	switch view {
	case topViewDisk:
		if process.DiskValid && process.DiskRead+process.DiskWrite > 0 {
			return topEventIOValue(process.DiskRead, process.DiskWrite)
		}
		return topProcessStateName(process.State)
	case topViewMemory:
		return formatProcessRSS(process.RSS)
	}
	return fmt.Sprintf("%.0f%%", process.CPU)
}

// topEventIOValue는 읽기와 쓰기 중 큰 쪽만 적어 한 줄에 원인 후보 둘이 들어가게 한다.
func topEventIOValue(read, write float64) string {
	if write >= read {
		return "w " + formatRate(write)
	}
	return "r " + formatRate(read)
}

func formatTopEventDuration(value time.Duration) string {
	seconds := int(value.Round(time.Second) / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, seconds/60%60)
}

// topEventCluster는 거의 함께 시작한 이벤트다. 시작 순으로 담는다.
type topEventCluster []topEvent

// topEventClusters는 이벤트를 시작 순으로 놓고, 묶음의 첫 시작에서 topEventMergeWindow 안에 시작한 것을 합친다.
func topEventClusters(events []topEvent) []topEventCluster {
	sorted := append([]topEvent(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	clusters := []topEventCluster{}
	for _, event := range sorted {
		if last := len(clusters) - 1; last >= 0 && event.start.Sub(clusters[last][0].start) <= topEventMergeWindow {
			clusters[last] = append(clusters[last], event)
			continue
		}
		clusters = append(clusters, topEventCluster{event})
	}
	return clusters
}

// worst는 묶음에서 단계와 값이 가장 나쁜 이벤트다. 한 줄로 줄일 때 이 이벤트의 문구와 원인 후보를 보인다.
func (cluster topEventCluster) worst() topEvent {
	worst := cluster[0]
	for _, event := range cluster[1:] {
		if event.level > worst.level || (event.level == worst.level && event.score > worst.score) {
			worst = event
		}
	}
	return worst
}

// span은 묶음이 이어진 구간이다. 하나라도 진행 중이면 끝은 now다.
func (cluster topEventCluster) span(now time.Time) (time.Time, bool) {
	end, ongoing := time.Time{}, false
	for _, event := range cluster {
		if !event.ended {
			ongoing = true
		} else if event.last.After(end) {
			end = event.last
		}
	}
	if ongoing {
		return now, true
	}
	return end, false
}

// topEventStart는 시작 시각 앞에 붙이는 한 칸이다. edc가 켜질 때 이미 켜져 있던 이벤트는 ≤를 붙여 그 전에 시작했을 수 있음을 보인다.
func topEventStart(event topEvent) string {
	if event.sinceStart {
		return "≤" + event.start.Format("15:04:05")
	}
	return " " + event.start.Format("15:04:05")
}

// topEventLine은 이벤트 칸의 한 줄이다. 진행 중이면 ●를 두고, 시각과 경고 문구에 가장 나쁜 단계의 색을 입힌다.
// 함께 시작한 이벤트는 +N으로 세고, 폭이 좁으면 원인 후보와 지속 시간부터 뺀다.
func topEventLine(cluster topEventCluster, now time.Time, width int, color bool) string {
	worst := cluster.worst()
	end, ongoing := cluster.span(now)
	marker := " "
	if ongoing {
		marker = "●"
	}
	text := worst.text
	if len(cluster) > 1 {
		text += fmt.Sprintf(" +%d", len(cluster)-1)
	}
	textWidth := max(1, min(topEventTextWidth, width-11))
	line := topPaint(marker+topEventStart(cluster[0])+" "+topFitCell(text, textWidth, true), worst.level, color)
	rest := width - 11 - textWidth
	if rest < 8 {
		return line
	}
	duration := " " + topFitCell(formatTopEventDuration(end.Sub(cluster[0].start)), 7, false)
	if rest-8 >= 2 {
		line += " " + topFitCell(worst.culprit, rest-9, true)
	}
	return line + duration
}

func (model topModel) eventList() []topEvent {
	if model.events == nil {
		return nil
	}
	return model.events.events
}

// latestAt은 진행 중인 이벤트의 지속 시간을 재는 기준이다. 벽시계가 아니라 마지막 샘플 시각이라 멈춘 화면에서 늘지 않는다.
func (model topModel) latestAt() time.Time {
	if len(model.rows) == 0 {
		return time.Time{}
	}
	return model.rows[len(model.rows)-1].at
}

// recordEvents는 새 행의 경고를 이벤트 기록에 넘긴다. 원인 후보는 이벤트가 가장 나빠진 순간의 행에서만 만든다.
func (model topModel) recordEvents(row topDashboardRow) topModel {
	if model.events == nil {
		model.events = &topEventLog{}
	}
	items := topDashboardSignalItems(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits)
	model.events.observe(row.at, items, func(view topView) string { return topEventCulprit(row, view) })
	return model
}

// eventsInFooter는 이벤트를 아래 영역에 둘 수 있는지다. 넓은 화면은 왼쪽 빈자리에, 좁은 화면은 process 목록 옆에 둔다.
// 필터를 걸었거나 상세 패널이 왼쪽을 차지하거나 폭이 모자라면 진행 중인 이벤트만 한 줄로 알린다.
func (model topModel) eventsInFooter() bool {
	if model.processFilter.active() || model.view == topViewSplit {
		return false
	}
	if model.displayWidth() >= topFooterDockWidth {
		return len(model.infoLines()) == 0
	}
	return model.displayWidth()-topFooterProcessWidth-topFooterGap >= topEventPaneMinWidth
}

// eventLines는 아래 영역의 이벤트 칸이다. 함께 시작한 이벤트는 한 줄로 합치고, 최근 것이 위에 온다.
func (model topModel) eventLines(width, count int) []string {
	if count <= 0 {
		return nil
	}
	lines := []string{topFitCell("events · e list · [ ] jump", width, true)}
	clusters := topEventClusters(model.eventList())
	if len(clusters) == 0 {
		return append(lines, topFitCell("no events yet", width, true))[:min(count, 2)]
	}
	now := model.latestAt()
	for index := len(clusters) - 1; index >= 0 && len(lines) < count; index-- {
		lines = append(lines, topEventLine(clusters[index], now, width, model.limits.color))
	}
	return lines
}

// eventTicker는 아래 영역에 이벤트 칸이 없을 때 진행 중인 이벤트 중 가장 심한 것을 한 줄로 알린다. 지난 이벤트는 e 목록에서 본다.
func (model topModel) eventTicker() string {
	if model.eventsInFooter() {
		return ""
	}
	ongoing := []topEvent{}
	for _, event := range model.eventList() {
		if !event.ended {
			ongoing = append(ongoing, event)
		}
	}
	if len(ongoing) == 0 {
		return ""
	}
	worst := ongoing[0]
	for _, event := range ongoing[1:] {
		if event.level > worst.level || (event.level == worst.level && event.score > worst.score) {
			worst = event
		}
	}
	text := topPaint("●"+topEventStart(worst)+" "+worst.text, worst.level, model.limits.color)
	if worst.culprit != "" {
		text += " · " + worst.culprit
	}
	if len(ongoing) > 1 {
		text += fmt.Sprintf(" · +%d", len(ongoing)-1)
	}
	return text + " · e events"
}

// eventStarts는 이벤트가 시작된 샘플 시각이다. 히스토리 표에서 그 행에 !를 붙인다.
func (model topModel) eventStarts() map[int64]topLevel {
	starts := map[int64]topLevel{}
	for _, event := range model.eventList() {
		key := event.start.UnixNano()
		if level, seen := starts[key]; !seen || event.level > level {
			starts[key] = event.level
		}
	}
	return starts
}

// eventListLines는 e 목록이다. 아래 영역처럼 함께 시작한 이벤트를 한 항목으로 합치고 최근 것이 위에 온다.
// 항목은 대표 경고, 함께 켜진 경고의 최고값, 원인 후보 순으로 두세 줄을 쓴다.
func (model topModel) eventListLines() []string {
	clusters := topEventClusters(model.eventList())
	if len(clusters) == 0 {
		return []string{"no events yet · warnings that last " + formatTopEventDuration(topEventOpenAfter) + " appear here"}
	}
	now := model.latestAt()
	entries, sinceStart := make([][]string, len(clusters)), false
	for position := range clusters {
		cluster := clusters[len(clusters)-1-position]
		entries[position] = model.eventListEntry(cluster, now, position == model.eventSelected)
		sinceStart = sinceStart || cluster[0].sinceStart
	}
	room := max(1, model.height-1)
	if sinceStart {
		room = max(1, room-1)
	}
	// 고른 항목이 보이도록 그 위로 들어가는 만큼만 앞 항목을 남긴다.
	first, used := model.eventSelected, 0
	for first >= 0 && used+len(entries[first]) <= room {
		used += len(entries[first])
		first--
	}
	first = min(model.eventSelected, first+1)
	lines := []string{}
	for position := first; position < len(entries) && len(lines)+len(entries[position]) <= room; position++ {
		lines = append(lines, entries[position]...)
	}
	if sinceStart {
		lines = append(lines, "≤ was already on when edc started; it may have begun earlier")
	}
	return lines
}

func (model topModel) eventListEntry(cluster topEventCluster, now time.Time, selected bool) []string {
	worst := cluster.worst()
	end, ongoing := cluster.span(now)
	marker, state, until := " ", " ", end.Format("15:04:05")
	if selected {
		marker = ">"
	}
	if ongoing {
		state, until = "●", "now     "
	}
	text := worst.text
	if len(cluster) > 1 {
		text += fmt.Sprintf(" +%d", len(cluster)-1)
	}
	head := fmt.Sprintf("%s %s%s–%s %7s  ", marker, state, topEventStart(cluster[0]), until, formatTopEventDuration(end.Sub(cluster[0].start)))
	line := head + topPaint(text, worst.level, model.limits.color && !selected)
	if selected && model.limits.color {
		line = topBannerStyle + topDashboardFitWidth(line, model.displayWidth()) + topColorReset
	}
	lines := []string{line}
	if len(cluster) > 1 {
		others := []string{}
		for _, event := range cluster {
			if event.kind != worst.kind {
				others = append(others, topPaint(event.text, event.level, model.limits.color))
			}
		}
		lines = append(lines, "      also: "+strings.Join(others, " · "))
	}
	culprit := worst.culprit
	if culprit == "" {
		culprit = "no process candidate in that sample"
	}
	return append(lines, "      "+culprit)
}

// newestCluster는 e 목록의 position번째 항목이다. 목록은 최근 것이 위다.
func (model topModel) newestCluster(position int) (topEventCluster, bool) {
	clusters := topEventClusters(model.eventList())
	if position < 0 || position >= len(clusters) {
		return nil, false
	}
	return clusters[len(clusters)-1-position], true
}

// jumpToEvent는 히스토리를 start 행으로 옮기고 event의 문구를 알린다. 행마다 그 시점의 process 목록이 남아 있어 당시 후보를 다시 본다.
func (model topModel) jumpToEvent(event topEvent, start time.Time) topModel {
	at := start.Format("15:04:05")
	if len(model.rows) == 0 || model.rows[0].at.After(start) {
		model.notice = "event " + at + " is older than the history · its summary stays in e"
		return model
	}
	index := sort.Search(len(model.rows), func(i int) bool { return !model.rows[i].at.Before(start) })
	model.selected, model.follow, model.processFocus = min(index, len(model.rows)-1), false, false
	model.notice = "event " + at + " · " + event.text + " · End live"
	return model
}

// stepEvent는 [와 ]로 지금 고른 행보다 앞이나 뒤에서 시작한 가장 가까운 이벤트 묶음으로 옮긴다.
// 한 줄로 합친 이벤트를 하나씩 밟지 않도록 묶음의 첫 시작만 본다.
func (model topModel) stepEvent(direction int) topModel {
	clusters := topEventClusters(model.eventList())
	if len(clusters) == 0 {
		model.notice = "no events yet"
		return model
	}
	reference := model.latestAt()
	if !model.follow && model.selected < len(model.rows) {
		reference = model.rows[model.selected].at
	}
	if direction < 0 {
		for index := len(clusters) - 1; index >= 0; index-- {
			if clusters[index][0].start.Before(reference) {
				return model.jumpToEvent(clusters[index].worst(), clusters[index][0].start)
			}
		}
		model.notice = "no earlier event"
		return model
	}
	for _, cluster := range clusters {
		if cluster[0].start.After(reference) {
			return model.jumpToEvent(cluster.worst(), cluster[0].start)
		}
	}
	model.notice = "no later event"
	return model
}
