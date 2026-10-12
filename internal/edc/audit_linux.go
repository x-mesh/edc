//go:build linux

package edc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func hostAuditSources() (auditSources, bool) {
	return auditSources{
		root:     "/",
		euid:     os.Geteuid(),
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
