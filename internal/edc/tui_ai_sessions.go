package edc

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// aiSessionsDetailLines는 고른 세션의 설명 칸이다. 목록은 나머지 높이를 쓴다.
const aiSessionsDetailLines = 12

func runAISessionsDashboard(path string, needs, all, bell bool) int {
	home, _ := os.UserHomeDir()
	model := aiSessionsModel{tail: &aiEventTail{path: path}, needs: needs, all: all, bell: bell, alerted: map[string]time.Time{}, activities: aiActivityReader{},
		tasks: newAITaskReader(), claudeDir: aiClaudeDir(home)}
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
	// tasks는 세션 아래에 붙일 agent와 shell을 읽는다. 읽기는 readCmd 한 줄기에서만 돈다. running은 마지막으로 읽은 결과다.
	tasks     *aiTaskReader
	claudeDir string
	running   []aiTask
	// activity는 Enter로 연 활동 화면이다. edc ai agents와 같은 화면을 쓴다. activityGen은 연 순서이고, 닫은 화면이 건
	// 읽기의 결과는 세대가 달라 버린다. 버린 결과는 다음 읽기를 걸지 않으므로 읽기가 쌓이지 않는다.
	activity    *aiAgentsModel
	activityGen int
}

type aiSessionsReadMsg struct {
	events []aiHookEvent
	tasks  []aiTask
	err    error
	at     time.Time
}

func (model aiSessionsModel) Init() tea.Cmd { return model.readCmd(0) }

// readCmd는 앞선 읽기가 끝난 뒤에만 다음 읽기를 건다. tail을 두 goroutine이 함께 고치지 않는다.
func (model aiSessionsModel) readCmd(delay time.Duration) tea.Cmd {
	tail, tasks, claudeDir := model.tail, model.tasks, model.claudeDir
	read := func(at time.Time) tea.Msg {
		err := tail.read()
		msg := aiSessionsReadMsg{events: tail.events, err: err, at: at}
		if tasks != nil {
			// Claude Code를 쓰지 않는 host에는 projects 디렉터리가 없다. 세션 목록은 그대로 보인다.
			msg.tasks, _ = tasks.list(claudeDir, at)
		}
		return msg
	}
	if delay == 0 {
		return func() tea.Msg { return read(time.Now()) }
	}
	return tea.Tick(delay, func(at time.Time) tea.Msg { return read(at) })
}

func (model aiSessionsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case aiAgentsReadMsg:
		if model.activity == nil || value.generation != model.activity.generation {
			return model, nil
		}
		next, cmd := model.activity.Update(value)
		activity := next.(aiAgentsModel)
		model.activity = &activity
		return model, cmd
	case aiSessionsReadMsg:
		model.events, model.err, model.now, model.running = value.events, value.err, value.at, value.tasks
		fresh := model.refresh()
		model.loaded = true
		next := model.readCmd(aiScanInterval)
		if fresh && model.bell {
			return model, tea.Batch(next, aiBell)
		}
		return model, next
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		if model.activity != nil {
			model.activity.width, model.activity.height = value.Width, value.Height
		}
	case tea.KeyPressMsg:
		if model.activity != nil {
			switch value.String() {
			case "esc", "left", "h", "backspace":
				model.activity = nil
				return model, nil
			case "q", "ctrl+c":
				return model, tea.Quit
			}
			next, cmd := model.activity.Update(value)
			activity := next.(aiAgentsModel)
			model.activity = &activity
			return model, cmd
		}
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		case "enter", "right", "l":
			return model.openActivity()
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
	attachAITasks(model.sessions, model.running)
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
	if !slices.Contains(model.rowKeys(), model.selected) && len(model.sessions) > 0 {
		model.selected = aiSessionKey(model.sessions[0])
	}
	return fresh
}

// aiChildKey는 세션 아래 줄의 키다. 세션 키와 겹치지 않는다.
func aiChildKey(task aiTask) string { return "task/" + aiTaskKey(task) }

// rowKeys는 화면에 그린 순서대로의 줄 키다. 세션 줄 바로 뒤에 그 세션의 agent, shell, monitor 줄이 온다.
func (model aiSessionsModel) rowKeys() []string {
	var keys []string
	for _, session := range model.sessions {
		keys = append(keys, aiSessionKey(session))
		for _, task := range session.Tasks {
			keys = append(keys, aiChildKey(task))
		}
	}
	return keys
}

// selectedTask는 고른 하위 줄의 작업이다. 세션 줄을 골랐으면 false다.
func (model aiSessionsModel) selectedTask() (aiTask, bool) {
	for _, session := range model.sessions {
		for _, task := range session.Tasks {
			if aiChildKey(task) == model.selected {
				return task, true
			}
		}
	}
	return aiTask{}, false
}

// openActivity는 고른 줄의 활동 화면을 연다. 세션 줄은 Claude 세션의 본 대화를 연다. Codex 세션과 기록 경로를 모르는 세션은 열지 않는다.
func (model aiSessionsModel) openActivity() (tea.Model, tea.Cmd) {
	target, ok := model.selectedTask()
	if !ok {
		index := model.index()
		if index < 0 {
			return model, nil
		}
		session := model.sessions[index]
		if session.Tool != "claude" || session.Transcript == "" {
			return model, nil
		}
		target = aiTask{Kind: "session", ID: session.Session, Session: session.Session, Cwd: session.Cwd, Transcript: session.Transcript}
	}
	model.activityGen++
	activity := aiAgentsModel{feed: &aiAgentsFeed{claudeDir: model.claudeDir, reader: newAITaskReader()}, width: model.width, height: model.height,
		target: &target, generation: model.activityGen}
	model.activity = &activity
	return model, activity.readCmd(0)
}

func aiBell() tea.Msg {
	_, _ = os.Stdout.WriteString("\a")
	return nil
}

func aiSessionKey(session aiSession) string { return session.Tool + "/" + session.Session }

// index는 고른 줄의 세션이다. 하위 줄을 골랐으면 그 줄이 속한 세션이다.
func (model aiSessionsModel) index() int {
	for index, session := range model.sessions {
		if aiSessionKey(session) == model.selected {
			return index
		}
		for _, task := range session.Tasks {
			if aiChildKey(task) == model.selected {
				return index
			}
		}
	}
	return -1
}

// move는 세션 줄과 하위 줄을 화면 순서대로 오간다.
func (model *aiSessionsModel) move(step int) {
	keys := model.rowKeys()
	if len(keys) == 0 {
		return
	}
	index := slices.Index(keys, model.selected)
	model.selected = keys[min(max(index+step, 0), len(keys)-1)]
}

func (model aiSessionsModel) View() tea.View {
	if model.activity != nil {
		return model.activity.View()
	}
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
		footer := append([]string{richDim(strings.Repeat("─", width))}, model.detail(width)...)
		lines = append(lines, model.rows(aiListRows(model.height, model.rowCount(), len(footer)))...)
		lines = append(aiFillHeight(lines, model.height-1-len(footer)), footer...)
	}
	lines = aiFillHeight(lines, model.height-1)
	lines = append(lines, liveKeyHints("↑↓ select · enter activity · n needs only · a ended · q quit", true,
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

// aiListRows는 제목 줄, 키 안내 줄, 목록 아래 칸을 뺀 목록 높이다. 높이를 모르면 모두 보인다.
func aiListRows(height, count, footer int) int {
	if height <= 0 {
		return count
	}
	return max(1, height-2-footer)
}

// aiFillHeight는 lines가 height줄이 되도록 빈 줄을 붙인다. 목록이 짧아도 아래 칸과 키 안내가 화면 맨 아래에 온다.
func aiFillHeight(lines []string, height int) []string {
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

func (model aiSessionsModel) rowCount() int {
	count := len(model.sessions)
	for _, session := range model.sessions {
		count += len(session.Tasks)
	}
	return count
}

// rows는 세션 줄과 그 아래 agent·shell 줄을, 고른 세션이 보이도록 limit줄로 자른다. 고르는 것은 세션 줄뿐이다.
func (model aiSessionsModel) rows(limit int) []string {
	var lines []string
	selected := -1
	for _, session := range model.sessions {
		line := aiSessionFit(aiSessionHead(session, model.now), aiSessionDetail(session, model.now), session, model.lineWidth()-1, true)
		if aiSessionKey(session) == model.selected {
			selected = len(lines)
			lines = append(lines, liveSelected(liveSelectedBar+line, true))
		} else {
			lines = append(lines, " "+line)
		}
		for _, task := range session.Tasks {
			line := aiChildRow(task, model.now, model.lineWidth()-1, true)
			if aiChildKey(task) == model.selected {
				selected = len(lines)
				lines = append(lines, liveSelected(liveSelectedBar+line, true))
				continue
			}
			lines = append(lines, " "+line)
		}
	}
	first := 0
	if selected >= limit {
		first = selected - limit + 1
	}
	return lines[first:min(len(lines), first+limit)]
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
	if session.State == aiStateWorking && session.Reason != "background" && (info.TurnTools > 0 || info.TurnFailed > 0) {
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

// detail은 목록에서 잘린 설명과 세션을 찾는 데 필요한 값을 보인다. 이름표는 회색으로 두고, 값은 종류마다 색을 달리해 훑어보기 쉽게 한다.
func (model aiSessionsModel) detail(width int) []string {
	if task, ok := model.selectedTask(); ok {
		return aiTaskInfoLines(task)
	}
	index := model.index()
	if index < 0 {
		return nil
	}
	session := model.sessions[index]
	where := richDim("-")
	if session.TTY != "" {
		where = session.TTY
	}
	if session.PID > 0 {
		where += richDim(" pid ") + strconv.Itoa(session.PID)
	}
	label := func(name string) string { return richDim(padRight(name, 9)) }
	lines := []string{
		label("session") + richPaint(session.Tool, richCyan, true) + " " + session.Session,
		label("dir") + richPaint(session.Cwd, richBlue, false),
		label("terminal") + where,
		label("state") + aiPaintSessionState(session.State, aiSessionStateText(session)) + richDim(" since "+session.Since.Local().Format("15:04:05")),
	}
	if session.Task != "" {
		lines = append(lines, label("task")+aiPaintActivity(session.Task))
	}
	_, percent := aiSessionContext(session)
	for _, line := range aiSessionInfoLines(session, model.now) {
		lines = append(lines, label(line[0])+aiPaintInfo(line[0], line[1], percent))
	}
	if detail := aiSessionDetail(session, model.now); detail != "" && detail != session.Task {
		wrapped := strings.Split(ansi.Wrap(aiPaintActivity(detail), max(20, width-9), ""), "\n")
		for index, part := range wrapped[:min(len(wrapped), aiSessionsDetailLines-len(lines))] {
			name := ""
			if index == 0 {
				name = "detail"
			}
			lines = append(lines, label(name)+part)
		}
	}
	return lines
}

// aiPaintSessionState는 상태 값의 색이다. 사용자를 기다리면 굵은 빨강, 실패는 빨강, 작업 중은 굵은 초록, 그 밖은 회색이다.
func aiPaintSessionState(state aiSessionState, text string) string {
	switch state {
	case aiStateNeeds:
		return richPaint(text, richRed, true)
	case aiStateFailed:
		return richPaint(text, richRed, false)
	case aiStateWorking:
		return richPaint(text, richGreen, true)
	}
	return richDim(text)
}

var (
	// aiActivityTool은 활동 설명의 "Bash: 명령" 같은 도구 이름이다. 처음 나온 것만 칠한다.
	aiActivityTool = regexp.MustCompile(`\b(mcp__[\w-]+|[A-Z][A-Za-z]+):`)
	// aiActivityLead는 설명 앞의 실패 수와 실행 시간이다.
	aiActivityLead = regexp.MustCompile(`^(✗\d+ )?(\d+h\d+m|\d+m\d+s|\d+s)?`)
	// aiInfoWord는 token 줄의 단위 단어다. 숫자가 눈에 먼저 들어오게 회색으로 둔다.
	aiInfoWord = regexp.MustCompile(`\b(in|out|cache|last)\b|/min|[()]`)
)

// aiPaintActivity는 활동 설명에서 실패 수를 빨강, 실행 시간을 노랑, 도구 이름을 굵은 청록으로 칠한다.
func aiPaintActivity(text string) string {
	lead := aiActivityLead.FindStringSubmatchIndex(text)
	var builder strings.Builder
	rest := text
	if lead != nil && lead[1] > 0 {
		if lead[2] >= 0 {
			builder.WriteString(richPaint(text[lead[2]:lead[3]], richRed, true))
		}
		if lead[4] >= 0 {
			builder.WriteString(richPaint(text[lead[4]:lead[5]], richYellow, false))
		}
		rest = text[lead[1]:]
	}
	if match := aiActivityTool.FindStringSubmatchIndex(rest); match != nil {
		builder.WriteString(rest[:match[2]] + richPaint(rest[match[2]:match[3]], richCyan, true) + rest[match[3]:])
		return builder.String()
	}
	builder.WriteString(rest)
	return builder.String()
}

// aiPaintInfo는 상세 칸 정보 줄의 값을 칠한다. 모델 이름은 자홍, 컨텍스트는 남은 창에 따라, 실패와 오류는 빨강이다.
func aiPaintInfo(name, text string, percent int) string {
	switch name {
	case "model":
		model, rest, _ := strings.Cut(text, " · ")
		if rest != "" {
			rest = richDim(" · ") + strings.ReplaceAll(rest, " · ", richDim(" · "))
		}
		return richPaint(model, richMagenta, true) + rest
	case "context":
		switch {
		case percent >= 90:
			return richPaint(text, richRed, true)
		case percent >= 80:
			return richPaint(text, richYellow, false)
		}
		return text
	case "turn":
		if index := strings.LastIndex(text, " · "); index >= 0 && strings.HasSuffix(text, " failed") {
			return text[:index] + richDim(" · ") + richPaint(text[index+len(" · "):], richRed, true)
		}
		return text
	case "tokens":
		return aiInfoWord.ReplaceAllStringFunc(text, richDim)
	case "error":
		return richPaint(text, richRed, false)
	}
	return text
}
