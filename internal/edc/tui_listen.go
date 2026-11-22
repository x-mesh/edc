package edc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type listenBrowser struct {
	work                               chan struct{}
	full                               bool
	ctx                                context.Context
	load                               func(context.Context) Result
	inspect                            func(context.Context, listenSocket) []string
	timeout                            time.Duration
	items                              []listenSocket
	warnings                           []string
	err                                string
	loaded, loading                    bool
	updated                            time.Time
	cursor, offset, width, height, seq int
	color, redact                      bool
	detail                             *listenBrowserDetail
	detailSeq                          int
}

type listenBrowserDetail struct {
	socket      listenSocket
	lines       []string
	offset, seq int
	loading     bool
}

type listenBrowserListMsg struct {
	seq    int
	result Result
	at     time.Time
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
			if full {
				attachListenCommands(sockets)
			}
			if err := attachListenContainers(ctx, sockets); err != nil {
				result.Warnings = append(result.Warnings, infoSingleLine(err.Error()))
			}
		}
		return result
	}
	model.inspect = inspectListenSocket
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
		return listenBrowserListMsg{seq: model.seq, result: model.load(ctx), at: time.Now()}
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
		return listenBrowserDetailMsg{seq: detail.seq, lines: model.inspect(ctx, detail.socket)}
	}
}

func (model listenBrowser) selected() (listenSocket, bool) {
	if model.cursor < 0 || model.cursor >= len(model.items) {
		return listenSocket{}, false
	}
	return model.items[model.cursor], true
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
	case tea.WindowSizeMsg:
		model.width, model.height = max(1, value.Width), max(1, value.Height)
		model.keepCursor()
		if model.detail != nil {
			detail := *model.detail
			detail.offset = 0
			model.detail = &detail
		}
	case listenBrowserListMsg:
		if value.seq != model.seq {
			return model, nil
		}
		model.loading = false
		sockets, ok := value.result.Metrics["sockets"].([]listenSocket)
		if !ok || value.result.Status == StatusFail || value.result.Status == StatusSkip {
			model.err = infoSingleLine(value.result.Summary)
			if model.err == "" {
				model.err = T("listenbrowse.unreadable")
			}
			return model, nil
		}
		selected, hadSelection := model.selected()
		model.items, model.warnings, model.loaded, model.updated, model.err = sockets, value.result.Warnings, true, value.at, ""
		model.cursor = 0
		if hadSelection {
			for i, item := range sockets {
				if listenKeyOf(item) == listenKeyOf(selected) {
					model.cursor = i
					break
				}
			}
		}
		model.keepCursor()
	case listenBrowserDetailMsg:
		if model.detail == nil || model.detail.seq != value.seq {
			return model, nil
		}
		detail := *model.detail
		detail.lines, detail.loading = value.lines, false
		model.detail = &detail
	case tea.KeyPressMsg:
		key := value.String()
		if key == "q" || key == "ctrl+c" {
			return model, tea.Quit
		}
		if model.detail != nil {
			detail := *model.detail
			switch key {
			case "esc", "backspace":
				model.detail = nil
				return model, nil
			case "r":
				model.detailSeq++
				detail.seq, detail.loading = model.detailSeq, true
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
			if socket, ok := model.selected(); ok {
				model.detailSeq++
				model.detail = &listenBrowserDetail{socket: socket, loading: true, seq: model.detailSeq}
				return model, model.loadDetail()
			}
		}
		model.keepCursor()
	}
	return model, nil
}

func (model listenBrowser) detailLines() []string {
	var lines []string
	for _, line := range model.detail.lines {
		if model.redact {
			line = redactIPAddresses(line)
		}
		lines = append(lines, strings.Split(ansi.Hardwrap(viewSanitize(line), max(1, model.width), true), "\n")...)
	}
	return lines
}

func (model listenBrowser) View() tea.View {
	if model.width < 48 || model.height < 8 {
		view := tea.NewView(ansi.Truncate(T("listenbrowse.small"), model.width, "…"))
		view.AltScreen = true
		return view
	}
	lines := []string{}
	if model.detail != nil {
		title := listenBrowserSocketName(model.detail.socket)
		if model.redact {
			title = redactIPAddresses(title)
		}
		lines = append(lines, svcBrowserHeading("edc listen", []string{viewSanitize(title)}, model.color), liveKeyHints(T("listenbrowse.detail_keys"), model.color), "")
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
		lines = append(lines, svcBrowserHeading("edc listen", []string{T("listenbrowse.count", len(model.items)), state}, model.color), liveKeyHints(T("listenbrowse.keys"), model.color))
		notice := T("listenbrowse.reachability")
		if model.err != "" {
			notice = T("listenbrowse.refresh_failed", model.err)
		} else if len(model.warnings) > 0 {
			notice = strings.Join(model.warnings, " · ")
		}
		if model.redact {
			notice = redactIPAddresses(notice)
		}
		lines = append(lines, viewSanitize(notice))
		if len(model.items) == 0 {
			if model.loaded {
				lines = append(lines, T("listenbrowse.empty"))
			}
		} else {
			processWidth := max(7, model.width-41)
			row := func(port, scope, process, pid string) string {
				return svcBrowserCell(port, 10) + "  " + svcBrowserCell(scope, 16) + "  " + svcBrowserCell(process, processWidth) + "  " + svcBrowserCell(pid, 7)
			}
			lines = append(lines, liveMuted("  "+row("PORT/PROTO", "BINDING", "PROCESS", "PID"), model.color))
			end := min(len(model.items), model.offset+max(1, model.height-5))
			for i := model.offset; i < end; i++ {
				item := model.items[i]
				port := fmt.Sprintf("%d/%s", item.Port, item.Proto)
				if item.Port == 0 {
					port = item.Proto
				}
				process := listenCell(item.Process)
				if item.Container != nil {
					process += " → " + listenContainerLabel(*item.Container)
				}
				if model.redact {
					process = redactIPAddresses(process)
				}
				lines = append(lines, svcBrowserRow(row(port, T("observe.listen.scope."+listenBindingScope(item.Proto, item.Address)), topPrintableText(process), listenCell(item.PID)), i == model.cursor, model.color, model.width))
			}
			if item, ok := model.selected(); ok {
				address := item.Address
				if model.redact {
					address = redactIPAddresses(address)
				}
				footer := T("listenbrowse.address", address)
				if model.full {
					home, _ := os.UserHomeDir()
					if command := listenCommandLine(item, home); command != "" {
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
	view := tea.NewView(strings.Join(lines[:min(len(lines), model.height)], "\n"))
	view.AltScreen = true
	return view
}

func listenBrowserSocketName(socket listenSocket) string {
	if socket.Port == 0 {
		return socket.Proto + " " + socket.Address
	}
	return fmt.Sprintf("%d/%s · %s · PID %s", socket.Port, socket.Proto, socket.Address, listenCell(socket.PID))
}

func inspectListenSocket(ctx context.Context, socket listenSocket) []string {
	lines := []string{T("listenbrowse.address", socket.Address), T("listenbrowse.owner", listenCell(socket.Process), listenCell(socket.PID)), T("listenbrowse.queue", listenQueueCell(socket))}
	if socket.Container != nil {
		lines = append(lines, T("listenbrowse.container", listenContainerLabel(*socket.Container)))
	}
	pid, err := strconv.Atoi(socket.PID)
	if err != nil || pid <= 0 {
		return append(lines, "", T("listenbrowse.no_owner"))
	}
	results, _, err := collectProcResults(ctx, socket.PID, true)
	if err != nil {
		return append(lines, "", T("listenbrowse.process_failed", infoSingleLine(err.Error())))
	}
	var buffer strings.Builder
	printTerminalWithColor(&buffer, results, true, false)
	lines = append(lines, "", strings.TrimRight(buffer.String(), "\n"))
	return strings.Split(strings.Join(lines, "\n"), "\n")
}

var _ tea.Model = listenBrowser{}
