//go:build linux

package edc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func hostCheckSources() (checkSources, bool) {
	return checkSources{
		services: func(ctx context.Context) ([]svcListItem, error) {
			sources, _ := hostSvcSources()
			return collectSvcList(ctx, sources, false)
		},
		processes: readCheckProcesses,
		memory:    collectInfoMemory,
		disks:     readCheckDisks,
		kernel: func(ctx context.Context, since time.Duration) eventsFacts {
			sources, _ := hostEventsSources()
			// failed unit은 check.service가 지금 상태로 보고, 일반 오류 줄은 판정하지 않는다. 읽는 시간만 늘어난다.
			sources.failedUnits, sources.journalErrors = nil, nil
			return collectEventsFacts(ctx, sources, since)
		},
		clock:       linuxClock,
		network:     readCheckNetwork,
		containers:  readCheckContainers,
		fileHandles: readCheckFileHandles,
		reboot:      readCheckReboot,
	}, true
}

func readCheckProcesses() ([]checkProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []checkProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		stat, valid := parseLinuxProcessStat(pid, string(data))
		ppid, _, parents := parseLinuxProcessParents(string(data))
		if valid && parents {
			processes = append(processes, checkProcess{PID: pid, PPID: ppid, Command: stat.Command, State: stat.State})
		}
	}
	if len(processes) == 0 {
		return nil, errors.New("no readable process statistics")
	}
	return processes, nil
}

func readCheckDisks() ([]checkDisk, error) {
	disks, err := collectDisks()
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	if data, err := os.ReadFile("/proc/self/mounts"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 3 {
				types[unescapeMountField(fields[1])] = fields[2]
			}
		}
	}
	var result []checkDisk
	for _, disk := range visibleDisks(disks) {
		item := checkDisk{Mount: disk.Mount, FSType: types[disk.Mount], Percent: disk.Percent}
		if signal, ok := statfsDiskSignal(disk.Mount); ok {
			item.ReadOnly, item.InodesUsed, item.InodesTotal = signal.ReadOnly, signal.InodesUsed, signal.InodesTotal
		}
		result = append(result, item)
	}
	return result, nil
}

func readCheckNetwork() checkNetwork {
	network := checkNetwork{DNS: readInfoDNS("/")}
	// collectDefaultRoute는 파일을 읽지 못해도 빈 값을 준다. route가 없다는 판정과 섞이지 않게 먼저 읽어 본다.
	if _, err := os.ReadFile("/proc/net/route"); err != nil {
		network.RouteErr = err
	} else if iface, gateway := collectDefaultRoute(); iface != "" {
		network.Route = gateway + " dev " + iface
	}
	if network.Route == "" {
		if data, err := os.ReadFile("/proc/net/ipv6_route"); err == nil {
			if route := parseLinuxIPv6Default(string(data)); route != "" {
				network.Route, network.RouteErr = route, nil
			}
		}
	}
	health := collectNetworkHealth()
	network.Conntrack, network.ConntrackOK = networkConntrackUsage(health)
	network.TCPMemory, network.TCPMemoryOK = networkTCPMemoryUsage(health)
	return network
}

func readCheckContainers(ctx context.Context) ([]ctrListItem, error) {
	path, err := ctrSocketEndpoint()
	if err != nil {
		return nil, err
	}
	// docker가 없는 host가 흔하다. socket이 없으면 연결 오류가 아니라 docker가 없는 것이다.
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, errCtrNoDocker
	}
	sources, _ := hostCtrSources()
	if sources.close != nil {
		defer sources.close()
	}
	// 자원 표본은 container마다 stats를 한 번 더 읽는다. 상태 판정에는 쓰지 않는다.
	sources.listStats = nil
	return collectCtrList(ctx, sources, false)
}

func readCheckFileHandles() (*infoFileHandles, error) {
	data, err := os.ReadFile("/proc/sys/fs/file-nr")
	if err != nil {
		return nil, err
	}
	return parseLinuxFileNr(string(data)), nil
}

func readCheckReboot() checkReboot {
	var reboot checkReboot
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		reboot.Flag = true
		if data, err := os.ReadFile("/var/run/reboot-required.pkgs"); err == nil {
			reboot.Packages = uniqueFields(string(data))
		}
	}
	reboot.Running = runningKernelRelease()
	matches, _ := filepath.Glob("/boot/vmlinuz-*")
	for _, match := range matches {
		release := strings.TrimPrefix(filepath.Base(match), "vmlinuz-")
		if !strings.Contains(release, "rescue") {
			reboot.Kernels = append(reboot.Kernels, release)
		}
	}
	return reboot
}
