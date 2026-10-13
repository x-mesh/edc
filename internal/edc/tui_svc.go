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

const (
	// svcBrowserRefresh는 목록을 다시 읽는 간격이다. 재시작을 반복하는 unit은 몇 초 사이에 상태가 바뀐다.
	svcBrowserRefresh = 2 * time.Second
	// svcBrowserUnitMin과 svcBrowserStateMin은 좁은 터미널에서 칸이 줄어드는 하한이다. 이보다 줄이면 알아보기 어렵다.
	svcBrowserUnitMin  = 24
	svcBrowserStateMin = 12
)

// svcBrowser는 unit 없이 터미널에서 부른 edc svc다. 목록에서 unit을 골라 Enter로 상세를 열고 Esc로 돌아온다.
type svcBrowser struct {
	sources   svcSources
	timeout   time.Duration
	all       bool
	failed    bool
	filter    string
	filtering bool
	items     []svcListItem
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

type svcBrowserDetail struct {
	unit    string
	lines   []string
	loading bool
	offset  int
	seq     int
}

type svcListMsg struct {
	seq   int
	items []svcListItem
	err   error
	at    time.Time
}

type svcDetailMsg struct {
	seq   int
	lines []string
}

type svcTickMsg struct{ seq int }

func runSvcBrowser(sources svcSources, timeout time.Duration, all bool) int {
	if _, err := tea.NewProgram(newSvcBrowser(sources, timeout, all, os.Getenv("NO_COLOR") == ""), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newSvcBrowser(sources svcSources, timeout time.Duration, all, color bool) svcBrowser {
	return svcBrowser{sources: sources, timeout: timeout, all: all, color: color, width: 80, height: 24}
}

func (model svcBrowser) Init() tea.Cmd { return model.loadList() }

func (model svcBrowser) loadList() tea.Cmd {
	sources, timeout, all, seq := model.sources, model.timeout, model.all, model.seq
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := collectSvcList(ctx, sources, all)
		return svcListMsg{seq: seq, items: items, err: err, at: time.Now()}
	}
}

func (model svcBrowser) tick() tea.Cmd {
	seq := model.seq
	return tea.Tick(svcBrowserRefresh, func(time.Time) tea.Msg { return svcTickMsg{seq: seq} })
}

// loadDetail은 edc svc -v <unit>과 같은 결과를 만든다. CPU를 두 번 재고 journal을 읽어 시간이 걸리므로 화면 밖에서 돈다.
func (model svcBrowser) loadDetail(unit string, seq int) tea.Cmd {
	sources, timeout, color := model.sources, model.timeout, model.color
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var buffer strings.Builder
		printTerminalWithColor(&buffer, evaluateSvc(collectSvcFacts(ctx, unit, sources)), true, color)
		return svcDetailMsg{seq: seq, lines: strings.Split(strings.TrimRight(buffer.String(), "\n"), "\n")}
	}
}

func (model svcBrowser) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.keepCursor()
	case svcListMsg:
		if value.seq != model.seq {
			return model, nil
		}
		// 갱신 뒤에도 고르던 unit에 머문다. 순서는 상태에 따라 바뀌므로 번호로 두면 다른 unit으로 옮겨 간다.
		selected := model.selectedUnit()
		model.items, model.err, model.loaded, model.updated = value.items, value.err, true, value.at
		model.cursor = 0
		for index, item := range model.visible() {
			if item.Unit == selected {
				model.cursor = index
			}
		}
		model.keepCursor()
		return model, model.tick()
	case svcTickMsg:
		if value.seq != model.seq {
			return model, nil
		}
		return model, model.loadList()
	case svcDetailMsg:
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

func (model svcBrowser) updateListKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
			model.detail = &svcBrowserDetail{unit: visible[model.cursor].Unit, loading: true, seq: model.detailSeq}
			return model, model.loadDetail(model.detail.unit, model.detail.seq)
		}
	}
	model.keepCursor()
	return model, nil
}

func (model svcBrowser) updateDetailKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
		return model, model.loadDetail(detail.unit, detail.seq)
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

func (model svcBrowser) selectedUnit() string {
	if visible := model.visible(); model.cursor >= 0 && model.cursor < len(visible) {
		return visible[model.cursor].Unit
	}
	return ""
}

func (model svcBrowser) visible() []svcListItem {
	filter := strings.ToLower(model.filter)
	var items []svcListItem
	for _, item := range model.items {
		if model.failed && !item.failed() {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(item.Unit+" "+item.Description), filter) {
			continue
		}
		items = append(items, item)
	}
	return items
}

// listRows는 표 머리줄 아래에 들어가는 unit 수다. 제목, 키 안내, 머리줄이 세 줄이다.
func (model svcBrowser) listRows() int { return model.height - 3 }

func (model *svcBrowser) keepCursor() {
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

func (model svcBrowser) View() tea.View {
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

func (model svcBrowser) detailLines() []string {
	detail := model.detail
	title := "edc svc · " + detail.unit + " · ↑↓ scroll · r reload · Esc back · q quit"
	if detail.loading {
		title = "edc svc · " + detail.unit + " · loading… · Esc back · q quit"
	}
	lines := []string{liveMuted(title, model.color)}
	end := min(len(detail.lines), detail.offset+max(1, model.height-1))
	return append(lines, detail.lines[min(detail.offset, end):end]...)
}

func (model svcBrowser) listLines() []string {
	visible := model.visible()
	failed, changing := 0, 0
	for _, item := range model.items {
		switch {
		case item.failed():
			failed++
		case item.changing():
			changing++
		}
	}
	title := fmt.Sprintf("edc svc · %d services · %d failed · %d starting or restarting", len(model.items), failed, changing)
	if model.all {
		title += " · all"
	}
	if !model.updated.IsZero() {
		title += " · " + model.updated.Format("15:04:05")
	}
	keys := "↑↓ select · Enter open · / filter · f failed · a all · r refresh · q quit"
	switch {
	case model.filtering:
		keys = "filter: " + model.filter + "█ · Enter done · Esc clear"
	case model.filter != "" || model.failed:
		shown := "failed only"
		if model.filter != "" {
			shown = "filter " + model.filter
			if model.failed {
				shown += ", failed only"
			}
		}
		keys = fmt.Sprintf("%s · %d shown · Esc clear · Enter open · q quit", shown, len(visible))
	}
	lines := []string{title, liveMuted(keys, model.color)}
	switch {
	case !model.loaded:
		return append(lines, "loading…")
	case model.err != nil:
		return append(lines, evaluateSvcList(nil, model.err).Summary)
	case len(visible) == 0:
		return append(lines, "no service matches")
	}
	unitWidth, stateWidth := 4, 5
	for _, item := range visible {
		unitWidth = max(unitWidth, min(40, liveWidth(item.Unit)))
		stateWidth = max(stateWidth, min(30, liveWidth(item.state())))
	}
	// 좁은 터미널에서는 PID를 먼저 빼고, 그래도 넘치면 unit 이름을 줄인다. 상태와 재시작이 이 화면에서 가장 중요하다.
	tail := "%8s  %8s  %7s  %8s"
	tailWidth := 39
	if 2+unitWidth+2+stateWidth+2+tailWidth > model.width {
		tail, tailWidth = "%8s  %8s  %7s", 29
	}
	overflow := 2 + unitWidth + 2 + stateWidth + 2 + tailWidth - model.width
	if overflow > 0 && unitWidth > svcBrowserUnitMin {
		shrink := min(overflow, unitWidth-svcBrowserUnitMin)
		unitWidth, overflow = unitWidth-shrink, overflow-shrink
	}
	if overflow > 0 {
		stateWidth = max(svcBrowserStateMin, stateWidth-overflow)
	}
	row := func(prefix string, cells []string) string {
		values := []any{cells[2], cells[3], cells[4], cells[5]}
		return prefix + svcBrowserCell(cells[0], unitWidth) + "  " + svcBrowserCell(cells[1], stateWidth) + "  " + fmt.Sprintf(tail, values[:strings.Count(tail, "%")]...)
	}
	lines = append(lines, row("  ", svcListHeaders()))
	end := min(len(visible), model.offset+max(1, model.listRows()))
	for index := model.offset; index < end; index++ {
		item := visible[index]
		prefix := "  "
		if index == model.cursor {
			prefix = "› "
		}
		line := row(prefix, svcListCells(item))
		switch {
		case item.failed():
			line = topPaint(line, topLevelDanger, model.color)
		case item.changing():
			line = topPaint(line, topLevelWarn, model.color)
		}
		lines = append(lines, line)
	}
	return lines
}

// svcBrowserCell은 칸 폭에 맞춰 자르고 채운다. 긴 unit 이름은 끝을 "…"로 줄인다.
func svcBrowserCell(text string, width int) string {
	text = ansi.Truncate(text, width, "…")
	return text + strings.Repeat(" ", max(0, width-liveWidth(text)))
}
