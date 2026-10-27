package edc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepAskRanksCodeAndHonorsIgnore(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore": "ignored.go\n",
		"auth.go":    "package example\n\n// ValidateToken checks a user token.\nfunc ValidateToken(token string) bool { return token != \"\" }\n",
		"other.go":   "package example\nfunc Other() {}\n",
		"ignored.go": "package example\nfunc ValidateToken(token string) bool { return false }\n",
		"binary":     "\x00binary",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, stderr := grepCommand(t, "", "--ask", "--keyword", "validate token", "--limit", "2", "--json", root)
	if code != 0 {
		t.Fatalf("code %d stdout %s stderr %s", code, out, stderr)
	}
	var result grepAskOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Method != "keyword" || len(result.Results) != 1 || result.Results[0].Path != filepath.Join(root, "auth.go") || result.Results[0].Start != 3 || result.Skipped != 1 {
		t.Fatalf("%+v", result)
	}
	if !strings.Contains(result.Results[0].Snippet, "ValidateToken") {
		t.Fatal(result.Results[0].Snippet)
	}
	code, plain, stderr := grepCommand(t, "", "--ask", "--keyword", "validate token", root)
	if code != 0 || !strings.Contains(plain, ":3-4") || !strings.Contains(stderr, "1") {
		t.Fatalf("code %d stdout %q stderr %q", code, plain, stderr)
	}
	for _, args := range [][]string{{"--ask", "--files", "x", root}, {"--ask", "--keyword", "--semantic", "x", root}, {"--ask", "--limit", "0", "x", root}, {"--ask", "-E", "x", root}, {"--json", "x", root}} {
		code, _, _ := grepCommand(t, "", args...)
		if code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	code, out, _ = grepCommand(t, "", "--ask", "--keyword", "no-such-term", root)
	if code != 1 || strings.Contains(out, "auth.go:") {
		t.Fatalf("absent: %d %q", code, out)
	}
}

func TestGrepAskDeterministicRankingAndBoundedResults(t *testing.T) {
	var chunks []grepAskChunk
	for _, path := range []string{"z.go", "a.go", "b.txt"} {
		chunks = append(chunks, grepAskChunks(path, []byte("token validate\n"))...)
	}
	got := grepAskRank("token validate", chunks, 2)
	if len(got) != 2 || got[0].Path != "a.go" || got[1].Path != "b.txt" {
		t.Fatalf("ranking %+v", got)
	}
}

func TestGrepAskReportsPartialSearchErrors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "auth.go")
	if err := os.WriteFile(path, []byte("package p\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	code, out, stderr := grepCommand(t, "", "--ask", "--keyword", "validate token", "--json", root, missing)
	var result grepAskOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if code != 2 || len(result.Results) == 0 || !strings.Contains(stderr, "missing") {
		t.Fatalf("code %d result %+v stderr %q", code, result, stderr)
	}
}

func TestGrepAskRejectsOversizedQuery(t *testing.T) {
	query := strings.Repeat("term ", 33)
	code, out, stderr := grepCommand(t, "", "--ask", query, t.TempDir())
	if code != 2 || out != "" || stderr == "" {
		t.Fatalf("code %d stdout %q stderr %q", code, out, stderr)
	}
}

type grepAskErrorWriter struct{}

func (grepAskErrorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func TestGrepAskReportsOutputFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		var stderr strings.Builder
		code := grepAsk([]string{root}, grepOptions{pattern: "hello", askLimit: 10, askJSON: jsonOutput}, grepAskErrorWriter{}, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "write failed") {
			t.Fatalf("json=%v code=%d stderr=%q", jsonOutput, code, stderr.String())
		}
	}
}

func TestGrepAskDoesNotRankSearchRootName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "uniqueneedle-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("other content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := grepCommand(t, "", "--ask", "--keyword", "uniqueneedle", root)
	if code != 1 || strings.Contains(out, "plain.txt") {
		t.Fatalf("code=%d output=%q", code, out)
	}
}

func TestGrepAskBudgetCountsOnlyAcceptedText(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	binaries := grepAskMaxScanBytes/grepAskMaxFileBytes + 2
	for i := 0; i < binaries; i++ {
		path := filepath.Join(root, fmt.Sprintf("asset%02d.bin", i))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, grepAskMaxFileBytes); err != nil {
			t.Fatal(err)
		}
	}
	large := filepath.Join(root, "large.txt")
	if err := os.WriteFile(large, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, grepAskMaxFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.go"), []byte("package example\nfunc ValidateToken() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	chunks, skipped, problems := grepAskCollect([]string{root}, grepOptions{})
	if len(problems) != 0 || skipped != binaries+1 || len(chunks) == 0 {
		t.Fatalf("chunks %d skipped %d problems %v", len(chunks), skipped, problems)
	}
}

func TestGrepAskChunkBudgetStopsAtFirstOverflowingFile(t *testing.T) {
	root := t.TempDir()
	half := strings.Repeat("x\n", grepAskMaxChunks/2*grepAskChunkLines)
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(half), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "d.bin"), []byte("\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	chunks, skipped, problems := grepAskCollect([]string{root}, grepOptions{})
	if len(chunks) != grepAskMaxChunks || skipped != 0 || len(problems) != 1 || problems[0] != T("grep.ask.budget") {
		t.Fatalf("chunks %d skipped %d problems %v", len(chunks), skipped, problems)
	}
	if first, last := chunks[0].result, chunks[len(chunks)-1].result; filepath.Base(first.Path) != "a.txt" || first.Start != 1 || filepath.Base(last.Path) != "b.txt" {
		t.Fatalf("first %+v last %+v", first, last)
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].result.Path == chunks[i-1].result.Path && chunks[i].result.Start != chunks[i-1].result.End+1 {
			t.Fatalf("chunk %d out of order: %+v after %+v", i, chunks[i].result, chunks[i-1].result)
		}
	}
}

func TestGrepAskSkipsGeneratedFixtureAndLockFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"auth.go":                  "package app\nfunc ValidateToken() {}\n",
		"events_bpfel.go":          "// Code generated by bpf2go; DO NOT EDIT.\npackage app\n",
		"bpf/helper_defs.h":        "/* This is auto-generated file. See bpf_doc.py for details. */\nstatic int x;\n",
		"go.sum":                   "example.com/x v1.0.0 h1:abc=\n",
		"testdata/capture.records": "server send 4 deadbeef\n",
		"testdata/format.txt":      "Fixture format notes\n",
		"notes.md":                 "This file mentions auto-generated code in its first line.\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	chunks, skipped, problems := grepAskCollect([]string{root}, grepOptions{})
	got := map[string]bool{}
	for _, chunk := range chunks {
		got[chunk.rankPath] = true
	}
	want := map[string]bool{"auth.go": true, "testdata/format.txt": true, "notes.md": true}
	if skipped != 0 || len(problems) != 0 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v skipped %d problems %v", got, skipped, problems)
	}
	explicit := filepath.Join(root, "events_bpfel.go")
	if chunks, _, _ := grepAskCollect([]string{explicit}, grepOptions{}); len(chunks) == 0 {
		t.Fatal("an explicitly named generated file was excluded")
	}
}
