package edc

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// historyTrendBrowserAfter는 키를 누르고, 키가 낸 불러오기를 바로 실행해 화면에 넣는다.
func historyTrendBrowserAfter(t *testing.T, model historyTrendBrowser, keys ...string) historyTrendBrowser {
	t.Helper()
	for _, key := range keys {
		updated, cmd := pressKey(model, key)
		model = updated.(historyTrendBrowser)
		if cmd != nil {
			if msg, ok := cmd().(historyTrendLoadedMsg); ok {
				next, _ := model.Update(msg)
				model = next.(historyTrendBrowser)
			}
		}
	}
	return model
}

func historyTrendBrowserView(model historyTrendBrowser) string {
	return ansi.Strip(model.View().Content)
}

func TestHistoryTrendBrowserTargetsMetricsAndCommand(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	db, nine := historyProcessTrendFixture(t)
	targets, err := historyTrendTargets(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0].kind != historyTrendHost || targets[1].value != "pg" || targets[1].rss != 130 || targets[2].value != "nginx" {
		t.Fatalf("targets = %+v", targets)
	}
	model := newHistoryTrendBrowser(db, targets, historyTrendOptions{by: "hour"}, historyTrendZone)
	model.now = nine.Add(24 * time.Hour)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	model = updated.(historyTrendBrowser)
	loaded, _ := model.Update(model.Init()())
	model = loaded.(historyTrendBrowser)
	if view := historyTrendBrowserView(model); !strings.Contains(view, "cpu_pct by hour") || !strings.Contains(view, "No values in this range") || !strings.Contains(view, "›host") || !strings.Contains(view, "TARGET") || !strings.Contains(view, "PEAK MEMORY") {
		t.Fatalf("host view:\n%s", view)
	}

	model = historyTrendBrowserAfter(t, model, "down")
	view := historyTrendBrowserView(model)
	for _, want := range []string{"rss_bytes of pg by hour", "RESTARTS", "75%", "█", "without --process", "RSS change: too few samples (2)", "›pg"} {
		if !strings.Contains(view, want) {
			t.Fatalf("process view lacks %q:\n%s", want, view)
		}
	}

	model = historyTrendBrowserAfter(t, model, "e", "m", "b")
	view = historyTrendBrowserView(model)
	if !strings.Contains(view, "memory_bytes of pg by day") || !strings.Contains(view, "STARTED") {
		t.Fatalf("each view:\n%s", view)
	}
	command := historyTrendBrowserCommand(model.trendOptions(), []string{"--by", "hour", "--metric=x", "--each", "--from", "2026-10-01T00:00:00Z", "my.db"})
	if command != "edc history trend --process pg --metric memory_bytes --by day --each --from 2026-10-01T00:00:00Z my.db" {
		t.Fatalf("command = %q", command)
	}

	// 호스트로 돌아가면 프로세스별 보기는 꺼지고, 호스트 지표는 전에 고른 값을 다시 쓴다.
	model = historyTrendBrowserAfter(t, model, "up", "M")
	if options := model.trendOptions(); options.each || options.process.active() || options.metric == "cpu_pct" || options.metric == "memory_bytes" {
		t.Fatalf("host options = %+v", options)
	}
	if command := historyTrendBrowserCommand(model.trendOptions(), nil); strings.Contains(command, "--each") || strings.Contains(command, "--process") {
		t.Fatalf("host command = %q", command)
	}
}

func TestHistoryTrendBrowserLocalizesPeakRSSAndShowsFullCommand(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	restore := currentLanguage()
	defer setLanguage(restore)
	target := historyTrendTarget{kind: historyTrendProcess, value: "/Applications/OrbStack Helper", rss: 1 << 30}
	model := newHistoryTrendBrowser(nil, []historyTrendTarget{target}, historyTrendOptions{by: "day"}, historyTrendZone)
	for language, peak := range map[string]string{"en": "PEAK MEMORY", "ko": "최대 메모리", "ja": "最大メモリ"} {
		setLanguage(language)
		for _, width := range []int{80, 120, 180} {
			updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 20})
			view := historyTrendBrowserView(updated.(historyTrendBrowser))
			if !strings.Contains(view, T("history.browser.targets")) || !strings.Contains(view, peak) || !strings.Contains(view, "OrbStack Helper") || !strings.Contains(view, "1.0 GB") || !strings.Contains(view, "/Applications/OrbStack Helper") {
				t.Fatalf("%s width %d view:\n%s", language, width, view)
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > width {
					t.Fatalf("%s width %d line %d: %q", language, width, ansi.StringWidth(line), line)
				}
			}
		}
	}
}

func TestHistoryTrendBrowserDisambiguatesDuplicateExecutableNames(t *testing.T) {
	model := newHistoryTrendBrowser(nil, []historyTrendTarget{
		{kind: historyTrendProcess, value: "/Applications/Claude.app/Contents/Helpers/Helper"},
		{kind: historyTrendProcess, value: "/Applications/Claude.app/Contents/Renderer/Helper"},
	}, historyTrendOptions{}, historyTrendZone)
	first, second := model.targetLabel(0, historyTrendTargetWidth), model.targetLabel(1, historyTrendTargetWidth)
	if first == second || strings.Contains(first, "PID") || strings.Contains(second, "PID") || !strings.Contains(first, "Helpers") || !strings.Contains(second, "Renderer") {
		t.Fatalf("duplicate labels = %q, %q", first, second)
	}
	spaced := newHistoryTrendBrowser(nil, []historyTrendTarget{
		{kind: historyTrendProcess, value: "/Applications/Google Chrome.app/Contents/Helpers/Chrome Helper"},
		{kind: historyTrendProcess, value: "/Applications/Claude Desktop.app/Contents/Helpers/Chrome Helper"},
	}, historyTrendOptions{}, historyTrendZone)
	first, second = spaced.targetLabel(0, historyTrendTargetWidth), spaced.targetLabel(1, historyTrendTargetWidth)
	if first == second || !strings.Contains(first, "· Google") || !strings.Contains(second, "· Claude") {
		t.Fatalf("labels for paths with spaces = %q, %q", first, second)
	}
	for _, commands := range [][]string{
		{"/a/x/bin/worker", "/b/x/bin/worker", "/c/y/bin/worker"},
		{"/a/x/bin/worker", "/b/x/bin/worker", "/c/y/bin/worker", "/d/y/bin/worker"},
	} {
		targets := make([]historyTrendTarget, len(commands))
		for index, command := range commands {
			targets[index] = historyTrendTarget{kind: historyTrendProcess, value: command}
		}
		model := newHistoryTrendBrowser(nil, targets, historyTrendOptions{}, historyTrendZone)
		labels := map[string]bool{}
		for index := range targets {
			label := model.targetLabel(index, historyTrendTargetWidth)
			if labels[label] {
				t.Fatalf("duplicate labels for %v: %q", commands, label)
			}
			labels[label] = true
		}
	}
}
func TestHistoryTrendBrowserStartsAtNamedProcessAndFitsSmallScreens(t *testing.T) {
	db, _ := historyProcessTrendFixture(t)
	targets, _ := historyTrendTargets(db)
	redis, _ := parseTopProcessFilter("redis")
	model := newHistoryTrendBrowser(db, targets, historyTrendOptions{by: "day", process: redis}, historyTrendZone)
	if model.cursor != 0 || model.target().value != "redis" || len(model.targets) != 4 {
		t.Fatalf("targets = %+v, cursor = %d", model.targets, model.cursor)
	}
	nginx, _ := parseTopProcessFilter("NGINX")
	if model := newHistoryTrendBrowser(db, targets, historyTrendOptions{by: "day", process: nginx}, historyTrendZone); model.target().value != "nginx" || len(model.targets) != 3 {
		t.Fatalf("an existing target is reused: %+v", model.target())
	}
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 50, Height: 8})
	if view := historyTrendBrowserView(updated.(historyTrendBrowser)); !strings.Contains(view, "q") || strings.Count(view, "\n") > 7 {
		t.Fatalf("small view:\n%s", view)
	}
	if _, cmd := pressKey(model, "q"); cmd == nil {
		t.Fatal("q must quit")
	}
}

func TestHistoryTrendBar(t *testing.T) {
	if bar := historyTrendBar(5, 10); bar != "█████" {
		t.Fatalf("half = %q", bar)
	}
	if bar := historyTrendBar(1, 80); bar != "▏" {
		t.Fatalf("one eighth = %q", bar)
	}
	if historyTrendBar(0, 10) != "" || historyTrendBar(5, 0) != "" || historyTrendBar(20, 10) != strings.Repeat("█", historyTrendBarWidth) {
		t.Fatal("edge cases")
	}
}
