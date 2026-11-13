package edc

import (
	"crypto/rand"
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
	if code, _, stderr := packTest(t, dir, "--force", "logs.tar.gz"); code != 1 || !strings.Contains(stderr, "logs exists; delete it or give -o logs-1") || packRead(t, filepath.Join(dir, "logs", "a.log")) != "a" {
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
