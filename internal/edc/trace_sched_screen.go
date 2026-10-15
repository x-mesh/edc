//go:build linux

package edc

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type schedTraceScreenModel struct {
	events        []schedEvent
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

type schedTraceEventMsg struct{ events []schedEvent }

func newSchedTraceScreenModel(stop func()) schedTraceScreenModel {
	input := textinput.New()
	input.SetStyles(traceInputStyles())
	input.Prompt = "filter / "
	input.Placeholder = "process, event, or syscall"
	input.SetWidth(48)
	return schedTraceScreenModel{stop: stop, input: input, selected: -1, width: 80, height: 24}
}

func (m schedTraceScreenModel) Init() tea.Cmd { return nil }

func (m schedTraceScreenModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = value.Width, value.Height
	case schedTraceEventMsg:
		m.events = append(m.events, value.events...)
		if len(m.events) > traceScreenEventLimit {
			m.events = m.events[len(m.events)-traceScreenEventLimit:]
		}
	case tea.KeyPressMsg:
		return m.updateKey(value)
	}
	return m, nil
}

func (m schedTraceScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	switch key.String() {
	case "/":
		m.input.SetValue(m.filter)
		m.filtering = true
		return m, m.input.Focus()
	case "p":
		m.groupBy = traceGroupByProcess
	case "e":
		m.groupBy = traceGroupByEvent
	case "c":
		m.groupBy = "cgroup"
	case "s":
		m.groupBy = schedGroupBySyscall
	case "g":
		m.groupBy = ""
	case "tab":
		if m.groupBy == "" {
			m.groupBy = traceGroupByProcess
		} else if m.groupBy == traceGroupByProcess {
			m.groupBy = traceGroupByEvent
		} else if m.groupBy == traceGroupByEvent {
			m.groupBy = "cgroup"
		} else if m.groupBy == "cgroup" {
			m.groupBy = schedGroupBySyscall
		} else {
			m.groupBy = ""
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

func (m schedTraceScreenModel) matching() []schedEvent {
	result := make([]schedEvent, 0, len(m.events))
	filter := strings.ToLower(strings.TrimSpace(m.filter))
	for _, event := range m.events {
		if filter == "" || strings.Contains(strings.ToLower(event.Process+" "+event.Event+" "+event.Syscall), filter) {
			result = append(result, event)
		}
	}
	return result
}

func (m schedTraceScreenModel) View() tea.View {
	color := traceScreenColor()
	// 평문 화면은 예전처럼 자르지 않는다. 색을 쓰면 칠한 줄이 화면 폭을 넘지 않게 자른다.
	width := 0
	if color {
		width = m.width
	}
	lines := []string{traceTexts(traceStatusSpans("edc trace sched", m.stopping, len(m.events), m.filter), width, color)}
	if m.filtering {
		lines = append(lines, traceFilterHints(m.input.View(), width, color))
	} else {
		lines = append(lines, traceKeyHints("/ filter  tab view  p process  e event  c cgroup  s syscall  g scroll  ↑↓ select  enter detail  q quit  ctrl-c stop", width, color))
	}
	if m.detail && m.selected >= 0 && m.selected < len(m.events) {
		event := m.events[m.selected]
		lines = append(lines, fmt.Sprintf("%s pid %d process %s latency %.3fms", event.Event, event.PID, event.Process, event.LatencyMS), fmt.Sprintf("cgroup %d  ·  offcpu class %s  ·  syscall %s", event.CgroupID, emptyAs(event.OffCPUClass, "-"), emptyAs(event.Syscall, schedNoSyscall)))
	} else if m.groupBy != "" {
		counts := map[string]int{}
		for _, event := range m.matching() {
			counts[schedGroupKey(event, m.groupBy)]++
		}
		keys := make([]string, 0, len(counts))
		for key := range counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		lines = append(lines, traceColumnTitles(strings.ToUpper(m.groupBy)+"                 EVENTS", width, color))
		for _, key := range keys {
			lines = append(lines, fmt.Sprintf("%-24s %d", key, counts[key]))
		}
	} else {
		lines = append(lines, traceColumnTitles("EVENT              PROCESS                  LATENCY  CLASS      SYSCALL", width, color))
		for index, event := range m.matching() {
			prefix := " "
			if m.selected == index {
				prefix = liveSelectedBar
			}
			line := fmt.Sprintf("%s%-18s %-24s %7.3f  %-9s  %s", prefix, event.Event, event.Process, event.LatencyMS, emptyAs(event.OffCPUClass, "-"), emptyAs(event.Syscall, schedNoSyscall))
			if color && m.selected == index {
				line = traceSelectedEvent(traceFit(line, width), width, true)
			} else if color {
				line = traceFit(line, width)
			}
			lines = append(lines, line)
		}
	}
	view := liveFrame(strings.Join(lines, "\n")+"\n", m.height)
	view.AltScreen = true
	return view
}
