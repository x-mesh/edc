package edc

import (
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"time"
)

const (
	traceARPNewEvent         = "arp_new"
	traceARPStateEvent       = "arp_state"
	traceARPMACChangeEvent   = "arp_mac_change"
	traceARPFailedEvent      = "arp_failed"
	traceARPDeleteEvent      = "arp_delete"
	traceNeighborFailedState = "FAILED"
)

// traceNeighbor는 kernel neighbor table의 IPv4 항목 하나다. state는 kernel의 NUD 상태 이름이다.
type traceNeighbor struct {
	iface   string
	ip      string
	mac     string
	state   string
	deleted bool
}

type neighborKey struct {
	iface string
	ip    string
}

// neighborTracker는 항목마다 마지막 MAC과 상태를 기억해 바뀐 것만 event로 낸다. kernel은 같은 상태를 여러 번 알리기도 한다.
// 실패는 예외다. kernel은 주소 확인을 시작할 때 알리지 않으므로, 이미 FAILED인 항목이 다시 실패해도 FAILED만 온다.
type neighborTracker struct {
	known map[neighborKey]traceNeighbor
}

func newNeighborTracker() *neighborTracker {
	return &neighborTracker{known: map[neighborKey]traceNeighbor{}}
}

// baseline은 trace 시작 때의 table이다. 이미 있던 항목은 event를 내지 않고, 뒤의 변화와 비교하는 데만 쓴다.
func (tracker *neighborTracker) baseline(neighbor traceNeighbor) {
	tracker.known[neighborKey{iface: neighbor.iface, ip: neighbor.ip}] = neighbor
}

func (tracker *neighborTracker) event(neighbor traceNeighbor, timestampNS, bootTimeNS uint64) (captureEvent, bool) {
	key := neighborKey{iface: neighbor.iface, ip: neighbor.ip}
	previous, seen := tracker.known[key]
	event := captureEvent{TimestampNS: timestampNS, BootTimeNS: bootTimeNS, Protocol: "arp", Target: neighbor.ip, Source: neighbor.iface, NewState: neighbor.state}
	if seen {
		event.OldState = previous.state
	}
	if neighbor.deleted {
		if !seen {
			return captureEvent{}, false
		}
		delete(tracker.known, key)
		event.Event, event.MAC, event.NewState = traceARPDeleteEvent, previous.mac, ""
		return event, true
	}
	// 실패 알림에는 MAC이 없기도 하다. 마지막으로 알던 MAC을 남겨야 다음 알림과 비교할 수 있다.
	if neighbor.mac == "" {
		neighbor.mac = previous.mac
	}
	event.MAC = neighbor.mac
	switch {
	case neighbor.state == traceNeighborFailedState:
		event.Event = traceARPFailedEvent
	case seen && previous.mac != "" && neighbor.mac != previous.mac:
		event.Event, event.OldMAC = traceARPMACChangeEvent, previous.mac
	case !seen:
		event.Event = traceARPNewEvent
	default:
		event.Event = traceARPStateEvent
	}
	tracker.known[key] = neighbor
	if seen && previous.state == neighbor.state && previous.mac == neighbor.mac && neighbor.state != traceNeighborFailedState {
		return captureEvent{}, false
	}
	return event, true
}

// neighborInterfaceNames는 interface 번호의 이름이다. 사라진 interface는 번호로 쓴다.
type neighborInterfaceNames map[int]string

func (names neighborInterfaceNames) name(index int) string {
	if name, ok := names[index]; ok {
		return name
	}
	name := "if" + strconv.Itoa(index)
	if iface, err := net.InterfaceByIndex(index); err == nil {
		name = iface.Name
	}
	names[index] = name
	return name
}

// neighborSnapshotChanges는 table을 두 번 읽은 사이의 변화다. 새 항목, MAC이나 상태가 바뀐 항목, 사라진 항목을 key 순서로
// 돌려준다. table을 주기적으로 읽는 macOS가 쓴다. 두 번 읽는 사이에 생겼다 사라진 변화는 보이지 않는다.
func neighborSnapshotChanges(previous, current map[neighborKey]traceNeighbor) []traceNeighbor {
	keys := slices.Collect(maps.Keys(current))
	for key := range previous {
		if _, ok := current[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].iface != keys[j].iface {
			return keys[i].iface < keys[j].iface
		}
		return keys[i].ip < keys[j].ip
	})
	var changes []traceNeighbor
	for _, key := range keys {
		neighbor, now := current[key]
		before, seen := previous[key]
		switch {
		case !now:
			changes = append(changes, traceNeighbor{iface: key.iface, ip: key.ip, deleted: true})
		case !seen || before.mac != neighbor.mac || before.state != neighbor.state:
			changes = append(changes, neighbor)
		}
	}
	return changes
}

// traceNeighborScrollLabels는 목적지 칸에 IP와 MAC을, event 칸에 event와 새 상태를 쓴다.
func traceNeighborScrollLabels(event captureEvent) (string, string) {
	destination := event.Target
	switch {
	case event.OldMAC != "":
		destination += " (" + event.OldMAC + " -> " + event.MAC + ")"
	case event.MAC != "":
		destination += " (" + event.MAC + ")"
	}
	label := event.Event
	if event.Event == traceARPStateEvent || event.Event == traceARPNewEvent {
		label += " " + event.NewState
	}
	return destination, label
}

// traceNeighborCounts는 ARP group 행과 요약이 함께 쓰는 값이다. 한 IP에 MAC이 여럿이면 주소 충돌이나 장비 교체다.
type traceNeighborCounts struct {
	MACs       []string `json:"macs"`
	MACChanges uint64   `json:"mac_changes"`
	Failures   uint64   `json:"failures"`
	macs       map[string]struct{}
}

func (counts *traceNeighborCounts) observe(event captureEvent) {
	for _, mac := range []string{event.OldMAC, event.MAC} {
		if mac == "" {
			continue
		}
		if counts.macs == nil {
			counts.macs = map[string]struct{}{}
		}
		counts.macs[mac] = struct{}{}
	}
	switch event.Event {
	case traceARPMACChangeEvent:
		counts.MACChanges++
	case traceARPFailedEvent:
		counts.Failures++
	}
}

func (counts traceNeighborCounts) finished() traceNeighborCounts {
	counts.MACs = slices.Sorted(maps.Keys(counts.macs))
	return counts
}

func traceNeighborGroupSummary(counts *traceNeighborCounts) *traceNeighborCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

func traceNeighborColumn(value func(counts traceNeighborCounts) uint64) func(traceGroupSummary) string {
	return func(group traceGroupSummary) string {
		if group.Neighbor == nil {
			return "-"
		}
		return strconv.FormatUint(value(*group.Neighbor), 10)
	}
}

var traceNeighborGroupColumns = []traceGroupColumn{
	{screenTitle: "MACS", reportTitle: "MACS", width: 4, value: traceNeighborColumn(func(counts traceNeighborCounts) uint64 { return uint64(len(counts.MACs)) })},
	{screenTitle: "CHG", reportTitle: "MAC_CHANGES", width: 4, value: traceNeighborColumn(func(counts traceNeighborCounts) uint64 { return counts.MACChanges })},
	{screenTitle: "FAIL", reportTitle: "FAILURES", width: 4, value: traceNeighborColumn(func(counts traceNeighborCounts) uint64 { return counts.Failures })},
}

type neighborTraceRow struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	MAC       string `json:"mac,omitempty"`
	State     string `json:"state,omitempty"`
	Events    uint64 `json:"events"`
	traceNeighborCounts
}

type neighborTraceReport struct {
	DurationMS int64              `json:"duration_ms"`
	Events     uint64             `json:"events"`
	LostEvents uint64             `json:"lost_events"`
	MACChanges uint64             `json:"mac_changes"`
	Failures   uint64             `json:"failures"`
	Neighbors  []neighborTraceRow `json:"neighbors"`
}

// neighborTraceSummarizer는 --group-by 없이 끝난 ARP trace를 interface와 IP마다 한 행으로 묶는다.
type neighborTraceSummarizer struct {
	neighbors map[neighborKey]*neighborTraceRow
	counts    map[neighborKey]*traceNeighborCounts
	events    uint64
}

func newNeighborTraceSummarizer() *neighborTraceSummarizer {
	return &neighborTraceSummarizer{neighbors: map[neighborKey]*neighborTraceRow{}, counts: map[neighborKey]*traceNeighborCounts{}}
}

func (summarizer *neighborTraceSummarizer) observe(event captureEvent) {
	key := neighborKey{iface: event.Source, ip: event.Target}
	neighbor := summarizer.neighbors[key]
	if neighbor == nil {
		neighbor = &neighborTraceRow{Interface: event.Source, IP: event.Target}
		summarizer.neighbors[key], summarizer.counts[key] = neighbor, &traceNeighborCounts{}
	}
	if event.MAC != "" {
		neighbor.MAC = event.MAC
	}
	neighbor.State = event.NewState
	if event.Event == traceARPDeleteEvent {
		neighbor.State = "deleted"
	}
	neighbor.Events++
	summarizer.counts[key].observe(event)
	summarizer.events++
}

func (summarizer *neighborTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := neighborTraceReport{DurationMS: duration.Milliseconds(), Events: summarizer.events, LostEvents: summary.LostEvents, Neighbors: make([]neighborTraceRow, 0, len(summarizer.neighbors))}
	for key, stored := range summarizer.neighbors {
		neighbor := *stored
		neighbor.traceNeighborCounts = summarizer.counts[key].finished()
		report.MACChanges += neighbor.MACChanges
		report.Failures += neighbor.Failures
		report.Neighbors = append(report.Neighbors, neighbor)
	}
	// MAC이 바뀐 항목과 실패한 항목을 위에 둔다. 진단할 때 먼저 봐야 하는 행이다.
	sort.Slice(report.Neighbors, func(i, j int) bool {
		left, right := report.Neighbors[i], report.Neighbors[j]
		if left.MACChanges != right.MACChanges {
			return left.MACChanges > right.MACChanges
		}
		if left.Failures != right.Failures {
			return left.Failures > right.Failures
		}
		if left.Interface != right.Interface {
			return left.Interface < right.Interface
		}
		return left.IP < right.IP
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. ARP 요약은 이미 neighbor마다 한 행이라 같은 표를 쓴다.
func (report neighborTraceReport) print(bool) {
	fmt.Fprintf(os.Stdout, "ARP trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Events: %d\nNeighbors: %d\nMAC changes: %d\nFailures: %d\nLost events: %d\n", report.Events, len(report.Neighbors), report.MACChanges, report.Failures, report.LostEvents)
	if len(report.Neighbors) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nINTERFACE\tIP\tMAC\tSTATE\tEVENTS\tMAC_CHANGES\tFAILURES\tMACS")
	for _, neighbor := range report.Neighbors {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n", neighbor.Interface, neighbor.IP, emptyAs(neighbor.MAC, "-"), emptyAs(neighbor.State, "-"), neighbor.Events, neighbor.MACChanges, neighbor.Failures, len(neighbor.MACs))
	}
}
