package edc

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const traceScreenEventLimit = 10000

// traceScreenGCPercent는 전체 화면 trace의 GC 빈도를 낮춘다. 요약만 쌓으면 heap이 작아 GC가 자주 돌고, 할당이 많은
// group 화면 갱신이 느려져 초당 수만 event에서 ring buffer 유실이 늘었다. 800이면 유실이 모든 event를 두던
// 때와 같고, 메모리는 일정하다.
const traceScreenGCPercent = 800

type traceEventMsg struct{ events []captureEvent }
type traceFinishedMsg struct {
	summary captureSummary
	err     error
}
type traceStopMsg struct{}

type traceScreenModel struct {
	protocol    string
	side        string
	groupBy     string
	process     string
	destination string
	duration    time.Duration
	slow        time.Duration
	started     time.Time
	events      []captureEvent
	arrivals    []time.Time
	truncated   bool
	filter      string
	input       textinput.Model
	filtering   bool
	stopping    bool
	received    uint64
	width       int
	height      int
	stop        func()
	finished    *traceFinishedMsg
	eventCh     <-chan captureEvent
	resultCh    <-chan traceFinishedMsg
	// first는 events[0]의 번호다. 앞의 event를 버려도 번호는 그대로라서, 고른 event와 payload 전체를 번호로 찾는다.
	first int
	// selected는 고른 event의 번호다. -1이면 고른 event가 없고 목록이 새 event를 따라간다.
	selected int
	detail   *traceDetail
	payloads traceFullPayloads
	// gzipped는 gzip message의 원본 byte다. 상세 보기에서 z로 본문을 풀 때 쓴다.
	gzipped traceFullPayloads
	// payloadLines는 목록에서 event 아래에 payload 줄을 보일지다. v로 바꾼다. --payload로 시작하면 켜져 있다.
	payloadLines bool
	// secrets는 인증 header 값을 보일지다. m으로 바꾼다. 화면은 원문을 두었다가 그릴 때 가린다.
	secrets bool
	// follow면 상세 보기가 가장 최근 event를 따라간다. decode면 상세 보기가 gzip 본문을 푼다.
	follow bool
	decode bool
	// split이면 목록 아래에 고른 event나 가장 최근 event의 message를 보인다. i로 바꾼다. previewOffset은 J/K로 옮긴
	// 미리 보기의 위치다.
	split         bool
	preview       *tracePreview
	previewOffset int
	// container는 --container로 고른 container의 이름이다. 목록이 조용해도 거르는 중임을 머리글에 보인다.
	container       string
	mysqlRows       []traceScreenRow
	mysqlRowsHead   int
	mysqlRowIndex   map[int]int
	mysqlPending    map[traceMySQLPairKey][]int
	mysqlPendingKey map[int]traceMySQLPairKey
}

type traceScreenRow struct {
	primary  int
	response int
	hidden   bool
}

type traceMySQLPairKey struct {
	socket  uint64
	side    string
	command string
	sql     string
}

func (model traceScreenModel) displayRows() []traceScreenRow {
	if model.protocol != "mysql" {
		rows := make([]traceScreenRow, 0, len(model.events))
		for index := range model.events {
			rows = append(rows, traceScreenRow{primary: model.first + index, response: -1})
		}
		return rows
	}
	if model.mysqlRows == nil && len(model.events) != 0 {
		return traceMySQLReferenceRows(model.events, model.first, model.slow)
	}
	return model.mysqlRows[model.mysqlRowsHead:]
}

// traceMySQLReferenceRows is the test reference for the incremental screen index.
func traceMySQLReferenceRows(events []captureEvent, first int, slow time.Duration) []traceScreenRow {
	rows := make([]traceScreenRow, 0, len(events))
	pending := map[traceMySQLPairKey][]int{}
	rowIndexByPrimary := make(map[int]int, len(events))
	for index, event := range events {
		number := first + index
		if event.MySQL == nil {
			rows = append(rows, traceScreenRow{primary: number, response: -1})
			rowIndexByPrimary[number] = len(rows) - 1
			continue
		}
		key := traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.MySQL.Command, sql: event.MySQL.SQL}
		if mysqlResponseEvent(event.Event) {
			queue := pending[key]
			if len(queue) > 0 {
				primary := queue[0]
				pending[key] = queue[1:]
				if rowIndex, ok := rowIndexByPrimary[primary]; ok {
					rows[rowIndex].response = number
				}
				continue
			}
			rows = append(rows, traceScreenRow{primary: number, response: -1})
			rowIndexByPrimary[number] = len(rows) - 1
			continue
		}
		rows = append(rows, traceScreenRow{primary: number, response: -1})
		rowIndexByPrimary[number] = len(rows) - 1
		if event.Event != mysqlEventTLS {
			pending[key] = append(pending[key], number)
		}
	}
	if slow == 0 {
		return rows
	}
	threshold := float64(slow) / float64(time.Millisecond)
	filtered := rows[:0]
	for _, row := range rows {
		if row.response < first || row.response >= first+len(events) {
			continue
		}
		response := events[row.response-first]
		if response.LatencyMS != nil && *response.LatencyMS >= threshold {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (model traceScreenModel) displayRowIsSlow(row traceScreenRow) bool {
	if row.response < model.first || row.response >= model.first+len(model.events) {
		return false
	}
	response := model.events[row.response-model.first]
	return response.LatencyMS != nil && *response.LatencyMS >= float64(model.slow)/float64(time.Millisecond)
}

func (model *traceScreenModel) appendMySQLRow(event captureEvent, number int) {
	if model.mysqlRowIndex == nil {
		model.mysqlRowIndex = map[int]int{}
		model.mysqlPending = map[traceMySQLPairKey][]int{}
		model.mysqlPendingKey = map[int]traceMySQLPairKey{}
	}
	appendRow := func(row traceScreenRow) {
		model.mysqlRowIndex[row.primary] = len(model.mysqlRows)
		model.mysqlRows = append(model.mysqlRows, row)
	}
	if event.MySQL == nil {
		appendRow(traceScreenRow{primary: number, response: -1})
		return
	}
	key := traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.MySQL.Command, sql: event.MySQL.SQL}
	if mysqlResponseEvent(event.Event) {
		appendRow(traceScreenRow{primary: number, response: -1, hidden: true})
		if pending := model.mysqlPending[key]; len(pending) > 0 {
			primary := pending[0]
			if len(pending) == 1 {
				delete(model.mysqlPending, key)
			} else {
				model.mysqlPending[key] = pending[1:]
			}
			delete(model.mysqlPendingKey, primary)
			if index, ok := model.mysqlRowIndex[primary]; ok {
				model.mysqlRows[index].response = number
			}
			return
		}
		model.mysqlRows[len(model.mysqlRows)-1].hidden = false
		return
	}
	appendRow(traceScreenRow{primary: number, response: -1})
	if event.Event != mysqlEventTLS {
		model.mysqlPending[key] = append(model.mysqlPending[key], number)
		model.mysqlPendingKey[number] = key
	}
}

func (model *traceScreenModel) evictMySQLRow(number int) {
	index, ok := model.mysqlRowIndex[number]
	if !ok {
		return
	}
	row := model.mysqlRows[index]
	if key, pending := model.mysqlPendingKey[number]; pending {
		queue := model.mysqlPending[key]
		if len(queue) > 0 && queue[0] == number {
			if len(queue) == 1 {
				delete(model.mysqlPending, key)
			} else {
				model.mysqlPending[key] = queue[1:]
			}
		}
		delete(model.mysqlPendingKey, number)
	}
	delete(model.mysqlRowIndex, number)
	if row.response >= 0 {
		if responseIndex, retained := model.mysqlRowIndex[row.response]; retained {
			model.mysqlRows[responseIndex].hidden = false
		}
	}
	if index != model.mysqlRowsHead {
		model.mysqlRows[index].hidden = true
		return
	}
	model.mysqlRowsHead++
	if model.mysqlRowsHead*2 >= len(model.mysqlRows) {
		copy(model.mysqlRows, model.mysqlRows[model.mysqlRowsHead:])
		model.mysqlRows = model.mysqlRows[:len(model.mysqlRows)-model.mysqlRowsHead]
		model.mysqlRowsHead = 0
		for index, current := range model.mysqlRows {
			model.mysqlRowIndex[current.primary] = index
		}
	}
}

func (model traceScreenModel) displayRowLabels(row traceScreenRow) (captureEvent, string, string, bool) {
	event, ok := model.displayRow(row)
	if !ok {
		return captureEvent{}, "", "", false
	}
	destination, label := traceScrollLabels(event)
	if row.response >= model.first && row.response < model.first+len(model.events) && event.Protocol == "mysql" {
		response := model.events[row.response-model.first]
		if response.MySQL != nil {
			destination += " → " + traceMySQLResultText(response)
			label = response.Event
			if response.LatencyMS != nil {
				label += " " + traceLatency(response.LatencyMS, "ms")
			}
		}
	}
	return event, destination, label, true
}

func (model traceScreenModel) displayRow(row traceScreenRow) (captureEvent, bool) {
	if row.primary < model.first || row.primary >= model.first+len(model.events) {
		return captureEvent{}, false
	}
	return model.events[row.primary-model.first], true
}

func (model traceScreenModel) displayRowMatches(row traceScreenRow) bool {
	if row.hidden || (model.protocol == "mysql" && model.slow > 0 && !model.displayRowIsSlow(row)) {
		return false
	}
	event, ok := model.displayRow(row)
	if !ok {
		return false
	}
	if traceEventMatchesText(event, model.filter) {
		return true
	}
	if row.response < 0 || row.response < model.first || row.response >= model.first+len(model.events) {
		return false
	}
	return traceEventMatchesText(model.events[row.response-model.first], model.filter)
}

func (model traceScreenModel) displayRowForSelected(rows []traceScreenRow) int {
	for index, row := range rows {
		if row.primary == model.selected || row.response == model.selected {
			return index
		}
	}
	return -1
}

func newTraceScreenModel(protocol string, options tcpTraceOptions, eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg, stop func()) traceScreenModel {
	input := textinput.New()
	input.Prompt = "filter / "
	input.Placeholder = "process, destination, event"
	input.CharLimit = 256
	input.SetWidth(48)
	return traceScreenModel{
		protocol: protocol, side: options.side, groupBy: options.groupBy, process: options.process, destination: options.destination,
		duration: options.duration, slow: options.slow, started: time.Now(), eventCh: eventCh, resultCh: resultCh, stop: stop, input: input,
		width: 80, height: 24, selected: -1, payloads: traceFullPayloads{minimum: httpPayloadHead},
		payloadLines: options.payload != "", secrets: options.showSecrets, container: traceContainerName(options.container),
	}
}

func (model traceScreenModel) Init() tea.Cmd {
	return waitTraceMessage(model.eventCh, model.resultCh)
}

// traceEventBatchLimit은 한 메시지에 담는 event 수의 상한이다. 한 번의 Update가 너무 길어져 키 입력이 늦어지지 않게 한다.
const traceEventBatchLimit = 4096

// waitTraceMessage는 채널에 쌓인 event를 한 메시지로 묶는다. event마다 메시지를 하나씩 보내면 화면이 초당 약
// 110건만 소화해, 그보다 많은 트래픽에서는 화면이 계속 뒤처지고 수집도 채널에서 막힌다.
func waitTraceMessage(eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg) tea.Cmd {
	return func() tea.Msg {
		var first captureEvent
		select {
		case first = <-eventCh:
		case result := <-resultCh:
			return result
		}
		events := []captureEvent{first}
		for len(events) < traceEventBatchLimit {
			select {
			case event := <-eventCh:
				events = append(events, event)
			default:
				return traceEventMsg{events: events}
			}
		}
		return traceEventMsg{events: events}
	}
}

// Update는 message를 처리한 뒤 미리 보기를 맞춘다. 고른 event는 키와 새 event 양쪽에서 바뀌므로 한곳에서 맞춘다.
func (model traceScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := model.update(msg)
	updated := next.(traceScreenModel)
	updated.refreshPreview()
	return updated, cmd
}

func (model traceScreenModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.input.SetWidth(max(20, min(60, value.Width-12)))
		if model.detail != nil {
			model.detail.wrap(model.width)
			model.detail.scroll(0, max(1, model.height-3))
		}
		return model, nil
	case traceEventMsg:
		now := time.Now()
		for _, event := range value.events {
			model.received++
			if traceProtocol(event) == model.protocol {
				// 화면은 event를 최대 10,000건 보관한다. --payload=all의 1MiB payload를 그대로 두면 메모리가 GB 단위로
				// 커지므로 목록에는 앞부분만 남긴다. 상세 보기에 쓸 전체는 최근 것만 상한 안에서 따로 둔다.
				model.payloads.keep(model.first+len(model.events), event.Payload)
				model.gzipped.keep(model.first+len(model.events), string(event.gzipped))
				event.Payload, event.gzipped = traceTrimText(event.Payload, httpPayloadHead), nil
				model.events = append(model.events, event)
				model.arrivals = append(model.arrivals, now)
				if model.protocol == "mysql" {
					model.appendMySQLRow(event, model.first+len(model.events)-1)
				}
			}
		}
		if len(model.events) > traceScreenEventLimit {
			drop := len(model.events) - traceScreenEventLimit
			if model.protocol == "mysql" {
				for number := model.first; number < model.first+drop; number++ {
					model.evictMySQLRow(number)
				}
			}
			model.first += drop
			model.events = model.events[drop:]
			model.arrivals = model.arrivals[drop:]
			model.truncated = true
			model.payloads.drop(model.first)
			model.gzipped.drop(model.first)
			model.normalizeSelection()
		}
		if model.follow {
			model.followNewest()
		}
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	case traceFinishedMsg:
		model.finished = &value
		return model, tea.Quit
	case traceStopMsg:
		model.requestStop()
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	case tea.KeyPressMsg:
		return model.updateKey(value)
	}
	return model, nil
}

func (model *traceScreenModel) normalizeSelection() {
	if model.selected < 0 {
		return
	}
	if model.protocol == "mysql" {
		rows := model.displayRows()
		if index := model.displayRowForSelected(rows); index >= 0 && model.displayRowMatches(rows[index]) {
			model.selected = rows[index].primary
			return
		}
		for _, row := range rows {
			if model.displayRowMatches(row) {
				model.selected = row.primary
				return
			}
		}
		model.selected, model.preview, model.previewOffset = -1, nil, 0
		return
	}
	index := model.selected - model.first
	if index >= 0 && index < len(model.events) && traceEventMatchesText(model.events[index], model.filter) {
		return
	}
	for index, event := range model.events {
		if traceEventMatchesText(event, model.filter) {
			model.selected = model.first + index
			return
		}
	}
	model.selected, model.preview, model.previewOffset = -1, nil, 0
}

func (model traceScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		model.requestStop()
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	}
	if model.detail != nil {
		return model.updateDetailKey(key.String()), nil
	}
	if model.filtering {
		switch key.String() {
		case "enter":
			model.filter = strings.TrimSpace(model.input.Value())
			model.input.Blur()
			model.filtering = false
			// 고른 event가 새 filter에 맞지 않으면 목록에 보이지 않으므로, 고른 것을 풀고 다시 따라간다.
			model.selected = -1
			return model, nil
		case "esc":
			model.input.SetValue(model.filter)
			model.input.Blur()
			model.filtering = false
			return model, nil
		}
		var cmd tea.Cmd
		model.input, cmd = model.input.Update(key)
		return model, cmd
	}
	switch key.String() {
	case "/":
		model.input.SetValue(model.filter)
		model.filtering = true
		return model, model.input.Focus()
	case "s", "t", "p", "c", "e", "u", "g":
		if view := traceGroupKeyViews[key.String()]; slices.Contains(traceGroupViews(model.protocol), view) {
			model.groupBy = view
		}
		return model, nil
	case "tab":
		model.groupBy = nextTraceGroup(traceGroupViews(model.protocol), model.groupBy, 1)
		return model, nil
	case "shift+tab":
		model.groupBy = nextTraceGroup(traceGroupViews(model.protocol), model.groupBy, -1)
		return model, nil
	case "esc":
		// Mac 자판에는 End가 없는 경우가 많다. 고른 event가 있으면 Esc가 먼저 선택을 풀고 다시 따라간다.
		if model.selected >= 0 && model.groupBy == "" {
			model.selected = -1
			return model, nil
		}
		model.filter = ""
		return model, nil
	case "up", "k", "down", "j", "pgup", "pgdown", "b", "space":
		// 고르는 동안에는 목록이 새 event를 따라가지 않는다. end를 누르면 다시 따라간다.
		if model.groupBy == "" {
			step, count, page := 1, 1, max(1, model.listRows()-1)
			switch key.String() {
			case "up", "k":
				step = -1
			case "pgup", "b":
				step, count = -1, page
			case "pgdown", "space":
				count = page
			}
			model.selected = model.moveSelection(step, count)
		}
		return model, nil
	case "end", "l":
		model.selected = -1
		return model, nil
	case "v":
		model.payloadLines = !model.payloadLines
		return model, nil
	case "m":
		model.secrets = !model.secrets
		return model, nil
	case "f":
		if model.groupBy == "" {
			model.openFollow()
		}
		return model, nil
	case "i":
		if model.groupBy == "" {
			model.split, model.preview, model.previewOffset = !model.split, nil, 0
		}
		return model, nil
	case "J", "K":
		if model.split {
			model.scrollPreview(map[string]int{"J": 1, "K": -1}[key.String()])
		}
		return model, nil
	case "z":
		if model.split && model.protocol == "http" {
			model.decode = !model.decode
		}
		return model, nil
	case "enter":
		if model.groupBy == "" {
			model.openDetail()
		}
		return model, nil
	case "q":
		model.requestStop()
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	}
	return model, nil
}

// traceGroupCycle은 Tab이 넘겨 가는 순서다. 기본 화면인 event 스크롤에서 시작해 s, t, p, c, e 키와 같은 순서로 간다.
var traceGroupCycle = []string{"", traceGroupBySource, traceGroupByTarget, traceGroupByPort, traceGroupByProcess, traceGroupByEvent}

var traceGroupKeyViews = map[string]string{"g": "", "s": traceGroupBySource, "t": traceGroupByTarget, "p": traceGroupByPort, "c": traceGroupByProcess, "e": traceGroupByEvent, "u": traceGroupByPath}

func nextTraceGroup(views []string, current string, step int) string {
	index := 0
	for position, groupBy := range views {
		if groupBy == current {
			index = position
		}
	}
	return views[(index+step+len(views))%len(views)]
}

// traceScreenHelp는 protocol이 쓰는 보기의 키만 보여 준다.
func traceScreenHelp(protocol string) string {
	keys := []string{"/ filter", "tab view"}
	for _, view := range traceGroupViews(protocol) {
		for key, keyView := range traceGroupKeyViews {
			if view != "" && keyView == view {
				keys = append(keys, key+" "+view)
			}
		}
	}
	keys = append(keys, "g scroll", "↑↓ select", "enter detail", "i split", "f follow")
	switch protocol {
	case "http":
		keys = append(keys, "v payload", "m secrets")
	case "socket":
		keys = append(keys, "v payload")
	}
	return strings.Join(append(keys, "l live", "esc clear", "q quit", "ctrl-c stop"), "  ")
}

func (model *traceScreenModel) requestStop() {
	if model.stopping {
		return
	}
	model.stopping = true
	if model.stop != nil {
		model.stop()
	}
}

func (model traceScreenModel) View() tea.View {
	if model.detail != nil {
		view := liveFrame(strings.Join(traceScreenDetailView(model), "\n")+"\n", model.height)
		view.AltScreen = true
		return view
	}
	header := traceScreenHeader(model)
	rows := traceScreenRows(model)
	content := strings.Join(append(header, rows...), "\n") + "\n"
	view := liveFrame(content, model.height)
	view.AltScreen = true
	return view
}

func traceScreenHeader(model traceScreenModel) []string {
	status := "live"
	if model.selected >= 0 && model.groupBy == "" {
		status = fmt.Sprintf("paused, %d newer", model.first+len(model.events)-1-model.selected)
	}
	if model.secrets && model.protocol == "http" {
		status += "  ·  secrets shown"
	}
	if model.stopping {
		status = "stopping"
	}
	filter := "all"
	if model.filter != "" {
		filter = model.filter
	}
	line := fmt.Sprintf("edc trace %s", traceLabel(model.protocol, model.side))
	if model.container != "" {
		line += " in container " + model.container
	}
	var report traceGroupReport
	if model.groupBy != "" {
		report = model.groupReport()
		traffic := fmt.Sprintf("  ·  %.1f bps", report.BitsPerSecond)
		if report.hideTraffic() {
			traffic = ""
		}
		line += fmt.Sprintf(" grouped by %s  ·  %s  ·  events %d  ·  %s %d  ·  event/s %.1f%s  ·  filter %s", model.groupBy, status, report.Events, model.groupBy, len(report.Groups), report.Rate, traffic, filter)
	} else {
		if model.protocol == "mysql" {
			line += fmt.Sprintf("  ·  %s  ·  raw events %d  ·  shown pairs %d  ·  slow %s  ·  filter %s", status, model.received, model.shownMySQLPairs(), model.slow, filter)
		} else {
			line += fmt.Sprintf("  ·  %s  ·  events %d  ·  filter %s", status, model.received, filter)
		}
	}
	help := traceScreenHelp(model.protocol)
	if model.filtering {
		help = model.input.View() + "  enter apply  esc cancel"
	}
	color := os.Getenv("NO_COLOR") == ""
	timeColumn := traceScrollTimeColumn(model.width)
	destinationWidth, _ := traceScrollColumns(model.width - timeColumn)
	columns := liveCell("PROCESS", traceScrollProcessWidth) + " " + liveCell("DESTINATION", destinationWidth) + " " + liveCell("EVENT", traceScrollEventWidth) + " SOURCE"
	if timeColumn > 0 {
		columns = liveCell("TIME", traceScrollTimeWidth) + " " + columns
	}
	if model.groupBy != "" {
		layout := traceScreenGroupLayout(model, report)
		names := []any{"EVT", "E/s"}
		if !traceProtocols[model.protocol].hideTraffic {
			names = append(names, "TX", "RX", "TOT", "B/s", "Mbps")
		}
		for _, column := range traceProtocols[model.protocol].groupColumns {
			names = append(names, column.screenTitle)
		}
		columns = liveCell(traceGroupLabel(model.groupBy), layout.labelWidth) + fmt.Sprintf(traceGroupColumns(model.protocol, layout.byteWidth), append(names, "LAST")...)
	}
	return []string{liveSelected(traceFit(line, model.width), color), liveMuted(traceFit(help, model.width), color), traceFit(columns, model.width)}
}

func (model traceScreenModel) shownMySQLPairs() int {
	shown := 0
	for _, row := range model.displayRows() {
		if row.response >= model.first && row.response < model.first+len(model.events) && model.displayRowMatches(row) {
			shown++
		}
	}
	return shown
}

// traceScreenRows는 머리글 아래의 줄이다. 화면 나누기가 켜져 있으면 목록 아래에 미리 보기 창을 붙인다.
func traceScreenRows(model traceScreenModel) []string {
	if list, preview, ok := traceSplitHeights(model.height); model.split && model.groupBy == "" && ok {
		return append(traceScreenListRows(model, list), traceScreenPreviewRows(model, preview)...)
	}
	return traceScreenListRows(model, max(0, model.height-3))
}

// listRows는 목록에 쓰는 줄 수다. 한 쪽씩 넘길 때 쓴다.
func (model traceScreenModel) listRows() int {
	if list, _, ok := traceSplitHeights(model.height); model.split && model.groupBy == "" && ok {
		return list
	}
	return max(0, model.height-3)
}

func traceScreenListRows(model traceScreenModel, available int) []string {
	rows := make([]string, 0, available)
	if model.groupBy != "" {
		report := model.groupReport()
		layout := traceScreenGroupLayout(model, report)
		for _, group := range traceScreenVisibleGroups(model, report) {
			rows = append(rows, formatTraceGroupScreenRow(model.protocol, model.groupBy, group, model.width, layout))
		}
		return traceScreenPadRows(rows, available)
	}
	// 화면에 보이는 줄만 뒤에서부터 서식화한다. 보관한 event 전부(최대 10,000건)를 서식화하면 한 번 그리는 데
	// 300ms가 넘게 걸려, 화면이 event를 따라가지 못하고 키 입력도 늦어진다.
	rowsForDisplay := model.displayRows()
	eventLines := func(rowIndex int) []string {
		row := rowsForDisplay[rowIndex]
		event, destination, name, ok := model.displayRowLabels(row)
		if !ok {
			return nil
		}
		line := formatTraceScreenEventLabels(event, destination, name, model.width)
		if row.primary == model.selected || row.response == model.selected {
			line = traceSelectedEvent(line, model.width, os.Getenv("NO_COLOR") == "")
		} else if model.width >= 72 {
			line = traceEventStyle(line, traceProtocol(event), name)
		}
		lines := []string{line}
		if model.payloadLines && event.Payload != "" {
			if !model.secrets && model.protocol == "http" {
				event.Payload = string(traceMaskHTTPHeaders([]byte(event.Payload)))
			}
			lines = append(lines, formatTraceScreenPayload(event, model.width))
		}
		return lines
	}
	// 고른 event가 있으면 그 event를 맨 아래에 두고 그보다 오래된 event를 위에 채운다.
	start := len(rowsForDisplay) - 1
	if model.selected >= 0 {
		if selected := model.displayRowForSelected(rowsForDisplay); selected >= 0 {
			start = selected
		}
	}
	for index := start; index >= 0; index-- {
		if !model.displayRowMatches(rowsForDisplay[index]) {
			continue
		}
		lines := eventLines(index)
		// 넘친 채로 두면 traceScreenPadRows가 위를 잘라 event 행 없이 payload 줄만 남으므로, 두 줄이 다 들어가지 않으면 멈춘다.
		if len(rows)+len(lines) > available {
			break
		}
		for line := len(lines) - 1; line >= 0; line-- {
			rows = append(rows, lines[line])
		}
	}
	slices.Reverse(rows)
	// 오래된 event가 모자라 화면이 남으면 고른 event 아래에 더 새 event를 채운다. 멈춘 동안 event가 10,000개 넘게 쌓이면
	// 고른 event가 가장 오래된 event로 밀려서, 전에는 목록이 한 줄만 보였다.
	for index := start + 1; model.selected >= 0 && index < len(rowsForDisplay); index++ {
		if !model.displayRowMatches(rowsForDisplay[index]) {
			continue
		}
		lines := eventLines(index)
		if len(rows)+len(lines) > available {
			break
		}
		rows = append(rows, lines...)
	}
	return traceScreenPadRows(rows, available)
}

func traceSelectedEvent(line string, width int, color bool) string {
	if !color {
		return line
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#f8fafc")).Bold(true).Render(line)
}

// traceScreenGroupOrder는 화면에 다 들어가지 않을 때 잘릴 group을 정한다. 이름순으로 자르면 같은
// group이 session 내내 보이지 않으므로, traffic이 많은 group을 위에 둔다.
func traceScreenGroupOrder(groups []traceGroupSummary) []traceGroupSummary {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].TotalBytes != groups[j].TotalBytes {
			return groups[i].TotalBytes > groups[j].TotalBytes
		}
		return groups[i].Events > groups[j].Events
	})
	return groups
}

func traceScreenPadRows(rows []string, available int) []string {
	available = max(0, available)
	if len(rows) > available {
		rows = rows[len(rows)-available:]
	}
	for len(rows) < available {
		rows = append(rows, "")
	}
	return rows
}

func (model traceScreenModel) groupReport() traceGroupReport {
	filtered := make([]captureEvent, 0, len(model.events))
	for _, event := range model.events {
		if traceEventMatchesText(event, model.filter) {
			filtered = append(filtered, event)
		}
	}
	return summarizeTraceGroups(model.protocol, model.groupBy, filtered, captureSummary{}, model.windowDuration(time.Now()), model.process, model.destination)
}

// windowDuration은 model.events가 덮는 시간이다. 창이 잘린 뒤에도 session 시작부터 재면 event 수는
// 고정인데 시간만 늘어서 rate가 0으로 수렴한다.
func (model traceScreenModel) windowDuration(now time.Time) time.Duration {
	end := now
	if limit := model.started.Add(model.duration); model.duration > 0 && limit.Before(end) {
		end = limit
	}
	start := model.started
	if model.truncated && len(model.arrivals) > 0 {
		start = model.arrivals[0]
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func traceScreenVisibleGroups(model traceScreenModel, report traceGroupReport) []traceGroupSummary {
	groups := traceScreenGroupOrder(report.Groups)
	return groups[:min(len(groups), max(0, model.height-3))]
}

const (
	traceGroupMinLabelWidth = 14
	traceGroupLastWidth     = len("tcp_receive_reset")
	// traceBytes는 9999.9GiB까지 9칸 안에 쓰므로 넓은 화면에서 byte 열이 밀리지 않는다.
	traceGroupWideByteWidth = 9
	// 좁은 화면은 예전 6칸을 지켜야 TCP의 CON RET RST가 80칸 안에 남는다. 대신 traceCompactBytes로 짧게 쓴다.
	traceGroupNarrowByteWidth = 6
)

type traceGroupLayout struct {
	labelWidth int
	byteWidth  int
	bytes      func(uint64) string
}

// traceGroupColumns는 헤더와 행이 같은 폭을 쓰도록 label 뒤 열 형식을 만든다.
func traceGroupColumns(protocol string, byteWidth int) string {
	columns := " %5v %6v"
	if !traceProtocols[protocol].hideTraffic {
		columns += strings.Repeat(fmt.Sprintf(" %%%dv", byteWidth), 4) + " %5v"
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		columns += fmt.Sprintf(" %%%dv", column.width)
	}
	return columns + " %v"
}

// traceGroupColumnsWidth는 label 뒤 열의 폭이다. LAST 앞의 공백까지 센다.
func traceGroupColumnsWidth(protocol string, byteWidth int) int {
	width := 6 + 7 + 1
	if !traceProtocols[protocol].hideTraffic {
		width += 4*(byteWidth+1) + 6
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		width += column.width + 1
	}
	return width
}

// traceScreenGroupLayout은 넓은 terminal에서 byte 열과 group 값을 자르지 않도록 열을 늘린다.
// 좁은 terminal에서는 예전 폭을 유지한다.
func traceScreenGroupLayout(model traceScreenModel, report traceGroupReport) traceGroupLayout {
	layout := traceGroupLayout{byteWidth: traceGroupWideByteWidth, bytes: traceBytes}
	if model.width < traceGroupMinLabelWidth+traceGroupColumnsWidth(model.protocol, traceGroupWideByteWidth)+traceGroupLastWidth {
		layout = traceGroupLayout{byteWidth: traceGroupNarrowByteWidth, bytes: traceCompactBytes}
	}
	longest := 0
	for _, group := range traceScreenVisibleGroups(model, report) {
		longest = max(longest, liveWidth(traceGroupDisplayValue(model.groupBy, group)))
	}
	layout.labelWidth = max(traceGroupMinLabelWidth, min(longest, model.width-traceGroupColumnsWidth(model.protocol, layout.byteWidth)-traceGroupLastWidth))
	return layout
}

// traceCompactBytes는 byte 값을 4칸 안에 쓴다. 단위는 traceBytes와 같은 1024 배수다.
func traceCompactBytes(bytes uint64) string {
	if bytes < 1000 {
		return fmt.Sprintf("%dB", bytes)
	}
	value := float64(bytes)
	for _, unit := range []string{"K", "M", "G", "T"} {
		value /= 1024
		if value < 9.95 {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
		if value < 999.5 {
			return fmt.Sprintf("%.0f%s", value, unit)
		}
	}
	return fmt.Sprintf("%.0fP", value/1024)
}

// traceScreenEventRate는 E/s 값을 6칸 안에 쓴다. 값이 크면 소수 자리를 버린다.
func traceScreenEventRate(rate float64) string {
	switch {
	case rate < 9999.95:
		return fmt.Sprintf("%.1f", rate)
	case rate < 999999.5:
		return fmt.Sprintf("%.0f", rate)
	}
	return fmt.Sprintf("%.0fk", rate/1000)
}

// traceScreenMegabits는 Mbps 값을 5칸 안에 쓴다. 1 Gbps를 넘는 traffic에서도 뒤 열이 밀리지 않게 소수 자리를 줄인다.
func traceScreenMegabits(megabits float64) string {
	switch {
	case megabits < 99.995:
		return fmt.Sprintf("%.2f", megabits)
	case megabits < 999.95:
		return fmt.Sprintf("%.1f", megabits)
	case megabits < 99999.5:
		return fmt.Sprintf("%.0f", megabits)
	}
	return fmt.Sprintf("%.0fG", megabits/1000)
}

const traceServerSuffix = " (server)"

// traceGroupFitLabel은 첫 열을 자를 때 서버 표시를 남긴다. 표시가 잘리면 같은 주소의 client 행과 구분되지
// 않는다. 주소는 서비스를 가리키는 port 쪽을 남긴다. 앞쪽을 남기면 127.0.0.1:19999와 127.0.0.53:53이
// 똑같이 127.0…으로 보인다.
func traceGroupFitLabel(groupBy string, group traceGroupSummary, width int) string {
	value := traceGroupDisplayValue(groupBy, group)
	if !group.Server || liveWidth(value) <= width || width <= liveWidth(traceServerSuffix)+1 {
		return traceFit(value, width)
	}
	return traceKeepTail(group.Group, width-liveWidth(traceServerSuffix)) + traceServerSuffix
}

func traceKeepTail(value string, width int) string {
	if liveWidth(value) <= width {
		return value
	}
	tail := value
	if index := strings.LastIndex(value, ":"); index >= 0 && liveWidth(value[index:])+1 <= width {
		tail = value[index:]
	}
	runes := []rune(tail)
	for len(runes) > 0 && liveWidth(string(runes))+1 > width {
		runes = runes[1:]
	}
	return "…" + string(runes)
}

func formatTraceGroupScreenRow(protocol, groupBy string, group traceGroupSummary, width int, layout traceGroupLayout) string {
	// liveCell은 폭보다 긴 값을 여러 줄로 감싸므로 먼저 자른다.
	value := liveCell(traceGroupFitLabel(groupBy, group, layout.labelWidth), layout.labelWidth)
	values := []any{group.Events, traceScreenEventRate(group.Rate)}
	if !traceProtocols[protocol].hideTraffic {
		values = append(values, layout.bytes(group.TXBytes), layout.bytes(group.RXBytes), layout.bytes(group.TotalBytes), layout.bytes(uint64(group.BytesPerSecond)), traceScreenMegabits(group.MegabitsPerSecond))
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		values = append(values, column.value(group))
	}
	line := value + fmt.Sprintf(traceGroupColumns(protocol, layout.byteWidth), append(values, group.LastEvent)...)
	return traceFit(traceGroupColorLine(line, protocol, group), width)
}

func traceGroupColorLine(line, protocol string, group traceGroupSummary) string {
	if os.Getenv("NO_COLOR") != "" {
		return line
	}
	color := lipgloss.Color(traceProtocols[protocol].screenColor)
	if (group.Resets != nil && *group.Resets > 0) || (group.Neighbor != nil && group.Neighbor.MACChanges > 0) || (group.HTTP != nil && group.HTTP.ServerErrors > 0) {
		color = lipgloss.Color("#fb7185")
	} else if (group.Retransmissions != nil && *group.Retransmissions > 0) || (group.DNS != nil && group.DNS.Errors > 0) || (group.Neighbor != nil && group.Neighbor.Failures > 0) || (group.HTTP != nil && group.HTTP.ClientErrors > 0) {
		color = lipgloss.Color("#fbbf24")
	}
	return lipgloss.NewStyle().Foreground(color).Render(line)
}

func traceEventMatchesText(event captureEvent, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	// 화면의 목적지 칸과 event 칸에 보이는 값도 찾는다. HTTP의 method와 path, DNS의 질의 종류는 이 칸에만 보인다.
	destination, name := traceScrollLabels(event)
	text := strings.ToLower(strings.Join([]string{event.Process, event.Target, event.Source, event.Destination, event.Event, destination, name}, " "))
	return strings.Contains(text, filter)
}

const (
	traceScrollProcessWidth = 16
	traceScrollEventWidth   = 20
	// traceScrollSourceWidth는 목적지 칸을 넓힐 때 source 칸에 남기는 폭이다. 100.83.200.248:52406 같은 값이 들어간다.
	traceScrollSourceWidth = 22
	// TIME 칸은 15:04:05.000을 쓴다. 좁은 화면에서 목적지 칸을 더 줄이지 않도록 traceScrollTimeMinWidth 이상일 때만 둔다.
	traceScrollTimeWidth    = 12
	traceScrollTimeMinWidth = 120
)

// traceScrollTimeColumn은 TIME 칸과 뒤의 빈칸을 더한 폭이다. 좁은 화면에서는 0이다.
func traceScrollTimeColumn(width int) int {
	if width < traceScrollTimeMinWidth {
		return 0
	}
	return traceScrollTimeWidth + 1
}

// traceEventClock은 event의 시각을 이 host의 시간대로 쓴다. 시각이 없는 event는 -다.
func traceEventClock(event captureEvent) string {
	if event.TimestampNS == 0 {
		return "-"
	}
	return time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000")
}

// traceScrollColumns는 스크롤 화면의 목적지와 source 칸 폭이다. 목적지에는 도메인이 붙어 길어지므로 넓은 화면에서
// 목적지 칸을 늘린다. 좁은 화면에서는 예전처럼 32칸을 지킨다.
func traceScrollColumns(width int) (int, int) {
	fixed := traceScrollProcessWidth + traceScrollEventWidth + 3
	destination := min(60, max(32, width-fixed-traceScrollSourceWidth))
	return destination, max(8, width-fixed-destination)
}

func formatTraceScreenEvent(event captureEvent, width int) string {
	line := formatTraceScreenEventLine(event, width)
	if width < 72 {
		return line
	}
	return traceEventStyle(line, traceProtocol(event), event.Event)
}

func formatTraceScreenEventLabels(event captureEvent, destination, name string, width int) string {
	process, source := event.Process, event.Source
	if process == "" {
		process = "-"
	}
	if source == "" {
		source = "-"
	}
	if width < 72 {
		return traceFit(strings.Join([]string{process, destination, name, source}, "  "), width)
	}
	timeColumn := traceScrollTimeColumn(width)
	destinationWidth, sourceWidth := traceScrollColumns(width - timeColumn)
	cell := func(value string, cellWidth int) string { return liveCell(traceFit(value, cellWidth), cellWidth) }
	line := cell(process, traceScrollProcessWidth) + " " + cell(destination, destinationWidth) + " " + cell(name, traceScrollEventWidth) + " " + cell(source, sourceWidth)
	if timeColumn > 0 {
		line = cell(traceEventClock(event), traceScrollTimeWidth) + " " + line
	}
	return line
}

// formatTraceScreenEventLine은 색을 칠하기 전의 행이다. 고른 행은 색 대신 글자와 배경을 뒤집어 보인다.
func formatTraceScreenEventLine(event captureEvent, width int) string {
	destination, name := traceScrollLabels(event)
	process, source := event.Process, event.Source
	if process == "" {
		process = "-"
	}
	if source == "" {
		source = "-"
	}
	if width < 72 {
		return traceFit(strings.Join([]string{process, destination, name, source}, "  "), width)
	}
	timeColumn := traceScrollTimeColumn(width)
	destinationWidth, sourceWidth := traceScrollColumns(width - timeColumn)
	// liveCell은 칸보다 긴 값을 여러 줄로 감싸므로, 한 행을 지키려고 먼저 자른다.
	cell := func(value string, width int) string { return liveCell(traceFit(value, width), width) }
	line := cell(process, traceScrollProcessWidth) + " " + cell(destination, destinationWidth) + " " + cell(name, traceScrollEventWidth) + " " + cell(source, sourceWidth)
	if timeColumn > 0 {
		line = cell(traceEventClock(event), traceScrollTimeWidth) + " " + line
	}
	return line
}

// formatTraceScreenPayload는 event 행 아래에 목적지 칸부터 payload를 한 줄로 쓴다.
func formatTraceScreenPayload(event captureEvent, width int) string {
	line := "↳ " + tracePayloadSummary(event)
	if width >= 72 {
		line = strings.Repeat(" ", traceScrollTimeColumn(width)+traceScrollProcessWidth+1) + line
	}
	return liveMuted(traceFit(line, width), os.Getenv("NO_COLOR") == "")
}

func traceEventStyle(line, protocol, event string) string {
	if os.Getenv("NO_COLOR") != "" {
		return line
	}
	color := lipgloss.Color(traceProtocols[protocol].screenColor)
	if strings.Contains(event, "reset") || strings.HasSuffix(event, "_"+traceNeighborMACChange) || event == "http_5xx" {
		color = lipgloss.Color("#fb7185")
	} else if strings.Contains(event, "retransmit") || strings.Contains(event, "fail") {
		color = lipgloss.Color("#fbbf24")
	}
	return lipgloss.NewStyle().Foreground(color).Render(line)
}

func traceFit(value string, width int) string {
	if width <= 0 {
		return value
	}
	// truncateLine은 한 글자씩 줄이며 폭을 다시 재서, 4KiB payload 한 줄에 200ms가 넘게 걸렸다. 화면을 그릴 때마다 이만큼
	// 걸려 키 입력이 밀렸다. 글자 하나는 적어도 한 칸이므로 width+1자에서 먼저 자른다. 색 escape가 있으면 글자 수와 칸 수가
	// 달라서 그대로 둔다.
	cut := false
	if !strings.ContainsRune(value, '\x1b') {
		if index := traceRuneIndex(value, width+1); index < len(value) {
			value, cut = value[:index], true
		}
	}
	if !cut && liveWidth(value) <= width {
		return value
	}
	// truncateLine은 자른 값 끝에 줄바꿈을 붙인다. View가 행을 줄바꿈으로 이으므로 그대로 두면 잘린 행마다 빈 줄이 생긴다.
	return strings.TrimRight(truncateLine(value, width), "\n")
}

// traceRuneIndex는 value에서 count번째 글자가 시작하는 byte 위치다. 글자가 모자라면 len(value)다.
func traceRuneIndex(value string, count int) int {
	for index := range value {
		if count == 0 {
			return index
		}
		count--
	}
	return len(value)
}

// traceScreenScope는 전체 화면이 모을 범위다. trace http와 trace socket은 --payload가 없어도 message나 호출의
// 앞부분을 모은다. 옵션을 늘리지 않고 Enter와 v로 바로 보려는 것이다. 인증 header는 원문으로 두고 그릴 때 가려서
// m으로 풀 수 있게 한다. 파이프와 --raw 출력은 이 범위를 쓰지 않는다.
func traceScreenScope(protocol string, options tcpTraceOptions) traceScope {
	scope := options.scope(protocol)
	switch protocol {
	case "http":
		scope.payload, scope.showSecrets, scope.keepGzip = true, true, true
	case "socket":
		// 6.4 전 kernel은 사용자 버퍼를 읽을 field가 없다. 사용자가 고르지 않은 payload 때문에 화면이 멈추지 않게 byte 수만 모은다.
		scope.payload = scope.payload || socketPayloadSupported()
	}
	return scope
}

func runTraceScreen(protocol string, options tcpTraceOptions) int {
	if err := traceProtocolPrerequisites(protocol); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(traceScreenGCPercent)
	}
	eventCh := make(chan captureEvent, 256)
	resultCh := make(chan traceFinishedMsg, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCapture := func() { stopOnce.Do(func() { close(stop) }) }
	started := time.Now()
	// 끝날 때 어느 보기일지 모르므로 모든 보기의 요약을 쌓는다. 수집 goroutine만 쓰고, resultCh를 받은 뒤에 읽는다.
	aggregate := newTraceAggregate(protocol, traceGroupViews(protocol)...)
	go func() {
		summary, err := collectTraceEventsLive(traceScreenScope(protocol, options), options.duration, func(event captureEvent) error {
			if traceProtocol(event) != protocol || !options.matches(event) {
				return nil
			}
			aggregate.observe(event)
			select {
			case eventCh <- event:
				return nil
			case <-stop:
				return nil
			}
		}, stop)
		resultCh <- traceFinishedMsg{summary: summary, err: err}
	}()
	model := newTraceScreenModel(protocol, options, eventCh, resultCh, stopCapture)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// bubbletea의 signal 처리기는 SIGINT를 중단 오류로, SIGTERM을 즉시 종료로 끝내서 수집 결과를 받기 전에
	// 화면이 닫혔다. 위의 NotifyContext가 대신 수집을 멈추고, 화면은 결과를 받은 뒤 요약과 함께 끝난다.
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	go func() {
		<-ctx.Done()
		stopCapture()
		program.Send(traceStopMsg{})
	}()
	finalModel, err := program.Run()
	if err != nil {
		stopCapture()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	screen, ok := finalModel.(traceScreenModel)
	if !ok || screen.finished == nil {
		stopCapture()
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", "trace screen ended without a result"))
		return 1
	}
	result := *screen.finished
	if result.err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", result.err))
		return 1
	}
	printTraceTLSExecProblem(result.summary)
	duration := options.duration
	if duration == 0 || screen.stopping || ctx.Err() != nil {
		duration = time.Since(started)
	}
	if screen.groupBy != "" {
		printTraceGroupReport(aggregate.groups[screen.groupBy].report(result.summary, duration))
		return 0
	}
	aggregate.summary.summarize(result.summary, duration).print(options.detail)
	return 0
}
