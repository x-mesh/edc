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
	includePublic := set.Bool("public", false, "외부 ipinfo.io 조회로 public IP/지역/ASN 표시")
	timeout := set.Duration("timeout", 5*time.Second, "public 조회 제한 시간")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "사용법: edc info [--public]")
		return 2
	}
	details, err := collectHostDetails()
	if err != nil {
		fmt.Fprintf(os.Stderr, "system 정보를 읽지 못했습니다: %v\n", err)
		return 1
	}
	defaultInterface, gateway := collectDefaultRoute()
	interfaces, interfaceErr := networkInterfaces(defaultInterface, gateway)
	disks, diskErr := collectDisks()
	var public *publicNetworkInfo
	if *includePublic {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		value, err := fetchPublicNetworkInfo(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "public network 조회 실패: %v\n", err)
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
	fmt.Fprintf(writer, "Description : This command displays server resource information.\nVersion     : %s\nAuthor      : %s\n\n%s\n\n", version, author, strings.Repeat("-", 50))
	fmt.Fprintln(writer, "🖥️  System Information")
	fmt.Fprintf(writer, "├── Hostname: %s\n├── System: %s\n├── OS: %s\n├── Version: %s\n├── Release: %s\n├── Machine: %s\n├── Processor: %s\n├── Python Version: %s\n├── Go Version: %s\n├── Model: %s\n├── Cores: %d\n├── Memory: %s\n", details.Hostname, details.System, details.OS, details.Version, details.Release, details.Machine, details.Processor, details.PythonVersion, runtime.Version(), details.Model, details.Cores, formatBytes(details.MemoryTotal))
	fmt.Fprintf(writer, "├── Resource limit\n│   ├── Soft: %s\n│   └── Hard: %s\n", formatLimit(details.RLimitSoft), formatLimit(details.RLimitHard))
	swapPercent := 0.0
	if details.SwapTotal > 0 {
		swapPercent = float64(details.SwapUsed) / float64(details.SwapTotal) * 100
	}
	fmt.Fprintf(writer, "├── Swap Usage: %s / %s (%.2f%%)\n├── CPU Load: %.2f, %.2f, %.2f (1, 5, 15 minutes)\n└── Uptime: %s\n\n", formatBytes(details.SwapUsed), formatBytes(details.SwapTotal), swapPercent, details.Load[0], details.Load[1], details.Load[2], formatDuration(details.Uptime))
	fmt.Fprintln(writer, "🛜 Network Interface")
	if public != nil {
		fmt.Fprintf(writer, "├── Public IP: %s\n│   ├── Region: %s, %s, %s, Timezone=%s\n│   └── ASN/ORG: %s\n", public.IP, public.Country, public.Region, public.City, public.Timezone, public.Org)
	} else {
		fmt.Fprintln(writer, "├── Public IP: (use --public to query ipinfo.io)")
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
	fmt.Fprintln(writer, "\n💾 Disk Usage")
	visible := visibleDisks(disks)
	for index, disk := range visible {
		branch := "├──"
		if index == len(visible)-1 {
			branch = "└──"
		}
		fmt.Fprintf(writer, "%s %-12s %-18s: %9s / %9s %s\n", branch, disk.Mount, disk.Device, formatBytes(disk.Used), formatBytes(disk.Total), formatUsageBar(disk.Percent, color))
	}
}

const (
	// diskBarWidth는 사용률 막대의 칸 수다. 한 칸이 5%다.
	diskBarWidth = 20
	diskBarFull  = "█"
	diskBarEmpty = "░"
)

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
		if disk.Total == 0 || strings.HasPrefix(disk.Mount, "/System/Volumes/") || strings.HasPrefix(disk.Mount, "/private/var/run/") || disk.Mount == "/dev" || strings.HasPrefix(disk.Mount, "/dev/") || disk.Mount == "/proc" || strings.HasPrefix(disk.Mount, "/proc/") || disk.Mount == "/sys" || strings.HasPrefix(disk.Mount, "/sys/") {
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
