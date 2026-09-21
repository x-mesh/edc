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
		for _, header := range topDashboardHeaders(view, topTableWidth) {
			if got := len([]rune(header)); got > topTableWidth {
				t.Fatalf("%s header width = %d", view, got)
			}
		}
		if got := len([]rune(formatTopDashboardRow(row, view, newTopLimits(8, false), topTableWidth))); got > topTableWidth {
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
		header := topDividerColumns(topDashboardHeaders(view, topTableWidth)[0])
		value := topDividerColumns(formatTopDashboardRow(row, view, newTopLimits(8, false), topTableWidth))
		if !reflect.DeepEqual(header, value) {
			t.Fatalf("%s dividers: header %v row %v", view, header, value)
		}
	}
	// all 보기는 폭마다 칸 구성이 달라진다. 폭마다 두 헤더 줄과 행의 구분선이 같은 칸에 있어야 한다.
	for _, width := range []int{topTableWidth, 84, 96, 110, 120, 125, 160} {
		headers := topDashboardHeaders(topViewAll, width)
		value := topDividerColumns(formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), width))
		if top, bottom := topDividerColumns(headers[0]), topDividerColumns(headers[1]); !reflect.DeepEqual(top, bottom) || !reflect.DeepEqual(bottom, value) {
			t.Fatalf("width %d dividers: group %v, column %v, row %v", width, top, bottom, value)
		}
	}
}

func TestTopViewsShowDashForUnsupportedValues(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{MemoryPercent: 99}}
	memory := formatTopDashboardRow(row, topViewMemory, newTopLimits(8, false), topTableWidth)
	if strings.Contains(memory, "ok") || !strings.Contains(memory, "—") {
		t.Fatalf("memory row = %q", memory)
	}
	disk := formatTopDashboardRow(row, topViewDisk, newTopLimits(8, false), topTableWidth)
	if strings.Count(disk, "—") != 3 {
		t.Fatalf("disk row must show — for iops, await and busy: %q", disk)
	}
}

func TestTopDiskViewShowsDashOnlyForBusyWithoutBusyTime(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{DiskHealthValid: true, DiskIOPS: 321, DiskAwait: 4.5}}
	disk := formatTopDashboardRow(row, topViewDisk, newTopLimits(8, false), topTableWidth)
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

func TestTopAllViewAddsColumnsAsTheTerminalWidens(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{PacketsIn: 123, PacketsOut: 456, NetErrors: 2, NetDrops: 3, NetHealthValid: true, CoreCPU: []float64{10, 95}, DiskIOPS: 789, DiskAwait: 12.3, DiskBusy: 80, DiskHealthValid: true, DiskBusyValid: true}}
	steps := []struct {
		width         int
		shown, hidden []string
	}{
		{topTableWidth, []string{"in", "mem%", "write"}, []string{"hot core", "iops", "pk_in", "drop", "busy"}},
		{83, nil, []string{"hot core"}},
		{84, []string{"hot core"}, []string{"iops"}},
		{96, []string{"iops", "await"}, []string{"pk_in"}},
		{110, []string{"pk_in", "pk_out"}, []string{"drop"}},
		{120, []string{"err", "drop"}, []string{"busy"}},
		{125, []string{"busy"}, nil},
	}
	for _, step := range steps {
		header := topDashboardHeaders(topViewAll, step.width)[1]
		line := formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), step.width)
		if got := len([]rune(line)); got != step.width {
			t.Fatalf("width %d row is %d wide: %q", step.width, got, line)
		}
		for _, title := range step.shown {
			if !strings.Contains(header, title) {
				t.Fatalf("width %d header %q does not contain %q", step.width, header, title)
			}
		}
		for _, title := range step.hidden {
			if strings.Contains(header, title) {
				t.Fatalf("width %d header %q must not contain %q yet", step.width, header, title)
			}
		}
	}
	line := formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), 125)
	for _, expected := range []string{"123", "456", "789", "12.3", "80", "1 95%"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("widest row %q does not contain %q", line, expected)
		}
	}
}

func TestTopCompactCountKeepsTheColumnWidth(t *testing.T) {
	for input, want := range map[float64]string{42: "42", 9999: "9999", 12345: "12k", 1234567: "1M"} {
		if got := topCompactCount(input, 4); got != want {
			t.Fatalf("topCompactCount(%v, 4) = %q, want %q", input, got, want)
		}
	}
}

func TestTopAllViewListsSignalsInTheSpareWidth(t *testing.T) {
	signals := []topSignalItem{{text: "node 185%"}, {text: "mem 96%"}, {text: "load 10.0"}}
	for width, want := range map[int]string{13: "node 185% +2", 24: "node 185% · mem 96% +1", 40: "node 185% · mem 96% · load 10.0"} {
		if got := formatTopSignalsWidth(signals, width); got != want {
			t.Fatalf("width %d signals = %q, want %q", width, got, want)
		}
	}
	if got := formatTopSignalsWidth(nil, 40); got != "-" {
		t.Fatalf("empty signals = %q", got)
	}
}

func TestTopDashboardTitleAddsHostDetailsAsTheTerminalWidens(t *testing.T) {
	model := topFixtureModel(nil)
	model.details = hostDetails{Hostname: "host", System: "Linux", OS: "ubuntu", Version: "Ubuntu 24.04.1 LTS", Model: "Intel(R) Xeon(R) Platinum 8375C CPU @ 2.90GHz", Cores: 8, MemoryTotal: 64 << 30}
	steps := []struct {
		width         int
		shown, hidden []string
	}{
		{topTableWidth, []string{"🐰 host · Ubuntu 24.04.1 LTS · 8 cores · 64.00 GB"}, []string{"Xeon"}},
		{120, []string{"🐰 host · Ubuntu 24.04.1 LTS · Intel(R) Xeon(R) Platinum 8375C CPU @ 2.90GHz · 8 cores · 64.00 GB"}, nil},
	}
	for _, step := range steps {
		model.width = step.width
		title := model.dashboardTitle()
		// 상태는 표 오른쪽 끝에 붙으므로 제목 폭이 표 폭과 같다.
		if got := topDisplayWidth(title); got != step.width || !strings.HasSuffix(title, "  view all · live 🐰") {
			t.Fatalf("width %d title is %d wide: %q", step.width, got, title)
		}
		for _, text := range step.shown {
			if !strings.HasPrefix(title, text) {
				t.Fatalf("width %d title %q does not start with %q", step.width, title, text)
			}
		}
		for _, text := range step.hidden {
			if strings.Contains(title, text) {
				t.Fatalf("width %d title %q must not contain %q", step.width, title, text)
			}
		}
	}
	// edc 버전은 OS와 memory 다음에 들어가므로, 폭이 모자라면 긴 CPU 모델보다 먼저 남는다.
	model.version, model.width = "0.9.0", 120
	if title := model.dashboardTitle(); !strings.HasSuffix(title, "  view all · live · edc 0.9.0 🐰") || strings.Contains(title, "Xeon") {
		t.Fatalf("title with version = %q", title)
	}
	model.version = ""
	// 다른 보기의 표는 80열이므로 상태도 80열 끝에 둔다.
	model.view, model.width = topViewCPU, 160
	if title := model.dashboardTitle(); topDisplayWidth(title) != topTableWidth || !strings.HasSuffix(title, "view cpu · live 🐰") {
		t.Fatalf("cpu view title = %q", title)
	}
	// host 이름이 길어 양쪽으로 뗄 수 없으면 한 줄로 잇는다.
	model.view, model.width = topViewAll, topTableWidth
	model.details.Hostname = strings.Repeat("h", 70)
	if title := model.dashboardTitle(); !strings.HasSuffix(title, " · 8 cores · view all · live 🐰") {
		t.Fatalf("long host title = %q", title)
	}
	if got := topHostOS(hostDetails{System: "darwin", OS: "macOS", Version: "26.0"}); got != "macOS 26.0" {
		t.Fatalf("macOS name = %q", got)
	}
}

func TestTopModelPagesThroughHistory(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = make([]topDashboardRow, 100)
	for index := range model.rows {
		model.rows[index].at = time.Unix(int64(index), 0)
	}
	model.selected = len(model.rows) - 1
	page := model.bodyLines()
	up := topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if up.follow || up.selected != 99-page {
		t.Fatalf("PgUp selected %d, want %d", up.selected, 99-page)
	}
	first := up
	for range 20 {
		first = topAfter(t, first, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	if first.selected != 0 || first.follow {
		t.Fatalf("PgUp must stop at the first row: %d", first.selected)
	}
	down := topAfter(t, first, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if down.selected != page || down.follow {
		t.Fatalf("PgDn selected %d, want %d", down.selected, page)
	}
	last := down
	for range 20 {
		last = topAfter(t, last, tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	if last.selected != 99 || !last.follow {
		t.Fatalf("PgDn must return to the live row: selected %d, follow %v", last.selected, last.follow)
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

// topAlertRate는 임계치를 넘긴 값만 모은 표본이다. core 8 기준으로 load·cpu·io·mem·await·err·psi가 모두 경고나 위험이다.
func topAlertRate() resourceRate {
	return resourceRate{
		Load1: 12, CPUUser: 95, CPUSystem: 75, CPUIOWait: 30, MemoryPercent: 97, CoreCPU: []float64{10, 99},
		DiskHealthValid: true, DiskIOPS: 900, DiskAwait: 80,
		NetHealthValid: true, NetErrors: 60, NetDrops: 5,
		PSIValid: true, PSICPU: 30, PSIMemory: 12, PSIIO: 40,
	}
}

func topDashboardViews() []topView {
	return []topView{topViewAll, topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure}
}

func topColoredRow(view topView, rate resourceRate, width int) string {
	return formatTopDashboardRow(topDashboardRow{at: time.Unix(1, 0), rate: rate}, view, newTopLimits(8, true), width)
}

func TestTopDashboardPaintsValuesOverTheThreshold(t *testing.T) {
	tests := []struct {
		name string
		view topView
		want string
	}{
		{"all load danger", topViewAll, topColorDanger + "12.0" + topColorReset},
		{"all system warn", topViewAll, topColorWarn + " 75.0" + topColorReset},
		{"all memory danger", topViewAll, topColorDanger + " 97.0" + topColorReset},
		{"memory percent danger", topViewMemory, topColorDanger + "  97.0" + topColorReset},
		{"disk await danger", topViewDisk, topColorDanger + "  80.0" + topColorReset},
		{"network drop warn", topViewNetwork, topColorWarn + "     5" + topColorReset},
		{"pressure memory warn", topViewPressure, topColorWarn + "   12.0" + topColorReset},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line := topColoredRow(test.view, topAlertRate(), topTableWidth)
			if !strings.Contains(line, test.want) {
				t.Fatalf("%s row %q does not contain %q", test.view, line, test.want)
			}
		})
	}
}

func TestTopDashboardLeavesNormalValuesUncolored(t *testing.T) {
	rate := resourceRate{Load1: 1, CPUUser: 5, CPUSystem: 2, CPUIOWait: 1, MemoryPercent: 30, CoreCPU: []float64{10, 20},
		DiskHealthValid: true, DiskIOPS: 12, DiskAwait: 3, NetHealthValid: true, PSIValid: true, PSICPU: 1}
	for _, view := range topDashboardViews() {
		if line := topColoredRow(view, rate, topTableWidth); strings.Contains(line, "\033[") {
			t.Fatalf("%s row colors a normal value: %q", view, line)
		}
	}
}

// 대시보드는 platform이 주지 않는 값을 —로 그린다. 값이 없으므로 색도 없어야 한다.
func TestTopDashboardDoesNotPaintUnsupportedValues(t *testing.T) {
	rate := resourceRate{DiskAwait: 80, NetErrors: 60, PSIIO: 40}
	for _, view := range topDashboardViews() {
		if line := topColoredRow(view, rate, 160); strings.Contains(line, "\033[") {
			t.Fatalf("%s row colors an unsupported value: %q", view, line)
		}
	}
}

// 색을 입혀도 칸이 밀리면 안 된다. escape를 폭 계산에서 빼고 색 없는 줄과 같은 폭인지 본다.
func TestTopDashboardKeepsTheWidthWithColor(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: topAlertRate()}
	for _, width := range []int{topTableWidth, 84, 96, 110, 120, 125, 160} {
		for _, view := range topDashboardViews() {
			plain := formatTopDashboardRow(row, view, newTopLimits(8, false), width)
			colored := formatTopDashboardRow(row, view, newTopLimits(8, true), width)
			if strings.Contains(plain, "\033[") {
				t.Fatalf("%s row has an escape without color: %q", view, plain)
			}
			if !strings.Contains(colored, "\033[") {
				t.Fatalf("%s row at width %d has no color: %q", view, width, colored)
			}
			if liveWidth(colored) != liveWidth(plain) {
				t.Fatalf("%s row at width %d is %d wide, want %d: %q", view, width, liveWidth(colored), liveWidth(plain), colored)
			}
			for _, header := range topDashboardHeaders(view, width) {
				if strings.Contains(header, "\033[") {
					t.Fatalf("%s header has an escape: %q", view, header)
				}
			}
		}
	}
}

// hot core는 core 하나의 사용률이라 host의 cpu 임계치보다 늦게 켜고 위험 단계를 두지 않는다.
func TestTopDashboardWarnsOnlyForAFullHotCore(t *testing.T) {
	tests := map[float64]string{72: "", 89: "", 90: topColorWarn, 100: topColorWarn}
	for usage, want := range tests {
		rate := resourceRate{CoreCPU: []float64{10, usage}}
		line := topColoredRow(topViewCPU, rate, topTableWidth)
		if want == "" {
			if strings.Contains(line, "\033[") {
				t.Fatalf("hot core %.0f%% must stay uncolored: %q", usage, line)
			}
			continue
		}
		if !strings.Contains(line, want) || strings.Contains(line, topColorDanger) {
			t.Fatalf("hot core %.0f%% must warn only: %q", usage, line)
		}
	}
}
