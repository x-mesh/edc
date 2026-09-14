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

type darwinNetwork struct{ packetsIn, packetsOut, bytesIn, bytesOut uint64 }
type darwinDisk struct{ read, write uint64 }
type darwinMemory struct{ total, used uint64 }

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
	)
	group.Add(4)
	go func() { defer group.Done(); network = readDarwinNetwork() }()
	go func() { defer group.Done(); disk = readDarwinDisk() }()
	go func() { defer group.Done(); memory = readDarwinMemory() }()
	go func() { defer group.Done(); load = readDarwinLoad() }()
	group.Wait()

	snapshot.PacketsIn, snapshot.PacketsOut = network.packetsIn, network.packetsOut
	snapshot.NetInBytes, snapshot.NetOutBytes = network.bytesIn, network.bytesOut
	snapshot.DiskRead, snapshot.DiskWrite = disk.read, disk.write
	snapshot.MemoryTotal, snapshot.MemoryUsed = memory.total, memory.used
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
	var load float64
	fmt.Sscanf(commandValue("/usr/sbin/sysctl", "-n", "vm.loadavg"), "{ %f", &load)
	return load
}

func readDarwinNetwork() darwinNetwork {
	var counters darwinNetwork
	output, err := exec.Command("/usr/sbin/netstat", "-ibn").Output()
	if err != nil {
		return counters
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[2] == "Network" || !strings.HasPrefix(fields[2], "<Link#") || fields[0] == "lo0" || strings.HasSuffix(fields[0], "*") || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		counters.packetsIn += parseUnsigned(fields[4])
		counters.bytesIn += parseUnsigned(fields[6])
		counters.packetsOut += parseUnsigned(fields[7])
		counters.bytesOut += parseUnsigned(fields[9])
	}
	return counters
}

func readDarwinDisk() darwinDisk {
	var counters darwinDisk
	output, err := exec.Command("/usr/sbin/ioreg", "-r", "-c", "IOBlockStorageDriver", "-l").Output()
	if err != nil {
		return counters
	}
	match := diskBytePattern.FindStringSubmatch(string(output))
	if match == nil {
		return counters
	}
	if match[1] != "" {
		counters.read, counters.write = parseUnsigned(match[1]), parseUnsigned(match[2])
	} else {
		counters.write, counters.read = parseUnsigned(match[3]), parseUnsigned(match[4])
	}
	return counters
}

// applyDarwinMemory는 top의 PhysMem 대신 vm_stat의 회수 가능 page를 빼서 Linux MemAvailable과 같은 기준으로 사용량을 구한다.
// top의 used는 캐시와 compressor를 포함해 평상시에도 97% 이상으로 나온다.
func readDarwinMemory() darwinMemory {
	total := darwinMemoryTotal()
	if total == 0 {
		return darwinMemory{}
	}
	output, err := exec.Command("/usr/bin/vm_stat").Output()
	if err != nil {
		return darwinMemory{}
	}
	available := parseDarwinAvailableMemory(string(output))
	if available == 0 || available > total {
		return darwinMemory{}
	}
	return darwinMemory{total: total, used: total - available}
}

// darwinMemoryTotal은 바뀌지 않는 값이라 한 번만 읽는다. sample마다 sysctl을 부르면 그만큼 주기가 길어진다.
var darwinMemoryTotal = sync.OnceValue(func() uint64 {
	total, err := strconv.ParseUint(commandValue("/usr/sbin/sysctl", "-n", "hw.memsize"), 10, 64)
	if err != nil {
		return 0
	}
	return total
})

// parseDarwinAvailableMemory는 즉시 회수 가능한 free, speculative, inactive page를 합친다.
func parseDarwinAvailableMemory(output string) uint64 {
	pageSize := uint64(4096)
	pages := make(map[string]uint64, 16)
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Mach Virtual Memory Statistics") {
			var size uint64
			if _, err := fmt.Sscanf(line, "Mach Virtual Memory Statistics: (page size of %d bytes)", &size); err == nil && size > 0 {
				pageSize = size
			}
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		pages[strings.TrimSpace(name)] = parseUnsigned(strings.TrimSuffix(strings.TrimSpace(value), "."))
	}
	return (pages["Pages free"] + pages["Pages speculative"] + pages["Pages inactive"]) * pageSize
}

var diskBytePattern = regexp.MustCompile(`"Bytes \(Read\)"=([0-9]+)[^}]*"Bytes \(Write\)"=([0-9]+)|"Bytes \(Write\)"=([0-9]+)[^}]*"Bytes \(Read\)"=([0-9]+)`)

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
	details.PythonVersion = detectPythonVersion()
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
