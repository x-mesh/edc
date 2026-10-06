package edc

import (
	"bytes"
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// traceTLSTarget은 --tls가 uprobe를 붙일 파일 하나다. container 안의 파일은 /proc/<pid>/root를 거친 경로라서 host의
// 같은 이름 파일과 구별된다. symbols는 그 파일이 정의한 traceTLSFunctions다.
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

// resolveTraceTLSTargets는 --tls 값으로 붙일 파일을 고른다. trace 화면을 열기 전에 불러서 안내를 stderr에 쓴다.
// notices는 libssl처럼 보였지만 붙일 수 없는 파일이다. 실패하면 exit code를 함께 돌려준다.
func resolveTraceTLSTargets(mode traceTLSMode) (targets []traceTLSTarget, notices []string, code int, err error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return nil, nil, 3, errors.New(T("cli.trace.tls_arch", runtime.GOARCH))
	}
	if mode != traceTLSAuto {
		path := string(mode)
		symbols, err := traceTLSFileSymbols(path, true)
		if err != nil {
			return nil, nil, 2, errors.New(T("cli.trace.tls_skipped", path, err))
		}
		if !traceTLSReadsPlaintext(symbols) {
			return nil, nil, 2, errors.New(T("cli.trace.tls_symbols", path))
		}
		return []traceTLSTarget{{path: path, symbols: symbols}}, nil, 0, nil
	}
	finder := traceTLSFinder{seen: map[[2]uint64]bool{}}
	for _, pattern := range traceTLSHostLibraries {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			finder.add(path, true)
		}
	}
	finder.scanProcesses()
	if len(finder.targets) == 0 {
		return nil, finder.notices, 3, errors.New(T("cli.trace.tls_none"))
	}
	return finder.targets, finder.notices, 0, nil
}

// traceTLSFinder는 같은 파일을 (device, inode)로 한 번만 고른다. 여러 process와 host 경로가 같은 libssl을 가리킨다.
type traceTLSFinder struct {
	seen    map[[2]uint64]bool
	targets []traceTLSTarget
	notices []string
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
		libraries := traceTLSMappedLibraries(maps)
		for _, library := range libraries {
			finder.add(filepath.Join(process, "root", library), true)
		}
		if len(libraries) == 0 {
			finder.add(filepath.Join(process, "exe"), false)
		}
	}
}

// traceTLSMappedLibraries는 /proc/<pid>/maps에서 libssl로 시작하는 파일 경로를 고른다. 지워진 파일은 열 수 없어 뺀다.
func traceTLSMappedLibraries(maps []byte) []string {
	var libraries []string
	for _, line := range bytes.Split(maps, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 6 {
			continue
		}
		path := strings.Join(fields[5:], " ")
		if !strings.HasPrefix(path, "/") || strings.HasSuffix(path, " (deleted)") || !strings.HasPrefix(filepath.Base(path), "libssl.so") {
			continue
		}
		if !slices.Contains(libraries, path) {
			libraries = append(libraries, path)
		}
	}
	return libraries
}

// add는 처음 본 파일이 SSL 함수를 정의하면 대상에 넣는다. library는 libssl로 찾은 파일이라 실패를 알린다. 실행 파일은
// 대부분 OpenSSL이 없으므로 알리지 않는다. 실행 파일은 .symtab을 읽지 않는다. 큰 binary마다 읽으면 시작이 느려진다.
func (finder *traceTLSFinder) add(path string, library bool) {
	info, err := os.Stat(path)
	if err != nil {
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
		finder.notices = append(finder.notices, T("cli.trace.tls_skipped", path, err))
	case err != nil:
	case traceTLSReadsPlaintext(symbols):
		finder.targets = append(finder.targets, traceTLSTarget{path: path, symbols: symbols})
	case library:
		finder.notices = append(finder.notices, T("cli.trace.tls_symbols", path))
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
