package edc

import "context"

// neighborEntry는 `ip neigh show <addr>` 한 줄을 담는다.
type neighborEntry struct {
	Dest   string
	Dev    string
	LLAddr string
	State  string
	Raw    string
}

// linkInfo는 `ip -o link show` 한 줄을 담는다. MTU는 터널 오버헤드로 생기는 블랙홀을 진단할 때 쓴다.
type linkInfo struct {
	Name  string
	Flags []string
	MTU   int
	State string
	Raw   string
}

// 도달성 판정 결과. absent는 실패가 아니다. 아직 통신한 적 없는 next-hop은 이웃 항목 자체가 없고
// `ip neigh show`가 빈 출력에 exit 0을 돌려주므로, 없음을 실패로 처리하면 멀쩡한 출구를 거부하게 된다.
const (
	reachUsable  = "usable"
	reachBroken  = "broken"
	reachUnknown = "unknown"
)

// neighborBrokenStates는 lladdr 없이 확정적으로 전달이 실패하는 상태다.
var neighborBrokenStates = map[string]bool{"INCOMPLETE": true, "FAILED": true}

// neighborReach는 next-hop의 L2 도달성을 셋으로 나눈다.
func neighborReach(entries []neighborEntry, dest string) (state string, detail string) {
	for _, entry := range entries {
		if entry.Dest != dest {
			continue
		}
		if neighborBrokenStates[entry.State] {
			return reachBroken, entry.State
		}
		if entry.LLAddr == "" {
			return reachUnknown, entry.State
		}
		return reachUsable, entry.State
	}
	return reachUnknown, ""
}

// linkReach는 장치가 트래픽을 내보낼 수 있는 상태인지 본다. tailscale0나 lo처럼 터널·가상 장치는
// 정상 동작 중에도 state가 UNKNOWN이므로 state가 아니라 flag로 판정한다.
func linkReach(links []linkInfo, name string) (state string, detail string, mtu int) {
	for _, link := range links {
		if link.Name != name {
			continue
		}
		flags := map[string]bool{}
		for _, flag := range link.Flags {
			flags[flag] = true
		}
		switch {
		case !flags["UP"]:
			return reachBroken, "DOWN", link.MTU
		case flags["NO-CARRIER"]:
			return reachBroken, "NO-CARRIER", link.MTU
		default:
			return reachUsable, link.State, link.MTU
		}
	}
	return reachUnknown, "", 0
}

// routeExitReachability는 출구 하나의 도달성을 실제 호스트에서 읽어 판정한다. 명령이 실패하면
// 판정을 못 한 것이므로 unknown으로 두고 전환을 막지 않는다. 막아야 하는 것은 확정 실패뿐이다.
func routeExitReachability(ctx context.Context, deps routeDeps, via, dev string) (state, neighborDetail, linkDetail string, mtu int) {
	if deps.backend == nil {
		return reachUnknown, "", "", 0
	}
	neighbors, err := deps.backend.Neighbors(ctx)
	if err != nil {
		return reachUnknown, "", "", 0
	}
	links, err := deps.backend.Links(ctx)
	if err != nil {
		return reachUnknown, "", "", 0
	}
	return exitReach(neighbors, links, via, dev)
}

// exitReach는 next-hop과 장치를 함께 보고 출구 하나의 도달성을 정한다. 둘 중 하나라도 확정 실패면
// 출구 전체를 실패로 본다.
func exitReach(neighbors []neighborEntry, links []linkInfo, via, dev string) (state, neighborDetail, linkDetail string, mtu int) {
	neighborState, neighborDetail := neighborReach(neighbors, via)
	linkState, linkDetail, mtu := linkReach(links, dev)
	switch {
	case neighborState == reachBroken || linkState == reachBroken:
		return reachBroken, neighborDetail, linkDetail, mtu
	case neighborState == reachUsable && linkState == reachUsable:
		return reachUsable, neighborDetail, linkDetail, mtu
	default:
		return reachUnknown, neighborDetail, linkDetail, mtu
	}
}
