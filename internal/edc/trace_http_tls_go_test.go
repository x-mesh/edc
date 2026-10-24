package edc

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"io"
	"math"
	"os"
	"runtime"
	"slices"
	"testing"
)

type traceTLSGoCountingReaderAt struct {
	reader *bytes.Reader
	read   int
}

func (reader *traceTLSGoCountingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := reader.reader.ReadAt(buffer, offset)
	reader.read += n
	return n, err
}

func TestTraceTLSGoSentinelUsesSectionNames(t *testing.T) {
	if runtime.GOOS == "linux" && !traceTLSGoSentinel(os.Args[0]) {
		t.Fatal("Go test executable has no Go pclntab sentinel")
	}
	path := t.TempDir() + "/not-elf"
	if err := os.WriteFile(path, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if traceTLSGoSentinel(path) {
		t.Fatal("plain text has a Go pclntab sentinel")
	}
}

func TestTraceTLSGoVersionsAreExact(t *testing.T) {
	want := []string{"go1.21.5", "go1.23.7", "go1.23.8", "go1.23.12", "go1.25.0", "go1.26.5", "go1.26.8", "go1.27.1", "go1.27.3"}
	if len(traceTLSGoVersions) != len(want) {
		t.Fatalf("supported versions=%v", traceTLSGoVersions)
	}
	for _, version := range want {
		if !traceTLSGoVersions[version] {
			t.Errorf("missing supported version %s", version)
		}
	}
	for _, version := range []string{"go1.21.4", "go1.21.6", "go1.23.6", "go1.23.9", "go1.23.13", "go1.26.4", "go1.26.6", "go1.27.2", "go1.27.4", "devel go1.28", "custom"} {
		if traceTLSGoVersions[version] {
			t.Errorf("accepted unsupported version %s", version)
		}
	}
}

func traceTLSGoBuildInfoFixture(version string) *elf.File {
	data := make([]byte, 0x100)
	copy(data, traceTLSGoBuildMagic)
	data[14] = 8
	data[15] = 2
	var length [binary.MaxVarintLen64]byte
	w := binary.PutUvarint(length[:], uint64(len(version)))
	copy(data[32:], length[:w])
	copy(data[32+w:], version)
	return &elf.File{
		FileHeader: elf.FileHeader{Class: elf.ELFCLASS64, Data: elf.ELFDATA2LSB},
		Sections:   []*elf.Section{{SectionHeader: elf.SectionHeader{Name: ".go.buildinfo", Addr: 0x1010, Offset: 0x10, Size: 0x80, Addralign: 16}, ReaderAt: bytes.NewReader(data)}},
		Progs: []*elf.Prog{
			{ProgHeader: elf.ProgHeader{Type: elf.PT_TLS, Vaddr: 0, Filesz: 0}},
			{ProgHeader: elf.ProgHeader{Type: elf.PT_GNU_STACK, Vaddr: 0, Filesz: 0}},
			{ProgHeader: elf.ProgHeader{Type: elf.PT_LOAD, Vaddr: 0x1000, Off: 0, Filesz: 0x200}},
		},
	}
}

func TestTraceTLSGoBuildVersion(t *testing.T) {
	file := traceTLSGoBuildInfoFixture("go1.23.12")
	if version, err := traceTLSGoBuildVersion(file); err != nil || version != "go1.23.12" {
		t.Fatalf("version=%q error=%v", version, err)
	}
	for _, tc := range []struct {
		name string
		edit func(*elf.File)
	}{
		{"zero_filesz_only", func(file *elf.File) { file.Progs = file.Progs[:2] }},
		{"alignment", func(file *elf.File) { file.Sections[0].Addralign = 8 }},
		{"overflow_address", func(file *elf.File) { file.Sections[0].Addr = ^uint64(0) - 8 }},
		{"overflow_file_offset", func(file *elf.File) { file.Sections[0].Offset = ^uint64(0) - 8 }},
		{"file_offset_above_int64", func(file *elf.File) { file.Sections[0].Offset = uint64(math.MaxInt64) + 1 }},
		{"file_range_above_int64", func(file *elf.File) { file.Sections[0].Size = uint64(math.MaxInt64) }},
		{"elfclass32", func(file *elf.File) { file.Class = elf.ELFCLASS32 }},
		{"big_endian", func(file *elf.File) { file.Data = elf.ELFDATA2MSB }},
		{"section_address_offset_mismatch", func(file *elf.File) { file.Sections[0].Offset++ }},
		{"misaligned_section", func(file *elf.File) { file.Sections[0].Addr++ }},
		{"missing_section", func(file *elf.File) { file.Sections = nil }},
		{"missing_section_reader", func(file *elf.File) { file.Sections[0].ReaderAt = nil }},
		{"bad_magic", func(file *elf.File) { file.Sections[0].ReaderAt = bytes.NewReader(make([]byte, 0x100)) }},
		{"bad_pointer_size", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 4
			data[15] = 2
			data[32] = 9
			copy(data[33:], "go1.23.12")
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"bad_flags", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 0
			data[32] = 9
			copy(data[33:], "go1.23.12")
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"multibyte_length", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			data[32] = 0x81
			data[33] = 0x01
			copy(data[34:], bytes.Repeat([]byte{'x'}, 129))
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"zero_length", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"long_version", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			data[32] = 65
			copy(data[33:], bytes.Repeat([]byte{'x'}, 65))
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"overflow_length", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			copy(data[32:], bytes.Repeat([]byte{0xff}, binary.MaxVarintLen64))
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"invalid_version_characters", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			data[32] = 3
			copy(data[33:], []byte{'g', 'o', 0x1b})
			file.Sections[0].ReaderAt = bytes.NewReader(data)
		}},
		{"truncated_version", func(file *elf.File) {
			data := bytes.Repeat([]byte{0}, 0x100)
			copy(data, traceTLSGoBuildMagic)
			data[14] = 8
			data[15] = 2
			data[32] = 9
			copy(data[33:], "short")
			file.Sections[0].Size = 38
			file.Sections[0].ReaderAt = bytes.NewReader(data[:38])
		}},
		{"short_read", func(file *elf.File) { file.Sections[0].ReaderAt = bytes.NewReader(make([]byte, 20)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := traceTLSGoBuildInfoFixture("go1.23.12")
			tc.edit(file)
			if _, err := traceTLSGoBuildVersion(file); err == nil {
				t.Fatal("accepted invalid build info")
			}
		})
	}
	t.Run("huge_section_bounded_read", func(t *testing.T) {
		file := traceTLSGoBuildInfoFixture("go1.23.12")
		file.Sections[0].Size = 1<<40 - 0x10
		file.Progs[2].Filesz = 1 << 40
		reader := &traceTLSGoCountingReaderAt{reader: bytes.NewReader(bytes.Repeat([]byte{0}, 0x100))}
		copyData := bytes.Repeat([]byte{0}, 0x100)
		copy(copyData, traceTLSGoBuildMagic)
		copyData[14] = 8
		copyData[15] = 2
		copyData[32] = 9
		copy(copyData[33:], "go1.23.12")
		reader.reader = bytes.NewReader(copyData)
		file.Sections[0].ReaderAt = reader
		version, err := traceTLSGoBuildVersion(file)
		if err != nil || version != "go1.23.12" || reader.read != 32+1+len(version) {
			t.Fatalf("bounded version=%q error=%v", version, err)
		}
	})
}

func TestTraceTLSGoRejectsMissingSectionReader(t *testing.T) {
	file := &elf.File{
		FileHeader: elf.FileHeader{Class: elf.ELFCLASS64, Data: elf.ELFDATA2LSB, Machine: elf.EM_X86_64, Type: elf.ET_EXEC},
		Sections:   []*elf.Section{{SectionHeader: elf.SectionHeader{Name: ".gopclntab", Size: 72}}},
	}
	if err := traceTLSGo(file, &traceTLSTarget{}); err == nil {
		t.Fatal("accepted function table without a reader")
	}
}

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
