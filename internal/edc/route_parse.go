package edc

import (
	"fmt"
	"strings"
)

// routeEntry는 `ip route show table all` 한 줄을 담는다. 원문 줄을 남겨 재구성 실패를 진단할 수 있게 한다.
// 필드는 두 종류다. 커널이 경로를 찾는 키(Dest, Table, Metric, Tos)와 그 경로에 붙은 속성
// (Via, Dev, Proto, Src, Scope, Type)이다. 키가 하나라도 빠지면 replace가 엉뚱한 경로를 건드리거나
// 새 경로를 만든다. metric을 빠뜨렸을 때 실제로 그 일이 일어났다.
type routeEntry struct {
	Dest      string
	Type      string
	Via       string
	Dev       string
	Proto     string
	Src       string
	Scope     string
	Metric    string
	HasMetric bool
	Tos       int
	Table     string
	Raw       string
}

// routeGetResult는 `ip route get <address>` 한 줄을 담는다.
type routeGetResult struct {
	Dest  string
	Type  string
	Via   string
	Dev   string
	Table string
	Src   string
	Raw   string
}

// ipRule은 `ip rule show` 한 줄을 담는다.
type ipRule struct {
	Priority string
	Table    string
	Not      bool
	Raw      string
}

// routeTypesWithoutDev는 dev를 갖지 않는 거부형 경로다. dev가 없다고 파싱 실패로 보면 blackhole
// default 같은 경로가 통째로 안 보이고, 그러면 경로 개수 불변식이 틀린 값을 쓴다.
var routeTypesWithoutDev = map[string]bool{
	"throw": true, "unreachable": true, "prohibit": true, "blackhole": true,
}

// routeReplaceArgs는 entry의 원문 스펙(metric, table, proto, src)을 그대로 담아 replace argv를 만든다.
// via만 newVia로 바꾼다. entry가 metric을 가졌다고 표시하면서 값이 빈 호출은 metric 누락 사고를 그대로
// 실행하지 않도록 오류로 막는다.
func routeReplaceArgs(entry routeEntry, newVia string) ([]string, error) {
	if entry.Dest == "" {
		return nil, fmt.Errorf("route entry carries no dest")
	}
	if entry.Dev == "" && !routeTypesWithoutDev[entry.Type] {
		return nil, fmt.Errorf("route entry carries no dev")
	}
	if entry.HasMetric && entry.Metric == "" {
		return nil, fmt.Errorf("route entry claims a metric but carries no value: replace would drop it and become add")
	}
	via := newVia
	if via == "" {
		via = entry.Via
	}
	args := []string{"route", "replace"}
	if entry.Type != "" && entry.Type != "unicast" {
		args = append(args, entry.Type)
	}
	args = append(args, entry.Dest)
	if via != "" {
		args = append(args, "via", via)
	}
	if entry.Dev != "" {
		args = append(args, "dev", entry.Dev)
	}
	if entry.Proto != "" {
		args = append(args, "proto", entry.Proto)
	}
	if entry.Src != "" {
		args = append(args, "src", entry.Src)
	}
	if entry.HasMetric {
		args = append(args, "metric", entry.Metric)
	}
	if entry.Table != "" && entry.Table != "main" {
		args = append(args, "table", entry.Table)
	}
	return args, nil
}

// routeDeleteArgs는 entry를 정확히 지우는 del argv를 만든다. rollback이 기준선을 넘는 잔존 경로를
// 지울 때 쓴다.
func routeDeleteArgs(entry routeEntry) ([]string, error) {
	if entry.Dest == "" || (entry.Dev == "" && !routeTypesWithoutDev[entry.Type]) {
		return nil, fmt.Errorf("route entry carries no dest or dev")
	}
	args := []string{"route", "del"}
	if entry.Type != "" && entry.Type != "unicast" {
		args = append(args, entry.Type)
	}
	args = append(args, entry.Dest)
	if entry.Via != "" {
		args = append(args, "via", entry.Via)
	}
	if entry.Dev != "" {
		args = append(args, "dev", entry.Dev)
	}
	if entry.HasMetric {
		args = append(args, "metric", entry.Metric)
	}
	if entry.Table != "" && entry.Table != "main" {
		args = append(args, "table", entry.Table)
	}
	return args, nil
}

// routeCountFor는 목적지별 경로 개수를 센다. 변경 전후 불변식 검사의 기준값이다.
func routeCountFor(entries []routeEntry, dest string) int {
	count := 0
	for _, entry := range entries {
		if entry.Dest == dest {
			count++
		}
	}
	return count
}

// entriesForDest는 dest에 해당하는 entry만 원문 순서대로 돌려준다.
func entriesForDest(entries []routeEntry, dest string) []routeEntry {
	var matched []routeEntry
	for _, entry := range entries {
		if entry.Dest == dest {
			matched = append(matched, entry)
		}
	}
	return matched
}

// routeGetMatches는 route get 결과가 entry가 가리키는 경로와 같은 dev/via를 타는지 본다.
// 다르면 entry를 바꿔도 이 목적지는 영향을 받지 않는다는 뜻이다.
func routeGetMatches(get routeGetResult, entry routeEntry) bool {
	if get.Dev != entry.Dev {
		return false
	}
	if entry.Via != "" && get.Via != entry.Via {
		return false
	}
	return true
}

// parseSSHConnection은 SSH_CONNECTION 환경변수(client-ip client-port server-ip server-port)를 나눈다.
func parseSSHConnection(value string) (client, server string, ok bool) {
	fields := strings.Fields(value)
	if len(fields) != 4 {
		return "", "", false
	}
	return fields[0], fields[2], true
}

// tunnelDevicePrefixes는 자기 차단 판정에서 재귀 확인이 필요한 터널 장치 이름 접두사다.
var tunnelDevicePrefixes = []string{"tailscale", "wg", "tun", "vxlan"}

// isTunnelDevice는 이름 패턴으로 터널 장치를 가른다.
func isTunnelDevice(dev string) bool {
	for _, prefix := range tunnelDevicePrefixes {
		if strings.HasPrefix(dev, prefix) {
			return true
		}
	}
	return false
}

// lockoutAssessment는 자기 차단 판정에 필요한 관측값을 담는다. 실행 계층 없이 순수 함수로 판정하기
// 위해 ip route get 결과를 값으로 받는다.
type lockoutAssessment struct {
	SessionGet  routeGetResult
	TargetEntry routeEntry
	// UnderlayGet은 SessionGet.Dev가 터널일 때 그 underlay endpoint로 한 번 더 ip route get한 결과다.
	// 확인하지 못했으면 nil이다.
	UnderlayGet *routeGetResult
}

// lockoutRisk는 세 겹 판정 중 자기 차단 위험을 매긴다. underlay를 확인하지 못하면 위험을 낮추지 않고
// 언제나 high로 본다. 사유는 로케일 키로 돌려주고 표시할 때 번역한다.
func lockoutRisk(assessment lockoutAssessment) (risk string, reasonKey string) {
	if !isTunnelDevice(assessment.SessionGet.Dev) {
		if routeGetMatches(assessment.SessionGet, assessment.TargetEntry) {
			return "high", "route.lockout.reason.session_rides"
		}
		return "low", "route.lockout.reason.session_clear"
	}
	if assessment.UnderlayGet == nil {
		return "high", "route.lockout.reason.underlay_unresolved"
	}
	if routeGetMatches(*assessment.UnderlayGet, assessment.TargetEntry) {
		return "high", "route.lockout.reason.underlay_rides"
	}
	return "low", "route.lockout.reason.underlay_clear"
}
