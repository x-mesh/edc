//go:build linux || darwin

package edc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func duRefreshRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func duRefreshWrite(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func duRefreshAppend(t *testing.T, path string, size int) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
}

func duRefreshScan(t *testing.T, root string, keepFiles int) duScan {
	t.Helper()
	sources, _ := hostDuSources()
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 1, keepFiles: keepFiles}, sources)
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

// duRefreshSizes는 디렉터리 경로마다 total과 count다. 다시 읽은 트리와 새로 탐색한 트리를 견준다.
func duRefreshSizes(node *duNode, path string, into map[string][2]uint64) {
	if !node.dir {
		return
	}
	into[path] = [2]uint64{node.total, uint64(node.count)}
	for _, child := range node.children {
		duRefreshSizes(child, filepath.Join(path, child.name), into)
	}
}

func duRefreshSame(t *testing.T, got duScan, root string) {
	t.Helper()
	want := duRefreshScan(t, root, duBrowseFiles)
	gotSizes, wantSizes := map[string][2]uint64{}, map[string][2]uint64{}
	duRefreshSizes(got.root, root, gotSizes)
	duRefreshSizes(want.root, root, wantSizes)
	for path, size := range wantSizes {
		if gotSizes[path] != size {
			t.Errorf("%s: total/count %v after refresh, %v by a new scan", path, gotSizes[path], size)
		}
	}
	for path := range gotSizes {
		if _, found := wantSizes[path]; !found {
			t.Errorf("%s stayed after refresh but is gone", path)
		}
	}
	if got.directories != want.directories {
		t.Errorf("directories %d after refresh, %d by a new scan", got.directories, want.directories)
	}
}

// 바꾸기 전의 시각을 기준으로 둔다. 파일 시스템의 mtime이 그보다 늦어야 바뀐 디렉터리로 보인다.
func duRefreshSince(t *testing.T) time.Time {
	t.Helper()
	since := time.Now()
	time.Sleep(20 * time.Millisecond)
	return since
}

// 폴링 한 번이 끝난 트리의 합계는 같은 트리를 새로 탐색한 값과 같다.
func TestDuPollMatchesANewScan(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "a", "big"), 128<<10)
	duRefreshWrite(t, filepath.Join(root, "a", "small"), 1)
	duRefreshWrite(t, filepath.Join(root, "b", "c", "file"), 64<<10)
	duRefreshWrite(t, filepath.Join(root, "top"), 8<<10)
	duRefreshAge(t, root)
	scan := duRefreshScan(t, root, duBrowseFiles)
	refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
	since := time.Now()
	duRefreshAppend(t, filepath.Join(root, "a", "big"), 256<<10)
	duRefreshWrite(t, filepath.Join(root, "b", "new", "deep", "file"), 64<<10)
	if err := os.RemoveAll(filepath.Join(root, "b", "c")); err != nil {
		t.Fatal(err)
	}
	duRefreshWrite(t, filepath.Join(root, "added"), 32<<10)
	result := duPoll(context.Background(), scan.walker, scan.root, since, refresher.floor, nil)
	if result.err != nil {
		t.Fatal(result.err)
	}
	refresher.apply(&scan, result)
	duRefreshSame(t, scan, root)
	if refresher.since != result.started || refresher.count != 1 {
		t.Fatalf("since %v count %d", refresher.since, refresher.count)
	}
}

// hard link는 다시 읽어도 한 번만 센다. 링크 둘이 든 디렉터리를 모두 다시 읽어도, 하나만 다시 읽어도 같다.
func TestDuPollCountsAHardLinkOnce(t *testing.T) {
	for _, touch := range [][]string{{"x"}, {"y"}, {"x", "y"}} {
		root := duRefreshRoot(t)
		duRefreshWrite(t, filepath.Join(root, "x", "f"), 64<<10)
		if err := os.MkdirAll(filepath.Join(root, "y"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "x", "f"), filepath.Join(root, "y", "g")); err != nil {
			t.Fatal(err)
		}
		duRefreshAge(t, root)
		scan := duRefreshScan(t, root, duBrowseFiles)
		refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
		since := time.Now()
		for _, dir := range touch {
			duRefreshWrite(t, filepath.Join(root, dir, "new"), 4<<10)
		}
		duRefreshSettle(t, refresher, &scan, since)
		duRefreshSame(t, scan, root)
	}
}

// r은 하위 전체를 다시 읽어, 큰 파일 줄로 남기지 않은 파일이 제자리에서 커진 것도 잡는다. 기준값은 이름으로 옮긴다.
func TestDuRescanFullCatchesASmallFileThatGrewInPlace(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "a", "big"), 256<<10)
	duRefreshWrite(t, filepath.Join(root, "a", "d", "small"), 4<<10)
	scan := duRefreshScan(t, root, 1)
	refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
	duRefreshAppend(t, filepath.Join(root, "a", "d", "small"), 128<<10)
	// 기준을 mtime 여유보다 뒤로 두어, 방금 만든 디렉터리가 바뀐 것으로 보이지 않게 한다.
	polled := duPoll(context.Background(), scan.walker, scan.root, time.Now().Add(duMtimeSlack+time.Second), refresher.floor, nil)
	if len(polled.dirs) != 0 || len(polled.files) != 0 {
		t.Fatalf("a file that grew in place changed no directory: %+v", polled)
	}
	var a *duNode
	for _, child := range scan.root.children {
		if child.name == "a" {
			a = child
		}
	}
	refresher.apply(&scan, duRescanFull(context.Background(), scan.walker, a, filepath.Join(root, "a"), root, refresher.floor))
	duRefreshSame(t, scan, root)
	for _, child := range a.children {
		if child.name == "d" && (!child.known || child.total <= child.base) {
			t.Fatalf("d known %v total %d base %d", child.known, child.total, child.base)
		}
	}
}

func TestParseDuRefresh(t *testing.T) {
	for text, want := range map[string]duRefreshSetting{
		"auto": {}, "0": {off: true}, "off": {off: true}, "10s": {every: 10 * time.Second},
	} {
		if got, err := parseDuRefresh(text); err != nil || got != want {
			t.Errorf("%q: %+v %v", text, got, err)
		}
	}
	for _, text := range []string{"fast", "500ms", "-1s"} {
		if _, err := parseDuRefresh(text); err == nil {
			t.Errorf("%q passed", text)
		}
	}
	if got := (duRefreshSetting{}).interval(20 * time.Millisecond); got != duRefreshMinimum {
		t.Errorf("a short poll waits %v", got)
	}
	if got := (duRefreshSetting{}).interval(time.Second); got != 50*time.Second {
		t.Errorf("a 1s poll waits %v", got)
	}
	if got := (duRefreshSetting{every: 7 * time.Second}).interval(time.Minute); got != 7*time.Second {
		t.Errorf("a fixed interval became %v", got)
	}
}

func TestDuRefresherStartsOffForAPartialTreeAndARemoteFileSystem(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "f"), 1)
	scan := duRefreshScan(t, root, duBrowseFiles)
	if refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now()); !refresher.on {
		t.Fatal("a local tree starts with the refresh off")
	}
	if refresher := newDuRefresher(context.Background(), duRefreshSetting{off: true}, scan, time.Now()); refresher.on || refresher.reason != "" {
		t.Fatalf("--refresh 0: on %v reason %q", refresher.on, refresher.reason)
	}
	partial := scan
	partial.partial = true
	if refresher := newDuRefresher(context.Background(), duRefreshSetting{explicit: true}, partial, time.Now()); refresher.on || refresher.reason == "" {
		t.Fatalf("partial: on %v reason %q", refresher.on, refresher.reason)
	}
	remote := scan
	remote.filesystem.FSType = "nfs4"
	if refresher := newDuRefresher(context.Background(), duRefreshSetting{}, remote, time.Now()); refresher.on || refresher.reason != "nfs4" {
		t.Fatalf("nfs: on %v reason %q", refresher.on, refresher.reason)
	}
	if refresher := newDuRefresher(context.Background(), duRefreshSetting{explicit: true, every: time.Minute}, remote, time.Now()); !refresher.on {
		t.Fatal("an explicit --refresh on nfs stays off")
	}
}

// 화면은 다시 읽은 결과를 옮긴 뒤 행을 다시 정렬하고, 고른 항목에 커서를 둔다. a는 자동 갱신을 끄고 켠다.
func TestDuBrowserAppliesARefreshAndTogglesIt(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "a", "f"), 256<<10)
	duRefreshWrite(t, filepath.Join(root, "b", "f"), 64<<10)
	scan := duRefreshScan(t, root, duBrowseFiles)
	job := &duScanJob{finished: make(chan struct{}), scan: scan}
	close(job.finished)
	started := time.Now()
	model := newDuBrowser(root, new(atomic.Int64), nil, job, started, time.Now, false)
	model.refreshCtx = context.Background()
	next, command := model.Update(duScanDoneMsg{})
	browser := next.(duBrowser)
	if browser.refresh == nil || !browser.refresh.on || command == nil {
		t.Fatal("the browse screen did not start the refresh")
	}
	if rows := browser.rows(); rows[0].node.name != "a" {
		t.Fatalf("first row %q", rows[0].node.name)
	}
	browser = duBrowserPress(t, browser, "down")
	since := duRefreshSince(t)
	duRefreshWrite(t, filepath.Join(root, "b", "grown"), 1<<20)
	next, command = browser.Update(duRefreshDoneMsg{duPoll(context.Background(), scan.walker, scan.root, since, browser.refresh.floor, nil)})
	browser = next.(duBrowser)
	rows := browser.rows()
	if rows[0].node.name != "b" || rows[browser.cursor].node.name != "b" || command == nil {
		t.Fatalf("rows %s cursor %d", duRowNames(rows), browser.cursor)
	}
	if text := browser.rowText(rows[0]); !strings.Contains(text, "+") {
		t.Fatalf("grown row %q", text)
	}
	if footer := browser.footer(); !strings.Contains(footer, T("du.browse.refresh_on", duRefreshMinimum)) {
		t.Fatalf("footer %q", footer)
	}
	browser = duBrowserPress(t, browser, "a")
	if browser.refresh.on || !strings.Contains(browser.footer(), T("du.browse.refresh_off")) {
		t.Fatalf("a: footer %q", browser.footer())
	}
	if _, command := browser.Update(duRefreshTickMsg{browser.refresh.generation - 1}); command != nil {
		t.Fatal("a stale tick started a poll")
	}
}

// 출력 모드의 결과는 한 시점의 값이다. --refresh를 주면 받지 않는다.
func TestDuRejectsRefreshOutsideTheBrowseScreen(t *testing.T) {
	root := duRefreshRoot(t)
	sources, ok := hostDuSources()
	for _, args := range [][]string{{"--refresh", "10s", root}, {"--refresh", "auto", "--json", filepath.Join(t.TempDir(), "out.json"), root}, {"--refresh", "fast", root}} {
		stdout, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		if code := runDuWith(args, "test", sources, ok, stdout); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

// r은 열린 디렉터리를 다시 읽는 Cmd를 낸다. 그 결과를 Update에 넣으면 트리가 새로 탐색한 값과 같아진다. 다시 읽는 중에
// 누른 r은 끝난 뒤에 한 번 더 돈다.
func TestDuBrowserRescansTheOpenDirectory(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "a", "big"), 256<<10)
	duRefreshWrite(t, filepath.Join(root, "a", "d", "small"), 4<<10)
	sources, _ := hostDuSources()
	scan, err := duWalk(context.Background(), duOptions{root: root, workers: 1, rows: 1, keepFiles: 1}, sources)
	if err != nil {
		t.Fatal(err)
	}
	job := &duScanJob{finished: make(chan struct{}), scan: scan}
	close(job.finished)
	model := newDuBrowser(root, new(atomic.Int64), nil, job, time.Now(), time.Now, false)
	model.refreshCtx, model.refreshSetting = context.Background(), duRefreshSetting{off: true}
	next, _ := model.Update(duScanDoneMsg{})
	browser := duBrowserPress(t, next.(duBrowser), "enter")
	if browser.current.name != "a" {
		t.Fatalf("opened %q", browser.current.name)
	}
	duRefreshAppend(t, filepath.Join(root, "a", "d", "small"), 128<<10)
	next, command := browser.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	browser = next.(duBrowser)
	if command == nil || !browser.refresh.running {
		t.Fatal("r started no rescan")
	}
	next, _ = browser.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	browser = next.(duBrowser)
	if browser.refresh.pending != browser.current {
		t.Fatal("r during a rescan was dropped")
	}
	next, again := browser.Update(command())
	browser = next.(duBrowser)
	if again == nil {
		t.Fatal("the pending rescan did not start")
	}
	next, _ = browser.Update(again())
	browser = next.(duBrowser)
	duRefreshSame(t, browser.scan, root)
	if browser.refresh.count != 2 || browser.refresh.on {
		t.Fatalf("count %d on %v", browser.refresh.count, browser.refresh.on)
	}
}

// duRefreshAge는 디렉터리의 mtime을 한 시간 전으로 돌린다. 방금 만든 디렉터리가 mtime 여유 안에 들어 바뀐 것으로 보이면,
// 다시 읽는 순서에 따라 우연히 맞는 값이 나온다.
func duRefreshAge(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// duRefreshSettle은 화면이 하듯 폴링을 되풀이한다. 하드링크를 센 디렉터리가 빠지면 남은 링크가 있는 디렉터리를 다음 폴링이
// 다시 읽는다.
func duRefreshSettle(t *testing.T, refresher *duRefresher, scan *duScan, since time.Time) {
	t.Helper()
	for range 3 {
		refresher.apply(scan, duPoll(context.Background(), scan.walker, scan.root, since, refresher.floor, refresher.takeForced()))
		if len(refresher.forced) == 0 {
			return
		}
	}
	t.Fatal("the refresh did not settle")
}

// 하드링크를 센 쪽의 링크나 디렉터리가 지워져도, 남은 링크는 다른 디렉터리에서 다시 센다.
func TestDuPollRecountsAHardLinkWhoseCounterWentAway(t *testing.T) {
	for _, remove := range []string{"x/f", "y/g", "x", "y"} {
		root := duRefreshRoot(t)
		duRefreshWrite(t, filepath.Join(root, "x", "f"), 64<<10)
		duRefreshWrite(t, filepath.Join(root, "x", "other"), 4<<10)
		duRefreshWrite(t, filepath.Join(root, "y", "other"), 4<<10)
		if err := os.Link(filepath.Join(root, "x", "f"), filepath.Join(root, "y", "g")); err != nil {
			t.Fatal(err)
		}
		duRefreshAge(t, root)
		scan := duRefreshScan(t, root, duBrowseFiles)
		refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
		since := time.Now()
		if err := os.RemoveAll(filepath.Join(root, remove)); err != nil {
			t.Fatal(err)
		}
		duRefreshSettle(t, refresher, &scan, since)
		t.Run(remove, func(t *testing.T) { duRefreshSame(t, scan, root) })
	}
}

// 취소된 폴링은 트리와 디렉터리 수를 건드리지 않는다.
func TestDuPollStoppedByTheScreenChangesNothing(t *testing.T) {
	root := duRefreshRoot(t)
	duRefreshWrite(t, filepath.Join(root, "a", "f"), 64<<10)
	scan := duRefreshScan(t, root, duBrowseFiles)
	refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
	since := duRefreshSince(t)
	duRefreshWrite(t, filepath.Join(root, "a", "new"), 1<<20)
	total, directories := scan.root.total, scan.directories
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := duPoll(ctx, scan.walker, scan.root, since, refresher.floor, nil)
	if result.err == nil {
		t.Fatal("a cancelled poll reported no error")
	}
	refresher.running = true
	refresher.apply(&scan, result)
	if scan.root.total != total || scan.directories != directories || refresher.count != 0 || refresher.running {
		t.Fatalf("total %d→%d directories %d→%d count %d running %v", total, scan.root.total, directories, scan.directories, refresher.count, refresher.running)
	}
}

// duBrowserDrain은 화면이 낸 다시 읽기를 끝까지 돌린다. 자동 갱신이 꺼져 있으면 타이머를 걸지 않으므로 Cmd는 모두 다시
// 읽기다.
func duBrowserDrain(t *testing.T, browser duBrowser, command tea.Cmd) duBrowser {
	t.Helper()
	for range 4 {
		if command == nil || !browser.refresh.running {
			return browser
		}
		next, again := browser.Update(command())
		browser, command = next.(duBrowser), again
	}
	t.Fatal("the rescans did not end")
	return browser
}

// r로 다시 읽을 디렉터리가 그 사이에 지워졌거나 파일로 바뀌었으면 화면에서 빼고, 부모를 다시 읽어 바뀐 자리를 센다.
func TestDuBrowserDropsAnOpenDirectoryThatWasRemoved(t *testing.T) {
	for _, replace := range []bool{false, true} {
		root := duRefreshRoot(t)
		duRefreshWrite(t, filepath.Join(root, "a", "f"), 256<<10)
		duRefreshWrite(t, filepath.Join(root, "b", "f"), 4<<10)
		duRefreshAge(t, root)
		scan := duRefreshScan(t, root, duBrowseFiles)
		job := &duScanJob{finished: make(chan struct{}), scan: scan}
		close(job.finished)
		model := newDuBrowser(root, new(atomic.Int64), nil, job, time.Now(), time.Now, false)
		model.refreshCtx, model.refreshSetting = context.Background(), duRefreshSetting{off: true}
		next, _ := model.Update(duScanDoneMsg{})
		browser := duBrowserPress(t, next.(duBrowser), "enter")
		if browser.current.name != "a" {
			t.Fatalf("opened %q", browser.current.name)
		}
		if err := os.RemoveAll(filepath.Join(root, "a")); err != nil {
			t.Fatal(err)
		}
		if replace {
			duRefreshWrite(t, filepath.Join(root, "a"), 64<<10)
		}
		next, command := browser.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
		browser = duBrowserDrain(t, next.(duBrowser), command)
		if browser.current != browser.scan.root {
			t.Fatalf("replace %v: the screen stayed in %q", replace, browser.current.name)
		}
		duRefreshSame(t, browser.scan, root)
		if browser.scan.root.total != duRefreshScan(t, root, duBrowseFiles).root.total {
			t.Fatalf("replace %v: root total %d", replace, browser.scan.root.total)
		}
	}
}

// 탐색한 디렉터리 자체가 지워지거나 파일로 바뀌면 트리를 비우고 자동 갱신을 끈다. 이전 트리를 그대로 보이면 없는 용량을
// 있는 것처럼 보인다.
func TestDuPollEmptiesARootThatWasRemoved(t *testing.T) {
	for _, replace := range []bool{false, true} {
		parent := duRefreshRoot(t)
		root := filepath.Join(parent, "tree")
		duRefreshWrite(t, filepath.Join(root, "a", "f"), 64<<10)
		scan := duRefreshScan(t, root, duBrowseFiles)
		refresher := newDuRefresher(context.Background(), duRefreshSetting{}, scan, time.Now())
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		if replace {
			duRefreshWrite(t, root, 4<<10)
		}
		refresher.apply(&scan, duPoll(context.Background(), scan.walker, scan.root, time.Now(), refresher.floor, nil))
		if scan.root.total != 0 || scan.root.count != 0 || len(scan.root.children) != 0 || scan.directories != 0 {
			t.Fatalf("replace %v: total %d count %d children %d directories %d", replace, scan.root.total, scan.root.count, len(scan.root.children), scan.directories)
		}
		if refresher.on || refresher.reason != T("du.browse.refresh_removed") {
			t.Fatalf("replace %v: on %v reason %q", replace, refresher.on, refresher.reason)
		}
	}
}

// 트리가 줄었을 때 하단 줄이 증가라고 적지 않는다.
func TestDuRefreshDeltaReadsForAShrinkToo(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko", "ja"} {
		setLanguage(language)
		if text := T("du.browse.refresh_delta", "-1.0 MB", "11:00"); strings.Contains(text, "증가") || strings.Contains(text, "増加") {
			t.Errorf("%s: %q", language, text)
		}
	}
}
