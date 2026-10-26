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
	"testing"
	"time"
)

// checkTestRemoteReport는 원격 edc check --json -의 출력에서 표가 읽는 부분만 담은 보고서다.
const checkTestRemoteReport = `{
  "tool": {"name": "edc", "version": "0.68.0"},
  "results": [
    {"probe": "check.service", "status": "fail", "summary": "1 failed units: nginx.service", "next": "edc svc nginx.service"},
    {"probe": "check.process", "status": "warn", "summary": "1 defunct processes, by parent: python3(812) ×1", "next": "edc top"},
    {"probe": "check.memory", "status": "pass", "summary": "available 1 GB of 2 GB (50%)"}
  ]
}`

const checkTestMacReport = `{"tool": {"name": "edc", "version": "0.68.0"}, "results": [{"probe": "check", "status": "skip", "summary": "edc check runs on Linux hosts only"}]}`

func TestCheckIDsFollowTheEvaluateOrder(t *testing.T) {
	results := evaluateCheck(checkFacts{}, checkTestPolicy)
	if len(results) != len(checkIDs) {
		t.Fatalf("evaluateCheck gives %d results, checkIDs has %d", len(results), len(checkIDs))
	}
	for index, id := range checkIDs {
		if results[index].Probe != "check."+id {
			t.Errorf("column %d = %s, result = %s", index, id, results[index].Probe)
		}
	}
}

func TestCheckHostFromCaptureReadsTheReportEvenWhenCheckFailed(t *testing.T) {
	// 원격 check는 fail이 있으면 1로 끝난다. 그래도 표준 출력의 결과를 써야 한다.
	row := checkHostFromCapture(remoteCapture{Stdout: checkTestRemoteReport, ExitCode: 1, Err: errors.New("exit status 1")})
	if row.Status != checkHostChecked || row.Version != "0.68.0" || len(row.Results) != 3 || row.worst() != StatusFail {
		t.Fatalf("row = %+v", row)
	}
}

func TestCheckHostFromCaptureNamesTheReasonWithoutAReport(t *testing.T) {
	for _, test := range []struct {
		capture remoteCapture
		want    string
	}{
		{remoteCapture{TimedOut: true, ExitCode: -1, Err: errors.New("killed")}, checkHostTimeout},
		{remoteCapture{Stderr: "ssh: connect to host beta port 22: Connection refused", ExitCode: 255, Err: errors.New("exit")}, checkHostUnreachable},
		{remoteCapture{Stderr: "bash: line 1: edc: command not found", ExitCode: 127, Err: errors.New("exit")}, checkHostMissing},
		{remoteCapture{Stderr: "unknown command: check\nrun edc help to see the command list", ExitCode: 2, Err: errors.New("exit")}, checkHostUnsupported},
		{remoteCapture{Stdout: "hello"}, checkHostInvalid},
		{remoteCapture{Stdout: `{"results": [{"probe": "check.service", "status": "pass"}]}`}, checkHostInvalid},
		{remoteCapture{Stdout: `{"tool": {"name": "edc"}, "results": [{"probe": "clean.scan", "status": "pass"}]}`}, checkHostInvalid},
		{remoteCapture{Stderr: "boom", ExitCode: 3, Err: errors.New("exit")}, checkHostFailed},
	} {
		if row := checkHostFromCapture(test.capture); row.Status != test.want {
			t.Errorf("%+v: status %q, want %q", test.capture, row.Status, test.want)
		}
	}
}

func TestCheckGroupResultTakesTheWorstHost(t *testing.T) {
	pass := checkHostRow{Status: checkHostChecked, Results: []checkRemoteResult{{Probe: "check.memory", Status: "pass"}}}
	warn := checkHostRow{Status: checkHostChecked, Results: []checkRemoteResult{{Probe: "check.reboot", Status: "warn"}}}
	fail := checkHostRow{Status: checkHostChecked, Results: []checkRemoteResult{{Probe: "check.service", Status: "fail"}}}
	missing := checkHostRow{Status: checkHostMissing}
	for _, test := range []struct {
		rows []checkHostRow
		want Status
	}{
		{[]checkHostRow{pass, pass}, StatusPass},
		{[]checkHostRow{pass, warn}, StatusWarn},
		{[]checkHostRow{pass, missing}, StatusWarn},
		{[]checkHostRow{warn, fail, missing}, StatusFail},
	} {
		if result := checkGroupResult(test.rows, time.Now()); result.Status != test.want {
			t.Errorf("%d rows: %s, want %s (%s)", len(test.rows), result.Status, test.want, result.Summary)
		}
	}
}

func TestFormatCheckGroupShowsTheTableAndTheFindings(t *testing.T) {
	rows := []checkHostRow{
		checkHostFromCapture(remoteCapture{Stdout: checkTestRemoteReport}),
		checkHostFromCapture(remoteCapture{Stdout: checkTestMacReport}),
		{Status: checkHostUnsupported},
	}
	rows[0].Host, rows[1].Host, rows[2].Host = "alpha", "mac", "old"
	text := formatCheckGroup(rows, 0)
	for _, want := range []string{
		"service", "filehandles",
		"alpha", "FAIL", "WARN", "—",
		"mac    SKIP  edc check runs on Linux hosts only",
		"old    this edc has no check, run edc update",
		"alpha  FAIL  service      1 failed units: nginx.service",
		"next: edc svc nginx.service",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "available 1 GB") {
		t.Errorf("a passing check reached the findings:\n%s", text)
	}
}

func checkTestInventory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, []byte("hosts:\n  - name: alpha\n  - name: beta\n    target: beta.example\ngroups:\n  servers: [alpha, beta]\nparallel: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCheckGroupReportsEveryHostAsJSONAndText(t *testing.T) {
	inventory := checkTestInventory(t)
	var mu sync.Mutex
	var commands []string
	run := func(_ context.Context, target, command string, timeout time.Duration) remoteCapture {
		// 호스트는 parallel 2로 동시에 돈다.
		mu.Lock()
		commands = append(commands, command)
		mu.Unlock()
		if timeout != 15*time.Second+checkGroupSSHMargin {
			t.Errorf("timeout = %s", timeout)
		}
		if target == "beta.example" {
			return remoteCapture{Stderr: "edc: command not found", ExitCode: 127, Err: errors.New("exit")}
		}
		return remoteCapture{Stdout: checkTestRemoteReport, ExitCode: 1, Err: errors.New("exit status 1")}
	}
	options := configuredCommon(15 * time.Second)
	options.jsonPath = filepath.Join(t.TempDir(), "out.json")
	var output bytes.Buffer
	if code := runCheckGroup(options, "servers", inventory, "test", run, &output); code != 1 {
		t.Fatalf("exit code = %d, want 1 for a failing host", code)
	}
	if len(commands) != 2 || commands[0] != "edc check --json - --timeout 15s" {
		t.Fatalf("commands = %q", commands)
	}
	data, err := os.ReadFile(options.jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Probe   string `json:"probe"`
			Status  string `json:"status"`
			Metrics struct {
				Hosts []checkHostRow `json:"hosts"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	result := report.Results[0]
	if result.Probe != checkGroupProbeID || result.Status != "fail" || len(result.Metrics.Hosts) != 2 ||
		result.Metrics.Hosts[0].Results[0].Next != "edc svc nginx.service" || result.Metrics.Hosts[1].Status != checkHostMissing {
		t.Fatalf("report = %+v", result)
	}

	options.jsonPath = ""
	output.Reset()
	runCheckGroup(options, "servers", inventory, "test", run, &output)
	for _, want := range []string{"check.group", "2 hosts, 1 checked, 1 without a result; FAIL on 1, WARN on 0", "beta   edc not found"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, output.String())
		}
	}
}

func TestRunCheckGroupRejectsWrongUse(t *testing.T) {
	inventory := checkTestInventory(t)
	run := func(context.Context, string, string, time.Duration) remoteCapture {
		t.Fatal("a wrong use reached a host")
		return remoteCapture{}
	}
	options := configuredCommon(15 * time.Second)
	for _, test := range []struct{ group, inventory string }{
		{"nobody", inventory},
		{"servers", filepath.Join(t.TempDir(), "missing.yaml")},
	} {
		if code := runCheckGroup(options, test.group, test.inventory, "test", run, &bytes.Buffer{}); code != 2 {
			t.Errorf("%+v: exit code %d, want 2", test, code)
		}
	}
	if code := runCheck([]string{"--inventory", inventory}, "test"); code != 2 {
		t.Errorf("--inventory without --group: exit code %d, want 2", code)
	}
}
