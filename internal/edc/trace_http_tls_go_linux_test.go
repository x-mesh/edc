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

func traceTLSGoFixture(t *testing.T, mode string) string {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "edc-go-tls")
	args := []string{"build", "-o", fixture}
	if mode == "stripped" {
		args = append(args, "-ldflags=-s -w")
	} else if mode == "pie" {
		args = append(args, "-buildmode=pie")
	}
	args = append(args, "testdata/go_tls_client.go")
	if output, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("Go TLS fixture: %v\n%s", err, output)
	}
	return fixture
}

func TestTraceTLSGoFixtureMetadata(t *testing.T) {
	for _, mode := range []string{"normal", "stripped", "pie"} {
		t.Run(mode, func(t *testing.T) {
			fixture := traceTLSGoFixture(t, mode)
			info, err := buildinfo.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			target, err := traceTLSReadFile(fixture, true)
			if info.GoVersion != traceTLSGoVersion || runtime.GOARCH != "amd64" {
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

func TestTraceTLSGoCaptures(t *testing.T) {
	version, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil || strings.TrimSpace(string(version)) != traceTLSGoVersion || runtime.GOARCH != "amd64" {
		t.Skip("live Go TLS fixture needs Go 1.27.1 amd64")
	}
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("Go TLS capture needs BPF and perf capabilities")
	}
	for _, tc := range []struct {
		mode, argument string
		port           uint16
	}{
		{"normal", "", 0}, {"stripped", "", 0}, {"pie", "", 0}, {"normal", "stack", 0}, {"normal", "stack", 443},
	} {
		t.Run(tc.mode+"-"+tc.argument+"-"+fmt.Sprint(tc.port), func(t *testing.T) {
			fixture := traceTLSGoFixture(t, tc.mode)
			finder, _, _, err := resolveTraceTLSTargets(traceTLSMode(fixture))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, fixture, tc.argument)
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
