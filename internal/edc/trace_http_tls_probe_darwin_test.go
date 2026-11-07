//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

//go:embed testdata/darwin_tls_probe_client.c
var darwinTLSProbeClientSource string

const darwinTLSPlaintextProbe = `
#pragma D option quiet
#pragma D option strsize=4096

BEGIN { printf("EDC_PROBES_READY\n"); }

pid$target::SSL_write:entry
{
 printf("EDC_HTTPS_REQUEST:%s\n",copyinstr(arg1,arg2+1));
}

pid$target::SSL_read:entry
{ self->edc_read_buffer=arg1; }

pid$target::SSL_read:return
/self->edc_read_buffer && (int)arg1 > 0/
{
 printf("EDC_HTTPS_RESPONSE:%s\n",copyinstr(self->edc_read_buffer,(int)arg1+1));
}

pid$target::SSL_read:return
{ self->edc_read_buffer=0; }
`

func buildDarwinTLSProbeClient(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source, binary := filepath.Join(directory, "client.c"), filepath.Join(directory, "client")
	if err := os.WriteFile(source, []byte(darwinTLSProbeClientSource), 0600); err != nil {
		t.Fatal(err)
	}
	pkgconfig, err := exec.LookPath("pkg-config")
	if err != nil {
		// sudo can omit Homebrew's verified pkg-config path from PATH.
		pkgconfig = "/opt/homebrew/bin/pkg-config"
		if _, err := os.Stat(pkgconfig); err != nil {
			t.Fatal("the TLS probe requires pkg-config and OpenSSL development files")
		}
		t.Logf("pkg-config is absent from PATH; use %s", pkgconfig)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	flags, err := exec.CommandContext(ctx, pkgconfig, "--cflags", "--libs", "openssl").CombinedOutput()
	if err != nil {
		t.Fatalf("find OpenSSL compiler flags: %v\n%s", err, flags)
	}
	args := append([]string{"-O0", "-g", source, "-o", binary}, strings.Fields(string(flags))...)
	if output, err := exec.CommandContext(ctx, "/usr/bin/clang", args...).CombinedOutput(); err != nil {
		t.Fatalf("compile TLS probe fixture: %v\n%s", err, output)
	}
	return binary
}

func darwinTLSProbeServer(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	received := &atomic.Bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		received.Store(r.URL.Path == "/keyless-probe" && r.TLS != nil)
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte{'o', 0, 'k', 255})
	}))
	t.Cleanup(server.Close)
	if server.TLS.KeyLogWriter != nil {
		t.Fatal("the probe must not generate TLS key logs")
	}
	return server, received
}
func TestDarwinTLSProbeFixtureHTTPS(t *testing.T) {
	binary := buildDarwinTLSProbeClient(t)
	server, received := darwinTLSProbeServer(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	command.Env = append(os.Environ(), "SSLKEYLOGFILE=")
	command.Stdin = strings.NewReader("\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run HTTPS fixture: %v\n%s", err, output)
	}
	if !received.Load() || !strings.Contains(string(output), "EDC_CLIENT_READY") {
		t.Fatalf("HTTPS request=%t output=%q", received.Load(), output)
	}
}
func TestDarwinTLSPlaintextProbeLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the DTrace TLS plaintext probe requires root")
	}
	binary := buildDarwinTLSProbeClient(t)
	server, received := darwinTLSProbeServer(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := exec.CommandContext(ctx, binary, strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port))
	client.Env = append(os.Environ(), "SSLKEYLOGFILE=")
	input, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	stdout, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var clientDiagnostic bytes.Buffer
	client.Stderr = &clientDiagnostic
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	var clientErr error
	go func() { clientErr = client.Wait(); close(clientDone) }()
	defer func() { _ = client.Process.Kill(); <-clientDone }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "EDC_CLIENT_READY" {
		t.Fatalf("TLS fixture did not become ready: %v %q", err, line)
	}
	script := filepath.Join(t.TempDir(), "plaintext.d")
	if err := os.WriteFile(script, []byte(darwinTLSPlaintextProbe), 0600); err != nil {
		t.Fatal(err)
	}
	trace := exec.CommandContext(ctx, "/usr/sbin/dtrace", "-q", "-p", strconv.Itoa(client.Process.Pid), "-s", script)
	trace.Env = append(os.Environ(), "SSLKEYLOGFILE=")
	traceOutput, err := trace.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	trace.Stderr = &diagnostic
	if err := trace.Start(); err != nil {
		t.Fatal(err)
	}
	ready, traceDone := make(chan struct{}), make(chan struct{})
	var output bytes.Buffer
	var traceErr error
	go func() {
		scanner := bufio.NewScanner(traceOutput)
		announced := false
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(&output, line)
			if line == "EDC_PROBES_READY" && !announced {
				announced = true
				close(ready)
			}
		}
		scanErr := scanner.Err()
		traceErr = trace.Wait()
		if traceErr == nil {
			traceErr = scanErr
		}
		close(traceDone)
	}()
	defer func() { _ = trace.Process.Kill(); <-traceDone }()
	select {
	case <-ready:
	case <-traceDone:
		t.Fatalf("DTrace could not attach to the TLS fixture: %v\n%s\n%s", traceErr, diagnostic.String(), output.String())
	case <-ctx.Done():
		t.Fatal("DTrace TLS probes did not become ready before the deadline")
	}
	if _, err := input.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientDone:
	case <-ctx.Done():
		t.Fatal("HTTPS fixture timed out")
	}
	select {
	case <-traceDone:
	case <-time.After(500 * time.Millisecond):
		_ = trace.Process.Signal(os.Interrupt)
		<-traceDone
	}
	if clientErr != nil {
		t.Fatalf("HTTPS fixture failed: %v\n%s", clientErr, clientDiagnostic.String())
	}
	if traceErr != nil {
		t.Fatalf("DTrace failed: %v\n%s\n%s", traceErr, diagnostic.String(), output.String())
	}
	if !received.Load() || !strings.Contains(output.String(), "EDC_HTTPS_REQUEST:GET /keyless-probe HTTP/1.1") || !strings.Contains(output.String(), "EDC_HTTPS_RESPONSE:HTTP/1.1 200 OK") {
		t.Fatalf("keyless TLS plaintext was not observed: request=%t\n%s\n%s", received.Load(), diagnostic.String(), output.String())
	}
	t.Log("observed OpenSSL request and response plaintext without a TLS key log")
}

func TestDarwinTLSProbeScriptSyntax(t *testing.T) {
	script := strings.ReplaceAll(darwinTLSPlaintextProbe, "pid$target::", "profile::")
	path := filepath.Join(t.TempDir(), "syntax.d")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/dtrace", "-e", "-Z", "-s", path).CombinedOutput()
	if err != nil {
		t.Fatalf("TLS probe script syntax is invalid: %v\n%s", err, output)
	}
}
