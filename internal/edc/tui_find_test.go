package edc

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

var findTestNow = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

func testFindModel(stdinMode bool) findModel {
	return newFindModel(findOptions{}, findPickerConfig{
		now: func() time.Time { return findTestNow }, self: "/usr/local/bin/edc", stdoutTerminal: true, editor: []string{"code", "-w"},
		stdinMode: stdinMode, stat: !stdinMode,
	})
}

func findTestEntry(path string, kind findKind, age time.Duration) findEntry {
	return findEntry{path: path, kind: kind, statted: true, size: 2048, mtime: findTestNow.Add(-age).UnixNano()}
}

func findUpdate(t *testing.T, model findModel, msg tea.Msg) (findModel, tea.Cmd) {
	t.Helper()
	next, cmd := model.Update(msg)
	return next.(findModel), cmd
}

// findType는 글자를 하나씩 치고, 계산 결과를 받는다.
func findType(t *testing.T, model findModel, text string) findModel {
	t.Helper()
	for _, r := range text {
		var cmd tea.Cmd
		model, cmd = findUpdate(t, model, tea.KeyPressMsg{Code: r, Text: string(r)})
		if cmd != nil {
			model, _ = findUpdate(t, model, cmd())
		}
	}
	return model
}

func findShown(model findModel) []string {
	var paths []string
	for _, hit := range model.top {
		paths = append(paths, model.paths[hit.index])
	}
	return paths
}

func TestFindModelShowsRecentFilesFirstForAnEmptyQuery(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{
		findTestEntry("old.log", findKindFile, 48*time.Hour), findTestEntry("new.log", findKindFile, time.Minute),
	}})
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("now.log", findKindFile, 0)}})
	if got := findShown(model); !reflect.DeepEqual(got, []string{"now.log", "new.log", "old.log"}) || model.total != 3 {
		t.Errorf("shown = %v, total %d", got, model.total)
	}
	// stdin의 줄은 stat하지 않으므로 들어온 순서다.
	stdin := testFindModel(true)
	stdin, _ = findUpdate(t, stdin, findBatchMsg{entries: []findEntry{{path: "b"}, {path: "a"}, {path: "c"}}})
	if got := findShown(stdin); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
		t.Errorf("stdin = %v", got)
	}
}

func TestFindModelRanksTypedQueriesAndMergesLateCandidates(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{
		findTestEntry("internal/edc/audit_journal.go", findKindFile, time.Hour), findTestEntry("android/jar/run.txt", findKindFile, time.Hour),
		findTestEntry("README.md", findKindFile, time.Hour),
	}})
	model = findType(t, model, "adjrn")
	if got := findShown(model); !reflect.DeepEqual(got, []string{"internal/edc/audit_journal.go", "android/jar/run.txt"}) || model.total != 2 {
		t.Fatalf("shown = %v", got)
	}
	// 계산이 끝난 뒤에 들어온 후보도 지금 질의로 계산해 합친다.
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("docs/adjrn.md", findKindFile, time.Hour)}})
	if got := findShown(model); got[0] != "docs/adjrn.md" || model.total != 3 {
		t.Errorf("late = %v, total %d", got, model.total)
	}
	// 지운 글자는 다시 넓힌다.
	for range 5 {
		var cmd tea.Cmd
		model, cmd = findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyBackspace})
		if cmd != nil {
			model, _ = findUpdate(t, model, cmd())
		}
	}
	if model.query != "" || model.total != 4 {
		t.Errorf("query %q, total %d", model.query, model.total)
	}
}

func TestFindModelDropsAnOldSearch(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("alpha.go", findKindFile, 0), findTestEntry("beta.go", findKindFile, 0)}})
	model, first := findUpdate(t, model, tea.KeyPressMsg{Code: 'a', Text: "a"})
	model, second := findUpdate(t, model, tea.KeyPressMsg{Code: 'l', Text: "l"})
	stale := first()
	model, _ = findUpdate(t, model, second())
	model, _ = findUpdate(t, model, stale)
	if got := findShown(model); !reflect.DeepEqual(got, []string{"alpha.go"}) {
		t.Errorf("a late result for an old query must not replace the new one: %v", got)
	}
}

func TestFindModelNarrowsInsideThePreviousResult(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("nginx.conf", findKindFile, 0), findTestEntry("nginx.log", findKindFile, 0)}})
	model = findType(t, model, "ng")
	// 앞 결과를 하나로 줄여 두면, 늘린 질의는 그 안에서만 찾는다.
	model.all = model.all[:1]
	model = findType(t, model, "x")
	if model.total != 1 {
		t.Errorf("total = %d, all = %v", model.total, model.all)
	}
	// 앞 질의를 늘린 것이 아니면 전부 다시 찾는다.
	model, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	model, _ = findUpdate(t, model, cmd())
	model = findType(t, model, "log")
	if got := findShown(model); !reflect.DeepEqual(got, []string{"nginx.log"}) {
		t.Errorf("shown = %v", got)
	}
}

func TestFindModelKeys(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("a.txt", findKindFile, 0), findTestEntry("src", findKindDir, time.Minute)}})
	model, _ = findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyDown})
	done, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if done.chosen != "src" || cmd == nil {
		t.Errorf("enter: chosen %q", done.chosen)
	}
	cancelled, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyEscape})
	if !cancelled.cancelled || cmd == nil || cancelled.chosen != "" {
		t.Error("esc must cancel")
	}
	// 디렉터리는 edc view로 열지 않는다.
	viewed, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	if cmd != nil || viewed.status != T("find.picker.view_dir") {
		t.Errorf("view dir: %q", viewed.status)
	}
	command, _ := model.editCommand("src", findKindDir)
	if !reflect.DeepEqual(command.Args, []string{"code", "-w", "src"}) {
		t.Errorf("editor = %v", command.Args)
	}
	if _, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}); cmd == nil {
		t.Error("ctrl+o must run the editor")
	}
	grep, _ := findUpdate(t, model, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	grep = findType(t, grep, "time out")
	if !grep.grepMode || grep.grepPattern != "time out" || grep.query != "" {
		t.Fatalf("grep mode: %+v", grep.grepPattern)
	}
	command, _ = grep.grepCommand("src", findKindDir)
	if command.Args[0] != "/bin/sh" || !reflect.DeepEqual(command.Args[3:], []string{"/usr/local/bin/edc", "time out", "src"}) {
		t.Errorf("grep = %q", command.Args)
	}
	if _, cmd := findUpdate(t, grep, tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil {
		t.Error("enter in grep mode must run grep")
	}
	back, _ := findUpdate(t, grep, tea.KeyPressMsg{Code: tea.KeyEscape})
	if back.grepMode || back.cancelled {
		t.Error("esc in grep mode goes back to the list")
	}
}

func TestFindModelDoesNotOpenInsideACommandSubstitution(t *testing.T) {
	model := testFindModel(false)
	model.config.stdoutTerminal = false
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("a.txt", findKindFile, 0)}})
	for _, code := range []rune{'o', 'v', 'g'} {
		next, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl})
		if cmd != nil || next.grepMode || next.status != T("find.picker.open_pipe") {
			t.Errorf("ctrl+%c: status %q", code, next.status)
		}
	}
	if !strings.Contains(model.hintLine(), "enter") || strings.Contains(model.hintLine(), "ctrl+o") {
		t.Errorf("hint = %q", model.hintLine())
	}
}

func TestFindModelKeepsResultsOnABrokenRegex(t *testing.T) {
	model := testFindModel(false)
	model.options.regex = true
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("a.go", findKindFile, 0), findTestEntry("b.md", findKindFile, 0)}})
	model = findType(t, model, `\.go`)
	model, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: '(', Text: "("})
	if cmd != nil || model.problem == "" || !reflect.DeepEqual(findShown(model), []string{"a.go"}) {
		t.Errorf("problem %q, shown %v", model.problem, findShown(model))
	}
}

func TestFindModelStopsAtTheLimit(t *testing.T) {
	model := testFindModel(false)
	model.limit = 2
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("a", findKindFile, 0), findTestEntry("b", findKindFile, 0), findTestEntry("c", findKindFile, 0)}})
	if len(model.paths) != 2 || !model.limited || !strings.Contains(model.statusLine(), "2") {
		t.Errorf("paths %v, limited %v, status %q", model.paths, model.limited, model.statusLine())
	}
}

func TestFindModelLinesFitTheWidth(t *testing.T) {
	model := testFindModel(false)
	model.config.color = true
	model, _ = findUpdate(t, model, tea.WindowSizeMsg{Width: 40, Height: 8})
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{
		findTestEntry("very/long/directory/name/that/does/not/fit/into/forty/columns/audit_journal.go", findKindFile, 3*time.Hour),
		findTestEntry("한글/디렉터리/이름이/길어서/잘리는/경우의/파일.txt", findKindFile, 0),
		findTestEntry("a\x1bb", findKindLink, 0),
	}})
	model = findType(t, model, "jrn")
	model, cmd := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyBackspace})
	model, _ = findUpdate(t, model, cmd())
	model.report = findWalkReport{Skipped: []findSkip{{Path: "/proc", Reason: findSkipPseudo}}, Stalled: []string{"/mnt/nfs"}, Partial: true}
	view := model.View()
	lines := strings.Split(view.Content, "\n")
	if len(lines) != 8 || !view.AltScreen {
		t.Fatalf("lines = %d", len(lines))
	}
	for _, line := range lines {
		if width := ansi.StringWidth(line); width > 40 {
			t.Errorf("width %d: %q", width, line)
		}
		if strings.Contains(ansi.Strip(line), "\x1b") {
			t.Errorf("control byte: %q", line)
		}
	}
	plain := ansi.Strip(view.Content)
	if !strings.Contains(plain, "…") || !strings.Contains(plain, "audit_journ") || !strings.Contains(plain, "3h") || !strings.Contains(plain, "/mnt/nfs") {
		t.Errorf("view = %s", plain)
	}
}

func TestFindTopHitsMatchesAFullSort(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	paths := make([]string, 5000)
	hits := make([]findHit, len(paths))
	for index := range paths {
		paths[index] = strings.Repeat("x", random.Intn(30))
		hits[index] = findHit{index: int32(index), score: int32(random.Intn(50))}
	}
	less := findHitLess(true, paths)
	full := slices.Clone(hits)
	slices.SortStableFunc(full, func(a, b findHit) int { return findCompare(less, a, b) })
	top := findTopHits(hits, 100, less)
	for position := range top {
		if findCompare(less, top[position], full[position]) != 0 {
			t.Fatalf("position %d: %+v vs %+v", position, top[position], full[position])
		}
	}
}

func TestFindAge(t *testing.T) {
	for elapsed, want := range map[time.Duration]string{
		10 * time.Second: "<1m", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 47 * time.Hour: "47h", 3 * 24 * time.Hour: "3d",
		90 * 24 * time.Hour: "3mo", 800 * 24 * time.Hour: "2y",
	} {
		if got := findAge(elapsed); got != want {
			t.Errorf("%v: %q", elapsed, got)
		}
	}
}

// 글자를 치고 바로 Enter를 누르면, 그 글자로 계산한 결과의 맨 위를 골라야 한다.
func TestFindModelEnterWaitsForThePendingSearch(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("internal/edc", findKindDir, 0), findTestEntry("internal/edc/locale", findKindDir, time.Hour)}})
	model, search := findUpdate(t, model, tea.KeyPressMsg{Code: 'l', Text: "l"})
	model, quit := findUpdate(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
	if quit != nil || model.chosen != "" || !model.accept {
		t.Fatalf("enter must wait: chosen %q", model.chosen)
	}
	model, quit = findUpdate(t, model, search())
	if model.chosen != "internal/edc/locale" || quit == nil {
		t.Errorf("chosen %q", model.chosen)
	}
}

// ctrl+g를 누른 뒤 계산이 끝나 선택이 바뀌어도 grep 대상은 누를 때 고른 경로다.
func TestFindModelKeepsTheGrepTarget(t *testing.T) {
	model := testFindModel(false)
	model, _ = findUpdate(t, model, findBatchMsg{entries: []findEntry{findTestEntry("old.log", findKindFile, 0), findTestEntry("app.log", findKindFile, time.Hour)}})
	model, search := findUpdate(t, model, tea.KeyPressMsg{Code: 'a', Text: "a"})
	model, _ = findUpdate(t, model, tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	model, _ = findUpdate(t, model, search())
	if model.grepPath != "old.log" || !strings.Contains(ansi.Strip(model.promptLine()), "old.log") {
		t.Errorf("grep path %q, prompt %q", model.grepPath, model.promptLine())
	}
}
