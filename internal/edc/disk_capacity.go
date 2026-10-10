package edc

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// READ CAPACITY 응답을 읽는 부분이다. ioctl은 Linux 파일에 있고, 여기는 바이트와 재시도 규칙만 다뤄 모든 platform에서 시험한다.
const (
	scsiReadCapacity16       = 0x9e
	scsiReadCapacity16SA     = 0x10
	scsiReadCapacity10       = 0x25
	scsiReadCapacity16Length = 32
	scsiReadCapacity10Length = 8
	scsiSenseUnitAttention   = 0x6
	// 볼륨 크기가 바뀐 뒤 첫 명령에는 장치가 UNIT ATTENTION(capacity data has changed)을 한 번 돌려준다.
	scsiAttempts = 2
)

// errSCSIUnitAttention은 장치가 명령 대신 상태 변화를 알린 경우다. 같은 명령을 다시 보내면 답한다.
var errSCSIUnitAttention = errors.New("unit attention")

func scsiReadCapacity16Command() []byte {
	return []byte{scsiReadCapacity16, scsiReadCapacity16SA, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, scsiReadCapacity16Length, 0, 0}
}

func scsiReadCapacity10Command() []byte {
	return []byte{scsiReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0}
}

// parseReadCapacity16은 마지막 LBA(8 바이트)와 블록 크기(4 바이트)에서 디스크 크기를 구한다. 값은 big endian이다.
func parseReadCapacity16(data []byte) (uint64, error) {
	if len(data) < 12 {
		return 0, fmt.Errorf("READ CAPACITY(16): %d bytes", len(data))
	}
	lastLBA, blockLength := binary.BigEndian.Uint64(data[0:8]), binary.BigEndian.Uint32(data[8:12])
	if blockLength == 0 {
		return 0, errors.New("READ CAPACITY(16): zero block length")
	}
	return (lastLBA + 1) * uint64(blockLength), nil
}

// parseReadCapacity10은 2 TiB까지만 센다. 그보다 크면 장치가 마지막 LBA로 0xffffffff를 돌려주므로 크기로 쓰지 않는다.
func parseReadCapacity10(data []byte) (uint64, error) {
	if len(data) < 8 {
		return 0, fmt.Errorf("READ CAPACITY(10): %d bytes", len(data))
	}
	lastLBA, blockLength := binary.BigEndian.Uint32(data[0:4]), binary.BigEndian.Uint32(data[4:8])
	if lastLBA == 0xffffffff || blockLength == 0 {
		return 0, errors.New("READ CAPACITY(10): no usable size")
	}
	return (uint64(lastLBA) + 1) * uint64(blockLength), nil
}

// scsiCapacityFrom은 READ CAPACITY(16)을 묻고, 장치가 받지 않으면 (10)으로 다시 묻는다. 각 명령은 UNIT ATTENTION이면
// 한 번 더 보낸다. 둘 다 실패하면 (16)의 오류를 돌려준다.
func scsiCapacityFrom(read16, read10 func() (uint64, error)) (uint64, error) {
	size, err := scsiRetry(read16)
	if err == nil {
		return size, nil
	}
	if size, err10 := scsiRetry(read10); err10 == nil {
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
