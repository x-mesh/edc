//go:build darwin

package edc

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"time"
)

func collectDarwinDNS(scope traceScope, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	return collectDarwinHTTPEvents(scope, duration, onEvent, stop)
}

func readDarwinDNSPCAP(r io.Reader, offset int64, local map[netip.Addr]bool, server bool, tracker *dnsQueryTracker, onEvent func(captureEvent) error) error {
	var header [24]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	var order binary.ByteOrder
	switch binary.BigEndian.Uint32(header[:4]) {
	case 0xa1b2c3d4:
		order = binary.BigEndian
	case 0xd4c3b2a1:
		order = binary.LittleEndian
	default:
		return fmt.Errorf("unsupported pcap format")
	}
	link := order.Uint32(header[20:24])
	if link != 0 && link != 1 {
		return fmt.Errorf("unsupported pcap link type %d", link)
	}
	for {
		var record [16]byte
		if _, err := io.ReadFull(r, record[:]); err != nil {
			return err
		}
		size := order.Uint32(record[8:12])
		if size > 65535 {
			return fmt.Errorf("pcap packet too large: %d", size)
		}
		frame := make([]byte, size)
		if _, err := io.ReadFull(r, frame); err != nil {
			return err
		}
		src, dst, payload, ok := decodeDarwinDNSPacket(frame, link)
		if !ok {
			continue
		}
		packet, ok := darwinDNSPacketForSide(src, dst, payload, local, server)
		if !ok {
			continue
		}
		stamp := int64(order.Uint32(record[:4]))*1e9 + int64(order.Uint32(record[4:8]))*1000
		if stamp < offset {
			continue
		}
		packet.bootTimeNS = uint64(stamp - offset)
		if event, ok := tracker.event(packet, offset); ok {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
}
