package edc

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestCheckServicesFailsOnFailedUnitsAndWarnsOnChangingOnes(t *testing.T) {
	facts := checkFacts{Services: []svcListItem{
		{Unit: "nginx.service", LoadState: "loaded", ActiveState: "failed", SubState: "failed"},
		{Unit: "app.service", LoadState: "loaded", ActiveState: "activating", SubState: "auto-restart"},
		{Unit: "sshd.service", LoadState: "loaded", ActiveState: "active", SubState: "running"},
	}}
	result := checkServices(facts)
	if result.Status != StatusFail || !strings.Contains(result.Summary, "nginx.service") || !strings.Contains(result.Summary, "app.service") {
		t.Fatalf("result = %+v", result)
	}
	if result.Next != "edc svc nginx.service" || result.Error == nil {
		t.Fatalf("next = %q, error = %+v", result.Next, result.Error)
	}

	facts.Services = facts.Services[1:]
	if result := checkServices(facts); result.Status != StatusWarn || result.Next != "edc svc app.service" {
		t.Fatalf("changing unit = %+v", result)
	}
	facts.Services = facts.Services[1:]
	if result := checkServices(facts); result.Status != StatusPass || result.Next != "" {
		t.Fatalf("healthy units = %+v", result)
	}
}

func TestCheckServicesSkipsWithoutSystemd(t *testing.T) {
	if result := checkServices(checkFacts{ServicesErr: errSvcNoSystemd}); result.Status != StatusSkip {
		t.Fatalf("no systemd = %+v", result)
	}
	if result := checkServices(checkFacts{ServicesErr: errors.New("exit status 1")}); result.Status != StatusSkip {
		t.Fatalf("systemctl error = %+v", result)
	}
}

func TestCheckProcessesGroupsZombiesByParent(t *testing.T) {
	facts := checkFacts{Processes: []checkProcess{
		{PID: 1, PPID: 0, Command: "systemd", State: "S"},
		{PID: 812, PPID: 1, Command: "python3", State: "S"},
		{PID: 900, PPID: 812, Command: "worker", State: "Z"},
		{PID: 901, PPID: 812, Command: "worker", State: "Z"},
		{PID: 950, PPID: 1, Command: "cron", State: "Z"},
	}}
	result := checkProcesses(facts)
	if result.Status != StatusWarn || result.Next != "edc top" {
		t.Fatalf("result = %+v", result)
	}
	// 가장 많이 남긴 부모가 먼저 온다.
	if first, second := strings.Index(result.Summary, "python3(812) ×2"), strings.Index(result.Summary, "systemd(1) ×1"); first < 0 || second < first {
		t.Fatalf("summary = %q", result.Summary)
	}

	facts.Processes = append(facts.Processes[:2], checkProcess{PID: 77, PPID: 1, Command: "dd", State: "D"})
	if result := checkProcesses(facts); result.Status != StatusWarn || !strings.Contains(result.Summary, "dd(77)") {
		t.Fatalf("blocked process = %+v", result)
	}
	if result := checkProcesses(checkFacts{Processes: facts.Processes[:2]}); result.Status != StatusPass {
		t.Fatalf("healthy processes = %+v", result)
	}
}

func TestCheckMemoryUsesAvailableShare(t *testing.T) {
	for _, test := range []struct {
		available uint64
		want      Status
	}{{11, StatusPass}, {10, StatusPass}, {9, StatusWarn}, {5, StatusWarn}, {4, StatusFail}} {
		facts := checkFacts{Memory: infoMemory{Total: 100 << 20, Available: test.available << 20}}
		if result := checkMemory(facts); result.Status != test.want {
			t.Errorf("available %d%% = %s, want %s", test.available, result.Status, test.want)
		}
	}
	if result := checkMemory(checkFacts{MemoryErr: fs.ErrPermission}); result.Status != StatusSkip {
		t.Fatalf("unreadable meminfo = %+v", result)
	}
}

func TestCheckDisksFollowsTheInfoLimits(t *testing.T) {
	for _, test := range []struct {
		percent float64
		want    Status
	}{{89, StatusPass}, {90, StatusWarn}, {95, StatusFail}} {
		facts := checkFacts{Disks: []checkDisk{{Mount: "/var", FSType: "ext4", Percent: test.percent}}}
		if result := checkDisks(facts); result.Status != test.want {
			t.Errorf("usage %.0f%% = %s, want %s", test.percent, result.Status, test.want)
		}
	}
	facts := checkFacts{Disks: []checkDisk{
		{Mount: "/", FSType: "ext4", Percent: 40, InodesUsed: 96, InodesTotal: 100},
		{Mount: "/var", FSType: "ext4", Percent: 96},
	}}
	if result := checkDisks(facts); result.Status != StatusFail || result.Next != "edc du /var" || !strings.Contains(result.Summary, "/ inodes 96%") {
		t.Fatalf("inode and usage = %+v", result)
	}
}

func TestCheckDisksWarnsOnReadOnlyMountsExceptImages(t *testing.T) {
	facts := checkFacts{Disks: []checkDisk{{Mount: "/media/cd", FSType: "iso9660", Percent: 100, ReadOnly: true}}}
	if result := checkDisks(facts); result.Status != StatusPass {
		t.Fatalf("image file system = %+v", result)
	}
	facts.Disks = []checkDisk{{Mount: "/data", FSType: "xfs", Percent: 10, ReadOnly: true}}
	if result := checkDisks(facts); result.Status != StatusWarn || !strings.Contains(result.Summary, "/data") {
		t.Fatalf("read-only data mount = %+v", result)
	}
}

func TestCheckKernelUsesTheEventsLevels(t *testing.T) {
	at := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	facts := checkFacts{Kernel: eventsFacts{KernelSource: "journal", Signals: []eventsSignal{
		{Kind: eventsSegfault, Time: at, Brief: "app segfault"},
		{Kind: eventsLink, Time: at, Brief: "eth0 down"},
	}}}
	result := checkKernel(facts)
	if result.Status != StatusWarn || strings.Contains(result.Summary, eventsLink) || result.Next != "edc events --since 1h" {
		t.Fatalf("segfault only = %+v", result)
	}
	facts.Kernel.Signals = append(facts.Kernel.Signals, eventsSignal{Kind: eventsOOM, Time: at, Brief: "killed java"})
	if result := checkKernel(facts); result.Status != StatusFail || !strings.Contains(result.Summary, "oom ×1") {
		t.Fatalf("oom = %+v", result)
	}
	facts.Kernel.Signals = []eventsSignal{{Kind: eventsOops, Time: at, Brief: "WARNING"}}
	if result := checkKernel(facts); result.Status != StatusWarn {
		t.Fatalf("kernel warning = %+v", result)
	}
	facts.Kernel.Signals[0].Failed = true
	if result := checkKernel(facts); result.Status != StatusFail {
		t.Fatalf("oops = %+v", result)
	}
	if result := checkKernel(checkFacts{Kernel: eventsFacts{KernelErr: fs.ErrPermission}}); result.Status != StatusSkip {
		t.Fatalf("unreadable kernel log = %+v", result)
	}
}

func TestCheckTimeWarnsWhenUnsynced(t *testing.T) {
	if result := checkTime(checkFacts{Clock: &infoClock{Synced: false}}); result.Status != StatusWarn {
		t.Fatalf("unsynced = %+v", result)
	}
	if result := checkTime(checkFacts{Clock: &infoClock{Synced: true}}); result.Status != StatusPass {
		t.Fatalf("synced = %+v", result)
	}
	if result := checkTime(checkFacts{}); result.Status != StatusSkip {
		t.Fatalf("unknown clock = %+v", result)
	}
}

func TestCheckNetworkDoesNotFailOnAnUnreadableRouteTable(t *testing.T) {
	dns := infoDNS{Config: resolvConfig{Nameservers: []string{"10.0.0.2"}}}
	if result := checkNetworkResult(checkFacts{Network: checkNetwork{RouteErr: fs.ErrPermission, DNS: dns}}); result.Status != StatusPass {
		t.Fatalf("unreadable route = %+v", result)
	}
	if result := checkNetworkResult(checkFacts{Network: checkNetwork{DNS: dns}}); result.Status != StatusFail || result.Next != "ip route" {
		t.Fatalf("no route = %+v", result)
	}
	route := "10.0.0.1 dev eth0"
	if result := checkNetworkResult(checkFacts{Network: checkNetwork{Route: route, DNS: infoDNS{Err: fs.ErrNotExist}}}); result.Status != StatusWarn {
		t.Fatalf("no resolv.conf = %+v", result)
	}
	if result := checkNetworkResult(checkFacts{Network: checkNetwork{Route: route}}); result.Status != StatusWarn {
		t.Fatalf("no nameserver = %+v", result)
	}
	for _, test := range []struct {
		percent float64
		want    Status
	}{{79, StatusPass}, {80, StatusWarn}, {90, StatusFail}} {
		network := checkNetwork{Route: route, DNS: dns, Conntrack: test.percent, ConntrackOK: true}
		if result := checkNetworkResult(checkFacts{Network: network}); result.Status != test.want {
			t.Errorf("conntrack %.0f%% = %s, want %s", test.percent, result.Status, test.want)
		}
		network = checkNetwork{Route: route, DNS: dns, TCPMemory: test.percent, TCPMemoryOK: true}
		if result := checkNetworkResult(checkFacts{Network: network}); result.Status != test.want {
			t.Errorf("tcp memory %.0f%% = %s, want %s", test.percent, result.Status, test.want)
		}
	}
}

func TestCheckContainers(t *testing.T) {
	if result := checkContainers(checkFacts{ContainersErr: errCtrNoDocker}); result.Status != StatusSkip {
		t.Fatalf("no docker = %+v", result)
	}
	denied := errors.New("dial unix /var/run/docker.sock: connect: permission denied")
	if result := checkContainers(checkFacts{ContainersErr: denied}); result.Status != StatusSkip {
		t.Fatalf("no docker access = %+v", result)
	}
	refused := errors.New("dial unix /var/run/docker.sock: connect: connection refused")
	if result := checkContainers(checkFacts{ContainersErr: refused}); result.Status != StatusWarn || result.Next != "edc svc docker" {
		t.Fatalf("docker down = %+v", result)
	}
	facts := checkFacts{Containers: []ctrListItem{
		{Name: "web", Status: "running", Health: "unhealthy", State: "running (unhealthy)"},
		{Name: "job", Status: "exited", ExitCode: 1, State: "exited (1)"},
	}}
	if result := checkContainers(facts); result.Status != StatusFail || result.Next != "edc ctr job" || !strings.Contains(result.Summary, "web") {
		t.Fatalf("failed container = %+v", result)
	}
	facts.Containers = facts.Containers[:1]
	if result := checkContainers(facts); result.Status != StatusWarn || result.Next != "edc ctr web" {
		t.Fatalf("unhealthy container = %+v", result)
	}
	if result := checkContainers(checkFacts{}); result.Status != StatusPass {
		t.Fatalf("no containers = %+v", result)
	}
}

func TestCheckFileHandles(t *testing.T) {
	for _, test := range []struct {
		used uint64
		want Status
	}{{79, StatusPass}, {80, StatusWarn}, {90, StatusFail}} {
		if result := checkFileHandles(checkFacts{FileHandles: &infoFileHandles{Used: test.used, Max: 100}}); result.Status != test.want {
			t.Errorf("file handles %d%% = %s, want %s", test.used, result.Status, test.want)
		}
	}
	unlimited := &infoFileHandles{Used: 3332, Max: 1<<63 - 1}
	if result := checkFileHandles(checkFacts{FileHandles: unlimited}); result.Status != StatusPass || strings.Contains(result.Summary, "9223372036854775807") {
		t.Fatalf("unlimited file-max = %+v", result)
	}
	if result := checkFileHandles(checkFacts{}); result.Status != StatusSkip {
		t.Fatalf("unparsed file-nr = %+v", result)
	}
}

func TestCheckReboot(t *testing.T) {
	if result := checkRebootResult(checkFacts{Reboot: checkReboot{Flag: true, Packages: []string{"linux-image-6.8.0-50"}}}); result.Status != StatusWarn || !strings.Contains(result.Summary, "linux-image") {
		t.Fatalf("reboot flag = %+v", result)
	}
	newer := checkReboot{Running: "6.8.0-45-generic", Kernels: []string{"6.8.0-45-generic", "6.8.0-50-generic"}}
	if result := checkRebootResult(checkFacts{Reboot: newer}); result.Status != StatusWarn {
		t.Fatalf("newer kernel = %+v", result)
	}
	newer.Kernels = newer.Kernels[:1]
	if result := checkRebootResult(checkFacts{Reboot: newer}); result.Status != StatusPass {
		t.Fatalf("newest kernel = %+v", result)
	}
	if result := checkRebootResult(checkFacts{Reboot: checkReboot{Running: "custom"}}); result.Status != StatusPass {
		t.Fatalf("custom kernel = %+v", result)
	}
}

func TestCollectCheckFactsReadsEverySourceInOrder(t *testing.T) {
	sources := checkSources{
		services:  func(context.Context) ([]svcListItem, error) { return nil, errSvcNoSystemd },
		processes: func() ([]checkProcess, error) { return []checkProcess{{PID: 1, Command: "init", State: "S"}}, nil },
		memory:    func() (infoMemory, error) { return infoMemory{Total: 100, Available: 50}, nil },
		disks:     func() ([]checkDisk, error) { return nil, nil },
		kernel: func(_ context.Context, since time.Duration) eventsFacts {
			if since != checkKernelWindow {
				t.Errorf("kernel window = %s", since)
			}
			return eventsFacts{KernelSource: "journal"}
		},
		clock:       func() *infoClock { return &infoClock{Synced: true} },
		network:     func() checkNetwork { return checkNetwork{Route: "10.0.0.1 dev eth0"} },
		containers:  func(context.Context) ([]ctrListItem, error) { return nil, errCtrNoDocker },
		fileHandles: func() (*infoFileHandles, error) { return &infoFileHandles{Used: 1, Max: 100}, nil },
		reboot:      func() checkReboot { return checkReboot{} },
	}
	results := evaluateCheck(collectCheckFacts(context.Background(), sources))
	want := []string{"service", "process", "memory", "disk", "kernel", "time", "network", "container", "filehandles", "reboot"}
	if len(results) != len(want) {
		t.Fatalf("results = %d", len(results))
	}
	for index, id := range want {
		if results[index].Probe != "check."+id {
			t.Errorf("result %d = %s, want check.%s", index, results[index].Probe, id)
		}
	}
	if code := exitCode(results); code != 0 {
		t.Fatalf("exit code = %d; a warning (no nameserver) must not fail the run", code)
	}
}
