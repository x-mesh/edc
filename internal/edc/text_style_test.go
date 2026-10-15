package edc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// assertSameText는 색을 쓴 출력에서 escape를 빼면 평문과 같은지 본다. 색을 쓰면 터미널 폭(test에서는 기본 폭)을
// 넘는 줄만 "…"로 자르므로 그런 줄은 자른 평문과 비교한다.
func assertSameText(t *testing.T, plain, colored string) {
	t.Helper()
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("plain output has an escape: %q", plain)
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("colored output has no escape: %q", colored)
	}
	plainLines, coloredLines := strings.Split(plain, "\n"), strings.Split(colored, "\n")
	if len(plainLines) != len(coloredLines) {
		t.Fatalf("line count %d != %d:\n%s\n---\n%s", len(plainLines), len(coloredLines), plain, ansi.Strip(colored))
	}
	width := terminalWidth()
	for index, line := range coloredLines {
		got, want := ansi.Strip(line), plainLines[index]
		if got != want && got != ansi.Truncate(want, width, "…") {
			t.Fatalf("line %d = %q, want %q", index, got, want)
		}
		if liveWidth(got) > width {
			t.Fatalf("line %d is wider than %d: %q", index, width, got)
		}
	}
}

func richCode(color string) string { return strings.TrimSuffix(richSGR("x", color), "x"+richReset) }

func TestInfoColorKeepsTextAndColorsOnlyAboveNormal(t *testing.T) {
	details := hostDetails{Hostname: "host", Machine: "arm64", Model: "M4", Cores: 4, MemoryTotal: 8 << 30, Load: [3]float64{0.5, 3.5, 5}, SwapTotal: 100, SwapUsed: 96}
	interfaces := []interfaceDetails{{Name: "en0", Address: "192.0.2.1", Mask: "255.255.255.0", Gateway: "192.0.2.254"}}
	disks := []diskDetails{{Mount: "/", Device: "/dev/disk1", Total: 100, Used: 97, Percent: 97}}
	public := &publicNetworkInfo{IP: "192.0.2.9", Org: "org"}
	render := func(color bool) string {
		var output strings.Builder
		printInfo(&output, "test", details, interfaces, disks, public, infoSignals{}, color, false)
		return output.String()
	}
	plain, colored := render(false), render(true)
	assertSameText(t, plain, colored)
	for _, want := range []string{
		richPaint("🖥️  System Information", richMagenta, true),
		richDim("├──") + " " + richDim("Hostname:") + " host",
		// 4 core에서 load 0.5는 정상, 3.5는 경고, 5는 위험이다.
		richDim("Load:") + " 0.50/" + richPaint("3.50", richYellow, false) + "/" + richPaint("5.00", richRed, false),
		richPaint("(96.00%)", richRed, false),
		richFill(strings.Repeat(diskBarFull, 19), richRed),
		richDim("G/W:") + " 192.0.2.254",
	} {
		if !strings.Contains(colored, want) {
			t.Fatalf("missing %q in\n%s", want, colored)
		}
	}
}

func TestInfoResourcesColorStates(t *testing.T) {
	capabilities := []infoCapability{{"Process I/O", "available", "ok"}, {"PSI", "unsupported", "Linux-only"}, {"CPU wait / I/O latency", "permission required", "missing CAP_BPF"}}
	render := func(color bool) string {
		var output strings.Builder
		printInfoResources(&output, infoMemory{Total: 8 << 30, Available: 1 << 28}, nil, infoSignals{}, nil, fmt.Errorf("process read failed"), capabilities, color, false)
		return output.String()
	}
	plain, colored := render(false), render(true)
	assertSameText(t, plain, colored)
	for _, want := range []string{
		richPaint("(96.88%)", richRed, false),
		richPaint("Unavailable: process read failed", richYellow, false),
		richPaint("available", richGreen, false),
		richDim("unsupported"),
		richPaint("permission required", richYellow, false),
	} {
		if !strings.Contains(colored, want) {
			t.Fatalf("missing %q in\n%s", want, colored)
		}
	}
}

func TestInfoListenColorKeepsTextAndHighlightsTheCommand(t *testing.T) {
	var sockets []listenSocket
	for port := 1; port <= infoListenRows+2; port++ {
		sockets = append(sockets, listenSocket{Proto: "tcp", Address: "0.0.0.0", Port: 8000 + port, Process: "server", PID: "42"})
	}
	result := Result{Metrics: map[string]interface{}{"sockets": sockets}, Warnings: []string{"partial"}}
	var plain, colored strings.Builder
	printInfoListen(&plain, result, true, false, false)
	printInfoListen(&colored, result, true, true, false)
	assertSameText(t, plain.String(), colored.String())
	for _, want := range []string{richPaint("edc listen", richCyan, true), richPaint("Warning: partial", richYellow, false), richDim("tcp ")} {
		if !strings.Contains(colored.String(), want) {
			t.Fatalf("missing %q in\n%s", want, colored.String())
		}
	}
}

func TestNetworkHealthColorKeepsText(t *testing.T) {
	var plain, colored strings.Builder
	printInfoNetworkHealth(&plain, nil, false, false)
	printInfoNetworkHealth(&colored, nil, true, false)
	assertSameText(t, plain.String(), colored.String())
}

func TestWhereColorKeepsTextAndMarksUnreachable(t *testing.T) {
	report := whereReport{
		Location: whereLocation{PublicIP: "192.0.2.1", Country: "KR", City: "Seoul", Interface: "en0", LocalAddress: "10.0.0.2", Gateway: "10.0.0.1", BehindNAT: true},
		Regions: []whereMeasurement{
			{City: "Seoul", Provider: "aws", Region: "ap-northeast-2", Min: 3 * time.Millisecond, Median: 4 * time.Millisecond, Jitter: time.Millisecond},
			{City: "Tokyo", Provider: "gcp", Region: "asia-northeast1", Error: "timeout"},
		},
	}
	for _, verbose := range []bool{false, true} {
		var plain, colored strings.Builder
		printWhere(&plain, report, false, verbose)
		printWhere(&colored, report, true, verbose)
		assertSameText(t, plain.String(), colored.String())
		if !strings.Contains(colored.String(), richPaint(T("observe.where.heading.nearest"), richMagenta, true)) {
			t.Fatalf("heading is not magenta bold:\n%s", colored.String())
		}
	}
	var colored strings.Builder
	printWhere(&colored, report, true, false)
	if !strings.Contains(colored.String(), richPaint(padLeft(T("observe.where.unreachable"), whereValueWidth()), richYellow, false)) {
		t.Fatalf("unreachable region is not yellow:\n%s", colored.String())
	}
}

func TestWatchSampleColorUsesBadgeAndThresholds(t *testing.T) {
	at := time.Date(2026, 10, 15, 1, 2, 3, 4_000_000, time.UTC)
	ttfb := int64(900)
	sample := watchSample{Time: at, Status: StatusWarn, HTTPStatus: 404, BodyBytes: 12, DurationMS: 1500, TTFBMS: &ttfb, Error: "slow"}
	got := formatWatchSample(sample, textStyle{color: true})
	want := richDim(at.Local().Format("15:04:05.000")) + "  " + richStatusBadge(StatusWarn) + "  " + richDim("HTTP") + " " + richPaint("404", richYellow, false) +
		"  12 " + richDim("bytes") + "  " + richPaint("1500ms", richYellow, false) + "  " + richDim("TTFB") + " 900ms  " + richPaint("slow", richRed, false)
	if got != want {
		t.Fatalf("sample = %q, want %q", got, want)
	}
	sample.DurationMS = 80
	if got := formatWatchSample(sample, textStyle{color: true}); !strings.Contains(got, "  80ms  ") {
		t.Fatalf("normal latency must keep the default color: %q", got)
	}
}

func TestTextStyleWrapKeepsEveryPart(t *testing.T) {
	parts := []string{"12:00:00.000", "first", "second", "third"}
	got := textStyle{color: true, width: 26}.wrap(parts, "  ", "    ")
	if got != "12:00:00.000  first\n    second  third" {
		t.Fatalf("wrap = %q", got)
	}
	if got := (textStyle{}).wrap(parts, "  ", "    "); got != strings.Join(parts, "  ") {
		t.Fatalf("plain wrap = %q", got)
	}
}

func TestWatchHelpColorMatchesTheCommandHelp(t *testing.T) {
	for _, mode := range []string{"http", "fs"} {
		doc, usage, options := watchSubcommandHelp(mode)
		var colored bytes.Buffer
		printRichWatchSubcommandHelp(&colored, mode, doc, usage, options)
		text := colored.String()
		if !strings.HasPrefix(text, richBadge("edc watch "+mode, richMagenta)+" "+richBold(doc.summary())+"\n\n") || !strings.Contains(text, richPanelTop(T("help.options_label"), "", "")) {
			t.Fatalf("%s help header = %q", mode, text)
		}
		width := optionFlagWidth(options)
		for _, option := range options {
			if !strings.Contains(text, richPanelLine(richPaint(padRight(option.flag, width), richCyan, true), "")) {
				t.Fatalf("%s help misses the bold cyan %q:\n%s", mode, option.flag, text)
			}
		}
		if hasCommon := strings.Contains(text, richPanelTop(T("help.common_options_label"), "", "")); hasCommon != (mode == "http") {
			t.Fatalf("%s help common options = %v", mode, hasCommon)
		}
	}
}

func TestFSWatchColorLines(t *testing.T) {
	at := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	stamp := richDim(at.Local().Format("15:04:05.000"))
	for event, color := range map[string]string{"create": richGreen, "modify": richCyan, "remove": richRed, "rename": richYellow} {
		var colored, plain bytes.Buffer
		record := fsWatchRecord{Type: "event", Time: at, Event: event, Path: "a.txt"}
		writeFSWatchRecord(&colored, record, false, textStyle{color: true})
		writeFSWatchRecord(&plain, record, false, textStyle{})
		if want := stamp + "  " + richPaint(fmt.Sprintf("%-7s", strings.ToUpper(event)), color, false) + `  "a.txt"` + "\n"; colored.String() != want {
			t.Fatalf("%s line = %q, want %q", event, colored.String(), want)
		}
		if ansi.Strip(colored.String()) != plain.String() {
			t.Fatalf("%s color changed the text: %q", event, colored.String())
		}
	}
	code := 3
	var colored, plain bytes.Buffer
	result := fsWatchRecord{Type: "action_result", Time: at, Rule: "build", Status: "failed", ExitCode: &code, DurationMS: 5, Output: "boom\n", Error: "exit status 3"}
	writeFSWatchRecord(&colored, result, false, textStyle{color: true})
	writeFSWatchRecord(&plain, result, false, textStyle{})
	assertSameText(t, plain.String(), colored.String())
	for _, want := range []string{richPaint("DONE", richRed, false), "exit " + richPaint("3", richRed, false), richDim(`          "build" |`)} {
		if !strings.Contains(colored.String(), want) {
			t.Fatalf("missing %q in %q", want, colored.String())
		}
	}
}

// 색을 써도 기록 줄은 평문처럼 터미널이 접게 둔다. 자르면 파일 이름과 실행한 명령을 잃는다.
func TestFSWatchColorRecordsKeepLongLines(t *testing.T) {
	at := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	style := textStyle{color: true, width: 40}
	for _, record := range []fsWatchRecord{
		{Type: "event", Time: at, Event: "modify", Path: "src/a/very/long/directory/name/file.go"},
		{Type: "action_start", Time: at, Rule: "build", Path: "src/file.go", Command: []string{"go", "test", "./internal/edc/...", "-run", "TestSomething"}},
	} {
		var colored, plain bytes.Buffer
		writeFSWatchRecord(&colored, record, false, style)
		writeFSWatchRecord(&plain, record, false, textStyle{})
		if ansi.Strip(colored.String()) != plain.String() {
			t.Fatalf("%s line = %q, want %q", record.Type, ansi.Strip(colored.String()), plain.String())
		}
	}
}

func TestFSWatchStatusColorIsGray(t *testing.T) {
	clock := time.Unix(1000, 0)
	var out bytes.Buffer
	status := newFSWatchTestStatus(&out, 200, &clock, false)
	status.Write([]byte("line\n"))
	if !strings.Contains(out.String(), "\x1b[2m─ 00:00") {
		t.Fatalf("NO_COLOR status line must stay faint: %q", out.String())
	}
	out.Reset()
	status.color = true
	status.tick()
	if !strings.Contains(out.String(), richCode(richGray)+"─ 00:00") {
		t.Fatalf("colored status line must be gray: %q", out.String())
	}
}

func TestChangesTableColorDimsHeaderAndTime(t *testing.T) {
	at := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	events := []changeEvent{{Time: at, Kind: "file", Name: "/etc/hosts", Detail: "modified"}}
	plain, colored := formatChangesTable(events, false, false), formatChangesTable(events, false, true)
	assertSameText(t, plain, colored)
	for _, line := range strings.Split(strings.TrimSuffix(colored, "\n"), "\n") {
		if !strings.HasPrefix(line, richCode(richGray)) {
			t.Fatalf("header and time cells must be gray: %q", line)
		}
	}
	if !strings.Contains(colored, "file") || strings.Contains(colored, richDim("file")) {
		t.Fatalf("values must keep the default color: %q", colored)
	}
}

func TestTextStyleTableDimsTheHeaderAndFitsRows(t *testing.T) {
	if got := (textStyle{color: true}).table("A  B\n1  2\n"); got != richDim("A  B")+"\n1  2\n" {
		t.Fatalf("table = %q", got)
	}
	if got := (textStyle{color: true, width: 6}).table("A  B\n1  2  long process\n"); got != richDim("A  B")+"\n1  2 …\n" {
		t.Fatalf("narrow table = %q", got)
	}
	if got := (textStyle{width: 6}).table("A  B\n1  2  long process\n"); got != "A  B\n1  2  long process\n" {
		t.Fatalf("plain table = %q", got)
	}
}

func TestSnapshotDiffHintColorsTheCommand(t *testing.T) {
	plain := snapshotDiffHint(textStyle{}, "x.json")
	index := strings.Index(plain, "edc ")
	if plain != T("snapshot.diff_hint", "x.json") || index < 0 {
		t.Fatalf("plain hint = %q", plain)
	}
	if got := snapshotDiffHint(textStyle{color: true}, "x.json"); got != richDim(plain[:index])+richPaint(plain[index:], richCyan, true) {
		t.Fatalf("colored hint = %q", got)
	}
}

func TestDoctorGuidanceColorKeepsText(t *testing.T) {
	results := []Result{{Probe: "dns.lookup", Status: StatusFail, Summary: "no such host", Error: &DiagnosticError{Kind: "dns"}}}
	report := Report{Results: results, Summary: summarize(results)}
	var plain, colored bytes.Buffer
	printDoctorGuidance(&plain, report, false)
	printDoctorGuidance(&colored, report, true)
	assertSameText(t, plain.String(), colored.String())
	for _, want := range []string{richPaint("edc dns lookup 'HOST'", richCyan, true), richPaint("fail", richRed, false), richPaint(T("observe.doctor.guidance_heading"), richMagenta, true)} {
		if !strings.Contains(colored.String(), want) {
			t.Fatalf("missing %q in\n%s", want, colored.String())
		}
	}
}
