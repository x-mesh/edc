package edc

import (
	"strings"
	"testing"
	"time"
)

func TestSelectProcByPIDAndName(t *testing.T) {
	processes := []topProcess{
		{PID: 10, Command: "/usr/sbin/nginx -g daemon off;"},
		{PID: 11, Command: "nginx"},
		{PID: 20, Command: "postgres"},
	}
	if process, _, err := selectProc(processes, "20"); err != nil || process.PID != 20 {
		t.Fatalf("pid = %+v, %v", process, err)
	}
	if process, _, err := selectProc(processes, "postgres"); err != nil || process.PID != 20 {
		t.Fatalf("name = %+v, %v", process, err)
	}
	// 경로와 인자가 붙은 명령도 이름으로 맞춘다. 여럿이면 고르지 않는다.
	if _, matches, err := selectProc(processes, "nginx"); err != nil || len(matches) != 2 || matches[0].PID != 10 {
		t.Fatalf("many = %+v, %v", matches, err)
	}
	for _, target := range []string{"99", "redis"} {
		if _, _, err := selectProc(processes, target); err != errProcNotFound {
			t.Errorf("%s: err = %v", target, err)
		}
	}
}

func TestProcPeerGroupsInboundByAddress(t *testing.T) {
	listen := map[string]bool{"8080": true}
	if got := procPeer("10.0.0.5:8080", "10.0.0.9:51234", listen); got != "10.0.0.9" {
		t.Fatalf("inbound = %s", got)
	}
	if got := procPeer("[::1]:8080", "[::1]:51234", listen); got != "::1" {
		t.Fatalf("inbound v6 = %s", got)
	}
	if got := procPeer("10.0.0.5:40000", "10.0.0.7:5432", listen); got != "10.0.0.7:5432" {
		t.Fatalf("outbound = %s", got)
	}
}

func TestProcResultsJudgeLimitsAndLeaks(t *testing.T) {
	soft := uint64(100)
	process := topProcess{PID: 42, PPID: 1, Command: "app", State: "S", Limits: &topProcessLimits{FD: topFDLimit{Soft: &soft}}}
	details := procDetails{
		Args:    []string{"/opt/app", "--password", "hunter2"},
		FDTypes: map[string]int{"file": 60, "socket": 25},
		Deleted: []procDeletedFile{{Path: "/var/log/app.log", Size: 3 << 20}, {Path: "/tmp/x", Size: 1 << 20}},
		Listen:  []string{"tcp *:8080", "tcp *:8080"},
		Conns:   map[string]int{"ESTABLISHED": 5, "CLOSE_WAIT": 10},
		Remotes: map[string]int{"10.0.0.9": 15},
	}
	results := evaluateProc(process, details, time.Now(), false)
	byID := map[string]Result{}
	for _, result := range results {
		byID[result.Probe] = result
	}
	if command := byID["proc.process"].Evidence[0].Value; strings.Contains(command, "hunter2") {
		t.Fatalf("a secret leaked: %s", command)
	}
	fd := byID["proc.fd"]
	if fd.Status != StatusWarn || !strings.Contains(fd.Summary, "85 of 100") || !strings.Contains(fd.Summary, "2 deleted files still open (4.0MiB)") {
		t.Fatalf("fd = %+v", fd)
	}
	if fd.Evidence[1].Value[:strings.Index(fd.Evidence[1].Value, "\n")] != "  3.0MiB  /var/log/app.log" {
		t.Fatalf("largest deleted file must come first: %q", fd.Evidence[1].Value)
	}
	sockets := byID["proc.sockets"]
	if sockets.Status != StatusWarn || !strings.HasPrefix(sockets.Summary, "listening: tcp *:8080 · 15 connections") {
		t.Fatalf("sockets = %+v", sockets)
	}
	details.Conns["CLOSE_WAIT"] = 9
	if quiet := procSocketsResult(details); quiet.Status != StatusPass {
		t.Fatalf("9 CLOSE_WAIT = %+v", quiet)
	}
}

func TestProcStateAndCgroup(t *testing.T) {
	zombie := procProcessResult(topProcess{PID: 7, PPID: 3, Command: "worker", State: "Z"}, procDetails{}, time.Now(), false)
	if zombie.Status != StatusWarn || zombie.Next != "edc proc 3" {
		t.Fatalf("zombie = %+v", zombie)
	}
	used, max, kills := uint64(95<<20), uint64(100<<20), uint64(2)
	limits := &topProcessLimits{Cgroup: topCgroupLimits{
		topLimitStatus: topLimitStatus{Status: "available"}, Path: "/system.slice/app.service",
		Memory: topCgroupMemory{Used: &used, Max: &max}, Events: topCgroupEvents{OOMKill: &kills},
	}}
	cgroup := procCgroupResult(topProcess{Limits: limits})
	if cgroup.Status != StatusWarn || !strings.Contains(cgroup.Summary, "95.0MiB of 100.0MiB (95%)") || !strings.Contains(cgroup.Summary, "2 OOM kills") {
		t.Fatalf("cgroup = %+v", cgroup)
	}
	if skip := procCgroupResult(topProcess{}); skip.Status != StatusSkip {
		t.Fatalf("no limits = %+v", skip)
	}
}
