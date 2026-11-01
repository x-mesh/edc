package edc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const grepAskMaxFileBytes = 2 << 20
const grepAskChunkLines = 32
const grepAskChunkOverlapLines = 8
const grepAskMaxScanBytes = 32 << 20
const grepAskMaxChunks = 50000
const grepAskSemanticMaxChunks = grepAskMaxChunks * grepAskChunkLines / (grepAskChunkLines - grepAskChunkOverlapLines)

// 디렉터리를 훑어 찾은 파일 중 아래 파일은 검색 결과를 흐리기만 하므로 수집하지 않는다. 경로로 직접 지정한 파일은 그대로 수집한다.
var grepAskLockFiles = map[string]bool{"go.sum": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "Cargo.lock": true, "poetry.lock": true, "uv.lock": true, "composer.lock": true, "Gemfile.lock": true}

var grepAskGeneratedHeader = regexp.MustCompile(`^\s*(//|/\*|#|--|<!--).*(?i:code generated .*do not edit|auto-?generated)`)

type grepAskResult struct {
	Path    string  `json:"path"`
	Start   int     `json:"start"`
	End     int     `json:"end"`
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet"`
}

type grepAskOutput struct {
	Method  string          `json:"method"`
	Results []grepAskResult `json:"results"`
	Skipped int             `json:"skipped_files"`
}

type grepSearchOutput struct {
	SchemaVersion int             `json:"schema_version"`
	Method        string          `json:"method"`
	Query         string          `json:"query"`
	Scopes        []string        `json:"scopes"`
	Complete      bool            `json:"complete"`
	SkippedFiles  int             `json:"skipped_files"`
	Results       []grepAskResult `json:"results"`
}

type grepSearchScope struct {
	path      string
	directory bool
	display   string
}

func grepSearchScopes(paths []string) ([]grepSearchScope, error) {
	scopes, problems := grepSearchScopesPartial(paths)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return scopes, nil
}

func grepSearchScopesPartial(paths []string) ([]grepSearchScope, []string) {
	scopes := make([]grepSearchScope, 0, len(paths))
	var problems []string
	seen := make(map[string]bool)
	for _, input := range paths {
		absolute, err := filepath.Abs(input)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", input, err))
			continue
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", input, err))
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", input, err))
			continue
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			problems = append(problems, fmt.Sprintf("%s: unsupported search path", input))
			continue
		}
		canonical = filepath.Clean(canonical)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		scopes = append(scopes, grepSearchScope{path: canonical, directory: info.IsDir(), display: input})
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].path < scopes[j].path })
	kept := scopes[:0]
	for _, candidate := range scopes {
		covered := false
		for _, parent := range kept {
			if !parent.directory {
				continue
			}
			relative, err := filepath.Rel(parent.path, candidate.path)
			if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, candidate)
		}
	}
	return kept, problems
}

func grepSearchDisplayPath(path string, scopes []grepSearchScope) string {
	for _, scope := range scopes {
		if scope.directory {
			relative, err := filepath.Rel(scope.path, path)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return filepath.Join(scope.display, relative)
			}
		} else if filepath.Clean(path) == scope.path {
			return scope.display
		}
	}
	return path
}

type grepAskChunk struct {
	rankPath string
	result   grepAskResult
	terms    map[string]int
	size     int
	lines    []string
}

func grepAskTokens(text string) []string {
	var words []string
	var word []rune
	var prev rune
	flush := func() {
		if len(word) > 0 {
			words = append(words, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	for _, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			prev = 0
			continue
		}
		if len(word) > 0 && unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
			flush()
		}
		word = append(word, r)
		prev = r
	}
	flush()
	return words
}

func grepAskChunks(path string, data []byte) []grepAskChunk {
	return grepAskChunksForMode(path, data, false)
}

func grepAskChunksForMode(path string, data []byte, overlap bool) []grepAskChunk {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	type span struct{ start, end int }
	var spans []span
	if strings.HasSuffix(path, ".go") {
		set := gotoken.NewFileSet()
		file, err := parser.ParseFile(set, path, data, parser.ParseComments)
		if err == nil {
			for _, decl := range file.Decls {
				start, end := set.Position(decl.Pos()).Line, set.Position(decl.End()).Line
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Doc != nil {
					start = set.Position(fn.Doc.Pos()).Line
				}
				if gen, ok := decl.(*ast.GenDecl); ok && gen.Doc != nil {
					start = set.Position(gen.Doc.Pos()).Line
				}
				if start > 0 && end >= start {
					step := grepAskChunkLines
					if overlap {
						step = max(1, grepAskChunkLines-grepAskChunkOverlapLines)
					}
					for n := start; n <= end; n += step {
						spans = append(spans, span{n, min(end, n+grepAskChunkLines-1)})
						if n+grepAskChunkLines-1 >= end {
							break
						}
					}
				}
			}
		}
	}
	if len(spans) == 0 {
		step := grepAskChunkLines
		if overlap {
			step = max(1, grepAskChunkLines-grepAskChunkOverlapLines)
		}
		for i := 1; i <= len(lines); i += step {
			spans = append(spans, span{i, min(len(lines), i+grepAskChunkLines-1)})
			if i+grepAskChunkLines-1 >= len(lines) {
				break
			}
		}
	}
	var chunks []grepAskChunk
	for _, sp := range spans {
		if sp.end > len(lines) {
			continue
		}
		excerpt := lines[sp.start-1 : sp.end]
		terms := map[string]int{}
		for _, term := range grepAskTokens(strings.Join(excerpt, "\n")) {
			terms[term]++
		}
		size := 0
		for _, count := range terms {
			size += count
		}
		chunks = append(chunks, grepAskChunk{rankPath: filepath.ToSlash(path), result: grepAskResult{Path: path, Start: sp.start, End: sp.end}, terms: terms, size: size, lines: excerpt})
	}
	return chunks
}

func grepAskScore(query string, chunks []grepAskChunk) []grepAskResult {
	stop := map[string]bool{"a": true, "an": true, "the": true, "is": true, "are": true, "of": true, "for": true, "in": true, "on": true, "to": true, "where": true, "how": true, "what": true, "does": true, "and": true}
	unique := map[string]bool{}
	var terms []string
	for _, t := range grepAskTokens(query) {
		if !stop[t] && !unique[t] {
			unique[t] = true
			terms = append(terms, t)
		}
	}
	if len(terms) > 32 {
		terms = terms[:32]
	}
	if len(terms) == 0 {
		return []grepAskResult{}
	}
	df := map[string]int{}
	total := 0
	for _, chunk := range chunks {
		total += chunk.size
		pathTerms := map[string]bool{}
		for _, t := range grepAskTokens(chunk.rankPath) {
			pathTerms[t] = true
		}
		for _, t := range terms {
			if chunk.terms[t] > 0 || pathTerms[t] {
				df[t]++
			}
		}
	}
	avg := float64(total) / float64(max(len(chunks), 1))
	if avg < 1 {
		avg = 1
	}
	var results []grepAskResult
	for _, chunk := range chunks {
		pathTokens := map[string]bool{}
		for _, t := range grepAskTokens(chunk.rankPath) {
			pathTokens[t] = true
		}
		score := 0.0
		for _, t := range terms {
			freq := float64(chunk.terms[t])
			if pathTokens[t] {
				freq += 2
			}
			if freq == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(chunks)-df[t])+0.5)/(float64(df[t])+0.5))
			score += idf * freq * 2.2 / (freq + 1.2*(0.25+0.75*float64(chunk.size)/avg))
		}
		if score == 0 {
			continue
		}
		start := 0
		for i, line := range chunk.lines {
			found := false
			for _, t := range grepAskTokens(line) {
				if unique[t] {
					found = true
					break
				}
			}
			if found {
				start = i
				break
			}
		}
		end := min(len(chunk.lines), start+3)
		snippetLines := make([]string, 0, end-start)
		for _, line := range chunk.lines[start:end] {
			runes := []rune(line)
			if len(runes) > 300 {
				line = string(runes[:300]) + "…"
			}
			snippetLines = append(snippetLines, line)
		}
		snippet := strings.Join(snippetLines, "\n")
		result := chunk.result
		result.Score = score
		result.Snippet = snippet
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].Path != results[j].Path {
			return results[i].Path < results[j].Path
		}
		return results[i].Start < results[j].Start
	})
	return results
}

func grepAskRank(query string, chunks []grepAskChunk, limit int) []grepAskResult {
	results := grepAskScore(query, chunks)
	selected := make([]grepAskResult, 0, min(limit, len(results)))
	perFile := map[string]int{}
	for _, r := range results {
		if perFile[r.Path] >= 2 {
			continue
		}
		selected = append(selected, r)
		perFile[r.Path]++
		if len(selected) == limit {
			break
		}
	}
	return selected
}

func grepAsk(paths []string, options grepOptions, stdout, stderr io.Writer) int {
	if strings.TrimSpace(options.pattern) == "" {
		fmt.Fprintln(stderr, T("grep.semantic.empty_query"))
		return 2
	}
	if len(grepAskTokens(options.pattern)) > 32 {
		fmt.Fprintln(stderr, T("grep.ask.query_limit"))
		return 2
	}
	if options.semantic {
		if len(paths) == 0 {
			paths = []string{"."}
		}
	}
	chunks, skipped, problems := grepAskCollect(paths, options)
	for _, problem := range problems {
		fmt.Fprintln(stderr, T("grep.failed", problem))
	}
	var results []grepAskResult
	method := "keyword"
	if options.semantic {
		if len(problems) > 0 {
			return 2
		}
		var err error
		results, err = grepSemanticRankScopes(paths, options, stderr)
		if err != nil {
			fmt.Fprintln(stderr, T("grep.failed", err))
			return 2
		}
		method = "semantic+keyword"
	} else {
		results = grepAskRank(options.pattern, chunks, options.askLimit)
	}
	if options.askJSON {
		if err := json.NewEncoder(stdout).Encode(grepAskOutput{Method: method, Results: results, Skipped: skipped}); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	} else {
		var writeErr error
		write := func(format string, values ...interface{}) {
			if writeErr == nil {
				_, writeErr = fmt.Fprintf(stdout, format, values...)
			}
		}
		terminal := false
		if file, ok := stdout.(*os.File); ok {
			terminal = isTerminal(file)
		}
		if options.semantic {
			write("%s\n", T("grep.semantic.method"))
		} else {
			write("%s\n", T("grep.ask.method"))
		}
		for _, r := range results {
			path := r.Path
			if terminal {
				path = viewSanitize(path)
			}
			write("%s:%d-%d\n", path, r.Start, r.End)
			for _, line := range strings.Split(r.Snippet, "\n") {
				write("  %s\n", viewSanitize(line))
			}
		}
		if skipped > 0 {
			fmt.Fprintln(stderr, T("grep.ask.skipped", skipped))
		}
		if writeErr != nil {
			fmt.Fprintln(stderr, writeErr)
			return 2
		}
	}
	if len(problems) > 0 {
		return 2
	}
	if len(results) == 0 {
		return 1
	}
	return 0
}

func grepSemanticRankScopes(paths []string, options grepOptions, stderr io.Writer) ([]grepAskResult, error) {
	if options.semanticInput == nil {
		options.semanticInput = strings.NewReader("")
	}
	if err := grepSemanticEnsure(options.semanticInput, options.semanticTerminal, options.semanticYes, stderr); err != nil {
		return nil, err
	}
	scopes, err := grepSearchScopes(paths)
	if err != nil {
		return nil, err
	}
	var results []grepAskResult
	var candidates []grepSemanticCandidate
	query := &grepSemanticQuery{text: options.pattern}
	for _, scope := range scopes {
		chunks, _, problems := grepAskCollect([]string{scope.path}, options)
		if len(problems) > 0 {
			return nil, errors.New(strings.Join(problems, "; "))
		}
		prepared, err := grepSemanticPrepareAuto(scope.path, options, chunks, query, stderr)
		if err != nil {
			return nil, err
		}
		for i := range prepared {
			prepared[i].displayPath = grepSearchDisplayPath(prepared[i].chunk.result.Path, []grepSearchScope{scope})
		}
		candidates = append(candidates, prepared...)
	}
	results = grepSemanticScoreCandidates(options, candidates)
	if len(results) > options.askLimit {
		results = results[:options.askLimit]
	}
	return results, nil
}

func grepSemanticSearch(options searchOptions, input io.Reader, terminal bool, stdout, stderr io.Writer) int {
	scopes, scopeProblems := grepSearchScopesPartial(options.paths)
	complete := len(scopeProblems) == 0
	for _, problem := range scopeProblems {
		fmt.Fprintln(stderr, T("grep.failed", problem))
	}
	if len(scopes) == 0 {
		return 2
	}
	if err := grepSemanticEnsure(input, terminal, options.yes, stderr); err != nil {
		if errors.Is(err, errGrepModelCancelled) {
			fmt.Fprintln(stderr, T("grep.model.cancelled"))
			return 4
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	var all []grepAskResult
	skipped := 0
	query := &grepSemanticQuery{text: options.query}
	for _, scope := range scopes {
		askOptions := grepOptions{pattern: options.query, askMode: true, semantic: true, askLimit: options.limit}
		chunks, skippedCount, problems := grepAskCollect([]string{scope.path}, askOptions)
		skipped += skippedCount
		if len(problems) > 0 {
			complete = false
			for _, problem := range problems {
				fmt.Fprintln(stderr, T("grep.failed", problem))
			}
		}
		results, rankErr := grepSemanticRankAuto(scope.path, askOptions, chunks, query, stderr)
		if rankErr != nil {
			complete = false
			fmt.Fprintln(stderr, T("grep.failed", rankErr))
			continue
		}
		all = append(all, results...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		return all[i].Start < all[j].Start
	})
	if len(all) > options.limit {
		all = all[:options.limit]
	}
	for i := range all {
		all[i].Path = sanitizeSearchText(grepSearchDisplayPath(all[i].Path, scopes))
		all[i].Snippet = sanitizeSearchText(all[i].Snippet)
	}
	if options.json {
		output := grepSearchOutput{SchemaVersion: 1, Method: "semantic+keyword", Query: options.query, Complete: complete, SkippedFiles: skipped, Results: all}
		for _, scope := range scopes {
			output.Scopes = append(output.Scopes, scope.display)
		}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	} else {
		for _, result := range all {
			if _, err := fmt.Fprintf(stdout, "%s:%d-%d\n", sanitizeSearchText(result.Path), result.Start, result.End); err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			for _, line := range strings.Split(result.Snippet, "\n") {
				if _, err := fmt.Fprintf(stdout, "  %s\n", sanitizeSearchText(line)); err != nil {
					fmt.Fprintln(stderr, err)
					return 2
				}
			}
		}
	}
	if !complete {
		return 2
	}
	if len(all) == 0 {
		return 1
	}
	return 0
}

func sanitizeSearchText(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			builder.WriteRune('�')
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// grepAskFile은 읽기를 마친 파일이다. problems와 skipped는 이 파일을 읽은 직후의 값이라, 청크 한도에 걸린 파일 뒤의 결과를 버릴 때 쓴다.
type grepAskFile struct {
	path     string
	data     []byte
	problems int
	skipped  int
	chunks   []grepAskChunk
}

// grepAskChunkFiles는 파일마다 청크를 병렬로 나눈다. 수집 시간의 대부분이 Go 파싱과 토큰화이기 때문이다.
func grepAskChunkFiles(files []grepAskFile, semantic bool) {
	jobs := make(chan int)
	var group sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(files)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := range jobs {
				files[i].chunks = grepAskChunksForMode(files[i].path, files[i].data, semantic)
				files[i].data = nil
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	group.Wait()
}

// grepAskWalkedPath는 path가 디렉터리 root 아래에서 찾은 파일이면 root 기준 상대 경로를 돌려준다. 파일 root로 직접 지정한 path는 디렉터리 아래에 있어도 찾은 파일로 보지 않는다.
func grepAskWalkedPath(roots []grepPathRoot, path string) (string, bool) {
	for _, root := range roots {
		if !root.directory && filepath.Clean(root.path) == filepath.Clean(path) {
			return "", false
		}
	}
	for _, root := range roots {
		if !root.directory {
			continue
		}
		relative, err := filepath.Rel(root.path, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return relative, true
		}
	}
	return "", false
}

// grepAskExcludedPath는 lock 파일과 testdata 아래 fixture를 고른다. testdata의 .md와 .txt는 사람이 쓴 설명이라 남긴다.
func grepAskExcludedPath(relative string) bool {
	if grepAskLockFiles[filepath.Base(relative)] {
		return true
	}
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "testdata" {
			extension := filepath.Ext(relative)
			return extension != ".md" && extension != ".txt"
		}
	}
	return false
}

func grepAskGenerated(data []byte) bool {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	return grepAskGeneratedHeader.Match(line)
}

func grepAskCollect(paths []string, options grepOptions) ([]grepAskChunk, int, []string) {
	targets, _, problems := grepCodeTargets(paths, options)
	roots := grepPathRoots(paths)
	var files []grepAskFile
	skipped := 0
	scanned := 0
	for _, path := range targets {
		relative, walked := grepAskWalkedPath(roots, path)
		if walked && grepAskExcludedPath(relative) {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		before, err := file.Stat()
		if err != nil {
			file.Close()
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if before.Size() > grepAskMaxFileBytes {
			file.Close()
			skipped++
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, grepAskMaxFileBytes+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if err == nil {
			err = statErr
		}
		if err == nil {
			err = closeErr
		}
		if err == nil {
			current, currentErr := os.Stat(path)
			if currentErr != nil {
				err = currentErr
			} else if !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Size() != current.Size() || !before.ModTime().Equal(current.ModTime()) {
				err = errors.New(T("grep.ask.changed"))
			}
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if len(data) > grepAskMaxFileBytes || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			skipped++
			continue
		}
		if walked && grepAskGenerated(data) {
			continue
		}
		scanned += len(data)
		if scanned > grepAskMaxScanBytes {
			problems = append(problems, T("grep.ask.budget"))
			break
		}
		files = append(files, grepAskFile{path: path, data: data, problems: len(problems), skipped: skipped})
	}
	grepAskChunkFiles(files, options.semantic)
	var chunks []grepAskChunk
	for _, file := range files {
		path, fileChunks := file.path, file.chunks
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
			for i := range fileChunks {
				fileChunks[i].rankPath = filepath.ToSlash(relative)
			}
			break
		}
		chunkLimit := grepAskMaxChunks
		if options.semantic {
			chunkLimit = grepAskSemanticMaxChunks
		}
		if len(chunks)+len(fileChunks) > chunkLimit {
			// 파일을 하나씩 처리하던 때처럼 이 파일 뒤에서 생긴 문제와 건너뛴 파일은 결과에 넣지 않는다.
			return chunks, file.skipped, append(problems[:file.problems], T("grep.ask.budget"))
		}
		chunks = append(chunks, fileChunks...)
	}
	return chunks, skipped, problems
}
