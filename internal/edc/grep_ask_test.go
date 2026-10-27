package edc

import (
	"encoding/json"
	"errors"
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
