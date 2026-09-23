package edc

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeAllIPsUsesOriginalHostAndStopsAtRedirect(t *testing.T) {
	hosts := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hosts <- request.Host
		writer.Header().Set("Location", "/next")
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	port := fmt.Sprint(server.Listener.Addr().(*net.TCPAddr).Port)
	host := "service.example.test"
	dns := Result{Probe: "dns.lookup", Status: StatusPass, Metrics: map[string]interface{}{"addresses": []string{"127.0.0.1"}}}
	result := probeAllIPs(context.Background(), dns, host, port, "http://"+net.JoinHostPort(host, port)+"/start")
	if result.Status != StatusPass || len(hosts) != 1 {
		t.Fatalf("result = %#v, requests = %d", result, len(hosts))
	}
	if receivedHost := <-hosts; receivedHost != net.JoinHostPort(host, port) {
		t.Fatalf("request Host = %q", receivedHost)
	}
	checks, ok := result.Metrics["endpoints"].([]endpointCheck)
	if !ok || len(checks) != 1 || checks[0].TCP != StatusPass || checks[0].TLS != StatusSkip || checks[0].HTTP != StatusPass || checks[0].HTTPCode != http.StatusFound {
		t.Fatalf("endpoint checks = %#v", result.Metrics["endpoints"])
	}
	if !strings.Contains(result.Evidence[0].Value, "127.0.0.1") || !strings.Contains(result.Evidence[0].Value, "302") {
		t.Fatalf("endpoint table = %q", result.Evidence[0].Value)
	}
}

func TestProbeAllIPsSkipsAfterDNSFailure(t *testing.T) {
	result := probeAllIPs(context.Background(), Result{Status: StatusFail}, "example.test", "443", "https://example.test")
	if result.Status != StatusSkip || result.Probe != endpointProbeID {
		t.Fatalf("result = %#v", result)
	}
}

func TestProbeAllIPsPreservesTLSName(t *testing.T) {
	seen := make(chan struct{ host, sni string }, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- struct{ host, sni string }{request.Host, request.TLS.ServerName}
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	port := fmt.Sprint(server.Listener.Addr().(*net.TCPAddr).Port)
	host := "example.com"
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	dns := Result{Status: StatusPass, Metrics: map[string]interface{}{"addresses": []string{"127.0.0.1"}}}
	result := probeAllIPsWithOptions(context.Background(), dns, host, port, "https://"+net.JoinHostPort(host, port)+"/", endpointOptions{rootCAs: roots})
	if result.Status != StatusPass {
		t.Fatalf("result = %#v", result)
	}
	checks := result.Metrics["endpoints"].([]endpointCheck)
	if checks[0].TLS != StatusPass || checks[0].HTTPCode != http.StatusNoContent {
		t.Fatalf("checks = %#v", checks)
	}
	request := <-seen
	if request.host != net.JoinHostPort(host, port) || request.sni != host {
		t.Fatalf("Host = %q, SNI = %q", request.host, request.sni)
	}
}

func TestEndpointResultCountsFailures(t *testing.T) {
	result := endpointResult(time.Now(), []endpointCheck{{IP: "192.0.2.1", TCP: StatusPass, TLS: StatusPass, HTTP: StatusPass, HTTPCode: 200}, {IP: "192.0.2.2", TCP: StatusFail, TLS: StatusSkip, HTTP: StatusSkip, Detail: "connection refused"}})
	if result.Status != StatusFail || result.Error == nil || result.Metrics["fail"] != 1 || result.Metrics["pass"] != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestEndpointResultRedactsAddresses(t *testing.T) {
	result := endpointResult(time.Now(), []endpointCheck{{IP: "192.0.2.1", TCP: StatusPass, TLS: StatusSkip, HTTP: StatusPass, HTTPCode: 200}})
	report := buildReport("test", time.Now(), nil, []Result{result}, true)
	data, err := json.Marshal(report)
	if err != nil || strings.Contains(string(data), "192.0.2.1") || !strings.Contains(string(data), "ip:") {
		t.Fatalf("redacted report = %s, error = %v", data, err)
	}
}
