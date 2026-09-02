//go:build darwin

package edc

import (
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

// darwinCPU는 process가 사는 동안 top을 배경에서 돌려 CPU 사용률을 최신으로 유지한다.
var darwinCPU darwinCPUSampler

// darwinCPUSample은 top이 읽어 온 CPU 사용률 한 벌이다.
type darwinCPUSample struct {
	user, system, idle, total uint64
	valid                     bool
}

// darwinCPUSampler는 top 호출을 snapshot 수집에서 떼어 낸다.
// top -l 1은 CPU 델타를 재려고 1초 넘게 기다리므로, 같은 goroutine에서 부르면
// --interval을 200ms로 줄여도 화면은 그만큼 늦게 갱신된다.
// 첫 값만 동기로 받고 그다음부터는 배경 goroutine이 채운 값을 즉시 돌려준다.
type darwinCPUSampler struct {
	mutex  sync.RWMutex
	sample darwinCPUSample
	start  sync.Once
}

func (sampler *darwinCPUSampler) latest() darwinCPUSample {
	sampler.start.Do(func() {
		// 첫 화면에 0%가 뜨지 않도록 한 번은 기다린다.
		sampler.refresh()
		go func() {
			// top 자체가 1초 넘게 걸리므로 따로 쉬지 않는다. process가 끝나면 함께 사라진다.
			for {
				sampler.refresh()
			}
		}()
	})
	sampler.mutex.RLock()
	defer sampler.mutex.RUnlock()
	return sampler.sample
}

func (sampler *darwinCPUSampler) refresh() {
	output, err := exec.Command("/usr/bin/top", "-l", "1", "-n", "0").Output()
	if err != nil {
		return
	}
	sample, ok := parseDarwinCPU(string(output))
	if !ok {
		return
	}
	sampler.mutex.Lock()
	sampler.sample = sample
	sampler.mutex.Unlock()
}

// parseDarwinCPU는 top의 CPU usage 줄을 읽는다. 백분율이므로 total은 10000(=100.00%)이다.
func parseDarwinCPU(output string) (darwinCPUSample, bool) {
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "CPU usage:") {
			continue
		}
		var user, system, idle float64
		if _, err := fmt.Sscanf(line, "CPU usage: %f%% user, %f%% sys, %f%% idle", &user, &system, &idle); err != nil {
			return darwinCPUSample{}, false
		}
		return darwinCPUSample{
			user: uint64(user * 100), system: uint64(system * 100), idle: uint64(idle * 100),
			total: 10000, valid: true,
		}, true
	}
	return darwinCPUSample{}, false
}

type darwinNetwork struct{ packetsIn, packetsOut, bytesIn, bytesOut uint64 }
type darwinDisk struct{ read, write uint64 }
type darwinMemory struct{ total, used uint64 }

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now(), CPUInstant: true}

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

	if sample := darwinCPU.latest(); sample.valid {
		snapshot.CPUUser, snapshot.CPUSystem = sample.user, sample.system
		snapshot.CPUIdle, snapshot.CPUTotal = sample.idle, sample.total
	}
	return snapshot, nil
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
