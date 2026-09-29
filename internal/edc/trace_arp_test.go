package edc

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestARPTrackerReportsOnlyChanges(t *testing.T) {
	tracker := newARPTracker()
	tracker.baseline(arpNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "REACHABLE"})
	steps := []struct {
		neighbor arpNeighbor
		want     string
	}{
		// trace 전부터 있던 항목의 같은 알림은 event가 아니다.
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "REACHABLE"}, ""},
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:01", state: "STALE"}, traceARPStateEvent},
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", mac: "02:00:00:00:00:02", state: "REACHABLE"}, traceARPMACChangeEvent},
		// 실패 알림에 MAC이 없어도 마지막 MAC을 남긴다. 다시 실패하면 확인을 새로 한 것이라 다시 센다.
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", state: traceARPFailedState}, traceARPFailedEvent},
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", state: traceARPFailedState}, traceARPFailedEvent},
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", deleted: true}, traceARPDeleteEvent},
		{arpNeighbor{iface: "eth0", ip: "192.0.2.1", deleted: true}, ""},
		{arpNeighbor{iface: "eth1", ip: "192.0.2.1", mac: "02:00:00:00:00:09", state: "REACHABLE"}, traceARPNewEvent},
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
	if failed.MAC != "02:00:00:00:00:02" || failed.OldState != "REACHABLE" || failed.NewState != traceARPFailedState {
		t.Fatalf("failed = %#v", failed)
	}
	if deleted.MAC != "02:00:00:00:00:02" || deleted.OldState != traceARPFailedState || deleted.NewState != "" {
		t.Fatalf("deleted = %#v", deleted)
	}
	if destination, label := traceARPScrollLabels(change); destination != "192.0.2.1 (02:00:00:00:00:01 -> 02:00:00:00:00:02)" || label != traceARPMACChangeEvent {
		t.Fatalf("mac change labels = %q, %q", destination, label)
	}
	if _, label := traceARPScrollLabels(events[0]); label != "arp_state STALE" {
		t.Fatalf("state label = %q", label)
	}

	summarizer := newARPTraceSummarizer()
	for _, event := range events {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 1}, time.Second).(arpTraceReport)
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
	if len(groups.Groups) != 1 || groups.Groups[0].Group != "192.0.2.1" || groups.Groups[0].ARP == nil || len(groups.Groups[0].ARP.MACs) != 3 || groups.Groups[0].ARP.MACChanges != 1 || groups.Groups[0].ARP.Failures != 2 {
		t.Fatalf("arp groups = %#v", groups.Groups)
	}
	if views := traceGroupViews("arp"); slices.Contains(views, traceGroupByPort) || slices.Contains(views, traceGroupByProcess) || !slices.Contains(views, traceGroupBySource) {
		t.Fatalf("arp views = %q", views)
	}
}
