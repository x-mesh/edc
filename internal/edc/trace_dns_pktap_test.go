package edc

import (
	"encoding/binary"
	"github.com/miekg/dns"
	"golang.org/x/net/bpf"
	"testing"
	"time"
)

func TestPKTAPDNSFragmentedTCPAndOwnership(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "dns"}, 0)
	msg := new(dns.Msg)
	msg.SetQuestion("fixture.example.", dns.TypeA)
	query := dnsTCPFrame(t, msg)
	reply := dnsTCPFrame(t, dnsTestReply(t, msg, dns.RcodeSuccess, "fixture.example. 60 IN A 192.0.2.1"))
	packet := func(sent bool, seq uint32, at uint64, data []byte) pktapTCPPacket {
		p := pktapTestTCP(sent, seq, string(data))
		p.source, p.destination = "127.0.0.1:50000", "127.0.0.1:53"
		p.bootTimeNS = at
		return p
	}
	now := time.Now()
	var events []captureEvent
	for i := range query {
		events = append(events, tracker.add(packet(true, 100+uint32(i), 1_000_000, query[i:i+1]), now)...)
	}
	if len(events) != 1 || events[0].PID != 42 || events[0].Transport != "tcp" {
		t.Fatalf("query=%+v", events)
	}
	events = nil
	for i := range reply {
		p := packet(false, 900+uint32(i), 31_000_000, reply[i:i+1])
		p.pid = 0
		events = append(events, tracker.add(p, now)...)
	}
	if len(events) != 1 || events[0].PID != 42 || events[0].LatencyMS == nil || *events[0].LatencyMS != 30 {
		t.Fatalf("reply=%+v", events)
	}
	tracker.sweep(now, true)
	if tracker.dns.size != 0 || tracker.buffered != 0 {
		t.Fatal("retained DNS state")
	}
}

func TestPKTAPDNSUDPDecoderAndFilter(t *testing.T) {
	instructions, err := pktapTransportFilter(53, true)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := bpf.NewVM(instructions)
	if err != nil {
		t.Fatal(err)
	}
	for _, v2 := range []bool{false, true} {
		for _, v6 := range []bool{false, true} {
			data := pktapTestRecord(v2, v6, true)
			header := int(binary.LittleEndian.Uint32(data))
			if v2 {
				header = int(data[0])
			}
			ip := data[header+4:]
			ipHeader := 20
			if v6 {
				ipHeader = 40
				ip[6] = 17
			} else {
				ip[9] = 17
			}
			udp := ip[ipHeader:]
			binary.BigEndian.PutUint16(udp, 50000)
			binary.BigEndian.PutUint16(udp[2:], 53)
			binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
			count, err := vm.Run(data)
			if err != nil || count == 0 {
				t.Fatalf("UDP filter=%d %v", count, err)
			}
			packet, ok := decodeTransportPKTAP(data, 1_000_000, 0, 17)
			if !ok || packet.pid != 42 || packet.destination != "127.0.0.2:53" && !v6 {
				t.Fatalf("UDP=%+v ok=%t", packet, ok)
			}
			for i := 0; i < len(data); i++ {
				decodeTransportPKTAP(data[:i], 1_000_000, 0, 17)
			}
		}
	}
}

func TestPKTAPDNSReportsIncompleteFrame(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{protocol: "dns"}, 0)
	p := pktapTestTCP(true, 1, string([]byte{0, 12, 0}))
	p.destination = "127.0.0.1:53"
	tracker.add(p, time.Now())
	tracker.sweep(time.Now(), true)
	if tracker.lost != 1 || tracker.buffered != 0 {
		t.Fatalf("loss=%d bytes=%d", tracker.lost, tracker.buffered)
	}
}
