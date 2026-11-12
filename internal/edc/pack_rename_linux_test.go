package edc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// RENAME_NOREPLACE가 없는 파일 시스템의 대체 경로다. 이름을 차지한 뒤 다른 프로세스가 그 자리에 파일이나 내용이 있는
// 디렉터리를 놓아도, 실패하면서 그것을 지우지 않는다.
func TestPackRenameClaimedKeepsWhatAnotherProcessMade(t *testing.T) {
	defer func() { packRenameHook = nil }()
	for _, test := range []struct {
		name  string
		place func(target string) error
		check func(target string) error
	}{
		{"file", func(target string) error { return os.WriteFile(target, []byte("theirs"), 0o644) },
			func(target string) error {
				if data, err := os.ReadFile(target); err != nil || string(data) != "theirs" {
					return errors.New("their file is gone")
				}
				return nil
			}},
		{"full directory", func(target string) error {
			if err := os.Mkdir(target, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "kept"), []byte("x"), 0o644)
		}, func(target string) error {
			_, err := os.Stat(filepath.Join(target, "kept"))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
			if err := os.Mkdir(source, 0o755); err != nil {
				t.Fatal(err)
			}
			packRenameHook = func() {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := test.place(target); err != nil {
					t.Fatal(err)
				}
			}
			if err := packRenameClaimed(source, target); !errors.Is(err, errCodecExists) {
				t.Fatalf("err %v", err)
			}
			if err := test.check(target); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(source); err != nil {
				t.Fatal("the source stays for the caller to clean up")
			}
		})
	}
	packRenameHook = nil
	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "target")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := packRenameClaimed(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("the source moved")
	}
}
