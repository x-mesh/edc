//go:build linux

package edc

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Linux reports ru_maxrss in KiB.
func rusageMaxRSSBytes(usage *syscall.Rusage) uint64 {
	if usage.Maxrss < 0 {
		return 0
	}
	return uint64(usage.Maxrss) * 1024
}

type linuxLogProcess struct {
	stat       linuxProcessStat
	ppid, pgrp int
}

// parseLinuxProcessParents reads ppid and pgrp, the two fields after state in
// /proc/<pid>/stat.
func parseLinuxProcessParents(data string) (ppid, pgrp int, ok bool) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) < 3 {
		return 0, 0, false
	}
	ppid, e1 := strconv.Atoi(fields[1])
	pgrp, e2 := strconv.Atoi(fields[2])
	return ppid, pgrp, e1 == nil && e2 == nil
}

func readLinuxLogProcess(pid int) (linuxLogProcess, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return linuxLogProcess{}, false
	}
	stat, ok := parseLinuxProcessStat(pid, string(data))
	ppid, pgrp, parents := parseLinuxProcessParents(string(data))
	return linuxLogProcess{stat: stat, ppid: ppid, pgrp: pgrp}, ok && parents
}

func readLinuxLogProcesses(pids []int) []linuxLogProcess {
	processes := make([]linuxLogProcess, 0, len(pids))
	for _, pid := range pids {
		if process, ok := readLinuxLogProcess(pid); ok {
			processes = append(processes, process)
		}
	}
	return processes
}

func listLinuxPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// linuxLogMember reads what a member contributes. Memory is RSS plus swap, as in
// top. A process of another user hides its io file unless edc runs as root.
func linuxLogMember(process linuxLogProcess) (logProcessMember, logResourceReading) {
	base := filepath.Join("/proc", strconv.Itoa(process.stat.PID))
	reading := logResourceReading{
		cpu:      time.Duration(process.stat.Ticks) * time.Second / linuxClockTicks,
		memory:   process.stat.RSSPages * uint64(os.Getpagesize()),
		cpuValid: true,
	}
	if data, err := os.ReadFile(filepath.Join(base, "status")); err == nil {
		if swap, ok := parseLinuxProcessSwap(string(data)); ok {
			reading.memory += swap
		}
	}
	if data, err := os.ReadFile(filepath.Join(base, "io")); err == nil {
		if io, ok := parseLinuxProcessIO(string(data)); ok {
			reading.diskRead, reading.diskWrite, reading.diskValid = io.read, io.write, true
		}
	}
	return logProcessMember{pid: process.stat.PID, start: process.stat.StartTicks}, reading
}

func linuxLogMembers(processes []linuxLogProcess, keep func(linuxLogProcess) bool) map[logProcessMember]logResourceReading {
	members := map[logProcessMember]logResourceReading{}
	for _, process := range processes {
		if keep(process) {
			member, reading := linuxLogMember(process)
			members[member] = reading
		}
	}
	return members
}

func newLogResourceProbe(command []string, pid int, started time.Time) logResourceProbe {
	totals := &logMemberTotals{}
	group := &logGroupProbe{
		group: newLogResourceAggregate(started, true),
		readGroup: func() (logResourceReading, bool) {
			members := linuxLogMembers(readLinuxLogProcesses(listLinuxPIDs()), func(process linuxLogProcess) bool { return process.pgrp == pid })
			return totals.reading(members), true
		},
	}
	docker, ok := parseLogDockerCommand(command)
	if !ok {
		return group
	}
	probe := &linuxDockerProbe{logGroupProbe: group, command: docker, started: started, target: newLogResourceAggregate(started, true)}
	probe.startTicks, probe.startKnown = readLinuxUptimeTicks()
	if docker.kind == "run" {
		probe.known = linuxDockerCgroups()
	}
	return probe
}

type linuxDockerProbe struct {
	*logGroupProbe
	command logDockerCommand
	started time.Time
	// startTicks is the uptime when the probe began, in the clock ticks of
	// /proc/<pid>/stat. Comparing ticks avoids btime, which is truncated to a
	// second and drifts from uptime as the wall clock is adjusted.
	startTicks uint64
	startKnown bool
	known      map[string]string
	id, dir    string
	resolved   bool
	// ambiguous is set when several containers start during a docker run, so
	// none of them can be attributed to it.
	ambiguous bool
	target    *logResourceAggregate
	whole     *logResourceAggregate
	tree      logMemberTotals
	root      *logProcessMember
}

// linuxDockerCgroups maps container IDs to their cgroup directories. The systemd
// driver uses docker-<id>.scope; the cgroupfs driver uses docker/<id>.
func linuxDockerCgroups() map[string]string {
	directories := map[string]string{}
	for _, pattern := range []string{filepath.Join(linuxCgroupDefaultMount, "system.slice", "docker-*.scope"), filepath.Join(linuxCgroupDefaultMount, "docker", "*")} {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(match), "docker-"), ".scope")
			if validHistoryKey(id) {
				directories[id] = match
			}
		}
	}
	return directories
}

func readLinuxLogCgroup(directory string, withPeak bool) (logResourceReading, bool) {
	memory, err := readLinuxCgroupNumber(filepath.Join(directory, "memory.current"))
	if err != nil {
		return logResourceReading{}, false
	}
	reading := logResourceReading{memory: memory}
	if counters, status := readLinuxCgroupCounters(directory, "cpu.stat", "usage_usec"); status.Status == "available" {
		reading.cpu, reading.cpuValid = time.Duration(counters["usage_usec"])*time.Microsecond, true
	}
	if data, err := os.ReadFile(filepath.Join(directory, "io.stat")); err == nil {
		reading.diskRead, reading.diskWrite, reading.diskValid = sumCgroupIOStat(string(data))
	}
	if withPeak {
		reading.peak, _ = readLinuxCgroupNumber(filepath.Join(directory, "memory.peak"))
	}
	return reading, true
}

func readLinuxUptimeTicks() (uint64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return uint64(seconds * linuxClockTicks), true
}

func readLinuxCgroupNumber(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func (probe *linuxDockerProbe) sample(at time.Time) {
	probe.logGroupProbe.sample(at)
	if probe.command.kind == "run" {
		probe.sampleRun(at)
	} else {
		probe.sampleExec(at)
	}
}

func (probe *linuxDockerProbe) sampleRun(at time.Time) {
	if probe.ambiguous {
		return
	}
	if probe.dir == "" {
		var fresh []string
		current := linuxDockerCgroups()
		for id := range current {
			if _, seen := probe.known[id]; !seen {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 1 {
			probe.ambiguous = true
			return
		}
		if len(fresh) == 0 {
			return
		}
		probe.id, probe.dir = fresh[0], current[fresh[0]]
	}
	// The container starts during the run, so its memory.peak covers only it.
	if reading, ok := readLinuxLogCgroup(probe.dir, true); ok {
		probe.target.add(reading, at)
	}
}

func (probe *linuxDockerProbe) resolveExec() {
	probe.resolved = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := commandOutput(ctx, "docker", "inspect", "--type", "container", "--format", "{{.State.Pid}}|{{.Id}}", "--", probe.command.container)
	if err != nil {
		return
	}
	pidText, id, _ := strings.Cut(output, "|")
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid < 1 || !validHistoryKey(id) {
		return
	}
	path, err := linuxCtrCgroupPath(pid)
	if err != nil {
		return
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return
	}
	directory, err := resolveLinuxCgroup(path, parseLinuxCgroupMounts(string(data)))
	if err != nil {
		return
	}
	probe.id, probe.dir = id, directory
	probe.whole = newLogResourceAggregate(probe.started, false)
}

// sampleExec follows the process docker exec started inside the container: a
// member of the container cgroup that began after the attempt and runs the
// given argv, plus its descendants.
func (probe *linuxDockerProbe) sampleExec(at time.Time) {
	if !probe.resolved {
		probe.resolveExec()
	}
	if probe.dir == "" {
		return
	}
	if reading, ok := readLinuxLogCgroup(probe.dir, false); ok {
		probe.whole.add(reading, at)
	}
	data, err := os.ReadFile(filepath.Join(probe.dir, "cgroup.procs"))
	if err != nil {
		return
	}
	var pids []int
	for _, field := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	processes := readLinuxLogProcesses(pids)
	if probe.root == nil {
		probe.root = probe.findExecRoot(processes)
		if probe.root == nil {
			return
		}
	}
	included := map[int]bool{}
	for changed := true; changed; {
		changed = false
		for _, process := range processes {
			member := logProcessMember{pid: process.stat.PID, start: process.stat.StartTicks}
			if !included[process.stat.PID] && (member == *probe.root || included[process.ppid]) {
				included[process.stat.PID], changed = true, true
			}
		}
	}
	members := linuxLogMembers(processes, func(process linuxLogProcess) bool { return included[process.stat.PID] })
	probe.target.add(probe.tree.reading(members), at)
}

// findExecRoot lists the processes that entered the container through exec:
// started after the probe, with a parent outside the container cgroup (runc and
// the shim live outside it).
func (probe *linuxDockerProbe) findExecRoot(processes []linuxLogProcess) *logProcessMember {
	// The exec process starts after the docker client, which starts before the
	// probe; half a second covers the probe starting late.
	const slack = linuxClockTicks / 2
	if !probe.startKnown {
		return nil
	}
	inside := map[int]bool{}
	for _, process := range processes {
		inside[process.stat.PID] = true
	}
	var candidates []logExecCandidate
	for _, process := range processes {
		if process.stat.StartTicks+slack < probe.startTicks || inside[process.ppid] {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(process.stat.PID), "cmdline"))
		if err != nil {
			continue
		}
		candidates = append(candidates, logExecCandidate{member: logProcessMember{pid: process.stat.PID, start: process.stat.StartTicks}, cmdline: strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")})
	}
	root, ok := pickLogExecRoot(candidates, probe.command.argv)
	if !ok {
		return nil
	}
	return &root
}

// finish records only the container target. The process group and the rusage
// describe the docker client, so a run too short to sample records nothing
// rather than the client's numbers under the command's name.
func (probe *linuxDockerProbe) finish(resources *logResources, _ *os.ProcessState) {
	short := probe.id
	if len(short) > 12 {
		short = short[:12]
	}
	switch {
	case probe.command.kind == "run" && probe.target.samples > 0:
		resources.addAggregate(probe.target, logResourceScopeContainer, short)
	case probe.command.kind == "exec" && probe.target.samples > 0:
		resources.addAggregate(probe.target, logResourceScopeExec, short)
	case probe.whole != nil && probe.whole.samples > 0:
		resources.addAggregate(probe.whole, logResourceScopeContainer, short)
	}
}
