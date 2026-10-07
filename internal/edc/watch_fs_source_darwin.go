//go:build darwin

package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/x-mesh/edc/internal/fsevents"
)

// fsWatchLatency는 FSEvents가 변경을 모아 한 번에 보내는 시간이다. action debounce 기본값 200ms보다 짧다.
const fsWatchLatency = 50 * time.Millisecond

// 삭제된 경로의 상태는 같은 경로의 늦은 이벤트를 다시 알리지 않을 만큼만 둔다. 빌드 디렉터리처럼 이름이 매번
// 다른 파일이 생겼다 사라지는 트리에서 상태가 계속 쌓이지 않게 한다.
const (
	fsWatchGoneTTL        = time.Minute
	fsWatchSweepInterval  = 30 * time.Second
	fsWatchExcludeEntries = 4096
	// fsWatchRescanLimit는 한 번 다시 훑을 항목 수다. 큰 트리를 끝까지 훑으면 그동안 이벤트를 받지 못해
	// 다시 유실이 생긴다. 이 Mac에서 약 112만 항목을 훑는 데 10초 이상 걸렸다.
	fsWatchRescanLimit = 200_000
)

var errFSWatchRescanLimit = errors.New("rescan limit")

// fsWatchPathState는 이벤트가 온 경로를 마지막으로 lstat한 결과다. FSEvents flag는 같은 경로의 이전 변경을
// 계속 달고 오므로, flag만으로는 이미 알린 생성을 다시 알리게 된다.
type fsWatchPathState struct {
	exists bool
	isDir  bool
	inode  uint64
	size   int64
	mtime  time.Time
	birth  time.Time
	gone   time.Time
}

func newFSWatchPathState(info fs.FileInfo) fsWatchPathState {
	state := fsWatchPathState{exists: true, isDir: info.IsDir(), size: info.Size(), mtime: info.ModTime()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		state.inode = stat.Ino
		state.birth = time.Unix(stat.Birthtimespec.Unix())
	}
	return state
}

// fsWatchClassify는 한 FSEvents 이벤트를 edc event로 바꾼다. known이 false면 이 감시에서 처음 보는 경로다.
// 이름이 바뀐 경로는 이전 경로에서 rename, 새 경로에서 create가 된다. 생겼다가 사라진 경로는 create와 remove다.
// FSEvents는 감시를 시작하기 직전의 변경도 보낼 수 있다. 처음 보는 경로는 started 뒤에 생기거나 바뀐 경우만 알린다.
// 이동해 들어온 파일은 생성 시각이 이전이어도 새 경로에서는 지금 생긴 것이다.
func fsWatchClassify(flags uint32, previous fsWatchPathState, known bool, current fsWatchPathState, started time.Time) []string {
	movedIn := flags&fsevents.FlagItemRenamed != 0
	createdNow := flags&fsevents.FlagItemCreated != 0 && !current.birth.Before(started)
	if current.exists {
		switch {
		case !known && (movedIn || createdNow), known && !previous.exists, known && previous.inode != current.inode:
			return []string{"create"}
		case current.isDir:
			return nil
		case !known && flags&fsevents.FlagItemModified != 0 && !current.mtime.Before(started), known && (previous.size != current.size || !previous.mtime.Equal(current.mtime)):
			return []string{"modify"}
		}
		return nil
	}
	if known && !previous.exists {
		return nil
	}
	if !known && flags&fsevents.FlagItemCreated != 0 && flags&fsevents.FlagItemRenamed == 0 {
		return []string{"create", "remove"}
	}
	if flags&fsevents.FlagItemRemoved != 0 {
		return []string{"remove"}
	}
	if flags&fsevents.FlagItemRenamed != 0 {
		return []string{"rename"}
	}
	return nil
}

// fsWatchFSEvents는 FSEvents 이벤트를 edc event로 바꾸는 상태다. 이벤트 goroutine 하나만 쓴다.
type fsWatchFSEvents struct {
	options             fsWatchOptions
	ignored             func(string, fs.DirEntry) bool
	root                string
	started             time.Time
	lastSweep           time.Time
	states              map[string]fsWatchPathState
	excludedDirectories map[string]bool
	emit                func(fsWatchEvent) bool
}

func newFSWatchFSEvents(options fsWatchOptions, started time.Time, emit func(fsWatchEvent) bool) *fsWatchFSEvents {
	return &fsWatchFSEvents{
		options: options, ignored: fsWatchIgnored(options),
		// FSEvents는 경로를 NFC로 보낸다. Finder가 만든 한글 이름처럼 NFD로 저장된 루트도 같은 형태로 비교한다.
		root:    fsevents.NormalizePath(options.root),
		started: started, lastSweep: started,
		states: map[string]fsWatchPathState{}, excludedDirectories: map[string]bool{}, emit: emit,
	}
}

func newFSWatchSource(options fsWatchOptions) (fsWatchSource, error) {
	started := time.Now()
	stream, err := fsevents.Start(options.root, fsWatchLatency)
	if err != nil {
		return fsWatchSource{}, err
	}
	events := make(chan fsWatchEvent, 256)
	failures := make(chan error, 1)
	done, finished := make(chan struct{}), make(chan struct{})
	watch := newFSWatchFSEvents(options, started, func(event fsWatchEvent) bool {
		select {
		case events <- event:
			return true
		case <-done:
			return false
		}
	})
	go func() {
		defer close(finished)
		defer close(events)
		defer close(failures)
		for {
			select {
			case <-done:
				return
			case batch, ok := <-stream.Events:
				if !ok {
					return
				}
				if err := watch.handle(batch, time.Now()); err != nil {
					if !errors.Is(err, fs.ErrClosed) {
						select {
						case failures <- err:
						case <-done:
						}
					}
					return
				}
			}
		}
	}()
	var once sync.Once
	return fsWatchSource{events: events, errors: failures, close: func() {
		once.Do(func() { close(done); stream.Close(); <-finished })
	}}, nil
}

// handle은 FSEvents callback 한 번의 이벤트를 처리한다. 출력이 닫히면 fs.ErrClosed를 돌려준다.
func (watch *fsWatchFSEvents) handle(batch []fsevents.Event, now time.Time) error {
	if now.Sub(watch.lastSweep) >= fsWatchSweepInterval {
		watch.lastSweep = now
		fsWatchForgetGone(watch.states, now)
	}
	// macOS가 이벤트를 버리면 그 경로에 MustScanSubDirs를 보낸다. 상위 경로부터 한 번씩만 다시 훑고, 나머지
	// 이벤트는 갱신된 상태와 비교하므로 같은 변경을 두 번 알리지 않는다.
	var rescans []string
	for _, raw := range batch {
		if raw.Flags&fsevents.FlagRootChanged != 0 {
			return errors.New(T("watchfs.root_removed", watch.options.root))
		}
		if raw.Flags&fsevents.FlagMustScanSubDirs != 0 {
			if relative, ok := watch.relative(raw.Path); ok && !watch.skipped(relative, nil) {
				rescans = append(rescans, relative)
			}
		}
	}
	slices.SortFunc(rescans, func(first, second string) int { return len(first) - len(second) })
	var scanned []string
	for _, relative := range rescans {
		if slices.ContainsFunc(scanned, func(parent string) bool { return fsWatchWithin(parent, relative) }) {
			continue
		}
		scanned = append(scanned, relative)
		shown := filepath.ToSlash(relative)
		if relative == "." {
			shown = watch.options.root
		}
		err := watch.rescan(relative, now)
		switch {
		case errors.Is(err, errFSWatchRescanLimit):
			fmt.Fprintln(fsWatchNotice, T("watchfs.rescan_partial", shown, fsWatchRescanLimit))
		case err != nil:
			return err
		default:
			fmt.Fprintln(fsWatchNotice, T("watchfs.rescanned", shown))
		}
	}
	for _, raw := range batch {
		if raw.Flags&fsevents.FlagMustScanSubDirs != 0 {
			continue
		}
		relative, ok := watch.relative(raw.Path)
		if !ok || relative == "." || watch.skipped(relative, nil) {
			continue
		}
		if err := watch.update(relative, raw.Flags, now); err != nil {
			return err
		}
	}
	return nil
}

// relative는 이벤트 경로를 감시 루트 기준으로 바꾼다. 루트 밖이면 false다.
func (watch *fsWatchFSEvents) relative(path string) (string, bool) {
	relative, err := filepath.Rel(watch.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

// skipped는 --recursive 없이 하위 디렉터리에 있거나 제외된 경로를 거른다. entry는 walk에서 얻은 항목이고,
// 이벤트에서는 nil이다.
func (watch *fsWatchFSEvents) skipped(relative string, entry fs.DirEntry) bool {
	if relative == "." {
		return false
	}
	if !watch.options.recursive && strings.Contains(relative, string(filepath.Separator)) {
		return true
	}
	return watch.underExcluded(relative) || watch.ignored(filepath.Join(watch.options.root, relative), entry)
}

// underExcluded는 제외된 디렉터리 아래의 경로를 거른다. FSEvents는 제외와 관계없이 하위 트리 전체를 보낸다.
// 상위 디렉터리는 출력 파일과 비교할 필요가 없으므로 glob만 비교하고 그 결과를 둔다. 이름이 매번 다른
// 디렉터리가 쌓이면 비운다.
func (watch *fsWatchFSEvents) underExcluded(relative string) bool {
	name := filepath.ToSlash(relative)
	for index := strings.IndexByte(name, '/'); index >= 0; {
		parent := name[:index]
		excluded, ok := watch.excludedDirectories[parent]
		if !ok {
			if len(watch.excludedDirectories) >= fsWatchExcludeEntries {
				clear(watch.excludedDirectories)
			}
			excluded = fsWatchExcluded(watch.options.exclude, parent)
			watch.excludedDirectories[parent] = excluded
		}
		if excluded {
			return true
		}
		next := strings.IndexByte(name[index+1:], '/')
		if next < 0 {
			break
		}
		index += next + 1
	}
	return false
}

// update는 한 경로를 lstat해서 상태를 갱신하고 바뀐 내용을 알린다.
func (watch *fsWatchFSEvents) update(relative string, flags uint32, now time.Time) error {
	filename := filepath.Join(watch.options.root, relative)
	current := fsWatchPathState{isDir: flags&fsevents.FlagItemIsDir != 0}
	symlink := false
	if info, err := os.Lstat(filename); err == nil {
		current = newFSWatchPathState(info)
		symlink = info.Mode()&os.ModeSymlink != 0
	}
	previous, known := watch.states[filename]
	if !current.exists {
		current.gone = now
		if known {
			current.isDir = previous.isDir
		}
		if current.isDir {
			fsWatchForgetTree(watch.states, filename)
		}
	}
	watch.states[filename] = current
	for _, name := range fsWatchClassify(flags, previous, known, current, watch.started) {
		if symlink && name != "create" {
			continue
		}
		if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: name, Path: filepath.ToSlash(relative), IsDir: current.isDir}) {
			return fs.ErrClosed
		}
		if name == "create" && current.isDir && watch.options.recursive && flags&fsevents.FlagItemRenamed != 0 {
			if err := watch.announceTree(filename); err != nil {
				return err
			}
		}
	}
	return nil
}

// announceTree는 이동해 들어온 디렉터리의 하위 항목을 create로 알린다. FSEvents는 그 디렉터리 하나만 보낸다.
func (watch *fsWatchFSEvents) announceTree(directory string) error {
	return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if filename == directory {
			return nil
		}
		relative, err := filepath.Rel(watch.options.root, filename)
		if err != nil {
			return err
		}
		if watch.skipped(relative, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		watch.states[filename] = newFSWatchPathState(info)
		if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: "create", Path: filepath.ToSlash(relative), IsDir: entry.IsDir()}) {
			return fs.ErrClosed
		}
		return nil
	})
}

// rescan은 이벤트가 유실된 디렉터리를 다시 훑어 기억한 상태와 맞춘다. 그 사이의 순서는 알 수 없으므로 이동은
// 이전 경로의 remove와 새 경로의 create로 나온다.
func (watch *fsWatchFSEvents) rescan(relative string, now time.Time) error {
	directory := filepath.Join(watch.options.root, relative)
	visited := map[string]bool{}
	entries := 0
	walkErr := filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
		if entries++; entries > fsWatchRescanLimit {
			return errFSWatchRescanLimit
		}
		if err != nil {
			// 훑는 사이에 사라졌거나 읽을 수 없는 디렉터리는 건너뛴다. 사라진 경로는 아래에서 remove가 된다.
			if entry != nil && entry.IsDir() && filename != directory {
				return filepath.SkipDir
			}
			return nil
		}
		if filename == watch.options.root {
			return nil
		}
		child, err := filepath.Rel(watch.options.root, filename)
		if err != nil {
			return err
		}
		if watch.skipped(child, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		visited[filename] = true
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		current := newFSWatchPathState(info)
		previous, known := watch.states[filename]
		var flags uint32
		if !known {
			flags = fsevents.FlagItemCreated | fsevents.FlagItemModified
		}
		names := fsWatchClassify(flags, previous, known, current, watch.started)
		// 감시 전과 같은 항목까지 기억하면 다시 훑을 때마다 트리 전체가 map에 들어간다.
		if known || len(names) > 0 {
			watch.states[filename] = current
		}
		for _, name := range names {
			if info.Mode()&os.ModeSymlink != 0 && name != "create" {
				continue
			}
			if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: name, Path: filepath.ToSlash(child), IsDir: current.isDir}) {
				return fs.ErrClosed
			}
		}
		if entry.IsDir() && !watch.options.recursive && filename != directory {
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errFSWatchRescanLimit) {
		return walkErr
	}
	var gone []string
	for filename, state := range watch.states {
		if state.exists && !visited[filename] && fsWatchWithin(directory, filename) {
			gone = append(gone, filename)
		}
	}
	slices.Sort(gone)
	for _, filename := range gone {
		if _, err := os.Lstat(filename); err == nil {
			continue
		}
		state := watch.states[filename]
		state.exists, state.gone = false, now
		watch.states[filename] = state
		child, err := filepath.Rel(watch.options.root, filename)
		if err != nil {
			return err
		}
		if !watch.emit(fsWatchEvent{Time: time.Now().UTC(), Event: "remove", Path: filepath.ToSlash(child), IsDir: state.isDir}) {
			return fs.ErrClosed
		}
	}
	return walkErr
}

// fsWatchWithin은 path가 parent이거나 그 아래에 있는지 본다. "."은 감시 루트다.
func fsWatchWithin(parent, path string) bool {
	return parent == "." || path == parent || strings.HasPrefix(path, parent+string(filepath.Separator))
}

func fsWatchForgetGone(states map[string]fsWatchPathState, now time.Time) {
	for name, state := range states {
		if !state.exists && now.Sub(state.gone) > fsWatchGoneTTL {
			delete(states, name)
		}
	}
}

// fsWatchForgetTree는 사라진 디렉터리 아래의 상태를 지운다. 디렉터리를 옮기면 하위 경로에는 이벤트가 오지 않는다.
func fsWatchForgetTree(states map[string]fsWatchPathState, directory string) {
	prefix := directory + string(filepath.Separator)
	for name := range states {
		if strings.HasPrefix(name, prefix) {
			delete(states, name)
		}
	}
}
