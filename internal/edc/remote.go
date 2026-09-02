package edc

import (
	"context"
	"errors"
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
	group, rest := splitRemoteGroupArgument(args)
	return runRemoteRun(group, rest, version)
}

// splitRemoteGroupArgument는 맨 앞의 positional group을 떼어낸다.
// flag 패키지는 첫 non-flag에서 파싱을 멈추므로 group 뒤에 오는 flag를 남겨 두어야 한다.
func splitRemoteGroupArgument(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// remoteGroupArgument는 positional group, --group, 남은 positional을 하나로 합친다.
func remoteGroupArgument(leading, flagValue string, rest []string) (string, error) {
	candidates := make([]string, 0, 2+len(rest))
	if leading != "" {
		candidates = append(candidates, leading)
	}
	if flagValue != "" {
		candidates = append(candidates, flagValue)
	}
	candidates = append(candidates, rest...)
	if len(candidates) > 1 {
		return "", errors.New(T("remote.error.group_once"))
	}
	if len(candidates) == 0 {
		return "", nil
	}
	if remoteReservedGroup(candidates[0]) {
		return "", errors.New(remoteReservedGroupHint(candidates[0]))
	}
	return candidates[0], nil
}

func remoteReservedGroupHint(name string) string {
	if name == "run" {
		return T("remote.error.run_renamed")
	}
	return T("remote.error.reserved_group", name)
}

func runRemoteRun(group string, args []string, version string) int {
	options := commonOptions{timeout: 10 * time.Minute, redact: true}
	remoteOptions := remoteRunOptions{}
	connectTimeout := 10 * time.Second
	outputLimit := remoteOutputLimit
	parallelOverride := 0
	force := false
	dryRun := false
	list := false
	groupFlag := ""
	set := flag.NewFlagSet("remote", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	set.StringVar(&remoteOptions.inventoryPath, "inventory", "", T("remote.flag.inventory"))
	set.StringVar(&remoteOptions.recipePath, "recipe", "", T("remote.flag.recipe"))
	set.StringVar(&groupFlag, "group", "", T("remote.flag.group"))
	set.DurationVar(&connectTimeout, "connect-timeout", connectTimeout, T("remote.flag.connect_timeout"))
	set.IntVar(&outputLimit, "output-limit", outputLimit, T("remote.flag.output_limit"))
	set.IntVar(&parallelOverride, "parallel", 0, T("remote.flag.parallel"))
	set.BoolVar(&force, "force", false, T("remote.flag.force"))
	set.BoolVar(&force, "f", false, T("remote.flag.force_short"))
	set.BoolVar(&dryRun, "dry-run", false, T("remote.flag.dry_run"))
	set.BoolVar(&dryRun, "n", false, T("remote.flag.dry_run_short"))
	set.BoolVar(&list, "list", false, T("remote.flag.list"))
	set.BoolVar(&list, "l", false, T("remote.flag.list_short"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	resolvedGroup, err := remoteGroupArgument(group, groupFlag, set.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	remoteOptions.group = resolvedGroup
	if dryRun && force {
		fmt.Fprintln(os.Stderr, T("remote.error.dry_run_with_force"))
		return 2
	}
	if list && (dryRun || force) {
		fmt.Fprintln(os.Stderr, T("remote.error.list_with_force"))
		return 2
	}
	if options.timeout <= 0 || options.timeout > remoteMaxTimeout || connectTimeout <= 0 || connectTimeout > remoteMaxTimeout {
		fmt.Fprintln(os.Stderr, T("remote.error.timeout_range", remoteMaxTimeout))
		return 2
	}
	if outputLimit <= 0 || outputLimit > remoteConfigLimit {
		fmt.Fprintln(os.Stderr, T("remote.error.output_limit_range", remoteConfigLimit))
		return 2
	}
	if parallelOverride < 0 || parallelOverride > remoteHostLimit {
		fmt.Fprintln(os.Stderr, T("remote.error.parallel_range", remoteHostLimit))
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
	if list {
		return runRemoteList(options, remoteOptions, cwd, configDir)
	}
	promptOutput := io.Writer(os.Stdout)
	if options.jsonPath == "-" {
		// stdout으로 나가는 JSON에 계획과 프롬프트가 섞이지 않게 한다.
		promptOutput = os.Stderr
	}
	remoteOptions.verbose = options.verbose
	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	promptFlags := remotePromptFlags{force: force, dryRun: dryRun, live: !dryRun && !list && options.jsonPath == "" && liveTerminal(), interactive: interactive}
	remoteOptions, err = promptRemoteOptions(os.Stdin, promptOutput, cwd, configDir, options.timeout, remoteOptions, promptFlags)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if err == errRemoteCancelled {
			return 4
		}
		return 2
	}
	options.verbose = remoteOptions.verbose
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
			fmt.Fprintln(os.Stderr, T("remote.warn.step_no_host", step.Name, strings.Join(step.Tags, ", "), remoteOptions.group))
		}
	}
	started := time.Now()
	parallel := remoteParallelForGroup(inventory, remoteOptions.group, parallelOverride)
	plan := remotePlanView{
		group: remoteOptions.group, inventoryPath: remoteOptions.inventoryPath, recipePath: remoteOptions.recipePath,
		cwd: cwd, hosts: hosts, recipe: recipe, width: terminalWidth(),
	}
	if dryRun {
		return emitRemotePlan(options, remoteOptions, plan, parallel)
	}
	terminalOutput := options.jsonPath == ""
	// Ctrl-C는 raw mode에서 signal이 아니라 키로 오므로 실행 중인 ssh는 context로만 끊을 수 있다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := terminalOutput && liveTerminal()
	display := newRemoteDisplay(os.Stdout, remoteDisplayOptions{
		verbose: options.verbose && terminalOutput,
		live:    live,
		confirm: live && !force,
		results: terminalOutput,
		redact:  options.redact,
		plan:    plan,
		cancel:  cancel,
	})
	// 확인은 실행 표와 같은 화면에서 받는다. 거절하면 아무것도 실행하지 않는다.
	if !display.awaitConfirm() {
		display.Close()
		fmt.Fprintln(os.Stderr, errRemoteCancelled)
		return 4
	}
	results := executeRemoteRecipeWithOptions(ctx, hosts, recipe, sshRemoteRunner{connectTimeout: connectTimeout, outputLimit: outputLimit}, parallel, display)
	// Close가 실시간 화면을 끝내면 onExit이 cancel을 부르므로, 사용자 취소 여부는 그 전에 읽는다.
	cancelled := ctx.Err() != nil
	display.Close()
	target := map[string]interface{}{"group": remoteOptions.group, "inventory": remoteOptions.inventoryPath, "recipe": remoteOptions.recipePath, "recipe_name": recipe.Name, "parallel": parallel}
	report := buildReport(version, started, target, results, options.redact)
	if terminalOutput {
		printResultDetails(os.Stdout, report.Results, options.verbose, display.color)
		fmt.Fprintf(os.Stdout, "\n%s  %d/%d  %s  ·  %s\n", remoteOutcome(cancelled), completedSteps(report.Results), len(report.Results), time.Since(started).Round(100*time.Millisecond), compactResultCounts(report.Results))
	} else if code := emit(options, report); !cancelled {
		return code
	}
	if cancelled {
		fmt.Fprintln(os.Stderr, errRemoteCancelled)
		return 4
	}
	return exitCode(report.Results)
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
		if ctx.Err() != nil {
			// 취소가 이전 step 실패보다 우선한다. 취소된 step을 ssh 실패로 남기지 않는다.
			cancelled := remoteCancelledResult(probe, host.Name, step.Name, started)
			results = append(results, cancelled)
			display.Result(cancelled)
			hostFailed = true
			continue
		}
		if hostFailed {
			skipped := Result{Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: T("remote.result.skipped_after_failure"), Metrics: map[string]interface{}{"host": host.Name, "step": step.Name, "command_status": "skip", "verify_status": "skip"}}
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
		if ctx.Err() != nil {
			cancelled := remoteCancelledResult(probe, host.Name, step.Name, started)
			results = append(results, cancelled)
			display.Result(cancelled)
			hostFailed = true
			continue
		}
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
			result.Summary = T("remote.result.command_failed", host.Name, step.Name)
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
			result.Summary = T("remote.result.command_ran", host.Name, step.Name)
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
			result.Summary = T("remote.result.verify_failed", host.Name, step.Name)
			result.Error = remoteExecutionError("verify", step.Timeout, verifyResult)
			result.Metrics["verify_status"] = "fail"
			hostFailed = true
		} else {
			result.Status = StatusPass
			result.Summary = T("remote.result.verify_passed", host.Name, step.Name)
			result.Metrics["verify_status"] = "pass"
		}
		result.DurationMS = time.Since(started).Milliseconds()
		results = append(results, result)
		display.Result(result)
	}
	return results
}

// remoteOutcome은 마지막 줄의 첫 낱말이다.
func remoteOutcome(cancelled bool) string {
	if cancelled {
		return T("remote.outcome.cancelled")
	}
	return T("remote.outcome.done")
}

// completedSteps는 실제로 실행해 결과가 난 step 수다. skip은 세지 않는다.
func completedSteps(results []Result) int {
	count := 0
	for _, result := range results {
		if result.Status != StatusSkip {
			count++
		}
	}
	return count
}

// remoteCancelledResult는 사용자가 취소해 실행하지 못한 step을 남긴다.
func remoteCancelledResult(probe, host, step string, started time.Time) Result {
	return Result{
		Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: T("remote.result.cancelled"),
		Metrics: map[string]interface{}{"host": host, "step": step, "command_status": "cancelled", "verify_status": "skip"},
	}
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
		result.Warnings = append(result.Warnings, T("remote.warn.output_truncated", label))
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
