package edc

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func duBrowserOn(scan duScan, width, height int) duBrowser {
	duTotal(scan.root)
	job := &duScanJob{finished: make(chan struct{}), scan: scan}
	close(job.finished)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	model := newDuBrowser(scan.root.name, new(atomic.Int64), nil, job, started, func() time.Time { return started }, false)
	next, _ := model.Update(duScanDoneMsg{})
	next, _ = next.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return next.(duBrowser)
}

func duBrowserPress(t *testing.T, model duBrowser, keys ...string) duBrowser {
	t.Helper()
	var current tea.Model = model
	for _, key := range keys {
		var message tea.KeyPressMsg
		switch key {
		case "left":
			message = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			message = tea.KeyPressMsg{Code: tea.KeyRight}
		case "backspace":
			message = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			message = tea.KeyPressMsg{Code: keyCode(key), Text: keyText(key)}
		}
		current, _ = current.Update(message)
	}
	return current.(duBrowser)
}

func duBrowserTree() *duNode {
	lib := duTestDir("lib", 4, duTestFile("big", 60), duTestDir("nested", 4, duTestFile("x", 26)))
	root := duTestDir("/data", 4, lib, duTestDir("log", 30), duTestFile("c", 10))
	root.own, lib.own = 4, 4
	return root
}

func TestDuBrowserOpensADirectoryAndComesBackToIt(t *testing.T) {
	model := duBrowserOn(duScan{root: duBrowserTree()}, 80, 12)
	if got := duRowNames(model.rows()); got != "lib log c" {
		t.Fatalf("rows %q", got)
	}
	model = duBrowserPress(t, model, "down", "enter")
	if duNodePath(model.current) != "/data/log" {
		t.Fatalf("current %s", duNodePath(model.current))
	}
	model = duBrowserPress(t, model, "left")
	if model.current.parent != nil || model.cursor != 1 {
		t.Fatalf("back at %s, cursor %d", duNodePath(model.current), model.cursor)
	}
	// 파일에서 Enter는 아무것도 하지 않고, 맨 위에서 Esc도 아무것도 하지 않는다.
	model = duBrowserPress(t, model, "down", "enter", "esc")
	if model.current.parent != nil || model.cursor != 2 {
		t.Fatalf("at %s, cursor %d", duNodePath(model.current), model.cursor)
	}
	model = duBrowserPress(t, model, "g", "l", "down", "right")
	if duNodePath(model.current) != "/data/lib/nested" {
		t.Fatalf("current %s", duNodePath(model.current))
	}
	model = duBrowserPress(t, model, "backspace", "h")
	if model.current.parent != nil || model.cursor != 0 {
		t.Fatalf("back at %s, cursor %d", duNodePath(model.current), model.cursor)
	}
}

func duRowNames(rows []duBrowseRow) string {
	var names []string
	for _, row := range rows {
		names = append(names, row.name())
	}
	return strings.Join(names, " ")
}

func TestDuBrowserShowsOtherFilesMountsAndUnreadableDirectories(t *testing.T) {
	root := duBrowserTree()
	// root에는 큰 파일로 남기지 않은 파일 3개, 합 45가 있다.
	root.bytes, root.entries = 4+45, 4
	locked := duTestDir("locked", 0)
	locked.unreadable, locked.parent = true, root
	root.children = append(root.children, locked)
	scan := duScan{root: root, mounts: []duMount{{Path: "/data/vol", FSType: "ext4"}, {Path: "/data/lib/deep", FSType: "tmpfs"}}}
	model := duBrowserOn(scan, 80, 14)
	view := model.View().Content
	for _, want := range []string{
		"edc du · /data · ",
		"  45 B  ",
		"(3 other files)",
		"  locked/  (unreadable)",
		"vol/  (ext4 mount, not counted)",
		"\n/data/lib\n",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in\n%s", want, view)
		}
	}
	if strings.Contains(view, "deep/") {
		t.Fatalf("a mount below another directory is listed here:\n%s", view)
	}
	if got := duRowNames(model.rows()); got != "lib  log c locked vol" {
		t.Fatalf("rows %q", got)
	}
}

func TestDuBrowserScrollsAndFitsTheScreen(t *testing.T) {
	root := duTestDir("/many", 4)
	for index := range 30 {
		child := duTestFile(fmt.Sprintf("file-with-a-long-name-%02d", index), uint64(1000-index))
		child.parent = root
		root.children = append(root.children, child)
	}
	scan := duScan{root: root, filesystem: duFilesystem{Mount: "/", FSType: "xfs", SizeBytes: 1 << 30, UsedBytes: 1 << 20}, partial: true, unreadableCount: 2}
	model := duBrowserPress(t, duBrowserOn(scan, 40, 10), "G")
	if model.cursor != 29 || model.offset != 24 {
		t.Fatalf("cursor %d offset %d", model.cursor, model.offset)
	}
	lines := strings.Split(model.View().Content, "\n")
	if len(lines) != 10 {
		t.Fatalf("%d lines", len(lines))
	}
	for _, line := range lines {
		if width := liveWidth(line); width > 40 {
			t.Errorf("line %q is %d columns", line, width)
		}
	}
	if !strings.Contains(lines[8], "/many/file-with-a-long-name-29") || !strings.HasPrefix(lines[9], "file system / xfs") {
		t.Fatalf("footer %q %q", lines[8], lines[9])
	}
	wide := strings.Split(duBrowserPress(t, duBrowserOn(scan, 120, 10), "G").View().Content, "\n")
	if !strings.HasSuffix(wide[9], " · partial · 2 unreadable") {
		t.Fatalf("footer %q", wide[9])
	}
	// bind mount는 2단계의 파일 시스템 줄과 같이 장치[디렉터리]로 적는다.
	scan.filesystem = duFilesystem{Mount: "/usr", FSType: "xfs", Source: "/dev/sda3", Root: "/usr", SizeBytes: 1 << 30}
	if footer := duBrowserOn(scan, 120, 10).footer(); !strings.HasPrefix(footer, "file system /usr (/dev/sda3[/usr]) xfs") {
		t.Fatalf("footer %q", footer)
	}
}

// 색 화면은 평문 화면과 같은 글자를 쓴다. 고른 줄은 chip 바탕의 굵은 평문이고, 나머지 줄은 크기 색과 차분한 막대를 쓴다.
func TestDuBrowserColorsRowsAndMarksTheSelectedRowWithAChip(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	scan := duScan{root: duBrowserTree(), filesystem: duFilesystem{Mount: "/", FSType: "xfs", SizeBytes: 1 << 30, UsedBytes: 1 << 20}, partial: true}
	plain := duBrowserOn(scan, 80, 12)
	model := plain
	model.color = true
	lines := strings.Split(model.View().Content, "\n")
	plainLines := strings.Split(plain.View().Content, "\n")
	if len(lines) != len(plainLines) {
		t.Fatalf("%d lines, plain %d", len(lines), len(plainLines))
	}
	// 고른 줄은 칸마다 칠하지 않고 한 번에 chip으로 감싼다. 칸 사이의 reset이 바탕을 끊지 않는다.
	selected := ansi.Strip(lines[2])
	if !strings.Contains(lines[2], richRGBCode(richChipBackground, true, true)) || strings.Count(lines[2], "\x1b[") != 2 ||
		liveWidth(selected) != liveWidth(plainLines[2]) || !strings.Contains(selected, richBar(94, 94, duBarWidth)+"  68%  lib/") {
		t.Fatalf("selected row %q, plain %q", lines[2], plainLines[2])
	}
	magenta := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(richHex(richMagenta))).Render("edc du")
	if !strings.HasPrefix(lines[0], magenta) {
		t.Fatalf("title %q", lines[0])
	}
	// 막대는 합계(138)가 아니라 가장 큰 lib(94)에 맞춘다. 평문의 ░ 자리는 비운다.
	log := ansi.Strip(lines[3])
	if !strings.Contains(log, richBar(30, 94, duBarWidth)+"  21%  log/") || strings.Contains(log, "░") {
		t.Fatalf("log row %q", log)
	}
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color(richHex(richYellow))).Render("partial")
	if !strings.Contains(lines[len(lines)-1], yellow) {
		t.Fatalf("footer %q", lines[len(lines)-1])
	}
	for _, line := range lines {
		if liveWidth(line) > 80 || strings.Contains(line, liveReverse) || strings.Contains(line, liveDim) {
			t.Errorf("line %q", line)
		}
	}
	// 항목이 하나뿐인 디렉터리는 막대가 늘 가득 차므로 뺀다.
	nested := duBrowserPress(t, model, "enter", "down", "enter")
	if row := ansi.Strip(strings.Split(nested.View().Content, "\n")[2]); strings.Contains(row, "█") {
		t.Fatalf("single row keeps a bar: %q", row)
	}
	// 256색 터미널에서도 chip 바탕은 rich256의 회색이다. Bubble Tea가 줄이면 남색(17)이 된다.
	t.Setenv("COLORTERM", "")
	if line := strings.Split(model.View().Content, "\n")[2]; !strings.Contains(line, richRGBCode(richChipBackground, true, false)) {
		t.Fatalf("256 color selected row %q", line)
	}
}

func TestDuBrowserStopsTheScanAndOpensWhatItRead(t *testing.T) {
	cancelled := 0
	progress := new(atomic.Int64)
	progress.Store(1234)
	job := &duScanJob{finished: make(chan struct{})}
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	model := newDuBrowser("/data", progress, func() { cancelled++ }, job, started, func() time.Time { return started.Add(3200 * time.Millisecond) }, false)
	model.workers = 4
	if view := model.View().Content; !strings.HasPrefix(view, "reading /data · 1234 directories · 3.2s · workers 4\nq stops") {
		t.Fatalf("view %q", view)
	}
	model = duBrowserPress(t, model, "q", "esc")
	if cancelled != 1 || !model.stopping || model.scanned || !strings.Contains(model.View().Content, "stopping…") {
		t.Fatalf("cancelled %d, stopping %v, scanned %v", cancelled, model.stopping, model.scanned)
	}
	job.scan = duScan{root: duBrowserTree(), partial: true}
	duTotal(job.scan.root)
	close(job.finished)
	next, _ := model.Update(duScanDoneMsg{})
	model = next.(duBrowser)
	if !model.scanned || model.current != job.scan.root || !strings.Contains(model.footer(), "partial") {
		t.Fatalf("scanned %v, footer %q", model.scanned, model.footer())
	}
	if _, command := model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); command == nil {
		t.Fatal("q does not quit after the scan")
	} else if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("q does not quit after the scan")
	}
}

// 탐색 중의 Ctrl-C는 멈춘 결과를 화면에 열지 않고 닫는다. 결과는 닫은 뒤에 출력한다.
func TestDuBrowserCtrlCDuringTheScanQuits(t *testing.T) {
	cancelled := 0
	job := &duScanJob{finished: make(chan struct{})}
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	model := newDuBrowser("/data", new(atomic.Int64), func() { cancelled++ }, job, started, func() time.Time { return started }, false)
	next, _ := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	job.scan = duScan{root: duBrowserTree(), partial: true}
	duTotal(job.scan.root)
	close(job.finished)
	next, command := next.Update(duScanDoneMsg{})
	if cancelled != 1 || !next.(duBrowser).scanned || command == nil {
		t.Fatalf("cancelled %d, command %v", cancelled, command)
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c during the scan does not quit")
	}
}

func TestDuBrowserShowsContainerLabels(t *testing.T) {
	root := duBrowserTree()
	// 나머지 파일 줄은 node가 없다. 이름표를 찾을 때 그 줄을 건너뛰어야 한다.
	root.bytes, root.entries = 4+45, 4
	scan := duScan{root: root, mounts: []duMount{{Path: "/data/vol", FSType: "overlay"}},
		labels: map[string]string{"/data/lib": "container api-1", "/data/vol": "root file system of db"}}
	view := duBrowserOn(scan, 120, 14).View().Content
	for _, want := range []string{"lib/  container api-1", "vol/  (overlay mount, not counted)  root file system of db", "(3 other files)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in\n%s", want, view)
		}
	}
}

func TestDuNoBrowseShowsProgressAndQuitsWithoutNavigation(t *testing.T) {
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	job := &duScanJob{finished: make(chan struct{})}
	model := newDuBrowser("/data", new(atomic.Int64), nil, job, started, func() time.Time { return started }, false)
	model.noBrowse = true
	view := model.View()
	if view.AltScreen || !strings.Contains(view.Content, liveSpinnerFrames[0]+" reading /data") || strings.Contains(view.Content, "q stops") {
		t.Fatalf("progress=%+v", view)
	}
	next, _ := model.Update(duTickMsg{})
	model = next.(duBrowser)
	if !strings.Contains(model.View().Content, liveSpinnerFrames[1]) {
		t.Fatal("spinner does not advance")
	}
	model = duBrowserPress(t, model, "enter", "right", "down", "q")
	if model.stopping || model.current != nil {
		t.Fatal("navigation key affected the scan")
	}
	job.scan = duScan{root: duBrowserTree()}
	duTotal(job.scan.root)
	close(job.finished)
	next, command := model.Update(duScanDoneMsg{})
	if command == nil || !next.(duBrowser).scanned || next.(duBrowser).current != nil {
		t.Fatal("no-browse did not finish automatically")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("completion did not quit")
	}
	if after, command := next.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); command != nil || after.(duBrowser).current != nil {
		t.Fatal("enter enabled directory navigation")
	}
}

// --no-browse는 alt screen을 쓰지 않으므로 마지막 화면이 결과 위에 남는다. 스캔이 끝난 뒤에는 진행 줄을 그리지 않는다(#265).
func TestDuNoBrowseLeavesNoProgressLineAfterTheScan(t *testing.T) {
	for _, partial := range []bool{false, true} {
		job := &duScanJob{finished: make(chan struct{})}
		now := time.Now()
		model := newDuBrowser("/data", new(atomic.Int64), func() {}, job, now, time.Now, false)
		model.noBrowse = true
		job.scan = duScan{root: duBrowserTree(), partial: partial}
		duTotal(job.scan.root)
		close(job.finished)
		next, _ := model.Update(duScanDoneMsg{})
		if view := next.(duBrowser).View(); view.Content != "" || view.AltScreen {
			t.Fatalf("partial %t: final frame = %q", partial, view.Content)
		}
	}
}

func TestDuNoBrowseCtrlCStopsAndQuits(t *testing.T) {
	cancelled := 0
	job := &duScanJob{finished: make(chan struct{})}
	now := time.Now()
	model := newDuBrowser("/data", new(atomic.Int64), func() { cancelled++ }, job, now, time.Now, false)
	model.noBrowse = true
	next, _ := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	job.scan = duScan{root: duBrowserTree(), partial: true}
	duTotal(job.scan.root)
	close(job.finished)
	next, command := next.Update(duScanDoneMsg{})
	if cancelled != 1 || !next.(duBrowser).scan.partial || command == nil {
		t.Fatal("ctrl-c did not preserve the partial scan and quit")
	}
}

func TestDuBrowseModelCarriesTheWorkersAndModeOfTheScan(t *testing.T) {
	progress := new(atomic.Int64)
	progress.Store(42)
	started := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	options := duOptions{root: "/data", workers: 6, progress: progress}
	for _, noBrowse := range []bool{false, true} {
		model := newDuBrowseModel(options, func() {}, &duScanJob{finished: make(chan struct{})}, started, noBrowse)
		model.now = func() time.Time { return started.Add(1500 * time.Millisecond) }
		if model.workers != 6 || model.noBrowse != noBrowse || model.root != "/data" || model.progress != progress {
			t.Fatalf("noBrowse %v: model = %+v", noBrowse, model)
		}
		if view := model.View().Content; !strings.Contains(view, "reading /data · 42 directories · 1.5s · workers 6") {
			t.Errorf("noBrowse %v: view %q", noBrowse, view)
		}
	}
}

func TestDuBrowserTitleShowsTheWorkersOfTheScan(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	scan := duScan{root: duBrowserTree()}
	model := duBrowserOn(scan, 120, 12)
	model.workers = 4
	plainTitle := model.title()
	if !strings.HasSuffix(plainTitle, " · workers 4") || !strings.HasPrefix(plainTitle, "edc du · /data · ") {
		t.Fatalf("plain title %q", plainTitle)
	}
	model.color = true
	coloredTitle := model.title()
	if coloredTitle == plainTitle || ansi.Strip(coloredTitle) != plainTitle {
		t.Fatalf("colored title %q does not read like the plain title %q", ansi.Strip(coloredTitle), plainTitle)
	}
	model.workers = 1
	if title := model.title(); !strings.HasSuffix(ansi.Strip(title), " · workers 1") {
		t.Fatalf("title with one worker %q", ansi.Strip(title))
	}
}
