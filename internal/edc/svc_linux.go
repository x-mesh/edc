//go:build linux

package edc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func hostSvcSources() (svcSources, bool) {
	return svcSources{
		euid:      os.Geteuid(),
		systemd:   linuxSystemdRunning,
		show:      linuxSvcShow,
		processes: linuxSvcProcesses,
		fdLimit:   func(pid int) topFDLimit { return readLinuxFDLimit(filepath.Join("/proc", strconv.Itoa(pid))) },
		listen:    func() ([]listenSocket, error) { return collectListenSockets(listenFamilies{TCP: true, UDP: true}) },
		journal:   linuxSvcJournal,
		monotonic: linuxMonotonicNow,
		list:      linuxSvcList,
		showUnits: linuxSvcShowUnits,
	}, true
}

func linuxSvcList(ctx context.Context) (string, error) {
	return commandOutput(ctx, "systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend", "--no-pager")
}

// linuxSvcShowUnits는 unit 이름을 systemctl이 낸 그대로 넘긴다. "--" 뒤에 두어 이름이 option으로 읽히지 않게 한다.
func linuxSvcShowUnits(ctx context.Context, units []string) (string, error) {
	args := append([]string{"show", "--no-pager", "-p", strings.Join(svcListProperties, ","), "--"}, units...)
	return commandOutput(ctx, "systemctl", args...)
}

// linuxSystemdRunning은 sd_booted()와 같은 기준을 쓴다. container와 Alpine에는 systemctl만 있고 systemd가 없을 수 있다.
func linuxSystemdRunning() bool {
	info, err := os.Stat("/run/systemd/system")
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = exec.LookPath("systemctl")
	return err == nil
}

func linuxSvcShow(ctx context.Context, unit string) (string, error) {
	return commandOutput(ctx, "systemctl", "show", "--no-pager", "-p", strings.Join(svcShowProperties, ","), unit)
}

// linuxSvcProcesses는 /proc/<pid>/cgroup이 unit의 cgroup을 가리키는 process를 고른다. cgroup 파일은 누구나 읽을 수 있어
// root가 아니어도 된다. CPU%는 같은 process의 stat을 두 번 읽어 구하고, 그 사이 끝난 process는 빠진다.
func linuxSvcProcesses(ctx context.Context, controlGroup string, mainPID int) ([]topProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cgroup"))
		// main process는 cgroup을 읽는 사이 경합이 나도 systemd가 알려 준 값이라 넣는다.
		if (err == nil && svcCgroupMember(string(data), controlGroup)) || pid == mainPID {
			members = append(members, pid)
		}
	}
	tracker := &topProcessTracker{clockTicks: linuxClockTicks, pageSize: uint64(os.Getpagesize()), boot: readLinuxBootTime()}
	tracker.update(time.Now(), readLinuxSvcStats(members))
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(svcCPUSampleGap):
	}
	processes, _ := tracker.update(time.Now(), readLinuxSvcStats(members))
	return processes, nil
}

func readLinuxSvcStats(pids []int) []linuxProcessStat {
	stats := make([]linuxProcessStat, 0, len(pids))
	for _, pid := range pids {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		if stat, ok := parseLinuxProcessStat(pid, string(data)); ok {
			stats = append(stats, stat)
		}
	}
	return stats
}

// linuxSvcJournal은 stdout과 stderr를 나눠 받는다. 권한 hint는 stderr에 나오고 기록 줄과 섞이면 안 된다.
func linuxSvcJournal(ctx context.Context, unit string) (string, string, error) {
	path, err := exec.LookPath("journalctl")
	if err != nil {
		return "", "", err
	}
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, path, "-u", unit, "-p", "err", "-n", strconv.Itoa(svcJournalLines), "--no-pager", "-o", "short-iso")
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	return stdout.String(), stderr.String(), err
}

// linuxMonotonicNow는 ActiveEnterTimestampMonotonic과 같은 CLOCK_MONOTONIC이다. 벽시계는 NTP가 바꿀 수 있어 쓰지 않는다.
func linuxMonotonicNow() (time.Duration, bool) {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		return 0, false
	}
	return time.Duration(now.Nano()), true
}
