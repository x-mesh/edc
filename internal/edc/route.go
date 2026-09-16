package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// routeDefaultDest는 이 명령군이 다루는 목적지다. gateway VM의 출구 전환은 default route를 바꾸는
// 일이므로 다른 목적지를 받는 옵션은 두지 않는다.
const routeDefaultDest = "default"

const (
	routeProbeTable   = "route.table"
	routeProbeTarget  = "route.target"
	routeProbeLockout = "route.lockout"
	routeProbeGuard   = "route.guard"
	routeProbeReach   = "route.reach"
)

// 롤백 유예 범위(A06). 기본 120초, 10초에서 900초 사이로 --seconds가 조정한다.
const (
	routeDefaultRollbackSeconds = 120
	routeMinRollbackSeconds     = 10
	routeMaxRollbackSeconds     = 900
)

// routeStateDirectory는 상태 파일의 기본 위치다(A02). tmpfs라 재부팅 시 transient timer와 함께
// 사라져 수명이 맞는다.
const routeStateDirectory = "/run/edc"

func routeStatePath(execID string) string {
	return filepath.Join(routeStateDirectory, "route-"+execID+".json")
}

func runRoute(args []string, version string) int {
	usage := T("cli.usage", "edc route <check|switch|status|rollback> ...")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "check":
		return runRouteCheck(args[1:], version)
	case "switch":
		return runRouteSwitch(args[1:], version)
	case "status":
		return runRouteStatus(args[1:], version)
	case "rollback":
		return runRouteRollback(args[1:], version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func runRouteCheck(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("route check", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "route check"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	deps, _ := newRouteDeps() // ok=false(다른 OS)면 runner는 nil이고, 각 probe가 unsupported로 건너뛴다.
	cwd, _ := os.Getwd()
	configDir, _ := os.UserConfigDir()
	sshConnection := os.Getenv("SSH_CONNECTION")
	results := runRouteCheckResults(ctx, deps, routeDefaultDest, sshConnection, cwd, configDir)
	return emit(options, buildReport(version, started, nil, results, options.redact))
}

// runRouteCheckResults는 check의 네 probe를 병렬로 돌린다. runner를 주입받으므로 darwin에서도 가짜
// runner로 이 함수를 그대로 테스트한다.
func runRouteCheckResults(ctx context.Context, deps routeDeps, dest, sshConnection, cwd, configDir string) []Result {
	return runParallel(ctx, []func(context.Context) Result{
		func(ctx context.Context) Result { return probeRouteTable(ctx, deps) },
		func(ctx context.Context) Result {
			return probeRouteTarget(ctx, deps, dest, sshConnection, cwd, configDir)
		},
		func(ctx context.Context) Result { return probeRouteLockout(ctx, deps, dest, sshConnection) },
		func(ctx context.Context) Result { return probeRouteGuard(ctx, deps) },
		func(ctx context.Context) Result { return probeRouteReach(ctx, deps, dest, cwd, configDir) },
	})
}

// probeRouteReach는 현재 출구와 exits.yaml에 정의된 출구들의 L2 도달성을 본다. next-hop의 이웃
// 항목이 INCOMPLETE나 FAILED면 그 출구로 바꾸는 순간 반드시 실패하므로, 깨뜨린 뒤 되돌리는 대신
// 미리 거를 수 있다. 다만 lladdr이 정상 형태이기만 하면 그 너머가 살아있는지는 L2에서 알 수 없다.
func probeRouteReach(ctx context.Context, deps routeDeps, dest, cwd, configDir string) Result {
	started := time.Now()
	if deps.backend == nil {
		return unsupported(routeProbeReach, T("route.skip.linux_only"))
	}
	neighbors, err := deps.backend.Neighbors(ctx)
	if err != nil {
		return resultFromError(routeProbeReach, started, "netlink", fmt.Errorf("neighbors: %w", err))
	}
	links, err := deps.backend.Links(ctx)
	if err != nil {
		return resultFromError(routeProbeReach, started, "netlink", fmt.Errorf("links: %w", err))
	}
	entries, err := deps.backend.Routes(ctx)
	if err != nil {
		return resultFromError(routeProbeReach, started, "netlink", fmt.Errorf("routes: %w", err))
	}

	type candidate struct{ name, via, dev string }
	var candidates []candidate
	if current := entriesForDest(entries, dest); len(current) > 0 && current[0].Via != "" {
		candidates = append(candidates, candidate{T("route.reach.current"), current[0].Via, current[0].Dev})
	}
	if exits, _, err := loadRouteExits(cwd, configDir, ""); err == nil {
		for _, exit := range exits.Exits {
			candidates = append(candidates, candidate{exit.Name, exit.Via, exit.Dev})
		}
	}
	if len(candidates) == 0 {
		return Result{Probe: routeProbeReach, Status: StatusSkip, StartedAt: started.UTC(), Summary: T("route.reach.skip.no_candidate")}
	}

	var broken, unknown []string
	var evidence []Evidence
	for _, item := range candidates {
		state, neighborDetail, linkDetail, mtu := exitReach(neighbors, links, item.via, item.dev)
		evidence = append(evidence, Evidence{
			Label: item.name,
			Value: T("route.reach.detail", item.via, item.dev, state, emptyAs(neighborDetail, "-"), emptyAs(linkDetail, "-"), mtu),
		})
		switch state {
		case reachBroken:
			broken = append(broken, item.name)
		case reachUnknown:
			unknown = append(unknown, item.name)
		}
	}
	status := StatusPass
	if len(unknown) > 0 {
		status = StatusWarn
	}
	if len(broken) > 0 {
		status = StatusFail
	}
	result := Result{
		Probe: routeProbeReach, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary:  T("route.reach.summary", len(candidates)-len(broken)-len(unknown), len(unknown), len(broken)),
		Metrics:  map[string]interface{}{"broken": broken, "unknown": unknown},
		Evidence: evidence,
	}
	if len(broken) > 0 {
		result.Warnings = append(result.Warnings, T("route.reach.warn.broken", strings.Join(broken, ", ")))
	}
	return result
}

// probeRouteTable은 `ip route show table all`과 `ip rule show` 원문을 모두 읽어 스냅샷 범위가
// main table만이 아니라는 것(R09)을 check 시점에도 확인한다.
func probeRouteTable(ctx context.Context, deps routeDeps) Result {
	started := time.Now()
	if deps.backend == nil {
		return unsupported(routeProbeTable, T("route.skip.linux_only"))
	}
	entries, err := deps.backend.Routes(ctx)
	if err != nil {
		return resultFromError(routeProbeTable, started, "netlink", fmt.Errorf("routes: %w", err))
	}
	rules, err := deps.backend.Rules(ctx)
	if err != nil {
		return resultFromError(routeProbeTable, started, "netlink", fmt.Errorf("rules: %w", err))
	}
	// netlink에서 읽으면 해석하지 못한 줄이라는 개념이 없다. 사람이 읽을 수 있는 형태는 렌더러가
	// 다시 만든다.
	return Result{
		Probe: routeProbeTable, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary:  T("route.table.summary", len(entries), len(rules)),
		Metrics:  map[string]interface{}{"entries": len(entries), "rules": len(rules)},
		Evidence: []Evidence{{Label: "routes", Value: renderRouteEntries(entries)}, {Label: "rules", Value: renderIPRules(rules)}},
	}
}

// probeRouteTarget은 전환 뒤 검증에 쓸 후보 주소마다 ip route get을 대조해, 더 구체적인 전용 경로가
// 있어 바꾼 경로를 타지 않는 주소를 미리 가려낸다(R04).
func probeRouteTarget(ctx context.Context, deps routeDeps, dest, sshConnection, cwd, configDir string) Result {
	started := time.Now()
	if deps.backend == nil {
		return unsupported(routeProbeTarget, T("route.skip.linux_only"))
	}
	entries, err := deps.backend.Routes(ctx)
	if err != nil {
		return resultFromError(routeProbeTarget, started, "netlink", err)
	}
	targetEntries := entriesForDest(entries, dest)
	if len(targetEntries) == 0 {
		return resultFromError(routeProbeTarget, started, "route", errors.New(T("route.target.error.no_route", dest)))
	}
	targetEntry := targetEntries[0]

	var warnings []string
	if _, found := discoverRemoteFile(cwd, configDir, "exits.yaml"); !found {
		warnings = append(warnings, T("route.target.warn.exits_missing"))
	}

	var matched, excluded []string
	for _, address := range routeVerificationCandidates(sshConnection) {
		get, err := deps.backend.RouteTo(ctx, address)
		if err != nil {
			warnings = append(warnings, T("route.target.warn.get_failed", address, err))
			continue
		}
		if routeGetMatches(get, targetEntry) {
			matched = append(matched, address)
			continue
		}
		excluded = append(excluded, fmt.Sprintf("%s (dev %s via %s)", address, get.Dev, get.Via))
	}

	status := StatusPass
	switch {
	case len(matched) == 0 && len(excluded) == 0:
		status = StatusSkip
	case len(excluded) > 0:
		status = StatusWarn
		warnings = append(warnings, T("route.target.warn.excluded", strings.Join(excluded, ", ")))
	}
	return Result{
		Probe: routeProbeTarget, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary:  T("route.target.summary", len(matched), len(excluded)),
		Metrics:  map[string]interface{}{"matched": matched, "excluded": excluded},
		Warnings: warnings,
	}
}

// routeVerificationCandidates는 route.target이 대조할 후보 주소다. SSH_CONNECTION의 양끝과, 브리프가
// 예로 든 8.8.8.8(더 구체적인 전용 경로가 흔히 있는 잘 알려진 공인 주소)을 함께 본다.
func routeVerificationCandidates(sshConnection string) []string {
	var addresses []string
	if client, server, ok := parseSSHConnection(sshConnection); ok {
		addresses = append(addresses, client, server)
	}
	addresses = append(addresses, "8.8.8.8")
	return dedupeStrings(addresses)
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// probeRouteLockout은 자기 차단 위험을 매긴다(R07). SSH_CONNECTION이 없으면(로컬 콘솔) 판정할 대상이
// 없으므로 skip한다.
func probeRouteLockout(ctx context.Context, deps routeDeps, dest, sshConnection string) Result {
	started := time.Now()
	if deps.backend == nil {
		return unsupported(routeProbeLockout, T("route.skip.linux_only"))
	}
	client, _, ok := parseSSHConnection(sshConnection)
	if !ok {
		return Result{Probe: routeProbeLockout, Status: StatusSkip, StartedAt: started.UTC(), Summary: T("route.lockout.skip.no_ssh_connection")}
	}
	entries, err := deps.backend.Routes(ctx)
	if err != nil {
		return resultFromError(routeProbeLockout, started, "netlink", err)
	}
	targetEntries := entriesForDest(entries, dest)
	if len(targetEntries) == 0 {
		return resultFromError(routeProbeLockout, started, "route", errors.New(T("route.target.error.no_route", dest)))
	}
	targetEntry := targetEntries[0]

	risk, reasonKey, sessionDev, err := assessRouteLockout(ctx, deps, targetEntry, client)
	if err != nil {
		return resultFromError(routeProbeLockout, started, "command", err)
	}
	status := StatusPass
	summaryKey := "route.lockout.summary.low"
	if risk == "high" {
		status = StatusWarn
		summaryKey = "route.lockout.summary.high"
	}
	return Result{
		Probe: routeProbeLockout, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T(summaryKey, T(reasonKey)),
		Metrics: map[string]interface{}{"risk": risk, "session_dev": sessionDev},
	}
}

// assessRouteLockout은 client 주소로 ip route get을 해 lockoutRisk를 매긴다. 터널 underlay
// endpoint를 알아낼 도구가 없으므로(이 명령군은 ip, systemd-run, systemctl만 부른다) 확인하지 못한
// 것으로 보고 위험을 낮추지 않는다(R07). probeRouteLockout과 switch가 함께 쓴다.
func assessRouteLockout(ctx context.Context, deps routeDeps, targetEntry routeEntry, client string) (risk, reason, sessionDev string, err error) {
	sessionGet, err := deps.backend.RouteTo(ctx, client)
	if err != nil {
		return "", "", "", err
	}
	risk, reason = lockoutRisk(lockoutAssessment{SessionGet: sessionGet, TargetEntry: targetEntry})
	return risk, reason, sessionGet.Dev, nil
}

// routeRollbackOutcome은 rollback 실행 한 번의 결과다. 실패하면 아직 실행하지 못한 복구 명령을
// RemainingCommands에 남겨, 호출자가 stderr에 그대로 출력해 수동 복구를 도울 수 있게 한다.
type routeRollbackOutcome struct {
	Result            Result
	RemainingCommands [][]string
}

func runRouteRollback(args []string, version string) int {
	options := configuredCommon(30 * time.Second)
	set := flag.NewFlagSet("route rollback", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	statePath := set.String("state", "", T("route.flag.state"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "route rollback"))
		return 2
	}
	if *statePath == "" {
		fmt.Fprintln(os.Stderr, T("route.rollback.error.state_required"))
		return 2
	}
	// 상태 파일이 없거나 이미 완료 표시가 있으면 실행 전에 exit 2로 끝낸다(R11). 이 두 경우는 명령을
	// 하나도 부르지 않은 사용법 오류이지 rollback 자체의 실패가 아니다.
	snapshot, err := readRouteState(*statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("route.rollback.error.state_unreadable", *statePath, err))
		return 2
	}
	if snapshot.CompletedAt != nil {
		fmt.Fprintln(os.Stderr, T("route.rollback.error.already_completed", *statePath))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	deps, ok := newRouteDeps()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("route.rollback", T("route.skip.linux_only"))}, options.redact))
	}
	outcome := executeRouteRollback(ctx, deps, *statePath, snapshot)
	if outcome.Result.Status == StatusFail {
		for _, command := range outcome.RemainingCommands {
			fmt.Fprintln(os.Stderr, "ip "+strings.Join(command, " "))
		}
	}
	return emit(options, buildReport(version, started, nil, []Result{outcome.Result}, options.redact))
}

// executeRouteRollback은 원래 스펙을 되돌리는 replace와 잔존 경로를 지우는 del을 순서대로 실행한 뒤,
// 테이블과 rule을 다시 읽어 기준선과 대조한다. 성공했을 때만 타이머를 해제하고 상태 파일에 완료
// 표시를 남긴다(R11).
func executeRouteRollback(ctx context.Context, deps routeDeps, statePath string, snapshot routeSnapshot) routeRollbackOutcome {
	started := time.Now()
	currentEntries, err := deps.backend.Routes(ctx)
	if err != nil {
		return routeRollbackOutcome{Result: resultFromError("route.rollback", started, "netlink", err)}
	}
	commands, err := rollbackCommands(snapshot, currentEntries)
	if err != nil {
		return routeRollbackOutcome{Result: resultFromError("route.rollback", started, "route", err)}
	}
	for index, command := range commands {
		if _, err := deps.runner.run(ctx, "ip", command...); err != nil {
			return routeRollbackOutcome{
				Result:            resultFromError("route.rollback", started, "command", fmt.Errorf("ip %s: %w", strings.Join(command, " "), err)),
				RemainingCommands: commands[index:],
			}
		}
	}
	afterEntries, err := deps.backend.Routes(ctx)
	if err != nil {
		return routeRollbackOutcome{Result: resultFromError("route.rollback", started, "netlink", err)}
	}
	afterRules, err := deps.backend.Rules(ctx)
	if err != nil {
		return routeRollbackOutcome{Result: resultFromError("route.rollback", started, "netlink", err)}
	}
	count := routeCountFor(afterEntries, snapshot.Dest)
	if count != snapshot.CountBaseline || renderIPRules(afterRules) != snapshot.IPRuleText {
		// 원래 경로를 정확히 복원해도 기준선을 넘는 잔존 경로가 남으면 여전히 깨진 상태다(R11).
		// 성공을 보고하지 않고, 처음부터 다시 실행할 수 있도록 전체 복구 명령을 남긴다.
		return routeRollbackOutcome{
			Result:            resultFromError("route.rollback", started, "verify", errors.New(T("route.rollback.error.verify_failed", snapshot.CountBaseline, count))),
			RemainingCommands: commands,
		}
	}
	warning := disarmGuard(ctx, deps.runner, snapshot.UnitName)
	completedAt := time.Now().UTC()
	snapshot.CompletedAt = &completedAt
	if err := writeRouteState(statePath, snapshot); err != nil {
		return routeRollbackOutcome{Result: resultFromError("route.rollback", started, "state", err)}
	}
	result := Result{
		Probe: "route.rollback", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("route.rollback.summary", snapshot.Dest, count),
	}
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	return routeRollbackOutcome{Result: result}
}

// probeRouteGuard는 check 동안 실제로 무장하지 않는다. systemd-run을 부르면 실제 transient timer가
// 생기는 부작용이 있어(R06), 읽기 전용이어야 하는 check에서는 쓸 수 없다. 등급은 switch 실행 시점에만
// 확인된다.
func probeRouteGuard(ctx context.Context, deps routeDeps) Result {
	started := time.Now()
	if deps.backend == nil {
		return unsupported(routeProbeGuard, T("route.skip.linux_only"))
	}
	return Result{
		Probe: routeProbeGuard, Status: StatusSkip, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("route.guard.skip.check_only"),
	}
}

// routeSwitchInput은 switch 오케스트레이션의 입력이다.
type routeSwitchInput struct {
	dest          string
	exit          routeExit
	seconds       int
	force         bool
	dryRun        bool
	edcPath       string
	execID        string
	statePath     string
	sshConnection string
}

// routeConfirmFunc는 확인 화면 하나를 추상화한다. terminal에서는 tui_confirm을, 아니면 capture.go의
// confirm을 쓴다. 테스트는 이 함수를 가짜로 바꿔 신호 경합을 재현한다.
type routeConfirmFunc func(detail, question string, initial bool) (bool, error)

// routeSwitchDeps는 switch가 쓰는 부수효과 있는 의존성이다. 구조체로 모아 테스트가 모두 가짜로
// 바꿔치기할 수 있게 한다.
type routeSwitchDeps struct {
	routeDeps
	confirm       routeConfirmFunc
	fetchPublicIP func(context.Context) (publicNetworkInfo, error)
	signals       <-chan os.Signal
}

// routeSwitchOutcome은 switch 실행 한 번의 결과다. Cancelled는 사용자가 확정을 거부했거나 확정 전에
// 신호를 받은 경우다(exit 4). 그 밖의 실패는 Result.Status가 fail이고 exit 1로 보고된다.
type routeSwitchOutcome struct {
	Result    Result
	Cancelled bool
	Message   string
}

func runRouteSwitch(args []string, version string) int {
	options := configuredCommon(60 * time.Second)
	set := flag.NewFlagSet("route switch", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	to := set.String("to", "", T("route.flag.to"))
	seconds := set.Int("seconds", routeDefaultRollbackSeconds, T("route.flag.seconds"))
	exitsPath := set.String("exits", "", T("route.flag.exits"))
	force := set.Bool("force", false, T("route.flag.force"))
	yes := set.Bool("yes", false, T("route.flag.yes"))
	dryRun := set.Bool("dry-run", false, T("route.flag.dry_run"))
	set.BoolVar(dryRun, "n", false, T("route.flag.dry_run"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "route switch"))
		return 2
	}
	if *to == "" {
		fmt.Fprintln(os.Stderr, T("route.switch.error.to_required"))
		return 2
	}
	if *seconds < routeMinRollbackSeconds || *seconds > routeMaxRollbackSeconds {
		fmt.Fprintln(os.Stderr, T("route.switch.error.seconds_range", routeMinRollbackSeconds, routeMaxRollbackSeconds))
		return 2
	}
	// exits.yaml 탐색과 이름 조회는 파일만 읽으므로 권한 확인보다 먼저 해도 안전하다(ip나 systemd
	// 명령을 부르지 않는다). --to 오타처럼 흔한 사용법 실수를 root 없이도 바로 알려 준다.
	cwd, _ := os.Getwd()
	configDir, _ := os.UserConfigDir()
	exits, _, err := loadRouteExits(cwd, configDir, *exitsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	exit, found := findRouteExit(exits, *to)
	if !found {
		fmt.Fprintln(os.Stderr, T("route.switch.error.unknown_exit", *to, strings.Join(routeExitNames(exits), ", ")))
		return 2
	}
	// 내부에서 sudo를 붙이지 않는다(A03). root가 아니면 어떤 명령도 부르지 않고 끝낸다.
	// --dry-run은 읽기만 하므로 이 확인에서 제외한다. 위험한 명령일수록 권한 없이도 계획을 볼 수 있어야 한다.
	if os.Geteuid() != 0 && !*dryRun {
		fmt.Fprintln(os.Stderr, T("route.switch.error.root_required"))
		return 3
	}

	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	deps, ok := newRouteDeps()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("route.switch", T("route.skip.linux_only"))}, options.redact))
	}

	// 타이머 유닛에는 PATH가 보장되지 않으므로 절대 경로를 쓴다(A08). 못 구해도 armGuard가 실패로
	// 처리해 fail closed로 이어진다.
	edcPath, _ := os.Executable()
	execID := runID(started)

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	input := routeSwitchInput{
		dest: exits.Dest, exit: exit, seconds: *seconds, force: *force, dryRun: *dryRun,
		edcPath: edcPath, execID: execID, statePath: routeStatePath(execID),
		sshConnection: os.Getenv("SSH_CONNECTION"),
	}
	switchDeps := routeSwitchDeps{
		routeDeps: deps, signals: signals,
		confirm:       routeSwitchTerminalConfirm,
		fetchPublicIP: fetchPublicNetworkInfo,
	}
	if *yes {
		switchDeps.confirm = routeSwitchAutoConfirm
	}
	outcome := executeRouteSwitch(ctx, switchDeps, input)
	if outcome.Cancelled {
		fmt.Fprintln(os.Stderr, outcome.Message)
		return 4
	}
	return emit(options, buildReport(version, started, nil, []Result{outcome.Result}, options.redact))
}

// executeRouteSwitch는 스냅샷 → check 판정(자기 차단 위험) → guardDecision → 무장 → is-active 확인 →
// 경로 변경 → 개수 불변식 → 출구 신원 확인 → 확정 → 타이머 해제 순으로 실행한다(T07 실행 순서).
// guardDecision이 거부하거나 경로 변경 전에 실패하면 아직 아무것도 바꾸지 않았으므로 원복 없이
// 끝낸다. 경로를 바꾼 뒤의 실패는 rollback 경로를 그대로 불러 원복한다.
func executeRouteSwitch(ctx context.Context, deps routeSwitchDeps, input routeSwitchInput) routeSwitchOutcome {
	started := time.Now()
	const probe = "route.switch"

	entries, err := deps.backend.Routes(ctx)
	if err != nil {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "netlink", err)}
	}
	rules, err := deps.backend.Rules(ctx)
	if err != nil {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "netlink", err)}
	}
	tableText, ruleText := renderRouteEntries(entries), renderIPRules(rules)
	targetEntries := entriesForDest(entries, input.dest)
	if len(targetEntries) == 0 {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "route", errors.New(T("route.target.error.no_route", input.dest)))}
	}
	targetEntry := targetEntries[0]
	baseline := len(targetEntries)

	// 프리플라이트: next-hop이 L2에서 확정 실패면 바꿔도 반드시 끊긴다. 아직 아무것도 만들지 않았으니
	// 깨뜨린 뒤 되돌리는 대신 여기서 끝낸다. 항목이 없는 경우는 실패가 아니라 미확인이므로 막지 않는다.
	reachState, neighborDetail, linkDetail, mtu := routeExitReachability(ctx, deps.routeDeps, input.exit.Via, input.exit.Dev)
	if reachState == reachBroken && !input.force {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "reach",
			errors.New(T("route.switch.error.exit_unreachable", input.exit.Name, emptyAs(neighborDetail, "-"), emptyAs(linkDetail, "-"))))}
	}

	if input.dryRun {
		replaceArgs, err := routeReplaceArgs(targetEntry, input.exit.Via)
		if err != nil {
			return routeSwitchOutcome{Result: resultFromError(probe, started, "plan", err)}
		}
		unit := routeRollbackUnitName(input.execID)
		return routeSwitchOutcome{Result: Result{
			Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
			Summary: T("route.switch.dry_run.summary", input.exit.Name, input.seconds),
			Evidence: []Evidence{
				{Label: T("route.switch.dry_run.label.current"), Value: strings.TrimSpace(targetEntry.Raw)},
				{Label: T("route.switch.dry_run.label.apply"), Value: "ip " + strings.Join(replaceArgs, " ")},
				{Label: T("route.switch.dry_run.label.rollback"), Value: input.edcPath + " route rollback --state " + input.statePath},
				{Label: T("route.switch.dry_run.label.guard"), Value: T("route.switch.dry_run.guard", unit, input.seconds)},
				{Label: T("route.switch.dry_run.label.reach"), Value: T("route.reach.detail", input.exit.Via, input.exit.Dev, reachState, emptyAs(neighborDetail, "-"), emptyAs(linkDetail, "-"), mtu)},
			},
		}}
	}

	snapshot := routeSnapshot{
		SchemaVersion: routeStateSchemaVersion, RunID: input.execID, CreatedAt: time.Now().UTC(),
		Dest: input.dest, RouteTableText: tableText, IPRuleText: ruleText, TargetEntry: targetEntry,
		NewVia: input.exit.Via, CountBaseline: baseline, UnitName: routeRollbackUnitName(input.execID),
		SSHConnection: input.sshConnection, ExitName: input.exit.Name, Seconds: input.seconds,
	}

	// check 판정: 자기 차단 위험. SSH_CONNECTION을 못 읽으면 확인하지 못한 것이므로 위험을 낮추지
	// 않고 높음으로 본다(R07의 안전 기본값을 그대로 적용한다).
	risk := "high"
	if client, _, ok := parseSSHConnection(input.sshConnection); ok {
		if measured, _, _, err := assessRouteLockout(ctx, deps.routeDeps, targetEntry, client); err == nil {
			risk = measured
		}
	}

	// 상태 파일은 경로를 바꾸기 전에 먼저 남긴다. 무장한 타이머가 읽는 파일이 실제 변경보다 앞서
	// 있어야, 우리 프로세스가 바로 죽어도 타이머의 rollback이 근거를 갖는다.
	if err := writeRouteState(input.statePath, snapshot); err != nil {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "state", err)}
	}

	guardProbe := armGuard(ctx, deps.runner, snapshot.UnitName, input.seconds, input.statePath, input.edcPath)
	tier := detectGuardTier(guardProbe)
	allow, reason := guardDecision(tier, risk, input.force)
	if !allow {
		return routeSwitchOutcome{Result: abortRouteSwitch(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, T("route.switch.error.guard_denied", reason))}
	}

	replaceArgs, err := routeReplaceArgs(targetEntry, input.exit.Via)
	if err != nil {
		return routeSwitchOutcome{Result: abortRouteSwitch(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, err.Error())}
	}
	if _, err := deps.runner.run(ctx, "ip", replaceArgs...); err != nil {
		return routeSwitchOutcome{Result: abortRouteSwitch(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, err.Error())}
	}

	// 경로를 바꾼 뒤의 실패는 여기부터 rollback 경로를 그대로 부른다(R03, R16).
	afterEntries, err := deps.backend.Routes(ctx)
	if err != nil {
		return routeSwitchOutcome{Result: routeSwitchRollbackAndFail(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, err.Error())}
	}
	if count := routeCountFor(afterEntries, input.dest); count != baseline {
		reason := T("route.rollback.error.verify_failed", baseline, count)
		return routeSwitchOutcome{Result: routeSwitchRollbackAndFail(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, reason)}
	}

	identity := "skipped"
	if input.exit.ExpectPublicIP != "" {
		info, err := deps.fetchPublicIP(ctx)
		if err != nil {
			reason := T("route.switch.error.identity_check_failed", err)
			return routeSwitchOutcome{Result: routeSwitchRollbackAndFail(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, reason)}
		}
		if info.IP != input.exit.ExpectPublicIP {
			reason := T("route.switch.error.identity_mismatch", input.exit.ExpectPublicIP, info.IP)
			return routeSwitchOutcome{Result: routeSwitchRollbackAndFail(ctx, deps.routeDeps, input.statePath, snapshot, probe, started, reason)}
		}
		identity = "matched"
	}

	// 확정: identity가 matched면 기본 예로 빠르게 확정할 수 있고, skipped면 기본 아니오라 사람이
	// 직접 확정해야 한다(R16).
	detail := routeSwitchConfirmDetail(input, identity, tier)
	confirmed, confirmErr := routeSwitchAwaitConfirm(deps, detail, T("route.switch.confirm.question"), identity == "matched")
	if !confirmed || confirmErr != nil {
		message := T("route.switch.cancelled.declined")
		if confirmErr != nil {
			message = confirmErr.Error() // 신호 케이스는 이미 번역된 문자열을 담고 있다.
		}
		rollbackResult := executeRouteRollback(ctx, deps.routeDeps, input.statePath, snapshot)
		return routeSwitchOutcome{Result: rollbackResult.Result, Cancelled: true, Message: message}
	}

	warning := disarmGuard(ctx, deps.runner, snapshot.UnitName)
	completedAt := time.Now().UTC()
	snapshot.CompletedAt = &completedAt
	if err := writeRouteState(input.statePath, snapshot); err != nil {
		return routeSwitchOutcome{Result: resultFromError(probe, started, "state", err)}
	}
	result := Result{
		Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("route.switch.summary", input.exit.Name, identity),
	}
	if warning != "" {
		result.Warnings = append(result.Warnings, warning)
	}
	return routeSwitchOutcome{Result: result}
}

// abortRouteSwitch는 경로를 하나도 바꾸지 않은 단계에서 멈출 때 쓴다. 방어적으로 타이머를 해제하고
// 상태 파일을 완료로 표시해, 아직 아무 일도 일어나지 않은 채로 남지 않게 한다.
func abortRouteSwitch(ctx context.Context, deps routeDeps, statePath string, snapshot routeSnapshot, probe string, started time.Time, reason string) Result {
	disarmGuard(ctx, deps.runner, snapshot.UnitName)
	completedAt := time.Now().UTC()
	snapshot.CompletedAt = &completedAt
	_ = writeRouteState(statePath, snapshot)
	return resultFromError(probe, started, "guard", errors.New(reason))
}

// routeSwitchRollbackAndFail은 경로를 이미 바꾼 뒤의 실패에서 rollback 경로를 그대로 부른다.
func routeSwitchRollbackAndFail(ctx context.Context, deps routeDeps, statePath string, snapshot routeSnapshot, probe string, started time.Time, reason string) Result {
	rollbackOutcome := executeRouteRollback(ctx, deps, statePath, snapshot)
	result := resultFromError(probe, started, "verify", errors.New(reason))
	result.Evidence = rollbackOutcome.Result.Evidence
	result.Warnings = append(result.Warnings, rollbackOutcome.Result.Summary)
	return result
}

// routeSwitchAwaitConfirm은 확인 함수를 goroutine에서 돌리고, 확정 전에 신호가 오면 그 결과를
// 기다리지 않고 즉시 거부로 처리한다(R08). 확정 뒤에 받은 신호는 이 함수가 이미 끝난 뒤라 영향이
// 없다.
func routeSwitchAwaitConfirm(deps routeSwitchDeps, detail, question string, initial bool) (bool, error) {
	type outcome struct {
		confirmed bool
		err       error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		confirmed, err := deps.confirm(detail, question, initial)
		resultCh <- outcome{confirmed, err}
	}()
	select {
	case <-deps.signals:
		return false, errors.New(T("route.switch.cancelled.signal"))
	case result := <-resultCh:
		return result.confirmed, result.err
	}
}

// routeSwitchConfirmDetail은 확인 화면에 보여 줄 설명이다. break-before-make로 기존 흐름이 끊긴다는
// 사실과 남은 유예를 알린다.
func routeSwitchConfirmDetail(input routeSwitchInput, identity string, tier int) string {
	return T("route.switch.confirm.detail", input.exit.Name, identity, input.seconds, tier)
}

// routeSwitchTerminalConfirm은 capture.go의 confirmCapture와 같은 형태다. terminal이면 기존
// tui_confirm 화면을, 아니면 capture.go의 confirm을 쓰고 기본값은 아니요다.
func routeSwitchTerminalConfirm(detail, question string, initial bool) (bool, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		confirmed, err := runConfirmModel(os.Stdin, os.Stdout, newDetailedConfirmModel(detail, question, initial))
		if err != nil {
			return false, err
		}
		return confirmed, nil
	}
	fmt.Fprint(os.Stdout, detail)
	return confirm(os.Stdin, os.Stdout, question, false), nil
}

// routeSwitchAutoConfirm은 --yes가 확정을 대신한다. expect_public_ip가 없어 identity가 skipped인
// 출구는 initial이 항상 false라 --yes로도 자동 확정되지 않는다(R16).
func routeSwitchAutoConfirm(_ string, _ string, initial bool) (bool, error) {
	return initial, nil
}

func runRouteStatus(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("route status", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "route status"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	deps, ok := newRouteDeps()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("route.status", T("route.skip.linux_only"))}, options.redact))
	}
	results := routeStatusResults(ctx, deps, routeStateDirectory)
	return emit(options, buildReport(version, started, nil, results, options.redact))
}

// routeStatusResults는 상태 디렉터리의 상태 파일마다 하나씩 Result를 만든다. 하나도 없으면 fail이
// 아니라 skip 사유를 남긴다(R12).
func routeStatusResults(ctx context.Context, deps routeDeps, stateDir string) []Result {
	paths, err := listRouteStateFiles(stateDir)
	if err != nil {
		return []Result{resultFromError("route.status", time.Now(), "state", err)}
	}
	if len(paths) == 0 {
		return []Result{{Probe: "route.status", Status: StatusSkip, StartedAt: time.Now().UTC(), Summary: T("route.status.skip.none")}}
	}
	results := make([]Result, 0, len(paths))
	for _, path := range paths {
		results = append(results, routeStatusForFile(ctx, deps, path))
	}
	return results
}

// listRouteStateFiles는 route-*.json 파일을 이름순으로 나열한다. 디렉터리가 아직 없으면(전환을
// 한 번도 하지 않은 호스트) 빈 목록을 돌려주고 오류로 보지 않는다.
func listRouteStateFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "route-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths, nil
}

// routeStatusForFile은 상태 파일 하나를 읽어 진행 중이거나 최근에 끝난 전환의 상태를 보고한다.
// 아직 완료되지 않았는데 가리키는 타이머가 active가 아니면 상태 파일과 타이머가 어긋난 것이라
// warn으로 표시한다.
func routeStatusForFile(ctx context.Context, deps routeDeps, path string) Result {
	started := time.Now()
	probe := "route.status." + strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "route-"), ".json")
	snapshot, err := readRouteState(path)
	if err != nil {
		return resultFromError(probe, started, "state", err)
	}
	completed := snapshot.CompletedAt != nil
	status := StatusPass
	var warnings []string
	if !completed {
		output, runErr := deps.runner.run(ctx, "systemctl", "is-active", snapshot.UnitName+".timer")
		if runErr != nil || !parseIsActive(output) {
			status = StatusWarn
			warnings = append(warnings, T("route.status.warn.timer_mismatch", snapshot.UnitName))
		}
	}
	remaining := "n/a"
	if !completed && snapshot.Seconds > 0 {
		deadline := snapshot.CreatedAt.Add(time.Duration(snapshot.Seconds) * time.Second)
		remaining = time.Until(deadline).Round(time.Second).String()
	}
	summaryKey := "route.status.summary.pending"
	if completed {
		summaryKey = "route.status.summary.completed"
	}
	return Result{
		Probe: probe, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T(summaryKey, snapshot.ExitName, snapshot.RunID),
		Metrics: map[string]interface{}{
			"run_id": snapshot.RunID, "exit": snapshot.ExitName, "started_at": snapshot.CreatedAt,
			"unit": snapshot.UnitName, "completed": completed, "remaining": remaining,
		},
		Warnings: warnings,
	}
}
