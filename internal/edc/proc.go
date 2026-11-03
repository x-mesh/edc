package edc

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// proc은 process 하나를 한 화면에 보인다. top은 많은 process를 얕게, svc는 systemd unit 단위로 본다. 여기서는 PID 하나의
// 명령행, 자원, fd, 소켓, cgroup 한도를 함께 읽는다. 아무것도 바꾸지 않는다.

func init() {
	for _, id := range []string{"process", "resources", "fd", "sockets", "cgroup"} {
		evidenceShownProbes["proc."+id] = true
	}
}

const (
	// procSampleWindow는 CPU 사용률을 구하려고 두 번 읽는 간격이다.
	procSampleWindow = 500 * time.Millisecond
	// procCloseWaitWarn 이상 CLOSE_WAIT가 쌓이면 상대가 닫은 연결을 process가 닫지 않는 것이다.
	procCloseWaitWarn = 10
	// procListShown은 근거에 적는 항목 수다.
	procListShown = 10
	// procCommandMax는 -v가 아닐 때 명령행을 자르는 길이다. 셸 -c 명령은 수 KB가 되어 다른 줄을 밀어낸다.
	procCommandMax = 200
)

// procDetails는 OS별로 읽는 값이다. 읽지 못한 부분은 Err에 이유가 남는다.
type procDetails struct {
	Args []string
	CWD  string
	User string
	// FDTypes는 fd 종류별 수다. Linux는 file, socket, pipe, anon, device이고 macOS는 lsof의 TYPE이다.
	FDTypes map[string]int
	FDErr   error
	// Deleted는 지워졌지만 아직 열려 있는 파일이다. 디스크 공간을 붙잡는다. Linux에서만 읽는다.
	Deleted []procDeletedFile
	// Listen은 대기 소켓이고 Conns는 상태별 연결 수, Remotes는 상대 주소별 연결 수다.
	Listen    []string
	Conns     map[string]int
	Remotes   map[string]int
	SocketErr error
}

type procDeletedFile struct {
	Path string
	Size uint64
}

var (
	errProcNotFound    = errors.New("process not found")
	errProcUnsupported = errors.New("unsupported on this OS")
)

// selectProc는 인자가 숫자면 PID로, 아니면 명령 이름으로 고른다. 이름이 여럿에 맞으면 고르지 않고 후보를 돌려준다.
func selectProc(processes []topProcess, target string) (topProcess, []topProcess, error) {
	if pid, err := strconv.Atoi(target); err == nil {
		for _, process := range processes {
			if process.PID == pid {
				return process, nil, nil
			}
		}
		return topProcess{}, nil, errProcNotFound
	}
	var matches []topProcess
	for _, process := range processes {
		if procName(process.Command) == target {
			matches = append(matches, process)
		}
	}
	switch len(matches) {
	case 0:
		return topProcess{}, nil, errProcNotFound
	case 1:
		return matches[0], nil, nil
	}
	sort.Slice(matches, func(a, b int) bool { return matches[a].PID < matches[b].PID })
	return topProcess{}, matches, nil
}

// procName은 명령의 첫 단어에서 경로를 뗀 이름이다. top은 OS에 따라 이름이나 경로를 주므로 둘 다 맞춘다.
func procName(command string) string {
	first, _, _ := strings.Cut(command, " ")
	return filepath.Base(first)
}

func procResult(id string, status Status, summary string) Result {
	return Result{Probe: "proc." + id, Status: status, StartedAt: time.Now().UTC(), Summary: summary}
}

func procCandidatesResult(target string, matches []topProcess) Result {
	lines := make([]string, 0, procListShown+1)
	for _, process := range matches[:min(len(matches), procListShown)] {
		lines = append(lines, fmt.Sprintf("%7d  %5.1f%%  %8s  %s", process.PID, process.CPU, traceBytes(process.RSS), svcTruncate(process.Command, procCommandMax)))
	}
	if hidden := len(matches) - procListShown; hidden > 0 {
		lines = append(lines, fmt.Sprintf("+%d", hidden))
	}
	result := procResult("process", StatusFail, T("proc.many", len(matches), target))
	result.Error = &DiagnosticError{Kind: "select", Message: result.Summary}
	result.Evidence = []Evidence{{Label: T("proc.evidence.candidates"), Value: strings.Join(lines, "\n")}}
	result.Next = fmt.Sprintf("edc proc %d", matches[0].PID)
	return result
}

func evaluateProc(process topProcess, details procDetails, now time.Time, verbose bool) []Result {
	return []Result{
		procProcessResult(process, details, now, verbose),
		procResourcesResult(process),
		procFDResult(process, details),
		procSocketsResult(details),
		procCgroupResult(process),
	}
}

func procProcessResult(process topProcess, details procDetails, now time.Time, verbose bool) Result {
	parts := []string{fmt.Sprintf("%d %s", process.PID, procName(process.Command))}
	if details.User != "" {
		parts = append(parts, details.User)
	}
	if !process.Started.IsZero() {
		parts = append(parts, T("proc.up", svcFormatUptime(now.Sub(process.Started))))
	}
	status := StatusPass
	switch process.State {
	case "Z":
		status = StatusWarn
		parts = append(parts, T("proc.state.zombie"))
	case "D":
		status = StatusWarn
		parts = append(parts, T("proc.state.disk_wait"))
	}
	result := procResult("process", status, strings.Join(parts, " · "))
	command := process.Command
	if len(details.Args) > 0 {
		// 명령행의 비밀 값은 listen과 같은 규칙으로 가린다.
		command = strings.Join(listenMaskArgs(details.Args), " ")
	}
	if !verbose {
		command = svcTruncate(infoSingleLine(command), procCommandMax)
	}
	lines := []string{T("proc.field.command", command), T("proc.field.ppid", process.PPID)}
	if details.CWD != "" || process.CWD != "" {
		lines = append(lines, T("proc.field.cwd", firstNonEmpty(details.CWD, process.CWD)))
	}
	if process.Threads > 0 {
		lines = append(lines, T("proc.field.threads", process.Threads))
	}
	result.Evidence = []Evidence{{Label: T("proc.evidence.process"), Value: strings.Join(lines, "\n")}}
	if status == StatusWarn && process.State == "Z" {
		result.Next = fmt.Sprintf("edc proc %d", process.PPID)
	}
	return result
}

// procPeer는 연결의 상대를 묶는 열쇠다. 이 process의 대기 포트로 들어온 연결은 상대 포트가 임시 포트라 IP로 묶고, 나간
// 연결은 상대 서비스가 포트로 갈리므로 IP:포트로 묶는다.
func procPeer(local, remote string, listenPorts map[string]bool) string {
	if listenPorts[procPort(local)] {
		if index := strings.LastIndex(remote, ":"); index > 0 {
			return strings.Trim(remote[:index], "[]")
		}
	}
	return remote
}

func procPort(endpoint string) string {
	if index := strings.LastIndex(endpoint, ":"); index >= 0 {
		return endpoint[index+1:]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func procResourcesResult(process topProcess) Result {
	memory := traceBytes(process.RSS)
	if process.MemoryValid && process.Memory != process.RSS {
		memory = T("proc.memory_with_rss", traceBytes(process.Memory), traceBytes(process.RSS))
	}
	result := procResult("resources", StatusPass, T("proc.resources", process.CPU, memory))
	result.Metrics = map[string]interface{}{"cpu_percent": process.CPU, "rss_bytes": process.RSS}
	if process.MemoryValid {
		result.Metrics["memory_bytes"] = process.Memory
	}
	return result
}

func procFDResult(process topProcess, details procDetails) Result {
	if details.FDErr != nil {
		if errors.Is(details.FDErr, os.ErrPermission) {
			return procResult("fd", StatusSkip, T("proc.skip.needs_root", T("proc.source.fd")))
		}
		return procResult("fd", StatusSkip, T("proc.skip.unreadable", T("proc.source.fd"), infoSingleLine(details.FDErr.Error())))
	}
	used := 0
	for _, count := range details.FDTypes {
		used += count
	}
	status, summary := StatusPass, T("proc.fd.count", used)
	if limits := process.Limits; limits != nil && limits.FD.Soft != nil && !limits.FD.Unlimited && *limits.FD.Soft > 0 {
		percent := float64(used) / float64(*limits.FD.Soft) * 100
		summary = T("proc.fd.limit", used, *limits.FD.Soft, percent)
		if percent >= svcFDWarnPercent {
			status = StatusWarn
		}
	}
	var lines []string
	for _, kind := range slices.Sorted(maps.Keys(details.FDTypes)) {
		lines = append(lines, fmt.Sprintf("%-8s %d", kind, details.FDTypes[kind]))
	}
	result := procResult("fd", status, summary)
	result.Evidence = []Evidence{{Label: T("proc.evidence.fd_types"), Value: strings.Join(lines, "\n")}}
	if len(details.Deleted) > 0 {
		var total uint64
		deleted := slices.Clone(details.Deleted)
		sort.Slice(deleted, func(a, b int) bool { return deleted[a].Size > deleted[b].Size })
		var rows []string
		for index, file := range deleted {
			total += file.Size
			if index < procListShown {
				rows = append(rows, fmt.Sprintf("%8s  %s", traceBytes(file.Size), file.Path))
			}
		}
		result.Summary += T("proc.fd.deleted", len(deleted), traceBytes(total))
		result.Evidence = append(result.Evidence, Evidence{Label: T("proc.evidence.deleted"), Value: strings.Join(rows, "\n")})
	}
	return result
}

func procSocketsResult(details procDetails) Result {
	if details.SocketErr != nil {
		if errors.Is(details.SocketErr, os.ErrPermission) {
			return procResult("sockets", StatusSkip, T("proc.skip.needs_root", T("proc.source.sockets")))
		}
		return procResult("sockets", StatusSkip, T("proc.skip.unreadable", T("proc.source.sockets"), infoSingleLine(details.SocketErr.Error())))
	}
	connections := 0
	var states []string
	for _, state := range slices.Sorted(maps.Keys(details.Conns)) {
		connections += details.Conns[state]
		states = append(states, fmt.Sprintf("%s %d", state, details.Conns[state]))
	}
	listen := T("proc.sockets.no_listen")
	// IPv4와 IPv6에 같이 bind한 소켓은 macOS lsof에서 같은 *:port로 두 번 온다.
	if labels := slices.Compact(slices.Sorted(slices.Values(details.Listen))); len(labels) > 0 {
		listen = strings.Join(labels, ", ")
	}
	status := StatusPass
	summary := T("proc.sockets.summary", listen, connections)
	if waiting := details.Conns["CLOSE_WAIT"]; waiting >= procCloseWaitWarn {
		status = StatusWarn
		summary += T("proc.sockets.close_wait", waiting)
	}
	result := procResult("sockets", status, summary)
	if len(states) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("proc.evidence.states"), Value: strings.Join(states, "\n")})
	}
	if len(details.Remotes) > 0 {
		remotes := slices.Collect(maps.Keys(details.Remotes))
		sort.Slice(remotes, func(a, b int) bool {
			if details.Remotes[remotes[a]] != details.Remotes[remotes[b]] {
				return details.Remotes[remotes[a]] > details.Remotes[remotes[b]]
			}
			return remotes[a] < remotes[b]
		})
		var rows []string
		for _, remote := range remotes[:min(len(remotes), procListShown)] {
			rows = append(rows, fmt.Sprintf("%5d  %s", details.Remotes[remote], remote))
		}
		result.Evidence = append(result.Evidence, Evidence{Label: T("proc.evidence.remotes"), Value: strings.Join(rows, "\n")})
	}
	return result
}

// procCgroupResult는 Linux의 cgroup v2 한도다. top의 수집기가 채운 값을 판정만 한다.
func procCgroupResult(process topProcess) Result {
	limits := process.Limits
	if limits == nil || limits.Cgroup.Status != "available" {
		reason := T("proc.cgroup.linux_only")
		if limits != nil && limits.Cgroup.Reason != "" {
			reason = limits.Cgroup.Reason
		}
		return procResult("cgroup", StatusSkip, reason)
	}
	cgroup := limits.Cgroup
	parts := []string{cgroup.Path}
	status := StatusPass
	if memory := cgroup.Memory; memory.Used != nil {
		switch {
		case memory.Max != nil && !memory.Unlimited && *memory.Max > 0:
			percent := float64(*memory.Used) / float64(*memory.Max) * 100
			parts = append(parts, T("proc.cgroup.memory_limit", traceBytes(*memory.Used), traceBytes(*memory.Max), percent))
			if percent >= checkUsageLimits.warn {
				status = StatusWarn
			}
		default:
			parts = append(parts, T("proc.cgroup.memory", traceBytes(*memory.Used)))
		}
	}
	if events := cgroup.Events; events.OOMKill != nil && *events.OOMKill > 0 {
		status = StatusWarn
		parts = append(parts, T("proc.cgroup.oom_kill", *events.OOMKill))
	}
	if cpu := cgroup.CPU; cpu.ThrottledPeriods != nil && *cpu.ThrottledPeriods > 0 {
		parts = append(parts, T("proc.cgroup.throttled", *cpu.ThrottledPeriods))
	}
	return procResult("cgroup", status, strings.Join(parts, " · "))
}

func runProc(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("proc")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc proc <pid|name> [options]"))
		return 2
	}
	started := time.Now()
	read := newTopProcessReader()
	// CPU 사용률은 두 번 읽은 차이다. 첫 읽기는 기준만 남기고, Linux는 이때 결과가 없다고 답한다.
	read()
	time.Sleep(procSampleWindow)
	processes, ok := read()
	if !ok {
		fmt.Fprintln(os.Stderr, T("proc.error.unreadable"))
		return 1
	}
	target := set.Arg(0)
	process, matches, err := selectProc(processes, target)
	switch {
	case errors.Is(err, errProcNotFound):
		fmt.Fprintln(os.Stderr, T("proc.error.not_found", target))
		return 1
	case len(matches) > 0:
		return emit(options, buildReport(version, started, map[string]interface{}{"process": target}, []Result{procCandidatesResult(target, matches)}, options.redact))
	}
	selected := []topProcess{process}
	newTopProcessEnricher()(selected)
	details := readProcDetails(process.PID)
	results := evaluateProc(selected[0], details, time.Now(), options.verbose)
	return emit(options, buildReport(version, started, map[string]interface{}{"process": target, "pid": process.PID}, results, options.redact))
}
