package edc

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func svcBrowserFixture(t *testing.T) (svcBrowser, svcSources) {
	t.Helper()
	var asked []string
	sources := svcListFakeSources(&asked)
	model := newSvcBrowser(sources, time.Second, false, false)
	items, err := collectSvcList(context.Background(), sources, false)
	if err != nil {
		t.Fatal(err)
	}
	next, cmd := model.Update(svcListMsg{seq: model.seq, items: items, at: time.Unix(0, 0)})
	if cmd == nil {
		t.Fatal("a list must schedule the next refresh")
	}
	return next.(svcBrowser), sources
}

func svcBrowserKeys(t *testing.T, model svcBrowser, keys ...string) (svcBrowser, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, key := range keys {
		var next tea.Model
		next, cmd = model.Update(topKey(key))
		model = next.(svcBrowser)
	}
	return model, cmd
}

func TestSvcBrowserOpensADetailAndGoesBack(t *testing.T) {
	model, _ := svcBrowserFixture(t)
	if content := model.View().Content; !strings.Contains(content, "5 services · 1 failed · 1 starting or restarting") || !strings.Contains(content, "› web.service") {
		t.Fatalf("list:\n%s", content)
	}
	model, cmd := svcBrowserKeys(t, model, "down", "down", "enter")
	if model.detail == nil || model.detail.unit != "Postgres.service" || !model.detail.loading || cmd == nil {
		t.Fatalf("enter opens the selected unit: %+v", model.detail)
	}
	if !strings.Contains(model.View().Content, "Postgres.service · loading…") {
		t.Fatalf("loading:\n%s", model.View().Content)
	}
	// 상세는 edc svc -v <unit>과 같은 결과다.
	next, _ := model.Update(cmd())
	model = next.(svcBrowser)
	if content := model.View().Content; model.detail.loading || !strings.Contains(content, "svc.state") || !strings.Contains(content, "Esc back") {
		t.Fatalf("detail:\n%s", content)
	}
	// 늦게 온 이전 상세는 지금 연 상세를 덮지 않는다.
	model, reload := svcBrowserKeys(t, model, "r")
	stale := svcDetailMsg{seq: model.detail.seq - 1, lines: []string{"stale"}}
	next, _ = model.Update(stale)
	if model = next.(svcBrowser); !model.detail.loading || reload == nil {
		t.Fatal("a stale detail must not replace the reload")
	}
	model, _ = svcBrowserKeys(t, model, "esc")
	if model.detail != nil || model.selectedUnit() != "Postgres.service" {
		t.Fatalf("esc goes back to the same row: detail %v, selected %s", model.detail != nil, model.selectedUnit())
	}
}

func TestSvcBrowserFiltersAndClearsWithEsc(t *testing.T) {
	model, _ := svcBrowserFixture(t)
	model, _ = svcBrowserKeys(t, model, "/", "c", "r", "o", "n", "enter")
	if visible := model.visible(); len(visible) != 1 || visible[0].Unit != "cron.service" || !strings.Contains(model.View().Content, "filter cron · 1 shown") {
		t.Fatalf("filter:\n%s", model.View().Content)
	}
	model, _ = svcBrowserKeys(t, model, "esc")
	if model.filter != "" || len(model.visible()) != 5 {
		t.Fatalf("esc clears the filter: %q", model.filter)
	}
	model, _ = svcBrowserKeys(t, model, "f")
	if visible := model.visible(); len(visible) != 1 || visible[0].Unit != "web.service" {
		t.Fatalf("f shows the failed units: %+v", visible)
	}
	model, _ = svcBrowserKeys(t, model, "esc")
	if model.failed {
		t.Fatal("esc clears failed only")
	}
}

func TestSvcBrowserRefreshKeepsTheSelectedUnit(t *testing.T) {
	model, _ := svcBrowserFixture(t)
	model, _ = svcBrowserKeys(t, model, "down", "down", "down", "down")
	if model.selectedUnit() != "cron.service" {
		t.Fatalf("selected %s", model.selectedUnit())
	}
	// 다시 읽은 목록에서 cron이 맨 위로 올라와도 cron에 머문다.
	reordered := append([]svcListItem{model.items[4]}, model.items[:4]...)
	next, _ := model.Update(svcListMsg{seq: model.seq, items: reordered, at: time.Unix(2, 0)})
	if model = next.(svcBrowser); model.selectedUnit() != "cron.service" || model.cursor != 0 {
		t.Fatalf("after refresh: selected %s at %d", model.selectedUnit(), model.cursor)
	}
	// a는 목록을 다시 읽고, 그 전에 보낸 읽기와 tick의 결과는 버린다.
	model, cmd := svcBrowserKeys(t, model, "a")
	if !model.all || cmd == nil {
		t.Fatal("a must reload with --all")
	}
	next, _ = model.Update(svcListMsg{seq: model.seq - 1, items: nil})
	if model = next.(svcBrowser); len(model.items) != 5 {
		t.Fatal("a stale list must be dropped")
	}
	if _, cmd := model.Update(svcTickMsg{seq: model.seq - 1}); cmd != nil {
		t.Fatal("a stale tick must not read again")
	}
}

func TestSvcBrowserFitsNarrowTerminals(t *testing.T) {
	model, _ := svcBrowserFixture(t)
	for _, width := range []int{80, 60} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 10})
		view := next.(svcBrowser).View().Content
		lines := strings.Split(view, "\n")
		if len(lines) > 10 {
			t.Fatalf("width %d: %d lines", width, len(lines))
		}
		for _, line := range lines {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d: line is %d wide: %q", width, got, line)
			}
		}
		// 80열에서는 PID만 빠지고 메모리는 남는다.
		if width == 80 && (!strings.Contains(lines[2], "MEM") || strings.Contains(lines[2], "PID")) {
			t.Fatalf("80 columns header: %q", lines[2])
		}
	}
}

// 색을 쓰면 상태 칸과 고른 줄만 칠한다. 칠한 것을 걷으면 고른 줄의 표시 말고는 평문 화면과 같다.
func TestSvcBrowserColorsStatesAndTheSelection(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")
	plain, _ := svcBrowserFixture(t)
	colored := plain
	colored.color = true
	plainLines, coloredLines := plain.listLines(), colored.listLines()
	if len(plainLines) != len(coloredLines) {
		t.Fatalf("%d colored lines, want %d", len(coloredLines), len(plainLines))
	}
	for index, line := range coloredLines {
		stripped := strings.TrimRight(strings.Replace(ansi.Strip(line), liveSelectedBar+" ", "› ", 1), " ")
		if stripped != strings.TrimRight(plainLines[index], " ") {
			t.Fatalf("line %d = %q, want %q", index, stripped, plainLines[index])
		}
	}
	view := strings.Join(coloredLines, "\n")
	for _, want := range []string{richPaint("edc svc", richMagenta, true), richPaint("1 failed", richRed, false), liveChipBase(), historySGR(richRed, false) + "failed", historySGR(richGreen, false) + "active"} {
		if !strings.Contains(view, want) {
			t.Fatalf("colored list misses %q:\n%q", want, view)
		}
	}
	if strings.Count(view, liveChipBase()+"\033[") < 1 || strings.Count(view, liveSelectedBar) != 1 {
		t.Fatalf("one selected row expected:\n%q", view)
	}
}
