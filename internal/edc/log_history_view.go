package edc

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func historyTint(text, code string, color bool) string {
	if !color {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func historyCell(text string, width int, right bool) string {
	text = ansi.Truncate(text, max(1, width), "…")
	padding := strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	if right {
		return padding + text
	}
	return text + padding
}

func historyRelativeTime(at, now time.Time) string {
	if at.IsZero() {
		return "—"
	}
	at, now = at.Local(), now.Local()
	day := at.Format("2006-01-02")
	if day == now.Format("2006-01-02") {
		return T("cli.log_history.today", at.Format("15:04"))
	}
	if day == now.AddDate(0, 0, -1).Format("2006-01-02") {
		return T("cli.log_history.yesterday", at.Format("15:04"))
	}
	if at.Year() == now.Year() {
		return at.Format("Jan 02 15:04")
	}
	return at.Format("2006-01-02")
}

type historyKeyColumns struct{ command, last, runs, failed, unknown int }

func historyKeyWidths(width int, groups []logHistoryGroup) historyKeyColumns {
	columns := historyKeyColumns{runs: 6, failed: 7}
	if width >= 64 {
		columns.last = 17
	}
	for _, group := range groups {
		if group.Unknown > 0 {
			columns.unknown = 8
			break
		}
	}
	reserved := 2 + columns.runs + columns.failed + 4
	if columns.last > 0 {
		reserved += columns.last + 2
	}
	if columns.unknown > 0 {
		reserved += columns.unknown + 2
	}
	columns.command = max(1, width-reserved)
	return columns
}

func historyKeyHeader(width int, groups []logHistoryGroup) string {
	columns := historyKeyWidths(width, groups)
	cells := []string{historyCell(T("cli.log_history.column.command"), columns.command, false)}
	if columns.last > 0 {
		cells = append(cells, historyCell(T("cli.log_history.column.last"), columns.last, false))
	}
	cells = append(cells, historyCell(T("cli.log_history.column.runs"), columns.runs, true), historyCell(T("cli.log_history.column.failed"), columns.failed, true))
	if columns.unknown > 0 {
		cells = append(cells, historyCell(T("cli.log_history.column.unknown"), columns.unknown, true))
	}
	return "  " + strings.Join(cells, "  ")
}

func historyKeyRow(group logHistoryGroup, width int, groups []logHistoryGroup, selected, color bool, now time.Time) string {
	return historyKeyRowColumns(group, historyKeyWidths(width, groups), selected, color, now)
}

func historyKeyRowColumns(group logHistoryGroup, columns historyKeyColumns, selected, color bool, now time.Time) string {
	marker := "  "
	if selected {
		marker = "▸ "
	}
	command := historyCell(historyCommand(group.Representative), columns.command, false)
	if selected {
		command = historyTint(command, "38;5;81;1", color)
	}
	cells := []string{command}
	if columns.last > 0 {
		cells = append(cells, historyTint(historyCell(historyRelativeTime(group.Rows[0].Started, now), columns.last, false), "38;5;246", color))
	}
	cells = append(cells, historyTint(historyCell(fmt.Sprint(len(group.Rows)), columns.runs, true), "38;5;246", color))
	failed := historyCell(fmt.Sprint(group.Failed), columns.failed, true)
	code := "38;5;246"
	if group.Failed > 0 {
		code = "31;1"
	}
	cells = append(cells, historyTint(failed, code, color))
	if columns.unknown > 0 {
		cells = append(cells, historyTint(historyCell(fmt.Sprint(group.Unknown), columns.unknown, true), "33", color))
	}
	line := marker + strings.Join(cells, "  ")
	if selected && color {
		base := "\x1b[48;5;236m"
		line = base + strings.ReplaceAll(line, "\x1b[0m", base) + "\x1b[0m"
	}
	return line
}

func historyRunHeader(width int) string {
	timeWidth := 24
	if width < 64 {
		timeWidth = 16
	}
	durationWidth := min(12, max(6, width-timeWidth-2-10-2-4-2-2))
	if width < 48 {
		return "  " + historyCell(T("cli.log_history.column.started"), timeWidth, false) + "  " + historyCell(T("cli.log_history.column.duration"), max(1, width-timeWidth-16), true) + "  " + historyCell(T("cli.log_history.column.result"), 10, false)
	}
	return "  " + historyCell(T("cli.log_history.column.started"), timeWidth, false) + "  " + historyCell(T("cli.log_history.column.duration"), durationWidth, true) + "  " + historyCell(T("cli.log_history.column.result"), 10, false) + "  " + historyCell("exit", 4, true)
}

func historyRunRow(row logHistoryAttempt, width int, selected, color bool) string {
	timeWidth := 24
	if width < 64 {
		timeWidth = 16
	}
	durationWidth := min(12, max(6, width-timeWidth-2-10-2-4-2-2))
	at := row.Started.Local().Format("2006-01-02 15:04:05")
	if row.Started.IsZero() {
		at = "—"
	}
	exit := "—"
	if row.Exit != nil {
		exit = fmt.Sprint(*row.Exit)
	}
	code := "33"
	switch row.Outcome {
	case "SUCCESS":
		code = "32"
	case "FAIL", "ERROR", "SIGNAL":
		code = "31;1"
	}
	marker := "  "
	if selected {
		marker = historyTint("▸ ", "38;5;81;1", color)
	}
	if width < 48 {
		return marker + historyTint(historyCell(at, timeWidth, false), "38;5;246", color) + "  " + historyCell(historyDuration(row), max(1, width-timeWidth-16), true) + "  " + historyTint(historyCell(historyOutcome(row), 10, false), code, color)
	}
	return marker + historyTint(historyCell(at, timeWidth, false), "38;5;246", color) + "  " + historyCell(historyDuration(row), durationWidth, true) + "  " + historyTint(historyCell(historyOutcome(row), 10, false), code, color) + "  " + historyTint(historyCell(exit, 4, true), "38;5;246", color)
}

func historyCompactSummary(rows []logHistoryAttempt, color bool) []string {
	type context struct{ rows []logHistoryAttempt }
	contexts := map[string]*context{}
	var order []string
	for _, row := range rows {
		if contexts[row.CWD] == nil {
			contexts[row.CWD] = &context{}
			order = append(order, row.CWD)
		}
		contexts[row.CWD].rows = append(contexts[row.CWD].rows, row)
	}
	var lines []string
	for _, cwd := range order {
		var total big.Int
		success, failed, unknown := 0, 0, 0
		for _, row := range contexts[cwd].rows {
			if row.Outcome == "SUCCESS" {
				success++
				total.Add(&total, big.NewInt(int64(row.Duration)))
			} else if row.failed() {
				failed++
			} else {
				unknown++
			}
		}
		parts := []string{historyTint(T("cli.log_history.summary_pass", success), "32", color)}
		if failed > 0 {
			parts = append(parts, historyTint(T("cli.log_history.summary_fail", failed), "31", color))
		}
		if unknown > 0 {
			parts = append(parts, historyTint(T("cli.log_history.summary_unknown", unknown), "33", color))
		}
		if success > 0 {
			mean := time.Duration(new(big.Int).Quo(&total, big.NewInt(int64(success))).Int64())
			parts = append(parts, T("cli.log_history.summary_avg", mean.String()))
		}
		if len(order) > 1 {
			lines = append(lines, historyTint("cwd "+reportIdentityValue(cwd), "38;5;246", color))
		}
		lines = append(lines, strings.Join(parts, " · "))
	}
	return lines
}
