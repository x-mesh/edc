//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseLinuxTCPTableDecodesAddresses(t *testing.T) {
	table := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		"   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 111 1 0 100 0 0 10 0",
		"   1: 0100007F:1F90 0100007F:C350 08 00000000:00000000 00:00000000 00000000     0        0 222 1 0 20 4 30 10 -1",
	}, "\n")
	rows := parseLinuxTCPTable(strings.NewReader(table))
	if len(rows) != 2 || rows[0].local != "127.0.0.1:8080" || rows[0].state != "LISTEN" || rows[1].remote != "127.0.0.1:50000" || rows[1].state != "CLOSE_WAIT" || rows[1].inode != "222" {
		t.Fatalf("rows = %+v", rows)
	}
	// tcp6은 32비트 단어마다 host byte order다. ::1은 마지막 단어가 01000000이다.
	if endpoint, ok := linuxHexEndpoint("00000000000000000000000001000000:0050"); !ok || endpoint != "[::1]:80" {
		t.Fatalf("v6 = %s, %v", endpoint, ok)
	}
}

func TestReadLinuxProcDetailsClassifiesFDs(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "42")
	for _, sub := range []string{"fd", "net"} {
		if err := os.MkdirAll(filepath.Join(directory, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{"0": "/dev/null", "1": "pipe:[9]", "3": "socket:[222]", "4": "/var/log/app.log (deleted)", "5": "/memfd:jit (deleted)", "6": "anon_inode:[eventpoll]", "7": "/etc/hosts"}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(directory, "fd", name)); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"cmdline": "app\x00--port\x008080\x00",
		"status":  "Name:\tapp\nUid:\t0\t0\t0\t0\n",
		"net/tcp": "  sl  local_address rem_address   st\n   0: 0100007F:1F90 0100007F:C350 01 0 0 0 0 0 222 1\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	details := readLinuxProcDetails(root, 42)
	want := map[string]int{"device": 1, "pipe": 1, "socket": 1, "file": 3, "anon": 1}
	for kind, count := range want {
		if details.FDTypes[kind] != count {
			t.Errorf("%s = %d, want %d (%v)", kind, details.FDTypes[kind], count, details.FDTypes)
		}
	}
	// memfd는 디스크를 붙잡지 않으므로 지워진 파일로 세지 않는다.
	if len(details.Deleted) != 1 || details.Deleted[0].Path != "/var/log/app.log" {
		t.Fatalf("deleted = %+v", details.Deleted)
	}
	if strings.Join(details.Args, " ") != "app --port 8080" || details.Conns["ESTABLISHED"] != 1 || details.Remotes["127.0.0.1:50000"] != 1 {
		t.Fatalf("details = %+v", details)
	}
}

func TestReadLinuxProcOriginReadsExePackageAndSession(t *testing.T) {
	root := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	process := func(pid, exe, sessionID string) {
		t.Helper()
		write("proc/"+pid+"/sessionid", sessionID)
		if err := os.Symlink(exe, filepath.Join(root, "proc", pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	process("42", "/usr/bin/tmate", "22190")
	// 로그인 세션 밖에서 뜬 process의 session ID다.
	process("43", "/opt/agent/agent (deleted)", "4294967295")
	process("44", "/usr/bin/foo", "22191")
	// 실행 중에 패키지를 업그레이드하면 옛 실행 파일은 지워진 채로 돈다.
	process("45", "/usr/bin/tmate (deleted)", "4294967295")
	write("run/systemd/sessions/22190", "# This is private data. Do not parse.\nUID=0\nUSER=root\nSTATE=closing\nREMOTE=1\nREMOTE_HOST=192.0.2.7\nSERVICE=sshd\nREALTIME=1771936023246410\n")
	// logind의 <id>.ref와 같은 FIFO다. 열면 쓰는 쪽이 올 때까지 멈추므로, 세션 기록이 FIFO면 읽지 않아야 한다.
	if err := syscall.Mkfifo(filepath.Join(root, "run/systemd/sessions/22191"), 0o600); err != nil {
		t.Fatal(err)
	}
	write("var/lib/dpkg/info/tmate.list", "/.\n/usr\n/usr/bin\n/usr/bin/tmate\n")
	write("var/lib/dpkg/info/foo:amd64.list", "/usr/bin/foo\n")
	installed := time.Date(2026, 2, 24, 22, 45, 47, 0, time.UTC)
	for _, list := range []string{"tmate.list", "foo:amd64.list"} {
		if err := os.Chtimes(filepath.Join(root, "var/lib/dpkg/info", list), installed, installed); err != nil {
			t.Fatal(err)
		}
	}

	origin := readLinuxProcOrigin(root, 42)
	session := origin.Session
	if origin.Exe != "/usr/bin/tmate" || !origin.PackageDB || origin.Package != "tmate" || !origin.PackageChanged.Equal(installed) || session == nil {
		t.Fatalf("origin = %+v", origin)
	}
	if *session != (procLoginSession{ID: "22190", User: "root", RemoteHost: "192.0.2.7", Service: "sshd", State: "closing", Start: time.UnixMicro(1771936023246410)}) {
		t.Fatalf("session = %+v", *session)
	}
	// 로그인 세션 밖의 process이고, 지워진 실행 파일은 어느 패키지에도 없다.
	if origin := readLinuxProcOrigin(root, 43); origin.Exe != "/opt/agent/agent (deleted)" || !origin.PackageDB || origin.Package != "" || origin.Session != nil {
		t.Fatalf("origin 43 = %+v", origin)
	}
	if origin := readLinuxProcOrigin(root, 45); origin.Package != "tmate" {
		t.Fatalf("origin 45 = %+v", origin)
	}
	done := make(chan procOrigin, 1)
	go func() { done <- readLinuxProcOrigin(root, 44) }()
	select {
	case origin := <-done:
		if origin.Package != "foo" || !origin.PackageChanged.Equal(installed) || origin.Session != nil {
			t.Fatalf("origin 44 = %+v", origin)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO session record blocked")
	}
	// /proc를 읽지 못하면 아무 줄도 없다.
	if origin := readLinuxProcOrigin(root, 99); origin != (procOrigin{}) {
		t.Fatalf("missing process = %+v", origin)
	}
}

// 이름이 " (deleted)"로 끝나는 파일이 실행 중이면 그 이름이 실행 파일이다. 지워진 파일일 때만 접미사를 뗀다.
func TestLinuxProcExePathKeepsARealDeletedSuffix(t *testing.T) {
	dir := t.TempDir()
	link := func(name, target string) string {
		t.Helper()
		directory := filepath.Join(dir, "proc", name)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, "exe")); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	for _, name := range []string{"tool", "tool (deleted)", "other"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	named := filepath.Join(dir, "tool (deleted)")
	if path, ok := linuxProcExePath("/", link("1", named), named); !ok || path != named {
		t.Fatalf("a running file named with the suffix = %q %v", path, ok)
	}
	// 실행 중인 파일이 그 이름의 파일과 다르면 지워진 실행 파일이다.
	if path, ok := linuxProcExePath("/", link("2", filepath.Join(dir, "other")), named); !ok || path != filepath.Join(dir, "tool") {
		t.Fatalf("a deleted file next to a file named with the suffix = %q %v", path, ok)
	}
	gone := filepath.Join(dir, "gone (deleted)")
	if path, ok := linuxProcExePath("/", link("3", gone), gone); !ok || path != filepath.Join(dir, "gone") {
		t.Fatalf("a deleted file = %q %v", path, ok)
	}
	if path, ok := linuxProcExePath("/", link("4", filepath.Join(dir, "tool")), filepath.Join(dir, "tool")); !ok || path != filepath.Join(dir, "tool") {
		t.Fatalf("a plain file = %q %v", path, ok)
	}
	// 그 이름의 파일을 확인하지 못하면 지워졌다고 단정하지 않는다. 부모가 보통 파일이면 Stat은 ENOTDIR이다.
	unchecked := filepath.Join(dir, "other", "tool (deleted)")
	if path, ok := linuxProcExePath("/", link("5", filepath.Join(dir, "tool")), unchecked); ok {
		t.Fatalf("an unchecked name = %q", path)
	}
}
