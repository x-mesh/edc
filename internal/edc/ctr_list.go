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

var errCtrNoDocker = errors.New("docker is not installed")

// ctrListFailedShown은 실패 상자에 이름을 적는 container 수다. 나머지는 개수로 적고 표에서 본다.
const ctrListFailedShown = 5

type ctrListItem struct {
	Name                  string   `json:"name"`
	ID                    string   `json:"id"`
	Image                 string   `json:"image"`
	Status                string   `json:"status"`
	Health                string   `json:"health,omitempty"`
	State                 string   `json:"state"`
	ExitCode              int      `json:"exit_code"`
	OOMKilled             bool     `json:"oom_killed,omitempty"`
	Restarts              int      `json:"restart_count"`
	PID                   int      `json:"pid,omitempty"`
	UptimeS               *int64   `json:"uptime_s,omitempty"`
	MemoryBytes           *uint64  `json:"memory_bytes,omitempty"`
	MemoryUnavailable     string   `json:"memory_unavailable,omitempty"`
	CPUPercent            *float64 `json:"cpu_usage_percent,omitempty"`
	CPUUnavailable        string   `json:"cpu_unavailable,omitempty"`
	MemoryLimitBytes      *uint64  `json:"memory_limit_bytes,omitempty"`
	MemoryUnlimited       bool     `json:"memory_unlimited,omitempty"`
	MemoryLimitKnown      bool     `json:"memory_limit_known"`
	MemoryPercent         *float64 `json:"memory_percent,omitempty"`
	PIDs                  *uint64  `json:"pids,omitempty"`
	PIDsUnavailable       string   `json:"pids_unavailable,omitempty"`
	NetworkRXBytes        *uint64  `json:"network_rx_bytes,omitempty"`
	NetworkTXBytes        *uint64  `json:"network_tx_bytes,omitempty"`
	NetworkRXUnavailable  string   `json:"network_rx_unavailable,omitempty"`
	NetworkTXUnavailable  string   `json:"network_tx_unavailable,omitempty"`
	BlockReadBytes        *uint64  `json:"block_read_bytes,omitempty"`
	BlockWriteBytes       *uint64  `json:"block_write_bytes,omitempty"`
	BlockReadUnavailable  string   `json:"block_read_unavailable,omitempty"`
	BlockWriteUnavailable string   `json:"block_write_unavailable,omitempty"`
}

// failed는 0이 아닌 코드로 끝났거나 메모리 때문에 죽은 container다. 코드 0으로 끝난 container는 일을 마친 것이다.
func (item ctrListItem) failed() bool {
	return item.Status == "dead" || (item.Status == "exited" && (item.ExitCode != 0 || item.OOMKilled))
}

// attention은 지금 손볼 일이 있을 수 있는 container다. edc ctr <container>의 state가 warn인 경우와 같다.
func (item ctrListItem) attention() bool {
	switch item.Status {
	case "restarting", "paused", "created", "removing":
		return true
	}
	return item.Health == "unhealthy" || item.Health == "starting" || (item.Status == "running" && item.OOMKilled)
}

// stopped는 코드 0으로 끝난 container다. 기본 목록에서 빠진다.
func (item ctrListItem) stopped() bool {
	return item.Status == "exited" && !item.failed()
}

// rank는 목록의 순서다. svc 목록과 같이 문제가 있는 container가 위에 온다.
func (item ctrListItem) rank() int {
	switch {
	case item.failed():
		return 0
	case item.attention():
		return 1
	case item.Restarts > 0:
		return 2
	case item.stopped():
		return 4
	}
	return 3
}

// collectCtrList는 container 목록이다. 기본은 코드 0으로 끝난 container를 뺀다. all이면 모두 보인다.
func collectCtrList(ctx context.Context, sources ctrSources, all bool) ([]ctrListItem, error) {
	if !sources.docker() {
		return nil, errCtrNoDocker
	}
	ids, err := sources.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	inspects, err := sources.inspectMany(ctx, ids)
	now := sources.now()
	var items []ctrListItem
	for _, inspect := range inspects {
		id := inspect.ID
		if len(id) > ctrShortIDLength {
			id = id[:ctrShortIDLength]
		}
		item := ctrListItem{Name: inspect.Name, ID: id, Image: inspect.Image, Status: inspect.Status, Health: inspect.Health, State: ctrStateText(inspect),
			ExitCode: inspect.ExitCode, OOMKilled: inspect.OOMKilled, Restarts: inspect.RestartCount, PID: inspect.PID}
		if !all && item.stopped() {
			continue
		}
		if inspect.Status == "running" && !inspect.StartedAt.IsZero() {
			if uptime := now.Sub(inspect.StartedAt); uptime >= 0 {
				seconds := int64(uptime.Seconds())
				item.UptimeS = &seconds
			}
		}
		items = append(items, item)
	}
	if sources.listStats != nil {
		running := make([]ctrInspect, 0, len(items))
		for _, inspect := range inspects {
			if inspect.Status == "running" {
				for _, item := range items {
					if item.ID == inspect.ID[:min(len(inspect.ID), ctrShortIDLength)] {
						running = append(running, inspect)
						break
					}
				}
			}
		}
		stats := sources.listStats(ctx, running)
		for index := range items {
			if sample, ok := stats[inspectsID(inspects, items[index].ID)]; ok {
				applyCtrListStats(&items[index], sample)
			} else if items[index].Status == "running" {
				items[index].CPUUnavailable = "resource sampling is unavailable"
				items[index].MemoryUnavailable = "resource sampling is unavailable"
				items[index].PIDsUnavailable = "resource sampling is unavailable"
				items[index].NetworkRXUnavailable = "resource sampling is unavailable"
				items[index].NetworkTXUnavailable = "resource sampling is unavailable"
				items[index].BlockReadUnavailable = "resource sampling is unavailable"
				items[index].BlockWriteUnavailable = "resource sampling is unavailable"
			}
		}
	} else {
		for index := range items {
			if items[index].Status == "running" {
				items[index].CPUUnavailable = "list stats callback is unavailable"
				items[index].MemoryUnavailable = "list stats callback is unavailable"
				items[index].PIDsUnavailable = "list stats callback is unavailable"
				items[index].NetworkRXUnavailable = "list stats callback is unavailable"
				items[index].NetworkTXUnavailable = "list stats callback is unavailable"
				items[index].BlockReadUnavailable = "list stats callback is unavailable"
				items[index].BlockWriteUnavailable = "list stats callback is unavailable"
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank() != items[j].rank() {
			return items[i].rank() < items[j].rank()
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, err
}

func inspectsID(inspects []ctrInspect, short string) string {
	for _, inspect := range inspects {
		id := inspect.ID
		if len(id) > ctrShortIDLength {
			id = id[:ctrShortIDLength]
		}
		if id == short {
			return inspect.ID
		}
	}
	return short
}

func applyCtrListStats(item *ctrListItem, sample ctrListStats) {
	limits := sample.Limits
	item.CPUPercent = limits.CPUUsagePercent
	item.CPUUnavailable = sample.CPUReason
	item.MemoryBytes = limits.Memory.Used
	item.MemoryLimitBytes = limits.Memory.Max
	item.MemoryUnlimited = limits.Memory.Unlimited
	item.MemoryLimitKnown = limits.Memory.Max != nil || limits.Memory.Unlimited
	item.MemoryUnavailable = limits.Memory.Reason
	if item.MemoryBytes != nil && limits.Memory.Max != nil && *limits.Memory.Max > 0 {
		percent := float64(*item.MemoryBytes) / float64(*limits.Memory.Max) * 100
		item.MemoryPercent = &percent
	}
	if item.MemoryBytes == nil || !item.MemoryLimitKnown {
		if item.MemoryUnavailable == "" {
			item.MemoryUnavailable = "memory usage or configured limit is unavailable"
		}
	}
	if limits.Pids.Status == "available" {
		value := limits.Pids.Current
		item.PIDs = &value
		item.PIDsUnavailable = limits.Pids.Reason
	} else {
		item.PIDsUnavailable = limits.Pids.Reason
		if item.PIDsUnavailable == "" {
			item.PIDsUnavailable = "pids current is unavailable"
		}
	}
	item.NetworkRXBytes, item.NetworkTXBytes = sample.NetworkRX, sample.NetworkTX
	item.NetworkRXUnavailable, item.NetworkTXUnavailable = sample.NetworkRXReason, sample.NetworkTXReason
	item.BlockReadBytes, item.BlockWriteBytes = sample.BlockRead, sample.BlockWrite
	item.BlockReadUnavailable, item.BlockWriteUnavailable = sample.BlockReadReason, sample.BlockWriteReason
}

// evaluateCtrList는 목록 전체를 결과 하나로 낸다. 실패한 container가 있으면 fail이고, 손볼 일이 있을 수 있는 container가 있으면
// warn이다. 예전에 재시작한 적만 있는 container는 위에 보이지만 경고하지 않는다.
func evaluateCtrList(items []ctrListItem, err error) Result {
	if errors.Is(err, errCtrNoDocker) {
		return unsupported(ctrProbeID, T("ctr.skip.no_docker"))
	}
	if err != nil {
		// docker socket을 열지 못한 것은 container의 문제가 아니다. 상세와 같이 skip으로 알린다.
		if strings.Contains(err.Error(), "permission denied") {
			result := svcResultFor(ctrProbeID, "list", StatusSkip, T("ctr.skip.needs_docker_access"))
			if items != nil {
				result.Metrics = map[string]interface{}{"containers": items}
			}
			return result
		}
		result := svcFailed(svcResultFor(ctrProbeID, "list", StatusFail, T("ctr.list.failed", infoSingleLine(err.Error()))))
		if items != nil {
			result.Metrics = map[string]interface{}{"containers": items}
		}
		return result
	}
	var failed []string
	attention := 0
	for _, item := range items {
		switch {
		case item.failed():
			failed = append(failed, item.Name+" ("+item.State+")")
		case item.attention():
			attention++
		}
	}
	status := StatusPass
	if attention > 0 {
		status = StatusWarn
	}
	result := svcResultFor(ctrProbeID, "list", status, T("ctr.list.summary", len(items), len(failed), attention))
	if len(failed) > 0 {
		result.Status = StatusFail
		result.Error = &DiagnosticError{Kind: "state", Message: strings.Join(failed[:min(len(failed), ctrListFailedShown)], ", ")}
		if hidden := len(failed) - ctrListFailedShown; hidden > 0 {
			result.Error.Message += fmt.Sprintf(" +%d", hidden)
		}
	}
	if items == nil {
		items = []ctrListItem{}
	}
	result.Metrics = map[string]interface{}{"containers": items, "failed": len(failed), "attention": attention}
	var unavailable []string
	for _, item := range items {
		for _, reason := range []string{item.MemoryUnavailable, item.CPUUnavailable, item.PIDsUnavailable, item.NetworkRXUnavailable, item.NetworkTXUnavailable, item.BlockReadUnavailable, item.BlockWriteUnavailable} {
			if reason != "" {
				unavailable = append(unavailable, item.Name+": "+reason)
			}
		}
	}
	if len(unavailable) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.unavailable"), Value: strings.Join(unavailable, "\n")})
	}
	return result
}

// ctrListCells는 표와 대화형 화면이 같이 쓰는 칸이다.
func ctrListCells(item ctrListItem) []string {
	uptime, memory := "—", "—"
	if item.UptimeS != nil {
		uptime = svcFormatUptime(time.Duration(*item.UptimeS) * time.Second)
	}
	if item.MemoryBytes != nil {
		limit := "?"
		if item.MemoryUnlimited {
			limit = "∞"
		} else if item.MemoryLimitBytes != nil {
			limit = formatCtrBytes(*item.MemoryLimitBytes)
		}
		memory = formatCtrBytes(*item.MemoryBytes) + "/" + limit
	} else if item.MemoryUnavailable != "" {
		memory = "n/a"
	}
	cpu := "—"
	if item.CPUPercent != nil {
		cpu = fmt.Sprintf("%.1f%%", *item.CPUPercent)
	} else if item.CPUUnavailable != "" {
		cpu = "n/a"
	}
	memPercent := "—"
	if item.MemoryPercent != nil {
		memPercent = fmt.Sprintf("%.1f%%", *item.MemoryPercent)
	} else if item.MemoryUnavailable != "" {
		memPercent = "n/a"
	}
	pids := "—"
	if item.PIDs != nil {
		pids = strconv.FormatUint(*item.PIDs, 10)
	} else if item.PIDsUnavailable != "" {
		pids = "n/a"
	}
	return []string{item.Name, item.State, cpu, memory, memPercent, pids, formatIO(item.NetworkRXBytes, item.NetworkTXBytes, item.NetworkRXUnavailable, item.NetworkTXUnavailable), formatIO(item.BlockReadBytes, item.BlockWriteBytes, item.BlockReadUnavailable, item.BlockWriteUnavailable), strconv.Itoa(item.Restarts), uptime, item.Image}
}

func formatIO(first, second *uint64, firstErr, secondErr string) string {
	a, b := "—", "—"
	if first != nil {
		a = formatCtrBytes(*first)
	} else if firstErr != "" {
		a = "n/a"
	}
	if second != nil {
		b = formatCtrBytes(*second)
	} else if secondErr != "" {
		b = "n/a"
	}
	return "↓" + a + " ↑" + b
}

func formatCtrBytes(value uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	amount := float64(value)
	unit := 0
	for amount >= 1024 && unit < len(units)-1 {
		amount /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%dB", value)
	}
	return fmt.Sprintf("%.1f%s", amount, units[unit])
}

func ctrListHeaders() []string {
	return []string{T("ctr.list.column.name"), T("ctr.list.column.state"), T("ctr.list.column.cpu"), T("ctr.list.column.memory"), T("ctr.list.column.memory_percent"), T("ctr.list.column.pids"), T("ctr.list.column.network"), T("ctr.list.column.block"), T("ctr.list.column.restarts"), T("ctr.list.column.uptime"), T("ctr.list.column.image")}
}

func formatCtrListTable(items []ctrListItem, style textStyle) string {
	if len(items) == 0 {
		return ""
	}
	rows := [][]string{ctrListHeaders()}
	colors := [][]string{nil}
	for _, item := range items {
		rows = append(rows, ctrListCells(item))
		colors = append(colors, []string{"", item.stateColor(), "", "", "", "", "", "", "", "", richGray})
	}
	return formatListTable(rows, colors, style)
}

// runCtrList는 container 없이 부른 edc ctr다. 터미널이면 목록에서 골라 상세를 여는 화면을 열고, 아니면 결과 한 줄과 표를 낸다.
func runCtrList(options commonOptions, version string, sources ctrSources, ok, all bool) int {
	if sources.close != nil {
		defer sources.close()
	}
	started := time.Now()
	target := map[string]interface{}{"all": all}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(ctrProbeID, T("ctr.skip.linux_only"))}, options.redact))
	}
	if options.jsonPath == "" && liveTerminal() && sources.docker() {
		return runCtrBrowser(sources, options.timeout, all)
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	items, err := collectCtrList(ctx, sources, all)
	result := evaluateCtrList(items, err)
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
	if table := formatCtrListTable(items, newTextStyle(color)); table != "" {
		fmt.Fprint(output, "\n"+table)
	}
	if options.redact {
		fmt.Fprint(os.Stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}
