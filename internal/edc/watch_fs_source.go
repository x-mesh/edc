package edc

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// fsWatchLoss는 감시가 놓친 변경의 수다. 드라이버와 큐 goroutine이 더하고 감시 루프가 요약에 싣는다. nil이면 세지 않는다.
type fsWatchLoss struct {
	// overflows는 커널 큐가 넘친 횟수다. 커널은 그때 몇 건을 버렸는지 알려 주지 않는다.
	overflows atomic.Int64
	// dropped는 edc가 버린 이벤트 수다. 사용자 큐가 찼을 때와 fanotify가 경로를 찾지 못한 이벤트를 더한다.
	dropped atomic.Int64
	// rescans는 macOS가 이벤트를 버려 디렉터리를 다시 훑은 횟수다.
	rescans atomic.Int64
}

func (loss *fsWatchLoss) overflow() {
	if loss != nil {
		loss.overflows.Add(1)
	}
}

func (loss *fsWatchLoss) drop(count int) {
	if loss != nil {
		loss.dropped.Add(int64(count))
	}
}

func (loss *fsWatchLoss) rescan() {
	if loss != nil {
		loss.rescans.Add(1)
	}
}

// counts는 지금까지의 수다. nil이면 모두 0이다.
func (loss *fsWatchLoss) counts() (overflows, dropped, rescans int) {
	if loss == nil {
		return 0, 0, 0
	}
	return int(loss.overflows.Load()), int(loss.dropped.Load()), int(loss.rescans.Load())
}

// fsWatchDropNoticeInterval은 이벤트 유실 안내 사이의 최소 간격이다. 커널은 큐가 찼다가 비는 한 번마다 overflow를
// 하나 보내므로, 초당 수만 건이 흐르면 안내도 초당 여러 번 나온다.
const fsWatchDropNoticeInterval = 10 * time.Second

// fsWatchDropNotice는 이벤트가 유실됐다는 안내를 간격을 두고 낸다. 한 goroutine에서만 쓴다.
type fsWatchDropNotice struct {
	now  func() time.Time
	last time.Time
}

func newFSWatchDropNotice() *fsWatchDropNotice { return &fsWatchDropNotice{now: time.Now} }

// notice는 key의 안내를 낸다. 앞 안내가 fsWatchDropNoticeInterval 안이면 내지 않고 false를 돌려준다.
func (drop *fsWatchDropNotice) notice(key string, args ...interface{}) bool {
	now := drop.now()
	if !drop.last.IsZero() && now.Sub(drop.last) < fsWatchDropNoticeInterval {
		return false
	}
	drop.last = now
	fmt.Fprintln(fsWatchNotice, T(key, args...))
	return true
}

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

// fsWatchIgnoredEvent는 이벤트의 경로를 fsWatchIgnored처럼 거른다. info는 드라이버가 그 경로를 지금 Lstat한 결과이고,
// 지워진 경로는 nil이다. 그 결과로 출력 파일인지 비교해 이벤트마다 stat을 한 번 더 하지 않는다. 지워진 경로는 출력
// 파일일 수 없고, symbolic link는 가리키는 파일이 출력일 수 있어 그때만 따라가 본다.
func fsWatchIgnoredEvent(options fsWatchOptions) func(string, fs.FileInfo) bool {
	return func(filename string, info fs.FileInfo) bool {
		relative, err := filepath.Rel(options.root, filename)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
		if options.outputPath != "" && fsWatchPathsEqual(options.outputPath, filename) {
			return true
		}
		if fsWatchExcluded(options.exclude, filepath.ToSlash(relative)) {
			return true
		}
		if info == nil || options.outputInfo == nil {
			return false
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if info, err = os.Stat(filename); err != nil {
				return false
			}
		}
		return os.SameFile(info, options.outputInfo)
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
