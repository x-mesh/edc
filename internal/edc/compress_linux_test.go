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

// codecStartWriter는 path를 fd 3에 쓰기 모드로 열어 둔 sh를 띄우고, 그 fd가 /proc에 보일 때까지 기다린다.
func codecStartWriter(t *testing.T, path string) *exec.Cmd {
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
			return writer
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the writer did not open %s", path)
	return nil
}

func codecExpectInUse(t *testing.T, dir, path string, writer *exec.Cmd) {
	t.Helper()
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

// 다른 프로세스가 입력을 쓰기 모드로 열어 두었으면 --rm은 지우지 않고 그 pid를 알린다. 마지막 확인 뒤에도 그 fd로 쓸 수 있다.
func TestCodecRemoveKeepsAnInputThatAnotherProcessWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecExpectInUse(t, dir, path, codecStartWriter(t, path))
}

// 쓰는 프로세스의 fd 경로에 .rm 디렉터리가 보이지 않아도 inode로 찾는다. 다른 이름으로 연 뒤 그 이름을 지우면 fd 경로는
// "(deleted)"로 남는다. 다른 mount namespace에서 파일을 bind mount해 연 fd도 이처럼 경로가 다르게 보인다.
func TestCodecRemoveFindsAWriterWhosePathDiffers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	other := filepath.Join(dir, "other")
	if err := os.Link(path, other); err != nil {
		t.Fatal(err)
	}
	writer := codecStartWriter(t, other)
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	codecExpectInUse(t, dir, path, writer)
}

// /proc이 없거나 procfs가 붙지 않았으면 쓰는 프로세스를 확인할 수 없으므로 지우지 않는다.
func TestCodecRemoveKeepsTheInputWithoutProc(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecProcRoot = t.TempDir()
	t.Cleanup(func() { codecProcRoot = "/proc" })
	_, err := codecPath(path, codecRemoveOptions(), false)
	if !errors.Is(err, errCodecNoProc) || codecErrorText(err) != T("compress.error.no_proc") {
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
