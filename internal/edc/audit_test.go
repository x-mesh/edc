package edc

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeAuditFile(t *testing.T, root, path, content string, mode fs.FileMode) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// umask가 bit를 지우므로 mode를 다시 정한다.
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

func auditResultByID(t *testing.T, results []Result, id string) Result {
	t.Helper()
	for _, result := range results {
		if result.Probe == "audit."+id {
			return result
		}
	}
	t.Fatalf("audit.%s가 없다", id)
	return Result{}
}

func TestReadSSHDConfigFirstValueWinsAcrossIncludes(t *testing.T) {
	root := t.TempDir()
	// Ubuntu는 Include를 맨 위에 두어 drop-in이 본 파일보다 먼저 읽힌다.
	writeAuditFile(t, root, "/etc/ssh/sshd_config", "Include /etc/ssh/sshd_config.d/*.conf\nPasswordAuthentication yes\nPermitRootLogin yes\nChallengeResponseAuthentication no\nMatch User backup\n  PermitRootLogin no\nUsePAM yes\n", 0o644)
	writeAuditFile(t, root, "/etc/ssh/sshd_config.d/50-cloud-init.conf", "PasswordAuthentication no\n", 0o644)
	writeAuditFile(t, root, "/etc/ssh/sshd_config.d/60-local.conf", "PasswordAuthentication=yes\n", 0o644)
	config, err := readSSHDConfig(root, "/etc/ssh/sshd_config")
	if err != nil {
		t.Fatal(err)
	}
	if value, source := config.value("passwordauthentication", "yes"); value != "no" || source != "/etc/ssh/sshd_config.d/50-cloud-init.conf" {
		t.Fatalf("PasswordAuthentication = %q from %q", value, source)
	}
	if value, _ := config.value("permitrootlogin", "prohibit-password"); value != "yes" {
		t.Fatalf("Match 블록이 앞선 값을 덮었다: %q", value)
	}
	if value, _ := config.value("kbdinteractiveauthentication", "yes"); value != "no" {
		t.Fatalf("옛 이름을 새 이름으로 읽지 못했다: %q", value)
	}
	if value, source := config.value("usepam", "no"); value != "no" || source != "default" {
		t.Fatalf("Match 아래 값을 전역 값으로 읽었다: %q from %q", value, source)
	}
	if !config.Match {
		t.Fatal("Match 블록이 있었다는 표시가 없다")
	}
}

func TestReadSudoNoPasswdRules(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/sudoers", "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n%admin ALL=(ALL) NOPASSWD: /usr/bin/systemctl restart app\n@includedir /etc/sudoers.d\n", 0o440)
	writeAuditFile(t, root, "/etc/sudoers.d/90-cloud-init-users", "# comment\nubuntu ALL=(ALL) NOPASSWD:ALL\n", 0o440)
	writeAuditFile(t, root, "/etc/sudoers.d/deploy", "deploy ALL=(ALL) NOPASSWD: SETENV: /bin/ls, \\\n  ALL\n", 0o440)
	// uid로 쓴 사용자는 주석이 아니다. PASSWD: 뒤의 ALL은 비밀번호를 묻는다.
	writeAuditFile(t, root, "/etc/sudoers.d/uid", "#1001 ALL=(ALL) NOPASSWD: ALL\nops ALL=(ALL) NOPASSWD: /bin/ls, PASSWD: ALL\n", 0o440)
	// sudo는 '.'이 들어간 이름을 읽지 않는다.
	writeAuditFile(t, root, "/etc/sudoers.d/old.bak", "old ALL=(ALL) NOPASSWD: ALL\n", 0o440)
	rules, err := readSudoNoPasswdRules(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 || !strings.Contains(rules[0], "ubuntu") || !strings.Contains(rules[1], "deploy") || !strings.Contains(rules[2], "#1001") {
		t.Fatalf("rules = %q", rules)
	}
}

func TestParseFirewallInput(t *testing.T) {
	tests := []struct {
		name string
		got  auditFirewall
		want auditFirewall
	}{
		{
			name: "nft policy drop",
			got:  parseNftInputChains("table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t\tct state established,related accept\n\t\ttcp dport 22 accept\n\t}\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy drop;\n\t}\n}\n"),
			want: auditFirewall{Tool: "nftables", Chains: 1, Rules: 2, Drops: true},
		},
		{
			name: "iptables-nft with final reject",
			got:  parseNftInputChains("table ip filter {\n\tchain INPUT {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tct state related,established counter packets 10 bytes 900 accept\n\t\tcounter packets 0 bytes 0 reject with icmp host-prohibited\n\t}\n}\ntable ip nat {\n\tchain INPUT {\n\t\ttype nat hook input priority 100; policy accept;\n\t}\n}\n"),
			want: auditFirewall{Tool: "nftables", Chains: 1, Rules: 2, Drops: true},
		},
		{
			name: "nft accept with port rule",
			got:  parseNftInputChains("table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\ttcp dport 6379 drop\n\t}\n}\n"),
			want: auditFirewall{Tool: "nftables", Chains: 1, Rules: 1},
		},
		{
			name: "iptables reject last",
			got:  parseIPTablesInput("*filter\n:INPUT ACCEPT [0:0]\n:FORWARD ACCEPT [0:0]\n-A INPUT -m state --state RELATED,ESTABLISHED -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\nCOMMIT\n"),
			want: auditFirewall{Tool: "iptables", Chains: 1, Rules: 2, Drops: true},
		},
		{
			name: "iptables empty accept",
			got:  parseIPTablesInput("*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n"),
			want: auditFirewall{Tool: "iptables", Chains: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %#v, want %#v", tt.got, tt.want)
			}
		})
	}
}

func TestCompareKernelReleases(t *testing.T) {
	if compareKernelReleases("6.8.0-10-generic", "6.8.0-9-generic") <= 0 {
		t.Fatal("-10이 -9보다 새것이어야 한다")
	}
	if compareKernelReleases("5.14.0-427.el9.x86_64", "5.14.0-427.el9.x86_64") != 0 {
		t.Fatal("같은 release는 같아야 한다")
	}
	if compareKernelReleases("6.1.0", "6.11.0") >= 0 {
		t.Fatal("6.11이 6.1보다 새것이어야 한다")
	}
}

func TestAuditExposureNeedsFirewallToFail(t *testing.T) {
	sockets := []listenSocket{
		{Proto: "tcp", Address: "0.0.0.0", Port: 6379, Process: "redis-server"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 5432},
		{Proto: "tcp", Address: "*", Port: 22},
	}
	tests := []struct {
		name     string
		firewall auditFirewall
		err      error
		want     Status
	}{
		{"accepting firewall", auditFirewall{Tool: "nftables", Chains: 1}, nil, StatusFail},
		{"dropping firewall", auditFirewall{Tool: "nftables", Chains: 1, Drops: true}, nil, StatusWarn},
		{"unknown firewall", auditFirewall{}, errAuditNeedsRoot, StatusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := auditExposure(auditFacts{Listen: sockets, Firewall: tt.firewall, FirewallErr: tt.err})
			if result.Status != tt.want || !strings.Contains(result.Summary, "redis/6379") || strings.Contains(result.Summary, "postgres") {
				t.Fatalf("got %s %q", result.Status, result.Summary)
			}
		})
	}
	published := []listenSocket{{Proto: "tcp", Address: "0.0.0.0", Port: 5432, Process: "docker-proxy"}}
	if result := auditExposure(auditFacts{Listen: published, Firewall: auditFirewall{Drops: true}}); len(result.Warnings) != 2 {
		t.Fatalf("docker가 publish한 포트에 FORWARD 안내가 없다: %q", result.Warnings)
	}
	if result := auditExposure(auditFacts{Listen: sockets[1:]}); result.Status != StatusPass {
		t.Fatalf("loopback과 ssh만 열린 호스트 = %s %q", result.Status, result.Summary)
	}
}

func TestAuditSSHPassword(t *testing.T) {
	config := func(values map[string]string) sshdConfig {
		sources := map[string]string{}
		for key := range values {
			sources[key] = "/etc/ssh/sshd_config"
		}
		return sshdConfig{values: values, sources: sources}
	}
	tests := []struct {
		name   string
		values map[string]string
		want   Status
	}{
		{"openssh default", map[string]string{}, StatusWarn},
		{"keys only", map[string]string{"passwordauthentication": "no", "kbdinteractiveauthentication": "no", "usepam": "yes"}, StatusPass},
		{"pam keyboard", map[string]string{"passwordauthentication": "no", "usepam": "yes"}, StatusWarn},
		{"empty passwords", map[string]string{"permitemptypasswords": "yes"}, StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := auditSSHPassword(auditFacts{SSHD: config(tt.values)}).Status; got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
	if got := auditSSHPassword(auditFacts{SSHDErr: fs.ErrNotExist, SSHDPath: "/etc/ssh/sshd_config"}).Status; got != StatusSkip {
		t.Fatalf("sshd 없는 호스트 = %s", got)
	}
}

func TestAuditRebootComparesInstalledKernels(t *testing.T) {
	if got := auditReboot(auditFacts{Running: "6.8.0-45-generic", Kernels: []string{"6.8.0-45-generic", "6.8.0-48-generic"}}); got.Status != StatusWarn || !strings.Contains(got.Summary, "6.8.0-48-generic") {
		t.Fatalf("새 커널 = %s %q", got.Status, got.Summary)
	}
	if got := auditReboot(auditFacts{Running: "6.8.0-48-generic", Kernels: []string{"6.8.0-45-generic", "6.8.0-48-generic"}}); got.Status != StatusPass {
		t.Fatalf("최신 커널 = %s %q", got.Status, got.Summary)
	}
	if got := auditReboot(auditFacts{Running: "6.6.87.2-microsoft-standard-WSL2", Kernels: []string{"6.8.0-48-generic"}}); got.Status != StatusPass {
		t.Fatalf("/boot에 없는 커널 = %s %q", got.Status, got.Summary)
	}
}

// 임시 디렉터리를 root로 삼아 수집부터 판정까지 한 번에 지난다. 파일 경로와 mode 해석이 틀리면 판정이 어긋난다.
func TestCollectAuditFactsFromFiles(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\ntoor:x:0:0::/root:/bin/sh\ndaemon:x:1:1::/usr/sbin:/usr/sbin/nologin\nalice:x:1000:1000::/home/alice:/bin/bash\nbob:x:1001:999::/home/bob:/bin/bash\n", 0o644)
	writeAuditFile(t, root, "/etc/group", "root:x:0:\ndocker:x:999:alice\nlxd:x:998:\n", 0o644)
	writeAuditFile(t, root, "/etc/shadow", "root:!:19000::::::\nalice::19000::::::\n", 0o606)
	writeAuditFile(t, root, "/home/alice/.ssh/authorized_keys", "ssh-ed25519 AAAA\n", 0o666)
	writeAuditFile(t, root, "/proc/sys/net/ipv4/ip_forward", "1\n", 0o644)
	writeAuditFile(t, root, "/proc/sys/kernel/randomize_va_space", "2\n", 0o644)
	writeAuditFile(t, root, "/proc/sys/kernel/dmesg_restrict", "0\n", 0o644)
	writeAuditFile(t, root, "/proc/sys/fs/suid_dumpable", "2\n", 0o644)
	writeAuditFile(t, root, "/var/run/reboot-required", "", 0o644)
	writeAuditFile(t, root, "/var/run/reboot-required.pkgs", "linux-base\nlinux-base\n", 0o644)
	writeAuditFile(t, root, "/boot/vmlinuz-6.8.0-45-generic", "", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "usr/local/bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "usr/local/bin"), 0o777); err != nil {
		t.Fatal(err)
	}
	sources := auditSources{
		root:     root,
		euid:     0,
		path:     "/usr/bin:.",
		listen:   func() ([]listenSocket, error) { return nil, nil },
		firewall: func(context.Context) (auditFirewall, error) { return auditFirewall{}, errAuditNoFirewallTool },
		kernel:   func() string { return "6.8.0-45-generic" },
		owner:    func(fs.FileInfo) (uint32, bool) { return 0, false },
	}
	results := evaluateAudit(collectAuditFacts(context.Background(), sources))
	want := map[string]Status{
		"net.exposed":        StatusPass,
		"net.firewall":       StatusSkip,
		"net.ip_forward":     StatusWarn,
		"ssh.root_login":     StatusSkip,
		"ssh.key_files":      StatusFail,
		"user.uid0":          StatusFail,
		"user.empty_pass":    StatusFail,
		"user.sudo_nopass":   StatusSkip,
		"user.root_groups":   StatusWarn,
		"file.sensitive":     StatusFail,
		"file.path_writable": StatusFail,
		"kernel.sysctl":      StatusWarn,
		"patch.reboot":       StatusWarn,
	}
	for id, status := range want {
		if result := auditResultByID(t, results, id); result.Status != status {
			t.Errorf("%s = %s %q, want %s", id, result.Status, result.Summary, status)
		}
	}
	if summary := auditResultByID(t, results, "user.root_groups").Summary; !strings.Contains(summary, "docker: alice, bob") {
		t.Errorf("primary group 멤버를 놓쳤다: %q", summary)
	}
	if summary := auditResultByID(t, results, "file.path_writable").Summary; !strings.Contains(summary, "/usr/local/bin") || !strings.Contains(summary, `"."`) {
		t.Errorf("PATH 판정 = %q", summary)
	}
	if summary := auditResultByID(t, results, "patch.reboot").Summary; !strings.HasSuffix(summary, "linux-base") {
		t.Errorf("재부팅 패키지 = %q", summary)
	}

	// PATH 없이 실행한 root를 현재 디렉터리가 PATH에 있는 것으로 보지 않는다.
	sources.path = ""
	if summary := auditResultByID(t, evaluateAudit(collectAuditFacts(context.Background(), sources)), "file.path_writable").Summary; strings.Contains(summary, `""`) {
		t.Errorf("빈 PATH를 상대 경로로 보았다: %q", summary)
	}

	// root가 아니면 지금 셸의 PATH는 root의 PATH가 아니므로 보지 않는다.
	sources.euid = 1000
	facts := collectAuditFacts(context.Background(), sources)
	if !errors.Is(facts.FirewallErr, errAuditNeedsRoot) {
		t.Errorf("root가 아닌데 방화벽을 읽었다: %v", facts.FirewallErr)
	}
	for _, entry := range facts.PathBad {
		if entry == `"."` {
			t.Error("root가 아닌 셸의 PATH를 root PATH로 보았다")
		}
	}
}

func linkAuditNamespace(t *testing.T, root, path, target string) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// 임시 root에 /proc과 docker.sock을 만들어 container 점검을 수집부터 판정까지 지난다.
func TestAuditContainerChecks(t *testing.T) {
	const allCaps = "000001ffffffffff"
	const dockerDefault = "00000000a80425fb"
	tests := []struct {
		name      string
		container string
		euid      int
		capEff    string
		lastCap   string
		pidNS     string
		selfNet   string
		initNet   string
		sock      string
		want      map[string]Status
		summary   map[string]string
		warns     int
	}{
		{
			name: "privileged with host namespaces and socket", container: "docker", euid: 0,
			capEff: allCaps, lastCap: "40", pidNS: "pid:[4026531836]", selfNet: "net:[4026531840]", initNet: "net:[4026531840]", sock: "socket",
			want:    map[string]Status{"ctr.privileged": StatusFail, "ctr.docker_sock": StatusFail, "ctr.root_user": StatusWarn, "ctr.host_ns": StatusFail},
			summary: map[string]string{"ctr.privileged": "0x" + allCaps, "ctr.docker_sock": "/var/run/docker.sock", "ctr.host_ns": "network"},
		},
		{
			name: "docker default caps in own namespace", container: "docker", euid: 1000,
			capEff: dockerDefault, lastCap: "40", pidNS: "pid:[4026532300]", selfNet: "net:[4026532303]", initNet: "net:[4026532303]", sock: "file",
			want:    map[string]Status{"ctr.privileged": StatusPass, "ctr.docker_sock": StatusPass, "ctr.root_user": StatusPass, "ctr.host_ns": StatusPass},
			summary: map[string]string{"ctr.privileged": "0x" + dockerDefault, "ctr.root_user": "1000"},
		},
		{
			name: "host pid without host network", container: "kubernetes", euid: 0,
			capEff: dockerDefault, lastCap: "40", pidNS: "pid:[4026531836]", selfNet: "net:[4026532303]", initNet: "net:[4026531840]",
			want: map[string]Status{"ctr.host_ns": StatusFail},
		},
		{
			name: "host pid with unreadable init net", container: "podman", euid: 0,
			capEff: dockerDefault, lastCap: "40", pidNS: "pid:[4026531836]", selfNet: "net:[4026531840]",
			want: map[string]Status{"ctr.host_ns": StatusFail}, warns: 1,
		},
		{
			name: "malformed CapEff", container: "docker", euid: 0,
			capEff: "zz", lastCap: "40", pidNS: "pid:[4026532300]",
			want:    map[string]Status{"ctr.privileged": StatusSkip},
			summary: map[string]string{"ctr.privileged": "/proc/self/status"},
		},
		{
			name: "missing CapEff line", container: "docker", euid: 0, lastCap: "40", pidNS: "pid:[4026532300]",
			want:    map[string]Status{"ctr.privileged": StatusSkip},
			summary: map[string]string{"ctr.privileged": "/proc/self/status"},
		},
		{
			name: "cap_last_cap out of range", container: "docker", euid: 0, capEff: allCaps, lastCap: "64", pidNS: "pid:[4026532300]",
			want:    map[string]Status{"ctr.privileged": StatusSkip},
			summary: map[string]string{"ctr.privileged": "/proc/sys/kernel/cap_last_cap"},
		},
		{
			name: "missing pid namespace link", container: "docker", euid: 0, capEff: allCaps,
			want:    map[string]Status{"ctr.privileged": StatusSkip, "ctr.host_ns": StatusSkip},
			summary: map[string]string{"ctr.privileged": "/proc/sys/kernel/cap_last_cap", "ctr.host_ns": "/proc/self/ns/pid"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			status := "Name:\tedc\nCapInh:\t0000000000000000\nCapBnd:\t000001ffffffffff\n"
			if tt.capEff != "" {
				status += "CapEff:\t" + tt.capEff + "\n"
			}
			writeAuditFile(t, root, "/proc/self/status", status, 0o644)
			if tt.lastCap != "" {
				writeAuditFile(t, root, "/proc/sys/kernel/cap_last_cap", tt.lastCap+"\n", 0o644)
			}
			for path, target := range map[string]string{"/proc/self/ns/pid": tt.pidNS, "/proc/self/ns/net": tt.selfNet, "/proc/1/ns/net": tt.initNet} {
				if target != "" {
					linkAuditNamespace(t, root, path, target)
				}
			}
			switch tt.sock {
			case "file":
				writeAuditFile(t, root, "/var/run/docker.sock", "", 0o644)
			case "socket":
				if err := os.MkdirAll(filepath.Join(root, "var/run"), 0o755); err != nil {
					t.Fatal(err)
				}
				// unix socket 경로는 100바이트 안팎으로 제한된다. macOS의 임시 디렉터리는 길어서 상대 경로로 만든다.
				t.Chdir(root)
				listener, err := net.Listen("unix", "var/run/docker.sock")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { listener.Close() })
			}
			sources := auditSources{
				root:      root,
				euid:      tt.euid,
				listen:    func() ([]listenSocket, error) { return nil, nil },
				firewall:  func(context.Context) (auditFirewall, error) { return auditFirewall{}, errAuditNoFirewallTool },
				kernel:    func() string { return "" },
				owner:     func(fs.FileInfo) (uint32, bool) { return 0, false },
				container: tt.container,
			}
			results := evaluateAudit(collectAuditFacts(context.Background(), sources))
			for id, want := range tt.want {
				if result := auditResultByID(t, results, id); result.Status != want {
					t.Errorf("%s = %s %q, want %s", id, result.Status, result.Summary, want)
				}
			}
			for id, part := range tt.summary {
				if summary := auditResultByID(t, results, id).Summary; !strings.Contains(summary, part) {
					t.Errorf("%s summary %q lacks %q", id, summary, part)
				}
			}
			if warnings := auditResultByID(t, results, "ctr.host_ns").Warnings; len(warnings) != tt.warns {
				t.Errorf("ctr.host_ns warnings = %q", warnings)
			}
		})
	}
	t.Run("host pid without host network names no network", func(t *testing.T) {
		result := auditHostNamespace(auditFacts{Container: "docker", PIDNS: auditHostPIDNamespace})
		if result.Status != StatusFail || result.Summary != T("audit.ctr.host_pid") {
			t.Fatalf("got %s %q", result.Status, result.Summary)
		}
	})
}

func TestAuditOmitsContainerChecksOnHost(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/proc/self/status", "CapEff:\t000001ffffffffff\n", 0o644)
	writeAuditFile(t, root, "/proc/sys/kernel/cap_last_cap", "40\n", 0o644)
	sources := auditSources{
		root:     root,
		listen:   func() ([]listenSocket, error) { return nil, nil },
		firewall: func(context.Context) (auditFirewall, error) { return auditFirewall{}, errAuditNoFirewallTool },
		kernel:   func() string { return "" },
		owner:    func(fs.FileInfo) (uint32, bool) { return 0, false },
	}
	for _, result := range evaluateAudit(collectAuditFacts(context.Background(), sources)) {
		if strings.HasPrefix(result.Probe, "audit.ctr.") {
			t.Errorf("container 밖에서 %s를 보였다", result.Probe)
		}
	}
}

func TestAuditResultIDsFitTheLabel(t *testing.T) {
	results := evaluateAudit(auditFacts{Container: "docker", CapErr: errors.New("x"), PIDNSErr: errors.New("x")})
	for _, result := range results {
		if len(result.Probe) > 24 {
			t.Errorf("%s는 %d자로 24자를 넘는다", result.Probe, len(result.Probe))
		}
	}
}

func TestAuditCheckIDsMatchEvaluate(t *testing.T) {
	var ids []string
	for _, result := range evaluateAudit(auditFacts{Container: "docker", CapErr: errors.New("x"), PIDNSErr: errors.New("x")}) {
		ids = append(ids, strings.TrimPrefix(result.Probe, "audit."))
	}
	if !slices.Equal(ids, auditCheckIDs) {
		t.Fatalf("evaluateAudit ids = %q, auditCheckIDs = %q", ids, auditCheckIDs)
	}
}

func TestApplyAuditAcceptKeepsFindingVisible(t *testing.T) {
	failing := auditResult("user.uid0", StatusFail, "UID 0 also belongs to toor")
	failing.Error = &DiagnosticError{Kind: "account", Message: failing.Summary}
	failing.Next = "edit /etc/passwd"
	passing := auditResult("kernel.sysctl", StatusPass, "8 hardening values match")
	results := applyAuditAccept([]Result{failing, passing}, []string{"user.uid0", "kernel.sysctl"})

	accepted := results[0]
	if accepted.Status != StatusSkip || accepted.Error != nil || accepted.Next != "" || !strings.Contains(accepted.Summary, "toor") {
		t.Fatalf("accepted = %#v", accepted)
	}
	if accepted.Summary != T("audit.accepted", StatusFail, failing.Summary) {
		t.Fatalf("accepted summary = %q", accepted.Summary)
	}
	if accepted.Metrics["accepted"] != true || accepted.Metrics["accepted_status"] != StatusFail {
		t.Fatalf("accepted metrics = %#v", accepted.Metrics)
	}
	if !reflect.DeepEqual(results[1], passing) {
		t.Fatalf("pass result changed: %#v", results[1])
	}
	if code := exitCode(results); code != 0 {
		t.Fatalf("exit code with an accepted fail = %d", code)
	}
	data, err := json.Marshal(accepted)
	if err != nil || !strings.Contains(string(data), `"accepted":true`) || !strings.Contains(string(data), `"accepted_status":"fail"`) {
		t.Fatalf("JSON = %s, error = %v", data, err)
	}
	if line := formatResultLine(accepted, false); !strings.HasPrefix(line, "SKIP") || !strings.Contains(line, "accepted (fail)") {
		t.Fatalf("terminal line = %q", line)
	}
}

func TestAuditExposureAllowPorts(t *testing.T) {
	sockets := []listenSocket{
		{Proto: "tcp", Address: "0.0.0.0", Port: 6379, Process: "redis-server"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 5432, Process: "postgres"},
	}
	open := auditFirewall{Tool: "nftables", Chains: 1}

	all := auditExposure(auditFacts{Listen: sockets, Firewall: open, AllowPorts: []int{5432, 6379}})
	if all.Status != StatusSkip || all.Metrics["accepted"] != true || all.Metrics["accepted_status"] != StatusFail || all.Error != nil {
		t.Fatalf("all allowed = %s %q %#v", all.Status, all.Summary, all.Metrics)
	}
	if !reflect.DeepEqual(all.Metrics["allowed_ports"], []int{6379, 5432}) || !strings.Contains(all.Summary, "redis/6379") {
		t.Fatalf("all allowed = %q %#v", all.Summary, all.Metrics)
	}

	partial := auditExposure(auditFacts{Listen: sockets, Firewall: open, AllowPorts: []int{5432}})
	if partial.Status != StatusFail || strings.Contains(partial.Summary, "postgres") || !strings.Contains(partial.Summary, "redis/6379") {
		t.Fatalf("partial = %s %q", partial.Status, partial.Summary)
	}
	if !reflect.DeepEqual(partial.Metrics["allowed_ports"], []int{5432}) || !slices.Contains(partial.Warnings, T("audit.net.allowed_ports", "postgres/5432")) {
		t.Fatalf("partial metrics = %#v warnings = %q", partial.Metrics, partial.Warnings)
	}
}
