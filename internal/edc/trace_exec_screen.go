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
		if m.groupBy == "" && len(m.events) > 0 {
			m.selected = min(len(m.events)-1, max(0, m.selected+1))
		}
	case "enter":
		if m.groupBy == "" && len(m.events) > 0 {
			if m.selected < 0 {
				m.selected = len(m.events) - 1
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

func execScreenTime(event execEvent) string {
	return time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000")
}

func (m execTraceScreenModel) View() tea.View {
	status := "live"
	if m.stopping {
		status = "stopping"
	}
	lines := []string{fmt.Sprintf("edc trace exec  ·  %s  ·  events %d  ·  filter %s", status, len(m.events), emptyAs(m.filter, "all"))}
	if m.filtering {
		lines = append(lines, m.input.View()+"  enter apply  esc cancel")
	} else {
		lines = append(lines, "/ filter  tab view  p process  a parent  e event  c cgroup  g scroll  ↑↓ select  enter detail  q quit  ctrl-c stop")
	}
	if m.detail && m.selected >= 0 && m.selected < len(m.events) {
		event := m.events[m.selected]
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
		lines = append(lines, fmt.Sprintf("%-24s %6s %6s %8s %8s %6s %10s", strings.ToUpper(m.groupBy), "EXEC", "EXIT", "NONZERO", "SIGNALED", "SHORT", "AVG LIFE"))
		for _, row := range report.Rows {
			lines = append(lines, fmt.Sprintf("%-24s %6d %6d %8d %8d %6d %10s", row.Group, row.Execs, row.Exits, row.NonZeroExits, row.Signaled, row.ShortLived, execOptionalLifetime(row.AvgLifetimeMS)))
		}
		if len(report.Signals) > 0 {
			lines = append(lines, "", fmt.Sprintf("%-10s %-20s %-20s %-18s %6s", "SIGNAL", "SENDER", "TARGET", "RESULT", "COUNT"))
			sort.SliceStable(report.Signals, func(i, j int) bool { return report.Signals[i].Count > report.Signals[j].Count })
			for _, row := range report.Signals {
				lines = append(lines, fmt.Sprintf("%-10s %-20s %-20s %-18s %6d", row.Signal, row.Sender, row.Target, row.Result, row.Count))
			}
		}
	} else {
		lines = append(lines, "TIME          EVENT   PID      PROCESS           DETAIL")
		for index, event := range m.matching() {
			prefix := " "
			if m.selected == index {
				prefix = liveSelectedBar
			}
			lines = append(lines, fmt.Sprintf("%s%s  %-6s  %-7d  %-16s  %s", prefix, execScreenTime(event), event.Event, event.PID, event.Process, execEventDetail(event)))
		}
	}
	view := liveFrame(strings.Join(lines, "\n")+"\n", m.height)
	view.AltScreen = true
	return view
}
