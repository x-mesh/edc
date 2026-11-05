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

	"golang.org/x/sys/unix"
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

// codecExpectKept는 --rm이 want에 맞는 오류로 끝나고 입력을 원래 이름에 남겼는지 본다.
func codecExpectKept(t *testing.T, dir, path string, want func(error) bool) {
	t.Helper()
	_, err := codecPath(path, codecRemoveOptions(), false)
	if !want(err) {
		t.Fatalf("err %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "line\n" {
		t.Fatalf("path holds %q err %v", data, err)
	}
	if held, _ := filepath.Glob(filepath.Join(dir, ".*.rm")); len(held) != 0 {
		t.Fatalf("left %v", held)
	}
}

// codecInUseBy는 pid가 입력을 쓰기 모드로 열었다는 오류인지 본다. pid 0은 누가 열었는지 모르는 경우다.
func codecInUseBy(pid int) func(error) bool {
	return func(err error) bool {
		text := T("compress.error.in_use", pid)
		if pid == 0 {
			text = T("compress.error.in_use_unknown")
		}
		var inUse codecInUseError
		return errors.As(err, &inUse) && inUse.pid == pid && codecErrorText(err) == text
	}
}

func codecIs(target error, key string) func(error) bool {
	return func(err error) bool { return errors.Is(err, target) && codecErrorText(err) == T(key) }
}

// codecWithoutLease는 소유자가 아니거나 NFS처럼 lease를 쓸 수 없는 경우를 만든다.
func codecWithoutLease(t *testing.T) {
	codecSetLease = func(*os.File) error { return unix.EINVAL }
	t.Cleanup(func() {
		codecSetLease = func(file *os.File) error {
			_, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_RDLCK)
			return err
		}
	})
}

// codecFakeProc은 자기 fdinfo와 프로세스 123 하나가 있는 /proc을 만든다. readable이 false면 123의 fdinfo를 읽을 수 없다.
func codecFakeProc(t *testing.T, readable bool) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "self", "fdinfo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "123"), 0o755); err != nil {
		t.Fatal(err)
	}
	fdinfo := filepath.Join(root, "123", "fdinfo")
	var err error
	if readable {
		err = os.Mkdir(fdinfo, 0o755)
	} else {
		// root는 권한과 관계없이 읽으므로, 디렉터리 대신 파일을 두어 ReadDir가 실패하게 한다.
		err = os.WriteFile(fdinfo, nil, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
	codecProcRoot = root
	t.Cleanup(func() { codecProcRoot = "/proc" })
}

// 다른 프로세스가 입력을 쓰기 모드로 열어 두었으면 --rm은 지우지 않고 그 pid를 알린다. 마지막 확인 뒤에도 그 fd로 쓸 수 있다.
func TestCodecRemoveKeepsAnInputThatAnotherProcessWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	writer := codecStartWriter(t, path)
	codecExpectKept(t, dir, path, codecInUseBy(writer.Process.Pid))
}

// read lease는 /proc으로 볼 수 없는 프로세스(다른 사용자, 다른 PID namespace)의 쓰기도 잡는다. 이때 pid는 알 수 없다.
func TestCodecRemoveFindsAWriterThatProcCannotShow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecStartWriter(t, path)
	codecProcRoot = t.TempDir()
	t.Cleanup(func() { codecProcRoot = "/proc" })
	codecExpectKept(t, dir, path, codecInUseBy(0))
}

// lease가 걸리면 쓰는 프로세스가 없으므로 /proc 없이도 지운다.
func TestCodecRemoveNeedsNoProcWhenTheLeaseHolds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	codecProcRoot = t.TempDir()
	t.Cleanup(func() { codecProcRoot = "/proc" })
	if _, err := codecPath(path, codecRemoveOptions(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the input is still there: %v", err)
	}
}

// lease를 쓸 수 없으면 /proc을 훑는다. 쓰는 프로세스를 찾으면 지우지 않고, 볼 수 없는 프로세스가 있거나 /proc이 없어도
// 지우지 않는다. 모든 프로세스를 보고 쓰는 프로세스가 없을 때만 지운다.
func TestCodecRemoveWithoutALeaseScansProc(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		codecWithoutLease(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		writer := codecStartWriter(t, path)
		codecExpectKept(t, dir, path, codecInUseBy(writer.Process.Pid))
	})
	t.Run("unreadable process", func(t *testing.T) {
		codecWithoutLease(t)
		codecFakeProc(t, false)
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		codecExpectKept(t, dir, path, codecIs(errCodecUnchecked, "compress.error.unchecked"))
	})
	t.Run("no proc", func(t *testing.T) {
		codecWithoutLease(t)
		codecProcRoot = t.TempDir()
		t.Cleanup(func() { codecProcRoot = "/proc" })
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		codecExpectKept(t, dir, path, codecIs(errCodecNoProc, "compress.error.no_proc"))
	})
	t.Run("every process read", func(t *testing.T) {
		codecWithoutLease(t)
		codecFakeProc(t, true)
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		if _, err := codecPath(path, codecRemoveOptions(), false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the input is still there: %v", err)
		}
	})
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
	codecExpectKept(t, dir, path, codecInUseBy(writer.Process.Pid))
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
