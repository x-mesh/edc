package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// svc는 systemd unit 하나의 상태를 한 화면에 모은다. 읽기만 하고 unit을 시작하거나 멈추지 않는다.

const (
	svcProbeID = "svc"
	// svcMaxRows는 화면에 남기는 process 수다. Delegate=yes unit은 process가 수백 개라 개수와 RSS는 전체로 센다.
	svcMaxRows          = 10
	svcFDWarnPercent    = 80
	svcJournalLines     = 5
	svcJournalLineRunes = 160
	svcListenSummaryMax = 5
	// svcCPUSampleGap은 CPU tick을 두 번 읽는 간격이다. 한 번만 읽으면 process 수명 전체의 평균이 된다.
	svcCPUSampleGap = 500 * time.Millisecond
)

// svcShowProperties는 systemctl show로 읽는 속성이다. 순서는 출력에 영향이 없다.
var svcShowProperties = []string{"Id", "LoadState", "ActiveState", "SubState", "Result", "MainPID", "NRestarts", "ActiveEnterTimestampMonotonic", "ControlGroup"}

// errSvcNeedsRoot는 root가 아니라 읽지 못한 원천이다. 확인하지 못한 항목을 통과로 보이지 않게 skip으로 둔다.
var errSvcNeedsRoot = errors.New("needs root")

var errSvcInvalidUnit = errors.New("invalid unit name")

// svcOtherUnitTypes는 service가 아닌 unit 접미사다. 이 unit에는 MainPID와 process가 없어 화면이 비게 된다.
var svcOtherUnitTypes = []string{".socket", ".timer", ".target", ".mount", ".path", ".slice", ".scope", ".device", ".swap", ".automount"}

// svcLoadErrors는 unit 파일을 읽지 못한 LoadState다. not-found는 runSvc가 따로 오류로 끝낸다.
var svcLoadErrors = []string{"not-found", "error", "bad-setting", "masked"}

// svcSources는 svc가 읽는 원천이다. 테스트는 함수들을 가짜로 넣는다.
type svcSources struct {
	euid    int
	systemd func() bool
	show    func(ctx context.Context, unit string) (string, error)
	// processes는 unit의 cgroup에 속한 process를 CPU% 순으로 돌려준다.
	processes func(ctx context.Context, controlGroup string, mainPID int) ([]topProcess, error)
	fdLimit   func(pid int) topFDLimit
	listen    func() ([]listenSocket, error)
	journal   func(ctx context.Context, unit string) (stdout, stderr string, err error)
	// monotonic은 CLOCK_MONOTONIC이다. ActiveEnterTimestampMonotonic과 같은 시계라 시각 변경에 흔들리지 않는다.
	monotonic func() (time.Duration, bool)
	// list는 service unit 목록이고, showUnits는 여러 unit의 속성을 한 번에 읽는다. unit마다 systemctl을 부르면 수백 번이 된다.
	list      func(ctx context.Context) (string, error)
	showUnits func(ctx context.Context, units []string) (string, error)
}

type svcShow struct {
	ID, LoadState, ActiveState, SubState, Result, ControlGroup string
	// MainPID가 0이면 main process가 없다.
	MainPID int
	// NRestarts는 오래된 systemd에 없다. nil이면 모른다.
	NRestarts *uint64
	// ActiveEnterMonotonic은 마지막으로 active가 된 CLOCK_MONOTONIC 마이크로초다. 0이면 모른다.
	ActiveEnterMonotonic uint64
	// MemoryCurrent는 unit cgroup의 메모리다. memory accounting이 꺼져 있으면 nil이다.
	MemoryCurrent *uint64
	Description   string
}

type svcJournal struct {
	Lines []string
	// Hidden은 journalctl이 다른 사용자와 시스템의 메시지를 숨겼다는 hint를 냈다는 뜻이다.
	Hidden bool
}

type svcFacts struct {
	Unit    string
	Root    bool
	Systemd bool
	// Probe는 결과 ID의 접두사다. 비어 있으면 svc이고, edc ctr은 같은 builder를 ctr로 쓴다.
	Probe string

	Show       svcShow
	ShowOutput string
	ShowErr    error

	Uptime      time.Duration
	UptimeKnown bool

	// Processes는 unit의 모든 process다. 화면에는 svcRows가 고른 것만 나온다.
	Processes  []topProcess
	ProcessErr error
	FD         map[int]topFDLimit

	// Listen은 호스트의 모든 소켓이다. unit process가 가진 것만 evaluateSvc가 고른다.
	Listen    []listenSocket
	ListenErr error

	Journal    svcJournal
	JournalErr error
}

func runSvc(args []string, version string) int {
	sources, ok := hostSvcSources()
	return runSvcWith(args, version, sources, ok)
}

func runSvcWith(args []string, version string, sources svcSources, ok bool) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("svc", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	all := set.Bool("all", false, T("command.svc.option.all"))
	// `edc svc nginx -v`처럼 unit 뒤에 온 flag도 받는다. flag 패키지는 첫 위치 인자에서 멈춘다.
	ordered, err := reorderFSWatchArgs(set, args)
	if err == nil {
		err = set.Parse(ordered)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	usage := T("cli.usage", "edc svc [options] [--all] | edc svc [options] <unit>")
	if set.NArg() == 0 {
		return runSvcList(options, version, sources, ok, *all)
	}
	// --all은 목록에서 멈춘 unit까지 보이는 option이다. unit 하나를 볼 때 받으면 무엇이 바뀌는지 오해한다.
	if set.NArg() != 1 || *all {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	unit, err := normalizeSvcUnit(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("svc.error.invalid_unit", set.Arg(0)))
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	started := time.Now()
	target := map[string]interface{}{"unit": unit}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(svcProbeID, T("svc.skip.linux_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	facts := collectSvcFacts(ctx, unit, sources)
	if facts.ShowErr == nil && facts.Show.LoadState == "not-found" {
		fmt.Fprintln(os.Stderr, T("svc.error.not_found", unit))
		return 1
	}
	return emit(options, buildReport(version, started, target, evaluateSvc(facts), options.redact))
}

// normalizeSvcUnit은 사용자가 준 이름을 systemctl과 journalctl에 넘겨도 되는 service 이름으로 바꾼다.
// '-'로 시작하면 option이 되고, glob 문자는 pattern이 되므로 명령을 실행하기 전에 막는다.
func normalizeSvcUnit(input string) (string, error) {
	if input == "" || strings.HasPrefix(input, "-") {
		return "", errSvcInvalidUnit
	}
	for _, char := range input {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune(":_.@-", char)) {
			return "", errSvcInvalidUnit
		}
	}
	for _, suffix := range svcOtherUnitTypes {
		if strings.HasSuffix(input, suffix) {
			return "", errSvcInvalidUnit
		}
	}
	name := strings.TrimSuffix(input, ".service")
	if name == "" || strings.HasPrefix(name, "-") {
		return "", errSvcInvalidUnit
	}
	return name + ".service", nil
}

// parseSvcShow는 systemctl show의 KEY=VALUE 줄을 읽는다. 값에도 '='이 들어갈 수 있어 첫 '='에서만 나눈다.
// 숫자를 읽지 못하면 모르는 값으로 둔다.
func parseSvcShow(output string) svcShow {
	var show svcShow
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !found {
			continue
		}
		switch key {
		case "Id":
			show.ID = value
		case "LoadState":
			show.LoadState = value
		case "ActiveState":
			show.ActiveState = value
		case "SubState":
			show.SubState = value
		case "Result":
			show.Result = value
		case "ControlGroup":
			show.ControlGroup = value
		case "MainPID":
			if pid, err := strconv.Atoi(value); err == nil && pid > 0 {
				show.MainPID = pid
			}
		case "NRestarts":
			if count, err := strconv.ParseUint(value, 10, 64); err == nil {
				show.NRestarts = &count
			}
		case "ActiveEnterTimestampMonotonic":
			if stamp, err := strconv.ParseUint(value, 10, 64); err == nil {
				show.ActiveEnterMonotonic = stamp
			}
		case "MemoryCurrent":
			// accounting이 꺼져 있으면 "[not set]"이고, 오래된 systemd는 2^64-1을 낸다. 둘 다 모르는 값이다.
			if memory, err := strconv.ParseUint(value, 10, 64); err == nil && memory != ^uint64(0) {
				show.MemoryCurrent = &memory
			}
		case "Description":
			show.Description = value
		}
	}
	return show
}

// svcUptime은 마지막으로 active가 된 뒤의 시간이다. active가 아니면 그 값은 지난 실행의 것이라 쓰지 않는다.
func svcUptime(show svcShow, now time.Duration, nowKnown bool) (time.Duration, bool) {
	if show.ActiveState != "active" || show.ActiveEnterMonotonic == 0 || !nowKnown {
		return 0, false
	}
	entered := time.Duration(show.ActiveEnterMonotonic) * time.Microsecond
	if now < entered {
		return 0, false
	}
	return now - entered, true
}

// svcCgroupPath는 /proc/<pid>/cgroup에서 systemd가 관리하는 경로를 고른다. cgroup v2는 "0::" 줄 하나다.
// v1과 hybrid는 systemd가 name=systemd 계층에 unit 경로를 두고, hybrid의 0:: 줄은 "/"일 수 있다.
func svcCgroupPath(data string) string {
	var unified, named string
	for _, line := range strings.Split(data, "\n") {
		hierarchy, rest, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		controllers, path, found := strings.Cut(rest, ":")
		if !found {
			continue
		}
		if hierarchy == "0" && controllers == "" {
			unified = path
		} else if slices.Contains(strings.Split(controllers, ","), "name=systemd") {
			named = path
		}
	}
	if unified != "" && unified != "/" {
		return unified
	}
	if named != "" {
		return named
	}
	return unified
}

// svcCgroupMember는 process가 unit의 cgroup 안에 있는지다. 접두사만 보면 foo.service가 foo.service-x를 삼키므로
// 같은 경로이거나 그 아래 경로일 때만 맞는다.
func svcCgroupMember(cgroupFileData, controlGroup string) bool {
	if controlGroup == "" || controlGroup == "/" {
		return false
	}
	path := svcCgroupPath(cgroupFileData)
	return path == controlGroup || strings.HasPrefix(path, controlGroup+"/")
}

// filterSvcJournal은 journalctl 출력에서 기록 줄만 남긴다. "-- No entries --" 같은 표시 줄은 기록이 아니다.
// 권한이 없는 사용자에게 journalctl은 실패하지 않고 hint만 남기므로 그 hint를 따로 찾는다.
func filterSvcJournal(stdout, stderr string) svcJournal {
	var journal svcJournal
	for _, line := range strings.Split(stderr+"\n"+stdout, "\n") {
		if strings.Contains(line, "not seeing messages") || strings.Contains(line, "insufficient permissions") {
			journal.Hidden = true
		}
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "-- ") || strings.HasPrefix(line, "Hint: ") {
			continue
		}
		journal.Lines = append(journal.Lines, svcTruncate(line, svcJournalLineRunes))
	}
	if len(journal.Lines) > svcJournalLines {
		journal.Lines = journal.Lines[len(journal.Lines)-svcJournalLines:]
	}
	return journal
}

func svcTruncate(line string, limit int) string {
	runes := []rune(line)
	if len(runes) <= limit {
		return line
	}
	return string(runes[:limit]) + "…"
}

func collectSvcFacts(ctx context.Context, unit string, sources svcSources) svcFacts {
	facts := svcFacts{Unit: unit, Root: sources.euid == 0, Systemd: sources.systemd()}
	if !facts.Systemd {
		return facts
	}
	facts.ShowOutput, facts.ShowErr = sources.show(ctx, unit)
	if facts.ShowErr != nil {
		return facts
	}
	facts.Show = parseSvcShow(facts.ShowOutput)
	if slices.Contains(svcLoadErrors, facts.Show.LoadState) {
		return facts
	}
	now, nowKnown := sources.monotonic()
	facts.Uptime, facts.UptimeKnown = svcUptime(facts.Show, now, nowKnown)

	if facts.Show.ControlGroup != "" {
		facts.Processes, facts.ProcessErr = sources.processes(ctx, facts.Show.ControlGroup, facts.Show.MainPID)
	}
	facts.FD = map[int]topFDLimit{}
	for _, process := range svcRows(facts.Processes, facts.Show.MainPID) {
		facts.FD[process.PID] = sources.fdLimit(process.PID)
	}
	if len(facts.Processes) > 0 && !svcListenHidden(facts) {
		facts.Listen, facts.ListenErr = sources.listen()
	}

	stdout, stderr, err := sources.journal(ctx, unit)
	switch {
	case ctx.Err() != nil:
		facts.JournalErr = fmt.Errorf("journalctl: %w", ctx.Err())
	case errors.Is(err, exec.ErrNotFound):
		facts.JournalErr = err
	default:
		facts.Journal = filterSvcJournal(stdout, stderr)
		if facts.Journal.Hidden && !facts.Root {
			facts.JournalErr = errSvcNeedsRoot
		} else if err != nil {
			facts.JournalErr = fmt.Errorf("journalctl: %w: %s", err, infoSingleLine(firstLine(stderr)))
		}
	}
	return facts
}

// svcRows는 화면에 남길 process다. main process는 CPU가 낮아도 맨 앞에 둔다.
func svcRows(processes []topProcess, mainPID int) []topProcess {
	rows := make([]topProcess, 0, min(len(processes), svcMaxRows))
	for _, process := range processes {
		if process.PID == mainPID {
			rows = append(rows, process)
		}
	}
	for _, process := range processes {
		if len(rows) == svcMaxRows {
			break
		}
		if process.PID != mainPID {
			rows = append(rows, process)
		}
	}
	return rows
}

// svcListenHidden은 소켓 주인을 알 수 없는 경우다. 다른 계정의 fd를 읽지 못하면 소켓에 주인이 붙지 않아
// "열린 소켓 없음"이 거짓 통과가 된다.
func svcListenHidden(facts svcFacts) bool {
	if facts.Root {
		return false
	}
	for _, limit := range facts.FD {
		if limit.Status == "permission_denied" {
			return true
		}
	}
	return false
}

func (facts svcFacts) probe() string {
	if facts.Probe == "" {
		return svcProbeID
	}
	return facts.Probe
}

// key는 접두사에 따라 다른 문구다. unit과 container는 "실행 중인 process가 없다"를 다르게 말한다.
func (facts svcFacts) key(name string) string { return facts.probe() + "." + name }

func svcResult(id string, status Status, summary string) Result {
	return svcResultFor(svcProbeID, id, status, summary)
}

func svcResultFor(probe, id string, status Status, summary string) Result {
	return Result{Probe: probe + "." + id, Status: status, StartedAt: time.Now().UTC(), Summary: summary}
}

func svcSkipFor(probe, id, source string, err error) Result {
	if errors.Is(err, errSvcNeedsRoot) {
		return svcResultFor(probe, id, StatusSkip, T("svc.skip.needs_root", source))
	}
	return svcResultFor(probe, id, StatusSkip, T("svc.skip.unreadable", source, infoSingleLine(err.Error())))
}

func evaluateSvc(facts svcFacts) []Result {
	if !facts.Systemd {
		return []Result{unsupported(svcProbeID, T("svc.skip.no_systemd"))}
	}
	if facts.ShowErr != nil {
		detail := facts.ShowOutput
		if detail == "" {
			detail = facts.ShowErr.Error()
		}
		result := svcResultFor(facts.probe(), "state", StatusFail, T("svc.state.show_failed", infoSingleLine(firstLine(detail))))
		result.Error = &DiagnosticError{Kind: "command", Message: strings.TrimSpace(detail)}
		return []Result{result}
	}
	state := svcStateResult(facts)
	if slices.Contains(svcLoadErrors, facts.Show.LoadState) {
		return []Result{state}
	}
	return []Result{state, svcProcessesResult(facts), svcFDResult(facts), svcListenResult(facts), svcJournalResult(facts)}
}

// svcFailed는 실패 상자에 원인을 채운다. Error가 비면 상자에 phase와 cause 줄이 없다.
func svcFailed(result Result) Result {
	result.Error = &DiagnosticError{Kind: "state", Message: result.Summary}
	return result
}

func svcStateResult(facts svcFacts) Result {
	show := facts.Show
	if show.LoadState != "loaded" {
		return svcFailed(svcResultFor(facts.probe(), "state", StatusFail, T("svc.state.load", facts.Unit, show.LoadState)))
	}
	state := show.ActiveState + " (" + show.SubState + ")"
	if show.Result != "" && show.Result != "success" {
		state += ", result " + show.Result
	}
	uptime := "-"
	if facts.UptimeKnown {
		uptime = svcFormatUptime(facts.Uptime)
	}
	restarts := "-"
	if show.NRestarts != nil {
		restarts = strconv.FormatUint(*show.NRestarts, 10)
	}
	status := StatusWarn
	switch show.ActiveState {
	case "active":
		status = StatusPass
	case "failed":
		status = StatusFail
	}
	if status == StatusPass && show.NRestarts != nil && *show.NRestarts > 0 {
		status = StatusWarn
	}
	result := svcResultFor(facts.probe(), "state", status, T("svc.state.summary", state, uptime, restarts))
	if status == StatusFail {
		result = svcFailed(result)
	}
	result.Metrics = map[string]interface{}{"active_state": show.ActiveState, "sub_state": show.SubState, "result": show.Result, "main_pid": show.MainPID}
	if show.NRestarts != nil {
		result.Metrics["n_restarts"] = *show.NRestarts
	}
	if facts.UptimeKnown {
		result.Metrics["uptime_s"] = int64(facts.Uptime.Seconds())
	}
	return result
}

// svcFormatUptime은 큰 단위 두 개만 쓴다. 초 단위까지 적으면 한 줄이 길어진다.
func svcFormatUptime(value time.Duration) string {
	days := int(value.Hours()) / 24
	hours := int(value.Hours()) % 24
	minutes := int(value.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", int(value.Seconds()))
}

func svcProcessesResult(facts svcFacts) Result {
	if facts.ProcessErr != nil {
		return svcSkipFor(facts.probe(), "processes", "/proc", facts.ProcessErr)
	}
	if len(facts.Processes) == 0 {
		return svcResultFor(facts.probe(), "processes", StatusSkip, T(facts.key("skip.not_running")))
	}
	var rss uint64
	var cpu float64
	for _, process := range facts.Processes {
		rss += process.RSS
		cpu += process.CPU
	}
	rows := svcRows(facts.Processes, facts.Show.MainPID)
	lines := make([]string, 0, len(rows))
	for _, process := range rows {
		line := fmt.Sprintf("%7d  %5.1f%%  %8s  %s", process.PID, process.CPU, traceBytes(process.RSS), process.Command)
		if process.PID == facts.Show.MainPID {
			line += "  (main)"
		}
		lines = append(lines, line)
	}
	result := svcResultFor(facts.probe(), "processes", StatusPass, T("svc.processes.summary", len(facts.Processes), cpu, traceBytes(rss)))
	result.Metrics = map[string]interface{}{"count": len(facts.Processes), "rss_bytes": rss, "cpu_percent": cpu}
	result.Evidence = []Evidence{{Label: T("svc.evidence.processes", len(rows), len(facts.Processes)), Value: strings.Join(lines, "\n")}}
	return result
}

func svcFDResult(facts svcFacts) Result {
	rows := svcRows(facts.Processes, facts.Show.MainPID)
	if facts.ProcessErr != nil || len(rows) == 0 {
		return svcResultFor(facts.probe(), "fd", StatusSkip, T(facts.key("skip.not_running")))
	}
	var lines, high []string
	denied, failed, readable := 0, 0, 0
	maxPercent := -1.0
	maxLine := ""
	for _, process := range rows {
		limit := facts.FD[process.PID]
		switch {
		case limit.Status == "permission_denied":
			denied++
			lines = append(lines, fmt.Sprintf("%7d  %s", process.PID, T("svc.fd.denied")))
			continue
		case limit.Status != "available" || limit.Used == nil:
			failed++
			lines = append(lines, fmt.Sprintf("%7d  %s", process.PID, infoSingleLine(limit.Reason)))
			continue
		}
		readable++
		used := *limit.Used
		if limit.Unlimited || limit.Soft == nil || *limit.Soft == 0 {
			lines = append(lines, fmt.Sprintf("%7d  %d/%s", process.PID, used, T("svc.fd.no_limit")))
			continue
		}
		percent := float64(used) / float64(*limit.Soft) * 100
		line := fmt.Sprintf("%7d  %d/%d (%.0f%%)  %s", process.PID, used, *limit.Soft, percent, process.Command)
		lines = append(lines, line)
		if percent >= svcFDWarnPercent {
			high = append(high, strings.TrimSpace(line))
		}
		if percent > maxPercent {
			maxPercent, maxLine = percent, strings.TrimSpace(line)
		}
	}
	var result Result
	switch {
	case len(high) > 0:
		result = svcResultFor(facts.probe(), "fd", StatusWarn, T("svc.fd.high", len(high), high[0]))
		if denied > 0 {
			result.Warnings = []string{T("svc.skip.needs_root", T("svc.source.fd"))}
		}
	case denied > 0:
		// 일부만 읽었다면 읽지 못한 process가 한계에 가까울 수 있어 통과로 두지 않는다.
		result = svcResultFor(facts.probe(), "fd", StatusSkip, T("svc.skip.needs_root", T("svc.source.fd")))
	case failed > 0:
		result = svcResultFor(facts.probe(), "fd", StatusSkip, T("svc.skip.unreadable", T("svc.source.fd"), lines[0]))
	case maxLine == "":
		result = svcResultFor(facts.probe(), "fd", StatusPass, T("svc.fd.unlimited", readable))
	default:
		result = svcResultFor(facts.probe(), "fd", StatusPass, T("svc.fd.ok", maxLine))
	}
	result.Evidence = []Evidence{{Label: T("svc.evidence.fd"), Value: strings.Join(lines, "\n")}}
	return result
}

func svcListenResult(facts svcFacts) Result {
	switch {
	case facts.ProcessErr != nil || len(facts.Processes) == 0:
		return svcResultFor(facts.probe(), "listen", StatusSkip, T(facts.key("skip.not_running")))
	case svcListenHidden(facts):
		return svcSkipFor(facts.probe(), "listen", T("svc.source.sockets"), errSvcNeedsRoot)
	case facts.ListenErr != nil:
		return svcSkipFor(facts.probe(), "listen", T("svc.source.sockets"), facts.ListenErr)
	}
	members := map[string]bool{}
	for _, process := range facts.Processes {
		members[strconv.Itoa(process.PID)] = true
	}
	var labels, lines []string
	for _, socket := range facts.Listen {
		if (socket.Proto != "tcp" && socket.Proto != "udp") || !members[socket.PID] {
			continue
		}
		label := fmt.Sprintf("%s %s:%d", socket.Proto, socket.Address, socket.Port)
		if !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
		lines = append(lines, fmt.Sprintf("%s  %s %s", label, socket.PID, socket.Process))
	}
	if len(labels) == 0 {
		return svcResultFor(facts.probe(), "listen", StatusPass, T(facts.key("listen.none")))
	}
	shown := labels
	if len(shown) > svcListenSummaryMax {
		shown = append(slices.Clone(shown[:svcListenSummaryMax]), fmt.Sprintf("+%d", len(labels)-svcListenSummaryMax))
	}
	result := svcResultFor(facts.probe(), "listen", StatusPass, T("svc.listen.summary", strings.Join(shown, ", ")))
	result.Metrics = map[string]interface{}{"sockets": len(labels)}
	result.Evidence = []Evidence{{Label: T("svc.evidence.sockets"), Value: strings.Join(lines, "\n")}}
	return result
}

func svcJournalResult(facts svcFacts) Result {
	if facts.JournalErr != nil {
		return svcSkipFor(facts.probe(), "journal", "journal", facts.JournalErr)
	}
	lines := facts.Journal.Lines
	if len(lines) == 0 {
		return svcResultFor(facts.probe(), "journal", StatusPass, T("svc.journal.none"))
	}
	result := svcResultFor(facts.probe(), "journal", StatusWarn, T("svc.journal.errors", len(lines), lines[len(lines)-1]))
	result.Evidence = []Evidence{{Label: T("svc.evidence.journal"), Value: strings.Join(lines, "\n")}}
	return result
}
