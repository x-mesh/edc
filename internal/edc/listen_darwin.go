//go:build darwin

package edc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// lsof도 같은 libproc 호출로 소켓을 읽지만 별도 process를 띄우고 모든 fd를 글로 바꾼다. 여기서는 소켓 fd만 읽는다.
// 다른 계정의 process는 root가 아니면 lsof와 마찬가지로 fd 목록을 읽을 수 없어 빠진다.
var procListAllPIDsAddr, procPIDFDInfoAddr uintptr

//go:cgo_import_dynamic libc_proc_listallpids proc_listallpids "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_proc_pidfdinfo proc_pidfdinfo "/usr/lib/libSystem.B.dylib"

const (
	darwinProcPIDListFDs        = 1 // PROC_PIDLISTFDS
	darwinProcPIDTBSDInfo       = 3 // PROC_PIDTBSDINFO
	darwinProcPIDFDSocketInfo   = 3 // PROC_PIDFDSOCKETINFO
	darwinProxFDTypeSocket      = 2 // PROX_FDTYPE_SOCKET
	darwinProcFDInfoSize        = 8 // struct proc_fdinfo
	darwinSocketKindIn          = 1 // SOCKINFO_IN
	darwinSocketKindTCP         = 2 // SOCKINFO_TCP
	darwinSocketKindUnix        = 3 // SOCKINFO_UN
	darwinTCPStateListen        = 1 // TSI_S_LISTEN
	darwinInIPv4                = 1 // INI_IPV4
	darwinSocketOptionAcceptCon = 2 // SO_ACCEPTCONN
	darwinIPProtoUDP            = 17
)

// socket_fdinfo와 proc_bsdinfo는 Go 구조체로 옮기지 않고 SDK 헤더의 offsetof 값으로 읽는다. 중첩 union과
// 정렬을 Go 구조체로 흉내 내면 틀려도 오류 없이 엉뚱한 값을 읽는다. 값은 macOS SDK에서 arm64와 x86_64
// 모두 같음을 확인했다.
const (
	darwinSocketFDInfoSize = 792
	darwinSoiType          = 176
	darwinSoiProtocol      = 180
	darwinSoiOptions       = 188
	darwinSoiQLen          = 194
	darwinSoiQLimit        = 198
	darwinSoiRecvCC        = 208
	darwinSoiSendCC        = 232
	darwinSoiKind          = 256
	darwinSoiProto         = 264
	// in_sockinfo 안의 위치다. tcp_sockinfo는 in_sockinfo로 시작한다.
	darwinInFPort    = 0
	darwinInLPort    = 4
	darwinInVFlag    = 24
	darwinInLAddr    = 48
	darwinInLAddr4   = 60
	darwinTCPState   = 80
	darwinUnixPath   = 16 + 2 // unsi_addr의 sockaddr_un.sun_path
	darwinUnixPathSz = 104

	darwinBSDInfoSize = 136
	darwinBSDUID      = 20
	darwinBSDComm     = 48
	darwinBSDName     = 64
	darwinBSDCommSz   = 16
	darwinBSDNameSz   = 32

	darwinProcPIDVnodePathInfo = 9    // PROC_PIDVNODEPATHINFO
	darwinVnodePathInfoSize    = 2352 // struct proc_vnodepathinfo
	darwinVnodeInfoSize        = 152  // pvi_cdir.vip_vi. 작업 디렉터리 경로가 바로 뒤에 온다.
	darwinMaxPathLen           = 1024
	darwinProcArgsMaxArgs      = 4096
)

func collectListenSockets(families listenFamilies) ([]listenSocket, error) {
	pids, err := darwinListAllPIDs()
	if err != nil {
		return nil, err
	}
	var sockets []listenSocket
	var fds []byte
	info := make([]byte, darwinSocketFDInfoSize)
	for _, pid := range pids {
		fds = darwinListFDs(pid, fds)
		var owner *darwinProcessOwner
		for offset := 0; offset+darwinProcFDInfoSize <= len(fds); offset += darwinProcFDInfoSize {
			if binary.LittleEndian.Uint32(fds[offset+4:]) != darwinProxFDTypeSocket {
				continue
			}
			fd := int32(binary.LittleEndian.Uint32(fds[offset:]))
			// fd를 목록으로 받은 뒤 닫혔으면 읽기가 실패한다. 그 사이에 사라진 소켓은 관측 대상이 아니다.
			if !darwinReadSocketInfo(pid, fd, info) {
				continue
			}
			socket, ok := parseDarwinSocketInfo(info, families)
			if !ok {
				continue
			}
			if owner == nil {
				owner = darwinReadProcessOwner(pid)
			}
			socket.PID, socket.FD = strconv.Itoa(int(pid)), strconv.Itoa(int(fd))
			socket.Process, socket.User = owner.name, owner.user
			sockets = append(sockets, socket)
		}
	}
	return sockets, nil
}

// parseDarwinSocketInfo는 socket_fdinfo 하나에서 연결을 기다리는 소켓만 고른다. TCP와 유닉스 스트림은
// LISTEN 상태로, UDP는 상대 포트가 없는 것으로 가려낸다. ss가 고르는 기준과 같다.
func parseDarwinSocketInfo(info []byte, families listenFamilies) (listenSocket, bool) {
	if len(info) < darwinSocketFDInfoSize {
		return listenSocket{}, false
	}
	proto := info[darwinSoiProto:]
	switch binary.LittleEndian.Uint32(info[darwinSoiKind:]) {
	case darwinSocketKindTCP:
		if !families.TCP || int32(binary.LittleEndian.Uint32(proto[darwinTCPState:])) != darwinTCPStateListen {
			return listenSocket{}, false
		}
		address, port := darwinInAddress(proto)
		socket := listenSocket{Proto: "tcp", Address: address, Port: port}
		// TCP listener의 큐는 ss와 같이 accept를 기다리는 연결 수와 그 한계다.
		socket.RecvQ = int(int16(binary.LittleEndian.Uint16(info[darwinSoiQLen:])))
		socket.SendQ = int(int16(binary.LittleEndian.Uint16(info[darwinSoiQLimit:])))
		socket.HasQueue = true
		return socket, true
	case darwinSocketKindIn:
		if !families.UDP || int32(binary.LittleEndian.Uint32(info[darwinSoiProtocol:])) != darwinIPProtoUDP {
			return listenSocket{}, false
		}
		// 상대 포트가 있으면 connect한 UDP 흐름(QUIC 등)이다.
		if binary.BigEndian.Uint16(proto[darwinInFPort:]) != 0 {
			return listenSocket{}, false
		}
		address, port := darwinInAddress(proto)
		// 포트가 0이면 아직 bind하지 않은 소켓이다. 보여 줄 포트가 없다.
		if port == 0 {
			return listenSocket{}, false
		}
		// UDP의 큐는 ss와 같이 수신·송신 버퍼에 쌓인 바이트다.
		return listenSocket{
			Proto: "udp", Address: address, Port: port,
			RecvQ:    int(binary.LittleEndian.Uint32(info[darwinSoiRecvCC:])),
			SendQ:    int(binary.LittleEndian.Uint32(info[darwinSoiSendCC:])),
			HasQueue: true,
		}, true
	case darwinSocketKindUnix:
		if !families.Unix || binary.LittleEndian.Uint16(info[darwinSoiOptions:])&darwinSocketOptionAcceptCon == 0 {
			return listenSocket{}, false
		}
		path := proto[darwinUnixPath : darwinUnixPath+darwinUnixPathSz]
		if end := bytes.IndexByte(path, 0); end >= 0 {
			path = path[:end]
		}
		return listenSocket{
			Proto: "unix", Address: string(path),
			RecvQ:    int(int16(binary.LittleEndian.Uint16(info[darwinSoiQLen:]))),
			SendQ:    int(int16(binary.LittleEndian.Uint16(info[darwinSoiQLimit:]))),
			HasQueue: true,
		}, true
	}
	return listenSocket{}, false
}

// darwinInAddress는 in_sockinfo의 로컬 주소와 포트를 읽는다. 포트는 network byte order로 int의 앞 두 바이트에 있다.
// 모든 주소에 바인드한 소켓은 lsof와 ss가 적던 대로 *로 적는다.
func darwinInAddress(proto []byte) (string, int) {
	port := int(binary.BigEndian.Uint16(proto[darwinInLPort:]))
	var address netip.Addr
	if proto[darwinInVFlag]&darwinInIPv4 != 0 {
		address = netip.AddrFrom4([4]byte(proto[darwinInLAddr4 : darwinInLAddr4+4]))
	} else {
		address = netip.AddrFrom16([16]byte(proto[darwinInLAddr : darwinInLAddr+16]))
	}
	if address.IsUnspecified() {
		return "*", port
	}
	return address.String(), port
}

func darwinListAllPIDs() ([]int32, error) {
	count, _, errno := darwinSyscall6(procListAllPIDsAddr, 0, 0, 0, 0, 0, 0)
	if int32(count) <= 0 {
		return nil, fmt.Errorf("proc_listallpids: %w", errno)
	}
	// 두 호출 사이에 process가 늘 수 있어 여유를 둔다. 넘친 process는 다음 관측에서 보인다.
	pids := make([]int32, int(int32(count))+64)
	count, _, errno = darwinSyscall6(procListAllPIDsAddr, uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)*4), 0, 0, 0, 0)
	if int32(count) <= 0 {
		return nil, fmt.Errorf("proc_listallpids: %w", errno)
	}
	return pids[:min(int(int32(count)), len(pids))], nil
}

// darwinListFDs는 process의 fd 목록을 buffer에 받는다. 읽을 수 없는 process(다른 계정, 이미 끝남)는 빈 목록이다.
func darwinListFDs(pid int32, buffer []byte) []byte {
	size, _, _ := darwinSyscall6(procPIDInfoAddr, uintptr(pid), darwinProcPIDListFDs, 0, 0, 0, 0)
	if int32(size) <= 0 {
		return buffer[:0]
	}
	if cap(buffer) < int(int32(size)) {
		buffer = make([]byte, int(int32(size)))
	}
	buffer = buffer[:int(int32(size))]
	read, _, _ := darwinSyscall6(procPIDInfoAddr, uintptr(pid), darwinProcPIDListFDs, 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0)
	if int32(read) <= 0 {
		return buffer[:0]
	}
	return buffer[:min(int(int32(read)), len(buffer))]
}

func darwinReadSocketInfo(pid, fd int32, info []byte) bool {
	read, _, _ := darwinSyscall6(procPIDFDInfoAddr, uintptr(pid), uintptr(fd), darwinProcPIDFDSocketInfo, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)), 0)
	return int32(read) == darwinSocketFDInfoSize
}

type darwinProcessOwner struct{ name, user string }

// darwinReadProcessOwner는 process 이름과 계정을 읽는다. pbi_name이 비면 16자로 잘린 pbi_comm을 쓴다.
func darwinReadProcessOwner(pid int32) *darwinProcessOwner {
	info := make([]byte, darwinBSDInfoSize)
	read, _, _ := darwinSyscall6(procPIDInfoAddr, uintptr(pid), darwinProcPIDTBSDInfo, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)), 0)
	if int32(read) != darwinBSDInfoSize {
		return &darwinProcessOwner{}
	}
	name := darwinCString(info[darwinBSDName : darwinBSDName+darwinBSDNameSz])
	if name == "" {
		name = darwinCString(info[darwinBSDComm : darwinBSDComm+darwinBSDCommSz])
	}
	uid := binary.LittleEndian.Uint32(info[darwinBSDUID:])
	return &darwinProcessOwner{name: name, user: lookupUserName(strconv.FormatUint(uint64(uid), 10))}
}

func darwinCString(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}

// listenProcessCommand는 process의 argv와 작업 디렉터리를 읽는다. ps도 argv를 kern.procargs2에서 읽는다. 다른 계정의
// process는 root가 아니면 둘 다 읽을 수 없어 비운다.
func listenProcessCommand(pid string) ([]string, string) {
	number, err := strconv.Atoi(pid)
	if err != nil {
		return nil, ""
	}
	var args []string
	if raw, err := unix.SysctlRaw("kern.procargs2", number); err == nil {
		args = parseDarwinProcArgs(raw)
	}
	info := make([]byte, darwinVnodePathInfoSize)
	read, _, _ := darwinSyscall6(procPIDInfoAddr, uintptr(number), darwinProcPIDVnodePathInfo, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)), 0)
	cwd := ""
	if int32(read) == darwinVnodePathInfoSize {
		cwd = darwinCString(info[darwinVnodeInfoSize : darwinVnodeInfoSize+darwinMaxPathLen])
	}
	return args, cwd
}

// parseDarwinProcArgs는 kern.procargs2의 argv만 꺼낸다. 값은 argc, 실행 파일 경로, 정렬용 NUL, argv, 환경 변수 순이다.
// 환경 변수에는 비밀이 들기 쉬워 argc개를 넘겨 읽지 않는다.
func parseDarwinProcArgs(raw []byte) []string {
	if len(raw) < 4 {
		return nil
	}
	argc := int(binary.LittleEndian.Uint32(raw))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 || argc <= 0 || argc > darwinProcArgsMaxArgs {
		return nil
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	args := make([]string, 0, min(argc, 64))
	for len(args) < argc && len(rest) != 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			args = append(args, string(rest))
			break
		}
		args = append(args, string(rest[:end]))
		rest = rest[end+1:]
	}
	return args
}
