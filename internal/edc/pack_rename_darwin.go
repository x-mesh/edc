package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// packRenameNoReplace는 target이 없을 때만 source를 그 이름으로 옮긴다. rename(2)는 빈 디렉터리를 덮으므로 RENAME_EXCL을 쓴다.
func packRenameNoReplace(source, target string) error {
	err := unix.RenamexNp(source, target, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%s: %w", target, errCodecExists)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		// RENAME_EXCL을 받지 않는 파일 시스템(SMB 등)이다. 이름이 없는 것을 보고 os.Rename으로 옮긴다. os.Rename은 대상이 이미
		// 있는 디렉터리면 옮기지 않으므로, 그 사이에 생긴 디렉터리는 덮지 않는다. 그 사이에 생긴 파일은 rename(2)가 거부한다.
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("%s: %w", target, errCodecExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(source, target)
	}
	return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
}
