package edc

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
		// WalkDir는 하위 경로를 정리된 형태로 만든다. levels의 루트 키도 같은 형태여야 부모 규칙을 찾는다.
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
			if err := grepReadIgnore(ancestors[i], base, &patterns); err != nil {
				problems = append(problems, err.Error())
			}
		}
		levels := map[string][]gitignore.Pattern{}
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", path, err))
				return nil
			}
			if path != root {
				if entry.Name() == ".git" || (!options.hidden && strings.HasPrefix(entry.Name(), ".")) {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				abs, err := filepath.Abs(path)
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(base, abs)
				if err != nil {
					return err
				}
				parent := levels[filepath.Dir(path)]
				if !options.noIgnore && gitignore.NewMatcher(parent).Match(strings.Split(filepath.ToSlash(relative), "/"), entry.IsDir()) {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if entry.IsDir() {
					levels[path] = append([]gitignore.Pattern(nil), parent...)
				}
			} else {
				levels[path] = append([]gitignore.Pattern(nil), patterns...)
			}
			if entry.IsDir() {
				if !options.noIgnore {
					p := levels[path]
					abs, err := filepath.Abs(path)
					if err != nil {
						return err
					}
					if err := grepReadIgnore(abs, base, &p); err != nil {
						problems = append(problems, err.Error())
					}
					levels[path] = p
				}
				return nil
			}
			if entry.Type().IsRegular() {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				if !options.pathMode || grepPathMatch(filepath.ToSlash(relative), options) {
					files = append(files, path)
				}
			}
			return nil
		})
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	return files, directories, problems
}

func grepReadIgnore(dir, base string, patterns *[]gitignore.Pattern) error {
	path := filepath.Join(dir, ".gitignore")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	relative, err := filepath.Rel(base, dir)
	if err != nil {
		return err
	}
	var domain []string
	if relative != "." {
		domain = strings.Split(filepath.ToSlash(relative), "/")
	}
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
