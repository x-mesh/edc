package edc

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// codecFindWriter는 held로 옮긴 입력을 쓰기 모드로 연 fd를 /proc에서 찾아 그 프로세스의 pid를 준다. 없으면 0이다.
// 읽을 권한이 없는 프로세스(다른 사용자의 프로세스)와 그사이 끝난 프로세스는 건너뛴다.
func codecFindWriter(held string, moved fs.FileInfo) (int, error) {
	want := moved.Sys().(*syscall.Stat_t)
	// unlink는 이름 하나만 지운다. 다른 hard link가 남은 inode는 데이터도 남으므로, 열린 fd로 계속 써도 잃지 않는다.
	if want.Nlink > 1 {
		return 0, nil
	}
	// 응답하지 않는 NFS나 FUSE의 파일을 연 fd를 stat하면 멈추므로, readlink로 경로를 먼저 본다. readlink는 dentry만 읽는다.
	// 옮긴 파일의 경로에는 이번에 만든 .rm 디렉터리 이름이 들어 있다. mount namespace가 달라도 그 아래 경로는 같다.
	token := string(filepath.Separator) + filepath.Base(filepath.Dir(held)) + string(filepath.Separator)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link := filepath.Join(fdDir, fd.Name())
			if target, err := os.Readlink(link); err != nil || !strings.Contains(target, token) {
				continue
			}
			info, err := os.Stat(link)
			if err != nil {
				continue
			}
			stat := info.Sys().(*syscall.Stat_t)
			if stat.Dev == want.Dev && stat.Ino == want.Ino && codecFdWrites(filepath.Join("/proc", proc.Name(), "fdinfo", fd.Name())) {
				return pid, nil
			}
		}
	}
	return 0, nil
}

// codecFdWrites는 fdinfo의 flags(8진수)가 O_WRONLY나 O_RDWR인지 본다.
func codecFdWrites(fdinfo string) bool {
	data, err := os.ReadFile(fdinfo)
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if value, ok := bytes.CutPrefix(line, []byte("flags:")); ok {
			flags, err := strconv.ParseUint(string(bytes.TrimSpace(value)), 8, 64)
			return err == nil && flags&syscall.O_ACCMODE != syscall.O_RDONLY
		}
	}
	return false
}
