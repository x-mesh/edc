package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fsWatchEvent struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
	Path  string    `json:"path"`
	IsDir bool      `json:"is_directory"`
}

type fsWatchSource struct {
	events <-chan fsWatchEvent
	errors <-chan error
	close  func()
}

func newFSWatchSource(options fsWatchOptions) (fsWatchSource, error) {
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
	ignored := func(filename string) bool {
		relative, err := filepath.Rel(options.root, filename)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
		if options.outputPath != "" && fsWatchPathsEqual(options.outputPath, filename) {
			return true
		}
		if options.outputInfo != nil {
			if info, err := os.Stat(filename); err == nil && os.SameFile(info, options.outputInfo) {
				return true
			}
		}
		name := filepath.ToSlash(relative)
		for _, pattern := range options.exclude {
			if matchFSWatchGlob(pattern, name) {
				return true
			}
		}
		return false
	}
	addTree := func(directory string, announce bool) error {
		return filepath.WalkDir(directory, func(filename string, entry fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if filename != options.root && ignored(filename) {
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
	if err := addTree(options.root, false); err != nil {
		watcher.Close()
		return fsWatchSource{}, err
	}
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
				if raw.Name == options.root || ignored(raw.Name) {
					continue
				}
				relative, err := filepath.Rel(options.root, raw.Name)
				if err != nil {
					fail(err)
					return
				}
				isDir := directories[raw.Name]
				symlink := false
				if info, err := os.Lstat(raw.Name); err == nil {
					isDir = info.IsDir()
					symlink = info.Mode()&os.ModeSymlink != 0
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
	return fsWatchSource{events: events, errors: failures, close: func() {
		once.Do(func() { close(done); watcher.Close(); <-finished })
	}}, nil
}
