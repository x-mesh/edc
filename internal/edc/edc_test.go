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
