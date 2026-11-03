//go:build darwin

package edc

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
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
