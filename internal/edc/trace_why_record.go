package edc

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	traceWhyManifestName    = "why.json"
	traceWhyManifestVersion = 1
	traceWhyUsage           = "edc trace why [--duration <d>] [--process <name|pid>] [--port <n>] [--with <db[:port],...>] [--out <dir>]"
)

// why는 http, dns와 고른 DB trace를 자식 process로 함께 기록하고, 서버가 받은 요청의 지연을 하위 호출로 나눈다.
// runTraceWhy가 기록을 읽으며 registry를 보므로 registry의 초깃값에 넣으면 초기화가 순환한다.
func init() {
	registerTraceProtocol(traceProtocolRegistration{name: "why", view: traceProtocolView{selectorLabel: "why"}, spec: traceProtocolSpec{hideTraffic: true, linuxOnly: true, prerequisites: func() error { return traceWhyPrerequisites() }}, run: runTraceWhy})
}

// traceWhyDatabases는 --with가 받는 trace다. clickhouse는 event에 boot_time_ns가 있는지 확인하지 못해 넣지 않았다.
var traceWhyDatabases = []string{"mysql", "postgres", "redis"}

// traceWhyStopTimeout은 멈추라고 한 자식이 요약 줄을 쓰고 끝나기를 기다리는 시간이다. trace http는 TLS probe를 떼고 나서
// 요약을 쓴다. 2026-11에 TLS 파일 28개에 붙은 trace http가 SIGTERM 뒤 끝나는 데 30초가 걸렸다.
var traceWhyStopTimeout = 2 * time.Minute

// traceWhyTrace는 함께 기록할 trace 하나다. port는 --with의 :port이고 0이면 그 trace의 기본 port다.
type traceWhyTrace struct {
	name string
	port int
}

type traceWhyPlan struct {
	traces   []traceWhyTrace
	duration time.Duration
	process  string
	port     int
	out      string
}

type traceWhyManifestTrace struct {
	Name string `json:"name"`
	File string `json:"file"`
}

// traceWhyManifest는 기록 디렉터리의 why.json이다. trace show <dir>이 이 파일에 적힌 기록만 읽어 같은 분석을 다시 만든다.
type traceWhyManifest struct {
	Version int                     `json:"version"`
	Traces  []traceWhyManifestTrace `json:"traces"`
	Port    int                     `json:"port,omitempty"`
	Process string                  `json:"process,omitempty"`
}

// traceWhyChildError는 자식 trace 하나가 실패했다는 뜻이다. stderr는 그 자식이 남긴 파일이다.
type traceWhyChildError struct {
	name   string
	stderr string
	err    error
}

func (failure *traceWhyChildError) Error() string {
	return fmt.Sprintf("trace %s: %v", failure.name, failure.err)
}

// traceWhyPrerequisites와 traceWhyStart는 시험이 바꿔 끼운다. 시험은 root와 kernel 없이 가짜 자식으로 기록 흐름을 본다.
var traceWhyPrerequisites = func() error {
	if runtime.GOOS != "linux" {
		return errors.New(T("cli.trace.protocol_linux_only", "why", runtime.GOOS))
	}
	return httpTracePrerequisites()
}

var traceWhyStart = func(args []string, stdout, stderr *os.File) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = stdout, stderr
	return command, command.Start()
}

// parseTraceWhyWith는 "mysql:13306,redis"를 읽는다.
func parseTraceWhyWith(value string) ([]traceWhyTrace, error) {
	var traces []traceWhyTrace
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	for _, item := range strings.Split(value, ",") {
		name, port, hasPort := strings.Cut(strings.TrimSpace(item), ":")
		trace := traceWhyTrace{name: name}
		if !slices.Contains(traceWhyDatabases, name) {
			return nil, fmt.Errorf("%q is not one of %s", name, strings.Join(traceWhyDatabases, ", "))
		}
		if slices.ContainsFunc(traces, func(other traceWhyTrace) bool { return other.name == name }) {
			return nil, fmt.Errorf("%s is given twice", name)
		}
		if hasPort {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return nil, fmt.Errorf("%s: port %q is not between 1 and 65535", name, port)
			}
			trace.port = number
		}
		traces = append(traces, trace)
	}
	return traces, nil
}

// traceWhyChildArgs는 자식 trace의 명령줄이다. http에는 --port를 넘기지 않는다. 넘기면 그 port를 쓰지 않는 연결, 곧 backend가
// 밖으로 보내는 요청이 기록에서 빠진다. port는 분석이 서버 요청을 고를 때만 쓴다. http는 payload 없이 기록해 header와 body가
// 파일에 남지 않게 한다.
func traceWhyChildArgs(trace traceWhyTrace, plan traceWhyPlan) []string {
	args := []string{"trace", trace.name, "--raw"}
	if plan.duration > 0 {
		args = append(args, "--duration", plan.duration.String())
	}
	if plan.process != "" {
		args = append(args, "--process", plan.process)
	}
	if trace.name == "http" {
		args = append(args, "--payload=false")
	}
	if trace.port != 0 {
		args = append(args, "--port", strconv.Itoa(trace.port))
	}
	return args
}

func traceWhyNames(traces []traceWhyTrace) string {
	names := make([]string, 0, len(traces))
	for _, trace := range traces {
		names = append(names, trace.name)
	}
	return strings.Join(names, ", ")
}

func runTraceWhy(args []string) int {
	plan := traceWhyPlan{}
	var with string
	set := newFlagSet("trace why")
	set.SetOutput(os.Stderr)
	durationFlagVar(set, &plan.duration, "duration", 0, T("command.trace.option.duration"))
	set.StringVar(&plan.process, "process", "", T("command.trace.option.process"))
	set.IntVar(&plan.port, "port", 0, T("command.trace.option.port"))
	set.StringVar(&with, "with", "", T("command.trace.option.with"))
	set.StringVar(&plan.out, "out", "", T("command.trace.option.out"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", "trace why"))
		return 2
	}
	if plan.duration < 0 || plan.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.trace.duration_range"))
		return 2
	}
	portSet := false
	set.Visit(func(option *flag.Flag) { portSet = portSet || option.Name == "port" })
	if portSet && (plan.port < 1 || plan.port > 65535) {
		fmt.Fprintln(os.Stderr, T("cli.trace.port_range"))
		return 2
	}
	databases, err := parseTraceWhyWith(with)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.why_with", err))
		return 2
	}
	plan.traces = append([]traceWhyTrace{{name: "http"}, {name: "dns"}}, databases...)
	if err := traceWhyPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if plan.out == "" {
		plan.out = "edc-why-" + time.Now().Format("20060102-150405")
	}
	// 자식을 띄우기 전에 만든다. 이미 있는 경로에는 쓰지 않아서 앞선 기록과 섞이지 않는다.
	if err := os.Mkdir(plan.out, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.why_out", err))
		return 2
	}
	status := T("cli.trace.why_recording", traceWhyNames(plan.traces))
	if plan.duration > 0 {
		status += " " + plan.duration.String()
	}
	fmt.Fprintln(os.Stderr, status)
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	finishing := func() { fmt.Fprintln(os.Stderr, T("cli.trace.why_finishing")) }
	if err := recordTraceWhy(plan, ctx.Done(), finishing); err != nil {
		var failure *traceWhyChildError
		if errors.As(err, &failure) {
			if text, readErr := os.ReadFile(failure.stderr); readErr == nil {
				os.Stderr.Write(text)
			}
		}
		fmt.Fprintln(os.Stderr, T("cli.trace.why_failed", err, plan.out))
		return 1
	}
	if code := printTraceWhyDirectory(plan.out); code != 0 {
		fmt.Fprintln(os.Stderr, T("cli.trace.why_saved", plan.out))
		return code
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, T("cli.trace.why_saved", plan.out))
	return 0
}

// recordTraceWhy는 trace마다 자식 process를 띄워 plan.out에 <이름>.jsonl과 <이름>.err를 남기고, 모두 끝날 때까지 기다린다.
// stop이 닫히면 자식을 멈추고 그때까지의 기록을 남긴다. 자식 하나가 실패하면 나머지를 멈추고 그 실패를 돌려준다. 한 계층이
// 빠진 기록으로 분석하면 그 계층의 시간이 요청의 자체 시간으로 보이기 때문이다. finishing은 기록이 끝나 자식이 끝나기를
// 기다리기 시작할 때 한 번 불린다. 그 뒤로도 probe를 떼는 자식을 한동안 기다린다.
func recordTraceWhy(plan traceWhyPlan, stop <-chan struct{}, finishing func()) error {
	manifest := traceWhyManifest{Version: traceWhyManifestVersion, Port: plan.port, Process: plan.process}
	for _, trace := range plan.traces {
		manifest.Traces = append(manifest.Traces, traceWhyManifestTrace{Name: trace.name, File: trace.name + ".jsonl"})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(plan.out, traceWhyManifestName), append(data, '\n'), 0o600); err != nil {
		return err
	}

	type child struct {
		trace   traceWhyTrace
		command *exec.Cmd
		stderr  string
		done    bool
	}
	type exit struct {
		index int
		err   error
	}
	var children []*child
	exits := make(chan exit, len(plan.traces))
	signalAll := func(signal os.Signal) {
		for _, item := range children {
			if !item.done {
				item.command.Process.Signal(signal)
			}
		}
	}
	var failure error
	for _, trace := range plan.traces {
		item := &child{trace: trace, stderr: filepath.Join(plan.out, trace.name+".err")}
		var command *exec.Cmd
		stdout, err := os.OpenFile(filepath.Join(plan.out, trace.name+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			var stderr *os.File
			if stderr, err = os.OpenFile(item.stderr, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
				command, err = traceWhyStart(traceWhyChildArgs(trace, plan), stdout, stderr)
				stderr.Close()
			}
			stdout.Close()
		}
		if err != nil {
			failure = &traceWhyChildError{trace.name, item.stderr, err}
			break
		}
		item.command = command
		index := len(children)
		children = append(children, item)
		go func() { exits <- exit{index, command.Wait()} }()
	}

	// 멈추라는 신호는 한 번만 보낸다. 자식은 SIGTERM을 받으면 요약 줄을 쓰고 끝난다.
	stopping, notified := false, false
	notify := func() {
		if !notified && finishing != nil {
			finishing()
		}
		notified = true
	}
	var timeout <-chan time.Time
	beginStop := func() {
		if stopping {
			return
		}
		stopping = true
		notify()
		signalAll(syscall.SIGTERM)
		timeout = time.After(traceWhyStopTimeout)
	}
	if failure != nil {
		beginStop()
	}
	for running := len(children); running > 0; {
		select {
		case result := <-exits:
			running--
			item := children[result.index]
			item.done = true
			if result.err != nil && failure == nil {
				failure = &traceWhyChildError{item.trace.name, item.stderr, result.err}
				beginStop()
			}
			// --duration이 지나 먼저 끝난 자식이 있으면 나머지도 끝나는 중이다.
			if running > 0 {
				notify()
			}
		case <-stop:
			stop = nil
			beginStop()
		case <-timeout:
			timeout = nil
			if failure == nil {
				for _, item := range children {
					if !item.done {
						failure = &traceWhyChildError{item.trace.name, item.stderr, fmt.Errorf("did not stop within %s", traceWhyStopTimeout)}
						break
					}
				}
			}
			signalAll(syscall.SIGKILL)
		}
	}
	return failure
}

// readTraceWhyDirectory는 why.json에 적힌 기록을 읽는다. 적힌 파일이 없으면 디렉터리의 다른 파일로 대신하지 않는다.
func readTraceWhyDirectory(directory string) ([]*traceShowFile, traceWhyManifest, error) {
	var manifest traceWhyManifest
	manifestPath := filepath.Join(directory, traceWhyManifestName)
	if err := requireTraceWhyRegularFile(manifestPath); err != nil {
		return nil, manifest, err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, manifest, fmt.Errorf("%s: %w", traceWhyManifestName, err)
	}
	if manifest.Version != traceWhyManifestVersion {
		return nil, manifest, fmt.Errorf("%s: version %d is not supported (this edc reads version %d)", traceWhyManifestName, manifest.Version, traceWhyManifestVersion)
	}
	var files []*traceShowFile
	for _, trace := range manifest.Traces {
		// 파일 이름은 디렉터리 안의 이름이어야 한다. 다른 곳을 가리키는 why.json으로 임의의 파일을 읽지 않는다.
		if trace.File == "" || trace.File != filepath.Base(trace.File) {
			return nil, manifest, fmt.Errorf("%s: %q is not a file name in the directory", traceWhyManifestName, trace.File)
		}
		path := filepath.Join(directory, trace.File)
		if err := requireTraceWhyRegularFile(path); err != nil {
			return nil, manifest, fmt.Errorf("%s: %w", trace.File, err)
		}
		file, err := openTraceShowFile(path)
		if err != nil {
			return nil, manifest, fmt.Errorf("%s: %w", trace.File, err)
		}
		files = append(files, file)
	}
	return files, manifest, nil
}

// requireTraceWhyRegularFile은 심볼릭 링크를 거부한다. 기록 디렉터리 안의 링크로 디렉터리 밖의 파일을 읽지 않는다.
func requireTraceWhyRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file in the directory", filepath.Base(path))
	}
	return nil
}

// printTraceWhyDirectory는 기록 디렉터리의 분석을 출력한다. 읽지 못한 디렉터리는 2, 분석할 수 없는 기록은 1을 돌려준다.
func printTraceWhyDirectory(directory string) int {
	files, manifest, err := readTraceWhyDirectory(directory)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.show_failed", directory, err))
		return 2
	}
	report, err := analyzeTraceWhy(files, traceWhyOptions{port: manifest.Port})
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.show_failed", directory, err))
		return 1
	}
	report.print(os.Stdout)
	return 0
}
