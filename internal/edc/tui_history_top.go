package edc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type historyTopColumn struct {
	name, title string
	width       int
}

func historyTopMetricText(sample historyTopSample, name string) string {
	text := historyDisplayMetric(sample, name)
	if name == "cpu_pct" {
		return fmt.Sprintf("%.1f%%", sample.CPU)
	}
	if text == "-" {
		return "—"
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "—"
	}
	return historyTrendValue(name, value)
}

func historyTopColumns(width int, samples []historyTopSample) []historyTopColumn {
	columns := []historyTopColumn{{"time", "TIME", 19}, {"cpu_pct", "CPU%", 6}, {"memory_pct", "MEM%", 6}, {"load1", "LOAD", 6}}
	if width < 48 {
		columns[0].width = 8
	}
	used := 2
	for _, column := range columns {
		used += column.width + 2
	}
	groups := [][]historyTopColumn{
		{{"disk_read_bytes_per_s", "DISK R/s", 10}, {"disk_write_bytes_per_s", "DISK W/s", 10}},
		{{"net_in_bytes_per_s", "NET IN/s", 10}, {"net_out_bytes_per_s", "NET OUT/s", 10}},
		{{"swap_out_bytes_per_s", "SWAP OUT/s", 10}},
		{{"cpu_user_pct", "USER%", 6}, {"cpu_system_pct", "SYS%", 6}, {"cpu_iowait_pct", "IOWAIT%", 7}},
		{{"disk_iops", "IOPS", 8}, {"disk_await_ms", "AWAIT ms", 8}, {"disk_busy_pct", "BUSY%", 6}},
		{{"psi_cpu_some_avg10_pct", "PSI CPU%", 8}, {"psi_memory_some_avg10_pct", "PSI MEM%", 8}, {"psi_io_some_avg10_pct", "PSI IO%", 8}},
	}
	for _, group := range groups {
		var supported []historyTopColumn
		need := 0
		for _, column := range group {
			for _, sample := range samples {
				if sample.metricSupported(column.name) {
					supported = append(supported, column)
					need += column.width + 2
					break
				}
			}
		}
		if used+need <= width+2 {
			columns = append(columns, supported...)
			used += need
		}
	}
	return columns
}

func historyTopTableLine(columns []historyTopColumn, sample *historyTopSample, selected, color bool) string {
	var cells []string
	for _, column := range columns {
		text := column.title
		if sample != nil {
			if column.name == "time" {
				text = sample.Time.Local().Format(time.DateTime)
				if column.width == 8 {
					text = sample.Time.Local().Format("15:04:05")
				}
			} else {
				text = historyTopMetricText(*sample, column.name)
			}
		}
		cell := historyCell(text, column.width, column.name != "time")
		if sample != nil && color && (column.name == "cpu_pct" || column.name == "memory_pct" || column.name == "load1") {
			limits := newTopLimits(sample.Cores, true)
			threshold, value := limits.cpu, sample.CPU
			if column.name == "memory_pct" {
				threshold, value = limits.memory, sample.MemoryPct
			} else if column.name == "load1" {
				threshold, value = limits.load, sample.Load1
			}
			cell = (textStyle{color: true}).level(cell, threshold.level(value))
		}
		cells = append(cells, cell)
	}
	line := strings.Join(cells, "  ")
	if selected {
		return historySelect(line, color)
	}
	return "  " + line
}

func loadHistoryTop(db *sql.DB, options historyOptions) ([]historyTopSample, error) {
	query, args := historyQuery("top", options)
	query = strings.Replace(query, "SELECT payload", "SELECT id, payload", 1)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []historyTopSample
	for rows.Next() {
		var payload string
		var id int64
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		var sample historyTopSample
		if err := json.Unmarshal([]byte(payload), &sample); err != nil {
			return nil, err
		}
		sample.sampleID = id
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

type historySamplesLoadedMsg struct {
	seq       int
	samples   []historyTopSample
	processes []historyProcessSample
	err       error
}

type historySamplesTickMsg struct{ seq int }

type historySampleBrowser struct {
	samples                  []historyTopSample
	processes                []historyProcessSample
	processMode              bool
	path                     string
	width, height, selected  int
	pinned, expanded, paused bool
	color                    bool
	seq                      int
	notice                   string
	view                     viewport.Model
	load                     func() ([]historyTopSample, error)
	loadProcesses            func() ([]historyProcessSample, error)
	hosted                   bool
}

func newHistoryTopBrowser(samples []historyTopSample, path string) historySampleBrowser {
	model := historySampleBrowser{samples: samples, path: path, width: 80, height: 24, color: os.Getenv("NO_COLOR") == "", view: viewport.New()}
	model.resize()
	return model
}

func (model historySampleBrowser) count() int {
	if model.processMode {
		return len(model.processes)
	}
	return len(model.samples)
}

func (model historySampleBrowser) atTop() bool { return !model.expanded }

func (model historySampleBrowser) navigate(key string, db *sql.DB, path string) (tea.Model, bool, error) {
	if !model.atTop() || model.count() == 0 {
		return nil, false, nil
	}
	if !model.processMode && key == "o" {
		sample := model.samples[model.selected]
		processes, err := loadHistoryProcessSample(db, sample.RunID, sample.sampleID)
		if err != nil {
			return nil, true, err
		}
		child := newHistoryProcessBrowser(processes, path)
		child.pinned = true
		child.loadProcesses = func() ([]historyProcessSample, error) {
			return loadHistoryProcessSample(db, sample.RunID, sample.sampleID)
		}
		return child, true, nil
	}
	if model.processMode && key == "t" {
		sample := model.processes[model.selected]
		filter, _ := parseTopProcessFilter(sample.Command)
		options := historyTrendOptions{by: "day", process: filter, metric: "rss_bytes"}
		if sample.Started != nil {
			options.identity = &historyProcessIdentity{pid: sample.PID, started: sample.Started.Format(time.RFC3339Nano)}
		}
		targets := []historyTrendTarget{{kind: historyTrendProcess, value: sample.Command}}
		return newHistoryTrendBrowser(db, targets, options, time.Local), true, nil
	}
	return nil, false, nil
}

func (model historySampleBrowser) canReload() bool {
	return model.load != nil || model.loadProcesses != nil
}

func (model historySampleBrowser) messageKey(name string) string {
	if model.processMode {
		return "history.process_browser." + name
	}
	return "history.top_browser." + name
}

type historySampleIdentity struct {
	run, at, started, command string
	pid                       int
}

func (model historySampleBrowser) identity(index int) historySampleIdentity {
	if model.processMode {
		sample := model.processes[index]
		identity := historySampleIdentity{run: sample.RunID, at: sample.Time.Format(time.RFC3339Nano), pid: sample.PID, command: sample.Command}
		if sample.Started != nil {
			identity.started = sample.Started.Format(time.RFC3339Nano)
		}
		return identity
	}
	sample := model.samples[index]
	return historySampleIdentity{run: sample.RunID, at: sample.Time.Format(time.RFC3339Nano)}
}

func (model historySampleBrowser) tableLine(index int) string {
	if model.processMode {
		return historyProcessTableLine(historyProcessColumns(model.tableWidth(), model.processes), model.processes, index, index >= 0 && index == model.selected, model.color)
	}
	columns := historyTopColumns(model.tableWidth(), model.samples)
	if index < 0 {
		return historyTopTableLine(columns, nil, false, false)
	}
	return historyTopTableLine(columns, &model.samples[index], index == model.selected, model.color)
}

func (model historySampleBrowser) split() bool {
	return !model.expanded && model.width >= 132 && model.height >= 16
}

func (model historySampleBrowser) tableWidth() int {
	if model.split() {
		return max(model.width*3/5, model.width-48)
	}
	return max(1, model.width)
}

func (model historySampleBrowser) detailLines(width int) []string {
	if model.processMode {
		return model.processDetailLines(width)
	}
	if model.count() == 0 {
		return nil
	}
	sample := model.samples[model.selected]
	lines := []string{historyTerminalText(sample.Hostname), sample.Time.Local().Format("2006-01-02 15:04:05 MST"), "run " + historyTerminalText(sample.RunID), fmt.Sprintf("window %.3fs", sample.WindowS), "filter " + historyTerminalText(emptyAs(sample.ProcessFilter, "—")), ""}
	for _, metric := range historyHostMetrics {
		lines = append(lines, metric.name+"  "+historyTopMetricText(sample, metric.name))
	}
	lines = append(lines, "memory_pressure  "+historyTerminalText(emptyAs(sample.MemoryPressure, "—")))
	if len(sample.CoreCPU) > 0 {
		var cores []string
		for i, value := range sample.CoreCPU {
			cores = append(cores, fmt.Sprintf("%d: %.1f%%", i, value))
		}
		lines = append(lines, "", "cores  "+strings.Join(cores, " · "))
	}
	return historyDetailWrappedLines(lines, width, model.color)
}

func historyDetailWrappedLines(lines []string, width int, color bool) []string {
	var wrapped []string
	for i, line := range lines {
		for _, part := range strings.Split(ansi.Wrap(line, max(1, width), ""), "\n") {
			if i == 0 {
				part = historyStrong(part, richCyan, color)
			}
			wrapped = append(wrapped, part)
		}
	}
	return wrapped
}

func (model historySampleBrowser) footer() []string {
	key := "history.top_browser.help"
	if model.expanded {
		key = "history.top_browser.detail_help"
	}
	items := []liveHintItem{{key: "p", on: model.paused}}
	var lines []string
	line := ""
	for _, hint := range strings.Split(T(key), "   ") {
		hint = liveKeyHints(strings.TrimSpace(hint), model.color, items...)
		if line != "" && liveWidth(line)+3+liveWidth(hint) > max(1, model.width) {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += "   "
		}
		line += hint
	}
	if model.hosted && !model.expanded {
		link := "history.top_browser.navigator_help"
		if model.processMode {
			link = "history.process_browser.navigator_help"
		}
		lines = append(lines, historyReviewKeyHints(T(link), model.width, model.color)...)
	}
	lines = append(lines, line)
	if !model.expanded {
		first := min(model.count(), model.view.YOffset()+1)
		last := min(model.count(), model.view.YOffset()+model.view.Height())
		state := T("history.top_browser.following")
		if model.pinned {
			state = T("history.top_browser.selected")
		}
		if model.paused {
			state = T("history.top_browser.paused")
		}
		lines = append([]string{historyTint(fmt.Sprintf("%d–%d / %d · %s", first, last, model.count(), state), richGray, model.color)}, lines...)
	}
	if model.notice != "" {
		lines = append(lines, historyTint(historyTerminalText(model.notice), richYellow, model.color))
	}
	return lines
}

func (model *historySampleBrowser) resize() {
	model.selected = min(max(0, model.selected), max(0, model.count()-1))
	width := model.tableWidth()
	model.view.SetWidth(width)
	model.view.SetHeight(max(1, model.height-4-len(model.footer())))
	var lines []string
	if model.expanded {
		lines = model.detailLines(width)
	} else if model.processMode {
		columns := historyProcessColumns(width, model.processes)
		for i := range model.processes {
			lines = append(lines, historyProcessTableLine(columns, model.processes, i, i == model.selected, model.color))
		}
	} else {
		columns := historyTopColumns(width, model.samples)
		for i := range model.samples {
			lines = append(lines, historyTopTableLine(columns, &model.samples[i], i == model.selected, model.color))
		}
	}
	model.view.SetContent(strings.Join(lines, "\n"))
	if !model.expanded {
		keepHistorySelection(&model.view, model.selected)
	}
}

func (model historySampleBrowser) Init() tea.Cmd {
	if !model.canReload() {
		return nil
	}
	return model.tick()
}

func (model historySampleBrowser) tick() tea.Cmd {
	seq := model.seq
	return tea.Tick(logHistoryRefresh, func(time.Time) tea.Msg { return historySamplesTickMsg{seq: seq} })
}

func (model historySampleBrowser) reload() tea.Cmd {
	seq, load, loadProcesses := model.seq, model.load, model.loadProcesses
	return func() tea.Msg {
		if loadProcesses != nil {
			processes, err := loadProcesses()
			return historySamplesLoadedMsg{seq: seq, processes: processes, err: err}
		}
		samples, err := load()
		return historySamplesLoadedMsg{seq: seq, samples: samples, err: err}
	}
}

func (model historySampleBrowser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.resize()
	case historySamplesTickMsg:
		if value.seq == model.seq && !model.paused && model.canReload() {
			return model, model.reload()
		}
	case historySamplesLoadedMsg:
		if value.seq != model.seq {
			return model, nil
		}
		if model.processMode && value.samples != nil || !model.processMode && value.processes != nil {
			return model, nil
		}
		if value.err != nil {
			model.notice = value.err.Error()
		} else {
			selected := -1
			var identity historySampleIdentity
			if model.pinned && model.count() > 0 {
				identity = model.identity(model.selected)
			}
			model.samples, model.processes, model.notice = value.samples, value.processes, ""
			if model.pinned {
				for i := 0; i < model.count(); i++ {
					if model.identity(i) == identity {
						selected = i
						break
					}
				}
			}
			if selected >= 0 {
				model.selected = selected
			} else {
				model.selected, model.pinned, model.expanded = 0, false, false
			}
		}
		model.resize()
		if model.canReload() && !model.paused {
			return model, model.tick()
		}
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "ctrl+c":
			return model, tea.Quit
		case "esc":
			if !model.expanded {
				return model, tea.Quit
			}
			model.expanded = false
			model.resize()
			return model, nil
		case "r", "p":
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
			model.selected = min(max(0, model.count()-1), model.selected+1)
		case "pgup":
			model.selected = max(0, model.selected-model.view.Height())
		case "pgdown":
			model.selected = min(max(0, model.count()-1), model.selected+model.view.Height())
		case "home":
			model.selected = 0
		case "end":
			model.selected = max(0, model.count()-1)
		case "enter":
			if model.count() > 0 {
				model.expanded, model.pinned = true, true
				model.view.GotoTop()
				model.resize()
			}
			return model, nil
		default:
			return model, nil
		}
		model.pinned = model.selected != 0
		model.resize()
	}
	return model, nil
}

func (model historySampleBrowser) View() tea.View {
	var lines []string
	if model.width < 40 || model.height < 10 {
		lines = []string{T("cli.log_history.small"), T("cli.log_history.quit_help")}
	} else {
		lines = []string{historyStrong(T(model.messageKey("title")), richCyan, model.color), historyTint(historyTerminalText(model.path), richGray, model.color), ""}
		header := ""
		if !model.expanded {
			header = model.tableLine(-1)
			if model.split() {
				header = historyCell(header, model.tableWidth(), false) + " │ " + T("history.top_browser.details")
			}
		}
		lines = append(lines, historyTint(header, richGray, model.color))
		body := strings.Split(model.view.View(), "\n")
		if model.count() == 0 {
			copy(body, strings.Split(ansi.Wrap(T(model.messageKey("empty")), model.tableWidth(), ""), "\n"))
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
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], max(1, model.width), "…")
	}
	lines = lines[:min(len(lines), max(1, model.height))]
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func runHistoryTopBrowser(db *sql.DB, path string, options historyOptions) error {
	samples, err := loadHistoryTop(db, options)
	if err != nil {
		return err
	}
	model := newHistoryTopBrowser(samples, path)
	model.load = func() ([]historyTopSample, error) { return loadHistoryTop(db, options) }
	_, err = tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}
