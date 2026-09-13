//go:build linux

package edc

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// linuxClockTicks는 /proc이 CPU 시간을 세는 단위(USER_HZ)다. kernel HZ와 달리 userspace에는 100으로 고정돼 나온다.
const linuxClockTicks = 100

// newTopProcessReader는 /proc/<pid>/stat의 CPU tick을 직전 읽기와 비교한다. 첫 읽기는 기준점만 만든다.
func newTopProcessReader() func() ([]topProcess, bool) {
	tracker := &topProcessTracker{clockTicks: linuxClockTicks, pageSize: uint64(os.Getpagesize())}
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
				stats = append(stats, stat)
			}
		}
		return tracker.update(time.Now(), stats)
	}
}

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now()}
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
	for _, line := range lines[1:] {
		parts := strings.Fields(line)
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
	psiCPU, okCPU := readLinuxPressure("/proc/pressure/cpu")
	psiMemory, okMemory := readLinuxPressure("/proc/pressure/memory")
	psiIO, okIO := readLinuxPressure("/proc/pressure/io")
	if okCPU && okMemory && okIO {
		snapshot.PSICPU, snapshot.PSIMemory, snapshot.PSIIO, snapshot.PSIValid = psiCPU, psiMemory, psiIO, true
	}
	if loads, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscan(string(loads), &snapshot.Load1)
	}
	if memory, err := readMemInfo(); err == nil {
		snapshot.MemoryTotal = memory["MemTotal"]
		snapshot.MemoryUsed = snapshot.MemoryTotal - memory["MemAvailable"]
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
			snapshot.DiskHealthValid = true
		}
	}
	return snapshot, nil
}

func readLinuxPressure(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parsePressureAvg10(string(data))
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
	details.PythonVersion = detectPythonVersion()
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
