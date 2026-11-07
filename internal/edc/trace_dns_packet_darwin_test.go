//go:build darwin

package edc

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/netip"
	"testing"
)

func TestDecodeDarwinDNSPacket(t *testing.T) {
	frame := make([]byte, 14+20+8+12)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	ip := frame[14:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	ip[9] = 17
	copy(ip[12:16], []byte{127, 0, 0, 1})
	copy(ip[16:20], []byte{8, 8, 8, 8})
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[:2], 12345)
	binary.BigEndian.PutUint16(udp[2:4], 53)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	src, dst, payload, ok := decodeDarwinDNSPacket(frame, 1)
	if !ok || src.String() != "127.0.0.1:12345" || dst.String() != "8.8.8.8:53" || len(payload) != 12 {
		t.Fatalf("got %v %v %d %v", src, dst, len(payload), ok)
	}
	for n := 0; n < len(frame); n++ {
		if _, _, _, ok := decodeDarwinDNSPacket(frame[:n], 1); ok {
			t.Fatalf("accepted truncated frame %d", n)
		}
	}
	ip[6] = 0x20
	if _, _, _, ok := decodeDarwinDNSPacket(frame, 1); ok {
		t.Fatal("accepted fragment")
	}
}

func TestDecodeDarwinDNSLoopbackIPv6(t *testing.T) {
	frame := make([]byte, 4+40+8+12)
	binary.LittleEndian.PutUint32(frame[:4], 30)
	ip := frame[4:]
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(ip)-40))
	ip[6] = 17
	ip[23], ip[39] = 1, 1
	udp := ip[40:]
	binary.BigEndian.PutUint16(udp[:2], 53)
	binary.BigEndian.PutUint16(udp[2:4], 4444)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	src, dst, _, ok := decodeDarwinDNSPacket(frame, 0)
	if !ok || src.String() != "[::1]:53" || dst.String() != "[::1]:4444" {
		t.Fatalf("got %v %v %v", src, dst, ok)
	}
	ip[6] = 44
	if _, _, _, ok := decodeDarwinDNSPacket(frame, 0); ok {
		t.Fatal("accepted fragment header")
	}
}

func TestDarwinDNSPacketForSide(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:12345")
	server := netip.MustParseAddrPort("127.0.0.1:53")
	local := map[netip.Addr]bool{client.Addr(): true}
	query := make([]byte, 12)
	for _, side := range []bool{false, true} {
		q, ok := darwinDNSPacketForSide(client, server, query, local, side)
		if !ok || q.sent == side {
			t.Fatalf("query side %v: %+v %v", side, q, ok)
		}
		response := make([]byte, 12)
		response[2] = 0x80
		a, ok := darwinDNSPacketForSide(server, client, response, local, side)
		if !ok || a.sent != side || a.source != q.source || a.destination != q.destination {
			t.Fatalf("response side %v: %+v %v", side, a, ok)
		}
	}
}

func TestDarwinDNSPCAP(t *testing.T) {
	var stream bytes.Buffer
	header := make([]byte, 24)
	binary.LittleEndian.PutUint32(header, 0xa1b2c3d4)
	binary.LittleEndian.PutUint32(header[20:], 0)
	stream.Write(header)
	frame := make([]byte, 4+20+8+12)
	binary.LittleEndian.PutUint32(frame, 2)
	ip := frame[4:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[9] = 17
	copy(ip[12:], []byte{127, 0, 0, 1})
	copy(ip[16:], []byte{127, 0, 0, 1})
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp, 12345)
	binary.BigEndian.PutUint16(udp[2:], 53)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	record := make([]byte, 16)
	binary.LittleEndian.PutUint32(record, 100)
	binary.LittleEndian.PutUint32(record[8:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(record[12:], uint32(len(frame)))
	stream.Write(record)
	stream.Write(frame)
	local := map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}
	var events []captureEvent
	err := readDarwinDNSPCAP(&stream, 0, local, false, newDNSQueryTracker(false), func(e captureEvent) error { events = append(events, e); return nil })
	if err != io.EOF || len(events) != 1 || events[0].Event != "dns_query" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}
