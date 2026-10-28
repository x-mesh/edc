package edc

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

func grepPathMatch(path string, options grepOptions) bool {
	return options.pathRegex != nil && options.pathRegex.MatchString(path)
}

type grepPathRoot struct {
	path      string
	directory bool
}

func grepPathRoots(paths []string) []grepPathRoot {
	roots := make([]grepPathRoot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		roots = append(roots, grepPathRoot{path: path, directory: err == nil && info.IsDir()})
	}
	return roots
}

func grepHighlightPath(path string, roots []grepPathRoot, options grepOptions, color bool) string {
	if !color {
		return path
	}
	for _, root := range roots {
		relative := filepath.Base(path)
		if root.directory {
			var err error
			relative, err = filepath.Rel(root.path, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				continue
			}
		} else if filepath.Clean(root.path) != filepath.Clean(path) {
			continue
		}
		if !strings.HasSuffix(path, relative) || options.pathRegex == nil {
			continue
		}
		spans := options.pathRegex.FindAllStringIndex(filepath.ToSlash(relative), -1)
		if len(spans) == 0 {
			continue
		}
		var out strings.Builder
		out.WriteString(viewSanitize(path[:len(path)-len(relative)]))
		pos := 0
		for _, span := range spans {
			if span[0] == span[1] {
				continue
			}
			out.WriteString(viewSanitize(relative[pos:span[0]]))
			out.WriteString(richPaint(viewSanitize(relative[span[0]:span[1]]), richRed, true))
			pos = span[1]
		}
		out.WriteString(viewSanitize(relative[pos:]))
		return out.String()
	}
	return viewSanitize(path)
}

func grepCodeTargets(paths []string, options grepOptions) ([]string, int, []string) {
	var files, problems []string
	directories := 0
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !info.IsDir() {
			if !options.pathMode || grepPathMatch(filepath.Base(root), options) {
				files = append(files, root)
			}
			continue
		}
		directories++
		// 하위 경로는 filepath.Join으로 정리된 형태가 된다. 루트도 정리해야 "src/"와 "src"가 같은 결과를 낸다.
		root = filepath.Clean(root)
		absolute, err := filepath.Abs(root)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		base := absolute
		for current := absolute; ; current = filepath.Dir(current) {
			if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
				base = current
				break
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
		var patterns []gitignore.Pattern
		ancestors := []string{}
		for d := absolute; d != base; d = filepath.Dir(d) {
			ancestors = append(ancestors, d)
		}
		ancestors = append(ancestors, base)
		for i := len(ancestors) - 1; i >= 1 && !options.noIgnore; i-- {
			if err := grepReadIgnore(ancestors[i], grepIgnoreDomain(base, ancestors[i]), &patterns); err != nil {
				problems = append(problems, err.Error())
			}
		}
		walk := grepCodeWalk{options: options, spawn: make(chan struct{}, runtime.GOMAXPROCS(0)*grepCodeWalkSpawnPerCPU)}
		result := walk.dir(root, "", grepIgnoreDomain(base, absolute), patterns)
		files, problems = append(files, result.files...), append(problems, result.problems...)
	}
	return files, directories, problems
}

// grepCodeWalk는 코드 검색 대상을 이름 순서로 모은다. filepath.WalkDir 대신 디렉터리를 직접 읽어, 목록에 .gitignore가
// 있을 때만 연다. 디렉터리마다 없는 .gitignore를 열어 보는 시스템 호출이 큰 저장소 탐색 시간의 절반을 넘었다.
// 디렉터리를 여는 시스템 호출도 하나에 수십 µs라서 하위 디렉터리는 병렬로 읽고, 결과는 이름 순서대로 잇는다.
type grepCodeWalk struct {
	options grepOptions
	// spawn은 하위 디렉터리를 읽으려고 띄우는 goroutine 수의 상한이다. 자리가 없으면 지금 goroutine에서 바로 읽는다. 상위
	// goroutine은 하위 결과를 기다리는 동안 살아 있으므로, 디렉터리마다 띄우면 큰 트리에서 디렉터리 수만큼 goroutine이 쌓인다.
	spawn chan struct{}
}

// grepCodeWalkSpawnPerCPU는 CPU 하나에 둘 탐색 goroutine 수다. 상위 goroutine이 하위 결과를 기다리며 자리를 쥐고 있어도
// 디렉터리를 읽는 goroutine이 CPU 수보다 넉넉하도록 몇 배로 둔다.
const grepCodeWalkSpawnPerCPU = 4

type grepCodeWalkResult struct {
	files, problems []string
}

// dir는 path 아래를 훑는다. relative는 검색 루트에서 본 경로, parts는 저장소 루트에서 본 경로 조각이다.
func (walk *grepCodeWalk) dir(path, relative string, parts []string, patterns []gitignore.Pattern) grepCodeWalkResult {
	var result grepCodeWalkResult
	entries, err := os.ReadDir(path)
	if err != nil {
		result.problems = append(result.problems, fmt.Sprintf("%s: %v", path, err))
	}
	if !walk.options.noIgnore {
		for _, entry := range entries {
			if entry.Name() == ".gitignore" {
				// 형제 디렉터리가 같은 배열에 덧붙이지 않도록 용량을 잘라 둔다.
				patterns = slices.Clip(patterns)
				if err := grepReadIgnore(path, parts, &patterns); err != nil {
					result.problems = append(result.problems, err.Error())
				}
				break
			}
		}
	}
	matcher := gitignore.NewMatcher(patterns)
	// items는 파일 경로이거나, 아직 읽고 있는 하위 디렉터리의 결과다. 이름 순서를 지키려고 섞어서 둔다.
	type item struct {
		file  string
		child *grepCodeWalkResult
	}
	var items []item
	var group sync.WaitGroup
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || (!walk.options.hidden && strings.HasPrefix(name, ".")) {
			continue
		}
		childParts := append(slices.Clip(parts), name)
		if !walk.options.noIgnore && matcher.Match(childParts, entry.IsDir()) {
			continue
		}
		child, childRelative := filepath.Join(path, name), name
		if relative != "" {
			childRelative = relative + "/" + name
		}
		if entry.IsDir() {
			pending := &grepCodeWalkResult{}
			items = append(items, item{child: pending})
			select {
			case walk.spawn <- struct{}{}:
				group.Go(func() {
					defer func() { <-walk.spawn }()
					*pending = walk.dir(child, childRelative, childParts, patterns)
				})
			default:
				*pending = walk.dir(child, childRelative, childParts, patterns)
			}
			continue
		}
		if entry.Type().IsRegular() && (!walk.options.pathMode || grepPathMatch(childRelative, walk.options)) {
			items = append(items, item{file: child})
		}
	}
	group.Wait()
	for _, item := range items {
		if item.child == nil {
			result.files = append(result.files, item.file)
			continue
		}
		result.files = append(result.files, item.child.files...)
		result.problems = append(result.problems, item.child.problems...)
	}
	return result
}

// grepIgnoreDomain은 저장소 루트 base에서 본 dir의 경로 조각이다. .gitignore 규칙은 이 조각에 상대적으로 맞춘다.
func grepIgnoreDomain(base, dir string) []string {
	relative, err := filepath.Rel(base, dir)
	if err != nil || relative == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(relative), "/")
}

func grepReadIgnore(dir string, domain []string, patterns *[]gitignore.Pattern) error {
	path := filepath.Join(dir, ".gitignore")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		*patterns = append(*patterns, gitignore.ParsePattern(line, domain))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
