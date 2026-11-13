package edc

import (
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// ip 표만 drop하면 IPv6는 열려 있다. inet 표는 두 계열을 함께 거른다. Oracle Cloud와 Ubuntu에서 흔한 모양이다.
func TestParseNftInputChainsSplitsIPv6(t *testing.T) {
	v4Only := parseNftInputChains(`table ip filter {
	chain INPUT {
		type filter hook input priority filter; policy drop;
		tcp dport 22 accept
	}
}
table ip6 filter {
	chain INPUT {
		type filter hook input priority filter; policy accept;
		jump ts-input
	}
}`)
	if !v4Only.Drops || v4Only.Chains != 2 || !v4Only.V6Known || v4Only.V6Chains != 1 || v4Only.V6Rules != 1 || v4Only.V6Drops {
		t.Fatalf("ip + ip6 = %+v", v4Only)
	}
	both := parseNftInputChains(`table inet filter {
	chain input {
		type filter hook input priority filter; policy accept;
		ct state established accept
		drop
	}
}`)
	if !both.Drops || !both.V6Drops || both.V6Chains != 1 {
		t.Fatalf("inet = %+v", both)
	}
}

func TestAuditIPv6Open(t *testing.T) {
	public := netip.MustParseAddr("2603:c020:4000::51")
	tailscale := netip.MustParseAddr("fd7a:115c:a1e0::ce3a:ee7d")
	listen := []listenSocket{
		{Proto: "tcp", Address: "*", Port: 3000, Process: "docker-proxy"},
		{Proto: "tcp", Address: "::", Port: 111, Process: "rpcbind"},
		{Proto: "tcp", Address: "::1", Port: 5432, Process: "postgres"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 6379, Process: "redis"},
	}
	open := auditFirewall{Tool: "iptables", Drops: true, V6Known: true, V6Chains: 1, V6Rules: 1}
	for name, test := range map[string]struct {
		facts auditFacts
		want  Status
	}{
		"tailscale only":          {auditFacts{Addrs: []netip.Addr{tailscale, netip.MustParseAddr("fe80::1")}, Firewall: open, Listen: listen}, StatusPass},
		"public and open":         {auditFacts{Addrs: []netip.Addr{public, tailscale}, Firewall: open, Listen: listen}, StatusWarn},
		"public, nothing listens": {auditFacts{Addrs: []netip.Addr{public}, Firewall: open, Listen: listen[2:]}, StatusPass},
		"public, ipv6 drops":      {auditFacts{Addrs: []netip.Addr{public}, Firewall: auditFirewall{Tool: "nftables", V6Known: true, V6Drops: true}, Listen: listen}, StatusPass},
		"public, rules unknown":   {auditFacts{Addrs: []netip.Addr{public}, Firewall: auditFirewall{Tool: "iptables", Drops: true}, Listen: listen}, StatusSkip},
		"no addresses":            {auditFacts{AddrsErr: errAuditNoSource}, StatusSkip},
	} {
		if result := auditIPv6Open(test.facts); result.Status != test.want {
			t.Errorf("%s = %+v", name, result)
		}
	}
	result := auditIPv6Open(auditFacts{Addrs: []netip.Addr{public}, Firewall: open, Listen: listen})
	if result.Summary != T("audit.net.ipv6_open", "2603:c020:4000::51", 2) || result.Evidence[1].Value != "tcp [*]:3000  docker-proxy\ntcp [::]:111  rpcbind" || result.Next != "sudo ip6tables -S INPUT" {
		t.Fatalf("result = %+v", result)
	}
}

// 방화벽이 막는다고 판정해도 docker-proxy가 모든 주소에 publish한 포트는 INPUT을 지나지 않는다. 민감 포트는 net.exposed가 판정한다.
func TestAuditDockerPublishedBypassesInput(t *testing.T) {
	listen := []listenSocket{
		{Proto: "tcp", Address: "0.0.0.0", Port: 3000, Process: "docker-proxy"},
		{Proto: "tcp", Address: "::", Port: 3000, Process: "docker-proxy"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 80, Process: "docker-proxy"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 8080, Process: "docker-proxy"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 3306, Process: "docker-proxy"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 443, Process: "nginx"},
	}
	facts := auditFacts{Listen: listen, Firewall: auditFirewall{Drops: true}}
	if got := auditDockerPublished(facts); got != T("audit.net.docker_published", "80, 3000") {
		t.Fatalf("published = %q", got)
	}
	if result := auditExposure(facts); !slices.Contains(result.Warnings, T("audit.net.docker_published", "80, 3000")) {
		t.Fatalf("exposure = %+v", result)
	}
	facts.Firewall.Drops = false
	if got := auditDockerPublished(facts); got != "" {
		t.Fatalf("open firewall = %q", got)
	}
}

func TestAuditSSHForwarding(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/ssh/sshd_config", "GatewayPorts clientspecified\nPermitTunnel yes\n", 0o644)
	config, err := readSSHDConfig(root, "/etc/ssh/sshd_config")
	if err != nil {
		t.Fatal(err)
	}
	result := auditSSHForwarding(auditFacts{SSHD: config, SSHDPath: "/etc/ssh/sshd_config"})
	want := strings.Join([]string{
		"GatewayPorts clientspecified  /etc/ssh/sshd_config:1", auditNotePrefix + T("audit.ssh.advice_gateway"),
		"PermitTunnel yes  /etc/ssh/sshd_config:2", auditNotePrefix + T("audit.ssh.advice_tunnel"),
		"AllowTcpForwarding yes  " + T("audit.ssh.setting_default"),
	}, "\n")
	if result.Status != StatusWarn || result.Summary != T("audit.ssh.forwarding_bad", "GatewayPorts clientspecified, PermitTunnel yes") || result.Evidence[0].Value != want {
		t.Fatalf("result = %+v", result)
	}
	clean, _ := readSSHDConfig(t.TempDir(), "/missing")
	if result := auditSSHForwarding(auditFacts{SSHD: clean, SSHDPath: "/etc/ssh/sshd_config"}); result.Status != StatusPass || result.Summary != T("audit.ssh.forwarding_ok", "no", "no") {
		t.Fatalf("default = %+v", result)
	}
}

func TestAuditContainerEscape(t *testing.T) {
	containers := []auditContainer{
		{Name: "web", Image: "nginx", Mounts: []auditContainerMount{{Source: "/srv/www", Destination: "/usr/share/nginx/html"}}},
		// node-exporter처럼 호스트 /를 읽기 전용으로 쓰는 것은 흔하다. host network도 그것만으로는 빠져나오지 못한다.
		{Name: "node-exporter", Image: "prom/node-exporter", Mounts: []auditContainerMount{{Source: "/", Destination: "/host"}}},
		{Name: "portainer", Image: "portainer/portainer-ce", Mounts: []auditContainerMount{{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}}},
		{Name: "debug", Image: "ubuntu", Privileged: true, PidMode: "host", CapAdd: []string{"CAP_SYS_ADMIN", "NET_BIND_SERVICE"},
			Mounts: []auditContainerMount{{Source: "/", Destination: "/rootfs", RW: true}}},
	}
	result := auditContainerEscape(auditFacts{Containers: containers})
	if result.Status != StatusWarn || result.Summary != T("audit.ctr.escape_bad", 2) || result.Next != "docker inspect portainer" {
		t.Fatalf("result = %+v", result)
	}
	want := "portainer  portainer/portainer-ce\n" + auditNotePrefix + T("audit.ctr.risk_docker_sock", "/var/run/docker.sock", "/var/run/docker.sock") + "\n" +
		"debug  ubuntu\n" + auditNotePrefix + T("audit.ctr.risk_privileged") + "\n" + auditNotePrefix + T("audit.ctr.risk_host_pid") + "\n" +
		auditNotePrefix + T("audit.ctr.risk_cap", "SYS_ADMIN") + "\n" + auditNotePrefix + T("audit.ctr.risk_root_mount", "/rootfs")
	if result.Evidence[0].Value != want {
		t.Fatalf("evidence = %q", result.Evidence[0].Value)
	}
	for name, test := range map[string]struct {
		facts auditFacts
		want  Status
	}{
		"no docker":  {auditFacts{ContainersErr: fs.ErrNotExist}, StatusPass},
		"denied":     {auditFacts{ContainersErr: fs.ErrPermission}, StatusSkip},
		"no source":  {auditFacts{ContainersErr: errAuditNoSource}, StatusSkip},
		"clean only": {auditFacts{Containers: containers[:2]}, StatusPass},
	} {
		if result := auditContainerEscape(test.facts); result.Status != test.want {
			t.Errorf("%s = %+v", name, result)
		}
	}
}

func TestAuditPersistChanged(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	write := func(path string, at time.Time) {
		writeAuditFile(t, root, path, "x\n", 0o644)
		if err := os.Chtimes(filepath.Join(root, path), at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("/etc/cron.d/backdoor", now.Add(-2*time.Hour))
	write("/etc/profile", now.Add(-30*24*time.Hour))
	write("/etc/pam.d/sshd", now.Add(-3*time.Hour))
	// openssh-server 업그레이드가 .list를 고친 때와 같은 때 바뀌었다.
	write("/var/lib/dpkg/info/openssh-server.list", now.Add(-3*time.Hour+5*time.Minute))
	writeAuditFile(t, root, "/var/lib/dpkg/info/openssh-server.list", "/etc/pam.d/sshd\n", 0o644)
	if err := os.Chtimes(filepath.Join(root, "var/lib/dpkg/info/openssh-server.list"), now.Add(-3*time.Hour+5*time.Minute), now.Add(-3*time.Hour+5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	write("/home/alice/.bashrc", now.Add(-time.Hour))
	write("/home/svc/.bashrc", now.Add(-time.Hour))
	write("/root/.ssh/rc", now.Add(-24*time.Hour))
	if err := os.MkdirAll(filepath.Join(root, "etc/systemd/system/multi-user.target.wants"), 0o755); err != nil {
		t.Fatal(err)
	}
	// systemctl enable이 만든 symlink다. 가리키는 unit이 없어도 symlink 자신의 시각을 본다.
	if err := os.Symlink("/etc/systemd/system/miner.service", filepath.Join(root, "etc/systemd/system/multi-user.target.wants/miner.service")); err != nil {
		t.Fatal(err)
	}
	facts := auditFacts{Root: true, Now: now, Passwd: []auditPasswd{
		{Name: "root", UID: 0, Home: "/root", Shell: "/bin/bash"},
		{Name: "alice", UID: 1001, Home: "/home/alice", Shell: "/bin/bash"},
		{Name: "svc", UID: 998, Home: "/home/svc", Shell: "/usr/sbin/nologin"},
	}, Logins: auditLoginRecords{Sessions: []auditLogin{{User: "root", Line: "pts/0", Host: "203.0.113.7", Start: now.Add(-5 * time.Hour)}}}}
	collectAuditPersist(auditSources{root: root}, &facts)
	var paths []string
	for _, file := range facts.Persist.Files {
		paths = append(paths, file.Path)
	}
	slices.Sort(paths)
	want := []string{"/etc/cron.d/backdoor", "/etc/systemd/system/multi-user.target.wants/miner.service", "/home/alice/.bashrc", "/root/.ssh/rc"}
	if !reflect.DeepEqual(paths, want) || facts.Persist.Upgraded != 1 {
		t.Fatalf("paths = %q, upgraded = %d", paths, facts.Persist.Upgraded)
	}
	result := auditPersistChanged(facts)
	if result.Status != StatusWarn || result.Summary != T("audit.persist.changed_bad", 4) || !slices.Contains(result.Warnings, T("audit.persist.upgraded", 1)) {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Evidence[0].Value, "/etc/cron.d/backdoor\n"+auditDetailPrefix+T("audit.ssh.key_changed_at", auditLoginTime(now.Add(-2*time.Hour)))+"\n"+
		auditDetailPrefix+T("audit.ssh.key_login", "root", auditLoginTime(now.Add(-5*time.Hour)), "pts/0", "203.0.113.7")) {
		t.Fatalf("evidence = %q", result.Evidence[0].Value)
	}
	quiet := auditFacts{Now: now.Add(30 * 24 * time.Hour)}
	collectAuditPersist(auditSources{root: root}, &quiet)
	if result := auditPersistChanged(quiet); result.Status != StatusPass || !slices.Contains(result.Warnings, T("audit.persist.not_root")) {
		t.Fatalf("quiet = %+v", result)
	}
}
