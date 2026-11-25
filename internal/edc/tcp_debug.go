package edc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// tcpHandshake는 연결 직후 커널이 알려 주는 TCP 상태다. 패킷을 보지 않고도 SYN-ACK에서 합의한 값을 알 수 있다.
// SendScale, ReceiveScale이 -1이면 값을 모른다(Linux).
type tcpHandshake struct {
	RTT, RTTVar, RTO                   time.Duration
	MSS                                int
	SACK, Timestamps, WindowScale, ECN bool
	SendScale, ReceiveScale            int
	Retransmits                        int
}

var errTCPInfoUnavailable = errors.New("TCP info is not available for this connection")

// debugFlag는 --debug 값이다. 값 없이 쓰거나 true면 과정과 프로토콜 해석을, raw면 원본 바이트까지 보인다.
type debugFlag struct {
	on  bool
	raw bool
}

func (value *debugFlag) String() string {
	switch {
	case value == nil || !value.on:
		return "false"
	case value.raw:
		return "raw"
	}
	return "true"
}

func (value *debugFlag) Set(text string) error {
	switch text {
	case "true":
		*value = debugFlag{on: true}
	case "false":
		*value = debugFlag{}
	case "raw":
		*value = debugFlag{on: true, raw: true}
	default:
		return errors.New(T("cli.error.debug_value", text))
	}
	return nil
}

func (value *debugFlag) IsBoolFlag() bool { return true }

const (
	// debugCaptureLimit은 탐침 한 연결에서 방향마다 남기는 바이트의 상한이다.
	debugCaptureLimit = 4096
	// debugHexLimit은 --debug=raw가 방향마다 보이는 바이트의 상한이다.
	debugHexLimit = 256
	// debugTextLimit은 기록 한 줄의 글자 상한이다. 대상 서버가 정한 글자가 들어가므로 길이를 묶는다.
	debugTextLimit = 200
)

// identifyRecorder는 판별 단계마다 연결과 송수신을 기록한다. nil이면 아무것도 기록하지 않는다. h2c 탐침은 http.Transport가
// 다른 goroutine에서 읽으므로 mutex로 묶는다.
type identifyRecorder struct {
	started time.Time
	mutex   sync.Mutex
	steps   []*debugStep
	skipped []string
}

type debugStep struct {
	name      string
	hinted    bool
	start     time.Duration
	end       time.Duration
	exchanges []*debugExchange
	notes     []string
	matched   string
}

// debugExchange는 탐침 연결 하나다. label은 같은 단계 안의 다른 연결(TLS 안쪽, Host를 넣은 재시도)을 가린다.
type debugExchange struct {
	label         string
	reused        bool
	opened        time.Duration
	dial          time.Duration
	dialErr       string
	sent          []byte
	received      []byte
	sentTotal     int
	receivedTotal int
	firstSend     time.Duration
	firstReceive  time.Duration
	sentAny       bool
	receivedAny   bool
	end           string
	endAt         time.Duration
}

func newIdentifyRecorder(started time.Time) *identifyRecorder {
	return &identifyRecorder{started: started}
}

func (recorder *identifyRecorder) now() time.Duration { return time.Since(recorder.started) }

func (recorder *identifyRecorder) begin(name string, hinted bool) *debugStep {
	if recorder == nil {
		return nil
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	step := &debugStep{name: name, hinted: hinted, start: recorder.now()}
	recorder.steps = append(recorder.steps, step)
	return step
}

func (recorder *identifyRecorder) finish(step *debugStep, matched string) {
	if recorder == nil || step == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	step.end, step.matched = recorder.now(), matched
}

func (recorder *identifyRecorder) skip(names []string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.skipped = append(recorder.skipped, names...)
}

// current는 지금 진행 중인 단계다. 단계가 없으면 nil이다.
func (recorder *identifyRecorder) current() *debugStep {
	if len(recorder.steps) == 0 {
		return nil
	}
	return recorder.steps[len(recorder.steps)-1]
}

func (recorder *identifyRecorder) note(text string) {
	if recorder == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if step := recorder.current(); step != nil {
		step.notes = append(step.notes, text)
	}
}

func (recorder *identifyRecorder) exchange(label string, reused bool, dial time.Duration, dialErr error) *debugExchange {
	if recorder == nil {
		return nil
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	exchange := &debugExchange{label: label, reused: reused, dial: dial, opened: recorder.now()}
	if dialErr != nil {
		exchange.dialErr = dialErr.Error()
	}
	if step := recorder.current(); step != nil {
		step.exchanges = append(step.exchanges, exchange)
	}
	return exchange
}

func (recorder *identifyRecorder) recordSend(exchange *debugExchange, data []byte) {
	if recorder == nil || exchange == nil || len(data) == 0 {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if !exchange.sentAny {
		exchange.sentAny, exchange.firstSend = true, recorder.now()
	}
	exchange.sentTotal += len(data)
	exchange.sent = appendCapture(exchange.sent, data)
}

func (recorder *identifyRecorder) recordReceive(exchange *debugExchange, data []byte, err error) {
	if recorder == nil || exchange == nil {
		return
	}
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if len(data) > 0 {
		if !exchange.receivedAny {
			exchange.receivedAny, exchange.firstReceive = true, recorder.now()
		}
		exchange.receivedTotal += len(data)
		exchange.received = appendCapture(exchange.received, data)
	}
	if err == nil || exchange.end != "" {
		return
	}
	var timeout net.Error
	switch {
	case errors.Is(err, io.EOF):
		exchange.end = "closed"
	case errors.As(err, &timeout) && timeout.Timeout():
		exchange.end = "waited"
	case errors.Is(err, syscall.ECONNRESET):
		exchange.end = "reset"
	default:
		exchange.end = "error: " + err.Error()
	}
	exchange.endAt = recorder.now()
}

func appendCapture(buffer, data []byte) []byte {
	if room := debugCaptureLimit - len(buffer); room > 0 {
		if len(data) > room {
			data = data[:room]
		}
		buffer = append(buffer, data...)
	}
	return buffer
}

// probeConn은 탐침 연결이다. 읽기가 답 없이 시간을 다 썼는지(waited)를 남기고, --debug면 송수신을 기록한다.
type probeConn struct {
	net.Conn
	waited   *atomic.Bool
	recorder *identifyRecorder
	exchange *debugExchange
}

func (connection *probeConn) Write(buffer []byte) (int, error) {
	count, err := connection.Conn.Write(buffer)
	connection.recorder.recordSend(connection.exchange, buffer[:count])
	return count, err
}

func (connection *probeConn) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	var timeout net.Error
	if connection.waited != nil && count == 0 && errors.As(err, &timeout) && timeout.Timeout() {
		connection.waited.Store(true)
	}
	connection.recorder.recordReceive(connection.exchange, buffer[:count], err)
	return count, err
}

// checkTCPWithProtocol은 tcp check 한 번이다. 연결하고, 판별하고, --debug면 과정을 결과에 남긴다. --debug의 이름 조회는
// 판별과 따로 한 번 더 한다. 연결은 Go의 resolver가 안에서 조회하므로 그 결과를 꺼낼 수 없기 때문이다.
func checkTCPWithProtocol(ctx context.Context, target string, identify identifyFlag, debug debugFlag, verbose bool) Result {
	started := time.Now()
	host, portText, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portText)
	connect := debugConnect{host: host}
	if debug.on && net.ParseIP(host) == nil {
		lookupStarted := time.Now()
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		connect.lookedUp, connect.lookup = true, time.Since(lookupStarted)
		if err != nil {
			connect.lookupErr = err.Error()
		}
		for _, address := range addresses {
			connect.addresses = append(connect.addresses, address.IP.String())
		}
	}
	connect.connectAt = time.Since(started)
	var inspect func(net.Conn, time.Duration)
	if debug.on {
		inspect = func(connection net.Conn, duration time.Duration) {
			connect.connect, connect.local, connect.remote = duration, connection.LocalAddr().String(), connection.RemoteAddr().String()
			handshake, err := readTCPHandshake(connection)
			connect.handshake, connect.handshakeOK = handshake, err == nil
		}
	}
	result := probeTCPInspect(ctx, target, inspect)
	if result.Error != nil {
		if debug.on {
			connect.connectErr = result.Error.Message
			result.Debug = tcpDebugLines(connect, nil, nil, port, identifyOptions{}, debug.raw)
		}
		return result
	}
	options := identifyOptions{limit: identify.limit, patient: identify.limit > 0}
	if !identify.on {
		if debug.on {
			result.Debug = tcpDebugLines(connect, nil, nil, port, options, debug.raw)
		}
		return result
	}
	if debug.on {
		options.recorder = newIdentifyRecorder(started)
	}
	identified := identifyProtocolWith(ctx, target, time.Duration(result.DurationMS)*time.Millisecond, options)
	result = annotateProtocol(result, identified, verbose)
	if debug.on {
		result.Debug = tcpDebugLines(connect, options.recorder, &identified, port, options, debug.raw)
	}
	return result
}

// debugConnect는 판별 앞의 이름 조회, 연결, 커널 TCP 정보다.
type debugConnect struct {
	host        string
	lookedUp    bool
	addresses   []string
	lookup      time.Duration
	lookupErr   string
	connectAt   time.Duration
	connect     time.Duration
	local       string
	remote      string
	connectErr  string
	handshake   tcpHandshake
	handshakeOK bool
}

// tcpDebugLines는 기록을 사람이 읽을 줄로 바꾼다. 맞은 단계와 raw일 때는 모든 단계를 펼치고, 나머지 단계는 한 줄로 접는다.
func tcpDebugLines(connect debugConnect, recorder *identifyRecorder, identified *protocolIdentification, port int, options identifyOptions, raw bool) []DebugLine {
	var lines []DebugLine
	add := func(at time.Duration, kind string, depth int, text string) {
		atMS := -1.0
		if at >= 0 {
			atMS = float64(at.Microseconds()) / 1000
		}
		lines = append(lines, DebugLine{AtMS: atMS, Kind: kind, Depth: depth, Text: debugText(text)})
	}
	if connect.lookedUp {
		if connect.lookupErr != "" {
			add(0, "dns", 0, fmt.Sprintf("%s: %s (%s)", connect.host, connect.lookupErr, debugDuration(connect.lookup)))
		} else {
			add(0, "dns", 0, fmt.Sprintf("%s → %s (lookup for debug, %s)", connect.host, strings.Join(connect.addresses, ", "), debugDuration(connect.lookup)))
		}
	}
	if connect.connectErr != "" {
		add(connect.connectAt, "connect", 0, "failed: "+connect.connectErr)
		return lines
	}
	add(connect.connectAt, "connect", 0, fmt.Sprintf("%s → %s · %s", connect.local, connect.remote, debugDuration(connect.connect)))
	if connect.handshakeOK {
		add(-1, "tcp", 0, handshakeText(connect.handshake))
	}
	// 첫 SYN 재전송은 Linux와 macOS 모두 1초 뒤에 간다. 연결이 그보다 오래 걸렸으면 재전송이 있었을 가능성이 크다.
	if connect.connect >= 900*time.Millisecond {
		add(-1, "tcp", 0, fmt.Sprintf("connect took %s: likely a SYN retransmit (inferred from the time)", debugDuration(connect.connect)))
	}
	if recorder == nil || identified == nil {
		return lines
	}
	add(-1, "identify", 0, identifyPlanText(recorder, port, options))
	for _, step := range recorder.steps {
		expand := raw || step.matched != ""
		if !expand {
			add(step.start, step.name, 0, compactStepText(step))
			continue
		}
		add(step.start, step.name, 0, expandedStepHeader(step))
		// raw는 연결마다의 바이트를 보려는 것이라 접지 않는다.
		if folded := foldedStepLines(step, raw); folded != nil {
			for _, line := range folded {
				add(-1, "", line.depth, line.text)
			}
		} else {
			for _, exchange := range step.exchanges {
				showConnection := len(step.exchanges) > 1
				for _, line := range exchangeLines(step.name, exchange, raw, showConnection) {
					add(-1, "", line.depth, line.text)
				}
			}
		}
		for _, note := range step.notes {
			add(-1, "", 1, note)
		}
	}
	if len(recorder.skipped) > 0 {
		add(-1, "stopped", 0, fmt.Sprintf("%s limit before %s", identified.Limit, strings.Join(recorder.skipped, ", ")))
	}
	// 맞은 단계 하나뿐이면 결과 줄은 그 단계 줄과 같은 말이다. 앞 단계가 기다린 시간이 있거나 unknown일 때만 쓴다.
	if identified.Name == protocolUnknown || len(recorder.steps) > 1 {
		add(-1, "result", 0, resultText(recorder, identified))
	}
	return lines
}

// foldedStepLines는 같은 모양의 연결이 반복된 단계를 접은 줄이다. raw이거나 접을 수 없으면 nil이다.
func foldedStepLines(step *debugStep, raw bool) []debugTextLine {
	if raw || step.name != protocolNameGoloopP2P {
		return nil
	}
	return foldGoloopStep(step)
}

func identifyPlanText(recorder *identifyRecorder, port int, options identifyOptions) string {
	if options.patient {
		return fmt.Sprintf("--identify=%s: every step waits up to %s", options.limit, identifyStageTimeout)
	}
	if len(recorder.steps) > 0 && recorder.steps[0].hinted {
		return fmt.Sprintf("port %d points to %s: send it first, no banner wait", port, recorder.steps[0].name)
	}
	return fmt.Sprintf("no hint for port %d: read a banner, then each step", port)
}

func expandedStepHeader(step *debugStep) string {
	duration := step.end - step.start
	if step.matched != "" {
		return fmt.Sprintf("matched %s · %s", step.matched, debugDuration(duration))
	}
	return fmt.Sprintf("%s · %s", stepOutcome(step), debugDuration(duration))
}

// compactStepText는 맞지 않은 단계의 한 줄이다. 무엇을 보냈고 서버가 어떻게 했는지만 쓴다.
func compactStepText(step *debugStep) string {
	request := debugRequestLabels[step.name]
	if request == "" {
		request = "nothing"
	}
	if step.name == "banner" {
		return stepOutcome(step)
	}
	text := fmt.Sprintf("sent %s · %s", request, stepOutcome(step))
	// 한 단계가 연결을 여러 개 쓰면(HTTP의 Host 재시도, goloop의 재시도) 개수를 밝힌다. 시간이 그만큼 더 들었다.
	if count := len(step.exchanges); count > 1 {
		text += fmt.Sprintf(" · %d connections, %s in all", count, debugDuration(step.end-step.start))
	}
	return text
}

// stepOutcome은 단계에서 서버가 한 일이다. 응답을 받았으면 그 내용을, 아니면 기다렸는지 끊겼는지를 쓴다.
func stepOutcome(step *debugStep) string {
	var waited, closed, reset, failed bool
	var waitedFor, closedAfter time.Duration
	for _, exchange := range step.exchanges {
		if exchange.receivedAny {
			return fmt.Sprintf("reply %d B in %s: %s", exchange.receivedTotal, debugDuration(exchange.firstReceive-exchange.opened), autoReply(exchange.received)[0])
		}
		switch {
		case exchange.dialErr != "":
			failed = true
		case exchange.end == "waited":
			waited, waitedFor = true, exchange.endAt-exchange.opened
		case exchange.end == "closed":
			closed, closedAfter = true, exchange.endAt-exchange.opened
		case exchange.end == "reset":
			reset, closedAfter = true, exchange.endAt-exchange.opened
		}
	}
	switch {
	case waited:
		return fmt.Sprintf("waited %s, no reply", debugDuration(waitedFor))
	case reset:
		return fmt.Sprintf("reset by the server after %s, no reply", debugDuration(closedAfter))
	case closed:
		return fmt.Sprintf("closed by the server after %s, no reply", debugDuration(closedAfter))
	case failed:
		return "connect failed: " + step.exchanges[0].dialErr
	}
	return "no reply"
}

func resultText(recorder *identifyRecorder, identified *protocolIdentification) string {
	total := time.Duration(0)
	if len(recorder.steps) > 0 {
		total = recorder.steps[len(recorder.steps)-1].end - recorder.steps[0].start
	}
	if identified.Name != protocolUnknown {
		return fmt.Sprintf("%s in %s", identified.Name, debugDuration(total))
	}
	var waited, closed, replied int
	for _, step := range recorder.steps {
		switch outcome := stepOutcome(step); {
		case strings.HasPrefix(outcome, "reply"):
			replied++
		case strings.HasPrefix(outcome, "waited"):
			waited++
		default:
			closed++
		}
	}
	reason := fmt.Sprintf("unknown: %d steps waited without a reply, %d closed, %d replied with something else", waited, closed, replied)
	if len(recorder.skipped) > 0 {
		reason += fmt.Sprintf(", %d not sent", len(recorder.skipped))
	}
	return reason
}

func handshakeText(handshake tcpHandshake) string {
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	scale := onOff(handshake.WindowScale)
	if handshake.WindowScale && handshake.SendScale >= 0 {
		scale = fmt.Sprintf("%d/%d", handshake.SendScale, handshake.ReceiveScale)
	}
	text := "rtt " + debugDuration(handshake.RTT)
	// macOS는 연결 직후 RTO를 0으로 알려 준다. 값이 없으면 쓰지 않는다.
	if handshake.RTO > 0 {
		text += " · rto " + debugDuration(handshake.RTO)
	}
	return text + fmt.Sprintf(" · mss %d · SACK %s · timestamps %s · window scale %s · ECN %s · retransmits %d",
		handshake.MSS, onOff(handshake.SACK), onOff(handshake.Timestamps), scale, onOff(handshake.ECN), handshake.Retransmits)
}

// debugDuration은 짧은 시간을 읽기 쉽게 쓴다. 1ms 아래는 0.1ms 단위, 1초 아래는 ms, 그 위는 0.01초 단위다.
func debugDuration(duration time.Duration) string {
	switch {
	case duration < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(duration.Microseconds())/1000)
	case duration < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(duration.Microseconds())/1000)
	case duration < time.Second:
		return fmt.Sprintf("%dms", duration.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", duration.Seconds())
}

// debugText는 대상 서버가 정한 글자를 결과에 넣을 수 있게 다듬는다. escape와 제어 문자를 지우고 길이를 묶는다.
func debugText(raw string) string {
	text := viewSanitize(raw)
	if runes := []rune(text); len(runes) > debugTextLimit {
		text = string(runes[:debugTextLimit]) + "…"
	}
	return text
}

// debugStyle은 debug 줄 하나에 입힐 색이다. 빈 값은 터미널 기본색이다. 색은 읽기를 돕는 것이라, 색이 없는 출력도 글만으로
// 같은 뜻이어야 한다. 줄을 만드는 쪽의 글머리와 종류로 정하므로 저장된 기록(report show)도 같은 색으로 그려진다.
type debugStyle struct {
	kind, text string
	bold       bool
}

func styleDebugLine(line DebugLine) debugStyle {
	trouble := strings.Contains(line.Text, "closed by the server") || strings.Contains(line.Text, "reset by the server") || strings.HasPrefix(line.Text, "connect failed")
	switch line.Kind {
	case "":
		if trouble {
			return debugStyle{text: richYellow}
		}
		return debugStyle{}
	case "dns", "identify":
		return debugStyle{kind: richCyan}
	case "connect":
		if strings.HasPrefix(line.Text, "failed") {
			return debugStyle{kind: richRed, text: richRed}
		}
		return debugStyle{kind: richCyan}
	case "tcp":
		if strings.Contains(line.Text, "likely a SYN retransmit") {
			return debugStyle{kind: richCyan, text: richYellow}
		}
		return debugStyle{kind: richCyan}
	case "stopped":
		return debugStyle{kind: richYellow, text: richYellow}
	case "result":
		if strings.HasPrefix(line.Text, "unknown") {
			return debugStyle{kind: richYellow, text: richYellow}
		}
		return debugStyle{kind: richGreen, text: richGreen}
	}
	// 나머지는 판별 단계의 이름이다. 맞은 단계는 눈에 띄게, 맞지 않은 단계는 흐리게 둔다. 서버가 끊거나 거부한 단계는 노랗게 둔다.
	switch {
	case strings.HasPrefix(line.Text, "matched"):
		return debugStyle{kind: richGreen, bold: true}
	case trouble:
		return debugStyle{kind: richGray, text: richYellow}
	}
	return debugStyle{kind: richGray, text: richGray}
}

// paintDebugText는 줄 내용에 색을 입힌다. 보낸 줄(→)과 받은 줄(←)은 글머리만 칠한다.
func paintDebugText(text, color string) string {
	switch {
	case strings.HasPrefix(text, debugSendMark):
		return richPaint(strings.TrimSpace(debugSendMark), richBlue, true) + " " + paintDebugBody(strings.TrimPrefix(text, debugSendMark), color)
	case strings.HasPrefix(text, debugReceiveMark):
		return richPaint(strings.TrimSpace(debugReceiveMark), richGreen, true) + " " + paintDebugBody(strings.TrimPrefix(text, debugReceiveMark), color)
	}
	return paintDebugBody(text, color)
}

func paintDebugBody(text, color string) string {
	if color == "" {
		return text
	}
	return richPaint(text, color, false)
}

// formatDebugLines는 기록을 터미널 줄로 그린다. 왼쪽에 경과 시각, 그다음 종류, 그다음 내용을 둔다.
func formatDebugLines(lines []DebugLine, color bool) []string {
	output := make([]string, 0, len(lines)+1)
	header := "      debug:"
	if color {
		header = "      " + richDim("debug:")
	}
	output = append(output, header)
	for _, line := range lines {
		at := ""
		if line.AtMS >= 0 {
			at = fmt.Sprintf("+%.1fms", line.AtMS)
			if line.AtMS >= 1000 {
				at = fmt.Sprintf("+%.2fs", line.AtMS/1000)
			}
		}
		kind := fmt.Sprintf("%-11s", line.Kind)
		indent := strings.Repeat("  ", line.Depth)
		text := line.Text
		if color {
			style := styleDebugLine(line)
			at = richDim(fmt.Sprintf("%9s", at))
			if style.kind != "" {
				kind = richPaint(kind, style.kind, style.bold)
			}
			text = paintDebugText(text, style.text)
		} else {
			at = fmt.Sprintf("%9s", at)
		}
		output = append(output, fmt.Sprintf("      %s  %s %s%s", at, kind, indent, text))
	}
	return output
}
