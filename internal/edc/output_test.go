package edc

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestColorResultLineUsesTheBadgeAndKeepsTheColumns(t *testing.T) {
	result := Result{Probe: "tcp.check", Status: StatusPass, Summary: "example.com:443 connected"}
	plain := formatResultLine(result, false)
	colored := formatResultLine(result, true)
	if !strings.HasPrefix(colored, richExpect("\033[1;30;42m PASS \033[0m  \033[1mtcp.check")) {
		t.Fatalf("colored = %q", colored)
	}
	// 배지가 두 칸 넓으므로 label과 summary는 평문보다 두 칸 뒤에서 시작하고, 나머지는 평문과 같다.
	if got, want := ansi.Strip(colored), " "+strings.Replace(plain, "PASS", "PASS ", 1); got != want {
		t.Fatalf("stripped = %q, want %q", got, want)
	}
}

func TestColorPendingRowsShareTheColumnWithFinishedRows(t *testing.T) {
	done := formatResultLine(Result{Probe: "dns.lookup", Status: StatusPass}, true)
	column := func(line, name string) int { return liveWidth(line[:strings.Index(line, name)]) }
	doctor := newDoctorModel("example.com", []string{"dns.lookup"}, true, false, nil)
	if pending := doctor.View().Content; column(pending, "dns.lookup") != column(done, "dns.lookup") {
		t.Fatalf("doctor pending %q and done %q do not share the name column", pending, done)
	}
	probe := newProbeModel("dns.lookup", "example.com", "", true, false, nil)
	if pending := probe.View().Content; column(pending, "dns.lookup") != column(done, "dns.lookup") {
		t.Fatalf("probe pending %q and done %q do not share the name column", pending, done)
	}
}

func TestColorFailurePanel(t *testing.T) {
	results := []Result{{
		Probe: "tcp.check", Status: StatusFail, Summary: "refused",
		Error:    &DiagnosticError{Kind: "connection_refused", Message: "dial tcp 127.0.0.1:1: connect: connection refused"},
		Evidence: []Evidence{{Label: "command output", Value: "first\nsecond\n"}},
	}}
	var output strings.Builder
	printTerminalWithColor(&output, results, false, true)
	colored := output.String()
	for _, code := range []string{richExpect("\033[31m╭─ \033[0m\033[1;31mERROR\033[0m"), richExpect("\033[33mconnection_refused\033[0m"), richExpect("\033[90mcause\033[0m"), richExpect("\033[31m╰─\033[0m")} {
		if !strings.Contains(colored, code) {
			t.Errorf("panel lacks %q: %q", code, colored)
		}
	}
	stripped := ansi.Strip(colored)
	want := "╭─ ERROR  tcp.check  connection_refused ───\n" +
		"│ cause  dial tcp 127.0.0.1:1: connect: connection refused\n" +
		"│\n│ command output\n│   first\n│   second\n╰─\n"
	if !strings.Contains(stripped, want) {
		t.Fatalf("stripped = %q, want it to contain %q", stripped, want)
	}
}

func TestColorFailurePanelWrapsTheRendererLines(t *testing.T) {
	richDetailRenderers["rich.fail"] = func(_ Result, width int) []string {
		return []string{"row one", strings.Repeat("x", width)}
	}
	defer delete(richDetailRenderers, "rich.fail")
	result := Result{Probe: "rich.fail", Status: StatusFail, Error: &DiagnosticError{Kind: "status", Message: "HTTP 500"}, Evidence: []Evidence{{Label: "requests", Value: "raw"}}}
	var output strings.Builder
	printFailureBox(&output, "rich.fail", result, true)
	stripped := ansi.Strip(output.String())
	if !strings.Contains(stripped, "│ cause  HTTP 500\n│\n│ row one\n") || strings.Contains(stripped, "raw") {
		t.Fatalf("stripped = %q", stripped)
	}
	for _, line := range strings.Split(strings.TrimRight(output.String(), "\n"), "\n") {
		if liveWidth(line) > terminalWidth() {
			t.Errorf("line is %d cells, wider than %d: %q", liveWidth(line), terminalWidth(), line)
		}
	}
}

func TestColorNextAndWarningLines(t *testing.T) {
	result := Result{Probe: "disk.check", Status: StatusWarn, Next: "edc disk grow /", Warnings: []string{"low space"}}
	var plain, colored strings.Builder
	printResultDetail(&plain, result, false, false)
	printResultDetail(&colored, result, false, true)
	if plain.String() != "      next: edc disk grow /\n      warning: low space\n" {
		t.Fatalf("plain = %q", plain.String())
	}
	if ansi.Strip(colored.String()) != plain.String() {
		t.Fatalf("stripped colored = %q", ansi.Strip(colored.String()))
	}
	for _, code := range []string{richExpect("\033[90mnext:\033[0m"), richExpect("\033[1;36medc disk grow /\033[0m"), richExpect("\033[1;33mwarning:\033[0m")} {
		if !strings.Contains(colored.String(), code) {
			t.Errorf("colored lacks %q: %q", code, colored.String())
		}
	}
}

func TestColorEvidenceIsAGrayPanel(t *testing.T) {
	result := Result{Probe: "net.route", Status: StatusPass, Evidence: []Evidence{{Label: "output", Value: "route to: 192.0.2.1\ngateway: 192.0.2.254"}}}
	var output strings.Builder
	printResultDetail(&output, result, true, true)
	if got, want := ansi.Strip(output.String()), "╭─ output  net.route ───\n│ route to: 192.0.2.1\n│ gateway: 192.0.2.254\n╰─\n"; got != want {
		t.Fatalf("stripped = %q, want %q", got, want)
	}
	if !strings.Contains(output.String(), richExpect("\033[90m╭─ \033[0m\033[1;35moutput\033[0m")) {
		t.Fatalf("panel = %q", output.String())
	}
}

// 막대를 둘 칸이 0보다 큰 상태 수보다 적으면 막대를 뺀다. 구간마다 한 칸을 주면 줄이 폭을 넘는다.
func TestColorSummaryNeverOverflowsANarrowLine(t *testing.T) {
	results := []Result{{Status: StatusPass}, {Status: StatusWarn}, {Status: StatusFail}}
	text := "1 pass  1 warn  1 fail  0 skip"
	width := len(text) + 3
	if line := richResultCounts(results, width); liveWidth(line) > width {
		t.Fatalf("summary is %d cells, wider than %d: %q", liveWidth(line), width, ansi.Strip(line))
	}
}

// 보고를 stderr로 보내면 stdout이 아니라 그 파일의 폭을 잰다. 터미널이 아닌 파일은 기본 폭이다.
func TestWriterWidthMeasuresTheWriter(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "width")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got := writerWidth(file); got != remoteDefaultWidth {
		t.Errorf("file width = %d, want %d", got, remoteDefaultWidth)
	}
	if got := writerWidth(&strings.Builder{}); got != terminalWidth() {
		t.Errorf("buffer width = %d, want the stdout width %d", got, terminalWidth())
	}
}

func TestColorSummaryLine(t *testing.T) {
	results := []Result{{Status: StatusPass}, {Status: StatusPass}, {Status: StatusFail}}
	var plain, colored strings.Builder
	printResultSummary(&plain, results, false)
	printResultSummary(&colored, results, true)
	if plain.String() != "\n2 pass  ·  0 warn  ·  1 fail  ·  0 skip\n" {
		t.Fatalf("plain = %q", plain.String())
	}
	stripped := ansi.Strip(colored.String())
	if !strings.HasPrefix(stripped, "\n"+strings.Repeat("█", resultCountsBarWidth)+"  ") || !strings.HasSuffix(stripped, "  2 pass  0 warn  1 fail  0 skip\n") {
		t.Fatalf("stripped = %q", stripped)
	}
	for _, code := range []string{richExpect("\033[1;32m2 pass\033[0m"), richExpect("\033[90m0 warn\033[0m"), richExpect("\033[1;31m1 fail\033[0m"), richExpect("\033[90m0 skip\033[0m"), strings.TrimSuffix(richFill("████", richGreen), richReset)} {
		if !strings.Contains(colored.String(), code) {
			t.Errorf("summary lacks %q: %q", code, colored.String())
		}
	}
	if line := richResultCounts(results, 40); liveWidth(line) > 40 {
		t.Errorf("narrow summary is %d cells: %q", liveWidth(line), line)
	}
	if line := ansi.Strip(richResultCounts(nil, 80)); line != "0 pass  0 warn  0 fail  0 skip" {
		t.Errorf("empty summary = %q", line)
	}
}

// 결과가 하나면 막대가 한 색으로 꽉 차서 화면만 차지한다. 개수 글자만 남긴다.
func TestColorSummaryOmitsTheBarForOneResult(t *testing.T) {
	var colored strings.Builder
	printResultSummary(&colored, []Result{{Probe: "http.check", Status: StatusPass}}, true)
	if stripped := ansi.Strip(colored.String()); stripped != "\n1 pass  0 warn  0 fail  0 skip\n" {
		t.Errorf("stripped = %q", stripped)
	}
}

// -v 없이 evidence를 보이는 probe는 각 probe 파일이 등록한다. 공통 출력 함수는 이름을 따로 알지 않는다.
func TestEvidenceShownProbesAreRegisteredByTheProbes(t *testing.T) {
	for _, probe := range []string{endpointProbeID, "dns.compare", "http.check"} {
		if !evidenceShownProbes[probe] {
			t.Errorf("%s must show its evidence without -v", probe)
		}
	}
	shown := Result{Probe: "dns.compare", Status: StatusPass, Evidence: []Evidence{{Label: "answers", Value: "1.1.1.1"}}}
	hidden := Result{Probe: "dns.lookup", Status: StatusPass, Evidence: []Evidence{{Label: "answers", Value: "1.1.1.1"}}}
	var withEvidence, withoutEvidence strings.Builder
	printResultDetail(&withEvidence, shown, false, false)
	printResultDetail(&withoutEvidence, hidden, false, false)
	if !strings.Contains(withEvidence.String(), "1.1.1.1") || withoutEvidence.Len() != 0 {
		t.Errorf("shown = %q, hidden = %q", withEvidence.String(), withoutEvidence.String())
	}
}
