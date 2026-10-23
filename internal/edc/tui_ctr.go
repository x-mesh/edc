package edc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ctrBrowser는 container 없이 터미널에서 부른 edc ctr다. svcBrowser와 같은 키로 목록에서 골라 Enter로 상세를 연다.
// 두 화면은 다루는 항목과 상세가 달라 따로 둔다. 세 번째 목록이 생기면 공통 부분을 묶는다.
type ctrBrowser struct {
	sources   ctrSources
	timeout   time.Duration
	all       bool
	failed    bool
	filter    string
	filtering bool
	items     []ctrListItem
	err       error
	loaded    bool
	updated   time.Time
	cursor    int
	offset    int
	detail    *svcBrowserDetail
	width     int
	height    int
	color     bool
	// seq는 목록을 읽는 차례다. --all을 바꾸면 올려서, 전에 보낸 읽기와 tick의 결과를 버린다.
	seq       int
	detailSeq int
}

type ctrListMsg struct {
	seq   int
	items []ctrListItem
	err   error
	at    time.Time
}

type ctrDetailMsg struct {
	seq   int
	lines []string
}

type ctrTickMsg struct{ seq int }

func runCtrBrowser(sources ctrSources, timeout time.Duration, all bool) int {
	if _, err := tea.NewProgram(newCtrBrowser(sources, timeout, all, os.Getenv("NO_COLOR") == ""), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newCtrBrowser(sources ctrSources, timeout time.Duration, all, color bool) ctrBrowser {
	return ctrBrowser{sources: sources, timeout: timeout, all: all, color: color, width: 80, height: 24}
}

func (model ctrBrowser) Init() tea.Cmd { return model.loadList() }

func (model ctrBrowser) loadList() tea.Cmd {
	sources, timeout, all, seq := model.sources, model.timeout, model.all, model.seq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := collectCtrList(ctx, sources, all)
		return ctrListMsg{seq: seq, items: items, err: err, at: time.Now()}
	}
}

func (model ctrBrowser) tick() tea.Cmd {
	seq := model.seq
	return tea.Tick(svcBrowserRefresh, func(time.Time) tea.Msg { return ctrTickMsg{seq: seq} })
}

// loadDetail은 edc ctr -v <container>와 같은 결과를 만든다. CPU를 두 번 재고 로그를 읽어 시간이 걸리므로 화면 밖에서 돈다.
func (model ctrBrowser) loadDetail(reference string, seq int) tea.Cmd {
	sources, timeout, color := model.sources, model.timeout, model.color
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var buffer strings.Builder
		printTerminalWithColor(&buffer, evaluateCtr(collectCtrFacts(ctx, reference, sources)), true, color)
		return ctrDetailMsg{seq: seq, lines: strings.Split(strings.TrimRight(buffer.String(), "\n"), "\n")}
	}
}

func (model ctrBrowser) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.keepCursor()
	case ctrListMsg:
		if value.seq != model.seq {
			return model, nil
		}
		// 갱신 뒤에도 고르던 container에 머문다. 순서는 상태에 따라 바뀌므로 번호로 두면 다른 container로 옮겨 간다.
		selected := model.selectedName()
		model.items, model.err, model.loaded, model.updated = value.items, value.err, true, value.at
		model.cursor = 0
		for index, item := range model.visible() {
			if item.Name == selected {
				model.cursor = index
			}
		}
		model.keepCursor()
		return model, model.tick()
	case ctrTickMsg:
		if value.seq != model.seq {
			return model, nil
		}
		return model, model.loadList()
	case ctrDetailMsg:
		if model.detail != nil && model.detail.seq == value.seq {
			model.detail.lines, model.detail.loading = value.lines, false
		}
	case tea.KeyPressMsg:
		if model.detail != nil {
			return model.updateDetailKey(value)
		}
		return model.updateListKey(value)
	}
	return model, nil
}

func (model ctrBrowser) updateListKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if model.filtering {
		switch key.String() {
		case "ctrl+c":
			return model, tea.Quit
		case "enter":
			model.filtering = false
		case "esc":
			model.filtering, model.filter = false, ""
		case "backspace":
			if runes := []rune(model.filter); len(runes) > 0 {
				model.filter = string(runes[:len(runes)-1])
			}
		default:
			if key.Text != "" {
				model.filter += key.Text
			}
		}
		model.cursor, model.offset = 0, 0
		return model, nil
	}
	visible := model.visible()
	page := max(1, model.listRows())
	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "up", "k":
		model.cursor--
	case "down", "j":
		model.cursor++
	case "pgup":
		model.cursor -= page
	case "pgdown":
		model.cursor += page
	case "home", "g":
		model.cursor = 0
	case "end", "G":
		model.cursor = len(visible) - 1
	case "/":
		model.filtering = true
	case "f":
		model.failed, model.cursor, model.offset = !model.failed, 0, 0
	case "a":
		model.all, model.cursor, model.offset = !model.all, 0, 0
		model.seq++
		return model, model.loadList()
	case "r":
		model.seq++
		return model, model.loadList()
	case "esc":
		// Esc는 한 단계 뒤로다. 목록에서는 이름 필터와 실패만 보기를 푼다.
		model.filter, model.failed, model.cursor, model.offset = "", false, 0, 0
	case "enter":
		if len(visible) > 0 {
			model.detailSeq++
			model.detail = &svcBrowserDetail{unit: visible[model.cursor].Name, loading: true, seq: model.detailSeq}
			return model, model.loadDetail(visible[model.cursor].ID, model.detail.seq)
		}
	}
	model.keepCursor()
	return model, nil
}

func (model ctrBrowser) updateDetailKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	detail := *model.detail
	page := max(1, model.height-1)
	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc", "backspace":
		model.detail = nil
		return model, nil
	case "r":
		model.detailSeq++
		detail.loading, detail.seq = true, model.detailSeq
		model.detail = &detail
		return model, model.loadDetail(model.detailID(), detail.seq)
	case "up", "k":
		detail.offset--
	case "down", "j":
		detail.offset++
	case "pgup":
		detail.offset -= page
	case "pgdown":
		detail.offset += page
	case "home", "g":
		detail.offset = 0
	}
	detail.offset = max(0, min(detail.offset, len(detail.lines)-page))
	model.detail = &detail
	return model, nil
}

// detailID는 연 container의 ID다. 이름은 같은 이름으로 다시 만든 container를 가리킬 수 있어 ID로 다시 읽는다.
func (model ctrBrowser) detailID() string {
	for _, item := range model.items {
		if item.Name == model.detail.unit {
			return item.ID
		}
	}
	return model.detail.unit
}

func (model ctrBrowser) selectedName() string {
	if visible := model.visible(); model.cursor >= 0 && model.cursor < len(visible) {
		return visible[model.cursor].Name
	}
	return ""
}

func (model ctrBrowser) visible() []ctrListItem {
	filter := strings.ToLower(model.filter)
	var items []ctrListItem
	for _, item := range model.items {
		if model.failed && !item.failed() {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(item.Name+" "+item.Image+" "+item.ID), filter) {
			continue
		}
		items = append(items, item)
	}
	return items
}

// listRows는 표 머리줄 아래에 들어가는 container 수다.
func (model ctrBrowser) listRows() int {
	visible := model.visible()
	if len(visible) == 0 {
		return 0
	}
	budget := model.listBudget()
	rows, used := 0, 0
	for index := model.offset; index < len(visible); index++ {
		height := model.rowHeightFor(visible[index])
		if rows > 0 && used+height > budget {
			break
		}
		used += height
		rows++
	}
	return max(1, rows)
}

func (model ctrBrowser) listBudget() int {
	used := 2
	if model.width >= 80 {
		used++
	}
	if model.err != nil && len(model.items) > 0 {
		used++
	}
	used += len(model.selectedUnavailableLines())
	return max(1, model.height-used)
}

func (model ctrBrowser) rowHeightFor(item ctrListItem) int {
	if model.width < 80 {
		return len(model.compactRow(item, false))
	}
	return 1
}

func (model ctrBrowser) selectedMemoryUnavailable() string {
	visible := model.visible()
	if model.cursor < 0 || model.cursor >= len(visible) {
		return ""
	}
	return visible[model.cursor].MemoryUnavailable
}

func (model ctrBrowser) selectedUnavailableLines() []string {
	visible := model.visible()
	if model.cursor < 0 || model.cursor >= len(visible) {
		return nil
	}
	item := visible[model.cursor]
	var reasons []string
	for _, metric := range []struct {
		label   string
		reasons []string
	}{{ctrListHeaders()[2], []string{item.CPUUnavailable}}, {ctrListHeaders()[3], []string{item.MemoryUnavailable}}, {ctrListHeaders()[5], []string{item.PIDsUnavailable}}, {ctrListHeaders()[6], []string{item.NetworkRXUnavailable, item.NetworkTXUnavailable}}, {ctrListHeaders()[7], []string{item.BlockReadUnavailable, item.BlockWriteUnavailable}}} {
		for _, reason := range metric.reasons {
			if reason != "" {
				reasons = append(reasons, metric.label+": "+reason)
			}
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	text := strings.Join(reasons, " · ")
	wrapped := strings.Split(ansi.Wrap(text, max(1, model.width), ""), "\n")
	available := model.height - 2
	if model.width >= 80 {
		available--
	}
	if model.err != nil && len(model.items) > 0 {
		available--
	}
	available -= model.rowHeightFor(item)
	if available <= 0 {
		return nil
	}
	if len(wrapped) > available {
		wrapped = wrapped[:available]
		wrapped[len(wrapped)-1] = strings.TrimRight(ansi.Truncate(wrapped[len(wrapped)-1], max(1, model.width-1), ""), " ") + "…"
	}
	return wrapped
}

func (model ctrBrowser) memoryUnavailableLines() []string {
	return model.selectedUnavailableLines()
}

func (model *ctrBrowser) keepCursor() {
	visible := model.visible()
	count := len(visible)
	model.cursor = max(0, min(model.cursor, count-1))
	if count == 0 {
		model.offset = 0
		return
	}
	budget := model.listBudget()
	if model.cursor < model.offset {
		model.offset = model.cursor
	}
	for model.offset < model.cursor && model.visibleHeight(model.offset, model.cursor+1) > budget {
		model.offset++
	}
	for model.offset > 0 && model.visibleHeight(model.offset-1, model.cursor+1) <= budget {
		model.offset--
	}
	model.offset = max(0, min(model.offset, count-1))
}

func (model ctrBrowser) visibleHeight(start, end int) int {
	visible := model.visible()
	height := 0
	for index := max(0, start); index < min(len(visible), end); index++ {
		height += model.rowHeightFor(visible[index])
	}
	return height
}

func (model ctrBrowser) View() tea.View {
	var lines []string
	if model.detail != nil {
		lines = model.detailLines()
	} else {
		lines = model.listLines()
	}
	for index, line := range lines {
		lines[index] = ansi.Truncate(line, model.width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model ctrBrowser) detailLines() []string {
	return svcBrowserDetailLines("edc ctr", model.detail, model.height, model.color)
}

func (model ctrBrowser) listLines() []string {
	visible := model.visible()
	failed, attention := 0, 0
	for _, item := range model.items {
		switch {
		case item.failed():
			failed++
		case item.attention():
			attention++
		}
	}
	parts := []string{fmt.Sprintf("%d containers", len(model.items)),
		svcBrowserCount(fmt.Sprintf("%d failed", failed), failed, richRed, model.color),
		svcBrowserCount(fmt.Sprintf("%d restarting, paused, or unhealthy", attention), attention, richYellow, model.color)}
	if model.all {
		parts = append(parts, liveMuted("all", model.color))
	}
	if !model.updated.IsZero() {
		parts = append(parts, liveMuted(model.updated.Format("15:04:05"), model.color))
	}
	title := svcBrowserHeading("edc ctr", parts, model.color)
	lines := []string{title, svcBrowserHints(model.filter, model.filtering, model.failed, model.all, len(visible), model.color)}
	switch {
	case !model.loaded:
		return append(lines, liveMuted("loading…", model.color))
	case model.err != nil && len(model.items) == 0:
		return append(lines, svcBrowserPaint(evaluateCtrList(nil, model.err).Summary, richRed, false, model.color))
	}
	if model.err != nil {
		lines = append(lines, svcBrowserPaint(evaluateCtrList(nil, model.err).Summary, richRed, false, model.color))
	}
	if len(visible) == 0 {
		return append(lines, liveMuted("no container matches", model.color))
	}
	nameWidth, stateWidth := 4, 5
	for _, item := range visible {
		nameWidth = max(nameWidth, min(40, liveWidth(item.Name)))
		stateWidth = max(stateWidth, min(30, liveWidth(item.State)))
	}
	if model.width < 80 {
		for index := model.offset; index < min(len(visible), model.offset+model.listRows()); index++ {
			lines = append(lines, model.compactRow(visible[index], index == model.cursor)...)
		}
	} else {
		headers := ctrListHeaders()
		columns := []int{2, 3, 4, 5}
		columnWidths := make([]int, len(columns))
		for position, column := range columns {
			columnWidths[position] = ansi.StringWidth(headers[column])
			for _, item := range visible {
				value := ctrListCells(item)[column]
				if column == 10 {
					value = ansi.Truncate(value, 20, "…")
				}
				columnWidths[position] = max(columnWidths[position], ansi.StringWidth(value))
			}
		}
		bodyBudget := model.width - 2
		bodyWidth := func(nameWidth, stateWidth int, widths []int) int {
			used := nameWidth + 2 + stateWidth + len(widths)
			for _, width := range widths {
				used += width
			}
			return used
		}
		if bodyWidth(nameWidth, stateWidth, columnWidths) > bodyBudget {
			excess := bodyWidth(nameWidth, stateWidth, columnWidths) - bodyBudget
			nameMinimum := min(nameWidth, svcBrowserUnitMin)
			stateMinimum := min(stateWidth, svcBrowserStateMin)
			shrink := min(excess, max(0, nameWidth-nameMinimum))
			nameWidth -= shrink
			excess -= shrink
			if excess > 0 {
				shrink = min(excess, max(0, stateWidth-stateMinimum))
				stateWidth -= shrink
				excess -= shrink
			}
			if excess > 0 {
				shrink = min(excess, max(0, nameWidth-4))
				nameWidth -= shrink
				excess -= shrink
			}
			if excess > 0 {
				stateWidth = max(7, stateWidth-excess)
			}
		}
		for _, column := range []int{6, 7, 8, 9, 10} {
			width := ansi.StringWidth(headers[column])
			for _, item := range visible {
				value := ctrListCells(item)[column]
				if column == 10 {
					value = ansi.Truncate(value, 30, "…")
				}
				width = max(width, ansi.StringWidth(value))
			}
			if column == 10 {
				width = min(width, 30)
				remaining := bodyBudget - bodyWidth(nameWidth, stateWidth, columnWidths) - 1
				minimum := min(width, max(ansi.StringWidth(headers[column]), 8))
				if remaining < minimum {
					continue
				}
				width = min(width, remaining)
			}
			if bodyWidth(nameWidth, stateWidth, columnWidths)+1+width <= bodyBudget {
				columns = append(columns, column)
				columnWidths = append(columnWidths, width)
			}
		}
		rowCells := func(values []string, name, state string) string {
			line := svcBrowserCell(name, nameWidth) + "  " + svcBrowserCell(state, stateWidth)
			for position, column := range columns {
				value := values[column]
				if column == 10 {
					value = svcBrowserPaint(svcBrowserCell(ansi.Truncate(value, columnWidths[position], "…"), columnWidths[position]), richGray, false, model.color)
					line += " " + value
					continue
				}
				line += " " + fmt.Sprintf("%*s", columnWidths[position], value)
			}
			return line
		}
		header := rowCells(headers, ansi.Truncate(headers[0], nameWidth, "…"), ansi.Truncate(headers[1], stateWidth, "…"))
		lines = append(lines, liveMuted("  "+header, model.color))
		for index := model.offset; index < min(len(visible), model.offset+model.listRows()); index++ {
			item := visible[index]
			c := ctrListCells(item)
			state := svcBrowserPaint(ansi.Truncate(c[1], stateWidth, "…"), item.stateColor(), false, model.color)
			line := rowCells(c, ansi.Truncate(c[0], nameWidth, "…"), state)
			lines = append(lines, svcBrowserRow(line, index == model.cursor, model.color, model.width))
		}
	}
	for _, hint := range model.selectedUnavailableLines() {
		lines = append(lines, svcBrowserPaint(hint, richYellow, false, model.color))
	}
	return lines
}

func (model ctrBrowser) compactRow(item ctrListItem, selected bool) []string {
	c := ctrListCells(item)
	stateWidth := min(12, max(7, liveWidth(c[1])))
	nameWidth := max(4, model.width-3-stateWidth)
	first := svcBrowserCell(c[0], nameWidth) + " " + svcBrowserPaint(svcBrowserCell(c[1], stateWidth), item.stateColor(), false, model.color)
	values := []string{"CPU " + c[2], "MEM " + c[3], "MEM% " + c[4], "PIDs " + c[5]}
	var result []string
	bodyWidth := max(1, model.width-2)
	result = append(result, svcBrowserRow(ansi.Truncate(first, bodyWidth, ""), selected, model.color, model.width))
	line := ""
	for _, value := range values {
		candidate := value
		if line != "" {
			candidate = line + "  " + value
		}
		if line != "" && ansi.StringWidth(candidate) > bodyWidth {
			result = append(result, svcBrowserRow(line, false, model.color, model.width))
			line = value
		} else {
			line = candidate
		}
	}
	if line != "" {
		result = append(result, svcBrowserRow(line, false, model.color, model.width))
	}
	return result
}

// stateColor는 상태 칸의 색이다. 실패는 빨강, 손볼 일이 있으면 노랑, 실행 중은 초록, 나머지는 회색이다.
func (item ctrListItem) stateColor() string {
	switch {
	case item.failed():
		return richRed
	case item.attention():
		return richYellow
	case item.Status == "running":
		return richGreen
	}
	return richGray
}
