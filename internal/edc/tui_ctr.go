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

// listRows는 표 머리줄 아래에 들어가는 container 수다. 제목, 키 안내, 머리줄이 세 줄이다.
func (model ctrBrowser) listRows() int { return model.height - 3 }

func (model *ctrBrowser) keepCursor() {
	count := len(model.visible())
	model.cursor = max(0, min(model.cursor, count-1))
	rows := max(1, model.listRows())
	if model.cursor < model.offset {
		model.offset = model.cursor
	}
	if model.cursor >= model.offset+rows {
		model.offset = model.cursor - rows + 1
	}
	model.offset = max(0, min(model.offset, max(0, count-rows)))
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
	case model.err != nil:
		return append(lines, svcBrowserPaint(evaluateCtrList(nil, model.err).Summary, richRed, false, model.color))
	case len(visible) == 0:
		return append(lines, liveMuted("no container matches", model.color))
	}
	nameWidth, stateWidth, imageWidth := 4, 5, 5
	for _, item := range visible {
		nameWidth = max(nameWidth, min(40, liveWidth(item.Name)))
		stateWidth = max(stateWidth, min(30, liveWidth(item.State)))
		imageWidth = max(imageWidth, min(30, liveWidth(item.Image)))
	}
	// 좁은 터미널에서는 IMAGE를 먼저 빼고, 그래도 넘치면 이름, 그다음 상태 칸을 줄인다.
	const numbers = 29
	showImage := 2+nameWidth+2+stateWidth+2+numbers+2+imageWidth <= model.width
	overflow := 2 + nameWidth + 2 + stateWidth + 2 + numbers - model.width
	if showImage {
		overflow = 0
	}
	if overflow > 0 && nameWidth > svcBrowserUnitMin {
		shrink := min(overflow, nameWidth-svcBrowserUnitMin)
		nameWidth, overflow = nameWidth-shrink, overflow-shrink
	}
	if overflow > 0 {
		stateWidth = max(svcBrowserStateMin, stateWidth-overflow)
	}
	row := func(cells []string, state string) string {
		line := svcBrowserCell(cells[0], nameWidth) + "  " + svcBrowserPaint(svcBrowserCell(cells[1], stateWidth), state, false, model.color) + "  " + fmt.Sprintf("%8s  %8s  %7s", cells[2], cells[3], cells[4])
		if showImage {
			line += "  " + svcBrowserPaint(ansi.Truncate(cells[5], imageWidth, "…"), richGray, false, model.color)
		}
		return line
	}
	lines = append(lines, liveMuted("  "+row(ctrListHeaders(), ""), model.color))
	end := min(len(visible), model.offset+max(1, model.listRows()))
	for index := model.offset; index < end; index++ {
		item := visible[index]
		lines = append(lines, svcBrowserRow(row(ctrListCells(item), item.stateColor()), index == model.cursor, model.color, model.width))
	}
	return lines
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
