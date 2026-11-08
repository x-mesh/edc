package edc

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	// remoteLogTailLines는 화면 높이를 모를 때 -v로 보여 줄 출력 줄 수다.
	remoteLogTailLines = 10
	// remoteCellOutputLines는 칸마다 모아 두는 출력 줄 수다. 이보다 큰 터미널은 드물다.
	remoteCellOutputLines = 200
	// remoteBottomLines는 표 아래 블록의 줄 수다. 확인 질문과 상태줄이 같은 한 줄을 쓴다.
	remoteBottomLines = 1
	// remoteRunningLines는 상태줄 아래에 보여 줄 실행 중인 칸의 최대 수다. 넘치면 남은 수만 알린다.
	remoteRunningLines = 5
)

// remoteStage는 한 화면이 지나는 단계다. 확인, 실행, 완료가 같은 표를 쓴다.
type remoteStage int

const (
	remoteStageConfirm remoteStage = iota
	remoteStageRunning
	remoteStageDone
)

type remoteCellState int

const (
	remoteCellAbsent  remoteCellState = iota // tag가 맞지 않아 실행하지 않는 자리
	remoteCellPending                        // 실행 대기
	remoteCellRunning                        // 실행 중
	remoteCellDone                           // 결과 확정
)

type remoteCell struct {
	state   remoteCellState
	phase   string
	status  Status
	started time.Time // 지금 phase를 시작한 시각
	last    string    // 마지막으로 받은 출력 줄
	lastAt  time.Time // 마지막 출력을 받은 시각
	output  []string  // 실행 중에 받은 최근 출력
}

type remoteModel struct {
	group      string
	header     string
	legend     []string
	stage      remoteStage
	confirmYes bool
	answered   chan bool // 확인 답을 한 번 보낸다
	table      remoteTable
	hostIndex  map[string]int
	stepIndex  map[string]int
	cells      [][]remoteCell
	spinner    spinner.Model
	started    time.Time
	now        func() time.Time
	color      bool
	verbose    bool
	logs       []string
	width      int
	height     int
	finished   bool
	cancelling bool
	cancel     context.CancelFunc
	total      int
	completed  int
}

type remoteStartMsg struct{ host, step, phase string }

type remoteResultMsg struct {
	host, step string
	status     Status
}

type remoteLogMsg struct{ host, step, prefix, line string }

type remoteDoneMsg struct{}

func newRemoteModel(view remotePlanView, confirm bool, color, verbose bool, cancel context.CancelFunc) remoteModel {
	hosts, recipe := view.hosts, view.recipe
	group := view.group
	stage := remoteStageRunning
	if confirm {
		stage = remoteStageConfirm
	}
	header := remoteRunHeader(group, view.inventoryPath, view.recipePath, view.cwd, hosts, recipe) + view.search
	if color {
		header = remotePaintHeader(header)
	}
	if view.command != "" {
		// 재현 명령은 복사해 갈 값이라 다른 명령처럼 굵은 청록으로 둔다.
		command := view.command
		if color {
			command = richPaint(command, richCyan, true)
		}
		header = command + "\n" + header
	}
	table := newRemoteTable(hosts, recipe, view.width)
	if color {
		table.badges = true
		table = table.withWidth(view.width)
	}
	model := remoteModel{
		group: group, stage: stage, answered: make(chan bool, 1),
		header:    header,
		legend:    remoteStepLegend(recipe, hosts),
		table:     table,
		hostIndex: make(map[string]int, len(hosts)), stepIndex: make(map[string]int, len(recipe.Steps)),
		spinner: liveSpinner(), started: time.Now(), now: time.Now, color: color, verbose: verbose, cancel: cancel, width: view.width,
	}
	for index, host := range hosts {
		model.hostIndex[host.Name] = index
	}
	for index, step := range recipe.Steps {
		model.stepIndex[step.Name] = index
	}
	model.cells = make([][]remoteCell, len(hosts))
	for hostIndex, host := range hosts {
		model.cells[hostIndex] = make([]remoteCell, len(recipe.Steps))
		for stepIndex, step := range recipe.Steps {
			if !stepRunsOnHost(step, host) {
				continue
			}
			model.cells[hostIndex][stepIndex] = remoteCell{state: remoteCellPending}
			model.total++
		}
	}
	return model
}

// remoteHeaderLabel은 머리말의 inventory, recipe, search 이름표다. 줄 맨 앞이나 앞 칸과 공백 여섯 칸 뒤에 온다.
var remoteHeaderLabel = regexp.MustCompile(`(?m)(^|\s{6})(inventory|recipe|search)(\s{2,})`)

// remotePaintHeader는 머리말의 요약 줄을 굵은 자홍으로, 파일 이름표를 회색으로 칠한다. 경로는 기본 색이다.
func remotePaintHeader(header string) string {
	summary, rest, _ := strings.Cut(header, "\n")
	rest = remoteHeaderLabel.ReplaceAllStringFunc(rest, func(match string) string {
		parts := remoteHeaderLabel.FindStringSubmatch(match)
		return parts[1] + richDim(parts[2]) + parts[3]
	})
	return richPaint(summary, richMagenta, true) + "\n" + rest
}

const (
	remotePhaseCommand = "command"
	remotePhaseVerify  = "verify"
)

func (model remoteModel) Init() tea.Cmd { return model.spinner.Tick }

func (model remoteModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case remoteStartMsg:
		now := model.now()
		updated := model.withCell(value.host, value.step, func(cell remoteCell) remoteCell {
			return remoteCell{state: remoteCellRunning, phase: value.phase, started: now}
		})
		return updated, nil
	case remoteResultMsg:
		updated := model.withCell(value.host, value.step, func(cell remoteCell) remoteCell {
			return remoteCell{state: remoteCellDone, status: value.status}
		})
		updated.completed = updated.countCompleted()
		return updated, nil
	case remoteLogMsg:
		if line := remoteLastLine(value.line); line != "" {
			now := model.now()
			model = model.withCell(value.host, value.step, func(cell remoteCell) remoteCell {
				if cell.state == remoteCellRunning {
					cell.last, cell.lastAt = line, now
					// 이전 model과 배열을 같이 쓰지 않게 매번 새로 만든다.
					cell.output = appendLimited(cell.output, remoteLogLine(value.line), remoteCellOutputLines)
				}
				return cell
			})
		}
		if model.verbose {
			model.logs = appendLimited(model.logs, value.prefix+" │ "+value.line, remoteLogTailLines)
		}
		return model, nil
	case remoteDoneMsg:
		model.finished = true
		model.stage = remoteStageDone
		return model, tea.Quit
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.table = model.table.withWidth(value.Width)
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(value)
	case spinner.TickMsg:
		if model.finished {
			return model, nil
		}
		var cmd tea.Cmd
		model.spinner, cmd = model.spinner.Update(value)
		return model, cmd
	}
	return model, nil
}

func (model remoteModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if model.stage == remoteStageConfirm {
		switch key.String() {
		case "left", "h", "right", "l", "tab":
			model.confirmYes = !model.confirmYes
		case "y":
			return model.answer(true)
		case "n", "esc", "q", "ctrl+c":
			return model.answer(false)
		case "enter":
			return model.answer(model.confirmYes)
		}
		return model, nil
	}
	if key.String() == "ctrl+c" {
		if model.cancelling {
			return model, tea.Quit
		}
		model.cancelling = true
		if model.cancel != nil {
			model.cancel()
		}
	}
	return model, nil
}

// answer는 확인 답을 caller에 전달하고 실행 단계로 넘어간다.
func (model remoteModel) answer(yes bool) (tea.Model, tea.Cmd) {
	model.confirmYes = yes
	model.stage = remoteStageRunning
	select {
	case model.answered <- yes:
	default:
	}
	if !yes {
		return model, tea.Quit
	}
	return model, nil
}

// withCell은 cell 하나만 바꾼 사본을 돌려준다. bubbletea model은 값으로 전달되므로 slice를 복사한다.
func (model remoteModel) withCell(host, step string, change func(remoteCell) remoteCell) remoteModel {
	hostIndex, hostKnown := model.hostIndex[host]
	stepIndex, stepKnown := model.stepIndex[step]
	if !hostKnown || !stepKnown {
		return model
	}
	cells := make([][]remoteCell, len(model.cells))
	copy(cells, model.cells)
	row := make([]remoteCell, len(cells[hostIndex]))
	copy(row, cells[hostIndex])
	row[stepIndex] = change(row[stepIndex])
	cells[hostIndex] = row
	model.cells = cells
	return model
}

func (model remoteModel) countCompleted() int {
	count := 0
	for _, row := range model.cells {
		for _, cell := range row {
			if cell.state == remoteCellDone {
				count++
			}
		}
	}
	return count
}

// remoteLastLine은 출력 한 줄에서 화면에 보여 줄 부분을 고른다. 진행 막대는 \r로 같은 줄을 덮어쓰므로
// 마지막 조각만 남기고, 색 코드와 앞뒤 공백을 지운다.
func remoteLastLine(line string) string {
	line = ansi.Strip(line)
	parts := strings.Split(line, "\r")
	for index := len(parts) - 1; index >= 0; index-- {
		if trimmed := strings.TrimSpace(parts[index]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// remoteLogLine은 출력 줄을 화면 한 줄에 맞게 다듬는다. 색 코드를 남기면 줄을 자를 때 escape 중간이 잘린다.
func remoteLogLine(line string) string {
	line = ansi.Strip(line)
	if index := strings.LastIndex(strings.TrimRight(line, "\r"), "\r"); index >= 0 {
		line = line[index+1:]
	}
	return strings.TrimRight(line, "\r \t")
}

func appendLimited(lines []string, line string, limit int) []string {
	lines = append(append(make([]string, 0, len(lines)+1), lines...), line)
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

func (model remoteModel) View() tea.View {
	var builder strings.Builder
	if model.stage == remoteStageConfirm && model.height > 0 {
		// bubbletea v2.0.9는 전체 화면에서 돌아올 때 이전 inline 화면을 한 줄 덜 올라가 지운다. 실행이 전체 화면으로
		// 넘어갈 확인 화면은 빈 줄로 시작해, 지워지지 않고 남는 줄이 머리말 대신 빈 줄이 되게 한다.
		builder.WriteString("\n")
	}
	builder.WriteString(model.header)
	builder.WriteString("\n")
	builder.WriteString(liveMuted(model.table.header(), model.color) + "\n")
	visible := model.visibleHosts()
	for hostIndex, host := range visible {
		cells := make([]string, 0, len(model.cells[hostIndex]))
		for _, cell := range model.cells[hostIndex] {
			cells = append(cells, model.cellText(cell))
		}
		builder.WriteString(model.table.row(host, cells) + "\n")
	}
	if hidden := len(model.table.hosts) - len(visible); hidden > 0 {
		builder.WriteString(liveMuted(T("remote.label.hidden_hosts", hidden), model.color) + "\n")
	}
	if model.table.symbols {
		builder.WriteString(liveMuted(remoteSymbolLegend(), model.color) + "\n")
	}
	// 확인과 진행 상황은 표 바로 아래에 둔다. 실행 대상을 본 자리에서 답하고, 같은 자리가 상태줄로 바뀐다.
	builder.WriteString("\n")
	builder.WriteString(model.bottom())
	for _, line := range model.runningLines() {
		builder.WriteString(line + "\n")
	}
	builder.WriteString("\n")
	for _, line := range model.legend {
		builder.WriteString(liveMuted(line, model.color) + "\n")
	}
	if model.fullScreen() {
		// 실행 중에는 전체 화면을 쓰고, 표 아래 남은 줄을 실행 중인 칸의 출력으로 채운다.
		for _, line := range model.outputBlocks(model.height - liveLineCount(builder.String())) {
			builder.WriteString(line + "\n")
		}
		view := liveFrame(builder.String(), model.height)
		view.AltScreen = true
		return view
	}
	if model.verbose && model.stage == remoteStageRunning && !model.finished {
		for _, line := range model.logs {
			builder.WriteString("  " + line + "\n")
		}
	}
	return liveFrame(builder.String(), liveLineCount(builder.String()))
}

// fullScreen은 전체 화면을 쓰는 단계인지다. 확인은 셸 기록과 함께 보도록 inline으로 받고, 끝난 화면은
// inline으로 돌아가 결과 표를 터미널 기록에 남긴다. 화면 높이를 모르면 채울 줄 수를 정할 수 없어 inline으로 둔다.
func (model remoteModel) fullScreen() bool {
	return model.stage == remoteStageRunning && !model.finished && model.height > 0
}

// bottom은 표 바로 아래 한 줄이다. 확인 중에는 질문, 실행 중에는 상태줄이다.
func (model remoteModel) bottom() string {
	if model.stage == remoteStageConfirm {
		return confirmPrompt(T("remote.confirm.run"), model.confirmYes, model.color) + "\n"
	}
	return model.statusLine()
}

// visibleHosts는 화면 높이를 넘는 host를 잘라 낸다. 잘린 수는 요약 줄로 알린다.
func (model remoteModel) visibleHosts() []string {
	limit := len(model.table.hosts)
	// 끝난 표는 터미널 기록에 남으므로 화면 높이와 상관없이 모든 host를 보여 준다.
	if model.height > 0 && !model.finished {
		// header 줄, 빈 줄 3개, 표 header, 범례, 아래 블록을 뺀 만큼만 host를 보여 준다. -v면 header에 탐색 줄이 붙는다.
		available := model.height - strings.Count(model.header, "\n") - 4 - len(model.legend) - remoteBottomLines - len(model.runningLines())
		if model.table.symbols {
			available--
		}
		if available < 1 {
			available = 1
		}
		if available < limit {
			limit = available
		}
	}
	return model.table.hosts[:limit]
}

func (model remoteModel) cellText(cell remoteCell) string {
	switch cell.state {
	case remoteCellPending:
		return liveMuted(remoteGlyphPending, model.color)
	case remoteCellRunning:
		// phase는 상태줄에서 알려 준다. 칸은 좁게 둔다.
		return model.spinner.View()
	case remoteCellDone:
		return model.table.statusCell(cell.status, model.color)
	default:
		return liveMuted(remoteGlyphAbsent, model.color)
	}
}

func (model remoteModel) statusLine() string {
	if model.cancelling && !model.finished {
		return T("remote.status.cancelling") + "\n"
	}
	if model.finished {
		return "\n"
	}
	completed := T("remote.label.completed_count", model.completed, model.total)
	if model.color {
		return model.richStatusLine(completed) + "\n"
	}
	return fmt.Sprintf("%s  %s  %s", model.spinner.View(), completed, model.elapsed()) + "\n"
}

// remoteProgressCells는 진행 막대의 칸 수다.
const remoteProgressCells = 20

// richStatusLine은 색을 쓰는 상태줄이다. 끝난 칸을 상태 색으로, 남은 칸을 회색으로 나눈 차분한 막대를 완료 수 앞에 둔다.
// 칸이 하나뿐이면 막대는 늘 한 색이라 빼고, 막대를 넣어 줄이 화면 폭을 넘으면 막대를 뺀다.
func (model remoteModel) richStatusLine(completed string) string {
	line := func(progress string) string {
		return model.spinner.View() + "  " + progress + "  " + richDim(model.elapsed())
	}
	if model.total <= 1 {
		return line(completed)
	}
	withBar := line(model.progressBar() + " " + completed)
	if model.width > 0 && liveWidth(withBar) > model.width {
		return line(completed)
	}
	return withBar
}

func (model remoteModel) progressBar() string {
	counts := map[Status]int{}
	for _, row := range model.cells {
		for _, cell := range row {
			if cell.state == remoteCellDone {
				counts[cell.status]++
			}
		}
	}
	segments := []richSegment{}
	for _, status := range []Status{StatusPass, StatusWarn, StatusFail, StatusSkip} {
		segments = append(segments, richSegment{value: float64(counts[status]), color: richStatusColor(status)})
	}
	bar := richSegments(segments, remoteProgressCells*model.completed/model.total)
	return bar + richDim(strings.Repeat("░", max(0, remoteProgressCells-liveWidth(bar))))
}

func (model remoteModel) elapsed() string {
	return model.now().Sub(model.started).Round(liveElapsedPrecision).String()
}

type remoteRunningCell struct {
	host, step string
	cell       remoteCell
}

// runningLines는 실행 중인 칸을 오래 걸린 순서로 한 줄씩 보여 준다. 칸마다 경과 시간과 마지막 출력,
// 그 출력이 나온 뒤 지난 시간을 붙여, 화면이 멈춘 것처럼 보일 때 어느 host의 어느 step이 무엇을 하다 멈췄는지 알린다.
func (model remoteModel) runningLines() []string {
	if model.stage != remoteStageRunning || model.finished || model.cancelling {
		return nil
	}
	running := model.runningCells()
	now := model.now()
	lines := make([]string, 0, min(len(running), remoteRunningLines)+1)
	for index, item := range running {
		if index == remoteRunningLines {
			lines = append(lines, liveMuted("  "+T("remote.label.hidden_running", len(running)-index), model.color))
			break
		}
		lines = append(lines, model.runningLine(item, now))
	}
	return lines
}

// runningCells는 실행 중인 칸을 오래 걸린 순서로 돌려준다.
func (model remoteModel) runningCells() []remoteRunningCell {
	var running []remoteRunningCell
	for hostIndex, row := range model.cells {
		for stepIndex, cell := range row {
			if cell.state == remoteCellRunning {
				running = append(running, remoteRunningCell{host: model.table.hosts[hostIndex], step: model.table.steps[stepIndex], cell: cell})
			}
		}
	}
	sort.SliceStable(running, func(left, right int) bool { return running[left].cell.started.Before(running[right].cell.started) })
	return running
}

func (model remoteModel) runningLine(item remoteRunningCell, now time.Time) string {
	place := item.host + " / " + item.step + " / " + item.cell.phase
	elapsed := now.Sub(item.cell.started).Round(time.Second).String()
	output := T("remote.label.no_output")
	if item.cell.last != "" {
		output = item.cell.last + "  " + T("remote.label.output_age", now.Sub(item.cell.lastAt).Round(time.Second))
	}
	line := "  " + place + "  " + elapsed + "  ·  " + output
	if model.width > 0 {
		line = strings.TrimSuffix(truncateLine(line, model.width), "\n")
	}
	if !model.color {
		return line
	}
	// 자른 뒤에 색을 칠해야 폭 계산이 색 코드에 흔들리지 않는다.
	if head, tail, ok := strings.Cut(line, "  ·  "); ok {
		return head + richDim("  ·  "+tail)
	}
	return line
}

// outputBlocks는 남은 rows 줄을 실행 중인 칸마다 제목 한 줄과 최근 출력으로 나눠 채운다. 칸마다 따로
// 모으므로 여러 host가 동시에 출력해도 섞이지 않는다. 오래 걸린 칸이 먼저 오고, 나누고 남은 줄도 그 칸이 받는다.
// 칸마다 두 줄을 줄 수 없으면 앞에서부터 들어가는 칸만 보여 준다.
func (model remoteModel) outputBlocks(rows int) []string {
	if rows < 2 {
		return nil
	}
	running := model.runningCells()
	if count := rows / 2; len(running) > count {
		running = running[:count]
	}
	if len(running) == 0 {
		return nil
	}
	share, extra := rows/len(running)-1, rows%len(running)
	lines := make([]string, 0, rows)
	for index, item := range running {
		size := share
		if index == 0 {
			size += extra
		}
		title := "── " + item.host + " / " + item.step + " / " + item.cell.phase + " "
		lines = append(lines, liveMuted(model.fit(title+strings.Repeat("─", max(0, model.width-liveWidth(title)))), model.color))
		output := item.cell.output
		if len(output) > size {
			output = output[len(output)-size:]
		}
		for _, line := range output {
			lines = append(lines, model.fit("  "+line))
		}
	}
	return lines
}

// fit은 줄이 접혀 화면 높이가 늘지 않게 화면 폭에 맞춰 자른다.
func (model remoteModel) fit(line string) string {
	if model.width <= 0 {
		return line
	}
	return strings.TrimSuffix(truncateLine(line, model.width), "\n")
}
