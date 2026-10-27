package edc

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// viewKeepLines는 화면에 보관하는 줄 수의 기본값이다. trace 화면이 event를 10,000건까지 보관하는 것과 같다.
// 통계도 이 줄들로 센다.
const viewKeepLines = 10000

const (
	// viewPanelWidth는 오른쪽 통계 패널의 최대 폭이다. 실제 폭은 내용에 맞춘다.
	viewPanelWidth = 38
	// viewPanelSideWidth는 통계 패널을 오른쪽에 두는 최소 화면 폭이다. 그보다 좁으면 목록 아래에 둔다.
	viewPanelSideWidth = 120
)

type viewDataMsg struct {
	event viewFollowEvent
	// eof는 stdin이 끝났다는 뜻이다. 파일은 끝나지 않고 계속 따라간다.
	eof bool
}

type viewTickMsg time.Time

type viewScreenModel struct {
	title  string
	format viewFormat
	// formatFixed는 형식을 정할 만큼 줄을 읽었다는 뜻이다. 그 전에는 줄이 들어올 때마다 다시 정한다.
	formatFixed bool
	lines       []viewLine
	// first는 lines[0]의 번호다. 오래된 줄을 버려도 고른 줄을 번호로 찾는다.
	first   int
	keep    int
	pending []byte
	stats   viewStats
	// selected는 고른 줄의 번호다. -1이면 맨 아래를 따라간다.
	selected  int
	filter    string
	filtering bool
	input     textinput.Model
	panel     bool
	// detail은 상세 화면에 연 줄의 번호다. -1이면 목록이다.
	detail       int
	detailOffset int
	// compact는 access 줄을 필드 열로 그리고 값이 모두 같은 열을 숨긴다. c로 원문과 오간다.
	compact bool
	// scroll은 목록을 왼쪽으로 민 칸 수다.
	scroll  int
	width   int
	height  int
	changed time.Time
	ended   bool
	err     error
	now     time.Time
}

func newViewScreenModel(title string, keep int) viewScreenModel {
	input := textinput.New()
	input.SetStyles(traceInputStyles())
	input.Prompt = "filter / "
	input.Placeholder = "text"
	input.CharLimit = 256
	input.SetWidth(48)
	return viewScreenModel{
		title:    title,
		format:   viewFormatPlain,
		keep:     max(1, keep),
		stats:    newViewStats(),
		selected: -1,
		detail:   -1,
		panel:    true,
		compact:  true,
		input:    input,
		width:    80,
		height:   24,
		now:      time.Now(),
	}
}

func viewTick() tea.Cmd {
	return tea.Tick(time.Second, func(at time.Time) tea.Msg { return viewTickMsg(at) })
}

func (model viewScreenModel) Init() tea.Cmd { return viewTick() }

func (model viewScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.input.SetWidth(max(20, min(60, value.Width-12)))
		return model, nil
	case viewTickMsg:
		model.now = time.Time(value)
		return model, viewTick()
	case viewDataMsg:
		model.receive(value, time.Now())
		return model, nil
	case tea.KeyPressMsg:
		return model.updateKey(value)
	}
	return model, nil
}

// receive는 따라가며 받은 바이트, 파일 교체, 끝을 화면 줄에 반영한다.
func (model *viewScreenModel) receive(msg viewDataMsg, now time.Time) {
	model.now = now
	switch {
	case msg.event.err != nil:
		model.err = msg.event.err
	case msg.event.reset != "":
		model.flushPending(now)
		model.pending = nil
		model.appendLines([]viewLine{{marker: true, text: viewResetText(msg.event.reset)}})
		model.changed = now
	case len(msg.event.data) != 0:
		var lines []string
		lines, model.pending = viewSplitLines(model.pending, msg.event.data)
		model.ingest(lines, now, true)
		if len(model.pending) > viewPendingLimit {
			model.flushPending(now)
		}
		model.changed = now
	}
	if msg.eof {
		model.flushPending(now)
		model.ended = true
	}
}

func viewResetText(reset string) string {
	if reset == viewResetTruncated {
		return "— file truncated, reading from the start"
	}
	return "— file replaced, reading the new file from the start"
}

// flushPending은 줄바꿈 없이 남은 마지막 조각을 한 줄로 넣는다. 입력이 끝났거나 파일이 바뀌어 더 이어질 수 없을 때 부른다.
func (model *viewScreenModel) flushPending(now time.Time) {
	if len(model.pending) == 0 {
		return
	}
	model.ingest([]string{string(model.pending)}, now, true)
	model.pending = nil
}

// ingest는 raw 줄을 읽어 붙인다. arrived가 false면 화면을 열 때 읽은 줄이라 초당 줄 수에 넣지 않는다.
func (model *viewScreenModel) ingest(raw []string, now time.Time, arrived bool) {
	if len(raw) == 0 {
		return
	}
	if !model.formatFixed {
		model.redetect(raw)
	}
	parsed := make([]viewLine, 0, len(raw))
	previous := model.lastLine()
	for _, text := range raw {
		line := parseViewLine(text, model.format, previous)
		parsed = append(parsed, line)
		previous = &parsed[len(parsed)-1]
	}
	model.appendLines(parsed)
	if arrived {
		model.stats.arrived(now, len(raw))
	}
}

// redetect는 처음 줄들로 형식을 다시 정하고, 형식이 바뀌면 보관한 줄을 다시 읽는다. 빈 파일을 열고 나중에 줄이 쌓이는
// 경우에도 형식을 맞춘다.
func (model *viewScreenModel) redetect(raw []string) {
	sample := make([]string, 0, viewDetectLines)
	for _, line := range model.lines {
		if !line.marker && len(sample) < viewDetectLines {
			sample = append(sample, line.text)
		}
	}
	for _, line := range raw {
		if len(sample) < viewDetectLines {
			sample = append(sample, line)
		}
	}
	model.formatFixed = len(sample) >= viewDetectLines
	format := detectViewFormat(sample)
	if format == model.format {
		return
	}
	model.format = format
	old := model.lines
	arrivals := model.stats.arrivals
	model.lines, model.stats = nil, newViewStats()
	model.stats.arrivals = arrivals
	var previous *viewLine
	for _, line := range old {
		if !line.marker {
			line = parseViewLine(line.text, format, previous)
		}
		model.lines = append(model.lines, line)
		model.stats.add(line)
		previous = &model.lines[len(model.lines)-1]
	}
}

func (model viewScreenModel) lastLine() *viewLine {
	for index := len(model.lines) - 1; index >= 0; index-- {
		if !model.lines[index].marker {
			return &model.lines[index]
		}
	}
	return nil
}

func (model *viewScreenModel) appendLines(lines []viewLine) {
	for _, line := range lines {
		model.stats.add(line)
	}
	model.lines = append(model.lines, lines...)
	if drop := len(model.lines) - model.keep; drop > 0 {
		for _, line := range model.lines[:drop] {
			model.stats.remove(line)
		}
		model.lines = model.lines[drop:]
		model.first += drop
	}
	if model.selected >= 0 && model.selected < model.first {
		model.selected = model.first
	}
	if model.detail >= 0 && model.detail < model.first {
		model.detail = -1
	}
}

func (model viewScreenModel) last() int { return model.first + len(model.lines) - 1 }

func (model viewScreenModel) line(number int) (viewLine, bool) {
	if number < model.first || number > model.last() {
		return viewLine{}, false
	}
	return model.lines[number-model.first], true
}

func (model viewScreenModel) matches(line viewLine) bool {
	if model.filter == "" || line.marker {
		return true
	}
	return strings.Contains(strings.ToLower(line.text), strings.ToLower(model.filter))
}

// shown은 filter에 맞는 줄 번호다.
func (model viewScreenModel) shown() []int {
	numbers := make([]int, 0, len(model.lines))
	for index, line := range model.lines {
		if model.matches(line) {
			numbers = append(numbers, model.first+index)
		}
	}
	return numbers
}

func (model viewScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		return model, tea.Quit
	}
	if model.detail >= 0 {
		return model.updateDetailKey(key.String())
	}
	if model.filtering {
		switch key.String() {
		case "enter":
			model.filter = strings.TrimSpace(model.input.Value())
			model.input.Blur()
			model.filtering = false
			model.selected = -1
			return model, nil
		case "esc":
			model.input.SetValue(model.filter)
			model.input.Blur()
			model.filtering = false
			return model, nil
		}
		var cmd tea.Cmd
		model.input, cmd = model.input.Update(key)
		return model, cmd
	}
	switch key.String() {
	case "/":
		model.input.SetValue(model.filter)
		model.filtering = true
		return model, model.input.Focus()
	case "up", "k", "down", "j", "pgup", "pgdown", "b", "space", "home":
		// 고르는 동안에는 목록이 새 줄을 따라가지 않는다. end나 l을 누르면 다시 따라간다.
		step, count, page := 1, 1, max(1, model.listRows()-1)
		switch key.String() {
		case "up", "k":
			step = -1
		case "pgup", "b":
			step, count = -1, page
		case "pgdown", "space":
			count = page
		case "home":
			step, count = -1, len(model.lines)
		}
		model.selected = model.moveSelection(step, count)
		return model, nil
	case "end", "l":
		model.selected = -1
		return model, nil
	case "esc":
		// Mac 자판에는 End가 없는 경우가 많다. 고른 줄이 있으면 Esc가 먼저 선택을 풀고 다시 따라간다.
		if model.selected >= 0 {
			model.selected = -1
			return model, nil
		}
		model.filter = ""
		return model, nil
	case "i":
		model.panel = !model.panel
		return model, nil
	case "c":
		model.compact = !model.compact
		return model, nil
	case "left":
		model.scroll = max(0, model.scroll-viewScrollStep)
		return model, nil
	case "right":
		model.scroll += viewScrollStep
		return model, nil
	case "enter":
		number := model.selected
		if number < 0 {
			shown := model.shown()
			if len(shown) == 0 {
				return model, nil
			}
			number = shown[len(shown)-1]
		}
		model.detail, model.detailOffset = number, 0
		return model, nil
	case "q":
		return model, tea.Quit
	}
	return model, nil
}

// moveSelection은 filter에 맞는 줄 사이에서 고른 줄을 옮긴다. 따라가던 중이면 맨 아래 줄에서 시작한다.
func (model viewScreenModel) moveSelection(step, count int) int {
	shown := model.shown()
	if len(shown) == 0 {
		return -1
	}
	position := len(shown) - 1
	if model.selected >= 0 {
		for index, number := range shown {
			if number >= model.selected {
				position = index
				break
			}
		}
		position += step * count
	} else if step < 0 {
		position += step * (count - 1)
	}
	return shown[min(max(position, 0), len(shown)-1)]
}

func (model viewScreenModel) updateDetailKey(key string) (tea.Model, tea.Cmd) {
	page := max(1, model.height-3)
	switch key {
	case "esc", "enter", "q", "backspace":
		model.detail = -1
	case "up", "k":
		model.detailOffset--
	case "down", "j":
		model.detailOffset++
	case "pgup", "b":
		model.detailOffset -= page
	case "pgdown", "space":
		model.detailOffset += page
	case "home":
		model.detailOffset = 0
	}
	model.detailOffset = min(max(model.detailOffset, 0), max(0, len(model.detailRows(model.width))-page))
	return model, nil
}

const viewScreenHelp = "/ filter  ↑↓ select  ←→ scroll  enter detail  c compact  i stats  l live  esc clear  q quit"

func (model viewScreenModel) View() tea.View {
	color := traceScreenColor()
	var content []string
	if model.detail >= 0 {
		content = model.detailView(color)
	} else {
		content = model.screen(color)
	}
	view := liveFrame(strings.Join(content, "\n")+"\n", model.height)
	view.AltScreen = true
	return view
}

func (model viewScreenModel) header(color bool) []string {
	status := traceText{text: "live", color: richGreen}
	switch {
	case model.err != nil:
		status = traceText{text: "read failed: " + model.err.Error(), color: richRed}
	case model.ended:
		status = traceText{text: "input ended", color: richGray}
	case model.selected >= 0:
		status = traceText{text: fmt.Sprintf("paused · %s new", groupThousands(int64(model.last()-model.selected))), color: richYellow}
	}
	line := append(traceTitleSpans("edc view "+model.title), traceSeparator, status, traceSeparator)
	line = append(line, traceField("events ", groupThousands(int64(model.stats.lines)), "")...)
	line = append(line, traceSeparator)
	line = append(line, traceField("format ", string(model.format), "")...)
	line = append(line, traceSeparator)
	line = append(line, traceField("filter ", emptyAs(model.filter, "all"), "")...)
	if model.scroll > 0 {
		line = append(line, traceSeparator)
		line = append(line, traceField("scroll ", "→"+strconv.Itoa(model.scroll), "")...)
	}
	if model.compactOn() {
		if hidden := model.columns().hidden; len(hidden) > 0 {
			line = append(line, traceSeparator)
			line = append(line, traceField("hidden: ", strings.Join(hidden, " · "), "")...)
		}
	}
	on := []string{}
	if model.panel {
		on = append(on, "i stats")
	}
	if model.compactOn() {
		on = append(on, "c compact")
	}
	if model.selected < 0 {
		on = append(on, "l live")
	}
	help := traceKeyHints(viewScreenHelp, model.width, color, on...)
	if model.filtering {
		help = traceFilterHints(model.input.View(), model.width, color)
	}
	return []string{traceTexts(line, model.width, color), help}
}

// screen은 목록 화면이다. 통계 패널은 화면이 넓으면 오른쪽, 좁으면 목록 아래에 둔다.
func (model viewScreenModel) screen(color bool) []string {
	header := model.header(color)
	available := max(0, model.height-len(header))
	if !model.panel {
		return append(header, model.listLines(model.width, available, color)...)
	}
	if model.width >= viewPanelSideWidth {
		panelWidth := model.panelWidth()
		listWidth := model.width - panelWidth - 3
		list := model.listLines(listWidth, available, color)
		// 패널이 화면보다 길면 아래를 자른다. 위의 요약이 더 중요하다.
		panel := model.panelColumn(panelWidth, color)
		panel = panel[:min(len(panel), available)]
		for len(panel) < available {
			panel = append(panel, "")
		}
		separator := " │ "
		if color {
			separator = traceDim(separator)
		}
		for index := range list {
			list[index] = liveCell(list[index], listWidth) + separator + panel[index]
		}
		return append(header, list...)
	}
	panel := model.panelRows(model.width, color)
	panelHeight := min(len(panel), available/2)
	list := model.listLines(model.width, available-panelHeight, color)
	return append(append(header, list...), panel[:panelHeight]...)
}

// listRows는 목록에 쓰는 줄 수다. 한 쪽씩 넘길 때 쓴다.
func (model viewScreenModel) listRows() int {
	available := max(0, model.height-2)
	if model.panel && model.width < viewPanelSideWidth {
		available -= min(len(model.panelRows(model.width, false)), available/2)
	}
	return max(1, available)
}

// listLines는 목록의 줄이다. 고른 줄이 있으면 그 줄을 맨 아래에 두고 오래된 줄을 위에 채운다. trace 화면과 같다.
func (model viewScreenModel) listLines(width, available int, color bool) []string {
	shown := model.shown()
	end := len(shown) - 1
	if model.selected >= 0 {
		for index, number := range shown {
			if number >= model.selected {
				end = index
				break
			}
		}
	}
	start := max(0, end-available+1)
	// 고른 줄 위에 줄이 모자라면 남는 자리에 아래 줄을 채운다.
	stop := min(len(shown), start+available)
	rows := make([]string, 0, available)
	var columns *viewColumns
	if model.compactOn() {
		found := model.columns()
		columns = &found
	}
	for _, number := range shown[start:stop] {
		line, _ := model.line(number)
		rows = append(rows, model.renderLine(line, number == model.selected, width, color, columns))
	}
	if len(model.pending) != 0 && model.selected < 0 {
		partial := viewLine{text: viewSanitize(string(model.pending))}
		if model.matches(partial) {
			rows = append(rows, model.renderLine(partial, false, width, color, nil))
		}
	}
	return traceScreenPadRows(rows, available)
}

// renderLine은 목록의 한 줄이다. columns가 있으면 compact 보기로 그리고, scroll만큼 왼쪽을 버린다.
func (model viewScreenModel) renderLine(line viewLine, selected bool, width int, color bool, columns *viewColumns) string {
	tableWidth := max(1, width-2)
	spans := viewLineSpans(line, model.format)
	if columns != nil {
		spans = viewCompactSpans(line, *columns)
	}
	spans = viewSkipCells(spans, model.scroll)
	if selected {
		var plain strings.Builder
		for _, span := range spans {
			plain.WriteString(span.text)
		}
		return traceSelectedEvent("▸ "+traceFit(plain.String(), tableWidth), width, color)
	}
	return "  " + traceTexts(spans, tableWidth, color)
}

// compactOn은 지금 compact 보기로 그리는지다. 필드 열은 access 형식에만 있다.
func (model viewScreenModel) compactOn() bool {
	return model.compact && model.format == viewFormatAccess
}

func (model viewScreenModel) changedText() string {
	if model.changed.IsZero() {
		return "—"
	}
	return fsWatchAgo(model.now.Sub(model.changed)) + " ago"
}

// panelWidth는 오른쪽 통계 패널의 폭이다. 내용 중 가장 긴 줄에 맞추고 viewPanelWidth를 넘지 않는다. 제목은 늘 있으므로 그보다
// 좁아지지 않고, 마지막 오류와 상위 목록이 길면 그만큼 넓어진다. level과 status 막대는 제목 폭 안에 든다.
func (model viewScreenModel) panelWidth() int {
	width := len(model.panelTitle())
	for _, value := range []string{groupThousands(int64(model.stats.lines)), fmt.Sprintf("%.1f", model.stats.rate(model.now)), model.changedText(), groupThousands(int64(model.stats.failures))} {
		width = max(width, 10+len(value))
	}
	if last := model.stats.lastError; last.text != "" {
		width = max(width, ansi.StringWidth(viewLinePlain(last, model.format)))
	}
	for _, counts := range []map[string]int{model.stats.paths, model.stats.clients, model.stats.programs} {
		for _, entry := range viewTop(counts) {
			width = max(width, 8+ansi.StringWidth(viewSanitize(entry.label)))
		}
	}
	return min(width, viewPanelWidth)
}

func (model viewScreenModel) panelTitle() string {
	return "STATS  last " + groupThousands(int64(model.keep)) + " lines"
}

// panelColumn은 오른쪽 통계 패널이다. 보관한 줄만 세므로 첫 줄에 그 범위를 밝힌다.
func (model viewScreenModel) panelColumn(width int, color bool) []string {
	dim := func(text string) string {
		if color {
			return traceDim(text)
		}
		return text
	}
	row := func(label, value string) string {
		return traceFit(dim(fmt.Sprintf("%-10s", label))+value, width)
	}
	rows := []string{
		dim(traceFit(model.panelTitle(), width)),
		row("events", groupThousands(int64(model.stats.lines))),
		row("lines/s", fmt.Sprintf("%.1f", model.stats.rate(model.now))),
		row("changed", model.changedText()),
		row("errors", viewErrorCount(model.stats.failures, color)),
	}
	if breakdown := model.breakdown(); len(breakdown) != 0 {
		largest := 0
		for _, entry := range breakdown {
			largest = max(largest, entry.count)
		}
		rows = append(rows, "", dim(model.breakdownTitle()))
		for _, entry := range breakdown {
			bar := richBar(float64(entry.count), float64(largest), max(4, width-18))
			label := fmt.Sprintf("%-6s", entry.label)
			if color {
				label = tracePaint(label, entry.color, false)
				bar = richFill(bar, entry.color)
			}
			rows = append(rows, label+fmt.Sprintf("%8s ", groupThousands(int64(entry.count)))+bar)
		}
	}
	if last := model.stats.lastError; last.text != "" {
		rows = append(rows, "", dim("LAST ERROR"))
		rows = append(rows, strings.Split(ansi.Hardwrap(viewLinePlain(last, model.format), width, true), "\n")...)
	}
	for _, top := range []struct {
		title  string
		counts map[string]int
	}{{"TOP PATHS", model.stats.paths}, {"TOP CLIENTS", model.stats.clients}, {"TOP PROGRAMS", model.stats.programs}} {
		entries := viewTop(top.counts)
		if len(entries) == 0 {
			continue
		}
		rows = append(rows, "", dim(top.title))
		for _, entry := range entries {
			rows = append(rows, traceFit(fmt.Sprintf("%7s ", groupThousands(int64(entry.count)))+viewSanitize(entry.label), width))
		}
	}
	return rows
}

// panelRows는 좁은 화면에서 목록 아래에 붙이는 통계다. 항목을 한 줄씩 가로로 늘어놓는다.
func (model viewScreenModel) panelRows(width int, color bool) []string {
	summary := []traceText{traceLabelSpan("stats  last " + groupThousands(int64(model.keep)) + " lines"), traceSeparator}
	summary = append(summary, traceField("lines/s ", fmt.Sprintf("%.1f", model.stats.rate(model.now)), "")...)
	summary = append(summary, traceSeparator)
	summary = append(summary, traceField("errors ", groupThousands(int64(model.stats.failures)), "")...)
	summary = append(summary, traceSeparator)
	summary = append(summary, traceField("changed ", model.changedText(), "")...)
	rows := []string{traceTexts(summary, width, color)}
	if breakdown := model.breakdown(); len(breakdown) != 0 {
		spans := []traceText{traceLabelSpan(strings.ToLower(model.breakdownTitle()) + "  ")}
		for index, entry := range breakdown {
			if index > 0 {
				spans = append(spans, traceText{text: "  "})
			}
			spans = append(spans, traceText{text: entry.label + " ", color: entry.color}, traceText{text: groupThousands(int64(entry.count))})
		}
		rows = append(rows, traceTexts(spans, width, color))
	}
	if last := model.stats.lastError; last.text != "" {
		rows = append(rows, traceTexts([]traceText{traceLabelSpan("last error  "), {text: viewLinePlain(last, model.format)}}, width, color))
	}
	for _, top := range []struct {
		title  string
		counts map[string]int
	}{{"top paths  ", model.stats.paths}, {"top clients  ", model.stats.clients}, {"top programs  ", model.stats.programs}} {
		entries := viewTop(top.counts)
		if len(entries) == 0 {
			continue
		}
		spans := []traceText{traceLabelSpan(top.title)}
		for index, entry := range entries {
			if index > 0 {
				spans = append(spans, traceSeparator)
			}
			spans = append(spans, traceText{text: viewSanitize(entry.label) + " "}, traceLabelSpan(groupThousands(int64(entry.count))))
		}
		rows = append(rows, traceTexts(spans, width, color))
	}
	return rows
}

func viewErrorCount(count int, color bool) string {
	text := groupThousands(int64(count))
	if color && count > 0 {
		return tracePaint(text, richRed, true)
	}
	return text
}

type viewBreakdownRow struct {
	label string
	count int
	color string
}

func (model viewScreenModel) breakdownTitle() string {
	if model.stats.classes != [6]int{} {
		return "STATUS"
	}
	return "LEVEL"
}

// breakdown은 status 범주나 level별 줄 수다. status가 있으면 status를 쓴다. 없는 항목은 뺀다.
func (model viewScreenModel) breakdown() []viewBreakdownRow {
	var rows []viewBreakdownRow
	if model.stats.classes != [6]int{} {
		for class := 1; class <= 5; class++ {
			if count := model.stats.classes[class]; count > 0 {
				rows = append(rows, viewBreakdownRow{fmt.Sprintf("%dxx", class), count, richHTTPCodeColor(class * 100)})
			}
		}
		return rows
	}
	for _, level := range viewLevels {
		if count := model.stats.levels[level]; count > 0 {
			rows = append(rows, viewBreakdownRow{level, count, viewLevelColor(level)})
		}
	}
	return rows
}

// viewDetailRow는 상세 화면 한 줄이다. name은 회색으로 칠하는 필드 이름이고, text는 칠하지 않는 로그 내용이다.
type viewDetailRow struct {
	name string
	text string
}

// detailRows는 상세 화면의 본문이다. 읽어 낸 필드를 먼저, 원문과 이어지는 줄(stack trace)을 그 아래에 둔다.
func (model viewScreenModel) detailRows(width int) []viewDetailRow {
	line, ok := model.line(model.detail)
	if !ok {
		return nil
	}
	width = max(20, width)
	var rows []viewDetailRow
	nameWidth := 0
	for _, field := range line.fields {
		nameWidth = max(nameWidth, len(field.name))
	}
	for _, field := range line.fields {
		name := fmt.Sprintf("%-*s  ", nameWidth, field.name)
		value := ansi.Hardwrap(viewSanitize(field.value), max(10, width-len(name)), true)
		for index, part := range strings.Split(value, "\n") {
			if index > 0 {
				name = strings.Repeat(" ", len(name))
			}
			rows = append(rows, viewDetailRow{name: name, text: part})
		}
	}
	if len(line.fields) != 0 {
		rows = append(rows, viewDetailRow{})
	}
	rows = append(rows, viewDetailRow{name: "raw"})
	texts := []string{line.text}
	for number := model.detail + 1; number <= model.last(); number++ {
		next, _ := model.line(number)
		if !next.cont {
			break
		}
		texts = append(texts, next.text)
	}
	for _, text := range texts {
		for _, part := range strings.Split(ansi.Hardwrap(text, width, true), "\n") {
			rows = append(rows, viewDetailRow{text: part})
		}
	}
	return rows
}

func (model viewScreenModel) detailView(color bool) []string {
	title := append(traceTitleSpans("edc view "+model.title), traceSeparator)
	title = append(title, traceField("line ", groupThousands(int64(model.detail+1)), "")...)
	header := []string{
		traceTexts(title, model.width, color),
		traceKeyHints("↑↓ scroll  esc back", model.width, color),
	}
	rows := model.detailRows(model.width)
	page := max(1, model.height-len(header))
	offset := min(model.detailOffset, max(0, len(rows)-page))
	content := make([]string, 0, page)
	for _, row := range rows[offset:min(len(rows), offset+page)] {
		name := row.name
		if color {
			name = traceDim(name)
		}
		content = append(content, traceFitColored(name+row.text, model.width))
	}
	return append(header, content...)
}
