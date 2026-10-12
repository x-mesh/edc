package edc

import (
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
	if got := infoPressureLine(signals); got != "Pressure (some avg10): CPU 0.50% · Memory 12.25% · I/O 0.00% · OOM kills since boot: 3" {
		t.Fatalf("pressure = %q", got)
	}
	if got := infoPressureLine(infoSignals{}); got != "" {
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
