//go:build linux

package edc

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

// kernel은 이 구조체를 그대로 읽고 쓴다. 64비트 Linux의 sizeof(struct sg_io_hdr)는 88이다.
func TestSCSISGIOHdrMatchesTheKernelLayout(t *testing.T) {
	var header scsiSGIOHdr
	if size := unsafe.Sizeof(header); size != 88 {
		t.Fatalf("sizeof = %d, want 88", size)
	}
	for name, got := range map[string]uintptr{"dxferp": unsafe.Offsetof(header.Dxferp), "usr_ptr": unsafe.Offsetof(header.UsrPtr), "status": unsafe.Offsetof(header.Status), "info": unsafe.Offsetof(header.Info)} {
		want := map[string]uintptr{"dxferp": 16, "usr_ptr": 56, "status": 64, "info": 80}[name]
		if got != want {
			t.Errorf("offsetof %s = %d, want %d", name, got, want)
		}
	}
}

func TestSCSISenseKeyReadsBothFormats(t *testing.T) {
	fixed := []byte{0x70, 0, 0x06, 0, 0, 0, 0, 10, 0, 0, 0, 0, 0x2a, 0x09}
	descriptor := []byte{0x72, 0x06, 0x2a, 0x09}
	if scsiSenseKey(fixed) != scsiSenseUnitAttention || scsiSenseKey(descriptor) != scsiSenseUnitAttention || scsiSenseKey(nil) != 0 {
		t.Fatal("sense key")
	}
}

// EDC_SCSI_DISK=sda처럼 SCSI 디스크를 주면 READ CAPACITY 답을 커널의 크기와 비교한다. 파싱이 틀리면 늘어난 볼륨을
// 못 보고도 같은 크기라고 판단하므로, 실제 장치에서 한 번 맞춰 본다. rescan하지 않으므로 커널 크기가 옛 값이면 다를 수 있다.
func TestSCSICapacityMatchesTheKernelSize(t *testing.T) {
	disk := os.Getenv("EDC_SCSI_DISK")
	if disk == "" {
		t.Skip("set EDC_SCSI_DISK to a SCSI disk to compare READ CAPACITY with sysfs")
	}
	size, err := scsiCapacity(disk)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/sys/block/" + disk + "/size")
	if err != nil {
		t.Fatal(err)
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("READ CAPACITY %d bytes, sysfs %d bytes", size, sectors*diskSectorSize)
	if size != sectors*diskSectorSize {
		t.Fatalf("READ CAPACITY %d, sysfs %d", size, sectors*diskSectorSize)
	}
}
