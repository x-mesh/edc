package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cleanProgressContextKey struct{}

type cleanProgressTask struct {
	stage, target string
	started       time.Time
	finished      time.Time
	visible       bool
	skipped       bool
}

type cleanProgress struct {
	mutex      sync.Mutex
	writer     io.Writer
	home       string
	redact     bool
	started    time.Time
	now        func() time.Time
	next       uint64
	tasks      map[uint64]cleanProgressTask
	stages     map[string]cleanProgressTask
	lastDetail time.Time
	cancelled  func() bool
}

func newCleanProgress(writer io.Writer, home string, redact bool) *cleanProgress {
	return &cleanProgress{writer: writer, home: home, redact: redact, started: time.Now(), now: time.Now, tasks: map[uint64]cleanProgressTask{}, stages: map[string]cleanProgressTask{}}
}

func (progress *cleanProgress) line(state string, task cleanProgressTask, now time.Time) {
	target := progress.targetText(task.target)
	fmt.Fprintf(progress.writer, "[%6.1fs] %-5s %s", now.Sub(progress.started).Seconds(), state, T("clean.progress.stage."+task.stage))
	if target != "" {
		fmt.Fprintf(progress.writer, "  %s", target)
	}
	if state != "START" {
		fmt.Fprintf(progress.writer, "  (%s)", now.Sub(task.started).Round(10*time.Millisecond))
	}
	fmt.Fprintln(progress.writer)
}

func (progress *cleanProgress) begin(stage, target string) func() {
	progress.mutex.Lock()
	now := progress.now()
	progress.next++
	id := progress.next
	detail := stage == "tool" || stage == "measure"
	visible := !detail || now.Sub(progress.lastDetail) >= 2*time.Second
	task := cleanProgressTask{stage: stage, target: target, started: now, visible: visible}
	if progress.cancelled != nil {
		task.skipped = progress.cancelled()
	}
	progress.tasks[id] = task
	if cleanProgressStage(stage) {
		progress.stages[stage] = task
	}
	if visible {
		progress.line("START", task, now)
		if detail {
			progress.lastDetail = now
		}
	}
	progress.mutex.Unlock()
	return func() {
		progress.mutex.Lock()
		defer progress.mutex.Unlock()
		now := progress.now()
		delete(progress.tasks, id)
		if cleanProgressStage(stage) {
			task.finished = now
			progress.stages[stage] = task
		}
		if task.visible || now.Sub(task.started) >= time.Second {
			progress.line("END", task, now)
		}
	}
}

func (progress *cleanProgress) heartbeat() {
	progress.mutex.Lock()
	defer progress.mutex.Unlock()
	var newest uint64
	for id := range progress.tasks {
		if id > newest {
			newest = id
		}
	}
	if newest != 0 {
		progress.line("RUN", progress.tasks[newest], progress.now())
	}
}

func (progress *cleanProgress) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			progress.heartbeat()
		}
	}
}

func (deps cleanDeps) begin(stage, target string) func() {
	if deps.progress == nil {
		return func() {}
	}
	return deps.progress(stage, target)
}

func cleanStep[T any](deps cleanDeps, stage, target string, run func() T) T {
	done := deps.begin(stage, target)
	defer done()
	return run()
}

func cleanProgressDeps(deps cleanDeps, progress *cleanProgress) cleanDeps {
	deps.progress = progress.begin
	run, measure := deps.run, deps.measure
	deps.run = func(ctx context.Context, spec cleanToolSpec) cleanToolOutput {
		args := spec.Args
		if spec.Name == "git" && len(args) >= 2 && args[0] == "-c" {
			args = args[2:]
		}
		name := spec.Name
		if len(args) > 0 {
			name += " " + args[0]
		}
		return cleanStep(deps, "tool", name+" · "+spec.Dir, func() cleanToolOutput { return run(ctx, spec) })
	}
	deps.measure = func(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
		done := deps.begin("measure", options.root)
		defer done()
		return measure(ctx, options, sources)
	}
	return deps
}

var cleanProgressStages = []string{"caches", "projects", "docker", "docker_root", "system", "vm", "worktrees", "newest"}

func cleanProgressStage(stage string) bool {
	for _, name := range cleanProgressStages {
		if name == stage {
			return true
		}
	}
	return false
}

func (progress *cleanProgress) targetText(target string) string {
	if progress.home != "" {
		target = strings.ReplaceAll(target, progress.home, "~")
	}
	target = historyTerminalText(target)
	if progress.redact {
		target = redactIPAddresses(target)
	}
	return target
}

type cleanProgressSnapshot struct {
	at, started time.Time
	stages      map[string]cleanProgressTask
	active      []cleanProgressTask
}

func (progress *cleanProgress) snapshot() cleanProgressSnapshot {
	progress.mutex.Lock()
	defer progress.mutex.Unlock()
	snapshot := cleanProgressSnapshot{at: progress.now(), started: progress.started, stages: make(map[string]cleanProgressTask, len(progress.stages))}
	for stage, task := range progress.stages {
		snapshot.stages[stage] = task
	}
	ids := make([]uint64, 0, len(progress.tasks))
	for id := range progress.tasks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		task := progress.tasks[id]
		if task.stage == "total" || cleanProgressStage(task.stage) {
			continue
		}
		task.target = progress.targetText(task.target)
		snapshot.active = append(snapshot.active, task)
	}
	return snapshot
}

// 응답 없음 기준은 끝난 시스템 호출이 없는 시간이다. 큰 디렉터리는 오래 걸려도 호출이 계속 끝나므로 기준에 걸리지 않는다.
const (
	cleanStallHint  = 3 * time.Second
	cleanStallLimit = 10 * time.Second
)

var errCleanStalled = errors.New("no response from the file system")

// cleanStalls는 응답이 없어 건너뛴 경로다. 측정은 여러 goroutine에서 끝나므로 잠근다.
type cleanStalls struct {
	mutex      sync.Mutex
	listMounts func() ([]diskMount, error)
	mounts     []diskMount
	loaded     bool
	local      []string
	// remote는 응답하지 않은 네트워크·FUSE 마운트다. 그 아래 다른 경로도 같은 서버를 기다리므로 다시 기다리지 않는다.
	remote []cleanStalledMount
}

type cleanStalledMount struct {
	mount diskMount
	paths int
}

func (stalls *cleanStalls) add(path string) {
	stalls.mutex.Lock()
	defer stalls.mutex.Unlock()
	if !stalls.loaded && stalls.listMounts != nil {
		stalls.mounts, _ = stalls.listMounts()
	}
	stalls.loaded = true
	mount, ok := findDiskMount(stalls.mounts, path)
	if !ok || !cleanRemoteFSType(mount.FSType) {
		stalls.local = append(stalls.local, path)
		return
	}
	for index := range stalls.remote {
		if stalls.remote[index].mount.Point == mount.Point {
			stalls.remote[index].paths++
			return
		}
	}
	stalls.remote = append(stalls.remote, cleanStalledMount{mount: mount, paths: 1})
}

// skip은 path가 이미 응답하지 않은 마운트 아래에 있으면 건너뛴 경로로 세고 true를 돌려준다.
func (stalls *cleanStalls) skip(path string) bool {
	stalls.mutex.Lock()
	defer stalls.mutex.Unlock()
	for index := range stalls.remote {
		if pathUnder(path, stalls.remote[index].mount.Point) {
			stalls.remote[index].paths++
			return true
		}
	}
	return false
}

// warnings는 로컬 경로마다, 네트워크·FUSE 마운트는 마운트마다 경고를 만든다. Linux에서는 그 마운트에 응답을 기다리는
// 스레드가 SIGKILL로도 끝나지 않아 보고서를 낸 뒤에도 명령이 끝나지 않을 수 있으므로 마운트를 떼는 방법을 덧붙인다.
func (stalls *cleanStalls) warnings(limit time.Duration) []string {
	stalls.mutex.Lock()
	defer stalls.mutex.Unlock()
	warnings := make([]string, 0, len(stalls.local)+len(stalls.remote))
	for _, path := range stalls.local {
		warnings = append(warnings, T("clean.warning.stalled", path, limit))
	}
	for _, stalled := range stalls.remote {
		warnings = append(warnings, T("clean.warning.stalled_mount", stalled.mount.Point, stalled.mount.FSType, limit, stalled.paths))
	}
	return warnings
}

// cleanRemoteFSType은 다른 프로세스나 다른 호스트가 응답해야 하는 파일 시스템이다.
func cleanRemoteFSType(fstype string) bool {
	for _, prefix := range []string{"fuse", "macfuse", "osxfuse", "nfs"} {
		if strings.HasPrefix(fstype, prefix) {
			return true
		}
	}
	switch fstype {
	case "cifs", "smb3", "smbfs", "afpfs", "webdav", "9p", "ceph", "glusterfs", "lustre":
		return true
	}
	return false
}

// cleanResolveGuard는 측정 전에 경로를 확인하는 호출을 limit까지만 기다린다.
func cleanResolveGuard(resolve func(string) (string, error), begin func(stage, target string) func(), stalls *cleanStalls,
	hint, limit time.Duration) func(string) (string, error) {
	return func(path string) (string, error) {
		return cleanWaitFor(path, begin, stalls, hint, limit, func() (string, error) { return resolve(path) })
	}
}

// cleanReadDirGuard는 빌드 폴더를 찾으며 디렉터리 하나를 읽는 호출을 limit까지만 기다린다.
func cleanReadDirGuard(readDir func(string) ([]os.DirEntry, error), begin func(stage, target string) func(), stalls *cleanStalls,
	hint, limit time.Duration) func(string) ([]os.DirEntry, error) {
	return func(path string) ([]os.DirEntry, error) {
		return cleanWaitFor(path, begin, stalls, hint, limit, func() ([]os.DirEntry, error) { return readDir(path) })
	}
}

// cleanCallGuard는 값을 돌려주지 않는 짧은 호출을 limit까지만 기다린다.
func cleanCallGuard(begin func(stage, target string) func(), stalls *cleanStalls, hint, limit time.Duration) func(string, func() error) error {
	return func(path string, call func() error) error {
		_, err := cleanWaitFor(path, begin, stalls, hint, limit, func() (struct{}, error) { return struct{}{}, call() })
		return err
	}
}

// cleanWaitFor는 path를 건드리는 짧은 호출 하나를 limit까지만 기다린다. 시스템 호출 몇 번뿐이라 진행 카운터 없이
// 시간으로 본다. 돌아오지 않은 호출의 goroutine은 남겨 둔다.
func cleanWaitFor[T any](path string, begin func(stage, target string) func(), stalls *cleanStalls, hint, limit time.Duration,
	call func() (T, error)) (T, error) {
	var zero T
	if stalls.skip(path) {
		return zero, errCleanStalled
	}
	type outcome struct {
		value T
		err   error
	}
	finished := make(chan outcome, 1)
	go func() {
		value, err := call()
		finished <- outcome{value: value, err: err}
	}()
	hinted, stalled := time.NewTimer(hint), time.NewTimer(limit)
	defer hinted.Stop()
	defer stalled.Stop()
	var waiting func()
	defer func() {
		if waiting != nil {
			waiting()
		}
	}()
	for {
		select {
		case got := <-finished:
			return got.value, got.err
		case <-hinted.C:
			if begin != nil {
				waiting = begin("stalled", path)
			}
		case <-stalled.C:
			stalls.add(path)
			return zero, errCleanStalled
		}
	}
}

// cleanStallGuard는 측정 하나가 커널 안에서 돌아오지 않아도 clean 전체가 기다리지 않게 한다. macOS는 권한 창에
// 답할 때까지 open을 돌려주지 않고, 멈춘 네트워크 마운트도 그렇다. 취소는 커널 안의 호출을 풀지 못하므로 그 goroutine은
// 남겨 두고 경로를 건너뛴다. begin은 hint가 지나면 기다리는 경로를 진행 화면에 띄운다.
func cleanStallGuard(measure func(context.Context, duOptions, duSources) (duScan, error), begin func(stage, target string) func(),
	stalls *cleanStalls, hint, limit time.Duration) func(context.Context, duOptions, duSources) (duScan, error) {
	return func(ctx context.Context, options duOptions, sources duSources) (duScan, error) {
		if stalls.skip(options.root) {
			return duScan{}, errCleanStalled
		}
		if options.activity == nil {
			options.activity = new(atomic.Int64)
		}
		inner, abandon := context.WithCancel(ctx)
		defer abandon()
		type outcome struct {
			scan duScan
			err  error
		}
		finished := make(chan outcome, 1)
		go func() {
			scan, err := measure(inner, options, sources)
			finished <- outcome{scan: scan, err: err}
		}()
		ticker := time.NewTicker(min(hint, limit) / 10)
		defer ticker.Stop()
		seen, changed := options.activity.Load(), time.Now()
		var waiting func()
		defer func() {
			if waiting != nil {
				waiting()
			}
		}()
		for {
			select {
			case got := <-finished:
				return got.scan, got.err
			case now := <-ticker.C:
				if current := options.activity.Load(); current != seen {
					seen, changed = current, now
					if waiting != nil {
						waiting()
						waiting = nil
					}
					continue
				}
				idle := now.Sub(changed)
				if idle >= limit {
					stalls.add(options.root)
					return duScan{}, errCleanStalled
				}
				if idle >= hint && waiting == nil && begin != nil {
					waiting = begin("stalled", options.root)
				}
			}
		}
	}
}
