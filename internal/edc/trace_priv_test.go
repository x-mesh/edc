package edc

import (
	"strings"
	"testing"
)

func TestPrivCapabilityNames(t *testing.T) {
	for cap, want := range map[int32]string{0: "CAP_CHOWN", 21: "CAP_SYS_ADMIN", 23: "CAP_SYS_NICE", 38: "CAP_PERFMON", 39: "CAP_BPF", 40: "CAP_CHECKPOINT_RESTORE", 41: "CAP_41", -1: "CAP_-1"} {
		if got := privCapabilityName(cap); got != want {
			t.Errorf("cap %d = %q, want %q", cap, got, want)
		}
	}
}

func TestPrivReportOrdersDeniedFirstAndSplitsEvents(t *testing.T) {
	uid, euid := [2]uint32{1000, 1000}, [2]uint32{1000, 0}
	collection := privCollection{samples: []privSample{
		{Process: "api", Cap: 21, Checks: 50},
		{Process: "api", Cap: 21, Denied: true, NoAudit: true, Checks: 400},
		{Process: "nginx", Cap: 10, Denied: true, Checks: 3},
		{Process: "nginx", Cap: 10, Denied: true, Checks: 2},
	}, events: []privEvent{
		{Event: privEventCap, Process: "nginx", PID: 7, TID: 7, Capability: "CAP_NET_BIND_SERVICE"},
		{Event: privEventCred, Process: "sudo", PID: 9, TID: 9, UID: &uid, EUID: &euid},
		{Event: privEventMod, Process: "modprobe", PID: 11, TID: 11, Module: "dummy"},
	}}
	report := privReportFor(collection, false)
	if report.Summary.Checks != 455 || report.Summary.Denied != 5 || report.Summary.StatsEntries != 4 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	first, second, last := report.Capabilities[0], report.Capabilities[1], report.Capabilities[2]
	if first.Process != "nginx" || first.Capability != "CAP_NET_BIND_SERVICE" || first.Result != "denied" || first.NoAudit || first.Checks != 5 {
		t.Fatalf("first = %+v", first)
	}
	if second.Result != "denied" || !second.NoAudit || second.Checks != 400 || last.Result != "allowed" || last.Checks != 50 {
		t.Fatalf("rows = %+v", report.Capabilities)
	}
	if len(report.Denied) != 1 || len(report.Credentials) != 1 || len(report.Modules) != 1 {
		t.Fatalf("events = %+v", report)
	}
	if got := report.Credentials[0].change(); got != "euid 1000→0" {
		t.Fatalf("change = %q", got)
	}
	var out strings.Builder
	report.print(&out)
	for _, want := range []string{"PRIV TRACE", "checks 455  denied 5", "denied (noaudit)", "DENIED", "denied  CAP_NET_BIND_SERVICE", "CREDENTIALS", "cred  euid 1000→0", "MODULES", "module  dummy"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output misses %q:\n%s", want, out.String())
		}
	}
	if only := privReportFor(collection, true); len(only.Capabilities) != 2 || only.Summary.Checks != 455 {
		t.Fatalf("denied only = %+v", only.Capabilities)
	}
}
