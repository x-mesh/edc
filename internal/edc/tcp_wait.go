package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	// tcpWaitAttemptTimeout는 -w에서 시도 한 번의 기본 시간 제한이다. SYN을 버리는 방화벽 뒤의 대상도 15초짜리 시도 한 번으로
	// 끝나지 않고 여러 번 시도하도록 짧게 둔다. --timeout으로 바꾼다.
	tcpWaitAttemptTimeout = 3 * time.Second
	// tcpWaitInterval은 시도를 시작하는 간격이다. 시도가 이보다 오래 걸리면 바로 다음 시도를 한다.
	tcpWaitInterval = time.Second
)

// tcpWaitLimit는 -w 값이다. 값 없이 쓰면 연결될 때까지 기다리고, -w=5m이면 그만큼만 기다린다. -w=0도 끝없이 기다린다.
type tcpWaitLimit struct {
	on    bool
	limit time.Duration
}

func (wait *tcpWaitLimit) String() string {
	if wait == nil || !wait.on {
		return ""
	}
	return wait.limit.String()
}

func (wait *tcpWaitLimit) Set(value string) error {
	switch value {
	case "true":
		*wait = tcpWaitLimit{on: true}
		return nil
	case "false":
		*wait = tcpWaitLimit{}
		return nil
	}
	limit, err := parseFlagDuration(value)
	if err != nil || limit < 0 {
		return errors.New(T("observe.tcp_wait.value_invalid", value))
	}
	*wait = tcpWaitLimit{on: true, limit: limit}
	return nil
}

// IsBoolFlag가 있어야 flag package가 값 없는 -w를 받는다. 그래서 시간은 -w=5m처럼 붙여 써야 한다.
func (wait *tcpWaitLimit) IsBoolFlag() bool { return true }

// runTCPWait는 tcp check -w다. 연결될 때까지 tcpWaitInterval마다 다시 시도하고, 끝나면 마지막 시도를 보통 check 결과처럼
// 출력한다. 연결되면 0, 기다린 시간이 지나면 1, Ctrl-C면 4로 끝난다.
func runTCPWait(args []string, version string) int {
	options := configuredCommon(tcpWaitAttemptTimeout)
	var wait tcpWaitLimit
	set := newFlagSet("tcp check")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	set.Var(&wait, "w", T("command.tcp.option.wait"))
	set.Var(&wait, "wait", T("command.tcp.option.wait"))
	positional, err := parseInterspersed(set, args)
	if err != nil {
		return 2
	}
	input := ""
	switch len(positional) {
	case 1:
		input = positional[0]
	case 0:
		values, ok := promptMissingArgs("edc tcp check", T("cli.prompt.host_port"))
		if !ok {
			fmt.Fprintln(os.Stderr, T("cli.error.one_target_required", "tcp check"))
			return 2
		}
		input = values[0]
	default:
		// IsBoolFlag인 -w는 뒤의 낱말을 값으로 받지 않는다. -w 5m의 5m가 대상 하나로 읽힌 것이면 = 쓰는 법을 알린다.
		if _, err := parseFlagDuration(positional[0]); err == nil && len(positional) == 2 {
			fmt.Fprintln(os.Stderr, T("observe.tcp_wait.value_separate", positional[0]))
			return 2
		}
		fmt.Fprintln(os.Stderr, T("cli.error.one_target_required", "tcp check"))
		return 2
	}
	started := time.Now()
	targetInfo := map[string]interface{}{"input": input}
	if _, _, err := net.SplitHostPort(input); err != nil {
		failed := resultFromError("tcp.check", started, "input", errors.New(T("cli.error.host_port_required", input)))
		return emit(options, buildReport(version, started, targetInfo, []Result{failed}, options.redact))
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx := signals
	if wait.limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(signals, wait.limit)
		defer cancel()
	}
	progress := newTCPWaitProgress(input, options)
	result := waitTCP(ctx, input, options.timeout, tcpWaitInterval, progress.attempted)
	progress.finish()
	code := emit(options, buildReport(version, started, targetInfo, []Result{result}, options.redact))
	if signals.Err() != nil {
		fmt.Fprintln(os.Stderr, errProbeCancelled)
		return 4
	}
	return code
}

// waitTCP는 address에 연결될 때까지 시도한다. 시도는 interval마다 시작하고, 한 번은 attemptTimeout까지 기다린다. ctx가
// 끝나면 마지막 실패를 돌려준다. ctx가 시도 도중에 끝나면 그 시도는 원인 대신 취소를 적으므로, 그 앞 시도의 실패를 쓴다.
func waitTCP(ctx context.Context, address string, attemptTimeout, interval time.Duration, attempted func(int, Result)) Result {
	started := time.Now()
	var last Result
	for attempt := 1; ; attempt++ {
		attemptStarted := time.Now()
		dial, cancel := context.WithTimeout(ctx, attemptTimeout)
		result := probeTCP(dial, address)
		cancel()
		if result.Status != StatusFail {
			attempted(attempt, result)
			return tcpWaitResult(result, started, attempt)
		}
		if ctx.Err() != nil && attempt > 1 {
			return tcpWaitResult(last, started, attempt-1)
		}
		attempted(attempt, result)
		last = result
		if ctx.Err() != nil {
			return tcpWaitResult(last, started, attempt)
		}
		select {
		case <-ctx.Done():
			return tcpWaitResult(last, started, attempt)
		case <-time.After(time.Until(attemptStarted.Add(interval))):
		}
	}
}

// tcpWaitResult는 마지막 시도에 기다린 시간과 시도 수를 붙인다. 시작 시각과 걸린 시간은 기다리기 시작한 때부터다.
func tcpWaitResult(result Result, started time.Time, attempts int) Result {
	waited := time.Since(started)
	result.StartedAt = started.UTC()
	result.DurationMS = waited.Milliseconds()
	key := "observe.tcp_wait.connected"
	if result.Status == StatusFail {
		key = "observe.tcp_wait.not_connected"
	}
	result.Summary += " · " + T(key, tcpWaitDuration(waited), attempts)
	if result.Metrics == nil {
		result.Metrics = map[string]interface{}{}
	}
	result.Metrics["attempts"] = attempts
	result.Metrics["waited_ms"] = waited.Milliseconds()
	return result
}

func tcpWaitDuration(waited time.Duration) string {
	if waited < time.Second {
		return waited.Round(time.Millisecond).String()
	}
	return waited.Round(time.Second).String()
}

// tcpWaitProgress는 기다리는 동안의 진행이다. 터미널에서는 한 줄을 고쳐 쓰고, pipe나 --json에서는 실패 원인이 바뀔 때만
// stderr에 한 줄 쓴다. 시도마다 쓰면 몇 분 기다린 로그가 시도 수만큼 쌓인다.
type tcpWaitProgress struct {
	out     io.Writer
	live    bool
	color   bool
	address string
	started time.Time
	cause   string
	drawn   bool
}

func newTCPWaitProgress(address string, options commonOptions) *tcpWaitProgress {
	progress := &tcpWaitProgress{out: os.Stderr, address: address, started: time.Now()}
	if options.jsonPath == "" && isTerminal(os.Stdout) {
		progress.out, progress.live, progress.color = os.Stdout, true, richEnabled(os.Stdout)
	}
	return progress
}

func (progress *tcpWaitProgress) attempted(attempt int, result Result) {
	cause := tcpWaitCause(result)
	if progress.live {
		line := T("observe.tcp_wait.waiting", progress.address, tcpWaitDuration(time.Since(progress.started)), attempt, cause)
		if progress.color {
			line = richDim(line)
		}
		fmt.Fprint(progress.out, "\r\033[K"+line)
		progress.drawn = true
		return
	}
	if cause != progress.cause {
		fmt.Fprintln(progress.out, T("observe.tcp_wait.waiting_line", progress.address, attempt, cause))
	}
	progress.cause = cause
}

// finish는 고쳐 쓰던 줄을 지운다. 그 자리에 보통 check 결과가 나온다.
func (progress *tcpWaitProgress) finish() {
	if progress.live && progress.drawn {
		fmt.Fprint(progress.out, "\r\033[K")
	}
}

// tcpWaitCause는 진행 줄에 쓰는 짧은 원인이다. 연결되면 connected, 실패하면 오류 종류(connection_refused 등)를 낱말로 쓴다.
func tcpWaitCause(result Result) string {
	if result.Status != StatusFail {
		return "connected"
	}
	if result.Error != nil && result.Error.Kind != "" {
		return strings.ReplaceAll(result.Error.Kind, "_", " ")
	}
	return "failed"
}
