package edc

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testLaunchdPrint = "gui/501/com.example.sync = {\n" +
	"\tactive count = 0\n" +
	"\tpath = /Library/LaunchAgents/com.example.sync.plist\n" +
	"\ttype = LaunchAgent\n" +
	"\tstate = spawn scheduled\n" +
	"\n" +
	"\tprogram = /opt/sync/bin/sync\n" +
	"\targuments = {\n" +
	"\t\t/opt/sync/bin/sync\n" +
	"\t\t--token\n" +
	"\t\tabc123\n" +
	"\t}\n" +
	"\n" +
	"\tstderr path = /tmp/sync.err\n" +
	"\tenvironment = {\n" +
	"\t\tPATH => /usr/bin\n" +
	"\t}\n" +
	"\n" +
	"\tdomain = gui/501 [100018]\n" +
	"\truns = 17\n" +
	"\tlast exit code = 78: EX_CONFIG\n" +
	"\tendpoints = {\n" +
	"\t\t\"com.example.sync\" = {\n" +
	"\t\t\tport = 0x6b183\n" +
	"\t\t}\n" +
	"\t}\n" +
	"}\n"

func TestParseLaunchdPrintReadsTopLevelFields(t *testing.T) {
	info := parseLaunchdPrint(testLaunchdPrint)
	if info.Domain != "gui/501" || info.State != "spawn scheduled" || info.Runs != 17 || info.ExitCode != "78: EX_CONFIG" || info.PID != 0 {
		t.Fatalf("info = %+v", info)
	}
	if info.StderrPath != "/tmp/sync.err" || strings.Join(info.Arguments, " ") != "/opt/sync/bin/sync --token abc123" {
		t.Fatalf("paths = %+v", info)
	}
	result := launchdStateResult("com.example.sync", info)
	if result.Status != StatusFail || result.Next != "launchctl print gui/501/com.example.sync" {
		t.Fatalf("state = %+v", result)
	}
	if strings.Contains(result.Evidence[0].Value, "abc123") {
		t.Fatalf("a secret argument leaked: %s", result.Evidence[0].Value)
	}
	// 지금 돌고 있으면 지난 종료 코드는 실패가 아니다.
	info.PID = 42
	if running := launchdStateResult("com.example.sync", info); running.Status != StatusPass {
		t.Fatalf("running = %+v", running)
	}
}

func TestLaunchdListShowsFailuresAndHidesNoise(t *testing.T) {
	items := parseLaunchdList("PID\tStatus\tLabel\n-\t78\tcom.example.sync\n123\t0\tcom.example.web\n-\t0\tcom.example.idle\n-\t-9\tcom.example.killed\n456\t0\tcom.apple.Finder\n-\t1\tcom.apple.broken\n789\t0\tapplication.com.apple.Music.1.2\n")
	result := launchdListResult(items, false)
	if result.Status != StatusFail || result.Next != "edc svc com.example.sync" || !strings.Contains(result.Error.Message, "com.apple.broken (exit 1)") {
		t.Fatalf("result = %+v", result)
	}
	table := result.Evidence[0].Value
	for _, label := range []string{"com.example.sync", "com.example.web", "com.apple.broken"} {
		if !strings.Contains(table, label) {
			t.Errorf("table misses %s:\n%s", label, table)
		}
	}
	// 신호로 끝난 job, 쉬는 job, Apple job, GUI 앱은 기본 목록에서 뺀다.
	for _, label := range []string{"com.example.idle", "com.example.killed", "com.apple.Finder", "application.com.apple.Music"} {
		if strings.Contains(table, label) {
			t.Errorf("table shows %s:\n%s", label, table)
		}
	}
	if all := launchdListResult(items, true); !strings.Contains(all.Evidence[0].Value, "com.apple.Finder") {
		t.Fatalf("--all = %s", all.Evidence[0].Value)
	}
}

func TestLaunchdLogResultTailsTheJobLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.err")
	var text strings.Builder
	for line := range 30 {
		text.WriteString("line " + strconv.Itoa(line) + "\n")
	}
	if err := os.WriteFile(path, []byte(text.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	result := launchdLogResult(launchdJobInfo{StderrPath: path}, tailLogFile)
	lines := strings.Split(result.Evidence[0].Value, "\n")
	if result.Status != StatusPass || len(lines) != launchdLogLines || lines[0] != "line 10" || lines[len(lines)-1] != "line 29" {
		t.Fatalf("result = %+v", result)
	}
	noPath := launchdLogResult(launchdJobInfo{Program: "/opt/sync/bin/sync"}, tailLogFile)
	if noPath.Status != StatusSkip || noPath.Next != `log show --last 1h --predicate 'process == "sync"'` {
		t.Fatalf("no path = %+v", noPath)
	}
	denied := launchdLogResult(launchdJobInfo{StderrPath: "/var/log/x"}, func(string) ([]string, error) { return nil, os.ErrPermission })
	if denied.Status != StatusSkip || !strings.Contains(denied.Summary, "/var/log/x") {
		t.Fatalf("denied = %+v", denied)
	}
}

func TestLaunchdLabelAndDomains(t *testing.T) {
	for _, label := range []string{"-h", "gui/501/x", "a b", ""} {
		if validLaunchdLabel(label) {
			t.Errorf("%q must be rejected", label)
		}
	}
	if !validLaunchdLabel("com.example.sync") {
		t.Fatal("a reverse-DNS label must pass")
	}
	if got := launchdDomains(0)[0]; got != "system" {
		t.Fatalf("root domain = %s", got)
	}
	if got := launchdDomains(501)[0]; got != "gui/501" {
		t.Fatalf("user domain = %s", got)
	}
}
