package edc

import (
	"context"
	"fmt"
	"os"
	"os/signal"
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
	if model.groupBy != "" {
		report := model.groupReport()
		line += fmt.Sprintf(" grouped by %s  ·  %s  ·  events %d  ·  %s %d  ·  event/s %.1f  ·  %.1f bps  ·  filter %s", model.groupBy, status, report.Events, model.groupBy, len(report.Groups), report.Rate, report.BitsPerSecond, filter)
	} else {
		line += fmt.Sprintf("  ·  %s  ·  events %d  ·  filter %s", status, model.received, filter)
	}
	help := "/ filter  s source  t target  g events  enter apply  esc clear  q quit  ctrl-c stop"
	if model.filtering {
		help = model.input.View() + "  enter apply  esc cancel"
	}
	color := os.Getenv("NO_COLOR") == ""
	columns := "PROCESS          DESTINATION                       EVENT                SOURCE"
	if model.groupBy != "" {
		label := traceGroupLabel(model.groupBy)
		if model.protocol == "udp" {
			columns = fmt.Sprintf("%-14s   EVT    E/s     TX     RX    TOT    B/s  Mbps LAST", label)
		} else {
			columns = fmt.Sprintf("%-14s   EVT    E/s     TX     RX    TOT    B/s  Mbps CON RET RST LAST", label)
		}
	}
	return []string{liveSelected(traceFit(line, model.width), color), liveMuted(traceFit(help, model.width), color), traceFit(columns, model.width)}
}

func traceScreenRows(model traceScreenModel) []string {
	rows := make([]string, 0, model.height)
	if model.groupBy != "" {
		report := model.groupReport()
		for _, group := range report.Groups {
			rows = append(rows, formatTraceGroupScreenRow(model.protocol, group, model.width))
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

func formatTraceGroupScreenRow(protocol string, group traceGroupSummary, width int) string {
	value := traceGroupDisplayValue(group)
	var line string
	if protocol == "udp" {
		line = liveCell(value, 14) + fmt.Sprintf(" %5d %6.1f %6s %6s %6s %6s %5.2f %s", group.Events, group.Rate, traceBytes(group.TXBytes), traceBytes(group.RXBytes), traceBytes(group.TotalBytes), traceBytes(uint64(group.BytesPerSecond)), group.MegabitsPerSecond, group.LastEvent)
	} else {
		line = liveCell(value, 14) + fmt.Sprintf(" %5d %6.1f %6s %6s %6s %6s %5.2f %3d %3d %3d %s", group.Events, group.Rate, traceBytes(group.TXBytes), traceBytes(group.RXBytes), traceBytes(group.TotalBytes), traceBytes(uint64(group.BytesPerSecond)), group.MegabitsPerSecond, group.Connect, group.Retransmissions, group.Resets, group.LastEvent)
	}
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
	return truncateLine(value, width)
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
