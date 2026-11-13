package edc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 백도어와 터널 점검은 /proc를 한 번 훑어 process마다 fd, 소켓, 명령 인자를 모은다. 네 점검이 같은 순회를 나눠 쓴다.
// 2026-11-13 jw-server와 okrr-intranet-slackbot-1에서 네 신호 모두 평소 0건이었고, 재현한 -R 터널, 두 번째 sshd,
// bash -i >& /dev/tcp/… 가 각각 잡혔다.

// auditBackdoorProcess는 근거에 적는 process다. Owner는 실행 파일을 가진 dpkg 패키지다.
type auditBackdoorProcess struct {
	auditProcess
	Args  string
	Owner string
}

type auditTunnelListener struct {
	Socket listenSocket
	Title  string
}

// auditSSHClient는 이 호스트에서 밖으로 -R이나 -w 터널을 연 ssh client다. -L은 이 호스트에서 원격으로 가는 길이라 흔하고 위험이 작다.
type auditSSHClient struct {
	Process     auditBackdoorProcess
	Destination string
	Forwards    []string
}

type auditSocketShell struct {
	Process auditBackdoorProcess
	FDs     []int
	Peer    string
}

type auditRawHolder struct {
	Process auditBackdoorProcess
	Kinds   []string
	Reason  string
}

type auditSSHDaemon struct {
	Process auditBackdoorProcess
	Cgroup  string
	Ports   []string
}

type auditBackdoor struct {
	Err     error
	Checked int
	// Listeners는 sshd 연결 process가 연 listen 소켓이다(ssh -R). X11은 X11 forwarding이라 판정하지 않은 소켓 수다.
	Listeners   []auditTunnelListener
	SSHDSockets int
	X11         int
	Clients     []auditSSHClient
	SSHClients  int
	Shells      []auditSocketShell
	TCPErr      error
	RawSockets  int
	RawErr      error
	Raw         []auditRawHolder
	Daemons     []auditSSHDaemon
	Extra       []auditSSHDaemon
	Systemd     bool
	PackageDB   bool
}

// auditSSHDFamily는 sshd의 process 이름이다. OpenSSH 9.8부터 연결마다 sshd-session이 돈다.
var auditSSHDFamily = []string{"sshd", "sshd-session"}

// auditSSHServiceUnits는 배포판이 sshd를 띄우는 systemd unit이다.
var auditSSHServiceUnits = []string{"ssh.service", "sshd.service"}

// auditContainerCgroups는 container 안의 process를 뜻하는 cgroup 경로 조각이다. container의 sshd는 자기 network namespace에서 듣는다.
var auditContainerCgroups = []string{"docker-", "/docker/", "libpod-", "kubepods", "containerd", "/lxc", "machine.slice"}

// ssh의 short option 가운데 값을 받는 것이다(ssh(1)). 값 없는 option은 묶어 쓸 수 있어 -NR처럼 온다.
const auditSSHValueOptions = "BbcDEeFIiJLlmOopQRSWw"

func collectAuditBackdoor(sources auditSources, facts *auditFacts) {
	backdoor := &facts.Backdoor
	procRoot := sources.file("/proc")
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		backdoor.Err = err
		return
	}
	_, statErr := os.Stat(sources.file("/run/systemd/system"))
	backdoor.Systemd = statErr == nil
	boot := readAuditBootTime(sources.file("/proc/stat"))
	raw, rawErr := readAuditRawSockets(sources)
	backdoor.RawErr, backdoor.RawSockets = rawErr, len(raw)
	tcp := map[uint64]auditTCPSocket{}
	switch {
	case sources.tcpConnected == nil:
		backdoor.TCPErr = errAuditNoSource
	default:
		sockets, err := sources.tcpConnected()
		if err != nil {
			backdoor.TCPErr = err
		}
		for _, socket := range sockets {
			tcp[socket.Inode] = socket
		}
	}
	describe := func(pid int, comm, exe string, args []string) auditBackdoorProcess {
		process := describeAuditProcess(sources, procRoot, boot, auditProcess{PID: pid, Comm: comm, Exe: exe})
		return auditBackdoorProcess{auditProcess: process, Args: auditMaskedArgs(args)}
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || (sources.pid != 0 && pid == sources.pid) {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		exe, err := os.Readlink(filepath.Join(dir, "exe"))
		// kernel thread에는 실행 파일이 없고, 다른 계정의 process는 root가 아니면 읽지 못한다.
		if err != nil {
			continue
		}
		backdoor.Checked++
		comm, _ := readTrimmed(filepath.Join(dir, "comm"))
		args := readAuditArgs(dir)
		var stdio []int
		var peer string
		kinds := map[string]bool{}
		fdDir := filepath.Join(dir, "fd")
		fds, _ := os.ReadDir(fdDir)
		for _, fd := range fds {
			number, err := strconv.Atoi(fd.Name())
			if err != nil {
				continue
			}
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			value, ok := strings.CutPrefix(target, "socket:[")
			if !ok {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(value, "]"), 10, 64)
			if err != nil {
				continue
			}
			if socket, ok := tcp[inode]; ok && number <= 2 {
				stdio = append(stdio, number)
				peer = socket.Remote.String()
			}
			if kind, ok := raw[inode]; ok {
				kinds[kind] = true
			}
		}
		// stdin이나 stdout이 원격 TCP 연결이면 그 연결이 명령을 넣고 결과를 받는다. stderr만 소켓인 daemon은 있다.
		if slices.Contains(stdio, 0) || slices.Contains(stdio, 1) {
			backdoor.Shells = append(backdoor.Shells, auditSocketShell{Process: describe(pid, comm, exe, args), FDs: stdio, Peer: peer})
		}
		if len(kinds) > 0 {
			holder := auditRawHolder{Process: describe(pid, comm, exe, args), Reason: auditExeReason(exe)}
			for kind := range kinds {
				holder.Kinds = append(holder.Kinds, kind)
			}
			slices.Sort(holder.Kinds)
			backdoor.Raw = append(backdoor.Raw, holder)
		}
		if comm == "ssh" {
			backdoor.SSHClients++
			if destination, forwards := auditSSHForwards(args); len(forwards) > 0 {
				backdoor.Clients = append(backdoor.Clients, auditSSHClient{Process: describe(pid, comm, exe, args), Destination: destination, Forwards: forwards})
			}
		}
		if comm == "sshd" && strings.Contains(strings.Join(args, " "), "[listener]") {
			backdoor.Daemons = append(backdoor.Daemons, auditSSHDaemon{Process: describe(pid, comm, exe, args), Cgroup: readAuditCgroup(dir)})
		}
	}
	for _, socket := range facts.Listen {
		if socket.Proto != "tcp" || !slices.Contains(auditSSHDFamily, socket.Process) {
			continue
		}
		backdoor.SSHDSockets++
		pid, _ := strconv.Atoi(socket.PID)
		address := net.JoinHostPort(socket.Address, strconv.Itoa(socket.Port))
		if index := slices.IndexFunc(backdoor.Daemons, func(daemon auditSSHDaemon) bool { return daemon.Process.PID == pid }); index >= 0 {
			backdoor.Daemons[index].Ports = append(backdoor.Daemons[index].Ports, address)
			continue
		}
		// ssh -X는 연결 process가 loopback의 6010부터 듣는다(X11DisplayOffset 10). 터널이 아니라 화면 전달이다.
		if socket.Port >= 6010 && socket.Port < 6100 && listenBindingScope(socket.Proto, socket.Address) == "loopback" {
			backdoor.X11++
			continue
		}
		title := ""
		if pid > 0 {
			title = auditMaskedArgs(readAuditArgs(filepath.Join(procRoot, socket.PID)))
		}
		backdoor.Listeners = append(backdoor.Listeners, auditTunnelListener{Socket: socket, Title: title})
	}
	for _, daemon := range backdoor.Daemons {
		if backdoor.Systemd && !auditForeignSSHD(daemon.Cgroup) {
			continue
		}
		backdoor.Extra = append(backdoor.Extra, daemon)
	}
	// systemd가 없으면 sshd가 어느 서비스에서 떴는지 모른다. 하나뿐이면 그것이 서비스다.
	if !backdoor.Systemd && len(backdoor.Extra) < 2 {
		backdoor.Extra = nil
	}
	// 소속은 근거에 적는 process에만 찾는다. ssh 서비스의 sshd까지 찾으면 sshd와 openssh-server처럼 이름이 달라 dpkg 목록을 매번 끝까지 읽는다.
	var exes []string
	add := func(process auditBackdoorProcess) {
		exes = append(exes, strings.TrimSuffix(process.Exe, deletedOpenSuffix))
	}
	for _, shell := range backdoor.Shells {
		add(shell.Process)
	}
	for _, client := range backdoor.Clients {
		add(client.Process)
	}
	for _, daemon := range backdoor.Extra {
		add(daemon.Process)
	}
	for _, holder := range backdoor.Raw {
		add(holder.Process)
	}
	if len(exes) == 0 {
		return
	}
	owners, found := auditDpkgOwners(sources, exes)
	backdoor.PackageDB = found
	owner := func(process *auditBackdoorProcess) {
		process.Owner = owners[strings.TrimSuffix(process.Exe, deletedOpenSuffix)]
	}
	for index := range backdoor.Shells {
		owner(&backdoor.Shells[index].Process)
	}
	for index := range backdoor.Clients {
		owner(&backdoor.Clients[index].Process)
	}
	for index := range backdoor.Extra {
		owner(&backdoor.Extra[index].Process)
	}
	// 패키지가 가진 binary는 대개 정상이다(systemd-networkd, dhclient, tcpdump). 패키지 밖이거나 임시 경로에서 도는 것만 남긴다.
	backdoor.Raw = slices.DeleteFunc(backdoor.Raw, func(holder auditRawHolder) bool {
		owner(&holder.Process)
		return holder.Reason == "" && (!found || holder.Process.Owner != "")
	})
	for index := range backdoor.Raw {
		owner(&backdoor.Raw[index].Process)
		if backdoor.Raw[index].Reason == "" {
			backdoor.Raw[index].Reason = "no_package"
		}
	}
}

// auditForeignSSHD는 sshd listener의 cgroup이 ssh 서비스가 아니라는 뜻이다. container 안의 sshd는 판정하지 않는다.
func auditForeignSSHD(cgroup string) bool {
	if slices.ContainsFunc(auditContainerCgroups, func(marker string) bool { return strings.Contains(cgroup, marker) }) {
		return false
	}
	return !slices.Contains(auditSSHServiceUnits, filepath.Base(cgroup))
}

// readAuditCgroup은 cgroup v2 경로다. v1 호스트는 name=systemd 줄의 경로를 쓴다.
func readAuditCgroup(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return ""
	}
	var fallback string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		if fields[0] == "0" && fields[1] == "" {
			return fields[2]
		}
		if fields[1] == "name=systemd" {
			fallback = fields[2]
		}
	}
	return fallback
}

func readAuditArgs(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil || len(strings.Trim(string(data), "\x00")) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
}

func auditMaskedArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return svcTruncate(infoSingleLine(strings.Join(listenMaskArgs(args), " ")), auditLocalSourceArgsLimit)
}

// auditSSHForwards는 ssh 명령 인자에서 목적지와 -R, -w, RemoteForward, Tunnel을 찾는다. 목적지 뒤는 원격 명령이라 보지 않는다.
// 설정 파일(~/.ssh/config)의 RemoteForward는 인자에 없어 보이지 않는다. 그 터널의 listener는 원격 호스트에 있다.
func auditSSHForwards(args []string) (string, []string) {
	var destination string
	var forwards []string
	for index := 1; index < len(args) && destination == ""; index++ {
		arg := args[index]
		if arg == "--" {
			if index+1 < len(args) {
				destination = args[index+1]
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			destination = arg
			break
		}
		for position := 1; position < len(arg); position++ {
			option := arg[position]
			if strings.IndexByte(auditSSHValueOptions, option) < 0 {
				continue
			}
			value := arg[position+1:]
			if value == "" && index+1 < len(args) {
				index++
				value = args[index]
			}
			switch option {
			case 'R', 'w':
				forwards = append(forwards, "-"+string(option)+" "+value)
			case 'o':
				key, setting, _ := strings.Cut(strings.Replace(value, " ", "=", 1), "=")
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "remoteforward":
					forwards = append(forwards, "-o RemoteForward="+strings.TrimSpace(setting))
				case "tunnel":
					if !strings.EqualFold(strings.TrimSpace(setting), "no") {
						forwards = append(forwards, "-o Tunnel="+strings.TrimSpace(setting))
					}
				}
			}
			break
		}
	}
	return destination, forwards
}

// readAuditRawSockets는 packet, raw, raw6 소켓의 inode다. packet 소켓은 마지막 열이, raw 소켓은 열 번째 열이 inode다.
func readAuditRawSockets(sources auditSources) (map[uint64]string, error) {
	sockets := map[uint64]string{}
	read := 0
	for _, table := range []struct{ kind, path string }{{"packet", "/proc/net/packet"}, {"raw", "/proc/net/raw"}, {"raw6", "/proc/net/raw6"}} {
		file, err := os.Open(sources.file(table.path))
		if err != nil {
			continue
		}
		read++
		parseAuditRawTable(file, table.kind, sockets)
		file.Close()
	}
	if read == 0 {
		return nil, fmt.Errorf("/proc/net/packet: %w", os.ErrNotExist)
	}
	return sockets, nil
}

func parseAuditRawTable(reader io.Reader, kind string, sockets map[uint64]string) {
	scanner := bufio.NewScanner(reader)
	scanner.Scan()
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		column := 9
		if kind == "packet" {
			column = len(fields) - 1
		}
		if column < 0 || column >= len(fields) {
			continue
		}
		if inode, err := strconv.ParseUint(fields[column], 10, 64); err == nil && inode != 0 {
			sockets[inode] = kind
		}
	}
}

// auditBackdoorDetails는 process의 명령 인자, 시작 시각, 부모다.
func auditBackdoorDetails(process auditBackdoorProcess, now time.Time, extra ...string) []string {
	details := append([]string{process.Args}, extra...)
	return append(details, strings.Join(auditProcessFacts(process.auditProcess, now), "  "))
}

// auditOwnerNote는 실행 파일의 소속이다. dpkg 기록이 없으면 판단할 근거가 없어 적지 않는다.
func auditOwnerNote(process auditBackdoorProcess, packageDB bool) string {
	switch {
	case process.Owner != "":
		return T("audit.proc.lib_package", process.Owner)
	case packageDB:
		return T("audit.proc.lib_no_package")
	}
	return ""
}

func auditReverseTunnel(facts auditFacts) Result {
	const id = "net.reverse_tunnel"
	backdoor := facts.Backdoor
	if backdoor.Err != nil {
		return auditSkip(id, "/proc", backdoor.Err)
	}
	var result Result
	if len(backdoor.Listeners) == 0 && len(backdoor.Clients) == 0 {
		// ssh -R이 연 포트는 listen 목록에서만 보인다. 그 목록을 읽지 못했으면 없다고 판정할 근거가 없다.
		if facts.ListenErr != nil {
			return auditSkip(id, "sockets", facts.ListenErr)
		}
		result = auditResult(id, StatusPass, T("audit.net.reverse_ok", backdoor.SSHClients, backdoor.SSHDSockets))
	} else {
		result = auditResult(id, StatusWarn, T("audit.net.reverse_bad", len(backdoor.Listeners), len(backdoor.Clients)))
		var listeners, clients []string
		for _, listener := range backdoor.Listeners {
			socket := listener.Socket
			head := fmt.Sprintf("%s %s  pid %s %s", socket.Proto, net.JoinHostPort(socket.Address, strconv.Itoa(socket.Port)), socket.PID, socket.Process)
			listeners = append(listeners, auditItem(head, []string{listener.Title}, []string{T("audit.net.reverse_listener_note")}))
		}
		for _, client := range backdoor.Clients {
			head := fmt.Sprintf("%d ssh  %s", client.Process.PID, client.Destination)
			clients = append(clients, auditItem(head, auditBackdoorDetails(client.Process, facts.Now, strings.Join(client.Forwards, "  ")), []string{T("audit.net.reverse_client_note")}))
		}
		if len(listeners) > 0 {
			result.Evidence = append(result.Evidence, Evidence{Label: T("audit.evidence.tunnel_listeners"), Value: strings.Join(listeners, "\n")})
			result.Next = fmt.Sprintf("ss -tnp | grep 'pid=%s,'", backdoor.Listeners[0].Socket.PID)
		}
		if len(clients) > 0 {
			result.Evidence = append(result.Evidence, Evidence{Label: T("audit.evidence.tunnel_clients"), Value: strings.Join(clients, "\n")})
			if result.Next == "" {
				result.Next = fmt.Sprintf("ps -o pid,ppid,lstart,args -p %d", backdoor.Clients[0].Process.PID)
			}
		}
	}
	if backdoor.X11 > 0 {
		result.Warnings = append(result.Warnings, T("audit.net.reverse_x11", backdoor.X11))
	}
	if facts.ListenErr != nil {
		result.Warnings = append(result.Warnings, T("audit.net.reverse_no_listen", infoSingleLine(facts.ListenErr.Error())))
	}
	return result
}

func auditSocketShellResult(facts auditFacts) Result {
	const id = "proc.socket_shell"
	backdoor := facts.Backdoor
	if backdoor.Err != nil {
		return auditSkip(id, "/proc", backdoor.Err)
	}
	if backdoor.TCPErr != nil {
		return auditSkip(id, "/proc/net/tcp", backdoor.TCPErr)
	}
	if len(backdoor.Shells) == 0 {
		return auditResult(id, StatusPass, T("audit.proc.shell_ok", backdoor.Checked))
	}
	result := auditResult(id, StatusWarn, T("audit.proc.shell_bad", len(backdoor.Shells)))
	items := make([]string, 0, min(len(backdoor.Shells), auditProcessShown)+1)
	for _, shell := range backdoor.Shells[:min(len(backdoor.Shells), auditProcessShown)] {
		process := shell.Process
		fds := make([]string, 0, len(shell.FDs))
		for _, fd := range shell.FDs {
			fds = append(fds, strconv.Itoa(fd))
		}
		head := fmt.Sprintf("%d %s  %s", process.PID, process.Comm, strings.TrimSuffix(process.Exe, deletedOpenSuffix))
		items = append(items, auditItem(head, auditBackdoorDetails(process, facts.Now, T("audit.proc.shell_peer", shell.Peer, strings.Join(fds, ","))),
			[]string{T("audit.proc.shell_note"), auditOwnerNote(process, backdoor.PackageDB)}))
	}
	if extra := len(backdoor.Shells) - auditProcessShown; extra > 0 {
		items = append(items, fmt.Sprintf("+%d", extra))
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.processes"), Value: strings.Join(items, "\n")}}
	pid := backdoor.Shells[0].Process.PID
	result.Next = fmt.Sprintf("ls -l /proc/%d/fd; tr '\\0' ' ' < /proc/%d/cmdline", pid, pid)
	return result
}

func auditRawSocketResult(facts auditFacts) Result {
	const id = "proc.raw_socket"
	backdoor := facts.Backdoor
	if backdoor.Err != nil {
		return auditSkip(id, "/proc", backdoor.Err)
	}
	if backdoor.RawErr != nil {
		return auditSkip(id, "/proc/net/packet", backdoor.RawErr)
	}
	if len(backdoor.Raw) == 0 {
		return auditResult(id, StatusPass, T("audit.proc.raw_ok", backdoor.RawSockets))
	}
	result := auditResult(id, StatusWarn, T("audit.proc.raw_bad", len(backdoor.Raw)))
	items := make([]string, 0, len(backdoor.Raw))
	for _, holder := range backdoor.Raw {
		process := holder.Process
		reason := T("audit.proc.lib_no_package")
		if holder.Reason != "no_package" {
			reason = T("audit.proc.reason." + holder.Reason)
		}
		head := fmt.Sprintf("%d %s  %s", process.PID, process.Comm, strings.TrimSuffix(process.Exe, deletedOpenSuffix))
		items = append(items, auditItem(head, auditBackdoorDetails(process, facts.Now, T("audit.proc.raw_kinds", strings.Join(holder.Kinds, ", "))),
			[]string{reason, T("audit.proc.raw_note")}))
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.processes"), Value: strings.Join(items, "\n")}}
	pid := backdoor.Raw[0].Process.PID
	result.Next = fmt.Sprintf("ls -l /proc/%d/exe; tr '\\0' ' ' < /proc/%d/cmdline", pid, pid)
	return result
}

func auditExtraSSHDResult(facts auditFacts) Result {
	const id = "ssh.extra_sshd"
	backdoor := facts.Backdoor
	if backdoor.Err != nil {
		return auditSkip(id, "/proc", backdoor.Err)
	}
	if len(backdoor.Extra) == 0 {
		return auditResult(id, StatusPass, T("audit.ssh.extra_ok", len(backdoor.Daemons)))
	}
	summary := T("audit.ssh.extra_bad", len(backdoor.Extra))
	if !backdoor.Systemd {
		summary = T("audit.ssh.extra_multi", len(backdoor.Extra))
	}
	result := auditResult(id, StatusWarn, summary)
	items := make([]string, 0, len(backdoor.Extra))
	for _, daemon := range backdoor.Extra {
		process := daemon.Process
		head := fmt.Sprintf("%d sshd  %s", process.PID, strings.Join(daemon.Ports, " "))
		items = append(items, auditItem(head, auditBackdoorDetails(process, facts.Now, "cgroup "+daemon.Cgroup),
			[]string{T("audit.ssh.extra_note"), auditOwnerNote(process, backdoor.PackageDB)}))
	}
	result.Evidence = []Evidence{{Label: T("audit.evidence.processes"), Value: strings.Join(items, "\n")}}
	pid := backdoor.Extra[0].Process.PID
	result.Next = fmt.Sprintf("tr '\\0' ' ' < /proc/%d/cmdline; cat /proc/%d/cgroup", pid, pid)
	return result
}
