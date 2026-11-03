//go:build linux

package edc

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

const (
	capNetRaw = 13
	// expectReceiveBuffer는 packet socket의 수신 buffer다. 바쁜 port에서 SYN이 몰려도 user space가 읽기 전에 넘치지 않게 넉넉히 둔다.
	expectReceiveBuffer = 4 << 20
	// expectPollTimeout마다 packet이 없어도 깨어나 Ctrl-C, --duration, 돌아가는 경로 표시를 확인한다.
	expectPollTimeout = 250
	// expectReadBatch는 한 번 깨어나 읽는 packet의 상한이다. packet이 쏟아져도 Ctrl-C를 늦지 않게 본다.
	expectReadBatch = 1024
	// expectOtherReadBatch는 다른 port의 SYN을 한 번에 읽는 상한이다. 세기만 하는 값이라 이 port의 packet보다 적게 읽는다.
	expectOtherReadBatch = 256
	// expectOtherReceiveBuffer는 다른 port의 SYN을 받는 socket의 buffer다. 넘치면 그만큼 덜 셀 뿐이다.
	expectOtherReceiveBuffer = 1 << 20
	// expectDropStartWait는 drop 추적이 붙기를 기다리는 시간이다. 그 안에 실패하면 시작 줄에 skip으로 알린다.
	expectDropStartWait = time.Second
	// expectLocalCacheTTL 동안 주소가 이 host의 것인지 다시 묻지 않는다. 주소가 바뀌는 host(DHCP, VIP 이동)를 따라간다.
	expectLocalCacheTTL = 30 * time.Second
)

// expectCapabilities는 AF_PACKET socket을 여는 데 필요한 capability다.
var expectCapabilities = []traceCapability{{capNetRaw, "CAP_NET_RAW"}}

func expectPrerequisites() error {
	capabilities, err := effectiveCapabilities()
	if err != nil {
		return fmt.Errorf("%s: %w", T("cli.capture.capability_check_failed"), err)
	}
	return traceCapabilityError(expectCapabilities, capabilities, "tcp expect", currentRootCommand())
}

// runExpectCapture는 interface에 묶지 않은 AF_PACKET socket으로 모든 interface의 packet을 본다. SOCK_DGRAM이라 data가
// IP header부터 시작해 ethernet과 tun(tailscale0 같은) interface를 같은 방식으로 읽는다.
func runExpectCapture(run expectRun) error {
	program, err := expectFilter(run.port)
	if err != nil {
		return err
	}
	fd, err := openExpectPacketSocket(program, expectReceiveBuffer)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	// 다른 port로 온 SYN은 따로 연 socket으로 센다. 같은 socket에 받으면 다른 port의 SYN이 몰릴 때 receive buffer가 차서
	// 이 port의 packet을 잃는다. 열지 못하면 세지 않고 관측은 계속한다.
	otherFD := -1
	if otherProgram, err := expectOtherSYNFilter(run.port); err == nil {
		if opened, err := openExpectPacketSocket(otherProgram, expectOtherReceiveBuffer); err == nil {
			otherFD = opened
			defer unix.Close(otherFD)
			run.session.countOtherPorts()
		}
	}
	buffer := make([]byte, 2*expectSnapLength)
	session := run.session
	locals := &expectLocalAddresses{entries: map[netip.Addr]expectLocalEntry{}}
	session.isLocal = locals.contains
	session.returnPath = func(remote, local netip.Addr, arrival string) expectReturnPath {
		return expectReturnRoute(remote, local, arrival, run.port)
	}
	session.ipsecInbound = func(from netip.Prefix) bool { return expectIPsecTransportInbound(from, run.port) }
	names := expectInterfaceNames{}
	drops := startExpectDropTrace(session)
	defer drops.stop()
	run.ready(drops.status)
	polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if otherFD >= 0 {
		polls = append(polls, unix.PollFd{Fd: int32(otherFD), Events: unix.POLLIN})
	}
	for run.ctx.Err() == nil {
		ready, err := unix.Poll(polls, expectPollTimeout)
		if err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("poll packet socket: %w", err)
		}
		for read := 0; ready > 0 && read < expectReadBatch; read++ {
			size, from, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT)
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				break
			}
			if err != nil {
				return fmt.Errorf("read packet socket: %w", err)
			}
			link, ok := from.(*unix.SockaddrLinklayer)
			if !ok {
				continue
			}
			var outgoing bool
			switch link.Pkttype {
			case unix.PACKET_HOST:
			case unix.PACKET_OUTGOING:
				outgoing = true
			default:
				continue
			}
			packet, ok := parseExpectPacket(buffer[:min(size, len(buffer))], run.port, outgoing)
			if !ok {
				continue
			}
			packet.at = time.Now()
			packet.iface = names.name(link.Ifindex)
			session.packet(packet)
		}
		if otherFD >= 0 && ready > 0 {
			readExpectOtherSYNs(otherFD, buffer, run.port, session)
		}
		drops.poll()
		now := time.Now()
		session.tick(now)
		if session.stopDue(now) {
			break
		}
	}
	if stats, err := unix.GetsockoptTpacketStats(fd, unix.SOL_PACKET, unix.PACKET_STATISTICS); err == nil {
		session.mu.Lock()
		session.socketDrops = uint64(stats.Drops)
		session.mu.Unlock()
	}
	return nil
}

// openExpectPacketSocket은 모든 interface의 packet을 보는 AF_PACKET socket을 열고 filter를 붙인다.
func openExpectPacketSocket(program []bpf.RawInstruction, receiveBuffer int) (int, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(expectHostToNetwork(unix.ETH_P_ALL)))
	if err != nil {
		return -1, fmt.Errorf("open packet socket: %w", err)
	}
	filters := make([]unix.SockFilter, len(program))
	for index, instruction := range program {
		filters[index] = unix.SockFilter{Code: instruction.Op, Jt: instruction.Jt, Jf: instruction.Jf, K: instruction.K}
	}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("attach packet filter: %w", err)
	}
	// buffer를 키우지 못해도 기본 크기로 동작한다. 넘친 packet 수는 끝에 PACKET_STATISTICS로 알린다.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBuffer)
	buffer := make([]byte, 2*expectSnapLength)
	// filter를 붙이기 전에 들어온 packet은 거르지 않은 것이라 버린다.
	for {
		if _, _, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT); err != nil {
			break
		}
	}
	return fd, nil
}

// readExpectOtherSYNs는 다른 port로 들어온 SYN을 읽어 port별로 센다. 이 host가 밖으로 여는 연결의 SYN은 세지 않는다.
func readExpectOtherSYNs(fd int, buffer []byte, port uint16, session *expectSession) {
	for read := 0; read < expectOtherReadBatch; read++ {
		size, from, err := unix.Recvfrom(fd, buffer, unix.MSG_DONTWAIT)
		if err != nil {
			return
		}
		link, ok := from.(*unix.SockaddrLinklayer)
		if !ok || link.Pkttype != unix.PACKET_HOST {
			continue
		}
		if remote, local, destinationPort, ok := parseExpectOtherSYN(buffer[:min(size, len(buffer))], port); ok {
			session.otherSYN(remote, local, destinationPort)
		}
	}
}

func expectHostToNetwork(value uint16) uint16 {
	return value<<8 | value>>8
}

// expectInterfaceNames는 ifindex를 이름으로 바꾼다. tailscale처럼 interface가 다시 만들어지면 index가 바뀌므로 모를 때만 다시 묻는다.
type expectInterfaceNames map[int]string

func (names expectInterfaceNames) name(index int) string {
	if name, ok := names[index]; ok {
		return name
	}
	name := "if" + strconv.Itoa(index)
	if iface, err := net.InterfaceByIndex(index); err == nil {
		name = iface.Name
	}
	names[index] = name
	return name
}

type expectLocalEntry struct {
	local   bool
	checked time.Time
}

// expectLocalAddresses는 packet의 이 host 쪽 주소가 정말 이 host의 것인지 kernel route의 local type으로 판단한다. interface
// 주소만 보면 대역 전체를 local로 둔 host(AnyIP, TPROXY)를 놓친다.
type expectLocalAddresses struct {
	entries map[netip.Addr]expectLocalEntry
}

func (addresses *expectLocalAddresses) contains(address netip.Addr) bool {
	now := time.Now()
	if entry, ok := addresses.entries[address]; ok && now.Sub(entry.checked) < expectLocalCacheTTL {
		return entry.local
	}
	routes, err := netlink.RouteGet(net.IP(address.AsSlice()))
	local := err == nil && len(routes) > 0 && routes[0].Type == unix.RTN_LOCAL
	if len(addresses.entries) > 4096 {
		addresses.entries = map[netip.Addr]expectLocalEntry{}
	}
	addresses.entries[address] = expectLocalEntry{local: local, checked: now}
	return local
}

// expectReturnRoute는 ip route get <remote> from <local>과 같은 질문이다. 이 host가 SYN-ACK를 보낼 때 kernel이 하는 조회이고,
// 정책 routing(tailscale의 table 52 같은)의 규칙도 따른다. fwmark나 UID로 나누는 규칙은 재현하지 않는다.
func expectReturnRoute(remote, local netip.Addr, arrival string, port uint16) expectReturnPath {
	options := &netlink.RouteGetOptions{}
	if local.IsValid() {
		options.SrcAddr = net.IP(local.AsSlice())
	}
	routes, err := netlink.RouteGetWithOptions(net.IP(remote.AsSlice()), options)
	if err != nil {
		return expectReturnPath{Error: err.Error()}
	}
	if len(routes) == 0 {
		return expectReturnPath{Error: "no route to " + remote.String()}
	}
	entry := routeEntryFromNetlink(routes[0], linkNamesByIndex())
	path := expectReturnPath{Device: entry.Dev, Via: entry.Via, Source: entry.Src, Table: entry.Table}
	// rp_filter는 IPv4에만 있다.
	if arrival != "" && remote.Is4() {
		path.RPFilter = readExpectRPFilter(arrival)
	}
	path.IPsecPolicy = expectIPsecPolicyCovers(remote, local, port)
	return path
}

func readExpectRPFilter(iface string) *expectRPFilter {
	read := func(name string) (int, bool) {
		data, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + name + "/rp_filter")
		if err != nil {
			return 0, false
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		return value, err == nil
	}
	all, allOK := read("all")
	own, ownOK := read(iface)
	if !allOK || !ownOK {
		return nil
	}
	return &expectRPFilter{Interface: own, All: all, Effective: max(all, own)}
}

// expectIPsecPolicyCovers는 이 port의 답이 IPsec policy를 타고 암호화되어 나가는지 kernel의 xfrm policy로 본다.
func expectIPsecPolicyCovers(remote, local netip.Addr, port uint16) bool {
	return expectPolicyCoversReply(expectXfrmPolicies(), remote, local, port)
}

// expectIPsecTransportInbound는 from에서 이 port로 오는 packet을 받는 transport mode IPsec policy가 있는지 본다. transport mode로
// 복호화한 packet은 packet socket에 다시 보이지 않는다(Linux 6.17에서 확인). tunnel mode는 복호화한 packet이 interface에 다시 보인다.
func expectIPsecTransportInbound(from netip.Prefix, port uint16) bool {
	return expectPolicyTransportInbound(expectXfrmPolicies(), from, port)
}

// expectXfrmPolicies는 kernel의 xfrm policy를 판정에 쓰는 값으로 옮긴다. 읽지 못하면 policy가 없는 것으로 본다.
func expectXfrmPolicies() []expectPolicy {
	policies, err := netlink.XfrmPolicyList(netlink.FAMILY_ALL)
	if err != nil {
		return nil
	}
	converted := make([]expectPolicy, 0, len(policies))
	for _, policy := range policies {
		if policy.Dir != netlink.XFRM_DIR_OUT && policy.Dir != netlink.XFRM_DIR_IN {
			continue
		}
		entry := expectPolicy{
			inbound: policy.Dir == netlink.XFRM_DIR_IN, src: policy.Src, dst: policy.Dst, proto: int(policy.Proto),
			srcPort: policy.SrcPort, dstPort: policy.DstPort, priority: policy.Priority, ifid: policy.Ifid,
			allow: policy.Action == netlink.XFRM_POLICY_ALLOW, templates: len(policy.Tmpls),
		}
		if policy.Mark != nil {
			entry.mark = policy.Mark.Value
		}
		for _, template := range policy.Tmpls {
			if template.Mode == netlink.XFRM_MODE_TRANSPORT {
				entry.transport = true
			}
		}
		converted = append(converted, entry)
	}
	return converted
}

// expectDropTracer는 tcp expect가 함께 돌리는 trace drop이다. 붙지 않아도 packet 관측은 계속한다.
type expectDropTracer struct {
	status  expectDropTrace
	session *expectSession
	halt    chan struct{}
	done    chan error
	ended   bool
}

func startExpectDropTrace(session *expectSession) *expectDropTracer {
	tracer := &expectDropTracer{status: expectDropTrace{Status: StatusPass}, session: session}
	if err := dropTracePrerequisites(); err != nil {
		tracer.status = expectDropTrace{Status: StatusSkip, Reason: firstLine(err.Error())}
		tracer.ended = true
		return tracer
	}
	tracer.halt = make(chan struct{})
	tracer.done = make(chan error, 1)
	go func() {
		summary, err := collectDropEvents(nil, 0, func(event captureEvent) error {
			session.drop(time.Unix(0, int64(event.TimestampNS)), event.Source, event.Destination, event.Reason, event.Location)
			return nil
		}, tracer.halt)
		// stop()이 done을 기다리므로 그 전에 적어야 result()가 읽는다.
		session.mu.Lock()
		session.dropLost = summary.LostEvents
		session.mu.Unlock()
		tracer.done <- err
	}()
	select {
	case err := <-tracer.done:
		tracer.fail(err)
	case <-time.After(expectDropStartWait):
	}
	return tracer
}

// poll은 시작 뒤에 drop 추적이 끝났는지 본다. 끝났으면 요약에 skip과 이유를 남긴다.
func (tracer *expectDropTracer) poll() {
	if tracer.ended {
		return
	}
	select {
	case err := <-tracer.done:
		tracer.fail(err)
		tracer.session.mu.Lock()
		tracer.session.dropTrace = tracer.status
		tracer.session.mu.Unlock()
	default:
	}
}

func (tracer *expectDropTracer) fail(err error) {
	tracer.ended = true
	reason := "drop trace stopped"
	if err != nil {
		reason = firstLine(err.Error())
	}
	tracer.status = expectDropTrace{Status: StatusSkip, Reason: reason}
}

func (tracer *expectDropTracer) stop() {
	if tracer.ended {
		return
	}
	close(tracer.halt)
	select {
	case <-tracer.done:
	case <-time.After(2 * time.Second):
	}
	tracer.ended = true
}
