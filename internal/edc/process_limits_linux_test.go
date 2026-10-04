//go:build linux

package edc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLimitFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func limitFixture(t *testing.T) (string, string, []topProcess) {
	t.Helper()
	root := t.TempDir()
	proc, mount := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	writeLimitFixture(t, filepath.Join(proc, "self/mountinfo"), "1 0 0:1 / "+mount+" rw - cgroup2 none rw\n")
	for _, pid := range []string{"10", "11"} {
		writeLimitFixture(t, filepath.Join(proc, pid, "cgroup"), "0::/workers\n")
		writeLimitFixture(t, filepath.Join(proc, pid, "limits"), "Limit Soft Limit Hard Limit Units\nMax open files 1024 4096 files\n")
		if err := os.MkdirAll(filepath.Join(proc, pid, "fd"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	group := filepath.Join(mount, "workers")
	writeLimitFixture(t, filepath.Join(group, "memory.current"), "128\n")
	writeLimitFixture(t, filepath.Join(group, "memory.max"), "512\n")
	writeLimitFixture(t, filepath.Join(group, "memory.events.local"), "low 0\nhigh 0\nmax 4\noom 2\noom_kill 1\n")
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "usage_usec 1000\nnr_periods 10\nnr_throttled 2\nthrottled_usec 500\n")
	return proc, group, []topProcess{{PID: 10}, {PID: 11}}
}

func TestLinuxFDLimitParsesSoftLimitAndUnlimited(t *testing.T) {
	for _, test := range []struct {
		row       string
		want      uint64
		unlimited bool
		valid     bool
	}{
		{"Max open files 1024 4096 files", 1024, false, true},
		{"Max open files 0 4096 files", 0, false, true},
		{"Max open files unlimited unlimited files", 0, true, true},
		{"Max open files nope 4096 files", 0, false, false},
		{"Max open files -1 4096 files", 0, false, false},
		{"Max open files 1024", 0, false, false},
		{"", 0, false, false},
	} {
		t.Run(test.row, func(t *testing.T) {
			value, unlimited, err := parseLinuxFDSoftLimit(test.row)
			if (err == nil) != test.valid || unlimited != test.unlimited {
				t.Fatalf("value=%v unlimited=%v err=%v", value, unlimited, err)
			}
			if test.valid && !test.unlimited && (value == nil || *value != test.want) {
				t.Fatalf("soft=%v, want %d", value, test.want)
			}
		})
	}
}

func TestLinuxCgroupMountResolutionUsesMountRootAndEscapes(t *testing.T) {
	mounts := parseLinuxCgroupMounts("1 0 0:1 / /sys/fs/cgroup rw - cgroup2 none rw\n2 0 0:2 /workers /visible\\040groups rw - cgroup2 none rw\n")
	if got, err := resolveLinuxCgroup("/workers/api", mounts); err != nil || got != "/visible groups/api" {
		t.Fatalf("path=%q err=%v", got, err)
	}
	if got, err := resolveLinuxCgroup("/", mounts); err != nil || got != "/sys/fs/cgroup" {
		t.Fatalf("root=%q err=%v", got, err)
	}
	if _, err := resolveLinuxCgroup("/workers-other", mounts[1:]); err == nil {
		t.Fatal("a prefix without a path boundary must not match")
	}
	for _, path := range []string{"../outside", "/../outside", "/workers/../../outside", "/workers (deleted)", ""} {
		if _, err := linuxUnifiedCgroupPath("0::" + path); err == nil {
			t.Fatalf("accepted invalid path %q", path)
		}
	}
	if _, err := linuxUnifiedCgroupPath("1:memory:/workers\n"); !errors.Is(err, errLinuxCgroupV1) {
		t.Fatalf("v1 err=%v", err)
	}
}

// calico 같은 agent는 같은 cgroup2 계층을 /run/calico/cgroup에도 mount한다. mountinfo에서 그쪽이 먼저 나와도
// 경로와 오류 문구는 사용자가 아는 /sys/fs/cgroup을 써야 한다.
func TestLinuxCgroupMountResolutionPrefersSysFsCgroupForTheSameRoot(t *testing.T) {
	mounts := parseLinuxCgroupMounts("12 0 0:1 / /run/calico/cgroup rw - cgroup2 none rw\n15 0 0:1 / /sys/fs/cgroup rw - cgroup2 none rw\n")
	if got, err := resolveLinuxCgroup("/system.slice/a.service", mounts); err != nil || got != "/sys/fs/cgroup/system.slice/a.service" {
		t.Fatalf("path=%q err=%v", got, err)
	}
	if got, err := resolveLinuxCgroup("/", mounts); err != nil || got != "/sys/fs/cgroup" {
		t.Fatalf("root=%q err=%v", got, err)
	}
	// /sys/fs/cgroup이 없으면 먼저 나온 mount를 그대로 쓴다.
	if got, err := resolveLinuxCgroup("/a", mounts[:1]); err != nil || got != "/run/calico/cgroup/a" {
		t.Fatalf("only calico=%q err=%v", got, err)
	}
}

func TestLinuxProcessLimitsShareGroupAndReportIntervalCounters(t *testing.T) {
	proc, group, processes := limitFixture(t)
	reader := newLinuxProcessLimits(proc)
	now := time.Unix(100, 0)
	reader.enrich(processes, now)
	first := processes[0].Limits
	if first.FD.Status != "available" || first.FD.Used == nil || *first.FD.Used != 0 || *first.FD.Soft != 1024 {
		t.Fatalf("FD=%+v", first.FD)
	}
	if first.Cgroup.CPU.Status != "baseline" || first.Cgroup.CPU.Periods != nil {
		t.Fatalf("first cpu=%+v", first.Cgroup.CPU)
	}
	if *first.Cgroup.Memory.Used != 128 || *first.Cgroup.Memory.Max != 512 || *first.Cgroup.Events.OOM != 2 || *first.Cgroup.Events.OOMKill != 1 {
		t.Fatalf("group=%+v", first.Cgroup)
	}
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "nr_periods 20\nnr_throttled 5\nthrottled_usec 4500\n")
	reader.enrich(processes, now.Add(2*time.Second))
	for _, process := range processes {
		cpu := process.Limits.Cgroup.CPU
		if cpu.Status != "available" || *cpu.Periods != 10 || *cpu.ThrottledPeriods != 3 || *cpu.ThrottledUS != 4000 || *cpu.WindowS != 2 {
			t.Fatalf("cpu=%+v", cpu)
		}
	}
	if len(reader.previous) != 1 {
		t.Fatalf("group baselines=%d", len(reader.previous))
	}
	lines := strings.Join(topProcessLimitLines(processes), "\n")
	if strings.Count(lines, "group scope") != 1 || strings.Count(lines, "pid ") != 2 {
		t.Fatalf("duplicate group or missing process: %s", lines)
	}
}

func TestLinuxProcessLimitsResetAfterCounterResetGapOrGroupReplacement(t *testing.T) {
	proc, group, processes := limitFixture(t)
	reader := newLinuxProcessLimits(proc)
	now := time.Unix(100, 0)
	reader.enrich(processes, now)
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "nr_periods 1\nnr_throttled 0\nthrottled_usec 0\n")
	reader.enrich(processes, now.Add(time.Second))
	if processes[0].Limits.Cgroup.CPU.Status != "baseline" {
		t.Fatal("counter reset must establish a new baseline")
	}
	if err := os.Remove(filepath.Join(group, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	reader.enrich(processes, now.Add(2*time.Second))
	if processes[0].Limits.Cgroup.CPU.Status != "unsupported" || len(reader.previous) != 0 {
		t.Fatal("a failed read must discard the baseline")
	}
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "nr_periods 100\nnr_throttled 50\nthrottled_usec 10000\n")
	reader.enrich(processes, now.Add(3*time.Second))
	if processes[0].Limits.Cgroup.CPU.Status != "baseline" {
		t.Fatal("recovery must not report old lifetime counters as a delta")
	}
	oldKey := processes[0].Limits.Cgroup.Key
	if err := os.Rename(group, group+"-old"); err != nil {
		t.Fatal(err)
	}
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "nr_periods 200\nnr_throttled 100\nthrottled_usec 20000\n")
	reader.enrich(processes, now.Add(4*time.Second))
	if processes[0].Limits.Cgroup.Key == oldKey || processes[0].Limits.Cgroup.CPU.Status != "baseline" {
		t.Fatal("a replacement cgroup must not reuse its predecessor's baseline")
	}
	reader.enrich(nil, now.Add(5*time.Second))
	if len(reader.previous) != 0 {
		t.Fatal("unobserved groups must not retain baselines")
	}
}

func TestLinuxProcessLimitsExposePartialFailuresAndUnlimitedMemory(t *testing.T) {
	proc, group, processes := limitFixture(t)
	reader := newLinuxProcessLimits(proc)
	writeLimitFixture(t, filepath.Join(group, "memory.max"), "max\n")
	writeLimitFixture(t, filepath.Join(group, "cpu.stat"), "usage_usec 100\n")
	writeLimitFixture(t, filepath.Join(proc, "10/limits"), "Max open files unlimited unlimited files\n")
	reader.enrich(processes, time.Now())
	limits := processes[0].Limits
	if !limits.FD.Unlimited || limits.FD.Soft != nil || !limits.Cgroup.Memory.Unlimited || limits.Cgroup.Memory.Max != nil {
		t.Fatalf("limits=%+v", limits)
	}
	if limits.Cgroup.CPU.Status != "unsupported" || limits.Cgroup.Memory.Status != "available" {
		t.Fatalf("controller states=%+v", limits.Cgroup)
	}
	writeLimitFixture(t, filepath.Join(group, "memory.current"), "not-a-number\n")
	writeLimitFixture(t, filepath.Join(group, "memory.events.local"), "oom bad\noom_kill 0\n")
	reader.enrich(processes, time.Now())
	if processes[0].Limits.Cgroup.Memory.Status != "error" || processes[0].Limits.Cgroup.Events.Status != "error" {
		t.Fatal("invalid counters must report an error")
	}
	if got := linuxLimitError(os.ErrPermission, true); got.Status != "permission_denied" {
		t.Fatalf("permission state=%+v", got)
	}
	if got := linuxLimitError(os.ErrNotExist, false); got.Status != "error" {
		t.Fatalf("vanished process state=%+v", got)
	}
	if err := os.Remove(filepath.Join(proc, "10/limits")); err != nil {
		t.Fatal(err)
	}
	reader.enrich(processes, time.Now())
	if fd := processes[0].Limits.FD; fd.Status != "error" || fd.Used == nil || fd.Soft != nil {
		t.Fatalf("partial FD=%+v", fd)
	}
}

func TestLinuxProcessLimitsReportV1AndInaccessibleMounts(t *testing.T) {
	proc, _, processes := limitFixture(t)
	reader := newLinuxProcessLimits(proc)
	writeLimitFixture(t, filepath.Join(proc, "10/cgroup"), "1:memory:/workers\n")
	writeLimitFixture(t, filepath.Join(proc, "11/cgroup"), "0::/../../outside\n")
	reader.enrich(processes, time.Now())
	if processes[0].Limits.Cgroup.Status != "unsupported" || processes[1].Limits.Cgroup.Status != "unavailable" {
		t.Fatalf("membership states=%+v %+v", processes[0].Limits.Cgroup, processes[1].Limits.Cgroup)
	}
	if err := os.Remove(filepath.Join(proc, "self/mountinfo")); err != nil {
		t.Fatal(err)
	}
	reader.enrich(processes, time.Now())
	if processes[0].Limits.Cgroup.Status != "error" {
		t.Fatal("a mount read failure must remain visible")
	}
}

func TestLinuxProcessLimitsReadCurrentProcess(t *testing.T) {
	reader := newLinuxProcessLimits("/proc")
	processes := []topProcess{{PID: os.Getpid()}}
	reader.enrich(processes, time.Now())
	fd := processes[0].Limits.FD
	if fd.Status != "available" || fd.Used == nil || fd.Soft == nil && !fd.Unlimited {
		t.Fatalf("current process FD=%+v", fd)
	}
	group := processes[0].Limits.Cgroup
	if group.Status == "" || group.Memory.Status == "" || group.Events.Status == "" || group.CPU.Status == "" {
		t.Fatalf("current process group has no state: %+v", group)
	}
}
