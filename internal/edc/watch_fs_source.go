package edc

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type fsWatchEvent struct {
	Time  time.Time `json:"time"`
	Event string    `json:"event"`
	Path  string    `json:"path"`
	IsDir bool      `json:"is_directory"`
	// PID와 Process는 이벤트를 일으킨 process다. fanotify만 알려 주고, 이미 끝난 process의 이름은 비어 있다.
	PID     int    `json:"pid,omitempty"`
	Process string `json:"process,omitempty"`
}

// --driver 값이다. auto는 OS가 고르고, fanotify와 inotify는 Linux에서만 쓴다.
const (
	fsWatchDriverAuto     = "auto"
	fsWatchDriverFanotify = "fanotify"
	fsWatchDriverInotify  = "inotify"
)

// fsWatchExcludeEntries는 제외 여부를 기억할 상위 디렉터리 수다. 이름이 매번 다른 디렉터리가 쌓이면 비운다.
const fsWatchExcludeEntries = 4096

type fsWatchSource struct {
	events <-chan fsWatchEvent
	errors <-chan error
	close  func()
	// driver는 ready 기록에 나가는 실제 감시 방식이다.
	driver string
	// driverReason은 auto가 inotify를 고른 이유다. 직접 고른 방식과 fanotify는 비어 있다.
	driverReason string
}

// auto가 inotify를 고른 이유다. ready 기록의 driver_reason에 그대로 나간다.
const (
	fsWatchReasonNotRecursive = "not_recursive"
	fsWatchReasonFanotifyDown = "fanotify_unavailable"
)

// fsWatchIgnored는 감시 루트 밖의 경로, JSON 출력 파일과 exclude glob에 맞는 경로를 거른다. entry는 walk에서
// 얻은 항목이고, 이벤트처럼 없으면 nil이다.
func fsWatchIgnored(options fsWatchOptions) func(string, fs.DirEntry) bool {
	// 출력 파일인지는 stat해야 안다. walk에서는 출력과 종류가 같은 항목과 symbolic link만 확인해서, 터미널로 출력할 때 파일마다 stat하지 않는다.
	mayBeOutput := func(entry fs.DirEntry) bool {
		if entry == nil || options.outputInfo == nil {
			return true
		}
		kind := entry.Type()
		return kind&fs.ModeSymlink != 0 || kind == options.outputInfo.Mode().Type()
	}
	return func(filename string, entry fs.DirEntry) bool {
		relative, err := filepath.Rel(options.root, filename)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
		if mayBeOutput(entry) {
			if options.outputPath != "" && fsWatchPathsEqual(options.outputPath, filename) {
				return true
			}
			if options.outputInfo != nil {
				if info, err := os.Stat(filename); err == nil && os.SameFile(info, options.outputInfo) {
					return true
				}
			}
		}
		return fsWatchExcluded(options.exclude, filepath.ToSlash(relative))
	}
}

// fsWatchUnderExcluded는 제외된 디렉터리 아래의 경로를 거른다. FSEvents와 fanotify는 제외와 관계없이 하위 트리
// 전체를 보낸다. 상위 디렉터리는 출력 파일과 비교할 필요가 없으므로 glob만 비교하고 그 결과를 cache에 둔다.
func fsWatchUnderExcluded(patterns []string, cache map[string]bool, relative string) bool {
	name := filepath.ToSlash(relative)
	for index := strings.IndexByte(name, '/'); index >= 0; {
		parent := name[:index]
		excluded, ok := cache[parent]
		if !ok {
			if len(cache) >= fsWatchExcludeEntries {
				clear(cache)
			}
			excluded = fsWatchExcluded(patterns, parent)
			cache[parent] = excluded
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

// fsWatchExcluded는 감시 루트 기준 slash 경로가 exclude glob에 맞는지 본다.
func fsWatchExcluded(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchFSWatchGlob(pattern, name) {
			return true
		}
	}
	return false
}
