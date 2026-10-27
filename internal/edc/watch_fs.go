package edc

import (
	"bufio"
	"bytes"
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
	Type         string    `json:"type"`
	Time         time.Time `json:"time"`
	Root         string    `json:"root,omitempty"`
	Recursive    bool      `json:"recursive,omitempty"`
	Driver       string    `json:"driver,omitempty"`
	DriverReason string    `json:"driver_reason,omitempty"`
	Event        string    `json:"event,omitempty"`
	Path         string    `json:"path,omitempty"`
	IsDir        bool      `json:"is_directory,omitempty"`
	// Origin과 Count는 묶어서 보일 때만 채운다. Origin은 이름을 바꿔 이 경로가 된 옛 이름이고, Count는 합친 modify의 수다.
	Origin string `json:"origin,omitempty"`
	Count  int    `json:"count,omitempty"`
	// PID와 Process는 이벤트를 일으킨 process다. fanotify일 때만 있다.
	PID             int      `json:"pid,omitempty"`
	Process         string   `json:"process,omitempty"`
	Rule            string   `json:"rule,omitempty"`
	Command         []string `json:"command,omitempty"`
	CWD             string   `json:"cwd,omitempty"`
	Status          string   `json:"status,omitempty"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	DurationMS      int64    `json:"duration_ms,omitempty"`
	Output          string   `json:"output,omitempty"`
	OutputTruncated bool     `json:"output_truncated,omitempty"`
	Error           string   `json:"error,omitempty"`
	Events          int      `json:"events,omitempty"`
	Actions         int      `json:"actions,omitempty"`
	Failed          int      `json:"failed,omitempty"`
	StopReason      string   `json:"stop_reason,omitempty"`
	// Incomplete는 감시가 변경을 놓쳤다는 뜻이다. Overflows는 커널 큐가 넘친 횟수, Dropped는 edc가 버린 이벤트 수, Rescans는
	// macOS가 다시 훑은 횟수다. 요약에만 있다.
	Incomplete bool `json:"incomplete,omitempty"`
	Overflows  int  `json:"overflows,omitempty"`
	Dropped    int  `json:"dropped,omitempty"`
	Rescans    int  `json:"rescans,omitempty"`
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
		if !errors.Is(err, errFlagReported) {
			fmt.Fprintln(os.Stderr, err)
		}
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
	options.loss = &fsWatchLoss{}
	source, err := newFSWatchSource(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// 감시 루프가 터미널 출력에 막혀도 드라이버는 계속 읽게 한다. 커널 큐는 몇 초면 넘치고 그동안의 변경을 잃는다.
	source = fsWatchBuffered(source, fsWatchQueueLimit, fsWatchQueueResume, options.loss)
	defer source.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.duration)
		defer cancel()
	}
	// 상태줄은 사람이 보는 터미널에만 그린다. pipe, 파일과 JSON 출력은 바뀌지 않는다.
	if options.jsonPath == "" && isTerminal(os.Stdout) {
		options.color = richEnabled(os.Stdout)
		options.status = newFSWatchStatus(os.Stdout, terminalWidth, time.Now, options.events, len(options.rules) > 0)
		options.status.color = options.color
		previous := fsWatchNotice.swap(options.status.through(os.Stderr))
		defer fsWatchNotice.swap(previous)
	}
	options.smart = fsWatchSmartEnabled(options.smartOn, options.smartOff, options.jsonPath, isTerminal(os.Stdout))
	// 알림은 stderr가 터미널일 때만 낸다. 묶어서 보이는 출력의 일부라서 --no-smart면 낸다고 하지 않는다.
	if options.smart && isTerminal(os.Stderr) {
		hintStyle := textStyle{color: richEnabled(os.Stderr)}
		options.notice = func(text string) { fmt.Fprintln(fsWatchNotice, fsWatchHintLine(hintStyle, text)) }
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
	var tick <-chan time.Time
	if options.status != nil {
		// 한 기록을 한 번에 써야 상태줄을 기록마다 한 번만 지우고 다시 그린다.
		defer options.status.close()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		tick = ticker.C
	}
	// 기록 줄은 자르지 않으므로 폭이 필요 없다. 기록마다 터미널 폭을 묻지 않게 한 번만 만든다.
	style := textStyle{color: options.color}
	// 터미널이 아니면 기록을 모아 쓴다. 이벤트마다 write하면 그 syscall이 이벤트 처리 시간의 1/4에 가까웠다. 상태줄은 지우고
	// 쓰고 다시 그리는 순서가 있어 바로 쓴다.
	output := writer
	var buffered *bufio.Writer
	if options.status == nil {
		buffered = bufio.NewWriterSize(writer, fsWatchOutputBuffer)
		output = buffered
	}
	flush := func() error {
		if buffered == nil {
			return nil
		}
		return buffered.Flush()
	}
	write := func(record fsWatchRecord) error {
		if options.status == nil {
			return writeFSWatchRecord(output, record, options.jsonPath != "", style)
		}
		if record.Type == "summary" {
			options.status.close()
		}
		var buffer bytes.Buffer
		if err := writeFSWatchRecord(&buffer, record, options.jsonPath != "", style); err != nil {
			return err
		}
		_, err := options.status.through(writer).Write(buffer.Bytes())
		return err
	}
	// 같은 줄이 이어서 오면 앞의 줄을 ×N으로 고쳐 쓴다. 상태줄이 있는 터미널에서 묶어 보일 때만 쓰고, 다른 것을 쓰면 잊는다.
	var repeater *fsWatchRepeater
	if options.smart && options.status != nil {
		repeater = newFSWatchRepeater(options.status.width, style)
	}
	emit := func(record fsWatchRecord) error {
		if repeater != nil {
			repeater.reset()
		}
		return write(record)
	}
	if err := emit(fsWatchRecord{Type: "ready", Time: time.Now().UTC(), Root: options.root, Recursive: options.recursive, Driver: source.driver, DriverReason: source.driverReason}); err != nil {
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
	// 묶어서 보일 때는 이벤트를 창 동안 모았다가 낸다. 규칙, 상태줄과 요약은 원본 이벤트를 그대로 쓴다.
	var smart *fsWatchSmart
	smartTimer := time.NewTimer(time.Hour)
	if !smartTimer.Stop() {
		<-smartTimer.C
	}
	defer smartTimer.Stop()
	var noisy *fsWatchNoisy
	var top *fsWatchTop
	if options.smart {
		smart = newFSWatchSmart(fsWatchSmartWindow)
		top = newFSWatchTop()
		if options.notice != nil {
			noisy = newFSWatchNoisy()
		}
	}
	emitSmart := func(records []fsWatchRecord) error {
		for _, record := range records {
			if repeater == nil {
				if err := write(record); err != nil {
					return err
				}
				continue
			}
			merged, rows := repeater.next(record)
			if rows == 0 {
				if err := write(merged); err != nil {
					return err
				}
				continue
			}
			var buffer bytes.Buffer
			if err := writeFSWatchRecord(&buffer, merged, false, style); err != nil {
				return err
			}
			if err := options.status.replaceLast(rows, buffer.Bytes()); err != nil {
				return err
			}
		}
		return nil
	}
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
		var smartC <-chan time.Time
		if smart != nil {
			if due, ok := smart.deadline(); ok {
				if !smartTimer.Stop() {
					select {
					case <-smartTimer.C:
					default:
					}
				}
				smartTimer.Reset(time.Until(due))
				smartC = smartTimer.C
			}
		}
		// 모은 기록은 바로 받을 이벤트가 없을 때 쓴다. 이벤트가 쏟아지는 동안은 모아서 syscall을 줄이고, 뜸하면 바로 보인다.
		if buffered != nil && buffered.Buffered() > 0 && len(events) == 0 {
			if err := flush(); err != nil {
				cancelActions()
				return 2, err
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
			if options.status != nil {
				options.status.event(event.Event, event.Time)
			}
			if top != nil {
				top.add(event.Path)
			}
			if noisy != nil {
				if hint := noisy.add(event.Path, time.Now()); hint != nil {
					// 안내가 맨 아래 줄을 차지하므로, 앞의 이벤트 줄을 고쳐 쓰면 안내가 지워진다.
					if repeater != nil {
						repeater.reset()
					}
					options.notice(T("watchfs.noisy", terminalJSON(hint.Directory), hint.Events, fsWatchNoisyWindow, fsWatchShellQuote(hint.Pattern)))
				}
			}
			if smart != nil {
				smart.add(event, time.Now())
			} else if err := emit(fsWatchEventRecord(event, options.jsonPath != "")); err != nil {
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
			failed := result.Status == "failed" || result.Status == "timeout"
			if failed {
				summary.Failed++
			}
			if options.status != nil {
				options.status.action(failed)
			}
			if err := emit(result); err != nil {
				cancelActions()
				return 2, err
			}
		case <-timerC:
		case <-smartC:
			if err := emitSmart(smart.flush(time.Now())); err != nil {
				cancelActions()
				return 2, err
			}
		case <-tick:
			options.status.tick()
		}
	}
	// 멈출 때 창에 남은 이벤트를 요약 앞에 낸다.
	if smart != nil {
		if err := emitSmart(smart.flushAll()); err != nil {
			return 2, err
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
	summary.Overflows, summary.Dropped, summary.Rescans = options.loss.counts()
	summary.Incomplete = summary.Overflows+summary.Dropped+summary.Rescans > 0
	if err := emit(summary); err != nil {
		return 2, err
	}
	// 요약 아래에 어느 디렉터리가 가장 시끄러웠는지 보인다. 묶어서 보일 때만 낸다.
	if top != nil {
		if text := fsWatchTopText(top.rows()); text != "" {
			if _, err := io.WriteString(output, text); err != nil {
				return 2, err
			}
		}
	}
	if err := flush(); err != nil {
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

// fsWatchOutputBuffer는 터미널이 아닌 출력에서 기록을 모아 두는 크기다.
const fsWatchOutputBuffer = 64 << 10

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

func writeFSWatchRecord(writer io.Writer, record fsWatchRecord, jsonOutput bool, style textStyle) error {
	if jsonOutput {
		if record.Type == "summary" {
			summary := map[string]interface{}{"type": record.Type, "time": record.Time, "root": record.Root, "events": record.Events, "actions": record.Actions, "failed": record.Failed, "duration_ms": record.DurationMS, "stop_reason": record.StopReason, "incomplete": record.Incomplete}
			if record.Error != "" {
				summary["error"] = record.Error
			}
			for key, count := range map[string]int{"overflows": record.Overflows, "dropped": record.Dropped, "rescans": record.Rescans} {
				if count > 0 {
					summary[key] = count
				}
			}
			return json.NewEncoder(writer).Encode(summary)
		}
		return json.NewEncoder(writer).Encode(record)
	}
	stamp := style.dim(record.Time.Local().Format("15:04:05.000"))
	switch record.Type {
	case "ready":
		line := T("watchfs.ready", terminalJSON(record.Root), record.Recursive, record.Driver)
		if record.DriverReason != "" {
			line = T("watchfs.ready_reason", terminalJSON(record.Root), record.Recursive, record.Driver, T("watchfs.driver_reason."+record.DriverReason))
		}
		_, err := fmt.Fprintln(writer, stamp+"  "+line)
		return err
	case "event":
		kind := style.paint(fmt.Sprintf("%-7s", strings.ToUpper(record.Event)), fsWatchEventColor(record.Event))
		line := stamp + "  " + kind + "  " + terminalJSON(record.Path)
		if record.Count > 1 {
			line += "  " + style.dim(fmt.Sprintf("×%d", record.Count))
		}
		if record.Origin != "" {
			line += "  " + style.dim("← "+terminalJSON(record.Origin))
		}
		if record.Process != "" {
			line += "  " + style.dim(T("watchfs.by", execPrintable([]byte(record.Process))))
		}
		_, err := fmt.Fprintln(writer, line)
		return err
	case "action_start":
		line := stamp + "  " + style.paint("START", richBlue) + "    " + style.join(" · ", terminalJSON(record.Rule), terminalJSON(record.Path), style.command(terminalJSON(strings.Join(record.Command, " "))))
		_, err := fmt.Fprintln(writer, line)
		return err
	case "action_result":
		exit := "—"
		if record.ExitCode != nil {
			exit = fmt.Sprint(*record.ExitCode)
			if *record.ExitCode != 0 {
				exit = style.bad(exit)
			}
		}
		color := fsWatchActionColor(record.Status)
		line := stamp + "  " + style.paint("DONE", color) + "     " + style.join(" · ", terminalJSON(record.Rule), style.paint(record.Status, color), "exit "+exit, fmt.Sprintf("%dms", record.DurationMS))
		if _, err := fmt.Fprintln(writer, line); err != nil {
			return err
		}
		if record.Output != "" {
			for _, line := range strings.Split(strings.TrimSuffix(record.Output, "\n"), "\n") {
				if _, err := fmt.Fprintln(writer, style.dim("          "+terminalJSON(record.Rule)+" |")+" "+terminalJSON(line)); err != nil {
					return err
				}
			}
		}
		if record.OutputTruncated {
			if _, err := fmt.Fprintln(writer, style.warn(T("watchfs.output_truncated", fsWatchOutputLimit))); err != nil {
				return err
			}
		}
		if record.Error != "" {
			_, err := fmt.Fprintln(writer, "          "+style.bad(terminalJSON(record.Error)))
			return err
		}
	case "summary":
		line := T("watchfs.summary", record.Events, record.Actions, record.Failed, fsWatchStopLabel(record.StopReason))
		if record.Incomplete {
			line += " · " + style.warn(fsWatchIncompleteText(record))
		}
		_, err := fmt.Fprintf(writer, "%s  %s\n", stamp, line)
		return err
	}
	return nil
}

func fsWatchEventColor(event string) string {
	switch event {
	case "create":
		return richGreen
	case "modify":
		return richCyan
	case "remove":
		return richRed
	case "rename":
		return richYellow
	}
	return ""
}

func fsWatchActionColor(status string) string {
	switch status {
	case "success":
		return richGreen
	case "failed", "timeout":
		return richRed
	case "cancelled":
		return richYellow
	}
	return richGray
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

// fsWatchIncompleteText는 요약 줄에 붙는 놓친 변경의 설명이다. 0인 항목은 뺀다.
func fsWatchIncompleteText(record fsWatchRecord) string {
	var parts []string
	for _, item := range []struct {
		key   string
		count int
	}{{"watchfs.incomplete_overflows", record.Overflows}, {"watchfs.incomplete_dropped", record.Dropped}, {"watchfs.incomplete_rescans", record.Rescans}} {
		if item.count > 0 {
			parts = append(parts, T(item.key, item.count))
		}
	}
	return T("watchfs.incomplete", strings.Join(parts, ", "))
}

// fsWatchEventRecord는 원본 이벤트를 기록으로 바꾼다. 일으킨 process는 JSON에만 싣는다. 묶지 않은 텍스트 출력은 이전과 같게 둔다.
func fsWatchEventRecord(event fsWatchEvent, withProcess bool) fsWatchRecord {
	record := fsWatchRecord{Type: "event", Time: event.Time, Event: event.Event, Path: event.Path, IsDir: event.IsDir}
	if withProcess {
		record.PID, record.Process = event.PID, event.Process
	}
	return record
}
