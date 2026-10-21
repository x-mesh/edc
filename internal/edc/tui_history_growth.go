package edc

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

func historyGrowthSize(value float64) string {
	return formatSizeShort(uint64(math.Max(value, 0)))
}

func historyGrowthReviewData(growth historyGrowth) historyReviewData {
	data := historyReviewData{summary: T("history.growth.window", growth.From.Local().Format(time.DateTime), growth.To.Local().Format(time.DateTime)), empty: T("history.growth.empty")}
	for _, disk := range growth.Disks {
		rate, full := "—", "—"
		if disk.BytesPerDay != nil {
			rate = historyGrowthRate(*disk.BytesPerDay)
		}
		if disk.FullInDays != nil {
			full = T("history.growth.full_in", historyGrowthDays(*disk.FullInDays))
		} else if disk.BytesPerDay == nil {
			if disk.Samples < historyGrowthMinSamples {
				full = T("history.growth.few_samples", disk.Samples)
			} else {
				full = T("history.growth.short_span")
			}
		}
		data.entries = append(data.entries, historyReviewEntry{id: "disk:" + disk.Mount, command: "disk", navigate: historyDiskProbePrefix + disk.Mount, cells: []string{"disk " + disk.Mount, rate, historyGrowthSize(disk.UsedBytes), fmt.Sprint(disk.Samples), full},
			details: []string{historyTerminalText(disk.Mount), "", "used  " + historyGrowthSize(disk.UsedBytes), "total  " + historyGrowthSize(disk.TotalBytes), "change  " + rate, "full in  " + full, fmt.Sprintf("samples  %d", disk.Samples), "first  " + disk.First.Local().Format(time.DateTime), "last  " + disk.Last.Local().Format(time.DateTime)}})
	}
	for _, dirs := range growth.Dirs {
		span := dirs.Last.Sub(dirs.First).Hours() / 24
		rate := "—"
		if span >= 1 && dirs.Note == "" {
			rate = historyGrowthRate((dirs.AfterBytes - dirs.BeforeBytes) / span)
		}
		var output strings.Builder
		writeHistoryGrowth(&output, historyGrowth{From: growth.From, To: growth.To, Dirs: []historyGrowthDirs{dirs}}, time.Local)
		details := strings.Split(strings.TrimSpace(output.String()), "\n")
		data.entries = append(data.entries, historyReviewEntry{id: "du:" + dirs.Root, cells: []string{"du " + dirs.Root, rate, historyGrowthSize(dirs.AfterBytes), fmt.Sprint(dirs.Snapshots), dirs.Note}, details: append([]string{historyTerminalText(dirs.Root), ""}, details...)})
		for _, delta := range dirs.Directories {
			path := filepath.Join(dirs.Root, delta.Path)
			if filepath.IsAbs(delta.Path) {
				path = delta.Path
			}
			rate := "—"
			if span >= 1 && delta.Bound == "" {
				rate = historyGrowthRate(float64(delta.Delta) / span)
			}
			data.entries = append(data.entries, historyReviewEntry{id: "dir:" + dirs.Root + "\x00" + delta.Path,
				cells:   []string{"dir " + path, rate, formatSizeShort(delta.After), fmt.Sprint(dirs.Snapshots), delta.Label},
				details: []string{historyTerminalText(path), "root  " + historyTerminalText(dirs.Root), "", "before  " + formatSizeShort(delta.Before), "after  " + formatSizeShort(delta.After), "change/day  " + rate, strings.Join(formatDuDirDeltas([]duDirDelta{delta}), " "), historyTerminalText(delta.Label), "first  " + dirs.First.Local().Format(time.DateTime), "last  " + dirs.Last.Local().Format(time.DateTime)}})
		}
	}
	for _, process := range growth.Processes {
		rate := "—"
		if process.BytesPerDay != nil {
			rate = historyGrowthRate(*process.BytesPerDay)
		}
		name := topProcessName(process.Command, max(1, len([]rune(process.Command))))
		started := "—"
		if process.Started != nil {
			started = process.Started.Local().Format(time.RFC3339Nano)
		}
		details := []string{historyTerminalText(process.Command), fmt.Sprintf("PID %d", process.PID), "started  " + started, "", "RSS before  " + historyGrowthSize(process.FirstRSSBytes), "RSS latest  " + historyGrowthSize(process.RSSBytes), "change/day  " + rate, fmt.Sprintf("samples  %d", process.Samples), "first  " + process.First.Local().Format(time.DateTime), "last  " + process.Last.Local().Format(time.DateTime), historyGrowthProcessNote(process)}
		if process.Fit != nil {
			details = append(details, fmt.Sprintf("R²  %.3f", *process.Fit))
		}
		if process.Unfiltered {
			details = append(details, T("history.trend.unfiltered", historyTerminalText(growth.Process), quoteLocalShellWord(growth.Process)))
		}
		var identity *historyProcessIdentity
		if process.Started != nil {
			identity = &historyProcessIdentity{pid: process.PID, started: process.Started.Format(time.RFC3339Nano)}
		}
		data.entries = append(data.entries, historyReviewEntry{id: fmt.Sprintf("process:%d:%s:%s", process.PID, started, process.Command), command: "process", navigate: process.Command, identity: identity, cells: []string{fmt.Sprintf("%d %s", process.PID, name), rate, historyGrowthSize(process.RSSBytes), fmt.Sprint(process.Samples), historyGrowthProcessNote(process)}, details: details})
	}
	if growth.Process != "" {
		data.empty = T("history.trend.process_empty", historyTerminalText(growth.Process), quoteLocalShellWord(growth.Process))
	}
	for i := range data.entries {
		for j := range data.entries[i].details {
			data.entries[i].details[j] = historyTerminalText(data.entries[i].details[j])
		}
	}
	return data
}

func runHistoryGrowthBrowser(db *sql.DB, path string, options historyGrowthOptions, growth historyGrowth) error {
	columns := []historyReviewColumn{{"TARGET", 0, 0}, {"CHANGE/DAY", 13, 0}, {"LATEST", 11, 64}, {"SAMPLES", 7, 84}, {"NOTE", 24, 120}}
	model := newHistoryReviewBrowser(T("history.review.growth_title"), path, columns, historyGrowthReviewData(growth))
	model.navigateFn = historyGrowthNavigate
	model.extraHelp = "history.growth.navigator_help"
	model.growthOptions = &options
	model.growthLoad = func(options historyGrowthOptions) (historyReviewData, error) {
		growth, err := collectHistoryGrowth(db, options, time.Now())
		if err != nil {
			return historyReviewData{}, err
		}
		return historyGrowthReviewData(growth), nil
	}
	_, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}

func historyGrowthNavigate(model historyReviewBrowser, key string, db *sql.DB, path string) (tea.Model, bool, error) {
	if key != "t" || model.expanded || len(model.data.entries) == 0 {
		return nil, false, nil
	}
	entry := model.data.entries[model.selected]
	options := historyTrendOptions{by: "day", metric: "rss_bytes"}
	target := historyTrendTarget{}
	switch entry.command {
	case "process":
		filter, _ := parseTopProcessFilter(entry.navigate)
		options.process, target = filter, historyTrendTarget{kind: historyTrendProcess, value: entry.navigate}
		options.identity = entry.identity
	case "disk":
		options.probe, options.metric, target = entry.navigate, "used_bytes", historyTrendTarget{kind: historyTrendProbe, value: entry.navigate}
	default:
		return nil, false, nil
	}
	return newHistoryTrendBrowser(db, []historyTrendTarget{target}, options, time.Local), true, nil
}

func newHistoryGrowthNavigator(db *sql.DB, path string, options historyGrowthOptions, growth historyGrowth) historyReviewBrowser {
	columns := []historyReviewColumn{{"TARGET", 0, 0}, {"CHANGE/DAY", 13, 0}, {"LATEST", 11, 64}, {"SAMPLES", 7, 84}, {"NOTE", 24, 120}}
	model := newHistoryReviewBrowser(T("history.review.growth_title"), path, columns, historyGrowthReviewData(growth))
	model.navigateFn = historyGrowthNavigate
	model.extraHelp = "history.growth.navigator_help"
	model.growthOptions = &options
	model.growthLoad = func(options historyGrowthOptions) (historyReviewData, error) {
		growth, err := collectHistoryGrowth(db, options, time.Now())
		if err != nil {
			return historyReviewData{}, err
		}
		return historyGrowthReviewData(growth), nil
	}
	return model
}
