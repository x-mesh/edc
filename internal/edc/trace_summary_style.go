package edc

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// traceSummaryTerminal은 trace가 끝날 때 내는 요약에 색을 쓸지와 한 줄의 폭이다. 파이프, NO_COLOR, test의
// buffer는 평문이다. test는 이 변수를 바꿔 정한 폭으로 색을 쓴 요약을 그린다.
var traceSummaryTerminal = func(out io.Writer) (bool, int) {
	file, ok := out.(*os.File)
	if !ok || !richEnabled(file) {
		return false, 0
	}
	return true, terminalWidth()
}

// traceSummaryStyle은 trace 요약의 색이다. color가 false면 모든 메서드가 지금의 평문 byte를 그대로 낸다.
type traceSummaryStyle struct{ textStyle }

func newTraceSummaryStyle(out io.Writer) traceSummaryStyle {
	color, width := traceSummaryTerminal(out)
	return traceSummaryStyle{textStyle{color: color, width: width}}
}

// traceSummaryProblems는 문제를 세는 값의 색이다. 0보다 클 때만 칠한다. 끊김, 오류, 버린 packet, 거부는 빨강,
// 재전송, 실패, 답 없음, 잃은 event는 노랑이다. live 화면의 traceGroupColumnSeverity와 같은 규칙이다.
var traceSummaryProblems = map[string]string{
	// 요약 줄의 label
	"Resets": richRed, "Errors": richRed, "Server errors (5xx)": richRed, "MAC changes": richRed, "Drops": richRed,
	"Listen overflows": richRed, "Listen drops": richRed,
	"Retransmissions": richYellow, "Incomplete": richYellow, "Client errors (4xx)": richYellow, "Unanswered": richYellow,
	"Failures": richYellow, "Lost events": richYellow, "Map full": richYellow, "TLS alerts": richYellow,
	// 표의 열 제목
	"RESET": richRed, "ERRORS": richRed, "MAC_CHANGES": richRed, "DROPS": richRed, "OOM": richRed,
	"RETRANS": richYellow, "INC": richYellow, "UNANSWERED": richYellow, "FAILURES": richYellow, "FAIL": richYellow,
	"ALERTS": richYellow, "NONZERO": richYellow, "SIGNALED": richYellow,
	// 제목 줄과 마지막 줄의 counter
	"errors": richRed, "denied": richRed,
	"map_full": richYellow, "stats_full": richYellow, "depth_full": richYellow, "counts_full": richYellow,
	"stacks_full": richYellow, "starts_full": richYellow, "ring_lost": richYellow, "stack_lost": richYellow,
	"collisions": richYellow,
}

// traceSummaryCount는 값 앞의 수다. "3 (buffer overflows…)", "12 ENOENT", 연결마다의 reset인 "true"를 읽는다.
func traceSummaryCount(value string) float64 {
	field, _, _ := strings.Cut(strings.TrimSpace(value), " ")
	if field == "true" {
		return 1
	}
	count, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return 0
	}
	return count
}

// problem은 name이 문제를 세는 값이고 value가 0보다 크면 그 색을 돌려준다.
func traceSummaryProblem(name, value string) string {
	if color := traceSummaryProblems[name]; color != "" && traceSummaryCount(value) > 0 {
		return color
	}
	return ""
}

// traceSummaryUnit은 "12.3ms", "1.5KiB", "50 Hz", "<2.00ms"처럼 수 뒤에 단위가 붙은 값이다.
// 단위는 아는 것만 받는다. "7zip" 같은 process 이름의 뒷부분을 단위로 칠하지 않는다.
var traceSummaryUnit = regexp.MustCompile(`^(\s*<?-?[0-9][0-9.]*)( ?)(ns|us|ms|s|B|KiB|MiB|GiB|B/s|bps|Mbps|Hz|CPUs|%|/s)(\s*)$`)

// units는 수 뒤의 단위만 회색으로 칠한다. 단위가 없는 값은 그대로 둔다.
func (style traceSummaryStyle) units(text string) string {
	match := traceSummaryUnit.FindStringSubmatch(text)
	if !style.color || match == nil {
		return text
	}
	return match[1] + match[2] + style.dim(match[3]) + match[4]
}

// value는 요약 줄의 값이다. " · "와 " / "로 이은 값은 조각마다 단위를 칠하고 구분자를 회색으로 둔다.
func (style traceSummaryStyle) value(text string) string {
	if !style.color {
		return text
	}
	for _, separator := range []string{" · ", " / "} {
		if strings.Contains(text, separator) {
			parts := strings.Split(text, separator)
			for index, part := range parts {
				parts[index] = style.units(part)
			}
			return style.join(separator, parts...)
		}
	}
	return style.units(text)
}

// problemValue는 0보다 큰 문제 값의 수를 color로 칠한다. 뒤의 괄호 설명은 회색이다.
func (style traceSummaryStyle) problemValue(text, color string) string {
	count, rest, _ := strings.Cut(text, " ")
	if rest == "" {
		return style.paint(text, color)
	}
	if strings.HasPrefix(rest, "(") {
		return style.paint(count, color) + " " + style.dim(rest)
	}
	return style.paint(text, color)
}

// title은 "TCP trace: 2s" 줄이다. 이름은 자홍 굵게, 구분자는 회색이다.
func (style traceSummaryStyle) title(name, duration string) string {
	if !style.color {
		return name + ": " + duration
	}
	return style.heading(name) + style.dim(":") + " " + style.value(duration)
}

// subheading은 요약 안의 묶음 제목이다.
func (style traceSummaryStyle) subheading(text string) string {
	if !style.color {
		return text
	}
	return richBold(text)
}

// field는 "Label: value" 줄이다. label은 회색이고, 문제를 세는 값은 0보다 클 때만 칠한다.
func (style traceSummaryStyle) field(label, value string) string {
	if !style.color {
		return label + ": " + value
	}
	if color := traceSummaryProblem(label, value); color != "" {
		return style.dim(label+":") + " " + style.problemValue(value, color)
	}
	return style.dim(label+":") + " " + style.value(value)
}

// fields는 indent 뒤에 "label: value"를 한 줄씩 쓴다. pairs는 label과 value를 번갈아 담는다.
func (style traceSummaryStyle) fields(out io.Writer, indent string, pairs ...string) {
	for index := 0; index+1 < len(pairs); index += 2 {
		style.line(out, indent+style.field(pairs[index], pairs[index+1]))
	}
}

// traceStat은 "calls 5"처럼 이름 뒤에 값이 오는 counter다. label이 비면 "50 Hz"처럼 값만 쓴다.
type traceStat struct{ label, value string }

// stats는 counter를 두 칸 띄어 잇는다. 이름은 회색, 값은 기본색이고 문제를 세는 값은 0보다 클 때만 칠한다.
func (style traceSummaryStyle) stats(stats ...traceStat) string {
	parts := make([]string, 0, len(stats))
	for _, stat := range stats {
		value := style.units(stat.value)
		if color := traceSummaryProblem(stat.label, stat.value); color != "" {
			value = style.paint(stat.value, color)
		}
		if stat.label == "" {
			parts = append(parts, value)
			continue
		}
		parts = append(parts, style.dim(stat.label)+" "+value)
	}
	return strings.Join(parts, "  ")
}

// headline은 "SYSCALL TRACE  duration 2s  calls 5" 줄이다.
func (style traceSummaryStyle) headline(name string, stats ...traceStat) string {
	return style.heading(name) + "  " + style.stats(stats...)
}

// hint는 "… 3 more rows in --json"처럼 표 밖의 안내다.
func (style traceSummaryStyle) hint(text string) string { return style.dim(text) }

// printf는 fmt.Fprintf와 같은 글자를 쓰되, 색을 쓸 때 줄마다 터미널 폭에 맞춰 자른다.
func (style traceSummaryStyle) printf(out io.Writer, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if style.color {
		lines := strings.Split(text, "\n")
		for index, line := range lines {
			lines[index] = style.fit(line)
		}
		text = strings.Join(lines, "\n")
	}
	io.WriteString(out, text)
}

// traceCell은 고정 폭 표의 칸이다. fmt의 verb와 폭으로 먼저 맞춘 뒤 칠해서, 색을 써도 칸의 폭이 평문과 같다.
type traceCell struct {
	value any
	color string
	units bool
	on    bool
}

func (cell traceCell) Format(state fmt.State, verb rune) {
	text := fmt.Sprintf(fmt.FormatString(state, verb), cell.value)
	switch {
	case !cell.on:
	case cell.color != "":
		text = richPaint(text, cell.color, false)
	case cell.units:
		text = traceSummaryStyle{textStyle{color: true}}.units(text)
	}
	io.WriteString(state, text)
}

// count는 name이 문제를 세는 열이면 0보다 큰 값을 그 색으로 칠하는 칸이다.
func (style traceSummaryStyle) count(name string, value any) traceCell {
	return traceCell{value: value, color: traceSummaryProblem(name, fmt.Sprint(value)), on: style.color}
}

// measure는 시간과 크기처럼 단위가 붙은 칸이다. 단위만 회색이다.
func (style traceSummaryStyle) measure(value any) traceCell {
	return traceCell{value: value, units: true, on: style.color}
}

// muted는 시각처럼 보조 정보인 칸이다.
func (style traceSummaryStyle) muted(value any) traceCell {
	return traceCell{value: value, color: richGray, on: style.color}
}

// traceTable은 tab으로 나눈 요약 표다. 평문은 지금처럼 칸을 tab으로 잇고, 색을 쓰면 칸을 공백으로 맞춰
// 터미널 폭 안에 자른다. tab은 폭을 잴 수 없어 자를 자리를 정하지 못한다.
type traceTable struct {
	titles []string
	rows   []traceTableRow
}

// traceTableRow는 표의 한 행이다. note가 있으면 칸 대신 그 줄을 그대로 쓴다.
type traceTableRow struct {
	cells  []string
	colors []string
	note   string
}

func (table *traceTable) add(cells ...string) {
	table.rows = append(table.rows, traceTableRow{cells: cells})
}

// addColored는 칸마다 색을 정한 행이다. 빈 색은 열 제목의 규칙을 따른다.
func (table *traceTable) addColored(cells, colors []string) {
	table.rows = append(table.rows, traceTableRow{cells: cells, colors: colors})
}

func (table *traceTable) addNote(text string) {
	table.rows = append(table.rows, traceTableRow{note: text})
}

const traceTableGap = "  "

func (style traceSummaryStyle) writeTable(out io.Writer, table traceTable) {
	if !style.color {
		lines := []string{strings.Join(table.titles, "\t")}
		for _, row := range table.rows {
			if row.cells == nil {
				lines = append(lines, row.note)
				continue
			}
			lines = append(lines, strings.Join(row.cells, "\t"))
		}
		io.WriteString(out, strings.Join(lines, "\n")+"\n")
		return
	}
	widths := make([]int, len(table.titles))
	for index, title := range table.titles {
		widths[index] = liveWidth(title)
	}
	for _, row := range table.rows {
		for index, cell := range row.cells {
			if index < len(widths) {
				widths[index] = max(widths[index], liveWidth(cell))
			}
		}
	}
	traceTableShrink(widths, table.titles, style.width)
	pad := func(text string, index int) string {
		if index == len(widths)-1 {
			return text
		}
		return text + strings.Repeat(" ", max(widths[index]-liveWidth(text), 0)) + traceTableGap
	}
	header := ""
	for index, title := range table.titles {
		header += pad(title, index)
	}
	style.line(out, style.dim(header))
	for _, row := range table.rows {
		if row.cells == nil {
			style.line(out, style.dim(row.note))
			continue
		}
		line := ""
		for index, cell := range row.cells {
			title := ""
			if index < len(table.titles) {
				title = table.titles[index]
			}
			// 문제의 수는 자르기 전의 값으로 판단한다.
			color := traceSummaryProblem(title, cell)
			if index < len(row.colors) && row.colors[index] != "" {
				color = row.colors[index]
			}
			if index < len(widths) && liveWidth(cell) > widths[index] {
				cell = ansi.Truncate(cell, widths[index], "…")
			}
			line += style.tableCell(title, cell, color, pad(cell, index)[len(cell):])
		}
		style.line(out, line)
	}
}

// traceTableShrink는 표가 width보다 넓으면 가장 넓은 열부터 한 칸씩 줄인다. 수를 담는 좁은 열은 남아서, 줄 끝을
// 잘랐을 때처럼 오른쪽의 문제 열을 잃지 않는다. 열은 제목 폭이나 traceTableMinWidth보다 좁아지지 않는다.
func traceTableShrink(widths []int, titles []string, width int) {
	if width <= 0 || len(widths) == 0 {
		return
	}
	total := (len(widths) - 1) * len(traceTableGap)
	for _, value := range widths {
		total += value
	}
	for total > width {
		widest := -1
		for index, value := range widths {
			floor := max(liveWidth(titles[index]), traceTableMinWidth)
			if value > floor && (widest < 0 || value > widths[widest]) {
				widest = index
			}
		}
		if widest < 0 {
			return
		}
		widths[widest]--
		total--
	}
}

// traceTableMinWidth는 줄인 열에 남기는 최소 폭이다. 주소와 이름의 앞부분을 알아볼 만큼 둔다.
const traceTableMinWidth = 12

// tableCell은 표의 한 칸이다. color는 칸의 색이고, padding은 칸 뒤의 공백이다.
func (style traceSummaryStyle) tableCell(title, cell, color, padding string) string {
	switch {
	case color != "":
		return style.paint(cell, color) + padding
	case title == "STATUS":
		return style.httpStatuses(cell) + padding
	}
	return style.units(cell) + padding
}

// httpStatuses는 "200 5, 404 1"의 code마다 HTTP code의 색을 입힌다. 수와 쉼표는 기본색과 회색이다.
func (style traceSummaryStyle) httpStatuses(text string) string {
	parts := strings.Split(text, ", ")
	for index, part := range parts {
		code, count, found := strings.Cut(part, " ")
		number, err := strconv.Atoi(code)
		if !found || err != nil {
			continue
		}
		parts[index] = style.paint(code, richHTTPCodeColor(number)) + " " + count
	}
	return style.join(", ", parts...)
}
