package edc

// 이 파일의 파서는 프로덕션에서 쓰지 않는다. 커널 상태는 netlink에서 타입 있는 값으로 읽으므로
// ip 출력을 해석할 일이 없다.
//
// 그래도 지우지 않는 이유는 테스트 입력의 출처 때문이다. 가짜 백엔드는 실제 호스트에서 캡처한
// 텍스트 fixture를 이 파서로 변환해 값을 만든다. 이 프로젝트에서 발견된 route 결함은 전부 상상한
// fixture가 실제 데이터와 달라서 생겼고, 구조체를 손으로 지어내면 그 함정으로 되돌아간다.

import (
	"fmt"
	"strconv"
	"strings"
)

// parseIPRule은 `ip rule show` 출력을 우선순위와 lookup 대상으로 나눈다.
// parseRouteGet은 `ip route get <address>` 첫 줄을 읽는다. cache 등 이어지는 줄은 보지 않는다.
// parseRouteTable은 `ip route show table all` 출력을 routeEntry 목록으로 바꾼다.
// dest나 dev를 읽지 못한 줄은 값을 추측하지 않고 parseErrors에 원문을 남긴다.
// routeTypeTokens는 목적지 앞에 올 수 있는 경로 타입이다. 타입은 metric과 마찬가지로 경로의 정체성
// 일부라, 복원할 때 빠뜨리면 같은 목적지의 다른 경로가 된다.
// routeFlagOnlyTokens는 값을 갖지 않는 토큰이다. 이 목록에 없는 낯선 토큰은 key-value 쌍으로 본다.
var routeFlagOnlyTokens = map[string]bool{
	"linkdown": true, "onlink": true, "offload": true, "notify": true,
	"static": true, "pervasive": true, "dead": true,
}

var routeTypeTokens = map[string]bool{
	"unicast": true, "local": true, "broadcast": true, "multicast": true,
	"throw": true, "unreachable": true, "prohibit": true, "blackhole": true,
	"nat": true, "anycast": true,
}

func parseRouteTable(text string) ([]routeEntry, []string) {
	var entries []routeEntry
	var parseErrors []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		tokens := strings.Fields(trimmed)
		entry := routeEntry{Raw: line}
		index := 0
		if routeTypeTokens[tokens[0]] && len(tokens) > 1 {
			entry.Type = tokens[0]
			index = 1
		}
		entry.Dest = tokens[index]
		index++
		for index < len(tokens) {
			key := tokens[index]
			if routeFlagOnlyTokens[key] {
				index++
				continue
			}
			if index+1 >= len(tokens) {
				break
			}
			value := tokens[index+1]
			switch key {
			case "via":
				entry.Via = value
			case "dev":
				entry.Dev = value
			case "proto":
				entry.Proto = value
			case "src":
				entry.Src = value
			case "metric":
				entry.Metric = value
				entry.HasMetric = true
			case "table":
				entry.Table = value
			}
			index += 2
		}
		if entry.Dev == "" && !routeTypesWithoutDev[entry.Type] {
			parseErrors = append(parseErrors, line)
			continue
		}
		entries = append(entries, entry)
	}
	return entries, parseErrors
}

func parseRouteGet(text string) (routeGetResult, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return routeGetResult{}, fmt.Errorf("empty route get output")
	}
	result := routeGetResult{Raw: text}
	index := 0
	// 자기 자신을 향한 ip route get은 `local <ip> dev lo ...`처럼 타입이 앞에 붙는다.
	if routeTypeTokens[tokens[0]] && len(tokens) > 1 {
		result.Type = tokens[0]
		index = 1
	}
	result.Dest = tokens[index]
	index++
	for index+1 < len(tokens) {
		key, value := tokens[index], tokens[index+1]
		switch key {
		case "via":
			result.Via = value
		case "dev":
			result.Dev = value
		case "table":
			result.Table = value
		case "src":
			result.Src = value
		}
		index += 2
	}
	if result.Dev == "" {
		return routeGetResult{}, fmt.Errorf("route get output carries no dev: %q", line)
	}
	return result, nil
}

func parseIPRule(text string) ([]ipRule, error) {
	var rules []ipRule
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		priority, rest, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, fmt.Errorf("ip rule line carries no priority: %q", line)
		}
		rule := ipRule{Priority: strings.TrimSpace(priority), Raw: line}
		tokens := strings.Fields(strings.TrimSpace(rest))
		for index, token := range tokens {
			switch token {
			case "not":
				rule.Not = true
			case "lookup":
				if index+1 < len(tokens) {
					rule.Table = tokens[index+1]
				}
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// parseLinks는 `ip -o link show` 출력을 장치 목록으로 바꾼다. -o는 한 줄에 한 장치를 담지만 link/ether
// 뒤쪽은 백슬래시로 이어 붙으므로 앞쪽만 읽는다.
// parseNeighbors는 `ip neigh show` 출력을 항목 목록으로 바꾼다.
func parseNeighbors(text string) []neighborEntry {
	var entries []neighborEntry
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		tokens := strings.Fields(trimmed)
		entry := neighborEntry{Dest: tokens[0], Raw: line}
		for index := 1; index < len(tokens); index++ {
			switch tokens[index] {
			case "dev":
				if index+1 < len(tokens) {
					entry.Dev = tokens[index+1]
					index++
				}
			case "lladdr":
				if index+1 < len(tokens) {
					entry.LLAddr = tokens[index+1]
					index++
				}
			default:
				// 상태는 값 없이 마지막에 대문자로 온다.
				if tokens[index] == strings.ToUpper(tokens[index]) {
					entry.State = tokens[index]
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func parseLinks(text string) []linkInfo {
	var links []linkInfo
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		head, _, _ := strings.Cut(trimmed, "\\")
		tokens := strings.Fields(head)
		if len(tokens) < 3 {
			continue
		}
		info := linkInfo{Raw: line}
		// "2: enp1s0: <BROADCAST,UP,LOWER_UP> mtu 1500 ... state UP ..."
		info.Name = strings.TrimSuffix(tokens[1], ":")
		if open := strings.Index(head, "<"); open >= 0 {
			if close := strings.Index(head[open:], ">"); close > 0 {
				info.Flags = strings.Split(head[open+1:open+close], ",")
			}
		}
		for index := 0; index+1 < len(tokens); index++ {
			switch tokens[index] {
			case "mtu":
				if value, err := strconv.Atoi(tokens[index+1]); err == nil {
					info.MTU = value
				}
			case "state":
				info.State = tokens[index+1]
			}
		}
		links = append(links, info)
	}
	return links
}
