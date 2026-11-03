package edc

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// 지연 줄은 trace가 있으면 RTT를, 없으면 connect 시간을 쓴다. 연결된 시도만 센다.
func TestJoinLatencyUsesRTTThenConnectTime(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	attempts := []interface{}{
		map[string]interface{}{"outcome": diagnoseOutcomeConnected, "connect_ms": float64(2), "rtt_us": float64(1400)},
		map[string]interface{}{"outcome": diagnoseOutcomeConnected, "connect_ms": float64(3), "rtt_us": float64(1800)},
		map[string]interface{}{"outcome": "timeout", "connect_ms": float64(3000)},
	}
	traced := extractJoinClient(map[string]interface{}{"attempts": attempts})
	if got := joinLatency(traced); got != "RTT 1.4ms–1.8ms (avg 1.6ms, 2 connections)" {
		t.Errorf("traced latency = %q", got)
	}
	for _, raw := range attempts {
		delete(raw.(map[string]interface{}), "rtt_us")
	}
	phases := extractJoinClient(map[string]interface{}{"attempts": attempts})
	if got := joinLatency(phases); !strings.HasPrefix(got, "connect 2ms–3ms (2 connections") {
		t.Errorf("connect latency = %q", got)
	}
	if got := joinLatency(joinClient{}); got != "" {
		t.Errorf("no connection must give no latency line, got %q", got)
	}
}

// 경로가 정상이면 다음은 서비스다. port로 짐작할 수 없으면 명령을 권하지 않는다. IPv6 link-local 주소의 zone은 URL에서
// %25로 써야 http check가 읽는다.
func TestDiagnoseServiceNextFollowsThePort(t *testing.T) {
	for _, test := range []struct{ hostPort, port, want string }{
		{"jwserver69:80", "80", "edc http check http://jwserver69:80"},
		{"jwserver69:443", "443", "edc tls check jwserver69:443"},
		{"jwserver69:22", "22", ""},
		{"[fe80::1%eth0]:8080", "8080", "edc http check http://[fe80::1%25eth0]:8080"},
	} {
		if got := diagnoseServiceNext(test.hostPort, test.port); got != test.want {
			t.Errorf("%s: %q, want %q", test.hostPort, got, test.want)
		}
	}
}

// 장비 담당자 제목은 두 host 사이의 장비가 의심될 때만 쓴다. 정상이면 시작 시각 차이도 보이지 않는다.
func TestJoinOutputHeadingFollowsTheSegment(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	for segment, want := range map[string]string{
		joinSegmentBetweenHosts: T("join.label.checklist"),
		joinSegmentReturnPath:   T("join.label.checklist"),
		joinSegmentTargetHost:   T("join.label.next"),
		joinSegmentWorks:        T("join.label.next"),
	} {
		var output bytes.Buffer
		printReportJoin(&output, reportJoin{Segment: segment, Checklist: []string{"item"}, Time: reportJoinTime{Overlap: true, Skew: "+3s"}})
		text := output.String()
		if !strings.Contains(text, want) {
			t.Errorf("%s: heading misses %q:\n%s", segment, want, text)
		}
		if strings.Contains(text, "+3s") {
			t.Errorf("%s: the skew of overlapping runs must not show:\n%s", segment, text)
		}
	}
}

// --peer는 두 결과의 판정 줄을 join 블록으로 대신한다. 같은 결론이 세 번 보이지 않게 한다.
func TestPeerSummaryShowsTheJoinOnly(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.81.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictCompleted)}
	run, stdout, _ := peerTestRun(t, runner, "")
	if code := run.execute(context.Background()); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	text := stdout.String()
	if strings.Contains(text, "  tcp.diagnose  ") || strings.Contains(text, "  tcp.expect  ") || strings.Contains(text, "next:") {
		t.Errorf("the separate result lines must fold into the join:\n%s", text)
	}
	if !strings.Contains(text, T("join.label.verdict")+"  "+joinSegmentWorks) {
		t.Errorf("the join verdict is missing:\n%s", text)
	}
}

// join이 구간을 정하지 못하면 client 오류 상자가 유일한 원인 설명이다. 접지 않고 보인다.
func TestPeerSummaryKeepsTheClientErrorWhenUndetermined(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	setLanguage("en")
	preflight := "uid=0\nedc=/usr/local/bin/edc\nversion=edc 0.81.1 (schema 1.0)\n"
	runner := &fakePeerRunner{preflight: preflight, header: true, report: peerTestTargetReport(t, expectVerdictNoSYN)}
	run, stdout, _ := peerTestRun(t, runner, "")
	run.diagnose = func(context.Context, func(Result)) Result {
		return Result{
			Probe: tcpDiagnoseProbeID, Status: StatusFail, Summary: "lookup failed",
			Error:   &DiagnosticError{Kind: "dns", Message: "lookup db.internal: no such host"},
			Metrics: map[string]interface{}{"input": "db.internal:2171", "verdict": diagnoseVerdictDNS},
		}
	}
	run.execute(context.Background())
	text := stdout.String()
	if !strings.Contains(text, "no such host") || !strings.Contains(text, T("join.label.verdict")+"  "+joinSegmentUndetermined) {
		t.Errorf("the client error must stay next to the undetermined join:\n%s", text)
	}
}
