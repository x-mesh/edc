//go:build !darwin

package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// newFSNotifyWatchSource는 디렉터리마다 watch를 등록한다. Linux에서는 inotify이고, fanotify를 쓸 수 없거나
// --driver inotify일 때 쓴다.
func newFSNotifyWatchSource(options fsWatchOptions) (fsWatchSource, error) {
	watcher, err := fsnotify.NewBufferedWatcher(256)
	if err != nil {
		return fsWatchSource{}, err
	}
	watched := map[string]bool{}
	directories := map[string]bool{}
	events := make(chan fsWatchEvent, 256)
	failures := make(chan error, 1)
	done, finished := make(chan struct{}), make(chan struct{})
	emit := func(event fsWatchEvent) bool {
		select {
		case events <- event:
			return true
		case <-done:
			return false
		}
	}
	ignored := fsWatchIgnored(options)
	ignoredEvent := fsWatchIgnoredEvent(options)
	addTree := func(directory string, announce bool) error {
		return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			// inotify는 디렉터리에만 watch를 건다. 처음 등록할 때는 파일마다 경로를 비교하고 출력 파일인지 stat하는 비용만 든다.
			if !announce && !entry.IsDir() {
				return nil
			}
			if filename != options.root && ignored(filename, entry) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				directories[filename] = true
				if filename != directory && !options.recursive {
					return filepath.SkipDir
				}
				if !watched[filename] {
					if err := watcher.Add(filename); err != nil {
						if filename != options.root && errors.Is(err, fs.ErrNotExist) {
							return filepath.SkipDir
						}
						if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
							return fmt.Errorf("%s: %w", T("watchfs.too_many_files"), err)
						}
						// inotify는 fs.inotify.max_user_watches를 넘으면 ENOSPC를 돌려준다. 그대로 두면 디스크 공간 오류로 보인다.
						if runtime.GOOS == "linux" && errors.Is(err, syscall.ENOSPC) {
							return fmt.Errorf("%s: %w", T("watchfs.watch_limit"), err)
						}
						return err
					}
					watched[filename] = true
				}
			}
			if announce && filename != directory {
				relative, err := filepath.Rel(options.root, filename)
				if err != nil {
					return err
				}
				if !emit(fsWatchEvent{Time: time.Now().UTC(), Event: "create", Path: filepath.ToSlash(relative), IsDir: entry.IsDir()}) {
					return fs.ErrClosed
				}
			}
			return nil
		})
	}
	// 재귀 감시는 모든 디렉터리를 등록한 뒤에야 첫 줄을 낸다. 큰 트리에서는 오래 걸려 멈춘 것처럼 보이므로 터미널에 먼저 알린다.
	if options.recursive && isTerminal(os.Stderr) {
		fmt.Fprintln(fsWatchNotice, T("watchfs.walking", terminalJSON(options.root)))
	}
	if err := addTree(options.root, false); err != nil {
		watcher.Close()
		return fsWatchSource{}, err
	}
	overflow := newFSWatchDropNotice()
	go func() {
		defer close(finished)
		defer close(events)
		defer close(failures)
		fail := func(err error) {
			select {
			case failures <- err:
			case <-done:
			}
		}
		for {
			select {
			case <-done:
				return
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				// 커널 큐가 넘치면 그동안의 이벤트만 잃고 감시는 이어진다. 놓친 디렉터리 생성은 watch를 걸지 못해 그 아래가 끝까지
				// 보이지 않으므로, 알림을 낼 때마다(10초에 한 번) 트리를 다시 훑어 빠진 디렉터리에 watch를 건다.
				if errors.Is(err, fsnotify.ErrEventOverflow) {
					options.loss.overflow()
					if overflow.notice("watchfs.overflow") && options.recursive {
						if err := addTree(options.root, false); err != nil && !errors.Is(err, fs.ErrClosed) {
							fail(err)
							return
						}
					}
					continue
				}
				fail(err)
				return
			case raw, ok := <-watcher.Events:
				if !ok {
					return
				}
				if raw.Name == options.root && raw.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
					fail(errors.New(T("watchfs.root_removed", options.root)))
					return
				}
				if raw.Name == options.root {
					continue
				}
				// Lstat 결과를 출력 파일 비교에도 넘겨 이벤트마다 stat을 두 번 하지 않는다.
				var current fs.FileInfo
				if info, err := os.Lstat(raw.Name); err == nil {
					current = info
				}
				if ignoredEvent(raw.Name, current) {
					continue
				}
				relative, err := filepath.Rel(options.root, raw.Name)
				if err != nil {
					fail(err)
					return
				}
				isDir := directories[raw.Name]
				symlink := false
				if current != nil {
					isDir = current.IsDir()
					symlink = current.Mode()&os.ModeSymlink != 0
					if isDir {
						directories[raw.Name] = true
					}
				}
				for _, operation := range []struct {
					op   fsnotify.Op
					name string
				}{{fsnotify.Create, "create"}, {fsnotify.Write, "modify"}, {fsnotify.Remove, "remove"}, {fsnotify.Rename, "rename"}} {
					if raw.Op&operation.op == 0 || (isDir && operation.op == fsnotify.Write) || (symlink && operation.op != fsnotify.Create) {
						continue
					}
					if !emit(fsWatchEvent{Time: time.Now().UTC(), Event: operation.name, Path: filepath.ToSlash(relative), IsDir: isDir}) {
						return
					}
				}
				if raw.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && isDir {
					for name := range watched {
						if name == raw.Name || strings.HasPrefix(name, raw.Name+string(filepath.Separator)) {
							if err := watcher.Remove(name); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
								fail(err)
								return
							}
							delete(watched, name)
						}
					}
					for name := range directories {
						if name == raw.Name || strings.HasPrefix(name, raw.Name+string(filepath.Separator)) {
							delete(directories, name)
						}
					}
				}
				if raw.Op&fsnotify.Create != 0 && isDir && options.recursive {
					if err := addTree(raw.Name, true); err != nil && !errors.Is(err, fs.ErrClosed) {
						fail(fmt.Errorf("%s: %w", raw.Name, err))
						return
					}
				}
			}
		}
	}()
	var once sync.Once
	driver := "fsnotify"
	if runtime.GOOS == "linux" {
		driver = fsWatchDriverInotify
	}
	return fsWatchSource{events: events, errors: failures, driver: driver, close: func() {
		once.Do(func() { close(done); watcher.Close(); <-finished })
	}}, nil
}
