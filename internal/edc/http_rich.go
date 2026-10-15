package edc

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// http_rich.go는 색을 쓰는 터미널에서 http.check의 requests와 body evidence 대신 그리는 패널이다.
// --redact를 거친 Result도 받으므로 evidence 문자열이 아니라 Metrics에서 그리고, JSON을 한 번 거친 타입도 읽는다.

func init() {
	richDetailRenderers["http.check"] = richHTTPCheck
}

// httpRichPhaseColors는 구간 막대와 범례의 색이다.
var httpRichPhaseColors = map[string]string{"dns_ms": richBlue, "connect_ms": richCyan, "tls_ms": richMagenta, "ttfb_ms": richGreen}

const httpRichMinCells = 8

type httpRichHop struct {
	url, proto, peer            string
	status                      int
	proxy, reused, decompressed bool
	timings                     map[string]float64
	headers, requestHeaders     map[string][]string
}

func (hop httpRichHop) total() float64 {
	total := 0.0
	for _, value := range hop.timings {
		total += math.Max(value, 0)
	}
	return total
}

func (hop httpRichHop) details() string {
	var items []string
	for _, phase := range httpTimingLabels {
		if value, ok := hop.timings[phase.key]; ok {
			items = append(items, fmt.Sprintf("%s %d", phase.label, int64(value)))
		}
	}
	if hop.reused {
		items = append(items, T("observe.http.reused"))
	}
	if hop.peer != "" {
		key := "observe.probe.peer_ip"
		if hop.proxy {
			key = "observe.probe.proxy_peer_ip"
		}
		items = append(items, T(key, hop.peer))
	}
	if hop.decompressed {
		items = append(items, T("observe.http.decompressed"))
	}
	return strings.Join(items, " · ")
}

// richHTTPCheck는 실패면 output.go의 ERROR 상자 안에 들어갈 hop 줄만, 아니면 패널 전체를 돌려준다.
// 두 경우 모두 앞에 "│ "가 붙으므로 내용 폭은 width-2다.
func richHTTPCheck(result Result, width int) []string {
	hops := httpRichHops(result.Metrics["hops"])
	if len(hops) == 0 {
		return nil
	}
	inner := width - 2
	if result.Status == StatusFail {
		return httpRichHopRows(hops, inner)
	}
	var lines []string
	// --body만 주고 redirect가 없으면 평문에도 requests 블록이 없다. 같은 내용만 그린다.
	if httpRichHasEvidence(result, T("observe.http.requests")) {
		lines = append(lines, httpRichPanelTop(T("observe.http.requests"), httpRichLegend(hops), width))
		for _, row := range httpRichHopRows(hops, inner) {
			lines = append(lines, richPanelLine(row, ""))
		}
		lines = append(lines, richPanelBottom(""))
	}
	_, hasType := result.Metrics["content_type"]
	_, hasOutput := result.Metrics["body_output"]
	if hasType || hasOutput {
		detail, body := httpRichBody(result.Metrics, hops[len(hops)-1], inner)
		lines = append(lines, httpRichPanelTop(T("observe.http.body"), detail, width))
		for _, row := range body {
			lines = append(lines, richPanelLine(row, ""))
		}
		lines = append(lines, richPanelBottom(""))
	}
	return lines
}

func httpRichHasEvidence(result Result, label string) bool {
	return slices.ContainsFunc(result.Evidence, func(evidence Evidence) bool { return evidence.Label == label })
}

// httpRichPanelTop은 제목 줄이 폭을 넘으면 detail을 줄이고, 그래도 넘으면 detail을 뺀다.
func httpRichPanelTop(title, detail string, width int) string {
	top := richPanelTop(title, detail, "")
	if liveWidth(top) <= width {
		return top
	}
	bare := richPanelTop(title, "", "")
	if room := width - liveWidth(bare) - 2; room >= httpMinValueWidth && !strings.Contains(detail, "\033") {
		return richPanelTop(title, ansi.Truncate(detail, room, "…"), "")
	}
	return ansi.Truncate(bare, width, "")
}

// httpRichLegend는 hop 중 하나라도 잰 구간만 범례에 둔다. 사각형 뒤에 reset이 오므로 이름은 따로 회색을 입힌다.
func httpRichLegend(hops []httpRichHop) string {
	var items []string
	for _, phase := range httpTimingLabels {
		if slices.ContainsFunc(hops, func(hop httpRichHop) bool { _, ok := hop.timings[phase.key]; return ok }) {
			items = append(items, richPaint("■", httpRichPhaseColors[phase.key], false)+richDim(" "+phase.label))
		}
	}
	return strings.Join(items, "  ")
}

// httpRichHopRows는 hop마다 status와 URL, 구간 막대와 세부 값, -v의 header를 쓰고 hop 사이에 빈 줄을 둔다.
// 막대는 가장 오래 걸린 hop을 기준으로 같은 축에 그리고, 합계 ms가 같은 열에 오도록 맞춘다.
func httpRichHopRows(hops []httpRichHop, width int) []string {
	numberWidth := len(strconv.Itoa(len(hops)))
	pad := strings.Repeat(" ", numberWidth+5)
	maxCells := max(httpRichMinCells, width*3/10)
	largest := 0.0
	for _, hop := range hops {
		largest = math.Max(largest, hop.total())
	}
	bars := make([]string, len(hops))
	barWidth, totalWidth := 0, 0
	for index, hop := range hops {
		if len(hop.timings) == 0 {
			continue
		}
		totalWidth = max(totalWidth, len(httpRichMS(hop.total())))
		if largest <= 0 {
			continue
		}
		segments := make([]richSegment, 0, len(httpTimingLabels))
		for _, phase := range httpTimingLabels {
			segments = append(segments, richSegment{hop.timings[phase.key], httpRichPhaseColors[phase.key]})
		}
		bars[index] = richSegments(segments, max(int(math.Round(hop.total()/largest*float64(maxCells))), 1))
		barWidth = max(barWidth, liveWidth(bars[index]))
	}
	var rows []string
	for index, hop := range hops {
		if index > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, httpRichHopHead(index+1, numberWidth, hop, width))
		details := hop.details()
		switch {
		case len(hop.timings) > 0:
			line := pad
			// 로컬 요청처럼 모든 구간이 1ms 아래면 막대 없이 합계만 남는다.
			if barWidth > 0 {
				line += bars[index] + strings.Repeat(" ", barWidth-liveWidth(bars[index])) + "  "
			}
			line += richBold(fmt.Sprintf("%*s", totalWidth, httpRichMS(hop.total())))
			if details != "" {
				line += "  " + richDim(details)
			}
			rows = append(rows, ansi.Truncate(line, width, "…"))
		case details != "":
			rows = append(rows, ansi.Truncate(pad+richDim(details), width, "…"))
		}
		rows = append(rows, httpRichHeaderRows(hop, pad, width)...)
	}
	return rows
}

func httpRichMS(value float64) string {
	return fmt.Sprintf("%d ms", int64(value))
}

// httpRichHopHead는 번호, status 배지, URL, protocol이다. URL을 먼저 줄여 protocol이 남게 한다.
func httpRichHopHead(number, numberWidth int, hop httpRichHop, width int) string {
	lead := richDim(fmt.Sprintf("%*d", numberWidth, number)) + " " + richBadge(strconv.Itoa(hop.status), richHTTPCodeColor(hop.status)) + "  "
	proto := ""
	if hop.proto != "" {
		proto = "  " + richDim(traceEscapeText([]byte(hop.proto)))
	}
	room := max(width-liveWidth(lead)-liveWidth(proto), httpMinValueWidth)
	url := ansi.Truncate(traceEscapeText([]byte(hop.url)), room, "…")
	return ansi.Truncate(lead+richSGR(url, "1", "4")+proto, width, "…")
}

// httpRichHeaderRows는 formatHTTPHops의 header 줄과 같은 배치다. 보낸 header는 노랑, 받은 header 이름은 회색이다.
// metrics의 값은 이미 가리고 escape했다.
func httpRichHeaderRows(hop httpRichHop, pad string, width int) []string {
	type row struct {
		name, value string
		sent        bool
	}
	var rows []row
	marker := ""
	if len(hop.requestHeaders) > 0 {
		marker = "  "
		for _, name := range httpRichHeaderNames(hop.requestHeaders) {
			for _, value := range hop.requestHeaders[name] {
				rows = append(rows, row{"> " + name, value, true})
			}
		}
	}
	for _, name := range httpRichHeaderNames(hop.headers) {
		for _, value := range hop.headers[name] {
			rows = append(rows, row{marker + name, value, false})
		}
	}
	nameWidth := 0
	for _, row := range rows {
		nameWidth = max(nameWidth, liveWidth(row.name))
	}
	valueWidth := max(width-len(pad)-nameWidth-2, httpMinValueWidth)
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		name := marker + richDim(strings.TrimPrefix(row.name, marker))
		if row.sent {
			name = richPaint(row.name, richYellow, false)
		}
		line := pad + name + strings.Repeat(" ", nameWidth-liveWidth(row.name)+2) + ansi.Truncate(row.value, valueWidth, "…")
		lines = append(lines, ansi.Truncate(line, width, "…"))
	}
	return lines
}

func httpRichHeaderNames(header map[string][]string) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// httpRichBody는 body 패널의 제목 옆 정보와 안쪽 줄이다. formatHTTPBody와 withHTTPBodyOutput이 쓰는 내용과 같다.
func httpRichBody(metrics map[string]interface{}, last httpRichHop, width int) (string, []string) {
	size, _ := richNumber(metrics["bytes_read"])
	contentType, hasType := metrics["content_type"].(string)
	preview, hasPreview := metrics["body_preview"].(string)
	var parts []string
	if hasType {
		if contentType == "" {
			contentType = T("observe.http.no_content_type")
		}
		parts = append(parts, traceEscapeText([]byte(contentType)))
	}
	parts = append(parts, T("observe.http.body_bytes", int64(size)))
	if lines, ok := richNumber(metrics["body_lines"]); ok && hasPreview {
		parts = append(parts, T("observe.http.body_lines", int64(lines)))
	}
	var lines []string
	if title, _ := metrics["title"].(string); title != "" {
		lines = append(lines, richBold(ansi.Truncate(traceEscapeText([]byte(title)), width, "…")))
	}
	switch {
	case hasPreview && strings.TrimSpace(preview) != "":
		for index, line := range strings.Split(strings.TrimRight(preview, "\n"), "\n") {
			if index == httpBodyPreviewLines {
				lines = append(lines, richDim("…"))
				break
			}
			lines = append(lines, richDim(ansi.Truncate(strings.ReplaceAll(line, "\t", "  "), width, "…")))
		}
	case hasType && !hasPreview && size > 0:
		note := T("observe.http.binary")
		if encodings := last.headers["content-encoding"]; len(encodings) > 0 && !strings.EqualFold(encodings[0], "identity") {
			note = T("observe.http.encoded", encodings[0])
		}
		lines = append(lines, richDim(ansi.Truncate(note, width, "…")))
	}
	if output, ok := metrics["body_output"].(string); ok {
		line := T("observe.http.body_saved", output, int64(size))
		if output == "-" {
			line = T("observe.http.body_stdout", int64(size))
		}
		lines = append(lines, ansi.Truncate(line, width, "…"))
		if truncated, _ := metrics["body_truncated"].(bool); truncated {
			lines = append(lines, richPaint(ansi.Truncate(T("observe.http.body_truncated", "10 MiB"), width, "…"), richYellow, false))
		}
	}
	return strings.Join(parts, " · "), lines
}

// httpRichHops는 Metrics["hops"]를 읽는다. probe가 만든 []map[string]interface{}와
// --redact의 JSON 왕복을 거친 []interface{}를 모두 받는다.
func httpRichHops(value interface{}) []httpRichHop {
	var rows []map[string]interface{}
	switch list := value.(type) {
	case []map[string]interface{}:
		rows = list
	case []interface{}:
		for _, item := range list {
			if row, ok := item.(map[string]interface{}); ok {
				rows = append(rows, row)
			}
		}
	}
	hops := make([]httpRichHop, 0, len(rows))
	for _, row := range rows {
		hop := httpRichHop{timings: map[string]float64{}}
		hop.url, _ = row["url"].(string)
		hop.proto, _ = row["proto"].(string)
		hop.peer, _ = row["peer_ip"].(string)
		status, _ := richNumber(row["status_code"])
		hop.status = int(status)
		hop.proxy, _ = row["peer_is_proxy"].(bool)
		hop.reused, _ = row["reused_connection"].(bool)
		hop.decompressed, _ = row["decompressed"].(bool)
		for _, phase := range httpTimingLabels {
			if value, ok := richNumber(row[phase.key]); ok {
				hop.timings[phase.key] = value
			}
		}
		hop.headers = httpRichHeaders(row["headers"])
		hop.requestHeaders = httpRichHeaders(row["request_headers"])
		hops = append(hops, hop)
	}
	return hops
}

func httpRichHeaders(value interface{}) map[string][]string {
	switch header := value.(type) {
	case map[string][]string:
		return header
	case map[string]interface{}:
		values := make(map[string][]string, len(header))
		for name, list := range header {
			items, _ := list.([]interface{})
			for _, item := range items {
				if text, ok := item.(string); ok {
					values[name] = append(values[name], text)
				}
			}
		}
		return values
	}
	return nil
}
