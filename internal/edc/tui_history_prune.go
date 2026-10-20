package edc

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

func historyPruneReviewData(plan historyPrunePlan, options historyPruneOptions, now time.Time) historyReviewData {
	data := historyReviewData{plan: plan, pruneOptions: options, summary: T("history.review.prune_range", now.Add(-options.olderThan).Local().Format(time.DateTime), len(plan.runs), plan.samples), empty: T("history.prune.none")}
	data.summary = T("history.review.age_current", historyPruneAgeLabel(options.olderThan)) + " · " + data.summary
	if options.vacuum {
		data.summary += " · VACUUM"
	}
	for _, entry := range plan.entries {
		run := entry.run
		ended := "—"
		if run.EndedAt != nil {
			ended = run.EndedAt.Local().Format(time.DateTime)
		}
		data.entries = append(data.entries, historyReviewEntry{id: run.ID,
			cells:   []string{run.Command + " " + run.ID, entry.lastActivity.Local().Format(time.DateTime), fmt.Sprint(entry.samples), run.Hostname, run.Status},
			details: []string{historyTerminalText(run.Command) + " · " + historyTerminalText(run.ID), historyTerminalText(run.Hostname), "", "started  " + run.StartedAt.Local().Format(time.DateTime), "ended  " + ended, "last active  " + entry.lastActivity.Local().Format(time.DateTime), "status  " + historyTerminalText(run.Status), fmt.Sprintf("samples/results  %d", entry.samples), "", T("history.review.prune_details")}})
	}
	return data
}

func selectedHistoryPrunePlan(plan historyPrunePlan, selected []string) historyPrunePlan {
	ids := map[string]bool{}
	for _, id := range selected {
		ids[id] = true
	}
	var chosen historyPrunePlan
	for _, entry := range plan.entries {
		if ids[entry.run.ID] {
			chosen.add(entry)
		}
	}
	return chosen
}

func newHistoryPruneBrowser(path string, options historyPruneOptions, plan historyPrunePlan, now time.Time) historyReviewBrowser {
	columns := []historyReviewColumn{{"RUN", 0, 0}, {"LAST ACTIVE", 19, 0}, {"SAMPLES", 8, 72}, {"HOST", 22, 112}, {"STATUS", 12, 144}}
	model := newHistoryReviewBrowser(T("history.review.prune_title"), path, columns, historyPruneReviewData(plan, options, now))
	model.prune, model.dryRun, model.pruneOptions = true, options.dryRun, options
	for _, entry := range plan.entries {
		model.checked[entry.run.ID] = true
	}
	model.resize()
	return model
}

func runHistoryPruneBrowser(input, output *os.File, db *sql.DB, path string, options historyPruneOptions, plan historyPrunePlan) (historyPrunePlan, historyPruneOptions, bool, error) {
	model := newHistoryPruneBrowser(path, options, plan, time.Now())
	model.loadPrune = func(options historyPruneOptions) (historyReviewData, error) {
		now := time.Now()
		plan, err := planHistoryPrune(db, options, now)
		if err != nil {
			return historyReviewData{}, err
		}
		return historyPruneReviewData(plan, options, now), nil
	}
	result, err := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return historyPrunePlan{}, options, false, err
	}
	final := result.(historyReviewBrowser)
	return selectedHistoryPrunePlan(final.data.plan, final.selectedIDs()), final.data.pruneOptions, final.confirmed && !options.dryRun, nil
}
