package edc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
