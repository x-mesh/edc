package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// edc check --group은 edc remote와 같은 인벤토리와 OpenSSH 설정으로 그룹의 호스트마다 edc check --json -을 실행하고 결과를
// 호스트와 점검의 표 하나로 모은다. check는 읽기만 하는 명령이라 원격에서도 바꾸는 것이 없다. 원격에 edc를 설치하지는 않는다.

const (
	checkGroupProbeID = "check.group"
	// 원격 check의 제한 시간 위에 SSH 연결과 셸 준비에 드는 여유를 둔다.
	checkGroupSSHMargin = 30 * time.Second
)

const (
	checkHostChecked     = "checked"
	checkHostMissing     = "edc_missing"
	checkHostUnsupported = "no_check"
	checkHostUnreachable = "unreachable"
	checkHostTimeout     = "timeout"
	checkHostInvalid     = "invalid_output"
	checkHostFailed      = "failed"
)

// checkIDs는 evaluateCheck가 내는 점검의 순서다. 표의 열이 이 순서를 따른다.
var checkIDs = []string{"service", "process", "memory", "disk", "kernel", "time", "network", "container", "filehandles", "reboot"}

type checkRemoteResult struct {
	Probe   string `json:"probe"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Next    string `json:"next,omitempty"`
}

type checkRemoteReport struct {
	Tool struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"tool"`
	Results []checkRemoteResult `json:"results"`
}

// isCheckReport는 JSON이 edc check의 보고서인지다. results가 있는 다른 JSON이나 다른 edc 명령의 출력을 점검 결과로 읽지 않는다.
func (report checkRemoteReport) isCheckReport() bool {
	if report.Tool.Name != "edc" || len(report.Results) == 0 {
		return false
	}
	for _, result := range report.Results {
		if result.Probe != "check" && !strings.HasPrefix(result.Probe, "check.") {
			return false
		}
	}
	return true
}

type checkHostRow struct {
	Host    string `json:"host"`
	Target  string `json:"target"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Version string `json:"edc_version,omitempty"`
	// Results는 원격 check가 낸 결과다. 결과를 읽은 호스트에만 있다.
	Results    []checkRemoteResult `json:"results,omitempty"`
	DurationMS int64               `json:"duration_ms"`
}

// worst는 호스트의 결과 가운데 가장 나쁜 판정이다.
func (row checkHostRow) worst() Status {
	worst := StatusPass
	for _, result := range row.Results {
		if status := Status(result.Status); checkSeverity(status) > checkSeverity(worst) {
			worst = status
		}
	}
	return worst
}

// runCheckGroup은 그룹의 호스트에서 check를 실행하고 표나 JSON으로 낸다.
func runCheckGroup(options commonOptions, group, inventoryPath, routine, version string, run cleanRemoteRunner, stdout io.Writer) int {
	if inventoryPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		configDir, _ := os.UserConfigDir()
		path, found := discoverRemoteInventory(cwd, configDir)
		if !found {
			fmt.Fprintln(os.Stderr, remoteInventoryNotFound(cwd))
			return 2
		}
		inventoryPath = path
	}
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	hosts, err := hostsForRemoteGroup(inventory, group)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := checkCollectHosts(ctx, hosts, remoteParallelForGroup(inventory, group, 0), options.timeout, routine, run)
	result := checkGroupResult(rows, started)
	target := map[string]interface{}{"group": group, "inventory": inventoryPath}
	if routine != "" {
		target["routine"] = routine
	}
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, target, []Result{result}, options.redact))
	}
	file, _ := stdout.(*os.File)
	width := 0
	if file != nil && isTerminal(file) {
		width = terminalWidth()
	}
	text := formatResultLine(result, false) + formatCheckGroup(rows, width)
	if options.redact {
		text = redactIPAddresses(text)
	}
	fmt.Fprint(stdout, text)
	return exitCode([]Result{result})
}

// checkCollectHosts는 호스트를 parallel개씩 동시에 처리하고 인벤토리 순서대로 돌려준다.
func checkCollectHosts(ctx context.Context, hosts []remoteHost, parallel int, timeout time.Duration, routine string, run cleanRemoteRunner) []checkHostRow {
	if parallel < 1 {
		parallel = 1
	}
	rows := make([]checkHostRow, len(hosts))
	slots := make(chan struct{}, parallel)
	var running sync.WaitGroup
	command := fmt.Sprintf("edc check --json - --timeout %s", timeout)
	// 이름은 runCheck가 checkNamePattern으로 검사했다. 셸에 그대로 넣어도 된다.
	if routine != "" {
		command += " " + routine
	}
	for index, host := range hosts {
		running.Add(1)
		slots <- struct{}{}
		go func(index int, host remoteHost) {
			defer func() {
				<-slots
				running.Done()
			}()
			target := host.Target
			if target == "" {
				target = host.Name
			}
			row := checkHostFromCapture(run(ctx, target, command, timeout+checkGroupSSHMargin))
			row.Host, row.Target = host.Name, target
			rows[index] = row
		}(index, host)
	}
	running.Wait()
	return rows
}

// checkHostFromCapture는 원격 결과 하나를 표의 행으로 바꾼다. check는 fail이 있으면 1로 끝나므로, 표준 출력이 check JSON으로
// 읽히면 종료 코드와 상관없이 그 결과를 쓴다. 읽히지 않을 때만 종료 코드와 표준 오류로 실패 원인을 고른다.
func checkHostFromCapture(capture remoteCapture) checkHostRow {
	row := checkHostRow{DurationMS: capture.Duration.Milliseconds()}
	var report checkRemoteReport
	if !capture.Truncated && json.Unmarshal([]byte(capture.Stdout), &report) == nil && report.isCheckReport() {
		row.Status, row.Version, row.Results = checkHostChecked, report.Tool.Version, report.Results
		return row
	}
	detail := cleanFirstLine(capture.Stderr)
	switch {
	case capture.TimedOut:
		row.Status = checkHostTimeout
	case capture.ExitCode == 255:
		row.Status, row.Detail = checkHostUnreachable, detail
	case capture.ExitCode == 127 || strings.Contains(capture.Stderr, "edc: command not found") || strings.Contains(capture.Stderr, "edc: not found"):
		row.Status = checkHostMissing
	// check가 없는 이전 edc는 사용법 오류로 2를 낸다.
	case capture.ExitCode == 2 && strings.Contains(capture.Stderr, "unknown command: check"):
		row.Status = checkHostUnsupported
	case capture.Err == nil:
		row.Status, row.Detail = checkHostInvalid, cleanFirstLine(capture.Stdout)
	default:
		row.Status, row.Detail = checkHostFailed, detail
		if row.Detail == "" {
			row.Detail = fmt.Sprintf("exit %d", capture.ExitCode)
		}
	}
	return row
}

// checkGroupResult는 호스트 하나라도 fail이면 fail이다. warn이 있거나 결과를 받지 못한 호스트가 있으면 warn이다.
func checkGroupResult(rows []checkHostRow, started time.Time) Result {
	checked, failed, warned := 0, 0, 0
	for _, row := range rows {
		if row.Status != checkHostChecked {
			continue
		}
		checked++
		switch row.worst() {
		case StatusFail:
			failed++
		case StatusWarn:
			warned++
		}
	}
	status := StatusPass
	switch {
	case failed > 0:
		status = StatusFail
	case warned > 0 || checked < len(rows):
		status = StatusWarn
	}
	return Result{Probe: checkGroupProbeID, StartedAt: started.UTC(), Status: status,
		Summary: T("check.group.summary", len(rows), checked, len(rows)-checked, failed, warned), DurationMS: time.Since(started).Milliseconds(),
		Metrics: map[string]interface{}{"hosts": rows}}
}

func formatCheckGroup(rows []checkHostRow, width int) string {
	// 사용자 명령은 루틴마다 다르므로 열 하나에 가장 나쁜 판정만 적고, 문제는 아래 상세에 이름과 함께 적는다.
	header := append(append([]string{T("check.group.col.host")}, checkIDs...), "cmd")
	table := [][]string{header}
	for _, row := range rows {
		if row.Status != checkHostChecked {
			state := T("check.group.status." + row.Status)
			if row.Detail != "" {
				state += "  " + row.Detail
			}
			table = append(table, []string{row.Host, state})
			continue
		}
		statuses := map[string]string{}
		commands := Status("")
		for _, result := range row.Results {
			id := strings.TrimPrefix(result.Probe, "check.")
			statuses[id] = strings.ToUpper(result.Status)
			if strings.HasPrefix(id, "cmd.") && (commands == "" || checkSeverity(Status(result.Status)) > checkSeverity(commands)) {
				commands = Status(result.Status)
			}
		}
		// macOS처럼 점검을 하지 않은 호스트는 결과가 skip 하나다. 열마다 —를 채우지 않고 그 이유를 적는다.
		if len(row.Results) == 1 && row.Results[0].Probe == "check" {
			table = append(table, []string{row.Host, strings.ToUpper(row.Results[0].Status) + "  " + row.Results[0].Summary})
			continue
		}
		cells := []string{row.Host}
		for _, id := range checkIDs {
			cell, ok := statuses[id]
			if !ok {
				cell = "—"
			}
			cells = append(cells, cell)
		}
		if commands == "" {
			cells = append(cells, "—")
		} else {
			cells = append(cells, strings.ToUpper(string(commands)))
		}
		table = append(table, cells)
	}
	widths := make([]int, len(header))
	for _, cells := range table {
		// 점검하지 못한 호스트의 행은 이름 뒤 칸 하나에 상태를 쓰므로 너비 계산에서 뺀다.
		if len(cells) != len(header) {
			widths[0] = max(widths[0], liveWidth(cells[0]))
			continue
		}
		for index, cell := range cells {
			widths[index] = max(widths[index], liveWidth(cell))
		}
	}
	var builder strings.Builder
	builder.WriteString("\n")
	for _, cells := range table {
		parts := make([]string, len(cells))
		for index, cell := range cells {
			if index == len(cells)-1 {
				parts[index] = cell
				continue
			}
			parts[index] = cleanPad(cell, widths[index])
		}
		builder.WriteString(duFit(strings.Join(parts, "  "), width) + "\n")
	}
	idWidth := 0
	for _, id := range checkIDs {
		idWidth = max(idWidth, len(id))
	}
	var findings []string
	for _, row := range rows {
		for _, result := range row.Results {
			if status := Status(result.Status); status != StatusFail && status != StatusWarn {
				continue
			}
			line := fmt.Sprintf("%s  %-4s  %-*s  %s", cleanPad(row.Host, widths[0]), strings.ToUpper(result.Status), idWidth, strings.TrimPrefix(result.Probe, "check."), result.Summary)
			findings = append(findings, duFit(line, width))
			if result.Next != "" {
				findings = append(findings, duFit(strings.Repeat(" ", widths[0])+"  next: "+result.Next, width))
			}
		}
	}
	if len(findings) > 0 {
		builder.WriteString("\n" + strings.Join(findings, "\n") + "\n")
	}
	return builder.String()
}
