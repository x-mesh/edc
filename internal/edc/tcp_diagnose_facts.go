package edc

import (
	"cmp"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tcpDiagnoseProbeID = "tcp.diagnose"

	diagnoseOutcomeConnected = "connected"
	diagnoseOutcomeRefused   = "refused"
	diagnoseOutcomeTimeout   = "timeout"
	diagnoseOutcomeOther     = "other"

	diagnoseVerdictConnected    = "connected"
	diagnoseVerdictDegraded     = "degraded"
	diagnoseVerdictIntermittent = "intermittent"
	diagnoseVerdictLocalDrop    = "local_drop"
	diagnoseVerdictRefused      = "refused"
	diagnoseVerdictNoReply      = "no_reply"
	diagnoseVerdictUnreachable  = "unreachable"
	diagnoseVerdictDNS          = "dns_failed"
	diagnoseVerdictNoAttempt    = "no_attempt"

	diagnoseBasisTrace   = "trace"
	diagnoseBasisPartial = "partial"
	diagnoseBasisPhases  = "phases"

	// diagnoseSlowConnect는 tcp check가 warn으로 보는 연결 시간과 같다. 두 명령의 느림 기준이 달라지지 않게 한다.
	diagnoseSlowConnect = time.Second
)

func init() { evidenceShownProbes[tcpDiagnoseProbeID] = true }

// diagnoseAttempt는 연결 시도 하나와, trace가 그 시도의 socket에서 본 event를 센 값이다.
type diagnoseAttempt struct {
	Index          int
	Outcome        string
	Phase          string
	Cause          string
	ConnectMS      int64
	LocalPort      int
	SYNRetransmits int
	Retransmits    int
	ResetsReceived int
	ResetsSent     int
	RTTUS          *uint32
	Drops          map[string]int
	// OtherDrops는 흐름을 막지 않는 drop이다. 판정에는 쓰지 않고 JSON에만 남긴다.
	OtherDrops map[string]int
	// elapsed는 화면에 쓰는 connect 시간이다. loopback과 LAN은 1ms보다 짧아 ConnectMS로는 0으로 보인다.
	elapsed time.Duration
}

func (attempt diagnoseAttempt) dropCount() int {
	total := 0
	for _, count := range attempt.Drops {
		total += count
	}
	return total
}

// diagnoseOutcome은 tcp check가 실패 phase로 쓰는 classifyNetworkError 값을 시도 결과로 줄인다.
func diagnoseOutcome(phase string) string {
	switch phase {
	case "":
		return diagnoseOutcomeConnected
	case "connection_refused":
		return diagnoseOutcomeRefused
	case "timeout":
		return diagnoseOutcomeTimeout
	default:
		return diagnoseOutcomeOther
	}
}

// diagnoseTraceEventLimit는 run이 남기는 event 상한이다. 시도 하나가 수십 개를 만들고 count 상한이 100이므로 정상 흐름은
// 이 안에 든다. 넘치는 event는 세기만 해서 결과가 개수가 적을 수 있다고 알린다.
const diagnoseTraceEventLimit = 4096

// diagnoseTraceRun은 시도하는 동안 돈 event 수집이다. tcpSkip과 dropSkip이 비어 있으면 그 수집이 처음부터 돌았다.
// startDiagnoseTrace는 어느 OS에서나 run을 돌려주므로 nil은 오지 않는다. collector goroutine이 observe와 warn을 부르는
// 동안 main goroutine이 줄을 그리므로, 모든 field는 mu 아래에서만 읽고 쓴다.
type diagnoseTraceRun struct {
	destination string
	mu          sync.Mutex
	events      []captureEvent
	limit       int
	overflow    uint64
	ports       map[int]bool
	sockets     map[uint64]bool
	tcpSkip     string
	dropSkip    string
	warnings    []string
	lost        uint64
	stop        func()
}

// diagnoseTraceStatus는 trace 줄과 결과를 그릴 때 읽는 run의 상태다. 한 번 잠그고 모두 읽어야 한 줄 안의 값이 섞이지 않는다.
type diagnoseTraceStatus struct {
	tcpSkip  string
	dropSkip string
	warnings []string
	lost     uint64
	overflow uint64
}

func newDiagnoseTraceRun(destination string) *diagnoseTraceRun {
	return &diagnoseTraceRun{destination: destination, limit: diagnoseTraceEventLimit, ports: map[int]bool{}, sockets: map[uint64]bool{}}
}

// expectPort는 시도가 bind한 local port를 등록한다. connect 전에 불러야 그 시도의 첫 SYN event부터 남는다.
func (run *diagnoseTraceRun) expectPort(port int) {
	if port <= 0 {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	run.ports[port] = true
}

// observe는 등록한 local port로 대상 주소와 오간 event만 남긴다. collector는 host의 모든 TCP와 drop을 보내므로, 같은 대상에
// 연결한 다른 process의 흐름도 여기서 버린다. 주소가 비어 오는 event는 같은 socket의 앞 event로 알아본다.
func (run *diagnoseTraceRun) observe(event captureEvent) {
	run.mu.Lock()
	defer run.mu.Unlock()
	known := event.SocketID != 0 && run.sockets[event.SocketID]
	if !known && !run.ports[diagnoseLocalPort(event, run.destination)] {
		return
	}
	if event.SocketID != 0 && event.Protocol != "drop" {
		run.sockets[event.SocketID] = true
	}
	if len(run.events) >= run.limit {
		run.overflow++
		return
	}
	run.events = append(run.events, event)
}

func (run *diagnoseTraceRun) snapshot() []captureEvent {
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]captureEvent(nil), run.events...)
}

func (run *diagnoseTraceRun) warn(message string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.warnings = append(run.warnings, message)
}

func (run *diagnoseTraceRun) addLost(lost uint64) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.lost += lost
}

func (run *diagnoseTraceRun) setTCPSkip(reason string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.tcpSkip = reason
}

func (run *diagnoseTraceRun) setDropSkip(reason string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.dropSkip = reason
}

func (run *diagnoseTraceRun) status() diagnoseTraceStatus {
	run.mu.Lock()
	defer run.mu.Unlock()
	return diagnoseTraceStatus{
		tcpSkip: run.tcpSkip, dropSkip: run.dropSkip, warnings: append([]string(nil), run.warnings...),
		lost: run.lost, overflow: run.overflow,
	}
}

// diagnoseSkipReason은 prerequisite 오류를 한 줄로 만든다. 권한 안내는 sudo 명령을 다음 줄에 두는데, 시도 줄과 trace 줄은
// 첫 줄만 그리므로 명령이 잘린다.
func diagnoseSkipReason(err error) string {
	if err == nil {
		return T("diagnose.trace.stopped_early")
	}
	lines := strings.Split(err.Error(), "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	return strings.Join(lines, " ")
}

func (run *diagnoseTraceRun) tcpTraced() bool  { return run.status().tcpSkip == "" }
func (run *diagnoseTraceRun) dropTraced() bool { return run.status().dropSkip == "" }

func (run *diagnoseTraceRun) finish() {
	if run.stop != nil {
		run.stop()
		run.stop = nil
	}
}

// correlateDiagnoseEvents는 event를 시도에 붙인다. 시도마다 connect 전에 local port를 정해 두므로, 실패한 시도도 port로
// 고른다. 어느 시도에도 붙지 않는 event는 같은 대상에 연결한 다른 process의 것이라 세지 않는다.
func correlateDiagnoseEvents(attempts []diagnoseAttempt, events []captureEvent, destination string) []diagnoseAttempt {
	result := make([]diagnoseAttempt, len(attempts))
	byPort := map[int]int{}
	for index, attempt := range attempts {
		attempt.SYNRetransmits, attempt.Retransmits, attempt.ResetsReceived, attempt.ResetsSent = 0, 0, 0, 0
		attempt.RTTUS, attempt.Drops, attempt.OtherDrops = nil, nil, nil
		result[index] = attempt
		if attempt.LocalPort > 0 {
			byPort[attempt.LocalPort] = index
		}
	}
	ordered := append([]captureEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TimestampNS < ordered[j].TimestampNS })
	bySocket := map[uint64]int{}
	for _, event := range ordered {
		index, ok := diagnoseEventAttempt(event, destination, byPort, bySocket)
		if !ok {
			continue
		}
		if event.SocketID != 0 && event.Protocol != "drop" {
			bySocket[event.SocketID] = index
		}
		attempt := &result[index]
		switch {
		case event.Protocol == "drop":
			reason := cmp.Or(event.Reason, "UNKNOWN")
			drops := &attempt.OtherDrops
			if diagnoseBlockingDrop(reason) {
				drops = &attempt.Drops
			}
			if *drops == nil {
				*drops = map[string]int{}
			}
			(*drops)[reason]++
		case event.Event == "tcp_retransmit":
			// SYN 재전송은 상대가 SYN에 답하지 않았다는 뜻이고, 연결 뒤 재전송은 경로 품질을 뜻해 따로 센다.
			if event.OldState == "SYN_SENT" {
				attempt.SYNRetransmits++
			} else {
				attempt.Retransmits++
			}
		case event.Event == "tcp_receive_reset":
			attempt.ResetsReceived++
		case event.Event == "tcp_send_reset":
			attempt.ResetsSent++
		case event.Event == "tcp_sample" && event.TCPValid&1 != 0 && event.RTTUS != 0:
			rtt := event.RTTUS
			attempt.RTTUS = &rtt
		}
	}
	return result
}

// diagnoseEventAttempt는 event의 이 host 쪽 port로 시도를 찾는다. 나가는 packet은 source가, 들어오는 packet은 destination이
// 이 host 쪽이다. 주소 없이 오는 socket event는 같은 socket의 앞 event로 찾는다.
// diagnoseBlockingDrops는 이 host가 흐름을 막았다고 볼 drop 이유다. 나머지는 흐름이 정상이어도 생긴다. 연결을 닫을 때
// 큐를 비우는 QUEUE_PURGE와 NOT_SPECIFIED, TCP가 늦거나 틀린 segment를 버리는 TCP_*, loopback 대상이 닫힌 port에서 SYN을
// 버리는 NO_SOCKET이 그렇다. jw-server(kernel 6.17)의 loopback 연결에서 이 값들이 시도마다 나왔다.
var diagnoseBlockingDrops = map[string]bool{
	"NETFILTER_DROP": true, "BPF_CGROUP_EGRESS": true, "TC_EGRESS": true, "TC_INGRESS": true, "SECURITY_HOOK": true,
	"XFRM_POLICY": true, "IP_RPFILTER": true, "IP_OUTNOROUTES": true, "IP_INNOROUTES": true, "IPV6DISABLED": true,
	"NEIGH_CREATEFAIL": true, "NEIGH_FAILED": true, "NEIGH_QUEUEFULL": true, "NEIGH_DEAD": true, "NEIGH_HH_FILLFAIL": true,
	"QDISC_DROP": true, "QDISC_OVERLIMIT": true, "QDISC_CONGESTED": true, "FULL_RING": true, "DEV_READY": true,
	"PKT_TOO_BIG": true, "IP_INVALID_SOURCE": true, "TUNNEL_TXINFO": true, "NO_TX_TARGET": true,
}

func diagnoseBlockingDrop(reason string) bool { return diagnoseBlockingDrops[reason] }

func diagnoseEventAttempt(event captureEvent, destination string, byPort map[int]int, bySocket map[uint64]int) (int, bool) {
	if index, ok := byPort[diagnoseLocalPort(event, destination)]; ok {
		return index, true
	}
	if event.SocketID != 0 && event.Protocol != "drop" {
		index, ok := bySocket[event.SocketID]
		return index, ok
	}
	return 0, false
}

// diagnoseLocalPort는 대상과 오간 event의 이 host 쪽 port다. 대상과 무관한 event나 주소 없는 event는 0이다.
func diagnoseLocalPort(event captureEvent, destination string) int {
	switch destination {
	case event.Destination:
		return addressPort(event.Source)
	case event.Source:
		return addressPort(event.Destination)
	}
	return 0
}

func addressPort(address string) int {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

// diagnoseFacts는 판정에 쓰는 사실이다. tcpTraced와 dropTraced가 false면 그 사실은 0이 아니라 모르는 값이다.
type diagnoseFacts struct {
	attempts   []diagnoseAttempt
	tcpTraced  bool
	dropTraced bool
}

type diagnoseTotals struct {
	Connected      int
	Refused        int
	Timeouts       int
	Other          int
	SYNRetransmits int
	Retransmits    int
	ResetsReceived int
	ResetsSent     int
	Drops          map[string]int
	OtherDrops     map[string]int
	SlowConnects   int
}

func (facts diagnoseFacts) totals() diagnoseTotals {
	totals := diagnoseTotals{Drops: map[string]int{}, OtherDrops: map[string]int{}}
	for _, attempt := range facts.attempts {
		switch attempt.Outcome {
		case diagnoseOutcomeConnected:
			totals.Connected++
			if time.Duration(attempt.ConnectMS)*time.Millisecond > diagnoseSlowConnect {
				totals.SlowConnects++
			}
		case diagnoseOutcomeRefused:
			totals.Refused++
		case diagnoseOutcomeTimeout:
			totals.Timeouts++
		default:
			totals.Other++
		}
		totals.SYNRetransmits += attempt.SYNRetransmits
		totals.Retransmits += attempt.Retransmits
		totals.ResetsReceived += attempt.ResetsReceived
		totals.ResetsSent += attempt.ResetsSent
		for reason, count := range attempt.Drops {
			totals.Drops[reason] += count
		}
		for reason, count := range attempt.OtherDrops {
			totals.OtherDrops[reason] += count
		}
	}
	return totals
}

func (totals diagnoseTotals) dropCount() int {
	total := 0
	for _, count := range totals.Drops {
		total += count
	}
	return total
}

func (totals diagnoseTotals) otherDropCount() int {
	total := 0
	for _, count := range totals.OtherDrops {
		total += count
	}
	return total
}

func (facts diagnoseFacts) basis() string {
	switch {
	case facts.tcpTraced && facts.dropTraced:
		return diagnoseBasisTrace
	case facts.tcpTraced || facts.dropTraced:
		return diagnoseBasisPartial
	default:
		return diagnoseBasisPhases
	}
}

// diagnoseVerdict는 관찰 표를 순서대로 적용한다. 이 host의 drop이 가장 구체적인 원인이라 먼저 보고, 모두 연결된 경우,
// 일부만 연결된 경우, 하나도 연결되지 않은 경우의 RST, 무응답, 그 밖의 오류 순으로 본다.
func diagnoseVerdict(facts diagnoseFacts) (string, Status) {
	totals := facts.totals()
	attempts := len(facts.attempts)
	failed := func(code string) (string, Status) {
		if totals.Connected > 0 {
			return code, StatusWarn
		}
		return code, StatusFail
	}
	switch {
	case attempts == 0:
		return diagnoseVerdictNoAttempt, StatusSkip
	case totals.dropCount() > 0:
		return failed(diagnoseVerdictLocalDrop)
	case totals.Connected == attempts:
		if totals.SYNRetransmits > 0 || totals.Retransmits > 0 || totals.SlowConnects > 0 {
			return diagnoseVerdictDegraded, StatusWarn
		}
		return diagnoseVerdictConnected, StatusPass
	case totals.Connected > 0:
		return diagnoseVerdictIntermittent, StatusWarn
	case totals.Refused > 0:
		return diagnoseVerdictRefused, StatusFail
	case totals.Timeouts > 0:
		return diagnoseVerdictNoReply, StatusFail
	default:
		return diagnoseVerdictUnreachable, StatusFail
	}
}

// diagnoseTarget은 다음 명령을 만들 때 쓰는 값이다. host는 사용자가 준 이름, source는 이 host가 대상으로 보낼 때 쓰는 주소다.
type diagnoseTarget struct {
	input   string
	host    string
	port    string
	address string
	source  string
}

// diagnoseNext는 판정마다 이어서 볼 명령이다. 대상 host에서 실행할 명령은 shell 주석으로 실행할 곳을 붙여, 복사해도 그대로 돈다.
func diagnoseNext(code string, target diagnoseTarget, totals diagnoseTotals) string {
	onTarget := func(command string) string { return command + "  # " + T("diagnose.next.on_host", target.host) }
	expect := func() string {
		command := "edc tcp expect " + target.port
		if target.source != "" {
			command = "edc tcp expect --from " + target.source + " " + target.port
		}
		return onTarget(command)
	}
	switch code {
	case diagnoseVerdictConnected:
		return "edc tls check " + target.input
	case diagnoseVerdictDegraded:
		return "edc net mtu " + target.host
	case diagnoseVerdictIntermittent, diagnoseVerdictNoReply:
		return expect()
	case diagnoseVerdictLocalDrop:
		reason := topDropReason(totals.Drops)
		if reason == "NETFILTER_DROP" {
			return "sudo nft list ruleset"
		}
		return "sudo edc trace drop --reason " + reason
	case diagnoseVerdictRefused:
		return onTarget("edc listen --tcp")
	case diagnoseVerdictUnreachable:
		return "edc net trace " + target.host
	case diagnoseVerdictDNS:
		return "edc dns lookup " + target.host
	default:
		return ""
	}
}

func topDropReason(drops map[string]int) string {
	best, bestCount := "", 0
	for reason, count := range drops {
		if count > bestCount || count == bestCount && reason < best {
			best, bestCount = reason, count
		}
	}
	return best
}

// diagnoseVerdictMessage는 판정 문장이다. 사실 수치를 함께 써서 판정의 근거가 줄 안에 보이게 한다.
func diagnoseVerdictMessage(code string, facts diagnoseFacts, totals diagnoseTotals) string {
	attempts := len(facts.attempts)
	switch code {
	case diagnoseVerdictConnected:
		return T("diagnose.verdict.connected", attempts)
	case diagnoseVerdictDegraded:
		return T("diagnose.verdict.degraded", attempts, diagnoseTroubleText(totals))
	case diagnoseVerdictIntermittent:
		return T("diagnose.verdict.intermittent", totals.Connected, attempts, diagnoseFailureText(totals))
	case diagnoseVerdictLocalDrop:
		return T("diagnose.verdict.local_drop", diagnoseDropText(totals.Drops))
	case diagnoseVerdictRefused:
		return T("diagnose.verdict.refused", totals.Refused, attempts)
	case diagnoseVerdictNoReply:
		if !facts.tcpTraced || !facts.dropTraced {
			return T("diagnose.verdict.no_reply_untraced", totals.Timeouts, attempts)
		}
		return T("diagnose.verdict.no_reply", totals.Timeouts, attempts, totals.SYNRetransmits)
	case diagnoseVerdictUnreachable:
		cause := ""
		for _, attempt := range facts.attempts {
			if attempt.Cause != "" {
				cause = attempt.Cause
				break
			}
		}
		return T("diagnose.verdict.unreachable", cause)
	default:
		return T("diagnose.verdict.no_attempt")
	}
}

func diagnoseTroubleText(totals diagnoseTotals) string {
	var parts []string
	if totals.SYNRetransmits > 0 {
		parts = append(parts, T("diagnose.fact.syn_retransmits", totals.SYNRetransmits))
	}
	if totals.Retransmits > 0 {
		parts = append(parts, T("diagnose.fact.retransmits", totals.Retransmits))
	}
	if totals.SlowConnects > 0 {
		parts = append(parts, T("diagnose.fact.slow_connects", totals.SlowConnects, diagnoseSlowConnect))
	}
	return strings.Join(parts, ", ")
}

func diagnoseFailureText(totals diagnoseTotals) string {
	var parts []string
	if totals.Refused > 0 {
		parts = append(parts, T("diagnose.fact.refused", totals.Refused))
	}
	if totals.Timeouts > 0 {
		parts = append(parts, T("diagnose.fact.timeouts", totals.Timeouts))
	}
	if totals.Other > 0 {
		parts = append(parts, T("diagnose.fact.other", totals.Other))
	}
	return strings.Join(parts, ", ")
}

func diagnoseDropText(drops map[string]int) string {
	reasons := make([]string, 0, len(drops))
	for reason := range drops {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s ×%d", reason, drops[reason]))
	}
	return strings.Join(parts, ", ")
}

// diagnoseAttemptSummary는 시도 한 줄이다. 실패한 시도는 tcp check와 같은 phase를 앞에 둔다.
func diagnoseAttemptSummary(attempt diagnoseAttempt, timeout time.Duration, traced bool) string {
	elapsed := diagnoseElapsed(attempt).String()
	var parts []string
	switch attempt.Outcome {
	case diagnoseOutcomeConnected:
		parts = append(parts, T("diagnose.attempt.connected", elapsed))
	case diagnoseOutcomeRefused:
		parts = append(parts, T("diagnose.attempt.refused", elapsed))
	case diagnoseOutcomeTimeout:
		parts = append(parts, T("diagnose.attempt.timeout", timeout))
	default:
		parts = append(parts, T("diagnose.attempt.other", attempt.Phase, attempt.Cause))
	}
	if attempt.LocalPort > 0 {
		parts = append(parts, T("diagnose.attempt.local_port", attempt.LocalPort))
	}
	if traced {
		if attempt.SYNRetransmits > 0 {
			parts = append(parts, T("diagnose.fact.syn_retransmits", attempt.SYNRetransmits))
		}
		if attempt.Retransmits > 0 {
			parts = append(parts, T("diagnose.fact.retransmits", attempt.Retransmits))
		}
		if attempt.ResetsReceived > 0 {
			parts = append(parts, T("diagnose.fact.resets_received", attempt.ResetsReceived))
		}
		if attempt.RTTUS != nil {
			parts = append(parts, T("diagnose.fact.rtt", diagnoseRTT(*attempt.RTTUS)))
		}
	}
	if drops := attempt.dropCount(); drops > 0 {
		parts = append(parts, T("diagnose.fact.drops", diagnoseDropText(attempt.Drops)))
	}
	return strings.Join(parts, ", ")
}

func diagnoseElapsed(attempt diagnoseAttempt) time.Duration {
	elapsed := attempt.elapsed
	if elapsed == 0 {
		elapsed = time.Duration(attempt.ConnectMS) * time.Millisecond
	}
	if elapsed < 10*time.Millisecond {
		return elapsed.Round(10 * time.Microsecond)
	}
	return elapsed.Round(time.Millisecond)
}

func diagnoseAttemptStatus(attempt diagnoseAttempt) Status {
	switch {
	case attempt.Outcome != diagnoseOutcomeConnected:
		return StatusFail
	case attempt.SYNRetransmits > 0 || attempt.Retransmits > 0 || attempt.dropCount() > 0 ||
		time.Duration(attempt.ConnectMS)*time.Millisecond > diagnoseSlowConnect:
		return StatusWarn
	default:
		return StatusPass
	}
}

func diagnoseRTT(us uint32) string {
	return (time.Duration(us) * time.Microsecond).Round(10 * time.Microsecond).String()
}

// diagnoseMetrics는 JSON에 쓸 시도와 합계다. 추적하지 않은 값은 0이 아니라 모르는 값이므로 key를 빼서, 0으로 관찰한 값과
// 구분되게 한다.
func diagnoseMetrics(facts diagnoseFacts, totals diagnoseTotals) ([]map[string]interface{}, map[string]interface{}) {
	attempts := make([]map[string]interface{}, 0, len(facts.attempts))
	for _, attempt := range facts.attempts {
		item := map[string]interface{}{"index": attempt.Index, "outcome": attempt.Outcome, "connect_ms": attempt.ConnectMS, "local_port": attempt.LocalPort}
		if attempt.Phase != "" {
			item["phase"], item["cause"] = attempt.Phase, attempt.Cause
		}
		if facts.tcpTraced {
			item["syn_retransmits"], item["retransmits"] = attempt.SYNRetransmits, attempt.Retransmits
			item["resets_received"], item["resets_sent"] = attempt.ResetsReceived, attempt.ResetsSent
			if attempt.RTTUS != nil {
				item["rtt_us"] = *attempt.RTTUS
			}
		}
		if facts.dropTraced {
			drops := attempt.Drops
			if drops == nil {
				drops = map[string]int{}
			}
			item["drops"] = drops
			if len(attempt.OtherDrops) > 0 {
				item["other_drops"] = attempt.OtherDrops
			}
		}
		attempts = append(attempts, item)
	}
	sums := map[string]interface{}{
		"connected": totals.Connected, "refused": totals.Refused, "timeouts": totals.Timeouts, "other": totals.Other, "slow_connects": totals.SlowConnects,
	}
	if facts.tcpTraced {
		sums["syn_retransmits"], sums["retransmits"] = totals.SYNRetransmits, totals.Retransmits
		sums["resets_received"], sums["resets_sent"] = totals.ResetsReceived, totals.ResetsSent
	}
	if facts.dropTraced {
		sums["local_drops"] = totals.dropCount()
		sums["other_drops"] = totals.otherDropCount()
	}
	return attempts, sums
}
