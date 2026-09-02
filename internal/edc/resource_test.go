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

func TestCalculateInstantCPU(t *testing.T) {
	previous := resourceSnapshot{TakenAt: time.Now()}
	current := resourceSnapshot{TakenAt: previous.TakenAt.Add(time.Second), CPUInstant: true, CPUUser: 1234, CPUSystem: 567, CPUIOWait: 89}
	rate := calculateRate(previous, current)
	if rate.CPUUser != 12.34 || rate.CPUSystem != 5.67 || rate.CPUIOWait != .89 {
		t.Fatalf("instant CPU = %#v", rate)
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

func TestTopSampleJSON(t *testing.T) {
	at := time.Date(2026, 1, 1, 11, 36, 44, 0, time.FixedZone("KST", 9*3600))
	sample := newTopSample(hostDetails{Hostname: "host", Cores: 8}, at, resourceRate{NetIn: 1234.567, CPUUser: 12.3456, MemoryPercent: 11.8, Load1: 0.5})
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{`"time":"2026-01-01T02:36:44Z"`, `"hostname":"host"`, `"cores":8`, `"net_in_bytes_per_s":1234.57`, `"cpu_user_pct":12.35`, `"memory_pct":11.8`, `"load1":0.5`} {
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
