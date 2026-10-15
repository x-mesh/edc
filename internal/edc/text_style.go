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
