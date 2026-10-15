package edc

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func ctrBrowserFixture(t *testing.T) ctrBrowser {
	t.Helper()
	var asked []string
	sources := ctrListFakeSources(&asked)
	// 상세는 container 하나의 inspect를 읽는다. 목록의 web 줄을 그대로 돌려준다.
	sources.inspect = func(context.Context, string) (string, error) { return ctrListLines[0], nil }
	model := newCtrBrowser(sources, time.Second, false, false)
	items, err := collectCtrList(context.Background(), sources, false)
	if err != nil {
		t.Fatal(err)
	}
	next, cmd := model.Update(ctrListMsg{seq: model.seq, items: items, at: time.Unix(0, 0)})
	if cmd == nil {
		t.Fatal("a list must schedule the next refresh")
	}
	return next.(ctrBrowser)
}

func ctrBrowserKeys(t *testing.T, model ctrBrowser, keys ...string) (ctrBrowser, tea.Cmd) {
	t.Helper()
	var cmd tea.Cmd
	for _, key := range keys {
		var next tea.Model
		next, cmd = model.Update(topKey(key))
		model = next.(ctrBrowser)
	}
	return model, cmd
}

func TestCtrBrowserOpensADetailAndGoesBack(t *testing.T) {
	model := ctrBrowserFixture(t)
	if content := model.View().Content; !strings.Contains(content, "6 containers · 2 failed · 2 restarting, paused, or unhealthy") || !strings.Contains(content, "› oom") {
		t.Fatalf("list:\n%s", content)
	}
	model, cmd := ctrBrowserKeys(t, model, "end", "enter")
	if model.detail == nil || model.detail.unit != "web" || cmd == nil {
		t.Fatalf("enter opens the selected container: %+v", model.detail)
	}
	next, _ := model.Update(cmd())
	model = next.(ctrBrowser)
	if content := model.View().Content; model.detail.loading || !strings.Contains(content, "ctr.state") {
		t.Fatalf("detail:\n%s", content)
	}
	if model.detailID() != "aaaaaaaaaaaa" {
		t.Fatalf("r reads the container again by ID, got %s", model.detailID())
	}
	model, _ = ctrBrowserKeys(t, model, "esc")
	if model.detail != nil || model.selectedName() != "web" {
		t.Fatalf("esc goes back to the same row: %v %s", model.detail != nil, model.selectedName())
	}
}

func TestCtrBrowserFiltersAndKeepsTheSelection(t *testing.T) {
	model := ctrBrowserFixture(t)
	model, _ = ctrBrowserKeys(t, model, "/", "n", "g", "i", "n", "x", "enter")
	if visible := model.visible(); len(visible) != 6 {
		t.Fatalf("the image name matches too: %d shown", len(visible))
	}
	model, _ = ctrBrowserKeys(t, model, "esc", "f")
	if visible := model.visible(); ctrListNames(visible) != "oom,worker" {
		t.Fatalf("f shows the failed containers: %s", ctrListNames(visible))
	}
	model, _ = ctrBrowserKeys(t, model, "esc", "down", "down", "down")
	if model.selectedName() != "new" {
		t.Fatalf("selected %s", model.selectedName())
	}
	reordered := append([]ctrListItem{model.items[3]}, append(append([]ctrListItem{}, model.items[:3]...), model.items[4:]...)...)
	next, _ := model.Update(ctrListMsg{seq: model.seq, items: reordered, at: time.Unix(2, 0)})
	if model = next.(ctrBrowser); model.selectedName() != "new" || model.cursor != 0 {
		t.Fatalf("after refresh: %s at %d", model.selectedName(), model.cursor)
	}
	model, cmd := ctrBrowserKeys(t, model, "a")
	if !model.all || cmd == nil {
		t.Fatal("a must reload with --all")
	}
	if _, cmd := model.Update(ctrTickMsg{seq: model.seq - 1}); cmd != nil {
		t.Fatal("a stale tick must not read again")
	}
}

func TestCtrBrowserDropsTheImageColumnFirst(t *testing.T) {
	model := ctrBrowserFixture(t)
	for _, tc := range []struct {
		width int
		image bool
	}{{120, true}, {80, false}, {60, false}} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: tc.width, Height: 10})
		lines := strings.Split(next.(ctrBrowser).View().Content, "\n")
		for _, line := range lines {
			if got := ansi.StringWidth(line); got > tc.width {
				t.Fatalf("width %d: line is %d wide: %q", tc.width, got, line)
			}
		}
		if strings.Contains(lines[2], "IMAGE") != tc.image || !strings.Contains(lines[2], "MEM") && tc.width >= 80 {
			t.Fatalf("width %d header: %q", tc.width, lines[2])
		}
	}
}

func TestCtrBrowserColorsStatesAndTheSelection(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("NO_COLOR", "")
	plain := ctrBrowserFixture(t)
	colored := plain
	colored.color = true
	plainLines, coloredLines := plain.listLines(), colored.listLines()
	for index, line := range coloredLines {
		stripped := strings.TrimRight(strings.Replace(ansi.Strip(line), liveSelectedBar+" ", "› ", 1), " ")
		if stripped != strings.TrimRight(plainLines[index], " ") {
			t.Fatalf("line %d = %q, want %q", index, stripped, plainLines[index])
		}
	}
	view := strings.Join(coloredLines, "\n")
	for _, want := range []string{richPaint("edc ctr", richMagenta, true), liveChipBase(), historySGR(richRed, false) + "exited", historySGR(richGreen, false) + "running"} {
		if !strings.Contains(view, want) {
			t.Fatalf("colored list misses %q:\n%q", want, view)
		}
	}
}
