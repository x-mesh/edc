package edc

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestGrepCodeAndFileSearch(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore":      "ignored/\n*.gen\n!keep.gen\n",
		"src/.gitignore":  "secret.go\n",
		"src/auth.go":     "package src\nfunc AuthToken() {}\n",
		"src/secret.go":   "AuthToken secret\n",
		"src/a.gen":       "AuthToken generated\n",
		"src/keep.gen":    "AuthToken kept\n",
		"ignored/auth.go": "AuthToken ignored\n",
		".hidden/auth.go": "AuthToken hidden\n",
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
	for _, tc := range []struct {
		args         []string
		want, absent []string
	}{
		{[]string{"--code", "AuthToken", root}, []string{"src/auth.go:2:", "src/keep.gen:1:"}, []string{"secret.go", "a.gen", "ignored/", ".hidden/"}},
		{[]string{"--code", "--hidden", "--no-ignore", "AuthToken", root}, []string{"secret.go", "a.gen", "ignored/", ".hidden/"}, nil},
		{[]string{"--files", "auth", root}, []string{"src/auth.go"}, []string{"ignored/", ".hidden/"}},
		{[]string{"--files", "-E", "-i", "AUTH\\.GO$", root}, []string{"src/auth.go"}, []string{"ignored/", ".hidden/"}},
		{[]string{"--code", "-l", "AuthToken", root}, []string{"src/auth.go\n"}, []string{"src/auth.go:2:"}},
	} {
		code, out, err := grepCommand(t, "", tc.args...)
		if code != 0 {
			t.Fatalf("%v: code %d stderr %s", tc.args, code, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, filepath.FromSlash(want)) {
				t.Errorf("%v missing %q: %s", tc.args, want, out)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(out, filepath.FromSlash(absent)) {
				t.Errorf("%v includes %q: %s", tc.args, absent, out)
			}
		}
	}
	for _, args := range [][]string{{"--files", "--code", "auth", root}, {"--files", "-l", "auth", root}, {"--code", "--index", "auth", root}, {"--files", "-E", "[", root}} {
		code, _, _ := grepCommand(t, "", args...)
		if code != 2 {
			t.Errorf("%v: code %d", args, code)
		}
	}
}

func TestGrepCodeReadsParentIgnoreAndCurrentContents(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "src"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("src/skip.go\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep.go", "skip.go"} {
		if err := os.WriteFile(filepath.Join(root, "src", name), []byte("first\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out, err := grepCommand(t, "", "--code", "first", filepath.Join(root, "src"))
	if code != 0 || strings.Contains(out, "skip.go") {
		t.Fatalf("code %d out %q err %q", code, out, err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "keep.go"), []byte("second\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, err = grepCommand(t, "", "--code", "second", filepath.Join(root, "src"))
	if code != 0 || !strings.Contains(out, "keep.go:1:second") {
		t.Fatalf("code %d out %q err %q", code, out, err)
	}
}

func TestGrepCodeOptionsAreCompleted(t *testing.T) {
	for name, script := range map[string]string{"bash": bashCompletion, "zsh": zshCompletion} {
		for _, option := range []string{"--code", "--files", "--hidden", "--no-ignore"} {
			if !strings.Contains(script, option) {
				t.Errorf("%s completion missing %s", name, option)
			}
		}
	}
}

func TestGrepSelectsModeFromInputAndArguments(t *testing.T) {
	for _, tc := range []struct {
		args                  []string
		terminal, code, files bool
		paths                 int
	}{
		{[]string{"needle"}, true, true, false, 0},
		{[]string{"-i", "needle"}, true, true, false, 0},
		{[]string{"-f", "needle"}, true, false, true, 0},
		{[]string{"--files", "needle"}, false, false, true, 0},
		{[]string{"needle"}, false, false, false, 0},
		{[]string{"needle", "."}, true, false, false, 1},
		{[]string{"needle", "app.log"}, true, false, false, 1},
		{[]string{"--level", "error", "needle"}, true, false, false, 0},
		{[]string{"--no-index", "needle"}, true, false, false, 0},
		{[]string{"--code", "needle", "."}, false, true, false, 1},
	} {
		opts, paths, err := parseGrepOptionsForInput(tc.args, io.Discard, time.Now(), tc.terminal)
		if err != nil || opts.codeMode != tc.code || opts.pathMode != tc.files || len(paths) != tc.paths {
			t.Errorf("args=%v terminal=%t: opts=%+v paths=%v err=%v", tc.args, tc.terminal, opts, paths, err)
		}
	}
	code, out, err := grepCommand(t, "needle from stdin\n", "needle")
	if code != 0 || out != "needle from stdin\n" {
		t.Fatalf("stdin: %d %q %q", code, out, err)
	}
}

func TestGrepFilePathHighlightsOnlyMatchingRelativePart(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		args                  []string
		relative, highlighted string
	}{
		{[]string{"-f", "auth"}, "auth/auth.go", "auth"},
		{[]string{"-f", "-i", "auth"}, "AUTH.go", "AUTH"},
		{[]string{"-f", "-E", "[a-z]+\\.go$"}, "src/auth.go", "auth.go"},
		{[]string{"-f", "-i", "K"}, "K.go", "K"},
	} {
		options, _, err := parseGrepOptions(tc.args, io.Discard, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, filepath.FromSlash(tc.relative))
		plain := grepHighlightPath(path, grepPathRoots([]string{root}), options, false)
		if plain != path {
			t.Fatalf("plain %q != %q", plain, path)
		}
		colored := grepHighlightPath(path, grepPathRoots([]string{root}), options, true)
		if ansi.Strip(colored) != path || !strings.Contains(colored, richPaint(tc.highlighted, richRed, true)) {
			t.Errorf("%v: %q", tc.args, colored)
		}
	}
	rootWithMatch := filepath.Join(root, "auth-root")
	if err := os.Mkdir(rootWithMatch, 0700); err != nil {
		t.Fatal(err)
	}
	options, _, err := parseGrepOptions([]string{"-f", "auth"}, io.Discard, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootWithMatch, "auth.go")
	colored := grepHighlightPath(path, grepPathRoots([]string{rootWithMatch}), options, true)
	if strings.Count(colored, richPaint("auth", richRed, true)) != 1 {
		t.Fatalf("root highlighted: %q", colored)
	}
}

func TestGrepCodeHonorsIgnoreForUncleanRoots(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "src/node_modules"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"src/keep.go", "src/node_modules/skip.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("needle\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(root, "src")
	for _, path := range []string{src, src + "/", root + "/./src", root + "//src/"} {
		code, out, err := grepCommand(t, "", "--code", "needle", path)
		if code != 0 || !strings.Contains(out, "keep.go") || strings.Contains(out, "skip.go") {
			t.Fatalf("%s: code %d out %q err %q", path, code, out, err)
		}
	}
}

func TestGrepLifecycleFlagsDispatchInAnyPosition(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--semantic-init", "--yes"}, "--semantic-init --yes"},
		{[]string{"--yes", "--semantic-init"}, "--semantic-init --yes"},
		{[]string{"--yes", "--semantic-init", "dir"}, "--semantic-init --yes dir"},
		{[]string{"dir", "--model-clear"}, "--model-clear dir"},
		{[]string{"needle", "src"}, ""},
		{[]string{"--", "--model-clear", "src"}, ""},
	} {
		got, ok := grepLifecycleArgs(tc.args)
		if ok != (tc.want != "") || strings.Join(got, " ") != tc.want {
			t.Fatalf("%v: got %v %t", tc.args, got, ok)
		}
	}
}
