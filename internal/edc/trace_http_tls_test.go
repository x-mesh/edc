package edc

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestTraceTLSMappedLibrariesReadsTLSLibraryPaths(t *testing.T) {
	maps := []byte(`7f00-7f01 r--p 00000000 08:03 123  /usr/lib/x86_64-linux-gnu/libssl.so.3
7f01-7f02 r-xp 00001000 08:03 123  /usr/lib/x86_64-linux-gnu/libssl.so.3
7f02-7f03 r--p 00000000 08:03 124  /usr/lib/x86_64-linux-gnu/libcrypto.so.3
7f03-7f04 r--p 00000000 00:23 125  /usr/lib/libssl.so.1.1 (deleted)
7f04-7f05 r--p 00000000 00:24 126  /opt/my app/lib/libssl.so.3
00400000-00401000 r-xp 00000000 00:25 127  /usr/local/lib/libssl.so.1.0.2
7f05-7f06 rw-p 00000000 00:00 0
7f06-7f07 r--p 00000000 00:00 0  [vdso]
7f07-7f08 r--p 00000000 08:03 128  /usr/lib/x86_64-linux-gnu/libgnutls.so.30.40.3
7f08-7f09 r--p 00000000 08:03 129  /usr/lib/x86_64-linux-gnu/libgnutls-dane.so.0
`)
	want := []traceTLSMapping{
		{"7f00-7f01", "/usr/lib/x86_64-linux-gnu/libssl.so.3"},
		{"7f03-7f04", "/usr/lib/libssl.so.1.1 (deleted)"},
		{"7f04-7f05", "/opt/my app/lib/libssl.so.3"},
		{"400000-401000", "/usr/local/lib/libssl.so.1.0.2"},
		{"7f07-7f08", "/usr/lib/x86_64-linux-gnu/libgnutls.so.30.40.3"},
	}
	if got := traceTLSMappedLibraries(maps); !slices.Equal(got, want) {
		t.Fatalf("libraries = %q, want %q", got, want)
	}
}

// traceTLSHostLibssl은 host에서 SSL_read를 정의한 libssl이다. 없는 host(macOS 등)에서는 ELF가 필요한 test를 건너뛴다.
func traceTLSHostLibssl(t *testing.T) string {
	t.Helper()
	for _, pattern := range traceTLSHostLibraries {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			if target, err := traceTLSReadFile(path, false); err == nil && traceTLSReadsPlaintext(target.symbols) {
				return path
			}
		}
	}
	t.Skip("no libssl with SSL_read on this host")
	return ""
}

// GnuTLS의 송수신 함수도 평문을 읽는 함수로 고른다. gnutls_deinit만으로는 붙이지 않는다.
func TestTraceTLSFileSymbolsReadsGnuTLS(t *testing.T) {
	if !slices.Contains(traceTLSHostLibraries, "/usr/lib/*/libgnutls.so*") {
		t.Fatalf("host libraries = %q", traceTLSHostLibraries)
	}
	if traceTLSReadsPlaintext([]string{"SSL_free", "gnutls_deinit"}) || !traceTLSReadsPlaintext([]string{"gnutls_deinit", "gnutls_record_recv"}) {
		t.Fatal("only the free functions must not count as plaintext")
	}
	for _, pattern := range traceTLSHostLibraries {
		if !strings.Contains(pattern, "libgnutls") {
			continue
		}
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			target, err := traceTLSReadFile(path, false)
			if err != nil {
				continue
			}
			for _, want := range []string{"gnutls_record_send", "gnutls_record_recv", "gnutls_deinit"} {
				if !slices.Contains(target.symbols, want) {
					t.Fatalf("%s symbols = %q, missing %s", path, target.symbols, want)
				}
			}
			return
		}
	}
	t.Skip("no libgnutls on this host")
}

// uprobe는 파일 위치에 붙으므로, 읽은 위치의 byte가 그 함수의 첫 명령이어야 한다. section에서 가상 주소로 읽은 byte와
// 파일에서 위치로 읽은 byte를 비교한다.
func TestTraceTLSReadFileFindsFunctionOffsets(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	target, err := traceTLSReadFile(libssl, false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := elf.Open(libssl)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw, err := os.ReadFile(libssl)
	if err != nil {
		t.Fatal(err)
	}
	symbols, err := file.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range symbols {
		offset, ok := target.offsets[symbol.Name]
		if symbol.Name != "SSL_read" && symbol.Name != "SSL_write" || symbol.Section == elf.SHN_UNDEF {
			continue
		}
		section := file.Sections[symbol.Section]
		data, err := section.Data()
		if err != nil {
			t.Fatal(err)
		}
		want := data[symbol.Value-section.Addr:][:16]
		if !ok || offset+16 > uint64(len(raw)) || !bytes.Equal(raw[offset:offset+16], want) {
			t.Fatalf("%s offset %#x (%t) does not hold the function", symbol.Name, offset, ok)
		}
	}
	if len(target.offsets) != len(target.symbols) {
		t.Fatalf("offsets = %v, symbols = %q", target.offsets, target.symbols)
	}
}

// 흔한 libssl은 실행 segment의 파일 위치와 주소가 같아서 변환이 틀려도 위 test를 통과한다. non-PIE Go test binary는
// 주소가 0x400000에서 시작하므로 주소를 파일 위치로 바꾸는 계산을 실제로 시험한다.
func TestTraceTLSFileOffsetFollowsTheSegment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the test binary is ELF only on Linux")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file, err := elf.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if !slices.ContainsFunc(file.Progs, func(program *elf.Prog) bool {
		return program.Type == elf.PT_LOAD && program.Flags&elf.PF_X != 0 && program.Off != program.Vaddr
	}) {
		t.Skip("the test binary has no executable segment whose file offset differs from its address")
	}
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	text := file.Section(".text")
	data, err := text.Data()
	if err != nil {
		t.Fatal(err)
	}
	// go test는 symbol 표를 지우므로 .text 안의 위치를 직접 고른다.
	for _, at := range []uint64{0, 16, uint64(len(data)) / 2, uint64(len(data)) - 16} {
		offset, ok := traceTLSFileOffset(file, text.Addr+at)
		if !ok || offset+16 > uint64(len(raw)) || !bytes.Equal(raw[offset:offset+16], data[at:at+16]) {
			t.Fatalf("address %#x: offset %#x (%t) does not hold the .text bytes", text.Addr+at, offset, ok)
		}
	}
}

func copyTraceTLSFile(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if _, err := io.Copy(target, source); err != nil {
		t.Fatal(err)
	}
}

// 같은 파일은 host 경로와 여러 process에서 보여도 한 번만 붙인다. process가 적재한 libssl은 map_files로 연다. maps의
// 경로를 /proc/<pid>/root에 붙여 열면 chroot한 process가 고른 파일을 열게 된다.
func TestResolveTraceTLSTargetsFindsEachLibraryOnce(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	root := t.TempDir()
	host := filepath.Join(root, "host", "libssl.so.3")
	copyTraceTLSFile(t, libssl, host)
	proc := filepath.Join(root, "proc")
	write := func(pid, name, content string) {
		path := filepath.Join(proc, pid, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mapped := func(pid string) string { return filepath.Join(proc, pid, "map_files", "7f00-7f01") }
	mkdir := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mapping := "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n"
	// 100은 container라 host와 다른 libssl이다. 101은 같은 container의 다른 process라 같은 파일이다.
	write("100", "maps", mapping)
	copyTraceTLSFile(t, libssl, mapped("100"))
	write("101", "maps", mapping)
	mkdir(mapped("101"))
	if err := os.Link(mapped("100"), mapped("101")); err != nil {
		t.Fatal(err)
	}
	// 102는 host libssl을 쓴다. 103은 libssl이 없고 실행 파일에도 SSL 함수가 없다.
	write("102", "maps", mapping)
	mkdir(mapped("102"))
	if err := os.Symlink(host, mapped("102")); err != nil {
		t.Fatal(err)
	}
	write("103", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /usr/bin/sleep\n")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyTraceTLSFile(t, executable, filepath.Join(proc, "103", "exe"))
	// 104는 libssl 이름의 다른 파일이다. 경로는 그 process가 정하므로 안내에 넣을 때 제어 문자를 escape한다.
	write("104", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/\x1b[31m/libssl.so.3\n")
	write("104", "map_files/7f00-7f01", "not an ELF file")
	// 105는 maps를 읽기 전에 끝난 process다. self는 숫자가 아니라 건너뛴다.
	if err := os.MkdirAll(filepath.Join(proc, "105"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("self", "maps", mapping)
	// 106은 패키지 업데이트로 지워진 libssl을 아직 쓴다. 107은 libssl 없이 실행 파일이 SSL 함수를 내보낸다(node).
	write("106", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3 (deleted)\n")
	copyTraceTLSFile(t, libssl, mapped("106"))
	write("107", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /usr/bin/node\n")
	copyTraceTLSFile(t, libssl, filepath.Join(proc, "107", "exe"))
	// 108은 chroot한 뒤 maps의 경로와 map_files 자리에 FIFO를 둔 process다. 열면 멈추므로 건너뛴다.
	write("108", "maps", mapping)
	for _, fifo := range []string{mapped("108"), filepath.Join(proc, "108", "root", "usr", "lib", "libssl.so.3")} {
		mkdir(fifo)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release := filepath.Join(root, "osrelease")
	if err := os.WriteFile(release, []byte("6.17.0-14-generic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	previousRoot, previousLibraries, previousRelease, previousProblem := traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSExecProblem
	traceProcRoot, traceTLSHostLibraries, traceKernelRelease = proc, []string{filepath.Join(root, "host", "libssl.so*")}, release
	traceTLSExecProblem = func() string { return "" }
	t.Cleanup(func() {
		traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSExecProblem = previousRoot, previousLibraries, previousRelease, previousProblem
	})

	finder, notices, code, err := resolveTraceTLSTargets(traceTLSAuto)
	if err != nil || code != 0 || !finder.rescan {
		t.Fatalf("resolve = %v, %d", err, code)
	}
	var paths []string
	for _, target := range finder.targets {
		paths = append(paths, target.path)
		if !slices.Contains(target.symbols, "SSL_read") || !slices.Contains(target.symbols, "SSL_write") {
			t.Fatalf("%s symbols = %q", target.path, target.symbols)
		}
	}
	if want := []string{host, mapped("100"), mapped("106"), filepath.Join(proc, "107", "exe")}; !slices.Equal(paths, want) {
		t.Fatalf("targets = %q, want %q", paths, want)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], filepath.Join(proc, "104", "root", "usr", "lib", `\x1b[31m`)) || strings.Contains(notices[0], "\x1b") {
		t.Fatalf("notices = %q", notices)
	}
}

func TestResolveTraceTLSTargetsChecksAnExplicitPath(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("--tls supports amd64 and arm64")
	}
	notELF := filepath.Join(t.TempDir(), "libssl.so.3")
	if err := os.WriteFile(notELF, []byte("text"), 0o644); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// i386 libssl은 인자를 stack으로 받아 BPF가 읽는 register와 맞지 않는다.
	header := make([]byte, 52)
	copy(header, "\x7fELF\x01\x01\x01")
	binary.LittleEndian.PutUint16(header[16:], 3)
	binary.LittleEndian.PutUint16(header[18:], 3)
	binary.LittleEndian.PutUint32(header[20:], 1)
	binary.LittleEndian.PutUint16(header[40:], 52)
	elf32 := filepath.Join(t.TempDir(), "libssl.so.3")
	if err := os.WriteFile(elf32, header, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notELF, executable, filepath.Join(t.TempDir(), "missing"), elf32} {
		if _, _, code, err := resolveTraceTLSTargets(traceTLSMode(path)); code != 2 || err == nil {
			t.Fatalf("--tls=%s = %d, %v", path, code, err)
		}
	}
	if _, err := traceTLSReadFile(elf32, true); err == nil || !strings.Contains(err.Error(), "ELFCLASS32") {
		t.Fatalf("32-bit ELF = %v", err)
	}
	libssl := traceTLSHostLibssl(t)
	finder, _, code, err := resolveTraceTLSTargets(traceTLSMode(libssl))
	if err != nil || code != 0 || len(finder.targets) != 1 || finder.targets[0].path != libssl || finder.rescan {
		t.Fatalf("--tls=%s = %+v, %d, %v", libssl, finder, code, err)
	}
}

// trace 중에 exec 알림을 받지 못하면 새로 시작한 프로그램의 첫 요청을 놓칠 수 있으므로, 화면을 열기 전에 알린다.
func TestResolveTraceTLSTargetsNoticesMissingExecEvents(t *testing.T) {
	libssl := traceTLSHostLibssl(t)
	root := t.TempDir()
	copyTraceTLSFile(t, libssl, filepath.Join(root, "host", "libssl.so.3"))
	release := filepath.Join(root, "osrelease")
	if err := os.WriteFile(release, []byte("6.17.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previousRoot, previousLibraries, previousRelease, previousProblem := traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSExecProblem
	traceProcRoot, traceTLSHostLibraries, traceKernelRelease = t.TempDir(), []string{filepath.Join(root, "host", "libssl.so*")}, release
	traceTLSExecProblem = func() string { return "network namespace net:[4026532000]" }
	t.Cleanup(func() {
		traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSExecProblem = previousRoot, previousLibraries, previousRelease, previousProblem
	})
	finder, notices, code, err := resolveTraceTLSTargets(traceTLSAuto)
	if err != nil || code != 0 || len(finder.targets) != 1 || !slices.Equal(notices, []string{T("cli.trace.tls_exec_events", "network namespace net:[4026532000]")}) {
		t.Fatalf("resolve = %q, %d, %v", notices, code, err)
	}
}

// 탐색이 아무 파일도 찾지 못하면 trace를 시작하지 않는다. 시작하면 HTTPS가 보이지 않는 이유를 알 수 없다.
func TestResolveTraceTLSTargetsFailsWithoutAnyLibrary(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("--tls supports amd64 and arm64")
	}
	previousRoot, previousLibraries := traceProcRoot, traceTLSHostLibraries
	traceProcRoot, traceTLSHostLibraries = t.TempDir(), nil
	t.Cleanup(func() { traceProcRoot, traceTLSHostLibraries = previousRoot, previousLibraries })
	_, _, code, err := resolveTraceTLSTargets(traceTLSAuto)
	if code != 3 || err == nil {
		t.Fatalf("resolve = %d, %v", code, err)
	}
}

// 6.11부터 uretprobe는 syscall로 돌아오고, 이 syscall을 통과시키는 seccomp 수정은 6.12.14, 6.13.3, 6.14에 들어갔다.
func TestTraceTLSSeccompRisk(t *testing.T) {
	for release, want := range map[string]bool{
		"6.10.14":           false,
		"6.11.0-29-generic": true,
		"6.12.13":           true,
		"6.12.14+deb13":     false,
		"6.13.2-arch1-1":    true,
		"6.13.3":            false,
		"6.14.0":            false,
		"6.17.0-14-generic": false,
		"5.15.0-153":        false,
		"":                  false,
	} {
		if got := traceTLSSeccompRisk(release); got != want {
			t.Errorf("%q = %t, want %t", release, got, want)
		}
	}
}

// map_files를 따라갈 권한이 없으면 process마다 조용히 건너뛰지 않고 끝에서 한 번 알린다.
func TestResolveTraceTLSTargetsNoticesAMissingCapability(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("--tls supports amd64 and arm64")
	}
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	for _, pid := range []string{"100", "101"} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(proc, pid, "maps"), []byte("7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release := filepath.Join(root, "osrelease")
	if err := os.WriteFile(release, []byte("6.17.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	previousRoot, previousLibraries, previousRelease, previousStat := traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSStat
	traceProcRoot, traceTLSHostLibraries, traceKernelRelease = proc, nil, release
	traceTLSStat = func(path string) (fs.FileInfo, error) {
		if strings.Contains(path, "map_files") {
			return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
		}
		return os.Stat(path)
	}
	t.Cleanup(func() {
		traceProcRoot, traceTLSHostLibraries, traceKernelRelease, traceTLSStat = previousRoot, previousLibraries, previousRelease, previousStat
	})
	_, notices, code, err := resolveTraceTLSTargets(traceTLSAuto)
	if code != 3 || err == nil || !slices.Equal(notices, []string{T("cli.trace.tls_permission")}) {
		t.Fatalf("resolve = %q, %d, %v", notices, code, err)
	}
}

// Enter를 받아야 trace를 시작한다. 입력이 끝나면 시작하지 않는다.
func TestTraceTLSConfirmWaitsForEnter(t *testing.T) {
	var out strings.Builder
	if !traceTLSConfirm(strings.NewReader("\n"), &out) || out.String() != T("cli.trace.tls_confirm") {
		t.Fatalf("Enter = %q", out.String())
	}
	if traceTLSConfirm(strings.NewReader(""), io.Discard) {
		t.Fatal("the end of input started the trace")
	}
}

// 위험 kernel에서만 묻고, stderr가 터미널이 아니면 보이지 않는 입력을 기다리지 않는다. 입력이 끝나면 취소로 알린다.
func TestTraceTLSConfirmStartAsksOnlyWhereTheWarningShows(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the seccomp risk applies to amd64 only")
	}
	release := filepath.Join(t.TempDir(), "osrelease")
	previous := traceKernelRelease
	traceKernelRelease = release
	t.Cleanup(func() { traceKernelRelease = previous })
	write := func(text string) {
		if err := os.WriteFile(release, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("6.17.0\n")
	if !traceTLSConfirmStart(strings.NewReader(""), io.Discard, true) {
		t.Fatal("a safe kernel asked for Enter")
	}
	write("6.11.0-29-generic\n")
	var out strings.Builder
	if !traceTLSConfirmStart(strings.NewReader(""), &out, false) || out.Len() != 0 {
		t.Fatalf("redirected stderr asked for Enter: %q", out.String())
	}
	if !traceTLSConfirmStart(strings.NewReader("\n"), io.Discard, true) {
		t.Fatal("Enter did not start the trace")
	}
	out.Reset()
	if traceTLSConfirmStart(strings.NewReader(""), &out, true) || !strings.HasSuffix(out.String(), T("cli.trace.cancelled")+"\n") {
		t.Fatalf("end of input = %q", out.String())
	}
}
