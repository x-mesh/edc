package edc

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// packRenameNoReplace는 target이 없을 때만 source를 그 이름으로 옮긴다. rename(2)는 빈 디렉터리를 덮으므로
// RENAME_NOREPLACE를 쓴다.
func packRenameNoReplace(source, target string) error {
	return packRenameResult(source, target, unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE))
}

// packRenameResult는 renameat2의 결과를 edc의 오류로 바꾼다. RENAME_NOREPLACE를 받지 않는 커널이나 파일 시스템(일부 NFS)에서는
// 옮기지 않는다. 이름이 없는지 본 뒤 옮기는 방법은 그 사이에 생긴 빈 디렉터리를 덮을 수 있어서, 덮어쓰지 않는다는 약속을 지킬 수
// 없기 때문이다.
func packRenameResult(source, target string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%s: %w", target, errCodecExists)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return fmt.Errorf("%s: %w", target, errPackNoReplace)
	}
	return &os.LinkError{Op: "rename", Old: source, New: target, Err: err}
}
