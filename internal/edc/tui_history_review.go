package edc

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type historyReviewColumn struct {
	title          string
	width, minimum int
}

type historyReviewEntry struct {
	id      string
	cells   []string
	details []string
}

type historyReviewData struct {
	entries        []historyReviewEntry
	summary, empty string
	plan           historyPrunePlan
	pruneOptions   historyPruneOptions
}

type historyReviewLoadedMsg struct {
	seq  int
	data historyReviewData
	err  error
}

type historyReviewTickMsg struct{ seq int }

type historyReviewBrowser struct {
	title, path                       string
	detailsTitle                      string
	extraHelp                         string
	columns                           []historyReviewColumn
	data                              historyReviewData
	view                              viewport.Model
	width, height, selected           int
	color, expanded, paused           bool
	prune, dryRun, confirm, confirmed bool
	checked                           map[string]bool
	seq                               int
	notice                            string
	load                              func() (historyReviewData, error)
	loadPrune                         func(historyPruneOptions) (historyReviewData, error)
	pruneOptions                      historyPruneOptions
	agePicker                         *historyPruneAgePicker
}

func newHistoryReviewBrowser(title, path string, columns []historyReviewColumn, data historyReviewData) historyReviewBrowser {
	model := historyReviewBrowser{title: title, path: path, columns: columns, data: data, view: viewport.New(), width: 80, height: 24, color: os.Getenv("NO_COLOR") == "", checked: map[string]bool{}}
	model.resize()
	return model
}

func (model historyReviewBrowser) split() bool {
	return !model.expanded && model.width >= 132 && model.height >= 16
}

func (model historyReviewBrowser) tableWidth() int {
	if model.split() {
		return max(model.width*3/5, model.width-48)
	}
	return max(1, model.width)
}

func (model historyReviewBrowser) selectedIDs() []string {
	var ids []string
	for _, entry := range model.data.entries {
		if model.checked[entry.id] {
			ids = append(ids, entry.id)
		}
	}
	return ids
}

func (model historyReviewBrowser) helpLines() []string {
	key := "history.review.help"
	if model.expanded {
		key = "history.review.detail_help"
	}
	if model.prune {
		key = "history.review.prune_help"
		if model.dryRun {
			key = "history.review.prune_dry_help"
		}
		if model.expanded {
			key = "history.review.prune_detail_help"
		}
	}
	if model.confirm {
		lines := strings.Split(ansi.Wrap(T("history.review.confirm", len(model.selectedIDs())), max(1, model.width), ""), "\n")
		for i := range lines {
			lines[i] = historyStrong(lines[i], richYellow, model.color)
		}
		return lines
	}
	lines := historyReviewKeyHints(T(key), model.width, model.color, liveHintItem{key: "p", on: model.paused})
	if model.extraHelp != "" && !model.expanded {
		lines = append(lines, historyReviewKeyHints(T(model.extraHelp), model.width, model.color)...)
	}
	return lines
}

func historyReviewKeyHints(text string, width int, color bool, items ...liveHintItem) []string {
	var lines []string
	line, separator := "", "   "
	if width < 60 {
		separator = " "
	}
	for _, hint := range strings.Split(text, "   ") {
		hint = liveKeyHints(strings.TrimSpace(hint), color, items...)
		if line != "" && liveWidth(line)+len(separator)+liveWidth(hint) > max(1, width) {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += separator
		}
		line += hint
	}
	return append(lines, line)
}

func (model historyReviewBrowser) canReload() bool {
	return model.load != nil || model.loadPrune != nil
}

func (model historyReviewBrowser) footer() []string {
	state := fmt.Sprintf("%d–%d / %d", min(len(model.data.entries), model.view.YOffset()+1), min(len(model.data.entries), model.view.YOffset()+model.view.Height()), len(model.data.entries))
	if model.expanded {
		state = fmt.Sprintf("%d / %d", min(len(model.data.entries), model.selected+1), len(model.data.entries))
	}
	if model.prune {
		state += " · " + T("history.review.checked", len(model.selectedIDs()))
		if model.dryRun {
			state += " · " + T("history.review.dry_run")
		}
	}
	if model.paused {
		state += " · " + T("history.top_browser.paused")
	}
	lines := []string{historyTint(state, richGray, model.color)}
	lines = append(lines, model.helpLines()...)
	if model.notice != "" && len(lines) < model.height-6 {
		lines = append(lines, historyTint(historyTerminalText(model.notice), richYellow, model.color))
	}
	return lines
}

func (model historyReviewBrowser) columnWidths() ([]int, []int) {
	indexes, widths := []int{0}, []int{0}
	reserved := 2
	if model.prune {
		reserved += 4
	}
	for i, column := range model.columns {
		if i == 0 || model.tableWidth() < column.minimum {
			continue
		}
		if reserved+column.width+2+12 > model.tableWidth() {
			continue
		}
		indexes = append(indexes, i)
		widths = append(widths, column.width)
		reserved += column.width + 2
	}
	widths[0] = max(1, model.tableWidth()-reserved)
	return indexes, widths
}

func (model historyReviewBrowser) tableLine(index int, indexes, widths []int) string {
	var cells []string
	for j, column := range indexes {
		text := model.columns[column].title
		if index >= 0 {
			text = model.data.entries[index].cells[column]
		}
		cells = append(cells, historyCell(historyTerminalText(text), widths[j], j > 0))
	}
	line := strings.Join(cells, "  ")
	if model.prune {
		mark := "    "
		if index >= 0 {
			mark = "[ ] "
			if model.checked[model.data.entries[index].id] {
				mark = "[x] "
			}
		}
		line = mark + line
	}
	if index >= 0 && index == model.selected {
		return historySelect(line, model.color)
	}
	return "  " + line
}

func (model historyReviewBrowser) detailLines(width int) []string {
	if len(model.data.entries) == 0 {
		return nil
	}
	return historyDetailWrappedLines(model.data.entries[model.selected].details, width, model.color)
}

func (model *historyReviewBrowser) resize() {
	model.selected = min(max(0, model.selected), max(0, len(model.data.entries)-1))
	model.view.SetWidth(model.tableWidth())
	model.view.SetHeight(max(1, model.height-5-len(model.footer())))
	var lines []string
	if model.expanded {
		lines = model.detailLines(model.tableWidth())
	} else {
		indexes, widths := model.columnWidths()
		for i := range model.data.entries {
			lines = append(lines, model.tableLine(i, indexes, widths))
		}
	}
	model.view.SetContent(strings.Join(lines, "\n"))
	if !model.expanded {
		keepHistorySelection(&model.view, model.selected)
	}
}

func (model historyReviewBrowser) Init() tea.Cmd {
	if !model.canReload() || model.prune {
		return nil
	}
	return model.tick()
}

func (model historyReviewBrowser) tick() tea.Cmd {
	seq := model.seq
	return tea.Tick(logHistoryRefresh, func(time.Time) tea.Msg { return historyReviewTickMsg{seq: seq} })
}

func (model historyReviewBrowser) reload() tea.Cmd {
	seq, load, loadPrune, options := model.seq, model.load, model.loadPrune, model.pruneOptions
	return func() tea.Msg {
		if loadPrune != nil {
			data, err := loadPrune(options)
			return historyReviewLoadedMsg{seq: seq, data: data, err: err}
		}
		data, err := load()
		return historyReviewLoadedMsg{seq: seq, data: data, err: err}
	}
}

func (model historyReviewBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.resize()
	case historyReviewTickMsg:
		if value.seq == model.seq && !model.paused && !model.confirm && model.canReload() {
			return model, model.reload()
		}
	case historyReviewLoadedMsg:
		if value.seq != model.seq || model.confirm {
			return model, nil
		}
		if value.err != nil {
			model.notice = value.err.Error()
			if model.prune && len(model.data.entries) == 0 {
				model.data.summary = T("history.review.age_current", historyPruneAgeLabel(model.pruneOptions.olderThan))
				model.data.empty = T("history.error.read", value.err)
			}
		} else {
			id := ""
			if len(model.data.entries) > 0 {
				id = model.data.entries[model.selected].id
			}
			checked := map[string]bool{}
			for _, entry := range model.data.entries {
				checked[entry.id] = true
			}
			model.data, model.notice = value.data, ""
			model.selected, model.expanded = 0, model.expanded && id != ""
			found := false
			for i, entry := range model.data.entries {
				if entry.id == id {
					model.selected, found = i, true
				}
				if model.prune && !checked[entry.id] {
					model.checked[entry.id] = true
				}
			}
			if !found {
				model.expanded = false
			}
		}
		model.resize()
		if !model.prune && !model.paused && model.canReload() {
			return model, model.tick()
		}
	case tea.PasteMsg:
		if model.agePicker != nil && model.agePicker.input {
			model.agePicker.text = pruneAgeInputText(model.agePicker.text + value.Content)
			model.agePicker.err = ""
		}
	case tea.KeyPressMsg:
		if model.agePicker != nil {
			return model.updatePruneAge(value)
		}
		if value.String() == "ctrl+c" || value.String() == "q" {
			return model, tea.Quit
		}
		if model.confirm {
			if value.String() == "y" && !model.dryRun {
				model.confirmed = true
				return model, tea.Quit
			}
			model.confirm = false
			model.resize()
			return model, nil
		}
		model.notice = ""
		switch value.String() {
		case "t":
			if model.prune {
				model.openPruneAgePicker()
				return model, nil
			}
		case "esc":
			if !model.expanded {
				return model, tea.Quit
			}
			model.expanded = false
			model.resize()
			return model, nil
		case "r", "p":
			if value.String() == "p" && model.prune {
				return model, nil
			}
			if model.canReload() {
				model.seq++
				if value.String() == "p" {
					model.paused = !model.paused
					if model.paused {
						return model, nil
					}
				}
				return model, model.reload()
			}
		case "d":
			if model.prune && !model.dryRun && !model.expanded && len(model.selectedIDs()) > 0 {
				model.seq++
				model.confirm = true
				model.resize()
				return model, nil
			}
		case "space", " ":
			if model.prune && !model.expanded && len(model.data.entries) > 0 {
				id := model.data.entries[model.selected].id
				model.checked[id] = !model.checked[id]
			}
		case "a":
			if model.prune && !model.expanded {
				on := len(model.selectedIDs()) != len(model.data.entries)
				for _, entry := range model.data.entries {
					model.checked[entry.id] = on
				}
			}
		}
		if model.expanded {
			var cmd tea.Cmd
			model.view, cmd = model.view.Update(msg)
			return model, cmd
		}
		switch value.String() {
		case "up", "k":
			model.selected = max(0, model.selected-1)
		case "down", "j":
			model.selected = min(max(0, len(model.data.entries)-1), model.selected+1)
		case "pgup":
			model.selected = max(0, model.selected-model.view.Height())
		case "pgdown":
			model.selected = min(max(0, len(model.data.entries)-1), model.selected+model.view.Height())
		case "home":
			model.selected = 0
		case "end":
			model.selected = max(0, len(model.data.entries)-1)
		case "enter":
			if len(model.data.entries) > 0 {
				model.expanded = true
				model.view.GotoTop()
			}
		}
		model.resize()
	}
	return model, nil
}

func (model historyReviewBrowser) View() tea.View {
	if model.agePicker != nil {
		return model.pruneAgeView()
	}
	if model.width < 40 || model.height < 12 {
		view := tea.NewView(ansi.Truncate(T("cli.log_history.small"), max(1, model.width), "…"))
		view.AltScreen = true
		return view
	}
	lines := []string{historyStrong(model.title, richCyan, model.color), historyTint(historyTerminalText(model.path), richGray, model.color), historyTint(historyTerminalText(model.data.summary), richGray, model.color), ""}
	header := ""
	if !model.expanded {
		indexes, widths := model.columnWidths()
		header = model.tableLine(-1, indexes, widths)
		if model.split() {
			detailsTitle := model.detailsTitle
			if detailsTitle == "" {
				detailsTitle = T("history.top_browser.details")
			}
			header = historyCell(header, model.tableWidth(), false) + " │ " + detailsTitle
		}
	}
	lines = append(lines, historyTint(header, richGray, model.color))
	body := strings.Split(model.view.View(), "\n")
	if len(model.data.entries) == 0 {
		copy(body, strings.Split(ansi.Wrap(historyTerminalText(model.data.empty), model.tableWidth(), ""), "\n"))
	}
	if model.split() {
		details := model.detailLines(model.width - model.tableWidth() - 3)
		for i := range body {
			right := ""
			if i < len(details) {
				right = details[i]
			}
			body[i] = historyCell(body[i], model.tableWidth(), false) + historyTint(" │ ", richGray, model.color) + right
		}
	}
	lines = append(lines, body...)
	footer := model.footer()
	for len(lines)+len(footer) < model.height {
		lines = append(lines, "")
	}
	lines = append(lines, footer...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, max(1, model.width), "…")
	}
	view := tea.NewView(strings.Join(lines[:min(len(lines), model.height)], "\n"))
	view.AltScreen = true
	return view
}
