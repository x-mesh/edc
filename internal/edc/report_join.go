package edc

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// join의 구간 코드다. client와 대상의 판정을 맞춰 packet이 사라진 구간 하나를 이름 짓는다.
const (
	joinSegmentBetweenHosts = "between_hosts"
	joinSegmentReturnPath   = "return_path"
	joinSegmentTargetHost   = "target_host"
	joinSegmentClientHost   = "client_host"
	joinSegmentPathQuality  = "path_quality"
	joinSegmentWorks        = "works"
	joinSegmentInconsistent = "inconsistent"
	joinSegmentUndetermined = "undetermined"
)

const (
	reportJoinUsage = "edc report join [--json <path|->] [--redact] <client.json> <target.json>"
	// joinLabelWidth는 client, target, time 이름표 열의 폭이다.
	joinLabelWidth = 8
)

// joinClient는 tcp.diagnose 결과에서 join이 읽는 값이다. dns_failed처럼 route 전에 끝난 결과는 input과 verdict만 있으므로
// 나머지는 비어 있을 수 있다.
type joinClient struct {
	Verdict  string         `json:"verdict"`
	Basis    string         `json:"basis,omitempty"`
	Source   string         `json:"source,omitempty"`
	Target   string         `json:"target,omitempty"`
	Host     string         `json:"host,omitempty"`
	Port     string         `json:"port,omitempty"`
	Attempts int            `json:"attempts"`
	Totals   joinTotals     `json:"totals"`
	Drops    map[string]int `json:"drops,omitempty"`
	Trace    string         `json:"trace,omitempty"`
	// ConnectMS와 RTTUS는 연결된 시도의 connect 시간과 RTT다. 화면의 지연 줄에만 쓰고, join JSON의 키는 바꾸지 않는다.
	ConnectMS []int `json:"-"`
	RTTUS     []int `json:"-"`
}

type joinTotals struct {
	Connected      int `json:"connected"`
	Refused        int `json:"refused"`
	Timeouts       int `json:"timeouts"`
	Other          int `json:"other"`
	SlowConnects   int `json:"slow_connects"`
	SYNRetransmits int `json:"syn_retransmits"`
	Retransmits    int `json:"retransmits"`
}

// joinTarget은 tcp.expect 결과에서 join이 읽는 값이다. Source는 client 출발지와 맞는 줄 하나이고, 맞는 줄이 없으면
// SourceMatched가 false다. 그때 Verdict는 대상이 다른 출발지만 봤으면 no_syn이고, 아무 출발지도 못 봤으면 전체 판정이다.
type joinTarget struct {
	Verdict           string            `json:"verdict"`
	OverallVerdict    string            `json:"overall_verdict"`
	Port              int               `json:"port"`
	From              string            `json:"from,omitempty"`
	ObservationStatus string            `json:"observation_status,omitempty"`
	SourceMatched     bool              `json:"source_matched"`
	Source            string            `json:"source,omitempty"`
	Local             string            `json:"local,omitempty"`
	SourceInterface   string            `json:"source_interface,omitempty"`
	Counts            expectCounts      `json:"counts"`
	DropReasons       map[string]int    `json:"drop_reasons,omitempty"`
	Return            *expectReturnPath `json:"return,omitempty"`
	SourceCount       int               `json:"source_count"`
	// Listener는 대상이 시작할 때 본 port의 listener다. temporary면 tcp expect --listen의 임시 listener가 답했다.
	Listener *expectListener `json:"listener,omitempty"`
}

type reportJoinInput struct {
	Path       string    `json:"path"`
	Probe      string    `json:"probe"`
	Hostname   string    `json:"hostname,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

type reportJoinInputs struct {
	Client reportJoinInput `json:"client"`
	Target reportJoinInput `json:"target"`
}

// reportJoinTime은 두 실행의 시간 관계다. Skew는 대상 시작 시각에서 client 시작 시각을 뺀 값이다.
type reportJoinTime struct {
	Overlap bool   `json:"overlap"`
	Skew    string `json:"skew"`
	Gap     string `json:"gap,omitempty"`
}

type reportJoin struct {
	SchemaVersion string           `json:"schema_version"`
	Inputs        reportJoinInputs `json:"inputs"`
	Segment       string           `json:"segment"`
	Status        Status           `json:"status"`
	Summary       string           `json:"summary"`
	Warnings      []string         `json:"warnings"`
	Checklist     []string         `json:"checklist"`
	Client        joinClient       `json:"client"`
	Target        joinTarget       `json:"target"`
	Time          reportJoinTime   `json:"time"`
}

func runReportJoin(args []string) int {
	set := newFlagSet("report join")
	set.SetOutput(os.Stderr)
	jsonPath := set.String("json", configuredString(activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	redact := set.Bool("redact", false, T("option.redact"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 2 {
		fmt.Fprintln(os.Stderr, T("cli.usage", reportJoinUsage))
		return 2
	}
	reports := make([]savedReport, 0, 2)
	for _, path := range set.Args() {
		report, err := loadReport(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		reports = append(reports, savedReport{path: path, report: report})
	}
	client, target, err := pairJoinReports(reports[0], reports[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, T("cli.usage", reportJoinUsage))
		return 2
	}
	join := joinReports(client, target)
	if *redact {
		join = redactJoin(join, client.report, target.report)
	}
	if *jsonPath != "" {
		if err := writeJSONOutput(*jsonPath, join); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		printReportJoin(os.Stdout, join)
	}
	if join.Segment == joinSegmentWorks {
		return 0
	}
	return 1
}

// pairJoinReports는 어느 파일이 client이고 어느 파일이 대상인지 probe id로 정한다. 순서는 상관없다.
func pairJoinReports(first, second savedReport) (client, target savedReport, err error) {
	probeOf := func(saved savedReport) string {
		for _, result := range saved.report.Results {
			if result.Probe == tcpDiagnoseProbeID || result.Probe == expectProbeID {
				return result.Probe
			}
		}
		return ""
	}
	firstProbe, secondProbe := probeOf(first), probeOf(second)
	switch {
	case firstProbe == tcpDiagnoseProbeID && secondProbe == expectProbeID:
		return first, second, nil
	case firstProbe == expectProbeID && secondProbe == tcpDiagnoseProbeID:
		return second, first, nil
	}
	describe := func(probe string) string {
		if probe == "" {
			return T("join.error.no_probe")
		}
		return probe
	}
	return savedReport{}, savedReport{}, fmt.Errorf("%s", T("join.error.not_pair", reportIdentityValue(first.path), describe(firstProbe), reportIdentityValue(second.path), describe(secondProbe)))
}

func joinFindResult(report Report, probe string) Result {
	for _, result := range report.Results {
		if result.Probe == probe {
			return result
		}
	}
	return Result{}
}

// joinReports는 두 report를 맞춰 구간 하나를 판정한다.
func joinReports(clientSaved, targetSaved savedReport) reportJoin {
	clientResult := joinFindResult(clientSaved.report, tcpDiagnoseProbeID)
	targetResult := joinFindResult(targetSaved.report, expectProbeID)
	client := extractJoinClient(clientResult.Metrics)
	target, sources := extractJoinTarget(targetResult.Metrics)
	target = joinMatchSource(target, sources, client.Source, joinClientReached(client.Verdict))
	join := reportJoin{
		SchemaVersion: "1.0",
		Inputs: reportJoinInputs{
			Client: joinInput(clientSaved, clientResult),
			Target: joinInput(targetSaved, targetResult),
		},
		Client: client, Target: target, Warnings: []string{}, Checklist: []string{},
	}
	join.Time = joinTimeRelation(clientSaved.report.Run, targetSaved.report.Run)
	if !join.Time.Overlap {
		join.Warnings = append(join.Warnings, T("join.warning.no_overlap", joinWindow(clientSaved.report.Run), joinWindow(targetSaved.report.Run), join.Time.Gap))
	}
	if client.Port != "" && target.Port != 0 && client.Port != strconv.Itoa(target.Port) {
		join.Warnings = append(join.Warnings, T("join.warning.port_mismatch", client.Port, target.Port))
	}
	fromExcludes := false
	if client.Source != "" && target.From != "" && !joinPrefixContains(target.From, client.Source) {
		fromExcludes = true
		join.Warnings = append(join.Warnings, T("join.warning.from_excludes", target.From, client.Source))
	}
	if client.Source != "" && !target.SourceMatched && len(sources) > 0 {
		join.Warnings = append(join.Warnings, T("join.warning.source_missing", client.Source, strings.Join(joinSourceAddresses(sources), ", ")))
	}
	segment, reasonKey := joinSegment(client.Verdict, client.Basis, target.Verdict)
	// --from이 client 출발지를 걸러 냈으면 대상은 그 SYN을 볼 수 없었으므로 no_syn은 증거가 아니다.
	if fromExcludes {
		segment, reasonKey = joinSegmentInconsistent, "join.reason.from_excludes"
	}
	if reasonKey != "" {
		join.Warnings = append(join.Warnings, T(reasonKey))
	}
	// 임시 listener가 답한 handshake는 경로만 증명한다. 구간 판정은 그대로 두고 서비스는 시험하지 않았다고 남긴다.
	if target.Listener != nil && target.Listener.State == expectListenerTemporary {
		switch {
		case segment == joinSegmentWorks || segment == joinSegmentPathQuality:
			join.Warnings = append(join.Warnings, T("join.warning.temporary_listener_works"))
		case target.Counts.SYNACKOut > 0 || target.Counts.Handshakes > 0:
			join.Warnings = append(join.Warnings, T("join.warning.temporary_listener"))
		default:
			// 임시 listener가 SYN-ACK를 보내지 않았으면 답한 것이 없다. 답했다고 적으면 경로를 잘못 짚게 한다.
			join.Warnings = append(join.Warnings, T("join.warning.temporary_listener_silent"))
		}
	}
	join.Segment = segment
	join.Status = joinStatus(segment)
	join.Summary = joinSummary(segment, client, target)
	join.Checklist = joinChecklist(segment, client, target)
	return join
}

func joinInput(saved savedReport, result Result) reportJoinInput {
	hostname, _ := saved.report.Host["hostname"].(string)
	return reportJoinInput{Path: saved.path, Probe: result.Probe, Hostname: hostname, StartedAt: saved.report.Run.StartedAt, DurationMS: saved.report.Run.DurationMS}
}

// joinSegment는 docs/network.md의 표다. 표에 없는 조합은 더 약한 판정이고, 그 이유 키를 함께 돌려준다.
func joinSegment(clientVerdict, clientBasis, targetVerdict string) (segment, reasonKey string) {
	phasesOnly := clientBasis == diagnoseBasisPhases
	switch clientVerdict {
	case diagnoseVerdictLocalDrop:
		return joinSegmentClientHost, ""
	case diagnoseVerdictNoReply:
		switch targetVerdict {
		case expectVerdictNoSYN:
			return joinSegmentBetweenHosts, joinPhasesReason(phasesOnly)
		case expectVerdictNoACK:
			return joinSegmentReturnPath, joinPhasesReason(phasesOnly)
		case expectVerdictNoReply, expectVerdictUnseen:
			return joinSegmentTargetHost, joinPhasesReason(phasesOnly)
		case expectVerdictRST:
			// 대상이 RST를 보냈는데 client에 닿지 않았다. SYN-ACK가 돌아오지 않는 경우와 같은 구간이다.
			return joinSegmentReturnPath, "join.reason.rst_lost"
		case expectVerdictIPsecHidden:
			return joinSegmentUndetermined, "join.reason.reply_hidden"
		case expectVerdictCompleted:
			return joinSegmentInconsistent, "join.reason.mismatch"
		}
	case diagnoseVerdictRefused:
		switch targetVerdict {
		case expectVerdictRST:
			return joinSegmentTargetHost, ""
		case expectVerdictNoSYN:
			// SYN이 대상에 닿지 않았는데 RST가 왔으면 경로 위의 장비가 거부한 것이다.
			return joinSegmentBetweenHosts, "join.reason.rst_from_path"
		default:
			return joinSegmentInconsistent, "join.reason.mismatch"
		}
	case diagnoseVerdictDegraded:
		if targetVerdict == expectVerdictCompleted {
			return joinSegmentPathQuality, ""
		}
		return joinSegmentInconsistent, "join.reason.mismatch"
	case diagnoseVerdictIntermittent:
		if targetVerdict == expectVerdictCompleted {
			return joinSegmentPathQuality, "join.reason.intermittent"
		}
		return joinSegmentInconsistent, "join.reason.mismatch"
	case diagnoseVerdictConnected:
		if targetVerdict == expectVerdictCompleted {
			return joinSegmentWorks, ""
		}
		return joinSegmentInconsistent, "join.reason.mismatch"
	case diagnoseVerdictDNS:
		return joinSegmentUndetermined, "join.reason.dns_failed"
	case diagnoseVerdictNoAttempt:
		return joinSegmentUndetermined, "join.reason.no_attempt"
	case diagnoseVerdictUnreachable:
		return joinSegmentUndetermined, "join.reason.unreachable"
	}
	return joinSegmentUndetermined, "join.reason.unknown_pair"
}

// joinPhasesReason은 client가 이벤트 추적 없이 no_reply를 냈을 때의 단서다. 추적이 없으면 client 자신의 drop을
// 배제하지 못하므로 판정이 약하다.
func joinPhasesReason(phasesOnly bool) string {
	if phasesOnly {
		return "join.reason.basis_phases"
	}
	return ""
}

func joinStatus(segment string) Status {
	switch segment {
	case joinSegmentWorks:
		return StatusPass
	case joinSegmentPathQuality, joinSegmentUndetermined, joinSegmentInconsistent:
		return StatusWarn
	default:
		return StatusFail
	}
}

func joinSummary(segment string, client joinClient, target joinTarget) string {
	source, destination := joinValue(client.Source), joinTargetAddress(client, target)
	switch segment {
	case joinSegmentBetweenHosts:
		return T("join.segment.between_hosts", source, destination)
	case joinSegmentReturnPath:
		return T("join.segment.return_path", joinValue(joinTargetIP(client, target)), source)
	case joinSegmentTargetHost:
		return T("join.segment.target_host", destination)
	case joinSegmentClientHost:
		return T("join.segment.client_host", source, joinDropList(client.Drops))
	case joinSegmentPathQuality:
		return T("join.segment.path_quality", source, destination)
	case joinSegmentWorks:
		// 주소는 머리말에 있고, 어떤 서비스를 볼지는 다음 단계가 port에 맞춰 알린다.
		return T("join.segment.works")
	case joinSegmentInconsistent:
		return T("join.segment.inconsistent", client.Verdict, target.Verdict)
	default:
		return T("join.segment.undetermined", client.Verdict, target.Verdict)
	}
}

// joinChecklist는 구간마다 장비 담당자가 볼 항목이다. 주소와 port는 두 report의 실제 값으로 채운다.
func joinChecklist(segment string, client joinClient, target joinTarget) []string {
	source, targetIP, port := joinValue(client.Source), joinValue(joinTargetIP(client, target)), joinPort(client, target)
	destination := net.JoinHostPort(targetIP, port)
	var items []string
	switch segment {
	case joinSegmentBetweenHosts:
		items = []string{
			T("join.check.selector_forward", source, destination),
			T("join.check.policy_hits", source, destination),
			T("join.check.nat_exemption", source, targetIP),
			T("join.check.forward_path", source, targetIP),
			T("join.check.target_filter", port, targetIP),
		}
	case joinSegmentReturnPath:
		items = []string{
			T("join.check.selector_reverse", targetIP, source),
			T("join.check.reverse_route", targetIP, source),
			T("join.check.client_rp_filter", source),
		}
		if target.Return != nil {
			items = append(items, T("join.check.target_return", joinReturnText(target.Return)))
		}
	case joinSegmentTargetHost:
		items = []string{
			T("join.check.listener", port),
			T("join.check.host_firewall", port, source),
			T("join.check.listen_queue", port),
		}
		if len(target.DropReasons) > 0 {
			items = append(items, T("join.check.target_drops", joinDropList(target.DropReasons)))
		}
		if target.Return != nil && target.Return.RPFilter != nil && target.Return.MatchesArrival != nil && !*target.Return.MatchesArrival {
			items = append(items, T("join.check.target_rp_filter", target.SourceInterface, joinReturnText(target.Return)))
		}
	case joinSegmentClientHost:
		items = []string{
			T("join.check.client_firewall", source, destination, joinDropList(client.Drops)),
			T("join.check.client_ruleset"),
		}
	case joinSegmentPathQuality:
		items = []string{
			T("join.check.mtu", joinValue(client.Host)),
			T("join.check.retransmits", client.Totals.SYNRetransmits, client.Totals.Retransmits, target.Counts.SYNRetransmits, target.Counts.SYNACKRetransmits),
			T("join.check.attempt_counts", client.Attempts, client.Totals.Connected, target.Counts.Attempts, target.Counts.Handshakes),
		}
	case joinSegmentWorks:
		// 서비스 점검은 client가 쓴 이름으로 한다. IP로 접속하면 가상 호스트가 다른 사이트를 주거나 인증서 이름이 맞지 않는다.
		serviceHost := client.Host
		if serviceHost == "" {
			serviceHost = targetIP
		}
		if next := diagnoseServiceNext(net.JoinHostPort(serviceHost, port), port); next != "" {
			items = append(items, next)
		}
		items = append(items, T("join.check.application"))
	case joinSegmentInconsistent:
		items = []string{
			T("join.check.same_port", joinValue(client.Port), target.Port),
			T("join.check.same_time"),
			T("join.check.same_source", source, joinValue(target.Source)),
		}
	default:
		items = []string{T("join.check.rerun")}
	}
	return items
}

func joinDropList(drops map[string]int) string {
	if len(drops) == 0 {
		return T("join.value.none")
	}
	keys := make([]string, 0, len(drops))
	for key := range drops {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, drops[key]))
	}
	return strings.Join(parts, ", ")
}

func joinReturnText(route *expectReturnPath) string {
	if route.Error != "" {
		return T("expect.route.failed", route.Error)
	}
	var parts []string
	if route.Device != "" {
		parts = append(parts, T("expect.route.device", route.Device))
	}
	if route.Via != "" {
		parts = append(parts, T("expect.route.via", route.Via))
	}
	if route.Source != "" {
		parts = append(parts, T("expect.route.source", route.Source))
	}
	if route.RPFilter != nil {
		parts = append(parts, fmt.Sprintf("rp_filter %d", route.RPFilter.Effective))
	}
	if route.IPsecPolicy {
		parts = append(parts, T("expect.route.ipsec"))
	}
	if len(parts) == 0 {
		return T("expect.route.unknown")
	}
	return strings.Join(parts, " ")
}

func joinValue(value string) string {
	if value == "" {
		return T("join.value.unknown")
	}
	return value
}

// joinTargetIP는 대상의 IP다. client가 연결한 주소를 우선하고, 없으면 대상이 SYN을 받은 주소를 쓴다.
func joinTargetIP(client joinClient, target joinTarget) string {
	if client.Target != "" {
		if host, _, err := net.SplitHostPort(client.Target); err == nil {
			return host
		}
	}
	return target.Local
}

func joinPort(client joinClient, target joinTarget) string {
	if client.Port != "" {
		return client.Port
	}
	if target.Port != 0 {
		return strconv.Itoa(target.Port)
	}
	return T("join.value.unknown")
}

func joinTargetAddress(client joinClient, target joinTarget) string {
	return net.JoinHostPort(joinValue(joinTargetIP(client, target)), joinPort(client, target))
}

// extractJoinClient는 tcp.diagnose metrics를 읽는다. 파일에서 읽은 수는 float64라 metricNumber로 맞춘다.
func extractJoinClient(metrics map[string]interface{}) joinClient {
	client := joinClient{}
	client.Verdict, _ = metrics["verdict"].(string)
	client.Basis, _ = metrics["basis"].(string)
	client.Target, _ = metrics["address"].(string)
	input, _ := metrics["input"].(string)
	if host, port, err := net.SplitHostPort(client.Target); err == nil {
		client.Host, client.Port = host, port
	}
	if host, port, err := net.SplitHostPort(input); err == nil {
		client.Host = host
		if client.Port == "" {
			client.Port = port
		}
	}
	if route, ok := metrics["route"].(map[string]interface{}); ok {
		client.Source, _ = route["source"].(string)
	}
	if attempts, ok := metrics["attempts"].([]interface{}); ok {
		client.Attempts = len(attempts)
		for _, raw := range attempts {
			attempt, ok := raw.(map[string]interface{})
			if !ok || attempt["outcome"] != diagnoseOutcomeConnected {
				continue
			}
			client.ConnectMS = append(client.ConnectMS, joinInt(attempt["connect_ms"]))
			if rtt, ok := attempt["rtt_us"]; ok {
				client.RTTUS = append(client.RTTUS, joinInt(rtt))
			}
		}
	}
	if totals, ok := metrics["totals"].(map[string]interface{}); ok {
		client.Totals = joinTotals{
			Connected: joinInt(totals["connected"]), Refused: joinInt(totals["refused"]), Timeouts: joinInt(totals["timeouts"]),
			Other: joinInt(totals["other"]), SlowConnects: joinInt(totals["slow_connects"]),
			SYNRetransmits: joinInt(totals["syn_retransmits"]), Retransmits: joinInt(totals["retransmits"]),
		}
	}
	client.Drops = joinIntMap(metrics["drops"])
	if trace, ok := metrics["trace"].(map[string]interface{}); ok {
		tcp, _ := trace["tcp"].(string)
		drop, _ := trace["drop"].(string)
		client.Trace = "tcp " + joinValue(tcp) + ", drop " + joinValue(drop)
	}
	return client
}

// extractJoinTarget은 tcp.expect metrics를 읽고, client 출발지와 맞는 줄은 joinReports가 고르도록 sources를 함께 돌려준다.
func extractJoinTarget(metrics map[string]interface{}) (joinTarget, []expectSource) {
	target := joinTarget{}
	target.OverallVerdict, _ = metrics["verdict"].(string)
	target.Verdict = target.OverallVerdict
	target.Port = joinInt(metrics["port"])
	target.From, _ = metrics["from"].(string)
	target.ObservationStatus, _ = metrics["observation_status"].(string)
	target.Counts = expectCounts{
		Attempts: joinInt(metrics["attempts"]), SYNIn: joinInt(metrics["syn_in"]), SYNRetransmits: joinInt(metrics["syn_retransmits"]),
		SYNACKOut: joinInt(metrics["syn_ack_out"]), SYNACKRetransmits: joinInt(metrics["syn_ack_retransmits"]),
		RSTOut: joinInt(metrics["rst_out"]), RSTIn: joinInt(metrics["rst_in"]), Handshakes: joinInt(metrics["handshakes"]),
	}
	var sources []expectSource
	if raw, ok := metrics["sources"]; ok {
		if data, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(data, &sources)
		}
	}
	target.SourceCount = len(sources) + joinInt(metrics["sources_omitted"])
	if raw, ok := metrics["listener"]; ok && raw != nil {
		if data, err := json.Marshal(raw); err == nil {
			var listener expectListener
			if json.Unmarshal(data, &listener) == nil && listener.State != "" {
				target.Listener = &listener
			}
		}
	}
	if route, ok := metrics["from_return"]; ok && route != nil {
		if data, err := json.Marshal(route); err == nil {
			var parsed expectReturnPath
			if json.Unmarshal(data, &parsed) == nil {
				target.Return = &parsed
			}
		}
	}
	return target, sources
}

// joinMatchSource는 client 출발지와 주소가 같은 대상 줄을 고른다. sources는 시도 수 내림차순이라 여러 interface에
// 같은 출발지가 보이면 첫 줄을 쓴다.
// joinClientReached는 client가 한 번이라도 연결된 판정이다. 이때 대상이 client 출발지를 못 봤다면 SYN이 안 온 것이 아니라
// 경로의 NAT가 출발지를 바꾼 것이므로, 다른 출발지의 결과를 그대로 쓰고 경고만 남긴다.
func joinClientReached(verdict string) bool {
	return verdict == diagnoseVerdictConnected || verdict == diagnoseVerdictDegraded || verdict == diagnoseVerdictIntermittent
}

func joinMatchSource(target joinTarget, sources []expectSource, clientSource string, clientReached bool) joinTarget {
	for _, source := range sources {
		if clientSource == "" || source.Address != clientSource {
			continue
		}
		target.SourceMatched = true
		target.Verdict = source.Verdict
		target.Source, target.Local, target.SourceInterface = source.Address, source.Local, source.Interface
		target.Counts = source.expectCounts
		target.DropReasons = source.DropReasons
		target.Return = source.Return
		return target
	}
	if len(sources) > 0 {
		lead := sources[0]
		target.Source, target.Local, target.SourceInterface = lead.Address, lead.Local, lead.Interface
		// 전체 판정은 다른 출발지의 결과다. client가 연결되지 않았다면 대상이 이 client의 SYN을 못 봤다는 것이 이 client에
		// 대한 증거다.
		if clientSource != "" && !clientReached {
			target.Verdict = expectVerdictNoSYN
		}
	}
	return target
}

func joinSourceAddresses(sources []expectSource) []string {
	var addresses []string
	for _, source := range sources {
		addresses = appendUnique(addresses, source.Address)
	}
	return addresses
}

func joinPrefixContains(prefixText, address string) bool {
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return true
	}
	if prefix, err := netip.ParsePrefix(prefixText); err == nil {
		return prefix.Contains(parsed)
	}
	if single, err := netip.ParseAddr(prefixText); err == nil {
		return single == parsed
	}
	return true
}

func joinInt(value interface{}) int {
	number, ok := metricNumber(value)
	if !ok {
		return 0
	}
	return int(number)
}

func joinIntMap(value interface{}) map[string]int {
	raw, ok := value.(map[string]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	result := make(map[string]int, len(raw))
	for key, item := range raw {
		result[key] = joinInt(item)
	}
	return result
}

// joinTimeRelation은 두 실행 시간이 겹치는지 본다. 겹치지 않으면 client의 SYN이 대상의 관측 창 밖에 있으므로 판정이 약하다.
func joinTimeRelation(client, target RunInfo) reportJoinTime {
	clientEnd := client.StartedAt.Add(time.Duration(client.DurationMS) * time.Millisecond)
	targetEnd := target.StartedAt.Add(time.Duration(target.DurationMS) * time.Millisecond)
	relation := reportJoinTime{Skew: joinSigned(target.StartedAt.Sub(client.StartedAt))}
	relation.Overlap = !client.StartedAt.After(targetEnd) && !target.StartedAt.After(clientEnd)
	if !relation.Overlap {
		gap := target.StartedAt.Sub(clientEnd)
		if gap < 0 {
			gap = client.StartedAt.Sub(targetEnd)
		}
		relation.Gap = gap.Round(time.Second).String()
	}
	return relation
}

func joinSigned(value time.Duration) string {
	rounded := value.Round(time.Second)
	if rounded >= 0 {
		return "+" + rounded.String()
	}
	return rounded.String()
}

func joinWindow(run RunInfo) string {
	end := run.StartedAt.Add(time.Duration(run.DurationMS) * time.Millisecond)
	return run.StartedAt.Local().Format(time.DateTime) + "–" + end.Local().Format("15:04:05")
}

// redactJoin은 redactReport와 같은 방식으로 주소와 host 이름을 가린다. 두 report의 host 이름과 client가 입력한 대상 이름을
// 모두 가려야 어느 쪽 파일에서도 상대 host가 드러나지 않는다.
func redactJoin(join reportJoin, client, target Report) reportJoin {
	patterns := []struct{ value, label string }{}
	for _, report := range []Report{client, target} {
		if hostname, _ := report.Host["hostname"].(string); hostname != "" {
			patterns = append(patterns, struct{ value, label string }{hostname, "host"})
		}
	}
	if join.Client.Host != "" && net.ParseIP(join.Client.Host) == nil {
		patterns = append(patterns, struct{ value, label string }{join.Client.Host, "host"})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		patterns = append(patterns, struct{ value, label string }{home, "home"})
	}
	data, _ := json.Marshal(join)
	text := string(data)
	for _, pattern := range patterns {
		text = strings.ReplaceAll(text, pattern.value, token(pattern.label, pattern.value))
	}
	text = redactIPAddresses(text)
	_ = json.Unmarshal([]byte(text), &join)
	return join
}

func printReportJoin(writer io.Writer, join reportJoin) {
	client, target := join.Client, join.Target
	// 한글 이름표는 byte 수와 표시 폭이 다르므로 표시 폭으로 열을 맞춘다.
	label := func(key string) string { return liveCell(T(key), joinLabelWidth) }
	targetName := join.Inputs.Target.Hostname
	if targetName == "" {
		targetName = client.Host
	}
	fmt.Fprintln(writer, T("join.line.headline", joinEndpoint(join.Inputs.Client.Hostname, client.Source), joinEndpoint(targetName, joinTargetIP(client, target)), joinPort(client, target)))
	// --peer의 client report는 파일이 아니라 "client"다. 파일에서 합쳤을 때만 어느 파일인지 보인다.
	if join.Inputs.Client.Path != joinPeerClientLabel {
		fmt.Fprintln(writer, T("join.line.reports", join.Inputs.Client.Path, join.Inputs.Target.Path))
	}
	fmt.Fprintln(writer)
	clientLine := T("join.line.client_counts", client.Totals.Connected, client.Attempts, client.Totals.Refused, client.Totals.Timeouts, client.Totals.SYNRetransmits, joinDropList(client.Drops))
	if client.Basis != "" {
		clientLine += "  " + T("join.basis."+client.Basis)
	}
	fmt.Fprintf(writer, "%s %s\n", label("join.label.client"), clientLine)
	arrival := T("join.line.target_overall", joinValue(target.OverallVerdict))
	if target.SourceMatched {
		arrival = T("join.line.arrived_on", joinValue(target.SourceInterface))
	}
	fmt.Fprintf(writer, "%s %s · %s\n", label("join.label.target"), arrival, T("join.line.target_counts", target.Counts.SYNIn, target.Counts.SYNACKOut, target.Counts.RSTOut, target.Counts.Handshakes, joinDropList(target.DropReasons)))
	if target.Return != nil {
		fmt.Fprintf(writer, "%s %s\n", strings.Repeat(" ", joinLabelWidth), joinReturnLine(target))
	}
	if latency := joinLatency(client); latency != "" {
		fmt.Fprintf(writer, "%s %s\n", label("join.label.latency"), latency)
	}
	// 두 실행이 겹치면 시작 시각 차이는 원격이 준비되는 데 걸린 시간일 뿐이다. 겹치지 않을 때만 판정이 약해지므로 보인다.
	if !join.Time.Overlap {
		fmt.Fprintf(writer, "%s %s\n", label("join.label.time"), T("join.line.time", join.Time.Skew, T("join.line.overlap_no", join.Time.Gap)))
	}
	fmt.Fprintf(writer, "\n%s  %s  %s\n", T("join.label.verdict"), join.Segment, join.Summary)
	for _, warning := range join.Warnings {
		fmt.Fprintf(writer, "%s %s\n", T("join.label.warning"), warning)
	}
	if len(join.Checklist) > 0 {
		heading := T("join.label.next")
		// 장비 담당자가 볼 것은 두 host 사이의 장비가 의심될 때뿐이다. 그 밖에는 host에서 사용자가 이어서 볼 항목이다.
		if join.Segment == joinSegmentBetweenHosts || join.Segment == joinSegmentReturnPath {
			heading = T("join.label.checklist")
		}
		fmt.Fprintf(writer, "\n%s\n", heading)
		for _, item := range join.Checklist {
			fmt.Fprintf(writer, "  - %s\n", item)
		}
	}
}

// joinPeerClientLabel은 --peer가 파일 없이 합칠 때 client report에 붙이는 이름이다.
const joinPeerClientLabel = "client"

// joinEndpoint는 "jw-server (100.83.200.248)"다. 이름이 없거나 주소와 같으면 주소만 쓴다.
func joinEndpoint(name, address string) string {
	switch {
	case address == "":
		return joinValue(name)
	case name == "" || name == address:
		return address
	}
	return name + " (" + address + ")"
}

// joinReturnLine은 답이 도착 interface로 나가고 IPsec policy도 없으면 그 사실만 쓴다. rp_filter 값은 경로가 갈릴 때만
// 의미가 있어, 그때 자세한 경로와 함께 보인다.
func joinReturnLine(target joinTarget) string {
	route := target.Return
	if route.Error == "" && !route.IPsecPolicy && route.MatchesArrival != nil {
		if *route.MatchesArrival {
			return T("join.line.return_same", joinValue(route.Device))
		}
		if target.SourceInterface != "" {
			device := joinValue(route.Device)
			if route.Via != "" {
				device += " " + T("expect.route.via", route.Via)
			}
			line := T("join.line.return_other", device, target.SourceInterface)
			if route.RPFilter != nil {
				line += " · " + joinRPFilterVerdict(route.RPFilter.Effective)
			}
			return line
		}
	}
	return T("expect.evidence.return") + " " + joinReturnText(route)
}

// joinRPFilterVerdict는 답이 도착 interface와 다른 곳으로 나갈 때 kernel이 SYN을 받는지 말한다. strict(1)만 버린다.
func joinRPFilterVerdict(mode int) string {
	switch mode {
	case 0:
		return T("join.rp_filter.off")
	case 1:
		return T("join.rp_filter.strict")
	case 2:
		return T("join.rp_filter.loose")
	}
	return fmt.Sprintf("rp_filter %d", mode)
}

// joinLatency는 연결된 시도의 RTT를 쓴다. RTT는 trace가 있을 때만 있으므로, 없으면 ms 단위의 connect 시간을 쓴다.
func joinLatency(client joinClient) string {
	if len(client.RTTUS) > 0 {
		low, high, average := joinSpread(client.RTTUS)
		return T("join.line.latency_rtt", joinMicros(low)+joinRangeEnd(low, high, joinMicros), joinMicros(average), len(client.RTTUS))
	}
	if len(client.ConnectMS) > 0 {
		low, high, _ := joinSpread(client.ConnectMS)
		millis := func(value int) string { return (time.Duration(value) * time.Millisecond).String() }
		return T("join.line.latency_connect", millis(low)+joinRangeEnd(low, high, millis), len(client.ConnectMS))
	}
	return ""
}

func joinSpread(values []int) (low, high, average int) {
	low, high = values[0], values[0]
	total := 0
	for _, value := range values {
		low, high, total = min(low, value), max(high, value), total+value
	}
	return low, high, total / len(values)
}

func joinRangeEnd(low, high int, format func(int) string) string {
	if low == high {
		return ""
	}
	return "–" + format(high)
}

func joinMicros(value int) string {
	return (time.Duration(value) * time.Microsecond).Round(10 * time.Microsecond).String()
}
