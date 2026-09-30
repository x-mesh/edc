package edc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// tracePreview는 화면 나누기에서 아래 창에 보이는 event다. 대상 event나 가리기, gzip 설정이 바뀔 때만 다시 만든다.
// payload를 가리고 푸는 일을 화면을 그릴 때마다 하면 --payload=all의 1MiB payload에서 event를 따라가지 못한다.
type tracePreview struct {
	number  int
	secrets bool
	decode  bool
	title   string
	// lines는 화면 폭으로 나누기 전의 줄이다. 그릴 때 창에 보일 만큼만 나눈다.
	lines []string
}

// traceSplitMinimum은 화면 나누기에 필요한 목록과 미리 보기의 줄 수다. 더 좁으면 목록만 보인다.
const traceSplitMinimum = 6

// traceSplitHeights는 머리글 아래를 목록과 미리 보기로 나눈다. 미리 보기의 첫 줄은 구분선이다.
func traceSplitHeights(height int) (int, int, bool) {
	remaining := max(0, height-3)
	if remaining < traceSplitMinimum {
		return remaining, 0, false
	}
	list := remaining / 2
	return list, remaining - list, true
}

// previewTarget은 미리 보기에 보일 event다. 고른 event가 있으면 그것이고, 없으면 filter에 맞는 가장 최근 event다.
func (model traceScreenModel) previewTarget() int {
	if model.selected >= 0 {
		return model.selected
	}
	return model.moveSelection(-1, 1)
}

// refreshPreview는 미리 보기의 대상이나 설정이 바뀌었으면 다시 만든다. 다른 event로 바뀌면 맨 위부터 보인다.
func (model *traceScreenModel) refreshPreview() {
	if !model.split || model.groupBy != "" {
		return
	}
	number := model.previewTarget()
	if number < model.first || number >= model.first+len(model.events) {
		model.preview = nil
		return
	}
	if preview := model.preview; preview != nil && preview.number == number && preview.secrets == model.secrets && preview.decode == model.decode {
		return
	}
	if model.preview == nil || model.preview.number != number {
		model.previewOffset = 0
	}
	event, payload, notes := model.detailPayload(number)
	preview := &tracePreview{number: number, secrets: model.secrets, decode: model.decode, title: traceEventTitle(event)}
	if payload != "" {
		// 창이 작아서 payload가 있으면 payload를 먼저 보인다. event 필드는 Enter의 상세 보기에 있다.
		event.Payload = payload
		preview.lines = append(notes, traceHTTPPayloadBlock(event)...)
	} else {
		event.Payload = ""
		encoded, err := json.MarshalIndent(event, "", "  ")
		if err != nil {
			encoded = []byte(err.Error())
		}
		preview.lines = strings.Split(string(encoded), "\n")
	}
	model.preview = preview
}

// traceWrapWindow는 lines를 width로 나눈 줄 가운데 offset부터 count줄을 돌려준다. 필요한 줄까지만 나눈다. more는 그 뒤에
// 줄이 더 있는지다.
func traceWrapWindow(lines []string, width, offset, count int) ([]string, bool) {
	if count <= 0 {
		return nil, false
	}
	need := offset + count
	var wrapped []string
	more := false
	for index, line := range lines {
		parts, cut := traceWrapLineUpTo(strings.ReplaceAll(line, "\t", "    "), width, need-len(wrapped))
		wrapped = append(wrapped, parts...)
		if len(wrapped) >= need {
			more = cut || index < len(lines)-1
			break
		}
	}
	if offset >= len(wrapped) {
		return nil, more
	}
	return wrapped[offset:min(need, len(wrapped))], more
}

// traceWrapLineUpTo는 traceWrapLine과 같지만 limit줄을 채우면 멈춘다. cut은 줄의 나머지를 나누지 않았는지다. 한 줄
// JSON 본문은 1MiB까지 길어서, 화면을 그릴 때마다 끝까지 나누면 event를 따라가지 못한다.
func traceWrapLineUpTo(line string, width, limit int) ([]string, bool) {
	if width <= 0 {
		return []string{line}, false
	}
	var parts []string
	var current strings.Builder
	used := 0
	for _, r := range line {
		cell := 1
		if r >= utf8.RuneSelf {
			cell = lipgloss.Width(string(r))
		}
		if used+cell > width && used > 0 {
			parts = append(parts, current.String())
			if len(parts) == limit {
				return parts, true
			}
			current.Reset()
			used = 0
		}
		current.WriteRune(r)
		used += cell
	}
	return append(parts, current.String()), false
}

// traceScreenPreviewRows는 미리 보기 창이다. 첫 줄은 대상 event와 키를 알리는 구분선이다.
func traceScreenPreviewRows(model traceScreenModel, rows int) []string {
	color := os.Getenv("NO_COLOR") == ""
	if model.preview == nil {
		return traceScreenPadRows([]string{liveMuted(traceDivider("── preview: no event", model.width), color)}, rows)
	}
	window, more := traceWrapWindow(model.preview.lines, model.width, model.previewOffset, rows-1)
	// 좁은 화면에서는 뒤가 잘리므로 위치를 제목과 키 안내보다 앞에 둔다.
	title := "── "
	if model.previewOffset > 0 {
		title += fmt.Sprintf("line %d  ·  ", model.previewOffset+1)
	}
	if more {
		title += "more below  ·  "
	}
	title += model.preview.title + "  ·  J/K scroll  enter full"
	if model.protocol == "http" {
		title += "  z gzip"
	}
	return traceScreenPadRows(append([]string{liveMuted(traceDivider(title, model.width), color)}, window...), rows)
}

// traceDivider는 미리 보기 제목 뒤에 남은 폭을 ─로 채운다. 색이 없는 terminal에서도 목록과 미리 보기의 경계가 보인다.
func traceDivider(title string, width int) string {
	title = traceFit(title, width)
	if fill := width - liveWidth(title) - 1; fill > 0 {
		title += " " + strings.Repeat("─", fill)
	}
	return title
}

// scrollPreview는 미리 보기를 step줄 옮긴다. 아래에 더 보일 줄이 없으면 더 내려가지 않는다.
func (model *traceScreenModel) scrollPreview(step int) {
	if model.preview == nil {
		return
	}
	_, rows, ok := traceSplitHeights(model.height)
	if !ok {
		return
	}
	if step > 0 {
		if _, more := traceWrapWindow(model.preview.lines, model.width, model.previewOffset, rows-1); !more {
			return
		}
	}
	model.previewOffset = max(0, model.previewOffset+step)
}
