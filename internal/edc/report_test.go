package edc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiffReportsClassifiesEntries(t *testing.T) {
	before := Report{Run: RunInfo{ID: "a"}, Host: map[string]interface{}{"hostname": "h"}, Results: []Result{
		{Probe: "http.check", Status: StatusPass, Summary: "HTTP 200", Metrics: map[string]interface{}{"status_code": 200, "total_ms": 120, "final_url": "https://a", "nested": []string{"x"}}},
		{Probe: "dns.lookup", Status: StatusPass, Summary: "ok", DurationMS: 10, Metrics: map[string]interface{}{"cname": "a."}},
		{Probe: "sockets", Status: StatusPass, Summary: "gone"},
		{Probe: "tls.check", Status: StatusFail, Summary: "expired"},
	}}
	after := Report{Run: RunInfo{ID: "b"}, Results: []Result{
		{Probe: "http.check", Status: StatusFail, Summary: "HTTP 503", Metrics: map[string]interface{}{"status_code": 503, "total_ms": 12, "final_url": "https://b", "nested": []string{"y"}}},
		{Probe: "dns.lookup", Status: StatusPass, Summary: "ok", DurationMS: 40, Metrics: map[string]interface{}{"cname": "a."}},
		{Probe: "net.quality", Status: StatusPass, Summary: "new"},
		{Probe: "tls.check", Status: StatusPass, Summary: "renewed"},
	}}
	diff := diffReports("a.json", before, "b.json", after)
	if diff.Summary != (reportDiffSummary{Same: 1, Changed: 2, Added: 1, Removed: 1, Regressed: 1}) {
		t.Fatalf("summary = %#v", diff.Summary)
	}
	byProbe := map[string]reportDiffEntry{}
	for _, entry := range diff.Entries {
		byProbe[entry.Probe] = entry
	}
	http := byProbe["http.check"]
	if http.Change != changeChanged || !http.Regressed || len(http.Metrics) != 3 {
		t.Fatalf("http entry = %#v", http)
	}
	if http.Metrics[0].Key != "final_url" || http.Metrics[0].Delta != nil {
		t.Fatalf("string metric = %#v", http.Metrics[0])
	}
	if http.Metrics[2].Key != "total_ms" || http.Metrics[2].Delta == nil || *http.Metrics[2].Delta != -108 {
		t.Fatalf("numeric metric = %#v", http.Metrics[2])
	}
	if byProbe["tls.check"].Regressed || byProbe["tls.check"].Change != changeChanged {
		t.Fatalf("recovery must not count as regression: %#v", byProbe["tls.check"])
	}
	if byProbe["sockets"].Change != changeRemoved || byProbe["net.quality"].Change != changeAdded || byProbe["dns.lookup"].Change != changeSame {
		t.Fatalf("entries = %#v", diff.Entries)
	}
	dns := byProbe["dns.lookup"]
	if len(dns.Metrics) != 1 || dns.Metrics[0].Key != "duration_ms" || *dns.Metrics[0].Delta != 30 {
		t.Fatalf("duration delta = %#v", dns.Metrics)
	}
}

func TestPrintReportDiff(t *testing.T) {
	diff := reportDiff{
		Before: reportDiffSide{Path: "a.json", RunID: "aaaa", StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		After:  reportDiffSide{Path: "b.json", RunID: "bbbb", StartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		Entries: []reportDiffEntry{
			{Probe: "remote.server.update", Change: changeChanged, Regressed: true, BeforeStatus: StatusPass, AfterStatus: StatusFail, AfterSummary: "boom", Metrics: []metricDelta{{Key: "total_ms", Before: 120, After: 12, Delta: floatPointer(-108)}, {Key: "final_url", Before: "a", After: "b"}}},
			{Probe: "dns.lookup", Change: changeSame, BeforeStatus: StatusPass, AfterStatus: StatusPass},
		},
		Summary: reportDiffSummary{Changed: 1, Same: 1, Regressed: 1},
	}
	var output strings.Builder
	printReportDiff(&output, diff, false)
	text := output.String()
	for _, expected := range []string{"diff  a.json → b.json", "WORSE           server.update", "PASS → FAIL  boom", "total_ms                  120 → 12 (-108)", "final_url                 a → b", "STATUS SAME     dns.lookup", "1 status changed  ·  1 status same  ·  0 added  ·  0 removed  ·  1 worse"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("output %q does not contain %q", text, expected)
		}
	}
}

func TestRunReportDiffExitCodeAndJSON(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, results []Result) string {
		path := filepath.Join(dir, name)
		report := buildReport("test", time.Now(), nil, results, false)
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	before := write("before.json", []Result{{Probe: "tcp.check", Status: StatusPass, Metrics: map[string]interface{}{"connect_ms": 10}}})
	worse := write("worse.json", []Result{{Probe: "tcp.check", Status: StatusFail, Metrics: map[string]interface{}{"connect_ms": 900}}})
	output := filepath.Join(dir, "diff.json")
	if code := runReportDiff([]string{"--json", output, before, worse}); code != 1 {
		t.Fatalf("regression exit code = %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var diff reportDiff
	if err := json.Unmarshal(data, &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Summary.Regressed != 1 || len(diff.Entries) != 1 || len(diff.Entries[0].Metrics) != 1 || *diff.Entries[0].Metrics[0].Delta != 890 {
		t.Fatalf("diff = %#v", diff)
	}
	if code := runReportDiff([]string{"--json", output, worse, before}); code != 0 {
		t.Fatalf("recovery exit code = %d", code)
	}
	if code := runReportDiff([]string{before}); code != 2 {
		t.Fatalf("missing argument exit code = %d", code)
	}
	if code := runReportDiff([]string{"--json", output, before, filepath.Join(dir, "absent.json")}); code != 2 {
		t.Fatalf("missing file exit code = %d", code)
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestCertificateVerdict(t *testing.T) {
	if status, warning, err := certificateVerdict(90, 0); status != StatusPass || warning != "" || err != nil {
		t.Fatalf("healthy certificate = %v %q %#v", status, warning, err)
	}
	status, warning, err := certificateVerdict(10, 0)
	// 번역 template이 인자를 잃어도 알아채도록 남은 일수가 문구에 실제로 들어갔는지 본다.
	if status != StatusWarn || err != nil || !strings.Contains(warning, "10") {
		t.Fatalf("default warning = %v %q %#v", status, warning, err)
	}
	if status, warning, err := certificateVerdict(10, 14); status != StatusFail || warning != "" || err == nil || err.Kind != "expiry" {
		t.Fatalf("min-days failure = %v %q %#v", status, warning, err)
	}
	if status, _, err := certificateVerdict(20, 14); status != StatusWarn || err != nil {
		t.Fatalf("above min-days but under default warning = %v %#v", status, err)
	}
}

func TestCertificateExpiryLabelUsesUTCDate(t *testing.T) {
	expires := time.Date(2026, time.October, 5, 5, 30, 0, 0, time.FixedZone("KST", 9*60*60))
	if label := certificateExpiryLabel(expires); !strings.Contains(label, "2026-10-04 20:30 UTC") {
		t.Fatalf("certificate expiry label = %q", label)
	}
}

func TestHTTPStatusVerdict(t *testing.T) {
	if status, err := httpStatusVerdict(404, 404); status != StatusPass || err != nil {
		t.Fatalf("expected 404 = %v %#v", status, err)
	}
	if status, err := httpStatusVerdict(200, 204); status != StatusFail || err == nil || err.Kind != "status" {
		t.Fatalf("mismatch = %v %#v", status, err)
	}
	if status, err := httpStatusVerdict(404, 0); status != StatusWarn || err != nil {
		t.Fatalf("default 4xx = %v %#v", status, err)
	}
	if status, _ := httpStatusVerdict(503, 0); status != StatusFail {
		t.Fatalf("default 5xx = %v", status)
	}
}

func TestPrintReportDiffEntryKeepsColumnsAligned(t *testing.T) {
	entries := []reportDiffEntry{
		{Probe: "dns.lookup", Change: changeSame, AfterStatus: StatusPass},
		{Probe: "net.ping", Change: changeChanged, Regressed: true, BeforeStatus: StatusPass, AfterStatus: StatusFail, AfterSummary: "signal: killed: PING example.com\n64 bytes from example.com"},
	}
	column := -1
	for _, entry := range entries {
		var line, detail strings.Builder
		printReportDiffEntry(&line, &detail, entry, true)
		text := strings.TrimRight(line.String(), "\n")
		if strings.Contains(text, "64 bytes") {
			t.Fatalf("line must keep only the first summary line: %q", text)
		}
		probe := strings.TrimPrefix(entry.Probe, "remote.")
		start := liveWidth(text[:strings.Index(text, probe)])
		if column >= 0 && start != column {
			t.Fatalf("probe column = %d, want %d in %q", start, column, text)
		}
		column = start
	}
}

func TestLoadReportRejectsSchemaOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":"1.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReport(path); err == nil {
		t.Fatal("schema-only report accepted")
	}
	if code := runReportShow(path); code != 2 {
		t.Fatalf("show code = %d", code)
	}
	if code := runReportDiff([]string{path, path}); code != 2 {
		t.Fatalf("diff code = %d", code)
	}
}

func TestLoadReportValidation(t *testing.T) {
	valid := buildReport("test", time.Now(), map[string]interface{}{"url": "[REDACTED]"}, []Result{{Probe: "remote.check", Status: StatusPass}}, true)
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	var base map[string]interface{}
	if err := json.Unmarshal(data, &base); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(map[string]interface{})
	}{
		{"tool", func(m map[string]interface{}) { m["tool"].(map[string]interface{})["name"] = "other" }},
		{"version", func(m map[string]interface{}) { m["tool"].(map[string]interface{})["version"] = " " }},
		{"run id", func(m map[string]interface{}) { m["run"].(map[string]interface{})["id"] = "" }},
		{"time", func(m map[string]interface{}) { m["run"].(map[string]interface{})["started_at"] = "bad" }},
		{"run duration", func(m map[string]interface{}) { m["run"].(map[string]interface{})["duration_ms"] = -1 }},
		{"results missing", func(m map[string]interface{}) { delete(m, "results") }},
		{"results type", func(m map[string]interface{}) { m["results"] = map[string]interface{}{} }},
		{"summary null", func(m map[string]interface{}) { m["summary"] = nil }},
		{"summary negative", func(m map[string]interface{}) { m["summary"].(map[string]interface{})["fail"] = -1 }},
		{"summary mismatch", func(m map[string]interface{}) { m["summary"].(map[string]interface{})["pass"] = 4 }},
		{"probe", func(m map[string]interface{}) {
			m["results"].([]interface{})[0].(map[string]interface{})["probe"] = " "
		}},
		{"status", func(m map[string]interface{}) {
			m["results"].([]interface{})[0].(map[string]interface{})["status"] = "unknown"
		}},
		{"duration", func(m map[string]interface{}) {
			m["results"].([]interface{})[0].(map[string]interface{})["duration_ms"] = -1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var m map[string]interface{}
			json.Unmarshal(data, &m)
			test.edit(m)
			changed, _ := json.Marshal(m)
			path := filepath.Join(t.TempDir(), "report.json")
			os.WriteFile(path, changed, 0600)
			if _, err := loadReport(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, invalid := range []string{"null", "[]", "42", string(data) + "{}", string(data) + "garbage", string(data) + strings.Repeat(" ", reportSizeLimit)} {
		path := filepath.Join(t.TempDir(), "report.json")
		os.WriteFile(path, []byte(invalid), 0600)
		if _, err := loadReport(path); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	for _, results := range [][]Result{nil, {}, valid.Results, {{Probe: "duplicate", Status: StatusSkip}, {Probe: "duplicate", Status: StatusPass}}} {
		report := buildReport("test", time.Now(), nil, results, true)
		encoded, _ := json.Marshal(report)
		var m map[string]interface{}
		json.Unmarshal(encoded, &m)
		m["extension"] = true
		encoded, _ = json.Marshal(m)
		path := filepath.Join(t.TempDir(), "report.json")
		os.WriteFile(path, encoded, 0600)
		if _, err := loadReport(path); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "old.json")
	os.WriteFile(path, []byte(`{"schema_version":"2.0"}`), 0600)
	if _, err := loadReport(path); err == nil || !strings.Contains(err.Error(), "2.0") {
		t.Fatalf("schema error = %v", err)
	}
}

func TestReportDiffIdentity(t *testing.T) {
	before := Report{Target: map[string]interface{}{"url": "[REDACTED]", "host": "target-a"}, Host: map[string]interface{}{"hostname": "collector-a"}, Results: []Result{{Probe: "check", Status: StatusPass, DurationMS: 1}}}
	after := Report{Target: map[string]interface{}{"url": "other\n\x1b[2J", "host": 42}, Host: map[string]interface{}{"hostname": "collector-b"}, Results: []Result{{Probe: "check", Status: StatusPass, DurationMS: 2}}}
	diff := diffReports("a", before, "b", after)
	text := reportDiffIdentity(diff)
	for _, expected := range []string{"[REDACTED]", "target-a", "collector-a", "collector-b", T("cli.report.unavailable"), T("cli.report.identity_differs"), T("cli.report.status_semantics"), `other\n\x1b[2J`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("identity %q missing %q", text, expected)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatal("control sequence in header")
	}
	encoded, _ := json.Marshal(diff)
	var value map[string]interface{}
	json.Unmarshal(encoded, &value)
	side := value["before"].(map[string]interface{})
	for _, key := range []string{"path", "run_id", "started_at", "hostname", "target_url", "target_host"} {
		if _, ok := side[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if diff.Entries[0].Change != changeSame || diff.Summary.Regressed != 0 {
		t.Fatalf("status semantics changed: %#v", diff)
	}
	same := diff
	same.After = same.Before
	if strings.Contains(reportDiffIdentity(same), T("cli.report.identity_differs")) {
		t.Fatal("same identities flagged")
	}
	missing := reportDiff{}
	if strings.Contains(reportDiffIdentity(missing), T("cli.report.identity_differs")) {
		t.Fatal("missing identities flagged as mismatch")
	}
}

func TestReportDiffDifferentTargetsContinue(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json")}
	for i, path := range paths {
		report := buildReport("test", time.Now(), map[string]interface{}{"url": []string{"[REDACTED]", "https://other.invalid"}[i]}, []Result{{Probe: "check", Status: StatusPass, Metrics: map[string]interface{}{"scalar": i, "nested": map[string]interface{}{"value": i}}}}, false)
		report.Host = map[string]interface{}{"hostname": []string{"one", "two"}[i]}
		data, _ := json.Marshal(report)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "diff.json")
	if code := runReportDiff([]string{"--json", output, paths[0], paths[1]}); code != 0 {
		t.Fatalf("cross-target code = %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var diff reportDiff
	if err := json.Unmarshal(data, &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Summary.Same != 1 || diff.Summary.Changed != 0 || len(diff.Entries[0].Metrics) != 1 || diff.Before.TargetURL != "[REDACTED]" || diff.After.Hostname != "two" {
		t.Fatalf("diff = %#v", diff)
	}
}
