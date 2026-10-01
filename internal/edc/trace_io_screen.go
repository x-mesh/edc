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

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type ioTraceEventMsg struct{ events []ioEvent }
type ioTraceFinishedMsg struct {
	summary ioSummary
	err     error
}
type ioTraceScreenModel struct {
	options             ioTraceOptions
	events              []ioEvent
	summary             ioSummary
	groupBy             string
	filter              string
	input               textinput.Model
	filtering, stopping bool
	selected            int
	width, height       int
	stop                func()
	eventCh             <-chan ioEvent
	resultCh            <-chan ioTraceFinishedMsg
	finished            *ioTraceFinishedMsg
}

func newIOTraceScreenModel(options ioTraceOptions, eventCh <-chan ioEvent, resultCh <-chan ioTraceFinishedMsg, stop func()) ioTraceScreenModel {
	input := textinput.New()
	input.Prompt = "filter / "
	input.Placeholder = "device, process, cgroup, event"
	input.SetWidth(48)
	return ioTraceScreenModel{options: options, groupBy: options.groupBy, input: input, selected: -1, width: 80, height: 24, stop: stop, eventCh: eventCh, resultCh: resultCh}
}
func (m ioTraceScreenModel) Init() tea.Cmd { return waitIOTraceMessage(m.eventCh, m.resultCh) }
func waitIOTraceMessage(events <-chan ioEvent, result <-chan ioTraceFinishedMsg) tea.Cmd {
	return func() tea.Msg {
		select {
		case event := <-events:
			batch := []ioEvent{event}
			for len(batch) < traceEventBatchLimit {
				select {
				case event := <-events:
					batch = append(batch, event)
				default:
					return ioTraceEventMsg{batch}
				}
			}
			return ioTraceEventMsg{batch}
		case done := <-result:
			return done
		}
	}
}
func (m ioTraceScreenModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = value.Width, value.Height
		m.input.SetWidth(max(16, min(60, value.Width-12)))
	case ioTraceEventMsg:
		m.events = append(m.events, value.events...)
		if len(m.events) > traceScreenEventLimit {
			m.events = m.events[len(m.events)-traceScreenEventLimit:]
		}
	case ioTraceFinishedMsg:
		m.summary, m.finished = value.summary, &value
		return m, tea.Quit
	case traceStopMsg:
		m.requestStop()
		return m, waitIOTraceMessage(m.eventCh, m.resultCh)
	case tea.KeyPressMsg:
		if value.String() == "ctrl+c" || value.String() == "q" {
			m.requestStop()
			return m, waitIOTraceMessage(m.eventCh, m.resultCh)
		}
		if m.filtering {
			switch value.String() {
			case "enter":
				m.filter = strings.TrimSpace(m.input.Value())
				m.input.Blur()
				m.filtering = false
				m.selected = -1
				return m, nil
			case "esc":
				m.input.Blur()
				m.filtering = false
				return m, nil
			}
			var command tea.Cmd
			m.input, command = m.input.Update(value)
			return m, command
		}
		switch value.String() {
		case "/":
			m.input.SetValue(m.filter)
			m.filtering = true
			return m, m.input.Focus()
		case "tab":
			m.groupBy = nextIOGroup(m.groupBy, 1)
		case "shift+tab":
			m.groupBy = nextIOGroup(m.groupBy, -1)
		case "d":
			m.groupBy = traceGroupByDevice
		case "p":
			m.groupBy = traceGroupByProcess
		case "c":
			m.groupBy = traceGroupByCgroup
		case "e":
			m.groupBy = traceGroupByEvent
		case "g":
			m.groupBy = ""
		case "up", "k":
			if m.groupBy == "" {
				m.selected = max(0, m.selected-1)
			}
		case "down", "j":
			if m.groupBy == "" {
				m.selected = min(len(m.filtered())-1, max(0, m.selected+1))
			}
		case "enter":
			if m.groupBy == "" && m.selected < 0 && len(m.filtered()) > 0 {
				m.selected = len(m.filtered()) - 1
			}
		case "esc":
			m.filter, m.selected = "", -1
		}
	}
	return m, nil
}
func (m *ioTraceScreenModel) requestStop() {
	if !m.stopping {
		m.stopping = true
		if m.stop != nil {
			m.stop()
		}
	}
}
func nextIOGroup(current string, step int) string {
	views := []string{"", traceGroupByDevice, traceGroupByProcess, traceGroupByCgroup, traceGroupByEvent}
	return nextTraceGroup(views, current, step)
}
func (m ioTraceScreenModel) filtered() []ioEvent {
	values := make([]ioEvent, 0, len(m.events))
	for _, event := range m.events {
		if m.filter == "" || strings.Contains(strings.ToLower(event.Device+" "+event.Process+" "+event.Event+" "+event.Operation), strings.ToLower(m.filter)) {
			values = append(values, event)
		}
	}
	return values
}
func (m ioTraceScreenModel) View() tea.View {
	header := fmt.Sprintf("trace io  ·  %s  ·  %d events  ·  / filter  tab view  d device  p process  c cgroup  e event  g scroll  ↑↓ select  enter detail  q quit", ioViewName(m.groupBy), len(m.events))
	lines := []string{truncateLine(header, m.width)}
	if m.filtering {
		lines = append(lines, m.input.View())
	}
	if m.groupBy == "" {
		values := m.filtered()
		start := max(0, len(values)-max(1, m.height-len(lines)-1))
		if m.selected >= 0 {
			start = max(0, min(m.selected, max(0, len(values)-1)))
		}
		for index, event := range values[start:] {
			lines = append(lines, ioScreenEventRow(event, m.width, start+index == m.selected))
		}
		if m.selected >= 0 && m.selected < len(values) {
			lines = append(lines, truncateLine(ioDetail(values[m.selected]), m.width))
		}
	} else {
		for _, row := range m.groups() {
			lines = append(lines, truncateLine(row, m.width))
		}
	}
	view := liveFrame(strings.Join(lines, "\n")+"\n", m.height)
	view.AltScreen = true
	return view
}
func ioViewName(group string) string {
	if group == "" {
		return "events"
	}
	return group
}
func ioScreenEventRow(event ioEvent, width int, selected bool) string {
	prefix := "  "
	if selected {
		prefix = "› "
	}
	text := fmt.Sprintf("%s%-12s %-12s %-8s %8s q:%s s:%s t:%s", prefix, event.Process, event.Device, event.Operation, traceBytes(event.Bytes), traceLatency(event.QueueMS, "ms"), traceLatency(event.ServiceMS, "ms"), traceLatency(event.TotalMS, "ms"))
	return truncateLine(text, width)
}
func ioDetail(event ioEvent) string {
	return fmt.Sprintf("%s %s %s bytes=%d pid=%d cgroup=%d attribution=%s queue=%s service=%s total=%s", event.Event, event.Operation, event.Device, event.Bytes, event.PID, event.CgroupID, event.Attribution, traceLatency(event.QueueMS, "ms"), traceLatency(event.ServiceMS, "ms"), traceLatency(event.TotalMS, "ms"))
}
func (m ioTraceScreenModel) groups() []string {
	counts := map[string]struct{ ops, bytes uint64 }{}
	for _, event := range m.filtered() {
		key := event.Event
		switch m.groupBy {
		case traceGroupByDevice:
			key = event.Device
		case traceGroupByProcess:
			key = event.Process
		case traceGroupByCgroup:
			key = fmt.Sprint(event.CgroupID)
		}
		value := counts[key]
		value.ops++
		value.bytes += event.Bytes
		counts[key] = value
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([]string, 0, len(keys))
	for _, key := range keys {
		value := counts[key]
		rows = append(rows, fmt.Sprintf("%-20s ops=%d bytes=%s", key, value.ops, traceBytes(value.bytes)))
	}
	return rows
}
func runIOTraceScreen(options ioTraceOptions) int {
	eventCh := make(chan ioEvent, 256)
	resultCh := make(chan ioTraceFinishedMsg, 1)
	stop := make(chan struct{})
	var once sync.Once
	stopCapture := func() { once.Do(func() { close(stop) }) }
	go func() {
		summary, err := collectIOEvents(options, func(event ioEvent) error {
			select {
			case eventCh <- event:
				return nil
			case <-stop:
				return nil
			}
		}, stop)
		resultCh <- ioTraceFinishedMsg{summary, err}
	}()
	model := newIOTraceScreenModel(options, eventCh, resultCh, stopCapture)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	go func() { <-ctx.Done(); stopCapture(); program.Send(traceStopMsg{}) }()
	final, err := program.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	screen := final.(ioTraceScreenModel)
	if screen.finished == nil {
		return 1
	}
	if screen.finished.err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.io_failed", screen.finished.err))
		return 1
	}
	printIOReport(ioReportFor(screen.events, screen.summary), screen.events, false)
	return 0
}
