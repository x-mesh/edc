package edc

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func mysqlPKTAPTestPacket(server, request bool, sequence uint32, at uint64, data []byte) pktapTCPPacket {
	packet := pktapTestTCP(request != server, sequence, string(data))
	packet.bootTimeNS = at
	packet.source, packet.destination = "127.0.0.1:50000", "127.0.0.1:3306"
	if server {
		packet.source, packet.destination = packet.destination, packet.source
	}
	return packet
}
func TestPKTAPMySQLPairsQueriesOnBothSides(t *testing.T) {
	for _, server := range []bool{false, true} {
		tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
		now := time.Now()
		query := mysqlFrame(0, mysqlQueryPayload("UPDATE t SET name = 'secret' WHERE id = 7"))
		events := tracker.add(mysqlPKTAPTestPacket(server, true, 100, 1_000_000, query), now)
		if len(events) != 1 || events[0].Event != "mysql_query" || events[0].MySQL == nil || strings.Contains(events[0].MySQL.SQL, "secret") {
			t.Fatalf("server=%t request=%#v", server, events)
		}
		events = tracker.add(mysqlPKTAPTestPacket(server, false, 900, 31_000_000, mysqlOKFrame(1, 3)), now)
		if len(events) != 1 || events[0].Event != "mysql_ok" || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 || events[0].MySQL.AffectedRows == nil || *events[0].MySQL.AffectedRows != 3 {
			t.Fatalf("server=%t response=%#v", server, events)
		}
		if server && events[0].Side != traceServerSide || !server && events[0].Side != traceClientSide {
			t.Fatalf("side=%q", events[0].Side)
		}
	}
}
func TestPKTAPMySQLFragmentedHandshakeAndQuery(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql", side: traceClientSide}, 0)
	now := time.Now()
	client, server := uint32(100), uint32(900)
	feed := func(request bool, at uint64, data []byte) []captureEvent {
		sequence := server
		if request {
			sequence = client
		}
		var events []captureEvent
		for atByte := 0; atByte < len(data); atByte++ {
			packet := mysqlPKTAPTestPacket(false, request, sequence+uint32(atByte), at+uint64(atByte), data[atByte:atByte+1])
			events = append(events, tracker.add(packet, now)...)
		}
		if request {
			client += uint32(len(data))
		} else {
			server += uint32(len(data))
		}
		return events
	}
	feed(false, 1, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps))
	events := feed(true, 100, mysqlHandshakeFrame(mysqlTestClientCaps, "fixture", "edc", []byte("fixture-auth")))
	events = append(events, feed(false, 1000, mysqlOKFrame(2, 0))...)
	connected := false
	for _, event := range events {
		connected = connected || event.Event == mysqlEventConnect
	}
	if !connected {
		t.Fatalf("fragmented handshake=%#v", events)
	}
	events = feed(true, 1_000_000, mysqlFrame(0, mysqlQueryPayload("SELECT 'masked'")))
	if len(events) != 1 || events[0].Event != "mysql_query" {
		t.Fatalf("fragmented query=%#v", events)
	}
	events = feed(false, 31_000_000, mysqlErrorFrame(1, 1064, "42000", "fixture error"))
	if len(events) != 1 || events[0].Event != mysqlEventError || events[0].MySQL.ErrorCode != 1064 {
		t.Fatalf("fragmented error=%#v", events)
	}
	tracker.sweep(now, true)
	if tracker.buffered != 0 || len(tracker.flows) != 0 {
		t.Fatalf("cleanup: buffered=%d flows=%d", tracker.buffered, len(tracker.flows))
	}
}

func TestPKTAPMySQLPortSideAndSecrets(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql", port: 3307, side: traceServerSide, showSecrets: true}, 0)
	packet := mysqlPKTAPTestPacket(true, true, 100, 1, mysqlFrame(0, mysqlQueryPayload("SELECT 'visible'")))
	if got := tracker.add(packet, time.Now()); len(got) != 0 {
		t.Fatalf("wrong port emitted events: %#v", got)
	}
	packet.source = "127.0.0.1:3307"
	got := tracker.add(packet, time.Now())
	if len(got) != 1 || got[0].Side != traceServerSide || got[0].MySQL.SQL != "SELECT 'visible'" {
		t.Fatalf("server query=%#v", got)
	}
	client := mysqlPKTAPTestPacket(false, true, 100, 1, packet.payload)
	client.destination = "127.0.0.1:3307"
	if got := tracker.add(client, time.Now()); len(got) != 0 {
		t.Fatalf("filtered client=%#v", got)
	}
	tracker.sweep(time.Now(), true)
	if len(tracker.mysql.conns) != 0 {
		t.Fatal("closed TCP flows retained MySQL connections")
	}
}
func TestPKTAPMySQLReorderedAndResentQuery(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
	now := time.Now()
	query := mysqlFrame(0, mysqlQueryPayload("SELECT 1"))
	first := mysqlPKTAPTestPacket(false, true, 100, 1, query[:2])
	tracker.add(first, now)
	tail := mysqlPKTAPTestPacket(false, true, 106, 2, query[6:])
	if got := tracker.add(tail, now); len(got) != 0 {
		t.Fatal(got)
	}
	middle := mysqlPKTAPTestPacket(false, true, 102, 3, query[2:6])
	got := tracker.add(middle, now)
	if len(got) != 1 || got[0].Event != "mysql_query" {
		t.Fatalf("reordered query=%#v", got)
	}
	if got := tracker.add(mysqlPKTAPTestPacket(false, true, 100, 4, query), now); len(got) != 0 {
		t.Fatalf("retransmission emitted a second query=%#v", got)
	}
}

func TestPKTAPMySQLLargePacketKeepsBoundedSQL(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
	now := time.Now()
	frame := mysqlFrame(0, mysqlQueryPayload("SELECT '"+strings.Repeat("x", httpMessageMax+4096)+"'"))
	var events []captureEvent
	for offset := 0; offset < len(frame); offset += 64 << 10 {
		end := min(offset+(64<<10), len(frame))
		events = append(events, tracker.add(mysqlPKTAPTestPacket(false, true, 100+uint32(offset), 1_000_000, frame[offset:end]), now)...)
	}
	if len(events) != 1 || events[0].MySQL == nil || !events[0].MySQL.SQLTruncated {
		t.Fatalf("large query=%#v", events)
	}
	events = tracker.add(mysqlPKTAPTestPacket(false, false, 900, 31_000_000, mysqlOKFrame(1, 1)), now)
	if len(events) != 1 || events[0].Event != mysqlEventOK {
		t.Fatalf("large query response=%#v", events)
	}
	tracker.sweep(now, true)
	if tracker.lost != 0 || tracker.buffered != 0 || len(tracker.mysql.conns) != 0 {
		t.Fatalf("lost=%d buffered=%d connections=%d", tracker.lost, tracker.buffered, len(tracker.mysql.conns))
	}
}
func TestPKTAPMySQLReportsTLSNegotiation(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
	now := time.Now()
	tracker.add(mysqlPKTAPTestPacket(false, false, 900, 1, mysqlGreetingFrame("8.4.11", mysqlTestServerCaps)), now)
	request := make([]byte, 32)
	binary.LittleEndian.PutUint32(request, mysqlCapProtocol41|mysqlCapSSL)
	events := tracker.add(mysqlPKTAPTestPacket(false, true, 100, 2, mysqlFrame(1, request)), now)
	if len(events) != 1 || events[0].Event != mysqlEventTLS || events[0].MySQL == nil || !events[0].MySQL.TLS {
		t.Fatalf("TLS negotiation=%#v", events)
	}
}

func TestPKTAPMySQLPreparedStatementLifetime(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "mysql"}, 0)
	now := time.Now()
	client, server := uint32(100), uint32(900)
	send := func(request bool, at uint64, data []byte) []captureEvent {
		sequence := server
		if request {
			sequence = client
			client += uint32(len(data))
		} else {
			server += uint32(len(data))
		}
		return tracker.add(mysqlPKTAPTestPacket(false, request, sequence, at, data), now)
	}
	send(true, 1, mysqlFrame(0, append([]byte{mysqlComStmtPrepare}, "SELECT id FROM t WHERE name = ?"...)))
	send(false, 2, mysqlFrame(1, []byte{0, 5, 0, 0, 0, 1, 0, 1, 0, 0, 0, 0}))
	events := send(true, 3, mysqlFrame(0, []byte{mysqlComStmtExecute, 5, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 254, 0, 3, 'b', 'o', 'b'}))
	if len(events) != 1 || events[0].Event != "mysql_execute" || events[0].MySQL.StatementID != 5 || events[0].MySQL.SQL != "SELECT id FROM t WHERE name = ?" {
		t.Fatalf("prepared execution=%#v", events)
	}
	if strings.Contains(mysqlJSON(t, events), "bob") {
		t.Fatal("prepared values were exposed")
	}
	tracker.sweep(now, true)
	if tracker.mysql.statements != 0 || len(tracker.mysql.conns) != 0 {
		t.Fatal("closed connection retained prepared statements")
	}
}
