package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const netMTUProbeID = "net.mtu"

const (
	// ping의 -s는 ICMP data 크기다. 경로 MTU는 IP packet 전체 크기이므로 IP와 ICMP 헤더를 더한다.
	mtuIPv4Overhead = 20 + 8
	mtuIPv6Overhead = 40 + 8
	// 576은 IPv4 host가 반드시 받아야 하는 크기이고, 1280은 IPv6 link의 최소 MTU다.
	// 이보다 작은 경로는 사실상 없으므로 여기서 응답이 없으면 ICMP가 막힌 것으로 본다.
	mtuIPv4Floor = 576
	mtuIPv6Floor = 1280
	// TCP MSS는 IP와 TCP 헤더를 뺀 크기다. IPv4 20+20, IPv6 40+20.
	mtuIPv4TCPHeaders = 40
	mtuIPv6TCPHeaders = 60
	mtuMaxLimit       = 65535
	// 블랙홀에서는 큰 크기마다 응답을 기다려야 한다. 기본 --timeout 15초 안에 이분 탐색을 마치려고
	// 시도 하나를 1초로 묶는다.
	mtuAttemptWait = time.Second
	// ping이 스스로 끝나지 않는 경우(macOS ping6에는 대기 option이 없다)에도 시도가 이 시간을 넘지 않게 한다.
	mtuAttemptSlack = 500 * time.Millisecond
)

type mtuOutcome string

const (
	mtuReply mtuOutcome = "reply"
	// mtuTooBig은 로컬 kernel이 보내기 전에 거절한 경우다. interface MTU보다 크거나 kernel이 더 작은 경로 MTU를 이미 안다.
	mtuTooBig mtuOutcome = "too_big"
	// mtuFragNeeded는 경로의 router가 ICMP fragmentation needed(IPv6 packet too big)로 알려 준 경우다.
	mtuFragNeeded mtuOutcome = "frag_needed"
	mtuTimeout    mtuOutcome = "timeout"
	mtuDenied     mtuOutcome = "denied"
	mtuError      mtuOutcome = "error"
)

type mtuAttempt struct {
	Size    int        `json:"size"`
	Outcome mtuOutcome `json:"outcome"`
	// MTU는 kernel이나 router가 알려 준 다음 hop의 MTU다. 알려 주지 않으면 0이다.
	MTU int `json:"mtu,omitempty"`
	// detail은 사람이 읽을 ping 오류 줄이다. JSON에는 outcome만 남긴다.
	detail string
}

type mtuSender func(ctx context.Context, size int) mtuAttempt

func runNetMTU(args []string, version string) int {
	maxSize := 0
	flags := probeFlags{
		bind: func(set *flag.FlagSet) {
			set.IntVar(&maxSize, "max", maxSize, T("command.net.option.max"))
		},
		check: func() error {
			if maxSize != 0 && (maxSize < mtuIPv4Floor || maxSize > mtuMaxLimit) {
				return errors.New(T("mtu.error.max_range", mtuIPv4Floor, mtuMaxLimit))
			}
			return nil
		},
		prompt: T("cli.prompt.host"),
	}
	return runTargetProbeWithFlags(args, version, "net mtu", netMTUProbeID, flags, func(ctx context.Context, target string) Result {
		return probeNetMTU(ctx, target, maxSize)
	})
}

// mtuDeps는 경로와 ping을 바꿔 끼워 탐색과 판정을 OS 없이 시험하게 한다.
type mtuDeps struct {
	goos           string
	resolve        func(context.Context, string) (string, error)
	routeInterface func(context.Context, string) (string, Result)
	interfaceMTU   func(string) (int, error)
	lookPath       func(string) (string, error)
	sender         func(goos string, address net.IP) mtuSender
}

func defaultMTUDeps() mtuDeps {
	return mtuDeps{
		goos:           runtime.GOOS,
		resolve:        resolveRouteAddress,
		routeInterface: mtuRouteInterface,
		interfaceMTU: func(name string) (int, error) {
			iface, err := net.InterfaceByName(name)
			if err != nil {
				return 0, err
			}
			return iface.MTU, nil
		},
		lookPath: exec.LookPath,
		sender:   mtuPingSender,
	}
}

func probeNetMTU(ctx context.Context, target string, maxSize int) Result {
	return probeNetMTUWith(ctx, defaultMTUDeps(), target, maxSize)
}

func probeNetMTUWith(ctx context.Context, deps mtuDeps, target string, maxSize int) Result {
	started := time.Now()
	if deps.goos != "darwin" && deps.goos != "linux" {
		return unsupported(netMTUProbeID, unsupportedOSReason())
	}
	text, err := deps.resolve(ctx, target)
	if err != nil {
		return resultFromError(netMTUProbeID, started, "dns", err)
	}
	address := net.ParseIP(text)
	if address == nil {
		return resultFromError(netMTUProbeID, started, "dns", errors.New(T("observe.system.address_missing", target)))
	}
	ipv6 := address.To4() == nil
	name, failed := deps.routeInterface(ctx, address.String())
	if name == "" {
		failed.Probe = netMTUProbeID
		return failed
	}
	interfaceMTU, err := deps.interfaceMTU(name)
	if err != nil {
		return resultFromError(netMTUProbeID, started, "system", err)
	}
	path, _ := mtuPingCommand(deps.goos, ipv6, 0, address.String())
	if _, err := deps.lookPath(path); err != nil {
		return Result{Probe: netMTUProbeID, Status: StatusSkip, StartedAt: started.UTC(), Summary: T("observe.system.command_missing", path)}
	}
	plan := mtuPlan{floor: mtuIPv4Floor, max: maxSize, observe: probeObserverFrom(ctx)}
	if ipv6 {
		plan.floor = mtuIPv6Floor
	}
	// DF를 켠 packet은 interface MTU를 넘으면 kernel이 보내지 않는다. 그 위는 시도할 필요가 없다.
	// IP packet은 65535를 넘지 못한다. Linux loopback은 MTU 65536을 보고하고, 그 크기의 -s는 ping이 거절한다.
	limit := min(interfaceMTU, mtuMaxLimit)
	if plan.max == 0 || plan.max > limit {
		plan.max = limit
	}
	plan.floor = min(plan.floor, plan.max)
	finding := searchPathMTU(ctx, plan, deps.sender(deps.goos, address))
	result := mtuVerdict(mtuTarget{input: target, address: address, ipv6: ipv6, iface: name, interfaceMTU: interfaceMTU, max: plan.max}, finding)
	result.StartedAt = started.UTC()
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// mtuRouteInterface는 net route와 같은 명령으로 대상에 쓰는 interface를 찾는다.
// 찾지 못하면 그 이유를 담은 Result를 돌려준다.
func mtuRouteInterface(ctx context.Context, address string) (string, Result) {
	// route 명령의 출력이 진행 줄에 섞이지 않게 한다. 진행 줄은 탐색 단계만 보여 준다.
	route := probeRoute(withProbeObserver(ctx, nil), address)
	if name, _ := route.Metrics["interface"].(string); name != "" {
		return name, Result{}
	}
	if route.Status == StatusPass {
		route.Status = StatusFail
		route.Summary = T("mtu.error.no_interface", address)
		route.Error = &DiagnosticError{Kind: "route", Message: route.Summary}
	}
	return "", route
}

// mtuPingCommand는 Don't Fragment를 켜고 echo 하나만 보내는 ping 명령을 만든다.
// payload는 ICMP data 크기다.
func mtuPingCommand(goos string, ipv6 bool, payload int, address string) (string, []string) {
	size := fmt.Sprint(payload)
	switch goos {
	case "darwin":
		if ipv6 {
			// ping6의 -D가 fragment를 막고, -m이 최소 MTU(1280)로 자르는 기본 동작을 끈다.
			// 대기 시간 option이 없으므로 시도마다 context로 끊는다.
			return "/sbin/ping6", []string{"-n", "-D", "-m", "-c", "1", "-s", size, address}
		}
		// macOS ping의 -W 단위는 밀리초다.
		return "/sbin/ping", []string{"-n", "-D", "-c", "1", "-W", fmt.Sprint(mtuAttemptWait.Milliseconds()), "-s", size, address}
	default:
		args := []string{"-n", "-M", "do", "-c", "1", "-W", fmt.Sprint(int(mtuAttemptWait.Seconds())), "-s", size, address}
		if ipv6 {
			args = append([]string{"-6"}, args...)
		}
		return "ping", args
	}
}

func mtuPingSender(goos string, address net.IP) mtuSender {
	ipv6 := address.To4() == nil
	overhead := mtuIPv4Overhead
	if ipv6 {
		overhead = mtuIPv6Overhead
	}
	return func(ctx context.Context, size int) mtuAttempt {
		path, args := mtuPingCommand(goos, ipv6, size-overhead, address.String())
		attemptCtx, cancel := context.WithTimeout(ctx, mtuAttemptWait+mtuAttemptSlack)
		defer cancel()
		// macOS ping은 로컬에서 sendto가 실패해도 대기 시간만큼 응답을 기다린다. 결과가 정해지는 줄이
		// 나오면 바로 끊는다. 이 observer는 ping 출력이 진행 줄에 섞이는 것도 막는다.
		attemptCtx = withProbeObserver(attemptCtx, func(line string) {
			if mtuTooBigPattern.MatchString(line) || mtuFragNeededPattern.MatchString(line) {
				cancel()
			}
		})
		text, err := commandOutput(attemptCtx, path, args...)
		attempt := classifyMTUPing(text, err)
		attempt.Size = size
		return attempt
	}
}

var (
	// Linux: "Frag needed and DF set (mtu = 1400)", "Packet too big: mtu=1400"
	// macOS: "frag needed and DF set (MTU 1400)"
	mtuFragNeededPattern = regexp.MustCompile(`(?i)(?:frag(?:mentation)? needed and DF set|packet too big)[^\n]*?mtu\s*=?\s*(\d+)`)
	// Linux: "ping: sendmsg: Message too long", "ping: local error: message too long, mtu=1400"
	// macOS: "ping: sendto: Message too long"
	mtuTooBigPattern = regexp.MustCompile(`(?i)message too long(?:, mtu\s*=\s*(\d+))?`)
	mtuReplyPattern  = regexp.MustCompile(`(?m)^\d+ bytes from `)
	// macOS ping6 -D는 root가 아니면 "IPV6_DONTFRAG: Operation not permitted"로 끝난다.
	mtuDeniedPattern = regexp.MustCompile(`(?i)operation not permitted`)
	// Linux: "ping: packet size 65508 is too large. Maximum is 65507"
	// macOS: "ping: packet size too large: 65508 > 65507"
	mtuErrorPattern = regexp.MustCompile(`(?i)unknown host|cannot resolve|network is unreachable|no route to host|usage:|invalid (?:argument|option)|unrecognized option|illegal option|too large`)
)

// classifyMTUPing은 ping 출력으로 시도 결과를 정한다. 두 OS 모두 too long과 무응답에서 같은 exit code를
// 내므로 exit code가 아니라 출력 문구로 나눈다.
func classifyMTUPing(text string, err error) mtuAttempt {
	if match := mtuFragNeededPattern.FindStringSubmatch(text); match != nil {
		return mtuAttempt{Outcome: mtuFragNeeded, MTU: atoiOrZero(match[1])}
	}
	if match := mtuTooBigPattern.FindStringSubmatch(text); match != nil {
		return mtuAttempt{Outcome: mtuTooBig, MTU: atoiOrZero(match[1])}
	}
	if mtuReplyPattern.MatchString(text) {
		return mtuAttempt{Outcome: mtuReply}
	}
	if mtuDeniedPattern.MatchString(text) {
		return mtuAttempt{Outcome: mtuDenied, detail: firstLine(text)}
	}
	if mtuErrorPattern.MatchString(text) {
		return mtuAttempt{Outcome: mtuError, detail: firstLine(text)}
	}
	// 시도 시간이 지나 끊긴 ping은 ExitError나 context 오류를 낸다. 그 밖의 오류는 ping을 띄우지도 못한 경우다.
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return mtuAttempt{Outcome: mtuError, detail: err.Error()}
	}
	return mtuAttempt{Outcome: mtuTimeout}
}

func atoiOrZero(text string) int {
	value, _ := strconv.Atoi(text)
	return value
}

type mtuPlan struct {
	floor   int
	max     int
	observe probeLineObserver
}

type mtuFinding struct {
	attempts []mtuAttempt
	// pathMTU는 응답을 받은 가장 큰 크기다. floor에서도 응답이 없으면 0이다.
	pathMTU int
	// upper는 실패한 가장 작은 크기이고, limit은 그 크기의 결과다. max까지 응답했으면 upper는 0이다.
	upper int
	limit mtuAttempt
	// start는 floor 시도의 결과다. reply가 아니면 탐색하지 않았다.
	start      mtuAttempt
	blackHole  bool
	incomplete bool
}

// searchPathMTU는 floor, max 순으로 보낸 뒤 그 사이를 이분 탐색한다. 대부분의 경로는 MTU가 interface와
// 같으므로 두 번에 끝난다. kernel이나 router가 MTU를 알려 주면 그 크기로 바로 건너뛴다.
func searchPathMTU(ctx context.Context, plan mtuPlan, send mtuSender) mtuFinding {
	var finding mtuFinding
	low, high := 0, plan.max+1
	try := func(size int) (mtuAttempt, bool) {
		if plan.observe != nil {
			plan.observe(mtuProgressLine(size, low, high, plan.max))
		}
		var attempt mtuAttempt
		// 무응답은 한 번 손실일 수 있다. 블랙홀로 판정하기 전에 한 번 더 보낸다.
		for round := 0; round < 2; round++ {
			if !mtuBudgetLeft(ctx) {
				finding.incomplete = true
				return attempt, false
			}
			attempt = send(ctx, size)
			if ctx.Err() != nil {
				finding.incomplete = true
				return attempt, false
			}
			finding.attempts = append(finding.attempts, attempt)
			if attempt.Outcome != mtuTimeout {
				break
			}
		}
		return attempt, true
	}
	start, ok := try(plan.floor)
	finding.start = start
	if !ok || start.Outcome != mtuReply {
		return finding
	}
	low = plan.floor
	finding.pathMTU = low
	next := plan.max
	for next > low && next < high {
		attempt, ok := try(next)
		if !ok {
			break
		}
		if attempt.Outcome == mtuReply {
			low = next
			finding.pathMTU = low
			// kernel이나 router가 알려 준 MTU에서 응답했으면 그보다 큰 크기는 같은 hop에서 막힌다.
			if next == finding.limit.MTU {
				break
			}
		} else {
			high = next
			finding.upper = next
			finding.limit = attempt
			if attempt.Outcome == mtuDenied || attempt.Outcome == mtuError {
				break
			}
		}
		if attempt.MTU > low && attempt.MTU < high {
			next = attempt.MTU
			continue
		}
		next = low + (high-low)/2
	}
	for _, attempt := range finding.attempts {
		if attempt.Outcome == mtuTimeout && attempt.Size > finding.pathMTU {
			finding.blackHole = true
		}
	}
	return finding
}

// mtuBudgetLeft는 남은 시간 안에 시도 하나를 끝낼 수 있는지다. 끝내지 못할 시도는 보내지 않고
// 그때까지 찾은 범위를 결과로 남긴다.
func mtuBudgetLeft(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > mtuAttemptWait+mtuAttemptSlack
}

func mtuProgressLine(size, low, high, max int) string {
	if low == 0 {
		return T("mtu.progress.first", size)
	}
	if high > max {
		return T("mtu.progress.open", size, low)
	}
	return T("mtu.progress.range", size, low, high)
}

type mtuTarget struct {
	input        string
	address      net.IP
	ipv6         bool
	iface        string
	interfaceMTU int
	max          int
}

func mtuOutcomeLabel(outcome mtuOutcome) string {
	switch outcome {
	case mtuReply:
		return T("mtu.outcome.reply")
	case mtuTooBig:
		return T("mtu.outcome.too_big")
	case mtuFragNeeded:
		return T("mtu.outcome.frag_needed")
	case mtuTimeout:
		return T("mtu.outcome.timeout")
	case mtuDenied:
		return T("mtu.outcome.denied")
	default:
		return T("mtu.outcome.error")
	}
}

func mtuAttemptsEvidence(attempts []mtuAttempt) Evidence {
	lines := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		line := fmt.Sprintf("%5d  %s", attempt.Size, mtuOutcomeLabel(attempt.Outcome))
		if attempt.MTU > 0 {
			line += "  " + T("mtu.evidence.reported", attempt.MTU)
		}
		if attempt.detail != "" {
			line += "  " + attempt.detail
		}
		lines = append(lines, line)
	}
	return Evidence{Label: T("mtu.evidence.attempts"), Value: strings.Join(lines, "\n")}
}

// mtuVerdict는 탐색 결과를 Result로 바꾼다. 경로 MTU가 interface MTU와 같으면 pass, 작거나 블랙홀이면
// warn, ICMP가 막혀 잴 수 없으면 skip이다.
func mtuVerdict(target mtuTarget, finding mtuFinding) Result {
	result := Result{Probe: netMTUProbeID}
	metrics := map[string]interface{}{
		"target_ip":     target.address.String(),
		"interface":     target.iface,
		"interface_mtu": target.interfaceMTU,
		"max":           target.max,
		"attempts":      finding.attempts,
		"black_hole":    finding.blackHole,
		"complete":      !finding.incomplete,
	}
	result.Metrics = metrics
	if len(finding.attempts) > 0 {
		result.Evidence = []Evidence{mtuAttemptsEvidence(finding.attempts)}
	}
	if finding.start.Outcome != mtuReply {
		switch {
		case finding.incomplete && len(finding.attempts) == 0:
			result.Status = StatusSkip
			result.Summary = T("mtu.skip.no_time")
		case finding.start.Outcome == mtuDenied:
			result.Status = StatusSkip
			result.Summary = T("mtu.skip.denied", finding.start.detail)
			if target.ipv6 {
				result.Warnings = []string{T("mtu.hint.ipv6_root")}
			}
		case finding.start.Outcome == mtuError:
			result.Status = StatusFail
			result.Summary = T("mtu.error.ping", finding.start.detail)
			result.Error = &DiagnosticError{Kind: "command", Message: finding.start.detail}
		default:
			result.Status = StatusSkip
			result.Summary = T("mtu.skip.blocked", plannedFloor(finding, target))
			result.Warnings = []string{T("mtu.hint.tcp", net.JoinHostPort(target.input, "<port>"))}
		}
		return result
	}
	pathMTU := finding.pathMTU
	tcpHeaders := mtuIPv4TCPHeaders
	if target.ipv6 {
		tcpHeaders = mtuIPv6TCPHeaders
	}
	mss := pathMTU - tcpHeaders
	metrics["path_mtu"] = pathMTU
	metrics["mss"] = mss
	if finding.upper > 0 {
		metrics["limit"] = string(finding.limit.Outcome)
	}
	// --max가 interface MTU보다 작고 그 크기까지 응답했으면 경로 MTU의 하한만 안다.
	if finding.upper == 0 && !finding.incomplete && target.max < target.interfaceMTU {
		metrics["at_least"] = true
		result.Status = StatusPass
		result.Summary = T("mtu.summary.at_least", pathMTU, target.iface, target.interfaceMTU, mss)
		return result
	}
	// 시간이 모자라 멈춘 탐색의 pathMTU는 하한일 뿐이다. 그 값으로 차이 원인이나 MSS를 짚으면 틀린 진단이 된다.
	if finding.incomplete {
		upper := finding.upper
		if upper == 0 {
			upper = target.max + 1
		}
		result.Status = StatusWarn
		result.Summary = T("mtu.summary.incomplete", pathMTU, upper-1, target.iface, target.interfaceMTU)
		result.Warnings = []string{T("mtu.warning.incomplete", pathMTU, upper-1)}
		return result
	}
	gap := max(target.interfaceMTU-pathMTU, 0)
	metrics["gap"] = gap
	result.Summary = T("mtu.summary.measured", pathMTU, target.iface, target.interfaceMTU, gap, mss)
	result.Status = StatusPass
	if finding.limit.Outcome == mtuDenied || finding.limit.Outcome == mtuError {
		result.Status = StatusWarn
		result.Warnings = append(result.Warnings, T("mtu.warning.stopped", finding.upper, finding.limit.detail))
	}
	if gap > 0 {
		result.Status = StatusWarn
		switch finding.limit.Outcome {
		case mtuTooBig:
			result.Warnings = append(result.Warnings, T("mtu.warning.kernel", finding.upper))
		case mtuFragNeeded:
			result.Warnings = append(result.Warnings, T("mtu.warning.icmp", finding.limit.MTU))
		}
		result.Warnings = append(result.Warnings, mtuGapHint(gap), T("mtu.warning.mss", mss, pathMTU))
	}
	if finding.blackHole {
		result.Status = StatusWarn
		result.Warnings = append(result.Warnings, T("mtu.warning.black_hole", pathMTU))
	}
	return result
}

func plannedFloor(finding mtuFinding, target mtuTarget) int {
	if len(finding.attempts) > 0 {
		return finding.attempts[0].Size
	}
	if target.ipv6 {
		return mtuIPv6Floor
	}
	return mtuIPv4Floor
}

// mtuGapHint는 차이 크기로 흔한 원인을 짚는다. 같은 크기를 내는 원인이 여럿이므로 tunnel 종류를
// 단정하지 않고 힌트로만 쓴다.
func mtuGapHint(gap int) string {
	switch {
	case gap < 50:
		return T("mtu.hint.gap_small", gap)
	case gap <= 80:
		return T("mtu.hint.gap_tunnel", gap)
	default:
		return T("mtu.hint.gap_large", gap)
	}
}
