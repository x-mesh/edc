package edc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func browserGroups(count int) []logHistoryGroup {
	groups := make([]logHistoryGroup, count)
	for i := range groups {
		row := logHistoryAttempt{Key: fmt.Sprintf("%064x", i+1), Command: []string{"ls", fmt.Sprint(i)}, CWD: "/tmp", Path: "fixture.log", Started: time.Now(), Ended: time.Now(), Outcome: "SUCCESS", Duration: time.Millisecond, Attempt: 1}
		groups[i] = logHistoryGroup{Key: row.Key, Representative: row, Rows: []logHistoryAttempt{row}}
	}
	return groups
}

func historyBrowserAfter(t *testing.T, model logHistoryBrowser, keys ...string) logHistoryBrowser {
	t.Helper()
	for _, key := range keys {
		updated, _ := pressKey(model, key)
		model = updated.(logHistoryBrowser)
	}
	return model
}

func TestLogHistoryBrowserSelectionDetailAndBack(t *testing.T) {
	groups := browserGroups(40)
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	for range 30 {
		model = historyBrowserAfter(t, model, "down")
	}
	if model.cursor != 30 || model.list.YOffset() == 0 {
		t.Fatal("selection did not scroll")
	}
	offset := model.list.YOffset()
	model = historyBrowserAfter(t, model, "enter")
	if !model.detail || !strings.Contains(model.View().Content, historyCommand(groups[30].Representative)) || strings.Contains(model.View().Content, groups[30].Key) {
		t.Fatal("detail selected wrong full key")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !model.expanded || !strings.Contains(model.View().Content, groups[30].Key) {
		t.Fatal("run details lost full identity")
	}
	model = historyBrowserAfter(t, model, "esc")
	if model.expanded || !model.detail {
		t.Fatal("details did not return to runs")
	}
	model = historyBrowserAfter(t, model, "esc")
	if model.detail || model.cursor != 30 || model.list.YOffset() != offset {
		t.Fatal("back lost selection")
	}
	model = historyBrowserAfter(t, model, "enter", "b")
	if model.detail || model.cursor != 30 {
		t.Fatal("b did not return to keys")
	}
	_, cmd := pressKey(model, "q")
	if cmd == nil {
		t.Fatal("q did not quit")
	}
}

func TestLogHistoryBrowserResizeEmptyAndSafeOutput(t *testing.T) {
	groups := browserGroups(2)
	groups[0].Representative.Command = []string{"echo", "秘密\n\x1b[2J"}
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, []string{"partial"})
	for _, size := range []tea.WindowSizeMsg{{Width: 80, Height: 24}, {Width: 40, Height: 9}, {Width: 20, Height: 4}, {Width: 80, Height: 24}} {
		next, _ := model.Update(size)
		model = next.(logHistoryBrowser)
		view := model.View().Content
		if strings.Contains(view, "\x1b[2J") || len(strings.Split(view, "\n")) > size.Height {
			t.Fatalf("unsafe/oversized view: %q", view)
		}
		for _, line := range strings.Split(view, "\n") {
			if liveWidth(line) > size.Width {
				t.Fatalf("line exceeds width: %q", line)
			}
		}
	}
	empty := newLogHistoryBrowser(nil, logHistoryOptions{Limit: 20}, nil)
	empty = historyBrowserAfter(t, empty, "down", "enter", "up")
	if empty.detail || !strings.Contains(empty.View().Content, T("cli.log_history.no_match")) {
		t.Fatal("empty model failed")
	}
	_, cmd := pressKey(empty, "esc")
	if cmd == nil {
		t.Fatal("esc did not quit")
	}
}

func TestLogHistoryBrowserCompactEnglishAndColor(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	groups := browserGroups(1)
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, []string{"2 runs excluded: arguments unavailable"})
	model.color = true
	model.refreshList()
	resized, _ := model.Update(tea.WindowSizeMsg{Width: 200, Height: 80})
	model = resized.(logHistoryBrowser)
	view := model.View().Content
	if len(strings.Split(view, "\n")) > 8 || strings.Contains(view, groups[0].Key) || strings.Contains(view, "0 unknown") || !strings.Contains(view, "Command") || !strings.Contains(view, "Last run") {
		t.Fatalf("not compact: %q", view)
	}
	if !strings.Contains(view, "\x1b[38;5;81") || !strings.Contains(view, "\x1b[48;5;236m") || !strings.Contains(view, "\x1b[33m") {
		t.Fatal("selection and notice colors missing")
	}
	for _, line := range strings.Split(view, "\n") {
		if liveWidth(line) > 96 {
			t.Fatal("content stretched across wide terminal")
		}
	}
	model = historyBrowserAfter(t, model, "enter")
	view = model.View().Content
	if !strings.Contains(view, "✓ PASS") || !strings.Contains(view, "\x1b[32m") || strings.Contains(view, "fixture.log") || strings.Contains(view, "key ") {
		t.Fatalf("run overview cluttered or uncolored: %q", view)
	}
	if !strings.Contains(view, "1 passed") || !strings.Contains(view, "success avg") {
		t.Fatal("compact success summary missing")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !strings.Contains(model.View().Content, "fixture.log") {
		t.Fatal("source file unavailable in details")
	}
}

func TestLogHistoryBrowserNoColorAndNarrowResults(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	groups := browserGroups(2)
	groups[0].Rows[0].Outcome = "FAIL"
	groups[0].Failed = 1
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	if model.color || strings.Contains(model.View().Content, "\x1b[") {
		t.Fatal("NO_COLOR emitted ANSI styles")
	}
	resized, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 9})
	model = resized.(logHistoryBrowser)
	model = historyBrowserAfter(t, model, "enter")
	view := model.View().Content
	if !strings.Contains(view, historyOutcome(groups[0].Rows[0])) {
		t.Fatalf("narrow view hid result: %q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if liveWidth(line) > 40 {
			t.Fatal("narrow row overflow")
		}
	}
}

func TestLogHistoryRelativeDateAndColumnAlignment(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local)
	if historyRelativeTime(now, now) != "Today 10:00" || historyRelativeTime(now.AddDate(0, 0, -1), now) != "Yesterday 10:00" {
		t.Fatal("relative dates incorrect")
	}
	groups := browserGroups(2)
	groups[1].Representative.Command = []string{"echo", "秘密 a long argument"}
	groups[0].Unknown = 1
	header := historyKeyHeader(80, groups)
	for _, group := range groups {
		row := historyKeyRow(group, 80, groups, false, false, now)
		if liveWidth(row) != liveWidth(header) || liveWidth(row) != 80 {
			t.Fatalf("columns misaligned: %q", row)
		}
	}
}

func TestLogHistoryBrowserFailureAndUnknownVisibility(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	groups := browserGroups(1)
	groups[0].Rows[0].Outcome = "FAIL"
	groups[0].Failed = 1
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model.color = true
	model.refreshList()
	if !strings.Contains(model.View().Content, "\x1b[31;1m") {
		t.Fatal("nonzero failure count is not emphasized")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !strings.Contains(model.View().Content, "✗ FAIL") || !strings.Contains(model.View().Content, "\x1b[31;1m") {
		t.Fatal("failure result is not visible in red")
	}
	groups[0].Rows[0].Outcome = "UNKNOWN"
	groups[0].Unknown = 1
	groups[0].Failed = 0
	model = newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model.color = true
	model.refreshList()
	if !strings.Contains(model.View().Content, "Unknown") || !strings.Contains(model.View().Content, "\x1b[33m") {
		t.Fatal("unknown records are not exposed")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !strings.Contains(model.View().Content, "UNKNOWN") || strings.Contains(model.View().Content, "success avg") {
		t.Fatal("unknown record appears successful")
	}
}
