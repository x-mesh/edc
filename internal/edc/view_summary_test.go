package edc

import (
	"bytes"
	"compress/gzip"
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
