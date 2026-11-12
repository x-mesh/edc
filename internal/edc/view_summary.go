package edc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// viewSummaryTopRows는 --stats가 보이는 상위 목록의 길이다. 화면 패널보다 자리가 넉넉하다.
const viewSummaryTopRows = 10

// viewSummary는 입력 전체를 한 번 읽으며 센 통계다. 줄은 보관하지 않으므로 파일 크기와 관계없이 메모리가 일정하다.
type viewSummary struct {
	format viewFormat
	lines  int
	stats  viewStats
	// firstTime과 lastTime은 시각이 있는 처음과 마지막 event의 시각이다. 형식마다 시각을 적는 방식이 달라 원문 그대로 둔다.
	firstTime string
	lastTime  string
	previous  viewLine
	read      bool
	// eventOpen은 이어지는 줄이 붙을 event를 읽었다는 뜻이다. 파일 첫머리의 stack trace 줄은 붙을 event가 없다.
	eventOpen bool
}

func newViewSummary(format viewFormat) *viewSummary {
	return &viewSummary{format: format, stats: newViewStats()}
}

// add는 한 줄을 센다. 예외 줄은 event보다 뒤에 오므로, event를 셀 때가 아니라 예외 줄을 읽을 때 그 event의 예외를 센다.
// 화면의 attachException과 같은 규칙이다.
func (summary *viewSummary) add(text string) {
	var previous *viewLine
	if summary.read {
		previous = &summary.previous
	}
	line := parseViewLine(text, summary.format, previous)
	summary.lines++
	if line.exceptionHead && summary.eventOpen {
		viewCount(summary.stats.exceptions, line.exception, 1)
	}
	if !line.cont {
		summary.eventOpen = true
		if line.time != "" {
			if summary.firstTime == "" {
				summary.firstTime = line.time
			}
			summary.lastTime = line.time
		}
	}
	summary.stats.add(line)
	summary.previous, summary.read = line, true
}

// runViewStats는 path(비어 있으면 stdin)를 처음부터 끝까지 읽어 통계를 stdout에 쓴다. .gz와 .zst는 풀어서 읽는다.
// color는 stdout이 터미널이고 NO_COLOR가 없을 때다.
func runViewStats(stdout io.Writer, path string, color bool) error {
	input, closeInput, err := viewOpenStats(path)
	if err != nil {
		return err
	}
	defer closeInput()
	summary, err := viewSummarize(input)
	if summary != nil {
		title := filepath.Base(path)
		if path == "" {
			title = "stdin"
		}
		if writeErr := summary.write(stdout, title, color); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func viewOpenStats(path string) (io.Reader, func(), error) {
	if path == "" {
		return os.Stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if info, err := file.Stat(); err != nil || info.IsDir() {
		file.Close()
		if err == nil {
			err = fmt.Errorf("%s: %w", path, syscall.EISDIR)
		}
		return nil, nil, err
	}
	decompress := logDecompressor(path)
	if decompress == nil {
		return file, func() { file.Close() }, nil
	}
	reader, err := decompress(file)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return reader, func() { reader.Close(); file.Close() }, nil
}

// viewSummarize는 처음 viewDetectLines줄로 형식을 정한 뒤 모든 줄을 센다. 끝까지 읽지 못하면(잘린 .gz 등) 그때까지 센
// 통계와 오류를 함께 돌려준다. 통계를 버리지 않는 것은 화면이 잘린 압축 파일을 다루는 방식과 같다.
func viewSummarize(input io.Reader) (*viewSummary, error) {
	reader := bufio.NewReaderSize(input, viewBlockSize)
	var sample []string
	var summary *viewSummary
	push := func(text string) {
		if summary != nil {
			summary.add(text)
			return
		}
		sample = append(sample, text)
		if len(sample) == viewDetectLines {
			summary = newViewSummary(detectViewFormat(sample))
			for _, line := range sample {
				summary.add(line)
			}
		}
	}
	var readErr error
	for {
		raw, _, err := viewReadLine(reader, viewStoreLimit)
		if len(raw) > 0 {
			push(strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r"))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	if summary == nil {
		summary = newViewSummary(detectViewFormat(sample))
		for _, line := range sample {
			summary.add(line)
		}
	}
	return summary, readErr
}

// write는 통계를 쓴다. color면 화면 패널처럼 칠하되 글자와 칸은 흑백과 똑같이 둔다. 색만 지우면 흑백 출력과 같아서
// 파일로 남긴 결과와 터미널에서 본 결과를 맞대어 볼 수 있다.
func (summary *viewSummary) write(out io.Writer, title string, color bool) error {
	paint := func(text, code string, bold bool) string {
		if !color {
			return text
		}
		return viewSummaryPaint(text, code, bold)
	}
	stats := summary.stats
	var builder strings.Builder
	row := func(label, value string) {
		builder.WriteString(paint(fmt.Sprintf("%-10s", label), richGray, false) + value + "\n")
	}
	section := func(name string) { builder.WriteString("\n" + paint(name, "", true) + "\n") }
	builder.WriteString(paint(title, "", true) + paint("  ·  format ", richGray, false) + string(summary.format) + "\n")
	row("lines", groupThousands(int64(summary.lines)))
	row("events", groupThousands(int64(stats.lines)))
	if summary.firstTime != "" {
		row("time", viewSanitize(summary.firstTime)+paint(" → ", richGray, false)+viewSanitize(summary.lastTime))
	}
	failures := groupThousands(int64(stats.failures))
	if stats.failures > 0 {
		failures = paint(failures, richRed, true)
	}
	row("errors", failures+paint("  "+strings.TrimSpace(viewPercent(stats.failures, stats.lines)), richGray, false))
	if breakdown := viewBreakdown(stats); len(breakdown) != 0 {
		section(viewBreakdownTitle(stats))
		width := 0
		for _, entry := range breakdown {
			width = max(width, len(groupThousands(int64(entry.count))))
		}
		for _, entry := range breakdown {
			label := paint(fmt.Sprintf("%-6s", entry.label), entry.color, entry.label == "error" || entry.label == "fatal" || entry.label == "5xx")
			builder.WriteString(label + fmt.Sprintf(" %*s ", width, groupThousands(int64(entry.count))) + paint(viewPercent(entry.count, stats.lines), richGray, false) + "\n")
		}
	}
	if last := stats.lastError; last.text != "" {
		section("LAST ERROR")
		builder.WriteString(summary.paintError(viewPanelError(last, summary.format), color) + "\n")
	}
	for _, top := range viewPanelTops(stats) {
		entries := viewTopN(top.counts, viewSummaryTopRows)
		if len(entries) == 0 {
			continue
		}
		section(top.title)
		width := 0
		for _, entry := range entries {
			width = max(width, len(groupThousands(int64(entry.count))))
		}
		for _, entry := range entries {
			fmt.Fprintf(&builder, "%*s  %s\n", width, groupThousands(int64(entry.count)), viewSanitize(entry.label))
		}
		if rest := len(top.counts) - len(entries); rest > 0 {
			builder.WriteString(paint(fmt.Sprintf("%*s  (%s more)", width, "", groupThousands(int64(rest))), richGray, false) + "\n")
		}
	}
	_, err := io.WriteString(out, builder.String())
	return err
}

// paintError는 마지막 오류 줄을 목록과 같은 색으로 칠한다. 공백을 줄인 줄을 다시 읽어 칠하고, 칠한 조각의 글자가 줄과
// 다르면(JSON 줄은 읽은 모양으로 줄여 그린다) 칠하지 않는다.
func (summary *viewSummary) paintError(text string, color bool) string {
	if !color {
		return text
	}
	var plain, painted strings.Builder
	for _, span := range viewLineSpans(parseViewLine(text, summary.format, nil), summary.format) {
		plain.WriteString(span.text)
		painted.WriteString(viewSummaryPaint(span.text, span.color, span.bold))
	}
	if plain.String() != text {
		return text
	}
	return painted.String()
}

// viewSummaryPaint는 richPaint와 같되 색이 없는 조각을 다룬다. 빈 색 코드를 SGR에 넣으면 "1;"의 빈 값이 0(초기화)으로
// 읽혀 굵게 칠한 것이 풀린다.
func viewSummaryPaint(text, code string, bold bool) string {
	switch {
	case code != "":
		return richPaint(text, code, bold)
	case bold:
		return richBold(text)
	}
	return text
}
