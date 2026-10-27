package edc

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// viewScrollStep은 ←/→ 한 번에 미는 칸 수다.
const viewScrollStep = 10

// viewColumns는 access 로그를 compact 보기로 그릴 때의 열이다. 보관한 줄에서 값이 모두 같은 host, client, 날짜는
// 줄마다 되풀이할 정보가 없으므로 숨기고, 머리 줄에 그 값을 한 번 보인다.
type viewColumns struct {
	host, client, date                 bool
	hostWidth, clientWidth, bytesWidth int
	hidden                             []string
}

// viewAccessTime은 "27/Oct/2026:16:43:23 +0900"을 날짜와 시각으로 나눈다. 모양이 다르면 전체를 시각으로 둔다.
func viewAccessTime(text string) (string, string) {
	date, clock, found := strings.Cut(text, ":")
	if !found {
		return "", text
	}
	clock, _, _ = strings.Cut(clock, " ")
	return date, clock
}

func viewFieldValue(line viewLine, name string) string {
	for _, field := range line.fields {
		if field.name == name {
			return field.value
		}
	}
	return ""
}

// columns는 지금 보관한 access 줄로 열을 정한다. 줄 수만큼 문자열을 비교하므로 10,000줄에서도 화면 한 번에 충분히 싸다.
func (model viewScreenModel) columns() viewColumns {
	var columns viewColumns
	var host, client, date string
	seen := false
	for _, line := range model.lines {
		if line.marker || line.cont || line.status == 0 {
			continue
		}
		lineHost, lineDate := viewFieldValue(line, "host"), ""
		lineDate, _ = viewAccessTime(line.time)
		if !seen {
			host, client, date, seen = lineHost, line.client, lineDate, true
		}
		columns.host = columns.host || lineHost != host
		columns.client = columns.client || line.client != client
		columns.date = columns.date || lineDate != date
		columns.hostWidth = max(columns.hostWidth, len(lineHost))
		columns.clientWidth = max(columns.clientWidth, len(line.client))
		columns.bytesWidth = max(columns.bytesWidth, len(viewFieldValue(line, "bytes")))
	}
	if !seen {
		return columns
	}
	for _, hidden := range []struct {
		name, value string
		shown       bool
	}{{"host", host, columns.host}, {"client", client, columns.client}, {"date", date, columns.date}} {
		if !hidden.shown && hidden.value != "" {
			columns.hidden = append(columns.hidden, hidden.name+" "+hidden.value)
		}
	}
	return columns
}

// viewCompactSpans는 access 줄을 "[host] [client] 시각 status method bytes path"로 그린다. 형식을 읽지 못한 줄은 원문이다.
func viewCompactSpans(line viewLine, columns viewColumns) []traceText {
	if line.status == 0 || line.marker || line.cont {
		return viewLineSpans(line, viewFormatAccess)
	}
	pad := func(text string, width int) string {
		return text + strings.Repeat(" ", max(0, width-len(text)))
	}
	var spans []traceText
	if columns.host {
		spans = append(spans, traceText{text: pad(viewFieldValue(line, "host"), columns.hostWidth) + "  ", color: richGray})
	}
	if columns.client {
		spans = append(spans, traceText{text: pad(line.client, columns.clientWidth) + "  "})
	}
	date, clock := viewAccessTime(line.time)
	if columns.date && date != "" {
		clock = date + " " + clock
	}
	bytesText := viewFieldValue(line, "bytes")
	return append(spans,
		traceText{text: clock + "  ", color: richGray},
		traceText{text: strconv.Itoa(line.status), color: richHTTPCodeColor(line.status), bold: line.status >= 500},
		traceText{text: "  " + pad(line.method, 6) + "  "},
		traceText{text: strings.Repeat(" ", max(0, columns.bytesWidth-len(bytesText))) + bytesText + "  ", color: richGray},
		traceText{text: line.path},
	)
}

// viewSkipCells는 spans의 앞 n칸을 버린다. 가로 스크롤에 쓴다.
func viewSkipCells(spans []traceText, n int) []traceText {
	if n <= 0 {
		return spans
	}
	var out []traceText
	for _, span := range spans {
		width := ansi.StringWidth(span.text)
		if n >= width {
			n -= width
			continue
		}
		if n > 0 {
			span.text = ansi.Cut(span.text, n, width)
			n = 0
		}
		out = append(out, span)
	}
	return out
}
