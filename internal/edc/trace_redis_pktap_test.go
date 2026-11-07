package edc

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func redisPKTAPCommand(parts ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(parts))
	for _, part := range parts {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(part), part)
	}
	return []byte(b.String())
}
func redisPKTAPTestPacket(server, request bool, sequence uint32, at uint64, data []byte) pktapTCPPacket {
	p := pktapTestTCP(request != server, sequence, string(data))
	p.bootTimeNS = at
	p.source, p.destination = "127.0.0.1:50000", "127.0.0.1:6379"
	if server {
		p.source, p.destination = p.destination, p.source
	}
	return p
}
func TestPKTAPRedisPairsPipelineAndMasksValues(t *testing.T) {
	for _, server := range []bool{false, true} {
		tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
		now := time.Now()
		request := append(redisPKTAPCommand("SET", "cache", "value-secret"), redisPKTAPCommand("GET", "cache")...)
		events := tracker.add(redisPKTAPTestPacket(server, true, 100, 1_000_000, request), now)
		if len(events) != 2 || events[0].Redis == nil || events[0].Redis.Command != "SET" || events[1].Redis.Command != "GET" {
			t.Fatalf("commands=%#v", events)
		}
		response := []byte("+OK\r\n$12\r\nvalue-secret\r\n")
		events = tracker.add(redisPKTAPTestPacket(server, false, 900, 31_000_000, response), now)
		if len(events) != 2 || events[0].Redis.Command != "SET" || events[1].Redis.Command != "GET" || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 {
			t.Fatalf("responses=%#v", events)
		}
		for _, event := range events {
			if event.Redis.Value != "" || len(event.Redis.ArgPreview) != 0 {
				t.Fatalf("Redis value leaked: %#v", event)
			}
		}
		tracker.sweep(now, true)
		if tracker.buffered != 0 || len(tracker.flows) != 0 {
			t.Fatal("Redis TCP state retained")
		}
	}
}
func TestPKTAPRedisPlatformRegistry(t *testing.T) {
	if traceProtocols["redis"].linuxOnly || !traceProtocols["redis"].serverSide {
		t.Fatal("Redis must allow macOS and server-side capture")
	}
}

func TestPKTAPRedisReorderedAndPIDLessFragments(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
	now := time.Now()
	data := redisPKTAPCommand("GET", "fragmented")
	syn := redisPKTAPTestPacket(false, true, 99, 1_000_000, nil)
	syn.flags = 2
	tracker.add(syn, now)
	tracker.add(redisPKTAPTestPacket(false, true, 100, 1_000_000, data[:5]), now)
	tail := redisPKTAPTestPacket(false, true, 120, 1_000_002, data[20:])
	tail.pid = 0
	tail.process = ""
	tracker.add(tail, now)
	events := tracker.add(redisPKTAPTestPacket(false, true, 105, 1_000_001, data[5:20]), now)
	if len(events) != 1 || events[0].Redis.Command != "GET" || events[0].Redis.Key != "fragmented" || events[0].PID != 42 {
		t.Fatalf("fragmented command=%#v", events)
	}
	tracker.add(redisPKTAPTestPacket(false, true, 100, 1_000_000, data), now)
	var replies []captureEvent
	response := []byte("$5\r\nhello\r\n")
	for i := range response {
		replies = append(replies, tracker.add(redisPKTAPTestPacket(false, false, 900+uint32(i), 31_000_000, response[i:i+1]), now)...)
	}
	if len(replies) != 1 || replies[0].Redis.Command != "GET" || replies[0].LatencyMS == nil || *replies[0].LatencyMS != 30 {
		t.Fatalf("reply=%#v", replies)
	}
	tracker.sweep(now, true)
	if len(tracker.redis.conns) != 0 || tracker.redis.pending != 0 || tracker.lost != 0 || tracker.buffered != 0 {
		t.Fatalf("Redis cleanup: pending=%d lost=%d bytes=%d", tracker.redis.pending, tracker.lost, tracker.buffered)
	}
}
func TestPKTAPRedisPortSideAndSensitiveCommands(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis", port: 6380, side: traceServerSide, showSecrets: true}, 0)
	now := time.Now()
	auth := redisPKTAPCommand("AUTH", "fixture-password")
	packet := redisPKTAPTestPacket(true, true, 100, 1_000_000, auth)
	if events := tracker.add(packet, now); len(events) != 0 {
		t.Fatal(events)
	}
	packet.source = "127.0.0.1:6380"
	events := tracker.add(packet, now)
	if len(events) != 1 || events[0].Redis.Command != "AUTH" || events[0].Side != traceServerSide || len(events[0].Redis.ArgPreview) != 0 {
		t.Fatalf("AUTH=%#v", events)
	}
	reply := redisPKTAPTestPacket(true, false, 900, 31_000_000, []byte("-ERR fixture-password rejected\r\n"))
	reply.source = packet.source
	events = tracker.add(reply, now)
	if len(events) != 1 || events[0].Event != redisEventError || events[0].Redis.Message != "" {
		t.Fatalf("sensitive error=%#v", events)
	}
	client := redisPKTAPTestPacket(false, true, 100, 1, auth)
	client.destination = packet.source
	if events := tracker.add(client, now); len(events) != 0 {
		t.Fatalf("client side leaked: %#v", events)
	}
}
func TestPKTAPRedisRESP3PushDoesNotConsumeReply(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
	now := time.Now()
	tracker.add(redisPKTAPTestPacket(false, true, 100, 1_000_000, redisPKTAPCommand("PING")), now)
	events := tracker.add(redisPKTAPTestPacket(false, false, 900, 31_000_000, []byte(">2\r\n+invalidate\r\n$3\r\nkey\r\n+PONG\r\n")), now)
	if len(events) != 2 || events[0].Event != redisEventPush || events[1].Event != redisEventReply || events[1].Redis.Command != "PING" {
		t.Fatalf("push/reply=%#v", events)
	}
}
