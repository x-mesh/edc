package edc

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// historyTint는 색을 쓸 때만 칠한다. code는 richGreen 같은 팔레트 번호다.
func historyTint(text, code string, color bool) string {
	if !color {
		return text
	}
	return richPaint(text, code, false)
}

func historyStrong(text, code string, color bool) string {
	if !color {
		return text
	}
	return richPaint(text, code, true)
}

// historySelect는 고른 줄이다. 색을 쓰면 막대와 chip 바탕으로, 아니면 "▸ "로 표시한다.
func historySelect(cells string, color bool) string {
	if !color {
		return "▸ " + cells
	}
	return liveSelected(liveSelectedBar+" "+cells, true)
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

// historyGroupSlow reports a running attempt that already took twice as long as
// the usual successful run of its command.
func historyGroupSlow(group logHistoryGroup) bool {
	for _, row := range group.Rows {
		if row.Outcome == historyOutcomeRunning && historyBaseline(group.Rows, row).duration >= historyRatioAlert {
			return true
		}
	}
	return false
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
	command := historyCell(historyCommand(group.Representative), columns.command, false)
	cells := []string{command}
	if columns.last > 0 && group.Running > 0 {
		label, code := T("cli.log_history.running_now"), richCyan
		if historyGroupSlow(group) {
			label, code = T("cli.log_history.running_slow"), richYellow
		}
		cells = append(cells, historyStrong(historyCell(label, columns.last, false), code, color))
	} else if columns.last > 0 {
		cells = append(cells, historyTint(historyCell(historyRelativeTime(group.Rows[0].Started, now), columns.last, false), richGray, color))
	}
	cells = append(cells, historyTint(historyCell(fmt.Sprint(len(group.Rows)), columns.runs, true), richGray, color))
	failed := historyCell(fmt.Sprint(group.Failed), columns.failed, true)
	if group.Failed > 0 {
		failed = historyStrong(failed, richRed, color)
	} else {
		failed = historyTint(failed, richGray, color)
	}
	cells = append(cells, failed)
	if columns.unknown > 0 {
		cells = append(cells, historyTint(historyCell(fmt.Sprint(group.Unknown), columns.unknown, true), richYellow, color))
	}
	if selected {
		return historySelect(strings.Join(cells, "  "), color)
	}
	return "  " + strings.Join(cells, "  ")
}

// historyRunColumns are the run list widths. Resource columns appear as the
// width allows: memory first, then CPU time and disk bytes, which need the
// start time and duration columns trimmed to their content.
type historyRunColumns struct{ time, duration, memory, cpu, disk int }

func historyRunWidths(width int) historyRunColumns {
	columns := historyRunColumns{time: 24}
	if width < 64 {
		columns.time = 16
	}
	columns.duration = min(12, max(6, width-columns.time-2-10-2-4-2-2))
	switch {
	case width >= 84:
		columns = historyRunColumns{time: 19, duration: 10, memory: 9, cpu: 8, disk: 9}
	case width >= 72:
		columns.memory = 9
	}
	return columns
}

func historyRunHeader(width int) string {
	columns := historyRunWidths(width)
	if width < 48 {
		return "  " + historyCell(T("cli.log_history.column.started"), columns.time, false) + "  " + historyCell(T("cli.log_history.column.duration"), max(1, width-columns.time-16), true) + "  " + historyCell(T("cli.log_history.column.result"), 10, false)
	}
	cells := []string{historyCell(T("cli.log_history.column.started"), columns.time, false), historyCell(T("cli.log_history.column.duration"), columns.duration, true)}
	for _, column := range []struct {
		key   string
		width int
	}{{"memory", columns.memory}, {"cpu", columns.cpu}, {"disk", columns.disk}} {
		if column.width > 0 {
			cells = append(cells, historyCell(T("cli.log_history.column."+column.key), column.width, true))
		}
	}
	cells = append(cells, historyCell(T("cli.log_history.column.result"), 10, false), historyCell("exit", 4, true))
	return "  " + strings.Join(cells, "  ")
}

// historyShortDuration fits a CPU time into the run list column.
func historyShortDuration(value time.Duration) string {
	switch {
	case value < time.Second:
		return fmt.Sprintf("%dms", value.Milliseconds())
	case value < time.Minute:
		return strconv.FormatFloat(value.Seconds(), 'f', 1, 64) + "s"
	case value < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(value.Minutes()), int(value.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(value.Hours()), int(value.Minutes())%60)
	}
}

func historyRunRow(row logHistoryAttempt, width int, selected, color bool, baseline historyRatios) string {
	columns := historyRunWidths(width)
	at := row.Started.Local().Format("2006-01-02 15:04:05")
	if row.Started.IsZero() {
		at = "—"
	}
	exit := "—"
	if row.Exit != nil {
		exit = fmt.Sprint(*row.Exit)
	}
	outcome := historyCell(historyOutcome(row), 10, false)
	switch {
	case row.Outcome == historyOutcomeSuccess:
		outcome = historyTint(outcome, richGreen, color)
	case row.Outcome == historyOutcomeRunning:
		outcome = historyStrong(outcome, richCyan, color)
	case row.failed():
		outcome = historyStrong(outcome, richRed, color)
	default:
		outcome = historyTint(outcome, richYellow, color)
	}
	resource := func(text string, width int, ratio float64) string {
		if text == "" {
			text = "—"
		}
		if ratio >= historyRatioAlert {
			return historyStrong(historyCell(text, width, true), richYellow, color)
		}
		return historyCell(text, width, true)
	}
	if width < 48 {
		cells := historyTint(historyCell(at, columns.time, false), richGray, color) + "  " + resource(historyDuration(row), max(1, width-columns.time-16), baseline.duration) + "  " + outcome
		if selected {
			return historySelect(cells, color)
		}
		return "  " + cells
	}
	resources := row.Resources
	if resources == nil {
		resources = &logHistoryResources{}
	}
	cells := []string{historyTint(historyCell(at, columns.time, false), richGray, color), resource(historyDuration(row), columns.duration, baseline.duration)}
	if columns.memory > 0 {
		text := ""
		if resources.memory() > 0 {
			text = formatSizeShort(resources.memory())
		}
		cells = append(cells, resource(text, columns.memory, baseline.memory))
	}
	if columns.cpu > 0 {
		text := ""
		if resources.cpu {
			text = historyShortDuration(resources.CPU)
		}
		cells = append(cells, resource(text, columns.cpu, baseline.cpu))
	}
	if columns.disk > 0 {
		text := ""
		if resources.disk {
			text = formatSizeShort(resources.DiskRead + resources.DiskWrite)
		}
		cells = append(cells, resource(text, columns.disk, 0))
	}
	cells = append(cells, outcome, historyTint(historyCell(exit, 4, true), richGray, color))
	if selected {
		return historySelect(strings.Join(cells, "  "), color)
	}
	return "  " + strings.Join(cells, "  ")
}

// historyRatioAlert marks a run that used twice its usual memory or CPU time.
const historyRatioAlert = 2

// historyRatios compares a run with the median of earlier successful runs of the
// same key in the same working directory. Memory and CPU compare only runs
// measured the same way: a whole container or a single process is not
// comparable with a process tree. A zero ratio means too few runs.
type historyRatios struct{ duration, memory, cpu float64 }

// historyBaseline compares a running attempt by its elapsed time only, so a run
// that takes twice as long as usual is marked before it ends.
func historyBaseline(rows []logHistoryAttempt, row logHistoryAttempt) historyRatios {
	const minimum, window = 3, 20
	running := row.Outcome == historyOutcomeRunning
	var durations, memory, cpu []float64
	for _, other := range rows {
		if !other.Started.Before(row.Started) || other.CWD != row.CWD || other.Outcome != historyOutcomeSuccess {
			continue
		}
		if !other.Ended.IsZero() && !other.damaged && other.Duration > 0 && len(durations) < window {
			durations = append(durations, float64(other.Duration))
		}
		if running || row.Resources == nil || other.Resources == nil || other.Resources.Scope != row.Resources.Scope {
			continue
		}
		if value := other.Resources.memory(); value > 0 && len(memory) < window {
			memory = append(memory, float64(value))
		}
		if other.Resources.cpu && other.Resources.CPU > 0 && len(cpu) < window {
			cpu = append(cpu, float64(other.Resources.CPU))
		}
	}
	median := func(values []float64) float64 {
		if len(values) < minimum {
			return 0
		}
		sort.Float64s(values)
		middle := len(values) / 2
		if len(values)%2 == 0 {
			return (values[middle-1] + values[middle]) / 2
		}
		return values[middle]
	}
	var ratios historyRatios
	if usual := median(durations); usual > 0 {
		switch {
		case running:
			ratios.duration = float64(time.Since(row.Started)) / usual
		case !row.Ended.IsZero() && !row.damaged:
			ratios.duration = float64(row.Duration) / usual
		}
	}
	if running || row.Resources == nil {
		return ratios
	}
	if usual := median(memory); usual > 0 && row.Resources.memory() > 0 {
		ratios.memory = float64(row.Resources.memory()) / usual
	}
	if usual := median(cpu); usual > 0 && row.Resources.cpu {
		ratios.cpu = float64(row.Resources.CPU) / usual
	}
	return ratios
}

// alerts names the measurements at historyRatioAlert or above, in a fixed order
// so scripts reading --follow can match them.
func (ratios historyRatios) alerts() []string {
	var names []string
	for _, entry := range []struct {
		name  string
		ratio float64
	}{{"duration", ratios.duration}, {"memory", ratios.memory}, {"cpu", ratios.cpu}} {
		if entry.ratio >= historyRatioAlert {
			names = append(names, entry.name)
		}
	}
	return names
}

func (ratios historyRatios) alertText() string {
	values := map[string]float64{"duration": ratios.duration, "memory": ratios.memory, "cpu": ratios.cpu}
	var parts []string
	for _, name := range ratios.alerts() {
		parts = append(parts, T("cli.log_history.alert."+name)+" "+strconv.FormatFloat(values[name], 'f', 1, 64)+"×")
	}
	return strings.Join(parts, " · ")
}

func historyRatioText(ratio float64) string {
	if ratio <= 0 {
		return ""
	}
	return T("cli.log_history.resources.ratio", strconv.FormatFloat(ratio, 'f', 1, 64))
}

// historyResourceLines describes a run's resources for the details view and
// the piped run list.
func historyResourceLines(row logHistoryAttempt, ratios historyRatios) []string {
	resources := row.Resources
	if resources == nil {
		return nil
	}
	ratio := func(parts []string, value float64) []string {
		if text := historyRatioText(value); text != "" {
			parts = append(parts, text)
		}
		return parts
	}
	var lines []string
	if resources.cpu {
		parts := []string{T("cli.log_history.resources.cpu", resources.CPU.Round(time.Millisecond).String())}
		if resources.CPUUser > 0 || resources.CPUSys > 0 {
			parts = append(parts, T("cli.log_history.resources.cpu_split", resources.CPUUser.Round(time.Millisecond).String(), resources.CPUSys.Round(time.Millisecond).String()))
		}
		if !row.Ended.IsZero() && row.Duration > 0 {
			parts = append(parts, T("cli.log_history.resources.average", strconv.FormatFloat(float64(resources.CPU)/float64(row.Duration)*100, 'f', 0, 64)+"%"))
		}
		if resources.Samples > 1 {
			parts = append(parts, T("cli.log_history.resources.peak", strconv.FormatFloat(resources.CPUPeak, 'f', 0, 64)+"%"))
		}
		lines = append(lines, strings.Join(ratio(parts, ratios.cpu), " · "))
	}
	var memory []string
	if resources.Samples > 0 {
		memory = append(memory, T("cli.log_history.resources.memory", formatSizeShort(resources.MemPeak)), T("cli.log_history.resources.average", formatSizeShort(resources.MemAvg)))
	}
	if resources.MaxRSS > 0 {
		memory = append(memory, T("cli.log_history.resources.max_rss", formatSizeShort(resources.MaxRSS)))
	}
	if len(memory) > 0 {
		lines = append(lines, strings.Join(ratio(memory, ratios.memory), " · "))
	}
	if resources.disk {
		lines = append(lines, T("cli.log_history.resources.disk", formatSizeShort(resources.DiskRead), formatSizeShort(resources.DiskWrite)))
	}
	switch {
	case resources.Samples > 0 && resources.Scope == logResourceScopeGroup:
		lines = append(lines, T("cli.log_history.resources.scope_pgrp", resources.Samples))
	case resources.Samples > 0:
		lines = append(lines, T("cli.log_history.resources.scope_"+resources.Scope, resources.Container, resources.Samples))
	}
	return lines
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
		success, failed, unknown, running := 0, 0, 0, 0
		for _, row := range contexts[cwd].rows {
			if row.Outcome == historyOutcomeSuccess {
				success++
				total.Add(&total, big.NewInt(int64(row.Duration)))
			} else if row.Outcome == historyOutcomeRunning {
				running++
			} else if row.failed() {
				failed++
			} else {
				unknown++
			}
		}
		parts := []string{historyTint(T("cli.log_history.summary_pass", success), richGreen, color)}
		if running > 0 {
			parts = append(parts, historyTint(T("cli.log_history.summary_running", running), richCyan, color))
		}
		if failed > 0 {
			parts = append(parts, historyTint(T("cli.log_history.summary_fail", failed), richRed, color))
		}
		if unknown > 0 {
			parts = append(parts, historyTint(T("cli.log_history.summary_unknown", unknown), richYellow, color))
		}
		if success > 0 {
			mean := time.Duration(new(big.Int).Quo(&total, big.NewInt(int64(success))).Int64())
			parts = append(parts, T("cli.log_history.summary_avg", mean.String()))
		}
		if len(order) > 1 {
			lines = append(lines, historyTint("cwd "+reportIdentityValue(cwd), richGray, color))
		}
		separator := " · "
		if color {
			separator = richDim(separator)
		}
		lines = append(lines, strings.Join(parts, separator))
	}
	return lines
}
