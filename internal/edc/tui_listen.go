package edc

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type listenBrowser struct {
	openTop func(string) tea.Cmd
	notice  string
	work    chan struct{}
	full    bool
	ctx     context.Context
	load    func(context.Context) Result
	inspect func(context.Context, listenRow) []string
	timeout time.Duration
	// sockets는 마지막으로 읽은 소켓 전부이고, items는 그것을 모으고 거른 화면의 줄이다.
	sockets      []listenSocket
	items        []listenRow
	hideLoopback bool
	// filter는 /로 받은 글자다. 낱말마다 줄에 보이는 글자에서 찾는다. filtering은 글자를 받는 중이라는 뜻이다.
	filter                             string
	filtering                          bool
	warnings                           []string
	err                                string
	loaded, loading                    bool
	updated                            time.Time
	cursor, offset, width, height, seq int
	color, redact                      bool
	detail                             *listenBrowserDetail
	detailSeq                          int
	// pidRow와 pid는 p로 고른 PID와 그 줄이다. 다른 줄에서는 줄의 첫 PID를 쓴다.
	pidRow listenRowKey
	pid    string
	// openStop은 S가 고른 process를 잡는다. 시험에서는 실제 process 대신 가짜를 넣는다.
	openStop func(pid int, name string, listed time.Time) (listenStopProcess, error)
	// listed는 지금 목록을 읽기 시작한 시각이다. 이보다 늦게 시작한 process는 목록의 process가 아니다.
	listed time.Time
	// stop은 S가 띄운 확인 상자다. 떠 있는 동안 키는 모두 확인 상자가 받는다.
	stop *listenStopConfirm
	// stopped는 SIGTERM을 보낸 PID다. 다음 목록에서 그 PID의 소켓이 남았는지 알린다.
	stopped int
}

type listenStopConfirm struct {
	process              listenStopProcess
	pid                  int
	name, question, note string
	yes                  bool
}

type listenStopDoneMsg struct {
	pid  int
	name string
	err  error
}

type listenStopCheckMsg struct{ pid int }

type listenBrowserDetail struct {
	row         listenRow
	lines       []string
	offset, seq int
	loading     bool
	// trace면 lines는 x가 만든 trace 안내다. 안내는 PID를 담고 있어서 p로 PID를 바꾸면 다시 만든다.
	trace bool
}

type listenBrowserListMsg struct {
	seq    int
	result Result
	at     time.Time
	read   time.Time
}
type listenBrowserDetailMsg struct {
	seq   int
	lines []string
}

func runListenBrowser(options commonOptions, families listenFamilies, full bool) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := newListenBrowser(ctx, options.timeout, richEnabled(os.Stdout), options.redact)
	model.load = func(ctx context.Context) Result {
		result := probeListen(ctx, families)
		if sockets, ok := result.Metrics["sockets"].([]listenSocket); ok {
			// PROCESS 칸의 인터프리터 꼬리표에 쓰려고 명령줄은 늘 읽는다. 목록 아래쪽의 전체 명령줄은 --full에서만 보인다.
			attachListenCommands(sockets)
			if err := attachListenContainers(ctx, sockets); err != nil {
				result.Warnings = append(result.Warnings, infoSingleLine(err.Error()))
			}
		}
		return result
	}
	color := model.color
	model.inspect = func(ctx context.Context, row listenRow) []string { return inspectListenSocket(ctx, row, color) }
	model.openTop = listenBrowserOpenTop
	model.openStop = openListenStopProcess
	model.full = full
	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newListenBrowser(ctx context.Context, timeout time.Duration, color, redact bool) listenBrowser {
	return listenBrowser{work: make(chan struct{}, 1), ctx: ctx, timeout: timeout, width: 80, height: 24, color: color, redact: redact, loading: true}
}

func (model listenBrowser) Init() tea.Cmd { return model.loadList() }

func (model listenBrowser) loadList() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, model.timeout)
		defer cancel()
		select {
		case model.work <- struct{}{}:
			defer func() { <-model.work }()
		case <-ctx.Done():
			return listenBrowserListMsg{seq: model.seq, result: Result{Status: StatusFail, Summary: ctx.Err().Error()}}
		}
		read := time.Now()
		return listenBrowserListMsg{seq: model.seq, result: model.load(ctx), at: time.Now(), read: read}
	}
}

func (model listenBrowser) loadDetail() tea.Cmd {
	detail := *model.detail
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(model.ctx, model.timeout)
		defer cancel()
		select {
		case model.work <- struct{}{}:
			defer func() { <-model.work }()
		case <-ctx.Done():
			return listenBrowserDetailMsg{seq: detail.seq, lines: []string{ctx.Err().Error()}}
		}
		return listenBrowserDetailMsg{seq: detail.seq, lines: model.inspect(ctx, detail.row)}
	}
}

func (model listenBrowser) selectedRow() (listenRow, bool) {
	if model.cursor < 0 || model.cursor >= len(model.items) {
		return listenRow{}, false
	}
	return model.items[model.cursor], true
}

// selected는 고른 줄의 첫 소켓이다.
func (model listenBrowser) selected() (listenSocket, bool) {
	row, ok := model.selectedRow()
	if !ok {
		return listenSocket{}, false
	}
	return row.first(), true
}

// activePID는 t와 x가 다룰 PID다. p로 고른 PID가 그 줄에 남아 있으면 그 PID이고, 아니면 줄의 첫 PID다. 새로고침으로
// 고른 process가 사라지면 첫 PID로 돌아간다.
func (model listenBrowser) activePID(row listenRow) string {
	pids := row.pids()
	if len(pids) == 0 {
		return ""
	}
	if row.key() == model.pidRow && slices.Contains(pids, model.pid) {
		return model.pid
	}
	return pids[0]
}

// activeSocket은 activePID가 가진 첫 소켓이다. --full의 명령줄을 그 process의 것으로 보이려고 쓴다.
func (model listenBrowser) activeSocket(row listenRow) listenSocket {
	pid := model.activePID(row)
	for _, socket := range row.Sockets {
		if socket.PID == pid {
			return socket
		}
	}
	return row.first()
}

// nextPID는 줄의 다음 PID를 고른다. PID가 하나뿐인 줄에서는 아무것도 바꾸지 않는다.
func (model *listenBrowser) nextPID(row listenRow) bool {
	pids := row.pids()
	if len(pids) < 2 {
		return false
	}
	at := slices.Index(pids, model.activePID(row))
	model.pidRow, model.pid = row.key(), pids[(at+1)%len(pids)]
	return true
}

// pidChoice는 PID가 여럿인 줄에서 지금 고른 PID와 바꾸는 키를 알린다. PID가 하나면 빈 문자열이다.
func (model listenBrowser) pidChoice(row listenRow) string {
	pids := row.pids()
	if len(pids) < 2 {
		return ""
	}
	active := model.activePID(row)
	return T("listenbrowse.pid_choice", active, slices.Index(pids, active)+1, len(pids))
}

// rebuild는 소켓을 줄로 모으고 loopback 숨김과 필터를 적용한다. 고른 줄이 남아 있으면 그 줄에 커서를 두고 true를 돌려준다.
// 남아 있지 않으면 커서는 같은 위치에 머문다.
func (model *listenBrowser) rebuild() bool {
	selected, hadSelection := model.selectedRow()
	model.items = model.items[:0:0]
	for _, row := range groupListenSockets(model.sockets) {
		if model.hideLoopback && row.scope() == "loopback" {
			continue
		}
		if !model.matchesFilter(row) {
			continue
		}
		model.items = append(model.items, row)
	}
	found := false
	if hadSelection {
		for i, row := range model.items {
			if row.key() == selected.key() {
				model.cursor, found = i, true
				break
			}
		}
	}
	model.keepCursor()
	return found
}

// matchesFilter는 줄이 필터의 모든 낱말을 담는지다. 낱말은 공백으로 가르고 대소문자를 가리지 않으며, 포트와 프로토콜, 주소,
// 범위, 프로세스, PID, 컨테이너 중 어디에 있어도 된다. 화면에 보이는 글자로 맞추므로 --redact로 가린 주소는 필터로 짚을 수 없다.
func (model listenBrowser) matchesFilter(row listenRow) bool {
	terms := strings.Fields(strings.ToLower(model.filter))
	if len(terms) == 0 {
		return true
	}
	first := row.first()
	port := fmt.Sprintf("%d/%s", first.Port, first.Proto)
	if first.Port == 0 {
		port = first.Proto
	}
	fields := []string{port, model.rowAddresses(row), row.scope(), listenBrowserScope(row.scope()), first.Process, row.joined(func(socket listenSocket) string { return socket.PID })}
	if first.Container != nil {
		container := listenContainerLabel(*first.Container)
		if model.redact {
			container = redactIPAddresses(container)
		}
		fields = append(fields, container)
	}
	text := strings.ToLower(strings.Join(fields, " "))
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

// applyFilter는 바뀐 필터로 목록을 다시 모은다. 고른 줄이 걸러졌으면 맨 위로 돌아간다.
func (model *listenBrowser) applyFilter() {
	if !model.rebuild() {
		model.cursor, model.offset = 0, 0
	}
	model.keepCursor()
}

// updateFilterKey는 필터 글자를 받는 동안의 키다. q는 글자이므로 끝내지 않고, Ctrl-C만 끝낸다.
func (model listenBrowser) updateFilterKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		return model, tea.Quit
	case "enter":
		model.filtering = false
		return model, nil
	case "esc":
		model.filtering, model.filter = false, ""
	case "backspace":
		if runes := []rune(model.filter); len(runes) > 0 {
			model.filter = string(runes[:len(runes)-1])
		}
	case "up":
		model.cursor--
		model.keepCursor()
		return model, nil
	case "down":
		model.cursor++
		model.keepCursor()
		return model, nil
	default:
		if key.Text == "" {
			return model, nil
		}
		model.filter += key.Text
	}
	model.applyFilter()
	return model, nil
}

func (model *listenBrowser) keepCursor() {
	model.cursor = max(0, min(model.cursor, len(model.items)-1))
	rows := max(1, model.height-5)
	if model.cursor < model.offset {
		model.offset = model.cursor
	}
	if model.cursor >= model.offset+rows {
		model.offset = model.cursor - rows + 1
	}
	model.offset = max(0, min(model.offset, max(0, len(model.items)-rows)))
}

func (model listenBrowser) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case listenBrowserTopDoneMsg:
		if value.err != nil {
			model.notice = T("listenbrowse.navigation_failed", infoSingleLine(value.err.Error()))
		} else {
			model.notice = T("listenbrowse.navigation_returned")
		}
		return model, nil
	case tea.WindowSizeMsg:
		widthChanged := model.width != max(1, value.Width)
		model.width, model.height = max(1, value.Width), max(1, value.Height)
		model.keepCursor()
		// 크기 안내만 보이는 화면에서는 확인 상자가 보이지 않으므로, y가 보이지 않는 질문에 답하지 않게 취소한다.
		if model.stop != nil && model.tooSmall() {
			model.stop.process.close()
			model.stop, model.notice = nil, T("listenbrowse.stop_cancelled")
		}
		if model.detail != nil && widthChanged {
			detail := *model.detail
			detail.offset = 0
			model.detail = &detail
		}
	case listenBrowserListMsg:
		if value.seq != model.seq {
			return model, nil
		}
		model.loading = false
		stopped := model.stopped
		model.stopped = 0
		sockets, ok := value.result.Metrics["sockets"].([]listenSocket)
		if !ok || value.result.Status == StatusFail || value.result.Status == StatusSkip {
			model.err = infoSingleLine(value.result.Summary)
			if model.err == "" {
				model.err = T("listenbrowse.unreadable")
			}
			return model, nil
		}
		model.sockets, model.warnings, model.loaded, model.updated, model.err = sockets, value.result.Warnings, true, value.at, ""
		model.listed = value.read
		if !model.rebuild() {
			model.cursor = 0
			model.keepCursor()
		}
		if stopped > 0 {
			model.notice = model.stopResult(stopped)
		}
	case listenStopDoneMsg:
		if value.err != nil {
			model.notice = listenStopMessage(value.pid, value.err)
			return model, nil
		}
		model.notice = T("listenbrowse.stop_sent", value.name, value.pid)
		// SIGTERM을 받은 process가 소켓을 닫을 틈을 두고 목록을 다시 읽는다.
		return model, tea.Tick(time.Second, func(time.Time) tea.Msg { return listenStopCheckMsg{pid: value.pid} })
	case listenStopCheckMsg:
		// 시그널 전에 시작한 새로고침의 결과로 판정하지 않게, 이 확인이 시작한 읽기에만 stopped를 건다.
		model.stopped = value.pid
		model.seq++
		model.loading = true
		return model, model.loadList()
	case listenBrowserDetailMsg:
		if model.detail == nil || model.detail.seq != value.seq {
			return model, nil
		}
		detail := *model.detail
		detail.lines, detail.loading = value.lines, false
		model.detail = &detail
	case tea.KeyPressMsg:
		key := value.String()
		if model.stop != nil {
			return model.answerStop(key)
		}
		if model.filtering {
			return model.updateFilterKey(value)
		}
		if key == "q" || key == "ctrl+c" {
			return model, tea.Quit
		}
		model.notice = ""
		if key == "t" || key == "x" {
			return model.navigateProcess(key)
		}
		if key == "S" {
			return model.askStop(), nil
		}
		if key == "p" {
			if row, ok := model.navigationRow(); ok && model.nextPID(row) && model.detail != nil && model.detail.trace {
				detail := *model.detail
				detail.lines, detail.offset = listenBrowserTraceHint(model.activePID(row)), 0
				model.detail = &detail
			}
			return model, nil
		}
		if model.detail != nil {
			detail := *model.detail
			switch key {
			case "esc", "backspace":
				model.detail = nil
				return model, nil
			case "r":
				model.detailSeq++
				detail.seq, detail.loading, detail.trace = model.detailSeq, true, false
				model.detail = &detail
				return model, model.loadDetail()
			case "up", "k":
				detail.offset--
			case "down", "j":
				detail.offset++
			case "pgup":
				detail.offset -= max(1, model.height-3)
			case "pgdown":
				detail.offset += max(1, model.height-3)
			case "home", "g":
				detail.offset = 0
			case "end", "G":
				detail.offset = len(model.detailLines())
			}
			detail.offset = max(0, min(detail.offset, max(0, len(model.detailLines())-max(1, model.height-3))))
			model.detail = &detail
			return model, nil
		}
		switch key {
		case "up", "k":
			model.cursor--
		case "down", "j":
			model.cursor++
		case "pgup":
			model.cursor -= max(1, model.height-5)
		case "pgdown":
			model.cursor += max(1, model.height-5)
		case "home", "g":
			model.cursor = 0
		case "end", "G":
			model.cursor = len(model.items) - 1
		case "r":
			if !model.loading {
				model.seq++
				model.loading = true
				return model, model.loadList()
			}
		case "enter":
			if row, ok := model.selectedRow(); ok {
				model.detailSeq++
				model.detail = &listenBrowserDetail{row: row, loading: true, seq: model.detailSeq}
				return model, model.loadDetail()
			}
		case "l":
			model.hideLoopback = !model.hideLoopback
			model.rebuild()
			return model, nil
		case "/":
			model.filtering = true
			return model, nil
		case "esc":
			// 목록에서 Esc는 필터를 푼다. 필터가 없으면 아무것도 하지 않는다.
			if model.filter != "" {
				model.filter = ""
				model.applyFilter()
			}
			return model, nil
		}
		model.keepCursor()
	}
	return model, nil
}

func (model listenBrowser) detailLines() []string {
	var lines []string
	for _, line := range model.detail.lines {
		lines = append(lines, strings.Split(ansi.Hardwrap(model.detailLine(line), max(1, model.width), true), "\n")...)
	}
	return lines
}

// detailLine은 상세 결과 한 줄을 화면에 맞게 정리한다. 색이 켜져 있으면 결과 색만 남긴다.
// --redact에서 주소가 색 경계에 걸리면 가려지지 않을 수 있으므로, 주소가 있는 줄은 색을 빼고 가린다.
func (model listenBrowser) detailLine(line string) string {
	plain := viewSanitize(line)
	if model.redact {
		if redacted := redactIPAddresses(plain); redacted != plain {
			return redacted
		}
	}
	if !liveRich(model.color) {
		return plain
	}
	return viewSanitizeKeepColor(line)
}

func (model listenBrowser) View() tea.View {
	if model.tooSmall() {
		view := tea.NewView(ansi.Truncate(T("listenbrowse.small"), model.width, "…"))
		view.AltScreen = true
		return view
	}
	lines := []string{}
	if model.detail != nil {
		title := listenBrowserRowName(model.detail.row)
		if model.redact {
			title = redactIPAddresses(title)
		}
		notice := model.notice
		if notice == "" {
			notice = model.pidChoice(model.detail.row)
		}
		lines = append(lines, svcBrowserHeading("edc listen", []string{viewSanitize(title)}, model.color), liveKeyHints(T("listenbrowse.detail_keys"), model.color), notice)
		if model.detail.loading {
			lines = append(lines, T("listenbrowse.loading"))
		} else {
			rows := model.detailLines()
			start := min(model.detail.offset, len(rows))
			end := min(len(rows), start+max(1, model.height-3))
			lines = append(lines, rows[start:end]...)
		}
	} else {
		state := T("listenbrowse.manual")
		if model.loading {
			state = T("listenbrowse.loading")
		} else if !model.updated.IsZero() {
			state = T("listenbrowse.updated", model.updated.Format("15:04:05"))
		}
		counts := []string{T("listenbrowse.count", len(model.sockets))}
		if len(model.items) != len(model.sockets) {
			counts = append(counts, T("listenbrowse.rows", len(model.items)))
		}
		if model.hideLoopback {
			counts = append(counts, T("listenbrowse.loopback_hidden"))
		}
		if model.filter != "" {
			counts = append(counts, T("listenbrowse.filtered", viewSanitize(model.filter)))
		}
		keys := T("listenbrowse.keys")
		switch {
		case model.filtering:
			keys = T("listenbrowse.filter_input", viewSanitize(model.filter))
		case model.filter != "":
			keys = T("listenbrowse.keys_filtered")
		}
		lines = append(lines, svcBrowserHeading("edc listen", append(counts, state), model.color), liveKeyHints(keys, model.color, liveHintItem{key: "l", on: model.hideLoopback}, liveHintItem{key: "/", on: model.filter != "" && !model.filtering}))
		notice := T("listenbrowse.reachability")
		if model.notice != "" {
			notice = model.notice
		} else if model.err != "" {
			notice = T("listenbrowse.refresh_failed", model.err)
		} else if len(model.warnings) > 0 {
			notice = strings.Join(model.warnings, " · ")
		}
		if model.redact {
			notice = redactIPAddresses(notice)
		}
		// 범례는 정리한 안내 뒤가 아니라 앞에 붙인다. viewSanitize가 색을 지운다.
		lines = append(lines, listenBrowserLegend(model.color)+viewSanitize(notice))
		if len(model.items) == 0 {
			if model.loaded {
				if model.filter != "" && len(model.sockets) > 0 {
					lines = append(lines, T("listenbrowse.no_match"))
				} else {
					lines = append(lines, T("listenbrowse.empty"))
				}
			}
		} else {
			// 고정 칸(PORT/PROTO 10, PID 10, 간격과 선택 표시 8)을 뺀 폭에서 주소 칸은 보이는 주소만큼, 많아야 절반을 쓴다.
			// 나머지는 프로세스 칸이다. 더 긴 주소는 잘리고 목록 아래쪽에 모두 나온다.
			visible := model.items[model.offset:min(len(model.items), model.offset+max(1, model.height-5))]
			addressWidth := liveWidth("ADDRESS")
			for _, item := range visible {
				addressWidth = max(addressWidth, liveWidth(topPrintableText(model.rowAddresses(item))))
			}
			// 색이 없으면 주소의 색이 말하던 범위를 SCOPE 칸에 글자로 적는다. 칸이 생긴 만큼 주소와 프로세스 칸이 줄어든다.
			scopeWidth, free := 0, model.width-28
			if !liveRich(model.color) {
				scopeWidth = liveWidth("SCOPE")
				for _, item := range visible {
					scopeWidth = max(scopeWidth, liveWidth(listenBrowserScope(item.scope())))
				}
				free -= scopeWidth + 2
			}
			addressWidth = max(min(9, free/2), min(addressWidth, free/2))
			processWidth := max(1, free-addressWidth)
			row := func(port, address, scope, process, pid string) string {
				cells := svcBrowserCell(port, 10) + "  " + address + "  "
				if scopeWidth > 0 {
					cells += svcBrowserCell(scope, scopeWidth) + "  "
				}
				return cells + svcBrowserCell(process, processWidth) + "  " + svcBrowserCell(pid, 10)
			}
			lines = append(lines, liveMuted("  "+row("PORT/PROTO", svcBrowserCell("ADDRESS", addressWidth), "SCOPE", "PROCESS", "PID"), model.color))
			home, _ := os.UserHomeDir()
			end := min(len(model.items), model.offset+max(1, model.height-5))
			for i := model.offset; i < end; i++ {
				item := model.items[i]
				first := item.first()
				port := fmt.Sprintf("%d/%s", first.Port, first.Proto)
				if first.Port == 0 {
					port = first.Proto
				}
				// 글자는 가린 뒤에 칠하고 자른다. 잘린 IPv6 주소는 가리는 패턴에 맞지 않을 수 있고, 칠한 뒤에는 색 코드가
				// 주소에 붙는다. svcBrowserCell은 색 코드를 폭에 세지 않고 자른다.
				process := topPrintableText(listenCell(first.Process))
				label := ""
				if text := listenProcessLabel(model.activeSocket(item), home); text != "" {
					label = " · " + text
				}
				container := ""
				if first.Container != nil {
					container = topPrintableText(" → " + listenContainerLabel(*first.Container))
				}
				if model.redact {
					process, label, container = redactIPAddresses(process), redactIPAddresses(label), redactIPAddresses(container)
				}
				process += model.paint(label, richGray, false) + model.paint(container, richCyan, false)
				address := model.paint(topPrintableText(model.rowAddresses(item)), listenScopeColor(item.scope()), false)
				lines = append(lines, svcBrowserRow(row(model.paint(port, "", true), svcBrowserCell(address, addressWidth), listenBrowserScope(item.scope()), process, model.paint(model.pidCell(item), richGray, false)), i == model.cursor, model.color, model.width))
			}
			if item, ok := model.selectedRow(); ok {
				footer := T("listenbrowse.address", model.rowAddresses(item))
				// p 키는 이 줄에만 나온다. 긴 주소 목록 뒤에 두면 폭에서 잘리므로 앞에 둔다.
				if choice := model.pidChoice(item); choice != "" {
					footer = choice + " · " + footer
				}
				if model.full {
					if command := listenCommandLine(model.activeSocket(item), home); command != "" {
						footer += " · " + command
					}
				}
				if model.redact {
					footer = redactIPAddresses(footer)
				}
				lines = append(lines, viewSanitize(footer))
			}
		}
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, model.width, "…")
	}
	lines = lines[:min(len(lines), model.height)]
	if model.stop != nil {
		// 확인 상자가 떠 있는 동안 배경은 색을 걷고 회색으로 칠해, 눈이 상자에 먼저 가게 한다.
		for i, line := range lines {
			lines[i] = topDim(ansi.Strip(line), liveRich(model.color))
		}
	}
	content := strings.Join(lines, "\n")
	if model.stop != nil {
		content = overlayTopBox(content, model.stopBox(), model.width, liveRich(model.color))
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// rowAddresses는 줄의 주소를 잇는다. --redact면 주소마다 가린다.
func (model listenBrowser) rowAddresses(row listenRow) string {
	addresses := row.addresses()
	if model.redact {
		for i, address := range addresses {
			addresses[i] = redactIPAddresses(address)
		}
	}
	return strings.Join(addresses, ", ")
}

// paint는 색을 쓸 때만 칠한다. 색이 없는 범위(unix, unknown)는 굵기만 적용하거나 그대로 둔다.
func (model listenBrowser) paint(text, color string, bold bool) string {
	switch {
	case !liveRich(model.color) || text == "":
		return text
	case color == "" && bold:
		return richBold(text)
	case color == "":
		return text
	}
	return richPaint(text, color, bold)
}

// pidCell은 PID 칸이다. PID가 여럿이면 t와 x가 다룰 PID 뒤에 나머지 개수를 붙인다.
func (model listenBrowser) pidCell(row listenRow) string {
	if pids := row.pids(); len(pids) > 1 {
		return fmt.Sprintf("%s +%d", model.activePID(row), len(pids)-1)
	}
	return listenCell(row.first().PID)
}

// listenBrowserScope는 색이 없을 때 SCOPE 칸에 적는 범위다. 표의 범위 문구는 탐색 화면의 칸에 넣기에 길어서 짧은 문구를 쓴다.
func listenBrowserScope(scope string) string {
	switch scope {
	case "all", "specific", "loopback":
		return T("listenbrowse.scope." + scope)
	}
	return T("observe.listen.scope." + scope)
}

// listenBrowserLegend는 주소 색의 뜻이다. 색이 없으면 범례도 없다.
func listenBrowserLegend(color bool) string {
	if !liveRich(color) {
		return ""
	}
	var parts []string
	for _, scope := range []string{"all", "specific", "loopback"} {
		parts = append(parts, richPaint("●", listenScopeColor(scope), false)+" "+T("observe.listen.scope."+scope))
	}
	return strings.Join(parts, "  ") + " · "
}

func listenBrowserRowName(row listenRow) string {
	first := row.first()
	if first.Port == 0 {
		return first.Proto + " " + first.Address
	}
	return fmt.Sprintf("%d/%s · %s · PID %s", first.Port, first.Proto, strings.Join(row.addresses(), ", "), row.joined(func(socket listenSocket) string { return socket.PID }))
}

func inspectListenSocket(ctx context.Context, row listenRow, color bool) []string {
	first := row.first()
	lines := []string{T("listenbrowse.address", strings.Join(row.addresses(), ", ")), T("listenbrowse.owner", listenCell(first.Process), row.joined(func(socket listenSocket) string { return socket.PID }))}
	if len(row.Sockets) == 1 {
		lines = append(lines, T("listenbrowse.queue", listenQueueCell(first)))
	} else {
		for _, socket := range row.Sockets {
			lines = append(lines, T("listenbrowse.socket", socket.Address, listenCell(socket.PID), listenQueueCell(socket)))
		}
	}
	if first.Container != nil {
		lines = append(lines, T("listenbrowse.container", listenContainerLabel(*first.Container)))
	}
	for i := range lines {
		lines[i] = viewSanitize(lines[i])
	}
	pids := row.pids()
	if len(pids) == 0 {
		return append(lines, "", T("listenbrowse.no_owner"))
	}
	for _, pid := range pids {
		if value, err := strconv.Atoi(pid); err != nil || value <= 0 {
			lines = append(lines, "", T("listenbrowse.no_owner"))
			continue
		}
		results, _, err := collectProcResults(ctx, pid, true)
		if err != nil {
			lines = append(lines, "", viewSanitize(T("listenbrowse.process_failed", infoSingleLine(err.Error()))))
			continue
		}
		var buffer strings.Builder
		printTerminalWithColor(&buffer, viewSanitizeResults(results), true, color)
		lines = append(lines, "", strings.TrimRight(buffer.String(), "\n"))
	}
	return strings.Split(strings.Join(lines, "\n"), "\n")
}

// viewSanitizeResults는 결과에 든 escape와 제어 문자를 출력 전에 지운다. 상세 화면은 edc가 입힌 색을 남기는데,
// 프로세스 이름이나 명령줄에 든 색 escape는 그것과 구별할 수 없어서 미리 지운다. 여러 줄 값은 줄바꿈을 유지한다.
func viewSanitizeResults(results []Result) []Result {
	clean := func(value string) string {
		lines := strings.Split(value, "\n")
		for i := range lines {
			lines[i] = viewSanitize(lines[i])
		}
		return strings.Join(lines, "\n")
	}
	sanitized := make([]Result, len(results))
	for i, result := range results {
		result.Summary, result.Next = clean(result.Summary), clean(result.Next)
		result.Warnings = append([]string(nil), result.Warnings...)
		for j := range result.Warnings {
			result.Warnings[j] = clean(result.Warnings[j])
		}
		result.Evidence = append([]Evidence(nil), result.Evidence...)
		for j := range result.Evidence {
			result.Evidence[j].Label, result.Evidence[j].Value = clean(result.Evidence[j].Label), clean(result.Evidence[j].Value)
		}
		if result.Error != nil {
			diagnostic := *result.Error
			diagnostic.Kind, diagnostic.Message = clean(diagnostic.Kind), clean(diagnostic.Message)
			result.Error = &diagnostic
		}
		if result.Collection != nil {
			observation := *result.Collection
			observation.Reason = clean(observation.Reason)
			result.Collection = &observation
		}
		sanitized[i] = result
	}
	return sanitized
}

var _ tea.Model = listenBrowser{}
