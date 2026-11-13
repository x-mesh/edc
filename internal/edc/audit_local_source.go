package edc

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 터널이나 중계를 거친 ssh 로그인은 sshd에 출발지가 이 호스트로 보인다. OpenSSH 10.0p2에서 잰 모양이다.
// 연결이 열려 있는 동안에는 양쪽 소켓이 같은 호스트에 있어, 서버 쪽 소켓의 상대편을 쥔 process가 로그인을 나른 process다.

// auditLocalSourceShown은 근거에 적는 줄 수의 상한이다. 열린 연결과 닫힌 로그인 묶음을 합해 센다.
const auditLocalSourceShown = 10

// auditLocalSourcePeers는 로그인 하나에 적는 상대 process 수의 상한이다.
const auditLocalSourcePeers = 3

// auditLocalSourceArgsLimit은 상대 process의 명령 인자를 자르는 글자 수다.
const auditLocalSourceArgsLimit = 160

var errAuditNoSource = errors.New("no source")

// auditTCPSocket은 연결된(ESTABLISHED) TCP 소켓이다.
type auditTCPSocket struct {
	Local, Remote netip.AddrPort
	Inode         uint64
}

type auditLocalPeer struct {
	PID             int
	Comm, Exe, Args string
	// Via는 상대 process가 sshd 포트로 받은 다른 연결의 출발지다. ViaLogin은 그 연결의 Accepted이고, journal 범위 밖이면 nil이다.
	Via      netip.AddrPort
	ViaLogin *auditAccepted
}

// auditLocalLogin은 출발지가 이 호스트인 Accepted다. Open이면 서버 쪽 소켓이 아직 열려 있다.
// Peers는 그 연결의 상대 소켓을 쥔 process이고, PeersMore는 auditLocalSourcePeers를 넘어 적지 않은 수다.
type auditLocalLogin struct {
	auditAccepted
	Addr      netip.Addr
	Open      bool
	Peers     []auditLocalPeer
	PeersMore int
}

type auditLocalSource struct {
	Logins []auditLocalLogin
	// Checked는 출발지를 주소로 읽어 판정한 Accepted의 수이고, Unparsed는 주소로 읽지 못해 판정하지 않은 수다.
	Checked, Unparsed int
	AddrsErr, TCPErr  error
}

func (local auditLocalSource) openCount() int {
	return len(local.Logins) - len(local.closed())
}

func (local auditLocalSource) closed() []auditLocalLogin {
	return slices.DeleteFunc(slices.Clone(local.Logins), func(login auditLocalLogin) bool { return login.Open })
}

func normalizeAuditAddr(addr netip.Addr) netip.Addr { return addr.Unmap().WithZone("") }

func collectAuditLocalSource(sources auditSources, facts *auditFacts) {
	if !facts.JournalRead {
		return
	}
	local := &facts.LocalSource
	own := map[netip.Addr]bool{}
	if sources.interfaceAddrs == nil {
		local.AddrsErr = errAuditNoSource
	} else if addrs, err := sources.interfaceAddrs(); err != nil {
		local.AddrsErr = err
	} else {
		for _, addr := range addrs {
			own[normalizeAuditAddr(addr)] = true
		}
	}
	for _, accepted := range facts.Accepted {
		addr, err := netip.ParseAddr(accepted.Host)
		if err != nil {
			local.Unparsed++
			continue
		}
		local.Checked++
		addr = normalizeAuditAddr(addr)
		if addr.IsLoopback() || own[addr] {
			local.Logins = append(local.Logins, auditLocalLogin{auditAccepted: accepted, Addr: addr})
		}
	}
	collectAuditLocalPeers(sources, local, facts.Accepted)
}

// collectAuditLocalPeers는 끝나지 않은 로그인 가운데 서버 쪽 소켓이 아직 열린 것을 찾고 상대 process를 붙인다.
func collectAuditLocalPeers(sources auditSources, local *auditLocalSource, accepted []auditAccepted) {
	var candidates []int
	for index, login := range local.Logins {
		if login.Until.IsZero() && login.Port != 0 {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return
	}
	if sources.tcpConnected == nil {
		local.TCPErr = errAuditNoSource
		return
	}
	sockets, err := sources.tcpConnected()
	if err != nil {
		local.TCPErr = err
		return
	}
	procRoot := sources.file("/proc")
	peerInodes := map[int]uint64{}
	sshdPorts := map[int]uint16{}
	wanted := map[uint64]bool{}
	for _, index := range candidates {
		login := &local.Logins[index]
		source := netip.AddrPortFrom(login.Addr, uint16(login.Port))
		// 포트는 다른 연결에 재사용될 수 있다. Accepted를 남긴 process가 서버 쪽 소켓을 쥘 때만 그 로그인의 연결이다.
		server, found := auditServerSocket(procRoot, login.PID, source, sockets)
		if !found {
			continue
		}
		login.Open = true
		sshdPorts[index] = server.Local.Port()
		for _, socket := range sockets {
			if socket.Local == source && socket.Remote == server.Local && socket.Inode != 0 {
				peerInodes[index] = socket.Inode
				wanted[socket.Inode] = true
				break
			}
		}
	}
	if len(wanted) == 0 {
		return
	}
	owners := auditSocketOwners(procRoot, wanted)
	for index, inode := range peerInodes {
		pids := owners[inode]
		login := &local.Logins[index]
		for _, pid := range pids[:min(len(pids), auditLocalSourcePeers)] {
			peer := readAuditLocalPeer(procRoot, pid)
			peer.Via, peer.ViaLogin = auditForwardedBy(procRoot, pid, sshdPorts[index], sockets, accepted)
			login.Peers = append(login.Peers, peer)
		}
		login.PeersMore = len(pids) - len(login.Peers)
	}
}

func auditServerSocket(procRoot string, pid int, source netip.AddrPort, sockets []auditTCPSocket) (auditTCPSocket, bool) {
	if pid <= 0 {
		return auditTCPSocket{}, false
	}
	for _, socket := range sockets {
		if socket.Remote == source && socket.Inode != 0 && auditHoldsSocket(procRoot, pid, socket.Inode) {
			return socket, true
		}
	}
	return auditTCPSocket{}, false
}

func auditHoldsSocket(procRoot string, pid int, inode uint64) bool {
	directory := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	want := fmt.Sprintf("socket:[%d]", inode)
	for _, entry := range entries {
		if target, err := os.Readlink(filepath.Join(directory, entry.Name())); err == nil && target == want {
			return true
		}
	}
	return false
}

// auditForwardedBy는 상대 process가 sshd 포트로 받은 다른 연결을 찾는다. ssh -L, -D, -J는 원격 sshd-session이 로그인을 나르므로,
// 그 sshd-session의 바깥 연결이 터널을 연 실제 출발지다. 상대가 sshd가 아니면 그런 소켓이 없어 빈 값을 돌려준다.
func auditForwardedBy(procRoot string, pid int, sshdPort uint16, sockets []auditTCPSocket, accepted []auditAccepted) (netip.AddrPort, *auditAccepted) {
	held := auditHeldSockets(procRoot, pid)
	for _, socket := range sockets {
		if !held[socket.Inode] || socket.Local.Port() != sshdPort {
			continue
		}
		for index := len(accepted) - 1; index >= 0; index-- {
			login := accepted[index]
			addr, err := netip.ParseAddr(login.Host)
			if err == nil && login.Until.IsZero() && login.Port == int(socket.Remote.Port()) && normalizeAuditAddr(addr) == normalizeAuditAddr(socket.Remote.Addr()) {
				return socket.Remote, &login
			}
		}
		return socket.Remote, nil
	}
	return netip.AddrPort{}, nil
}

func auditHeldSockets(procRoot string, pid int) map[uint64]bool {
	held := map[uint64]bool{}
	directory := filepath.Join(procRoot, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return held
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		if value, found := strings.CutPrefix(target, "socket:["); found {
			if inode, err := strconv.ParseUint(strings.TrimSuffix(value, "]"), 10, 64); err == nil {
				held[inode] = true
			}
		}
	}
	return held
}

// auditSocketOwners는 소켓 inode를 쥔 process를 PID 순으로 돌려준다. /proc를 한 번만 훑는다.
func auditSocketOwners(procRoot string, inodes map[uint64]bool) map[uint64][]int {
	owners := map[uint64][]int{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return owners
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		directory := filepath.Join(procRoot, entry.Name(), "fd")
		fds, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(directory, fd.Name()))
			if err != nil {
				continue
			}
			value, found := strings.CutPrefix(target, "socket:[")
			if !found {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(value, "]"), 10, 64)
			if err != nil || !inodes[inode] || slices.Contains(owners[inode], pid) {
				continue
			}
			owners[inode] = append(owners[inode], pid)
		}
	}
	for inode := range owners {
		slices.Sort(owners[inode])
	}
	return owners
}

// readAuditLocalPeer는 읽은 값만 채운다. 훑는 사이에 process가 끝났거나 권한이 없을 수 있어, 읽지 못한 값은 비워 두고 오류로 올리지 않는다.
func readAuditLocalPeer(procRoot string, pid int) auditLocalPeer {
	directory := filepath.Join(procRoot, strconv.Itoa(pid))
	peer := auditLocalPeer{PID: pid}
	peer.Comm, _ = readTrimmed(filepath.Join(directory, "comm"))
	peer.Exe, _ = os.Readlink(filepath.Join(directory, "exe"))
	if data, err := os.ReadFile(filepath.Join(directory, "cmdline")); err == nil && len(strings.Trim(string(data), "\x00")) != 0 {
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		peer.Args = svcTruncate(infoSingleLine(strings.Join(listenMaskArgs(args), " ")), auditLocalSourceArgsLimit)
	}
	return peer
}

func auditLoginLocalSource(facts auditFacts) Result {
	const id = "login.local_source"
	if !facts.JournalRead {
		if facts.JournalErr != nil {
			return auditSkip(id, "journal", facts.JournalErr)
		}
		return auditResult(id, StatusSkip, T("audit.skip.no_journal"))
	}
	local := facts.LocalSource
	var result Result
	if len(local.Logins) == 0 {
		result = auditResult(id, StatusPass, T("audit.login.local_source_ok", local.Checked, auditLoginTime(facts.JournalSince)))
	} else {
		result = auditResult(id, StatusWarn, T("audit.login.local_source_bad", len(local.Logins), local.openCount(), auditLoginTime(facts.JournalSince)))
		result.Evidence = auditLocalSourceEvidence(local)
		result.Next = auditLocalSourceNext(local)
	}
	if local.AddrsErr != nil {
		result.Warnings = append(result.Warnings, T("audit.login.local_addrs", infoSingleLine(local.AddrsErr.Error())))
	}
	if local.TCPErr != nil {
		result.Warnings = append(result.Warnings, T("audit.login.local_tcp", infoSingleLine(local.TCPErr.Error())))
	}
	if local.Unparsed > 0 {
		result.Warnings = append(result.Warnings, T("audit.login.local_unparsed", local.Unparsed))
	}
	if facts.JournalLimited {
		result.Warnings = append(result.Warnings, T("audit.login.journal_limited", auditJournalMaxEntries))
	}
	return result
}

// auditLocalSourceNext는 상대 process가 있으면 그 process를 보는 명령이고, 없으면 가장 이른 로그인 1분 전부터의 journalctl이다.
func auditLocalSourceNext(local auditLocalSource) string {
	for _, login := range local.Logins {
		if login.Open && len(login.Peers) > 0 {
			pid := login.Peers[0].PID
			return fmt.Sprintf("ls -l /proc/%d/exe; cat /proc/%d/cmdline | tr '\\0' ' '", pid, pid)
		}
	}
	return "journalctl -t sshd -t sshd-session --since '" + auditLoginTime(local.Logins[0].Time.Add(-time.Minute)) + "'"
}

func auditLocalSourceEvidence(local auditLocalSource) []Evidence {
	var open []string
	for _, login := range local.Logins {
		if !login.Open {
			continue
		}
		open = append(open, auditLocalOpenItem(login))
	}
	closed := auditLocalClosedLines(local.closed())
	extra := len(open) + len(closed) - auditLocalSourceShown
	if extra > 0 {
		open = open[:min(len(open), auditLocalSourceShown)]
		closed = closed[:max(0, auditLocalSourceShown-len(open))]
	}
	var evidence []Evidence
	if len(open) > 0 {
		evidence = append(evidence, Evidence{Label: T("audit.evidence.open_logins"), Value: strings.Join(open, "\n")})
	}
	if len(closed) > 0 {
		evidence = append(evidence, Evidence{Label: T("audit.evidence.closed_logins"), Value: strings.Join(closed, "\n")})
	}
	if extra > 0 {
		last := &evidence[len(evidence)-1]
		last.Value += fmt.Sprintf("\n+%d", extra)
	}
	return evidence
}

// auditLocalOpenItem은 열린 연결 하나다. 상대 process마다 읽은 값을 적고, 그 process가 나르는 터널의 출처를 설명으로 붙인다.
func auditLocalOpenItem(login auditLocalLogin) string {
	head := fmt.Sprintf("%s  %s  %s  %s", login.User, netip.AddrPortFrom(login.Addr, uint16(login.Port)), login.Method, auditLoginTime(login.Time))
	if len(login.Peers) == 0 {
		return auditItem(head, nil, []string{T("audit.login.peer_none")})
	}
	lines := []string{head}
	for _, peer := range login.Peers {
		fields := []string{T("audit.login.peer"), "pid " + strconv.Itoa(peer.PID)}
		for _, value := range []string{peer.Comm, peer.Exe, peer.Args} {
			if value != "" {
				fields = append(fields, value)
			}
		}
		lines = append(lines, auditDetailPrefix+strings.Join(fields, "  "))
		switch {
		case peer.ViaLogin != nil:
			lines = append(lines, auditNotePrefix+T("audit.login.peer_forward", peer.ViaLogin.User, peer.Via.String(), peer.ViaLogin.Method, auditLoginTime(peer.ViaLogin.Time)))
		case peer.Via.IsValid():
			lines = append(lines, auditNotePrefix+T("audit.login.peer_forward_unknown", peer.Via.String()))
		}
	}
	if login.PeersMore > 0 {
		lines = append(lines, auditDetailPrefix+fmt.Sprintf("+%d", login.PeersMore))
	}
	return strings.Join(lines, "\n")
}

// auditLocalClosedLines는 닫힌 로그인을 사용자, 출발지, 방식으로 묶어 마지막 시각이 늦은 순서로 돌려준다.
func auditLocalClosedLines(closed []auditLocalLogin) []string {
	type group struct {
		user, host, method string
		count              int
		last               time.Time
	}
	groups := map[[3]string]*group{}
	for _, login := range closed {
		key := [3]string{login.User, login.Host, login.Method}
		current := groups[key]
		if current == nil {
			current = &group{user: login.User, host: login.Host, method: login.Method}
			groups[key] = current
		}
		current.count++
		if login.Time.After(current.last) {
			current.last = login.Time
		}
	}
	ordered := make([]*group, 0, len(groups))
	for _, current := range groups {
		ordered = append(ordered, current)
	}
	slices.SortFunc(ordered, func(a, b *group) int {
		return cmp.Or(b.last.Compare(a.last), strings.Compare(a.user+"\x00"+a.host+"\x00"+a.method, b.user+"\x00"+b.host+"\x00"+b.method))
	})
	lines := make([]string, 0, len(ordered))
	for _, current := range ordered {
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %d  %s", current.user, current.host, current.method, current.count, auditLoginTime(current.last)))
	}
	return lines
}
