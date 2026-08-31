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
	"time"
)

func collectResourceSnapshot() (resourceSnapshot, error) {
	snapshot := resourceSnapshot{TakenAt: time.Now(), CPUInstant: true}
	output, err := exec.Command("/usr/bin/top", "-l", "1", "-n", "0").Output()
	if err != nil {
		return snapshot, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "CPU usage:") {
			var user, system, idle float64
			fmt.Sscanf(line, "CPU usage: %f%% user, %f%% sys, %f%% idle", &user, &system, &idle)
			snapshot.CPUUser, snapshot.CPUSystem, snapshot.CPUIdle = uint64(user*100), uint64(system*100), uint64(idle*100)
			snapshot.CPUTotal = 10000
		}
		if strings.HasPrefix(line, "Load Avg:") {
			fmt.Sscanf(line, "Load Avg: %f,", &snapshot.Load1)
		}
		if strings.HasPrefix(line, "PhysMem:") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				snapshot.MemoryUsed = parseHumanBytes(fields[1])
				snapshot.MemoryTotal = snapshot.MemoryUsed + parseHumanBytes(fields[len(fields)-2])
			}
		}
		if strings.HasPrefix(line, "Networks:") {
			var inPackets, outPackets uint64
			var inBytes, outBytes string
			fmt.Sscanf(line, "Networks: packets: %d/%s in, %d/%s out.", &inPackets, &inBytes, &outPackets, &outBytes)
			snapshot.PacketsIn, snapshot.PacketsOut = inPackets, outPackets
			snapshot.NetInBytes, snapshot.NetOutBytes = parseHumanBytes(inBytes), parseHumanBytes(outBytes)
		}
		if strings.HasPrefix(line, "Disks:") {
			var reads, writes uint64
			var readBytes, writeBytes string
			fmt.Sscanf(line, "Disks: %d/%s read, %d/%s written.", &reads, &readBytes, &writes, &writeBytes)
			snapshot.DiskRead, snapshot.DiskWrite = parseHumanBytes(readBytes), parseHumanBytes(writeBytes)
		}
	}
	if network, err := exec.Command("/usr/sbin/netstat", "-ibn").Output(); err == nil {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(network), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[2] == "Network" || !strings.HasPrefix(fields[2], "<Link#") || fields[0] == "lo0" || strings.HasSuffix(fields[0], "*") || seen[fields[0]] {
				continue
			}
			seen[fields[0]] = true
			snapshot.PacketsIn += parseUnsigned(fields[4])
			snapshot.NetInBytes += parseUnsigned(fields[6])
			snapshot.PacketsOut += parseUnsigned(fields[7])
			snapshot.NetOutBytes += parseUnsigned(fields[9])
		}
	}
	if storage, err := exec.Command("/usr/sbin/ioreg", "-r", "-c", "IOBlockStorageDriver", "-l").Output(); err == nil {
		if match := diskBytePattern.FindStringSubmatch(string(storage)); match != nil {
			if match[1] != "" {
				snapshot.DiskRead, snapshot.DiskWrite = parseUnsigned(match[1]), parseUnsigned(match[2])
			} else {
				snapshot.DiskWrite, snapshot.DiskRead = parseUnsigned(match[3]), parseUnsigned(match[4])
			}
		}
	}
	return snapshot, nil
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
