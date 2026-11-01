package edc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGrepSearchScopesCanonicalizesAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(child, "one.go")
	if err := os.WriteFile(file, []byte("package p"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	scopes, err := grepSearchScopes([]string{link, child, root, file, file})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0].path != canonicalRoot || !scopes[0].directory {
		t.Fatalf("scopes: %+v", scopes)
	}
}

func TestGrepSearchOptionsAndJSONOutput(t *testing.T) {
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "search-test", Vectors: vectors}, nil
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth.go"), []byte("package p\nfunc Authenticate() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options, err := parseSearchOptions([]string{"--json", "-m", "1", "authentication", "--", root}, io.Discard)
	if err != nil || options.limit != 1 || options.query != "authentication" || len(options.paths) != 1 {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	var stdout, stderr bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var output grepSearchOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.SchemaVersion != 1 || output.Query != "authentication" || !output.Complete || len(output.Results) != 1 {
		t.Fatalf("output=%+v", output)
	}
	if len(output.Scopes) != 1 || output.Scopes[0] != root || output.Results[0].Path != filepath.Join(root, "auth.go") || output.Results[0].Start != 2 || output.Results[0].End != 2 {
		t.Fatalf("schema fields=%+v", output)
	}
	var repeat bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &repeat, io.Discard); code != 0 || !bytes.Equal(stdout.Bytes(), repeat.Bytes()) {
		t.Fatalf("JSON is not byte-stable: first=%q repeat=%q", stdout.String(), repeat.String())
	}
}

func TestGrepSearchInitAndFilesUseQueryChunkBoundaries(t *testing.T) {
	oldRoot, oldEnsure, oldEncode := grepSemanticCacheRoot, grepSemanticEnsure, grepSemanticEncode
	defer func() { grepSemanticCacheRoot, grepSemanticEnsure, grepSemanticEncode = oldRoot, oldEnsure, oldEncode }()
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	var calls int
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		calls++
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "lifecycle-chunks", Vectors: vectors}, nil
	}
	root := t.TempDir()
	lines := make([]string, 64)
	for i := range lines {
		lines[i] = "searchable content"
	}
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := grepSemanticLifecycleWithInput([]string{"--semantic-init", root}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("init=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if calls != 1 {
		t.Fatalf("init calls=%d", calls)
	}
	options := searchOptions{query: "searchable", limit: 4, paths: []string{root}}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("search=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if calls != 2 {
		t.Fatalf("search re-embedded unchanged chunks: calls=%d", calls)
	}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticLifecycleWithInput([]string{"--semantic-files", root}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("files=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "3/3\tlong.txt\n") {
		t.Fatalf("files=%q", stdout.String())
	}
}

func TestGrepSearchJSONSanitizesControlCharacterPaths(t *testing.T) {
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "path-sanitize", Vectors: vectors}, nil
	}
	root := t.TempDir()
	path := filepath.Join(root, "line\nbreak.go")
	if err := os.WriteFile(path, []byte("package p\nfunc Searchable() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := grepSemanticSearch(searchOptions{query: "searchable", limit: 1, json: true, paths: []string{root}}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var output grepSearchOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || strings.ContainsRune(output.Results[0].Path, '\n') || !strings.ContainsRune(output.Results[0].Path, '�') {
		t.Fatalf("path=%q", output.Results[0].Path)
	}
}

func TestGrepSearchParsesQuotedQueryAndBarePaths(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, dir := range []string{left, right} {
		if err := os.WriteFile(filepath.Join(dir, "match.go"), []byte("package p\nfunc Match() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	options, err := parseSearchOptions([]string{"-m", "8", "request admission handling", left, right}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.query != "request admission handling" || !reflect.DeepEqual(options.paths, []string{left, right}) {
		t.Fatalf("parsed options=%+v", options)
	}
	missingOptions, err := parseSearchOptions([]string{"quoted query", "missing"}, io.Discard)
	if err != nil || len(missingOptions.paths) != 1 {
		t.Fatalf("nonexistent trailing path was not preserved: %+v %v", missingOptions, err)
	}
	if _, err := grepSearchScopes(missingOptions.paths); err == nil {
		t.Fatal("nonexistent trailing path was not rejected")
	}
}

func TestGrepSemanticMultipleScopesLifecycleWarmAndCorruptRebuild(t *testing.T) {
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	var calls int
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		calls++
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "lifecycle-v1", Vectors: vectors}, nil
	}
	left, right := t.TempDir(), t.TempDir()
	for dir, name := range map[string]string{left: "left.go", right: "right.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\nfunc Searchable() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	options := searchOptions{query: "searchable", limit: 8, json: true, paths: []string{right, left}}
	var firstOut, firstErr bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &firstOut, &firstErr); code != 0 {
		t.Fatalf("cold search code=%d stderr=%s", code, firstErr.String())
	}
	firstCalls := calls
	var secondOut, secondErr bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &secondOut, &secondErr); code != 0 {
		t.Fatalf("warm search code=%d stderr=%s", code, secondErr.String())
	}
	if calls != firstCalls+2 {
		t.Fatalf("warm query should encode one query per scope, calls=%d first=%d", calls, firstCalls)
	}
	var firstJSON, secondJSON grepSearchOutput
	if err := json.Unmarshal(firstOut.Bytes(), &firstJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(secondOut.Bytes(), &secondJSON); err != nil {
		t.Fatal(err)
	}
	wantScopes := append([]string{left, right}, "")[:2]
	sort.Strings(wantScopes)
	if !reflect.DeepEqual(firstJSON.Results, secondJSON.Results) || !reflect.DeepEqual(firstJSON.Scopes, wantScopes) {
		t.Fatalf("unstable result first=%+v second=%+v", firstJSON, secondJSON)
	}
	canonical, err := filepath.EvalSymlinks(left)
	if err != nil {
		t.Fatal(err)
	}
	indexPath, err := grepSemanticPath(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	var rebuildOut, rebuildErr bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &rebuildOut, &rebuildErr); code != 0 {
		t.Fatalf("rebuild code=%d stderr=%s", code, rebuildErr.String())
	}
	if _, err := grepSemanticRead(indexPath, canonical); err != nil {
		t.Fatalf("rebuild did not restore index: %v", err)
	}
}

func TestGrepSemanticMultipleScopesConcurrentNoDeadlock(t *testing.T) {
	const lifecycleTimeout = 5 * time.Second
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "concurrent-v1", Vectors: vectors}, nil
	}
	paths := []string{t.TempDir(), t.TempDir()}
	for _, dir := range paths {
		if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package p\nfunc X() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	options := searchOptions{query: "x", limit: 4, paths: paths}
	done := make(chan int, 2)
	for range 2 {
		go func() {
			var out, errOut bytes.Buffer
			done <- grepSemanticSearch(options, strings.NewReader(""), false, &out, &errOut)
		}()
	}
	for range 2 {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("concurrent search code=%d", code)
			}
		case <-time.After(lifecycleTimeout):
			t.Fatal("concurrent search deadlocked")
		}
	}
}

func TestGrepSemanticMultipleScopesGlobalFusionIsOrderIndependent(t *testing.T) {
	makeCandidate := func(path string, similarity float32) grepSemanticCandidate {
		chunk := grepAskChunk{rankPath: path, result: grepAskResult{Path: path, Start: 1, End: 1}, terms: map[string]int{"unrelated": 1}, size: 1, lines: []string{"unrelated"}}
		index := &grepSemanticIndex{Vectors: map[string][]float32{grepSemanticKey(".", chunk): append([]float32{similarity}, make([]float32, grepSemanticDimensions-1)...)}}
		return grepSemanticCandidate{root: ".", chunk: chunk, index: index, query: append([]float32{1}, make([]float32, grepSemanticDimensions-1)...), displayPath: path}
	}
	options := grepOptions{pattern: "global query", askLimit: 1}
	first := grepSemanticScoreCandidates(options, []grepSemanticCandidate{makeCandidate("weak.go", 0.8), makeCandidate("strong.go", 1)})
	second := grepSemanticScoreCandidates(options, []grepSemanticCandidate{makeCandidate("strong.go", 1), makeCandidate("weak.go", 0.8)})
	if len(first) != 1 || len(second) != 1 || first[0].Path != "strong.go" || second[0].Path != "strong.go" {
		t.Fatalf("scope order changed global rank: first=%+v second=%+v", first, second)
	}
}

func TestGrepSemanticSelectionDeduplicatesCanonicalOverlappingIntervals(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "source.go")
	if err := os.WriteFile(file, []byte(strings.Repeat("package p\n", 12)), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.go")
	if err := os.Symlink(file, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	canonicalFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		t.Fatal(err)
	}
	vector := append([]float32{1}, make([]float32, grepSemanticDimensions-1)...)
	makeCandidate := func(path string, start, end int) grepSemanticCandidate {
		chunk := grepAskChunk{
			rankPath: path,
			result:   grepAskResult{Path: path, Start: start, End: end},
			terms:    map[string]int{"unrelated": 1},
			size:     1,
			lines:    []string{"unrelated"},
		}
		return grepSemanticCandidate{
			root:  ".",
			chunk: chunk,
			index: &grepSemanticIndex{Vectors: map[string][]float32{grepSemanticKey(".", chunk): vector}},
			query: vector,
		}
	}
	options := grepOptions{pattern: "unrelated", askLimit: 4}
	results := grepSemanticScoreCandidates(options, []grepSemanticCandidate{
		makeCandidate(file, 1, 3),
		makeCandidate(alias, 2, 4),
		makeCandidate(alias, 8, 9),
		makeCandidate(file, 9, 10),
	})
	if len(results) != 2 {
		t.Fatalf("canonical overlapping intervals were not deduplicated: %+v", results)
	}
	for i, result := range results {
		if i > 0 && result.Start <= results[i-1].End {
			t.Fatalf("selected intervals overlap: %+v", results)
		}
		if got := grepSemanticFileIdentity(result.Path); got != filepath.Clean(canonicalFile) {
			t.Fatalf("result %q resolved to %q, want %q", result.Path, got, filepath.Clean(canonicalFile))
		}
	}
}

func TestGrepSemanticSelectionUsesOneChunkPerFileBeforeSecondPass(t *testing.T) {
	vector := append([]float32{1}, make([]float32, grepSemanticDimensions-1)...)
	makeCandidate := func(path string, start int, similarity float32) grepSemanticCandidate {
		chunk := grepAskChunk{rankPath: path, result: grepAskResult{Path: path, Start: start, End: start}, terms: map[string]int{"needle": 1}, size: 1, lines: []string{"needle"}}
		value := append([]float32{similarity}, make([]float32, grepSemanticDimensions-1)...)
		return grepSemanticCandidate{root: ".", chunk: chunk, index: &grepSemanticIndex{Vectors: map[string][]float32{grepSemanticKey(".", chunk): value}}, query: vector}
	}
	results := grepSemanticScoreCandidates(grepOptions{pattern: "needle", askLimit: 3}, []grepSemanticCandidate{
		makeCandidate("a.go", 1, 1),
		makeCandidate("a.go", 10, 0.99),
		makeCandidate("b.go", 1, 0.98),
	})
	if len(results) != 3 || results[0].Path != "a.go" || results[1].Path != "b.go" || results[2].Path != "a.go" {
		t.Fatalf("soft diversity order=%+v", results)
	}
}

func TestGrepSemanticFusionUsesCanonicalRankIdentityWithDisplayPath(t *testing.T) {
	makeCandidate := func(path, displayPath string, similarity float32, hasKeyword bool) grepSemanticCandidate {
		terms := map[string]int{}
		if hasKeyword {
			terms["needle"] = 1
		}
		chunk := grepAskChunk{
			rankPath: path,
			result:   grepAskResult{Path: path, Start: 1, End: 1},
			terms:    terms,
			size:     1,
			lines:    []string{path},
		}
		vector := append([]float32{similarity}, make([]float32, grepSemanticDimensions-1)...)
		return grepSemanticCandidate{
			root:        ".",
			chunk:       chunk,
			index:       &grepSemanticIndex{Vectors: map[string][]float32{grepSemanticKey(".", chunk): vector}},
			query:       append([]float32{1}, make([]float32, grepSemanticDimensions-1)...),
			displayPath: displayPath,
		}
	}
	options := grepOptions{pattern: "needle", askLimit: 1}
	results := grepSemanticScoreCandidates(options, []grepSemanticCandidate{
		makeCandidate("canonical.go", "shown/canonical.go", 0.7, true),
		makeCandidate("other.go", "shown/other.go", 0.72, false),
	})
	if len(results) != 1 || results[0].Path != "shown/canonical.go" {
		t.Fatalf("display path changed rank fusion: %+v", results)
	}
}

func TestGrepSemanticPathCoverageUsesFileTokens(t *testing.T) {
	if got := grepSemanticPathCoverage("format HTTP response", "internal/edc/http_rich.go"); got <= 0 {
		t.Fatalf("path coverage=%v", got)
	}
	if got := grepSemanticPathCoverage("file system watch event", "internal/edc/watch_fs.go"); got <= 0 {
		t.Fatalf("path coverage=%v", got)
	}
}

func TestGrepSemanticSourceBiasKeepsAuxiliarySourcesBounded(t *testing.T) {
	if got := grepSemanticSourceBias("normal production query", "internal/edc/grep.go"); got != 0 {
		t.Fatalf("production source bias=%v", got)
	}
	if got := grepSemanticSourceBias("normal production query", "internal/edc/docs/guide.md"); got != -1 {
		t.Fatalf("auxiliary source bias=%v", got)
	}
}

func TestGrepAskSemanticMultiPathAutomaticallyBuildsIndex(t *testing.T) {
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "legacy-auto", Vectors: vectors}, nil
	}
	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "left.go"), []byte("package p\nfunc Left() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, "right.go"), []byte("package p\nfunc Right() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	options := grepOptions{pattern: "function", askMode: true, semantic: true, askJSON: true, askLimit: 8, semanticInput: strings.NewReader(""), semanticTerminal: false}
	var out, errOut bytes.Buffer
	if code := grepAsk([]string{left, right}, options, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "left.go") || !strings.Contains(out.String(), "right.go") {
		t.Fatalf("multi-path output=%s", out.String())
	}
}

func TestGrepAskSemanticMissingModelNonTTYDoesNotDownload(t *testing.T) {
	oldEnsure := grepSemanticEnsure
	defer func() { grepSemanticEnsure = oldEnsure }()
	called := false
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error {
		called = true
		return errors.New("No terminal; use edc search index init --yes")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package p\nfunc X() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	options := grepOptions{pattern: "x", askMode: true, semantic: true, askJSON: true, askLimit: 1, semanticInput: strings.NewReader(""), semanticTerminal: false}
	if code := grepAsk([]string{root}, options, &out, &errOut); code != 2 || !called || out.Len() != 0 || !strings.Contains(errOut.String(), "edc search index init --yes") {
		t.Fatalf("code=%d called=%v out=%q err=%q", code, called, out.String(), errOut.String())
	}
}

func TestGrepSemanticOutputMarksIncompleteAndSkippedFiles(t *testing.T) {
	oldEnsure, oldEncode, oldCache := grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot
	defer func() { grepSemanticEnsure, grepSemanticEncode, grepSemanticCacheRoot = oldEnsure, oldEncode, oldCache }()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, grepSemanticDimensions)
		}
		return grepSemanticEncoded{Revision: "incomplete-v1", Vectors: vectors}, nil
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package p\nfunc OK() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0, 1, 2}, 0600); err != nil {
		t.Fatal(err)
	}
	options := searchOptions{query: "ok", limit: 4, json: true, paths: []string{root}}
	var out, errOut bytes.Buffer
	if code := grepSemanticSearch(options, strings.NewReader(""), false, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var output grepSearchOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if !output.Complete || output.SkippedFiles != 1 {
		t.Fatalf("complete=%v skipped=%d stderr=%s", output.Complete, output.SkippedFiles, errOut.String())
	}
}

func TestGrepSemanticPartialScopeJSONOutput(t *testing.T) {
	grepSemanticTestSetup(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package p\nfunc OK() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	var out, errOut bytes.Buffer
	code := grepSemanticSearch(searchOptions{query: "ok", limit: 4, json: true, paths: []string{root, missing}}, strings.NewReader(""), false, &out, &errOut)
	var output grepSearchOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if code != 2 || output.Complete || len(output.Scopes) != 1 || len(output.Results) == 0 || !strings.Contains(errOut.String(), missing) {
		t.Fatalf("code=%d output=%+v stderr=%q", code, output, errOut.String())
	}
}

func TestGrepSemanticJSONOutputFailure(t *testing.T) {
	grepSemanticTestSetup(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.go"), []byte("package p\nfunc OK() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	code := grepSemanticSearch(searchOptions{query: "ok", limit: 4, json: true, paths: []string{root}}, strings.NewReader(""), false, grepAskErrorWriter{}, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "write failed") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}

func TestGrepSemanticLifecycleAndIncrementalUpdates(t *testing.T) {
	oldRoot, oldEncode, oldEnsure := grepSemanticCacheRoot, grepSemanticEncode, grepSemanticEnsure
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepSemanticEncode = oldEncode
		grepSemanticEnsure = oldEnsure
	}()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	root := t.TempDir()
	file := filepath.Join(root, "auth.go")
	if err := os.WriteFile(file, []byte("package app\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		calls = append(calls, append([]string(nil), texts...))
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, 384)
			out[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "test-revision", Vectors: out}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSearchWithInput([]string{"index", "init", root}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("init=%d %s", code, stderr.String())
	}
	if len(calls) != 1 || len(calls[0]) != 1 || !strings.Contains(calls[0][0], "auth.go") {
		t.Fatalf("init calls %v", calls)
	}
	canonical, _ := grepSemanticRoot([]string{root})
	indexPath, _ := grepSemanticPath(canonical)
	beforeInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	calls = nil
	code, out, errout := searchCommand(t, "", "--json", "credential verification", root)
	if code != 0 || !strings.Contains(out, `"method":"semantic+keyword"`) || !strings.Contains(out, "auth.go") {
		t.Fatalf("search %d %s %s", code, out, errout)
	}
	afterInfo, err := os.Stat(indexPath)
	if err != nil || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("unchanged index was rewritten")
	}
	if len(calls) != 1 || len(calls[0]) != 1 || calls[0][0] != "credential verification" {
		t.Fatalf("unchanged calls %v", calls)
	}
	if err := os.WriteFile(file, []byte("package app\nfunc NewToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls = nil
	code, _, errout = searchCommand(t, "", "token", root)
	if code != 0 || len(calls) != 1 || len(calls[0]) != 2 {
		t.Fatalf("update %d calls %v stderr %s", code, calls, errout)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package app\nfunc Changed() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	grepSemanticEncode = func([]string, io.Writer) (grepSemanticEncoded, error) {
		return grepSemanticEncoded{}, errors.New("backend failed")
	}
	code, _, errout = searchCommand(t, "", "token", root)
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 || !strings.Contains(errout, "backend failed") || !bytes.Equal(before, after) {
		t.Fatalf("failed update code=%d stderr=%s cache changed=%v", code, errout, !bytes.Equal(before, after))
	}
	stdout.Reset()
	stderr.Reset()
	if code := runSearchWithInput([]string{"index", "clear", root}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("clear %d %s", code, stderr.String())
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("index remains %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runSearchWithInput([]string{"index", "status", root}, strings.NewReader(""), false, &stdout, &stderr); code != 1 {
		t.Fatalf("status %d", code)
	}
}

func TestGrepSemanticRequiresInitializationWithoutWritingCache(t *testing.T) {
	oldRoot := grepSemanticCacheRoot
	defer func() { grepSemanticCacheRoot = oldRoot }()
	cache := filepath.Join(t.TempDir(), "unused")
	grepSemanticCacheRoot = cache
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x\nfunc X() {}"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := searchCommand(t, "", "x", root)
	if code != 2 || out != "" || !strings.Contains(stderr, "edc search index init") {
		t.Fatalf("code %d out %q stderr %q", code, out, stderr)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("cache created: %v", err)
	}
}

func TestGrepSemanticRejectsModelRevisionChanges(t *testing.T) {
	oldEncode := grepSemanticEncode
	defer func() { grepSemanticEncode = oldEncode }()
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, 384)
		}
		return grepSemanticEncoded{Revision: "new-revision", Vectors: out}, nil
	}
	index := &grepSemanticIndex{Revision: "old-revision", Vectors: map[string][]float32{}}
	_, _, _, err := grepSemanticSync(t.TempDir(), nil, index, "a query", io.Discard)
	if err == nil || index.Revision != "old-revision" || len(index.Vectors) != 0 {
		t.Fatalf("err %v index %+v", err, index)
	}
}

func TestGrepSemanticRejectsDamagedCache(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "index.gob")
	vec := make([]float32, 384)
	vec[0] = 1
	index := &grepSemanticIndex{Version: grepSemanticVersion, Root: root, Model: grepSemanticModel, Revision: "revision", Vectors: map[string][]float32{strings.Repeat("ab", 32): vec}}
	if err := grepSemanticSave(path, index); err != nil {
		t.Fatal(err)
	}
	if _, err := grepSemanticRead(path, root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := grepSemanticRead(path, root); err == nil {
		t.Fatal("corrupted cache was accepted")
	}
}

func TestGrepSemanticDoesNotSaveWhenFilesChangeDuringEmbedding(t *testing.T) {
	oldRoot, oldEncode, oldEnsure := grepSemanticCacheRoot, grepSemanticEncode, grepSemanticEnsure
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepSemanticEncode = oldEncode
		grepSemanticEnsure = oldEnsure
	}()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticCacheRoot = t.TempDir()
	root := t.TempDir()
	file := filepath.Join(root, "auth.go")
	if err := os.WriteFile(file, []byte("package x\nfunc Old() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		if err := os.WriteFile(file, []byte("package x\nfunc New() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = make([]float32, 384)
		}
		return grepSemanticEncoded{Revision: "test", Vectors: out}, nil
	}
	var stdout, stderr bytes.Buffer
	code := runSearchWithInput([]string{"index", "init", root}, strings.NewReader(""), false, &stdout, &stderr)
	canonical, _ := grepSemanticRoot([]string{root})
	path, _ := grepSemanticPath(canonical)
	if _, err := os.Stat(path); code != 2 || !os.IsNotExist(err) {
		t.Fatalf("code=%d cache error=%v stderr=%q", code, err, stderr.String())
	}
}

func TestGrepSemanticRejectsPreviousModelIndex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "old.gob")
	vector := make([]float32, 384)
	index := &grepSemanticIndex{Version: grepSemanticVersion, Root: root, Model: "intfloat/multilingual-e5-small", Revision: "old", Vectors: map[string][]float32{strings.Repeat("ab", 32): vector}}
	if err := grepSemanticSave(path, index); err != nil {
		t.Fatal(err)
	}
	if _, err := grepSemanticRead(path, root); err == nil || !strings.Contains(err.Error(), "edc search index init") {
		t.Fatalf("old model accepted or missing instruction: %v", err)
	}
}

func TestGrepSemanticReusesUnchangedChunksWhenLinesMove(t *testing.T) {
	oldRoot, oldEncode, oldEnsure := grepSemanticCacheRoot, grepSemanticEncode, grepSemanticEnsure
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepSemanticEncode = oldEncode
		grepSemanticEnsure = oldEnsure
	}()
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package p\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		calls = append(calls, append([]string(nil), texts...))
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, 384)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "revision", Vectors: vectors}, nil
	}
	var out, stderr bytes.Buffer
	if code := runSearchWithInput([]string{"index", "init", root}, strings.NewReader(""), false, &out, &stderr); code != 0 {
		t.Fatalf("init %d: %s", code, stderr.String())
	}
	if err := os.WriteFile(path, []byte("package p\n\n\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls = nil
	code, text, errtext := searchCommand(t, "", "--json", "validate token", root)
	if code != 0 || len(calls) != 1 || len(calls[0]) != 1 || !strings.Contains(text, `"start":4`) {
		t.Fatalf("code %d calls %v text %s stderr %s", code, calls, text, errtext)
	}
}

func TestGrepSemanticSkipsRecollectWhenNothingWasEmbedded(t *testing.T) {
	oldRoot, oldEncode, oldEnsure := grepSemanticCacheRoot, grepSemanticEncode, grepSemanticEnsure
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepSemanticEncode = oldEncode
		grepSemanticEnsure = oldEnsure
	}()
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package p\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	searching := false
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		if searching {
			if err := os.WriteFile(path, []byte("package p\nfunc Changed() {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, 384)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "revision", Vectors: vectors}, nil
	}
	var out, stderr bytes.Buffer
	if code := runSearchWithInput([]string{"index", "init", root}, strings.NewReader(""), false, &out, &stderr); code != 0 {
		t.Fatalf("init %d: %s", code, stderr.String())
	}
	searching = true
	code, text, errtext := searchCommand(t, "", "--json", "validate token", root)
	if code != 0 || !strings.Contains(text, "a.go") {
		t.Fatalf("code %d text %s stderr %s", code, text, errtext)
	}
}

func TestGrepRejectsRemovedSemanticFlags(t *testing.T) {
	oldRoot := grepSemanticCacheRoot
	defer func() { grepSemanticCacheRoot = oldRoot }()
	grepSemanticCacheRoot = t.TempDir()
	root := t.TempDir()
	code, out, stderr := grepCommand(t, "", "--semantic", "where is my function?", root)
	if code != 2 || out != "" || !strings.Contains(stderr, "flag provided") {
		t.Fatalf("code %d out %q stderr %q", code, out, stderr)
	}
}

func TestGrepSemanticFilesListsIndexedChunks(t *testing.T) {
	oldRoot, oldEncode, oldEnsure := grepSemanticCacheRoot, grepSemanticEncode, grepSemanticEnsure
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepSemanticEncode = oldEncode
		grepSemanticEnsure = oldEnsure
	}()
	grepSemanticCacheRoot = t.TempDir()
	grepSemanticEnsure = func(io.Reader, bool, bool, io.Writer) error { return nil }
	grepSemanticEncode = func(texts []string, _ io.Writer) (grepSemanticEncoded, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = make([]float32, 384)
			vectors[i][0] = 1
		}
		return grepSemanticEncoded{Revision: "revision", Vectors: vectors}, nil
	}
	root := t.TempDir()
	for name, content := range map[string]string{"a.go": "package p\nfunc A() {}\n", "docs/b.md": "# B\n"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out, stderr bytes.Buffer
	if code := runSearchWithInput([]string{"index", "files", root}, strings.NewReader(""), false, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "edc search index init") {
		t.Fatalf("before init %d %q", code, stderr.String())
	}
	if code := runSearchWithInput([]string{"index", "init", root}, strings.NewReader(""), false, &out, &stderr); code != 0 {
		t.Fatalf("init %d: %s", code, stderr.String())
	}
	if err := os.WriteFile(filepath.Join(root, "c.go"), []byte("package p\nfunc C() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := runSearchWithInput([]string{"index", "files", root}, strings.NewReader(""), false, &out, &stderr); code != 0 {
		t.Fatalf("files %d: %s", code, stderr.String())
	}
	for _, name := range []string{"tab\tname.go", "line\nbreak.go", "quote\".go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package p\nfunc T() {}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	stderr.Reset()
	if code := runSearchWithInput([]string{"index", "files", root}, strings.NewReader(""), false, &out, &stderr); code != 0 {
		t.Fatalf("files %d: %s", code, stderr.String())
	}
	if want := "1/1\ta.go\n0/1\tc.go\n1/1\tdocs/b.md\n0/1\t\"line\\nbreak.go\"\n0/1\t\"quote\\\".go\"\n0/1\t\"tab\\tname.go\"\n"; out.String() != want {
		t.Fatalf("got %q want %q", out.String(), want)
	}
	if action, args, ok := searchLifecycleArgs([]string{"index", "files", root}); !ok || action != "files" || len(args) != 1 || args[0] != root {
		t.Fatalf("lifecycle args %q %v %v", action, args, ok)
	}
	if code := runSearchWithInput([]string{"index", "files", "--yes", root}, strings.NewReader(""), false, &out, &stderr); code != 2 {
		t.Fatalf("--yes accepted: %d", code)
	}
}

func TestGrepSemanticStoresHalfPrecisionVectors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "index.bin")
	vector := make([]float32, 384)
	for i := range vector {
		vector[i] = float32(math.Sin(float64(i))) / 20
	}
	vector[1], vector[2] = 3e-7, -1
	key := strings.Repeat("0f", 32)
	index := &grepSemanticIndex{Version: grepSemanticVersion, Root: root, Model: grepSemanticModel, Revision: "revision", Vectors: map[string][]float32{key: vector}}
	if err := grepSemanticSave(path, index); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 36+1024+32+384*2 {
		t.Fatalf("size %v %v", info, err)
	}
	loaded, err := grepSemanticRead(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != "revision" || len(loaded.Vectors) != 1 {
		t.Fatalf("loaded %+v", loaded)
	}
	for i, x := range loaded.Vectors[key] {
		if diff := math.Abs(float64(x - vector[i])); diff > math.Max(math.Abs(float64(vector[i]))/1024, 6e-8) {
			t.Fatalf("dimension %d got %g want %g", i, x, vector[i])
		}
	}
	if err := grepSemanticSave(path, &grepSemanticIndex{Version: grepSemanticVersion, Root: root, Model: grepSemanticModel, Revision: "r", Vectors: map[string][]float32{"not-hex": vector}}); err == nil {
		t.Fatal("invalid key saved")
	}
}

func TestGrepSemanticRejectsGobIndex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "old.gob")
	if err := os.WriteFile(path, append([]byte("EDC1"), make([]byte, 64)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := grepSemanticRead(path, root); err == nil || !strings.Contains(err.Error(), "edc search index init") {
		t.Fatalf("old format accepted or missing instruction: %v", err)
	}
}
