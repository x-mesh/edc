package edc

import (
	"crypto/rand"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packRandom(t *testing.T, name string, size int) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// 압축이 줄지 않으면 결과 줄에 알린다. 자동으로 고른 압축이면 --rm이어도 원본을 남기고, -z면 사용자가 고른 대로 지운다.
func TestPackKeepsAnInputThatDoesNotGetSmaller(t *testing.T) {
	dir := t.TempDir()
	packRandom(t, filepath.Join(dir, "photo.jpg"), 64<<10)
	code, _, stderr := packTest(t, dir, "--rm", "photo.jpg")
	if code != 0 || !strings.Contains(stderr, "not smaller than the input, so edc kept photo.jpg although --rm was given") {
		t.Fatalf("auto --rm: %d %s", code, stderr)
	}
	if !packExists(filepath.Join(dir, "photo.jpg")) || !packExists(filepath.Join(dir, "photo.jpg.gz")) {
		t.Fatal("the input and the output are both kept")
	}
	packRandom(t, filepath.Join(dir, "noise.bin"), 64<<10)
	if code, _, stderr := packTest(t, dir, "-z", "--rm", "noise.bin"); code != 0 || !strings.Contains(stderr, "not smaller than the input; the data is likely compressed already") || packExists(filepath.Join(dir, "noise.bin")) {
		t.Fatalf("-z --rm: %d %s", code, stderr)
	}
	packWrite(t, filepath.Join(dir, "app.log"), strings.Repeat("line\n", 1000))
	if code, _, stderr := packTest(t, dir, "--rm", "app.log"); code != 0 || strings.Contains(stderr, "not smaller") || packExists(filepath.Join(dir, "app.log")) {
		t.Fatalf("a log that gets smaller: %d %s", code, stderr)
	}
}

// --force는 이미 있는 출력 파일만 바꾼다. 입력 자신과 디렉터리는 바꾸지 않는다. 출력이 있으면 -o에 쓸 빈 이름을 알린다.
func TestPackForceReplacesOnlyAnOutputFile(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "app.log"), strings.Repeat("new\n", 300))
	packWrite(t, filepath.Join(dir, "app.log.gz"), "old")
	packWrite(t, filepath.Join(dir, "app.log-1.gz"), "taken")
	if code, _, stderr := packTest(t, dir, "app.log"); code != 1 || !strings.Contains(stderr, "app.log.gz exists; give --force to replace it, or -o app.log-2.gz") {
		t.Fatalf("suggestion: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--force", "app.log"); code != 0 || packRead(t, filepath.Join(dir, "app.log.gz")) == "old" {
		t.Fatalf("--force: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--force", "-z", "-o", "app.log", "app.log"); code != 1 || !strings.Contains(stderr, "the output is the input file") || packRead(t, filepath.Join(dir, "app.log")) != strings.Repeat("new\n", 300) {
		t.Fatalf("the input itself: %d %s", code, stderr)
	}
	packWrite(t, filepath.Join(dir, "logs", "a.log"), "a")
	if code, _, stderr := packTest(t, dir, "logs"); code != 0 {
		t.Fatalf("pack: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--force", "logs"); code != 0 || !strings.Contains(stderr, "logs/ → logs.tar.gz") {
		t.Fatalf("--force archive: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--force", "logs.tar.gz"); code != 1 || !strings.Contains(stderr, "logs exists; delete it or give -o\n") || packRead(t, filepath.Join(dir, "logs", "a.log")) != "a" {
		t.Fatalf("a directory is not replaced: %d %s", code, stderr)
	}
	if code, _, _ := packTest(t, dir, "-l", "--force", "logs.tar.gz"); code != 2 {
		t.Fatal("-l and --force together are a usage error")
	}
	if left := packLeftovers(t, dir); len(left) > 0 {
		t.Fatalf("leftovers %v", left)
	}
}

func TestPackFreeName(t *testing.T) {
	dir := t.TempDir()
	for name, want := range map[string]string{"d.tar.gz": "d-1.tar.gz", "app.log": "app-1.log", "logs": "logs-1", ".bashrc": ".bashrc-1", "x.out": "x-1.out"} {
		if got := packFreeName(filepath.Join(dir, name)); got != filepath.Join(dir, want) {
			t.Errorf("%s: got %s, want %s", name, got, want)
		}
	}
}

// --level은 압축 수준을 정한다. 1~9 밖이거나 -x, -l과 함께면 사용법 오류다.
func TestPackLevel(t *testing.T) {
	dir := t.TempDir()
	words := []string{"alpha", "beta", "gamma", "delta", "error", "warn", "info", "request", "user", "id"}
	random := mathrand.New(mathrand.NewPCG(1, 2))
	var text strings.Builder
	for range 40000 {
		text.WriteString(words[random.IntN(len(words))])
		text.WriteByte(" \n"[random.IntN(2)])
	}
	body := text.String()
	packWrite(t, filepath.Join(dir, "a.log"), body)
	if code, _, stderr := packTest(t, dir, "--level", "1", "-o", "fast.gz", "a.log"); code != 0 {
		t.Fatalf("level 1: %d %s", code, stderr)
	}
	if code, _, stderr := packTest(t, dir, "--level", "9", "-o", "small.gz", "a.log"); code != 0 {
		t.Fatalf("level 9: %d %s", code, stderr)
	}
	fast, _ := os.Stat(filepath.Join(dir, "fast.gz"))
	small, _ := os.Stat(filepath.Join(dir, "small.gz"))
	if fast == nil || small == nil || small.Size() >= fast.Size() {
		t.Fatalf("level 9 is not smaller than level 1: %v %v", fast, small)
	}
	if code, _, stderr := packTest(t, dir, "--level", "10", "a.log"); code != 2 || !strings.Contains(stderr, "--level takes 1 to 9: 10") {
		t.Fatalf("level 10: %d %s", code, stderr)
	}
	for _, args := range [][]string{{"--level", "3", "-x", "fast.gz"}, {"--level", "3", "-l", "fast.gz"}} {
		if code, _, _ := packTest(t, dir, args...); code != 2 {
			t.Fatalf("%v is a usage error", args)
		}
	}
}

func packArchiveNames(t *testing.T, dir, archive string) []string {
	t.Helper()
	code, stdout, stderr := packTest(t, dir, "-l", archive)
	if code != 0 {
		t.Fatalf("list %s: %d %s", archive, code, stderr)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if fields := strings.Fields(line); len(fields) >= 5 {
			names = append(names, fields[len(fields)-1])
		}
	}
	return names
}

func packHasName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// --exclude는 .gitignore 문법으로 묶는 디렉터리 안의 경로를 뺀다. 명령줄에 준 경로는 빼지 않는다. 결과 줄과 -v에 뺀 항목이 보인다.
func TestPackExcludePatterns(t *testing.T) {
	dir := t.TempDir()
	packWrite(t, filepath.Join(dir, "app", "main.go"), "package main")
	packWrite(t, filepath.Join(dir, "app", "debug.log"), "log")
	packWrite(t, filepath.Join(dir, "app", "node_modules", "x", "index.js"), "x")
	packWrite(t, filepath.Join(dir, "app", "web", "node_modules", "y.js"), "y")
	packWrite(t, filepath.Join(dir, "app", "build", "out.bin"), "b")
	packWrite(t, filepath.Join(dir, "app", "web", "build", "keep.txt"), "k")
	code, _, stderr := packTest(t, dir, "-v", "--exclude", "node_modules", "--exclude", "*.log", "--exclude", "/build", "app")
	if code != 0 || !strings.Contains(stderr, "4 excluded") || !strings.Contains(stderr, "- app/debug.log (excluded)") || !strings.Contains(stderr, "- app/node_modules/ (excluded)") {
		t.Fatalf("pack: %d %s", code, stderr)
	}
	names := packArchiveNames(t, dir, "app.tar.gz")
	for _, gone := range []string{"app/debug.log", "app/node_modules/", "app/web/node_modules/", "app/build/"} {
		if packHasName(names, gone) {
			t.Errorf("%s is packed: %v", gone, names)
		}
	}
	for _, kept := range []string{"app/main.go", "app/web/build/keep.txt"} {
		if !packHasName(names, kept) {
			t.Errorf("%s is missing: %v", kept, names)
		}
	}
	packWrite(t, filepath.Join(dir, "top.log"), "named")
	if code, _, stderr := packTest(t, dir, "--exclude", "*.log", "-o", "both.tar.gz", "top.log", "app"); code != 0 || !packHasName(packArchiveNames(t, dir, "both.tar.gz"), "top.log") {
		t.Fatalf("a named path is packed: %d %s", code, stderr)
	}
	for _, args := range [][]string{{"--exclude", "x", "-x", "app.tar.gz"}, {"--gitignore", "-l", "app.tar.gz"}, {"--exclude", " ", "app"}} {
		if code, _, _ := packTest(t, dir, args...); code != 2 {
			t.Fatalf("%v is a usage error", args)
		}
	}
}

// --gitignore는 .gitignore를 git처럼 읽고 .git을 뺀다. 하위 디렉터리를 묶어도 작업 트리 맨 위의 .gitignore가 쓰인다.
// 하위 .gitignore의 규칙은 그 디렉터리 안에만 쓰이고, !로 다시 넣은 항목은 묶는다.
func TestPackGitignore(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	packWrite(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main")
	packWrite(t, filepath.Join(repo, ".gitignore"), "*.tmp\ndist/\n")
	packWrite(t, filepath.Join(repo, "src", ".gitignore"), "generated.go\n!keep.tmp\n")
	packWrite(t, filepath.Join(repo, "src", "main.go"), "package main")
	packWrite(t, filepath.Join(repo, "src", "generated.go"), "gen")
	packWrite(t, filepath.Join(repo, "src", "a.tmp"), "tmp")
	packWrite(t, filepath.Join(repo, "src", "keep.tmp"), "keep")
	packWrite(t, filepath.Join(repo, "src", "dist", "bundle.js"), "js")
	packWrite(t, filepath.Join(repo, "generated.go"), "top generated")
	if code, _, stderr := packTest(t, dir, "--gitignore", "repo"); code != 0 {
		t.Fatalf("repo: %d %s", code, stderr)
	}
	names := packArchiveNames(t, dir, "repo.tar.gz")
	for _, gone := range []string{"repo/.git/", "repo/src/generated.go", "repo/src/a.tmp", "repo/src/dist/"} {
		if packHasName(names, gone) {
			t.Errorf("%s is packed: %v", gone, names)
		}
	}
	for _, kept := range []string{"repo/.gitignore", "repo/src/main.go", "repo/src/keep.tmp", "repo/generated.go"} {
		if !packHasName(names, kept) {
			t.Errorf("%s is missing: %v", kept, names)
		}
	}
	if code, _, stderr := packTest(t, repo, "--gitignore", "src"); code != 0 {
		t.Fatalf("src: %d %s", code, stderr)
	}
	names = packArchiveNames(t, repo, "src.tar.gz")
	if packHasName(names, "src/a.tmp") || packHasName(names, "src/dist/") || !packHasName(names, "src/keep.tmp") {
		t.Fatalf("the rules of the work tree apply to a subdirectory: %v", names)
	}
}

// 풀 디렉터리 이름에 파일이 있어도, 디렉터리로 푸는 작업에는 --force나 다른 이름을 권하지 않는다.
func TestPackExtractExistsDoesNotSuggestForce(t *testing.T) {
	dir := t.TempDir()
	packTarGz(t, filepath.Join(dir, "app.tar.gz"), packEntry{name: "app/", kind: '5'}, packEntry{name: "app/a", body: "a", kind: '0'})
	packWrite(t, filepath.Join(dir, "app"), "a file")
	code, _, stderr := packTest(t, dir, "app.tar.gz")
	if code != 1 || !strings.Contains(stderr, "app exists; delete it or give -o\n") || strings.Contains(stderr, "--force") {
		t.Fatalf("extract: %d %s", code, stderr)
	}
}

// 작업 트리가 무시하는 디렉터리를 직접 주면 위의 규칙을 쓰지 않고 그 안을 묶는다.
func TestPackGitignoreKeepsANamedIgnoredDirectory(t *testing.T) {
	repo := t.TempDir()
	packWrite(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main")
	packWrite(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	packWrite(t, filepath.Join(repo, "dist", "app.js"), "js")
	packWrite(t, filepath.Join(repo, "dist", ".gitignore"), "*.map\n")
	packWrite(t, filepath.Join(repo, "dist", "app.js.map"), "map")
	if code, _, stderr := packTest(t, repo, "--gitignore", "dist"); code != 0 {
		t.Fatalf("dist: %d %s", code, stderr)
	}
	names := packArchiveNames(t, repo, "dist.tar.gz")
	if !packHasName(names, "dist/app.js") || packHasName(names, "dist/app.js.map") {
		t.Fatalf("names %v", names)
	}
}
