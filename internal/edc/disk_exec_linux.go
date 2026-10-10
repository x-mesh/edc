//go:build linux

package edc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	scsiSGIO               = 0x2285
	scsiSGDxferFromDev     = -3
	scsiSGInfoCheck        = 0x1
	scsiReadCapacity16     = 0x9e
	scsiReadCapacity16SA   = 0x10
	scsiReadCapacity10     = 0x25
	scsiSenseUnitAttention = 0x6
	scsiSenseLength        = 32
	scsiCommandTimeoutMS   = 5000
	// 볼륨 크기가 바뀐 뒤 첫 명령에는 장치가 UNIT ATTENTION(capacity data has changed)을 한 번 돌려준다.
	scsiAttempts = 2
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

// errSCSIUnitAttention은 장치가 명령 대신 상태 변화를 알린 경우다. 같은 명령을 다시 보내면 답한다.
var errSCSIUnitAttention = errors.New("unit attention")

// scsiCapacity는 SCSI 디스크에 지금 크기를 묻는다. 커널이 기억하는 크기(sysfs size)는 rescan 전까지 옛 값이라,
// 클라우드 콘솔에서 늘린 볼륨은 장치에 직접 물어야 보인다. rescan과 달리 커널 상태를 바꾸지 않는다.
func scsiCapacity(disk string) (uint64, error) {
	file, err := os.OpenFile("/dev/"+disk, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	size, err := scsiRetry(func() (uint64, error) { return scsiReadCapacity16Bytes(file) })
	if err == nil {
		return size, nil
	}
	if size, err10 := scsiRetry(func() (uint64, error) { return scsiReadCapacity10Bytes(file) }); err10 == nil {
		return size, nil
	}
	return 0, err
}

func scsiRetry(read func() (uint64, error)) (uint64, error) {
	var err error
	for attempt := 0; attempt < scsiAttempts; attempt++ {
		var size uint64
		if size, err = read(); !errors.Is(err, errSCSIUnitAttention) {
			return size, err
		}
	}
	return 0, err
}

func scsiReadCapacity16Bytes(file *os.File) (uint64, error) {
	data := make([]byte, 32)
	command := []byte{scsiReadCapacity16, scsiReadCapacity16SA, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(len(data)), 0, 0}
	if err := scsiCommand(file, command, data); err != nil {
		return 0, fmt.Errorf("READ CAPACITY(16) %s: %w", file.Name(), err)
	}
	lastLBA, blockLength := binary.BigEndian.Uint64(data[0:8]), binary.BigEndian.Uint32(data[8:12])
	if blockLength == 0 {
		return 0, fmt.Errorf("READ CAPACITY(16) %s: zero block length", file.Name())
	}
	return (lastLBA + 1) * uint64(blockLength), nil
}

// READ CAPACITY(10)은 2 TiB까지만 센다. 그보다 크면 마지막 LBA로 0xffffffff를 돌려준다.
func scsiReadCapacity10Bytes(file *os.File) (uint64, error) {
	data := make([]byte, 8)
	command := []byte{scsiReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := scsiCommand(file, command, data); err != nil {
		return 0, fmt.Errorf("READ CAPACITY(10) %s: %w", file.Name(), err)
	}
	lastLBA, blockLength := binary.BigEndian.Uint32(data[0:4]), binary.BigEndian.Uint32(data[4:8])
	if lastLBA == 0xffffffff || blockLength == 0 {
		return 0, fmt.Errorf("READ CAPACITY(10) %s: no usable size", file.Name())
	}
	return (uint64(lastLBA) + 1) * uint64(blockLength), nil
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

// scsiSenseKey는 fixed(0x70, 0x71)와 descriptor(0x72, 0x73) 형식의 sense key를 읽는다.
func scsiSenseKey(sense []byte) byte {
	if len(sense) < 3 {
		return 0
	}
	switch sense[0] & 0x7f {
	case 0x70, 0x71:
		return sense[2] & 0x0f
	case 0x72, 0x73:
		return sense[1] & 0x0f
	}
	return 0
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
