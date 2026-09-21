package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const author = "jinwoo"

type publicNetworkInfo struct {
	IP, City, Region, Country, Timezone, Org string
}

func runInfo(args []string, version string) int {
	set := flag.NewFlagSet("info", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	config := activeConfig.Defaults.Info
	includePublic := set.Bool("public", configuredBool(config.Public, true), T("command.info.option.public"))
	timeout := set.Duration("timeout", configuredDurationFallback(config.Timeout, activeConfig.Defaults.Common.Timeout, 3*time.Second), T("command.info.option.timeout"))
	verbose := configuredBoolFallback(config.Verbose, activeConfig.Defaults.Common.Verbose, false)
	set.BoolVar(&verbose, "verbose", verbose, T("command.info.option.verbose"))
	set.BoolVar(&verbose, "v", verbose, T("option.verbose"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc info [--public=false] [--timeout 3s] [-v]"))
		return 2
	}
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintln(os.Stderr, T("cli.info.host_failed", err))
		return 1
	}
	// python 확인은 shim에 따라 수백 ms가 걸리므로 host 정보를 함께 쓰는 top 시작에서 빼고 info에서만 한다.
	details.PythonVersion = detectPythonVersion()
	defaultInterface, gateway := collectDefaultRoute()
	interfaces, interfaceErr := networkInterfaces(defaultInterface, gateway)
	disks, diskErr := collectDisks()
	var public *publicNetworkInfo
	if *includePublic {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		value, err := fetchPublicNetworkInfo(ctx)
		// 조회는 기본 동작이므로 실패해도 줄만 빼고 진단은 계속한다. 원인은 -v로 본다.
		if err != nil {
			if verbose {
				fmt.Fprintln(os.Stderr, T("cli.info.public_failed", err))
			}
		} else {
			public = &value
		}
	}
	printInfo(os.Stdout, version, details, interfaces, disks, public, isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "")
	if interfaceErr != nil || diskErr != nil {
		return 1
	}
	return 0
}

func printInfo(writer io.Writer, version string, details hostDetails, interfaces []interfaceDetails, disks []diskDetails, public *publicNetworkInfo, color bool) {
	// banner가 이미 버전을 담으므로 Version 줄을 따로 두지 않는다.
	fmt.Fprintf(writer, "%s\nDescription : This command displays server resource information.\nAuthor      : %s\n\n%s\n\n", formatBanner(version, color), author, strings.Repeat("-", 50))
	fmt.Fprintln(writer, "🖥️  System Information")
	fmt.Fprintf(writer, "├── Hostname: %s\n├── System: %s\n├── OS: %s\n├── Version: %s\n├── Release: %s\n├── Machine: %s\n├── Processor: %s\n├── Python Version: %s\n├── Go Version: %s\n├── Model: %s\n├── Cores: %d\n├── Memory: %s\n", details.Hostname, details.System, details.OS, details.Version, details.Release, details.Machine, details.Processor, details.PythonVersion, runtime.Version(), details.Model, details.Cores, formatBytes(details.MemoryTotal))
	fmt.Fprintf(writer, "├── Resource limit\n│   ├── Soft: %s\n│   └── Hard: %s\n", formatLimit(details.RLimitSoft), formatLimit(details.RLimitHard))
	swapPercent := 0.0
	if details.SwapTotal > 0 {
		swapPercent = float64(details.SwapUsed) / float64(details.SwapTotal) * 100
	}
	fmt.Fprintf(writer, "├── Swap Usage: %s / %s (%.2f%%)\n├── CPU Load: %.2f, %.2f, %.2f (1, 5, 15 minutes)\n└── Uptime: %s\n\n", formatBytes(details.SwapUsed), formatBytes(details.SwapTotal), swapPercent, details.Load[0], details.Load[1], details.Load[2], formatDuration(details.Uptime))
	fmt.Fprintln(writer, "🛜 Network Interface")
	// 조회하지 못하면 줄 자체를 빼서 없는 값을 설명하지 않는다.
	if public != nil {
		fmt.Fprintf(writer, "├── Public IP: %s\n│   ├── Region: %s, %s, %s, Timezone=%s\n│   └── ASN/ORG: %s\n", public.IP, public.Country, public.Region, public.City, public.Timezone, public.Org)
	}
	fmt.Fprintln(writer, "└── Local IP")
	for index, iface := range interfaces {
		branch := "├──"
		if index == len(interfaces)-1 {
			branch = "└──"
		}
		gateway := ""
		if iface.Gateway != "" {
			gateway = ", G/W: " + iface.Gateway
		}
		fmt.Fprintf(writer, "    %s %-15s: %s / %s%s\n", branch, iface.Name, iface.Address, iface.Mask, gateway)
	}
	printDiskUsage(writer, disks, color)
}

func printDiskUsage(writer io.Writer, disks []diskDetails, color bool) {
	fmt.Fprintln(writer, "\n💾 Disk Usage")
	visible := visibleDisks(disks)
	// 열 폭을 값에서 잰다. 고정 폭은 긴 mount 경로나 LVM 장치 이름에서 뒤 열을 모두 밀어낸다.
	mountWidth, deviceWidth := diskMountWidth, diskDeviceWidth
	for _, disk := range visible {
		mountWidth = max(mountWidth, liveWidth(disk.Mount))
		deviceWidth = max(deviceWidth, liveWidth(disk.Device))
	}
	for index, disk := range visible {
		branch := "├──"
		if index == len(visible)-1 {
			branch = "└──"
		}
		fmt.Fprintf(writer, "%s %s %s: %9s / %9s %s\n", branch, liveCell(disk.Mount, mountWidth), liveCell(disk.Device, deviceWidth), formatBytes(disk.Used), formatBytes(disk.Total), formatUsageBar(disk.Percent, color))
	}
}

const (
	// diskBarWidth는 사용률 막대의 칸 수다. 한 칸이 5%다.
	diskBarWidth = 20
	diskBarFull  = "█"
	diskBarEmpty = "░"
	// diskMountWidth와 diskDeviceWidth는 두 열의 최소 폭이다. 값이 짧아도 열이 흔들리지 않는다.
	diskMountWidth  = 12
	diskDeviceWidth = 18
)

// diskPseudoDevices는 저장 장치를 쓰지 않는 파일 시스템이다. tmpfs는 램을 쓰고,
// overlay는 container layer라 그 아래 디스크를 다시 센다.
var diskPseudoDevices = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true, "shm": true}

// diskHiddenPrefixes는 숨기는 mount 경로다. snap은 package마다 읽기 전용 image를 하나씩 붙여
// 디스크 한 대를 수십 줄로 늘린다. loop device 자체는 숨기지 않으므로 직접 붙인 image는 남는다.
var diskHiddenPrefixes = []string{"/System/Volumes/", "/private/var/run/", "/snap/"}

// diskHiddenRoots는 그 자신과 하위 경로를 모두 숨기는 mount다.
var diskHiddenRoots = []string{"/dev", "/proc", "/sys"}

// hiddenDisk는 실제 저장 장치를 나타내지 않는 항목이다. os.Stat을 쓰지 않아 어느 platform에서도 검사할 수 있다.
func hiddenDisk(disk diskDetails) bool {
	if disk.Total == 0 || diskPseudoDevices[disk.Device] {
		return true
	}
	for _, prefix := range diskHiddenPrefixes {
		if strings.HasPrefix(disk.Mount, prefix) {
			return true
		}
	}
	for _, root := range diskHiddenRoots {
		if disk.Mount == root || strings.HasPrefix(disk.Mount, root+"/") {
			return true
		}
	}
	return false
}

// diskLimits는 디스크 사용률의 경고와 위험 기준이다. top의 memory 기준과 같다.
var diskLimits = topThreshold{warn: 90, danger: 95}

// formatUsageBar는 사용률을 막대와 백분율로 그린다. 색을 못 쓰면 막대만으로도 정도를 알 수 있다.
func formatUsageBar(percent float64, color bool) string {
	filled := int(percent / 100 * diskBarWidth)
	if filled < 0 {
		filled = 0
	}
	if filled > diskBarWidth {
		filled = diskBarWidth
	}
	bar := strings.Repeat(diskBarFull, filled) + strings.Repeat(diskBarEmpty, diskBarWidth-filled)
	text := fmt.Sprintf("%s %6.2f%%", bar, percent)
	return topLimits{memory: diskLimits, color: color}.paint(text, diskLimits, percent)
}

func fetchPublicNetworkInfo(ctx context.Context) (publicNetworkInfo, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://ipinfo.io/json", nil)
	if err != nil {
		return publicNetworkInfo{}, err
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		return publicNetworkInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return publicNetworkInfo{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var wire struct{ IP, City, Region, Country, Timezone, Org string }
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&wire); err != nil {
		return publicNetworkInfo{}, err
	}
	return publicNetworkInfo(wire), nil
}

func visibleDisks(disks []diskDetails) []diskDetails {
	result := make([]diskDetails, 0, len(disks))
	for _, disk := range disks {
		if hiddenDisk(disk) {
			continue
		}
		if info, err := os.Stat(disk.Mount); err != nil || !info.IsDir() {
			continue
		}
		result = append(result, disk)
	}
	return result
}
func formatLimit(value uint64) string {
	if value == ^uint64(0) {
		return "unlimited"
	}
	return fmt.Sprint(value)
}
func formatDuration(value time.Duration) string {
	days := int(value.Hours()) / 24
	hours := int(value.Hours()) % 24
	minutes := int(value.Minutes()) % 60
	return fmt.Sprintf("%d days, %d hours, %d minutes", days, hours, minutes)
}
