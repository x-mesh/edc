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
)

func historyProcessColumns(width int, samples []historyProcessSample) []historyTopColumn {
	columns := []historyTopColumn{{"time", "TIME", 19}, {"pid", "PID", 7}, {"cpu", "CPU%", 7}, {"rss", "RSS", 9}}
	if width < 72 {
		columns = []historyTopColumn{{"time", "TIME", 8}, {"pid", "PID", 5}, {"cpu", "CPU%", 5}, {"rss", "RSS", 7}}
	}
	used := 2
	for _, column := range columns {
		used += column.width + 2
	}
	reserve := min(24, max(5, width/5))
	var memory, disk, threads, fds bool
	for _, sample := range samples {
		memory = memory || sample.MemoryBytes != nil
		disk = disk || sample.DiskRead != nil || sample.DiskWrite != nil
		threads = threads || sample.Threads > 0
		fds = fds || sample.FDs > 0
	}
	for _, group := range []struct {
		show    bool
		columns []historyTopColumn
	}{
		{memory, []historyTopColumn{{"memory", "MEM", 9}}},
		{disk, []historyTopColumn{{"read", "READ/s", 10}, {"write", "WRITE/s", 10}}},
		{threads, []historyTopColumn{{"threads", "THR", 6}}},
		{fds, []historyTopColumn{{"fds", "FDs", 6}}},
	} {
		if !group.show {
			continue
		}
		need := 0
		for _, column := range group.columns {
			need += column.width + 2
		}
		if used+need+reserve <= width {
			columns = append(columns, group.columns...)
			used += need
		}
	}
	commandWidth := max(1, width-used)
	title := "COMMAND"
	if commandWidth < len(title) {
		title = "CMD"
	}
	return append(columns, historyTopColumn{"command", title, commandWidth})
}

func historyProcessCell(sample historyProcessSample, name string) string {
	switch name {
	case "pid":
		return strconv.Itoa(sample.PID)
	case "cpu":
		return fmt.Sprintf("%.1f%%", sample.CPU)
	case "rss":
		return formatSizeShort(sample.RSSBytes)
	case "memory":
		if sample.MemoryBytes != nil {
			return formatSizeShort(*sample.MemoryBytes)
		}
	case "read":
		if sample.DiskRead != nil {
			return historyTrendValue("disk_read_bytes_per_s", *sample.DiskRead)
		}
	case "write":
		if sample.DiskWrite != nil {
			return historyTrendValue("disk_write_bytes_per_s", *sample.DiskWrite)
		}
	case "threads":
		if sample.Threads > 0 {
			return strconv.Itoa(sample.Threads)
		}
	case "fds":
		if sample.FDs > 0 {
			return strconv.Itoa(sample.FDs)
		}
	case "command":
		return historyTerminalText(sample.Command)
	}
	return "—"
}

func historyProcessTableLine(columns []historyTopColumn, samples []historyProcessSample, index int, selected, color bool) string {
	var cells []string
	for _, column := range columns {
		text := column.title
		if index >= 0 {
			sample := samples[index]
			text = historyProcessCell(sample, column.name)
			if column.name == "command" {
				text = historyTerminalText(topProcessName(sample.Command, max(1, len([]rune(sample.Command)))))
			}
			if column.name == "cpu" && column.width < 7 {
				text = fmt.Sprintf("%.0f%%", sample.CPU)
			}
			if column.name == "time" {
				text = sample.Time.Local().Format(time.DateTime)
				if column.width == 8 {
					text = sample.Time.Local().Format("15:04:05")
				}
			}
		}
		cells = append(cells, historyCell(text, column.width, column.name != "time" && column.name != "command"))
	}
	line := strings.Join(cells, "  ")
	if selected {
		return historySelect(line, color)
	}
	return "  " + line
}

func loadHistoryProcess(db *sql.DB, options historyOptions) ([]historyProcessSample, error) {
	query, args := historyQuery("process", options)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []historyProcessSample
	for rows.Next() {
		var payload, hostPayload string
		if err := rows.Scan(&payload, &hostPayload); err != nil {
			return nil, err
		}
		var process topProcessSample
		var host historyTopSample
		if err := json.Unmarshal([]byte(payload), &process); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hostPayload), &host); err != nil {
			return nil, err
		}
		samples = append(samples, historyProcessSample{topProcessSample: process, RunID: host.RunID, Time: host.Time, Hostname: host.Hostname, WindowS: host.WindowS, ProcessFilter: host.ProcessFilter, ProcessObservedAt: host.ProcessObservedAt})
	}
	return samples, rows.Err()
}

func loadHistoryProcessSample(db *sql.DB, run string, sampleID int64) ([]historyProcessSample, error) {
	rows, err := db.Query(`SELECT p.payload, s.payload FROM top_process_samples p JOIN top_samples s ON s.id=p.sample_id WHERE p.run_id=? AND p.sample_id=? ORDER BY p.id`, run, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []historyProcessSample
	for rows.Next() {
		var payload, hostPayload string
		if err := rows.Scan(&payload, &hostPayload); err != nil {
			return nil, err
		}
		var process topProcessSample
		var host historyTopSample
		if err := json.Unmarshal([]byte(payload), &process); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hostPayload), &host); err != nil {
			return nil, err
		}
		samples = append(samples, historyProcessSample{topProcessSample: process, RunID: host.RunID, Time: host.Time, Hostname: host.Hostname, WindowS: host.WindowS, ProcessFilter: host.ProcessFilter, ProcessObservedAt: host.ProcessObservedAt})
	}
	return samples, rows.Err()
}

func newHistoryProcessBrowser(samples []historyProcessSample, path string) historySampleBrowser {
	model := historySampleBrowser{processes: samples, processMode: true, path: path, width: 80, height: 24, color: os.Getenv("NO_COLOR") == "", view: viewport.New(), owner: historyBrowserOwner.Add(1)}
	model.resize()
	return model
}

func (model historySampleBrowser) processDetailLines(width int) []string {
	if len(model.processes) == 0 {
		return nil
	}
	sample := model.processes[model.selected]
	lines := []string{historyTerminalText(sample.Command), fmt.Sprintf("PID %d · %s", sample.PID, historyTerminalText(sample.Hostname)), sample.Time.Local().Format("2006-01-02 15:04:05 MST"), ""}
	for _, column := range []historyTopColumn{{"cpu", "CPU (100% = 1 core)", 0}, {"rss", "RSS", 0}, {"memory", "MEM", 0}, {"read", "Read", 0}, {"write", "Write", 0}, {"threads", "Threads", 0}, {"fds", "FDs", 0}} {
		lines = append(lines, column.title+"  "+historyProcessCell(sample, column.name))
	}
	started := "—"
	if sample.Started != nil {
		started = sample.Started.Local().Format("2006-01-02 15:04:05 MST")
	}
	lines = append(lines, "", "started  "+started, "run  "+historyTerminalText(sample.RunID), fmt.Sprintf("window  %.3fs", sample.WindowS), "filter  "+historyTerminalText(emptyAs(sample.ProcessFilter, "—")))
	if sample.ProcessObservedAt != nil {
		lines = append(lines, "observed  "+sample.ProcessObservedAt.Local().Format("2006-01-02 15:04:05.000 MST"))
	}
	if sample.Limits != nil {
		lines = append(lines, "")
		for _, line := range topProcessLimitLines([]topProcess{{PID: sample.PID, Limits: sample.Limits}}) {
			lines = append(lines, historyTerminalText(line))
		}
	}
	if probe := sample.EBPF; probe != nil {
		lines = append(lines, "", fmt.Sprintf("%s · %.3fs", historyTerminalText(string(probe.Source)), probe.WindowS))
		for _, entry := range []struct {
			name  string
			value *float64
		}{{"runq avg ms", probe.RunqAvgMS}, {"runq p95 ms", probe.RunqP95MS}, {"I/O avg ms", probe.IOAvgMS}, {"I/O p95 ms", probe.IOP95MS}} {
			if entry.value != nil {
				lines = append(lines, fmt.Sprintf("%s  %.3f", entry.name, *entry.value))
			}
		}
		for _, wait := range probe.Waits {
			lines = append(lines, fmt.Sprintf("%s  %d · %.3fms", historyTerminalText(wait.Syscall), wait.Count, wait.TotalMS))
		}
	}
	return historyDetailWrappedLines(lines, width, model.color)
}

func runHistoryProcessBrowser(db *sql.DB, path string, options historyOptions) error {
	samples, err := loadHistoryProcess(db, options)
	if err != nil {
		return err
	}
	model := newHistoryProcessBrowser(samples, path)
	model.options = options
	model.loadProcessesWith = func(options historyOptions) ([]historyProcessSample, error) { return loadHistoryProcess(db, options) }
	_, err = tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}
