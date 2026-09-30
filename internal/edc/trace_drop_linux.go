//go:build linux

package edc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	// dropOwnerRescan은 socket 주인을 다시 찾는 최소 간격이다. /proc의 모든 fd를 읽으므로 모르는 socket마다 읽지 않는다.
	// 버림은 짧게 몰려 오고 socket을 가진 process가 곧 끝날 수 있어 간격을 짧게 둔다.
	dropOwnerRescan = 250 * time.Millisecond
	// dropOwnerMiss는 찾지 못한 inode를 다시 찾지 않는 시간이다. 주인이 없는 socket(닫히는 중인 socket)마다 /proc을 읽지 않는다.
	dropOwnerMiss = 5 * time.Second
	// dropWake는 event가 없어도 reader가 깨어나 --duration과 Ctrl-C를 확인하는 간격이다.
	dropWake = time.Second
)

// dropTracepointPaths는 tracepoint를 붙일 때 cilium/ebpf가 읽는 tracefs 경로다. container 안처럼 tracefs가 없으면
// 붙이지 못하므로 먼저 알린다.
var dropTracepointPaths = []string{"/sys/kernel/tracing/events/skb/kfree_skb", "/sys/kernel/debug/tracing/events/skb/kfree_skb"}

// dropTracePrerequisites는 eBPF 조건과 kfree_skb tracepoint를 확인한다.
func dropTracePrerequisites() error {
	if err := captureBPFPrerequisites(); err != nil {
		return err
	}
	for _, path := range dropTracepointPaths {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	return errors.New(T("cli.trace.drop_tracepoint", strings.Join(dropTracepointPaths, ", ")))
}

var loadDropReasonsOnce = sync.OnceValues(func() (dropReasons, error) {
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		return dropReasons{}, fmt.Errorf("read kernel BTF: %w", err)
	}
	var enum *btf.Enum
	if err := spec.TypeByName("skb_drop_reason", &enum); err != nil {
		// 5.17 전 kernel에는 이 enum이 없다. 이유 없이 위치만 보인다.
		return newDropReasons(nil), nil
	}
	values := map[string]uint64{}
	for _, value := range enum.Values {
		values[value.Name] = value.Value
	}
	return newDropReasons(values), nil
})

// validateDropReasons는 trace를 시작하기 전에 --reason 이름을 이 kernel의 이유와 맞춘다.
func validateDropReasons(names []string) error {
	if len(names) == 0 {
		return nil
	}
	reasons, err := loadDropReasonsOnce()
	if err != nil {
		return err
	}
	if len(reasons.values) == 0 {
		return errors.New(T("cli.trace.drop_reason_kernel"))
	}
	_, err = reasons.selectNames(names)
	return err
}

// readListenCounters는 TcpExt의 ListenOverflows와 ListenDrops다. 두 값이 모두 있어야 쓴다.
func readListenCounters() (map[string]uint64, bool) {
	data, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return nil, false
	}
	counters := parseNetstat(data, "TcpExt")
	_, overflows := counters["ListenOverflows"]
	_, drops := counters["ListenDrops"]
	return counters, overflows && drops
}

func loadKernelSymbols() kernelSymbols {
	file, err := os.Open("/proc/kallsyms")
	if err != nil {
		return kernelSymbols{}
	}
	defer file.Close()
	return parseKallsyms(file)
}

// dropOwners는 socket inode의 주인 process다. BPF는 inode만 알므로 /proc/*/fd에서 socket:[inode]를 찾는다.
type dropOwners struct {
	byInode map[uint64]uint32
	pids    map[uint32]dropOwner
	missed  map[uint64]time.Time
	scanned time.Time
	scan    func() map[uint64]uint32
	refresh chan struct{}
	results chan map[uint64]uint32
	done    chan struct{}
	wg      sync.WaitGroup
}

func newDropOwners() *dropOwners {
	return newDropOwnersWithScan(scanDropOwners)
}

func newDropOwnersWithScan(scan func() map[uint64]uint32) *dropOwners {
	owners := &dropOwners{byInode: map[uint64]uint32{}, pids: map[uint32]dropOwner{}, missed: map[uint64]time.Time{}, scan: scan, refresh: make(chan struct{}, 1), results: make(chan map[uint64]uint32, 1), done: make(chan struct{})}
	owners.wg.Add(1)
	go owners.run()
	return owners
}

func (owners *dropOwners) lookup(inode uint64, now time.Time) dropOwner {
	owners.publish()
	if inode == 0 {
		return dropOwner{}
	}
	pid, ok := owners.byInode[inode]
	if !ok {
		if missed, seen := owners.missed[inode]; seen && now.Sub(missed) < dropOwnerMiss {
			return dropOwner{}
		}
		if now.Sub(owners.scanned) >= dropOwnerRescan {
			owners.scanned = now
			owners.missed[inode] = now
			select {
			case owners.refresh <- struct{}{}:
			default:
			}
		}
		if !ok {
			return dropOwner{}
		}
	}
	if owner, cached := owners.pids[pid]; cached {
		return owner
	}
	owner := dropOwner{pid: pid, process: readProcessName(pid), cgroup: processCgroupID(pid)}
	owners.pids[pid] = owner
	return owner
}

func (owners *dropOwners) run() {
	defer owners.wg.Done()
	for {
		select {
		case <-owners.done:
			return
		case <-owners.refresh:
			result := owners.scan()
			select {
			case owners.results <- result:
			case <-owners.done:
				return
			}
		}
	}
}

func (owners *dropOwners) publish() {
	select {
	case byInode := <-owners.results:
		owners.byInode = byInode
		owners.pids = map[uint32]dropOwner{}
		if len(owners.missed) > 4096 {
			owners.missed = map[uint64]time.Time{}
		}
	default:
	}
}

func (owners *dropOwners) close() {
	close(owners.done)
	owners.wg.Wait()
}

// scanDropOwners는 모든 process의 fd에서 socket inode를 모은다. 한 socket을 여러 process가 나눠 가지면 처음 찾은 process를 쓴다.
func scanDropOwners() map[uint64]uint32 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return map[uint64]uint32{}
	}
	byInode := map[uint64]uint32{}
	for _, entry := range entries {
		pid, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil {
			continue
		}
		directory := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(directory, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]"), 10, 64)
			if err != nil {
				continue
			}
			if _, seen := byInode[inode]; !seen {
				byInode[inode] = uint32(pid)
			}
		}
	}
	return byInode
}

func readProcessName(pid uint32) string {
	data, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// processCgroupID는 process가 있는 cgroup v2 디렉터리의 inode다. eBPF가 쓰는 cgroup ID와 같아 --container로 거를 수 있다.
func processCgroupID(pid uint32) uint64 {
	data, err := os.ReadFile(filepath.Join(traceProcRoot, strconv.FormatUint(uint64(pid), 10), "cgroup"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		for _, root := range traceCgroupRoots {
			var stat syscall.Stat_t
			if err := syscall.Stat(filepath.Join(root, path), &stat); err == nil {
				return stat.Ino
			}
		}
	}
	return 0
}

// dropTraceEvent는 버려진 패킷 하나를 event로 바꾼다. 주소를 읽지 못한 패킷은 목적지 대신 protocol 이름을 둔다.
func dropTraceEvent(record dropRecord, clockOffset int64, reason, location string, owner dropOwner) captureEvent {
	event := captureEvent{
		TimestampNS: uint64(int64(record.bootTimeNS) + clockOffset),
		BootTimeNS:  record.bootTimeNS,
		Event:       "drop_" + strings.ReplaceAll(strings.ToLower(reason), " ", "_"),
		Protocol:    "drop",
		Reason:      reason,
		Location:    location,
		Bytes:       uint64(record.length),
		PID:         owner.pid,
		Process:     owner.process,
		CgroupID:    owner.cgroup,
	}
	switch record.family {
	case 4:
		event.Source = formatCaptureAddress(2, record.source, record.sport)
		event.Destination = formatCaptureAddress(2, record.destination, record.dport)
	case 6:
		event.Source = formatCaptureAddress(10, record.source, record.sport)
		event.Destination = formatCaptureAddress(10, record.destination, record.dport)
	default:
		if name, ok := etherTypeNames[record.protocol]; ok {
			event.Target = name
		} else {
			event.Target = fmt.Sprintf("ethertype 0x%04x", record.protocol)
		}
	}
	return event
}

// collectDropEvents는 kernel이 버린 패킷을 모은다. reasons가 있으면 그 이유만 event로 보낸다. 이유별 합계는 늘 모두 센다.
func collectDropEvents(names []string, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	reasons, err := loadDropReasonsOnce()
	if err != nil {
		return captureSummary{}, err
	}
	selected, err := reasons.selectNames(names)
	if err != nil {
		return captureSummary{}, err
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return captureSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadDropEvents()
	if err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	var variables dropEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	var bitmap [dropReasonMax / 64]uint64
	for _, value := range selected {
		bitmap[value/64] |= 1 << (value % 64)
	}
	filter := uint8(0)
	if len(selected) > 0 {
		filter = 1
	}
	if err := errors.Join(variables.EventLimit.Set(uint32(dropEventLimit)), variables.FilterReasons.Set(filter), variables.ReasonFilter.Set(bitmap)); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	objects := dropEventsObjects{}
	if err := spec.LoadAndAssign(&objects, nil); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()
	attached, err := link.Tracepoint("skb", "kfree_skb", objects.KfreeSkb, nil)
	if err != nil {
		return captureSummary{}, fmt.Errorf("attach skb/kfree_skb: %w", err)
	}
	defer attached.Close()

	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return captureSummary{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	listenBefore, listenRead := readListenCounters()
	// --reason이 listen 넘침과 무관한 이유만 고르면 이 값은 요약에 넣지 않는다.
	if value, ok := reasons.values["TCP_LISTEN_OVERFLOW"]; len(selected) > 0 && (!ok || !slices.Contains(selected, value)) {
		listenRead = false
	}
	clockOffset, err := captureClockOffset()
	if err != nil {
		return captureSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	deadline := time.Now().Add(duration)
	readerDone := make(chan struct{})
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = reader.Close()
			case <-readerDone:
			}
		}()
		defer close(readerDone)
	}

	var symbols *kernelSymbols
	owners := newDropOwners()
	defer owners.close()
	var eventCount uint64
	wake := func() {
		next := time.Now().Add(dropWake)
		if duration > 0 && deadline.Before(next) {
			next = deadline
		}
		reader.SetDeadline(next)
	}
	wake()
	finish := func() (captureSummary, error) {
		summary := captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, DropCounts: map[string]uint64{}}
		if err := objects.LostEvents.Lookup(uint32(0), &summary.LostEvents); err != nil {
			return captureSummary{}, fmt.Errorf("read lost event count: %w", err)
		}
		chosen := map[uint32]bool{}
		for _, value := range selected {
			chosen[value] = true
		}
		var key uint32
		var perCPU []uint64
		counts := objects.DropCounts.Iterate()
		for counts.Next(&key, &perCPU) {
			if len(chosen) > 0 && !chosen[key] {
				continue
			}
			var total uint64
			for _, value := range perCPU {
				total += value
			}
			if total > 0 {
				summary.DropCounts[reasons.name(key)] += total
			}
		}
		if err := counts.Err(); err != nil {
			return captureSummary{}, fmt.Errorf("read drop counts: %w", err)
		}
		var windows []dropEventsDropWindow
		if err := objects.DropWindows.Lookup(uint32(0), &windows); err != nil {
			return captureSummary{}, fmt.Errorf("read drop windows: %w", err)
		}
		for _, window := range windows {
			summary.DropSampled += window.Suppressed
		}
		if listenAfter, ok := readListenCounters(); ok && listenRead {
			overflows := listenAfter["ListenOverflows"] - min(listenAfter["ListenOverflows"], listenBefore["ListenOverflows"])
			drops := listenAfter["ListenDrops"] - min(listenAfter["ListenDrops"], listenBefore["ListenDrops"])
			summary.ListenOverflows, summary.ListenDrops = &overflows, &drops
		}
		return summary, nil
	}
	for {
		if duration > 0 && !time.Now().Before(deadline) {
			return finish()
		}
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if duration == 0 || time.Now().Before(deadline) {
				wake()
				continue
			}
			return finish()
		}
		if errors.Is(err, os.ErrClosed) && traceStopRequested(stop) {
			return finish()
		}
		if err != nil {
			return captureSummary{}, err
		}
		parsed, ok := parseDropRecord(record.RawSample)
		if !ok {
			continue
		}
		// kallsyms는 크므로 처음 버린 패킷이 올 때 한 번 읽는다.
		if symbols == nil {
			loaded := loadKernelSymbols()
			symbols = &loaded
		}
		event := dropTraceEvent(parsed, clockOffset, reasons.name(parsed.reason), symbols.name(parsed.location), owners.lookup(parsed.socket, time.Now()))
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return captureSummary{}, err
			}
		}
		eventCount++
	}
}
