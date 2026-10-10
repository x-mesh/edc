package edc

import (
	"encoding/binary"
	"errors"
	"testing"
)

func readCapacity16Bytes(lastLBA uint64, blockLength uint32) []byte {
	data := make([]byte, scsiReadCapacity16Length)
	binary.BigEndian.PutUint64(data[0:8], lastLBA)
	binary.BigEndian.PutUint32(data[8:12], blockLength)
	return data
}

// 값은 OCI의 200 GB 부트 볼륨이 돌려준 답이다. sg_readcap과 sysfs가 같은 크기를 보였다.
func TestParseReadCapacityReadsTheLastLBAAndTheBlockLength(t *testing.T) {
	if size, err := parseReadCapacity16(readCapacity16Bytes(419430399, 512)); err != nil || size != 214748364800 {
		t.Fatalf("RC16 512 = %d, %v", size, err)
	}
	if size, err := parseReadCapacity16(readCapacity16Bytes(52428799, 4096)); err != nil || size != 214748364800 {
		t.Fatalf("RC16 4096 = %d, %v", size, err)
	}
	for name, data := range map[string][]byte{"short": make([]byte, 8), "zero block": readCapacity16Bytes(10, 0)} {
		if _, err := parseReadCapacity16(data); err == nil {
			t.Errorf("RC16 %s must fail", name)
		}
	}
	rc10 := make([]byte, scsiReadCapacity10Length)
	binary.BigEndian.PutUint32(rc10[0:4], 2097151)
	binary.BigEndian.PutUint32(rc10[4:8], 512)
	if size, err := parseReadCapacity10(rc10); err != nil || size != 1<<30 {
		t.Fatalf("RC10 = %d, %v", size, err)
	}
	// 2 TiB보다 큰 디스크는 RC10에서 0xffffffff로 답한다. 그 값을 크기로 쓰면 2 TiB로 잘못 본다.
	binary.BigEndian.PutUint32(rc10[0:4], 0xffffffff)
	if _, err := parseReadCapacity10(rc10); err == nil {
		t.Fatal("RC10 past 2 TiB must fail")
	}
}

// 볼륨을 늘린 뒤 첫 명령은 UNIT ATTENTION을 받는다. 한 번 더 묻고, (16)을 받지 않는 장치는 (10)으로 묻는다.
func TestSCSICapacityRetriesAUnitAttentionAndFallsBack(t *testing.T) {
	sequence := func(answers ...error) (func() (uint64, error), *int) {
		calls := 0
		return func() (uint64, error) {
			err := answers[min(calls, len(answers)-1)]
			calls++
			if err != nil {
				return 0, err
			}
			return 200 * gib, nil
		}, &calls
	}
	never := func() (uint64, error) { t.Fatal("READ CAPACITY(10) must not run"); return 0, nil }
	attention, calls := sequence(errSCSIUnitAttention, nil)
	if size, err := scsiCapacityFrom(attention, never); err != nil || size != 200*gib || *calls != 2 {
		t.Fatalf("after a unit attention: %d, %v, calls %d", size, err, *calls)
	}
	refused := errors.New("illegal request")
	read16, _ := sequence(refused)
	read10, calls10 := sequence(errSCSIUnitAttention, nil)
	if size, err := scsiCapacityFrom(read16, read10); err != nil || size != 200*gib || *calls10 != 2 {
		t.Fatalf("fallback: %d, %v, calls %d", size, err, *calls10)
	}
	stuck, stuckCalls := sequence(errSCSIUnitAttention)
	failing, _ := sequence(errors.New("no answer"))
	if _, err := scsiCapacityFrom(stuck, failing); !errors.Is(err, errSCSIUnitAttention) || *stuckCalls != scsiAttempts {
		t.Fatalf("both failing: %v, calls %d", err, *stuckCalls)
	}
}

func TestSCSISenseKeyReadsBothFormats(t *testing.T) {
	fixed := []byte{0x70, 0, 0x06, 0, 0, 0, 0, 10, 0, 0, 0, 0, 0x2a, 0x09}
	descriptor := []byte{0x72, 0x06, 0x2a, 0x09}
	if scsiSenseKey(fixed) != scsiSenseUnitAttention || scsiSenseKey(descriptor) != scsiSenseUnitAttention || scsiSenseKey(nil) != 0 {
		t.Fatal("sense key")
	}
}
