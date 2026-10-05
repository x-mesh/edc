package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogRotationBoundsFilesPreservesOrderAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	if err := os.WriteFile(path, nil, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	file, err := openRotatingLog(path, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	payload := "0123456789ABCDEFGHIJabcdefghijKLMNOPQRSTuvwxyz"
	if n, err := file.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("write=%d, %v", n, err)
	}
	var joined strings.Builder
	for index := 3; index >= 0; index-- {
		name := path
		if index > 0 {
			name = file.archive(index)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(name)
		if err != nil || info.Size() > 10 || info.Mode().Perm() != 0640 {
			t.Fatalf("file=%s info=%v err=%v", name, info, err)
		}
		joined.Write(data)
	}
	if got, want := joined.String(), payload[10:]; got != want {
		t.Fatalf("retained=%q, want %q", got, want)
	}
	if _, err := os.Stat(file.archive(4)); !os.IsNotExist(err) {
		t.Fatalf("fourth archive exists: %v", err)
	}
}

func TestLogRotationKeepsSmallMarkersTogetherAndCanBeDisabled(t *testing.T) {
	for _, limit := range []int64{10, 0} {
		path := filepath.Join(t.TempDir(), "job.log")
		file, err := openRotatingLog(path, limit, 3)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("12345678")); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("MARK")); err != nil {
			t.Fatal(err)
		}
		file.Close()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "12345678MARK"
		if limit > 0 {
			want = "MARK"
		}
		if string(data) != want {
			t.Fatalf("limit=%d content=%q", limit, data)
		}
	}
}

func TestLogRotationRejectsForeignSymlinksAndLeavesOtherFiles(t *testing.T) {
	root := t.TempDir()
	path, other := filepath.Join(root, "job.log"), filepath.Join(root, "backup.log")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openRotatingLog(path, 4, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Symlink(other, file.archive(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("5")); err == nil {
		t.Fatal("rotation accepted a symlink archive")
	}
	if data, err := os.ReadFile(other); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated file=%q err=%v", data, err)
	}
	if _, err := openRotatingLog(file.archive(1), 4, 3); err == nil {
		t.Fatal("a symlink log path was accepted")
	}
}

func TestLogDefaultOutputIsUniqueSecureAndDoesNotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	first, err := createDefaultLogOutput("../../odd command", root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createDefaultLogOutput("../../odd command", root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || filepath.Dir(first) != filepath.Join(root, "odd_command") {
		t.Fatalf("paths=%q, %q", first, second)
	}
	info, err := os.Stat(first)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file info=%v err=%v", info, err)
	}
	info, err = os.Stat(filepath.Dir(first))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory info=%v err=%v", info, err)
	}
	if _, err := createDefaultLogOutput("job", ""); err == nil {
		t.Fatal("an unavailable default directory must fail visibly")
	}
}
