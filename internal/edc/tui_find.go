package edc

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func init() { findPicker = runFindPicker }

const (
	// findPickerTop는 화면이 순서대로 들고 있는 결과 수다. 전부 정렬하면 짧은 질의에서 수십만 개를 정렬하느라 입력이 밀린다.
	findPickerTop = 2000
	// findFeedInterval은 순회 결과를 화면으로 보내는 간격이다. 디렉터리마다 보내면 / 전체에서 수만 번 갱신한다.
	findFeedInterval = 30 * time.Millisecond
	findFeedBatch    = 8192
)

type findBatchMsg struct{ entries []findEntry }

type findWalkDoneMsg struct{ report findWalkReport }

// findSearchMsg는 질의 하나의 계산 결과다. covered는 계산한 후보 수이고, 그 뒤에 들어온 후보는 받은 쪽이 이어서 계산한다.
type findSearchMsg struct {
	generation int
	covered    int
	all        []int32
	top        []findHit
}

type findExecDoneMsg struct {
	err  error
	what string
}

// findPickerConfig는 화면이 쓰는 바깥 값이다. 시험이 바꿔 끼운다.
type findPickerConfig struct {
	now            func() time.Time
	self           string
	stdoutTerminal bool
	color          bool
	editor         []string
	stdinMode      bool
	stat           bool
}

type findModel struct {
	options findOptions
	config  findPickerConfig
	width   int
	height  int

	paths  []string
	kinds  []findKind
	mtimes []int64
	sizes  []int64

	// limit는 후보 수의 상한이다. 시험이 낮춘다.
	limit   int
	limited bool
	walking bool
	report  findWalkReport

	query      string
	matcher    findMatcher
	problem    string
	generation int
	// pending은 아직 결과가 오지 않은 계산의 세대다. 0이면 계산 중이 아니다.
	pending int
	// resultQuery는 지금 결과가 나타내는 질의다. all이 nil이면 모든 후보가 맞는다.
	resultQuery string
	all         []int32
	total       int
	top         []findHit
	covered     int

	cursor, offset int
	status         string

	grepMode    bool
	grepPattern string
	// grepPath는 ctrl+g를 누를 때 고른 경로다. 패턴을 치는 동안 계산이 끝나 선택이 바뀌어도 대상은 그대로다.
	grepPath string

	chosen    string
	cancelled bool
	// accept는 계산 중에 Enter를 누른 것이다. 결과가 오면 맨 위를 고른다. 바로 고르면 친 글자가 반영되기 전의 줄을 고른다.
	accept bool

	previewOn         bool
	preview           findPreview
	previewPath       string
	previewGeneration int
}

func newFindModel(options findOptions, config findPickerConfig) findModel {
	matcher, _, _ := newFindMatcher("", options.regex)
	return findModel{options: options, config: config, matcher: matcher, walking: true, width: 80, height: 24, limit: findCandidateLimit}
}

// runFindPicker는 /dev/tty에 화면을 그리고 고른 경로만 stdout에 쓴다. /dev/tty를 열지 못하면 shown이 false다.
func runFindPicker(options findOptions, env findEnv) (int, bool) {
	input, output, err := tea.OpenTTY()
	if err != nil {
		return 0, false
	}
	defer input.Close()
	defer output.Close()
	self, _ := os.Executable()
	stdinMode := !options.explicit && env.stdinPipe
	config := findPickerConfig{
		now: env.now, self: self, stdoutTerminal: env.stdoutTerminal, color: os.Getenv("NO_COLOR") == "",
		editor: findEditor(), stdinMode: stdinMode, stat: !stdinMode,
	}
	// 시간 제한은 순회에만 건다. 화면에 걸면 고르는 중이나 편집기를 연 동안에도 화면이 끝난다.
	ctx, cancel := context.WithTimeout(context.Background(), options.common.timeout)
	defer cancel()
	filter := newFindFilter(options, env.now())
	program := tea.NewProgram(newFindModel(options, config), tea.WithInput(input), tea.WithOutput(output))
	feed := make(chan []findEntry, 64)
	done := make(chan findWalkReport, 1)
	// quit은 화면이 끝났다는 표시다. 순회는 시간 제한(ctx)이나 이 표시로 멈춘다.
	quit := make(chan struct{})
	defer close(quit)
	go findFeedProgram(program, feed, done)
	go func() {
		var report findWalkReport
		send := func(batch []findEntry) bool {
			kept := filter.apply(batch)
			if len(kept) == 0 {
				return true
			}
			select {
			case feed <- kept:
				return true
			case <-quit:
				return false
			}
		}
		if stdinMode {
			report = findReadStdin(env.stdin, send)
		} else {
			report = findWalk(ctx, findWalkOptions{
				roots: options.paths, noIgnore: options.noIgnore, crossMounts: options.crossMounts, layers: options.layers,
				workers: options.workers, stat: true, stall: findStall,
			}, env.sources, send)
		}
		// 끝은 findFeedProgram이 남은 묶음을 보낸 뒤에 알린다. 따로 보내면 마지막 묶음보다 먼저 도착할 수 있다.
		done <- report
		close(feed)
	}()
	final, err := program.Run()
	cancel()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		fmt.Fprintln(env.stderr, err)
		return 2, true
	}
	model, ok := final.(findModel)
	if !ok || model.cancelled || model.chosen == "" {
		return 130, true
	}
	end := "\n"
	if options.null {
		end = "\x00"
	}
	fmt.Fprint(env.stdout, model.chosen+end)
	return 0, true
}

// findFeedProgram은 순회 결과를 모아 findFeedInterval마다 한 번 화면으로 보낸다. feed가 닫히면 남은 것을 보내고 끝을 알린다.
func findFeedProgram(program *tea.Program, feed <-chan []findEntry, done <-chan findWalkReport) {
	ticker := time.NewTicker(findFeedInterval)
	defer ticker.Stop()
	var buffer []findEntry
	flush := func() {
		if len(buffer) > 0 {
			program.Send(findBatchMsg{entries: buffer})
			buffer = nil
		}
	}
	for {
		select {
		case batch, ok := <-feed:
			if !ok {
				flush()
				program.Send(findWalkDoneMsg{report: <-done})
				return
			}
			buffer = append(buffer, batch...)
			if len(buffer) >= findFeedBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// findEditor는 ctrl+o로 여는 편집기다. VISUAL, EDITOR 순서이고 둘 다 없으면 vi다. 값에 인자가 들어 있을 수 있다(code -w).
func findEditor() []string {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if fields := strings.Fields(os.Getenv(name)); len(fields) > 0 {
			return fields
		}
	}
	return []string{"vi"}
}

func (model findModel) Init() tea.Cmd { return nil }

// Update는 메시지를 처리한 뒤, 미리보기가 켜져 있고 고른 경로가 바뀌었으면 읽기를 더한다.
func (model findModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := model.update(msg)
	updated := next.(findModel)
	if updated.chosen != "" || updated.cancelled {
		return updated, cmd
	}
	if preview := updated.previewCmd(); preview != nil {
		return updated, tea.Batch(cmd, preview)
	}
	return updated, cmd
}

func (model findModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case findPreviewMsg:
		if value.generation == model.previewGeneration {
			model.preview = value.preview
		}
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.clamp()
	case findBatchMsg:
		model.add(value.entries)
	case findWalkDoneMsg:
		model.walking, model.report = false, value.report
	case findSearchMsg:
		if value.generation != model.pending {
			return model, nil
		}
		model.pending = 0
		model.resultQuery, model.all, model.top, model.covered = model.query, value.all, value.top, value.covered
		model.total = len(value.all)
		if value.all == nil {
			// 빈 질의는 모든 후보가 맞는다.
			model.total = value.covered
		}
		model.catchUp()
		model.clamp()
		if model.accept {
			model.accept = false
			if hit, ok := model.selected(); ok {
				model.chosen = model.paths[hit]
				return model, tea.Quit
			}
		}
	case findExecDoneMsg:
		model.status = ""
		if value.err != nil {
			model.status = T("find.picker.exec_failed", value.what, value.err.Error())
		}
	case tea.PasteMsg:
		text := strings.NewReplacer("\r", "", "\n", " ", "\t", " ").Replace(value.Content)
		if model.grepMode {
			model.grepPattern += text
			return model, nil
		}
		return model.setQuery(model.query + text)
	case tea.KeyPressMsg:
		return model.key(value)
	}
	return model, nil
}

func (model findModel) key(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	name := key.String()
	if model.grepMode {
		return model.grepKey(key, name)
	}
	switch name {
	case "ctrl+c", "esc":
		model.cancelled = true
		return model, tea.Quit
	case "enter":
		if model.pending != 0 {
			model.accept = true
			return model, nil
		}
		if hit, ok := model.selected(); ok {
			model.chosen = model.paths[hit]
			return model, tea.Quit
		}
		return model, nil
	case "up", "ctrl+p", "ctrl+k":
		model.cursor--
	case "down", "ctrl+n", "ctrl+j":
		model.cursor++
	case "pgup":
		model.cursor -= model.rows()
	case "pgdown":
		model.cursor += model.rows()
	case "home":
		model.cursor = 0
	case "end":
		model.cursor = len(model.top) - 1
	case "backspace", "ctrl+h":
		if model.query == "" {
			return model, nil
		}
		runes := []rune(model.query)
		return model.setQuery(string(runes[:len(runes)-1]))
	case "ctrl+u":
		return model.setQuery("")
	case "tab":
		model.previewOn = !model.previewOn
		model.previewPath, model.preview = "", findPreview{}
	case "ctrl+w":
		trimmed := strings.TrimRight(model.query, " ")
		cut := strings.LastIndexByte(trimmed, ' ') + 1
		return model.setQuery(trimmed[:cut])
	case "ctrl+o":
		return model.open(model.editCommand, "editor")
	case "ctrl+v":
		return model.open(model.viewCommand, "view")
	case "ctrl+g":
		if !model.config.stdoutTerminal {
			model.status = T("find.picker.open_pipe")
			return model, nil
		}
		if hit, ok := model.selected(); ok {
			model.grepMode, model.grepPattern, model.grepPath, model.status = true, "", model.paths[hit], ""
		}
		return model, nil
	default:
		if key.Text != "" && !strings.ContainsAny(key.Text, "\x00\x1b") {
			return model.setQuery(model.query + key.Text)
		}
		return model, nil
	}
	model.clamp()
	return model, nil
}

func (model findModel) grepKey(key tea.KeyPressMsg, name string) (tea.Model, tea.Cmd) {
	switch name {
	case "ctrl+c":
		model.cancelled = true
		return model, tea.Quit
	case "esc":
		model.grepMode = false
	case "backspace", "ctrl+h":
		if runes := []rune(model.grepPattern); len(runes) > 0 {
			model.grepPattern = string(runes[:len(runes)-1])
		}
	case "ctrl+u":
		model.grepPattern = ""
	case "enter":
		if model.grepPattern == "" {
			return model, nil
		}
		model.grepMode = false
		if !model.config.stdoutTerminal {
			model.status = T("find.picker.open_pipe")
			return model, nil
		}
		command, _ := model.grepCommand(model.grepPath, findKindOther)
		return model, tea.ExecProcess(command, func(err error) tea.Msg { return findExecDoneMsg{err: err, what: "grep"} })
	default:
		if key.Text != "" && !strings.ContainsAny(key.Text, "\x00\x1b") {
			model.grepPattern += key.Text
		}
	}
	return model, nil
}

// open은 고른 경로로 다른 프로그램을 실행하고 화면으로 돌아온다. stdout이 터미널이 아니면 $(edc find) 안이므로 열지 않는다.
func (model findModel) open(build func(path string, kind findKind) (*exec.Cmd, string), what string) (tea.Model, tea.Cmd) {
	if !model.config.stdoutTerminal {
		model.status = T("find.picker.open_pipe")
		return model, nil
	}
	hit, ok := model.selected()
	if !ok {
		return model, nil
	}
	command, problem := build(model.paths[hit], model.kinds[hit])
	if problem != "" {
		model.status = problem
		return model, nil
	}
	return model, tea.ExecProcess(command, func(err error) tea.Msg { return findExecDoneMsg{err: err, what: what} })
}

func (model findModel) editCommand(path string, _ findKind) (*exec.Cmd, string) {
	editor := model.config.editor
	return exec.Command(editor[0], append(slices.Clone(editor[1:]), path)...), ""
}

func (model findModel) viewCommand(path string, kind findKind) (*exec.Cmd, string) {
	if kind == findKindDir {
		return nil, T("find.picker.view_dir")
	}
	return exec.Command(model.config.self, "view", path), ""
}

// grepCommand는 edc grep의 결과를 edc view로 읽는다. 패턴과 경로는 셸이 해석하지 않도록 위치 인자로 넘긴다.
func (model findModel) grepCommand(path string, _ findKind) (*exec.Cmd, string) {
	script := `"$0" grep -n -C 2 -- "$1" "$2" | "$0" view`
	return exec.Command("/bin/sh", "-c", script, model.config.self, model.grepPattern, path), ""
}

func (model findModel) selected() (int, bool) {
	if model.cursor < 0 || model.cursor >= len(model.top) {
		return 0, false
	}
	return int(model.top[model.cursor].index), true
}

func (model findModel) rows() int {
	rows := model.height - 3
	if side, size := model.previewLayout(); !side && size > 0 {
		// 아래 미리보기는 구분선 한 줄과 내용 size줄을 쓴다.
		rows -= size + 1
	}
	return max(1, rows)
}

func (model *findModel) clamp() {
	model.cursor = max(0, min(model.cursor, len(model.top)-1))
	rows := model.rows()
	if model.cursor < model.offset {
		model.offset = model.cursor
	}
	if model.cursor >= model.offset+rows {
		model.offset = model.cursor - rows + 1
	}
	model.offset = max(0, min(model.offset, max(0, len(model.top)-rows)))
}

// add는 순회에서 들어온 후보를 붙인다. 계산 중이 아니면 새 후보만 바로 계산한다.
func (model *findModel) add(entries []findEntry) {
	room := model.limit - len(model.paths)
	if room <= 0 {
		model.limited = true
		return
	}
	if len(entries) > room {
		entries, model.limited = entries[:room], true
	}
	for _, entry := range entries {
		model.paths = append(model.paths, entry.path)
		model.kinds = append(model.kinds, entry.kind)
		model.mtimes = append(model.mtimes, entry.mtime)
		model.sizes = append(model.sizes, entry.size)
	}
	if model.pending == 0 {
		model.catchUp()
		model.clamp()
	}
}

// setQuery는 질의를 바꾸고 계산을 시작한다. 앞 질의를 늘린 것이면 앞 결과 안에서만 찾는다.
func (model findModel) setQuery(query string) (tea.Model, tea.Cmd) {
	matcher, truncated, err := newFindMatcher(query, model.options.regex)
	if err != nil {
		// 쓰다 만 정규식은 흔하다. 앞 결과를 그대로 두고 상태 줄에만 보인다.
		model.query, model.problem = query, T("find.error.regex", err.Error())
		return model, nil
	}
	model.query, model.matcher, model.problem = query, matcher, ""
	if truncated {
		model.problem = T("find.note.query_cut", findQueryLimit)
	}
	model.cursor, model.offset = 0, 0
	model.generation++
	model.pending = model.generation
	var candidates []int32
	if !model.options.regex && model.resultQuery != "" && model.all != nil && strings.HasPrefix(query, model.resultQuery) && model.covered == len(model.paths) {
		candidates = model.all
	}
	generation, paths, mtimes, covered := model.generation, model.paths, model.mtimes, len(model.paths)
	stdinMode := model.config.stdinMode
	return model, func() tea.Msg {
		return findSearch(matcher, paths[:covered], mtimes[:covered], candidates, generation, stdinMode)
	}
}

// findSearch는 질의 하나를 계산한다. 빈 질의는 모든 후보가 맞고, 최근에 바뀐 순서다(stdin 후보는 들어온 순서).
func findSearch(matcher findMatcher, paths []string, mtimes []int64, candidates []int32, generation int, stdinMode bool) findSearchMsg {
	msg := findSearchMsg{generation: generation, covered: len(paths)}
	if !matcher.ranked() && !findIsRegex(matcher) {
		msg.top = findEmptyTop(len(paths), 0, nil, mtimes, stdinMode)
		return msg
	}
	hits := findScore(matcher, paths, candidates, 0)
	msg.all = make([]int32, len(hits))
	for index, hit := range hits {
		msg.all[index] = hit.index
	}
	msg.top = findTopHits(hits, findPickerTop, findHitLess(matcher.ranked(), paths))
	return msg
}

func findIsRegex(matcher findMatcher) bool {
	_, ok := matcher.(findRegexMatcher)
	return ok
}

// catchUp은 지금 결과가 계산하지 않은 후보(그 뒤에 들어온 것)를 계산해 결과에 합친다.
func (model *findModel) catchUp() {
	from, to := model.covered, len(model.paths)
	if from >= to {
		return
	}
	model.covered = to
	if !model.matcher.ranked() && !findIsRegex(model.matcher) {
		model.top = findEmptyTop(to, from, model.top, model.mtimes, model.config.stdinMode)
		model.total = to
		return
	}
	var fresh []findHit
	for index := from; index < to; index++ {
		path := model.paths[index]
		if score, ok := model.matcher.score(path, findBase(path)); ok {
			fresh = append(fresh, findHit{index: int32(index), score: int32(score)})
			model.all = append(model.all, int32(index))
		}
	}
	if model.all == nil {
		// 아무것도 맞지 않았어도 계산은 했으므로, 모든 후보가 맞는다는 뜻의 nil과 구별한다.
		model.all = []int32{}
	}
	model.total = len(model.all)
	if len(fresh) == 0 {
		return
	}
	merged := append(slices.Clone(model.top), fresh...)
	model.top = findTopHits(merged, findPickerTop, findHitLess(model.matcher.ranked(), model.paths))
}

// findEmptyTop은 빈 질의의 순서다. 순회 후보는 최근에 바뀐 순이고, stdin 후보는 들어온 순서다.
func findEmptyTop(total, from int, current []findHit, mtimes []int64, stdinMode bool) []findHit {
	if stdinMode {
		top := slices.Clone(current)
		for index := from; index < total && len(top) < findPickerTop; index++ {
			top = append(top, findHit{index: int32(index)})
		}
		return top
	}
	merged := slices.Clone(current)
	for index := from; index < total; index++ {
		merged = append(merged, findHit{index: int32(index)})
	}
	return findTopHits(merged, findPickerTop, func(a, b findHit) bool {
		if mtimes[a.index] != mtimes[b.index] {
			return mtimes[a.index] > mtimes[b.index]
		}
		return a.index < b.index
	})
}

func findHitLess(ranked bool, paths []string) func(a, b findHit) bool {
	return func(a, b findHit) bool {
		if ranked {
			if a.score != b.score {
				return a.score > b.score
			}
			if len(paths[a.index]) != len(paths[b.index]) {
				return len(paths[a.index]) < len(paths[b.index])
			}
		}
		return paths[a.index] < paths[b.index]
	}
}

// findHitHeap은 위로 올릴 k개를 고르는 heap이다. 맨 위가 지금까지 고른 것 중 가장 나쁜 것이다.
type findHitHeap struct {
	hits []findHit
	less func(a, b findHit) bool
}

func (h *findHitHeap) Len() int           { return len(h.hits) }
func (h *findHitHeap) Less(i, j int) bool { return h.less(h.hits[j], h.hits[i]) }
func (h *findHitHeap) Swap(i, j int)      { h.hits[i], h.hits[j] = h.hits[j], h.hits[i] }
func (h *findHitHeap) Push(x any)         { h.hits = append(h.hits, x.(findHit)) }
func (h *findHitHeap) Pop() any {
	last := h.hits[len(h.hits)-1]
	h.hits = h.hits[:len(h.hits)-1]
	return last
}

// findTopHits는 hits에서 가장 좋은 k개를 좋은 순서로 돌려준다. 전부 정렬하지 않고 크기 k의 heap으로 고른다.
func findTopHits(hits []findHit, k int, less func(a, b findHit) bool) []findHit {
	if len(hits) <= k {
		top := slices.Clone(hits)
		slices.SortFunc(top, func(a, b findHit) int { return findCompare(less, a, b) })
		return top
	}
	selected := &findHitHeap{hits: make([]findHit, 0, k), less: less}
	for _, hit := range hits {
		if selected.Len() < k {
			heap.Push(selected, hit)
		} else if less(hit, selected.hits[0]) {
			selected.hits[0] = hit
			heap.Fix(selected, 0)
		}
	}
	top := selected.hits
	slices.SortFunc(top, func(a, b findHit) int { return findCompare(less, a, b) })
	return top
}

func findCompare(less func(a, b findHit) bool, a, b findHit) int {
	switch {
	case less(a, b):
		return -1
	case less(b, a):
		return 1
	}
	return 0
}

func (model findModel) View() tea.View {
	lines := []string{model.promptLine()}
	rows := model.rows()
	side, size := model.previewLayout()
	listWidth := model.width
	var preview []string
	if side {
		listWidth = model.width - size - 1
		preview = model.previewLines(size, rows)
	}
	for line := 0; line < rows; line++ {
		index := model.offset + line
		text := ""
		if index < len(model.top) {
			text = model.row(int(model.top[index].index), index == model.cursor, listWidth)
		}
		if side {
			text = ansi.Truncate(text, listWidth, "") + strings.Repeat(" ", max(0, listWidth-ansi.StringWidth(text))) + model.paint("│", richGray, false) + preview[line]
		}
		lines = append(lines, text)
	}
	if !side && size > 0 {
		lines = append(lines, model.paint(strings.Repeat("─", max(1, model.width)), richGray, false))
		lines = append(lines, model.previewLines(model.width, size)...)
	}
	lines = append(lines, model.statusLine(), model.hintLine())
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	return view
}

func (model findModel) paint(text, color string, bold bool) string {
	if !model.config.color {
		return text
	}
	return richPaint(text, color, bold)
}

func (model findModel) promptLine() string {
	if model.grepMode {
		prompt := T("find.picker.grep_prompt", viewSanitize(model.grepPath)) + " "
		return ansi.Truncate(model.paint(prompt, richCyan, true)+viewSanitize(model.grepPattern)+"█", model.width, "…")
	}
	count := fmt.Sprintf("%d/%d", model.total, len(model.paths))
	if model.walking {
		count += " " + T("find.picker.reading")
	}
	left := model.paint("> ", richCyan, true) + viewSanitize(model.query) + "█"
	room := model.width - ansi.StringWidth(left) - ansi.StringWidth(count) - 1
	if room < 1 {
		return ansi.Truncate(left, model.width, "…")
	}
	return left + strings.Repeat(" ", room) + model.paint(count, richGray, false)
}

// row는 결과 한 줄이다. 경로가 너무 길면 파일 이름을 남기고 앞을 줄인다.
func (model findModel) row(index int, selected bool, width int) string {
	path := model.paths[index]
	kind := model.kinds[index]
	suffix := ""
	switch kind {
	case findKindDir:
		suffix = "/"
	case findKindLink:
		suffix = "@"
	}
	meta := ""
	if model.config.stat {
		age := findAge(model.config.now().Sub(time.Unix(0, model.mtimes[index])))
		size := ""
		if kind == findKindFile {
			size = formatSizeShort(uint64(max(0, int(model.sizes[index]))))
		}
		meta = fmt.Sprintf("%9s %4s", size, age)
	}
	marker := "  "
	if selected {
		marker = model.paint(liveSelectedBar, richCyan, true) + " "
	}
	room := width - 2 - ansi.StringWidth(meta) - 1
	shown := model.highlight(path, room-len(suffix)) + suffix
	gap := max(1, room-ansi.StringWidth(shown)+1)
	return marker + shown + strings.Repeat(" ", gap) + model.paint(meta, richGray, false)
}

// highlight는 경로를 width칸에 맞추고 맞은 글자를 칠한다. 줄일 때는 앞을 자르고 …를 붙인다.
func (model findModel) highlight(path string, width int) string {
	start := 0
	if width > 0 && ansi.StringWidth(viewSanitize(path)) > width {
		for start < len(path) && ansi.StringWidth(viewSanitize(path[start:]))+1 > width {
			_, size := utf8.DecodeRuneInString(path[start:])
			start += size
		}
	}
	spans := model.matcher.spans(path, findBase(path))
	var builder strings.Builder
	if start > 0 {
		builder.WriteString("…")
	}
	position := start
	for _, span := range spans {
		if span[1] <= start {
			continue
		}
		from := max(span[0], start)
		builder.WriteString(viewSanitize(path[position:from]))
		builder.WriteString(model.paint(viewSanitize(path[from:span[1]]), richRed, true))
		position = span[1]
	}
	builder.WriteString(viewSanitize(path[position:]))
	return builder.String()
}

func (model findModel) statusLine() string {
	var parts []string
	if model.problem != "" {
		parts = append(parts, model.paint(model.problem, richRed, false))
	}
	if model.status != "" {
		parts = append(parts, model.paint(model.status, richYellow, false))
	}
	// 결과가 빠졌을 수 있다는 경고를 먼저 둔다. 좁은 화면에서는 뒤가 잘린다.
	if len(model.report.Stalled) > 0 {
		parts = append(parts, model.paint(T("find.picker.stalled", len(model.report.Stalled), findFirst(model.report.Stalled, 1)[0]), richYellow, false))
	} else if model.report.Partial {
		parts = append(parts, model.paint(T("find.picker.partial"), richYellow, false))
	}
	if model.limited {
		parts = append(parts, model.paint(T("find.note.limited", model.limit), richYellow, false))
	}
	for _, problem := range model.report.Problems {
		parts = append(parts, model.paint(viewSanitize(problem), richRed, false))
	}
	if model.report.UnreadableCount > 0 {
		parts = append(parts, T("find.picker.unreadable", model.report.UnreadableCount))
	}
	if skipped := findSkippedSummary(model.report.Skipped); skipped != "" {
		parts = append(parts, T("find.note.skipped_short", skipped))
	}
	return ansi.Truncate(strings.Join(parts, " · "), model.width, "…")
}

func (model findModel) hintLine() string {
	hint := T("find.picker.hint")
	if model.grepMode {
		hint = T("find.picker.grep_hint")
	} else if !model.config.stdoutTerminal {
		hint = T("find.picker.hint_pipe")
	}
	return ansi.Truncate(model.paint(hint, richGray, false), model.width, "…")
}

// findAge는 바뀐 지 얼마인지를 4칸 안에 적는다.
func findAge(elapsed time.Duration) string {
	switch {
	case elapsed < time.Minute:
		return "<1m"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed/time.Minute))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed/time.Hour))
	case elapsed < 60*24*time.Hour:
		return fmt.Sprintf("%dd", int(elapsed/(24*time.Hour)))
	case elapsed < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(elapsed/(30*24*time.Hour)))
	}
	return fmt.Sprintf("%dy", int(elapsed/(365*24*time.Hour)))
}
