package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	peerMinVersion  = "0.80.0"
	peerInstallHint = "curl -fsSL https://github.com/x-mesh/edc/releases/latest/download/install.sh | sh"
	// peerConnectTimeout은 edc remote의 기본 ssh 연결 제한과 같다.
	peerConnectTimeout   = 10 * time.Second
	peerPreflightTimeout = 30 * time.Second
	// peerHeaderWait 안에 원격 tcp expect가 capture를 준비했다는 header를 내야 한다. 그 전에 SYN을 보내면 대상이 첫 시도를 놓친다.
	peerHeaderWait = 15 * time.Second
	// peerTraceAllowance는 Linux client에서 eBPF trace가 붙을 때까지 기다리는 상한(diagnoseTraceStartLimit)과 같다.
	// 그 상수는 Linux 파일에만 있어 여기서 같은 값을 둔다.
	peerTraceAllowance = 15 * time.Second
	peerDurationMargin = 5 * time.Second
	// peerLinger는 마지막 시도 뒤 대상에 늦게 닿는 packet(FIN, RST, SYN-ACK 재전송)을 원격이 볼 시간이다.
	peerLinger = 500 * time.Millisecond
	// peerStopGrace는 입력을 닫은 뒤 원격 expect가 report를 내고 끝나기를 기다리는 시간이다. 넘으면 ssh를 죽인다.
	// Ctrl-C 뒤에는 두 번째 Ctrl-C를 NotifyContext가 삼키므로 더 짧게 기다린다.
	peerStopGrace   = 10 * time.Second
	peerCancelGrace = 5 * time.Second

	peerSudoNone  = ""
	peerSudoEnv   = "env"
	peerSudoPlain = "plain"
)

// peerSharedSpace는 RFC 6598 주소다. Tailscale 같은 overlay가 쓰며 NAT 너머의 공인 주소가 아니다.
var peerSharedSpace = netip.MustParsePrefix("100.64.0.0/10")

var peerVersionPattern = regexp.MustCompile(`^edc\s+v?(\S+)`)

// peerSession은 원격에서 도는 ssh 하나다. test는 가짜를 쓴다.
type peerSession interface {
	CloseInput() error
	Kill() error
	Wait() error
}

type peerRunner interface {
	Capture(ctx context.Context, target, command string, timeout time.Duration, stdoutLimit int) remoteCapture
	Start(target, command string, stdout, stderr io.Writer) (peerSession, error)
}

type sshPeerRunner struct {
	sshRemoteRunner
}

func (runner sshPeerRunner) Start(target, command string, stdout, stderr io.Writer) (peerSession, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", T("remote.error.ssh_not_found"), err)
	}
	connectSeconds := int((runner.connectTimeout + time.Second - 1) / time.Second)
	process := exec.Command(path, "-o", "BatchMode=yes", "-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds), target, remoteShellCommand(command))
	process.Stdout, process.Stderr = stdout, stderr
	// terminal의 Ctrl-C가 ssh에 바로 가면 원격 report를 받기 전에 연결이 끊긴다. ssh를 다른 process group에 두고,
	// 입력을 닫아 원격 expect를 끝낸 뒤 report를 받는다.
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := process.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := process.Start(); err != nil {
		return nil, err
	}
	return &sshPeerSession{process: process, input: input}, nil
}

type sshPeerSession struct {
	process *exec.Cmd
	input   io.WriteCloser
}

func (session *sshPeerSession) CloseInput() error { return session.input.Close() }
func (session *sshPeerSession) Kill() error       { return session.process.Process.Kill() }
func (session *sshPeerSession) Wait() error       { return session.process.Wait() }

// peerHost는 preflight가 확인한 원격 edc다.
type peerHost struct {
	name    string
	edc     string
	version string
	sudo    string
}

type peerPreflight struct {
	uid       string
	edc       string
	version   string
	sudo      string
	sudoError string
}

// peerPreflightScript는 uid, edc 위치와 version, root가 아니면 sudo로 같은 명령을 쓸 수 있는지를 key=value 줄로 낸다.
// sudo는 실행할 때와 같은 꼴로 시험한다. 기본 sudoers의 env_reset은 EDC_LANG을 거절할 수 있으므로 그때는 언어 없이 시험한다.
func peerPreflightScript(language string) string {
	env := "EDC_LANG=" + quoteRemoteShell(language)
	return `echo "uid=$(id -u)"; p=$(command -v edc) || { echo "edc="; exit 0; }; echo "edc=$p"; ` +
		`echo "version=$("$p" version </dev/null 2>&1 | head -n 1)"; ` +
		`if [ "$(id -u)" != 0 ]; then ` +
		`if sudo -n ` + env + ` "$p" version </dev/null >/dev/null 2>&1; then echo "sudo=env"; ` +
		`elif sudo -n "$p" version </dev/null >/dev/null 2>&1; then echo "sudo=plain"; ` +
		`else echo "sudo=no"; echo "sudo_error=$(sudo -n "$p" version </dev/null 2>&1 >/dev/null | head -n 1)"; fi; fi`
}

func parsePeerPreflight(output string) (peerPreflight, bool) {
	var result peerPreflight
	seen := false
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch key {
		case "uid":
			result.uid, seen = value, true
		case "edc":
			result.edc = value
		case "version":
			result.version = value
		case "sudo":
			result.sudo = value
		case "sudo_error":
			result.sudoError = value
		}
	}
	return result, seen
}

// parsePeerVersion은 edc version의 한 줄(edc 0.80.1 (schema 1.0))에서 version을 읽는다.
func parsePeerVersion(line string) (string, bool) {
	match := peerVersionPattern.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// peerVersionSupported는 version이 minimum 이상인지 본다. dev는 Makefile이 version 없이 source에서 만든 build다.
// 그 build의 기능은 version으로 알 수 없으므로 막지 않는다. 0.80.1-rc1 같은 꼬리는 무시한다.
func peerVersionSupported(version, minimum string) bool {
	if version == "dev" {
		return true
	}
	have, ok := peerVersionNumbers(version)
	if !ok {
		return false
	}
	want, _ := peerVersionNumbers(minimum)
	for index := range want {
		if have[index] != want[index] {
			return have[index] > want[index]
		}
	}
	return true
}

func peerVersionNumbers(version string) ([3]int, bool) {
	var numbers [3]int
	core, _, _ := strings.Cut(version, "-")
	core, _, _ = strings.Cut(core, "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return numbers, false
	}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return numbers, false
		}
		numbers[index] = value
	}
	return numbers, true
}

// checkPeerPreflight는 preflight 결과로 원격 edc를 정하거나, 사용자가 고칠 일을 담은 오류를 돌려준다.
func checkPeerPreflight(name string, preflight peerPreflight) (peerHost, error) {
	if preflight.edc == "" {
		return peerHost{}, errors.New(T("diagnose.peer.edc_missing", name, peerInstallHint))
	}
	version, ok := parsePeerVersion(preflight.version)
	if !ok {
		return peerHost{}, errors.New(T("diagnose.peer.version_unknown", name, joinValue(preflight.version)))
	}
	if !peerVersionSupported(version, peerMinVersion) {
		return peerHost{}, errors.New(T("diagnose.peer.version_old", version, name, peerMinVersion, name))
	}
	host := peerHost{name: name, edc: preflight.edc, version: version}
	if preflight.uid == "0" {
		return host, nil
	}
	switch preflight.sudo {
	case peerSudoEnv, peerSudoPlain:
		host.sudo = preflight.sudo
		return host, nil
	}
	return peerHost{}, errors.New(T("diagnose.peer.sudo_password", name, joinValue(preflight.sudoError), peerSSHHost(name), preflight.edc))
}

// peerSSHHost는 user@host에서 host다. 사용자에게 root@host를 권할 때 쓴다.
func peerSSHHost(name string) string {
	if index := strings.LastIndex(name, "@"); index >= 0 {
		return name[index+1:]
	}
	return name
}

// peerExpectDuration은 원격 expect의 --duration이다. 보통은 입력을 닫아 먼저 끝내므로 이 값은 연결이 끊긴 채 남은
// 원격 process를 끝내는 안전장치다. header 대기, 경로 조회, trace 시작, 시도마다의 connect 제한과 settle, 시도 사이
// 간격을 모두 담고 초 단위로 올린다.
func peerExpectDuration(options tcpDiagnoseOptions) time.Duration {
	count := time.Duration(options.count)
	total := count*(options.common.timeout+tcpDiagnoseSettle) + (count-1)*options.interval +
		options.common.timeout + peerHeaderWait + peerTraceAllowance + peerDurationMargin
	total = (total + time.Second - 1).Truncate(time.Second)
	return min(total, expectMaxDuration)
}

// peerExpectFrom은 원격 expect의 --from이다. 대상이 공인 주소이고 출발지가 사설 주소면 그 사이의 NAT가 출발지를
// 바꾼다. 그때 이 host의 주소로 거르면 대상은 바뀐 출발지를 모두 버려 아무것도 못 보므로 거르지 않는다.
func peerExpectFrom(source, address string) string {
	from, err := netip.ParseAddr(source)
	if err != nil {
		return ""
	}
	from = from.Unmap().WithZone("")
	if host, _, err := net.SplitHostPort(address); err == nil {
		if destination, err := netip.ParseAddr(host); err == nil && peerPublic(destination) && !peerPublic(from) {
			return ""
		}
	}
	return from.String()
}

func peerPublic(address netip.Addr) bool {
	address = address.Unmap()
	return address.IsGlobalUnicast() && !address.IsPrivate() && !peerSharedSpace.Contains(address)
}

// peerExpectScript는 원격에서 sh가 실행할 script다. expect는 background로 돌리고, 앞의 cat이 ssh 입력을 붙든다.
// 입력이 닫히면(정상 종료, Ctrl-C, ssh 연결 끊김 모두) cat이 끝나고 expect에 SIGTERM을 보내, expect가 그때까지의 report를
// 내고 끝난다. expect가 먼저 끝나면 wait가 그 exit code로 script를 끝낸다.
func peerExpectScript(host peerHost, language, port string, duration time.Duration, from string) string {
	args := []string{"tcp", "expect", "--json", "-", "--duration", fmt.Sprintf("%ds", int(duration/time.Second))}
	if from != "" {
		args = append(args, "--from", from)
	}
	args = append(args, port)
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, quoteRemoteShell(arg))
	}
	invocation := peerInvocation(host, language) + " " + strings.Join(quoted, " ")
	// 비대화형 sh는 background 명령의 입력을 /dev/null로 바꾸므로, 감시하는 cat에는 미리 복제한 fd 5로 ssh 입력을 준다.
	// 감시 subshell의 출력을 닫아야 expect가 끝났을 때 sshd가 session을 닫는다.
	return "exec 5<&0; " + invocation + ` 5<&- </dev/null & pid=$!; ` +
		`( cat <&5 >/dev/null 2>&1; kill -TERM "$pid" 2>/dev/null ) >/dev/null 2>&1 & ` +
		`exec 5<&-; wait "$pid"`
}

func peerInvocation(host peerHost, language string) string {
	env := "EDC_LANG=" + quoteRemoteShell(language)
	edc := quoteRemoteShell(host.edc)
	switch host.sudo {
	case peerSudoEnv:
		return "sudo -n " + env + " " + edc
	case peerSudoPlain:
		return "sudo -n " + edc
	}
	return env + " " + edc
}

// peerRemoteCommand는 sshRemoteRunner처럼 login shell을 거친다. 사용자의 PATH를 쓰되, 대화형 shell이 background job마다
// 내는 [1] pid 줄을 막으려고 script는 비대화형 sh로 돌린다.
func peerRemoteCommand(script string) string {
	return "exec sh -c " + quoteRemoteShell(script)
}

func peerJSONPaths(path string) (target, join string) {
	stem := strings.TrimSuffix(path, ".json")
	return stem + ".target.json", stem + ".join.json"
}

// peerLines는 원격 stderr를 줄로 나눈다. header 전의 줄은 원격이 시작하지 못한 이유로 모으고, header부터는 show에 넘긴다.
type peerLines struct {
	mu      sync.Mutex
	header  string
	partial []byte
	early   []string
	started bool
	ready   chan struct{}
	show    func(string)
}

func newPeerLines(port string, show func(string)) *peerLines {
	// header의 "tcp expect :<port>"는 번역하지 않고, pipe로는 색도 없다.
	return &peerLines{header: "tcp expect :" + port, ready: make(chan struct{}), show: show}
}

// peerEarlyLines와 peerLineLimit은 원격이 header를 내기 전의 줄과 줄바꿈 없는 조각을 묶어 둔다. 원격이 잘못된 출력을 끝없이
// 내도 이 host의 메모리가 그만큼 늘지 않게 한다. header 전의 줄은 시작하지 못한 이유를 보이려는 것이라 마지막 몇 줄이면 된다.
const (
	peerEarlyLines = 50
	peerLineLimit  = 64 << 10
)

func (lines *peerLines) Write(data []byte) (int, error) {
	lines.mu.Lock()
	defer lines.mu.Unlock()
	lines.partial = append(lines.partial, data...)
	for {
		index := bytes.IndexByte(lines.partial, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(lines.partial[:index]), "\r")
		lines.partial = lines.partial[index+1:]
		lines.handle(line)
	}
	if len(lines.partial) > peerLineLimit {
		lines.handle(string(lines.partial[:peerLineLimit]))
		lines.partial = nil
	}
	return len(data), nil
}

func (lines *peerLines) flush() {
	lines.mu.Lock()
	defer lines.mu.Unlock()
	if len(lines.partial) > 0 {
		lines.handle(string(lines.partial))
		lines.partial = nil
	}
}

func (lines *peerLines) handle(line string) {
	if !lines.started {
		if !strings.Contains(line, lines.header) {
			lines.early = append(lines.early, line)
			if len(lines.early) > peerEarlyLines {
				lines.early = lines.early[len(lines.early)-peerEarlyLines:]
			}
			return
		}
		lines.started = true
		close(lines.ready)
	}
	if peerLineShown(line) {
		lines.show(line)
	}
}

func (lines *peerLines) earlyText() string {
	lines.mu.Lock()
	defer lines.mu.Unlock()
	return strings.TrimSpace(strings.Join(lines.early, "\n"))
}

// peerLineShown은 원격 expect가 사람에게 시키는 줄을 뺀다. client에서 실행할 명령을 알리는 대기 줄과, 끝난 뒤 report를
// 합치라는 안내다. --peer는 그 명령을 이미 실행하고 결과도 스스로 합치므로, 두 줄은 틀린 지시가 된다. 원격은 이 host와
// 같은 언어로 실행하므로 접두사를 이 host의 번역으로 맞춘다.
func peerLineShown(line string) bool {
	if strings.Contains(line, "edc tcp check --diagnose") || strings.Contains(line, "edc report join") {
		return false
	}
	// 원격 결과 줄 아래의 next:는 이 host의 판정과 join 확인 항목이 이미 다룬다. 두 번 보이면 어느 쪽에서 실행할지 헷갈린다.
	if strings.HasPrefix(strings.TrimSpace(line), "next:") {
		return false
	}
	step, _, _ := strings.Cut(T("expect.next.step"), "%s")
	return step == "" || !strings.HasPrefix(line, step)
}

// peerExpect는 시작된 원격 expect다. stop이 입력을 닫고 report를 받는다.
type peerExpect struct {
	session peerSession
	stdout  *remoteLimitedBuffer
	lines   *peerLines
	done    chan struct{}
	err     error
}

func startPeerExpect(ctx context.Context, runner peerRunner, name, command, port string, show func(string)) (*peerExpect, error) {
	expect := &peerExpect{stdout: &remoteLimitedBuffer{limit: reportSizeLimit}, lines: newPeerLines(port, show), done: make(chan struct{})}
	session, err := runner.Start(name, command, expect.stdout, expect.lines)
	if err != nil {
		return nil, errors.New(T("diagnose.peer.ssh_failed", name, err))
	}
	expect.session = session
	go func() {
		expect.err = session.Wait()
		expect.lines.flush()
		close(expect.done)
	}()
	timer := time.NewTimer(peerHeaderWait)
	defer timer.Stop()
	select {
	case <-expect.lines.ready:
		return expect, nil
	case <-expect.done:
		return nil, errors.New(T("diagnose.peer.ended", name, peerExitText(expect.err), joinValue(peerFailureText(expect))))
	case <-timer.C:
		expect.kill()
		return nil, errors.New(T("diagnose.peer.not_ready", name, peerHeaderWait, joinValue(expect.lines.earlyText())))
	case <-ctx.Done():
		expect.stop(peerCancelGrace)
		return nil, ctx.Err()
	}
}

// peerFailureText는 원격이 header 전에 끝난 이유다. stderr가 비면 stdout report의 첫 결과 summary를 쓴다.
func peerFailureText(expect *peerExpect) string {
	if text := expect.lines.earlyText(); text != "" {
		return text
	}
	if report, err := parsePeerReport([]byte(expect.stdout.String())); err == nil {
		return firstLine(joinFindResult(report, expectProbeID).Summary)
	}
	return ""
}

func peerExitText(err error) string {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return strconv.Itoa(exitError.ExitCode())
	}
	if err != nil {
		return err.Error()
	}
	return "0"
}

func (expect *peerExpect) kill() {
	_ = expect.session.Kill()
	<-expect.done
}

// stop은 입력을 닫아 원격 expect를 끝내고 grace 안에 끝나지 않으면 ssh를 죽인다. ssh가 죽으면 sshd가 입력을 닫아 원격의
// cat이 같은 경로로 expect를 끝낸다.
func (expect *peerExpect) stop(grace time.Duration) []byte {
	_ = expect.session.CloseInput()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-expect.done:
	case <-timer.C:
		expect.kill()
	}
	return []byte(expect.stdout.String())
}

func parsePeerReport(data []byte) (Report, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Report{}, errors.New(T("diagnose.peer.empty_report"))
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, err
	}
	if report.SchemaVersion != "1.0" {
		return Report{}, errors.New(T("cli.report.unsupported_schema", "-", report.SchemaVersion))
	}
	if joinFindResult(report, expectProbeID).Probe == "" {
		return Report{}, errors.New(T("diagnose.peer.empty_report"))
	}
	return report, nil
}

// normalizeReport는 report를 JSON으로 한 번 오가게 한다. join은 파일에서 읽은 값(float64, []interface{})을 기대하므로
// 이 process에서 만든 report도 같은 꼴로 맞춘다. 결과는 원본과 map을 공유하지 않는 사본이다.
func normalizeReport(report Report) Report {
	data, _ := json.Marshal(report)
	var copied Report
	_ = json.Unmarshal(data, &copied)
	return copied
}

func redactedReport(report Report) Report {
	copied := normalizeReport(report)
	copied.Redaction.Enabled = true
	redactReport(&copied)
	return copied
}

type peerCombined struct {
	Client Report     `json:"client"`
	Target Report     `json:"target"`
	Join   reportJoin `json:"join"`
}

// peerPrinter는 이 host의 줄과 원격 줄을 한 writer에 섞어 쓴다. 원격 줄 앞에는 [peer]를 붙인다.
type peerPrinter struct {
	mu     sync.Mutex
	writer io.Writer
	prefix string
	redact bool
}

func newPeerPrinter(writer io.Writer, name string, color, redact bool) *peerPrinter {
	return &peerPrinter{writer: writer, prefix: newTextStyle(color).dim("["+name+"]") + " ", redact: redact}
}

func (printer *peerPrinter) local(text string) {
	printer.mu.Lock()
	defer printer.mu.Unlock()
	fmt.Fprint(printer.writer, text)
}

func (printer *peerPrinter) remote(line string) {
	printer.mu.Lock()
	defer printer.mu.Unlock()
	fmt.Fprint(printer.writer, formatPeerLine(printer.prefix, line, printer.redact))
}

func formatPeerLine(prefix, line string, redact bool) string {
	text := prefix + strings.TrimRight(line, "\n") + "\n"
	if redact {
		text = redactIPAddresses(text)
	}
	return text
}

// peerRun은 --peer 한 번의 입력이다. diagnose는 이 host의 시도이고, test는 network 없이 가짜를 넣는다.
type peerRun struct {
	name       string
	target     diagnoseTarget
	options    tcpDiagnoseOptions
	version    string
	started    time.Time
	targetInfo map[string]interface{}
	language   string
	source     string
	runner     peerRunner
	diagnose   func(context.Context, func(Result)) Result
	stdout     io.Writer
	stderr     io.Writer
	color      bool
	liveColor  bool
}

func runTCPDiagnosePeer(ctx context.Context, target diagnoseTarget, options tcpDiagnoseOptions, name, version string, started time.Time, targetInfo map[string]interface{}) int {
	// ssh는 -로 시작하는 destination을 option으로 읽는다.
	if strings.HasPrefix(name, "-") || strings.TrimSpace(name) == "" {
		fmt.Fprintln(os.Stderr, T("diagnose.peer.invalid", name))
		return 2
	}
	live := io.Writer(os.Stdout)
	if options.common.jsonPath == "-" {
		live = os.Stderr
	}
	run := peerRun{
		name: name, target: target, options: options, version: version, started: started, targetInfo: targetInfo,
		language: currentLanguage(), source: diagnoseSource(target.address),
		runner: sshPeerRunner{sshRemoteRunner{connectTimeout: peerConnectTimeout, outputLimit: remoteOutputLimit}},
		diagnose: func(ctx context.Context, show func(Result)) Result {
			return diagnoseTCP(ctx, target, options, show)
		},
		stdout: os.Stdout, stderr: os.Stderr,
		color:     options.common.jsonPath != "-" && isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
		liveColor: helpColor(live) && os.Getenv("NO_COLOR") == "",
	}
	return run.execute(ctx)
}

func (run peerRun) execute(ctx context.Context) int {
	capture := run.runner.Capture(ctx, run.name, peerRemoteCommand(peerPreflightScript(run.language)), peerPreflightTimeout, remoteOutputLimit)
	if ctx.Err() != nil {
		return run.cancelledEarly()
	}
	preflight, ok := parsePeerPreflight(capture.Stdout)
	if !ok {
		reason := firstLine(strings.TrimSpace(capture.Stderr))
		if reason == "" && capture.Err != nil {
			reason = capture.Err.Error()
		}
		fmt.Fprintln(run.stderr, T("diagnose.peer.ssh_failed", run.name, joinValue(reason)))
		return 2
	}
	host, err := checkPeerPreflight(run.name, preflight)
	if err != nil {
		fmt.Fprintln(run.stderr, err)
		return 2
	}
	live := run.stdout
	if run.options.common.jsonPath == "-" {
		live = run.stderr
	}
	printer := newPeerPrinter(live, run.name, run.liveColor, run.options.common.redact)
	duration := peerExpectDuration(run.options)
	script := peerExpectScript(host, run.language, run.target.port, duration, peerExpectFrom(run.source, run.target.address))
	expect, err := startPeerExpect(ctx, run.runner, run.name, peerRemoteCommand(script), run.target.port, printer.remote)
	if err != nil {
		if ctx.Err() != nil {
			return run.cancelledEarly()
		}
		fmt.Fprintln(run.stderr, err)
		return 2
	}
	show := func(row Result) { printer.local(diagnoseRowLine(row, run.liveColor, run.options.common.redact)) }
	result := run.diagnose(ctx, show)
	cancelled := ctx.Err() != nil
	grace := peerStopGrace
	if cancelled {
		grace = peerCancelGrace
	} else {
		sleepContext(ctx, peerLinger)
	}
	output := expect.stop(grace)
	// 로컬 시도가 끝난 뒤 원격 report를 기다리는 동안 누른 Ctrl-C도 취소다. 놓치면 정상 결과와 종료 코드로 끝난다.
	cancelled = cancelled || ctx.Err() != nil
	client := buildReport(run.version, run.started, run.targetInfo, []Result{result}, false)
	targetReport, err := parsePeerReport(output)
	if err != nil {
		run.printClient(client)
		fmt.Fprintln(run.stderr, T("diagnose.peer.no_report", run.name, peerExitText(expect.err), joinValue(firstLine(err.Error()))))
		if cancelled {
			fmt.Fprintln(run.stderr, errProbeCancelled)
			return 4
		}
		return 2
	}
	code := run.finish(client, targetReport)
	if cancelled {
		fmt.Fprintln(run.stderr, errProbeCancelled)
		return 4
	}
	return code
}

// cancelledEarly는 첫 연결 시도 전에 Ctrl-C로 멈춘 경우다. 원격이 막 준비된 순간과 겹치면 원격 줄만 보이고 끝나므로,
// 판정할 결과가 없다는 것을 알린다.
func (run peerRun) cancelledEarly() int {
	fmt.Fprintln(run.stderr, T("diagnose.peer.cancelled_early"))
	fmt.Fprintln(run.stderr, errProbeCancelled)
	return 4
}

// printClient는 원격 report가 없을 때 이 host의 결과만 보인다.
func (run peerRun) printClient(client Report) {
	if run.options.common.redact {
		client = redactedReport(client)
	}
	if run.options.common.jsonPath != "" {
		if err := writeJSONOutput(run.options.common.jsonPath, client); err != nil {
			fmt.Fprintln(run.stderr, err)
		}
		return
	}
	result := withoutEvidence(client.Results[0])
	fmt.Fprintln(run.stdout)
	fmt.Fprint(run.stdout, formatResultLine(result, run.color))
	printResultDetail(run.stdout, result, run.options.common.verbose, run.color)
}

// finish는 두 report를 맞춰 출력하고 join 판정의 exit code를 돌려준다. join은 가리기 전의 값으로 해야 출발지를 맞출 수 있다.
func (run peerRun) finish(clientRaw, targetRaw Report) int {
	client, target := normalizeReport(clientRaw), normalizeReport(targetRaw)
	jsonPath := run.options.common.jsonPath
	clientLabel, targetLabel, joinPath := "client", run.name, ""
	if jsonPath != "" && jsonPath != "-" {
		clientLabel = jsonPath
		targetLabel, joinPath = peerJSONPaths(jsonPath)
	}
	join := joinReports(savedReport{path: clientLabel, report: client}, savedReport{path: targetLabel, report: target})
	clientOut, targetOut := client, target
	if run.options.common.redact {
		join = redactJoin(join, client, target)
		clientOut, targetOut = redactedReport(client), redactedReport(target)
	}
	code := 1
	if join.Segment == joinSegmentWorks {
		code = 0
	}
	if jsonPath == "-" {
		if err := writeJSON(run.stdout, peerCombined{Client: clientOut, Target: targetOut, Join: join}); err != nil {
			fmt.Fprintln(run.stderr, err)
			return 2
		}
		return code
	}
	if jsonPath != "" {
		for _, file := range []struct {
			path  string
			value interface{}
		}{{jsonPath, clientOut}, {targetLabel, targetOut}, {joinPath, join}} {
			if err := writeJSONOutput(file.path, file.value); err != nil {
				fmt.Fprintln(run.stderr, err)
				return 2
			}
		}
	}
	run.printSummary(clientOut, targetOut, join)
	if jsonPath != "" {
		fmt.Fprintln(run.stdout, T("diagnose.peer.saved", strings.Join([]string{jsonPath, targetLabel, joinPath}, ", ")))
	}
	return code
}

func (run peerRun) printSummary(client, target Report, join reportJoin) {
	verbose := run.options.common.verbose
	local := joinFindResult(client, tcpDiagnoseProbeID)
	remote := joinFindResult(target, expectProbeID)
	// 두 결과의 판정, 오류 상자, next는 아래 join 블록이 한 번에 보인다. 따로 보이면 같은 결론을 세 번 읽게 된다.
	// 판정을 바꾸지 않는 경고(추적을 건너뛴 이유 같은)는 join에 없으므로 남긴다. join이 구간을 정하지 못하면(이름을 풀지
	// 못했거나 경로가 없을 때) client의 오류 상자가 유일한 원인 설명이므로 그때는 남긴다.
	fmt.Fprintln(run.stdout)
	detail := Result{Probe: local.Probe, Warnings: local.Warnings}
	if join.Segment == joinSegmentUndetermined {
		detail.Status, detail.Error = local.Status, local.Error
	}
	printResultDetail(run.stdout, detail, verbose, run.color)
	if len(remote.Warnings) > 0 {
		prefix := newTextStyle(run.color).dim("[" + run.name + "]")
		for _, warning := range remote.Warnings {
			fmt.Fprint(run.stdout, formatPeerLine(prefix+" ", T("join.label.warning")+" "+warning+"\n", run.options.common.redact))
		}
	}
	printReportJoin(run.stdout, join)
}
