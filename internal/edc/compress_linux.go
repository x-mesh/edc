package edc

import (
	"bytes"
	"errors"
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

// codecProcRoot와 codecSetLease는 시험이 /proc이 없거나 lease를 쓸 수 없는 환경을 만들 때만 바꾼다.
var (
	codecProcRoot = "/proc"
	codecSetLease = func(file *os.File) error {
		_, err := unix.FcntlInt(file.Fd(), unix.F_SETLEASE, unix.F_RDLCK)
		return err
	}
)

// codecCheckWriters는 held로 옮긴 입력을 다른 프로세스가 쓰기 모드로 열어 두었는지 본다. 그런 프로세스는 다시 읽은 뒤에도
// 열린 fd로 쓸 수 있고, 그 데이터는 지울 때 사라진다. input은 읽기 모드로 연 그 파일이다. leased는 input에 read lease를
// 걸었는지다.
func codecCheckWriters(input *os.File, held string, moved fs.FileInfo) (leased bool, err error) {
	want := moved.Sys().(*syscall.Stat_t)
	// unlink는 이름 하나만 지운다. 다른 hard link가 남은 inode는 데이터도 남으므로, 열린 fd로 계속 써도 잃지 않는다.
	if want.Nlink > 1 {
		return false, nil
	}
	// read lease는 그 파일을 쓰기 모드로 연 프로세스가 하나도 없을 때만 걸린다. 커널이 사용자와 namespace에 관계없이 세므로
	// /proc으로 볼 수 없는 프로세스도 잡는다. lease는 input을 닫을 때 풀리고, 그때까지 쓰기 모드로 여는 open은 기다린다.
	err = codecSetLease(input)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EAGAIN) {
		pid, _, _ := codecScanWriters(held, want)
		return false, codecInUseError{pid: pid}
	}
	// lease는 파일 소유자나 CAP_LEASE가 있어야 하고, lease를 지원하지 않는 파일 시스템도 있다. 그때는 /proc을 훑고, 볼 수
	// 없는 프로세스가 있으면 지우지 않는다.
	pid, complete, err := codecScanWriters(held, want)
	switch {
	case err != nil:
		return false, err
	case pid != 0:
		return false, codecInUseError{pid: pid}
	case !complete:
		return false, errCodecUnchecked
	}
	return false, nil
}

// codecLeaseBroken은 lease를 건 뒤 다른 프로세스가 그 파일을 쓰기 모드로 열려고 했는지 본다. 그런 open이 기다리는 동안
// 커널은 lease를 F_UNLCK로 깨는 중으로 두고, F_GETLEASE는 F_RDLCK가 아닌 값을 준다.
func codecLeaseBroken(input *os.File) bool {
	lease, err := unix.FcntlInt(input.Fd(), unix.F_GETLEASE, 0)
	return err != nil || lease != unix.F_RDLCK
}

// codecScanWriters는 want를 쓰기 모드로 연 fd를 /proc에서 찾아 그 프로세스의 pid를 준다. 없으면 0이다. complete는 모든
// 프로세스의 fdinfo를 읽었는지다. fdinfo의 ino, mnt_id, flags와 mountinfo의 장치 번호로 맞춘다. fd를 stat하면 응답하지
// 않는 NFS나 FUSE의 파일에서 멈추지만, fdinfo와 mountinfo는 파일 시스템에 묻지 않는다. 경로가 아니라 장치와 inode로
// 맞추므로 다른 mount namespace에서 bind mount로 연 fd도 찾는다.
func codecScanWriters(held string, want *syscall.Stat_t) (pid int, complete bool, err error) {
	// /proc 디렉터리만 있고 procfs가 붙지 않은 chroot에서는 프로세스가 하나도 보이지 않으므로, 자기 fdinfo로 확인한다.
	if _, err := os.Stat(filepath.Join(codecProcRoot, "self", "fdinfo")); err != nil {
		return 0, false, errCodecNoProc
	}
	procs, err := os.ReadDir(codecProcRoot)
	if err != nil {
		return 0, false, errCodecNoProc
	}
	// fdinfo에 ino 줄이 없는 커널에서는 경로로 맞춘다. 옮긴 파일의 경로에는 이번에 만든 .rm 디렉터리 이름이 들어 있다.
	token := string(filepath.Separator) + filepath.Base(filepath.Dir(held)) + string(filepath.Separator)
	// fd마다 fdinfo를 열어 읽으므로 fd 3,000개에 약 70 ms가 걸린다. 프로세스를 나눠 여러 goroutine이 읽는다.
	var found atomic.Int64
	var missed atomic.Bool
	var wg sync.WaitGroup
	names := make(chan string)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range names {
				if found.Load() != 0 {
					continue
				}
				writes, read := codecProcWrites(name, want, token)
				if !read {
					missed.Store(true)
				}
				if writes {
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
	return int(found.Load()), !missed.Load(), nil
}

// codecProcWrites는 프로세스 하나가 want를 쓰기 모드로 연 fd나 쓰기 가능한 공유 mapping을 가졌는지 본다. read는 그
// 프로세스의 fdinfo와 maps를 읽었는지다. 그사이 끝난 프로세스는 쓸 수 없으므로 읽은 것으로 친다.
func codecProcWrites(name string, want *syscall.Stat_t, token string) (writes, read bool) {
	infoDir := filepath.Join(codecProcRoot, name, "fdinfo")
	fds, err := os.ReadDir(infoDir)
	if err != nil {
		return false, codecProcGone(err)
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
				return true, true
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
			return true, true
		}
	}
	// 쓰기 가능한 공유 mapping은 fd를 닫은 뒤에도 그 파일에 쓰고, fdinfo에는 보이지 않는다.
	return codecMapsWrite(name, want)
}

func codecProcGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// codecMapsWrite는 프로세스의 maps에 want를 쓰기 가능한 공유 mapping으로 연 줄이 있는지 본다. 한 줄은 "주소 권한 offset
// major:minor inode 경로"이고, 권한의 둘째 글자가 w이고 넷째 글자가 s이면 쓰기 가능한 공유 mapping이다. 장치 번호는 16진수다.
// 다른 사용자의 프로세스는 maps를 읽을 수 없으므로 read가 false다.
func codecMapsWrite(name string, want *syscall.Stat_t) (writes, read bool) {
	data, err := os.ReadFile(filepath.Join(codecProcRoot, name, "maps"))
	if err != nil {
		return false, codecProcGone(err)
	}
	ino := strconv.FormatUint(want.Ino, 10)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != ino || len(fields[1]) < 4 || fields[1][1] != 'w' || fields[1][3] != 's' {
			continue
		}
		major, minor, ok := strings.Cut(fields[3], ":")
		if !ok {
			continue
		}
		maj, err1 := strconv.ParseUint(major, 16, 32)
		mnr, err2 := strconv.ParseUint(minor, 16, 32)
		if err1 == nil && err2 == nil && unix.Mkdev(uint32(maj), uint32(mnr)) == want.Dev {
			return true, true
		}
	}
	return false, true
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
