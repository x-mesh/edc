package edc

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	aiUsageBarWidth = 10
	aiWarnPercent   = 80
	aiDangerPercent = 95
	// aiBoxWidth는 한도 상자의 최대 폭이다. 표와 같은 88열에 맞춘다.
	aiBoxWidth = 88
	// aiDefaultBodyLines는 terminal 크기를 받기 전에 그리는 표 행 수다.
	aiDefaultBodyLines = 12
)

// aiRowSizes는 +와 -로 옮겨 다니는 행 구간이다. 1시간 행은 하루를 시간대별로 본다.
var aiRowSizes = []time.Duration{aiBucket, time.Minute, 5 * time.Minute, time.Hour}

// aiColumns는 도구마다 같은 순서로 그리는 열이다.
var aiColumns = []struct {
	title string
	width int
}{{"req", 4}, {"in", 6}, {"out", 6}, {"cache", 6}, {"hit", 4}, {"total", 7}}

func runAIDashboard(collector *aiCollector, poll time.Duration, version string) int {
	model := aiModel{collector: collector, poll: poll, version: version, now: time.Now(), rowSize: time.Minute}
	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type aiModel struct {
	collector     *aiCollector
	poll          time.Duration
	version       string
	width, height int
	now           time.Time
	providers     []aiProvider
	lastResets    map[string]aiResetEvent
	usage         aiUsageSnapshot
	rowSize       time.Duration
	// back은 진행 중인 행에서 몇 행 거슬러 올라갔는지다. 0이면 실시간 보기다.
	back           int
	claudeInterval time.Duration
	polled         bool
	scanned        bool
	logErr         error
	stateErr       error
}

type aiClockMsg time.Time

type aiPollMsg aiPollResult

type aiScanMsg aiUsageSnapshot

func (model aiModel) Init() tea.Cmd {
	return tea.Batch(aiClock(), model.pollCmd(0), model.scanCmd(0))
}

// aiClock은 system 시계의 초 경계에 맞춰 깨운다. 카운트다운이 초가 바뀌는 순간에 함께 바뀐다.
func aiClock() tea.Cmd {
	return tea.Every(time.Second, func(at time.Time) tea.Msg { return aiClockMsg(at) })
}

// pollCmd와 scanCmd는 앞선 결과를 받은 뒤에만 다음 호출을 예약한다. 수집기 상태를 두 goroutine이 함께 쓰지 않는다.
func (model aiModel) pollCmd(delay time.Duration) tea.Cmd {
	collector, poll := model.collector, model.poll
	run := func() tea.Msg { return aiPollMsg(collector.poll(context.Background(), poll)) }
	if delay == 0 {
		return run
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return run() })
}

func (model aiModel) scanCmd(delay time.Duration) tea.Cmd {
	collector := model.collector
	run := func() tea.Msg { return aiScanMsg(collector.scan()) }
	if delay == 0 {
		return run
	}
	return tea.Tick(delay, func(time.Time) tea.Msg { return run() })
}

func (model aiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case aiClockMsg:
		model.now = time.Time(value)
		return model, aiClock()
	case aiPollMsg:
		model.providers, model.lastResets, model.logErr, model.polled = value.providers, value.lastResets, value.logErr, true
		model.claudeInterval, model.stateErr = value.claudeInterval, value.stateErr
		return model, model.pollCmd(model.poll)
	case aiScanMsg:
		model.usage, model.scanned = aiUsageSnapshot(value), true
		return model, model.scanCmd(aiScanInterval)
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.back = min(model.back, model.maxBack())
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		}
		return model.updateKey(value.String()), nil
	}
	return model, nil
}

// updateKey는 edc top과 같은 키를 쓴다. 행 구간을 바꾸면 실시간 보기로 돌아간다. 같은 행 번호가 다른 시각을 가리키기 때문이다.
func (model aiModel) updateKey(key string) aiModel {
	switch key {
	case "+", "=":
		model.rowSize, model.back = aiNextRowSize(model.rowSize, 1), 0
	case "-", "_":
		model.rowSize, model.back = aiNextRowSize(model.rowSize, -1), 0
	case "up", "k":
		model.back = min(model.back+1, model.maxBack())
	case "down", "j":
		model.back = max(0, model.back-1)
	case "pgup":
		model.back = min(model.back+model.bodyLines(), model.maxBack())
	case "pgdown":
		model.back = max(0, model.back-model.bodyLines())
	case "end", "g":
		model.back = 0
	}
	return model
}

func aiNextRowSize(current time.Duration, step int) time.Duration {
	for index, size := range aiRowSizes {
		if size == current {
			return aiRowSizes[min(len(aiRowSizes)-1, max(0, index+step))]
		}
	}
	return time.Minute
}

// maxBack은 보관한 24시간 안에서 거슬러 올라갈 수 있는 행 수다.
func (model aiModel) maxBack() int {
	return max(0, int(aiRetention/model.rowSize)-model.bodyLines())
}

func (model aiModel) bodyLines() int {
	if model.height <= 0 {
		return aiDefaultBodyLines
	}
	// 제목, 묶음 머리, 열 머리, 구분선, Σ 행, 상태 줄을 뺀다.
	return max(1, model.height-5-len(aiTotalWindows)-len(model.boxLines(model.displayWidth())))
}

// minHeight는 표 본문이 한 줄 남는 높이다. 한도 상자의 줄 수는 받은 한도 창 수에 따라 달라진다.
func (model aiModel) minHeight() int {
	return 6 + len(aiTotalWindows) + len(model.boxLines(max(model.displayWidth(), 40)))
}

func (model aiModel) displayWidth() int {
	if model.width <= 0 {
		return aiBoxWidth
	}
	return model.width
}

func (model aiModel) View() tea.View {
	width := model.displayWidth()
	if minHeight := model.minHeight(); model.width > 0 && (model.width < 40 || model.height < minHeight) {
		view := tea.NewView(ansi.Truncate("terminal too small · q quit", width, "") + "\n" + ansi.Truncate(fmt.Sprintf("resize to at least 40×%d", minHeight), width, ""))
		view.AltScreen = true
		return view
	}
	lines := []string{model.title(width), model.groupHeader(), aiColumnHeader()}
	if !model.scanned {
		lines = append(lines, "reading the local logs")
	} else {
		rows := model.usage.rows(model.now.Add(-time.Duration(model.back)*model.rowSize), model.rowSize, model.bodyLines())
		for index, row := range rows {
			lines = append(lines, aiTableRow(row, model.rowSize, model.back == 0 && index == len(rows)-1))
		}
		lines = append(lines, aiTableSeparator())
		// Σ 행은 기록을 거슬러 봐도 지금 기준이다. 행 간격과도 관계없다.
		for _, total := range model.usage.totals(model.now) {
			lines = append(lines, aiTotalRow(total))
		}
	}
	lines = append(lines, model.boxLines(width)...)
	lines = append(lines, model.statusLine(width))
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// title은 오른쪽에 조회 간격과 보기 상태를 둔다. 맨 아래 줄은 키 안내만 남긴다.
func (model aiModel) title(width int) string {
	left := "🐰 edc ai · this host"
	state := "poll " + aiDuration(model.poll)
	if model.claudeInterval > model.poll {
		// 429로 늘어난 Claude 간격이다. 숫자가 바뀌지 않는 이유를 여기서 알 수 있다.
		state += " · claude " + aiDuration(model.claudeInterval)
	}
	if model.back > 0 {
		state += fmt.Sprintf(" · history -%d", model.back)
	} else {
		state += " · live"
	}
	right := state + " · " + model.now.Local().Format("15:04:05 MST") + " 🐰"
	space := min(width, aiBoxWidth) - ansi.StringWidth(left) - ansi.StringWidth(right)
	if space < 2 {
		return left + " · " + right
	}
	return left + strings.Repeat(" ", space) + right
}

func aiGroupWidth() int {
	width := len(aiColumns) - 1
	for _, column := range aiColumns {
		width += column.width
	}
	return width
}

func (model aiModel) groupHeader() string {
	return strings.Repeat(" ", 9) + "│" + topGroupTitle("claude", aiGroupWidth()) + "│" + topGroupTitle("codex", aiGroupWidth()) + "│"
}

func aiColumnHeader() string {
	titles := make([]string, len(aiColumns))
	for index, column := range aiColumns {
		titles[index] = fmt.Sprintf("%*s", column.width, column.title)
	}
	cells := strings.Join(titles, "│")
	return "│    time│" + cells + "│" + cells + "│"
}

// aiTableRow는 진행 중인 행의 왼쪽 테두리를 ▸로 바꾼다. 색 없이도 아직 늘고 있는 행을 알 수 있다.
func aiTableRow(row aiRow, size time.Duration, live bool) string {
	label := row.Start.Format("15:04:05")
	if size >= time.Hour {
		label = row.Start.Format("01-02 15")
	}
	edge := "│"
	if live {
		edge = "▸"
	}
	return edge + fmt.Sprintf("%8s", label) + "│" + aiUsageCells(row.Claude) + "│" + aiUsageCells(row.Codex) + "│"
}

func aiTableSeparator() string {
	cells := make([]string, len(aiColumns))
	for index, column := range aiColumns {
		cells[index] = strings.Repeat("─", column.width)
	}
	group := strings.Join(cells, "┼")
	return "├" + strings.Repeat("─", 8) + "┼" + group + "┼" + group + "┤"
}

func aiTotalRow(total aiTotal) string {
	return "│" + fmt.Sprintf("%8s", "Σ "+total.Window) + "│" + aiUsageCells(total.Claude) + "│" + aiUsageCells(total.Codex) + "│"
}

func aiUsageCells(usage aiUsage) string {
	values := []string{fmt.Sprint(usage.Requests), aiCompact(usage.Input), aiCompact(usage.Output), aiCompact(usage.Cache), usage.cacheHitText(), aiCompact(usage.total())}
	cells := make([]string, len(values))
	for index, value := range values {
		cells[index] = fmt.Sprintf("%*s", aiColumns[index].width, value)
	}
	return strings.Join(cells, "│")
}

// aiCompact는 6칸 안에 들어가는 token 수다. 크기마다 유효 숫자를 세 자리 안팎으로 맞춘다.
func aiCompact(value int64) string {
	switch {
	case value >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(value)/1e9)
	case value >= 100_000_000:
		return fmt.Sprintf("%.0fM", float64(value)/1e6)
	case value >= 10_000_000:
		return fmt.Sprintf("%.1fM", float64(value)/1e6)
	case value >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(value)/1e6)
	case value >= 10_000:
		return fmt.Sprintf("%.0fK", float64(value)/1e3)
	case value >= 1_000:
		return fmt.Sprintf("%.1fK", float64(value)/1e3)
	}
	return fmt.Sprint(value)
}

func (model aiModel) provider(name string) aiProvider {
	for _, provider := range model.providers {
		if provider.Name == name {
			return provider
		}
	}
	return aiProvider{Name: name}
}

// boxLines는 두 상자를 위아래로 쌓는다. 한도 창 하나를 한 줄에 그려야 리셋 시각과 남은 시간이 잘리지 않는다.
func (model aiModel) boxLines(width int) []string {
	nameWidth := 3
	for _, provider := range model.providers {
		for _, window := range provider.Windows {
			nameWidth = max(nameWidth, len(window.Name))
		}
	}
	width = min(width, aiBoxWidth)
	var lines []string
	for _, name := range []string{"claude", "codex"} {
		provider := model.provider(name)
		lines = append(lines, aiBox(aiBoxTitle(provider), model.boxNote(provider), model.boxBody(provider, nameWidth), width)...)
	}
	return lines
}

func aiBoxTitle(provider aiProvider) string {
	name := map[string]string{"claude": "Claude", "codex": "Codex"}[provider.Name]
	if provider.Plan == "" {
		return name
	}
	return name + " · " + provider.Plan
}

// boxNote는 위 테두리 오른쪽에 쓰는 마지막 리셋과 받은 시각이다.
func (model aiModel) boxNote(provider aiProvider) string {
	var parts []string
	if event, ok := model.lastResets[provider.Name]; ok {
		parts = append(parts, "last reset "+event.ResetAt.Local().Format("01-02 15:04")+" "+event.Window)
	}
	switch {
	case provider.FetchedAt.IsZero():
	case provider.Err != "":
		parts = append(parts, "values from "+aiAgo(model.now.Sub(provider.FetchedAt))+" ago")
	default:
		parts = append(parts, "updated "+aiAgo(model.now.Sub(provider.FetchedAt))+" ago")
	}
	return strings.Join(parts, " · ")
}

func (model aiModel) boxBody(provider aiProvider, nameWidth int) []string {
	if !model.polled {
		return []string{"waiting for the first poll"}
	}
	var lines []string
	for _, window := range provider.Windows {
		lines = append(lines, fmt.Sprintf("%-*s  %s %s   %s", nameWidth, window.Name, aiUsageBar(window.Used),
			aiPaintPercent(fmt.Sprintf("%3.0f%%", window.Used), window.Used), aiResetText(window.ResetsAt, model.now)))
	}
	if provider.Err != "" {
		lines = append(lines, topColorDanger+"! "+provider.Err+topColorReset)
	} else if len(provider.Windows) == 0 {
		lines = append(lines, "no limits reported")
	}
	return lines
}

// aiBox는 제목과 메모가 위 테두리에 붙은 상자다. 폭이 모자라면 메모를 뺀다.
func aiBox(title, note string, body []string, width int) []string {
	head := "╭─ " + title + " "
	tail := "╮"
	if note != "" {
		tail = " " + note + " ─╮"
	}
	fill := width - ansi.StringWidth(head) - ansi.StringWidth(tail)
	if fill < 1 {
		tail = "╮"
		fill = max(0, width-ansi.StringWidth(head)-1)
	}
	lines := []string{head + strings.Repeat("─", fill) + tail}
	for _, text := range body {
		lines = append(lines, "│ "+topDashboardFitWidth(text, width-4)+" │")
	}
	return append(lines, "╰"+strings.Repeat("─", max(0, width-2))+"╯")
}

func aiUsageBar(used float64) string {
	filled := int(math.Round(used / 100 * aiUsageBarWidth))
	filled = min(aiUsageBarWidth, max(0, filled))
	return aiPaintPercent(strings.Repeat("█", filled), used) + strings.Repeat("░", aiUsageBarWidth-filled)
}

func aiPaintPercent(text string, used float64) string {
	switch {
	case used >= aiDangerPercent:
		return topColorDanger + text + topColorReset
	case used >= aiWarnPercent:
		return topColorWarn + text + topColorReset
	}
	return text
}

func aiAgo(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int64(elapsed / time.Second)
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh%02dm", seconds/3600, seconds%3600/60)
}

func (model aiModel) statusLine(width int) string {
	switch {
	case model.logErr != nil:
		return topColorDanger + topDashboardFitWidth(T("observe.ai.error.reset_log", model.logErr), width) + topColorReset
	case model.stateErr != nil:
		return topColorDanger + topDashboardFitWidth(T("observe.ai.error.state", model.stateErr), width) + topColorReset
	case model.usage.Err != "":
		return topColorDanger + topDashboardFitWidth("scan error · "+model.usage.Err, width) + topColorReset
	}
	return topDashboardFitWidth(aiRowSizeHint(model.rowSize)+liveMuted("  row size · ↑↓ history · End live · q quit", true), width)
}

// aiRowSizeHint는 -가 왼쪽(짧게), +가 오른쪽(길게)으로 옮긴다는 것을 단계 순서로 보인다.
// 지금 단계는 대괄호와 반전으로 표시해 색이 없어도 구분된다.
func aiRowSizeHint(current time.Duration) string {
	parts := []string{liveMuted("-", true)}
	for _, size := range aiRowSizes {
		if size == current {
			parts = append(parts, liveSelected("["+aiDuration(size)+"]", true))
			continue
		}
		parts = append(parts, liveMuted(aiDuration(size), true))
	}
	return strings.Join(append(parts, liveMuted("+", true)), " ")
}

func aiDuration(value time.Duration) string {
	switch {
	case value%time.Hour == 0:
		return fmt.Sprintf("%dh", value/time.Hour)
	case value%time.Minute == 0:
		return fmt.Sprintf("%dm", value/time.Minute)
	case value%time.Second == 0:
		return fmt.Sprintf("%ds", value/time.Second)
	}
	return value.String()
}
