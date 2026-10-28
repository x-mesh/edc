package edc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"testing"
)

// grepNaiveLines는 scanner와 비교하는 기준이다. 줄을 하나씩 나눠 accept로 확정하고, 상한보다 긴 줄은 앞부분만 본다.
func grepNaiveLines(data []byte, lineLimit int, accept func([]byte) bool) []grepLineRef {
	var lines []grepLineRef
	var offset, number int64
	for len(data) != 0 {
		end := bytes.IndexByte(data, '\n')
		next := end + 1
		if end < 0 {
			end, next = len(data), len(data)
		}
		line := data[:end]
		if len(line) > lineLimit {
			line = line[:lineLimit]
		}
		number++
		if accept(line) {
			lines = append(lines, grepLineRef{offset: offset, number: number, text: line})
		}
		offset += int64(next)
		data = data[next:]
	}
	return lines
}

// grepFixture는 블록 경계와 긴 줄을 모두 지나는 로그다. 패턴 낱말이 든 줄, 대소문자만 다른 줄, 블록보다 긴 줄, 상한보다
// 긴 줄, 빈 줄을 섞는다.
func grepFixture(lineLimit int) []byte {
	random := rand.New(rand.NewSource(7))
	var buffer bytes.Buffer
	words := []string{"GET /api/users 200", "POST /login 302", "GET /static/app.js 200", "GET /api/orders/41/cancel 503", "timeout connecting to 10.0.0.3", "Timeout waiting for lock"}
	for index := 0; index < 400; index++ {
		switch random.Intn(40) {
		case 0:
			buffer.WriteString("\n")
		case 1:
			buffer.WriteString(strings.Repeat("x", 300) + " cancel " + strings.Repeat("y", 200) + "\n")
		case 2:
			// 상한 뒤에만 낱말이 있는 줄은 맞지 않아야 한다.
			buffer.WriteString(strings.Repeat("z", lineLimit) + " cancel\n")
		default:
			fmt.Fprintf(&buffer, "10.0.%d.%d [27/Oct/2026:10:%02d:%02d +0900] \"%s\" %d\n", random.Intn(256), random.Intn(256), random.Intn(60), random.Intn(60), words[random.Intn(len(words))], random.Intn(50000))
		}
	}
	buffer.WriteString("last line without newline cancel")
	return buffer.Bytes()
}

func grepRefsEqual(t *testing.T, label string, got, want []grepLineRef, numbers bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d lines, want %d", label, len(got), len(want))
	}
	for index := range got {
		if got[index].offset != want[index].offset || !bytes.Equal(got[index].text, want[index].text) || (numbers && got[index].number != want[index].number) {
			t.Fatalf("%s: line %d is %+v, want %+v", label, index, got[index], want[index])
		}
	}
}

func TestGrepScanRangeEqualsNaive(t *testing.T) {
	const lineLimit = 1000
	data := grepFixture(lineLimit)
	sample := data[:min(len(data), grepSampleSize)]
	for _, scenario := range []struct {
		name       string
		pattern    string
		regex      bool
		ignoreCase bool
	}{
		{"literal", "cancel", false, false},
		{"literal common bytes", " 200", false, false},
		{"literal with letters only", "timeout", false, false},
		{"ignore case", "timeout", false, true},
		{"ignore case with digits", "orders/41", false, true},
		{"regex", `orders/[0-9]+/cancel`, true, false},
		{"regex alternation", `timeout|login`, true, false},
		{"empty pattern matches every line", "", false, false},
	} {
		matcher, err := newGrepMatcher(scenario.pattern, scenario.regex, scenario.ignoreCase, sample)
		if err != nil {
			t.Fatal(err)
		}
		want := grepNaiveLines(data, lineLimit, matcher.match)
		if len(want) == 0 && scenario.pattern != "" {
			t.Fatalf("%s: fixture has no match", scenario.name)
		}
		for _, block := range []int{1, 7, 64, 4096, 1 << 20} {
			scanner := newGrepScanner(matcher, nil)
			scanner.block, scanner.lineLimit = block, lineLimit
			var got []grepLineRef
			if _, err := scanner.scanRange(bytes.NewReader(data), 0, int64(len(data)), 0, func(ref grepLineRef) bool {
				got = append(got, grepLineRef{ref.offset, ref.number, bytes.Clone(ref.text)})
				return true
			}); err != nil {
				t.Fatal(err)
			}
			grepRefsEqual(t, fmt.Sprintf("%s block %d", scenario.name, block), got, want, true)
		}
	}
}

// 구간을 줄 경계와 상관없이 나눠도 각 줄은 시작한 구간에 한 번만 속하고, 줄 번호도 이어진다.
func TestGrepScanRangeAttributesLinesToTheirStart(t *testing.T) {
	const lineLimit = 1000
	data := grepFixture(lineLimit)
	matcher, _ := newGrepMatcher("cancel", false, false, data[:grepSampleSize/8])
	want := grepNaiveLines(data, lineLimit, matcher.match)
	random := rand.New(rand.NewSource(3))
	for _, block := range []int{5, 64, 777} {
		scanner := newGrepScanner(matcher, nil)
		scanner.block, scanner.lineLimit = block, lineLimit
		var got []grepLineRef
		from := int64(0)
		for from < int64(len(data)) {
			to := from + 1 + int64(random.Intn(3000))
			// 구간이 정확히 줄 시작에서 갈리는 경우도 섞는다. 그 줄은 뒤 구간의 것이다.
			if random.Intn(3) == 0 {
				if newline := bytes.IndexByte(data[min(to, int64(len(data))):], '\n'); newline >= 0 {
					to += int64(newline + 1)
				}
			}
			if to > int64(len(data)) {
				to = int64(len(data))
			}
			lineBase := int64(bytes.Count(data[:from], []byte{'\n'}))
			if _, err := scanner.scanRange(bytes.NewReader(data), from, to, lineBase, func(ref grepLineRef) bool {
				got = append(got, grepLineRef{ref.offset, ref.number, bytes.Clone(ref.text)})
				return true
			}); err != nil {
				t.Fatal(err)
			}
			from = to
		}
		grepRefsEqual(t, fmt.Sprintf("block %d", block), got, want, true)
	}
}

func TestGrepScanLastReturnsNewestMatches(t *testing.T) {
	const lineLimit = 1000
	data := grepFixture(lineLimit)
	matcher, _ := newGrepMatcher("cancel", false, false, data[:grepSampleSize/8])
	want := grepNaiveLines(data, lineLimit, matcher.match)
	for _, count := range []int{1, 5, 1000} {
		for _, block := range []int{64, 4096} {
			scanner := newGrepScanner(matcher, nil)
			scanner.block, scanner.lineLimit = block, lineLimit
			got, read, err := scanner.scanLast(bytes.NewReader(data), int64(len(data)), count)
			if err != nil {
				t.Fatal(err)
			}
			expected := want
			if len(expected) > count {
				expected = expected[len(expected)-count:]
			}
			if len(got) != len(expected) {
				t.Fatalf("last %d block %d: %d lines, want %d", count, block, len(got), len(expected))
			}
			for index := range got {
				if got[index].offset != expected[index].offset || !bytes.Equal(got[index].text, expected[index].text) {
					t.Fatalf("last %d block %d: line %d is %q at %d, want %q at %d", count, block, index, got[index].text, got[index].offset, expected[index].text, expected[index].offset)
				}
			}
			if count == 1 && read >= int64(len(data)) {
				t.Fatalf("last 1 read the whole file: %d of %d bytes", read, len(data))
			}
		}
	}
}

func TestGrepChoosePivotUsesTheSample(t *testing.T) {
	sample := []byte(strings.Repeat("aaaa bbbb cccc 1234 ", 100) + "q")
	if first, second := grepChoosePivot([]byte("abq1"), sample); first != 2 || second != 3 {
		t.Fatalf("pivot %d second %d, want the rare q then the digit", first, second)
	}
	// 가장 드문 바이트도 흔하면 bytes.Index로 돌아간다.
	if first, _ := grepChoosePivot([]byte("ab 1"), sample); first != -1 {
		t.Fatalf("common bytes must not pick a pivot: %d", first)
	}
	if first, _ := grepChoosePivot([]byte("q"), sample); first != -1 {
		t.Fatalf("a one-byte needle has no pivot: %d", first)
	}
	// 첫 바이트가 3~5%로 bytes.Index가 가장 느린 구간에서는, 1.5%보다 흔해도 첫 바이트보다 충분히 드문 바이트를 쓴다.
	log := []byte(strings.Repeat("s", 46) + strings.Repeat("3", 18) + strings.Repeat("x", 936))
	if first, _ := grepChoosePivot([]byte("s3"), log); first != 1 {
		t.Fatalf("a rarer byte than a 4.6%% first byte must be the pivot: %d", first)
	}
	if first, _ := grepChoosePivot([]byte("s3"), []byte(strings.Repeat("s", 46)+strings.Repeat("3", 40)+strings.Repeat("x", 914))); first != -1 {
		t.Fatalf("a byte almost as common as the first byte must not be the pivot: %d", first)
	}
	if first, _ := grepChoosePivot([]byte("s3"), []byte(strings.Repeat("s", 100)+strings.Repeat("3", 20)+strings.Repeat("x", 880))); first != -1 {
		t.Fatalf("bytes.Index is fast when the first byte is above 5%%: %d", first)
	}
}

func TestGrepMatcherNeedles(t *testing.T) {
	sample := []byte("sample text 0123456789 with letters and digits")
	for _, scenario := range []struct {
		pattern    string
		regex      bool
		ignoreCase bool
		needle     string
	}{
		{"Error 500", false, false, "Error 500"},
		{"Error 500 at", false, true, "error 500 at"},
		{"timeout", false, true, "timeout"},
		{"Status 500 ok", false, true, " 500 "},
		{"skip", false, true, ""},
		{`orders/[0-9]+`, true, false, "orders/"},
		{`(?i)orders`, true, false, ""},
		{`a|b`, true, false, ""},
	} {
		matcher, err := newGrepMatcher(scenario.pattern, scenario.regex, scenario.ignoreCase, sample)
		if err != nil {
			t.Fatal(err)
		}
		if string(matcher.needle) != scenario.needle {
			t.Errorf("%q (regex %v, -i %v): needle %q, want %q", scenario.pattern, scenario.regex, scenario.ignoreCase, matcher.needle, scenario.needle)
		}
	}
}

// grepFailingReader는 limit 앞을 읽으면 실패하는 파일이다.
type grepFailingReader struct {
	data  []byte
	limit int64
}

var errGrepTestRead = errors.New("test read failure")

func (reader grepFailingReader) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < reader.limit {
		return 0, errGrepTestRead
	}
	return bytes.NewReader(reader.data).ReadAt(buffer, offset)
}

// --last가 앞 블록에서 읽기 오류를 만나도 이미 찾은 뒤쪽 일치는 돌려준다. 호출한 쪽이 그 줄을 낸 뒤 오류로 끝낸다.
func TestGrepScanLastKeepsMatchesBeforeAReadError(t *testing.T) {
	const lineLimit = 1000
	data := grepFixture(lineLimit)
	matcher, _ := newGrepMatcher("cancel", false, false, data[:grepSampleSize/8])
	want := grepNaiveLines(data, lineLimit, matcher.match)
	scanner := newGrepScanner(matcher, nil)
	scanner.block, scanner.lineLimit = 4096, lineLimit
	limit := int64(len(data)) - 3*4096
	got, _, err := scanner.scanLast(grepFailingReader{data: data, limit: limit}, int64(len(data)), 100000)
	if !errors.Is(err, errGrepTestRead) {
		t.Fatalf("err %v", err)
	}
	var tail []grepLineRef
	for _, ref := range want {
		if ref.offset >= limit {
			tail = append(tail, ref)
		}
	}
	if len(got) == 0 || len(got) > len(tail) {
		t.Fatalf("kept %d lines, %d matches lie after the failing offset", len(got), len(tail))
	}
	for index := range got {
		expected := tail[len(tail)-len(got)+index]
		if got[index].offset != expected.offset || !bytes.Equal(got[index].text, expected.text) {
			t.Fatalf("line %d at %d, want %d", index, got[index].offset, expected.offset)
		}
	}
}

func TestGrepMatcherWithSampleKeepsCompiledPattern(t *testing.T) {
	compiled, err := newGrepMatcher("cancel", true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	matcher := compiled.withSample([]byte(strings.Repeat("aaaa", 100) + "cancel"))
	if matcher.regex != compiled.regex {
		t.Fatal("withSample compiled the pattern again")
	}
	if compiled.pivot != -1 || compiled.second != -1 {
		t.Fatalf("withSample changed the shared matcher: pivot %d, second %d", compiled.pivot, compiled.second)
	}
}

func TestGrepSearchFileReusesReadBuffer(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a.go", "b.go"} {
		path := dir + "/" + name
		if err := os.WriteFile(path, []byte("package a\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	options := grepOptions{pattern: "main", codeMode: true, minLevel: -1, index: grepIndexOff}
	matcher, err := newGrepMatcher(options.pattern, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	buffers := &grepBuffers{}
	printer := &grepPrinter{out: bufio.NewWriter(io.Discard)}
	hints := &grepHints{options: options}
	var first *byte
	for _, path := range paths {
		found, err := grepSearchFile(path, options, matcher, buffers, printer, hints)
		if err != nil || !found {
			t.Fatalf("%s: found %v, err %v", path, found, err)
		}
		data := buffers.buffer[:1]
		if first == nil {
			first = &data[0]
		} else if &data[0] != first {
			t.Fatal("the second file allocated a new read buffer")
		}
	}
}

func TestGrepFoldMatchesIgnoreCaseRegex(t *testing.T) {
	data := append(grepFixture(1000), "\nTIMEOUT\nTiMeOuT x\ntime-out\nORDERS/41/CANCEL\nKelvin Key\nſecret\n"...)
	lines := bytes.Split(data, []byte("\n"))
	sample := data[:min(len(data), grepSampleSize)]
	for _, scenario := range []struct {
		pattern string
		fold    bool
	}{
		{"timeout", true},
		{"/41/cancel", true},
		{"T", true},
		{"/41/", true},
		{"key", false},
		{"secret", false},
	} {
		matcher, err := newGrepMatcher(scenario.pattern, false, true, sample)
		if err != nil {
			t.Fatal(err)
		}
		if matcher.fold != scenario.fold {
			t.Fatalf("%s: fold %v, want %v", scenario.pattern, matcher.fold, scenario.fold)
		}
		regex := regexp.MustCompile("(?i)" + regexp.QuoteMeta(scenario.pattern))
		matched := 0
		for _, line := range lines {
			if got, want := matcher.match(line), regex.Match(line); got != want {
				t.Fatalf("%s: match(%q) = %v, regex %v", scenario.pattern, line, got, want)
			} else if got {
				matched++
			}
		}
		if matched == 0 {
			t.Fatalf("%s: no line matched", scenario.pattern)
		}
		if scenario.fold {
			for position := 0; position < len(data); {
				got, want := matcher.find(data[position:]), regex.FindIndex(data[position:])
				if (want == nil) != (got < 0) || (want != nil && got != want[0]) {
					t.Fatalf("%s at %d: find %d, regex %v", scenario.pattern, position, got, want)
				}
				if got < 0 {
					break
				}
				position += got + 1
			}
		}
	}
}
