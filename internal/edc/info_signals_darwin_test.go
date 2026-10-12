//go:build darwin

package edc

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinMountOptions(t *testing.T) {
	if got := darwinMountOptions(unix.MNT_RDONLY | unix.MNT_SNAPSHOT | unix.MNT_LOCAL | unix.MNT_JOURNALED); got != "ro,sealed,local,journaled" {
		t.Fatalf("options = %q", got)
	}
	if got := darwinMountOptions(unix.MNT_NOSUID | unix.MNT_NODEV); got != "rw,nosuid,nodev" {
		t.Fatalf("options = %q", got)
	}
}

// 서명된 system volume은 snapshot이라 읽기 전용이 정상이다. 경고하면 macOS에서는 늘 경고가 뜬다.
func TestDarwinSealedRootIsNotReadOnlyWarning(t *testing.T) {
	signal, flags, ok := statfsDarwin("/")
	if !ok {
		t.Fatal("/를 statfs하지 못했다")
	}
	if flags&unix.MNT_SNAPSHOT != 0 && signal.ReadOnly {
		t.Fatalf("sealed root를 읽기 전용으로 경고했다: %+v", signal)
	}
}
