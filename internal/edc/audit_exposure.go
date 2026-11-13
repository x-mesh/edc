package edc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// IPv6 방화벽, sshd 포워딩 설정, container 탈출 설정, 지속 실행 위치를 본다. 2026-11-13 jw-server, okrr-intranet-1,
// okrr-intranet-slackbot-1에서 IPv6·포워딩·container 신호는 0건이었고, 지속 실행 위치는 jw-server에서 관리자가 바꾼 3건이었다.

// auditPublicIPv6는 인터넷에서 닿는 IPv6 주소다. ULA(fc00::/7, Tailscale의 fd7a::/48 포함)와 link-local은 뺀다.
func auditPublicIPv6(addrs []netip.Addr) []netip.Addr {
	var public []netip.Addr
	for _, addr := range addrs {
		addr = addr.WithZone("")
		if addr.Is6() && !addr.Is4In6() && addr.IsGlobalUnicast() && !addr.IsPrivate() {
			public = append(public, addr)
		}
	}
	return public
}

func auditIPv6Open(facts auditFacts) Result {
	const id = "net.ipv6_open"
	if errors.Is(facts.FirewallErr, errAuditNoFirewallTool) {
		return auditResult(id, StatusSkip, T("audit.skip.no_firewall_tool"))
	}
	if facts.FirewallErr != nil {
		return auditSkip(id, "nft/ip6tables-save", facts.FirewallErr)
	}
	if facts.AddrsErr != nil {
		return auditSkip(id, "interfaces", facts.AddrsErr)
	}
	public := auditPublicIPv6(facts.Addrs)
	if len(public) == 0 {
		return auditResult(id, StatusPass, T("audit.net.ipv6_none"))
	}
	addresses := make([]string, 0, len(public))
	for _, addr := range public {
		addresses = append(addresses, addr.String())
	}
	firewall := facts.Firewall
	if !firewall.V6Known {
		return auditResult(id, StatusSkip, T("audit.net.ipv6_unknown"))
	}
	if firewall.V6Drops {
		return auditResult(id, StatusPass, T("audit.net.ipv6_drops", firewall.V6Chains, firewall.V6Rules))
	}
	var listeners []string
	for _, socket := range facts.Listen {
		if socket.Proto != "tcp" && socket.Proto != "udp" {
			continue
		}
		// *는 IPv4도 받는 IPv6 wildcard이고, ::는 IPv6만 받는 wildcard다.
		address, err := netip.ParseAddr(socket.Address)
		if socket.Address != "*" && (err != nil || !(address == netip.IPv6Unspecified() || slices.Contains(public, address.WithZone("")))) {
			continue
		}
		line := fmt.Sprintf("%s [%s]:%d", socket.Proto, socket.Address, socket.Port)
		if socket.Process != "" {
			line += "  " + socket.Process
		}
		if !slices.Contains(listeners, line) {
			listeners = append(listeners, line)
		}
	}
	if len(listeners) == 0 {
		return auditResult(id, StatusPass, T("audit.net.ipv6_no_listen", strings.Join(addresses, ", ")))
	}
	result := auditResult(id, StatusWarn, T("audit.net.ipv6_open", strings.Join(addresses, ", "), len(listeners)))
	result.Evidence = []Evidence{
		{Label: T("audit.evidence.values"), Value: T("audit.net.ipv6_rules", firewall.Tool, firewall.V6Chains, firewall.V6Rules) + "\n" + T("audit.net.ipv4_rules", firewall.Drops)},
		{Label: T("audit.evidence.sockets"), Value: strings.Join(listeners, "\n")},
	}
	result.Warnings = []string{T("audit.net.cloud_note")}
	result.Next = "sudo nft list ruleset | grep -B2 -A6 'hook input'"
	if firewall.Tool == "iptables" {
		result.Next = "sudo ip6tables -S INPUT"
	}
	return result
}

// auditDockerPublished는 docker가 모든 주소에 publish한 포트다. 방화벽이 들어오는 트래픽을 막는다고 판정해도 이 포트는 DNAT되어
// FORWARD를 지나므로 INPUT 차단이 적용되지 않는다. 민감 포트는 net.exposed가 따로 판정한다.
func auditDockerPublished(facts auditFacts) string {
	if facts.FirewallErr != nil || !facts.Firewall.Drops {
		return ""
	}
	var ports []int
	for _, socket := range facts.Listen {
		if _, sensitive := auditSensitivePorts[socket.Port]; sensitive || socket.Process != "docker-proxy" || listenBindingScope(socket.Proto, socket.Address) != "all" {
			continue
		}
		if !slices.Contains(ports, socket.Port) {
			ports = append(ports, socket.Port)
		}
	}
	if len(ports) == 0 {
		return ""
	}
	slices.Sort(ports)
	labels := make([]string, 0, len(ports))
	for _, port := range ports {
		labels = append(labels, strconv.Itoa(port))
	}
	return T("audit.net.docker_published", strings.Join(labels, ", "))
}

func auditSSHForwarding(facts auditFacts) Result {
	const id = "ssh.forwarding"
	if result, skip := auditSSHConfigSkip(id, facts); skip {
		return result
	}
	gateway, _ := facts.SSHD.value("gatewayports", "no")
	tunnel, _ := facts.SSHD.value("permittunnel", "no")
	settings := []sshdDefault{{"gatewayports", "no", ""}, {"permittunnel", "no", ""}, {"allowtcpforwarding", "yes", ""}}
	var open []string
	// GatewayPorts yes면 ssh -R로 연 포트가 loopback이 아니라 모든 인터페이스에서 들린다. clientspecified는 client가 주소를 고른다.
	if gateway == "yes" || gateway == "clientspecified" {
		settings[0].advice = T("audit.ssh.advice_gateway")
		open = append(open, "GatewayPorts "+gateway)
	}
	if tunnel != "no" {
		settings[1].advice = T("audit.ssh.advice_tunnel")
		open = append(open, "PermitTunnel "+tunnel)
	}
	var result Result
	if len(open) == 0 {
		result = auditResult(id, StatusPass, T("audit.ssh.forwarding_ok", gateway, tunnel))
	} else {
		result = auditResult(id, StatusWarn, T("audit.ssh.forwarding_bad", strings.Join(open, ", ")))
		result.Next = "sudo sshd -T | grep -Ei '^(gatewayports|permittunnel|allowtcpforwarding) '"
	}
	result.Evidence = []Evidence{auditSSHDEvidence(facts.SSHD, settings...)}
	return auditSSHMatchWarning(result, facts)
}

type auditContainerMount struct {
	Source, Destination string
	RW                  bool
}

// auditContainer는 실행 중인 docker container에서 호스트로 빠져나올 수 있는 설정이다.
type auditContainer struct {
	Name, Image string
	Privileged  bool
	PidMode     string
	CapAdd      []string
	Mounts      []auditContainerMount
}

type auditAPIContainer struct {
	Name   string `json:"Name"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	HostConfig struct {
		Privileged bool     `json:"Privileged"`
		PidMode    string   `json:"PidMode"`
		CapAdd     []string `json:"CapAdd"`
	} `json:"HostConfig"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

// readHostAuditContainers는 docker API로 실행 중인 container를 읽는다. socket이 없으면 fs.ErrNotExist다.
func readHostAuditContainers(ctx context.Context) ([]auditContainer, error) {
	socket := ctrDefaultSocket()
	if _, err := os.Stat(socket); err != nil {
		return nil, err
	}
	engine := newCtrEngine(socket)
	data, err := engine.get(ctx, "/containers/json", ctrJSONLimit)
	if err != nil {
		return nil, err
	}
	var rows []ctrAPIContainer
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	containers := make([]auditContainer, 0, len(rows))
	for _, row := range rows {
		data, err := engine.get(ctx, "/containers/"+ctrSegment(row.ID)+"/json", ctrJSONLimit)
		if err != nil {
			// 그 사이에 끝난 container는 볼 것이 없다.
			if ctrHTTPStatus(err, 404) {
				continue
			}
			return nil, err
		}
		var inspect auditAPIContainer
		if err := json.Unmarshal(data, &inspect); err != nil {
			return nil, err
		}
		container := auditContainer{Name: strings.TrimPrefix(inspect.Name, "/"), Image: inspect.Config.Image, Privileged: inspect.HostConfig.Privileged,
			PidMode: inspect.HostConfig.PidMode, CapAdd: inspect.HostConfig.CapAdd}
		for _, mount := range inspect.Mounts {
			container.Mounts = append(container.Mounts, auditContainerMount{Source: mount.Source, Destination: mount.Destination, RW: mount.RW})
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// auditRiskyCaps는 container에서 호스트로 빠져나오는 데 쓰이는 capability다. ALL은 모든 capability다.
var auditRiskyCaps = []string{"ALL", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_RAWIO", "DAC_READ_SEARCH"}

// auditContainerRisks는 호스트 root로 이어지는 설정이다. host network는 격리를 줄이지만 그것만으로 빠져나오지는 못해 보지 않는다.
func auditContainerRisks(container auditContainer) []string {
	var risks []string
	if container.Privileged {
		risks = append(risks, T("audit.ctr.risk_privileged"))
	}
	if container.PidMode == "host" {
		risks = append(risks, T("audit.ctr.risk_host_pid"))
	}
	for _, capability := range container.CapAdd {
		name := strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
		if slices.Contains(auditRiskyCaps, name) {
			risks = append(risks, T("audit.ctr.risk_cap", name))
		}
	}
	for _, mount := range container.Mounts {
		switch {
		case slices.Contains(auditDockerSockets, mount.Source):
			risks = append(risks, T("audit.ctr.risk_docker_sock", mount.Source, mount.Destination))
		case mount.Source == "/" && mount.RW:
			risks = append(risks, T("audit.ctr.risk_root_mount", mount.Destination))
		}
	}
	return risks
}

func auditContainerEscape(facts auditFacts) Result {
	const id = "docker.escape"
	if errors.Is(facts.ContainersErr, fs.ErrNotExist) {
		return auditResult(id, StatusPass, T("audit.ctr.escape_none"))
	}
	if facts.ContainersErr != nil {
		return auditSkip(id, "docker", facts.ContainersErr)
	}
	var items []string
	var first string
	for _, container := range facts.Containers {
		risks := auditContainerRisks(container)
		if len(risks) == 0 {
			continue
		}
		first = cmp.Or(first, container.Name)
		items = append(items, auditItem(container.Name+"  "+container.Image, nil, risks))
	}
	if len(items) == 0 {
		return auditResult(id, StatusPass, T("audit.ctr.escape_ok", len(facts.Containers)))
	}
	result := auditResult(id, StatusWarn, T("audit.ctr.escape_bad", len(items)))
	result.Evidence = []Evidence{{Label: T("audit.evidence.containers"), Value: strings.Join(items, "\n")}}
	result.Next = "docker inspect " + first
	return result
}

// 지속 실행 위치는 로그인, 부팅, 정해진 시각에 명령을 실행하는 파일이다. 침입자는 여기에 명령을 넣어 다시 들어온다.
var (
	auditPersistFiles = []string{"/etc/crontab", "/etc/rc.local", "/etc/profile", "/etc/bash.bashrc", "/etc/environment",
		"/etc/ld.so.conf", "/etc/sudoers", "/etc/ssh/sshd_config"}
	auditPersistDirs = []string{"/etc/cron.d", "/etc/cron.hourly", "/etc/cron.daily", "/etc/cron.weekly", "/etc/cron.monthly",
		"/var/spool/cron", "/etc/systemd/system", "/etc/init.d", "/etc/profile.d", "/etc/ld.so.conf.d", "/etc/sudoers.d",
		"/etc/pam.d", "/etc/udev/rules.d", "/etc/update-motd.d", "/etc/ssh/sshd_config.d", "/etc/xdg/autostart"}
	auditPersistHomeFiles = []string{".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshrc", ".zprofile", ".ssh/rc"}
	auditPersistHomeDirs  = []string{".config/systemd/user", ".config/autostart"}
)

// auditPersistWindow는 바뀐 지속 실행 위치를 알리는 기간이다. ssh.key_changed와 같다.
const auditPersistWindow = 7 * 24 * time.Hour

// auditUpgradeSkew는 패키지 업그레이드가 그 패키지의 파일을 바꾼 것으로 보는 시각 차이다. postinst가 몇 분 뒤에 권한을 바꾸기도 한다.
const auditUpgradeSkew = time.Hour

type auditPersistFile struct {
	Path    string
	Changed time.Time
	Owner   string
}

type auditPersist struct {
	Checked  int
	Files    []auditPersistFile
	Upgraded int
}

func collectAuditPersist(sources auditSources, facts *auditFacts) {
	persist := &facts.Persist
	since := facts.Now.Add(-auditPersistWindow)
	var changed []auditPersistFile
	look := func(path string) {
		info, err := os.Lstat(sources.file(path))
		if err != nil || info.IsDir() {
			return
		}
		persist.Checked++
		at := info.ModTime()
		if sources.ctime != nil {
			if ctime, ok := sources.ctime(info); ok && ctime.After(at) {
				at = ctime
			}
		}
		if !at.Before(since) {
			changed = append(changed, auditPersistFile{Path: path, Changed: at})
		}
	}
	walk := func(dir string) {
		_ = filepath.WalkDir(sources.file(dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(sources.file("/"), path)
			if err == nil {
				look("/" + relative)
			}
			return nil
		})
	}
	for _, path := range auditPersistFiles {
		look(path)
	}
	for _, dir := range auditPersistDirs {
		walk(dir)
	}
	if facts.PasswdErr == nil {
		for _, account := range facts.Passwd {
			if (account.UID != 0 && account.UID < 1000) || account.Home == "" || strings.HasSuffix(account.Shell, "nologin") || strings.HasSuffix(account.Shell, "false") {
				continue
			}
			for _, name := range auditPersistHomeFiles {
				look(filepath.Join(account.Home, name))
			}
			for _, name := range auditPersistHomeDirs {
				walk(filepath.Join(account.Home, name))
			}
		}
	}
	if len(changed) == 0 {
		return
	}
	paths := make([]string, 0, len(changed))
	for _, file := range changed {
		paths = append(paths, file.Path)
	}
	owners, _ := auditDpkgOwners(sources, paths)
	for _, file := range changed {
		file.Owner = owners[file.Path]
		// 패키지 업그레이드는 그 패키지의 설정과 스크립트를 함께 바꾼다. dpkg가 .list를 고친 때와 가까우면 업그레이드가 바꾼 것이다.
		if file.Owner != "" && auditChangedWithPackage(sources, file.Owner, file.Changed) {
			persist.Upgraded++
			continue
		}
		persist.Files = append(persist.Files, file)
	}
	slices.SortFunc(persist.Files, func(a, b auditPersistFile) int { return b.Changed.Compare(a.Changed) })
}

func auditChangedWithPackage(sources auditSources, owner string, changed time.Time) bool {
	lists, _ := filepath.Glob(sources.file("/var/lib/dpkg/info/" + owner + ".list"))
	arch, _ := filepath.Glob(sources.file("/var/lib/dpkg/info/" + owner + ":*.list"))
	for _, list := range append(lists, arch...) {
		info, err := os.Stat(list)
		if err == nil && changed.Sub(info.ModTime()).Abs() <= auditUpgradeSkew {
			return true
		}
	}
	return false
}

// auditLoginBefore는 그 시각 바로 전의 wtmp 로그인이다. 그 로그인에서 파일을 바꿨을 가능성이 가장 크다.
func auditLoginBefore(facts auditFacts, at time.Time) string {
	if facts.LoginErr != nil {
		return ""
	}
	var previous auditLogin
	found := false
	for _, session := range facts.Logins.Sessions {
		if !session.Start.After(at) && (!found || session.Start.After(previous.Start)) {
			previous, found = session, true
		}
	}
	if !found {
		return T("audit.ssh.key_no_login")
	}
	host := previous.Host
	if host == "" {
		host = "-"
	}
	return T("audit.ssh.key_login", previous.User, auditLoginTime(previous.Start), previous.Line, host)
}

func auditPersistChanged(facts auditFacts) Result {
	const id = "persist.changed"
	persist := facts.Persist
	var result Result
	if len(persist.Files) == 0 {
		result = auditResult(id, StatusPass, T("audit.persist.changed_ok", persist.Checked))
	} else {
		result = auditResult(id, StatusWarn, T("audit.persist.changed_bad", len(persist.Files)))
		items := make([]string, 0, min(len(persist.Files), auditProcessShown)+1)
		for _, file := range persist.Files[:min(len(persist.Files), auditProcessShown)] {
			owner := T("audit.proc.lib_no_package")
			if file.Owner != "" {
				owner = T("audit.proc.lib_package", file.Owner)
			}
			items = append(items, auditItem(file.Path, []string{T("audit.ssh.key_changed_at", auditLoginTime(file.Changed)), auditLoginBefore(facts, file.Changed)}, []string{owner}))
		}
		if extra := len(persist.Files) - auditProcessShown; extra > 0 {
			items = append(items, fmt.Sprintf("+%d", extra))
		}
		result.Evidence = []Evidence{{Label: T("audit.evidence.files"), Value: strings.Join(items, "\n")}}
		result.Next = "ls -la --time-style=full-iso " + persist.Files[0].Path
	}
	if persist.Upgraded > 0 {
		result.Warnings = append(result.Warnings, T("audit.persist.upgraded", persist.Upgraded))
	}
	if !facts.Root {
		result.Warnings = append(result.Warnings, T("audit.persist.not_root"))
	}
	return result
}
