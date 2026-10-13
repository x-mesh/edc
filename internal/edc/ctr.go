package edc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ctr은 docker container 하나의 상태를 한 화면에 모은다. svc와 같은 틀이고, systemd unit 대신 docker inspect와 container의
// cgroup을 읽는다. 읽기만 하고 container를 시작하거나 멈추지 않는다.

const (
	ctrProbeID = "ctr"
	// ctrLogLines는 docker logs에서 읽는 마지막 줄 수이고, ctrLogShown은 그 가운데 보여 주는 오류 줄 수다.
	ctrLogLines = 50
	ctrLogShown = 5
	// ctrMemoryWarnPercent 이상이면 memory 한도에 가깝다. ctrPidsWarnPercent는 pids 한도, ctrThrottleWarnPercent는
	// CPU throttle이 걸린 period의 비율이다.
	ctrMemoryWarnPercent   = 90
	ctrPidsWarnPercent     = 90
	ctrThrottleWarnPercent = 25
	ctrShortIDLength       = 12
)

// ctrInspectFormat은 docker inspect에 넘기는 template이다. Health는 healthcheck가 없으면 nil이라 with로 감싼다.
// NetworkSettings와 HostConfig도 with로 감싼다. 여러 container를 한 번에 읽을 때 template 오류 하나가 나머지 출력까지 막는다.
const ctrInspectFormat = "{{.State.Pid}}|{{.Name}}|{{.State.Status}}|{{.State.StartedAt}}|{{.State.FinishedAt}}|{{.RestartCount}}|{{.State.OOMKilled}}|{{.State.ExitCode}}|{{.Config.Image}}|{{.Id}}|{{with index .State \"Health\"}}{{index . \"Status\"}}{{end}}|{{with .NetworkSettings}}{{json .Ports}}{{end}}|{{with .HostConfig}}{{with .RestartPolicy}}{{.Name}}:{{.MaximumRetryCount}}{{end}}{{end}}"

// ctrHealthFormat은 healthcheck 기록이다. 검사 출력이 길 수 있어 목록에는 넣지 않고 container 하나를 볼 때만 읽는다.
const ctrHealthFormat = "{{json .State.Health}}"

var errCtrInvalidReference = errors.New("invalid container reference")

// ctrLogErrorLine은 로그에서 오류로 보는 줄이다. 로그 형식을 모르므로 흔한 낱말로만 고른다.
var ctrLogErrorLine = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|traceback|oom|out of memory)\b`)

type ctrInspect struct {
	PID          int
	Name         string
	Status       string
	Health       string
	Image        string
	ID           string
	StartedAt    time.Time
	FinishedAt   time.Time
	RestartCount int
	OOMKilled    bool
	ExitCode     int
	Ports        []ctrPublishedPort
	// RestartPolicy는 docker의 --restart 이름이고, RestartMax는 on-failure의 재시도 한도다. 0이면 한도가 없다.
	RestartPolicy string
	RestartMax    int
}

// ctrHealth는 docker가 남긴 healthcheck 기록이다. docker는 최근 검사 5개만 남긴다.
type ctrHealth struct {
	Status        string
	FailingStreak int
	Log           []ctrHealthCheck
}

type ctrHealthCheck struct {
	Start    time.Time
	ExitCode int
	Output   string
}

// ctrPublishedPort는 host에 publish한 container port다. docker는 IPv4와 IPv6에 따로 bind하므로 Hosts가 여럿일 수 있다.
type ctrPublishedPort struct {
	Proto string
	Port  int
	Hosts []string
}

// ctrLimits는 container cgroup의 한도와 사용량이다. CPU는 누적 counter라 container가 산 동안의 비율이다.
type ctrLimits struct {
	topLimitStatus
	Path   string
	Memory topCgroupMemory
	Events topCgroupEvents
	CPU    ctrCPULimits
	Pids   ctrPidsLimits
}

type ctrCPULimits struct {
	topLimitStatus
	Periods, Throttled, ThrottledUS uint64
}

type ctrPidsLimits struct {
	topLimitStatus
	Current   uint64
	Max       uint64
	Unlimited bool
}

type ctrLog struct {
	Lines   []string
	Scanned int
}

// ctrSources는 ctr이 읽는 원천이다. 테스트는 함수를 가짜로 넣는다.
type ctrSources struct {
	euid    int
	docker  func() bool
	inspect func(ctx context.Context, reference string) (string, error)
	// cgroupPath는 /proc/<pid>/cgroup의 cgroup v2 경로다.
	cgroupPath func(pid int) (string, error)
	limits     func(path string) ctrLimits
	processes  func(ctx context.Context, controlGroup string, mainPID int) ([]topProcess, error)
	fdLimit    func(pid int) topFDLimit
	// listen은 pid가 속한 network namespace의 대기 소켓이다.
	listen func(pid int) ([]listenSocket, error)
	logs   func(ctx context.Context, id string, lines int) (string, error)
	health func(ctx context.Context, id string) (string, error)
	now    func() time.Time
	// listIDs는 모든 container의 ID이고, inspectMany는 여러 container를 한 번에 읽는다. container마다 docker를 부르면 수백 번이 된다.
	listIDs     func(ctx context.Context) (string, error)
	inspectMany func(ctx context.Context, ids []string) (string, error)
}

type ctrFacts struct {
	svcFacts
	Reference     string
	Docker        bool
	Inspect       ctrInspect
	InspectOutput string
	InspectErr    error
	Health        ctrHealth
	HealthErr     error
	Limits        ctrLimits
	LimitsErr     error
	Log           ctrLog
	LogErr        error
}

func runCtr(args []string, version string) int {
	sources, ok := hostCtrSources()
	return runCtrWith(args, version, sources, ok)
}

func runCtrWith(args []string, version string, sources ctrSources, ok bool) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("ctr", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	all := set.Bool("all", false, T("command.ctr.option.all"))
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
	usage := T("cli.usage", "edc ctr [options] [--all] | edc ctr [options] <container>")
	if set.NArg() == 0 {
		return runCtrList(options, version, sources, ok, *all)
	}
	// --all은 목록에서 정상 종료한 container까지 보이는 option이다. container 하나를 볼 때 받으면 무엇이 바뀌는지 오해한다.
	if set.NArg() != 1 || *all {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	reference, err := normalizeCtrReference(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("ctr.error.invalid_reference", set.Arg(0)))
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	started := time.Now()
	target := map[string]interface{}{"container": reference}
	if !ok {
		return emit(options, buildReport(version, started, target, []Result{unsupported(ctrProbeID, T("ctr.skip.linux_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	facts := collectCtrFacts(ctx, reference, sources)
	if facts.Docker && facts.InspectErr != nil && ctrNotFound(facts.InspectOutput) {
		fmt.Fprintln(os.Stderr, T("ctr.error.not_found", reference))
		return 1
	}
	return emit(options, buildReport(version, started, target, evaluateCtr(facts), options.redact))
}

// normalizeCtrReference는 docker에 넘겨도 되는 이름이나 ID만 받는다. '-'로 시작하면 option이 된다.
func normalizeCtrReference(input string) (string, error) {
	if input == "" || strings.HasPrefix(input, "-") || len(input) > 128 {
		return "", errCtrInvalidReference
	}
	for _, char := range input {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.-/", char)) {
			return "", errCtrInvalidReference
		}
	}
	return input, nil
}

func ctrNotFound(output string) bool {
	return strings.Contains(output, "No such object") || strings.Contains(output, "No such container")
}

// parseCtrInspect는 ctrInspectFormat 출력을 읽는다. 시각은 RFC 3339이고, 멈춘 적 없는 container의 FinishedAt은 0001년이다.
func parseCtrInspect(output string) (ctrInspect, error) {
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 13 {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect output %q", output)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect pid %q", fields[0])
	}
	restarts, err := strconv.Atoi(fields[5])
	if err != nil {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect restart count %q", fields[5])
	}
	exitCode, err := strconv.Atoi(fields[7])
	if err != nil {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect exit code %q", fields[7])
	}
	ports, err := parseCtrPorts(fields[11])
	if err != nil {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect ports %q", fields[11])
	}
	policy, maximum, err := parseCtrRestartPolicy(fields[12])
	if err != nil {
		return ctrInspect{}, fmt.Errorf("unexpected docker inspect restart policy %q", fields[12])
	}
	inspect := ctrInspect{
		PID: pid, Name: strings.TrimPrefix(fields[1], "/"), Status: fields[2], RestartCount: restarts, OOMKilled: fields[6] == "true",
		ExitCode: exitCode, Image: fields[8], ID: fields[9], Health: fields[10], Ports: ports, RestartPolicy: policy, RestartMax: maximum,
	}
	inspect.StartedAt, _ = time.Parse(time.RFC3339Nano, fields[3])
	inspect.FinishedAt, _ = time.Parse(time.RFC3339Nano, fields[4])
	return inspect, nil
}

// parseCtrRestartPolicy는 "on-failure:5"처럼 이름과 재시도 한도를 읽는다. HostConfig를 읽지 못한 container는 칸이 빈다.
func parseCtrRestartPolicy(text string) (string, int, error) {
	if text == "" {
		return "", 0, nil
	}
	name, count, found := strings.Cut(text, ":")
	if !found {
		return "", 0, errors.New("missing retry count")
	}
	maximum, err := strconv.Atoi(count)
	if err != nil {
		return "", 0, err
	}
	return name, maximum, nil
}

// parseCtrPorts는 NetworkSettings.Ports의 JSON을 읽는다. EXPOSE만 하고 publish하지 않은 port는 binding이 null이라 뺀다.
func parseCtrPorts(text string) ([]ctrPublishedPort, error) {
	if text == "" {
		return nil, nil
	}
	var bindings map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(text), &bindings); err != nil {
		return nil, err
	}
	var ports []ctrPublishedPort
	for key, hosts := range bindings {
		if len(hosts) == 0 {
			continue
		}
		number, proto, _ := strings.Cut(key, "/")
		port, err := strconv.Atoi(number)
		if err != nil {
			return nil, err
		}
		published := ctrPublishedPort{Proto: proto, Port: port}
		for _, host := range hosts {
			address := host.HostIP
			if address == "" {
				address = "*"
			}
			published.Hosts = append(published.Hosts, net.JoinHostPort(address, host.HostPort))
		}
		ports = append(ports, published)
	}
	slices.SortFunc(ports, func(a, b ctrPublishedPort) int {
		return cmp.Or(cmp.Compare(a.Port, b.Port), cmp.Compare(a.Proto, b.Proto))
	})
	return ports, nil
}

// filterCtrLog는 docker logs 출력에서 오류 줄만 마지막 ctrLogShown줄 남긴다.
func filterCtrLog(output string) ctrLog {
	var log ctrLog
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		log.Scanned++
		if ctrLogErrorLine.MatchString(line) {
			log.Lines = append(log.Lines, svcTruncate(line, svcJournalLineRunes))
		}
	}
	if len(log.Lines) > ctrLogShown {
		log.Lines = log.Lines[len(log.Lines)-ctrLogShown:]
	}
	return log
}

func collectCtrFacts(ctx context.Context, reference string, sources ctrSources) ctrFacts {
	facts := ctrFacts{svcFacts: svcFacts{Unit: reference, Root: sources.euid == 0, Systemd: true, Probe: ctrProbeID}, Reference: reference, Docker: sources.docker()}
	if !facts.Docker {
		return facts
	}
	facts.InspectOutput, facts.InspectErr = sources.inspect(ctx, reference)
	if facts.InspectErr != nil {
		return facts
	}
	inspect, err := parseCtrInspect(facts.InspectOutput)
	if err != nil {
		facts.InspectErr = err
		return facts
	}
	facts.Inspect = inspect
	if inspect.Status == "running" && !inspect.StartedAt.IsZero() {
		if uptime := sources.now().Sub(inspect.StartedAt); uptime >= 0 {
			facts.Uptime, facts.UptimeKnown = uptime, true
		}
	}
	if inspect.Health != "" {
		facts.Health, facts.HealthErr = readCtrHealth(ctx, sources, inspect.ID)
	}
	facts.Show = svcShow{ID: inspect.Name, LoadState: "loaded", ActiveState: inspect.Status, MainPID: inspect.PID}
	if inspect.PID > 0 {
		path, err := sources.cgroupPath(inspect.PID)
		if err != nil {
			facts.ProcessErr, facts.LimitsErr = err, err
		} else {
			facts.Show.ControlGroup = path
			facts.Processes, facts.ProcessErr = sources.processes(ctx, path, inspect.PID)
			facts.Limits = sources.limits(path)
		}
		facts.FD = map[int]topFDLimit{}
		for _, process := range svcRows(facts.Processes, inspect.PID) {
			facts.FD[process.PID] = sources.fdLimit(process.PID)
		}
		if len(facts.Processes) > 0 && !svcListenHidden(facts.svcFacts) {
			facts.Listen, facts.ListenErr = sources.listen(inspect.PID)
		}
	}
	output, err := sources.logs(ctx, inspect.ID, ctrLogLines)
	switch {
	case ctx.Err() != nil:
		facts.LogErr = fmt.Errorf("docker logs: %w", ctx.Err())
	case errors.Is(err, exec.ErrNotFound):
		facts.LogErr = err
	case err != nil:
		facts.LogErr = fmt.Errorf("docker logs: %w: %s", err, infoSingleLine(firstLine(output)))
	default:
		facts.Log = filterCtrLog(output)
	}
	return facts
}

func readCtrHealth(ctx context.Context, sources ctrSources, id string) (ctrHealth, error) {
	output, err := sources.health(ctx, id)
	if err != nil {
		return ctrHealth{}, fmt.Errorf("docker inspect: %w: %s", err, infoSingleLine(firstLine(output)))
	}
	var health ctrHealth
	if err := json.Unmarshal([]byte(output), &health); err != nil {
		return ctrHealth{}, fmt.Errorf("unexpected docker health output: %w", err)
	}
	return health, nil
}

func evaluateCtr(facts ctrFacts) []Result {
	if !facts.Docker {
		return []Result{unsupported(ctrProbeID, T("ctr.skip.no_docker"))}
	}
	if facts.InspectErr != nil {
		detail := strings.TrimSpace(facts.InspectOutput)
		if detail == "" {
			detail = facts.InspectErr.Error()
		}
		// docker socket을 열지 못한 것은 container의 문제가 아니다. root나 docker 그룹이 필요하다고 알린다.
		if strings.Contains(detail, "permission denied") {
			return []Result{svcResultFor(ctrProbeID, "state", StatusSkip, T("ctr.skip.needs_docker_access"))}
		}
		result := svcResultFor(ctrProbeID, "state", StatusFail, T("ctr.state.inspect_failed", infoSingleLine(firstLine(detail))))
		result.Error = &DiagnosticError{Kind: "command", Message: detail}
		return []Result{result}
	}
	return []Result{ctrStateResult(facts), svcProcessesResult(facts.svcFacts), ctrLimitsResult(facts), svcFDResult(facts.svcFacts), ctrListenResult(facts), ctrLogResult(facts)}
}

// ctrListenResult는 svc의 listen 결과에 publish한 port를 맞춰 본다. docker는 host port로 온 연결을 container IP로 넘기므로,
// 안에서 그 port를 listen하는 process가 없거나 loopback에서만 listen하면 publish한 port로는 닿지 않는다. 막 시작한 container는
// 아직 listen하기 전일 수 있어 fail이 아니라 warn이다.
func ctrListenResult(facts ctrFacts) Result {
	result := svcListenResult(facts.svcFacts)
	if result.Status == StatusSkip || len(facts.Inspect.Ports) == 0 {
		return result
	}
	members := map[string]bool{}
	for _, process := range facts.Processes {
		members[strconv.Itoa(process.PID)] = true
	}
	var lines []string
	var published []map[string]interface{}
	for _, port := range facts.Inspect.Ports {
		reachable, loopback := false, false
		for _, socket := range facts.Listen {
			if socket.Proto != port.Proto || socket.Port != port.Port || !members[socket.PID] {
				continue
			}
			if address, err := netip.ParseAddr(socket.Address); err == nil && address.Unmap().IsLoopback() {
				loopback = true
			} else {
				reachable = true
			}
		}
		label, hosts := fmt.Sprintf("%d/%s", port.Port, port.Proto), strings.Join(port.Hosts, ", ")
		state := "listening"
		switch {
		case reachable:
		case loopback:
			state = "loopback_only"
			result.Warnings = append(result.Warnings, T("ctr.listen.published_loopback", label, hosts))
		default:
			state = "not_listening"
			result.Warnings = append(result.Warnings, T("ctr.listen.published_none", label, hosts))
		}
		lines = append(lines, label+" ← "+hosts)
		published = append(published, map[string]interface{}{"port": port.Port, "proto": port.Proto, "hosts": port.Hosts, "state": state})
	}
	if len(result.Warnings) > 0 {
		result.Status = StatusWarn
	}
	if result.Metrics == nil {
		result.Metrics = map[string]interface{}{}
	}
	result.Metrics["published"] = published
	result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.published"), Value: strings.Join(lines, "\n")})
	return result
}

// ctrStateText는 상세와 목록이 같이 쓰는 상태 문구다. 한 번도 돈 적 없는 container는 종료 코드가 없다.
func ctrStateText(inspect ctrInspect) string {
	state := inspect.Status
	if inspect.Health != "" {
		state += " (" + inspect.Health + ")"
	}
	if inspect.Status != "running" && !inspect.FinishedAt.IsZero() {
		state += fmt.Sprintf(", exit code %d", inspect.ExitCode)
	}
	if inspect.OOMKilled {
		state += ", OOM killed"
	}
	return state
}

func ctrStateResult(facts ctrFacts) Result {
	inspect := facts.Inspect
	state := ctrStateText(inspect)
	uptime := "-"
	if facts.UptimeKnown {
		uptime = svcFormatUptime(facts.Uptime)
	}
	status := StatusWarn
	switch inspect.Status {
	case "running":
		status = StatusPass
		if inspect.RestartCount > 0 || inspect.OOMKilled || inspect.Health == "unhealthy" {
			status = StatusWarn
		}
	case "exited", "dead":
		status = StatusFail
	}
	policy := ctrRestartPolicyText(inspect)
	result := svcResultFor(ctrProbeID, "state", status, T("ctr.state.summary", state, uptime, inspect.RestartCount, policy, inspect.Image))
	if status == StatusFail {
		result = svcFailed(result)
	}
	id := inspect.ID
	if len(id) > ctrShortIDLength {
		id = id[:ctrShortIDLength]
	}
	result.Metrics = map[string]interface{}{"status": inspect.Status, "pid": inspect.PID, "restart_count": inspect.RestartCount, "oom_killed": inspect.OOMKilled, "exit_code": inspect.ExitCode, "image": inspect.Image, "id": id, "restart_policy": inspect.RestartPolicy}
	if inspect.RestartPolicy == "on-failure" {
		result.Metrics["restart_max"] = inspect.RestartMax
	}
	// on-failure의 한도만큼 재시작한 뒤 멈춘 container는 docker가 다시 띄우지 않는다. 누가 시작하기 전까지 멈춰 있다.
	if inspect.Status == "exited" && inspect.RestartPolicy == "on-failure" && inspect.RestartMax > 0 && inspect.RestartCount >= inspect.RestartMax {
		result.Warnings = append(result.Warnings, T("ctr.state.restart_gave_up", inspect.RestartCount, policy))
	}
	if inspect.Health != "" {
		result.Metrics["health"] = inspect.Health
		addCtrHealth(&result, facts)
	}
	if facts.UptimeKnown {
		result.Metrics["uptime_s"] = int64(facts.Uptime.Seconds())
	}
	return result
}

// ctrRestartPolicyText는 docker --restart의 표기를 그대로 쓴다. HostConfig를 읽지 못해 정책을 모르면 -이다.
func ctrRestartPolicyText(inspect ctrInspect) string {
	switch {
	case inspect.RestartPolicy == "":
		return "-"
	case inspect.RestartPolicy == "no":
		return T("ctr.state.no_policy")
	case inspect.RestartPolicy == "on-failure" && inspect.RestartMax > 0:
		return fmt.Sprintf("on-failure:%d", inspect.RestartMax)
	}
	return inspect.RestartPolicy
}

// addCtrHealth는 healthcheck 기록을 상세에 붙인다. unhealthy이면 가장 최근에 실패한 검사의 출력을 warning으로 올린다.
// 기록을 읽지 못해도 상태는 docker inspect가 준 값이 맞으므로, 읽지 못한 까닭만 상세에 적는다.
func addCtrHealth(result *Result, facts ctrFacts) {
	if facts.HealthErr != nil {
		result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.health"), Value: facts.HealthErr.Error()})
		return
	}
	health := facts.Health
	result.Metrics["health_failing_streak"] = health.FailingStreak
	if len(health.Log) == 0 {
		return
	}
	var lines []string
	var failed *ctrHealthCheck
	for index, check := range health.Log {
		lines = append(lines, fmt.Sprintf("%s  exit %d  %s", check.Start.Local().Format(time.DateTime), check.ExitCode, ctrHealthOutput(check.Output)))
		if check.ExitCode != 0 {
			failed = &health.Log[index]
		}
	}
	last := health.Log[len(health.Log)-1]
	result.Metrics["health_last_exit_code"], result.Metrics["health_last_output"] = last.ExitCode, ctrHealthOutput(last.Output)
	result.Evidence = append(result.Evidence, Evidence{Label: T("ctr.evidence.health"), Value: strings.Join(lines, "\n")})
	if facts.Inspect.Health == "unhealthy" && failed != nil {
		result.Warnings = append(result.Warnings, T("ctr.state.health_failed", health.FailingStreak, failed.ExitCode, ctrHealthOutput(failed.Output)))
	}
}

// ctrHealthOutput은 검사 출력의 첫 줄이다. 출력이 없는 검사는 -이다.
func ctrHealthOutput(output string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	if line = strings.TrimSpace(line); line == "" {
		return "-"
	}
	return svcTruncate(line, svcJournalLineRunes)
}

func ctrLimitsResult(facts ctrFacts) Result {
	switch {
	case facts.Inspect.PID <= 0:
		return svcResultFor(ctrProbeID, "limits", StatusSkip, T("ctr.skip.not_running"))
	case facts.LimitsErr != nil:
		return svcSkipFor(ctrProbeID, "limits", "cgroup", facts.LimitsErr)
	case facts.Limits.Status != "available":
		return svcResultFor(ctrProbeID, "limits", StatusSkip, T("svc.skip.unreadable", "cgroup", infoSingleLine(facts.Limits.Reason)))
	}
	limits := facts.Limits
	var warnings []string
	metrics := map[string]interface{}{"cgroup": limits.Path}
	memory := "-"
	if limits.Memory.Status == "available" && limits.Memory.Used != nil {
		metrics["memory_used_bytes"] = *limits.Memory.Used
		switch {
		case limits.Memory.Unlimited || limits.Memory.Max == nil || *limits.Memory.Max == 0:
			memory = traceBytes(*limits.Memory.Used) + " / " + T("ctr.limits.no_limit")
		default:
			percent := float64(*limits.Memory.Used) / float64(*limits.Memory.Max) * 100
			memory = fmt.Sprintf("%s / %s (%.0f%%)", traceBytes(*limits.Memory.Used), traceBytes(*limits.Memory.Max), percent)
			metrics["memory_max_bytes"], metrics["memory_percent"] = *limits.Memory.Max, percent
			if percent >= ctrMemoryWarnPercent {
				warnings = append(warnings, T("ctr.limits.memory_high", memory))
			}
		}
	}
	oom := "-"
	if limits.Events.Status == "available" && limits.Events.OOMKill != nil {
		oom = strconv.FormatUint(*limits.Events.OOMKill, 10)
		metrics["oom_kills"] = *limits.Events.OOMKill
		if *limits.Events.OOMKill > 0 {
			warnings = append(warnings, T("ctr.limits.oom", *limits.Events.OOMKill))
		}
	}
	cpu := T("ctr.limits.no_limit")
	if limits.CPU.Status == "available" {
		metrics["cpu_periods"], metrics["cpu_throttled_periods"], metrics["cpu_throttled_usec"] = limits.CPU.Periods, limits.CPU.Throttled, limits.CPU.ThrottledUS
		if limits.CPU.Periods > 0 {
			percent := float64(limits.CPU.Throttled) / float64(limits.CPU.Periods) * 100
			cpu = fmt.Sprintf("%d/%d periods (%.0f%%)", limits.CPU.Throttled, limits.CPU.Periods, percent)
			metrics["cpu_throttled_percent"] = percent
			if percent >= ctrThrottleWarnPercent {
				warnings = append(warnings, T("ctr.limits.throttled", cpu))
			}
		}
	}
	pids := "-"
	if limits.Pids.Status == "available" {
		metrics["pids_current"] = limits.Pids.Current
		if limits.Pids.Unlimited || limits.Pids.Max == 0 {
			pids = strconv.FormatUint(limits.Pids.Current, 10) + " / " + T("ctr.limits.no_limit")
		} else {
			percent := float64(limits.Pids.Current) / float64(limits.Pids.Max) * 100
			pids = fmt.Sprintf("%d / %d (%.0f%%)", limits.Pids.Current, limits.Pids.Max, percent)
			metrics["pids_max"] = limits.Pids.Max
			if percent >= ctrPidsWarnPercent {
				warnings = append(warnings, T("ctr.limits.pids_high", pids))
			}
		}
	}
	status := StatusPass
	if len(warnings) > 0 {
		status = StatusWarn
	}
	result := svcResultFor(ctrProbeID, "limits", status, T("ctr.limits.summary", memory, oom, cpu, pids))
	result.Metrics = metrics
	result.Warnings = warnings
	return result
}

func ctrLogResult(facts ctrFacts) Result {
	if facts.LogErr != nil {
		return svcSkipFor(ctrProbeID, "log", "docker logs", facts.LogErr)
	}
	lines := facts.Log.Lines
	if len(lines) == 0 {
		return svcResultFor(ctrProbeID, "log", StatusPass, T("ctr.log.none", facts.Log.Scanned))
	}
	result := svcResultFor(ctrProbeID, "log", StatusWarn, T("ctr.log.errors", len(lines), facts.Log.Scanned, lines[len(lines)-1]))
	result.Evidence = []Evidence{{Label: T("ctr.evidence.log"), Value: strings.Join(lines, "\n")}}
	return result
}
