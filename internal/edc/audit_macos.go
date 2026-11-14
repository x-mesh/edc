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
	// Rules는 들어오는 트래픽에 걸리는 block·pass 줄의 수이고 Blocks는 그 가운데 block, CatchAll은 조건 없는 규칙이다. V4, V6은 family마다 센 수다.
	Rules, Blocks    int
	CatchAll         int
	V4, V6           int
	V4Drops, V6Drops bool
}

// pfRuleQualifiers가 있는 pass 규칙은 일부 트래픽만 통과시킨다. 모르는 조건이 붙은 pass는 모두 통과시킨다고 본다.
// 그래야 앞의 block을 실제보다 넓게 보지 않는다.
var pfRuleQualifiers = []string{"on", "proto", "from", "to", "port", "user", "group"}

// pfCatchAllBlock은 조건 없는 block이다. pfctl이 정규형으로 찍는 앞부분(동작, 응답 방식, in, log, quick, family) 뒤에 all이 오고,
// 그 뒤에는 매칭 범위를 바꾸지 않는 옵션(label, tag, queue, no state)만 있어야 한다.
// tagged, probability처럼 모르는 조건이 붙은 block을 전체 차단으로 보면 열린 포트를 막혔다고 보고한다.
func pfCatchAllBlock(fields []string) bool {
	rest := fields[1:]
	if len(rest) > 0 && (rest[0] == "drop" || strings.HasPrefix(rest[0], "return")) {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == "in" {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == "log" {
		rest = rest[1:]
		// log의 선택 사항은 "(all)", "(to pflog1)"처럼 괄호로 묶이며 필드 여러 개로 나뉠 수 있다.
		if len(rest) > 0 && strings.HasPrefix(rest[0], "(") {
			rest = skipPFValue(rest)
		}
	}
	if len(rest) > 0 && rest[0] == "quick" {
		rest = rest[1:]
	}
	if len(rest) > 0 && (rest[0] == "inet" || rest[0] == "inet6") {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != "all" {
		return false
	}
	rest = rest[1:]
	for len(rest) > 0 {
		switch {
		case rest[0] == "label" || rest[0] == "tag" || rest[0] == "queue":
			if len(rest) < 2 {
				return false
			}
			rest = skipPFValue(rest[1:])
		case rest[0] == "no" && len(rest) > 1 && rest[1] == "state":
			rest = rest[2:]
		default:
			return false
		}
	}
	return true
}

// skipPFValue는 값 하나를 건너뛴다. 따옴표나 괄호로 묶인 값은 공백 때문에 필드 여러 개로 나뉘므로 닫는 문자까지 건너뛴다.
func skipPFValue(fields []string) []string {
	closer := ""
	switch {
	case strings.HasPrefix(fields[0], "\""):
		closer = "\""
	case strings.HasPrefix(fields[0], "("):
		closer = ")"
	default:
		return fields[1:]
	}
	for index, field := range fields {
		if (index > 0 || len(field) > 1) && strings.HasSuffix(field, closer) {
			return fields[index+1:]
		}
	}
	return nil
}

// parsePFInputRules는 pfctl -s rules에서 들어오는 트래픽에 걸리는 block과 pass를 센다. pf는 마지막에 맞은 규칙을 따르고 quick이면 거기서 멈춘다.
// anchor 안의 규칙은 이 출력에 펼쳐지지 않는다. readDarwinFirewall이 anchor마다 따로 읽는다.
func parsePFInputRules(text string) pfInputRules {
	var rules pfInputRules
	verdict := map[string]string{"v4": "pass", "v6": "pass"}
	frozen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "block" && fields[0] != "pass") || slices.Contains(fields, "out") {
			continue
		}
		rules.Rules++
		block := fields[0] == "block"
		if block {
			rules.Blocks++
		}
		families := []string{"v4", "v6"}
		if slices.Contains(fields, "inet") {
			families = []string{"v4"}
		} else if slices.Contains(fields, "inet6") {
			families = []string{"v6"}
		}
		catchAll := pfCatchAllBlock(fields)
		if !block {
			catchAll = slices.Contains(fields, "all") && !slices.ContainsFunc(fields, func(field string) bool { return slices.Contains(pfRuleQualifiers, field) })
		}
		if catchAll {
			rules.CatchAll++
		}
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

// parsePFAnchors는 pfctl -s Anchors -v의 anchor 경로다. 한 줄에 하나씩, 하위 anchor까지 전체 경로로 나온다.
func parsePFAnchors(text string) []string {
	var anchors []string
	for _, line := range strings.Split(text, "\n") {
		if anchor := strings.TrimSpace(line); anchor != "" && !strings.Contains(anchor, " ") {
			anchors = append(anchors, anchor)
		}
	}
	return anchors
}

// darwinAuditFirewall은 Application Firewall과 pf를 합친다. 두 방화벽을 모두 지나야 들어오므로 하나가 막으면 막힌다.
// pf를 읽지 않았으면 pfRead가 거짓이고, ALF가 앱마다 거르거나 모두 막는다는 판정만 남는다.
// anchored는 모든 anchor의 규칙을 합친 것이다. anchor는 최상위 규칙 사이에서 평가된다. 조건 없는 규칙은 규칙에 맞지 않은 트래픽의 처리를
// 바꿀 수 있어 하나라도 있으면 pf를 판정하지 않는다. 조건이 붙은 규칙은 일부 트래픽만 바꾸므로 최상위의 같은 규칙처럼 판정에 넣지 않는다.
// macOS 인터넷 공유는 com.apple.internet-sharing anchor에 bridge 인터페이스와 주소를 지정한 규칙을 넣는다.
func darwinAuditFirewall(alfState int, pfRead, pfEnabled bool, pf, anchored pfInputRules) auditFirewall {
	firewall := auditFirewall{Tool: "socketfilterfw", ALF: true, ALFState: alfState, V6Known: true}
	details := []string{fmt.Sprintf("socketfilterfw  globalstate %d", alfState)}
	pfJudged := false
	switch {
	case !pfRead:
		details = append(details, T("audit.net.pf_unread"))
	case !pfEnabled:
		details = append(details, "pf  disabled")
	default:
		firewall.Tool += "+pf"
		firewall.Rules, firewall.V6Rules, firewall.PFBlocks, firewall.PFAnchored = pf.Rules, pf.V6, pf.Blocks, anchored.CatchAll
		firewall.PFUnknown = anchored.CatchAll > 0
		pfJudged = !firewall.PFUnknown
		details = append(details, fmt.Sprintf("pf  enabled  in %d  block %d  inet drop %t  inet6 drop %t", pf.Rules, pf.Blocks, pf.V4Drops, pf.V6Drops),
			fmt.Sprintf("pf anchors  rules %d  catch-all %d", anchored.Rules, anchored.CatchAll))
	}
	blockAll := alfState == darwinALFBlockAll
	firewall.Drops = blockAll || (pfJudged && pf.V4Drops)
	firewall.V6Drops = blockAll || (pfJudged && pf.V6Drops)
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
	case firewall.PFUnknown:
		result = auditResult(id, StatusSkip, T("audit.net.pf_anchor_unknown", firewall.PFAnchored))
		result.Next = "sudo pfctl -s Anchors -v"
	// pass 규칙만 있으면 pf가 켜져 있어도 아무것도 막지 않는다.
	case firewall.PFBlocks > 0:
		result = auditResult(id, StatusPass, T("audit.net.firewall_pf_blocks", firewall.PFBlocks))
	default:
		result = auditResult(id, StatusWarn, T("audit.net.firewall_mac_open"))
		result.Warnings = []string{T("audit.net.cloud_note")}
		result.Next = "sudo /usr/libexec/ApplicationFirewall/socketfilterfw --setglobalstate on"
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.values"), Value: firewall.Detail}}
	return result
}
