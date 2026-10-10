package edc

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// topViewSplit은 박스 화면이다. 박스 하나는 기존 보기 표 하나를 그대로 그린다.
const topViewSplit topView = "split"

var topSplitNames = []struct {
	name string
	view topView
}{
	{"cpu", topViewCPU}, {"mem", topViewMemory}, {"disk", topViewDisk}, {"net", topViewNetwork}, {"psi", topViewPressure},
}

func topSplitAll() []topView {
	views := make([]topView, len(topSplitNames))
	for index, entry := range topSplitNames {
		views[index] = entry.view
	}
	return views
}

func topSplitName(view topView) string {
	for _, entry := range topSplitNames {
		if entry.view == view {
			return entry.name
		}
	}
	return string(view)
}

// parseTopSplit은 --split과 defaults.top.split의 값이다. all은 전체, none은 nil이고, 나머지는 쉼표로 나눈 이름을 적은 순서대로 쓴다.
func parseTopSplit(value string) ([]topView, error) {
	switch value {
	case "all":
		return topSplitAll(), nil
	case "none":
		return nil, nil
	}
	var views []topView
	seen := map[topView]bool{}
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		found := false
		for _, entry := range topSplitNames {
			if entry.name == name {
				if seen[entry.view] {
					return nil, fmt.Errorf("duplicate name %q", name)
				}
				seen[entry.view] = true
				views, found = append(views, entry.view), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown name %q", name)
		}
	}
	return views, nil
}

const (
	// topSplitMinRows는 박스 하나가 보여야 하는 데이터 행 수다. 이보다 적으면 추세를 읽을 수 없다.
	topSplitMinRows = 3
	// topSplitBoxChrome는 데이터 행 말고 박스가 쓰는 줄이다: 위 테두리, header, 아래 테두리.
	topSplitBoxChrome = 3
	// topSplitWideWidth는 박스의 자연 폭을 잴 때 tableColumns가 열을 버리지 못하게 주는 폭이다.
	topSplitWideWidth = 1000
	// topSplitTimeWidth는 행 앞의 "15:04:05 │" 폭이다.
	topSplitTimeWidth = topSelectionColumn + 2
	topSplitPSINotice = "psi box is Linux-only · s shows macOS pressure"
)

// splitHidden은 박스에서 뺄 열이다. signal은 박스 아래 한 줄로 한 번만 보인다. load와 mem%는 그 값을 가진
// cpu나 mem 박스가 화면에 있을 때만 뺀다. 그 박스가 없으면 값이 화면에서 사라진다.
func (model topModel) splitHidden(title string) bool {
	switch title {
	case "signal":
		return true
	case "load":
		return model.view != topViewCPU && slices.Contains(model.splitViews(), topViewCPU)
	case "mem%":
		return model.view != topViewMemory && slices.Contains(model.splitViews(), topViewMemory)
	}
	return false
}

type topSplitBox struct {
	view  topView
	pane  topModel
	inner int
	// timeless는 줄의 첫 박스가 아닌 박스다. 같은 줄의 박스는 같은 시각의 행을 나란히 보이므로 시각 열을 첫 박스에만 둔다.
	timeless bool
}

func (model topModel) splitList() []topView {
	if len(model.split) == 0 {
		return topSplitAll()
	}
	return model.split
}

// splitViews는 실제로 그릴 박스다. macOS에는 PSI가 없고 memory 압박 단계는 mem 박스에 있다. 판단은 호스트 정보로만 한다.
func (model topModel) splitViews() []topView {
	var views []topView
	for _, view := range model.splitList() {
		if view == topViewPressure && model.details.System == "darwin" {
			continue
		}
		views = append(views, view)
	}
	return views
}

func (model topModel) withSplit(views []topView) topModel {
	model.split = views
	if model.processFilter.active() || len(views) == 0 {
		return model
	}
	return model.enterSplit()
}

func (model topModel) enterSplit() topModel {
	views := model.splitViews()
	if len(views) < len(model.splitList()) {
		model.notice = topSplitPSINotice
	}
	if len(views) == 0 {
		return model
	}
	model.view = topViewSplit
	return model
}

// splitBox는 박스의 자연 폭을 잰다. 큰 폭으로 그려 열을 모두 남기고, 그린 header 폭이 박스 안쪽 폭이다.
// tableColumns는 마지막 열까지 쓰고 나면 used가 안쪽 폭 + 1이므로, 렌더할 폭도 안쪽 폭 + 1로 둔다.
// 테두리를 더해 width보다 넓으면 단일 보기처럼 들어가지 않는 뒤쪽 열을 뺀다. 새 열은 뒤에 붙어 있어 먼저 빠진다.
func (model topModel) splitBox(view topView, width int, compact bool) topSplitBox {
	pane := model
	pane.view, pane.boxed, pane.compact, pane.width = view, true, compact, topSplitWideWidth
	inner := ansi.StringWidth(pane.tableHeader()[0])
	if inner+2 > width {
		pane.width = width - 1
		inner = ansi.StringWidth(pane.tableHeader()[0])
	}
	pane.width = inner + 1
	return topSplitBox{view: view, pane: pane, inner: inner}
}

// splitLayout은 박스를 줄에 나눈다. optional 칸 때문에 줄이 늘면 optional 칸을 뺀 배치를 쓴다.
// 줄이 늘면 박스마다 데이터 행이 줄어 보이는 시간 범위가 짧아진다.
// ok가 false이면 박스 데이터 행이 모자라거나 박스 하나가 터미널보다 넓은 것이다.
func (model topModel) splitLayout() (rows [][]topSplitBox, dataRows int, ok bool) {
	rows = model.splitRows(false)
	if compact := model.splitRows(true); compact != nil && (rows == nil || len(compact) < len(rows)) {
		rows = compact
	}
	if rows == nil {
		return nil, 0, false
	}
	available := model.height - 1 - len(model.processBanner()) - 1 - len(model.footerLines())
	dataRows = available/len(rows) - topSplitBoxChrome
	return rows, dataRows, dataRows >= topSplitMinRows
}

// splitRows는 박스를 순서대로 줄에 나눈다. 줄 수는 왼쪽부터 채울 때의 최소 줄 수이고, 그 줄 수 안에서
// 가장 넓은 줄이 가장 좁아지게 나눈다. 박스 줄들은 높이를 똑같이 나눠 가지므로 빈 폭이 한 줄에 몰리면 그 줄 전체가 빈다.
func (model topModel) splitRows(compact bool) [][]topSplitBox {
	width := model.displayWidth()
	var boxes []topSplitBox
	for _, view := range model.splitViews() {
		box := model.splitBox(view, width, compact)
		if box.inner+2 > width {
			return nil
		}
		boxes = append(boxes, box)
	}
	count, used := 0, 0
	for _, box := range boxes {
		if count > 0 && used+box.inner-topSplitTimeWidth+2 <= width {
			used += box.inner - topSplitTimeWidth + 2
			continue
		}
		count, used = count+1, box.inner+2
	}
	return topSplitBalance(boxes, count, width)
}

// topSplitRow는 boxes를 한 줄에 놓는다. 첫 박스만 시각 열을 둔다.
func topSplitRow(boxes []topSplitBox) ([]topSplitBox, int) {
	row := make([]topSplitBox, len(boxes))
	width := 0
	for index, box := range boxes {
		if index > 0 {
			box.inner, box.timeless = box.inner-topSplitTimeWidth, true
		}
		row[index] = box
		width += box.inner + 2
	}
	return row, width
}

// topSplitBalance는 boxes를 순서를 지켜 count줄로 나누는 방법 중 가장 넓은 줄이 가장 좁은 것을 고른다.
// 박스는 다섯 개 이하라 모든 경우를 따진다. 같으면 먼저 찾은 나눔, 곧 앞줄이 더 많은 나눔을 쓴다.
func topSplitBalance(boxes []topSplitBox, count, width int) [][]topSplitBox {
	var best [][]topSplitBox
	bestWidth := 0
	var walk func(start, widest int, rows [][]topSplitBox)
	walk = func(start, widest int, rows [][]topSplitBox) {
		if len(rows) == count {
			if start == len(boxes) && (best == nil || widest < bestWidth) {
				best, bestWidth = slices.Clone(rows), widest
			}
			return
		}
		for end := len(boxes); end > start; end-- {
			row, rowWidth := topSplitRow(boxes[start:end])
			if rowWidth <= width {
				walk(end, max(widest, rowWidth), append(rows, row))
			}
		}
	}
	walk(0, 0, nil)
	return best
}

// splitFallback은 박스를 그릴 수 없을 때 보이는 첫 박스의 단일 보기다.
func (model topModel) splitFallback() topModel {
	view := model.splitViews()[0]
	pane := model
	pane.view = view
	if pane.notice == "" {
		pane.notice = "split needs a larger terminal · showing " + topSplitName(view)
	}
	return pane
}

func (model topModel) splitBodyLines() int {
	_, dataRows, ok := model.splitLayout()
	if !ok {
		return model.splitFallback().bodyLines()
	}
	return dataRows
}

func (model topModel) splitView() tea.View {
	boxRows, dataRows, ok := model.splitLayout()
	if !ok {
		return model.splitFallback().View()
	}
	lines := append([]string{model.dashboardTitle()}, model.processBanner()...)
	start := max(0, len(model.rows)-dataRows)
	if !model.follow && len(model.rows) > dataRows {
		start = min(model.selected, len(model.rows)-dataRows)
	}
	for _, boxes := range boxRows {
		rendered := make([][]string, len(boxes))
		for index, box := range boxes {
			rendered[index] = model.splitBoxLines(box, start, dataRows)
		}
		for line := range rendered[0] {
			var joined strings.Builder
			for _, box := range rendered {
				joined.WriteString(box[line])
			}
			lines = append(lines, joined.String())
		}
	}
	lines = append(lines, model.splitSignalLine())
	footer := model.footerLines()
	lines = append(model.padToFooter(lines, footer), footer...)
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], model.displayWidth(), "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

// splitBoxLines는 박스 하나를 그린다. 선택 표시는 테두리와 강조를 입히기 전의 행에 넣는다.
func (model topModel) splitBoxLines(box topSplitBox, start, dataRows int) []string {
	title := "─ " + topSplitName(box.view) + " "
	lines := []string{"╭" + title + strings.Repeat("─", max(0, box.inner-ansi.StringWidth(title))) + "╮"}
	body := []string{box.trimTime(box.pane.tableHeader()[0])}
	for index := start; index < len(model.rows) && index < start+dataRows; index++ {
		line := box.pane.tableRow(model.currentFilterRow(model.rows[index]))
		if index == model.selected && !model.follow {
			line = box.trimTime(line[:topSelectionColumn] + ">" + line[topSelectionColumn+1:])
			if !model.processFocus && model.limits.color {
				line = topBannerStyle + strings.ReplaceAll(topDashboardFitWidth(line, box.inner), topColorReset, topColorReset+topBannerStyle) + topColorReset
			}
		} else {
			line = box.trimTime(line)
		}
		body = append(body, line)
	}
	for len(body) < dataRows+1 {
		body = append(body, "")
	}
	for _, line := range body {
		lines = append(lines, "│"+topDashboardFitWidth(line, box.inner)+"│")
	}
	return append(lines, "╰"+strings.Repeat("─", box.inner)+"╯")
}

// trimTime은 시각 열을 뺀다. 행은 색 없는 "15:04:05 │"로 시작하므로 첫 구분선까지 자른다.
func (box topSplitBox) trimTime(line string) string {
	if !box.timeless {
		return line
	}
	_, rest, _ := strings.Cut(line, "│")
	return rest
}

func (model topModel) splitSignalLine() string {
	row, ok := model.selectedRow()
	if !ok {
		return "signal · waiting for a sample"
	}
	row = model.currentFilterRow(row)
	prefix := "signal " + row.at.Format("15:04:05") + " · "
	items := topDashboardSignalItems(row.rate, row.processes, row.processTotal.Groups, row.processesValid, model.limits)
	return prefix + formatTopSignalsWidth(items, model.displayWidth()-ansi.StringWidth(prefix))
}
