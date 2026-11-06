package edc

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestCodecMapperHelper는 아래 시험이 띄우는 process다. EDC_CODEC_MAPPER의 파일을 쓰기 가능한 공유 mapping으로 두고 fd는 닫는다.
func TestCodecMapperHelper(t *testing.T) {
	path := os.Getenv("EDC_CODEC_MAPPER")
	if path == "" {
		t.Skip("helper process for TestCodecRemoveWithoutALeaseScansProc")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := syscall.Mmap(int(file.Fd()), 0, 1, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(mapped)
	file.Close()
	fmt.Println("mapped")
	time.Sleep(60 * time.Second)
}

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

// lease를 건 뒤 다른 프로세스가 쓰기 모드로 열려고 하면, 그 open은 lease가 풀릴 때까지 기다린다. 지우면 그 프로세스가 쓴
// 데이터는 지운 inode로 가므로, 지우지 않고 원래 이름에 남긴다.
func TestCodecRemoveKeepsAnInputOpenedForWritingDuringTheCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log.1")
	codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
	var opener *exec.Cmd
	codecRemoveHook = func(stage string) {
		if stage != "delete" {
			return
		}
		held, _ := filepath.Glob(filepath.Join(dir, ".*.rm", "app.log.1"))
		if len(held) != 1 {
			t.Errorf("held %v", held)
			return
		}
		opener = exec.Command("sh", "-c", `exec 3>>"$0"`, held[0])
		if err := opener.Start(); err != nil {
			t.Error(err)
			return
		}
		// open이 lease를 깨기 시작해 /proc/locks에 BREAKING으로 보일 때까지 기다린다.
		breaking := "LEASE  BREAKING  UNLCK " + strconv.Itoa(os.Getpid()) + " "
		for range 500 {
			if locks, _ := os.ReadFile("/proc/locks"); strings.Contains(string(locks), breaking) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("the lease did not start to break")
	}
	t.Cleanup(func() {
		codecRemoveHook = nil
		if opener != nil {
			opener.Wait()
		}
	})
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
	t.Run("shared writable mapping", func(t *testing.T) {
		codecWithoutLease(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		mapper := exec.Command(os.Args[0], "-test.run=^TestCodecMapperHelper$", "-test.count=1")
		mapper.Env = append(os.Environ(), "EDC_CODEC_MAPPER="+path)
		out, err := mapper.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := mapper.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			mapper.Process.Kill()
			mapper.Wait()
		})
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "mapped\n" {
			t.Fatalf("the helper said %q: %v", line, err)
		}
		codecExpectKept(t, dir, path, codecInUseBy(mapper.Process.Pid))
	})
	// 공유 mapping에 쓰기 권한이 있을 때만 파일에 쓴다. private mapping은 고쳐도 복사본에 쓴다.
	for perms, writes := range map[string]bool{"rw-s": true, "rw-p": false, "r--s": false} {
		t.Run("mapping "+perms, func(t *testing.T) {
			codecWithoutLease(t)
			codecFakeProc(t, true)
			dir := t.TempDir()
			path := filepath.Join(dir, "app.log.1")
			codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			line := fmt.Sprintf("7f0000000000-7f0000001000 %s 00000000 %x:%x %d %s\n", perms, unix.Major(stat.Dev), unix.Minor(stat.Dev), stat.Ino, path)
			if err := os.WriteFile(filepath.Join(codecProcRoot, "123", "maps"), []byte(line), 0o644); err != nil {
				t.Fatal(err)
			}
			if writes {
				codecExpectKept(t, dir, path, codecInUseBy(123))
				return
			}
			if _, err := codecPath(path, codecRemoveOptions(), false); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("unreadable maps", func(t *testing.T) {
		codecWithoutLease(t)
		codecFakeProc(t, true)
		// root는 권한과 관계없이 읽으므로, 파일 대신 디렉터리를 두어 maps를 읽지 못하게 한다.
		if err := os.Mkdir(filepath.Join(codecProcRoot, "123", "maps"), 0o755); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "app.log.1")
		codecWrite(t, path, []byte("line\n"), 0o644, time.Now().Add(-time.Hour))
		codecExpectKept(t, dir, path, codecIs(errCodecUnchecked, "compress.error.unchecked"))
	})
	// maps를 한 줄씩 읽으므로 읽을 수 있는 길이를 넘는 줄 뒤는 보지 못한다. 그때는 확인하지 못한 것으로 친다.
	t.Run("maps line too long", func(t *testing.T) {
		codecWithoutLease(t)
		codecFakeProc(t, true)
		long := "7f0000000000-7f0000001000 r--p 00000000 00:00 0 /" + strings.Repeat("x", bufio.MaxScanTokenSize) + "\n"
		if err := os.WriteFile(filepath.Join(codecProcRoot, "123", "maps"), []byte(long), 0o644); err != nil {
			t.Fatal(err)
		}
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
