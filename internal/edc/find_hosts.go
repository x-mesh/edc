package edc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// edc find --group은 edc remote와 같은 인벤토리와 OpenSSH 설정으로 그룹의 호스트마다 edc find --json -을 실행하고 결과를
// host:path로 모은다. 원격 호스트의 find(1)로 대신하지 않는다. 일치 방식과 기본값이 달라 결과를 섞을 수 없다.

const (
	// findGroupOutputLimit는 원격 JSON 한 개의 상한이다. 일치가 많으면 edc remote의 64 KiB 상한을 넘는다.
	findGroupOutputLimit = 16 << 20
	// findGroupSSHMargin은 원격 find의 제한 시간 위에 SSH 연결과 셸 준비에 드는 여유다.
	findGroupSSHMargin = 30 * time.Second
)

const (
	findHostFound       = "found"
	findHostNone        = "none"
	findHostMissing     = "edc_missing"
	findHostTooOld      = "edc_too_old"
	findHostUnreachable = "unreachable"
	findHostTimeout     = "timeout"
	findHostInvalid     = "invalid_output"
	findHostFailed      = "failed"
)

type findHostRow struct {
	Host       string          `json:"host"`
	Target     string          `json:"target"`
	Status     string          `json:"status"`
	Version    string          `json:"edc_version,omitempty"`
	Detail     string          `json:"detail,omitempty"`
	Matches    []findJSONMatch `json:"matches"`
	Partial    bool            `json:"partial,omitempty"`
	Stalled    []string        `json:"stalled,omitempty"`
	DurationMS int64           `json:"duration_ms"`
}

// findRemoteCommand는 원격에서 실행할 명령이다. 값은 원격 셸이 다르게 읽지 않도록 따옴표로 감싼다.
func findRemoteCommand(options findOptions) string {
	words := []string{"edc", "find", "--json", "-", "--timeout", options.common.timeout.String()}
	for _, kind := range []findKind{findKindFile, findKindDir, findKindLink} {
		if options.types[kind] {
			words = append(words, "--type", string(kind))
		}
	}
	if options.sizeText != "" {
		words = append(words, "--size", options.sizeText)
	}
	if options.changedText != "" {
		words = append(words, "--changed", options.changedText)
	}
	for _, flag := range []struct {
		name string
		on   bool
	}{{"-E", options.regex}, {"--no-ignore", options.noIgnore}, {"--cross-mounts", options.crossMounts}, {"--layers", options.layers}} {
		if flag.on {
			words = append(words, flag.name)
		}
	}
	if options.workers != findDefaultWorkers {
		words = append(words, "--workers", fmt.Sprint(options.workers))
	}
	words = append(words, "--", options.pattern)
	words = append(words, options.paths...)
	for index, word := range words {
		words[index] = quoteLocalShellWord(word)
	}
	return strings.Join(words, " ")
}

func findSSHRunner(ctx context.Context, target, command string, timeout time.Duration) remoteCapture {
	runner := sshRemoteRunner{connectTimeout: configuredDuration(activeConfig.Defaults.Remote.ConnectTimeout, 10*time.Second)}
	return runner.Capture(ctx, target, command, timeout, findGroupOutputLimit)
}

// findRemote는 원격 실행기다. 시험이 SSH 대신 가짜 실행기를 넣는다.
var findRemote cleanRemoteRunner = findSSHRunner

func runFindGroup(options findOptions, env findEnv, run cleanRemoteRunner) int {
	// 원격의 .은 SSH 사용자의 홈이라 이 컴퓨터의 .과 뜻이 다르다. 찾을 곳을 직접 받는다.
	if options.pattern == "" || !options.explicit {
		fmt.Fprintln(env.stderr, T("find.error.group_args"))
		return 2
	}
	inventoryPath := options.inventory
	if inventoryPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(env.stderr, err)
			return 2
		}
		configDir, _ := os.UserConfigDir()
		path, found := discoverRemoteInventory(cwd, configDir)
		if !found {
			fmt.Fprintln(env.stderr, remoteInventoryNotFound(cwd))
			return 2
		}
		inventoryPath = path
	}
	inventory, err := loadRemoteInventory(inventoryPath)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 2
	}
	hosts, err := hostsForRemoteGroup(inventory, options.group)
	if err != nil {
		fmt.Fprintln(env.stderr, err)
		return 2
	}
	started := env.now()
	rows := findCollectHosts(context.Background(), hosts, remoteParallelForGroup(inventory, options.group, 0), options, run)
	if options.common.jsonPath != "" {
		out := map[string]interface{}{
			"schema_version": "1.0", "tool": map[string]string{"name": "edc", "version": env.version}, "group": options.group,
			"inventory": inventoryPath, "pattern": options.pattern, "paths": options.paths, "hosts": rows,
			"duration_ms": env.now().Sub(started).Milliseconds(),
		}
		data, err := json.Marshal(out)
		if err == nil && options.common.redact {
			data = []byte(redactIPAddresses(string(data)))
		}
		if err == nil {
			err = writeJSONOutput(options.common.jsonPath, json.RawMessage(data))
		}
		if err != nil {
			fmt.Fprintln(env.stderr, err)
			return 2
		}
		return findGroupCode(rows)
	}
	if err := writeFindGroup(env.stdout, env.stderr, rows, options.common.verbose); err != nil {
		fmt.Fprintln(env.stderr, "edc find: "+err.Error())
		return 2
	}
	return findGroupCode(rows)
}

// findGroupCode는 한 호스트에서라도 찾으면 0, 찾은 호스트가 없으면 1, 검색한 호스트가 하나도 없으면 2다.
func findGroupCode(rows []findHostRow) int {
	searched := false
	for _, row := range rows {
		switch row.Status {
		case findHostFound:
			return 0
		case findHostNone:
			searched = true
		}
	}
	if searched {
		return 1
	}
	return 2
}

// findCollectHosts는 호스트를 parallel개씩 동시에 처리하고 인벤토리 순서대로 돌려준다.
func findCollectHosts(ctx context.Context, hosts []remoteHost, parallel int, options findOptions, run cleanRemoteRunner) []findHostRow {
	parallel = max(1, parallel)
	rows := make([]findHostRow, len(hosts))
	slots := make(chan struct{}, parallel)
	var running sync.WaitGroup
	command := findRemoteCommand(options)
	for index, host := range hosts {
		running.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() {
				<-slots
				running.Done()
			}()
			target := host.Target
			if target == "" {
				target = host.Name
			}
			row := findHostFromCapture(run(ctx, target, command, options.common.timeout+findGroupSSHMargin))
			row.Host, row.Target = host.Name, target
			rows[index] = row
		}()
	}
	running.Wait()
	return rows
}

// findHostFromCapture는 원격 결과 하나를 행으로 바꾼다. 표준 출력이 find JSON으로 읽히면 종료 코드와 상관없이 그 결과를 쓴다.
// 일치가 없으면 원격 edc는 1로 끝나므로 종료 코드만 보고 실패로 볼 수 없다.
func findHostFromCapture(capture remoteCapture) findHostRow {
	row := findHostRow{DurationMS: capture.Duration.Milliseconds(), Matches: []findJSONMatch{}}
	var report findJSONReport
	if !capture.Truncated && json.Unmarshal([]byte(capture.Stdout), &report) == nil && report.SchemaVersion != "" {
		row.Version, row.Partial, row.Stalled = report.Tool["version"], report.Partial, report.Stalled
		row.Matches = append(row.Matches, report.Matches...)
		row.Status = findHostNone
		if len(row.Matches) > 0 {
			row.Status = findHostFound
		}
		if len(report.Problems) > 0 {
			row.Detail = report.Problems[0]
		}
		// 원격 edc는 일치가 없으면 1, 찾을 곳을 하나도 읽지 못하면 2로 끝난다. 2는 검색하지 못한 것이다.
		if capture.ExitCode == 2 && len(row.Matches) == 0 {
			row.Status = findHostFailed
			if row.Detail == "" {
				row.Detail = "exit 2"
			}
		}
		return row
	}
	detail := cleanFirstLine(capture.Stderr)
	switch {
	case capture.TimedOut:
		row.Status = findHostTimeout
	case capture.ExitCode == 255:
		row.Status, row.Detail = findHostUnreachable, detail
	case capture.ExitCode == 127 || strings.Contains(capture.Stderr, "edc: command not found") || strings.Contains(capture.Stderr, "edc: not found"):
		row.Status = findHostMissing
	// find가 없는 edc는 "unknown command: find"를 낸다. 언어마다 앞부분만 다르고 "command: find"는 같다.
	case capture.ExitCode == 2 && strings.Contains(capture.Stderr, "command: find"):
		row.Status, row.Detail = findHostTooOld, T("find.group.too_old")
	case capture.Truncated:
		row.Status, row.Detail = findHostInvalid, T("find.group.truncated", formatSizeShort(findGroupOutputLimit))
	case capture.Err == nil:
		row.Status, row.Detail = findHostInvalid, cleanFirstLine(capture.Stdout)
	default:
		row.Status, row.Detail = findHostFailed, detail
		if row.Detail == "" {
			row.Detail = fmt.Sprintf("exit %d", capture.ExitCode)
		}
	}
	return row
}

// writeFindGroup은 일치를 host:path로 stdout에 쓰고, 찾지 못한 호스트의 이유를 stderr에 쓴다. stdout만 파이프로 넘길 수 있다.
// stdout에 쓰지 못하면 오류를 돌려준다. 일부만 쓴 목록을 성공으로 끝내지 않는다.
func writeFindGroup(stdout, stderr io.Writer, rows []findHostRow, verbose bool) error {
	found := 0
	writer := bufio.NewWriter(stdout)
	for _, row := range rows {
		for _, match := range row.Matches {
			fmt.Fprintf(writer, "%s:%s\n", row.Host, match.Path)
		}
		if row.Status == findHostFound {
			found++
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	for _, row := range rows {
		line := ""
		switch row.Status {
		case findHostFound:
			if row.Partial {
				line = T("find.group.partial")
			}
		case findHostNone:
			if verbose || row.Partial {
				line = T("find.group.none")
				if row.Partial {
					line += ", " + T("find.group.partial")
				}
			}
		default:
			line = T("find.group.status." + row.Status)
			if row.Detail != "" {
				line += ": " + viewSanitize(row.Detail)
			}
		}
		if line != "" {
			fmt.Fprintf(stderr, "%s  %s\n", row.Host, line)
		}
	}
	fmt.Fprintln(stderr, T("find.group.summary", found, len(rows)))
	return nil
}
