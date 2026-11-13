//go:build darwin

package edc

import (
	"context"
	"io/fs"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// macOS audit는 계정을 Directory Services에서, 방화벽을 Application Firewall과 pf에서 읽는다.
// darwinAuditUnsupported의 점검은 macOS 원천이 없어 skip이다.
func hostAuditSources() (auditSources, bool) {
	euid := os.Geteuid()
	return auditSources{
		root:     "/",
		euid:     euid,
		pid:      os.Getpid(),
		path:     os.Getenv("PATH"),
		listen:   func() ([]listenSocket, error) { return collectListenSockets(listenFamilies{TCP: true, UDP: true}) },
		firewall: readDarwinFirewall,
		owner: func(info fs.FileInfo) (uint32, bool) {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false
			}
			return stat.Uid, true
		},
		ctime: func(info fs.FileInfo) (time.Time, bool) {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return time.Time{}, false
			}
			return time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec), true
		},
		interfaceAddrs: readHostInterfaceAddrs,
		accounts: auditAccountSource{
			label:       "dscl",
			read:        readDarwinAccounts,
			external:    readDarwinExternalAccounts,
			externalKey: "audit.account.external_ds",
			loginUID:    darwinLoginUID,
		},
		ipForward:      readDarwinIPForward,
		ipForwardName:  "net.inet.ip.forwarding",
		listenPartial:  euid != 0,
		firewallAsUser: true,
		firewallSource: "socketfilterfw/pfctl",
		// Docker Desktop과 OrbStack은 Linux VM 안에서 컨테이너를 돌려 호스트 forwarding을 쓰지 않는다. /var/run/docker.sock은 그 VM으로 가는 symlink다.
		vmContainers: true,
		unsupported:  darwinAuditUnsupported,
	}, true
}

// readDarwinAccounts는 로컬 노드만 읽는다. /etc/passwd는 single-user mode에서만 쓰여 사용자 계정이 없다.
func readDarwinAccounts() ([]auditPasswd, error) {
	output, _, err := runDarwinCheckTool(context.Background(), "/usr/bin/dscl", ".", "-readall", "/Users", "RecordName", "UniqueID", "PrimaryGroupID", "NFSHomeDirectory", "UserShell")
	if err != nil {
		return nil, err
	}
	return parseDsclUsers(output), nil
}

func readDarwinExternalAccounts() string {
	output, _, err := runDarwinCheckTool(context.Background(), "/usr/bin/dscl", "/Search", "-read", "/", "CSPSearchPath")
	if err != nil {
		return ""
	}
	return parseDsclSearchPath(output)
}

func readDarwinIPForward() (string, error) {
	value, err := unix.SysctlUint32("net.inet.ip.forwarding")
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(value), 10), nil
}

// readDarwinFirewall은 root가 아니어도 ALF를 읽는다. pf는 /dev/pf를 열어야 해서 root가 필요하다.
// ALF가 꺼져 있고 pf를 읽지 못했으면 들어오는 연결을 막는지 알 수 없다.
func readDarwinFirewall(ctx context.Context) (auditFirewall, error) {
	output, _, err := runDarwinCheckTool(ctx, "/usr/libexec/ApplicationFirewall/socketfilterfw", "--getglobalstate", "--getblockall")
	if err != nil {
		return auditFirewall{}, err
	}
	state, err := parseSocketFilterState(output)
	if err != nil {
		return auditFirewall{}, err
	}
	if os.Geteuid() != 0 {
		if state == darwinALFOff {
			return auditFirewall{}, errAuditNeedsRoot
		}
		return darwinAuditFirewall(state, false, false, pfInputRules{}), nil
	}
	output, _, err = runDarwinCheckTool(ctx, "/sbin/pfctl", "-s", "info")
	if err != nil {
		return auditFirewall{}, err
	}
	enabled, err := parsePFStatus(output)
	if err != nil {
		return auditFirewall{}, err
	}
	var rules pfInputRules
	if enabled {
		output, _, err = runDarwinCheckTool(ctx, "/sbin/pfctl", "-s", "rules")
		if err != nil {
			return auditFirewall{}, err
		}
		rules = parsePFInputRules(output)
	}
	return darwinAuditFirewall(state, true, enabled, rules), nil
}
