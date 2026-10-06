package edc

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestTraceTLSMappedLibrariesReadsLibsslPaths(t *testing.T) {
	maps := []byte(`7f00-7f01 r--p 00000000 08:03 123  /usr/lib/x86_64-linux-gnu/libssl.so.3
7f01-7f02 r-xp 00001000 08:03 123  /usr/lib/x86_64-linux-gnu/libssl.so.3
7f02-7f03 r--p 00000000 08:03 124  /usr/lib/x86_64-linux-gnu/libcrypto.so.3
7f03-7f04 r--p 00000000 00:23 125  /usr/lib/libssl.so.1.1 (deleted)
7f04-7f05 r--p 00000000 00:24 126  /opt/my app/lib/libssl.so.3
7f05-7f06 rw-p 00000000 00:00 0
7f06-7f07 r--p 00000000 00:00 0  [vdso]
`)
	want := []string{"/usr/lib/x86_64-linux-gnu/libssl.so.3", "/opt/my app/lib/libssl.so.3"}
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
			if symbols, err := traceTLSFileSymbols(path, false); err == nil && traceTLSReadsPlaintext(symbols) {
				return path
			}
		}
	}
	t.Skip("no libssl with SSL_read on this host")
	return ""
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

// 같은 파일은 host 경로와 여러 process에서 보여도 한 번만 붙인다. container의 libssl은 /proc/<pid>/root를 거쳐 연다.
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
	mapping := "7f00-7f01 r-xp 00000000 08:03 1  /usr/lib/libssl.so.3\n"
	// 100은 container라 host와 다른 libssl이다. 101은 같은 container의 다른 process라 같은 파일이다.
	write("100", "maps", mapping)
	container := filepath.Join(proc, "100", "root", "usr", "lib", "libssl.so.3")
	copyTraceTLSFile(t, libssl, container)
	write("101", "maps", mapping)
	if err := os.MkdirAll(filepath.Join(proc, "101", "root", "usr", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(container, filepath.Join(proc, "101", "root", "usr", "lib", "libssl.so.3")); err != nil {
		t.Fatal(err)
	}
	// 102는 host libssl을 쓴다. 103은 libssl이 없고 실행 파일에도 SSL 함수가 없다. 104는 libssl 이름의 다른 파일이다.
	write("102", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /host/libssl.so.3\n")
	if err := os.MkdirAll(filepath.Join(proc, "102", "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "host"), filepath.Join(proc, "102", "root", "host")); err != nil {
		t.Fatal(err)
	}
	write("103", "maps", "7f00-7f01 r-xp 00000000 08:03 1  /usr/bin/sleep\n")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copyTraceTLSFile(t, executable, filepath.Join(proc, "103", "exe"))
	write("104", "maps", mapping)
	write("104", "root/usr/lib/libssl.so.3", "not an ELF file")
	// 105는 maps를 읽기 전에 끝난 process다. self는 숫자가 아니라 건너뛴다.
	if err := os.MkdirAll(filepath.Join(proc, "105"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("self", "maps", mapping)

	previousRoot, previousLibraries := traceProcRoot, traceTLSHostLibraries
	traceProcRoot, traceTLSHostLibraries = proc, []string{filepath.Join(root, "host", "libssl.so*")}
	t.Cleanup(func() { traceProcRoot, traceTLSHostLibraries = previousRoot, previousLibraries })

	targets, notices, code, err := resolveTraceTLSTargets(traceTLSAuto)
	if err != nil || code != 0 {
		t.Fatalf("resolve = %v, %d", err, code)
	}
	var paths []string
	for _, target := range targets {
		paths = append(paths, target.path)
		if !slices.Contains(target.symbols, "SSL_read") || !slices.Contains(target.symbols, "SSL_write") {
			t.Fatalf("%s symbols = %q", target.path, target.symbols)
		}
	}
	if want := []string{host, container}; !slices.Equal(paths, want) {
		t.Fatalf("targets = %q, want %q", paths, want)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], filepath.Join(proc, "104")) {
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
	for _, path := range []string{notELF, executable, filepath.Join(t.TempDir(), "missing")} {
		if _, _, code, err := resolveTraceTLSTargets(traceTLSMode(path)); code != 2 || err == nil {
			t.Fatalf("--tls=%s = %d, %v", path, code, err)
		}
	}
	libssl := traceTLSHostLibssl(t)
	targets, _, code, err := resolveTraceTLSTargets(traceTLSMode(libssl))
	if err != nil || code != 0 || len(targets) != 1 || targets[0].path != libssl {
		t.Fatalf("--tls=%s = %+v, %d, %v", libssl, targets, code, err)
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
