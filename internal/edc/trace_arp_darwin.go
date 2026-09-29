package edc

import (
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// traceARPTableInterval은 macOS가 ARP table을 다시 읽는 간격이다. routing socket 알림이 ARP 항목의 모든 변화를 알리는지
// 확인하지 못해, arp -an과 같은 sysctl로 table을 읽고 직전 table과 비교한다.
const traceARPTableInterval = time.Second

// readDarwinARPTable은 IPv4 ARP table을 읽는다. syscall.RouteRIB는 deprecated이지만, x/net/route를 새 의존성으로
// 들이지 않으려고 쓴다. arp -an과 같은 NET_RT_FLAGS, RTF_LLINFO 조회이고, 주소 family를 0(전체)으로 묻기 때문에
// parseDarwinARPTable이 IPv4 항목만 고른다.
func readDarwinARPTable(names arpInterfaceNames) (map[arpNeighborKey]arpNeighbor, error) {
	rib, err := syscall.RouteRIB(syscall.NET_RT_FLAGS, syscall.RTF_LLINFO)
	if err != nil {
		return nil, fmt.Errorf("read ARP table: %w", err)
	}
	entries, err := parseDarwinARPTable(rib)
	if err != nil {
		return nil, fmt.Errorf("parse ARP table: %w", err)
	}
	return darwinARPNeighbors(entries, names), nil
}

// collectARPEvents는 ARP table을 1초마다 읽어 바뀐 항목을 event로 낸다. root가 필요 없다.
func collectARPEvents(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	names := arpInterfaceNames{}
	tracker := newARPTracker()
	previous, err := readDarwinARPTable(names)
	if err != nil {
		return captureSummary{}, err
	}
	for _, neighbor := range previous {
		tracker.baseline(neighbor)
	}
	var eventCount uint64
	finish := func() (captureSummary, error) {
		return captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount}, nil
	}
	deadline := time.Now().Add(duration)
	ticker := time.NewTicker(traceARPTableInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return finish()
		case <-ticker.C:
		}
		if duration > 0 && !time.Now().Before(deadline) {
			return finish()
		}
		current, err := readDarwinARPTable(names)
		if err != nil {
			return captureSummary{}, err
		}
		now := uint64(time.Now().UnixNano())
		var monotonic unix.Timespec
		_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic)
		for _, change := range arpSnapshotChanges(previous, current) {
			event, ok := tracker.event(change, now, uint64(monotonic.Nano()))
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
		previous = current
	}
}
