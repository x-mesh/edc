//go:build darwin

package edc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// checkDarwinCommandTimeout은 launchctl, ps, route 하나의 제한이다. 전체 --timeout과 따로 둔다.
const checkDarwinCommandTimeout = 5 * time.Second

// macOS에는 커널 기록, 시계 동기화, 재부팅 필요 표시를 읽을 원천이 없어 kernel, clock, reboot을 두지 않는다. 그 점검은 skip이다.
func hostCheckSources() (checkSources, bool) {
	return checkSources{
		services:       readDarwinLaunchd,
		processes:      readDarwinCheckProcesses,
		memory:         collectInfoMemory,
		memoryPressure: readDarwinMemoryPressure,
		disks:          readDarwinCheckDisks,
		network:        readDarwinCheckNetwork,
		containers:     readCheckContainers,
		fileHandles:    readDarwinFileHandles,
		// 사용자로 실행하면 gui domain의 agent를, sudo로 실행하면 system domain의 daemon을 본다.
		serviceNext: "launchctl list %s",
		configSafe:  func() error { return activeConfig.commandSafety },
		runCommand:  runCheckCommand,
	}, true
}

func runDarwinCheckTool(ctx context.Context, name string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkDarwinCommandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, name, args...)
	// ps와 route의 출력 형식이 지역 설정을 따르지 않게 한다.
	command.Env = append(os.Environ(), "LC_ALL=C")
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func readDarwinLaunchd(ctx context.Context) ([]svcListItem, error) {
	output, _, err := runDarwinCheckTool(ctx, "/bin/launchctl", "list")
	if err != nil {
		return nil, err
	}
	return parseLaunchctlList(output), nil
}

func readDarwinCheckProcesses() ([]checkProcess, error) {
	output, _, err := runDarwinCheckTool(context.Background(), "/bin/ps", "-Ao", "pid=,ppid=,state=,comm=")
	if err != nil {
		return nil, err
	}
	return parseCheckPS(output), nil
}

func readDarwinCheckDisks() ([]checkDisk, error) {
	disks, err := collectDisks()
	if err != nil {
		return nil, err
	}
	var result []checkDisk
	for _, disk := range visibleDisks(disks) {
		item := checkDisk{Mount: disk.Mount, FSType: disk.FSType, Percent: disk.Percent}
		// statfsDarwin은 서명된 system volume의 read-only를 정상으로 본다.
		signal, _, ok := statfsDarwin(disk.Mount)
		// 그 밖의 read-only mount는 대개 설치용 DMG처럼 가득 찬 disk image다. 더 쓸 수 없으니 사용률도 판정하지 않는다.
		// macOS는 오류가 난 볼륨을 read-only로 다시 붙이는 일이 드물어 Linux와 달리 경고하지 않는다.
		if ok && signal.ReadOnly {
			continue
		}
		if ok {
			item.InodesUsed, item.InodesTotal = signal.InodesUsed, signal.InodesTotal
		}
		result = append(result, item)
	}
	return result, nil
}

func readDarwinCheckNetwork() checkNetwork {
	network := checkNetwork{DNS: readInfoDNS("/")}
	ctx := context.Background()
	stdout, stderr, err := runDarwinCheckTool(ctx, "/sbin/route", "-n", "get", "default")
	network.Route, network.RouteErr = parseCheckRouteGet(stdout, stderr, err)
	if network.Route == "" && network.RouteErr == nil {
		stdout, stderr, err = runDarwinCheckTool(ctx, "/sbin/route", "-n", "get", "-inet6", "default")
		network.Route, network.RouteErr = parseCheckRouteGet(stdout, stderr, err)
	}
	return network
}

func readDarwinFileHandles() (*infoFileHandles, error) {
	used, err := syscall.SysctlUint32("kern.num_files")
	if err != nil {
		return nil, err
	}
	limit, err := syscall.SysctlUint32("kern.maxfiles")
	if err != nil {
		return nil, err
	}
	return &infoFileHandles{Used: uint64(used), Max: uint64(limit)}, nil
}
