package edc

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func diagnoseTestAttempt(index int, outcome string, port int) diagnoseAttempt {
	phases := map[string]string{diagnoseOutcomeConnected: "", diagnoseOutcomeRefused: "connection_refused", diagnoseOutcomeTimeout: "timeout", diagnoseOutcomeOther: "network"}
	return diagnoseAttempt{Index: index, Outcome: outcome, Phase: phases[outcome], LocalPort: port, ConnectMS: 3}
}

func TestDiagnoseVerdictFollowsTheObservationTable(t *testing.T) {
	connected := diagnoseTestAttempt(1, diagnoseOutcomeConnected, 40001)
	refused := diagnoseTestAttempt(1, diagnoseOutcomeRefused, 40001)
	timeout := diagnoseTestAttempt(1, diagnoseOutcomeTimeout, 40001)
	timeout.SYNRetransmits = 3
	unreachable := diagnoseTestAttempt(1, diagnoseOutcomeOther, 40001)
	dropped := timeout
	dropped.Drops = map[string]int{"NETFILTER_DROP": 4}
	retransmitted := connected
	retransmitted.SYNRetransmits = 1
	slow := connected
	slow.ConnectMS = 1500
	tests := []struct {
		name     string
		attempts []diagnoseAttempt
		code     string
		status   Status
	}{
		{"SYN retransmits only, no handshake, no local drop", []diagnoseAttempt{timeout, timeout}, diagnoseVerdictNoReply, StatusFail},
		{"connection refused right away", []diagnoseAttempt{refused, refused}, diagnoseVerdictRefused, StatusFail},
		{"local kernel drop for this flow", []diagnoseAttempt{dropped, timeout}, diagnoseVerdictLocalDrop, StatusFail},
		{"local drop while some attempts connect", []diagnoseAttempt{connected, dropped}, diagnoseVerdictLocalDrop, StatusWarn},
		{"connects but retransmits", []diagnoseAttempt{connected, retransmitted}, diagnoseVerdictDegraded, StatusWarn},
		{"connects but latency is high", []diagnoseAttempt{slow, connected}, diagnoseVerdictDegraded, StatusWarn},
		{"all connect", []diagnoseAttempt{connected, connected}, diagnoseVerdictConnected, StatusPass},
		{"some connect and some fail", []diagnoseAttempt{connected, timeout}, diagnoseVerdictIntermittent, StatusWarn},
		{"refused beats no answer when none connect", []diagnoseAttempt{refused, timeout}, diagnoseVerdictRefused, StatusFail},
		{"other network errors", []diagnoseAttempt{unreachable}, diagnoseVerdictUnreachable, StatusFail},
		{"no attempt finished", nil, diagnoseVerdictNoAttempt, StatusSkip},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, status := diagnoseVerdict(diagnoseFacts{attempts: test.attempts, tcpTraced: true, dropTraced: true})
			if code != test.code || status != test.status {
				t.Fatalf("verdict = %s %s, want %s %s", code, status, test.code, test.status)
			}
		})
	}
}

// trace가 없으면 같은 phase에서 같은 판정을 하되, 문장이 이 host의 drop을 배제하지 못했다고 알린다.
func TestDiagnoseVerdictWithoutTraceUsesPhasesOnly(t *testing.T) {
	timeout := diagnoseTestAttempt(1, diagnoseOutcomeTimeout, 40001)
	traced := diagnoseFacts{attempts: []diagnoseAttempt{timeout}, tcpTraced: true, dropTraced: true}
	untraced := diagnoseFacts{attempts: []diagnoseAttempt{timeout}}
	tracedCode, _ := diagnoseVerdict(traced)
	untracedCode, status := diagnoseVerdict(untraced)
	if tracedCode != untracedCode || untracedCode != diagnoseVerdictNoReply || status != StatusFail {
		t.Fatalf("untraced verdict = %s %s, traced = %s", untracedCode, status, tracedCode)
	}
	if untraced.basis() != diagnoseBasisPhases || traced.basis() != diagnoseBasisTrace {
		t.Fatalf("basis = %s and %s", untraced.basis(), traced.basis())
	}
	if (diagnoseFacts{tcpTraced: true}).basis() != diagnoseBasisPartial {
		t.Fatal("one collector should give a partial basis")
	}
	tracedMessage := diagnoseVerdictMessage(tracedCode, traced, traced.totals())
	untracedMessage := diagnoseVerdictMessage(untracedCode, untraced, untraced.totals())
	if tracedMessage == untracedMessage {
		t.Fatalf("the untraced message should differ from the traced one: %q", untracedMessage)
	}
}

func TestCorrelateDiagnoseEventsByLocalPort(t *testing.T) {
	const destination = "10.0.0.9:443"
	attempts := []diagnoseAttempt{diagnoseTestAttempt(1, diagnoseOutcomeTimeout, 40001), diagnoseTestAttempt(2, diagnoseOutcomeConnected, 40002)}
	events := []captureEvent{
		{SocketID: 1, TimestampNS: 1, Event: "tcp_state", Protocol: "tcp", Source: "10.0.0.5:40001", Destination: destination, OldState: "CLOSE", NewState: "SYN_SENT"},
		{SocketID: 1, TimestampNS: 2, Event: "tcp_retransmit", Protocol: "tcp", Source: "10.0.0.5:40001", Destination: destination, OldState: "SYN_SENT"},
		{SocketID: 1, TimestampNS: 3, Event: "tcp_retransmit", Protocol: "tcp", Source: "10.0.0.5:40001", Destination: destination, OldState: "SYN_SENT"},
		// 주소 없이 온 event는 같은 socket의 앞 event로 시도를 찾는다.
		{SocketID: 1, TimestampNS: 4, Event: "tcp_destroy", Protocol: "tcp"},
		{TimestampNS: 5, Event: "drop_netfilter_drop", Protocol: "drop", Reason: "NETFILTER_DROP", Source: "10.0.0.5:40001", Destination: destination},
		{SocketID: 2, TimestampNS: 6, Event: "tcp_connect", Protocol: "tcp", Source: "10.0.0.5:40002", Destination: destination, OldState: "SYN_SENT", NewState: "ESTABLISHED"},
		{SocketID: 2, TimestampNS: 7, Event: "tcp_sample", Protocol: "tcp", Source: "10.0.0.5:40002", Destination: destination, TCPValid: 1, RTTUS: 850},
		{SocketID: 2, TimestampNS: 8, Event: "tcp_retransmit", Protocol: "tcp", Source: "10.0.0.5:40002", Destination: destination, OldState: "FIN_WAIT1"},
		// 들어오는 packet의 drop은 destination이 이 host 쪽이다.
		{TimestampNS: 9, Event: "drop_tcp_invalid_sequence", Protocol: "drop", Reason: "TCP_INVALID_SEQUENCE", Source: destination, Destination: "10.0.0.5:40002"},
		{SocketID: 2, TimestampNS: 10, Event: "tcp_receive_reset", Protocol: "tcp", Source: "10.0.0.5:40002", Destination: destination, OldState: "ESTABLISHED"},
		// 같은 대상에 연결한 다른 process의 socket은 세지 않는다.
		{SocketID: 3, TimestampNS: 11, Event: "tcp_retransmit", Protocol: "tcp", Source: "10.0.0.5:50000", Destination: destination, OldState: "SYN_SENT"},
		{TimestampNS: 12, Event: "drop_netfilter_drop", Protocol: "drop", Reason: "NETFILTER_DROP", Source: "10.0.0.5:50000", Destination: destination},
	}
	got := correlateDiagnoseEvents(attempts, events, destination)
	first, second := got[0], got[1]
	if first.SYNRetransmits != 2 || first.Retransmits != 0 || first.Drops["NETFILTER_DROP"] != 1 || first.RTTUS != nil {
		t.Fatalf("first attempt = %+v", first)
	}
	if second.SYNRetransmits != 0 || second.Retransmits != 1 || second.ResetsReceived != 1 || second.RTTUS == nil || *second.RTTUS != 850 || second.OtherDrops["TCP_INVALID_SEQUENCE"] != 1 || len(second.Drops) != 0 {
		t.Fatalf("second attempt = %+v", second)
	}
	totals := diagnoseFacts{attempts: got}.totals()
	if totals.SYNRetransmits != 2 || totals.dropCount() != 1 || totals.otherDropCount() != 1 {
		t.Fatalf("totals = %+v", totals)
	}
	// 다시 붙여도 앞 결과를 더하지 않는다. 시도 줄마다 그때까지의 event로 다시 계산하기 때문이다.
	again := correlateDiagnoseEvents(got, events, destination)
	if again[0].SYNRetransmits != 2 || again[0].Drops["NETFILTER_DROP"] != 1 {
		t.Fatalf("correlating twice = %+v", again[0])
	}
}

// 등록한 local port의 흐름만 남는다. 같은 대상에 연결한 다른 process의 흐름은 시도에 붙지 않으므로 처음부터 버린다.
func TestDiagnoseTraceRunKeepsOnlyTheAttemptFlows(t *testing.T) {
	run := newDiagnoseTraceRun("10.0.0.9:443")
	run.expectPort(40001)
	run.observe(captureEvent{SocketID: 7, Event: "tcp_state", Source: "10.0.0.5:40001", Destination: "10.0.0.9:443"})
	run.observe(captureEvent{SocketID: 7, Event: "tcp_destroy"})
	run.observe(captureEvent{SocketID: 8, Event: "tcp_state", Source: "10.0.0.5:40002", Destination: "10.0.0.9:80"})
	run.observe(captureEvent{SocketID: 8, Event: "tcp_destroy"})
	run.observe(captureEvent{Protocol: "drop", Source: "10.0.0.9:443", Destination: "10.0.0.5:40001"})
	run.observe(captureEvent{Protocol: "drop", Source: "127.0.0.1:5000", Destination: "127.0.0.1:6000"})
	// 같은 대상이라도 등록하지 않은 port의 event는 다른 process의 것이다.
	run.observe(captureEvent{SocketID: 9, Event: "tcp_retransmit", Source: "10.0.0.5:50000", Destination: "10.0.0.9:443"})
	run.observe(captureEvent{SocketID: 9, Event: "tcp_destroy"})
	run.observe(captureEvent{Protocol: "drop", Source: "10.0.0.5:50000", Destination: "10.0.0.9:443"})
	if got := len(run.snapshot()); got != 3 {
		t.Fatalf("kept %d events, want 3: %+v", got, run.snapshot())
	}
	if status := run.status(); status.overflow != 0 {
		t.Fatalf("filtered events counted as overflow: %d", status.overflow)
	}
}

func TestDiagnoseConnectedNextChecksOnlyTheServiceThePortImplies(t *testing.T) {
	for port, want := range map[string]string{
		"443":  "edc tls check example.com:443",
		"8443": "edc tls check example.com:8443",
		"993":  "edc tls check example.com:993",
		"80":   "edc http check http://example.com:80",
		"8080": "edc http check http://example.com:8080",
		"22":   "",
		"5432": "",
		"25":   "",
	} {
		target := diagnoseTarget{input: "example.com:" + port, host: "example.com", port: port}
		if got := diagnoseNext(diagnoseVerdictConnected, target, diagnoseTotals{}); got != want {
			t.Errorf("port %s next = %q, want %q", port, got, want)
		}
	}
}

func TestJoinChecklistForAWorkingPathSkipsTLSOnAPlainPort(t *testing.T) {
	ssh := joinChecklist(joinSegmentWorks, joinClient{Target: "10.0.0.9:22", Port: "22"}, joinTarget{Port: 22})
	if len(ssh) != 1 || ssh[0] != T("join.check.application") {
		t.Fatalf("port 22 checklist = %q", ssh)
	}
	https := joinChecklist(joinSegmentWorks, joinClient{Target: "10.0.0.9:443", Port: "443"}, joinTarget{Port: 443})
	if len(https) != 2 || https[0] != "edc tls check 10.0.0.9:443" {
		t.Fatalf("port 443 checklist = %q", https)
	}
}

func TestDiagnoseNextNamesTheCommandAndWhereToRunIt(t *testing.T) {
	target := diagnoseTarget{input: "db.internal:5432", host: "db.internal", port: "5432", address: "10.0.0.9:5432", source: "10.0.0.5"}
	tests := map[string]string{
		diagnoseVerdictDegraded:     "edc net mtu db.internal",
		diagnoseVerdictNoReply:      "edc tcp expect --from 10.0.0.5 5432  # ",
		diagnoseVerdictIntermittent: "edc tcp expect --from 10.0.0.5 5432  # ",
		diagnoseVerdictRefused:      "edc listen --tcp  # ",
		diagnoseVerdictUnreachable:  "edc net trace db.internal",
		diagnoseVerdictDNS:          "edc dns lookup db.internal",
	}
	for code, want := range tests {
		got := diagnoseNext(code, target, diagnoseTotals{})
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s next = %q, want prefix %q", code, got, want)
		}
		if strings.Contains(want, "#") && !strings.Contains(got, "db.internal") {
			t.Errorf("%s next = %q, want the host to run it on", code, got)
		}
	}
	if got := diagnoseNext(diagnoseVerdictLocalDrop, target, diagnoseTotals{Drops: map[string]int{"NETFILTER_DROP": 2, "NO_SOCKET": 1}}); got != "sudo nft list ruleset" {
		t.Errorf("netfilter next = %q", got)
	}
	if got := diagnoseNext(diagnoseVerdictLocalDrop, target, diagnoseTotals{Drops: map[string]int{"XFRM_POLICY": 2}}); got != "sudo edc trace drop --reason XFRM_POLICY" {
		t.Errorf("drop next = %q", got)
	}
	untraced := target
	untraced.source = ""
	if got := diagnoseNext(diagnoseVerdictNoReply, untraced, diagnoseTotals{}); !strings.HasPrefix(got, "edc tcp expect 5432  # ") {
		t.Errorf("next without a source = %q", got)
	}
}

func TestTCPDiagnoseOptionRanges(t *testing.T) {
	valid := tcpDiagnoseOptions{common: commonOptions{timeout: time.Second}, count: 5, interval: time.Second}
	if err := valid.check(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*tcpDiagnoseOptions){
		"zero count":        func(options *tcpDiagnoseOptions) { options.count = 0 },
		"count over max":    func(options *tcpDiagnoseOptions) { options.count = tcpDiagnoseMaxCount + 1 },
		"negative interval": func(options *tcpDiagnoseOptions) { options.interval = -time.Second },
		"interval over max": func(options *tcpDiagnoseOptions) { options.interval = tcpDiagnoseMaxInterval + time.Second },
		"zero timeout":      func(options *tcpDiagnoseOptions) { options.common.timeout = 0 },
	} {
		options := valid
		change(&options)
		if options.check() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// 연결을 닫을 때와 loopback 대상의 닫힌 port에서 나오는 drop은 흐름을 막은 것이 아니므로 판정을 바꾸지 않는다.
func TestDiagnoseIgnoresDropsOfANormalFlow(t *testing.T) {
	const destination = "127.0.0.1:2150"
	attempts := []diagnoseAttempt{diagnoseTestAttempt(1, diagnoseOutcomeRefused, 40001), diagnoseTestAttempt(2, diagnoseOutcomeConnected, 40002)}
	events := []captureEvent{
		{TimestampNS: 1, Protocol: "drop", Reason: "NO_SOCKET", Source: "127.0.0.1:40001", Destination: destination},
		{TimestampNS: 2, Protocol: "drop", Reason: "QUEUE_PURGE", Source: "127.0.0.1:40002", Destination: destination},
		{TimestampNS: 3, Protocol: "drop", Reason: "NOT_SPECIFIED", Source: destination, Destination: "127.0.0.1:40002"},
	}
	facts := diagnoseFacts{attempts: correlateDiagnoseEvents(attempts, events, destination), tcpTraced: true, dropTraced: true}
	if code, _ := diagnoseVerdict(facts); code != diagnoseVerdictIntermittent {
		t.Fatalf("verdict = %s, want %s", code, diagnoseVerdictIntermittent)
	}
	if totals := facts.totals(); totals.dropCount() != 0 || totals.otherDropCount() != 3 {
		t.Fatalf("totals = %+v", totals)
	}
}

// 추적하지 않은 값은 JSON에서 빠져야 0으로 관찰한 값과 구분된다.
func TestDiagnoseMetricsOmitWhatWasNotTraced(t *testing.T) {
	attempt := diagnoseTestAttempt(1, diagnoseOutcomeTimeout, 40001)
	facts := diagnoseFacts{attempts: []diagnoseAttempt{attempt}}
	attempts, totals := diagnoseMetrics(facts, facts.totals())
	for _, key := range []string{"syn_retransmits", "drops"} {
		if _, ok := attempts[0][key]; ok {
			t.Errorf("untraced attempt has %s", key)
		}
	}
	if _, ok := totals["local_drops"]; ok {
		t.Error("untraced totals have local_drops")
	}
	facts.tcpTraced, facts.dropTraced = true, true
	attempts, totals = diagnoseMetrics(facts, facts.totals())
	if attempts[0]["syn_retransmits"] != 0 || totals["local_drops"] != 0 {
		t.Errorf("traced metrics = %v, %v", attempts[0], totals)
	}
}

// 실패한 connect에도 local port가 있어야 SYN 재전송과 drop을 그 시도에 붙일 수 있다.
func TestDiagnoseDialKnowsTheLocalPortOfAFailedConnect(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	trace := newDiagnoseTraceRun(address)
	attempt := diagnoseDial(context.Background(), address, time.Second, 1, trace)
	if attempt.Outcome != diagnoseOutcomeRefused || attempt.Phase != "connection_refused" || attempt.LocalPort == 0 {
		t.Fatalf("attempt = %+v", attempt)
	}
	// 시도가 bind한 port는 connect 전에 trace에 등록되어야 그 port의 event가 남는다.
	trace.observe(captureEvent{SocketID: 1, Event: "tcp_state", Source: net.JoinHostPort("127.0.0.1", strconv.Itoa(attempt.LocalPort)), Destination: address})
	if got := len(trace.snapshot()); got != 1 {
		t.Fatalf("kept %d events for the dialed port, want 1", got)
	}
}

// 상한을 넘은 event는 세기만 하고 남기지 않아서, 오래 도는 진단에서도 memory가 host의 흐름만큼 늘지 않는다.
func TestDiagnoseTraceRunCapsRetainedEvents(t *testing.T) {
	run := newDiagnoseTraceRun("10.0.0.9:443")
	run.limit = 2
	run.expectPort(40001)
	for stamp := uint64(1); stamp <= 5; stamp++ {
		run.observe(captureEvent{SocketID: 7, TimestampNS: stamp, Event: "tcp_retransmit", Source: "10.0.0.5:40001", Destination: "10.0.0.9:443"})
	}
	status := run.status()
	if got := len(run.snapshot()); got != 2 || status.overflow != 3 {
		t.Fatalf("kept %d events with overflow %d, want 2 and 3", got, status.overflow)
	}
	row := diagnoseTraceRow(status, diagnoseTotals{})
	if row.Status != StatusWarn {
		t.Fatalf("trace row with overflow = %s, want %s", row.Status, StatusWarn)
	}
}

// skip 이유는 collector가 멈추면 뒤늦게 바뀌므로, 줄을 그리는 goroutine과 같은 mutex 아래에서 읽고 써야 한다.
func TestDiagnoseTraceRunGuardsSkipReasons(t *testing.T) {
	run := newDiagnoseTraceRun("10.0.0.9:443")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < 1000; index++ {
			run.setTCPSkip("stopped")
			run.setDropSkip("")
			run.warn("late")
		}
	}()
	for index := 0; index < 1000; index++ {
		_ = run.tcpTraced()
		_ = run.dropTraced()
		_ = diagnoseTraceRow(run.status(), diagnoseTotals{})
	}
	<-done
	status := run.status()
	if status.tcpSkip != "stopped" || status.dropSkip != "" || len(status.warnings) != 1000 {
		t.Fatalf("status = %+v", status)
	}
	if run.tcpTraced() || !run.dropTraced() {
		t.Fatal("tcpTraced and dropTraced should follow the skip reasons")
	}
}

// --count와 --interval은 diagnose에만 있는 값이다. tcp check에 주면 flag 오류 대신 diagnose로 넘긴다.
func TestTCPCheckCountOrIntervalImpliesDiagnose(t *testing.T) {
	for _, args := range [][]string{{"host:80", "--count", "10"}, {"--count=3", "host:80"}, {"-interval", "2s", "host:80"}} {
		if !hasValueFlag(args, "count") && !hasValueFlag(args, "interval") {
			t.Errorf("%v: not detected", args)
		}
	}
	for _, args := range [][]string{{"host:80"}, {"--", "--count", "3"}, {"---count", "3"}, {"--counter", "3"}} {
		if hasValueFlag(args, "count") {
			t.Errorf("%v: detected", args)
		}
	}
}
