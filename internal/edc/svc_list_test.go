package edc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

const svcListUnitsOutput = `apparmor.service        loaded    active   exited  Load AppArmor profiles
auditd.service          not-found inactive dead    auditd.service
cron.service            loaded    active   running Regular background program processing daemon
apt-daily.service       loaded    inactive dead    Daily apt download activities
web.service             loaded    failed   failed  Web front end
worker.service          loaded    activating auto-restart Queue worker
Postgres.service        loaded    active   running PostgreSQL
dbus.socket             loaded    active   running D-Bus socket
`

// svcListShowOutput은 systemctl show가 unit마다 빈 줄로 나눠 내는 출력이다. MemoryCurrent는 accounting이 꺼지면 [not set]이다.
const svcListShowOutput = `Id=apparmor.service
LoadState=loaded
ActiveState=active
SubState=exited
Result=success
MainPID=0
NRestarts=0
MemoryCurrent=[not set]
ActiveEnterTimestampMonotonic=1000000

Id=cron.service
Description=Regular background program processing daemon
LoadState=loaded
ActiveState=active
SubState=running
Result=success
MainPID=1488
NRestarts=0
MemoryCurrent=974848
ActiveEnterTimestampMonotonic=4000000

Id=web.service
LoadState=loaded
ActiveState=failed
SubState=failed
Result=exit-code
MainPID=0
NRestarts=3
MemoryCurrent=[not set]
ActiveEnterTimestampMonotonic=0

Id=worker.service
LoadState=loaded
ActiveState=activating
SubState=auto-restart
Result=exit-code
MainPID=0
NRestarts=12
MemoryCurrent=18446744073709551615
ActiveEnterTimestampMonotonic=0

Id=Postgres.service
LoadState=loaded
ActiveState=active
SubState=running
Result=success
MainPID=1201
NRestarts=1
MemoryCurrent=429916160
ActiveEnterTimestampMonotonic=2000000

Id=apt-daily.service
LoadState=loaded
ActiveState=inactive
SubState=dead
Result=success
MainPID=0
NRestarts=0
MemoryCurrent=[not set]
ActiveEnterTimestampMonotonic=0
`

func svcListFakeSources(asked *[]string) svcSources {
	sources := (&svcFakeSources{}).sources(0, svcActiveShow, func(context.Context) (string, string, error) { return "", "", nil })
	sources.list = func(context.Context) (string, error) { return svcListUnitsOutput, nil }
	sources.showUnits = func(_ context.Context, units []string) (string, error) {
		*asked = append(*asked, units...)
		return svcListShowOutput, nil
	}
	return sources
}

func svcListUnits(items []svcListItem) string {
	var units []string
	for _, item := range items {
		units = append(units, item.Unit)
	}
	return strings.Join(units, ",")
}

func TestParseSvcListUnitsKeepsServicesOnly(t *testing.T) {
	items := parseSvcListUnits(svcListUnitsOutput)
	if got := svcListUnits(items); got != "apparmor.service,auditd.service,cron.service,apt-daily.service,web.service,worker.service,Postgres.service" {
		t.Fatalf("units = %s", got)
	}
	if items[0].Description != "Load AppArmor profiles" || items[5].SubState != "auto-restart" {
		t.Fatalf("items = %+v", items)
	}
	shows := parseSvcShowUnits(svcListShowOutput)
	if len(shows) != 6 || shows["cron.service"].MainPID != 1488 || *shows["cron.service"].MemoryCurrent != 974848 {
		t.Fatalf("shows = %+v", shows)
	}
	// accounting이 꺼진 값과 오래된 systemd의 2^64-1은 모르는 값이다.
	if shows["apparmor.service"].MemoryCurrent != nil || shows["worker.service"].MemoryCurrent != nil {
		t.Fatalf("unknown memory = %+v %+v", shows["apparmor.service"].MemoryCurrent, shows["worker.service"].MemoryCurrent)
	}
}

func TestCollectSvcListPutsProblemsFirst(t *testing.T) {
	var asked []string
	items, err := collectSvcList(context.Background(), svcListFakeSources(&asked), false)
	if err != nil {
		t.Fatal(err)
	}
	// 실패, 다시 시작을 기다림, 재시작 기록, 나머지 순이다. 이름은 대소문자를 가리지 않는다.
	if got := svcListUnits(items); got != "web.service,worker.service,Postgres.service,apparmor.service,cron.service" {
		t.Fatalf("order = %s", got)
	}
	if got := strings.Join(asked, ","); got != "apparmor.service,cron.service,web.service,worker.service,Postgres.service" {
		t.Fatalf("show asked for %s", got)
	}
	cron := items[4]
	if cron.MainPID != 1488 || cron.UptimeS == nil || *cron.UptimeS != 6 || cron.MemoryBytes == nil {
		t.Fatalf("cron = %+v", cron)
	}
	asked = nil
	items, _ = collectSvcList(context.Background(), svcListFakeSources(&asked), true)
	// --all은 멈춘 unit을 맨 뒤에 더한다. 파일이 없는 not-found 이름은 이 host의 service가 아니라 넣지 않는다.
	if got := svcListUnits(items); got != "web.service,worker.service,Postgres.service,apparmor.service,cron.service,apt-daily.service" {
		t.Fatalf("all = %s", got)
	}
	noSystemd := svcListFakeSources(&asked)
	noSystemd.systemd = func() bool { return false }
	if _, err := collectSvcList(context.Background(), noSystemd, false); !errors.Is(err, errSvcNoSystemd) {
		t.Fatalf("no systemd = %v", err)
	}
}

func TestEvaluateSvcListNamesTheFailedUnits(t *testing.T) {
	var asked []string
	items, _ := collectSvcList(context.Background(), svcListFakeSources(&asked), false)
	result := evaluateSvcList(items, nil)
	if result.Probe != "svc.list" || result.Status != StatusFail || result.Error == nil || result.Error.Message != "web.service (failed, exit-code)" {
		t.Fatalf("result = %+v", result)
	}
	if result.Metrics["failed"] != 1 || result.Metrics["changing"] != 1 || len(result.Metrics["services"].([]svcListItem)) != 5 {
		t.Fatalf("metrics = %+v", result.Metrics)
	}
	// 재시작을 기다리는 unit만 있으면 warn이고, 예전에 재시작한 적만 있으면 pass다.
	if status := evaluateSvcList(items[1:], nil).Status; status != StatusWarn {
		t.Fatalf("restarting = %s", status)
	}
	if status := evaluateSvcList(items[2:], nil).Status; status != StatusPass {
		t.Fatalf("restarted before = %s", status)
	}
	if result := evaluateSvcList(nil, errors.New("boom")); result.Status != StatusFail || !strings.Contains(result.Summary, "boom") {
		t.Fatalf("error = %+v", result)
	}
	if result := evaluateSvcList(nil, errSvcNoSystemd); result.Status != StatusSkip {
		t.Fatalf("no systemd = %+v", result)
	}
}

func TestFormatSvcListTableAlignsColumns(t *testing.T) {
	var asked []string
	items, _ := collectSvcList(context.Background(), svcListFakeSources(&asked), false)
	lines := strings.Split(strings.TrimRight(formatSvcListTable(items, textStyle{}), "\n"), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "UNIT") || !strings.HasPrefix(lines[1], "web.service") {
		t.Fatalf("table:\n%s", strings.Join(lines, "\n"))
	}
	// 모든 줄의 상태 칸이 머리줄의 STATE와 같은 열에서 시작한다.
	stateColumn := strings.Index(lines[0], "STATE")
	for _, line := range lines[1:] {
		if line[stateColumn-2:stateColumn] != "  " || line[stateColumn] == ' ' {
			t.Fatalf("state column moved: %q", line)
		}
	}
	if !strings.Contains(lines[2], "activating (auto-restart), exit-code") || !strings.Contains(lines[5], "952K") {
		t.Fatalf("cells:\n%s", strings.Join(lines, "\n"))
	}
}

// 터미널에 쓰는 표는 머리글이 회색이고 상태 칸은 대화형 화면과 같은 색이다. 색을 벗기면 평문 표와 같아야 열이 맞는다.
func TestFormatSvcListTableColorsTheState(t *testing.T) {
	var asked []string
	items, _ := collectSvcList(context.Background(), svcListFakeSources(&asked), false)
	plain, colored := formatSvcListTable(items, textStyle{}), formatSvcListTable(items, textStyle{color: true})
	if ansi.Strip(colored) != plain {
		t.Fatalf("plain:\n%s\ncolored:\n%q", plain, colored)
	}
	if !strings.HasPrefix(colored, richExpect("\033[90mUNIT\033[0m")) {
		t.Fatalf("header = %q", strings.SplitN(colored, "\n", 2)[0])
	}
	for _, item := range items {
		if !strings.Contains(colored, richPaint(item.state(), item.stateColor(), false)) {
			t.Fatalf("%s state is not %s: %q", item.Unit, item.stateColor(), colored)
		}
	}
	if items[0].stateColor() != richRed || items[1].stateColor() != richYellow {
		t.Fatalf("fixture order changed: %s %s", items[0].state(), items[1].state())
	}
}

func TestRunSvcWithoutAUnitListsTheServices(t *testing.T) {
	var asked []string
	output := filepath.Join(t.TempDir(), "svc.json")
	if code := runSvcWith([]string{"--json", output}, "test", svcListFakeSources(&asked), true); code != 1 {
		t.Fatalf("a failed service exits with 1, got %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Results []struct {
			Probe   string `json:"probe"`
			Metrics struct {
				Services []svcListItem `json:"services"`
			} `json:"metrics"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Results) != 1 || report.Results[0].Probe != "svc.list" || len(report.Results[0].Metrics.Services) != 5 {
		t.Fatalf("report = %s", data)
	}
	if got := report.Results[0].Metrics.Services[0]; got.Unit != "web.service" || got.Restarts == nil || *got.Restarts != 3 {
		t.Fatalf("first service = %+v", got)
	}
	if code := runSvcWith([]string{"--all", "nginx"}, "test", svcListFakeSources(&asked), true); code != 2 {
		t.Fatalf("--all with a unit exits with 2, got %d", code)
	}
	if code := runSvcWith([]string{"--json", output}, "test", svcSources{}, false); code != 0 {
		t.Fatalf("an unsupported host skips, got %d", code)
	}
	if data, _ := os.ReadFile(output); !strings.Contains(string(data), `"status": "skip"`) && !strings.Contains(string(data), `"status":"skip"`) {
		t.Fatalf("skip report = %s", data)
	}
}
