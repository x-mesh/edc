//go:build linux

package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
		storage:     readLinuxStorage,
		certs:       readCheckCerts,
		configSafe:  func() error { return activeConfig.commandSafety },
		runCommand:  runCheckCommand,
	}, true
}

func readCheckProcesses() ([]checkProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []checkProcess
	unreadable := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		// 목록을 읽은 뒤 끝난 process는 ENOENT나 ESRCH다. hidepid=1처럼 막힌 process만 세어 놓친 상태가 있음을 알린다.
		if errors.Is(err, fs.ErrPermission) {
			unreadable++
			continue
		}
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
	if unreadable > 0 {
		return processes, checkPartialError{Unreadable: unreadable}
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

func readCheckFileHandles() (*infoFileHandles, error) {
	data, err := os.ReadFile("/proc/sys/fs/file-nr")
	if err != nil {
		return nil, err
	}
	return parseLinuxFileNr(string(data)), nil
}

func readCheckReboot() checkReboot {
	var reboot checkReboot
	_, err := os.Stat("/var/run/reboot-required")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		reboot.Err = err
	}
	if err == nil {
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

// smartctlTimeout은 장치 하나를 읽는 제한이다. 잠든 HDD는 깨어나는 데 몇 초가 걸린다.
const smartctlTimeout = 10 * time.Second

func readLinuxStorage(ctx context.Context) checkStorage {
	var storage checkStorage
	// md 모듈이 없는 커널에는 파일이 없다. RAID가 없는 것이고 읽기 실패가 아니다.
	data, err := os.ReadFile("/proc/mdstat")
	switch {
	case err == nil:
		storage.Arrays = parseMdstat(string(data))
	case !errors.Is(err, fs.ErrNotExist):
		storage.ArraysErr = err
	}
	storage.Devices, storage.DevicesErr = readSmartctlDevices(ctx)
	return storage
}

// readSmartctlDevices는 smartctl이 찾은 장치마다 SMART 판정과 속성을 읽는다. 장치를 열려면 root가 필요하다.
func readSmartctlDevices(ctx context.Context) ([]checkStorageDevice, error) {
	path, err := exec.LookPath("smartctl")
	if err != nil {
		return nil, errCheckNoSmartctl
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("smartctl: %w", fs.ErrPermission)
	}
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, smartctlTimeout)
		defer cancel()
		output, err := exec.CommandContext(ctx, path, args...).Output()
		// smartctl의 exit status는 판정 bit를 담으므로 출력이 있으면 오류로 보지 않는다.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(output) > 0 {
			return output, nil
		}
		return output, err
	}
	output, err := run("--scan", "--json")
	if err != nil {
		return nil, fmt.Errorf("smartctl --scan: %w", err)
	}
	var scan smartctlScan
	if err := json.Unmarshal(output, &scan); err != nil {
		return nil, fmt.Errorf("smartctl --scan: %w", err)
	}
	var devices []checkStorageDevice
	var failures []string
	for _, entry := range scan.Devices {
		output, err := run("--json", "-H", "-A", "-d", entry.Type, entry.Name)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", entry.Name, err))
			continue
		}
		name := entry.Name
		// megaraid,N나 cciss,N처럼 한 경로 뒤에 여러 디스크가 있으면 종류까지 붙여야 줄이 구분된다.
		if strings.Contains(entry.Type, ",") {
			name += " [" + entry.Type + "]"
		}
		device, err := parseSmartctlReport(name, output)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		device.Next = "smartctl -a -d " + entry.Type + " " + entry.Name
		devices = append(devices, device)
	}
	if len(failures) > 0 {
		return devices, errors.New(strings.Join(failures, "; "))
	}
	return devices, nil
}
