//go:build linux

package edc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
		kernel:   runningKernelRelease,
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
			return firewall, nil
		}
	}
	if !found {
		return auditFirewall{}, errAuditNoFirewallTool
	}
	return auditFirewall{Tool: "nftables"}, nil
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
