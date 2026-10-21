package edc

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

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
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
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
	title := T("history.overview.metrics")
	if model.overview.Run != nil {
		title += " · " + historyTerminalText(model.overview.Run.Command)
	}
	lines := []string{title, fmt.Sprintf("%s → %s · %s %d · %s %d", model.overview.From.Local().Format("2006-01-02 15:04"), model.overview.To.Local().Format("2006-01-02 15:04"), T("history.overview.runs"), model.overview.Runs, T("history.overview.samples"), model.overview.Samples), "", T("history.overview.metrics"), fmt.Sprintf("%-24s %10s %10s %10s", "METRIC", strings.ToUpper(T("history.overview.avg")), "P95", strings.ToUpper(T("history.overview.max")))}
	for _, metric := range model.overview.Metrics {
		lines = append(lines, fmt.Sprintf("%-24s %10s %10s %10s", metric.Metric, historyTrendValue(metric.Metric, metric.Avg), historyTrendValue(metric.Metric, metric.P95), historyTrendValue(metric.Metric, metric.Max)))
	}
	lines = append(lines, "", T("history.overview.processes", T("history.overview."+model.overview.Memory)), fmt.Sprintf("%-30s %10s %10s %8s", "PROCESS", strings.ToUpper(T("history.overview.peak")), strings.ToUpper(T("history.overview.cpu")), strings.ToUpper(T("history.overview.restarts"))))
	for _, process := range model.overview.Processes {
		lines = append(lines, fmt.Sprintf("%-30s %10s %9.1f%% %8d", topProcessName(process.Command, 30), formatSizeShort(uint64(process.Peak)), process.CPU, process.Restarts))
	}
	return lines
}

func (model historyOverviewBrowser) View() tea.View {
	width := max(1, model.width)
	lines := model.lines()
	footer := liveKeyHints("↑↓ scroll · PgUp/PgDn page · r refresh · q quit", model.color)
	room := max(1, model.height-1)
	model.offset = min(model.offset, max(0, len(lines)-room))
	body := lines[model.offset:min(len(lines), model.offset+room)]
	body = append(body, footer)
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
