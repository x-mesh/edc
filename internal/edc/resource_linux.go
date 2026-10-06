//go:build linux

package edc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
)

// linuxClockTicks는 /proc이 CPU 시간을 세는 단위(USER_HZ)다. kernel HZ와 달리 userspace에는 100으로 고정돼 나온다.
const linuxClockTicks = 100

// newTopProcessReader는 /proc/<pid>/stat의 CPU tick을 직전 읽기와 비교한다. 첫 읽기는 기준점만 만든다.
func newTopProcessReader() func() ([]topProcess, bool) {
	tracker := &topProcessTracker{clockTicks: linuxClockTicks, pageSize: uint64(os.Getpagesize()), boot: readLinuxBootTime()}
	return func() ([]topProcess, bool) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return nil, false
		}
		stats := make([]linuxProcessStat, 0, len(entries))
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			// 목록을 읽은 뒤 끝난 process는 파일이 없다.
			data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err != nil {
				continue
			}
			if stat, ok := parseLinuxProcessStat(pid, string(data)); ok {
				if topFullCommand {
					if argv, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline"); err == nil {
						if line := linuxProcessCommandLine(argv); line != "" {
							stat.Command = line
						}
					}
				}
				stats = append(stats, stat)
			}
		}
		return tracker.update(time.Now(), stats)
	}
}

// readLinuxBootTime은 /proc/stat의 btime(부팅 시각, Unix 초)을 읽는다. 없으면 zero다.
func readLinuxBootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(seconds, 0)
		}
	}
	return time.Time{}
}

// newTopProcessEnricher는 목록에 남은 process의 I/O rate와 fd 수를 채운다. 모든 process가 아니라 남은 것만 읽어
// 필터를 걸지 않을 때 비용이 늘지 않는다. 다른 사용자의 process는 root가 아니면 /proc/<pid>/io와 fd를 읽을 수 없어 비워 둔다.
func newTopProcessEnricher() func([]topProcess) {
	previous := map[int]linuxProcessIOSample{}
	limits := newLinuxProcessLimits("/proc")
	return func(processes []topProcess) {
		now := time.Now()
		limits.enrich(processes, now)
		current := make(map[int]linuxProcessIOSample, len(processes))
		for index := range processes {
			process := &processes[index]
			pid := strconv.Itoa(process.PID)
			data, err := os.ReadFile("/proc/" + pid + "/io")
			if err != nil {
				continue
			}
			io, ok := parseLinuxProcessIO(string(data))
			if !ok {
				continue
			}
			current[process.PID] = linuxProcessIOSample{io: io, at: now}
			before, seen := previous[process.PID]
			seconds := now.Sub(before.at).Seconds()
			if !seen || seconds <= 0 || io.read < before.io.read || io.write < before.io.write {
				continue
			}
			process.DiskValid = true
			process.DiskRead = float64(io.read-before.io.read) / seconds
			process.DiskWrite = float64(io.write-before.io.write) / seconds
		}
		previous = current
	}
}

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now(), NetworkHealth: collectNetworkHealth()}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return snapshot, err
	}
	lines := strings.Split(string(stat), "\n")
	fields := strings.Fields(lines[0])
	if len(fields) < 8 {
		return snapshot, fmt.Errorf("invalid /proc/stat cpu line")
	}
	values := make([]uint64, len(fields)-1)
	for i, field := range fields[1:] {
		values[i], _ = strconv.ParseUint(field, 10, 64)
		snapshot.CPUTotal += values[i]
	}
	snapshot.CPUUser = values[0] + values[1]
	snapshot.CPUSystem = values[2] + values[5] + values[6]
	snapshot.CPUIdle = values[3]
	snapshot.CPUIOWait = values[4]
	if len(values) > 7 {
		snapshot.CPUSteal, snapshot.CPUStealValid = values[7], true
	}
	for _, line := range lines[1:] {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == "procs_blocked" {
			snapshot.ProcsBlocked, snapshot.ProcsBlockedValid = parseUint(parts[1]), true
			continue
		}
		if len(parts) < 5 || !strings.HasPrefix(parts[0], "cpu") || len(parts[0]) == 3 {
			continue
		}
		core := resourceCPU{}
		for _, value := range parts[1:] {
			core.Total += parseUint(value)
		}
		core.Idle = parseUint(parts[4])
		snapshot.Cores = append(snapshot.Cores, core)
	}
	// memory와 io의 full 행은 PSI가 처음 들어간 4.20부터 some과 함께 있다. cpu의 full은 5.13부터 나오지만
	// system 수준에서는 kernel이 항상 0을 쓰므로 읽지 않는다.
	psiCPU, okCPU := readLinuxPressure("/proc/pressure/cpu", "some")
	psiMemory, okMemory := readLinuxPressure("/proc/pressure/memory", "some", "full")
	psiIO, okIO := readLinuxPressure("/proc/pressure/io", "some", "full")
	if okCPU && okMemory && okIO {
		snapshot.PSICPU, snapshot.PSIMemory, snapshot.PSIIO, snapshot.PSIValid = psiCPU[0], psiMemory[0], psiIO[0], true
		snapshot.PSIMemoryFull, snapshot.PSIIOFull = psiMemory[1], psiIO[1]
	}
	if loads, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscan(string(loads), &snapshot.Load1)
	}
	if memory, err := readMemInfo(); err == nil {
		snapshot.MemoryTotal = memory["MemTotal"]
		snapshot.MemoryUsed = snapshot.MemoryTotal - memory["MemAvailable"]
	}
	if vmstat, err := os.ReadFile("/proc/vmstat"); err == nil {
		pages, ok := parseLinuxSwapOutPages(string(vmstat))
		snapshot.SwapOutBytes, snapshot.SwapMissing = pages*uint64(os.Getpagesize()), !ok
	} else {
		snapshot.SwapMissing = true
	}
	if network, err := os.ReadFile("/proc/net/dev"); err == nil {
		for _, line := range strings.Split(string(network), "\n") {
			parts := strings.Fields(strings.Replace(line, ":", " ", 1))
			if len(parts) < 17 || parts[0] == "lo" {
				continue
			}
			snapshot.NetInBytes += parseUint(parts[1])
			snapshot.PacketsIn += parseUint(parts[2])
			snapshot.NetErrors += parseUint(parts[3]) + parseUint(parts[11])
			snapshot.NetDrops += parseUint(parts[4]) + parseUint(parts[12])
			snapshot.NetOutBytes += parseUint(parts[9])
			snapshot.PacketsOut += parseUint(parts[10])
			snapshot.NetHealthValid = true
		}
	} else {
		snapshot.NetMissing = true
	}
	if disks, err := os.ReadFile("/proc/diskstats"); err == nil {
		for _, line := range strings.Split(string(disks), "\n") {
			parts := strings.Fields(line)
			if len(parts) < 14 || !isPhysicalLinuxDisk(parts[2]) {
				continue
			}
			snapshot.DiskRead += parseUint(parts[5]) * 512
			snapshot.DiskWrite += parseUint(parts[9]) * 512
			snapshot.DiskOps += parseUint(parts[3]) + parseUint(parts[7])
			snapshot.DiskWaitMS += parseUint(parts[6]) + parseUint(parts[10])
			snapshot.DiskBusyMS += parseUint(parts[12])
			snapshot.DiskQueueMS += parseUint(parts[13])
			snapshot.DiskHealthValid, snapshot.DiskBusyValid = true, true
		}
	} else {
		snapshot.DiskMissing = true
	}
	return snapshot, nil
}

func readLinuxPressure(path string, kinds ...string) ([]float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	values := make([]float64, len(kinds))
	for index, kind := range kinds {
		value, ok := parsePressureAvg10(string(data), kind)
		if !ok {
			return nil, false
		}
		values[index] = value
	}
	return values, true
}

func collectHostDetails() (hostDetails, error) {
	var details hostDetails
	details.Hostname, _ = os.Hostname()
	details.System = "Linux"
	details.Machine = runtimeMachine()
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		values := parseKeyValues(string(data))
		details.OS = strings.Trim(values["ID"], "\"")
		details.Version = strings.Trim(values["PRETTY_NAME"], "\"")
	}
	if output, err := exec.Command("uname", "-r").Output(); err == nil {
		details.Release = strings.TrimSpace(string(output))
	}
	if output, err := exec.Command("uname", "-v").Output(); err == nil {
		details.Processor = strings.TrimSpace(string(output))
	}
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "model name") {
				_, details.Model, _ = strings.Cut(line, ":")
				details.Model = strings.TrimSpace(details.Model)
				break
			}
		}
	}
	details.Cores = runtime.NumCPU()
	memory, err := readMemInfo()
	if err != nil {
		return details, err
	}
	details.MemoryTotal = memory["MemTotal"]
	details.SwapTotal = memory["SwapTotal"]
	details.SwapUsed = details.SwapTotal - memory["SwapFree"]
	if loads, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscan(string(loads), &details.Load[0], &details.Load[1], &details.Load[2])
	}
	if uptime, err := os.ReadFile("/proc/uptime"); err == nil {
		var seconds float64
		fmt.Sscan(string(uptime), &seconds)
		details.Uptime = time.Duration(seconds * float64(time.Second))
	}
	var limits syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limits) == nil {
		details.RLimitSoft, details.RLimitHard = limits.Cur, limits.Max
	}
	return details, nil
}

func collectInfoMemory() (infoMemory, error) {
	values, err := readMemInfo()
	if err != nil {
		return infoMemory{}, err
	}
	return infoMemoryFromLinux(values)
}

func infoMemoryFromLinux(values map[string]uint64) (infoMemory, error) {
	total, available := values["MemTotal"], values["MemAvailable"]
	if _, found := values["MemAvailable"]; !found || total == 0 || available > total {
		return infoMemory{}, fmt.Errorf("missing or invalid MemTotal/MemAvailable")
	}
	memory := infoMemory{Total: total, Available: available, Basis: "used = MemTotal - MemAvailable; availability estimate, not pressure"}
	for _, field := range []struct{ key, name string }{{"Cached", "Page cache (includes tmpfs/shmem)"}, {"Buffers", "Buffers"}, {"SReclaimable", "Reclaimable slab"}, {"Zswap", "Zswap compressed pool"}} {
		if value, found := values[field.key]; found {
			memory.Details = append(memory.Details, infoMemoryDetail{field.name, value})
		}
	}
	return memory, nil
}

func collectInfoProcesses() ([]topProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []topProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		if stat, valid := parseLinuxProcessStat(pid, string(data)); valid {
			processes = append(processes, topProcess{PID: pid, Command: stat.Command, RSS: stat.RSSPages * uint64(os.Getpagesize()), Threads: stat.Threads})
		}
	}
	if len(processes) == 0 {
		return nil, fmt.Errorf("no readable process statistics")
	}
	return processes, nil
}

func collectInfoCapabilities() []infoCapability {
	ioSupport := infoCapability{"Process I/O", "available for current process", "/proc; access to other PIDs can differ"}
	if data, err := os.ReadFile("/proc/self/io"); err != nil {
		ioSupport.State, ioSupport.Detail = "unavailable", err.Error()
	} else if _, valid := parseLinuxProcessIO(string(data)); !valid {
		ioSupport.State, ioSupport.Detail = "unknown", "invalid /proc/self/io counters"
	}
	psi := infoCapability{"PSI", "available", "CPU, memory and I/O some avg10 can be read"}
	for _, resource := range []string{"cpu", "memory", "io"} {
		path := "/proc/pressure/" + resource
		data, err := os.ReadFile(path)
		if err != nil {
			psi.State, psi.Detail = "unavailable", err.Error()
			break
		}
		if _, valid := parsePressureAvg10(string(data), "some"); !valid {
			psi.State, psi.Detail = "unknown", "invalid PSI counters in "+path
			break
		}
	}
	return []infoCapability{ioSupport, psi, infoTopDetailCapability()}
}

func infoTopDetailCapability() infoCapability {
	status := infoCapability{"CPU wait / I/O latency", "prerequisites met", "edc top -d; program attachment not tested"}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		status.State, status.Detail = "unknown", err.Error()
		if errors.Is(err, ebpf.ErrNotSupported) {
			status.State = "unsupported"
		}
		return status
	}
	if err := topProcessKernelSupported(kernel); err != nil {
		status.State, status.Detail = "unsupported", err.Error()
		return status
	}
	capabilities, err := effectiveCapabilities()
	if err != nil {
		status.State, status.Detail = "unknown", err.Error()
		return status
	}
	if missing := missingCapabilities(bpfTraceCapabilities, capabilities); missing != "" {
		status.State, status.Detail = "permission required", "missing "+missing
	}
	return status
}

func collectDefaultRoute() (string, string) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		value, _ := strconv.ParseUint(fields[2], 16, 32)
		gateway := fmt.Sprintf("%d.%d.%d.%d", byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
		return fields[0], gateway
	}
	return "", ""
}

func collectDisks() ([]diskDetails, error) { return collectDisksFromDF() }

func readMemInfo() (map[string]uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	result := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			result[strings.TrimSuffix(fields[0], ":")] = parseUint(fields[1]) * 1024
		}
	}
	return result, nil
}
func parseUint(value string) uint64 { number, _ := strconv.ParseUint(value, 10, 64); return number }
func isPhysicalLinuxDisk(name string) bool {
	_, err := os.Stat("/sys/block/" + name)
	return err == nil && !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "ram")
}
func parseKeyValues(input string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(input, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}
