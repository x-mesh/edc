package edc

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// runTopDashboard는 alt screen 대시보드를 실행한다. 종료하면 화면이 원래대로 돌아온다.
func runTopDashboard(interval time.Duration) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.host", err))
		return 1
	}
	first, err := sampleTopDashboard()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.resource", err))
		return 1
	}
	if _, err := tea.NewProgram(newTopModel(details, first, interval, sampleTopDashboard), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// sampleTopDashboard는 snapshot에 process 목록을 붙인다. 표와 JSON 출력은 process를 쓰지 않으므로
// collectResourceSnapshot이 아니라 대시보드에서만 process를 수집한다.
func sampleTopDashboard() (resourceSnapshot, error) {
	snapshot, err := collectResourceSnapshot()
	snapshot.Processes, snapshot.ProcessesValid = processSampler.latest()
	return snapshot, err
}

type topView string

const (
	topViewAll      topView = "all"
	topViewCPU      topView = "cpu"
	topViewMemory   topView = "memory"
	topViewDisk     topView = "disk"
	topViewNetwork  topView = "network"
	topViewPressure topView = "pressure"
)

const (
	topWideTableWidth = 132
	// topPeakWindow는 h 패널이 지표별 최고치를 찾는 구간이다.
	topPeakWindow = time.Minute
	// topSelectionColumn은 행에서 "15:04:05" 바로 뒤 공백 자리다. 선택 표시가 시각을 가리지 않는다.
	topSelectionColumn = 8
	// topProcessNameWidth는 상세 패널의 process 이름 폭이다. 세 개가 80열 한 줄에 들어간다.
	topProcessNameWidth = 10
	// topSignalProcessNameWidth는 signal 열의 process 이름 폭이다.
	topSignalProcessNameWidth = 4
	// topProcessSignalCPU는 process 하나가 signal에 오르는 CPU%다. core 하나가 100%다.
	topProcessSignalCPU = 80
	// topCoreBarLimit는 CPU 보기 막대에 그리는 최대 core 수다.
	topCoreBarLimit = 24
)

// topDashboardRow는 포맷 문자열 대신 측정값을 보존한다. 같은 시점을 다른 렌즈로
// 다시 그릴 수 있고, 선택한 과거 행의 상세도 최신 값과 섞이지 않는다.
type topDashboardRow struct {
	at             time.Time
	rate           resourceRate
	processes      []topProcess
	processesValid bool
}

type topModel struct {
	details       hostDetails
	limits        topLimits
	interval      time.Duration
	paused        bool
	previous      resourceSnapshot
	rows          []topDashboardRow
	view          topView
	follow        bool
	baseline      bool
	selected      int
	detail        bool
	peaks         bool
	width, height int
	sample        func() (resourceSnapshot, error)
	seq           int
	lastErr       error
}

// topSampleMsg는 tick마다 수집한 snapshot이다. seq가 다르면 interval이 바뀐 뒤의 낡은 tick이다.
type topSampleMsg struct {
	seq      int
	snapshot resourceSnapshot
	err      error
}

func newTopModel(details hostDetails, first resourceSnapshot, interval time.Duration, sample func() (resourceSnapshot, error)) topModel {
	return topModel{details: details, limits: newTopLimits(details.Cores, true), interval: interval, previous: first, view: topViewAll, follow: true, sample: sample}
}

func (model topModel) Init() tea.Cmd { return model.tick() }

func (model topModel) tick() tea.Cmd {
	seq, sample := model.seq, model.sample
	return tea.Tick(model.interval, func(time.Time) tea.Msg {
		snapshot, err := sample()
		return topSampleMsg{seq: seq, snapshot: snapshot, err: err}
	})
}

func (model topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case topSampleMsg:
		if value.seq != model.seq {
			return model, nil
		}
		if value.err != nil {
			// 일시적인 수집 실패는 화면을 닫지 않는다. 마지막 유효 row를 유지하고 다음 tick에서 복구한다.
			model.lastErr = value.err
			if model.paused {
				return model, nil
			}
			return model, model.tick()
		}
		model.lastErr = nil
		if model.baseline {
			// 재개 직후에는 새 기준점만 세운다. 멈춘 시간 전체를 한 행의 평균 rate로 보이지 않는다.
			model.previous, model.baseline = value.snapshot, false
			return model, model.tick()
		}
		before := len(model.rows) + 1
		model.rows = appendTopDashboardRow(model.rows, topDashboardRow{at: value.snapshot.TakenAt, rate: calculateRate(model.previous, value.snapshot), processes: value.snapshot.Processes, processesValid: value.snapshot.ProcessesValid})
		model.previous = value.snapshot
		if model.follow {
			model.selected = len(model.rows) - 1
		} else {
			// history 한도에서 앞 row가 잘리면 번호를 같이 당겨야 고른 시점이 그대로 남는다.
			model.selected = max(0, model.selected-(before-len(model.rows)))
		}
		if model.paused {
			return model, nil
		}
		return model, model.tick()
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(value)
	}
	return model, nil
}

func (model topModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "p":
		model.paused, model.seq = !model.paused, model.seq+1
		if model.paused {
			return model, nil
		}
		model.baseline = true
		return model, model.tick()
	case "+", "=":
		return model.withInterval(nextTopInterval(model.interval, 1))
	case "-", "_":
		return model.withInterval(nextTopInterval(model.interval, -1))
	case "1":
		model.view = topViewAll
	case "c":
		model.view = topViewCPU
	case "m":
		model.view = topViewMemory
	case "d":
		model.view = topViewDisk
	case "n":
		model.view = topViewNetwork
	case "s":
		model.view = topViewPressure
	case "enter":
		model.detail = !model.detail
		if model.detail {
			model.peaks = false
		}
	case "h":
		model.peaks = !model.peaks
		if model.peaks {
			model.detail = false
		}
	case "up", "k":
		if model.selected > 0 {
			model.selected--
			model.follow = false
		}
	case "down", "j":
		if model.selected < len(model.rows)-1 {
			model.selected++
			model.follow = model.selected == len(model.rows)-1
		}
	case "end", "g":
		if len(model.rows) > 0 {
			model.selected, model.follow = len(model.rows)-1, true
		}
	}
	return model, nil
}

func (model topModel) withInterval(interval time.Duration) (tea.Model, tea.Cmd) {
	if interval == model.interval {
		return model, nil
	}
	// 낡은 tick이 새 간격을 덮어쓰지 않게 순번을 올린다.
	model.interval, model.seq = interval, model.seq+1
	if model.paused {
		return model, nil
	}
	return model, model.tick()
}

// nextTopInterval은 ladder에서 한 칸 옮긴다. CLI로 받은 값이 ladder에 없으면 가장 가까운 자리에 끼워 넣는다.
func nextTopInterval(current time.Duration, step int) time.Duration {
	ladder := append([]time.Duration{}, topIntervalLadder...)
	index := sort.Search(len(ladder), func(i int) bool { return ladder[i] >= current })
	if index == len(ladder) || ladder[index] != current {
		ladder = append(ladder, current)
		sort.Slice(ladder, func(i, j int) bool { return ladder[i] < ladder[j] })
		index = sort.Search(len(ladder), func(i int) bool { return ladder[i] >= current })
	}
	return ladder[min(max(index+step, 0), len(ladder)-1)]
}

func appendTopDashboardRow(rows []topDashboardRow, row topDashboardRow) []topDashboardRow {
	rows = append(rows, row)
	if len(rows) > topDashboardHistory {
		rows = rows[len(rows)-topDashboardHistory:]
	}
	return rows
}

func (model topModel) selectedRow() (topDashboardRow, bool) {
	if model.selected < 0 || model.selected >= len(model.rows) {
		return topDashboardRow{}, false
	}
	return model.rows[model.selected], true
}

func (model topModel) View() tea.View {
	wide := model.view == topViewAll && model.width >= topWideTableWidth
	lines := append([]string{model.dashboardTitle()}, topDashboardHeaders(model.view, wide)...)
	panel, status := model.panelLines(), model.statusLines()
	bodyLines := max(1, model.height-len(lines)-len(panel)-len(status))
	start := max(0, len(model.rows)-bodyLines)
	if !model.follow && len(model.rows) > bodyLines {
		start = min(model.selected, len(model.rows)-bodyLines)
	}
	for index := start; index < len(model.rows) && index < start+bodyLines; index++ {
		line := formatTopDashboardRow(model.rows[index], model.view, model.limits, wide)
		if index == model.selected && !model.follow {
			line = line[:topSelectionColumn] + ">" + line[topSelectionColumn+1:]
		}
		lines = append(lines, line)
	}
	lines = append(lines, panel...)
	lines = append(lines, status...)
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// panelLines는 detail이나 peaks 패널이다. renderer는 넘치는 줄을 접지 않고 자르므로
// 패널을 여러 줄로 나눠 80열에서도 끝까지 보이게 한다.
func (model topModel) panelLines() []string {
	var lines []string
	switch {
	case model.detail:
		lines = model.detailLines()
	case model.peaks:
		lines = model.peakLines()
	}
	for index, line := range lines {
		lines[index] = topDashboardFitWidth(line, max(topTableWidth, model.width))
	}
	return lines
}

func (model topModel) dashboardTitle() string {
	state := "latest"
	if !model.follow {
		state = "history"
	}
	if model.lastErr != nil {
		state += " · sample error"
	}
	return fmt.Sprintf("🐰 %s · %d cores · %s · %s 🐰", model.details.Hostname, model.details.Cores, model.view, state)
}

func (model topModel) detailLines() []string {
	row, ok := model.selectedRow()
	if !ok {
		return []string{"detail · waiting for a sample"}
	}
	rate := row.rate
	return []string{
		fmt.Sprintf("detail %s · load %.1f · cpu %.1f/%.1f%% · iowait %.1f%% · mem %.1f%%", row.at.Format("15:04:05"), rate.Load1, rate.CPUUser, rate.CPUSystem, rate.CPUIOWait, rate.MemoryPercent),
		fmt.Sprintf("  %s · %s · %s", topDiskDetail(rate), topNetworkDetail(rate), topPressureDetail(rate)),
		"  " + topProcessDetail(row.processes, row.processesValid),
	}
}

// peakLines는 지표마다 따로 최고치를 찾는다. 단위가 다른 값을 더해 한 행을 고르면 load 급등이 memory에 가려진다.
func (model topModel) peakLines() []string {
	if len(model.rows) == 0 {
		return []string{"peaks 60s · waiting for a sample"}
	}
	last := model.rows[len(model.rows)-1]
	load, cpu, iowait, memory := last, last, last, last
	for _, row := range model.rows {
		if last.at.Sub(row.at) > topPeakWindow {
			continue
		}
		if row.rate.Load1 > load.rate.Load1 {
			load = row
		}
		if row.rate.CPUUser+row.rate.CPUSystem > cpu.rate.CPUUser+cpu.rate.CPUSystem {
			cpu = row
		}
		if row.rate.CPUIOWait > iowait.rate.CPUIOWait {
			iowait = row
		}
		if row.rate.MemoryPercent > memory.rate.MemoryPercent {
			memory = row
		}
	}
	return []string{
		fmt.Sprintf("peaks 60s · load %.1f at %s · cpu %.1f%% at %s", load.rate.Load1, load.at.Format("15:04:05"), cpu.rate.CPUUser+cpu.rate.CPUSystem, cpu.at.Format("15:04:05")),
		fmt.Sprintf("          · iowait %.1f%% at %s · mem %.1f%% at %s", iowait.rate.CPUIOWait, iowait.at.Format("15:04:05"), memory.rate.MemoryPercent, memory.at.Format("15:04:05")),
	}
}

func (model topModel) statusLines() []string {
	state := "interval " + model.interval.String()
	if model.paused {
		state = T("observe.top.paused") + " · " + state
	} else if !model.follow {
		state = fmt.Sprintf("history · %d new · %s", max(0, len(model.rows)-1-model.selected), state)
	}
	views := fmt.Sprintf("1 all c cpu m mem d disk n net s pressure · %s", state)
	actions := "keys  ↑↓ history  End latest  Enter detail  h peaks  ·  q quit  p pause  +/-"
	return []string{topDashboardFit(views), topDashboardFit(actions)}
}

// topColumn은 보기별 표의 한 칸이다. 헤더와 행이 같은 정의로 그려져 구분선이 어긋나지 않는다.
// width가 0인 마지막 칸은 남은 폭을 모두 쓴다.
type topColumn struct {
	title string
	width int
	left  bool
}

func topViewColumns(view topView) []topColumn {
	signal := topColumn{title: "signal", left: true}
	switch view {
	case topViewCPU:
		return []topColumn{{title: "load", width: 5}, {title: "usr%", width: 5}, {title: "sys%", width: 5}, {title: "io%", width: 5}, {title: "hot core", width: 8, left: true}, {title: "cores", width: topCoreBarLimit, left: true}, signal}
	case topViewMemory:
		return []topColumn{{title: "mem%", width: 6}, {title: "psi mem", width: 7}, {title: "load", width: 6}, signal}
	case topViewDisk:
		return []topColumn{{title: "read/s", width: 7}, {title: "write/s", width: 7}, {title: "iops", width: 6}, {title: "await", width: 6}, {title: "busy%", width: 6}, signal}
	case topViewNetwork:
		return []topColumn{{title: "in/s", width: 7}, {title: "out/s", width: 7}, {title: "pk_in", width: 6}, {title: "pk_out", width: 6}, {title: "err/s", width: 6}, {title: "drop/s", width: 6}, signal}
	case topViewPressure:
		return []topColumn{{title: "cpu psi", width: 7}, {title: "mem psi", width: 7}, {title: "io psi", width: 7}, {title: "load", width: 6}, {title: "mem%", width: 6}, signal}
	}
	return nil
}

func topViewCells(rate resourceRate, view topView, signal string) []string {
	switch view {
	case topViewCPU:
		return []string{fmt.Sprintf("%.1f", rate.Load1), fmt.Sprintf("%.1f", rate.CPUUser), fmt.Sprintf("%.1f", rate.CPUSystem), fmt.Sprintf("%.1f", rate.CPUIOWait), topHotCore(rate.CoreCPU), topCoreBar(rate.CoreCPU), signal}
	case topViewMemory:
		return []string{fmt.Sprintf("%.1f", rate.MemoryPercent), topOptionalValue(rate.PSIValid, "%.1f", rate.PSIMemory), fmt.Sprintf("%.1f", rate.Load1), signal}
	case topViewDisk:
		return []string{formatRate(rate.DiskRead), formatRate(rate.DiskWrite), topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskIOPS), topOptionalValue(rate.DiskHealthValid, "%.1f", rate.DiskAwait), topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskBusy), signal}
	case topViewNetwork:
		return []string{formatRate(rate.NetIn), formatRate(rate.NetOut), fmt.Sprintf("%.0f", rate.PacketsIn), fmt.Sprintf("%.0f", rate.PacketsOut), topOptionalValue(rate.NetHealthValid, "%.0f", rate.NetErrors), topOptionalValue(rate.NetHealthValid, "%.0f", rate.NetDrops), signal}
	case topViewPressure:
		return []string{topOptionalValue(rate.PSIValid, "%.1f", rate.PSICPU), topOptionalValue(rate.PSIValid, "%.1f", rate.PSIMemory), topOptionalValue(rate.PSIValid, "%.1f", rate.PSIIO), fmt.Sprintf("%.1f", rate.Load1), fmt.Sprintf("%.1f", rate.MemoryPercent), signal}
	}
	return nil
}

// topOptionalValue는 platform이 주지 않는 값을 0 대신 —로 보여 준다.
func topOptionalValue(valid bool, format string, value float64) string {
	if !valid {
		return "—"
	}
	return fmt.Sprintf(format, value)
}

func formatTopColumns(at string, columns []topColumn, cells []string) string {
	var line strings.Builder
	fmt.Fprintf(&line, "%8s │", at)
	for index, column := range columns {
		if index > 0 {
			line.WriteString("│")
		}
		if column.left {
			fmt.Fprintf(&line, "%-*s", column.width, cells[index])
		} else {
			fmt.Fprintf(&line, "%*s", column.width, cells[index])
		}
	}
	return topDashboardFit(line.String())
}

func topDashboardHeaders(view topView, wide bool) []string {
	if view != topViewAll {
		columns := topViewColumns(view)
		titles := make([]string, len(columns))
		for index, column := range columns {
			titles[index] = column.title
		}
		return []string{formatTopColumns("time", columns, titles)}
	}
	if wide {
		return []string{
			topDashboardFitWidth(fmt.Sprintf("%8s │%-38s│%-30s│%-5s│%-29s│%-15s", "time", "------------ network -----------", "----------- cpu ------------", "mem", "---------- disk ----------", "signal"), topWideTableWidth),
			topDashboardFitWidth(fmt.Sprintf("%8s │%5s %6s %6s %6s %5s %5s│%4s %5s %5s %4s %-8s│%5s│%5s %5s %5s %5s %5s│%-15s", "", "in", "out", "pk_in", "pk_out", "err", "drop", "load", "usr%", "sys%", "i/o", "hot core", "mem%", "read", "write", "iops", "await", "busy", ""), topWideTableWidth),
		}
	}
	return []string{
		topDashboardFit(fmt.Sprintf("%8s │%-12s│%-21s│%-11s│%-5s│%-18s", "time", "- network -", "------ cpu ------", "-- disk --", "mem", "signal")),
		topDashboardFit(fmt.Sprintf("%8s │%-12s│%-21s│%-11s│%-5s│", "", "in      out", "load usr% sys% i/o", "dsk_r dsk_w", "mem%")),
	}
}

func formatTopDashboardRow(row topDashboardRow, view topView, limits topLimits, wide bool) string {
	signal := topDashboardSignal(row.rate, row.processes, row.processesValid, limits)
	at, rate := row.at.Format("15:04:05"), row.rate
	if view != topViewAll {
		return formatTopColumns(at, topViewColumns(view), topViewCells(rate, view, signal))
	}
	if wide {
		errors, drops := topOptionalValue(rate.NetHealthValid, "%.0f", rate.NetErrors), topOptionalValue(rate.NetHealthValid, "%.0f", rate.NetDrops)
		iops, await, busy := topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskIOPS), topOptionalValue(rate.DiskHealthValid, "%.1f", rate.DiskAwait), topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskBusy)
		line := fmt.Sprintf("%8s │%5s %6s %6.0f %6.0f %5s %5s│%4.1f %5.1f %5.1f %4.1f %-8s│%5.1f│%5s %5s %5s %5s %5s│%-15s", at, formatRate(rate.NetIn), formatRate(rate.NetOut), rate.PacketsIn, rate.PacketsOut, errors, drops, rate.Load1, rate.CPUUser, rate.CPUSystem, rate.CPUIOWait, topHotCore(rate.CoreCPU), rate.MemoryPercent, formatRate(rate.DiskRead), formatRate(rate.DiskWrite), iops, await, busy, signal)
		return topDashboardFitWidth(line, topWideTableWidth)
	}
	return topDashboardFit(fmt.Sprintf("%8s │%5s %6s│%4.1f %5.1f %5.1f %4.1f│%5s %5s│%5.1f│%-18s", at, formatRate(rate.NetIn), formatRate(rate.NetOut), rate.Load1, rate.CPUUser, rate.CPUSystem, rate.CPUIOWait, formatRate(rate.DiskRead), formatRate(rate.DiskWrite), rate.MemoryPercent, signal))
}

// topSignalItem은 signal 후보다. score는 값을 danger 임계치로 나눈 값이라 단위가 다른 지표끼리 비교된다.
type topSignalItem struct {
	text  string
	score float64
}

func topDashboardSignal(rate resourceRate, processes []topProcess, valid bool, limits topLimits) string {
	signals := topSignals(rate, limits)
	if process, ok := topProcessSignal(processes, valid); ok {
		// process는 원인을 바로 가리키므로 점수와 상관없이 맨 앞에 두고, 나머지 경고는 개수로 남긴다.
		signals = append([]topSignalItem{{text: process}}, signals...)
	}
	return formatTopSignals(signals)
}

// ps의 CPU%는 core 하나를 100%로 계산한다. 80%부터 signal에 보여 주어
// 한 core를 오래 점유하는 process를 평균 CPU 수치보다 먼저 찾게 한다.
func topProcessSignal(processes []topProcess, valid bool) (string, bool) {
	if !valid || len(processes) == 0 || processes[0].CPU < topProcessSignalCPU {
		return "", false
	}
	return fmt.Sprintf("%s %.0f%%", topProcessName(processes[0].Command, topSignalProcessNameWidth), processes[0].CPU), true
}

// topProcessName은 화면에 쓸 process 이름이다. macOS ps는 전체 경로를 주므로 마지막 요소만 남기고,
// 이름에 섞인 제어 문자가 terminal escape로 해석되지 않게 바꾼다.
func topProcessName(command string, width int) string {
	name := strings.TrimSpace(command)
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, name)
	if runes := []rune(name); len(runes) > width {
		name = string(runes[:width])
	}
	if name == "" {
		return "proc"
	}
	return name
}

func topHotCore(cores []float64) string {
	if len(cores) == 0 {
		return "—"
	}
	index, usage := 0, cores[0]
	for current, value := range cores {
		if value > usage {
			index, usage = current, value
		}
	}
	return fmt.Sprintf("%d %.0f%%", index, usage)
}

func topCoreBar(cores []float64) string {
	if len(cores) == 0 {
		return "—"
	}
	if len(cores) > topCoreBarLimit {
		cores = cores[:topCoreBarLimit]
	}
	var result strings.Builder
	for _, usage := range cores {
		switch {
		case usage >= 90:
			result.WriteByte('#')
		case usage >= 70:
			result.WriteByte('*')
		case usage >= 40:
			result.WriteByte(':')
		default:
			result.WriteByte('.')
		}
	}
	return result.String()
}

func topDashboardFit(value string) string {
	return topDashboardFitWidth(value, topTableWidth)
}

func topDashboardFitWidth(value string, width int) string {
	runes := []rune(value)
	if len(runes) > width {
		return string(runes[:width])
	}
	return value + strings.Repeat(" ", width-len(runes))
}

func topSignal(rate resourceRate, limits topLimits) string {
	return formatTopSignals(topSignals(rate, limits))
}

func topSignals(rate resourceRate, limits topLimits) []topSignalItem {
	all := []topSignalItem{}
	if rate.MemoryPercent >= limits.memory.warn {
		all = append(all, topSignalItem{fmt.Sprintf("mem %.0f%%", rate.MemoryPercent), rate.MemoryPercent / limits.memory.danger})
	}
	if rate.Load1 >= limits.load.warn {
		all = append(all, topSignalItem{fmt.Sprintf("load %.1f", rate.Load1), rate.Load1 / limits.load.danger})
	}
	if rate.CPUIOWait >= limits.io.warn {
		all = append(all, topSignalItem{fmt.Sprintf("io %.1f%%", rate.CPUIOWait), rate.CPUIOWait / limits.io.danger})
	}
	if rate.CPUUser+rate.CPUSystem >= limits.cpu.warn {
		all = append(all, topSignalItem{fmt.Sprintf("cpu %.0f%%", rate.CPUUser+rate.CPUSystem), (rate.CPUUser + rate.CPUSystem) / limits.cpu.danger})
	}
	if rate.DiskHealthValid && rate.DiskAwait >= limits.await.warn {
		all = append(all, topSignalItem{fmt.Sprintf("await %.0fms", rate.DiskAwait), rate.DiskAwait / limits.await.danger})
	}
	if rate.NetHealthValid && rate.NetDrops >= limits.network.warn {
		all = append(all, topSignalItem{fmt.Sprintf("drop %.0f/s", rate.NetDrops), rate.NetDrops / limits.network.danger})
	}
	if rate.NetHealthValid && rate.NetErrors >= limits.network.warn {
		all = append(all, topSignalItem{fmt.Sprintf("err %.0f/s", rate.NetErrors), rate.NetErrors / limits.network.danger})
	}
	if rate.PSIValid && rate.PSIIO >= limits.psi.warn {
		all = append(all, topSignalItem{fmt.Sprintf("psi io %.0f%%", rate.PSIIO), rate.PSIIO / limits.psi.danger})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	return all
}

func formatTopSignals(signals []topSignalItem) string {
	switch len(signals) {
	case 0:
		return "-"
	case 1:
		return signals[0].text
	}
	return fmt.Sprintf("%s +%d", signals[0].text, len(signals)-1)
}

func topDiskDetail(rate resourceRate) string {
	if !rate.DiskHealthValid {
		return "disk health —"
	}
	return fmt.Sprintf("disk %.0f iops %.1fms %.0f%% busy", rate.DiskIOPS, rate.DiskAwait, rate.DiskBusy)
}

func topNetworkDetail(rate resourceRate) string {
	if !rate.NetHealthValid {
		return "network health —"
	}
	return fmt.Sprintf("net err %.0f/s drop %.0f/s", rate.NetErrors, rate.NetDrops)
}

func topPressureDetail(rate resourceRate) string {
	if !rate.PSIValid {
		return "pressure —"
	}
	return fmt.Sprintf("psi %.1f/%.1f/%.1f%%", rate.PSICPU, rate.PSIMemory, rate.PSIIO)
}

func topProcessDetail(processes []topProcess, valid bool) string {
	if !valid {
		return "processes —"
	}
	items := make([]string, 0, min(3, len(processes)))
	for _, process := range processes[:min(3, len(processes))] {
		items = append(items, fmt.Sprintf("%s %.0f%% %s", topProcessName(process.Command, topProcessNameWidth), process.CPU, formatProcessRSS(process.RSS)))
	}
	return "top " + strings.Join(items, ", ")
}

func formatProcessRSS(bytes uint64) string {
	const mib = 1024 * 1024
	if bytes >= 1024*mib {
		return fmt.Sprintf("%.1fG", float64(bytes)/(1024*mib))
	}
	if bytes >= mib {
		return fmt.Sprintf("%.1fM", float64(bytes)/mib)
	}
	return fmt.Sprintf("%.0fK", float64(bytes)/1024)
}
