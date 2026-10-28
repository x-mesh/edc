//go:build linux

package edc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// ss를 띄우는 대신 같은 sock_diag 질의를 직접 보낸다. ss는 실행만으로 수십 ms가 들었다.
// TCP와 UDP는 상태 mask로 kernel이 대기 소켓만 돌려주게 한다. 연결이 많은 서버에서 모든 연결을 덤프하면 ss보다 느려진다.
const (
	linuxTCPListen = 10 // TCP_LISTEN
	// 연결하지 않은 UDP 소켓은 TCP_CLOSE 상태로 보인다. ss는 이를 UNCONN으로 적는다.
	linuxTCPClose = 7

	linuxInetDiagRequestSize = 56 // struct inet_diag_req_v2
	linuxInetDiagMsgSize     = 72 // struct inet_diag_msg
	linuxInetDiagSkV6Only    = 11 // INET_DIAG_SKV6ONLY
)

type linuxInetDiagRequest struct {
	family, protocol uint8
	states           uint32
}

func (request linuxInetDiagRequest) Len() int { return linuxInetDiagRequestSize }

func (request linuxInetDiagRequest) Serialize() []byte {
	data := make([]byte, linuxInetDiagRequestSize)
	data[0], data[1] = request.family, request.protocol
	binary.NativeEndian.PutUint32(data[4:], request.states)
	return data
}

func collectListenSockets(families listenFamilies) ([]listenSocket, error) {
	sockets, err := linuxInetListenSockets(families)
	if err != nil {
		return nil, err
	}
	if families.Unix {
		unixSockets, err := linuxUnixListenSockets()
		if err != nil {
			return nil, fmt.Errorf("sock_diag unix: %w", err)
		}
		sockets = append(sockets, unixSockets...)
	}
	attachLinuxSocketOwners("/proc", sockets)
	return sockets, nil
}

// collectNetnsListenSockets는 pid가 속한 network namespace의 TCP·UDP 대기 소켓을 읽는다. 자기 network namespace를 가진
// container의 소켓은 그 안에만 있어 host에서 덤프하면 보이지 않는다.
func collectNetnsListenSockets(pid int) ([]listenSocket, error) {
	type dump struct {
		sockets []listenSocket
		err     error
	}
	done := make(chan dump, 1)
	go func() {
		// netlink 소켓은 만든 thread의 namespace에 속한다. 읽은 뒤에는 원래 namespace로 되돌린다. 이 goroutine이 main thread에서
		// 돌면 Go runtime은 그 thread를 끝내지 못하고 남겨 두는데, /proc/self/net과 /proc/net은 main thread의 namespace를 보인다.
		// 되돌리지 않으면 process 전체가 그 뒤로 container의 /proc/net을 읽는다. 되돌리지 못했을 때만 UnlockOSThread 없이 끝내
		// runtime이 그 thread를 버리게 한다.
		runtime.LockOSThread()
		original, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			done <- dump{err: err}
			return
		}
		defer original.Close()
		namespace, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "ns", "net"))
		if err != nil {
			runtime.UnlockOSThread()
			done <- dump{err: err}
			return
		}
		err = unix.Setns(int(namespace.Fd()), unix.CLONE_NEWNET)
		namespace.Close()
		if err != nil {
			runtime.UnlockOSThread()
			done <- dump{err: fmt.Errorf("setns %s: %w", namespace.Name(), err)}
			return
		}
		sockets, err := linuxInetListenSockets(listenFamilies{TCP: true, UDP: true})
		if unix.Setns(int(original.Fd()), unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
		done <- dump{sockets, err}
	}()
	result := <-done
	if result.err != nil {
		return nil, result.err
	}
	attachLinuxSocketOwners("/proc", result.sockets)
	return result.sockets, nil
}

// linuxInetListenSockets는 지금 thread가 속한 network namespace의 TCP·UDP 대기 소켓이다. 소켓 주인은 붙이지 않는다.
func linuxInetListenSockets(families listenFamilies) ([]listenSocket, error) {
	var sockets []listenSocket
	type query struct {
		proto    string
		protocol uint8
		state    uint8
		wanted   bool
	}
	for _, q := range []query{{"tcp", unix.IPPROTO_TCP, linuxTCPListen, families.TCP}, {"udp", unix.IPPROTO_UDP, linuxTCPClose, families.UDP}} {
		if !q.wanted {
			continue
		}
		for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
			messages, err := linuxInetDiagDump(family, q.protocol, 1<<q.state)
			if err != nil {
				return nil, fmt.Errorf("sock_diag %s: %w", q.proto, err)
			}
			for _, message := range messages {
				if socket, ok := parseLinuxInetDiagMsg(q.proto, message); ok {
					sockets = append(sockets, socket)
				}
			}
		}
	}
	return sockets, nil
}

func linuxInetDiagDump(family, protocol uint8, states uint32) ([][]byte, error) {
	request := nl.NewNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	request.AddData(linuxInetDiagRequest{family: family, protocol: protocol, states: states})
	messages, err := request.Execute(unix.NETLINK_INET_DIAG, nl.SOCK_DIAG_BY_FAMILY)
	// 덤프 중에 소켓이 바뀌면 kernel이 중단 표시를 붙인다. 받은 목록은 그 순간의 근사치로 쓴다. ss도 같은 경우에 받은 것을 출력한다.
	if err != nil && !errors.Is(err, nl.ErrDumpInterrupted) {
		return nil, err
	}
	return messages, nil
}

// parseLinuxInetDiagMsg는 inet_diag_msg 하나를 읽는다. 포트는 network byte order이고 나머지 정수는 host byte order다.
func parseLinuxInetDiagMsg(proto string, message []byte) (listenSocket, bool) {
	if len(message) < linuxInetDiagMsgSize {
		return listenSocket{}, false
	}
	port := int(binary.BigEndian.Uint16(message[4:]))
	// UDP는 bind하지 않은 소켓도 UNCONN으로 온다. 보여 줄 포트가 없다.
	if port == 0 {
		return listenSocket{}, false
	}
	var address netip.Addr
	if message[0] == unix.AF_INET {
		address = netip.AddrFrom4([4]byte(message[8:12]))
	} else {
		address = netip.AddrFrom16([16]byte(message[8:24]))
	}
	text := address.String()
	// ss는 IPv4도 받는 IPv6 wildcard를 *로 적는다. IPV6_V6ONLY 소켓의 wildcard는 ::로 남는다.
	if message[0] == unix.AF_INET6 && address.IsUnspecified() && !linuxInetDiagV6Only(message[linuxInetDiagMsgSize:]) {
		text = "*"
	}
	return listenSocket{
		Proto: proto, Address: text, Port: port,
		RecvQ:    int(binary.NativeEndian.Uint32(message[56:])),
		SendQ:    int(binary.NativeEndian.Uint32(message[60:])),
		HasQueue: true,
		User:     lookupUserName(strconv.FormatUint(uint64(binary.NativeEndian.Uint32(message[64:])), 10)),
		inode:    binary.NativeEndian.Uint32(message[68:]),
	}, true
}

// linuxInetDiagV6Only는 inet_diag_msg 뒤의 attribute에서 INET_DIAG_SKV6ONLY를 찾는다. kernel은 IPv6 소켓에 늘 붙인다.
func linuxInetDiagV6Only(attributes []byte) bool {
	v6only := false
	forEachLinuxDiagAttribute(attributes, func(kind uint16, payload []byte) {
		if kind == linuxInetDiagSkV6Only && len(payload) > 0 {
			v6only = payload[0] != 0
		}
	})
	return v6only
}

// struct unix_diag_req와 unix_diag_msg의 크기, 그리고 요청할 attribute다. 경로, 큐, 소유 계정을 받는다.
const (
	linuxUnixDiagRequestSize = 24
	linuxUnixDiagMsgSize     = 16
	linuxUnixDiagShowName    = 0x01 // UDIAG_SHOW_NAME
	linuxUnixDiagShowRQLen   = 0x10 // UDIAG_SHOW_RQLEN
	linuxUnixDiagShowUID     = 0x40 // UDIAG_SHOW_UID
	linuxUnixDiagName        = 0    // UNIX_DIAG_NAME
	linuxUnixDiagRQLen       = 4    // UNIX_DIAG_RQLEN
	linuxUnixDiagUID         = 7    // UNIX_DIAG_UID
)

type linuxUnixDiagRequest struct{}

func (linuxUnixDiagRequest) Len() int { return linuxUnixDiagRequestSize }

func (linuxUnixDiagRequest) Serialize() []byte {
	data := make([]byte, linuxUnixDiagRequestSize)
	data[0] = unix.AF_UNIX
	binary.NativeEndian.PutUint32(data[4:], 1<<linuxTCPListen)
	binary.NativeEndian.PutUint32(data[12:], linuxUnixDiagShowName|linuxUnixDiagShowRQLen|linuxUnixDiagShowUID)
	return data
}

// vishvananda/netlink의 UnixSocketDiagInfo는 이름 뒤에 다음 attribute의 바이트를 붙이고 목록 일부만 돌려줘서 쓰지 않는다.
func linuxUnixListenSockets() ([]listenSocket, error) {
	request := nl.NewNetlinkRequest(nl.SOCK_DIAG_BY_FAMILY, unix.NLM_F_DUMP)
	request.AddData(linuxUnixDiagRequest{})
	messages, err := request.Execute(unix.NETLINK_SOCK_DIAG, nl.SOCK_DIAG_BY_FAMILY)
	if err != nil && !errors.Is(err, nl.ErrDumpInterrupted) {
		return nil, err
	}
	var sockets []listenSocket
	for _, message := range messages {
		if socket, ok := parseLinuxUnixDiagMsg(message); ok {
			sockets = append(sockets, socket)
		}
	}
	return sockets, nil
}

func parseLinuxUnixDiagMsg(message []byte) (listenSocket, bool) {
	if len(message) < linuxUnixDiagMsgSize || message[2] != linuxTCPListen {
		return listenSocket{}, false
	}
	// 이름 없는 소켓은 ss와 같이 *로 적는다.
	socket := listenSocket{Proto: "unix", Address: "*", inode: binary.NativeEndian.Uint32(message[4:])}
	forEachLinuxDiagAttribute(message[linuxUnixDiagMsgSize:], func(kind uint16, payload []byte) {
		switch {
		case kind == linuxUnixDiagName && len(payload) > 0:
			socket.Address = linuxUnixSocketName(payload)
		case kind == linuxUnixDiagRQLen && len(payload) >= 8:
			socket.RecvQ = int(binary.NativeEndian.Uint32(payload))
			socket.SendQ = int(binary.NativeEndian.Uint32(payload[4:]))
			socket.HasQueue = true
		case kind == linuxUnixDiagUID && len(payload) >= 4:
			socket.User = lookupUserName(strconv.FormatUint(uint64(binary.NativeEndian.Uint32(payload)), 10))
		}
	})
	return socket, true
}

// linuxUnixSocketName은 sun_path를 사람이 읽는 이름으로 바꾼다. abstract 이름은 NUL로 시작하므로 ss와 같이 @로 적는다.
func linuxUnixSocketName(path []byte) string {
	if path[0] == 0 {
		return "@" + string(path[1:])
	}
	if end := bytes.IndexByte(path, 0); end >= 0 {
		path = path[:end]
	}
	return string(path)
}

// forEachLinuxDiagAttribute는 sock_diag 응답 뒤에 붙은 netlink attribute를 차례로 넘긴다.
func forEachLinuxDiagAttribute(attributes []byte, visit func(kind uint16, payload []byte)) {
	for len(attributes) >= unix.SizeofRtAttr {
		length := int(binary.NativeEndian.Uint16(attributes[0:]))
		if length < unix.SizeofRtAttr || length > len(attributes) {
			return
		}
		visit(binary.NativeEndian.Uint16(attributes[2:]), attributes[unix.SizeofRtAttr:length])
		attributes = attributes[min(len(attributes), (length+unix.RTA_ALIGNTO-1)&^(unix.RTA_ALIGNTO-1)):]
	}
}

type linuxSocketOwner struct{ pid, fd int }

// attachLinuxSocketOwners는 /proc/*/fd에서 socket inode의 주인을 찾는다. 다른 계정의 fd는 root가 아니면 읽을 수 없어
// 그 소켓은 process 없이 남는다. ss -p도 같다.
//
// 한 소켓을 여러 process가 가지면 ss는 pid가 큰 쪽, 같은 pid에서는 fd가 큰 쪽을 먼저 적는다. ss와 같은 주인을 고른다.
func attachLinuxSocketOwners(procRoot string, sockets []listenSocket) {
	owners := map[uint32]linuxSocketOwner{}
	for _, socket := range sockets {
		owners[socket.inode] = linuxSocketOwner{pid: -1}
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return
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
		for _, fdEntry := range fds {
			target, err := os.Readlink(filepath.Join(directory, fdEntry.Name()))
			if err != nil {
				continue
			}
			value, found := strings.CutPrefix(target, "socket:[")
			if !found {
				continue
			}
			inode, err := strconv.ParseUint(strings.TrimSuffix(value, "]"), 10, 32)
			if err != nil {
				continue
			}
			current, wanted := owners[uint32(inode)]
			fd, err := strconv.Atoi(fdEntry.Name())
			if !wanted || err != nil {
				continue
			}
			if pid > current.pid || (pid == current.pid && fd > current.fd) {
				owners[uint32(inode)] = linuxSocketOwner{pid: pid, fd: fd}
			}
		}
	}
	names := map[int]string{}
	for index := range sockets {
		owner := owners[sockets[index].inode]
		if owner.pid < 0 {
			continue
		}
		name, cached := names[owner.pid]
		if !cached {
			data, _ := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(owner.pid), "comm"))
			name = strings.TrimSpace(string(data))
			names[owner.pid] = name
		}
		sockets[index].Process, sockets[index].PID, sockets[index].FD = name, strconv.Itoa(owner.pid), strconv.Itoa(owner.fd)
	}
}

func listenProcessCommand(pid string) ([]string, string) {
	return readLinuxProcessCommand("/proc", pid)
}

// readLinuxProcessCommand는 /proc/<pid>/cmdline의 argv와 cwd 링크를 읽는다. 커널 thread는 cmdline이 비고, 다른 계정의
// cwd는 root가 아니면 읽을 수 없어 비운다.
func readLinuxProcessCommand(procRoot, pid string) ([]string, string) {
	directory := filepath.Join(procRoot, pid)
	var args []string
	if data, err := os.ReadFile(filepath.Join(directory, "cmdline")); err == nil && len(bytes.Trim(data, "\x00")) != 0 {
		args = strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	}
	cwd, _ := os.Readlink(filepath.Join(directory, "cwd"))
	return args, cwd
}
