//go:build linux

package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type priv_key -type priv_comm -type priv_counters -type priv_window -type priv_event privEvents trace_priv_bpf.c -- -I./bpf

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
	// privEventSize는 BPF의 struct priv_event 크기다.
	privEventSize = 136
	privEventPoll = 200 * time.Millisecond
)

// privHooks는 붙일 program과 kernel BTF에 있어야 할 이름이다. 하나가 없어도 나머지는 붙인다.
var privHooks = []struct {
	name    string
	target  string
	program string
}{
	{"capability", "cap_capable", "priv_cap_capable"},
	{"credentials", "commit_creds", "priv_commit_creds"},
	{"modules", "btf_trace_module_load", "priv_module_load"},
}

func privTracePrerequisites() error {
	return traceBPFPrerequisites("trace priv", func(kernel *btf.Spec) error {
		var task *btf.Struct
		if err := kernel.TypeByName("task_struct", &task); err != nil {
			return errors.New("kernel BTF has no type task_struct")
		}
		if missing := execMissingBTFMembers(task.Members, []string{"cred", "group_leader", "comm"}); missing != "" {
			return fmt.Errorf("kernel BTF has no field task_struct.%s", missing)
		}
		return nil
	})
}

func collectPrivEvents(options privTraceOptions, stop <-chan struct{}, onEvent func(privEvent) error) (privCollection, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return privCollection{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadPrivEvents()
	if err != nil {
		return privCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	namespace, _ := os.Readlink("/proc/self/ns/pid")
	self, selfExcluded := syscallSelfTGID(namespace, os.Getpid())
	var variables privEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return privCollection{}, fmt.Errorf("load eBPF variables: %w", err)
	}
	if err := errors.Join(
		variables.SelfTgid.Set(self),
		variables.FilterProcess.Set(boolByte(len(options.pids)+len(options.comms) > 0)),
		variables.FilterCgroup.Set(boolByte(options.container != nil)),
	); err != nil {
		return privCollection{}, fmt.Errorf("set eBPF variable: %w", err)
	}
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return privCollection{}, fmt.Errorf("read kernel BTF: %w", err)
	}
	// 없는 hook의 program은 verifier가 대상 BTF를 찾지 못해 적재가 실패한다. 적재 전에 빼야 나머지가 돈다.
	unavailable := map[string]string{}
	for _, hook := range privHooks {
		if _, err := kernel.AnyTypesByName(hook.target); err != nil {
			unavailable[hook.name] = "kernel BTF has no " + hook.target
			delete(spec.Programs, hook.program)
		}
	}
	if len(unavailable) == len(privHooks) {
		return privCollection{}, errors.New("kernel BTF has none of the privilege hooks")
	}
	collection, err := ebpf.NewCollection(spec)
	if err != nil {
		return privCollection{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer collection.Close()
	if err := fillPrivFilters(collection, options); err != nil {
		return privCollection{}, err
	}
	clockOffset, err := captureClockOffset()
	if err != nil {
		return privCollection{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	reader, err := ringbuf.NewReader(collection.Maps["events"])
	if err != nil {
		return privCollection{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	var links []link.Link
	defer func() { closeCaptureLinks(links) }()
	for _, hook := range privHooks {
		if unavailable[hook.name] != "" {
			continue
		}
		attached, err := link.AttachTracing(link.TracingOptions{Program: collection.Programs[hook.program]})
		if err != nil {
			return privCollection{}, fmt.Errorf("attach %s: %w", hook.program, err)
		}
		links = append(links, attached)
	}
	result := privCollection{summary: privSummary{SelfExcluded: selfExcluded}}
	if len(unavailable) > 0 {
		result.summary.Unavailable = unavailable
	}
	keep := func(event privEvent) error {
		result.summary.Events++
		result.events = append(result.events, event)
		if onEvent != nil {
			return onEvent(event)
		}
		return nil
	}
	started := time.Now()
	end := started.Add(options.duration)
	finished := func() bool {
		return traceStopRequested(stop) || (options.duration > 0 && !time.Now().Before(end))
	}
	for !finished() {
		wake := time.Now().Add(privEventPoll)
		if options.duration > 0 && end.Before(wake) {
			wake = end
		}
		reader.SetDeadline(wake)
		if err := readPrivEvents(reader, clockOffset, keep, finished); err != nil {
			return privCollection{}, err
		}
	}
	closeCaptureLinks(links)
	links = nil
	result.summary.DurationMS = time.Since(started).Milliseconds()
	// 떼기 전에 ring에 들어온 event를 마저 읽는다.
	reader.SetDeadline(time.Now())
	if err := readPrivEvents(reader, clockOffset, keep, func() bool { return false }); err != nil {
		return privCollection{}, err
	}
	var key privEventsPrivKey
	var values []uint64
	iterator := collection.Maps["checks"].Iterate()
	for iterator.Next(&key, &values) {
		sample := privSample{Process: syscallComm(key.Comm), CgroupID: key.CgroupId, Cap: int32(key.Cap), Denied: key.Denied != 0, NoAudit: key.Noaudit != 0}
		for _, value := range values {
			sample.Checks += value
		}
		result.samples = append(result.samples, sample)
	}
	if err := iterator.Err(); err != nil {
		return privCollection{}, fmt.Errorf("read capability checks: %w", err)
	}
	var counters privEventsPrivCounters
	if err := collection.Maps["counters"].Lookup(uint32(0), &counters); err != nil {
		return privCollection{}, fmt.Errorf("read priv counters: %w", err)
	}
	result.summary.StatsFull, result.summary.RingLost = counters.Values[0], counters.Values[1]
	var windows []privEventsPrivWindow
	if err := collection.Maps["event_windows"].Lookup(uint32(0), &windows); err != nil {
		return privCollection{}, fmt.Errorf("read event windows: %w", err)
	}
	for _, window := range windows {
		result.summary.SampledOut += window.Suppressed
	}
	return result, nil
}

func fillPrivFilters(collection *ebpf.Collection, options privTraceOptions) error {
	allowed := uint8(1)
	for _, pid := range options.pids {
		if err := collection.Maps["allowed_pids"].Put(pid, allowed); err != nil {
			return fmt.Errorf("set --process %d: %w", pid, err)
		}
	}
	for _, name := range options.comms {
		var key privEventsPrivComm
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

// readPrivEvents는 done이 참이 되거나 reader의 마감이 올 때까지 event를 읽는다.
func readPrivEvents(reader syscallEventReader, clockOffset int64, onEvent func(privEvent) error, done func() bool) error {
	for !done() {
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read priv events: %w", err)
		}
		event, ok := parsePrivEvent(record.RawSample, clockOffset)
		if !ok {
			continue
		}
		if err := onEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func parsePrivEvent(sample []byte, clockOffset int64) (privEvent, bool) {
	if len(sample) < privEventSize {
		return privEvent{}, false
	}
	event := privEvent{
		TimestampNS: uint64(int64(binary.NativeEndian.Uint64(sample[0:8])) + clockOffset),
		CgroupID:    binary.NativeEndian.Uint64(sample[8:16]),
		PID:         binary.NativeEndian.Uint32(sample[20:24]),
		TID:         binary.NativeEndian.Uint32(sample[24:28]),
		Process:     string(bytesTrimNUL(sample[64:80])),
	}
	ids := func(index int) *[2]uint32 {
		return &[2]uint32{binary.NativeEndian.Uint32(sample[32+8*index:]), binary.NativeEndian.Uint32(sample[36+8*index:])}
	}
	switch binary.NativeEndian.Uint32(sample[16:20]) {
	case 0:
		event.Event = privEventCap
		event.Capability = privCapabilityName(int32(binary.NativeEndian.Uint32(sample[28:32])))
	case 1:
		event.Event = privEventCred
		event.UID, event.EUID, event.GID, event.EGID = ids(0), ids(1), ids(2), ids(3)
	case 2:
		event.Event = privEventMod
		event.Module = string(bytesTrimNUL(sample[80:136]))
	default:
		return privEvent{}, false
	}
	return event, true
}
