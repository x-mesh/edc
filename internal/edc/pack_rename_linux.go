package edc

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// packRenameNoReplace는 target이 없을 때만 source를 그 이름으로 옮긴다. 그냥 rename은 빈 디렉터리를 덮는다.
func packRenameNoReplace(source, target string) error {
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%s: %w", target, errCodecExists)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return packRenameClaimed(source, target)
	}
	return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
}

// packRenameHook은 시험만 쓴다. 이름을 차지한 뒤, 옮기기 직전에 불려 그사이 다른 프로세스가 그 이름을 바꾸는 경우를 만든다.
var packRenameHook func()

// packRenameClaimed는 RENAME_NOREPLACE를 받지 않는 커널이나 파일 시스템(일부 NFS)에서 쓴다. Linux의 rename은 빈 디렉터리를
// 덮으므로 이름을 Mkdir로 먼저 차지한 뒤 그 위로 옮긴다. 차지한 뒤 다른 프로세스가 그 자리를 바꿀 수 있다. 파일이나 내용이 있는
// 디렉터리로 바꿨으면 rename이 실패하고, 되돌릴 때 rmdir을 써서 그것을 지우지 않는다. rmdir은 빈 디렉터리만 지운다. 빈
// 디렉터리로 바꿨으면 rename이 그것을 덮지만 잃는 내용은 없다.
func packRenameClaimed(source, target string) error {
	if err := packClaim(target); err != nil {
		return err
	}
	claimed, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if packRenameHook != nil {
		packRenameHook()
	}
	// 옮기기 직전에 그 이름이 아직 edc가 만든 디렉터리인지 본다. 다른 프로세스가 지우고 새로 만든 빈 디렉터리를 덮지 않는다.
	// 지운 디렉터리의 inode 번호는 곧 다시 쓰일 수 있어 ctime까지 같아야 같은 디렉터리로 본다. 이 확인과 rename 사이의 틈은
	// RENAME_NOREPLACE 없이는 없앨 수 없다.
	if now, err := os.Lstat(target); err != nil || !packSameDirectory(claimed, now) {
		return fmt.Errorf("%s: %w", target, errCodecExists)
	}
	// os.Rename은 대상이 이미 있는 디렉터리면 시스템 호출 전에 EEXIST를 돌려주므로, 차지한 빈 디렉터리 위로 옮기려면
	// rename(2)를 바로 부른다.
	if err := unix.Rename(source, target); err != nil {
		unix.Rmdir(target)
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EISDIR) {
			return fmt.Errorf("%s: %w", target, errCodecExists)
		}
		return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
	}
	return nil
}

func packSameDirectory(left, right os.FileInfo) bool {
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return os.SameFile(left, right) && leftOK && rightOK && leftStat.Ctim == rightStat.Ctim
}
