package edc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

const (
	remoteLogTailLines = 10
	// remoteBottomLines는 표 아래 블록의 줄 수다. 확인 질문과 상태줄이 같은 한 줄을 쓴다.
	remoteBottomLines = 1
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
	state  remoteCellState
	phase  string
	status Status
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
	height     int
	running    string // 지금 실행 중인 host / step / phase
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

type remoteLogMsg struct{ prefix, line string }

type remoteDoneMsg struct{}

func newRemoteModel(view remotePlanView, confirm bool, color, verbose bool, cancel context.CancelFunc) remoteModel {
	hosts, recipe := view.hosts, view.recipe
	group := view.group
	stage := remoteStageRunning
	if confirm {
		stage = remoteStageConfirm
	}
	model := remoteModel{
		group: group, stage: stage, answered: make(chan bool, 1),
		header:    remoteRunHeader(group, view.inventoryPath, view.recipePath, view.cwd, hosts, recipe),
		legend:    remoteStepLegend(recipe, hosts),
		table:     newRemoteTable(hosts, recipe, view.width),
		hostIndex: make(map[string]int, len(hosts)), stepIndex: make(map[string]int, len(recipe.Steps)),
		spinner: liveSpinner(), started: time.Now(), now: time.Now, color: color, verbose: verbose, cancel: cancel,
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

const (
	remotePhaseCommand = "command"
	remotePhaseVerify  = "verify"
)

func (model remoteModel) Init() tea.Cmd { return model.spinner.Tick }

func (model remoteModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case remoteStartMsg:
		updated := model.withCell(value.host, value.step, func(cell remoteCell) remoteCell {
			return remoteCell{state: remoteCellRunning, phase: value.phase}
		})
		updated.running = value.host + " / " + value.step + " / " + value.phase
		return updated, nil
	case remoteResultMsg:
		updated := model.withCell(value.host, value.step, func(cell remoteCell) remoteCell {
			return remoteCell{state: remoteCellDone, status: value.status}
		})
		updated.completed = updated.countCompleted()
		return updated, nil
	case remoteLogMsg:
		model.logs = appendRemoteLog(model.logs, value.prefix+" │ "+value.line)
		return model, nil
	case remoteDoneMsg:
		model.finished = true
		model.stage = remoteStageDone
		return model, tea.Quit
	case tea.WindowSizeMsg:
		model.height = value.Height
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

func appendRemoteLog(logs []string, line string) []string {
	logs = append(append([]string{}, logs...), line)
	if len(logs) > remoteLogTailLines {
		logs = logs[len(logs)-remoteLogTailLines:]
	}
	return logs
}

func (model remoteModel) View() tea.View {
	var builder strings.Builder
	builder.WriteString(model.header)
	builder.WriteString("\n")
	builder.WriteString(model.table.header() + "\n")
	visible := model.visibleHosts()
	for hostIndex, host := range visible {
		cells := make([]string, 0, len(model.cells[hostIndex]))
		for _, cell := range model.cells[hostIndex] {
			cells = append(cells, model.cellText(cell))
		}
		builder.WriteString(model.table.row(host, cells) + "\n")
	}
	if hidden := len(model.table.hosts) - len(visible); hidden > 0 {
		builder.WriteString(T("remote.label.hidden_hosts", hidden) + "\n")
	}
	if model.table.symbols {
		builder.WriteString(remoteSymbolLegend() + "\n")
	}
	// 확인과 진행 상황은 표 바로 아래에 둔다. 실행 대상을 본 자리에서 답하고, 같은 자리가 상태줄로 바뀐다.
	builder.WriteString("\n")
	builder.WriteString(model.bottom())
	builder.WriteString("\n")
	for _, line := range model.legend {
		builder.WriteString(liveMuted(line, model.color) + "\n")
	}
	if model.verbose {
		for _, line := range model.logs {
			builder.WriteString("  " + line + "\n")
		}
	}
	return liveFrame(builder.String(), liveLineCount(builder.String()))
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
	if model.height > 0 {
		// header 2줄, 빈 줄 3개, 표 header, 범례, 아래 블록을 뺀 만큼만 host를 보여 준다.
		available := model.height - 6 - len(model.legend) - remoteBottomLines
		if model.verbose {
			available -= len(model.logs)
		}
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
		return remoteGlyphPending
	case remoteCellRunning:
		// phase는 상태줄에서 알려 준다. 칸은 좁게 둔다.
		return model.spinner.View()
	case remoteCellDone:
		return model.table.statusCell(cell.status, model.color)
	default:
		return remoteGlyphAbsent
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
	line := fmt.Sprintf("%s  %s  %s", model.spinner.View(), completed, model.elapsed())
	if model.running != "" {
		line = fmt.Sprintf("%s  %s  ·  %s  %s", model.spinner.View(), model.running, completed, model.elapsed())
	}
	return line + "\n"
}

func (model remoteModel) elapsed() string {
	return model.now().Sub(model.started).Round(liveElapsedPrecision).String()
}
