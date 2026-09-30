package edc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// traceScreenPayloadBytes는 상세 보기를 위해 payload 전체를 두는 합계 상한이다. 목록은 event마다 앞 4KiB만 두므로,
// 최근 event의 전체는 따로 둔다. 넘으면 오래된 event부터 앞 4KiB만 남는다.
const traceScreenPayloadBytes = 64 << 20

// traceFullPayloads는 목록에서 앞부분만 남긴 payload의 전체다. event 번호로 찾는다.
type traceFullPayloads struct {
	byNumber map[int]string
	order    []int
	bytes    int
}

func (payloads *traceFullPayloads) keep(number int, payload string) {
	if len(payload) <= httpPayloadHead {
		return
	}
	if payloads.byNumber == nil {
		payloads.byNumber = map[int]string{}
	}
	payloads.byNumber[number] = payload
	payloads.order = append(payloads.order, number)
	payloads.bytes += len(payload)
	for payloads.bytes > traceScreenPayloadBytes && len(payloads.order) > 0 {
		payloads.forgetOldest()
	}
}

// drop은 목록에서 빠진 event의 payload를 버린다.
func (payloads *traceFullPayloads) drop(before int) {
	for len(payloads.order) > 0 && payloads.order[0] < before {
		payloads.forgetOldest()
	}
}

func (payloads *traceFullPayloads) forgetOldest() {
	number := payloads.order[0]
	payloads.order = payloads.order[1:]
	payloads.bytes -= len(payloads.byNumber[number])
	delete(payloads.byNumber, number)
}

func (payloads traceFullPayloads) get(number int) (string, bool) {
	payload, ok := payloads.byNumber[number]
	return payload, ok
}

// traceDetail은 event 하나를 보는 화면이다. raw는 줄 바꿈 전의 줄이고, lines는 width에 맞춰 나눈 줄이다.
type traceDetail struct {
	title  string
	raw    []string
	width  int
	lines  []string
	offset int
}

// newTraceDetail은 event의 필드와 payload 전체를 줄로 만든다. 목록의 payload는 앞 4KiB라서 full이 있으면 그것을 쓴다.
func newTraceDetail(event captureEvent, full string, kept bool, width int) *traceDetail {
	destination, name := traceScrollLabels(event)
	detail := &traceDetail{title: fmt.Sprintf("%s  %s  %s", emptyAs(event.Process, "-"), destination, name)}
	payload := event.Payload
	if kept {
		payload = full
	}
	fields := event
	fields.Payload = ""
	encoded, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		encoded = []byte(err.Error())
	}
	detail.raw = strings.Split(string(encoded), "\n")
	if payload != "" {
		detail.raw = append(detail.raw, "", "payload:")
		if !kept && len(payload) >= httpPayloadHead {
			detail.raw = append(detail.raw, fmt.Sprintf("(only the first %d bytes: older payloads are not kept)", httpPayloadHead))
		}
		event.Payload = payload
		detail.raw = append(detail.raw, traceHTTPPayloadBlock(event)...)
	}
	detail.wrap(width)
	return detail
}

func (detail *traceDetail) wrap(width int) {
	detail.width = width
	detail.lines = detail.lines[:0]
	for _, line := range detail.raw {
		detail.lines = append(detail.lines, traceWrapLine(strings.ReplaceAll(line, "\t", "    "), width)...)
	}
}

// scroll은 offset을 옮기고, 마지막 쪽이 화면을 채우도록 범위 안에 둔다.
func (detail *traceDetail) scroll(step, page int) {
	detail.offset = max(0, min(detail.offset+step, len(detail.lines)-page))
}

// traceWrapLine은 line을 width 표시 폭마다 나눈다. 본문은 한 줄 JSON처럼 길어서, 자르면 상세 보기에서 볼 수 없다.
func traceWrapLine(line string, width int) []string {
	if width <= 0 || liveWidth(line) <= width {
		return []string{line}
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
			current.Reset()
			used = 0
		}
		current.WriteRune(r)
		used += cell
	}
	return append(parts, current.String())
}

// moveSelection은 고른 event에서 filter에 맞는 event를 step 방향으로 count개 건너간 번호다. 고른 event가 없으면
// 가장 최근 event부터 센다. 더 갈 곳이 없으면 그대로 둔다.
func (model traceScreenModel) moveSelection(step, count int) int {
	index := len(model.events)
	if model.selected >= 0 {
		index = model.selected - model.first
	}
	found := model.selected
	for index += step; index >= 0 && index < len(model.events) && count > 0; index += step {
		if traceEventMatchesText(model.events[index], model.filter) {
			found, count = model.first+index, count-1
		}
	}
	return found
}

// openDetail은 고른 event의 상세 보기를 연다. 고른 event가 없으면 가장 최근 event를 고른다.
func (model *traceScreenModel) openDetail() {
	if model.selected < 0 {
		model.selected = model.moveSelection(-1, 1)
	}
	if model.selected < 0 {
		return
	}
	full, kept := model.payloads.get(model.selected)
	model.detail = newTraceDetail(model.events[model.selected-model.first], full, kept, model.width)
}

func (model traceScreenModel) updateDetailKey(key string) traceScreenModel {
	page := max(1, model.height-3)
	switch key {
	case "esc", "enter", "q", "backspace":
		model.detail = nil
	case "up", "k":
		model.detail.scroll(-1, page)
	case "down", "j":
		model.detail.scroll(1, page)
	case "pgup", "b":
		model.detail.scroll(-page, page)
	case "pgdown", "space", "f":
		model.detail.scroll(page, page)
	case "home", "g":
		model.detail.scroll(-len(model.detail.lines), page)
	case "end", "G":
		model.detail.scroll(len(model.detail.lines), page)
	}
	return model
}

func traceScreenDetailView(model traceScreenModel) []string {
	detail := model.detail
	page := max(0, model.height-3)
	last := min(len(detail.lines), detail.offset+page)
	color := os.Getenv("NO_COLOR") == ""
	header := fmt.Sprintf("edc trace %s  ·  detail  ·  lines %d-%d of %d", traceLabel(model.protocol, model.side), min(detail.offset+1, last), last, len(detail.lines))
	help := "↑↓ scroll  pgup/pgdn page  home/end  esc back  ctrl-c stop"
	rows := []string{liveSelected(traceFit(header, model.width), color), liveMuted(traceFit(help, model.width), color), traceFit(detail.title, model.width)}
	rows = append(rows, detail.lines[detail.offset:last]...)
	return traceScreenPadRows(rows, model.height)
}
