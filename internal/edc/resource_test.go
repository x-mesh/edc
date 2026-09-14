package edc

import (
	"encoding/json"
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
	previous := resourceSnapshot{TakenAt: start, DiskOps: 10, DiskWaitMS: 100, DiskBusyMS: 500, NetErrors: 4, NetDrops: 2, DiskHealthValid: true, NetHealthValid: true}
	current := resourceSnapshot{TakenAt: start.Add(2 * time.Second), DiskOps: 30, DiskWaitMS: 500, DiskBusyMS: 1300, NetErrors: 10, NetDrops: 6, DiskHealthValid: true, NetHealthValid: true}
	rate := calculateRate(previous, current)
	if rate.DiskIOPS != 10 || rate.DiskAwait != 20 || rate.DiskBusy != 40 || rate.NetErrors != 3 || rate.NetDrops != 2 {
		t.Fatalf("health rate = %#v", rate)
	}
	if !rate.DiskHealthValid || !rate.NetHealthValid {
		t.Fatalf("health validity = %#v", rate)
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
	if value, ok := parsePressureAvg10(input); !ok || value != 1.25 {
		t.Fatalf("pressure = %v, %v", value, ok)
	}
	if _, ok := parsePressureAvg10("full avg10=1.0"); ok {
		t.Fatal("missing some row must not be valid")
	}
}

func TestParseTopProcesses(t *testing.T) {
	processes := parseTopProcesses(" 9 12.5 2048 node server.js\n 2 99.0 1024 java -jar app.jar\n")
	if len(processes) != 2 || processes[0].PID != 2 || processes[0].RSS != 1024*1024 || processes[1].Command != "node server.js" {
		t.Fatalf("processes = %#v", processes)
	}
}

func TestParseLinuxProcessStat(t *testing.T) {
	data := "1234 (my (odd) proc) S 1 1234 1234 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 1 0 100 1000000 2048 18446744073709551615\n"
	stat, ok := parseLinuxProcessStat(1234, data)
	if !ok || stat.Command != "my (odd) proc" || stat.Ticks != 300 || stat.RSSPages != 2048 {
		t.Fatalf("stat = %#v, %v", stat, ok)
	}
	if _, ok := parseLinuxProcessStat(1, "1 (short) S 1 2"); ok {
		t.Fatal("truncated stat must not be valid")
	}
}

func TestTopProcessTrackerUsesRecentTicks(t *testing.T) {
	tracker := &topProcessTracker{clockTicks: 100, pageSize: 4096}
	start := time.Unix(0, 0)
	if _, ok := tracker.update(start, []linuxProcessStat{{PID: 1, Command: "old", Ticks: 1_000_000}, {PID: 2, Command: "idle", Ticks: 50}}); ok {
		t.Fatal("the first read must only set a baseline")
	}
	// 오래 산 process가 방금 2초 동안 core 1.5개를 썼다. 수명 평균이었다면 거의 0이다.
	processes, ok := tracker.update(start.Add(2*time.Second), []linuxProcessStat{{PID: 1, Command: "old", Ticks: 1_000_300, RSSPages: 10}, {PID: 2, Command: "idle", Ticks: 50}, {PID: 3, Command: "new", Ticks: 999}})
	if !ok || len(processes) != 2 || processes[0].PID != 1 || processes[0].CPU != 150 || processes[0].RSS != 40960 || processes[1].CPU != 0 {
		t.Fatalf("processes = %#v", processes)
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
		{"normal load", resourceRate{Load1: 3}, topColorNormal},
		{"warn load", resourceRate{Load1: 7.5}, topColorWarn},
		{"danger load", resourceRate{Load1: 12}, topColorDanger},
		{"danger cpu", resourceRate{CPUUser: 95}, topColorDanger},
		{"warn iowait", resourceRate{CPUIOWait: 12}, topColorWarn},
		{"danger memory", resourceRate{MemoryPercent: 99.4}, topColorDanger},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			printTopRow(&output, time.Now(), test.rate, limits)
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

func TestTopSampleJSON(t *testing.T) {
	at := time.Date(2026, 1, 1, 11, 36, 44, 0, time.FixedZone("KST", 9*3600))
	sample := newTopSample(hostDetails{Hostname: "host", Cores: 8}, at, resourceRate{NetIn: 1234.567, NetDrops: 2.2, NetHealthValid: true, DiskAwait: 15.555, DiskHealthValid: true, PSIIO: 3.3, PSIValid: true, CPUUser: 12.3456, MemoryPercent: 11.8, Load1: 0.5, SwapOut: 20480.456})
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{`"time":"2026-01-01T02:36:44Z"`, `"hostname":"host"`, `"cores":8`, `"net_in_bytes_per_s":1234.57`, `"network_drops_per_s":2.2`, `"network_health_supported":true`, `"disk_await_ms":15.56`, `"disk_health_supported":true`, `"psi_io_some_avg10_pct":3.3`, `"psi_supported":true`, `"cpu_user_pct":12.35`, `"memory_pct":11.8`, `"load1":0.5`, `"swap_out_bytes_per_s":20480.46`} {
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
	tests := map[float64]string{40: topColorNormal, 92: topColorWarn, 97: topColorDanger}
	for percent, code := range tests {
		if got := formatUsageBar(percent, true); !strings.Contains(got, code) {
			t.Fatalf("formatUsageBar(%v) = %q does not use %q", percent, got, code)
		}
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
