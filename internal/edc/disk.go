package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	diskProbeCheck = "disk.check"
	diskProbeGrow  = "disk.grow"
	diskSectorSize = 512
	// diskGrowMinGap보다 작은 차이는 정렬과 메타데이터의 몫이다. 이것까지 늘릴 공간으로 보면
	// 이미 다 늘린 디스크에서도 매번 할 일이 남았다고 말한다.
	diskGrowMinGap = 1 << 20
	// GPT는 디스크 끝 33 섹터에 백업 헤더를 둔다. 파티션은 그 앞까지만 늘어난다.
	diskGPTTailBytes = 34 * diskSectorSize
	// MBR은 섹터 번호를 32비트로 적으므로 파티션 끝이 2 TiB를 넘지 못한다.
	diskMBRLimitBytes = (1 << 32) * diskSectorSize
	// diskMaxSteps는 rescan을 뺀 단계 수(partition, pv, lv, filesystem)보다 넉넉하다.
	// 단계가 크기를 바꾸지 못하면 같은 단계를 되풀이하지 않고 멈추므로 실제로는 닿지 않는다.
	diskMaxSteps = 6
)

// diskGrowFSTypes는 마운트한 채로 늘릴 수 있는 파일시스템이다. btrfs와 zfs는 크기를 다루는
// 명령과 의미가 달라 이번 범위에서 뺀다.
var diskGrowFSTypes = map[string]bool{"ext3": true, "ext4": true, "xfs": true}

// diskStep은 계획의 한 단계다. From과 To는 그 층의 지금 크기와 단계 뒤의 예상 크기다.
type diskStep struct {
	Layer    string
	Command  []string
	From, To uint64
}

func (step diskStep) commandLine() string { return strings.Join(step.Command, " ") }

// planDiskGrow는 아래 층부터 늘릴 공간을 계산한다. 아래 층이 늘면 그 위 층은 같은 공간만큼 늘 일이
// 생기므로, 아직 실행하지 않은 단계의 결과를 예상 크기로 넘긴다.
func planDiskGrow(chain diskChain) ([]diskStep, string) {
	if chain.Blocked != "" {
		return nil, chain.Blocked
	}
	var steps []diskStep
	deviceSize := chain.Disk.Size
	if chain.Part != nil {
		limit := chain.DiskEnd
		if chain.NextStart > 0 {
			limit = chain.NextStart
		}
		if chain.Table == "dos" && limit > diskMBRLimitBytes {
			limit = diskMBRLimitBytes
		}
		partEnd := chain.Part.end()
		switch {
		case limit >= partEnd+diskGrowMinGap:
			steps = append(steps, diskStep{
				Layer: "partition", Command: []string{"growpart", "/dev/" + chain.Disk.Name, strconv.Itoa(chain.Part.Partition)},
				From: chain.Part.Size, To: limit - chain.Part.Start,
			})
		case chain.NextStart > 0 && chain.DiskEnd >= chain.LastEnd+diskGrowMinGap:
			return nil, T("disk.blocked.not_last", chain.Part.Name, formatBytes(chain.DiskEnd-chain.LastEnd))
		case chain.Table == "dos" && chain.DiskEnd >= diskMBRLimitBytes+diskGrowMinGap && partEnd >= diskMBRLimitBytes-diskGrowMinGap:
			return nil, T("disk.blocked.mbr_limit", chain.Disk.Name)
		}
		deviceSize = chain.Part.Size
		if len(steps) > 0 {
			deviceSize = steps[len(steps)-1].To
		}
	}
	if chain.PV != nil && chain.LV != nil {
		extent := diskExtent(chain.PV.ExtentSize)
		usable := uint64(0)
		if deviceSize > chain.PV.PEStart {
			usable = (deviceSize - chain.PV.PEStart) / extent
		}
		added := uint64(0)
		if usable > chain.PV.PECount {
			added = (usable - chain.PV.PECount) * extent
			steps = append(steps, diskStep{
				Layer: "pv", Command: []string{"pvresize", chain.PV.Path},
				From: chain.PV.PECount * extent, To: usable * extent,
			})
		}
		free := chain.LV.VGFree + added
		deviceSize = chain.LV.Size
		if free >= diskExtent(chain.LV.ExtentSize) {
			steps = append(steps, diskStep{
				Layer: "lv", Command: []string{"lvextend", "-l", "+100%FREE", chain.LV.Path},
				From: chain.LV.Size, To: chain.LV.Size + free,
			})
			deviceSize = chain.LV.Size + free
		}
	}
	if len(steps) > 0 || (chain.FSSizeErr == nil && deviceSize >= chain.FSSize+diskGrowMinGap) {
		command := []string{"resize2fs", chain.fsDevicePath()}
		if chain.Mount.FSType == "xfs" {
			command = []string{"xfs_growfs", "-d", chain.Mount.Point}
		}
		steps = append(steps, diskStep{Layer: "filesystem", Command: command, From: chain.FSSize, To: deviceSize})
	}
	return steps, ""
}

// diskExtent는 extent 크기를 읽지 못해 0일 때 나눗셈이 터지지 않게 한다.
func diskExtent(size uint64) uint64 {
	if size == 0 {
		return 1
	}
	return size
}

// diskEvidence는 층마다 한 줄을 만든다. 늘어날 층에는 예상 크기를 화살표로 붙인다.
func diskEvidence(chain diskChain, steps []diskStep) []Evidence {
	planned := map[string]diskStep{}
	for _, step := range steps {
		planned[step.Layer] = step
	}
	sized := func(name string, size uint64, layer string) string {
		if step, ok := planned[layer]; ok {
			return fmt.Sprintf("%s · %s → %s", name, formatBytes(size), formatBytes(step.To))
		}
		return fmt.Sprintf("%s · %s", name, formatBytes(size))
	}
	var evidence []Evidence
	if chain.Disk.Name != "" {
		value := fmt.Sprintf("%s · %s", chain.Disk.Name, formatBytes(chain.Disk.Size))
		if chain.Table != "" {
			value += " · " + chain.Table
		}
		if chain.Disk.Rescan {
			value += " · " + T("disk.evidence.rescan")
		}
		evidence = append(evidence, Evidence{Label: T("disk.layer.disk"), Value: value})
	}
	if chain.Part != nil {
		evidence = append(evidence, Evidence{Label: T("disk.layer.partition"), Value: sized(chain.Part.Name, chain.Part.Size, "partition")})
	}
	if chain.PV != nil {
		evidence = append(evidence, Evidence{Label: T("disk.layer.pv"), Value: sized(chain.PV.Path, chain.PV.PECount*chain.PV.ExtentSize, "pv")})
	}
	if chain.LV != nil {
		evidence = append(evidence, Evidence{Label: T("disk.layer.lv"), Value: sized(chain.LV.VG+"/"+chain.LV.Name, chain.LV.Size, "lv")})
	}
	if chain.FS.Name != "" {
		value := chain.Mount.FSType + " · " + T("disk.evidence.unknown_size", chain.FSSizeErr)
		if chain.FSSizeErr == nil {
			value = sized(chain.Mount.FSType, chain.FSSize, "filesystem")
		}
		evidence = append(evidence, Evidence{Label: T("disk.layer.filesystem"), Value: value})
	}
	for index, step := range steps {
		evidence = append(evidence, Evidence{Label: T("disk.evidence.step", index+1), Value: step.commandLine()})
	}
	return evidence
}

func diskGrowth(steps []diskStep) uint64 {
	if len(steps) == 0 {
		return 0
	}
	last := steps[len(steps)-1]
	if last.To <= last.From {
		return 0
	}
	return last.To - last.From
}

// checkDiskMount는 check의 마운트 하나다. 늘릴 공간이 남았으면 warn으로 눈에 띄게 한다.
func checkDiskMount(ctx context.Context, system diskSystem, mount diskMount) Result {
	started := time.Now()
	chain, err := system.readChain(ctx, mount)
	if err != nil {
		return resultFromError(diskProbeCheck, started, "disk", fmt.Errorf("%s: %w", mount.Point, err))
	}
	result := Result{Probe: diskProbeCheck, Status: StatusPass, StartedAt: started.UTC()}
	if !diskGrowFSTypes[mount.FSType] {
		result.Status, result.Summary = StatusSkip, mount.Point+": "+chain.Blocked
		return finishDiskResult(result, started)
	}
	steps, blocked := planDiskGrow(chain)
	result.Evidence = diskEvidence(chain, steps)
	switch {
	case blocked != "":
		result.Status, result.Summary = StatusWarn, mount.Point+": "+blocked
	case len(steps) > 0:
		result.Status = StatusWarn
		result.Summary = T("disk.check.growable", mount.Point, formatBytes(diskGrowth(steps)), mount.Point)
	case chain.FSSizeErr != nil:
		result.Status = StatusWarn
		result.Summary = T("disk.check.unknown", mount.Point)
	default:
		result.Summary = T("disk.check.full", mount.Point, formatBytes(chain.FSSize))
	}
	if chain.Disk.Rescan && blocked == "" {
		result.Warnings = append(result.Warnings, T("disk.check.rescan_note", chain.Disk.Name))
	}
	return finishDiskResult(result, started)
}

func finishDiskResult(result Result, started time.Time) Result {
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// checkDiskMounts는 인자가 없을 때 블록 디바이스 위의 마운트를 한 번씩만 본다. bind 마운트는
// root 필드가 "/"가 아니므로 건너뛴다.
func checkDiskMounts(ctx context.Context, system diskSystem, path string) []Result {
	mounts, err := system.mounts()
	if err != nil {
		return []Result{resultFromError(diskProbeCheck, time.Now(), "disk", err)}
	}
	if path != "" {
		mount, ok := findDiskMount(mounts, path)
		if !ok {
			return []Result{resultFromError(diskProbeCheck, time.Now(), "disk", errors.New(T("disk.error.no_mount", path)))}
		}
		return []Result{checkDiskMount(ctx, system, mount)}
	}
	seen := map[string]bool{}
	var results []Result
	for _, mount := range mounts {
		key := fmt.Sprintf("%d:%d", mount.Major, mount.Minor)
		// 이름을 주지 않았을 때는 늘릴 수 있는 파일시스템만 본다. vfat인 /boot/efi 같은 줄은 매번 skip으로 남아 소음이 된다.
		if mount.Major == 0 || mount.Root != "/" || !strings.HasPrefix(mount.Source, "/dev/") || !diskGrowFSTypes[mount.FSType] || seen[key] {
			continue
		}
		seen[key] = true
		results = append(results, checkDiskMount(ctx, system, mount))
	}
	if len(results) == 0 {
		return []Result{unsupported(diskProbeCheck, T("disk.check.none"))}
	}
	return results
}

type diskGrowInput struct {
	path    string
	dryRun  bool
	confirm func(detail, question string) (bool, error)
	// notify는 확인 뒤에 신호를 받기 시작한다. 확인 전에 받으면 파이프 입력을 읽는 확인이 Ctrl+C로
	// 끝나지 않는다. nil이면 신호를 보지 않는다.
	notify func() (<-chan os.Signal, func())
}

type diskGrowOutcome struct {
	Result    Result
	Cancelled bool
}

func (system diskSystem) missingTool(steps []diskStep) string {
	for _, step := range steps {
		if err := system.lookPath(step.Command[0]); err != nil {
			if step.Command[0] == "growpart" {
				return T("disk.blocked.no_growpart")
			}
			return T("disk.blocked.no_tool", step.Command[0])
		}
	}
	return ""
}

// rescan은 OCI 문서의 순서를 따른다. 한 블록을 직접 읽은 뒤 SCSI 디바이스에 크기를 다시 읽게 한다.
// AWS Nitro의 NVMe와 virtio-blk에는 rescan 파일이 없고 커널이 새 크기를 바로 본다.
func (system diskSystem) rescan(ctx context.Context, disk string) error {
	if _, err := system.run(ctx, "dd", "iflag=direct", "if=/dev/"+disk, "of=/dev/null", "count=1"); err != nil {
		return err
	}
	return os.WriteFile(system.path("sys/class/block", disk, "device", "rescan"), []byte("1"), 0o200)
}

// executeDiskGrow는 rescan → 계획 → 확인 → 단계 실행 순서로 돈다. 단계마다 층을 다시 읽어 그 층이
// 실제로 늘었는지 본다. 같은 명령을 다시 실행하면 이미 늘어난 층은 계획에서 빠지므로 중간에 끊긴
// 실행을 이어 간다.
func executeDiskGrow(ctx context.Context, system diskSystem, input diskGrowInput) diskGrowOutcome {
	started := time.Now()
	fail := func(kind string, err error) diskGrowOutcome {
		return diskGrowOutcome{Result: resultFromError(diskProbeGrow, started, kind, err)}
	}
	mounts, err := system.mounts()
	if err != nil {
		return fail("disk", err)
	}
	mount, ok := findDiskMount(mounts, input.path)
	if !ok {
		return fail("disk", errors.New(T("disk.error.no_mount", input.path)))
	}
	chain, err := system.readChain(ctx, mount)
	if err != nil {
		return fail("disk", err)
	}
	// 디스크를 바꾸는 명령은 시간 제한으로 끊지 않는다. resize2fs를 죽여도 커널은 확장을 끝까지
	// 하므로, 끊으면 실제로는 늘어난 디스크를 실패로 보고한다. 시간 제한은 계획을 세우는 읽기에만 건다.
	changeCtx := context.WithoutCancel(ctx)
	var done []Evidence
	if chain.Disk.Rescan && !input.dryRun {
		if err := system.rescan(changeCtx, chain.Disk.Name); err != nil {
			return fail("rescan", fmt.Errorf("rescan %s: %w", chain.Disk.Name, err))
		}
		done = append(done, Evidence{Label: T("disk.evidence.done"), Value: "rescan " + chain.Disk.Name})
		if chain, err = system.readChain(changeCtx, mount); err != nil {
			return fail("disk", err)
		}
	}
	steps, blocked := planDiskGrow(chain)
	if blocked == "" {
		blocked = system.missingTool(steps)
	}
	if blocked != "" {
		result := fail("plan", errors.New(mount.Point+": "+blocked))
		result.Result.Evidence = diskEvidence(chain, nil)
		return result
	}
	if len(steps) == 0 {
		if chain.FSSizeErr != nil {
			return fail("disk", fmt.Errorf("%s: %w", mount.Point, chain.FSSizeErr))
		}
		return diskGrowOutcome{Result: finishDiskResult(Result{
			Probe: diskProbeGrow, Status: StatusPass, StartedAt: started.UTC(),
			Summary: T("disk.check.full", mount.Point, formatBytes(chain.FSSize)), Evidence: append(done, diskEvidence(chain, nil)...),
		}, started)}
	}
	if input.dryRun {
		result := Result{
			Probe: diskProbeGrow, Status: StatusPass, StartedAt: started.UTC(),
			Summary:  T("disk.grow.dry_run", mount.Point, formatBytes(diskGrowth(steps))),
			Evidence: diskEvidence(chain, steps),
		}
		if chain.Disk.Rescan {
			result.Warnings = append(result.Warnings, T("disk.check.rescan_note", chain.Disk.Name))
		}
		return diskGrowOutcome{Result: finishDiskResult(result, started)}
	}
	confirmed, err := input.confirm(diskConfirmDetail(chain, steps), T("disk.grow.confirm.question", mount.Point))
	if err != nil {
		return fail("confirm", err)
	}
	if !confirmed {
		return diskGrowOutcome{Cancelled: true}
	}
	var signals <-chan os.Signal
	if input.notify != nil {
		var stop func()
		signals, stop = input.notify()
		defer stop()
	}
	before := chain.FSSize
	for attempt := 0; attempt < diskMaxSteps; attempt++ {
		steps, blocked = planDiskGrow(chain)
		if blocked != "" {
			return failDiskStep(started, done, chain, errors.New(blocked))
		}
		if len(steps) == 0 {
			break
		}
		// 신호는 다음 단계 앞에서만 본다. 마지막 단계 중에 온 신호는 이미 끝난 일을 실패로 만들지 않는다.
		select {
		case <-signals:
			if attempt == 0 {
				return diskGrowOutcome{Cancelled: true}
			}
			return failDiskStep(started, done, chain, errors.New(T("disk.grow.error.interrupted", done[len(done)-1].Value)))
		default:
		}
		step := steps[0]
		if _, err := system.run(changeCtx, step.Command[0], step.Command[1:]...); err != nil {
			return failDiskStep(started, done, chain, fmt.Errorf("%s: %w", step.commandLine(), err))
		}
		done = append(done, Evidence{Label: T("disk.evidence.done"), Value: step.commandLine()})
		if chain, err = system.readChain(changeCtx, mount); err != nil {
			return failDiskStep(started, done, chain, err)
		}
		if next, _ := planDiskGrow(chain); len(next) > 0 && next[0].Layer == step.Layer {
			return failDiskStep(started, done, chain, errors.New(T("disk.grow.error.unchanged", step.commandLine())))
		}
	}
	if chain.FSSizeErr != nil {
		return failDiskStep(started, done, chain, chain.FSSizeErr)
	}
	return diskGrowOutcome{Result: finishDiskResult(Result{
		Probe: diskProbeGrow, Status: StatusPass, StartedAt: started.UTC(),
		Summary:  T("disk.grow.summary", mount.Point, formatBytes(before), formatBytes(chain.FSSize)),
		Evidence: append(done, diskEvidence(chain, nil)...),
	}, started)}
}

func failDiskStep(started time.Time, done []Evidence, chain diskChain, err error) diskGrowOutcome {
	result := resultFromError(diskProbeGrow, started, "grow", err)
	result.Evidence = append(done, diskEvidence(chain, nil)...)
	result.Warnings = append(result.Warnings, T("disk.grow.error.resume"))
	return diskGrowOutcome{Result: result}
}

func diskConfirmDetail(chain diskChain, steps []diskStep) string {
	var builder strings.Builder
	for _, evidence := range diskEvidence(chain, steps) {
		fmt.Fprintf(&builder, "  %-12s %s\n", evidence.Label, evidence.Value)
	}
	builder.WriteString(T("disk.grow.confirm.detail") + "\n")
	return builder.String()
}

func diskTerminalConfirm(detail, question string) (bool, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return runConfirmModel(os.Stdin, os.Stdout, newDetailedConfirmModel(detail, question, false))
	}
	fmt.Fprint(os.Stdout, detail)
	return confirm(os.Stdin, os.Stdout, question+" [y/N] ", false), nil
}

func diskAutoConfirm(string, string) (bool, error) { return true, nil }

// diskNotifySignals는 단계 중의 Ctrl+C와 종료 신호를 잡아 edc가 지금 단계를 끝낸 뒤 멈추게 한다.
// 단계 명령은 자기 프로세스 그룹에서 돌므로 터미널의 Ctrl+C를 받지 않는다.
func diskNotifySignals() (<-chan os.Signal, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	return signals, func() { signal.Stop(signals) }
}

// diskCommandMessage는 실패한 명령의 출력에서 마지막 의미 있는 줄을 고른다. resize2fs처럼 첫 줄에
// 버전을 쓰는 명령은 첫 줄로 이유를 알 수 없다. stderr가 비면 stdout을 본다. growpart는 이유를
// stdout에 쓴다.
func diskCommandMessage(stderr, stdout string) string {
	for _, output := range []string{stderr, stdout} {
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for index := len(lines) - 1; index >= 0; index-- {
			if line := strings.TrimSpace(lines[index]); line != "" {
				return line
			}
		}
	}
	return ""
}

func runDisk(args []string, version string) int {
	usage := T("cli.usage", "edc disk <check|grow> ...")
	if len(args) == 0 {
		choice, ok := promptMissingChoice("edc disk", []string{"check", "grow"})
		if !ok {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		args = []string{choice}
	}
	switch args[0] {
	case "check":
		return runDiskCheck(args[1:], version)
	case "grow":
		return runDiskGrow(args[1:], version)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
}

func runDiskCheck(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet("disk check", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc disk check [mount]"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	system, ok := newDiskSystem()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported(diskProbeCheck, T("disk.skip.linux_only"))}, options.redact))
	}
	return emit(options, buildReport(version, started, nil, checkDiskMounts(ctx, system, set.Arg(0)), options.redact))
}

func runDiskGrow(args []string, version string) int {
	options := configuredCommon(5 * time.Minute)
	set := flag.NewFlagSet("disk grow", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	yes := set.Bool("yes", false, T("disk.flag.yes"))
	dryRun := set.Bool("dry-run", false, T("disk.flag.dry_run"))
	set.BoolVar(dryRun, "n", false, T("disk.flag.dry_run"))
	// `edc disk grow / -n`처럼 마운트 뒤에 온 flag도 받는다. flag 패키지는 첫 위치 인자에서 멈춘다.
	ordered, err := reorderFSWatchArgs(set, args)
	if err == nil {
		err = set.Parse(ordered)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			set.Usage()
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	if set.NArg() != 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc disk grow <mount> [-n] [--yes]"))
		return 2
	}
	// 내부에서 sudo를 붙이지 않는다. --dry-run은 읽기만 하므로 root 없이도 계획을 볼 수 있다.
	if os.Geteuid() != 0 && !*dryRun {
		fmt.Fprintln(os.Stderr, T("disk.grow.error.root_required"))
		return 3
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	system, ok := newDiskSystem()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported(diskProbeGrow, T("disk.skip.linux_only"))}, options.redact))
	}
	input := diskGrowInput{path: set.Arg(0), dryRun: *dryRun, confirm: diskTerminalConfirm, notify: diskNotifySignals}
	if *yes {
		input.confirm = diskAutoConfirm
	}
	outcome := executeDiskGrow(ctx, system, input)
	if outcome.Cancelled {
		fmt.Fprintln(os.Stderr, T("disk.grow.cancelled"))
		return 4
	}
	return emit(options, buildReport(version, started, nil, []Result{outcome.Result}, options.redact))
}
