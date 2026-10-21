package edc

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type historyMenuBrowser struct {
	db                      *sql.DB
	path, run               string
	selected, width, height int
	color                   bool
	notice                  string
}

var historyMenuItems = []string{"trend", "overview", "growth", "prune", "log", "export"}

func newHistoryMenuBrowser(db *sql.DB, path, run string) historyMenuBrowser {
	return historyMenuBrowser{db: db, path: path, run: run, width: 80, height: 24, color: os.Getenv("NO_COLOR") == ""}
}

func (model historyMenuBrowser) Init() tea.Cmd { return nil }

func (model historyMenuBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "ctrl+c", "esc":
			return model, tea.Quit
		case "up", "k":
			model.selected = max(0, model.selected-1)
		case "down", "j":
			model.selected = min(len(historyMenuItems)-1, model.selected+1)
		}
	}
	return model, nil
}

func (model historyMenuBrowser) View() tea.View {
	lines := []string{historyStrong(T("history.menu.title"), richCyan, model.color), ""}
	for index, key := range historyMenuItems {
		line := T("history.menu." + key)
		if index == model.selected {
			line = historySelect(line, model.color)
		} else {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	if model.notice != "" {
		lines = append(lines, "", historyTint(model.notice, richYellow, model.color))
	}
	lines = append(lines, "")
	lines = append(lines, historyReviewKeyHints(T("history.menu.help"), model.width, model.color)...)
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model historyMenuBrowser) atTop() bool { return true }
func (model historyMenuBrowser) navigate(key string, db *sql.DB, path string) (tea.Model, bool, error) {
	if key != "enter" {
		return nil, false, nil
	}
	switch historyMenuItems[model.selected] {
	case "trend":
		targets, err := historyTrendTargets(db)
		if err != nil {
			return nil, true, err
		}
		return newHistoryTrendBrowser(db, targets, historyTrendOptions{by: "day"}, time.Local), true, nil
	case "overview":
		child := newHistoryOverviewBrowser(db, path, historyOverviewOptions{})
		child.hosted = true
		return child, true, nil
	case "growth":
		growth, err := collectHistoryGrowth(db, historyGrowthOptions{}, time.Now())
		if err != nil {
			return nil, true, err
		}
		return newHistoryGrowthNavigator(db, path, historyGrowthOptions{}, growth), true, nil
	case "prune":
		options := historyPruneOptions{olderThan: 90 * 24 * time.Hour}
		plan, err := planHistoryPrune(db, options, time.Now())
		if err != nil {
			return nil, true, err
		}
		return newHistoryPruneBrowser(path, options, plan, time.Now()), true, nil
	case "log":
		options := logHistoryOptions{}
		cache := logHistoryCache{}
		snapshot, err := loadLogHistory(options, cache)
		if err != nil {
			return nil, true, err
		}
		groups, _ := logHistoryGroups(snapshot, options)
		notices := append(append([]string{}, snapshot.Issues...), snapshot.Notices...)
		browser := newLogHistoryBrowser(groups, options, notices)
		browser.load = logHistoryLoaderFor(options, cache)
		return browser, true, nil
	case "export":
		var command string
		if err := db.QueryRow("SELECT command FROM runs WHERE id=?", model.run).Scan(&command); err != nil {
			return nil, false, err
		}
		if command == historySnapshotCommand {
			model.notice = fmt.Sprintf(T("history.menu.export_notice"), model.run, path)
		} else {
			model.notice = T("history.menu.export_snapshot_only")
		}
		return model, false, nil
	}
	return model, false, nil
}
