package edc

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// whyTestWaiting은 SIGTERM을 받을 때까지 도는 가짜 자식이다. 받으면 before를 실행하고 정상 종료한다.
func whyTestWaiting(before string) string {
	return "trap '" + before + " exit 0' TERM; while :; do sleep 0.05; done"
}

// whyTestChildren은 자식 trace를 sh script로 바꾼다. scripts의 key는 trace 이름이고, 띄운 명령줄을 돌려주는 slice에 쌓는다.
func whyTestChildren(t *testing.T, scripts map[string]string) *[][]string {
	t.Helper()
	previousStart, previousPrerequisites := traceWhyStart, traceWhyPrerequisites
	t.Cleanup(func() { traceWhyStart, traceWhyPrerequisites = previousStart, previousPrerequisites })
	traceWhyPrerequisites = func() error { return nil }
	started := &[][]string{}
	traceWhyStart = func(args []string, stdout, stderr *os.File) (*exec.Cmd, error) {
		*started = append(*started, args)
		script, ok := scripts[args[1]]
		if !ok {
			return nil, errors.New("no such trace")
		}
		command := exec.Command("sh", "-c", script)
		command.Stdout, command.Stderr = stdout, stderr
		return command, command.Start()
	}
	return started
}

// whyTestRecordings는 /dns 요청 하나와 그 안의 느린 DNS 응답을 담은 raw 파일들이다. 돌려주는 값은 trace 이름별 경로다.
func whyTestRecordings(t *testing.T) map[string]string {
	t.Helper()
	summary := func(protocol string) captureSummary {
		return captureSummary{Event: "capture_summary", TraceProtocol: protocol, StartBootTimeNS: 1, EndBootTimeNS: 10_000 * whyTestMS}
	}
	return map[string]string{
		"http":  traceShowWrite(t, "http.jsonl", whyTestRequest(7, "/dns", 400, 300), summary("http")),
		"dns":   traceShowWrite(t, "dns.jsonl", whyTestDNS(7, "slow.test", 395, 290), summary("dns")),
		"mysql": traceShowWrite(t, "mysql.jsonl", summary("mysql")),
	}
}

func whyTestRun(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stderr = traceCaptureOutput(t, &os.Stderr, func() {
		stdout = traceCaptureOutput(t, &os.Stdout, func() { code = runTraceWhy(args) })
	})
	return code, stdout, stderr
}

func TestTraceWhyRecordsEveryTraceAndPrintsTheBreakdown(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	scripts := map[string]string{}
	for name, path := range whyTestRecordings(t) {
		scripts[name] = "cat " + path
	}
	started := whyTestChildren(t, scripts)
	out := filepath.Join(t.TempDir(), "incident")
	code, stdout, stderr := whyTestRun(t, "--duration", "5s", "--process", "app", "--port", "8080", "--with", "mysql:13306", "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := [][]string{
		{"trace", "http", "--raw", "--duration", "5s", "--process", "app", "--payload=false"},
		{"trace", "dns", "--raw", "--duration", "5s", "--process", "app"},
		{"trace", "mysql", "--raw", "--duration", "5s", "--process", "app", "--port", "13306"},
	}
	if !slices.EqualFunc(*started, want, slices.Equal[[]string]) {
		t.Fatalf("children = %v", *started)
	}
	if !strings.Contains(stderr, "recording http, dns, mysql 5s\n") || strings.Count(stderr, "waiting for the traces to stop") != 1 {
		t.Fatalf("stderr = %q", stderr)
	}
	for _, text := range []string{"app\tGET /dns\t1\t300.0ms", "\t  dns slow.test A\t1\t290.0ms\t\t97%", "saved: " + out} {
		if !strings.Contains(stdout, text) {
			t.Fatalf("stdout %q does not contain %q", stdout, text)
		}
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory = %v, %v", info, err)
	}
	for _, name := range []string{"http.jsonl", "http.err", "mysql.jsonl", "why.json"} {
		if info, err := os.Stat(filepath.Join(out, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s = %v, %v", name, info, err)
		}
	}
	var manifest traceWhyManifest
	data, err := os.ReadFile(filepath.Join(out, traceWhyManifestName))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatalf("manifest = %q, %v", data, err)
	}
	if manifest.Version != 1 || manifest.Port != 8080 || manifest.Process != "app" || len(manifest.Traces) != 3 || manifest.Traces[2] != (traceWhyManifestTrace{"mysql", "mysql.jsonl"}) {
		t.Fatalf("manifest = %+v", manifest)
	}

	// 다시 보기는 why.json의 port를 쓴다. 이 기록의 요청은 8080으로 왔으므로 다른 port로 기록했다면 요청이 없다.
	again := traceCaptureOutput(t, &os.Stdout, func() {
		if code := runTraceShow([]string{out}); code != 0 {
			t.Errorf("show exit = %d", code)
		}
	})
	if again+"\nsaved: "+out+"\n" != stdout {
		t.Fatalf("trace show printed %q, trace why printed %q", again, stdout)
	}
}

// 자식 하나가 실패하면 나머지를 멈추고 분석 없이 끝난다. 한 계층이 빠진 기록은 그 계층의 시간을 자체 시간으로 보이게 한다.
func TestTraceWhyStopsWhenOneTraceFails(t *testing.T) {
	recordings := whyTestRecordings(t)
	whyTestChildren(t, map[string]string{
		"http": whyTestWaiting("cat " + recordings["http"] + ";"),
		"dns":  "echo 'kernel BTF is missing' >&2; exit 3",
	})
	out := filepath.Join(t.TempDir(), "incident")
	began := time.Now()
	code, stdout, stderr := whyTestRun(t, "--out", out)
	if code != 1 || stdout != "" {
		t.Fatalf("exit = %d, stdout %q", code, stdout)
	}
	for _, text := range []string{"kernel BTF is missing", "trace dns: exit status 3", out} {
		if !strings.Contains(stderr, text) {
			t.Fatalf("stderr %q does not contain %q", stderr, text)
		}
	}
	if time.Since(began) > 5*time.Second {
		t.Fatalf("the other traces were not stopped: %s", time.Since(began))
	}
	if _, err := os.Stat(filepath.Join(out, "http.jsonl")); err != nil {
		t.Fatalf("the recording was not kept: %v", err)
	}
}

func TestTraceWhyStopsWhenATraceCannotStart(t *testing.T) {
	started := whyTestChildren(t, map[string]string{"http": whyTestWaiting("")})
	out := filepath.Join(t.TempDir(), "incident")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	err := recordTraceWhy(traceWhyPlan{traces: []traceWhyTrace{{name: "http"}, {name: "dns"}}, out: out}, nil, nil)
	var failure *traceWhyChildError
	if !errors.As(err, &failure) || failure.name != "dns" || len(*started) != 2 {
		t.Fatalf("error = %v, started %v", err, *started)
	}
}

// 멈추라는 신호를 받으면 자식이 요약 줄까지 쓰고 끝나기를 기다린다. 그때까지의 기록은 분석할 수 있다.
func TestTraceWhyKeepsTheRecordingWhenItIsStopped(t *testing.T) {
	scripts := map[string]string{}
	for name, path := range whyTestRecordings(t) {
		scripts[name] = whyTestWaiting("cat " + path + ";")
	}
	whyTestChildren(t, scripts)
	out := filepath.Join(t.TempDir(), "incident")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(stop) })
	if err := recordTraceWhy(traceWhyPlan{traces: []traceWhyTrace{{name: "http"}, {name: "dns"}}, out: out}, stop, nil); err != nil {
		t.Fatal(err)
	}
	files, _, err := readTraceWhyDirectory(out)
	if err != nil {
		t.Fatal(err)
	}
	report, err := analyzeTraceWhy(files, traceWhyOptions{})
	if err != nil || report.Requests != 1 || report.Calls != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
}

func TestTraceWhyKillsATraceThatDoesNotStop(t *testing.T) {
	previous := traceWhyStopTimeout
	t.Cleanup(func() { traceWhyStopTimeout = previous })
	traceWhyStopTimeout = 300 * time.Millisecond
	whyTestChildren(t, map[string]string{"http": whyTestWaiting(""), "dns": "trap '' TERM; exec sleep 30"})
	out := filepath.Join(t.TempDir(), "incident")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	// 가짜 자식이 trap을 건 뒤에 멈춘다. 그 전에 온 SIGTERM은 자식을 바로 끝낸다.
	stop := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(stop) })
	began := time.Now()
	notices := 0
	err := recordTraceWhy(traceWhyPlan{traces: []traceWhyTrace{{name: "http"}, {name: "dns"}}, out: out}, stop, func() { notices++ })
	var failure *traceWhyChildError
	if !errors.As(err, &failure) || failure.name != "dns" || !strings.Contains(err.Error(), "did not stop within") {
		t.Fatalf("error = %v", err)
	}
	if time.Since(began) > 10*time.Second || notices != 1 {
		t.Fatalf("the stuck trace was not killed in time: %s, %d notices", time.Since(began), notices)
	}
}

func TestTraceWhyRejectsBadOptionsBeforeItStartsATrace(t *testing.T) {
	started := whyTestChildren(t, map[string]string{})
	existing := t.TempDir()
	fresh := filepath.Join(t.TempDir(), "incident")
	for name, args := range map[string][]string{
		"existing --out":     {"--out", existing},
		"unknown database":   {"--out", fresh, "--with", "clickhouse"},
		"database twice":     {"--out", fresh, "--with", "mysql,mysql:3307"},
		"database port":      {"--out", fresh, "--with", "redis:70000"},
		"empty database":     {"--out", fresh, "--with", "mysql,"},
		"port":               {"--out", fresh, "--port", "0"},
		"duration":           {"--out", fresh, "--duration", "61s"},
		"positional":         {"--out", fresh, "nginx"},
		"unknown option":     {"--out", fresh, "--payload"},
		"missing parent dir": {"--out", filepath.Join(fresh, "a", "b")},
	} {
		if code, stdout, stderr := whyTestRun(t, args...); code != 2 || stdout != "" || stderr == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	if len(*started) != 0 {
		t.Fatalf("started %v", *started)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected run left %s: %v", fresh, err)
	}
}

func TestTraceWhyChecksThePlatformBeforeItRecords(t *testing.T) {
	started := whyTestChildren(t, map[string]string{})
	traceWhyPrerequisites = func() error { return errors.New("trace why requires Linux, not plan9") }
	out := filepath.Join(t.TempDir(), "incident")
	code, _, stderr := whyTestRun(t, "--out", out)
	if code != 3 || !strings.Contains(stderr, "requires Linux") || len(*started) != 0 {
		t.Fatalf("exit %d, stderr %q, started %v", code, stderr, *started)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory was made: %v", err)
	}
}

func TestParseTraceWhyWith(t *testing.T) {
	traces, err := parseTraceWhyWith(" mysql:13306, redis ,postgres")
	if err != nil || !slices.Equal(traces, []traceWhyTrace{{"mysql", 13306}, {"redis", 0}, {"postgres", 0}}) {
		t.Fatalf("traces = %v, %v", traces, err)
	}
	if traces, err := parseTraceWhyWith(""); err != nil || traces != nil {
		t.Fatalf("empty = %v, %v", traces, err)
	}
	for _, value := range []string{"http", "mysql:", "mysql:abc", "mysql:0", "redis,redis", ","} {
		if _, err := parseTraceWhyWith(value); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
}
