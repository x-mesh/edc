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
