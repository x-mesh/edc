package edc

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// runTopDashboard는 alt screen 대시보드를 실행한다. 종료하면 화면이 원래대로 돌아온다.
func runTopDashboard(interval time.Duration, version string, filter topProcessFilter, recorder *topRecorder, split []topView) int {
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.host", err))
		return 1
	}
	sample := sampleTopDashboard
	if filter.active() {
		sample = sampleTopDashboardNow
	}
	first, err := sample()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.error.resource", err))
		return 1
	}
	model := newTopModel(details, first, interval, sampleTopDashboard)
	model.sampleNow, model.setFilter, model.setScanIO = sampleTopDashboardNow, processSampler.setFilter, processSampler.setScanIO
	model.version = version
	model.limits.color = os.Getenv("NO_COLOR") == ""
	processSampler.mutex.Lock()
	model.probeEnabled = processSampler.observe != nil
	processSampler.mutex.Unlock()
	model = model.withProcessFilter(filter).withSplit(split)
	if recorder != nil {
		model.record = recorder.Record
		model.recordFailure = func() tea.Msg {
			select {
			case <-recorder.failed:
				return topRecordingErrorMsg{err: recorder.Err()}
			case <-recorder.done:
				return nil
			}
		}
	}
	final, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if final.(topModel).recordingErr != nil {
		return 1
	}
	return 0
}

// sampleTopDashboard는 snapshot에 process 목록을 붙인다. 표와 JSON 출력은 process를 쓰지 않으므로
// collectResourceSnapshot이 아니라 대시보드에서만 process를 수집한다.
func sampleTopDashboard() (resourceSnapshot, error) {
	snapshot, err := collectResourceSnapshot()
	snapshot.Processes, snapshot.ProcessTotal, snapshot.ProcessesValid, snapshot.ProcessesAt = processSampler.latestWithTotalAt()
	fillProcsBlocked(&snapshot)
	return snapshot, err
}

// fillProcsBlocked는 kernel이 멈춘 작업 수를 주지 않는 host에서 process 목록의 멈춘 process 수를 쓴다.
// Linux의 procs_blocked는 thread를 세지만 이 값은 process를 센다.
func fillProcsBlocked(snapshot *resourceSnapshot) {
	if !snapshot.ProcsBlockedSource.known() && snapshot.ProcessTotal.BlockedValid {
		snapshot.ProcsBlocked, snapshot.ProcsBlockedSource = uint64(snapshot.ProcessTotal.Blocked), topBlockedProcessList
	}
}

// sampleTopDashboardNow는 필터를 건 대시보드가 쓴다. process 보기는 행마다 그 시점의 값이 필요하므로, 배경 갱신이
// 끝난 직전 목록을 다시 쓰지 않고 지금 읽는다. tick의 tea.Cmd 안에서 돌아 /proc을 읽는 동안 화면은 멈추지 않는다.
func sampleTopDashboardNow() (resourceSnapshot, error) {
	snapshot, err := collectResourceSnapshot()
	snapshot.Processes, snapshot.ProcessTotal, snapshot.ProcessesValid, snapshot.ProcessesAt = processSampler.refreshNowAt()
	fillProcsBlocked(&snapshot)
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
	// topViewProcess는 --process에 맞은 process 묶음을 시점마다 한 행으로 보인다. 필터가 있을 때만 고를 수 있다.
	topViewProcess topView = "process"
)

const (
	// topSignalMinWidth는 all 보기에 칸을 더할 때 signal에 남기는 최소 폭이다. "node 185% +2"와 "await 65ms +1"이 들어간다.
	topSignalMinWidth = 13
	// topSignalWideWidth는 all 보기가 남는 폭을 칸에 나눌 때 signal에 남기는 폭이다. 경고 하나와 +N이 들어간다.
	topSignalWideWidth = 16
	// topPeakWindow는 h 패널이 지표별 최고치를 찾는 구간이다.
	topPeakWindow = time.Minute
	// topSelectionColumn은 행에서 "15:04:05" 바로 뒤 공백 자리다. 선택 표시가 시각을 가리지 않는다.
	topSelectionColumn = 8
	// topFooterProcessWidth는 아래 영역 오른쪽 process 후보 칸의 폭이다. 이름 칸이 쌓을 때와 같은 28칸이 된다.
	topFooterProcessWidth = 63
	topFooterGap          = 2
	// topFooterDockWidth부터 process 후보를 오른쪽에 둔다. 왼쪽 패널은 80열에 맞춰 줄을 나눠 두었으므로 왼쪽에 80열이 남아야 한다.
	topFooterDockWidth = topTableWidth + topFooterGap + topFooterProcessWidth
	// topTallHeight부터 process 패널이 후보를 topProcessLimit개까지 보인다. 두 줄을 더 써도 history가 28행 남는다.
	topTallHeight = 40
	// topProcessNameWidth는 상세 패널의 process 이름 폭이다. 세 개가 80열 한 줄에 들어간다.
	topProcessNameWidth = 10
	// topSignalProcessNameWidth는 signal 열의 process 이름 폭이다.
	topSignalProcessNameWidth = 4
	// topProcessSignalCPU는 process 하나가 signal에 오르는 CPU%다. core 하나가 100%다.
	topProcessSignalCPU = 80
	// topProcessIOMinWidth는 후보 줄에 READ·WRITE 칸을 붙이는 최소 폭이다.
	topProcessIOMinWidth = 60
	// topProcessStateWidth는 STATE 칸의 폭이다. 가장 긴 이름인 iowait와 zombie가 들어간다.
	topProcessStateWidth = 6
	// topProcessColumnsWidth는 후보 줄에서 이름 칸을 뺀 표시·PID·STATE·CPU%·RSS 칸과 사이 공백의 폭이다.
	topProcessColumnsWidth = 26 + topProcessStateWidth
	// topProcessIOColumnsWidth는 READ·WRITE 두 칸과 앞 공백의 폭이다. 63칸 후보 패널에서 이름 칸이 17칸 남는다.
	topProcessIOColumnsWidth = 14
	// topBlockedSignalMin은 iowait이 높을 때 blocked가 signal에 오르는 D state 작업 수다. 4 core host의 core 수와 같다.
	topBlockedSignalMin = 4
	// topBlockedSignalDanger는 blocked 경고의 순위를 다른 경고와 맞추는 기준 작업 수다.
	topBlockedSignalDanger = 16
	// topCoreBarLimit는 CPU 보기 막대에 그리는 최대 core 수다.
	topCoreBarLimit = 24
	// topHotCoreWarn은 hot core 칸에 경고를 주는 사용률이다. core 하나가 포화해도 core가 여럿이면
	// host 전체는 여유가 있으므로 host의 cpu 임계치보다 늦게 켜고 위험 단계를 두지 않는다.
	topHotCoreWarn = 90
)

// topProcessIOThreshold는 후보의 READ·WRITE 칸에 색을 입히는 byte/s다. 디스크가 한가할 때 작은 값에 색이
// 들지 않도록 host 처리량 대비 비율이 아니라 절대값으로 정한다.
var topProcessIOThreshold = topThreshold{warn: 10 << 20, danger: 50 << 20}

// topDashboardRow는 포맷 문자열 대신 측정값을 보존한다. 같은 시점을 다른 렌즈로
// 다시 그릴 수 있고, 선택한 과거 행의 상세도 최신 값과 섞이지 않는다.
type topDashboardRow struct {
	at             time.Time
	rate           resourceRate
	processes      []topProcess
	processTotal   topProcessTotal
	processesValid bool
	// filter는 이 행을 수집할 때 걸린 필터다. 실행 중에 필터를 바꾸면 그 전 행은 지금 필터의 값이 아니다.
	filter string
}

type topModel struct {
	details  hostDetails
	limits   topLimits
	interval time.Duration
	paused   bool
	previous resourceSnapshot
	rows     []topDashboardRow
	view     topView
	follow   bool
	baseline bool
	selected int
	detail   bool
	peaks    bool
	// events는 경고가 이어진 구간의 기록이다. 모델은 값으로 복사되므로 포인터로 두어 Update 사이에 이어진다.
	events        *topEventLog
	eventsOpen    bool
	eventSelected int
	width, height int
	sample        func() (resourceSnapshot, error)
	seq           int
	lastErr       error
	version       string
	processFilter topProcessFilter
	// focusName은 f로 PID에 초점을 맞췄을 때 배너에 함께 보일 process 이름이다.
	focusName string
	// sampleNow는 필터가 있을 때 쓰는 수집 함수다. 없으면 sample을 쓴다.
	sampleNow func() (resourceSnapshot, error)
	// setFilter는 실행 중에 바꾼 필터를 process 수집기에 알린다.
	setFilter func(topProcessFilter)
	// setScanIO는 디스크 보기에서 모든 process의 I/O를 읽게 수집기에 알린다.
	setScanIO func(bool)
	// notice는 다음 키까지 상태 줄에 보이는 안내다.
	notice string
	// input은 /로 연 필터 입력 중인지다. inputText는 입력한 글자다.
	input           bool
	inputText       string
	help            bool
	helpOffset      int
	processFocus    bool
	processSelected int
	probeEnabled    bool
	record          func(historyTopSample) error
	recordFailure   tea.Cmd
	recordingErr    error
	// split은 박스로 보일 보기 목록이고, 비어 있으면 전체다. boxed는 박스 안에서 그리는 표임을 나타낸다.
	split []topView
	boxed bool
	// compact는 박스가 optional 칸을 빼고 그리는지다.
	compact bool
}

type topRecordingErrorMsg struct{ err error }

// topSampleMsg는 tick마다 수집한 snapshot이다. seq가 다르면 interval이 바뀐 뒤의 낡은 tick이다.
type topSampleMsg struct {
	seq      int
	snapshot resourceSnapshot
	err      error
}

func newTopModel(details hostDetails, first resourceSnapshot, interval time.Duration, sample func() (resourceSnapshot, error)) topModel {
	return topModel{details: details, limits: newTopLimits(details.Cores, true), interval: interval, previous: first, view: topViewAll, follow: true, sample: sample, events: &topEventLog{}}
}

// withProcessFilter는 필터를 건다. 필터를 건 사용자는 그 process를 보려는 것이므로 host 지표 대신 process 묶음 보기로 연다.
func (model topModel) withProcessFilter(filter topProcessFilter) topModel {
	model.processFilter = filter
	if filter.active() {
		model.view = topViewProcess
	}
	return model
}

func (model topModel) Init() tea.Cmd { return tea.Batch(model.tick(), model.recordFailure) }

func (model topModel) tick() tea.Cmd {
	seq, sample := model.seq, model.sample
	if model.setScanIO != nil {
		model.setScanIO(model.view == topViewDisk)
	}
	if model.processFilter.active() && model.sampleNow != nil {
		sample = model.sampleNow
	}
	return tea.Tick(model.interval, func(time.Time) tea.Msg {
		snapshot, err := sample()
		return topSampleMsg{seq: seq, snapshot: snapshot, err: err}
	})
}

func (model topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case topRecordingErrorMsg:
		model.recordingErr = value.err
		return model, tea.Quit
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
		rate := calculateRate(model.previous, value.snapshot)
		if model.record != nil {
			if err := model.record(newHistoryTopSample(model.details, model.previous, value.snapshot, rate, model.processFilter.String())); err != nil {
				model.recordingErr = err
				return model, tea.Quit
			}
		}
		before := len(model.rows) + 1
		model.rows = appendTopDashboardRow(model.rows, topDashboardRow{at: value.snapshot.TakenAt, rate: rate, processes: value.snapshot.Processes, processTotal: value.snapshot.ProcessTotal, processesValid: value.snapshot.ProcessesValid, filter: model.processFilter.String()})
		model = model.recordEvents(model.rows[len(model.rows)-1])
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
	if model.input {
		return model.updateInput(key)
	}
	model.notice = ""
	if model.help {
		switch key.String() {
		case "?", "esc":
			model.help = false
		case "q", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			model.helpOffset = max(0, model.helpOffset-1)
		case "down", "j":
			model.helpOffset = min(max(0, len(model.helpLines())-max(1, model.height-2)), model.helpOffset+1)
		case "pgdown":
			model.helpOffset = min(max(0, len(model.helpLines())-max(1, model.height-2)), model.helpOffset+max(1, model.height-2))
		case "pgup":
			model.helpOffset = max(0, model.helpOffset-max(1, model.height-2))
		}
		return model, nil
	}
	if model.eventsOpen {
		switch key.String() {
		case "e", "esc":
			model.eventsOpen = false
		case "q", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			model.eventSelected = max(0, model.eventSelected-1)
		case "down", "j":
			model.eventSelected = min(max(0, len(topEventClusters(model.eventList()))-1), model.eventSelected+1)
		case "enter":
			if cluster, ok := model.newestCluster(model.eventSelected); ok {
				model.eventsOpen = false
				model = model.jumpToEvent(cluster.worst(), cluster[0].start)
			}
		}
		return model, nil
	}
	if model.processFocus {
		switch key.String() {
		case "up", "k":
			model.processSelected = max(0, model.processSelected-1)
			return model, nil
		case "down", "j":
			model.processSelected = min(max(0, len(model.candidates())-1), model.processSelected+1)
			return model, nil
		case "pgup":
			model.processSelected = 0
			return model, nil
		case "pgdown":
			model.processSelected = max(0, len(model.candidates())-1)
			return model, nil
		case "enter":
			return model.focusProcess()
		case "esc":
			model.processFocus = false
			return model, nil
		}
	}
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
	case "v":
		model = model.enterSplit()
	case "s":
		model.view = topViewPressure
	case "?":
		model.help = true
		model.helpOffset = 0
	case "tab":
		model.processFocus = !model.processFocus
		model.processSelected = 0
		if model.processFocus {
			model.follow, model.peaks, model.detail = false, false, false
		}
	case "f":
		return model.followSignal()
	case "e":
		model.eventsOpen, model.eventSelected = true, 0
	case "[":
		model = model.stepEvent(-1)
	case "]":
		model = model.stepEvent(1)
	case "/":
		model.input, model.inputText = true, model.processFilter.String()
		if model.inputText == "" {
			if candidates := model.candidates(); len(candidates) > 0 {
				model.inputText = topFilterSeed(candidates[min(model.processSelected, len(candidates)-1)].Command)
			}
		}
	case "esc":
		if model.processFilter.active() {
			model.notice = "process filter cleared"
			return model.applyFilter(topProcessFilter{}, "")
		}
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
		model.processFocus = false
		if len(model.rows) > 0 {
			model.selected, model.follow = len(model.rows)-1, true
		}
	}
	if key.String() == "1" || key.String() == "c" || key.String() == "m" || key.String() == "d" || key.String() == "n" || key.String() == "s" || key.String() == "v" {
		model.processSelected = 0
	}
	return model, nil
}

func (model topModel) followSignal() (tea.Model, tea.Cmd) {
	if model.processFilter.active() {
		model.view = topViewProcess
		return model, nil
	}
	row, ok := model.selectedRow()
	if !ok || !row.processesValid {
		model.notice = "f: waiting for a sample"
		return model, nil
	}
	items := topDashboardSignalItems(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits)
	if len(items) > 0 && items[0].view != topViewProcess && model.view != items[0].view {
		model.view = items[0].view
		model.notice = fmt.Sprintf("f: %s → %s view · f again selects a candidate", items[0].text, items[0].view)
		return model, nil
	}
	if len(row.processes) == 0 {
		model.notice = "f: no process in this sample"
		return model, nil
	}
	model.processFocus, model.follow, model.detail, model.peaks = true, false, false, false
	model.processSelected = 0
	model.notice = "process candidates · ↑↓ select · Enter focus · Esc back"
	return model, nil
}

func (model topModel) focusProcess() (tea.Model, tea.Cmd) {
	candidates := model.candidates()
	if len(candidates) == 0 {
		model.notice = "no candidate in this sample · / filter"
		return model, nil
	}
	process := candidates[min(model.processSelected, len(candidates)-1)]
	filter, err := parseTopProcessFilter(strconv.Itoa(process.PID))
	if err != nil {
		model.notice = "f: " + err.Error()
		return model, nil
	}
	name := topProcessName(process.Command, topProcessNameWidth)
	model.notice = fmt.Sprintf("focus pid %d %s · / edits · Esc clears", process.PID, name)
	return model.applyFilter(filter, name)
}

// updateInput은 /로 연 필터 입력을 받는다. Enter는 적용하고, Esc는 바꾸지 않고 닫는다. 빈 값을 적용하면 필터를 지운다.
func (model topModel) updateInput(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return model, tea.Quit
	case "esc":
		model.input = false
	case "enter":
		model.input = false
		filter, err := parseTopProcessFilter(model.inputText)
		if err != nil {
			model.notice = T("observe.top.process_invalid")
			return model, nil
		}
		return model.applyFilter(filter, "")
	case "backspace":
		if runes := []rune(model.inputText); len(runes) > 0 {
			model.inputText = string(runes[:len(runes)-1])
		}
	default:
		if key.Text != "" {
			model.inputText += key.Text
		}
	}
	return model, nil
}

// applyFilter는 실행 중에 필터를 바꾼다. 순번을 올려 이전 필터로 수집 중인 표본을 버리고, 필터가 있으면 process 보기로 연다.
func (model topModel) applyFilter(filter topProcessFilter, name string) (tea.Model, tea.Cmd) {
	model.processFocus, model.processSelected = false, 0
	model.follow = true
	model.processFilter, model.focusName = filter, name
	if model.setFilter != nil {
		model.setFilter(filter)
	}
	model.view = topViewAll
	if filter.active() {
		model.view = topViewProcess
	}
	model.seq++
	if model.paused {
		return model, nil
	}
	return model, model.tick()
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
	if model.view == topViewSplit {
		return model.splitBodyLines()
	}
	headers := len(model.tableHeader())
	return max(1, model.height-1-len(model.processBanner())-headers-len(model.footerLines()))
}

func (model topModel) View() tea.View {
	if model.width > 0 && (model.width < 24 || model.height < 8) {
		view := tea.NewView(ansi.Truncate("terminal too small · q quit", model.displayWidth(), "") + "\n" + ansi.Truncate("resize to at least 24×8", model.displayWidth(), ""))
		view.AltScreen = true
		return view
	}
	lines := append([]string{model.dashboardTitle()}, model.processBanner()...)
	if model.help {
		lines = []string{topDashboardFitWidth("Help · ↑↓ scroll · ?/Esc back · q quit", model.displayWidth())}
		help := model.helpLines()
		start := min(model.helpOffset, max(0, len(help)-1))
		end := min(len(help), start+max(1, model.height-2))
		lines = append(lines, help[start:end]...)
		view := tea.NewView(strings.Join(lines, "\n"))
		view.AltScreen = true
		return view
	}
	if model.eventsOpen {
		lines = []string{topDashboardFitWidth("Events · ↑↓ select · Enter jump to its start · e/Esc back · q quit", model.displayWidth())}
		lines = append(lines, model.eventListLines()...)
		for index := range lines {
			lines[index] = ansi.Truncate(lines[index], model.displayWidth(), "")
		}
		view := tea.NewView(strings.Join(lines, "\n"))
		view.AltScreen = true
		return view
	}
	if model.view == topViewSplit {
		return model.splitView()
	}
	lines = append(lines, model.tableHeader()...)
	footer := model.footerLines()
	bodyLines := model.bodyLines()
	start := max(0, len(model.rows)-bodyLines)
	if !model.follow && len(model.rows) > bodyLines {
		start = min(model.selected, len(model.rows)-bodyLines)
	}
	starts := model.eventStarts()
	for index := start; index < len(model.rows) && index < start+bodyLines; index++ {
		line := model.tableRow(model.currentFilterRow(model.rows[index]))
		selected := index == model.selected && !model.follow
		// 고른 행은 같은 칸에 >를 쓰므로 이벤트 표시를 넣지 않는다.
		if level, ok := starts[model.rows[index].at.UnixNano()]; ok && !selected {
			line = line[:topSelectionColumn] + topPaint("!", level, model.limits.color) + line[topSelectionColumn+1:]
		}
		if selected {
			line = line[:topSelectionColumn] + ">" + line[topSelectionColumn+1:]
			if !model.processFocus && model.limits.color {
				line = topBannerStyle + strings.ReplaceAll(topDashboardFitWidth(line, model.displayWidth()), topColorReset, topColorReset+topBannerStyle) + topColorReset
			}
		}
		lines = append(lines, line)
	}
	lines = append(model.padToFooter(lines, footer), footer...)
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], model.displayWidth(), "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// panelLines는 detail이나 peaks 패널이다. renderer는 넘치는 줄을 접지 않고 자르므로
// 패널을 여러 줄로 나눠 80열에서도 끝까지 보이게 한다.
func (model topModel) panelLines() []string {
	lines := append(model.infoLines(), model.candidateLines()...)
	if model.height > 0 {
		available := max(0, model.height-2-len(model.processBanner())-len(model.tableHeader())-len(model.statusLines()))
		if len(lines) > available {
			lines = lines[:available]
		}
	}
	for index, line := range lines {
		lines[index] = topDashboardFitWidth(line, model.displayWidth())
	}
	return lines
}

// infoLines는 process 후보 위에 보이는 detail, peaks, network 패널과 수집 실패 줄이다.
// panelLinesWithEvents는 좁은 아래 영역에서 process 목록을 후보 칸 폭으로 줄이고 남는 오른쪽에 이벤트를 둔다.
func (model topModel) panelLinesWithEvents() []string {
	right := model
	right.width = topFooterProcessWidth
	candidates := right.candidateLines()
	events := model.eventLines(model.displayWidth()-topFooterProcessWidth-topFooterGap, max(topEventMinLines, len(candidates)))
	lines := model.infoLines()
	for index := 0; index < max(len(candidates), len(events)); index++ {
		var candidate, event string
		if index < len(candidates) {
			candidate = candidates[index]
		}
		if index < len(events) {
			event = events[index]
		}
		lines = append(lines, topDashboardFitWidth(candidate, topFooterProcessWidth)+strings.Repeat(" ", topFooterGap)+event)
	}
	if model.height > 0 {
		available := max(0, model.height-2-len(model.processBanner())-len(model.tableHeader())-len(model.statusLines()))
		lines = lines[:min(len(lines), available)]
	}
	for index, line := range lines {
		lines[index] = topDashboardFitWidth(line, model.displayWidth())
	}
	return lines
}

func (model topModel) infoLines() []string {
	var lines []string
	switch {
	case model.detail:
		lines = model.detailLines()
	case model.peaks:
		lines = model.peakLines()
	case model.view == topViewNetwork && model.previous.NetworkHealth != nil:
		if row, ok := model.selectedRow(); ok {
			lines = append([]string{"network " + row.at.Format("15:04:05") + " · current namespace · Enter settings"}, networkHealthLines(row.rate.NetworkHealth)...)
		}
	}
	if model.lastErr != nil {
		lines = append(lines, fmt.Sprintf("sample failed · last success %s · %s", model.previous.TakenAt.Format("15:04:05"), model.lastErr))
	}
	return lines
}

// footerLines는 표 아래 영역이다. 넓은 화면에서는 process 후보를 오른쪽에, 패널과 안내를 왼쪽에 둬 줄을 아낀다.
// 필터가 있으면 process 줄에 I/O와 limit 줄이 붙어 길어지므로 위아래로 쌓는다.
// footerLines는 표 아래 영역이다. 이벤트 칸이 없는 폭에서는 진행 중인 이벤트 한 줄을 맨 위에 둔다.
func (model topModel) footerLines() []string {
	if ticker := model.eventTicker(); ticker != "" {
		return append([]string{ticker}, model.footerPanelLines()...)
	}
	return model.footerPanelLines()
}

func (model topModel) footerPanelLines() []string {
	if model.displayWidth() < topFooterDockWidth || model.processFilter.active() {
		if model.eventsInFooter() {
			return append(model.panelLinesWithEvents(), model.statusLines()...)
		}
		return append(model.panelLines(), model.statusLines()...)
	}
	leftWidth := model.displayWidth() - topFooterGap - topFooterProcessWidth
	left, right := model, model
	left.width, right.width = leftWidth, topFooterProcessWidth
	info, status, candidates := left.infoLines(), left.statusLines(), right.candidateLines()
	if model.height > 0 {
		available := max(len(status), model.height-2-len(model.processBanner())-len(model.tableHeader()))
		info = info[:min(len(info), available-len(status))]
		candidates = candidates[:min(len(candidates), available)]
	}
	// 상세나 최고치 패널이 없으면 왼쪽 빈자리에 이벤트를 둔다. 줄 수는 오른쪽 process 목록에 맞춘다.
	if len(info) == 0 && model.eventsInFooter() {
		info = model.eventLines(leftWidth, max(topEventMinLines, len(candidates)-len(status)))
	}
	rows := max(len(info)+len(status), len(candidates))
	lines := make([]string, rows)
	for index := range lines {
		var leftLine, rightLine string
		if index < len(info) {
			leftLine = info[index]
		}
		// 안내는 왼쪽 칸의 맨 아래에 둬 화면 마지막 줄에 머문다.
		if offset := index - (rows - len(status)); offset >= 0 {
			leftLine = status[offset]
		}
		if index < len(candidates) {
			rightLine = candidates[index]
		}
		lines[index] = topDashboardFitWidth(leftLine, leftWidth) + strings.Repeat(" ", topFooterGap) + topDashboardFitWidth(rightLine, topFooterProcessWidth)
	}
	return lines
}

// padToFooter는 아래 영역이 화면 맨 아래에 오도록 빈 줄을 채운다. 시작 직후 행이 적어도 안내 줄이 움직이지 않는다.
func (model topModel) padToFooter(lines, footer []string) []string {
	for model.height > 0 && len(lines)+len(footer) < model.height {
		lines = append(lines, "")
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
	if model.displayWidth() < topTableWidth {
		return topDashboardFitWidth("edc top · "+string(model.view)+" · "+state, model.displayWidth())
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
	// "all latest"를 버전으로 읽는 일이 없게 보기 이름 앞에 view를 붙인다. 필터는 바로 아래 배너가 보인다.
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

// currentFilterRow는 지금 필터와 다른 필터로 수집한 행의 process 값을 비운다. 초점을 바꾸기 전 행이 새 필터의 값처럼 보이지 않는다.
func (model topModel) currentFilterRow(row topDashboardRow) topDashboardRow {
	if row.filter != model.processFilter.String() {
		row.processesValid = false
	}
	return row
}

func (model topModel) detailLines() []string {
	row, ok := model.selectedRow()
	row = model.currentFilterRow(row)
	if !ok {
		return []string{"detail · waiting for a sample"}
	}
	rate := row.rate
	if model.view == topViewNetwork {
		return append(append([]string{"network detail " + row.at.Format("15:04:05")}, networkHealthLines(rate.NetworkHealth)...), networkSettingLines(rate.NetworkHealth)...)
	}
	lines := []string{
		fmt.Sprintf("detail %s · load %.1f · cpu %.1f/%.1f%% · iowait %.1f%% · mem %.1f%%", row.at.Format("15:04:05"), rate.Load1, rate.CPUUser, rate.CPUSystem, rate.CPUIOWait, rate.MemoryPercent),
		fmt.Sprintf("  %s · %s · %s", topDiskDetail(rate), topNetworkDetail(rate), topPressureDetail(rate)),
	}
	if !model.processFilter.active() {
		return append(lines, "  "+topProcessDetail(row.processes, row.processesValid))
	}
	for index, line := range topMatchDetail(row.processes, row.processTotal, row.processesValid) {
		if index == 0 {
			line = "  " + line
		}
		lines = append(lines, line)
	}
	if row.processesValid {
		lines = append(lines, topProcessLimitLines(row.processes)...)
	}
	return lines
}

// peakLines는 지표마다 따로 최고치를 찾는다. 단위가 다른 값을 더해 한 행을 고르면 load 급등이 memory에 가려진다.
func (model topModel) peakLines() []string {
	if len(model.rows) == 0 {
		return []string{"peaks 60s · waiting for a sample"}
	}
	last := model.rows[len(model.rows)-1]
	if model.view == topViewNetwork && last.rate.NetworkHealth != nil {
		return model.networkPeakLines(last)
	}
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
	if model.record != nil {
		state = "SQLite · " + state
	}
	if model.paused {
		state = T("observe.top.paused") + " · " + state
	} else if !model.follow {
		state = fmt.Sprintf("history · %d new · %s", max(0, len(model.rows)-1-model.selected), state)
	}
	keys := "c cpu m mem d disk n net f candidates / filter ? help"
	if model.processFilter.active() {
		keys = "1 host f proc / edit Esc clear ? help"
	}
	views := fmt.Sprintf("%s · %s", state, keys)
	switch {
	case model.input:
		views = "process filter: " + model.inputText + "█ · Enter apply · Esc cancel"
	case model.notice != "":
		views = model.notice
	}
	// PgUp/Dn을 넣어도 80열에서 끝의 +/-가 잘리지 않게 구분 공백을 두 칸으로 맞췄다.
	actions := "↑↓ history  Tab processes  Enter detail  End live  ? help  q quit"
	if model.processFocus {
		actions = "↑↓ select  Enter focus PID  Tab/Esc history  End live  ? help  q quit"
	}
	if model.displayWidth() < 70 {
		views = state + " · / filter · ? help · q quit"
		actions = "Tab processes · ↑↓ history · End live"
		if model.processFocus {
			actions = "↑↓ select · Enter focus · Esc back"
		}
		if model.input {
			views = "filter: " + model.inputText + "█"
			actions = "Enter apply · Esc cancel"
		} else if model.notice != "" {
			views = model.notice
		}
	}
	if model.displayWidth() < 40 && !model.input && model.notice == "" {
		views = model.interval.String() + " · ? help · q quit"
		actions = "↑↓ history · Tab procs"
		if model.processFocus {
			actions = "↑↓ pick · Enter · Esc"
		}
	}
	// 두 줄이 한 폭에 들어가면 합쳐 표가 한 줄 더 보이게 한다. 양쪽에 있는 ? help는 한 번만 둔다.
	if joined := strings.TrimSuffix(views, " ? help") + "  ·  " + actions; ansi.StringWidth(joined) <= model.displayWidth() {
		return []string{liveMuted(topDashboardFitWidth(joined, model.displayWidth()), model.limits.color)}
	}
	// 폭을 먼저 맞춘다. escape가 rune 수에 들어가면 잘리는 위치가 어긋난다.
	return []string{liveMuted(topDashboardFitWidth(views, model.displayWidth()), model.limits.color), liveMuted(topDashboardFitWidth(actions, model.displayWidth()), model.limits.color)}
}

func (model topModel) displayWidth() int {
	if model.width <= 0 {
		return topTableWidth
	}
	return model.width
}

func (model topModel) candidates() []topProcess {
	row, ok := model.selectedRow()
	row = model.currentFilterRow(row)
	if !ok || !row.processesValid {
		return nil
	}
	processes := append([]topProcess(nil), row.processes...)
	switch model.view {
	case topViewMemory:
		sort.SliceStable(processes, func(i, j int) bool { return processes[i].RSS > processes[j].RSS })
	case topViewDisk:
		// 디스크 대기는 CPU를 쓰지 않으므로 I/O가 큰 process를 앞에 둔다. I/O를 읽지 못했거나 같으면
		// I/O를 기다리며 멈춘 process를 CPU 순위보다 앞에 둔다.
		sort.SliceStable(processes, func(i, j int) bool {
			if before, after := topProcessIOTotal(processes[i]), topProcessIOTotal(processes[j]); before != after {
				return before > after
			}
			return processes[i].State == topProcessStateBlocked && processes[j].State != topProcessStateBlocked
		})
	}
	return processes[:min(topProcessLimit, len(processes))]
}

// showsProcessIO는 후보 줄 끝에 읽기·쓰기 rate를 붙일지다. 필터를 걸었거나 디스크 보기이면 읽지 못한 값도 —로 보이고,
// 그 밖의 보기에서는 값을 구한 process가 있을 때만 붙인다.
func (model topModel) showsProcessIO(processes []topProcess) bool {
	if model.displayWidth() < topProcessIOMinWidth {
		return false
	}
	if model.processFilter.active() || model.view == topViewDisk {
		return true
	}
	for _, process := range processes {
		if process.DiskValid {
			return true
		}
	}
	return false
}

// topProcessStateNames는 /proc/<pid>/stat의 state 문자를 읽을 수 있는 이름으로 바꾼다. D는 커널이 disk sleep이라 부르지만
// 디스크 외의 I/O에서도 생기므로 iowait로 쓴다.
var topProcessStateNames = map[string]string{
	"R": "run", "S": "sleep", "D": "iowait", "Z": "zombie", "T": "stop", "t": "trace", "I": "idle", "X": "dead", "P": "park",
}

// topProcessStateName은 STATE 칸의 이름이다. 표에 없는 문자는 그대로 보이고, 모르면 빈 칸이다.
func topProcessStateName(state string) string {
	if name, ok := topProcessStateNames[state]; ok {
		return name
	}
	return topPrintableText(state)
}

// topProcessIOCell은 READ·WRITE 한 칸이다. 폭을 먼저 맞춘 뒤 색을 입혀 열이 어긋나지 않는다.
func topProcessIOCell(valid bool, value float64, color bool) string {
	text := topFitCell(topOptionalRate(valid, value), 6, false)
	if !valid {
		return text
	}
	return topPaint(text, topProcessIOThreshold.level(value), color)
}

// topProcessGroupDetail은 묶음 이름 뒤 괄호에 넣는 process 수와 I/O를 기다리는 수다. 이름에 수를 붙여 쓰면
// gm×163처럼 다른 process 이름으로 읽혀 괄호 안에 단어와 함께 적는다.
func topProcessGroupDetail(group topProcessGroup) string {
	detail := fmt.Sprintf("%d procs", group.Count)
	if group.Blocked > 0 {
		detail += fmt.Sprintf(", %d %s", group.Blocked, topProcessStateName(topProcessStateBlocked))
	}
	return detail
}

// topProcessGroupLabel은 후보 목록의 묶음 이름 칸이다. 폭이 모자라면 I/O 대기 수를 빼고, 그래도 모자라면 이름을 줄인다.
func topProcessGroupLabel(group topProcessGroup, width int) string {
	name := topProcessName(group.Name, width)
	count := fmt.Sprintf(" (%d procs)", group.Count)
	for _, suffix := range []string{" (" + topProcessGroupDetail(group) + ")", count} {
		if ansi.StringWidth(name+suffix) <= width {
			return name + suffix
		}
	}
	return topProcessName(group.Name, max(1, width-len(count))) + count
}

// topProcessGroupLine은 묶음 한 줄이다. 고를 수 없는 줄이라 PID 칸과 표시를 비운다.
// CPU 합이 process 경고 기준을 넘으면 그 칸을 위험 색으로 칠한다.
func topProcessGroupLine(group topProcessGroup, nameWidth int, showIO, color bool) string {
	name := topProcessGroupLabel(group, nameWidth)
	cpu := fmt.Sprintf("%6.1f%%", group.CPU)
	if group.CPU >= topProcessSignalCPU {
		cpu = topPaint(cpu, topLevelDanger, color)
	}
	line := fmt.Sprintf("  %7s %s %s %s %6s", "", topFitCell(name, nameWidth, true), strings.Repeat(" ", topProcessStateWidth), cpu, formatProcessGroupRSS(group.RSS))
	if showIO {
		line += " " + topProcessIOCell(group.DiskValid, group.DiskRead, color) + " " + topProcessIOCell(group.DiskValid, group.DiskWrite, color)
	}
	return line
}

// topViewProcessGroups는 보기의 순위 기준에 맞는 묶음이다. 디스크는 I/O, 메모리는 RSS, 나머지는 CPU 합 순이다.
func topViewProcessGroups(groups []topProcessGroup, view topView) []topProcessGroup {
	switch view {
	case topViewDisk:
		return topProcessGroupsByIO(groups)
	case topViewMemory:
		return topProcessGroupsByRSS(groups)
	}
	return topProcessGroupsByCPU(groups)
}

// formatProcessGroupRSS는 묶음의 RSS 합이다. 공유 page를 process마다 세므로 실제 사용량의 상한이라 ≤를 붙인다.
func formatProcessGroupRSS(bytes uint64) string {
	const mib = 1024 * 1024
	switch {
	case bytes >= 1024*mib:
		return fmt.Sprintf("≤%.1fG", float64(bytes)/(1024*mib))
	case bytes >= 10*mib:
		return fmt.Sprintf("≤%.0fM", float64(bytes)/mib)
	}
	return fmt.Sprintf("≤%.1fM", float64(bytes)/mib)
}

func (model topModel) candidateLines() []string {
	row, ok := model.selectedRow()
	row = model.currentFilterRow(row)
	if !ok || !row.processesValid {
		return []string{"processes · waiting for a sample with this filter"}
	}
	rank := "CPU"
	if model.view == topViewMemory {
		rank = "RSS"
	}
	state := "live"
	if !model.follow {
		state = "history"
	}
	title := rank
	if model.view == topViewDisk {
		title = "I/O"
	}
	lines := []string{fmt.Sprintf("processes · %s rank · %s %s · Tab select", title, state, row.at.Format("15:04:05"))}
	processes := model.candidates()
	if len(processes) == 0 {
		return append(lines, "no match · / edit filter · Esc clear")
	}
	showIO := model.showsProcessIO(processes)
	nameWidth := max(4, min(28, model.displayWidth()-topProcessColumnsWidth-10))
	if showIO {
		nameWidth = max(4, min(28, model.displayWidth()-topProcessColumnsWidth-topProcessIOColumnsWidth))
	}
	if model.displayWidth() < 40 {
		nameWidth = max(3, model.displayWidth()-18)
	}
	room := model.height - 2 - len(model.processBanner()) - len(model.tableHeader()) - len(model.statusLines())
	if room >= 3 {
		// PID는 Linux에서 7자리(기본 pid_max 4194304)까지 가므로 일곱 칸을 둔다.
		header := fmt.Sprintf("  %7s %s %s %7s %6s", "PID", topFitCell("COMMAND", nameWidth, true), topFitCell("STATE", topProcessStateWidth, true), "CPU%", "RSS")
		if showIO {
			header += fmt.Sprintf(" %6s %6s", "READ", "WRITE")
		}
		if model.displayWidth() < 40 {
			header = fmt.Sprintf("  %7s %s %6s", "PID", topFitCell("COMMAND", nameWidth, true), rank)
		}
		lines = append(lines, header)
	}
	if model.displayWidth() >= 40 && !model.processFilter.active() {
		groups := topViewProcessGroups(row.processTotal.Groups, model.view)
		// 묶음이 process 줄을 모두 밀어내지 않도록 process 한 줄은 남긴다.
		groups = groups[:min(len(groups), max(0, room-len(lines)-1))]
		for _, group := range groups {
			lines = append(lines, topProcessGroupLine(group, nameWidth, showIO, model.limits.color))
		}
	}
	count := min(3, len(processes))
	if model.processFocus || model.height >= topTallHeight {
		count = min(topProcessLimit, len(processes))
	}
	count = min(count, max(1, room-len(lines)))
	start := 0
	if model.processFocus {
		start = max(0, model.processSelected-count+1)
	}
	for index := start; index < min(start+count, len(processes)); index++ {
		process := processes[index]
		selected := model.processFocus && index == model.processSelected
		marker := " "
		if selected {
			marker = ">"
		}
		// 고른 줄은 줄 전체를 반전하므로 칸마다 색을 넣으면 중간의 reset이 반전을 끊는다.
		color := model.limits.color && !selected
		state := topFitCell(topProcessStateName(process.State), topProcessStateWidth, true)
		if process.State == topProcessStateBlocked {
			state = topPaint(state, topLevelWarn, color)
		}
		line := fmt.Sprintf("%s %7d %s %s %6.1f%% %6s", marker, process.PID, topFitCell(topProcessName(process.Command, nameWidth), nameWidth, true), state, process.CPU, formatProcessRSS(process.RSS))
		if model.displayWidth() < 40 {
			value := fmt.Sprintf("%.1f%%", process.CPU)
			if model.view == topViewMemory {
				value = formatProcessRSS(process.RSS)
			}
			line = fmt.Sprintf("%s %7d %s %6s", marker, process.PID, topFitCell(topProcessName(process.Command, nameWidth), nameWidth, true), value)
		}
		if showIO {
			line += " " + topProcessIOCell(process.DiskValid, process.DiskRead, color) + " " + topProcessIOCell(process.DiskValid, process.DiskWrite, color)
		}
		if selected && model.limits.color {
			line = topBannerStyle + topDashboardFitWidth(line, model.displayWidth()) + topColorReset
		}
		lines = append(lines, line)
	}
	if model.processFocus || model.processFilter.active() {
		process := processes[min(model.processSelected, len(processes)-1)]
		if process.DiskStatus != "" {
			lines = append(lines, "I/O · "+process.DiskStatus)
		}
		if model.processFilter.active() && !model.detail {
			lines = append(lines, topProcessLimitLines([]topProcess{process})...)
		}
	}
	return lines
}

func (model topModel) helpLines() []string {
	lines := []string{
		"Views: 1 all · c CPU · m memory · d disk · n network · s pressure · v boxes",
		"History: ↑↓ or PgUp/PgDn · End live · p pause · +/- interval",
		"Processes: Tab select · ↑↓ choose · Enter focus PID · Tab/Esc back",
		"Signals: f opens its view, then selects candidates. A candidate is not a confirmed cause.",
		"Events: warnings that last 5s are kept with their candidates · e list · Enter jump · [ ] previous/next · ! marks the start row",
		"Events that start within 5s share one line. ≤ means the warning was already on when edc started.",
		"Filter: / edit names or PIDs · Enter apply · Esc cancel or clear",
		"Details: Enter on history · h peaks from the last 60 seconds",
		"CPU: host % uses all cores. Process 100% uses one core. RSS is resident memory.",
		"— means unavailable or no baseline. Read the process I/O status for errors.",
		"no ev means the -d observer is active but collected no events in that interval. n/a means this OS cannot measure it.",
	}
	if model.details.System == "darwin" {
		lines = append(lines, "macOS: process CPU is a recent ps average. Threads and disk I/O use libproc. Process state iowait is the U state.",
			"macOS: -d gives the average CPU wait per context switch from libproc. It has no p95 and no I/O latency.",
			"macOS: root in the runq column means macOS refused to read another user's process. Run as root to measure it.",
			"macOS: mem lvl is the kernel memory pressure level. blocked counts processes in the U state.",
			"macOS: FDs, PSI, CPU iowait, disk busy and network limits are not collected.")
	} else {
		lines = append(lines, "Linux: process CPU uses sample deltas. Runq and I/O latency require -d at startup.")
	}
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(line, model.displayWidth(), ""), "\n")...)
	}
	return wrapped
}

func (model topModel) tableColumns() ([]topColumn, []int) {
	columns := topViewColumns(model.view)
	var kept []topColumn
	var indexes []int
	used := topSelectionColumn + 2
	dropped := false
	for index, column := range columns {
		if model.view == topViewProcess && model.displayWidth() < 56 && column.title == "thr" {
			continue
		}
		if !column.host.shown(model.details.System) {
			continue
		}
		if model.boxed && model.splitHidden(column.title) {
			continue
		}
		if cores := len(model.previous.Cores); column.title == "cores" && cores > 0 {
			// 막대는 core마다 한 칸이다. details.Cores는 CPU affinity를 따라 /proc/stat의 core 수보다 작을 수 있어 막대가 그리는 수를 쓴다.
			column.width = min(topCoreBarLimit, max(len(column.title), cores))
		}
		if model.view == topViewProcess && !model.probeEnabled && (column.title == "runq ms" || column.title == "io ms") {
			continue
		}
		if column.optional && (dropped || model.compact) {
			continue
		}
		needed, reserve := column.width+1, 0
		if column.optional && !model.boxed {
			reserve = topSignalMinWidth
		}
		if column.width > 0 && used+needed+reserve > model.displayWidth() {
			dropped = dropped || !column.optional
			continue
		}
		kept, indexes = append(kept, column), append(indexes, index)
		used += needed
	}
	return kept, indexes
}

func (model topModel) tableHeader() []string {
	if model.view == topViewAll && model.displayWidth() >= topTableWidth {
		columns, signalWidth := model.hostLayout()
		titles := make([]topCell, len(columns))
		for index, column := range columns {
			titles[index] = topPlainCell(column.title)
		}
		return []string{formatTopAllGroupHeader(columns, signalWidth), formatTopAllLine("", columns, titles, "", signalWidth, false)}
	}
	if model.view == topViewAll {
		return []string{model.compactRow(topDashboardRow{}, true)}
	}
	columns, _ := model.tableColumns()
	titles := make([]topCell, len(columns))
	for index, column := range columns {
		titles[index] = topPlainCell(column.title)
	}
	return []string{formatTopColumnsWidth("time", columns, titles, false, model.displayWidth())}
}

func (model topModel) tableRow(row topDashboardRow) string {
	if model.view == topViewAll {
		if model.displayWidth() < topTableWidth {
			return model.compactRow(row, false)
		}
		columns, signalWidth := model.hostLayout()
		cells := make([]topCell, len(columns))
		for index, column := range columns {
			cells[index] = topPlainCell(column.cell(row.rate))
			if column.level != nil {
				cells[index].level = column.level(model.limits, row.rate)
			}
		}
		signals := topDashboardSignalItems(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits)
		return formatTopAllLine(row.at.Format("15:04:05"), columns, cells, formatTopSignalsWidth(signals, signalWidth), signalWidth, model.limits.color)
	}
	columns, indexes := model.tableColumns()
	signal := topDashboardSignal(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits)
	cells := topViewCells(row.rate, model.view, signal, model.limits)
	if model.view == topViewProcess {
		cells = topProcessViewCells(row)
	}
	kept := make([]topCell, len(indexes))
	for index, source := range indexes {
		kept[index] = cells[source]
	}
	return formatTopColumnsWidth(row.at.Format("15:04:05"), columns, kept, model.limits.color, model.displayWidth())
}

func (model topModel) hostLayout() ([]topAllColumn, int) {
	columns, signalWidth := topAllLayout(model.displayWidth())
	if model.details.System != "darwin" {
		return columns, signalWidth
	}
	kept := make([]topAllColumn, 0, len(columns))
	for _, column := range columns {
		if column.host.shown(model.details.System) {
			kept = append(kept, column)
		}
	}
	spare := max(0, model.displayWidth()-topAllLineWidth(kept)-signalWidth)
	for index := range kept {
		extra := spare*(index+1)/len(kept) - spare*index/len(kept)
		kept[index].width += extra
		kept[index].indent += extra
	}
	return kept, signalWidth
}

func (model topModel) compactRow(row topDashboardRow, header bool) string {
	columns := []topColumn{{title: "cpu%", width: 5}, {title: "mem%", width: 5}, {title: "load", width: 5}, {title: "signal", left: true}}
	cells := []topCell{topValueCell("%.1f", row.rate.CPUUser+row.rate.CPUSystem, model.limits.cpu), topValueCell("%.1f", row.rate.MemoryPercent, model.limits.memory), topValueCell("%.1f", row.rate.Load1, model.limits.load), topPlainCell(topDashboardSignal(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits))}
	if model.displayWidth() < 32 {
		columns = append(columns[:2], columns[3])
		cells = append(cells[:2], cells[3])
	}
	at := row.at.Format("15:04:05")
	if header {
		at = "time"
		for index, column := range columns {
			cells[index] = topPlainCell(column.title)
		}
	}
	return formatTopColumnsWidth(at, columns, cells, model.limits.color && !header, model.displayWidth())
}

// topColumnHost는 열의 값을 주는 host다. 값을 주지 않는 host에서는 열을 숨긴다.
type topColumnHost uint8

const (
	topColumnAnyHost topColumnHost = iota
	topColumnLinuxOnly
	topColumnDarwinOnly
)

func (host topColumnHost) shown(system string) bool {
	switch host {
	case topColumnLinuxOnly:
		return system != "darwin"
	case topColumnDarwinOnly:
		return system == "darwin"
	}
	return true
}

// topColumn은 보기별 표의 한 칸이다. 헤더와 행이 같은 정의로 그려져 구분선이 어긋나지 않는다.
// width가 0인 마지막 칸은 남은 폭을 모두 쓴다.
type topColumn struct {
	title string
	width int
	left  bool
	host  topColumnHost
	// optional인 칸은 기존 칸이 모두 들어가고 signal 칸이 topSignalMinWidth를 지킬 때만 넣는다.
	// 좁은 화면에서 기존 칸과 signal을 그대로 두려고 뒤에 붙인 칸이다.
	optional bool
}

func topViewColumns(view topView) []topColumn {
	signal := topColumn{title: "signal", left: true}
	switch view {
	case topViewCPU:
		return []topColumn{{title: "load", width: 5}, {title: "usr%", width: 5}, {title: "sys%", width: 5}, {title: "io%", width: 5, host: topColumnLinuxOnly}, {title: "hot core", width: 8, left: true}, {title: "cores", width: topCoreBarLimit, left: true}, {title: "steal%", width: 6, optional: true, host: topColumnLinuxOnly}, {title: "blocked", width: 7, optional: true}, signal}
	case topViewMemory:
		return []topColumn{{title: "mem%", width: 6}, {title: "swap/s", width: 7}, {title: "psi mem", width: 7, host: topColumnLinuxOnly}, {title: "mem lvl", width: 8, host: topColumnDarwinOnly}, {title: "load", width: 6}, signal}
	case topViewDisk:
		return []topColumn{{title: "read/s", width: 7}, {title: "write/s", width: 7}, {title: "iops", width: 6}, {title: "await", width: 6}, {title: "busy%", width: 6, host: topColumnLinuxOnly}, {title: "queue", width: 6, optional: true, host: topColumnLinuxOnly}, signal}
	case topViewNetwork:
		return []topColumn{{title: "in/s", width: 7}, {title: "out/s", width: 7}, {title: "ct%", width: 6, host: topColumnLinuxOnly}, {title: "listen/s", width: 8, host: topColumnLinuxOnly}, {title: "err/s", width: 6}, {title: "drop/s", width: 6}, {title: "soft/s", width: 6, host: topColumnLinuxOnly}, {title: "pk_in", width: 6}, {title: "pk_out", width: 6}, {title: "retr/s", width: 7, optional: true, host: topColumnLinuxOnly}, {title: "rst/s", width: 6, optional: true, host: topColumnLinuxOnly}, {title: "fail/s", width: 6, optional: true, host: topColumnLinuxOnly}, signal}
	case topViewPressure:
		return []topColumn{{title: "cpu psi", width: 7, host: topColumnLinuxOnly}, {title: "mem psi", width: 7, host: topColumnLinuxOnly}, {title: "io psi", width: 7, host: topColumnLinuxOnly}, {title: "mem full", width: 8, host: topColumnLinuxOnly}, {title: "io full", width: 7, host: topColumnLinuxOnly}, {title: "mem lvl", width: 8, host: topColumnDarwinOnly}, {title: "blocked", width: 7}, {title: "load", width: 6}, {title: "mem%", width: 6}, signal}
	case topViewProcess:
		// cpu%는 core 하나를 100으로 센다. runq와 io는 --ebpf가 있을 때의 평균 대기와 지연이다. 가장 바쁜 process는 상세 패널에 있다.
		return []topColumn{{title: "match", width: 5}, {title: "cpu%", width: 6}, {title: "rss", width: 6}, {title: "thr", width: 5}, {title: "fds", width: 5, host: topColumnLinuxOnly}, {title: "read/s", width: 7}, {title: "write/s", width: 7}, {title: "runq ms", width: 7}, {title: "io ms"}}
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
		return []topCell{topValueCell("%.1f", rate.Load1, limits.load), topValueCell("%.1f", rate.CPUUser, limits.cpu), topValueCell("%.1f", rate.CPUSystem, limits.cpu), topValueCell("%.1f", rate.CPUIOWait, limits.io), {text: topHotCore(rate.CoreCPU), level: topHotCoreLevel(rate.CoreCPU)}, topPlainCell(topCoreBar(rate.CoreCPU)), topPlainCell(topOptionalValue(rate.CPUStealValid, "%.1f", rate.CPUSteal)), topPlainCell(topOptionalValue(rate.ProcsBlockedSource.known(), "%.0f", rate.ProcsBlocked)), topPlainCell(signal)}
	case topViewMemory:
		return []topCell{topValueCell("%.1f", rate.MemoryPercent, limits.memory), topPlainCell(formatRate(rate.SwapOut)), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIMemory, limits.psi), topMemoryPressureCell(rate), topValueCell("%.1f", rate.Load1, limits.load), topPlainCell(signal)}
	case topViewDisk:
		return []topCell{topPlainCell(formatRate(rate.DiskRead)), topPlainCell(formatRate(rate.DiskWrite)), topPlainCell(topOptionalValue(rate.DiskHealthValid, "%.0f", rate.DiskIOPS)), topOptionalCell(rate.DiskHealthValid, "%.1f", rate.DiskAwait, limits.await), topPlainCell(topOptionalValue(rate.DiskBusyValid, "%.0f", rate.DiskBusy)), topPlainCell(topOptionalValue(rate.DiskBusyValid, "%.1f", rate.DiskQueue)), topPlainCell(signal)}
	case topViewNetwork:
		return []topCell{topPlainCell(formatRate(rate.NetIn)), topPlainCell(formatRate(rate.NetOut)), networkConntrackCell(rate.NetworkHealth), topPlainCell(networkRateText(rate.NetworkHealth, "listen_overflows")), topOptionalCell(rate.NetHealthValid, "%.0f", rate.NetErrors, limits.network), topOptionalCell(rate.NetHealthValid, "%.0f", rate.NetDrops, limits.network), topPlainCell(networkRateText(rate.NetworkHealth, "softnet_dropped")), topPlainCell(fmt.Sprintf("%.0f", rate.PacketsIn)), topPlainCell(fmt.Sprintf("%.0f", rate.PacketsOut)), topPlainCell(topNetworkRateCell(rate.NetworkHealth, "tcp_retrans_segs", 7)), topPlainCell(topNetworkRateCell(rate.NetworkHealth, "tcp_out_rsts", 6)), topPlainCell(topNetworkRateCell(rate.NetworkHealth, "tcp_attempt_fails", 6)), topPlainCell(signal)}
	case topViewPressure:
		return []topCell{topOptionalCell(rate.PSIValid, "%.1f", rate.PSICPU, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIMemory, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIIO, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIMemoryFull, limits.psi), topOptionalCell(rate.PSIValid, "%.1f", rate.PSIIOFull, limits.psi), topMemoryPressureCell(rate), topPlainCell(topOptionalValue(rate.ProcsBlockedSource.known(), "%.0f", rate.ProcsBlocked)), topValueCell("%.1f", rate.Load1, limits.load), topValueCell("%.1f", rate.MemoryPercent, limits.memory), topPlainCell(signal)}
	}
	return nil
}

func topMemoryPressureCell(rate resourceRate) topCell {
	if !rate.MemoryPressure.known() {
		return topPlainCell("—")
	}
	return topCell{text: rate.MemoryPressure.String(), level: rate.MemoryPressure.level()}
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
	return formatTopColumnsWidth(at, columns, cells, color, topTableWidth)
}

func formatTopColumnsWidth(at string, columns []topColumn, cells []topCell, color bool, targetWidth int) string {
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
			width = max(0, targetWidth-used)
		}
		used += width
		line.WriteString(topPaint(topFitCell(cells[index].text, width, column.left), cells[index].level, color))
	}
	return line.String()
}

// topFitCell은 칸 하나를 폭에 맞춘다. 색이 붙기 전이라 rune 단위로 잘라도 escape가 끊기지 않는다.
func topFitCell(text string, width int, left bool) string {
	text = ansi.Truncate(text, width, "")
	gap := strings.Repeat(" ", max(0, width-ansi.StringWidth(text)))
	if left {
		return text + gap
	}
	return gap + text
}

// topAllColumn은 all 보기의 한 칸이다. tier 0은 항상 보이고, 나머지는 terminal이 넓어질수록 tier 순서대로 추가된다.
// 순서는 진단에 쓸모가 큰 값부터다: hot core, disk iops·await, packet, network err·drop, disk busy,
// pressure, swap out, listen overflow·softnet drop, conntrack 사용률.
type topAllColumn struct {
	group string
	title string
	host  topColumnHost
	width int
	tier  int
	left  bool
	// indent는 topAllLayout이 넓힌 폭을 칸 앞에 두는 공백이다. 왼쪽 정렬 칸도 앞 칸과의 간격이 다른 칸만큼 벌어진다.
	indent int
	cell   func(rate resourceRate) string
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
	{group: "network", title: "listen", host: topColumnLinuxOnly, width: 6, tier: 8, cell: func(rate resourceRate) string { return topNetworkRateCell(rate.NetworkHealth, "listen_overflows", 6) }},
	{group: "network", title: "soft", host: topColumnLinuxOnly, width: 4, tier: 8, cell: func(rate resourceRate) string { return topNetworkRateCell(rate.NetworkHealth, "softnet_dropped", 4) }},
	{group: "network", title: "ct%", host: topColumnLinuxOnly, width: 5, tier: 9, cell: func(rate resourceRate) string { return networkConntrackCell(rate.NetworkHealth).text },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return networkConntrackCell(rate.NetworkHealth).level
		}},
	{group: "cpu", title: "load", width: 4, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.Load1) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.load.level(rate.Load1) }},
	{group: "cpu", title: "usr%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUUser) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.cpu.level(rate.CPUUser) }},
	{group: "cpu", title: "sys%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUSystem) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.cpu.level(rate.CPUSystem) }},
	{group: "cpu", title: "i/o", host: topColumnLinuxOnly, width: 4, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.CPUIOWait) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.io.level(rate.CPUIOWait) }},
	{group: "cpu", title: "hot core", width: 8, tier: 1, left: true, cell: func(rate resourceRate) string { return topHotCore(rate.CoreCPU) },
		level: func(limits topLimits, rate resourceRate) topLevel { return topHotCoreLevel(rate.CoreCPU) }},
	{group: "mem", title: "mem%", width: 5, cell: func(rate resourceRate) string { return fmt.Sprintf("%.1f", rate.MemoryPercent) },
		level: func(limits topLimits, rate resourceRate) topLevel { return limits.memory.level(rate.MemoryPercent) }},
	{group: "mem", title: "swap", width: 5, tier: 7, cell: func(rate resourceRate) string { return formatRate(rate.SwapOut) }},
	{group: "disk", title: "read", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.DiskRead) }},
	{group: "disk", title: "write", width: 5, cell: func(rate resourceRate) string { return formatRate(rate.DiskWrite) }},
	{group: "disk", title: "iops", width: 5, tier: 2, cell: func(rate resourceRate) string { return topOptionalCount(rate.DiskHealthValid, rate.DiskIOPS, 5) }},
	{group: "disk", title: "await", width: 5, tier: 2, cell: func(rate resourceRate) string { return topOptionalValue(rate.DiskHealthValid, "%.1f", rate.DiskAwait) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.DiskHealthValid, limits.await, rate.DiskAwait)
		}},
	{group: "disk", title: "busy", host: topColumnLinuxOnly, width: 4, tier: 5, cell: func(rate resourceRate) string { return topOptionalValue(rate.DiskBusyValid, "%.0f", rate.DiskBusy) }},
	{group: "psi", title: "cpu", host: topColumnLinuxOnly, width: 5, tier: 6, cell: func(rate resourceRate) string { return topOptionalValue(rate.PSIValid, "%.1f", rate.PSICPU) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.PSIValid, limits.psi, rate.PSICPU)
		}},
	{group: "psi", title: "mem", host: topColumnLinuxOnly, width: 5, tier: 6, cell: func(rate resourceRate) string { return topOptionalValue(rate.PSIValid, "%.1f", rate.PSIMemory) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.PSIValid, limits.psi, rate.PSIMemory)
		}},
	{group: "psi", title: "io", host: topColumnLinuxOnly, width: 5, tier: 6, cell: func(rate resourceRate) string { return topOptionalValue(rate.PSIValid, "%.1f", rate.PSIIO) },
		level: func(limits topLimits, rate resourceRate) topLevel {
			return topValidLevel(rate.PSIValid, limits.psi, rate.PSIIO)
		}},
}

// topAllLayout은 width 안에 signal 최소 폭까지 들어가는 가장 높은 tier의 칸을 고른다.
// signal은 대개 "-" 한 글자라서 남는 폭을 signal에 주면 넓은 terminal에서 표 오른쪽이 비어 보인다.
// 그래서 signal에는 topSignalWideWidth만 남기고 나머지는 칸에 나눈다. 나누고 남은 폭도 칸 사이에 1칸씩 흩어서
// 칸 폭 차이가 1을 넘지 않으면서 표가 오른쪽 끝까지 찬다.
func topAllLayout(width int) ([]topAllColumn, int) {
	chosen := topAllColumnsUpTo(0)
	for tier := 1; tier <= topAllMaxTier(); tier++ {
		candidate := topAllColumnsUpTo(tier)
		if topAllLineWidth(candidate)+topSignalMinWidth > width {
			break
		}
		chosen = candidate
	}
	spare := max(0, width-topAllLineWidth(chosen)-topSignalWideWidth)
	for index := range chosen {
		extra := spare*(index+1)/len(chosen) - spare*index/len(chosen)
		chosen[index].width += extra
		chosen[index].indent = extra
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
		line.WriteString(strings.Repeat(" ", column.indent))
		line.WriteString(topPaint(topFitCell(cells[index].text, column.width-column.indent, column.left), cells[index].level, color))
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
	if view == topViewProcess {
		return formatTopColumns(at, topViewColumns(view), topProcessViewCells(row), limits.color)
	}
	if view != topViewAll {
		signal := topDashboardSignal(rate, row.processes, row.processTotal.Groups, row.processesValid, limits)
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
	signals := topDashboardSignalItems(rate, row.processes, row.processTotal.Groups, row.processesValid, limits)
	return formatTopAllLine(at, columns, cells, formatTopSignalsWidth(signals, signalWidth), signalWidth, limits.color)
}

// topSignalItem은 signal 후보다. score는 값을 danger 임계치로 나눈 값이라 단위가 다른 지표끼리 비교된다.
type topSignalItem struct {
	text  string
	score float64
	// view는 이 경고를 자세히 보이는 보기다. f가 그 보기로 간다. process 경고는 topViewProcess다.
	view topView
	// kind는 경고의 종류다. 값이 바뀌어도 같은 종류가 이어지면 한 이벤트로 합친다.
	kind string
}

func topDashboardSignal(rate resourceRate, processes []topProcess, groups []topProcessGroup, valid bool, limits topLimits) string {
	return formatTopSignals(topDashboardSignalItems(rate, processes, groups, valid, limits))
}

func topDashboardSignalItems(rate resourceRate, processes []topProcess, groups []topProcessGroup, valid bool, limits topLimits) []topSignalItem {
	signals := topSignals(rate, limits)
	if process, ok := topProcessSignal(processes, valid); ok {
		signals = append(signals, topSignalItem{text: process, view: topViewProcess, kind: "process " + strconv.Itoa(processes[0].PID)})
	}
	if valid {
		signals = append(signals, topProcessGroupSignals(groups)...)
	}
	return signals
}

// topProcessGroupSignals는 묶음 합이 process 하나의 경고 기준을 넘을 때 경고한다. 하나하나는 작아 process 경고에
// 오르지 않는 경우다. I/O 합은 디스크 보기에서만 구하므로 I/O 묶음 경고도 그때만 나온다.
func topProcessGroupSignals(groups []topProcessGroup) []topSignalItem {
	signals := []topSignalItem{}
	if ranked := topProcessGroupsByCPU(groups); len(ranked) > 0 && ranked[0].CPU >= topProcessSignalCPU {
		group := ranked[0]
		signals = append(signals, topSignalItem{text: fmt.Sprintf("%s (%d) %.0f%%", topProcessName(group.Name, topSignalProcessNameWidth), group.Count, group.CPU), view: topViewCPU, kind: "group cpu " + group.Name})
	}
	if ranked := topProcessGroupsByIO(groups); len(ranked) > 0 && ranked[0].DiskValid && ranked[0].DiskRead+ranked[0].DiskWrite >= topProcessIOThreshold.danger {
		group := ranked[0]
		io := group.DiskRead + group.DiskWrite
		signals = append(signals, topSignalItem{text: fmt.Sprintf("%s (%d) %s", topProcessName(group.Name, topSignalProcessNameWidth), group.Count, formatRate(io)), score: io / topProcessIOThreshold.danger, view: topViewDisk, kind: "group io " + group.Name})
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

// topFullCommandMinWidth는 전체 명령줄을 담을 최소 칸이다. 이보다 좁으면 실행 파일 이름만 보여 준다.
const topFullCommandMinWidth = 12

// topProcessName은 화면에 쓸 process 이름이다. macOS ps는 전체 경로를 주므로 마지막 요소만 남기고,
// 이름에 섞인 제어 문자가 terminal escape로 해석되지 않게 바꾼다.
func topProcessName(command string, width int) string {
	name := strings.TrimSpace(command)
	rest := ""
	if topFullCommand {
		if index := strings.IndexByte(name, ' '); index >= 0 {
			name, rest = name[:index], strings.TrimSpace(name[index+1:])
		}
	}
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	name = topPrintableText(name)
	if rest != "" && width >= topFullCommandMinWidth {
		name = topJoinCommandTail(name, topPrintableText(rest), width)
	}
	if runes := []rune(name); len(runes) > width {
		name = string(runes[:width])
	}
	if name == "" {
		return "proc"
	}
	return name
}

// topFilterSeed는 /로 연 필터 입력의 출발값이다. macOS의 comm은 경로째로 오고 전체 명령줄 모드는 인자까지
// 담아서, 그대로 넣으면 입력 줄의 커서와 안내가 폭에 밀려 사라진다. 실행 파일 이름만 쓰면 짧고, 부분 일치라
// 고른 process에 그대로 걸린다. 표시용 topProcessName과 달리 빈 이름을 proc으로 바꾸지 않는다. proc은
// 그 process에 걸리지 않는 값이다.
func topFilterSeed(command string) string {
	name := strings.TrimSpace(command)
	if topFullCommand {
		if index := strings.IndexByte(name, ' '); index >= 0 {
			name = name[:index]
		}
	}
	// login shell은 ps가 -/bin/zsh처럼 앞에 -를 붙여 준다. 경로는 표시와 같은 규칙으로 마지막 조각만 남기고,
	// kworker/0:1처럼 경로가 아닌 이름은 그대로 둔다.
	name = strings.TrimPrefix(name, "-")
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	return topPrintableText(name)
}

func topPrintableText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, text)
}

// topJoinCommandTail은 이름 뒤에 인자의 끝을 붙인다. 같은 실행 파일을 구분하는 부분이 보통 끝에 있어서,
// 칸이 좁으면 앞을 버리고 경로 구분자에 맞춰 …로 잇는다.
func topJoinCommandTail(name, rest string, width int) string {
	room := width - len([]rune(name)) - 1
	if room < 4 {
		return name
	}
	if runes := []rune(rest); len(runes) <= room {
		return name + " " + rest
	}
	budget := room - 1
	tail := ""
	// 앞에서부터 처음으로 들어가는 조각이 가장 긴 조각이다.
	for index, letter := range rest {
		if letter != '/' {
			continue
		}
		if candidate := rest[index:]; len([]rune(candidate)) <= budget {
			tail = candidate
			break
		}
	}
	if tail == "" {
		runes := []rune(rest)
		tail = string(runes[len(runes)-budget:])
	}
	return name + " …" + tail
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
	value = ansi.Truncate(value, width, "")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}

func topSignal(rate resourceRate, limits topLimits) string {
	return formatTopSignals(topSignals(rate, limits))
}

func topSignals(rate resourceRate, limits topLimits) []topSignalItem {
	all := []topSignalItem{}
	if rate.MemoryPercent >= limits.memory.warn {
		all = append(all, topSignalItem{fmt.Sprintf("mem %.0f%%", rate.MemoryPercent), rate.MemoryPercent / limits.memory.danger, topViewMemory, "mem"})
	}
	if rate.Load1 >= limits.load.warn {
		all = append(all, topSignalItem{fmt.Sprintf("load %.1f", rate.Load1), rate.Load1 / limits.load.danger, topViewCPU, "load"})
	}
	if rate.CPUIOWait >= limits.io.warn {
		all = append(all, topSignalItem{fmt.Sprintf("io %.1f%%", rate.CPUIOWait), rate.CPUIOWait / limits.io.danger, topViewDisk, "io"})
	}
	if topBlockedEvidence(rate, limits) && rate.ProcsBlocked >= topBlockedSignalMin {
		all = append(all, topSignalItem{fmt.Sprintf("blocked %.0f", rate.ProcsBlocked), rate.ProcsBlocked / topBlockedSignalDanger, topViewDisk, "blocked"})
	}
	if rate.CPUUser+rate.CPUSystem >= limits.cpu.warn {
		all = append(all, topSignalItem{fmt.Sprintf("cpu %.0f%%", rate.CPUUser+rate.CPUSystem), (rate.CPUUser + rate.CPUSystem) / limits.cpu.danger, topViewCPU, "cpu"})
	}
	if rate.DiskHealthValid && rate.DiskAwait >= limits.await.warn {
		all = append(all, topSignalItem{fmt.Sprintf("await %.0fms", rate.DiskAwait), rate.DiskAwait / limits.await.danger, topViewDisk, "await"})
	}
	if rate.NetHealthValid && rate.NetDrops >= limits.network.warn {
		all = append(all, topSignalItem{fmt.Sprintf("drop %.0f/s", rate.NetDrops), rate.NetDrops / limits.network.danger, topViewNetwork, "drop"})
	}
	if rate.NetHealthValid && rate.NetErrors >= limits.network.warn {
		all = append(all, topSignalItem{fmt.Sprintf("err %.0f/s", rate.NetErrors), rate.NetErrors / limits.network.danger, topViewNetwork, "err"})
	}
	if rate.MemoryPressure.level() != topLevelNormal {
		all = append(all, topSignalItem{"mem pressure " + rate.MemoryPressure.String(), rate.MemoryPressure.score(), topViewMemory, "mem pressure"})
	}
	if rate.PSIValid && rate.PSIIO >= limits.psi.warn {
		all = append(all, topSignalItem{fmt.Sprintf("psi io %.0f%%", rate.PSIIO), rate.PSIIO / limits.psi.danger, topViewPressure, "psi io"})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	return all
}

// topBlockedEvidence는 멈춘 작업 수 외의 두 번째 근거다. Linux는 iowait이다. iowait이 없는 macOS의 U 상태는 디스크
// 대기 말고 page-in 같은 VM 대기에서도 생기므로, 디스크 await 경고나 memory 압박이 함께 있을 때만 경고한다.
func topBlockedEvidence(rate resourceRate, limits topLimits) bool {
	switch rate.ProcsBlockedSource {
	case topBlockedKernelTasks:
		return rate.CPUIOWait >= limits.io.warn
	case topBlockedProcessList:
		return (rate.DiskHealthValid && rate.DiskAwait >= limits.await.warn) || rate.MemoryPressure.level() != topLevelNormal
	}
	return false
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

// topProcessDetail은 CPU 순으로 process 세 개를 보인다. 필터가 있으면 topMatchDetail로 넘어간다.
func topProcessDetail(processes []topProcess, valid bool) string {
	if !valid {
		return "processes —"
	}
	return "top " + strings.Join(topProcessItems(processes), ", ")
}

func topProcessItems(processes []topProcess) []string {
	items := make([]string, 0, min(3, len(processes)))
	for _, process := range processes[:min(3, len(processes))] {
		items = append(items, fmt.Sprintf("%s %.0f%% %s", topProcessName(process.Command, topProcessNameWidth), process.CPU, formatProcessRSS(process.RSS)))
	}
	return items
}

// topMatchDetail은 필터에 맞은 process 전체의 합 한 줄과, CPU 상위 세 개와 남은 개수 한 줄이다. 한 줄에 다 담으면 80열에서 잘린다.
// 디스크와 fd는 목록에 남은 process만 읽으므로 합도 그 범위다.
func topMatchDetail(processes []topProcess, total topProcessTotal, valid bool) []string {
	if !valid {
		return []string{"processes —"}
	}
	if total.Count == 0 {
		return []string{"match none"}
	}
	summary := fmt.Sprintf("match %d · cpu %.0f%% · rss %s", total.Count, total.CPU, formatProcessRSS(total.RSS))
	if total.Threads > 0 {
		summary += fmt.Sprintf(" · thr %d", total.Threads)
	}
	fds, read, write, diskKnown := topMatchIO(processes)
	if fds > 0 {
		summary += fmt.Sprintf(" · fds %d", fds)
	}
	if diskKnown {
		summary += fmt.Sprintf(" · io r %s/s w %s/s", formatRate(read), formatRate(write))
	}
	items := topProcessItems(processes)
	if rest := total.Count - len(items); rest > 0 {
		items = append(items, fmt.Sprintf("+%d", rest))
	}
	lines := []string{summary, "  " + strings.Join(items, ", ")}
	if total.Probe != nil {
		lines = append(lines, "  "+topProbeDetail(*total.Probe))
	}
	return lines
}

// topBannerStyle은 process 배너의 굵은 반전 표시다. 배너가 terminal 폭을 모두 채워 한 덩어리로 보인다.
const topBannerStyle = "\033[1;7m"

// processBanner는 --process에 맞은 process 묶음의 값을 제목 바로 아래에 크게 보인다. 선택한 행이 있으면 그 시점의 값이다.
// 폭이 모자라면 뒤의 항목부터 뺀다. -d로 얻는 대기와 지연을 디스크보다 앞에 둔다.
func (model topModel) processBanner() []string {
	if !model.processFilter.active() {
		return nil
	}
	width := model.displayWidth()
	var parts []string
	row, ok := model.selectedRow()
	row = model.currentFilterRow(row)
	switch {
	case !ok || !row.processesValid:
		parts = []string{"waiting for a sample"}
	case row.processTotal.Count == 0:
		parts = []string{"no match"}
	default:
		total := row.processTotal
		parts = []string{fmt.Sprintf("%d matched", total.Count), fmt.Sprintf("cpu %.0f%%", total.CPU), "rss " + formatProcessRSS(total.RSS)}
		if probe := total.Probe; probe != nil {
			if average, ok := topProbeAverageMS(probe.RunqSumNS, probe.RunqCount); ok {
				parts = append(parts, fmt.Sprintf("runq %.2fms", average))
			}
			if probe.IO != nil {
				if average, ok := topProbeAverageMS(probe.IO.SumNS, probe.IO.Count); ok {
					parts = append(parts, fmt.Sprintf("io %.2fms", average))
				}
			}
			if probe.Unreadable > 0 {
				parts = append(parts, fmt.Sprintf("%d need root", probe.Unreadable))
			}
		}
		fds, read, write, diskKnown := topMatchIO(row.processes)
		if diskKnown {
			parts = append(parts, fmt.Sprintf("disk r %s w %s", formatRate(read), formatRate(write)))
		}
		if total.Threads > 0 {
			parts = append(parts, fmt.Sprintf("thr %d", total.Threads))
		}
		if fds > 0 {
			parts = append(parts, fmt.Sprintf("fds %d", fds))
		}
	}
	line := " PROCESS " + model.processFilter.String()
	if model.focusName != "" {
		line = " PROCESS pid " + model.processFilter.String() + " " + model.focusName
	}
	stamp := ""
	if ok && row.processesValid {
		stamp = row.at.Format("15:04:05")
		if !model.follow {
			stamp = "history " + stamp
		}
	}
	contentWidth := width
	if stamp != "" {
		contentWidth = max(0, width-ansi.StringWidth(stamp)-3)
	}
	for _, part := range parts {
		next := line + " · " + part
		if ansi.StringWidth(next)+1 > contentWidth {
			break
		}
		line = next
	}
	if stamp != "" {
		line = ansi.Truncate(line, contentWidth, "") + " · " + stamp
	}
	line = topDashboardFitWidth(line, width)
	if model.limits.color {
		line = topBannerStyle + line + topColorReset
	}
	return []string{line}
}

// topMatchIO는 목록에 남은 process의 fd 수와 디스크 rate를 더한다. 이 값은 남은 process만 읽으므로 합도 그 범위다.
func topMatchIO(processes []topProcess) (fds int, read, write float64, diskKnown bool) {
	for _, process := range processes {
		fds += process.FDs
		if process.DiskValid {
			diskKnown = true
			read += process.DiskRead
			write += process.DiskWrite
		}
	}
	return fds, read, write, diskKnown
}

// topProcessViewCells는 process 보기의 한 행이다. 읽지 못한 값은 0 대신 —로 둔다.
func topProcessViewCells(row topDashboardRow) []topCell {
	empty := topPlainCell("—")
	if !row.processesValid {
		return []topCell{empty, empty, empty, empty, empty, empty, empty, empty, empty}
	}
	total := row.processTotal
	if total.Count == 0 {
		return []topCell{topPlainCell("0"), empty, empty, empty, empty, empty, empty, empty, empty}
	}
	fds, read, write, diskKnown := topMatchIO(row.processes)
	cells := []topCell{
		topPlainCell(fmt.Sprintf("%d", total.Count)), topPlainCell(fmt.Sprintf("%.1f", total.CPU)), topPlainCell(formatProcessRSS(total.RSS)),
		topPlainCell(topOptionalValue(total.Threads > 0, "%.0f", float64(total.Threads))), topPlainCell(topOptionalValue(fds > 0, "%.0f", float64(fds))),
		topPlainCell(topOptionalRate(diskKnown, read)), topPlainCell(topOptionalRate(diskKnown, write)), empty, empty,
	}
	if probe := total.Probe; probe != nil {
		cells[7], cells[8] = topPlainCell("no ev"), topPlainCell("no ev")
		if probe.unmeasured() {
			// 권한이 없어 하나도 읽지 못한 값이다. no ev로 두면 대기가 없었던 것으로 읽힌다.
			cells[7] = topPlainCell("root")
		} else if average, ok := topProbeAverageMS(probe.RunqSumNS, probe.RunqCount); ok {
			cells[7] = topPlainCell(fmt.Sprintf("%.2f", average))
		}
		if probe.IO == nil {
			cells[8] = topPlainCell("n/a")
		} else if average, ok := topProbeAverageMS(probe.IO.SumNS, probe.IO.Count); ok {
			cells[8] = topPlainCell(fmt.Sprintf("%.2f", average))
		}
	}
	return cells
}

func topOptionalRate(valid bool, value float64) string {
	if !valid {
		return "—"
	}
	return formatRate(value)
}

func topLimitStateText(status topLimitStatus) string {
	if status.Reason != "" {
		return status.Status + " · " + doctorGuidanceExcerpt(status.Reason)
	}
	return status.Status
}

func topProcessLimitLines(processes []topProcess) []string {
	var lines []string
	groups := map[string]bool{}
	for _, process := range processes {
		limits := processLimitsOf(process)
		fd := limits.FD
		used, soft := "—", "—"
		if fd.Used != nil {
			used = strconv.Itoa(*fd.Used)
		}
		if fd.Soft != nil {
			soft = strconv.FormatUint(*fd.Soft, 10)
		}
		if fd.Unlimited {
			soft = "unlimited"
		}
		line := fmt.Sprintf("pid %d · fd %s/%s", process.PID, used, soft)
		if fd.Status != "available" {
			line += " · " + topLimitStateText(fd.topLimitStatus)
		}
		lines = append(lines, line)
		group := limits.Cgroup
		key := group.Key
		if key == "" {
			key = group.Path + "|" + group.Status + "|" + group.Reason
		}
		if groups[key] {
			continue
		}
		groups[key] = true
		if group.Status != "available" {
			lines = append(lines, "cgroup · "+topLimitStateText(group.topLimitStatus))
			continue
		}
		lines = append(lines, "cgroup "+doctorGuidanceExcerpt(group.Path)+" · group scope")
		memory := group.Memory
		if memory.Status == "available" && memory.Used != nil && (memory.Max != nil || memory.Unlimited) {
			maximum := "unlimited"
			if memory.Max != nil {
				maximum = formatProcessRSS(*memory.Max)
			}
			lines = append(lines, fmt.Sprintf("  memory %s · local max %s", formatProcessRSS(*memory.Used), maximum))
		} else {
			lines = append(lines, "  memory · "+topLimitStateText(memory.topLimitStatus))
		}
		events := group.Events
		if events.Status == "available" && events.OOM != nil && events.OOMKill != nil {
			lines = append(lines, fmt.Sprintf("  local OOM %d · OOM kill %d · cumulative", *events.OOM, *events.OOMKill))
		} else {
			lines = append(lines, "  local OOM · "+topLimitStateText(events.topLimitStatus))
		}
		cpu := group.CPU
		if cpu.Status == "available" && cpu.WindowS != nil && cpu.Periods != nil && cpu.ThrottledPeriods != nil && cpu.ThrottledUS != nil {
			lines = append(lines, fmt.Sprintf("  cpu throttle %d/%d periods · %.2fms · window %.2fs", *cpu.ThrottledPeriods, *cpu.Periods, float64(*cpu.ThrottledUS)/1000, *cpu.WindowS))
		} else {
			lines = append(lines, "  cpu throttle · "+topLimitStateText(cpu.topLimitStatus))
		}
	}
	return lines
}

// topProbeDetail은 window 동안 센 run-queue 대기와 block I/O 지연이다. 지연 뒤의 p95는 그 값이 든 구간의 위쪽 경계다.
// 분포가 없는 platform은 p95를 빼고, 셀 수 없는 I/O는 n/a로 둔다.
func topProbeDetail(stats topProbeStats) string {
	latency := func(sumNS, count uint64, hist *topProbeHist) string {
		average, ok := topProbeAverageMS(sumNS, count)
		if !ok {
			return "—"
		}
		if hist == nil {
			return fmt.Sprintf("avg %.2fms", average)
		}
		p95, ok := topProbePercentileMS(*hist, 0.95)
		if !ok {
			return fmt.Sprintf("avg %.2fms", average)
		}
		return fmt.Sprintf("avg %.2fms p95 <%gms", average, p95)
	}
	runq := fmt.Sprintf("runq %d %s", stats.RunqCount, latency(stats.RunqSumNS, stats.RunqCount, stats.RunqHist))
	if stats.unmeasured() {
		runq = "runq n/a"
	}
	io := "io n/a"
	if stats.IO != nil {
		io = fmt.Sprintf("io %d %s", stats.IO.Count, latency(stats.IO.SumNS, stats.IO.Count, &stats.IO.Hist))
	}
	detail := fmt.Sprintf("%s %.0fs · %s · %s", stats.Source, stats.Window.Seconds(), runq, io)
	if stats.Unreadable > 0 {
		detail += fmt.Sprintf(" · %d need root", stats.Unreadable)
	}
	return detail
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

func networkConntrackCell(health *networkHealthRate) topCell {
	if health == nil {
		return topPlainCell("—")
	}
	usage, valid := networkConntrackUsage(&health.networkHealth)
	return topOptionalCell(valid, "%.1f", usage, topThreshold{warn: 90, danger: 98})
}

// topNetworkRateCell은 소수 한 자리를 우선한다. interval이 2초면 overflow 한 번이 0.5/s라서 정수로 줄이면 0으로 숨는다.
func topNetworkRateCell(health *networkHealthRate, name string, width int) string {
	if health == nil || health.Rates[name].PerSecond == nil {
		return "—"
	}
	value := *health.Rates[name].PerSecond
	if text := fmt.Sprintf("%.1f", value); len(text) <= width {
		return text
	}
	return topCompactCount(value, width)
}

func (model topModel) networkPeakLines(last topDashboardRow) []string {
	var peakUsage float64
	var peakAt time.Time
	rates := map[string]float64{}
	for _, row := range model.rows {
		if last.at.Sub(row.at) > topPeakWindow {
			continue
		}
		health := row.rate.NetworkHealth
		if health == nil {
			continue
		}
		if usage, ok := networkConntrackUsage(&health.networkHealth); ok && (peakAt.IsZero() || usage > peakUsage) {
			peakUsage, peakAt = usage, row.at
		}
		for name, rate := range health.Rates {
			if rate.PerSecond != nil && *rate.PerSecond > rates[name] {
				rates[name] = *rate.PerSecond
			}
		}
	}
	usage := "—"
	if !peakAt.IsZero() {
		usage = fmt.Sprintf("%.1f%% at %s", peakUsage, peakAt.Format("15:04:05"))
	}
	peak := func(key string) string {
		for _, row := range model.rows {
			if last.at.Sub(row.at) <= topPeakWindow && row.rate.NetworkHealth != nil && row.rate.NetworkHealth.Rates[key].PerSecond != nil {
				return fmt.Sprintf("%.1f/s", rates[key])
			}
		}
		return "—"
	}
	return []string{
		"peaks 60s · conntrack " + usage,
		"listen overflow " + peak("listen_overflows") + " · drop " + peak("listen_drops"),
		"softnet drop " + peak("softnet_dropped") + " · UDP buffer " + peak("udp_rcvbuf_errors"),
	}
}
