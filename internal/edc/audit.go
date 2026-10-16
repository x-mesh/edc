package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// audit는 호스트의 설정과 노출 상태를 읽기만 하는 점검이다. 설정을 바꾸지 않고 네트워크로 나가지 않는다.
// CVE 대조나 파일 시스템 전체 스캔은 하지 않는다. 몇 초 안에 끝나고 판정 근거를 보일 수 있는 항목만 둔다.

// errAuditNeedsRoot는 root가 아니라 읽지 않은 원천이다. 확인하지 못한 항목을 통과로 보이지 않게 skip으로 둔다.
var errAuditNeedsRoot = errors.New("needs root")

// errAuditNoFirewallTool은 nft와 iptables-save가 모두 없어서 방화벽 규칙을 읽지 못한 경우다.
var errAuditNoFirewallTool = errors.New("no firewall tool")

// auditSensitivePorts는 인터넷에 열려 있으면 거의 언제나 사고가 되는 포트다. 인증이 없거나 약한 기본값으로 배포되는 서비스다.
var auditSensitivePorts = map[int]string{
	2375:  "docker",
	2379:  "etcd",
	3306:  "mysql",
	5432:  "postgres",
	6379:  "redis",
	9200:  "elasticsearch",
	10250: "kubelet",
	11211: "memcached",
	27017: "mongodb",
}

// auditDefaultPath는 sudo의 secure_path와 배포판 root PATH가 공통으로 쓰는 값이다. root가 아닐 때는 이 경로만 본다.
const auditDefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// auditSysctl은 권장 값과 비교할 커널 설정이다. ok가 거짓이면 WARN이다. 배포판과 용도에 따라 다를 수 있어 FAIL로 두지 않는다.
type auditSysctlRule struct {
	name string
	want string
	ok   func(int) bool
}

var auditSysctlRules = []auditSysctlRule{
	{"kernel.randomize_va_space", "2", func(v int) bool { return v >= 2 }},
	{"kernel.kptr_restrict", ">=1", func(v int) bool { return v >= 1 }},
	{"kernel.dmesg_restrict", "1", func(v int) bool { return v >= 1 }},
	{"kernel.yama.ptrace_scope", ">=1", func(v int) bool { return v >= 1 }},
	{"kernel.unprivileged_bpf_disabled", ">=1", func(v int) bool { return v >= 1 }},
	// Ubuntu는 apport 때문에 2를 쓴다. 2는 root만 읽는 dump라 안전하고, 1만 setuid 프로그램의 메모리를 사용자에게 남긴다.
	{"fs.suid_dumpable", "0 or 2", func(v int) bool { return v != 1 }},
	{"fs.protected_symlinks", "1", func(v int) bool { return v >= 1 }},
	{"fs.protected_hardlinks", "1", func(v int) bool { return v >= 1 }},
}

// auditSensitiveFile은 mode에서 허용하지 않는 bit다.
type auditSensitiveFile struct {
	path string
	deny fs.FileMode
}

var auditSensitiveFiles = []auditSensitiveFile{
	{"/etc/shadow", 0o006},
	{"/etc/gshadow", 0o006},
	{"/etc/passwd", 0o002},
	{"/etc/group", 0o002},
	{"/etc/sudoers", 0o002},
	// docker.sock에 쓸 수 있으면 누구나 호스트 root다.
	{"/var/run/docker.sock", 0o002},
}

// auditContainerMarkers가 있으면 IP forwarding이 켜진 것이 정상이다. container network가 forwarding을 쓴다.
var auditContainerMarkers = []struct{ path, name string }{
	{"/var/run/docker.sock", "docker"},
	{"/run/containerd/containerd.sock", "containerd"},
	{"/var/lib/kubelet", "kubelet"},
	{"/etc/rancher/k3s", "k3s"},
	{"/var/run/libvirt", "libvirt"},
}

// auditSources는 점검이 읽는 원천이다. 테스트는 root를 임시 디렉터리로 바꾸고 함수들을 가짜로 넣는다.
type auditSources struct {
	root     string
	euid     int
	path     string
	listen   func() ([]listenSocket, error)
	firewall func(context.Context) (auditFirewall, error)
	kernel   func() string
	owner    func(fs.FileInfo) (uint32, bool)
	// container는 edc가 도는 container runtime이다. 비어 있으면 ctr.* 점검을 하지 않는다.
	container string
}

type auditFirewall struct {
	Tool string
	// Chains는 input hook에 걸린 filter chain의 수다. 0이면 들어오는 트래픽을 거르는 규칙이 없다.
	Chains, Rules int
	// Drops는 규칙에 맞지 않은 트래픽을 버리는지다. 기본 정책이 drop이거나 마지막 규칙이 조건 없는 drop·reject다.
	Drops bool
}

type auditFile struct {
	Path string
	Mode fs.FileMode
	// BadOwner는 파일 주인이 계정 자신도 root도 아닌 경우다.
	BadOwner bool
}

type auditPasswd struct {
	Name     string
	UID, GID int
	Home     string
}

type auditFacts struct {
	Root bool

	Listen    []listenSocket
	ListenErr error
	Firewall  auditFirewall
	// FirewallErr가 있으면 방화벽을 모른다는 뜻이다. 막는다고도 열려 있다고도 판정하지 않는다.
	FirewallErr   error
	IPForward     string
	IPForwardErr  error
	ContainerHost string

	SSHD       sshdConfig
	SSHDErr    error
	SSHDPath   string
	KeyFiles   []auditFile
	KeySkipped int

	Passwd     []auditPasswd
	PasswdErr  error
	EmptyPass  []string
	ShadowErr  error
	Sudo       []string
	SudoErr    error
	RootGroups map[string][]string
	GroupErr   error

	Sensitive []auditFile
	PathDirs  []string
	PathBad   []string

	Sysctl map[string]string

	RebootPackages []string
	RebootFlag     bool
	Running        string
	Kernels        []string

	// AllowPorts는 설정에서 열어 두기로 한 포트다. net.exposed 판정에서 빼고 따로 알린다.
	AllowPorts []int

	Container     string
	EUID          int
	CapEff        uint64
	CapLast       int
	CapSource     string
	CapErr        error
	DockerSock    string
	DockerSockAt  string
	DockerSockErr error
	PIDNS         string
	PIDNSErr      error
	HostNet       bool
	NetSource     string
	NetErr        error
}

func runAudit(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := newFlagSet("audit")
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc audit [options]"))
		return 2
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	sources, ok := hostAuditSources()
	if !ok {
		return emit(options, buildReport(version, started, nil, []Result{unsupported("audit", T("audit.skip.linux_only"))}, options.redact))
	}
	facts := collectAuditFacts(ctx, sources)
	facts.AllowPorts = activeConfig.Audit.AllowPorts
	results := applyAuditAccept(evaluateAudit(facts), activeConfig.Audit.Accept)
	return emit(options, buildReport(version, started, nil, results, options.redact))
}

// auditCheckIDs는 evaluateAudit가 돌려줄 수 있는 모든 id다. 설정의 accept에 오타가 있으면 이 목록으로 거른다.
var auditCheckIDs = []string{
	"net.exposed", "net.firewall", "net.ip_forward",
	"ssh.root_login", "ssh.password_auth", "ssh.key_files",
	"user.uid0", "user.empty_pass", "user.sudo_nopass", "user.root_groups",
	"file.sensitive", "file.path_writable", "kernel.sysctl", "patch.reboot",
	"ctr.privileged", "ctr.docker_sock", "ctr.root_user", "ctr.host_ns",
}

// applyAuditAccept는 받아들인 warn·fail을 skip으로 바꾼다. 결과를 지우지 않고 원래 판정을 요약과 metrics에 남긴다.
func applyAuditAccept(results []Result, accept []string) []Result {
	for index, result := range results {
		if (result.Status == StatusWarn || result.Status == StatusFail) && slices.Contains(accept, strings.TrimPrefix(result.Probe, "audit.")) {
			results[index] = acceptAuditResult(result)
		}
	}
	return results
}

func acceptAuditResult(result Result) Result {
	original := result.Status
	result.Status = StatusSkip
	result.Summary = T("audit.accepted", original, result.Summary)
	result.Error = nil
	result.Next = ""
	if result.Metrics == nil {
		result.Metrics = map[string]interface{}{}
	}
	result.Metrics["accepted"] = true
	result.Metrics["accepted_status"] = original
	return result
}

func (sources auditSources) file(path string) string { return filepath.Join(sources.root, path) }

func collectAuditFacts(ctx context.Context, sources auditSources) auditFacts {
	facts := auditFacts{Root: sources.euid == 0}
	facts.Listen, facts.ListenErr = sources.listen()
	if facts.Root {
		facts.Firewall, facts.FirewallErr = sources.firewall(ctx)
	} else {
		facts.FirewallErr = errAuditNeedsRoot
	}
	facts.IPForward, facts.IPForwardErr = readTrimmed(sources.file("/proc/sys/net/ipv4/ip_forward"))
	for _, marker := range auditContainerMarkers {
		if _, err := os.Stat(sources.file(marker.path)); err == nil {
			facts.ContainerHost = marker.name
			break
		}
	}

	facts.SSHDPath = "/etc/ssh/sshd_config"
	facts.SSHD, facts.SSHDErr = readSSHDConfig(sources.root, facts.SSHDPath)

	facts.Passwd, facts.PasswdErr = readAuditPasswd(sources.file("/etc/passwd"))
	if facts.PasswdErr == nil {
		facts.KeyFiles, facts.KeySkipped = collectAuditKeyFiles(sources, facts.Passwd)
	}
	facts.EmptyPass, facts.ShadowErr = readAuditEmptyPasswords(sources.file("/etc/shadow"))
	facts.Sudo, facts.SudoErr = readSudoNoPasswdRules(sources.root)
	facts.RootGroups, facts.GroupErr = readAuditRootGroups(sources.file("/etc/group"), facts.Passwd)

	for _, file := range auditSensitiveFiles {
		info, err := os.Stat(sources.file(file.path))
		if err != nil {
			continue
		}
		facts.Sensitive = append(facts.Sensitive, auditFile{Path: file.path, Mode: info.Mode().Perm() & file.deny})
	}
	facts.PathDirs, facts.PathBad = auditPathEntries(sources)

	facts.Sysctl = map[string]string{}
	for _, rule := range auditSysctlRules {
		if value, err := readTrimmed(sources.file("/proc/sys/" + strings.ReplaceAll(rule.name, ".", "/"))); err == nil {
			facts.Sysctl[rule.name] = value
		}
	}

	if _, err := os.Stat(sources.file("/var/run/reboot-required")); err == nil {
		facts.RebootFlag = true
		if data, err := os.ReadFile(sources.file("/var/run/reboot-required.pkgs")); err == nil {
			facts.RebootPackages = uniqueFields(string(data))
		}
	}
	facts.Running = sources.kernel()
	matches, _ := filepath.Glob(sources.file("/boot/vmlinuz-*"))
	for _, match := range matches {
		release := strings.TrimPrefix(filepath.Base(match), "vmlinuz-")
		if !strings.Contains(release, "rescue") {
			facts.Kernels = append(facts.Kernels, release)
		}
	}
	if sources.container != "" {
		collectAuditContainer(sources, &facts)
	}
	return facts
}

// auditHostPIDNamespace는 커널이 처음 만든 PID namespace의 inode다(PROC_PID_INIT_INO). 커널마다 고정된 값이라 이 값이면 호스트의 PID namespace다.
const auditHostPIDNamespace = "pid:[4026531836]"

var auditDockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

func collectAuditContainer(sources auditSources, facts *auditFacts) {
	facts.Container = sources.container
	facts.EUID = sources.euid
	facts.CapEff, facts.CapLast, facts.CapSource, facts.CapErr = readAuditCaps(sources)
	for _, path := range auditDockerSockets {
		info, err := os.Stat(sources.file(path))
		if err == nil && info.Mode()&fs.ModeSocket != 0 {
			facts.DockerSock, facts.DockerSockAt, facts.DockerSockErr = path, "", nil
			break
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) && facts.DockerSockErr == nil {
			facts.DockerSockAt, facts.DockerSockErr = path, err
		}
	}
	facts.PIDNS, facts.PIDNSErr = readAuditNamespace(sources, "/proc/self/ns/pid", "pid")
	// container 자신의 PID namespace에서는 /proc/1이 container의 init이라 net namespace가 언제나 같다. 호스트 PID namespace일 때만 /proc/1이 호스트 init이다.
	if facts.PIDNSErr == nil && facts.PIDNS == auditHostPIDNamespace {
		self, err := readAuditNamespace(sources, "/proc/self/ns/net", "net")
		if err != nil {
			facts.NetSource, facts.NetErr = "/proc/self/ns/net", err
			return
		}
		initNet, err := readAuditNamespace(sources, "/proc/1/ns/net", "net")
		if err != nil {
			facts.NetSource, facts.NetErr = "/proc/1/ns/net", err
			return
		}
		facts.HostNet = self == initNet
	}
}

// readAuditCaps는 CapEff와 cap_last_cap을 읽는다. 둘 중 하나라도 읽지 못하면 CapBnd 같은 다른 값으로 대신하지 않는다.
func readAuditCaps(sources auditSources) (uint64, int, string, error) {
	const status, lastCap = "/proc/self/status", "/proc/sys/kernel/cap_last_cap"
	data, err := os.ReadFile(sources.file(status))
	if err != nil {
		return 0, 0, status, err
	}
	capEff, found := uint64(0), false
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "CapEff" {
			continue
		}
		if capEff, err = strconv.ParseUint(strings.TrimSpace(value), 16, 64); err != nil {
			return 0, 0, status, fmt.Errorf("CapEff %q is not hex", strings.TrimSpace(value))
		}
		found = true
		break
	}
	if !found {
		return 0, 0, status, errors.New("no CapEff line")
	}
	text, err := readTrimmed(sources.file(lastCap))
	if err != nil {
		return 0, 0, lastCap, err
	}
	last, err := strconv.Atoi(text)
	if err != nil || last < 0 || last > 63 {
		return 0, 0, lastCap, fmt.Errorf("%q is not in 0..63", text)
	}
	return capEff, last, "", nil
}

func readAuditNamespace(sources auditSources, path, kind string) (string, error) {
	link, err := os.Readlink(sources.file(path))
	if err != nil {
		return "", err
	}
	inode, found := strings.CutPrefix(link, kind+":[")
	inode, closed := strings.CutSuffix(inode, "]")
	if _, err := strconv.ParseUint(inode, 10, 64); !found || !closed || err != nil {
		return "", fmt.Errorf("unexpected link %q", link)
	}
	return link, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	return strings.TrimSpace(string(data)), err
}

func uniqueFields(text string) []string {
	var values []string
	for _, field := range strings.Fields(text) {
		if !slices.Contains(values, field) {
			values = append(values, field)
		}
	}
	return values
}

func readAuditPasswd(path string) ([]auditPasswd, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []auditPasswd
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid, uidErr := strconv.Atoi(fields[2])
		gid, gidErr := strconv.Atoi(fields[3])
		if uidErr != nil || gidErr != nil {
			continue
		}
		entries = append(entries, auditPasswd{Name: fields[0], UID: uid, GID: gid, Home: fields[5]})
	}
	return entries, nil
}

func readAuditEmptyPasswords(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		// "!"나 "*"는 잠긴 계정이다. 빈 칸만 비밀번호 없이 로그인할 수 있다.
		if len(fields) >= 2 && fields[0] != "" && fields[1] == "" {
			names = append(names, fields[0])
		}
	}
	return names, nil
}

// readAuditRootGroups는 사실상 root 권한을 주는 그룹의 멤버를 모은다. docker 그룹은 host 디렉터리를 마운트한 컨테이너를,
// lxd 그룹은 privileged 컨테이너를 띄울 수 있다. primary group으로 속한 계정도 멤버다.
func readAuditRootGroups(path string, passwd []auditPasswd) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	groups := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || (fields[0] != "docker" && fields[0] != "lxd") {
			continue
		}
		var members []string
		for _, member := range strings.Split(fields[3], ",") {
			if member = strings.TrimSpace(member); member != "" {
				members = append(members, member)
			}
		}
		if gid, err := strconv.Atoi(fields[2]); err == nil {
			for _, entry := range passwd {
				if entry.GID == gid && !slices.Contains(members, entry.Name) {
					members = append(members, entry.Name)
				}
			}
		}
		if len(members) > 0 {
			sort.Strings(members)
			groups[fields[0]] = members
		}
	}
	return groups, nil
}

// collectAuditKeyFiles는 로그인 계정의 ~/.ssh와 authorized_keys를 본다. 다른 사람이 쓸 수 있으면 자기 key를 넣어 그 계정으로 들어온다.
func collectAuditKeyFiles(sources auditSources, passwd []auditPasswd) ([]auditFile, int) {
	var files []auditFile
	skipped := 0
	seen := map[string]bool{}
	for _, entry := range passwd {
		// 1000 미만은 서비스 계정이다. 로그인 계정은 root와 일반 사용자뿐이다.
		if (entry.UID != 0 && entry.UID < 1000) || entry.Home == "" || entry.Home == "/" || seen[entry.Home] {
			continue
		}
		seen[entry.Home] = true
		for _, path := range []string{filepath.Join(entry.Home, ".ssh"), filepath.Join(entry.Home, ".ssh", "authorized_keys")} {
			info, err := os.Stat(sources.file(path))
			if errors.Is(err, fs.ErrPermission) {
				skipped++
				break
			}
			if err != nil {
				break
			}
			file := auditFile{Path: path, Mode: info.Mode().Perm() & 0o022}
			if owner, ok := sources.owner(info); ok && owner != 0 && int(owner) != entry.UID {
				file.BadOwner = true
			}
			files = append(files, file)
		}
	}
	return files, skipped
}

// auditPathEntries는 PATH에서 다른 사용자가 쓸 수 있는 디렉터리와 상대 경로를 찾는다. 거기에 ls 같은 이름의 파일을 두면 root가 실행한다.
func auditPathEntries(sources auditSources) ([]string, []string) {
	entries := strings.Split(auditDefaultPath, ":")
	// root가 아닌 셸의 PATH는 root가 쓰는 PATH가 아니다. root로 실행할 때만 지금 PATH를 더한다.
	// PATH가 아예 없으면 빈 항목 하나로 쪼개져 현재 디렉터리로 오판한다. cron처럼 PATH 없이 실행한 경우다.
	if sources.euid == 0 && sources.path != "" {
		for _, entry := range strings.Split(sources.path, ":") {
			if !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
		}
	}
	var checked, bad []string
	for _, entry := range entries {
		if !filepath.IsAbs(entry) {
			bad = append(bad, strconv.Quote(entry))
			continue
		}
		info, err := os.Stat(sources.file(entry))
		if err != nil || !info.IsDir() {
			continue
		}
		checked = append(checked, entry)
		if info.Mode().Perm()&0o002 != 0 {
			bad = append(bad, entry)
		}
	}
	return checked, bad
}

// sshdConfig는 sshd처럼 처음 나온 값을 쓴다. Match 블록은 조건부라 해석하지 않고 있었다는 사실만 남긴다.
type sshdConfig struct {
	values  map[string]string
	sources map[string]string
	Match   bool
}

// sshdMaxIncludeDepth는 서로를 Include하는 파일이 끝나지 않게 막는다.
const sshdMaxIncludeDepth = 16

func readSSHDConfig(root, path string) (sshdConfig, error) {
	config := sshdConfig{values: map[string]string{}, sources: map[string]string{}}
	return config, config.read(root, path, 0)
}

func (config *sshdConfig) read(root, path string, depth int) error {
	if depth > sshdMaxIncludeDepth {
		return fmt.Errorf("%s: Include nests more than %d levels", path, sshdMaxIncludeDepth)
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := sshdKeyword(line)
		if !ok {
			continue
		}
		switch key {
		case "match":
			// Match 아래 설정은 그 파일이 끝날 때까지 조건에 맞는 연결에만 적용된다.
			config.Match = true
			return nil
		case "include":
			for _, pattern := range strings.Fields(value) {
				if !filepath.IsAbs(pattern) {
					pattern = filepath.Join("/etc/ssh", pattern)
				}
				matches, _ := filepath.Glob(filepath.Join(root, pattern))
				sort.Strings(matches)
				for _, match := range matches {
					relative, err := filepath.Rel(root, match)
					if err != nil {
						return err
					}
					if err := config.read(root, "/"+relative, depth+1); err != nil {
						return err
					}
				}
			}
		default:
			if _, seen := config.values[key]; !seen {
				config.values[key] = strings.ToLower(strings.Fields(value)[0])
				config.sources[key] = path
			}
		}
	}
	return nil
}

func sshdKeyword(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	index := strings.IndexAny(line, " \t=")
	if index < 0 {
		return "", "", false
	}
	key := strings.ToLower(line[:index])
	value := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line[index:]), "="))
	if value == "" {
		return "", "", false
	}
	// OpenSSH 8.7에서 ChallengeResponseAuthentication의 이름이 바뀌었다. 옛 이름도 같은 설정이다.
	if key == "challengeresponseauthentication" {
		key = "kbdinteractiveauthentication"
	}
	return key, value, true
}

// value는 설정 값과 그 출처다. 값이 없으면 OpenSSH의 기본값과 "default"를 돌려준다.
func (config sshdConfig) value(key, fallback string) (string, string) {
	if value, ok := config.values[key]; ok {
		return value, config.sources[key]
	}
	return fallback, "default"
}

// readSudoNoPasswdRules는 비밀번호 없이 모든 명령을 실행하게 하는 규칙을 찾는다. 명령을 제한한 NOPASSWD 규칙은 흔하고 위험이 작아 세지 않는다.
func readSudoNoPasswdRules(root string) ([]string, error) {
	var rules []string
	return rules, readSudoersFile(root, "/etc/sudoers", 0, &rules)
}

func readSudoersFile(root, path string, depth int, rules *[]string) error {
	if depth > sshdMaxIncludeDepth {
		return fmt.Errorf("%s: include nests more than %d levels", path, sshdMaxIncludeDepth)
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		directive, target, _ := strings.Cut(line, " ")
		target = strings.TrimSpace(target)
		if target != "" && !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		switch directive {
		case "#includedir", "@includedir":
			entries, err := os.ReadDir(filepath.Join(root, target))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				// sudo는 '.'이 들어가거나 '~'로 끝나는 이름을 건너뛴다. 편집기 백업과 패키지 임시 파일이다.
				if entry.IsDir() || strings.Contains(entry.Name(), ".") || strings.HasSuffix(entry.Name(), "~") {
					continue
				}
				if err := readSudoersFile(root, filepath.Join(target, entry.Name()), depth+1, rules); err != nil {
					return err
				}
			}
			continue
		case "#include", "@include":
			if err := readSudoersFile(root, target, depth+1, rules); err != nil {
				return err
			}
			continue
		}
		// "#1000 ALL=..."처럼 # 뒤에 숫자가 오면 주석이 아니라 uid로 쓴 사용자다.
		if line == "" || (strings.HasPrefix(line, "#") && !(len(line) > 1 && isDigit(line[1]))) || strings.HasPrefix(line, "Defaults") {
			continue
		}
		if sudoRunsAllWithoutPassword(line) {
			*rules = append(*rules, path+": "+line)
		}
	}
	return nil
}

func sudoRunsAllWithoutPassword(rule string) bool {
	index := strings.LastIndex(rule, "NOPASSWD:")
	if index < 0 {
		return false
	}
	for _, command := range strings.Split(rule[index+len("NOPASSWD:"):], ",") {
		command = strings.TrimSpace(command)
		// NOPASSWD 뒤에 SETENV: 같은 태그가 더 올 수 있다.
		for {
			tag, rest, found := strings.Cut(command, ":")
			if !found || tag == "" || strings.ToUpper(tag) != tag || strings.ContainsAny(tag, " /") {
				break
			}
			// PASSWD: 뒤의 명령은 다시 비밀번호를 묻는다.
			if tag == "PASSWD" {
				return false
			}
			command = strings.TrimSpace(rest)
		}
		if command == "ALL" {
			return true
		}
	}
	return false
}

// parseNftInputChains는 `nft list ruleset`에서 input hook에 걸린 filter chain만 센다. iptables-nft가 만든 규칙도 여기에 보인다.
func parseNftInputChains(text string) auditFirewall {
	firewall := auditFirewall{Tool: "nftables"}
	inChain, input := false, false
	last := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "chain ") && strings.HasSuffix(line, "{"):
			inChain, input, last = true, false, ""
		case inChain && line == "}":
			if input {
				firewall.Chains++
				firewall.Drops = firewall.Drops || firewallCatchAll(nftRuleVerdict(last))
			}
			inChain = false
		case inChain && strings.HasPrefix(line, "type filter hook input"):
			input = true
			if _, policy, ok := strings.Cut(line, "policy "); ok && strings.TrimSuffix(strings.TrimSpace(policy), ";") == "drop" {
				firewall.Drops = true
			}
		case inChain && input && line != "" && !strings.HasPrefix(line, "#"):
			firewall.Rules++
			last = line
		}
	}
	return firewall
}

// nftRuleVerdict는 조건 없는 규칙의 판정만 남긴다. counter와 comment는 조건이 아니다.
func nftRuleVerdict(rule string) string {
	if index := strings.Index(rule, " comment \""); index >= 0 {
		rule = rule[:index]
	}
	fields := strings.Fields(rule)
	if len(fields) > 0 && fields[0] == "counter" {
		fields = fields[1:]
		if len(fields) >= 4 && fields[0] == "packets" && fields[2] == "bytes" {
			fields = fields[4:]
		}
	}
	return strings.Join(fields, " ")
}

func firewallCatchAll(verdict string) bool {
	return verdict == "drop" || verdict == "reject" || strings.HasPrefix(verdict, "reject with ")
}

// parseIPTablesInput은 `iptables-save`의 filter 표에서 INPUT chain을 읽는다. legacy iptables의 규칙은 nft에 보이지 않는다.
func parseIPTablesInput(text string) auditFirewall {
	firewall := auditFirewall{Tool: "iptables"}
	table, last := "", ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "*"):
			table = line[1:]
		case table != "filter":
		case strings.HasPrefix(line, ":INPUT "):
			firewall.Chains++
			fields := strings.Fields(line)
			firewall.Drops = firewall.Drops || (len(fields) > 1 && fields[1] == "DROP")
		case strings.HasPrefix(line, "-A INPUT "):
			firewall.Rules++
			last = line
		}
	}
	if last != "" {
		fields := strings.Fields(strings.TrimPrefix(last, "-A INPUT "))
		if index := slices.Index(fields, "--reject-with"); index >= 0 && index+1 < len(fields) {
			fields = slices.Delete(fields, index, index+2)
		}
		if len(fields) == 2 && fields[0] == "-j" && (fields[1] == "DROP" || fields[1] == "REJECT") {
			firewall.Drops = true
		}
	}
	return firewall
}

// compareKernelReleases는 숫자 부분을 숫자로 비교한다. 문자열로 비교하면 6.8.0-9가 6.8.0-10보다 새것이 된다.
func compareKernelReleases(a, b string) int {
	split := func(value string) []string {
		var parts []string
		start := 0
		for index := 1; index <= len(value); index++ {
			if index == len(value) || isDigit(value[index]) != isDigit(value[start]) {
				parts = append(parts, value[start:index])
				start = index
			}
		}
		return parts
	}
	left, right := split(a), split(b)
	for index := 0; index < len(left) && index < len(right); index++ {
		x, xErr := strconv.Atoi(left[index])
		y, yErr := strconv.Atoi(right[index])
		if xErr == nil && yErr == nil {
			if x != y {
				return x - y
			}
			continue
		}
		if comparison := strings.Compare(left[index], right[index]); comparison != 0 {
			return comparison
		}
	}
	return len(left) - len(right)
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

func auditResult(id string, status Status, summary string) Result {
	return Result{Probe: "audit." + id, Status: status, StartedAt: time.Now().UTC(), Summary: summary}
}

// auditSkip은 원천을 읽지 못한 항목이다. 권한 때문이면 root가 필요하다고 알린다.
func auditSkip(id, source string, err error) Result {
	if errors.Is(err, errAuditNeedsRoot) || errors.Is(err, fs.ErrPermission) {
		return auditResult(id, StatusSkip, T("audit.skip.needs_root", source))
	}
	return auditResult(id, StatusSkip, T("audit.skip.unreadable", source, infoSingleLine(err.Error())))
}

func evaluateAudit(facts auditFacts) []Result {
	results := []Result{
		auditExposure(facts),
		auditFirewallResult(facts),
		auditIPForward(facts),
		auditSSHRootLogin(facts),
		auditSSHPassword(facts),
		auditAuthorizedKeys(facts),
		auditUID0(facts),
		auditEmptyPasswords(facts),
		auditSudo(facts),
		auditRootGroups(facts),
		auditSensitiveModes(facts),
		auditPath(facts),
		auditKernelSysctl(facts),
		auditReboot(facts),
	}
	if facts.Container != "" {
		results = append(results, auditPrivileged(facts), auditDockerSocket(facts), auditRootUser(facts), auditHostNamespace(facts))
	}
	return results
}

type auditExposed struct {
	labels, evidence []string
	ports            []int
	published        bool
}

func (exposed *auditExposed) add(name string, socket listenSocket) {
	label := fmt.Sprintf("%s/%d", name, socket.Port)
	if !slices.Contains(exposed.labels, label) {
		exposed.labels = append(exposed.labels, label)
		exposed.ports = append(exposed.ports, socket.Port)
	}
	exposed.published = exposed.published || socket.Process == "docker-proxy"
	line := fmt.Sprintf("%s %s:%d", socket.Proto, socket.Address, socket.Port)
	if socket.Process != "" {
		line += "  " + socket.Process
	}
	exposed.evidence = append(exposed.evidence, line)
}

func auditExposure(facts auditFacts) Result {
	const id = "net.exposed"
	if facts.ListenErr != nil {
		return auditSkip(id, "sockets", facts.ListenErr)
	}
	var rest, allowed auditExposed
	seen := map[string]bool{}
	for _, socket := range facts.Listen {
		name, sensitive := auditSensitivePorts[socket.Port]
		scope := listenBindingScope(socket.Proto, socket.Address)
		if !sensitive || (scope != "all" && scope != "specific") {
			continue
		}
		key := fmt.Sprintf("%s %s %d", socket.Proto, socket.Address, socket.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		if slices.Contains(facts.AllowPorts, socket.Port) {
			allowed.add(name, socket)
		} else {
			rest.add(name, socket)
		}
	}
	switch {
	case len(rest.labels) == 0 && len(allowed.labels) == 0:
		return auditResult(id, StatusPass, T("audit.net.exposed_none", len(facts.Listen)))
	case len(rest.labels) == 0:
		// 모두 허용한 포트여도 판정을 지우지 않는다. 원래 판정을 accepted로 보인다.
		result := acceptAuditResult(auditExposureResult(facts, allowed))
		result.Metrics["allowed_ports"] = allowed.ports
		result.Warnings = append(result.Warnings, T("audit.net.allowed_ports", strings.Join(allowed.labels, ", ")))
		return result
	}
	result := auditExposureResult(facts, rest)
	if len(allowed.labels) > 0 {
		result.Metrics = map[string]interface{}{"allowed_ports": allowed.ports}
		result.Warnings = append(result.Warnings, T("audit.net.allowed_ports", strings.Join(allowed.labels, ", ")))
	}
	return result
}

func auditExposureResult(facts auditFacts, exposed auditExposed) Result {
	const id = "net.exposed"
	ports := strings.Join(exposed.labels, ", ")
	var result Result
	switch {
	case facts.FirewallErr != nil:
		result = auditResult(id, StatusWarn, T("audit.net.exposed_unknown", ports))
	case facts.Firewall.Drops:
		result = auditResult(id, StatusWarn, T("audit.net.exposed_filtered", ports))
	default:
		result = auditResult(id, StatusFail, T("audit.net.exposed_open", ports))
		result.Error = &DiagnosticError{Kind: "exposure", Message: result.Summary}
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.sockets"), Value: strings.Join(exposed.evidence, "\n")}}
	result.Warnings = []string{T("audit.net.cloud_note")}
	// docker가 publish한 포트는 nat 표에서 DNAT되어 INPUT이 아니라 FORWARD를 지난다. INPUT의 drop 정책은 막지 못한다.
	if exposed.published {
		result.Warnings = append(result.Warnings, T("audit.net.docker_note"))
	}
	result.Next = "edc listen -v"
	return result
}

func auditFirewallResult(facts auditFacts) Result {
	const id = "net.firewall"
	if errors.Is(facts.FirewallErr, errAuditNoFirewallTool) {
		return auditResult(id, StatusSkip, T("audit.skip.no_firewall_tool"))
	}
	if facts.FirewallErr != nil {
		return auditSkip(id, "nft/iptables-save", facts.FirewallErr)
	}
	firewall := facts.Firewall
	if firewall.Drops {
		return auditResult(id, StatusPass, T("audit.net.firewall_drops", firewall.Tool, firewall.Chains, firewall.Rules))
	}
	if firewall.Rules > 0 {
		// 규칙이 있으면 특정 포트를 막고 있을 수 있다. 순서와 조건까지 해석하지 않으므로 경고하지 않는다.
		return auditResult(id, StatusPass, T("audit.net.firewall_rules", firewall.Tool, firewall.Rules))
	}
	result := auditResult(id, StatusWarn, T("audit.net.firewall_open", firewall.Tool))
	result.Warnings = []string{T("audit.net.cloud_note")}
	return result
}

func auditIPForward(facts auditFacts) Result {
	const id = "net.ip_forward"
	if facts.IPForwardErr != nil {
		return auditSkip(id, "net.ipv4.ip_forward", facts.IPForwardErr)
	}
	switch {
	case facts.IPForward == "0":
		return auditResult(id, StatusPass, T("audit.net.forward_off"))
	case facts.ContainerHost != "":
		return auditResult(id, StatusPass, T("audit.net.forward_container", facts.ContainerHost))
	default:
		return auditResult(id, StatusWarn, T("audit.net.forward_on"))
	}
}

func auditSSHConfigSkip(id string, facts auditFacts) (Result, bool) {
	if errors.Is(facts.SSHDErr, fs.ErrNotExist) {
		return auditResult(id, StatusSkip, T("audit.skip.no_sshd", facts.SSHDPath)), true
	}
	if facts.SSHDErr != nil {
		return auditSkip(id, facts.SSHDPath, facts.SSHDErr), true
	}
	return Result{}, false
}

func auditSSHMatchWarning(result Result, facts auditFacts) Result {
	if facts.SSHD.Match {
		result.Warnings = append(result.Warnings, T("audit.ssh.match_note"))
	}
	return result
}

func auditSSHRootLogin(facts auditFacts) Result {
	const id = "ssh.root_login"
	if result, skip := auditSSHConfigSkip(id, facts); skip {
		return result
	}
	// OpenSSH 7.0부터 기본값은 prohibit-password다. root는 key로만 들어온다.
	value, source := facts.SSHD.value("permitrootlogin", "prohibit-password")
	if value == "yes" {
		result := auditResult(id, StatusWarn, T("audit.ssh.root_login_yes", source))
		result.Next = auditSSHDNext
		return auditSSHMatchWarning(result, facts)
	}
	return auditSSHMatchWarning(auditResult(id, StatusPass, T("audit.ssh.root_login_ok", value, source)), facts)
}

const auditSSHDNext = "sudo sshd -T | grep -Ei '^(permitrootlogin|passwordauthentication|kbdinteractiveauthentication|permitemptypasswords|usepam) '"

func auditSSHPassword(facts auditFacts) Result {
	const id = "ssh.password_auth"
	if result, skip := auditSSHConfigSkip(id, facts); skip {
		return result
	}
	password, passwordSource := facts.SSHD.value("passwordauthentication", "yes")
	keyboard, keyboardSource := facts.SSHD.value("kbdinteractiveauthentication", "yes")
	pam, pamSource := facts.SSHD.value("usepam", "no")
	empty, _ := facts.SSHD.value("permitemptypasswords", "no")
	var methods []string
	if password == "yes" {
		methods = append(methods, "PasswordAuthentication ("+passwordSource+")")
	}
	// keyboard-interactive는 PAM을 거칠 때 비밀번호를 묻는다. PAM이 꺼져 있으면 비밀번호 방식이 아니다.
	if keyboard == "yes" && pam == "yes" {
		methods = append(methods, "KbdInteractiveAuthentication ("+keyboardSource+") + UsePAM ("+pamSource+")")
	}
	var result Result
	switch {
	case len(methods) == 0:
		return auditSSHMatchWarning(auditResult(id, StatusPass, T("audit.ssh.password_off")), facts)
	case empty == "yes":
		result = auditResult(id, StatusFail, T("audit.ssh.empty_passwords"))
		result.Error = &DiagnosticError{Kind: "ssh", Message: result.Summary}
	default:
		result = auditResult(id, StatusWarn, T("audit.ssh.password_on", strings.Join(methods, ", ")))
	}
	result.Next = auditSSHDNext
	return auditSSHMatchWarning(result, facts)
}

func auditAuthorizedKeys(facts auditFacts) Result {
	const id = "ssh.key_files"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	var bad []string
	keys := 0
	for _, file := range facts.KeyFiles {
		if filepath.Base(file.Path) == "authorized_keys" {
			keys++
		}
		var problems []string
		if file.Mode != 0 {
			problems = append(problems, fmt.Sprintf("mode %s", file.Mode))
		}
		if file.BadOwner {
			problems = append(problems, T("audit.evidence.other_owner"))
		}
		if len(problems) > 0 {
			bad = append(bad, file.Path+"  "+strings.Join(problems, ", "))
		}
	}
	var result Result
	if len(bad) == 0 {
		result = auditResult(id, StatusPass, T("audit.ssh.keys_ok", keys))
	} else {
		result = auditResult(id, StatusFail, T("audit.ssh.keys_bad", len(bad)))
		result.Error = &DiagnosticError{Kind: "permission", Message: result.Summary}
		result.Evidence = []Evidence{{Label: T("audit.evidence.files"), Value: strings.Join(bad, "\n")}}
	}
	if facts.KeySkipped > 0 {
		result.Warnings = []string{T("audit.ssh.keys_skipped", facts.KeySkipped)}
	}
	return result
}

func auditUID0(facts auditFacts) Result {
	const id = "user.uid0"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	var others []string
	for _, entry := range facts.Passwd {
		if entry.UID == 0 && entry.Name != "root" {
			others = append(others, entry.Name)
		}
	}
	if len(others) == 0 {
		return auditResult(id, StatusPass, T("audit.account.uid0_ok"))
	}
	result := auditResult(id, StatusFail, T("audit.account.uid0_bad", strings.Join(others, ", ")))
	result.Error = &DiagnosticError{Kind: "account", Message: result.Summary}
	return result
}

func auditEmptyPasswords(facts auditFacts) Result {
	const id = "user.empty_pass"
	if facts.ShadowErr != nil {
		return auditSkip(id, "/etc/shadow", facts.ShadowErr)
	}
	if len(facts.EmptyPass) == 0 {
		return auditResult(id, StatusPass, T("audit.account.empty_ok"))
	}
	result := auditResult(id, StatusFail, T("audit.account.empty_bad", strings.Join(facts.EmptyPass, ", ")))
	result.Error = &DiagnosticError{Kind: "account", Message: result.Summary}
	result.Next = "sudo passwd -l " + facts.EmptyPass[0]
	return result
}

func auditSudo(facts auditFacts) Result {
	const id = "user.sudo_nopass"
	if facts.SudoErr != nil {
		return auditSkip(id, "/etc/sudoers", facts.SudoErr)
	}
	if len(facts.Sudo) == 0 {
		return auditResult(id, StatusPass, T("audit.account.sudo_ok"))
	}
	result := auditResult(id, StatusWarn, T("audit.account.sudo_bad", len(facts.Sudo)))
	result.Evidence = []Evidence{{Label: T("audit.evidence.rules"), Value: strings.Join(facts.Sudo, "\n")}}
	return result
}

func auditRootGroups(facts auditFacts) Result {
	const id = "user.root_groups"
	if facts.GroupErr != nil {
		return auditSkip(id, "/etc/group", facts.GroupErr)
	}
	if len(facts.RootGroups) == 0 {
		return auditResult(id, StatusPass, T("audit.account.groups_ok"))
	}
	var parts []string
	for _, group := range []string{"docker", "lxd"} {
		if members := facts.RootGroups[group]; len(members) > 0 {
			parts = append(parts, group+": "+strings.Join(members, ", "))
		}
	}
	return auditResult(id, StatusWarn, T("audit.account.groups_bad", strings.Join(parts, " · ")))
}

func auditSensitiveModes(facts auditFacts) Result {
	const id = "file.sensitive"
	var bad []string
	for _, file := range facts.Sensitive {
		if file.Mode != 0 {
			bad = append(bad, fmt.Sprintf("%s  %s", file.Path, auditModeWords(file.Mode)))
		}
	}
	if len(bad) == 0 {
		return auditResult(id, StatusPass, T("audit.file.sensitive_ok", len(facts.Sensitive)))
	}
	result := auditResult(id, StatusFail, T("audit.file.sensitive_bad", len(bad)))
	result.Error = &DiagnosticError{Kind: "permission", Message: result.Summary}
	result.Evidence = []Evidence{{Label: T("audit.evidence.files"), Value: strings.Join(bad, "\n")}}
	return result
}

func auditModeWords(bits fs.FileMode) string {
	var words []string
	if bits&0o004 != 0 {
		words = append(words, T("audit.evidence.other_read"))
	}
	if bits&0o002 != 0 {
		words = append(words, T("audit.evidence.other_write"))
	}
	return strings.Join(words, ", ")
}

func auditPath(facts auditFacts) Result {
	const id = "file.path_writable"
	if len(facts.PathBad) == 0 {
		return auditResult(id, StatusPass, T("audit.file.path_ok", len(facts.PathDirs)))
	}
	result := auditResult(id, StatusFail, T("audit.file.path_bad", strings.Join(facts.PathBad, ", ")))
	result.Error = &DiagnosticError{Kind: "permission", Message: result.Summary}
	return result
}

func auditKernelSysctl(facts auditFacts) Result {
	const id = "kernel.sysctl"
	var bad, all []string
	for _, rule := range auditSysctlRules {
		value, ok := facts.Sysctl[rule.name]
		if !ok {
			continue
		}
		line := fmt.Sprintf("%s = %s", rule.name, value)
		number, err := strconv.Atoi(value)
		if err != nil || !rule.ok(number) {
			bad = append(bad, fmt.Sprintf("%s=%s (want %s)", rule.name, value, rule.want))
			line += "  (want " + rule.want + ")"
		}
		all = append(all, line)
	}
	if len(all) == 0 {
		return auditResult(id, StatusSkip, T("audit.skip.no_sysctl"))
	}
	var result Result
	if len(bad) == 0 {
		result = auditResult(id, StatusPass, T("audit.kernel.sysctl_ok", len(all)))
	} else {
		result = auditResult(id, StatusWarn, T("audit.kernel.sysctl_bad", strings.Join(bad, ", ")))
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.values"), Value: strings.Join(all, "\n")}}
	return result
}

// auditRebootShown은 요약 줄에 적는 패키지 수다. 커널을 여러 번 올리면 수십 개가 쌓여 줄이 잘린다.
const auditRebootShown = 3

func auditReboot(facts auditFacts) Result {
	const id = "patch.reboot"
	if facts.RebootFlag {
		packages := "/var/run/reboot-required"
		if count := len(facts.RebootPackages); count > 0 {
			packages = strings.Join(facts.RebootPackages[:min(count, auditRebootShown)], ", ")
			if count > auditRebootShown {
				packages += fmt.Sprintf(" (+%d)", count-auditRebootShown)
			}
		}
		return auditResult(id, StatusWarn, T("audit.patch.reboot_flag", packages))
	}
	// 실행 중인 커널이 /boot에 없으면 직접 넣은 커널이나 컨테이너다. 설치된 커널과 비교할 기준이 없다.
	if facts.Running == "" || !slices.Contains(facts.Kernels, facts.Running) {
		return auditResult(id, StatusPass, T("audit.patch.reboot_none"))
	}
	newest := facts.Running
	for _, release := range facts.Kernels {
		if compareKernelReleases(release, newest) > 0 {
			newest = release
		}
	}
	if newest != facts.Running {
		return auditResult(id, StatusWarn, T("audit.patch.reboot_kernel", newest, facts.Running))
	}
	return auditResult(id, StatusPass, T("audit.patch.reboot_ok", facts.Running))
}

func auditPrivileged(facts auditFacts) Result {
	const id = "ctr.privileged"
	if facts.CapErr != nil {
		return auditSkip(id, facts.CapSource, facts.CapErr)
	}
	capEff := fmt.Sprintf("0x%016x", facts.CapEff)
	mask := uint64(1)<<(facts.CapLast+1) - 1
	if facts.CapEff&mask != mask {
		return auditResult(id, StatusPass, T("audit.ctr.caps_limited", capEff))
	}
	result := auditResult(id, StatusFail, T("audit.ctr.privileged", facts.Container, capEff))
	result.Error = &DiagnosticError{Kind: "container", Message: result.Summary}
	return result
}

func auditDockerSocket(facts auditFacts) Result {
	const id = "ctr.docker_sock"
	if facts.DockerSock != "" {
		result := auditResult(id, StatusFail, T("audit.ctr.sock_mounted", facts.DockerSock))
		result.Error = &DiagnosticError{Kind: "container", Message: result.Summary}
		return result
	}
	if facts.DockerSockErr != nil {
		return auditSkip(id, facts.DockerSockAt, facts.DockerSockErr)
	}
	return auditResult(id, StatusPass, T("audit.ctr.sock_none"))
}

func auditRootUser(facts auditFacts) Result {
	const id = "ctr.root_user"
	if facts.EUID == 0 {
		return auditResult(id, StatusWarn, T("audit.ctr.root_user", facts.Container))
	}
	return auditResult(id, StatusPass, T("audit.ctr.user_ok", facts.EUID))
}

func auditHostNamespace(facts auditFacts) Result {
	const id = "ctr.host_ns"
	if facts.PIDNSErr != nil {
		return auditSkip(id, "/proc/self/ns/pid", facts.PIDNSErr)
	}
	if facts.PIDNS != auditHostPIDNamespace {
		return auditResult(id, StatusPass, T("audit.ctr.own_pid", facts.PIDNS))
	}
	summary := T("audit.ctr.host_pid")
	if facts.NetErr == nil && facts.HostNet {
		summary = T("audit.ctr.host_pid_net")
	}
	result := auditResult(id, StatusFail, summary)
	result.Error = &DiagnosticError{Kind: "container", Message: result.Summary}
	if facts.NetErr != nil {
		result.Warnings = []string{T("audit.ctr.net_unknown", facts.NetSource, infoSingleLine(facts.NetErr.Error()))}
	}
	return result
}
