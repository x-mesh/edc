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
		return "", errors.New("group은 positional argument나 --group 중 하나로 한 번만 지정합니다")
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
		return "edc remote run은 edc remote <group>으로 바뀌었습니다. 인자 없이 edc remote를 실행하면 group을 선택합니다"
	}
	return fmt.Sprintf("%q는 하위 command 이름으로 예약되어 있어 group으로 쓸 수 없습니다", name)
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
	set.StringVar(&remoteOptions.inventoryPath, "inventory", "", "inventory YAML 경로")
	set.StringVar(&remoteOptions.recipePath, "recipe", "", "recipe YAML 경로")
	set.StringVar(&groupFlag, "group", "", "실행할 inventory group, positional argument의 별칭")
	set.DurationVar(&connectTimeout, "connect-timeout", connectTimeout, "SSH 연결 제한 시간")
	set.IntVar(&outputLimit, "output-limit", outputLimit, "command별 출력 byte 상한")
	set.IntVar(&parallelOverride, "parallel", 0, "동시에 실행할 host 수, inventory 설정 override")
	set.BoolVar(&force, "force", false, "계획 확인 프롬프트 생략")
	set.BoolVar(&force, "f", false, "--force 단축 option")
	set.BoolVar(&dryRun, "dry-run", false, "실행 계획만 출력하고 종료")
	set.BoolVar(&dryRun, "n", false, "--dry-run 단축 option")
	set.BoolVar(&list, "list", false, "inventory의 group과 host를 출력하고 종료")
	set.BoolVar(&list, "l", false, "--list 단축 option")
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
		fmt.Fprintln(os.Stderr, "--dry-run과 -f는 같이 쓸 수 없습니다")
		return 2
	}
	if list && (dryRun || force) {
		fmt.Fprintln(os.Stderr, "--list는 --dry-run, -f와 같이 쓸 수 없습니다")
		return 2
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
	promptFlags := remotePromptFlags{force: force, dryRun: dryRun, interactive: isTerminal(os.Stdin) && isTerminal(os.Stdout)}
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
			fmt.Fprintf(os.Stderr, "경고: step %q의 tag(%s)와 일치하는 host가 group %q에 없습니다\n", step.Name, strings.Join(step.Tags, ", "), remoteOptions.group)
		}
	}
	started := time.Now()
	parallel := remoteParallelForGroup(inventory, remoteOptions.group, parallelOverride)
	if dryRun {
		return emitRemotePlan(options, remoteOptions, hosts, recipe, parallel)
	}
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
