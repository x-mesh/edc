//go:build darwin

package edc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type darwinPKTAPReplayPacket struct {
	Timestamp                    uint64
	Now                          int64
	PID                          uint32
	Process, Source, Destination string
	Sent                         bool
	Sequence                     uint32
	Flags                        byte
	Payload                      []byte
	TLS, Closed                  bool
	Object, Size                 uint64
	FD                           int32
	Port                         uint16
}

type darwinPKTAPReplay struct {
	Packets   []darwinPKTAPReplayPacket
	Expected  []darwinPKTAPReplayHTTPEvent
	Truncated bool
	bytes     int
}

type darwinPKTAPReplayHTTPEvent struct {
	PID                                     uint32
	Source, Destination, Method, Path, Side string
	Status                                  int
	TLS, HasLatency                         bool
	Latency                                 float64
}

func darwinReplayHTTPEvent(event captureEvent) (darwinPKTAPReplayHTTPEvent, bool) {
	if event.Protocol != "http" || (event.Method == "" && event.Status == 0) {
		return darwinPKTAPReplayHTTPEvent{}, false
	}
	value := darwinPKTAPReplayHTTPEvent{PID: event.PID, Source: event.Source, Destination: event.Destination, Method: event.Method, Path: event.Path, Side: event.Side, Status: event.Status, TLS: event.TLS, HasLatency: event.LatencyMS != nil}
	if event.LatencyMS != nil {
		value.Latency = *event.LatencyMS
	}
	return value, true
}
func validateDarwinReplayHTTPEvents(expected, actual []darwinPKTAPReplayHTTPEvent) error {
	if len(expected) != len(actual) {
		return fmt.Errorf("HTTP replay events=%d expected=%d", len(actual), len(expected))
	}
	for i := range expected {
		if expected[i] != actual[i] {
			return fmt.Errorf("HTTP replay event %d differs: actual=%#v expected=%#v", i, actual[i], expected[i])
		}
	}
	return nil
}

func (r *darwinPKTAPReplay) add(packet pktapTCPPacket, now time.Time) {
	size := r.bytes + len(packet.payload)
	if len(r.Packets) >= 8192 || size > 8<<20 {
		r.Truncated = true
		return
	}
	r.bytes = size
	r.Packets = append(r.Packets, darwinPKTAPReplayPacket{Timestamp: packet.bootTimeNS, Now: now.UnixNano(), PID: packet.pid, Process: packet.process, Source: packet.source, Destination: packet.destination, Sent: packet.sent, Sequence: packet.sequence, Flags: packet.flags, Payload: bytes.Clone(packet.payload)})
}
func (r *darwinPKTAPReplay) addTLS(record darwinTLSRecord, port uint16, now time.Time) {
	size := r.bytes + len(record.payload)
	if len(r.Packets) >= 8192 || size > 8<<20 {
		r.Truncated = true
		return
	}
	r.bytes = size
	r.Packets = append(r.Packets, darwinPKTAPReplayPacket{Timestamp: record.wallNS, Now: now.UnixNano(), PID: record.pid, Process: record.process, Source: record.source, Destination: record.destination, Sent: record.sent, Sequence: record.offset, Payload: bytes.Clone(record.payload), TLS: true, Closed: record.closed, Object: record.object, Size: record.size, FD: record.fd, Port: port})
}
func (r *darwinPKTAPReplay) save(t *testing.T) {
	t.Helper()
	directory, err := os.MkdirTemp("", "edc-go-pktap-replay-")
	if err != nil {
		t.Logf("replay directory: %v", err)
		return
	}
	if err := os.Chmod(directory, 0755); err != nil {
		t.Logf("replay directory permissions: %v", err)
		return
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Logf("replay encode: %v", err)
		return
	}
	path := filepath.Join(directory, "packets.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Logf("replay save: %v", err)
		return
	}
	t.Logf("PKTAP replay=%s packets=%d truncated=%t", path, len(r.Packets), r.Truncated)
}
func TestDarwinPKTAPSavedReplay(t *testing.T) {
	path := os.Getenv("EDC_PKTAP_REPLAY")
	if path == "" {
		t.Skip("set EDC_PKTAP_REPLAY to a saved fixture packet trace")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var replay darwinPKTAPReplay
	if err := json.NewDecoder(file).Decode(&replay); err != nil {
		t.Fatal(err)
	}
	if replay.Truncated {
		t.Fatal("packet replay is truncated")
	}
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "http", side: "client", payload: true, payloadAll: true}, 0)
	losses := map[string]int{}
	tracker.lossHook = func(reason string, packet httpPacket, detail string) {
		losses[reason]++
		t.Logf("%s pid=%d local=%s remote=%s %s", reason, packet.pid, packet.source, packet.destination, detail)
	}
	var actual []darwinPKTAPReplayHTTPEvent
	observe := func(events []captureEvent) {
		for _, event := range events {
			if value, ok := darwinReplayHTTPEvent(event); ok {
				if value.Status != 0 && (value.Method == "" || value.Path == "" || !value.HasLatency) {
					t.Fatalf("orphan HTTP replay response: %#v", value)
				}
				actual = append(actual, value)
			}
		}
	}
	var now time.Time
	for _, p := range replay.Packets {
		now = time.Unix(0, p.Now)
		if p.TLS {
			events, _ := tracker.addDarwinTLS(darwinTLSRecord{pid: p.PID, object: p.Object, wallNS: p.Timestamp, source: p.Source, destination: p.Destination, process: p.Process, sent: p.Sent, closed: p.Closed, payload: p.Payload, fd: p.FD, offset: p.Sequence, size: p.Size}, p.Port, now)
			observe(events)
			continue
		}
		observe(tracker.add(pktapTCPPacket{httpPacket: httpPacket{bootTimeNS: p.Timestamp, pid: p.PID, process: p.Process, source: p.Source, destination: p.Destination, sent: p.Sent, payload: p.Payload}, sequence: p.Sequence, flags: p.Flags}, now))
	}
	observe(tracker.sweep(now, true))
	if len(replay.Expected) > 0 {
		if err := validateDarwinReplayHTTPEvents(replay.Expected, actual); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log("legacy replay has no captured HTTP event oracle; only framing and orphan-response checks apply")
	}
	if tracker.lost != 0 {
		t.Fatalf("replayed decoder losses=%d reasons=%v", tracker.lost, losses)
	}
}

func TestDarwinPKTAPReplayRecorderOwnsPacketBytes(t *testing.T) {
	var replay darwinPKTAPReplay
	packet := pktapTestTCP(true, 1, "fixture")
	replay.add(packet, time.Now())
	packet.payload[0] = 'x'
	if string(replay.Packets[0].Payload) != "fixture" || replay.bytes != 7 {
		t.Fatalf("packet=%q bytes=%d", replay.Packets[0].Payload, replay.bytes)
	}
}

func TestDarwinPKTAPReplayValidatesHTTPEventParity(t *testing.T) {
	expected := []darwinPKTAPReplayHTTPEvent{{PID: 42, Method: "GET", Path: "/request", TLS: true}, {PID: 42, Method: "GET", Path: "/request", Status: 200, TLS: true, HasLatency: true, Latency: 30}}
	if err := validateDarwinReplayHTTPEvents(expected, expected); err != nil {
		t.Fatal(err)
	}
	if err := validateDarwinReplayHTTPEvents(expected, nil); err == nil {
		t.Fatal("missing events accepted")
	}
	actual := append([]darwinPKTAPReplayHTTPEvent{}, expected...)
	actual[1].Path = "/wrong"
	if err := validateDarwinReplayHTTPEvents(expected, actual); err == nil {
		t.Fatal("response matched to another request accepted")
	}
	actual[1] = expected[1]
	actual[1].Latency = 0
	if err := validateDarwinReplayHTTPEvents(expected, actual); err == nil {
		t.Fatal("changed latency accepted")
	}
}

func TestDarwinReplayHTTPEventPreservesIdentityAndLatency(t *testing.T) {
	latency := 12.5
	event := captureEvent{Protocol: "http", PID: 42, Source: "127.0.0.1:50000", Destination: "127.0.0.1:443", Method: "GET", Path: "/fixture", Side: traceClientSide, Status: 200, TLS: true, LatencyMS: &latency}
	actual, ok := darwinReplayHTTPEvent(event)
	expected := darwinPKTAPReplayHTTPEvent{PID: 42, Source: event.Source, Destination: event.Destination, Method: "GET", Path: "/fixture", Side: traceClientSide, Status: 200, TLS: true, HasLatency: true, Latency: latency}
	if !ok || actual != expected {
		t.Fatalf("actual=%+v expected=%+v ok=%t", actual, expected, ok)
	}
	event.LatencyMS = nil
	actual, ok = darwinReplayHTTPEvent(event)
	if !ok || actual.HasLatency || actual.Latency != 0 {
		t.Fatalf("absent latency=%+v", actual)
	}
	for _, event := range []captureEvent{{Protocol: "redis", Method: "GET"}, {Protocol: "http"}} {
		if _, ok := darwinReplayHTTPEvent(event); ok {
			t.Fatalf("non-message HTTP event accepted: %+v", event)
		}
	}
}
