package edc

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	logResourceFirstSample = 200 * time.Millisecond
	logResourceInterval    = time.Second
)

const (
	logResourceScopeGroup     = "pgrp"
	logResourceScopeContainer = "container"
	logResourceScopeExec      = "exec"
)

// logResourceReading is one look at a target. CPU and disk are cumulative so a
// rate can come from two readings; memory is the current total.
type logResourceReading struct {
	cpu                 time.Duration
	memory              uint64
	diskRead, diskWrite uint64
	cpuValid, diskValid bool
	// peak is the kernel's own high-water mark when the target has one.
	peak uint64
}

type logResourceAggregate struct {
	samples    int
	memorySum  float64
	memoryPeak uint64
	cpuPeak    float64
	base, last logResourceReading
	lastAt     time.Time
	based      bool
}

// newLogResourceAggregate starts from zero at the attempt start when every
// process of the target begins after it. A target that existed before, such as
// a whole container, starts from its first reading instead.
func newLogResourceAggregate(started time.Time, zeroBase bool) *logResourceAggregate {
	aggregate := &logResourceAggregate{}
	if zeroBase {
		aggregate.base = logResourceReading{cpuValid: true, diskValid: true}
		aggregate.last, aggregate.lastAt, aggregate.based = aggregate.base, started, true
	}
	return aggregate
}

func (aggregate *logResourceAggregate) add(reading logResourceReading, at time.Time) {
	if !aggregate.based {
		aggregate.base, aggregate.last, aggregate.lastAt, aggregate.based = reading, reading, at, true
	} else if elapsed := at.Sub(aggregate.lastAt); elapsed > 0 && reading.cpuValid && aggregate.last.cpuValid && reading.cpu >= aggregate.last.cpu {
		if percent := float64(reading.cpu-aggregate.last.cpu) / float64(elapsed) * 100; percent > aggregate.cpuPeak {
			aggregate.cpuPeak = percent
		}
	}
	aggregate.samples++
	aggregate.memorySum += float64(reading.memory)
	for _, memory := range []uint64{reading.memory, reading.peak} {
		if memory > aggregate.memoryPeak {
			aggregate.memoryPeak = memory
		}
	}
	aggregate.last, aggregate.lastAt = reading, at
}

type logResources struct {
	CPUUser, CPUSys     time.Duration
	MaxRSS              uint64
	rusage              bool
	Scope, Container    string
	Samples             int
	CPUTime             time.Duration
	MemPeak, MemAvg     uint64
	CPUPeak             float64
	DiskRead, DiskWrite uint64
	cpuTime, disk       bool
}

func (resources *logResources) addRusage(state *os.ProcessState) {
	if state == nil {
		return
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage == nil {
		return
	}
	resources.CPUUser = time.Duration(usage.Utime.Nano())
	resources.CPUSys = time.Duration(usage.Stime.Nano())
	resources.MaxRSS = rusageMaxRSSBytes(usage)
	resources.rusage = true
}

func (resources *logResources) addAggregate(aggregate *logResourceAggregate, scope, container string) {
	if aggregate == nil || aggregate.samples == 0 {
		return
	}
	resources.Scope, resources.Container, resources.Samples = scope, container, aggregate.samples
	resources.MemPeak = aggregate.memoryPeak
	resources.MemAvg = uint64(aggregate.memorySum / float64(aggregate.samples))
	resources.CPUPeak = aggregate.cpuPeak
	last, base := aggregate.last, aggregate.base
	if last.cpuValid && base.cpuValid && last.cpu >= base.cpu {
		resources.CPUTime, resources.cpuTime = last.cpu-base.cpu, true
	}
	if last.diskValid && base.diskValid && last.diskRead >= base.diskRead && last.diskWrite >= base.diskWrite {
		resources.DiskRead, resources.DiskWrite, resources.disk = last.diskRead-base.diskRead, last.diskWrite-base.diskWrite, true
	}
}

// fields is appended to the end marker. Readers ignore end fields they do not
// know, so logs with these fields stay readable by older edc versions. Times are
// integer microseconds because a Duration string can contain "µ" and markers
// stay ASCII.
func (resources logResources) fields() string {
	text := ""
	if resources.rusage {
		text += fmt.Sprintf(" cpu_user_us=%d cpu_sys_us=%d max_rss=%d", resources.CPUUser.Microseconds(), resources.CPUSys.Microseconds(), resources.MaxRSS)
	}
	if resources.Samples > 0 {
		text += fmt.Sprintf(" scope=%s samples=%d mem_peak=%d mem_avg=%d cpu_peak=%s", resources.Scope, resources.Samples, resources.MemPeak, resources.MemAvg, strconv.FormatFloat(resources.CPUPeak, 'f', 1, 64))
		if resources.cpuTime && !resources.rusage {
			text += fmt.Sprintf(" cpu_time_us=%d", resources.CPUTime.Microseconds())
		}
		if resources.disk {
			text += fmt.Sprintf(" disk_read=%d disk_write=%d", resources.DiskRead, resources.DiskWrite)
		}
		if resources.Container != "" {
			text += " container=" + resources.Container
		}
	}
	return text
}

// logResourceProbe reads the attempt's targets once. The process group is
// always read; a container target appears once docker detection finds it.
type logResourceProbe interface {
	sample(at time.Time)
	finish(resources *logResources, state *os.ProcessState)
}

type logResourceSampler struct {
	probe logResourceProbe
	stop  chan struct{}
	done  sync.WaitGroup
}

func startLogResourceSampler(probe logResourceProbe, first, interval time.Duration) *logResourceSampler {
	sampler := &logResourceSampler{probe: probe, stop: make(chan struct{})}
	sampler.done.Add(1)
	go func() {
		defer sampler.done.Done()
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-sampler.stop:
				return
			case at := <-timer.C:
				probe.sample(at)
				timer.Reset(interval)
			}
		}
	}()
	return sampler
}

// finish waits for a sample in progress before the probe is read, because the
// probe's state belongs to the sampling goroutine until then.
func (sampler *logResourceSampler) finish(state *os.ProcessState) logResources {
	close(sampler.stop)
	sampler.done.Wait()
	var resources logResources
	sampler.probe.finish(&resources, state)
	return resources
}

// logGroupProbe measures the child's process group. It is the target for every
// command that docker detection does not claim.
type logGroupProbe struct {
	group     *logResourceAggregate
	readGroup func() (logResourceReading, bool)
}

func (probe *logGroupProbe) sample(at time.Time) {
	if reading, ok := probe.readGroup(); ok {
		probe.group.add(reading, at)
	}
}

func (probe *logGroupProbe) finish(resources *logResources, state *os.ProcessState) {
	resources.addRusage(state)
	resources.addAggregate(probe.group, logResourceScopeGroup, "")
}

// logProcessMember keys a process by pid and start time, because a pid can be
// reused while the attempt runs.
type logProcessMember struct {
	pid   int
	start uint64
}

// logMemberTotals keeps the last cumulative CPU and disk of every member seen.
// Their sum stays monotonic when members exit between samples.
type logMemberTotals struct {
	last map[logProcessMember]logResourceReading
}

func (totals *logMemberTotals) reading(current map[logProcessMember]logResourceReading) logResourceReading {
	if totals.last == nil {
		totals.last = map[logProcessMember]logResourceReading{}
	}
	reading := logResourceReading{cpuValid: true, diskValid: true}
	for member, value := range current {
		totals.last[member] = value
		reading.memory += value.memory
	}
	for _, value := range totals.last {
		reading.cpu += value.cpu
		reading.diskRead += value.diskRead
		reading.diskWrite += value.diskWrite
		reading.cpuValid = reading.cpuValid && value.cpuValid
		reading.diskValid = reading.diskValid && value.diskValid
	}
	return reading
}

type logDockerCommand struct {
	kind, container string
	argv            []string
}

// parseLogDockerCommand recognizes `docker run` and `docker exec`. The process
// they start belongs to dockerd, not to edc, so the process group would hold
// only the docker client.
func parseLogDockerCommand(command []string) (logDockerCommand, bool) {
	if len(command) < 2 || filepath.Base(command[0]) != "docker" {
		return logDockerCommand{}, false
	}
	globalValues := map[string]bool{"-H": true, "--host": true, "-c": true, "--context": true, "--config": true, "-l": true, "--log-level": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true}
	args := command[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		if globalValues[args[0]] {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "container" {
		args = args[1:]
	}
	if len(args) == 0 {
		return logDockerCommand{}, false
	}
	switch args[0] {
	case "run":
		return logDockerCommand{kind: "run"}, true
	case "exec":
	default:
		return logDockerCommand{}, false
	}
	execValues := map[string]bool{"-e": true, "--env": true, "--env-file": true, "-u": true, "--user": true, "-w": true, "--workdir": true, "--detach-keys": true}
	args = args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		if execValues[args[0]] {
			args = args[1:]
		}
		args = args[1:]
	}
	if len(args) < 2 {
		return logDockerCommand{}, false
	}
	return logDockerCommand{kind: "exec", container: args[0], argv: args[1:]}, true
}

// logCommandLineMatches compares a process argv with the argv given to docker
// exec. The first word may be resolved through PATH inside the container.
func logCommandLineMatches(cmdline, argv []string) bool {
	if len(cmdline) != len(argv) || len(argv) == 0 || filepath.Base(cmdline[0]) != filepath.Base(argv[0]) {
		return false
	}
	return slices.Equal(cmdline[1:], argv[1:])
}

type logExecCandidate struct {
	member  logProcessMember
	cmdline []string
}

// pickLogExecRoot chooses the exec process among the processes that entered a
// container during the run. An exact argv wins; a program that rewrites its argv,
// as busybox dd does, still keeps its first word; a lone candidate is accepted
// last. Among equals the earliest start wins.
func pickLogExecRoot(candidates []logExecCandidate, argv []string) (logProcessMember, bool) {
	sorted := slices.Clone(candidates)
	slices.SortFunc(sorted, func(a, b logExecCandidate) int { return cmp.Compare(a.member.start, b.member.start) })
	for _, matches := range []func(logExecCandidate) bool{
		func(candidate logExecCandidate) bool { return logCommandLineMatches(candidate.cmdline, argv) },
		func(candidate logExecCandidate) bool {
			return len(candidate.cmdline) > 0 && len(argv) > 0 && filepath.Base(candidate.cmdline[0]) == filepath.Base(argv[0])
		},
	} {
		for _, candidate := range sorted {
			if matches(candidate) {
				return candidate.member, true
			}
		}
	}
	if len(sorted) == 1 {
		return sorted[0].member, true
	}
	return logProcessMember{}, false
}

// sumCgroupIOStat adds the per-device rbytes and wbytes of a cgroup io.stat.
func sumCgroupIOStat(data string) (read, write uint64, ok bool) {
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, field := range fields[1:] {
			key, value, found := strings.Cut(field, "=")
			if !found || key != "rbytes" && key != "wbytes" {
				continue
			}
			number, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			if key == "rbytes" {
				read += number
			} else {
				write += number
			}
		}
	}
	return read, write, true
}
