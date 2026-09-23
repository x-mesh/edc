package edc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTarget(t *testing.T) {
	tests := []struct {
		input, host, address, rawURL string
	}{
		{"example.com", "example.com", "example.com:443", "https://example.com"},
		{"https://example.com:8443/health", "example.com", "example.com:8443", "https://example.com:8443/health"},
		{"example.com:9443", "example.com", "example.com:9443", "https://example.com:9443"},
		{"2001:db8::1", "2001:db8::1", "[2001:db8::1]:443", "https://[2001:db8::1]"},
	}
	for _, test := range tests {
		host, address, rawURL, err := normalizeTarget(test.input)
		if err != nil {
			t.Fatalf("normalizeTarget(%q): %v", test.input, err)
		}
		if host != test.host || address != test.address || rawURL != test.rawURL {
			t.Errorf("normalizeTarget(%q) = %q, %q, %q", test.input, host, address, rawURL)
		}
	}
	if _, _, _, err := normalizeTarget("ftp://example.com/file"); err == nil {
		t.Fatal("unsupported URL scheme must fail")
	}
}

func TestProbeHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	result := probeHTTP(context.Background(), server.URL)
	if result.Status != StatusPass {
		t.Fatalf("status = %s, error = %#v", result.Status, result.Error)
	}
	if result.Metrics["status_code"] != http.StatusNoContent {
		t.Errorf("status_code = %#v", result.Metrics["status_code"])
	}
	if result.Metrics["peer_ip"] == "" || !strings.Contains(result.Summary, "TTFB") {
		t.Errorf("HTTP connection details = %#v", result)
	}
}

func TestProbeHTTPRedirectShowsFinalURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/final", http.StatusFound)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	result := probeHTTP(context.Background(), server.URL+"/start")
	if result.Status != StatusPass || result.Metrics["redirects"] != 1 || !strings.Contains(result.Summary, server.URL+"/final") || !strings.Contains(result.Summary, "TTFB") || !strings.Contains(result.Summary, T("observe.probe.final_request")) {
		t.Fatalf("redirect result = %#v", result)
	}
}

func TestProbeTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	result := probeTLS(context.Background(), strings.TrimPrefix(server.URL, "https://"), "example.invalid")
	if result.Status != StatusFail || result.Error == nil || result.Error.Kind != "tls" {
		t.Fatalf("self-signed certificate should fail: %#v", result)
	}
}

func TestRedactReport(t *testing.T) {
	hostname, _ := os.Hostname()
	report := Report{
		Host:    map[string]interface{}{"hostname": hostname},
		Target:  map[string]interface{}{"address": "192.0.2.10:443"},
		Results: []Result{{Summary: "route via 2001:db8::1 and 192.0.2.10"}},
	}
	redactReport(&report)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, secret := range []string{hostname, "192.0.2.10", "2001:db8::1"} {
		if secret != "" && strings.Contains(text, secret) {
			t.Errorf("redaction leaked %q in %s", secret, text)
		}
	}
	address, _ := report.Target["address"].(string)
	if !strings.Contains(address, "<ip:") {
		t.Errorf("redaction token missing: %s", address)
	}
}

func TestObservationRedactionIsOptIn(t *testing.T) {
	previousConfig, previousOutput := activeConfig, os.Stdout
	activeConfig = edcConfig{}
	defer func() { activeConfig, os.Stdout = previousConfig, previousOutput }()
	probe := func(context.Context, string) Result {
		return Result{Probe: "dns.lookup", Status: StatusPass, Summary: "example.test → 192.0.2.10", Metrics: map[string]interface{}{"addresses": []string{"192.0.2.10"}}}
	}
	terminal := func(args ...string) string {
		t.Helper()
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = write
		if code := runTargetProbe(args, "test", "dns lookup", "dns.lookup", "host", probe); code != 0 {
			t.Fatalf("terminal exit code = %d", code)
		}
		write.Close()
		defer read.Close()
		output, err := io.ReadAll(read)
		if err != nil {
			t.Fatal(err)
		}
		return string(output)
	}
	if output := terminal("example.test"); !strings.Contains(output, "192.0.2.10") {
		t.Fatalf("default output = %q", output)
	}
	if output := terminal("--redact", "example.test"); strings.Contains(output, "192.0.2.10") || !strings.Contains(output, "<ip:") {
		t.Fatalf("redacted output = %q", output)
	}
	for _, test := range []struct {
		name   string
		args   []string
		redact bool
	}{
		{"default", nil, false},
		{"redacted", []string{"--redact"}, true},
	} {
		path := filepath.Join(t.TempDir(), test.name+".json")
		args := append(append([]string{}, test.args...), "--json", path, "example.test")
		if code := runTargetProbe(args, "test", "dns lookup", "dns.lookup", "host", probe); code != 0 {
			t.Fatalf("%s JSON exit code = %d", test.name, code)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Results) != 1 || report.Redaction.Enabled != test.redact || strings.Contains(report.Results[0].Summary, "192.0.2.10") == test.redact {
			t.Fatalf("%s JSON report = %#v", test.name, report)
		}
	}
}

func TestRedactReportPreservesUsernameInsideRemoteHost(t *testing.T) {
	username := os.Getenv("USER")
	if username == "" {
		t.Skip("USER is empty")
	}
	host := username + "s-macbook-pro"
	report := Report{Host: map[string]interface{}{"hostname": "local-host"}, Results: []Result{{Probe: "remote." + host + ".update", Summary: host}}}
	redactReport(&report)
	if report.Results[0].Probe != "remote."+host+".update" || report.Results[0].Summary != host {
		t.Fatalf("remote host was redacted: %#v", report.Results[0])
	}
}

func TestBuildReportAndExitCode(t *testing.T) {
	results := []Result{{Probe: "a", Status: StatusPass}, {Probe: "b", Status: StatusFail}}
	report := buildReport("test", time.Now(), nil, results, false)
	if report.Summary.Pass != 1 || report.Summary.Fail != 1 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if exitCode(results) != 1 {
		t.Fatal("failed probe must produce exit code 1")
	}
}

func TestPrintTerminalShowsFailureDetails(t *testing.T) {
	results := []Result{{
		Probe: "remote.server.update", Status: StatusFail, Summary: "command가 실패했습니다",
		Error:    &DiagnosticError{Kind: "command", Message: "exit status 127"},
		Evidence: []Evidence{{Label: "command output", Value: "gk: command not found"}},
	}}
	var output strings.Builder
	printTerminal(&output, results, false)
	for _, expected := range []string{"┌─ ERROR  server.update", "│ phase   command", "│ cause   exit status 127", "│ command output", "│   gk: command not found", "└─\n"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output %q does not contain %q", output.String(), expected)
		}
	}
}

func TestPrintTerminalDoesNotRepeatStreamedRemoteEvidence(t *testing.T) {
	results := []Result{{Probe: "remote.server.update", Status: StatusPass, Summary: "ok", Evidence: []Evidence{{Label: "output", Value: "streamed"}}}}
	var output strings.Builder
	printTerminal(&output, results, true)
	if strings.Contains(output.String(), "streamed") {
		t.Fatalf("remote evidence repeated: %q", output.String())
	}
}

func TestTerminalStatusColor(t *testing.T) {
	if got := terminalStatus(StatusPass, true); got != "\033[32mPASS\033[0m" {
		t.Fatalf("pass color = %q", got)
	}
	if got := terminalStatus(StatusFail, true); got != "\033[31mFAIL\033[0m" {
		t.Fatalf("fail color = %q", got)
	}
	if got := terminalStatus(StatusPass, false); got != "PASS" {
		t.Fatalf("plain status = %q", got)
	}
}

func TestCaptureOutputDoesNotOverwrite(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "existing")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := captureOutputPath(file.Name()); err == nil {
		t.Fatal("existing capture file must not be overwritten")
	}
}

func TestProbeHTTPExpectStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	matched := probeHTTPWithOptions(context.Background(), server.URL, httpCheckOptions{expectStatus: http.StatusNotFound})
	if matched.Status != StatusPass || matched.Metrics["expected_status"] != http.StatusNotFound {
		t.Fatalf("expected status must pass: %#v", matched)
	}
	mismatched := probeHTTPWithOptions(context.Background(), server.URL, httpCheckOptions{expectStatus: http.StatusOK})
	if mismatched.Status != StatusFail || mismatched.Error == nil || mismatched.Error.Kind != "status" || !strings.Contains(mismatched.Summary, T("observe.probe.status_mismatch", http.StatusNotFound, http.StatusOK)) {
		t.Fatalf("mismatch must fail: %#v", mismatched)
	}
}

func TestFormatResultLineKeepsOneLine(t *testing.T) {
	result := Result{Probe: "net.ping", Status: StatusFail, Summary: "signal: killed: PING example.com\n64 bytes from example.com"}
	line := formatResultLine(result, false)
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("line must stay on one line: %q", line)
	}
	if !strings.Contains(line, "signal: killed") || strings.Contains(line, "64 bytes") {
		t.Fatalf("line = %q", line)
	}
}

// 스킴 없는 입력은 http://로 본다. 지금까지는 `unsupported protocol scheme ""`로 실패했다.
// 지표의 url이 실제로 요청한 주소여야 report만 보고도 무엇을 쳤는지 알 수 있다.
func TestProbeHTTPAddsTheSchemeWhenItIsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	bare := strings.TrimPrefix(server.URL, "http://")
	result := probeHTTP(context.Background(), bare)
	if result.Status != StatusPass {
		t.Fatalf("status = %s, error = %#v", result.Status, result.Error)
	}
	if result.Metrics["url"] != server.URL {
		t.Errorf("url = %#v, want %q", result.Metrics["url"], server.URL)
	}
}

func TestWithHTTPScheme(t *testing.T) {
	tests := map[string]string{
		"naver.com":             "http://naver.com",
		"naver.com:8080/health": "http://naver.com:8080/health",
		"127.0.0.1:3000":        "http://127.0.0.1:3000",
		"[::1]:8080":            "http://[::1]:8080",
		// 대괄호가 없는 IPv6는 URL에 그대로 쓸 수 없다. normalizeTarget과 같이 감싼다.
		"::1": "http://[::1]",
		// 스킴이 있으면 손대지 않는다. 지원하지 않는 스킴은 기존 오류가 알려 준다.
		"https://naver.com": "https://naver.com",
		"http://naver.com":  "http://naver.com",
		"HTTP://naver.com":  "HTTP://naver.com",
		"ftp://naver.com":   "ftp://naver.com",
	}
	for input, want := range tests {
		if got := withHTTPScheme(input); got != want {
			t.Errorf("withHTTPScheme(%q) = %q, want %q", input, got, want)
		}
	}
}
