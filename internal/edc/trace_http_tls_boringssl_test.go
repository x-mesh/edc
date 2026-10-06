package edc

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func traceTLSBoringSSLFixture(t *testing.T, order binary.ByteOrder, note, code []byte, executable bool) string {
	t.Helper()
	data := make([]byte, 0x2000+3*64)
	copy(data, "\x7fELF\x02\x01\x01")
	if order == binary.BigEndian {
		data[5] = 2
	}
	order.PutUint16(data[16:], uint16(elf.ET_EXEC))
	order.PutUint16(data[18:], uint16(traceTLSMachines[runtime.GOARCH]))
	order.PutUint32(data[20:], 1)
	order.PutUint64(data[32:], 64)
	order.PutUint64(data[40:], 0x2000)
	order.PutUint16(data[52:], 64)
	order.PutUint16(data[54:], 56)
	order.PutUint16(data[56:], 1)
	order.PutUint16(data[58:], 64)
	order.PutUint16(data[60:], 3)
	order.PutUint16(data[62:], 1)
	program := data[64:120]
	order.PutUint32(program, uint32(elf.PT_LOAD))
	flags := elf.PF_R
	if executable {
		flags |= elf.PF_X
	}
	order.PutUint32(program[4:], uint32(flags))
	order.PutUint64(program[8:], 0x1000)
	order.PutUint64(program[16:], 0x401000)
	order.PutUint64(program[32:], uint64(len(code)))
	order.PutUint64(program[40:], 0x1000)
	copy(data[0x1000:], code)
	names := []byte("\x00.shstrtab\x00.note.gnu.build-id\x00")
	copy(data[128:], names)
	copy(data[512:], note)
	section := data[0x2040:0x2080]
	order.PutUint32(section, 1)
	order.PutUint32(section[4:], uint32(elf.SHT_STRTAB))
	order.PutUint64(section[24:], 128)
	order.PutUint64(section[32:], uint64(len(names)))
	section = data[0x2080:0x20c0]
	order.PutUint32(section, 11)
	order.PutUint32(section[4:], uint32(elf.SHT_NOTE))
	order.PutUint64(section[24:], 512)
	order.PutUint64(section[32:], uint64(len(note)))
	order.PutUint64(section[48:], 4)
	path := filepath.Join(t.TempDir(), "bun")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func traceTLSBoringSSLNote(order binary.ByteOrder, id []byte) []byte {
	note := make([]byte, 16+(len(id)+3)&^3)
	order.PutUint32(note, 4)
	order.PutUint32(note[4:], uint32(len(id)))
	order.PutUint32(note[8:], 3)
	copy(note[12:], "GNU\x00")
	copy(note[16:], id)
	return note
}

func TestTraceTLSBoringSSLRequiresIdentityAndCode(t *testing.T) {
	if _, ok := traceTLSMachines[runtime.GOARCH]; !ok {
		t.Skip("--tls supports amd64 and arm64")
	}
	id := []byte{1, 2, 3, 4, 5}
	code := []byte{0x55, 0xc3, 0x56, 0xc3, 0x57, 0xc3}
	functions := make([]traceTLSBoringSSLFunction, 0, 3)
	for i, name := range []string{"SSL_read", "SSL_write", "SSL_free"} {
		digest := sha256.Sum256(code[i*2 : i*2+2])
		functions = append(functions, traceTLSBoringSSLFunction{name, uint64(0x1000 + i*2), 2, hex.EncodeToString(digest[:])})
	}
	previous := traceTLSBoringSSLBuilds
	traceTLSBoringSSLBuilds = map[string]traceTLSBoringSSLBuild{
		hex.EncodeToString(id): {traceTLSMachines[runtime.GOARCH], functions},
	}
	t.Cleanup(func() { traceTLSBoringSSLBuilds = previous })
	for _, test := range []struct {
		name       string
		id         []byte
		code       []byte
		executable bool
		want       []string
		errorText  string
	}{
		{"known", id, code, true, []string{"SSL_read", "SSL_write", "SSL_free"}, ""},
		{"unknown", []byte{9, 9, 9, 9, 9}, code, true, nil, ""},
		{"modified", id, []byte{0x55, 0xc3, 0x56, 0xc3, 0x90, 0xc3}, true, nil, "SSL_free code does not match"},
		{"data segment", id, code, false, nil, "outside executable file data"},
		{"memory without file data", id, code[:5], true, nil, "outside executable file data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := traceTLSBoringSSLFixture(t, binary.LittleEndian, traceTLSBoringSSLNote(binary.LittleEndian, test.id), test.code, test.executable)
			for _, withSymtab := range []bool{false, true} {
				target, err := traceTLSReadFile(path, withSymtab)
				if test.errorText != "" {
					if err == nil || !strings.Contains(err.Error(), test.errorText) || len(target.symbols) != 0 || len(target.offsets) != 0 {
						t.Fatalf("partial or accepted target: %#v, %v", target, err)
					}
					continue
				}
				if err != nil || !slices.Equal(target.symbols, test.want) || len(target.offsets) != len(test.want) {
					t.Fatalf("target = %#v, %v", target, err)
				}
				for i, name := range test.want {
					if target.offsets[name] != uint64(0x1000+i*2) {
						t.Fatalf("%s offset = %#x", name, target.offsets[name])
					}
				}
			}
			finder := &traceTLSFinder{seen: map[[2]uint64]bool{}}
			finder.add(path, path, false)
			if (len(finder.targets) == 1) != (len(test.want) != 0) {
				t.Fatalf("automatic targets = %#v", finder.targets)
			}
			if (len(finder.notices) == 1) != (test.errorText != "") {
				t.Fatalf("automatic notices = %q", finder.notices)
			}
		})
	}
	t.Run("wrong machine", func(t *testing.T) {
		path := traceTLSBoringSSLFixture(t, binary.LittleEndian, traceTLSBoringSSLNote(binary.LittleEndian, id), code, true)
		file, err := elf.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		file.Machine = elf.EM_386
		target := traceTLSTarget{offsets: map[string]uint64{}}
		if err := traceTLSBoringSSL(file, &target); err != nil || len(target.symbols) != 0 || len(target.offsets) != 0 {
			t.Fatalf("wrong machine target = %#v, %v", target, err)
		}
	})
}

func TestTraceTLSBoringSSLProfileBuildsHaveTLSFunctions(t *testing.T) {
	for _, id := range []string{"2bbcd6d3ddc6b1a248d1bfb2c64a09e4642e7a52", "5afca2666bfab8605a934f1b6231dacae0518a5f"} {
		build, ok := traceTLSBoringSSLBuilds[id]
		if !ok || build.machine != elf.EM_X86_64 || len(build.functions) != 3 {
			t.Fatalf("profile %s = %#v", id, build)
		}
		for index, name := range []string{"SSL_read", "SSL_write", "SSL_free"} {
			function := build.functions[index]
			if function.name != name || function.offset == 0 || function.size == 0 || len(function.digest) != sha256.Size*2 {
				t.Fatalf("profile %s function %#v", id, function)
			}
		}
	}
}

func TestTraceTLSBoringSSLOfficialBunProfiles(t *testing.T) {
	profiles := []struct {
		path string
		id   string
	}{
		{"/tmp/bun-profile-1.4.1/bun-linux-x64-profile/bun-profile", "2bbcd6d3ddc6b1a248d1bfb2c64a09e4642e7a52"},
		{"/tmp/bun-profile.d5u6IJ/bun-linux-x64-profile/bun-profile", "5afca2666bfab8605a934f1b6231dacae0518a5f"},
	}
	for _, profile := range profiles {
		if _, err := os.Stat(profile.path); err != nil {
			t.Skipf("official Bun profile unavailable: %s", profile.path)
		}
		file, err := elf.Open(profile.path)
		if err != nil {
			t.Fatal(err)
		}
		target := traceTLSTarget{path: profile.path, offsets: map[string]uint64{}}
		err = traceTLSBoringSSL(file, &target)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(target.symbols, []string{"SSL_read", "SSL_write", "SSL_free"}) {
			t.Fatalf("profile %s symbols=%q", profile.id, target.symbols)
		}
		for _, name := range target.symbols {
			if target.offsets[name] == 0 {
				t.Fatalf("profile %s missing %s", profile.id, name)
			}
		}
	}
}

func TestTraceTLSBuildIDReadsNotes(t *testing.T) {
	id := []byte{1, 2, 3, 4, 5}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		valid := traceTLSBoringSSLNote(order, id)
		other := bytes.Clone(valid)
		order.PutUint32(other[8:], 4)
		badSize := bytes.Clone(valid)
		order.PutUint32(badSize, ^uint32(0))
		for _, test := range []struct {
			name string
			note []byte
			want string
			fail bool
		}{
			{"GNU", valid, "0102030405", false},
			{"other then GNU", append(bytes.Clone(other), valid...), "0102030405", false},
			{"other", other, "", false},
			{"empty", nil, "", false},
			{"short header", valid[:11], "", true},
			{"short descriptor", valid[:len(valid)-1], "", true},
			{"overflow", badSize, "", true},
			{"large", make([]byte, 4097), "", true},
		} {
			t.Run(order.String()+"/"+test.name, func(t *testing.T) {
				path := traceTLSBoringSSLFixture(t, order, test.note, nil, true)
				file, err := elf.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				got, err := traceTLSBuildID(file)
				if got != test.want || (err != nil) != test.fail {
					t.Fatalf("build ID = %q, %v", got, err)
				}
			})
		}
	}
}
