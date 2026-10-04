package edc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestTopProcessLimitJSONPreservesZeroAndUnavailableStates(t *testing.T) {
	zero, soft := 0, uint64(1024)
	limits := &topProcessLimits{
		FD: topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &zero, Soft: &soft},
		Cgroup: topCgroupLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: "/workers", Key: "private identity",
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "permission_denied", Reason: "permission denied"}},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: new(uint64), OOMKill: new(uint64)},
			CPU:    topCgroupCPU{topLimitStatus: topLimitStatus{Status: "baseline", Reason: "waiting for a CPU throttling baseline"}},
		},
	}
	data, err := json.Marshal(newTopProcessSamples([]topProcess{{PID: 1, Limits: limits}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"limits":`, `"used":0`, `"soft_limit":1024`, `"status":"permission_denied"`, `"oom":0`, `"oom_kill":0`, `"status":"baseline"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s: %s", want, data)
		}
	}
	for _, absent := range []string{"private identity", "used_bytes", "local_max_bytes", "periods", "throttled_usec"} {
		if strings.Contains(string(data), absent) {
			t.Fatalf("unavailable/internal value %s appeared: %s", absent, data)
		}
	}
}

func TestTopProcessLimitPanelUsesTheSelectedHistoryAndFitsTheScreen(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	model := topFixtureModel(nil)
	model.processFilter, model.follow, model.detail, model.height = filter, false, true, 40
	oldFD, newFD, soft := 4, 9, uint64(1024)
	oldLimits := &topProcessLimits{FD: topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &oldFD, Soft: &soft}, Cgroup: topCgroupLimits{topLimitStatus: topLimitStatus{Status: "unsupported", Reason: "cgroup v1"}}}
	newLimits := &topProcessLimits{FD: topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &newFD, Soft: &soft}, Cgroup: oldLimits.Cgroup}
	for index, limits := range []*topProcessLimits{oldLimits, newLimits} {
		model.rows = append(model.rows, topDashboardRow{at: time.Unix(int64(index), 0), processes: []topProcess{{PID: 1, Command: "worker", Limits: limits}}, processTotal: topProcessTotal{Count: 1}, processesValid: true, filter: filter.String()})
	}
	model.selected = 0
	view := model.View().Content
	if !strings.Contains(view, "pid 1 · fd 4/1024") || strings.Contains(view, "fd 9/1024") {
		t.Fatalf("selected history mixed with live sample: %s", view)
	}
	for _, width := range []int{30, 80} {
		model.width = width
		for _, line := range strings.Split(model.View().Content, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("line exceeds width %d: %q", width, line)
			}
		}
	}
}

func TestTopProcessLimitLinesDoNotSumSharedCgroups(t *testing.T) {
	used, maximum, oom, killed, periods, throttled, usec := uint64(128<<20), uint64(512<<20), uint64(2), uint64(1), uint64(10), uint64(3), uint64(4000)
	fd, soft, window := 4, uint64(1024), float64(2)
	limits := &topProcessLimits{
		FD: topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &fd, Soft: &soft},
		Cgroup: topCgroupLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: "/workers", Key: "same group",
			Memory: topCgroupMemory{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Max: &maximum},
			Events: topCgroupEvents{topLimitStatus: topLimitStatus{Status: "available"}, OOM: &oom, OOMKill: &killed},
			CPU:    topCgroupCPU{topLimitStatus: topLimitStatus{Status: "available"}, WindowS: &window, Periods: &periods, ThrottledPeriods: &throttled, ThrottledUS: &usec},
		},
	}
	lines := strings.Join(topProcessLimitLines([]topProcess{{PID: 1, Limits: limits}, {PID: 2, Limits: limits}}), "\n")
	for _, want := range []string{"pid 1 · fd 4/1024", "pid 2 · fd 4/1024", "group scope", "memory 128.0M · local max 512.0M", "local OOM 2 · OOM kill 1 · cumulative", "cpu throttle 3/10 periods · 4.00ms · window 2.00s"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q: %s", want, lines)
		}
	}
	if strings.Count(lines, "group scope") != 1 {
		t.Fatalf("duplicate group: %s", lines)
	}
}

func TestTopProcessLimitLinesReportUnlimitedAndPermissionFailure(t *testing.T) {
	used := 0
	limits := &topProcessLimits{FD: topFDLimit{topLimitStatus: topLimitStatus{Status: "available"}, Used: &used, Unlimited: true}, Cgroup: topCgroupLimits{topLimitStatus: topLimitStatus{Status: "permission_denied", Reason: "permission denied"}}}
	lines := strings.Join(topProcessLimitLines([]topProcess{{PID: 1, Limits: limits}}), "\n")
	if !strings.Contains(lines, "fd 0/unlimited") || !strings.Contains(lines, "cgroup · permission_denied · permission denied") {
		t.Fatalf("lines=%s", lines)
	}
	if strings.Contains(lines, "memory 0") || strings.Contains(lines, "throttle 0") {
		t.Fatalf("unknown must not be zero: %s", lines)
	}
}
