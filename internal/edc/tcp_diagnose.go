package edc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	tcpDiagnoseDefaultCount    = 5
	tcpDiagnoseMaxCount        = 100
	tcpDiagnoseDefaultInterval = time.Second
	tcpDiagnoseMaxInterval     = time.Minute
	// tcpDiagnoseSettle는 시도가 끝난 뒤 그 시도의 RST, drop, socket 해제 event를 ring에서 읽을 때까지 기다리는 시간이다.
	// 이만큼 기다린 뒤에 시도 줄을 그려야 줄의 재전송 수와 마지막 요약이 같다.
	tcpDiagnoseSettle = 300 * time.Millisecond

	diagnoseRowRoute = "route"
	diagnoseRowTrace = "trace"
)

type tcpDiagnoseOptions struct {
	common   commonOptions
	count    int
	interval time.Duration
}

func (options tcpDiagnoseOptions) check() error {
	if options.count < 1 || options.count > tcpDiagnoseMaxCount {
		return errors.New(T("diagnose.error.count_range", tcpDiagnoseMaxCount))
	}
	if options.interval < 0 || options.interval > tcpDiagnoseMaxInterval {
		return errors.New(T("diagnose.error.interval_range", tcpDiagnoseMaxInterval))
	}
	if options.common.timeout <= 0 {
		return errors.New(T("diagnose.error.timeout_range"))
	}
	return nil
}

// runTCPDiagnose는 tcp check --diagnose다. --timeout은 시도 하나의 connect 제한이다. 전체 시간은
// count×timeout + (count-1)×interval에 trace를 붙이는 시간을 더한 값을 넘지 않는다.
func runTCPDiagnose(args []string, version string) int {
	options := tcpDiagnoseOptions{common: configuredCommon(15 * time.Second), count: tcpDiagnoseDefaultCount, interval: tcpDiagnoseDefaultInterval}
	set := newFlagSet("tcp check")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options.common)
	// runTCP가 이 flag를 보고 여기로 넘긴다. 값은 쓰지 않지만 정의해야 parse가 받는다.
	set.Bool("diagnose", true, T("command.tcp.option.diagnose"))
	set.IntVar(&options.count, "count", options.count, T("command.tcp.option.count"))
	set.DurationVar(&options.interval, "interval", options.interval, T("command.tcp.option.interval"))
	positional, err := parseInterspersed(set, args)
	if err != nil {
		return 2
	}
	if err := options.check(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	input := ""
	switch {
	case len(positional) == 1:
		input = positional[0]
	case len(positional) == 0:
		values, ok := promptMissingArgs("edc tcp check", T("cli.prompt.host_port"))
		if !ok {
			fmt.Fprintln(os.Stderr, T("cli.error.one_target_required", "tcp check"))
			return 2
		}
		input = values[0]
	default:
		fmt.Fprintln(os.Stderr, T("cli.error.one_target_required", "tcp check"))
		return 2
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signals)
	defer cancel()
	started := time.Now()
	targetInfo := map[string]interface{}{"input": input}
	target, failed := resolveDiagnoseTarget(ctx, input, options.common.timeout)
	if failed != nil {
		return finishTCPDiagnose(ctx, options, buildReport(version, started, targetInfo, []Result{*failed}, options.common.redact), isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "", nil)
	}
	if options.common.jsonPath == "" && liveTerminal() {
		if code, ok := runTCPDiagnoseLive(ctx, cancel, target, options, version, started, targetInfo); ok {
			return code
		}
	}
	color := options.common.jsonPath == "" && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	show := func(Result) {}
	if options.common.jsonPath == "" {
		show = func(row Result) { fmt.Fprint(os.Stdout, diagnoseRowLine(row, color, options.common.redact)) }
	}
	result := diagnoseTCP(ctx, target, options, show)
	return finishTCPDiagnose(ctx, options, buildReport(version, started, targetInfo, []Result{result}, options.common.redact), color, show)
}

// runTCPDiagnoseLive는 doctor처럼 줄마다 spinner를 돌리다 끝난 줄을 고쳐 쓴다. 화면을 열지 못하면 false를 돌려
// caller가 줄 출력으로 내려가게 한다.
func runTCPDiagnoseLive(ctx context.Context, cancel context.CancelFunc, target diagnoseTarget, options tcpDiagnoseOptions, version string, started time.Time, targetInfo map[string]interface{}) (int, bool) {
	names := []string{diagnoseRowRoute}
	for index := 1; index <= options.count; index++ {
		names = append(names, diagnoseAttemptLabel(index, options.count))
	}
	names = append(names, diagnoseRowTrace, tcpDiagnoseProbeID)
	model := newDoctorModel(target.input, names, true, options.common.redact, cancel)
	live, err := startLiveProgram(model, cancel, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.live.start_failed", err))
		return 0, false
	}
	show := func(row Result) { live.send(doctorResultMsg{name: row.Probe, result: row}) }
	result := diagnoseTCP(ctx, target, options, show)
	show(withoutEvidence(result))
	// finish가 Run을 끝내면 onExit이 cancel을 부르므로, 사용자 취소 여부는 그 전에 읽는다.
	cancelled := ctx.Err() != nil
	if _, err := live.finish(doctorDoneMsg{}); err != nil {
		fmt.Fprintln(os.Stderr, T("observe.live.exit_error", err))
	}
	report := buildReport(version, started, targetInfo, []Result{result}, options.common.redact)
	printResultTail(os.Stdout, []Result{withoutEvidence(report.Results[0])}, options.common.verbose, true)
	if cancelled {
		fmt.Fprintln(os.Stderr, errProbeCancelled)
		return 4, true
	}
	return exitCode(report.Results), true
}

// finishTCPDiagnose는 줄 출력과 JSON 출력의 마무리다. 줄 출력은 시도 줄을 이미 그렸으므로 evidence를 빼고 결과를 쓴다.
func finishTCPDiagnose(ctx context.Context, options tcpDiagnoseOptions, report Report, color bool, show func(Result)) int {
	if options.common.jsonPath != "" {
		if err := writeJSONOutput(options.common.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else if show == nil {
		printTerminalWithColor(os.Stdout, report.Results, options.common.verbose, color)
	} else {
		results := []Result{withoutEvidence(report.Results[0])}
		fmt.Fprint(os.Stdout, formatResultLine(results[0], color))
		printResultTail(os.Stdout, results, options.common.verbose, color)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, errProbeCancelled)
		return 4
	}
	return exitCode(report.Results)
}

func withoutEvidence(result Result) Result {
	result.Evidence = nil
	return result
}

func diagnoseRowLine(row Result, color, redact bool) string {
	line := formatResultLine(row, color)
	if redact {
		line = redactIPAddresses(line)
	}
	return line
}

func diagnoseAttemptLabel(index, count int) string {
	return fmt.Sprintf("attempt %d/%d", index, count)
}

// resolveDiagnoseTarget은 대상을 주소 하나로 정한다. 모든 시도와 경로, trace 필터가 같은 주소를 봐야 event를 시도에
// 붙일 수 있다. 실패하면 tcp check와 같은 phase로 끝난 결과를 돌려준다.
func resolveDiagnoseTarget(ctx context.Context, input string, timeout time.Duration) (diagnoseTarget, *Result) {
	started := time.Now()
	host, port, err := net.SplitHostPort(input)
	if err != nil {
		result := resultFromError(tcpDiagnoseProbeID, started, "input", errors.New(T("cli.error.host_port_required", input)))
		return diagnoseTarget{}, &result
	}
	lookup, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	address, err := resolveRouteAddress(lookup, host)
	if err != nil {
		result := resultFromError(tcpDiagnoseProbeID, started, "dns", err)
		result.Next = diagnoseNext(diagnoseVerdictDNS, diagnoseTarget{host: host}, diagnoseTotals{})
		result.Metrics = map[string]interface{}{"input": input, "verdict": diagnoseVerdictDNS}
		return diagnoseTarget{}, &result
	}
	return diagnoseTarget{input: input, host: host, port: port, address: net.JoinHostPort(address, port)}, nil
}

// diagnoseTCP는 경로를 보고, trace를 붙인 채 연결을 count번 시도한 뒤 판정한다. show는 줄이 끝날 때마다 불린다.
func diagnoseTCP(ctx context.Context, target diagnoseTarget, options tcpDiagnoseOptions, show func(Result)) Result {
	started := time.Now()
	routeRow, routeMetrics := diagnoseRoute(ctx, target.address, options.common.timeout)
	target.source, _ = routeMetrics["source"].(string)
	show(routeRow)
	trace := startDiagnoseTrace(ctx, target.address)
	defer trace.finish()
	traced := trace.tcpTraced() || trace.dropTraced()
	var attempts []diagnoseAttempt
	for index := 1; index <= options.count && ctx.Err() == nil; index++ {
		attempt := diagnoseDial(ctx, target.address, options.common.timeout, index)
		// Ctrl-C로 끊긴 시도는 대상의 응답이 아니므로 세지 않는다.
		if ctx.Err() != nil && attempt.Outcome != diagnoseOutcomeConnected {
			break
		}
		attempts = append(attempts, attempt)
		waited := time.Duration(0)
		if traced {
			waited = tcpDiagnoseSettle
			sleepContext(ctx, waited)
		}
		current := correlateDiagnoseEvents(attempts, trace.snapshot(), target.address)
		show(diagnoseAttemptRow(current[len(current)-1], options, trace.tcpTraced()))
		if index < options.count && options.interval > waited {
			sleepContext(ctx, options.interval-waited)
		}
	}
	trace.finish()
	facts := diagnoseFacts{attempts: correlateDiagnoseEvents(attempts, trace.snapshot(), target.address), tcpTraced: trace.tcpTraced(), dropTraced: trace.dropTraced()}
	totals := facts.totals()
	show(diagnoseTraceRow(trace, totals))
	return diagnoseResult(started, target, options, routeRow, routeMetrics, trace, facts, totals)
}

func sleepContext(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// diagnoseRoute는 net route와 같은 경로 조회에 source 주소를 더한다. ip route get의 src는 Linux에만 있으므로, 어느 OS에서나
// 같은 값을 얻도록 UDP socket을 대상에 연결해 kernel이 고른 주소를 읽는다. packet은 보내지 않는다.
func diagnoseRoute(ctx context.Context, address string, timeout time.Duration) (Result, map[string]interface{}) {
	host, _, _ := net.SplitHostPort(address)
	lookup, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	route := probeRoute(lookup, host)
	metrics := map[string]interface{}{}
	for _, key := range []string{"destination", "gateway", "interface"} {
		if value, ok := route.Metrics[key]; ok {
			metrics[key] = value
		}
	}
	if connection, err := net.Dial("udp", address); err == nil {
		if local, ok := connection.LocalAddr().(*net.UDPAddr); ok {
			metrics["source"] = local.IP.String()
		}
		connection.Close()
	}
	row := Result{Probe: diagnoseRowRoute, Status: route.Status, StartedAt: route.StartedAt, DurationMS: route.DurationMS}
	if route.Status != StatusPass {
		// 경로를 읽지 못해도 연결 시도는 할 수 있으므로 진단을 멈추지 않는다.
		if route.Status == StatusFail {
			row.Status = StatusWarn
		}
		row.Summary = T("diagnose.route.failed", firstLine(route.Summary))
		metrics["error"] = firstLine(route.Summary)
		return row, metrics
	}
	var parts []string
	if value, ok := metrics["interface"].(string); ok {
		parts = append(parts, "dev "+value)
	}
	if value, ok := metrics["gateway"].(string); ok {
		parts = append(parts, "via "+value)
	}
	if value, ok := metrics["source"].(string); ok {
		parts = append(parts, "src "+value)
	}
	row.Summary = strings.Join(parts, ", ")
	if row.Summary == "" {
		row.Summary = firstLine(route.Summary)
	}
	return row, metrics
}

// diagnoseDial은 tcp check와 같은 dialer로 한 번 연결한다. connect 전에 port 0으로 bind해 local port를 먼저 정하므로,
// 응답이 없거나 거부된 시도의 SYN, RST, drop event도 port로 이 시도에 붙일 수 있다.
func diagnoseDial(ctx context.Context, address string, timeout time.Duration, index int) diagnoseAttempt {
	attempt := diagnoseAttempt{Index: index}
	dialer := &net.Dialer{Control: func(network, _ string, raw syscall.RawConn) error {
		port, err := bindEphemeralPort(network, raw)
		attempt.LocalPort = port
		return err
	}}
	started := time.Now()
	dial, cancel := context.WithTimeout(ctx, timeout)
	connection, err := dialer.DialContext(dial, "tcp", address)
	cancel()
	attempt.elapsed = time.Since(started)
	attempt.ConnectMS = attempt.elapsed.Milliseconds()
	if err != nil {
		attempt.Phase = classifyNetworkError(err)
		attempt.Cause = err.Error()
	} else {
		connection.Close()
	}
	attempt.Outcome = diagnoseOutcome(attempt.Phase)
	return attempt
}

func bindEphemeralPort(network string, raw syscall.RawConn) (int, error) {
	var port int
	var bindErr error
	err := raw.Control(func(fd uintptr) {
		var unspecified syscall.Sockaddr = &syscall.SockaddrInet4{}
		if network == "tcp6" {
			unspecified = &syscall.SockaddrInet6{}
		}
		if bindErr = syscall.Bind(int(fd), unspecified); bindErr != nil {
			return
		}
		bound, err := syscall.Getsockname(int(fd))
		if err != nil {
			bindErr = err
			return
		}
		switch value := bound.(type) {
		case *syscall.SockaddrInet4:
			port = value.Port
		case *syscall.SockaddrInet6:
			port = value.Port
		}
	})
	if err != nil {
		return 0, err
	}
	return port, bindErr
}

func diagnoseAttemptRow(attempt diagnoseAttempt, options tcpDiagnoseOptions, traced bool) Result {
	return Result{
		Probe: diagnoseAttemptLabel(attempt.Index, options.count), Status: diagnoseAttemptStatus(attempt),
		DurationMS: attempt.ConnectMS, Summary: diagnoseAttemptSummary(attempt, options.common.timeout, traced),
	}
}

func diagnoseTraceRow(trace *diagnoseTraceRun, totals diagnoseTotals) Result {
	row := Result{Probe: diagnoseRowTrace, Status: StatusPass}
	if !trace.tcpTraced() && !trace.dropTraced() {
		row.Status = StatusSkip
		row.Summary = T("diagnose.trace.skipped", trace.tcpSkip)
		return row
	}
	var parts []string
	if trace.tcpTraced() {
		parts = append(parts, T("diagnose.trace.tcp_counts", totals.SYNRetransmits, totals.Retransmits, totals.ResetsReceived, totals.ResetsSent))
	} else {
		parts = append(parts, T("diagnose.trace.tcp_skipped"))
	}
	if trace.dropTraced() {
		parts = append(parts, T("diagnose.trace.drop_counts", totals.dropCount(), totals.otherDropCount()))
	} else {
		parts = append(parts, T("diagnose.trace.drop_skipped"))
	}
	if len(trace.warnings) > 0 || trace.lost > 0 || !trace.tcpTraced() || !trace.dropTraced() {
		row.Status = StatusWarn
	}
	row.Summary = strings.Join(parts, ", ")
	return row
}

func diagnoseResult(started time.Time, target diagnoseTarget, options tcpDiagnoseOptions, routeRow Result, routeMetrics map[string]interface{}, trace *diagnoseTraceRun, facts diagnoseFacts, totals diagnoseTotals) Result {
	code, status := diagnoseVerdict(facts)
	message := diagnoseVerdictMessage(code, facts, totals)
	result := Result{
		Probe: tcpDiagnoseProbeID, Status: status, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(),
		Summary: message, Next: diagnoseNext(code, target, totals),
	}
	if status == StatusFail {
		result.Error = &DiagnosticError{Kind: code, Message: message}
	}
	switch facts.basis() {
	case diagnoseBasisPhases:
		result.Warnings = append(result.Warnings, T("diagnose.basis.phases", trace.tcpSkip))
	case diagnoseBasisPartial:
		reason := trace.tcpSkip
		if reason == "" {
			reason = trace.dropSkip
		}
		result.Warnings = append(result.Warnings, T("diagnose.basis.partial", reason))
	}
	result.Warnings = append(result.Warnings, trace.warnings...)
	if trace.lost > 0 {
		result.Warnings = append(result.Warnings, T("diagnose.trace.lost", trace.lost))
	}
	traceMetrics := map[string]interface{}{"tcp": diagnoseTraceState(trace.tcpSkip), "drop": diagnoseTraceState(trace.dropSkip), "lost_events": trace.lost}
	if trace.tcpSkip != "" {
		traceMetrics["tcp_skip_reason"] = trace.tcpSkip
	}
	if trace.dropSkip != "" {
		traceMetrics["drop_skip_reason"] = trace.dropSkip
	}
	attempts, sums := diagnoseMetrics(facts, totals)
	result.Metrics = map[string]interface{}{
		"input": target.input, "address": target.address, "count": options.count,
		"interval_ms": options.interval.Milliseconds(), "timeout_ms": options.common.timeout.Milliseconds(),
		"route": routeMetrics, "attempts": attempts, "totals": sums,
		"trace": traceMetrics, "verdict": code, "basis": facts.basis(),
	}
	if facts.dropTraced {
		result.Metrics["drops"] = totals.Drops
	}
	lines := make([]string, 0, len(facts.attempts))
	for _, attempt := range facts.attempts {
		lines = append(lines, fmt.Sprintf("%-6s %s", fmt.Sprintf("#%d", attempt.Index), diagnoseAttemptSummary(attempt, options.common.timeout, facts.tcpTraced)))
	}
	result.Evidence = []Evidence{{Label: diagnoseRowRoute, Value: routeRow.Summary}}
	if len(lines) > 0 {
		result.Evidence = append(result.Evidence, Evidence{Label: T("diagnose.evidence.attempts"), Value: strings.Join(lines, "\n")})
	}
	result.Evidence = append(result.Evidence, Evidence{Label: diagnoseRowTrace, Value: diagnoseTraceRow(trace, totals).Summary})
	return result
}

func diagnoseTraceState(skip string) string {
	if skip != "" {
		return "skip"
	}
	return "on"
}
