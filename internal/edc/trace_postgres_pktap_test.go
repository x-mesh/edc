package edc

import (
	"strings"
	"testing"
	"time"
)

func TestPKTAPPostgresFragmentedStartupAndQuery(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "postgres"}, 0)
	now := time.Now()
	packet := func(sent bool, seq uint32, at uint64, data []byte) pktapTCPPacket {
		p := pktapTestTCP(sent, seq, string(data))
		p.source, p.destination = "127.0.0.1:50000", "127.0.0.1:5432"
		p.bootTimeNS = at
		return p
	}
	syn := packet(true, 99, 1, nil)
	syn.flags = 2
	tracker.add(syn, now)
	startup := pgStartup("user", "fixture", "database", "test")
	var events []captureEvent
	for i := range startup {
		events = append(events, tracker.add(packet(true, 100+uint32(i), 1, startup[i:i+1]), now)...)
	}
	if len(events) != 1 || events[0].Postgres == nil {
		t.Fatalf("startup=%+v conn=%+v", events, tracker.postgres.conns[1])
	}
	tracker.add(packet(false, 900, 2, pgReady()), now)
	query := pgQuery("select 'private-value'")
	events = tracker.add(packet(true, 100+uint32(len(startup)), 1_000_000, query), now)
	if len(events) != 1 || events[0].Postgres == nil {
		t.Fatalf("query=%+v", events)
	}
	response := append(pgComplete("SELECT 1"), pgReady()...)
	events = tracker.add(packet(false, 900+uint32(len(pgReady())), 31_000_000, response), now)
	if len(events) != 1 || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 {
		t.Fatalf("response=%+v", events)
	}
	tracker.sweep(now, true)
	if len(tracker.postgres.conns) != 0 || tracker.postgres.pending != 0 || tracker.buffered != 0 {
		t.Fatal("retained PostgreSQL state")
	}
}

func TestPKTAPPostgresPreparedPortAndSide(t *testing.T) {
	for _, server := range []bool{false, true} {
		side := traceClientSide
		if server {
			side = traceServerSide
		}
		tracker := newPKTAPHTTPTracker(traceScope{protocol: "postgres", port: 6432, side: side}, 0)
		now := time.Now()
		packet := func(request bool, seq uint32, at uint64, data []byte) pktapTCPPacket {
			p := pktapTestTCP(request != server, seq, string(data))
			p.bootTimeNS = at
			p.source, p.destination = "127.0.0.1:50000", "127.0.0.1:6432"
			if server {
				p.source, p.destination = p.destination, p.source
			}
			return p
		}
		syn := packet(true, 99, 1, nil)
		syn.flags = 2
		tracker.add(syn, now)
		startup := pgStartup("user", "fixture")
		tracker.add(packet(true, 100, 1, startup), now)
		tracker.add(packet(false, 900, 2, pgReady()), now)
		query := append(pgParse("named", "SELECT 'private-value'"), pgBind("portal", "named")...)
		query = append(query, pgExecute("portal")...)
		query = append(query, pgSync()...)
		events := tracker.add(packet(true, 100+uint32(len(startup)), 1_000_000, query), now)
		if len(events) != 1 || events[0].Postgres == nil || events[0].Postgres.Statement != "named" || events[0].Side != side {
			t.Fatalf("prepared query=%+v", events)
		}
		if strings.Contains(events[0].Postgres.SQL, "private-value") {
			t.Fatal("unmasked SQL")
		}
		response := append(pgMsg('1'), pgMsg('2')...)
		response = append(response, pgComplete("SELECT 1")...)
		response = append(response, pgReady()...)
		events = tracker.add(packet(false, 900+uint32(len(pgReady())), 31_000_000, response), now)
		if len(events) != 1 || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 {
			t.Fatalf("prepared response=%+v", events)
		}
		tracker.sweep(now, true)
		if tracker.lost != 0 || tracker.postgres.pending != 0 || tracker.postgres.statements != 0 || tracker.postgres.portals != 0 {
			t.Fatal("retained prepared state or lost data")
		}
	}
}

func TestPKTAPPostgresSSLNegotiation(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "postgres"}, 0)
	now := time.Now()
	packet := func(sent bool, seq uint32, data []byte) pktapTCPPacket {
		p := pktapTestTCP(sent, seq, string(data))
		p.destination = "127.0.0.1:5432"
		return p
	}
	tracker.add(packet(true, 100, pgUntyped(postgresCodeSSL, nil)), now)
	events := tracker.add(packet(false, 900, []byte("S")), now)
	if len(events) != 1 || events[0].Event != postgresEventTLS {
		t.Fatalf("SSL answer=%+v", events)
	}
	tracker.add(packet(true, 108, []byte{0x16, 3, 3, 0, 1, 0}), now)
	tracker.sweep(now, true)
	if tracker.lost != 0 || tracker.buffered != 0 {
		t.Fatal("encrypted bytes entered PostgreSQL framing")
	}
}
