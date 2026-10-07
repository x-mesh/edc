//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraceTLSWolfSSLCapturesBothAPIs(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("wolfSSL capture needs BPF and perf capabilities")
	}
	flags, err := exec.Command("pkg-config", "--cflags", "--libs", "wolfssl").Output()
	if err != nil {
		t.Skip("wolfSSL fixture needs the installed development headers")
	}
	libraryDir, err := exec.Command("pkg-config", "--variable=libdir", "wolfssl").Output()
	if err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(strings.TrimSpace(string(libraryDir)), "libwolfssl.so")
	finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(library))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("wolfSSL fixture needs a C compiler")
	}
	fixture := filepath.Join(t.TempDir(), "edc-wolf-client")
	args := []string{"-O2", "-Wall", "-Wextra", "testdata/wolfssl_client.c", "-o", fixture}
	args = append(args, strings.Fields(string(flags))...)
	if output, err := exec.Command(compiler, args...).CombinedOutput(); err != nil {
		t.Fatalf("wolfSSL fixture: %v\n%s", err, output)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "wolfssl-test-response:"+r.URL.Path)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture, port)
	command.Env = []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=" + strings.TrimSpace(string(libraryDir))}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("wolfSSL ready: %q, %v", line, err)
	}
	requests, responses := map[string]int{}, map[string]int{}
	summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(library), tlsFinder: finder, payload: true}, 5*time.Second, func(event captureEvent) error {
		if event.PID != uint32(command.Process.Pid) || !event.TLS {
			return nil
		}
		if event.Event == "http_request" {
			requests[event.Path]++
		} else if event.Status == 200 {
			responses[event.Path]++
			if !strings.Contains(string(event.Payload), "wolfssl-test-response:") {
				t.Errorf("wolfSSL body missing: %#v", event)
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(reader)
	if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
		t.Fatalf("wolfSSL client: %v, %v, %s, %s", err, readErr, output, stderr.String())
	}
	if len(requests) != 6 || len(responses) != 6 || summary.LostEvents != 0 {
		t.Fatalf("wolfSSL requests=%v responses=%v lost=%d", requests, responses, summary.LostEvents)
	}
	for path, count := range requests {
		if count != 1 || responses[path] != 1 {
			t.Fatalf("wolfSSL duplicate or missing: %s requests=%d responses=%d", path, count, responses[path])
		}
	}
}
