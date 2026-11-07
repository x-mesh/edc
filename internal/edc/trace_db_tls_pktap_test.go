package edc

import (
	"testing"
	"time"
)

func TestPKTAPDBTLSMarksEventsAndMasksValues(t *testing.T) {
	now := time.Now()
	redis := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
	request := redisPKTAPTestPacket(false, true, 0, 1_000_000, redisPKTAPCommand("SET", "cache", "value-secret"))
	request.tlsObject = 1
	request.decrypted = true
	events := redis.add(request, now)
	if len(events) != 1 || !events[0].TLS || len(events[0].Redis.ArgPreview) != 0 {
		t.Fatalf("Redis TLS command=%+v", events)
	}
	reply := redisPKTAPTestPacket(false, false, 0, 31_000_000, []byte("+OK\r\n"))
	reply.tlsObject = 1
	reply.decrypted = true
	events = redis.add(reply, now)
	if len(events) != 1 || !events[0].TLS || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 {
		t.Fatalf("Redis TLS reply=%+v", events)
	}
	mysql := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
	p := pktapTestTCP(true, 0, string(mysqlFrame(0, mysqlQueryPayload("SELECT 'value-secret'"))))
	p.source, p.destination = "127.0.0.1:50000", "127.0.0.1:3306"
	p.tlsObject = 2
	p.decrypted = true
	events = mysql.add(p, now)
	if len(events) != 1 || !events[0].TLS || events[0].MySQL == nil {
		t.Fatalf("MySQL TLS query=%+v", events)
	}
}

func TestPKTAPRedisIgnoresFragmentedTLSCiphertext(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
	data := []byte{0x16, 3, 3, 0, 4, 1, 2, 3, 4}
	for i := range data {
		p := redisPKTAPTestPacket(false, true, uint32(i), 1, data[i:i+1])
		if events := tracker.add(p, time.Now()); len(events) != 0 {
			t.Fatalf("ciphertext events=%+v", events)
		}
	}
	if tracker.buffered != 0 || len(tracker.redis.conns) != 0 {
		t.Fatal("retained Redis ciphertext")
	}
}

func TestPKTAPRedisTLSFragmentedFirstReply(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "redis"}, 0)
	now := time.Now()
	request := redisPKTAPTestPacket(false, true, 0, 1_000_000, redisPKTAPCommand("GET", "cache"))
	request.tlsObject = 1
	request.decrypted = true
	tracker.add(request, now)
	var events []captureEvent
	for i, b := range []byte("+OK\r\n") {
		p := redisPKTAPTestPacket(false, false, uint32(i), 31_000_000, []byte{b})
		p.tlsObject = 1
		p.decrypted = true
		events = append(events, tracker.add(p, now)...)
	}
	if len(events) != 1 || events[0].Event != redisEventReply {
		t.Fatalf("fragmented TLS reply=%+v", events)
	}
}
