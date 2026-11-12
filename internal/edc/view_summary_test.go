package edc

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// --stats는 화면과 같은 규칙으로 센다. 같은 파일을 화면에 다 담아 센 수와 같아야 한다.
func TestViewStatsMatchesTheScreen(t *testing.T) {
	for _, fixture := range []string{"spring.log", "access.log", "app.log", "auth.log", "goloop.log", "app.jsonl"} {
		summary, err := viewSummarize(strings.NewReader(strings.Join(readViewFixture(t, fixture), "\n") + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		screen := viewScreenWith(t, fixture, 140, 40).stats
		got, want := summary.stats, screen
		if got.lines != want.lines || got.failures != want.failures || got.classes != want.classes ||
			!viewSameCounts(got.levels, want.levels) || !viewSameCounts(got.paths, want.paths) || !viewSameCounts(got.clients, want.clients) ||
			!viewSameCounts(got.programs, want.programs) || !viewSameCounts(got.exceptions, want.exceptions) {
			t.Errorf("%s: stats %+v, screen %+v", fixture, got, want)
		}
		if summary.lines != len(readViewFixture(t, fixture)) {
			t.Errorf("%s: %d lines", fixture, summary.lines)
		}
	}
}

func viewSameCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, count := range left {
		if right[key] != count {
			return false
		}
	}
	return true
}

func TestViewStatsWritesTheReport(t *testing.T) {
	path := filepath.Join("testdata", "view", "spring.log")
	var out bytes.Buffer
	if err := runViewStats(&out, path, false); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{
		"spring.log  ·  format level\n",
		"lines     20\n",
		"events    9\n",
		"time      2026-11-12 13:51:11.246 → 2026-11-12 13:51:15.500\n",
		"errors    5  56%\n",
		"\nLEVEL\nerror  5  56%\nwarn   1  11%\n",
		"\nLAST ERROR\n2026-11-12 13:51:15.000 ERROR --- [main] c.i.m.i.Application : shutdown requested\n",
		// 화면 패널과 달리 예외 이름을 자르지 않는다.
		"\nTOP EXCEPTIONS\n2  IllegalArgumentException: Invalid URI path\n2  ScoreSyncerException: block N not found\n",
		"\nTOP PROGRAMS\n3  c.i.m.i.c.IconServiceManager\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report misses %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "TOP PATHS") {
		t.Errorf("an empty list is left out:\n%s", report)
	}
}

// 상위 목록은 10개까지 보이고, 나머지는 몇 개인지만 적는다.
func TestViewStatsCountsTheRestOfALongList(t *testing.T) {
	var input strings.Builder
	for index := range 13 {
		for range 13 - index {
			input.WriteString(`192.0.2.1 - - [26/Oct/2026:10:00:00 +0900] "GET /p` + string(rune('a'+index)) + ` HTTP/1.1" 200 1 "-" "x"` + "\n")
		}
	}
	summary, err := viewSummarize(strings.NewReader(input.String()))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := summary.write(&out, "access", false); err != nil {
		t.Fatal(err)
	}
	if report := out.String(); !strings.Contains(report, "13  /pa\n") || !strings.Contains(report, " 4  /pj\n    (3 more)\n") || strings.Contains(report, "/pk") {
		t.Fatalf("report:\n%s", report)
	}
}

// 요청마다 다른 path가 끝없이 나와도 --stats가 세는 값의 수에는 상한이 있다. 자주 나온 값은 남고, 목록은 근사라고 적는다.
func TestViewStatsBoundsDistinctValues(t *testing.T) {
	line := func(path string) string {
		return `192.0.2.1 - - [26/Oct/2026:10:00:00 +0900] "GET ` + path + ` HTTP/1.1" 200 1 "-" "x"` + "\n"
	}
	var input strings.Builder
	for index := range 2*viewSummaryKeyLimit + 10 {
		if index%1000 == 0 {
			input.WriteString(line("/health"))
		}
		input.WriteString(line(fmt.Sprintf("/item/%d", index)))
	}
	summary, err := viewSummarize(strings.NewReader(input.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.stats.paths) > viewSummaryKeyLimit {
		t.Fatalf("%d distinct paths kept", len(summary.stats.paths))
	}
	var out bytes.Buffer
	if err := summary.write(&out, "access", false); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	if want := fmt.Sprintf("%d  /health\n", (2*viewSummaryKeyLimit+10+999)/1000); !strings.Contains(report, want) || !strings.Contains(report, "approximate") {
		t.Fatalf("want %q and an approximate note:\n%s", want, report)
	}
	if strings.Contains(report[strings.Index(report, "TOP CLIENTS"):], "approximate") {
		t.Fatalf("clients were not trimmed:\n%s", report)
	}
}

// .gz를 풀어서 센다. 잘린 .gz는 읽은 데까지 센 통계를 쓰고 오류를 돌려준다.
func TestViewStatsReadsCompressedLogs(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Write([]byte(strings.Join(readViewFixture(t, "spring.log"), "\n") + "\n"))
	writer.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "spring.log.gz")
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runViewStats(&out, path, false); err != nil || !strings.Contains(out.String(), "events    9\n") {
		t.Fatalf("gz: %v\n%s", err, out.String())
	}
	cut := filepath.Join(dir, "cut.log.gz")
	if err := os.WriteFile(cut, compressed.Bytes()[:compressed.Len()-20], 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runViewStats(&out, cut, false); err == nil || !strings.Contains(out.String(), "format level") {
		t.Fatalf("cut gz: %v\n%s", err, out.String())
	}
	if err := runViewStats(&out, dir, false); err == nil {
		t.Fatal("a directory is an error")
	}
}

// 오류만 모은 로그는 ERROR 줄 하나에 stack trace가 길게 붙는다. stack trace 줄을 빼고 형식을 정해야 level 형식이다.
func TestViewDetectsAnErrorOnlyLog(t *testing.T) {
	var lines []string
	for index := range 3 {
		lines = append(lines, "2026-11-12 12:14:46.603 ERROR --- [pool-1] o.s.s.s.TaskUtils$LoggingErrorHandler : Unexpected error", "java.lang.IllegalArgumentException: Invalid URI path")
		for frame := range 30 {
			lines = append(lines, "\tat com.example.Frame"+string(rune('a'+index))+".call(Frame.java:"+string(rune('0'+frame%10))+")")
		}
	}
	if format := detectViewFormat(lines); format != viewFormatLevel {
		t.Fatalf("format %q", format)
	}
	if format := detectViewFormat([]string{"\tat x", "Caused by: y"}); format != viewFormatPlain {
		t.Fatalf("stack trace lines alone: %q", format)
	}
}

// 터미널에서는 칠하되, 색을 지우면 흑백 출력과 글자가 같다.
func TestViewStatsPaintsOnlyColor(t *testing.T) {
	for _, fixture := range []string{"spring.log", "access.log", "app.jsonl"} {
		path := filepath.Join("testdata", "view", fixture)
		var plain, colored bytes.Buffer
		if err := runViewStats(&plain, path, false); err != nil {
			t.Fatal(err)
		}
		if err := runViewStats(&colored, path, true); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plain.String(), "\x1b[") || !strings.Contains(colored.String(), "\x1b[") {
			t.Fatalf("%s: plain has color %v, colored has color %v", fixture, strings.Contains(plain.String(), "\x1b["), strings.Contains(colored.String(), "\x1b["))
		}
		if strings.Contains(colored.String(), ";m") || strings.Contains(colored.String(), "\x1b[m") {
			t.Errorf("%s: an empty SGR parameter resets the style: %q", fixture, colored.String())
		}
		if stripped := ansi.Strip(colored.String()); stripped != plain.String() {
			t.Errorf("%s: colored text differs:\n%s\n---\n%s", fixture, stripped, plain.String())
		}
	}
}

// 형식을 정할 때 stack trace 줄은 세지 않고 건너뛰지만, 보는 줄 수에는 상한이 있다. 깊은 stack trace 뒤의 줄은 보지 않는다.
func TestViewDetectStopsAfterTheScanLimit(t *testing.T) {
	lines := []string{"2026-11-12 12:14:46.603 ERROR --- [pool-1] c.e.Job : failed", "java.lang.IllegalStateException: boom"}
	for len(lines) < viewDetectScanLines {
		lines = append(lines, "\tat com.example.Job.run(Job.java:1)")
	}
	for range viewDetectLines {
		lines = append(lines, `{"level":"info","msg":"later"}`)
	}
	if format := detectViewFormat(lines); format != viewFormatLevel {
		t.Fatalf("format %q: lines after the scan limit decided it", format)
	}
}

// viewLinesAfterAStackTrace는 stack trace 줄 stack개로 시작하고 level event가 뒤따르는 줄이다. 순환된 로그는 stack trace 중간에서
// 시작할 수 있다.
func viewLinesAfterAStackTrace(stack int) []string {
	var lines []string
	for range stack {
		lines = append(lines, "\tat com.example.Job.run(Job.java:1)")
	}
	for range viewDetectLines {
		lines = append(lines, "2026-11-12 12:14:47.000  INFO 1 --- [main] c.e.App : ok")
	}
	return lines
}

// 첫 viewDetectLines줄이 모두 stack trace여도 --stats는 grep처럼 viewDetectScanLines줄까지 보고 형식을 정한다. 앞 줄이 없는
// 첫 stack trace 줄은 event 하나가 되고, 나머지 stack trace 줄은 거기에 붙는다.
func TestViewStatsDetectsAFileThatStartsInAStackTrace(t *testing.T) {
	for _, test := range []struct {
		stack  int
		format viewFormat
		events int
	}{
		{viewDetectLines + 6, viewFormatLevel, 1 + viewDetectLines},
		{viewDetectScanLines + 10, viewFormatPlain, viewDetectScanLines + 10 + viewDetectLines},
	} {
		text := strings.Join(viewLinesAfterAStackTrace(test.stack), "\n") + "\n"
		summary, err := viewSummarize(strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		if grep := grepDetectFormat([]byte(text), true); summary.format != test.format || grep != test.format {
			t.Errorf("%d stack lines: --stats %q, grep %q, want %q", test.stack, summary.format, grep, test.format)
		}
		if summary.stats.lines != test.events {
			t.Errorf("%d stack lines: %d events, want %d", test.stack, summary.stats.lines, test.events)
		}
	}
}

// 파일 이름에 든 escape는 --stats 제목에 그대로 나오지 않는다.
func TestViewStatsSanitizesTheFileName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evil\x1b]0;owned\x07\x1b[31m.log")
	if err := os.WriteFile(path, []byte("2026-11-12 13:51:11.258 ERROR --- [main] c.e.Job : failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, color := range []bool{false, true} {
		var out bytes.Buffer
		if err := runViewStats(&out, path, color); err != nil {
			t.Fatal(err)
		}
		title, _, _ := strings.Cut(ansi.Strip(out.String()), "\n")
		if strings.ContainsAny(title, "\x1b\x07") || !strings.HasPrefix(title, "evil") {
			t.Fatalf("color %v: title %q", color, title)
		}
		if strings.Contains(out.String(), "owned\x07") || strings.Contains(out.String(), "]0;") {
			t.Fatalf("color %v: the OSC sequence reached the output: %q", color, out.String())
		}
	}
}
