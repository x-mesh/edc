package edc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// edc clean --group은 edc remote와 같은 인벤토리와 OpenSSH 설정으로 그룹의 호스트마다 edc clean --json -을 실행하고 결과를
// 표 하나로 모은다. clean은 읽기만 하는 명령이라 원격에서도 바꾸는 것이 없다. 원격에 edc를 설치하지는 않는다.

const (
	cleanGroupProbeID = "clean.group"
	// clean의 JSON은 후보와 묶음 수에 따라 수십 KB가 된다. edc remote의 출력 상한(64 KiB)으로는 잘리므로 따로 둔다.
	cleanGroupOutputLimit = 16 << 20
	// 원격 clean의 제한 시간 위에 SSH 연결과 셸 준비에 드는 여유를 둔다.
	cleanGroupSSHMargin = 30 * time.Second
)

const (
	cleanHostMeasured    = "measured"
	cleanHostMissing     = "edc_missing"
	cleanHostUnreachable = "unreachable"
	cleanHostTimeout     = "timeout"
	cleanHostInvalid     = "invalid_output"
	cleanHostFailed      = "failed"
)

// cleanRemoteRunner는 원격 명령 하나를 실행한다. 시험이 SSH 대신 가짜 실행기를 넣는다.
type cleanRemoteRunner func(ctx context.Context, target, command string, timeout time.Duration) remoteCapture

type cleanHostRow struct {
	Host   string `json:"host"`
	Target string `json:"target"`
	Status string `json:"status"`
	// ResultStatus는 원격 clean이 낸 PASS, WARN, FAIL이다. 결과를 읽은 호스트에만 있다.
	ResultStatus string `json:"result_status,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Version      string `json:"edc_version,omitempty"`
	// Disk는 루트 파일 시스템이고, 루트가 목록에 없으면 후보가 가장 많은 파일 시스템이다.
	DiskMount      string  `json:"disk_mount,omitempty"`
	DiskUsedBytes  *uint64 `json:"disk_used_bytes,omitempty"`
	DiskSizeBytes  *uint64 `json:"disk_size_bytes,omitempty"`
	CandidateBytes *uint64 `json:"candidate_bytes,omitempty"`
	// OldDockerBytes와 LogBytes가 nil이면 원격 edc가 그 항목을 모르는 이전 버전이거나 Linux가 아닌 것이다.
	OldDockerBytes *uint64 `json:"old_docker_bytes,omitempty"`
	LogBytes       *uint64 `json:"log_bytes,omitempty"`
	LargestName    string  `json:"largest,omitempty"`
	LargestBytes   uint64  `json:"largest_bytes,omitempty"`
	DurationMS     int64   `json:"duration_ms"`
}

// cleanRemoteReport는 원격 clean JSON에서 표에 쓰는 필드만 읽는다.
type cleanRemoteReport struct {
	Tool struct {
		Version string `json:"version"`
	} `json:"tool"`
	Results []struct {
		Status  string `json:"status"`
		Metrics struct {
			Candidates []struct {
				ID          string  `json:"id"`
				Status      string  `json:"status"`
				UsageBytes  *uint64 `json:"usage_bytes"`
				Regenerable bool    `json:"regenerable"`
			} `json:"candidates"`
			Filesystems []struct {
				Mount      string `json:"mount"`
				SizeBytes  uint64 `json:"size_bytes"`
				UsedBytes  uint64 `json:"used_bytes"`
				UsageBytes uint64 `json:"usage_bytes"`
			} `json:"filesystems"`
			DockerRoot *struct {
				Roots []struct {
					Path       string  `json:"path"`
					UsageBytes *uint64 `json:"usage_bytes"`
				} `json:"roots"`
			} `json:"docker_root"`
			System *struct {
				Logs []struct {
					Bytes uint64 `json:"bytes"`
				} `json:"large_logs"`
				LogsOmittedBytes uint64 `json:"large_logs_omitted_bytes"`
			} `json:"system"`
		} `json:"metrics"`
	} `json:"results"`
}

// runCleanGroup은 그룹의 호스트에서 clean을 실행하고 표나 JSON으로 낸다.
func runCleanGroup(options commonOptions, group, inventoryPath, version string, run cleanRemoteRunner, stdout io.Writer) int {
	if inventoryPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		configDir, _ := os.UserConfigDir()
		path, found := discoverRemoteInventory(cwd, configDir)
		if !found {
			fmt.Fprintln(os.Stderr, remoteInventoryNotFound(cwd))
			return 2
		}
		inventoryPath = path
	}
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	hosts, err := hostsForRemoteGroup(inventory, group)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := cleanCollectHosts(ctx, hosts, remoteParallelForGroup(inventory, group, 0), options.timeout, run)
	result := cleanGroupResult(rows, started)
	target := map[string]interface{}{"group": group, "inventory": inventoryPath}
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, target, []Result{result}, options.redact))
	}
	file, _ := stdout.(*os.File)
	width := 0
	if file != nil && isTerminal(file) {
		width = terminalWidth()
	}
	var output strings.Builder
	output.WriteString(formatResultLine(result, false))
	output.WriteString(formatCleanGroup(rows, width))
	output.WriteString("\n" + T("clean.advisory") + "\n")
	text := output.String()
	if options.redact {
		text = redactIPAddresses(text)
	}
	fmt.Fprint(stdout, text)
	return exitCode([]Result{result})
}

// cleanCollectHosts는 호스트를 parallel개씩 동시에 처리하고 인벤토리 순서대로 돌려준다.
func cleanCollectHosts(ctx context.Context, hosts []remoteHost, parallel int, timeout time.Duration, run cleanRemoteRunner) []cleanHostRow {
	if parallel < 1 {
		parallel = 1
	}
	rows := make([]cleanHostRow, len(hosts))
	slots := make(chan struct{}, parallel)
	var running sync.WaitGroup
	command := fmt.Sprintf("edc clean --json - --timeout %s", timeout)
	for index, host := range hosts {
		running.Add(1)
		slots <- struct{}{}
		go func(index int, host remoteHost) {
			defer func() {
				<-slots
				running.Done()
			}()
			target := host.Target
			if target == "" {
				target = host.Name
			}
			capture := run(ctx, target, command, timeout+cleanGroupSSHMargin)
			row := cleanHostFromCapture(capture)
			row.Host, row.Target = host.Name, target
			rows[index] = row
		}(index, host)
	}
	running.Wait()
	return rows
}

// cleanHostFromCapture는 원격 결과 하나를 표의 행으로 바꾼다. 표준 출력이 clean JSON으로 읽히면 종료 코드와 상관없이 그 결과를
// 쓴다. 읽히지 않을 때만 종료 코드와 표준 오류로 실패 원인을 고른다.
func cleanHostFromCapture(capture remoteCapture) cleanHostRow {
	row := cleanHostRow{DurationMS: capture.Duration.Milliseconds()}
	var report cleanRemoteReport
	if !capture.Truncated && json.Unmarshal([]byte(capture.Stdout), &report) == nil && len(report.Results) > 0 {
		cleanFillHostRow(&row, report)
		return row
	}
	detail := cleanFirstLine(capture.Stderr)
	switch {
	case capture.TimedOut:
		row.Status = cleanHostTimeout
	case capture.ExitCode == 255:
		row.Status, row.Detail = cleanHostUnreachable, detail
	case capture.ExitCode == 127 || strings.Contains(capture.Stderr, "edc: command not found") || strings.Contains(capture.Stderr, "edc: not found"):
		row.Status = cleanHostMissing
	case capture.Truncated:
		row.Status, row.Detail = cleanHostInvalid, T("clean.group.truncated", formatSizeShort(cleanGroupOutputLimit))
	case capture.Err == nil:
		row.Status, row.Detail = cleanHostInvalid, cleanFirstLine(capture.Stdout)
	default:
		row.Status, row.Detail = cleanHostFailed, detail
		if row.Detail == "" {
			row.Detail = fmt.Sprintf("exit %d", capture.ExitCode)
		}
	}
	return row
}

func cleanFillHostRow(row *cleanHostRow, report cleanRemoteReport) {
	row.Status, row.Version = cleanHostMeasured, report.Tool.Version
	result := report.Results[0]
	row.ResultStatus = result.Status
	metrics := result.Metrics
	var candidates uint64
	for _, candidate := range metrics.Candidates {
		if candidate.UsageBytes == nil {
			continue
		}
		candidates += *candidate.UsageBytes
		if *candidate.UsageBytes > row.LargestBytes {
			row.LargestName, row.LargestBytes = T("clean.candidate."+candidate.ID), *candidate.UsageBytes
		}
	}
	row.CandidateBytes = &candidates
	best := -1
	for index, filesystem := range metrics.Filesystems {
		if filesystem.Mount == "/" {
			best = index
			break
		}
		if best < 0 || filesystem.UsageBytes > metrics.Filesystems[best].UsageBytes {
			best = index
		}
	}
	if best >= 0 {
		filesystem := metrics.Filesystems[best]
		used, size := filesystem.UsedBytes, filesystem.SizeBytes
		row.DiskMount, row.DiskUsedBytes, row.DiskSizeBytes = filesystem.Mount, &used, &size
	}
	if metrics.DockerRoot != nil {
		var old uint64
		for _, root := range metrics.DockerRoot.Roots {
			if root.UsageBytes == nil {
				continue
			}
			old += *root.UsageBytes
			if *root.UsageBytes > row.LargestBytes {
				row.LargestName, row.LargestBytes = root.Path, *root.UsageBytes
			}
		}
		row.OldDockerBytes = &old
	}
	if metrics.System != nil {
		logs := metrics.System.LogsOmittedBytes
		for _, log := range metrics.System.Logs {
			logs += log.Bytes
		}
		row.LogBytes = &logs
	}
}

func cleanGroupResult(rows []cleanHostRow, started time.Time) Result {
	measured := 0
	for _, row := range rows {
		if row.Status == cleanHostMeasured {
			measured++
		}
	}
	result := Result{Probe: cleanGroupProbeID, StartedAt: started.UTC(), Status: StatusPass,
		Summary: T("clean.group.summary", len(rows), measured, len(rows)-measured), DurationMS: time.Since(started).Milliseconds(),
		Metrics: map[string]interface{}{"hosts": rows}}
	if measured < len(rows) {
		result.Status = StatusWarn
	}
	return result
}

func formatCleanGroup(rows []cleanHostRow, width int) string {
	size := func(value *uint64) string {
		if value == nil {
			return "—"
		}
		return formatSizeShort(*value)
	}
	header := []string{T("clean.group.col.host"), T("clean.group.col.disk"), T("clean.group.col.candidates"), T("clean.group.col.old_docker"),
		T("clean.group.col.logs"), T("clean.group.col.largest")}
	table := [][]string{header}
	for _, row := range rows {
		if row.Status != cleanHostMeasured {
			state := T("clean.group.status." + row.Status)
			if row.Detail != "" {
				state += "  " + row.Detail
			}
			table = append(table, []string{row.Host, state})
			continue
		}
		disk := "—"
		if row.DiskUsedBytes != nil && row.DiskSizeBytes != nil {
			disk = formatSizeShort(*row.DiskUsedBytes) + " / " + formatSizeShort(*row.DiskSizeBytes)
		}
		largest := "—"
		if row.LargestName != "" {
			largest = row.LargestName + " " + formatSizeShort(row.LargestBytes)
		}
		table = append(table, []string{row.Host, disk, size(row.CandidateBytes), size(row.OldDockerBytes), size(row.LogBytes), largest})
	}
	widths := make([]int, len(header))
	for _, cells := range table {
		// 실패한 호스트의 행은 이름 뒤 칸 하나에 상태를 쓰므로 너비 계산에서 뺀다.
		if len(cells) != len(header) {
			widths[0] = max(widths[0], liveWidth(cells[0]))
			continue
		}
		for index, cell := range cells {
			widths[index] = max(widths[index], liveWidth(cell))
		}
	}
	var builder strings.Builder
	builder.WriteString("\n")
	for _, cells := range table {
		parts := make([]string, len(cells))
		for index, cell := range cells {
			if index == len(cells)-1 {
				parts[index] = cell
				continue
			}
			parts[index] = cleanPad(cell, widths[index])
		}
		builder.WriteString(duFit(strings.Join(parts, "  "), width) + "\n")
	}
	var versions []string
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Version != "" && !seen[row.Version] {
			seen[row.Version] = true
			versions = append(versions, row.Version)
		}
	}
	sort.Strings(versions)
	if len(versions) > 1 {
		builder.WriteString(duFit(T("clean.group.versions", strings.Join(versions, ", ")), width) + "\n")
	}
	return builder.String()
}
