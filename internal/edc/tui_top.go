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
func runTopDashboard(interval time.Duration, version string) int {
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
	model := newTopModel(details, first, interval, sampleTopDashboard)
	model.version = version
	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
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
	// topSignalMinWidth는 all 보기에 칸을 더할 때 signal에 남기는 최소 폭이다. "node 185% +2"와 "await 65ms +1"이 들어간다.
	topSignalMinWidth = 13
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
	// topHotCoreWarn은 hot core 칸에 경고를 주는 사용률이다. core 하나가 포화해도 core가 여럿이면
	// host 전체는 여유가 있으므로 host의 cpu 임계치보다 늦게 켜고 위험 단계를 두지 않는다.
	topHotCoreWarn = 90
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
	version       string
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
	case "pgup":
		if model.selected > 0 {
			model.selected = max(0, model.selected-model.bodyLines())
			model.follow = false
		}
	case "pgdown":
		if model.selected < len(model.rows)-1 {
			model.selected = min(len(model.rows)-1, model.selected+model.bodyLines())
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

// bodyLines는 표 본문에 쓸 수 있는 줄 수다. View와 PgUp·PgDn이 같은 값을 써서 한 화면씩 넘긴다.
func (model topModel) bodyLines() int {
	headers := len(topDashboardHeaders(model.view, max(topTableWidth, model.width)))
	return max(1, model.height-1-headers-len(model.panelLines())-len(model.statusLines()))
}

func (model topModel) View() tea.View {
	// 창 크기를 받기 전(width 0)이나 80열보다 좁을 때도 80열 표를 그리고 넘치는 부분은 renderer가 자른다.
	width := max(topTableWidth, model.width)
	lines := append([]string{model.dashboardTitle()}, topDashboardHeaders(model.view, width)...)
	panel, status := model.panelLines(), model.statusLines()
	bodyLines := model.bodyLines()
	start := max(0, len(model.rows)-bodyLines)
	if !model.follow && len(model.rows) > bodyLines {
		start = min(model.selected, len(model.rows)-bodyLines)
	}
	for index := start; index < len(model.rows) && index < start+bodyLines; index++ {
		line := formatTopDashboardRow(model.rows[index], model.view, model.limits, width)
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
	state := "live"
	if !model.follow {
		state = "history"
	}
	if model.lastErr != nil {
		state += " · sample error"
	}
	// 왼쪽은 host 정보, 오른쪽은 보기와 상태다. priority 0은 항상 보이고, 나머지는 폭이 허락하는 만큼
	// OS, memory, edc 버전, CPU 모델 순서로 더한다. 자리는 slice 순서를 따르므로 상태가 바뀌어도 host 정보가 움직이지 않는다.
	type titlePart struct {
		text     string
		priority int
	}
	version := ""
	if model.version != "" {
		version = "edc " + model.version
	}
	leftParts := []titlePart{
		{model.details.Hostname, 0},
		{topHostOS(model.details), 1},
		{model.details.Model, 4},
		{fmt.Sprintf("%d cores", model.details.Cores), 0},
		{topMemorySize(model.details.MemoryTotal), 2},
	}
	// "all latest"를 버전으로 읽는 일이 없게 보기 이름 앞에 view를 붙인다.
	rightParts := []titlePart{{"view " + string(model.view), 0}, {state, 0}, {version, 3}}
	join := func(parts []titlePart, priority int) string {
		texts := []string{}
		for _, part := range parts {
			if part.priority <= priority && part.text != "" {
				texts = append(texts, part.text)
			}
		}
		return strings.Join(texts, " · ")
	}
	// 상태를 표 오른쪽 끝에 맞춘다. all 보기의 표는 terminal 폭을 쓰고, 다른 보기의 표는 80열이다.
	width := topTableWidth
	if model.view == topViewAll {
		width = max(topTableWidth, model.width)
	}
	// 두 부분 사이에 최소 두 칸을 둔다. 한 칸이면 이어진 문장처럼 읽힌다.
	const gap = 2
	title := ""
	for priority := 0; priority <= 4; priority++ {
		left, right := "🐰 "+join(leftParts, priority), join(rightParts, priority)+" 🐰"
		space := width - topDisplayWidth(left) - topDisplayWidth(right)
		if space < gap {
			break
		}
		title = left + strings.Repeat(" ", space) + right
	}
	if title == "" {
		// host 이름이 길어 양쪽으로 뗄 수 없으면 한 줄로 잇는다. 넘치는 부분은 renderer가 자른다.
		title = "🐰 " + join(leftParts, 0) + " · " + join(rightParts, 0) + " 🐰"
	}
	return title
}

// topHostOS는 제목에 쓸 OS 이름이다. Linux의 Version은 os-release의 PRETTY_NAME이라 배포판 이름을 이미 담고 있다.
func topHostOS(details hostDetails) string {
	if details.System == "Linux" && details.Version != "" {
		return details.Version
	}
	return strings.TrimSpace(details.OS + " " + details.Version)
}

func topMemorySize(total uint64) string {
	if total == 0 {
		return ""
	}
	return formatBytes(total)
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
	// PgUp/Dn을 넣어도 80열에서 끝의 +/-가 잘리지 않게 구분 공백을 두 칸으로 맞췄다.
	actions := "keys ↑↓ PgUp/Dn history  End live  Enter detail  h peaks  q quit  p pause  +/-"
	// 폭을 먼저 맞춘다. escape가 rune 수에 들어가면 잘리는 위치가 어긋난다.
	return []string{liveMuted(topDashboardFit(views), model.limits.color), liveMuted(topDashboardFit(actions), model.limits.color)}
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
		return []topColumn{{title: "mem%", width: 6}, {title: "swap/s", width: 7}, {title: "psi mem", width: 7}, {title: "load", width: 6}, signal}
	case topViewDisk:
		return []topColumn{{title: "read/s", width: 7}, {title: "write/s", width: 7}, {title: "iops", width: 6}, {title: "await", width: 6}, {title: "busy%", width: 6}, signal}
	case topViewNetwork:
		return []topColumn{{title: "in/s", width: 7}, {title: "out/s", width: 7}, {title: "pk_in", width: 6}, {title: "pk_out", width: 6}, {title: "err/s", width: 6}, {title: "drop/s", width: 6}, signal}
	case topViewPressure:
		return []topColumn{{title: "cpu psi", width: 7}, {title: "mem psi", width: 7}, {title: "io psi", width: 7}, {title: "load", width: 6}, {title: "mem%", width: 6}, signal}
	}
	return nil
}

// topCell은 칸 하나의 표시 문자열과 위험도다. 둘을 따로 들고 있어야 폭을 먼저 맞춘 뒤 색을 입힐 수 있다.
type topCell struct {
	text  string
	level topLevel
}

// topPlainCell은 임계치가 없어 색을 쓰지 않는 칸이다.
func topPlainCell(text string) topCell {
	return topCell{text: text}
}

func topValueCell(format string, value float64, threshold topThreshold) topCell {
	return topCell{text: fmt.Sprintf(format, value), level: threshold.level(value)}
}

func topOptionalCell(valid bool, format string, value float64, threshold topThreshold) topCell {
	if !valid {
		return topPlainCell("—")
	}
	return topValueCell(format, value, threshold)
}

// topValidLevel은 platform이 주지 않는 값을 정상으로 둔다. —에는 색이 붙지 않는다.
func topValidLevel(valid bool, threshold topThreshold, value float64) topLevel {
	if !valid {
		return topLevelNormal
	}
	return threshold.level(value)
}

func topViewCells(rate resourceRate, view topView, signal string, limits topLimits) []topCell {
	switch view {
	case topViewCPU:
		return []topCell{topValueCell("%.1f", rate.Load1, limits.load), topValueCell("%.1f", rate.CPUUser, limits.cpu), topValueCell("%.1f", rate.CPUSystem, limits.cpu), topValueCell("%.1f", rate.CPUIOWait, limits.io), {text: topHotCore(rate.CoreCPU), level: topHotCoreLevel(rate.CoreCPU)}, topPlainCell(topCoreBar(rate.CoreCPU)), topPlainCell(signal)}
	case topViewMemory:
		return []topCell{topValueCell("%.1f", rate.MemoryPercent, limits.memory), topPlainCell(formatRate(rate.SwapOut)), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIMemory, limits.psi), topValueCell("%.1f", rate.Load1, limits.load), topPlainCell(signal)}
	case topViewDisk:
		return []topCell{topPlainCell(formatRate(rate.DiskRead)), topPlainCell(formatRate(rate.DiskWrite)), topPlainCell(topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskIOPS)), topOptionalCell(rate.DiskHealthValid, "%.1f", rate.DiskAwait, limits.await), topPlainCell(topOptionalValue(rate.DiskBusyValid, "%.0f", rate.DiskBusy)), topPlainCell(signal)}
	case topViewNetwork:
		return []topCell{topPlainCell(formatRate(rate.NetIn)), topPlainCell(formatRate(rate.NetOut)), topPlainCell(fmt.Sprintf("%.0f", rate.PacketsIn)), topPlainCell(fmt.Sprintf("%.0f", rate.PacketsOut)), topOptionalCell(rate.NetHealthValid, "%.0f", rate.NetErrors, limits.network), topOptionalCell(rate.NetHealthValid, "%.0f", rate.NetDrops, limits.network), topPlainCell(signal)}
	case topViewPressure:
		return []topCell{topOptionalCell(rate.PSIValid, "%.1f", rate.PSICPU, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIMemory, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIIO, limits.psi), topValueCell("%.1f", rate.Load1, limits.load), topValueCell("%.1f", rate.MemoryPercent, limits.memory), topPlainCell(signal)}
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

// formatTopColumns는 칸마다 폭을 먼저 맞춘 뒤 색을 입힌다. 줄 전체를 나중에 자르면
// escape가 rune 수에 섞여 자르는 위치가 어긋나므로 줄 단위 절단을 쓰지 않는다.
func formatTopColumns(at string, columns []topColumn, cells []topCell, color bool) string {
	var line strings.Builder
	fmt.Fprintf(&line, "%8s │", at)
	used := topSelectionColumn + 2
	for index, column := range columns {
		if index > 0 {
			line.WriteString("│")
			used++
		}
		width := column.width
		if width == 0 {
			width = max(0, topTableWidth-used)
		}
		used += width
		line.WriteString(topPaint(topFitCell(cells[index].text, width, column.left), cells[index].level, color))
	}
	return line.String()
}

// topFitCell은 칸 하나를 폭에 맞춘다. 색이 붙기 전이라 rune 단위로 잘라도 escape가 끊기지 않는다.
func topFitCell(text string, width int, left bool) string {
	runes := []rune(text)
	if len(runes) > width {
		return string(runes[:width])
	}
	gap := strings.Repeat(" ", width-len(runes))
	if left {
		return text + gap
	}
	return gap + text
}

// topAllColumn은 all 보기의 한 칸이다. tier 0은 항상 보이고, 나머지는 terminal이 넓어질수록 tier 순서대로 추가된다.
// 순서는 진단에 쓸모가 큰 값부터다: hot core, disk iops·await, packet, network err·drop, disk busy.
type topAllColumn struct {
	group string
	title string
	width int
	tier  int
	left  bool
	cell  func(rate resourceRate) string
	// level은 칸을 칠할 위험도다. nil이면 임계치가 없어 색을 쓰지 않는 칸이다.
	level func(limits topLimits, rate resourceRate) topLevel
}

var topAllColumns = []topAllColumn{
	{group: "network", title: "in", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.NetIn) }},
	{group: "network", title: "out", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.NetOut) }},
	{group: "network", title: "pk_in", width: 6, tier: 3, cell: func(rate resourceRate) string { return topCompactCount(rate.PacketsIn, 6) }},
	{group: "network", title: "pk_out", width: 6, tier: 3, cell: func(rate resourceRate) string { return topCompactCount(rate.PacketsOut, 6) }},
	{group: "network", title: "err", width: 4, tier: 4, cell: func(rate resourceRate) string { return topOptionalCount(rate.NetHealthValid, rate.NetErrors, 4) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.NetHealthValid, limits.network, rate.NetErrors)
		}},
	{group: "network", title: "drop", width: 4, tier: 4, cell: func(rate resourceRate) string { return topOptionalCount(rate.NetHealthValid, rate.NetDrops, 4) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.NetHealthValid, limits.network, rate.NetDrops)
		}},
	{group: "cpu", title: "load", width: 4, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.Load1) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.load.level(rate.Load1) }},
	{group: "cpu", title: "usr%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUUser) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.cpu.level(rate.CPUUser) }},
	{group: "cpu", title: "sys%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUSystem) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.cpu.level(rate.CPUSystem) }},
	{group: "cpu", title: "i/o", width: 4, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUIOWait) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.io.level(rate.CPUIOWait) }},
	{group: "cpu", title: "hot core", width: 8, tier: 1, left: true, cell: func(rate resourceRate) string { return topHotCore(rate.CoreCPU) },
		level: func(limits topLimits, rate resourceRate) topLevel { return topHotCoreLevel(rate.CoreCPU) }},
	{group: "mem", title: "mem%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.MemoryPercent) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.memory.level(rate.MemoryPercent) }},
	{group: "disk", title: "read", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.DiskRead) }},
	{group: "disk", title: "write", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.DiskWrite) }},
	{group: "disk", title: "iops", width: 5, tier: 2, cell: func(rate resourceRate) string { return topOptionalCount(rate.DiskHealthValid, rate.DiskIOPS, 5) }},
	{group: "disk", title: "await", width: 5, tier: 2, cell: func(rate resourceRate) string { return topOptionalValue(rate.DiskHealthValid, "%.1f", rate.DiskAwait) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.DiskHealthValid, limits.await, rate.DiskAwait)
		}},
	{group: "disk", title: "busy", width: 4, tier: 5, cell: func(rate resourceRate) string { return topOptionalValue(rate.DiskBusyValid, "%.0f", rate.DiskBusy) }},
}

// topAllLayout은 width 안에 signal 최소 폭까지 들어가는 가장 높은 tier의 칸을 고르고, 남는 폭을 signal에 준다.
func topAllLayout(width int) ([]topAllColumn, int) {
	chosen := topAllColumnsUpTo(0)
	for tier := 1; tier <= topAllMaxTier(); tier++ {
		candidate := topAllColumnsUpTo(tier)
		if topAllLineWidth(candidate)+topSignalMinWidth > width {
			break
		}
		chosen = candidate
	}
	return chosen, max(topSignalMinWidth, width-topAllLineWidth(chosen))
}

func topAllMaxTier() int {
	tier := 0
	for _, column := range topAllColumns {
		tier = max(tier, column.tier)
	}
	return tier
}

func topAllColumnsUpTo(tier int) []topAllColumn {
	columns := []topAllColumn{}
	for _, column := range topAllColumns {
		if column.tier <= tier {
			columns = append(columns, column)
		}
	}
	return columns
}

// topAllLineWidth는 signal 앞까지의 폭이다. 시각 뒤 " │"에 이어 같은 group 칸 사이 공백과 group 끝 구분선이 붙는다.
func topAllLineWidth(columns []topAllColumn) int {
	width := topSelectionColumn + 2
	for index, column := range columns {
		if index > 0 && columns[index-1].group == column.group {
			width++
		}
		width += column.width
		if index == len(columns)-1 || columns[index+1].group != column.group {
			width++
		}
	}
	return width
}

// formatTopAllLine은 둘째 헤더 줄과 행을 같은 규칙으로 그려 구분선 위치를 맞춘다.
func formatTopAllLine(first string, columns []topAllColumn, cells []topCell, signal string, signalWidth int, color bool) string {
	var line strings.Builder
	fmt.Fprintf(&line, "%8s │", first)
	for index, column := range columns {
		if index > 0 && columns[index-1].group == column.group {
			line.WriteByte(' ')
		}
		line.WriteString(topPaint(topFitCell(cells[index].text, column.width, column.left), cells[index].level, color))
		if index == len(columns)-1 || columns[index+1].group != column.group {
			line.WriteString("│")
		}
	}
	// 칸마다 폭을 맞췄으므로 여기까지가 정확히 topAllLineWidth다. signal만 남은 폭에 맞춘다.
	line.WriteString(topFitCell(signal, signalWidth, true))
	return line.String()
}

// formatTopAllGroupHeader는 첫 헤더 줄이다. group 이름이 그 group 칸들을 합친 폭을 차지한다.
func formatTopAllGroupHeader(columns []topAllColumn, signalWidth int) string {
	var line strings.Builder
	fmt.Fprintf(&line, "%8s │", "time")
	for start := 0; start < len(columns); {
		end, span := start, columns[start].width
		for end+1 < len(columns) && columns[end+1].group == columns[start].group {
			end++
			span += 1 + columns[end].width
		}
		line.WriteString(topGroupTitle(columns[start].group, span))
		line.WriteString("│")
		start = end + 1
	}
	line.WriteString("signal")
	return topDashboardFitWidth(line.String(), topAllLineWidth(columns)+signalWidth)
}

// topGroupTitle은 group 이름을 폭 가운데에 두고 양옆을 -로 채운다. 여유가 없으면 이름만 왼쪽에 둔다.
func topGroupTitle(name string, width int) string {
	dashes := width - len(name) - 2
	if dashes < 2 {
		return fmt.Sprintf("%-*s", width, name)
	}
	return strings.Repeat("-", dashes/2) + " " + name + " " + strings.Repeat("-", dashes-dashes/2)
}

// topCompactCount는 칸보다 긴 초당 개수를 k, M 단위로 줄여 열 정렬을 지킨다.
func topCompactCount(value float64, width int) string {
	text := fmt.Sprintf("%.0f", value)
	if len(text) > width {
		text = fmt.Sprintf("%.0fk", value/1e3)
	}
	if len(text) > width {
		text = fmt.Sprintf("%.0fM", value/1e6)
	}
	return text
}

func topOptionalCount(valid bool, value float64, width int) string {
	if !valid {
		return "—"
	}
	return topCompactCount(value, width)
}

func topDashboardHeaders(view topView, width int) []string {
	if view != topViewAll {
		columns := topViewColumns(view)
		titles := make([]topCell, len(columns))
		for index, column := range columns {
			titles[index] = topPlainCell(column.title)
		}
		return []string{formatTopColumns("time", columns, titles, false)}
	}
	columns, signalWidth := topAllLayout(width)
	titles := make([]topCell, len(columns))
	for index, column := range columns {
		titles[index] = topPlainCell(column.title)
	}
	return []string{formatTopAllGroupHeader(columns, signalWidth), formatTopAllLine("", columns, titles, "", signalWidth, false)}
}

// formatTopDashboardRow의 width는 all 보기에만 쓴다. 다른 보기는 80열 고정 칸이다.
func formatTopDashboardRow(row topDashboardRow, view topView, limits topLimits, width int) string {
	at, rate := row.at.Format("15:04:05"), row.rate
	if view != topViewAll {
		signal := topDashboardSignal(rate, row.processes, row.processesValid, limits)
		return formatTopColumns(at, topViewColumns(view), topViewCells(rate, view, signal, limits), limits.color)
	}
	columns, signalWidth := topAllLayout(width)
	cells := make([]topCell, len(columns))
	for index, column := range columns {
		cells[index] = topPlainCell(column.cell(rate))
		if column.level != nil {
			cells[index].level = column.level(limits, rate)
		}
	}
	signals := topDashboardSignalItems(rate, row.processes, row.processesValid, limits)
	return formatTopAllLine(at, columns, cells, formatTopSignalsWidth(signals, signalWidth), signalWidth, limits.color)
}

// topSignalItem은 signal 후보다. score는 값을 danger 임계치로 나눈 값이라 단위가 다른 지표끼리 비교된다.
type topSignalItem struct {
	text  string
	score float64
}

func topDashboardSignal(rate resourceRate, processes []topProcess, valid bool, limits topLimits) string {
	return formatTopSignals(topDashboardSignalItems(rate, processes, valid, limits))
}

func topDashboardSignalItems(rate resourceRate, processes []topProcess, valid bool, limits topLimits) []topSignalItem {
	signals := topSignals(rate, limits)
	if process, ok := topProcessSignal(processes, valid); ok {
		// process는 원인을 바로 가리키므로 점수와 상관없이 맨 앞에 두고, 나머지 경고는 개수로 남긴다.
		signals = append([]topSignalItem{{text: process}}, signals...)
	}
	return signals
}

// formatTopSignalsWidth는 폭 안에 들어가는 만큼 경고를 " · "로 잇고, 넣지 못한 경고는 +N으로 센다.
// 첫 경고는 폭이 좁아도 남겨 가장 중요한 원인을 가리지 않는다.
func formatTopSignalsWidth(signals []topSignalItem, width int) string {
	if len(signals) <= 1 {
		return formatTopSignals(signals)
	}
	text, shown := signals[0].text, 1
	for shown < len(signals) {
		next, tail := text+" · "+signals[shown].text, ""
		if rest := len(signals) - shown - 1; rest > 0 {
			tail = fmt.Sprintf(" +%d", rest)
		}
		if len([]rune(next+tail)) > width {
			break
		}
		text, shown = next, shown+1
	}
	if rest := len(signals) - shown; rest > 0 {
		text += fmt.Sprintf(" +%d", rest)
	}
	return text
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

// topHotCoreLevel은 hot core 칸의 위험도다. 위험 단계 없이 경고만 준다.
func topHotCoreLevel(cores []float64) topLevel {
	if topHotCoreUsage(cores) >= topHotCoreWarn {
		return topLevelWarn
	}
	return topLevelNormal
}

// topHotCoreUsage는 가장 바쁜 core의 사용률이다.
func topHotCoreUsage(cores []float64) float64 {
	usage := 0.0
	for _, value := range cores {
		if value > usage {
			usage = value
		}
	}
	return usage
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
	busy := "busy —"
	if rate.DiskBusyValid {
		busy = fmt.Sprintf("%.0f%% busy", rate.DiskBusy)
	}
	return fmt.Sprintf("disk %.0f iops %.1fms %s", rate.DiskIOPS, rate.DiskAwait, busy)
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
