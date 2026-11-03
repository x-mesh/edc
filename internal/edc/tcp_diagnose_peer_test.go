package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPeerExpectDurationCoversEveryWaitAndIsCapped(t *testing.T) {
	options := tcpDiagnoseOptions{common: commonOptions{timeout: 5 * time.Second}, count: 5, interval: time.Second}
	// 5×(5s+0.3s) + 4×1s + 경로 5s + header 15s + trace 15s + 여유 5s = 70.5s, 초로 올려 71s.
	if got := peerExpectDuration(options); got != 71*time.Second {
		t.Fatalf("duration = %s, want 71s", got)
	}
	options = tcpDiagnoseOptions{common: commonOptions{timeout: time.Minute}, count: tcpDiagnoseMaxCount, interval: time.Minute}
	if got := peerExpectDuration(options); got != expectMaxDuration {
		t.Fatalf("duration = %s, want the cap %s", got, expectMaxDuration)
	}
}

func TestPeerExpectFromSkipsALikelyNAT(t *testing.T) {
	for _, row := range []struct{ source, address, want string }{
		{"100.65.168.177", "100.83.200.248:2171", "100.65.168.177"},
		{"10.0.0.5", "10.0.0.9:443", "10.0.0.5"},
		{"192.168.1.92", "203.0.113.10:443", ""},
		{"198.51.100.7", "203.0.113.10:443", "198.51.100.7"},
		{"fd00::5", "[fd00::9]:443", "fd00::5"},
		{"", "10.0.0.9:443", ""},
	} {
		if got := peerExpectFrom(row.source, row.address); got != row.want {
			t.Errorf("peerExpectFrom(%q, %q) = %q, want %q", row.source, row.address, got, row.want)
		}
	}
}

func TestPeerVersionParseAndCompare(t *testing.T) {
	for _, row := range []struct {
		line      string
		version   string
		parsed    bool
		supported bool
	}{
		{"edc 0.80.1 (schema 1.0)", "0.80.1", true, true},
		{"edc 0.80.0 (schema 1.0)", "0.80.0", true, true},
		{"edc 0.79.9 (schema 1.0)", "0.79.9", true, false},
		{"edc 1.0.0", "1.0.0", true, true},
		{"edc 0.80.0-rc1 (schema 1.0)", "0.80.0-rc1", true, true},
		{"edc dev (schema 1.0)", "dev", true, true},
		{"edc banana", "banana", true, false},
		{"sh: 1: edc: not found", "", false, false},
	} {
		version, ok := parsePeerVersion(row.line)
		if version != row.version || ok != row.parsed {
			t.Errorf("parsePeerVersion(%q) = %q, %v; want %q, %v", row.line, version, ok, row.version, row.parsed)
			continue
		}
		if ok && peerVersionSupported(version, peerMinVersion) != row.supported {
			t.Errorf("peerVersionSupported(%q) = %v, want %v", version, !row.supported, row.supported)
		}
	}
}

func TestCheckPeerPreflightNamesTheFix(t *testing.T) {
	setLanguage("en")
	output := "uid=1000\nedc=/home/me/.local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\nsudo=no\nsudo_error=sudo: a password is required\n"
	preflight, ok := parsePeerPreflight(output)
	if !ok || preflight.uid != "1000" || preflight.sudo != "no" {
		t.Fatalf("parsePeerPreflight = %+v, %v", preflight, ok)
	}
	if _, err := checkPeerPreflight("me@db1", preflight); err == nil || !strings.Contains(err.Error(), "root@db1") || !strings.Contains(err.Error(), "a password is required") {
		t.Fatalf("sudo error = %v", err)
	}
	if _, ok := parsePeerPreflight("Permission denied (publickey).\n"); ok {
		t.Fatal("ssh failure parsed as preflight")
	}
	_, err := checkPeerPreflight("db1", peerPreflight{uid: "0"})
	if err == nil || !strings.Contains(err.Error(), peerInstallHint) {
		t.Fatalf("missing edc error = %v", err)
	}
	_, err = checkPeerPreflight("db1", peerPreflight{uid: "0", edc: "/usr/local/bin/edc", version: "edc 0.79.1 (schema 1.0)"})
	if err == nil || !strings.Contains(err.Error(), "edc update") || !strings.Contains(err.Error(), "0.79.1") {
		t.Fatalf("old version error = %v", err)
	}
	host, err := checkPeerPreflight("db1", peerPreflight{uid: "0", edc: "/usr/local/bin/edc", version: "edc 0.80.1 (schema 1.0)"})
	if err != nil || host.sudo != peerSudoNone || host.version != "0.80.1" {
		t.Fatalf("root host = %+v, %v", host, err)
	}
	host, err = checkPeerPreflight("me@db1", peerPreflight{uid: "1000", edc: "/usr/bin/edc", version: "edc 0.80.1", sudo: "plain"})
	if err != nil || host.sudo != peerSudoPlain {
		t.Fatalf("sudo host = %+v, %v", host, err)
	}
}

func TestPeerInvocationRunsSudoInTheTestedForm(t *testing.T) {
	host := peerHost{edc: "/usr/bin/edc"}
	if got := peerInvocation(host, "ko"); got != "EDC_LANG='ko' '/usr/bin/edc'" {
		t.Errorf("root invocation = %s", got)
	}
	host.sudo = peerSudoEnv
	if got := peerInvocation(host, "ko"); got != "sudo -n EDC_LANG='ko' '/usr/bin/edc'" {
		t.Errorf("sudo env invocation = %s", got)
	}
	host.sudo = peerSudoPlain
	if got := peerInvocation(host, "ko"); got != "sudo -n '/usr/bin/edc'" {
		t.Errorf("sudo plain invocation = %s", got)
	}
}

// 원격 script를 이 host의 sh로 실제로 돌린다. 가짜 edc는 header를 낸 뒤 SIGTERM을 받으면 report를 내고 4로 끝난다.
// 입력을 닫으면 expect가 report를 내고 끝나는지, 인자가 quote를 지나 그대로 닿는지 본다.
func TestPeerExpectScriptStopsExpectWhenInputCloses(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	directory := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(directory, "args")
	fake := filepath.Join(directory, "edc")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$EDC_LANG\" \"$@\" > " + quoteRemoteShell(argsFile) + "\n" +
		"trap 'echo \"{\\\"stopped\\\": true}\"; exit 4' TERM\n" +
		"echo 'tcp expect :2171  ·  71s  ·  Ctrl-C stop' >&2\n" +
		"while :; do sleep 0.05; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := peerExpectScript(peerHost{edc: fake}, "ko", "2171", 71*time.Second, "10.0.0.5")
	process := exec.Command("sh", "-c", peerRemoteCommand(remote))
	var stdout bytes.Buffer
	lines := newPeerLines("2171", func(string) {})
	process.Stdout, process.Stderr = &stdout, lines
	input, err := process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lines.ready:
	case <-time.After(5 * time.Second):
		_ = process.Process.Kill()
		t.Fatal("no header")
	}
	input.Close()
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		_ = process.Process.Kill()
		t.Fatal("the script did not end after the input closed")
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 4 {
		t.Fatalf("exit = %v, want 4", err)
	}
	if strings.TrimSpace(stdout.String()) != `{"stopped": true}` {
		t.Fatalf("stdout = %q", stdout.String())
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "ko\ntcp\nexpect\n--json\n-\n--duration\n71s\n--from\n10.0.0.5\n2171\n"
	if string(data) != want {
		t.Fatalf("args = %q, want %q", data, want)
	}
}

func TestPeerLinesWaitsForTheHeaderAndHidesTheClientHint(t *testing.T) {
	var shown []string
	lines := newPeerLines("2171", func(line string) { shown = append(shown, line) })
	for _, chunk := range []string{"warning: something\n", "tcp expect :21", "71  ·  71s\nkernel drop reasons: on\n", "waiting. On the client run edc tcp check --diagnose this-host:2171\n", "12:00:00.000  SYN in"} {
		_, _ = lines.Write([]byte(chunk))
	}
	select {
	case <-lines.ready:
	default:
		t.Fatal("header not seen")
	}
	lines.flush()
	want := []string{"tcp expect :2171  ·  71s", "kernel drop reasons: on", "12:00:00.000  SYN in"}
	if strings.Join(shown, "|") != strings.Join(want, "|") {
		t.Fatalf("shown = %q, want %q", shown, want)
	}
	if lines.earlyText() != "warning: something" {
		t.Fatalf("early = %q", lines.earlyText())
	}
}

func TestPeerJSONPathsAndLinePrefix(t *testing.T) {
	for path, want := range map[string][2]string{
		"client.json":     {"client.target.json", "client.join.json"},
		"out/run":         {"out/run.target.json", "out/run.join.json"},
		"a.b/report.json": {"a.b/report.target.json", "a.b/report.join.json"},
	} {
		target, join := peerJSONPaths(path)
		if target != want[0] || join != want[1] {
			t.Errorf("peerJSONPaths(%q) = %q, %q", path, target, join)
		}
	}
	line := formatPeerLine("[jw-server] ", "SYN in  tailscale0  100.65.168.177:50000 → 100.83.200.248:2171", true)
	if !strings.HasPrefix(line, "[jw-server] SYN in") || strings.Contains(line, "100.65.168.177") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("redacted line = %q", line)
	}
}

type fakePeerRunner struct {
	preflight string
	header    bool
	// silent는 원격이 header를 내지 않고 입력이 닫힐 때까지 기다리는 경우다. 원격 준비를 기다리는 중의 취소를 만든다.
	silent   bool
	report   []byte
	commands []string
	mu       sync.Mutex
}

func (runner *fakePeerRunner) Capture(_ context.Context, _, command string, _ time.Duration, _ int) remoteCapture {
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	return remoteCapture{Stdout: runner.preflight}
}

func (runner *fakePeerRunner) Start(_, command string, stdout, stderr io.Writer) (peerSession, error) {
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	session := &fakePeerSession{closed: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(session.done)
		if runner.silent {
			<-session.closed
			return
		}
		if !runner.header {
			_, _ = stderr.Write([]byte("tcp expect needs root\n"))
			return
		}
		_, _ = stderr.Write([]byte("tcp expect :2171  ·  71s  ·  Ctrl-C stop\n12:00:00.000  SYN in  eth0  10.0.0.5:50000 → 10.0.0.9:2171\n"))
		<-session.closed
		_, _ = stdout.Write(runner.report)
	}()
	return session, nil
}

type fakePeerSession struct {
	once   sync.Once
	closed chan struct{}
	done   chan struct{}
}

func (session *fakePeerSession) CloseInput() error {
	session.once.Do(func() { close(session.closed) })
	return nil
}
func (session *fakePeerSession) Kill() error { return session.CloseInput() }
func (session *fakePeerSession) Wait() error { <-session.done; return nil }

func peerTestRun(t *testing.T, runner *fakePeerRunner, jsonPath string) (peerRun, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	options := tcpDiagnoseOptions{common: commonOptions{timeout: time.Second, jsonPath: jsonPath}, count: 1}
	target := diagnoseTarget{input: "10.0.0.9:2171", host: "10.0.0.9", port: "2171", address: "10.0.0.9:2171"}
	return peerRun{
		name: "db1", target: target, options: options, version: "0.80.1", started: time.Now(), targetInfo: map[string]interface{}{"input": target.input},
		language: "en", source: "10.0.0.5", runner: runner, stdout: &stdout, stderr: &stderr,
		diagnose: func(_ context.Context, show func(Result)) Result {
			show(Result{Probe: diagnoseRowRoute, Status: StatusPass, Summary: "dev eth0, src 10.0.0.5"})
			return Result{Probe: tcpDiagnoseProbeID, Status: StatusPass, Summary: "connected", Metrics: map[string]interface{}{
				"input": target.input, "address": target.address, "verdict": diagnoseVerdictConnected, "basis": diagnoseBasisPhases,
				"route": map[string]interface{}{"source": "10.0.0.5"}, "attempts": []diagnoseAttempt{{Index: 1, Outcome: diagnoseOutcomeConnected}},
				"totals": map[string]int{"connected": 1},
			}}
		},
	}, &stdout, &stderr
}

func peerTestTargetReport(t *testing.T, verdict string) []byte {
	t.Helper()
	source := expectSource{Address: "10.0.0.5", Local: "10.0.0.9", Interface: "eth0", Verdict: verdict}
	source.Attempts, source.SYNIn = 1, 1
	if verdict == expectVerdictCompleted {
		source.SYNACKOut, source.Handshakes = 1, 1
	}
	result := Result{Probe: expectProbeID, Status: expectVerdictStatus(verdict), Summary: verdict, Metrics: map[string]interface{}{
		"port": 2171, "verdict": verdict, "from": "10.0.0.5", "sources": []expectSource{source}, "attempts": 1,
	}}
	data, err := json.Marshal(buildReport("0.80.1", time.Now(), map[string]interface{}{"input": "2171"}, []Result{result}, false))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPeerRunJoinsBothSidesInProcess(t *testing.T) {
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictCompleted)}
	run, stdout, stderr := peerTestRun(t, runner, "")
	if code := run.execute(context.Background()); code != 0 {
		t.Fatalf("exit = %d, stderr %s, stdout %s", code, stderr, stdout)
	}
	text := stdout.String()
	for _, want := range []string{"[db1] tcp expect :2171", "[db1] 12:00:00.000  SYN in", "dev eth0, src 10.0.0.5", "works"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if len(runner.commands) != 2 || !strings.Contains(runner.commands[1], "'--from'") || !strings.Contains(runner.commands[1], "'10.0.0.5'") {
		t.Fatalf("commands = %q", runner.commands)
	}

	runner = &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictRST)}
	run, stdout, _ = peerTestRun(t, runner, "-")
	if code := run.execute(context.Background()); code != 1 {
		t.Fatalf("exit = %d, want 1 for a target_host join", code)
	}
	var combined struct {
		Client Report     `json:"client"`
		Target Report     `json:"target"`
		Join   reportJoin `json:"join"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &combined); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	// 시도는 연결됐다고 하고 대상은 RST를 봤으므로 같은 흐름이 아니다.
	if combined.Join.Segment != joinSegmentInconsistent || combined.Client.Results[0].Probe != tcpDiagnoseProbeID || combined.Target.Results[0].Probe != expectProbeID {
		t.Fatalf("combined = %+v", combined.Join)
	}
	if !combined.Join.Target.SourceMatched {
		t.Fatal("the join did not match the client source in the target report")
	}
}

func TestPeerRunWritesThreeFiles(t *testing.T) {
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictCompleted)}
	path := filepath.Join(t.TempDir(), "client.json")
	run, stdout, _ := peerTestRun(t, runner, path)
	run.options.common.redact = true
	if code := run.execute(context.Background()); code != 0 {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	targetPath, joinPath := peerJSONPaths(path)
	for _, file := range []string{path, targetPath} {
		report, err := loadReport(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if !report.Redaction.Enabled {
			t.Errorf("%s is not redacted", file)
		}
	}
	data, err := os.ReadFile(joinPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "10.0.0.5") || !strings.Contains(string(data), `"segment": "works"`) {
		t.Fatalf("join file = %s", data)
	}
	if !strings.Contains(stdout.String(), joinPath) {
		t.Fatalf("output does not name %s:\n%s", joinPath, stdout)
	}
}

func TestPeerRunStopsAtPreflightAndEarlyExit(t *testing.T) {
	setLanguage("en")
	run, _, stderr := peerTestRun(t, &fakePeerRunner{preflight: "uid=0\nedc=\n"}, "")
	if code := run.execute(context.Background()); code != 2 || !strings.Contains(stderr.String(), peerInstallHint) {
		t.Fatalf("missing edc: exit %d, %s", code, stderr)
	}
	run, _, stderr = peerTestRun(t, &fakePeerRunner{}, "")
	if code := run.execute(context.Background()); code != 2 || !strings.Contains(stderr.String(), "ssh") {
		t.Fatalf("ssh failure: exit %d, %s", code, stderr)
	}
	run, _, stderr = peerTestRun(t, &fakePeerRunner{preflight: "uid=0\nedc=/usr/bin/edc\nversion=edc 0.80.1\n", header: false}, "")
	if code := run.execute(context.Background()); code != 2 || !strings.Contains(stderr.String(), "tcp expect needs root") {
		t.Fatalf("early exit: exit %d, %s", code, stderr)
	}
}

// 원격 expect의 종료 안내는 report를 손으로 합치라고 시킨다. --peer는 스스로 합치므로 그 줄을 보이지 않는다.
func TestPeerLineShownHidesTheRemoteNextSteps(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	for _, language := range []string{"en", "ko"} {
		setLanguage(language)
		hidden := []string{
			T("expect.next.step", T("expect.next.rst")),
			T("expect.next.join_rerun", "edc report join <client.json> target.json"),
			T("expect.live.waiting", 2171, "edc tcp check --diagnose 10.0.0.1:2171"),
		}
		for _, line := range hidden {
			if peerLineShown(line) {
				t.Errorf("%s: %q is shown", language, line)
			}
		}
		for _, line := range []string{T("expect.live.drop_on"), "12:00:00.000  SYN in  eth0  10.0.0.2:5000 → 10.0.0.1:2171"} {
			if !peerLineShown(line) {
				t.Errorf("%s: %q is hidden", language, line)
			}
		}
	}
}

// 로컬 시도가 끝난 뒤 원격 report를 기다리는 사이의 Ctrl-C도 취소로 끝나야 한다. 놓치면 결과가 정상 종료 코드로 나온다.
func TestPeerRunTreatsACtrlCAfterTheAttemptsAsCancelled(t *testing.T) {
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictCompleted)}
	run, stdout, stderr := peerTestRun(t, runner, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := run.diagnose
	run.diagnose = func(ctx context.Context, show func(Result)) Result {
		result := attempts(ctx, show)
		time.AfterFunc(20*time.Millisecond, cancel)
		return result
	}
	if code := run.execute(ctx); code != 4 {
		t.Fatalf("exit = %d, want 4; stderr %s", code, stderr)
	}
	if !strings.Contains(stdout.String(), "works") {
		t.Errorf("the joined result must still show:\n%s", stdout)
	}
}

// 원격이 header 없이 줄을 끝없이 내도 모아 두는 줄과 조각은 상한을 넘지 않는다.
func TestPeerLinesBoundsWhatTheyKeep(t *testing.T) {
	lines := newPeerLines("2171", func(string) {})
	for index := 0; index < peerEarlyLines*3; index++ {
		_, _ = lines.Write([]byte("noise line\n"))
	}
	if len(lines.early) != peerEarlyLines {
		t.Errorf("early = %d lines, want %d", len(lines.early), peerEarlyLines)
	}
	_, _ = lines.Write(bytes.Repeat([]byte("x"), peerLineLimit+10))
	if len(lines.partial) != 0 {
		t.Errorf("partial = %d bytes, want 0 after the limit", len(lines.partial))
	}
}

// 원격 결과 줄 아래의 next:는 이 host의 판정과 겹친다. --peer 화면에는 한 번만 보인다.
func TestPeerLineShownHidesTheRemoteNextLine(t *testing.T) {
	if peerLineShown("      next: edc listen") {
		t.Error("the remote next line is shown")
	}
	if !peerLineShown("FAIL  tcp.expect                10.0.0.5, eth0: SYN 3 arrived") {
		t.Error("the remote result line is hidden")
	}
}

// 원격 report에서 그리는 대상 결과 줄도 next를 보이지 않는다. 이 host의 next와 겹친다.
func TestPeerSummaryShowsOneNextLine(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	report := peerTestTargetReport(t, expectVerdictRST)
	var parsed Report
	if err := json.Unmarshal(report, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed.Results[0].Next = "edc listen"
	report, _ = json.Marshal(parsed)
	runner := &fakePeerRunner{preflight: preflight, header: true, report: report}
	run, stdout, _ := peerTestRun(t, runner, "")
	run.execute(context.Background())
	if strings.Contains(stdout.String(), "next: edc listen\n") {
		t.Errorf("the remote next line is shown:\n%s", stdout)
	}
}

// 첫 연결 시도 전에 멈추면 취소 한 줄만으로는 무엇이 됐는지 알 수 없다. 판정할 결과가 없다고 알린다.
func TestPeerRunSaysWhenItStopsBeforeTheFirstAttempt(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictCompleted)}
	run, _, stderr := peerTestRun(t, runner, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := run.execute(ctx); code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if !strings.Contains(stderr.String(), T("diagnose.peer.cancelled_early")) {
		t.Errorf("stderr lacks the early-stop note:\n%s", stderr)
	}
}

// 원격이 준비되기를 기다리는 동안 Ctrl-C가 오면, 원격을 멈추고 시도 전에 멈췄다고 알린다. jw-server에서 원격 준비와
// Ctrl-C가 겹쳤을 때 결과 없이 끝나던 경우다.
func TestPeerRunSaysWhenItStopsWhileWaitingForTheRemote(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.80.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, silent: true}
	run, stdout, stderr := peerTestRun(t, runner, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(30*time.Millisecond, cancel)
	if code := run.execute(ctx); code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("the remote expect did not start: %q", runner.commands)
	}
	if !strings.Contains(stderr.String(), T("diagnose.peer.cancelled_early")) {
		t.Errorf("stderr lacks the early-stop note:\n%s", stderr)
	}
	if strings.Contains(stdout.String(), "route") {
		t.Errorf("no local attempt may start:\n%s", stdout)
	}
}
