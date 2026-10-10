//go:build linux

package edc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SG_IO는 <scsi/sg.h>의 ioctl이다. READ CAPACITY는 읽기 권한으로 열어도 커널이 보내 주는 명령이고 장치 상태를 바꾸지 않는다.
const (
	scsiSGIO             = 0x2285
	scsiSGDxferFromDev   = -3
	scsiSGInfoCheck      = 0x1
	scsiSenseLength      = 32
	scsiCommandTimeoutMS = 5000
)

// scsiSGIOHdr는 64비트 Linux의 struct sg_io_hdr와 같은 배치다(88 바이트).
type scsiSGIOHdr struct {
	InterfaceID    int32
	DxferDirection int32
	CmdLen         uint8
	MxSbLen        uint8
	IovecCount     uint16
	DxferLen       uint32
	Dxferp         unsafe.Pointer
	Cmdp           unsafe.Pointer
	Sbp            unsafe.Pointer
	Timeout        uint32
	Flags          uint32
	PackID         int32
	UsrPtr         unsafe.Pointer
	Status         uint8
	MaskedStatus   uint8
	MsgStatus      uint8
	SbLenWr        uint8
	HostStatus     uint16
	DriverStatus   uint16
	Resid          int32
	Duration       uint32
	Info           uint32
}

// scsiCapacity는 SCSI 디스크에 지금 크기를 묻는다. 커널이 기억하는 크기(sysfs size)는 rescan 전까지 옛 값이라,
// 클라우드 콘솔에서 늘린 볼륨은 장치에 직접 물어야 보인다. rescan과 달리 커널 상태를 바꾸지 않는다.
func scsiCapacity(disk string) (uint64, error) {
	file, err := os.OpenFile("/dev/"+disk, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	read := func(command []byte, length int, parse func([]byte) (uint64, error)) func() (uint64, error) {
		return func() (uint64, error) {
			data := make([]byte, length)
			if err := scsiCommand(file, command, data); err != nil {
				return 0, fmt.Errorf("%s: %w", file.Name(), err)
			}
			return parse(data)
		}
	}
	return scsiCapacityFrom(
		read(scsiReadCapacity16Command(), scsiReadCapacity16Length, parseReadCapacity16),
		read(scsiReadCapacity10Command(), scsiReadCapacity10Length, parseReadCapacity10),
	)
}

func scsiCommand(file *os.File, command, data []byte) error {
	sense := make([]byte, scsiSenseLength)
	header := scsiSGIOHdr{
		InterfaceID: 'S', DxferDirection: scsiSGDxferFromDev, CmdLen: uint8(len(command)), MxSbLen: uint8(len(sense)),
		DxferLen: uint32(len(data)), Dxferp: unsafe.Pointer(&data[0]), Cmdp: unsafe.Pointer(&command[0]), Sbp: unsafe.Pointer(&sense[0]),
		Timeout: scsiCommandTimeoutMS,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), scsiSGIO, uintptr(unsafe.Pointer(&header)))
	runtime.KeepAlive(command)
	runtime.KeepAlive(data)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return errno
	}
	if header.Info&scsiSGInfoCheck == 0 {
		return nil
	}
	if scsiSenseKey(sense[:header.SbLenWr]) == scsiSenseUnitAttention {
		return errSCSIUnitAttention
	}
	return fmt.Errorf("status 0x%x, host 0x%x, driver 0x%x", header.Status, header.HostStatus, header.DriverStatus)
}

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
		xfsSize:  xfsDataSize,
		capacity: scsiCapacity,
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
