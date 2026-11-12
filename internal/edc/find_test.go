package edc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testFindEnv(stdout, stderr io.Writer) findEnv {
	return findEnv{
		version: "test", stdin: os.Stdin, stdout: stdout, stderr: stderr, sources: testFindSources(nil),
		now: time.Now,
	}
}

func TestParseFindOptionsPositionals(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "src/main.go")
	t.Chdir(root)
	for _, tc := range []struct {
		args      []string
		stdinPipe bool
		pattern   string
		paths     []string
		explicit  bool
	}{
		{nil, false, "", []string{"."}, false},
		{[]string{"main"}, false, "main", []string{"."}, false},
		// /가 든 기존 디렉터리 하나는 찾을 곳이다.
		{[]string{"src/"}, false, "", []string{"src/"}, true},
		{[]string{"./src"}, false, "", []string{"./src"}, true},
		// /가 들어 있어도 디렉터리가 아니면 패턴이다.
		{[]string{"nginx/conf"}, false, "nginx/conf", []string{"."}, false},
		// /가 없으면 디렉터리가 있어도 패턴이다.
		{[]string{"src"}, false, "src", []string{"."}, false},
		{[]string{"main", "src", "/tmp"}, false, "main", []string{"src", "/tmp"}, true},
		// find .에 익숙한 사용자를 위해 .과 ..은 경로다.
		{[]string{"."}, false, "", []string{"."}, true},
		{[]string{".."}, false, "", []string{".."}, true},
		{[]string{"-t", "f", "main", "--", "-odd"}, false, "main", []string{"-odd"}, true},
		// stdin이 파이프이고 경로가 없으면 stdin의 줄이 후보다.
		{[]string{"main"}, true, "main", nil, false},
	} {
		options, err := parseFindOptions(tc.args, io.Discard, tc.stdinPipe)
		if err != nil || options.pattern != tc.pattern || !reflect.DeepEqual(options.paths, tc.paths) || options.explicit != tc.explicit {
			t.Errorf("%v: pattern %q, paths %v, explicit %v, err %v", tc.args, options.pattern, options.paths, options.explicit, err)
		}
	}
}

func TestParseFindSize(t *testing.T) {
	for _, tc := range []struct {
		text     string
		min, max int64
		ok       bool
	}{
		{"+100M", 100 << 20, 0, true},
		{"100m", 100 << 20, 0, true},
		{"-1K", 0, 1 << 10, true},
		{"1.5G", 3 << 29, 0, true},
		{"2TB", 2 << 40, 0, true},
		{"512", 512, 0, true},
		{"", 0, 0, false},
		{"+", 0, 0, false},
		{"10X", 0, 0, false},
		{"-5-", 0, 0, false},
	} {
		minimum, maximum, err := parseFindSize(tc.text)
		if (err == nil) != tc.ok || tc.ok && (minimum != tc.min || maximum != tc.max) {
			t.Errorf("%q: min %d, max %d, err %v", tc.text, minimum, maximum, err)
		}
	}
}

func TestRunFindRejectsBadValues(t *testing.T) {
	for _, args := range [][]string{{"--size", "big"}, {"--changed", "soon"}, {"-t", "x"}, {"--workers", "0"}, {"-E", "("}} {
		var stderr bytes.Buffer
		if code := runFindWith(args, testFindEnv(io.Discard, &stderr)); code != 2 || stderr.Len() == 0 || strings.Contains(stderr.String(), "find.error") {
			t.Errorf("%v: code %d, stderr %q", args, code, stderr.String())
		}
	}
}

func TestRunFindListsMatchesBestFirst(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "internal/edc/audit_journal.go", "internal/edc/admin/journal.go", "docs/adjrn.md", "README.md")
	t.Chdir(root)
	var stdout bytes.Buffer
	if code := runFindWith([]string{"adjrn"}, testFindEnv(&stdout, io.Discard)); code != 0 {
		t.Fatalf("code %d", code)
	}
	if lines := strings.Split(strings.TrimSpace(stdout.String()), "\n"); !reflect.DeepEqual(lines, []string{"docs/adjrn.md", "internal/edc/audit_journal.go", "internal/edc/admin/journal.go"}) {
		t.Errorf("lines = %q", lines)
	}
	stdout.Reset()
	if code := runFindWith([]string{"-E", "journal\\.go$"}, testFindEnv(&stdout, io.Discard)); code != 0 || stdout.String() != "internal/edc/admin/journal.go\ninternal/edc/audit_journal.go\n" {
		t.Errorf("regex keeps path order: code %d, %q", code, stdout.String())
	}
	stdout.Reset()
	if code := runFindWith([]string{"-0", "readme"}, testFindEnv(&stdout, io.Discard)); code != 0 || stdout.String() != "README.md\x00" {
		t.Errorf("-0: %q", stdout.String())
	}
	if code := runFindWith([]string{"nothingmatches"}, testFindEnv(io.Discard, io.Discard)); code != 1 {
		t.Errorf("no match: code %d", code)
	}
	var stderr bytes.Buffer
	if code := runFindWith([]string{"x", "missing"}, testFindEnv(io.Discard, &stderr)); code != 2 || !strings.Contains(stderr.String(), "missing") {
		t.Errorf("missing root: code %d, %q", code, stderr.String())
	}
}

func TestRunFindFilters(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "logs/", "logs/old.log", "logs/new.log", "logs/big.bin")
	if err := os.WriteFile(filepath.Join(root, "logs/big.bin"), make([]byte, 3<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "logs/old.log"), old, old); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-t", "d", "log"}, "logs\n"},
		{[]string{"--type", "f,l", "-E", "log$"}, "logs/new.log\nlogs/old.log\n"},
		// --changed는 최근에 바뀐 것만 남긴다.
		{[]string{"--changed", "1d", "-t", "f", "-E", "log$"}, "logs/new.log\n"},
		// --size는 일반 파일에만 적용한다.
		{[]string{"--size", "+2K", "-E", "."}, "logs/big.bin\n"},
		{[]string{"--size", "-1K", "-E", "\\.log$"}, "logs/new.log\nlogs/old.log\n"},
	} {
		var stdout bytes.Buffer
		runFindWith(tc.args, testFindEnv(&stdout, io.Discard))
		if stdout.String() != tc.want {
			t.Errorf("%v: %q", tc.args, stdout.String())
		}
	}
}

func TestRunFindHighlightsAndEscapesOnATerminal(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "a\x1bb.txt", "notes.txt")
	t.Chdir(root)
	env := testFindEnv(nil, io.Discard)
	var stdout bytes.Buffer
	env.stdout, env.stdoutTerminal = &stdout, true
	runFindWith([]string{"-E", "b\\.txt"}, env)
	if strings.Contains(stdout.String(), "\x1bb") {
		t.Errorf("a control byte must not reach the terminal: %q", stdout.String())
	}
	stdout.Reset()
	env.color = true
	runFindWith([]string{"note"}, env)
	if !strings.Contains(stdout.String(), "\x1b[") || !strings.Contains(stdout.String(), "s.txt") {
		t.Errorf("color: %q", stdout.String())
	}
}

func TestRunFindWritesJSON(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "etc/nginx/nginx.conf")
	t.Chdir(root)
	out := filepath.Join(t.TempDir(), "find.json")
	if code := runFindWith([]string{"--json", out, "nginxconf"}, testFindEnv(io.Discard, io.Discard)); code != 0 {
		t.Fatalf("code %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report findJSONReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Tool["version"] != "test" || len(report.Matches) != 1 || report.Matches[0].Path != "etc/nginx/nginx.conf" || report.Matches[0].Type != "file" ||
		report.Matches[0].Score == nil || report.Matches[0].Size != nil || report.Skipped == nil || report.Problems == nil {
		t.Errorf("report = %s", data)
	}
	// 크기 조건이 있으면 stat한 값이 들어간다.
	if code := runFindWith([]string{"--json", out, "--size", "-1M", "nginxconf"}, testFindEnv(io.Discard, io.Discard)); code != 0 {
		t.Fatalf("code %d", code)
	}
	data, _ = os.ReadFile(out)
	report = findJSONReport{}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Matches) != 1 || report.Matches[0].Size == nil || *report.Matches[0].Size != 20 || report.Matches[0].MTime == nil {
		t.Errorf("stat fields: %s", data)
	}
}

func TestRunFindReadsStdinLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "list")
	long := strings.Repeat("x", findStdinLineLimit+10)
	if err := os.WriteFile(file, []byte("cmd/edc/main.go\r\ninternal/edc/find.go\n\n"+long+"\ninternal/edc/find_test.go"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	env := testFindEnv(nil, io.Discard)
	var stdout bytes.Buffer
	env.stdout, env.stdin, env.stdinPipe = &stdout, stdin, true
	if code := runFindWith([]string{"fndgo"}, env); code != 0 || stdout.String() != "internal/edc/find.go\ninternal/edc/find_test.go\n" {
		t.Errorf("code %d, %q", code, stdout.String())
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	var stderr bytes.Buffer
	env.stderr = &stderr
	runFindWith([]string{"-E", "^x+$"}, env)
	if got := strings.TrimSpace(stdout.String()); len(got) != findStdinLineLimit {
		t.Errorf("a long line must be cut to the limit: %d bytes", len(got))
	}
}

func TestRunFindFallsBackToTheListWithoutATerminal(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root, "b", "a")
	t.Chdir(root)
	env := testFindEnv(nil, io.Discard)
	var stdout bytes.Buffer
	env.stdout = &stdout
	asked := false
	env.pick = func(findOptions, findEnv) (int, bool) {
		asked = true
		return 0, false
	}
	if code := runFindWith(nil, env); code != 0 || !asked || stdout.String() != "a\nb\n" {
		t.Errorf("code %d, asked %v, %q", code, asked, stdout.String())
	}
	asked = false
	runFindWith([]string{"a"}, env)
	if asked {
		t.Error("a pattern must not open the picker")
	}
}

func TestFindFilterApplyKeepsTheBatchWithoutConditions(t *testing.T) {
	batch := []findEntry{{path: "a", kind: findKindFile}, {path: "b", kind: findKindDir}}
	if kept := (findFilter{}).apply(batch); len(kept) != 2 {
		t.Errorf("kept = %v", kept)
	}
	filter := newFindFilter(findOptions{types: map[findKind]bool{findKindDir: true}}, time.Now())
	if kept := filter.apply(batch); len(kept) != 1 || kept[0].path != "b" || len(batch) != 2 {
		t.Errorf("kept = %v, batch = %v", kept, batch)
	}
}

func TestRunFindRejectsFiltersOnStdinLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "list")
	if err := os.WriteFile(file, []byte("a.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	for _, args := range [][]string{{"-t", "f", "a"}, {"--size", "+1K", "a"}, {"--changed", "1h"}} {
		var stderr bytes.Buffer
		env := testFindEnv(io.Discard, &stderr)
		env.stdin, env.stdinPipe = stdin, true
		if code := runFindWith(args, env); code != 2 || !strings.Contains(stderr.String(), "--type") {
			t.Errorf("%v: code %d, %q", args, code, stderr.String())
		}
	}
}
