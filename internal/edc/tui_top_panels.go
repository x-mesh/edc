package edc

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// topPanel은 아래 왼쪽 칸에 늘 보이는 패널이다. --panels와 defaults.top.panels가 고르고, 적은 순서가 우선순위다.
type topPanel string

const (
	topPanelCPU    topPanel = "cpu"
	topPanelMemory topPanel = "mem"
	topPanelDisk   topPanel = "disk"
	topPanelNet    topPanel = "net"
	topPanelPSI    topPanel = "psi"
	topPanelAI     topPanel = "ai"
	topPanelEvents topPanel = "events"
)

// topPanelNames는 자원 패널을 박스와 같은 순서로 둔 뒤 ai와 events를 붙인다.
var topPanelNames = []topPanel{topPanelCPU, topPanelMemory, topPanelDisk, topPanelNet, topPanelPSI, topPanelAI, topPanelEvents}

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
	case topPanelCPU, topPanelMemory, topPanelDisk, topPanelPSI:
		lines = model.resourcePanelLines(panel)
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
	lines, _ := model.customFooterPlan()
	return lines
}

// customFooterPlan은 아래 영역과 숨긴 패널이다. edc top setup은 숨긴 패널을 설정 줄 옆에 알린다.
func (model topModel) customFooterPlan() ([]string, []topPanel) {
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
		return append(lines, model.statusWithHidden(hidden, len(status))...), hidden
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
	return lines, hidden
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

// topLayout은 --view, --panels, --columns와 defaults.top이 고른 화면 배치다.
type topLayout struct {
	view        topView
	panels      []topPanel
	panelsSet   bool
	processSort topView
	columns     []topColumnGroup
}

// withLayout은 withSplit 뒤에 부른다. view=network와 split=cpu,net을 함께 쓰면 network 보기로 열고 v가 그 박스를 연다.
// process 필터가 있으면 process 보기를 그대로 둔다.
func (model topModel) withLayout(layout topLayout) topModel {
	model.startView, model.processSort, model.columnGroups = layout.view, layout.processSort, layout.columns
	model.panels, model.panelsSet = layout.panels, layout.panelsSet
	model.aiOn = slices.Contains(layout.panels, topPanelAI)
	model.aiTokens = slices.ContainsFunc(layout.columns, func(group topColumnGroup) bool { return group.name == topAIColumnGroup })
	if model.processFilter.active() || layout.view == "" {
		return model
	}
	if layout.view == topViewSplit {
		return model.enterSplit()
	}
	model.view = layout.view
	return model
}

// topColumnGroup은 defaults.top.columns의 한 항목이다. name은 all 보기의 group 이름이고, detail이면 이름 뒤에 +를 적은 것이다.
type topColumnGroup struct {
	name   string
	detail bool
}

// topColumnGroupNames는 --columns에 쓰는 이름과 all 보기의 group 이름이다. 이름은 --split 박스와 같다.
// ai는 all에 들지 않는다. 고를 때만 Claude와 Codex 대화 기록을 읽는다.
var topColumnGroupNames = []struct{ name, group string }{
	{"net", "network"}, {"cpu", "cpu"}, {"mem", "mem"}, {"disk", "disk"}, {"psi", "psi"}, {"ai", topAIColumnGroup},
}

// topAIColumnGroup은 --columns의 ai가 가리키는 그룹이다. 표에서는 claude와 codex 두 그룹으로 나뉘어
// 그룹 머리줄이 도구 이름을 보인다. 열 이름에 cl, cx를 붙이면 넓은 화면에서 어느 도구인지 알아보기 어렵다.
const topAIColumnGroup = "ai"

// topAIColumns는 직전 행부터 이 행까지 Claude와 Codex에 들어온 요청 수와 token이다. token은 edc ai의 total과 같은
// in + out이고, cache 읽기는 대화가 길수록 요청마다 커져 활동량을 가리므로 넣지 않는다. 한 행에서 궁금한 것은 요청이
// 있었는지와 그 크기라서 in과 out을 나누지 않는다.
var topAIColumns = []topAllColumn{
	{group: "claude", title: "req", width: 5, tokens: func(tokens topAITokens) string { return topAITokenCell(tokens.claudeOK, tokens.claude.Requests) }},
	{group: "claude", title: "tok", width: 6, tokens: func(tokens topAITokens) string { return topAITokenCell(tokens.claudeOK, tokens.claude.total()) }},
	{group: "codex", title: "req", width: 5, tokens: func(tokens topAITokens) string { return topAITokenCell(tokens.codexOK, tokens.codex.Requests) }},
	{group: "codex", title: "tok", width: 6, tokens: func(tokens topAITokens) string { return topAITokenCell(tokens.codexOK, tokens.codex.total()) }},
}

// topAITokenCell은 0을 ·로 쓴다. 대부분의 행이 0이라 요청이 들어온 행이 눈에 띄어야 한다.
func topAITokenCell(ok bool, value int64) string {
	switch {
	case !ok:
		return "—"
	case value == 0:
		return "·"
	}
	return aiCompact(value)
}

// topAllColumnGroups는 all이 뜻하는 다섯 그룹이다. ai는 들지 않는다.
func topAllColumnGroups() []topColumnGroup {
	var groups []topColumnGroup
	for _, entry := range topColumnGroupNames {
		if entry.group != topAIColumnGroup {
			groups = append(groups, topColumnGroup{name: entry.group})
		}
	}
	return groups
}

func topColumnGroupName(group string) string {
	for _, entry := range topColumnGroupNames {
		if entry.group == group {
			return entry.name
		}
	}
	return group
}

// parseTopColumns는 --columns와 defaults.top.columns의 값이다. all은 기본 배치(nil)이고, 나머지는 쉼표로 나눈 그룹을
// 적은 순서대로 쓴다. 이름 뒤 +는 그 그룹의 열을 다른 그룹의 열보다 먼저 더한다.
func parseTopColumns(value string) ([]topColumnGroup, error) {
	if value == "all" {
		return nil, nil
	}
	var groups []topColumnGroup
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		name, detail := strings.CutSuffix(part, "+")
		index := slices.IndexFunc(topColumnGroupNames, func(entry struct{ name, group string }) bool { return entry.name == name })
		if index < 0 {
			return nil, fmt.Errorf("unknown name %q", part)
		}
		group := topColumnGroup{name: topColumnGroupNames[index].group, detail: detail}
		if slices.ContainsFunc(groups, func(seen topColumnGroup) bool { return seen.name == group.name }) {
			return nil, fmt.Errorf("duplicate name %q", name)
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// topAllGroupColumns는 고른 그룹의 all 열을 그룹 순서대로 모은다. +를 붙인 그룹의 추가 열은 원래 tier를 쓰고,
// 다른 그룹의 추가 열은 tier를 모든 tier 뒤로 미룬다. 폭이 남으면 +를 붙인 그룹이 먼저 열을 모두 보인다.
func topAllGroupColumns(groups []topColumnGroup) []topAllColumn {
	if len(groups) == 0 {
		return topAllColumns
	}
	shift := topAllMaxTier()
	var columns []topAllColumn
	for _, group := range groups {
		source, ai := topAllColumns, group.name == topAIColumnGroup
		if ai {
			source = topAIColumns
		}
		for _, column := range source {
			if !ai && column.group != group.name {
				continue
			}
			if column.tier > 0 && !group.detail {
				column.tier += shift
			}
			columns = append(columns, column)
		}
	}
	return columns
}

const (
	// topPanelTopProcesses는 자원 패널의 top 줄에 보이는 process 수다. 오른쪽 후보 목록과 달리 한 줄에 모은다.
	topPanelTopProcesses = 3
	// topPanelNameWidth는 top 줄의 process 이름 폭이다. 셋이 80열 한 줄에 들어간다.
	topPanelNameWidth = 14
)

// resourcePanelLines는 cpu, mem, disk, psi 패널이다. 첫 줄은 선택한 행의 요약, 둘째 줄은 그 행까지 60초의 최고치,
// 셋째 줄은 그 자원을 가장 많이 쓴 process다. 오른쪽 후보 목록은 기준 하나로만 줄 세우므로 자원마다 따로 보인다.
func (model topModel) resourcePanelLines(panel topPanel) []string {
	row, ok := model.selectedRow()
	if !ok {
		return []string{string(panel) + " · waiting for a sample"}
	}
	row = model.currentFilterRow(row)
	rate, at, linux := row.rate, row.at.Format("15:04:05"), model.details.System != "darwin"
	peak := func(label string, value func(resourceRate) (float64, bool), format string) string {
		best, when, found := model.panelPeak(row, value)
		if !found {
			return label + " —"
		}
		return label + " " + fmt.Sprintf(format, best) + " at " + when.Format("15:04:05")
	}
	switch panel {
	case topPanelCPU:
		head := fmt.Sprintf("cpu %s · load %.1f · usr %.1f%% sys %.1f%%", at, rate.Load1, rate.CPUUser, rate.CPUSystem)
		if linux {
			head += fmt.Sprintf(" io %.1f%%", rate.CPUIOWait)
		}
		if rate.CPUStealValid {
			head += fmt.Sprintf(" steal %.1f%%", rate.CPUSteal)
		}
		if rate.ProcsBlockedSource.known() {
			head += fmt.Sprintf(" · blocked %.0f", rate.ProcsBlocked)
		}
		// 1초 sample 사이에 끝나는 build process는 top 목록에 보이지 않아 생성 rate로만 드러난다.
		if rate.ProcsCreatedValid {
			head += fmt.Sprintf(" · spawn %.0f/s", rate.ProcsCreated)
		}
		cores := "  cores " + topCoreBar(rate.CoreCPU) + " · " + peak("peak 60s usr+sys", func(rate resourceRate) (float64, bool) { return rate.CPUUser + rate.CPUSystem, true }, "%.1f%%")
		if rate.ProcsCreatedValid {
			cores += " · " + peak("spawn", func(rate resourceRate) (float64, bool) { return rate.ProcsCreated, rate.ProcsCreatedValid }, "%.0f/s")
		}
		return []string{head, cores,
			"  top " + topPanelProcesses(row, topViewCPU, func(process topProcess) string { return fmt.Sprintf("%.0f%%", process.CPU) })}
	case topPanelMemory:
		head := fmt.Sprintf("mem %s · %.1f%% of %s · swap out %s/s", at, rate.MemoryPercent, topMemorySize(model.details.MemoryTotal), formatRate(rate.SwapOut))
		if percent, ok := topSwapPercent(rate); ok {
			head += fmt.Sprintf(" · swap %s/%s %.0f%%", formatProcessRSS(rate.SwapUsed), formatProcessRSS(rate.SwapTotal), percent)
		}
		switch {
		case rate.MemoryPressure.known():
			head += " · lvl " + rate.MemoryPressure.String()
		case rate.PSIValid:
			head += fmt.Sprintf(" · psi mem %.1f", rate.PSIMemory)
		}
		peaks := "  " + peak("peak 60s", func(rate resourceRate) (float64, bool) { return rate.MemoryPercent, true }, "%.1f%%") + " · " +
			peak("swap out", func(rate resourceRate) (float64, bool) { return rate.SwapOut / (1 << 20), true }, "%.2fM/s")
		if rate.CompressorValid {
			peaks += " · compressed " + formatProcessRSS(rate.Compressor)
		}
		return []string{head, peaks,
			"  top " + topPanelProcesses(row, topViewMemory, func(process topProcess) string { return formatProcessRSS(process.memory()) })}
	case topPanelDisk:
		head := fmt.Sprintf("disk %s · read %s/s write %s/s", at, formatRate(rate.DiskRead), formatRate(rate.DiskWrite))
		if rate.DiskHealthValid {
			head += fmt.Sprintf(" · iops %.0f · await %.1fms", rate.DiskIOPS, rate.DiskAwait)
		}
		if rate.DiskBusyValid {
			head += fmt.Sprintf(" · busy %.0f%% queue %.1f", rate.DiskBusy, rate.DiskQueue)
		}
		return []string{head,
			"  " + peak("peak 60s await", func(rate resourceRate) (float64, bool) { return rate.DiskAwait, rate.DiskHealthValid }, "%.1fms") + " · " +
				peak("iops", func(rate resourceRate) (float64, bool) { return rate.DiskIOPS, rate.DiskHealthValid }, "%.0f"),
			"  top i/o " + topPanelDiskProcesses(row)}
	}
	if !rate.PSIValid {
		if !linux {
			return []string{"psi · Linux only · mem shows the macOS memory level"}
		}
		return []string{"psi " + at + " · unavailable"}
	}
	head := fmt.Sprintf("psi %s · some cpu %.1f mem %.1f io %.1f · full mem %.1f io %.1f", at, rate.PSICPU, rate.PSIMemory, rate.PSIIO, rate.PSIMemoryFull, rate.PSIIOFull)
	var blocked []string
	for _, process := range row.processes {
		if process.State == topProcessStateBlocked && len(blocked) < topPanelTopProcesses {
			blocked = append(blocked, ansi.Truncate(topProcessName(process.Command, topPanelNameWidth), topPanelNameWidth, ""))
		}
	}
	waiting := "no process waits for I/O"
	if len(blocked) > 0 {
		waiting = "waiting for I/O: " + strings.Join(blocked, " · ")
	}
	return []string{head,
		"  " + peak("peak 60s io", func(rate resourceRate) (float64, bool) { return rate.PSIIO, rate.PSIValid }, "%.1f") + " · " +
			peak("mem", func(rate resourceRate) (float64, bool) { return rate.PSIMemory, rate.PSIValid }, "%.1f"),
		"  " + waiting}
}

// panelPeak은 row까지 60초 안의 가장 큰 값과 그 시각이다. h 패널처럼 지표마다 따로 찾는다.
func (model topModel) panelPeak(row topDashboardRow, value func(resourceRate) (float64, bool)) (float64, time.Time, bool) {
	var best float64
	var when time.Time
	found := false
	for _, candidate := range model.rows {
		if candidate.at.After(row.at) || row.at.Sub(candidate.at) > topPeakWindow {
			continue
		}
		if current, ok := value(candidate.rate); ok && (!found || current > best) {
			best, when, found = current, candidate.at, true
		}
	}
	return best, when, found
}

// topPanelProcesses는 rank 순서로 앞의 process 셋을 "이름 값"으로 잇는다.
func topPanelProcesses(row topDashboardRow, rank topView, value func(topProcess) string) string {
	if !row.processesValid || len(row.processes) == 0 {
		return "—"
	}
	ranked := topRankedProcesses(row.processes, rank)
	parts := make([]string, 0, topPanelTopProcesses)
	for _, process := range ranked[:min(topPanelTopProcesses, len(ranked))] {
		parts = append(parts, ansi.Truncate(topProcessName(process.Command, topPanelNameWidth), topPanelNameWidth, "")+" "+value(process))
	}
	return strings.Join(parts, " · ")
}

// topPanelDiskProcesses는 I/O를 구한 process가 없으면 이름을 —와 함께 늘어놓지 않고 그 이유를 한 줄로 보인다.
func topPanelDiskProcesses(row topDashboardRow) string {
	status := ""
	for _, process := range row.processes {
		if process.DiskValid {
			return topPanelProcesses(row, topViewDisk, func(process topProcess) string {
				if !process.DiskValid {
					return "—"
				}
				return formatRate(process.DiskRead+process.DiskWrite) + "/s"
			})
		}
		if status == "" {
			status = process.DiskStatus
		}
	}
	if status == "" {
		return "— no process I/O measured"
	}
	return "— no process I/O measured · " + status
}
