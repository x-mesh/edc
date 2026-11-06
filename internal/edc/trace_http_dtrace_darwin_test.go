//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDarwinTLSGeneratedScriptSyntax(t *testing.T) {
	script, err := darwinTLSScript([]traceTLSTarget{{path: "/test/libssl.dylib", symbols: darwinTLSFunctions}})
	if err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, "pid$target:", "profile:")
	script = strings.ReplaceAll(script, "$target", "1")
	path := filepath.Join(t.TempDir(), "syntax.d")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/dtrace", "-e", "-Z", "-S", "-s", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generated TLS probe syntax is invalid: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "! copyin") || strings.HasSuffix(strings.TrimSpace(line), "! alloca") {
			t.Fatalf("TLS script still uses a kernel-privileged memory helper: %s", line)
		}
	}
	if !strings.Contains(string(output), "uldx ") || !strings.Contains(string(output), "uldub ") || !strings.Contains(string(output), "by user ref (size 16)") || !strings.Contains(string(output), "by user ref (size 1048576)") {
		t.Fatal("TLS payload and output lengths must use user-address-space loads")
	}
}
func TestDarwinTLSMappingsAndMachO(t *testing.T) {
	mappings := darwinTLSMappings("p101\nn/tmp/server\nn/test/libssl.dylib\nn/test/libssl.dylib\np202\nn/tmp/other\npbad\nn/ignored\n")
	if len(mappings) != 2 || len(mappings[101]) != 2 || len(mappings[202]) != 1 {
		t.Fatalf("mappings=%#v", mappings)
	}
	directory := t.TempDir()
	source, binary := filepath.Join(directory, "fixture.c"), filepath.Join(directory, "fixture")
	code := "int SSL_read(void *s,void *b,int n){return n;}\nint SSL_write(void *s,const void *b,int n){return n;}\nvoid SSL_free(void *s){}\nint main(void){return 0;}\n"
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/clang", "-O0", source, "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile Mach-O fixture: %v\n%s", err, output)
	}
	symbols, err := darwinTLSFileSymbols(binary)
	if err != nil || len(symbols) != 3 {
		t.Fatalf("symbols=%q err=%v", symbols, err)
	}
	previous := readDarwinTLSMappings
	t.Cleanup(func() { readDarwinTLSMappings = previous })
	readDarwinTLSMappings = func(context.Context) (map[uint32][]string, error) { return map[uint32][]string{101: {binary}}, nil }
	finder, _, codeStatus, err := resolveTraceHTTPTLSTargets(traceTLSMode(binary), false, "")
	if err != nil || codeStatus != 0 || len(finder.targets) != 1 || finder.targets[0].pid != 101 {
		t.Fatalf("finder=%#v status=%d err=%v", finder, codeStatus, err)
	}
}
func TestDarwinTLSFindsOpenSSLFixture(t *testing.T) {
	binary := buildDarwinTLSProbeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := exec.CommandContext(ctx, binary, "443")
	input, err := client.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := client.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Process.Kill(); _ = client.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || strings.TrimSpace(line) != "EDC_CLIENT_READY" {
		t.Fatalf("fixture ready=%q err=%v", line, err)
	}
	targets, err := findDarwinTLSTargets(ctx, traceTLSAuto, "client")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range targets {
		if target.pid == uint32(client.Process.Pid) {
			found = true
			if _, err := darwinTLSScript([]traceTLSTarget{target}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatalf("OpenSSL process %d not found: %#v", client.Process.Pid, targets)
	}
}
func TestDarwinTLSAddresses(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connection, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	raw, err := connection.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var local, remote string
	if err := raw.Control(func(fd uintptr) { local, remote = darwinTLSAddresses(uint32(os.Getpid()), int32(fd)) }); err != nil {
		t.Fatal(err)
	}
	if local != connection.LocalAddr().String() || remote != connection.RemoteAddr().String() {
		t.Fatalf("addresses=%s %s want %s %s", local, remote, connection.LocalAddr(), connection.RemoteAddr())
	}
	info := make([]byte, darwinSocketFDInfoSize)
	binary.LittleEndian.PutUint32(info[darwinSoiKind:], darwinSocketKindTCP)
	proto := info[darwinSoiProto:]
	proto[darwinInVFlag] = 2
	proto[darwinInLAddr+15] = 1
	proto[darwinTLSForeignAddress+15] = 2
	binary.BigEndian.PutUint16(proto[darwinInLPort:], 50000)
	binary.BigEndian.PutUint16(proto[darwinInFPort:], 443)
	local, remote = darwinTLSAddressesFromInfo(info)
	if local != "[::1]:50000" || remote != "[::2]:443" {
		t.Fatalf("IPv6=%s %s", local, remote)
	}
}
func TestDarwinTLSUnmappedCloseAndTruncation(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	now := time.Now()
	record := darwinTLSRecord{pid: 42, wallNS: 1000000, object: 100, sent: true, fd: -1, process: "fixture", payload: []byte("GET /secure HTTP/1.1\r\nHost: example.com\r\n\r\n")}
	record.size = uint64(len(record.payload))
	if events, missing := tracker.addDarwinTLS(record, 443, now); !missing || len(events) != 0 {
		t.Fatalf("events=%#v missing=%t", events, missing)
	}
	if events, missing := tracker.addDarwinTLS(record, 0, now); missing || len(events) != 1 || !events[0].TLS {
		t.Fatalf("events=%#v missing=%t", events, missing)
	}
	record.offset = uint32(len(record.payload))
	record.size = httpMessageMax + 1
	record.payload = []byte("GET /cut HTTP/1.1\r\nContent-Length: 9999999\r\n\r\n")
	tracker.addDarwinTLS(record, 0, now)
	if tracker.lost != 1 || tracker.buffered != 0 {
		t.Fatalf("lost=%d buffered=%d", tracker.lost, tracker.buffered)
	}
	record.closed = true
	record.size = 0
	record.payload = nil
	tracker.addDarwinTLS(record, 443, now)
	if len(tracker.flows) != 0 {
		t.Fatalf("SSL_free left sessions: %#v", tracker.flows)
	}
}

func TestDarwinHTTPSDTraceLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("live keyless HTTPS capture requires root")
	}
	for _, mode := range []string{"classic", "ex"} {
		t.Run(mode, func(t *testing.T) {
			binary := buildDarwinTLSProbeClient(t)
			server, received := darwinTLSProbeServer(t, 30*time.Millisecond)
			port := server.Listener.Addr().(*net.TCPAddr).Port
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client := exec.CommandContext(ctx, binary, strconv.Itoa(port), mode, "hold")
			client.Env = append(os.Environ(), "SSLKEYLOGFILE=")
			input, err := client.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := client.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			client.Stderr = &diagnostic
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var clientErr error
			go func() { clientErr = client.Wait(); close(done) }()
			defer func() { _ = client.Process.Kill(); <-done }()
			if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || strings.TrimSpace(line) != "EDC_CLIENT_READY" {
				t.Fatalf("fixture ready=%q err=%v", line, err)
			}
			targets, err := findDarwinTLSTargets(ctx, traceTLSAuto, "client")
			if err != nil {
				t.Fatal(err)
			}
			var selected []traceTLSTarget
			for _, target := range targets {
				if target.pid == uint32(client.Process.Pid) {
					selected = append(selected, target)
				}
			}
			if len(selected) == 0 {
				t.Fatal("OpenSSL TLS fixture was not discovered")
			}
			previous := captureAttachedHook
			t.Cleanup(func() { captureAttachedHook = previous })
			captureAttachedHook = func() {
				if _, err := input.Write([]byte("\n")); err != nil {
					t.Error(err)
				}
			}
			api, err := loadDarwinHTTPPCAP()
			if err != nil {
				t.Fatal(err)
			}
			originalStats := api.stats
			var packetStats darwinPCAPStats
			api.stats = func(handle uintptr, stats *darwinPCAPStats) int32 {
				result := originalStats(handle, stats)
				packetStats = *stats
				return result
			}
			t.Cleanup(func() { api.stats = originalStats })
			request, response, released := false, false, false
			summary, err := collectDarwinHTTPEvents(traceScope{protocol: "http", tls: traceTLSMode(selected[0].path), tlsFinder: &traceTLSFinder{targets: selected}, port: uint16(port), payload: true, payloadAll: true}, 2*time.Second, func(event captureEvent) error {
				if !event.TLS || event.PID != uint32(client.Process.Pid) {
					return nil
				}
				if strings.Contains(event.Payload, "Bearer fixture-secret") {
					return fmt.Errorf("TLS authorization header was not masked")
				}
				if event.Path == "/keyless-probe" {
					request = request || event.Method == "GET" && event.Status == 0
					response = response || event.Status == 200
				}
				if event.Status == 200 && !strings.Contains(event.Payload, traceEscapeText([]byte{'o', 0, 'k', 255})) {
					return fmt.Errorf("TLS binary response body was not preserved")
				}
				if event.Status == 200 && (event.LatencyMS == nil || *event.LatencyMS < 20) {
					return fmt.Errorf("TLS response latency did not include the server delay: %v", event.LatencyMS)
				}
				if response && !released {
					released = true
					_, err := input.Write([]byte("\n"))
					return err
				}
				return nil
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			<-done
			if clientErr != nil {
				t.Fatalf("HTTPS fixture failed: %v\n%s", clientErr, diagnostic.String())
			}
			if !received.Load() || !request || !response || summary.TLSUnmapped != 0 || summary.LostEvents != 0 {
				t.Fatalf("HTTPS=%t request=%t response=%t pcap=%#v decoder_loss=%d summary=%#v", received.Load(), request, response, packetStats, summary.LostEvents-uint64(packetStats.dropped)-uint64(packetStats.interfaceDropped), summary)
			}
			t.Log("captured HTTPS request and response without a key log, with port filtering and header masking")
		})
	}
}

func TestDarwinTLSAttachFailuresAreVisible(t *testing.T) {
	for _, test := range []struct {
		name                  string
		implicit, ready, fail bool
	}{{"implicit-initial", true, false, false}, {"explicit-initial", false, false, true}, {"implicit-running", true, true, true}} {
		t.Run(test.name, func(t *testing.T) {
			capture, err := newDarwinTLSCapture(traceScope{})
			if err != nil {
				t.Fatal(err)
			}
			defer capture.stop()
			capture.scope.implicitTLS = test.implicit
			done := make(chan struct{})
			close(done)
			worker := &darwinTLSWorker{done: done, err: fmt.Errorf("target rejects tracing")}
			worker.ready.Store(test.ready)
			capture.workers[42] = worker
			tracker := newPKTAPHTTPTracker(traceScope{}, 0)
			err = capture.poll(tracker, func([]captureEvent) error { return nil }, time.Now())
			if (err != nil) != test.fail {
				t.Fatalf("error=%v want failure=%t", err, test.fail)
			}
			if !test.fail {
				notices, _ := capture.notices.summary()
				if len(notices) != 1 || !strings.Contains(notices[0], "target rejects tracing") {
					t.Fatalf("notices=%q", notices)
				}
			}
		})
	}
}
func TestDarwinTLSDiagnosticsDetectDropsAfterTruncation(t *testing.T) {
	var diagnostic darwinTLSDiagnostics
	_, _ = diagnostic.Write(bytes.Repeat([]byte{'x'}, 8192))
	_, _ = diagnostic.Write([]byte("\ndtrace: 2 dr"))
	_, _ = diagnostic.Write([]byte("ops on CPU 0\n"))
	_, dropped := diagnostic.snapshot()
	if !dropped {
		t.Fatal("DTrace drops after the diagnostic limit were not detected")
	}
}
