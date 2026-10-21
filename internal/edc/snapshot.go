package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	snapshotDefaultSince = 6 * time.Hour
	// snapshotGrace는 budget이 끝난 뒤 ctx를 보는 section이 모은 결과를 돌려줄 여유다. 이보다 늦은 section은
	// hung NFS 위의 df처럼 ctx를 보지 않는 것으로 보고 더 기다리지 않는다.
	snapshotGrace = 2 * time.Second
	// snapshotTopProcesses는 Evidence에 남기는 RSS 상위 process 수다. report 크기 제한을 넘지 않게 한다.
	snapshotTopProcesses = 5
	// snapshotPressureWarn은 PSI some avg10의 경고 기준(%)이다. 10초 중 1초 넘게 기다렸다는 뜻이다.
	snapshotPressureWarn = 10
)

const snapshotUsage = "edc snapshot [--output <file|->] [--write [db] | --no-write] [--since 6h] [--force] [--du <path>]... [--timeout 15s] [--redact]"

// runSnapshot은 host 상태를 report 하나로 저장한다. 장애 전후에 한 번씩 찍어 edc report diff로 비교하려는 것이다.
// 공인 IP 조회처럼 host 밖으로 나가는 요청은 하지 않는다.
func runSnapshot(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("snapshot")
	set.SetOutput(os.Stderr)
	output := set.String("output", "", T("command.snapshot.option.output"))
	since := set.Duration("since", configuredDuration(activeConfig.Defaults.Snapshot.Since, snapshotDefaultSince), T("command.snapshot.option.since"))
	force := set.Bool("force", false, T("command.snapshot.option.force"))
	var duPaths snapshotPaths
	set.Var(&duPaths, "du", T("command.snapshot.option.du"))
	writePath := set.String("write", "", T("command.snapshot.option.write"))
	set.StringVar(writePath, "w", "", T("command.snapshot.option.write"))
	noWrite := set.Bool("no-write", false, T("command.snapshot.option.no_write"))
	set.DurationVar(&options.timeout, "timeout", options.timeout, T("option.timeout"))
	set.BoolVar(&options.redact, "redact", options.redact, T("option.redact"))
	args, defaultWrite := normalizeTopWriteArgs(args, set)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 || *since <= 0 || options.timeout <= 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", snapshotUsage))
		return 2
	}
	writeRequested := false
	set.Visit(func(option *flag.Flag) {
		if option.Name == "write" || option.Name == "w" {
			writeRequested = true
		}
	})
	useDefault := !*noWrite && *writePath == ""
	database, err := snapshotDatabasePath(*writePath, *output, *noWrite, writeRequested, defaultWrite)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if useDefault {
		if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, T("history.error.write", err))
			return 1
		}
	}
	*writePath = database
	record := database != ""
	started := time.Now()
	path := *output
	if path != "" && path != "-" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		path = absolute
	}
	if record && sameHistoryPath(*writePath, path) {
		fmt.Fprintln(os.Stderr, T("history.error.path"))
		return 2
	}
	// 수집은 timeout까지 걸릴 수 있으므로 쓰지 못할 경로는 먼저 거른다. 쓸 때 O_EXCL로 다시 막는다.
	if path != "" && path != "-" {
		if err := checkSnapshotPath(path, *force); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	results := collectSnapshot(ctx, snapshotSections(*since, duPaths, options.timeout), snapshotGrace)
	sortResults(results)
	report := buildReport(version, started, map[string]interface{}{"since": since.String()}, results, options.redact)
	if record {
		if err := recordSnapshot(*writePath, report, *since, duPaths); err != nil {
			fmt.Fprintln(os.Stderr, T("history.error.write", err))
			return 1
		}
	}
	if path == "-" {
		if err := writeJSON(os.Stdout, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if record {
			fmt.Fprintln(os.Stderr, T("snapshot.recorded", report.Run.ID, *writePath))
		}
		return exitCode(report.Results)
	}
	style := newTextStyle(richEnabled(os.Stdout))
	if path != "" {
		if err := writeSnapshotFile(path, report, *force); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Fprintln(os.Stdout, style.phrase("snapshot.saved", path, ""))
	}
	if record {
		fmt.Fprintln(os.Stdout, T("snapshot.recorded", report.Run.ID, *writePath))
	}
	if style.color {
		fmt.Fprintln(os.Stdout, richResultCounts(report.Results, style.width))
	} else {
		fmt.Fprintln(os.Stdout, summaryLine(report.Results))
	}
	fmt.Fprintln(os.Stdout)
	for _, line := range snapshotContentLines(report.Results, style.color) {
		fmt.Fprintln(os.Stdout, line)
	}
	fmt.Fprintln(os.Stdout)
	for _, hint := range snapshotOutputHints(style, path, *writePath, report.Run.ID) {
		fmt.Fprintln(os.Stdout, hint)
	}
	return exitCode(report.Results)
}

func snapshotDatabasePath(path, output string, noWrite, writeRequested, defaultWrite bool) (string, error) {
	if noWrite {
		if writeRequested || output == "" {
			return "", errors.New(T("snapshot.error.no_write"))
		}
		return "", nil
	}
	if writeRequested && path == "" && !defaultWrite {
		return "", errors.New(T("history.error.path"))
	}
	if path == "" {
		var err error
		path, err = defaultHistoryPath()
		if err != nil {
			return "", err
		}
	}
	return filepath.Abs(path)
}

func snapshotContentLines(results []Result, color bool) []string {
	type section struct {
		probe, label string
		results      []Result
	}
	sections := []section{
		{probe: "info.host", label: "host"},
		{probe: "info.memory", label: "memory"},
		{probe: "info.disk", label: "disk"},
		{probe: "info.processes", label: "processes"},
		{probe: listenProbeID, label: "listen"},
		{probe: "info.pressure", label: "pressure"},
		{probe: "audit", label: "audit"},
		{probe: "events", label: "events"},
		{probe: "changes", label: "changes"},
		{probe: duProbeID, label: "du"},
	}
	width := 0
	for i := range sections {
		for _, result := range results {
			probe := sections[i].probe
			if result.Probe == probe || strings.HasPrefix(result.Probe, probe+".") || strings.HasPrefix(result.Probe, probe+":") {
				sections[i].results = append(sections[i].results, result)
			}
		}
		if len(sections[i].results) > 0 {
			sections[i].label = T("snapshot.contents." + sections[i].label)
			width = max(width, liveWidth(sections[i].label))
		}
	}
	lines := []string{historyStrong(T("snapshot.contents.title"), richCyan, color)}
	for _, section := range sections {
		if len(section.results) == 0 {
			continue
		}
		summary := summarize(section.results)
		var counts []string
		for _, entry := range []struct {
			count  int
			status Status
		}{{summary.Pass, StatusPass}, {summary.Warn, StatusWarn}, {summary.Fail, StatusFail}, {summary.Skip, StatusSkip}} {
			if entry.count > 0 {
				counts = append(counts, historyTint(fmt.Sprintf("%d %s", entry.count, entry.status), richStatusColor(entry.status), color))
			}
		}
		lines = append(lines, "  "+liveCell(section.label, width)+"  "+strings.Join(counts, " · "))
	}
	return lines
}

func snapshotOutputHints(style textStyle, path, database, runID string) []string {
	var hints []string
	if path != "" && path != "-" {
		hints = append(hints,
			reportHint(T("snapshot.view_hint", quoteLocalShellWord(path)), style.color),
			reportHint(T("snapshot.list_hint", quoteLocalShellWord(filepath.Dir(path))), style.color),
			snapshotDiffHint(style, path))
	}
	if database == "" {
		hints = append(hints, reportHint(T("snapshot.file_only_hint"), style.color))
	} else {
		db := quoteLocalShellWord(database)
		hints = append(hints, reportHint(T("snapshot.history_hint", db)+"\n"+T("snapshot.trend_hint", db)+"\n"+T("snapshot.prune_hint", db), style.color))
		if path == "" {
			hints = append(hints, reportHint(T("snapshot.export_hint", quoteLocalShellWord(runID), db), style.color))
		}
	}
	return hints
}

// snapshotDiffHint는 비교 안내다. 색을 쓰면 그대로 복사할 명령 부분만 굵은 청록이다.
func snapshotDiffHint(style textStyle, path string) string {
	hint := T("snapshot.diff_hint", quoteLocalShellWord(path))
	index := strings.Index(hint, "edc ")
	if !style.color || index < 0 {
		return hint
	}
	return style.dim(hint[:index]) + style.command(hint[index:])
}

// snapshotSections는 각 명령이 쓰는 수집기와 판정을 그대로 부른다. 판정 기준이 명령과 snapshot에서 갈라지지 않게 한다.
// du는 경로 아래를 모두 읽어 오래 걸리므로 --du로 경로를 줄 때만 넣는다.
func snapshotSections(since time.Duration, duPaths []string, timeout time.Duration) []snapshotSection {
	sections := []snapshotSection{
		{name: "info.host", run: func(context.Context) []Result {
			started := time.Now()
			details, err := collectHostDetails()
			return []Result{snapshotHostResult(started, details, err)}
		}},
		{name: "info.memory", run: func(context.Context) []Result {
			started := time.Now()
			memory, err := collectInfoMemory()
			return []Result{snapshotMemoryResult(started, memory, err)}
		}},
		{name: "info.pressure", run: func(context.Context) []Result {
			return []Result{snapshotPressureResult(time.Now(), collectInfoSignals(false))}
		}},
		{name: "info.disk", run: func(context.Context) []Result {
			started := time.Now()
			disks, err := collectDisks()
			if err != nil {
				return snapshotDiskResults(started, nil, nil, err)
			}
			return snapshotDiskResults(started, visibleDisks(disks), collectDiskSignals(disks, false), nil)
		}},
		{name: "info.processes", run: func(context.Context) []Result {
			started := time.Now()
			processes, err := collectInfoProcesses()
			return []Result{snapshotProcessResult(started, processes, err)}
		}},
		{name: listenProbeID, run: func(ctx context.Context) []Result {
			return []Result{probeListen(ctx, defaultListenFamilies())}
		}},
		{name: "audit", run: func(ctx context.Context) []Result {
			sources, ok := hostAuditSources()
			if !ok {
				return []Result{unsupported("audit", T("audit.skip.linux_only"))}
			}
			facts := collectAuditFacts(ctx, sources)
			facts.AllowPorts = activeConfig.Audit.AllowPorts
			return applyAuditAccept(evaluateAudit(facts), activeConfig.Audit.Accept)
		}, ids: func() []string { return snapshotProbeIDs(evaluateAudit(auditFacts{})) }},
		{name: "events", run: func(ctx context.Context) []Result {
			sources, ok := hostEventsSources()
			if !ok {
				return []Result{unsupported("events", T("events.skip.linux_only"))}
			}
			return evaluateEvents(collectEventsFacts(ctx, sources, since))
		}, ids: func() []string { return snapshotProbeIDs(evaluateEvents(eventsFacts{})) }},
		{name: "changes", run: func(ctx context.Context) []Result {
			sources, ok := hostChangesSources()
			if !ok {
				return []Result{unsupported("changes", T("changes.skip.linux_only"))}
			}
			return evaluateChanges(ctx, sources, since)
		}, ids: func() []string { return []string{"changes.packages", "changes.etc", "changes.services"} }},
	}
	// 경로가 둘 이상이면 결과 이름에 경로를 붙여 report diff가 경로끼리 맞추게 한다. 시간 제한의 skip도 이 이름으로 남는다.
	for _, duPath := range duPaths {
		probe := snapshotDuProbe(duPaths, duPath)
		sections = append(sections, snapshotSection{name: probe, run: func(ctx context.Context) []Result {
			result := duSnapshotResult(ctx, duPath, timeout)
			result.Probe = probe
			return []Result{result}
		}})
	}
	return sections
}

type snapshotSection struct {
	name string
	run  func(context.Context) []Result
	// ids는 section이 정상일 때 내는 결과의 probe ID다. timeout이면 이 ID마다 skip을 남긴다. section 이름 하나로 남기면
	// report diff가 개별 점검을 removed로만 보이고 악화로 세지 않는다. 비어 있으면 section 이름 하나로 남긴다.
	ids func() []string
}

func snapshotProbeIDs(results []Result) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Probe)
	}
	return ids
}

// collectSnapshot은 section마다 goroutine을 하나 띄우고, 모두 끝나거나 ctx가 끝난 뒤 grace가 지날 때까지 기다린다.
// 그때까지 돌아오지 않은 section은 timeout skip으로 남긴다. goroutine은 크기 1인 자기 channel에만 쓰므로
// 늦게 끝나도 막히지 않고, 그 결과는 버려진다.
func collectSnapshot(ctx context.Context, sections []snapshotSection, grace time.Duration) []Result {
	started := time.Now()
	channels := make([]chan []Result, len(sections))
	for index, section := range sections {
		channel := make(chan []Result, 1)
		channels[index] = channel
		go func(run func(context.Context) []Result) { channel <- run(ctx) }(section.run)
	}
	budget := func() time.Duration {
		if deadline, ok := ctx.Deadline(); ok {
			return deadline.Sub(started).Round(time.Millisecond)
		}
		return time.Since(started).Round(time.Millisecond)
	}
	var results []Result
	var cutoff <-chan time.Time
	expired := false
	for index, channel := range channels {
		name := sections[index].name
		section := sections[index]
		timedOut := func() []Result {
			reason := T("snapshot.skip.timeout", name, budget())
			if section.ids == nil {
				return []Result{unsupported(name, reason)}
			}
			var skipped []Result
			for _, id := range section.ids() {
				skipped = append(skipped, unsupported(id, reason))
			}
			return skipped
		}
		if expired {
			select {
			case section := <-channel:
				results = append(results, section...)
			default:
				results = append(results, timedOut()...)
			}
			continue
		}
		if cutoff == nil {
			select {
			case section := <-channel:
				results = append(results, section...)
				continue
			case <-ctx.Done():
				cutoff = time.After(grace)
			}
		}
		select {
		case section := <-channel:
			results = append(results, section...)
		case <-cutoff:
			expired = true
			results = append(results, timedOut()...)
		}
	}
	return results
}

func snapshotHostResult(started time.Time, details hostDetails, err error) Result {
	if err != nil {
		return resultFromError("info.host", started, "snapshot", err)
	}
	return Result{
		Probe: "info.host", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("snapshot.summary.host", topHostOS(details), details.Machine, details.Cores, details.Load[0], details.Load[1], details.Load[2]),
		Metrics: map[string]interface{}{
			"os": details.OS, "version": details.Version, "release": details.Release, "machine": details.Machine, "model": details.Model,
			"cores": details.Cores, "uptime_seconds": int64(details.Uptime.Seconds()),
			"load1": details.Load[0], "load5": details.Load[1], "load15": details.Load[2],
			"memory_total_bytes": details.MemoryTotal, "swap_used_bytes": details.SwapUsed, "swap_total_bytes": details.SwapTotal,
			"nofile_soft": details.RLimitSoft, "nofile_hard": details.RLimitHard,
		},
	}
}

func snapshotMemoryResult(started time.Time, memory infoMemory, err error) Result {
	if err != nil {
		return resultFromError("info.memory", started, "snapshot", err)
	}
	used := memory.Total - min(memory.Available, memory.Total)
	percent := 0.0
	if memory.Total > 0 {
		percent = float64(used) / float64(memory.Total) * 100
	}
	status := StatusPass
	if percent >= diskLimits.warn {
		status = StatusWarn
	}
	return Result{
		Probe: "info.memory", Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("snapshot.summary.memory", formatBytes(used), formatBytes(memory.Total), percent, formatBytes(memory.Available)),
		Metrics: map[string]interface{}{"total_bytes": memory.Total, "available_bytes": memory.Available, "used_bytes": used, "used_percent": percent},
	}
}

// snapshotPressureResult는 PSI와 OOM counter 중 읽은 것만 metric으로 남긴다. 둘 다 없으면(macOS, 오래된 kernel)
// 0으로 채우지 않고 skip한다. 0은 "압박 없음"으로 읽히기 때문이다.
func snapshotPressureResult(started time.Time, signals infoSignals) Result {
	hasPressure, hasOOM := len(signals.Pressure) == 3, signals.OOMKills != nil
	if !hasPressure && !hasOOM {
		return unsupported("info.pressure", T("snapshot.skip.pressure_unavailable"))
	}
	result := Result{Probe: "info.pressure", Status: StatusPass, StartedAt: started.UTC(), Metrics: map[string]interface{}{}}
	var parts []string
	if hasPressure {
		result.Metrics["psi_cpu_some_avg10"] = signals.Pressure[0]
		result.Metrics["psi_memory_some_avg10"] = signals.Pressure[1]
		result.Metrics["psi_io_some_avg10"] = signals.Pressure[2]
		parts = append(parts, T("snapshot.summary.psi", signals.Pressure[0], signals.Pressure[1], signals.Pressure[2]))
		for _, value := range signals.Pressure {
			if value >= snapshotPressureWarn {
				result.Status = StatusWarn
			}
		}
	} else {
		result.Warnings = append(result.Warnings, T("snapshot.warning.psi_unavailable"))
	}
	if hasOOM {
		result.Metrics["oom_kills"] = *signals.OOMKills
		parts = append(parts, T("snapshot.summary.oom", *signals.OOMKills))
		if *signals.OOMKills > 0 {
			result.Status = StatusWarn
		}
	} else {
		result.Warnings = append(result.Warnings, T("snapshot.warning.oom_unavailable"))
	}
	result.Summary = strings.Join(parts, " · ")
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// snapshotDiskResults는 mount 경로마다 Result를 하나 만든다. df가 같은 mount를 두 번 내면 나중 줄이 이긴다.
// 나중에 붙은 mount가 앞의 것을 덮기 때문이다. probe id가 겹치면 report diff가 한쪽을 잃는다.
// disks는 caller가 visibleDisks로 거른 목록이다.
func snapshotDiskResults(started time.Time, disks []diskDetails, signals map[string]diskSignal, err error) []Result {
	if err != nil {
		return []Result{resultFromError("info.disk", started, "snapshot", err)}
	}
	latest := map[string]diskDetails{}
	for _, disk := range disks {
		latest[disk.Mount] = disk
	}
	mounts := make([]string, 0, len(latest))
	for mount := range latest {
		mounts = append(mounts, mount)
	}
	sort.Strings(mounts)
	results := make([]Result, 0, len(mounts))
	for _, mount := range mounts {
		disk, signal := latest[mount], signals[mount]
		result := Result{
			Probe: "info.disk:" + mount, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
			Summary: T("snapshot.summary.disk", mount, disk.Device, formatBytes(disk.Used), formatBytes(disk.Total), disk.Percent),
			Metrics: map[string]interface{}{
				"device": disk.Device, "used_bytes": disk.Used, "total_bytes": disk.Total, "used_percent": disk.Percent, "read_only": signal.ReadOnly,
			},
		}
		if disk.Percent >= diskLimits.warn {
			result.Status = StatusWarn
		}
		if signal.ReadOnly {
			result.Status = StatusWarn
			result.Warnings = append(result.Warnings, T("snapshot.warning.read_only", mount))
		}
		if signal.InodesTotal > 0 {
			inodes := float64(signal.InodesUsed) / float64(signal.InodesTotal) * 100
			result.Metrics["inodes_used_percent"] = inodes
			if inodes >= infoInodeWarnPercent {
				result.Status = StatusWarn
				result.Warnings = append(result.Warnings, T("snapshot.warning.inodes", mount, inodes))
			}
		}
		if deleted := signal.DeletedOpen; deleted.Files > 0 || deleted.Partial {
			result.Metrics["deleted_open_files"] = deleted.Files
			result.Metrics["deleted_open_bytes"] = deleted.Bytes
			result.Metrics["deleted_open_partial"] = deleted.Partial
			if disk.Total > 0 && deleted.Bytes >= disk.Total/100*infoDeletedOpenWarnPercent {
				result.Status = StatusWarn
				result.Warnings = append(result.Warnings, T("snapshot.warning.deleted_open", mount, deletedOpenText(deleted)))
			}
			if len(deleted.Top) > 0 {
				result.Evidence = append(result.Evidence, Evidence{Label: T("snapshot.evidence.deleted_open"), Value: strings.Join(deletedOpenLines(deleted), "\n")})
			}
		}
		results = append(results, result)
	}
	return results
}

func snapshotProcessResult(started time.Time, processes []topProcess, err error) Result {
	if err != nil {
		return resultFromError("info.processes", started, "snapshot", err)
	}
	threads := 0
	for _, process := range processes {
		threads += process.Threads
	}
	ordered := append([]topProcess(nil), processes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RSS > ordered[j].RSS })
	lines := make([]string, 0, snapshotTopProcesses)
	for _, process := range ordered[:min(snapshotTopProcesses, len(ordered))] {
		lines = append(lines, fmt.Sprintf("PID %-7d %-28s %s", process.PID, topProcessName(process.Command, 28), formatProcessRSS(process.RSS)))
	}
	result := Result{
		Probe: "info.processes", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("snapshot.summary.processes", len(processes), threads),
		Metrics: map[string]interface{}{"count": len(processes), "threads": threads},
	}
	if len(lines) > 0 {
		result.Evidence = []Evidence{{Label: "top RSS", Value: strings.Join(lines, "\n")}}
	}
	return result
}

// errSnapshotExists는 --force 없이 이미 있는 경로를 만났다는 뜻이다. caller는 errors.Is로 구별한다.
var errSnapshotExists = errors.New("snapshot path exists")

// checkSnapshotPath는 수집 전에 경로를 본다. 디렉터리는 --force로도 바꿀 수 없고, 없는 디렉터리 아래에는 쓸 수 없다.
func checkSnapshotPath(path string, force bool) error {
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return errors.New(T("snapshot.is_dir", path))
		}
		if !force {
			return snapshotExistsError{path: path}
		}
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New(T("snapshot.not_dir", filepath.Dir(path)))
	}
	return nil
}

type snapshotExistsError struct{ path string }

func (err snapshotExistsError) Error() string        { return T("snapshot.exists", err.path) }
func (err snapshotExistsError) Is(target error) bool { return target == errSnapshotExists }

// writeSnapshotFile은 report를 mode 0600으로 쓴다. snapshot에는 계정, sudo 설정, process 이름이 들어가므로
// 다른 사용자가 읽을 수 없어야 한다. --force 없이는 O_EXCL로 만들어 기존 파일과 symlink를 덮지 않는다.
// --force는 같은 디렉터리의 임시 파일을 경로 위로 rename하므로 symlink의 대상이 아니라 symlink 자체를 바꾼다.
func writeSnapshotFile(path string, report Report, force bool) error {
	if force {
		return replaceSnapshotFile(path, report)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return snapshotExistsError{path: path}
	}
	if err != nil {
		return err
	}
	if err := writeJSON(file, report); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func replaceSnapshotFile(path string, report Report) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	if err := writeJSON(file, report); err != nil {
		file.Close()
		os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

// snapshotDefaultPath는 현재 디렉터리의 파일 이름을 만든다. --redact는 host 이름을 가리므로 파일 이름에도 넣지 않는다.
func snapshotDefaultPath(hostname string, started time.Time, redact bool) string {
	stamp := started.UTC().Format("20060102T150405Z")
	host := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, hostname)
	if redact || host == "" {
		return "edc-snapshot-" + stamp + ".json"
	}
	return "edc-snapshot-" + host + "-" + stamp + ".json"
}
