package edc

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// unsupportedOSReason은 probe가 이 OS에서 돌지 않는 이유다.
func unsupportedOSReason() string { return T("observe.system.unsupported_os") }

func probeInterfaces() Result {
	started := time.Now()
	interfaces, err := net.Interfaces()
	if err != nil {
		return resultFromError("net.interfaces", started, "system", err)
	}
	rows := make([]map[string]interface{}, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, _ := iface.Addrs()
		values := make([]string, 0, len(addresses))
		for _, address := range addresses {
			values = append(values, address.String())
		}
		rows = append(rows, map[string]interface{}{"name": iface.Name, "mtu": iface.MTU, "flags": iface.Flags.String(), "addresses": values})
	}
	return Result{Probe: "net.interfaces", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: T("observe.system.interfaces", len(rows)), Metrics: map[string]interface{}{"interfaces": rows}}
}

// probeOutputLimit은 command 출력을 이 크기까지만 남긴다.
const probeOutputLimit = 1 << 20

// probeLineObserver는 command가 한 줄을 낼 때마다 호출된다. 실시간 화면이 진행을 보여 주는 데 쓴다.
type probeLineObserver func(line string)

type probeObserverKey struct{}

// withProbeObserver는 probe 실행 전체에 걸리는 출력 관찰자를 context에 싣는다.
// probe 함수의 signature를 바꾸지 않고 stream 여부만 실행 시점에 정하기 위한 것이다.
func withProbeObserver(ctx context.Context, observe probeLineObserver) context.Context {
	return context.WithValue(ctx, probeObserverKey{}, observe)
}

func probeObserverFrom(ctx context.Context) probeLineObserver {
	observe, _ := ctx.Value(probeObserverKey{}).(probeLineObserver)
	return observe
}

// commandOutput은 stdout과 stderr를 합쳐 상한까지만 남기고 앞뒤 공백을 제거한다.
// observer가 있으면 줄 단위로 흘려 보내면서 같은 내용을 모은다.
func commandOutput(ctx context.Context, path string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, path, args...)
	observe := probeObserverFrom(ctx)
	if observe == nil {
		output, err := command.CombinedOutput()
		if len(output) > probeOutputLimit {
			output = output[:probeOutputLimit]
		}
		return strings.TrimSpace(string(output)), err
	}
	buffer := &remoteLimitedBuffer{limit: probeOutputLimit}
	lines := &probeLineWriter{observe: observe}
	writer := io.MultiWriter(buffer, lines)
	command.Stdout, command.Stderr = writer, writer
	err := command.Run()
	lines.Flush()
	return strings.TrimSpace(buffer.String()), err
}

// probeLineWriter는 들어온 byte를 줄로 잘라 observer에 넘긴다.
type probeLineWriter struct {
	mu      sync.Mutex
	observe probeLineObserver
	pending string
}

func (writer *probeLineWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending += string(data)
	for {
		index := strings.IndexByte(writer.pending, '\n')
		if index < 0 {
			break
		}
		writer.observe(strings.TrimRight(writer.pending[:index], "\r"))
		writer.pending = writer.pending[index+1:]
	}
	return len(data), nil
}

func (writer *probeLineWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.pending != "" {
		writer.observe(writer.pending)
		writer.pending = ""
	}
}

func probeCommand(ctx context.Context, probe, path string, args ...string) Result {
	started := time.Now()
	if _, err := exec.LookPath(path); err != nil {
		return Result{Probe: probe, Status: StatusSkip, StartedAt: started.UTC(), Summary: T("observe.system.command_missing", path)}
	}
	text, err := commandOutput(ctx, path, args...)
	if err != nil {
		return resultFromError(probe, started, classifyCommandError(ctx, err), fmt.Errorf("%s: %s", err, text))
	}
	return Result{Probe: probe, Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: firstLine(text), Evidence: []Evidence{{Label: "output", Value: text}}}
}

func probeRoute(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return routeResult(probeCommand(ctx, "net.route", "/sbin/route", "-n", "get", target), "darwin")
	case "linux":
		// ip route get은 host 이름을 받지 않으므로 먼저 주소로 바꾼다.
		address, err := resolveRouteAddress(ctx, target)
		if err != nil {
			return resultFromError("net.route", time.Now(), "dns", err)
		}
		return routeResult(probeCommand(ctx, "net.route", "ip", "route", "get", address), "linux")
	default:
		return unsupported("net.route", unsupportedOSReason())
	}
}

type routeDetails struct {
	destination string
	gateway     string
	interfaceID string
}

func parseRouteDetails(output, platform string) routeDetails {
	var details routeDetails
	if platform == "darwin" {
		for _, line := range strings.Split(output, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			switch key {
			case "route to":
				details.destination = strings.TrimSpace(value)
			case "gateway":
				details.gateway = strings.TrimSpace(value)
			case "interface":
				details.interfaceID = strings.TrimSpace(value)
			}
		}
		return details
	}
	fields := strings.Fields(strings.Split(output, "\n")[0])
	if len(fields) == 0 {
		return details
	}
	start := 1
	if fields[0] == "local" || fields[0] == "broadcast" || fields[0] == "multicast" {
		if len(fields) < 2 {
			return details
		}
		start = 2
	}
	details.destination = fields[start-1]
	for index := start; index+1 < len(fields); index++ {
		switch fields[index] {
		case "via":
			details.gateway = fields[index+1]
		case "dev":
			details.interfaceID = fields[index+1]
		}
	}
	return details
}

func routeResult(result Result, platform string) Result {
	if result.Status != StatusPass || len(result.Evidence) == 0 {
		return result
	}
	details := parseRouteDetails(result.Evidence[0].Value, platform)
	if details.destination == "" {
		return result
	}
	result.Summary = "route to: " + details.destination
	result.Metrics = map[string]interface{}{"destination": details.destination}
	if details.gateway != "" {
		result.Summary += ", via " + details.gateway
		result.Metrics["gateway"] = details.gateway
	}
	if details.interfaceID != "" {
		result.Summary += ", dev " + details.interfaceID
		result.Metrics["interface"] = details.interfaceID
	}
	return result
}

// resolveRouteAddress는 IPv4 주소를 우선 고른다. 주소 literal은 그대로 돌려준다.
func resolveRouteAddress(ctx context.Context, target string) (string, error) {
	if net.ParseIP(target) != nil {
		return target, nil
	}
	addresses, err := net.DefaultResolver.LookupHost(ctx, target)
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", fmt.Errorf("%s", T("observe.system.address_missing", target))
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
			return address, nil
		}
	}
	return addresses[0], nil
}

func probeDNSConfig(ctx context.Context) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "dns.config", "/usr/sbin/scutil", "--dns")
	case "linux":
		return probeResolvConf(ctx, "/etc/resolv.conf")
	default:
		return unsupported("dns.config", unsupportedOSReason())
	}
}

type resolvConfig struct {
	Nameservers []string
	Search      []string
}

// parseResolvConf는 nameserver, search, domain 줄만 읽는다. 나머지는 evidence 원문으로 남긴다.
func parseResolvConf(text string) resolvConfig {
	config := resolvConfig{Nameservers: []string{}, Search: []string{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			config.Nameservers = append(config.Nameservers, fields[1])
		case "search", "domain":
			config.Search = append(config.Search, fields[1:]...)
		}
	}
	return config
}

func probeResolvConf(ctx context.Context, path string) Result {
	started := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return resultFromError("dns.config", started, "system", err)
	}
	text := strings.TrimSpace(string(data))
	config := parseResolvConf(text)
	result := Result{
		Probe: "dns.config", Status: StatusPass, StartedAt: started.UTC(),
		Summary:  "nameserver " + strings.Join(config.Nameservers, ", "),
		Metrics:  map[string]interface{}{"path": path, "nameservers": config.Nameservers, "search": config.Search},
		Evidence: []Evidence{{Label: path, Value: text}},
	}
	if len(config.Nameservers) == 0 {
		result.Status = StatusWarn
		result.Summary = T("observe.system.no_nameserver", path)
		result.Warnings = append(result.Warnings, T("observe.system.nameserver_warning"))
	}
	// systemd-resolved는 resolv.conf에 stub 주소만 남기므로 실제 upstream은 resolvectl에서 읽는다.
	if _, err := exec.LookPath("resolvectl"); err == nil {
		output, err := commandOutput(ctx, "resolvectl", "status")
		if err != nil {
			result.Warnings = append(result.Warnings, T("observe.system.resolvectl_failed", firstLine(output)))
		} else {
			result.Evidence = append(result.Evidence, Evidence{Label: "resolvectl status", Value: output})
		}
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// listenProbeID는 명령 이름이자 JSON과 doctor 결과에 나가는 probe ID다. 둘을 같은 값으로 둔다.
const listenProbeID = "listen"

// probeListen은 연결을 기다리는 소켓을 본다. 연결된 소켓은 대상이 아니다. families가 볼 종류를
// 정한다. UDP에는 LISTEN 상태가 없지만 ss -ul과 마찬가지로 바인드되어 수신을 기다리는 소켓을 같은
// 관점으로 다룬다.
//
// 두 플랫폼 모두 기계용 출력을 쓴다. lsof -F는 필드마다 한 줄을 쓰고, ss는 -t와 -u를 함께 주면
// 프로토콜 열이 생겨 형식이 하나로 고정된다. 사람용 표를 열로 쪼개지 않는다.
func probeListen(ctx context.Context, families listenFamilies) Result {
	var result Result
	var sockets []listenSocket
	var unparsed int
	switch runtime.GOOS {
	case "darwin":
		// -i와 -U는 선택 조건이라 함께 주면 합집합이 된다. 유닉스 소켓에는 P(프로토콜) 필드가 없어
		// t(종류) 필드를 함께 받는다.
		args := []string{"-nP", "-FpcntPTL"}
		if families.TCP || families.UDP {
			args = append(args, "-i")
		}
		if families.Unix {
			args = append(args, "-U")
		}
		result = probeCommand(ctx, listenProbeID, "/usr/sbin/lsof", args...)
		if result.Status != StatusPass {
			return result
		}
		sockets, unparsed = parseLsofFields(socketOutput(result), families)
	case "linux":
		// TCP만 볼 때도 -tu로 물어 프로토콜 열을 얻는다. 형식이 하나면 파서도 하나다.
		// -e는 uid를 붙인다. 계정 이름은 -v에서만 쓰지만 한 형식으로 물어야 파서가 하나로 남는다.
		flags := "-Htulnpe"
		if families.Unix {
			flags = "-Htulxnpe"
		}
		result = probeCommand(ctx, listenProbeID, "ss", flags)
		if result.Status != StatusPass {
			return result
		}
		sockets, unparsed = parseSSRows(socketOutput(result), families)
	default:
		return unsupported(listenProbeID, unsupportedOSReason())
	}
	sockets = dedupeListenSockets(sockets)
	result.Summary = T("observe.system.listen", len(sockets))
	// 표는 화면에 직접 그리므로 evidence에 넣지 않는다. 넣으면 -v에서 두 번 나온다.
	// JSON에는 사람이 읽는 표 대신 구조화된 목록을 담는다.
	result.Metrics = map[string]interface{}{"listening": len(sockets), "sockets": sockets}
	if unparsed > 0 {
		result.Status = StatusWarn
		result.Warnings = append(result.Warnings, T("observe.listen.warn.unparsed", unparsed))
	}
	// lsof는 유닉스 소켓의 상태를 알려 주지 않는다. 경로를 가진 소켓과 정말로 연결을 기다리는 소켓을
	// 가를 수 없으므로 근사치임을 밝힌다. 상태를 알 수 없는 것과 목록이 틀린 것은 다르다.
	if families.Unix && runtime.GOOS == "darwin" {
		result.Warnings = append(result.Warnings, T("observe.listen.warn.unix_state"))
	}
	return result
}

// socketOutput은 probeCommand가 남긴 원문을 꺼낸다.
func socketOutput(result Result) string {
	for _, evidence := range result.Evidence {
		if evidence.Label == "output" {
			return evidence.Value
		}
	}
	return ""
}

// countSocketRows는 열 제목을 뺀 소켓 줄 수를 센다. 소켓이 하나도 없으면 lsof는 아무 줄도 내지
// 않으므로 제목을 빼지 않는다.
func countSocketRows(text string) int {
	rows := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			rows++
		}
	}
	if rows == 0 {
		return 0
	}
	return rows - 1
}

func probeQuality(ctx context.Context) Result {
	started := time.Now()
	if runtime.GOOS != "darwin" {
		return unsupported("net.quality", T("observe.system.quality_darwin_only"))
	}
	command := exec.CommandContext(ctx, "/usr/bin/networkQuality", "-c")
	output, err := command.Output()
	if err != nil {
		return resultFromError("net.quality", started, classifyCommandError(ctx, err), err)
	}
	var metrics map[string]interface{}
	if err := json.Unmarshal(output, &metrics); err != nil {
		return resultFromError("net.quality", started, "parse", err)
	}
	return Result{Probe: "net.quality", Status: StatusPass, StartedAt: started.UTC(), DurationMS: time.Since(started).Milliseconds(), Summary: T("observe.system.quality_done"), Metrics: metrics}
}

func probePing(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return pingResult(probeCommand(ctx, "net.ping", "/sbin/ping", "-n", "-c", "4", "-W", "2000", target))
	case "linux":
		// Linux ping의 -W 단위는 초다.
		return pingResult(probeCommand(ctx, "net.ping", "ping", "-n", "-c", "4", "-W", "2", target))
	default:
		return unsupported("net.ping", unsupportedOSReason())
	}
}

var pingLossPattern = regexp.MustCompile(`(?m)(\d+) packets transmitted,\s*(\d+)(?: packets)? received[^\n]*?([\d.]+)% packet loss`)
var pingAveragePattern = regexp.MustCompile(`(?:round-trip|rtt) min/avg/max/(?:stddev|mdev) = [\d.]+/([\d.]+)/`)
var pingTargetPattern = regexp.MustCompile(`PING [^\n]*?\(([^)]+)\)`)

func pingResult(result Result) Result {
	if result.Status == StatusSkip {
		return result
	}
	output := result.Summary
	if len(result.Evidence) > 0 {
		output = result.Evidence[0].Value
	}
	match := pingLossPattern.FindStringSubmatch(output)
	if len(match) == 0 {
		return result
	}
	transmitted, _ := strconv.Atoi(match[1])
	received, _ := strconv.Atoi(match[2])
	loss, _ := strconv.ParseFloat(match[3], 64)
	result.Summary = T("observe.probe.ping_loss", match[3], received, transmitted)
	result.Metrics = map[string]interface{}{"transmitted": transmitted, "received": received, "packet_loss_percent": loss}
	if target := pingTargetPattern.FindStringSubmatch(output); len(target) > 0 && net.ParseIP(target[1]) != nil {
		result.Metrics["target_ip"] = target[1]
		result.Summary += ", " + T("observe.probe.ping_target", target[1])
	}
	if average := pingAveragePattern.FindStringSubmatch(output); len(average) > 0 {
		if value, err := strconv.ParseFloat(average[1], 64); err == nil {
			result.Metrics["avg_rtt_ms"] = value
			result.Summary += ", " + T("observe.probe.ping_average", average[1])
		}
	}
	return result
}

func probeTrace(ctx context.Context, target string) Result {
	switch runtime.GOOS {
	case "darwin":
		return probeCommand(ctx, "net.trace", "/usr/sbin/traceroute", "-n", "-m", "20", "-w", "2", target)
	case "linux":
		path, args, found := linuxTraceCommand(exec.LookPath, target)
		if !found {
			return Result{Probe: "net.trace", Status: StatusSkip, StartedAt: time.Now().UTC(), Summary: T("observe.system.trace_missing")}
		}
		return probeCommand(ctx, "net.trace", path, args...)
	default:
		return unsupported("net.trace", unsupportedOSReason())
	}
}

// linuxTraceCommand는 traceroute를 우선 쓰고, 없으면 iputils의 tracepath를 쓴다.
func linuxTraceCommand(lookPath func(string) (string, error), target string) (string, []string, bool) {
	if _, err := lookPath("traceroute"); err == nil {
		return "traceroute", []string{"-n", "-m", "20", "-w", "2", target}, true
	}
	if _, err := lookPath("tracepath"); err == nil {
		return "tracepath", []string{"-n", "-m", "20", target}, true
	}
	return "", nil, false
}

func unsupported(probe, reason string) Result {
	return Result{Probe: probe, Status: StatusSkip, StartedAt: time.Now().UTC(), Summary: reason}
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(value, "\n")
	if len(line) > 160 {
		return line[:160] + "…"
	}
	return line
}

func classifyCommandError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timeout"
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("exit_%d", exit.ExitCode())
	}
	return "command"
}

func sortResults(results []Result) {
	sort.Slice(results, func(i, j int) bool { return results[i].Probe < results[j].Probe })
}

// runListen은 listen 명령을 실행한다. 종류 플래그는 더하기가 아니라 필터다. --tcp는 TCP를 켜는 것이
// 아니라 TCP만 남기고, 아무것도 주지 않으면 기본 집합을 본다.
//
// 이 명령의 주 출력은 표다. 사람이 묻는 것이 "어느 포트가 열려 있나"이므로 개수 한 줄로는 답이
// 되지 않는다. where와 마찬가지로 JSON이면 JSON을, 터미널이면 표를 직접 그린다.
func runListen(args []string, version string) int {
	options := configuredCommon(15 * time.Second)
	set := flag.NewFlagSet(listenProbeID, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	tcp := set.Bool("tcp", false, T("command.listen.option.tcp"))
	udp := set.Bool("udp", false, T("command.listen.option.udp"))
	set.BoolVar(udp, "u", false, T("command.listen.option.udp"))
	unix := set.Bool("unix", false, T("command.listen.option.unix"))
	all := set.Bool("all", false, T("command.listen.option.all"))
	watch := set.Bool("watch", false, T("command.listen.option.watch"))
	intervalText := "1"
	set.StringVar(&intervalText, "i", intervalText, T("command.watch.option.interval"))
	set.StringVar(&intervalText, "interval", intervalText, T("command.watch.option.interval"))
	duration := set.Duration("duration", 0, T("command.watch.option.duration"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("cli.error.no_positional", listenProbeID))
		return 2
	}
	if *watch {
		return runListenWatch(options, selectedListenFamilies(*tcp, *udp, *unix, *all), intervalText, *duration)
	}
	started := time.Now()
	ctx, cancel, deadline := probeContext(options.timeout)
	defer cancel()
	defer deadline()
	result := probeListen(ctx, selectedListenFamilies(*tcp, *udp, *unix, *all))
	if options.jsonPath != "" {
		return emit(options, buildReport(version, started, nil, []Result{result}, options.redact))
	}
	color := isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == ""
	var output io.Writer = os.Stdout
	var buffer strings.Builder
	if options.redact {
		output = &buffer
	}
	fmt.Fprint(output, formatResultLine(result, color))
	// evidence는 lsof -F와 ss의 원문, 곧 파서의 입력이다. 사람에게 그것을 펼치면 표가 감추려던
	// 형식을 도로 보여 주는 셈이므로 화면에서는 열지 않는다. 원문은 --json에 그대로 남는다.
	printResultDetail(output, result, false, color)
	// 표는 화면에서 언제나 보여 준다. 개수 한 줄로는 "어느 포트가 열려 있나"에 답이 되지 않는다.
	if table := listenTableOf(result, options.verbose); table != "" {
		fmt.Fprint(output, "\n"+table)
	}
	if options.redact {
		fmt.Fprint(os.Stdout, redactIPAddresses(buffer.String()))
	}
	return exitCode([]Result{result})
}

// listenTableOf는 결과의 metrics에 담아 둔 목록으로 표를 만든다.
func listenTableOf(result Result, detail bool) string {
	sockets, ok := result.Metrics["sockets"].([]listenSocket)
	if !ok || len(sockets) == 0 {
		return ""
	}
	return formatListenTable(sockets, detail)
}
