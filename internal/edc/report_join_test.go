package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestJoinSegmentFollowsTheDocumentedTable(t *testing.T) {
	for _, row := range []struct {
		client, basis, target string
		segment, reason       string
	}{
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictNoSYN, joinSegmentBetweenHosts, ""},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictNoACK, joinSegmentReturnPath, ""},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictNoReply, joinSegmentTargetHost, ""},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictUnseen, joinSegmentTargetHost, ""},
		{diagnoseVerdictRefused, diagnoseBasisTrace, expectVerdictRST, joinSegmentTargetHost, ""},
		{diagnoseVerdictLocalDrop, diagnoseBasisTrace, expectVerdictNoSYN, joinSegmentClientHost, ""},
		{diagnoseVerdictLocalDrop, diagnoseBasisPartial, expectVerdictCompleted, joinSegmentClientHost, ""},
		{diagnoseVerdictDegraded, diagnoseBasisPhases, expectVerdictCompleted, joinSegmentPathQuality, ""},
		{diagnoseVerdictConnected, diagnoseBasisPhases, expectVerdictCompleted, joinSegmentWorks, ""},
		// 표 밖의 조합은 더 약한 판정이고 이유가 붙는다.
		{diagnoseVerdictNoReply, diagnoseBasisPhases, expectVerdictNoSYN, joinSegmentBetweenHosts, "join.reason.basis_phases"},
		{diagnoseVerdictNoReply, diagnoseBasisPhases, expectVerdictNoACK, joinSegmentReturnPath, "join.reason.basis_phases"},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictRST, joinSegmentReturnPath, "join.reason.rst_lost"},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictIPsecHidden, joinSegmentUndetermined, "join.reason.reply_hidden"},
		{diagnoseVerdictNoReply, diagnoseBasisTrace, expectVerdictCompleted, joinSegmentInconsistent, "join.reason.mismatch"},
		{diagnoseVerdictRefused, diagnoseBasisPhases, expectVerdictNoSYN, joinSegmentBetweenHosts, "join.reason.rst_from_path"},
		{diagnoseVerdictRefused, diagnoseBasisTrace, expectVerdictCompleted, joinSegmentInconsistent, "join.reason.mismatch"},
		{diagnoseVerdictIntermittent, diagnoseBasisTrace, expectVerdictCompleted, joinSegmentPathQuality, "join.reason.intermittent"},
		{diagnoseVerdictIntermittent, diagnoseBasisTrace, expectVerdictNoSYN, joinSegmentInconsistent, "join.reason.mismatch"},
		{diagnoseVerdictDegraded, diagnoseBasisTrace, expectVerdictRST, joinSegmentInconsistent, "join.reason.mismatch"},
		{diagnoseVerdictConnected, diagnoseBasisPhases, expectVerdictNoSYN, joinSegmentInconsistent, "join.reason.mismatch"},
		{diagnoseVerdictDNS, "", expectVerdictNoSYN, joinSegmentUndetermined, "join.reason.dns_failed"},
		{diagnoseVerdictNoAttempt, diagnoseBasisPhases, expectVerdictNoSYN, joinSegmentUndetermined, "join.reason.no_attempt"},
		{diagnoseVerdictUnreachable, diagnoseBasisTrace, expectVerdictNoSYN, joinSegmentUndetermined, "join.reason.unreachable"},
		{"", "", "", joinSegmentUndetermined, "join.reason.unknown_pair"},
	} {
		segment, reason := joinSegment(row.client, row.basis, row.target)
		if segment != row.segment || reason != row.reason {
			t.Errorf("joinSegment(%q, %q, %q) = %q, %q; want %q, %q", row.client, row.basis, row.target, segment, reason, row.segment, row.reason)
		}
		if reason != "" && T(reason) == reason {
			t.Errorf("reason %q has no message", reason)
		}
	}
	for segment, status := range map[string]Status{
		joinSegmentWorks: StatusPass, joinSegmentPathQuality: StatusWarn, joinSegmentUndetermined: StatusWarn, joinSegmentInconsistent: StatusWarn,
		joinSegmentBetweenHosts: StatusFail, joinSegmentReturnPath: StatusFail, joinSegmentTargetHost: StatusFail, joinSegmentClientHost: StatusFail,
	} {
		if got := joinStatus(segment); got != status {
			t.Errorf("joinStatus(%q) = %q, want %q", segment, got, status)
		}
	}
}

// joinTestClient는 파일에서 읽은 tcp.diagnose 결과와 같은 꼴이다. 수는 float64, 배열은 []interface{}다.
func joinTestClient(verdict, basis string, started time.Time) Result {
	attempts := []interface{}{}
	for index := 1; index <= 3; index++ {
		attempts = append(attempts, map[string]interface{}{"index": float64(index), "outcome": "timeout", "connect_ms": float64(3000)})
	}
	metrics := map[string]interface{}{
		"input": "db.internal:5432", "address": "10.9.0.5:5432", "count": float64(3), "verdict": verdict, "basis": basis,
		"route":    map[string]interface{}{"destination": "10.9.0.5", "interface": "tun0", "source": "10.8.0.2"},
		"attempts": attempts,
		"totals":   map[string]interface{}{"connected": float64(0), "refused": float64(0), "timeouts": float64(3), "other": float64(0), "slow_connects": float64(0), "syn_retransmits": float64(9)},
		"trace":    map[string]interface{}{"tcp": "on", "drop": "on", "lost_events": float64(0)},
	}
	if verdict == diagnoseVerdictLocalDrop {
		metrics["drops"] = map[string]interface{}{"NETFILTER_DROP": float64(3)}
	}
	return Result{Probe: tcpDiagnoseProbeID, Status: StatusFail, StartedAt: started, DurationMS: 12000, Summary: verdict, Metrics: metrics}
}

func joinTestTarget(verdict string, started time.Time, sources []expectSource) Result {
	metrics := map[string]interface{}{
		"port": float64(5432), "duration_ms": float64(30000), "stop_reason": "duration", "verdict": verdict, "observation_status": "observed",
		"attempts": float64(3), "syn_in": float64(3), "syn_retransmits": float64(6), "syn_ack_out": float64(0), "syn_ack_retransmits": float64(0),
		"rst_out": float64(0), "rst_in": float64(0), "handshakes": float64(0),
		"drop_trace": map[string]interface{}{"status": "pass"}, "forwarded_packets": float64(0), "socket_drops": float64(0),
	}
	if len(sources) == 0 {
		metrics["observation_status"] = "no_syn"
	}
	// 파일에서 읽은 값과 같게 map 배열로 둔다.
	data, _ := json.Marshal(sources)
	var raw []interface{}
	_ = json.Unmarshal(data, &raw)
	metrics["sources"] = raw
	return Result{Probe: expectProbeID, Status: expectVerdictStatus(verdict), StartedAt: started, DurationMS: 30000, Summary: verdict, Metrics: metrics}
}

func joinTestSource(address, verdict string) expectSource {
	matches := true
	return expectSource{
		Address: address, Local: "10.9.0.5", Interface: "eth0", Verdict: verdict,
		expectCounts: expectCounts{Attempts: 3, SYNIn: 3, SYNRetransmits: 6, RSTOut: 3},
		Return:       &expectReturnPath{Device: "eth0", Via: "10.9.0.1", Source: "10.9.0.5", MatchesArrival: &matches, RPFilter: &expectRPFilter{Interface: 1, All: 0, Effective: 1}},
	}
}

func joinTestSaved(path string, started time.Time, durationMS int64, hostname string, result Result) savedReport {
	return savedReport{path: path, report: Report{
		SchemaVersion: "1.0", Tool: ToolInfo{Name: "edc", Version: "test"},
		Run:  RunInfo{ID: "run-" + hostname, StartedAt: started, DurationMS: durationMS},
		Host: map[string]interface{}{"hostname": hostname}, Results: []Result{result}, Summary: summarize([]Result{result}),
	}}
}

func TestJoinReportsMatchesTheClientSourceAndFillsTheChecklist(t *testing.T) {
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	client := joinTestSaved("c.json", started.Add(5*time.Second), 12000, "client-host", joinTestClient(diagnoseVerdictRefused, diagnoseBasisTrace, started))
	target := joinTestSaved("t.json", started, 30000, "target-host", joinTestTarget(expectVerdictNoACK, started, []expectSource{
		joinTestSource("10.8.0.9", expectVerdictNoACK), joinTestSource("10.8.0.2", expectVerdictRST),
	}))
	join := joinReports(client, target)
	if join.Segment != joinSegmentTargetHost || join.Status != StatusFail {
		t.Fatalf("segment = %q status = %q warnings = %v", join.Segment, join.Status, join.Warnings)
	}
	if !join.Target.SourceMatched || join.Target.Source != "10.8.0.2" || join.Target.Verdict != expectVerdictRST || join.Target.SourceInterface != "eth0" || join.Target.Counts.RSTOut != 3 {
		t.Fatalf("target = %#v", join.Target)
	}
	if join.Client.Source != "10.8.0.2" || join.Client.Port != "5432" || join.Client.Host != "db.internal" || join.Client.Attempts != 3 || join.Client.Totals.SYNRetransmits != 9 {
		t.Fatalf("client = %#v", join.Client)
	}
	if len(join.Warnings) != 0 {
		t.Fatalf("warnings = %v", join.Warnings)
	}
	if !join.Time.Overlap || join.Time.Skew != "-5s" {
		t.Fatalf("time = %#v", join.Time)
	}
	joined := strings.Join(join.Checklist, "\n")
	for _, want := range []string{"5432", "10.8.0.2", "edc listen --tcp", "ss -ltn"} {
		if !strings.Contains(joined, want) {
			t.Errorf("checklist misses %q: %q", want, joined)
		}
	}
	if join.Inputs.Client.Path != "c.json" || join.Inputs.Target.Probe != expectProbeID || join.Inputs.Target.Hostname != "target-host" {
		t.Fatalf("inputs = %#v", join.Inputs)
	}
	var output bytes.Buffer
	printReportJoin(&output, join)
	text := output.String()
	for _, want := range []string{"join  c.json + t.json", T("join.label.verdict") + "  " + joinSegmentTargetHost, "10.8.0.2 → 10.9.0.5:5432", "verdict rst", "dev eth0"} {
		if !strings.Contains(text, want) {
			t.Errorf("output misses %q:\n%s", want, text)
		}
	}
}

func TestJoinReportsWarnsWhenTheRunsDoNotOverlap(t *testing.T) {
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	client := joinTestSaved("c.json", started.Add(2*time.Minute), 12000, "client-host", joinTestClient(diagnoseVerdictNoReply, diagnoseBasisTrace, started))
	target := joinTestSaved("t.json", started, 30000, "target-host", joinTestTarget(expectVerdictNoSYN, started, nil))
	join := joinReports(client, target)
	if join.Segment != joinSegmentBetweenHosts || join.Time.Overlap || join.Time.Skew != "-2m0s" || join.Time.Gap != "1m30s" {
		t.Fatalf("segment = %q time = %#v", join.Segment, join.Time)
	}
	if len(join.Warnings) != 1 || !strings.Contains(join.Warnings[0], "1m30s") {
		t.Fatalf("warnings = %v", join.Warnings)
	}
	var output bytes.Buffer
	printReportJoin(&output, join)
	if !strings.Contains(output.String(), T("join.line.overlap_no", "1m30s")) {
		t.Fatalf("output misses the gap:\n%s", output.String())
	}
	for _, want := range []string{"10.8.0.2 → 10.9.0.5:5432", "10.8.0.2 → 10.9.0.5"} {
		if !strings.Contains(strings.Join(join.Checklist, "\n"), want) {
			t.Errorf("checklist misses %q: %v", want, join.Checklist)
		}
	}
}

func TestJoinReportsWarnsOnPortSourceAndFromMismatch(t *testing.T) {
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	client := joinTestSaved("c.json", started, 12000, "client-host", joinTestClient(diagnoseVerdictNoReply, diagnoseBasisTrace, started))

	other := joinTestTarget(expectVerdictNoACK, started, []expectSource{joinTestSource("10.8.0.9", expectVerdictNoACK)})
	other.Metrics["port"] = float64(5433)
	join := joinReports(client, joinTestSaved("t.json", started, 30000, "target-host", other))
	if join.Segment != joinSegmentReturnPath || join.Target.SourceMatched || join.Target.Verdict != expectVerdictNoACK {
		t.Fatalf("fallback segment = %q target = %#v", join.Segment, join.Target)
	}
	if len(join.Warnings) != 2 || !strings.Contains(join.Warnings[0], "5433") || !strings.Contains(join.Warnings[1], "10.8.0.9") {
		t.Fatalf("warnings = %v", join.Warnings)
	}

	filtered := joinTestTarget(expectVerdictNoSYN, started, nil)
	filtered.Metrics["from"] = "10.8.1.0/24"
	join = joinReports(client, joinTestSaved("t.json", started, 30000, "target-host", filtered))
	if join.Segment != joinSegmentInconsistent || join.Status != StatusWarn {
		t.Fatalf("filtered segment = %q status = %q warnings = %v", join.Segment, join.Status, join.Warnings)
	}
	if len(join.Warnings) != 2 || !strings.Contains(join.Warnings[0], "10.8.1.0/24") || join.Warnings[1] != T("join.reason.from_excludes") {
		t.Fatalf("filtered warnings = %v", join.Warnings)
	}

	included := joinTestTarget(expectVerdictNoSYN, started, nil)
	included.Metrics["from"] = "10.8.0.2"
	join = joinReports(client, joinTestSaved("t.json", started, 30000, "target-host", included))
	if join.Segment != joinSegmentBetweenHosts || len(join.Warnings) != 0 {
		t.Fatalf("included segment = %q warnings = %v", join.Segment, join.Warnings)
	}
}

func TestJoinReportsToleratesADNSFailureWithoutRoute(t *testing.T) {
	started := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	failed := Result{Probe: tcpDiagnoseProbeID, Status: StatusFail, StartedAt: started, Summary: "dns", Metrics: map[string]interface{}{"input": "db.internal:5432", "verdict": diagnoseVerdictDNS}}
	join := joinReports(joinTestSaved("c.json", started, 100, "client-host", failed), joinTestSaved("t.json", started, 30000, "target-host", joinTestTarget(expectVerdictNoSYN, started, nil)))
	if join.Segment != joinSegmentUndetermined || join.Client.Port != "5432" || join.Client.Host != "db.internal" || join.Client.Source != "" {
		t.Fatalf("join = %#v", join)
	}
	if len(join.Warnings) != 1 || join.Warnings[0] != T("join.reason.dns_failed") {
		t.Fatalf("warnings = %v", join.Warnings)
	}
	var output bytes.Buffer
	printReportJoin(&output, join)
	if !strings.Contains(output.String(), T("join.value.unknown")+" → 10.9.0.5:5432") && !strings.Contains(output.String(), T("join.value.unknown")) {
		t.Fatalf("output:\n%s", output.String())
	}
}

func TestRunReportJoinAcceptsEitherOrderAndRedacts(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, results []Result) string {
		path := filepath.Join(dir, name)
		report := buildReport("test", time.Now(), nil, results, false)
		// buildReport는 실행 시간을 0으로 두므로 두 실행이 겹치도록 늘린다.
		report.Run.DurationMS = 30000
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	started := time.Now().UTC()
	connected := joinTestClient(diagnoseVerdictConnected, diagnoseBasisPhases, started)
	connected.Status = StatusPass
	completed := joinTestSource("10.8.0.2", expectVerdictCompleted)
	completed.Handshakes, completed.RSTOut = 3, 0
	client := write("client.json", []Result{connected})
	target := write("target.json", []Result{joinTestTarget(expectVerdictCompleted, started, []expectSource{completed})})
	refusedClient := write("refused.json", []Result{joinTestClient(diagnoseVerdictRefused, diagnoseBasisPhases, started)})
	rstTarget := write("rst.json", []Result{joinTestTarget(expectVerdictRST, started, []expectSource{joinTestSource("10.8.0.2", expectVerdictRST)})})
	other := write("other.json", []Result{{Probe: "tcp.check", Status: StatusPass}})

	output := filepath.Join(dir, "join.json")
	if code := runReportJoin([]string{"--json", output, client, target}); code != 0 {
		t.Fatalf("works exit code = %d", code)
	}
	var join reportJoin
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &join); err != nil {
		t.Fatal(err)
	}
	if join.Segment != joinSegmentWorks || join.Status != StatusPass || join.Inputs.Client.Path != client || join.Inputs.Target.Path != target {
		t.Fatalf("join = %#v", join)
	}
	if code := runReportJoin([]string{"--json", output, target, client}); code != 0 {
		t.Fatalf("reversed exit code = %d", code)
	}
	data, _ = os.ReadFile(output)
	var reversed reportJoin
	_ = json.Unmarshal(data, &reversed)
	if reversed.Segment != join.Segment || reversed.Inputs.Client.Path != client {
		t.Fatalf("reversed = %#v", reversed.Inputs)
	}
	if code := runReportJoin([]string{"--json", output, refusedClient, rstTarget}); code != 1 {
		t.Fatalf("target_host exit code = %d", code)
	}
	if code := runReportJoin([]string{"--json", output, "--redact", refusedClient, rstTarget}); code != 1 {
		t.Fatalf("redacted exit code = %d", code)
	}
	data, _ = os.ReadFile(output)
	if ipv4Pattern.Match(data) || bytes.Contains(data, []byte("db.internal")) {
		t.Fatalf("redacted output keeps addresses:\n%s", data)
	}
	var redacted reportJoin
	if err := json.Unmarshal(data, &redacted); err != nil || redacted.Segment != joinSegmentTargetHost {
		t.Fatalf("redacted join = %#v err = %v", redacted, err)
	}
	if !regexp.MustCompile(`^<ip:[0-9a-f]{8}>$`).MatchString(redacted.Client.Source) || !strings.HasPrefix(redacted.Client.Host, "<host:") || !strings.Contains(strings.Join(redacted.Checklist, "\n"), "<ip:") {
		t.Fatalf("redacted join = %#v", redacted)
	}
	for _, args := range [][]string{
		{client},
		{client, client},
		{target, target},
		{client, other},
		{client, filepath.Join(dir, "absent.json")},
	} {
		if code := runReportJoin(args); code != 2 {
			t.Errorf("runReportJoin(%v) = %d, want 2", args, code)
		}
	}
}
