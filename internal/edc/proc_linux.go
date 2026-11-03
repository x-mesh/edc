//go:build linux

package edc

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readProcDetails(pid int) procDetails {
	return readLinuxProcDetails("/proc", pid)
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
