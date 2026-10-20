package edc

import (
	"context"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// duBrowser는 사람이 보는 터미널에서 부른 edc du다. 탐색이 도는 동안 읽은 디렉터리 수를 보이고, 끝나면 큰
// 항목부터 디렉터리를 내려가며 본다. 읽기만 한다. 파일을 지우거나 바꾸는 키는 두지 않는다.
type duBrowser struct {
	root         string
	started      time.Time
	now          func() time.Time
	progress     *atomic.Int64
	cancel       context.CancelFunc
	job          *duScanJob
	stopping     bool
	noBrowse     bool
	workers      int
	spinnerFrame int
	// quitAfterScan은 탐색 중에 Ctrl-C를 눌렀다는 뜻이다. 탐색이 멈추면 화면을 열지 않고 닫는다.
	quitAfterScan bool
	scanned       bool
	scan          duScan
	err           error
	current       *duNode
	cursor        int
	offset        int
	width         int
	height        int
	color         bool
}

// duScanJob은 화면 밖에서 도는 탐색이다. finished를 닫아 끝을 알린다. 화면이 먼저 끝나도 결과를 다시 읽을 수
// 있게 channel로 값을 넘기지 않고 닫기만 한다.
type duScanJob struct {
	finished chan struct{}
	scan     duScan
	err      error
}

type duScanDoneMsg struct{}

type duTickMsg struct{}

// duBrowseRow는 화면의 한 줄이다. node가 없으면 큰 파일로 남기지 않은 파일을 합친 줄이거나 건너뛴 마운트다.
type duBrowseRow struct {
	node  *duNode
	mount *duMount
	bytes uint64
	files int64
}

const (
	duBrowseTick = 100 * time.Millisecond
	duBarWidth   = 10
)

func duBrowse(ctx context.Context, cancel context.CancelFunc, options duOptions, sources duSources, started time.Time, noBrowse bool) (duScan, error) {
	options.progress = new(atomic.Int64)
	job := &duScanJob{finished: make(chan struct{})}
	go func() {
		job.scan, job.err = duWalk(ctx, options, sources)
		close(job.finished)
	}()
	model := newDuBrowseModel(options, cancel, job, started, noBrowse)
	final, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout)).Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if browser, ok := final.(duBrowser); ok && browser.scanned {
		return browser.scan, browser.err
	}
	// 화면이 탐색보다 먼저 끝났다. 탐색을 멈추고 그때까지 센 값을 받는다.
	cancel()
	<-job.finished
	return job.scan, job.err
}

// newDuBrowseModel은 화면 모델에 실제 탐색의 옵션을 옮긴다. 진행 줄의 worker 수는 여기서 정해진다.
func newDuBrowseModel(options duOptions, cancel context.CancelFunc, job *duScanJob, started time.Time, noBrowse bool) duBrowser {
	model := newDuBrowser(options.root, options.progress, cancel, job, started, time.Now, true)
	model.noBrowse = noBrowse
	model.workers = options.workers
	return model
}

func newDuBrowser(root string, progress *atomic.Int64, cancel context.CancelFunc, job *duScanJob, started time.Time, now func() time.Time, color bool) duBrowser {
	return duBrowser{root: root, started: started, now: now, progress: progress, cancel: cancel, job: job, color: color, width: 80, height: 24}
}

func (model duBrowser) Init() tea.Cmd { return tea.Batch(model.tick(), model.wait()) }

func (model duBrowser) tick() tea.Cmd {
	return tea.Tick(duBrowseTick, func(time.Time) tea.Msg { return duTickMsg{} })
}

func (model duBrowser) wait() tea.Cmd {
	job := model.job
	return func() tea.Msg {
		<-job.finished
		return duScanDoneMsg{}
	}
}

func (model duBrowser) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = value.Width, value.Height
		model.keepCursor()
	case duTickMsg:
		if !model.scanned {
			model.spinnerFrame = (model.spinnerFrame + 1) % len(liveSpinnerFrames)
			return model, model.tick()
		}
	case duScanDoneMsg:
		model.scanned, model.scan, model.err = true, model.job.scan, model.job.err
		if model.err != nil || model.quitAfterScan || model.noBrowse {
			return model, tea.Quit
		}
		model.current, model.cursor, model.offset = model.scan.root, 0, 0
	case tea.KeyPressMsg:
		if model.noBrowse && value.String() != "ctrl+c" {
			return model, nil
		}
		if !model.scanned {
			return model.stopKey(value)
		}
		return model.browseKey(value)
	}
	return model, nil
}

// stopKey는 탐색 중의 키다. q와 Esc는 탐색을 멈추고 그때까지 센 결과를 화면에 연다. 오래 기다린 탐색 결과를
// 바로 버리지 않으려는 것이다. Ctrl-C는 다른 화면처럼 빠져나가되, 멈춘 결과는 닫은 뒤에 출력한다.
func (model duBrowser) stopKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "ctrl+c":
		model.quitAfterScan = true
		fallthrough
	case "q", "esc":
		if !model.stopping && model.cancel != nil {
			model.stopping = true
			model.cancel()
		}
	}
	return model, nil
}

func (model duBrowser) browseKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	rows := model.rows()
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
		model.cursor = len(rows) - 1
	case "enter", "right", "l":
		if model.cursor >= 0 && model.cursor < len(rows) {
			if node := rows[model.cursor].node; node != nil && node.dir {
				model.current, model.cursor, model.offset = node, 0, 0
			}
		}
	case "left", "h", "backspace", "esc":
		// 위로 올라가면 방금 나온 디렉터리에 커서를 둔다. 맨 위에서는 아무것도 하지 않는다.
		if parent := model.current.parent; parent != nil {
			from := model.current
			model.current, model.cursor, model.offset = parent, 0, 0
			for index, row := range model.rows() {
				if row.node == from {
					model.cursor = index
				}
			}
		}
	}
	model.keepCursor()
	return model, nil
}

// rows는 지금 디렉터리의 항목을 크기순으로 둔다. 큰 파일로 남기지 않은 파일은 한 줄로 합치고, 건너뛴 마운트는
// 크기를 모르므로 맨 뒤에 둔다.
func (model duBrowser) rows() []duBrowseRow {
	current := model.current
	if current == nil {
		return nil
	}
	rows := make([]duBrowseRow, 0, len(current.children)+1)
	for _, child := range current.children {
		rows = append(rows, duBrowseRow{node: child, bytes: child.total})
	}
	if files := current.entries - 1; files > 0 {
		rows = append(rows, duBrowseRow{bytes: current.bytes - current.own, files: files})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].bytes != rows[j].bytes {
			return rows[i].bytes > rows[j].bytes
		}
		return rows[i].name() < rows[j].name()
	})
	path := duNodePath(current)
	for index := range model.scan.mounts {
		if filepath.Dir(model.scan.mounts[index].Path) == path {
			rows = append(rows, duBrowseRow{mount: &model.scan.mounts[index]})
		}
	}
	return rows
}

func (row duBrowseRow) name() string {
	switch {
	case row.node != nil:
		return row.node.name
	case row.mount != nil:
		return filepath.Base(row.mount.Path)
	}
	return ""
}

func duNodePath(node *duNode) string {
	if node.parent == nil {
		return node.name
	}
	return filepath.Join(duNodePath(node.parent), node.name)
}

// duBrowseCells는 한 줄의 칸이다. 평문과 색 화면이 같은 칸을 같은 폭으로 쓴다. notes는 이름 뒤에 두 칸씩 띄워 붙인다.
type duBrowseCells struct {
	size, share, name string
	notes             []duBrowseNote
	// secondary는 나머지 파일 줄과 마운트 줄이다. 색 화면에서 크기와 이름을 회색으로 둔다.
	secondary bool
}

type duBrowseNote struct{ text, color string }

func (model duBrowser) cells(row duBrowseRow) duBrowseCells {
	if row.mount != nil {
		cells := duBrowseCells{size: "-", name: row.name() + "/", secondary: true,
			notes: []duBrowseNote{{T("du.browse.mount", row.mount.FSType), richGray}}}
		if label := model.scan.labels[row.mount.Path]; label != "" {
			cells.notes = append(cells.notes, duBrowseNote{label, richGray})
		}
		return cells
	}
	share := 0
	if total := model.current.total; total > 0 {
		share = int(row.bytes * 100 / total)
	}
	cells := duBrowseCells{size: formatSizeShort(row.bytes), share: fmt.Sprintf("%d%%", share)}
	switch {
	case row.node == nil:
		cells.name, cells.secondary = T("du.browse.other", row.files), true
	case row.node.dir:
		cells.name = row.node.name + "/"
	default:
		cells.name = row.node.name
	}
	if row.node != nil && row.node.unreadable {
		cells.notes = append(cells.notes, duBrowseNote{T("du.browse.unreadable"), richYellow})
	}
	if row.node != nil {
		if label := model.scan.labels[duNodePath(row.node)]; label != "" {
			cells.notes = append(cells.notes, duBrowseNote{label, richGray})
		}
	}
	return cells
}

func (model duBrowser) rowText(row duBrowseRow) string {
	cells := model.cells(row)
	bar := strings.Repeat(" ", duBarWidth)
	if row.mount == nil {
		bar = duBar(row.bytes, model.current.total)
	}
	text := fmt.Sprintf("%9s  %s %4s  %s", cells.size, bar, cells.share, cells.name)
	for _, note := range cells.notes {
		text += "  " + note.text
	}
	return text
}

// richRowText는 색 화면의 한 줄이다. 막대는 이 디렉터리에서 가장 큰 항목에 맞춘 차분한 색이다. bars가 false면
// 막대 자리를 비운다. 항목이 하나뿐이면 막대가 늘 가득 차서 아무것도 알려 주지 않는다. 고른 줄은 selectedCell이
// 한 번에 칠하도록 글자만 돌려준다.
func (model duBrowser) richRowText(row duBrowseRow, largest uint64, bars, selected bool) string {
	paint, fill := model.paint, model.fill
	if selected {
		paint = func(text, _ string, _ bool) string { return text }
		fill = func(text, _ string) string { return text }
	}
	cells := model.cells(row)
	color := richSizeColor(int64(row.bytes))
	if cells.secondary {
		color = richGray
	}
	bar := strings.Repeat(" ", duBarWidth)
	if bars && row.mount == nil {
		bar = fill(richBar(float64(row.bytes), float64(largest), duBarWidth), color)
	}
	name := cells.name
	if cells.secondary {
		name = paint(name, richGray, false)
	}
	text := paint(fmt.Sprintf("%9s", cells.size), color, false) + "  " + bar + " " +
		paint(fmt.Sprintf("%4s", cells.share), richGray, false) + "  " + name
	for _, note := range cells.notes {
		text += "  " + paint(note.text, note.color, false)
	}
	return text
}

// paint는 lipgloss로 rich.go의 고정 색을 칠한다. 색 수는 Bubble Tea가 터미널에 맞춰 줄인다. color는 richRed 같은
// ANSI 번호이고, 빈 문자열이면 기본 색이다.
func (model duBrowser) paint(text, color string, bold bool) string {
	if !model.color || text == "" {
		return text
	}
	style := lipgloss.NewStyle().Bold(bold)
	if hex := richHex(color); hex != "" {
		style = style.Foreground(duScreenColor(hex))
	}
	return style.Render(text)
}

func (model duBrowser) fill(text, color string) string {
	if !model.color {
		return text
	}
	return lipgloss.NewStyle().Foreground(duScreenColor(richFillHex(color))).Render(text)
}

// duScreenColor는 truecolor가 아닌 터미널에서 rich256이 고른 256색 번호를 직접 넘긴다. Bubble Tea가 줄이게 두면
// chip 바탕 #262b33이 회색(236)이 아니라 남색(17)이 되어, 고른 줄이 파랗게 칠해졌다.
func duScreenColor(hex string) color.Color {
	var rgb richRGB
	if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &rgb.r, &rgb.g, &rgb.b); err != nil || richTruecolor() {
		return lipgloss.Color(hex)
	}
	return lipgloss.Color(strconv.Itoa(rich256(rgb)))
}

// selectedCell은 고른 줄을 chip 바탕과 굵은 글씨로 width칸까지 칠한다. 칸마다 칠한 글자를 다시 감싸면 안쪽 reset이
// 바탕을 지우므로 평문을 칠한다.
func (model duBrowser) selectedCell(text string, width int) string {
	if !model.color {
		return liveCell(text, width)
	}
	return lipgloss.NewStyle().Width(width).Bold(true).Background(duScreenColor(richChipHex())).
		Foreground(duScreenColor(richHex("97"))).Render(text)
}

// duBar는 지금 디렉터리의 합 가운데 이 항목의 몫이다. 색 없이도 길이로 읽힌다.
func duBar(bytes, total uint64) string {
	filled := 0
	if total > 0 {
		filled = int((bytes*duBarWidth + total/2) / total)
	}
	filled = min(filled, duBarWidth)
	return strings.Repeat("█", filled) + strings.Repeat("░", duBarWidth-filled)
}

// listRows는 항목 줄 수다. 제목, 키 안내, 고른 경로, 파일 시스템 줄이 네 줄이다.
func (model duBrowser) listRows() int { return model.height - 4 }

func (model *duBrowser) keepCursor() {
	count := len(model.rows())
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

func (model duBrowser) View() tea.View {
	// --no-browse는 alt screen 없이 그리므로 종료할 때의 마지막 화면이 결과 위에 남는다. 스캔이 끝나면 비운다.
	if model.noBrowse && model.scanned {
		return tea.NewView("")
	}
	var lines []string
	if model.scanned && model.current != nil {
		lines = model.browseLines()
	} else {
		lines = model.progressLines()
	}
	for index, line := range lines {
		lines[index] = model.fit(line)
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = !model.noBrowse
	return view
}

func (model duBrowser) fit(line string) string {
	if model.width <= 0 {
		return line
	}
	return ansi.Truncate(line, model.width, "…")
}

func (model duBrowser) progressLines() []string {
	elapsed := model.now().Sub(model.started).Round(liveElapsedPrecision)
	help := T("du.browse.stop_help")
	if model.stopping {
		help = T("du.browse.stopping")
	}
	reading := T("du.browse.reading", model.root, model.progress.Load(), elapsed, model.workers)
	if model.noBrowse {
		reading = liveSpinnerFrames[model.spinnerFrame] + " " + reading
		if !model.stopping {
			help = T("du.browse.cancel_help")
		}
	}
	return []string{reading, liveKeyHints(help, model.color)}
}

func (model duBrowser) browseLines() []string {
	lines := []string{model.title(), liveKeyHints(T("du.browse.help"), model.color)}
	rows := model.rows()
	texts := make([]string, len(rows))
	width := 0
	var largest uint64
	sized := 0
	for index, row := range rows {
		texts[index] = model.rowText(row)
		width = max(width, liveWidth(texts[index]))
		if row.mount == nil {
			if row.bytes > largest {
				largest = row.bytes
			}
			sized++
		}
	}
	if model.width > 0 {
		width = min(width, model.width-2)
	}
	if len(rows) == 0 {
		lines = append(lines, liveIdleBar+" "+model.paint(T("du.browse.empty"), richGray, false))
	}
	end := min(len(rows), model.offset+max(1, model.listRows()))
	for index := model.offset; index < end; index++ {
		text := texts[index]
		selected := index == model.cursor
		if model.color {
			text = model.richRowText(rows[index], largest, sized > 1, selected)
		}
		if model.width > 0 {
			text = ansi.Truncate(text, model.width-2, "…")
		}
		if selected {
			lines = append(lines, liveSelectedBar+" "+model.selectedCell(text, width))
			continue
		}
		lines = append(lines, liveIdleBar+" "+text)
	}
	// 아래 두 줄은 항목 수와 상관없이 화면 맨 아래에 둔다.
	for len(lines) < 2+model.listRows() {
		lines = append(lines, "")
	}
	return append(lines, model.paint(model.selectedPath(rows), richGray, false), model.richFooter())
}

// title은 "edc du · 경로 · 크기 · 항목 수"다. 색 화면에서는 edc du를 굵은 자주, 경로를 굵게, 크기를 크기 색으로 칠한다.
func (model duBrowser) title() string {
	current := model.current
	if !model.color {
		return T("du.browse.title", duNodePath(current), formatSizeShort(current.total), current.count, model.workers)
	}
	text := strings.Replace(T("du.browse.title", "\x00", "\x01", current.count, model.workers), "edc du", "\x02", 1)
	gray := func(text string) string { return model.paint(text, richGray, false) }
	return duPhrase(text, gray, model.paint(duNodePath(current), "", true),
		model.paint(formatSizeShort(current.total), richSizeColor(int64(current.total)), true), model.paint("edc du", richMagenta, true))
}

func (model duBrowser) selectedPath(rows []duBrowseRow) string {
	if model.cursor < 0 || model.cursor >= len(rows) {
		return duNodePath(model.current)
	}
	switch row := rows[model.cursor]; {
	case row.node != nil:
		return duNodePath(row.node)
	case row.mount != nil:
		return row.mount.Path
	}
	return duNodePath(model.current)
}

// footer는 2단계의 파일 시스템 줄을 한 줄로 줄인 것이다. 일부만 셌으면 함께 알린다.
func (model duBrowser) footer() string {
	var texts []string
	for _, part := range model.footerParts() {
		texts = append(texts, part.text)
	}
	return strings.Join(texts, " · ")
}

// footerParts의 색은 색 화면에서만 쓴다. 다시 확인해야 하는 상태(일부만 셈, 읽지 못함)는 노랑, 나머지는 회색이다.
func (model duBrowser) footerParts() []duBrowseNote {
	var parts []duBrowseNote
	if filesystem := model.scan.filesystem; filesystem.Mount != "" {
		parts = append(parts, duBrowseNote{T("du.browse.fs", duMountLabel(filesystem), filesystem.FSType, formatSizeShort(filesystem.UsedBytes), formatSizeShort(filesystem.SizeBytes)), richGray})
	}
	if deleted := model.scan.deleted; deleted.Files > 0 {
		size := formatSizeShort(deleted.Bytes)
		if deleted.Partial {
			size = "≥" + size
		}
		parts = append(parts, duBrowseNote{T("du.browse.deleted", size), richGray})
	}
	if model.scan.partial {
		parts = append(parts, duBrowseNote{T("du.browse.partial"), richYellow})
	}
	if model.scan.unreadableCount > 0 {
		parts = append(parts, duBrowseNote{T("du.browse.unreadable_count", model.scan.unreadableCount), richYellow})
	}
	return parts
}

func (model duBrowser) richFooter() string {
	if !model.color {
		return model.footer()
	}
	var texts []string
	for _, part := range model.footerParts() {
		texts = append(texts, model.paint(part.text, part.color, false))
	}
	return strings.Join(texts, model.paint(" · ", richGray, false))
}
