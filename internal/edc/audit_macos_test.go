package edc

import (
	"context"
	"io/fs"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseDsclUsers(t *testing.T) {
	// 실제 dscl -readall 출력의 모양이다. 공백이 든 RecordName과 홈 두 개를 함께 쓴다.
	text := "NFSHomeDirectory: /var/root /private/var/root\nPrimaryGroupID: 0\nRecordName:\n root\n BUILTIN\\Local System\nUniqueID: 0\nUserShell: /bin/sh\n-\n" +
		"NFSHomeDirectory: /var/virusmails\nPrimaryGroupID: 83\nRecordName: _amavisd amavisd\nUniqueID: 83\nUserShell: /usr/bin/false\n-\n" +
		"NFSHomeDirectory: /Users/dev\nPrimaryGroupID: 20\nRecordName: dev\nUniqueID: 501\nUserShell: /bin/zsh\n-\n" +
		"RecordName: broken\nUserShell: /bin/zsh\n"
	got := parseDsclUsers(text)
	want := []auditPasswd{
		{Name: "root", UID: 0, GID: 0, Home: "/var/root", Shell: "/bin/sh"},
		{Name: "_amavisd", UID: 83, GID: 83, Home: "/var/virusmails", Shell: "/usr/bin/false"},
		{Name: "dev", UID: 501, GID: 20, Home: "/Users/dev", Shell: "/bin/zsh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseDsclSearchPath(t *testing.T) {
	if got := parseDsclSearchPath("CSPSearchPath: /Local/Default\n"); got != "" {
		t.Fatalf("로컬 노드만 있는데 %q", got)
	}
	if got := parseDsclSearchPath("CSPSearchPath:\n /Local/Default\n /Active Directory/CORP/All Domains\n"); got != "/Active Directory/CORP/All Domains" {
		t.Fatalf("got %q", got)
	}
}

func TestDarwinAccountsUseLoginUID(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/Users/dev/.ssh/authorized_keys", "ssh-ed25519 AAAA\n", 0o600)
	writeAuditFile(t, root, "/var/db/svc/.ssh/authorized_keys", "ssh-ed25519 AAAA\n", 0o600)
	accounts := []auditPasswd{{Name: "dev", UID: 501, Home: "/Users/dev"}, {Name: "_svc", UID: 300, Home: "/var/db/svc"}}
	sources := auditSources{root: root, owner: func(fs.FileInfo) (uint32, bool) { return 501, true },
		accounts: auditAccountSource{read: func() ([]auditPasswd, error) { return accounts, nil }, loginUID: darwinLoginUID}}
	files, _ := collectAuditKeyFiles(sources, accounts, auditKeyFilePatterns(sshdConfig{}))
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if !reflect.DeepEqual(paths, []string{"/Users/dev/.ssh", "/Users/dev/.ssh/authorized_keys"}) {
		t.Fatalf("UID 501 계정만 보아야 한다: %q", paths)
	}
	// 기본 계정 원천은 Linux 기준이라 UID 501을 서비스 계정으로 본다.
	if (auditSources{root: root}).loginAccount(accounts[0]) {
		t.Fatal("Linux 기준에서 UID 501을 로그인 계정으로 보았다")
	}
}

func TestParseSocketFilterState(t *testing.T) {
	for text, want := range map[string]int{
		"Firewall is disabled. (State = 0)\n":                                               darwinALFOff,
		"Firewall is enabled. (State = 1)\n":                                                darwinALFApps,
		"Firewall is blocking all non-essential traffic. (State = 2)":                       darwinALFBlockAll,
		"Firewall is enabled. (State = 1)\nFirewall has block all state set to enabled.\n":  darwinALFBlockAll,
		"Firewall is enabled. (State = 1)\nFirewall has block all state set to disabled.\n": darwinALFApps,
		"Firewall is disabled. (State = 0)\nFirewall has block all state set to enabled.\n": darwinALFOff,
	} {
		got, err := parseSocketFilterState(text)
		if err != nil || got != want {
			t.Errorf("%q = %d, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "Firewall is enabled.", "(State = 7)"} {
		if _, err := parseSocketFilterState(text); err == nil {
			t.Errorf("%q를 읽었다", text)
		}
	}
}

func TestParsePFStatus(t *testing.T) {
	enabled, err := parsePFStatus("No ALTQ support in kernel\nStatus: Enabled for 0 days 00:01:02           Debug: Urgent\n")
	if err != nil || !enabled {
		t.Fatalf("enabled = %t, %v", enabled, err)
	}
	if enabled, err := parsePFStatus("Status: Disabled                              Debug: Urgent\n"); err != nil || enabled {
		t.Fatalf("disabled = %t, %v", enabled, err)
	}
	if _, err := parsePFStatus("pfctl: /dev/pf: Permission denied\n"); err == nil {
		t.Fatal("Status 줄이 없는데 읽었다")
	}
}

func TestParsePFInputRules(t *testing.T) {
	tests := []struct {
		name string
		text string
		want pfInputRules
	}{
		{"macOS default", "scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all\n", pfInputRules{}},
		{"block then allow ssh", "block drop in all\npass in proto tcp from any to any port = 22 flags S/SA keep state\npass out all flags S/SA keep state\n",
			pfInputRules{V4: 2, V6: 2, V4Drops: true, V6Drops: true}},
		{"later pass all wins", "block drop in all\npass in all flags S/SA keep state\n", pfInputRules{V4: 2, V6: 2}},
		{"quick pass stops", "pass in quick all\nblock drop in all\n", pfInputRules{V4: 2, V6: 2}},
		{"quick block stops", "block drop in quick all\npass in all\n", pfInputRules{V4: 2, V6: 2, V4Drops: true, V6Drops: true}},
		{"inet only", "block return in inet all\n", pfInputRules{V4: 1, V4Drops: true}},
		{"interface only", "block drop in on en0 all\n", pfInputRules{V4: 1, V6: 1}},
		{"out ignored", "block drop out all\n", pfInputRules{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePFInputRules(tt.text); got != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// pfctl -nvf가 macOS 27.0.1에서 찍은 정규형이다. pfctl -s rules와 같은 형식으로 규칙을 출력하고 root 없이 실행된다.
func TestParsePFInputRulesCapturedOutput(t *testing.T) {
	tests := map[string]pfInputRules{
		"block drop in all\npass in proto tcp from any to any port = 22 flags S/SA keep state\npass in quick on lo0 all flags S/SA keep state\nblock return in inet6 all\npass out all flags S/SA keep state\n": {V4: 3, V6: 4, V4Drops: true, V6Drops: true},
		"block drop in log all\npass in all flags S/SA keep state\n":                  {V4: 2, V6: 2},
		"pass in quick all flags S/SA keep state\nblock drop in all\n":                {V4: 2, V6: 2},
		"block drop in log quick inet all\npass in inet6 all flags S/SA keep state\n": {V4: 1, V6: 1, V4Drops: true},
		"block drop all\n": {V4: 1, V6: 1, V4Drops: true, V6Drops: true},
		"block drop in on en0 all\npass in all flags S/SA keep state\n":                                                  {V4: 2, V6: 2},
		"block return-rst in proto tcp all\n":                                                                            {V4: 1, V6: 1},
		"scrub-anchor \"/*\" all fragment reassemble\nnat-anchor \"/*\" all\nrdr-anchor \"/*\" all\nanchor \"/*\" all\n": {},
	}
	for text, want := range tests {
		if got := parsePFInputRules(text); got != want {
			t.Errorf("%q = %+v, want %+v", text, got, want)
		}
	}
}

func TestAuditDarwinFirewall(t *testing.T) {
	tests := []struct {
		name     string
		firewall auditFirewall
		want     Status
		summary  string
	}{
		{"alf off, pf off", darwinAuditFirewall(darwinALFOff, true, false, pfInputRules{}), StatusWarn, T("audit.net.firewall_mac_open")},
		{"alf apps", darwinAuditFirewall(darwinALFApps, false, false, pfInputRules{}), StatusPass, T("audit.net.firewall_alf_apps")},
		{"alf block all", darwinAuditFirewall(darwinALFBlockAll, false, false, pfInputRules{}), StatusPass, T("audit.net.firewall_alf_blocks")},
		{"pf drops", darwinAuditFirewall(darwinALFOff, true, true, pfInputRules{V4: 2, V6: 2, V4Drops: true, V6Drops: true}), StatusPass, T("audit.net.firewall_pf_drops", 2)},
		{"pf rules only", darwinAuditFirewall(darwinALFOff, true, true, pfInputRules{V4: 1, V6: 1}), StatusPass, T("audit.net.firewall_rules", "pf", 1)},
		// pf가 켜져 있어도 규칙에 맞지 않은 트래픽을 통과시키면 막지 않는다.
		{"pf enabled but empty", darwinAuditFirewall(darwinALFOff, true, true, pfInputRules{}), StatusWarn, T("audit.net.firewall_mac_open")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := auditFirewallResult(auditFacts{Firewall: tt.firewall})
			if result.Status != tt.want || result.Summary != tt.summary {
				t.Fatalf("got %s %q", result.Status, result.Summary)
			}
			if len(result.Evidence) == 0 || !strings.Contains(result.Evidence[0].Value, "socketfilterfw  globalstate") {
				t.Fatalf("읽은 값이 근거에 없다: %+v", result.Evidence)
			}
		})
	}
	if pf := darwinAuditFirewall(darwinALFApps, false, false, pfInputRules{}); !strings.Contains(pf.Detail, T("audit.net.pf_unread")) {
		t.Fatalf("pf를 읽지 않았다는 표시가 없다: %q", pf.Detail)
	}
}

func TestAuditExposureDarwin(t *testing.T) {
	sockets := []listenSocket{{Proto: "tcp", Address: "*", Port: 5432, Process: "postgres"}}
	perApp := darwinAuditFirewall(darwinALFApps, false, false, pfInputRules{})
	if result := auditExposure(auditFacts{Listen: sockets, Firewall: perApp}); result.Status != StatusWarn || result.Summary != T("audit.net.exposed_app_filter", "postgres/5432") {
		t.Fatalf("ALF가 앱마다 거르는데 %s %q", result.Status, result.Summary)
	}
	blocked := darwinAuditFirewall(darwinALFBlockAll, false, false, pfInputRules{})
	if result := auditExposure(auditFacts{Listen: sockets, Firewall: blocked}); result.Status != StatusWarn || result.Summary != T("audit.net.exposed_filtered", "postgres/5432") {
		t.Fatalf("ALF가 모두 막는데 %s %q", result.Status, result.Summary)
	}
	// root가 아니면 launchd나 root process가 연 소켓이 빠진다. 찾지 못한 것을 열린 포트가 없다고 보지 않는다.
	if result := auditExposure(auditFacts{ListenPartial: true}); result.Status != StatusSkip {
		t.Fatalf("일부만 읽었는데 %s %q", result.Status, result.Summary)
	}
	result := auditExposure(auditFacts{Listen: sockets, ListenPartial: true, FirewallErr: errAuditNeedsRoot})
	if result.Status != StatusWarn || !strings.Contains(strings.Join(result.Warnings, "\n"), T("audit.net.listen_partial")) {
		t.Fatalf("일부만 읽었다는 경고가 없다: %s %q", result.Status, result.Warnings)
	}
}

func TestAuditIPv6Darwin(t *testing.T) {
	public := []netip.Addr{netip.MustParseAddr("2603:c020:4000::51")}
	sockets := []listenSocket{{Proto: "tcp", Address: "*", Port: 8080}}
	tests := []struct {
		name     string
		facts    auditFacts
		want     Status
		contains string
	}{
		{"alf apps", auditFacts{Addrs: public, Listen: sockets, Firewall: darwinAuditFirewall(darwinALFApps, false, false, pfInputRules{})}, StatusPass, T("audit.net.ipv6_app_filter")},
		{"alf block all", auditFacts{Addrs: public, Listen: sockets, Firewall: darwinAuditFirewall(darwinALFBlockAll, false, false, pfInputRules{})}, StatusPass, T("audit.net.ipv6_blocks", "socketfilterfw")},
		{"pf blocks inet only", auditFacts{Addrs: public, Listen: sockets, Firewall: darwinAuditFirewall(darwinALFOff, true, true, pfInputRules{V4: 1, V4Drops: true})}, StatusWarn, "pf  enabled"},
		{"partial listen", auditFacts{Addrs: public, ListenPartial: true, Firewall: darwinAuditFirewall(darwinALFOff, true, false, pfInputRules{})}, StatusSkip, T("audit.net.listen_partial")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := auditIPv6Open(tt.facts)
			text := result.Summary
			for _, evidence := range result.Evidence {
				text += "\n" + evidence.Value
			}
			if result.Status != tt.want || !strings.Contains(text, tt.contains) {
				t.Fatalf("got %s %q", result.Status, text)
			}
		})
	}
}

func TestMarkAuditUnsupported(t *testing.T) {
	results := []Result{auditResult("proc.preload", StatusPass, "ok"), auditResult("ssh.root_login", StatusWarn, "on")}
	got := markAuditUnsupported(results, darwinAuditUnsupported)
	if got[0].Status != StatusSkip || got[0].Summary != T("audit.skip.unsupported_os") || got[0].Probe != "audit.proc.preload" {
		t.Fatalf("원천이 없는 점검을 판정했다: %+v", got[0])
	}
	if got[1].Status != StatusWarn {
		t.Fatalf("macOS에서 읽는 점검을 지웠다: %+v", got[1])
	}
	for _, id := range darwinAuditUnsupported {
		if !strings.HasPrefix(id, "ctr.") && !slices.Contains(auditCheckIDs, id) {
			t.Errorf("%s는 audit 점검이 아니다", id)
		}
	}
}

func TestAuditIPForwardWithVMContainers(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/var/run/docker.sock", "", 0o600)
	sources := auditSources{root: root, euid: 501, vmContainers: true, ipForwardName: "net.inet.ip.forwarding",
		ipForward: func() (string, error) { return "1", nil },
		listen:    func() ([]listenSocket, error) { return nil, nil },
		owner:     func(fs.FileInfo) (uint32, bool) { return 0, true }}
	facts := collectAuditFacts(context.Background(), sources)
	if facts.ContainerHost != "" {
		t.Fatalf("VM 안의 컨테이너 런타임을 호스트 forwarding의 이유로 보았다: %q", facts.ContainerHost)
	}
	result := auditIPForward(facts)
	if result.Status != StatusWarn || result.Summary != T("audit.net.forward_on_host") || result.Evidence[0].Value != "net.inet.ip.forwarding = 1" {
		t.Fatalf("got %s %q %+v", result.Status, result.Summary, result.Evidence)
	}
}
