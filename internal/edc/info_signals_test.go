package edc

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeInfoFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// systemd-resolved는 resolv.conf에 local stub만 적는다. 그대로 보여 주면 실제로 어느 서버에 묻는지 알 수 없다.
func TestReadInfoDNSFollowsSystemdResolvedStub(t *testing.T) {
	root := t.TempDir()
	writeInfoFixture(t, root, "etc/resolv.conf", "nameserver 127.0.0.53\noptions edns0\nsearch corp.example\n")
	writeInfoFixture(t, root, "run/systemd/resolve/resolv.conf", "nameserver 192.0.2.53\nnameserver 192.0.2.54\nsearch corp.example\n")
	dns := readInfoDNS(root)
	if got := infoDNSLine(dns); got != "192.0.2.53, 192.0.2.54 via systemd-resolved · search corp.example" {
		t.Fatalf("DNS = %q", got)
	}

	plain := t.TempDir()
	writeInfoFixture(t, plain, "etc/resolv.conf", "nameserver 192.0.2.1\nnameserver 192.0.2.2\nnameserver 192.0.2.3\nnameserver 192.0.2.4\n")
	if got := infoDNSLine(readInfoDNS(plain)); got != "192.0.2.1, 192.0.2.2, 192.0.2.3 (+1)" {
		t.Fatalf("DNS = %q", got)
	}
	if got := infoDNSLine(readInfoDNS(t.TempDir())); !strings.HasPrefix(got, "unavailable: ") {
		t.Fatalf("파일이 없는데 DNS = %q", got)
	}
}

func TestInfoSignalLines(t *testing.T) {
	kills := uint64(3)
	signals := infoSignals{Platform: "Amazon EC2 m5.large", Virtual: "virtual machine", Container: "docker", Pressure: []float64{0.5, 12.25, 0}, OOMKills: &kills}
	if got := infoPlatformLine(signals); got != "Amazon EC2 m5.large · virtual machine · container: docker" {
		t.Fatalf("platform = %q", got)
	}
	if got := infoPressureLine(signals, false); got != "Pressure (some avg10): CPU 0.50% · Memory 12.25% · I/O 0.00% · OOM kills since boot: 3" {
		t.Fatalf("pressure = %q", got)
	}
	if got := infoPressureLine(infoSignals{}, false); got != "" {
		t.Fatalf("값이 없는데 pressure = %q", got)
	}
	if got := infoClockLine(&infoClock{Synced: true}); got != "synchronized" {
		t.Fatalf("clock = %q", got)
	}
	if got := infoClockLine(&infoClock{}); !strings.HasPrefix(got, "not synchronized") {
		t.Fatalf("clock = %q", got)
	}
	if got := infoFileHandlesLine(&infoFileHandles{Used: 3228, Max: math.MaxInt64}); got != "3228 / unlimited" {
		t.Fatalf("file handles = %q", got)
	}
	if infoClockLine(nil) != "" || infoPlatformLine(infoSignals{}) != "" {
		t.Fatal("읽지 못한 값을 출력했다")
	}
}

// 정상인 디스크에는 아무것도 붙이지 않는다. 경고는 읽기 전용이거나 inode가 바닥날 때만이다.
func TestDiskSignalSuffix(t *testing.T) {
	tests := []struct {
		signal diskSignal
		want   string
	}{
		{diskSignal{InodesUsed: 79, InodesTotal: 100}, ""},
		{diskSignal{InodesUsed: 80, InodesTotal: 100}, "  (inodes 80%)"},
		{diskSignal{ReadOnly: true, InodesUsed: 95, InodesTotal: 100}, "  (read-only · inodes 95%)"},
		{diskSignal{ReadOnly: true}, "  (read-only)"},
		{diskSignal{}, ""},
		{diskSignal{DeletedOpen: deletedOpenSummary{Files: 3, Bytes: 12 << 30, Top: []deletedOpenFile{{Comm: "java"}}}}, "  (deleted-open 12.0 GB in 3 files, java)"},
		{diskSignal{ReadOnly: true, DeletedOpen: deletedOpenSummary{Files: 1, Bytes: 1 << 20, Partial: true}}, "  (read-only · deleted-open ≥1.0 MB in 1 file)"},
		// 읽지 못한 fd가 있어도 찾은 파일이 없으면 줄에 붙이지 않는다. 하한 0은 정보가 아니다.
		{diskSignal{DeletedOpen: deletedOpenSummary{Partial: true}}, ""},
	}
	for _, tt := range tests {
		if got := diskSignalSuffix(tt.signal); got != tt.want {
			t.Fatalf("%+v = %q, want %q", tt.signal, got, tt.want)
		}
	}
}

func TestInfoVerboseNetworkAndDiskDetails(t *testing.T) {
	// visibleDisks는 실제로 있는 디렉터리만 보여 준다.
	data := t.TempDir()
	signals := infoSignals{
		DNS:         infoDNS{Config: resolvConfig{Nameservers: []string{"192.0.2.53"}}},
		IPv6Default: "fe80::1 dev eno1",
		FileHandles: &infoFileHandles{Used: 2048, Max: 8192},
		Interfaces:  []infoInterface{{Name: "eno1", MTU: 9000, MAC: "00:00:5e:00:53:01", Link: "1000 Mb/s full", IPv6: []string{"2001:db8::5/64"}}},
		Disks:       map[string]diskSignal{"/": {Options: "rw,relatime"}, data: {ReadOnly: true, Options: "ro,noexec"}},
	}
	interfaces := []interfaceDetails{{Name: "eno1", Address: "192.0.2.5", Mask: "255.255.255.0", Gateway: "192.0.2.1"}}
	disks := []diskDetails{{Mount: "/", Device: "/dev/sda1", Total: 100, Used: 50, Percent: 50}, {Mount: data, Device: "/dev/sdb1", Total: 100, Used: 10, Percent: 10}}
	var verbose strings.Builder
	printInfo(&verbose, "test", hostDetails{}, interfaces, disks, nil, signals, false, true)
	for _, want := range []string{
		"├── Open files (system): 2048 / 8192 (25.00%)\n",
		"├── IPv6 default route: fe80::1 dev eno1\n",
		"├── Local IP\n│   └── eno1           : 192.0.2.5 / 255.255.255.0, G/W: 192.0.2.1\n└── Interfaces\n",
		"    └── eno1           : mtu 9000 · 1000 Mb/s full · MAC 00:00:5e:00:53:01 · IPv6 2001:db8::5/64\n",
		"│   options: rw,relatime\n",
		"(read-only)\n    options: ro,noexec\n",
	} {
		if !strings.Contains(verbose.String(), want) {
			t.Fatalf("-v 출력에 %q가 없다:\n%s", want, verbose.String())
		}
	}

	// -v가 아니면 수집도 하지 않으므로 값이 비어 있다. 기본 출력의 Local IP는 마지막 가지로 남는다.
	var compact strings.Builder
	printInfo(&compact, "test", hostDetails{}, interfaces, disks, nil, infoSignals{DNS: signals.DNS}, false, false)
	if !strings.Contains(compact.String(), "└── Local IP\n    └── eno1") || strings.Contains(compact.String(), "Interfaces") || strings.Contains(compact.String(), "options:") {
		t.Fatalf("기본 출력이 바뀌었다:\n%s", compact.String())
	}
}

func TestInfoPressureLineColorsOOMKillsOnlyWhenPresent(t *testing.T) {
	none, some := uint64(0), uint64(2)
	if got := infoPressureLine(infoSignals{OOMKills: &none}, true); got != "OOM kills since boot: 0" {
		t.Fatalf("0회에 색을 입혔다: %q", got)
	}
	if got := infoPressureLine(infoSignals{OOMKills: &some}, true); got != topColorWarn+"OOM kills since boot: 2"+topColorReset {
		t.Fatalf("OOM이 있는데 강조하지 않았다: %q", got)
	}
	if got := infoPressureLine(infoSignals{OOMKills: &some}, false); got != "OOM kills since boot: 2" {
		t.Fatalf("색이 꺼졌는데 escape가 남았다: %q", got)
	}
}

// 가짜 /proc에서 지워진 파일을 센다. 같은 inode를 두 process가 열어도 한 번이고, hard link가 남은 파일과 디렉터리는 빼며,
// 읽지 못한 fd가 있으면 하한으로 표시한다.
func TestScanDeletedOpenCountsEachInodeOnce(t *testing.T) {
	root := t.TempDir()
	link := func(pid, fd, target string) {
		dir := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, fd)); err != nil {
			t.Fatal(err)
		}
	}
	comm := func(pid, name string) {
		if err := os.WriteFile(filepath.Join(root, pid, "comm"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link("100", "3", "/var/log/app.log (deleted)")
	link("100", "4", "/var/log/app.log (deleted)")
	link("100", "5", "/etc/hosts")
	link("100", "6", "/tmp/linked (deleted)")
	link("100", "7", "/tmp/dir (deleted)")
	comm("100", "java")
	link("200", "3", "/var/log/app.log (deleted)")
	link("200", "4", "/data/db/wal.0 (deleted)")
	link("200", "5", "/missing (deleted)")
	comm("200", "postgres")
	if err := os.MkdirAll(filepath.Join(root, "notapid", "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	stats := map[string]deletedOpenStat{
		"/var/log/app.log (deleted)": {Dev: 1, Inode: 10, Size: 4 << 30, Regular: true},
		"/tmp/linked (deleted)":      {Dev: 1, Inode: 11, Size: 1 << 30, Links: 1, Regular: true},
		"/tmp/dir (deleted)":         {Dev: 1, Inode: 12, Size: 4096},
		"/data/db/wal.0 (deleted)":   {Dev: 2, Inode: 20, Size: 16 << 20, Regular: true},
	}
	stat := func(path string) (deletedOpenStat, error) {
		target, err := os.Readlink(path)
		if err != nil {
			return deletedOpenStat{}, err
		}
		info, ok := stats[target]
		if !ok {
			return deletedOpenStat{}, os.ErrNotExist
		}
		return info, nil
	}
	summaries := scanDeletedOpen(root, stat, infoDeletedOpenFDLimit)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v", summaries)
	}
	first := summaries[1]
	if first.Files != 1 || first.Bytes != 4<<30 || first.Partial || len(first.Top) != 1 || first.Top[0] != (deletedOpenFile{Path: "/var/log/app.log", Size: 4 << 30, PID: 100, Comm: "java"}) {
		t.Fatalf("dev 1 = %+v, top %+v", first, first.Top)
	}
	second := summaries[2]
	if second.Files != 1 || second.Bytes != 16<<20 || second.Top[0].Comm != "postgres" || second.Top[0].PID != 200 {
		t.Fatalf("dev 2 = %+v, top %+v", second, second.Top)
	}
	// fd 상한에 닿으면 멈추고 하한으로 표시한다.
	limited := scanDeletedOpen(root, stat, 2)
	if len(limited) != 1 || !limited[1].Partial || limited[1].Files != 1 {
		t.Fatalf("limited = %+v", limited)
	}
	// 읽지 못한 fd 디렉터리는 다른 사용자의 process다. 찾은 값은 하한이 된다.
	if err := os.Chmod(filepath.Join(root, "200", "fd"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "200", "fd"), 0o755) })
	if os.Getuid() != 0 {
		denied := scanDeletedOpen(root, stat, infoDeletedOpenFDLimit)
		if len(denied) != 1 || !denied[1].Partial {
			t.Fatalf("denied = %+v", denied)
		}
	}
}

func TestDeletedOpenSummaryKeepsTheLargestFiles(t *testing.T) {
	var summary deletedOpenSummary
	for size := uint64(1); size <= 8; size++ {
		summary.add(deletedOpenFile{Path: fmt.Sprint("/f", size), Size: size})
	}
	if len(summary.Top) != infoDeletedOpenTop || summary.Top[0].Size != 8 || summary.Top[4].Size != 4 {
		t.Fatalf("top = %+v", summary.Top)
	}
	lines := deletedOpenLines(deletedOpenSummary{Top: []deletedOpenFile{{Path: "/var/log/app.log", Size: 4 << 30, PID: 100, Comm: "java"}}})
	if len(lines) != 1 || lines[0] != "   4.0 GB  pid 100 java  /var/log/app.log" {
		t.Fatalf("lines = %q", lines)
	}
}
