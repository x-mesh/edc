//go:build linux

package edc

import (
	"encoding/binary"
	"testing"
)

func TestCaptureProcessFilterPacksTheNameLikeComm(t *testing.T) {
	low, high := captureProcessFilter("nginx")
	var comm [16]byte
	binary.NativeEndian.PutUint64(comm[:8], low)
	binary.NativeEndian.PutUint64(comm[8:], high)
	if string(comm[:5]) != "nginx" || comm[5] != 0 || high != 0 {
		t.Fatalf("comm = %q, high = %d", comm, high)
	}
	low, high = captureProcessFilter("123456789012345")
	binary.NativeEndian.PutUint64(comm[:8], low)
	binary.NativeEndian.PutUint64(comm[8:], high)
	if string(comm[:15]) != "123456789012345" || comm[15] != 0 {
		t.Fatalf("comm = %q", comm)
	}
}

func TestCaptureProcessFilterStaysOffForNamesThatCommCannotHold(t *testing.T) {
	for _, name := range []string{"", "1234567890123456", "has\x00nul"} {
		if low, high := captureProcessFilter(name); low != 0 || high != 0 {
			t.Fatalf("filter(%q) = %d, %d", name, low, high)
		}
	}
}
