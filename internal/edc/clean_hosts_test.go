package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cleanTestRemoteReport는 원격 edc clean --json -의 출력에서 표가 읽는 부분만 담은 보고서다.
const cleanTestRemoteReport = `{
  "schema_version": "1.0",
  "tool": {"name": "edc", "version": "0.57.0"},
  "results": [{
    "probe": "clean.scan", "status": "warn",
    "metrics": {
      "candidates": [
        {"id": "go-build", "status": "measured", "usage_bytes": 3000000000, "regenerable": true},
        {"id": "journald", "status": "measured", "usage_bytes": 4000000000},
        {"id": "npm", "status": "absent"}
      ],
      "filesystems": [
        {"mount": "/app", "size_bytes": 300000000000, "used_bytes": 200000000000, "usage_bytes": 1},
        {"mount": "/", "size_bytes": 49000000000, "used_bytes": 39000000000, "usage_bytes": 7000000000}
      ],
      "docker_root": {"status": "measured", "roots": [{"path": "/var/lib/docker", "usage_bytes": 15200000000}]},
      "system": {"status": "measured", "large_logs": [{"bytes": 450000000}, {"bytes": 130000000}], "large_logs_omitted_bytes": 20000000}
    }
  }]
}`

// cleanTestOldRemoteReport는 docker_root와 system을 모르는 이전 버전 edc의 보고서다.
const cleanTestOldRemoteReport = `{"tool": {"version": "0.50.0"}, "results": [{"status": "pass", "metrics": {
  "candidates": [{"id": "pip", "status": "measured", "usage_bytes": 1000}],
  "filesystems": [{"mount": "/home", "size_bytes": 100, "used_bytes": 50, "usage_bytes": 1000}]}}]}`

func TestCleanHostFromCaptureReadsTheRemoteReport(t *testing.T) {
	row := cleanHostFromCapture(remoteCapture{Stdout: cleanTestRemoteReport, Duration: 2 * time.Second})
	if row.Status != cleanHostMeasured || row.ResultStatus != "warn" || row.Version != "0.57.0" || row.DurationMS != 2000 {
		t.Fatalf("row = %+v", row)
	}
	if row.CandidateBytes == nil || *row.CandidateBytes != 7000000000 {
		t.Errorf("candidates = %v", row.CandidateBytes)
	}
	if row.DiskMount != "/" || *row.DiskUsedBytes != 39000000000 || *row.DiskSizeBytes != 49000000000 {
		t.Errorf("disk = %s %v/%v", row.DiskMount, row.DiskUsedBytes, row.DiskSizeBytes)
	}
	if row.OldDockerBytes == nil || *row.OldDockerBytes != 15200000000 || row.LargestName != "/var/lib/docker" || row.LargestBytes != 15200000000 {
		t.Errorf("old docker %v largest %s %d", row.OldDockerBytes, row.LargestName, row.LargestBytes)
	}
	if row.LogBytes == nil || *row.LogBytes != 600000000 {
		t.Errorf("logs = %v", row.LogBytes)
	}

	// 원격 clean이 FAIL을 내도 보고서를 읽을 수 있으면 결과로 쓴다.
	failed := cleanHostFromCapture(remoteCapture{Stdout: strings.Replace(cleanTestRemoteReport, `"warn"`, `"fail"`, 1), ExitCode: 1, Err: errors.New("exit status 1")})
	if failed.Status != cleanHostMeasured || failed.ResultStatus != "fail" {
		t.Errorf("a FAIL report = %+v", failed)
	}

	old := cleanHostFromCapture(remoteCapture{Stdout: cleanTestOldRemoteReport})
	if old.Status != cleanHostMeasured || old.OldDockerBytes != nil || old.LogBytes != nil || old.DiskMount != "/home" || old.LargestName != "pip cache" {
		t.Errorf("an older edc = %+v", old)
	}
}

func TestCleanHostFromCaptureNamesTheReasonWithoutAReport(t *testing.T) {
	exitErr := errors.New("exit status")
	for _, test := range []struct {
		name    string
		capture remoteCapture
		status  string
		detail  string
	}{
		{"edc missing", remoteCapture{Stderr: "bash: line 1: edc: command not found\n", ExitCode: 127, Err: exitErr}, cleanHostMissing, ""},
		{"edc missing in sh", remoteCapture{Stderr: "sh: 1: edc: not found\n", ExitCode: 2, Err: exitErr}, cleanHostMissing, ""},
		{"ssh failed", remoteCapture{Stderr: "ssh: connect to host h port 22: Operation timed out\n", ExitCode: 255, Err: exitErr}, cleanHostUnreachable, "ssh: connect to host h port 22: Operation timed out"},
		{"timeout", remoteCapture{TimedOut: true, ExitCode: -1, Err: exitErr}, cleanHostTimeout, ""},
		{"truncated", remoteCapture{Stdout: `{"tool":`, Truncated: true}, cleanHostInvalid, "output over 16.0 MB"},
		{"not json", remoteCapture{Stdout: "usage: edc clean [options]\nmore"}, cleanHostInvalid, "usage: edc clean [options]"},
		{"other failure", remoteCapture{Stderr: "permission denied\n", ExitCode: 3, Err: exitErr}, cleanHostFailed, "permission denied"},
		{"other failure without stderr", remoteCapture{ExitCode: 3, Err: exitErr}, cleanHostFailed, "exit 3"},
	} {
		row := cleanHostFromCapture(test.capture)
		if row.Status != test.status || row.Detail != test.detail {
			t.Errorf("%s: status %q detail %q, want %q %q", test.name, row.Status, row.Detail, test.status, test.detail)
		}
	}
}

func TestCleanCollectHostsKeepsTheOrderAndTheParallelLimit(t *testing.T) {
	var running, peak atomic.Int32
	var mu sync.Mutex
	commands := map[string]string{}
	run := func(ctx context.Context, target, command string, timeout time.Duration) remoteCapture {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		mu.Lock()
		commands[target] = command
		mu.Unlock()
		if timeout != time.Minute+cleanGroupSSHMargin {
			t.Errorf("ssh timeout = %s", timeout)
		}
		if target == "b.example" {
			return remoteCapture{Stderr: "edc: command not found", ExitCode: 127, Err: errors.New("exit")}
		}
		return remoteCapture{Stdout: cleanTestRemoteReport}
	}
	hosts := []remoteHost{{Name: "a"}, {Name: "b", Target: "b.example"}, {Name: "c"}, {Name: "d"}, {Name: "e"}}
	rows := cleanCollectHosts(context.Background(), hosts, 2, time.Minute, run)
	if peak.Load() > 2 {
		t.Errorf("%d hosts ran at once, want at most 2", peak.Load())
	}
	for index, want := range []string{"a", "b", "c", "d", "e"} {
		if rows[index].Host != want {
			t.Fatalf("row %d is %s, want %s", index, rows[index].Host, want)
		}
	}
	if rows[1].Target != "b.example" || rows[1].Status != cleanHostMissing || rows[0].Target != "a" || rows[0].Status != cleanHostMeasured {
		t.Errorf("rows = %+v", rows[:2])
	}
	if got := commands["a"]; got != "edc clean --json - --timeout 1m0s" {
		t.Errorf("remote command = %q", got)
	}
}

func TestFormatCleanGroupShowsOneRowPerHost(t *testing.T) {
	rows := []cleanHostRow{
		cleanHostFromCapture(remoteCapture{Stdout: cleanTestRemoteReport}),
		cleanHostFromCapture(remoteCapture{Stdout: cleanTestOldRemoteReport}),
		cleanHostFromCapture(remoteCapture{Stderr: "ssh: Could not resolve hostname x", ExitCode: 255, Err: errors.New("exit")}),
	}
	rows[0].Host, rows[1].Host, rows[2].Host = "server-a", "old-host", "gone"
	text := formatCleanGroup(rows, 0)
	for _, want := range []string{"host", "disk used", "server-a", "36.3 GB / 45.6 GB", "6.5 GB", "14.2 GB", "572.2 MB", "/var/lib/docker 14.2 GB",
		"old-host", "—", "gone", "SSH failed  ssh: Could not resolve hostname x", "edc versions differ between hosts: 0.50.0, 0.57.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("table lacks %q:\n%s", want, text)
		}
	}
}

func cleanTestInventory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	cleanTestWriteFile(t, path, "hosts:\n  - name: alpha\n  - name: beta\n    target: beta.example\ngroups:\n  servers: [alpha, beta]\nparallel: 2\n", time.Now())
	return path
}

func TestRunCleanGroupReportsEveryHostAsJSONAndText(t *testing.T) {
	inventory := cleanTestInventory(t)
	deps := cleanRunDeps(t, cleanTestHome(t))
	deps.remote = func(ctx context.Context, target, command string, timeout time.Duration) remoteCapture {
		if target == "beta.example" {
			return remoteCapture{Stderr: "edc: command not found", ExitCode: 127, Err: errors.New("exit")}
		}
		return remoteCapture{Stdout: cleanTestRemoteReport}
	}
	path := filepath.Join(t.TempDir(), "out.json")
	var output bytes.Buffer
	if code := runCleanWith([]string{"--group", "servers", "--inventory", inventory, "--json", path}, "test", deps, true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Probe   string `json:"probe"`
			Status  string `json:"status"`
			Metrics struct {
				Hosts []cleanHostRow `json:"hosts"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if result.Probe != cleanGroupProbeID || result.Status != "warn" || len(result.Metrics.Hosts) != 2 ||
		result.Metrics.Hosts[0].Status != cleanHostMeasured || result.Metrics.Hosts[1].Status != cleanHostMissing {
		t.Fatalf("report = %+v", result)
	}

	output.Reset()
	if code := runCleanWith([]string{"--group", "servers", "--inventory", inventory}, "test", deps, true, nil, &output); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	for _, want := range []string{T("clean.advisory"), "clean.group", "2 hosts, 1 measured, 1 without a result", "alpha", "beta", "edc not found, install it with install.sh"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, output.String())
		}
	}
}

func TestRunCleanGroupRejectsWrongUse(t *testing.T) {
	inventory := cleanTestInventory(t)
	deps := cleanRunDeps(t, cleanTestHome(t))
	deps.remote = func(context.Context, string, string, time.Duration) remoteCapture {
		t.Fatal("a wrong use reached a host")
		return remoteCapture{}
	}
	for _, args := range [][]string{
		{"--inventory", inventory},
		{"--group", "servers", "--inventory", inventory, "--project", t.TempDir()},
		{"--group", "nobody", "--inventory", inventory},
		{"--group", "servers", "--inventory", filepath.Join(t.TempDir(), "missing.yaml")},
	} {
		var output bytes.Buffer
		if code := runCleanWith(args, "test", deps, true, nil, &output); code != 2 {
			t.Errorf("%q: exit code %d, want 2", args, code)
		}
	}
}
