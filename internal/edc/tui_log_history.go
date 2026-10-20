package edc

import (
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const logHistoryRefresh = 2 * time.Second

type logHistoryLoader func() ([]logHistoryGroup, []string, error)

type logHistoryTickMsg struct{ seq int }

type logHistoryLoadedMsg struct {
	seq     int
	groups  []logHistoryGroup
	notices []string
	err     error
}

type logHistoryBrowser struct {
	groups                           []logHistoryGroup
	options                          logHistoryOptions
	notices                          []string
	cursor, rowCursor, width, height int
	rowLimit                         int
	detail, expanded, color          bool
	now                              time.Time
	list, history                    viewport.Model
	load                             logHistoryLoader
	seq                              int
	paused                           bool
	rowPinned                        bool
	// confirm is the deletion waiting for y; status is the last deletion result.
	confirm *logHistoryDeletion
	status  string
}

func newLogHistoryBrowser(groups []logHistoryGroup, options logHistoryOptions, notices []string) logHistoryBrowser {
	model := logHistoryBrowser{groups: groups, options: options, notices: notices, width: 80, height: 24, color: os.Getenv("NO_COLOR") == "", now: time.Now(), list: viewport.New(), history: viewport.New()}
	model.resize()
	return model
}

func (model logHistoryBrowser) contentWidth() int { return max(1, model.width) }

func (model logHistoryBrowser) Init() tea.Cmd {
	if model.load == nil {
		return nil
	}
	return model.tick()
}

func (model logHistoryBrowser) tick() tea.Cmd {
	seq := model.seq
	return tea.Tick(logHistoryRefresh, func(time.Time) tea.Msg { return logHistoryTickMsg{seq: seq} })
}

func (model logHistoryBrowser) reload() tea.Cmd {
	seq, load := model.seq, model.load
	return func() tea.Msg {
		groups, notices, err := load()
		return logHistoryLoadedMsg{seq: seq, groups: groups, notices: notices, err: err}
	}
}

// apply는 고른 명령과 실행을 순서가 아니라 키와 시작 시각으로 다시 찾는다. 갱신하면 커서 위에 명령이 끼어들고,
// 회전하면 실행이 든 파일 이름이 바뀐다.
func (model *logHistoryBrowser) apply(msg logHistoryLoadedMsg) {
	model.now = time.Now()
	if msg.err != nil {
		model.notices = []string{msg.err.Error()}
		model.resize()
		return
	}
	var key string
	var run *logHistoryAttempt
	if len(model.groups) > 0 {
		key = model.groups[model.cursor].Key
		if model.detail && model.rowPinned {
			if rows := model.rows(); model.rowCursor >= 0 && model.rowCursor < len(rows) {
				run = &rows[model.rowCursor]
			}
		}
	}
	model.groups, model.notices = msg.groups, msg.notices
	model.cursor = min(model.cursor, max(0, len(model.groups)-1))
	found := false
	for index, group := range model.groups {
		if group.Key == key {
			model.cursor, found = index, true
			break
		}
	}
	if !found {
		model.detail, model.expanded, model.rowCursor, model.rowPinned = false, false, 0, false
	} else if run != nil {
		kept := false
		for index, row := range model.rows() {
			if row.Started.Equal(run.Started) && row.Path == run.Path {
				model.rowCursor, kept = index, true
				break
			}
		}
		if !kept {
			model.expanded, model.rowPinned, model.rowCursor = false, false, 0
		}
	} else {
		model.rowCursor = 0
	}
	model.resize()
}

func (model *logHistoryBrowser) resize() {
	if !model.options.limitExplicit {
		model.rowLimit = max(model.rowLimit, max(model.options.Limit, model.height-len(model.headerLines())-len(model.footerLines())))
	}
	model.list.SetWidth(model.contentWidth())
	model.list.SetHeight(max(1, model.height-3-len(model.footerLines())))
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
	options := model.options
	if !options.limitExplicit {
		options.Limit = max(options.Limit, model.rowLimit)
	}
	return historyRows(model.groups[model.cursor], options)
}

func (model *logHistoryBrowser) refreshHistory() {
	rows := model.rows()
	model.rowCursor = min(model.rowCursor, max(0, len(rows)-1))
	var lines []string
	if model.expanded && len(rows) > 0 {
		lines = model.attemptLines(rows[model.rowCursor], model.contentWidth())
	} else {
		for index, row := range rows {
			lines = append(lines, historyRunRow(row, model.tableWidth(), index == model.rowCursor, model.color, historyBaseline(model.groups[model.cursor].Rows, row)))
		}
		if len(rows) == 0 {
			lines = append(lines, T("cli.log_history.no_match"))
		}
	}
	content := strings.Join(lines, "\n")
	model.history.SetWidth(model.tableWidth())
	model.history.SetHeight(max(1, model.height-len(model.headerLines())-len(model.footerLines())))
	model.history.SetContent(content)
	if !model.expanded {
		keepHistorySelection(&model.history, model.rowCursor)
	}
}

func (model *logHistoryBrowser) summary() []string {
	var trend []string
	if len(model.groups) > 0 {
		if line := historyTrend(model.groups[model.cursor], model.now, model.color); line != "" {
			trend = append(trend, line)
		}
	}
	return append(trend, model.summaryLines()...)
}

func (model *logHistoryBrowser) summaryLines() []string {
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
	case logHistoryTickMsg:
		if value.seq != model.seq || model.paused {
			return model, nil
		}
		return model, model.reload()
	case logHistoryLoadedMsg:
		if value.seq != model.seq {
			return model, nil
		}
		model.apply(value)
		return model, model.tick()
	case tea.KeyPressMsg:
		if model.confirm != nil {
			return model.answerDeletion(value.String() == "y")
		}
		if value.String() == "d" {
			model.planDeletion()
			model.resize()
			return model, nil
		}
		switch value.String() {
		case "q", "ctrl+c":
			return model, tea.Quit
		case "r", "p":
			if model.load == nil {
				break
			}
			model.seq++
			if value.String() == "p" {
				model.paused = !model.paused
				if model.paused {
					return model, nil
				}
			}
			return model, model.reload()
		case "esc", "b":
			if model.expanded {
				model.expanded = false
				model.refreshHistory()
				return model, nil
			}
			if model.detail {
				model.detail = false
				model.resize()
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
					model.rowPinned = true
					model.history.GotoTop()
				}
			}
			switch value.String() {
			case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
				model.rowPinned = model.rowCursor != 0
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
					model.rowPinned = false
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
	} else {
		lines = model.headerLines()
		if model.detail {
			lines = append(lines, model.historyLines()...)
		} else if len(model.groups) > 0 {
			lines = append(lines, strings.Split(model.list.View(), "\n")...)
		} else {
			lines = append(lines, T("cli.log_history.no_match"))
		}
		footer := model.footerLines()
		for len(lines)+len(footer) < model.height {
			lines = append(lines, "")
		}
		lines = append(lines, footer...)
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, model.contentWidth(), "…")
	}
	if len(lines) > model.height {
		lines = lines[:max(1, model.height)]
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// planDeletion selects the runs of the command in the list, or the run in the
// run list, and asks before it deletes anything.
func (model *logHistoryBrowser) planDeletion() {
	if len(model.groups) == 0 {
		return
	}
	targets := model.groups[model.cursor].Rows
	if model.detail {
		rows := model.rows()
		if len(rows) == 0 {
			return
		}
		targets = rows[model.rowCursor : model.rowCursor+1]
	}
	plan := planLogHistoryDeletion(targets)
	if plan.Runs == 0 {
		model.status = T("cli.log_history.delete_none", plan.Shared, plan.Running)
		return
	}
	model.confirm, model.status = &plan, ""
}

func (model logHistoryBrowser) answerDeletion(yes bool) (tea.Model, tea.Cmd) {
	plan := *model.confirm
	model.confirm = nil
	if !yes {
		model.resize()
		return model, nil
	}
	deleted, err := plan.execute()
	model.status = T("cli.log_history.delete_done", deleted.Runs, deleted.Files, formatSizeShort(deleted.Bytes))
	if skipped := deleted.Shared + deleted.Running; skipped > 0 {
		model.status += " · " + T("cli.log_history.delete_skipped", skipped)
	}
	if err != nil {
		model.status += " · " + T("cli.log_history.delete_failed", reportIdentityValue(err.Error()))
	}
	model.resize()
	if model.load == nil {
		return model, nil
	}
	model.seq++
	return model, model.reload()
}

func (model logHistoryBrowser) deletionPrompt() string {
	plan := model.confirm
	text := T("cli.log_history.delete_confirm", plan.Runs, plan.Files, formatSizeShort(plan.Bytes))
	if plan.Shared+plan.Running > 0 {
		text += "  " + T("cli.log_history.delete_kept", plan.Shared, plan.Running)
	}
	return historyStrong(text, richYellow, model.color)
}

func (model logHistoryBrowser) liveState() string {
	switch {
	case model.load == nil:
		return ""
	case model.paused:
		return " · " + T("cli.log_history.paused")
	default:
		return " · " + T("cli.log_history.live")
	}
}

// keyHints는 화면별 키 안내 뒤에 갱신 키를 붙이고, 멈춘 동안 p를 켜진 항목으로 칠한다.
func (model logHistoryBrowser) keyHints(key string) string {
	if model.load == nil {
		return liveKeyHints(T(key), model.color)
	}
	var on []string
	if model.paused {
		on = append(on, "p")
	}
	return liveKeyHints(T(key)+T("cli.log_history.live_help"), model.color, on...)
}

func runLogHistoryBrowser(input, output *os.File, groups []logHistoryGroup, options logHistoryOptions, notices []string, load logHistoryLoader) error {
	model := newLogHistoryBrowser(groups, options, notices)
	model.load = load
	_, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	return err
}
