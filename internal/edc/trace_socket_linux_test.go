//go:build linux

package edc

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSocketKernelDevUsesTheKernelEncoding(t *testing.T) {
	// /dev/sda3은 8:3이다. stat은 0x803을, kernel의 s_dev는 0x800003을 쓴다.
	if got := socketKernelDev(unix.Mkdev(8, 3)); got != 0x800003 {
		t.Fatalf("socketKernelDev(8:3) = %#x", got)
	}
	// tmpfs처럼 major가 0이면 두 형식이 같다.
	if got := socketKernelDev(unix.Mkdev(0, 26)); got != 26 {
		t.Fatalf("socketKernelDev(0:26) = %#x", got)
	}
}

func TestUnixSocketKindReadsProcNetUnix(t *testing.T) {
	data := []byte(`Num       RefCount Protocol Flags    Type St Inode Path
ffff8a9801676780: 00000002 00000000 00010000 0001 01 22959149 /run/docker.sock
ffff8a9801676781: 00000002 00000000 00000000 0002 01 22959150 /run/systemd/journal/socket
ffff8a9801676782: 00000002 00000000 00000000 0005 01 22959151 /run/seq.sock
ffff8a9801676783: 00000003 00000000 00000000 0001 03 22959152
`)
	for _, test := range []struct {
		paths []string
		kind  string
	}{
		{[]string{"/run/docker.sock"}, "stream"},
		{[]string{"/var/run/docker.sock", "/run/docker.sock"}, "stream"},
		{[]string{"/run/systemd/journal/socket"}, "dgram"},
		{[]string{"/run/seq.sock"}, "seqpacket"},
		{[]string{"/run/missing.sock"}, ""},
	} {
		if got := unixSocketKind(data, test.paths); got != test.kind {
			t.Fatalf("unixSocketKind(%v) = %q, want %q", test.paths, got, test.kind)
		}
	}
}

func TestResolveSocketTargetChecksTheFile(t *testing.T) {
	directory := t.TempDir()
	if _, err := resolveSocketTarget(""); err == nil || !strings.Contains(err.Error(), "edc trace socket") {
		t.Fatalf("empty path: %v", err)
	}
	if _, err := resolveSocketTarget(filepath.Join(directory, "missing.sock")); err == nil {
		t.Fatal("a missing file must fail")
	}
	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSocketTarget(file); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: %v", err)
	}
	path := filepath.Join(directory, "stream.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	target, err := resolveSocketTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	if target.path != path || target.key.Ino != stat.Ino || target.key.Dev != socketKernelDev(stat.Dev) {
		t.Fatalf("target = %+v, stat ino %d dev %#x", target, stat.Ino, stat.Dev)
	}
	datagram := filepath.Join(directory, "dgram.sock")
	connection, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: datagram, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := resolveSocketTarget(datagram); err == nil || !strings.Contains(err.Error(), "dgram") {
		t.Fatalf("datagram socket: %v", err)
	}
}

func TestTraceSocketTakesOnePathWithOptionsOnEitherSide(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	for _, test := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"socket"}, "usage: edc trace socket"},
		{[]string{"socket", missing, "extra"}, "usage: edc trace socket"},
		// 경로를 확인하는 단계까지 가면 option을 모두 읽은 것이다.
		{[]string{"socket", "--duration", "1s", missing}, "cannot read the socket file"},
		{[]string{"socket", missing, "--duration", "1s", "--payload=all"}, "cannot read the socket file"},
		{[]string{"socket", missing, "--port", "80"}, "--port is not available for trace socket"},
		{[]string{"socket", missing, "--payload", "--show-secrets"}, "--show-secrets is not available for trace socket"},
		{[]string{"socket", missing, "--side", "server"}, "--side server is not available for trace socket"},
		{[]string{"tcp", missing}, "trace tcp takes no positional argument"},
	} {
		var code int
		stderr := captureTraceStderr(t, func() { code = runTrace(test.args) })
		if code != 2 || !strings.Contains(stderr, test.stderr) {
			t.Fatalf("trace %q exit = %d, stderr %q, want 2 and %q", test.args, code, stderr, test.stderr)
		}
	}
}

func captureTraceStderr(t *testing.T, run func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = previous }()
	run()
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
