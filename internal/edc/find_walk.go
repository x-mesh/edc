package edc

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

type findKind byte

const (
	findKindFile  findKind = 'f'
	findKindDir   findKind = 'd'
	findKindLink  findKind = 'l'
	findKindOther findKind = 'o'
)

// findEntry는 후보 하나다. size와 mtime은 stat한 때만 채운다(statted).
type findEntry struct {
	path    string
	kind    findKind
	statted bool
	size    int64
	mtime   int64
}

// findSkip은 순회하지 않은 디렉터리와 그 이유다.
type findSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	FSType string `json:"fstype,omitempty"`
}

const (
	findSkipMount  = "mount"
	findSkipPseudo = "pseudo"
	findSkipLayer  = "layer"
)

// findPseudoFSTypes는 커널이 만드는 파일 시스템이다. 디스크의 파일이 아니고, /proc는 process마다 같은 이름을 되풀이하므로
// --cross-mounts에서도 들어가지 않는다.
var findPseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "cgroup": true, "cgroup2": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "bpf": true, "pstore": true, "mqueue": true, "configfs": true, "fusectl": true,
	"binfmt_misc": true, "autofs": true, "efivarfs": true, "hugetlbfs": true, "rpc_pipefs": true, "nsfs": true, "devfs": true,
}

// findPseudoPaths는 마운트 표를 읽지 못할 때 이름으로 빼는 경로다. 표가 있으면 쓰지 않는다.
var findPseudoPaths = []string{"/proc", "/sys", "/dev", "/run"}

// findLayerPaths는 컨테이너 이미지 층이 풀린 곳이다. 이미지마다 /etc와 /usr의 사본이 있어, 호스트의 파일보다 위에 나온다.
// 컨테이너의 로그(/var/lib/docker/containers)는 호스트에서 찾는 대상이라 넣지 않는다.
var findLayerPaths = []string{
	"/var/lib/docker/overlay2", "/var/lib/docker/image",
	"/var/lib/containers/storage/overlay", "/var/lib/containers/storage/overlay-images",
}

// findLayerPrefix는 containerd의 snapshotter 디렉터리 이름 앞부분이다. 뒤에 overlayfs, native 같은 이름이 붙는다.
const findLayerPrefix = "/var/lib/containerd/io.containerd.snapshotter.v1."

type findWalkOptions struct {
	roots       []string
	noIgnore    bool
	crossMounts bool
	layers      bool
	workers     int
	stat        bool
	// stall은 남은 일이 응답 없는 디렉터리뿐일 때 기다리는 시간이다.
	stall time.Duration
}

type findSources struct {
	mounts func() ([]diskMount, error)
	// pseudoPaths, layerPaths, layerPrefix는 호스트의 절대 경로다. 시험은 임시 디렉터리 아래의 경로로 바꾼다.
	pseudoPaths, layerPaths []string
	layerPrefix             string
	// beforeRead는 디렉터리를 열거나 목록을 읽기 직전에 부른다. 시험이 돌아오지 않는 시스템 호출을 여기서 흉내 낸다.
	beforeRead func(path string)
}

func hostFindSources() findSources {
	return findSources{mounts: duHostMounts, pseudoPaths: findPseudoPaths, layerPaths: findLayerPaths, layerPrefix: findLayerPrefix,
		beforeRead: func(string) {}}
}

// findUnreadableShown은 읽지 못한 디렉터리를 이름으로 남기는 수다. 나머지는 개수만 센다.
const findUnreadableShown = 100

type findWalkReport struct {
	Skipped         []findSkip
	Unreadable      []string
	UnreadableCount int
	Stalled         []string
	// Partial은 시간이 다 됐거나 응답 없는 디렉터리 때문에 끝까지 읽지 못했다는 뜻이다. Stopped는 받는 쪽이 그만 받겠다고 한 것이다.
	Partial, Stopped bool
	MountErr         error
	Problems         []string
	Directories      int
}

// findRoot는 시작 경로 하나의 순회 설정이다.
type findRoot struct {
	display string
	mounts  map[string]diskMount
	pseudo  map[string]diskMount
	layers  bool
	ignore  bool
}

type findDir struct {
	root     *findRoot
	abs      string
	display  string
	parts    []string
	patterns []gitignore.Pattern
}

// findArena는 worker 하나가 경로를 이어 붙이는 버퍼다. 후보마다 문자열을 따로 만들면 / 전체에서 수십만 개의 작은 객체가 생긴다.
// strings.Builder는 이미 쓴 바이트를 바꾸지 않으므로, 그 위의 부분 문자열은 버퍼가 다시 커져도 그대로다.
type findArena struct{ builder strings.Builder }

const findArenaChunk = 1 << 20

func (arena *findArena) join(dir, name string) string {
	size := len(dir) + 1 + len(name)
	if dir == "" {
		size = len(name)
	}
	if arena.builder.Cap()-arena.builder.Len() < size {
		arena.builder = strings.Builder{}
		arena.builder.Grow(max(findArenaChunk, size))
	}
	start := arena.builder.Len()
	if dir != "" {
		arena.builder.WriteString(dir)
		if !strings.HasSuffix(dir, "/") {
			arena.builder.WriteByte('/')
		}
	}
	arena.builder.WriteString(name)
	return arena.builder.String()[start:]
}

// findQueue는 읽을 디렉터리다. 뒤에서 꺼내 깊이 우선으로 읽어, 넓은 트리에서도 기다리는 디렉터리 수가 덜 쌓인다.
type findQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	dirs    []findDir
	pending int
	closed  bool
}

func newFindQueue() *findQueue {
	queue := &findQueue{}
	queue.cond = sync.NewCond(&queue.mu)
	return queue
}

func (queue *findQueue) push(dirs ...findDir) {
	queue.mu.Lock()
	queue.dirs = append(queue.dirs, dirs...)
	queue.pending += len(dirs)
	queue.mu.Unlock()
	queue.cond.Broadcast()
}

func (queue *findQueue) pop() (findDir, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for len(queue.dirs) == 0 && !queue.closed {
		queue.cond.Wait()
	}
	if len(queue.dirs) == 0 {
		return findDir{}, false
	}
	dir := queue.dirs[len(queue.dirs)-1]
	queue.dirs = queue.dirs[:len(queue.dirs)-1]
	return dir, true
}

// done은 디렉터리 하나를 다 읽었다는 뜻이다. 남은 것이 없으면 queue를 닫아 worker를 끝낸다.
func (queue *findQueue) done() {
	queue.mu.Lock()
	queue.pending--
	last := queue.pending == 0
	if last {
		queue.closed = true
	}
	queue.mu.Unlock()
	if last {
		queue.cond.Broadcast()
	}
}

func (queue *findQueue) close() {
	queue.mu.Lock()
	queue.closed = true
	queue.mu.Unlock()
	queue.cond.Broadcast()
}

// unfinished는 넣었지만 아직 다 읽지 않은 디렉터리 수다. 기다리는 것과 읽는 중인 것을 모두 센다.
func (queue *findQueue) unfinished() int {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.pending
}

// findBusy는 worker 하나가 읽는 디렉터리와 마지막으로 진척이 있던 시각이다. path는 Skipped, Unreadable처럼 사용자가 준 경로 모양이다.
type findBusy struct {
	path     string
	progress time.Time
}

type findWalker struct {
	ctx     context.Context
	options findWalkOptions
	sources findSources
	queue   *findQueue
	results chan []findEntry
	quit    chan struct{}

	mu              sync.Mutex
	busy            map[int]findBusy
	skipped         []findSkip
	unreadable      []string
	unreadableCount int
	directories     int
}

// findWalk는 시작 경로들을 읽고 후보를 묶음으로 emit에 넘긴다. emit은 이 goroutine에서만 불리고, false를 돌려주면 순회를 멈춘다.
// 응답 없는 디렉터리를 읽는 goroutine은 기다리지 않고 남겨 둔다. 그 goroutine은 깨어나도 quit을 보고 아무것도 보내지 않는다.
func findWalk(ctx context.Context, options findWalkOptions, sources findSources, emit func([]findEntry) bool) findWalkReport {
	if options.workers <= 0 {
		options.workers = 1
	}
	if sources.beforeRead == nil {
		sources.beforeRead = func(string) {}
	}
	walker := &findWalker{
		ctx: ctx, options: options, sources: sources, queue: newFindQueue(),
		results: make(chan []findEntry, options.workers*2), quit: make(chan struct{}), busy: map[int]findBusy{},
	}
	var report findWalkReport
	var mounts []diskMount
	if sources.mounts != nil {
		mounts, report.MountErr = sources.mounts()
	} else {
		report.MountErr = errors.ErrUnsupported
	}
	var roots []findDir
	for _, root := range options.roots {
		dir, entry, err := walker.prepareRoot(root, mounts, report.MountErr)
		switch {
		case err != nil:
			report.Problems = append(report.Problems, err.Error())
		case entry != nil:
			// 파일을 시작 경로로 주면 그 파일 하나가 후보다.
			if !emit([]findEntry{*entry}) {
				report.Stopped = true
				return report
			}
		default:
			roots = append(roots, dir)
		}
	}
	if len(roots) == 0 {
		return report
	}
	walker.queue.push(roots...)
	var group sync.WaitGroup
	for worker := range options.workers {
		group.Go(func() { walker.work(worker) })
	}
	finished := make(chan struct{})
	go func() {
		group.Wait()
		close(finished)
	}()
	interval := 250 * time.Millisecond
	if quarter := options.stall / 4; options.stall > 0 && quarter < interval {
		interval = quarter
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
	}
	stall := time.NewTicker(interval)
	defer stall.Stop()
	stop := func() {
		close(walker.quit)
		walker.queue.close()
	}
loop:
	for {
		select {
		case batch := <-walker.results:
			if !emit(batch) {
				report.Stopped = true
				stop()
				break loop
			}
		case <-finished:
			// worker가 모두 끝났으므로 남은 묶음은 channel에 있는 것뿐이다.
			for {
				select {
				case batch := <-walker.results:
					if !emit(batch) {
						report.Stopped = true
						close(walker.quit)
						break loop
					}
				default:
					break loop
				}
			}
		case <-ctx.Done():
			report.Partial = true
			stop()
			report.Stopped = !walker.drain(emit)
			break loop
		case <-stall.C:
			if stalled := walker.stalled(); stalled != nil {
				report.Partial, report.Stalled = true, stalled
				stop()
				report.Stopped = !walker.drain(emit)
				break loop
			}
		}
	}
	walker.mu.Lock()
	defer walker.mu.Unlock()
	report.Skipped = slices.Clone(walker.skipped)
	slices.SortFunc(report.Skipped, func(a, b findSkip) int { return strings.Compare(a.Path, b.Path) })
	report.Unreadable = slices.Clone(walker.unreadable)
	slices.Sort(report.Unreadable)
	report.UnreadableCount = walker.unreadableCount
	report.Directories = walker.directories
	return report
}

// drain은 멈춘 뒤 channel에 남은 묶음을 넘긴다. 이미 읽은 후보를 버리지 않는다. emit이 false를 돌려주면 false다.
func (walker *findWalker) drain(emit func([]findEntry) bool) bool {
	for {
		select {
		case batch := <-walker.results:
			if !emit(batch) {
				return false
			}
		default:
			return true
		}
	}
}

// stalled는 남은 일이 모두 options.stall보다 오래 진척이 없는 디렉터리뿐인지 본다. 그러면 그 경로들을 돌려준다.
// 남은 디렉터리 수(pending)를 먼저 읽는다. 읽기를 마친 worker는 busy에서 빠진 뒤에도 하위 디렉터리를 넣고 결과를 보낼 때까지
// pending에 남으므로, pending이 오래 멈춘 busy 수와 같을 때만 응답 없는 디렉터리만 남은 것이다.
func (walker *findWalker) stalled() []string {
	if walker.options.stall <= 0 {
		return nil
	}
	pending := walker.queue.unfinished()
	walker.mu.Lock()
	defer walker.mu.Unlock()
	if len(walker.busy) == 0 || pending != len(walker.busy) {
		return nil
	}
	var paths []string
	for _, busy := range walker.busy {
		if time.Since(busy.progress) < walker.options.stall {
			return nil
		}
		paths = append(paths, busy.path)
	}
	slices.Sort(paths)
	return paths
}

func (walker *findWalker) prepareRoot(root string, mounts []diskMount, mountErr error) (findDir, *findEntry, error) {
	info, err := os.Stat(root)
	if err != nil {
		return findDir{}, nil, err
	}
	display := root
	if display != "/" {
		display = filepath.Clean(root)
	}
	if display == "." {
		display = ""
	}
	if !info.IsDir() {
		entry := findEntry{path: root, kind: findKindOf(info.Mode())}
		if walker.options.stat {
			entry.statted, entry.size, entry.mtime = true, info.Size(), info.ModTime().UnixNano()
		}
		return findDir{}, &entry, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return findDir{}, nil, err
	}
	// 마운트 표는 실제 경로를 적는다. /var/run이나 macOS의 /tmp처럼 symlink인 시작 경로는 실제 경로로 바꿔야 마운트와 이미지 층을 알아본다.
	// 표시하는 경로는 사용자가 준 모양 그대로다.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	state := &findRoot{display: display, mounts: map[string]diskMount{}, pseudo: map[string]diskMount{}, layers: !walker.options.layers}
	for _, mount := range mounts {
		if mount.Point == abs || !pathUnder(mount.Point, abs) || findSelfBind(mounts, mount) {
			continue
		}
		if findPseudoFSTypes[mount.FSType] {
			state.pseudo[mount.Point] = mount
		} else if !walker.options.crossMounts {
			state.mounts[mount.Point] = mount
		}
	}
	if mountErr != nil {
		for _, path := range walker.sources.pseudoPaths {
			if path != abs && pathUnder(path, abs) {
				state.pseudo[path] = diskMount{Point: path}
			}
		}
	}
	dir := findDir{root: state, abs: abs, display: display}
	if !walker.options.noIgnore {
		if base, ok := findRepositoryBase(abs); ok {
			state.ignore = true
			dir.parts = grepIgnoreDomain(base, abs)
			ancestors := []string{}
			for current := abs; current != base; current = filepath.Dir(current) {
				ancestors = append(ancestors, current)
			}
			ancestors = append(ancestors, base)
			// 시작 디렉터리 자신의 .gitignore는 목록을 읽을 때 연다.
			for index := len(ancestors) - 1; index >= 1; index-- {
				_ = grepReadIgnore(ancestors[index], grepIgnoreDomain(base, ancestors[index]), &dir.patterns)
			}
		}
	}
	return dir, nil, nil
}

// findRepositoryBase는 path를 품은 git 작업 트리의 맨 위다. .git이 디렉터리든 파일(worktree)이든 같다.
func findRepositoryBase(path string) (string, bool) {
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current, true
		}
		if parent := filepath.Dir(current); parent == current {
			return "", false
		}
	}
}

func findKindOf(mode fs.FileMode) findKind {
	switch {
	case mode.IsDir():
		return findKindDir
	case mode&fs.ModeSymlink != 0:
		return findKindLink
	case mode.IsRegular():
		return findKindFile
	}
	return findKindOther
}

// findReadBatch는 디렉터리 목록을 한 번에 읽는 항목 수다. 큰 디렉터리도 묶음마다 진척을 남긴다.
const findReadBatch = 1024

func (walker *findWalker) work(worker int) {
	var arena findArena
	for {
		dir, ok := walker.queue.pop()
		if !ok {
			return
		}
		walker.read(worker, dir, &arena)
		walker.queue.done()
	}
}

func (walker *findWalker) progress(worker int, path string) {
	walker.mu.Lock()
	walker.busy[worker] = findBusy{path: path, progress: time.Now()}
	walker.mu.Unlock()
}

func (walker *findWalker) read(worker int, dir findDir, arena *findArena) {
	select {
	case <-walker.quit:
		return
	default:
	}
	shown := findDisplayPath(dir.display)
	walker.progress(worker, shown)
	// 결과를 보내려고 기다리는 동안은 읽는 중이 아니다. 받는 쪽이 느릴 때 응답 없는 디렉터리로 보지 않도록 먼저 지운다.
	idle := func() {
		walker.mu.Lock()
		delete(walker.busy, worker)
		walker.mu.Unlock()
	}
	defer idle()
	walker.sources.beforeRead(dir.abs)
	file, err := os.Open(dir.abs)
	if err != nil {
		idle()
		walker.markUnreadable(dir.display, err)
		return
	}
	defer file.Close()
	walker.mu.Lock()
	walker.directories++
	walker.mu.Unlock()
	// 목록은 묶음마다 처리하므로 .gitignore가 어느 묶음에 있을지 모른다. 규칙은 저장소 안에서만 쓰므로 그때만 먼저 열어 본다.
	patterns := dir.patterns
	if dir.root.ignore {
		if _, err := os.Lstat(filepath.Join(dir.abs, ".gitignore")); err == nil {
			patterns = slices.Clip(patterns)
			_ = grepReadIgnore(dir.abs, dir.parts, &patterns)
		}
	}
	var matcher gitignore.Matcher
	if dir.root.ignore && len(patterns) > 0 {
		matcher = gitignore.NewMatcher(patterns)
	}
	// 큰 디렉터리도 묶음마다 보낸다. 한 디렉터리를 끝까지 모으면 후보 상한이 늦게 걸리고, 받는 쪽이 멈춰도 끝까지 읽는다.
	for {
		walker.sources.beforeRead(dir.abs)
		batch, err := file.ReadDir(findReadBatch)
		walker.progress(worker, shown)
		found, subdirs := walker.entries(worker, dir, batch, patterns, matcher, arena)
		if len(subdirs) > 0 {
			walker.queue.push(subdirs...)
		}
		if len(found) > 0 {
			idle()
			select {
			case walker.results <- found:
			case <-walker.quit:
				return
			}
			walker.progress(worker, shown)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				walker.markUnreadable(dir.display, err)
			}
			return
		}
	}
}

// entries는 목록 한 묶음을 후보와 읽을 하위 디렉터리로 나눈다.
func (walker *findWalker) entries(worker int, dir findDir, batch []fs.DirEntry, patterns []gitignore.Pattern, matcher gitignore.Matcher, arena *findArena) ([]findEntry, []findDir) {
	shown := findDisplayPath(dir.display)
	found := make([]findEntry, 0, len(batch))
	var subdirs []findDir
	for _, entry := range batch {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		// 파일 시스템이 종류를 주지 않으면(DT_UNKNOWN) os 패키지가 lstat로 채우므로 Type은 비어 있지 않다.
		kind := findKindOf(entry.Type())
		var childParts []string
		if dir.root.ignore {
			childParts = append(slices.Clip(dir.parts), name)
			if matcher != nil && matcher.Match(childParts, kind == findKindDir) {
				continue
			}
		}
		candidate := findEntry{path: arena.join(dir.display, name), kind: kind}
		if walker.options.stat {
			info, err := entry.Info()
			walker.progress(worker, shown)
			if err != nil {
				// 읽는 사이에 지워진 항목은 후보가 아니다.
				continue
			}
			candidate.statted, candidate.size, candidate.mtime = true, info.Size(), info.ModTime().UnixNano()
		}
		found = append(found, candidate)
		if kind != findKindDir {
			continue
		}
		abs := filepath.Join(dir.abs, name)
		if skip, ok := walker.skip(dir.root, abs, candidate.path); ok {
			walker.mu.Lock()
			walker.skipped = append(walker.skipped, skip)
			walker.mu.Unlock()
			continue
		}
		subdirs = append(subdirs, findDir{root: dir.root, abs: abs, display: candidate.path, parts: childParts, patterns: patterns})
	}
	return found, subdirs
}

func (walker *findWalker) skip(root *findRoot, abs, display string) (findSkip, bool) {
	if mount, ok := root.pseudo[abs]; ok {
		return findSkip{Path: display, Reason: findSkipPseudo, FSType: mount.FSType}, true
	}
	if mount, ok := root.mounts[abs]; ok {
		return findSkip{Path: display, Reason: findSkipMount, FSType: mount.FSType}, true
	}
	prefix := walker.sources.layerPrefix
	if root.layers && (slices.Contains(walker.sources.layerPaths, abs) || prefix != "" && strings.HasPrefix(abs, prefix) && !strings.Contains(abs[len(prefix):], "/")) {
		return findSkip{Path: display, Reason: findSkipLayer}, true
	}
	return findSkip{}, false
}

func (walker *findWalker) markUnreadable(display string, err error) {
	// 읽는 사이에 지워진 디렉터리는 읽지 못한 것이 아니다.
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	display = findDisplayPath(display)
	walker.mu.Lock()
	defer walker.mu.Unlock()
	walker.unreadableCount++
	if len(walker.unreadable) < findUnreadableShown {
		walker.unreadable = append(walker.unreadable, display)
	}
}

// findDisplayPath는 알릴 때 쓰는 경로다. 시작 경로 .은 표시 경로가 비어 있다.
func findDisplayPath(display string) string {
	if display == "" {
		return "."
	}
	return display
}
