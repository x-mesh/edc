package edc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// macOS의 svc는 launchd job을 본다. systemd의 unit·cgroup과 모델이 달라 Linux 화면을 쓰지 않고, launchctl이 알려 주는
// 상태와 마지막 종료, process, 그리고 job이 적어 둔 log 파일을 보인다.

func init() {
	for _, id := range []string{"list", "state", "process", "log"} {
		evidenceShownProbes["svc."+id] = true
	}
}

const (
	launchdTool = "/bin/launchctl"
	// launchdLogLines는 log 파일 끝에서 읽는 줄 수이고 launchdLogTail은 그만큼 찾으려고 읽는 byte 수다.
	launchdLogLines = 20
	launchdLogTail  = 64 << 10
)

// launchdItem은 launchctl list의 한 줄이다. PID가 0이면 돌고 있지 않다. Status는 마지막 종료 상태이고, 음수는 신호다.
type launchdItem struct {
	Label  string
	PID    int
	Status int
}

func (item launchdItem) failed() bool { return item.PID == 0 && item.Status > 0 }

func parseLaunchdList(output string) []launchdItem {
	var items []launchdItem
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "PID" {
			continue
		}
		item := launchdItem{Label: strings.Join(fields[2:], " ")}
		item.PID, _ = strconv.Atoi(fields[0])
		item.Status, _ = strconv.Atoi(fields[1])
		items = append(items, item)
	}
	return items
}

// launchdListResult는 목록 하나다. 기본은 Apple의 job, 열린 GUI 앱(application.*), 돌지 않고 0으로 끝난 job을 뺀다.
// 모두 합쳐 수백 개라 문제를 가린다. 실패한 job은 Apple 것이라도 늘 보인다.
func launchdListResult(items []launchdItem, all bool) Result {
	var shown []launchdItem
	var failed []string
	running := 0
	for _, item := range items {
		if item.failed() {
			failed = append(failed, fmt.Sprintf("%s (exit %d)", item.Label, item.Status))
		}
		if item.PID > 0 {
			running++
		}
		hidden := strings.HasPrefix(item.Label, "com.apple.") || strings.HasPrefix(item.Label, "application.")
		if all || item.failed() || (!hidden && item.PID > 0) {
			shown = append(shown, item)
		}
	}
	slices.SortStableFunc(shown, func(a, b launchdItem) int {
		if a.failed() != b.failed() {
			if a.failed() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Label, b.Label)
	})
	result := svcResult("list", StatusPass, T("svc.launchd.list", len(items), running, len(failed)))
	if len(failed) > 0 {
		result.Status = StatusFail
		result.Error = &DiagnosticError{Kind: "state", Message: strings.Join(failed[:min(len(failed), svcListFailedShown)], ", ")}
		if hidden := len(failed) - svcListFailedShown; hidden > 0 {
			result.Error.Message += fmt.Sprintf(" +%d", hidden)
		}
		result.Next = "edc svc " + strings.Fields(failed[0])[0]
	}
	lines := []string{fmt.Sprintf("%7s  %6s  %s", "PID", "EXIT", "LABEL")}
	for _, item := range shown {
		pid := "-"
		if item.PID > 0 {
			pid = strconv.Itoa(item.PID)
		}
		lines = append(lines, fmt.Sprintf("%7s  %6d  %s", pid, item.Status, item.Label))
	}
	if len(shown) > 0 {
		result.Evidence = []Evidence{{Label: T("svc.launchd.jobs", len(shown)), Value: strings.Join(lines, "\n")}}
	}
	return result
}

// launchdJobInfo는 launchctl print의 맨 위 칸이다. 들여쓴 하위 블록(environment, endpoints)은 읽지 않는다.
type launchdJobInfo struct {
	Domain, Path, Type, State, Program, ExitCode, ExitReason string
	StdoutPath, StderrPath                                   string
	PID, Runs                                                int
	Arguments                                                []string
}

// parseLaunchdPrint는 launchctl print의 출력을 읽는다. 형식은 사람이 읽으라고 만든 것이라 맨 위 칸의 "key = value"만 믿는다.
func parseLaunchdPrint(output string) launchdJobInfo {
	var info launchdJobInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	inArguments := false
	for scanner.Scan() {
		line := scanner.Text()
		if inArguments {
			if strings.TrimSpace(line) == "}" {
				inArguments = false
				continue
			}
			info.Arguments = append(info.Arguments, strings.TrimSpace(line))
			continue
		}
		// 맨 위 칸은 탭 하나로 들여쓴다. 더 깊은 줄은 하위 블록이다.
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "arguments":
			inArguments = value == "{"
		case "path":
			info.Path = value
		case "type":
			info.Type = value
		case "state":
			info.State = value
		case "program":
			info.Program = value
		case "domain":
			info.Domain, _, _ = strings.Cut(value, " ")
		case "pid":
			info.PID, _ = strconv.Atoi(value)
		case "runs":
			info.Runs, _ = strconv.Atoi(value)
		case "last exit code":
			info.ExitCode = value
		case "last exit reason":
			info.ExitReason = value
		case "stdout path":
			info.StdoutPath = value
		case "stderr path":
			info.StderrPath = value
		}
	}
	return info
}

// launchdExitCode는 "78: EX_CONFIG"의 숫자다. "(never exited)"처럼 숫자가 아니면 모른다.
func launchdExitCode(text string) (int, bool) {
	number, _, _ := strings.Cut(text, ":")
	code, err := strconv.Atoi(strings.TrimSpace(number))
	return code, err == nil
}

func launchdStateResult(label string, info launchdJobInfo) Result {
	parts := []string{info.State}
	if info.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", info.PID))
	}
	if info.Runs > 0 {
		parts = append(parts, T("svc.launchd.runs", info.Runs))
	}
	status := StatusPass
	if info.ExitCode != "" {
		parts = append(parts, T("svc.launchd.last_exit", info.ExitCode))
		// 지금 돌고 있으면 마지막 종료는 지난 일이다. 돌지 않으면서 양수로 끝났으면 job이 실패한 채 멈춘 것이다.
		if code, ok := launchdExitCode(info.ExitCode); ok && code > 0 && info.PID == 0 {
			status = StatusFail
		}
	}
	if info.ExitReason != "" {
		parts = append(parts, info.ExitReason)
	}
	result := svcResult("state", status, strings.Join(parts, " · "))
	var lines []string
	for _, field := range []struct{ name, value string }{
		{"domain", info.Domain}, {"type", info.Type}, {"plist", info.Path}, {"program", info.Program},
		{"arguments", strings.Join(listenMaskArgs(info.Arguments), " ")}, {"stdout", info.StdoutPath}, {"stderr", info.StderrPath},
	} {
		if field.value != "" {
			lines = append(lines, fmt.Sprintf("%-9s %s", field.name, field.value))
		}
	}
	result.Evidence = []Evidence{{Label: label, Value: strings.Join(lines, "\n")}}
	if status == StatusFail {
		result.Error = &DiagnosticError{Kind: "state", Message: result.Summary}
		result.Next = "launchctl print " + info.Domain + "/" + label
	}
	return result
}

// launchdProcessResult는 돌고 있는 job의 process 하나를 proc과 같은 원천으로 읽는다.
func launchdProcessResult(info launchdJobInfo, process topProcess, found bool, details procDetails) Result {
	if info.PID == 0 {
		return svcResult("process", StatusSkip, T("svc.launchd.not_running"))
	}
	if !found {
		return svcResult("process", StatusSkip, T("proc.error.not_found", strconv.Itoa(info.PID)))
	}
	parts := []string{T("proc.resources", process.CPU, traceBytes(process.RSS))}
	if details.FDErr == nil {
		total := 0
		for _, count := range details.FDTypes {
			total += count
		}
		parts = append(parts, T("proc.fd.count", total))
	}
	if details.SocketErr == nil && len(details.Listen) > 0 {
		parts = append(parts, T("svc.launchd.listening", strings.Join(slices.Compact(slices.Sorted(slices.Values(details.Listen))), ", ")))
	}
	result := svcResult("process", StatusPass, strings.Join(parts, " · "))
	result.Next = fmt.Sprintf("edc proc %d", info.PID)
	return result
}

// launchdLogResult는 job이 적어 둔 stderr, 없으면 stdout 파일의 끝이다. launchd는 log를 따로 모으지 않으므로 이 경로가
// 없으면 unified log에서 찾아야 한다.
func launchdLogResult(info launchdJobInfo, read func(path string) ([]string, error)) Result {
	path := firstNonEmpty(info.StderrPath, info.StdoutPath)
	if path == "" || path == "/dev/null" {
		result := svcResult("log", StatusSkip, T("svc.launchd.no_log_path"))
		if name := procName(info.Program); name != "" && name != "." {
			result.Next = fmt.Sprintf("log show --last 1h --predicate 'process == %q'", name)
		}
		return result
	}
	lines, err := read(path)
	switch {
	case errors.Is(err, os.ErrPermission):
		return svcResult("log", StatusSkip, T("svc.skip.needs_root", path))
	case errors.Is(err, os.ErrNotExist):
		return svcResult("log", StatusSkip, T("svc.launchd.log_missing", path))
	case err != nil:
		return svcResult("log", StatusSkip, T("svc.skip.unreadable", path, infoSingleLine(err.Error())))
	case len(lines) == 0:
		return svcResult("log", StatusPass, T("svc.launchd.log_empty", path))
	}
	result := svcResult("log", StatusPass, T("svc.launchd.log", len(lines), path))
	result.Evidence = []Evidence{{Label: path, Value: strings.Join(lines, "\n")}}
	return result
}

// tailLogFile은 파일 끝 launchdLogTail byte에서 마지막 launchdLogLines 줄을 읽는다. 큰 log를 처음부터 읽지 않는다.
func tailLogFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - launchdLogTail
	if offset < 0 {
		offset = 0
	}
	buffer := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buffer, offset); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(buffer), "\n"), "\n")
	if offset > 0 && len(lines) > 1 {
		// 잘라 읽은 첫 줄은 중간에서 시작한다.
		lines = lines[1:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	lines = lines[max(len(lines)-launchdLogLines, 0):]
	for index, line := range lines {
		lines[index] = svcTruncate(infoSingleLine(line), procCommandMax)
	}
	return lines, nil
}

// launchdDomains는 job을 찾는 순서다. 사용자로 실행하면 그 사용자의 agent를, root로 실행하면 system daemon을 먼저 본다.
func launchdDomains(uid int) []string {
	user := []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)}
	if uid == 0 {
		return append([]string{"system"}, user...)
	}
	return append(user, "system")
}

// validLaunchdLabel은 launchctl에 넘길 이름이다. '-'로 시작하면 option이 되고 '/'는 domain 구분자라 막는다.
func validLaunchdLabel(label string) bool {
	return label != "" && !strings.HasPrefix(label, "-") && !strings.ContainsAny(label, "/ \t\n")
}

func runLaunchctl(ctx context.Context, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, launchdTool, args...).CombinedOutput()
	return string(output), err
}

func runSvcLaunchd(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("svc")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	all := set.Bool("all", false, T("command.svc.option.all"))
	if err := parseReordered(set, args); err != nil {
		return 2
	}
	usage := T("cli.usage", "edc svc [options] [--all] | edc svc [options] <label>")
	if set.NArg() > 1 || (set.NArg() == 1 && *all) {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	if set.NArg() == 0 {
		output, err := runLaunchctl(ctx, "list")
		result := launchdListResult(parseLaunchdList(output), *all)
		if err != nil {
			result = svcFailed(svcResult("list", StatusFail, T("svc.list.failed", infoSingleLine(err.Error()))))
		}
		return emit(options, buildReport(version, started, map[string]interface{}{"all": *all}, []Result{result}, options.redact))
	}
	label := set.Arg(0)
	if !validLaunchdLabel(label) {
		fmt.Fprintln(os.Stderr, T("svc.launchd.invalid_label", label))
		return 2
	}
	var info launchdJobInfo
	found := false
	for _, domain := range launchdDomains(os.Geteuid()) {
		if output, err := runLaunchctl(ctx, "print", domain+"/"+label); err == nil {
			info, found = parseLaunchdPrint(output), true
			if info.Domain == "" {
				info.Domain = domain
			}
			break
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, T("svc.launchd.not_found", label))
		return 1
	}
	var process topProcess
	var details procDetails
	processFound := false
	if info.PID > 0 {
		read := newTopProcessReader()
		read()
		time.Sleep(procSampleWindow)
		processes, _ := read()
		selected, _, err := selectProc(processes, strconv.Itoa(info.PID))
		if processFound = err == nil; processFound {
			process, details = selected, readProcDetails(selected.PID)
		}
	}
	results := []Result{
		launchdStateResult(label, info),
		launchdProcessResult(info, process, processFound, details),
		launchdLogResult(info, tailLogFile),
	}
	return emit(options, buildReport(version, started, map[string]interface{}{"label": label}, results, options.redact))
}
