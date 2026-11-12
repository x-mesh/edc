package edc

import (
	"fmt"
	"slices"
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
	// matched는 filter에 맞는 줄만 센 통계다. filter가 있으면 통계 패널이 이것을 그린다.
	matched viewStats
	// selected는 고른 줄의 번호다. -1이면 맨 아래를 따라간다.
	selected   int
	filter     string
	filterExpr viewFilter
	// history는 지금 filter 전에 건 filter들이다. 목록의 Esc와 패널의 Backspace가 하나씩 되돌린다.
	history []string
	// filterErr는 Enter로 적용하지 못한 filter의 오류다. 입력 칸 옆에 보이고, 글자를 고치면 지운다.
	filterErr string
	filtering bool
	input     textinput.Model
	panel     bool
	// picking은 통계 패널에서 filter에 더할 항목을 고르는 중이라는 뜻이다. pick은 고른 항목이다. 새 줄이 들어와 TOP
	// 순서가 바뀌어도 같은 항목을 가리키도록 순서가 아니라 항목으로 기억한다.
	picking bool
	pick    viewPanelPick
	// notice는 n이나 N으로 더 갈 오류가 없을 때 머리 줄에 보이는 안내다. 다음 키를 누르면 지운다.
	notice string
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
	input.Placeholder = "text  -text  path:/api/*  status:5xx"
	input.CharLimit = 256
	input.SetWidth(48)
	return viewScreenModel{
		title:    title,
		format:   viewFormatPlain,
		keep:     max(1, keep),
		stats:    newViewStats(),
		matched:  newViewStats(),
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
		if arrived {
			line.arrived = now.Unix()
		}
		parsed = append(parsed, line)
		previous = &parsed[len(parsed)-1]
	}
	model.appendLines(parsed)
	if arrived {
		model.stats.arrived(now, len(raw))
		if model.filter != "" {
			count := 0
			for _, line := range parsed {
				if model.matches(line) {
					count++
				}
			}
			model.matched.arrived(now, count)
		}
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
			arrived := line.arrived
			line = parseViewLine(line.text, format, previous)
			line.arrived = arrived
		}
		if line.exceptionHead {
			model.attachException(line.exception)
		}
		model.lines = append(model.lines, line)
		model.stats.add(line)
		previous = &model.lines[len(model.lines)-1]
	}
	model.recountMatched()
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
		if line.exceptionHead {
			model.attachException(line.exception)
		}
		model.stats.add(line)
		if model.filter != "" && model.matches(line) {
			model.matched.add(line)
		}
		model.lines = append(model.lines, line)
	}
	if drop := len(model.lines) - model.keep; drop > 0 {
		for _, line := range model.lines[:drop] {
			model.stats.remove(line)
			if model.filter != "" && model.matches(line) {
				model.matched.remove(line)
			}
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

// attachException은 예외 줄의 열쇠를 그 줄이 속한 event 줄에 붙이고 통계에 더한다. event 줄은 예외 줄보다 먼저 들어와
// 이미 세었으므로 여기서 더한다. event 줄에도 열쇠가 있어야 exception: filter가 event와 그 stack trace를 함께 남긴다.
func (model *viewScreenModel) attachException(key string) {
	for index := len(model.lines) - 1; index >= 0; index-- {
		event := &model.lines[index]
		if event.marker {
			return
		}
		if event.cont {
			continue
		}
		if event.exception != "" {
			return
		}
		before := *event
		event.exception = key
		viewCount(model.stats.exceptions, key, 1)
		if model.filter != "" {
			was, now := model.matches(before), model.matches(*event)
			switch {
			case was && now:
				viewCount(model.matched.exceptions, key, 1)
			case now:
				model.matched.add(*event)
			case was:
				model.matched.remove(before)
			}
		}
		return
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
	return model.filter == "" || model.filterExpr.matches(line)
}

// applyFilter는 사용자가 filter를 바꿀 때 부른다. 바꾸기 전 filter를 history에 남겨 한 단계씩 되돌릴 수 있게 한다.
func (model *viewScreenModel) applyFilter(filter string) error {
	previous := model.filter
	if filter == previous {
		return nil
	}
	if err := model.setFilter(filter); err != nil {
		return err
	}
	model.history = append(model.history, previous)
	return nil
}

// undoFilter는 바로 전 filter로 되돌린다. 되돌릴 것이 없으면 false다.
func (model *viewScreenModel) undoFilter() bool {
	last := len(model.history) - 1
	if last < 0 {
		return false
	}
	// history에는 한 번 적용한 filter만 들어가므로 다시 읽어도 오류가 나지 않는다.
	_ = model.setFilter(model.history[last])
	model.history = model.history[:last]
	model.selected = -1
	return true
}

// setFilter는 filter를 바꾸고 통계 패널이 그릴 줄을 다시 센다.
func (model *viewScreenModel) setFilter(filter string) error {
	expr, err := parseViewFilter(filter)
	if err != nil {
		return err
	}
	model.filter, model.filterExpr = filter, expr
	model.recountMatched()
	return nil
}

func (model *viewScreenModel) recountMatched() {
	model.matched = newViewStats()
	if model.filter == "" {
		return
	}
	// 보관하지 않은 줄은 셀 수 없으므로, 초당 줄 수 × viewRateWindow가 보관하는 줄 수보다 많으면 초당 수가 실제보다 작다.
	for _, line := range model.lines {
		if model.matches(line) {
			model.matched.add(line)
			if line.arrived != 0 {
				model.matched.arrived(time.Unix(line.arrived, 0), 1)
			}
		}
	}
}

// panelStats는 통계 패널이 그리는 통계다. filter가 있으면 filter에 맞는 줄만 센다.
func (model viewScreenModel) panelStats() viewStats {
	if model.filter == "" {
		return model.stats
	}
	return model.matched
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
	model.notice = ""
	if model.detail >= 0 {
		return model.updateDetailKey(key.String())
	}
	if model.picking {
		return model.updatePickKey(key.String())
	}
	if model.filtering {
		switch key.String() {
		case "enter":
			if err := model.applyFilter(strings.TrimSpace(model.input.Value())); err != nil {
				model.filterErr = err.Error()
				return model, nil
			}
			model.filterErr = ""
			model.input.Blur()
			model.filtering = false
			model.selected = -1
			return model, nil
		case "esc":
			model.input.SetValue(model.filter)
			model.filterErr = ""
			model.input.Blur()
			model.filtering = false
			return model, nil
		}
		var cmd tea.Cmd
		before := model.input.Value()
		model.input, cmd = model.input.Update(key)
		if model.input.Value() != before {
			model.filterErr = ""
		}
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
	case "n", "N":
		step := 1
		if key.String() == "N" {
			step = -1
		}
		if number, found := model.errorStep(step); found {
			model.selected = number
		} else {
			model.notice = model.errorStepNotice(step)
		}
		return model, nil
	case "x":
		if model.filter != "" {
			_ = model.applyFilter("")
			model.selected = -1
		}
		return model, nil
	case "esc":
		// Mac 자판에는 End가 없는 경우가 많다. 고른 줄이 있으면 Esc가 먼저 선택을 풀고 다시 따라간다.
		if model.selected >= 0 {
			model.selected = -1
			return model, nil
		}
		model.undoFilter()
		return model, nil
	case "i":
		model.panel = !model.panel
		return model, nil
	case "tab":
		if picks := model.panelPicks(); len(picks) != 0 {
			model.panel, model.picking = true, true
			model.pick = picks[model.pickIndex(picks)]
		}
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

// updatePickKey는 통계 패널에서 항목을 고르는 동안의 키다. Enter로 항목을 filter에 더한 뒤에도 패널에 남아서, path를
// 고른 다음 client를 고르는 식으로 이어서 좁힌다.
func (model viewScreenModel) updatePickKey(key string) (tea.Model, tea.Cmd) {
	picks := model.panelPicks()
	at := model.pickIndex(picks)
	switch key {
	case "up", "k":
		at--
	case "down", "j":
		at++
	case "enter":
		if at < len(picks) {
			next := viewFilterWith(model.filter, picks[at].field, picks[at].value)
			if err := model.applyFilter(next); err != nil {
				model.picking, model.filtering, model.filterErr = false, true, err.Error()
				model.input.SetValue(next)
				return model, model.input.Focus()
			}
			model.selected = -1
		}
	case "tab", "esc":
		model.picking = false
		return model, nil
	case "backspace":
		model.undoFilter()
	case "x":
		if model.filter != "" {
			_ = model.applyFilter("")
		}
	case "i":
		model.picking, model.panel = false, false
		return model, nil
	case "q":
		return model, tea.Quit
	}
	if len(picks) != 0 {
		model.pick = picks[min(max(at, 0), len(picks)-1)]
	}
	return model, nil
}

// errorStep은 고른 줄에서 step 방향(-1은 위, 1은 아래)으로 가장 가까운 오류 event다. 목록에 보이는 줄만 본다. 따라가는
// 중이면 방향과 관계없이 가장 최근 오류다. stack trace처럼 앞 줄에 이어지는 줄은 앞 줄의 level을 물려받으므로 건너뛴다.
func (model viewScreenModel) errorStep(step int) (int, bool) {
	shown := model.shown()
	position := len(shown) - 1
	if model.selected >= 0 {
		for index, number := range shown {
			if number >= model.selected {
				position = index
				break
			}
		}
		position += step
	} else {
		step = -1
	}
	for ; position >= 0 && position < len(shown); position += step {
		if line, _ := model.line(shown[position]); !line.marker && !line.cont && viewFailed(line) {
			return shown[position], true
		}
	}
	return 0, false
}

func (model viewScreenModel) errorStepNotice(step int) string {
	switch {
	case model.selected < 0:
		return "no error"
	case step < 0:
		return "no older error"
	}
	return "no newer error"
}

// pickIndex는 고른 항목의 picks 안 순서다. 그 항목이 패널에서 빠졌으면 첫 항목이다.
func (model viewScreenModel) pickIndex(picks []viewPanelPick) int {
	for index, pick := range picks {
		if pick == model.pick {
			return index
		}
	}
	return 0
}

// chosenPick은 패널에 고른 상태로 그릴 항목의 순서다. 고르는 중이 아니면 -1이다.
func (model viewScreenModel) chosenPick() int {
	if !model.picking {
		return -1
	}
	return model.pickIndex(model.panelPicks())
}

// viewKeepPickVisible은 좁은 화면의 통계 한 줄에서 고른 항목이 폭 밖으로 밀려나면 제목 뒤의 앞 항목을 …로 줄인다.
// spans[0]은 제목이고 picked는 고른 항목의 조각이다.
func viewKeepPickVisible(spans []traceText, picked, width int) []traceText {
	if picked < 0 {
		return spans
	}
	used := 0
	for _, span := range spans[:picked+1] {
		used += ansi.StringWidth(span.text)
	}
	if used <= width {
		return spans
	}
	return append([]traceText{spans[0], traceLabelSpan("… ")}, spans[picked:]...)
}

type viewPanelPick struct {
	field string
	value string
}

// panelPicks는 통계 패널에서 고를 수 있는 항목이다. 패널에 그리는 순서와 같다.
func (model viewScreenModel) panelPicks() []viewPanelPick {
	field := "level"
	if model.panelStats().classes != [6]int{} {
		field = "status"
	}
	var picks []viewPanelPick
	for _, entry := range model.breakdown() {
		picks = append(picks, viewPanelPick{field, entry.label})
	}
	for _, top := range viewPanelTops(model.panelStats()) {
		for _, entry := range viewTop(top.counts) {
			picks = append(picks, viewPanelPick{top.field, entry.label})
		}
	}
	return picks
}

type viewPanelTop struct {
	title  string
	field  string
	counts map[string]int
}

// viewPanelTops는 패널의 상위 목록이다. 예외가 있으면 무엇이 실패했는지가 가장 먼저 필요하므로 맨 앞에 둔다.
func viewPanelTops(stats viewStats) []viewPanelTop {
	return []viewPanelTop{{"TOP EXCEPTIONS", "exception", stats.exceptions}, {"TOP PATHS", "path", stats.paths}, {"TOP CLIENTS", "client", stats.clients}, {"TOP PROGRAMS", "program", stats.programs}}
}

// viewPickedRow는 패널에서 고른 행이다. 색이 있으면 목록의 고른 줄과 같은 바탕이고, 없으면 ▸로 표시한다.
func viewPickedRow(plain string, width int, color bool) string {
	if !color {
		return traceFit("▸"+plain, width)
	}
	return traceSelectedEvent(traceFit(plain, width), width, color)
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

// viewScreenHelp는 100열 안에 든다. ↑↓는 따로 적지 않는다. x clear는 filter가 있을 때만 머리 줄의 filter 뒤에 보인다.
const viewScreenHelp = "/ filter  n/N error  tab pick  ←→ scroll  enter open  c compact  i stats  l live  esc back  q quit"

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
	if model.notice != "" {
		status = traceText{text: status.text + " · " + model.notice, color: richYellow}
	}
	// filter를 앞에 둔다. 줄이 폭을 넘으면 뒤부터 잘리는데, 지금 무엇을 보는지가 형식보다 중요하다.
	line := append(traceTitleSpans("edc view "+model.title), traceSeparator, status, traceSeparator)
	line = append(line, traceField("filter ", emptyAs(viewSanitize(model.filter), "all"), "")...)
	if model.filter != "" {
		line = append(line, traceText{text: "  "})
		line = append(line, traceKeySpans("x clear")...)
	}
	line = append(line, traceSeparator)
	line = append(line, traceField("events ", model.eventsText(), "")...)
	line = append(line, traceSeparator)
	line = append(line, traceField("format ", string(model.format), "")...)
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
	if model.picking {
		help = traceKeyHints("↑↓ select  enter add to filter  bksp undo  esc back  q quit", model.width, color)
	}
	if model.filtering {
		input := model.input.View()
		if model.filterErr != "" {
			problem := model.filterErr
			if color {
				problem = tracePaint(problem, richRed, true)
			}
			input += "  " + problem
		}
		help = traceFilterHints(input, model.width, color)
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
		// 패널이 화면보다 길면 아래를 자른다. 위의 요약이 더 중요하다. 고른 항목이 잘리면 그 항목이 맨 아래에 오도록 민다.
		panel, picked := model.panelColumn(panelWidth, color)
		offset := max(0, picked-available+1)
		panel = panel[offset:min(len(panel), offset+available)]
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
	panel, picked := model.panelRows(model.width, color)
	panelHeight := min(len(panel), available/2)
	offset := max(0, picked-panelHeight+1)
	list := model.listLines(model.width, available-panelHeight, color)
	return append(append(header, list...), panel[offset:offset+panelHeight]...)
}

// listRows는 목록에 쓰는 줄 수다. 한 쪽씩 넘길 때 쓴다.
func (model viewScreenModel) listRows() int {
	available := max(0, model.height-2)
	if model.panel && model.width < viewPanelSideWidth {
		rows, _ := model.panelRows(model.width, false)
		available -= min(len(rows), available/2)
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
	stats := model.panelStats()
	width := len(model.panelTitle())
	for _, value := range []string{model.eventsText(), fmt.Sprintf("%.1f", stats.rate(model.now)), model.changedText(), groupThousands(int64(stats.failures))} {
		width = max(width, 10+len(value))
	}
	if last := stats.lastError; last.text != "" {
		width = max(width, ansi.StringWidth(viewPanelError(last, model.format)))
	}
	if model.filter != "" {
		width = max(width, 10+ansi.StringWidth(viewSanitize(model.filter)))
	}
	for _, top := range viewPanelTops(stats) {
		for _, entry := range viewTop(top.counts) {
			width = max(width, 8+ansi.StringWidth(viewSanitize(entry.label)))
		}
	}
	return min(width, viewPanelWidth)
}

// eventsText는 event 수다. filter가 있으면 filter에 맞는 수와 보관한 전체 수를 함께 쓴다.
func (model viewScreenModel) eventsText() string {
	total := groupThousands(int64(model.stats.lines))
	if model.filter == "" {
		return total
	}
	return groupThousands(int64(model.matched.lines)) + " / " + total
}

// viewPercent는 total에서 count가 차지하는 비율을 반올림한 글자다. 폭은 4칸이다.
func viewPercent(count, total int) string {
	if total <= 0 {
		return "  —"
	}
	return fmt.Sprintf("%3d%%", (count*100+total/2)/total)
}

func (model viewScreenModel) panelTitle() string {
	return "STATS  " + model.panelScope()
}

// panelScope는 통계가 센 범위다. filter가 있으면 filter에 맞는 줄만 센다고 밝힌다.
func (model viewScreenModel) panelScope() string {
	scope := "last " + groupThousands(int64(model.keep)) + " lines"
	if model.filter != "" {
		return "filtered · " + scope
	}
	return scope
}

// panelColumn은 오른쪽 통계 패널이다. 보관한 줄만 세므로 첫 줄에 그 범위를 밝힌다. picked는 고른 항목의 행이고, 고르는
// 중이 아니면 -1이다.
func (model viewScreenModel) panelColumn(width int, color bool) (rows []string, picked int) {
	stats := model.panelStats()
	dim := func(text string) string {
		if color {
			return traceDim(text)
		}
		return text
	}
	row := func(label, value string) string {
		return traceFit(dim(fmt.Sprintf("%-10s", label))+value, width)
	}
	rows = []string{dim(traceFit(model.panelTitle(), width))}
	if model.filter != "" {
		// filter가 패널 폭보다 길면 잘라 버리지 않고 줄을 바꾼다. 무엇으로 걸렀는지 다 보여야 한다.
		for index, part := range strings.Split(ansi.Hardwrap(viewSanitize(model.filter), max(1, width-10), true), "\n") {
			label := "filter"
			if index > 0 {
				label = ""
			}
			if color {
				part = tracePaint(part, richYellow, true)
			}
			rows = append(rows, traceFit(dim(fmt.Sprintf("%-10s", label))+part, width))
		}
	}
	rows = append(rows,
		row("events", model.eventsText()),
		row("lines/s", fmt.Sprintf("%.1f", stats.rate(model.now))),
		row("changed", model.changedText()),
		row("errors", viewErrorCount(stats.failures, color)),
	)
	chosen, pickAt, picked := model.chosenPick(), 0, -1
	if breakdown := model.breakdown(); len(breakdown) != 0 {
		largest := 0
		for _, entry := range breakdown {
			largest = max(largest, entry.count)
		}
		rows = append(rows, "", dim(model.breakdownTitle()))
		for _, entry := range breakdown {
			bar := richBar(float64(entry.count), float64(largest), max(4, width-23))
			label := fmt.Sprintf("%-6s", entry.label)
			count := fmt.Sprintf("%8s ", groupThousands(int64(entry.count)))
			percent := " " + viewPercent(entry.count, stats.lines)
			if pickAt == chosen {
				picked = len(rows)
				rows = append(rows, viewPickedRow(label+count+bar+percent, width, color))
				pickAt++
				continue
			}
			pickAt++
			if color {
				label = tracePaint(label, entry.color, false)
				bar = richFill(bar, entry.color)
			}
			rows = append(rows, label+count+bar+dim(percent))
		}
	}
	if last := stats.lastError; last.text != "" {
		rows = append(rows, "", dim("LAST ERROR"))
		rows = append(rows, strings.Split(ansi.Hardwrap(viewPanelError(last, model.format), width, true), "\n")...)
	}
	for _, top := range viewPanelTops(stats) {
		entries := viewTop(top.counts)
		if len(entries) == 0 {
			continue
		}
		rows = append(rows, "", dim(top.title))
		for _, entry := range entries {
			for index, part := range viewPanelLabel(top.field, viewSanitize(entry.label), width-8) {
				row := strings.Repeat(" ", 8) + part
				if index == 0 {
					row = fmt.Sprintf("%7s ", groupThousands(int64(entry.count))) + part
				}
				switch {
				case pickAt != chosen:
					row = traceFit(row, width)
				case index == 0:
					row = viewPickedRow(row, width, color)
				default:
					row = traceSelectedEvent(traceFit(row, width), width, color)
				}
				if pickAt == chosen {
					// 고른 항목이 두 줄이면 아랫줄을 고른 행으로 둔다. 패널을 밀 때 두 줄이 모두 보인다.
					picked = len(rows)
				}
				rows = append(rows, row)
			}
			pickAt++
		}
	}
	return rows, picked
}

// viewPanelLabel은 TOP 목록 항목 하나의 줄이다. 예외는 "클래스: 메시지"라 패널 폭에서 클래스 이름만으로 다 차므로, 두
// 줄까지 낱말 단위로 줄을 바꾼다. 다른 항목은 한 줄이고 넘치면 자른다.
func viewPanelLabel(field, label string, width int) []string {
	if field != "exception" {
		return []string{label}
	}
	parts := strings.Split(ansi.Wrap(label, max(1, width), ""), "\n")
	if len(parts) > 2 {
		parts = []string{parts[0], strings.Join(parts[1:], " ")}
	}
	return parts
}

// viewPanelError는 패널의 마지막 오류다. Spring Boot처럼 logger를 정렬하려고 넣은 공백이 패널 폭을 차지하지 않게 이어진
// 공백을 하나로 줄인다.
func viewPanelError(last viewLine, format viewFormat) string {
	return strings.Join(strings.Fields(viewLinePlain(last, format)), " ")
}

// panelRows는 좁은 화면에서 목록 아래에 붙이는 통계다. 항목을 한 줄씩 가로로 늘어놓는다. picked는 고른 항목이 든 줄이고,
// 고르는 중이 아니면 -1이다.
func (model viewScreenModel) panelRows(width int, color bool) (rows []string, picked int) {
	stats := model.panelStats()
	summary := []traceText{traceLabelSpan("stats  " + model.panelScope()), traceSeparator}
	if model.filter != "" {
		summary = append(summary, traceLabelSpan("filter "), traceText{text: viewSanitize(model.filter), color: richYellow, bold: true})
		summary = append(summary, traceSeparator)
	}
	summary = append(summary, traceField("lines/s ", fmt.Sprintf("%.1f", stats.rate(model.now)), "")...)
	summary = append(summary, traceSeparator)
	summary = append(summary, traceField("errors ", groupThousands(int64(stats.failures)), "")...)
	summary = append(summary, traceSeparator)
	summary = append(summary, traceField("changed ", model.changedText(), "")...)
	rows = []string{traceTexts(summary, width, color)}
	chosen, pickAt, picked := model.chosenPick(), 0, -1
	if breakdown := model.breakdown(); len(breakdown) != 0 {
		spans := []traceText{traceLabelSpan(strings.ToLower(model.breakdownTitle()) + "  ")}
		pickedSpan := -1
		for index, entry := range breakdown {
			if index > 0 {
				spans = append(spans, traceText{text: "  "})
			}
			count := groupThousands(int64(entry.count))
			percent := " " + strings.TrimSpace(viewPercent(entry.count, stats.lines))
			if pickAt == chosen {
				picked, pickedSpan = len(rows), len(spans)
				spans = append(spans, traceText{text: "▸" + entry.label + " " + count + percent, on: true})
			} else {
				spans = append(spans, traceText{text: entry.label + " ", color: entry.color}, traceText{text: count}, traceLabelSpan(percent))
			}
			pickAt++
		}
		rows = append(rows, traceTexts(viewKeepPickVisible(spans, pickedSpan, width), width, color))
	}
	if last := stats.lastError; last.text != "" {
		rows = append(rows, traceTexts([]traceText{traceLabelSpan("last error  "), {text: viewPanelError(last, model.format)}}, width, color))
	}
	for _, top := range viewPanelTops(stats) {
		entries := viewTop(top.counts)
		if len(entries) == 0 {
			continue
		}
		spans := []traceText{traceLabelSpan(strings.ToLower(top.title) + "  ")}
		pickedSpan := -1
		for index, entry := range entries {
			if index > 0 {
				spans = append(spans, traceSeparator)
			}
			if pickAt == chosen {
				picked, pickedSpan = len(rows), len(spans)
				spans = append(spans, traceText{text: "▸" + viewSanitize(entry.label) + " " + groupThousands(int64(entry.count)), on: true})
			} else {
				spans = append(spans, traceText{text: viewSanitize(entry.label) + " "}, traceLabelSpan(groupThousands(int64(entry.count))))
			}
			pickAt++
		}
		rows = append(rows, traceTexts(viewKeepPickVisible(spans, pickedSpan, width), width, color))
	}
	return rows, picked
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

func (model viewScreenModel) breakdownTitle() string { return viewBreakdownTitle(model.panelStats()) }

func (model viewScreenModel) breakdown() []viewBreakdownRow { return viewBreakdown(model.panelStats()) }

func viewBreakdownTitle(stats viewStats) string {
	if stats.classes != [6]int{} {
		return "STATUS"
	}
	return "LEVEL"
}

// viewBreakdown은 status 범주나 level별 줄 수다. status가 있으면 status를 쓴다. 없는 항목은 뺀다.
func viewBreakdown(stats viewStats) []viewBreakdownRow {
	var rows []viewBreakdownRow
	if stats.classes != [6]int{} {
		for class := 1; class <= 5; class++ {
			if count := stats.classes[class]; count > 0 {
				rows = append(rows, viewBreakdownRow{fmt.Sprintf("%dxx", class), count, richHTTPCodeColor(class * 100)})
			}
		}
		return rows
	}
	for _, level := range viewLevels {
		if count := stats.levels[level]; count > 0 {
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
	fields := line.fields
	if line.exception != "" && !line.cont {
		fields = append(slices.Clone(fields), viewField{"exception", line.exception})
	}
	nameWidth := 0
	for _, field := range fields {
		nameWidth = max(nameWidth, len(field.name))
	}
	for _, field := range fields {
		name := fmt.Sprintf("%-*s  ", nameWidth, field.name)
		value := ansi.Hardwrap(viewSanitize(field.value), max(10, width-len(name)), true)
		for index, part := range strings.Split(value, "\n") {
			if index > 0 {
				name = strings.Repeat(" ", len(name))
			}
			rows = append(rows, viewDetailRow{name: name, text: part})
		}
	}
	if len(fields) != 0 {
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
