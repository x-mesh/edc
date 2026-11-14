package edc

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

var errPackExcludeEmpty = errors.New("--exclude needs a pattern")

// packExcludes는 여러 번 줄 수 있는 --exclude 값이다. .gitignore와 같은 문법이고, 묶는 디렉터리 안의 경로에 맞춘다.
type packExcludes []gitignore.Pattern

func (patterns *packExcludes) String() string { return "" }

func (patterns *packExcludes) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errPackExcludeEmpty
	}
	*patterns = append(*patterns, gitignore.ParsePattern(value, nil))
	return nil
}

// packFilter는 묶을 때 뺄 항목을 고른다. --exclude는 최상위 아래 경로에 맞추고, --gitignore는 .gitignore를 git처럼 읽는다.
// 최상위가 git 작업 트리 안이면 그 위 디렉터리의 .gitignore도 읽어, 하위 디렉터리를 묶어도 edc find와 같은 규칙이 된다.
// 명령줄에 준 경로 자체는 빼지 않는다.
type packFilter struct {
	exclude  gitignore.Matcher
	git      bool
	prefix   []string
	patterns []gitignore.Pattern
}

func newPackFilter(dir string, options packOptions) (*packFilter, error) {
	if len(options.exclude) == 0 && !options.gitignore {
		return nil, nil
	}
	filter := &packFilter{git: options.gitignore}
	if len(options.exclude) > 0 {
		filter.exclude = gitignore.NewMatcher(options.exclude)
	}
	if !options.gitignore {
		return filter, nil
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	base, ok := findRepositoryBase(absolute)
	if !ok {
		return filter, nil
	}
	filter.prefix = grepIgnoreDomain(base, absolute)
	var ancestors []string
	for current := filepath.Dir(absolute); ; current = filepath.Dir(current) {
		ancestors = append(ancestors, current)
		if current == base || current == filepath.Dir(current) {
			break
		}
	}
	if absolute == base {
		ancestors = nil
	}
	var rules []packRule
	for index := len(ancestors) - 1; index >= 0; index-- {
		domain := grepIgnoreDomain(base, ancestors[index])
		lines, err := grepIgnoreLines(ancestors[index])
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			pattern := gitignore.ParsePattern(line, domain)
			rules = append(rules, packRule{line: line, pattern: pattern})
			filter.patterns = append(filter.patterns, pattern)
		}
	}
	// 위의 규칙이 지정한 디렉터리 자체를 무시하면(dist/를 무시하는 저장소에서 edc pack --gitignore dist), 그 아래가 모두
	// 빠져 빈 묶음이 된다. 직접 준 경로는 묶는다는 약속을 지키려고 그 디렉터리나 그 위에 맞는 규칙을 고친다.
	if len(filter.patterns) > 0 && gitignore.NewMatcher(filter.patterns).Match(filter.prefix, true) {
		filter.patterns = packRulesBelow(rules, filter.prefix)
	}
	return filter, nil
}

type packRule struct {
	line    string
	pattern gitignore.Pattern
}

// packRulesBelow는 지정한 디렉터리나 그 위에 맞는 규칙을 안의 항목에만 쓰이게 바꾼다. 이름만 보는 규칙(*.env, dist/, **/x)은
// 안의 항목에도 맞을 수 있으므로 버리지 않고 그 디렉터리를 기준으로 다시 읽는다. 그러면 *.env가 dist.env를 무시해도
// dist.env/private.env는 빠진다. 경로를 정한 규칙(/dist, a/dist)은 그 디렉터리나 그 위만 가리키므로 버린다.
func packRulesBelow(rules []packRule, prefix []string) []gitignore.Pattern {
	var kept []gitignore.Pattern
	for _, rule := range rules {
		above := false
		for depth := 1; depth <= len(prefix) && !above; depth++ {
			above = rule.pattern.Match(prefix[:depth], true) == gitignore.Exclude
		}
		switch {
		case !above:
			kept = append(kept, rule.pattern)
		case packNameRule(rule.line):
			kept = append(kept, gitignore.ParsePattern(rule.line, prefix))
		}
	}
	return kept
}

// packNameRule은 go-git처럼 끝의 /를 떼고도 /가 남지 않는 규칙, 또는 **/로 시작해 어느 깊이에나 맞는 규칙이다.
func packNameRule(line string) bool {
	body := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "!"), "/")
	return !strings.Contains(body, "/") || strings.HasPrefix(body, "**/")
}

// enter는 디렉터리 하나에 들어갈 때 그 .gitignore를 읽는다. 그 규칙은 디렉터리 안의 항목에만 쓰인다.
func (filter *packFilter) enter(dir string, parts []string) error {
	if filter == nil || !filter.git {
		return nil
	}
	return grepReadIgnore(dir, filter.full(parts), &filter.patterns)
}

// skip은 최상위 기준 경로 parts를 뺄지 본다. --gitignore면 git이 다루지 않는 .git도 뺀다.
func (filter *packFilter) skip(parts []string, dir bool) bool {
	if filter == nil || len(parts) == 0 {
		return false
	}
	if filter.exclude != nil && filter.exclude.Match(parts, dir) {
		return true
	}
	if !filter.git {
		return false
	}
	if parts[len(parts)-1] == ".git" {
		return true
	}
	return len(filter.patterns) > 0 && gitignore.NewMatcher(filter.patterns).Match(filter.full(parts), dir)
}

func (filter *packFilter) full(parts []string) []string {
	if len(parts) == 0 {
		return filter.prefix
	}
	return append(append([]string(nil), filter.prefix...), parts...)
}
