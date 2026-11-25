package edc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const whyTestMS = uint64(time.Millisecond)

// whyTestFile은 0ms부터 10초까지 기록한 파일이다. event의 시각은 ms 단위로 쓴다.
func whyTestFile(protocol string, events ...captureEvent) *traceShowFile {
	return &traceShowFile{protocol: protocol, events: events, summary: captureSummary{Event: "capture_summary", StartBootTimeNS: 1, EndBootTimeNS: 10_000 * whyTestMS}}
}

func whyTestResponse(protocol string, pid uint32, endMS, latencyMS float64) captureEvent {
	return captureEvent{Protocol: protocol, PID: pid, Process: "app", BootTimeNS: uint64(endMS * float64(whyTestMS)), LatencyMS: &latencyMS}
}

// whyTestRequest는 서버가 받은 요청의 최종 응답이다. 요청은 endMS-latencyMS에 시작했다.
func whyTestRequest(pid uint32, path string, endMS, latencyMS float64) captureEvent {
	event := whyTestResponse("http", pid, endMS, latencyMS)
	event.Event, event.Side, event.Status, event.Method, event.Path, event.Source = "http_2xx", traceServerSide, 200, "GET", path, "127.0.0.1:8080"
	return event
}

func whyTestDNS(pid uint32, name string, endMS, latencyMS float64) captureEvent {
	event := whyTestResponse("dns", pid, endMS, latencyMS)
	event.Event, event.Target, event.QueryType = "dns_noerror", name, "A"
	return event
}

func whyTestPath(t *testing.T, report traceWhyReport, path string) traceWhyPath {
	t.Helper()
	for _, item := range report.Paths {
		if item.Path == path {
			return item
		}
	}
	t.Fatalf("no path %q in %+v", path, report.Paths)
	return traceWhyPath{}
}

func whyTestAnalyze(t *testing.T, options traceWhyOptions, files ...*traceShowFile) traceWhyReport {
	t.Helper()
	report, err := analyzeTraceWhy(files, options)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestTraceWhyAttributesACallToTheOnlyRequestThatContainsIt(t *testing.T) {
	sql, client := whyTestResponse("mysql", 7, 590, 80), whyTestResponse("http", 7, 760, 50)
	sql.Side, sql.MySQL = traceClientSide, &traceMySQLEvent{Command: "query", SQL: "SELECT * FROM t WHERE id = 5"}
	client.Side, client.Status, client.Method, client.Target, client.Path = traceClientSide, 200, "POST", "vault:8200", "/v1/transit/sign/key"
	report := whyTestAnalyze(t, traceWhyOptions{},
		whyTestFile("http", whyTestRequest(7, "/dns", 400, 300), whyTestRequest(7, "/db", 600, 100), whyTestRequest(7, "/sign", 800, 100), whyTestRequest(7, "/ok", 900, 2), client),
		whyTestFile("dns", whyTestDNS(7, "slow.test", 395, 290)),
		whyTestFile("mysql", sql))
	if report.Requests != 4 || report.Calls != 3 || report.AmbiguousCalls != 0 || report.OutsideCalls != 0 {
		t.Fatalf("report = %+v", report)
	}
	// 평균 지연이 큰 경로가 먼저다.
	if report.Paths[0].Path != "/dns" || report.Paths[3].Path != "/ok" {
		t.Fatalf("order = %+v", report.Paths)
	}
	dns := whyTestPath(t, report, "/dns")
	if len(dns.Calls) != 1 || dns.Calls[0] != (traceWhyCall{"dns", "slow.test A", 1, 290}) || dns.SelfMS != 10 || dns.LatencyMS != 300 || dns.Parallel {
		t.Fatalf("/dns = %+v", dns)
	}
	if db := whyTestPath(t, report, "/db"); len(db.Calls) != 1 || db.Calls[0] != (traceWhyCall{"mysql", "SELECT * FROM t WHERE id = ?", 1, 80}) || db.SelfMS != 20 {
		t.Fatalf("/db = %+v", db)
	}
	if sign := whyTestPath(t, report, "/sign"); len(sign.Calls) != 1 || sign.Calls[0] != (traceWhyCall{"http", "POST vault:8200/v1/transit/sign/key", 1, 50}) {
		t.Fatalf("/sign = %+v", sign)
	}
	if ok := whyTestPath(t, report, "/ok"); len(ok.Calls) != 0 || ok.SelfMS != 2 {
		t.Fatalf("/ok = %+v", ok)
	}
}

// 한 process가 두 요청을 동시에 처리하면 두 요청에 모두 들어가는 호출은 어느 쪽에도 더하지 않는다. 긴 호출은 짧은 요청에
// 들어갈 수 없어서 제 요청에 붙는다.
func TestTraceWhyLeavesACallThatSeveralRequestsContainUnassigned(t *testing.T) {
	report := whyTestAnalyze(t, traceWhyOptions{},
		whyTestFile("http", whyTestRequest(7, "/slow", 400, 300), whyTestRequest(7, "/other", 420, 300), whyTestRequest(7, "/quick", 205, 5)),
		whyTestFile("dns",
			whyTestDNS(7, "fast.test", 203, 1),   // 세 요청이 모두 포함한다
			whyTestDNS(7, "slow.test", 399, 295), // /slow만 포함한다. /other는 더 늦게 시작했고 /quick은 먼저 끝났다
		))
	if report.Calls != 2 || report.AmbiguousCalls != 1 || report.AmbiguousMS != 1 || report.CallMS != 296 {
		t.Fatalf("report = %+v", report)
	}
	slow := whyTestPath(t, report, "/slow")
	// 가릴 수 없는 호출이 덮는 1ms는 /slow에서는 제 호출이 이미 덮은 시간이다.
	if len(slow.Calls) != 1 || slow.Calls[0].Name != "slow.test A" || slow.AmbiguousCalls != 1 || slow.AmbiguousMS != 0 || slow.SelfMS != 5 {
		t.Fatalf("/slow = %+v", slow)
	}
	for path, self := range map[string]float64{"/other": 299, "/quick": 4} {
		if item := whyTestPath(t, report, path); len(item.Calls) != 0 || item.AmbiguousCalls != 1 || item.AmbiguousMS != 1 || item.SelfMS != self {
			t.Fatalf("%s = %+v", path, item)
		}
	}
	var out bytes.Buffer
	report.print(&out)
	if candidates := slow.AmbiguousCandidates; len(candidates) != 1 || candidates[0].Name != "fast.test A" || candidates[0].Calls != 1 {
		t.Fatalf("/slow candidates = %+v", candidates)
	}
	for _, want := range []string{"\t    maybe dns fast.test A\t1\t", "maybe: calls that happened while the request ran", "Ambiguous calls: 1 (0% of call time)", "\t  (ambiguous)\t1\t1.0ms\t\t0%", "\t  (ambiguous)\t1\t1.0ms\t\t20%", "ambiguous: request time that only calls of an unknown request cover"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestTraceWhyCountsCallsOutsideEveryRequest(t *testing.T) {
	server := whyTestDNS(9, "served.test", 250, 10)
	server.Side = traceServerSide
	report := whyTestAnalyze(t, traceWhyOptions{},
		whyTestFile("http", whyTestRequest(7, "/a", 300, 100)),
		whyTestFile("dns",
			whyTestDNS(7, "before.test", 150, 20),    // 요청보다 먼저 끝났다
			whyTestDNS(7, "crossing.test", 320, 50),  // 요청 안에서 시작해 요청이 끝난 뒤에 끝났다
			whyTestDNS(7, "early.test", 250, 80),     // 요청보다 먼저 시작했다
			whyTestDNS(8, "other-pid.test", 250, 10), // 요청을 받지 않은 process의 호출은 세지 않는다
			server,                                   // 로컬 DNS 서버가 본 응답은 호출이 아니다
		))
	if a := whyTestPath(t, report, "/a"); len(a.Calls) != 0 || a.SelfMS != 100 {
		t.Fatalf("/a = %+v", a)
	}
	if report.Calls != 3 || report.OutsideCalls != 3 || report.OutsideMS != 150 || report.AmbiguousCalls != 0 {
		t.Fatalf("report = %+v", report)
	}
}

// 한 요청 안에서 두 호출이 겹치면 자체 시간은 두 구간의 합집합을 뺀 값이고 0 밑으로 내려가지 않는다.
func TestTraceWhySubtractsTheUnionOfParallelCalls(t *testing.T) {
	report := whyTestAnalyze(t, traceWhyOptions{},
		whyTestFile("http", whyTestRequest(7, "/fan", 400, 300), whyTestRequest(7, "/full", 800, 100)),
		whyTestFile("dns",
			whyTestDNS(7, "a.test", 300, 200), whyTestDNS(7, "b.test", 350, 200), // 100~300과 150~350
			whyTestDNS(7, "c.test", 800, 100), whyTestDNS(7, "d.test", 800, 100), // 둘 다 요청 전체
		))
	fan := whyTestPath(t, report, "/fan")
	if !fan.Parallel || fan.SelfMS != 50 || len(fan.Calls) != 2 || fan.Calls[0].MS+fan.Calls[1].MS != 400 {
		t.Fatalf("/fan = %+v", fan)
	}
	if full := whyTestPath(t, report, "/full"); !full.Parallel || full.SelfMS != 0 {
		t.Fatalf("/full = %+v", full)
	}
	var out bytes.Buffer
	report.print(&out)
	if !strings.Contains(out.String(), "  self *\t") || !strings.Contains(out.String(), "can add up to more than 100%") {
		t.Fatalf("output = %q", out.String())
	}
}

// 파일마다 기록을 시작하고 끝낸 때가 다르다. 모든 파일이 기록하던 구간 밖에 걸친 요청과 호출은 세지 않는다.
func TestTraceWhyUsesOnlyTheRangeThatEveryRecordingCovers(t *testing.T) {
	http := whyTestFile("http", whyTestRequest(7, "/early", 600, 200), whyTestRequest(7, "/inside", 1500, 200), whyTestRequest(7, "/late", 8100, 200))
	dns := whyTestFile("dns", whyTestDNS(7, "early.test", 590, 100), whyTestDNS(7, "inside.test", 1490, 100))
	http.summary.StartBootTimeNS, http.summary.EndBootTimeNS = 100*whyTestMS, 9000*whyTestMS
	dns.summary.StartBootTimeNS, dns.summary.EndBootTimeNS = 500*whyTestMS, 8000*whyTestMS
	report := whyTestAnalyze(t, traceWhyOptions{}, http, dns)
	if report.Requests != 1 || report.Calls != 1 || report.AnalyzedMS != 7500 || report.RecordedMS != 8900 {
		t.Fatalf("report = %+v", report)
	}
	if inside := whyTestPath(t, report, "/inside"); len(inside.Calls) != 1 || inside.Calls[0].Name != "inside.test A" {
		t.Fatalf("/inside = %+v", inside)
	}

	dns.summary.StartBootTimeNS = 9500 * whyTestMS
	dns.summary.EndBootTimeNS = 9900 * whyTestMS
	if _, err := analyzeTraceWhy([]*traceShowFile{http, dns}, traceWhyOptions{}); !errors.Is(err, errTraceWhyNoSharedRange) {
		t.Fatalf("recordings without a shared range: %v", err)
	}
}

func TestTraceWhyRefusesRecordingsItCannotAlign(t *testing.T) {
	old := whyTestFile("dns", whyTestDNS(7, "a.test", 300, 10))
	old.summary.StartBootTimeNS, old.summary.EndBootTimeNS = 0, 0
	if _, err := analyzeTraceWhy([]*traceShowFile{whyTestFile("http", whyTestRequest(7, "/a", 400, 300)), old}, traceWhyOptions{}); err == nil || !strings.Contains(err.Error(), "the dns recording has no recorded range") {
		t.Fatalf("a file without a range: %v", err)
	}
	if _, err := analyzeTraceWhy([]*traceShowFile{whyTestFile("dns")}, traceWhyOptions{}); err == nil || !strings.Contains(err.Error(), "no http recording") {
		t.Fatalf("no http file: %v", err)
	}
}

func TestTraceWhyKeepsOnlyRequestsOfThePort(t *testing.T) {
	proxy := whyTestRequest(7, "/proxy", 400, 300)
	proxy.Source = "[::1]:80"
	unmapped := whyTestRequest(7, "/unmapped", 900, 100)
	unmapped.Source = ""
	files := []*traceShowFile{whyTestFile("http", proxy, whyTestRequest(7, "/app", 700, 100), unmapped)}
	if report := whyTestAnalyze(t, traceWhyOptions{port: 80}, files...); len(report.Paths) != 1 || report.Paths[0].Path != "/proxy" {
		t.Fatalf("port 80 = %+v", report.Paths)
	}
	if report := whyTestAnalyze(t, traceWhyOptions{}, files...); len(report.Paths) != 3 {
		t.Fatalf("no port = %+v", report.Paths)
	}
}

// 응답 시간이 없는 event, 중간 응답, 요청 event는 구간을 만들지 못하므로 요청으로도 호출로도 세지 않는다.
func TestTraceWhyIgnoresEventsWithoutAnInterval(t *testing.T) {
	request := captureEvent{Protocol: "http", Event: traceHTTPRequestEvent, Side: traceServerSide, PID: 7, Method: "GET", Path: "/a", BootTimeNS: 100 * whyTestMS}
	unpaired := whyTestRequest(7, "/unpaired", 300, 0)
	unpaired.LatencyMS = nil
	interim := whyTestRequest(7, "/interim", 300, 10)
	interim.Status = 100
	query := captureEvent{Protocol: "dns", Event: traceDNSQueryEvent, PID: 7, Target: "a.test", BootTimeNS: 350 * whyTestMS}
	report := whyTestAnalyze(t, traceWhyOptions{}, whyTestFile("http", request, unpaired, interim, whyTestRequest(7, "/a", 400, 300)), whyTestFile("dns", query))
	if report.Requests != 1 || report.Calls != 0 || len(report.Paths) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestTraceWhyCapsPathsAndCallNames(t *testing.T) {
	var requests, calls []captureEvent
	for index := range traceGroupLimit + 5 {
		requests = append(requests, whyTestRequest(7, fmt.Sprintf("/users/%d", index), float64(index)+1, 0.5))
	}
	requests = append(requests, whyTestRequest(8, "/fan", 9000, 5000))
	for index := range traceWhyCallLimit + 3 {
		call := whyTestResponse("http", 8, 4001+float64(index), 0.5)
		call.Side, call.Status, call.Method, call.Target, call.Path = traceClientSide, 200, "GET", "api", fmt.Sprintf("/items/%d", index)
		calls = append(calls, call)
	}
	file := whyTestFile("http", append(requests, calls...)...)
	file.summary.EndBootTimeNS = 20_000 * whyTestMS
	report := whyTestAnalyze(t, traceWhyOptions{}, file)
	if len(report.Paths) != traceGroupLimit+1 {
		t.Fatalf("paths = %d", len(report.Paths))
	}
	// /fan은 상한이 찬 뒤에 와서 다른 넘친 경로와 함께 (other)에 들어간다.
	other := whyTestPath(t, report, traceGroupOther)
	if other.Requests != 6 || other.Process != "" || len(other.Calls) != traceWhyCallLimit+1 {
		t.Fatalf("(other) = requests %d, process %q, calls %d", other.Requests, other.Process, len(other.Calls))
	}
	var overflow uint64
	for _, call := range other.Calls {
		if call.Name == traceGroupOther {
			overflow = call.Calls
		}
	}
	if overflow != 3 {
		t.Fatalf("overflow calls = %d", overflow)
	}
	var out bytes.Buffer
	report.print(&out)
	if !strings.Contains(out.String(), fmt.Sprintf("  %d more calls\t", traceWhyCallLimit+1-traceWhyCallRows)) {
		t.Fatal("the call rows past the first five were not merged into one row")
	}
}

func TestTraceWhyPrintsTheBreakdownAndLostEvents(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	redis, postgres := whyTestResponse("redis", 7, 150, 10), whyTestResponse("postgres", 7, 250, 60)
	redis.Side, redis.Redis = traceClientSide, &traceRedisEvent{Command: "GET", Key: "session:1"}
	postgres.Side, postgres.Postgres = traceClientSide, &tracePostgresEvent{Command: "query", SQL: "SELECT 1"}
	dns := whyTestFile("dns")
	dns.summary.LostEvents = 12
	report := whyTestAnalyze(t, traceWhyOptions{}, whyTestFile("http", whyTestRequest(7, "/a", 300, 200)), dns, whyTestFile("redis", redis), whyTestFile("postgres", postgres))
	if len(report.Lost) != 1 || report.Lost[0] != (traceWhyLost{"dns", 12}) {
		t.Fatalf("lost = %+v", report.Lost)
	}
	var out bytes.Buffer
	report.print(&out)
	for _, want := range []string{
		"10.0s of 10.0s recorded", "Requests: 1", "Calls: 2", "Lost events: 12 in dns",
		"PROCESS\tREQUEST / CALL\tCOUNT\tAVG\tMAX\tSHARE",
		"app\tGET /a\t1\t200.0ms\t200.0ms\t",
		"\t  postgres SELECT ?\t1\t60.0ms\t\t30%",
		"\t  redis GET\t1\t10.0ms\t\t5%",
		"\t  self\t\t130.0ms\t\t65%",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not contain %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "session:1") || strings.Contains(out.String(), "mbiguous") {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	whyTestAnalyze(t, traceWhyOptions{port: 9}, whyTestFile("http", whyTestRequest(7, "/a", 300, 200))).print(&out)
	if !strings.Contains(out.String(), "No HTTP request started and ended in the analyzed range.") {
		t.Fatalf("empty output = %q", out.String())
	}
}
