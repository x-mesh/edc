package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFindRemoteCommandQuotesValues(t *testing.T) {
	options, err := parseFindOptions([]string{"--group", "web", "-t", "f", "--size", "+100M", "--changed", "1h", "-E", "--layers", "--workers", "2", "--timeout", "30s", "nginx conf", "/etc", "/srv/my app"}, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	want := "edc find --json - --timeout 30s --type f --size +100M --changed 1h -E --layers --workers 2 -- 'nginx conf' /etc '/srv/my app'"
	if got := findRemoteCommand(options); got != want {
		t.Errorf("command:\n got %s\nwant %s", got, want)
	}
}

func findTestJSON(t *testing.T, paths ...string) string {
	t.Helper()
	report := findJSONReport{SchemaVersion: "1.0", Tool: map[string]string{"name": "edc", "version": "0.86.0"}, Matches: []findJSONMatch{}}
	for _, path := range paths {
		report.Matches = append(report.Matches, findJSONMatch{Path: path, Type: "file"})
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFindHostFromCapture(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capture remoteCapture
		status  string
	}{
		{"found", remoteCapture{Stdout: findTestJSON(t, "/etc/nginx/nginx.conf")}, findHostFound},
		// 일치가 없으면 원격 edc는 1로 끝난다. JSON을 읽었으면 실패가 아니다.
		{"none", remoteCapture{Stdout: findTestJSON(t), ExitCode: 1}, findHostNone},
		{"missing", remoteCapture{ExitCode: 127, Stderr: "bash: edc: command not found"}, findHostMissing},
		{"too old en", remoteCapture{ExitCode: 2, Stderr: "unknown command: find\nRun edc help"}, findHostTooOld},
		{"too old ko", remoteCapture{ExitCode: 2, Stderr: "알 수 없는 command: find"}, findHostTooOld},
		{"too old ja", remoteCapture{ExitCode: 2, Stderr: "不明な command: find"}, findHostTooOld},
		{"unreachable", remoteCapture{ExitCode: 255, Stderr: "ssh: connect to host web2 port 22: Connection refused", Err: io.EOF}, findHostUnreachable},
		{"timeout", remoteCapture{TimedOut: true, Err: context.DeadlineExceeded}, findHostTimeout},
		{"garbage", remoteCapture{Stdout: "hello"}, findHostInvalid},
		{"truncated", remoteCapture{Stdout: findTestJSON(t, "/x"), Truncated: true}, findHostInvalid},
		{"failed", remoteCapture{ExitCode: 2, Stderr: "edc find: permission denied", Err: io.EOF}, findHostFailed},
		// JSON을 냈어도 2로 끝났으면 찾을 곳을 하나도 읽지 못한 것이다.
		{"no root", remoteCapture{Stdout: findTestJSON(t), ExitCode: 2, Err: io.EOF}, findHostFailed},
	} {
		row := findHostFromCapture(tc.capture)
		if row.Status != tc.status {
			t.Errorf("%s: status %q, detail %q", tc.name, row.Status, row.Detail)
		}
	}
	if row := findHostFromCapture(remoteCapture{ExitCode: 2, Stderr: "unknown command: find"}); row.Detail != T("find.group.too_old") {
		t.Errorf("too old detail = %q", row.Detail)
	}
}

func TestRunFindGroupCollectsHosts(t *testing.T) {
	inventory := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(inventory, []byte("hosts:\n  - name: alpha\n  - name: beta\n    target: beta.example\n  - name: gamma\ngroups:\n  web: [alpha, beta, gamma]\nparallel: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var commands []string
	run := func(_ context.Context, target, command string, timeout time.Duration) remoteCapture {
		mu.Lock()
		commands = append(commands, target+" "+command)
		mu.Unlock()
		switch target {
		case "alpha":
			return remoteCapture{Stdout: findTestJSON(t, "/etc/nginx/nginx.conf", "/etc/nginx/conf.d/x.conf")}
		case "beta.example":
			return remoteCapture{Stdout: findTestJSON(t), ExitCode: 1}
		}
		return remoteCapture{ExitCode: 2, Stderr: "unknown command: find"}
	}
	options, err := parseFindOptions([]string{"--group", "web", "--inventory", inventory, "nginxconf", "/etc"}, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := testFindEnv(&stdout, &stderr)
	if code := runFindGroup(options, env, run); code != 0 {
		t.Fatalf("code %d", code)
	}
	if stdout.String() != "alpha:/etc/nginx/nginx.conf\nalpha:/etc/nginx/conf.d/x.conf\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gamma  "+T("find.group.status.edc_too_old")) || !strings.Contains(stderr.String(), T("find.group.summary", 1, 3)) || strings.Contains(stderr.String(), "beta") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if len(commands) != 3 || !strings.Contains(commands[0], "-- nginxconf /etc") {
		t.Errorf("commands = %q", commands)
	}
	out := filepath.Join(t.TempDir(), "group.json")
	options.common.jsonPath = out
	if code := runFindGroup(options, env, run); code != 0 {
		t.Fatalf("json code %d", code)
	}
	data, _ := os.ReadFile(out)
	var report struct {
		Hosts []findHostRow `json:"hosts"`
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Hosts) != 3 || report.Hosts[1].Status != findHostNone || report.Hosts[2].Status != findHostTooOld || report.Hosts[0].Version != "0.86.0" {
		t.Errorf("json = %s", data)
	}
}

func TestRunFindGroupNeedsAPatternAndAPath(t *testing.T) {
	for _, args := range [][]string{{"--group", "web"}, {"--group", "web", "nginx"}, {"--inventory", "x.yaml", "nginx", "/etc"}} {
		var stderr bytes.Buffer
		if code := runFindWith(args, testFindEnv(io.Discard, &stderr)); code != 2 || stderr.Len() == 0 {
			t.Errorf("%v: code %d, %q", args, code, stderr.String())
		}
	}
	if code := findGroupCode([]findHostRow{{Status: findHostUnreachable}, {Status: findHostTooOld}}); code != 2 {
		t.Errorf("no host searched: code %d", code)
	}
	if code := findGroupCode([]findHostRow{{Status: findHostUnreachable}, {Status: findHostNone}}); code != 1 {
		t.Errorf("none: code %d", code)
	}
}

// 일치를 stdout에 쓰지 못하면 찾았더라도 exit 2와 오류로 끝나야 한다. 일부만 쓴 목록을 성공으로 넘기지 않는다.
func TestRunFindGroupReportsAWriteError(t *testing.T) {
	inventory := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(inventory, []byte("hosts:\n  - name: web1\ngroups:\n  web: [web1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string, time.Duration) remoteCapture {
		return remoteCapture{Stdout: findTestJSON(t, "/etc/nginx/nginx.conf")}
	}
	options, err := parseFindOptions([]string{"--group", "web", "--inventory", inventory, "nginxconf", "/etc"}, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runFindGroup(options, testFindEnv(findFailingWriter{}, &stderr), run); code != 2 || !strings.Contains(stderr.String(), "edc find: no space left on device") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}
