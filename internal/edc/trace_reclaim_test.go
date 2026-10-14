package edc

import (
	"strings"
	"testing"
)

func TestReclaimReportGroupsAndOrdersByTotal(t *testing.T) {
	var fast, slow topProbeHist
	fast[3], slow[10] = 4, 1
	collection := reclaimCollection{samples: []reclaimSample{
		{Process: "api", CgroupID: 7, Kind: "memcg", Count: 4, SumNS: 40_000, MaxNS: 15_000, Reclaimed: 256, Hist: fast},
		{Process: "api", CgroupID: 7, Kind: "throttle", Count: 1, SumNS: 600_000_000, MaxNS: 600_000_000, Hist: slow},
		{Process: "db", CgroupID: 8, Kind: "direct", Count: 2, SumNS: 2_000_000, MaxNS: 1_500_000, Reclaimed: 10},
	}, summary: reclaimSummary{DurationMS: 1000}}
	report := reclaimReportFor(collection, "", 4096)
	if report.Summary.Stalls != 7 || report.Summary.StatsEntries != 3 || report.Summary.TotalMS != 602.04 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	first := report.Rows[0]
	if first.Process != "api" || first.Kind != "throttle" || first.TotalMS != 600 || first.ReclaimedBytes != 0 || first.P95MS == nil {
		t.Fatalf("first row = %+v", first)
	}
	memcg := report.Rows[2]
	if memcg.Kind != "memcg" || memcg.Stalls != 4 || memcg.AverageMS != 0.01 || memcg.MaxMS != 0.015 || memcg.ReclaimedBytes != 256*4096 {
		t.Fatalf("memcg row = %+v", memcg)
	}
	byProcess := reclaimReportFor(collection, traceGroupByProcess, 4096)
	if len(byProcess.Rows) != 2 || byProcess.Rows[0].Process != "api" || byProcess.Rows[0].Stalls != 5 || byProcess.Rows[0].MaxMS != 600 || byProcess.Rows[0].ReclaimedBytes != 256*4096 {
		t.Fatalf("by process = %+v", byProcess.Rows)
	}
	byCgroup := reclaimReportFor(collection, traceGroupByCgroup, 4096)
	if len(byCgroup.Rows) != 2 || *byCgroup.Rows[0].CgroupID != 7 || byCgroup.Rows[0].cells(traceGroupByCgroup)[0] != "7" {
		t.Fatalf("by cgroup = %+v", byCgroup.Rows)
	}
	byKind := reclaimReportFor(collection, reclaimGroupByKind, 4096)
	if len(byKind.Rows) != 3 || byKind.Rows[0].Kind != "throttle" || byKind.Rows[0].Process != "" {
		t.Fatalf("by kind = %+v", byKind.Rows)
	}
	var out strings.Builder
	report.print(&out)
	for _, want := range []string{"RECLAIM TRACE", "stalls 7", "api      throttle", "1.0MiB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output misses %q:\n%s", want, out.String())
		}
	}
}

func TestReclaimKindNamesAndEventLine(t *testing.T) {
	for kind, want := range map[uint32]string{0: "direct", 1: "memcg", 2: "throttle", 3: "compact", 9: "kind_9"} {
		if got := reclaimKindName(kind); got != want {
			t.Errorf("kind %d = %q, want %q", kind, got, want)
		}
	}
	reclaimed := uint64(2 << 20)
	line := reclaimEvent{Process: "api", PID: 10, TID: 11, Kind: "memcg", LatencyMS: 12.5, Reclaimed: &reclaimed, CgroupID: 7}.line()
	if !strings.Contains(line, "api 10/11  memcg  12.50ms  reclaimed 2.0MiB  cgroup 7") {
		t.Fatalf("line = %q", line)
	}
	if line := (reclaimEvent{Process: "api", Kind: "throttle", LatencyMS: 600}).line(); strings.Contains(line, "reclaimed") {
		t.Fatalf("throttle line = %q", line)
	}
}
