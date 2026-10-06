package edc

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const maxTraceTLSBuildIDNoteSize = 4096

var errTraceTLSBoringSSL = errors.New("BoringSSL build validation failed")

type traceTLSBoringSSLFunction struct {
	name   string
	offset uint64
	size   uint64
	digest string
}

type traceTLSBoringSSLBuild struct {
	machine   elf.Machine
	functions []traceTLSBoringSSLFunction
}

var traceTLSBoringSSLBuilds = map[string]traceTLSBoringSSLBuild{
	// Claude Code 2.1.291의 Bun 1.4.3(eecfd55de). 공식 Bun 1.4.2 profile과 비교하고 로컬 HTTPS로 확인한 위치다.
	"ca2032b38650b44e05b2074617d524c7475c80f0": {
		machine: elf.EM_X86_64,
		functions: []traceTLSBoringSSLFunction{
			{"SSL_read", 0x232d160, 251, "abe8f0a43aa3f29650b3841a23a25e6092cf46a9aeda3a198ef2245d7f9ed455"},
			{"SSL_write", 0x232d550, 388, "8e7029193146a0b027304ef26906e51d4ba8af66a8a4c3ff5af653d253a5430e"},
			// SSL_free가 인라인된 빌드라 같은 SSL 객체를 받는 bssl::SSLImpl 소멸자에서 연결 상태를 지운다.
			{"SSL_free", 0x232bb40, 474, "6860bf0bed1a3415f345dc3526382f6ccd4bd2fd32a5640a20d976ea94b4ef93"},
		},
	},
}

func traceTLSBoringSSL(file *elf.File, target *traceTLSTarget) error {
	id, err := traceTLSBuildID(file)
	if err != nil {
		return err
	}
	build, ok := traceTLSBoringSSLBuilds[id]
	if !ok || file.Machine != build.machine {
		return nil
	}
	for _, function := range build.functions {
		var program *elf.Prog
		for _, candidate := range file.Progs {
			if candidate.Type == elf.PT_LOAD && candidate.Flags&elf.PF_X != 0 &&
				candidate.Off <= function.offset && function.offset-candidate.Off <= candidate.Filesz &&
				function.size <= candidate.Filesz-(function.offset-candidate.Off) {
				program = candidate
				break
			}
		}
		if program == nil {
			return fmt.Errorf("%w (build ID %s): %s is outside executable file data", errTraceTLSBoringSSL, id, function.name)
		}
		hash := sha256.New()
		reader := io.NewSectionReader(program, int64(function.offset-program.Off), int64(function.size))
		if _, err := io.CopyN(hash, reader, int64(function.size)); err != nil {
			return fmt.Errorf("%w (build ID %s): read %s: %w", errTraceTLSBoringSSL, id, function.name, err)
		}
		if hex.EncodeToString(hash.Sum(nil)) != function.digest {
			return fmt.Errorf("%w (build ID %s): %s code does not match", errTraceTLSBoringSSL, id, function.name)
		}
	}
	for _, function := range build.functions {
		if _, exists := target.offsets[function.name]; !exists {
			target.symbols = append(target.symbols, function.name)
			target.offsets[function.name] = function.offset
		}
	}
	return nil
}

func traceTLSBuildID(file *elf.File) (string, error) {
	section := file.Section(".note.gnu.build-id")
	if section == nil || section.Type != elf.SHT_NOTE {
		return "", nil
	}
	if section.Size > maxTraceTLSBuildIDNoteSize {
		return "", fmt.Errorf("GNU build ID note is too large: %d", section.Size)
	}
	data, err := section.Data()
	if err != nil {
		return "", err
	}
	for len(data) > 0 {
		if len(data) < 12 {
			return "", fmt.Errorf("truncated GNU build ID note")
		}
		nameSize := uint64(file.ByteOrder.Uint32(data[:4]))
		descSize := uint64(file.ByteOrder.Uint32(data[4:8]))
		typeID := file.ByteOrder.Uint32(data[8:12])
		nameEnd := uint64(12) + nameSize
		descStart := uint64(12) + (nameSize+3)&^uint64(3)
		descEnd := descStart + descSize
		end := descStart + (descSize+3)&^uint64(3)
		if nameEnd > uint64(len(data)) || end > uint64(len(data)) {
			return "", fmt.Errorf("truncated GNU build ID note")
		}
		if typeID == 3 && string(data[12:nameEnd]) == "GNU\x00" {
			return hex.EncodeToString(data[descStart:descEnd]), nil
		}
		data = data[end:]
	}
	return "", nil
}
