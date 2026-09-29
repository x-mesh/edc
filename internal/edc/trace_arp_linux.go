//go:build linux

package edc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// traceARPPollInterval은 netlink 수신을 기다리는 최대 시간이다. 이 간격으로 --duration과 Ctrl-C를 확인한다.
const traceARPPollInterval = 200 * time.Millisecond

// collectARPEvents는 kernel neighbor table의 IPv4 변화를 netlink로 받는다. root와 eBPF가 필요 없다.
func collectARPEvents(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return captureSummary{}, fmt.Errorf("open netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1 << (unix.RTNLGRP_NEIGH - 1)}); err != nil {
		return captureSummary{}, fmt.Errorf("subscribe to neighbor changes: %w", err)
	}
	timeout := unix.NsecToTimeval(traceARPPollInterval.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		return captureSummary{}, fmt.Errorf("set netlink timeout: %w", err)
	}
	names := arpInterfaceNames{}
	tracker := newARPTracker()
	// 구독한 뒤에 table을 읽는다. 먼저 읽으면 그 사이의 변화를 놓친다. 두 번 본 상태는 tracker가 거른다.
	table, err := syscall.NetlinkRIB(unix.RTM_GETNEIGH, unix.AF_INET)
	if err != nil {
		return captureSummary{}, fmt.Errorf("read neighbor table: %w", err)
	}
	messages, err := syscall.ParseNetlinkMessage(table)
	if err != nil {
		return captureSummary{}, fmt.Errorf("parse neighbor table: %w", err)
	}
	for _, message := range messages {
		if neighbor, ok := parseARPNeighbor(message, names); ok {
			tracker.baseline(neighbor)
		}
	}
	var eventCount, lost uint64
	finish := func() (captureSummary, error) {
		return captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost}, nil
	}
	deadline := time.Now().Add(duration)
	buffer := make([]byte, 1<<16)
	for {
		if traceStopRequested(stop) || (duration > 0 && !time.Now().Before(deadline)) {
			return finish()
		}
		size, _, err := unix.Recvfrom(fd, buffer, 0)
		switch {
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOBUFS):
			// 수신 buffer가 넘쳐 kernel이 알림을 버렸다. 버린 수는 알 수 없어 한 번으로 센다.
			lost++
			continue
		case err != nil:
			return captureSummary{}, fmt.Errorf("read neighbor changes: %w", err)
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:size])
		if err != nil {
			return captureSummary{}, fmt.Errorf("parse neighbor changes: %w", err)
		}
		now := uint64(time.Now().UnixNano())
		var monotonic unix.Timespec
		_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic)
		for _, message := range messages {
			neighbor, ok := parseARPNeighbor(message, names)
			if !ok {
				continue
			}
			event, ok := tracker.event(neighbor, now, uint64(monotonic.Nano()))
			if !ok {
				continue
			}
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return captureSummary{}, err
				}
			}
			eventCount++
		}
	}
}

// parseARPNeighbor는 RTM_NEWNEIGH와 RTM_DELNEIGH의 ndmsg와 속성을 읽는다. ARP를 쓰지 않는 항목(NOARP)과
// IPv6 neighbor는 뺀다. netlink는 host byte order다.
func parseARPNeighbor(message syscall.NetlinkMessage, names arpInterfaceNames) (arpNeighbor, bool) {
	if message.Header.Type != unix.RTM_NEWNEIGH && message.Header.Type != unix.RTM_DELNEIGH {
		return arpNeighbor{}, false
	}
	data := message.Data
	if len(data) < unix.SizeofNdMsg || data[0] != unix.AF_INET {
		return arpNeighbor{}, false
	}
	state := binary.NativeEndian.Uint16(data[8:10])
	if state&unix.NUD_NOARP != 0 {
		return arpNeighbor{}, false
	}
	neighbor := arpNeighbor{iface: names.name(int(int32(binary.NativeEndian.Uint32(data[4:8])))), state: arpStateName(state), deleted: message.Header.Type == unix.RTM_DELNEIGH}
	for attributes := data[unix.SizeofNdMsg:]; len(attributes) >= unix.SizeofRtAttr; {
		length := int(binary.NativeEndian.Uint16(attributes[0:2]))
		if length < unix.SizeofRtAttr || length > len(attributes) {
			break
		}
		value := attributes[unix.SizeofRtAttr:length]
		switch binary.NativeEndian.Uint16(attributes[2:4]) {
		case unix.NDA_DST:
			if address, ok := netip.AddrFromSlice(value); ok {
				neighbor.ip = address.Unmap().String()
			}
		case unix.NDA_LLADDR:
			if len(value) > 0 {
				neighbor.mac = net.HardwareAddr(value).String()
			}
		}
		attributes = attributes[min(len(attributes), (length+unix.RTA_ALIGNTO-1)&^(unix.RTA_ALIGNTO-1)):]
	}
	return neighbor, neighbor.ip != ""
}

func arpStateName(state uint16) string {
	for _, known := range []struct {
		bit  uint16
		name string
	}{
		{unix.NUD_FAILED, traceARPFailedState},
		{unix.NUD_INCOMPLETE, "INCOMPLETE"},
		{unix.NUD_REACHABLE, "REACHABLE"},
		{unix.NUD_STALE, "STALE"},
		{unix.NUD_DELAY, "DELAY"},
		{unix.NUD_PROBE, "PROBE"},
		{unix.NUD_PERMANENT, "PERMANENT"},
	} {
		if state&known.bit != 0 {
			return known.name
		}
	}
	return "NONE"
}
