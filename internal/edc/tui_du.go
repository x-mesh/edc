package edc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
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
	model := newDuBrowser(options.root, options.progress, cancel, job, started, time.Now, true)
	model.noBrowse = noBrowse
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

func (model duBrowser) rowText(row duBrowseRow) string {
	if row.mount != nil {
		text := fmt.Sprintf("%9s  %s       %s/  %s", "-", strings.Repeat(" ", duBarWidth), row.name(), T("du.browse.mount", row.mount.FSType))
		if label := model.scan.labels[row.mount.Path]; label != "" {
			text += "  " + label
		}
		return text
	}
	total, share := model.current.total, 0
	if total > 0 {
		share = int(row.bytes * 100 / total)
	}
	var name string
	switch {
	case row.node == nil:
		name = T("du.browse.other", row.files)
	case row.node.dir:
		name = row.node.name + "/"
	default:
		name = row.node.name
	}
	if row.node != nil && row.node.unreadable {
		name += "  " + T("du.browse.unreadable")
	}
	if row.node != nil {
		if label := model.scan.labels[duNodePath(row.node)]; label != "" {
			name += "  " + label
		}
	}
	return fmt.Sprintf("%9s  %s %3d%%  %s", formatSizeShort(row.bytes), duBar(row.bytes, total), share, name)
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
	reading := T("du.browse.reading", model.root, model.progress.Load(), elapsed)
	if model.noBrowse {
		reading = liveSpinnerFrames[model.spinnerFrame] + " " + reading
		if !model.stopping {
			help = T("du.browse.cancel_help")
		}
	}
	return []string{reading, liveMuted(help, model.color)}
}

func (model duBrowser) browseLines() []string {
	current := model.current
	lines := []string{
		T("du.browse.title", duNodePath(current), formatSizeShort(current.total), current.count),
		liveMuted(T("du.browse.help"), model.color),
	}
	rows := model.rows()
	texts := make([]string, len(rows))
	width := 0
	for index, row := range rows {
		texts[index] = model.rowText(row)
		width = max(width, liveWidth(texts[index]))
	}
	if model.width > 0 {
		width = min(width, model.width-2)
	}
	if len(rows) == 0 {
		lines = append(lines, liveIdleBar+" "+T("du.browse.empty"))
	}
	end := min(len(rows), model.offset+max(1, model.listRows()))
	for index := model.offset; index < end; index++ {
		text := texts[index]
		if model.width > 0 {
			text = ansi.Truncate(text, model.width-2, "…")
		}
		if index == model.cursor {
			lines = append(lines, liveSelectedBar+" "+liveSelected(liveCell(text, width), model.color))
			continue
		}
		lines = append(lines, liveIdleBar+" "+text)
	}
	// 아래 두 줄은 항목 수와 상관없이 화면 맨 아래에 둔다.
	for len(lines) < 2+model.listRows() {
		lines = append(lines, "")
	}
	return append(lines, liveMuted(model.selectedPath(rows), model.color), liveMuted(model.footer(), model.color))
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
	var parts []string
	if filesystem := model.scan.filesystem; filesystem.Mount != "" {
		parts = append(parts, T("du.browse.fs", duMountLabel(filesystem), filesystem.FSType, formatSizeShort(filesystem.UsedBytes), formatSizeShort(filesystem.SizeBytes)))
	}
	if deleted := model.scan.deleted; deleted.Files > 0 {
		size := formatSizeShort(deleted.Bytes)
		if deleted.Partial {
			size = "≥" + size
		}
		parts = append(parts, T("du.browse.deleted", size))
	}
	if model.scan.partial {
		parts = append(parts, T("du.browse.partial"))
	}
	if model.scan.unreadableCount > 0 {
		parts = append(parts, T("du.browse.unreadable_count", model.scan.unreadableCount))
	}
	return strings.Join(parts, " · ")
}
