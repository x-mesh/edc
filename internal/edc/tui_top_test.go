package edc

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func topFixtureModel(sample func() (resourceSnapshot, error)) topModel {
	details := hostDetails{Hostname: "host", Model: "model", Cores: 8, MemoryTotal: 16 * 1024 * 1024 * 1024}
	first := resourceSnapshot{TakenAt: time.Unix(0, 0), CPUTotal: 100}
	model := newTopModel(details, first, time.Second, sample)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: topTableWidth, Height: 20})
	return updated.(topModel)
}

func topSampleAt(seconds int) resourceSnapshot {
	return resourceSnapshot{TakenAt: time.Unix(int64(seconds), 0), CPUTotal: 200, MemoryUsed: 25, MemoryTotal: 100}
}

func topAfter(t *testing.T, model topModel, messages ...tea.Msg) topModel {
	t.Helper()
	var current tea.Model = model
	for _, msg := range messages {
		current, _ = current.Update(msg)
	}
	updated, ok := current.(topModel)
	if !ok {
		t.Fatalf("model = %#v", current)
	}
	return updated
}

func TestTopModelAppendsRowsFromSamples(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: topSampleAt(1)}, topSampleMsg{snapshot: topSampleAt(2)})
	if len(model.rows) != 2 {
		t.Fatalf("rows = %#v", model.rows)
	}
	want := topDashboardRow{at: topSampleAt(2).TakenAt, rate: calculateRate(topSampleAt(1), topSampleAt(2))}
	if model.rows[1].at != want.at || !reflect.DeepEqual(model.rows[1].rate, want.rate) {
		t.Fatalf("row = %#v, want %#v", model.rows[1], want)
	}
	view := model.View()
	if !view.AltScreen {
		t.Fatal("dashboard must use the alt screen")
	}
	for _, expected := range []string{"network", "q quit  p pause  +/-", "interval 1s"} {
		if !strings.Contains(view.Content, expected) {
			t.Fatalf("view %q does not contain %q", view.Content, expected)
		}
	}
}

func TestTopModelIgnoresStaleSamples(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{seq: 3, snapshot: topSampleAt(1)})
	if len(model.rows) != 0 {
		t.Fatalf("stale sample was applied: %#v", model.rows)
	}
}

func TestTopModelKeepsDashboardOpenOnSampleError(t *testing.T) {
	model, cmd := topFixtureModel(nil).Update(topSampleMsg{err: errors.New("read failed")})
	final, ok := model.(topModel)
	if !ok || final.lastErr == nil {
		t.Fatalf("model = %#v", model)
	}
	if cmd == nil {
		t.Fatal("a sample error must schedule a retry")
	}
}

func TestTopModelPauseStopsSampling(t *testing.T) {
	sample := func() (resourceSnapshot, error) { return topSampleAt(1), nil }
	paused, cmd := topFixtureModel(sample).Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	model, ok := paused.(topModel)
	if !ok || !model.paused {
		t.Fatalf("p did not pause: %#v", paused)
	}
	if cmd != nil {
		t.Fatal("a paused dashboard must not schedule the next sample")
	}
	resumed, cmd := model.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if resumed.(topModel).paused {
		t.Fatal("p must resume the dashboard")
	}
	if cmd == nil {
		t.Fatal("resuming must schedule the next sample")
	}
	if !strings.Contains(model.View().Content, T("observe.top.paused")) {
		t.Fatalf("view = %q", model.View().Content)
	}
}

func TestTopModelResumeSetsANewBaseline(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: topSampleAt(1)})
	paused := topAfter(t, model, tea.KeyPressMsg{Code: 'p', Text: "p"})
	resumed := topAfter(t, paused, tea.KeyPressMsg{Code: 'p', Text: "p"})
	afterBaseline := topAfter(t, resumed, topSampleMsg{seq: resumed.seq, snapshot: topSampleAt(100)})
	if len(afterBaseline.rows) != 1 || afterBaseline.baseline {
		t.Fatalf("resume must only establish a baseline: %#v", afterBaseline)
	}
	withRate := topAfter(t, afterBaseline, topSampleMsg{seq: afterBaseline.seq, snapshot: topSampleAt(101)})
	if len(withRate.rows) != 2 {
		t.Fatalf("sample after the baseline must append a row: %#v", withRate)
	}
}

func TestTopModelChangesInterval(t *testing.T) {
	model := topFixtureModel(func() (resourceSnapshot, error) { return topSampleAt(1), nil })
	slower := topAfter(t, model, tea.KeyPressMsg{Code: '+', Text: "+"})
	if slower.interval != 2*time.Second || slower.seq == model.seq {
		t.Fatalf("interval = %s, seq = %d", slower.interval, slower.seq)
	}
	faster := topAfter(t, model, tea.KeyPressMsg{Code: '-', Text: "-"})
	if faster.interval != 500*time.Millisecond {
		t.Fatalf("interval = %s", faster.interval)
	}
}

func TestNextTopIntervalClampsAndInsertsCLIValue(t *testing.T) {
	if got := nextTopInterval(topMinInterval, -1); got != topMinInterval {
		t.Fatalf("lower bound = %s", got)
	}
	if got := nextTopInterval(topMaxInterval, 1); got != topMaxInterval {
		t.Fatalf("upper bound = %s", got)
	}
	// ladder에 없는 CLI 값은 자기 자리를 만들고 그 옆으로 움직인다.
	if got := nextTopInterval(3*time.Second, 1); got != 5*time.Second {
		t.Fatalf("next from 3s = %s", got)
	}
	if got := nextTopInterval(3*time.Second, -1); got != 2*time.Second {
		t.Fatalf("previous from 3s = %s", got)
	}
}

func TestTopModelTrimsHistory(t *testing.T) {
	rows := make([]topDashboardRow, topDashboardHistory)
	model := topFixtureModel(nil)
	model.rows = rows
	model = topAfter(t, model, topSampleMsg{snapshot: topSampleAt(1)})
	if len(model.rows) != topDashboardHistory {
		t.Fatalf("rows = %d, want %d", len(model.rows), topDashboardHistory)
	}
}

func TestTopModelKeepsHistorySelectionWhileSampling(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: topSampleAt(1)}, topSampleMsg{snapshot: topSampleAt(2)})
	browsing := topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	if browsing.follow || browsing.selected != 0 {
		t.Fatalf("browsing state = %#v", browsing)
	}
	updated := topAfter(t, browsing, topSampleMsg{snapshot: topSampleAt(3)})
	if updated.selected != 0 || updated.follow || len(updated.rows) != 3 {
		t.Fatalf("sample changed history selection: %#v", updated)
	}
	latest := topAfter(t, updated, tea.KeyPressMsg{Code: tea.KeyEnd})
	if !latest.follow || latest.selected != 2 {
		t.Fatalf("End did not return to latest: %#v", latest)
	}
}

func TestTopModelChangesViewsAndPanels(t *testing.T) {
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: topSampleAt(1)})
	disk := topAfter(t, model, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if disk.view != topViewDisk || !strings.Contains(disk.View().Content, "read/s") {
		t.Fatalf("disk view = %q", disk.View().Content)
	}
	detail := topAfter(t, disk, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !detail.detail || !strings.Contains(detail.View().Content, "detail") {
		t.Fatalf("detail view = %q", detail.View().Content)
	}
	peaks := topAfter(t, detail, tea.KeyPressMsg{Code: 'h', Text: "h"})
	if !peaks.peaks || peaks.detail || !strings.Contains(peaks.View().Content, "peaks 60s") {
		t.Fatalf("peaks view = %q", peaks.View().Content)
	}
}

func TestTopSignalSummarizesHighestRisk(t *testing.T) {
	limits := newTopLimits(8, false)
	got := topSignal(resourceRate{MemoryPercent: 96, CPUIOWait: 30, Load1: 10}, limits)
	if !strings.Contains(got, "load 10.0") || !strings.Contains(got, "+2") {
		t.Fatalf("signal = %q", got)
	}
}

func TestTopSignalIncludesNetworkAndDiskHealth(t *testing.T) {
	limits := newTopLimits(8, false)
	if got := topSignal(resourceRate{DiskHealthValid: true, DiskAwait: 65, NetHealthValid: true, NetDrops: 2}, limits); got != "await 65ms +1" {
		t.Fatalf("signal = %q", got)
	}
	// 적은 drop이 memory 위험을 가리지 않고, 1/s 미만은 경고로 세지 않는다.
	if got := topSignal(resourceRate{MemoryPercent: 97, NetHealthValid: true, NetDrops: 1}, limits); got != "mem 97% +1" {
		t.Fatalf("drop outranked memory: %q", got)
	}
	if got := topSignal(resourceRate{NetHealthValid: true, NetDrops: 0.2, NetErrors: 0.4}, limits); got != "-" {
		t.Fatalf("sub-1/s network noise signalled: %q", got)
	}
	if got := topSignal(resourceRate{NetHealthValid: true, NetErrors: 60}, limits); got != "err 60/s" {
		t.Fatalf("error signal = %q", got)
	}
	if got := topDiskDetail(resourceRate{}); got != "disk health —" {
		t.Fatalf("disk fallback = %q", got)
	}
	if got := topNetworkDetail(resourceRate{}); got != "network health —" {
		t.Fatalf("network fallback = %q", got)
	}
}

func TestTopPressureAndCorePresentation(t *testing.T) {
	if got := topHotCore([]float64{10, 97, 50}); got != "1 97%" {
		t.Fatalf("hot core = %q", got)
	}
	if got := topCoreBar([]float64{10, 45, 75, 95}); got != ".:*#" {
		t.Fatalf("core bar = %q", got)
	}
	limits := newTopLimits(8, false)
	if got := topSignal(resourceRate{PSIValid: true, PSIIO: 30}, limits); got != "psi io 30%" {
		t.Fatalf("pressure signal = %q", got)
	}
}

func TestTopProcessDetailUsesTheSelectedSnapshot(t *testing.T) {
	processes := []topProcess{{PID: 4, CPU: 99, RSS: 2 * 1024 * 1024, Command: "java"}, {PID: 7, CPU: 12, RSS: 1024, Command: "node"}}
	got := topProcessDetail(processes, true)
	if !strings.Contains(got, "java 99% 2.0M") || !strings.Contains(got, "node 12%") {
		t.Fatalf("process detail = %q", got)
	}
	if got := topProcessDetail(nil, false); got != "processes —" {
		t.Fatalf("process fallback = %q", got)
	}
}

func TestTopDashboardSignalIncludesBusyProcess(t *testing.T) {
	limits := newTopLimits(8, false)
	processes := []topProcess{{CPU: 185, Command: "/usr/local/bin/node"}}
	if got := topDashboardSignal(resourceRate{}, processes, true, limits); got != "node 185%" {
		t.Fatalf("process signal = %q", got)
	}
	if got := topDashboardSignal(resourceRate{MemoryPercent: 96}, processes, true, limits); got != "node 185% +1" {
		t.Fatalf("combined signal = %q", got)
	}
	if _, ok := topProcessSignal([]topProcess{{CPU: 79, Command: "node"}}, true); ok {
		t.Fatal("process below threshold must not signal")
	}
	// process가 앞에 와도 가려진 경고 개수는 그대로 남는다.
	if got := topDashboardSignal(resourceRate{MemoryPercent: 96, CPUIOWait: 30, Load1: 10}, processes, true, limits); got != "node 185% +3" {
		t.Fatalf("combined signal count = %q", got)
	}
}

func TestTopProcessNameStripsPathsAndControlCharacters(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/node": "node",
		"kworker/0:1":         "kwor",
		"evil\x1b]0;x\a":      "evil",
		"":                    "proc",
	}
	for command, want := range cases {
		if got := topProcessName(command, topSignalProcessNameWidth); got != want {
			t.Fatalf("topProcessName(%q) = %q, want %q", command, got, want)
		}
	}
	if got := topProcessName("a\x1b[2Jb", topProcessNameWidth); got != "a?[2Jb" {
		t.Fatalf("control characters must be replaced: %q", got)
	}
}

func TestTopDashboardRowsFitTargetWidth(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{NetIn: 12 * 1024 * 1024, NetOut: 2 * 1024 * 1024, PacketsIn: 42, PacketsOut: 99, Load1: 2.5, CPUUser: 12, CPUSystem: 4, CPUIOWait: 1, DiskRead: 3 * 1024 * 1024, DiskWrite: 4 * 1024 * 1024, MemoryPercent: 55}}
	for _, view := range []topView{topViewAll, topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure} {
		for _, header := range topDashboardHeaders(view, false) {
			if got := len([]rune(header)); got > topTableWidth {
				t.Fatalf("%s header width = %d", view, got)
			}
		}
		if got := len([]rune(formatTopDashboardRow(row, view, newTopLimits(8, false), false))); got > topTableWidth {
			t.Fatalf("%s row width = %d", view, got)
		}
	}
}

func topDividerColumns(line string) []int {
	columns := []int{}
	for index, r := range []rune(line) {
		if r == '│' {
			columns = append(columns, index)
		}
	}
	return columns
}

func TestTopViewHeadersUseTheSameColumnsAsRows(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{CoreCPU: []float64{10, 95}, DiskHealthValid: true, DiskIOPS: 12, NetHealthValid: true, PSIValid: true}}
	for _, view := range []topView{topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure} {
		header := topDividerColumns(topDashboardHeaders(view, false)[0])
		value := topDividerColumns(formatTopDashboardRow(row, view, newTopLimits(8, false), false))
		if !reflect.DeepEqual(header, value) {
			t.Fatalf("%s dividers: header %v row %v", view, header, value)
		}
	}
	// 첫 헤더 줄의 group 구분선도 둘째 헤더 줄과 같은 칸에 있어야 한다.
	for _, wide := range []bool{false, true} {
		headers := topDashboardHeaders(topViewAll, wide)
		if top, bottom := topDividerColumns(headers[0]), topDividerColumns(headers[1]); !reflect.DeepEqual(top, bottom) {
			t.Fatalf("wide=%v group header %v, column header %v", wide, top, bottom)
		}
	}
}

func TestTopViewsShowDashForUnsupportedValues(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{MemoryPercent: 99}}
	memory := formatTopDashboardRow(row, topViewMemory, newTopLimits(8, false), false)
	if strings.Contains(memory, "ok") || !strings.Contains(memory, "—") {
		t.Fatalf("memory row = %q", memory)
	}
	disk := formatTopDashboardRow(row, topViewDisk, newTopLimits(8, false), false)
	if strings.Count(disk, "—") != 3 {
		t.Fatalf("disk row must show — for iops, await and busy: %q", disk)
	}
}

func TestTopDiskViewShowsDashOnlyForBusyWithoutBusyTime(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{DiskHealthValid: true, DiskIOPS: 321, DiskAwait: 4.5}}
	disk := formatTopDashboardRow(row, topViewDisk, newTopLimits(8, false), false)
	if strings.Count(disk, "—") != 1 || !strings.Contains(disk, "321") || !strings.Contains(disk, "4.5") {
		t.Fatalf("disk row must show iops and await with — for busy: %q", disk)
	}
	if detail := topDiskDetail(row.rate); !strings.Contains(detail, "321 iops") || !strings.Contains(detail, "busy —") {
		t.Fatalf("disk detail = %q", detail)
	}
}

func TestTopModelKeepsSelectedTimeWhenHistoryIsTrimmed(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = make([]topDashboardRow, topDashboardHistory)
	for index := range model.rows {
		model.rows[index].at = time.Unix(int64(index), 0)
	}
	model.selected, model.follow = 100, false
	picked := model.rows[model.selected].at
	model.previous = topSampleAt(topDashboardHistory)
	updated := topAfter(t, model, topSampleMsg{snapshot: topSampleAt(topDashboardHistory + 1)})
	if row, ok := updated.selectedRow(); !ok || row.at != picked || updated.follow {
		t.Fatalf("selected time moved from %v to %v", picked, row.at)
	}
	updated.selected = 0
	trimmed := topAfter(t, updated, topSampleMsg{snapshot: topSampleAt(topDashboardHistory + 2)})
	if trimmed.selected != 0 {
		t.Fatalf("selection below zero: %d", trimmed.selected)
	}
}

func TestTopSelectionMarkerKeepsTheTime(t *testing.T) {
	at := time.Date(2026, 1, 1, 9, 0, 1, 0, time.Local)
	model := topAfter(t, topFixtureModel(nil), topSampleMsg{snapshot: resourceSnapshot{TakenAt: at}}, topSampleMsg{snapshot: resourceSnapshot{TakenAt: at.Add(time.Second)}})
	browsing := topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	if !strings.Contains(browsing.View().Content, "09:00:01>│") {
		t.Fatalf("view = %q", browsing.View().Content)
	}
}

func TestTopStatusLinesAreMuted(t *testing.T) {
	model := topFixtureModel(nil)
	for _, line := range model.statusLines() {
		if !strings.HasPrefix(line, liveDim) || !strings.HasSuffix(line, liveReset) {
			t.Fatalf("status line is not muted: %q", line)
		}
		if got := liveWidth(line); got != topTableWidth {
			t.Fatalf("status line is %d wide: %q", got, line)
		}
	}
	model.limits.color = false
	if line := model.statusLines()[1]; strings.Contains(line, "\033[") {
		t.Fatalf("status line has escape without color: %q", line)
	}
}

func TestTopPanelsFitTheTableWidth(t *testing.T) {
	processes := []topProcess{{CPU: 185, RSS: 2 << 30, Command: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome Helper (Renderer)"}, {CPU: 99, RSS: 300 << 20, Command: "postgres"}, {CPU: 12, RSS: 1 << 20, Command: "node"}}
	rate := resourceRate{Load1: 12.5, CPUUser: 100, CPUSystem: 100, CPUIOWait: 30, MemoryPercent: 97.5, DiskHealthValid: true, DiskIOPS: 1234, DiskAwait: 12.3, DiskBusy: 100, NetHealthValid: true, NetErrors: 1, NetDrops: 2, PSIValid: true, PSICPU: 1, PSIMemory: 2, PSIIO: 3}
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), rate: rate, processes: processes, processesValid: true}}
	model.detail = true
	lines := model.detailLines()
	model.detail, model.peaks = false, true
	lines = append(lines, model.peakLines()...)
	for _, line := range lines {
		if got := len([]rune(line)); got > topTableWidth {
			t.Fatalf("panel line is %d wide and would be clipped: %q", got, line)
		}
	}
	if !strings.Contains(lines[2], "Google Chr 185% 2.0G") || !strings.Contains(lines[2], "node 12%") {
		t.Fatalf("process line = %q", lines[2])
	}
	// 패널 줄 수만큼 본문이 줄어 전체 높이를 넘지 않는다.
	model.detail, model.peaks = true, false
	if got := strings.Count(model.View().Content, "\n") + 1; got > 20 {
		t.Fatalf("view has %d lines for height 20", got)
	}
}

func TestTopPeaksTrackEachMetricSeparately(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{
		{at: time.Unix(0, 0), rate: resourceRate{Load1: 30, MemoryPercent: 99}},
		{at: time.Unix(100, 0), rate: resourceRate{Load1: 8, MemoryPercent: 50}},
		{at: time.Unix(110, 0), rate: resourceRate{Load1: 0.5, MemoryPercent: 60, CPUUser: 40, CPUSystem: 5, CPUIOWait: 7}},
	}
	lines := strings.Join(model.peakLines(), "\n")
	for _, expected := range []string{"load 8.0", "mem 60.0%", "cpu 45.0%", "iowait 7.0%"} {
		if !strings.Contains(lines, expected) {
			t.Fatalf("peaks %q does not contain %q", lines, expected)
		}
	}
	if strings.Contains(lines, "load 30.0") {
		t.Fatalf("row older than the peak window leaked: %q", lines)
	}
}

func TestTopAllViewUsesTheSameGroupBoundariesForHeaderAndRow(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{MemoryPercent: 55}}
	header := []rune(topDashboardHeaders(topViewAll, false)[1])
	value := []rune(formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), false))
	for _, column := range []int{9, 22, 44, 56, 62} {
		if header[column] != '│' || value[column] != '│' {
			t.Fatalf("group boundary %d: header %q row %q", column, header[column], value[column])
		}
	}
}

func TestTopWideLayoutAddsPacketsAndHealth(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{PacketsIn: 123, PacketsOut: 456, NetErrors: 2, NetDrops: 3, NetHealthValid: true, DiskIOPS: 789, DiskAwait: 12.3, DiskBusy: 80, DiskHealthValid: true, DiskBusyValid: true}}
	headers := topDashboardHeaders(topViewAll, true)
	line := formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), true)
	if len(headers) != 2 || len([]rune(line)) != topWideTableWidth || !strings.Contains(headers[1], "pk_in") {
		t.Fatalf("wide layout = %#v / %q", headers, line)
	}
	for _, expected := range []string{"123", "456", "789", "12.3", "80"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("wide row %q does not contain %q", line, expected)
		}
	}
}

func TestTopWideHeaderUsesTheSameColumnsAsRows(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0)}
	header := []rune(topDashboardHeaders(topViewAll, true)[1])
	value := []rune(formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), true))
	for _, column := range []int{9, 48, 79, 85, 115} {
		if header[column] != '│' || value[column] != '│' {
			t.Fatalf("wide boundary %d: header %q row %q", column, header[column], value[column])
		}
	}
}

func TestTopModelQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'q', Text: "q"}, {Code: 'c', Mod: tea.ModCtrl}} {
		if _, cmd := topFixtureModel(nil).Update(key); cmd == nil {
			t.Fatalf("%s must quit the dashboard", key.String())
		}
	}
}

func TestFormatTopRowMatchesPrintedRow(t *testing.T) {
	var output strings.Builder
	rate := resourceRate{NetIn: 0.04 * 1024 * 1024, CPUUser: 1, MemoryPercent: 11.8}
	at := time.Date(2026, 1, 1, 11, 36, 44, 0, time.UTC)
	printTopRow(&output, at, rate, newTopLimits(8, false))
	if strings.TrimRight(output.String(), "\n") != formatTopRow(at, rate, newTopLimits(8, false)) {
		t.Fatalf("printed row and formatted row differ: %q", output.String())
	}
}
