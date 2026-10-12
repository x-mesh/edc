package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// snapshotGrace는 budget이 끝난 뒤 ctx를 보는 section이 모은 결과를 돌려줄 여유다. 이보다 늦은 section은
	// hung NFS 위의 df처럼 ctx를 보지 않는 것으로 보고 더 기다리지 않는다.
	snapshotGrace = 2 * time.Second
	// snapshotTopProcesses는 Evidence에 남기는 RSS 상위 process 수다. report 크기 제한을 넘지 않게 한다.
	snapshotTopProcesses = 5
	// snapshotPressureWarn은 PSI some avg10의 경고 기준(%)이다. 10초 중 1초 넘게 기다렸다는 뜻이다.
	snapshotPressureWarn = 10
)

type snapshotSection struct {
	name string
	run  func(context.Context) []Result
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
		timedOut := func() Result { return unsupported(name, T("snapshot.skip.timeout", name, budget())) }
		if expired {
			select {
			case section := <-channel:
				results = append(results, section...)
			default:
				results = append(results, timedOut())
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
			results = append(results, timedOut())
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
