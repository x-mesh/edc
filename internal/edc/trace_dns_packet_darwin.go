//go:build darwin

package edc

import (
	"encoding/binary"
	"net/netip"
)

// decodeDarwinDNSPacket accepts Ethernet and loopback frames with an unfragmented UDP datagram.
func decodeDarwinDNSPacket(frame []byte, link uint32) (netip.AddrPort, netip.AddrPort, []byte, bool) {
	var empty netip.AddrPort
	var version byte
	switch link {
	case 1: // DLT_EN10MB
		if len(frame) < 14 {
			return empty, empty, nil, false
		}
		kind := binary.BigEndian.Uint16(frame[12:14])
		frame = frame[14:]
		for kind == 0x8100 || kind == 0x88a8 {
			if len(frame) < 4 {
				return empty, empty, nil, false
			}
			kind = binary.BigEndian.Uint16(frame[2:4])
			frame = frame[4:]
		}
		switch kind {
		case 0x0800:
			version = 4
		case 0x86dd:
			version = 6
		default:
			return empty, empty, nil, false
		}
	case 0: // DLT_NULL
		if len(frame) < 4 {
			return empty, empty, nil, false
		}
		switch binary.LittleEndian.Uint32(frame[:4]) {
		case 2:
			version = 4
		case 30:
			version = 6
		default:
			return empty, empty, nil, false
		}
		frame = frame[4:]
	default:
		return empty, empty, nil, false
	}
	var src, dst netip.Addr
	if version == 4 {
		if len(frame) < 20 || frame[0]>>4 != 4 {
			return empty, empty, nil, false
		}
		header := int(frame[0]&15) * 4
		total := int(binary.BigEndian.Uint16(frame[2:4]))
		if header < 20 || total < header || total > len(frame) || frame[9] != 17 || binary.BigEndian.Uint16(frame[6:8])&0x3fff != 0 {
			return empty, empty, nil, false
		}
		src = netip.AddrFrom4([4]byte(frame[12:16]))
		dst = netip.AddrFrom4([4]byte(frame[16:20]))
		frame = frame[header:total]
	} else {
		if len(frame) < 40 || frame[0]>>4 != 6 || frame[6] != 17 {
			return empty, empty, nil, false
		}
		total := 40 + int(binary.BigEndian.Uint16(frame[4:6]))
		if total > len(frame) {
			return empty, empty, nil, false
		}
		src = netip.AddrFrom16([16]byte(frame[8:24]))
		dst = netip.AddrFrom16([16]byte(frame[24:40]))
		frame = frame[40:total]
	}
	if len(frame) < 8 {
		return empty, empty, nil, false
	}
	sport, dport := binary.BigEndian.Uint16(frame[:2]), binary.BigEndian.Uint16(frame[2:4])
	length := int(binary.BigEndian.Uint16(frame[4:6]))
	if (sport != 53 && dport != 53) || length < 8+traceDNSHeaderSize || length > len(frame) {
		return empty, empty, nil, false
	}
	return netip.AddrPortFrom(src, sport), netip.AddrPortFrom(dst, dport), frame[8:length], true
}

func darwinDNSPacketForSide(src, dst netip.AddrPort, payload []byte, local map[netip.Addr]bool, server bool) (dnsPacket, bool) {
	if len(payload) < traceDNSHeaderSize {
		return dnsPacket{}, false
	}
	query := payload[2]&0x80 == 0
	var client, service netip.AddrPort
	if query && dst.Port() == 53 {
		client, service = src, dst
	} else if !query && src.Port() == 53 {
		client, service = dst, src
	} else {
		return dnsPacket{}, false
	}
	if server {
		if !local[service.Addr()] {
			return dnsPacket{}, false
		}
		return dnsPacket{source: service.String(), destination: client.String(), sent: !query, payload: payload}, true
	}
	if !local[client.Addr()] {
		return dnsPacket{}, false
	}
	return dnsPacket{source: client.String(), destination: service.String(), sent: query, payload: payload}, true
}
