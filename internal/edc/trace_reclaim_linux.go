//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type reclaim_key -type reclaim_stats -type reclaim_comm -type reclaim_counters -type reclaim_window -type reclaim_event reclaimEvents trace_reclaim_bpf.c -- -I./bpf

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

const (
	// reclaimEventSize는 BPF의 struct reclaim_event 크기다.
	reclaimEventSize = 64
	reclaimEventPoll = 200 * time.Millisecond
)

// reclaimTracePrerequisites는 task local storage만 본다. 회수 hook은 kernel 설정에 따라 없을 수 있어, 붙일 때 하나씩
// 확인하고 없는 종류를 요약에 남긴다.
func reclaimTracePrerequisites() error {
	return traceBPFPrerequisites("trace reclaim", func(kernel *btf.Spec) error {
		var storage *btf.Func
		if err := kernel.TypeByName("bpf_task_storage_get", &storage); err != nil {
			return errors.New("kernel BTF has no task local storage")
		}
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

func collectReclaimStats(options reclaimTraceOptions, stop <-chan struct{}, onEvent func(reclaimEvent) error) (reclaimCollection, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return reclaimCollection{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadReclaimEvents()
	if err != nil {
		return reclaimCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	self, selfExcluded := syscallSelfTGID(namespace, os.Getpid())
	var variables reclaimEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return reclaimCollection{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := errors.Join(
		variables.SelfTgid.Set(self),
		variables.FilterProcess.Set(boolByte(len(options.pids)+len(options.comms) > 0)),
		variables.FilterCgroup.Set(boolByte(options.container != nil)),
		variables.SlowNs.Set(uint64(options.slow)),
	); err != nil {
		return reclaimCollection{}, fmt.Errorf("set eBPF variable: %w", err)
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return reclaimCollection{}, fmt.Errorf("read kernel BTF: %w", err)
	}
	// 없는 hook의 program은 verifier가 대상 BTF를 찾지 못해 적재가 실패한다. 적재 전에 빼야 나머지가 돈다.
	unavailable := map[string]string{}
	hooks := []struct {
		kind    string
		targets [2]string
		names   [2]string
	}{
		{"direct", [2]string{"btf_trace_mm_vmscan_direct_reclaim_begin", "btf_trace_mm_vmscan_direct_reclaim_end"}, [2]string{"reclaim_direct_begin", "reclaim_direct_end"}},
		{"memcg", [2]string{"btf_trace_mm_vmscan_memcg_reclaim_begin", "btf_trace_mm_vmscan_memcg_reclaim_end"}, [2]string{"reclaim_memcg_begin", "reclaim_memcg_end"}},
		{"throttle", [2]string{"mem_cgroup_handle_over_high", "mem_cgroup_handle_over_high"}, [2]string{"reclaim_throttle_begin", "reclaim_throttle_end"}},
		{"compact", [2]string{"btf_trace_mm_compaction_begin", "btf_trace_mm_compaction_end"}, [2]string{"reclaim_compact_begin", "reclaim_compact_end"}},
	}
	for _, hook := range hooks {
		for _, target := range hook.targets {
			if _, err := kernel.AnyTypesByName(target); err != nil {
				unavailable[hook.kind] = "kernel BTF has no " + target
			}
		}
		if unavailable[hook.kind] != "" {
			delete(spec.Programs, hook.names[0])
			delete(spec.Programs, hook.names[1])
		}
	}
	if len(unavailable) == len(hooks) {
		return reclaimCollection{}, errors.New("kernel BTF has none of the reclaim hooks")
	}
	collection, err := ebpf.NewCollection(spec)
	if err != nil {
		return reclaimCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer collection.Close()
	objects := reclaimObjectsFrom(collection)
	if err := fillReclaimFilters(objects, options); err != nil {
		return reclaimCollection{}, err
	}
	var clockOffset int64
	var reader *ringbuf.Reader
	if options.slow > 0 {
		if clockOffset, err = captureClockOffset(); err != nil {
			return reclaimCollection{}, fmt.Errorf("read monotonic clock: %w", err)
		}
		if reader, err = ringbuf.NewReader(objects.events); err != nil {
			return reclaimCollection{}, fmt.Errorf("open event ring: %w", err)
		}
		defer reader.Close()
	}
	// 끝 hook을 먼저 붙인다. 반대 순서면 그 사이에 시작한 회수가 끝을 놓쳐 다음 회수와 짝이 어긋날 수 있다.
	var links []link.Link
	defer func() { closeCaptureLinks(links) }()
	for _, hook := range hooks {
		if unavailable[hook.kind] != "" {
			continue
		}
		for _, name := range []string{hook.names[1], hook.names[0]} {
			attached, err := link.AttachTracing(link.TracingOptions{Program: collection.Programs[name]})
			if err != nil {
				return reclaimCollection{}, fmt.Errorf("attach %s: %w", name, err)
			}
			links = append(links, attached)
		}
	}
	started := time.Now()
	var events uint64
	if reader == nil {
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
	} else {
		end := started.Add(options.duration)
		finished := func() bool {
			return traceStopRequested(stop) || (options.duration > 0 && !time.Now().Before(end))
		}
		for !finished() {
			wake := time.Now().Add(reclaimEventPoll)
			if options.duration > 0 && end.Before(wake) {
				wake = end
			}
			reader.SetDeadline(wake)
			if err := readReclaimEvents(reader, clockOffset, onEvent, &events, finished); err != nil {
				return reclaimCollection{}, err
			}
		}
	}
	closeCaptureLinks(links)
	links = nil
	elapsed := time.Since(started)
	if reader != nil {
		reader.SetDeadline(time.Now())
		if err := readReclaimEvents(reader, clockOffset, onEvent, &events, func() bool { return false }); err != nil {
			return reclaimCollection{}, err
		}
	}
	result := reclaimCollection{summary: reclaimSummary{DurationMS: elapsed.Milliseconds(), SelfExcluded: selfExcluded, Events: events}}
	if len(unavailable) > 0 {
		result.summary.Unavailable = unavailable
	}
	var key reclaimEventsReclaimKey
	var values []reclaimEventsReclaimStats
	iterator := objects.stats.Iterate()
	for iterator.Next(&key, &values) {
		sample := reclaimSample{Process: syscallComm(key.Comm), CgroupID: key.CgroupId, Kind: reclaimKindName(key.Kind)}
		for _, value := range values {
			sample.Count += value.Count
			sample.SumNS += value.SumNs
			if value.MaxNs > sample.MaxNS {
				sample.MaxNS = value.MaxNs
			}
			sample.Reclaimed += value.Reclaimed
			for index, count := range value.Hist {
				sample.Hist[index] += count
			}
		}
		result.samples = append(result.samples, sample)
	}
	if err := iterator.Err(); err != nil {
		return reclaimCollection{}, fmt.Errorf("read reclaim stats: %w", err)
	}
	var counters reclaimEventsReclaimCounters
	if err := objects.counters.Lookup(uint32(0), &counters); err != nil {
		return reclaimCollection{}, fmt.Errorf("read reclaim counters: %w", err)
	}
	result.summary.StatsFull, result.summary.RingLost = counters.Values[0], counters.Values[1]
	var windows []reclaimEventsReclaimWindow
	if err := objects.eventWindows.Lookup(uint32(0), &windows); err != nil {
		return reclaimCollection{}, fmt.Errorf("read event windows: %w", err)
	}
	for _, window := range windows {
		result.summary.SampledOut += window.Suppressed
	}
	return result, nil
}

// reclaimObjects는 적재한 collection의 map이다. 없는 hook의 program을 빼고 적재하므로 생성된 Objects 구조체를 쓰지 않는다.
type reclaimObjects struct {
	stats, allowedPids, allowedComms, allowedCgroups, counters, events, eventWindows *ebpf.Map
}

func reclaimObjectsFrom(collection *ebpf.Collection) reclaimObjects {
	return reclaimObjects{
		stats: collection.Maps["stats"], allowedPids: collection.Maps["allowed_pids"], allowedComms: collection.Maps["allowed_comms"],
		allowedCgroups: collection.Maps["allowed_cgroups"], counters: collection.Maps["counters"], events: collection.Maps["events"], eventWindows: collection.Maps["event_windows"],
	}
}

func fillReclaimFilters(objects reclaimObjects, options reclaimTraceOptions) error {
	allowed := uint8(1)
	for _, pid := range options.pids {
		if err := objects.allowedPids.Put(pid, allowed); err != nil {
			return fmt.Errorf("set --process %d: %w", pid, err)
		}
	}
	for _, name := range options.comms {
		var key reclaimEventsReclaimComm
		for index := 0; index < len(name) && index < syscallCommMax; index++ {
			key.Comm[index] = int8(name[index])
		}
		if err := objects.allowedComms.Put(key, allowed); err != nil {
			return fmt.Errorf("set --process %s: %w", name, err)
		}
	}
	if options.container != nil {
		for cgroupID := range options.container.cgroups {
			if err := objects.allowedCgroups.Put(cgroupID, allowed); err != nil {
				return fmt.Errorf("set --container %s: %w", options.container.name, err)
			}
		}
	}
	return nil
}

// readReclaimEvents는 done이 참이 되거나 reader의 마감이 올 때까지 event를 읽는다. 기록이 쏟아져도 --duration과 Ctrl-C에
// 멈추도록 기록마다 done을 본다.
func readReclaimEvents(reader syscallEventReader, clockOffset int64, onEvent func(reclaimEvent) error, count *uint64, done func() bool) error {
	pageSize := uint64(os.Getpagesize())
	for !done() {
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read reclaim events: %w", err)
		}
		event, ok := parseReclaimEvent(record.RawSample, clockOffset, pageSize)
		if !ok {
			continue
		}
		*count++
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseReclaimEvent(sample []byte, clockOffset int64, pageSize uint64) (reclaimEvent, bool) {
	if len(sample) < reclaimEventSize {
		return reclaimEvent{}, false
	}
	kind := binary.NativeEndian.Uint32(sample[40:44])
	event := reclaimEvent{
		TimestampNS: uint64(int64(binary.NativeEndian.Uint64(sample[0:8])) + clockOffset),
		Event:       "reclaim",
		LatencyMS:   float64(binary.NativeEndian.Uint64(sample[8:16])) / float64(time.Millisecond),
		CgroupID:    binary.NativeEndian.Uint64(sample[24:32]),
		PID:         binary.NativeEndian.Uint32(sample[32:36]),
		TID:         binary.NativeEndian.Uint32(sample[36:40]),
		Kind:        reclaimKindName(kind),
		Process:     string(bytesTrimNUL(sample[48:64])),
	}
	if kind == 0 || kind == 1 {
		reclaimed := binary.NativeEndian.Uint64(sample[16:24]) * pageSize
		event.Reclaimed = &reclaimed
	}
	return event, true
}
