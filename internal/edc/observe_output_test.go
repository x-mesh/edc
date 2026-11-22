package edc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func observeOutputCompact(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '·' {
			return -1
		}
		return r
	}, ansi.Strip(text))
}

func assertObserveOutputWidth(t *testing.T, text string, width int) {
	t.Helper()
	for _, line := range strings.Split(ansi.Strip(text), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "next:") {
			continue
		}
		if got := liveWidth(line); got > width {
			t.Fatalf("width %d got %d: %q", width, got, line)
		}
	}
}

func TestObserveOutputSelectsOnlyOneSupportedFamily(t *testing.T) {
	for _, family := range []string{"proc", "svc", "jobs", "changes"} {
		if !observeOutputResults([]Result{{Probe: family}, {Probe: family + ".details"}}) {
			t.Fatalf("family %s not selected", family)
		}
	}
	for _, results := range [][]Result{nil, {{Probe: "tcp.check"}}, {{Probe: "remote.proc.state"}}, {{Probe: "proc.state"}, {Probe: "jobs.cron"}}, {{Probe: "proc.state"}, {Probe: "tcp.check"}}, {{Probe: "procfoo.state"}}} {
		if observeOutputResults(results) {
			t.Fatalf("unsupported results selected: %+v", results)
		}
	}
}

func TestObserveOutputPreservesFullFieldsAcrossWidths(t *testing.T) {
	for _, family := range []string{"proc", "svc", "jobs", "changes"} {
		result := Result{Probe: family + ".fixture", Status: StatusFail, Summary: strings.Repeat("long-name/", 35) + "SUMMARY_END 4194304", Error: &DiagnosticError{Kind: "read_failed", Message: strings.Repeat("path/", 60) + "ERROR_END"}, Next: "edc proc 'quoted path with spaces and apostrophes'", Warnings: []string{strings.Repeat("warning ", 30) + "WARNING_END"}, Evidence: []Evidence{{Label: "프로세스🙂 é", Value: strings.Repeat("한글🙂é", 40) + "EVIDENCE_END\nline two 95.00%"}}}
		before, _ := json.Marshal(result)
		for _, width := range []int{32, 48, 80, 140} {
			for _, color := range []bool{false, true} {
				var output strings.Builder
				printObserveTerminal(&output, []Result{result}, false, infoStyle{textStyle{width: width, color: color}})
				text := output.String()
				compact := observeOutputCompact(text)
				for _, want := range []string{"FAIL", family + ".fixture", "SUMMARY_END4194304", "ERROR_END", "read_failed", "WARNING_END", "EVIDENCE_END", "linetwo95.00%", "프로세스🙂é"} {
					if !strings.Contains(compact, want) {
						t.Fatalf("family %s width %d lost %q:\n%s", family, width, want, ansi.Strip(text))
					}
				}
				if !strings.Contains(ansi.Strip(text), result.Next) {
					t.Fatal("copyable command gained a forced line break")
				}
				if !color && strings.Contains(text, "\x1b") {
					t.Fatal("plain output contains escapes")
				}
				assertObserveOutputWidth(t, text, width)
			}
		}
		after, _ := json.Marshal(result)
		if string(before) != string(after) {
			t.Fatal("formatting mutated the result")
		}
	}
}

func TestObserveOutputReusesTheExistingDetailPolicy(t *testing.T) {
	probe := "proc.fixture_policy"
	result := Result{Probe: probe, Status: StatusPass, Summary: "summary", Next: "edc proc 'a long quoted target'", Evidence: []Evidence{{Label: "hidden evidence", Value: "PRIVATE_DETAIL_END"}}, Warnings: []string{"visible warning"}}
	var hidden strings.Builder
	printObserveResult(&hidden, result, false, infoStyle{textStyle{width: 48}})
	if strings.Contains(hidden.String(), "PRIVATE_DETAIL_END") || !strings.Contains(hidden.String(), "visible warning") {
		t.Fatal("evidence visibility changed")
	}
	warnEvidenceProbes[probe] = true
	defer delete(warnEvidenceProbes, probe)
	result.Status = StatusWarn
	for _, verbose := range []bool{false, true} {
		var expected, output strings.Builder
		details := result
		details.Next = ""
		printResultDetail(&expected, details, verbose, false)
		printObserveResult(&output, result, verbose, infoStyle{textStyle{width: 32}})
		if !strings.Contains(observeOutputCompact(output.String()), observeOutputCompact(expected.String())) {
			t.Fatalf("existing detail output was lost:\n%s\n%s", expected.String(), output.String())
		}
		if result.Next == "" {
			t.Fatal("source Next was cleared")
		}
	}
}

func TestObserveOutputSanitizesExternalControls(t *testing.T) {
	result := Result{Probe: "proc.fixture", Status: StatusFail, Summary: "name\x1b[2J\tSUMMARY_END", Error: &DiagnosticError{Kind: "error", Message: "cause\x1b[2JERROR_END"}, Warnings: []string{"warn\x1b[2JWARNING_END"}, Evidence: []Evidence{{Label: "label\x1b[2J", Value: "body\x1b[2JEVIDENCE_END"}}}
	var output strings.Builder
	printObserveTerminal(&output, []Result{result}, false, infoStyle{textStyle{width: 32}})
	if strings.Contains(output.String(), "\x1b") {
		t.Fatalf("external escape emitted: %q", output.String())
	}
	for _, want := range []string{"SUMMARY_END", "ERROR_END", "WARNING_END", "EVIDENCE_END"} {
		if !strings.Contains(observeOutputCompact(output.String()), want) {
			t.Fatalf("tail %s lost", want)
		}
	}
	assertObserveOutputWidth(t, output.String(), 32)
}

func TestObserveOutputRedactsBeforeWrappingWithoutMutatingTypedData(t *testing.T) {
	events := []changeEvent{{Time: time.Unix(10, 0), Kind: "etc", Name: "/etc/192.0.2.123.conf", Detail: "peer 2001:db8::123"}}
	result := Result{Probe: "changes.etc", Status: StatusFail, Summary: strings.Repeat("summary ", 20) + "192.0.2.123", Error: &DiagnosticError{Kind: "read", Message: "peer 2001:db8::123"}, Warnings: []string{"peer 192.0.2.123"}, Evidence: []Evidence{{Label: "peer 2001:db8::123", Value: "peer 192.0.2.123"}}, Metrics: map[string]interface{}{"events": events}}
	results := []Result{result}
	before, _ := json.Marshal(results)
	extracted := changesEventsOf(results)
	redacted, err := observeOutputRedactedResults(results)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	printObserveTerminal(&output, redacted, false, infoStyle{textStyle{width: 32}})
	output.WriteString(formatObserveChangesTable(extracted, true, infoStyle{textStyle{width: 32}}, true))
	compact := observeOutputCompact(output.String())
	for _, raw := range []string{"192.0.2.123", "2001:db8::123"} {
		if strings.Contains(compact, raw) {
			t.Fatalf("raw address survived wrapping: %s", compact)
		}
	}
	if !strings.Contains(compact, "<ip:") || len(extracted) != 1 {
		t.Fatal("redaction or typed event extraction failed")
	}
	after, _ := json.Marshal(results)
	if string(before) != string(after) {
		t.Fatal("display redaction mutated source fields")
	}
	if _, ok := results[0].Metrics["events"].([]changeEvent); !ok {
		t.Fatal("typed metrics were converted")
	}
}

func TestObserveOutputLeavesPipesJSONAndExitCodesUnchanged(t *testing.T) {
	results := []Result{{Probe: "proc.fixture", Status: StatusFail, Summary: strings.Repeat("long summary ", 40), Error: &DiagnosticError{Kind: "read", Message: "cause"}, Evidence: []Evidence{{Label: "source", Value: "data"}}}}
	var legacy, zeroWidth strings.Builder
	printTerminalWithColor(&legacy, results, true, false)
	printObserveTerminal(&zeroWidth, results, true, infoStyle{})
	if legacy.String() != zeroWidth.String() {
		t.Fatal("zero-width legacy output changed")
	}
	file, err := os.CreateTemp(t.TempDir(), "pipe-output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	report := Report{Results: results, Summary: summarize(results)}
	if code := emitTo(file, commonOptions{verbose: true}, report); code != 1 {
		t.Fatalf("text exit code %d", code)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != legacy.String() {
		t.Fatalf("pipe output changed: %v\n%s", err, data)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if code := emitTo(file, commonOptions{jsonPath: path}, report); code != 1 {
		t.Fatalf("JSON exit code %d", code)
	}
	var expected strings.Builder
	if err := writeJSON(&expected, report); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != expected.String() {
		t.Fatalf("JSON changed: %v %s", err, data)
	}
}

func TestObserveOutputDoesNotChangeGenericOrTUIFormatters(t *testing.T) {
	results := []Result{{Probe: "svc.fixture", Status: StatusPass, Summary: "state", Evidence: []Evidence{{Label: "detail", Value: strings.Repeat("value ", 80)}}}}
	var before, after strings.Builder
	printTerminalWithColor(&before, results, true, false)
	var wrapped strings.Builder
	printObserveTerminal(&wrapped, results, true, infoStyle{textStyle{width: 32}})
	printTerminalWithColor(&after, results, true, false)
	if before.String() != after.String() {
		t.Fatal("direct formatter state changed")
	}
	if observeOutputCompact(wrapped.String()) == "" {
		t.Fatal("wrapped output empty")
	}
}

func TestObserveOutputFooterKeepsCountPairs(t *testing.T) {
	results := []Result{{Probe: "proc.fixture", Status: StatusPass, Summary: "observed"}, {Probe: "proc.fixture", Status: StatusFail, Summary: "observed"}}
	for _, width := range []int{32, 48, 80, 140} {
		for _, color := range []bool{false, true} {
			var output strings.Builder
			printObserveTerminal(&output, results, false, infoStyle{textStyle{width: width, color: color}})
			for _, pair := range []string{"1 pass", "0 warn", "1 fail", "0 skip"} {
				if !strings.Contains(ansi.Strip(output.String()), pair) {
					t.Fatalf("width %d color %t split count pair %q:\n%s", width, color, pair, ansi.Strip(output.String()))
				}
			}
		}
	}
}
