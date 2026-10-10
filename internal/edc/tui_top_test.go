package edc

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	for _, expected := range []string{"network", "Tab processes", "? help", "q quit", "interval 1s"} {
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

func TestTopSignalReportsBlockedTasksOnlyWithHighIOWait(t *testing.T) {
	limits := newTopLimits(4, false)
	items := topSignals(resourceRate{CPUIOWait: 78, ProcsBlocked: 14, ProcsBlockedSource: topBlockedKernelTasks}, limits)
	if len(items) != 2 || items[1].text != "blocked 14" || items[1].view != topViewDisk {
		t.Fatalf("items = %+v", items)
	}
	for name, rate := range map[string]resourceRate{
		"low iowait":   {CPUIOWait: 2, ProcsBlocked: 14, ProcsBlockedSource: topBlockedKernelTasks},
		"few blocked":  {CPUIOWait: 78, ProcsBlocked: 3, ProcsBlockedSource: topBlockedKernelTasks},
		"not measured": {CPUIOWait: 78, ProcsBlocked: 14},
	} {
		for _, item := range topSignals(rate, limits) {
			if strings.HasPrefix(item.text, "blocked") {
				t.Fatalf("%s must not signal blocked: %+v", name, item)
			}
		}
	}
}

func TestTopDiskCandidatesPutBlockedProcessesFirst(t *testing.T) {
	processes := []topProcess{
		{PID: 1, CPU: 20, Command: "busy"},
		{PID: 2, CPU: 5, Command: "gm", State: "D"},
		{PID: 3, CPU: 3, Command: "gm", State: "D"},
		{PID: 4, CPU: 1, Command: "idle"},
	}
	for view, want := range map[topView][]int{topViewDisk: {2, 3, 1, 4}, topViewCPU: {1, 2, 3, 4}} {
		model := topFixtureModel(nil)
		model.width, model.height = 120, topTallHeight
		model.view = view
		model.rows = append(model.rows, topDashboardRow{at: time.Unix(1, 0), processesValid: true, processes: processes})
		model.selected = len(model.rows) - 1
		got := []int{}
		for _, process := range model.candidates() {
			got = append(got, process.PID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("view %v order = %v, want %v", view, got, want)
		}
	}
}

func TestTopDiskCandidatesSortByIOAndShowRates(t *testing.T) {
	processes := []topProcess{
		{PID: 1, CPU: 20, Command: "busy"},
		{PID: 2, CPU: 5, Command: "gm", State: "D", DiskValid: true, DiskRead: 1 << 20, DiskWrite: 2 << 20},
		{PID: 3, CPU: 3, Command: "node", DiskValid: true, DiskRead: 40 << 20},
		{PID: 4, CPU: 1, Command: "unreadable", State: "D"},
	}
	model := topFixtureModel(nil)
	model.limits.color = false
	model.width, model.height = 120, topTallHeight
	model.view = topViewDisk
	model.rows = append(model.rows, topDashboardRow{at: time.Unix(1, 0), processesValid: true, processes: processes})
	model.selected = len(model.rows) - 1
	got := []int{}
	for _, process := range model.candidates() {
		got = append(got, process.PID)
	}
	if want := []int{3, 2, 4, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	lines := model.candidateLines()
	if !strings.Contains(lines[1], "STATE") || !strings.Contains(lines[1], "READ") || !strings.Contains(lines[1], "WRITE") {
		t.Fatalf("header = %q", lines[1])
	}
	for _, want := range []string{"   3 node", "   2 gm", "   4 unreadable"} {
		found := false
		for _, line := range lines {
			found = found || strings.Contains(line, want)
		}
		if !found {
			t.Fatalf("%q missing in %q", want, lines)
		}
	}
	node, gm, unreadable := lines[2], lines[3], lines[4]
	if !strings.HasSuffix(node, " 40.0M  0.00M") || !strings.HasSuffix(gm, " 1.00M  2.00M") || !strings.HasSuffix(unreadable, "     —      —") {
		t.Fatalf("I/O columns:\n%s\n%s\n%s", node, gm, unreadable)
	}
	if index := strings.Index(lines[1], "STATE"); index < 0 || !strings.HasPrefix(gm[index:], "iowait") || !strings.HasPrefix(node[index:], "      ") {
		t.Fatalf("state column misaligned:\n%s\n%s", lines[1], gm)
	}
	model.view = topViewCPU
	if content := model.View().Content; !strings.Contains(content, "40.0M") {
		t.Fatalf("a measured rate must also show in other views: %q", content)
	}
}

func TestTopProcessStateNames(t *testing.T) {
	for state, want := range map[string]string{"R": "run", "S": "sleep", "D": "iowait", "Z": "zombie", "T": "stop", "I": "idle", "W": "W", "": ""} {
		if got := topProcessStateName(state); got != want {
			t.Fatalf("state %q = %q, want %q", state, got, want)
		}
		if len(want) > topProcessStateWidth {
			t.Fatalf("state %q name %q is wider than the column", state, want)
		}
	}
}

func TestTopDiskCandidatesColorHeavyIOAndShowGroups(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = true
	model.width, model.height = topFooterProcessWidth, topTallHeight
	model.view = topViewDisk
	processes := []topProcess{
		{PID: 1, CPU: 30, Command: "node", DiskValid: true, DiskRead: 60 << 20},
		{PID: 2, CPU: 1, Command: "gm", State: "D", DiskValid: true, DiskWrite: 12 << 20},
		{PID: 3, CPU: 1, Command: "gm", DiskValid: true, DiskWrite: 1 << 20},
	}
	total := topProcessTotal{Groups: []topProcessGroup{{Name: "gm", Count: 200, Blocked: 150, CPU: 100, RSS: 460 << 20, DiskValid: true, DiskRead: 200 << 20, DiskWrite: 400 << 20}}}
	model.rows = append(model.rows, topDashboardRow{at: time.Unix(1, 0), processesValid: true, processes: processes, processTotal: total})
	model.selected = len(model.rows) - 1
	lines := model.candidateLines()
	for _, line := range lines[1:] {
		if width := ansi.StringWidth(line); width > topFooterProcessWidth {
			t.Fatalf("line is %d columns in a %d column panel: %q", width, topFooterProcessWidth, line)
		}
	}
	group := lines[2]
	// 63칸 패널의 이름 칸에는 I/O 대기 수까지 들어가지 않아 process 수만 남는다.
	if strings.Contains(group, "×") || !strings.Contains(group, "gm (200 procs)") || !strings.Contains(group, topColorDanger+"  400M") {
		t.Fatalf("group line = %q", group)
	}
	if !strings.Contains(lines[3], topColorDanger+" 60.0M") || !strings.Contains(lines[4], topColorWarn+" 12.0M") || !strings.Contains(lines[4], topColorWarn+"iowait") {
		t.Fatalf("colors:\n%q\n%q", lines[3], lines[4])
	}
	if strings.Contains(lines[5], topColorWarn) || strings.Contains(lines[5], topColorDanger) {
		t.Fatalf("a small writer must stay plain: %q", lines[5])
	}
	// 고른 줄은 반전 한 번으로 그려야 하고, 칸 색의 reset이 반전을 끊으면 안 된다.
	model.processFocus, model.processSelected = true, 0
	selected := model.candidateLines()[3]
	if strings.Count(selected, topColorReset) != 1 {
		t.Fatalf("selected line = %q", selected)
	}
	model.processFilter, _ = parseTopProcessFilter("gm")
	for _, line := range model.candidateLines() {
		if strings.Contains(line, "procs)") {
			t.Fatalf("a filtered view must not show groups: %q", line)
		}
	}
}

func TestTopCPUAndMemoryViewsShowGroups(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = false
	model.width, model.height = topFooterProcessWidth, topTallHeight
	processes := []topProcess{{PID: 1, CPU: 30, RSS: 500 << 20, Command: "node"}}
	total := topProcessTotal{Groups: []topProcessGroup{
		{Name: "gm", Count: 200, CPU: 100, RSS: 400 << 20},
		{Name: "php-fpm", Count: 10, CPU: 20, RSS: 800 << 20},
	}}
	model.rows = append(model.rows, topDashboardRow{at: time.Unix(1, 0), processesValid: true, processes: processes, processTotal: total})
	model.selected = len(model.rows) - 1
	for view, want := range map[topView][]string{topViewCPU: {"gm (200 procs)", "php-fpm (10 procs)"}, topViewMemory: {"php-fpm (10 procs)", "gm (200 procs)"}} {
		model.view = view
		lines := model.candidateLines()
		if !strings.Contains(lines[2], want[0]) || !strings.Contains(lines[3], want[1]) {
			t.Fatalf("view %v groups:\n%s", view, strings.Join(lines, "\n"))
		}
		for _, line := range lines[1:] {
			if width := ansi.StringWidth(line); width > topFooterProcessWidth {
				t.Fatalf("view %v line is %d columns: %q", view, width, line)
			}
		}
	}
	model.view = topViewMemory
	if line := model.candidateLines()[3]; !strings.Contains(line, "≤400M") {
		t.Fatalf("a group RSS must be marked as an upper bound: %q", line)
	}
	// 묶음이 후보 줄을 모두 밀어내면 고를 process가 없어진다.
	model.height = 0
	for height := 8; height <= 12; height++ {
		model.height = height
		found := false
		for _, line := range model.candidateLines() {
			found = found || strings.Contains(line, "node")
		}
		if !found {
			t.Fatalf("height %d hides every process: %q", height, model.candidateLines())
		}
	}
}

func TestTopProcessGroupLabelShrinksToFit(t *testing.T) {
	group := topProcessGroup{Name: "gm", Count: 200, Blocked: 150}
	for width, want := range map[int]string{30: "gm (200 procs, 150 iowait)", 17: "gm (200 procs)"} {
		if got := topProcessGroupLabel(group, width); got != want {
			t.Fatalf("width %d label = %q, want %q", width, got, want)
		}
	}
	if got := topProcessGroupLabel(topProcessGroup{Name: "php-fpm-worker", Count: 10}, 14); got != "php (10 procs)" {
		t.Fatalf("narrow label = %q", got)
	}
}

func TestTopProcessGroupSignals(t *testing.T) {
	groups := []topProcessGroup{
		{Name: "gm", Count: 200, CPU: 100, DiskValid: true, DiskWrite: 400 << 20},
		{Name: "php-fpm", Count: 10, CPU: 20},
	}
	items := topProcessGroupSignals(groups)
	if len(items) != 2 || items[0].text != "gm (200) 100%" || items[0].view != topViewCPU || items[1].text != "gm (200) 400M" || items[1].view != topViewDisk {
		t.Fatalf("items = %+v", items)
	}
	// I/O를 읽지 못한 묶음이나 기준 아래 묶음은 경고하지 않는다.
	if items := topProcessGroupSignals([]topProcessGroup{{Name: "gm", Count: 200, CPU: 79, Blocked: 150}}); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
	if got := topDashboardSignal(resourceRate{}, nil, groups, true, newTopLimits(4, false)); got != "gm (200) 100% +1" {
		t.Fatalf("signal = %q", got)
	}
	if got := topDashboardSignal(resourceRate{}, nil, groups, false, newTopLimits(4, false)); got != "-" {
		t.Fatalf("an invalid sample must not signal: %q", got)
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
	model := topFixtureModel(nil)
	model.view = topViewPressure
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{PSIValid: true, PSIMemoryFull: 12.5, PSIIOFull: 7.25}}
	if header := model.tableHeader()[0]; !strings.Contains(header, "mem full") || !strings.Contains(header, "io full") {
		t.Fatalf("pressure header = %q", header)
	}
	if line := model.tableRow(row); !strings.Contains(line, "12.5") || !strings.Contains(line, "7.2") {
		t.Fatalf("pressure row = %q", line)
	}
	if line := model.tableRow(topDashboardRow{at: time.Unix(1, 0)}); strings.Count(line, "—") < 5 {
		t.Fatalf("pressure row without PSI = %q", line)
	}
}

func TestTopCoresColumnFollowsTheCoreCount(t *testing.T) {
	model := topFixtureModel(nil)
	model.view = topViewCPU
	for _, row := range []struct{ cores, width int }{{0, topCoreBarLimit}, {2, len("cores")}, {12, 12}, {40, topCoreBarLimit}} {
		model.previous.Cores = make([]resourceCPU, row.cores)
		columns, _ := model.tableColumns()
		for _, column := range columns {
			if column.title == "cores" && column.width != row.width {
				t.Errorf("%d cores: width = %d, want %d", row.cores, column.width, row.width)
			}
		}
		header, line := model.tableHeader()[0], model.tableRow(topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{CoreCPU: make([]float64, row.cores)}})
		if ansi.StringWidth(header) != ansi.StringWidth(line) {
			t.Errorf("%d cores: header %d columns, row %d columns", row.cores, ansi.StringWidth(header), ansi.StringWidth(line))
		}
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

func TestTopMatchDetailSumsEveryMatchAndNamesTheRest(t *testing.T) {
	processes := []topProcess{
		{PID: 1, CPU: 50, RSS: 3 << 20, Command: "worker-a", FDs: 10, DiskValid: true, DiskRead: 1 << 20, DiskWrite: 2 << 20},
		{PID: 2, CPU: 40, RSS: 2 << 20, Command: "worker-b", FDs: 5, DiskValid: true, DiskWrite: 1 << 20},
	}
	total := topProcessTotal{Count: 5, CPU: 130, RSS: 9 << 20, Threads: 12}
	lines := topMatchDetail(processes, total, true)
	if len(lines) != 2 || lines[0] != "match 5 · cpu 130% · rss 9.0M · thr 12 · fds 15 · io r 1.00M/s w 3.00M/s" || lines[1] != "  worker-a 50% 3.0M, worker-b 40% 2.0M, +3" {
		t.Fatalf("match detail = %q", lines)
	}
	if got := topMatchDetail(nil, topProcessTotal{}, true); len(got) != 1 || got[0] != "match none" {
		t.Fatalf("no match detail = %q", got)
	}
	if got := topMatchDetail(nil, topProcessTotal{}, false); len(got) != 1 || got[0] != "processes —" {
		t.Fatalf("no sample detail = %q", got)
	}
	quiet := topMatchDetail([]topProcess{{PID: 3, CPU: 1, Command: "x"}}, topProcessTotal{Count: 1, CPU: 1}, true)
	if strings.Contains(quiet[0], "fds") || strings.Contains(quiet[0], "io r") || strings.Contains(quiet[0], "thr") {
		t.Fatalf("unknown values must not appear: %q", quiet)
	}
}

func TestTopDashboardSignalIncludesBusyProcess(t *testing.T) {
	limits := newTopLimits(8, false)
	processes := []topProcess{{CPU: 185, Command: "/usr/local/bin/node"}}
	if got := topDashboardSignal(resourceRate{}, processes, nil, true, limits); got != "node 185%" {
		t.Fatalf("process signal = %q", got)
	}
	if got := topDashboardSignal(resourceRate{MemoryPercent: 96}, processes, nil, true, limits); got != "mem 96% +1" {
		t.Fatalf("combined signal = %q", got)
	}
	if _, ok := topProcessSignal([]topProcess{{CPU: 79, Command: "node"}}, true); ok {
		t.Fatal("process below threshold must not signal")
	}
	if got := topDashboardSignal(resourceRate{MemoryPercent: 96, CPUIOWait: 30, Load1: 10}, processes, nil, true, limits); got != "load 10.0 +3" {
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

func TestTopProcessNameKeepsTheCommandTailWhenFull(t *testing.T) {
	previous := topFullCommand
	topFullCommand = true
	t.Cleanup(func() { topFullCommand = previous })
	command := "bun /var/folders/j3/x/T/bunx-501-output-mesh@latest/node_modules/.bin/output-mesh"
	if got := topProcessName(command, 28); got != "bun …/.bin/output-mesh" {
		t.Fatalf("full name = %q", got)
	}
	// 인자가 다 들어가면 자르지 않는다.
	if got := topProcessName("dig +short example.com", 28); got != "dig +short example.com" {
		t.Fatalf("short command line = %q", got)
	}
	// 칸이 좁으면 실행 파일 이름만 남는다.
	if got := topProcessName(command, topSignalProcessNameWidth); got != "bun" {
		t.Fatalf("narrow name = %q", got)
	}
}

func TestTopFilterSeedKeepsTheExecutableNameOnly(t *testing.T) {
	// macOS의 comm은 경로째로 온다. 마지막 조각만 남아야 입력 줄이 폭에 밀리지 않는다.
	if got := topFilterSeed("/System/Library/Frameworks/Security.framework/Versions/A/Resources/CloudKeychainProxy"); got != "CloudKeychainProxy" {
		t.Fatalf("seed = %q", got)
	}
	if got := topFilterSeed(""); got != "" {
		t.Fatalf("empty command seed = %q", got)
	}
	// 경로가 아닌 이름은 표에 보이는 그대로 둔다.
	if got := topFilterSeed("kworker/0:1"); got != "kworker/0:1" {
		t.Fatalf("name that is not a path = %q", got)
	}
	// login shell은 ps가 앞에 -를 붙여 준다.
	if got := topFilterSeed("-/Applications/term-mesh.app/Contents/Resources/bin/term-mesh-peer-relay"); got != "term-mesh-peer-relay" {
		t.Fatalf("login shell seed = %q", got)
	}
	previous := topFullCommand
	topFullCommand = true
	t.Cleanup(func() { topFullCommand = previous })
	command := "bun /var/folders/j3/x/T/bunx-501-output-mesh@latest/node_modules/.bin/output-mesh"
	seed := topFilterSeed(command)
	if seed != "bun" {
		t.Fatalf("full command seed = %q", seed)
	}
	filter, err := parseTopProcessFilter(seed)
	if err != nil {
		t.Fatal(err)
	}
	if !filter.match(topProcess{PID: 46800, Command: command}) {
		t.Fatal("the seed must match the process it came from")
	}
}

func TestTopDashboardRowsFitTargetWidth(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{NetIn: 12 * 1024 * 1024, NetOut: 2 * 1024 * 1024, PacketsIn: 42, PacketsOut: 99, Load1: 2.5, CPUUser: 12, CPUSystem: 4, CPUIOWait: 1, DiskRead: 3 * 1024 * 1024, DiskWrite: 4 * 1024 * 1024, MemoryPercent: 55}}
	for _, header := range topDashboardHeaders(topViewAll, topTableWidth) {
		if got := len([]rune(header)); got > topTableWidth {
			t.Fatalf("all header width = %d", got)
		}
	}
	if got := len([]rune(formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), topTableWidth))); got > topTableWidth {
		t.Fatalf("all row width = %d", got)
	}
	// 보기 칸은 tableColumns가 폭에 맞춰 고른다. 뒤에 붙인 optional 칸이 들어가도 80열을 넘지 않는다.
	model := topFixtureModel(nil)
	for _, view := range []topView{topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure} {
		model.view = view
		if got := ansi.StringWidth(model.tableHeader()[0]); got > topTableWidth {
			t.Fatalf("%s header width = %d", view, got)
		}
		if got := ansi.StringWidth(model.tableRow(row)); got > topTableWidth {
			t.Fatalf("%s row width = %d", view, got)
		}
	}
}

func TestTopOptionalColumnsKeepTheSignalWidth(t *testing.T) {
	model := topFixtureModel(nil)
	header := func(view topView, width, cores int) string {
		model.view, model.width = view, width
		model.previous.Cores = make([]resourceCPU, cores)
		return model.tableHeader()[0]
	}
	signalWidth := func(header string) int {
		_, signal, _ := strings.Cut(header, "signal")
		return len("signal") + len(signal)
	}
	for _, row := range []struct {
		view    topView
		width   int
		cores   int
		present []string
		absent  []string
	}{
		// 80열 기존 칸은 그대로다. 24 core의 cpu 보기와 network 보기에는 새 칸이 들어갈 자리가 없다.
		{topViewCPU, 80, 24, []string{"cores"}, []string{"steal%", "blocked"}},
		{topViewCPU, 80, 12, []string{"cores", "steal%"}, []string{"blocked"}},
		{topViewCPU, 120, 24, []string{"steal%", "blocked"}, nil},
		{topViewDisk, 80, 0, []string{"busy%", "queue"}, nil},
		{topViewNetwork, 80, 0, []string{"pk_out"}, []string{"retr/s", "rst/s", "fail/s"}},
		{topViewNetwork, 130, 0, []string{"retr/s", "rst/s", "fail/s"}, nil},
	} {
		got := header(row.view, row.width, row.cores)
		for _, title := range row.present {
			if !strings.Contains(got, title) {
				t.Errorf("%s %d cols %d cores: %s missing in %q", row.view, row.width, row.cores, title, got)
			}
		}
		for _, title := range row.absent {
			if strings.Contains(got, title) {
				t.Errorf("%s %d cols %d cores: %s shown in %q", row.view, row.width, row.cores, title, got)
			}
		}
		optional := false
		for _, title := range []string{"steal%", "blocked", "queue", "retr/s", "rst/s", "fail/s"} {
			optional = optional || strings.Contains(got, title)
		}
		if optional && signalWidth(got) < topSignalMinWidth {
			t.Errorf("%s %d cols: signal is %d columns in %q", row.view, row.width, signalWidth(got), got)
		}
	}
	darwin := topFixtureModel(nil)
	darwin.details.System = "darwin"
	darwin.width = 200
	for _, view := range []topView{topViewCPU, topViewDisk, topViewNetwork} {
		darwin.view = view
		for _, title := range []string{"steal%", "queue", "retr/s", "rst/s", "fail/s"} {
			if got := darwin.tableHeader()[0]; strings.Contains(got, title) {
				t.Errorf("darwin %s shows %s: %q", view, title, got)
			}
		}
	}
	// macOS는 U 상태 process 수로 blocked를 채우므로 그 열을 보인다.
	darwin.view = topViewCPU
	if got := darwin.tableHeader()[0]; !strings.Contains(got, "blocked") {
		t.Errorf("darwin cpu hides blocked: %q", got)
	}
	rate := resourceRate{CPUSteal: 12.5, CPUStealValid: true, ProcsBlocked: 3, ProcsBlockedSource: topBlockedKernelTasks, DiskQueue: 2.25, DiskHealthValid: true, DiskBusyValid: true}
	model.width, model.view = 200, topViewCPU
	if line := model.tableRow(topDashboardRow{at: time.Unix(1, 0), rate: rate}); !strings.Contains(line, "12.5") || !strings.Contains(line, "│      3│") {
		t.Errorf("cpu row = %q", line)
	}
	model.view = topViewDisk
	if line := model.tableRow(topDashboardRow{at: time.Unix(1, 0), rate: rate}); !strings.Contains(line, "2.2") {
		t.Errorf("disk row = %q", line)
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
	for _, view := range []topView{topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure, topViewProcess} {
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
	if strings.Count(disk, "—") != 4 {
		t.Fatalf("disk row must show — for iops, await, busy and queue: %q", disk)
	}
}

func TestTopDiskViewShowsDashOnlyForBusyWithoutBusyTime(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{DiskHealthValid: true, DiskIOPS: 321, DiskAwait: 4.5}}
	disk := formatTopDashboardRow(row, topViewDisk, newTopLimits(8, false), topTableWidth)
	if strings.Count(disk, "—") != 2 || !strings.Contains(disk, "321") || !strings.Contains(disk, "4.5") {
		t.Fatalf("disk row must show iops and await with — for busy and queue: %q", disk)
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
		{topTableWidth, []string{"in", "mem%", "write"}, []string{"hot core", "iops", "pk_in", "drop", "busy", "psi", "swap", "listen", "soft", "ct%"}},
		{83, nil, []string{"hot core"}},
		{84, []string{"hot core"}, []string{"iops"}},
		{96, []string{"iops", "await"}, []string{"pk_in"}},
		{110, []string{"pk_in", "pk_out"}, []string{"drop"}},
		{120, []string{"err", "drop"}, []string{"busy"}},
		{125, []string{"busy"}, []string{"psi", "swap", "listen", "soft", "ct%"}},
		{142, nil, []string{"psi"}},
		{143, []string{"psi"}, []string{"swap"}},
		{149, []string{"swap"}, []string{"listen"}},
		{161, []string{"listen", "soft"}, []string{"ct%"}},
		{167, []string{"ct%"}, nil},
	}
	for _, step := range steps {
		// psi는 group 이름이라 첫 헤더 줄에만 있다. 칸 제목 cpu·mem·io는 cpu group 이름, mem%, iops와 구별되지 않는다.
		header := strings.Join(topDashboardHeaders(topViewAll, step.width), "\n")
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

func TestTopAllViewShowsPressureSwapAndNetworkLimitsWhenWide(t *testing.T) {
	rate := func(value float64) networkCounterRate {
		return networkCounterRate{Status: "observed", PerSecond: &value}
	}
	health := &networkHealthRate{
		networkHealth: networkHealth{Supported: true,
			Gauges:   map[string]networkReading{"conntrack_entries": networkNumber(100)},
			Settings: map[string]networkReading{"net.netfilter.nf_conntrack_max": networkNumber(1000)}},
		Rates: map[string]networkCounterRate{"listen_overflows": rate(0.5), "softnet_dropped": rate(12345)},
	}
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{PSIValid: true, PSICPU: 12.5, PSIMemory: 3.2, PSIIO: 30.1, SwapOut: 2 << 20, NetworkHealth: health}}
	line := formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), 167)
	for _, expected := range []string{"12.5", "3.2", "30.1", "2.00M", "0.5", "12k", "10.0"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("widest row %q does not contain %q", line, expected)
		}
	}
	colored := formatTopDashboardRow(row, topViewAll, newTopLimits(8, true), 167)
	if !strings.Contains(colored, topPaint(topFitCell("30.1", 5, false), topLevelDanger, true)) || !strings.Contains(colored, topPaint(topFitCell("12.5", 5, false), topLevelWarn, true)) {
		t.Fatalf("psi must use the pressure thresholds: %q", colored)
	}
}

func TestTopNetworkRateCellKeepsSmallRatesVisible(t *testing.T) {
	value := func(value float64) *networkHealthRate {
		return &networkHealthRate{Rates: map[string]networkCounterRate{"listen_overflows": {Status: "observed", PerSecond: &value}}}
	}
	for _, test := range []struct {
		health *networkHealthRate
		width  int
		want   string
	}{
		{nil, 6, "—"},
		{&networkHealthRate{Rates: map[string]networkCounterRate{"listen_overflows": {Status: "unavailable"}}}, 6, "—"},
		{value(0.5), 4, "0.5"},
		{value(123.4), 6, "123.4"},
		{value(123.4), 4, "123"},
		{value(12345), 4, "12k"},
	} {
		if got := topNetworkRateCell(test.health, "listen_overflows", test.width); got != test.want {
			t.Fatalf("topNetworkRateCell(width %d) = %q, want %q", test.width, got, test.want)
		}
	}
}

func TestTopAllViewGivesTheSpareWidthToTheColumns(t *testing.T) {
	row := topDashboardRow{at: time.Unix(1, 0), rate: resourceRate{CoreCPU: []float64{10, 95}, DiskHealthValid: true, DiskIOPS: 12, NetHealthValid: true}}
	full := topAllColumnsUpTo(topAllMaxTier())
	wide := topAllLineWidth(full) + topSignalWideWidth
	for _, width := range []int{wide - 1, wide, wide + 1, wide + len(full)*3 + 5, wide + 200} {
		spare := max(0, width-wide)
		columns, signalWidth := topAllLayout(width)
		added, hotCoreExtra := 0, -1
		for index, column := range columns {
			extra := column.width - full[index].width
			if extra != column.indent || extra < spare/len(full) || extra > spare/len(full)+1 {
				t.Fatalf("width %d %s got %d more (indent %d), want %d or %d", width, column.title, extra, column.indent, spare/len(full), spare/len(full)+1)
			}
			added += extra
			if column.title == "hot core" {
				hotCoreExtra = extra
			}
		}
		if added != spare {
			t.Fatalf("width %d columns got %d more, want all %d spare", width, added, spare)
		}
		if want := min(topSignalWideWidth, width-topAllLineWidth(full)); signalWidth != want {
			t.Fatalf("width %d signal is %d wide, want %d", width, signalWidth, want)
		}
		headers := topDashboardHeaders(topViewAll, width)
		line := formatTopDashboardRow(row, topViewAll, newTopLimits(8, false), width)
		for _, text := range append(headers, line) {
			if got := len([]rune(text)); got != width {
				t.Fatalf("width %d line is %d wide: %q", width, got, text)
			}
		}
		if top, bottom, value := topDividerColumns(headers[0]), topDividerColumns(headers[1]), topDividerColumns(line); !reflect.DeepEqual(top, bottom) || !reflect.DeepEqual(bottom, value) {
			t.Fatalf("width %d dividers: group %v, column %v, row %v", width, top, bottom, value)
		}
		// 왼쪽 정렬인 hot core도 앞 칸과의 간격이 다른 칸처럼 넓어져야 i/o 값과 붙어 읽히지 않는다.
		if gap := strings.Repeat(" ", hotCoreExtra+1); !strings.Contains(headers[1], gap+"hot core") || !strings.Contains(line, gap+"1 95%") {
			t.Fatalf("width %d hot core is not indented by %d: %q / %q", width, hotCoreExtra, headers[1], line)
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

func TestTopProcessViewShowsTheMatchedGroupForEachSample(t *testing.T) {
	limits := newTopLimits(8, false)
	cells := func(row topDashboardRow) []string {
		line := formatTopDashboardRow(row, topViewProcess, limits, topTableWidth)
		if width := liveWidth(line); width > topTableWidth {
			t.Fatalf("row is %d columns: %q", width, line)
		}
		parts := strings.Split(line, "│")
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
		}
		return parts[1:]
	}
	row := topDashboardRow{at: time.Unix(1, 0), processesValid: true,
		processes: []topProcess{
			{PID: 1, CPU: 50, Command: "worker-a", FDs: 10, DiskValid: true, DiskRead: 1 << 20, DiskWrite: 2 << 20},
			{PID: 2, CPU: 40, Command: "worker-b", FDs: 5, DiskValid: true, DiskWrite: 1 << 20},
		},
		processTotal: topProcessTotal{Count: 5, CPU: 130, RSS: 9 << 20, Threads: 12, Probe: &topProbeStats{Source: topProbeSourceEBPF, Measured: 1, RunqCount: 4, RunqSumNS: 6_000_000, IO: &topProbeIO{Count: 2, SumNS: 500_000}}},
	}
	if got, want := cells(row), []string{"5", "130.0", "9.0M", "12", "15", "1.00M", "3.00M", "1.50", "0.25"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("process row = %q, want %q", got, want)
	}
	row.processTotal.Probe = nil
	row.processes = []topProcess{{PID: 3, CPU: 1, Command: "quiet"}}
	if got := cells(row); got[3] != "12" || got[4] != "—" || got[5] != "—" || got[7] != "—" || got[8] != "—" {
		t.Fatalf("unknown values must show —: %q", got)
	}
	if got := cells(topDashboardRow{at: time.Unix(1, 0), processesValid: true}); got[0] != "0" || got[1] != "—" {
		t.Fatalf("no match row = %q", got)
	}
	if got := cells(topDashboardRow{at: time.Unix(1, 0)}); got[0] != "—" {
		t.Fatalf("first sample row = %q", got)
	}
}

func TestTopProcessFilterOpensTheProcessView(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	model := topFixtureModel(nil)
	if model.withProcessFilter(topProcessFilter{}).view != topViewAll {
		t.Fatal("no filter must keep the all view")
	}
	filtered := model.withProcessFilter(filter)
	if filtered.view != topViewProcess || !strings.Contains(filtered.statusLines()[0], "f proc") {
		t.Fatalf("filtered view = %s, status %q", filtered.view, filtered.statusLines()[0])
	}
	host := topAfter(t, filtered, tea.KeyPressMsg{Code: '1', Text: "1"})
	if host.view != topViewAll {
		t.Fatalf("1 view = %s", host.view)
	}
	if back := topAfter(t, host, tea.KeyPressMsg{Code: 'f', Text: "f"}); back.view != topViewProcess {
		t.Fatalf("f view = %s", back.view)
	}
	if plain := topAfter(t, model, tea.KeyPressMsg{Code: 'f', Text: "f"}); plain.view != topViewAll || strings.Contains(plain.statusLines()[0], "f proc") {
		t.Fatalf("f without a filter: view %s, status %q", plain.view, plain.statusLines()[0])
	}
}

func TestTopProcessBannerLeadsWithTheMatchedGroup(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	plain := topFixtureModel(nil)
	if banner := plain.processBanner(); banner != nil {
		t.Fatalf("no filter banner = %q", banner)
	}
	colored := plain.withProcessFilter(filter).processBanner()[0]
	if !strings.HasPrefix(colored, topBannerStyle) || !strings.HasSuffix(colored, topColorReset) {
		t.Fatalf("colored banner = %q", colored)
	}
	model := plain.withProcessFilter(filter)
	model.limits.color = false
	if banner := model.processBanner(); len(banner) != 1 || !strings.HasPrefix(banner[0], " PROCESS worker · waiting for a sample") {
		t.Fatalf("first sample banner = %q", banner)
	}
	if strings.Contains(model.dashboardTitle(), "process worker") {
		t.Fatalf("the title must leave the filter to the banner: %q", model.dashboardTitle())
	}
	sameView := plain
	sameView.view = topViewProcess
	if model.bodyLines() != sameView.bodyLines()-1 {
		t.Fatalf("body lines = %d, without the banner %d", model.bodyLines(), sameView.bodyLines())
	}
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, filter: "worker",
		processes:    []topProcess{{PID: 1, CPU: 50, Command: "worker-a", FDs: 10, DiskValid: true, DiskWrite: 2 << 20}},
		processTotal: topProcessTotal{Count: 7, CPU: 600.4, RSS: 9 << 20, Threads: 12, Probe: &topProbeStats{Source: topProbeSourceEBPF, Measured: 1, RunqCount: 4, RunqSumNS: 6_000_000, IO: &topProbeIO{Count: 2, SumNS: 500_000}}},
	}}
	model.selected = 0
	banner := model.processBanner()[0]
	if !strings.HasPrefix(banner, " PROCESS worker · 7 matched · cpu 600% · rss 9.0M") || !strings.Contains(banner, model.rows[0].at.Format("15:04:05")) || liveWidth(banner) != topTableWidth {
		t.Fatalf("80 column banner = %q (%d columns)", banner, liveWidth(banner))
	}
	if strings.Contains(banner, "fds") {
		t.Fatalf("an 80 column banner drops the last items first: %q", banner)
	}
	model.width = 160
	if wide := model.processBanner()[0]; !strings.Contains(wide, "disk r 0.00M w 2.00M · thr 12 · fds 10") || liveWidth(wide) != 160 {
		t.Fatalf("160 column banner = %q", wide)
	}
	model.rows[0] = topDashboardRow{at: time.Unix(1, 0), processesValid: true, filter: "worker"}
	if banner := model.processBanner()[0]; !strings.HasPrefix(banner, " PROCESS worker · no match") {
		t.Fatalf("no match banner = %q", banner)
	}
}

func topKey(text string) tea.KeyPressMsg {
	switch text {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
}

func TestTopFollowSignalOpensTheSignalViewThenSelectsACandidate(t *testing.T) {
	var applied []topProcessFilter
	model := topFixtureModel(nil)
	model.limits.color = false
	model.setFilter = func(filter topProcessFilter) { applied = append(applied, filter) }
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true,
		rate:      resourceRate{DiskHealthValid: true, DiskAwait: 1000},
		processes: []topProcess{{PID: 4321, CPU: 30, Command: "postgres"}},
	}}
	model.selected = 0
	model = topAfter(t, model, topKey("f"))
	if model.view != topViewDisk || !strings.Contains(model.statusLines()[0], "await 1000ms → disk view") || len(applied) != 0 {
		t.Fatalf("first f: view %s, status %q, filters %v", model.view, model.statusLines()[0], applied)
	}
	seq := model.seq
	model = topAfter(t, model, topKey("f"))
	if !model.processFocus || model.processFilter.active() || model.seq != seq || len(applied) != 0 {
		t.Fatalf("second f must select a candidate without changing the filter: %+v", model)
	}
	model = topAfter(t, model, topKey("enter"))
	if model.view != topViewProcess || model.processFilter.String() != "4321" || model.seq != seq+1 || len(applied) != 1 || applied[0].String() != "4321" {
		t.Fatalf("second f: view %s, filter %q, seq %d, filters %v", model.view, model.processFilter, model.seq, applied)
	}
	if banner := model.processBanner()[0]; !strings.HasPrefix(banner, " PROCESS pid 4321 postgres · waiting for a sample") {
		t.Fatalf("rows from before the focus must not count: %q", banner)
	}
	if row := formatTopDashboardRow(model.currentFilterRow(model.rows[0]), topViewProcess, model.limits, topTableWidth); !strings.Contains(row, "│    —│") {
		t.Fatalf("process row from before the focus = %q", row)
	}
	model = topAfter(t, model, topSampleMsg{seq: model.seq, snapshot: resourceSnapshot{TakenAt: time.Unix(2, 0), CPUTotal: 200, ProcessesValid: true,
		Processes: []topProcess{{PID: 4321, CPU: 31, Command: "postgres"}}, ProcessTotal: topProcessTotal{Count: 1, CPU: 31}}})
	if last := model.rows[len(model.rows)-1]; last.filter != "4321" || !strings.Contains(model.processBanner()[0], "1 matched · cpu 31%") {
		t.Fatalf("row after the focus: filter %q, banner %q", last.filter, model.processBanner()[0])
	}
	model = topAfter(t, model, topKey("esc"))
	if model.processFilter.active() || model.view != topViewAll || len(applied) != 2 || applied[1].active() || model.processBanner() != nil {
		t.Fatalf("esc: filter %q, view %s, filters %v", model.processFilter, model.view, applied)
	}
}

func TestTopFollowSignalSelectsABusyProcessBeforeFocus(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{{PID: 77, CPU: 185, Command: "/usr/local/bin/node"}}}}
	model.selected = 0
	model = topAfter(t, model, topKey("f"))
	if !model.processFocus || model.processFilter.active() {
		t.Fatal("a process signal must open candidate selection")
	}
	model = topAfter(t, model, topKey("enter"))
	if model.view != topViewProcess || model.processFilter.String() != "77" || model.focusName != "node" {
		t.Fatalf("f on a process signal: view %s, filter %q, name %q", model.view, model.processFilter, model.focusName)
	}
}

func TestTopSlashEditsTheProcessFilter(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = false
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{{PID: 9, CPU: 5, Command: "nginx"}}}}
	model.selected = 0
	model = topAfter(t, model, topKey("/"))
	if !model.input || model.inputText != "nginx" || !strings.HasPrefix(model.statusLines()[0], "process filter: nginx") {
		t.Fatalf("/ opens the input with the busiest process: input %t, text %q", model.input, model.inputText)
	}
	model = topAfter(t, model, topKey("backspace"), topKey("x"), topKey(","), topKey("9"), topKey("enter"))
	if model.input || model.processFilter.String() != "nginx,9" || model.view != topViewProcess || model.focusName != "" {
		t.Fatalf("enter applies the filter: input %t, filter %q, view %s", model.input, model.processFilter, model.view)
	}
	model = topAfter(t, model, topKey("/"), topKey(","), topKey("enter"))
	if model.processFilter.String() != "nginx,9" || !strings.Contains(model.statusLines()[0], T("observe.top.process_invalid")) {
		t.Fatalf("an invalid filter keeps the old one: filter %q, status %q", model.processFilter, model.statusLines()[0])
	}
	model = topAfter(t, model, topKey("/"), topKey("z"), topKey("esc"))
	if model.input || model.processFilter.String() != "nginx,9" {
		t.Fatalf("esc closes the input without a change: input %t, filter %q", model.input, model.processFilter)
	}
	if got := topAfter(t, model, topKey("q")); got.input {
		t.Fatal("q must not open input")
	}
}

func TestTopMemoryCandidatesFocusTheSelectedPIDAndKeepTheSnapshot(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = false
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{
		{PID: 1, CPU: 185, RSS: 1 << 20, Command: "busy"},
		{PID: 2, CPU: 1, RSS: 4 << 30, Command: "memory-heavy"},
		{PID: 3, CPU: 2, RSS: 2 << 30, Command: "memory-second"},
	}}}
	model = topAfter(t, model, topKey("m"), topKey("tab"))
	if !model.processFocus || model.follow || model.candidates()[0].PID != 2 {
		t.Fatalf("memory candidates = %+v, focus %v, follow %v", model.candidates(), model.processFocus, model.follow)
	}
	model = topAfter(t, model, topSampleMsg{snapshot: resourceSnapshot{TakenAt: time.Unix(2, 0), CPUTotal: 200, ProcessesValid: true, Processes: []topProcess{{PID: 99, Command: "new"}}}})
	if model.selected != 0 || model.candidates()[0].PID != 2 {
		t.Fatal("new samples must not move the selected candidate snapshot")
	}
	model = topAfter(t, model, topKey("down"), topKey("enter"))
	if model.processFilter.String() != "3" || model.processFocus || !model.follow || model.focusName != "memory-sec" {
		t.Fatalf("selected PID focus = %+v", model)
	}
	model = topAfter(t, model, topKey("esc"))
	if model.currentFilterRow(topDashboardRow{filter: "3", processesValid: true}).processesValid {
		t.Fatal("clearing the filter must not treat old filtered rows as host process samples")
	}
}

func TestTopHelpScrollsWithoutChangingTheFilter(t *testing.T) {
	filter, _ := parseTopProcessFilter("worker")
	model := topFixtureModel(nil).withProcessFilter(filter)
	model.width, model.height = 48, 10
	model = topAfter(t, model, topKey("?"))
	first := model.View().Content
	if !model.help || !strings.Contains(first, "Help") || len(strings.Split(first, "\n")) > model.height {
		t.Fatalf("help view = %q", first)
	}
	model = topAfter(t, model, topKey("down"))
	if model.helpOffset != 1 || model.View().Content == first {
		t.Fatal("help must scroll on a small terminal")
	}
	model = topAfter(t, model, topKey("esc"))
	if model.help || model.processFilter.String() != "worker" {
		t.Fatal("Esc in help must preserve the process filter")
	}
}

func TestTopDashboardFitsNarrowTerminalsAndHidesDarwinUnsupportedColumns(t *testing.T) {
	filter, _ := parseTopProcessFilter("worker")
	for _, width := range []int{24, 32, 40, 48, 60, 80, 120, 160, 170} {
		for _, view := range []topView{topViewAll, topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewProcess} {
			model := topFixtureModel(nil).withProcessFilter(filter)
			model.details.System = "darwin"
			model.view, model.width = view, width
			model.limits.color = true
			model.rows = []topDashboardRow{{at: time.Unix(1, 0), filter: "worker", processesValid: true, processes: []topProcess{{PID: 1, Command: "작업🙂", CPU: 99, RSS: 1 << 20}}, processTotal: topProcessTotal{Count: 1, CPU: 99}, rate: resourceRate{CPUUser: 99, MemoryPercent: 97, DiskHealthValid: true}}}
			model.selected = 0
			for _, line := range strings.Split(model.View().Content, "\n") {
				if got := ansi.StringWidth(line); got > width {
					t.Fatalf("%s at %d columns: %d wide: %q", view, width, got, line)
				}
			}
			for _, header := range model.tableHeader() {
				for _, unsupported := range []string{"fds", "runq ms", "io ms", "psi", "busy%", "i/o", "listen", "soft", "ct%"} {
					if strings.Contains(header, unsupported) {
						t.Fatalf("Darwin header contains %s: %q", unsupported, header)
					}
				}
			}
			headers := model.tableHeader()
			if got, want := topDividerColumns(ansi.Strip(model.tableRow(model.rows[0]))), topDividerColumns(headers[len(headers)-1]); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s at %d columns: row dividers %v, header %v", view, width, got, want)
			}
		}
	}
}

func TestTopDashboardShowsFailedSampleTimeAndIOStatus(t *testing.T) {
	filter, _ := parseTopProcessFilter("worker")
	model := topFixtureModel(nil).withProcessFilter(filter)
	model.lastErr = errors.New("host read failed")
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), filter: "worker", processesValid: true, processes: []topProcess{{PID: 1, Command: "worker", DiskStatus: "proc_pid_rusage: operation not permitted"}}, processTotal: topProcessTotal{Count: 1}}}
	model.follow = false
	for _, want := range []string{"last success " + model.previous.TakenAt.Format("15:04:05"), "host read failed", "history " + model.rows[0].at.Format("15:04:05"), "operation not permitted"} {
		if content := model.View().Content; !strings.Contains(content, want) {
			t.Fatalf("view does not show %q: %q", want, content)
		}
	}
}

func TestTopCandidateSelectionStaysVisibleOnShortTerminals(t *testing.T) {
	for _, height := range []int{8, 12, 18, 24} {
		model := topFixtureModel(nil)
		model.width, model.height = 48, height
		model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{
			{PID: 1, Command: "one"}, {PID: 2, Command: "two"}, {PID: 3, Command: "three"}, {PID: 4, Command: "four"}, {PID: 5, Command: "five"},
		}}}
		model = topAfter(t, model, topKey("tab"), topKey("down"), topKey("down"), topKey("down"), topKey("down"))
		content := model.View().Content
		if len(strings.Split(content, "\n")) > height || !strings.Contains(content, ">       5") || !strings.Contains(content, "Enter focus") {
			t.Fatalf("%d rows: selected candidate or controls lost: %q", height, content)
		}
	}
}

func TestTopProcessLatencyDistinguishesNoEventsFromUnavailable(t *testing.T) {
	row := topDashboardRow{processesValid: true, processTotal: topProcessTotal{Count: 1}}
	if cells := topProcessViewCells(row); cells[7].text != "—" || cells[8].text != "—" {
		t.Fatalf("latency without an observer = %+v", cells)
	}
	row.processTotal.Probe = &topProbeStats{Source: topProbeSourceEBPF, Measured: 1, IO: &topProbeIO{}}
	if cells := topProcessViewCells(row); cells[7].text != "no ev" || cells[8].text != "no ev" {
		t.Fatalf("active observer without events = %+v", cells)
	}
}

func TestTopProcessBannerFitsWideCharacterFilters(t *testing.T) {
	filter, _ := parseTopProcessFilter("작업🙂")
	model := topFixtureModel(nil).withProcessFilter(filter)
	model.limits.color = false
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), filter: filter.String(), processesValid: true, processTotal: topProcessTotal{Count: 1, CPU: 10, RSS: 1 << 20}}}
	for _, width := range []int{24, 40, 80} {
		model.width = width
		if banner := model.processBanner()[0]; ansi.StringWidth(banner) != width {
			t.Fatalf("banner at %d columns is %d wide: %q", width, ansi.StringWidth(banner), banner)
		}
	}
}

func TestTopSelectedRowHighlightsTheActivePane(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, rate: resourceRate{MemoryPercent: 99}, processes: []topProcess{
		{PID: 1, Command: "first"}, {PID: 2, Command: "second"},
	}}}
	model.follow = false
	if content := model.View().Content; !strings.Contains(content, topBannerStyle+model.rows[0].at.Format("15:04:05")+">") || !strings.Contains(content, topColorDanger) {
		t.Fatalf("history selection must keep warning color and highlight its row: %q", content)
	}
	model = topAfter(t, model, topKey("tab"), topKey("down"))
	selected := 0
	for _, line := range strings.Split(model.View().Content, "\n") {
		if strings.Contains(line, topBannerStyle) {
			selected++
			if !strings.HasPrefix(line, topBannerStyle+">       2") || ansi.StringWidth(line) != model.width || !strings.HasSuffix(line, topColorReset) {
				t.Fatalf("selected process must highlight the full row: %q", line)
			}
		}
	}
	if selected != 1 {
		t.Fatalf("active pane must have one highlighted row, got %d", selected)
	}
	model.limits.color = false
	if content := model.View().Content; strings.Contains(content, topBannerStyle) || !strings.Contains(content, ">       2") {
		t.Fatalf("plain selection must keep its marker: %q", content)
	}
}

func TestTopCandidatePagingKeepsTheSelectedSnapshot(t *testing.T) {
	model := topFixtureModel(nil)
	model.rows = []topDashboardRow{
		{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{{PID: 1}, {PID: 2}, {PID: 3}, {PID: 4}, {PID: 5}}},
		{at: time.Unix(2, 0), processesValid: true, processes: []topProcess{{PID: 99}}},
	}
	model = topAfter(t, model, topKey("tab"), tea.KeyPressMsg{Code: tea.KeyPgDown})
	if model.selected != 0 || model.processSelected != 4 {
		t.Fatalf("candidate page-down moved history: time row %d, process row %d", model.selected, model.processSelected)
	}
	model = topAfter(t, model, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if model.selected != 0 || model.processSelected != 0 {
		t.Fatalf("candidate page-up moved history: time row %d, process row %d", model.selected, model.processSelected)
	}
}

func TestTopCandidatePanelShowsMoreProcessesOnTallTerminals(t *testing.T) {
	processes := []topProcess{}
	for pid := 1; pid <= 6; pid++ {
		processes = append(processes, topProcess{PID: pid, CPU: float64(10 - pid), Command: "proc" + string(rune('0'+pid))})
	}
	for height, want := range map[int]int{topTallHeight - 1: 3, topTallHeight: topProcessLimit} {
		model := topFixtureModel(nil)
		model.width, model.height = 120, height
		for second := 0; second < 60; second++ {
			model.rows = append(model.rows, topDashboardRow{at: time.Unix(int64(second), 0), processesValid: true, processes: processes})
		}
		model.selected = len(model.rows) - 1
		content, got := model.View().Content, 0
		for _, process := range processes {
			if strings.Contains(content, process.Command) {
				got++
			}
		}
		if got != want {
			t.Fatalf("height %d shows %d processes, want %d: %q", height, got, want, content)
		}
		if lines := len(strings.Split(content, "\n")); lines > height {
			t.Fatalf("height %d renders %d lines", height, lines)
		}
		if height == topTallHeight && model.bodyLines() != 28 {
			t.Fatalf("height %d keeps %d history rows, want 28", height, model.bodyLines())
		}
	}
}

// Linux의 pid_max는 기본 4194304라 PID가 7자리일 수 있다. PID 길이가 달라도 후보 목록의 열은 같은 자리에서 시작해야 한다.
func TestTopCandidateColumnsAlignForSevenDigitPIDs(t *testing.T) {
	for _, width := range []int{100, 36} {
		model := topFixtureModel(nil)
		model.limits.color = false
		model.width = width
		model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: []topProcess{
			{PID: 245099, CPU: 100, RSS: 10 << 20, Command: "edcbusy"},
			{PID: 3688912, CPU: 7, RSS: 430 << 20, Command: "claude"},
		}}}
		model.selected = 0
		lines := model.candidateLines()
		var columns []int
		// 첫 줄은 창 제목이고 화면에 그릴 때 폭에 맞춰 잘린다. 머리글과 process 행은 그대로 폭 안에 들어가야 한다.
		for _, line := range lines[1:] {
			if lineWidth := liveWidth(line); lineWidth > width {
				t.Fatalf("width %d: line is %d columns: %q", width, lineWidth, line)
			}
			for _, name := range []string{"COMMAND", "edcbusy", "claude"} {
				if index := strings.Index(line, name); index >= 0 {
					columns = append(columns, index)
				}
			}
		}
		if len(columns) != 3 || columns[0] != columns[1] || columns[1] != columns[2] {
			t.Fatalf("width %d: command columns %v in %q", width, columns, lines)
		}
	}
}

func topFooterModelAt(width, height, rows int) topModel {
	model := topFixtureModel(nil)
	model.width, model.height = width, height
	processes := []topProcess{{PID: 11, CPU: 90, RSS: 1 << 20, Command: "alpha"}, {PID: 12, CPU: 80, RSS: 1 << 20, Command: "beta"}, {PID: 13, CPU: 70, RSS: 1 << 20, Command: "gamma"}, {PID: 14, CPU: 60, RSS: 1 << 20, Command: "delta"}, {PID: 15, CPU: 50, RSS: 1 << 20, Command: "eps"}}
	for index := 0; index < rows; index++ {
		model.rows = append(model.rows, topDashboardRow{at: time.Date(2026, 1, 1, 9, 0, index, 0, time.UTC), processes: processes, processesValid: true, rate: resourceRate{Load1: 1, MemoryPercent: 30}})
	}
	model.selected = max(0, rows-1)
	return model
}

func TestTopStatusLinesMergeWhenTheyFit(t *testing.T) {
	wide := topFooterModelAt(200, 40, 3).statusLines()
	if len(wide) != 1 || !strings.Contains(wide[0], "interval") || !strings.Contains(wide[0], "q quit") || strings.Count(wide[0], "? help") != 1 {
		t.Errorf("200 columns: status = %q", wide)
	}
	if narrow := topFooterModelAt(120, 40, 3).statusLines(); len(narrow) != 2 {
		t.Errorf("120 columns: status = %q", narrow)
	}
}

func TestTopFooterDocksProcessesOnTheRight(t *testing.T) {
	model := topFooterModelAt(200, 40, 3)
	left := 200 - topFooterGap - topFooterProcessWidth
	footer := model.footerLines()
	// process 후보는 제목, 열 이름, 다섯 개로 7줄이고 안내 한 줄은 그 왼쪽 마지막 줄에 들어간다.
	if len(footer) != 7 {
		t.Fatalf("footer = %d lines:\n%s", len(footer), strings.Join(footer, "\n"))
	}
	// 왼쪽 칸에는 이벤트가 들어가고 ·가 여러 바이트라, 오른쪽 칸은 바이트가 아니라 셀 위치로 자른다.
	if !strings.HasPrefix(ansi.Cut(footer[0], left+topFooterGap, 200), "processes · CPU rank") || !strings.HasPrefix(footer[0], "events · e list") {
		t.Errorf("first footer line = %q", footer[0])
	}
	if last := ansi.Strip(footer[len(footer)-1]); !strings.HasPrefix(last, "interval") || !strings.Contains(last, "eps") {
		t.Errorf("last footer line = %q", last)
	}
	for _, line := range footer {
		if ansi.StringWidth(line) != 200 {
			t.Errorf("footer line is %d columns: %q", ansi.StringWidth(line), line)
		}
	}
	detail := topAfter(t, model, topKey("enter"))
	if got := len(detail.footerLines()); got != 7 {
		t.Errorf("detail footer = %d lines, want 7 beside the processes", got)
	}
	if lines := strings.Split(detail.View().Content, "\n"); !strings.HasPrefix(lines[len(lines)-7], "detail ") {
		t.Errorf("detail panel is not at the top left of the footer:\n%s", detail.View().Content)
	}
	filter, err := parseTopProcessFilter("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stacked := model.withProcessFilter(filter); len(stacked.footerLines()) != len(stacked.panelLines())+len(stacked.statusLines()) {
		t.Error("a process filter must keep the stacked footer")
	}
	if narrow := topFooterModelAt(144, 40, 3); len(narrow.footerLines()) != len(narrow.panelLines())+len(narrow.statusLines()) {
		t.Error("144 columns must keep the stacked footer")
	}
}

func TestTopFooterStaysAtTheBottom(t *testing.T) {
	for _, size := range [][2]int{{80, 30}, {200, 40}} {
		model := topFooterModelAt(size[0], size[1], 2)
		lines := strings.Split(model.View().Content, "\n")
		if len(lines) != size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		if last := ansi.Strip(lines[len(lines)-1]); !strings.Contains(last, "q quit") {
			t.Errorf("%v: last line = %q", size, last)
		}
		// 시각 두 행 바로 다음 줄은 빈 줄이고, 아래 영역은 화면 끝에 붙는다.
		for index, line := range lines {
			if strings.HasPrefix(line, "09:00:01") {
				if strings.TrimSpace(lines[index+1]) != "" {
					t.Errorf("%v: line after the rows = %q", size, lines[index+1])
				}
			}
		}
	}
	split := topSplitModelAt(200, 60, 3, false, nil)
	lines := strings.Split(split.View().Content, "\n")
	if len(lines) != 60 || !strings.Contains(ansi.Strip(lines[59]), "q quit") {
		t.Errorf("split: %d lines, last = %q", len(lines), lines[len(lines)-1])
	}
}

// macOS는 PSI 대신 kernel의 memory 압박 단계를 보이고, Linux는 그 열을 숨긴다.
func TestTopMemoryPressureColumnFollowsTheHost(t *testing.T) {
	model := topFixtureModel(nil)
	model.width = 200
	darwin := model
	darwin.details.System = "darwin"
	for _, view := range []topView{topViewMemory, topViewPressure} {
		model.view, darwin.view = view, view
		if got := model.tableHeader()[0]; strings.Contains(got, "mem lvl") {
			t.Errorf("linux %s shows mem lvl: %q", view, got)
		}
		got := darwin.tableHeader()[0]
		if !strings.Contains(got, "mem lvl") || strings.Contains(got, "psi") || strings.Contains(got, "full") {
			t.Errorf("darwin %s header = %q", view, got)
		}
	}
	darwin.view = topViewPressure
	rate := resourceRate{MemoryPressure: topMemoryPressureCritical, ProcsBlocked: 2, ProcsBlockedSource: topBlockedProcessList}
	if line := darwin.tableRow(topDashboardRow{at: time.Unix(1, 0), rate: rate}); !strings.Contains(line, "critical") || !strings.Contains(line, "│      2│") {
		t.Errorf("darwin pressure row = %q", line)
	}
	if got := topAfter(t, darwin, topKey("s")); got.view != topViewPressure {
		t.Errorf("darwin s: view = %s, notice = %q", got.view, got.notice)
	}
}

func TestTopSignalsUseMemoryPressureAndBlockedWithoutIOWait(t *testing.T) {
	limits := newTopLimits(4, false)
	for level, want := range map[topMemoryPressure]string{topMemoryPressureNormal: "", topMemoryPressureWarn: "mem pressure warn", 3: "mem pressure warn", topMemoryPressureCritical: "mem pressure critical"} {
		got := ""
		for _, item := range topSignals(resourceRate{MemoryPressure: level}, limits) {
			if item.kind == "mem pressure" {
				got = item.text
				if item.level() != level.level() {
					t.Errorf("level %d: signal level %v", level, item.level())
				}
			}
		}
		if got != want {
			t.Errorf("level %d: signal %q, want %q", level, got, want)
		}
	}
	if items := topSignals(resourceRate{MemoryPressure: topMemoryPressureUnknown}, limits); len(items) != 0 {
		t.Errorf("an unread level must not warn: %+v", items)
	}
	blocked := func(rate resourceRate) bool {
		for _, item := range topSignals(rate, limits) {
			if item.kind == "blocked" {
				return true
			}
		}
		return false
	}
	// process 목록에서 센 blocked(macOS)는 iowait 대신 디스크 await 경고나 memory 압박이 두 번째 근거다. U 상태는 page-in
	// 대기에서도 생기므로 개수만으로는 경고하지 않는다. kernel 값(Linux)은 iowait도 높아야 한다. zero value는 kernel 값이라
	// 표시를 빠뜨려도 경고가 느슨해지지 않는다.
	fromProcesses := resourceRate{ProcsBlocked: topBlockedSignalMin, ProcsBlockedSource: topBlockedProcessList}
	if blocked(fromProcesses) {
		t.Error("blocked processes from the process list must not warn alone")
	}
	slowDisk := fromProcesses
	slowDisk.DiskHealthValid, slowDisk.DiskAwait = true, limits.await.warn
	if !blocked(slowDisk) {
		t.Error("blocked processes with a slow disk must warn")
	}
	unreadDisk := slowDisk
	unreadDisk.DiskHealthValid = false
	if blocked(unreadDisk) {
		t.Error("an unread disk await must not count as evidence")
	}
	pressured := fromProcesses
	pressured.MemoryPressure = topMemoryPressureWarn
	if !blocked(pressured) {
		t.Error("blocked processes under memory pressure must warn")
	}
	if blocked(resourceRate{ProcsBlocked: 14, ProcsBlockedSource: topBlockedKernelTasks}) {
		t.Error("kernel blocked tasks without iowait must not warn")
	}
}
