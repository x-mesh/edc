//go:build linux

package edc

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func readProcDetails(pid int) procDetails {
	details := readLinuxProcDetails("/proc", pid)
	details.Origin = readLinuxProcOrigin("/", pid)
	return details
}

// procHandle은 열어 둔 /proc/<pid> 디렉터리다. 이 fd는 연 때의 process에 묶여서, 그 process가 끝나면 다른 process가 PID를 다시
// 써도 이 fd로는 파일을 열지 못한다(ESRCH). 시작 tick은 clock tick 단위라 같은 tick 안에 PID를 다시 쓴 process와 같을 수 있어서
// 이 fd로 가른다. 끝에 이 fd로 stat을 읽을 수 있으면, 연 때부터 그때까지 /proc/<pid> 경로로 읽은 값은 모두 그 process 것이다.
type procHandle struct{ dir *os.File }

// openProcHandle은 PID의 /proc 디렉터리를 연다. 그런 process가 없으면 false다.
func openProcHandle(pid int) (procHandle, bool) { return openLinuxProcHandle("/proc", pid) }

func openLinuxProcHandle(procRoot string, pid int) (procHandle, bool) {
	dir, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid)))
	if err != nil {
		return procHandle{}, false
	}
	return procHandle{dir: dir}, true
}

// sameProcess는 열어 둔 process가 아직 PID를 쥐고 있고 시작 tick이 표본과 같은지 본다. 끝난 뒤 거두지 않은 zombie도 PID를 쥐므로
// 같은 process로 본다.
func (handle procHandle) sameProcess(process topProcess) bool {
	if handle.dir == nil {
		return false
	}
	fd, err := syscall.Openat(int(handle.dir.Fd()), "stat", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	file := os.NewFile(uintptr(fd), "stat")
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	stat, ok := parseLinuxProcessStat(process.PID, string(data))
	return ok && stat.StartTicks == process.startTicks
}

func (handle procHandle) close() {
	if handle.dir != nil {
		handle.dir.Close()
	}
}

// readLinuxProcOrigin은 root 아래의 /proc/<pid>/exe, dpkg 파일 목록, logind 세션 기록을 읽는다. 읽지 못한 값은 비우고, 그 줄은
// 나오지 않는다.
func readLinuxProcOrigin(root string, pid int) procOrigin {
	var origin procOrigin
	directory := filepath.Join(root, "proc", strconv.Itoa(pid))
	if exe, err := os.Readlink(filepath.Join(directory, "exe")); err == nil {
		origin.Exe = exe
		if path, ok := linuxProcExePath(root, directory, exe); ok {
			owners, packageDB := auditDpkgOwners(auditSources{root: root}, []string{path})
			origin.PackageDB, origin.Package = packageDB, owners[path]
			if origin.Package != "" {
				origin.PackageChanged = readLinuxDpkgListTime(root, origin.Package)
			}
		}
	}
	origin.Session = readLinuxLoginSession(root, directory)
	return origin
}

// linuxProcExePath는 dpkg에서 찾을 실행 파일 경로다. 커널은 지워진 실행 파일의 링크에 " (deleted)"를 붙이지만, 파일 이름 자체가
// 그렇게 끝날 수도 있다. 그 이름의 파일이 없거나 실행 중인 파일과 다를 때만 접미사를 뗀다. 둘 중 어느 쪽인지 확인하지 못하면
// 다른 파일의 패키지를 출처로 적지 않도록 경로를 내지 않는다.
func linuxProcExePath(root, directory, exe string) (string, bool) {
	path, deleted := strings.CutSuffix(exe, deletedOpenSuffix)
	if !deleted {
		return exe, true
	}
	named, err := os.Stat(filepath.Join(root, exe))
	if errors.Is(err, fs.ErrNotExist) {
		return path, true
	}
	if err != nil {
		return "", false
	}
	running, err := os.Stat(filepath.Join(directory, "exe"))
	if err != nil {
		return "", false
	}
	if os.SameFile(running, named) {
		return exe, true
	}
	return path, true
}

// readLinuxDpkgListTime은 패키지의 파일 목록을 마지막으로 쓴 때다. multiarch 패키지는 <package>:<arch>.list라 둘 다 찾는다.
func readLinuxDpkgListTime(root, name string) time.Time {
	lists, _ := filepath.Glob(filepath.Join(root, "var/lib/dpkg/info", name+".list"))
	arch, _ := filepath.Glob(filepath.Join(root, "var/lib/dpkg/info", name+":*.list"))
	var latest time.Time
	for _, list := range append(lists, arch...) {
		if info, err := os.Stat(list); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// readLinuxProcDetails는 /proc/<pid>의 cmdline, status, fd 링크, 그 process의 network namespace에서 본 TCP 표를 읽는다.
// 다른 계정의 fd는 root가 아니면 읽을 수 없다.
func readLinuxProcDetails(procRoot string, pid int) procDetails {
	directory := filepath.Join(procRoot, strconv.Itoa(pid))
	var details procDetails
	details.Args, details.CWD = readLinuxProcessCommand(procRoot, strconv.Itoa(pid))
	if status, err := os.ReadFile(filepath.Join(directory, "status")); err == nil {
		details.User = linuxStatusUser(string(status))
	}
	entries, err := os.ReadDir(filepath.Join(directory, "fd"))
	if err != nil {
		details.FDErr, details.SocketErr = err, err
		return details
	}
	details.FDTypes = map[string]int{}
	sockets := map[string]bool{}
	for _, entry := range entries {
		link := filepath.Join(directory, "fd", entry.Name())
		target, err := os.Readlink(link)
		if err != nil {
			continue
		}
		kind := linuxFDKind(target)
		details.FDTypes[kind]++
		switch {
		case kind == "socket":
			sockets[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		case kind == "file" && strings.HasSuffix(target, " (deleted)"):
			// memfd와 /dev/shm은 처음부터 이름 없이 쓰는 메모리라 디스크를 붙잡지 않는다.
			path := strings.TrimSuffix(target, " (deleted)")
			if strings.HasPrefix(path, "/memfd:") || strings.HasPrefix(path, "/dev/shm/") {
				continue
			}
			var size uint64
			if info, err := os.Stat(link); err == nil {
				size = uint64(info.Size())
			}
			details.Deleted = append(details.Deleted, procDeletedFile{Path: path, Size: size})
		}
	}
	details.Conns, details.Remotes = map[string]int{}, map[string]int{}
	var rows []linuxTCPRow
	for _, name := range []string{"tcp", "tcp6"} {
		file, err := os.Open(filepath.Join(directory, "net", name))
		if err != nil {
			continue
		}
		for _, row := range parseLinuxTCPTable(file) {
			if sockets[row.inode] {
				rows = append(rows, row)
			}
		}
		file.Close()
	}
	listenPorts := map[string]bool{}
	for _, row := range rows {
		if row.state == "LISTEN" {
			details.Listen = append(details.Listen, fmt.Sprintf("tcp %s", row.local))
			listenPorts[procPort(row.local)] = true
		}
	}
	for _, row := range rows {
		if row.state != "LISTEN" {
			details.Conns[row.state]++
			details.Remotes[procPeer(row.local, row.remote, listenPorts)]++
		}
	}
	return details
}

// linuxFDKind는 /proc/<pid>/fd 링크가 가리키는 대상의 종류다.
func linuxFDKind(target string) string {
	switch {
	case strings.HasPrefix(target, "socket:"):
		return "socket"
	case strings.HasPrefix(target, "pipe:"):
		return "pipe"
	case strings.HasPrefix(target, "anon_inode:"):
		return "anon"
	case strings.HasPrefix(target, "/dev/"):
		return "device"
	}
	return "file"
}

// linuxStatusUser는 status의 Uid 줄에서 실효 UID를 사용자 이름으로 바꾼다.
func linuxStatusUser(status string) string {
	for _, line := range strings.Split(status, "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[0] == "Uid:" {
			return lookupUserName(fields[2])
		}
	}
	return ""
}

type linuxTCPRow struct {
	local, remote, state, inode string
}

// linuxTCPStates는 include/net/tcp_states.h의 번호다.
var linuxTCPStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV", "04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT",
	"07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK", "0A": "LISTEN", "0B": "CLOSING",
}

// parseLinuxTCPTable은 /proc/net/tcp와 tcp6을 읽는다. 열은 sl local_address rem_address st ... uid timeout inode다.
func parseLinuxTCPTable(file io.Reader) []linuxTCPRow {
	var rows []linuxTCPRow
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[0] == "sl" {
			continue
		}
		local, ok := linuxHexEndpoint(fields[1])
		remote, ok2 := linuxHexEndpoint(fields[2])
		if !ok || !ok2 {
			continue
		}
		state := linuxTCPStates[strings.ToUpper(fields[3])]
		if state == "" {
			state = fields[3]
		}
		rows = append(rows, linuxTCPRow{local: local, remote: remote, state: state, inode: fields[9]})
	}
	return rows
}

// linuxHexEndpoint는 "0100007F:1F90" 같은 주소다. 주소는 32비트 단어마다 host byte order이고 포트는 big endian 16진수다.
func linuxHexEndpoint(value string) (string, bool) {
	address, portText, ok := strings.Cut(value, ":")
	if !ok {
		return "", false
	}
	port, err := strconv.ParseUint(portText, 16, 16)
	raw, err2 := hex.DecodeString(address)
	if err != nil || err2 != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", false
	}
	for word := 0; word < len(raw); word += 4 {
		raw[word], raw[word+1], raw[word+2], raw[word+3] = raw[word+3], raw[word+2], raw[word+1], raw[word]
	}
	addr, _ := netip.AddrFromSlice(raw)
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)).String(), true
}
