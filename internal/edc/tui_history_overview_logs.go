package edc

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type historyOverviewLogList struct {
	logs            []historyOverviewLog
	selected, width int
	height          int
	color           bool
}

var historyOverviewLogBrowserFor = func() (logHistoryBrowser, error) {
	options, cache := logHistoryOptions{}, logHistoryCache{}
	snapshot, err := loadLogHistory(options, cache)
	if err != nil {
		return logHistoryBrowser{}, err
	}
	groups, _ := logHistoryGroups(snapshot, options)
	browser := newLogHistoryBrowser(groups, options, append(snapshot.Issues, snapshot.Notices...))
	browser.load = logHistoryLoaderFor(options, cache)
	return browser, nil
}

func newHistoryOverviewLogList(logs []historyOverviewLog, color bool) historyOverviewLogList {
	return historyOverviewLogList{logs: logs, width: 80, height: 24, color: color}
}

func (model historyOverviewLogList) Init() tea.Cmd { return nil }
func (model historyOverviewLogList) atTop() bool   { return true }

func (model historyOverviewLogList) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		model.width, model.height = size.Width, size.Height
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "up", "k":
			model.selected = max(0, model.selected-1)
		case "down", "j":
			model.selected = min(len(model.logs)-1, model.selected+1)
		}
	}
	return model, nil
}

func (model historyOverviewLogList) View() tea.View {
	lines := []string{T("history.overview.logs"), T("history.overview.logs_time_only"), historyOverviewLogTableHeader()}
	for index, log := range model.logs {
		line := historyOverviewLogRow(log.Command, log.Started.Local().Format(time.DateTime), log.Outcome, historyShortDuration(log.Duration))
		if index == model.selected {
			line = historySelect(line, model.color)
		}
		lines = append(lines, line)
	}
	lines = append(lines, "")
	lines = append(lines, historyReviewKeyHints(T("history.overview.browser.log_help"), model.width, model.color)...)
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], max(1, model.width), "…")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model historyOverviewLogList) navigate(key string, db *sql.DB, path string) (tea.Model, bool, error) {
	if key != "enter" || len(model.logs) == 0 {
		return nil, false, nil
	}
	selected := model.logs[model.selected]
	browser, err := historyOverviewLogBrowserFor()
	if err != nil {
		return nil, true, err
	}
	groups := browser.groups
	for groupIndex := range groups {
		if groups[groupIndex].Key != selected.Key {
			continue
		}
		browser.cursor, browser.detail, browser.rowPinned = groupIndex, true, true
		for rowIndex, row := range browser.rows() {
			if row.Started.Equal(selected.Started) && row.Path == selected.Path {
				browser.rowCursor = rowIndex
				break
			}
		}
		browser.resize()
		return browser, true, nil
	}
	return nil, true, fmt.Errorf("%s", T("cli.log_history.no_match"))
}
