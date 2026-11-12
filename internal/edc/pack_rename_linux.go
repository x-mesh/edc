package edc

import (
	"errors"
	"fmt"
	"os"

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
		// RENAME_NOREPLACE를 받지 않는 커널이나 파일 시스템(일부 NFS)이다. Linux의 rename은 빈 디렉터리를 덮으므로, 이름을
		// Mkdir로 먼저 차지한 뒤 그 위로 옮기면 그사이 생긴 다른 디렉터리를 덮지 않는다.
		if err := packClaim(target); err != nil {
			return err
		}
		if err := os.Rename(source, target); err != nil {
			os.Remove(target)
			return err
		}
		return nil
	}
	return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
}
