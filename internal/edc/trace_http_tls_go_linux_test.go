//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func traceTLSGoFixture(t *testing.T, goCommand, mode string) string {
	t.Helper()
	return traceTLSGoBuildFixture(t, goCommand, "testdata/go_tls_client.go", mode, runtime.GOARCH, true)
}

const traceTLSGoNoTLSSource = "package main\n\nfunc main() {}\n"

func TestTraceTLSGoNoTLSProgram(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	if !traceTLSGoVersions[traceTLSGoVersion(t, goCommand)] || runtime.GOARCH != "amd64" {
		t.Skip("no-TLS fixture needs a supported amd64 compiler")
	}
	source := filepath.Join(t.TempDir(), "edc-go-no-tls.go")
	if err := os.WriteFile(source, []byte(traceTLSGoNoTLSSource), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := traceTLSGoBuildFixture(t, goCommand, source, "normal", "amd64", true)
	target, err := traceTLSReadFile(fixture, true)
	if err != nil || len(target.symbols) != 0 || len(target.offsets) != 0 || len(target.goReturns) != 0 {
		t.Fatalf("no-TLS target=%+v error=%v", target, err)
	}
	finder := &traceTLSFinder{}
	finder.addGoExecutable(fixture)
	if len(finder.targets) != 0 || len(finder.notices) != 0 {
		t.Fatalf("no-TLS finder targets=%d notices=%q", len(finder.targets), finder.notices)
	}
	if _, _, code, err := resolveTraceTLSTargets(traceTLSMode(fixture)); code != 2 || err == nil {
		t.Fatalf("explicit no-TLS code=%d error=%v", code, err)
	}
}

func traceTLSGoBuildFixture(t *testing.T, goCommand, source, mode, arch string, stdlib bool) string {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "edc-go-tls")
	args := []string{"build", "-o", fixture}
	if mode == "stripped" {
		args = append(args, "-ldflags=-s -w")
	} else if mode == "pie" {
		args = append(args, "-buildmode=pie")
	}
	args = append(args, source)
	command := exec.Command(goCommand, args...)
	command.Env = traceTLSGoCompilerEnv(stdlib, "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Go TLS fixture: %v\n%s", err, output)
	}
	info, err := buildinfo.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if version := traceTLSGoVersion(t, goCommand); info.GoVersion != version {
		t.Fatalf("Go TLS fixture compiler=%s buildinfo=%s", version, info.GoVersion)
	}
	return fixture
}

func traceTLSGoCompilerEnv(stdlib bool, extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+4)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key == "GOROOT" || key == "GOOS" || key == "GOARCH" || key == "GO111MODULE" || key == "GOWORK" || key == "GOTOOLCHAIN" || key == "CGO_ENABLED" || key == "GOFLAGS" || key == "GOENV" {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off")
	if stdlib {
		env = append(env, "GO111MODULE=off")
	}
	return append(env, extra...)
}

func traceTLSGoCommand(t *testing.T) string {
	t.Helper()
	if command := os.Getenv("EDC_TEST_GO"); command != "" {
		return command
	}
	return "go"
}

func traceTLSGoVersion(t *testing.T, goCommand string) string {
	t.Helper()
	command := exec.Command(goCommand, "env", "GOVERSION")
	command.Env = traceTLSGoCompilerEnv(false)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func TestTraceTLSGoFixtureMetadata(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	for _, arch := range []string{"amd64", "arm64"} {
		for _, mode := range []string{"normal", "stripped", "pie"} {
			t.Run(arch+"/"+mode, func(t *testing.T) {
				fixture := traceTLSGoBuildFixture(t, goCommand, "testdata/go_tls_client.go", mode, arch, true)
				info, err := buildinfo.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				file, err := elf.Open(fixture)
				if err != nil {
					t.Fatal(err)
				}
				wantMachine := elf.EM_X86_64
				if arch == "arm64" {
					wantMachine = elf.EM_AARCH64
				}
				wantType := elf.ET_EXEC
				if mode == "pie" {
					wantType = elf.ET_DYN
				}
				if file.Machine != wantMachine || file.Type != wantType {
					t.Fatalf("ELF metadata machine=%s type=%s", file.Machine, file.Type)
				}
				target := traceTLSTarget{path: fixture}
				parseErr := traceTLSGo(file, &target)
				if !traceTLSGoVersions[info.GoVersion] {
					file.Close()
					if parseErr == nil || !strings.Contains(parseErr.Error(), "unsupported Go TLS ABI") {
						t.Fatalf("unsupported ABI accepted: %s %v", info.GoVersion, parseErr)
					}
					return
				}
				section := file.Section(".gopclntab")
				if section == nil {
					section = file.Section(".data.rel.ro.gopclntab")
				}
				if section == nil {
					file.Close()
					t.Fatal("missing Go function table")
				}
				offset := section.Offset
				file.Close()
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
					if _, ok := target.offsets[name]; !ok {
						t.Errorf("missing entry: %s", name)
					}
					if name != traceTLSGoClose && len(target.goReturns[name]) == 0 {
						t.Errorf("missing returns: %s", name)
					}
				}
				if mode != "normal" || arch != "amd64" {
					return
				}
				data, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				for _, field := range []int{0, 8, 32, 40, 48, 56, 64} {
					copyData := bytes.Clone(data)
					binary.LittleEndian.PutUint64(copyData[offset+uint64(field):], ^uint64(0))
					bad := filepath.Join(t.TempDir(), "bad-go-table")
					if err := os.WriteFile(bad, copyData, 0700); err != nil {
						t.Fatal(err)
					}
					if _, err := traceTLSReadFile(bad, true); err == nil {
						t.Errorf("accepted malformed table field %d", field)
					}
				}
			})
		}
	}
}

func TestTraceTLSGoMissingMethodsAreClassified(t *testing.T) {
	if !traceTLSGoVersions[traceTLSGoVersion(t, traceTLSGoCommand(t))] {
		t.Skip("fixture compiler version is not supported")
	}
	fixture := traceTLSGoFixture(t, traceTLSGoCommand(t), "normal")
	original, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	file, err := elf.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	section := file.Section(".gopclntab")
	if section == nil {
		section = file.Section(".data.rel.ro.gopclntab")
	}
	if section == nil {
		file.Close()
		t.Fatal("missing Go function table")
	}
	tableOffset := section.Offset
	file.Close()
	mutate := func(name string, edits map[string]string) string {
		t.Helper()
		data := bytes.Clone(original)
		start, end := int(tableOffset), int(tableOffset+section.Size)
		for from, to := range edits {
			count := bytes.Count(data[start:end], []byte(from))
			if len(from) != len(to) || count == 0 {
				t.Fatalf("fixture name %q is absent or changes length", from)
			}
			copy(data[start:end], bytes.ReplaceAll(data[start:end], []byte(from), []byte(to)))
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unknown := func(name string) string { return strings.Repeat("x", len(name)) }
	names := []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose}
	allMissing := map[string]string{}
	for _, name := range names {
		allMissing[name] = unknown(name)
	}
	path := mutate("no-tls", allMissing)
	target, err := traceTLSReadFile(path, true)
	if err != nil || len(target.goReturns) != 0 || len(target.symbols) != 0 {
		t.Fatalf("no TLS target=%+v error=%v", target, err)
	}
	finder := &traceTLSFinder{}
	finder.addGoExecutable(path)
	if len(finder.targets) != 0 || len(finder.notices) != 0 {
		t.Fatalf("no TLS finder targets=%d notices=%q", len(finder.targets), finder.notices)
	}
	if _, _, code, err := resolveTraceTLSTargets(traceTLSMode(path)); err == nil || code != 2 {
		t.Fatalf("explicit no TLS code=%d error=%v", code, err)
	}
	for mask := 1; mask < (1<<len(names))-1; mask++ {
		edits := map[string]string{}
		for index, name := range names {
			if mask&(1<<index) != 0 {
				edits[name] = unknown(name)
			}
		}
		path := mutate(fmt.Sprintf("partial-%d", mask), edits)
		file, err := elf.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		target := traceTLSTarget{path: path}
		err = traceTLSGo(file, &target)
		file.Close()
		if err == nil || !strings.Contains(err.Error(), "missing Go TLS function") {
			t.Fatalf("partial mask=%03b error=%v", mask, err)
		}
		finder := &traceTLSFinder{}
		finder.addGoExecutable(path)
		if len(finder.notices) != 1 || len(finder.targets) != 0 {
			t.Fatalf("partial finder targets=%d notices=%q", len(finder.targets), finder.notices)
		}
	}
	duplicate := mutate("duplicate", map[string]string{traceTLSGoWrite: traceTLSGoClose})
	file, err = elf.Open(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	var duplicateTarget traceTLSTarget
	err = traceTLSGo(file, &duplicateTarget)
	file.Close()
	if err == nil || !strings.Contains(err.Error(), "duplicate Go TLS function") {
		t.Fatalf("duplicate function error=%v", err)
	}
	corrupt := bytes.Clone(original)
	binary.LittleEndian.PutUint32(corrupt[tableOffset:], 0)
	corruptPath := filepath.Join(t.TempDir(), "corrupt-table")
	if err := os.WriteFile(corruptPath, corrupt, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := traceTLSReadFile(corruptPath, true); err == nil {
		t.Fatal("accepted corrupt function table")
	}
}

func TestTraceTLSGoCaptures(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	if !traceTLSGoVersions[traceTLSGoVersion(t, goCommand)] || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("live Go TLS fixture needs a supported Linux amd64 or arm64 Go version")
	}
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("Go TLS capture needs BPF and perf capabilities")
	}
	for _, tc := range []struct {
		mode, argument string
		port           uint16
	}{
		{"normal", "", 0}, {"stripped", "", 0}, {"pie", "", 0}, {"normal", "stack", 0}, {"normal", "stack", 443}, {"normal", "errors", 0},
	} {
		t.Run(tc.mode+"-"+tc.argument+"-"+fmt.Sprint(tc.port), func(t *testing.T) {
			fixture := traceTLSGoFixture(t, goCommand, tc.mode)
			finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(fixture))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, fixture, tc.argument)
			// edc는 page fault 없이 user memory를 읽는다. THP가 always인 host에서는 Go heap의 huge page를 쪼개거나 합치는 순간 그 읽기가
			// 실패해 잃은 event가 생기므로, 시험 program의 heap에는 THP를 쓰지 않는다.
			command.Env = append(os.Environ(), "GODEBUG=disablethp=1")
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready ") {
				t.Fatalf("Go TLS ready: %q %v", line, err)
			}
			requests, responses := map[string]int{}, map[string]int{}
			var observed []captureEvent
			startTLSFixtureOnAttach(t, stdin)
			stop, fixtureOutput := stopWhenTLSFixtureExits(reader)
			summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(fixture), tlsFinder: finder, payload: true, port: tc.port}, 6*time.Second, func(event captureEvent) error {
				if event.PID != uint32(command.Process.Pid) || !event.TLS {
					return nil
				}
				observed = append(observed, event)
				if event.Source != "" || event.Destination != "" {
					t.Errorf("Go TLS endpoint unexpectedly mapped: %#v", event)
				}
				if event.Event == "http_request" {
					requests[event.Path]++
				} else if event.Status == 200 {
					responses[event.Path]++
					if event.LatencyMS == nil || *event.LatencyMS <= 0 {
						t.Errorf("missing Go TLS latency: %#v", event)
					}
					if !strings.Contains(string(event.Payload), "response") {
						t.Errorf("missing Go TLS body: %#v", event)
					}
				}
				return nil
			}, stop)
			if err != nil {
				t.Fatal(err)
			}
			output := <-fixtureOutput
			if err := command.Wait(); err != nil || output.err != nil || (!strings.Contains(output.text, "completed 64") && !strings.Contains(output.text, "done")) {
				t.Fatalf("Go TLS client: %v %v %s %s", err, output.err, output.text, stderr.String())
			}
			want := 64
			if tc.argument == "stack" {
				want = 16
			}
			if tc.argument == "errors" {
				want = 0
			}
			if tc.port != 0 {
				if len(requests) != 0 || len(responses) != 0 || summary.TLSUnmapped == 0 {
					t.Fatalf("Go TLS port exclusion: requests=%v responses=%v summary=%#v", requests, responses, summary)
				}
				return
			}
			if len(requests) != want || len(responses) != want || summary.LostEvents != 0 {
				for _, item := range observed {
					t.Logf("%s %#x %d path=%s body=%q", item.Event, item.SocketID, item.TimestampNS, item.Path, item.Payload)
				}
				t.Fatalf("Go TLS requests=%v responses=%v lost=%d", requests, responses, summary.LostEvents)
			}
			for path, count := range requests {
				if count != 2 || responses[path] != 2 {
					t.Errorf("Go TLS client/server copies: %s requests=%d responses=%d", path, count, responses[path])
				}
			}
		})
	}
}

func TestTraceTLSGoHTTP2CapturesStreamsAndBodies(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	if !traceTLSGoVersions[traceTLSGoVersion(t, goCommand)] || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("live Go HTTP/2 fixture needs a supported Linux amd64 or arm64 Go version")
	}
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("Go HTTP/2 capture needs BPF and perf capabilities")
	}
	for _, mode := range []string{"normal", "stripped", "pie"} {
		t.Run(mode, func(t *testing.T) {
			fixture := traceTLSGoHTTP2Fixture(t, goCommand, mode)
			finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(fixture))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, fixture)
			// TestTraceTLSGoCaptures와 같은 이유로 시험 program의 heap에는 THP를 쓰지 않는다.
			command.Env = append(os.Environ(), "GODEBUG=disablethp=1")
			var stderr bytes.Buffer
			command.Stderr = &stderr
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready ") {
				t.Fatalf("HTTP/2 ready: %q %v", line, err)
			}
			requests, responses := map[string]int{}, map[string]int{}
			sockets := map[uint64]bool{}
			startTLSFixtureOnAttach(t, stdin)
			stop, fixtureOutput := stopWhenTLSFixtureExits(reader)
			summary, err := collectTraceEventsLive(traceScope{protocol: "http", tls: traceTLSMode(fixture), tlsFinder: finder, payload: true, payloadAll: true, http2Payload: true}, 8*time.Second, func(event captureEvent) error {
				if event.PID != uint32(command.Process.Pid) || !event.TLS {
					return nil
				}
				if event.Method != "POST" {
					t.Errorf("HTTP/2 method: %#v", event)
				}
				sockets[event.SocketID] = true
				prefix, repeated := "h2-request:", "q"
				if event.Event == "http_request" {
					requests[event.Path]++
				} else if event.Status == 200 {
					responses[event.Path]++
					prefix, repeated = "h2-response:", "r"
				} else {
					t.Errorf("unexpected HTTP/2 event: %#v", event)
					return nil
				}
				want := prefix + event.Path + ":" + strings.Repeat(repeated, 65536)
				if event.Path == "" || !strings.HasSuffix(string(event.Payload), want) || event.PayloadTruncated {
					t.Errorf("HTTP/2 payload path=%q length=%d truncated=%t", event.Path, len(event.Payload), event.PayloadTruncated)
				}
				return nil
			}, stop)
			if err != nil {
				t.Fatal(err)
			}
			output := <-fixtureOutput
			if err := command.Wait(); err != nil || output.err != nil || !strings.Contains(output.text, "done") {
				t.Fatalf("HTTP/2 client: %v %v %s %s", err, output.err, output.text, stderr.String())
			}
			if len(requests) != 16 || len(responses) != 16 || len(sockets) != 2 || summary.LostEvents != 0 {
				t.Fatalf("HTTP/2 requests=%v responses=%v sockets=%v lost=%d", requests, responses, sockets, summary.LostEvents)
			}
			for path, count := range requests {
				if count != 2 || responses[path] != 2 {
					t.Errorf("HTTP/2 copies: %s requests=%d responses=%d", path, count, responses[path])
				}
			}
		})
	}
}

func traceTLSGoHTTP2Fixture(t *testing.T, goCommand, mode string) string {
	t.Helper()
	version := traceTLSGoVersion(t, goCommand)
	if version == "go1.21.5" || version == "go1.23.7" || version == "go1.23.8" || version == "go1.23.12" {
		source := filepath.Join(t.TempDir(), "go_tls_http2_legacy.go")
		if err := os.WriteFile(source, []byte(traceTLSGoLegacyHTTP2Source), 0600); err != nil {
			t.Fatal(err)
		}
		return traceTLSGoBuildFixture(t, goCommand, source, mode, runtime.GOARCH, true)
	}
	return traceTLSGoBuildFixture(t, goCommand, "testdata/go_tls_http2_client.go", mode, runtime.GOARCH, false)
}

const traceTLSGoLegacyHTTP2Source = `package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

func waitForStart() {
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil { os.Exit(3) }
}

func waitForTLSWrites() {
	buffer := make([]byte, 1<<20)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if !strings.Contains(string(buffer[:runtime.Stack(buffer, true)]), "crypto/tls.(*Conn).Write(") { return }
	}
	panic("a TLS Write did not return")
}

func body(prefix, path, fill string) string { return prefix + path + ":" + strings.Repeat(fill, 65536) }

func main() {
	var serverConnections sync.WaitGroup
	var serverConnectionCount int
	var serverConnectionMu sync.Mutex
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		request := body("h2-request:", path, "q")
		if r.ProtoMajor != 2 || r.Method != "POST" || r.Header.Get("X-Repeated") != "shared-hpack-value" || len(r.Header.Get("X-Large")) != 54000 { panic("HTTP/2 request metadata mismatch") }
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != request { panic("HTTP/2 request body mismatch") }
		w.Header().Set("X-Repeated", "shared-hpack-value")
		response := []byte(body("h2-response:", path, "r"))
		for offset := 0; offset < len(response); offset += 4096 {
			end := offset + 4096
			if end > len(response) { end = len(response) }
			if _, err := w.Write(response[offset:end]); err != nil { panic(err) }
			w.(http.Flusher).Flush()
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew { serverConnections.Add(1); serverConnectionMu.Lock(); serverConnectionCount++; serverConnectionMu.Unlock() }
		if state == http.StateClosed { serverConnections.Done() }
	}
	server.StartTLS()
	defer server.Close()
	fmt.Println("ready", os.Getpid())
	waitForStart()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connections := map[net.Conn]bool{}
	var connectionsMu sync.Mutex
	var wait sync.WaitGroup
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			path := fmt.Sprintf("/h2-stream-%d", i)
			trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { connectionsMu.Lock(); connections[info.Conn] = true; connectionsMu.Unlock() }}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "POST", server.URL+path, strings.NewReader(body("h2-request:", path, "q")))
			if err != nil { errors <- err; return }
			req.Header.Set("X-Repeated", "shared-hpack-value")
			req.Header.Set("X-Large", strings.Repeat("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ", 1500))
			resp, err := client.Do(req)
			if err != nil { errors <- err; return }
			got, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || resp.ProtoMajor != 2 || resp.Header.Get("X-Repeated") != "shared-hpack-value" || string(got) != body("h2-response:", path, "r") { errors <- fmt.Errorf("HTTP/2 response mismatch %s", path) }
		}(i)
	}
	wait.Wait()
	close(errors)
	for err := range errors { panic(err) }
	connectionsMu.Lock()
	clientCount := len(connections)
	connectionsMu.Unlock()
	if clientCount != 1 { panic(fmt.Sprintf("HTTP/2 client connections: %d", clientCount)) }
	transport.CloseIdleConnections()
	server.Close()
	serverConnections.Wait()
	serverConnectionMu.Lock()
	serverCount := serverConnectionCount
	serverConnectionMu.Unlock()
	if serverCount != 1 { panic(fmt.Sprintf("HTTP/2 server connections: %d", serverCount)) }
	waitForTLSWrites()
	fmt.Println("done")
}
`
