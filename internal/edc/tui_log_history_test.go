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
	t.Setenv("NO_COLOR", "")
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
	if !strings.Contains(view, liveChipBase()) || !strings.Contains(view, historySGR(richMagenta, false)+liveSelectedBar) || !strings.Contains(view, historySGR(richYellow, false)) {
		t.Fatal("selection and notice colors missing")
	}
	for _, line := range strings.Split(view, "\n") {
		if liveWidth(line) > 96 {
			t.Fatal("content stretched across wide terminal")
		}
	}
	model = historyBrowserAfter(t, model, "enter")
	view = model.View().Content
	if !strings.Contains(view, "✓ PASS") || !strings.Contains(view, historySGR(richGreen, false)) || strings.Contains(view, "fixture.log") || strings.Contains(view, "key ") {
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
	t.Setenv("NO_COLOR", "")
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	groups := browserGroups(1)
	groups[0].Rows[0].Outcome = "FAIL"
	groups[0].Failed = 1
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model.color = true
	model.refreshList()
	if !strings.Contains(model.View().Content, historySGR(richRed, true)) {
		t.Fatal("nonzero failure count is not emphasized")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !strings.Contains(model.View().Content, "✗ FAIL") || !strings.Contains(model.View().Content, historySGR(richRed, true)) {
		t.Fatal("failure result is not visible in red")
	}
	groups[0].Rows[0].Outcome = "UNKNOWN"
	groups[0].Unknown = 1
	groups[0].Failed = 0
	model = newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model.color = true
	model.refreshList()
	if !strings.Contains(model.View().Content, "Unknown") || !strings.Contains(model.View().Content, historySGR(richYellow, false)) {
		t.Fatal("unknown records are not exposed")
	}
	model = historyBrowserAfter(t, model, "enter")
	if !strings.Contains(model.View().Content, "UNKNOWN") || strings.Contains(model.View().Content, "success avg") {
		t.Fatal("unknown record appears successful")
	}
}

func TestLogHistoryBrowserRefreshKeepsSelectionByKey(t *testing.T) {
	groups := browserGroups(3)
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	fresh := append(browserGroups(5)[3:4], groups...)
	model.load = func() ([]logHistoryGroup, []string, error) { return fresh, []string{"partial"}, nil }
	if model.Init() == nil {
		t.Fatal("live browser did not schedule a refresh")
	}
	model = historyBrowserAfter(t, model, "down", "enter")
	updated, cmd := model.Update(logHistoryTickMsg{seq: model.seq})
	if cmd == nil {
		t.Fatal("tick did not reload")
	}
	updated, next := updated.Update(cmd())
	model = updated.(logHistoryBrowser)
	if next == nil || model.cursor != 2 || model.groups[model.cursor].Key != groups[1].Key || !model.detail || model.notices[0] != "partial" {
		t.Fatalf("refresh lost selection: cursor=%d detail=%v", model.cursor, model.detail)
	}
	model.load = func() ([]logHistoryGroup, []string, error) { return groups[2:], nil, nil }
	updated, _ = model.Update(model.reload()())
	model = updated.(logHistoryBrowser)
	if model.detail || model.cursor != 0 {
		t.Fatal("detail stayed open for a command that disappeared")
	}
}

func TestLogHistoryBrowserPauseIgnoresTicks(t *testing.T) {
	model := newLogHistoryBrowser(browserGroups(2), logHistoryOptions{Limit: 20}, nil)
	model.load = func() ([]logHistoryGroup, []string, error) { return nil, nil, fmt.Errorf("boom") }
	stale := model.seq
	model = historyBrowserAfter(t, model, "p")
	if !model.paused || !strings.Contains(model.View().Content, T("cli.log_history.paused")) {
		t.Fatal("pause not shown")
	}
	if _, cmd := model.Update(logHistoryTickMsg{seq: model.seq}); cmd != nil {
		t.Fatal("paused browser reloaded on tick")
	}
	if _, cmd := model.Update(logHistoryLoadedMsg{seq: stale}); cmd != nil {
		t.Fatal("stale load was applied")
	}
	updated, cmd := pressKey(model, "r")
	model = updated.(logHistoryBrowser)
	updated, _ = model.Update(cmd())
	model = updated.(logHistoryBrowser)
	if len(model.groups) != 2 || model.notices[0] != "boom" || !model.paused {
		t.Fatal("failed refresh replaced the list or resumed")
	}
}
