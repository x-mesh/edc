package edc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// aiSessionsDetailLines는 고른 세션의 설명 칸이다. 목록은 나머지 높이를 쓴다.
const aiSessionsDetailLines = 12

func runAISessionsDashboard(path string, needs, all, bell bool) int {
	model := aiSessionsModel{tail: &aiEventTail{path: path}, needs: needs, all: all, bell: bell, alerted: map[string]time.Time{}, activities: aiActivityReader{}}
	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

type aiSessionsModel struct {
	tail          *aiEventTail
	events        []aiHookEvent
	sessions      []aiSession
	now           time.Time
	needs, all    bool
	bell          bool
	loaded        bool
	err           error
	width, height int
	// selected는 고른 세션의 키다. 순서가 바뀌어도 같은 세션을 가리킨다.
	selected   string
	activities aiActivityReader
	// alerted는 세션마다 마지막으로 알린 입력 요청의 시각이다. 같은 요청을 두 번 알리지 않는다.
	alerted map[string]time.Time
}

type aiSessionsReadMsg struct {
	events []aiHookEvent
	err    error
	at     time.Time
}

func (model aiSessionsModel) Init() tea.Cmd { return model.readCmd(0) }

// readCmd는 앞선 읽기가 끝난 뒤에만 다음 읽기를 건다. tail을 두 goroutine이 함께 고치지 않는다.
func (model aiSessionsModel) readCmd(delay time.Duration) tea.Cmd {
	tail := model.tail
	read := func(at time.Time) tea.Msg {
		err := tail.read()
		return aiSessionsReadMsg{events: tail.events, err: err, at: at}
	}
	if delay == 0 {
		return func() tea.Msg { return read(time.Now()) }
	}
	return tea.Tick(delay, func(at time.Time) tea.Msg { return read(at) })
}

func (model aiSessionsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case aiSessionsReadMsg:
		model.events, model.err, model.now = value.events, value.err, value.at
		fresh := model.refresh()
		model.loaded = true
		next := model.readCmd(aiScanInterval)
		if fresh && model.bell {
			return model, tea.Batch(next, aiBell)
		}
		return model, next
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
	case tea.KeyPressMsg:
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			model.move(-1)
		case "down", "j":
			model.move(1)
		case "n":
			model.needs = !model.needs
			model.refresh()
		case "a":
			model.all = !model.all
			model.refresh()
		}
	}
	return model, nil
}

// refresh는 이벤트를 다시 접는다. 첫 읽기 뒤에 새로 입력을 요청한 세션이 있으면 true를 돌려준다.
// 처음 읽은 요청은 화면을 열기 전부터 있던 것이므로 알리지 않는다.
func (model *aiSessionsModel) refresh() bool {
	model.sessions = selectAISessions(foldAIHookEvents(model.events), model.now, model.needs, model.all, aiProcessAlive, aiFileModified)
	if model.activities != nil {
		model.activities.annotate(model.sessions, model.now)
	}
	fresh := false
	for _, session := range model.sessions {
		if session.State != aiStateNeeds && session.State != aiStateFailed {
			continue
		}
		key := aiSessionKey(session)
		if seen, ok := model.alerted[key]; !ok || !seen.Equal(session.Since) {
			fresh = fresh || model.loaded
			model.alerted[key] = session.Since
		}
	}
	if model.index() < 0 && len(model.sessions) > 0 {
		model.selected = aiSessionKey(model.sessions[0])
	}
	return fresh
}

func aiBell() tea.Msg {
	_, _ = os.Stdout.WriteString("\a")
	return nil
}

func aiSessionKey(session aiSession) string { return session.Tool + "/" + session.Session }

func (model aiSessionsModel) index() int {
	for index, session := range model.sessions {
		if aiSessionKey(session) == model.selected {
			return index
		}
	}
	return -1
}

func (model *aiSessionsModel) move(step int) {
	if len(model.sessions) == 0 {
		return
	}
	index := min(max(model.index()+step, 0), len(model.sessions)-1)
	model.selected = aiSessionKey(model.sessions[index])
}

func (model aiSessionsModel) View() tea.View {
	width := model.width
	if width <= 0 {
		width = 100
	}
	lines := []string{model.title(width)}
	switch {
	case model.err != nil:
		lines = append(lines, richPaint(model.err.Error(), richRed, false))
	case !model.loaded:
		lines = append(lines, richDim("reading "+model.tail.path))
	case len(model.events) == 0:
		lines = append(lines, T("observe.ai.sessions.no_events", model.tail.path))
	case len(model.sessions) == 0:
		lines = append(lines, richDim(T("observe.ai.sessions.none")))
	default:
		lines = append(lines, model.rows()...)
		lines = append(lines, richDim(strings.Repeat("─", min(width, 100))))
		lines = append(lines, model.detail(width)...)
	}
	lines = append(lines, liveKeyHints("↑↓ select · n needs only · a ended · q quit", true,
		liveHintItem{key: "n", on: model.needs}, liveHintItem{key: "a", on: model.all}))
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model aiSessionsModel) title(width int) string {
	counts := map[aiSessionState]int{}
	for _, session := range model.sessions {
		counts[session.State]++
	}
	var parts []string
	for _, state := range []aiSessionState{aiStateNeeds, aiStateFailed, aiStateWorking, aiStateWaiting} {
		if counts[state] > 0 {
			parts = append(parts, aiStateMarks[state]+" "+strconv.Itoa(counts[state]))
		}
	}
	left := richPaint("🐰 edc ai sessions", richMagenta, true) + richDim(" · this host")
	right := strings.Join(append(parts, "live", model.now.Local().Format("15:04:05 MST")), " · ")
	if counts[aiStateNeeds] > 0 {
		right = richPaint(right, richRed, true)
	} else {
		right = richDim(right)
	}
	space := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if space < 2 {
		return left + "  " + right
	}
	return left + strings.Repeat(" ", space) + right
}

// rows는 고른 세션이 보이도록 목록을 자른다.
func (model aiSessionsModel) rows() []string {
	limit := len(model.sessions)
	if model.height > 0 {
		limit = max(1, model.height-aiSessionsDetailLines-3)
	}
	first := 0
	if index := model.index(); index >= limit {
		first = index - limit + 1
	}
	var lines []string
	for index := first; index < min(len(model.sessions), first+limit); index++ {
		session := model.sessions[index]
		line := aiSessionFit(aiSessionHead(session, model.now), aiSessionDetail(session, model.now), session, model.lineWidth()-1, true)
		if aiSessionKey(session) == model.selected {
			lines = append(lines, liveSelected(liveSelectedBar+line, true))
			continue
		}
		lines = append(lines, " "+line)
	}
	return lines
}

func (model aiSessionsModel) lineWidth() int {
	if model.width <= 0 {
		return 100
	}
	return model.width
}

// aiSessionInfoLines는 상세 칸의 모델, 컨텍스트, 턴, token, 오류 줄이다. 모르는 값은 줄째 뺀다.
func aiSessionInfoLines(session aiSession, now time.Time) [][2]string {
	info := session.Info
	if info == nil {
		return nil
	}
	var lines [][2]string
	var settings []string
	for _, value := range []string{info.Model, info.Effort, info.Branch, info.Mode, info.Sandbox} {
		if value != "" {
			settings = append(settings, value)
		}
	}
	if len(settings) > 0 {
		lines = append(lines, [2]string{"model", strings.Join(settings, " · ")})
	}
	if info.Context > 0 {
		context := aiCompact(info.Context)
		if window, guessed := aiSessionWindow(session); window > 0 {
			context += fmt.Sprintf(" / %s (%d%%", aiCompact(window), info.Context*100/window)
			if guessed {
				context += ", estimated"
			}
			context += ")"
		}
		lines = append(lines, [2]string{"context", context})
	}
	if session.State == aiStateWorking && (info.TurnTools > 0 || info.TurnFailed > 0) {
		count := ""
		if info.TurnPartial {
			count = "≥"
		}
		turn := fmt.Sprintf("%s · %s%d tools", formatTopEventDuration(now.Sub(session.Since)), count, info.TurnTools)
		if info.TurnFailed > 0 {
			turn += fmt.Sprintf(" · %d failed", info.TurnFailed)
		}
		lines = append(lines, [2]string{"turn", turn})
	}
	if !info.Tokens.empty() {
		tokens := fmt.Sprintf("in %s · out %s · cache %s", aiCompact(info.Tokens.Input), aiCompact(info.Tokens.Output), aiCompact(info.Tokens.Cache))
		if !info.Last.empty() {
			tokens += fmt.Sprintf(" · last in %s (cache %s) out %s", aiCompact(info.Last.Input+info.Last.Cache), info.Last.cacheHitText(), aiCompact(info.Last.Output))
		}
		if info.Rate > 0 {
			tokens += " · " + aiCompact(info.Rate) + "/min"
		}
		lines = append(lines, [2]string{"tokens", tokens})
	}
	if info.LastError != "" {
		lines = append(lines, [2]string{"error", info.LastError})
	}
	return lines
}

func aiPaintSession(state aiSessionState, line string) string {
	switch state {
	case aiStateNeeds:
		return richPaint(line, richRed, true)
	case aiStateFailed:
		return richPaint(line, richRed, false)
	case aiStateWaiting, aiStateEnded:
		return richDim(line)
	}
	return line
}

// detail은 목록에서 잘린 설명과 세션을 찾는 데 필요한 값을 보인다.
func (model aiSessionsModel) detail(width int) []string {
	index := model.index()
	if index < 0 {
		return nil
	}
	session := model.sessions[index]
	where := session.TTY
	if session.PID > 0 {
		where = strings.TrimSpace(where + " pid " + strconv.Itoa(session.PID))
	}
	if where == "" {
		where = "-"
	}
	lines := []string{
		richDim("session  ") + session.Tool + " " + session.Session,
		richDim("dir      ") + session.Cwd,
		richDim("terminal ") + where,
		richDim("state    ") + aiSessionStateText(session) + richDim(" since "+session.Since.Local().Format("15:04:05")),
	}
	if session.Task != "" {
		lines = append(lines, richDim("task     ")+session.Task)
	}
	for _, line := range aiSessionInfoLines(session, model.now) {
		text := line[1]
		if line[0] == "error" {
			text = richPaint(text, richRed, false)
		}
		lines = append(lines, richDim(padRight(line[0], 9))+text)
	}
	if detail := aiSessionDetail(session, model.now); detail != "" && detail != session.Task {
		wrapped := strings.Split(ansi.Wrap(detail, max(20, width-9), ""), "\n")
		for index, part := range wrapped[:min(len(wrapped), aiSessionsDetailLines-len(lines))] {
			label := "         "
			if index == 0 {
				label = "detail   "
			}
			lines = append(lines, richDim(label)+part)
		}
	}
	return lines
}
