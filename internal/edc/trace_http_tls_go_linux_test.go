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
	"io"
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
	fixture := filepath.Join(t.TempDir(), "edc-go-tls")
	args := []string{"build", "-o", fixture}
	if mode == "stripped" {
		args = append(args, "-ldflags=-s -w")
	} else if mode == "pie" {
		args = append(args, "-buildmode=pie")
	}
	args = append(args, "testdata/go_tls_client.go")
	if output, err := exec.Command(goCommand, args...).CombinedOutput(); err != nil {
		t.Fatalf("Go TLS fixture: %v\n%s", err, output)
	}
	return fixture
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
	output, err := exec.Command(goCommand, "env", "GOVERSION").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func TestTraceTLSGoFixtureMetadata(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	for _, mode := range []string{"normal", "stripped", "pie"} {
		t.Run(mode, func(t *testing.T) {
			fixture := traceTLSGoFixture(t, goCommand, mode)
			info, err := buildinfo.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			target, err := traceTLSReadFile(fixture, true)
			if !traceTLSGoVersions[info.GoVersion] || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
				if err == nil || !strings.Contains(err.Error(), "unsupported Go TLS ABI") {
					t.Fatalf("unsupported ABI accepted: %s %v", info.GoVersion, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
				if _, ok := target.offsets[name]; !ok {
					t.Errorf("missing entry: %s", name)
				}
				if name != traceTLSGoClose && len(target.goReturns[name]) == 0 {
					t.Errorf("missing returns: %s", name)
				}
			}
			if mode != "normal" {
				return
			}
			file, err := elf.Open(fixture)
			if err != nil {
				t.Fatal(err)
			}
			section := file.Section(".gopclntab")
			offset := section.Offset
			file.Close()
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

func TestTraceTLSGoArm64FixtureMetadata(t *testing.T) {
	goCommand := traceTLSGoCommand(t)
	if !traceTLSGoVersions[traceTLSGoVersion(t, goCommand)] {
		t.Skip("arm64 fixture needs a supported Go version")
	}
	fixture := filepath.Join(t.TempDir(), "edc-go-tls-arm64")
	command := exec.Command(goCommand, "build", "-o", fixture, "testdata/go_tls_client.go")
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Go TLS arm64 fixture: %v\n%s", err, output)
	}
	file, err := elf.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	target := traceTLSTarget{path: fixture}
	if err := traceTLSGo(file, &target); err != nil {
		t.Fatal(err)
	}
	if target.goMachine != elf.EM_AARCH64 {
		t.Fatalf("machine=%s", target.goMachine)
	}
	for _, name := range []string{traceTLSGoRead, traceTLSGoWrite, traceTLSGoClose} {
		if target.offsets[name] == 0 {
			t.Errorf("missing entry: %s", name)
		}
		if name != traceTLSGoClose && len(target.goReturns[name]) == 0 {
			t.Errorf("missing returns: %s", name)
		}
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
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready ") {
				t.Fatalf("Go TLS ready: %q %v", line, err)
			}
			requests, responses := map[string]int{}, map[string]int{}
			var observed []captureEvent
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
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			output, readErr := io.ReadAll(reader)
			if err := command.Wait(); err != nil || readErr != nil || (!strings.Contains(string(output), "completed 64") && !strings.Contains(string(output), "done")) {
				t.Fatalf("Go TLS client: %v %v %s %s", err, readErr, output, stderr.String())
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
			fixture := filepath.Join(t.TempDir(), "edc-go-h2")
			args := []string{"build", "-o", fixture}
			if mode == "stripped" {
				args = append(args, "-ldflags=-s -w")
			}
			if mode == "pie" {
				args = append(args, "-buildmode=pie")
			}
			args = append(args, "testdata/go_tls_http2_client.go")
			if output, err := exec.Command(goCommand, args...).CombinedOutput(); err != nil {
				t.Fatalf("HTTP/2 fixture: %v\n%s", err, output)
			}
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
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(stdout)
			if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "ready ") {
				t.Fatalf("HTTP/2 ready: %q %v", line, err)
			}
			requests, responses := map[string]int{}, map[string]int{}
			sockets := map[uint64]bool{}
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
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			output, readErr := io.ReadAll(reader)
			if err := command.Wait(); err != nil || readErr != nil || !strings.Contains(string(output), "done") {
				t.Fatalf("HTTP/2 client: %v %v %s %s", err, readErr, output, stderr.String())
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
