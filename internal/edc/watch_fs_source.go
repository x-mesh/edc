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
}

type fsWatchSource struct {
	events <-chan fsWatchEvent
	errors <-chan error
	close  func()
}

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

// fsWatchExcluded는 감시 루트 기준 slash 경로가 exclude glob에 맞는지 본다.
func fsWatchExcluded(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matchFSWatchGlob(pattern, name) {
			return true
		}
	}
	return false
}
