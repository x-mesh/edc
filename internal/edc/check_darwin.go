//go:build darwin

package edc

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// checkDarwinCommandTimeout은 launchctl, ps, route 하나의 제한이다. 전체 --timeout과 따로 둔다.
const checkDarwinCommandTimeout = 5 * time.Second

// macOS에는 시계 동기화와 재부팅 필요 표시를 읽을 원천이 없어 clock과 reboot을 두지 않는다. 그 점검은 skip이다.
// kernel은 커널 log 대신 진단 보고서를 읽는다.
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
		kernel:         readDarwinDiagnosticReports,
		thermal:        readDarwinThermal,
		backup:         readDarwinBackup,
		kernelNext:     "ls -lt /Library/Logs/DiagnosticReports ~/Library/Logs/DiagnosticReports",
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

// checkDiagnosticHeaderBytes는 .ips의 첫 줄 header를 읽는 상한이다. 본문은 수 MB가 될 수 있다.
const checkDiagnosticHeaderBytes = 4 << 10

// readDarwinDiagnosticReports는 since 안에 쓰인 진단 보고서를 커널 신호로 옮긴다. 두 폴더를 모두 읽지 못하면 KernelErr다.
func readDarwinDiagnosticReports(_ context.Context, since time.Duration) eventsFacts {
	now := time.Now()
	facts := eventsFacts{Now: now, Start: now.Add(-since), Since: since, KernelSource: "DiagnosticReports"}
	directories := []string{"/Library/Logs/DiagnosticReports"}
	if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, filepath.Join(home, "Library/Logs/DiagnosticReports"))
	}
	var readErr error
	readable := 0
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			readErr = err
			continue
		}
		readable++
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || entry.IsDir() || info.ModTime().Before(facts.Start) {
				continue
			}
			kind, subject := classifyDiagnosticReport(entry.Name(), readDiagnosticHeader(filepath.Join(directory, entry.Name())))
			if kind == "" {
				continue
			}
			facts.Signals = append(facts.Signals, eventsSignal{Kind: kind, Time: info.ModTime(), Subject: subject, Brief: subject + " (" + entry.Name() + ")", Failed: kind == eventsOops})
		}
	}
	if readable == 0 {
		facts.KernelErr = readErr
	}
	sort.Slice(facts.Signals, func(i, j int) bool { return facts.Signals[i].Time.Before(facts.Signals[j].Time) })
	return facts
}

func readDiagnosticHeader(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	line, _ := bufio.NewReader(io.LimitReader(file, checkDiagnosticHeaderBytes)).ReadString('\n')
	return line
}

// readDarwinThermal은 OSThermalNotification.h의 kOSThermalNotificationPressureLevelName 값을 읽는다. 값은 0(nominal)부터
// 4(sleeping)까지다. 권한이 필요 없다.
func readDarwinThermal() (int, error) {
	output, _, err := runDarwinCheckTool(context.Background(), "/usr/bin/notifyutil", "-g", "com.apple.system.thermalpressurelevel")
	if err != nil {
		return 0, err
	}
	return parseThermalLevel(output)
}

func readDarwinBackup() checkBackup {
	destinations, _, err := runDarwinCheckTool(context.Background(), "/usr/bin/tmutil", "destinationinfo")
	if err != nil && !strings.Contains(destinations, "No destinations configured") {
		return checkBackup{Err: err}
	}
	if strings.Contains(destinations, "No destinations configured") {
		return checkBackup{}
	}
	latest, _, _ := runDarwinCheckTool(context.Background(), "/usr/bin/tmutil", "latestbackup")
	return parseBackup(destinations, latest)
}
