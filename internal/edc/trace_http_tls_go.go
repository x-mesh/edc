package edc

import (
	"debug/elf"
	"debug/gosym"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"golang.org/x/arch/x86/x86asm"
)

const (
	traceTLSGoRead       = "crypto/tls.(*Conn).Read"
	traceTLSGoWrite      = "crypto/tls.(*Conn).Write"
	traceTLSGoClose      = "crypto/tls.(*Conn).Close"
	traceTLSGoMaxCode    = 1 << 20
	traceTLSGoMaxTable   = 64 << 20
	traceTLSGoMaxReturns = 128
	traceTLSGoBuildMagic = "\xff Go buildinf:"
)

var traceTLSGoVersions = map[string]bool{
	"go1.21.5":  true,
	"go1.23.7":  true,
	"go1.23.8":  true,
	"go1.23.12": true,
	"go1.25.0":  true,
	"go1.26.5":  true,
	"go1.26.8":  true,
	"go1.27.3":  true,
	"go1.27.1":  true,
}

func traceTLSGo(file *elf.File, target *traceTLSTarget) (err error) {
	section := file.Section(".gopclntab")
	if section == nil {
		section = file.Section(".data.rel.ro.gopclntab")
	}
	if section == nil {
		return nil
	}
	if section.Size < 4 || section.ReaderAt == nil {
		return fmt.Errorf("invalid Go TLS function table size")
	}
	var tableMagic [4]byte
	if _, err := section.ReaderAt.ReadAt(tableMagic[:], 0); err != nil {
		return fmt.Errorf("Go TLS function table: %w", err)
	}
	magic := binary.LittleEndian.Uint32(tableMagic[:])
	if magic == 0xfffffffb || magic == 0xfffffffa || magic == 0xfffffff0 {
		return fmt.Errorf("unsupported Go TLS ABI: legacy Go function table (pclntab %#x)", magic)
	}
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB {
		return fmt.Errorf("unsupported Go TLS ELF format: %s %s", file.Class, file.Data)
	}
	text := file.Section(".text")
	if text == nil || section.Size > traceTLSGoMaxTable || section.Size < 72 {
		return fmt.Errorf("invalid Go TLS text or function table")
	}
	data, err := section.Data()
	if err != nil {
		return fmt.Errorf("Go TLS function table: %w", err)
	}
	if len(data) < 72 || len(data) > traceTLSGoMaxTable {
		return fmt.Errorf("invalid Go TLS function table size")
	}
	quantum := byte(1)
	if file.Machine == elf.EM_AARCH64 {
		quantum = 4
	}
	if binary.LittleEndian.Uint32(data) != 0xfffffff1 || data[4] != 0 || data[5] != 0 || data[6] != quantum || data[7] != 8 {
		return fmt.Errorf("unsupported Go TLS function table header")
	}
	nfunc := binary.LittleEndian.Uint64(data[8:16])
	if nfunc == 0 || nfunc > traceTLSGoMaxTable/16 {
		return fmt.Errorf("invalid Go TLS function count")
	}
	for _, offset := range []int{32, 40, 48, 56, 64} {
		if binary.LittleEndian.Uint64(data[offset:offset+8]) >= uint64(len(data)) {
			return fmt.Errorf("invalid Go TLS function table offset")
		}
	}
	// gosym can panic on corrupt function tables, which an explicit --tls file can contain.
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("invalid Go TLS function table: %v", recovered)
		}
	}()
	table, err := gosym.NewTable(nil, gosym.NewLineTable(data, text.Addr))
	if err != nil {
		return fmt.Errorf("Go TLS function table: %w", err)
	}
	if uint64(len(table.Funcs)) != nfunc {
		return fmt.Errorf("invalid Go TLS function count: header=%d parsed=%d", nfunc, len(table.Funcs))
	}
	functions := make(map[string]*gosym.Func, 3)
	for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
		var found *gosym.Func
		for i := range table.Funcs {
			if table.Funcs[i].Name == name {
				if found != nil {
					return fmt.Errorf("duplicate Go TLS function %s", name)
				}
				found = &table.Funcs[i]
			}
		}
		if found == nil {
			continue
		}
		functions[name] = found
	}
	if len(functions) == 0 {
		return nil
	}
	if len(functions) != 3 {
		for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
			if _, ok := functions[name]; !ok {
				return fmt.Errorf("missing Go TLS function %s", name)
			}
		}
	}
	version, err := traceTLSGoBuildVersion(file)
	if err != nil {
		return fmt.Errorf("Go build info: %w", err)
	}
	if !traceTLSGoVersions[version] || (file.Machine != elf.EM_X86_64 && file.Machine != elf.EM_AARCH64) || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return fmt.Errorf("unsupported Go TLS ABI: %s %s %s", version, file.Machine, file.Type)
	}
	entries := map[string]uint64{}
	returns := map[string][]uint64{}
	for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
		found := functions[name]
		if found.End <= found.Entry || found.End-found.Entry > traceTLSGoMaxCode {
			return fmt.Errorf("invalid Go TLS function span: %s", name)
		}
		code, offset, err := traceTLSGoCode(file, found.Entry, found.End)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entries[name], err = traceTLSGoEntry(file.Machine, code, offset)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name != traceTLSGoClose {
			returns[name], err = traceTLSGoReturns(file.Machine, code, offset)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	target.symbols = []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose}
	target.offsets = entries
	target.goReturns = returns
	target.goMachine = file.Machine
	return nil
}

func traceTLSGoBuildVersion(file *elf.File) (string, error) {
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB {
		return "", fmt.Errorf("unsupported Go build info ELF format: %s %s", file.Class, file.Data)
	}
	section := file.Section(".go.buildinfo")
	if section == nil || section.Size < 33 || section.Addralign < 16 || section.Addr%16 != 0 || section.Offset%16 != 0 || section.ReaderAt == nil {
		return "", fmt.Errorf("invalid .go.buildinfo section")
	}
	if section.Offset > math.MaxInt64 || section.Size > uint64(math.MaxInt64)-section.Offset || section.Size > math.MaxUint64-section.Offset || section.Addr > math.MaxUint64-section.Size {
		return "", fmt.Errorf("invalid .go.buildinfo range")
	}
	var mapped bool
	for _, program := range file.Progs {
		if program.Type != elf.PT_LOAD || program.Filesz == 0 || program.Filesz > math.MaxUint64-program.Vaddr || program.Filesz > math.MaxUint64-program.Off {
			continue
		}
		if section.Addr < program.Vaddr || section.Addr+section.Size > program.Vaddr+program.Filesz || section.Offset < program.Off || section.Offset+section.Size > program.Off+program.Filesz {
			continue
		}
		if section.Addr-program.Vaddr != section.Offset-program.Off {
			continue
		}
		mapped = true
		break
	}
	if !mapped {
		return "", fmt.Errorf(".go.buildinfo is outside file-backed PT_LOAD data")
	}
	const headerSize = 32
	if section.Size < headerSize+1 {
		return "", fmt.Errorf("invalid .go.buildinfo size")
	}
	header := make([]byte, headerSize)
	n, err := section.ReaderAt.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read .go.buildinfo header: %w", err)
	}
	if n != len(header) {
		return "", fmt.Errorf("short .go.buildinfo header read")
	}
	if string(header[:14]) != traceTLSGoBuildMagic || header[14] != 8 || header[15] != 2 {
		return "", fmt.Errorf("unsupported .go.buildinfo header")
	}
	var encodedLength [1]byte
	n, err = section.ReaderAt.ReadAt(encodedLength[:], headerSize)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read Go build version length: %w", err)
	}
	length, width := binary.Uvarint(encodedLength[:n])
	if width != 1 || length == 0 || length > 64 || uint64(headerSize+width)+length > section.Size {
		return "", fmt.Errorf("invalid Go build version length")
	}
	version := make([]byte, int(length))
	n, err = section.ReaderAt.ReadAt(version, headerSize+int64(width))
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read Go build version: %w", err)
	}
	if n != len(version) {
		return "", fmt.Errorf("short Go build version read")
	}
	for _, character := range version {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '.' && character != '+' && character != '-' {
			return "", fmt.Errorf("invalid Go build version characters")
		}
	}
	return string(version), nil
}
func traceTLSGoCode(file *elf.File, start, end uint64) ([]byte, uint64, error) {
	for _, program := range file.Progs {
		if program.Type != elf.PT_LOAD || program.Flags&elf.PF_X == 0 || start < program.Vaddr || end <= start || program.Filesz > math.MaxUint64-program.Vaddr {
			continue
		}
		if end > program.Vaddr+program.Filesz || program.Off > math.MaxUint64-(start-program.Vaddr) {
			continue
		}
		relative := start - program.Vaddr
		if relative > math.MaxInt64 || end-start > traceTLSGoMaxCode || program.Off+relative > math.MaxInt64 {
			return nil, 0, fmt.Errorf("invalid Go TLS file range")
		}
		code := make([]byte, int(end-start))
		if _, err := program.ReadAt(code, int64(relative)); err != nil && err != io.EOF {
			return nil, 0, err
		} else if err == io.EOF {
			return nil, 0, io.ErrUnexpectedEOF
		}
		return code, program.Off + relative, nil
	}
	return nil, 0, fmt.Errorf("Go TLS function is outside executable file data")
}

func traceTLSGoReturns(machine elf.Machine, code []byte, offset uint64) ([]uint64, error) {
	if len(code) == 0 || len(code) > traceTLSGoMaxCode || offset > math.MaxUint64-uint64(len(code)) {
		return nil, fmt.Errorf("invalid Go TLS instruction range")
	}
	if machine == elf.EM_AARCH64 {
		if len(code)%4 != 0 {
			return nil, fmt.Errorf("invalid Go TLS arm64 instruction range")
		}
		var returns []uint64
		for pos := 0; pos < len(code); pos += 4 {
			if binary.LittleEndian.Uint32(code[pos:])&0xfffffc1f == 0xd65f0000 {
				if len(returns) >= traceTLSGoMaxReturns {
					return nil, fmt.Errorf("unsupported Go TLS return at %#x", offset+uint64(pos))
				}
				returns = append(returns, offset+uint64(pos))
			}
		}
		if len(returns) == 0 {
			return nil, fmt.Errorf("Go TLS function has no RET")
		}
		return returns, nil
	}
	if machine != elf.EM_X86_64 {
		return nil, fmt.Errorf("unsupported Go TLS machine")
	}
	var returns []uint64
	for pos := 0; pos < len(code); {
		instruction, err := x86asm.Decode(code[pos:], 64)
		if err != nil || instruction.Len == 0 || instruction.Op == 0 {
			return nil, fmt.Errorf("invalid Go TLS instruction at %#x", offset+uint64(pos))
		}
		if instruction.Op == x86asm.RET {
			if instruction.Len != 1 || len(returns) >= traceTLSGoMaxReturns {
				return nil, fmt.Errorf("unsupported Go TLS return at %#x", offset+uint64(pos))
			}
			returns = append(returns, offset+uint64(pos))
		}
		pos += instruction.Len
	}
	if len(returns) == 0 {
		return nil, fmt.Errorf("Go TLS function has no RET")
	}
	return returns, nil
}

func traceTLSGoEntry(machine elf.Machine, code []byte, offset uint64) (uint64, error) {
	if len(code) == 0 || offset > math.MaxUint64-uint64(len(code)) {
		return 0, fmt.Errorf("invalid Go TLS entry range")
	}
	if machine == elf.EM_AARCH64 {
		for pos := 0; pos < 16 && pos+8 <= len(code); pos += 4 {
			if binary.LittleEndian.Uint32(code[pos:])&0xff00001f == 0x54000009 {
				return offset + uint64(pos+4), nil
			}
		}
		return 0, fmt.Errorf("unsupported Go TLS arm64 prologue")
	}
	if machine != elf.EM_X86_64 {
		return 0, fmt.Errorf("unsupported Go TLS machine")
	}
	pos := 0
	stack := x86asm.RSP
	decode := func() (x86asm.Inst, error) {
		if pos >= len(code) {
			return x86asm.Inst{}, fmt.Errorf("missing Go TLS prologue")
		}
		instruction, err := x86asm.Decode(code[pos:], 64)
		if err != nil || instruction.Len == 0 {
			return instruction, fmt.Errorf("invalid Go TLS prologue")
		}
		pos += instruction.Len
		return instruction, nil
	}
	instruction, err := decode()
	if err != nil {
		return 0, err
	}
	if instruction.Op == x86asm.LEA {
		memory, ok := instruction.Args[1].(x86asm.Mem)
		if instruction.Args[0] != x86asm.R12 || !ok || memory.Base != x86asm.RSP || memory.Index != 0 || memory.Disp >= 0 {
			return 0, fmt.Errorf("unsupported Go TLS stack adjustment")
		}
		stack = x86asm.R12
		instruction, err = decode()
		if err != nil {
			return 0, err
		}
	}
	memory, ok := instruction.Args[1].(x86asm.Mem)
	if instruction.Op != x86asm.CMP || instruction.Args[0] != stack || !ok || memory.Base != x86asm.R14 || memory.Index != 0 || memory.Disp != 16 {
		return 0, fmt.Errorf("unsupported Go TLS stack guard")
	}
	instruction, err = decode()
	if err != nil {
		return 0, err
	}
	branch, ok := instruction.Args[0].(x86asm.Rel)
	if instruction.Op != x86asm.JBE || !ok || int64(pos)+int64(branch) <= int64(pos) || int64(pos)+int64(branch) >= int64(len(code)) {
		return 0, fmt.Errorf("unsupported Go TLS stack branch")
	}
	entry := pos
	instruction, err = decode()
	if err != nil || instruction.Op != x86asm.PUSH || instruction.Args[0] != x86asm.RBP || instruction.Len != 1 {
		return 0, fmt.Errorf("unsupported Go TLS frame prologue")
	}
	return offset + uint64(entry), nil
}
