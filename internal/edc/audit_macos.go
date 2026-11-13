package edc

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// macOS audit의 출력 해석이다. 빌드 태그를 두지 않아 Linux CI에서도 테스트한다.

// darwinAuditUnsupported는 macOS에 원천이 없거나 아직 macOS 원천을 읽지 않는 점검이다. /proc, wtmp, dpkg, shadow를 읽는 점검은
// macOS에서 파일이 없어 통과로 보일 수 있다.
var darwinAuditUnsupported = []string{
	"user.empty_pass", "user.root_groups", "login.mismatch", "login.local_source", "history.tampered",
	"proc.unusual_exe", "proc.preload", "proc.socket_shell", "proc.raw_socket", "net.reverse_tunnel", "ssh.extra_sshd",
	"persist.changed", "file.pkg_modified", "kernel.sysctl", "kernel.taint", "docker.escape",
}

// darwinLoginUID는 macOS가 처음 만든 사용자에게 주는 UID다. 그 아래는 _로 시작하는 서비스 계정이다.
const darwinLoginUID = 501

const (
	darwinALFOff      = 0
	darwinALFApps     = 1
	darwinALFBlockAll = 2
)

// parseDsclRecords는 dscl -readall의 출력을 레코드마다 속성 이름과 값으로 나눈다. 레코드 사이에는 "-" 줄이 있다.
// 값에 공백이 없으면 "Key: v1 v2"처럼 한 줄에 적고, 공백이 든 값이 하나라도 있으면 "Key:" 아래 줄마다 공백 하나를 앞에 두고 적는다.
func parseDsclRecords(text string) []map[string][]string {
	var records []map[string][]string
	record := map[string][]string{}
	key := ""
	flush := func() {
		if len(record) > 0 {
			records = append(records, record)
		}
		record, key = map[string][]string{}, ""
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case line == "-":
			flush()
		case strings.HasPrefix(line, " "):
			if key != "" {
				record[key] = append(record[key], strings.TrimPrefix(line, " "))
			}
		default:
			name, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = name
			record[key] = strings.Fields(value)
		}
	}
	flush()
	return records
}

func parseDsclUsers(text string) []auditPasswd {
	var users []auditPasswd
	for _, record := range parseDsclRecords(text) {
		first := func(key string) string {
			if values := record[key]; len(values) > 0 {
				return values[0]
			}
			return ""
		}
		uid, uidErr := strconv.Atoi(first("UniqueID"))
		gid, gidErr := strconv.Atoi(first("PrimaryGroupID"))
		name := first("RecordName")
		if uidErr != nil || gidErr != nil || name == "" {
			continue
		}
		users = append(users, auditPasswd{Name: name, UID: uid, GID: gid, Home: first("NFSHomeDirectory"), Shell: first("UserShell")})
	}
	return users
}

// parseDsclSearchPath는 계정을 찾는 Directory Services 노드에서 로컬 노드를 뺀 것이다. LDAP나 Active Directory 노드의 계정은 dscl .로 읽지 못한다.
func parseDsclSearchPath(text string) string {
	var external []string
	for _, record := range parseDsclRecords(text) {
		for _, node := range record["CSPSearchPath"] {
			if node != "/Local/Default" && node != "/BSD/local" && !slices.Contains(external, node) {
				external = append(external, node)
			}
		}
	}
	return strings.Join(external, ", ")
}

// parseSocketFilterState는 socketfilterfw --getglobalstate --getblockall의 출력을 읽는다. "모두 차단"은 globalstate와 따로 켜는
// 설정이라 "(State = 1)"과 "block all state set to enabled"가 함께 나온다. 꺼진 방화벽의 block all 설정은 적용되지 않는다.
func parseSocketFilterState(text string) (int, error) {
	state, err := parseSocketFilterGlobalState(text)
	if err == nil && state == darwinALFApps && strings.Contains(text, "block all state set to enabled") {
		state = darwinALFBlockAll
	}
	return state, err
}

func parseSocketFilterGlobalState(text string) (int, error) {
	_, rest, ok := strings.Cut(text, "State = ")
	if !ok {
		return 0, fmt.Errorf("socketfilterfw: %q", strings.TrimSpace(text))
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	state, err := strconv.Atoi(rest[:end])
	if err != nil || state < darwinALFOff || state > darwinALFBlockAll {
		return 0, fmt.Errorf("socketfilterfw: %q", strings.TrimSpace(text))
	}
	return state, nil
}

func parsePFStatus(text string) (bool, error) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Status:" {
			return fields[1] == "Enabled", nil
		}
	}
	return false, fmt.Errorf("pfctl -s info: no Status line")
}

type pfInputRules struct {
	V4, V6           int
	V4Drops, V6Drops bool
}

// pfRuleQualifiers가 있는 규칙은 일부 트래픽에만 맞는다. 규칙에 맞지 않은 트래픽의 처리는 조건 없는 규칙으로만 정해진다.
var pfRuleQualifiers = []string{"on", "proto", "from", "to", "port", "user", "group"}

// parsePFInputRules는 pfctl -s rules에서 들어오는 트래픽에 걸리는 block과 pass를 센다. pf는 마지막에 맞은 규칙을 따르고 quick이면 거기서 멈춘다.
// anchor 안의 규칙은 pfctl -s rules가 펼치지 않아 세지 않는다. macOS 기본 com.apple anchor는 비어 있다.
func parsePFInputRules(text string) pfInputRules {
	var rules pfInputRules
	verdict := map[string]string{"v4": "pass", "v6": "pass"}
	frozen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "block" && fields[0] != "pass") || slices.Contains(fields, "out") {
			continue
		}
		families := []string{"v4", "v6"}
		if slices.Contains(fields, "inet") {
			families = []string{"v4"}
		} else if slices.Contains(fields, "inet6") {
			families = []string{"v6"}
		}
		catchAll := slices.Contains(fields, "all") && !slices.ContainsFunc(fields, func(field string) bool { return slices.Contains(pfRuleQualifiers, field) })
		for _, family := range families {
			if family == "v4" {
				rules.V4++
			} else {
				rules.V6++
			}
			if catchAll && !frozen[family] {
				verdict[family] = fields[0]
				frozen[family] = slices.Contains(fields, "quick")
			}
		}
	}
	rules.V4Drops, rules.V6Drops = verdict["v4"] == "block", verdict["v6"] == "block"
	return rules
}

// darwinAuditFirewall은 Application Firewall과 pf를 합친다. 두 방화벽을 모두 지나야 들어오므로 하나가 막으면 막힌다.
// pf를 읽지 않았으면 pfRead가 거짓이고, ALF가 앱마다 거르거나 모두 막는다는 판정만 남는다.
func darwinAuditFirewall(alfState int, pfRead, pfEnabled bool, pf pfInputRules) auditFirewall {
	firewall := auditFirewall{Tool: "socketfilterfw", ALF: true, ALFState: alfState, V6Known: true}
	details := []string{fmt.Sprintf("socketfilterfw  globalstate %d", alfState)}
	switch {
	case !pfRead:
		details = append(details, T("audit.net.pf_unread"))
	case !pfEnabled:
		details = append(details, "pf  disabled")
	default:
		firewall.Tool += "+pf"
		firewall.Rules, firewall.V6Rules = pf.V4, pf.V6
		details = append(details, fmt.Sprintf("pf  enabled  inet in %d  inet6 in %d  inet drop %t  inet6 drop %t", pf.V4, pf.V6, pf.V4Drops, pf.V6Drops))
	}
	blockAll := alfState == darwinALFBlockAll
	firewall.Drops = blockAll || (pfEnabled && pf.V4Drops)
	firewall.V6Drops = blockAll || (pfEnabled && pf.V6Drops)
	firewall.Detail = strings.Join(details, "\n")
	return firewall
}

// perApp은 ALF가 앱마다 들어오는 연결을 거르는 상태다. 어느 앱을 허용했는지는 판정하지 않는다.
func (firewall auditFirewall) perApp() bool {
	return firewall.ALF && firewall.ALFState == darwinALFApps
}

func auditDarwinFirewallResult(facts auditFacts) Result {
	const id = "net.firewall"
	firewall := facts.Firewall
	var result Result
	switch {
	case firewall.ALFState == darwinALFBlockAll:
		result = auditResult(id, StatusPass, T("audit.net.firewall_alf_blocks"))
	case firewall.Drops:
		result = auditResult(id, StatusPass, T("audit.net.firewall_pf_drops", firewall.Rules))
	case firewall.perApp():
		result = auditResult(id, StatusPass, T("audit.net.firewall_alf_apps"))
	case firewall.Rules > 0:
		result = auditResult(id, StatusPass, T("audit.net.firewall_rules", "pf", firewall.Rules))
	default:
		result = auditResult(id, StatusWarn, T("audit.net.firewall_mac_open"))
		result.Warnings = []string{T("audit.net.cloud_note")}
		result.Next = "/usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate"
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.values"), Value: firewall.Detail}}
	return result
}
