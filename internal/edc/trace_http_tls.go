package edc

import (
	"bufio"
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// traceTLSTarget은 --tls가 uprobe를 붙일 파일 하나다. process가 적재한 파일은 /proc/<pid>/map_files를 거친 경로라서
// host의 같은 이름 파일과 구별된다. symbols는 그 파일이 정의한 traceTLSFunctions다.
type traceTLSTarget struct {
	path    string
	symbols []string
}

// traceTLSFunctions는 평문을 읽는 OpenSSL 함수와, SSL 객체가 끝날 때 짝짓기 상태를 지우는 SSL_free다. _ex는 OpenSSL
// 1.1.1에 생겨서 없는 파일도 있다.
var traceTLSFunctions = []string{"SSL_read", "SSL_write", "SSL_read_ex", "SSL_write_ex", "SSL_free"}

// traceTLSHostLibraries는 실행 중인 process가 적재하지 않아도 붙이는 host의 libssl이다. uprobe는 파일 단위라서, trace를
// 시작한 뒤에 뜬 curl이나 python도 이 파일을 쓰면 보인다. test가 바꾼다.
var traceTLSHostLibraries = []string{"/lib/*/libssl.so*", "/usr/lib/*/libssl.so*", "/lib64/libssl.so*", "/usr/lib64/libssl.so*", "/usr/lib/libssl.so*", "/usr/local/lib/libssl.so*", "/usr/local/lib64/libssl.so*"}

// traceTLSMachines는 BPF가 인자를 읽는 register 배치와 맞는 ELF다. multilib host의 i386 libssl은 인자를 stack으로 받는다.
var traceTLSMachines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// traceKernelRelease는 kernel 버전을 읽는 파일이다. test가 바꾼다.
var traceKernelRelease = "/proc/sys/kernel/osrelease"

// traceTLSStat은 탐색이 파일을 확인하는 함수다. root는 파일 권한을 무시하므로, test는 map_files의 EPERM을 이 함수로 만든다.
var traceTLSStat = os.Stat

// resolveTraceTLSTargets는 --tls 값으로 붙일 파일을 고른다. trace 화면을 열기 전에 불러서 안내를 stderr에 쓴다.
// notices는 libssl처럼 보였지만 붙일 수 없는 파일이다. 실패하면 exit code를 함께 돌려준다.
func resolveTraceTLSTargets(mode traceTLSMode) (targets []traceTLSTarget, notices []string, code int, err error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return nil, nil, 3, errors.New(T("cli.trace.tls_arch", runtime.GOARCH))
	}
	if release, risky := traceTLSSeccompKernel(); risky {
		notices = append(notices, T("cli.trace.tls_seccomp", release))
	}
	if mode != traceTLSAuto {
		path := string(mode)
		symbols, err := traceTLSFileSymbols(path, true)
		if err != nil {
			return nil, notices, 2, errors.New(T("cli.trace.tls_skipped", path, err))
		}
		if !traceTLSReadsPlaintext(symbols) {
			return nil, notices, 2, errors.New(T("cli.trace.tls_symbols", path))
		}
		return []traceTLSTarget{{path: path, symbols: symbols}}, notices, 0, nil
	}
	finder := traceTLSFinder{seen: map[[2]uint64]bool{}, notices: notices}
	for _, pattern := range traceTLSHostLibraries {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			finder.add(path, path, true)
		}
	}
	finder.scanProcesses()
	if finder.denied {
		finder.notices = append(finder.notices, T("cli.trace.tls_permission"))
	}
	if len(finder.targets) == 0 {
		return nil, finder.notices, 3, errors.New(T("cli.trace.tls_none"))
	}
	return finder.targets, finder.notices, 0, nil
}

// traceTLSSeccompKernel은 이 host가 traceTLSSeccompRisk에 드는 amd64 kernel이면 그 release를 돌려준다.
func traceTLSSeccompKernel() (string, bool) {
	release, err := os.ReadFile(traceKernelRelease)
	text := string(bytes.TrimSpace(release))
	return text, err == nil && runtime.GOARCH == "amd64" && traceTLSSeccompRisk(text)
}

// traceTLSConfirm은 seccomp 경고 뒤에 Enter를 기다린다. 전체 화면이 경고를 곧바로 덮으므로, probe를 붙이기 전에
// 읽고 Ctrl-C로 멈출 수 있게 한다. 입력이 끝나면 trace를 시작하지 않는다.
func traceTLSConfirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, T("cli.trace.tls_confirm"))
	_, err := bufio.NewReader(in).ReadString('\n')
	return err == nil
}

// traceTLSSeccompRisk는 uretprobe가 seccomp filter 아래의 process를 죽일 수 있는 amd64 kernel이다. 6.11부터 uretprobe는
// 돌아올 때 syscall을 부르고, Docker 기본 profile처럼 모르는 syscall을 막는 filter가 그 process를 끝낸다. 6.14와
// 6.12.14, 6.13.3이 seccomp가 이 syscall을 통과시키게 고쳤다. distro kernel은 고친 패치를 따로 넣었을 수 있어 막지 않고 알린다.
func traceTLSSeccompRisk(release string) bool {
	var major, minor, patch int
	fmt.Sscanf(release, "%d.%d.%d", &major, &minor, &patch)
	return major == 6 && (minor == 11 || minor == 12 && patch < 14 || minor == 13 && patch < 3)
}

// traceTLSFinder는 같은 파일을 (device, inode)로 한 번만 고른다. 여러 process와 host 경로가 같은 libssl을 가리킨다.
// denied는 다른 process가 적재한 libssl을 권한 때문에 열지 못한 것이다.
type traceTLSFinder struct {
	seen    map[[2]uint64]bool
	targets []traceTLSTarget
	notices []string
	denied  bool
}

// scanProcesses는 process마다 적재한 libssl을 찾는다. libssl이 없는 process는 실행 파일이 SSL 함수를 내보낼 때만 고른다.
// node는 OpenSSL을 실행 파일 안에 넣고 함수를 내보낸다. 사라진 process와 읽을 수 없는 process는 건너뛴다.
func (finder *traceTLSFinder) scanProcesses() {
	entries, err := os.ReadDir(traceProcRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		process := filepath.Join(traceProcRoot, entry.Name())
		maps, err := os.ReadFile(filepath.Join(process, "maps"))
		if err != nil {
			continue
		}
		mappings := traceTLSMappedLibraries(maps)
		// maps의 경로를 /proc/<pid>/root에 붙여 열면 안 된다. chroot한 process는 그 자리에 FIFO나 host를 가리키는
		// symlink를 둘 수 있다. map_files는 실제로 적재한 inode로 이어지고, 업데이트로 지워진 libssl도 연다.
		for _, mapping := range mappings {
			finder.add(filepath.Join(process, "map_files", mapping.addresses), filepath.Join(process, "root", mapping.path), true)
		}
		if len(mappings) == 0 {
			exe := filepath.Join(process, "exe")
			finder.add(exe, exe, false)
		}
	}
}

// traceTLSMapping은 maps에서 libssl을 적재한 줄이다. addresses는 /proc/<pid>/map_files 안의 이름이고, path는 안내에 쓴다.
type traceTLSMapping struct {
	addresses string
	path      string
}

// traceTLSMappedLibraries는 /proc/<pid>/maps에서 libssl로 시작하는 파일을 경로마다 한 번 고른다. 지워진 파일은 path에
// " (deleted)"가 붙는다. maps는 주소를 0으로 채우지만 map_files 이름은 채우지 않는다.
func traceTLSMappedLibraries(maps []byte) []traceTLSMapping {
	var mappings []traceTLSMapping
	for _, line := range bytes.Split(maps, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 6 {
			continue
		}
		path := strings.Join(fields[5:], " ")
		if !strings.HasPrefix(path, "/") || !strings.HasPrefix(filepath.Base(strings.TrimSuffix(path, " (deleted)")), "libssl.so") {
			continue
		}
		start, end, _ := strings.Cut(fields[0], "-")
		low, lowErr := strconv.ParseUint(start, 16, 64)
		high, highErr := strconv.ParseUint(end, 16, 64)
		if lowErr != nil || highErr != nil || slices.ContainsFunc(mappings, func(mapping traceTLSMapping) bool { return mapping.path == path }) {
			continue
		}
		mappings = append(mappings, traceTLSMapping{addresses: fmt.Sprintf("%x-%x", low, high), path: path})
	}
	return mappings
}

// add는 처음 본 파일이 SSL 함수를 정의하면 대상에 넣는다. name은 안내에 쓰는 이름이고, 다른 process가 정한 경로라서
// 제어 문자를 escape한다. library는 libssl로 찾은 파일이라 실패를 알린다. 실행 파일은 대부분 OpenSSL이 없으므로
// 알리지 않는다. 실행 파일은 .symtab을 읽지 않는다. 큰 binary마다 읽으면 시작이 느려진다.
func (finder *traceTLSFinder) add(path, name string, library bool) {
	info, err := traceTLSStat(path)
	// map_files를 따라가려면 CAP_SYS_ADMIN이나 CAP_CHECKPOINT_RESTORE가 필요하다. BPF 권한만 더한 container에서는
	// 다른 process의 libssl을 모두 놓치므로, 끝에서 한 번 알린다.
	if errors.Is(err, fs.ErrPermission) && library {
		finder.denied = true
	}
	// FIFO나 device는 여는 것만으로 멈추거나 부작용이 있다. 적재한 파일은 늘 일반 파일이다.
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	id := [2]uint64{uint64(stat.Dev), uint64(stat.Ino)}
	if finder.seen[id] {
		return
	}
	finder.seen[id] = true
	symbols, err := traceTLSFileSymbols(path, false)
	switch {
	case err != nil && library:
		finder.notices = append(finder.notices, T("cli.trace.tls_skipped", traceEscapeText([]byte(name)), err))
	case err != nil:
	case traceTLSReadsPlaintext(symbols):
		finder.targets = append(finder.targets, traceTLSTarget{path: path, symbols: symbols})
	case library:
		finder.notices = append(finder.notices, T("cli.trace.tls_symbols", traceEscapeText([]byte(name))))
	}
}

// traceTLSFileSymbols는 path가 정의한 traceTLSFunctions다. 가져다 쓰기만 하는 심볼은 값이 0이라 붙일 곳이 없다.
// withSymtab이면 .symtab도 읽는다. --tls=<경로>로 준 정적 binary는 .dynsym이 없을 수 있다.
func traceTLSFileSymbols(path string, withSymtab bool) ([]string, error) {
	file, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if machine, ok := traceTLSMachines[runtime.GOARCH]; !ok || file.Class != elf.ELFCLASS64 || file.Machine != machine {
		return nil, fmt.Errorf("%v %v", file.Class, file.Machine)
	}
	symbols, err := file.DynamicSymbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
		return nil, err
	}
	if withSymtab {
		table, err := file.Symbols()
		if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
			return nil, err
		}
		symbols = append(symbols, table...)
	}
	var found []string
	for _, symbol := range symbols {
		if elf.ST_TYPE(symbol.Info) != elf.STT_FUNC || symbol.Section == elf.SHN_UNDEF || symbol.Value == 0 {
			continue
		}
		if slices.Contains(traceTLSFunctions, symbol.Name) && !slices.Contains(found, symbol.Name) {
			found = append(found, symbol.Name)
		}
	}
	return found, nil
}

// traceTLSReadsPlaintext는 평문을 읽을 함수가 하나라도 있는 파일이다. SSL_free만 있으면 붙일 이유가 없다.
func traceTLSReadsPlaintext(symbols []string) bool {
	return slices.ContainsFunc(symbols, func(name string) bool { return name != "SSL_free" })
}
