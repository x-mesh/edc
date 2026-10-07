package edc

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type fsWatchRecord struct {
	Type            string    `json:"type"`
	Time            time.Time `json:"time"`
	Root            string    `json:"root,omitempty"`
	Recursive       bool      `json:"recursive,omitempty"`
	Event           string    `json:"event,omitempty"`
	Path            string    `json:"path,omitempty"`
	IsDir           bool      `json:"is_directory,omitempty"`
	Rule            string    `json:"rule,omitempty"`
	Command         []string  `json:"command,omitempty"`
	CWD             string    `json:"cwd,omitempty"`
	Status          string    `json:"status,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	DurationMS      int64     `json:"duration_ms,omitempty"`
	Output          string    `json:"output,omitempty"`
	OutputTruncated bool      `json:"output_truncated,omitempty"`
	Error           string    `json:"error,omitempty"`
	Events          int       `json:"events,omitempty"`
	Actions         int       `json:"actions,omitempty"`
	Failed          int       `json:"failed,omitempty"`
	StopReason      string    `json:"stop_reason,omitempty"`
}

type fsWatchRuleState struct {
	rule    fsWatchRule
	pending bool
	event   fsWatchEvent
	due     time.Time
}

type fsWatchActionRunner func(context.Context, fsWatchRule, fsWatchEvent, string) fsWatchRecord

func runFSWatch(args []string) int {
	options, err := parseFSWatchOptions(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printWatchSubcommandHelp(os.Stdout, "fs")
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	writer, closeOutput, err := openObserveStream(options.jsonPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer closeOutput()
	if file, ok := writer.(*os.File); ok {
		options.outputInfo, _ = file.Stat()
	}
	// 재귀 감시는 모든 디렉터리를 등록한 뒤에야 첫 줄을 낸다. 큰 트리에서는 수십 초 걸려 멈춘 것처럼 보이므로 터미널에 먼저 알린다.
	if options.recursive && isTerminal(os.Stderr) {
		fmt.Fprintln(os.Stderr, T("watchfs.walking", terminalJSON(options.root)))
	}
	source, err := newFSWatchSource(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer source.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.duration)
		defer cancel()
	}
	code, err := streamFSWatch(ctx, writer, source, options, executeFSWatchAction)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return code
}

func streamFSWatch(ctx context.Context, writer io.Writer, source fsWatchSource, options fsWatchOptions, execute fsWatchActionRunner) (int, error) {
	actionCtx, cancelActions := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancelActions(); workers.Wait() }()
	emit := func(record fsWatchRecord) error { return writeFSWatchRecord(writer, record, options.jsonPath != "") }
	if err := emit(fsWatchRecord{Type: "ready", Time: time.Now().UTC(), Root: options.root, Recursive: options.recursive}); err != nil {
		return 2, err
	}
	states := make([]fsWatchRuleState, len(options.rules))
	for index, rule := range options.rules {
		states[index].rule = rule
	}
	completed := make(chan fsWatchRecord, 1)
	var running, stopping bool
	var failure error
	summary := fsWatchRecord{Type: "summary", Root: options.root}
	started := time.Now()
	events, failures, contextDone := source.events, source.errors, ctx.Done()
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if ctx.Err() != nil && !stopping {
			stopping = true
			events, failures, contextDone = nil, nil, nil
			cancelActions()
		}
		if stopping && !running {
			break
		}
		var timerC <-chan time.Time
		if !running && !stopping {
			index, due := nextFSWatchAction(states)
			if index >= 0 {
				if !time.Now().Before(due) {
					state := &states[index]
					state.pending, running = false, true
					event, rule := state.event, state.rule
					if err := emit(fsWatchRecord{Type: "action_start", Time: time.Now().UTC(), Rule: rule.Name, Command: rule.Command, CWD: rule.CWD, Event: event.Event, Path: event.Path}); err != nil {
						cancelActions()
						return 2, err
					}
					workers.Add(1)
					go func() {
						defer workers.Done()
						var record fsWatchRecord
						if options.dryRun {
							record = fsWatchRecord{Type: "action_result", Time: time.Now().UTC(), Rule: rule.Name, Command: rule.Command, CWD: rule.CWD, Event: event.Event, Path: event.Path, Status: "dry_run"}
						} else {
							record = execute(actionCtx, rule, event, options.root)
						}
						completed <- record
					}()
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(time.Until(due))
					timerC = timer.C
				}
			}
		}
		select {
		case <-contextDone:
			stopping = true
			events, failures, contextDone = nil, nil, nil
			cancelActions()
		case event, ok := <-events:
			if !ok {
				failure = errors.New(T("watchfs.source_closed"))
				select {
				case err, ok := <-failures:
					if ok {
						failure = err
					}
				default:
				}
				stopping = true
				events, failures = nil, nil
				cancelActions()
				continue
			}
			if !fsWatchEventMatches(options.events, options.match, event) {
				continue
			}
			summary.Events++
			if err := emit(fsWatchRecord{Type: "event", Time: event.Time, Event: event.Event, Path: event.Path, IsDir: event.IsDir}); err != nil {
				cancelActions()
				return 2, err
			}
			for index := range states {
				state := &states[index]
				if fsWatchEventMatches(state.rule.Events, state.rule.Match, event) {
					state.pending, state.event, state.due = true, event, time.Now().Add(state.rule.debounce)
				}
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			failure, stopping = err, true
			events, failures = nil, nil
			cancelActions()
		case result := <-completed:
			running = false
			summary.Actions++
			if result.Status == "failed" || result.Status == "timeout" {
				summary.Failed++
			}
			if err := emit(result); err != nil {
				cancelActions()
				return 2, err
			}
		case <-timerC:
		}
	}
	summary.Time, summary.DurationMS = time.Now().UTC(), time.Since(started).Milliseconds()
	summary.StopReason = "cancelled"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		summary.StopReason = "duration"
	}
	if failure != nil {
		summary.StopReason, summary.Error = "watch_error", failure.Error()
	}
	if err := emit(summary); err != nil {
		return 2, err
	}
	if failure != nil {
		return 1, failure
	}
	if summary.Failed > 0 {
		return 1, nil
	}
	return 0, nil
}

func nextFSWatchAction(states []fsWatchRuleState) (int, time.Time) {
	index := -1
	var due time.Time
	for candidate, state := range states {
		if state.pending && (index < 0 || state.due.Before(due)) {
			index, due = candidate, state.due
		}
	}
	return index, due
}

func fsWatchEventMatches(events []string, pattern string, event fsWatchEvent) bool {
	for _, kind := range events {
		if kind == event.Event {
			return matchFSWatchGlob(pattern, event.Path)
		}
	}
	return false
}

func executeFSWatchAction(ctx context.Context, rule fsWatchRule, event fsWatchEvent, root string) fsWatchRecord {
	record := fsWatchRecord{Type: "action_result", Rule: rule.Name, Command: rule.Command, CWD: rule.CWD, Event: event.Event, Path: event.Path}
	started := time.Now()
	childCtx, cancel := context.WithTimeout(ctx, rule.timeout)
	defer cancel()
	command := exec.CommandContext(childCtx, rule.Command[0], rule.Command[1:]...)
	command.Dir = rule.CWD
	command.Env = os.Environ()
	for key, value := range map[string]string{"EDC_WATCH_ROOT": root, "EDC_WATCH_PATH": event.Path, "EDC_WATCH_EVENT": event.Event, "EDC_WATCH_RULE": rule.Name} {
		command.Env = append(command.Env, key+"="+value)
	}
	configureLogProcess(command)
	command.Cancel = func() error { return signalLogProcess(command, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	var output fsWatchActionOutput
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = signalLogProcess(command, syscall.SIGKILL)
	}
	record.Time, record.DurationMS = time.Now().UTC(), time.Since(started).Milliseconds()
	record.Output, record.OutputTruncated = output.text(), output.truncated
	record.Status = "success"
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		record.ExitCode = &code
	}
	if err != nil {
		record.Status, record.Error = "failed", err.Error()
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			record.Status = "timeout"
		} else if ctx.Err() != nil {
			record.Status = "cancelled"
		}
	}
	return record
}

const fsWatchOutputLimit = 64 << 10

type fsWatchActionOutput struct {
	mutex     sync.Mutex
	data      []byte
	truncated bool
}

func (output *fsWatchActionOutput) Write(data []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	remaining := fsWatchOutputLimit - len(output.data)
	if len(data) > remaining {
		output.truncated = true
	}
	output.data = append(output.data, data[:min(len(data), remaining)]...)
	return len(data), nil
}

func (output *fsWatchActionOutput) text() string {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	return strings.ToValidUTF8(string(output.data), "�")
}

func writeFSWatchRecord(writer io.Writer, record fsWatchRecord, jsonOutput bool) error {
	if jsonOutput {
		if record.Type == "summary" {
			summary := map[string]interface{}{"type": record.Type, "time": record.Time, "root": record.Root, "events": record.Events, "actions": record.Actions, "failed": record.Failed, "duration_ms": record.DurationMS, "stop_reason": record.StopReason}
			if record.Error != "" {
				summary["error"] = record.Error
			}
			return json.NewEncoder(writer).Encode(summary)
		}
		return json.NewEncoder(writer).Encode(record)
	}
	stamp := record.Time.Local().Format("15:04:05.000")
	switch record.Type {
	case "ready":
		_, err := fmt.Fprintf(writer, "%s  %s\n", stamp, T("watchfs.ready", terminalJSON(record.Root), record.Recursive))
		return err
	case "event":
		_, err := fmt.Fprintf(writer, "%s  %-7s  %s\n", stamp, strings.ToUpper(record.Event), terminalJSON(record.Path))
		return err
	case "action_start":
		_, err := fmt.Fprintf(writer, "%s  START    %s · %s · %s\n", stamp, terminalJSON(record.Rule), terminalJSON(record.Path), terminalJSON(strings.Join(record.Command, " ")))
		return err
	case "action_result":
		exit := "—"
		if record.ExitCode != nil {
			exit = fmt.Sprint(*record.ExitCode)
		}
		if _, err := fmt.Fprintf(writer, "%s  DONE     %s · %s · exit %s · %dms\n", stamp, terminalJSON(record.Rule), record.Status, exit, record.DurationMS); err != nil {
			return err
		}
		if record.Output != "" {
			for _, line := range strings.Split(strings.TrimSuffix(record.Output, "\n"), "\n") {
				if _, err := fmt.Fprintf(writer, "          %s | %s\n", terminalJSON(record.Rule), terminalJSON(line)); err != nil {
					return err
				}
			}
		}
		if record.OutputTruncated {
			if _, err := fmt.Fprintln(writer, T("watchfs.output_truncated", fsWatchOutputLimit)); err != nil {
				return err
			}
		}
		if record.Error != "" {
			_, err := fmt.Fprintln(writer, "          "+terminalJSON(record.Error))
			return err
		}
	case "summary":
		_, err := fmt.Fprintf(writer, "%s  %s\n", stamp, T("watchfs.summary", record.Events, record.Actions, record.Failed, fsWatchStopLabel(record.StopReason)))
		return err
	}
	return nil
}

func fsWatchStopLabel(reason string) string {
	switch reason {
	case "duration":
		return T("watchfs.stop.duration")
	case "cancelled":
		return T("watchfs.stop.cancelled")
	case "watch_error":
		return T("watchfs.stop.watch_error")
	default:
		return reason
	}
}
