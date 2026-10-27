//go:build (linux || darwin) && (amd64 || arm64)

package edc

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGrepONNXMatchesMiniLMReference(t *testing.T) {
	model, library, vocab := os.Getenv("EDC_TEST_ONNX_MODEL"), os.Getenv("EDC_TEST_ONNX_LIBRARY"), os.Getenv("EDC_TEST_ONNX_VOCAB")
	if model == "" || library == "" || vocab == "" {
		t.Skip("set EDC_TEST_ONNX_MODEL, EDC_TEST_ONNX_LIBRARY, and EDC_TEST_ONNX_VOCAB")
	}
	tokenizer, err := newGrepTokenizer(vocab)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		text string
		ids  []int64
	}{
		{"hello world", []int64{101, 7592, 2088, 102}},
		{"ValidateToken: token.Auth(ctx)", []int64{101, 9398, 3686, 18715, 2368, 1024, 19204, 1012, 8740, 2705, 1006, 14931, 2595, 1007, 102}},
		{"Café [MASK] 서울 🌍", []int64{101, 7668, 103, 1461, 30008, 29999, 30014, 30022, 100, 102}},
	} {
		if ids := tokenizer.encode(tc.text); !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("%q: got %v want %v", tc.text, ids, tc.ids)
		}
	}
	engine, err := newGrepONNX(library, model)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.close()
	out, err := engine.encode([][]int64{tokenizer.encode("hello world"), tokenizer.encode("ValidateToken: token.Auth(ctx)"), tokenizer.encode("hello world"), tokenizer.encode("Café [MASK] 서울 🌍")})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{-0.03447729, 0.03102328, 0.00673501, 0.02610899, -0.03936208, -0.16030249, 0.06692406, -0.00644133}
	if len(out) != 4 || len(out[0]) != 384 {
		t.Fatalf("output shape %d", len(out))
	}
	for i, value := range want {
		if delta := math.Abs(float64(out[0][i]) - value); delta > 0.00002 {
			t.Errorf("dimension %d got %.8f want %.8f", i, out[0][i], value)
		}
	}
	for row, expected := range map[int][]float64{
		1: {-0.06644844, 0.02141709, -0.00811831, -0.07270612, -0.02133382, -0.03537991, 0.07272128, 0.03060021},
		3: {-0.06382043, 0.01721764, 0.06237771, -0.00343661, 0.00350912, 0.07851864, 0.12559187, -0.01493837},
	} {
		for i, want := range expected {
			if math.Abs(float64(out[row][i])-want) > 0.00002 {
				t.Errorf("row %d dimension %d got %.8f want %.8f", row, i, out[row][i], want)
			}
		}
	}
	for i := range out[0] {
		if math.Abs(float64(out[0][i]-out[2][i])) > 0.00001 {
			t.Fatalf("duplicate encoding differs at %d", i)
		}
	}
}

func TestGrepNativeModelInstallAndSearch(t *testing.T) {
	modelPath, vocabPath, archivePath := os.Getenv("EDC_TEST_ONNX_MODEL"), os.Getenv("EDC_TEST_ONNX_VOCAB"), os.Getenv("EDC_TEST_ONNX_ARCHIVE")
	if modelPath == "" || vocabPath == "" || archivePath == "" {
		t.Skip("set EDC_TEST_ONNX_MODEL, EDC_TEST_ONNX_VOCAB, and EDC_TEST_ONNX_ARCHIVE")
	}
	oldRoot, oldManifest, oldClient := grepSemanticCacheRoot, grepModelManifest, grepModelHTTPClient
	defer func() {
		grepSemanticCacheRoot = oldRoot
		grepModelManifest = oldManifest
		grepModelHTTPClient = oldClient
	}()
	grepSemanticCacheRoot = filepath.Join(t.TempDir(), "cache")
	model, vocab, rt, err := grepModelAssets()
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{"/model": modelPath, "/vocab": vocabPath, "/runtime": archivePath}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		path, ok := sources[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	}))
	defer server.Close()
	model.url = server.URL + "/model"
	vocab.url = server.URL + "/vocab"
	rt.archive.url = server.URL + "/runtime"
	grepModelManifest = func() (grepModelAsset, grepModelAsset, grepRuntimeAsset, error) { return model, vocab, rt, nil }
	grepModelHTTPClient = server.Client()
	root := t.TempDir()
	path := filepath.Join(root, "auth.go")
	if err := os.WriteFile(path, []byte("package auth\n// ValidateToken verifies authentication tokens.\nfunc ValidateToken() bool { return true }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := grepSemanticLifecycleWithInput([]string{"--semantic-init", root}, strings.NewReader(""), false, &stdout, &stderr); code != 2 || requests.Load() != 0 {
		t.Fatalf("unexpected download without confirmation: %d calls=%d %s", code, requests.Load(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticLifecycleWithInput([]string{"--semantic-init", "--yes", root}, strings.NewReader(""), false, &stdout, &stderr); code != 0 || requests.Load() != 3 {
		t.Fatalf("init=%d calls=%d out=%s err=%s", code, requests.Load(), stdout.String(), stderr.String())
	}
	encoded, err := grepSemanticNative([]string{"hello world", "a considerably longer sentence to test reordered batches and padding", "hello world"}, io.Discard)
	if err != nil || len(encoded.Vectors) != 3 {
		t.Fatalf("native encoding: %v", err)
	}
	for i, value := range []float64{-0.03447729, 0.03102328, 0.00673501, 0.02610899, -0.03936208, -0.16030249, 0.06692406, -0.00644133} {
		if math.Abs(float64(encoded.Vectors[0][i])-value) > 0.00002 || math.Abs(float64(encoded.Vectors[2][i])-value) > 0.00002 {
			t.Fatalf("batch order/reference mismatch at %d", i)
		}
	}
	code, out, stderrText := grepCommand(t, "", "--ask", "authentication token verification", "--json", root)
	if code != 0 || !strings.Contains(out, `"method":"semantic+keyword"`) || !strings.Contains(out, "auth.go") || requests.Load() != 3 {
		t.Fatalf("search=%d out=%s err=%s calls=%d", code, out, stderrText, requests.Load())
	}
	if err := os.WriteFile(path, []byte("package auth\n// RenewToken refreshes login credentials.\nfunc RenewToken() bool { return true }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderrText = grepCommand(t, "", "--ask", "refresh login credentials", "--json", root)
	if code != 0 || !strings.Contains(out, "RenewToken") || requests.Load() != 3 {
		t.Fatalf("updated search=%d out=%s err=%s calls=%d", code, out, stderrText, requests.Load())
	}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticLifecycleWithInput([]string{"--model-clear"}, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("clear model=%d %s", code, stderr.String())
	}
	code, _, stderrText = grepCommand(t, "", "--ask", "login", root)
	if code != 2 || !strings.Contains(stderrText, "--semantic-init") || requests.Load() != 3 {
		t.Fatalf("missing model search=%d err=%s calls=%d", code, stderrText, requests.Load())
	}
	code, out, stderrText = grepCommand(t, "", "--ask", "--keyword", "login", "--json", root)
	if code != 0 || !strings.Contains(out, `"method":"keyword"`) || requests.Load() != 3 {
		t.Fatalf("keyword after model clear=%d out=%s err=%s calls=%d", code, out, stderrText, requests.Load())
	}
}

func TestGrepNativeOfficialDownload(t *testing.T) {
	if os.Getenv("EDC_TEST_ONLINE_MODEL") != "1" {
		t.Skip("set EDC_TEST_ONLINE_MODEL=1 to download the pinned public model and runtime")
	}
	oldRoot := grepSemanticCacheRoot
	defer func() { grepSemanticCacheRoot = oldRoot }()
	grepSemanticCacheRoot = filepath.Join(t.TempDir(), "cache")
	t.Setenv("PATH", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth.go"), []byte("package auth\n// Check login credentials.\nfunc VerifyToken() bool { return true }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output, stderr bytes.Buffer
	code := grepSemanticLifecycleWithInput([]string{"--semantic-init", "--yes", root}, strings.NewReader(""), false, &output, &stderr)
	if code != 0 {
		t.Fatalf("official init=%d output=%s stderr=%s", code, output.String(), stderr.String())
	}
	dir, err := grepModelDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := grepModelCheck(dir); err != nil {
		t.Fatal(err)
	}
	code, out, errout := grepCommand(t, "", "--ask", "login authentication", "--json", root)
	if code != 0 || !strings.Contains(out, `"method":"semantic+keyword"`) || !strings.Contains(out, "auth.go") {
		t.Fatalf("official search=%d output=%s stderr=%s", code, out, errout)
	}
}

func TestGrepNativeCorpusEvaluation(t *testing.T) {
	model, library, vocab := os.Getenv("EDC_TEST_ONNX_MODEL"), os.Getenv("EDC_TEST_ONNX_LIBRARY"), os.Getenv("EDC_TEST_ONNX_VOCAB")
	if os.Getenv("EDC_EVAL_GREP") != "1" || model == "" || library == "" || vocab == "" {
		t.Skip("set EDC_EVAL_GREP=1 and the EDC_TEST_ONNX_* paths")
	}
	raw, err := os.ReadFile("testdata/grep-ask/queries.json")
	if err != nil {
		t.Fatal(err)
	}
	var queries []struct {
		Query string `json:"query"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal(raw, &queries); err != nil {
		t.Fatal(err)
	}
	if len(queries) < 20 {
		t.Fatal("20 English queries required")
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	targets, _, problems := grepCodeTargets([]string{root}, grepOptions{})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	var chunks []grepAskChunk
	for _, path := range targets {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if rel == "grep_ask_eval_test.go" || strings.HasPrefix(filepath.ToSlash(rel), "testdata/grep-ask/") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > grepAskMaxFileBytes || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
			continue
		}
		fileChunks := grepAskChunks(path, data)
		for i := range fileChunks {
			fileChunks[i].rankPath = filepath.ToSlash(rel)
		}
		chunks = append(chunks, fileChunks...)
	}
	tokenizer, err := newGrepTokenizer(vocab)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := newGrepONNX(library, model)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.close()
	index := &grepSemanticIndex{Vectors: make(map[string][]float32, len(chunks))}
	start := time.Now()
	type tokenized struct {
		index int
		ids   []int64
	}
	items := make([]tokenized, len(chunks))
	for i, chunk := range chunks {
		text := strings.Join(chunk.lines, "\n")
		if runes := []rune(text); len(runes) > 3000 {
			text = string(runes[:3000])
		}
		items[i] = tokenized{index: i, ids: tokenizer.encode(chunk.rankPath + "\n" + text)}
	}
	sort.SliceStable(items, func(i, j int) bool { return len(items[i].ids) > len(items[j].ids) })
	if os.Getenv("EDC_EVAL_TOKEN_ONLY") == "1" {
		t.Logf("tokenized %d chunks in %s", len(items), time.Since(start).Round(time.Millisecond))
		return
	}
	for from := 0; from < len(items); from += 32 {
		end := min(from+32, len(items))
		batch := make([][]int64, 0, end-from)
		for _, item := range items[from:end] {
			batch = append(batch, item.ids)
		}
		vectors, err := engine.encode(batch)
		if err != nil {
			t.Fatal(err)
		}
		for i, vector := range vectors {
			index.Vectors[grepSemanticKey(root, chunks[items[from+i].index])] = vector
		}
	}
	elapsed := time.Since(start)
	hits := 0
	for _, q := range queries[:20] {
		query, err := engine.encode([][]int64{tokenizer.encode(q.Query)})
		if err != nil {
			t.Fatal(err)
		}
		results := grepSemanticScore(root, grepOptions{pattern: q.Query, askLimit: 5}, chunks, index, query[0])
		found := false
		for _, r := range results {
			rel, err := filepath.Rel(root, r.Path)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.ToSlash(filepath.Join("internal/edc", rel)) == q.Path {
				found = true
				break
			}
		}
		if found {
			hits++
		} else {
			t.Logf("MISS %q expected %s", q.Query, q.Path)
		}
	}
	t.Logf("native MiniLM hybrid top-5: %d/20; %d chunks; embedding %s", hits, len(chunks), elapsed.Round(time.Millisecond))
}
