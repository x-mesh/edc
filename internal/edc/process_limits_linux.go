//go:build linux

package edc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type linuxCgroupMount struct{ root, path string }

var errLinuxCgroupV1 = errors.New("cgroup v2 membership is not available (cgroup v1 is unsupported)")

type linuxCgroupCPURead struct {
	periods, throttled, usec uint64
	at                       time.Time
}
type linuxProcessLimits struct {
	proc     string
	previous map[string]linuxCgroupCPURead
}

func newLinuxProcessLimits(proc string) *linuxProcessLimits {
	return &linuxProcessLimits{proc: proc, previous: map[string]linuxCgroupCPURead{}}
}

func linuxLimitError(err error, optional bool) topLimitStatus {
	status := "error"
	if errors.Is(err, os.ErrPermission) {
		status = "permission_denied"
	} else if optional && errors.Is(err, os.ErrNotExist) {
		status = "unsupported"
	}
	return topLimitStatus{Status: status, Reason: err.Error()}
}

func (reader *linuxProcessLimits) enrich(processes []topProcess, now time.Time) {
	mountData, mountErr := os.ReadFile(filepath.Join(reader.proc, "self/mountinfo"))
	mounts := parseLinuxCgroupMounts(string(mountData))
	groups := map[string]topCgroupLimits{}
	current := map[string]linuxCgroupCPURead{}
	for index := range processes {
		process := &processes[index]
		base := filepath.Join(reader.proc, strconv.Itoa(process.PID))
		limits := &topProcessLimits{FD: readLinuxFDLimit(base)}
		if limits.FD.Used != nil {
			process.FDs = *limits.FD.Used
		}
		membership, err := os.ReadFile(filepath.Join(base, "cgroup"))
		switch {
		case err != nil:
			limits.Cgroup.topLimitStatus = linuxLimitError(err, false)
		case mountErr != nil:
			limits.Cgroup.topLimitStatus = linuxLimitError(mountErr, false)
		default:
			path, err := linuxUnifiedCgroupPath(string(membership))
			if err != nil {
				status := "unavailable"
				if errors.Is(err, errLinuxCgroupV1) {
					status = "unsupported"
				}
				limits.Cgroup.topLimitStatus = topLimitStatus{Status: status, Reason: err.Error()}
				break
			}
			limits.Cgroup.Path = path
			directory, err := resolveLinuxCgroup(path, mounts)
			if err != nil {
				limits.Cgroup.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: err.Error()}
				break
			}
			if group, ok := groups[directory]; ok {
				limits.Cgroup = group
				break
			}
			group := reader.readCgroup(directory, path, now, current)
			groups[directory] = group
			limits.Cgroup = group
		}
		if limits.Cgroup.Status != "available" {
			limits.Cgroup.Memory.topLimitStatus = limits.Cgroup.topLimitStatus
			limits.Cgroup.Events.topLimitStatus = limits.Cgroup.topLimitStatus
			limits.Cgroup.CPU.topLimitStatus = limits.Cgroup.topLimitStatus
		}
		process.Limits = limits
	}
	reader.previous = current
}

func readLinuxFDLimit(base string) topFDLimit {
	result := topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}}
	entries, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		result.topLimitStatus = linuxLimitError(err, false)
	} else {
		used := len(entries)
		result.Used = &used
	}
	data, err := os.ReadFile(filepath.Join(base, "limits"))
	if err != nil {
		result.topLimitStatus = linuxLimitError(err, false)
		return result
	}
	soft, unlimited, err := parseLinuxFDSoftLimit(string(data))
	if err != nil {
		result.topLimitStatus = topLimitStatus{Status: "error", Reason: err.Error()}
		return result
	}
	result.Soft, result.Unlimited = soft, unlimited
	return result
}

func parseLinuxFDSoftLimit(data string) (*uint64, bool, error) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && strings.Join(fields[:3], " ") == "Max open files" {
			if len(fields) != 6 || fields[5] != "files" {
				return nil, false, fmt.Errorf("invalid Max open files row")
			}
			if fields[3] == "unlimited" {
				return nil, true, nil
			}
			value, err := strconv.ParseUint(fields[3], 10, 64)
			if err != nil {
				return nil, false, fmt.Errorf("invalid FD soft limit: %w", err)
			}
			return &value, false, nil
		}
	}
	return nil, false, fmt.Errorf("Max open files row is missing")
}

func linuxUnifiedCgroupPath(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasSuffix(path, " (deleted)") {
				return "", fmt.Errorf("cgroup v2 path is unavailable: %q", path)
			}
			return path, nil
		}
	}
	return "", errLinuxCgroupV1
}

func parseLinuxCgroupMounts(data string) []linuxCgroupMount {
	var mounts []linuxCgroupMount
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(data, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		fields, filesystem := strings.Fields(before), strings.Fields(after)
		if !ok || len(fields) < 6 || len(filesystem) < 3 || filesystem[0] != "cgroup2" {
			continue
		}
		root, path := unescape.Replace(fields[3]), unescape.Replace(fields[4])
		if filepath.IsAbs(root) && filepath.IsAbs(path) && filepath.Clean(root) == root && filepath.Clean(path) == path {
			mounts = append(mounts, linuxCgroupMount{root: root, path: path})
		}
	}
	return mounts
}

// linuxCgroupDefaultMount는 systemd가 cgroup2를 mount하는 자리다. calico 같은 agent가 같은 계층을 다른 자리에도
// mount하면 mountinfo에서 그쪽이 먼저 나올 수 있다. 값은 같지만 경로와 오류 문구는 사용자가 아는 이 자리를 쓴다.
const linuxCgroupDefaultMount = "/sys/fs/cgroup"

func resolveLinuxCgroup(path string, mounts []linuxCgroupMount) (string, error) {
	var chosen *linuxCgroupMount
	for index := range mounts {
		mount := &mounts[index]
		if path == mount.root || mount.root == "/" || strings.HasPrefix(path, mount.root+"/") {
			if chosen == nil || len(mount.root) > len(chosen.root) ||
				len(mount.root) == len(chosen.root) && mount.path == linuxCgroupDefaultMount && chosen.path != linuxCgroupDefaultMount {
				chosen = mount
			}
		}
	}
	if chosen == nil {
		return "", fmt.Errorf("no visible cgroup v2 mount contains %q", path)
	}
	relative, err := filepath.Rel(chosen.root, path)
	if err != nil {
		return "", err
	}
	return filepath.Join(chosen.path, relative), nil
}

func (reader *linuxProcessLimits) readCgroup(directory, path string, now time.Time, current map[string]linuxCgroupCPURead) topCgroupLimits {
	group := topCgroupLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path}
	info, err := os.Stat(directory)
	if err != nil {
		group.topLimitStatus = linuxLimitError(err, false)
		return group
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		group.topLimitStatus = topLimitStatus{Status: "error", Reason: "invalid cgroup directory identity"}
		return group
	}
	group.Key = fmt.Sprintf("%s:%d:%d", directory, stat.Dev, stat.Ino)
	group.Memory = readLinuxCgroupMemory(directory)
	group.Events = readLinuxCgroupEvents(directory)
	cpu, status := readLinuxCgroupCPU(directory)
	group.CPU.topLimitStatus = status
	if status.Status == "available" {
		cpu.at = now
		current[group.Key] = cpu
		before, seen := reader.previous[group.Key]
		seconds := now.Sub(before.at).Seconds()
		if !seen || seconds <= 0 || cpu.periods < before.periods || cpu.throttled < before.throttled || cpu.usec < before.usec {
			group.CPU.topLimitStatus = topLimitStatus{Status: "baseline", Reason: "waiting for a CPU throttling baseline"}
		} else {
			periods, throttled, usec := cpu.periods-before.periods, cpu.throttled-before.throttled, cpu.usec-before.usec
			group.CPU.WindowS, group.CPU.Periods, group.CPU.ThrottledPeriods, group.CPU.ThrottledUS = &seconds, &periods, &throttled, &usec
		}
	}
	return group
}

func readLinuxCgroupMemory(directory string) topCgroupMemory {
	memory := topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}}
	for _, name := range []string{"memory.current", "memory.max"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			memory.topLimitStatus = linuxLimitError(err, true)
			return memory
		}
		text := strings.TrimSpace(string(data))
		if name == "memory.max" && text == "max" {
			memory.Unlimited = true
			continue
		}
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			memory.topLimitStatus = topLimitStatus{Status: "error", Reason: "invalid " + name}
			return memory
		}
		if name == "memory.current" {
			memory.Used = &value
		} else {
			memory.Max = &value
		}
	}
	return memory
}

func readLinuxCgroupCounters(directory, file string, required ...string) (map[string]uint64, topLimitStatus) {
	data, err := os.ReadFile(filepath.Join(directory, file))
	if err != nil {
		return nil, linuxLimitError(err, true)
	}
	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, topLimitStatus{Status: "error", Reason: "invalid " + file + " row"}
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, topLimitStatus{Status: "error", Reason: "invalid " + file + " counter"}
		}
		if _, exists := values[fields[0]]; exists {
			return nil, topLimitStatus{Status: "error", Reason: "duplicate " + file + " counter"}
		}
		values[fields[0]] = value
	}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return nil, topLimitStatus{Status: "unsupported", Reason: file + " does not provide " + key}
		}
	}
	return values, topLimitStatus{Status: "available"}
}

func readLinuxCgroupEvents(directory string) topCgroupEvents {
	values, status := readLinuxCgroupCounters(directory, "memory.events.local", "oom", "oom_kill")
	events := topCgroupEvents{topLimitStatus: status}
	if status.Status == "available" {
		oom, killed := values["oom"], values["oom_kill"]
		events.OOM, events.OOMKill = &oom, &killed
	}
	return events
}

func readLinuxCgroupCPU(directory string) (linuxCgroupCPURead, topLimitStatus) {
	values, status := readLinuxCgroupCounters(directory, "cpu.stat", "nr_periods", "nr_throttled", "throttled_usec")
	return linuxCgroupCPURead{periods: values["nr_periods"], throttled: values["nr_throttled"], usec: values["throttled_usec"]}, status
}
