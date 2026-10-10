//go:build linux

package edc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// xfsIocFSGeometryV1은 _IOR('X', 100, struct xfs_fsop_geom_v1)이다. 구조체는 112 바이트다.
// v1은 오래된 커널부터 지금 커널까지 모두 받는다.
const (
	xfsIocFSGeometryV1  = 0x80705864
	xfsGeometryV1Length = 112
)

func newDiskSystem() (diskSystem, bool) {
	return diskSystem{
		root: "/",
		run:  diskCommand,
		lookPath: func(name string) error {
			_, err := exec.LookPath(name)
			return err
		},
		xfsSize: xfsDataSize,
	}, true
}

// diskCommand는 stdout만 돌려준다. lvs와 pvs는 경고를 stderr에 쓰므로 섞으면 JSON이 깨진다.
// 명령은 자기 프로세스 그룹에서 돈다. 터미널의 Ctrl+C가 resize2fs 같은 단계를 중간에 죽이지 않는다.
func diskCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), "LC_ALL=C", "LVM_SUPPRESS_FD_WARNINGS=1")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if message := diskCommandMessage(stderr.String(), stdout.String()); message != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, message)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// xfsDataSize는 마운트된 xfs의 데이터 영역 크기를 커널에 묻는다. xfs는 자체 버퍼 캐시를 쓰므로
// 디바이스의 슈퍼블록은 늘린 직후에도 옛 값일 수 있다.
func xfsDataSize(mount string) (uint64, error) {
	file, err := os.Open(mount)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	var geometry [xfsGeometryV1Length]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), xfsIocFSGeometryV1, uintptr(unsafe.Pointer(&geometry[0]))); errno != 0 {
		return 0, fmt.Errorf("XFS_IOC_FSGEOMETRY_V1 %s: %w", mount, errno)
	}
	blockSize := binary.NativeEndian.Uint32(geometry[0:4])
	dataBlocks := binary.NativeEndian.Uint64(geometry[32:40])
	return uint64(blockSize) * dataBlocks, nil
}
