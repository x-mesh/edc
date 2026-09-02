package edc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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
