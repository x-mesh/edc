package edc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// RENAME_EXCL을 받지 않는 파일 시스템에서는 옮기지 않고 그 까닭을 알린다. 빈 디렉터리도 덮지 않는다.
func TestPackRenameRefusesWithoutExclusive(t *testing.T) {
	for _, test := range []struct {
		err  error
		want error
	}{
		{unix.ENOTSUP, errPackNoReplace},
		{unix.EINVAL, errPackNoReplace},
		{unix.EEXIST, errCodecExists},
	} {
		if err := packRenameResult("a", "b", test.err); !errors.Is(err, test.want) {
			t.Errorf("%v: %v", test.err, err)
		}
	}
	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := packRenameNoReplace(source, target); !errors.Is(err, errCodecExists) {
		t.Fatalf("an empty directory in the way: %v", err)
	}
}
