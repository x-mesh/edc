//go:build linux

package edc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const (
	capNetAdmin = 12
	capPerfmon  = 38
	capBPF      = 39
)

func runCaptureEvents(options captureEventsOptions) int {
	if options.duration <= 0 || options.duration > maxCaptureDuration {
		fmt.Fprintln(os.Stderr, T("cli.capture.duration_range"))
		return 2
	}
	output, err := captureEventsOutputPath(options.output)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !options.yes {
		fmt.Fprintf(os.Stdout, T("cli.capture.events_plan"), output, options.duration)
		if !confirm(os.Stdin, os.Stdout, T("cli.confirm"), false) {
			fmt.Fprintln(os.Stderr, T("cli.capture.cancelled"))
			return 4
		}
	}
	if err := captureEventsPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if err := captureEventsRun(options.duration, output); err != nil {
		fmt.Fprintln(os.Stderr, T("cli.capture.events_failed", err))
		return 1
	}
	fmt.Fprintln(os.Stdout, T("cli.capture.events_done", output))
	return 0
}

func captureEventsOutputPath(requested string) (string, error) {
	if requested != "" {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(absolute); err == nil {
			return "", errors.New(T("cli.capture.output_exists", absolute))
		} else if !os.IsNotExist(err) {
			return "", err
		}
		return absolute, nil
	}
	file, err := os.CreateTemp("", "edc-events-*.jsonl")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func captureEventsPrerequisites() error {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return errors.New(T("cli.capture.btf_missing"))
	}
	capabilities, err := effectiveCapabilities()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.capability_check_failed"), err)
	}
	for _, capability := range []int{capBPF, capPerfmon, capNetAdmin} {
		if !capabilities[capability] {
			return errors.New(T("cli.capture.capability_missing", capability))
		}
	}
	return nil
}

func effectiveCapabilities() (map[int]bool, error) {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "CapEff:" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return nil, err
		}
		capabilities := map[int]bool{}
		for capability := 0; capability < 64; capability++ {
			capabilities[capability] = value&(uint64(1)<<capability) != 0
		}
		return capabilities, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("CapEff is missing")
}

var captureEventsCollect = collectCaptureEventsUntil

func captureEventsRun(duration time.Duration, output string) error {
	// Ctrl-C가 process를 바로 끝내면 그때까지 모은 event와 output file을 잃는다. trace처럼 수집만
	// 멈추고 모은 결과를 쓴다.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	events, summary, err := captureEventsCollect(duration, nil, ctx.Done())
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			return err
		}
	}
	return encoder.Encode(summary)
}

func collectCaptureEvents(duration time.Duration, onEvent func(captureEvent) error) ([]captureEvent, captureSummary, error) {
	return collectCaptureEventsUntil(duration, onEvent, nil)
}

func collectCaptureEventsUntil(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, captureSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	objects := captureEventsObjects{}
	if err := loadCaptureEventsObjects(&objects, nil); err != nil {
		return nil, captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()

	attachments := []struct {
		group string
		name  string
		prog  *ebpf.Program
	}{
		{"sock", "inet_sock_set_state", objects.InetSockSetState},
		{"tcp", "tcp_retransmit_skb", objects.TcpRetransmitSkb},
		{"tcp", "tcp_send_reset", objects.TcpSendReset},
		{"tcp", "tcp_receive_reset", objects.TcpReceiveReset},
		{"tcp", "tcp_destroy_sock", objects.TcpDestroySock},
		{"sock", "sock_send_length", objects.UdpSendLength},
		{"sock", "sock_recv_length", objects.UdpRecvLength},
	}
	links := make([]link.Link, 0, len(attachments))
	for _, attachment := range attachments {
		attached, err := link.Tracepoint(attachment.group, attachment.name, attachment.prog, nil)
		if err != nil {
			for _, current := range links {
				_ = current.Close()
			}
			return nil, captureSummary{}, fmt.Errorf("attach %s/%s: %w", attachment.group, attachment.name, err)
		}
		links = append(links, attached)
	}
	defer func() {
		for _, current := range links {
			_ = current.Close()
		}
	}()

	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return nil, captureSummary{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	clockOffset, err := captureClockOffset()
	if err != nil {
		return nil, captureSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	if duration > 0 {
		reader.SetDeadline(time.Now().Add(duration))
	}
	readerDone := make(chan struct{})
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = reader.Close()
			case <-readerDone:
			}
		}()
		defer close(readerDone)
	}

	targets := newCommandTargetCache(commandTarget)
	events := make([]captureEvent, 0)
	var eventCount uint64
	for {
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			var lost uint64
			if lookupErr := objects.LostEvents.Lookup(uint32(0), &lost); lookupErr != nil {
				return nil, captureSummary{}, fmt.Errorf("read lost event count: %w", lookupErr)
			}
			return events, captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost}, nil
		}
		if errors.Is(err, os.ErrClosed) && traceStopRequested(stop) {
			var lost uint64
			if lookupErr := objects.LostEvents.Lookup(uint32(0), &lost); lookupErr != nil {
				return nil, captureSummary{}, fmt.Errorf("read lost event count: %w", lookupErr)
			}
			return events, captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost}, nil
		}
		if err != nil {
			return nil, captureSummary{}, err
		}
		var raw captureEventRaw
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &raw); err != nil {
			return nil, captureSummary{}, fmt.Errorf("decode event: %w", err)
		}
		event := raw.event(clockOffset)
		if event.PID != 0 {
			event.Target = targets.target(event.PID, time.Now())
		}
		events = append(events, event)
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return nil, captureSummary{}, err
			}
		}
		eventCount++
	}
}

func traceStopRequested(stop <-chan struct{}) bool {
	if stop == nil {
		return false
	}
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

type captureEventRaw struct {
	TimestampNS uint64
	EventType   uint32
	PID         uint32
	CgroupID    uint64
	SkAddr      uint64
	OldState    uint32
	NewState    uint32
	Family      uint16
	Sport       uint16
	Dport       uint16
	Protocol    uint16
	Source      [16]byte
	Destination [16]byte
	Comm        [16]byte
	Bytes       uint64
}

// captureClockOffset은 CLOCK_MONOTONIC 값에 더하면 Unix epoch 시각이 되는 차이다.
// bpf_ktime_get_ns는 부팅 후 monotonic 시간이라, 그대로 두면 요약 줄의 epoch 시각과 기준이 다르다.
func captureClockOffset() (int64, error) {
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		return 0, err
	}
	return time.Now().UnixNano() - monotonic.Nano(), nil
}

func (raw captureEventRaw) event(clockOffset int64) captureEvent {
	name, protocol := captureEventTypeName(raw.EventType, raw.Protocol)
	switch raw.EventType {
	case 1:
		name = captureEventName(raw.OldState, raw.NewState)
	}
	return captureEvent{
		SocketID:    raw.SkAddr,
		TimestampNS: uint64(int64(raw.TimestampNS) + clockOffset),
		BootTimeNS:  raw.TimestampNS,
		Event:       name,
		Protocol:    protocol,
		PID:         raw.PID,
		Process:     strings.TrimRight(string(raw.Comm[:]), "\x00"),
		CgroupID:    raw.CgroupID,
		Source:      formatCaptureAddress(raw.Family, raw.Source, raw.Sport),
		Destination: formatCaptureAddress(raw.Family, raw.Destination, raw.Dport),
		OldState:    tcpStateName(raw.OldState),
		NewState:    tcpStateName(raw.NewState),
		Bytes:       raw.Bytes,
	}
}

func formatCaptureAddress(family uint16, address [16]byte, port uint16) string {
	if family == 2 {
		return fmt.Sprintf("%d.%d.%d.%d:%d", address[0], address[1], address[2], address[3], port)
	}
	if family == 10 {
		parts := make([]string, 0, 8)
		for offset := 0; offset < 16; offset += 2 {
			parts = append(parts, fmt.Sprintf("%x", binary.BigEndian.Uint16(address[offset:offset+2])))
		}
		return fmt.Sprintf("[%s]:%d", strings.Join(parts, ":"), port)
	}
	return ""
}
