//go:build linux

package edc

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCaptureEventAddressFormatting(t *testing.T) {
	var address [16]byte
	address[0], address[1], address[2], address[3] = 192, 0, 2, 10
	if got := formatCaptureAddress(2, address, 443); got != "192.0.2.10:443" {
		t.Fatalf("IPv4 address = %q", got)
	}
	address[15] = 1
	if got := formatCaptureAddress(10, address, 443); got != "[c000:20a:0:0:0:0:0:1]:443" {
		t.Fatalf("IPv6 address = %q", got)
	}
}

func TestTraceTargetFromArguments(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"curl", "https://naver.com/path"}, "naver.com"},
		{[]string{"curl", "naver.com"}, "naver.com"},
		{[]string{"curl", "http://127.0.0.1:8080/health"}, "127.0.0.1"},
		{[]string{"curl", "--fail", "https://example.com"}, "example.com"},
	}
	for _, test := range cases {
		if got := traceTargetFromArguments(test.args); got != test.want {
			t.Fatalf("traceTargetFromArguments(%q) = %q, want %q", test.args, got, test.want)
		}
	}
}

func TestCaptureEventsRunWritesEventsAfterSIGINT(t *testing.T) {
	previous := captureEventsCollect
	defer func() { captureEventsCollect = previous }()
	captureEventsCollect = func(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
		if stop == nil {
			t.Fatal("capture events run without a stop channel")
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stop:
		case <-time.After(5 * time.Second):
			t.Fatal("SIGINT did not close the stop channel")
		}
		events := []captureEvent{{Event: "connect"}, {Event: "close"}}
		return events, captureSummary{Event: "capture_summary", EventCount: uint64(len(events))}, nil
	}

	output := filepath.Join(t.TempDir(), "events.jsonl")
	if err := captureEventsRun(10*time.Minute, output); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var line struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		names = append(names, line.Event)
	}
	if got := len(names); got != 3 || names[0] != "connect" || names[1] != "close" || names[2] != "capture_summary" {
		t.Fatalf("output events = %q", names)
	}
}

func TestCaptureEventUsesWallClockTimestamp(t *testing.T) {
	raw := captureEventRaw{TimestampNS: 5 * uint64(time.Second), EventType: 1}
	offset := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC).UnixNano()
	event := raw.event(offset)
	if event.BootTimeNS != 5*uint64(time.Second) {
		t.Fatalf("boot_time_ns = %d", event.BootTimeNS)
	}
	if want := uint64(offset) + 5*uint64(time.Second); event.TimestampNS != want {
		t.Fatalf("timestamp_ns = %d, want %d", event.TimestampNS, want)
	}
}

func TestCaptureClockOffsetMatchesWallClock(t *testing.T) {
	offset, err := captureClockOffset()
	if err != nil {
		t.Fatal(err)
	}
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		t.Fatal(err)
	}
	wall := time.Unix(0, monotonic.Nano()+offset)
	if drift := time.Since(wall); drift < -time.Second || drift > time.Second {
		t.Fatalf("monotonic + offset = %s, drift %s", wall, drift)
	}
}

func TestCommandTargetCacheReusesLookupWithinTTL(t *testing.T) {
	lookups := map[uint32]int{}
	cache := newCommandTargetCache(func(pid uint32) string {
		lookups[pid]++
		return "example.com"
	})
	now := time.Now()
	for index := 0; index < 100; index++ {
		if got := cache.target(42, now.Add(time.Duration(index)*time.Millisecond)); got != "example.com" {
			t.Fatalf("target = %q", got)
		}
	}
	if lookups[42] != 1 {
		t.Fatalf("lookups within TTL = %d, want 1", lookups[42])
	}
	cache.target(42, now.Add(commandTargetTTL))
	if lookups[42] != 2 {
		t.Fatalf("lookups after TTL = %d, want 2", lookups[42])
	}
}

func TestCommandTargetCacheSweepsExpiredEntries(t *testing.T) {
	cache := newCommandTargetCache(func(uint32) string { return "" })
	now := time.Now()
	for pid := uint32(0); pid < commandTargetSweepSize; pid++ {
		cache.target(pid, now)
	}
	cache.target(commandTargetSweepSize, now.Add(commandTargetTTL))
	if len(cache.entries) != 1 {
		t.Fatalf("entries after sweep = %d, want 1", len(cache.entries))
	}
}
