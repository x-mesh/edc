package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tcpScanProbeID = "tcp.scan"
	// tcpScanDialTimeout은 port 하나의 connect 상한이다. SYN이 버려지는 port 하나가 scan 전체의 예산을 쓰지 못하게 한다.
	tcpScanDialTimeout     = 2 * time.Second
	tcpScanDefaultParallel = 32
	tcpScanMaxParallel     = 256
	tcpScanDefaultTimeout  = 60 * time.Second
	// tcpScanProgressInterval은 진행 줄을 고치는 간격이다. port마다 보내면 큰 범위에서 화면이 메시지에 잠긴다.
	tcpScanProgressInterval = 100 * time.Millisecond
	// tcpScanSummaryOpenLimit은 요약 줄에 번호로 적는 open port의 수다. 나머지는 evidence에 있다.
	tcpScanSummaryOpenLimit = 5
	tcpScanMaxPort          = 65535
)

func init() { evidenceShownProbes[tcpScanProbeID] = true }

// tcpScanPortNames는 port 옆에 적는 서비스 이름이다. audit이 지켜보는 민감 port의 이름은 auditSensitivePorts에서 가져온다.
var tcpScanPortNames = map[int]string{
	21: "ftp", 22: "ssh", 23: "telnet", 25: "smtp", 53: "dns", 80: "http", 110: "pop3", 143: "imap",
	443: "https", 445: "smb", 465: "smtps", 587: "submission", 993: "imaps", 995: "pop3s",
	1433: "mssql", 3389: "rdp", 5900: "vnc", 8080: "http-alt", 8443: "https-alt",
}

// tcpScanDefaultPorts는 --ports가 없을 때 보는 port다. audit이 사고로 보는 민감 port에 원격 접속·웹·메일 port를 더해,
// audit에 port가 늘면 scan의 기본값도 따라간다.
func tcpScanDefaultPorts() []int {
	seen := map[int]bool{}
	for port := range auditSensitivePorts {
		seen[port] = true
	}
	for port := range tcpScanPortNames {
		seen[port] = true
	}
	ports := make([]int, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func tcpScanPortName(port int) string {
	if name, ok := tcpScanPortNames[port]; ok {
		return name
	}
	return auditSensitivePorts[port]
}

type portState string

const (
	portOpen       portState = "open"
	portRefused    portState = "refused"
	portNoReply    portState = "no_reply"
	portError      portState = "error"
	portNotScanned portState = "not_scanned"
)

// portResult는 port 하나의 connect 결과다. Elapsed는 화면용이고 JSON에는 MS로 남는다.
type portResult struct {
	Port    int           `json:"port"`
	State   portState     `json:"state"`
	MS      float64       `json:"connect_ms,omitempty"`
	Error   string        `json:"error,omitempty"`
	Elapsed time.Duration `json:"-"`
}

type tcpScanOptions struct {
	common   commonOptions
	ports    []int
	parallel int
}

func runTCPScan(args []string, version string) int {
	options := tcpScanOptions{parallel: tcpScanDefaultParallel}
	spec := ""
	flags := probeFlags{
		bind: func(set *flag.FlagSet) {
			set.StringVar(&spec, "ports", "", T("command.tcp.option.ports"))
			set.IntVar(&options.parallel, "parallel", options.parallel, T("command.tcp.option.parallel"))
		},
		check: func() error {
			if options.parallel < 1 || options.parallel > tcpScanMaxParallel {
				return errors.New(T("scan.error.parallel_range", tcpScanMaxParallel))
			}
			if strings.TrimSpace(spec) == "" {
				options.ports = tcpScanDefaultPorts()
				return nil
			}
			ports, err := parsePortSet(spec)
			if err != nil {
				return err
			}
			options.ports = ports
			return nil
		},
		parsed: func(common commonOptions) error {
			options.common = common
			return nil
		},
		prompt: T("cli.prompt.host"),
	}
	return runTargetProbeWithOptions(args, version, "tcp scan", tcpScanProbeID, configuredCommon(tcpScanDefaultTimeout), flags, func(ctx context.Context, input string) Result {
		return probeTCPScan(ctx, input, options)
	})
}

// parsePortSet은 "22,80,443,8000-8100"을 정렬된 중복 없는 port 목록으로 바꾼다.
func parsePortSet(spec string) ([]int, error) {
	var seen [tcpScanMaxPort + 1]bool
	count := 0
	for _, token := range strings.Split(spec, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		low, high, err := parsePortToken(token)
		if err != nil {
			return nil, err
		}
		for port := low; port <= high; port++ {
			if !seen[port] {
				seen[port] = true
				count++
			}
		}
	}
	if count == 0 {
		return nil, errors.New(T("scan.error.ports_empty"))
	}
	ports := make([]int, 0, count)
	for port := 1; port <= tcpScanMaxPort; port++ {
		if seen[port] {
			ports = append(ports, port)
		}
	}
	return ports, nil
}

func parsePortToken(token string) (int, int, error) {
	first, second, isRange := strings.Cut(token, "-")
	low, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0, 0, errors.New(T("scan.error.ports_invalid", token))
	}
	high := low
	if isRange {
		if high, err = strconv.Atoi(strings.TrimSpace(second)); err != nil {
			return 0, 0, errors.New(T("scan.error.ports_invalid", token))
		}
	}
	if low < 1 || high > tcpScanMaxPort || low > high {
		return 0, 0, errors.New(T("scan.error.ports_range", token))
	}
	return low, high, nil
}

// parseScanHost는 대상을 host 하나로 읽는다. tcp check의 버릇으로 host:port를 주면 --ports를 가리키며 거절한다.
func parseScanHost(input string) (string, error) {
	host := strings.TrimSpace(input)
	if host == "" {
		return "", errors.New(T("scan.error.host_empty"))
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		inner := host[1 : len(host)-1]
		if net.ParseIP(inner) == nil {
			return "", errors.New(T("scan.error.host_only", input))
		}
		return inner, nil
	}
	if net.ParseIP(host) != nil {
		return host, nil
	}
	if strings.Contains(host, ":") {
		return "", errors.New(T("scan.error.host_only", input))
	}
	return host, nil
}

func probeTCPScan(ctx context.Context, input string, options tcpScanOptions) Result {
	started := time.Now()
	host, err := parseScanHost(input)
	if err != nil {
		return resultFromError(tcpScanProbeID, started, "input", err)
	}
	address, err := resolveRouteAddress(ctx, host)
	if err != nil {
		return resultFromError(tcpScanProbeID, started, "dns", err)
	}
	scanner := tcpScanner{dial: dialTCPOnce, parallel: options.parallel, timeout: tcpScanDialTimeout, observe: scanProgressReporter(ctx)}
	results := scanner.scan(ctx, address, options.ports)
	return scanResult(started, host, address, results, options, scanner.timeout)
}

func dialTCPOnce(ctx context.Context, address string) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
}

// scanProgressReporter는 실시간 한 줄에 "120/1024 port · open 3"을 쓴다. 화면이 없으면 nil이다.
func scanProgressReporter(ctx context.Context) func(done, total, open int) {
	observe := probeObserverFrom(ctx)
	if observe == nil {
		return nil
	}
	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(done, total, open int) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if done != total && now.Sub(last) < tcpScanProgressInterval {
			return
		}
		last = now
		observe(T("scan.progress", done, total, open))
	}
}

// tcpScanner는 port 목록을 worker가 나눠 연결한다. dial은 test가 바꿔 끼운다.
type tcpScanner struct {
	dial     func(ctx context.Context, address string) error
	parallel int
	timeout  time.Duration
	observe  func(done, total, open int)
}

// scan은 parallel개의 worker가 queue에서 port를 꺼내 connect한다. ctx가 끝나면 남은 port는 꺼내지 않고
// not scanned로 둔다. port마다 goroutine을 열지 않는 것은 65535개를 한꺼번에 띄우지 않기 위해서다.
func (scanner tcpScanner) scan(ctx context.Context, address string, ports []int) []portResult {
	results := make([]portResult, len(ports))
	for index, port := range ports {
		results[index] = portResult{Port: port, State: portNotScanned}
	}
	queue := make(chan int)
	var (
		group sync.WaitGroup
		mu    sync.Mutex
		done  int
		open  int
	)
	workers := min(max(scanner.parallel, 1), len(ports))
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range queue {
				results[index] = scanner.probe(ctx, address, ports[index])
				if scanner.observe == nil {
					continue
				}
				mu.Lock()
				done++
				if results[index].State == portOpen {
					open++
				}
				finished, found := done, open
				mu.Unlock()
				scanner.observe(finished, len(ports), found)
			}
		}()
	}
feed:
	for index := range ports {
		select {
		case queue <- index:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	group.Wait()
	return results
}

// probe는 port 하나를 판정한다. 부모 ctx가 먼저 끝난 시도는 관측하지 못한 것이므로 no reply가 아니라 not scanned다.
func (scanner tcpScanner) probe(ctx context.Context, address string, port int) portResult {
	result := portResult{Port: port, State: portNotScanned}
	if ctx.Err() != nil {
		return result
	}
	attempt, cancel := context.WithTimeout(ctx, scanner.timeout)
	defer cancel()
	started := time.Now()
	err := scanner.dial(attempt, net.JoinHostPort(address, strconv.Itoa(port)))
	result.Elapsed = time.Since(started)
	result.MS = milliseconds(result.Elapsed)
	switch {
	case err == nil:
		result.State = portOpen
	case ctx.Err() != nil:
		result.State = portNotScanned
		result.Elapsed, result.MS = 0, 0
	case errors.Is(attempt.Err(), context.DeadlineExceeded):
		result.State = portNoReply
	case errors.Is(err, syscall.ECONNREFUSED) || classifyNetworkError(err) == "connection_refused":
		result.State = portRefused
	default:
		result.State = portError
		result.Error = err.Error()
	}
	return result
}

// portCounts는 상태별 개수와 요약에 쓸 port다.
type portCounts struct {
	open, refused, noReply, errors, notScanned int
	openPorts, noReplyPorts, refusedPorts      []int
	notScannedPorts                            []int
	errorLines                                 []string
	firstError                                 string
}

func countPorts(results []portResult) portCounts {
	var counts portCounts
	for _, result := range results {
		switch result.State {
		case portOpen:
			counts.open++
			counts.openPorts = append(counts.openPorts, result.Port)
		case portRefused:
			counts.refused++
			counts.refusedPorts = append(counts.refusedPorts, result.Port)
		case portNoReply:
			counts.noReply++
			counts.noReplyPorts = append(counts.noReplyPorts, result.Port)
		case portError:
			counts.errors++
			counts.errorLines = append(counts.errorLines, fmt.Sprintf("%d  %s", result.Port, result.Error))
			if counts.firstError == "" {
				counts.firstError = result.Error
			}
		default:
			counts.notScanned++
			counts.notScannedPorts = append(counts.notScannedPorts, result.Port)
		}
	}
	return counts
}

func (counts portCounts) answered() int { return counts.open + counts.refused }

// scanResult는 scan 하나를 Result 하나로 만든다. 상태 규칙: 모든 port를 확인했는데 응답 전에 모두 오류면 FAIL,
// 아무 port도 답하지 않았거나 예산이 먼저 끝났으면 WARN, 그 밖에는 PASS다. 예산이 먼저 끝난 scan은 보지 못한 port가
// 있으므로 모두 실패했다고 단정하지 않는다. 응답 없음은 닫힘으로 적지 않는다.
func scanResult(started time.Time, host, address string, results []portResult, options tcpScanOptions, dialTimeout time.Duration) Result {
	counts := countPorts(results)
	total := len(results)
	scanned := total - counts.notScanned
	metrics := map[string]interface{}{
		"host": host, "address": address, "parallel": options.parallel, "connect_timeout_ms": dialTimeout.Milliseconds(),
		"ports_total": total, "scanned": scanned, "open": counts.open, "refused": counts.refused, "no_reply": counts.noReply,
		"errors": counts.errors, "not_scanned": counts.notScanned, "open_ports": append([]int{}, counts.openPorts...), "ports": results,
	}
	evidence := scanEvidence(counts, results)
	var result Result
	switch {
	case counts.notScanned == 0 && counts.answered()+counts.noReply == 0:
		result = resultFromError(tcpScanProbeID, started, "network", errors.New(T("scan.error.all_errors", counts.firstError)))
	default:
		elapsed := time.Since(started)
		result = Result{
			Probe: tcpScanProbeID, Status: StatusPass, StartedAt: started.UTC(), DurationMS: elapsed.Milliseconds(),
			Summary: scanSummary(counts, total, elapsed, host, address, dialTimeout),
		}
		silent := counts.answered() == 0 && counts.noReply > 0
		if silent || counts.notScanned > 0 {
			result.Status = StatusWarn
		}
		if silent {
			result.Next = "edc tcp check --diagnose " + net.JoinHostPort(host, strconv.Itoa(counts.noReplyPorts[0]))
		}
		if counts.noReply > 0 {
			result.Warnings = append(result.Warnings, T("scan.warning.no_reply", counts.noReply, dialTimeout))
		}
		if counts.errors > 0 && counts.answered()+counts.noReply == 0 {
			result.Warnings = append(result.Warnings, T("scan.warning.errors", counts.errors, counts.firstError))
		}
		switch {
		case scanned == 0:
			result.Warnings = append(result.Warnings, T("scan.warning.none_checked", options.common.timeout))
		case counts.notScanned > 0:
			result.Warnings = append(result.Warnings, T("scan.warning.partial", scanned, total, options.common.timeout))
		}
	}
	result.Metrics = metrics
	result.Evidence = evidence
	return result
}

// scanSummary는 "3 open (22, 80, 443) · 17 refused · 2 no reply · 22 ports in 1.2s, peer 1.2.3.4"다.
// 0인 항목은 open 말고는 적지 않는다. 아무 port도 답하지 않았으면 그 사실을 맨 앞에 둔다.
func scanSummary(counts portCounts, total int, elapsed time.Duration, host, address string, dialTimeout time.Duration) string {
	var parts []string
	if counts.answered() == 0 && counts.noReply > 0 {
		parts = append(parts, T("scan.summary.silent", dialTimeout))
	}
	if counts.open == 0 {
		parts = append(parts, T("scan.summary.open", 0))
	} else {
		parts = append(parts, T("scan.summary.open_list", counts.open, portList(counts.openPorts, tcpScanSummaryOpenLimit)))
	}
	for _, part := range []struct {
		count int
		key   string
	}{{counts.refused, "scan.summary.refused"}, {counts.noReply, "scan.summary.no_reply"}, {counts.errors, "scan.summary.error"}, {counts.notScanned, "scan.summary.not_scanned"}} {
		if part.count > 0 {
			parts = append(parts, T(part.key, part.count))
		}
	}
	parts = append(parts, T("scan.summary.total", total, elapsed.Round(time.Millisecond)))
	summary := strings.Join(parts, " · ")
	if address != host {
		summary += ", " + T("observe.probe.peer_ip", address)
	}
	return summary
}

func scanEvidence(counts portCounts, results []portResult) []Evidence {
	var evidence []Evidence
	if counts.open > 0 {
		evidence = append(evidence, Evidence{Label: T("scan.evidence.open"), Value: strings.Join(scanOpenLines(results), "\n")})
	}
	for _, group := range []struct {
		ports []int
		key   string
	}{{counts.refusedPorts, "scan.evidence.refused"}, {counts.noReplyPorts, "scan.evidence.no_reply"}, {counts.notScannedPorts, "scan.evidence.not_scanned"}} {
		if len(group.ports) > 0 {
			evidence = append(evidence, Evidence{Label: T(group.key), Value: portRanges(group.ports)})
		}
	}
	if counts.errors > 0 {
		evidence = append(evidence, Evidence{Label: T("scan.evidence.error"), Value: strings.Join(counts.errorLines, "\n")})
	}
	return evidence
}

// scanOpenLines는 open port를 "  443  https  12ms" 꼴로 적는다. 번호는 5칸에 오른쪽 맞춤해 줄마다 열이 맞고,
// 이름이 없는 port는 번호와 시간만 적는다.
func scanOpenLines(results []portResult) []string {
	var lines []string
	for _, result := range results {
		if result.State != portOpen {
			continue
		}
		parts := []string{fmt.Sprintf("%5d", result.Port)}
		if name := tcpScanPortName(result.Port); name != "" {
			parts = append(parts, name)
		}
		parts = append(parts, connectDuration(result.Elapsed).String())
		lines = append(lines, strings.Join(parts, "  "))
	}
	return lines
}

// connectDuration은 1ms 아래의 loopback 연결이 0s로 보이지 않게 단위를 내린다.
func connectDuration(elapsed time.Duration) time.Duration {
	if elapsed < time.Millisecond {
		return elapsed.Round(10 * time.Microsecond)
	}
	return elapsed.Round(time.Millisecond)
}

// portList는 앞의 limit개만 번호로 적고 나머지는 개수로 접는다.
func portList(ports []int, limit int) string {
	shown := ports
	if len(shown) > limit {
		shown = shown[:limit]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, port := range shown {
		parts = append(parts, strconv.Itoa(port))
	}
	if rest := len(ports) - len(shown); rest > 0 {
		parts = append(parts, T("scan.more", rest))
	}
	return strings.Join(parts, ", ")
}

// portRanges는 정렬된 port를 "1-21, 23, 25-79"로 접는다. no reply가 수천 개여도 evidence가 한 줄에 든다.
func portRanges(ports []int) string {
	var parts []string
	for index := 0; index < len(ports); {
		end := index
		for end+1 < len(ports) && ports[end+1] == ports[end]+1 {
			end++
		}
		if end == index {
			parts = append(parts, strconv.Itoa(ports[index]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ports[index], ports[end]))
		}
		index = end + 1
	}
	return strings.Join(parts, ", ")
}
