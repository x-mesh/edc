package edc

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
)

func TestPKTAPHTTPFilterVersionsAndPorts(t *testing.T) {
	for _, port := range []uint16{0, 8080} {
		instructions, err := pktapHTTPFilter(port)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bpf.Assemble(instructions); err != nil {
			t.Fatal(err)
		}
		machine, err := bpf.NewVM(instructions)
		if err != nil {
			t.Fatal(err)
		}
		for _, v2 := range []bool{false, true} {
			for _, ipv6 := range []bool{false, true} {
				packet := pktapTestRecord(v2, ipv6, true)
				header := int(binary.LittleEndian.Uint32(packet))
				if v2 {
					header = int(packet[0])
				}
				ip := packet[header+4:]
				ipHeader := 20
				if ipv6 {
					ipHeader = 40
				}
				for _, ports := range [][2]uint16{{50000, 8080}, {8080, 9090}, {50000, 9090}} {
					binary.BigEndian.PutUint16(ip[ipHeader:], ports[0])
					binary.BigEndian.PutUint16(ip[ipHeader+2:], ports[1])
					count, err := machine.Run(packet)
					if err != nil {
						t.Fatal(err)
					}
					want := port == 0 || ports[0] == port || ports[1] == port
					if (count > 0) != want {
						t.Fatalf("v2=%t ipv6=%t filter=%d ports=%v accepted=%t", v2, ipv6, port, ports, count > 0)
					}
				}
				if ipv6 {
					ip[6] = 17
				} else {
					ip[9] = 17
				}
				count, err := machine.Run(packet)
				if err != nil || count != 0 {
					t.Fatalf("UDP was accepted: v2=%t ipv6=%t result=%d err=%v", v2, ipv6, count, err)
				}
			}
		}
	}
}
func TestPKTAPHTTPFilterVariableHeaderSizes(t *testing.T) {
	instructions, err := pktapHTTPFilter(8080)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := bpf.NewVM(instructions)
	if err != nil {
		t.Fatal(err)
	}
	packet := pktapTestRecord(false, false, true)
	oldHeader := int(binary.LittleEndian.Uint32(packet))
	expanded := append([]byte{}, packet[:oldHeader]...)
	expanded = append(expanded, make([]byte, 300)...)
	expanded = append(expanded, packet[oldHeader:]...)
	binary.LittleEndian.PutUint32(expanded, uint32(oldHeader+300))
	if count, err := machine.Run(expanded); err != nil || count == 0 {
		t.Fatalf("large v1 header: result=%d err=%v", count, err)
	}
	packet = pktapTestRecord(true, true, true)
	header := int(packet[0])
	expanded = append([]byte{}, packet[:header]...)
	expanded = append(expanded, make([]byte, 300)...)
	expanded = append(expanded, packet[header+4:]...)
	binary.LittleEndian.PutUint16(expanded[8:], 300)
	if count, err := machine.Run(expanded); err != nil || count == 0 {
		t.Fatalf("large v2 frame prefix: result=%d err=%v", count, err)
	}
	for n := 0; n < len(expanded); n++ {
		if _, err := machine.Run(expanded[:n]); err != nil {
			t.Fatalf("truncated packet length=%d: %v", n, err)
		}
	}
}
func TestPKTAPHTTPFilterKeepsIPv6ExtensionHeaders(t *testing.T) {
	instructions, err := pktapHTTPFilter(8080)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := bpf.NewVM(instructions)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []byte{0, 43, 51, 60} {
		packet := pktapTestRecord(true, true, true)
		header := int(packet[0])
		ip := packet[header+4:]
		tcp := append([]byte{}, ip[40:]...)
		ip = append(ip[:40], make([]byte, 8)...)
		ip = append(ip, tcp...)
		ip[6], ip[40] = protocol, 6
		binary.BigEndian.PutUint16(ip[4:], uint16(len(ip)-40))
		packet = append(packet[:header+4], ip...)
		count, err := machine.Run(packet)
		if err != nil || count == 0 {
			t.Fatalf("extension=%d result=%d err=%v", protocol, count, err)
		}
		decoded, ok := decodeHTTPPKTAP(packet, 1, 0)
		if !ok || !pktapHTTPPortMatches(decoded, 8080) {
			t.Fatalf("extension=%d did not reach the TCP decoder", protocol)
		}
	}
}
func FuzzPKTAPHTTPFilter(f *testing.F) {
	f.Add(pktapTestRecord(false, false, true))
	f.Add(pktapTestRecord(true, true, true))
	instructions, err := pktapHTTPFilter(8080)
	if err != nil {
		f.Fatal(err)
	}
	machine, err := bpf.NewVM(instructions)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, packet []byte) {
		count, err := machine.Run(packet)
		if err != nil || count < 0 || count > pktapHTTPPacketLimit {
			t.Fatalf("invalid filter result=%d err=%v", count, err)
		}
	})
}
