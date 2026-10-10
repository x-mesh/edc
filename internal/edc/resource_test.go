package edc

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCalculateRate(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, CPUUser: 10, CPUSystem: 5, CPUIOWait: 2, CPUTotal: 100, NetInBytes: 1000, NetOutBytes: 2000, PacketsIn: 10, PacketsOut: 20, DiskRead: 100, DiskWrite: 200}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), CPUUser: 30, CPUSystem: 15, CPUIOWait: 4, CPUTotal: 200, NetInBytes: 3000, NetOutBytes: 6000, PacketsIn: 30, PacketsOut: 60, DiskRead: 2100, DiskWrite: 4200, MemoryUsed: 25, MemoryTotal: 100, Load1: 1.5}
	rate := calculateRate(previous, current)
	if rate.NetIn != 1000 || rate.NetOut != 2000 || rate.PacketsIn != 10 || rate.DiskRead != 1000 {
		t.Fatalf("unexpected rate: %#v", rate)
	}
	if rate.CPUUser != 20 || rate.CPUSystem != 10 || rate.CPUIOWait != 2 || rate.MemoryPercent != 25 {
		t.Fatalf("unexpected percent: %#v", rate)
	}
}

func TestCalculateRateSkipsCountersAfterMissingRead(t *testing.T) {
	start := time.Unix(0, 0)
	// 앞 sample은 network를 읽지 못해 counter가 0이고 disk만 읽었다.
	previous := resourceSnapshot{TakenAt: start, NetMissing: true, DiskRead: 1000, DiskWrite: 1000}
	current := resourceSnapshot{TakenAt: start.Add(time.Second), NetInBytes: 5 << 40, NetOutBytes: 4 << 40, PacketsIn: 1 << 30, PacketsOut: 1 << 30, DiskRead: 3000, DiskWrite: 5000}
	rate := calculateRate(previous, current)
	if rate.NetIn != 0 || rate.NetOut != 0 || rate.PacketsIn != 0 || rate.PacketsOut != 0 || rate.NetHealthValid {
		t.Fatalf("network rate after a missing read = %#v", rate)
	}
	if rate.DiskRead != 2000 || rate.DiskWrite != 4000 {
		t.Fatalf("disk rate must not depend on the network read: %#v", rate)
	}
	previous.DiskMissing, previous.DiskRead, previous.DiskWrite = true, 0, 0
	if rate := calculateRate(previous, current); rate.DiskRead != 0 || rate.DiskWrite != 0 || rate.DiskHealthValid {
		t.Fatalf("disk rate after a missing read = %#v", rate)
	}
}

func TestCalculateRateIncludesHealthCounters(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, DiskOps: 10, DiskWaitMS: 100, DiskBusyMS: 500, NetErrors: 4, NetDrops: 2, DiskHealthValid: true, DiskBusyValid: true, NetHealthValid: true}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), DiskOps: 30, DiskWaitMS: 500, DiskBusyMS: 1300, NetErrors: 10, NetDrops: 6, DiskHealthValid: true, DiskBusyValid: true, NetHealthValid: true}
	rate := calculateRate(previous, current)
	if rate.DiskIOPS != 10 || rate.DiskAwait != 20 || rate.DiskBusy != 40 || rate.NetErrors != 3 || rate.NetDrops != 2 {
		t.Fatalf("health rate = %#v", rate)
	}
	if !rate.DiskHealthValid || !rate.DiskBusyValid || !rate.NetHealthValid {
		t.Fatalf("health validity = %#v", rate)
	}
	// macOS는 busy 시간을 주지 않으므로 IOPS와 await만 계산한다.
	previous.DiskBusyValid, current.DiskBusyValid = false, false
	if rate := calculateRate(previous, current); rate.DiskIOPS != 10 || rate.DiskAwait != 20 || rate.DiskBusy != 0 || rate.DiskBusyValid || !rate.DiskHealthValid {
		t.Fatalf("rate without busy time = %#v", rate)
	}
}

func TestCalculateRateIncludesCoreAndPressure(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, Cores: []resourceCPU{{Total: 100, Idle: 40}, {Total: 100, Idle: 90}}}
	current := resourceSnapshot{TakenAt: start.Add(time.Second), Cores: []resourceCPU{{Total: 200, Idle: 60}, {Total: 200, Idle: 150}}, PSICPU: 2.5, PSIMemory: 4.5, PSIIO: 12.5, PSIValid: true}
	rate := calculateRate(previous, current)
	if len(rate.CoreCPU) != 2 || rate.CoreCPU[0] != 80 || rate.CoreCPU[1] != 40 {
		t.Fatalf("core rate = %#v", rate.CoreCPU)
	}
	if !rate.PSIValid || rate.PSIIO != 12.5 {
		t.Fatalf("pressure rate = %#v", rate)
	}
}

func TestParsePressureAvg10(t *testing.T) {
	input := "some avg10=1.25 avg60=0.20 avg300=0.10 total=123\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	if value, ok := parsePressureAvg10(input, "some"); !ok || value != 1.25 {
		t.Fatalf("pressure = %v, %v", value, ok)
	}
	if value, ok := parsePressureAvg10("some avg10=1.25\nfull avg10=0.75 avg60=0.10", "full"); !ok || value != 0.75 {
		t.Fatalf("full pressure = %v, %v", value, ok)
	}
	if _, ok := parsePressureAvg10("full avg10=1.0", "some"); ok {
		t.Fatal("missing some row must not be valid")
	}
	if _, ok := parsePressureAvg10("some avg10=1.0", "full"); ok {
		t.Fatal("missing full row must not be valid")
	}
}

func TestParseTopProcesses(t *testing.T) {
	processes := parseTopProcesses(" 9 12.5 2048 Ss+ Sat Oct  3 05:06:51 2026 node server.js\n 2 99.0 1024 U< Mon Jan 12 23:00:01 2026 java -jar app.jar\n 5 1.0 10 R garbled start comm\n")
	if len(processes) != 2 || processes[0].PID != 2 || processes[0].RSS != 1024*1024 || processes[1].Command != "node server.js" {
		t.Fatalf("processes = %#v", processes)
	}
	// macOS의 U는 Linux의 D처럼 I/O를 기다리며 멈춘 상태라 blocked 후보와 묶음 집계에 들어가야 한다.
	if processes[0].State != topProcessStateBlocked || processes[1].State != "S" {
		t.Fatalf("states = %q, %q", processes[0].State, processes[1].State)
	}
	if want := time.Date(2026, time.October, 3, 5, 6, 51, 0, time.Local); !processes[1].Started.Equal(want) {
		t.Fatalf("started = %v, want %v", processes[1].Started, want)
	}
}

func TestParseLinuxProcessStat(t *testing.T) {
	data := "1234 (my (odd) proc) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 100 1000000 2048 18446744073709551615\n"
	stat, ok := parseLinuxProcessStat(1234, data)
	if !ok || stat.Command != "my (odd) proc" || stat.Ticks != 300 || stat.RSSPages != 2048 || stat.Threads != 1 || stat.StartTicks != 100 {
		t.Fatalf("stat = %#v, %v", stat, ok)
	}
	if stat.State != "S" {
		t.Fatalf("state = %q, want S", stat.State)
	}
	if blocked, ok := parseLinuxProcessStat(9, "9 (gm) D 1 9 9 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 100 1000000 2048 18446744073709551615\n"); !ok || blocked.State != "D" {
		t.Fatalf("blocked stat = %#v, %v", blocked, ok)
	}
	if _, ok := parseLinuxProcessStat(1, "1 (short) S 1 2"); ok {
		t.Fatal("truncated stat must not be valid")
	}
}

func TestLinuxProcessCommandLine(t *testing.T) {
	if got := linuxProcessCommandLine([]byte("bun\x00/tmp/bunx/node_modules/.bin/output-mesh\x00")); got != "bun /tmp/bunx/node_modules/.bin/output-mesh" {
		t.Fatalf("command line = %q", got)
	}
	// 커널 thread는 cmdline이 비어 있다. 부른 쪽이 comm을 그대로 쓰도록 빈 문자열이어야 한다.
	if got := linuxProcessCommandLine([]byte("\x00")); got != "" {
		t.Fatalf("kernel thread command line = %q", got)
	}
}

func TestTopProcessFilterMatchesTheFullCommandLine(t *testing.T) {
	filter, err := parseTopProcessFilter("output-mesh")
	if err != nil {
		t.Fatal(err)
	}
	if !filter.match(topProcess{PID: 46800, Command: "bun /tmp/bunx/node_modules/.bin/output-mesh"}) {
		t.Fatal("the term must match a command line that holds it")
	}
	if filter.match(topProcess{PID: 46800, Command: "bun"}) {
		t.Fatal("the term must not match the executable name alone")
	}
}

func TestTopProcessTrackerUsesRecentTicks(t *testing.T) {
	boot := time.Unix(1_000_000, 0)
	tracker := &topProcessTracker{clockTicks: 100, pageSize: 4096, boot: boot}
	start := time.Unix(0, 0)
	if _, ok := tracker.update(start, []linuxProcessStat{{PID: 1, Command: "old", Ticks: 1_000_000}, {PID: 2, Command: "idle", Ticks: 50}}); ok {
		t.Fatal("the first read must only set a baseline")
	}
	// 오래 산 process가 방금 2초 동안 core 1.5개를 썼다. 수명 평균이었다면 거의 0이다.
	processes, ok := tracker.update(start.Add(2*time.Second), []linuxProcessStat{{PID: 1, Command: "old", Ticks: 1_000_300, RSSPages: 10, Threads: 4, StartTicks: 250}, {PID: 2, Command: "idle", Ticks: 50}, {PID: 3, Command: "new", Ticks: 999}})
	if !ok || len(processes) != 2 || processes[0].PID != 1 || processes[0].CPU != 150 || processes[0].RSS != 40960 || processes[1].CPU != 0 {
		t.Fatalf("processes = %#v", processes)
	}
	// starttime 250 tick은 부팅 2.5초 뒤다.
	if want := boot.Add(2500 * time.Millisecond); processes[0].Threads != 4 || !processes[0].Started.Equal(want) {
		t.Fatalf("threads %d, started %v, want %v", processes[0].Threads, processes[0].Started, want)
	}
}

func TestTopProcessSamplerDropsStaleListOnFailure(t *testing.T) {
	succeed := true
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) {
		if succeed {
			return []topProcess{{PID: 1, CPU: 90, Command: "node"}}, true
		}
		return nil, false
	}}
	sampler.refresh()
	if processes, valid := sampler.latest(); !valid || len(processes) != 1 {
		t.Fatalf("first refresh = %#v, %v", processes, valid)
	}
	succeed = false
	sampler.refresh()
	if processes, valid := sampler.latest(); valid || len(processes) != 0 {
		t.Fatalf("failed refresh kept a stale list: %#v, %v", processes, valid)
	}
	if sampler.running {
		t.Fatal("a failed refresh must still wait for the refresh interval")
	}
}

func TestFormatHelpers(t *testing.T) {
	if got := formatBytes(1024 * 1024 * 1024); got != "1.00 GB" {
		t.Fatalf("formatBytes = %s", got)
	}
	if got := formatDuration(49*time.Hour + 3*time.Minute); got != "2 days, 1 hours, 3 minutes" {
		t.Fatalf("formatDuration = %s", got)
	}
}

func TestVisibleDisks(t *testing.T) {
	disks := []diskDetails{{Mount: "/", Total: 100}, {Mount: "/System/Volumes/VM", Total: 100}, {Mount: t.TempDir(), Total: 100}, {Mount: "/etc/hosts", Total: 100}}
	visible := visibleDisks(disks)
	if len(visible) != 2 || visible[1].Mount == "/etc/hosts" {
		t.Fatalf("visible disks = %#v", visible)
	}
}

func TestPrintTopRow(t *testing.T) {
	var output strings.Builder
	printTopRow(&output, time.Date(2026, 1, 1, 11, 36, 44, 0, time.UTC), resourceRate{NetIn: 0.04 * 1024 * 1024, CPUUser: 1, MemoryPercent: 11.8}, newTopLimits(8, false))
	row := strings.TrimRight(output.String(), "\n")
	for _, expected := range []string{"11:36:44", "0.04M", " 11.8"} {
		if !strings.Contains(row, expected) {
			t.Fatalf("row %q does not contain %q", row, expected)
		}
	}
	if width := len([]rune(row)); width != topTableWidth {
		t.Fatalf("row width = %d, want %d", width, topTableWidth)
	}
}

func TestTopTableFitsTargetWidth(t *testing.T) {
	var output strings.Builder
	printTopHeader(&output, hostDetails{Hostname: "host", Model: "model", Cores: 8, MemoryTotal: 16 * 1024 * 1024 * 1024})
	for _, line := range strings.Split(strings.TrimRight(output.String(), "\n"), "\n") {
		if width := topDisplayWidth(line); width != topTableWidth {
			t.Fatalf("line %q width = %d, want %d", line, width, topTableWidth)
		}
	}
}

func TestTopLoadThresholdFollowsCores(t *testing.T) {
	limits := newTopLimits(16, false)
	if limits.load.warn != 11.2 || limits.load.danger != 16 {
		t.Fatalf("load threshold = %#v", limits.load)
	}
	if single := newTopLimits(0, false); single.load.danger != 1 {
		t.Fatalf("unknown core count must fall back to one core: %#v", single.load)
	}
}

func TestTopRowColorsByRiskLevel(t *testing.T) {
	limits := newTopLimits(10, true)
	tests := []struct {
		name  string
		rate  resourceRate
		color string
	}{
		{"normal load", resourceRate{Load1: 3}, ""},
		{"warn load", resourceRate{Load1: 7.5}, topColorWarn},
		{"danger load", resourceRate{Load1: 12}, topColorDanger},
		{"danger cpu", resourceRate{CPUUser: 95}, topColorDanger},
		{"warn iowait", resourceRate{CPUIOWait: 12}, topColorWarn},
		{"danger memory", resourceRate{MemoryPercent: 99.4}, topColorDanger},
	}
	// color가 비면 정상값이라 escape가 하나도 없어야 한다. 색은 경고와 위험 둘뿐이다.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			printTopRow(&output, time.Now(), test.rate, limits)
			if test.color == "" {
				if strings.Contains(output.String(), "\033[") {
					t.Fatalf("normal row must have no escape: %q", output.String())
				}
				return
			}
			if !strings.Contains(output.String(), test.color) {
				t.Fatalf("row %q does not use color %q", output.String(), test.color)
			}
		})
	}
}

func TestTopRowKeepsWidthWithColor(t *testing.T) {
	var output strings.Builder
	printTopRow(&output, time.Now(), resourceRate{Load1: 12, MemoryPercent: 99.4}, newTopLimits(4, true))
	plain := regexp.MustCompile(`\033\[[0-9;]*m`).ReplaceAllString(strings.TrimRight(output.String(), "\n"), "")
	if width := len([]rune(plain)); width != topTableWidth {
		t.Fatalf("row width without color codes = %d, want %d", width, topTableWidth)
	}
}

func TestFormatRateStaysShort(t *testing.T) {
	const mib = 1024 * 1024
	tests := map[float64]string{0.04 * mib: "0.04M", 12.3 * mib: "12.3M", 512 * mib: "512M", 2048 * mib: "2.00G", 300 * 1024 * mib: "300G"}
	for input, want := range tests {
		got := formatRate(input)
		if got != want {
			t.Fatalf("formatRate(%v) = %q, want %q", input, got, want)
		}
		if len(got) > 5 {
			t.Fatalf("formatRate(%v) = %q is wider than 5 columns", input, got)
		}
	}
}

func TestParseLinuxSwapOutPages(t *testing.T) {
	if pages, ok := parseLinuxSwapOutPages("pswpin 12\npswpout 345\npgfault 9\n"); !ok || pages != 345 {
		t.Fatalf("pswpout = %d, %v", pages, ok)
	}
	if _, ok := parseLinuxSwapOutPages("pgfault 9\n"); ok {
		t.Fatal("vmstat without pswpout must report a missing read")
	}
}

func TestCalculateRateSwapOut(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, SwapOutBytes: 1 << 30}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), SwapOutBytes: 1<<30 + 4096*10}
	if rate := calculateRate(previous, current); rate.SwapOut != 4096*5 {
		t.Fatalf("swap out = %v", rate.SwapOut)
	}
	previous = resourceSnapshot{TakenAt: start, SwapMissing: true}
	if rate := calculateRate(previous, current); rate.SwapOut != 0 {
		t.Fatalf("swap out after a missing read = %v", rate.SwapOut)
	}
}

func TestCalculateRateStealBlockedAndQueue(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, CPUTotal: 1000, CPUSteal: 100, CPUStealValid: true, DiskQueueMS: 1000, DiskHealthValid: true, DiskBusyValid: true}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), CPUTotal: 1200, CPUSteal: 130, CPUStealValid: true, ProcsBlocked: 4, ProcsBlockedSource: topBlockedKernelTasks, DiskQueueMS: 4000, DiskHealthValid: true, DiskBusyValid: true}
	rate := calculateRate(previous, current)
	if !rate.CPUStealValid || rate.CPUSteal != 15 {
		t.Fatalf("steal = %v %v", rate.CPUSteal, rate.CPUStealValid)
	}
	if rate.ProcsBlockedSource != topBlockedKernelTasks || rate.ProcsBlocked != 4 {
		t.Fatalf("blocked = %v from %v", rate.ProcsBlocked, rate.ProcsBlockedSource)
	}
	// 2초 동안 가중 I/O 시간이 3000ms 늘었으면 평균 1.5개가 진행 중이었다.
	if rate.DiskQueue != 1.5 {
		t.Fatalf("queue = %v", rate.DiskQueue)
	}
	previous.CPUStealValid, current.DiskMissing = false, true
	rate = calculateRate(previous, current)
	if rate.CPUStealValid || rate.CPUSteal != 0 || rate.DiskQueue != 0 {
		t.Fatalf("missing reads: steal %v %v, queue %v", rate.CPUSteal, rate.CPUStealValid, rate.DiskQueue)
	}
}

func TestTopSampleJSON(t *testing.T) {
	at := time.Date(2026, 1, 1, 11, 36, 44, 0, time.FixedZone("KST", 9*3600))
	sample := newTopSample(hostDetails{Hostname: "host", Cores: 8}, at, resourceRate{NetIn: 1234.567, NetDrops: 2.2, NetHealthValid: true, DiskAwait: 15.555, DiskHealthValid: true, PSIIO: 3.3, PSIMemoryFull: 1.25, PSIIOFull: 0.5, PSIValid: true, CPUUser: 12.3456, MemoryPercent: 11.8, Load1: 0.5, SwapOut: 20480.456})
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{`"time":"2026-01-01T02:36:44Z"`, `"hostname":"host"`, `"cores":8`, `"net_in_bytes_per_s":1234.57`, `"network_drops_per_s":2.2`, `"network_health_supported":true`, `"disk_await_ms":15.56`, `"disk_health_supported":true`, `"psi_io_some_avg10_pct":3.3`, `"psi_memory_full_avg10_pct":1.25`, `"psi_io_full_avg10_pct":0.5`, `"psi_supported":true`, `"cpu_user_pct":12.35`, `"memory_pct":11.8`, `"load1":0.5`, `"swap_out_bytes_per_s":20480.46`, `"disk_busy_supported":false`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("sample %s does not contain %s", text, expected)
		}
	}
	if strings.Contains(text, "\n") {
		t.Fatalf("sample must stay on one line: %q", text)
	}
}

func TestFormatUsageBar(t *testing.T) {
	tests := map[float64]string{0: strings.Repeat(diskBarEmpty, diskBarWidth), 50: strings.Repeat(diskBarFull, 10) + strings.Repeat(diskBarEmpty, 10), 100: strings.Repeat(diskBarFull, diskBarWidth)}
	for percent, bar := range tests {
		got := formatUsageBar(percent, false)
		if !strings.HasPrefix(got, bar) {
			t.Fatalf("formatUsageBar(%v) = %q, want the bar %q", percent, got, bar)
		}
		if !strings.HasSuffix(got, "%") || len([]rune(got)) != diskBarWidth+8 {
			t.Fatalf("formatUsageBar(%v) = %q has an unexpected width", percent, got)
		}
	}
	// 범위를 벗어난 값도 막대를 넘지 않는다.
	for _, percent := range []float64{-5, 140} {
		if width := len([]rune(formatUsageBar(percent, false))); width != diskBarWidth+8 {
			t.Fatalf("formatUsageBar(%v) width = %d", percent, width)
		}
	}
}

func TestFormatUsageBarColorsByThreshold(t *testing.T) {
	tests := map[float64]string{92: topColorWarn, 97: topColorDanger}
	for percent, code := range tests {
		if got := formatUsageBar(percent, true); !strings.Contains(got, code) {
			t.Fatalf("formatUsageBar(%v) = %q does not use %q", percent, got, code)
		}
	}
	if got := formatUsageBar(40, true); strings.Contains(got, "\033[") {
		t.Fatalf("normal bar must have no escape: %q", got)
	}
	if strings.Contains(formatUsageBar(97, false), "\033[") {
		t.Fatal("색을 끄면 escape가 없어야 한다")
	}
}

// APFS는 container 하나를 여러 volume이 나눠 쓴다. `/`의 Used 열은 system volume이 쓴 양만 세므로
// 그 값을 그대로 쓰면 926GB 중 16GB만 쓴 것처럼 보인다. 남은 공간에서 계산해야 실제 사용률이 나온다.
func TestParseDiskUsageCountsSharedAPFSContainer(t *testing.T) {
	output := `Filesystem     1024-blocks      Used Available Capacity  Mounted on
/dev/disk3s1s1   971350180  16689044  42274080    29%    /
/dev/disk3s5     971350180 886182012  42274080    96%    /System/Volumes/Data
`
	disks, err := parseDiskUsage(strings.NewReader(output))
	if err != nil {
		t.Fatalf("parse returned %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("disks = %d", len(disks))
	}
	root := disks[0]
	if root.Mount != "/" || root.Device != "/dev/disk3s1s1" {
		t.Fatalf("root = %#v", root)
	}
	if root.Percent < 95.6 || root.Percent > 95.7 {
		t.Fatalf("percent = %.2f, want about 95.65", root.Percent)
	}
	if root.Used != (971350180-42274080)*1024 {
		t.Fatalf("used = %d", root.Used)
	}
	// 같은 container를 공유하므로 두 volume의 사용률이 같아야 한다.
	if disks[1].Percent != root.Percent {
		t.Fatalf("volumes of one container disagree: %.2f vs %.2f", disks[1].Percent, root.Percent)
	}
}

func TestParseDiskUsageSkipsUnusableRows(t *testing.T) {
	output := `Filesystem 1024-blocks Used Available Capacity Mounted on
devfs 200 200 0 100% /dev
map -hosts 0 0 0 100% /net
/dev/disk1 0 0 0 100% /empty
/dev/disk2 abc 10 20 50% /bad
/dev/disk3 100 10 200 10% /over
/dev/disk4 1000 250 750 25% /ok
`
	disks, err := parseDiskUsage(strings.NewReader(output))
	if err != nil {
		t.Fatalf("parse returned %v", err)
	}
	if len(disks) != 1 || disks[0].Mount != "/ok" {
		t.Fatalf("disks = %#v", disks)
	}
	if disks[0].Percent != 25 {
		t.Fatalf("percent = %.2f, want 25", disks[0].Percent)
	}
}

// 실제 Linux 호스트의 `df -kP`다. tmpfs와 overlay, snap image가 실제 디스크 두 대를 열두 줄로 늘렸다.
// loop device 자체는 숨기지 않으므로 직접 붙인 image는 남아야 한다.
func TestHiddenDiskKeepsOnlyStorageFileSystems(t *testing.T) {
	output := `Filesystem     1024-blocks       Used  Available Capacity Mounted on
tmpfs               6421252       1928    6419324       1% /run
/dev/sda3         104805396   76540120   23000000      77% /
tmpfs                  5120          0       5120       0% /run/lock
tmpfs              32106260    2077696   30028564       7% /tmp
/dev/sda5        2223999552 1127219200 1096780352      51% /app
tmpfs                   128          8        120       7% /run/credentials/getty@tty1.service
tmpfs               6421252       9216    6412036       1% /run/user/0
tmpfs                   128          8        120       7% /run/credentials/systemd-journald.service
overlay           104805396   76540120   23000000      77% /var/lib/docker/rootfs/overlayfs/dd6a96448ca90dc9dc72b383c1224a5d7dc10a49207be1a4012af3c79bf25cd0
/dev/loop0           130944     130944          0     100% /snap/core22/1963
/dev/loop1            12345      12345          0     100% /snap/snapd/21759
/dev/loop2           102400      50000      52400      49% /mnt/image
`
	disks, err := parseDiskUsage(strings.NewReader(output))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, disk := range disks {
		if !hiddenDisk(disk) {
			kept = append(kept, disk.Mount)
		}
	}
	if got := strings.Join(kept, " "); got != "/ /app /mnt/image" {
		t.Fatalf("kept = %q, want %q", got, "/ /app /mnt/image")
	}
}

// 고정 폭은 긴 mount 경로나 LVM 장치 이름에서 뒤 열을 밀어낸다. 열 폭을 값에서 재야 표가 선다.
func TestPrintDiskUsageKeepsColumnsAligned(t *testing.T) {
	disks := []diskDetails{
		{Mount: "/", Device: "/dev/sda3", Total: 100 << 30, Used: 73 << 30, Percent: 73},
		{Mount: t.TempDir(), Device: "/dev/mapper/ubuntu--vg-ubuntu--lv", Total: 2 << 40, Used: 1 << 40, Percent: 50},
	}
	var output strings.Builder
	printDiskUsage(&output, disks, false)
	columns, rows := map[int]bool{}, 0
	for _, line := range strings.Split(output.String(), "\n") {
		index := strings.Index(line, ": ")
		if index < 0 {
			continue
		}
		rows++
		columns[liveWidth(line[:index])] = true
	}
	if rows != 2 || len(columns) != 1 {
		t.Fatalf("disk columns are not aligned (rows %d, columns %v):\n%s", rows, columns, output.String())
	}
}

func TestParseTopProcessFilter(t *testing.T) {
	if filter, err := parseTopProcessFilter("  "); err != nil || filter.active() {
		t.Fatalf("blank filter = %+v, %v", filter, err)
	}
	for _, value := range []string{"node,", ",node", "node, ,java"} {
		if _, err := parseTopProcessFilter(value); !errors.Is(err, errTopProcessFilterEmpty) {
			t.Fatalf("%q error = %v", value, err)
		}
	}
	filter, err := parseTopProcessFilter("42, Output-Mesh")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		process topProcess
		want    bool
	}{
		{topProcess{PID: 42, Command: "unrelated"}, true},
		{topProcess{PID: 7, Command: "/opt/bin/OUTPUT-MESH --serve"}, true},
		{topProcess{PID: 420, Command: "unrelated"}, false},
		{topProcess{PID: 7, Command: "node"}, false},
	} {
		if got := filter.match(test.process); got != test.want {
			t.Fatalf("match(%+v) = %v, want %v", test.process, got, test.want)
		}
	}
	// 숫자 항목은 PID와만 비교한다. command에 숫자가 들어 있어도 맞지 않는다.
	if filter, _ := parseTopProcessFilter("42"); filter.match(topProcess{PID: 7, Command: "worker42"}) {
		t.Fatal("a numeric term must match the PID only")
	}
}

func TestTopProcessSamplerFiltersBeforeKeepingTheBusiestOnes(t *testing.T) {
	busy := make([]topProcess, 0, 10)
	for pid := 1; pid <= 10; pid++ {
		busy = append(busy, topProcess{PID: pid, CPU: float64(100 - pid), Command: "busy"})
	}
	quiet := topProcess{PID: 99, CPU: 0.1, Command: "quiet"}
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) {
		return append(append([]topProcess(nil), busy...), quiet), true
	}}
	sampler.refresh()
	if processes, _ := sampler.latest(); len(processes) != topProcessLimit {
		t.Fatalf("unfiltered list has %d processes, want %d", len(processes), topProcessLimit)
	}
	filter, err := parseTopProcessFilter("quiet,busy")
	if err != nil {
		t.Fatal(err)
	}
	sampler.setFilter(filter)
	sampler.refresh()
	if processes, valid := sampler.latest(); !valid || len(processes) != 11 {
		t.Fatalf("filtered list = %d processes, valid %v", len(processes), valid)
	}
	only, _ := parseTopProcessFilter("quiet")
	sampler.setFilter(only)
	sampler.refresh()
	if processes, _ := sampler.latest(); len(processes) != 1 || processes[0].PID != 99 {
		t.Fatalf("a low CPU match was dropped: %+v", processes)
	}
}

func TestTopProcessSamplerKeepsMemoryCandidatesOutsideCPUList(t *testing.T) {
	all := make([]topProcess, 0, 10)
	for pid := 1; pid <= 10; pid++ {
		all = append(all, topProcess{PID: pid, CPU: float64(11 - pid), RSS: uint64(pid) << 20, Command: "worker"})
	}
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return all, true }}
	sampler.refresh()
	processes, _, valid := sampler.latestWithTotal()
	if !valid || len(processes) != 2*topProcessLimit {
		t.Fatalf("candidate list = %+v, valid %v", processes, valid)
	}
	if processes[0].PID != 1 || processes[len(processes)-1].PID != 10 {
		t.Fatalf("CPU and memory leaders must both remain: %+v", processes)
	}
}

func TestTopProcessSamplerKeepsBlockedProcessesOutsideCPUAndMemoryLists(t *testing.T) {
	all := make([]topProcess, 0, 12)
	for pid := 1; pid <= 10; pid++ {
		all = append(all, topProcess{PID: pid, CPU: float64(50 - pid), RSS: uint64(100-pid) << 20, Command: "busy"})
	}
	all = append(all, topProcess{PID: 11, CPU: 1, RSS: 1 << 20, Command: "gm", State: "D"}, topProcess{PID: 12, CPU: 1, RSS: 1 << 20, Command: "gm", State: "S"})
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return all, true }}
	sampler.refresh()
	processes, _, _ := sampler.latestWithTotal()
	found := map[int]bool{}
	for _, process := range processes {
		found[process.PID] = true
	}
	if !found[11] || found[12] {
		t.Fatalf("only the D state process must be added: %+v", processes)
	}
}

func TestTopProcessSamplerReadsIOOnlyForCandidatesUnlessScanning(t *testing.T) {
	all := make([]topProcess, 0, 40)
	for pid := 1; pid <= 40; pid++ {
		rss := uint64(1) << 20
		if pid <= 20 {
			rss = uint64(pid) << 20
		}
		all = append(all, topProcess{PID: pid, CPU: float64(100 - pid), RSS: rss, Command: "worker"})
	}
	filled := 0
	fill := func(processes []topProcess) {
		filled = len(processes)
		for index := range processes {
			// 40번은 CPU와 메모리 순위가 가장 낮지만 I/O가 가장 크다.
			if processes[index].PID == 40 {
				processes[index].DiskValid, processes[index].DiskWrite = true, 50<<20
			}
		}
	}
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return append([]topProcess(nil), all...), true }, fillIO: fill}
	sampler.refresh()
	processes, _, _ := sampler.latestWithTotal()
	if filled != len(processes) || filled != 2*topProcessLimit {
		t.Fatalf("I/O read for %d processes, candidates %d", filled, len(processes))
	}
	for _, process := range processes {
		if process.PID == 40 {
			t.Fatalf("a low CPU and memory process must not be a candidate without a scan: %+v", processes)
		}
	}
	sampler.setScanIO(true)
	sampler.refresh()
	processes, _, _ = sampler.latestWithTotal()
	if filled != len(all) {
		t.Fatalf("a scan read I/O for %d processes, want %d", filled, len(all))
	}
	found := false
	for _, process := range processes {
		found = found || (process.PID == 40 && process.DiskValid)
	}
	if !found || len(processes) != 2*topProcessLimit+1 {
		t.Fatalf("the top I/O process must join the candidates: %+v", processes)
	}
}

func TestGroupTopProcessesSumsManySmallWriters(t *testing.T) {
	processes := []topProcess{{PID: 1, Command: "node", CPU: 30, DiskValid: true, DiskRead: 40 << 20}}
	for pid := 2; pid <= 201; pid++ {
		process := topProcess{PID: pid, Command: "gm", CPU: 0.5, RSS: 2 << 20, DiskValid: true, DiskRead: 1 << 20, DiskWrite: 2 << 20}
		if pid%4 != 0 {
			process.State = "D"
		}
		processes = append(processes, process)
	}
	// I/O를 읽지 못해도 대기 중인 process가 모이면 묶음으로 보인다.
	processes = append(processes, topProcess{PID: 300, Command: "sync", State: "D"}, topProcess{PID: 301, Command: "sync", State: "D"}, topProcess{PID: 302, Command: "shim", DiskValid: true, DiskWrite: 4 << 10}, topProcess{PID: 303, Command: "shim", DiskValid: true, DiskWrite: 4 << 10})
	groups := topProcessGroupsByIO(groupTopProcesses(processes))
	if len(groups) != topProcessGroupLimit {
		t.Fatalf("groups = %+v", groups)
	}
	gm := groups[0]
	if gm.Name != "gm" || gm.Count != 200 || gm.Blocked != 150 || gm.DiskRead != 200<<20 || gm.DiskWrite != 400<<20 || !gm.DiskValid {
		t.Fatalf("gm group = %+v", gm)
	}
	if groups[1].Name != "sync" || groups[1].Blocked != 2 || groups[1].DiskValid {
		t.Fatalf("a blocked group without I/O must follow: %+v", groups[1])
	}
}

func TestGroupTopProcessesKeepsCPUAndMemoryLeaders(t *testing.T) {
	processes := []topProcess{}
	for pid := 1; pid <= 200; pid++ {
		processes = append(processes, topProcess{PID: pid, Command: "gm", CPU: 0.5, RSS: 2 << 20})
	}
	for pid := 201; pid <= 210; pid++ {
		processes = append(processes, topProcess{PID: pid, Command: "php-fpm", CPU: 2, RSS: 80 << 20})
	}
	// 합이 기준에 못 미치는 묶음은 남지 않는다.
	processes = append(processes, topProcess{PID: 300, Command: "sshd", CPU: 1, RSS: 8 << 20}, topProcess{PID: 301, Command: "sshd", CPU: 1, RSS: 8 << 20})
	groups := groupTopProcesses(processes)
	byCPU, byRSS := topProcessGroupsByCPU(groups), topProcessGroupsByRSS(groups)
	if len(byCPU) != 2 || byCPU[0].Name != "gm" || byCPU[0].CPU != 100 || byCPU[1].Name != "php-fpm" {
		t.Fatalf("CPU groups = %+v", byCPU)
	}
	if len(byRSS) != 2 || byRSS[0].Name != "php-fpm" || byRSS[0].RSS != 800<<20 || byRSS[1].RSS != 400<<20 {
		t.Fatalf("RSS groups = %+v", byRSS)
	}
	for _, group := range groups {
		if group.Name == "sshd" {
			t.Fatalf("a small group must not be kept: %+v", groups)
		}
	}
}

func TestTopProcessGroupNameDropsArgumentsAndPath(t *testing.T) {
	if got := topProcessGroupName("/usr/bin/gm"); got != "gm" {
		t.Fatalf("path = %q", got)
	}
	// comm에는 인자가 없고 공백이 이름의 일부일 수 있어 자르지 않는다.
	if got := topProcessGroupName("Web Content"); got != "Web Content" {
		t.Fatalf("comm = %q", got)
	}
	topFullCommand = true
	defer func() { topFullCommand = false }()
	if got := topProcessGroupName("/usr/bin/gm convert -density 100x100 -[1]"); got != "gm" {
		t.Fatalf("full command = %q", got)
	}
}

func TestTopProcessSamplerClearsCachedMatchesWhenFilterChanges(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) {
		return []topProcess{{PID: 1, CPU: 90, Command: "other"}, {PID: 2, CPU: 10, Command: "worker"}}, true
	}}
	sampler.setFilter(filter)
	sampler.refresh()
	sampler.setFilter(filter)
	if processes, total, valid := sampler.latestWithTotal(); !valid || len(processes) != 1 || total.Count != 1 {
		t.Fatalf("same filter dropped valid matches: processes %+v, total %+v, valid %v", processes, total, valid)
	}
	sampler.setFilter(topProcessFilter{})
	processes, total, valid := sampler.latestWithTotal()
	if valid || len(processes) != 0 || total.Count != 0 {
		t.Fatalf("changed filter returned cached matches: processes %+v, total %+v, valid %v", processes, total, valid)
	}
}

func TestTopProcessSamplerDiscardsRefreshAfterFilterChanges(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%t", restore), func(t *testing.T) {
			sampler := &topProcessSampler{read: func() ([]topProcess, bool) {
				return []topProcess{{PID: 2, CPU: 10, Command: "worker"}}, true
			}}
			sampler.enrich = func([]topProcess) {
				sampler.setFilter(topProcessFilter{})
				if restore {
					sampler.setFilter(filter)
				}
			}
			sampler.setFilter(filter)
			sampler.refresh()
			sampler.mutex.Lock()
			defer sampler.mutex.Unlock()
			if sampler.valid || len(sampler.processes) != 0 || sampler.total.Count != 0 || sampler.running || !sampler.updated.IsZero() {
				t.Fatalf("refresh published old filter: processes %+v, total %+v, valid %v, running %v, updated %v", sampler.processes, sampler.total, sampler.valid, sampler.running, sampler.updated)
			}
		})
	}
}

func TestTopSampleCarriesProcessesOnlyWhenFiltering(t *testing.T) {
	sample := newTopSample(hostDetails{Hostname: "host"}, time.Unix(0, 0), resourceRate{})
	data, err := json.Marshal(sample)
	if err != nil || strings.Contains(string(data), "processes") {
		t.Fatalf("unfiltered sample = %s, %v", data, err)
	}
	sample.Processes = newTopProcessSamples(nil)
	if data, _ = json.Marshal(sample); !strings.Contains(string(data), `"processes":[]`) {
		t.Fatalf("empty match = %s", data)
	}
	sample.Processes = newTopProcessSamples([]topProcess{{PID: 7, CPU: 12.3456, RSS: 2048, Command: "node"}})
	if data, _ = json.Marshal(sample); !strings.Contains(string(data), `"processes":[{"pid":7,"command":"node","cpu_pct":12.35,"rss_bytes":2048,"limits":`) {
		t.Fatalf("process sample = %s", data)
	}
	started := time.Date(2026, time.October, 3, 5, 6, 51, 0, time.UTC)
	sample.Processes = newTopProcessSamples([]topProcess{{PID: 7, CPU: 1, RSS: 2, Command: "node", Started: started, Threads: 4, FDs: 9, DiskValid: true, DiskRead: 10.5, DiskWrite: 0}})
	sample.ProcessTotal = newTopProcessTotalSample(topProcessTotal{Count: 2, CPU: 3.456, RSS: 5, Threads: 8})
	data, _ = json.Marshal(sample)
	for _, want := range []string{`"started":"2026-10-03T05:06:51Z"`, `"threads":4`, `"fds":9`, `"disk_read_bytes_per_s":10.5`, `"disk_write_bytes_per_s":0`, `"process_total":{"count":2,"cpu_pct":3.46,"rss_bytes":5,"threads":8}`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("sample is missing %s: %s", want, data)
		}
	}
}

func TestParseLinuxProcessIO(t *testing.T) {
	data := "rchar: 100\nwchar: 200\nsyscr: 1\nsyscw: 2\nread_bytes: 4096\nwrite_bytes: 8192\ncancelled_write_bytes: 0\n"
	if io, ok := parseLinuxProcessIO(data); !ok || io.read != 4096 || io.write != 8192 {
		t.Fatalf("io = %+v, %v", io, ok)
	}
	if _, ok := parseLinuxProcessIO("rchar: 1\nread_bytes: 5\n"); ok {
		t.Fatal("a missing write_bytes must not be valid")
	}
}

func TestTopProcessSamplerTotalsEveryMatchAndEnrichesTheKeptOnes(t *testing.T) {
	all := make([]topProcess, 0, topFilteredProcessLimit+10)
	for pid := 1; pid <= topFilteredProcessLimit+10; pid++ {
		all = append(all, topProcess{PID: pid, CPU: 2, RSS: 1000, Threads: 3, Command: "worker"})
	}
	all = append(all, topProcess{PID: 999, CPU: 90, Command: "other"})
	enriched := 0
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return all, true }, enrich: func(processes []topProcess) { enriched = len(processes) }}
	sampler.refresh()
	if enriched != 0 {
		t.Fatal("enrich must not run without a filter")
	}
	filter, _ := parseTopProcessFilter("worker")
	sampler.setFilter(filter)
	sampler.refresh()
	processes, total, valid := sampler.latestWithTotal()
	if !valid || len(processes) != topFilteredProcessLimit || enriched != topFilteredProcessLimit {
		t.Fatalf("kept %d, enriched %d, valid %v", len(processes), enriched, valid)
	}
	if want := (topProcessTotal{Count: topFilteredProcessLimit + 10, CPU: 2 * float64(topFilteredProcessLimit+10), RSS: 1000 * uint64(topFilteredProcessLimit+10), Threads: 3 * (topFilteredProcessLimit + 10)}); !reflect.DeepEqual(total, want) {
		t.Fatalf("total = %+v, want %+v", total, want)
	}
}

// 멈춘 process 수는 host 값이라 필터와 상관없이 모든 process에서 센다.
func TestTopProcessSamplerCountsBlockedBeforeTheFilter(t *testing.T) {
	all := []topProcess{{PID: 1, State: topProcessStateBlocked, Command: "dd"}, {PID: 2, State: topProcessStateBlocked, Command: "cp"}, {PID: 3, State: "S", Command: "worker"}}
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return append([]topProcess(nil), all...), true }}
	filter, _ := parseTopProcessFilter("worker")
	sampler.setFilter(filter)
	_, total, _ := sampler.refreshNow()
	if total.Blocked != 2 || !total.BlockedValid {
		t.Fatalf("blocked = %d, %v", total.Blocked, total.BlockedValid)
	}
	snapshot := resourceSnapshot{ProcessTotal: total}
	fillProcsBlocked(&snapshot)
	if snapshot.ProcsBlocked != 2 || snapshot.ProcsBlockedSource != topBlockedProcessList {
		t.Fatalf("snapshot = %d from %v", snapshot.ProcsBlocked, snapshot.ProcsBlockedSource)
	}
	// kernel이 준 값(Linux procs_blocked)은 덮지 않는다.
	kernel := resourceSnapshot{ProcsBlocked: 9, ProcsBlockedSource: topBlockedKernelTasks, ProcessTotal: total}
	fillProcsBlocked(&kernel)
	if kernel.ProcsBlocked != 9 || kernel.ProcsBlockedSource != topBlockedKernelTasks {
		t.Fatalf("kernel value overwritten: %d from %v", kernel.ProcsBlocked, kernel.ProcsBlockedSource)
	}
	// state를 읽지 못한 목록은 0이 아니라 모르는 값이다.
	if _, known := countBlockedTopProcesses([]topProcess{{PID: 1}}); known {
		t.Fatal("a list without states must not count")
	}
}

func TestTopSampleWritesMemoryPressureOnlyWhenRead(t *testing.T) {
	data, err := json.Marshal(newTopSample(hostDetails{}, time.Unix(1, 0), resourceRate{MemoryPressure: topMemoryPressureWarn}))
	if err != nil || !strings.Contains(string(data), `"memory_pressure":"warn"`) || !strings.Contains(string(data), `"memory_pressure_supported":true`) {
		t.Fatalf("sample = %s, %v", data, err)
	}
	// psi_supported처럼 지원 여부는 늘 나오고, 값은 읽었을 때만 나온다.
	data, _ = json.Marshal(newTopSample(hostDetails{}, time.Unix(1, 0), resourceRate{}))
	if strings.Contains(string(data), `"memory_pressure":`) || !strings.Contains(string(data), `"memory_pressure_supported":false`) {
		t.Fatalf("an unread level must be left out: %s", data)
	}
}

// 압박 단계와 blocked의 출처는 누적값이 아니라 현재 sample의 값이다.
func TestCalculateRateCarriesBlockedOriginAndMemoryPressure(t *testing.T) {
	start := time.Unix(0, 0)
	previous := resourceSnapshot{TakenAt: start, CPUTotal: 100, MemoryPressure: topMemoryPressureNormal}
	current := resourceSnapshot{TakenAt: start.Add(time.Second), CPUTotal: 200, MemoryPressure: topMemoryPressureCritical, ProcsBlocked: 5, ProcsBlockedSource: topBlockedProcessList}
	rate := calculateRate(previous, current)
	if rate.MemoryPressure != topMemoryPressureCritical || rate.ProcsBlockedSource != topBlockedProcessList {
		t.Fatalf("rate = %+v", rate)
	}
	current.MemoryPressure, current.ProcsBlockedSource = topMemoryPressureUnknown, topBlockedKernelTasks
	if rate := calculateRate(previous, current); rate.MemoryPressure.known() || rate.ProcsBlockedSource != topBlockedKernelTasks {
		t.Fatalf("an unread level or a kernel count must not carry over: %+v", rate)
	}
}

func TestTopMemoryPressureLevelsAndNames(t *testing.T) {
	for pressure, want := range map[topMemoryPressure]string{topMemoryPressureUnknown: "unknown", topMemoryPressureNormal: "normal", topMemoryPressureWarn: "warn", 3: "warn", topMemoryPressureCritical: "critical", 8: "critical"} {
		if got := pressure.String(); got != want {
			t.Errorf("%d = %q, want %q", pressure, got, want)
		}
	}
	if topMemoryPressureUnknown.level() != topLevelNormal || topMemoryPressureCritical.score() != 1 || topMemoryPressure(8).score() != 1 {
		t.Fatal("an unknown level must not warn, and critical and above must score 1")
	}
}

// process 목록을 읽지 못하면 멈춘 수는 0이 아니라 모르는 값이다. 0으로 두면 blocked 열이 0을 보인다.
func TestTopProcessSamplerLeavesBlockedUnknownWhenTheReadFails(t *testing.T) {
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) {
		return []topProcess{{PID: 1, State: topProcessStateBlocked}}, false
	}}
	_, total, _ := sampler.refreshNow()
	if total.BlockedValid {
		t.Fatalf("total = %+v", total)
	}
	snapshot := resourceSnapshot{ProcessTotal: total}
	fillProcsBlocked(&snapshot)
	if snapshot.ProcsBlockedSource.known() {
		t.Fatal("an unknown count must not fill blocked")
	}
}
