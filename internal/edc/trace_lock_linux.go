//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type lock_key -type lock_stats -type lock_comm -type lock_counters -type lock_fresh lockEvents trace_lock_bpf.c -- -I./bpf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// BPF의 enum lock_counter 순서다.
const (
	lockCounterStatsFull = iota
	lockCounterDepthFull
	lockCounterUnmatched
	lockCounterStackLost
	lockCounterFreshLost
	lockCounters
)

const lockStackDepth = 64

func lockTracePrerequisites() error {
	return traceBPFPrerequisites("trace lock", func(kernel *btf.Spec) error {
		var storage *btf.Func
		if err := kernel.TypeByName("bpf_task_storage_get", &storage); err != nil {
			return errors.New("kernel BTF has no task local storage")
		}
		return nil
	})
}

// lockHooks는 kind마다 붙일 program과 kernel BTF에 있어야 할 이름이다. 5.19 전 kernel에는 contention tracepoint가 없다.
var lockHooks = []struct {
	kind     string
	target   string
	programs []string
}{
	{lockKindKernel, "btf_trace_contention_begin", []string{"lock_contention_end", "lock_contention_begin"}},
	{lockKindFutex, "futex_wait", []string{"lock_futex_wait_exit", "lock_futex_wait"}},
}

func collectLockStats(options lockTraceOptions, stop <-chan struct{}) (lockCollection, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return lockCollection{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadLockEvents()
	if err != nil {
		return lockCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	self, selfExcluded := syscallSelfTGID(namespace, os.Getpid())
	var variables lockEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return lockCollection{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := errors.Join(
		variables.SelfTgid.Set(self),
		variables.FilterProcess.Set(boolByte(len(options.pids)+len(options.comms) > 0)),
		variables.FilterCgroup.Set(boolByte(options.container != nil)),
	); err != nil {
		return lockCollection{}, fmt.Errorf("set eBPF variable: %w", err)
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return lockCollection{}, fmt.Errorf("read kernel BTF: %w", err)
	}
	// 고르지 않은 kind와 kernel에 없는 hook의 program은 적재 전에 뺀다. 없는 대상의 program은 verifier가 받지 않는다.
	unavailable := map[string]string{}
	attached := map[string]bool{}
	for _, hook := range lockHooks {
		_, missing := kernel.AnyTypesByName(hook.target)
		switch {
		case options.kind != "" && options.kind != hook.kind:
		case missing != nil:
			unavailable[hook.kind] = "kernel BTF has no " + hook.target
		default:
			attached[hook.kind] = true
			continue
		}
		for _, name := range hook.programs {
			delete(spec.Programs, name)
		}
	}
	if len(attached) == 0 {
		reasons := []string{}
		for kind, reason := range unavailable {
			reasons = append(reasons, kind+": "+reason)
		}
		return lockCollection{}, fmt.Errorf("no lock hook to attach: %v", reasons)
	}
	collection, err := ebpf.NewCollection(spec)
	if err != nil {
		return lockCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer collection.Close()
	if err := fillLockFilters(collection, options); err != nil {
		return lockCollection{}, err
	}
	symbolizer := newCPUSymbolizer("/proc")
	symbolizer.data = true
	defer symbolizer.close()
	kernelErr := symbolizer.loadKernel()
	reader, err := ringbuf.NewReader(collection.Maps["fresh"])
	if err != nil {
		return lockCollection{}, fmt.Errorf("open fresh ring: %w", err)
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
	// 끝 hook을 먼저 붙인다. 반대 순서면 그 사이에 시작한 기다림의 끝을 놓친다.
	var links []link.Link
	defer func() { closeCaptureLinks(links) }()
	for _, hook := range lockHooks {
		if !attached[hook.kind] {
			continue
		}
		for _, name := range hook.programs {
			attachedLink, err := link.AttachTracing(link.TracingOptions{Program: collection.Programs[name]})
			if err != nil {
				return lockCollection{}, fmt.Errorf("attach %s: %w", name, err)
			}
			links = append(links, attachedLink)
		}
	}
	started := time.Now()
	var deadline <-chan time.Time
	if options.duration > 0 {
		timer := time.NewTimer(options.duration)
		defer timer.Stop()
		deadline = timer.C
	}
	for running := true; running; {
		select {
		case <-stop:
			running = false
		case <-deadline:
			running = false
		case key, ok := <-fresh:
			if ok {
				symbolizer.snapshot(key)
			}
		}
	}
	closeCaptureLinks(links)
	links = nil
	elapsed := time.Since(started)
	_ = reader.Close()
	for key := range fresh {
		symbolizer.snapshot(key)
	}
	result := lockCollection{summary: lockSummary{DurationMS: elapsed.Milliseconds(), SelfExcluded: selfExcluded, KernelSymbols: kernelErr == nil && len(symbolizer.kernel) > 0}}
	if len(unavailable) > 0 {
		result.summary.Unavailable = unavailable
	}
	var counters lockEventsLockCounters
	if err := collection.Maps["counters"].Lookup(uint32(0), &counters); err != nil {
		return lockCollection{}, fmt.Errorf("read lock counters: %w", err)
	}
	result.summary.StatsFull, result.summary.DepthFull, result.summary.Unmatched = counters.Values[lockCounterStatsFull], counters.Values[lockCounterDepthFull], counters.Values[lockCounterUnmatched]
	result.summary.StackLost, result.summary.FreshLost = counters.Values[lockCounterStackLost], counters.Values[lockCounterFreshLost]
	stacks := map[int32][]uint64{}
	readStack := func(id int32) []uint64 {
		if id < 0 {
			return nil
		}
		if cached, ok := stacks[id]; ok {
			return cached
		}
		var raw [lockStackDepth]uint64
		var ips []uint64
		if err := collection.Maps["stacks"].Lookup(uint32(id), &raw); err == nil {
			for _, ip := range raw {
				if ip == 0 {
					break
				}
				ips = append(ips, ip)
			}
		}
		stacks[id] = ips
		return ips
	}
	var key lockEventsLockKey
	var values []lockEventsLockStats
	iterator := collection.Maps["stats"].Iterate()
	for iterator.Next(&key, &values) {
		sample := lockSample{Lock: key.Lock, Flags: key.Flags, Process: syscallComm(key.Comm), TGID: key.Tgid, CgroupID: key.CgroupId}
		process := cpuProcessKey{TGID: key.Tgid, Comm: sample.Process}
		switch key.Kind {
		case 0:
			sample.Kind = lockKindKernel
			sample.LockName = symbolizer.kernelDataName(key.Lock)
			for _, ip := range readStack(key.Stack) {
				sample.Frames = append(sample.Frames, symbolizer.kernelFrame(ip))
			}
		default:
			sample.Kind = lockKindFutex
			sample.LockName = symbolizer.userDataName(process, key.Lock)
			for index, ip := range readStack(key.Stack) {
				// 가장 안쪽이 아닌 frame의 주소는 call 다음 명령이다. 함수 끝의 call이면 다음 함수로 넘어가므로 1을 빼고 찾는다.
				if index > 0 {
					ip--
				}
				sample.Frames = append(sample.Frames, symbolizer.userFrame(process, ip))
			}
		}
		for _, value := range values {
			sample.Count += value.Count
			sample.SumNS += value.SumNs
			if value.MaxNs > sample.MaxNS {
				sample.MaxNS = value.MaxNs
			}
			for index, count := range value.Hist {
				sample.Hist[index] += count
			}
		}
		result.samples = append(result.samples, sample)
	}
	if err := iterator.Err(); err != nil {
		return lockCollection{}, fmt.Errorf("read lock stats: %w", err)
	}
	return result, nil
}

func fillLockFilters(collection *ebpf.Collection, options lockTraceOptions) error {
	allowed := uint8(1)
	for _, pid := range options.pids {
		if err := collection.Maps["allowed_pids"].Put(pid, allowed); err != nil {
			return fmt.Errorf("set --process %d: %w", pid, err)
		}
	}
	for _, name := range options.comms {
		var key lockEventsLockComm
		for index := 0; index < len(name) && index < syscallCommMax; index++ {
			key.Comm[index] = int8(name[index])
		}
		if err := collection.Maps["allowed_comms"].Put(key, allowed); err != nil {
			return fmt.Errorf("set --process %s: %w", name, err)
		}
	}
	if options.container != nil {
		for cgroupID := range options.container.cgroups {
			if err := collection.Maps["allowed_cgroups"].Put(cgroupID, allowed); err != nil {
				return fmt.Errorf("set --container %s: %w", options.container.name, err)
			}
		}
	}
	return nil
}
