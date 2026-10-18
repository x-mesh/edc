package edc

import (
	"fmt"
	"slices"
	"strings"
)

// topPanel은 아래 왼쪽 칸에 늘 보이는 패널이다. --panels와 defaults.top.panels가 고르고, 적은 순서가 우선순위다.
type topPanel string

const (
	topPanelNet    topPanel = "net"
	topPanelAI     topPanel = "ai"
	topPanelEvents topPanel = "events"
)

var topPanelNames = []topPanel{topPanelNet, topPanelAI, topPanelEvents}

const (
	// topPanelHistoryRows는 패널을 늘 보일 때도 표에 남기는 행 수다. 이보다 적으면 추세를 읽을 수 없다.
	topPanelHistoryRows = 5
	// topPanelEventLines는 events 패널의 줄 수다. 제목과 최근 이벤트 세 개다.
	topPanelEventLines = 4
	// topPanelCandidateRows는 좁은 화면에서 패널 아래에 남기는 process 후보 줄 수다. 제목과 후보 세 개다.
	topPanelCandidateRows = 4
)

// parseTopPanels는 --panels와 defaults.top.panels의 값이다. none은 빈 목록이고, 나머지는 쉼표로 나눈 이름을 적은 순서대로 쓴다.
func parseTopPanels(value string) ([]topPanel, error) {
	if value == "none" {
		return []topPanel{}, nil
	}
	panels := []topPanel{}
	for _, part := range strings.Split(value, ",") {
		panel := topPanel(strings.TrimSpace(part))
		if !slices.Contains(topPanelNames, panel) {
			return nil, fmt.Errorf("unknown name %q", panel)
		}
		if slices.Contains(panels, panel) {
			return nil, fmt.Errorf("duplicate name %q", panel)
		}
		panels = append(panels, panel)
	}
	return panels, nil
}

// parseTopView는 --view와 defaults.top.view의 값이다. process 보기는 --process가 열므로 받지 않는다.
func parseTopView(value string) (topView, error) {
	switch view := topView(value); view {
	case topViewAll, topViewCPU, topViewMemory, topViewDisk, topViewNetwork, topViewPressure, topViewSplit:
		return view, nil
	}
	return "", fmt.Errorf("unknown view %q", value)
}

// parseTopProcessSort는 defaults.top.process_sort의 값이다. 패널 순위는 보기 값으로 나타낸다.
func parseTopProcessSort(value string) (topView, error) {
	switch value {
	case "cpu":
		return topViewCPU, nil
	case "mem":
		return topViewMemory, nil
	case "io":
		return topViewDisk, nil
	}
	return "", fmt.Errorf("unknown sort %q", value)
}

// customFooter는 아래 영역을 패널 목록으로 그리는지다. 목록을 정하지 않고 AI도 켜지 않았으면 기존 배치를 그대로 쓴다.
func (model topModel) customFooter() bool {
	return model.panelsSet || model.aiOn
}

// footerPanels는 지금 그릴 패널이다. 목록이 없으면 기존 배치를 흉내 낸다: network 보기의 net 상세, 그 뒤 events.
// a로 켠 AI가 목록에 없으면 맨 뒤에 붙는다.
func (model topModel) footerPanels() []topPanel {
	var panels []topPanel
	if model.panelsSet {
		for _, panel := range model.panels {
			if panel != topPanelAI || model.aiOn {
				panels = append(panels, panel)
			}
		}
	} else {
		if model.view == topViewNetwork && model.previous.NetworkHealth != nil {
			panels = append(panels, topPanelNet)
		}
		if !model.processFilter.active() && model.view != topViewSplit {
			panels = append(panels, topPanelEvents)
		}
	}
	if model.aiOn && !slices.Contains(panels, topPanelAI) {
		panels = append(panels, topPanelAI)
	}
	return panels
}

func (model topModel) panelBlock(panel topPanel, width int) []string {
	var lines []string
	switch panel {
	case topPanelNet:
		row, ok := model.selectedRow()
		if !ok {
			return nil
		}
		lines = append([]string{"network " + row.at.Format("15:04:05") + " · current namespace · i select"}, networkHealthLines(row.rate.NetworkHealth)...)
	case topPanelEvents:
		return model.eventLines(width, topPanelEventLines)
	case topPanelAI:
		lines = model.aiPanelLines()
	}
	if len(lines) > 0 {
		lines[0] = topLead(lines[0], model.limits.color)
	}
	return lines
}

// footerRoom은 아래 영역이 쓸 수 있는 줄 수다. 표나 박스가 최소 행을 먼저 갖는다. 크기를 모르면 제한하지 않는다.
func (model topModel) footerRoom() int {
	if model.height <= 0 {
		return 1 << 20
	}
	room := model.height - 1 - len(model.processBanner())
	if model.eventTicker() != "" {
		room--
	}
	if model.view == topViewSplit {
		rows := model.splitRows(false)
		if compact := model.splitRows(true); compact != nil && (rows == nil || len(compact) < len(rows)) {
			rows = compact
		}
		if rows != nil {
			return room - 1 - len(rows)*(topSplitBoxChrome+topSplitMinRows)
		}
		model = model.splitFallback()
	}
	return room - len(model.tableHeader()) - topPanelHistoryRows
}

// panelPlan은 room 줄 안에 들어가는 패널을 우선순위대로 고른다. 들어가지 않는 패널은 자르지 않고 통째로 숨기며,
// 뒤의 작은 패널은 남은 자리에 들어가면 보인다. Enter 상세와 h 최고치는 열려 있는 동안 패널 자리를 대신 쓴다.
func (model topModel) panelPlan(width, room int) (lines []string, hidden []topPanel) {
	if model.detail || model.peaks {
		lines = model.infoLines()
		return lines[:min(len(lines), max(0, room))], nil
	}
	if model.lastErr != nil {
		lines = []string{model.sampleErrorLine()}
	}
	for _, panel := range model.footerPanels() {
		block := model.panelBlock(panel, width)
		if len(lines)+len(block) > room {
			hidden = append(hidden, panel)
			continue
		}
		lines = append(lines, block...)
	}
	return lines[:min(len(lines), max(0, room))], hidden
}

// customFooterLines는 패널 목록으로 그리는 아래 영역이다. 넓은 화면은 왼쪽에 패널과 안내, 오른쪽에 process 후보를 둔다.
// 좁은 화면은 패널, 후보, 안내 순으로 쌓는다. 숨긴 패널은 안내 줄에 알린다. 알림 때문에 안내가 두 줄이 되면 알리지 않는다.
func (model topModel) customFooterLines() []string {
	room := model.footerRoom()
	if model.displayWidth() < topFooterDockWidth || model.processFilter.active() {
		status := model.statusLines()
		candidates := model.candidateLines()
		panels, hidden := model.panelPlan(model.displayWidth(), room-len(status)-min(len(candidates), topPanelCandidateRows))
		candidates = candidates[:min(len(candidates), max(0, room-len(status)-len(panels)))]
		lines := append(panels, candidates...)
		for index, line := range lines {
			lines[index] = topDashboardFitWidth(line, model.displayWidth())
		}
		return append(lines, model.statusWithHidden(hidden, len(status))...)
	}
	processWidth := model.footerProcessWidth()
	leftWidth := model.displayWidth() - topFooterGap - processWidth
	left, right := model, model
	left.width, right.width = leftWidth, processWidth
	status := left.statusLines()
	info, hidden := left.panelPlan(leftWidth, room-len(status))
	status = left.statusWithHidden(hidden, len(status))
	candidates := right.candidateLines()
	candidates = candidates[:min(len(candidates), max(len(status), room))]
	rows := max(len(info)+len(status), len(candidates))
	lines := make([]string, rows)
	for index := range lines {
		var leftLine, rightLine string
		if index < len(info) {
			leftLine = info[index]
		}
		if offset := index - (rows - len(status)); offset >= 0 {
			leftLine = status[offset]
		}
		if index < len(candidates) {
			rightLine = candidates[index]
		}
		lines[index] = topDashboardFitWidth(leftLine, leftWidth) + strings.Repeat(" ", topFooterGap) + topDashboardFitWidth(rightLine, processWidth)
	}
	return lines
}

func (model topModel) statusWithHidden(hidden []topPanel, count int) []string {
	if len(hidden) == 0 {
		return model.statusLines()
	}
	model.hiddenPanels = hidden
	if status := model.statusLines(); len(status) == count {
		return status
	}
	model.hiddenPanels = nil
	return model.statusLines()
}

// hiddenText는 안내 줄 앞에 붙는 숨긴 패널이다. +ai hidden처럼 쓴다.
func (model topModel) hiddenText() string {
	if len(model.hiddenPanels) == 0 {
		return ""
	}
	names := make([]string, len(model.hiddenPanels))
	for index, panel := range model.hiddenPanels {
		names[index] = string(panel)
	}
	return "+" + strings.Join(names, ",") + " hidden"
}

// topLayout은 --view, --panels와 defaults.top이 고른 화면 배치다.
type topLayout struct {
	view        topView
	panels      []topPanel
	panelsSet   bool
	processSort topView
}

// withLayout은 withSplit 뒤에 부른다. view=network와 split=cpu,net을 함께 쓰면 network 보기로 열고 v가 그 박스를 연다.
// process 필터가 있으면 process 보기를 그대로 둔다.
func (model topModel) withLayout(layout topLayout) topModel {
	model.startView, model.processSort = layout.view, layout.processSort
	model.panels, model.panelsSet = layout.panels, layout.panelsSet
	model.aiOn = slices.Contains(layout.panels, topPanelAI)
	if model.processFilter.active() || layout.view == "" {
		return model
	}
	if layout.view == topViewSplit {
		return model.enterSplit()
	}
	model.view = layout.view
	return model
}
