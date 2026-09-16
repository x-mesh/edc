//go:build linux

package edc

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// netlinkBackend는 커널과 직접 말한다. ip 명령의 사람용 출력을 해석하지 않으므로 전사 오류가 생길
// 자리가 없다.
type netlinkBackend struct{}

func (netlinkBackend) Routes(context.Context) ([]routeEntry, error) {
	// RT_TABLE_UNSPEC로 걸러 모든 테이블을 받는다. main만 보면 정책 라우팅을 쓰는 호스트에서
	// 스냅샷이 실제 상태를 놓친다.
	routes, err := netlink.RouteListFiltered(unix.AF_UNSPEC, &netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	names := linkNamesByIndex()
	entries := make([]routeEntry, 0, len(routes))
	for _, route := range routes {
		entries = append(entries, routeEntryFromNetlink(route, names))
	}
	return entries, nil
}

func (netlinkBackend) RouteTo(_ context.Context, address string) (routeGetResult, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return routeGetResult{}, fmt.Errorf("not an IP address: %s", address)
	}
	routes, err := netlink.RouteGet(ip)
	if err != nil {
		return routeGetResult{}, err
	}
	if len(routes) == 0 {
		return routeGetResult{}, fmt.Errorf("no route to %s", address)
	}
	entry := routeEntryFromNetlink(routes[0], linkNamesByIndex())
	result := routeGetResult{
		Dest: address, Type: entry.Type, Via: entry.Via, Dev: entry.Dev,
		Table: entry.Table, Src: entry.Src,
	}
	result.Raw = renderRouteEntry(entry)
	if result.Dev == "" {
		return routeGetResult{}, fmt.Errorf("route to %s carries no device", address)
	}
	return result, nil
}

// Rules는 IPv4와 IPv6 규칙을 각각 묻는다. AF_UNSPEC은 같은 규칙을 중복으로 돌려주는 경우가 있어
// ip rule show가 보여 주는 것과 개수가 어긋난다.
func (netlinkBackend) Rules(context.Context) ([]ipRule, error) {
	var out []ipRule
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		rules, err := netlink.RuleList(family)
		if err != nil {
			return nil, err
		}
		for _, rule := range rules {
			out = append(out, ipRuleFromNetlink(rule, family))
		}
	}
	return out, nil
}

// ipRuleFromNetlink은 규칙 하나를 ip rule show와 같은 모양으로 옮긴다. 규칙의 표기는 경로와 다르다.
// 경로는 main table을 생략하지만 규칙은 언제나 table 이름을 적고, unreachable 같은 액션 규칙은
// table을 가리키지 않는다.
func ipRuleFromNetlink(rule netlink.Rule, family int) ipRule {
	// table 0은 table을 가리키지 않는 액션 규칙(unreachable, prohibit, blackhole)이다. 이 라이브러리의
	// RuleList는 rule.Type을 채우지 않아(쓰기 경로에서만 쓴다) 어떤 액션인지 알 수 없다. lookup 0처럼
	// 틀린 값을 적느니 모른다고 적는다.
	action := "lookup " + ruleTableName(rule.Table)
	if rule.Table == 0 {
		action = "(action)"
	}
	selector := "from all"
	if rule.Src != nil {
		selector = "from " + rule.Src.String()
	}
	if rule.Dst != nil {
		selector += " to " + rule.Dst.String()
	}
	if rule.Mark != 0 {
		if rule.Mask != nil {
			selector += fmt.Sprintf(" fwmark %#x/%#x", rule.Mark, *rule.Mask)
		} else {
			selector += fmt.Sprintf(" fwmark %#x", rule.Mark)
		}
	}
	if rule.IifName != "" {
		selector += " iif " + rule.IifName
	}
	if rule.OifName != "" {
		selector += " oif " + rule.OifName
	}
	if rule.SuppressPrefixlen >= 0 {
		action += fmt.Sprintf(" suppress_prefixlength %d", rule.SuppressPrefixlen)
	}
	prefix := ""
	if family == unix.AF_INET6 {
		// 같은 우선순위가 v4와 v6에 모두 있으므로 스냅샷 비교에서 서로 구분돼야 한다.
		prefix = "[v6] "
	}
	return ipRule{
		Priority: strconv.Itoa(rule.Priority),
		Table:    ruleTableName(rule.Table),
		Not:      rule.Invert,
		Raw:      fmt.Sprintf("%s%d:\t%s %s", prefix, rule.Priority, selector, action),
	}
}

// ruleTableName은 규칙이 가리키는 table 이름이다. 경로와 달리 main도 이름을 적는다.
func ruleTableName(table int) string {
	if table == unix.RT_TABLE_MAIN {
		return "main"
	}
	return routeTableName(table)
}

func (netlinkBackend) Neighbors(context.Context) ([]neighborEntry, error) {
	neighbors, err := netlink.NeighList(0, unix.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	names := linkNamesByIndex()
	out := make([]neighborEntry, 0, len(neighbors))
	for _, neighbor := range neighbors {
		entry := neighborEntry{
			Dest:  neighbor.IP.String(),
			Dev:   names[neighbor.LinkIndex],
			State: neighborStateName(neighbor.State),
		}
		if len(neighbor.HardwareAddr) > 0 {
			entry.LLAddr = neighbor.HardwareAddr.String()
		}
		entry.Raw = entry.Dest + " dev " + entry.Dev + " " + entry.State
		out = append(out, entry)
	}
	return out, nil
}

func (netlinkBackend) Links(context.Context) ([]linkInfo, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	out := make([]linkInfo, 0, len(links))
	for _, link := range links {
		attrs := link.Attrs()
		out = append(out, linkInfo{
			Name:  attrs.Name,
			Flags: linkFlagNames(attrs.RawFlags),
			MTU:   attrs.MTU,
			State: attrs.OperState.String(),
			Raw:   fmt.Sprintf("%s mtu %d state %s", attrs.Name, attrs.MTU, attrs.OperState),
		})
	}
	return out, nil
}

// linkNamesByIndex는 경로와 이웃이 들고 있는 장치 index를 이름으로 바꾸는 표를 만든다.
func linkNamesByIndex() map[int]string {
	names := map[int]string{}
	links, err := netlink.LinkList()
	if err != nil {
		return names
	}
	for _, link := range links {
		attrs := link.Attrs()
		names[attrs.Index] = attrs.Name
	}
	return names
}

// routeEntryFromNetlink은 netlink.Route를 이 패키지가 쓰는 routeEntry로 옮긴다. 판정 로직과 테스트가
// 그대로 남도록 기존 타입을 유지한다.
func routeEntryFromNetlink(route netlink.Route, names map[int]string) routeEntry {
	entry := routeEntry{
		Dest:   routeDestName(route.Dst),
		Type:   routeTypeName(route.Type),
		Dev:    names[route.LinkIndex],
		Table:  routeTableName(route.Table),
		Metric: strconv.Itoa(route.Priority),
		// ip route show는 metric 0을 생략한다. 커널에서 priority 0과 metric 없음은 같은 상태다.
		HasMetric: route.Priority != 0,
		Tos:       route.Tos,
		Scope:     routeScopeName(route.Scope),
	}
	if route.Gw != nil {
		entry.Via = route.Gw.String()
	}
	if route.Src != nil {
		entry.Src = route.Src.String()
	}
	if proto := route.Protocol.String(); proto != "boot" && proto != "unspec" {
		entry.Proto = proto
	}
	entry.Raw = renderRouteEntry(entry)
	return entry
}

// routeScopeName은 scope를 ip route show와 같은 이름으로 바꾼다. global(universe)은 생략한다.
func routeScopeName(scope netlink.Scope) string {
	switch int(scope) {
	case unix.RT_SCOPE_HOST:
		return "host"
	case unix.RT_SCOPE_LINK:
		return "link"
	case unix.RT_SCOPE_SITE:
		return "site"
	case unix.RT_SCOPE_NOWHERE:
		return "nowhere"
	default:
		return ""
	}
}

func routeTableName(table int) string {
	switch table {
	case unix.RT_TABLE_MAIN:
		// main 테이블 경로는 ip route show에서 table 토큰 없이 나온다. 같은 표기를 유지한다.
		return ""
	case unix.RT_TABLE_LOCAL:
		return "local"
	case unix.RT_TABLE_DEFAULT:
		return "default"
	default:
		return strconv.Itoa(table)
	}
}

// routeTypeName은 경로 타입을 문자열로 바꾼다. 타입은 metric과 마찬가지로 경로의 정체성 일부다.
func routeTypeName(routeType int) string {
	switch routeType {
	case unix.RTN_LOCAL:
		return "local"
	case unix.RTN_BROADCAST:
		return "broadcast"
	case unix.RTN_ANYCAST:
		return "anycast"
	case unix.RTN_MULTICAST:
		return "multicast"
	case unix.RTN_BLACKHOLE:
		return "blackhole"
	case unix.RTN_UNREACHABLE:
		return "unreachable"
	case unix.RTN_PROHIBIT:
		return "prohibit"
	case unix.RTN_THROW:
		return "throw"
	case unix.RTN_NAT:
		return "nat"
	default:
		return ""
	}
}

// neighborStateName은 NUD 비트를 ip neigh show와 같은 이름으로 바꾼다. 이름이 같아야 도달성 분류가
// 그대로 동작한다.
func neighborStateName(state int) string {
	switch {
	case state&unix.NUD_PERMANENT != 0:
		return "PERMANENT"
	case state&unix.NUD_NOARP != 0:
		return "NOARP"
	case state&unix.NUD_REACHABLE != 0:
		return "REACHABLE"
	case state&unix.NUD_STALE != 0:
		return "STALE"
	case state&unix.NUD_DELAY != 0:
		return "DELAY"
	case state&unix.NUD_PROBE != 0:
		return "PROBE"
	case state&unix.NUD_FAILED != 0:
		return "FAILED"
	case state&unix.NUD_INCOMPLETE != 0:
		return "INCOMPLETE"
	default:
		return "NONE"
	}
}

// linkFlagNames는 IFF_ 비트를 ip link show가 찍는 이름으로 바꾼다. NO-CARRIER는 별도 비트가 아니라
// UP인데 LOWER_UP이 아닌 상태다.
func linkFlagNames(raw uint32) []string {
	var flags []string
	if raw&unix.IFF_UP != 0 && raw&unix.IFF_LOWER_UP == 0 {
		flags = append(flags, "NO-CARRIER")
	}
	for _, pair := range []struct {
		bit  uint32
		name string
	}{
		{unix.IFF_BROADCAST, "BROADCAST"},
		{unix.IFF_MULTICAST, "MULTICAST"},
		{unix.IFF_LOOPBACK, "LOOPBACK"},
		{unix.IFF_POINTOPOINT, "POINTOPOINT"},
		{unix.IFF_NOARP, "NOARP"},
		{unix.IFF_UP, "UP"},
		{unix.IFF_LOWER_UP, "LOWER_UP"},
	} {
		if raw&pair.bit != 0 {
			flags = append(flags, pair.name)
		}
	}
	return flags
}

func newRouteBackend() (routeBackend, bool) { return netlinkBackend{}, true }

// netlinkRouteFrom은 routeEntry를 netlink.Route로 되돌린다. rollback은 별도 프로세스라 JSON 스냅샷만
// 보고 경로를 재구성해야 하므로, 이 변환이 키 필드를 하나도 잃지 않아야 한다.
func netlinkRouteFrom(entry routeEntry, newVia string) (*netlink.Route, error) {
	route := &netlink.Route{
		Table: routeTableValue(entry.Table),
		Type:  routeTypeValue(entry.Type),
		Tos:   entry.Tos,
		Scope: netlink.Scope(routeScopeValue(entry.Scope)),
	}
	if entry.Dest != "default" {
		dst, err := parseRouteDest(entry.Dest)
		if err != nil {
			return nil, err
		}
		route.Dst = dst
	}
	via := newVia
	if via == "" {
		via = entry.Via
	}
	if via != "" {
		if route.Gw = net.ParseIP(via); route.Gw == nil {
			return nil, fmt.Errorf("not an IP address: %s", via)
		}
	}
	if entry.Dev != "" {
		link, err := netlink.LinkByName(entry.Dev)
		if err != nil {
			return nil, fmt.Errorf("device %s: %w", entry.Dev, err)
		}
		route.LinkIndex = link.Attrs().Index
	}
	if entry.Src != "" {
		route.Src = net.ParseIP(entry.Src)
	}
	if entry.HasMetric {
		priority, err := strconv.Atoi(entry.Metric)
		if err != nil {
			return nil, fmt.Errorf("metric %q: %w", entry.Metric, err)
		}
		route.Priority = priority
	}
	route.Protocol = netlink.RouteProtocol(routeProtocolValue(entry.Proto))
	return route, nil
}

func (netlinkBackend) ReplaceRoute(_ context.Context, entry routeEntry, newVia string) error {
	route, err := netlinkRouteFrom(entry, newVia)
	if err != nil {
		return err
	}
	return netlink.RouteReplace(route)
}

func (netlinkBackend) DeleteRoute(_ context.Context, entry routeEntry) error {
	route, err := netlinkRouteFrom(entry, "")
	if err != nil {
		return err
	}
	return netlink.RouteDel(route)
}

func routeTableValue(name string) int {
	switch name {
	case "", "main":
		return unix.RT_TABLE_MAIN
	case "local":
		return unix.RT_TABLE_LOCAL
	case "default":
		return unix.RT_TABLE_DEFAULT
	default:
		value, err := strconv.Atoi(name)
		if err != nil {
			return unix.RT_TABLE_MAIN
		}
		return value
	}
}

func routeTypeValue(name string) int {
	switch name {
	case "local":
		return unix.RTN_LOCAL
	case "broadcast":
		return unix.RTN_BROADCAST
	case "anycast":
		return unix.RTN_ANYCAST
	case "multicast":
		return unix.RTN_MULTICAST
	case "blackhole":
		return unix.RTN_BLACKHOLE
	case "unreachable":
		return unix.RTN_UNREACHABLE
	case "prohibit":
		return unix.RTN_PROHIBIT
	case "throw":
		return unix.RTN_THROW
	case "nat":
		return unix.RTN_NAT
	default:
		return unix.RTN_UNICAST
	}
}

func routeScopeValue(name string) int {
	switch name {
	case "host":
		return unix.RT_SCOPE_HOST
	case "link":
		return unix.RT_SCOPE_LINK
	case "site":
		return unix.RT_SCOPE_SITE
	case "nowhere":
		return unix.RT_SCOPE_NOWHERE
	default:
		return unix.RT_SCOPE_UNIVERSE
	}
}

func routeProtocolValue(name string) int {
	switch name {
	case "kernel":
		return unix.RTPROT_KERNEL
	case "dhcp":
		return unix.RTPROT_DHCP
	case "static":
		return unix.RTPROT_STATIC
	case "ra":
		return unix.RTPROT_RA
	case "bgp":
		return unix.RTPROT_BGP
	case "ospf":
		return unix.RTPROT_OSPF
	default:
		return unix.RTPROT_BOOT
	}
}
