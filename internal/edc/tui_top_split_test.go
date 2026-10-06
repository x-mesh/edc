package edc

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTopSplitParse(t *testing.T) {
	all := []topView{topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure}
	valid := []struct {
		value string
		want  []topView
	}{
		{"all", all},
		{"none", nil},
		{"mem,disk,cpu", []topView{topViewMemory, topViewDisk, topViewCPU}},
		{" mem , disk ", []topView{topViewMemory, topViewDisk}},
		{"psi", []topView{topViewPressure}},
	}
	for _, test := range valid {
		got, err := parseTopSplit(test.value)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Errorf("parseTopSplit(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"", "mem,", "mem,,disk", "memory", "Mem", "mem,mem", "all,mem", "none,cpu"} {
		if got, err := parseTopSplit(value); err == nil {
			t.Errorf("parseTopSplit(%q) = %v, want error", value, got)
		}
	}
}

func topSplitModelAt(width, height, rows int, color bool, split []topView) topModel {
	model := topFixtureModel(nil)
	model.width, model.height = width, height
	model.limits.color = color
	processes := []topProcess{{PID: 11, CPU: 90, RSS: 1 << 20, Command: "alpha"}, {PID: 12, CPU: 80, RSS: 1 << 20, Command: "beta"}, {PID: 13, CPU: 70, RSS: 1 << 20, Command: "gamma"}, {PID: 14, CPU: 60, RSS: 1 << 20, Command: "delta"}, {PID: 15, CPU: 50, RSS: 1 << 20, Command: "eps"}}
	for index := 0; index < rows; index++ {
		model.rows = append(model.rows, topDashboardRow{at: time.Date(2026, 1, 1, 9, 0, index, 0, time.UTC), processes: processes, processesValid: true, rate: resourceRate{
			MemoryPercent: 40 + float64(index), SwapOut: float64(index) * 1024, PSIValid: true, PSIMemory: float64(index), PSIIO: 3, PSICPU: 1, Load1: 1.5,
			DiskRead: float64(index) * 3e6, DiskWrite: 4e6, DiskHealthValid: true, DiskIOPS: 120, DiskAwait: float64(index * 6), DiskBusyValid: true, DiskBusy: 30,
			NetIn: 1e6, NetOut: 2e6, NetHealthValid: true, PacketsIn: 300, PacketsOut: 200, CPUUser: 20, CPUSystem: 5, CoreCPU: []float64{10, 95, 20, 30},
		}})
	}
	model.selected = max(0, rows-1)
	if len(split) == 0 {
		return model.enterSplit()
	}
	return model.withSplit(split)
}

func topSplitLines(model topModel) []string {
	return strings.Split(model.View().Content, "\n")
}

func TestTopSplitStaysInsideTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}, {160, 45}} {
		for _, rows := range []int{0, 2, 100} {
			for _, color := range []bool{false, true} {
				for _, split := range [][]topView{nil, {topViewMemory, topViewDisk}} {
					model := topSplitModelAt(size[0], size[1], rows, color, split)
					lines := topSplitLines(model)
					if len(lines) > size[1] {
						t.Errorf("%v rows=%d color=%t split=%v: %d lines", size, rows, color, split, len(lines))
					}
					for _, line := range lines {
						if width := ansi.StringWidth(line); width > size[0] {
							t.Errorf("%v rows=%d: line is %d columns: %q", size, rows, width, line)
						}
					}
				}
			}
		}
	}
}

func topSplitBoxRows(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(ansi.Strip(line), "╭") {
			count++
		}
	}
	return count
}

func TestTopSplitPlacesBoxesLeftToRight(t *testing.T) {
	for _, row := range []struct {
		width, height, boxRows, dataRows int
	}{{120, 40, 3, 6}, {160, 45, 2, 15}} {
		model := topSplitModelAt(row.width, row.height, 20, false, nil)
		if model.view != topViewSplit {
			t.Fatalf("view = %s", model.view)
		}
		if got := len(model.panelLines()); got != 7 {
			t.Fatalf("panel lines = %d, want 7", got)
		}
		lines := topSplitLines(model)
		if got := topSplitBoxRows(lines); got != row.boxRows {
			t.Errorf("%dx%d: box rows = %d, want %d\n%s", row.width, row.height, got, row.boxRows, strings.Join(lines, "\n"))
		}
		if got := model.splitBodyLines(); got != row.dataRows {
			t.Errorf("%dx%d: data rows = %d, want %d", row.width, row.height, got, row.dataRows)
		}
	}
	first := topSplitLines(topSplitModelAt(160, 45, 20, false, nil))[1]
	if !strings.Contains(first, "╭─ cpu ") || !strings.Contains(first, "╭─ mem ") || !strings.Contains(first, "╭─ disk ") || strings.Contains(first, "╭─ net ") {
		t.Errorf("160 columns: first box row = %q, want cpu, mem and disk", first)
	}
	// cpu 박스가 없으면 mem은 load를 남겨 41열이다. mem 옆의 disk는 시각 열을 빼고 queue까지 45열, queue를 빼면 38열이다.
	for _, row := range []struct {
		width, boxRows int
		queue          bool
	}{{86, 1, true}, {85, 1, false}, {79, 1, false}, {78, 2, true}} {
		narrow := topSplitModelAt(row.width, 24, 20, false, []topView{topViewMemory, topViewDisk})
		lines := topSplitLines(narrow)
		if got := topSplitBoxRows(lines); got != row.boxRows {
			t.Errorf("%dx24 mem,disk: box rows = %d, want %d", row.width, got, row.boxRows)
		}
		if got := strings.Contains(strings.Join(lines, "\n"), "queue"); got != row.queue {
			t.Errorf("%dx24 mem,disk: queue shown = %t, want %t", row.width, got, row.queue)
		}
	}
	// 줄 수가 같으면 optional 칸을 남기고, 늘면 뺀다.
	if content := topSplitModelAt(160, 45, 20, false, nil).View().Content; !strings.Contains(content, "retr/s") || !strings.Contains(content, "steal%") {
		t.Errorf("160x45 dropped the optional columns:\n%s", content)
	}
	if content := topSplitModelAt(120, 40, 20, false, nil).View().Content; strings.Contains(content, "retr/s") || strings.Contains(content, "steal%") {
		t.Errorf("120x40 kept optional columns at the cost of a row:\n%s", content)
	}
}

func TestTopSplitShowsTimeOncePerBoxRow(t *testing.T) {
	model := topSplitModelAt(160, 45, 20, false, nil)
	for _, line := range topSplitLines(model) {
		if !strings.HasPrefix(line, "│") {
			continue
		}
		if got := strings.Count(line, "time │"); strings.Contains(line, "time") && got != 1 {
			t.Errorf("header has %d time columns: %q", got, line)
		}
		if got := strings.Count(line, "09:00:"); got > 1 {
			t.Errorf("row has %d time columns: %q", got, line)
		}
	}
	rows, _, ok := model.splitLayout()
	if !ok {
		t.Fatal("160x45 did not fit")
	}
	for _, boxes := range rows {
		for index, box := range boxes {
			if box.timeless != (index > 0) {
				t.Errorf("%s at %d: timeless = %t", box.view, index, box.timeless)
			}
			if full := model.splitBox(box.view, model.displayWidth(), box.pane.compact).inner; index > 0 && box.inner != full-topSplitTimeWidth {
				t.Errorf("%s: inner = %d, want %d", box.view, box.inner, full-topSplitTimeWidth)
			}
		}
	}
}

func TestTopSplitFallsBackToTheFirstBox(t *testing.T) {
	for _, row := range []struct {
		model topModel
		title string
	}{{topSplitModelAt(80, 24, 20, false, nil), "view cpu"}, {topSplitModelAt(120, 12, 20, false, []topView{topViewCPU}), "view cpu"}} {
		model := row.model
		content := model.View().Content
		if strings.Contains(content, "╭") || !strings.Contains(content, row.title) || !strings.Contains(content, "split needs a larger terminal · showing cpu") {
			t.Errorf("%dx%d is not the cpu fallback:\n%s", model.width, model.height, content)
		}
	}
	// 터미널보다 넓은 박스는 단일 보기로 바꾸지 않고 뒤쪽 칸을 뺀다.
	narrow := topSplitModelAt(60, 40, 20, false, []topView{topViewCPU})
	content := narrow.View().Content
	if !strings.Contains(content, "╭─ cpu ") || strings.Contains(content, "split needs") || strings.Contains(content, "steal%") || strings.Contains(content, "blocked") {
		t.Errorf("60x40 cpu box did not shrink:\n%s", content)
	}
	for _, line := range strings.Split(content, "\n") {
		if ansi.StringWidth(line) > 60 {
			t.Errorf("60x40 line is %d columns: %q", ansi.StringWidth(line), line)
		}
	}
	model := topSplitModelAt(80, 24, 20, false, nil)
	model.notice = "other notice"
	if content := model.View().Content; strings.Contains(content, "split needs") || !strings.Contains(content, "other notice") {
		t.Errorf("fallback overwrote the notice:\n%s", content)
	}
}

func TestTopSplitDropsSharedColumns(t *testing.T) {
	model := topSplitModelAt(160, 45, 20, false, nil)
	header := func(view topView) string {
		return model.splitBox(view, model.displayWidth(), false).pane.tableHeader()[0]
	}
	for _, view := range []topView{topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure} {
		if strings.Contains(header(view), "signal") {
			t.Errorf("%s header has signal: %q", view, header(view))
		}
	}
	if strings.Contains(header(topViewMemory), "load") {
		t.Errorf("mem header has load: %q", header(topViewMemory))
	}
	for _, title := range []string{"load", "mem%"} {
		if strings.Contains(header(topViewPressure), title) {
			t.Errorf("psi header has %s: %q", title, header(topViewPressure))
		}
	}
	for _, title := range []string{"mem full", "io full"} {
		if !strings.Contains(header(topViewPressure), title) {
			t.Errorf("psi header lost %s: %q", title, header(topViewPressure))
		}
	}
	for _, title := range []string{"busy%", "await"} {
		if !strings.Contains(header(topViewDisk), title) {
			t.Errorf("disk header lost %s: %q", title, header(topViewDisk))
		}
	}
	signals := 0
	for _, line := range topSplitLines(model) {
		if strings.HasPrefix(line, "signal ") {
			signals++
		}
	}
	if signals != 1 {
		t.Errorf("signal lines = %d, want 1", signals)
	}
}

func TestTopSplitSelectionSharesOneTimeWindow(t *testing.T) {
	model := topSplitModelAt(120, 40, 20, false, nil)
	model.follow, model.selected = false, 10
	content := model.View().Content
	lines := topSplitLines(model)
	if got, want := strings.Count(content, "│09:00:10>│"), topSplitBoxRows(lines); got != want {
		t.Errorf("selected row marker appears %d times, want once per box row (%d)\n%s", got, want, content)
	}
	for _, line := range lines {
		if strings.Contains(line, "09:00:09 ") {
			t.Errorf("a box starts before the shared window: %q", line)
		}
	}
}

func TestTopSplitBordersAlignWithColor(t *testing.T) {
	for _, follow := range []bool{true, false} {
		model := topSplitModelAt(120, 40, 20, true, nil)
		model.follow, model.selected = follow, 10
		content := model.View().Content
		if !follow && !strings.Contains(content, topBannerStyle) {
			t.Fatal("the selected row has no highlight")
		}
		var group []int
		flush := func() {
			for _, width := range group {
				if width != group[0] {
					t.Errorf("follow=%t: box row widths differ: %v", follow, group)
					break
				}
			}
			group = nil
		}
		for _, line := range strings.Split(content, "\n") {
			plain := ansi.Strip(line)
			if strings.HasPrefix(plain, "╭") {
				flush()
			}
			if strings.HasPrefix(plain, "╭") || strings.HasPrefix(plain, "│") || strings.HasPrefix(plain, "╰") {
				group = append(group, ansi.StringWidth(line))
				if !strings.HasSuffix(plain, "╮") && !strings.HasSuffix(plain, "│") && !strings.HasSuffix(plain, "╯") {
					t.Errorf("box line has no right border: %q", plain)
				}
			} else {
				flush()
			}
		}
		flush()
	}
}

func TestTopSplitPagingUsesBoxRows(t *testing.T) {
	model := topSplitModelAt(120, 40, 60, false, nil)
	paged := topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if want := model.selected - model.splitBodyLines(); paged.selected != want || model.splitBodyLines() < topSplitMinRows {
		t.Errorf("PgUp selected = %d, want %d", paged.selected, want)
	}
	small := topSplitModelAt(80, 24, 60, false, nil)
	paged = topAfter(t, small, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if want := small.selected - small.splitFallback().bodyLines(); paged.selected != want {
		t.Errorf("fallback PgUp selected = %d, want %d", paged.selected, want)
	}
}

func TestTopWithSplitHonorsProcessFilterAndPlatform(t *testing.T) {
	model := topFixtureModel(nil)
	filter, err := parseTopProcessFilter("nginx")
	if err != nil {
		t.Fatal(err)
	}
	filtered := model.withProcessFilter(filter).withSplit([]topView{topViewMemory})
	if !filtered.processFilter.active() || filtered.view != topViewProcess {
		t.Errorf("filter active: view = %s", filtered.view)
	}
	if got := model.withSplit(nil); got.view != topViewAll {
		t.Errorf("nil list: view = %s", got.view)
	}
	darwin := model
	darwin.details.System = "darwin"
	only := darwin.withSplit([]topView{topViewPressure})
	if only.view != topViewAll || only.notice == "" {
		t.Errorf("darwin [psi]: view = %s, notice = %q", only.view, only.notice)
	}
	mixed := darwin.withSplit([]topView{topViewMemory, topViewPressure})
	if mixed.view != topViewSplit || mixed.notice == "" {
		t.Errorf("darwin [mem psi]: view = %s, notice = %q", mixed.view, mixed.notice)
	}
	if got := mixed.splitViews(); !reflect.DeepEqual(got, []topView{topViewMemory}) {
		t.Errorf("darwin boxes = %v", got)
	}
	linux := model.withSplit([]topView{topViewPressure})
	if linux.view != topViewSplit || linux.notice != "" {
		t.Errorf("linux [psi]: view = %s, notice = %q", linux.view, linux.notice)
	}
}

func TestTopSplitKeys(t *testing.T) {
	model := topSplitModelAt(160, 45, 20, false, []topView{topViewMemory, topViewDisk})
	single := topAfter(t, model, topKey("c"))
	if single.view != topViewCPU {
		t.Fatalf("c: view = %s", single.view)
	}
	back := topAfter(t, single, topKey("v"))
	if back.view != topViewSplit || !reflect.DeepEqual(back.splitViews(), []topView{topViewMemory, topViewDisk}) {
		t.Errorf("v: view = %s, boxes = %v", back.view, back.splitViews())
	}
	for _, key := range []string{"1", "c", "m", "d", "n", "s"} {
		if got := topAfter(t, model, topKey(key)); got.view == topViewSplit {
			t.Errorf("%s stayed in the box screen", key)
		}
	}
	all := topAfter(t, topSplitModelAt(160, 45, 20, false, nil).withSplit(nil), topKey("c"), topKey("v"))
	if all.view != topViewSplit || len(all.splitViews()) != 5 {
		t.Errorf("v with no list: view = %s, boxes = %v", all.view, all.splitViews())
	}
	darwin := topFixtureModel(nil)
	darwin.details.System = "darwin"
	if got := topAfter(t, darwin, topKey("v")); got.view != topViewSplit || got.notice == "" {
		t.Errorf("darwin v: view = %s, notice = %q", got.view, got.notice)
	}
	if got := topAfter(t, model, topKey("f"), topKey("v")); got.view != topViewSplit {
		t.Errorf("f then v: view = %s", got.view)
	}
}

func TestTopSplitKeepsPanelsAndSelection(t *testing.T) {
	model := topSplitModelAt(160, 45, 20, false, nil)
	detail := topAfter(t, model, topKey("enter"))
	found := false
	for _, line := range topSplitLines(detail) {
		if strings.HasPrefix(line, "detail ") {
			found = true
			if ansi.StringWidth(line) != 160 {
				t.Errorf("detail panel is %d columns, want 160", ansi.StringWidth(line))
			}
		}
	}
	if !found || topSplitBoxRows(topSplitLines(detail)) != 2 {
		t.Errorf("detail panel missing or boxes lost:\n%s", detail.View().Content)
	}
	if peaks := topAfter(t, model, topKey("h")); !strings.Contains(peaks.View().Content, "peaks 60s") {
		t.Error("h did not show the peaks panel")
	}
	if focus := topAfter(t, model, topKey("tab")); !focus.processFocus || !strings.Contains(focus.View().Content, "Enter focus PID") || focus.view != topViewSplit {
		t.Error("Tab did not open candidate selection inside the box screen")
	}
	up := topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	if up.selected != model.selected-1 || up.follow || !strings.Contains(up.View().Content, "│09:00:18>│") {
		t.Errorf("up: selected = %d follow = %t", up.selected, up.follow)
	}
	if end := topAfter(t, up, tea.KeyPressMsg{Code: tea.KeyEnd}); end.selected != model.selected || !end.follow {
		t.Errorf("End: selected = %d follow = %t", end.selected, end.follow)
	}
	help := topAfter(t, model, topKey("?"))
	if !strings.Contains(help.View().Content, "v boxes") {
		t.Errorf("help has no v key:\n%s", help.View().Content)
	}
}

func topSplitRowViews(rows [][]topSplitBox) [][]topView {
	var views [][]topView
	for _, row := range rows {
		var line []topView
		for _, box := range row {
			line = append(line, box.view)
		}
		views = append(views, line)
	}
	return views
}

func TestTopSplitBalancesRowWidths(t *testing.T) {
	// 12 core면 cpu 박스는 57열이다. 왼쪽부터 채우면 197열에서 cpu+mem+disk+net(187) / psi(52)가 되어 둘째 줄이 빈다.
	model := topSplitModelAt(197, 60, 20, false, nil)
	model.previous.Cores = make([]resourceCPU, 12)
	rows, _, ok := model.splitLayout()
	if !ok {
		t.Fatal("197x60 did not fit")
	}
	want := [][]topView{{topViewCPU, topViewMemory, topViewDisk}, {topViewNetwork, topViewPressure}}
	if got := topSplitRowViews(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	for _, row := range rows {
		width := 0
		for _, box := range row {
			width += box.inner + 2
		}
		if width > 197 {
			t.Errorf("row is %d columns: %v", width, topSplitRowViews([][]topSplitBox{row}))
		}
	}
	// 줄 수는 왼쪽부터 채울 때보다 늘지 않는다.
	box := func(inner int) topSplitBox { return topSplitBox{inner: inner} }
	boxes := []topSplitBox{box(50), box(50), box(50), box(50)}
	if got := topSplitBalance(boxes, 2, 200); len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 2 {
		t.Errorf("four equal boxes = %v, want two and two", topSplitRowViews(got))
	}
	if got := topSplitBalance([]topSplitBox{box(100)}, 1, 200); len(got) != 1 || got[0][0].timeless {
		t.Errorf("one box = %+v", got)
	}
}

func TestTopSplitKeepsSharedColumnsWithoutTheirOwner(t *testing.T) {
	header := func(split []topView, view topView) string {
		model := topSplitModelAt(200, 60, 20, false, split)
		return model.splitBox(view, model.displayWidth(), false).pane.tableHeader()[0]
	}
	for _, row := range []struct {
		split   []topView
		view    topView
		present []string
		absent  []string
	}{
		{nil, topViewMemory, nil, []string{"load"}},
		{nil, topViewPressure, nil, []string{"load", "mem%"}},
		{[]topView{topViewMemory, topViewDisk}, topViewMemory, []string{"load"}, nil},
		{[]topView{topViewPressure}, topViewPressure, []string{"load", "mem%"}, nil},
		{[]topView{topViewPressure, topViewMemory}, topViewPressure, []string{"load"}, []string{"mem%"}},
		{[]topView{topViewCPU}, topViewCPU, []string{"load"}, nil},
	} {
		got := header(row.split, row.view)
		for _, title := range row.present {
			if !strings.Contains(got, title) {
				t.Errorf("split=%v %s: %s missing in %q", row.split, row.view, title, got)
			}
		}
		for _, title := range row.absent {
			if strings.Contains(got, title) {
				t.Errorf("split=%v %s: %s shown in %q", row.split, row.view, title, got)
			}
		}
	}
}
