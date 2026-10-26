package edc

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readViewFixture(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "view", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func parseViewFixture(t *testing.T, name string) (viewFormat, []viewLine) {
	t.Helper()
	raw := readViewFixture(t, name)
	format := detectViewFormat(raw)
	var lines []viewLine
	var previous *viewLine
	for _, text := range raw {
		lines = append(lines, parseViewLine(text, format, previous))
		previous = &lines[len(lines)-1]
	}
	return format, lines
}

func TestDetectViewFormat(t *testing.T) {
	for name, want := range map[string]viewFormat{"access.log": viewFormatAccess, "app.jsonl": viewFormatJSON, "app.log": viewFormatLevel} {
		if got := detectViewFormat(readViewFixture(t, name)); got != want {
			t.Errorf("%s: format %q, want %q", name, got, want)
		}
	}
	if got := detectViewFormat([]string{"hello", "world"}); got != viewFormatPlain {
		t.Errorf("plain text: format %q", got)
	}
}

func TestParseViewAccessLine(t *testing.T) {
	_, lines := parseViewFixture(t, "access.log")
	line := lines[4]
	if line.client != "192.0.2.77" || line.status != 502 || line.method != "GET" || line.path != "/api/orders?id=7" || line.time != "26/Oct/2026:10:00:04 +0900" {
		t.Fatalf("access fields: %+v", line)
	}
	if !viewHasField(line.fields, "user agent", "Go-http-client/1.1") {
		t.Fatalf("user agent missing: %+v", line.fields)
	}
	// referrer와 user agent가 없는 common 형식도 읽는다.
	if lines[5].status != 200 {
		t.Fatalf("common format status: %+v", lines[5])
	}
	if got := viewLinePlain(line, viewFormatAccess); got != line.text {
		t.Fatalf("colored access line changes the text: %q", got)
	}
}

func TestParseViewJSONLine(t *testing.T) {
	_, lines := parseViewFixture(t, "app.jsonl")
	failed := lines[2]
	if failed.level != "error" || failed.status != 502 || failed.time != "2026-10-26T10:00:02Z" {
		t.Fatalf("json fields: %+v", failed)
	}
	if !strings.HasPrefix(failed.message, "upstream failed") || !strings.Contains(failed.message, "upstream=10.0.0.3:9000") {
		t.Fatalf("json message: %q", failed.message)
	}
	if got := viewLinePlain(lines[1], viewFormatJSON); got != "2026-10-26T10:00:01Z WARN slow query  duration_ms=812 table=orders" {
		t.Fatalf("json line: %q", got)
	}
	if failed.fields[0].name != "ts" || failed.fields[1].name != "severity" || failed.fields[2].name != "message" {
		t.Fatalf("json detail must start with time, level, and message: %+v", failed.fields)
	}
}

func TestParseViewLevelLineKeepsStackTraceTogether(t *testing.T) {
	_, lines := parseViewFixture(t, "app.log")
	if lines[2].level != "error" || lines[2].time != "2026-10-26 10:00:02,310" {
		t.Fatalf("error line: %+v", lines[2])
	}
	for index := 3; index <= 7; index++ {
		if !lines[index].cont || lines[index].level != "error" {
			t.Fatalf("line %d must continue the error: %+v", index, lines[index])
		}
	}
	// 본문의 소문자 "error"는 level이 아니다.
	if lines[8].level != "info" || lines[8].cont {
		t.Fatalf("info line: %+v", lines[8])
	}
}

func TestViewLevelNeedsUppercaseOrKey(t *testing.T) {
	for text, want := range map[string]string{
		"I think the error is here":            "",
		"user saw an error page":               "",
		"[ERROR] disk full":                    "error",
		"time=10:00 level=warn msg=slow":       "warn",
		"Oct 26 10:00:00 host app[42]: NOTICE": "info",
	} {
		if got := viewFindLevel(text); got != want {
			t.Errorf("%q: level %q, want %q", text, got, want)
		}
	}
}

func TestViewSanitizeRemovesTerminalControl(t *testing.T) {
	got := viewSanitize("a\x1b[31mred\x1b[0m\tb\x07c\r")
	if got != "ared    b·c" {
		t.Fatalf("sanitized: %q", got)
	}
}

// 화면을 열지 않는 경로는 받은 바이트를 바꾸지 않아야 한다. 이미 색이 든 줄, 강조 상한보다 긴 줄, CRLF,
// 줄바꿈 없는 마지막 줄을 넣는다.
func TestViewPassthroughKeepsBytes(t *testing.T) {
	long := strings.Repeat("x", viewParseLimit+100)
	content := "\x1b[31mERROR\x1b[0m colored\r\n" + long + "\nplain\nlast without newline"
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for lines, want := range map[int]string{-1: content, 0: "", 2: "plain\nlast without newline", 3: long + "\nplain\nlast without newline", 10: content} {
		var stdout, stderr bytes.Buffer
		if err := viewPassthrough(&stdout, &stderr, path, lines, false); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != want {
			t.Errorf("-n %d: got %d bytes, want %d", lines, stdout.Len(), len(want))
		}
	}
}

func TestViewTailOffsetCrossesBlocks(t *testing.T) {
	line := strings.Repeat("y", viewBlockSize/3) + "\n"
	content := strings.Repeat(line, 10)
	reader := strings.NewReader(content)
	for _, lines := range []int{1, 4, 10, 11} {
		offset, err := viewTailOffset(reader, int64(len(content)), lines)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(len(line) * max(0, 10-lines)); offset != want {
			t.Errorf("%d lines: offset %d, want %d", lines, offset, want)
		}
	}
}

func TestViewTailReaderKeepsLastLines(t *testing.T) {
	got, err := viewTailReader(strings.NewReader("a\nb\nc\nd"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes.Join(got, nil)) != "c\nd" {
		t.Fatalf("tail: %q", bytes.Join(got, nil))
	}
}

func TestViewSplitLinesKeepsPartialLine(t *testing.T) {
	lines, pending := viewSplitLines(nil, []byte("a\r\nb\nc"))
	if strings.Join(lines, "|") != "a|b" || string(pending) != "c" {
		t.Fatalf("lines %q pending %q", lines, pending)
	}
	lines, pending = viewSplitLines(pending, []byte("d\n"))
	if strings.Join(lines, "|") != "cd" || len(pending) != 0 {
		t.Fatalf("lines %q pending %q", lines, pending)
	}
}

// logrotate처럼 파일을 옮기고 새로 만들면 옛 파일의 나머지를 읽은 뒤 새 파일을 처음부터 읽는다. 잘리면 처음부터 다시 읽는다.
func TestFollowViewFileFollowsRotationAndTruncation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "app.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan viewFollowEvent, 32)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go followViewFile(ctx, path, file, 4, func(event viewFollowEvent) bool {
		events <- event
		return true
	})
	next := func() viewFollowEvent {
		t.Helper()
		select {
		case event := <-events:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("no follow event")
		}
		return viewFollowEvent{}
	}
	appendTo := func(text string) {
		t.Helper()
		handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer handle.Close()
		if _, err := handle.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	appendTo("two\n")
	if event := next(); string(event.data) != "two\n" {
		t.Fatalf("append: %+v", event)
	}
	appendTo("three\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 3 {
		event := next()
		if event.reset != "" {
			got = append(got, "<"+event.reset+">")
		} else {
			got = append(got, string(event.data))
		}
	}
	if strings.Join(got, "") != "three\n<replaced>new\n" {
		t.Fatalf("rotation: %q", got)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if event := next(); event.reset != viewResetTruncated {
		t.Fatalf("truncate: %+v", event)
	}
	if event := next(); string(event.data) != "x\n" {
		t.Fatalf("after truncate: %+v", event)
	}
}

func viewHasField(fields []viewField, name, value string) bool {
	for _, field := range fields {
		if field.name == name && field.value == value {
			return true
		}
	}
	return false
}

// JSON 키도 로그 내용이다. 키에 든 escape가 목록 줄이나 상세 화면에 그대로 나가면 터미널을 조작한다.
func TestParseViewJSONRemovesControlFromKeys(t *testing.T) {
	line := parseViewLine(`{"level":"info","msg":"hi","\u001b[2J\u001b]0;title\u0007":"x"}`, viewFormatJSON, nil)
	if strings.ContainsRune(line.message, 0x1b) {
		t.Fatalf("message keeps an escape: %q", line.message)
	}
	for _, field := range line.fields {
		if strings.ContainsRune(field.name, 0x1b) || strings.ContainsRune(field.name, 0x07) {
			t.Fatalf("field name keeps a control character: %q", field.name)
		}
	}
}

// 줄바꿈이 없는 큰 파일을 열어도 limit보다 많이 읽지 않는다. 창이 줄 중간에서 시작하면 그 조각은 버린다.
func TestViewReadInitialStaysWithinLimit(t *testing.T) {
	content := strings.Repeat("z", 5000) + "\nlast\n"
	reader := strings.NewReader(content)
	data, err := viewReadInitial(reader, int64(len(content)), viewKeepLines, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "last\n" {
		t.Fatalf("read %d bytes: %q", len(data), data[:min(len(data), 40)])
	}
	data, err = viewReadInitial(reader, int64(len(content)), 0, 1024)
	if err != nil || len(data) != 0 {
		t.Fatalf("-n 0 reads nothing: %q %v", data, err)
	}
	noNewline := strings.Repeat("q", 5000)
	data, _ = viewReadInitial(strings.NewReader(noNewline), int64(len(noNewline)), viewKeepLines, 1024)
	if len(data) > 1024 {
		t.Fatalf("a file without newlines read %d bytes", len(data))
	}
}

func TestViewTailReaderKeepsOrderAcrossTheRing(t *testing.T) {
	got, err := viewTailReader(strings.NewReader("1\n2\n3\n4\n5\n6\n7\n"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes.Join(got, nil)) != "5\n6\n7\n" {
		t.Fatalf("tail: %q", bytes.Join(got, nil))
	}
}
