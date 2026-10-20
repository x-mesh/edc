package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type logHistoryFollowEvent struct {
	Type       string    `json:"type"`
	Time       time.Time `json:"time"`
	Key        string    `json:"key"`
	Command    []string  `json:"command,omitempty"`
	CWD        string    `json:"cwd"`
	Log        string    `json:"log"`
	Attempt    int       `json:"attempt"`
	Outcome    string    `json:"outcome,omitempty"`
	Exit       *int      `json:"exit,omitempty"`
	DurationMS *int64    `json:"duration_ms,omitempty"`
	CPUMS      *int64    `json:"cpu_ms,omitempty"`
	MaxRSS     uint64    `json:"max_rss,omitempty"`
	MemPeak    uint64    `json:"mem_peak,omitempty"`
	MemAvg     uint64    `json:"mem_avg,omitempty"`
	CPUPeak    *float64  `json:"cpu_peak,omitempty"`
	DiskRead   *uint64   `json:"disk_read,omitempty"`
	DiskWrite  *uint64   `json:"disk_write,omitempty"`
	Scope      string    `json:"scope,omitempty"`
	Container  string    `json:"container,omitempty"`
	ElapsedMS  *int64    `json:"elapsed_ms,omitempty"`
	// The ratios compare with the median of earlier successful runs of the key;
	// alerts names those at twice the median or more.
	DurationRatio *float64 `json:"duration_ratio,omitempty"`
	MemoryRatio   *float64 `json:"memory_ratio,omitempty"`
	CPURatio      *float64 `json:"cpu_ratio,omitempty"`
	Alerts        []string `json:"alerts,omitempty"`
	row           logHistoryAttempt
	ratios        historyRatios
}

type logHistoryFollowSummary struct {
	Type       string `json:"type"`
	Started    int    `json:"started"`
	Ended      int    `json:"ended"`
	Failed     int    `json:"failed"`
	Slow       int    `json:"slow"`
	Errors     int    `json:"errors"`
	DurationMS int64  `json:"duration_ms"`
}

type logHistoryFollowState struct {
	outcome logHistoryOutcome
	// unknownPolls counts refreshes that saw a running attempt as UNKNOWN. A
	// refresh that raced an append drops the end marker for one pass, so one
	// UNKNOWN is not yet a crash.
	unknownPolls int
	// slow is set once a slow event was sent for the running attempt.
	slow bool
}

func runLogHistoryFollow(options logHistoryOptions, streams logStreams) int {
	writer := streams.stdout
	if options.JSON != "" {
		output, closeOutput, err := openObserveStream(options.JSON)
		if err != nil {
			fmt.Fprintln(streams.stderr, err)
			return 2
		}
		defer closeOutput()
		writer = output
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	terminal, ok := writer.(*os.File)
	color := options.JSON == "" && ok && isTerminal(terminal) && os.Getenv("NO_COLOR") == ""
	cache := logHistoryCache{}
	load := func() (logHistorySnapshot, error) { return loadLogHistory(options, cache) }
	return streamLogHistoryFollow(ctx, writer, streams.stderr, logHistoryRefresh, options, color, load)
}

func followedHistoryRows(snapshot logHistorySnapshot, options logHistoryOptions) []logHistoryAttempt {
	var rows []logHistoryAttempt
	for _, row := range snapshot.Rows {
		if row.Key == "" || !strings.HasPrefix(row.Key, options.Key) {
			continue
		}
		if options.Name != "" && (len(row.Command) == 0 || filepath.Base(row.Command[0]) != options.Name) {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

func followedHistoryID(row logHistoryAttempt) string {
	return row.Path + "\x00" + row.Key + "\x00" + row.Started.Format(time.RFC3339Nano)
}

func followedHistoryOpen(outcome logHistoryOutcome) bool {
	return outcome == historyOutcomeRunning || outcome == historyOutcomeUnknown
}

func followRatio(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	rounded := math.Round(value*100) / 100
	return &rounded
}

func newFollowEvent(kind string, row logHistoryAttempt, at time.Time, ratios historyRatios) logHistoryFollowEvent {
	event := logHistoryFollowEvent{Type: kind, Time: at.UTC(), Key: row.Key, Command: row.Command, CWD: row.CWD, Log: row.Path, Attempt: row.Attempt, row: row, ratios: ratios}
	if kind == "slow" {
		elapsed := at.Sub(row.Started).Milliseconds()
		event.ElapsedMS, event.DurationRatio, event.Alerts = &elapsed, followRatio(ratios.duration), []string{"duration"}
	}
	if kind == "end" {
		event.Outcome = string(row.Outcome)
		event.Exit = row.Exit
		if !row.Ended.IsZero() && !row.damaged {
			duration := row.Duration.Milliseconds()
			event.DurationMS = &duration
		}
		if resources := row.Resources; resources != nil {
			if resources.cpu {
				cpu := resources.CPU.Milliseconds()
				event.CPUMS = &cpu
			}
			event.MaxRSS, event.Scope, event.Container = resources.MaxRSS, resources.Scope, resources.Container
			if resources.Samples > 0 {
				peak := resources.CPUPeak
				event.MemPeak, event.MemAvg, event.CPUPeak = resources.MemPeak, resources.MemAvg, &peak
			}
			if resources.disk {
				read, write := resources.DiskRead, resources.DiskWrite
				event.DiskRead, event.DiskWrite = &read, &write
			}
		}
		event.DurationRatio, event.MemoryRatio, event.CPURatio, event.Alerts = followRatio(ratios.duration), followRatio(ratios.memory), followRatio(ratios.cpu), ratios.alerts()
	}
	return event
}

func formatFollowEvent(event logHistoryFollowEvent, color bool) string {
	label := historyCell(T("cli.log_history.follow_start"), 10, false)
	detail := "cwd " + reportIdentityValue(event.CWD)
	if event.Type == "end" {
		row := event.row
		label = historyCell(historyOutcome(row), 10, false)
		switch {
		case row.Outcome == historyOutcomeSuccess:
			label = historyTint(label, richGreen, color)
		case row.failed():
			label = historyStrong(label, richRed, color)
		default:
			label = historyTint(label, richYellow, color)
		}
		detail = historyDuration(row)
		if row.Exit != nil {
			detail += fmt.Sprintf("  exit %d", *row.Exit)
		}
		if resources := row.Resources; resources != nil {
			if resources.memory() > 0 {
				detail += "  " + T("cli.log_history.resources.memory", formatSizeShort(resources.memory()))
			}
			if resources.cpu {
				detail += "  " + T("cli.log_history.resources.cpu", resources.CPU.Round(time.Millisecond).String())
			}
		}
		if alerts := event.ratios.alertText(); alerts != "" {
			return fmt.Sprintf("%s  %s  %s  %s  %s", historyTint(event.Time.Local().Format("15:04:05"), richGray, color), label, historyCommand(row), historyTint(detail, richGray, color), historyStrong("▲ "+alerts, richYellow, color))
		}
	} else if event.Type == "slow" {
		label = historyStrong(historyCell(T("cli.log_history.follow_slow"), 10, false), richYellow, color)
		detail = historyShortDuration(time.Duration(*event.ElapsedMS)*time.Millisecond) + "  " + historyRatioText(event.ratios.duration)
	} else {
		label = historyStrong(label, richCyan, color)
	}
	return fmt.Sprintf("%s  %s  %s  %s", historyTint(event.Time.Local().Format("15:04:05"), richGray, color), label, historyCommand(event.row), historyTint(detail, richGray, color))
}

// Attempts already running are announced first so a late start still shows
// what is in progress. Older finished attempts stay silent.
func streamLogHistoryFollow(ctx context.Context, writer, stderr io.Writer, interval time.Duration, options logHistoryOptions, color bool, load func() (logHistorySnapshot, error)) int {
	started := time.Now()
	snapshot, err := load()
	if err != nil {
		fmt.Fprintln(stderr, reportIdentityValue(err.Error()))
		return 2
	}
	summary := logHistoryFollowSummary{Type: "summary"}
	jsonOutput := options.JSON != ""
	emit := func(value any, text string) bool {
		if jsonOutput {
			data, err := json.Marshal(value)
			if err == nil {
				_, err = fmt.Fprintln(writer, string(data))
			}
			if err != nil {
				fmt.Fprintln(stderr, err)
				return false
			}
			return true
		}
		if _, err := fmt.Fprintln(writer, text); err != nil {
			fmt.Fprintln(stderr, err)
			return false
		}
		return true
	}
	reported := map[string]bool{}
	report := func(issues []string) bool {
		current := map[string]bool{}
		for _, issue := range issues {
			current[issue] = true
			if reported[issue] {
				continue
			}
			summary.Errors++
			if jsonOutput {
				if !emit(map[string]any{"type": "error", "time": time.Now().UTC(), "message": issue}, "") {
					return false
				}
			} else {
				fmt.Fprintln(stderr, issue)
			}
		}
		reported = current
		return true
	}
	states := map[string]*logHistoryFollowState{}
	send := func(event logHistoryFollowEvent) bool {
		switch event.Type {
		case "start":
			summary.Started++
		case "slow":
			summary.Slow++
		default:
			summary.Ended++
			if event.row.failed() {
				summary.Failed++
			}
		}
		if options.Failed && (event.Type != "end" || !event.row.failed()) {
			return true
		}
		return emit(event, formatFollowEvent(event, color))
	}
	if !report(snapshot.Issues) {
		return 2
	}
	rows := followedHistoryRows(snapshot, options)
	for index := len(rows) - 1; index >= 0; index-- {
		row := rows[index]
		states[followedHistoryID(row)] = &logHistoryFollowState{outcome: row.Outcome}
		if row.Outcome == historyOutcomeRunning && !send(newFollowEvent("start", row, row.Started, historyRatios{})) {
			return 2
		}
	}
	if !jsonOutput {
		fmt.Fprintln(stderr, T("cli.log_history.follow_ready"))
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			summary.DurationMS = time.Since(started).Milliseconds()
			if jsonOutput {
				if !emit(summary, "") {
					return 2
				}
			} else {
				fmt.Fprintln(stderr, T("cli.log_history.follow_summary", summary.Started, summary.Ended, summary.Failed, summary.Slow, time.Duration(summary.DurationMS)*time.Millisecond))
			}
			if summary.Errors > 0 {
				return 1
			}
			return 0
		case <-ticker.C:
		}
		snapshot, err := load()
		if err != nil {
			if !report([]string{reportIdentityValue(err.Error())}) {
				return 2
			}
			continue
		}
		if !report(snapshot.Issues) {
			return 2
		}
		rows := followedHistoryRows(snapshot, options)
		byKey := map[string][]logHistoryAttempt{}
		for _, row := range rows {
			byKey[row.Key] = append(byKey[row.Key], row)
		}
		for index := len(rows) - 1; index >= 0; index-- {
			row := rows[index]
			ratios := historyBaseline(byKey[row.Key], row)
			id := followedHistoryID(row)
			state := states[id]
			if state == nil {
				state = &logHistoryFollowState{outcome: historyOutcomeRunning}
				states[id] = state
				if !send(newFollowEvent("start", row, row.Started, historyRatios{})) {
					return 2
				}
			}
			if !followedHistoryOpen(state.outcome) {
				continue
			}
			switch {
			case row.Outcome == historyOutcomeRunning:
				state.outcome, state.unknownPolls = row.Outcome, 0
				if !state.slow && ratios.duration >= historyRatioAlert {
					state.slow = true
					if !send(newFollowEvent("slow", row, time.Now(), ratios)) {
						return 2
					}
				}
			case row.Outcome == historyOutcomeUnknown:
				if state.outcome != historyOutcomeRunning {
					continue
				}
				state.unknownPolls++
				if state.unknownPolls < 2 {
					continue
				}
				state.outcome = row.Outcome
				if !send(newFollowEvent("end", row, time.Now(), ratios)) {
					return 2
				}
			default:
				state.outcome = row.Outcome
				if !send(newFollowEvent("end", row, row.Ended, ratios)) {
					return 2
				}
			}
		}
	}
}
