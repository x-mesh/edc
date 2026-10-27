package edc

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	if code := grepSemanticLifecycle([]string{"--semantic-init", root}, &stdout, &stderr); code != 0 {
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
	code, out, errout := grepCommand(t, "", "--ask", "credential verification", "--json", root)
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
	code, _, errout = grepCommand(t, "", "--ask", "--semantic", "token", root)
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
	code, _, errout = grepCommand(t, "", "--ask", "--semantic", "token", root)
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if code != 2 || !strings.Contains(errout, "backend failed") || !bytes.Equal(before, after) {
		t.Fatalf("failed update code=%d stderr=%s cache changed=%v", code, errout, !bytes.Equal(before, after))
	}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticLifecycle([]string{"--semantic-clear", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("clear %d %s", code, stderr.String())
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("index remains %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := grepSemanticLifecycle([]string{"--semantic-status", root}, &stdout, &stderr); code != 1 {
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
	code, out, stderr := grepCommand(t, "", "--ask", "x", root)
	if code != 2 || out != "" || !strings.Contains(stderr, "--semantic-init") {
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
	index := &grepSemanticIndex{Version: 1, Root: root, Model: grepSemanticModel, Revision: "revision", Vectors: map[string][]float32{"key": vec}}
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
	code := grepSemanticLifecycle([]string{"--semantic-init", root}, &stdout, &stderr)
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
	index := &grepSemanticIndex{Version: 1, Root: root, Model: "intfloat/multilingual-e5-small", Revision: "old", Vectors: map[string][]float32{"key": vector}}
	if err := grepSemanticSave(path, index); err != nil {
		t.Fatal(err)
	}
	if _, err := grepSemanticRead(path, root); err == nil || !strings.Contains(err.Error(), "--semantic-init") {
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
	if code := grepSemanticLifecycle([]string{"--semantic-init", root}, &out, &stderr); code != 0 {
		t.Fatalf("init %d: %s", code, stderr.String())
	}
	if err := os.WriteFile(path, []byte("package p\n\n\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls = nil
	code, text, errtext := grepCommand(t, "", "--ask", "validate token", "--json", root)
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
	if code := grepSemanticLifecycle([]string{"--semantic-init", root}, &out, &stderr); code != 0 {
		t.Fatalf("init %d: %s", code, stderr.String())
	}
	searching = true
	code, text, errtext := grepCommand(t, "", "--ask", "validate token", "--json", root)
	if code != 0 || !strings.Contains(text, "a.go") {
		t.Fatalf("code %d text %s stderr %s", code, text, errtext)
	}
}
