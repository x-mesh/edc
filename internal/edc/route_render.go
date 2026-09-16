package edc

import "strings"

// renderRouteEntry는 routeEntry를 `ip route show` 형태의 한 줄로 되돌린다. netlink에서 읽으면 원문
// 텍스트가 없어지는데, 스냅샷과 계획 출력은 사람이 읽고 필요하면 손으로 복구할 수 있어야 한다.
// 안전 도구에서 이 성질을 잃으면 edc가 망가졌을 때 운영자가 기댈 곳이 없다.
func renderRouteEntry(entry routeEntry) string {
	var parts []string
	if entry.Type != "" && entry.Type != "unicast" {
		parts = append(parts, entry.Type)
	}
	parts = append(parts, entry.Dest)
	if entry.Via != "" {
		parts = append(parts, "via", entry.Via)
	}
	if entry.Dev != "" {
		parts = append(parts, "dev", entry.Dev)
	}
	if entry.Proto != "" {
		parts = append(parts, "proto", entry.Proto)
	}
	if entry.Src != "" {
		parts = append(parts, "src", entry.Src)
	}
	if entry.HasMetric {
		parts = append(parts, "metric", entry.Metric)
	}
	if entry.Table != "" && entry.Table != "main" {
		parts = append(parts, "table", entry.Table)
	}
	return strings.Join(parts, " ")
}

// renderIPRules는 정책 라우팅 규칙을 사람이 읽을 수 있는 형태로 만든다.
func renderIPRules(rules []ipRule) string {
	lines := make([]string, 0, len(rules))
	for _, rule := range rules {
		lines = append(lines, rule.Raw)
	}
	return strings.Join(lines, "\n")
}

// renderRouteEntries는 여러 경로를 스냅샷용 텍스트로 만든다.
func renderRouteEntries(entries []routeEntry) string {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, renderRouteEntry(entry))
	}
	return strings.Join(lines, "\n")
}
