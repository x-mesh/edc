package edc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/bpf"
)

const (
	expectProbeID = "tcp.expect"
	expectUsage   = "edc tcp expect [--from <ip|cidr>] [--duration 30s] [options] <port>"
	// expectMaxDuration은 --duration의 상한이다. 기본(0)은 Ctrl-C까지 돌고, 그동안의 memory는 expectFlowTrackLimit과
	// expectSourceTrackLimit이 묶는다.
	expectMaxDuration = time.Hour
	// expectSnapLength는 kernel이 넘기는 packet 앞부분이다. IP와 TCP header만 읽고 payload는 받지 않는다.
	expectSnapLength = 128
	// expectDuplicateWindow 안에 같은 sequence로 다시 보이면 재전송이 아니라 같은 packet이다. bridge, VLAN, bond에서는
	// 한 packet이 여러 interface에 보인다. TCP의 최소 재전송 간격(200ms)보다 짧아야 재전송을 놓치지 않는다.
	expectDuplicateWindow = 150 * time.Millisecond
	// expectFlowIdle 동안 아무 packet도 없는 연결 시도는 요약으로 접는다. 바쁜 port에서 memory가 늘지 않게 한다.
	expectFlowIdle = 2 * time.Minute
	// expectSweepInterval마다 오래된 연결 시도를 접는다.
	expectSweepInterval = 5 * time.Second
	// expectSourceLimit은 JSON에 남기는 출발지 줄의 상한이다. 공개 port에서는 scanner가 수천 개의 출발지를 만든다.
	expectSourceLimit = 50
	// expectEvidenceLimit은 terminal 표에 보이는 출발지 줄의 상한이다.
	expectEvidenceLimit = 20
	// expectFlowTrackLimit은 동시에 따라가는 연결 시도의 상한이고, expectSourceTrackLimit은 합계를 남기는 출발지의 상한이다.
	// 공개 port에 SYN flood가 오면 시도마다 2분, 출발지마다 끝까지 memory를 쥐므로, 넘치는 만큼은 세기만 하고 쥐지 않는다.
	expectFlowTrackLimit   = 10000
	expectSourceTrackLimit = 10000

	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
	tcpFlagACK = 0x10
)

const (
	expectVerdictNoSYN       = "no_syn"
	expectVerdictRST         = "rst"
	expectVerdictNoACK       = "synack_no_ack"
	expectVerdictNoReply     = "syn_no_reply"
	expectVerdictIPsecHidden = "reply_hidden_ipsec"
	expectVerdictUnseen      = "dropped_unseen"
	expectVerdictCompleted   = "completed"
)

// expectSYNPathDrops는 연결을 시작하는 SYN이 버려질 때 나오는 이유다. packet socket이 SYN을 못 봐도(IPsec transport mode)
// 이 이유로 버린 packet이 있으면 SYN은 이 host까지 왔다. 연결이 끝난 뒤 늦게 온 packet에서 나오는 이유는 넣지 않는다.
var expectSYNPathDrops = map[string]bool{
	"NO_SOCKET": true, "IP_RPFILTER": true, "NETFILTER_DROP": true, "XFRM_POLICY": true,
	"TCP_LISTEN_OVERFLOW": true, "TCP_REQQFULLDROP": true, "SOCKET_FILTER": true,
}

// expectVerdictOrder는 출발지 여럿이 서로 다른 판정일 때 요약에 올릴 순서다. 앞쪽이 먼저 고칠 문제다.
var expectVerdictOrder = []string{expectVerdictNoReply, expectVerdictNoACK, expectVerdictRST, expectVerdictUnseen, expectVerdictIPsecHidden, expectVerdictCompleted}

func init() { evidenceShownProbes[expectProbeID] = true }

type expectOptions struct {
	common   commonOptions
	port     uint16
	from     netip.Prefix
	duration time.Duration
}

// parseTCPExpectArgs는 option과 port를 읽는다. 실패하면 이미 오류를 출력했고 exit code를 돌려준다.
func parseTCPExpectArgs(args []string) (expectOptions, int, bool) {
	options := expectOptions{common: configuredCommon(15 * time.Second)}
	set := newFlagSet("tcp expect")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options.common)
	fromText := ""
	set.StringVar(&fromText, "from", "", T("command.tcp.option.from"))
	set.DurationVar(&options.duration, "duration", 0, T("command.tcp.option.duration"))
	positional, err := parseInterspersed(set, args)
	if err != nil {
		return options, 2, false
	}
	if len(positional) == 0 {
		// 인자 없이 실행한 사람은 어느 host의 어떤 port를 넣을지 모른다. 묻기 전에 하는 일과 넣을 값을 알린다.
		if _, ok := terminalInput(os.Stdin); ok {
			fmt.Fprintln(os.Stdout, T("expect.prompt_hint"))
		}
		values, ok := promptMissingArgs("edc tcp expect", T("expect.prompt_port"))
		if !ok {
			fmt.Fprintln(os.Stderr, T("cli.usage", expectUsage))
			return options, 2, false
		}
		positional = values
	}
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, T("cli.usage", expectUsage))
		return options, 2, false
	}
	port, err := parseExpectPort(positional[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return options, 2, false
	}
	options.port = port
	if fromText != "" {
		if options.from, err = parseExpectFrom(fromText); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return options, 2, false
		}
	}
	if options.duration < 0 || options.duration > expectMaxDuration {
		fmt.Fprintln(os.Stderr, T("expect.duration_range", expectMaxDuration))
		return options, 2, false
	}
	return options, 0, true
}

func parseExpectPort(text string) (uint16, error) {
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || value < 1 || value > 65535 {
		return 0, errors.New(T("expect.port_invalid", text))
	}
	return uint16(value), nil
}

// parseExpectFrom은 주소 하나나 CIDR을 받는다. 주소 하나는 그 주소만 담는 prefix다.
func parseExpectFrom(text string) (netip.Prefix, error) {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "/") {
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return netip.Prefix{}, errors.New(T("expect.from_invalid", text))
		}
		if prefix.Addr().Is4In6() {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), max(prefix.Bits()-96, 0))
		}
		return prefix.Masked(), nil
	}
	address, err := netip.ParseAddr(text)
	if err != nil {
		return netip.Prefix{}, errors.New(T("expect.from_invalid", text))
	}
	address = address.Unmap().WithZone("")
	return netip.PrefixFrom(address, address.BitLen()), nil
}

// expectPacket은 TCP header 하나다. remote는 연결을 시도한 쪽, local은 이 host에서 port를 가진 쪽이다.
type expectPacket struct {
	at       time.Time
	iface    string
	outgoing bool
	remote   netip.AddrPort
	local    netip.AddrPort
	flags    uint8
	seq      uint32
}

// parseExpectPacket은 IP header부터 시작하는 data를 읽는다. kernel filter를 붙이기 전에 들어온 packet도 있으므로
// protocol과 port를 여기서 다시 확인한다. IPv6 확장 header 뒤의 TCP는 읽지 않는다.
func parseExpectPacket(data []byte, port uint16, outgoing bool) (expectPacket, bool) {
	if len(data) < 1 {
		return expectPacket{}, false
	}
	var source, destination netip.Addr
	var tcp []byte
	packet := expectPacket{outgoing: outgoing}
	switch data[0] >> 4 {
	case 4:
		headerLength := int(data[0]&0x0f) * 4
		if headerLength < 20 || len(data) < headerLength+14 || data[9] != 6 {
			return expectPacket{}, false
		}
		// 첫 조각이 아니면 TCP header가 없다.
		if binary.BigEndian.Uint16(data[6:8])&0x1fff != 0 {
			return expectPacket{}, false
		}
		source = netip.AddrFrom4([4]byte(data[12:16]))
		destination = netip.AddrFrom4([4]byte(data[16:20]))
		tcp = data[headerLength:]
	case 6:
		if len(data) < 40+14 || data[6] != 6 {
			return expectPacket{}, false
		}
		source = netip.AddrFrom16([16]byte(data[8:24])).Unmap()
		destination = netip.AddrFrom16([16]byte(data[24:40])).Unmap()
		tcp = data[40:]
	default:
		return expectPacket{}, false
	}
	sourcePort := binary.BigEndian.Uint16(tcp[0:2])
	destinationPort := binary.BigEndian.Uint16(tcp[2:4])
	packet.seq = binary.BigEndian.Uint32(tcp[4:8])
	packet.flags = tcp[13]
	// 들어온 packet은 목적지 port가, 나간 packet은 출발 port가 이 port여야 한다. 그래야 이 host가 밖으로 연
	// 연결의 임시 port가 우연히 같을 때 세지 않는다.
	if outgoing {
		if sourcePort != port {
			return expectPacket{}, false
		}
		packet.local = netip.AddrPortFrom(source, sourcePort)
		packet.remote = netip.AddrPortFrom(destination, destinationPort)
	} else {
		if destinationPort != port {
			return expectPacket{}, false
		}
		packet.remote = netip.AddrPortFrom(source, sourcePort)
		packet.local = netip.AddrPortFrom(destination, destinationPort)
	}
	return packet, true
}

// expectFilter는 kernel에서 이 port의 TCP 중 필요한 packet만 남기는 classic BPF다. 들어오는 쪽은 SYN, RST와
// payload 없는 ACK(handshake의 마지막 ACK)만, 나가는 쪽은 SYN-ACK와 RST만 남긴다. 연결된 뒤의 data는 user space로
// 오지 않아 바쁜 port에서도 가볍다. 결과는 앞 128 byte다.
func expectFilter(port uint16) ([]bpf.RawInstruction, error) {
	const flagsOfInterest = tcpFlagSYN | tcpFlagRST
	steps := []expectFilterStep{
		{ins: bpf.LoadAbsolute{Off: 0, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 4}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 4}, no: "v6"},
		{ins: bpf.LoadAbsolute{Off: 9, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 6, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x1fff}, yes: "drop"},
		{ins: bpf.LoadMemShift{Off: 0}},
		{ins: bpf.LoadIndirect{Off: 2, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, yes: "in4"},
		{ins: bpf.LoadIndirect{Off: 0, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, no: "drop"},
		{ins: bpf.LoadIndirect{Off: 13, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: flagsOfInterest}, yes: "accept", no: "drop"},
		{label: "in4", ins: bpf.LoadIndirect{Off: 13, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: flagsOfInterest}, yes: "accept"},
		// IPv4 전체 길이가 IP header와 TCP header의 합과 같으면 payload가 없다.
		{ins: bpf.LoadIndirect{Off: 12, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 4}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftLeft, Val: 2}},
		{ins: bpf.ALUOpX{Op: bpf.ALUOpAdd}},
		{ins: bpf.TAX{}},
		{ins: bpf.LoadAbsolute{Off: 2, Size: 2}},
		{ins: bpf.JumpIfX{Cond: bpf.JumpEqual}, yes: "accept", no: "drop"},
		{label: "v6", ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 6, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 42, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, yes: "in6"},
		{ins: bpf.LoadAbsolute{Off: 40, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 53, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: flagsOfInterest}, yes: "accept", no: "drop"},
		{label: "in6", ins: bpf.LoadAbsolute{Off: 53, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: flagsOfInterest}, yes: "accept"},
		// IPv6 payload 길이가 TCP header 길이와 같으면 payload가 없다.
		{ins: bpf.LoadAbsolute{Off: 52, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 4}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftLeft, Val: 2}},
		{ins: bpf.TAX{}},
		{ins: bpf.LoadAbsolute{Off: 4, Size: 2}},
		{ins: bpf.JumpIfX{Cond: bpf.JumpEqual}, yes: "accept", no: "drop"},
		{label: "accept", ins: bpf.RetConstant{Val: expectSnapLength}},
		{label: "drop", ins: bpf.RetConstant{Val: 0}},
	}
	return assembleExpectFilter(steps)
}

// expectFilterStep은 이름으로 건너뛸 곳을 적는 BPF 명령이다. 건너뛸 칸 수를 손으로 세면 명령을 고칠 때마다 틀리기 쉽다.
type expectFilterStep struct {
	label   string
	ins     bpf.Instruction
	yes, no string
}

func assembleExpectFilter(steps []expectFilterStep) ([]bpf.RawInstruction, error) {
	labels := map[string]int{}
	for index, step := range steps {
		if step.label != "" {
			labels[step.label] = index
		}
	}
	skip := func(from int, label string) (uint8, error) {
		if label == "" {
			return 0, nil
		}
		target, ok := labels[label]
		if !ok || target <= from || target-from-1 > 255 {
			return 0, fmt.Errorf("bpf jump from %d to %q", from, label)
		}
		return uint8(target - from - 1), nil
	}
	program := make([]bpf.Instruction, len(steps))
	for index, step := range steps {
		yes, err := skip(index, step.yes)
		if err != nil {
			return nil, err
		}
		no, err := skip(index, step.no)
		if err != nil {
			return nil, err
		}
		switch ins := step.ins.(type) {
		case bpf.JumpIf:
			ins.SkipTrue, ins.SkipFalse = yes, no
			program[index] = ins
		case bpf.JumpIfX:
			ins.SkipTrue, ins.SkipFalse = yes, no
			program[index] = ins
		default:
			program[index] = step.ins
		}
	}
	return bpf.Assemble(program)
}

type expectFlowKey struct {
	remote netip.AddrPort
	local  netip.AddrPort
}

// expectFlow는 SYN 하나로 시작한 연결 시도다. isn은 client의 첫 sequence이고, 같은 4-tuple에서 isn이 다르면 새 시도다.
type expectFlow struct {
	key            expectFlowKey
	iface          string
	isn            uint32
	firstSYN       time.Time
	lastSYN        time.Time
	lastActivity   time.Time
	synIn          int
	synRetransmits int
	synAckOut      int
	synAckSeq      uint32
	lastSynAck     time.Time
	rstOut         int
	lastRST        time.Time
	rstIn          int
	completed      bool
	outIfaces      []string
	drops          map[string]int
	routeShown     bool
}

type expectCounts struct {
	Attempts          int `json:"attempts"`
	SYNIn             int `json:"syn_in"`
	SYNRetransmits    int `json:"syn_retransmits"`
	SYNACKOut         int `json:"syn_ack_out"`
	SYNACKRetransmits int `json:"syn_ack_retransmits"`
	RSTOut            int `json:"rst_out"`
	RSTIn             int `json:"rst_in"`
	Handshakes        int `json:"handshakes"`
}

func (counts *expectCounts) add(other expectCounts) {
	counts.Attempts += other.Attempts
	counts.SYNIn += other.SYNIn
	counts.SYNRetransmits += other.SYNRetransmits
	counts.SYNACKOut += other.SYNACKOut
	counts.SYNACKRetransmits += other.SYNACKRetransmits
	counts.RSTOut += other.RSTOut
	counts.RSTIn += other.RSTIn
	counts.Handshakes += other.Handshakes
}

// expectRPFilter는 rp_filter 값이다. kernel은 interface 값과 all 값 중 큰 값을 쓴다.
type expectRPFilter struct {
	Interface int `json:"interface"`
	All       int `json:"all"`
	Effective int `json:"effective"`
}

// expectReturnPath는 이 host가 출발지로 답을 보낼 때 고르는 경로다. ip route get <출발지> from <SYN을 받은 주소>와 같다.
type expectReturnPath struct {
	Device         string          `json:"device,omitempty"`
	Via            string          `json:"via,omitempty"`
	Source         string          `json:"source,omitempty"`
	Table          string          `json:"table,omitempty"`
	MatchesArrival *bool           `json:"matches_arrival,omitempty"`
	RPFilter       *expectRPFilter `json:"rp_filter,omitempty"`
	IPsecPolicy    bool            `json:"ipsec_policy,omitempty"`
	Error          string          `json:"error,omitempty"`
}

// expectSource는 출발지 주소, SYN이 도착한 interface, SYN을 받은 주소가 같은 연결 시도의 합계다.
type expectSource struct {
	Address   string `json:"address"`
	Local     string `json:"local"`
	Interface string `json:"interface"`
	expectCounts
	OutInterfaces []string          `json:"out_interfaces,omitempty"`
	Verdict       string            `json:"verdict"`
	DropReasons   map[string]int    `json:"drop_reasons,omitempty"`
	Return        *expectReturnPath `json:"return,omitempty"`
}

type expectSourceKey struct {
	remote netip.Addr
	local  netip.Addr
	iface  string
}

// expectPendingDrops는 SYN보다 먼저 읽힌 drop이다.
type expectPendingDrops struct {
	at      time.Time
	reasons map[string]int
	events  []expectEvent
}

type expectDropTrace struct {
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
	// LostEvents는 drop ring buffer가 넘쳐 읽지 못한 drop 수다. 이 port의 drop도 그 안에 있을 수 있다.
	LostEvents uint64 `json:"lost_events,omitempty"`
}

// expectPolicy는 xfrm policy 중 판정에 쓰는 부분이다. netlink의 policy type은 Linux에서만 컴파일되므로 OS에 매이지 않는
// 값으로 옮겨 둔다. src와 dst가 nil이면 모든 주소에 맞는다. port가 0이면 모든 port에 맞는다.
type expectPolicy struct {
	inbound   bool
	src, dst  *net.IPNet
	proto     int
	srcPort   int
	dstPort   int
	priority  int
	ifid      int
	mark      uint32
	allow     bool
	templates int
	transport bool
}

// expectPolicyMatchesTCP는 policy가 이 port의 TCP에 맞는지다. ownPort는 이 host 쪽 port 선택자이고, peerPort는 상대 쪽이다.
// 상대 쪽 port는 연결마다 다른 임시 port라 policy가 그 port를 고정하면 이 관측의 연결 대부분에 맞지 않으므로 맞지 않는 것으로 본다.
func expectPolicyMatchesTCP(ownPort, peerPort int, port uint16) bool {
	return (ownPort == 0 || ownPort == int(port)) && peerPort == 0
}

// expectPolicyCoversReply는 답(SYN-ACK, RST)이 IPsec policy를 타고 암호화되어 나가는지 본다. 그러면 packet socket에는 평문이
// 아니라 ESP만 보인다. if_id나 mark가 있는 policy는 xfrm/VTI interface로 나가 평문이 그 interface에 보이므로 뺀다. 우선순위가
// 가장 높은(숫자가 작은) 일치 policy가 정한다. 같은 대역을 우회(bypass)로 두는 policy가 먼저 맞을 수 있다.
func expectPolicyCoversReply(policies []expectPolicy, remote, local netip.Addr, port uint16) bool {
	var matched []expectPolicy
	for _, policy := range policies {
		if policy.inbound || policy.ifid != 0 || policy.mark != 0 {
			continue
		}
		if policy.proto != 0 && policy.proto != int(syscall.IPPROTO_TCP) {
			continue
		}
		// 답은 local:port에서 remote의 임시 port로 나간다.
		if !expectPolicyMatchesTCP(policy.srcPort, policy.dstPort, port) {
			continue
		}
		if !expectNetContains(policy.dst, remote) || (local.IsValid() && !expectNetContains(policy.src, local)) {
			continue
		}
		matched = append(matched, policy)
	}
	if len(matched) == 0 {
		return false
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].priority < matched[j].priority })
	return matched[0].allow && matched[0].templates > 0
}

// expectPolicyTransportInbound는 from에서 이 port로 오는 packet을 받는 transport mode IPsec policy가 있는지 본다.
func expectPolicyTransportInbound(policies []expectPolicy, from netip.Prefix, port uint16) bool {
	for _, policy := range policies {
		if !policy.inbound || !policy.allow || policy.ifid != 0 || !policy.transport {
			continue
		}
		if policy.proto != 0 && policy.proto != int(syscall.IPPROTO_TCP) {
			continue
		}
		// SYN은 remote의 임시 port에서 local:port로 들어온다.
		if !expectPolicyMatchesTCP(policy.dstPort, policy.srcPort, port) {
			continue
		}
		if from.IsValid() && !expectNetContains(policy.src, from.Addr()) {
			continue
		}
		return true
	}
	return false
}

func expectNetContains(network *net.IPNet, address netip.Addr) bool {
	if network == nil {
		return true
	}
	return network.Contains(net.IP(address.AsSlice()))
}

// expectEvent는 실행 중에 한 줄로 보이는 일이다.
type expectEvent struct {
	at     time.Time
	kind   string
	iface  string
	remote netip.AddrPort
	local  netip.AddrPort
	count  int
	detail string
}

const (
	expectEventSYN           = "syn"
	expectEventSYNRetransmit = "syn_retransmit"
	expectEventSYNACK        = "synack"
	expectEventSYNACKRetrans = "synack_retransmit"
	expectEventRSTOut        = "rst_out"
	expectEventRSTIn         = "rst_in"
	expectEventEstablished   = "established"
	expectEventDrop          = "drop"
	expectEventRoute         = "route"
	expectEventDuplicate     = "duplicate"
)

// expectSession은 packet과 kernel drop을 받아 연결 시도로 묶는다. packet과 drop은 서로 다른 goroutine에서 오므로
// 잠금 아래에서 다룬다.
type expectSession struct {
	mu           sync.Mutex
	port         uint16
	from         netip.Prefix
	verbose      bool
	flows        map[expectFlowKey]*expectFlow
	sources      map[expectSourceKey]*expectSource
	routes       map[expectSourceKey]*expectReturnPath
	pendingDrops map[expectFlowKey]*expectPendingDrops
	otherDrops   map[string]int
	forwarded    int
	socketDrops  uint64
	dropTrace    expectDropTrace
	// dropLost는 drop ring buffer가 넘쳐 읽지 못한 drop 수다. drop 추적이 끝난 뒤에 적힌다.
	dropLost uint64
	// untracked는 flowLimit이나 sourceLimit에 막혀 세지 않은 연결 시도 수다.
	untracked   int
	flowLimit   int
	sourceLimit int
	lastSweep   time.Time
	// isLocal은 주소가 이 host의 것인지다. nil이면 모두 이 host의 것으로 본다. 다른 host로 넘겨주는 packet을 세지 않는다.
	isLocal func(netip.Addr) bool
	// returnPath는 답이 나갈 경로를 kernel에 묻는다. local이나 arrival이 비어 있을 수 있다.
	returnPath func(remote, local netip.Addr, arrival string) expectReturnPath
	// ipsecInbound는 from에서 오는 packet을 IPsec transport mode policy가 받는지다. 그러면 복호화한 SYN이 packet socket에
	// 보이지 않는다.
	ipsecInbound func(from netip.Prefix) bool
	emit         func(expectEvent)
}

func newExpectSession(port uint16, from netip.Prefix, emit func(expectEvent)) *expectSession {
	if emit == nil {
		emit = func(expectEvent) {}
	}
	return &expectSession{
		port: port, from: from, emit: emit,
		flows: map[expectFlowKey]*expectFlow{}, sources: map[expectSourceKey]*expectSource{}, routes: map[expectSourceKey]*expectReturnPath{},
		pendingDrops: map[expectFlowKey]*expectPendingDrops{}, otherDrops: map[string]int{},
		flowLimit: expectFlowTrackLimit, sourceLimit: expectSourceTrackLimit,
	}
}

func (session *expectSession) wanted(remote netip.Addr) bool {
	return !session.from.IsValid() || session.from.Contains(remote)
}

// packet은 TCP header 하나를 연결 시도에 반영한다.
func (session *expectSession) packet(packet expectPacket) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.wanted(packet.remote.Addr()) {
		return
	}
	if session.isLocal != nil && !session.isLocal(packet.local.Addr()) {
		session.forwarded++
		return
	}
	key := expectFlowKey{remote: packet.remote, local: packet.local}
	flow := session.flows[key]
	syn := packet.flags&tcpFlagSYN != 0
	ack := packet.flags&tcpFlagACK != 0
	rst := packet.flags&tcpFlagRST != 0
	event := expectEvent{at: packet.at, iface: packet.iface, remote: packet.remote, local: packet.local}
	switch {
	case !packet.outgoing && syn && !ack && !rst:
		if flow != nil && flow.isn == packet.seq && !flow.completed && flow.rstOut == 0 {
			flow.lastActivity = packet.at
			if packet.at.Sub(flow.lastSYN) < expectDuplicateWindow {
				// 같은 packet이 다른 interface에 또 보였다. IP 계층이 받는 interface가 마지막에 보이므로 그쪽을 도착 interface로 둔다.
				if flow.iface != packet.iface {
					flow.iface = packet.iface
					if session.verbose {
						event.kind = expectEventDuplicate
						session.emit(event)
					}
				}
				return
			}
			flow.synIn++
			flow.synRetransmits++
			flow.lastSYN = packet.at
			event.kind, event.count, event.iface = expectEventSYNRetransmit, flow.synIn, flow.iface
			session.emit(event)
			return
		}
		if flow != nil {
			session.finish(flow)
		} else if len(session.flows) >= session.flowLimit {
			session.untracked++
			return
		}
		flow = &expectFlow{key: key, iface: packet.iface, isn: packet.seq, firstSYN: packet.at, lastSYN: packet.at, lastActivity: packet.at, synIn: 1}
		session.flows[key] = flow
		event.kind = expectEventSYN
		session.emit(event)
		if pending, ok := session.pendingDrops[key]; ok {
			flow.drops = pending.reasons
			delete(session.pendingDrops, key)
			for _, drop := range pending.events {
				drop.iface = flow.iface
				session.emit(drop)
			}
		}
	case packet.outgoing && syn && ack:
		if flow == nil || flow.completed {
			return
		}
		flow.lastActivity = packet.at
		if flow.synAckOut > 0 && flow.synAckSeq == packet.seq && packet.at.Sub(flow.lastSynAck) < expectDuplicateWindow {
			return
		}
		flow.synAckOut++
		flow.synAckSeq, flow.lastSynAck = packet.seq, packet.at
		flow.outIfaces = appendUnique(flow.outIfaces, packet.iface)
		event.kind = expectEventSYNACK
		if flow.synAckOut > 1 {
			event.kind, event.count = expectEventSYNACKRetrans, flow.synAckOut
		}
		session.emit(event)
	case packet.outgoing && rst:
		if flow == nil || flow.completed {
			return
		}
		flow.lastActivity = packet.at
		if flow.rstOut > 0 && packet.at.Sub(flow.lastRST) < expectDuplicateWindow {
			return
		}
		flow.rstOut++
		flow.lastRST = packet.at
		flow.outIfaces = appendUnique(flow.outIfaces, packet.iface)
		event.kind = expectEventRSTOut
		session.emit(event)
	case !packet.outgoing && rst:
		if flow == nil || flow.completed {
			return
		}
		flow.lastActivity = packet.at
		flow.rstIn++
		event.kind = expectEventRSTIn
		session.emit(event)
	case !packet.outgoing && ack && !syn:
		// handshake의 마지막 ACK는 client의 첫 sequence 다음 번호를 단다. SYN-ACK가 보이지 않아도(IPsec policy로 암호화되어
		// 나간 경우) client가 SYN-ACK를 받았다는 증거다. 연결된 뒤의 ACK는 번호가 달라 세지 않는다.
		if flow == nil || flow.completed || packet.seq != flow.isn+1 {
			return
		}
		flow.lastActivity = packet.at
		flow.completed = true
		event.kind, event.iface = expectEventEstablished, flow.iface
		event.detail = T("expect.live.handshake_time", formatExpectElapsed(packet.at.Sub(flow.firstSYN)))
		session.emit(event)
	}
}

// drop은 kernel이 버린 packet 하나를 연결 시도에 붙인다. drop은 packet socket보다 먼저 읽힐 수 있어 SYN이 아직 없으면
// 잠시 들고 있다.
func (session *expectSession) drop(at time.Time, source, destination, reason, location string) {
	remote, err := netip.ParseAddrPort(source)
	if err != nil {
		return
	}
	local, err := netip.ParseAddrPort(destination)
	if err != nil || local.Port() != session.port {
		return
	}
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.wanted(remote.Addr()) {
		return
	}
	key := expectFlowKey{remote: remote, local: local}
	flow := session.flows[key]
	switch {
	case flow != nil && flow.completed:
		// 연결된 뒤에 버린 packet(연결을 닫은 뒤 늦게 온 FIN 같은)은 연결 시도의 판정과 관계없다. 실행 중에는 -v일 때만 보인다.
		session.otherDrops[reason]++
		if !session.verbose {
			return
		}
	case flow != nil:
		if flow.drops == nil {
			flow.drops = map[string]int{}
		}
		flow.drops[reason]++
	default:
		if session.pendingDrops[key] == nil {
			session.pendingDrops[key] = &expectPendingDrops{at: at, reasons: map[string]int{}}
		}
		session.pendingDrops[key].reasons[reason]++
	}
	detail := reason
	if location != "" {
		detail += " " + location
	}
	event := expectEvent{at: at, kind: expectEventDrop, remote: remote, local: local, detail: detail}
	if flow == nil && !session.verbose {
		// SYN이 따라오면 SYN 줄 뒤에 보인다. 끝까지 SYN이 없으면 관측 전에 시작한 연결의 것이라 실행 중에는 보이지 않는다.
		session.pendingDrops[key].events = append(session.pendingDrops[key].events, event)
		return
	}
	event.iface = "-"
	if flow != nil {
		event.iface = flow.iface
	}
	session.emit(event)
}

// start는 관측을 시작할 때 drop 추적의 상태를 남긴다. --from이 주소 하나면 그 주소로 돌아가는 경로를 먼저 보인다.
func (session *expectSession) start(dropTrace expectDropTrace, now time.Time) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.dropTrace = dropTrace
	if session.from.IsValid() && session.from.IsSingleIP() && session.returnPath != nil {
		route := session.lookupRoute(expectSourceKey{remote: session.from.Addr()})
		session.emit(expectEvent{at: now, kind: expectEventRoute, remote: netip.AddrPortFrom(session.from.Addr(), 0), detail: describeExpectRoute(route, "")})
	}
}

// tick은 packet이 없을 때도 불린다. 도착 interface가 정해진 새 출발지의 돌아가는 경로를 보이고, 오래된 시도를 접는다.
func (session *expectSession) tick(now time.Time) {
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, flow := range session.flows {
		if flow.routeShown || now.Sub(flow.firstSYN) < expectDuplicateWindow {
			continue
		}
		flow.routeShown = true
		key := expectSourceKey{remote: flow.key.remote.Addr(), local: flow.key.local.Addr(), iface: flow.iface}
		// 조회 한 번이 netlink dump 세 번이다. scanner가 출발지를 쏟아내도 packet 읽기가 멈추지 않게 상한을 둔다.
		if _, known := session.routes[key]; known || len(session.routes) >= expectSourceLimit {
			continue
		}
		route := session.lookupRoute(key)
		session.emit(expectEvent{at: now, kind: expectEventRoute, iface: flow.iface, remote: flow.key.remote, local: flow.key.local, detail: describeExpectRoute(route, flow.iface)})
	}
	if now.Sub(session.lastSweep) < expectSweepInterval {
		return
	}
	session.lastSweep = now
	for _, flow := range session.flows {
		if now.Sub(flow.lastActivity) >= expectFlowIdle {
			session.finish(flow)
		}
	}
	for key, pending := range session.pendingDrops {
		// 한동안 SYN이 따라오지 않은 drop은 다른 연결의 것이다.
		if now.Sub(pending.at) < expectSweepInterval {
			continue
		}
		session.releaseDrops(key, pending)
	}
}

// releaseDrops는 SYN이 따라오지 않은 drop을 정리한다. SYN 경로의 이유는 packet socket이 보지 못한 SYN으로 출발지에 남긴다.
func (session *expectSession) releaseDrops(key expectFlowKey, pending *expectPendingDrops) {
	delete(session.pendingDrops, key)
	for reason, count := range pending.reasons {
		if !expectSYNPathDrops[reason] {
			session.otherDrops[reason] += count
			continue
		}
		sourceKey := expectSourceKey{remote: key.remote.Addr(), local: key.local.Addr()}
		source := session.sources[sourceKey]
		if source == nil {
			if len(session.sources) >= session.sourceLimit {
				// 출발지를 더 쥘 수 없으면 수는 버리지 않고 연결 시도 밖의 drop으로 남긴다.
				session.otherDrops[reason] += count
				continue
			}
			source = &expectSource{Address: sourceKey.remote.String(), Local: sourceKey.local.String(), Interface: "-"}
			session.sources[sourceKey] = source
		}
		if source.DropReasons == nil {
			source.DropReasons = map[string]int{}
		}
		source.DropReasons[reason] += count
	}
}

func (session *expectSession) lookupRoute(key expectSourceKey) *expectReturnPath {
	if route, ok := session.routes[key]; ok {
		return route
	}
	if session.returnPath == nil {
		return nil
	}
	route := session.returnPath(key.remote, key.local, key.iface)
	if route.Device != "" && key.iface != "" {
		matches := route.Device == key.iface
		route.MatchesArrival = &matches
	}
	session.routes[key] = &route
	return &route
}

// finish는 연결 시도 하나를 출발지 합계로 접는다.
func (session *expectSession) finish(flow *expectFlow) {
	delete(session.flows, flow.key)
	if pending, ok := session.pendingDrops[flow.key]; ok {
		if flow.drops == nil {
			flow.drops = map[string]int{}
		}
		for reason, count := range pending.reasons {
			flow.drops[reason] += count
		}
		delete(session.pendingDrops, flow.key)
	}
	key := expectSourceKey{remote: flow.key.remote.Addr(), local: flow.key.local.Addr(), iface: flow.iface}
	source := session.sources[key]
	if source == nil {
		if len(session.sources) >= session.sourceLimit {
			session.untracked++
			return
		}
		source = &expectSource{Address: key.remote.String(), Local: key.local.String(), Interface: key.iface}
		session.sources[key] = source
	}
	counts := expectCounts{Attempts: 1, SYNIn: flow.synIn, SYNRetransmits: flow.synRetransmits, SYNACKOut: flow.synAckOut, RSTOut: flow.rstOut, RSTIn: flow.rstIn}
	if flow.synAckOut > 1 {
		counts.SYNACKRetransmits = flow.synAckOut - 1
	}
	if flow.completed {
		counts.Handshakes = 1
	}
	source.expectCounts.add(counts)
	for _, iface := range flow.outIfaces {
		source.OutInterfaces = appendUnique(source.OutInterfaces, iface)
	}
	for reason, count := range flow.drops {
		if source.DropReasons == nil {
			source.DropReasons = map[string]int{}
		}
		source.DropReasons[reason] += count
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// expectVerdict는 출발지 하나의 판정이다. 한 번이라도 연결되면 그 경로는 동작한다.
func expectVerdict(counts expectCounts, drops map[string]int, ipsecHidden bool) string {
	switch {
	case counts.Handshakes > 0:
		return expectVerdictCompleted
	case counts.RSTOut > 0:
		return expectVerdictRST
	case counts.SYNACKOut > 0:
		return expectVerdictNoACK
	case counts.Attempts == 0 && len(drops) > 0:
		return expectVerdictUnseen
	case counts.Attempts == 0:
		return expectVerdictNoSYN
	// socket이 없으면 kernel이 RST로 답한다. IPsec policy가 그 RST를 ESP로 감싸 보이지 않을 뿐이다.
	case ipsecHidden && drops["NO_SOCKET"] > 0:
		return expectVerdictRST
	case ipsecHidden:
		return expectVerdictIPsecHidden
	default:
		return expectVerdictNoReply
	}
}

func expectVerdictStatus(verdict string) Status {
	switch verdict {
	case expectVerdictCompleted:
		return StatusPass
	case expectVerdictIPsecHidden:
		return StatusWarn
	default:
		return StatusFail
	}
}

// expectExitCode는 watch http의 관례를 따른다. SYN이 없으면 관측 시간이 끝났을 때 2, 취소했을 때 4다. SYN이 있으면 모든
// 출발지가 한 번 이상 연결되었을 때만 0이다.
func expectExitCode(sources []expectSource, stopReason string) int {
	if len(sources) == 0 {
		if stopReason == "duration" {
			return 2
		}
		return 4
	}
	for _, source := range sources {
		if source.Verdict != expectVerdictCompleted {
			return 1
		}
	}
	return 0
}

// result는 관측을 끝내고 Result 하나와 exit code를 만든다.
func (session *expectSession) result(started time.Time, stopReason string) (Result, int) {
	session.mu.Lock()
	defer session.mu.Unlock()
	for _, flow := range session.flows {
		session.finish(flow)
	}
	for key, pending := range session.pendingDrops {
		session.releaseDrops(key, pending)
	}
	keys := make([]expectSourceKey, 0, len(session.sources))
	var totals expectCounts
	for key, source := range session.sources {
		keys = append(keys, key)
		totals.add(source.expectCounts)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := session.sources[keys[i]], session.sources[keys[j]]
		if left.Attempts != right.Attempts {
			return left.Attempts > right.Attempts
		}
		return left.Address+left.Interface < right.Address+right.Interface
	})
	sources := make([]expectSource, 0, len(keys))
	for index, key := range keys {
		source := *session.sources[key]
		// 남기는 줄만 경로를 조회한다. 나머지는 IPsec 여부를 모르는 채로 판정한다.
		if index < expectSourceLimit {
			source.Return = session.lookupRoute(key)
		}
		source.Verdict = expectVerdict(source.expectCounts, source.DropReasons, source.Return != nil && source.Return.IPsecPolicy)
		sources = append(sources, source)
	}
	elapsed := time.Since(started)
	verdict, lead := expectOverallVerdict(sources)
	dropTrace := session.dropTrace
	dropTrace.LostEvents = session.dropLost
	metrics := map[string]interface{}{
		"port": int(session.port), "duration_ms": elapsed.Milliseconds(), "stop_reason": stopReason, "verdict": verdict,
		"attempts": totals.Attempts, "syn_in": totals.SYNIn, "syn_retransmits": totals.SYNRetransmits, "syn_ack_out": totals.SYNACKOut,
		"syn_ack_retransmits": totals.SYNACKRetransmits, "rst_out": totals.RSTOut, "rst_in": totals.RSTIn, "handshakes": totals.Handshakes,
		"drop_trace": dropTrace, "forwarded_packets": session.forwarded, "socket_drops": session.socketDrops, "untracked_attempts": session.untracked,
	}
	metrics["observation_status"] = "observed"
	if len(sources) == 0 {
		metrics["observation_status"] = "no_syn"
	}
	if session.from.IsValid() {
		metrics["from"] = session.from.String()
	}
	kept := sources
	if len(kept) > expectSourceLimit {
		kept = kept[:expectSourceLimit]
		metrics["sources_omitted"] = len(sources) - expectSourceLimit
	}
	metrics["sources"] = kept
	if len(session.otherDrops) > 0 {
		metrics["other_drops"] = session.otherDrops
	}
	result := Result{Probe: expectProbeID, Status: expectVerdictStatus(verdict), StartedAt: started.UTC(), DurationMS: elapsed.Milliseconds(), Metrics: metrics}
	var fromRoute *expectReturnPath
	if len(sources) == 0 && session.from.IsValid() && session.from.IsSingleIP() {
		fromRoute = session.lookupRoute(expectSourceKey{remote: session.from.Addr()})
		metrics["from_return"] = fromRoute
	}
	result.Summary = expectSummary(verdict, lead, len(sources), session.port, elapsed)
	result.Warnings = expectWarnings(sources, session.socketDrops, session.dropLost, session.untracked)
	if (verdict == expectVerdictNoSYN || verdict == expectVerdictUnseen) && session.ipsecInbound != nil && session.ipsecInbound(session.from) {
		result.Warnings = append(result.Warnings, T("expect.warning.ipsec_transport"))
		// SYN이 왔어도 보이지 않았을 수 있으므로 오지 않았다고 단정하지 않는다.
		if verdict == expectVerdictNoSYN {
			result.Summary = T("expect.summary.no_syn_ipsec", session.port, formatExpectElapsed(elapsed))
			result.Status = StatusWarn
		}
	}
	result.Evidence = expectEvidence(sources, session.dropTrace, session.otherDrops, fromRoute, session.from)
	switch verdict {
	case expectVerdictRST:
		result.Next = "edc listen"
	case expectVerdictNoReply:
		if session.dropTrace.Status != StatusPass {
			result.Next = "edc trace drop"
		}
	}
	return result, expectExitCode(sources, stopReason)
}

// expectOverallVerdict는 여러 출발지 중 먼저 고칠 판정과 그 출발지다. 모두 연결되면 가장 많이 시도한 출발지다.
func expectOverallVerdict(sources []expectSource) (string, *expectSource) {
	if len(sources) == 0 {
		return expectVerdictNoSYN, nil
	}
	for _, verdict := range expectVerdictOrder {
		for index := range sources {
			if sources[index].Verdict == verdict {
				return verdict, &sources[index]
			}
		}
	}
	return sources[0].Verdict, &sources[0]
}

func expectSummary(verdict string, lead *expectSource, sourceCount int, port uint16, elapsed time.Duration) string {
	if lead == nil {
		return T("expect.summary.no_syn", port, formatExpectElapsed(elapsed))
	}
	var text string
	switch verdict {
	case expectVerdictCompleted:
		text = T("expect.summary.completed", lead.Address, lead.Interface, lead.Handshakes, lead.Attempts)
	case expectVerdictRST:
		text = T("expect.summary.rst", lead.Address, lead.Interface, lead.SYNIn, lead.RSTOut)
		if lead.RSTOut == 0 {
			text = T("expect.summary.rst_hidden", lead.Address, lead.Interface, lead.SYNIn)
		}
	case expectVerdictUnseen:
		text = T("expect.summary.dropped_unseen", lead.Address, port)
	case expectVerdictNoACK:
		text = T("expect.summary.synack_no_ack", lead.Address, lead.Interface, lead.SYNIn, lead.SYNACKOut)
	case expectVerdictIPsecHidden:
		text = T("expect.summary.reply_hidden_ipsec", lead.Address, lead.Interface, lead.SYNIn)
	default:
		text = T("expect.summary.syn_no_reply", lead.Address, lead.Interface, lead.SYNIn)
	}
	if reasons := formatExpectDrops(lead.DropReasons); reasons != "" {
		text += " " + T("expect.summary.drops", reasons)
	}
	if sourceCount > 1 {
		text += " " + T("expect.summary.more_sources", sourceCount-1)
	}
	return text
}

func expectWarnings(sources []expectSource, socketDrops, dropLost uint64, untracked int) []string {
	var warnings []string
	for _, source := range sources {
		route := source.Return
		if route == nil {
			continue
		}
		if route.Error != "" {
			warnings = append(warnings, T("expect.warning.route_failed", source.Address, route.Error))
			continue
		}
		if route.MatchesArrival == nil || *route.MatchesArrival {
			continue
		}
		if route.RPFilter != nil && route.RPFilter.Effective == 1 {
			warnings = append(warnings, T("expect.warning.rp_filter_strict", source.Interface, source.Address, route.Device))
		} else if source.Verdict != expectVerdictCompleted {
			warnings = append(warnings, T("expect.warning.asymmetric", source.Address, route.Device, source.Interface))
		}
	}
	if socketDrops > 0 {
		warnings = append(warnings, T("expect.warning.socket_drops", socketDrops))
	}
	if dropLost > 0 {
		warnings = append(warnings, T("expect.warning.drop_lost", dropLost))
	}
	if untracked > 0 {
		warnings = append(warnings, T("expect.warning.untracked", untracked))
	}
	return warnings
}

func expectEvidence(sources []expectSource, dropTrace expectDropTrace, otherDrops map[string]int, fromRoute *expectReturnPath, from netip.Prefix) []Evidence {
	var evidence []Evidence
	if len(sources) > 0 {
		var table strings.Builder
		fmt.Fprintf(&table, "%-24s %-14s %5s %8s %5s %5s %5s  %s\n", "SOURCE", "INTERFACE", "SYN", "SYN-ACK", "RST", "ACK", "RETX", T("expect.evidence.verdict"))
		for index, source := range sources {
			if index == expectEvidenceLimit {
				fmt.Fprintln(&table, T("expect.evidence.more", len(sources)-expectEvidenceLimit))
				break
			}
			fmt.Fprintf(&table, "%-24s %-14s %5d %8d %5d %5d %5d  %s\n", source.Address, source.Interface, source.SYNIn, source.SYNACKOut, source.RSTOut, source.Handshakes, source.SYNACKRetransmits, T("expect.verdict."+source.Verdict))
		}
		evidence = append(evidence, Evidence{Label: T("expect.evidence.sources"), Value: strings.TrimRight(table.String(), "\n")})
		var routes []string
		for index, source := range sources {
			if index == expectEvidenceLimit {
				break
			}
			if source.Return != nil {
				routes = append(routes, source.Address+": "+describeExpectRoute(source.Return, source.Interface))
			}
		}
		if len(routes) > 0 {
			evidence = append(evidence, Evidence{Label: T("expect.evidence.return"), Value: strings.Join(routes, "\n")})
		}
	} else if fromRoute != nil {
		evidence = append(evidence, Evidence{Label: T("expect.evidence.return"), Value: from.Addr().String() + ": " + describeExpectRoute(fromRoute, "")})
	}
	drops := T("expect.evidence.drop_on")
	if dropTrace.Status != StatusPass {
		drops = T("expect.evidence.drop_skip", dropTrace.Reason)
	}
	for index, source := range sources {
		if index == expectEvidenceLimit {
			break
		}
		if reasons := formatExpectDrops(source.DropReasons); reasons != "" {
			drops += "\n" + source.Address + ": " + reasons
		}
	}
	if other := formatExpectDrops(otherDrops); other != "" {
		drops += "\n" + T("expect.evidence.other_drops", other)
	}
	evidence = append(evidence, Evidence{Label: T("expect.evidence.drops"), Value: drops})
	return evidence
}

// describeExpectRoute는 돌아가는 경로를 한 줄로 쓴다. arrival이 있으면 도착 interface와 같은지 붙인다.
func describeExpectRoute(route *expectReturnPath, arrival string) string {
	if route == nil {
		return T("expect.route.unknown")
	}
	if route.Error != "" {
		return T("expect.route.failed", route.Error)
	}
	parts := []string{T("expect.route.device", route.Device)}
	if route.Via != "" {
		parts = append(parts, T("expect.route.via", route.Via))
	}
	if route.Source != "" {
		parts = append(parts, T("expect.route.source", route.Source))
	}
	if route.Table != "" && route.Table != "main" {
		parts = append(parts, T("expect.route.table", route.Table))
	}
	if route.MatchesArrival != nil {
		if *route.MatchesArrival {
			parts = append(parts, T("expect.route.same_interface"))
		} else {
			parts = append(parts, T("expect.route.other_interface", arrival))
		}
	}
	if route.RPFilter != nil {
		parts = append(parts, fmt.Sprintf("rp_filter %d (%s %d, all %d)", route.RPFilter.Effective, arrival, route.RPFilter.Interface, route.RPFilter.All))
	}
	if route.IPsecPolicy {
		parts = append(parts, T("expect.route.ipsec"))
	}
	return strings.Join(parts, ", ")
}

func formatExpectDrops(drops map[string]int) string {
	if len(drops) == 0 {
		return ""
	}
	names := make([]string, 0, len(drops))
	for name := range drops {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if drops[names[i]] != drops[names[j]] {
			return drops[names[i]] > drops[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d", name, drops[name]))
	}
	return strings.Join(parts, ", ")
}

func formatExpectElapsed(elapsed time.Duration) string {
	if elapsed < time.Second {
		return elapsed.Round(100 * time.Microsecond).String()
	}
	return elapsed.Round(100 * time.Millisecond).String()
}

// expectLivePrinter는 실행 중의 한 줄 출력이다. --json -이면 stdout을 JSON에 비워 두려고 stderr에 쓴다.
type expectLivePrinter struct {
	writer io.Writer
	style  textStyle
	redact bool
}

var expectEventLabels = map[string]string{
	expectEventSYN: "SYN in", expectEventSYNRetransmit: "SYN in", expectEventSYNACK: "SYN-ACK out", expectEventSYNACKRetrans: "SYN-ACK out",
	expectEventRSTOut: "RST out", expectEventRSTIn: "RST in", expectEventEstablished: "ESTABLISHED", expectEventDrop: "DROP",
	expectEventRoute: "ROUTE", expectEventDuplicate: "SYN in",
}

func (printer expectLivePrinter) header(port uint16, from netip.Prefix, duration time.Duration, dropTrace expectDropTrace) {
	style := printer.style
	parts := []string{style.heading(fmt.Sprintf("tcp expect :%d", port))}
	if from.IsValid() {
		parts = append(parts, T("expect.live.from", from.String()))
	}
	if duration > 0 {
		parts = append(parts, duration.String())
	}
	parts = append(parts, style.command("Ctrl-C")+" "+T("expect.live.stop"))
	line := strings.Join(parts, style.dim("  ·  "))
	drops := T("expect.live.drop_on")
	if dropTrace.Status != StatusPass {
		drops = T("expect.live.drop_skip", dropTrace.Reason)
	}
	printer.println(line)
	printer.println(style.dim(drops))
	// 시작한 뒤 SYN이 오기 전에는 화면이 멈춘 것처럼 보인다. 무엇을 기다리는지와 client에서 할 일을 알린다.
	client := "edc tcp check --diagnose " + T("expect.live.this_host") + ":" + strconv.Itoa(int(port))
	printer.println(T("expect.live.waiting", port, style.command(client)))
}

func (printer expectLivePrinter) event(event expectEvent) {
	printer.println(formatExpectEvent(event, printer.style))
}

func (printer expectLivePrinter) println(line string) {
	if printer.redact {
		line = redactIPAddresses(line)
	}
	fmt.Fprintln(printer.writer, line)
}

func formatExpectEvent(event expectEvent, style textStyle) string {
	label := fmt.Sprintf("%-11s", expectEventLabels[event.kind])
	switch event.kind {
	case expectEventRSTOut, expectEventDrop:
		label = style.bad(label)
	case expectEventSYNACKRetrans, expectEventSYNRetransmit, expectEventRSTIn:
		label = style.warn(label)
	case expectEventEstablished:
		label = style.good(label)
	case expectEventRoute, expectEventDuplicate:
		label = style.dim(label)
	}
	iface := event.iface
	if iface == "" {
		iface = "-"
	}
	peers := event.remote.String() + " → " + event.local.String()
	if event.kind == expectEventRoute {
		peers = event.remote.Addr().String()
	}
	parts := []string{style.dim(event.at.Local().Format("15:04:05.000")), label, fmt.Sprintf("%-12s", iface), peers}
	switch event.kind {
	case expectEventSYNRetransmit, expectEventSYNACKRetrans:
		parts = append(parts, style.warn(T("expect.live.retransmit", event.count)))
	case expectEventDuplicate:
		parts = append(parts, style.dim(T("expect.live.duplicate")))
	}
	if event.detail != "" {
		parts = append(parts, style.dim(event.detail))
	}
	return strings.Join(parts, "  ")
}

// expectRun은 운영체제마다 다른 관측 부분이다. ready는 socket을 연 뒤 drop 추적의 상태와 함께 한 번 부른다.
type expectRun struct {
	ctx     context.Context
	port    uint16
	session *expectSession
	ready   func(expectDropTrace)
}

func runTCPExpect(args []string, version string) int {
	options, code, ok := parseTCPExpectArgs(args)
	if !ok {
		return code
	}
	if err := expectPrerequisites(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if options.duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, options.duration)
		defer cancel()
	}
	liveOutput := os.Stdout
	if options.common.jsonPath == "-" {
		liveOutput = os.Stderr
	}
	printer := expectLivePrinter{writer: liveOutput, style: newTextStyle(helpColor(liveOutput)), redact: options.common.redact}
	session := newExpectSession(options.port, options.from, printer.event)
	session.verbose = options.common.verbose
	started := time.Now()
	err := runExpectCapture(expectRun{ctx: ctx, port: options.port, session: session, ready: func(dropTrace expectDropTrace) {
		printer.header(options.port, options.from, options.duration, dropTrace)
		session.start(dropTrace, time.Now())
	}})
	target := map[string]interface{}{"input": strconv.Itoa(int(options.port))}
	if err != nil {
		report := buildReport(version, started, target, []Result{resultFromError(expectProbeID, started, "capture", err)}, options.common.redact)
		emit(options.common, report)
		return 1
	}
	stopReason := "cancelled"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		stopReason = "duration"
	}
	result, exit := session.result(started, stopReason)
	if options.common.jsonPath == "" {
		fmt.Fprintln(os.Stdout)
	}
	if code := emit(options.common, buildReport(version, started, target, []Result{result}, options.common.redact)); code == 2 {
		return 2
	}
	return exit
}
