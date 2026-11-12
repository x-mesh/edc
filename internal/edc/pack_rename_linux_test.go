package edc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// RENAME_NOREPLACE를 받지 않는 파일 시스템에서는 옮기지 않고 그 까닭을 알린다. 겹친 이름은 "이미 있음"이다.
func TestPackRenameRefusesWithoutNoReplace(t *testing.T) {
	for _, test := range []struct {
		err  error
		want error
	}{
		{unix.EINVAL, errPackNoReplace},
		{unix.ENOSYS, errPackNoReplace},
		{unix.EEXIST, errCodecExists},
		{unix.ENOTEMPTY, errCodecExists},
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
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := packRenameNoReplace(source, target); err != nil {
		t.Fatal(err)
	}
}
