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
	return dnsPacket{bootTimeNS: at, pid: 10, process: "dig", query: !message.Response, source: source, destination: "127.0.0.53:53", payload: payload}
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
	tracker := newDNSQueryTracker()
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
	if !ok || answer.Event != "dns_noerror" || answer.LatencyMS == nil || *answer.LatencyMS != 5.5 || answer.dnsAnswered != 2 || !slices.Equal(answer.Answers, []string{"203.0.113.10"}) {
		t.Fatalf("answer event = %#v, %t", answer, ok)
	}
	if tracker.size != 0 || len(tracker.pending) != 0 {
		t.Fatalf("pending after the answer = %d, %v", tracker.size, tracker.pending)
	}
	// 다른 로컬 port로 온 응답은 짝이 없으므로 응답 시간이 없다.
	other, _ := tracker.event(dnsTestPacket(t, dnsTestReply(t, question, dns.RcodeSuccess, "example.com. 60 IN A 203.0.113.10"), 7_000_000, "127.0.0.1:42000"), 0)
	if other.LatencyMS != nil || other.dnsAnswered != 0 {
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
		event, ok := newDNSQueryTracker().event(dnsTestPacket(t, dnsTestReply(t, question, test.rcode, test.records...), 1, "127.0.0.1:41000"), 0)
		if !ok || event.Event != test.want {
			t.Fatalf("rcode %d with %d answers = %q, want %q", test.rcode, len(test.records), event.Event, test.want)
		}
		if traceDNSError(event.Event) != (test.want != "dns_noerror" && test.want != "dns_nodata") {
			t.Fatalf("traceDNSError(%q) = %t", event.Event, traceDNSError(event.Event))
		}
	}
}

func TestDNSCutAnswerTakesTheNameFromTheQuery(t *testing.T) {
	tracker := newDNSQueryTracker()
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
	served.query = true
	if _, ok := tracker.event(served, 0); ok {
		t.Fatal("an answer sent to port 53 was read as a query")
	}
}

func dnsTestEvents() []captureEvent {
	latency := func(value float64) *float64 { return &value }
	return []captureEvent{
		{Protocol: "dns", Event: traceDNSQueryEvent, Process: "dig", Target: "example.com", QueryType: "A", Destination: "127.0.0.53:53", Bytes: 40},
		{Protocol: "dns", Event: "dns_noerror", Process: "dig", Target: "example.com", QueryType: "A", Destination: "127.0.0.53:53", Answers: []string{"203.0.113.10"}, LatencyMS: latency(2), dnsAnswered: 1, Bytes: 56},
		{Protocol: "dns", Event: traceDNSQueryEvent, Process: "curl", Target: "missing.invalid", QueryType: "A", Destination: "127.0.0.53:53"},
		{Protocol: "dns", Event: "dns_nxdomain", Process: "curl", Target: "missing.invalid", QueryType: "A", Destination: "127.0.0.53:53", LatencyMS: latency(6), dnsAnswered: 1},
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
