package edc

import (
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// findQueryLimit는 질의의 최대 길이다. 점수 계산은 질의 길이에 비례하므로, 붙여 넣은 긴 글이 입력마다 화면을 멈추지 않게 자른다.
const findQueryLimit = 256

// 점수 상수다. 글자 하나가 맞을 때의 점수를 기준으로, 단어 경계와 파일 이름 안의 일치를 크게 올리고 간격은 조금씩 뺀다.
// 경로가 길수록 조금씩 빼서, 점수가 같으면 얕은 경로가 위로 온다.
const (
	findScoreMatch       = 16
	findScoreBoundary    = 8
	findScoreConsecutive = 4
	findScoreBasename    = 6
	findScoreExactName   = 32
	findScoreGap         = 1
	findScoreLengthUnit  = 16
)

// findMatcher는 후보 경로 하나를 판정한다. base는 경로에서 파일 이름이 시작하는 바이트 위치다.
// ranked가 false면 점수가 의미 없으므로 호출하는 쪽이 경로순으로 쓴다.
type findMatcher interface {
	score(path string, base int) (int, bool)
	spans(path string, base int) [][2]int
	ranked() bool
}

// findBase는 경로에서 파일 이름이 시작하는 위치다.
func findBase(path string) int {
	return strings.LastIndexByte(path, '/') + 1
}

// newFindMatcher는 질의로 matcher를 만든다. 빈 질의는 모든 후보와 맞는다. truncated는 질의를 findQueryLimit에서 잘랐다는 뜻이다.
func newFindMatcher(query string, regex bool) (matcher findMatcher, truncated bool, err error) {
	if len(query) > findQueryLimit {
		cut := findQueryLimit
		for cut > 0 && !utf8.RuneStart(query[cut]) {
			cut--
		}
		query, truncated = query[:cut], true
	}
	if regex {
		pattern := query
		if !findHasUpper(query) {
			pattern = "(?i)" + pattern
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, truncated, err
		}
		return findRegexMatcher{compiled}, truncated, nil
	}
	return newFindFuzzy(query), truncated, nil
}

func findHasUpper(text string) bool {
	for _, r := range text {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

type findRegexMatcher struct{ pattern *regexp.Regexp }

func (matcher findRegexMatcher) score(path string, _ int) (int, bool) {
	return 0, matcher.pattern.MatchString(path)
}

func (matcher findRegexMatcher) spans(path string, _ int) [][2]int {
	var spans [][2]int
	for _, span := range matcher.pattern.FindAllStringIndex(path, -1) {
		if span[0] < span[1] {
			spans = append(spans, [2]int{span[0], span[1]})
		}
	}
	return spans
}

func (findRegexMatcher) ranked() bool { return false }

// findTerm은 공백으로 나눈 질의 단어 하나다. text는 대소문자를 무시할 때 소문자로 바꾼 값이다.
// 질의에 ASCII가 아닌 글자가 있으면 runes로 비교한다. 바이트로 비교하면 한 글자의 UTF-8 바이트를 서로 다른 글자에서 골라 맞춘다.
type findTerm struct {
	text  string
	runes []rune
}

type findFuzzy struct {
	terms []findTerm
	// exact는 대문자가 있는 질의다. 이때만 대소문자를 구분한다.
	exact bool
}

func newFindFuzzy(query string) findFuzzy {
	fuzzy := findFuzzy{exact: findHasUpper(query)}
	for _, word := range strings.Fields(query) {
		if !fuzzy.exact {
			word = findLowerASCII(word)
		}
		term := findTerm{text: word}
		for index := 0; index < len(word); index++ {
			if word[index] >= utf8.RuneSelf {
				term.runes = []rune(word)
				if !fuzzy.exact {
					for position, r := range term.runes {
						term.runes[position] = findCaseFold(r)
					}
				}
				break
			}
		}
		fuzzy.terms = append(fuzzy.terms, term)
	}
	return fuzzy
}

func findLowerASCII(text string) string {
	for index := 0; index < len(text); index++ {
		if c := text[index]; c >= 'A' && c <= 'Z' {
			buffer := []byte(text)
			for position := index; position < len(buffer); position++ {
				if c := buffer[position]; c >= 'A' && c <= 'Z' {
					buffer[position] = c + 'a' - 'A'
				}
			}
			return string(buffer)
		}
	}
	return text
}

func (fuzzy findFuzzy) ranked() bool { return len(fuzzy.terms) > 0 }

func (fuzzy findFuzzy) score(path string, base int) (int, bool) {
	total := 0
	for _, term := range fuzzy.terms {
		score, ok := fuzzy.termScore(path, base, term)
		if !ok {
			return 0, false
		}
		total += score
	}
	if len(fuzzy.terms) > 0 {
		total -= len(path) / findScoreLengthUnit
	}
	return total, true
}

// termScore는 단어 하나의 점수다. 파일 이름 안에서 맞으면 그 구간과 전체 경로의 구간 중 높은 점수를 쓴다.
// 앞에서 처음 맞는 구간만 보면 nginx/conf.d/nginx.conf의 conf가 디렉터리에서 맞아 파일 이름의 보너스를 놓친다.
func (fuzzy findFuzzy) termScore(path string, base int, term findTerm) (int, bool) {
	start, end, ok := fuzzy.window(path, 0, term)
	if !ok {
		return 0, false
	}
	best := fuzzy.windowScore(path, base, start, end, term, nil)
	if base > 0 && start < base {
		if nameStart, nameEnd, inName := fuzzy.window(path, base, term); inName {
			best = max(best, fuzzy.windowScore(path, base, nameStart, nameEnd, term, nil))
		}
	}
	if fuzzy.equal(path[base:], term) {
		best += findScoreExactName
	}
	return best, true
}

func (fuzzy findFuzzy) equal(name string, term findTerm) bool {
	if fuzzy.exact {
		return name == term.text
	}
	if term.runes == nil {
		return len(name) == len(term.text) && findLowerASCII(name) == term.text
	}
	return strings.EqualFold(name, term.text)
}

// window는 from 뒤에서 단어가 순서대로 맞는 가장 짧은 구간을 찾는다. 앞에서 끝을 찾고, 그 끝에서 거꾸로 시작을 줄인다(fzf v1).
func (fuzzy findFuzzy) window(path string, from int, term findTerm) (int, int, bool) {
	if term.runes != nil {
		return fuzzy.runeWindow(path, from, term.runes)
	}
	query := term.text
	matched, end := 0, -1
	for index := from; index < len(path); index++ {
		if fuzzy.fold(path[index]) == query[matched] {
			matched++
			if matched == len(query) {
				end = index
				break
			}
		}
	}
	if end < 0 {
		return 0, 0, false
	}
	matched = len(query) - 1
	for index := end; index >= from; index-- {
		if fuzzy.fold(path[index]) == query[matched] {
			if matched == 0 {
				return index, end, true
			}
			matched--
		}
	}
	return 0, 0, false
}

func (fuzzy findFuzzy) fold(c byte) byte {
	if !fuzzy.exact && c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func (fuzzy findFuzzy) foldRune(r rune) rune {
	if fuzzy.exact {
		return r
	}
	return findCaseFold(r)
}

// findCaseFold는 대소문자를 가리지 않을 때 견주는 글자다. 소문자로 바꾼 뒤 단순 대소문자 접기의 순환에서 가장 작은 글자를
// 고른다. 소문자로만 바꾸면 σ와 끝 시그마 ς처럼 같은 접기에 드는 소문자끼리 다르고, 접기만 쓰면 İ가 i와 같아지지 않는다.
func findCaseFold(r rune) rune {
	r = unicode.ToLower(r)
	least := r
	for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
		least = min(least, next)
	}
	return least
}

func (fuzzy findFuzzy) runeWindow(path string, from int, query []rune) (int, int, bool) {
	matched, end := 0, -1
	for index := from; index < len(path); {
		r, size := utf8.DecodeRuneInString(path[index:])
		if fuzzy.foldRune(r) == query[matched] {
			matched++
			if matched == len(query) {
				end = index
				break
			}
		}
		index += size
	}
	if end < 0 {
		return 0, 0, false
	}
	matched = len(query) - 1
	// 끝 글자의 크기는 경로 쪽에서 잰다. 대소문자를 바꾸면 UTF-8 길이가 달라지는 글자가 있다(K와 켈빈 기호).
	_, last := utf8.DecodeRuneInString(path[end:])
	for index := end + last; index > from; {
		r, size := utf8.DecodeLastRuneInString(path[from:index])
		index -= size
		if fuzzy.foldRune(r) == query[matched] {
			if matched == 0 {
				return index, end, true
			}
			matched--
		}
	}
	return 0, 0, false
}

// windowScore는 구간 안에서 글자를 앞에서부터 맞춰 점수를 매긴다. positions가 있으면 맞은 글자의 시작 바이트를 담는다.
func (fuzzy findFuzzy) windowScore(path string, base, start, end int, term findTerm, positions *[]int) int {
	score, matched, run := 0, 0, 0
	length := len(term.text)
	if term.runes != nil {
		length = len(term.runes)
	}
	for index := start; index <= end && matched < length; {
		size := 1
		equal := false
		if term.runes != nil {
			var r rune
			r, size = utf8.DecodeRuneInString(path[index:])
			equal = fuzzy.foldRune(r) == term.runes[matched]
		} else {
			equal = fuzzy.fold(path[index]) == term.text[matched]
		}
		if equal {
			score += findScoreMatch
			if findBoundary(path, index) {
				score += findScoreBoundary
			}
			score += findScoreConsecutive * run
			if index >= base {
				score += findScoreBasename
			}
			if positions != nil {
				*positions = append(*positions, index)
			}
			run++
			matched++
		} else {
			// 구분 문자만 건너뛴 것은 연속으로 본다. sshdconfig처럼 구분 문자를 빼고 치는 것이 fuzzy의 주된 쓰임이다.
			if !findSeparator(path[index]) {
				run = 0
			}
			score -= findScoreGap
		}
		index += size
	}
	return score
}

// findBoundary는 index의 글자가 단어의 시작인지 본다. 경로의 시작, 구분 문자 뒤, 소문자 뒤의 대문자다.
func findBoundary(path string, index int) bool {
	if index == 0 {
		return true
	}
	previous, current := path[index-1], path[index]
	return findSeparator(previous) || previous >= 'a' && previous <= 'z' && current >= 'A' && current <= 'Z'
}

func findSeparator(c byte) bool {
	switch c {
	case '/', '_', '-', '.', ' ':
		return true
	}
	return false
}

// spans는 강조할 바이트 구간이다. 점수를 매긴 것과 같은 구간을 고른다.
func (fuzzy findFuzzy) spans(path string, base int) [][2]int {
	var positions []int
	for _, term := range fuzzy.terms {
		start, end, ok := fuzzy.window(path, 0, term)
		if !ok {
			return nil
		}
		best := fuzzy.windowScore(path, base, start, end, term, nil)
		if base > 0 && start < base {
			if nameStart, nameEnd, inName := fuzzy.window(path, base, term); inName {
				if fuzzy.windowScore(path, base, nameStart, nameEnd, term, nil) > best {
					start, end = nameStart, nameEnd
				}
			}
		}
		fuzzy.windowScore(path, base, start, end, term, &positions)
	}
	return findSpansFromPositions(path, positions)
}

// findSpansFromPositions는 맞은 글자의 시작 바이트를 이어진 구간으로 묶는다.
func findSpansFromPositions(path string, positions []int) [][2]int {
	if len(positions) == 0 {
		return nil
	}
	seen := make(map[int]bool, len(positions))
	ordered := make([]int, 0, len(positions))
	for _, position := range positions {
		if !seen[position] {
			seen[position] = true
			ordered = append(ordered, position)
		}
	}
	// 단어마다 따로 맞추므로 위치가 섞일 수 있다.
	for left := 1; left < len(ordered); left++ {
		for right := left; right > 0 && ordered[right-1] > ordered[right]; right-- {
			ordered[right-1], ordered[right] = ordered[right], ordered[right-1]
		}
	}
	var spans [][2]int
	for _, position := range ordered {
		_, size := utf8.DecodeRuneInString(path[position:])
		if len(spans) > 0 && spans[len(spans)-1][1] == position {
			spans[len(spans)-1][1] = position + size
			continue
		}
		spans = append(spans, [2]int{position, position + size})
	}
	return spans
}

// findHit는 맞은 후보의 번호와 점수다.
type findHit struct {
	index, score int32
}

// findScoreChunk는 goroutine 하나가 한 번에 맡는 후보 수다. 이보다 적으면 나누는 비용이 더 크다.
const findScoreChunk = 4096

// findScore는 paths를 병렬로 판정해 맞은 후보를 번호 순서대로 돌려준다. candidates가 nil이 아니면 그 번호만 본다.
// 질의를 늘린 경우 앞 결과만 다시 보면 되므로 선택 화면이 candidates를 넘긴다. workers가 0이면 GOMAXPROCS다.
func findScore(matcher findMatcher, paths []string, candidates []int32, workers int) []findHit {
	total := len(paths)
	if candidates != nil {
		total = len(candidates)
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = max(1, min(workers, (total+findScoreChunk-1)/findScoreChunk))
	parts := make([][]findHit, workers)
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			from, to := worker*total/workers, (worker+1)*total/workers
			var hits []findHit
			for position := from; position < to; position++ {
				index := position
				if candidates != nil {
					index = int(candidates[position])
				}
				path := paths[index]
				if score, ok := matcher.score(path, findBase(path)); ok {
					hits = append(hits, findHit{index: int32(index), score: int32(score)})
				}
			}
			parts[worker] = hits
		})
	}
	group.Wait()
	count := 0
	for _, part := range parts {
		count += len(part)
	}
	hits := make([]findHit, 0, count)
	for _, part := range parts {
		hits = append(hits, part...)
	}
	return hits
}
