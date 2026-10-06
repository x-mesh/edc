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
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// traceTLSTarget은 --tls가 uprobe를 붙일 파일 하나다. process가 적재한 파일은 /proc/<pid>/map_files를 거친 경로라서
// host의 같은 이름 파일과 구별된다. symbols는 그 파일이 정의한 traceTLSFunctions이고, offsets는 각 함수의 파일 안
// 위치다. 위치를 주면 uprobe를 붙일 때 ELF의 심볼 표를 다시 읽지 않는다. node처럼 큰 실행 파일은 그 표를 읽는 데 100ms
// 넘게 걸려, trace 중에 새로 뜬 process의 첫 요청보다 늦게 붙는다.
type traceTLSTarget struct {
	path    string
	symbols []string
	offsets map[string]uint64
	// id는 탐색이 고른 파일의 (device, inode)다. --tls=<경로>로 준 파일은 0이다.
	id [2]uint64
}

// traceTLSFunctions는 평문을 읽는 OpenSSL과 GnuTLS 함수, 그리고 연결 상태가 끝날 때 짝짓기 상태를 지우는
// traceTLSFreeFunctions다. OpenSSL의 _ex는 1.1.1에 생겨서 없는 파일도 있다. GnuTLS 3.6.3부터 gnutls_record_send는
// gnutls_record_send2로 넘어가는 stub이지만, BPF는 중첩된 호출의 평문을 한 번만 내므로 둘 다 붙인다.
var traceTLSFunctions = []string{"SSL_read", "SSL_write", "SSL_read_ex", "SSL_write_ex", "SSL_free",
	"gnutls_record_send", "gnutls_record_send2", "gnutls_record_recv", "gnutls_record_recv_seq", "gnutls_deinit",
	"SSL_ImportFD", "SSL_OptionSet", "SSL_OptionSetDefault", "PR_Accept",
	"PR_Read", "PR_Recv", "PR_Write", "PR_Send", "PR_Close"}

var traceTLSNSSControls = []string{"SSL_ImportFD", "SSL_OptionSet", "SSL_OptionSetDefault", "PR_Accept"}

var traceTLSNSSFunctions = []string{"SSL_ImportFD", "SSL_OptionSet", "SSL_OptionSetDefault", "PR_Accept",
	"PR_Read", "PR_Recv", "PR_Write", "PR_Send", "PR_Close"}

// traceTLSFreeFunctions는 평문을 읽지 않는 traceTLSFunctions다.
var traceTLSFreeFunctions = []string{"SSL_free", "gnutls_deinit", "PR_Close"}

// traceTLSLibraryNames는 maps와 host 디렉터리에서 찾는 TLS library 파일 이름의 앞부분이다.
var traceTLSLibraryNames = []string{"libssl.so", "libgnutls.so", "libssl3.so", "libnspr4.so"}

// traceTLSHostLibraries는 실행 중인 process가 적재하지 않아도 붙이는 host의 TLS library다. uprobe는 파일 단위라서, trace를
// 시작한 뒤에 뜬 curl이나 wget도 이 파일을 쓰면 보인다. test가 바꾼다.
var traceTLSHostLibraries = []string{"/lib/*/libssl.so*", "/usr/lib/*/libssl.so*", "/lib64/libssl.so*", "/usr/lib64/libssl.so*", "/usr/lib/libssl.so*", "/usr/local/lib/libssl.so*", "/usr/local/lib64/libssl.so*",
	"/lib/*/libgnutls.so*", "/usr/lib/*/libgnutls.so*", "/lib64/libgnutls.so*", "/usr/lib64/libgnutls.so*", "/usr/lib/libgnutls.so*", "/usr/local/lib/libgnutls.so*", "/usr/local/lib64/libgnutls.so*",
	"/lib/*/libssl3.so", "/usr/lib/*/libssl3.so", "/lib64/libssl3.so", "/usr/lib64/libssl3.so", "/usr/lib/libssl3.so", "/usr/local/lib/libssl3.so", "/usr/local/lib64/libssl3.so",
	"/lib/*/libnspr4.so", "/usr/lib/*/libnspr4.so", "/lib64/libnspr4.so", "/usr/lib64/libnspr4.so", "/usr/lib/libnspr4.so", "/usr/local/lib/libnspr4.so", "/usr/local/lib64/libnspr4.so"}

// traceTLSMachines는 BPF가 인자를 읽는 register 배치와 맞는 ELF다. multilib host의 i386 libssl은 인자를 stack으로 받는다.
var traceTLSMachines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// traceKernelRelease는 kernel 버전을 읽는 파일이다. test가 바꾼다.
var traceKernelRelease = "/proc/sys/kernel/osrelease"

// traceTLSStat은 탐색이 파일을 확인하는 함수다. root는 파일 권한을 무시하므로, test는 map_files의 EPERM을 이 함수로 만든다.
var traceTLSStat = os.Stat

// resolveTraceTLSTargets는 --tls 값으로 붙일 파일을 고른다. trace 화면을 열기 전에 불러서 안내를 stderr에 쓴다.
// 돌려주는 finder의 targets가 붙일 파일이고, 자동 탐색이면 trace 중에도 이 finder로 새 파일을 찾는다. notices는 TLS
// library처럼 보였지만 붙일 수 없는 파일이다. 실패하면 exit code를 함께 돌려준다.
func resolveTraceTLSTargets(mode traceTLSMode) (finder *traceTLSFinder, notices []string, code int, err error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return nil, nil, 3, errors.New(T("cli.trace.tls_arch", runtime.GOARCH))
	}
	if release, risky := traceTLSSeccompKernel(); risky {
		notices = append(notices, T("cli.trace.tls_seccomp", release))
	}
	if mode != traceTLSAuto {
		path := string(mode)
		target, err := traceTLSReadFile(path, true)
		// 기존 상대 경로가 PATH의 같은 이름 실행 파일로 바뀌지 않게, 그 이름의 파일이 없거나 디렉터리일 때만 찾는다.
		// 디렉터리를 연 오류는 elf.FormatError가 값으로만 담아 errors.Is로 가릴 수 없으므로 stat으로 판단한다.
		if err != nil && !strings.ContainsRune(path, '/') {
			if info, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) || (statErr == nil && info.IsDir()) {
				var resolved string
				resolved, err = exec.LookPath(path)
				if err == nil {
					path = resolved
					target, err = traceTLSReadFile(path, true)
				}
			}
		}
		if err != nil {
			return nil, notices, 2, errors.New(T("cli.trace.tls_skipped", path, err))
		}
		peer, needed, err := traceTLSNSSPeer(target)
		if err != nil {
			return nil, notices, 2, err
		}
		targets := []traceTLSTarget{target}
		if needed {
			if slices.Contains(target.symbols, "SSL_ImportFD") {
				targets = append(targets, peer)
			} else {
				targets = []traceTLSTarget{peer, target}
			}
		}
		if !slices.ContainsFunc(targets, func(target traceTLSTarget) bool { return traceTLSReadsPlaintext(target.symbols) }) {
			return nil, notices, 2, errors.New(T("cli.trace.tls_symbols", path))
		}
		return &traceTLSFinder{targets: targets}, notices, 0, nil
	}
	finder = &traceTLSFinder{seen: map[[2]uint64]bool{}, notices: notices, rescan: true}
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
	if !slices.ContainsFunc(finder.targets, func(target traceTLSTarget) bool { return traceTLSReadsPlaintext(target.symbols) }) {
		return nil, finder.notices, 3, errors.New(T("cli.trace.tls_none"))
	}
	if problem := traceTLSExecProblem(); problem != "" {
		finder.notices = append(finder.notices, T("cli.trace.tls_exec_events", problem))
	}
	return finder, finder.notices, 0, nil
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

// traceTLSConfirmStart는 seccomp 위험 kernel에서 전체 화면을 열기 전에 Enter를 받는다. 경고와 확인 문구는 stderr에 쓰므로,
// stderr를 리다이렉트했으면 보이지 않는 입력을 기다리지 않도록 묻지 않는다. 입력이 끝나면 취소로 알리고 false를 돌려준다.
func traceTLSConfirmStart(in io.Reader, out io.Writer, outTerminal bool) bool {
	if _, risky := traceTLSSeccompKernel(); !risky || !outTerminal || traceTLSConfirm(in, out) {
		return true
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, T("cli.trace.cancelled"))
	return false
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
// seen은 붙인 파일뿐 아니라 확인한 실행 파일도 담아서, trace 중 다시 탐색할 때 같은 ELF를 다시 읽지 않는다. denied는
// 다른 process가 적재한 library를 권한 때문에 열지 못한 것이다. rescan은 자동 탐색이라 trace 중에도 찾는다는 뜻이다.
type traceTLSFinder struct {
	seen    map[[2]uint64]bool
	targets []traceTLSTarget
	notices []string
	denied  bool
	rescan  bool
}

// retry는 붙이기 전에 경로가 사라진 파일을 다음 탐색에서 다시 고르게 한다. 고른 process가 그 사이에 끝나도, 같은 파일을
// 적재한 다른 process가 살아 있는 경로를 준다.
func (finder *traceTLSFinder) retry(target traceTLSTarget) {
	delete(finder.seen, target.id)
}

// printTraceTLSExecProblem은 trace 중에 exec 알림을 받지 못했으면 trace가 끝난 뒤 알린다. 화면을 연 동안에는 stderr에 쓸
// 수 없다.
func printTraceTLSExecProblem(summary captureSummary) {
	if summary.TLSExecProblem != "" {
		fmt.Fprintln(os.Stderr, T("cli.trace.tls_exec_events", summary.TLSExecProblem))
	}
}

// scanProcesses는 process마다 적재한 TLS library를 찾는다.
func (finder *traceTLSFinder) scanProcesses() {
	entries, err := os.ReadDir(traceProcRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		finder.scanProcess(entry.Name())
	}
}

// scanProcess는 process 하나가 적재한 TLS library를 찾는다. library가 없는 process는 실행 파일이 SSL 함수를 내보낼 때만
// 고른다. node는 OpenSSL을 실행 파일 안에 넣고 함수를 내보낸다. 사라진 process와 읽을 수 없는 process는 건너뛰고 false를
// 돌려준다.
func (finder *traceTLSFinder) scanProcess(pid string) bool {
	if _, err := strconv.Atoi(pid); err != nil {
		return false
	}
	process := filepath.Join(traceProcRoot, pid)
	maps, err := os.ReadFile(filepath.Join(process, "maps"))
	if err != nil {
		return false
	}
	mappings := traceTLSMappedLibraries(maps)
	slices.SortStableFunc(mappings, func(a, b traceTLSMapping) int {
		aNSS := filepath.Base(strings.TrimSuffix(a.path, " (deleted)")) == "libssl3.so"
		bNSS := filepath.Base(strings.TrimSuffix(b.path, " (deleted)")) == "libssl3.so"
		if aNSS && !bNSS {
			return -1
		}
		if bNSS && !aNSS {
			return 1
		}
		return 0
	})
	// maps의 경로를 /proc/<pid>/root에 붙여 열면 안 된다. chroot한 process는 그 자리에 FIFO나 host를 가리키는
	// symlink를 둘 수 있다. map_files는 실제로 적재한 inode로 이어지고, 업데이트로 지워진 library도 연다.
	for _, mapping := range mappings {
		finder.add(filepath.Join(process, "map_files", mapping.addresses), filepath.Join(process, "root", mapping.path), true)
	}
	if len(mappings) == 0 {
		exe := filepath.Join(process, "exe")
		finder.add(exe, exe, false)
	}
	return true
}

// traceTLSMapping은 maps에서 TLS library를 적재한 줄이다. addresses는 /proc/<pid>/map_files 안의 이름이고, path는 안내에 쓴다.
type traceTLSMapping struct {
	addresses string
	path      string
}

// traceTLSMappedLibraries는 /proc/<pid>/maps에서 traceTLSLibraryNames로 시작하는 파일을 경로마다 한 번 고른다. 지워진 파일은 path에
// " (deleted)"가 붙는다. maps는 주소를 0으로 채우지만 map_files 이름은 채우지 않는다.
func traceTLSMappedLibraries(maps []byte) []traceTLSMapping {
	// 대부분의 process는 TLS library가 없다. trace 중에는 exec마다 여러 번 읽으므로 줄을 나누기 전에 거른다.
	if !slices.ContainsFunc(traceTLSLibraryNames, func(name string) bool { return bytes.Contains(maps, []byte(name)) }) {
		return nil
	}
	var mappings []traceTLSMapping
	for _, line := range bytes.Split(maps, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 6 {
			continue
		}
		path := strings.Join(fields[5:], " ")
		base := filepath.Base(strings.TrimSuffix(path, " (deleted)"))
		if !strings.HasPrefix(path, "/") || !slices.ContainsFunc(traceTLSLibraryNames, func(name string) bool { return strings.HasPrefix(base, name) }) {
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

// add는 처음 본 파일이 TLS 함수를 정의하면 대상에 넣는다. name은 안내에 쓰는 이름이고, 다른 process가 정한 경로라서
// 제어 문자를 escape한다. library는 TLS library로 찾은 파일이라 실패를 알린다. 실행 파일은 대부분 OpenSSL이 없으므로
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
	target, err := traceTLSReadFile(path, false)
	switch {
	case err != nil && (library || errors.Is(err, errTraceTLSBoringSSL)):
		finder.notices = append(finder.notices, T("cli.trace.tls_skipped", traceEscapeText([]byte(name)), err))
	case err != nil:
	case traceTLSReadsPlaintext(target.symbols) || slices.Contains(target.symbols, "SSL_ImportFD"):
		target.id = id
		finder.targets = append(finder.targets, target)
	case library:
		finder.notices = append(finder.notices, T("cli.trace.tls_symbols", traceEscapeText([]byte(name))))
	}
}

// traceTLSReadFile은 path가 정의한 traceTLSFunctions와 그 위치를 읽는다. 가져다 쓰기만 하는 심볼은 값이 0이라 붙일 곳이
// 없다. withSymtab이면 .symtab도 읽는다. --tls=<경로>로 준 정적 binary는 .dynsym이 없을 수 있다.
func traceTLSReadFile(path string, withSymtab bool) (traceTLSTarget, error) {
	target := traceTLSTarget{path: path, offsets: map[string]uint64{}}
	file, err := elf.Open(path)
	if err != nil {
		return target, err
	}
	defer file.Close()
	if machine, ok := traceTLSMachines[runtime.GOARCH]; !ok || file.Class != elf.ELFCLASS64 || file.Machine != machine {
		return target, fmt.Errorf("%v %v", file.Class, file.Machine)
	}
	symbols, err := file.DynamicSymbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
		return target, err
	}
	if withSymtab {
		table, err := file.Symbols()
		if err != nil && !errors.Is(err, elf.ErrNoSymbols) {
			return target, err
		}
		symbols = append(symbols, table...)
	}
	for _, symbol := range symbols {
		if elf.ST_TYPE(symbol.Info) != elf.STT_FUNC || symbol.Section == elf.SHN_UNDEF || symbol.Value == 0 {
			continue
		}
		if slices.Contains(traceTLSFunctions, symbol.Name) && !slices.Contains(target.symbols, symbol.Name) {
			target.symbols = append(target.symbols, symbol.Name)
			if offset, ok := traceTLSFileOffset(file, symbol.Value); ok {
				target.offsets[symbol.Name] = offset
			}
		}
	}
	if !traceTLSReadsPlaintext(target.symbols) {
		if err := traceTLSBoringSSL(file, &target); err != nil {
			return target, err
		}
	}
	slices.SortStableFunc(target.symbols, func(a, b string) int {
		aControl, bControl := slices.Contains(traceTLSNSSControls, a), slices.Contains(traceTLSNSSControls, b)
		if aControl && !bControl {
			return -1
		}
		if bControl && !aControl {
			return 1
		}
		return 0
	})
	return target, nil
}

func traceTLSNSSPeer(target traceTLSTarget) (traceTLSTarget, bool, error) {
	control := slices.Contains(target.symbols, "SSL_ImportFD")
	reader := slices.Contains(target.symbols, "PR_Read") || slices.Contains(target.symbols, "PR_Recv") || slices.Contains(target.symbols, "PR_Write") || slices.Contains(target.symbols, "PR_Send")
	if control == reader {
		return traceTLSTarget{}, false, nil
	}
	name := "libssl3.so"
	required := []string{"SSL_ImportFD", "SSL_OptionSet", "SSL_OptionSetDefault"}
	if control {
		name = "libnspr4.so"
		required = []string{"PR_Read", "PR_Write", "PR_Close"}
	}
	candidates := []string{filepath.Join(filepath.Dir(target.path), name)}
	for _, pattern := range traceTLSHostLibraries {
		if filepath.Base(pattern) == name {
			matches, _ := filepath.Glob(pattern)
			candidates = append(candidates, matches...)
		}
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			break
		}
		peer, err := traceTLSReadFile(path, true)
		if err == nil && !slices.ContainsFunc(required, func(symbol string) bool { return !slices.Contains(peer.symbols, symbol) }) {
			return peer, true, nil
		}
		break
	}
	return traceTLSTarget{}, true, errors.New(T("cli.trace.tls_nss_peer", target.path, name))
}

// traceTLSFileOffset은 함수의 가상 주소를 파일 안 위치로 바꾼다. uprobe는 파일 위치에 붙는다. cilium/ebpf가 심볼 이름으로
// 붙일 때와 같은 계산이다. 실행할 수 있는 PT_LOAD segment 밖이면 false다.
func traceTLSFileOffset(file *elf.File, address uint64) (uint64, bool) {
	for _, program := range file.Progs {
		if program.Type == elf.PT_LOAD && program.Flags&elf.PF_X != 0 && program.Vaddr <= address && address < program.Vaddr+program.Memsz {
			return address - program.Vaddr + program.Off, true
		}
	}
	return 0, false
}

// traceTLSReadsPlaintext는 평문을 읽을 함수가 하나라도 있는 파일이다. SSL_free나 gnutls_deinit만 있으면 붙일 이유가 없다.
func traceTLSReadsPlaintext(symbols []string) bool {
	return slices.ContainsFunc(symbols, func(name string) bool {
		return !slices.Contains(traceTLSFreeFunctions, name) && !slices.Contains(traceTLSNSSControls, name)
	})
}
