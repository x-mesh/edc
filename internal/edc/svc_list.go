package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// svcListProperties는 목록이 unit마다 읽는 속성이다. process, fd, journal은 상세 화면에서만 읽는다.
var svcListProperties = []string{"Id", "Description", "LoadState", "ActiveState", "SubState", "Result", "MainPID", "NRestarts", "MemoryCurrent", "ActiveEnterTimestampMonotonic"}

var errSvcNoSystemd = errors.New("systemd is not running")

// svcListFailedShown은 실패 상자에 이름을 적는 unit 수다. 나머지는 개수로 적고 표에서 본다.
const svcListFailedShown = 5

type svcListItem struct {
	Unit        string  `json:"unit"`
	Description string  `json:"description,omitempty"`
	LoadState   string  `json:"load_state"`
	ActiveState string  `json:"active_state"`
	SubState    string  `json:"sub_state"`
	Result      string  `json:"result,omitempty"`
	MainPID     int     `json:"main_pid,omitempty"`
	Restarts    *uint64 `json:"n_restarts,omitempty"`
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
	UptimeS     *int64  `json:"uptime_s,omitempty"`
}

// rank는 목록의 순서다. 실패한 unit, 시작하거나 다시 시작하는 unit, 재시작한 적이 있는 unit이 먼저 와서 문제가 위에 보인다.
func (item svcListItem) rank() int {
	switch {
	case item.failed():
		return 0
	case item.changing():
		return 1
	case item.Restarts != nil && *item.Restarts > 0:
		return 2
	case item.ActiveState == "inactive":
		return 4
	}
	return 3
}

// failed는 실패했거나 unit 파일을 읽지 못한 unit이다. masked는 사용자가 일부러 끈 것이라 실패로 세지 않는다.
func (item svcListItem) failed() bool {
	return item.ActiveState == "failed" || item.LoadState == "error" || item.LoadState == "bad-setting"
}

// changing은 상태가 바뀌는 중인 unit이다. auto-restart는 실패 뒤 다시 시작을 기다리는 상태라 재시작을 반복하는 unit이 여기 머문다.
func (item svcListItem) changing() bool {
	switch item.ActiveState {
	case "activating", "deactivating", "reloading":
		return true
	}
	return item.SubState == "auto-restart"
}

func (item svcListItem) state() string {
	state := item.ActiveState
	if item.SubState != "" && item.SubState != item.ActiveState {
		state += " (" + item.SubState + ")"
	}
	if item.LoadState != "loaded" {
		state = item.LoadState
	}
	if item.Result != "" && item.Result != "success" {
		state += ", " + item.Result
	}
	return state
}

// parseSvcListUnits는 systemctl list-units --plain --no-legend의 줄을 읽는다. 열은 UNIT LOAD ACTIVE SUB DESCRIPTION이다.
func parseSvcListUnits(output string) []svcListItem {
	var items []svcListItem
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		item := svcListItem{Unit: fields[0], LoadState: fields[1], ActiveState: fields[2], SubState: fields[3]}
		if len(fields) > 4 {
			item.Description = strings.Join(fields[4:], " ")
		}
		items = append(items, item)
	}
	return items
}

// parseSvcShowUnits는 여러 unit의 systemctl show 출력이다. unit마다 빈 줄로 나뉜다.
func parseSvcShowUnits(output string) map[string]svcShow {
	shows := map[string]svcShow{}
	for _, block := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n\n") {
		if show := parseSvcShow(block); show.ID != "" {
			shows[show.ID] = show
		}
	}
	return shows
}

// collectSvcList는 service unit 목록이다. 기본은 멈춘(inactive) unit을 뺀다. all이면 systemd가 아는 unit을 모두 보인다.
func collectSvcList(ctx context.Context, sources svcSources, all bool) ([]svcListItem, error) {
	if !sources.systemd() {
		return nil, errSvcNoSystemd
	}
	output, err := sources.list(ctx)
	if err != nil {
		return nil, err
	}
	var items []svcListItem
	for _, item := range parseSvcListUnits(output) {
		// not-found는 다른 unit이 참조만 하고 파일이 없는 이름이다. 이 host에 있는 service가 아니다.
		if item.LoadState == "not-found" && item.ActiveState == "inactive" {
			continue
		}
		if all || item.ActiveState != "inactive" {
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return items, nil
	}
	units := make([]string, 0, len(items))
	for _, item := range items {
		units = append(units, item.Unit)
	}
	output, err = sources.showUnits(ctx, units)
	if err != nil {
		return nil, err
	}
	shows := parseSvcShowUnits(output)
	now, nowKnown := sources.monotonic()
	for index, item := range items {
		show, ok := shows[item.Unit]
		if !ok {
			continue
		}
		item.LoadState, item.ActiveState, item.SubState, item.Result = show.LoadState, show.ActiveState, show.SubState, show.Result
		item.MainPID, item.Restarts, item.MemoryBytes = show.MainPID, show.NRestarts, show.MemoryCurrent
		if show.Description != "" {
			item.Description = show.Description
		}
		if uptime, ok := svcUptime(show, now, nowKnown); ok {
			seconds := int64(uptime.Seconds())
			item.UptimeS = &seconds
		}
		items[index] = item
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank() != items[j].rank() {
			return items[i].rank() < items[j].rank()
		}
		return strings.ToLower(items[i].Unit) < strings.ToLower(items[j].Unit)
	})
	return items, nil
}

// evaluateSvcList는 목록 전체를 결과 하나로 낸다. 실패한 unit이 있으면 fail이고, 상태가 바뀌는 중이거나 다시 시작을 기다리는
// unit이 있으면 warn이다. 예전에 재시작한 적만 있는 unit은 위에 보이지만 경고하지 않는다. 오래 돈 host에서는 흔하다.
func evaluateSvcList(items []svcListItem, err error) Result {
	if errors.Is(err, errSvcNoSystemd) {
		return unsupported(svcProbeID, T("svc.skip.no_systemd"))
	}
	if err != nil {
		return svcFailed(svcResult("list", StatusFail, T("svc.list.failed", infoSingleLine(err.Error()))))
	}
	var failed []string
	changing := 0
	for _, item := range items {
		switch {
		case item.failed():
			failed = append(failed, item.Unit+" ("+item.state()+")")
		case item.changing():
			changing++
		}
	}
	status := StatusPass
	if changing > 0 {
		status = StatusWarn
	}
	result := svcResult("list", status, T("svc.list.summary", len(items), len(failed), changing))
	if len(failed) > 0 {
		// 실패 상자에는 실패한 unit을 적는다. 요약 줄을 다시 쓰면 어느 unit인지 표를 찾아봐야 한다.
		result.Status = StatusFail
		result.Error = &DiagnosticError{Kind: "state", Message: strings.Join(failed[:min(len(failed), svcListFailedShown)], ", ")}
		if hidden := len(failed) - svcListFailedShown; hidden > 0 {
			result.Error.Message += fmt.Sprintf(" +%d", hidden)
		}
	}
	result.Metrics = map[string]interface{}{"services": items, "failed": len(failed), "changing": changing}
	return result
}

// svcListCells는 표와 대화형 화면이 같이 쓰는 칸이다.
func svcListCells(item svcListItem) []string {
	restarts, uptime, memory, pid := "—", "—", "—", "—"
	if item.Restarts != nil {
		restarts = strconv.FormatUint(*item.Restarts, 10)
	}
	if item.UptimeS != nil {
		uptime = svcFormatUptime(time.Duration(*item.UptimeS) * time.Second)
	}
	if item.MemoryBytes != nil {
		memory = formatProcessRSS(*item.MemoryBytes)
	}
	if item.MainPID > 0 {
		pid = strconv.Itoa(item.MainPID)
	}
	return []string{item.Unit, item.state(), restarts, uptime, memory, pid}
}

func svcListHeaders() []string {
	return []string{T("svc.list.column.unit"), T("svc.list.column.state"), T("svc.list.column.restarts"), T("svc.list.column.uptime"), T("svc.list.column.memory"), T("svc.list.column.pid")}
}

func formatSvcListTable(items []svcListItem) string {
	if len(items) == 0 {
		return ""
	}
	rows := [][]string{svcListHeaders()}
	for _, item := range items {
		rows = append(rows, svcListCells(item))
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for index, cell := range row {
			widths[index] = max(widths[index], liveWidth(cell))
		}
	}
	var builder strings.Builder
	for _, row := range rows {
		for index, cell := range row {
			if index == len(row)-1 {
				builder.WriteString(cell)
				break
			}
			builder.WriteString(liveCell(cell, widths[index]+2))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// runSvcList는 unit 없이 부른 edc svc다. 터미널이면 목록에서 골라 상세를 여는 화면을 열고, 아니면 결과 한 줄과 표를 낸다.
func runSvcList(options commonOptions, version string, sources svcSources, ok, all bool) int {
	started := time.Now()
	target := map[string]interface{}{"all": all}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(svcProbeID, T("svc.skip.linux_only"))}, options.redact))
	}
	if options.jsonPath == "" && liveTerminal() && sources.systemd() {
		return runSvcBrowser(sources, options.timeout, all)
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	items, err := collectSvcList(ctx, sources, all)
	result := evaluateSvcList(items, err)
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, target, []Result{result}, options.redact))
	}
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	var output io.Writer = os.Stdout
	var buffer strings.Builder
	if options.redact {
		output = &buffer
	}
	fmt.Fprint(output, formatResultLine(result, color))
	printResultDetail(output, result, false, color)
	if table := formatSvcListTable(items); table != "" {
		fmt.Fprint(output, "\n"+table)
	}
	if options.redact {
		fmt.Fprint(os.Stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}
