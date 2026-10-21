package edc

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type historyOverviewBrowser struct {
	db                    *sql.DB
	path                  string
	options               historyOverviewOptions
	overview              historyOverview
	width, height, offset int
	color                 bool
	err                   error
	hosted                bool
}

func newHistoryOverviewBrowser(db *sql.DB, path string, options historyOverviewOptions) historyOverviewBrowser {
	model := historyOverviewBrowser{db: db, path: path, options: options, width: 80, height: 24, color: os.Getenv("NO_COLOR") == ""}
	model.overview, model.err = loadHistoryOverview(db, options)
	return model
}

func (model historyOverviewBrowser) atTop() bool   { return model.offset == 0 }
func (model historyOverviewBrowser) Init() tea.Cmd { return nil }

func (model historyOverviewBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "ctrl+c":
			return model, tea.Quit
		case "esc":
			if !model.hosted {
				return model, tea.Quit
			}
		case "r":
			model.overview, model.err = loadHistoryOverview(model.db, model.options)
			model.offset = 0
		case "up", "k":
			model.offset = max(0, model.offset-1)
		case "down", "j":
			model.offset++
		case "pgup":
			model.offset = max(0, model.offset-max(1, model.height-4))
		case "pgdown":
			model.offset += max(1, model.height-4)
		case "home":
			model.offset = 0
		case "end":
			model.offset = 1 << 30
		}
	}
	return model, nil
}

func (model historyOverviewBrowser) lines() []string {
	if model.err != nil {
		return []string{model.err.Error()}
	}
	title := T("history.overview.browser.title")
	if model.overview.Run != nil {
		run := model.overview.Run
		duration := "—"
		if run.EndedAt != nil {
			duration = historyShortDuration(run.EndedAt.Sub(run.StartedAt))
		}
		lines := []string{title, fmt.Sprintf(T("history.overview.browser.run"), run.ID, historyTerminalText(run.Command), historyTerminalText(run.Hostname)), fmt.Sprintf(T("history.overview.browser.run_detail"), duration, run.IntervalS, historyTerminalText(emptyAs(run.Process, "—")))}
		return model.tableLines(lines)
	}
	lines := []string{title, fmt.Sprintf(T("history.overview.browser.window"), model.overview.From.Local().Format(time.DateTime), model.overview.To.Local().Format(time.DateTime), model.overview.Runs, model.overview.Samples)}
	return model.tableLines(lines)
}

func (model historyOverviewBrowser) tableLines(lines []string) []string {
	metricWidth := 24
	lines = append(lines, "", T("history.overview.metrics"), historyOverviewMetricRow(T("history.overview.browser.metric"), strings.ToUpper(T("history.overview.avg")), "P95", strings.ToUpper(T("history.overview.max")), "", metricWidth))
	for _, metric := range model.overview.Metrics {
		lines = append(lines, historyOverviewMetricRow(historyOverviewMetricLabel(metric.Metric), historyTrendValue(metric.Metric, metric.Avg), historyTrendValue(metric.Metric, metric.P95), historyTrendValue(metric.Metric, metric.Max), historyOverviewSparkline(model.overview.Series[metric.Metric], 12), metricWidth))
	}
	nameWidth := max(16, model.width-45)
	lines = append(lines, "", T("history.overview.processes", historyOverviewMemoryLabel(model.overview.Memory)), historyOverviewProcessRow(T("history.overview.browser.process"), strings.ToUpper(T("history.overview.peak")), strings.ToUpper(T("history.overview.cpu")), strings.ToUpper(T("history.overview.restarts")), nameWidth))
	for _, process := range model.overview.Processes {
		lines = append(lines, historyOverviewProcessRow(topProcessName(process.Command, nameWidth), formatSizeShort(uint64(process.Peak)), fmt.Sprintf("%.1f%%", process.CPU), fmt.Sprint(process.Restarts), nameWidth))
	}
	if len(model.overview.Disks) > 0 {
		lines = append(lines, "", T("history.overview.disks"), model.diskHeader())
		for _, disk := range model.overview.Disks {
			growth := "—"
			if disk.BytesPerDay != nil {
				growth = historyGrowthRate(*disk.BytesPerDay)
			}
			lines = append(lines, model.diskRow(disk.Mount, formatSizeShort(uint64(disk.UsedBytes)), formatSizeShort(uint64(disk.TotalBytes)), growth))
		}
	}
	if len(model.overview.Logs) > 0 {
		lines = append(lines, "", T("history.overview.logs"), T("history.overview.logs_time_only"), model.logHeader())
		for _, log := range model.overview.Logs {
			lines = append(lines, model.logRow(log.Command, log.Started.Local().Format(time.DateTime), log.Outcome, historyShortDuration(log.Duration)))
		}
	}
	return lines
}

func historyOverviewMetricRow(name, average, p95, maximum, spark string, nameWidth int) string {
	return historyCell(name, nameWidth, false) + " " + historyCell(average, 10, true) + " " + historyCell(p95, 10, true) + " " + historyCell(maximum, 10, true) + " " + spark
}

func historyOverviewProcessRow(name, peak, cpu, restarts string, nameWidth int) string {
	return historyCell(name, nameWidth, false) + " " + historyCell(peak, 10, true) + " " + historyCell(cpu, 10, true) + " " + historyCell(restarts, 8, true)
}

func (model historyOverviewBrowser) diskHeader() string {
	pathWidth := max(16, model.width-48)
	return historyCell(T("history.overview.column.path"), pathWidth, false) + " " + historyCell(T("history.overview.column.used"), 10, true) + " " + historyCell(T("history.overview.column.total"), 10, true) + " " + historyCell(T("history.overview.column.growth_per_day"), 14, true)
}

func (model historyOverviewBrowser) diskRow(path, used, total, growth string) string {
	pathWidth := max(16, model.width-48)
	return historyCell(path, pathWidth, false) + " " + historyCell(used, 10, true) + " " + historyCell(total, 10, true) + " " + historyCell(growth, 14, true)
}

func (model historyOverviewBrowser) logHeader() string {
	commandWidth := max(16, model.width-55)
	return historyCell(T("history.overview.column.command"), commandWidth, false) + " " + historyCell(T("history.overview.column.started"), 19, false) + " " + historyCell(T("history.overview.column.result"), 12, false) + " " + historyCell(T("history.overview.column.duration"), 10, true)
}

func (model historyOverviewBrowser) logRow(command, started, result, duration string) string {
	commandWidth := max(16, model.width-55)
	return historyCell(command, commandWidth, false) + " " + historyCell(started, 19, false) + " " + historyCell(result, 12, false) + " " + historyCell(duration, 10, true)
}

func historyOverviewSparkline(values []float64, width int) string {
	if len(values) == 0 || width <= 0 {
		return ""
	}
	start := max(0, len(values)-width)
	values = values[start:]
	low, high := values[0], values[0]
	for _, value := range values {
		low, high = math.Min(low, value), math.Max(high, value)
	}
	const levels = "▁▂▃▄▅▆▇█"
	var line strings.Builder
	for _, value := range values {
		index := 0
		if high > low {
			index = int(math.Round((value - low) / (high - low) * 7))
		}
		line.WriteRune([]rune(levels)[index])
	}
	return line.String()
}

func (model historyOverviewBrowser) View() tea.View {
	width := max(1, model.width)
	lines := model.lines()
	footer := historyReviewKeyHints(T("history.overview.browser.help"), width, model.color)
	room := max(1, model.height-len(footer))
	model.offset = min(model.offset, max(0, len(lines)-room))
	body := lines[model.offset:min(len(lines), model.offset+room)]
	body = append(body, footer...)
	for index := range body {
		body[index] = ansi.Truncate(body[index], width, "…")
	}
	view := tea.NewView(strings.Join(body, "\n"))
	view.AltScreen = true
	return view
}

func runHistoryOverviewBrowser(db *sql.DB, path string, options historyOverviewOptions) error {
	_, err := tea.NewProgram(newHistoryOverviewBrowser(db, path, options), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}
