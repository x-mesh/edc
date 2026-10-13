package edc

import (
	"fmt"
	"maps"
	"net"
	"path"
	"slices"
	"strings"
)

// topInterfaceFilter는 --interface와 defaults.top.interface의 값이다. 비었거나 all이면 loopback을 뺀 모든 interface를 더한다.
// 이름, glob, physical을 쉼표로 이어 쓰면 그중 하나에라도 맞는 interface만 더한다.
type topInterfaceFilter struct {
	text     string
	names    []string
	patterns []string
	physical bool
}

func parseTopInterfaceFilter(value string) (topInterfaceFilter, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "all" {
		return topInterfaceFilter{}, nil
	}
	filter := topInterfaceFilter{text: value}
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		item := strings.TrimSpace(part)
		switch {
		case item == "":
			return topInterfaceFilter{}, fmt.Errorf("empty name")
		case item == "all":
			return topInterfaceFilter{}, fmt.Errorf("all cannot be combined with other names")
		case seen[item]:
			return topInterfaceFilter{}, fmt.Errorf("duplicate name %q", item)
		case item == "physical":
			filter.physical = true
		case strings.ContainsAny(item, "*?["):
			if _, err := path.Match(item, ""); err != nil {
				return topInterfaceFilter{}, fmt.Errorf("bad pattern %q", item)
			}
			filter.patterns = append(filter.patterns, item)
		default:
			filter.names = append(filter.names, item)
		}
		seen[item] = true
	}
	return filter, nil
}

func (filter topInterfaceFilter) active() bool { return filter.text != "" }

func (filter topInterfaceFilter) String() string { return filter.text }

// matches는 physical을 마지막에 본다. physical은 interface마다 sysfs를 읽으므로 이름과 glob으로 정해지면 읽지 않는다.
func (filter topInterfaceFilter) matches(name string, physical func(string) bool) bool {
	if !filter.active() || slices.Contains(filter.names, name) {
		return true
	}
	for _, pattern := range filter.patterns {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return filter.physical && physical(name)
}

// withoutPhysical은 physical을 판정할 수 없는 OS에서 쓴다. 공유한 설정 파일의 physical 때문에 top이 실행되지 않으면 안 되므로
// 경고를 내고 모든 interface를 더한다.
func (filter topInterfaceFilter) withoutPhysical() topInterfaceFilter {
	if filter.physical {
		return topInterfaceFilter{}
	}
	return filter
}

// missingNames는 지금 없는 이름이다. 이름은 사용자가 직접 적었으므로 오타를 조용히 0으로 보이지 않는다.
func (filter topInterfaceFilter) missingNames(present []string) []string {
	var missing []string
	for _, name := range filter.names {
		if !slices.Contains(present, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

func (filter topInterfaceFilter) matchesAny(present []string, physical func(string) bool) bool {
	for _, name := range present {
		if filter.matches(name, physical) {
			return true
		}
	}
	return false
}

// apply는 snapshot에서 고르지 않은 interface를 빼고, 장치 쪽 drop도 고른 interface만 다시 더한다. 고른 interface가 하나도
// 없으면 error와 drop을 알 수 없음으로 둔다. 0으로 두면 없는 interface가 오류 없이 도는 것처럼 읽힌다.
func (filter topInterfaceFilter) apply(snapshot resourceSnapshot, physical func(string) bool) resourceSnapshot {
	if !filter.active() {
		return snapshot
	}
	keep := func(name string) bool { return filter.matches(name, physical) }
	var kept []netInterfaceCounters
	for _, item := range snapshot.NetInterfaces {
		if keep(item.Name) {
			kept = append(kept, item)
		}
	}
	snapshot.NetInterfaces = kept
	if len(kept) == 0 {
		snapshot.NetHealthValid = false
	}
	if health := snapshot.NetworkHealth; health != nil && health.NICInterfaces != nil {
		selected := *health
		selected.Counters = maps.Clone(health.Counters)
		maps.Copy(selected.Counters, sumNetworkNICCounters(health.NICInterfaces, keep))
		snapshot.NetworkHealth = &selected
	}
	return snapshot
}

// topPresentInterfaces는 loopback이 아닌 interface의 이름이다. 수집을 시작하기 전에 --interface를 확인할 때 쓴다.
func topPresentInterfaces() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			names = append(names, iface.Name)
		}
	}
	return names, nil
}
