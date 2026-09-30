package edc

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
)

// traceScreenPayloadBytes는 상세 보기를 위해 payload 전체를 두는 합계 상한이다. 목록은 event마다 앞 4KiB만 두므로,
// 최근 event의 전체는 따로 둔다. 넘으면 오래된 event부터 앞 4KiB만 남는다.
const traceScreenPayloadBytes = 64 << 20

// traceGunzipLimit는 상세 보기가 푸는 본문의 상한이다. 작은 압축 data가 아주 크게 풀리는 경우(zip bomb)를 막는다.
// 제어 문자는 \xNN 네 글자로 보이므로, 화면에 쓰는 글자는 이보다 네 배까지 길다.
const traceGunzipLimit = httpMessageMax

// traceFullPayloads는 event 번호로 찾는 payload다. 목록에서 앞부분만 남긴 payload의 전체와 gzip message의 원본 byte를
// 둔다. minimum보다 긴 것만 둔다.
type traceFullPayloads struct {
	minimum  int
	byNumber map[int]string
	order    []int
	bytes    int
}

func (payloads *traceFullPayloads) keep(number int, payload string) {
	if len(payload) <= payloads.minimum {
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
	number int
	raw    []string
	width  int
	lines  []string
	offset int
}

// newTraceDetail은 event의 필드와 payload를 줄로 만든다. payload는 부르는 쪽이 가리기와 gzip 풀기를 마친 글자다.
func newTraceDetail(event captureEvent, payload string, notes []string, number, width int) *traceDetail {
	destination, name := traceScrollLabels(event)
	detail := &traceDetail{title: fmt.Sprintf("%s  %s  %s", emptyAs(event.Process, "-"), destination, name), number: number}
	fields := event
	fields.Payload = ""
	encoded, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		encoded = []byte(err.Error())
	}
	detail.raw = strings.Split(string(encoded), "\n")
	if payload != "" {
		detail.raw = append(append(detail.raw, "", "payload:"), notes...)
		event.Payload = payload
		detail.raw = append(detail.raw, traceHTTPPayloadBlock(event)...)
	}
	detail.wrap(width)
	return detail
}

// buildDetail은 번호가 number인 event의 상세 보기를 만든다. 목록에서 빠진 event면 nil이다.
func (model traceScreenModel) buildDetail(number int) *traceDetail {
	if number < model.first || number >= model.first+len(model.events) {
		return nil
	}
	event := model.events[number-model.first]
	payload, kept := model.payloads.get(number)
	var notes []string
	if !kept {
		payload = event.Payload
		if len(payload) >= httpPayloadHead {
			notes = append(notes, fmt.Sprintf("(only the first %d bytes: older payloads are not kept)", httpPayloadHead))
		}
	}
	if model.decode && payload != "" {
		raw, ok := model.gzipped.get(number)
		if !ok {
			notes = append(notes, "(gzip: this message is not gzip, or its raw bytes are no longer kept)")
		} else if decoded, note := traceGunzipMessage([]byte(raw)); decoded != "" {
			payload, notes = decoded, append(notes, note)
		} else {
			notes = append(notes, note)
		}
	}
	if !model.secrets && model.protocol == "http" {
		payload = string(traceMaskHTTPHeaders([]byte(payload)))
	}
	return newTraceDetail(event, payload, notes, number, model.width)
}

// rebuildDetail은 가리기나 gzip을 바꾼 뒤 같은 event를 다시 그린다. 보던 위치는 그대로 둔다.
func (model *traceScreenModel) rebuildDetail() {
	if detail := model.buildDetail(model.detail.number); detail != nil {
		detail.offset = model.detail.offset
		model.detail = detail
		model.detail.scroll(0, max(1, model.height-3))
	}
}

// followNewest는 상세 보기를 filter에 맞는 가장 최근 event로 바꾼다. 같은 event면 그대로 둔다.
func (model *traceScreenModel) followNewest() {
	newest := model.moveSelection(-1, 1)
	if newest < 0 || (model.detail != nil && model.detail.number == newest) {
		return
	}
	if detail := model.buildDetail(newest); detail != nil {
		model.detail = detail
	}
}

// traceGunzipMessage는 gzip message의 원본 byte에서 본문을 푼다. chunked면 조각을 먼저 잇는다. 잘린 message는 푼 데까지
// 보여 준다. 돌려주는 글자는 header와 푼 본문을 escape한 것이다.
func traceGunzipMessage(raw []byte) (string, string) {
	head, body, found := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !found {
		return "", "(gzip: the headers did not end, so there is no body)"
	}
	if httpChunkedBody(head) {
		body = httpDechunk(body)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Sprintf("(gzip: cannot read the body: %v)", err)
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, traceGunzipLimit+1))
	note := fmt.Sprintf("(gzip: %d bytes decoded)", len(decoded))
	switch {
	case len(decoded) > traceGunzipLimit:
		decoded = decoded[:traceGunzipLimit]
		note = fmt.Sprintf("(gzip: cut at %d bytes)", traceGunzipLimit)
	case err != nil:
		note = fmt.Sprintf("(gzip: %d bytes decoded, then %v: the rest of the body was not captured)", len(decoded), err)
	}
	text := append(append(slices.Clone(head), "\r\n\r\n"...), decoded...)
	return traceEscapeText(text), note
}

func httpChunkedBody(head []byte) bool {
	for _, line := range bytes.Split(head, []byte("\r\n"))[1:] {
		name, value, ok := bytes.Cut(line, []byte(":"))
		if ok && bytes.EqualFold(bytes.TrimSpace(name), []byte("transfer-encoding")) && bytes.Contains(bytes.ToLower(value), []byte("chunked")) {
			return true
		}
	}
	return false
}

// httpDechunk는 chunked 본문의 조각을 잇는다. 끝나기 전에 잘렸으면 받은 데까지 잇는다.
func httpDechunk(body []byte) []byte {
	var joined []byte
	for len(body) > 0 {
		line, rest, found := bytes.Cut(body, []byte("\r\n"))
		if !found {
			break
		}
		sizeText, _, _ := strings.Cut(string(line), ";")
		size, err := strconv.ParseUint(strings.TrimSpace(sizeText), 16, 32)
		if err != nil || size == 0 {
			break
		}
		part := rest[:min(len(rest), int(size))]
		joined = append(joined, part...)
		if len(rest) < int(size)+2 {
			break
		}
		body = rest[size+2:]
	}
	return joined
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
	model.detail = model.buildDetail(model.selected)
}

// openFollow는 가장 최근 event를 보는 상세 보기를 열고, 새 event가 오면 그 event로 바꾼다.
func (model *traceScreenModel) openFollow() {
	model.follow, model.selected = true, -1
	model.followNewest()
}

func (model traceScreenModel) updateDetailKey(key string) traceScreenModel {
	page := max(1, model.height-3)
	switch key {
	case "esc", "enter", "q", "backspace":
		model.detail, model.follow = nil, false
	case "f":
		// 따라가기를 끄면 보던 event에서 멈추고, 목록으로 돌아가도 그 event를 고른 채로 둔다.
		if model.follow {
			model.follow, model.selected = false, model.detail.number
		} else {
			model.openFollow()
		}
	case "m":
		model.secrets = !model.secrets
		model.rebuildDetail()
	case "z":
		model.decode = !model.decode
		model.rebuildDetail()
	case "up", "k":
		model.detail.scroll(-1, page)
	case "down", "j":
		model.detail.scroll(1, page)
	case "pgup", "b":
		model.detail.scroll(-page, page)
	case "pgdown", "space":
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
	for _, state := range []struct {
		on   bool
		name string
	}{{model.follow, "follow"}, {model.decode, "gzip decoded"}, {model.secrets && model.protocol == "http", "secrets shown"}} {
		if state.on {
			header += "  ·  " + state.name
		}
	}
	// Mac 자판에는 PgUp, PgDn, Home, End가 없는 경우가 많아, 어디서나 쓸 수 있는 키를 안내한다. 원래 키도 그대로 된다.
	help := "↑↓ scroll  space/b page  g/G top/end  f follow  esc back  ctrl-c stop"
	if model.protocol == "http" {
		help = "↑↓ scroll  space/b page  g/G top/end  f follow  m secrets  z gzip  esc back  ctrl-c stop"
	}
	rows := []string{liveSelected(traceFit(header, model.width), color), liveMuted(traceFit(help, model.width), color), traceFit(detail.title, model.width)}
	rows = append(rows, detail.lines[detail.offset:last]...)
	return traceScreenPadRows(rows, model.height)
}
