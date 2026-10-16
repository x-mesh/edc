package edc

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// topInterfacePickerWidth는 상자 안쪽의 최대 폭이다. 가장 긴 interface 이름(15자)과 in/out 값이 한 줄에 들어간다.
const topInterfacePickerWidth = 60

// topInterfacePicker는 i로 여는 interface 선택 상자다. 줄 순서는 열 때 정한다. 열려 있는 동안 traffic 값만 바뀌므로 고르던 줄이
// 움직이지 않는다.
type topInterfacePicker struct {
	presets []topInterfacePreset
	rows    []topInterfacePickerRow
	checked map[string]bool
	// preset은 고른 프리셋 줄의 번호다. -1이면 체크한 이름 목록이 선택이다.
	preset int
	cursor int
	offset int
	notice string
}

// topInterfacePreset은 이름 목록이 아닌 선택이다. physical과 glob은 interface가 바뀌어도 따라가므로 이름으로 풀지 않는다.
type topInterfacePreset struct {
	label  string
	filter topInterfaceFilter
}

// footerOn은 키 안내에서 지금 고른 프리셋이다. a는 첫 프리셋(all)을, p는 physical을 고른다.
func (picker topInterfacePicker) footerOn() []string {
	switch {
	case picker.preset == 0:
		return []string{"a"}
	case picker.preset >= 0 && picker.preset == picker.presetIndex("physical"):
		return []string{"p"}
	}
	return nil
}

type topInterfacePickerRow struct {
	name     string
	physical bool
	up       bool
}

func newTopInterfacePicker(current topInterfaceFilter, list []topInterfaceInfo, rates []netInterfaceRate, physical func(string) bool, physicalSupported bool) *topInterfacePicker {
	traffic := map[string]float64{}
	for _, rate := range rates {
		traffic[rate.Name] = rate.In + rate.Out
	}
	picker := &topInterfacePicker{checked: map[string]bool{}, preset: -1}
	for _, item := range list {
		picker.rows = append(picker.rows, topInterfacePickerRow{name: item.name, physical: physical(item.name), up: item.up})
	}
	// 실제 회선인 interface를 먼저, 그다음 traffic이 많은 순으로 둔다. 고를 interface가 대개 위에 온다.
	sort.SliceStable(picker.rows, func(i, j int) bool {
		a, b := picker.rows[i], picker.rows[j]
		if a.physical != b.physical {
			return a.physical
		}
		if traffic[a.name] != traffic[b.name] {
			return traffic[a.name] > traffic[b.name]
		}
		return a.name < b.name
	})
	picker.presets = []topInterfacePreset{{label: "all"}}
	if physicalSupported {
		filter, _ := parseTopInterfaceFilter("physical")
		picker.presets = append(picker.presets, topInterfacePreset{label: "physical", filter: filter})
	}
	// --interface의 glob은 체크 목록으로 나타낼 수 없다. 프리셋으로 두어야 그대로 적용해도 지금 맞는 이름으로 굳지 않는다.
	if len(current.patterns) > 0 || (current.physical && len(current.names) > 0) {
		picker.presets = append(picker.presets, topInterfacePreset{label: current.String(), filter: current})
	}
	for index, preset := range picker.presets {
		if preset.filter.String() == current.String() {
			picker.preset = index
		}
	}
	for _, row := range picker.rows {
		picker.checked[row.name] = current.matches(row.name, func(string) bool { return row.physical })
	}
	if picker.preset >= 0 {
		picker.cursor = picker.preset
	} else if index := slices.IndexFunc(picker.rows, func(row topInterfacePickerRow) bool { return picker.checked[row.name] }); index >= 0 {
		picker.cursor = len(picker.presets) + index
	}
	return picker
}

func (picker *topInterfacePicker) presetIndex(label string) int {
	return slices.IndexFunc(picker.presets, func(preset topInterfacePreset) bool { return preset.label == label })
}

func (picker *topInterfacePicker) selectPreset(index int) {
	picker.preset = index
	filter := picker.presets[index].filter
	for _, row := range picker.rows {
		picker.checked[row.name] = filter.matches(row.name, func(string) bool { return row.physical })
	}
}

// toggle은 프리셋을 풀고 이름 목록으로 바꾼다. 프리셋이 맞춘 체크에서 시작하므로 physical에서 하나만 빼기도 쉽다.
func (picker *topInterfacePicker) toggle(index int) {
	picker.preset = -1
	name := picker.rows[index].name
	picker.checked[name] = !picker.checked[name]
}

func (picker *topInterfacePicker) activate() {
	if picker.cursor < len(picker.presets) {
		picker.selectPreset(picker.cursor)
		return
	}
	if index := picker.cursor - len(picker.presets); index < len(picker.rows) {
		picker.toggle(index)
	}
}

// value는 적용할 --interface 값이다. 체크한 것이 없으면 false다. 아무 interface도 더하지 않는 선택은 쓸 데가 없다.
func (picker *topInterfacePicker) value() (string, bool) {
	if picker.preset >= 0 {
		return picker.presets[picker.preset].filter.String(), true
	}
	var names []string
	for _, row := range picker.rows {
		if picker.checked[row.name] {
			names = append(names, row.name)
		}
	}
	return strings.Join(names, ","), len(names) > 0
}

// visibleRows는 height인 화면에서 상자가 보일 interface 줄 수다. 위아래로 한 줄씩 대시보드가 보이게 남긴다.
func (picker *topInterfacePicker) visibleRows(height int) int {
	fixed := len(picker.presets) + 5
	return max(1, min(len(picker.rows), height-2-fixed))
}

func (picker *topInterfacePicker) move(delta, visible int) {
	total := len(picker.presets) + len(picker.rows)
	picker.cursor = max(0, min(total-1, picker.cursor+delta))
	if row := picker.cursor - len(picker.presets); row >= 0 {
		if row < picker.offset {
			picker.offset = row
		} else if row >= picker.offset+visible {
			picker.offset = row - visible + 1
		}
	}
}

// lines는 상자다. 색을 쓰면 테두리와 키 안내는 회색, 제목은 자홍 굵게, 커서 줄은 chip 바탕이다.
func (picker *topInterfacePicker) lines(rates []netInterfaceRate, width, height int, color bool) []string {
	byName := map[string]netInterfaceRate{}
	for _, rate := range rates {
		byName[rate.Name] = rate
	}
	inner := max(16, min(topInterfacePickerWidth, width-4))
	frame := func(left, fill, right, title string) string {
		title = ansi.Truncate(title, inner, "")
		rest := strings.Repeat(fill, inner-ansi.StringWidth(title)) + right
		if color && title != "" {
			name, hint, _ := strings.Cut(strings.TrimPrefix(title, " "), " ")
			return richDim(left+" ") + richPaint(name, richMagenta, true) + richDim(" "+hint+rest)
		}
		return topDim(left+title+rest, color)
	}
	side := topDim("│", color)
	padStyled := func(text string, style func(string) string) string {
		text = ansi.Truncate(text, inner, "…")
		return side + style(text+strings.Repeat(" ", inner-ansi.StringWidth(text))) + side
	}
	plain := func(text string) string { return text }
	pad := func(text string) string { return padStyled(text, plain) }
	// row는 고를 수 있는 줄이다. 커서가 있는 줄은 chip 바탕으로 칠한다.
	row := func(index int, text string) string {
		if color && index == picker.cursor {
			return padStyled(text, func(text string) string { return topHighlight(text, inner) })
		}
		return pad(text)
	}
	dim := func(text string) string {
		return padStyled(text, func(text string) string { return topDim(text, color) })
	}
	marker := func(index int) string {
		if index == picker.cursor {
			return "›"
		}
		return " "
	}
	lines := []string{frame("┌", "─", "┐", " interfaces · net columns add the checked ones ")}
	for index, preset := range picker.presets {
		radio := "( )"
		if picker.preset == index {
			radio = "(•)"
		}
		lines = append(lines, row(index, fmt.Sprintf("%s%s %-12s %s", marker(index), radio, preset.label, picker.presetDetail(preset))))
	}
	lines = append(lines, frame("├", "─", "┤", ""))
	visible := picker.visibleRows(height)
	if len(picker.rows) == 0 {
		lines = append(lines, dim("  no interface other than loopback"))
	}
	for index := picker.offset; index < len(picker.rows) && index < picker.offset+visible; index++ {
		item := picker.rows[index]
		box, kind, state := "[ ]", "virt", ""
		if picker.checked[item.name] {
			box = "[x]"
		}
		if item.physical {
			kind = "phys"
		}
		if !item.up {
			state = "  down"
		}
		rate := byName[item.name]
		lines = append(lines, row(len(picker.presets)+index, fmt.Sprintf("%s%s %-15s %s  in %5s out %5s%s", marker(len(picker.presets)+index), box, item.name, kind, formatRate(rate.In), formatRate(rate.Out), state)))
	}
	var more []string
	if picker.offset > 0 {
		more = append(more, fmt.Sprintf("↑ %d above", picker.offset))
	}
	if rest := len(picker.rows) - picker.offset - visible; rest > 0 {
		more = append(more, fmt.Sprintf("↓ %d more", rest))
	}
	lines = append(lines, dim("  "+strings.Join(more, " · ")))
	footer := "Space check · a all · Enter apply · Esc cancel"
	if picker.presetIndex("physical") >= 0 {
		footer = "Space check · a all · p physical · Enter apply · Esc cancel"
	}
	if picker.notice != "" {
		footer = picker.notice
	}
	paint := func(text string) string { return liveKeyHints(text, color, picker.footerOn()...) }
	if picker.notice != "" {
		paint = func(text string) string { return topTint(text, topLevelWarn, color) }
	}
	footerLine := padStyled(" "+footer, paint)
	return append(lines, footerLine, frame("└", "─", "┘", ""))
}

func (picker *topInterfacePicker) presetDetail(preset topInterfacePreset) string {
	if !preset.filter.active() {
		return "every interface but lo"
	}
	var names []string
	for _, row := range picker.rows {
		if preset.filter.matches(row.name, func(string) bool { return row.physical }) {
			names = append(names, row.name)
		}
	}
	if len(names) == 0 {
		return "none now"
	}
	return strings.Join(names, " ")
}

// openInterfacePicker는 i로 상자를 연다. 기록 중에 바꾸면 한 run의 net 값이 서로 다른 interface의 합이 되므로 열지 않는다.
func (model topModel) openInterfacePicker() (tea.Model, tea.Cmd) {
	if model.record != nil {
		model.notice = T("observe.top.interface_recording")
		return model, nil
	}
	list, err := model.listInterfaces()
	if err != nil {
		model.notice = err.Error()
		return model, nil
	}
	model.picker = newTopInterfacePicker(model.interfaces, list, model.interfaceRates, model.interfacePhysical, model.physicalSupported)
	return model, nil
}

func (model topModel) updatePicker(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	picker := model.picker
	picker.notice = ""
	visible := picker.visibleRows(model.height)
	switch key.String() {
	case "ctrl+c", "q":
		return model, tea.Quit
	case "esc", "i":
		model.picker = nil
	case "up", "k":
		picker.move(-1, visible)
	case "down", "j":
		picker.move(1, visible)
	case "pgup":
		picker.move(-visible, visible)
	case "pgdown":
		picker.move(visible, visible)
	case "space", " ":
		picker.activate()
	case "a":
		picker.selectPreset(0)
	case "p":
		if index := picker.presetIndex("physical"); index >= 0 {
			picker.selectPreset(index)
		}
	case "enter":
		value, ok := picker.value()
		if !ok {
			picker.notice = "check at least one interface"
			return model, nil
		}
		model.picker = nil
		return model.applyInterfaces(value)
	}
	return model, nil
}

// overlayTopBox는 상자를 화면 가운데에 겹친다. 상자 밖의 글자는 그대로 둔다. 색을 쓰면 상자 앞에서 색을 끊어, 상자가 아래 표의
// 색을 물려받지 않게 한다.
func overlayTopBox(content string, box []string, width int, color bool) string {
	if len(box) == 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	boxWidth := ansi.StringWidth(box[0])
	top := max(0, (len(lines)-len(box))/2)
	left := max(0, (width-boxWidth)/2)
	for len(lines) < top+len(box) {
		lines = append(lines, "")
	}
	reset := ""
	if color {
		reset = richReset
	}
	for index, row := range box {
		line := lines[top+index]
		if gap := left + boxWidth - ansi.StringWidth(line); gap > 0 {
			line += strings.Repeat(" ", gap)
		}
		lines[top+index] = ansi.Truncate(line, left, "") + reset + row + ansi.TruncateLeft(line, left+boxWidth, "")
	}
	return strings.Join(lines, "\n")
}
