package edc

import (
	"strings"
	"testing"
)

// routeTableFixture는 실측 형태를 흉내 낸 `ip route show table all` 출력이다. metric·table·proto·src가
// 있는 줄, 빈 줄, linkdown 줄, proto 없는 줄, IPv6 줄, 그리고 목적지 앞에 타입이 붙는 줄을 모두 담는다.
// local·broadcast 줄은 실제 Ubuntu 24.04 호스트의 `ip route show table all`에서 그대로 가져왔다.
const routeTableFixture = `default via 10.20.1.1 dev enp1s0 metric 100
8.8.8.8 via 192.0.2.254 dev enp1s0

10.20.1.0/24 dev enp1s0 proto kernel scope link src 10.20.1.5
192.168.1.0/24 dev eth1 proto kernel scope link src 192.168.1.5 linkdown
169.254.0.0/16 dev enp1s0 scope link src 169.254.1.5
100.64.0.0/10 dev tailscale0 proto kernel scope link src 100.64.0.1 table 52
fe80::/64 dev enp1s0 proto kernel metric 256 pref medium
unreachable 203.0.113.0/24
local 192.0.2.13 dev enp1s0 table local proto kernel scope host src 192.0.2.13
broadcast 192.0.2.255 dev enp1s0 table local proto kernel scope link src 192.0.2.13
blackhole 198.51.100.0/24
`

// routeMetricTrapFixture는 브리프의 metric 함정을 흉내 낸다: 같은 목적지 8.8.8.8에 원래 metric 100
// 경로와, metric 없는 replace가 add로 동작해 생긴 metric 0 경로가 함께 남는다.
const routeMetricTrapFixture = `8.8.8.8 via 10.20.1.1 dev enp1s0 metric 100
8.8.8.8 via 192.0.2.254 dev enp1s0
`

func TestParseRouteTable(t *testing.T) {
	entries, parseErrors := parseRouteTable(routeTableFixture)
	// 타입이 붙은 줄을 읽지 못하면 blackhole default 같은 경로가 통째로 안 보이고, 그러면 경로 개수
	// 불변식이 틀린 값을 쓴다.
	if len(parseErrors) != 0 {
		t.Fatalf("parseErrors = %#v", parseErrors)
	}
	if len(entries) != 11 {
		t.Fatalf("entries = %d, want 11: %#v", len(entries), entries)
	}
	defaultEntry := entries[0]
	if defaultEntry.Dest != "default" || defaultEntry.Via != "10.20.1.1" || defaultEntry.Dev != "enp1s0" || !defaultEntry.HasMetric || defaultEntry.Metric != "100" {
		t.Fatalf("default entry = %#v", defaultEntry)
	}
	tailscaleEntry := entries[5]
	if tailscaleEntry.Dev != "tailscale0" || tailscaleEntry.Table != "52" || tailscaleEntry.Proto != "kernel" || tailscaleEntry.Src != "100.64.0.1" {
		t.Fatalf("tailscale entry = %#v", tailscaleEntry)
	}
	linkdownEntry := entries[3]
	if linkdownEntry.Dev != "eth1" || linkdownEntry.Src != "192.168.1.5" {
		t.Fatalf("linkdown entry = %#v", linkdownEntry)
	}
	noProtoEntry := entries[4]
	if noProtoEntry.Proto != "" || noProtoEntry.Src != "169.254.1.5" {
		t.Fatalf("no-proto entry = %#v", noProtoEntry)
	}
	ipv6Entry := entries[6]
	if ipv6Entry.Dest != "fe80::/64" || !ipv6Entry.HasMetric || ipv6Entry.Metric != "256" {
		t.Fatalf("ipv6 entry = %#v", ipv6Entry)
	}
	unreachableEntry := entries[7]
	if unreachableEntry.Type != "unreachable" || unreachableEntry.Dest != "203.0.113.0/24" || unreachableEntry.Dev != "" {
		t.Fatalf("unreachable entry = %#v", unreachableEntry)
	}
	localEntry := entries[8]
	if localEntry.Type != "local" || localEntry.Dest != "192.0.2.13" || localEntry.Dev != "enp1s0" || localEntry.Table != "local" {
		t.Fatalf("local entry = %#v", localEntry)
	}
	broadcastEntry := entries[9]
	if broadcastEntry.Type != "broadcast" || broadcastEntry.Dest != "192.0.2.255" || broadcastEntry.Dev != "enp1s0" {
		t.Fatalf("broadcast entry = %#v", broadcastEntry)
	}
	blackholeEntry := entries[10]
	if blackholeEntry.Type != "blackhole" || blackholeEntry.Dest != "198.51.100.0/24" || blackholeEntry.Dev != "" {
		t.Fatalf("blackhole entry = %#v", blackholeEntry)
	}
}

// 타입은 metric과 마찬가지로 경로의 정체성이다. 복원 argv에서 빠뜨리면 같은 목적지의 다른 경로가 되어
// 롤백이 성공을 보고하고도 원래 상태로 돌아가지 못한다.
func TestRouteArgsPreserveRouteType(t *testing.T) {
	entries, _ := parseRouteTable(routeTableFixture)
	blackholeEntry := entries[10]
	replaceArgs, err := routeReplaceArgs(blackholeEntry, "")
	if err != nil {
		t.Fatalf("routeReplaceArgs error: %v", err)
	}
	if strings.Join(replaceArgs, " ") != "route replace blackhole 198.51.100.0/24" {
		t.Fatalf("replace args = %#v", replaceArgs)
	}
	deleteArgs, err := routeDeleteArgs(blackholeEntry)
	if err != nil {
		t.Fatalf("routeDeleteArgs error: %v", err)
	}
	if strings.Join(deleteArgs, " ") != "route del blackhole 198.51.100.0/24" {
		t.Fatalf("delete args = %#v", deleteArgs)
	}
	localArgs, err := routeReplaceArgs(entries[8], "")
	if err != nil {
		t.Fatalf("routeReplaceArgs error: %v", err)
	}
	if strings.Join(localArgs, " ") != "route replace local 192.0.2.13 dev enp1s0 proto kernel src 192.0.2.13 table local" {
		t.Fatalf("local replace args = %#v", localArgs)
	}
}

func TestParseRouteTablePanicFreeEdgeCases(t *testing.T) {
	for _, text := range []string{"", "\n\n\n", "default", "default dev", "malformed !! tokens here"} {
		entries, parseErrors := parseRouteTable(text)
		_ = entries
		_ = parseErrors
	}
}

func TestRouteReplaceArgsKeepsFullSpec(t *testing.T) {
	entries, _ := parseRouteTable(routeTableFixture)
	defaultEntry := entries[0]
	args, err := routeReplaceArgs(defaultEntry, "192.0.2.254")
	if err != nil {
		t.Fatalf("routeReplaceArgs error: %v", err)
	}
	joined := strings.Join(args, " ")
	if joined != "route replace default via 192.0.2.254 dev enp1s0 metric 100" {
		t.Fatalf("args = %q", joined)
	}

	tableEntry := entries[5]
	args, err = routeReplaceArgs(tableEntry, "100.64.0.1")
	if err != nil {
		t.Fatalf("routeReplaceArgs error: %v", err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "table 52") {
		t.Fatalf("args = %q, want table 52", joined)
	}
}

func TestRouteReplaceArgsRejectsDroppedMetric(t *testing.T) {
	entries, _ := parseRouteTable(routeTableFixture)
	broken := entries[0]
	broken.Metric = "" // HasMetric은 그대로 true인데 값만 사라진 상태를 흉내 낸다.
	if _, err := routeReplaceArgs(broken, "192.0.2.254"); err == nil {
		t.Fatal("expected an error when a metric-bearing entry carries no metric value")
	}
}

func TestRouteCountForDetectsMetricTrap(t *testing.T) {
	entries, parseErrors := parseRouteTable(routeMetricTrapFixture)
	if len(parseErrors) != 0 {
		t.Fatalf("parseErrors = %#v", parseErrors)
	}
	if count := routeCountFor(entries, "8.8.8.8"); count != 2 {
		t.Fatalf("routeCountFor = %d, want 2", count)
	}
}

func TestRouteDeleteArgsForResidualEntry(t *testing.T) {
	entries, _ := parseRouteTable(routeMetricTrapFixture)
	residual := entriesForDest(entries, "8.8.8.8")[1]
	args, err := routeDeleteArgs(residual)
	if err != nil {
		t.Fatalf("routeDeleteArgs error: %v", err)
	}
	joined := strings.Join(args, " ")
	if joined != "route del 8.8.8.8 via 192.0.2.254 dev enp1s0" {
		t.Fatalf("args = %q", joined)
	}
}

func TestParseRouteGet(t *testing.T) {
	get, err := parseRouteGet("100.64.0.2 dev tailscale0 table 52 src 100.64.0.1 uid 0")
	if err != nil {
		t.Fatalf("parseRouteGet error: %v", err)
	}
	if get.Dev != "tailscale0" || get.Table != "52" || get.Src != "100.64.0.1" {
		t.Fatalf("get = %#v", get)
	}
}

func TestParseRouteGetRejectsMissingDev(t *testing.T) {
	if _, err := parseRouteGet("8.8.8.8 uid 0"); err == nil {
		t.Fatal("expected an error when route get output carries no dev")
	}
}

func TestRouteGetMatchesDetectsMoreSpecificRoute(t *testing.T) {
	entries, _ := parseRouteTable(routeTableFixture)
	defaultEntry := entries[0]
	dedicatedGet, err := parseRouteGet("8.8.8.8 via 192.0.2.254 dev enp1s0 src 10.20.1.5 uid 0")
	if err != nil {
		t.Fatalf("parseRouteGet error: %v", err)
	}
	if routeGetMatches(dedicatedGet, defaultEntry) {
		t.Fatal("8.8.8.8's dedicated route must not match the default entry (different via)")
	}
}

// ipRuleFixture는 브리프의 ip rule 7줄을 흉내 낸다.
const ipRuleFixture = `0:	from all lookup local
5210:	from all to 100.115.92.0/23 lookup main
5230:	from all fwmark 0x80000/0x80000 lookup 52
5250:	not from all fwmark 0x80000/0x80000 lookup 52
5270:	from all lookup main suppress_prefixlength 0
32766:	from all lookup main
32767:	from all lookup default
`

func TestParseIPRule(t *testing.T) {
	rules, err := parseIPRule(ipRuleFixture)
	if err != nil {
		t.Fatalf("parseIPRule error: %v", err)
	}
	if len(rules) != 7 {
		t.Fatalf("rules = %d, want 7: %#v", len(rules), rules)
	}
	if rules[0].Priority != "0" || rules[0].Table != "local" {
		t.Fatalf("rules[0] = %#v", rules[0])
	}
	if rules[3].Priority != "5250" || !rules[3].Not || rules[3].Table != "52" {
		t.Fatalf("rules[3] = %#v", rules[3])
	}
	if rules[6].Table != "default" {
		t.Fatalf("rules[6] = %#v", rules[6])
	}
}

func TestParseSSHConnection(t *testing.T) {
	client, server, ok := parseSSHConnection("100.64.0.2 50015 100.64.0.1 22")
	if !ok || client != "100.64.0.2" || server != "100.64.0.1" {
		t.Fatalf("client=%q server=%q ok=%v", client, server, ok)
	}
	if _, _, ok := parseSSHConnection("malformed"); ok {
		t.Fatal("malformed SSH_CONNECTION must not parse")
	}
}

func TestLockoutRisk(t *testing.T) {
	defaultEntry := routeEntry{Dest: "default", Via: "10.20.1.1", Dev: "enp1s0", HasMetric: true, Metric: "100"}
	cases := []struct {
		name       string
		assessment lockoutAssessment
		wantRisk   string
	}{
		{
			name: "non-tunnel session rides the target route",
			assessment: lockoutAssessment{
				SessionGet:  routeGetResult{Dev: "enp1s0", Via: "10.20.1.1"},
				TargetEntry: defaultEntry,
			},
			wantRisk: "high",
		},
		{
			name: "non-tunnel session uses a dedicated route",
			assessment: lockoutAssessment{
				SessionGet:  routeGetResult{Dev: "enp1s0", Via: "192.0.2.254"},
				TargetEntry: defaultEntry,
			},
			wantRisk: "low",
		},
		{
			name: "tunnel session, underlay rides the target route",
			assessment: lockoutAssessment{
				SessionGet:  routeGetResult{Dev: "tailscale0", Table: "52"},
				TargetEntry: defaultEntry,
				UnderlayGet: &routeGetResult{Dev: "enp1s0", Via: "10.20.1.1"},
			},
			wantRisk: "high",
		},
		{
			name: "tunnel session, underlay does not ride the target route",
			assessment: lockoutAssessment{
				SessionGet:  routeGetResult{Dev: "tailscale0", Table: "52"},
				TargetEntry: defaultEntry,
				UnderlayGet: &routeGetResult{Dev: "enp1s0", Via: "192.0.2.254"},
			},
			wantRisk: "low",
		},
		{
			name: "tunnel session, underlay unresolved must not lower risk",
			assessment: lockoutAssessment{
				SessionGet:  routeGetResult{Dev: "tailscale0", Table: "52"},
				TargetEntry: defaultEntry,
				UnderlayGet: nil,
			},
			wantRisk: "high",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			risk, reason := lockoutRisk(testCase.assessment)
			if risk != testCase.wantRisk || reason == "" {
				t.Fatalf("risk = %q reason = %q, want risk %q", risk, reason, testCase.wantRisk)
			}
		})
	}
}
