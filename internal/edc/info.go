package edc

import (
	"context"
	"encoding/json"
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
	set := newFlagSet("info")
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
	color := richEnabled(os.Stdout)
	printInfo(os.Stdout, version, details, interfaces, disks, public, signals, color, verbose)
	printInfoResources(os.Stdout, memory, memoryErr, signals, processes, processErr, capabilities, color, verbose)
	printInfoListen(os.Stdout, listen, runtime.GOOS == "darwin" && os.Geteuid() != 0, color, verbose)
	printInfoNetworkHealth(os.Stdout, health, color, verbose)
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
	style := newTextStyle(color)
	fmt.Fprintln(writer)
	style.line(writer, style.heading("Memory Status"))
	if memoryErr != nil {
		style.tree(writer, "└──", infoUnavailable(style, memoryErr.Error()))
	} else {
		used := memory.Total - memory.Available
		percent := float64(used) / float64(memory.Total) * 100
		usage := formatBytes(used) + " / " + formatBytes(memory.Total) + " " + style.level(fmt.Sprintf("(%.2f%%)", percent), newTopLimits(1, false).memory.level(percent))
		style.tree(writer, "├──", style.join(" · ", style.kv("Used", usage), style.kv("Available", formatBytes(memory.Available))))
		if line := infoPressureLine(signals, color); line != "" {
			style.tree(writer, "├──", line)
		}
		for index := 0; index < len(memory.Details); index += 2 {
			parts := []string{style.kv(memory.Details[index].Name, formatBytes(memory.Details[index].Bytes))}
			if index+1 < len(memory.Details) {
				parts = append(parts, style.kv(memory.Details[index+1].Name, formatBytes(memory.Details[index+1].Bytes)))
			}
			branch := "├──"
			if !verbose && index+2 >= len(memory.Details) {
				branch = "└──"
			}
			style.tree(writer, branch, style.join(" · ", parts...))
		}
		if verbose {
			style.tree(writer, "├──", style.kv("Basis", memory.Basis))
			style.tree(writer, "└──", style.dim("Detail counters can overlap; they are not parts to add together."))
		}
	}
	fmt.Fprintln(writer)
	style.line(writer, style.heading("Process Snapshot"))
	if processErr != nil {
		style.tree(writer, "└──", infoUnavailable(style, processErr.Error()))
	} else {
		threads, known := 0, 0
		for _, process := range processes {
			if process.Threads > 0 {
				threads, known = threads+process.Threads, known+1
			}
		}
		line := style.kv("Processes observed", fmt.Sprint(len(processes))) + style.dim(" · ")
		if known > 0 {
			line += style.kv("Threads observed", fmt.Sprint(threads)+" "+style.dim(fmt.Sprintf("(%d/%d processes)", known, len(processes))))
		} else {
			line += style.kv("Threads", style.dim("unavailable"))
		}
		style.tree(writer, "├──", line)
		style.tree(writer, "└──", style.dim("Top RSS"))
		if verbose {
			style.line(writer, style.dim("    Resident memory; shared pages can appear in multiple processes."))
		}
		ordered := append([]topProcess(nil), processes...)
		sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].RSS > ordered[j].RSS })
		for _, process := range ordered[:min(3, len(ordered))] {
			rss := style.paint(formatProcessRSS(process.RSS), richSizeColor(int64(process.RSS)))
			style.line(writer, "    "+style.dim("PID")+" "+fmt.Sprintf("%-7d %-28s ", process.PID, topProcessName(process.Command, 28))+rss)
		}
	}
	fmt.Fprintln(writer)
	style.line(writer, style.heading("Diagnostic Support"))
	for index, capability := range capabilities {
		branch := "├──"
		if index == len(capabilities)-1 {
			branch = "└──"
		}
		style.tree(writer, branch, style.kv(capability.Name, infoCapabilityState(style, capability.State)+style.dim(" · "+infoSingleLine(capability.Detail))))
	}
}

// infoListenRows는 -v 없이 보여 줄 노출 포트의 수다. info는 요약이고 전체 목록은 edc listen이 맡는다.
const infoListenRows = 5

// printInfoListen은 TCP와 UDP의 대기 소켓을 요약한다. 밖에서 닿을 수 있는지가 요약에서 묻는 것이므로
// loopback은 개수만 세고, 주소를 해석하지 못한 소켓은 노출된 쪽으로 센다. -v는 loopback까지 모두 보여 준다.
// hidesOtherAccounts는 macOS에서 root가 아니라 다른 계정의 소켓이 목록에서 빠진다는 뜻이다.
func printInfoListen(writer io.Writer, result Result, hidesOtherAccounts, color, verbose bool) {
	style := newTextStyle(color)
	fmt.Fprintln(writer)
	style.line(writer, style.heading("Listening Ports"))
	sockets, ok := result.Metrics["sockets"].([]listenSocket)
	if !ok {
		style.tree(writer, "└──", infoUnavailable(style, result.Summary))
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
	count := func(label string, value int) string { return style.dim(label) + " " + fmt.Sprint(value) }
	lines := []string{style.join(" · ", count("TCP", tcp), count("UDP", udp), count("Exposed", len(sockets)-loopback), count("Loopback", loopback))}
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
			process += style.dim(" (" + socket.PID + ")")
		}
		lines = append(lines, fmt.Sprintf("%-6d ", socket.Port)+style.dim(fmt.Sprintf("%-4s", socket.Proto))+fmt.Sprintf(" %-*s  ", addressWidth, socket.Address)+process)
	}
	if hidden := len(shown) - limit; hidden > 0 {
		lines = append(lines, style.dim(fmt.Sprintf("… %d more exposed · ", hidden))+style.command("edc listen"))
	}
	for _, warning := range result.Warnings {
		lines = append(lines, style.warn("Warning: "+infoSingleLine(warning)))
	}
	if hidesOtherAccounts {
		lines = append(lines, style.dim("Sockets of other accounts are hidden without root"))
	}
	for index, line := range lines {
		branch := "├──"
		if index == len(lines)-1 {
			branch = "└──"
		}
		style.tree(writer, branch, line)
	}
}

func infoSingleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// infoUnavailable은 읽지 못한 섹션의 줄이다. 값이 0인 것과 구분되도록 노랑이다.
func infoUnavailable(style textStyle, reason string) string {
	return style.warn("Unavailable: " + infoSingleLine(reason))
}

// infoCapabilityState는 진단 기능의 상태 색이다. 이 OS에서 원래 없는 기능은 문제가 아니라서 회색이다.
func infoCapabilityState(style textStyle, state string) string {
	switch state {
	case "available", "available for current process", "prerequisites met":
		return style.good(state)
	case "unavailable", "unknown", "permission required":
		return style.warn(state)
	case "unsupported":
		return style.dim(state)
	}
	return state
}

func printInfo(writer io.Writer, version string, details hostDetails, interfaces []interfaceDetails, disks []diskDetails, public *publicNetworkInfo, signals infoSignals, color, verbose bool) {
	style := newTextStyle(color)
	// banner가 이미 버전을 담으므로 Version 줄을 따로 두지 않는다.
	fmt.Fprintf(writer, "%s\n\n", formatBanner(version, color))
	if verbose {
		style.line(writer, style.dim("Description :")+" This command displays server resource information.")
		style.line(writer, style.dim("Author      :")+" "+author)
		fmt.Fprintln(writer)
	}
	style.line(writer, style.heading("🖥️  System Information"))
	swapPercent := 0.0
	if details.SwapTotal > 0 {
		swapPercent = float64(details.SwapUsed) / float64(details.SwapTotal) * 100
	}
	limits := newTopLimits(details.Cores, false)
	loads := make([]string, 0, len(details.Load))
	for _, load := range details.Load {
		loads = append(loads, style.level(fmt.Sprintf("%.2f", load), limits.load.level(load)))
	}
	style.tree(writer, "├──", style.join(" · ", style.kv("Hostname", details.Hostname), topHostOS(details), details.Machine))
	style.tree(writer, "├──", style.join(" · ", style.kv("CPU", details.Model), fmt.Sprint(details.Cores)+" "+style.dim("cores"), style.dim("RAM")+" "+formatBytes(details.MemoryTotal)))
	style.tree(writer, "├──", style.join(" · ", style.kv("Load", strings.Join(loads, "/")+" "+style.dim("(1/5/15m)")), style.kv("Uptime", formatDuration(details.Uptime))))
	if details.PythonVersion != "" {
		style.tree(writer, "├──", style.kv("Runtime", style.join(" · ", "Python "+details.PythonVersion, "Go "+runtime.Version())))
	} else {
		style.tree(writer, "├──", style.kv("Runtime", "Go "+runtime.Version()))
	}
	if line := infoPlatformLine(signals); line != "" {
		style.tree(writer, "├──", style.kv("Platform", line))
	}
	if line := infoClockLine(signals.Clock); line != "" {
		style.tree(writer, "├──", style.kv("Clock", line))
	}
	if verbose {
		style.tree(writer, "├──", style.join(" · ", style.kv("System", details.System), style.kv("Release", details.Release)))
		style.tree(writer, "├──", style.kv("Processor", details.Processor))
		if line := infoFileHandlesLine(signals.FileHandles); line != "" {
			style.tree(writer, "├──", style.kv("Open files (system)", line))
		}
	}
	swap := formatBytes(details.SwapUsed) + "/" + formatBytes(details.SwapTotal) + " " + style.level(fmt.Sprintf("(%.2f%%)", swapPercent), limits.memory.level(swapPercent))
	style.tree(writer, "└──", style.join(" · ", style.kv("Files (edc soft/hard)", formatLimit(details.RLimitSoft)+"/"+formatLimit(details.RLimitHard)), style.kv("Swap", swap)))
	fmt.Fprintln(writer)
	style.line(writer, style.heading("🛜 Network Interface"))
	// 조회하지 못하면 줄 자체를 빼서 없는 값을 설명하지 않는다.
	if public != nil {
		line := public.IP
		if public.Org != "" {
			line += style.dim(" · ") + public.Org
		}
		style.tree(writer, "├──", style.kv("Public IP", line))
		// 기관과 지역은 -v에서 ipinfo가 답했을 때만 있다.
		if public.City != "" || public.Region != "" || public.Country != "" || public.Timezone != "" {
			style.tree(writer, "├──", style.kv("Region", fmt.Sprintf("%s, %s, %s", public.City, public.Region, public.Country)+style.dim(" · ")+public.Timezone))
		}
	}
	style.tree(writer, "├──", style.kv("DNS", infoDNSLine(signals.DNS)))
	if signals.IPv6Default != "" {
		style.tree(writer, "├──", style.kv("IPv6 default route", signals.IPv6Default))
	}
	// -v는 인터페이스 상세를 Local IP 아래에 하나 더 붙이므로 Local IP가 마지막 가지가 아니다.
	showInterfaces := verbose && len(signals.Interfaces) > 0
	branch, indent := "└──", "    "
	if showInterfaces {
		branch, indent = "├──", "│   "
	}
	style.tree(writer, branch, style.dim("Local IP"))
	for index, iface := range interfaces {
		branch := "├──"
		if index == len(interfaces)-1 {
			branch = "└──"
		}
		gateway := ""
		if iface.Gateway != "" {
			gateway = style.dim(",") + " " + style.kv("G/W", iface.Gateway)
		}
		style.line(writer, style.dim(indent+branch)+" "+style.kv(fmt.Sprintf("%-15s", iface.Name), iface.Address+" "+style.dim("/")+" "+iface.Mask+gateway))
	}
	if showInterfaces {
		style.tree(writer, "└──", style.dim("Interfaces"))
		for index, iface := range signals.Interfaces {
			branch := "├──"
			if index == len(signals.Interfaces)-1 {
				branch = "└──"
			}
			style.line(writer, style.dim("    "+branch)+" "+infoInterfaceLine(iface))
		}
	}
	printDiskUsage(writer, disks, signals.Disks, color, verbose)
}

func printDiskUsage(writer io.Writer, disks []diskDetails, signals map[string]diskSignal, color, verbose bool) {
	style := newTextStyle(color)
	fmt.Fprintln(writer)
	style.line(writer, style.heading("💾 Disk Usage"))
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
		row := liveCell(disk.Mount, mountWidth) + " " + style.dim(liveCell(disk.Device, deviceWidth)+":") + " " +
			fmt.Sprintf("%9s / %9s ", formatBytes(disk.Used), formatBytes(disk.Total)) + formatUsageBar(disk.Percent, color) + style.warn(diskSignalSuffix(signals[disk.Mount]))
		style.tree(writer, branch, row)
		indent := "│   "
		if index == len(visible)-1 {
			indent = "    "
		}
		// 마운트 옵션은 -v에서만 읽으므로 기본 출력에서는 비어 있다.
		if options := signals[disk.Mount].Options; options != "" {
			style.line(writer, style.dim(indent)+style.kv("options", options))
		}
		// 지워진 파일의 목록은 길어서 -v에서만 보인다. 기본 출력은 줄 끝의 합계로 충분하다.
		if verbose {
			for _, line := range deletedOpenLines(signals[disk.Mount].DeletedOpen) {
				style.line(writer, style.dim(indent)+style.kv("deleted-open", line))
			}
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
	full, empty := strings.Repeat(diskBarFull, filled), strings.Repeat(diskBarEmpty, diskBarWidth-filled)
	text := fmt.Sprintf(" %6.2f%%", percent)
	if !color {
		return full + empty + text
	}
	level := diskLimits.level(percent)
	return richFill(full, levelColor(level)) + richDim(empty) + textStyle{color: true}.level(text, level)
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
