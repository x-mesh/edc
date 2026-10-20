package edc

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func (model logHistoryBrowser) splitDetail() bool {
	return model.detail && !model.expanded && model.width >= 132 && model.height >= 16
}

func (model logHistoryBrowser) tableWidth() int {
	if model.splitDetail() {
		return max(max(88, model.contentWidth()*3/5), min(116, model.contentWidth()-43))
	}
	return model.contentWidth()
}

func (model logHistoryBrowser) headerLines() []string {
	if !model.detail || len(model.groups) == 0 {
		return []string{
			historyStrong(T("cli.log_history.browser_title", len(model.groups)), richMagenta, model.color) + historyTint(model.liveState(), richGray, model.color),
			"",
			historyTint(historyKeyHeader(model.contentWidth(), model.groups), richGray, model.color),
		}
	}
	command := historyCommand(model.groups[model.cursor].Representative)
	wrapped := strings.Split(ansi.Wrap(command, model.contentWidth(), ""), "\n")
	if len(wrapped) > 2 {
		wrapped = wrapped[:2]
		wrapped[1] = ansi.Truncate(wrapped[1], max(1, model.contentWidth()-1), "") + "…"
	}
	var lines []string
	for _, line := range wrapped {
		lines = append(lines, historyStrong(line, richCyan, model.color))
	}
	if model.height >= 14 {
		lines = append(lines, historyTint(T("cli.log_history.run_count", len(model.rows()))+model.liveState(), richGray, model.color))
	}
	lines = append(lines, "")
	if !model.expanded {
		header := historyRunHeader(model.tableWidth())
		if model.splitDetail() {
			header = historyCell(header, model.tableWidth(), false) + " │ " + T("cli.log_history.selected_run")
		}
		lines = append(lines, historyTint(header, richGray, model.color))
	}
	return lines
}

func (model logHistoryBrowser) attemptLines(row logHistoryAttempt, width int) []string {
	ratios := historyBaseline(model.groups[model.cursor].Rows, row)
	timing := historyTime(row.Started) + "  ·  " + historyDuration(row)
	if ratio := historyRatioText(ratios.duration); ratio != "" {
		timing += "  ·  " + ratio
	}
	outcome := historyOutcome(row)
	if row.Exit != nil {
		outcome += fmt.Sprintf(" · exit %d", *row.Exit)
	}
	code := richYellow
	switch {
	case row.failed():
		code = richRed
	case row.Outcome == historyOutcomeSuccess:
		code = richGreen
	case row.Outcome == historyOutcomeRunning:
		code = richCyan
	}
	lines := []string{historyStrong(outcome, code, model.color), timing, T("cli.log_history.attempt", row.Attempt)}
	if resources := historyResourceLines(row, ratios); len(resources) > 0 {
		lines = append(append(lines, ""), resources...)
	}
	lines = append(lines, "", "cwd "+reportIdentityValue(row.CWD), "file "+reportIdentityValue(row.Path), "key "+row.Key, "", historyCommand(row))
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(line, width, ""), "\n")...)
	}
	return wrapped
}

func (model logHistoryBrowser) historyLines() []string {
	left := strings.Split(model.history.View(), "\n")
	rows := model.rows()
	if !model.splitDetail() || len(rows) == 0 {
		return left
	}
	right := model.attemptLines(rows[model.rowCursor], model.contentWidth()-model.tableWidth()-3)
	lines := make([]string, model.history.Height())
	for i := range lines {
		var a, b string
		if i < len(left) {
			a = left[i]
		}
		if i < len(right) {
			b = right[i]
		}
		lines[i] = historyCell(a, model.tableWidth(), false) + historyTint(" │ ", richGray, model.color) + b
	}
	return lines
}

func (model logHistoryBrowser) footerLines() []string {
	var lines []string
	if model.detail && !model.expanded && model.height >= 14 {
		lines = append(lines, "")
		lines = append(lines, model.summary()...)
	}
	var position string
	if model.detail && !model.expanded {
		count := len(model.rows())
		first := min(count, model.history.YOffset()+1)
		last := min(count, model.history.YOffset()+model.history.Height())
		position = fmt.Sprintf("%d–%d / %d", first, last, count)
	} else if !model.detail {
		count := len(model.groups)
		first := min(count, model.list.YOffset()+1)
		last := min(count, model.list.YOffset()+model.list.Height())
		position = fmt.Sprintf("%d–%d / %d", first, last, count)
	}
	if position != "" && model.height >= 14 {
		lines = append(lines, historyTint(historyCell(position, model.contentWidth(), true), richGray, model.color))
	}
	key := "cli.log_history.list_help"
	if model.detail {
		key = "cli.log_history.detail_help"
	}
	if model.expanded {
		key = "cli.log_history.expanded_help"
	}
	help := model.keyHints(key)
	if model.confirm != nil {
		help = model.deletionPrompt()
	}
	lines = append(lines, strings.Split(ansi.Wrap(help, model.contentWidth(), ""), "\n")...)
	if model.status != "" {
		lines = append(lines, historyTint(model.status, richYellow, model.color))
	}
	if len(model.notices) > 0 {
		lines = append(lines, historyTint(model.notices[0], richYellow, model.color))
		if len(model.notices) > 1 {
			lines = append(lines, historyTint(T("cli.log_history.more_notices", len(model.notices)-1), richYellow, model.color))
		}
	}
	lines = lines[:min(len(lines), max(1, model.height-len(model.headerLines())-1))]
	return lines
}
