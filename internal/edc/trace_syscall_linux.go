//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type syscall_key -type syscall_stats -type syscall_start -type syscall_in_flight -type syscall_errno_key -type syscall_comm -type syscall_counters syscallEvents trace_syscall_bpf.c -- -I./bpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const (
	// syscallStatsBudget은 per-CPU stats map이 미리 잡는 메모리의 상한이다. 항목 하나가 CPU 수만큼 값을 가지므로,
	// CPU가 많은 host에서는 항목 수를 줄인다. 넘친 호출은 stats_full로 센다.
	syscallStatsBudget     = 32 << 20
	syscallStatsMinEntries = 1024
	syscallStatsMaxEntries = 16384
	syscallCompatBit       = 0x80000000
	syscallNoNumber        = 0xFFFFFFFF
)

var syscallTracepoints = []string{"sys_enter", "sys_exit"}

// syscallTracePrerequisites는 tracepoint와 함께 task iterator를 본다. 진행 중인 호출은 그 iterator로만 읽을 수 있다.
func syscallTracePrerequisites() error {
	return traceBPFPrerequisites("trace syscall", func(kernel *btf.Spec) error {
		var iterator *btf.Struct
		if err := kernel.TypeByName("bpf_iter__task", &iterator); err != nil {
			return errors.New("kernel BTF has no task iterator")
		}
		return traceTracepointsAvailable(kernel, syscallTracepoints)
	})
}

// syscallStatsEntries는 메모리 상한 안에서 stats map의 항목 수를 정한다.
func syscallStatsEntries(cpus int, valueSize uint32) uint32 {
	if cpus < 1 || valueSize == 0 {
		return syscallStatsMinEntries
	}
	entries := syscallStatsBudget / (cpus * int(valueSize))
	return uint32(min(max(entries, syscallStatsMinEntries), syscallStatsMaxEntries))
}

// syscallExitNumbers는 돌아오지 않는 syscall의 번호다. 32비트 호환 호출은 amd64에만 있다.
func syscallExitNumbers(arch string) (exit, exitGroup, compatExit, compatExitGroup uint32) {
	exit, exitGroup, compatExit, compatExitGroup = syscallNoNumber, syscallNoNumber, syscallNoNumber, syscallNoNumber
	if nr, ok := syscallNumber(arch, "exit"); ok {
		exit = nr
	}
	if nr, ok := syscallNumber(arch, "exit_group"); ok {
		exitGroup = nr
	}
	if arch == "amd64" {
		if nr, ok := syscallNumber("386", "exit"); ok {
			compatExit = nr | syscallCompatBit
		}
		if nr, ok := syscallNumber("386", "exit_group"); ok {
			compatExitGroup = nr | syscallCompatBit
		}
	}
	return exit, exitGroup, compatExit, compatExitGroup
}

func collectSyscallStats(options syscallTraceOptions, stop <-chan struct{}) (syscallCollection, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return syscallCollection{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadSyscallEvents()
	if err != nil {
		return syscallCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	cpus, err := ebpf.PossibleCPU()
	if err != nil {
		return syscallCollection{}, fmt.Errorf("count CPUs: %w", err)
	}
	statsLimit := syscallStatsEntries(cpus, spec.Maps["stats"].ValueSize)
	spec.Maps["stats"].MaxEntries = statsLimit
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	self, selfExcluded := syscallSelfTGID(namespace, os.Getpid())
	exit, exitGroup, compatExit, compatExitGroup := syscallExitNumbers(runtime.GOARCH)
	var variables syscallEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return syscallCollection{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := errors.Join(
		variables.SelfTgid.Set(self),
		variables.FilterProcess.Set(boolByte(len(options.pids)+len(options.comms) > 0)),
		variables.FilterCgroup.Set(boolByte(options.container != nil)),
		variables.KeyCgroup.Set(boolByte(options.groupBy == traceGroupByCgroup)),
		variables.ExitNr.Set(exit),
		variables.ExitGroupNr.Set(exitGroup),
		variables.CompatExitNr.Set(compatExit),
		variables.CompatExitGroupNr.Set(compatExitGroup),
	); err != nil {
		return syscallCollection{}, fmt.Errorf("set eBPF variable: %w", err)
	}
	objects := syscallEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return syscallCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()
	if err := fillSyscallFilters(objects, options); err != nil {
		return syscallCollection{}, err
	}
	// sys_exit를 sys_enter보다 먼저 붙인다. 반대 순서면 그 사이에 돌아온 호출의 시작 시각이 남아 진행 중으로 보인다.
	links := make([]link.Link, 0, 2)
	defer func() { closeCaptureLinks(links) }()
	for _, hook := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"sys_exit", objects.SyscallExit}, {"sys_enter", objects.SyscallEnter},
	} {
		attached, err := link.AttachTracing(link.TracingOptions{Program: hook.program})
		if err != nil {
			return syscallCollection{}, fmt.Errorf("attach tp_btf/%s: %w", hook.name, err)
		}
		links = append(links, attached)
	}
	started := time.Now()
	var deadline <-chan time.Time
	if options.duration > 0 {
		timer := time.NewTimer(options.duration)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-stop:
	case <-deadline:
	}
	// sys_enter를 먼저 떼고 sys_exit를 뗀다. 그 사이에 돌아온 호출은 세고, 그래도 남은 시작 시각만 진행 중이다.
	for index := len(links) - 1; index >= 0; index-- {
		_ = links[index].Close()
	}
	links = nil
	elapsed := time.Since(started)
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		return syscallCollection{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	collection := syscallCollection{summary: syscallSummary{DurationMS: elapsed.Milliseconds(), StatsLimit: statsLimit, SelfExcluded: selfExcluded}}
	errnos, err := readSyscallErrnos(objects.Errnos)
	if err != nil {
		return syscallCollection{}, err
	}
	var key syscallEventsSyscallKey
	var values []syscallEventsSyscallStats
	iterator := objects.Stats.Iterate()
	for iterator.Next(&key, &values) {
		sample := syscallSample{Process: syscallComm(key.Comm), CgroupID: key.CgroupId, Syscall: syscallName(runtime.GOARCH, key.Nr&^syscallCompatBit, key.Nr&syscallCompatBit != 0), Errnos: errnos[key]}
		for _, value := range values {
			sample.Calls += value.Calls
			sample.Timed += value.Timed
			sample.Errors += value.Errors
			sample.SumNS += value.SumNs
			if value.MaxNs > sample.MaxNS {
				sample.MaxNS = value.MaxNs
			}
			for index, count := range value.Hist {
				sample.Hist[index] += count
			}
		}
		collection.samples = append(collection.samples, sample)
	}
	if err := iterator.Err(); err != nil {
		return syscallCollection{}, fmt.Errorf("read syscall stats: %w", err)
	}
	inFlight, err := readSyscallInFlight(objects.SyscallIterInFlight, uint64(now.Nano()))
	if err != nil {
		return syscallCollection{}, err
	}
	collection.inFlight = inFlight
	var counters syscallEventsSyscallCounters
	if err := objects.Counters.Lookup(uint32(0), &counters); err != nil {
		return syscallCollection{}, fmt.Errorf("read syscall counters: %w", err)
	}
	summary := &collection.summary
	summary.StartsFull, summary.StatsFull, summary.ErrnosFull = counters.Values[0], counters.Values[1], counters.Values[2]
	if len(options.pids)+len(options.comms) == 0 && options.container == nil {
		unmatched := counters.Values[3]
		summary.Unmatched = &unmatched
	}
	return collection, nil
}

func fillSyscallFilters(objects syscallEventsObjects, options syscallTraceOptions) error {
	allowed := uint8(1)
	for _, pid := range options.pids {
		if err := objects.AllowedPids.Put(pid, allowed); err != nil {
			return fmt.Errorf("set --process %d: %w", pid, err)
		}
	}
	for _, name := range options.comms {
		var key syscallEventsSyscallComm
		for index := 0; index < len(name) && index < syscallCommMax; index++ {
			key.Comm[index] = int8(name[index])
		}
		if err := objects.AllowedComms.Put(key, allowed); err != nil {
			return fmt.Errorf("set --process %s: %w", name, err)
		}
	}
	if options.container != nil {
		for cgroupID := range options.container.cgroups {
			if err := objects.AllowedCgroups.Put(cgroupID, allowed); err != nil {
				return fmt.Errorf("set --container %s: %w", options.container.name, err)
			}
		}
	}
	return nil
}

// syscallInFlightSize는 BPF의 struct syscall_in_flight 크기다. iterator는 이 크기의 기록을 이어서 쓴다.
const syscallInFlightSize = 32

// readSyscallInFlight는 task iterator를 한 번 돌려 아직 돌아오지 않은 호출을 읽는다. monotonic은 trace를 뗀 시각이다.
func readSyscallInFlight(program *ebpf.Program, monotonic uint64) ([]syscallInFlightSample, error) {
	iterator, err := link.AttachIter(link.IterOptions{Program: program})
	if err != nil {
		return nil, fmt.Errorf("attach iter/task: %w", err)
	}
	defer iterator.Close()
	reader, err := iterator.Open()
	if err != nil {
		return nil, fmt.Errorf("open iter/task: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read in-flight syscalls: %w", err)
	}
	return parseSyscallInFlight(data, monotonic), nil
}

func parseSyscallInFlight(data []byte, monotonic uint64) []syscallInFlightSample {
	samples := make([]syscallInFlightSample, 0, len(data)/syscallInFlightSize)
	for offset := 0; offset+syscallInFlightSize <= len(data); offset += syscallInFlightSize {
		record := data[offset : offset+syscallInFlightSize]
		start := binary.NativeEndian.Uint64(record[0:8])
		nr := binary.NativeEndian.Uint32(record[12:16])
		elapsed := uint64(0)
		if monotonic > start {
			elapsed = monotonic - start
		}
		samples = append(samples, syscallInFlightSample{Process: string(bytesTrimNUL(record[16:32])), Syscall: syscallName(runtime.GOARCH, nr&^syscallCompatBit, nr&syscallCompatBit != 0), ElapsedNS: elapsed})
	}
	return samples
}

func readSyscallErrnos(errnos *ebpf.Map) (map[syscallEventsSyscallKey]map[string]uint64, error) {
	result := map[syscallEventsSyscallKey]map[string]uint64{}
	var key syscallEventsSyscallErrnoKey
	var count uint64
	iterator := errnos.Iterate()
	for iterator.Next(&key, &count) {
		if result[key.Owner] == nil {
			result[key.Owner] = map[string]uint64{}
		}
		result[key.Owner][syscallErrnoName(key.Err)] += count
	}
	if err := iterator.Err(); err != nil {
		return nil, fmt.Errorf("read syscall errors: %w", err)
	}
	return result, nil
}

// syscallRestartErrnos는 kernel 안에서만 쓰는 재시작 값이다(include/linux/errno.h). 신호로 끊긴 호출은 sys_exit
// tracepoint 뒤에 신호를 처리하므로, trace에는 사용자 공간이 보지 못하는 이 값이 보인다.
var syscallRestartErrnos = map[uint32]string{512: "ERESTARTSYS", 513: "ERESTARTNOINTR", 514: "ERESTARTNOHAND", 516: "ERESTART_RESTARTBLOCK"}

// syscallErrnoName은 errno 이름이다. 이름이 없는 값은 번호로 쓴다.
func syscallErrnoName(value uint32) string {
	if name := unix.ErrnoName(syscall.Errno(value)); name != "" {
		return name
	}
	if name, ok := syscallRestartErrnos[value]; ok {
		return name
	}
	return "errno " + strconv.FormatUint(uint64(value), 10)
}

func syscallComm(comm [16]int8) string {
	value := make([]byte, 0, len(comm))
	for _, char := range comm {
		if char == 0 {
			break
		}
		value = append(value, byte(char))
	}
	return string(value)
}

func boolByte(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}
