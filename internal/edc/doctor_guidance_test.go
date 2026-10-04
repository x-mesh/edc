package edc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorGuidanceObservedStages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		results  []Result
		commands []string
	}{
		{"mixed", []Result{{Probe: "http.check", Status: StatusFail, Summary: "http"}, {Probe: "dns.lookup", Status: StatusFail, Summary: "dns"}, {Probe: "tcp.check", Status: StatusPass}, {Probe: "tls.check", Status: StatusWarn, Summary: "expiry"}}, []string{"edc dns", "edc tls", "edc http"}},
		{"timeout", []Result{{Probe: "tcp.check", Status: StatusFail, Error: &DiagnosticError{Kind: "timeout"}, Summary: "deadline exceeded"}}, []string{"edc tcp"}},
		{"cancelled", []Result{{Probe: "dns.lookup", Status: StatusFail, Error: &DiagnosticError{Kind: "cancelled"}, Summary: "cancelled"}}, []string{"edc dns"}},
		{"http skip", []Result{{Probe: "tls.check", Status: StatusSkip}, {Probe: "http.check", Status: StatusPass}}, nil},
		{"extra", []Result{{Probe: "net.quality", Status: StatusFail}, {Probe: "endpoints.check", Status: StatusWarn}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := Report{Results: tc.results, Summary: summarize(tc.results)}
			before, _ := json.Marshal(report)
			var out bytes.Buffer
			printDoctorGuidance(&out, report)
			after, _ := json.Marshal(report)
			if !bytes.Equal(before, after) {
				t.Fatal("report mutated")
			}
			if len(tc.commands) == 0 && out.Len() != 0 {
				t.Fatalf("unexpected guidance: %s", out.String())
			}
			previous := -1
			for _, command := range tc.commands {
				index := strings.Index(out.String(), command)
				if index <= previous {
					t.Fatalf("missing or unordered %s: %s", command, out.String())
				}
				previous = index
			}
		})
	}
}

func TestDoctorGuidanceExcerpt(t *testing.T) {
	value := "\x1b[31mred\x1b[0m\nnext\tline\x00\u202e"
	if got := doctorGuidanceExcerpt(value); got != "red next line" {
		t.Fatalf("excerpt = %q", got)
	}
	if got := doctorGuidanceExcerpt(strings.Repeat("界", 200)); len([]rune(got)) != doctorGuidanceExcerptLimit {
		t.Fatalf("excerpt length = %d", len([]rune(got)))
	}
}

func TestDoctorCompletionGuidanceAndJSON(t *testing.T) {
	report := buildReport("fixture", time.Now(), map[string]interface{}{"input": "192.0.2.10;secret"}, []Result{
		{Probe: "dns.lookup", Status: StatusFail, Summary: "lookup 192.0.2.10\nfailed", Evidence: []Evidence{{Label: "address", Value: "2001:db8::1"}}},
		{Probe: "net.quality", Status: StatusSkip}, {Probe: "endpoints.check", Status: StatusWarn},
	}, true)
	before, _ := json.Marshal(report)
	var guidance, tail bytes.Buffer
	printDoctorGuidance(&guidance, report)
	printDoctorTail(&tail, report, false, false)
	var code int
	plain := captureSchedStdout(t, func() { code = emitDoctor(commonOptions{}, report) })
	if code != 1 || !strings.HasSuffix(plain, guidance.String()) || !strings.HasSuffix(tail.String(), guidance.String()) {
		t.Fatalf("completion mismatch: code=%d plain=%q tail=%q", code, plain, tail.String())
	}
	if strings.Contains(guidance.String(), "192.0.2.10") || strings.Contains(guidance.String(), "2001:db8::1") || strings.Contains(guidance.String(), "secret") {
		t.Fatalf("guidance leaked target: %s", guidance.String())
	}
	for _, path := range []string{"-", filepath.Join(t.TempDir(), "report.json")} {
		output := captureSchedStdout(t, func() { code = emitDoctor(commonOptions{jsonPath: path}, report) })
		if code != 1 {
			t.Fatalf("JSON exit=%d", code)
		}
		if path != "-" {
			if output != "" {
				t.Fatalf("JSON file stdout=%q", output)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			output = string(data)
		}
		var got Report
		if err := json.Unmarshal([]byte(output), &got); err != nil {
			t.Fatalf("JSON invalid: %v: %q", err, output)
		}
		marshaled, _ := json.Marshal(got)
		if !bytes.Equal(before, marshaled) {
			t.Fatalf("JSON report changed: %s", marshaled)
		}
	}
	after, _ := json.Marshal(report)
	if !bytes.Equal(before, after) {
		t.Fatal("completion mutated report")
	}
}
