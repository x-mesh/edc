//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type cpu_key -type cpu_comm -type cpu_counters -type cpu_fresh cpuEvents trace_cpu_bpf.c -- -I./bpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// BPF의 enum cpu_counter 순서다.
const (
	cpuCounterSamples = iota
	cpuCounterIdle
	cpuCounterSelf
	cpuCounterFiltered
	cpuCounterCountsFull
	cpuCounterUserMissing
	cpuCounterUserCollision
	cpuCounterKernelEmpty
	cpuCounterKernelCollision
	cpuCounterStacksFull
	cpuCounterFreshLost
)

const (
	cpuStackDepth = 127
	// 새 process는 BPF의 fresh 알림을 받자마자 읽는다. cpuSnapshotInterval마다 counts map도 훑어, ring이 넘쳐 놓친
	// 알림을 채운다.
	cpuSnapshotInterval = time.Second
)

// cpuRefreshDelays는 새 process를 다시 읽는 때다. exec는 comm을 바꾼 뒤에 공유 library를 mapping하므로, 알림 직후에 읽은
// mapping에는 library가 빠져 있을 수 있다.
var cpuRefreshDelays = []time.Duration{20 * time.Millisecond, 200 * time.Millisecond}

func cpuTracePrerequisites() error {
	return traceBPFPrerequisites("trace cpu", func(kernel *btf.Spec) error {
		var task *btf.Struct
		if err := kernel.TypeByName("task_struct", &task); err != nil {
			return errors.New("kernel BTF has no type task_struct")
		}
		if missing := execMissingBTFMembers(task.Members, []string{"flags", "group_leader", "comm"}); missing != "" {
			return fmt.Errorf("kernel BTF has no field task_struct.%s", missing)
		}
		return nil
	})
}

// cpuOnlineCPUs는 /sys/devices/system/cpu/online의 "0-3,6" 같은 목록을 읽는다. offline CPU에는 perf event를 열 수 없다.
func cpuOnlineCPUs(text string) ([]int, error) {
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(text), ",") {
		if part == "" {
			continue
		}
		low, high, found := strings.Cut(part, "-")
		first, err := strconv.Atoi(low)
		if err != nil {
			return nil, fmt.Errorf("parse online CPUs %q", text)
		}
		last := first
		if found {
			if last, err = strconv.Atoi(high); err != nil || last < first {
				return nil, fmt.Errorf("parse online CPUs %q", text)
			}
		}
		for cpu := first; cpu <= last; cpu++ {
			cpus = append(cpus, cpu)
		}
	}
	if len(cpus) == 0 {
		return nil, fmt.Errorf("parse online CPUs %q", text)
	}
	return cpus, nil
}

// openCPUSampler는 CPU 하나에 cpu-clock event를 frequency Hz로 열고 program을 붙인다. cpu-clock은 hrtimer로 돌아서
// hardware counter가 없는 VM에서도 열린다.
func openCPUSampler(cpu, frequency int, program *ebpf.Program) (int, link.Link, error) {
	attr := unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(frequency),
		Bits:   unix.PerfBitFreq,
	}
	fd, err := unix.PerfEventOpen(&attr, -1, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return -1, nil, fmt.Errorf("open perf event on CPU %d: %w", cpu, err)
	}
	attached, err := link.AttachRawLink(link.RawLinkOptions{Target: fd, Program: program, Attach: ebpf.AttachPerfEvent})
	if err != nil {
		unix.Close(fd)
		return -1, nil, fmt.Errorf("attach perf_event on CPU %d: %w", cpu, err)
	}
	return fd, attached, nil
}

func collectCPUStacks(options cpuTraceOptions, stop <-chan struct{}) ([]cpuStack, cpuSummary, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, cpuSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	online, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, cpuSummary{}, err
	}
	cpus, err := cpuOnlineCPUs(string(online))
	if err != nil {
		return nil, cpuSummary{}, err
	}
	spec, err := loadCpuEvents()
	if err != nil {
		return nil, cpuSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	self, selfExcluded := syscallSelfTGID(namespace, os.Getpid())
	var variables cpuEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return nil, cpuSummary{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := errors.Join(
		variables.SelfTgid.Set(self),
		variables.FilterProcess.Set(boolByte(len(options.pids)+len(options.comms) > 0)),
		variables.FilterCgroup.Set(boolByte(options.container != nil)),
	); err != nil {
		return nil, cpuSummary{}, fmt.Errorf("set eBPF variable: %w", err)
	}
	objects := cpuEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return nil, cpuSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()
	if err := fillCPUFilters(objects, options); err != nil {
		return nil, cpuSummary{}, err
	}
	symbolizer := newCPUSymbolizer("/proc")
	defer symbolizer.close()
	kernelErr := symbolizer.loadKernel()
	var fds []int
	var links []link.Link
	detach := func() {
		closeCaptureLinks(links)
		for _, fd := range fds {
			unix.Close(fd)
		}
		links, fds = nil, nil
	}
	defer detach()
	for _, cpu := range cpus {
		fd, attached, err := openCPUSampler(cpu, options.frequency, objects.CpuSample)
		if err != nil {
			return nil, cpuSummary{}, err
		}
		fds, links = append(fds, fd), append(links, attached)
	}
	reader, err := ringbuf.NewReader(objects.Fresh)
	if err != nil {
		return nil, cpuSummary{}, fmt.Errorf("open fresh ring: %w", err)
	}
	defer reader.Close()
	fresh := make(chan cpuProcessKey, 1024)
	go func() {
		defer close(fresh)
		for {
			record, err := reader.Read()
			if err != nil {
				return
			}
			if len(record.RawSample) >= 20 {
				var comm [16]int8
				for index := range comm {
					comm[index] = int8(record.RawSample[4+index])
				}
				fresh <- cpuProcessKey{TGID: binary.NativeEndian.Uint32(record.RawSample[0:4]), Comm: syscallComm(comm)}
			}
		}
	}()
	started := time.Now()
	var deadline <-chan time.Time
	if options.duration > 0 {
		timer := time.NewTimer(options.duration)
		defer timer.Stop()
		deadline = timer.C
	}
	ticker := time.NewTicker(cpuSnapshotInterval)
	defer ticker.Stop()
	// snapshot은 fresh 알림으로 읽지 못한 process를 counts map에서 찾아 읽는다. 이미 읽은 process는 건너뛴다.
	snapshot := func() {
		var key cpuEventsCpuKey
		var count uint64
		iterator := objects.Counts.Iterate()
		for iterator.Next(&key, &count) {
			if process := (cpuProcessKey{TGID: key.Tgid, Comm: syscallComm(key.Comm)}); symbolizer.processes[process] == nil {
				symbolizer.snapshot(process)
			}
		}
	}
	// 알림을 놓친 때만 counts map을 훑는다. 바쁜 host에서는 항목이 수천 개라 매초 훑는 비용이 크다.
	var freshLost uint64
	missed := func() bool {
		var perCPU []cpuEventsCpuCounters
		if err := objects.Counters.Lookup(uint32(0), &perCPU); err != nil {
			return true
		}
		var lost uint64
		for _, counters := range perCPU {
			lost += counters.Values[cpuCounterFreshLost]
		}
		changed := lost != freshLost
		freshLost = lost
		return changed
	}
	type refresh struct {
		due time.Time
		key cpuProcessKey
	}
	var pending []refresh
	for running := true; running; {
		var wake <-chan time.Time
		if len(pending) > 0 {
			wake = time.After(time.Until(pending[0].due))
		}
		select {
		case <-stop:
			running = false
		case <-deadline:
			running = false
		case key, ok := <-fresh:
			if ok {
				symbolizer.snapshot(key)
				now := time.Now()
				for _, delay := range cpuRefreshDelays {
					pending = append(pending, refresh{now.Add(delay), key})
				}
				sort.Slice(pending, func(i, j int) bool { return pending[i].due.Before(pending[j].due) })
			}
		case <-wake:
			now := time.Now()
			for len(pending) > 0 && !pending[0].due.After(now) {
				symbolizer.snapshot(pending[0].key)
				pending = pending[1:]
			}
		case <-ticker.C:
			if missed() {
				snapshot()
			}
		}
	}
	// 샘플링을 먼저 멈춘다. 그 뒤에 map을 읽어야 읽는 동안 수가 바뀌지 않는다.
	detach()
	elapsed := time.Since(started)
	_ = reader.Close()
	for key := range fresh {
		symbolizer.snapshot(key)
	}
	snapshot()
	symbolizer.refreshPerfMaps()
	summary := cpuSummary{DurationMS: elapsed.Milliseconds(), FrequencyHz: options.frequency, CPUs: len(cpus), SelfExcluded: selfExcluded, KernelSymbols: kernelErr == nil && len(symbolizer.kernel) > 0}
	var perCPU []cpuEventsCpuCounters
	if err := objects.Counters.Lookup(uint32(0), &perCPU); err != nil {
		return nil, cpuSummary{}, fmt.Errorf("read cpu counters: %w", err)
	}
	var totals [cpuCounterFreshLost + 1]uint64
	for _, counters := range perCPU {
		for index := range totals {
			totals[index] += counters.Values[index]
		}
	}
	summary.Samples, summary.Idle, summary.Self, summary.Filtered = totals[cpuCounterSamples], totals[cpuCounterIdle], totals[cpuCounterSelf], totals[cpuCounterFiltered]
	summary.CountsFull, summary.StacksFull = totals[cpuCounterCountsFull], totals[cpuCounterStacksFull]
	summary.UserStackMissing, summary.UserStackCollision = totals[cpuCounterUserMissing], totals[cpuCounterUserCollision]
	summary.KernelStackEmpty, summary.KernelStackCollision = totals[cpuCounterKernelEmpty], totals[cpuCounterKernelCollision]
	summary.FreshLost = totals[cpuCounterFreshLost]
	summary.BusyPercent = cpuBusyPercent(summary.Samples-summary.Idle, options.frequency, elapsed, len(cpus))
	stacks, err := readCPUStacks(objects, symbolizer)
	if err != nil {
		return nil, cpuSummary{}, err
	}
	return stacks, summary, nil
}

func fillCPUFilters(objects cpuEventsObjects, options cpuTraceOptions) error {
	allowed := uint8(1)
	for _, pid := range options.pids {
		if err := objects.AllowedPids.Put(pid, allowed); err != nil {
			return fmt.Errorf("set --process %d: %w", pid, err)
		}
	}
	for _, name := range options.comms {
		var key cpuEventsCpuComm
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

// readCPUStacks는 counts의 항목마다 두 stack을 stack map에서 읽어 이름을 붙인다. 같은 stack id는 한 번만 읽는다.
func readCPUStacks(objects cpuEventsObjects, symbolizer *cpuSymbolizer) ([]cpuStack, error) {
	addresses := map[int32][]uint64{}
	read := func(id int32) []uint64 {
		if id < 0 {
			return nil
		}
		if cached, ok := addresses[id]; ok {
			return cached
		}
		var raw [cpuStackDepth]uint64
		var ips []uint64
		if err := objects.Stacks.Lookup(uint32(id), &raw); err == nil {
			for _, ip := range raw {
				if ip == 0 {
					break
				}
				ips = append(ips, ip)
			}
		}
		addresses[id] = ips
		return ips
	}
	var stacks []cpuStack
	var key cpuEventsCpuKey
	var count uint64
	iterator := objects.Counts.Iterate()
	for iterator.Next(&key, &count) {
		stack := cpuStack{TGID: key.Tgid, Process: syscallComm(key.Comm), CgroupID: key.CgroupId, Samples: count}
		for _, ip := range read(key.KernelStack) {
			stack.Kernel = append(stack.Kernel, symbolizer.kernelFrame(ip))
		}
		for index, ip := range read(key.UserStack) {
			// 가장 안쪽이 아닌 frame의 주소는 call 다음 명령이다. 함수 끝의 call이면 다음 함수로 넘어가므로 1을 빼고 찾는다.
			if index > 0 {
				ip--
			}
			stack.User = append(stack.User, symbolizer.userFrame(cpuProcessKey{TGID: key.Tgid, Comm: stack.Process}, ip))
		}
		stacks = append(stacks, stack)
	}
	if err := iterator.Err(); err != nil {
		return nil, fmt.Errorf("read cpu samples: %w", err)
	}
	return stacks, nil
}
