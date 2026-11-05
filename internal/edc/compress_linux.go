package edc

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// codecProcRoot는 시험이 /proc이 없는 환경을 만들 때만 바꾼다.
var codecProcRoot = "/proc"

// codecFindWriter는 held로 옮긴 입력을 쓰기 모드로 연 fd를 /proc에서 찾아 그 프로세스의 pid를 준다. 없으면 0이다.
// fdinfo의 ino, mnt_id, flags와 mountinfo의 장치 번호로 맞춘다. fd를 stat하면 응답하지 않는 NFS나 FUSE의 파일에서
// 멈추지만, fdinfo와 mountinfo는 파일 시스템에 묻지 않는다. 경로가 아니라 장치와 inode로 맞추므로 다른 mount namespace에서
// bind mount로 연 fd도 찾는다. 읽을 권한이 없는 프로세스(다른 사용자의 프로세스)와 그사이 끝난 프로세스는 건너뛴다.
func codecFindWriter(held string, moved fs.FileInfo) (int, error) {
	want := moved.Sys().(*syscall.Stat_t)
	// unlink는 이름 하나만 지운다. 다른 hard link가 남은 inode는 데이터도 남으므로, 열린 fd로 계속 써도 잃지 않는다.
	if want.Nlink > 1 {
		return 0, nil
	}
	// /proc 디렉터리만 있고 procfs가 붙지 않은 chroot에서는 프로세스가 하나도 보이지 않으므로, 자기 fdinfo로 확인한다.
	if _, err := os.Stat(filepath.Join(codecProcRoot, "self", "fdinfo")); err != nil {
		return 0, errCodecNoProc
	}
	procs, err := os.ReadDir(codecProcRoot)
	if err != nil {
		return 0, errCodecNoProc
	}
	// fdinfo에 ino 줄이 없는 커널에서는 경로로 맞춘다. 옮긴 파일의 경로에는 이번에 만든 .rm 디렉터리 이름이 들어 있다.
	token := string(filepath.Separator) + filepath.Base(filepath.Dir(held)) + string(filepath.Separator)
	// fd마다 fdinfo를 열어 읽으므로 fd 3,000개에 약 70 ms가 걸린다. 프로세스를 나눠 여러 goroutine이 읽는다.
	var found atomic.Int64
	var wg sync.WaitGroup
	names := make(chan string)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range names {
				if found.Load() == 0 && codecProcWrites(name, want, token) {
					pid, _ := strconv.Atoi(name)
					found.CompareAndSwap(0, int64(pid))
				}
			}
		}()
	}
	for _, proc := range procs {
		if found.Load() != 0 {
			break
		}
		if _, err := strconv.Atoi(proc.Name()); err == nil {
			names <- proc.Name()
		}
	}
	close(names)
	wg.Wait()
	return int(found.Load()), nil
}

// codecProcWrites는 프로세스 하나가 want를 쓰기 모드로 연 fd를 가졌는지 본다.
func codecProcWrites(name string, want *syscall.Stat_t, token string) bool {
	infoDir := filepath.Join(codecProcRoot, name, "fdinfo")
	fds, err := os.ReadDir(infoDir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		info := codecFdInfo(filepath.Join(infoDir, fd.Name()))
		if !info.writes || info.hasIno && info.ino != want.Ino {
			continue
		}
		// inode 번호는 파일 시스템마다 따로 매기므로 장치도 맞춘다. fd를 stat하면 inode 번호만 같은 다른 파일 시스템의 fd가
		// 응답하지 않는 NFS나 FUSE에 있을 때 멈추므로, 그 fd의 mount를 mountinfo에서 찾아 장치 번호를 읽는다.
		if info.hasIno {
			if dev, ok := codecMountDev(name, info.mnt); ok && dev == want.Dev {
				return true
			}
			continue
		}
		link := filepath.Join(codecProcRoot, name, "fd", fd.Name())
		if target, err := os.Readlink(link); err != nil || !strings.Contains(target, token) {
			continue
		}
		stat, err := os.Stat(link)
		if err != nil {
			continue
		}
		if sys := stat.Sys().(*syscall.Stat_t); sys.Dev == want.Dev && sys.Ino == want.Ino {
			return true
		}
	}
	return false
}

type codecFd struct {
	ino, mnt       uint64
	hasIno, writes bool
}

// codecFdInfo는 fdinfo에서 inode 번호, mount id, 그리고 flags(8진수)가 O_WRONLY나 O_RDWR인지를 읽는다.
func codecFdInfo(fdinfo string) codecFd {
	var fd codecFd
	data, err := os.ReadFile(fdinfo)
	if err != nil {
		return fd
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		key, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		value = bytes.TrimSpace(value)
		switch string(key) {
		case "flags":
			flags, err := strconv.ParseUint(string(value), 8, 64)
			fd.writes = err == nil && flags&syscall.O_ACCMODE != syscall.O_RDONLY
		case "ino":
			ino, err := strconv.ParseUint(string(value), 10, 64)
			fd.ino, fd.hasIno = ino, err == nil
		case "mnt_id":
			fd.mnt, _ = strconv.ParseUint(string(value), 10, 64)
		}
	}
	return fd
}

// codecMountDev는 프로세스의 mountinfo에서 mount id가 mnt인 줄의 major:minor를 장치 번호로 준다. mount id는 mount
// namespace마다 다르므로 그 프로세스의 mountinfo를 읽는다.
func codecMountDev(name string, mnt uint64) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join(codecProcRoot, name, "mountinfo"))
	if err != nil {
		return 0, false
	}
	prefix := strconv.FormatUint(mnt, 10) + " "
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if !strings.HasPrefix(line, prefix) || len(fields) < 3 {
			continue
		}
		major, minor, ok := strings.Cut(fields[2], ":")
		if !ok {
			return 0, false
		}
		maj, err1 := strconv.ParseUint(major, 10, 32)
		mnr, err2 := strconv.ParseUint(minor, 10, 32)
		if err1 != nil || err2 != nil {
			return 0, false
		}
		return unix.Mkdev(uint32(maj), uint32(mnr)), true
	}
	return 0, false
}
