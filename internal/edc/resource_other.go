//go:build darwin

package edc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type darwinNetwork struct{ packetsIn, packetsOut, bytesIn, bytesOut, errors, drops uint64 }
type darwinDisk struct{ read, write, operations, waitNS uint64 }
type darwinMemory struct {
	total, used, swapOut uint64
	swapOK               bool
}

// darwinProcessTimeout은 ps 한 번을 기다리는 한도다. refresh 간격보다 짧아 다음 갱신과 겹치지 않는다.
const darwinProcessTimeout = 800 * time.Millisecond

// newTopProcessReader는 macOS ps를 쓴다. macOS의 %cpu는 최근 사용률의 감쇠 평균이라 그대로 쓸 수 있다.
func newTopProcessReader() func() ([]topProcess, bool) {
	return func() ([]topProcess, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), darwinProcessTimeout)
		defer cancel()
		output, err := exec.CommandContext(ctx, "/bin/ps", "-Ao", "pid=,pcpu=,rss=,comm=").Output()
		if err != nil {
			return nil, false
		}
		processes := parseTopProcesses(string(output))
		return processes, len(processes) > 0
	}
}

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now()}
	cores, err := readDarwinCoreTicks()
	if err != nil {
		return snapshot, err
	}
	addDarwinCoreTicks(&snapshot, cores)

	// 남은 값은 서로 독립이므로 함께 읽는다. 차례로 부르면 0.6초가 그대로 주기에 붙는다.
	var (
		group   sync.WaitGroup
		network darwinNetwork
		disk    darwinDisk
		memory  darwinMemory
		load    float64
		// networkOK와 diskOK가 false면 counter가 0이므로 다음 rate의 기준으로 쓰면 안 된다.
		networkOK, diskOK bool
	)
	group.Add(4)
	go func() { defer group.Done(); network, networkOK = readDarwinNetwork() }()
	go func() { defer group.Done(); disk, diskOK = readDarwinDisk() }()
	go func() { defer group.Done(); memory = readDarwinMemory() }()
	go func() { defer group.Done(); load = readDarwinLoad() }()
	group.Wait()

	snapshot.NetMissing, snapshot.DiskMissing = !networkOK, !diskOK
	snapshot.PacketsIn, snapshot.PacketsOut = network.packetsIn, network.packetsOut
	snapshot.NetInBytes, snapshot.NetOutBytes = network.bytesIn, network.bytesOut
	snapshot.NetErrors, snapshot.NetDrops, snapshot.NetHealthValid = network.errors, network.drops, networkOK
	snapshot.DiskRead, snapshot.DiskWrite = disk.read, disk.write
	snapshot.DiskOps, snapshot.DiskWaitMS, snapshot.DiskHealthValid = disk.operations, disk.waitNS/uint64(time.Millisecond), diskOK
	snapshot.MemoryTotal, snapshot.MemoryUsed = memory.total, memory.used
	snapshot.SwapOutBytes, snapshot.SwapMissing = memory.swapOut, !memory.swapOK
	snapshot.Load1 = load
	return snapshot, nil
}

// addDarwinCoreTicks는 core별 tick을 /proc/stat과 같은 누적 tick으로 합친다. nice는 Linux처럼 user에 넣는다.
func addDarwinCoreTicks(snapshot *resourceSnapshot, cores []darwinCoreTicks) {
	for _, core := range cores {
		total := core.User + core.System + core.Idle + core.Nice
		snapshot.CPUUser += core.User + core.Nice
		snapshot.CPUSystem += core.System
		snapshot.CPUIdle += core.Idle
		snapshot.CPUTotal += total
		snapshot.Cores = append(snapshot.Cores, resourceCPU{Total: total, Idle: core.Idle})
	}
}

func readDarwinLoad() float64 {
	load, err := readDarwinLoad1()
	if err != nil {
		return 0
	}
	return load
}

// readDarwinNetwork의 bool은 counter를 읽었는지다.
func readDarwinNetwork() (darwinNetwork, bool) {
	counters, err := readDarwinNetworkCounters()
	return counters, err == nil
}

// readDarwinDisk는 block device와 바로 아래 driver까지만 읽는다. 더 깊이 내려가면 APFS container가 만든
// synthesized disk의 driver도 나와 같은 I/O를 두 번 센다. bool은 counter를 읽었는지다.
func readDarwinDisk() (darwinDisk, bool) {
	output, err := exec.Command("/usr/sbin/ioreg", "-r", "-d", "2", "-c", "IOBlockStorageDevice", "-l").Output()
	if err != nil {
		return darwinDisk{}, false
	}
	return parseDarwinDisk(string(output))
}

// parseDarwinDisk는 device마다 driver의 누적 counter를 더한다. 첫 device는 빈 SD card reader일 수 있어 하나만 읽으면 0이 된다.
// disk image의 I/O는 image 파일이 있는 물리 disk에서 이미 세므로 Virtual Interface device는 뺀다.
func parseDarwinDisk(output string) (darwinDisk, bool) {
	var counters darwinDisk
	found := false
	for _, device := range strings.Split("\n"+output, "\n+-o ")[1:] {
		if strings.Contains(device, `"Physical Interconnect"="Virtual Interface"`) {
			continue
		}
		statistics := diskStatisticsPattern.FindStringSubmatch(device)
		if statistics == nil {
			continue
		}
		found = true
		values := map[string]uint64{}
		for _, pair := range diskCounterPattern.FindAllStringSubmatch(statistics[1], -1) {
			values[pair[1]] = parseUnsigned(pair[2])
		}
		counters.read += values["Bytes (Read)"]
		counters.write += values["Bytes (Write)"]
		counters.operations += values["Operations (Read)"] + values["Operations (Write)"]
		// Total Time은 요청마다 걸린 시간을 ns로 누적한 값이다. 읽기 한 번에 약 0.19ms로 NVMe 지연과 맞는다.
		counters.waitNS += values["Total Time (Read)"] + values["Total Time (Write)"]
	}
	return counters, found
}

// readDarwinMemory는 top의 PhysMem 대신 즉시 회수 가능한 page를 빼서 Linux MemAvailable과 같은 기준으로 사용량을 구한다.
// top의 used는 캐시와 compressor를 포함해 평상시에도 97% 이상으로 나온다.
func readDarwinMemory() darwinMemory {
	stats, err := readDarwinVMStatistics()
	if err != nil {
		return darwinMemory{}
	}
	pageSize, err := darwinPageSize()
	if err != nil {
		return darwinMemory{}
	}
	return darwinMemoryFromStatistics(stats, pageSize, darwinMemoryTotal())
}

// darwinMemoryTotal은 바뀌지 않는 값이라 한 번만 읽는다.
var darwinMemoryTotal = sync.OnceValue(func() uint64 {
	total, err := readDarwinMemorySize()
	if err != nil {
		return 0
	}
	return total
})

// darwinMemoryFromStatistics는 free와 inactive page를 즉시 회수 가능한 memory로 본다.
// FreeCount에는 speculative page가 이미 들어 있어 vm_stat의 free, speculative, inactive 합과 같다.
func darwinMemoryFromStatistics(stats darwinVMStatistics64, pageSize, total uint64) darwinMemory {
	memory := darwinMemory{swapOut: stats.Swapouts * pageSize, swapOK: true}
	if available := (uint64(stats.FreeCount) + uint64(stats.InactiveCount)) * pageSize; total > 0 && available > 0 && available <= total {
		memory.total, memory.used = total, total-available
	}
	return memory
}

var (
	diskStatisticsPattern = regexp.MustCompile(`"Statistics" = \{([^}]*)\}`)
	diskCounterPattern    = regexp.MustCompile(`"([^"]+)"=([0-9]+)`)
)

func collectHostDetails() (hostDetails, error) {
	var details hostDetails
	details.Hostname, _ = os.Hostname()
	details.System = runtime.GOOS
	details.OS = "macOS"
	details.Machine = runtimeMachine()
	details.Cores = runtime.NumCPU()
	details.Version = commandValue("/usr/bin/sw_vers", "-productVersion")
	details.Release = commandValue("/usr/bin/uname", "-r")
	details.Processor = commandValue("/usr/bin/uname", "-v")
	details.Model = commandValue("/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string")
	details.MemoryTotal, _ = strconv.ParseUint(commandValue("/usr/sbin/sysctl", "-n", "hw.memsize"), 10, 64)
	var used, total float64
	fmt.Sscanf(commandValue("/usr/sbin/sysctl", "-n", "vm.swapusage"), "total = %fM used = %fM", &total, &used)
	details.SwapTotal, details.SwapUsed = uint64(total*1024*1024), uint64(used*1024*1024)
	fmt.Sscanf(commandValue("/usr/sbin/sysctl", "-n", "vm.loadavg"), "{ %f %f %f }", &details.Load[0], &details.Load[1], &details.Load[2])
	var bootSeconds int64
	fmt.Sscanf(commandValue("/usr/sbin/sysctl", "-n", "kern.boottime"), "{ sec = %d,", &bootSeconds)
	if bootSeconds > 0 {
		details.Uptime = time.Since(time.Unix(bootSeconds, 0))
	}
	details.RLimitSoft = parseLimit(commandValue("/bin/sh", "-c", "ulimit -Sn"))
	details.RLimitHard = parseLimit(commandValue("/bin/sh", "-c", "ulimit -Hn"))
	return details, nil
}

func collectDefaultRoute() (string, string) {
	output, _ := exec.Command("/sbin/route", "-n", "get", "default").Output()
	var iface, gateway string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			iface = fields[1]
		}
		if len(fields) == 2 && fields[0] == "gateway:" {
			gateway = fields[1]
		}
	}
	return iface, gateway
}
func collectDisks() ([]diskDetails, error) { return collectDisksFromDF() }
func commandValue(path string, args ...string) string {
	output, _ := exec.Command(path, args...).Output()
	return strings.TrimSpace(string(output))
}
func parseHumanBytes(value string) uint64 {
	value = strings.TrimSpace(strings.TrimSuffix(value, "."))
	if value == "" {
		return 0
	}
	multiplier := float64(1)
	last := value[len(value)-1]
	if last < '0' || last > '9' {
		value = value[:len(value)-1]
		switch last {
		case 'K':
			multiplier = 1024
		case 'M':
			multiplier = 1024 * 1024
		case 'G':
			multiplier = 1024 * 1024 * 1024
		case 'T':
			multiplier = 1024 * 1024 * 1024 * 1024
		}
	}
	number, _ := strconv.ParseFloat(value, 64)
	return uint64(number * multiplier)
}
func parseUnsigned(value string) uint64 { result, _ := strconv.ParseUint(value, 10, 64); return result }
func parseLimit(value string) uint64 {
	if value == "unlimited" {
		return ^uint64(0)
	}
	result, _ := strconv.ParseUint(value, 10, 64)
	return result
}
