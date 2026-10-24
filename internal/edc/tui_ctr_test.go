package edc

import (
	"context"
	"errors"
	"fmt"
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
	sources.inspect = func(context.Context, string) (ctrInspect, error) { return parseCtrInspect(ctrListLines[0]) }
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

func TestCtrBrowserDetailUsesCollectorOptIn(t *testing.T) {
	model := ctrBrowserFixture(t)
	called := false
	model.sources.collectorImage = "collector:dev"
	model.sources.inspect = func(context.Context, string) (ctrInspect, error) {
		inspect, err := parseCtrInspect(ctrInspectLine("running", 0, false, 0, ""))
		inspect.Ports = []ctrPublishedPort{{Proto: "tcp", Port: 8080, Hosts: []string{"127.0.0.1:80"}}}
		return inspect, err
	}
	model.sources.collector = func(_ context.Context, _ ctrInspect, image string) (ctrCollectorResponse, error) {
		called = image == "collector:dev"
		return ctrCollectorResponse{
			ListenStatus: ctrCollectorMetricStatus{Status: "available"},
			Listeners:    []ctrCollectorListen{{Proto: "tcp", Address: "0.0.0.0", Port: 8080, Inode: 555}},
		}, nil
	}
	model, command := ctrBrowserKeys(t, model, "end", "enter")
	if command == nil {
		t.Fatal("enter did not start the detail request")
	}
	next, _ := model.Update(command())
	content := strings.Join(next.(ctrBrowser).detail.lines, "\n")
	if !called || next.(ctrBrowser).detail.loading || !strings.Contains(content, "TCP 0.0.0.0:8080") || !strings.Contains(content, "8080/tcp → 127.0.0.1:80") {
		t.Fatalf("collector called=%t detail=%+v content:\n%s", called, next.(ctrBrowser).detail, content)
	}
}

func TestCtrBrowserDetailShowsCPUAndPidsWhenTopFails(t *testing.T) {
	model := ctrBrowserFixture(t)
	model.width, model.height = 80, 24
	inspect, err := parseCtrInspect(ctrInspectLine("running", 0, false, 0, ""))
	if err != nil {
		t.Fatal(err)
	}
	model.sources.inspect = func(context.Context, string) (ctrInspect, error) { return inspect, nil }
	model.sources.top = func(context.Context, string) ([]topProcess, error) {
		return nil, errors.New("top process columns unavailable")
	}
	percent, used, maximum := 50.0, uint64(512<<20), uint64(1<<30)
	model.sources.stats = func(_ context.Context, _ string, _ ctrInspect, detailed bool) ctrLimits {
		if !detailed {
			t.Error("detail requested one-shot list stats")
		}
		return ctrLimits{
			topLimitStatus:  topLimitStatus{Status: "available"},
			Memory:          topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
			Events:          topCgroupEvents{topLimitStatus: topLimitStatus{Status: "unavailable", Reason: "OOM counters are unavailable"}},
			CPU:             ctrCPULimits{topLimitStatus: topLimitStatus{Status: "available"}, Periods: 100, Throttled: 10},
			CPUUsage:        topLimitStatus{Status: "available"},
			CPUUsagePercent: &percent,
			Pids:            ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}, Current: 17},
		}
	}
	model, command := ctrBrowserKeys(t, model, "end", "enter")
	if command == nil {
		t.Fatal("enter did not open detail")
	}
	next, _ := model.Update(command())
	model = next.(ctrBrowser)
	if model.detail == nil {
		t.Fatal("detail did not load")
	}
	want := []string{"CPU: 50.00%", "Memory: 512.0MiB / 1.0GiB", "PIDs: 17 / unknown limit", "OOM: unavailable (OOM counters are unavailable)", "Throttle: 10/100 periods (10%)"}
	fullDetail := strings.Join(model.detail.lines, "\n")
	for _, value := range want {
		if !strings.Contains(fullDetail, value) {
			t.Errorf("detail lines omit %q:\n%s", value, fullDetail)
		}
	}
	if !strings.Contains(fullDetail, "CPU 50.00%") || !strings.Contains(fullDetail, "17 / unknown limit") {
		t.Fatalf("full detail lines omit resource values:\n%s", strings.Join(model.detail.lines, "\n"))
	}
	seen := map[string]bool{}
	for range len(model.detail.lines) {
		content := model.View().Content
		for _, value := range want {
			if strings.Contains(content, value) {
				seen[value] = true
			}
		}
		if len(seen) == len(want) {
			return
		}
		model, _ = ctrBrowserKeys(t, model, "down")
	}
	t.Fatalf("80-column detail did not expose every resource row while scrolling: visible=%v\n%s", seen, model.View().Content)
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

func TestCtrBrowserAddsOptionalColumnsByWidth(t *testing.T) {
	model := ctrBrowserFixture(t)
	for _, tc := range []struct {
		width int
		image bool
	}{{180, true}, {120, true}, {80, false}, {60, false}, {40, false}} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: tc.width, Height: 10})
		lines := strings.Split(next.(ctrBrowser).View().Content, "\n")
		for _, line := range lines {
			if got := ansi.StringWidth(line); got > tc.width {
				t.Fatalf("width %d: line is %d wide: %q", tc.width, got, line)
			}
		}
		content := strings.Join(lines, "\n")
		if strings.Contains(content, "IMAGE") != tc.image || !strings.Contains(content, "MEM") {
			t.Fatalf("width %d content: %q", tc.width, content)
		}
		if tc.width >= 120 && (!strings.Contains(content, "NET I/O") || !strings.Contains(content, "BLOCK I/O")) {
			t.Fatalf("width %d missed cumulative I/O headers: %s", tc.width, content)
		}
		if tc.width < 60 && (!strings.Contains(content, "CPU") || !strings.Contains(content, "PIDs")) {
			t.Fatalf("width %d missed core resource rows: %s", tc.width, content)
		}
	}
}

func TestCtrBrowserPacksOptionalColumnsIntoAvailableWidth(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	cpu, used, limit, percent, pids := 1.0, uint64(1<<20), uint64(2<<20), 50.0, uint64(2)
	rx, tx := uint64(0), uint64(0)
	uptime := int64(3600)
	model.items = []ctrListItem{{Name: "web", ID: "web", Image: "nginx:latest", State: "running", Status: "running", CPUPercent: &cpu, MemoryBytes: &used, MemoryLimitBytes: &limit, MemoryLimitKnown: true, MemoryPercent: &percent, PIDs: &pids, NetworkRXBytes: &rx, NetworkTXBytes: &tx, BlockReadBytes: &rx, BlockWriteBytes: &tx, UptimeS: &uptime}}
	model.loaded = true
	for _, width := range []int{100, 110, 119} {
		model.width, model.height = width, 10
		view := model.View().Content
		if !strings.Contains(view, "RESTARTS") && !strings.Contains(view, "NET I/O") && !strings.Contains(view, "UPTIME") && !strings.Contains(view, "IMAGE") {
			t.Fatalf("width %d left room for optional data but showed none:\n%s", width, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d line width %d: %q", width, got, line)
			}
		}
	}
}

func TestCtrBrowserShortensImageIntoRemainingWidth(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	cpu, used, limit, percent, pids := 1.0, uint64(1<<20), uint64(2<<20), 50.0, uint64(2)
	rx, tx := uint64(0), uint64(0)
	uptime := int64(3600)
	model.items = []ctrListItem{{Name: "web", ID: "web", Image: "nginx:latest-long-reference", State: "running", Status: "running", CPUPercent: &cpu, MemoryBytes: &used, MemoryLimitBytes: &limit, MemoryLimitKnown: true, MemoryPercent: &percent, PIDs: &pids, NetworkRXBytes: &rx, NetworkTXBytes: &tx, BlockReadBytes: &rx, BlockWriteBytes: &tx, UptimeS: &uptime}}
	model.loaded, model.height = true, 10
	model.width = 92
	view := model.View().Content
	if !strings.Contains(view, "IMAGE") || !strings.Contains(view, "nginx") || !strings.Contains(view, "…") {
		t.Fatalf("image should use the remaining width with an ellipsis:\n%s", view)
	}
	if !strings.Contains(view, "1.0%") || !strings.Contains(view, "1.0MiB/2.0MiB") || !strings.Contains(view, "2") {
		t.Fatalf("optional image column clipped core values:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if got := ansi.StringWidth(line); got > model.width {
			t.Fatalf("width %d line width %d: %q", model.width, got, line)
		}
	}
	model.width = 82
	view = model.View().Content
	if strings.Contains(view, "IMAGE") {
		t.Fatalf("image should be omitted when only a narrow fragment fits:\n%s", view)
	}
	if !strings.Contains(view, "1.0MiB/2.0MiB") || !strings.Contains(view, "2") {
		t.Fatalf("small width clipped core values:\n%s", view)
	}
}

func TestCtrBrowserTriesSmallerOptionalColumnsAfterIODoesNotFit(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	cpu, used, percent, pids, ioBytes := 123456789.0, uint64(^uint64(0)), 91.1, uint64(^uint64(0)), uint64(^uint64(0))
	uptime := int64(3600)
	model.items = []ctrListItem{{Name: "x", ID: "x", Image: "nginx", State: "running", Status: "running", Restarts: 137, UptimeS: &uptime, CPUPercent: &cpu, MemoryBytes: &used, MemoryUnlimited: true, MemoryLimitKnown: true, MemoryPercent: &percent, PIDs: &pids, NetworkRXBytes: &ioBytes, NetworkTXBytes: &ioBytes, BlockReadBytes: &ioBytes, BlockWriteBytes: &ioBytes}}
	model.loaded, model.width, model.height = true, 80, 10
	view := model.View().Content
	if strings.Contains(view, "NET I/O") || !strings.Contains(view, "RESTARTS") || !strings.Contains(view, "IMAGE") {
		t.Fatalf("small optional columns did not use width left by large I/O values:\n%s", view)
	}
	for _, value := range []string{"123456789.0%", "18446744073709551615", "137"} {
		if !strings.Contains(view, value) {
			t.Fatalf("width 80 clipped %q:\n%s", value, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if got := ansi.StringWidth(line); got > model.width {
			t.Fatalf("width 80 line width %d: %q", got, line)
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
	for _, want := range []string{richPaint("edc ctr", richMagenta, true), liveChipBase(), historySGR(richRed, false), "exited", historySGR(richGreen, false), "running"} {
		if !strings.Contains(view, want) {
			t.Fatalf("colored list misses %q:\n%q", want, view)
		}
	}
}

func TestCtrBrowserShowsPartialRowsWithAnError(t *testing.T) {
	model := ctrBrowserFixture(t)
	items := []ctrListItem{{Name: "web", ID: "aaaaaaaaaaaa", Image: "nginx", State: "running", Status: "running"}}
	next, _ := model.Update(ctrListMsg{seq: model.seq, items: items, err: errors.New("permission denied while reading stats"), at: time.Unix(1, 0)})
	model = next.(ctrBrowser)
	content := model.View().Content
	if !strings.Contains(content, "web") || !strings.Contains(content, "needs permission to access the Docker Engine socket") {
		t.Fatalf("partial list:\n%s", content)
	}
}

func TestCtrBrowserKeepsUnavailableMemoryNarrowAndShowsTheReason(t *testing.T) {
	model := ctrBrowserFixture(t)
	model.items = []ctrListItem{{Name: "web", ID: "aaaaaaaaaaaa", Image: "nginx", State: "running", Status: "running", MemoryUnavailable: "stats memory usage field is missing"}}
	model.cursor = 0
	for _, width := range []int{80, 40} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 8})
		view := next.(ctrBrowser).View().Content
		if !strings.Contains(view, "n/a") || !strings.Contains(view, "MEM: stats memory usage field is missing") {
			t.Fatalf("width %d missing memory marker/reason:\n%s", width, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d line width %d: %q", width, ansi.StringWidth(line), line)
			}
		}
	}
}

func TestCtrBrowserViewKeepsNumericValuesAtResponsiveWidths(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	cpu := 550.0
	memory := uint64(^uint64(0))
	limit := uint64(1 << 30)
	memoryPercent := 91.1
	pids := uint64(^uint64(0))
	model.items = []ctrListItem{{Name: "a-very-long-container-name-that-can-shrink", ID: "aaaaaaaaaaaa", Image: "long/image:reference", State: "running", Status: "running", CPUPercent: &cpu, MemoryBytes: &memory, MemoryLimitBytes: &limit, MemoryLimitKnown: true, MemoryPercent: &memoryPercent, PIDs: &pids}}
	model.loaded = true
	model.cursor = 0
	for _, width := range []int{40, 80, 120, 180} {
		next, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: 20})
		view := next.(ctrBrowser).View().Content
		for _, want := range []string{"550.0%", "91.1%", "18446744073709551615"} {
			if !strings.Contains(view, want) {
				t.Fatalf("width %d truncated %q:\n%s", width, want, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d line width %d: %q", width, got, line)
			}
		}
	}
}

func TestCtrBrowserWideRowsAlignValuesAndColorOnlyState(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, true)
	cpu := 12.5
	used := uint64(1024)
	limit := uint64(2048)
	percent := 50.0
	pids := uint64(3)
	makeItem := func(name string) ctrListItem {
		return ctrListItem{Name: name, ID: name, State: "running", Status: "running", CPUPercent: &cpu, MemoryBytes: &used, MemoryLimitBytes: &limit, MemoryLimitKnown: true, MemoryPercent: &percent, PIDs: &pids}
	}
	model.items = []ctrListItem{makeItem("short"), makeItem("a-longer-container-name")}
	model.loaded = true
	model.width = 80
	model.height = 10
	lines := strings.Split(model.View().Content, "\n")
	header := ansi.Strip(lines[2])
	if !strings.Contains(header, "CPU%") {
		t.Fatalf("resource header is missing CPU%%: %q", header)
	}
	short, long := strings.Replace(ansi.Strip(lines[3]), liveSelectedBar+" ", "  ", 1), ansi.Strip(lines[4])
	shortCPU, longCPU := strings.Index(short, "12.5%"), strings.Index(long, "12.5%")
	if shortCPU < 0 || shortCPU != longCPU {
		t.Fatalf("CPU columns do not align: %q / %q", short, long)
	}
	stateColor := historySGR(richGreen, false)
	raw := lines[3]
	stateAt := strings.Index(raw, stateColor)
	nameAt := strings.Index(raw, "short")
	if stateAt < nameAt || nameAt < 0 {
		t.Fatalf("state color does not follow the name cell: %q", raw)
	}
}

func TestCtrBrowserCompactHeightTracksRowsAndHints(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	model.width = 40
	model.height = 8
	model.loaded = true
	items := make([]ctrListItem, 5)
	for index := range items {
		items[index] = ctrListItem{Name: fmt.Sprintf("web-%d", index), ID: fmt.Sprint(index), State: "running", Status: "running", CPUUnavailable: "CPU sample is unavailable"}
	}
	model.items = items
	model.cursor = 4
	model.keepCursor()
	view := model.View().Content
	if !strings.Contains(view, "web-4") || len(strings.Split(view, "\n")) > model.height {
		t.Fatalf("compact page does not fit selected row and hint (%d lines):\n%s", len(strings.Split(view, "\n")), view)
	}
	model, _ = ctrBrowserKeys(t, model, "pgup")
	if model.cursor >= 4 {
		t.Fatalf("page up did not move across compact rows: cursor=%d", model.cursor)
	}
}

func TestCtrBrowserCompactNormalValuesFitWithinSelectionPrefix(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	cpu := 0.0
	used := uint64(48_863_642)
	pids := uint64(17)
	model.items = []ctrListItem{{Name: "nginx", ID: "nginx", State: "running", Status: "running", CPUPercent: &cpu, MemoryBytes: &used, MemoryUnlimited: true, MemoryLimitKnown: true, PIDs: &pids}}
	model.loaded, model.width, model.height = true, 40, 10
	view := model.View().Content
	for _, value := range []string{"CPU 0.0%", "MEM 46.6MiB/∞", "MEM% —", "PIDs 17"} {
		if !strings.Contains(view, value) {
			t.Fatalf("40-column view clipped %q:\n%s", value, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if width := ansi.StringWidth(line); width > model.width {
			t.Fatalf("40-column line has width %d: %q", width, line)
		}
	}
}

func TestCtrBrowserWideStateUsesAvailableWidth(t *testing.T) {
	model := newCtrBrowser(ctrSources{}, time.Second, false, false)
	used, limit, pids := uint64(0), uint64(1024), uint64(0)
	model.items = []ctrListItem{
		{Name: "worker", ID: "worker", State: "exited, exit code 137", Status: "exited", MemoryBytes: &used, MemoryLimitBytes: &limit, MemoryLimitKnown: true, PIDs: &pids},
		{Name: "web", ID: "web", State: "running (healthy)", Status: "running", MemoryBytes: &used, MemoryLimitBytes: &limit, MemoryLimitKnown: true, PIDs: &pids},
	}
	model.loaded, model.width, model.height = true, 180, 10
	view := model.View().Content
	for _, state := range []string{"exited, exit code 137", "running (healthy)"} {
		if !strings.Contains(view, state) {
			t.Fatalf("180-column view shortened state %q:\n%s", state, view)
		}
	}
}

func TestCtrBrowserBoundsFallbackHintsAtSmallHeights(t *testing.T) {
	cpu, memory, pids, percent := 50.0, uint64(46<<20), uint64(17), 46.0
	item := ctrListItem{
		Name: "web", ID: "web", State: "running", Status: "running", CPUPercent: &cpu,
		MemoryBytes: &memory, MemoryUnlimited: true, MemoryLimitKnown: true, MemoryPercent: &percent, PIDs: &pids,
		CPUUnavailable:    "phase-two sample unavailable because the stats request failed with a long daemon error",
		MemoryUnavailable: "phase-two sample unavailable because the memory usage field is missing after a long daemon error",
		PIDsUnavailable:   "phase-two sample unavailable because the pids current field is missing after a long daemon error",
		NetworkRXBytes:    &memory, NetworkRXUnavailable: "phase-two sample unavailable because the rx counter is invalid after a long daemon error",
		NetworkTXBytes: &memory, NetworkTXUnavailable: "phase-two sample unavailable because the tx counter is invalid after a long daemon error",
		BlockReadBytes: &memory, BlockReadUnavailable: "phase-two sample unavailable because the block read counter is invalid after a long daemon error",
		BlockWriteBytes: &memory, BlockWriteUnavailable: "phase-two sample unavailable because the block write counter is invalid after a long daemon error",
	}
	for _, width := range []int{40, 80} {
		model := newCtrBrowser(ctrSources{}, time.Second, false, false)
		model.items, model.cursor, model.width, model.height, model.loaded = []ctrListItem{item}, 0, width, 8, true
		model.err = errors.New("partial inspect error")
		view := model.View().Content
		if lines := strings.Split(view, "\n"); len(lines) > model.height {
			t.Fatalf("width %d rendered %d lines in an 8-line view:\n%s", width, len(lines), view)
		}
		for _, value := range []string{"50.0%", "46.0%", "17"} {
			if !strings.Contains(view, value) {
				t.Fatalf("width %d hid core value %q:\n%s", width, value, view)
			}
		}
		for _, line := range strings.Split(view, "\n") {
			if got := ansi.StringWidth(line); got > width {
				t.Fatalf("width %d line width %d: %q", width, got, line)
			}
		}
	}
}
