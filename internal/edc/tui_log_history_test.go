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
		var updated tea.Model
		if code, ok := map[string]rune{"home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown}[key]; ok {
			updated, _ = model.Update(tea.KeyPressMsg{Code: code})
		} else {
			updated, _ = pressKey(model, key)
		}
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

func TestLogHistoryBrowserResponsiveEnglishAndColor(t *testing.T) {
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
	if len(strings.Split(view, "\n")) != 80 || strings.Contains(view, groups[0].Key) || strings.Contains(view, "0 unknown") || !strings.Contains(view, "Command") || !strings.Contains(view, "Last run") {
		t.Fatalf("not compact: %q", view)
	}
	if !strings.Contains(view, liveChipBase()) || !strings.Contains(view, historySGR(richMagenta, false)+liveSelectedBar) || !strings.Contains(view, historySGR(richYellow, false)) {
		t.Fatal("selection and notice colors missing")
	}
	for _, line := range strings.Split(view, "\n") {
		if liveWidth(line) > 200 {
			t.Fatal("content exceeds terminal width")
		}
	}
	model = historyBrowserAfter(t, model, "enter")
	view = model.View().Content
	if !strings.Contains(view, "✓ PASS") || !strings.Contains(view, historySGR(richGreen, false)) || !strings.Contains(view, "Selected run") || !strings.Contains(view, "fixture.log") {
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

func TestLogHistoryKeyAverageColumnsAndValues(t *testing.T) {
	if historyKeyWidths(87, nil).avgMemory != 0 || historyKeyWidths(103, nil).avgCPU != 0 || historyKeyWidths(119, nil).avgIO != 0 {
		t.Fatal("average columns appear before their width thresholds")
	}
	if historyKeyWidths(88, nil).avgMemory != 11 || historyKeyWidths(104, nil).avgCPU != 8 || historyKeyWidths(120, nil).avgIO != 9 {
		t.Fatal("average columns do not appear at their width thresholds")
	}
	ended := time.Date(2026, 10, 5, 10, 0, 2, 0, time.Local)
	group := logHistoryGroup{Rows: []logHistoryAttempt{
		{Duration: 2 * time.Second, Ended: ended, Resources: &logHistoryResources{MaxRSS: 100, CPU: time.Second, cpu: true, DiskRead: 100, DiskWrite: 300, disk: true}},
		{Duration: 4 * time.Second, Ended: ended, Resources: &logHistoryResources{MaxRSS: 300, CPU: time.Second, cpu: true, DiskRead: 300, DiskWrite: 100, disk: true}},
		{Duration: 0, Resources: &logHistoryResources{MaxRSS: 900, CPU: time.Second, cpu: true, DiskRead: 900, disk: true}},
		{Duration: 2 * time.Second, Ended: ended, Resources: &logHistoryResources{MaxRSS: 0, CPU: time.Second, cpu: true}},
		{Duration: 2 * time.Second, Ended: ended, damaged: true, Resources: &logHistoryResources{CPU: 10 * time.Second, cpu: true}},
		{Duration: 2 * time.Second},
	}}
	averages := historyGroupAverages(group)
	if averages.memory != formatSizeShort(433) || averages.cpu != "42%" || averages.io != formatSizeShort(566) {
		t.Fatalf("unexpected averages: %#v", averages)
	}
	empty := historyGroupAverages(logHistoryGroup{Rows: []logHistoryAttempt{{Duration: 0, Resources: &logHistoryResources{}}}})
	if empty != (historyGroupAverageValues{}) {
		t.Fatalf("missing metrics are not empty: %#v", empty)
	}
}

func TestLogHistoryKeyAverageHeadersFitInEveryLanguage(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		header := historyKeyHeader(220, nil)
		for _, key := range []string{"avg_memory", "avg_cpu", "avg_io"} {
			if label := T("cli.log_history.column." + key); !strings.Contains(header, label) {
				t.Fatalf("%s header clips %q: %q", language, label, header)
			}
		}
	}
}

func TestLogHistoryKeyAverageColumnWidths(t *testing.T) {
	group := logHistoryGroup{Rows: []logHistoryAttempt{{Started: time.Now()}}}
	for _, width := range []int{88, 104, 120, 220} {
		groups := []logHistoryGroup{group}
		header := historyKeyHeader(width, groups)
		row := historyKeyRow(group, width, groups, false, false, time.Now())
		if liveWidth(header) != width || liveWidth(row) != width {
			t.Fatalf("width %d mismatch: header=%d row=%d", width, liveWidth(header), liveWidth(row))
		}
		group.Unknown = 1
		header = historyKeyHeader(width, []logHistoryGroup{group})
		row = historyKeyRow(group, width, []logHistoryGroup{group}, false, false, time.Now())
		if liveWidth(header) != width || liveWidth(row) != width {
			t.Fatalf("unknown width %d mismatch: header=%d row=%d", width, liveWidth(header), liveWidth(row))
		}
		group.Unknown = 0
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

func TestLogHistoryBrowserFillsTerminalAndAdaptsDetails(t *testing.T) {
	groups := browserGroups(1)
	row := groups[0].Rows[0]
	row.Command = []string{"curl", "https://example.com/" + strings.Repeat("long-path/", 40)}
	groups[0].Representative = row
	groups[0].Rows = make([]logHistoryAttempt, 100)
	for i := range groups[0].Rows {
		groups[0].Rows[i] = row
		groups[0].Rows[i].Started = row.Started.Add(-time.Duration(i) * time.Minute)
	}
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model = historyBrowserAfter(t, model, "enter")
	for _, color := range []bool{false, true} {
		model.color = color
		for _, size := range []tea.WindowSizeMsg{{Width: 160, Height: 50}, {Width: 80, Height: 24}, {Width: 40, Height: 9}, {Width: 132, Height: 16}, {Width: 200, Height: 60}} {
			selected := model.rows()[model.rowCursor].Started
			next, _ := model.Update(size)
			model = next.(logHistoryBrowser)
			if !model.rows()[model.rowCursor].Started.Equal(selected) {
				t.Fatalf("resize lost selected run at %+v", size)
			}
			lines := strings.Split(model.View().Content, "\n")
			if len(lines) != size.Height {
				t.Fatalf("layout height %d, want %d at %+v", len(lines), size.Height, size)
			}
			for _, line := range lines {
				if liveWidth(line) > size.Width {
					t.Fatalf("line exceeds %+v: %q", size, line)
				}
			}
			if !strings.Contains(lines[len(lines)-1], "q") {
				t.Fatalf("quit hint lost at %+v: %q", size, lines[len(lines)-1])
			}
			if model.splitDetail() != strings.Contains(model.View().Content, T("cli.log_history.selected_run")) {
				t.Fatalf("detail panel breakpoint wrong at %+v", size)
			}
			if size.Height >= 50 && model.history.Height() <= 20 {
				t.Fatalf("tall terminal still limited to 20 rows: %d", model.history.Height())
			}
			model = historyBrowserAfter(t, model, "end")
			if model.rowCursor < model.history.YOffset() || model.rowCursor >= model.history.YOffset()+model.history.Height() {
				t.Fatal("selected run is outside the viewport")
			}
		}
	}
	model = historyBrowserAfter(t, model, "enter")
	if strings.Contains(model.View().Content, T("cli.log_history.selected_run")) || model.history.Width() != model.width {
		t.Fatal("expanded details did not use the full terminal width")
	}
}

func TestLogHistoryBrowserPreservesExplicitLimit(t *testing.T) {
	options, err := parseLogHistoryOptions([]string{"--limit", "3"}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	groups := browserGroups(1)
	row := groups[0].Rows[0]
	groups[0].Rows = make([]logHistoryAttempt, 100)
	for i := range groups[0].Rows {
		groups[0].Rows[i] = row
	}
	model := newLogHistoryBrowser(groups, options, nil)
	next, _ := model.Update(tea.WindowSizeMsg{Width: 160, Height: 60})
	model = next.(logHistoryBrowser)
	if len(model.rows()) != 3 {
		t.Fatalf("explicit limit changed to %d", len(model.rows()))
	}
	model = historyBrowserAfter(t, model, "enter")
	model.confirm = &logHistoryDeletion{Runs: 1, Files: 1, Bytes: 1024}
	model.resize()
	if !strings.Contains(model.View().Content, T("cli.log_history.delete_confirm", 1, 1, formatSizeShort(model.confirm.Bytes))) {
		t.Fatal("deletion prompt is missing")
	}
	model = historyBrowserAfter(t, model, "n")
	if model.confirm != nil || len(strings.Split(model.View().Content, "\n")) != 60 {
		t.Fatal("canceling deletion did not restore the layout")
	}
}

func TestLogHistoryBrowserRefreshAfterChangingCommand(t *testing.T) {
	groups := browserGroups(2)
	row := groups[0].Rows[0]
	for i := 1; i < 5; i++ {
		older := row
		older.Started = row.Started.Add(-time.Duration(i) * time.Minute)
		groups[0].Rows = append(groups[0].Rows, older)
	}
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model = historyBrowserAfter(t, model, "enter", "down", "down", "down", "esc", "down")
	if model.detail || model.rowCursor != 3 || model.cursor != 1 {
		t.Fatal("did not reproduce a stale run cursor after changing commands")
	}
	model.apply(logHistoryLoadedMsg{groups: groups})
	if model.detail || model.cursor != 1 || model.rowCursor != 0 {
		t.Fatal("refresh did not preserve the selected command and normalize its run cursor")
	}
	model = historyBrowserAfter(t, model, "enter")
	if model.rows()[model.rowCursor].Key != groups[1].Key {
		t.Fatal("opening history selected a run from the previous command")
	}
}

func TestLogHistoryBrowserFollowsLatestUntilSelectionMoves(t *testing.T) {
	groups := browserGroups(1)
	model := newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil)
	model = historyBrowserAfter(t, model, "enter")
	prepend := func() {
		group := model.groups[0]
		row := group.Rows[0]
		row.Started = row.Started.Add(time.Minute)
		group.Rows = append([]logHistoryAttempt{row}, group.Rows...)
		model.apply(logHistoryLoadedMsg{groups: []logHistoryGroup{group}})
	}
	prepend()
	if model.rowCursor != 0 || model.rowPinned {
		t.Fatal("untouched selection did not follow the latest run")
	}
	model = historyBrowserAfter(t, model, "down")
	selected := model.rows()[model.rowCursor].Started
	prepend()
	if model.rowCursor != 2 || !model.rows()[model.rowCursor].Started.Equal(selected) {
		t.Fatal("refresh moved a manually selected run")
	}
	model = historyBrowserAfter(t, model, "home")
	prepend()
	if model.rowCursor != 0 || model.rowPinned || model.history.YOffset() != 0 {
		t.Fatal("Home did not resume following the latest run")
	}
	model = historyBrowserAfter(t, model, "enter")
	selected = model.rows()[model.rowCursor].Started
	prepend()
	if !model.expanded || !model.rows()[model.rowCursor].Started.Equal(selected) {
		t.Fatal("refresh changed the run while inspecting expanded details")
	}
	model = historyBrowserAfter(t, model, "esc", "esc", "enter")
	prepend()
	if model.rowPinned || model.rowCursor != 0 {
		t.Fatal("reopening command history did not resume following")
	}
}
