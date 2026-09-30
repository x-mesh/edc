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
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
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
	executable, err := os.Executable()
	if err != nil {
		// 실행 경로를 모르면 명령줄의 이름을 쓴다. 안내 문구에만 들어간다.
		executable = os.Args[0]
	}
	if err := captureCapabilityError(capabilities, rootCommand(executable, os.Args[1:])); err != nil {
		return err
	}
	return captureTraceHooksAvailable()
}

// captureCapabilities는 eBPF program을 불러오고 붙이는 데 필요한 capability다.
var captureCapabilities = []struct {
	number int
	name   string
}{{capBPF, "CAP_BPF"}, {capPerfmon, "CAP_PERFMON"}, {capNetAdmin, "CAP_NET_ADMIN"}}

// captureCapabilityError는 빠진 capability를 모두 이름으로 알리고, 지금 명령을 root로 다시 실행하는 줄을 붙인다.
func captureCapabilityError(capabilities map[int]bool, command string) error {
	var missing []string
	for _, capability := range captureCapabilities {
		if !capabilities[capability.number] {
			missing = append(missing, capability.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errors.New(T("cli.capture.capability_missing", strings.Join(missing, ", "), command))
}

// rootCommand는 지금 명령을 sudo로 다시 실행하는 줄이다. Ubuntu의 sudo는 secure_path만 PATH로 써서 ~/.local/bin의
// edc를 찾지 못하므로 실행 파일의 전체 경로를 쓴다.
func rootCommand(executable string, args []string) string {
	words := []string{"sudo", shellWord(executable)}
	for _, arg := range args {
		words = append(words, shellWord(arg))
	}
	return strings.Join(words, " ")
}

// shellWord는 셸에 그대로 붙여 넣을 수 있게, 특수 문자가 있을 때만 작은따옴표로 감싼다.
func shellWord(word string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/-_.=:,@+%", r)
	}
	if word != "" && strings.IndexFunc(word, func(r rune) bool { return !plain(r) }) < 0 {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// captureTraceHooksAvailable은 fentry와 fexit 대상이 kernel BTF에 있는지 본다. UDP 송신 함수는 static이라 kernel
// build에 따라 inline되어 사라질 수 있다. 확인하지 않으면 object load가 실패해 TCP trace까지 이유 없이 멈춘다.
func captureTraceHooksAvailable() error {
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.btf_missing"), err)
	}
	for _, hook := range []struct{ name, message string }{
		{"udp_send_skb", "cli.capture.udp_send_hook_missing"},
		{"udp_v6_send_skb", "cli.capture.udp_send_hook_missing"},
		{"inet_csk_accept", "cli.capture.tcp_accept_hook_missing"},
		{"tcp_create_openreq_child", "cli.capture.tcp_accept_hook_missing"},
	} {
		var function *btf.Func
		if err := kernel.TypeByName(hook.name, &function); err != nil {
			return errors.New(T(hook.message, hook.name))
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

// closeCaptureLinks는 link를 동시에 닫는다. 떼어 낼 때마다 kernel 안에서 기다림이 있어, 순서대로 닫으면
// trace를 끝낼 때마다 1초 넘게 걸렸다. 동시에 닫으면 tracepoint link의 기다림은 겹친다. fentry와 fexit link는
// 동시에 닫아도 kernel이 하나씩 떼어 낸다.
func closeCaptureLinks(links []link.Link) {
	var wait sync.WaitGroup
	for _, current := range links {
		wait.Go(func() { _ = current.Close() })
	}
	wait.Wait()
}

func collectCaptureEvents(duration time.Duration, onEvent func(captureEvent) error) ([]captureEvent, captureSummary, error) {
	return collectCaptureEventsUntil(duration, onEvent, nil)
}

// collectCaptureEventsUntil은 capture가 파일에 쓸 event를 모은다. trace는 event를 모으지 않고 요약만 쌓는다.
func collectCaptureEventsUntil(duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) ([]captureEvent, captureSummary, error) {
	events := make([]captureEvent, 0)
	summary, err := collectCaptureEventsFor(traceScope{}, duration, func(event captureEvent) error {
		events = append(events, event)
		if onEvent != nil {
			return onEvent(event)
		}
		return nil
	}, stop)
	if err != nil {
		return nil, captureSummary{}, err
	}
	return events, summary, nil
}

// protocols는 hook을 쓰는 trace protocol이다. nil이면 모든 protocol이 쓴다.
type captureTracepoint struct {
	protocols   []string
	group, name string
	prog        *ebpf.Program
}

type captureTracing struct {
	protocols []string
	name      string
	prog      *ebpf.Program
}

// captureAttachments는 protocol에 필요한 hook만 고른다. fentry와 fexit는 뗄 때 kernel이 하나씩 처리해 hook마다
// 0.1초 넘게 걸리므로, 쓰지 않는 hook을 붙이면 trace를 끝낼 때마다 그만큼 늦어진다. 빈 protocol은 모든 hook을 고른다.
func captureAttachments(objects *captureEventsObjects, protocol string) ([]captureTracepoint, []captureTracing) {
	tcp, udp := []string{"tcp"}, []string{"udp", "dns"}
	// DNS는 port 53으로 가는 TCP 연결을 inet_sock_set_state로, 질의를 UDP 송신 hook으로 본다.
	tracepoints := []captureTracepoint{
		{[]string{"tcp", "dns"}, "sock", "inet_sock_set_state", objects.InetSockSetState},
		{tcp, "tcp", "tcp_retransmit_skb", objects.TcpRetransmitSkb},
		{tcp, "tcp", "tcp_send_reset", objects.TcpSendReset},
		{tcp, "tcp", "tcp_receive_reset", objects.TcpReceiveReset},
		{tcp, "tcp", "tcp_destroy_sock", objects.TcpDestroySock},
		{tcp, "sock", "sock_send_length", objects.TcpSendLength},
		{tcp, "sock", "sock_recv_length", objects.TcpRecvLength},
	}
	// skb_consume_udp는 모든 protocol이 쓴다. UDP 수신 event와 함께, target 이름을 짓는 DNS 응답도 이 hook이 읽는다.
	tracing := []captureTracing{
		{udp, "fentry/udp_send_skb", objects.UdpSendSkbEntry},
		{udp, "fexit/udp_send_skb", objects.UdpSendSkbExit},
		{udp, "fentry/udp_v6_send_skb", objects.UdpV6SendSkbEntry},
		{udp, "fexit/udp_v6_send_skb", objects.UdpV6SendSkbExit},
		// DNS 응답 시간을 network와 읽기 지연으로 나누려고 수신 큐에 들어간 시각을 잰다.
		{[]string{"dns"}, "fentry/__udp_enqueue_schedule_skb", objects.UdpEnqueueEntry},
		{nil, "fentry/skb_consume_udp", objects.SkbConsumeUdpEntry},
		// HTTP와 DNS over TCP는 TCP로 주고받는 사용자 버퍼의 앞부분을 읽는다.
		{[]string{"http", "dns"}, "fentry/tcp_sendmsg", objects.TcpSendmsgEntry},
		{[]string{"http", "dns"}, "fentry/tcp_recvmsg", objects.TcpRecvmsgEntry},
		{[]string{"http", "dns"}, "fexit/tcp_recvmsg", objects.TcpRecvmsgExit},
		// 서버 쪽 DNS over TCP도 받은 연결의 process를 알아야 해서 dns가 함께 쓴다.
		{[]string{"tcp", "dns"}, "fentry/inet_csk_accept", objects.InetCskAcceptEntry},
		{[]string{"tcp", "dns"}, "fexit/tcp_create_openreq_child", objects.TcpCreateOpenreqChildExit},
		// SYN_SENT 전에 실패한 connect도 process를 알려고 connect()에서 주인을 배운다. 이 함수는 export되어 inline으로
		// 사라지지 않으므로 captureTraceHooksAvailable에서 확인하지 않는다.
		{tcp, "fentry/__inet_stream_connect", objects.InetStreamConnectEntry},
	}
	// 빈 protocol은 capture다. capture는 TCP와 UDP hook을 모두 쓰고 DNS 전용 hook은 쓰지 않는다.
	wanted := []string{protocol}
	if protocol == "" {
		wanted = []string{"tcp", "udp"}
	}
	unwanted := func(protocols []string) bool {
		return protocols != nil && !slices.ContainsFunc(wanted, func(name string) bool { return slices.Contains(protocols, name) })
	}
	tracepoints = slices.DeleteFunc(tracepoints, func(hook captureTracepoint) bool { return unwanted(hook.protocols) })
	tracing = slices.DeleteFunc(tracing, func(hook captureTracing) bool { return unwanted(hook.protocols) })
	return tracepoints, tracing
}

// captureEventFilter는 BPF를 불러오기 전에 정하는 event 필터다. protocol이 쓰지 않을 event를 kernel에서 버린다.
type captureEventFilter struct {
	udpEvents    bool
	dnsSent      bool
	server       bool
	tcpStatePort uint16
	httpMessages bool
	dnsTCP       bool
	// httpPayload는 HTTP message를 httpRecordPayloadMax까지 읽는다. 끄면 BPF 기본값인 512바이트만 읽는다.
	httpPayload bool
	// httpPort가 0이 아니면 로컬이나 상대 port가 이 값인 socket의 HTTP message만 본다.
	httpPort uint16
}

func captureEventFilterFor(scope traceScope) captureEventFilter {
	filter := captureEventFilter{udpEvents: true, dnsSent: true}
	switch scope.protocol {
	case "tcp":
		// skb_consume_udp는 target 이름을 지을 DNS 응답만 보낸다.
		filter.udpEvents = false
	case "udp":
		filter.dnsSent = false
	case "dns":
		filter.udpEvents, filter.server, filter.tcpStatePort, filter.dnsTCP = false, scope.server, 53, true
	case "http":
		filter.udpEvents, filter.httpMessages, filter.httpPayload, filter.httpPort = false, true, scope.payload, scope.port
	}
	return filter
}

func loadCaptureEventsFor(scope traceScope, objects *captureEventsObjects) error {
	spec, err := loadCaptureEvents()
	if err != nil {
		return err
	}
	var variables captureEventsVariableSpecs
	if err := spec.Assign(&variables); err != nil {
		return err
	}
	filter := captureEventFilterFor(scope)
	flag := func(on bool) uint8 {
		if on {
			return 1
		}
		return 0
	}
	if err := errors.Join(variables.EmitUdpEvents.Set(flag(filter.udpEvents)), variables.EmitDnsSent.Set(flag(filter.dnsSent)), variables.EmitDnsServer.Set(flag(filter.server)), variables.TcpStatePort.Set(filter.tcpStatePort),
		variables.EmitHttpMessages.Set(flag(filter.httpMessages)), variables.EmitDnsTcpMessages.Set(flag(filter.dnsTCP)), variables.HttpPort.Set(filter.httpPort)); err != nil {
		return err
	}
	if filter.httpPayload {
		if err := variables.HttpPayloadLimit.Set(uint32(httpRecordPayloadMax)); err != nil {
			return err
		}
	}
	return spec.LoadAndAssign(objects, nil)
}

func collectCaptureEventsFor(scope traceScope, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	protocol := scope.protocol
	if err := rlimit.RemoveMemlock(); err != nil {
		return captureSummary{}, fmt.Errorf("remove memlock limit: %w", err)
	}
	// 프로그램을 붙이기 전에 채워서, 붙인 뒤 첫 event를 읽는 시점을 늦추지 않는다.
	names := newDNSNameCache()
	seedResolverCache(names)
	objects := captureEventsObjects{}
	if err := loadCaptureEventsFor(scope, &objects); err != nil {
		return captureSummary{}, fmt.Errorf("load eBPF objects: %w", err)
	}
	defer objects.Close()

	attachments, tracing := captureAttachments(&objects, protocol)
	links := make([]link.Link, 0, len(attachments)+len(tracing))
	closeLinks := func() { closeCaptureLinks(links) }
	for _, attachment := range attachments {
		attached, err := link.Tracepoint(attachment.group, attachment.name, attachment.prog, nil)
		if err != nil {
			closeLinks()
			return captureSummary{}, fmt.Errorf("attach %s/%s: %w", attachment.group, attachment.name, err)
		}
		links = append(links, attached)
	}
	for _, attachment := range tracing {
		attached, err := link.AttachTracing(link.TracingOptions{Program: attachment.prog})
		if err != nil {
			closeLinks()
			// fentry와 fexit는 BPF trampoline이 필요하다. UDP 목적지를 socket 기준으로 대신 기록하면 틀린 값이 나오므로 멈춘다.
			return captureSummary{}, fmt.Errorf("attach %s (needs BPF trampolines: x86_64 5.5+, arm64 6.0+): %w", attachment.name, err)
		}
		links = append(links, attached)
	}
	defer closeLinks()

	reader, err := ringbuf.NewReader(objects.Events)
	if err != nil {
		return captureSummary{}, fmt.Errorf("open event ring: %w", err)
	}
	defer reader.Close()
	clockOffset, err := captureClockOffset()
	if err != nil {
		return captureSummary{}, fmt.Errorf("read monotonic clock: %w", err)
	}
	deadline := time.Now().Add(duration)
	if duration > 0 {
		reader.SetDeadline(deadline)
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
	sockets := newSocketTargetCache()
	owners := newPIDTargetCache()
	queries := newDNSQueryTracker(scope.server)
	requests := newHTTPTracker(scope.server, scope.payload)
	streams := newDNSTCPStreams()
	var eventCount uint64
	finish := func() (captureSummary, error) {
		var lost uint64
		if lookupErr := objects.LostEvents.Lookup(uint32(0), &lost); lookupErr != nil {
			return captureSummary{}, fmt.Errorf("read lost event count: %w", lookupErr)
		}
		return captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: eventCount, LostEvents: lost}, nil
	}
	for {
		// ring buffer reader는 버퍼가 비었을 때만 deadline을 본다. event가 계속 쌓이면 버퍼가 비지 않아
		// --duration이 지나도 끝나지 않으므로 여기서 직접 확인한다.
		if duration > 0 && !time.Now().Before(deadline) {
			return finish()
		}
		record, err := reader.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return finish()
		}
		if errors.Is(err, os.ErrClosed) && traceStopRequested(stop) {
			return finish()
		}
		if err != nil {
			return captureSummary{}, err
		}
		if packet, ok := parseDNSRecord(record.RawSample); ok {
			if !packet.sent {
				names.rememberAnswer(packet.pid, dnsAnswerNames(packet.payload))
			}
			// DNS 레코드는 다른 protocol에서 target 이름에만 쓴다. capture와 trace tcp/udp의 출력에 섞지 않는다.
			if protocol != "dns" {
				continue
			}
			// 서버 쪽 레코드는 --side server일 때만 BPF가 보낸다. client 쪽 레코드는 늘 오므로 tracker가 다른 쪽을 거른다.
			// TCP 조각은 길이와 message를 맞춘 뒤 UDP message와 같은 방법으로 읽는다.
			messages := [][]byte{packet.payload}
			if packet.tcp {
				messages = streams.messages(dnsTCPStreamKey{socket: packet.socket, sent: packet.sent}, packet.payload, dnsRecordPayloadSize)
			}
			for _, message := range messages {
				packet.payload = message
				event, ok := queries.event(packet, clockOffset)
				if !ok {
					continue
				}
				if packet.tcp {
					event.Transport = "tcp"
				}
				if onEvent != nil {
					if err := onEvent(event); err != nil {
						return captureSummary{}, err
					}
				}
				eventCount++
			}
			continue
		}
		if packet, ok := parseHTTPRecord(record.RawSample); ok {
			event, ok := requests.event(packet, clockOffset)
			if !ok {
				continue
			}
			if onEvent != nil {
				if err := onEvent(event); err != nil {
					return captureSummary{}, err
				}
			}
			eventCount++
			continue
		}
		if owner, ok := parseOwnerAnnouncement(record.RawSample); ok {
			if owner.readable {
				owners.remember(owner.pid, owner.target)
			}
			continue
		}
		var raw captureEventRaw
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &raw); err != nil {
			return captureSummary{}, fmt.Errorf("decode event: %w", err)
		}
		event := raw.event(clockOffset)
		event.Source, event.Destination = sockets.addresses(event)
		commandTarget := ""
		if event.PID != 0 {
			if target, ok := owners.target(event.PID); ok {
				commandTarget = target
			} else {
				commandTarget = targets.target(event.PID, time.Now())
			}
		}
		event.Target, event.TargetSource = resolveTraceTarget(event, commandTarget, names)
		event.Target, event.TargetSource = sockets.target(event)
		if protocol == "dns" {
			if dnsEvent, ok := queries.tcpEvent(event); ok {
				event = dnsEvent
			}
		}
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return captureSummary{}, err
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
		// dual-stack socket의 IPv4 상대는 ::ffff:a.b.c.d로 담겨 온다. IPv4로 써야 같은 상대가 한 주소로 보인다.
		if netip.AddrFrom16(address).Is4In6() {
			return fmt.Sprintf("%d.%d.%d.%d:%d", address[12], address[13], address[14], address[15], port)
		}
		// RFC 5952 축약형으로 쓴다. 풀어 쓰면 ::1이 0:0:0:0:0:0:0:1이 되어 읽기 어렵다.
		return netip.AddrPortFrom(netip.AddrFrom16(address), port).String()
	}
	return ""
}
