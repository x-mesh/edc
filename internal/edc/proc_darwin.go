//go:build darwin

package edc

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procLsofTimeout은 lsof 한 번의 제한이다. fd가 많은 process도 1초 안에 끝났다.
const procLsofTimeout = 10 * time.Second

// readProcDetails는 명령행과 cwd를 libproc에서, fd와 소켓을 lsof에서 읽는다. 다른 계정의 process는 root가 아니면
// lsof가 아무것도 알려 주지 않는다.
func readProcDetails(pid int) procDetails {
	var details procDetails
	details.Args, details.CWD = listenProcessCommand(strconv.Itoa(pid))
	if owner := darwinReadProcessOwner(int32(pid)); owner != nil {
		details.User = owner.user
	}
	ctx, cancel := context.WithTimeout(context.Background(), procLsofTimeout)
	defer cancel()
	stdout, stderr, err := runDarwinCheckTool(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-FtnT")
	// lsof는 일부 fd를 읽지 못해도 1로 끝난다. 출력이 있으면 읽은 만큼 쓴다.
	if strings.TrimSpace(stdout) == "" {
		if err == nil || strings.Contains(stderr, "ermission") || os.Geteuid() != 0 {
			err = os.ErrPermission
		}
		details.FDErr, details.SocketErr = err, err
		return details
	}
	parseLsofFields(stdout, &details)
	return details
}

// parseLsofFields는 lsof -F의 줄이다. f로 fd가 시작하고, t는 종류, n은 이름, TST=는 TCP 상태다.
func parseLsofFields(output string, details *procDetails) {
	details.FDTypes, details.Conns, details.Remotes = map[string]int{}, map[string]int{}, map[string]int{}
	type entry struct{ fd, kind, name, state string }
	var current *entry
	var entries []entry
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			entries = append(entries, entry{fd: line[1:]})
			current = &entries[len(entries)-1]
		case 't':
			if current != nil {
				current.kind = line[1:]
			}
		case 'n':
			if current != nil {
				current.name = line[1:]
			}
		case 'T':
			if current != nil && strings.HasPrefix(line, "TST=") {
				current.state = strings.TrimPrefix(line, "TST=")
			}
		}
	}
	listenPorts := map[string]bool{}
	for _, item := range entries {
		if item.state == "LISTEN" {
			listenPorts[procPort(item.name)] = true
		}
	}
	for _, item := range entries {
		// cwd, txt, mem 같은 항목도 f 줄로 오지만 열린 fd가 아니다.
		if _, err := strconv.Atoi(item.fd); err != nil || item.kind == "" {
			continue
		}
		details.FDTypes[item.kind]++
		if item.state == "" {
			continue
		}
		if item.state == "LISTEN" {
			details.Listen = append(details.Listen, "tcp "+item.name)
			continue
		}
		details.Conns[item.state]++
		if local, remote, ok := strings.Cut(item.name, "->"); ok {
			details.Remotes[procPeer(local, remote, listenPorts)]++
		}
	}
}

// procHandle은 연 때의 process 시작 시각이다. 표본의 시작 시각은 ps의 초 단위 값이라 같은 초 안의 PID 재사용을 가르지 못해서,
// kern.proc.pid의 p_starttime(마이크로초)을 쓴다. 이 값은 거두지 않은 zombie에도 있고, 거둔 뒤에는 읽히지 않는다.
type procHandle struct {
	pid   int
	start unix.Timeval
	ok    bool
}

// openProcHandle은 PID의 시작 시각을 읽어 둔다. 그런 process가 없으면 false다.
func openProcHandle(pid int) (procHandle, bool) {
	start, _, ok := darwinProcessStart(pid)
	return procHandle{pid: pid, start: start, ok: ok}, ok
}

// darwinProcZombie는 kinfo_proc의 p_stat에서 거두지 않은 zombie(SZOMB)다.
const darwinProcZombie = 5

// darwinProcessStart는 process의 시작 시각과 zombie인지를 읽는다. ps도 이 sysctl로 process를 읽는다.
func darwinProcessStart(pid int) (unix.Timeval, bool, bool) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(info.Proc.P_pid) != pid {
		return unix.Timeval{}, false, false
	}
	return info.Proc.P_starttime, info.Proc.P_stat == darwinProcZombie, true
}

// sameProcess는 연 때의 process가 아직 PID를 쥐고 있는지 본다. 표본의 시작 시각은 ps의 초 단위 값이라 초까지만 견준다.
func (handle procHandle) sameProcess(process topProcess) bool {
	if !handle.ok || handle.pid != process.PID {
		return false
	}
	start, zombie, ok := darwinProcessStart(handle.pid)
	if !ok || start != handle.start || (!process.Started.IsZero() && process.Started.Unix() != start.Sec) {
		return false
	}
	// 읽는 동안 끝나 zombie가 된 process는 lsof가 fd를 보여 주지 않는다. 표본에서부터 zombie였던 process만 그대로 보여 준다.
	return !zombie || process.State == "Z"
}

func (procHandle) close() {}
