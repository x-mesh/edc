package edc

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func overviewBrowserFixture() historyOverviewBrowser {
	return historyOverviewBrowser{width: 80, height: 10, overview: historyOverview{Runs: 1, Samples: 4, Memory: "RSS", Series: map[string][]float64{"cpu_pct": {1, 2, 3}}, Metrics: []historyOverviewStat{{Metric: "cpu_pct", Avg: 10, P95: 20, Max: 30}, {Metric: "memory_pct", Avg: 20, P95: 30, Max: 40}, {Metric: "load1", Avg: 1, P95: 2, Max: 3}}, Processes: []historyOverviewProcess{{Command: "/usr/bin/worker", Peak: 1 << 20, CPU: 12.5, Restarts: 1}}}}
}

func TestHistoryOverviewBrowserFitsAndScrolls(t *testing.T) {
	for _, width := range []int{80, 120, 180} {
		model := overviewBrowserFixture()
		updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 8})
		model = updated.(historyOverviewBrowser)
		for _, line := range strings.Split(model.View().Content, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d: %q", width, line)
			}
		}
		if !strings.Contains(strings.Join(model.lines(), "\n"), strings.ToUpper(T("history.overview.restarts"))) {
			t.Fatalf("width %d hides the restart heading", width)
		}
		updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(historyOverviewBrowser)
		if model.atTop() {
			t.Fatalf("width %d did not scroll", width)
		}
		if !strings.Contains(strings.Join(model.lines(), "\n"), "▁▅█") {
			t.Fatal("sparkline missing")
		}
	}
}

func TestHistoryOverviewBrowserShowsEmptyError(t *testing.T) {
	model := overviewBrowserFixture()
	model.err = errors.New(T("history.overview.empty"))
	if !strings.Contains(model.View().Content, T("history.overview.empty")) {
		t.Fatal("empty error missing")
	}
}

func TestHistoryOverviewBrowserUsesRunDuration(t *testing.T) {
	model := overviewBrowserFixture()
	model.overview.Run = &historyRun{ID: "run", Command: "top", Hostname: "host", StartedAt: time.Unix(0, 0)}
	model.overview.From = time.Unix(0, 0)
	model.overview.To = model.overview.From.Add(15100 * time.Millisecond)
	ended := model.overview.Run.StartedAt.Add(15100 * time.Millisecond)
	model.overview.Run.EndedAt = &ended
	if !strings.Contains(strings.Join(model.lines(), "\n"), "15.1s") {
		t.Fatalf("run duration = %q", model.lines()[2])
	}
}

func TestHistoryOverviewBrowserAlignsTranslatedColumns(t *testing.T) {
	previous := currentLanguage()
	defer setLanguage(previous)
	for _, language := range []string{"ko", "ja"} {
		setLanguage(language)
		lines := overviewBrowserFixture().lines()
		var offsets []int
		for _, metric := range []historyOverviewStat{{Metric: "cpu_pct", Avg: 10}, {Metric: "memory_pct", Avg: 20}} {
			value := historyTrendValue(metric.Metric, metric.Avg)
			for _, line := range lines {
				if strings.HasPrefix(line, historyCell(historyOverviewMetricLabel(metric.Metric), 24, false)) {
					offsets = append(offsets, ansi.StringWidth(line[:strings.Index(line, value)]))
					break
				}
			}
		}
		if len(offsets) != 2 || offsets[0] != offsets[1] {
			t.Fatalf("%s metric columns = %v", language, offsets)
		}
	}
}
