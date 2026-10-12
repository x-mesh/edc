package edc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	// sudo는 '.'이 들어간 이름을 읽지 않는다.
	writeAuditFile(t, root, "/etc/sudoers.d/old.bak", "old ALL=(ALL) NOPASSWD: ALL\n", 0o440)
	rules, err := readSudoNoPasswdRules(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || !strings.Contains(rules[0], "ubuntu") || !strings.Contains(rules[1], "deploy") {
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
		"net.exposed":            StatusPass,
		"net.firewall":           StatusSkip,
		"net.ip_forward":         StatusWarn,
		"ssh.root_login":         StatusSkip,
		"ssh.key_files":    StatusFail,
		"user.uid0":           StatusFail,
		"user.empty_pass": StatusFail,
		"user.sudo_nopass":  StatusSkip,
		"user.root_groups":    StatusWarn,
		"file.sensitive":         StatusFail,
		"file.path_writable":     StatusFail,
		"kernel.sysctl":          StatusWarn,
		"patch.reboot":           StatusWarn,
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
