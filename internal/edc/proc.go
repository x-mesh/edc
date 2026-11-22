package edc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
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
	// Origin은 process가 어디서 왔는지다. Linux에서만 읽는다.
	Origin procOrigin
}

// procOrigin은 실행 파일, 그 파일을 가진 dpkg 패키지, process를 띄운 로그인 세션이다.
type procOrigin struct {
	Exe string
	// PackageDB는 dpkg 파일 목록이 있는 호스트다. 그러면 Package가 비었을 때 어느 패키지에도 없는 파일이라고 적을 수 있다.
	PackageDB bool
	Package   string
	// PackageChanged는 dpkg가 그 패키지의 파일 목록을 마지막으로 쓴 때다. dpkg는 설치와 업그레이드 때 목록을 다시 쓴다.
	PackageChanged time.Time
	Session        *procLoginSession
}

// procLoginSession은 systemd-logind가 /run/systemd/sessions/<id>에 남긴 세션 기록이다.
type procLoginSession struct {
	ID, User, RemoteHost, Service, State string
	Start                                time.Time
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
	results := []Result{
		procProcessResult(process, details, now, verbose),
		procResourcesResult(process),
		procFDResult(process, details),
		procSocketsResult(details),
		procCgroupResult(process),
	}
	results[0] = withCollection(results[0], collectionObserved, intPointer(1), "")
	results[1] = withCollection(results[1], collectionObserved, intPointer(1), "")
	fdCount := 0
	for _, count := range details.FDTypes {
		fdCount += count
	}
	results[2] = procSourceCollection(results[2], details.FDErr, fdCount)
	socketCount := len(details.Listen)
	for _, count := range details.Conns {
		socketCount += count
	}
	results[3] = procSourceCollection(results[3], details.SocketErr, socketCount)
	state := collectionUnavailable
	if runtime.GOOS != "linux" {
		state = collectionUnsupported
	}
	if limits := process.Limits; limits != nil {
		switch limits.Cgroup.Status {
		case "available":
			state = collectionObserved
		case "permission_denied":
			state = collectionPermissionDenied
		case "unsupported":
			state = collectionUnsupported
		}
	}
	results[4] = withCollection(results[4], state, nil, "")
	return results
}

func procSourceCollection(result Result, err error, count int) Result {
	if errors.Is(err, errProcUnsupported) {
		return withCollection(result, collectionUnsupported, nil, "")
	}
	if err != nil {
		return collectionReadError(result, err)
	}
	return withCollection(result, collectionObserved, intPointer(count), "")
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
	lines = append(lines, procOriginLines(process, details.Origin)...)
	result.Evidence = []Evidence{{Label: T("proc.evidence.process"), Value: strings.Join(lines, "\n")}}
	if status == StatusWarn && process.State == "Z" {
		result.Next = fmt.Sprintf("edc proc %d", process.PPID)
	}
	return result
}

// procOriginLines는 시작 시각과 출처 줄이다. 상대 시간은 요약에 있고, 여기에는 로그인 기록과 견줄 수 있는 시각을 쓴다.
func procOriginLines(process topProcess, origin procOrigin) []string {
	var lines []string
	if !process.Started.IsZero() {
		lines = append(lines, T("proc.field.started", auditLoginTime(process.Started)))
	}
	if origin.Exe != "" {
		// 파일 이름에는 줄바꿈과 escape가 들어갈 수 있다. 그대로 쓰면 다른 근거 줄처럼 보이는 줄을 만들 수 있다.
		lines = append(lines, T("proc.field.exe", viewSanitize(origin.Exe)))
	}
	switch {
	case origin.Package != "" && !origin.PackageChanged.IsZero():
		lines = append(lines, T("proc.field.package", origin.Package, auditLoginTime(origin.PackageChanged)))
	case origin.Package != "":
		lines = append(lines, T("proc.field.package_unknown_time", origin.Package))
	case origin.Exe != "" && origin.PackageDB:
		lines = append(lines, T("proc.field.package_none"))
	}
	if session := origin.Session; session != nil {
		parts := []string{session.ID}
		if who := session.User; who != "" && session.RemoteHost != "" {
			parts = append(parts, T("proc.session.from", who, session.RemoteHost))
		} else if who != "" {
			parts = append(parts, who)
		}
		if session.Service != "" {
			parts = append(parts, session.Service)
		}
		if !session.Start.IsZero() {
			parts = append(parts, T("proc.session.started", auditLoginTime(session.Start)))
		}
		// logind는 세션을 연 process가 끝나도 남은 process가 있으면 세션을 closing으로 둔다. 로그아웃한 뒤에도 도는 process다.
		if session.State == "closing" {
			parts = append(parts, T("proc.session.closing"))
		} else if session.State != "" {
			parts = append(parts, session.State)
		}
		lines = append(lines, T("proc.field.session", strings.Join(parts, " · ")))
	}
	return lines
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
	target := set.Arg(0)
	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()
	results, pid, err := collectProcResults(ctx, target, options.verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	targetInfo := map[string]interface{}{"process": target}
	if pid > 0 {
		targetInfo["pid"] = pid
	}
	return emit(options, buildReport(version, started, targetInfo, results, options.redact))
}

func collectProcResults(ctx context.Context, target string, verbose bool) ([]Result, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// PID를 주면 표본을 뜨기 전에 그 process를 열어 둔다. 끝에 같은 process이면 표본과 세부 정보가 모두 그 process 것이다.
	handle, opened := procHandle{}, false
	if pid, err := strconv.Atoi(target); err == nil {
		if handle, opened = openProcHandle(pid); opened {
			defer handle.close()
		}
	}
	read := newTopProcessReader()
	// CPU 사용률은 두 번 읽은 차이다. 첫 읽기는 기준만 남기고, Linux는 이때 결과가 없다고 답한다.
	read()
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case <-time.After(procSampleWindow):
	}
	processes, ok := read()
	if !ok {
		return nil, 0, errors.New(T("proc.error.unreadable"))
	}
	process, matches, err := selectProc(processes, target)
	switch {
	case errors.Is(err, errProcNotFound):
		return nil, 0, errors.New(T("proc.error.not_found", target))
	case len(matches) > 0:
		return []Result{procCandidatesResult(target, matches)}, 0, nil
	}
	// 이름으로 고르면 표본을 뜬 뒤에야 PID를 안다. 표본과 이 열기 사이에 같은 tick으로 PID를 다시 쓴 process는 가르지 못한다.
	if !opened {
		if handle, opened = openProcHandle(process.PID); opened {
			defer handle.close()
		}
	}
	selected := []topProcess{process}
	newTopProcessEnricher()(selected)
	details := readProcDetails(process.PID)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// 열어 둔 process가 끝나 PID가 다른 process에 넘어갔으면 위에서 읽은 limit, 명령행, fd, 출처가 섞인다.
	if !handle.sameProcess(process) {
		return nil, 0, errors.New(T("proc.error.replaced", process.PID))
	}
	return evaluateProc(selected[0], details, time.Now(), verbose), process.PID, nil
}
