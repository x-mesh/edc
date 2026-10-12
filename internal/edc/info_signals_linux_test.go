//go:build linux

package edc

import "testing"

func TestLinuxPlatformJoinsDMIAndHypervisor(t *testing.T) {
	tests := []struct {
		name, vendor, product, assetTag, flags string
		platform, virtual                      string
	}{
		{"cloud", "Amazon EC2", "m5.large", "", "fpu vme hypervisor lahf_lm", "Amazon EC2 m5.large", "virtual machine"},
		{"product repeats vendor", "Google", "Google Compute Engine", "", "fpu hypervisor", "Google Compute Engine", "virtual machine"},
		{"bare metal", "Dell Inc.", "PowerEdge R640", "", "fpu vme", "Dell Inc. PowerEdge R640", ""},
		// okrd-rca-central(OCI Ampere)의 실제 값이다. ARM cpuinfo에는 hypervisor flag가 없다.
		{"oci arm", "QEMU", "KVM Virtual Machine", "OracleCloud.com", "fp asimd evtstrm aes", "Oracle Cloud · QEMU KVM Virtual Machine", ""},
		{"qemu without name", "QEMU", "Standard PC (Q35 + ICH9, 2009)", "", "fpu", "QEMU Standard PC (Q35 + ICH9, 2009)", "virtual machine"},
		{"no dmi", "", "", "", "fpu", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.vendor != "" {
				writeInfoFixture(t, root, "sys/class/dmi/id/sys_vendor", tt.vendor+"\n")
				writeInfoFixture(t, root, "sys/class/dmi/id/product_name", tt.product+"\n")
				writeInfoFixture(t, root, "sys/class/dmi/id/chassis_asset_tag", tt.assetTag+"\n")
			}
			writeInfoFixture(t, root, "proc/cpuinfo", "processor\t: 0\nflags\t\t: "+tt.flags+"\n")
			platform, virtual := linuxPlatform(root)
			if platform != tt.platform || virtual != tt.virtual {
				t.Fatalf("got (%q, %q), want (%q, %q)", platform, virtual, tt.platform, tt.virtual)
			}
		})
	}
}

func TestLinuxContainerMarkers(t *testing.T) {
	root := t.TempDir()
	if got := linuxContainer(root, false); got != "" {
		t.Fatalf("표식이 없는데 container = %q", got)
	}
	writeInfoFixture(t, root, "run/.containerenv", "")
	if got := linuxContainer(root, false); got != "podman" {
		t.Fatalf("container = %q", got)
	}
	writeInfoFixture(t, root, ".dockerenv", "")
	if got := linuxContainer(root, false); got != "docker" {
		t.Fatalf("container = %q", got)
	}
	if got := linuxContainer(root, true); got != "kubernetes" {
		t.Fatalf("container = %q", got)
	}
}

func TestParseLinuxVMStatValue(t *testing.T) {
	input := "pgfault 10\noom_kill 4\npswpout 7\n"
	if value, ok := parseLinuxVMStatValue(input, "oom_kill"); !ok || value != 4 {
		t.Fatalf("oom_kill = %d, %v", value, ok)
	}
	if _, ok := parseLinuxVMStatValue(input, "missing"); ok {
		t.Fatal("없는 counter를 읽었다")
	}
}

// adjtimex는 읽기만 하므로 권한 없이 실제 kernel에서 동작해야 한다.
func TestLinuxClockReadsKernelState(t *testing.T) {
	if linuxClock() == nil {
		t.Fatal("adjtimex를 읽지 못했다")
	}
}

func TestParseLinuxIPv6Default(t *testing.T) {
	zero := "00000000000000000000000000000000"
	input := zero + " 00 " + zero + " 00 fe800000000000000000000000000002 00000400 00000001 00000000 00000003 eno2\n" +
		zero + " 00 " + zero + " 00 fe800000000000000000000000000001 00000100 00000001 00000000 00000003 eno1\n" +
		// 기본 경로가 없으면 kernel은 lo에 reject 경로를 둔다.
		zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200 lo\n" +
		"20010db8000000000000000000000000 40 " + zero + " 00 " + zero + " 00000100 00000001 00000000 00000001 eno1\n"
	if got := parseLinuxIPv6Default(input); got != "fe80::1 dev eno1" {
		t.Fatalf("default = %q", got)
	}
	if got := parseLinuxIPv6Default(zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200 lo\n"); got != "" {
		t.Fatalf("기본 경로가 없는데 %q", got)
	}
}

func TestParseLinuxFileNrAndMountOptions(t *testing.T) {
	if got := parseLinuxFileNr("10240\t0\t9223372036854775807\n"); got == nil || got.Used != 10240 || got.Max != 9223372036854775807 {
		t.Fatalf("file-nr = %+v", got)
	}
	if parseLinuxFileNr("broken") != nil {
		t.Fatal("깨진 file-nr를 읽었다")
	}
	options := parseLinuxMountOptions("/dev/sda3 / ext4 rw,relatime 0 0\n/dev/sdb1 /mnt/my\\040disk xfs ro,noexec 0 0\n/dev/sdc1 / ext4 ro 0 0\n")
	if options["/mnt/my disk"] != "ro,noexec" || options["/"] != "ro" {
		t.Fatalf("options = %v", options)
	}
}
