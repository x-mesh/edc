package edc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	// aiAgentsLogLimit는 활동 화면이 들고 있는 줄 수다. 오래 도는 작업도 메모리가 늘지 않는다.
	aiAgentsLogLimit = 5000
)

func runAIAgentsDashboard(claudeDir string) int {
	model := aiAgentsModel{feed: &aiAgentsFeed{claudeDir: claudeDir, reader: newAITaskReader()}}
	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// aiAgentsFeed는 목록과 활동을 읽는 상태다. 다른 작업을 열면 새 읽기를 곧바로 걸므로, 앞선 읽기가 아직 돌고 있을 수 있다.
// mu가 둘을 차례로 돌린다. reader는 map이라 동시에 고치면 Go runtime이 프로세스를 끝낸다.
type aiAgentsFeed struct {
	mu        sync.Mutex
	claudeDir string
	reader    *aiTaskReader
	log       *aiTaskLog
	logKey    string
	// logGeneration은 log를 만든 화면의 세대다. 같은 작업을 닫았다 다시 열면 세대가 바뀌므로 처음부터 다시 읽는다.
	logGeneration int
}

type aiAgentsReadMsg struct {
	tasks []aiTask
	// key는 steps를 읽은 작업이다. 읽는 사이에 다른 작업을 열면 버린다.
	key string
	// generation은 읽기를 건 화면의 세대다. 지난 세대의 결과는 버리고 다음 읽기를 걸지 않아 읽기가 하나만 이어진다.
	generation int
	reset      bool
	steps      []aiAgentStep
	err        error
	at         time.Time
}

func aiTaskKey(task aiTask) string { return task.Kind + "/" + task.ID }

// read는 목록을 읽고, target이 있으면 그 작업의 새 활동도 읽는다. target은 목록에서 빠져도 계속 읽는다.
func (feed *aiAgentsFeed) read(at time.Time, target *aiTask, generation int) aiAgentsReadMsg {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	msg := aiAgentsReadMsg{at: at, generation: generation}
	msg.tasks, msg.err = feed.reader.list(feed.claudeDir, at)
	if target == nil || msg.err != nil {
		return msg
	}
	msg.key = aiTaskKey(*target)
	if msg.key != feed.logKey || generation != feed.logGeneration {
		feed.log, feed.logKey, feed.logGeneration, msg.reset = newAITaskLog(*target), msg.key, generation, true
	}
	msg.steps, msg.err = feed.log.read(feed.reader, feed.claudeDir, at)
	return msg
}

type aiAgentsModel struct {
	feed          *aiAgentsFeed
	tasks         []aiTask
	now           time.Time
	loaded        bool
	err           error
	width, height int
	// selected는 고른 작업의 키다. 순서가 바뀌어도 같은 작업을 가리킨다.
	selected string
	// target은 활동 화면에서 보는 작업이다. 끝나 목록에서 빠져도 화면을 유지하려고 연 순간의 값을 붙잡아 둔다.
	// scroll은 끝에서 올라간 줄 수이고 0이면 새 줄을 따라간다.
	target     *aiTask
	steps      []aiAgentStep
	scroll     int
	generation int
}

func (model aiAgentsModel) Init() tea.Cmd { return model.readCmd(0) }

func (model aiAgentsModel) readCmd(delay time.Duration) tea.Cmd {
	feed, target, generation := model.feed, model.target, model.generation
	if delay == 0 {
		return func() tea.Msg { return feed.read(time.Now(), target, generation) }
	}
	return tea.Tick(delay, func(at time.Time) tea.Msg { return feed.read(at, target, generation) })
}

// restart는 바뀐 보기로 곧바로 읽고, 이미 걸린 읽기의 결과는 버리게 한다.
func (model *aiAgentsModel) restart() tea.Cmd {
	model.generation++
	return model.readCmd(0)
}

// clampScroll은 scroll을 보이는 범위로 묶는다. 기록을 다시 읽거나 앞을 잘라 steps가 줄면 끝에서 올라간 줄 수가 steps보다 커질 수 있다.
func (model *aiAgentsModel) clampScroll() {
	model.scroll = min(max(model.scroll, 0), max(0, len(model.steps)-model.logRows()))
}

func (model aiAgentsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case aiAgentsReadMsg:
		if value.generation != model.generation {
			return model, nil
		}
		model.now, model.err, model.loaded = value.at, value.err, true
		if value.err == nil {
			model.tasks = value.tasks
		}
		if model.index() < 0 && len(model.tasks) > 0 && model.target == nil {
			model.selected = aiTaskKey(model.tasks[0])
		}
		if model.target != nil && value.key == aiTaskKey(*model.target) {
			if value.reset {
				model.steps = nil
			}
			model.steps = append(model.steps, value.steps...)
			if extra := len(model.steps) - aiAgentsLogLimit; extra > 0 {
				model.steps = model.steps[extra:]
			}
			if model.scroll > 0 {
				model.scroll += len(value.steps)
			}
			model.clampScroll()
		}
		return model, model.readCmd(aiAgentFollowInterval)
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
	case tea.KeyPressMsg:
		if model.target != nil {
			return model.logKey(value.String())
		}
		switch value.String() {
		case "q", "esc", "ctrl+c":
			return model, tea.Quit
		case "up", "k":
			model.move(-1)
		case "down", "j":
			model.move(1)
		case "enter", "right", "l":
			if index := model.index(); index >= 0 {
				target := model.tasks[index]
				model.target, model.steps, model.scroll = &target, nil, 0
				return model, model.restart()
			}
		}
	}
	return model, nil
}

func (model aiAgentsModel) logKey(key string) (tea.Model, tea.Cmd) {
	page := max(1, model.logRows()-1)
	switch key {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "esc", "left", "h", "backspace":
		model.target, model.steps, model.scroll = nil, nil, 0
		return model, model.restart()
	case "up", "k":
		model.scroll++
	case "down", "j":
		model.scroll--
	case "pgup":
		model.scroll += page
	case "pgdown":
		model.scroll -= page
	case "end", "G":
		model.scroll = 0
	case "home", "g":
		model.scroll = len(model.steps)
	}
	model.clampScroll()
	return model, nil
}

func (model aiAgentsModel) index() int {
	for index, task := range model.tasks {
		if aiTaskKey(task) == model.selected {
			return index
		}
	}
	return -1
}

func (model *aiAgentsModel) move(step int) {
	if len(model.tasks) == 0 {
		return
	}
	index := min(max(model.index()+step, 0), len(model.tasks)-1)
	model.selected = aiTaskKey(model.tasks[index])
}

func (model aiAgentsModel) lineWidth() int {
	if model.width <= 0 {
		return 100
	}
	return model.width
}

func (model aiAgentsModel) View() tea.View {
	width := model.lineWidth()
	lines := []string{model.title(width)}
	switch {
	case model.err != nil:
		lines = append(lines, richPaint(model.err.Error(), richRed, false))
	case !model.loaded:
		lines = append(lines, richDim("reading "+model.feed.claudeDir))
	case model.target != nil:
		lines = append(lines, model.logLines(width)...)
	case len(model.tasks) == 0:
		lines = append(lines, richDim(T("observe.ai.agents.none")))
	default:
		footer := append([]string{richDim(strings.Repeat("─", width))}, model.detail()...)
		lines = append(lines, model.rows(aiListRows(model.height, len(model.tasks), len(footer)))...)
		lines = append(aiFillHeight(lines, model.height-1-len(footer)), footer...)
	}
	lines = aiFillHeight(lines, model.height-1)
	if model.target != nil {
		hints := "↑↓ scroll · PgUp/PgDn page · End follow · esc back · q quit"
		lines = append(lines, liveKeyHints(hints, true, liveHintItem{key: "End", on: model.scroll == 0}))
	} else {
		lines = append(lines, liveKeyHints("↑↓ select · enter activity · q quit", true))
	}
	for index := range lines {
		lines[index] = ansi.Truncate(lines[index], width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model aiAgentsModel) title(width int) string {
	left := richPaint("🐰 edc ai agents", richMagenta, true) + richDim(" · this host")
	var right string
	if model.target != nil {
		left += richDim(" · ") + aiTaskTitle(*model.target)
		right = "live"
	} else {
		counts := map[string]int{}
		for _, task := range model.tasks {
			counts[task.Kind]++
		}
		var parts []string
		for _, kind := range []string{"session", "agent", "shell"} {
			if counts[kind] > 0 {
				parts = append(parts, aiTaskMarks[kind]+" "+strconv.Itoa(counts[kind]))
			}
		}
		right = strings.Join(append(parts, "live"), " · ")
	}
	right = richDim(right + " · " + model.now.Local().Format("15:04:05 MST"))
	room := width - ansi.StringWidth(right) - 2
	left = ansi.Truncate(left, max(0, room), "…")
	space := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	return left + strings.Repeat(" ", max(2, space)) + right
}

// rows는 고른 작업이 보이도록 목록을 limit줄로 자른다.
func (model aiAgentsModel) rows(limit int) []string {
	first := 0
	if index := model.index(); index >= limit {
		first = index - limit + 1
	}
	var lines []string
	for index := first; index < min(len(model.tasks), first+limit); index++ {
		task := model.tasks[index]
		head := aiTaskHead(task, model.now)
		room := max(0, model.lineWidth()-2-liveWidth(head)-1)
		line := head + " " + padRight(ansi.Truncate(aiTaskDetail(task), room, "…"), room)
		switch {
		case task.Kind == "session" && task.State == aiTaskRunning:
			line = richPaint(line, richMagenta, false)
		case task.State != aiTaskRunning:
			line = richDim(line)
		}
		if aiTaskKey(task) == model.selected {
			lines = append(lines, liveSelected(liveSelectedBar+line, true))
			continue
		}
		lines = append(lines, " "+line)
	}
	return lines
}

// detail은 목록에서 잘린 설명과 작업을 찾는 데 필요한 값이다. 이름표는 회색, 값은 edc ai sessions와 같은 색 규칙을 따른다.
func (model aiAgentsModel) detail() []string {
	index := model.index()
	if index < 0 {
		return nil
	}
	task := model.tasks[index]
	label := func(name string) string { return richDim(padRight(name, 9)) }
	kind := richPaint(task.Kind, richCyan, true)
	if task.Type != "" {
		kind += " " + task.Type
	}
	if task.Model != "" {
		kind += " " + richPaint(task.Model, richMagenta, true)
	}
	state := richDim(aiTaskStateText(task.State))
	if task.State == aiTaskRunning {
		state = richPaint(aiTaskStateText(task.State), richGreen, true)
	}
	lines := []string{
		label("id") + task.ID + richDim(" session "+task.Session),
		label("dir") + richPaint(task.Cwd, richBlue, false),
		label("kind") + kind + richDim(" · ") + state,
		label("started") + task.Started.Local().Format("01-02 15:04:05") + richDim(" last ") + task.Last.Local().Format("15:04:05"),
	}
	if task.Description != "" {
		lines = append(lines, label("task")+task.Description)
	}
	if task.Step != nil {
		lines = append(lines, label("last")+aiPaintStep(*task.Step))
	}
	return lines
}

// aiPaintStep은 활동 한 줄의 색이다. 도구 이름은 굵은 청록, 실패는 빨강, 지시는 굵은 자홍, shell 출력의 기호와 멈춤 줄은 회색이다.
func aiPaintStep(step aiAgentStep) string {
	switch step.Kind {
	case "tool":
		return strings.TrimRight(richPaint(step.Tool, richCyan, true)+" "+step.Text, " ")
	case "failed":
		return richPaint(aiAgentStepText(step), richRed, false)
	case "prompt":
		return richPaint(aiAgentStepText(step), richMagenta, true)
	case "output":
		return richDim("│ ") + step.Text
	case "stopped":
		return richDim(aiAgentStepText(step))
	}
	return aiAgentStepText(step)
}

func (model aiAgentsModel) logRows() int {
	if model.height <= 0 {
		return 20
	}
	return max(1, model.height-2)
}

// logLines는 활동의 끝부분이다. scroll만큼 위로 올라간 곳을 보인다.
func (model aiAgentsModel) logLines(width int) []string {
	if len(model.steps) == 0 {
		return []string{richDim(T("observe.ai.agents.no_activity"))}
	}
	rows := model.logRows()
	end := len(model.steps) - model.scroll
	var lines []string
	for _, step := range model.steps[max(0, end-rows):end] {
		at := richDim(step.At.Local().Format("15:04:05") + "  ")
		text := ansi.Truncate(aiPaintStep(step), max(0, width-10), "…")
		lines = append(lines, at+text)
	}
	return lines
}
