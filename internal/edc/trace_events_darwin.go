//go:build darwin

package edc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// macOS에는 socket event를 주는 공개 kernel hook이 없다. 그래서 nettop이 쓰는 ntstat kernel control에서
// socket별 누적 counter를 주기적으로 받고, 직전 값과의 차이로 event를 만든다. RST, 연결 지연, 재전송
// 횟수는 이 counter에 없으므로 요약은 이 값들을 관측하지 않은 값으로 표시한다.
var traceKernelEvents = false

// ntstat은 비공개 ABI다. offset은 xnu bsd/net/ntstat.h의 구조체 배치다. macOS 27의 TCP descriptor는
// pname 뒤쪽이 헤더보다 16 byte 길어서, 배치를 확인한 pname 끝까지만 읽는다.
const (
	ntstatControlName = "com.apple.network.statistics"
	// golang.org/x/sys/unix는 darwin의 SYSPROTO_CONTROL을 정의하지 않는다.
	ntstatSysprotoControl = 2

	ntstatMsgSuccess    = 0
	ntstatMsgError      = 1
	ntstatMsgAddAllSrcs = 1002
	ntstatMsgGetUpdate  = 1007
	ntstatMsgSrcAdded   = 10001
	ntstatMsgSrcRemoved = 10002
	ntstatMsgSrcUpdate  = 10006
	// 요청한 확장이 붙은 update다. 이미 사라진 socket의 마지막 update에는 확장이 없어 SRC_UPDATE로 온다.
	ntstatMsgSrcExtendedUpdate = 10007

	ntstatProviderTCP         = 2 // NSTAT_PROVIDER_TCP_KERNEL
	ntstatProviderTCPUserland = 3 // NSTAT_PROVIDER_TCP_USERLAND
	ntstatProviderUDP         = 4 // NSTAT_PROVIDER_UDP_KERNEL
	ntstatProviderUDPUserland = 5 // NSTAT_PROVIDER_UDP_USERLAND
	// Network.framework의 QUIC flow다. QUIC는 UDP 위에서 동작하므로 UDP trace로 보고한다. kernel UDP로
	// QUIC를 쓰는 process도 UDP event로 나타난다.
	ntstatProviderQUICUserland = 8 // NSTAT_PROVIDER_QUIC_USERLAND

	// root가 아니면 kernel은 확장 요청을 무시하고 확장 없는 update를 보낸다.
	ntstatExtensionDomain     = 1 // NSTAT_EXTENDED_UPDATE_TYPE_DOMAIN
	ntstatDomainFilter        = uint64(1) << (ntstatExtensionDomain + 40)
	ntstatExtensionHeaderSize = 8
	ntstatDomainNameSize      = 256

	ntstatSrcRefAll           = ^uint64(0)
	ntstatFlagClosedAfterDrop = 1 << 3

	ntstatHeaderSize        = 16
	ntstatAddAllSize        = 56
	ntstatQuerySize         = 24
	ntstatCountsOffset      = 32
	ntstatUpdateProvider    = 144
	ntstatUpdateDescriptor  = 152
	ntstatTCPDescriptorSize = 260
	ntstatUDPDescriptorSize = 196

	ntstatPollInterval = time.Second
	ntstatReadTimeout  = 100 * time.Millisecond
	ntstatPollTimeout  = 3 * time.Second
	// poll 응답 하나가 socket 수백 개의 update를 한꺼번에 queue에 넣는다. 기본 수신 buffer에서는 kernel이
	// 넘친 update를 버린다.
	ntstatReceiveBuffer = 4 << 20
)

const (
	darwinTCPClosed      = 0
	darwinTCPListen      = 1
	darwinTCPSynSent     = 2
	darwinTCPEstablished = 4
	darwinTCPTimeWait    = 10
)

type ntstatCounts struct {
	rxPackets, rxBytes, txPackets, txBytes, retransmitBytes uint64
}

type ntstatSource struct {
	ref       uint64
	protocol  string
	pid       uint32
	process   string
	state     uint32
	local     string
	localPort uint16
	remote    string
	domain    string
	counts    ntstatCounts
}

func decodeNtstatUpdate(message []byte) (ntstatSource, error) {
	if len(message) < ntstatUpdateDescriptor {
		return ntstatSource{}, fmt.Errorf("ntstat update is %d bytes, want at least %d", len(message), ntstatUpdateDescriptor)
	}
	counts := message[ntstatCountsOffset:]
	source := ntstatSource{
		ref: binary.LittleEndian.Uint64(message[16:]),
		counts: ntstatCounts{
			rxPackets:       binary.LittleEndian.Uint64(counts[0:]),
			rxBytes:         binary.LittleEndian.Uint64(counts[8:]),
			txPackets:       binary.LittleEndian.Uint64(counts[16:]),
			txBytes:         binary.LittleEndian.Uint64(counts[24:]),
			retransmitBytes: uint64(binary.LittleEndian.Uint32(counts[88:])),
		},
	}
	descriptor := message[ntstatUpdateDescriptor:]
	minimumDescriptor := ntstatTCPDescriptorSize
	var err error
	switch provider := binary.LittleEndian.Uint32(message[ntstatUpdateProvider:]); provider {
	case ntstatProviderTCP, ntstatProviderTCPUserland, ntstatProviderQUICUserland:
		// xnu의 nstat_quic_descriptor는 nstat_tcp_descriptor의 typedef라 배치가 같다.
		if len(descriptor) < ntstatTCPDescriptorSize {
			return ntstatSource{}, fmt.Errorf("ntstat TCP descriptor is %d bytes, want at least %d", len(descriptor), ntstatTCPDescriptorSize)
		}
		source.protocol = "tcp"
		if provider == ntstatProviderQUICUserland {
			// UDP event에는 TCP 상태가 없으므로 QUIC의 상태 값은 읽지 않는다.
			source.protocol = "udp"
		} else if source.state = binary.LittleEndian.Uint32(descriptor[76:]); source.state > darwinTCPTimeWait {
			return ntstatSource{}, fmt.Errorf("ntstat TCP descriptor (%d bytes) has state %d", len(descriptor), source.state)
		}
		source.pid = binary.LittleEndian.Uint32(descriptor[116:])
		source.local, source.localPort, err = ntstatAddress(descriptor[124:152])
		if err == nil {
			source.remote, _, err = ntstatAddress(descriptor[152:180])
		}
		source.process = strings.TrimRight(string(descriptor[196:260]), "\x00")
	case ntstatProviderUDP, ntstatProviderUDPUserland:
		if len(descriptor) < ntstatUDPDescriptorSize {
			return ntstatSource{}, fmt.Errorf("ntstat UDP descriptor is %d bytes, want at least %d", len(descriptor), ntstatUDPDescriptorSize)
		}
		source.protocol = "udp"
		minimumDescriptor = ntstatUDPDescriptorSize
		source.local, source.localPort, err = ntstatAddress(descriptor[56:84])
		if err == nil {
			source.remote, _, err = ntstatAddress(descriptor[84:112])
		}
		source.pid = binary.LittleEndian.Uint32(descriptor[128:])
		source.process = strings.TrimRight(string(descriptor[132:196]), "\x00")
	default:
		return ntstatSource{}, fmt.Errorf("ntstat update has provider %d", provider)
	}
	if err == nil && binary.LittleEndian.Uint32(message[8:]) == ntstatMsgSrcExtendedUpdate {
		source.domain, err = ntstatDomain(message, minimumDescriptor)
	}
	if err != nil {
		return ntstatSource{}, fmt.Errorf("ntstat %s descriptor (%d bytes): %w", source.protocol, len(descriptor), err)
	}
	// 연결하지 않은 UDP socket은 datagram마다 목적지가 달라서 kernel이 비워 둔다. 0.0.0.0:0으로 두면 그 주소로
	// 보낸 것처럼 보이고, --destination과 target 그룹에서 서로 다른 목적지가 하나로 묶인다.
	if source.protocol == "udp" && (source.remote == "0.0.0.0:0" || source.remote == "[::]:0") {
		source.remote = ""
	}
	return source, nil
}

// ntstatDomain은 descriptor 뒤에 붙은 domain 확장에서 domain 이름을 꺼낸다. descriptor 길이는 macOS 버전마다
// 달라서 확장의 위치를 고정할 수 없다. 요청한 확장은 domain 하나뿐이므로, 8 byte 경계에서 header의 길이가
// message 끝과 정확히 맞는 위치를 찾는다.
func ntstatDomain(message []byte, minimumDescriptor int) (string, error) {
	for offset := ntstatUpdateDescriptor + roundUp8(minimumDescriptor); offset+ntstatExtensionHeaderSize <= len(message); offset += 8 {
		kind := binary.LittleEndian.Uint32(message[offset:])
		length := int(binary.LittleEndian.Uint32(message[offset+4:]))
		// 0으로 채운 영역은 길이 0인 header처럼 보이지만, kernel이 쓰는 header는 길이가 0보다 크다.
		if length == 0 || offset+ntstatExtensionHeaderSize+roundUp8(length) != len(message) {
			continue
		}
		// kernel이 확장을 채우지 못하면 type을 UNKNOWN(0)으로 두고 공간만 남긴다.
		if kind == 0 {
			return "", nil
		}
		if kind != ntstatExtensionDomain {
			continue
		}
		if length < ntstatDomainNameSize {
			return "", fmt.Errorf("domain extension is %d bytes, want at least %d", length, ntstatDomainNameSize)
		}
		name := message[offset+ntstatExtensionHeaderSize : offset+ntstatExtensionHeaderSize+ntstatDomainNameSize]
		if end := bytes.IndexByte(name, 0); end >= 0 {
			name = name[:end]
		}
		return string(name), nil
	}
	return "", fmt.Errorf("extended update of %d bytes has no domain extension", len(message))
}

func roundUp8(value int) int {
	return (value + 7) &^ 7
}

// ntstatAddress는 sockaddr_in 또는 sockaddr_in6을 Linux trace의 formatCaptureAddress와 같은 형식으로 쓴다.
func ntstatAddress(sockaddr []byte) (string, uint16, error) {
	length, family := sockaddr[0], sockaddr[1]
	port := binary.BigEndian.Uint16(sockaddr[2:4])
	switch {
	case length == 16 && family == unix.AF_INET:
		return fmt.Sprintf("%d.%d.%d.%d:%d", sockaddr[4], sockaddr[5], sockaddr[6], sockaddr[7], port), port, nil
	case length == 28 && family == unix.AF_INET6:
		address := netip.AddrFrom16([16]byte(sockaddr[8:24]))
		if address.Is4In6() {
			return netip.AddrPortFrom(address.Unmap(), port).String(), port, nil
		}
		return netip.AddrPortFrom(address, port).String(), port, nil
	default:
		return "", 0, fmt.Errorf("sockaddr length %d and family %d", length, family)
	}
}

func darwinTCPStateName(state uint32) string {
	switch state {
	case darwinTCPClosed:
		return "CLOSE"
	case darwinTCPListen:
		return "LISTEN"
	case darwinTCPSynSent:
		return "SYN_SENT"
	case 3:
		return "SYN_RECV"
	case darwinTCPEstablished:
		return "ESTABLISHED"
	case 5:
		return "CLOSE_WAIT"
	case 6:
		return "FIN_WAIT1"
	case 7:
		return "CLOSING"
	case 8:
		return "LAST_ACK"
	case 9:
		return "FIN_WAIT2"
	case darwinTCPTimeWait:
		return "TIME_WAIT"
	default:
		return "UNKNOWN"
	}
}

type traceStamp struct {
	wallNS, bootNS uint64
}

// darwinClockOffset은 Unix epoch 시각에서 빼면 부팅 뒤 시간이 되는 차이다. Linux trace의 boot_time_ns와
// 같은 기준을 쓰기 위해 macOS에서도 부팅 뒤 시간을 세는 CLOCK_MONOTONIC을 읽는다.
func darwinClockOffset() (int64, error) {
	var monotonic unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &monotonic); err != nil {
		return 0, err
	}
	return time.Now().UnixNano() - monotonic.Nano(), nil
}

type ntstatTrackedSource struct {
	ntstatSource
	existing  bool
	seen      bool
	connected bool
	closed    bool
	// active는 socket이 CLOSED 말고 다른 상태를 거쳤거나 connect를 시도했는지 나타낸다. 만들기만 하고 연결하지
	// 않은 socket은 Linux에서 상태 전이 event가 없으므로, 여기서도 event를 만들지 않는다.
	active bool
}

type ntstatTracker struct {
	baseline  bool
	sources   map[uint64]*ntstatTrackedSource
	listeners map[uint16]bool
}

func newNtstatTracker() *ntstatTracker {
	return &ntstatTracker{baseline: true, sources: map[uint64]*ntstatTrackedSource{}, listeners: map[uint16]bool{}}
}

func (tracker *ntstatTracker) finishBaseline() {
	tracker.baseline = false
}

func (tracker *ntstatTracker) added(ref uint64) {
	if tracker.sources[ref] == nil {
		tracker.sources[ref] = &ntstatTrackedSource{existing: tracker.baseline}
	}
}

// updated는 source의 새 counter와 상태를 직전 값과 비교해 event를 만든다. trace를 시작할 때 이미 있던
// socket은 첫 update를 기준값으로만 쓴다. 그 전에 쌓인 byte를 이번 trace의 traffic으로 세지 않기 위해서다.
func (tracker *ntstatTracker) updated(source ntstatSource, stamp traceStamp) []captureEvent {
	tracked := tracker.sources[source.ref]
	if tracked == nil {
		tracked = &ntstatTrackedSource{existing: tracker.baseline}
		tracker.sources[source.ref] = tracked
	}
	// 사라진 socket의 마지막 update에는 domain 확장이 없으므로, 앞서 받은 domain을 이어 쓴다.
	if source.domain == "" {
		source.domain = tracked.domain
	}
	if source.protocol == "tcp" && source.state == darwinTCPListen {
		tracker.listeners[source.localPort] = true
	}
	if tracked.existing && !tracked.seen {
		tracked.ntstatSource = source
		tracked.seen = true
		tracked.connected = source.state == darwinTCPListen || darwinTCPReached(source)
		tracked.active = darwinTCPActive(source) || tracked.connected
		tracked.closed = source.state == darwinTCPTimeWait
		return nil
	}
	previous := tracked.ntstatSource
	if !tracked.seen {
		previous = ntstatSource{state: source.state}
	}
	events := make([]captureEvent, 0, 4)
	closing := false
	closedFrom := previous.state
	if source.protocol == "tcp" {
		connecting := !tracked.connected && darwinTCPReached(source)
		if connecting {
			tracked.connected = true
			closedFrom = darwinTCPEstablished
		}
		active := tracked.active || darwinTCPActive(source) || connecting
		// 거부되거나 시간이 지난 connect는 두 poll 사이에 SYN_SENT에서 CLOSED로 끝나서 CLOSED로 처음 보인다.
		// Linux처럼 SYN_SENT에서 닫힌 것으로 보고한다.
		if !tracked.seen && !connecting && source.state == darwinTCPClosed && darwinTCPActive(source) {
			closedFrom = darwinTCPSynSent
		}
		// Linux는 TIME_WAIT에 들어갈 때 원래 socket을 CLOSE로 바꾸고, 거부된 connect도 CLOSE로 끝나므로
		// 이 전이는 tcp_close 하나로만 보고한다.
		closing = active && !tracked.closed && (source.state == darwinTCPTimeWait || source.state == darwinTCPClosed)
		if connecting || (active && !closing && (!tracked.seen || previous.state != source.state)) {
			name := "tcp_state"
			if connecting {
				name = "tcp_connect"
				if tracker.listeners[source.localPort] {
					name = "tcp_accept"
				}
			}
			event := source.event(name, stamp)
			if tracked.seen {
				event.OldState = darwinTCPStateName(previous.state)
			}
			event.NewState = darwinTCPStateName(source.state)
			if connecting && closing {
				event.NewState = darwinTCPStateName(darwinTCPEstablished)
			}
			events = append(events, event)
		}
		tracked.active = active
	}
	send, receive := source.protocol+"_send", source.protocol+"_receive"
	if bytes, packets := counterDelta(source.counts.txBytes, previous.counts.txBytes), counterDelta(source.counts.txPackets, previous.counts.txPackets); source.carriesData(bytes, packets) {
		event := source.event(send, stamp)
		event.Bytes, event.Packets = bytes, packets
		events = append(events, event)
	}
	if bytes, packets := counterDelta(source.counts.rxBytes, previous.counts.rxBytes), counterDelta(source.counts.rxPackets, previous.counts.rxPackets); source.carriesData(bytes, packets) {
		event := source.event(receive, stamp)
		event.Bytes, event.Packets = bytes, packets
		events = append(events, event)
	}
	if source.protocol == "tcp" {
		if bytes := counterDelta(source.counts.retransmitBytes, previous.counts.retransmitBytes); bytes > 0 {
			event := source.event("tcp_retransmit", stamp)
			event.Bytes = bytes
			events = append(events, event)
		}
	}
	if closing {
		events = append(events, source.closeEvent(closedFrom, stamp))
		tracked.closed = true
	}
	tracked.ntstatSource = source
	tracked.seen = true
	return events
}

// darwinTCPActive는 socket이 연결을 시도했는지 판단한다. connect가 실패해 CLOSED로 끝나도 상대 주소는 남는다.
// packet counter는 SYN을 세지 않아서 근거가 되지 못한다.
func darwinTCPActive(source ntstatSource) bool {
	return source.state != darwinTCPClosed || (source.remote != "" && source.remote != "0.0.0.0:0" && source.remote != "[::]:0")
}

// darwinTCPReached는 socket이 연결을 맺은 적이 있는지 판단한다. 두 poll 사이에 연결과 종료가 모두
// 끝나면 마지막 상태가 CLOSED일 수 있으므로, 받은 byte가 있으면 연결을 맺은 것으로 본다.
func darwinTCPReached(source ntstatSource) bool {
	return source.state >= darwinTCPEstablished || source.counts.rxBytes > 0
}

func (tracker *ntstatTracker) removed(ref uint64, stamp traceStamp) []captureEvent {
	tracked := tracker.sources[ref]
	delete(tracker.sources, ref)
	if tracked == nil || !tracked.seen {
		return nil
	}
	if tracked.protocol == "tcp" && tracked.state == darwinTCPListen {
		delete(tracker.listeners, tracked.localPort)
	}
	if tracked.protocol != "tcp" || tracked.closed || !tracked.active {
		return nil
	}
	return []captureEvent{tracked.closeEvent(tracked.state, stamp)}
}

func (source ntstatSource) event(name string, stamp traceStamp) captureEvent {
	event := captureEvent{
		SocketID: source.ref, TimestampNS: stamp.wallNS, BootTimeNS: stamp.bootNS, Event: name, Protocol: source.protocol,
		PID: source.pid, Process: source.process, Target: source.domain, Source: source.local, Destination: source.remote,
	}
	if source.domain != "" {
		event.TargetSource = "system"
	}
	return event
}

func (source ntstatSource) closeEvent(previousState uint32, stamp traceStamp) captureEvent {
	event := source.event("tcp_close", stamp)
	event.OldState = darwinTCPStateName(previousState)
	event.NewState = darwinTCPStateName(darwinTCPClosed)
	return event
}

// carriesData는 구간의 counter 변화가 송수신 event가 되는지 정한다. TCP packet counter는 payload 없는 ACK도
// 세므로 byte가 있을 때만 event로 본다. Linux의 tcp_send, tcp_receive도 payload가 있을 때만 생긴다. UDP는
// 길이 0인 datagram도 실제 datagram이다.
func (source ntstatSource) carriesData(bytes, packets uint64) bool {
	if source.protocol == "tcp" {
		return bytes > 0
	}
	return bytes > 0 || packets > 0
}

func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

type ntstatClient struct {
	fd      int
	buffer  []byte
	context uint64
}

func openNtstat() (*ntstatClient, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, ntstatSysprotoControl)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", ntstatControlName, err)
	}
	client := &ntstatClient{fd: fd, buffer: make([]byte, 1<<16), context: 100}
	info := unix.CtlInfo{}
	copy(info.Name[:], ntstatControlName)
	if err := unix.IoctlCtlInfo(fd, &info); err != nil {
		client.close()
		return nil, fmt.Errorf("find %s: %w", ntstatControlName, err)
	}
	if err := unix.Connect(fd, &unix.SockaddrCtl{ID: info.Id}); err != nil {
		client.close()
		return nil, fmt.Errorf("connect %s: %w", ntstatControlName, err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, ntstatReceiveBuffer); err != nil {
		client.close()
		return nil, fmt.Errorf("set %s receive buffer: %w", ntstatControlName, err)
	}
	timeout := unix.NsecToTimeval(ntstatReadTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		client.close()
		return nil, fmt.Errorf("set %s read timeout: %w", ntstatControlName, err)
	}
	return client, nil
}

func (client *ntstatClient) close() {
	_ = unix.Close(client.fd)
}

func (client *ntstatClient) subscribe(provider uint32, filter uint64) error {
	message := make([]byte, ntstatAddAllSize)
	binary.LittleEndian.PutUint64(message[0:], uint64(provider))
	binary.LittleEndian.PutUint32(message[8:], ntstatMsgAddAllSrcs)
	binary.LittleEndian.PutUint16(message[12:], ntstatAddAllSize)
	binary.LittleEndian.PutUint64(message[16:], filter)
	binary.LittleEndian.PutUint32(message[32:], provider)
	_, err := unix.Write(client.fd, message)
	return err
}

func (client *ntstatClient) requestUpdate() (uint64, error) {
	client.context++
	message := make([]byte, ntstatQuerySize)
	binary.LittleEndian.PutUint64(message[0:], client.context)
	binary.LittleEndian.PutUint32(message[8:], ntstatMsgGetUpdate)
	binary.LittleEndian.PutUint16(message[12:], ntstatQuerySize)
	binary.LittleEndian.PutUint64(message[16:], ntstatSrcRefAll)
	_, err := unix.Write(client.fd, message)
	return client.context, err
}

// read는 datagram 하나를 받아 그 안의 message를 차례로 넘긴다. kernel은 message 여러 개를 한 datagram에
// 이어 붙인다. 제한 시간 안에 받은 것이 없으면 false를 돌려준다.
func (client *ntstatClient) read(handle func(message []byte) error) (bool, error) {
	n, err := unix.Read(client.fd, client.buffer)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", ntstatControlName, err)
	}
	for offset := 0; offset < n; {
		if n-offset < ntstatHeaderSize {
			return true, fmt.Errorf("ntstat message has %d trailing bytes", n-offset)
		}
		length := int(binary.LittleEndian.Uint16(client.buffer[offset+12:]))
		if length < ntstatHeaderSize || offset+length > n {
			return true, fmt.Errorf("ntstat message length %d exceeds %d received bytes", length, n-offset)
		}
		if err := handle(client.buffer[offset : offset+length]); err != nil {
			return true, err
		}
		offset += length
	}
	return true, nil
}

type ntstatCollector struct {
	client      *ntstatClient
	tracker     *ntstatTracker
	onEvent     func(captureEvent) error
	eventCount  uint64
	lostEvents  uint64
	pendingPoll uint64
	clockOffset int64
}

func (collector *ntstatCollector) stamp() traceStamp {
	wall := time.Now().UnixNano()
	return traceStamp{wallNS: uint64(wall), bootNS: uint64(wall - collector.clockOffset)}
}

func (collector *ntstatCollector) handle(message []byte) error {
	context := binary.LittleEndian.Uint64(message[0:])
	switch binary.LittleEndian.Uint32(message[8:]) {
	case ntstatMsgSrcAdded:
		if len(message) >= 24 {
			collector.tracker.added(binary.LittleEndian.Uint64(message[16:]))
		}
	case ntstatMsgSrcRemoved:
		if len(message) < 24 {
			return nil
		}
		// kernel이 queue가 넘쳐 이 source의 마지막 update를 버렸다는 표시다. 그 사이의 byte는 잃는다.
		if binary.LittleEndian.Uint16(message[14:])&ntstatFlagClosedAfterDrop != 0 {
			collector.lostEvents++
		}
		return collector.emit(collector.tracker.removed(binary.LittleEndian.Uint64(message[16:]), collector.stamp()))
	case ntstatMsgSrcUpdate, ntstatMsgSrcExtendedUpdate:
		source, err := decodeNtstatUpdate(message)
		if err != nil {
			return err
		}
		return collector.emit(collector.tracker.updated(source, collector.stamp()))
	case ntstatMsgSuccess:
		if context == collector.pendingPoll {
			collector.pendingPoll = 0
		}
	case ntstatMsgError:
		errno := syscall.Errno(0)
		if len(message) >= 20 {
			errno = syscall.Errno(binary.LittleEndian.Uint32(message[16:]))
		}
		// counter는 누적값이라 다음 poll이 빠진 byte를 따라잡는다. 다만 이번 구간의 update가 버려졌음을 남긴다.
		if context == collector.pendingPoll && errno == unix.ENOBUFS {
			collector.lostEvents++
			collector.pendingPoll = 0
			return nil
		}
		return fmt.Errorf("ntstat request %d failed: %w", context, errno)
	}
	return nil
}

func (collector *ntstatCollector) emit(events []captureEvent) error {
	for _, event := range events {
		collector.eventCount++
		if collector.onEvent != nil {
			if err := collector.onEvent(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (collector *ntstatCollector) poll() error {
	context, err := collector.client.requestUpdate()
	if err != nil {
		return fmt.Errorf("request ntstat update: %w", err)
	}
	collector.pendingPoll = context
	deadline := time.Now().Add(ntstatPollTimeout)
	for collector.pendingPoll != 0 {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("ntstat update %d did not finish in %s", context, ntstatPollTimeout)
		}
		if _, err := collector.client.read(collector.handle); err != nil {
			return err
		}
	}
	return nil
}

func (collector *ntstatCollector) start() error {
	offset, err := darwinClockOffset()
	if err != nil {
		return fmt.Errorf("read monotonic clock: %w", err)
	}
	collector.clockOffset = offset
	// userland provider는 kernel 밖의 network stack이 처리하는 flow다. kernel provider에는 나타나지 않는다.
	for _, provider := range []uint32{ntstatProviderTCP, ntstatProviderTCPUserland, ntstatProviderUDP, ntstatProviderUDPUserland, ntstatProviderQUICUserland} {
		if err := collector.client.subscribe(provider, ntstatDomainFilter); err != nil {
			return fmt.Errorf("subscribe ntstat provider %d: %w", provider, err)
		}
	}
	if err := collector.poll(); err != nil {
		return err
	}
	collector.tracker.finishBaseline()
	return nil
}

// ARP는 ARP table을 읽는다. 나머지는 ntstat로 TCP와 UDP provider를 함께 구독하고, 호출자가 protocol로 거른다.
func collectTraceEventsLive(scope traceScope, duration time.Duration, onEvent func(captureEvent) error, stop <-chan struct{}) (captureSummary, error) {
	if scope.protocol == "arp" || scope.protocol == "ndp" {
		return collectNeighborEvents(scope.protocol, duration, onEvent, stop)
	}
	client, err := openNtstat()
	if err != nil {
		return captureSummary{}, err
	}
	defer client.close()
	collector := &ntstatCollector{client: client, tracker: newNtstatTracker(), onEvent: onEvent}
	if err := collector.start(); err != nil {
		return captureSummary{}, err
	}
	var deadline time.Time
	if duration > 0 {
		deadline = time.Now().Add(duration)
	}
	nextPoll := time.Now().Add(ntstatPollInterval)
	for !darwinTraceStopped(stop) && (deadline.IsZero() || time.Now().Before(deadline)) {
		if !time.Now().Before(nextPoll) {
			if err := collector.poll(); err != nil {
				return captureSummary{}, err
			}
			nextPoll = time.Now().Add(ntstatPollInterval)
			continue
		}
		if _, err := client.read(collector.handle); err != nil {
			return captureSummary{}, err
		}
	}
	// 마지막 poll 뒤에 쌓인 byte를 잃지 않도록 멈출 때 한 번 더 받는다.
	if err := collector.poll(); err != nil {
		return captureSummary{}, err
	}
	summary := captureSummary{TimestampNS: uint64(time.Now().UnixNano()), Event: "capture_summary", EventCount: collector.eventCount, LostEvents: collector.lostEvents}
	return summary, nil
}

func readEphemeralPortRange() (int, int, bool) {
	low, lowErr := unix.SysctlUint32("net.inet.ip.portrange.first")
	high, highErr := unix.SysctlUint32("net.inet.ip.portrange.last")
	if lowErr != nil || highErr != nil || low == 0 || low > high {
		return 0, 0, false
	}
	return int(low), int(high), true
}

func darwinTraceStopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

func captureEventsPrerequisites() error {
	client, err := openNtstat()
	if err != nil {
		return err
	}
	client.close()
	return nil
}

// httpTracePrerequisites는 protocol 표가 모든 platform에서 참조한다. trace http는 Linux 전용이라 여기까지 오지 않는다.
func httpTracePrerequisites() error {
	return captureEventsPrerequisites()
}
