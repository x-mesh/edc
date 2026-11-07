//go:build darwin

package edc

import (
	"bytes"
	"encoding/json"
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
	Truncated bool
	bytes     int
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
	var now time.Time
	for _, p := range replay.Packets {
		now = time.Unix(0, p.Now)
		if p.TLS {
			tracker.addDarwinTLS(darwinTLSRecord{pid: p.PID, object: p.Object, wallNS: p.Timestamp, source: p.Source, destination: p.Destination, process: p.Process, sent: p.Sent, closed: p.Closed, payload: p.Payload, fd: p.FD, offset: p.Sequence, size: p.Size}, p.Port, now)
			continue
		}
		tracker.add(pktapTCPPacket{httpPacket: httpPacket{bootTimeNS: p.Timestamp, pid: p.PID, process: p.Process, source: p.Source, destination: p.Destination, sent: p.Sent, payload: p.Payload}, sequence: p.Sequence, flags: p.Flags}, now)
	}
	tracker.sweep(now, true)
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
