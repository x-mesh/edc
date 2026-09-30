package edc

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// httpPayloadHead는 --payload가 message마다 읽는 byte 수다. 전체 화면도 event마다 이만큼만 보관한다.
	httpPayloadHead = 4096
	// httpMessageMax는 --payload=all이 message 하나를 따라가는 byte 수다. 큰 업로드 하나가 ring buffer와 메모리를
	// 다 쓰지 않도록 둔다.
	httpMessageMax = 1 << 20
	// httpMessageIdle은 조각이 더 오지 않는 message를 내보내기까지 기다리는 시간이다. 길이를 알 수 없는 응답은 연결이
	// 닫혀야 끝나는데, trace http는 연결 종료를 보지 않는다.
	httpMessageIdle = time.Second
	// httpMessageOpenBytes와 httpMessageOpenLimit는 끝나기를 기다리는 message의 합계 상한이다. 넘으면 오래된 것부터 낸다.
	httpMessageOpenBytes  = 64 << 20
	httpMessageOpenLimit  = 4096
	traceHTTPRequestEvent = "http_request"
	// traceHTTPPendingLimit은 응답을 기다리는 요청 수의 상한이다. 응답을 읽지 못한 요청이 쌓여도 메모리를 제한한다.
	traceHTTPPendingLimit = 65536
)

// traceHTTPMethods는 BPF가 앞 4바이트로 거르는 method와 같다. 사용자 공간은 요청 줄 전체를 다시 확인한다.
var traceHTTPMethods = []string{"GET", "POST", "PUT", "HEAD", "DELETE", "PATCH", "OPTIONS"}

// httpPacket은 kernel이 TCP로 주고받은 HTTP message의 앞부분이다. source는 로컬 쪽, destination은 상대 쪽이다.
type httpPacket struct {
	bootTimeNS  uint64
	pid         uint32
	cgroupID    uint64
	process     string
	sent        bool
	socket      uint64
	source      string
	destination string
	payload     []byte
	// continued는 앞 message에 이어지는 조각이다. --payload=all이 아니면 첫 줄이 끊긴 첫 조각 뒤에만 온다. offset은 조각이
	// message 안에서 시작하는 위치다.
	continued bool
	offset    uint32
}

type httpPendingRequest struct {
	bootTimeNS uint64
	method     string
	host       string
	path       string
}

// httpTracker는 socket마다 요청을 순서대로 두고 응답과 짝짓는다. HTTP/1.x는 한 연결에서 요청 순서대로 응답한다.
// server는 tracker가 볼 쪽이다. 다른 쪽 message는 기억하지 않는다.
type httpTracker struct {
	server      bool
	payload     bool
	showSecrets bool
	// keepGzip이면 gzip message의 원본 byte를 event에 붙인다. 전체 화면만 켠다.
	keepGzip bool
	pending  map[uint64][]httpPendingRequest
	size     int
}

func newHTTPTracker(server, payload, showSecrets bool) *httpTracker {
	return &httpTracker{server: server, payload: payload, showSecrets: showSecrets, pending: map[uint64][]httpPendingRequest{}}
}

func (tracker *httpTracker) event(packet httpPacket, clockOffset int64) (captureEvent, bool) {
	method, target, host, requestOK := parseHTTPRequest(packet.payload)
	status, responseOK := parseHTTPStatus(packet.payload)
	if !requestOK && !responseOK {
		return captureEvent{}, false
	}
	// client는 요청을 보내고 응답을 받는다. 서버는 요청을 받고 응답을 보낸다.
	server := requestOK != packet.sent
	if server != tracker.server {
		return captureEvent{}, false
	}
	event := captureEvent{
		SocketID: packet.socket, TimestampNS: uint64(int64(packet.bootTimeNS) + clockOffset), BootTimeNS: packet.bootTimeNS, Protocol: "http",
		PID: packet.pid, Process: packet.process, CgroupID: packet.cgroupID, Source: packet.source, Destination: packet.destination, Bytes: uint64(len(packet.payload)),
	}
	if server {
		event.Side = traceServerSide
	}
	if tracker.payload {
		event.Payload = traceHTTPPayload(packet.payload, tracker.showSecrets)
		if tracker.keepGzip && httpGzipped(packet.payload) {
			event.gzipped = slices.Clone(packet.payload)
		}
	}
	if requestOK {
		host, path := traceHTTPTarget(target, host)
		event.Event, event.Method, event.Path, event.Target = traceHTTPRequestEvent, method, path, emptyAs(host, traceHTTPHost(packet.destination, server))
		if tracker.size >= traceHTTPPendingLimit {
			// 이때 버린 요청의 응답은 응답 시간 없이 보인다.
			clear(tracker.pending)
			tracker.size = 0
		}
		tracker.pending[packet.socket] = append(tracker.pending[packet.socket], httpPendingRequest{bootTimeNS: packet.bootTimeNS, method: method, host: event.Target, path: path})
		tracker.size++
		return event, true
	}
	event.Event, event.Status = traceHTTPStatusEvent(status), status
	queue := tracker.pending[packet.socket]
	if len(queue) == 0 {
		event.Target = traceHTTPHost(packet.destination, server)
		return event, true
	}
	request := queue[0]
	event.Method, event.Path, event.Target = request.method, request.path, request.host
	// 1xx는 중간 응답이다. 최종 응답이 같은 요청에 다시 온다.
	if status >= 200 {
		event.LatencyMS = traceSpan(request.bootTimeNS, packet.bootTimeNS)
		event.answered = 1
		tracker.pending[packet.socket] = queue[1:]
		if len(queue) == 1 {
			delete(tracker.pending, packet.socket)
		}
		tracker.size--
	}
	return event, true
}

// parseHTTPRequest는 요청 줄과 Host header를 읽는다. 요청 줄이 "METHOD target HTTP/1.x"가 아니면 HTTP가 아니다.
func parseHTTPRequest(payload []byte) (string, string, string, bool) {
	line, rest, ok := bytes.Cut(payload, []byte("\r\n"))
	if !ok {
		return "", "", "", false
	}
	fields := strings.Split(string(line), " ")
	if len(fields) != 3 || !slices.Contains(traceHTTPMethods, fields[0]) || (fields[2] != "HTTP/1.1" && fields[2] != "HTTP/1.0") || fields[1] == "" {
		return "", "", "", false
	}
	host := ""
	for _, header := range bytes.Split(rest, []byte("\r\n")) {
		name, value, ok := bytes.Cut(header, []byte(":"))
		if ok && strings.EqualFold(string(name), "host") {
			host = strings.ToLower(strings.TrimSpace(string(value)))
			break
		}
	}
	return fields[0], fields[1], host, true
}

// parseHTTPStatus는 "HTTP/1.x NNN" 상태 줄의 코드를 읽는다.
func parseHTTPStatus(payload []byte) (int, bool) {
	if len(payload) < 12 || (!bytes.HasPrefix(payload, []byte("HTTP/1.1 ")) && !bytes.HasPrefix(payload, []byte("HTTP/1.0 "))) {
		return 0, false
	}
	status, err := strconv.Atoi(string(payload[9:12]))
	if err != nil || status < 100 || status > 599 || (len(payload) > 12 && payload[12] != ' ' && payload[12] != '\r') {
		return 0, false
	}
	return status, true
}

// traceHTTPTarget은 요청 target에서 query와 fragment를 뺀 path를 돌려준다. query에는 token 같은 값이 들어 있기도 하다.
// proxy에 보내는 절대 형식(http://host/path)이면 host도 여기서 읽는다.
func traceHTTPTarget(target, host string) (string, string) {
	if rest, ok := strings.CutPrefix(target, "http://"); ok {
		authority, path, _ := strings.Cut(rest, "/")
		host, target = emptyAs(host, strings.ToLower(authority)), "/"+path
	}
	if index := strings.IndexAny(target, "?#"); index >= 0 {
		target = target[:index]
	}
	return host, target
}

// traceHTTPSecretHeaders는 --payload에서도 값을 가리는 header다. 인증 정보라서, 출력을 log나 issue에 옮기면 그대로 샌다.
var traceHTTPSecretHeaders = []string{"authorization", "proxy-authorization", "cookie", "set-cookie"}

// traceHTTPPayload는 --payload로 보여 줄 message 앞부분이다. BPF가 앞부분만 읽으므로 header 끝을 못 봤으면
// 마지막 header 줄이 CRLF 없이 값 중간에서 끊겨 있다. 그 줄도 header로 보고 가린다.
func traceHTTPPayload(payload []byte, showSecrets bool) string {
	if !showSecrets {
		payload = traceMaskHTTPHeaders(payload)
	}
	return traceEscapeText(payload)
}

// traceMaskHTTPHeaders는 traceHTTPSecretHeaders의 값을 ***로 바꾼다. escape한 글자에도 쓸 수 있다. escape는 header
// 이름과 CRLF를 바꾸지 않으므로, 전체 화면은 원문을 두었다가 그릴 때 가린다.
func traceMaskHTTPHeaders(payload []byte) []byte {
	head, body, complete := bytes.Cut(payload, []byte("\r\n\r\n"))
	lines := bytes.Split(head, []byte("\r\n"))
	for index := 1; index < len(lines); index++ {
		name, _, ok := bytes.Cut(lines[index], []byte(":"))
		if ok && slices.ContainsFunc(traceHTTPSecretHeaders, func(secret string) bool { return strings.EqualFold(string(bytes.TrimSpace(name)), secret) }) {
			lines[index] = append(slices.Clip(name), ": ***"...)
		}
	}
	text := bytes.Join(lines, []byte("\r\n"))
	if complete {
		text = append(append(text, "\r\n\r\n"...), body...)
	}
	return text
}

// httpGzipped는 header에 Content-Encoding: gzip이 있는 message다. 전체 화면은 이런 message만 원본 byte를 두었다가
// 상세 보기에서 푼다. escape한 글자로는 byte를 되돌릴 수 없다. 원문의 \x41 같은 글자와 escape를 구분할 수 없기 때문이다.
func httpGzipped(payload []byte) bool {
	head, _, _ := bytes.Cut(payload, []byte("\r\n\r\n"))
	for _, line := range bytes.Split(head, []byte("\r\n"))[1:] {
		name, value, ok := bytes.Cut(line, []byte(":"))
		if ok && bytes.EqualFold(bytes.TrimSpace(name), []byte("content-encoding")) && bytes.Contains(bytes.ToLower(value), []byte("gzip")) {
			return true
		}
	}
	return false
}

// traceEscapeText는 제어 문자와 UTF-8이 아닌 byte를 \xNN으로 바꾼다. payload는 상대가 보낸 값이라, 그대로 찍으면
// terminal escape sequence가 실행될 수 있다. 줄바꿈과 tab은 message 모양을 지키려고 남긴다.
// --payload는 4KB까지 읽으므로 이 함수가 event 처리 시간의 대부분이다. 문자를 하나씩 쓰면 초당 수만 건에서 event를
// 따라가지 못해, 바꿀 것이 없는 앞부분은 한 번에 복사하고 ASCII는 byte 단위로 처리한다.
func traceEscapeText(text []byte) string {
	clean := 0
	for clean < len(text) && traceTextByteKept(text[clean]) {
		clean++
	}
	if clean == len(text) {
		return string(text)
	}
	var out strings.Builder
	out.Grow(len(text) + 16)
	out.Write(text[:clean])
	for text = text[clean:]; len(text) > 0; {
		if value := text[0]; value < utf8.RuneSelf {
			if traceTextByteKept(value) {
				out.WriteByte(value)
			} else {
				fmt.Fprintf(&out, `\x%02x`, value)
			}
			text = text[1:]
			continue
		}
		r, size := utf8.DecodeRune(text)
		if (r == utf8.RuneError && size == 1) || (r >= 0x80 && r < 0xa0) {
			for _, value := range text[:size] {
				fmt.Fprintf(&out, `\x%02x`, value)
			}
		} else {
			out.Write(text[:size])
		}
		text = text[size:]
	}
	return out.String()
}

// traceTextByteKept는 그대로 두는 ASCII byte다. UTF-8의 첫 byte(0x80 이상)는 false라서 느린 경로가 확인한다.
func traceTextByteKept(value byte) bool {
	return (value >= 0x20 && value < 0x7f) || value == '\r' || value == '\n' || value == '\t'
}

// traceHTTPPayloadLine은 payload를 event 행 아래 한 줄로 보여 준다. 요청 줄과 상태 줄은 event 행에 이미 있으므로 빼고,
// 본문이 있으면 본문을, 없으면 header를 보인다. 전체는 --raw의 payload에 있다.
func traceHTTPPayloadLine(payload string) string {
	head, body, _ := strings.Cut(payload, "\r\n\r\n")
	if body != "" {
		return "body " + strings.NewReplacer("\r\n", " ↵ ", "\n", " ↵ ", "\r", " ", "\t", " ").Replace(body)
	}
	_, headers, _ := strings.Cut(head, "\r\n")
	return "headers " + emptyAs(strings.ReplaceAll(strings.TrimSuffix(headers, "\r\n"), "\r\n", " · "), "-")
}

// traceHTTPPayloadSummary는 한 줄 요약에 잘림 표시를 붙인다. 긴 본문은 화면 폭에서 잘리므로 표시를 앞에 둔다.
func traceHTTPPayloadSummary(event captureEvent) string {
	line := traceHTTPPayloadLine(event.Payload)
	if event.PayloadTruncated {
		line = "[truncated] " + line
	}
	return line
}

// traceHTTPPayloadBlock은 payload를 줄마다 나눈다. 마지막 줄바꿈 뒤의 빈 줄은 뺀다.
func traceHTTPPayloadBlock(event captureEvent) []string {
	lines := strings.Split(strings.ReplaceAll(event.Payload, "\r\n", "\n"), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if event.PayloadTruncated {
		lines = append(lines, "[truncated]")
	}
	return lines
}

// traceTrimText는 text를 limit byte 안에서 UTF-8 문자 경계로 자른다.
func traceTrimText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// traceHTTPHost는 Host header가 없을 때 쓸 상대 주소다. 서버 쪽 상대는 client라서 로컬 주소를 쓴다.
func traceHTTPHost(destination string, server bool) string {
	if server {
		return ""
	}
	return destination
}

func traceHTTPStatusEvent(status int) string {
	return fmt.Sprintf("http_%dxx", status/100)
}

// traceHTTPCounts는 HTTP group 행과 요약이 함께 쓰는 값이다.
type traceHTTPCounts struct {
	Requests     uint64   `json:"requests"`
	Responses    uint64   `json:"responses"`
	ClientErrors uint64   `json:"client_errors"`
	ServerErrors uint64   `json:"server_errors"`
	Unanswered   uint64   `json:"unanswered"`
	LatencyAvgMS *float64 `json:"latency_avg_ms"`
	LatencyMaxMS *float64 `json:"latency_max_ms"`
	answered     uint64
	latency      traceSpans
}

func (counts *traceHTTPCounts) observe(event captureEvent) {
	if event.Event == traceHTTPRequestEvent {
		counts.Requests++
		return
	}
	counts.Responses++
	switch event.Status / 100 {
	case 4:
		counts.ClientErrors++
	case 5:
		counts.ServerErrors++
	}
	counts.answered += event.answered
	counts.latency.observe(event.LatencyMS)
}

func (counts traceHTTPCounts) finished() traceHTTPCounts {
	counts.Unanswered = counts.Requests - min(counts.Requests, counts.answered)
	counts.LatencyAvgMS, counts.LatencyMaxMS = counts.latency.summary()
	return counts
}

func traceHTTPGroupSummary(counts *traceHTTPCounts) *traceHTTPCounts {
	if counts == nil {
		return nil
	}
	finished := counts.finished()
	return &finished
}

func traceHTTPColumn(value func(counts traceHTTPCounts) string) func(traceGroupSummary) string {
	return func(group traceGroupSummary) string {
		if group.HTTP == nil {
			return "-"
		}
		return value(*group.HTTP)
	}
}

var traceHTTPGroupColumns = []traceGroupColumn{
	{screenTitle: "REQ", reportTitle: "REQUESTS", width: 5, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return strconv.FormatUint(counts.Requests, 10) })},
	{screenTitle: "RSP", reportTitle: "RESPONSES", width: 5, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return strconv.FormatUint(counts.Responses, 10) })},
	{screenTitle: "4XX", reportTitle: "4XX", width: 4, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return strconv.FormatUint(counts.ClientErrors, 10) })},
	{screenTitle: "5XX", reportTitle: "5XX", width: 4, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return strconv.FormatUint(counts.ServerErrors, 10) })},
	{screenTitle: "NOANS", reportTitle: "UNANSWERED", width: 5, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return strconv.FormatUint(counts.Unanswered, 10) })},
	{screenTitle: "AVGms", reportTitle: "AVG_MS", width: 6, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return traceLatency(counts.LatencyAvgMS, "") })},
	{screenTitle: "MAXms", reportTitle: "MAX_MS", width: 6, value: traceHTTPColumn(func(counts traceHTTPCounts) string { return traceLatency(counts.LatencyMaxMS, "") })},
}

// traceHTTPScrollLabels는 목적지 칸에 method, host, path, 상대 주소를, event 칸에 결과와 상태 코드, 응답 시간을 쓴다.
func traceHTTPScrollLabels(event captureEvent) (string, string) {
	request := strings.TrimSpace(event.Method + " " + event.Target + event.Path)
	if event.Destination != "" && event.Destination != event.Target {
		request += " (" + event.Destination + ")"
	}
	label := event.Event
	if event.Status != 0 {
		label += " " + strconv.Itoa(event.Status)
	}
	if event.LatencyMS != nil {
		label += " " + traceLatency(event.LatencyMS, "ms")
	}
	return emptyAs(request, "-"), label
}

type httpTraceKey struct {
	method string
	host   string
	path   string
}

type httpTraceRow struct {
	Method    string            `json:"method,omitempty"`
	Host      string            `json:"host,omitempty"`
	Path      string            `json:"path,omitempty"`
	Processes []string          `json:"processes,omitempty"`
	Statuses  map[string]uint64 `json:"statuses,omitempty"`
	traceHTTPCounts
}

type httpTraceReport struct {
	Side       string         `json:"side,omitempty"`
	DurationMS int64          `json:"duration_ms"`
	LostEvents uint64         `json:"lost_events"`
	Paths      []httpTraceRow `json:"paths"`
	traceHTTPCounts
}

type httpTraceRowStats struct {
	processes map[string]struct{}
	statuses  map[string]uint64
	counts    traceHTTPCounts
}

// httpTraceSummarizer는 --group-by 없이 끝난 HTTP trace를 method, host, path마다 한 행으로 묶는다.
type httpTraceSummarizer struct {
	rows   map[httpTraceKey]*httpTraceRowStats
	counts traceHTTPCounts
	side   string
}

func newHTTPTraceSummarizer() *httpTraceSummarizer {
	return &httpTraceSummarizer{rows: map[httpTraceKey]*httpTraceRowStats{}}
}

func (summarizer *httpTraceSummarizer) observe(event captureEvent) {
	if event.Side != "" {
		summarizer.side = event.Side
	}
	key := httpTraceKey{method: event.Method, host: event.Target, path: event.Path}
	stats := summarizer.rows[key]
	if stats == nil {
		stats = &httpTraceRowStats{processes: map[string]struct{}{}, statuses: map[string]uint64{}}
		summarizer.rows[key] = stats
	}
	if event.Process != "" {
		stats.processes[event.Process] = struct{}{}
	}
	if event.Status != 0 {
		stats.statuses[strconv.Itoa(event.Status)]++
	}
	stats.counts.observe(event)
	summarizer.counts.observe(event)
}

func (summarizer *httpTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := httpTraceReport{Side: summarizer.side, DurationMS: duration.Milliseconds(), LostEvents: summary.LostEvents, traceHTTPCounts: summarizer.counts.finished()}
	report.Paths = make([]httpTraceRow, 0, len(summarizer.rows))
	for key, stats := range summarizer.rows {
		report.Paths = append(report.Paths, httpTraceRow{Method: key.method, Host: key.host, Path: key.path, Processes: slices.Sorted(maps.Keys(stats.processes)), Statuses: maps.Clone(stats.statuses), traceHTTPCounts: stats.counts.finished()})
	}
	sort.Slice(report.Paths, func(i, j int) bool {
		left, right := report.Paths[i], report.Paths[j]
		if left.Requests != right.Requests {
			return left.Requests > right.Requests
		}
		if left.Host != right.Host {
			return left.Host < right.Host
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Method < right.Method
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. HTTP 요약은 요청 종류마다 한 행이라 같은 표를 쓴다.
func (report httpTraceReport) print(bool) {
	title := "HTTP trace"
	if report.Side == traceServerSide {
		title = "HTTP server trace"
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n\n", title, (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Requests: %d\nResponses: %d\nClient errors (4xx): %d\nServer errors (5xx): %d\nUnanswered: %d\nLatency avg: %s\nLatency max: %s\nLost events: %d\n", report.Requests, report.Responses, report.ClientErrors, report.ServerErrors, report.Unanswered, traceLatency(report.LatencyAvgMS, "ms"), traceLatency(report.LatencyMaxMS, "ms"), report.LostEvents)
	if len(report.Paths) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nMETHOD\tHOST\tPATH\tREQUESTS\tSTATUS\tUNANSWERED\tAVG\tMAX\tPROCESS")
	for _, row := range report.Paths {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s\n", emptyAs(row.Method, "-"), emptyAs(row.Host, "-"), emptyAs(row.Path, "-"), row.Requests, traceResultCounts(row.Statuses), row.Unanswered, traceLatency(row.LatencyAvgMS, "ms"), traceLatency(row.LatencyMaxMS, "ms"), emptyAs(strings.Join(row.Processes, ","), "-"))
	}
}
