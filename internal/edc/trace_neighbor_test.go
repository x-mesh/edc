package edc

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestARPTrackerReportsOnlyChanges(t *testing.T) {
	tracker := newNeighborTracker("arp")
	tracker.baseline(traceNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "REACHABLE"})
	steps := []struct {
		neighbor traceNeighbor
		want     string
	}{
		// trace 전부터 있던 항목의 같은 알림은 event가 아니다.
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "REACHABLE"}, ""},
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "STALE"}, "arp_state"},
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:02", state: "REACHABLE"}, "arp_mac_change"},
		// 실패 알림에 MAC이 없어도 마지막 MAC을 남긴다. 다시 실패하면 확인을 새로 한 것이라 다시 센다.
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", state: traceNeighborFailedState}, "arp_failed"},
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", state: traceNeighborFailedState}, "arp_failed"},
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", deleted: true}, "arp_delete"},
		{traceNeighbor{iface: "eth0", ip: "192.0.2.1", deleted: true}, ""},
		{traceNeighbor{iface: "eth1", ip: "192.0.2.1", mac: "02:00:00:00:00:09", state: "REACHABLE"}, "arp_new"},
	}
	var events []captureEvent
	for index, step := range steps {
		event, ok := tracker.event(step.neighbor, 1, 1)
		if got := map[bool]string{true: event.Event}[ok]; got != step.want {
			t.Fatalf("step %d: event %q, want %q (%#v)", index, got, step.want, event)
		}
		if ok {
			events = append(events, event)
		}
	}
	change, failed, deleted := events[1], events[2], events[4]
	if change.OldMAC != "02:00:00:00:00:01" || change.MAC != "02:00:00:00:00:02" || change.Protocol != "arp" || change.Target != "192.0.2.1" || change.Source != "eth0" {
		t.Fatalf("mac change = %#v", change)
	}
	if failed.MAC != "02:00:00:00:00:02" || failed.OldState != "REACHABLE" || failed.NewState != traceNeighborFailedState {
		t.Fatalf("failed = %#v", failed)
	}
	if deleted.MAC != "02:00:00:00:00:02" || deleted.OldState != traceNeighborFailedState || deleted.NewState != "" {
		t.Fatalf("deleted = %#v", deleted)
	}
	if destination, label := traceNeighborScrollLabels(change); destination != "192.0.2.1 (02:00:00:00:00:01 -> 02:00:00:00:00:02)" || label != "arp_mac_change" {
		t.Fatalf("mac change labels = %q, %q", destination, label)
	}
	if _, label := traceNeighborScrollLabels(events[0]); label != "arp_state STALE" {
		t.Fatalf("state label = %q", label)
	}
	if destination, label := traceNeighborScrollLabels(events[len(events)-1]); destination != "192.0.2.1 (02:00:00:00:00:09)" || label != "arp_new REACHABLE" {
		t.Fatalf("new labels = %q, %q", destination, label)
	}

	summarizer := newNeighborTraceSummarizer("arp")
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 1}, time.Second).(neighborTraceReport)
	if report.Events != 6 || report.MACChanges != 1 || report.Failures != 2 || len(report.Neighbors) != 2 {
		t.Fatalf("report = %#v", report)
	}
	first := report.Neighbors[0]
	if first.Interface != "eth0" || first.State != "deleted" || first.MAC != "02:00:00:00:00:02" || !slices.Equal(first.MACs, []string{"02:00:00:00:00:01", "02:00:00:00:00:02"}) {
		t.Fatalf("first neighbor = %#v", first)
	}
	data, _ := json.Marshal(report)
	for _, field := range []string{`"mac_changes":1`, `"neighbors":[`, `"macs":["02:00:00:00:00:01","02:00:00:00:00:02"]`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}

	groups := summarizeTraceGroups("arp", traceGroupByTarget, events, captureSummary{}, time.Second, "", "")
	if len(groups.Groups) != 1 || groups.Groups[0].Group != "192.0.2.1" || groups.Groups[0].Neighbor == nil || len(groups.Groups[0].Neighbor.MACs) != 3 || groups.Groups[0].Neighbor.MACChanges != 1 || groups.Groups[0].Neighbor.Failures != 2 {
		t.Fatalf("arp groups = %#v", groups.Groups)
	}
	if views := traceGroupViews("arp"); slices.Contains(views, traceGroupByPort) || slices.Contains(views, traceGroupByProcess) || !slices.Contains(views, traceGroupBySource) {
		t.Fatalf("arp views = %q", views)
	}
}

// ARP와 NDP는 netlink나 sysctl만 쓰므로 root 없이 동작한다. eBPF 확인을 붙이면 일반 사용자가 쓸 수 없게 된다.
func TestOnlyBPFProtocolsCheckEBPFPrerequisites(t *testing.T) {
	for protocol, spec := range traceProtocols {
		if (spec.prerequisites == nil) != (protocol == "arp" || protocol == "ndp") {
			t.Fatalf("trace %s prerequisites set = %t", protocol, spec.prerequisites != nil)
		}
	}
}

func TestARPSnapshotChangesFindNewChangedAndGoneEntries(t *testing.T) {
	previous := map[neighborKey]traceNeighbor{
		{iface: "en0", ip: "192.0.2.1"}: {iface: "en0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "COMPLETE"},
		{iface: "en0", ip: "192.0.2.2"}: {iface: "en0", ip: "192.0.2.2", state: "INCOMPLETE"},
		{iface: "en0", ip: "192.0.2.3"}: {iface: "en0", ip: "192.0.2.3", mac: "02:00:00:00:00:03", state: "COMPLETE"},
	}
	current := map[neighborKey]traceNeighbor{
		{iface: "en0", ip: "192.0.2.1"}: {iface: "en0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "COMPLETE"},
		{iface: "en0", ip: "192.0.2.2"}: {iface: "en0", ip: "192.0.2.2", mac: "02:00:00:00:00:02", state: "COMPLETE"},
		{iface: "en1", ip: "192.0.2.4"}: {iface: "en1", ip: "192.0.2.4", mac: "02:00:00:00:00:04", state: "COMPLETE"},
	}
	changes := neighborSnapshotChanges(previous, current)
	got := make([]string, 0, len(changes))
	for _, change := range changes {
		got = append(got, change.iface+" "+change.ip+" "+map[bool]string{true: "deleted", false: change.state}[change.deleted])
	}
	want := []string{"en0 192.0.2.2 COMPLETE", "en0 192.0.2.3 deleted", "en1 192.0.2.4 COMPLETE"}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	// 바뀌지 않은 FAILED는 다시 넘기지 않는다. table을 다시 읽을 때마다 실패로 세면 안 된다.
	failed := map[neighborKey]traceNeighbor{{iface: "en0", ip: "192.0.2.9"}: {iface: "en0", ip: "192.0.2.9", state: traceNeighborFailedState}}
	if changes := neighborSnapshotChanges(failed, failed); len(changes) != 0 {
		t.Fatalf("unchanged failed entry = %#v", changes)
	}
}

// NDP는 ARP와 같은 tracker를 쓰고 event 이름과 요약 제목만 다르다.
func TestNDPEventsUseTheirOwnNames(t *testing.T) {
	tracker := newNeighborTracker("ndp")
	tracker.baseline(traceNeighbor{iface: "eth0", ip: "fe80::1", mac: "02:00:00:00:00:01", state: "REACHABLE"})
	event, ok := tracker.event(traceNeighbor{iface: "eth0", ip: "fe80::1", mac: "02:00:00:00:00:02", state: "REACHABLE"}, 1, 1)
	if !ok || event.Event != "ndp_mac_change" || event.Protocol != "ndp" || event.OldMAC != "02:00:00:00:00:01" {
		t.Fatalf("ndp event = %#v, %t", event, ok)
	}
	if traceNeighborKind(event.Event) != traceNeighborMACChange {
		t.Fatalf("kind of %q = %q", event.Event, traceNeighborKind(event.Event))
	}
	summarizer := newNeighborTraceSummarizer("ndp")
	summarizer.observe(event)
	report := summarizer.summarize(captureSummary{}, time.Second).(neighborTraceReport)
	if report.protocol != "ndp" || report.MACChanges != 1 {
		t.Fatalf("ndp report = %#v", report)
	}
	groups := summarizeTraceGroups("ndp", traceGroupByTarget, []captureEvent{event}, captureSummary{}, time.Second, "", "")
	if len(groups.Groups) != 1 || groups.Groups[0].Neighbor == nil || groups.Groups[0].Neighbor.MACChanges != 1 {
		t.Fatalf("ndp groups = %#v", groups.Groups)
	}
}
