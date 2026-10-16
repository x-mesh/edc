//go:build linux

package edc

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type execTraceScreenModel struct {
	events        []execEvent
	filter        string
	filtering     bool
	input         textinput.Model
	groupBy       string
	selected      int
	detail        bool
	stopping      bool
	width, height int
	stop          func()
}

type execTraceEventMsg struct{ events []execEvent }

func newExecTraceScreenModel(stop func()) execTraceScreenModel {
	input := textinput.New()
	input.SetStyles(traceInputStyles())
	input.Prompt = "filter / "
	input.Placeholder = "process, args, signal, or event"
	input.SetWidth(48)
	return execTraceScreenModel{stop: stop, input: input, selected: -1, width: 80, height: 24}
}

func (m execTraceScreenModel) Init() tea.Cmd { return nil }

func (m execTraceScreenModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = value.Width, value.Height
	case execTraceEventMsg:
		m.events = append(m.events, value.events...)
		if len(m.events) > traceScreenEventLimit {
			m.events = m.events[len(m.events)-traceScreenEventLimit:]
		}
	case tea.KeyPressMsg:
		return m.updateKey(value)
	}
	return m, nil
}

func (m execTraceScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" || key.String() == "q" {
		m.stopping = true
		if m.stop != nil {
			m.stop()
		}
		return m, tea.Quit
	}
	if m.filtering {
		switch key.String() {
		case "enter":
			m.filter = strings.TrimSpace(m.input.Value())
			m.input.Blur()
			m.filtering = false
			m.selected = -1
			return m, nil
		case "esc":
			m.input.SetValue(m.filter)
			m.input.Blur()
			m.filtering = false
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(key)
		return m, cmd
	}
	if m.detail {
		if key.String() == "enter" || key.String() == "esc" {
			m.detail = false
		}
		return m, nil
	}
	views := []string{"", traceGroupByProcess, execGroupByParent, traceGroupByEvent, execGroupByCgroup}
	switch key.String() {
	case "/":
		m.input.SetValue(m.filter)
		m.filtering = true
		return m, m.input.Focus()
	case "p":
		m.groupBy = traceGroupByProcess
	case "a":
		m.groupBy = execGroupByParent
	case "e":
		m.groupBy = traceGroupByEvent
	case "c":
		m.groupBy = execGroupByCgroup
	case "g":
		m.groupBy = ""
	case "tab":
		for index, view := range views {
			if view == m.groupBy {
				m.groupBy = views[(index+1)%len(views)]
				break
			}
		}
	case "up", "k":
		if m.groupBy == "" {
			m.selected = max(0, m.selected-1)
		}
	case "down", "j":
		if count := len(m.matching()); m.groupBy == "" && count > 0 {
			m.selected = min(count-1, max(0, m.selected+1))
		}
	case "enter":
		if count := len(m.matching()); m.groupBy == "" && count > 0 {
			if m.selected < 0 {
				m.selected = count - 1
			}
			m.detail = true
		}
	case "end", "l":
		m.selected = -1
	case "esc":
		m.filter = ""
		m.selected = -1
	}
	return m, nil
}

func (m execTraceScreenModel) matching() []execEvent {
	result := make([]execEvent, 0, len(m.events))
	filter := strings.ToLower(strings.TrimSpace(m.filter))
	for _, event := range m.events {
		if filter == "" || strings.Contains(strings.ToLower(event.Process+" "+event.Event+" "+event.Args+" "+event.Signal+" "+event.Sender+" "+event.Parent), filter) {
			result = append(result, event)
		}
	}
	return result
}

// execEventSeverity는 exec event가 알리는 문제의 색이다. OOM은 빨강, signal과 0이 아닌 종료는 노랑이다.
func execEventSeverity(event execEvent) string {
	switch {
	case event.Event == execEventOOM || event.OOMKilled:
		return richRed
	case event.Event == execEventSignal, event.Event == execEventExit && (event.Signal != "" || (event.ExitCode != nil && *event.ExitCode != 0)):
		return richYellow
	}
	return ""
}

func execScreenTime(event execEvent) string {
	return time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000")
}

func (m execTraceScreenModel) View() tea.View {
	color := traceScreenColor()
	// 평문 화면은 예전처럼 자르지 않는다. 색을 쓰면 칠한 줄이 화면 폭을 넘지 않게 자른다.
	width := 0
	if color {
		width = m.width
	}
	lines := []string{traceTexts(traceStatusSpans("edc trace exec", m.stopping, len(m.events), m.filter), width, color)}
	if m.filtering {
		lines = append(lines, traceFilterHints(m.input.View(), width, color))
	} else {
		const keys = "/ filter  tab view  p process  a parent  e event  c cgroup  g scroll  ↑↓ select  enter detail  q quit  ctrl-c stop"
		lines = append(lines, traceKeyHints(keys, width, color, traceViewOn(keys, m.groupBy)))
	}
	if matching := m.matching(); m.detail && m.selected >= 0 && m.selected < len(matching) {
		event := matching[m.selected]
		lines = append(lines,
			fmt.Sprintf("%s  %s  pid %d  process %s  uid %d  cgroup %d", execScreenTime(event), event.Event, event.PID, event.Process, event.UID, event.CgroupID),
			fmt.Sprintf("parent %s (pid %d)", emptyAs(event.Parent, "-"), event.PPID),
			execEventDetail(event))
		if event.Args != "" {
			lines = append(lines, "args "+event.Args)
		}
	} else if m.groupBy != "" {
		aggregator := newExecAggregator(m.groupBy)
		for _, event := range m.matching() {
			aggregator.add(event)
		}
		report := aggregator.report(time.Second, execSummary{})
		lines = append(lines, traceColumnTitles(fmt.Sprintf("%-24s %6s %6s %8s %8s %6s %4s %10s", strings.ToUpper(m.groupBy), "EXEC", "EXIT", "NONZERO", "SIGNALED", "SHORT", "OOM", "AVG LIFE"), width, color))
		for _, row := range report.Rows {
			lines = append(lines, fmt.Sprintf("%-24s %6d %6d %8d %8d %6d %4d %10s", row.Group, row.Execs, row.Exits, row.NonZeroExits, row.Signaled, row.ShortLived, row.OOMKilled, execOptionalLifetime(row.AvgLifetimeMS)))
		}
		if len(report.Signals) > 0 {
			lines = append(lines, "", traceColumnTitles(fmt.Sprintf("%-10s %-20s %-20s %-18s %6s", "SIGNAL", "SENDER", "TARGET", "RESULT", "COUNT"), width, color))
			sort.SliceStable(report.Signals, func(i, j int) bool { return report.Signals[i].Count > report.Signals[j].Count })
			for _, row := range report.Signals {
				lines = append(lines, fmt.Sprintf("%-10s %-20s %-20s %-18s %6d", row.Signal, row.Sender, row.Target, row.Result, row.Count))
			}
		}
	} else {
		lines = append(lines, traceColumnTitles("TIME          EVENT   PID      PROCESS           DETAIL", width, color))
		for index, event := range m.matching() {
			prefix := " "
			if m.selected == index {
				prefix = liveSelectedBar
			}
			line := fmt.Sprintf("%s%s  %-6s  %-7d  %-16s  %s", prefix, execScreenTime(event), event.Event, event.PID, event.Process, execEventDetail(event))
			switch {
			case !color:
			case m.selected == index:
				line = traceSelectedEvent(traceFit(line, width), width, true)
			default:
				line = traceTexts([]traceText{{text: prefix}, traceLabelSpan(execScreenTime(event)), {text: "  "}, {text: fmt.Sprintf("%-6s", event.Event), color: execEventSeverity(event)},
					{text: fmt.Sprintf("  %-7d  %-16s  %s", event.PID, event.Process, execEventDetail(event))}}, width, true)
			}
			lines = append(lines, line)
		}
	}
	view := liveFrame(strings.Join(lines, "\n")+"\n", m.height)
	view.AltScreen = true
	return view
}
