package edc

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestHistoryEvidenceNavigationKeepsRunAndWindow(t *testing.T) {
	previousLoader := historyOverviewLogLoader
	t.Cleanup(func() { historyOverviewLogLoader = previousLoader })
	historyOverviewLogLoader = func() (logHistorySnapshot, error) { return logHistorySnapshot{}, nil }
	path := filepath.Join(t.TempDir(), "history.db")
	recorder := topHistoryFixture(t, path)
	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		sample := historyFixtureSample(i+1, 20, topProcess{PID: 7, Command: "worker"})
		sample.Time = from.Add(time.Duration(i) * 10 * time.Minute)
		if err := recorder.Record(sample); err != nil {
			t.Fatal(err)
		}
	}
	if err := recorder.Close("finished"); err != nil {
		t.Fatal(err)
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	samples, err := loadHistoryTop(db, historyOptions{run: recorder.runID, limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	parent := newHistoryTopBrowser(samples, path)
	parent.hosted, parent.selected, parent.pinned = true, 1, true
	navigator := newHistoryNavigator(db, path, newHistoryListBrowser(path, historyReviewData{}))
	navigator.stack = append(navigator.stack, parent)
	next, _ := navigator.Update(tea.WindowSizeMsg{Width: 80, Height: 16})
	navigator = next.(historyNavigator)
	next, _ = navigator.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	navigator = next.(historyNavigator)
	child, ok := navigator.current().(historyOverviewBrowser)
	selected := samples[1]
	if !ok || !child.hosted || child.options.run != recorder.runID || !child.options.from.Equal(selected.Time.Add(-5*time.Minute)) || !child.options.to.Equal(selected.Time.Add(5*time.Minute)) || child.overview.Samples != 1 {
		t.Fatalf("wrong selected evidence window: %+v", navigator.current())
	}
	if !child.overview.From.Equal(from.Add(5*time.Minute)) || !child.overview.To.Equal(from.Add(15*time.Minute)) {
		t.Fatalf("unexpected intersected range: %s to %s", child.overview.From, child.overview.To)
	}
	next, _ = navigator.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	navigator = next.(historyNavigator)
	restored, ok := navigator.current().(historySampleBrowser)
	if !ok || len(navigator.stack) != 2 || restored.selected != 1 || restored.identity(1) != parent.identity(1) {
		t.Fatal("Esc lost parent sample identity")
	}
	parent.hosted = false
	if _, handled, err := parent.navigate("e", db, path); handled || err != nil {
		t.Fatal("standalone browser captured navigator key")
	}
	parent.hosted = true
	parent.samples = []historyTopSample{{}}
	parent.selected = 0
	if _, handled, err := parent.navigate("e", db, path); !handled || err == nil {
		t.Fatal("missing identity silently chose a current window")
	}
}

func TestHistoryEvidenceOverviewFitsAndExplainsCoverage(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	at := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	for _, lang := range supportedLanguages {
		setLanguage(lang)
		model := overviewBrowserFixture()
		model.color = false
		model.overview.From, model.overview.To = at, at.Add(time.Minute)
		model.overview.Evidence = []historyOverviewEvidence{{Time: at, Source: "log", Kind: "command", Summary: "start " + strings.Repeat("한글🙂", 40) + " LAST\x1b[2J", HostUnknown: true}}
		model.overview.EvidenceNotices = []historyEvidenceNotice{{Code: "legacy_events", Source: "events.window", RunID: "snapshot"}, {Code: "entries_limit", Detail: "200"}, {Code: "logs_host_unknown", Source: "log"}}
		var plain bytes.Buffer
		writeHistoryOverview(&plain, model.overview, time.UTC)
		text := plain.String()
		for _, want := range []string{T("history.evidence.title"), T("history.evidence.time_only"), T("history.evidence.host_unknown"), T("history.evidence.notice.legacy_events"), "LAST"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %q: %s", lang, want, text)
			}
		}
		if strings.Contains(text, "\x1b") || strings.Contains(text, "history.evidence.") || strings.Contains(text, "%!s") {
			t.Fatalf("unsafe/unresolved plain output: %q", text)
		}
		for _, width := range []int{40, 80, 140} {
			next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 9})
			model = next.(historyOverviewBrowser)
			var displayed strings.Builder
			for offset := 0; offset <= model.maxOffset(); offset += 5 {
				model.offset = offset
				content := model.View().Content
				if len(strings.Split(content, "\n")) > 9 {
					t.Fatal("overview exceeded height")
				}
				for _, line := range strings.Split(content, "\n") {
					if ansi.StringWidth(line) > width {
						t.Fatalf("%s width %d overflow: %q", lang, width, line)
					}
				}
				displayed.WriteString(content)
			}
			model.offset = model.maxOffset()
			displayed.WriteString(model.View().Content)
			if !strings.Contains(strings.ReplaceAll(displayed.String(), "\n", ""), "LAST") {
				t.Fatalf("%s width %d dropped the end of wrapped evidence", lang, width)
			}
		}
		payload, _ := json.Marshal(model.overview)
		if !strings.Contains(string(payload), `"host_unknown":true`) || !strings.Contains(string(payload), `"evidence_notices"`) {
			t.Fatalf("scope missing from JSON: %s", payload)
		}
	}
}

func TestHistoryEvidenceKeepsLogNavigation(t *testing.T) {
	previous := historyOverviewLogBrowserFor
	t.Cleanup(func() { historyOverviewLogBrowserFor = previous })
	groups := browserGroups(1)
	row := groups[0].Rows[0]
	historyOverviewLogBrowserFor = func() (logHistoryBrowser, error) {
		return newLogHistoryBrowser(groups, logHistoryOptions{Limit: 20}, nil), nil
	}
	model := overviewBrowserFixture()
	model.hosted = true
	model.overview.Evidence = []historyOverviewEvidence{{Time: row.Started, Source: "log", Kind: "command", Summary: "recorded command", HostUnknown: true, LogKey: row.Key, LogPath: row.Path}}
	model.overview.Logs = []historyOverviewLog{{Started: row.Started, Key: row.Key, Path: row.Path}}
	next, handled, err := model.navigate("l", nil, "history.db")
	if err != nil || !handled {
		t.Fatalf("log list navigation: %v", err)
	}
	list, ok := next.(historyOverviewLogList)
	if !ok || len(list.logs) != 1 {
		t.Fatalf("log list: %T", next)
	}
	next, handled, err = list.navigate("enter", nil, "history.db")
	if err != nil || !handled {
		t.Fatalf("saved log navigation: %v", err)
	}
	browser, ok := next.(logHistoryBrowser)
	if !ok || !browser.detail || !browser.rows()[browser.rowCursor].Started.Equal(row.Started) || browser.rows()[browser.rowCursor].Path != row.Path {
		t.Fatal("evidence section changed saved log identity")
	}
}
