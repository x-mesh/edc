package edc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

func historyListReviewData(db *sql.DB, options historyOptions, now time.Time) (historyReviewData, error) {
	query, values := historyQuery("list", options)
	rows, err := db.Query(query, values...)
	if err != nil {
		return historyReviewData{}, err
	}
	defer rows.Close()
	data := historyReviewData{summary: T("history.list_browser.summary"), empty: T("history.list_browser.empty")}
	for rows.Next() {
		var payload, status string
		var ended sql.NullInt64
		var samples int
		if err := rows.Scan(&payload, &ended, &status, &samples); err != nil {
			return historyReviewData{}, err
		}
		var run historyRun
		if err := json.Unmarshal([]byte(payload), &run); err != nil {
			return historyReviewData{}, err
		}
		run.Status, run.SampleCount = status, samples
		if ended.Valid {
			at := time.Unix(0, ended.Int64)
			run.EndedAt = &at
		}
		endedText, duration := "—", "—"
		if run.EndedAt != nil {
			endedText = run.EndedAt.Local().Format("2006-01-02 15:04:05 MST")
			duration = historyShortDuration(run.EndedAt.Sub(run.StartedAt))
		}
		interval, process, interfaces := fmt.Sprintf("%gs", run.IntervalS), emptyAs(run.Process, "—"), emptyAs(run.Interfaces, "all")
		if run.Command == historySnapshotCommand {
			interval, process, interfaces = "—", "—", "—"
		}
		details := []string{
			T("history.list_browser.run", run.ID),
			T("history.list_browser.command", run.Command),
			T("history.list_browser.host", run.Hostname),
			"",
			T("history.list_browser.started", run.StartedAt.Local().Format("2006-01-02 15:04:05 MST")),
			T("history.list_browser.ended", endedText),
			T("history.list_browser.duration", duration),
			T("history.list_browser.status", run.Status),
			T("history.list_browser.samples", samples),
			T("history.list_browser.interval", interval),
			T("history.list_browser.process", process),
			T("history.list_browser.interfaces", interfaces),
		}
		data.entries = append(data.entries, historyReviewEntry{id: run.ID, cells: []string{historyRelativeTime(run.StartedAt, now), run.Command, duration, fmt.Sprint(samples), run.Status, run.Hostname}, details: details})
	}
	if err := rows.Err(); err != nil {
		return historyReviewData{}, err
	}
	return data, nil
}

func newHistoryListBrowser(path string, data historyReviewData) historyReviewBrowser {
	columns := []historyReviewColumn{{T("history.list_browser.column.start"), 0, 0}, {T("history.list_browser.column.command"), 12, 0}, {T("history.list_browser.column.duration"), 10, 80}, {T("history.list_browser.column.samples"), 8, 100}, {T("history.list_browser.column.status"), 12, 120}, {T("history.list_browser.column.host"), 22, 160}}
	return newHistoryReviewBrowser(T("history.list_browser.title"), path, columns, data)
}

func runHistoryListBrowser(db *sql.DB, path string, options historyOptions) error {
	now := time.Now()
	data, err := historyListReviewData(db, options, now)
	if err != nil {
		return err
	}
	model := newHistoryListBrowser(path, data)
	model.load = func() (historyReviewData, error) { return historyListReviewData(db, options, time.Now()) }
	_, err = tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	return err
}
