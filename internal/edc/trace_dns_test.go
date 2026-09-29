package edc

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/miekg/dns"
)

func dnsTestPacket(t *testing.T, message *dns.Msg, at uint64, source string) dnsPacket {
	t.Helper()
	payload, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return dnsPacket{bootTimeNS: at, pid: 10, process: "dig", sent: !message.Response, source: source, destination: "127.0.0.53:53", payload: payload}
}

func dnsTestReply(t *testing.T, question *dns.Msg, rcode int, records ...string) *dns.Msg {
	t.Helper()
	reply := new(dns.Msg)
	reply.SetRcode(question, rcode)
	for _, text := range records {
		record, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		reply.Answer = append(reply.Answer, record)
	}
	return reply
}

func TestDNSQueryTrackerMatchesAnswersToQueries(t *testing.T) {
	tracker := newDNSQueryTracker(false)
	question := new(dns.Msg)
	question.SetQuestion("Example.COM.", dns.TypeA)
	// 같은 질의를 다시 보낸 뒤 온 응답 하나가 두 질의에 답한다. 응답 시간은 처음 보낸 때부터 잰다.
	for _, at := range []uint64{1_000_000, 3_000_000} {
		event, ok := tracker.event(dnsTestPacket(t, question, at, "127.0.0.1:41000"), 0)
		if !ok || event.Event != traceDNSQueryEvent || event.Target != "example.com" || event.QueryType != "A" || event.Protocol != "dns" {
			t.Fatalf("query event = %#v, %t", event, ok)
		}
	}
	answer, ok := tracker.event(dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "example.com. 60 IN A 203.0.113.10"), 6_500_000, "127.0.0.1:41000"), 0)
	if !ok || answer.Event != "dns_noerror" || answer.LatencyMS == nil || *answer.LatencyMS != 5.5 || answer.answered != 2 || !slices.Equal(answer.Answers, []string{"203.0.113.10"}) {
		t.Fatalf("answer event = %#v, %t", answer, ok)
	}
	if tracker.size != 0 || len(tracker.pending) != 0 {
		t.Fatalf("pending after the answer = %d, %v", tracker.size, tracker.pending)
	}
	// 다른 로컬 port로 온 응답은 짝이 없으므로 응답 시간이 없다.
	other, _ := tracker.event(dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "example.com. 60 IN A 203.0.113.10"), 7_000_000, "127.0.0.1:42000"), 0)
	if other.LatencyMS != nil || other.answered != 0 {
		t.Fatalf("an answer without a query = %#v", other)
	}
}

func TestDNSAnswerEventsNameTheResult(t *testing.T) {
	question := new(dns.Msg)
	question.SetQuestion("example.com.", dns.TypeAAAA)
	for _, test := range []struct {
		rcode   int
		records []string
		want    string
	}{
		{dns.RcodeSuccess, []string{"example.com. 60 IN AAAA 2001:db8::1"}, "dns_noerror"},
		{dns.RcodeSuccess, nil, "dns_nodata"},
		{dns.RcodeNameError, nil, "dns_nxdomain"},
		{dns.RcodeServerFailure, nil, "dns_servfail"},
		{dns.RcodeRefused, nil, "dns_refused"},
	} {
		event, ok := newDNSQueryTracker(false).event(dnsTestPacket(t, dnsTestReply(t, question, test.rcode, test.records...), 1, "127.0.0.1:41000"), 0)
		if !ok || event.Event != test.want {
			t.Fatalf("rcode %d with %d answers = %q, want %q", test.rcode, len(test.records), event.Event, test.want)
		}
		if traceDNSError(event.Event) != (test.want != "dns_noerror" && test.want != "dns_nodata") {
			t.Fatalf("traceDNSError(%q) = %t", event.Event, traceDNSError(event.Event))
		}
	}
}

func TestDNSCutAnswerTakesTheNameFromTheQuery(t *testing.T) {
	tracker := newDNSQueryTracker(false)
	question := new(dns.Msg)
	question.SetQuestion("big.example.com.", dns.TypeA)
	tracker.event(dnsTestPacket(t, question, 1_000_000, "127.0.0.1:41000"), 0)
	packet := dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "big.example.com. 60 IN A 203.0.113.10"), 2_000_000, "127.0.0.1:41000")
	// BPF는 응답을 1024바이트까지만 읽는다. 뒤가 잘린 응답은 header만 믿는다.
	packet.payload = packet.payload[:traceDNSHeaderSize+4]
	event, ok := tracker.event(packet, 0)
	if !ok || event.Event != "dns_noerror" || event.Target != "big.example.com" || event.QueryType != "A" || event.LatencyMS == nil || event.Answers != nil {
		t.Fatalf("cut answer = %#v, %t", event, ok)
	}
	short := packet
	short.payload = short.payload[:traceDNSHeaderSize-1]
	if _, ok := tracker.event(short, 0); ok {
		t.Fatal("a payload shorter than the DNS header made an event")
	}
	// 이 host의 DNS 서버가 port 53 client에게 보낸 응답은 질의 hook에 걸려도 client 쪽 질의가 아니다.
	served := packet
	served.sent = true
	if _, ok := tracker.event(served, 0); ok {
		t.Fatal("an answer sent to port 53 was read as a query")
	}
}

func dnsTestEvents() []captureEvent {
	latency := func(value float64) *float64 { return &value }
	return []captureEvent{
		{Protocol: "dns", Event: traceDNSQueryEvent, Process: "dig", Target: "example.com", QueryType: "A", Destination: "127.0.0.53:53", Bytes: 40},
		{Protocol: "dns", Event: "dns_noerror", Process: "dig", Target: "example.com", QueryType: "A", Destination: "127.0.0.53:53", Answers: []string{"203.0.113.10"}, LatencyMS: latency(2), answered: 1, Bytes: 56},
		{Protocol: "dns", Event: traceDNSQueryEvent, Process: "curl", Target: "missing.invalid", QueryType: "A", Destination: "127.0.0.53:53"},
		{Protocol: "dns", Event: "dns_nxdomain", Process: "curl", Target: "missing.invalid", QueryType: "A", Destination: "127.0.0.53:53", LatencyMS: latency(6), answered: 1},
		{Protocol: "dns", Event: traceDNSQueryEvent, Process: "dig", Target: "example.com", QueryType: "A", Destination: "192.0.2.1:53"},
	}
}

func TestDNSTraceSummaryCountsNamesAndUnansweredQueries(t *testing.T) {
	summarizer := newDNSTraceSummarizer()
	for _, event := range dnsTestEvents() {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 1}, 2*time.Second).(dnsTraceReport)
	if report.Queries != 3 || report.Answers != 2 || report.Errors != 1 || report.Unanswered != 1 || *report.LatencyAvgMS != 4 || *report.LatencyMaxMS != 6 || report.Results["noerror"] != 1 || report.Results["nxdomain"] != 1 {
		t.Fatalf("report totals = %#v", report)
	}
	if len(report.Names) != 2 || report.Names[0].Name != "example.com" || report.Names[0].Queries != 2 || report.Names[0].Unanswered != 1 || !slices.Equal(report.Names[0].Servers, []string{"127.0.0.53:53", "192.0.2.1:53"}) || !slices.Equal(report.Names[0].Addresses, []string{"203.0.113.10"}) {
		t.Fatalf("report names = %#v", report.Names)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"queries":3`, `"unanswered":1`, `"latency_avg_ms":4`, `"results":{"noerror":1,"nxdomain":1}`, `"lost_events":1`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("report JSON %s does not contain %s", data, field)
		}
	}
	if again := summarizer.summarize(captureSummary{LostEvents: 1}, 2*time.Second); again.(dnsTraceReport).Unanswered != 1 {
		t.Fatal("a second summary changed the counts")
	}
}

func TestDNSGroupsCountQueriesAndKeepTCPGroupsUnchanged(t *testing.T) {
	report := summarizeTraceGroups("dns", traceGroupByTarget, dnsTestEvents(), captureSummary{}, time.Second, "", "")
	if len(report.Groups) != 2 || report.Groups[0].Group != "example.com" || report.Groups[0].DNS == nil || report.Groups[0].DNS.Queries != 2 || report.Groups[0].DNS.Unanswered != 1 || report.Groups[1].DNS.Errors != 1 {
		t.Fatalf("dns groups = %#v", report.Groups)
	}
	// event 보기는 질의와 응답을 다른 행에 둔다. 답을 받은 질의는 dns_query 행에서 응답 없음으로 세지 않는다.
	events := summarizeTraceGroups("dns", traceGroupByEvent, dnsTestEvents(), captureSummary{}, time.Second, "", "")
	for _, group := range events.Groups {
		if want := map[string]uint64{traceDNSQueryEvent: 1}[group.Group]; group.DNS.Unanswered != want {
			t.Fatalf("event view %s unanswered = %d, want %d", group.Group, group.DNS.Unanswered, want)
		}
	}
	for _, group := range summarizeTraceGroups("dns", traceGroupBySource, dnsTestEvents(), captureSummary{}, time.Second, "", "").Groups {
		if group.DNS.Queries != 3 || group.DNS.Unanswered != 1 {
			t.Fatalf("source view = %#v", group.DNS)
		}
	}
	data, _ := json.Marshal(summarizeTraceGroups("tcp", traceGroupByTarget, []captureEvent{{Protocol: "tcp", Event: "tcp_connect", Target: "example.com"}}, captureSummary{}, time.Second, "", ""))
	if strings.Contains(string(data), `"dns"`) {
		t.Fatalf("tcp group JSON has a dns field: %s", data)
	}
}

func TestDNSTraceHasNoPortView(t *testing.T) {
	if views := traceGroupViews("dns"); slices.Contains(views, traceGroupByPort) || len(views) != len(traceGroupCycle)-1 {
		t.Fatalf("dns views = %q", views)
	}
	if help := traceScreenHelp("dns"); strings.Contains(help, "p port") || !strings.Contains(help, "t target") {
		t.Fatalf("dns help = %q", help)
	}
	if help := traceScreenHelp("tcp"); help != "/ filter  tab view  s source  t target  p port  c process  e event  g scroll  enter apply  esc clear  q quit  ctrl-c stop" {
		t.Fatalf("tcp help = %q", help)
	}
	model := newTraceScreenModel("dns", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	next, _ := model.updateKey(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if next.(traceScreenModel).groupBy != traceGroupByTarget {
		t.Fatal("p switched the dns trace to the port view")
	}
	if got := nextTraceGroup(traceGroupViews("dns"), traceGroupByTarget, 1); got != traceGroupByProcess {
		t.Fatalf("tab after target = %q, want process", got)
	}
	if code := runTrace([]string{"dns", "--group-by", traceGroupByPort}); code != 2 {
		t.Fatalf("trace dns --group-by port exit = %d, want 2", code)
	}
}

func TestDNSTraceScreenShowsQueryColumns(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	model := newTraceScreenModel("dns", tcpTraceOptions{groupBy: traceGroupByTarget}, make(chan captureEvent), make(chan traceFinishedMsg), nil)
	model.events, model.width, model.height = dnsTestEvents(), 120, 10
	header := traceScreenHeader(model)
	if strings.Contains(header[0], "bps") || strings.Contains(header[2], "TX") || !strings.Contains(header[2], "QRY") || !strings.Contains(header[2], "NOANS") {
		t.Fatalf("dns group header = %q", header)
	}
	row := traceScreenRows(model)[0]
	// example.com은 질의 둘 중 하나에 응답이 없다. 1이 NOANS 제목과 같은 칸에서 끝나야 한다.
	if end := strings.Index(header[2], "NOANS") + len("NOANS"); row[end-len("    1"):end+1] != "    1 " {
		t.Fatalf("NOANS is not aligned:\n%q\n%q", header[2], row)
	}
	model.groupBy = ""
	rows := traceScreenRows(model)
	index := slices.IndexFunc(rows, func(row string) bool { return strings.Contains(row, "dns_nxdomain") })
	if index < 0 || !strings.Contains(rows[index], "missing.invalid A (127.0.0.53:53)") || !strings.Contains(rows[index], "dns_nxdomain 6.0ms") {
		t.Fatalf("dns scroll rows = %q", rows)
	}
}

func TestDNSTruncatedAnswerNamesTheTCPRetry(t *testing.T) {
	tracker := newDNSQueryTracker(false)
	question := new(dns.Msg)
	question.SetQuestion("big.example.com.", dns.TypeTXT)
	tracker.event(dnsTestPacket(t, question, 1_000_000, "127.0.0.1:41000"), 0)
	reply := dnsTestReply(t, question, dns.RcodeSuccess)
	reply.Truncated = true
	answer, ok := tracker.event(dnsTestPacket(t, reply, 2_000_000, "127.0.0.1:41000"), 0)
	if !ok || answer.Event != traceDNSTruncatedEvent || answer.LatencyMS == nil || traceDNSError(answer.Event) {
		t.Fatalf("truncated answer = %#v, %t", answer, ok)
	}
	connect := captureEvent{Protocol: "tcp", Event: "tcp_connect", PID: 10, Process: "dig", Source: "127.0.0.1:50000", Destination: "127.0.0.53:53"}
	retry, ok := tracker.tcpEvent(connect)
	if !ok || retry.Event != traceDNSTCPConnectEvent || retry.Protocol != "dns" || retry.Process != "dig" || retry.Target != "big.example.com" || retry.QueryType != "TXT" {
		t.Fatalf("tcp retry = %#v, %t", retry, ok)
	}
	// 이름은 한 번만 붙는다. 같은 process의 다음 TCP 연결은 다른 조회다.
	if again, _ := tracker.tcpEvent(connect); again.Target != "" {
		t.Fatalf("second tcp connect = %#v", again)
	}
	failed, ok := tracker.tcpEvent(captureEvent{Protocol: "tcp", Event: "tcp_close", OldState: "SYN_SENT", NewState: "CLOSE", Destination: "192.0.2.1:53"})
	if !ok || failed.Event != traceDNSTCPFailEvent || !traceDNSError(failed.Event) {
		t.Fatalf("failed tcp connect = %#v, %t", failed, ok)
	}
	for _, other := range []captureEvent{
		{Protocol: "tcp", Event: "tcp_connect", Destination: "203.0.113.10:443"},
		{Protocol: "tcp", Event: "tcp_close", OldState: "ESTABLISHED", Destination: "127.0.0.53:53"},
		{Protocol: "udp", Event: "udp_send", Destination: "127.0.0.53:53"},
	} {
		if event, ok := tracker.tcpEvent(other); ok {
			t.Fatalf("%#v became a dns event: %#v", other, event)
		}
	}
	var counts traceDNSCounts
	for _, event := range []captureEvent{answer, retry, failed} {
		counts.observe(event)
	}
	if counts.Answers != 1 || counts.TCPConnections != 2 || counts.Errors != 1 {
		t.Fatalf("counts = %#v", counts)
	}
}

// 로컬 DNS 서버는 port 53으로 질의를 받고 응답을 보낸다. 응답 시간은 서버가 질의를 읽은 때부터 응답을 보낸 때까지다.
func TestDNSServerSideMatchesAnswersToClients(t *testing.T) {
	tracker := newDNSQueryTracker(true)
	question := new(dns.Msg)
	question.SetQuestion("example.com.", dns.TypeA)
	received := dnsTestPacket(t, question, 1_000_000, "127.0.0.53:53")
	received.sent, received.process, received.destination = false, "systemd-resolve", "127.0.0.1:41000"
	query, ok := tracker.event(received, 0)
	if !ok || query.Event != traceDNSQueryEvent || query.Side != traceServerSide || query.Target != "example.com" || query.Destination != "127.0.0.1:41000" {
		t.Fatalf("server query = %#v, %t", query, ok)
	}
	sent := dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeNameError), 1_250_000, "127.0.0.53:53")
	sent.sent, sent.process, sent.destination = true, "systemd-resolve", "127.0.0.1:41000"
	answer, ok := tracker.event(sent, 0)
	if !ok || answer.Event != "dns_nxdomain" || answer.Side != traceServerSide || answer.LatencyMS == nil || *answer.LatencyMS != 0.25 || answer.answered != 1 {
		t.Fatalf("server answer = %#v, %t", answer, ok)
	}
	// 각 tracker는 자기 쪽만 기록한다. 서버 쪽 tracker는 client 질의와 port 53 TCP 연결을 버리고, client 쪽은 서버 질의를 버린다.
	if event, ok := tracker.event(dnsTestPacket(t, question, 2_000_000, "127.0.0.1:41000"), 0); ok {
		t.Fatalf("server tracker kept a client query: %#v", event)
	}
	if event, ok := tracker.tcpEvent(captureEvent{Protocol: "tcp", Event: "tcp_connect", Destination: "127.0.0.53:53"}); ok {
		t.Fatalf("server tracker kept a tcp connect: %#v", event)
	}
	if event, ok := newDNSQueryTracker(false).event(received, 0); ok {
		t.Fatalf("client tracker kept a server query: %#v", event)
	}
	// 서버가 port 53이 아닌 곳으로 받은 질의와, 서버 port가 아닌 곳에서 보낸 응답은 어느 쪽도 아니다.
	stray := received
	stray.source = "127.0.0.1:5353"
	if event, ok := tracker.event(stray, 0); ok {
		t.Fatalf("a query on port 5353 made an event: %#v", event)
	}
	summarizer := newDNSTraceSummarizer()
	groups := newTraceGroupSummarizer("dns", traceGroupByTarget)
	// side가 없는 event가 뒤에 와도 서버 쪽 요약의 side는 그대로다.
	for _, event := range []captureEvent{query, answer, {Protocol: "dns", Event: traceDNSQueryEvent, Target: "example.com"}} {
		summarizer.observe(event)
		groups.observe(event)
	}
	report := summarizer.summarize(captureSummary{}, time.Second).(dnsTraceReport)
	if report.Side != traceServerSide || report.Errors != 1 || report.Unanswered != 1 {
		t.Fatalf("server report = %#v", report)
	}
	if side := groups.report(captureSummary{}, time.Second).Side; side != traceServerSide {
		t.Fatalf("server group report side = %q", side)
	}
}

func TestTraceSideOptionIsOnlyForDNS(t *testing.T) {
	for _, args := range [][]string{{"tcp", "--side", traceServerSide}, {"dns", "--side", "both"}} {
		if code := runTrace(args); code != 2 {
			t.Fatalf("trace %q exit = %d, want 2", args, code)
		}
	}
}

// 응답 시간은 network 시간과 읽기 지연으로 나뉜다. 수신 큐 시각을 모르면 둘 다 없다.
func TestDNSLatencySplitsNetworkAndReadDelay(t *testing.T) {
	approx := func(value *float64, want float64) bool {
		return value != nil && *value > want-1e-9 && *value < want+1e-9
	}
	tracker := newDNSQueryTracker(false)
	question := new(dns.Msg)
	question.SetQuestion("example.com.", dns.TypeA)
	tracker.event(dnsTestPacket(t, question, 1_000_000, "127.0.0.1:41000"), 0)
	answer := dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "example.com. 60 IN A 203.0.113.10"), 3_500_000, "127.0.0.1:41000")
	answer.arrivalNS = 3_000_000
	event, _ := tracker.event(answer, 0)
	if !approx(event.LatencyMS, 2.5) || !approx(event.NetworkMS, 2) || !approx(event.ReadDelayMS, 0.5) {
		t.Fatalf("client answer = latency %v, network %v, read delay %v", event.LatencyMS, event.NetworkMS, event.ReadDelayMS)
	}
	unknown := answer
	unknown.arrivalNS = 0
	if event, _ := tracker.event(unknown, 0); event.NetworkMS != nil || event.ReadDelayMS != nil {
		t.Fatalf("answer without an arrival time = %#v", event)
	}

	server := newDNSQueryTracker(true)
	received := dnsTestPacket(t, question, 1_200_000, "127.0.0.53:53")
	received.sent, received.destination, received.arrivalNS = false, "127.0.0.1:41000", 1_000_000
	query, _ := server.event(received, 0)
	sent := dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "example.com. 60 IN A 203.0.113.10"), 2_200_000, "127.0.0.53:53")
	sent.sent, sent.destination = true, "127.0.0.1:41000"
	reply, _ := server.event(sent, 0)
	if !approx(query.ReadDelayMS, 0.2) || query.NetworkMS != nil || !approx(reply.LatencyMS, 1) || reply.NetworkMS != nil || reply.ReadDelayMS != nil {
		t.Fatalf("server query read delay %v, answer latency %v network %v", query.ReadDelayMS, reply.LatencyMS, reply.NetworkMS)
	}

	var counts traceDNSCounts
	for _, event := range []captureEvent{event, query, reply} {
		counts.observe(event)
	}
	finished := counts.finished()
	if !approx(finished.NetworkAvgMS, 2) || !approx(finished.ReadDelayAvgMS, 0.35) || !approx(finished.ReadDelayMaxMS, 0.5) || !approx(finished.LatencyMaxMS, 2.5) {
		t.Fatalf("counts = network %v, read delay %v max %v, latency max %v", finished.NetworkAvgMS, finished.ReadDelayAvgMS, finished.ReadDelayMaxMS, finished.LatencyMaxMS)
	}
}

// 서버가 잘린 응답을 보낸 client가 TCP로 다시 연결하면, 서버 쪽 accept event에 그 질의의 이름을 붙인다.
func TestDNSServerSideTCPAcceptFollowsATruncatedAnswer(t *testing.T) {
	tracker := newDNSQueryTracker(true)
	question := new(dns.Msg)
	question.SetQuestion("big.example.com.", dns.TypeTXT)
	received := dnsTestPacket(t, question, 1_000_000, "127.0.0.53:53")
	received.sent, received.destination, received.pid = false, "127.0.0.1:41000", 50
	tracker.event(received, 0)
	reply := dnsTestReply(t, question, dns.RcodeSuccess)
	reply.Truncated = true
	sent := dnsTestPacket(t, reply, 1_200_000, "127.0.0.53:53")
	sent.sent, sent.destination, sent.pid = true, "127.0.0.1:41000", 50
	if answer, ok := tracker.event(sent, 0); !ok || answer.Event != traceDNSTruncatedEvent || answer.Side != traceServerSide {
		t.Fatalf("server truncated answer = %#v, %t", answer, ok)
	}
	// accept event는 socket 주인을 아직 모를 수 있다. pid 없이도 client host로 이름을 찾는다.
	accept := captureEvent{Protocol: "tcp", Event: "tcp_accept", Source: "127.0.0.53:53", Destination: "127.0.0.1:52000"}
	event, ok := tracker.tcpEvent(accept)
	if !ok || event.Event != traceDNSTCPAcceptEvent || event.Side != traceServerSide || event.Target != "big.example.com" || event.QueryType != "TXT" {
		t.Fatalf("server tcp accept = %#v, %t", event, ok)
	}
	for _, other := range []captureEvent{
		{Protocol: "tcp", Event: "tcp_accept", Source: "127.0.0.1:8080", Destination: "127.0.0.1:52000"},
		{Protocol: "tcp", Event: "tcp_connect", Source: "127.0.0.1:52000", Destination: "127.0.0.53:53"},
	} {
		if dropped, ok := tracker.tcpEvent(other); ok {
			t.Fatalf("server tracker kept %#v as %#v", other, dropped)
		}
	}
	if client, ok := newDNSQueryTracker(false).tcpEvent(accept); ok {
		t.Fatalf("client tracker kept a server accept: %#v", client)
	}
	var counts traceDNSCounts
	counts.observe(event)
	if counts.TCPConnections != 1 || counts.Errors != 0 {
		t.Fatalf("counts = %#v", counts)
	}
}
