//go:build linux

package edc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func hostAuditSources() (auditSources, bool) {
	return auditSources{
		root:     "/",
		euid:     os.Geteuid(),
		pid:      os.Getpid(),
		journal:  readHostAuditJournal,
		path:     os.Getenv("PATH"),
		listen:   func() ([]listenSocket, error) { return collectListenSockets(listenFamilies{TCP: true, UDP: true}) },
		firewall: readHostFirewall,
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
			return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec), true
		},
		container: linuxContainer("/", os.Getenv("KUBERNETES_SERVICE_HOST") != ""),

		interfaceAddrs: readHostInterfaceAddrs,
		tcpConnected:   readHostConnectedTCP,
		containers:     readHostAuditContainers,
	}, true
}

func runningKernelRelease() string {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return ""
	}
	return unix.ByteSliceToString(name.Release[:])
}

// readHostFirewall은 nft를 먼저 읽는다. nft에 input chain이 없으면 legacy iptables에만 규칙이 있을 수 있어 iptables-save도 읽는다.
func readHostFirewall(ctx context.Context) (auditFirewall, error) {
	found := false
	if path, err := exec.LookPath("nft"); err == nil {
		found = true
		output, err := exec.CommandContext(ctx, path, "list", "ruleset").Output()
		if err != nil {
			return auditFirewall{}, fmt.Errorf("nft list ruleset: %w", err)
		}
		if firewall := parseNftInputChains(string(output)); firewall.Chains > 0 {
			return firewall, nil
		}
	}
	if path, err := exec.LookPath("iptables-save"); err == nil {
		output, err := exec.CommandContext(ctx, path, "-t", "filter").Output()
		if err != nil {
			return auditFirewall{}, fmt.Errorf("iptables-save: %w", err)
		}
		firewall := parseIPTablesInput(string(output))
		if firewall.Chains > 0 || !found {
			readHostIP6Tables(ctx, &firewall)
			return firewall, nil
		}
	}
	if !found {
		return auditFirewall{}, errAuditNoFirewallTool
	}
	// nft는 있지만 input chain이 IPv4에도 IPv6에도 없다. 두 계열 모두 기본 허용이다.
	return auditFirewall{Tool: "nftables", V6Known: true}, nil
}

// readHostIP6Tables는 IPv6 input 규칙이다. iptables-save는 IPv4만 적고 IPv6는 ip6tables-save가 따로 적는다.
func readHostIP6Tables(ctx context.Context, firewall *auditFirewall) {
	path, err := exec.LookPath("ip6tables-save")
	if err != nil {
		return
	}
	output, err := exec.CommandContext(ctx, path, "-t", "filter").Output()
	if err != nil {
		return
	}
	v6 := parseIPTablesInput(string(output))
	firewall.V6Known, firewall.V6Chains, firewall.V6Rules, firewall.V6Drops = true, v6.Chains, v6.Rules, v6.Drops
}

// readHostAuditJournal은 logind와 sshd를 따로 동시에 읽는다. 한 번에 읽으면 journalctl이 두 원천을 섞어 정렬해 바쁜 호스트에서 audit의 1초를 넘긴다.
func readHostAuditJournal(ctx context.Context, since time.Time) ([]auditJournalEntry, bool, error) {
	groups := [][]string{{"systemd-logind"}, auditSSHDIdentifiers}
	type part struct {
		entries []auditJournalEntry
		err     error
	}
	parts := make([]part, len(groups))
	var wait sync.WaitGroup
	for index, identifiers := range groups {
		wait.Add(1)
		go func() {
			defer wait.Done()
			args := []string{"-o", "json", "--no-pager", "--since", fmt.Sprintf("@%d", since.Unix()), "-n", fmt.Sprint(auditJournalMaxEntries),
				"--output-fields=SYSLOG_IDENTIFIER,MESSAGE_ID,MESSAGE,_PID,LEADER,SESSION_ID"}
			for _, identifier := range identifiers {
				args = append(args, "-t", identifier)
			}
			reader, _, err := runHostJournal(ctx, args...)
			if err != nil {
				parts[index].err = err
				return
			}
			parts[index].entries, parts[index].err = parseAuditJournal(reader)
		}()
	}
	wait.Wait()
	var entries []auditJournalEntry
	limited := false
	for _, part := range parts {
		if part.err != nil {
			return nil, false, part.err
		}
		limited = limited || len(part.entries) >= auditJournalMaxEntries
		entries = append(entries, part.entries...)
	}
	return entries, limited, nil
}

// readHostConnectedTCP는 이 network namespace의 ESTABLISHED TCP 소켓이다. IPv6를 끈 커널에는 tcp6 표가 없다.
func readHostConnectedTCP() ([]auditTCPSocket, error) {
	var sockets []auditTCPSocket
	for _, name := range []string{"tcp", "tcp6"} {
		file, err := os.Open("/proc/net/" + name)
		if errors.Is(err, fs.ErrNotExist) && name == "tcp6" {
			continue
		}
		if err != nil {
			return nil, err
		}
		sockets = append(sockets, auditConnectedTCP(parseLinuxTCPTable(file))...)
		file.Close()
	}
	return sockets, nil
}

// auditConnectedTCP는 ESTABLISHED 행만 남긴다. inode가 0인 행은 소켓의 주인을 찾을 수 없어 버린다.
func auditConnectedTCP(rows []linuxTCPRow) []auditTCPSocket {
	var sockets []auditTCPSocket
	for _, row := range rows {
		if row.state != "ESTABLISHED" {
			continue
		}
		local, err := netip.ParseAddrPort(row.local)
		remote, err2 := netip.ParseAddrPort(row.remote)
		inode, err3 := strconv.ParseUint(row.inode, 10, 64)
		if err != nil || err2 != nil || err3 != nil || inode == 0 {
			continue
		}
		sockets = append(sockets, auditTCPSocket{Local: local, Remote: remote, Inode: inode})
	}
	return sockets
}
