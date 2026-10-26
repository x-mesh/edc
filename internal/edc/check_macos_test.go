package edc

import (
	"errors"
	"strings"
	"testing"
)

func TestParseLaunchctlListKeepsRunningAndFailedJobs(t *testing.T) {
	output := "PID\tStatus\tLabel\n" +
		"412\t0\tcom.apple.Finder\n" +
		"-\t0\tcom.apple.SafariHistoryServiceAgent\n" +
		"-\t-9\tcom.apple.progressd\n" +
		"-\t78\tcom.example.supervisor\n"
	items := parseLaunchctlList(output)
	if len(items) != 2 || items[0].Unit != "com.apple.Finder" || items[0].ActiveState != "active" {
		t.Fatalf("items = %+v", items)
	}
	// SIGKILL로 정리된 on-demand agent(-9)와 일을 마친 job(0)은 실패가 아니다.
	if !items[1].failed() || items[1].Unit != "com.example.supervisor" || items[1].SubState != "exit 78" {
		t.Fatalf("failed job = %+v", items[1])
	}
	facts := checkFacts{Services: items, ServiceNext: "launchctl list %s"}
	if result := checkServices(facts, checkTestPolicy); result.Status != StatusFail || result.Next != "launchctl list com.example.supervisor" {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseCheckPSReadsZombiesWithSpacesInThePath(t *testing.T) {
	output := "    1     0 Ss   /sbin/launchd\n" +
		" 1284     1 S    /System/Applications/Utilities/Screen Sharing.app/Contents/MacOS/Screen Sharing\n" +
		" 2001  1284 Z    <defunct>\n" +
		" 3000     1 U    /usr/sbin/diskarbitrationd\n"
	processes := parseCheckPS(output)
	if len(processes) != 4 || processes[1].Command != "Screen Sharing" || processes[2].State != "Z" || processes[2].PPID != 1284 {
		t.Fatalf("processes = %+v", processes)
	}
	result := checkProcesses(checkFacts{Processes: processes})
	// macOS의 U(uninterruptible wait)는 Linux의 D와 뜻이 달라 판정하지 않는다.
	if result.Status != StatusWarn || !strings.Contains(result.Summary, "Screen Sharing(1284) ×1") || strings.Contains(result.Summary, "diskarbitrationd") {
		t.Fatalf("result = %+v", result)
	}
}

func TestParseCheckRouteGetTellsNoRouteFromAFailure(t *testing.T) {
	output := "   route to: default\ndestination: default\n    gateway: 192.168.1.1\n  interface: en0\n"
	if route, err := parseCheckRouteGet(output, "", nil); err != nil || route != "192.168.1.1 dev en0" {
		t.Fatalf("route = %q, %v", route, err)
	}
	if route, err := parseCheckRouteGet("", "route: writing to routing socket: not in table\n", errors.New("exit status 1")); err != nil || route != "" {
		t.Fatalf("no route = %q, %v", route, err)
	}
	if _, err := parseCheckRouteGet("", "route: permission denied\n", errors.New("exit status 1")); err == nil {
		t.Fatalf("a route failure must not look like a missing route")
	}
}

func TestCheckMemoryUsesThePressureLevelWhenKnown(t *testing.T) {
	low := infoMemory{Total: 100, Available: 2}
	if result := checkMemory(checkFacts{Memory: low, MemoryPressure: topMemoryPressureNormal}, checkTestPolicy); result.Status != StatusPass {
		t.Fatalf("normal pressure with little free memory = %+v", result)
	}
	if result := checkMemory(checkFacts{Memory: low, MemoryPressure: topMemoryPressureWarn}, checkTestPolicy); result.Status != StatusWarn {
		t.Fatalf("warn pressure = %+v", result)
	}
	if result := checkMemory(checkFacts{Memory: low, MemoryPressure: topMemoryPressureCritical}, checkTestPolicy); result.Status != StatusFail {
		t.Fatalf("critical pressure = %+v", result)
	}
}

func TestCollectCheckFactsSkipsChecksWithoutASource(t *testing.T) {
	sources := checkTestSources()
	sources.kernel, sources.clock, sources.reboot = nil, nil, nil
	facts := collectCheckFacts(t.Context(), sources, nil)
	results := evaluateCheck(facts, checkTestPolicy)
	for _, result := range results {
		switch result.Probe {
		case "check.kernel", "check.time", "check.reboot":
			if result.Status != StatusSkip || result.Summary != T("check.skip.unsupported_os") {
				t.Errorf("%s = %+v", result.Probe, result)
			}
		}
	}
}
