package edc

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// checkCounterInterval은 drop 카운터를 두 번 읽는 간격이다. 부팅 뒤 누적된 값은 한 번 읽어서는 지금 늘고 있는지 알 수 없다.
const checkCounterInterval = time.Second

// checkCounterMargin은 두 번째로 읽은 뒤 결과를 넘기는 데 남겨 두는 시간이다. collectCheckFacts는 deadline이 지나면 늦게 온
// 원천을 버리므로, 기다리다 deadline에 걸리면 route와 DNS 판정까지 잃는다.
const checkCounterMargin = 500 * time.Millisecond

// checkDropCounters는 늘었다면 패킷이나 연결을 버린 것인 카운터다. 재전송, RST, 연결 시도 실패는 정상 host에서도 늘어 뺀다.
var checkDropCounters = []string{
	"listen_overflows", "listen_drops", "softnet_dropped",
	"nic_rx_missed", "nic_rx_fifo", "nic_tx_fifo",
	"conntrack_drop", "conntrack_early_drop", "conntrack_insert_failed",
	"udp_rcvbuf_errors", "tcp_memory_pressures", "tcp_abort_on_memory",
}

// checkLinkDownStates는 traffic이 나갈 수 없는 operstate다. tun, WireGuard, Tailscale 같은 가상 interface는 상태를 알리지
// 않아 unknown이므로 여기에 넣지 않는다.
var checkLinkDownStates = []string{"down", "lowerlayerdown", "notpresent"}

// checkNetworkLink는 default route가 나가는 interface의 link 상태다. State는 Linux operstate 이름이고 macOS도 같은 이름으로
// 옮긴다. Carrier는 "1"이나 "0"이고, 읽지 못하면 비어 있다. admin down인 interface의 carrier 파일은 EINVAL을 낸다.
type checkNetworkLink struct {
	State, Carrier string
}

func (link checkNetworkLink) down() bool {
	if link.State == "" || link.State == "unknown" {
		return false
	}
	return slices.Contains(checkLinkDownStates, link.State) || link.Carrier == "0"
}

func (link checkNetworkLink) detail() string {
	if link.Carrier == "0" {
		return "no carrier"
	}
	return link.State
}

// checkRouteParts는 "gateway dev interface"나 "dev interface" 모양의 route에서 gateway와 interface를 꺼낸다.
func checkRouteParts(route string) (string, string) {
	gateway, iface, found := strings.Cut(" "+route, " dev ")
	if !found {
		return "", ""
	}
	return strings.TrimSpace(gateway), strings.TrimSpace(iface)
}

// checkGatewayAddress는 neighbor table에서 찾을 gateway 주소다. point-to-point route는 gateway가 없거나 0.0.0.0이고,
// macOS는 link#N을 적는다. 그런 route는 neighbor 항목이 없어 찾지 않는다. link-local 주소의 zone은 table에 없어 뗀다.
func checkGatewayAddress(gateway string) (netip.Addr, bool) {
	gateway, _, _ = strings.Cut(gateway, "%")
	address, err := netip.ParseAddr(gateway)
	if err != nil || address.IsUnspecified() {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}

// checkNeighborProblem은 gateway의 MAC 주소를 확인하지 못한 상태다. 항목이 없으면 아직 보낸 적이 없는 것이라 문제가 아니다.
func checkNeighborProblem(state string) bool {
	return state == traceNeighborFailedState || state == "INCOMPLETE"
}

// waitCheckCounterSample은 sampledAt에서 interval이 지날 때까지 기다린다. 두 번째로 읽고 결과를 넘기기 전에 deadline이
// 오면 기다리지 않고 false를 돌려준다. 그때 카운터는 판정하지 않는다.
func waitCheckCounterSample(ctx context.Context, sampledAt time.Time, interval time.Duration) bool {
	due := sampledAt.Add(interval)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(due.Add(checkCounterMargin)) {
		return false
	}
	timer := time.NewTimer(time.Until(due))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// checkCounterGrowth는 두 번 읽은 사이에 늘어난 drop 카운터를 "이름 +증가량"으로 적는다. 값이 줄었으면 카운터가 초기화된
// 것이라 늘었다고 보지 않는다. NIC 카운터는 두 번 모두 있던 interface만 더한다. 그 사이 생긴 veth의 값이 증가로 보이거나
// 사라진 interface 때문에 합이 줄어 초기화로 보이지 않게 하려는 것이다.
func checkCounterGrowth(previous, current *networkHealth) []string {
	if previous == nil || current == nil || previous.Scope != current.Scope {
		return nil
	}
	both := func(iface string) bool {
		_, before := previous.NICInterfaces[iface]
		_, now := current.NICInterfaces[iface]
		return before && now
	}
	beforeNIC := sumNetworkNICCounters(previous.NICInterfaces, both)
	nowNIC := sumNetworkNICCounters(current.NICInterfaces, both)
	var grown []string
	for _, name := range checkDropCounters {
		before, now := previous.Counters[name], current.Counters[name]
		if strings.HasPrefix(name, "nic_") {
			before, now = beforeNIC[name], nowNIC[name]
		}
		if before.Value != nil && now.Value != nil && *now.Value > *before.Value {
			grown = append(grown, fmt.Sprintf("%s +%d", name, *now.Value-*before.Value))
		}
	}
	return grown
}

func checkCounterNext(grown string) string {
	switch {
	case strings.HasPrefix(grown, "listen_"):
		return "edc listen"
	case strings.HasPrefix(grown, "nic_"):
		return "edc net interfaces"
	}
	return "edc top"
}
