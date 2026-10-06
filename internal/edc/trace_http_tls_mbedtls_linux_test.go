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

func TestTraceTLSMbedTLSCapturesResetContext(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("Mbed TLS capture needs BPF and perf capabilities")
	}
	flags, err := exec.Command("pkg-config", "--cflags", "--libs", "mbedtls", "mbedx509", "mbedcrypto").Output()
	if err != nil {
		t.Skip("Mbed TLS fixture needs the installed development headers")
	}
	libraryDir, err := exec.Command("pkg-config", "--variable=libdir", "mbedtls").Output()
	if err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(strings.TrimSpace(string(libraryDir)), "libmbedtls.so")
	finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(library))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("Mbed TLS fixture needs a C compiler")
	}
	fixture := filepath.Join(t.TempDir(), "edc-wolf-client")
	args := []string{"-O2", "-Wall", "-Wextra", "testdata/mbedtls_client.c", "-o", fixture}
	args = append(args, strings.Fields(string(flags))...)
	if output, err := exec.Command(compiler, args...).CombinedOutput(); err != nil {
		t.Fatalf("Mbed TLS fixture: %v\n%s", err, output)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "mbedtls-test-response:"+r.URL.Path)
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
		t.Fatalf("Mbed TLS ready: %q, %v", line, err)
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
			if !strings.Contains(string(event.Payload), "mbedtls-test-response:") {
				t.Errorf("Mbed TLS body missing: %#v", event)
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(reader)
	if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
		t.Fatalf("Mbed TLS client: %v, %v, %s, %s", err, readErr, output, stderr.String())
	}
	if len(requests) != 6 || len(responses) != 6 || summary.LostEvents != 0 {
		t.Fatalf("Mbed TLS requests=%v responses=%v lost=%d", requests, responses, summary.LostEvents)
	}
	for path, count := range requests {
		if count != 1 || responses[path] != 1 {
			t.Fatalf("Mbed TLS duplicate or missing: %s requests=%d responses=%d", path, count, responses[path])
		}
	}
}

func TestTraceTLSMbedTLSFullWidthAndErrors(t *testing.T) {
	traceTLSBoundedABIFixture(t, false)
}

func traceTLSBoundedABIFixture(t *testing.T, outLength bool) {
	t.Helper()
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("TLS ABI fixture needs BPF and perf capabilities")
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("TLS ABI fixture needs a C compiler")
	}
	source := `
#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#ifdef OUT_LENGTH
#define READ rustls_connection_read
#define WRITE rustls_connection_write
#define FREE rustls_connection_free
#define RESET rustls_connection_free
#define EXTRA , size_t *out
#define CALL_EXTRA , &out
#define SUCCESS 7000
#else
#define READ mbedtls_ssl_read
#define WRITE mbedtls_ssl_write
#define FREE mbedtls_ssl_free
#define RESET mbedtls_ssl_session_reset
#define EXTRA
#define CALL_EXTRA
#define SUCCESS 0
#endif
__attribute__((noinline)) int READ(void *conn, unsigned char *buf, size_t len EXTRA) {
 (void)conn;
 if (!len) return -1;
 size_t n = strlen((char *)buf);
#ifdef OUT_LENGTH
 *out = n;
 return SUCCESS;
#else
 return n;
#endif
}
__attribute__((noinline)) int WRITE(void *conn, const unsigned char *buf, size_t len EXTRA) {
 (void)conn;
 if (!len) return -1;
 size_t n = strlen((char *)buf);
#ifdef OUT_LENGTH
 *out = n;
 return SUCCESS;
#else
 return n;
#endif
}
__attribute__((noinline)) void FREE(void *conn) { __asm__ volatile("" : : "r"(conn) : "memory"); }
#ifndef OUT_LENGTH
__attribute__((noinline)) int RESET(void *conn) { __asm__ volatile("" : : "r"(conn) : "memory"); return -1; }
#endif
int main(void) {
 puts("ready"); fflush(stdout); sleep(3);
 int conn = 0; size_t out = 999;
 unsigned char old[] = "GET /stale HTTP/1.1\r\nHost: localhost\r\n";
 WRITE(&conn, old, sizeof(old) CALL_EXTRA);
 RESET(&conn);
 unsigned char request[] = "GET /abi HTTP/1.1\r\nHost: localhost\r\n\r\n";
 unsigned char response[] = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok";
 WRITE(&conn, request, ((size_t)1 << 32)+1 CALL_EXTRA);
 READ(&conn, response, ((size_t)1 << 32)+1 CALL_EXTRA);
 out = 999;
 unsigned char error[] = "GET /error HTTP/1.1\r\nHost: localhost\r\n\r\n";
 WRITE(&conn, error, 0 CALL_EXTRA);
 READ(&conn, response, 0 CALL_EXTRA);
 FREE(&conn);
 puts("done"); return out == 0;
}
`
	fixture := filepath.Join(t.TempDir(), "tls-abi")
	args := []string{"-x", "c", "-", "-O0", "-o", fixture}
	if outLength {
		args = append(args, "-DOUT_LENGTH")
	}
	build := exec.Command(compiler, args...)
	build.Stdin = strings.NewReader(source)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("TLS ABI build: %v\n%s", err, output)
	}
	finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(fixture))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("TLS ABI ready: %q, %v", line, err)
	}
	requests, responses := 0, 0
	summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(fixture), tlsFinder: finder, payload: true}, 4*time.Second, func(event captureEvent) error {
		if event.PID != uint32(command.Process.Pid) || !event.TLS {
			return nil
		}
		if event.Event == "http_request" {
			requests++
			if event.Path != "/abi" && event.Path != "/stale" {
				t.Errorf("failed request: %#v", event)
			}
		} else if event.Status == 200 {
			responses++
			if event.Path != "/abi" || !strings.Contains(string(event.Payload), "ok") {
				t.Errorf("wrong ABI response: %#v", event)
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(reader)
	if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
		t.Fatalf("TLS ABI client: %v, %v, %s", err, readErr, output)
	}
	if requests != 2 || responses != 1 || summary.LostEvents != 0 {
		t.Fatalf("TLS ABI requests=%d responses=%d lost=%d", requests, responses, summary.LostEvents)
	}
}
