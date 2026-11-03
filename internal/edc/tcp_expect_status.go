package edc

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/net/bpf"
)

const (
	// expectStatusPorts는 상태줄에 보이는 다른 port의 수이고, expectMetricPorts는 metrics에 남기는 수다.
	expectStatusPorts = 3
	expectMetricPorts = 5
)

// expectLiveCounts는 상태줄이 보이는 실행 중 합계다. 출발지 합계는 연결 시도를 접을 때 만들어지므로 따로 센다.
type expectLiveCounts struct {
	SYN        int
	Handshakes int
	Drops      int
	// OtherPorts는 다른 port로 온 SYN 중 많은 순서의 앞부분이다. 다른 port를 세지 않으면 nil이다.
	OtherPorts []expectPortCount
}

type expectPortCount struct {
	Port  uint16
	Count int
}

// topExpectPorts는 SYN이 많이 온 port부터 limit개다. 수가 같으면 port 번호 순서다.
func topExpectPorts(counts map[uint16]int, limit int) []expectPortCount {
	ports := make([]expectPortCount, 0, len(counts))
	for port, count := range counts {
		ports = append(ports, expectPortCount{Port: port, Count: count})
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Count != ports[j].Count {
			return ports[i].Count > ports[j].Count
		}
		return ports[i].Port < ports[j].Port
	})
	if len(ports) > limit {
		ports = ports[:limit]
	}
	return ports
}

func formatExpectPorts(ports []expectPortCount) string {
	parts := make([]string, 0, len(ports))
	for _, port := range ports {
		parts = append(parts, fmt.Sprintf("%d×%d", port.Port, port.Count))
	}
	return strings.Join(parts, ", ")
}

// expectPortMetric은 metrics의 other_port_syns다. JSON object의 key는 문자열이라 port 번호를 문자열로 쓴다.
func expectPortMetric(ports []expectPortCount) map[string]int {
	metric := make(map[string]int, len(ports))
	for _, port := range ports {
		metric[strconv.Itoa(int(port.Port))] = port.Count
	}
	return metric
}

// expectOtherSYNFilter는 이 port가 아닌 TCP port로 오는 SYN(ACK 없음)만 남기는 classic BPF다. 이 port의 packet을 받는
// socket과 따로 연다. 다른 port의 SYN이 쏟아져 receive buffer가 차도 이 port의 packet을 잃지 않게 하려는 것이다. 결과는 IP와
// TCP header의 앞부분이다.
func expectOtherSYNFilter(port uint16) ([]bpf.RawInstruction, error) {
	steps := []expectFilterStep{
		{ins: bpf.LoadAbsolute{Off: 0, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpShiftRight, Val: 4}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 4}, no: "v6"},
		{ins: bpf.LoadAbsolute{Off: 9, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 6, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x1fff}, yes: "drop"},
		{ins: bpf.LoadMemShift{Off: 0}},
		{ins: bpf.LoadIndirect{Off: 13, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: tcpFlagSYN | tcpFlagACK}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: tcpFlagSYN}, no: "drop"},
		{ins: bpf.LoadIndirect{Off: 2, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, yes: "drop", no: "accept"},
		{label: "v6", ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 6, Size: 1}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 6}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 53, Size: 1}},
		{ins: bpf.ALUOpConstant{Op: bpf.ALUOpAnd, Val: tcpFlagSYN | tcpFlagACK}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: tcpFlagSYN}, no: "drop"},
		{ins: bpf.LoadAbsolute{Off: 42, Size: 2}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(port)}, yes: "drop", no: "accept"},
		{label: "accept", ins: bpf.RetConstant{Val: expectSnapLength}},
		{label: "drop", ins: bpf.RetConstant{Val: 0}},
	}
	return assembleExpectFilter(steps)
}

// parseExpectOtherSYN은 다른 port로 들어온 SYN의 출발지, 목적지와 목적지 port를 읽는다. filter를 붙이기 전에 들어온 packet도
// 있으므로 여기서 다시 확인한다.
func parseExpectOtherSYN(data []byte, port uint16) (remote, local netip.Addr, destinationPort uint16, ok bool) {
	if len(data) < 1 {
		return remote, local, 0, false
	}
	var tcp []byte
	switch data[0] >> 4 {
	case 4:
		headerLength := int(data[0]&0x0f) * 4
		if headerLength < 20 || len(data) < headerLength+14 || data[9] != 6 || binary.BigEndian.Uint16(data[6:8])&0x1fff != 0 {
			return remote, local, 0, false
		}
		remote, local = netip.AddrFrom4([4]byte(data[12:16])), netip.AddrFrom4([4]byte(data[16:20]))
		tcp = data[headerLength:]
	case 6:
		if len(data) < 40+14 || data[6] != 6 {
			return remote, local, 0, false
		}
		remote, local = netip.AddrFrom16([16]byte(data[8:24])).Unmap(), netip.AddrFrom16([16]byte(data[24:40])).Unmap()
		tcp = data[40:]
	default:
		return remote, local, 0, false
	}
	destinationPort = binary.BigEndian.Uint16(tcp[2:4])
	if tcp[13]&(tcpFlagSYN|tcpFlagACK) != tcpFlagSYN || destinationPort == port {
		return remote, local, 0, false
	}
	return remote, local, destinationPort, true
}

// expectStatus는 터미널 맨 아래 한 줄에 관측 합계를 그린다. edc watch fs의 상태줄과 같은 방식이다. 실행 중 줄을 쓸 때마다
// 상태줄을 지우고 쓴 뒤 다시 그린다. 상태줄의 값은 1초마다 update로 받는다. session의 잠금 아래에서 줄을 쓰므로, 여기서
// session을 다시 읽으면 잠금 순서가 엇갈린다.
type expectStatus struct {
	mutex   sync.Mutex
	out     io.Writer
	width   func() int
	now     func() time.Time
	started time.Time
	counts  expectLiveCounts
	shown   bool
	closed  bool
	// color가 false면 NO_COLOR다. 상태줄을 회색 대신 흐린 글자로 그린다.
	color bool
}

func newExpectStatus(out io.Writer, width func() int, now func() time.Time) *expectStatus {
	return &expectStatus{out: out, width: width, now: now, started: now()}
}

func (status *expectStatus) Write(data []byte) (int, error) {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.clear()
	written, err := status.out.Write(data)
	if err == nil {
		status.draw()
	}
	return written, err
}

func (status *expectStatus) update(counts expectLiveCounts) {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.counts = counts
	status.clear()
	status.draw()
}

// close는 상태줄을 지운다. 이후에는 다시 그리지 않으므로 결과가 상태줄 없이 남는다.
func (status *expectStatus) close() {
	status.mutex.Lock()
	defer status.mutex.Unlock()
	status.clear()
	status.closed = true
}

func (status *expectStatus) clear() {
	if status.shown {
		io.WriteString(status.out, "\r\x1b[2K")
		status.shown = false
	}
}

func (status *expectStatus) draw() {
	if status.closed {
		return
	}
	// 마지막 열에 쓰면 일부 터미널이 줄을 넘기므로 한 칸을 비운다.
	width := max(1, status.width()-1)
	line := ansi.Truncate(status.line(width), width, "…")
	if status.color {
		io.WriteString(status.out, richDim(line))
	} else {
		io.WriteString(status.out, richSGR(line, "2"))
	}
	status.shown = true
}

// line은 width 안에 들어가게 상태줄을 만든다. 넘치면 다른 port를 뺀다.
func (status *expectStatus) line(width int) string {
	counts := status.counts
	head := []string{
		"─ " + fsWatchElapsed(status.now().Sub(status.started)),
		T("expect.status.syn", counts.SYN), T("expect.status.handshakes", counts.Handshakes), T("expect.status.drops", counts.Drops),
	}
	text := strings.Join(head, " · ")
	if len(counts.OtherPorts) == 0 {
		return text
	}
	full := text + " · " + T("expect.status.other_ports", formatExpectPorts(counts.OtherPorts))
	if ansi.StringWidth(full) <= width {
		return full
	}
	return text
}
