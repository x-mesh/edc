package edc

import "testing"

// ociAmpereCPUInfo는 OCI Ampere A1 instance의 /proc/cpuinfo 앞부분이다. lscpu는 이 CPU를 ARM Neoverse-N1로 적는다.
const ociAmpereCPUInfo = `processor	: 0
BogoMIPS	: 50.00
Features	: fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid asimdrdm lrcpc dcpop asimddp
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1

processor	: 1
BogoMIPS	: 50.00
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1
`

func TestLinuxCPUModel(t *testing.T) {
	arm := func(implementer, part string) string {
		return "processor\t: 0\nCPU implementer\t: " + implementer + "\nCPU part\t: " + part + "\n"
	}
	tests := []struct {
		name, cpuinfo, want string
	}{
		{"x86 model name", "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Gold 6128 CPU @ 3.40GHz\n", "Intel(R) Xeon(R) Gold 6128 CPU @ 3.40GHz"},
		{"oci ampere a1", ociAmpereCPUInfo, "ARM Neoverse-N1"},
		{"vendor in the name", arm("0xc0", "0xac3"), "Ampere-1"},
		{"graviton 3", arm("0x41", "0xd40"), "ARM Neoverse-V1"},
		// RK3588 같은 big.LITTLE 보드는 코어 종류가 둘이다.
		{"big.LITTLE", arm("0x41", "0xd05") + "\n" + arm("0x41", "0xd05") + "\n" + arm("0x41", "0xd0b"), "ARM Cortex-A55 + ARM Cortex-A76"},
		{"unknown part", arm("0x41", "0xfff"), "ARM part 0xfff"},
		{"unknown implementer", arm("0x99", "0x001"), "implementer 0x99 part 0x1"},
		{"no identity", "processor\t: 0\nBogoMIPS\t: 50.00\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linuxCPUModel(tt.cpuinfo); got != tt.want {
				t.Fatalf("model = %q, want %q", got, tt.want)
			}
		})
	}
}
