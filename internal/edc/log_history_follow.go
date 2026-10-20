package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	row        logHistoryAttempt
}

type logHistoryFollowSummary struct {
	Type       string `json:"type"`
	Started    int    `json:"started"`
	Ended      int    `json:"ended"`
	Failed     int    `json:"failed"`
	Errors     int    `json:"errors"`
	DurationMS int64  `json:"duration_ms"`
}

type logHistoryFollowState struct {
	outcome logHistoryOutcome
	// unknownPolls counts refreshes that saw a running attempt as UNKNOWN. A
	// refresh that raced an append drops the end marker for one pass, so one
	// UNKNOWN is not yet a crash.
	unknownPolls int
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

func newFollowEvent(kind string, row logHistoryAttempt, at time.Time) logHistoryFollowEvent {
	event := logHistoryFollowEvent{Type: kind, Time: at.UTC(), Key: row.Key, Command: row.Command, CWD: row.CWD, Log: row.Path, Attempt: row.Attempt, row: row}
	if kind == "end" {
		event.Outcome = string(row.Outcome)
		event.Exit = row.Exit
		if !row.Ended.IsZero() && !row.damaged {
			duration := row.Duration.Milliseconds()
			event.DurationMS = &duration
		}
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
		if event.Type == "start" {
			summary.Started++
		} else {
			summary.Ended++
			if event.row.failed() {
				summary.Failed++
			}
		}
		if options.Failed && (event.Type == "start" || !event.row.failed()) {
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
		if row.Outcome == historyOutcomeRunning && !send(newFollowEvent("start", row, row.Started)) {
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
				fmt.Fprintln(stderr, T("cli.log_history.follow_summary", summary.Started, summary.Ended, summary.Failed, time.Duration(summary.DurationMS)*time.Millisecond))
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
		for index := len(rows) - 1; index >= 0; index-- {
			row := rows[index]
			id := followedHistoryID(row)
			state := states[id]
			if state == nil {
				state = &logHistoryFollowState{outcome: historyOutcomeRunning}
				states[id] = state
				if !send(newFollowEvent("start", row, row.Started)) {
					return 2
				}
			}
			if !followedHistoryOpen(state.outcome) {
				continue
			}
			switch {
			case row.Outcome == historyOutcomeRunning:
				state.outcome, state.unknownPolls = row.Outcome, 0
			case row.Outcome == historyOutcomeUnknown:
				if state.outcome != historyOutcomeRunning {
					continue
				}
				state.unknownPolls++
				if state.unknownPolls < 2 {
					continue
				}
				state.outcome = row.Outcome
				if !send(newFollowEvent("end", row, time.Now())) {
					return 2
				}
			default:
				state.outcome = row.Outcome
				if !send(newFollowEvent("end", row, row.Ended)) {
					return 2
				}
			}
		}
	}
}
