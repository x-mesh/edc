package edc

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 다른 프로세스가 입력을 쓰기 모드로 열어 두었으면 --rm은 지우지 않고 그 pid를 알린다. 마지막 확인 뒤에도 그 fd로 쓸 수 있다.
func TestCodecRemoveKeepsAnInputThatAnotherProcessWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	writer := exec.Command("sh", "-c", `exec 3>>"$0"; exec sleep 60`, path)
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		writer.Process.Kill()
		writer.Wait()
	})
	fd := filepath.Join("/proc", strconv.Itoa(writer.Process.Pid), "fd", "3")
	for range 200 {
		if _, err := os.Readlink(fd); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := codecPath(path, codecRemoveOptions(), false)
	var inUse codecInUseError
	if !errors.As(err, &inUse) || inUse.pid != writer.Process.Pid || codecErrorText(err) != T("compress.error.in_use", writer.Process.Pid) {
		t.Fatalf("err %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "line\n" {
		t.Fatalf("path holds %q err %v", data, err)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
}

// 다른 hard link가 있으면 --rm은 이름 하나만 지우고 데이터는 남으므로, 쓰기 모드로 연 fd가 있어도 지운다.
func TestCodecRemoveDeletesOneNameOfAHardLinkedInput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	if err := os.Link(path, filepath.Join(dir, "keep")); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := codecPath(path, codecRemoveOptions(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the input is still there: %v", err)
	}
	writer.WriteString("late line\n")
	if data, err := os.ReadFile(filepath.Join(dir, "keep")); err != nil || string(data) != "line\nlate line\n" {
		t.Fatalf("the other name holds %q err %v", data, err)
	}
}

// 읽기 모드로만 연 프로세스는 쓸 수 없으므로 --rm을 막지 않는다.
func TestCodecRemoveDeletesAnInputThatOthersOnlyRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := codecPath(path, codecRemoveOptions(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the input is still there: %v", err)
	}
}
