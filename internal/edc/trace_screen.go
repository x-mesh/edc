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
}

func newTraceScreenModel(protocol string, options tcpTraceOptions, eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg, stop func()) traceScreenModel {
	input := textinput.New()
	input.Prompt = "filter / "
	input.Placeholder = "process, destination, event"
	input.CharLimit = 256
	input.SetWidth(48)
	return traceScreenModel{
		protocol: protocol, side: options.side, groupBy: options.groupBy, process: options.process, destination: options.destination,
		duration: options.duration, started: time.Now(), eventCh: eventCh, resultCh: resultCh, stop: stop, input: input,
		width: 80, height: 24,
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

func (model traceScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.input.SetWidth(max(20, min(60, value.Width-12)))
		return model, nil
	case traceEventMsg:
		now := time.Now()
		for _, event := range value.events {
			model.received++
			if traceProtocol(event) == model.protocol {
				model.events = append(model.events, event)
				model.arrivals = append(model.arrivals, now)
			}
		}
		if len(model.events) > traceScreenEventLimit {
			model.events = model.events[len(model.events)-traceScreenEventLimit:]
			model.arrivals = model.arrivals[len(model.arrivals)-traceScreenEventLimit:]
			model.truncated = true
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

func (model traceScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		model.requestStop()
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	}
	if model.filtering {
		switch key.String() {
		case "enter":
			model.filter = strings.TrimSpace(model.input.Value())
			model.input.Blur()
			model.filtering = false
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
	case "s", "t", "p", "c", "e", "g":
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
		model.filter = ""
		return model, nil
	case "q":
		model.requestStop()
		return model, waitTraceMessage(model.eventCh, model.resultCh)
	}
	return model, nil
}

// traceGroupCycle은 Tab이 넘겨 가는 순서다. 기본 화면인 event 스크롤에서 시작해 s, t, p, c, e 키와 같은 순서로 간다.
var traceGroupCycle = []string{"", traceGroupBySource, traceGroupByTarget, traceGroupByPort, traceGroupByProcess, traceGroupByEvent}

var traceGroupKeyViews = map[string]string{"g": "", "s": traceGroupBySource, "t": traceGroupByTarget, "p": traceGroupByPort, "c": traceGroupByProcess, "e": traceGroupByEvent}

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
	return strings.Join(append(keys, "g scroll", "enter apply", "esc clear", "q quit", "ctrl-c stop"), "  ")
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
	header := traceScreenHeader(model)
	rows := traceScreenRows(model)
	content := strings.Join(append(header, rows...), "\n") + "\n"
	view := liveFrame(content, model.height)
	view.AltScreen = true
	return view
}

func traceScreenHeader(model traceScreenModel) []string {
	status := "live"
	if model.stopping {
		status = "stopping"
	}
	filter := "all"
	if model.filter != "" {
		filter = model.filter
	}
	line := fmt.Sprintf("edc trace %s", traceLabel(model.protocol, model.side))
	var report traceGroupReport
	if model.groupBy != "" {
		report = model.groupReport()
		traffic := fmt.Sprintf("  ·  %.1f bps", report.BitsPerSecond)
		if report.hideTraffic() {
			traffic = ""
		}
		line += fmt.Sprintf(" grouped by %s  ·  %s  ·  events %d  ·  %s %d  ·  event/s %.1f%s  ·  filter %s", model.groupBy, status, report.Events, model.groupBy, len(report.Groups), report.Rate, traffic, filter)
	} else {
		line += fmt.Sprintf("  ·  %s  ·  events %d  ·  filter %s", status, model.received, filter)
	}
	help := traceScreenHelp(model.protocol)
	if model.filtering {
		help = model.input.View() + "  enter apply  esc cancel"
	}
	color := os.Getenv("NO_COLOR") == ""
	destinationWidth, _ := traceScrollColumns(model.width)
	columns := liveCell("PROCESS", traceScrollProcessWidth) + " " + liveCell("DESTINATION", destinationWidth) + " " + liveCell("EVENT", traceScrollEventWidth) + " SOURCE"
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

func traceScreenRows(model traceScreenModel) []string {
	rows := make([]string, 0, model.height)
	if model.groupBy != "" {
		report := model.groupReport()
		layout := traceScreenGroupLayout(model, report)
		for _, group := range traceScreenVisibleGroups(model, report) {
			rows = append(rows, formatTraceGroupScreenRow(model.protocol, model.groupBy, group, model.width, layout))
		}
		return traceScreenPadRows(rows, model.height-3)
	}
	// 화면에 보이는 줄만 뒤에서부터 서식화한다. 보관한 event 전부(최대 10,000건)를 서식화하면 한 번 그리는 데
	// 300ms가 넘게 걸려, 화면이 event를 따라가지 못하고 키 입력도 늦어진다.
	available := max(0, model.height-3)
	for index := len(model.events) - 1; index >= 0 && len(rows) < available; index-- {
		event := model.events[index]
		if !traceEventMatchesText(event, model.filter) {
			continue
		}
		rows = append(rows, formatTraceScreenEvent(event, model.width))
	}
	slices.Reverse(rows)
	return traceScreenPadRows(rows, available)
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
	if (group.Resets != nil && *group.Resets > 0) || (group.ARP != nil && group.ARP.MACChanges > 0) {
		color = lipgloss.Color("#fb7185")
	} else if (group.Retransmissions != nil && *group.Retransmissions > 0) || (group.DNS != nil && group.DNS.Errors > 0) || (group.ARP != nil && group.ARP.Failures > 0) {
		color = lipgloss.Color("#fbbf24")
	}
	return lipgloss.NewStyle().Foreground(color).Render(line)
}

func traceEventMatchesText(event captureEvent, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	text := strings.ToLower(strings.Join([]string{event.Process, event.Target, event.Source, event.Destination, event.Event}, " "))
	return strings.Contains(text, filter)
}

const (
	traceScrollProcessWidth = 16
	traceScrollEventWidth   = 20
	// traceScrollSourceWidth는 목적지 칸을 넓힐 때 source 칸에 남기는 폭이다. 100.83.200.248:52406 같은 값이 들어간다.
	traceScrollSourceWidth = 22
)

// traceScrollColumns는 스크롤 화면의 목적지와 source 칸 폭이다. 목적지에는 도메인이 붙어 길어지므로 넓은 화면에서
// 목적지 칸을 늘린다. 좁은 화면에서는 예전처럼 32칸을 지킨다.
func traceScrollColumns(width int) (int, int) {
	fixed := traceScrollProcessWidth + traceScrollEventWidth + 3
	destination := min(60, max(32, width-fixed-traceScrollSourceWidth))
	return destination, max(8, width-fixed-destination)
}

func formatTraceScreenEvent(event captureEvent, width int) string {
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
	destinationWidth, sourceWidth := traceScrollColumns(width)
	// liveCell은 칸보다 긴 값을 여러 줄로 감싸므로, 한 행을 지키려고 먼저 자른다.
	cell := func(value string, width int) string { return liveCell(traceFit(value, width), width) }
	line := cell(process, traceScrollProcessWidth) + " " + cell(destination, destinationWidth) + " " + cell(name, traceScrollEventWidth) + " " + cell(source, sourceWidth)
	return traceEventStyle(line, traceProtocol(event), event.Event)
}

func traceEventStyle(line, protocol, event string) string {
	if os.Getenv("NO_COLOR") != "" {
		return line
	}
	color := lipgloss.Color(traceProtocols[protocol].screenColor)
	if strings.Contains(event, "reset") || event == traceARPMACChangeEvent {
		color = lipgloss.Color("#fb7185")
	} else if strings.Contains(event, "retransmit") || strings.Contains(event, "fail") {
		color = lipgloss.Color("#fbbf24")
	}
	return lipgloss.NewStyle().Foreground(color).Render(line)
}

func traceFit(value string, width int) string {
	if width <= 0 || liveWidth(value) <= width {
		return value
	}
	// truncateLine은 자른 값 끝에 줄바꿈을 붙인다. View가 행을 줄바꿈으로 이으므로 그대로 두면 잘린 행마다 빈 줄이 생긴다.
	return strings.TrimRight(truncateLine(value, width), "\n")
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
		summary, err := collectTraceEventsLive(options.scope(protocol), options.duration, func(event captureEvent) error {
			if traceProtocol(event) != protocol || !traceEventMatches(event, options.process, options.destination) {
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
