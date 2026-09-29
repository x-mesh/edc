package edc

import (
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// neighbor event의 종류다. event 이름은 protocol 이름을 앞에 붙인다. 예: arp_mac_change, ndp_failed.
const (
	traceNeighborNew         = "new"
	traceNeighborState       = "state"
	traceNeighborMACChange   = "mac_change"
	traceNeighborFailed      = "failed"
	traceNeighborDelete      = "delete"
	traceNeighborFailedState = "FAILED"
)

func traceNeighborEvent(protocol, kind string) string {
	return protocol + "_" + kind
}

// traceNeighborKind는 ARP와 NDP event 이름에서 protocol을 뺀 종류다.
func traceNeighborKind(event string) string {
	_, kind, _ := strings.Cut(event, "_")
	return kind
}

// traceNeighbor는 kernel neighbor table의 항목 하나다. ARP는 IPv4, NDP는 IPv6 항목이다. state는 kernel의 NUD 상태 이름이다.
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
	protocol string
	known    map[neighborKey]traceNeighbor
}

func newNeighborTracker(protocol string) *neighborTracker {
	return &neighborTracker{protocol: protocol, known: map[neighborKey]traceNeighbor{}}
}

// baseline은 trace 시작 때의 table이다. 이미 있던 항목은 event를 내지 않고, 뒤의 변화와 비교하는 데만 쓴다.
func (tracker *neighborTracker) baseline(neighbor traceNeighbor) {
	tracker.known[neighborKey{iface: neighbor.iface, ip: neighbor.ip}] = neighbor
}

func (tracker *neighborTracker) event(neighbor traceNeighbor, timestampNS, bootTimeNS uint64) (captureEvent, bool) {
	key := neighborKey{iface: neighbor.iface, ip: neighbor.ip}
	previous, seen := tracker.known[key]
	event := captureEvent{TimestampNS: timestampNS, BootTimeNS: bootTimeNS, Protocol: tracker.protocol, Target: neighbor.ip, Source: neighbor.iface, NewState: neighbor.state}
	if seen {
		event.OldState = previous.state
	}
	if neighbor.deleted {
		if !seen {
			return captureEvent{}, false
		}
		delete(tracker.known, key)
		event.Event, event.MAC, event.NewState = traceNeighborEvent(tracker.protocol, traceNeighborDelete), previous.mac, ""
		return event, true
	}
	// 실패 알림에는 MAC이 없기도 하다. 마지막으로 알던 MAC을 남겨야 다음 알림과 비교할 수 있다.
	if neighbor.mac == "" {
		neighbor.mac = previous.mac
	}
	event.MAC = neighbor.mac
	kind := traceNeighborState
	switch {
	case neighbor.state == traceNeighborFailedState:
		kind = traceNeighborFailed
	case seen && previous.mac != "" && neighbor.mac != previous.mac:
		kind, event.OldMAC = traceNeighborMACChange, previous.mac
	case !seen:
		kind = traceNeighborNew
	}
	event.Event = traceNeighborEvent(tracker.protocol, kind)
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
	if kind := traceNeighborKind(event.Event); kind == traceNeighborState || kind == traceNeighborNew {
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
	switch traceNeighborKind(event.Event) {
	case traceNeighborMACChange:
		counts.MACChanges++
	case traceNeighborFailed:
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
	protocol   string
	DurationMS int64              `json:"duration_ms"`
	Events     uint64             `json:"events"`
	LostEvents uint64             `json:"lost_events"`
	MACChanges uint64             `json:"mac_changes"`
	Failures   uint64             `json:"failures"`
	Neighbors  []neighborTraceRow `json:"neighbors"`
}

// neighborTraceSummarizer는 --group-by 없이 끝난 ARP나 NDP trace를 interface와 IP마다 한 행으로 묶는다.
type neighborTraceSummarizer struct {
	protocol  string
	neighbors map[neighborKey]*neighborTraceRow
	counts    map[neighborKey]*traceNeighborCounts
	events    uint64
}

func newNeighborTraceSummarizer(protocol string) *neighborTraceSummarizer {
	return &neighborTraceSummarizer{protocol: protocol, neighbors: map[neighborKey]*neighborTraceRow{}, counts: map[neighborKey]*traceNeighborCounts{}}
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
	if traceNeighborKind(event.Event) == traceNeighborDelete {
		neighbor.State = "deleted"
	}
	neighbor.Events++
	summarizer.counts[key].observe(event)
	summarizer.events++
}

func (summarizer *neighborTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := neighborTraceReport{protocol: summarizer.protocol, DurationMS: duration.Milliseconds(), Events: summarizer.events, LostEvents: summary.LostEvents, Neighbors: make([]neighborTraceRow, 0, len(summarizer.neighbors))}
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

// -d는 연결마다 한 행을 쓰는 option이다. ARP와 NDP 요약은 이미 neighbor마다 한 행이라 같은 표를 쓴다.
func (report neighborTraceReport) print(bool) {
	fmt.Fprintf(os.Stdout, "%s trace: %s\n\n", strings.ToUpper(report.protocol), (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Events: %d\nNeighbors: %d\nMAC changes: %d\nFailures: %d\nLost events: %d\n", report.Events, len(report.Neighbors), report.MACChanges, report.Failures, report.LostEvents)
	if len(report.Neighbors) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nINTERFACE\tIP\tMAC\tSTATE\tEVENTS\tMAC_CHANGES\tFAILURES\tMACS")
	for _, neighbor := range report.Neighbors {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n", neighbor.Interface, neighbor.IP, emptyAs(neighbor.MAC, "-"), emptyAs(neighbor.State, "-"), neighbor.Events, neighbor.MACChanges, neighbor.Failures, len(neighbor.MACs))
	}
}
