package edc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// macOS 원천의 출력을 읽는 함수다. build tag를 두지 않아 Linux에서도 테스트한다.

// parseLaunchctlList는 launchctl list의 PID, Status, Label 열을 읽는다. 실행 중인 job과 양수 종료 코드로 멈춘 job만 남긴다.
// 음수 Status는 signal로 끝난 것이다. launchd는 쉬고 있는 on-demand agent를 SIGKILL(-9)로 정리하므로 실패가 아니다.
func parseLaunchctlList(output string) []svcListItem {
	var items []svcListItem
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "PID" {
			continue
		}
		label := strings.Join(fields[2:], " ")
		if fields[0] != "-" {
			items = append(items, svcListItem{Unit: label, LoadState: "loaded", ActiveState: "active", SubState: "running"})
			continue
		}
		if status, err := strconv.Atoi(fields[1]); err == nil && status > 0 {
			items = append(items, svcListItem{Unit: label, LoadState: "loaded", ActiveState: "failed", SubState: "exit " + fields[1], Result: "exit-code"})
		}
	}
	return items
}

// parseCheckPS는 ps -Ao pid=,ppid=,state=,comm=의 출력을 읽는다. comm에는 공백이 들어갈 수 있다.
func parseCheckPS(output string) []checkProcess {
	var processes []checkProcess
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || fields[2] == "" {
			continue
		}
		// comm은 실행 파일의 경로다. 앱 경로에는 공백이 들어가므로 마지막 / 뒤만 이름으로 쓴다.
		command := strings.Join(fields[3:], " ")
		if slash := strings.LastIndexByte(command, '/'); slash >= 0 && slash < len(command)-1 {
			command = command[slash+1:]
		}
		// macOS의 D 상태에 해당하는 U는 Linux와 뜻이 달라 판정하지 않는다. 첫 글자만 쓴다.
		state := fields[2][:1]
		if state == "D" {
			state = "U"
		}
		processes = append(processes, checkProcess{PID: pid, PPID: ppid, Command: command, State: state})
	}
	return processes
}

// parseCheckRouteGet은 route -n get default의 interface와 gateway다. route가 없으면 route는 "not in table"과 함께 실패한다.
func parseCheckRouteGet(output, stderr string, err error) (string, error) {
	if err != nil {
		if strings.Contains(stderr, "not in table") {
			return "", nil
		}
		return "", errors.New(strings.TrimSpace(stderr + " " + err.Error()))
	}
	var iface, gateway string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			iface = fields[1]
		}
		if len(fields) == 2 && fields[0] == "gateway:" {
			gateway = fields[1]
		}
	}
	if iface == "" {
		return "", nil
	}
	if gateway == "" {
		return "dev " + iface, nil
	}
	return gateway + " dev " + iface, nil
}

// readCheckContainers는 Linux와 macOS가 함께 쓴다. edc ctr과 같은 Docker Engine socket을 읽는다.
func readCheckContainers(ctx context.Context) ([]ctrListItem, error) {
	path, err := ctrSocketEndpoint()
	if err != nil {
		return nil, err
	}
	// docker가 없는 host가 흔하다. socket이 없으면 연결 오류가 아니라 docker가 없는 것이다.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, errCtrNoDocker
	}
	sources, _ := hostCtrSources()
	if sources.close != nil {
		defer sources.close()
	}
	// 자원 표본은 container마다 stats를 한 번 더 읽는다. 상태 판정에는 쓰지 않는다.
	sources.listStats = nil
	return collectCtrList(ctx, sources, false)
}

// macOS의 진단 보고서는 /Library/Logs/DiagnosticReports와 ~/Library/Logs/DiagnosticReports에 쌓인다. .ips의 첫 줄은
// bug_type이 든 JSON header다. 210은 kernel panic, 298은 JetsamEvent(memory가 모자라 커널이 process를 끝냄), 109와 309는
// app crash다. CPU나 wakeup 같은 자원 보고서(.diag)는 장애가 아니라서 세지 않는다.
const (
	diagnosticPanic  = "210"
	diagnosticJetsam = "298"
)

var diagnosticCrashTypes = map[string]bool{"109": true, "309": true}

// diagnosticDate는 보고서 이름에 붙는 날짜다. 앞부분이 process 이름이다.
var diagnosticDate = regexp.MustCompile(`[-_]\d{4}-\d{2}-\d{2}-\d{6}`)

// classifyDiagnosticReport는 보고서 하나를 커널 신호의 종류로 옮긴다. header를 읽지 못하면 이름으로만 고른다.
func classifyDiagnosticReport(name, header string) (kind, subject string) {
	var fields struct {
		BugType string `json:"bug_type"`
		AppName string `json:"app_name"`
	}
	_ = json.Unmarshal([]byte(header), &fields)
	subject = fields.AppName
	if subject == "" {
		subject = strings.TrimSuffix(name, filepath.Ext(name))
		if location := diagnosticDate.FindStringIndex(subject); location != nil {
			subject = subject[:location[0]]
		}
		subject = strings.TrimPrefix(subject, "ExcUserFault_")
	}
	switch {
	case strings.HasSuffix(name, ".panic") || fields.BugType == diagnosticPanic:
		return eventsOops, "kernel panic"
	case strings.HasPrefix(name, "JetsamEvent") || fields.BugType == diagnosticJetsam:
		return eventsOOM, "JetsamEvent"
	case strings.HasSuffix(name, ".hang"):
		return eventsHungTask, subject
	case strings.HasSuffix(name, ".ips") && diagnosticCrashTypes[fields.BugType]:
		return eventsSegfault, subject
	}
	return "", ""
}

// parseThermalLevel은 notifyutil -g com.apple.system.thermalpressurelevel의 값이다. 출력은 "<key> <값>" 한 줄이다.
func parseThermalLevel(output string) (int, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return 0, errors.New("unexpected notifyutil output: " + strings.TrimSpace(output))
	}
	return strconv.Atoi(fields[1])
}

// backupStamp는 Time Machine 백업 이름의 시각이다. latestbackup은 이 이름으로 끝나는 경로를 준다.
var backupStamp = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}-\d{6})`)

// parseBackup은 tmutil destinationinfo와 latestbackup의 출력이다. 대상이 없으면 Configured가 false다.
func parseBackup(destinations, latest string) checkBackup {
	if strings.Contains(destinations, "No destinations configured") {
		return checkBackup{}
	}
	backup := checkBackup{Configured: true}
	matches := backupStamp.FindAllString(latest, -1)
	if len(matches) == 0 {
		return backup
	}
	if at, err := time.ParseInLocation("2006-01-02-150405", matches[len(matches)-1], time.Local); err == nil {
		backup.Latest = at
	}
	return backup
}
