package edc

import (
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type logHistoryBrowser struct {
	groups                           []logHistoryGroup
	options                          logHistoryOptions
	notices                          []string
	cursor, rowCursor, width, height int
	detail, expanded, color          bool
	now                              time.Time
	list, history                    viewport.Model
}

func newLogHistoryBrowser(groups []logHistoryGroup, options logHistoryOptions, notices []string) logHistoryBrowser {
	model := logHistoryBrowser{groups: groups, options: options, notices: notices, width: 80, height: 24, color: os.Getenv("NO_COLOR") == "", now: time.Now(), list: viewport.New(), history: viewport.New()}
	model.resize()
	return model
}

func (model logHistoryBrowser) contentWidth() int { return max(1, min(model.width, 88)) }

func (model logHistoryBrowser) Init() tea.Cmd { return nil }

func (model *logHistoryBrowser) resize() {
	model.list.SetWidth(model.contentWidth())
	model.list.SetHeight(max(1, min(len(model.groups), model.height-7)))
	model.history.SetWidth(model.contentWidth())
	model.refreshList()
	if len(model.groups) > 0 {
		model.refreshHistory()
	}
}

func (model *logHistoryBrowser) refreshList() {
	var lines []string
	columns := historyKeyWidths(model.contentWidth(), model.groups)
	for index, group := range model.groups {
		lines = append(lines, historyKeyRowColumns(group, columns, index == model.cursor, model.color, model.now))
	}
	model.list.SetContent(strings.Join(lines, "\n"))
	keepHistorySelection(&model.list, model.cursor)
}

func keepHistorySelection(view *viewport.Model, cursor int) {
	if cursor < view.YOffset() {
		view.SetYOffset(cursor)
	}
	if cursor >= view.YOffset()+view.Height() {
		view.SetYOffset(cursor - view.Height() + 1)
	}
}

func (model *logHistoryBrowser) rows() []logHistoryAttempt {
	if len(model.groups) == 0 {
		return nil
	}
	return historyRows(model.groups[model.cursor], model.options)
}

func (model *logHistoryBrowser) refreshHistory() {
	rows := model.rows()
	model.rowCursor = min(model.rowCursor, max(0, len(rows)-1))
	var lines []string
	if model.expanded && len(rows) > 0 {
		row := rows[model.rowCursor]
		lines = []string{historyCommand(row), "", historyTime(row.Started) + "  ·  " + historyDuration(row) + "  ·  " + historyOutcome(row), T("cli.log_history.attempt", row.Attempt), "", "cwd " + reportIdentityValue(row.CWD), "key " + row.Key, "file " + reportIdentityValue(row.Path)}
		for i, line := range lines {
			lines[i] = ansi.Wrap(line, model.contentWidth(), "")
		}
	} else {
		for index, row := range rows {
			lines = append(lines, historyRunRow(row, model.contentWidth(), index == model.rowCursor, model.color))
		}
		if len(rows) == 0 {
			lines = append(lines, T("cli.log_history.no_match"))
		}
	}
	content := strings.Join(lines, "\n")
	footer := 8
	if !model.expanded {
		footer += len(model.summary())
	}
	model.history.SetHeight(max(1, min(strings.Count(content, "\n")+1, model.height-footer)))
	model.history.SetContent(content)
	if !model.expanded {
		keepHistorySelection(&model.history, model.rowCursor)
	}
}

func (model *logHistoryBrowser) summary() []string {
	rows := model.rows()
	contexts := map[string]bool{}
	for _, row := range rows {
		contexts[row.CWD] = true
	}
	if len(contexts) > 1 {
		return []string{T("cli.log_history.contexts_note", len(contexts))}
	}
	return historyCompactSummary(rows, model.color)
}

func (model logHistoryBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.resize()
		return model, nil
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "ctrl+c":
			return model, tea.Quit
		case "esc", "b":
			if model.expanded {
				model.expanded = false
				model.refreshHistory()
				return model, nil
			}
			if model.detail {
				model.detail = false
				return model, nil
			}
			if value.String() == "esc" {
				return model, tea.Quit
			}
		}
		if model.expanded {
			if value.String() == "enter" || value.String() == "e" {
				model.expanded = false
				model.refreshHistory()
				return model, nil
			}
		} else if model.detail {
			rows := model.rows()
			switch value.String() {
			case "up", "k":
				model.rowCursor = max(0, model.rowCursor-1)
			case "down", "j":
				model.rowCursor = min(max(0, len(rows)-1), model.rowCursor+1)
			case "pgup":
				model.rowCursor = max(0, model.rowCursor-model.history.Height())
			case "pgdown":
				model.rowCursor = min(max(0, len(rows)-1), model.rowCursor+model.history.Height())
			case "home":
				model.rowCursor = 0
			case "end":
				model.rowCursor = max(0, len(rows)-1)
			case "enter", "e":
				if len(rows) > 0 {
					model.expanded = true
					model.history.GotoTop()
				}
			}
			model.refreshHistory()
			return model, nil
		} else {
			switch value.String() {
			case "up", "k":
				model.cursor = max(0, model.cursor-1)
			case "down", "j":
				model.cursor = min(max(0, len(model.groups)-1), model.cursor+1)
			case "pgup":
				model.cursor = max(0, model.cursor-model.list.Height())
			case "pgdown":
				model.cursor = min(max(0, len(model.groups)-1), model.cursor+model.list.Height())
			case "home":
				model.cursor = 0
			case "end":
				model.cursor = max(0, len(model.groups)-1)
			case "enter":
				if len(model.groups) > 0 {
					model.detail = true
					model.rowCursor = 0
					model.expanded = false
					model.refreshHistory()
					model.history.GotoTop()
				}
			}
			model.refreshList()
			return model, nil
		}
	}
	if model.expanded {
		var cmd tea.Cmd
		model.history, cmd = model.history.Update(msg)
		return model, cmd
	}
	return model, nil
}

func (model logHistoryBrowser) View() tea.View {
	var lines []string
	if model.height < 7 || model.width < 40 || model.detail && model.height < 9 {
		lines = []string{T("cli.log_history.small"), T("cli.log_history.quit_help")}
	} else if model.detail {
		group := model.groups[model.cursor]
		title := historyCommand(group.Representative) + " · " + T("cli.log_history.run_count", len(model.rows()))
		lines = []string{historyTint(title, "1", model.color), ""}
		if !model.expanded {
			lines = append(lines, historyTint(historyRunHeader(model.contentWidth()), "38;5;246", model.color))
		}
		lines = append(lines, strings.TrimRight(model.history.View(), "\n"), "")
		if !model.expanded {
			lines = append(lines, model.summary()...)
			lines = append(lines, "")
		}
		lines = append(lines, historyTint(T("cli.log_history.detail_help"), "38;5;246", model.color))
		if model.expanded {
			lines[len(lines)-1] = historyTint(T("cli.log_history.expanded_help"), "38;5;246", model.color)
		}
	} else {
		lines = []string{historyTint(T("cli.log_history.browser_title", len(model.groups)), "1", model.color), "", historyTint(historyKeyHeader(model.contentWidth(), model.groups), "38;5;246", model.color)}
		if len(model.groups) > 0 {
			lines = append(lines, strings.TrimRight(model.list.View(), "\n"))
		} else {
			lines = append(lines, T("cli.log_history.no_match"))
		}
		lines = append(lines, "", historyTint(T("cli.log_history.list_help"), "38;5;246", model.color))
	}
	if len(model.notices) > 0 {
		lines = append(lines, historyTint(model.notices[0], "33", model.color))
		if len(model.notices) > 1 {
			lines = append(lines, historyTint(T("cli.log_history.more_notices", len(model.notices)-1), "33", model.color))
		}
	}
	body := strings.Split(strings.Join(lines, "\n"), "\n")
	for i, line := range body {
		body[i] = ansi.Truncate(line, model.contentWidth(), "…")
	}
	if len(body) > model.height {
		body = body[:max(1, model.height)]
	}
	view := tea.NewView(strings.Join(body, "\n"))
	view.AltScreen = true
	return view
}

func runLogHistoryBrowser(input, output *os.File, groups []logHistoryGroup, options logHistoryOptions, notices []string) error {
	_, err := tea.NewProgram(newLogHistoryBrowser(groups, options, notices), tea.WithInput(input), tea.WithOutput(output)).Run()
	return err
}
