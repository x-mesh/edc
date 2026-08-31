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

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now()}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return snapshot, err
	}
	fields := strings.Fields(strings.SplitN(string(stat), "\n", 2)[0])
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
			snapshot.NetOutBytes += parseUint(parts[9])
			snapshot.PacketsOut += parseUint(parts[10])
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
		}
	}
	return snapshot, nil
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
