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
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const grepAskMaxFileBytes = 2 << 20
const grepAskChunkLines = 32
const grepAskMaxScanBytes = 32 << 20
const grepAskMaxChunks = 50000

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
					for n := start; n <= end; n += grepAskChunkLines {
						spans = append(spans, span{n, min(end, n+grepAskChunkLines-1)})
					}
				}
			}
		}
	}
	if len(spans) == 0 {
		for i := 1; i <= len(lines); i += grepAskChunkLines {
			spans = append(spans, span{i, min(len(lines), i+grepAskChunkLines-1)})
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
	displayRoot := ""
	if options.semantic {
		root, err := grepSemanticRoot(paths)
		if err != nil {
			fmt.Fprintln(stderr, T("grep.failed", err))
			return 2
		}
		displayRoot = paths[0]
		indexPath, err := grepSemanticPath(root)
		if err != nil {
			fmt.Fprintln(stderr, T("grep.failed", err))
			return 2
		}
		if _, err := os.Stat(indexPath); err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintln(stderr, T("grep.semantic.init_required"))
			} else {
				fmt.Fprintln(stderr, T("grep.failed", err))
			}
			return 2
		}
		paths = []string{root}
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
		results, err = grepSemanticRank(paths, options, chunks, stderr)
		if err != nil {
			fmt.Fprintln(stderr, T("grep.failed", err))
			return 2
		}
		for i := range results {
			if relative, err := filepath.Rel(paths[0], results[i].Path); err == nil {
				results[i].Path = filepath.Join(displayRoot, relative)
			}
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

func grepAskCollect(paths []string, options grepOptions) ([]grepAskChunk, int, []string) {
	targets, _, problems := grepCodeTargets(paths, options)
	roots := grepPathRoots(paths)
	var chunks []grepAskChunk
	skipped := 0
	scanned := 0
	for _, path := range targets {
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
		scanned += len(data)
		if scanned > grepAskMaxScanBytes {
			problems = append(problems, T("grep.ask.budget"))
			break
		}
		fileChunks := grepAskChunks(path, data)
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
		if len(chunks)+len(fileChunks) > grepAskMaxChunks {
			problems = append(problems, T("grep.ask.budget"))
			break
		}
		chunks = append(chunks, fileChunks...)
	}
	return chunks, skipped, problems
}
