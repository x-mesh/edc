package edc

import (
	"debug/buildinfo"
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
	traceTLSGoVersion    = "go1.27.1"
	traceTLSGoMaxCode    = 1 << 20
	traceTLSGoMaxTable   = 64 << 20
	traceTLSGoMaxReturns = 128
)

func traceTLSGo(file *elf.File, target *traceTLSTarget) (err error) {
	section := file.Section(".gopclntab")
	if section == nil {
		section = file.Section(".data.rel.ro.gopclntab")
	}
	if section == nil {
		return nil
	}
	info, err := buildinfo.ReadFile(target.path)
	if err != nil {
		return fmt.Errorf("Go build info: %w", err)
	}
	if info.GoVersion != traceTLSGoVersion || file.Machine != elf.EM_X86_64 || file.Data != elf.ELFDATA2LSB || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) {
		return fmt.Errorf("unsupported Go TLS ABI: %s %s %s", info.GoVersion, file.Machine, file.Type)
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
	if binary.LittleEndian.Uint32(data) != 0xfffffff1 || data[4] != 0 || data[5] != 0 || data[6] != 1 || data[7] != 8 {
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
	entries := map[string]uint64{}
	returns := map[string][]uint64{}
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
			return fmt.Errorf("missing Go TLS function %s", name)
		}
		if found.End <= found.Entry || found.End-found.Entry > traceTLSGoMaxCode {
			return fmt.Errorf("invalid Go TLS function span: %s", name)
		}
		code, offset, err := traceTLSGoCode(file, found.Entry, found.End)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		entries[name], err = traceTLSGoEntry(code, offset)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name != traceTLSGoClose {
			returns[name], err = traceTLSGoReturns(code, offset)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	target.symbols = []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose}
	target.offsets = entries
	target.goReturns = returns
	return nil
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

func traceTLSGoReturns(code []byte, offset uint64) ([]uint64, error) {
	if len(code) == 0 || len(code) > traceTLSGoMaxCode || offset > math.MaxUint64-uint64(len(code)) {
		return nil, fmt.Errorf("invalid Go TLS instruction range")
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

func traceTLSGoEntry(code []byte, offset uint64) (uint64, error) {
	if len(code) == 0 || offset > math.MaxUint64-uint64(len(code)) {
		return 0, fmt.Errorf("invalid Go TLS entry range")
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
