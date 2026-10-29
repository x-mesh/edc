//go:build darwin

package edc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
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
	swapUsed, swapTotal  uint64
	swapUsageOK          bool
	compressor           uint64
	compressorOK         bool
}

// darwinProcessTimeout은 ps 한 번을 기다리는 한도다. refresh 간격보다 짧아 다음 갱신과 겹치지 않는다.
const darwinProcessTimeout = 800 * time.Millisecond

var darwinProcessRusageReader = readDarwinProcessRusage
var darwinProcessTaskInfoReader = readDarwinProcessTaskInfo
var darwinProcessNow = time.Now

// newTopProcessReader는 macOS ps를 쓴다. macOS의 %cpu는 최근 사용률의 감쇠 평균이라 그대로 쓸 수 있다.
func newTopProcessReaderWithRusage(readRusage bool) func() ([]topProcess, bool) {
	var kernel darwinKernelTaskCPU
	return func() ([]topProcess, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), darwinProcessTimeout)
		defer cancel()
		args := []string{"-Ao", "pid=,pcpu=,rss=,state=,lstart=,comm="}
		if topFullCommand {
			// -ww는 ps가 출력 폭에 맞춰 argv를 자르지 않게 한다.
			args = []string{"-ww", "-Ao", "pid=,pcpu=,rss=,state=,lstart=,args="}
		}
		command := exec.CommandContext(ctx, "/bin/ps", args...)
		// lstart의 요일과 달 이름이 지역 설정을 따르면 시작 시각을 읽을 수 없다.
		command.Env = append(os.Environ(), "LC_ALL=C")
		output, err := command.Output()
		if err != nil {
			return nil, false
		}
		processes := parseTopProcesses(string(output))
		for index := range processes {
			threads, err := readDarwinProcessThreads(processes[index].PID)
			if err == nil {
				processes[index].Threads = threads
			}
			// 다른 사용자 소유 process는 root로 실행할 때만 읽는다. 그때는 MemoryValid가 false로 남아 RSS로 대신한다.
			if readRusage {
				if usage, err := darwinProcessRusageReader(processes[index].PID); err == nil {
					processes[index].Memory, processes[index].MemoryValid = usage.Footprint, true
					processes[index].diskSampled, processes[index].diskStarted = true, usage.Started
					processes[index].diskReadBytes, processes[index].diskWriteBytes = usage.DiskRead, usage.DiskWrite
					processes[index].diskSampledAt = darwinProcessNow()
				}
			}
		}
		if readRusage {
			if process, ok := kernel.read(); ok {
				processes = sortTopProcessesByCPU(append(processes, process))
			}
		}
		return processes, len(processes) > 0
	}
}

// darwinKernelTaskCPU는 kernel_task(PID 0)의 직전 CPU 시간이다. ps는 PID 0을 출력하지 않지만 kernel_task의 CPU는
// iowait와 disk busy가 없는 macOS에서 I/O와 발열 부하를 보는 주된 신호라 libproc으로 따로 읽는다.
// root가 아니면 libproc이 EPERM으로 거부해 행이 생기지 않는다.
type darwinKernelTaskCPU struct {
	ticks darwinMachTicks
	at    time.Time
}

// read는 첫 관측에서 CPU 0을 준다. ps의 %cpu와 달리 누적 시간만 있어 직전 기준이 있어야 rate를 구한다.
// Rosetta에서는 rusage의 tick 단위가 번역된 timebase와 어긋나 CPU를 0으로 둔다.
func (kernel *darwinKernelTaskCPU) read() (topProcess, bool) {
	usage, err := darwinProcessRusageReader(0)
	if err != nil {
		return topProcess{}, false
	}
	now := darwinProcessNow()
	// kernel_task의 resident 크기는 root여도 rusage_info와 proc_taskinfo 모두 0을 준다.
	process := topProcess{PID: 0, Command: "kernel_task", RSSMissing: true, Memory: usage.Footprint, MemoryValid: true}
	process.diskSampled, process.diskStarted, process.diskSampledAt = true, usage.Started, now
	process.diskReadBytes, process.diskWriteBytes = usage.DiskRead, usage.DiskWrite
	if task, err := darwinProcessTaskInfoReader(0); err == nil {
		process.Threads = int(task.Threads)
	}
	ticks := usage.UserTime + usage.SystemTime
	timebase, err := darwinMachTimebase()
	if seconds := now.Sub(kernel.at).Seconds(); err == nil && !darwinTranslated() && !kernel.at.IsZero() && seconds > 0 && ticks >= kernel.ticks {
		process.CPU = timebase.duration(ticks-kernel.ticks).Seconds() / seconds * 100
	}
	kernel.ticks, kernel.at = ticks, now
	return process, true
}

func newTopProcessReader() func() ([]topProcess, bool) { return newTopProcessReaderWithRusage(true) }

func darwinRusageReads(hasSample bool) int {
	if hasSample {
		return 0
	}
	return 1
}

func darwinProcessSampleTime(process topProcess, fallback time.Time) time.Time {
	if process.diskSampled && !process.diskSampledAt.IsZero() {
		return process.diskSampledAt
	}
	return fallback
}

type darwinProcessIOSample struct {
	stats darwinProcessRusage
	at    time.Time
}

// newTopProcessEnricher는 필터로 고른 process에 I/O rate와 fd 수를 채운다.
func newTopProcessEnricher() func([]topProcess) {
	fill, count := newTopProcessIOFiller(), newTopProcessFDCounter()
	return func(processes []topProcess) {
		fill(processes)
		count(processes)
	}
}

// newTopProcessIOFiller는 수집기가 읽어 둔 rusage counter의 차이로 I/O rate를 구한다. 그 counter가 없을 때만 rusage를
// 다시 읽는다. 필터가 없을 때도 같은 방식을 써야 macOS history에 디스크 I/O가 남는다.
func newTopProcessIOFiller() func([]topProcess) {
	previous := map[int]darwinProcessIOSample{}
	return func(processes []topProcess) {
		current := make(map[int]darwinProcessIOSample, len(processes))
		for index := range processes {
			process := &processes[index]
			process.DiskValid, process.DiskStatus = false, ""
			stats := darwinProcessRusage{Started: process.diskStarted, DiskRead: process.diskReadBytes, DiskWrite: process.diskWriteBytes}
			if darwinRusageReads(process.diskSampled) > 0 {
				var err error
				stats, err = darwinProcessRusageReader(process.PID)
				if err != nil {
					process.DiskStatus = err.Error()
					continue
				}
			}
			now := darwinProcessSampleTime(*process, darwinProcessNow())
			current[process.PID] = darwinProcessIOSample{stats: stats, at: now}
			before, seen := previous[process.PID]
			seconds := now.Sub(before.at).Seconds()
			if !seen || seconds <= 0 || stats.Started != before.stats.Started || stats.DiskRead < before.stats.DiskRead || stats.DiskWrite < before.stats.DiskWrite {
				process.DiskStatus = "waiting for I/O baseline"
				continue
			}
			process.DiskValid = true
			process.DiskRead = float64(stats.DiskRead-before.stats.DiskRead) / seconds
			process.DiskWrite = float64(stats.DiskWrite-before.stats.DiskWrite) / seconds
		}
		previous = current
	}
}

// newTopProcessFDCounter는 proc_pidinfo의 fd 목록 길이로 열린 fd 수를 센다. 다른 사용자의 process는 root가 아니면
// 목록이 비어 0으로 남는다.
func newTopProcessFDCounter() func([]topProcess) {
	var buffer []byte
	return func(processes []topProcess) {
		for index := range processes {
			buffer = darwinListFDs(int32(processes[index].PID), buffer)
			processes[index].FDs = len(buffer) / darwinProcFDInfoSize
		}
	}
}

// darwinPIDFirst와 darwinPIDLast는 macOS kernel이 배정하는 PID의 범위다. 99999를 넘으면 100부터 다시 배정한다.
const (
	darwinPIDFirst = 100
	darwinPIDLast  = 99999
)

// darwinSpawnTracker는 macOS에 없는 process 생성 counter를 PID로 근사한다. 직전 sample에 없던 PID는 그 사이 fork로
// 배정된 것이므로, 그 가운데 직전 최신 PID에서 가장 먼 거리가 그동안 배정된 PID 수다. 그 사이 만들어졌다 끝난 process만
// 있으면 다음에 살아남은 process가 보일 때 한꺼번에 센다. 첫 sample의 가장 큰 PID는 PID가 한 바퀴 돈 뒤라면 오래된
// process라서, 두 번째 sample에 새로 보인 PID로 최신 PID를 정한 뒤부터 센다.
type darwinSpawnTracker struct {
	mutex    sync.Mutex
	previous map[int32]bool
	newest   int32
	ready    bool
	total    uint64
}

func (tracker *darwinSpawnTracker) observe(pids []int32) (total uint64, ready bool) {
	tracker.mutex.Lock()
	defer tracker.mutex.Unlock()
	current := make(map[int32]bool, len(pids))
	var fresh []int32
	for _, pid := range pids {
		current[pid] = true
		if tracker.previous != nil && !tracker.previous[pid] && pid >= darwinPIDFirst && pid <= darwinPIDLast {
			fresh = append(fresh, pid)
		}
	}
	switch {
	case tracker.previous == nil:
	case !tracker.ready:
		if newest, ok := darwinNewestPID(fresh); ok {
			tracker.newest, tracker.ready = newest, true
		}
	default:
		farthest, newest := 0, tracker.newest
		for _, pid := range fresh {
			if distance := darwinPIDDistance(tracker.newest, pid); distance > farthest {
				farthest, newest = distance, pid
			}
		}
		tracker.newest, tracker.total = newest, tracker.total+uint64(farthest)
	}
	tracker.previous = current
	return tracker.total, tracker.ready
}

// darwinPIDDistance는 from 다음부터 to까지 배정된 PID 수다. 99999 다음은 100이다.
func darwinPIDDistance(from, to int32) int {
	const cycle = darwinPIDLast - darwinPIDFirst + 1
	return ((int(to)-int(from))%cycle + cycle) % cycle
}

// darwinNewestPID는 한 구간에 새로 보인 PID 가운데 가장 늦게 배정된 것이다. 새 PID는 원 위의 한 구간에 몰리므로 가장 큰
// 빈틈 바로 앞이 끝이다. PID가 99999에서 100으로 돌아간 구간도 같다.
func darwinNewestPID(fresh []int32) (int32, bool) {
	if len(fresh) == 0 {
		return 0, false
	}
	sorted := slices.Clone(fresh)
	slices.Sort(sorted)
	newest, widest := sorted[len(sorted)-1], darwinPIDDistance(sorted[len(sorted)-1], sorted[0])
	for index := 0; index+1 < len(sorted); index++ {
		if gap := darwinPIDDistance(sorted[index], sorted[index+1]); gap > widest {
			newest, widest = sorted[index], gap
		}
	}
	return newest, true
}

var darwinSpawns darwinSpawnTracker

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
		network []netInterfaceCounters
		disk    darwinDisk
		memory  darwinMemory
		load    float64
		// networkOK와 diskOK가 false면 counter가 0이므로 다음 rate의 기준으로 쓰면 안 된다.
		networkOK, diskOK bool
		pressure          topMemoryPressure
		pressureErr       error
	)
	group.Add(5)
	go func() { defer group.Done(); network, networkOK = readDarwinNetwork() }()
	go func() { defer group.Done(); disk, diskOK = readDarwinDisk() }()
	go func() { defer group.Done(); memory = readDarwinMemory() }()
	go func() { defer group.Done(); load = readDarwinLoad() }()
	go func() { defer group.Done(); pressure, pressureErr = readDarwinMemoryPressure() }()
	group.Wait()

	snapshot.NetMissing, snapshot.DiskMissing = !networkOK, !diskOK
	snapshot.NetInterfaces, snapshot.NetHealthValid = network, networkOK
	snapshot.DiskRead, snapshot.DiskWrite = disk.read, disk.write
	snapshot.DiskOps, snapshot.DiskWaitMS, snapshot.DiskHealthValid = disk.operations, disk.waitNS/uint64(time.Millisecond), diskOK
	snapshot.MemoryTotal, snapshot.MemoryUsed = memory.total, memory.used
	snapshot.SwapOutBytes, snapshot.SwapMissing = memory.swapOut, !memory.swapOK
	snapshot.SwapUsed, snapshot.SwapTotal, snapshot.SwapUsageValid = memory.swapUsed, memory.swapTotal, memory.swapUsageOK
	snapshot.Compressor, snapshot.CompressorValid = memory.compressor, memory.compressorOK
	if pids, err := darwinListAllPIDs(); err == nil {
		total, ready := darwinSpawns.observe(pids)
		snapshot.ProcsCreated, snapshot.ProcsCreatedValid, snapshot.ProcsCreatedSource = total, ready, "pid_delta"
	}
	snapshot.Load1 = load
	if pressureErr == nil {
		snapshot.MemoryPressure = pressure
	}
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

// topPhysicalSupported는 --interface physical을 판정할 수 있는지다. macOS에는 Linux의 /sys/class/net/<이름>/device 같은
// 구분이 없다.
const topPhysicalSupported = false

func topInterfacePhysical(string) bool { return false }

// readDarwinNetwork의 bool은 counter를 읽었는지다.
func readDarwinNetwork() ([]netInterfaceCounters, bool) {
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
	memory := darwinMemoryFromStatistics(stats, pageSize, darwinMemoryTotal())
	if swap, err := readDarwinSwapUsage(); err == nil {
		memory.swapUsed, memory.swapTotal, memory.swapUsageOK = swap.Used, swap.Total, true
	}
	return memory
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
	memory := darwinMemory{swapOut: stats.Swapouts * pageSize, swapOK: true, compressor: uint64(stats.CompressorPageCount) * pageSize, compressorOK: true}
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

func collectInfoMemory() (infoMemory, error) {
	stats, err := readDarwinVMStatistics()
	if err != nil {
		return infoMemory{}, err
	}
	pageSize, err := darwinPageSize()
	if err != nil {
		return infoMemory{}, err
	}
	total, err := readDarwinMemorySize()
	if err != nil {
		return infoMemory{}, err
	}
	return infoMemoryFromDarwin(stats, pageSize, total)
}

func infoMemoryFromDarwin(stats darwinVMStatistics64, pageSize, total uint64) (infoMemory, error) {
	available := (uint64(stats.FreeCount) + uint64(stats.InactiveCount)) * pageSize
	if total == 0 || pageSize == 0 || available > total {
		return infoMemory{}, fmt.Errorf("invalid Mach memory counters")
	}
	return infoMemory{Total: total, Available: available, Basis: "used = total - (free + inactive pages); availability estimate, not pressure",
		Details: []infoMemoryDetail{
			{"File-backed (non-swap)", uint64(stats.ExternalPageCount) * pageSize},
			{"Wired", uint64(stats.WireCount) * pageSize},
			{"Compressed physical", uint64(stats.CompressorPageCount) * pageSize},
			{"Compressed original", stats.TotalUncompressedPagesInCompressor * pageSize},
		}}, nil
}

func collectInfoProcesses() ([]topProcess, error) {
	processes, valid := newTopProcessReaderWithRusage(false)()
	if !valid {
		return nil, fmt.Errorf("process list collection failed")
	}
	return processes, nil
}

func collectInfoCapabilities() []infoCapability {
	ioSupport := infoCapability{"Process I/O", "available for current process", "libproc; access to other PIDs can differ"}
	if _, err := readDarwinProcessRusage(os.Getpid()); err != nil {
		ioSupport.State, ioSupport.Detail = "unavailable", err.Error()
	}
	return []infoCapability{ioSupport,
		{"PSI", "unsupported", "Linux-only; edc top shows the macOS memory pressure level"},
		darwinDetailCapability(),
	}
}

// darwinDetailCapability는 edc top -d가 macOS에서 세는 범위다. CPU 대기만 libproc으로 세고 I/O 지연은 세지 못한다.
func darwinDetailCapability() infoCapability {
	if darwinTranslated() {
		return infoCapability{"CPU wait / I/O latency", "unsupported", "edc top -d stops under Rosetta; use the arm64 build"}
	}
	return infoCapability{"CPU wait / I/O latency", "CPU wait only", "edc top -d reads libproc; I/O latency needs Linux eBPF"}
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
func collectDisks() ([]diskDetails, error) {
	disks, err := collectDisksFromDF()
	if err != nil {
		return nil, err
	}
	// getfsstat가 실패하면 종류 없이 둔다. info는 종류를 모르면 장치 이름이 같은 마운트만 한 줄로 접는다.
	if mounts, err := duDarwinMounts(); err == nil {
		types := make(map[string]string, len(mounts))
		for _, mount := range mounts {
			types[mount.Point] = mount.FSType
		}
		for index := range disks {
			disks[index].FSType = types[disks[index].Mount]
		}
	}
	return disks, nil
}
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
