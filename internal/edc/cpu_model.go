package edc

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// linuxCPUModel은 /proc/cpuinfo에서 CPU 이름을 읽는다. x86은 "model name"을 주지만 arm64는 주지 않고
// 제조사 번호(CPU implementer)와 부품 번호(CPU part)만 준다. 그때는 lscpu처럼 번호를 이름으로 바꾼다.
func linuxCPUModel(cpuinfo string) string {
	var implementer, part int64 = -1, -1
	var names []string
	add := func() {
		if implementer < 0 || part < 0 {
			return
		}
		if name := armCPUName(implementer, part); !slices.Contains(names, name) {
			names = append(names, name)
		}
		implementer, part = -1, -1
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "model name":
			return value
		case "processor":
			add()
		case "CPU implementer":
			implementer = parseCPUInfoNumber(value)
		case "CPU part":
			part = parseCPUInfoNumber(value)
		}
	}
	add()
	// big.LITTLE처럼 코어 종류가 섞이면 처음 나온 순서대로 모두 적는다.
	return strings.Join(names, " + ")
}

func parseCPUInfoNumber(value string) int64 {
	number, err := strconv.ParseInt(value, 0, 64)
	if err != nil {
		return -1
	}
	return number
}

// armImplementers와 armParts는 util-linux sys-utils/lscpu-arm.c의 번호다. 서버, 클라우드, 보드에서 만나는 코어만 옮겼다.
// 표에 없는 부품은 번호를 그대로 적는다. 비워 두면 CPU를 모른다는 사실도 감춰진다.
var armImplementers = map[int64]string{
	0x41: "ARM", 0x46: "FUJITSU", 0x48: "HiSilicon", 0x4e: "NVIDIA", 0x51: "Qualcomm", 0x61: "Apple", 0xc0: "Ampere",
}

var armParts = map[int64]map[int64]string{
	0x41: {
		0xd03: "Cortex-A53", 0xd05: "Cortex-A55", 0xd07: "Cortex-A57", 0xd08: "Cortex-A72", 0xd09: "Cortex-A73",
		0xd0a: "Cortex-A75", 0xd0b: "Cortex-A76", 0xd0c: "Neoverse-N1", 0xd0d: "Cortex-A77", 0xd40: "Neoverse-V1",
		0xd41: "Cortex-A78", 0xd46: "Cortex-A510", 0xd47: "Cortex-A710", 0xd49: "Neoverse-N2", 0xd4a: "Neoverse-E1",
		0xd4f: "Neoverse-V2", 0xd80: "Cortex-A520", 0xd81: "Cortex-A720", 0xd84: "Neoverse-V3", 0xd8e: "Neoverse-N3",
	},
	0x46: {0x001: "A64FX"},
	0x48: {0xd01: "Kunpeng-920"},
	0xc0: {0xac3: "Ampere-1", 0xac4: "Ampere-1a"},
}

func armCPUName(implementer, part int64) string {
	vendor, known := armImplementers[implementer]
	if !known {
		return fmt.Sprintf("implementer %#x part %#x", implementer, part)
	}
	name, known := armParts[implementer][part]
	if !known {
		return fmt.Sprintf("%s part %#x", vendor, part)
	}
	// Ampere-1처럼 이름에 제조사가 들어 있으면 되풀이하지 않는다.
	if strings.HasPrefix(name, vendor) {
		return name
	}
	return vendor + " " + name
}
