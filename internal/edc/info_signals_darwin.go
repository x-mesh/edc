//go:build darwin

package edc

import (
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// macOS에는 PSI와 OOM kill counter가 없고, 시계 동기화 상태는 외부 명령 없이 읽을 방법이 없어 비워 둔다.
func collectInfoSignals(verbose bool) infoSignals {
	signals := infoSignals{DNS: readInfoDNS("/")}
	signals.Platform, _ = syscall.Sysctl("hw.model")
	if present, err := syscall.SysctlUint32("kern.hv_vmm_present"); err == nil && present == 1 {
		signals.Virtual = "virtual machine"
	}
	if verbose {
		// macOS는 링크 속도를 외부 명령 없이 읽을 방법이 없어 비워 둔다.
		signals.Interfaces = collectInfoInterfaces(func(string) string { return "" })
		used, usedErr := syscall.SysctlUint32("kern.num_files")
		limit, limitErr := syscall.SysctlUint32("kern.maxfiles")
		if usedErr == nil && limitErr == nil {
			signals.FileHandles = &infoFileHandles{Used: uint64(used), Max: uint64(limit)}
		}
	}
	return signals
}

func collectDiskSignals(disks []diskDetails, verbose bool) map[string]diskSignal {
	signals := map[string]diskSignal{}
	for _, disk := range visibleDisks(disks) {
		signal, flags, ok := statfsDarwin(disk.Mount)
		if !ok {
			continue
		}
		if verbose {
			signal.Options = darwinMountOptions(flags)
		}
		signals[disk.Mount] = signal
	}
	return signals
}

// darwinMountOptions는 mount 명령이 적는 이름으로 statfs 플래그를 옮긴다. 진단에 쓰는 플래그만 고른다.
func darwinMountOptions(flags uint32) string {
	names := []string{"rw"}
	if flags&unix.MNT_RDONLY != 0 {
		names[0] = "ro"
	}
	for _, option := range []struct {
		flag uint32
		name string
	}{
		{unix.MNT_SNAPSHOT, "sealed"}, {unix.MNT_NOEXEC, "noexec"}, {unix.MNT_NOSUID, "nosuid"},
		{unix.MNT_NODEV, "nodev"}, {unix.MNT_LOCAL, "local"}, {unix.MNT_JOURNALED, "journaled"},
	} {
		if flags&option.flag != 0 {
			names = append(names, option.name)
		}
	}
	return strings.Join(names, ",")
}

func statfsDarwin(path string) (diskSignal, uint32, bool) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return diskSignal{}, 0, false
	}
	// 서명된 system volume은 snapshot에서 읽기 전용으로 붙는다. 정상이므로 경고하지 않는다.
	signal := diskSignal{ReadOnly: stat.Flags&unix.MNT_RDONLY != 0 && stat.Flags&unix.MNT_SNAPSHOT == 0}
	if stat.Files > 0 && stat.Ffree <= stat.Files {
		signal.InodesUsed, signal.InodesTotal = stat.Files-stat.Ffree, stat.Files
	}
	return signal, stat.Flags, true
}
