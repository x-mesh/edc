//go:build darwin

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
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDarwinTLSStabilityLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("expanded macOS HTTPS collector requires root")
	}
	for _, name := range []string{"reconnect", "concurrent", "concurrent-client", "soak"} {
		want := 128
		duration := 8 * time.Second
		if strings.HasPrefix(name, "concurrent") {
			want = 256
			duration = 12 * time.Second
		}
		if name == "soak" {
			want = 240
			duration = 65 * time.Second
		}
		t.Run(name, func(t *testing.T) {
			binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
			ctx, cancel := context.WithTimeout(context.Background(), duration+20*time.Second)
			defer cancel()
			port := 0
			args := []string{name}
			if name == "concurrent-client" {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "go-tls-response:"+r.URL.Path) }))
				defer server.Close()
				args = []string{"concurrent", server.URL}
			}
			client := exec.CommandContext(ctx, binary, args...)
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
			var done chan error
			collected := false
			defer func() {
				if !collected {
					_ = client.Process.Kill()
					if done == nil {
						_ = client.Wait()
					} else {
						<-done
					}
				}
			}()
			reader := bufio.NewReader(output)
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}

			fields := strings.Fields(line)
			if len(fields) != 3 {
				_ = client.Process.Kill()
				_ = client.Wait()
				t.Fatalf("Go ready=%q", line)
			}
			address := strings.TrimPrefix(fields[2], "https://")
			_, value, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			port, err = strconv.Atoi(value)
			if err != nil {
				t.Fatal(err)
			}

			done = make(chan error, 1)
			var completion bytes.Buffer
			go func() {
				_, readErr := io.Copy(&completion, reader)
				waitErr := client.Wait()
				if readErr != nil {
					waitErr = readErr
				}
				done <- waitErr
			}()

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
			if len(selected) != 1 {
				t.Fatalf("TLS discovery=%#v", selected)
			}
			previous := captureAttachedHook
			t.Cleanup(func() { captureAttachedHook = previous })
			captureAttachedHook = func() {
				if _, err := input.Write([]byte("go\n")); err != nil {
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
			previousTrackerHook := captureTrackerHook
			t.Cleanup(func() { captureTrackerHook = previousTrackerHook })
			previousPacketHook := capturePacketHook
			t.Cleanup(func() { capturePacketHook = previousPacketHook })
			previousTLSHook := captureTLSRecordHook
			t.Cleanup(func() { captureTLSRecordHook = previousTLSHook })
			var replay darwinPKTAPReplay

			captureTLSRecordHook = func(record darwinTLSRecord, port uint16, now time.Time) {
				if record.pid == uint32(client.Process.Pid) {
					replay.addTLS(record, port, now)
				}
			}

			capturePacketHook = func(packet pktapTCPPacket, now time.Time) {
				if packet.pid == 0 || packet.pid == uint32(client.Process.Pid) {
					replay.add(packet, now)
				}
			}

			var capturedTracker *pktapHTTPTracker
			lossCounts := map[string]int{}
			var lossSamples []string
			captureTrackerHook = func(tracker *pktapHTTPTracker) {
				capturedTracker = tracker
				tracker.lossHook = func(reason string, packet httpPacket, detail string) {
					lossCounts[reason]++
					if len(lossSamples) < 8 {
						lossSamples = append(lossSamples, fmt.Sprintf("%s pid=%d sent=%t decrypted=%t local=%s remote=%s %s", reason, packet.pid, packet.sent, packet.decrypted, packet.source, packet.destination, detail))
					}
				}
			}
			requests, responses := 0, 0
			seenRequests := map[string]bool{}
			seenResponses := map[string]bool{}
			summary, err := collectDarwinHTTPEvents(traceScope{protocol: "http", process: "client", tls: traceTLSMode(binary), tlsFinder: &traceTLSFinder{targets: selected}, side: "client", port: uint16(port), payload: true, payloadAll: true}, duration, func(event captureEvent) error {
				if !event.TLS || event.PID != uint32(client.Process.Pid) {
					return nil
				}

				if value, ok := darwinReplayHTTPEvent(event); ok {
					replay.Expected = append(replay.Expected, value)
				}

				if strings.Contains(event.Payload, "Bearer fixture-secret") {
					return fmt.Errorf("authorization header was not masked")
				}
				if event.Method == "GET" && event.Status == 0 {
					if seenRequests[event.Path] {
						return fmt.Errorf("duplicate TLS request: %s", event.Path)
					}
					seenRequests[event.Path] = true
					requests++
				}
				if event.Status == 200 {
					if !seenRequests[event.Path] || seenResponses[event.Path] {
						return fmt.Errorf("unmatched or duplicate TLS response: %s", event.Path)
					}
					if !strings.Contains(event.Payload, "go-tls-response:"+event.Path) {
						return fmt.Errorf("response body belongs to another request: %s", event.Path)
					}
					seenResponses[event.Path] = true
					responses++
					if event.Method != "GET" || event.Path == "" || event.LatencyMS == nil {
						return fmt.Errorf("TLS response lacks matched request: %#v", event)
					}

				}
				return nil
			}, nil)
			if err != nil {
				replay.save(t)
				t.Fatal(err)
			}
			select {
			case err := <-done:
				collected = true
				if err != nil {
					t.Fatalf("fixture=%v\n%s", err, diagnostic.String())
				}
			case <-ctx.Done():
				t.Fatal("fixture timed out")
			}
			if requests != want || responses != want || summary.TLSUnmapped != 0 || summary.LostEvents != 0 {
				if summary.LostEvents != 0 {
					replay.save(t)
				}
				t.Fatalf("requests=%d responses=%d expected=%d pcap=%#v decoder_loss=%d loss_counts=%v loss_samples=%v summary=%#v", requests, responses, want, packetStats, summary.LostEvents-uint64(packetStats.dropped)-uint64(packetStats.interfaceDropped), lossCounts, lossSamples, summary)
			}
			var completed, reused, connections int
			if _, err := fmt.Sscanf(strings.TrimSpace(completion.String()), "completed %d HTTPS requests fd_reuses=%d fd_connections=%d", &completed, &reused, &connections); err != nil || completed != want || name != "soak" && (reused == 0 || connections != want) || name == "soak" && (connections < 1 || connections > 8) {
				t.Fatalf("FD reuse was not exercised: %q", completion.String())
			}
			if capturedTracker == nil || len(capturedTracker.flows) != 0 || capturedTracker.buffered != 0 || len(capturedTracker.tracker.pending) != 0 {
				t.Fatal("TLS capture retained stream state after shutdown")
			}
			t.Logf("%s: requests=%d connections=%d fd_reuses=%d, no loss or unmapped records", name, want, connections, reused)
		})
	}
}

func TestDarwinTLSStabilityFixtures(t *testing.T) {
	binary := buildDarwinTLSExpansionFixture(t, true, runtime.GOARCH)
	for _, mode := range []string{"reconnect", "concurrent", "concurrent-client", "keepalive"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args := []string{mode}
			if mode == "concurrent-client" {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "go-tls-response:"+r.URL.Path) }))
				defer server.Close()
				args = []string{"concurrent", server.URL}
			}
			command := exec.CommandContext(ctx, binary, args...)
			command.Stdin = strings.NewReader("go\n")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", mode, err, output)
			}
			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			var completed, reused, connections int
			want := 128
			if strings.HasPrefix(mode, "concurrent") {
				want = 256
			}
			if mode == "keepalive" {
				want = 32
			}
			if _, err := fmt.Sscanf(lines[len(lines)-1], "completed %d HTTPS requests fd_reuses=%d fd_connections=%d", &completed, &reused, &connections); err != nil || completed != want || mode != "keepalive" && (reused == 0 || connections != want) || mode == "keepalive" && (connections < 1 || connections > 8) {
				t.Fatalf("%s: %s", mode, output)
			}
		})
	}
}
