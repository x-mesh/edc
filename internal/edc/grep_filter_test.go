package edc

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrepTimeParserReadsLogTimestamps(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local)
	seoul := time.FixedZone("KST", 9*3600)
	for text, want := range map[string]time.Time{
		"26/Oct/2026:10:00:04 +0900":         time.Date(2026, 10, 26, 10, 0, 4, 0, seoul),
		"[26/Oct/2026:10:00:04 +0900]":       time.Date(2026, 10, 26, 10, 0, 4, 0, seoul),
		"2026-10-26T10:00:02Z":               time.Date(2026, 10, 26, 10, 0, 2, 0, time.UTC),
		"2026-10-26T10:00:02.123456Z":        time.Date(2026, 10, 26, 10, 0, 2, 123456000, time.UTC),
		"2026-10-26 10:00:02,310":            time.Date(2026, 10, 26, 10, 0, 2, 310000000, time.Local),
		"2026-10-26 10:00:02.5 host app[42]": time.Date(2026, 10, 26, 10, 0, 2, 500000000, time.Local),
		"Oct 26 10:00:00 host sshd[1]":       time.Date(2026, 10, 26, 10, 0, 0, 0, time.Local),
		"Dec 31 23:59:59":                    time.Date(2025, 12, 31, 23, 59, 59, 0, time.Local),
		"1761530400":                         time.Unix(1761530400, 0),
		"1761530400123":                      time.UnixMilli(1761530400123),
		"2026-10-26":                         time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local),
		"2026/10/26 10:00:02 - [main] INFO":  time.Date(2026, 10, 26, 10, 0, 2, 0, time.Local),
	} {
		parser := grepTimeParser{now: now}
		got, ok := parser.parse([]byte(text))
		if !ok || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", text, got, ok, want)
		}
	}
	parser := grepTimeParser{now: now}
	for _, text := range []string{"", "GET /api", "main", "12345"} {
		if _, ok := parser.parse([]byte(text)); ok {
			t.Errorf("%q must not parse", text)
		}
	}
	// 한 번 맞은 layout을 기억해 같은 표기의 다음 줄에 바로 쓴다.
	parser.parse([]byte("Oct 26 10:00:00 host sshd[1]: accepted"))
	if parser.layout != "Jan _2 15:04:05" || parser.words != 3 {
		t.Fatalf("layout %q words %d", parser.layout, parser.words)
	}
}

func TestGrepParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 30, 0, 0, time.Local)
	for text, want := range map[string]time.Time{
		"10m":                  now.Add(-10 * time.Minute),
		"2h":                   now.Add(-2 * time.Hour),
		"1d":                   now.Add(-24 * time.Hour),
		"1w":                   now.Add(-7 * 24 * time.Hour),
		"2026-10-27 09:00":     time.Date(2026, 10, 27, 9, 0, 0, 0, time.Local),
		"2026-10-27":           time.Date(2026, 10, 27, 0, 0, 0, 0, time.Local),
		"09:15":                time.Date(2026, 10, 27, 9, 15, 0, 0, time.Local),
		"2026-10-27T09:00:00Z": time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	} {
		got, err := grepParseWhen(text, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", text, got, err, want)
		}
	}
	for _, text := range []string{"", "yesterday", "-5m", "0m", "25:00"} {
		if _, err := grepParseWhen(text, now); err == nil {
			t.Errorf("%q must fail", text)
		}
	}
}

func TestGrepStatusSet(t *testing.T) {
	set, err := parseGrepStatuses("5xx,404,300-302")
	if err != nil {
		t.Fatal(err)
	}
	for status, want := range map[int]bool{500: true, 599: true, 404: true, 400: false, 301: true, 303: false, 200: false, 0: false} {
		if set.match(status) != want {
			t.Errorf("status %d: %v", status, !want)
		}
	}
	markers := map[string]bool{}
	for _, marker := range set.markers() {
		markers[string(marker)] = true
	}
	if !markers["\" 5"] || !markers["\" 3"] || !markers["\" 404 "] || len(markers) != 3 {
		t.Fatalf("markers %v", markers)
	}
	for _, bad := range []string{"", "6", "abc", "600-500", "0xx"} {
		if _, err := parseGrepStatuses(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}

// 싼 추출이 view의 parser와 같은 level, status, 시각을 낸다. 이어지는 줄은 앞 event의 시각도 물려받는다.
func TestGrepFilterFieldsMatchViewParser(t *testing.T) {
	for _, name := range []string{"access.log", "app.jsonl", "app.log"} {
		raw := readViewFixture(t, name)
		format := detectViewFormat(raw)
		filter := newGrepFilter(format, nil, time.Now())
		var previous *viewLine
		eventTime := ""
		for index, text := range raw {
			line := parseViewLine(text, format, previous)
			previous = &line
			if !line.cont {
				eventTime = line.time
			}
			fields := filter.track([]byte(text))
			if fields.level != line.level || fields.status != line.status || string(fields.timeText) != eventTime {
				t.Errorf("%s line %d: level %q status %d time %q, want %q %d %q", name, index+1, fields.level, fields.status, fields.timeText, line.level, line.status, eventTime)
			}
		}
	}
}

func grepCollect(t *testing.T, data []byte, format viewFormat, pattern string, configure func(*grepFilter), from int64) ([]int64, int64) {
	t.Helper()
	matcher, err := newGrepMatcher(pattern, false, false, data)
	if err != nil {
		t.Fatal(err)
	}
	filter := newGrepFilter(format, nil, time.Now())
	configure(filter)
	scanner := newGrepScanner(matcher, filter)
	scanner.block = 64 << 10
	reader := bytes.NewReader(data)
	if err := filter.prime(reader, from); err != nil {
		t.Fatal(err)
	}
	var offsets []int64
	read, err := scanner.scanRange(reader, from, int64(len(data)), -1, func(ref grepLineRef) bool {
		offsets = append(offsets, ref.offset)
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return offsets, read
}

func grepLineOffsets(data []byte, numbers ...int) []int64 {
	var starts []int64
	offset := int64(0)
	for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
		starts = append(starts, offset)
		offset += int64(len(line))
	}
	var result []int64
	for _, number := range numbers {
		result = append(result, starts[number-1])
	}
	return result
}

func TestGrepFilterSelectsByLevelStatusAndTime(t *testing.T) {
	seoul := time.FixedZone("KST", 9*3600)
	app, _ := os.ReadFile(filepath.Join("testdata", "view", "app.log"))
	jsonl, _ := os.ReadFile(filepath.Join("testdata", "view", "app.jsonl"))
	access, _ := os.ReadFile(filepath.Join("testdata", "view", "access.log"))
	for _, scenario := range []struct {
		name      string
		data      []byte
		format    viewFormat
		pattern   string
		configure func(*grepFilter)
		want      []int
	}{
		{"level warn keeps the stack trace", app, viewFormatLevel, "", func(filter *grepFilter) { filter.minLevel = grepLevelRank("warn") }, []int{2, 3, 4, 5, 6, 7, 8}},
		{"level error with pattern", app, viewFormatLevel, "java", func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, []int{4, 5, 6, 7}},
		{"level error with pattern in the message only", app, viewFormatLevel, "closed", func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, []int{4}},
		{"since inherits the event time", app, viewFormatLevel, "", func(filter *grepFilter) {
			filter.since = time.Date(2026, 10, 26, 10, 0, 2, 0, time.Local)
		}, []int{3, 4, 5, 6, 7, 8, 9}},
		{"until", app, viewFormatLevel, "", func(filter *grepFilter) {
			filter.until = time.Date(2026, 10, 26, 10, 0, 1, 500000000, time.Local)
		}, []int{1, 2}},
		{"json level", jsonl, viewFormatJSON, "", func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, []int{3}},
		{"json status", jsonl, viewFormatJSON, "", func(filter *grepFilter) { set, _ := parseGrepStatuses("5xx"); filter.setStatuses(set) }, []int{3}},
		{"access status class", access, viewFormatAccess, "", func(filter *grepFilter) { set, _ := parseGrepStatuses("5xx"); filter.setStatuses(set) }, []int{5}},
		{"access status list", access, viewFormatAccess, "", func(filter *grepFilter) { set, _ := parseGrepStatuses("404,3xx"); filter.setStatuses(set) }, []int{2, 3, 4}},
		{"access since", access, viewFormatAccess, "", func(filter *grepFilter) {
			filter.since = time.Date(2026, 10, 26, 10, 0, 4, 0, seoul)
		}, []int{5, 6}},
		{"access since with pattern", access, viewFormatAccess, "users", func(filter *grepFilter) {
			filter.since = time.Date(2026, 10, 26, 10, 0, 3, 0, seoul)
		}, []int{4, 6}},
	} {
		got, _ := grepCollect(t, scenario.data, scenario.format, scenario.pattern, scenario.configure, 0)
		want := grepLineOffsets(scenario.data, scenario.want...)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: offsets %v, want %v", scenario.name, got, want)
		}
	}
}

// 구간이 stack trace 중간에서 시작해도 앞의 ERROR 줄을 찾아 그 줄들을 오류로 본다.
func TestGrepFilterPrimeRestoresTheEvent(t *testing.T) {
	app, _ := os.ReadFile(filepath.Join("testdata", "view", "app.log"))
	from := grepLineOffsets(app, 5)[0]
	got, _ := grepCollect(t, app, viewFormatLevel, "", func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }, from)
	if want := grepLineOffsets(app, 5, 6, 7, 8); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("offsets %v, want %v", got, want)
	}
}

// grepTimedAccessLog는 초마다 한 줄씩 쌓이되 nginx처럼 최대 2초 뒤섞인 access log다.
func grepTimedAccessLog(lines int, start time.Time) []byte {
	random := rand.New(rand.NewSource(11))
	var buffer bytes.Buffer
	for index := 0; index < lines; index++ {
		at := start.Add(time.Duration(index)*time.Second - time.Duration(random.Intn(2000))*time.Millisecond)
		status := 200
		if random.Intn(50) == 0 {
			status = 503
		}
		fmt.Fprintf(&buffer, "10.0.0.%d - - [%s] \"GET /api/%d HTTP/1.1\" %d %d \"-\" \"curl\"\n", random.Intn(256), at.Format("02/Jan/2006:15:04:05 -0700"), random.Intn(1000), status, random.Intn(5000))
	}
	return buffer.Bytes()
}

func TestGrepSeekFindsTheSameLinesAsAFullScan(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepTimedAccessLog(20000, start)
	since := start.Add(20000*time.Second - 10*time.Minute)
	configure := func(filter *grepFilter) { filter.since = since }
	full, fullRead := grepCollect(t, data, viewFormatAccess, "503", configure, 0)
	filter := newGrepFilter(viewFormatAccess, nil, time.Now())
	from, probes, ok := filter.seek(bytes.NewReader(data), int64(len(data)), since.Add(-grepTolerance))
	if !ok || from == 0 || probes == 0 {
		t.Fatalf("seek: offset %d probes %d ok %v", from, probes, ok)
	}
	sought, soughtRead := grepCollect(t, data, viewFormatAccess, "503", configure, from)
	if len(full) == 0 || fmt.Sprint(sought) != fmt.Sprint(full) {
		t.Fatalf("seek found %d lines, full scan %d", len(sought), len(full))
	}
	if soughtRead*4 > fullRead {
		t.Fatalf("seek read %d of %d bytes", soughtRead, fullRead)
	}
	until := start.Add(10 * time.Minute)
	withUntil, _ := grepCollect(t, data, viewFormatAccess, "", func(filter *grepFilter) { filter.until = until }, 0)
	if len(withUntil) < 590 || len(withUntil) > 602 {
		t.Fatalf("until: %d lines", len(withUntil))
	}
}

// 두 로그를 이어 붙인 파일은 처음과 끝만 보면 순서 같지만 가운데에서 시각이 되돌아간다. 그런 파일은 이진 탐색하지 않는다.
func TestGrepBoundsRejectConcatenatedLogs(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	first := grepTimedAccessLog(6000, start)
	second := grepTimedAccessLog(6000, start.Add(30*time.Minute))
	joined := append(bytes.Clone(first), second...)
	filter := newGrepFilter(viewFormatAccess, nil, time.Now())
	if _, _, _, ok := filter.bounds(bytes.NewReader(joined), int64(len(joined))); ok {
		t.Fatal("a concatenated log must not count as ordered")
	}
	if _, _, ok := filter.seek(bytes.NewReader(joined), int64(len(joined)), start.Add(time.Hour)); ok {
		t.Fatal("a concatenated log must not be bisected")
	}
	if filter.endsBefore(bytes.NewReader(joined), int64(len(joined)), start.Add(48*time.Hour)) {
		t.Fatal("endsBefore must not trust an unordered file")
	}
	if _, _, _, ok := filter.bounds(bytes.NewReader(first), int64(len(first))); !ok {
		t.Fatal("an ordered log must count as ordered")
	}
	if !filter.endsBefore(bytes.NewReader(first), int64(len(first)), start.Add(48*time.Hour)) || filter.endsBefore(bytes.NewReader(first), int64(len(first)), start.Add(time.Minute)) {
		t.Fatal("endsBefore must compare the last timestamp")
	}
}

// grepLevelLog는 stack trace가 블록 경계를 넘을 만큼 긴 level 로그다. 시각은 늘어나고, 드문 낱말이 가끔 나온다.
func grepLevelLog(lines int, start time.Time, seed int64) []byte {
	random := rand.New(rand.NewSource(seed))
	words := []string{"started in 2.1s", "pool 80% used", "request failed", "cache hit key=user:7", "slow query table=orders", "connection reset by peer"}
	levels := []string{"INFO", "INFO", "INFO", "WARN", "ERROR", "DEBUG"}
	var buffer bytes.Buffer
	at := start
	for index := 0; index < lines; index++ {
		at = at.Add(time.Duration(random.Intn(3000)) * time.Millisecond)
		level := levels[random.Intn(len(levels))]
		message := words[random.Intn(len(words))]
		if random.Intn(400) == 0 {
			message = "payment refunded order=" + fmt.Sprint(random.Intn(100000))
		}
		fmt.Fprintf(&buffer, "%s %-5s [http-%d] c.e.Api - %s %d\n", at.Format("2006-01-02 15:04:05,000"), level, random.Intn(16), message, random.Intn(1000))
		if level == "ERROR" && random.Intn(3) == 0 {
			buffer.WriteString("java.lang.IllegalStateException: connection closed\n")
			for depth := random.Intn(90); depth >= 0; depth-- {
				fmt.Fprintf(&buffer, "\tat com.example.Db.query(Db.java:%d)\n", depth)
			}
			buffer.WriteString("Caused by: java.io.IOException: broken pipe\n\t... 12 more\n")
		}
	}
	return buffer.Bytes()
}

// --last는 뒤 블록부터 읽는다. 블록 앞에서 시작하는 stack trace도 앞 블록의 ERROR 줄에서 level을 물려받아야 한다.
func TestGrepLastKeepsTheTraceStateAcrossBlocks(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	data := grepLevelLog(6000, start, 31)
	configure := func(filter *grepFilter) { filter.minLevel = grepLevelRank("error") }
	forward, _ := grepCollect(t, data, viewFormatLevel, "", configure, 0)
	matcher, _ := newGrepMatcher("", false, false, data[:grepSampleSize/8])
	filter := newGrepFilter(viewFormatLevel, nil, time.Now())
	configure(filter)
	scanner := newGrepScanner(matcher, filter)
	scanner.block = 4096
	for _, count := range []int{50, 500, 100000} {
		got, _, err := scanner.scanLast(bytes.NewReader(data), int64(len(data)), count)
		if err != nil {
			t.Fatal(err)
		}
		want := forward
		if len(want) > count {
			want = want[len(want)-count:]
		}
		if len(got) != len(want) {
			t.Fatalf("last %d: %d lines, want %d", count, len(got), len(want))
		}
		for index := range got {
			if got[index].offset != want[index] {
				t.Fatalf("last %d: line %d at %d, want %d", count, index, got[index].offset, want[index])
			}
		}
	}
}

func TestGrepSeekGivesUpOnUnsortedOrUntimedFiles(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local)
	lines := bytes.Split(bytes.TrimSuffix(grepTimedAccessLog(5000, start), []byte{'\n'}), []byte{'\n'})
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	reversed := append(bytes.Join(lines, []byte{'\n'}), '\n')
	filter := newGrepFilter(viewFormatAccess, nil, time.Now())
	if _, _, ok := filter.seek(bytes.NewReader(reversed), int64(len(reversed)), start.Add(time.Hour)); ok {
		t.Fatal("a reversed file must not be bisected")
	}
	plain := []byte(strings.Repeat("no timestamps here\n", 20000))
	filter = newGrepFilter(viewFormatPlain, nil, time.Now())
	if _, _, ok := filter.seek(bytes.NewReader(plain), int64(len(plain)), start); ok {
		t.Fatal("a file without timestamps must not be bisected")
	}
}

func TestGrepCountNewlines(t *testing.T) {
	data := []byte("a\nb\nc\nd")
	for to, want := range map[int64]int64{0: 0, 1: 0, 2: 1, 6: 3, 7: 3} {
		if got, err := grepCountNewlines(bytes.NewReader(data), 0, to); err != nil || got != want {
			t.Errorf("to %d: %d %v, want %d", to, got, err, want)
		}
	}
}
