package edc

import (
	"bytes"
	"debug/elf"
	"io"
	"math"
	"slices"
	"testing"
)

func TestTraceTLSGoReturnsDecodeInstructions(t *testing.T) {
	code := []byte{0x48, 0xb8, 0xc3, 0xc3, 0, 0, 0, 0, 0, 0, 0xc3}
	got, err := traceTLSGoReturns(elf.EM_X86_64, code, 100)
	if err != nil || !slices.Equal(got, []uint64{110}) {
		t.Fatalf("RET offsets=%v error=%v", got, err)
	}
	for _, code := range [][]byte{nil, {0x90}, {0x0f}, {0xc2, 8, 0}, bytes.Repeat([]byte{0xc3}, traceTLSGoMaxReturns+1)} {
		if _, err := traceTLSGoReturns(elf.EM_X86_64, code, 100); err == nil {
			t.Fatalf("accepted invalid return stream %x", code)
		}
	}
	if _, err := traceTLSGoReturns(elf.EM_X86_64, []byte{0xc3}, math.MaxUint64); err == nil {
		t.Fatal("accepted overflowing offset")
	}
}

func TestTraceTLSGoCodeRequiresExecutableFileBytes(t *testing.T) {
	file := &elf.File{Progs: []*elf.Prog{{ProgHeader: elf.ProgHeader{Type: elf.PT_LOAD, Flags: elf.PF_X, Vaddr: 100, Off: 20, Filesz: 2, Memsz: 10}, ReaderAt: bytes.NewReader([]byte{0x90, 0xc3})}}}
	code, offset, err := traceTLSGoCode(file, 100, 102)
	if err != nil || offset != 20 || !bytes.Equal(code, []byte{0x90, 0xc3}) {
		t.Fatalf("code=%x offset=%d error=%v", code, offset, err)
	}
	for _, span := range [][2]uint64{{100, 103}, {99, 101}, {102, 102}, {100, math.MaxUint64}} {
		if _, _, err := traceTLSGoCode(file, span[0], span[1]); err == nil {
			t.Fatalf("accepted invalid span %v", span)
		}
	}
	file.Progs[0].ReaderAt = bytes.NewReader(nil)
	if _, _, err := traceTLSGoCode(file, 100, 102); err != io.ErrUnexpectedEOF {
		t.Fatalf("short file error=%v", err)
	}
	if traceTLSReadsPlaintext([]string{traceTLSGoClose}) {
		t.Fatal("Go Close is not plaintext")
	}
}

func TestTraceTLSGoEntryFollowsStackGuard(t *testing.T) {
	guard := []byte{0x49, 0x3b, 0x66, 0x10, 0x76, 0x02, 0x55, 0x90, 0xc3}
	for _, tc := range []struct {
		code  []byte
		entry uint64
	}{
		{guard, 106},
		{append([]byte{0x4c, 0x8d, 0x64, 0x24, 0xb8}, []byte{0x4d, 0x3b, 0x66, 0x10, 0x76, 0x02, 0x55, 0x90, 0xc3}...), 111},
	} {
		got, err := traceTLSGoEntry(elf.EM_X86_64, tc.code, 100)
		if err != nil || got != tc.entry {
			t.Fatalf("entry=%d want=%d err=%v", got, tc.entry, err)
		}
	}
	for _, code := range [][]byte{nil, {0x55, 0xc3}, {0x49, 0x3b, 0x66}, {0x49, 0x3b, 0x66, 0x18, 0x76, 0x02, 0x55, 0x90, 0xc3}, {0x49, 0x3b, 0x66, 0x10, 0x77, 0x02, 0x55, 0x90, 0xc3}, {0x49, 0x3b, 0x66, 0x10, 0x76, 0xfe, 0x55, 0x90, 0xc3}, {0x49, 0x3b, 0x66, 0x10, 0x76, 0x02, 0x53, 0x90, 0xc3}} {
		if _, err := traceTLSGoEntry(elf.EM_X86_64, code, 100); err == nil {
			t.Fatalf("accepted unsupported prologue: %x", code)
		}
	}
	if _, err := traceTLSGoEntry(elf.EM_X86_64, guard, math.MaxUint64); err == nil {
		t.Fatal("accepted overflowing entry")
	}
}

func TestTraceTLSGoArm64Instructions(t *testing.T) {
	returns, err := traceTLSGoReturns(elf.EM_AARCH64, []byte{0xc0, 0x03, 0x5f, 0xd6}, 100)
	if err != nil || !slices.Equal(returns, []uint64{100}) {
		t.Fatalf("returns=%v err=%v", returns, err)
	}
	code := []byte{0x90, 0x0b, 0x40, 0xf9, 0xff, 0x63, 0x30, 0xeb, 0x49, 0x00, 0x00, 0x54, 0xfd, 0x7b, 0xbf, 0xa9}
	entry, err := traceTLSGoEntry(elf.EM_AARCH64, code, 100)
	if err != nil || entry != 112 {
		t.Fatalf("entry=%d err=%v", entry, err)
	}
}
