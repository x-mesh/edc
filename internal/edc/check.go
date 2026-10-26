package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

// check는 지금 고장 난 곳을 한 번에 훑는 읽기 전용 점검이다. 원천은 info, svc, ctr, events의 수집기를 그대로 쓰고
// 판정만 여기서 한다. NIC drop처럼 부팅 뒤 누적된 카운터는 한 번 읽어서는 지금의 문제인지 알 수 없어 판정하지 않는다.

// checkKernelWindow는 커널 장애 신호를 찾는 범위다. 지금의 장애를 보려는 것이라 events의 기본 창보다 좁다.
const checkKernelWindow = time.Hour

// checkNamesShown은 요약에 이름을 적는 항목 수다. 나머지는 개수로 적고 근거에서 본다.
const checkNamesShown = 5

// checkEvidenceLines는 근거에 적는 줄 수다.
const checkEvidenceLines = 10

// checkUsageLimits는 conntrack, TCP memory, file handle처럼 한도에 닿으면 새 연결이나 파일 열기가 실패하는 값의 기준이다.
var checkUsageLimits = topThreshold{warn: 80, danger: 90}

// checkUnlimitedFileHandles 이상인 fs.file-max는 한도가 없는 것과 같다. systemd 240부터 기본값을 LONG_MAX로 둔다.
const checkUnlimitedFileHandles = uint64(1) << 62

// checkMemoryLimits는 MemAvailable이 전체에서 차지하는 비율의 하한이다. 이보다 낮으면 경고와 실패다.
var checkMemoryLimits = topThreshold{warn: 10, danger: 5}

// checkRecentStop은 restart policy가 없는 container가 오류로 멈춘 것을 경고하는 기간이다. 그보다 오래된 것은 일부러 둔
// container로 본다. 개발 host에는 오래전에 멈춘 시험용 container가 많다.
const checkRecentStop = 24 * time.Hour

// checkImageFileSystems는 원래 읽기 전용인 파일 시스템이다. read-only라도 문제가 아니다.
var checkImageFileSystems = map[string]bool{"squashfs": true, "iso9660": true, "erofs": true, "cramfs": true, "udf": true}

// checkPolicy는 설정의 [check] 표를 판정에 쓰는 모양으로 바꾼 것이다. 설정에 없는 기준은 기본값을 쓴다.
type checkPolicy struct {
	accept, acceptUnits, acceptContainers, acceptMounts, requireUnits []string
	disk, memory, usage                                               topThreshold
	// checks가 비어 있으면 모든 점검을 낸다. commands는 루틴의 사용자 명령이다.
	checks   []string
	commands []checkCommandConfig
}

// newCheckPolicy는 전역 [check]에 루틴을 얹는다. 목록은 더하고, limits는 루틴에 적은 값만 바꾼다. routine이 비면 전역 값만 쓴다.
func newCheckPolicy(config checkConfig, routine string) checkPolicy {
	selected := config.Routines[routine]
	limits := func(values, global []float64, fallback topThreshold) topThreshold {
		if len(values) != 2 {
			values = global
		}
		if len(values) != 2 {
			return fallback
		}
		return topThreshold{warn: values[0], danger: values[1]}
	}
	join := func(global, own []string) []string { return append(slices.Clone(global), own...) }
	return checkPolicy{
		accept: join(config.Accept, selected.Accept), acceptUnits: join(config.AcceptUnits, selected.AcceptUnits),
		acceptContainers: join(config.AcceptContainers, selected.AcceptContainers), acceptMounts: join(config.AcceptMounts, selected.AcceptMounts),
		requireUnits: join(config.RequireUnits, selected.RequireUnits),
		disk:         limits(selected.Limits.Disk, config.Limits.Disk, diskLimits),
		memory:       limits(selected.Limits.MemoryAvailable, config.Limits.MemoryAvailable, checkMemoryLimits),
		usage:        limits(selected.Limits.Usage, config.Limits.Usage, checkUsageLimits),
		checks:       selected.Checks,
		commands:     selected.Commands,
	}
}

// checkMatches는 name이 glob 하나에라도 맞는지다. 설정을 읽을 때 glob을 검사하므로 여기서 오류는 나지 않는다.
func checkMatches(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

type checkProcess struct {
	PID, PPID      int
	Command, State string
}

type checkDisk struct {
	Mount                   string
	FSType                  string
	Percent                 float64
	InodesUsed, InodesTotal uint64
	ReadOnly                bool
}

type checkNetwork struct {
	// RouteErr가 있으면 route table을 읽지 못한 것이다. route가 없다고 판정하지 않는다.
	Route    string
	RouteErr error
	DNS      infoDNS
	// Conntrack과 TCPMemory는 한도 대비 사용률이다. OK가 false면 읽지 못했거나 이 커널에 없는 값이다.
	Conntrack, TCPMemory     float64
	ConntrackOK, TCPMemoryOK bool
}

// checkBackup은 마지막 Time Machine 백업이다. Configured가 false면 백업 대상이 없다.
type checkBackup struct {
	Configured bool
	Latest     time.Time
	Err        error
}

// checkBackupAge보다 오래된 백업은 경고한다. Time Machine은 한 시간마다 백업하므로 일주일은 여러 번 실패한 것이다.
const checkBackupAge = 7 * 24 * time.Hour

type checkReboot struct {
	// Err는 재부팅 표시 파일을 확인하지 못한 이유다. 없는 파일은 오류가 아니다.
	Err      error
	Flag     bool
	Packages []string
	Running  string
	Kernels  []string
}

// checkSources는 점검이 읽는 원천이다. 테스트는 함수들을 가짜로 넣는다.
type checkSources struct {
	services    func(ctx context.Context) ([]svcListItem, error)
	processes   func() ([]checkProcess, error)
	memory      func() (infoMemory, error)
	disks       func() ([]checkDisk, error)
	kernel      func(ctx context.Context, since time.Duration) eventsFacts
	clock       func() *infoClock
	network     func() checkNetwork
	containers  func(ctx context.Context) ([]ctrListItem, error)
	fileHandles func() (*infoFileHandles, error)
	reboot      func() checkReboot
	// thermal은 macOS의 thermal pressure 단계(0~4)이고 backup은 마지막 Time Machine 백업이다. Linux에는 없다.
	thermal func() (int, error)
	backup  func() checkBackup
	// memoryPressure는 macOS의 memory pressure 단계다. nil이면 MemAvailable의 비율로 판정한다.
	memoryPressure func() (topMemoryPressure, error)
	// serviceNext는 unit 하나를 더 볼 명령의 형식이다. 비어 있으면 edc svc를 쓴다. kernelNext는 커널 신호를 더 볼 명령이다.
	serviceNext, kernelNext string
	// configSafe는 사용자 명령을 실행해도 되는 설정 파일인지 본다. runCommand는 명령 하나를 실행한다.
	configSafe func() error
	runCommand func(ctx context.Context, command checkCommandConfig) checkCommandOutcome
}

type checkFacts struct {
	Services       []svcListItem
	ServicesErr    error
	Processes      []checkProcess
	ProcessesErr   error
	Memory         infoMemory
	MemoryErr      error
	Disks          []checkDisk
	DisksErr       error
	Kernel         eventsFacts
	Clock          *infoClock
	Network        checkNetwork
	Containers     []ctrListItem
	ContainersErr  error
	FileHandles    *infoFileHandles
	FileHandlesErr error
	Reboot         checkReboot
	// Commands는 policy.commands와 같은 순서다. CommandsErr가 있으면 명령을 실행하지 않은 이유다.
	Commands    []checkCommandOutcome
	CommandsErr error
	// TimedOut은 --timeout 안에 끝나지 않은 원천의 점검 이름이다. 그 점검은 skip이다.
	TimedOut []string
	// Unsupported는 이 OS에 원천이 없는 점검 이름이다. 그 점검은 skip이다.
	Unsupported             []string
	MemoryPressure          topMemoryPressure
	ServiceNext, KernelNext string
	// Now는 판정 시각이다. 비어 있으면 지금이다.
	Now        time.Time
	Thermal    int
	ThermalErr error
	Backup     checkBackup
}

func runCheck(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("check")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	group := set.String("group", "", T("command.check.option.group"))
	inventory := set.String("inventory", "", T("command.check.option.inventory"))
	routine := set.String("routine", "", T("command.check.option.routine"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 1 || (set.NArg() == 1 && *routine != "") {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc check [options] [routine]"))
		return 2
	}
	if set.NArg() == 1 {
		*routine = set.Arg(0)
	}
	// 루틴 이름은 --group에서 원격 셸 명령에 그대로 들어간다.
	if *routine != "" && !checkNamePattern.MatchString(*routine) {
		fmt.Fprintln(os.Stderr, T("check.error.routine_name", *routine))
		return 2
	}
	if *group == "" && *inventory != "" {
		fmt.Fprintln(os.Stderr, T("check.group.error.inventory_without_group"))
		return 2
	}
	// 원격 호스트를 점검하는 것이라 이 컴퓨터가 Linux가 아니어도 된다.
	if *group != "" {
		return runCheckGroup(options, *group, *inventory, *routine, version, cleanSSHRunner, os.Stdout)
	}
	// 원격 호스트의 루틴은 그 호스트의 설정에 있으므로, 이 컴퓨터의 설정에서는 로컬 실행일 때만 찾는다.
	if _, found := activeConfig.Check.Routines[*routine]; *routine != "" && !found {
		names := slices.Sorted(maps.Keys(activeConfig.Check.Routines))
		fmt.Fprintln(os.Stderr, T("check.error.unknown_routine", *routine, strings.Join(names, ", ")))
		return 2
	}
	started := time.Now()
	sources, ok := hostCheckSources()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("check", T("check.skip.linux_only"))}, options.redact))
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	policy := newCheckPolicy(activeConfig.Check, *routine)
	var target map[string]interface{}
	if *routine != "" {
		target = map[string]interface{}{"routine": *routine}
	}
	return emit(options, buildReport(version, started, target, evaluateCheck(collectCheckFacts(ctx, sources, policy.commands), policy), options.redact))
}

// collectCheckFacts는 원천을 함께 읽는다. systemctl, journalctl, docker socket이 각각 외부 호출이라 차례로 읽으면 느리다.
// ctx가 끝나면 남은 원천을 기다리지 않고 TimedOut에 적는다. df처럼 ctx를 받지 않는 원천이 멈춘 NFS mount에 걸려도
// --timeout 안에 끝나야 하기 때문이다. 늦게 끝난 원천은 버퍼가 있는 channel에 결과를 놓고 끝나므로 facts를 건드리지 않는다.
func collectCheckFacts(ctx context.Context, sources checkSources, commands []checkCommandConfig) checkFacts {
	var facts checkFacts
	type collected struct {
		id    string
		apply func(*checkFacts)
	}
	jobs := map[string]func() func(*checkFacts){
		"service": func() func(*checkFacts) {
			items, err := sources.services(ctx)
			return func(facts *checkFacts) { facts.Services, facts.ServicesErr = items, err }
		},
		"process": func() func(*checkFacts) {
			processes, err := sources.processes()
			return func(facts *checkFacts) { facts.Processes, facts.ProcessesErr = processes, err }
		},
		"memory": func() func(*checkFacts) {
			memory, err := sources.memory()
			var pressure topMemoryPressure
			if sources.memoryPressure != nil {
				pressure, _ = sources.memoryPressure()
			}
			return func(facts *checkFacts) { facts.Memory, facts.MemoryErr, facts.MemoryPressure = memory, err, pressure }
		},
		"disk": func() func(*checkFacts) {
			disks, err := sources.disks()
			return func(facts *checkFacts) { facts.Disks, facts.DisksErr = disks, err }
		},
		"kernel": func() func(*checkFacts) {
			kernel := sources.kernel(ctx, checkKernelWindow)
			return func(facts *checkFacts) { facts.Kernel = kernel }
		},
		"time": func() func(*checkFacts) {
			clock := sources.clock()
			return func(facts *checkFacts) { facts.Clock = clock }
		},
		"network": func() func(*checkFacts) {
			network := sources.network()
			return func(facts *checkFacts) { facts.Network = network }
		},
		"container": func() func(*checkFacts) {
			items, err := sources.containers(ctx)
			return func(facts *checkFacts) { facts.Containers, facts.ContainersErr = items, err }
		},
		"filehandles": func() func(*checkFacts) {
			handles, err := sources.fileHandles()
			return func(facts *checkFacts) { facts.FileHandles, facts.FileHandlesErr = handles, err }
		},
		"reboot": func() func(*checkFacts) {
			reboot := sources.reboot()
			return func(facts *checkFacts) { facts.Reboot = reboot }
		},
		"thermal": func() func(*checkFacts) {
			level, err := sources.thermal()
			return func(facts *checkFacts) { facts.Thermal, facts.ThermalErr = level, err }
		},
		"backup": func() func(*checkFacts) {
			backup := sources.backup()
			return func(facts *checkFacts) { facts.Backup = backup }
		},
	}
	facts.ServiceNext, facts.KernelNext = sources.serviceNext, sources.kernelNext
	for id, available := range map[string]bool{
		"service": sources.services != nil, "process": sources.processes != nil, "memory": sources.memory != nil,
		"disk": sources.disks != nil, "kernel": sources.kernel != nil, "time": sources.clock != nil, "network": sources.network != nil,
		"container": sources.containers != nil, "filehandles": sources.fileHandles != nil, "reboot": sources.reboot != nil,
		"thermal": sources.thermal != nil, "backup": sources.backup != nil,
	} {
		if !available {
			delete(jobs, id)
			facts.Unsupported = append(facts.Unsupported, id)
		}
	}
	// 사용자 명령도 같은 방식으로 기다린다. 설정 파일이 안전하지 않으면 실행하지 않는다.
	if len(commands) > 0 {
		facts.CommandsErr = sources.configSafe()
	}
	if len(commands) > 0 && facts.CommandsErr == nil {
		facts.Commands = make([]checkCommandOutcome, len(commands))
		for index, command := range commands {
			jobs["cmd."+command.Name] = func() func(*checkFacts) {
				outcome := sources.runCommand(ctx, command)
				return func(facts *checkFacts) { facts.Commands[index] = outcome }
			}
		}
	}
	done := make(chan collected, len(jobs))
	pending := map[string]bool{}
	for id, job := range jobs {
		pending[id] = true
		go func() { done <- collected{id: id, apply: job()} }()
	}
	for len(pending) > 0 {
		select {
		case result := <-done:
			result.apply(&facts)
			delete(pending, result.id)
		case <-ctx.Done():
			// 끝난 원천과 deadline이 함께 준비되면 select는 아무 쪽이나 고른다. 이미 온 결과는 버리지 않는다.
			for drained := false; !drained; {
				select {
				case result := <-done:
					result.apply(&facts)
					delete(pending, result.id)
				default:
					drained = true
				}
			}
			facts.TimedOut = slices.Sorted(maps.Keys(pending))
			return facts
		}
	}
	return facts
}

func evaluateCheck(facts checkFacts, policy checkPolicy) []Result {
	results := []Result{
		checkServices(facts, policy),
		checkProcesses(facts),
		checkMemory(facts, policy),
		checkDisks(facts, policy),
		checkKernel(facts),
		checkTime(facts),
		checkNetworkResult(facts, policy),
		checkContainers(facts, policy),
		checkFileHandles(facts, policy),
		checkRebootResult(facts),
		checkThermal(facts),
		checkBackupResult(facts),
	}
	results = slices.DeleteFunc(results, func(result Result) bool {
		id := strings.TrimPrefix(result.Probe, "check.")
		return slices.Contains(checkPlatformIDs, id) && slices.Contains(facts.Unsupported, id)
	})
	for index, result := range results {
		id := strings.TrimPrefix(result.Probe, "check.")
		switch {
		case slices.Contains(facts.Unsupported, id):
			results[index] = checkResult(id, StatusSkip, T("check.skip.unsupported_os"))
		case slices.Contains(facts.TimedOut, id):
			results[index] = checkResult(id, StatusSkip, T("check.skip.timed_out"))
		}
	}
	if len(policy.checks) > 0 {
		results = slices.DeleteFunc(results, func(result Result) bool {
			return !slices.Contains(policy.checks, strings.TrimPrefix(result.Probe, "check."))
		})
	}
	for index, command := range policy.commands {
		id := "cmd." + command.Name
		switch {
		case facts.CommandsErr != nil:
			results = append(results, checkResult(id, StatusSkip, T("check.cmd.not_run", infoSingleLine(facts.CommandsErr.Error()))))
		case slices.Contains(facts.TimedOut, id):
			results = append(results, checkResult(id, StatusSkip, T("check.skip.timed_out")))
		case index < len(facts.Commands):
			results = append(results, checkCommandResult(facts.Commands[index]))
		}
	}
	return applyCheckAccept(results, policy.accept)
}

// applyCheckAccept는 받아들인 점검의 warn·fail을 skip으로 바꾼다. audit의 accept와 같은 표시와 metrics를 쓴다.
func applyCheckAccept(results []Result, accept []string) []Result {
	for index, result := range results {
		if (result.Status == StatusWarn || result.Status == StatusFail) && slices.Contains(accept, strings.TrimPrefix(result.Probe, "check.")) {
			results[index] = acceptAuditResult(result)
		}
	}
	return results
}

func checkResult(id string, status Status, summary string) Result {
	return Result{Probe: "check." + id, Status: status, StartedAt: time.Now().UTC(), Summary: summary}
}

func checkSkip(id, source string, err error) Result {
	if errors.Is(err, fs.ErrPermission) {
		return checkResult(id, StatusSkip, T("check.skip.needs_root", source))
	}
	return checkResult(id, StatusSkip, T("check.skip.unreadable", source, infoSingleLine(err.Error())))
}

// checkIssues는 한 점검 안에서 찾은 문제다. 가장 나쁜 판정이 점검의 판정이 되고, 문제마다 한 줄을 근거로 남긴다.
type checkIssues struct {
	status Status
	lines  []string
	// accepted는 설정이 받아들여 판정하지 않은 이름이다. 판정에서 빠져도 개수와 이름은 남긴다.
	accepted []string
}

func (issues *checkIssues) add(status Status, line string) {
	if checkSeverity(status) > checkSeverity(issues.status) {
		issues.status = status
	}
	issues.lines = append(issues.lines, line)
}

func checkSeverity(status Status) int {
	switch status {
	case StatusFail:
		return 2
	case StatusWarn:
		return 1
	}
	return 0
}

// result는 문제가 없으면 pass 요약을, 있으면 문제 줄을 이은 요약을 낸다. fail이면 실패 상자에 같은 줄을 적는다.
func (issues checkIssues) result(id, pass, next string) Result {
	result := issues.judged(id, pass, next)
	if len(issues.accepted) > 0 {
		result.Summary += T("check.accepted_items", len(issues.accepted))
		result.Metrics = map[string]interface{}{"accepted": issues.accepted}
	}
	return result
}

func (issues checkIssues) judged(id, pass, next string) Result {
	if len(issues.lines) == 0 {
		return checkResult(id, StatusPass, pass)
	}
	result := checkResult(id, issues.status, strings.Join(issues.lines, "; "))
	// 문제가 하나면 요약과 같은 줄이라 근거로 다시 적지 않는다.
	if len(issues.lines) > 1 {
		result.Evidence = []Evidence{{Label: T("check.evidence.issues"), Value: strings.Join(issues.lines, "\n")}}
	}
	result.Next = next
	if issues.status == StatusFail {
		result.Error = &DiagnosticError{Kind: "check", Message: result.Summary}
	}
	return result
}

// checkNames는 이름을 몇 개만 잇고 나머지를 개수로 적는다.
func checkNames(names []string) string {
	text := strings.Join(names[:min(len(names), checkNamesShown)], ", ")
	if hidden := len(names) - checkNamesShown; hidden > 0 {
		text += fmt.Sprintf(" +%d", hidden)
	}
	return text
}

// checkUsageStatus는 사용률이 기준을 넘은 정도다.
func checkUsageStatus(percent float64, limits topThreshold) Status {
	switch {
	case percent >= limits.danger:
		return StatusFail
	case percent >= limits.warn:
		return StatusWarn
	}
	return StatusPass
}

// serviceCommand는 unit 하나를 더 볼 명령이다. macOS에는 edc svc가 없어 launchctl을 쓴다.
func (facts checkFacts) serviceCommand(unit string) string {
	if facts.ServiceNext == "" {
		return "edc svc " + unit
	}
	return fmt.Sprintf(facts.ServiceNext, unit)
}

func checkServices(facts checkFacts, policy checkPolicy) Result {
	const id = "service"
	if errors.Is(facts.ServicesErr, errSvcNoSystemd) {
		return checkResult(id, StatusSkip, T("svc.skip.no_systemd"))
	}
	if facts.ServicesErr != nil {
		return checkSkip(id, "systemctl", facts.ServicesErr)
	}
	var issues checkIssues
	var failed, changing []string
	listed := map[string]bool{}
	for _, item := range facts.Services {
		listed[item.Unit] = true
		if (item.failed() || item.changing()) && checkMatches(policy.acceptUnits, item.Unit) {
			issues.accepted = append(issues.accepted, item.Unit)
			continue
		}
		switch {
		case item.failed():
			failed = append(failed, item.Unit)
		case item.changing():
			changing = append(changing, item.Unit+" ("+item.state()+")")
		}
	}
	// 목록은 inactive unit을 빼므로, 목록에 없는 필수 unit은 실행 중이 아니다.
	var missing []string
	for _, unit := range policy.requireUnits {
		if !listed[unit] {
			missing = append(missing, unit)
		}
	}
	next := ""
	if len(failed) > 0 {
		issues.add(StatusFail, T("check.service.failed", len(failed), checkNames(failed)))
		next = facts.serviceCommand(failed[0])
	}
	if len(missing) > 0 {
		issues.add(StatusFail, T("check.service.required", len(missing), checkNames(missing)))
		if next == "" {
			next = facts.serviceCommand(missing[0])
		}
	}
	if len(changing) > 0 {
		issues.add(StatusWarn, T("check.service.changing", len(changing), checkNames(changing)))
		if next == "" {
			next = facts.serviceCommand(strings.Fields(changing[0])[0])
		}
	}
	return issues.result(id, T("check.service.ok", len(facts.Services)), next)
}

// checkPartialError는 원천을 일부만 읽은 경우다. 읽은 만큼은 판정하고, 읽지 못한 수를 경고로 남긴다.
type checkPartialError struct{ Unreadable int }

func (err checkPartialError) Error() string {
	return fmt.Sprintf("%d entries could not be read", err.Unreadable)
}

func checkProcesses(facts checkFacts) Result {
	const id = "process"
	var partial checkPartialError
	if facts.ProcessesErr != nil && !errors.As(facts.ProcessesErr, &partial) {
		return checkSkip(id, "/proc", facts.ProcessesErr)
	}
	result := checkProcessStates(facts)
	if partial.Unreadable > 0 {
		result.Warnings = append(result.Warnings, T("check.process.unreadable", partial.Unreadable))
	}
	return result
}

func checkProcessStates(facts checkFacts) Result {
	const id = "process"
	commands := map[int]string{}
	for _, process := range facts.Processes {
		commands[process.PID] = process.Command
	}
	zombies := map[int]int{}
	var blocked []string
	for _, process := range facts.Processes {
		switch process.State {
		case "Z":
			zombies[process.PPID]++
		case "D":
			blocked = append(blocked, fmt.Sprintf("%s(%d)", process.Command, process.PID))
		}
	}
	var issues checkIssues
	if len(zombies) > 0 {
		// 같은 부모가 거둬 가지 않은 zombie가 대부분이다. 부모를 알아야 무엇을 다시 시작할지 정할 수 있다.
		parents := make([]int, 0, len(zombies))
		total := 0
		for parent, count := range zombies {
			parents = append(parents, parent)
			total += count
		}
		sort.Slice(parents, func(i, j int) bool {
			if zombies[parents[i]] != zombies[parents[j]] {
				return zombies[parents[i]] > zombies[parents[j]]
			}
			return parents[i] < parents[j]
		})
		names := make([]string, 0, len(parents))
		for _, parent := range parents {
			names = append(names, fmt.Sprintf("%s(%d) ×%d", commands[parent], parent, zombies[parent]))
		}
		issues.add(StatusWarn, T("check.process.zombie", total, checkNames(names)))
	}
	if len(blocked) > 0 {
		issues.add(StatusWarn, T("check.process.blocked", len(blocked), checkNames(blocked)))
	}
	return issues.result(id, T("check.process.ok", len(facts.Processes)), "edc top")
}

func checkMemory(facts checkFacts, policy checkPolicy) Result {
	const id = "memory"
	if facts.MemoryErr != nil {
		return checkSkip(id, "/proc/meminfo", facts.MemoryErr)
	}
	memory := facts.Memory
	percent := float64(memory.Available) / float64(memory.Total) * 100
	text := T("check.memory.available", formatSizeShort(memory.Available), formatSizeShort(memory.Total), percent)
	var issues checkIssues
	// macOS는 page cache와 압축 memory 때문에 가용 비율이 늘 낮게 보인다. 커널이 알려 주는 pressure 단계로만 판정한다.
	if facts.MemoryPressure.known() {
		text = T("check.memory.pressure", facts.MemoryPressure, text)
		switch facts.MemoryPressure.level() {
		case topLevelDanger:
			issues.add(StatusFail, text)
		case topLevelWarn:
			issues.add(StatusWarn, text)
		}
		return issues.result(id, text, "edc top")
	}
	switch {
	case percent < policy.memory.danger:
		issues.add(StatusFail, text)
	case percent < policy.memory.warn:
		issues.add(StatusWarn, text)
	}
	return issues.result(id, text, "edc top")
}

func checkDisks(facts checkFacts, policy checkPolicy) Result {
	const id = "disk"
	if facts.DisksErr != nil {
		return checkSkip(id, "df", facts.DisksErr)
	}
	var issues checkIssues
	next, worst := "", -1.0
	highest := checkDisk{}
	for _, disk := range facts.Disks {
		// image 파일 시스템은 만들 때 크기를 내용에 맞추므로 df가 늘 100%를 준다.
		if checkImageFileSystems[disk.FSType] {
			continue
		}
		if checkMatches(policy.acceptMounts, disk.Mount) {
			issues.accepted = append(issues.accepted, disk.Mount)
			continue
		}
		if disk.Percent > highest.Percent {
			highest = disk
		}
		if status := checkUsageStatus(disk.Percent, policy.disk); status != StatusPass {
			issues.add(status, T("check.disk.usage", disk.Mount, disk.Percent))
			if disk.Percent > worst {
				next, worst = "edc du "+disk.Mount, disk.Percent
			}
		}
		if disk.InodesTotal > 0 {
			percent := float64(disk.InodesUsed) / float64(disk.InodesTotal) * 100
			if status := checkUsageStatus(percent, policy.disk); status != StatusPass {
				issues.add(status, T("check.disk.inodes", disk.Mount, percent))
				if next == "" {
					next = "edc du " + disk.Mount
				}
			}
		}
		// 오류로 read-only가 된 파일 시스템은 check.kernel이 커널 기록으로 fail을 낸다. 여기서는 일부러 ro로 붙인 mount와
		// 구별할 수 없어 경고로 둔다.
		if disk.ReadOnly {
			issues.add(StatusWarn, T("check.disk.readonly", disk.Mount))
		}
	}
	if next == "" && len(issues.lines) > 0 {
		next = "edc info -v"
	}
	pass := T("check.disk.ok", len(facts.Disks), highest.Mount, highest.Percent)
	if len(facts.Disks) == 0 {
		pass = T("check.disk.none")
	}
	return issues.result(id, pass, next)
}

// checkKernelStatus는 커널 신호 종류마다의 판정이다. events와 같은 기준이다. 목록에 없는 종류(link, units, clock)는 다른
// 점검이 지금 상태로 보므로 여기서 세지 않는다.
var checkKernelStatus = map[string]Status{
	eventsOOM: StatusFail, eventsDiskIO: StatusFail, eventsFSReadonly: StatusFail, eventsLockup: StatusFail,
	eventsSegfault: StatusWarn, eventsHungTask: StatusWarn,
}

func checkKernel(facts checkFacts) Result {
	const id = "kernel"
	window := eventsDurationText(checkKernelWindow)
	if facts.Kernel.KernelErr != nil {
		return checkResult(id, StatusSkip, eventsSkip(id, "/dev/kmsg", facts.Kernel.KernelErr).Summary)
	}
	counts := map[string]int{}
	status := map[string]Status{}
	var kinds, lines []string
	for _, signal := range facts.Kernel.Signals {
		level, ok := checkKernelStatus[signal.Kind]
		if signal.Kind == eventsOops {
			level, ok = StatusWarn, true
			if signal.Failed {
				level = StatusFail
			}
		}
		if !ok {
			continue
		}
		if counts[signal.Kind] == 0 {
			kinds = append(kinds, signal.Kind)
		}
		counts[signal.Kind]++
		if checkSeverity(level) > checkSeverity(status[signal.Kind]) {
			status[signal.Kind] = level
		}
		lines = append(lines, signal.Time.Format(eventsTimeLayout)+"  "+signal.Kind+"  "+signal.Brief)
	}
	if len(kinds) == 0 {
		return checkResult(id, StatusPass, T("check.kernel.none", window))
	}
	var issues checkIssues
	for _, kind := range kinds {
		issues.add(status[kind], fmt.Sprintf("%s ×%d", kind, counts[kind]))
	}
	result := checkResult(id, issues.status, T("check.kernel.found", window, strings.Join(issues.lines, ", ")))
	if len(lines) > checkEvidenceLines {
		lines = lines[len(lines)-checkEvidenceLines:]
	}
	result.Evidence = []Evidence{{Label: T("check.evidence.events"), Value: strings.Join(lines, "\n")}}
	result.Next = "edc events --since " + window
	if facts.KernelNext != "" {
		result.Next = facts.KernelNext
	}
	if issues.status == StatusFail {
		result.Error = &DiagnosticError{Kind: "check", Message: result.Summary}
	}
	return result
}

func checkTime(facts checkFacts) Result {
	const id = "time"
	if facts.Clock == nil {
		return checkResult(id, StatusSkip, T("check.time.unknown"))
	}
	var issues checkIssues
	if !facts.Clock.Synced {
		issues.add(StatusWarn, T("check.time.unsynced"))
	}
	return issues.result(id, T("check.time.synced"), "timedatectl status")
}

func checkNetworkResult(facts checkFacts, policy checkPolicy) Result {
	const id = "network"
	network := facts.Network
	var issues checkIssues
	next := "edc info -v"
	if network.RouteErr == nil && network.Route == "" {
		issues.add(StatusFail, T("check.network.no_route"))
		next = "ip route"
	}
	switch {
	case errors.Is(network.DNS.Err, fs.ErrNotExist):
		issues.add(StatusWarn, T("check.network.no_resolv"))
	case network.DNS.Err != nil:
		issues.add(StatusWarn, T("check.network.resolv_unreadable", infoSingleLine(network.DNS.Err.Error())))
	case len(network.DNS.Config.Nameservers) == 0:
		issues.add(StatusWarn, T("check.network.no_nameserver"))
	}
	if network.ConntrackOK {
		if status := checkUsageStatus(network.Conntrack, policy.usage); status != StatusPass {
			issues.add(status, T("check.network.conntrack", network.Conntrack))
		}
	}
	if network.TCPMemoryOK {
		if status := checkUsageStatus(network.TCPMemory, policy.usage); status != StatusPass {
			issues.add(status, T("check.network.tcp_memory", network.TCPMemory))
		}
	}
	route := network.Route
	if network.RouteErr != nil {
		route = T("check.network.route_unknown")
	}
	return issues.result(id, T("check.network.ok", route, len(network.DNS.Config.Nameservers)), next)
}

func checkContainers(facts checkFacts, policy checkPolicy) Result {
	const id = "container"
	err := facts.ContainersErr
	switch {
	case errors.Is(err, errCtrNoDocker):
		return checkResult(id, StatusSkip, T("ctr.skip.no_docker"))
	case err != nil && strings.Contains(err.Error(), "permission denied"):
		return checkResult(id, StatusSkip, T("ctr.skip.needs_docker_access"))
	case err != nil:
		// socket은 있는데 연결이 안 되면 docker daemon이 멈춘 것이다. container를 모르는 채로 통과로 두지 않는다.
		result := checkResult(id, StatusWarn, T("check.container.unreachable", infoSingleLine(err.Error())))
		result.Next = "edc svc docker"
		return result
	}
	var issues checkIssues
	var failed, recent, attention []string
	now := facts.Now
	if now.IsZero() {
		now = time.Now()
	}
	for _, item := range facts.Containers {
		if (item.failed() || item.attention()) && checkMatches(policy.acceptContainers, item.Name) {
			issues.accepted = append(issues.accepted, item.Name)
			continue
		}
		// restart policy가 있는 container는 돌고 있어야 한다. 없는 container는 사람이 멈췄을 수 있어 최근에 멈춘 것만 경고한다.
		switch {
		case item.failed() && item.RestartPolicy != "" && item.RestartPolicy != "no":
			failed = append(failed, item.Name+" ("+item.State+")")
		case item.failed() && !item.FinishedAt.IsZero() && now.Sub(item.FinishedAt) < checkRecentStop:
			recent = append(recent, item.Name+" ("+item.State+")")
		case item.failed():
		case item.attention():
			attention = append(attention, item.Name+" ("+item.State+")")
		}
	}
	next := ""
	if len(failed) > 0 {
		issues.add(StatusFail, T("check.container.failed", len(failed), checkNames(failed)))
		next = "edc ctr " + strings.Fields(failed[0])[0]
	}
	if len(recent) > 0 {
		issues.add(StatusWarn, T("check.container.recent", len(recent), checkNames(recent)))
		if next == "" {
			next = "edc ctr " + strings.Fields(recent[0])[0]
		}
	}
	if len(attention) > 0 {
		issues.add(StatusWarn, T("check.container.attention", len(attention), checkNames(attention)))
		if next == "" {
			next = "edc ctr " + strings.Fields(attention[0])[0]
		}
	}
	return issues.result(id, T("check.container.ok", len(facts.Containers)), next)
}

func checkFileHandles(facts checkFacts, policy checkPolicy) Result {
	const id = "filehandles"
	if facts.FileHandlesErr != nil {
		return checkSkip(id, "/proc/sys/fs/file-nr", facts.FileHandlesErr)
	}
	handles := facts.FileHandles
	if handles == nil || handles.Max == 0 {
		return checkResult(id, StatusSkip, T("check.filehandles.unknown"))
	}
	if handles.Max >= checkUnlimitedFileHandles {
		return checkResult(id, StatusPass, T("check.filehandles.unlimited", handles.Used))
	}
	percent := float64(handles.Used) / float64(handles.Max) * 100
	text := T("check.filehandles.usage", handles.Used, handles.Max, percent)
	var issues checkIssues
	if status := checkUsageStatus(percent, policy.usage); status != StatusPass {
		issues.add(status, text)
	}
	return issues.result(id, text, "edc info -v")
}

// checkRebootResult는 audit의 patch.reboot와 같은 기준이다. audit는 보안 점검에서, check는 지금 상태 점검에서 같은 사실을 본다.
func checkRebootResult(facts checkFacts) Result {
	const id = "reboot"
	reboot := facts.Reboot
	if reboot.Err != nil {
		return checkSkip(id, "/var/run/reboot-required", reboot.Err)
	}
	if reboot.Flag {
		packages := "/var/run/reboot-required"
		if len(reboot.Packages) > 0 {
			packages = checkNames(reboot.Packages)
		}
		return checkResult(id, StatusWarn, T("check.reboot.flag", packages))
	}
	// 실행 중인 커널이 /boot에 없으면 직접 넣은 커널이나 container다. 설치된 커널과 비교할 기준이 없다.
	if reboot.Running == "" || !slices.Contains(reboot.Kernels, reboot.Running) {
		return checkResult(id, StatusPass, T("check.reboot.none"))
	}
	newest := reboot.Running
	for _, release := range reboot.Kernels {
		if compareKernelReleases(release, newest) > 0 {
			newest = release
		}
	}
	if newest != reboot.Running {
		return checkResult(id, StatusWarn, T("check.reboot.kernel", newest, reboot.Running))
	}
	return checkResult(id, StatusPass, T("check.reboot.ok", reboot.Running))
}

// checkThermal은 macOS의 thermal pressure다. heavy부터 CPU 속도를 크게 낮추고, trapping부터는 장치를 보호하려고 일을 멈춘다.
// moderate는 부하가 걸리면 흔히 오는 단계라 판정하지 않고 요약에만 적는다.
func checkThermal(facts checkFacts) Result {
	const id = "thermal"
	if facts.ThermalErr != nil {
		return checkSkip(id, "notifyutil", facts.ThermalErr)
	}
	names := []string{"nominal", "moderate", "heavy", "trapping", "sleeping"}
	name := fmt.Sprint(facts.Thermal)
	if facts.Thermal >= 0 && facts.Thermal < len(names) {
		name = names[facts.Thermal]
	}
	text := T("check.thermal.level", name)
	var issues checkIssues
	switch {
	case facts.Thermal >= 3:
		issues.add(StatusFail, text)
	case facts.Thermal == 2:
		issues.add(StatusWarn, text)
	}
	return issues.result(id, text, "pmset -g therm")
}

func checkBackupResult(facts checkFacts) Result {
	const id = "backup"
	backup := facts.Backup
	switch {
	case backup.Err != nil:
		return checkSkip(id, "tmutil", backup.Err)
	case !backup.Configured:
		return checkResult(id, StatusSkip, T("check.backup.none"))
	case backup.Latest.IsZero():
		return checkResult(id, StatusSkip, T("check.backup.unknown"))
	}
	now := facts.Now
	if now.IsZero() {
		now = time.Now()
	}
	age := now.Sub(backup.Latest)
	text := T("check.backup.latest", backup.Latest.Format(eventsTimeLayout), eventsDurationText(age.Truncate(time.Hour)))
	var issues checkIssues
	if age > checkBackupAge {
		issues.add(StatusWarn, text)
	}
	return issues.result(id, text, "tmutil latestbackup")
}
