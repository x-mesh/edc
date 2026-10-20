package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestHistoryPruneAgePickerReloadsAndDeletesWithChosenAge(t *testing.T) {
	path, ids := historyPruneFixture(t)
	db, err := openHistoryDB(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	options := historyPruneOptions{olderThan: 90 * 24 * time.Hour}
	plan, err := planHistoryPrune(db, options, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	model := newHistoryPruneBrowser(path, options, plan, time.Now())
	model.loadPrune = func(chosen historyPruneOptions) (historyReviewData, error) {
		now := time.Now()
		plan, err := planHistoryPrune(db, chosen, now)
		return historyPruneReviewData(plan, chosen, now), err
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: 't', Text: "t"})
	if model.agePicker == nil || historyPruneAges[model.agePicker.selected] != options.olderThan {
		t.Fatal("picker did not select the current age")
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	next, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(historyReviewBrowser)
	if cmd == nil || model.pruneOptions.olderThan != 30*24*time.Hour || len(model.data.entries) != 0 {
		t.Fatal("preset did not reload or left stale deletion candidates")
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"}, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if model.confirmed || model.confirm {
		t.Fatal("deletion was possible while the new age was loading")
	}
	oldResult := cmd()
	model = reviewAfter(t, model, oldResult)
	if len(model.data.entries) != 1 || model.data.entries[0].id != ids["old_snapshot"] || !strings.Contains(model.data.summary, "30d") {
		t.Fatal("30d candidate list is wrong")
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: 't', Text: "t"}, tea.KeyPressMsg{Code: tea.KeyEnd}, tea.KeyPressMsg{Code: tea.KeyEnter}, tea.PasteMsg{Content: "7d"})
	next, cmd = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(historyReviewBrowser)
	model = reviewAfter(t, model, oldResult)
	if len(model.data.entries) != 0 || model.pruneOptions.olderThan != 7*24*time.Hour {
		t.Fatal("stale age response restored outdated candidates")
	}
	model = reviewAfter(t, model, cmd())
	if len(model.data.entries) != 2 || model.data.pruneOptions.olderThan != 7*24*time.Hour {
		t.Fatal("custom age was not attached to the selected candidates")
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"}, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if !model.confirmed {
		t.Fatal("chosen candidates could not be confirmed")
	}
	deleted, err := deleteEligibleHistoryRuns(db, model.data.pruneOptions, model.selectedIDs(), time.Now())
	if err != nil || len(deleted.runs) != 2 {
		t.Fatalf("deletion did not use the chosen age: %+v %v", deleted, err)
	}
	left := historyRunIDs(t, path)
	if left[ids["old_top"]] || left[ids["old_snapshot"]] || !left[ids["new_snapshot"]] || !left[ids["running_top"]] {
		t.Fatal("age change deleted the wrong runs")
	}
}

func TestHistoryPruneCustomAgeValidationAndCancel(t *testing.T) {
	options := historyPruneOptions{olderThan: 90 * 24 * time.Hour, command: "top", dryRun: true, vacuum: true}
	model := newHistoryPruneBrowser("history.db", options, historyPrunePlan{}, time.Now())
	model.loadPrune = func(chosen historyPruneOptions) (historyReviewData, error) {
		return historyPruneReviewData(historyPrunePlan{}, chosen, time.Now()), nil
	}
	for _, invalid := range []string{"0d", "-7d", "bad", "1e999d"} {
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: 't', Text: "t"}, tea.KeyPressMsg{Code: tea.KeyEnd}, tea.KeyPressMsg{Code: tea.KeyEnter}, tea.PasteMsg{Content: invalid})
		next, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = next.(historyReviewBrowser)
		if cmd != nil || model.agePicker == nil || model.agePicker.err == "" || model.pruneOptions.olderThan != options.olderThan {
			t.Fatalf("invalid age was accepted: %s", invalid)
		}
		model = reviewAfter(t, model, tea.KeyPressMsg{Code: tea.KeyEscape})
		if model.agePicker != nil || model.pruneOptions != options {
			t.Fatal("cancel changed the age or other prune options")
		}
	}
	model = reviewAfter(t, model, tea.KeyPressMsg{Code: 't', Text: "t"}, tea.KeyPressMsg{Code: tea.KeyEnd}, tea.KeyPressMsg{Code: tea.KeyEnter}, tea.PasteMsg{Content: "2w"})
	next, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = next.(historyReviewBrowser)
	model = reviewAfter(t, model, cmd())
	if model.pruneOptions.olderThan != 14*24*time.Hour || !model.pruneOptions.dryRun || !model.pruneOptions.vacuum || model.pruneOptions.command != "top" {
		t.Fatal("age selection changed command/dry-run/vacuum settings")
	}
	if model.data.pruneOptions != model.pruneOptions {
		t.Fatal("refreshed candidates have mismatched pruning settings")
	}
}

func TestHistoryPruneAgePickerLayout(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		for _, color := range []bool{false, true} {
			for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 12}, {Width: 80, Height: 24}, {Width: 160, Height: 40}} {
				model := newHistoryPruneBrowser("history.db", historyPruneOptions{olderThan: 90 * 24 * time.Hour}, historyPrunePlan{}, time.Now())
				model.color = color
				model = reviewAfter(t, model, size, tea.KeyPressMsg{Code: 't', Text: "t"}, tea.KeyPressMsg{Code: tea.KeyEnd})
				for _, input := range []bool{false, true} {
					model.agePicker.input = input
					model.agePicker.text = "0d"
					if input {
						model.agePicker.err = T("history.review.age_invalid")
					}
					lines := strings.Split(model.View().Content, "\n")
					if len(lines) != size.Height || !strings.Contains(model.View().Content, T("history.review.age_title")) {
						t.Fatalf("picker layout invalid at %s/%+v", language, size)
					}
					for _, line := range lines {
						if liveWidth(line) > size.Width || !color && strings.Contains(line, "\x1b[") {
							t.Fatalf("picker line too wide or unexpectedly colored: %q", line)
						}
					}
				}
			}
		}
	}
}
