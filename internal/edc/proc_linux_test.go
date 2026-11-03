//go:build linux

package edc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
