package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
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
	// 수집기는 서로 의존하지 않는다. 공인 IP 조회와 python shim 확인은 각각 수백 ms가 걸리므로 순서대로 돌리면 시간이 더해진다.
	var (
		collectors                                            sync.WaitGroup
		details                                               hostDetails
		hostErr, interfaceErr, diskErr, memoryErr, processErr error
		interfaces                                            []interfaceDetails
		disks                                                 []diskDetails
		memory                                                infoMemory
		processes                                             []topProcess
		capabilities                                          []infoCapability
		listen                                                Result
		health                                                *networkHealth
		public                                                *publicNetworkInfo
		publicErr, publicIPErr                                error
		publicIP, pythonVersion                               string
		signals                                               infoSignals
		diskSignals                                           map[string]diskSignal
	)
	collectors.Go(func() { details, hostErr = collectHostDetails() })
	// python 확인은 shim에 따라 수백 ms가 걸려 info 기본 실행에서 가장 느렸다. top과 info 기본 실행에서는 빼고 -v에서만 한다.
	if verbose {
		collectors.Go(func() { pythonVersion = detectPythonVersion() })
	}
	collectors.Go(func() {
		defaultInterface, gateway := collectDefaultRoute()
		interfaces, interfaceErr = networkInterfaces(defaultInterface, gateway)
	})
	collectors.Go(func() {
		disks, diskErr = collectDisks()
		diskSignals = collectDiskSignals(disks, verbose)
	})
	collectors.Go(func() { signals = collectInfoSignals(verbose) })
	collectors.Go(func() { memory, memoryErr = collectInfoMemory() })
	collectors.Go(func() { processes, processErr = collectInfoProcesses() })
	collectors.Go(func() { capabilities = collectInfoCapabilities() })
	collectors.Go(func() { listen = probeListen(context.Background(), defaultListenFamilies()) })
	collectors.Go(func() { health = collectNetworkHealth() })
	// 기본은 DNS로 IP만 받는다. ipinfo.io는 TLS 뒤 첫 응답까지 약 190ms가 걸려 info에서 가장 느린 항목이었다.
	// 기관과 지역은 ipinfo만 주므로 -v에서만 함께 묻는다.
	if *includePublic {
		collectors.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			publicIP, publicIPErr = fetchPublicIPByDNS(ctx)
		})
	}
	if *includePublic && verbose {
		collectors.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			value, err := fetchPublicNetworkInfo(ctx)
			if err == nil {
				public = &value
			}
			publicErr = err
		})
	}
	collectors.Wait()
	if hostErr != nil {
		fmt.Fprintln(os.Stderr, T("cli.info.host_failed", hostErr))
		return 1
	}
	details.PythonVersion = pythonVersion
	signals.Disks = diskSignals
	// 조회는 기본 동작이므로 실패해도 줄만 빼고 진단은 계속한다. 원인은 -v로 본다.
	if verbose {
		for _, err := range []error{publicIPErr, publicErr} {
			if err != nil {
				fmt.Fprintln(os.Stderr, T("cli.info.public_failed", err))
			}
		}
	}
	// -v에서 ipinfo가 실패해도 DNS로 받은 IP는 보여 준다.
	if public == nil && publicIPErr == nil && publicIP != "" {
		public = &publicNetworkInfo{IP: publicIP}
	}
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	printInfo(os.Stdout, version, details, interfaces, disks, public, signals, color, verbose)
	printInfoResources(os.Stdout, memory, memoryErr, signals, processes, processErr, capabilities, color, verbose)
	printInfoListen(os.Stdout, listen, runtime.GOOS == "darwin" && os.Geteuid() != 0, verbose)
	printInfoNetworkHealth(os.Stdout, health, verbose)
	// 포트 요약은 info에 새로 붙은 섹션이다. sock_diag나 libproc을 막는 sandbox에서 읽지 못해도 섹션에 Unavailable만 적고,
	// 전에 0으로 끝나던 info의 exit code는 바꾸지 않는다.
	if interfaceErr != nil || diskErr != nil || memoryErr != nil || processErr != nil {
		return 1
	}
	return 0
}

type infoMemory struct {
	Total, Available uint64
	Basis            string
	Details          []infoMemoryDetail
}

type infoMemoryDetail struct {
	Name  string
	Bytes uint64
}

type infoCapability struct {
	Name, State, Detail string
}

func printInfoResources(writer io.Writer, memory infoMemory, memoryErr error, signals infoSignals, processes []topProcess, processErr error, capabilities []infoCapability, color, verbose bool) {
	fmt.Fprintln(writer, "\nMemory Status")
	if memoryErr != nil {
		fmt.Fprintf(writer, "└── Unavailable: %s\n", infoSingleLine(memoryErr.Error()))
	} else {
		fmt.Fprintf(writer, "├── Used: %s / %s (%.2f%%) · Available: %s\n", formatBytes(memory.Total-memory.Available), formatBytes(memory.Total), float64(memory.Total-memory.Available)/float64(memory.Total)*100, formatBytes(memory.Available))
		if line := infoPressureLine(signals, color); line != "" {
			fmt.Fprintf(writer, "├── %s\n", line)
		}
		for index := 0; index < len(memory.Details); index += 2 {
			parts := []string{memory.Details[index].Name + ": " + formatBytes(memory.Details[index].Bytes)}
			if index+1 < len(memory.Details) {
				parts = append(parts, memory.Details[index+1].Name+": "+formatBytes(memory.Details[index+1].Bytes))
			}
			branch := "├──"
			if !verbose && index+2 >= len(memory.Details) {
				branch = "└──"
			}
			fmt.Fprintf(writer, "%s %s\n", branch, strings.Join(parts, " · "))
		}
		if verbose {
			fmt.Fprintf(writer, "├── Basis: %s\n└── Detail counters can overlap; they are not parts to add together.\n", memory.Basis)
		}
	}
	fmt.Fprintln(writer, "\nProcess Snapshot")
	if processErr != nil {
		fmt.Fprintf(writer, "└── Unavailable: %s\n", infoSingleLine(processErr.Error()))
	} else {
		threads, known := 0, 0
		for _, process := range processes {
			if process.Threads > 0 {
				threads, known = threads+process.Threads, known+1
			}
		}
		fmt.Fprintf(writer, "├── Processes observed: %d", len(processes))
		if known > 0 {
			fmt.Fprintf(writer, " · Threads observed: %d (%d/%d processes)\n", threads, known, len(processes))
		} else {
			fmt.Fprintln(writer, " · Threads: unavailable")
		}
		fmt.Fprintln(writer, "└── Top RSS")
		if verbose {
			fmt.Fprintln(writer, "    Resident memory; shared pages can appear in multiple processes.")
		}
		ordered := append([]topProcess(nil), processes...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RSS > ordered[j].RSS })
		for _, process := range ordered[:min(3, len(ordered))] {
			fmt.Fprintf(writer, "    PID %-7d %-28s %s\n", process.PID, topProcessName(process.Command, 28), formatProcessRSS(process.RSS))
		}
	}
	fmt.Fprintln(writer, "\nDiagnostic Support")
	for index, capability := range capabilities {
		branch := "├──"
		if index == len(capabilities)-1 {
			branch = "└──"
		}
		fmt.Fprintf(writer, "%s %s: %s · %s\n", branch, capability.Name, capability.State, infoSingleLine(capability.Detail))
	}
}

// infoListenRows는 -v 없이 보여 줄 노출 포트의 수다. info는 요약이고 전체 목록은 edc listen이 맡는다.
const infoListenRows = 5

// printInfoListen은 TCP와 UDP의 대기 소켓을 요약한다. 밖에서 닿을 수 있는지가 요약에서 묻는 것이므로
// loopback은 개수만 세고, 주소를 해석하지 못한 소켓은 노출된 쪽으로 센다. -v는 loopback까지 모두 보여 준다.
// hidesOtherAccounts는 macOS에서 root가 아니라 다른 계정의 소켓이 목록에서 빠진다는 뜻이다.
func printInfoListen(writer io.Writer, result Result, hidesOtherAccounts, verbose bool) {
	fmt.Fprintln(writer, "\nListening Ports")
	sockets, ok := result.Metrics["sockets"].([]listenSocket)
	if !ok {
		fmt.Fprintf(writer, "└── Unavailable: %s\n", infoSingleLine(result.Summary))
		return
	}
	var tcp, udp, loopback int
	var shown []listenSocket
	for _, socket := range sockets {
		switch socket.Proto {
		case "tcp":
			tcp++
		case "udp":
			udp++
		}
		local := listenBindingScope(socket.Proto, socket.Address) == "loopback"
		if local {
			loopback++
		}
		if verbose || !local {
			shown = append(shown, socket)
		}
	}
	lines := []string{fmt.Sprintf("TCP %d · UDP %d · Exposed %d · Loopback %d", tcp, udp, len(sockets)-loopback, loopback)}
	limit := len(shown)
	if !verbose {
		limit = min(limit, infoListenRows)
	}
	addressWidth := 0
	for _, socket := range shown[:limit] {
		addressWidth = max(addressWidth, len(socket.Address))
	}
	for _, socket := range shown[:limit] {
		process := listenCell(socket.Process)
		if socket.PID != "" {
			process += " (" + socket.PID + ")"
		}
		lines = append(lines, fmt.Sprintf("%-6d %-4s %-*s  %s", socket.Port, socket.Proto, addressWidth, socket.Address, process))
	}
	if hidden := len(shown) - limit; hidden > 0 {
		lines = append(lines, fmt.Sprintf("… %d more exposed · edc listen", hidden))
	}
	for _, warning := range result.Warnings {
		lines = append(lines, "Warning: "+infoSingleLine(warning))
	}
	if hidesOtherAccounts {
		lines = append(lines, "Sockets of other accounts are hidden without root")
	}
	for index, line := range lines {
		branch := "├──"
		if index == len(lines)-1 {
			branch = "└──"
		}
		fmt.Fprintf(writer, "%s %s\n", branch, line)
	}
}

func infoSingleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func printInfo(writer io.Writer, version string, details hostDetails, interfaces []interfaceDetails, disks []diskDetails, public *publicNetworkInfo, signals infoSignals, color, verbose bool) {
	// banner가 이미 버전을 담으므로 Version 줄을 따로 두지 않는다.
	fmt.Fprintf(writer, "%s\n\n", formatBanner(version, color))
	if verbose {
		fmt.Fprintf(writer, "Description : This command displays server resource information.\nAuthor      : %s\n\n", author)
	}
	fmt.Fprintln(writer, "🖥️  System Information")
	swapPercent := 0.0
	if details.SwapTotal > 0 {
		swapPercent = float64(details.SwapUsed) / float64(details.SwapTotal) * 100
	}
	fmt.Fprintf(writer, "├── Hostname: %s · %s · %s\n├── CPU: %s · %d cores · RAM %s\n├── Load: %.2f/%.2f/%.2f (1/5/15m) · Uptime: %s\n", details.Hostname, topHostOS(details), details.Machine, details.Model, details.Cores, formatBytes(details.MemoryTotal), details.Load[0], details.Load[1], details.Load[2], formatDuration(details.Uptime))
	if details.PythonVersion != "" {
		fmt.Fprintf(writer, "├── Runtime: Python %s · Go %s\n", details.PythonVersion, runtime.Version())
	} else {
		fmt.Fprintf(writer, "├── Runtime: Go %s\n", runtime.Version())
	}
	if line := infoPlatformLine(signals); line != "" {
		fmt.Fprintf(writer, "├── Platform: %s\n", line)
	}
	if line := infoClockLine(signals.Clock); line != "" {
		fmt.Fprintf(writer, "├── Clock: %s\n", line)
	}
	if verbose {
		fmt.Fprintf(writer, "├── System: %s · Release: %s\n├── Processor: %s\n", details.System, details.Release, details.Processor)
		if line := infoFileHandlesLine(signals.FileHandles); line != "" {
			fmt.Fprintf(writer, "├── Open files (system): %s\n", line)
		}
	}
	fmt.Fprintf(writer, "└── Files (edc soft/hard): %s/%s · Swap: %s/%s (%.2f%%)\n\n", formatLimit(details.RLimitSoft), formatLimit(details.RLimitHard), formatBytes(details.SwapUsed), formatBytes(details.SwapTotal), swapPercent)
	fmt.Fprintln(writer, "🛜 Network Interface")
	// 조회하지 못하면 줄 자체를 빼서 없는 값을 설명하지 않는다.
	if public != nil {
		line := public.IP
		if public.Org != "" {
			line += " · " + public.Org
		}
		fmt.Fprintf(writer, "├── Public IP: %s\n", line)
		// 기관과 지역은 -v에서 ipinfo가 답했을 때만 있다.
		if public.City != "" || public.Region != "" || public.Country != "" || public.Timezone != "" {
			fmt.Fprintf(writer, "├── Region: %s, %s, %s · %s\n", public.City, public.Region, public.Country, public.Timezone)
		}
	}
	fmt.Fprintf(writer, "├── DNS: %s\n", infoDNSLine(signals.DNS))
	if signals.IPv6Default != "" {
		fmt.Fprintf(writer, "├── IPv6 default route: %s\n", signals.IPv6Default)
	}
	// -v는 인터페이스 상세를 Local IP 아래에 하나 더 붙이므로 Local IP가 마지막 가지가 아니다.
	showInterfaces := verbose && len(signals.Interfaces) > 0
	branch, indent := "└──", "    "
	if showInterfaces {
		branch, indent = "├──", "│   "
	}
	fmt.Fprintf(writer, "%s Local IP\n", branch)
	for index, iface := range interfaces {
		branch := "├──"
		if index == len(interfaces)-1 {
			branch = "└──"
		}
		gateway := ""
		if iface.Gateway != "" {
			gateway = ", G/W: " + iface.Gateway
		}
		fmt.Fprintf(writer, "%s%s %-15s: %s / %s%s\n", indent, branch, iface.Name, iface.Address, iface.Mask, gateway)
	}
	if showInterfaces {
		fmt.Fprintln(writer, "└── Interfaces")
		for index, iface := range signals.Interfaces {
			branch := "├──"
			if index == len(signals.Interfaces)-1 {
				branch = "└──"
			}
			fmt.Fprintf(writer, "    %s %s\n", branch, infoInterfaceLine(iface))
		}
	}
	printDiskUsage(writer, disks, signals.Disks, color)
}

func printDiskUsage(writer io.Writer, disks []diskDetails, signals map[string]diskSignal, color bool) {
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
		fmt.Fprintf(writer, "%s %s %s: %9s / %9s %s%s\n", branch, liveCell(disk.Mount, mountWidth), liveCell(disk.Device, deviceWidth), formatBytes(disk.Used), formatBytes(disk.Total), formatUsageBar(disk.Percent, color), diskSignalSuffix(signals[disk.Mount]))
		// 마운트 옵션은 -v에서만 읽으므로 기본 출력에서는 비어 있다.
		if options := signals[disk.Mount].Options; options != "" {
			indent := "│   "
			if index == len(visible)-1 {
				indent = "    "
			}
			fmt.Fprintf(writer, "%soptions: %s\n", indent, options)
		}
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

// o-o.myaddr.l.google.com의 TXT는 질의를 보낸 주소다. 재귀 resolver를 거치면 resolver의 주소가 오므로 Google의
// 권한 서버에 직접 묻는다. 1.1.1.1의 whoami.cloudflare는 이 질의를 가로채는 망에서 다른 주소를 돌려줘서 쓰지 않는다.
const (
	publicIPDNSName   = "o-o.myaddr.l.google.com."
	publicIPDNSServer = "ns1.google.com:53"
)

func fetchPublicIPByDNS(ctx context.Context) (string, error) {
	message := new(dns.Msg)
	message.SetQuestion(publicIPDNSName, dns.TypeTXT)
	// IPv6로 물으면 IPv6 주소가 온다. ipinfo.io가 보여 주던 IPv4와 맞추려고 IPv4로 묻는다.
	client := &dns.Client{Net: "udp4"}
	response, _, err := client.ExchangeContext(ctx, message, publicIPDNSServer)
	if err != nil {
		return "", err
	}
	if response.Rcode != dns.RcodeSuccess {
		return "", fmt.Errorf("%s: %s", publicIPDNSName, dns.RcodeToString[response.Rcode])
	}
	for _, answer := range response.Answer {
		record, ok := answer.(*dns.TXT)
		if !ok {
			continue
		}
		for _, value := range record.Txt {
			if address, err := netip.ParseAddr(value); err == nil {
				return address.String(), nil
			}
		}
	}
	return "", fmt.Errorf("%s: no address in the TXT answer", publicIPDNSName)
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
