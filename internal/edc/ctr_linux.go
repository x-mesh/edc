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
)

func hostCtrSources() (ctrSources, bool) {
	return ctrSources{
		euid: os.Geteuid(),
		docker: func() bool {
			_, err := exec.LookPath("docker")
			return err == nil
		},
		inspect:    linuxCtrInspect,
		cgroupPath: linuxCtrCgroupPath,
		limits:     linuxCtrLimits,
		processes:  linuxSvcProcesses,
		fdLimit:    func(pid int) topFDLimit { return readLinuxFDLimit(filepath.Join("/proc", strconv.Itoa(pid))) },
		listen:     collectNetnsListenSockets,
		logs:       linuxCtrLogs,
		health: func(ctx context.Context, id string) (string, error) {
			return commandOutput(ctx, "docker", "inspect", "--type", "container", "--format", ctrHealthFormat, id)
		},
		disk: func(ctx context.Context, id string) (string, error) {
			return commandOutput(ctx, "docker", "inspect", "--type", "container", "--size", "--format", ctrDiskFormat, id)
		},
		fileSize: func(path string) (int64, error) {
			info, err := os.Stat(path)
			if err != nil {
				return 0, err
			}
			return info.Size(), nil
		},
		now: time.Now,
		listIDs: func(ctx context.Context) (string, error) {
			return commandOutput(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc")
		},
		inspectMany: linuxCtrInspectMany,
	}, true
}

// linuxCtrInspectMany는 ID를 docker ps가 낸 그대로 넘긴다. 16진수라 option으로 읽힐 일이 없다.
func linuxCtrInspectMany(ctx context.Context, ids []string) (string, error) {
	args := append([]string{"inspect", "--type", "container", "--format", ctrInspectFormat}, ids...)
	return commandOutput(ctx, "docker", args...)
}

// linuxCtrInspect는 stdout과 stderr를 합쳐 돌려준다. "No such container"는 stderr에 나온다.
func linuxCtrInspect(ctx context.Context, reference string) (string, error) {
	return commandOutput(ctx, "docker", "inspect", "--type", "container", "--format", ctrInspectFormat, reference)
}

func linuxCtrCgroupPath(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	return linuxUnifiedCgroupPath(string(data))
}

// linuxCtrLimits는 container cgroup의 memory, OOM, CPU throttle, pids를 한 번 읽는다. svc와 달리 두 번 재지 않으므로
// CPU는 container가 산 동안의 누적이다.
func linuxCtrLimits(path string) ctrLimits {
	limits := ctrLimits{topLimitStatus: topLimitStatus{Status: "available"}, Path: path}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		limits.topLimitStatus = linuxLimitError(err, false)
		return limits
	}
	directory, err := resolveLinuxCgroup(path, parseLinuxCgroupMounts(string(data)))
	if err != nil {
		limits.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: err.Error()}
		return limits
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		limits.topLimitStatus = topLimitStatus{Status: "unavailable", Reason: "cgroup directory " + directory + " is not readable"}
		return limits
	}
	limits.Memory = readLinuxCgroupMemory(directory)
	limits.Events = readLinuxCgroupEvents(directory)
	cpu, status := readLinuxCgroupCPU(directory)
	limits.CPU = ctrCPULimits{topLimitStatus: status, Periods: cpu.periods, Throttled: cpu.throttled, ThrottledUS: cpu.usec}
	limits.Pids = readLinuxCgroupPids(directory)
	return limits
}

func readLinuxCgroupPids(directory string) ctrPidsLimits {
	pids := ctrPidsLimits{topLimitStatus: topLimitStatus{Status: "available"}}
	for _, name := range []string{"pids.current", "pids.max"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			pids.topLimitStatus = linuxLimitError(err, true)
			return pids
		}
		text := strings.TrimSpace(string(data))
		if name == "pids.max" && text == "max" {
			pids.Unlimited = true
			continue
		}
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			pids.topLimitStatus = topLimitStatus{Status: "error", Reason: "invalid " + name}
			return pids
		}
		if name == "pids.current" {
			pids.Current = value
		} else {
			pids.Max = value
		}
	}
	return pids
}

// linuxCtrLogs는 container의 마지막 줄을 읽는다. stdout과 stderr를 합쳐야 애플리케이션이 어느 쪽에 쓰든 오류가 보인다.
func linuxCtrLogs(ctx context.Context, id string, lines int) (string, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	command := exec.CommandContext(ctx, path, "logs", "--tail", strconv.Itoa(lines), "--timestamps", id)
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	text := output.String()
	if len(text) > probeOutputLimit {
		text = text[len(text)-probeOutputLimit:]
	}
	return text, err
}
