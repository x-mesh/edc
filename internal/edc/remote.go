package edc

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const remoteOutputLimit = 64 * 1024

func runRemote(args []string, version string) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "사용법: edc remote run [--inventory <file> --recipe <file> --group <name>]")
		return 2
	}
	return runRemoteRun(args[1:], version)
}

func runRemoteRun(args []string, version string) int {
	options := commonOptions{timeout: 10 * time.Minute, redact: true}
	remoteOptions := remoteRunOptions{}
	connectTimeout := 10 * time.Second
	outputLimit := remoteOutputLimit
	parallelOverride := 0
	force := false
	interactiveArgs := len(args) == 0 || onlyRemoteInteractiveFlags(args)
	if interactiveArgs {
		if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
			fmt.Fprintln(os.Stderr, "인자가 없는 edc remote run은 terminal에서만 실행할 수 있습니다")
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		configDir, err := os.UserConfigDir()
		if err != nil {
			configDir = ""
		}
		for _, argument := range args {
			if argument == "-v" || argument == "--verbose" {
				options.verbose = true
			}
			if argument == "-f" || argument == "--force" {
				force = true
			}
		}
		remoteOptions, err = promptRemoteOptions(os.Stdin, os.Stdout, cwd, configDir, options.timeout, options.verbose, force)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			if err == errRemoteCancelled {
				return 4
			}
			return 2
		}
		options.verbose = remoteOptions.verbose
	} else {
		set := flag.NewFlagSet("remote run", flag.ContinueOnError)
		set.SetOutput(os.Stderr)
		bindCommon(set, &options)
		set.StringVar(&remoteOptions.inventoryPath, "inventory", "", "inventory YAML 경로")
		set.StringVar(&remoteOptions.recipePath, "recipe", "", "recipe YAML 경로")
		set.StringVar(&remoteOptions.group, "group", "", "실행할 inventory group")
		set.DurationVar(&connectTimeout, "connect-timeout", connectTimeout, "SSH 연결 제한 시간")
		set.IntVar(&outputLimit, "output-limit", outputLimit, "command별 출력 byte 상한")
		set.IntVar(&parallelOverride, "parallel", 0, "동시에 실행할 host 수, inventory 설정 override")
		if err := set.Parse(args); err != nil {
			return 2
		}
		if set.NArg() != 0 || remoteOptions.inventoryPath == "" || remoteOptions.recipePath == "" || remoteOptions.group == "" {
			fmt.Fprintln(os.Stderr, "--inventory, --recipe, --group이 모두 필요하며 positional argument는 허용하지 않습니다")
			return 2
		}
	}
	if options.timeout <= 0 || options.timeout > remoteMaxTimeout || connectTimeout <= 0 || connectTimeout > remoteMaxTimeout {
		fmt.Fprintf(os.Stderr, "--timeout과 --connect-timeout은 0보다 크고 %s 이하여야 합니다\n", remoteMaxTimeout)
		return 2
	}
	if outputLimit <= 0 || outputLimit > remoteConfigLimit {
		fmt.Fprintf(os.Stderr, "--output-limit은 1에서 %d 사이여야 합니다\n", remoteConfigLimit)
		return 2
	}
	if parallelOverride < 0 || parallelOverride > remoteHostLimit {
		fmt.Fprintf(os.Stderr, "--parallel은 1에서 %d 사이여야 합니다\n", remoteHostLimit)
		return 2
	}
	inventory, err := loadRemoteInventory(remoteOptions.inventoryPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	hosts, err := hostsForRemoteGroup(inventory, remoteOptions.group)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	recipe, err := loadRemoteRecipe(remoteOptions.recipePath, options.timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	for _, step := range recipe.Steps {
		if len(stepHostNames(step, hosts)) == 0 {
			fmt.Fprintf(os.Stderr, "경고: step %q의 tag(%s)와 일치하는 host가 group %q에 없습니다\n", step.Name, strings.Join(step.Tags, ", "), remoteOptions.group)
		}
	}
	started := time.Now()
	parallel := remoteParallelForGroup(inventory, remoteOptions.group, parallelOverride)
	terminalOutput := options.jsonPath == ""
	display := newRemoteDisplay(os.Stdout, remoteDisplayOptions{
		verbose: options.verbose && terminalOutput,
		spinner: terminalOutput && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
		results: terminalOutput,
		redact:  options.redact,
	})
	results := executeRemoteRecipeWithOptions(context.Background(), hosts, recipe, sshRemoteRunner{connectTimeout: connectTimeout, outputLimit: outputLimit}, parallel, display)
	display.Close()
	target := map[string]interface{}{"group": remoteOptions.group, "inventory": remoteOptions.inventoryPath, "recipe": remoteOptions.recipePath, "recipe_name": recipe.Name, "parallel": parallel}
	report := buildReport(version, started, target, results, options.redact)
	if terminalOutput {
		printRemoteTail(os.Stdout, report.Results, display.color)
		return exitCode(report.Results)
	}
	return emit(options, report)
}

func onlyRemoteInteractiveFlags(args []string) bool {
	for _, argument := range args {
		if argument != "-v" && argument != "--verbose" && argument != "-f" && argument != "--force" {
			return false
		}
	}
	return true
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func executeRemoteRecipe(ctx context.Context, hosts []remoteHost, recipe remoteRecipe, runner remoteCommandRunner) []Result {
	return executeRemoteRecipeWithOptions(ctx, hosts, recipe, runner, 1, nil)
}

func executeRemoteRecipeWithOptions(ctx context.Context, hosts []remoteHost, recipe remoteRecipe, runner remoteCommandRunner, parallel int, display *remoteDisplay) []Result {
	if parallel < 1 {
		parallel = 1
	}
	type hostResults struct {
		index   int
		results []Result
	}
	jobs := make(chan int)
	completed := make(chan hostResults, len(hosts))
	workers := parallel
	if workers > len(hosts) {
		workers = len(hosts)
	}
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				completed <- hostResults{index: index, results: executeRemoteHost(ctx, hosts[index], recipe, runner, display)}
			}
		}()
	}
	go func() {
		for index := range hosts {
			jobs <- index
		}
		close(jobs)
		group.Wait()
		close(completed)
	}()
	ordered := make([]hostResults, 0, len(hosts))
	for item := range completed {
		ordered = append(ordered, item)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].index < ordered[right].index })
	results := make([]Result, 0, len(hosts)*len(recipe.Steps))
	for _, item := range ordered {
		results = append(results, item.results...)
	}
	return results
}

func executeRemoteHost(ctx context.Context, host remoteHost, recipe remoteRecipe, runner remoteCommandRunner, display *remoteDisplay) []Result {
	results := make([]Result, 0, len(recipe.Steps))
	hostFailed := false
	for _, step := range recipe.Steps {
		if !stepRunsOnHost(step, host) {
			continue
		}
		started := time.Now()
		probe := fmt.Sprintf("remote.%s.%s", host.Name, step.Name)
		if hostFailed {
			skipped := Result{Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: "이 host의 이전 step이 실패해 건너뛰었습니다", Metrics: map[string]interface{}{"host": host.Name, "step": step.Name, "command_status": "skip", "verify_status": "skip"}}
			results = append(results, skipped)
			display.Result(skipped)
			continue
		}
		if display != nil {
			display.Start(host.Name, step.Name, "command")
		}
		commandStream := streamWriter(display, host.Name, step.Name, "command")
		commandResult := runner.Run(ctx, host.Target, step.Command, step.Timeout, commandStream)
		flushRemoteStream(commandStream)
		result := Result{
			Probe: probe, StartedAt: started.UTC(),
			Metrics: map[string]interface{}{
				"host": host.Name, "target": host.Target, "step": step.Name,
				"command_duration_ms": commandResult.Duration.Milliseconds(),
				"command_truncated":   commandResult.Truncated, "verify_status": "skip",
			},
		}
		appendRemoteOutput(&result, "command output", commandResult)
		if commandResult.Err != nil {
			result.Status = StatusFail
			result.Summary = fmt.Sprintf("%s에서 %s command가 실패했습니다", host.Name, step.Name)
			result.Error = remoteExecutionError("command", step.Timeout, commandResult)
			result.Metrics["command_status"] = "fail"
			result.Metrics["command_exit_code"] = commandResult.ExitCode
			result.DurationMS = time.Since(started).Milliseconds()
			results = append(results, result)
			display.Result(result)
			hostFailed = true
			continue
		}
		result.Metrics["command_status"] = "pass"
		result.Metrics["command_exit_code"] = 0
		if step.Verify == "" {
			result.Status = StatusPass
			result.Summary = fmt.Sprintf("%s에서 %s command를 실행했습니다", host.Name, step.Name)
			result.Metrics["verify_status"] = "none"
			result.DurationMS = time.Since(started).Milliseconds()
			results = append(results, result)
			display.Result(result)
			continue
		}
		if display != nil {
			display.Start(host.Name, step.Name, "verify")
		}
		verifyStream := streamWriter(display, host.Name, step.Name, "verify")
		verifyResult := runner.Run(ctx, host.Target, step.Verify, step.Timeout, verifyStream)
		flushRemoteStream(verifyStream)
		result.Metrics["verify_duration_ms"] = verifyResult.Duration.Milliseconds()
		result.Metrics["verify_truncated"] = verifyResult.Truncated
		result.Metrics["verify_exit_code"] = verifyResult.ExitCode
		appendRemoteOutput(&result, "verify output", verifyResult)
		if verifyResult.Err != nil {
			result.Status = StatusFail
			result.Summary = fmt.Sprintf("%s에서 %s verify가 실패했습니다", host.Name, step.Name)
			result.Error = remoteExecutionError("verify", step.Timeout, verifyResult)
			result.Metrics["verify_status"] = "fail"
			hostFailed = true
		} else {
			result.Status = StatusPass
			result.Summary = fmt.Sprintf("%s에서 %s 설치 상태를 확인했습니다", host.Name, step.Name)
			result.Metrics["verify_status"] = "pass"
		}
		result.DurationMS = time.Since(started).Milliseconds()
		results = append(results, result)
		display.Result(result)
	}
	return results
}

func streamWriter(display *remoteDisplay, host, step, phase string) io.Writer {
	if display == nil || !display.verbose {
		return nil
	}
	return &remoteStreamWriter{display: display, prefix: fmt.Sprintf("%s/%s/%s", host, step, phase)}
}

func flushRemoteStream(writer io.Writer) {
	if stream, ok := writer.(*remoteStreamWriter); ok {
		stream.Flush()
	}
}

func appendRemoteOutput(result *Result, label string, command remoteCommandResult) {
	if command.Output != "" {
		result.Evidence = append(result.Evidence, Evidence{Label: label, Value: command.Output})
	}
	if command.Truncated {
		result.Warnings = append(result.Warnings, label+"이 제한에 도달해 잘렸습니다")
	}
}

func remoteExecutionError(phase string, timeout time.Duration, result remoteCommandResult) *DiagnosticError {
	kind := phase
	message := result.Err.Error()
	if result.TimedOut {
		kind = "timeout"
		message = fmt.Sprintf("%s timeout: %s", phase, timeout)
	} else if result.ExitCode == 255 || result.ExitCode == -1 {
		kind = "ssh"
	}
	return &DiagnosticError{Kind: kind, Message: message}
}
