package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
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

// checkImageFileSystems는 원래 읽기 전용인 파일 시스템이다. read-only라도 문제가 아니다.
var checkImageFileSystems = map[string]bool{"squashfs": true, "iso9660": true, "erofs": true, "cramfs": true, "udf": true}

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
	// TimedOut은 --timeout 안에 끝나지 않은 원천의 점검 이름이다. 그 점검은 skip이다.
	TimedOut []string
}

func runCheck(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("check")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc check [options]"))
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
	return emit(options, buildReport(version, started, nil, evaluateCheck(collectCheckFacts(ctx, sources)), options.redact))
}

// collectCheckFacts는 원천을 함께 읽는다. systemctl, journalctl, docker socket이 각각 외부 호출이라 차례로 읽으면 느리다.
// ctx가 끝나면 남은 원천을 기다리지 않고 TimedOut에 적는다. df처럼 ctx를 받지 않는 원천이 멈춘 NFS mount에 걸려도
// --timeout 안에 끝나야 하기 때문이다. 늦게 끝난 원천은 버퍼가 있는 channel에 결과를 놓고 끝나므로 facts를 건드리지 않는다.
func collectCheckFacts(ctx context.Context, sources checkSources) checkFacts {
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
			return func(facts *checkFacts) { facts.Memory, facts.MemoryErr = memory, err }
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
	}
	done := make(chan collected, len(jobs))
	pending := map[string]bool{}
	for id, job := range jobs {
		pending[id] = true
		go func() { done <- collected{id: id, apply: job()} }()
	}
	var facts checkFacts
	for len(pending) > 0 {
		select {
		case result := <-done:
			result.apply(&facts)
			delete(pending, result.id)
		case <-ctx.Done():
			facts.TimedOut = slices.Sorted(maps.Keys(pending))
			return facts
		}
	}
	return facts
}

func evaluateCheck(facts checkFacts) []Result {
	results := []Result{
		checkServices(facts),
		checkProcesses(facts),
		checkMemory(facts),
		checkDisks(facts),
		checkKernel(facts),
		checkTime(facts),
		checkNetworkResult(facts),
		checkContainers(facts),
		checkFileHandles(facts),
		checkRebootResult(facts),
	}
	for index, result := range results {
		if id := strings.TrimPrefix(result.Probe, "check."); slices.Contains(facts.TimedOut, id) {
			results[index] = checkResult(id, StatusSkip, T("check.skip.timed_out"))
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

func checkServices(facts checkFacts) Result {
	const id = "service"
	if errors.Is(facts.ServicesErr, errSvcNoSystemd) {
		return checkResult(id, StatusSkip, T("svc.skip.no_systemd"))
	}
	if facts.ServicesErr != nil {
		return checkSkip(id, "systemctl", facts.ServicesErr)
	}
	var failed, changing []string
	for _, item := range facts.Services {
		switch {
		case item.failed():
			failed = append(failed, item.Unit)
		case item.changing():
			changing = append(changing, item.Unit+" ("+item.state()+")")
		}
	}
	var issues checkIssues
	next := ""
	if len(failed) > 0 {
		issues.add(StatusFail, T("check.service.failed", len(failed), checkNames(failed)))
		next = "edc svc " + failed[0]
	}
	if len(changing) > 0 {
		issues.add(StatusWarn, T("check.service.changing", len(changing), checkNames(changing)))
		if next == "" {
			next = "edc svc " + strings.Fields(changing[0])[0]
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

func checkMemory(facts checkFacts) Result {
	const id = "memory"
	if facts.MemoryErr != nil {
		return checkSkip(id, "/proc/meminfo", facts.MemoryErr)
	}
	memory := facts.Memory
	percent := float64(memory.Available) / float64(memory.Total) * 100
	text := T("check.memory.available", formatSizeShort(memory.Available), formatSizeShort(memory.Total), percent)
	var issues checkIssues
	switch {
	case percent < checkMemoryLimits.danger:
		issues.add(StatusFail, text)
	case percent < checkMemoryLimits.warn:
		issues.add(StatusWarn, text)
	}
	return issues.result(id, text, "edc top")
}

func checkDisks(facts checkFacts) Result {
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
		if disk.Percent > highest.Percent {
			highest = disk
		}
		if status := checkUsageStatus(disk.Percent, diskLimits); status != StatusPass {
			issues.add(status, T("check.disk.usage", disk.Mount, disk.Percent))
			if disk.Percent > worst {
				next, worst = "edc du "+disk.Mount, disk.Percent
			}
		}
		if disk.InodesTotal > 0 {
			percent := float64(disk.InodesUsed) / float64(disk.InodesTotal) * 100
			if status := checkUsageStatus(percent, diskLimits); status != StatusPass {
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

func checkNetworkResult(facts checkFacts) Result {
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
		if status := checkUsageStatus(network.Conntrack, checkUsageLimits); status != StatusPass {
			issues.add(status, T("check.network.conntrack", network.Conntrack))
		}
	}
	if network.TCPMemoryOK {
		if status := checkUsageStatus(network.TCPMemory, checkUsageLimits); status != StatusPass {
			issues.add(status, T("check.network.tcp_memory", network.TCPMemory))
		}
	}
	route := network.Route
	if network.RouteErr != nil {
		route = T("check.network.route_unknown")
	}
	return issues.result(id, T("check.network.ok", route, len(network.DNS.Config.Nameservers)), next)
}

func checkContainers(facts checkFacts) Result {
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
	var failed, attention []string
	for _, item := range facts.Containers {
		switch {
		case item.failed():
			failed = append(failed, item.Name+" ("+item.State+")")
		case item.attention():
			attention = append(attention, item.Name+" ("+item.State+")")
		}
	}
	var issues checkIssues
	next := ""
	if len(failed) > 0 {
		issues.add(StatusFail, T("check.container.failed", len(failed), checkNames(failed)))
		next = "edc ctr " + strings.Fields(failed[0])[0]
	}
	if len(attention) > 0 {
		issues.add(StatusWarn, T("check.container.attention", len(attention), checkNames(attention)))
		if next == "" {
			next = "edc ctr " + strings.Fields(attention[0])[0]
		}
	}
	return issues.result(id, T("check.container.ok", len(facts.Containers)), next)
}

func checkFileHandles(facts checkFacts) Result {
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
	if status := checkUsageStatus(percent, checkUsageLimits); status != StatusPass {
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
