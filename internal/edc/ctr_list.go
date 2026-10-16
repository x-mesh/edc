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
	Name        string  `json:"name"`
	ID          string  `json:"id"`
	Image       string  `json:"image"`
	Status      string  `json:"status"`
	Health      string  `json:"health,omitempty"`
	State       string  `json:"state"`
	ExitCode    int     `json:"exit_code"`
	OOMKilled   bool    `json:"oom_killed,omitempty"`
	Restarts    int     `json:"restart_count"`
	PID         int     `json:"pid,omitempty"`
	UptimeS     *int64  `json:"uptime_s,omitempty"`
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
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
	output, err := sources.listIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, infoSingleLine(firstLine(output)))
	}
	ids := strings.Fields(output)
	if len(ids) == 0 {
		return nil, nil
	}
	// 목록을 읽은 뒤 사라진 container가 있으면 docker inspect는 실패하지만 나머지 줄은 낸다. 읽은 줄은 쓴다.
	output, err = sources.inspectMany(ctx, ids)
	now := sources.now()
	var items []ctrListItem
	for _, line := range strings.Split(output, "\n") {
		inspect, parseErr := parseCtrInspect(line)
		if parseErr != nil {
			continue
		}
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
		if inspect.PID > 0 {
			if path, err := sources.cgroupPath(inspect.PID); err == nil {
				item.MemoryBytes = sources.limits(path).Memory.Used
			}
		}
		items = append(items, item)
	}
	if len(items) == 0 && err != nil {
		return nil, fmt.Errorf("%w: %s", err, infoSingleLine(firstLine(output)))
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank() != items[j].rank() {
			return items[i].rank() < items[j].rank()
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, nil
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
			return svcResultFor(ctrProbeID, "list", StatusSkip, T("ctr.skip.needs_docker_access"))
		}
		return svcFailed(svcResultFor(ctrProbeID, "list", StatusFail, T("ctr.list.failed", infoSingleLine(err.Error()))))
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
	return result
}

// ctrListCells는 표와 대화형 화면이 같이 쓰는 칸이다.
func ctrListCells(item ctrListItem) []string {
	uptime, memory := "—", "—"
	if item.UptimeS != nil {
		uptime = svcFormatUptime(time.Duration(*item.UptimeS) * time.Second)
	}
	if item.MemoryBytes != nil {
		memory = formatProcessRSS(*item.MemoryBytes)
	}
	return []string{item.Name, item.State, strconv.Itoa(item.Restarts), uptime, memory, item.Image}
}

func ctrListHeaders() []string {
	return []string{T("ctr.list.column.name"), T("ctr.list.column.state"), T("ctr.list.column.restarts"), T("ctr.list.column.uptime"), T("ctr.list.column.memory"), T("ctr.list.column.image")}
}

func formatCtrListTable(items []ctrListItem, style textStyle) string {
	if len(items) == 0 {
		return ""
	}
	rows := [][]string{ctrListHeaders()}
	colors := [][]string{nil}
	for _, item := range items {
		rows = append(rows, ctrListCells(item))
		colors = append(colors, []string{"", item.stateColor(), "", "", "", richGray})
	}
	return formatListTable(rows, colors, style)
}

// runCtrList는 container 없이 부른 edc ctr다. 터미널이면 목록에서 골라 상세를 여는 화면을 열고, 아니면 결과 한 줄과 표를 낸다.
func runCtrList(options commonOptions, version string, sources ctrSources, ok, all bool) int {
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
