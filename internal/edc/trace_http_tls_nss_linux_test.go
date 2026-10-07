//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func traceTLSNSSFixture(t *testing.T) (string, *traceTLSFinder) {
	t.Helper()
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("NSS capture needs BPF and perf capabilities")
	}
	ssl, _ := traceTLSHostNSS(t)
	finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(ssl))
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("NSS fixture needs a C compiler")
	}
	flags, err := exec.Command("pkg-config", "--cflags", "--libs", "nss").Output()
	if err != nil {
		t.Skip("NSS fixture needs the installed NSS development headers")
	}
	path := filepath.Join(t.TempDir(), "edc-nss-client")
	args := []string{"-O2", "-Wall", "-Wextra", "testdata/nss_client.c", "-o", path}
	args = append(args, strings.Fields(string(flags))...)
	if output, err := exec.Command(compiler, args...).CombinedOutput(); err != nil {
		t.Fatalf("NSS fixture: %v\n%s", err, output)
	}
	return path, finder
}

func TestTraceTLSNSSCapturesOnlySecureIO(t *testing.T) {
	fixture, finder := traceTLSNSSFixture(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "nss-test-response:"+r.URL.Path)
	})
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	plain := httptest.NewServer(handler)
	defer plain.Close()
	port := func(url string) string {
		_, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://"))
		if err != nil {
			t.Fatal(err)
		}
		return port
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture, port(secure.URL), port(plain.URL))
	command.Dir = t.TempDir()
	command.Env = []string{"PATH=/usr/bin:/bin"}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel() })
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("NSS ready: %q, %v, %s", line, err, stderr.String())
	}
	var events []captureEvent
	summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(finder.targets[0].path), tlsFinder: finder, payload: true}, 5*time.Second, func(event captureEvent) error {
		if event.PID == uint32(command.Process.Pid) {
			events = append(events, event)
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(reader)
	if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
		t.Fatalf("NSS client: %v, %v, %s, %s", err, readErr, output, stderr.String())
	}
	requests, responses := map[string]int{}, map[string]int{}
	for _, event := range events {
		if event.Path == "/nss-plain-file" {
			t.Fatalf("file I/O became HTTP: %#v", event)
		}
		if !event.TLS {
			continue
		}
		if !strings.HasPrefix(event.Path, "/nss-tls-") {
			t.Fatalf("plain or disabled NSS I/O became TLS: %#v", event)
		}
		if event.Event == "http_request" {
			requests[event.Path]++
		} else if event.Status == 200 {
			responses[event.Path]++
			if !strings.Contains(string(event.Payload), "nss-test-response:") {
				t.Fatalf("NSS response body missing: %#v", event)
			}
		}
	}
	if len(requests) != 6 || len(responses) != 6 || summary.LostEvents != 0 {
		t.Fatalf("NSS requests=%v responses=%v lost=%d stderr=%s", requests, responses, summary.LostEvents, stderr.String())
	}
	for path, count := range requests {
		if count != 1 || responses[path] != 1 {
			t.Fatalf("duplicate, peek, or reused descriptor: %s requests=%d responses=%d", path, count, responses[path])
		}
	}
}

func TestTraceTLSNSSAcceptsASecureConnection(t *testing.T) {
	fixture, finder := traceTLSNSSFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	for _, data := range [][]byte{certificate, privateKey} {
		if err := binary.Write(&input, binary.BigEndian, uint32(len(data))); err != nil {
			t.Fatal(err)
		}
		input.Write(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, fixture, "server")
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.Stdin = &input
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
		t.Fatalf("NSS server ready: %q, %v, %s", line, err, stderr.String())
	}
	clientDone := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			clientDone <- err
			return
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "listen" {
			clientDone <- fmt.Errorf("NSS listen: %q", line)
			return
		}
		// 서버 인증서와 key는 이 테스트가 메모리에서 생성하며 외부 trust DB를 사용하지 않는다.
		transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		response, err := client.Get("https://127.0.0.1:" + fields[1] + "/nss-server")
		if err != nil {
			clientDone <- err
			return
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err == nil && string(body) != "nss-server-response" {
			err = fmt.Errorf("NSS server response: %q", body)
		}
		if _, readErr := io.ReadAll(reader); err == nil {
			err = readErr
		}
		clientDone <- err
	}()
	requests, responses := 0, 0
	summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(finder.targets[0].path), tlsFinder: finder, payload: true}, 5*time.Second, func(event captureEvent) error {
		if event.PID == uint32(command.Process.Pid) && event.TLS && event.Path == "/nss-server" {
			if event.Event == "http_request" {
				requests++
			} else if event.Status == 200 {
				responses++
			}
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("NSS server: %v, %s", err, stderr.String())
	}
	if requests != 1 || responses != 1 || summary.LostEvents != 0 {
		t.Fatalf("NSS accepted connection: requests=%d responses=%d lost=%d", requests, responses, summary.LostEvents)
	}
}
