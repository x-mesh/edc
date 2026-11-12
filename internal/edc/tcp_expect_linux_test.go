//go:build linux

package edc

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// 실행 중 줄의 시각은 kernel이 packet을 받은 시각이다. DROP 줄도 kernel 시각이라 두 줄을 시각순으로 맞출 수 있다.
func TestExpectPacketTimeReadsTheKernelStamp(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
		t.Skip(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Skip(err)
	}
	self, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := unix.Sendto(fd, []byte("x"), 0, self); err != nil {
		t.Fatal(err)
	}
	buffer, control := make([]byte, 16), make([]byte, unix.CmsgSpace(16))
	_, controlSize, _, _, err := unix.Recvmsg(fd, buffer, control, 0)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	fallback := time.Unix(1, 0)
	got := expectPacketTime(control[:controlSize], fallback)
	if got.Equal(fallback) || got.Before(before.Add(-time.Second)) || got.After(after) {
		t.Fatalf("stamp %v, want between %v and %v", got, before, after)
	}
	if got := expectPacketTime(nil, fallback); !got.Equal(fallback) {
		t.Errorf("no control message: %v", got)
	}
}
