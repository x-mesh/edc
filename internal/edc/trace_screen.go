package edc

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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

type traceEventMsg struct{ event captureEvent }
type traceFinishedMsg struct {
	events  []captureEvent
	summary captureSummary
	err     error
}
type traceStopMsg struct{}

type traceScreenModel struct {
	protocol    string
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
		protocol: protocol, groupBy: options.groupBy, process: options.process, destination: options.destination,
		duration: options.duration, started: time.Now(), eventCh: eventCh, resultCh: resultCh, stop: stop, input: input,
		width: 80, height: 24,
	}
}

func (model traceScreenModel) Init() tea.Cmd {
	return waitTraceMessage(model.eventCh, model.resultCh)
}

func waitTraceMessage(eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-eventCh:
			return traceEventMsg{event: event}
		case result := <-resultCh:
			return result
		}
	}
}

func (model traceScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.input.SetWidth(max(20, min(60, value.Width-12)))
		return model, nil
	case traceEventMsg:
		model.received++
		if traceProtocol(value.event) == model.protocol {
			model.events = append(model.events, value.event)
			model.arrivals = append(model.arrivals, time.Now())
			if len(model.events) > traceScreenEventLimit {
				model.events = model.events[len(model.events)-traceScreenEventLimit:]
				model.arrivals = model.arrivals[len(model.arrivals)-traceScreenEventLimit:]
				model.truncated = true
			}
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
	case "s":
		model.groupBy = traceGroupBySource
		return model, nil
	case "t":
		model.groupBy = traceGroupByTarget
		return model, nil
	case "e":
		model.groupBy = traceGroupByEvent
		return model, nil
	case "g":
		model.groupBy = ""
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
	line := fmt.Sprintf("edc trace %s", model.protocol)
	var report traceGroupReport
	if model.groupBy != "" {
		report = model.groupReport()
		line += fmt.Sprintf(" grouped by %s  ·  %s  ·  events %d  ·  %s %d  ·  event/s %.1f  ·  %.1f bps  ·  filter %s", model.groupBy, status, report.Events, model.groupBy, len(report.Groups), report.Rate, report.BitsPerSecond, filter)
	} else {
		line += fmt.Sprintf("  ·  %s  ·  events %d  ·  filter %s", status, model.received, filter)
	}
	help := "/ filter  s source  t target  e event  g scroll  enter apply  esc clear  q quit  ctrl-c stop"
	if model.filtering {
		help = model.input.View() + "  enter apply  esc cancel"
	}
	color := os.Getenv("NO_COLOR") == ""
	columns := "PROCESS          DESTINATION                       EVENT                SOURCE"
	if model.groupBy != "" {
		layout := traceScreenGroupLayout(model, report)
		names := []any{"EVT", "E/s", "TX", "RX", "TOT", "B/s", "Mbps"}
		if model.protocol != "udp" {
			names = append(names, "CON", "RET", "RST")
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
	for _, event := range model.events {
		if !traceEventMatchesText(event, model.filter) {
			continue
		}
		rows = append(rows, formatTraceScreenEvent(event, model.width))
	}
	return traceScreenPadRows(rows, model.height-3)
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
	// traceBytes의 가장 긴 값은 1023.9KiB와 9999.9MiB라서 9칸이면 넓은 화면에서 열이 밀리지 않는다.
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
	byteColumn := fmt.Sprintf(" %%%dv", byteWidth)
	columns := " %5v %6v" + strings.Repeat(byteColumn, 4) + " %5v"
	if protocol != "udp" {
		columns += " %3v %3v %3v"
	}
	return columns + " %v"
}

// traceGroupColumnsWidth는 label 뒤 열의 폭이다. LAST 앞의 공백까지 센다.
func traceGroupColumnsWidth(protocol string, byteWidth int) int {
	width := 6 + 7 + 4*(byteWidth+1) + 6 + 1
	if protocol != "udp" {
		width += 12
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

func formatTraceGroupScreenRow(protocol, groupBy string, group traceGroupSummary, width int, layout traceGroupLayout) string {
	// liveCell은 폭보다 긴 값을 여러 줄로 감싸므로 먼저 자른다.
	value := liveCell(traceFit(traceGroupDisplayValue(groupBy, group), layout.labelWidth), layout.labelWidth)
	values := []any{group.Events, traceScreenEventRate(group.Rate), layout.bytes(group.TXBytes), layout.bytes(group.RXBytes), layout.bytes(group.TotalBytes), layout.bytes(uint64(group.BytesPerSecond)), traceScreenMegabits(group.MegabitsPerSecond)}
	if protocol != "udp" {
		values = append(values, group.Connect, group.Retransmissions, group.Resets)
	}
	line := value + fmt.Sprintf(traceGroupColumns(protocol, layout.byteWidth), append(values, group.LastEvent)...)
	return traceFit(traceGroupColorLine(line, protocol, group), width)
}

func traceGroupColorLine(line, protocol string, group traceGroupSummary) string {
	if os.Getenv("NO_COLOR") != "" {
		return line
	}
	color := lipgloss.Color("#22d3ee")
	if protocol == "udp" {
		color = lipgloss.Color("#c084fc")
	}
	if group.Resets > 0 {
		color = lipgloss.Color("#fb7185")
	} else if group.Retransmissions > 0 {
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

func formatTraceScreenEvent(event captureEvent, width int) string {
	process, destination, name, source := event.Process, event.Destination, event.Event, event.Source
	if process == "" {
		process = "-"
	}
	if destination == "" {
		destination = "-"
	}
	if source == "" {
		source = "-"
	}
	if width < 72 {
		return traceFit(strings.Join([]string{process, destination, name, source}, "  "), width)
	}
	sourceWidth := max(8, width-72)
	line := liveCell(process, 16) + " " + liveCell(destination, 32) + " " + liveCell(name, 20) + " " + liveCell(source, sourceWidth)
	return traceEventStyle(line, traceProtocol(event), name)
}

func traceEventStyle(line, protocol, event string) string {
	if os.Getenv("NO_COLOR") != "" {
		return line
	}
	color := lipgloss.Color("#22d3ee")
	if protocol == "udp" {
		color = lipgloss.Color("#c084fc")
	}
	if strings.Contains(event, "reset") {
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
	if err := captureEventsPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	eventCh := make(chan captureEvent, 256)
	resultCh := make(chan traceFinishedMsg, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCapture := func() { stopOnce.Do(func() { close(stop) }) }
	started := time.Now()
	go func() {
		events, summary, err := collectTraceEventsLive(options.duration, func(event captureEvent) error {
			if traceProtocol(event) != protocol || !traceEventMatches(event, options.process, options.destination) {
				return nil
			}
			select {
			case eventCh <- event:
				return nil
			case <-stop:
				return nil
			}
		}, stop)
		resultCh <- traceFinishedMsg{events: events, summary: summary, err: err}
	}()
	model := newTraceScreenModel(protocol, options, eventCh, resultCh, stopCapture)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
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
		printTraceGroupReport(summarizeTraceGroups(protocol, screen.groupBy, result.events, result.summary, duration, options.process, options.destination))
		return 0
	}
	if protocol == "udp" {
		printUDPTraceReport(summarizeUDPTrace(result.events, result.summary, duration, options.process, options.destination))
		return 0
	}
	printTCPTraceReport(summarizeTCPTrace(result.events, result.summary, duration, options.process, options.destination))
	return 0
}
