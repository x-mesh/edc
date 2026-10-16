package edc

import (
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// textStyle은 info, where, watch처럼 자기 layout을 가진 text 출력의 색이다. color가 false면 모든 메서드가 받은
// 글자를 그대로 돌려줘, 평문은 같은 조각에서 지금과 같은 byte로 나온다.
type textStyle struct {
	color bool
	// width는 색을 쓸 때 한 줄의 최대 폭이다. 0이면 자르지 않는다.
	width int
}

func newTextStyle(color bool) textStyle {
	if !color {
		return textStyle{}
	}
	return textStyle{color: true, width: terminalWidth()}
}

// paint는 color가 빈 문자열이면 기본색으로 둔다.
func (style textStyle) paint(text, color string) string {
	if !style.color || color == "" {
		return text
	}
	return richPaint(text, color, false)
}

func (style textStyle) dim(text string) string { return style.paint(text, richGray) }

func (style textStyle) warn(text string) string { return style.paint(text, richYellow) }

func (style textStyle) bad(text string) string { return style.paint(text, richRed) }

func (style textStyle) good(text string) string { return style.paint(text, richGreen) }

func (style textStyle) heading(text string) string {
	if !style.color {
		return text
	}
	return richPaint(text, richMagenta, true)
}

// command는 그대로 복사해 실행할 수 있는 명령이다.
func (style textStyle) command(text string) string {
	if !style.color {
		return text
	}
	return richPaint(text, richCyan, true)
}

// kv는 "label: value"다. label과 ": "만 회색이다.
func (style textStyle) kv(label, value string) string {
	return style.dim(label+":") + " " + value
}

// join은 parts를 sep으로 잇고 sep만 회색으로 칠한다.
func (style textStyle) join(sep string, parts ...string) string {
	return strings.Join(parts, style.dim(sep))
}

// phrase는 번역 문장 key를 회색으로, %s 자리의 value를 color로 칠한다. 문장 전체를 회색으로 감싸면 value의
// reset 뒤가 회색을 잃는다.
func (style textStyle) phrase(key, value, color string) string {
	if !style.color {
		return T(key, value)
	}
	before, after, _ := strings.Cut(T(key, "\x00"), "\x00")
	return style.dim(before) + style.paint(value, color) + style.dim(after)
}

// level은 임계치를 넘은 값만 노랑이나 빨강으로 칠한다. 정상 값은 기본색이다.
func (style textStyle) level(text string, level topLevel) string {
	switch level {
	case topLevelWarn:
		return style.warn(text)
	case topLevelDanger:
		return style.bad(text)
	}
	return text
}

// levelColor는 막대처럼 정상일 때도 색이 필요한 곳의 색이다.
func levelColor(level topLevel) string {
	switch level {
	case topLevelWarn:
		return richYellow
	case topLevelDanger:
		return richRed
	}
	return richGreen
}

// fit은 색을 쓸 때 줄을 터미널 폭에 맞춰 자른다. 평문은 지금처럼 터미널이 접는다.
func (style textStyle) fit(line string) string {
	if !style.color || style.width <= 0 || liveWidth(line) <= style.width {
		return line
	}
	return ansi.Truncate(line, style.width, "…")
}

// wrap은 parts를 sep으로 잇되, 색을 쓸 때 폭을 넘으면 다음 항목부터 indent 뒤의 새 줄에 둔다. 잘라 버리면
// 줄 끝의 값을 잃는다. 한 항목이 폭보다 길면 그 줄만 자른다.
func (style textStyle) wrap(parts []string, sep, indent string) string {
	if !style.color || style.width <= 0 {
		return strings.Join(parts, sep)
	}
	var lines []string
	current := ""
	for _, part := range parts {
		switch {
		case current == "":
			current = part
		case liveWidth(current+sep+part) <= style.width:
			current += sep + part
		default:
			lines = append(lines, style.fit(current))
			current = indent + part
		}
	}
	return strings.Join(append(lines, style.fit(current)), "\n")
}

func (style textStyle) line(writer io.Writer, text string) {
	io.WriteString(writer, style.fit(text)+"\n")
}

// tree는 "├── text" 같은 나무 줄이다. 가지는 회색이다.
func (style textStyle) tree(writer io.Writer, branch, text string) {
	style.line(writer, style.dim(branch)+" "+text)
}

// table은 다 맞춘 표의 첫 줄을 회색으로 칠하고, 색을 쓰면 줄마다 터미널 폭에 맞춰 자른다.
func (style textStyle) table(text string) string {
	if !style.color {
		return text
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if index == 0 {
			line = style.dim(line)
		}
		lines[index] = style.fit(line)
	}
	return strings.Join(lines, "\n")
}

// textTableRow는 색 표의 한 행이다. note가 있으면 칸 대신 그 줄을 회색으로 쓴다.
type textTableRow struct {
	cells []string
	note  string
}

const (
	textTableGap = "  "
	// textTableMinWidth는 줄인 열에 남기는 최소 폭이다. 주소와 이름의 앞부분을 알아볼 만큼 둔다.
	textTableMinWidth = 12
)

// colorTable은 색을 쓰는 터미널의 표다. 칸 폭은 색을 입히기 전의 글자로 재고, 칸을 채운 뒤 색을 입혀 열이 어긋나지
// 않는다. 첫 줄은 칸마다 회색인 머리글이고 행의 마지막 칸은 채우지 않는다. paint는 자르기 전의 칸(original)과 화면에 둘
// 칸(shown)을 받아 색을 입힌다. row는 머리글을 뺀 행 번호다. paint가 nil이면 칸을 그대로 둔다.
//
// shrink면 터미널보다 넓을 때 가장 넓은 열부터 줄여, 오른쪽 열을 통째로 잃지 않는다. 가장 넓은 열이 사용자가 복사해 쓰는
// 식별자(run ID, unit 이름)인 표는 shrink를 끄고 줄 끝을 자른다. 식별자가 …로 잘리면 다음 명령에 넣을 수 없다.
func (style textStyle) colorTable(header []string, rows []textTableRow, shrink bool, paint func(row, column int, original, shown string) string) string {
	widths := make([]int, len(header))
	for index, title := range header {
		widths[index] = liveWidth(title)
	}
	for _, row := range rows {
		for index, cell := range row.cells {
			if index == len(widths) {
				widths = append(widths, 0)
			}
			widths[index] = max(widths[index], liveWidth(cell))
		}
	}
	if shrink {
		textTableShrink(widths, header, style.width)
	}
	pad := func(text string, index, cells int) string {
		if index == cells-1 {
			return ""
		}
		return strings.Repeat(" ", max(widths[index]-liveWidth(text), 0)) + textTableGap
	}
	var builder strings.Builder
	head := ""
	for index, title := range header {
		head += style.dim(title) + pad(title, index, len(header))
	}
	builder.WriteString(style.fit(head) + "\n")
	for rowIndex, row := range rows {
		if row.cells == nil {
			builder.WriteString(style.fit(style.dim(row.note)) + "\n")
			continue
		}
		line := ""
		for index, cell := range row.cells {
			shown := cell
			if liveWidth(cell) > widths[index] {
				shown = ansi.Truncate(cell, widths[index], "…")
			}
			painted := shown
			if paint != nil {
				painted = paint(rowIndex, index, cell, shown)
			}
			line += painted + pad(shown, index, len(row.cells))
		}
		builder.WriteString(style.fit(line) + "\n")
	}
	return builder.String()
}

// colorTableRows는 첫 행이 머리글인 [][]string 표를 colorTable로 그린다. paint의 row는 머리글을 뺀 행 번호다.
func (style textStyle) colorTableRows(rows [][]string, shrink bool, paint func(row, column int, original, shown string) string) string {
	if len(rows) == 0 {
		return ""
	}
	body := make([]textTableRow, 0, len(rows)-1)
	for _, row := range rows[1:] {
		body = append(body, textTableRow{cells: row})
	}
	return style.colorTable(rows[0], body, shrink, paint)
}

// textTableShrink는 표가 width보다 넓으면 가장 넓은 열부터 한 칸씩 줄인다. 수를 담는 좁은 열은 그대로 남는다.
// 열은 머리글 폭이나 textTableMinWidth보다 좁아지지 않는다.
func textTableShrink(widths []int, header []string, width int) {
	if width <= 0 || len(widths) == 0 {
		return
	}
	total := (len(widths) - 1) * len(textTableGap)
	for _, value := range widths {
		total += value
	}
	for total > width {
		widest := -1
		for index, value := range widths {
			floor := textTableMinWidth
			if index < len(header) {
				floor = max(liveWidth(header[index]), textTableMinWidth)
			}
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
