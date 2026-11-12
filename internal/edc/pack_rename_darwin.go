package edc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// packRenameNoReplace는 target이 없을 때만 source를 그 이름으로 옮긴다. macOS의 rename은 빈 디렉터리 위로 옮기지 않으므로
// 이름을 먼저 차지하는 방법을 쓸 수 없다.
func packRenameNoReplace(source, target string) error {
	err := unix.RenamexNp(source, target, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%s: %w", target, errCodecExists)
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		// RENAME_EXCL을 받지 않는 파일 시스템(SMB 등)이다. 이름이 없는 것을 보고 옮기므로, 그 사이에 생긴 빈 디렉터리는 rename이
		// 거부한다.
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("%s: %w", target, errCodecExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Rename(source, target)
	}
	return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
}
