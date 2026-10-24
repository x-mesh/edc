package edc

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// traceScreenPayloadBytes는 상세 보기를 위해 payload 전체를 두는 합계 상한이다. 목록은 event마다 앞 4KiB만 두므로,
// 최근 event의 전체는 따로 둔다. 넘으면 오래된 event부터 앞 4KiB만 남는다.
const traceScreenPayloadBytes = 64 << 20

// traceGunzipLimit는 상세 보기가 푸는 본문의 상한이다. 작은 압축 data가 아주 크게 풀리는 경우(zip bomb)를 막는다.
// 제어 문자는 \xNN 네 글자로 보이므로, 화면에 쓰는 글자는 이보다 네 배까지 길다.
const traceGunzipLimit = httpMessageMax

// traceFullPayloads는 event 번호로 찾는 payload다. 목록에서 앞부분만 남긴 payload의 전체와 gzip message의 원본 byte를
// 둔다. minimum보다 긴 것만 둔다. 합계 상한 traceScreenPayloadBytes는 payload와 gzip 원본이 나눠 쓰므로 여기서 적용하지
// 않고, traceScreenModel.keepEventPayload가 두 값의 합계로 적용한다.
type traceFullPayloads struct {
	minimum  int
	byNumber map[int]string
	order    []int
	bytes    int
}

func (payloads *traceFullPayloads) keep(number int, payload string) {
	payloads.forget(number)
	if len(payload) <= payloads.minimum {
		return
	}
	if payloads.byNumber == nil {
		payloads.byNumber = map[int]string{}
	}
	payloads.byNumber[number] = payload
	payloads.order = append(payloads.order, number)
	payloads.bytes += len(payload)
}

func (payloads *traceFullPayloads) forget(number int) {
	value, ok := payloads.byNumber[number]
	if !ok {
		return
	}
	delete(payloads.byNumber, number)
	payloads.bytes -= len(value)
	if index := slices.Index(payloads.order, number); index >= 0 {
		payloads.order = append(payloads.order[:index], payloads.order[index+1:]...)
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
	title            string
	number           int
	raw              []string
	width            int
	lines            []string
	offset           int
	payloadRawStart  int
	payloadStart     int
	preserveOffset   bool
	restoreOffset    int
	revision         uint64
	responseRevision uint64
	responseNumber   int
	updating         bool
}

type traceDetailSnapshot struct {
	kind             string
	generation       uint64
	revision         uint64
	responseRevision uint64
	responseNumber   int
	number           int
	width            int
	protocol         string
	filter           string
	secrets          bool
	decode           bool
	event            captureEvent
	response         *captureEvent
	payload          string
	gzipRaw          string
	notes            []string
	payloadEvicted   bool
}

func cloneTraceEvent(event captureEvent) captureEvent {
	clone := event
	clone.Answers = slices.Clone(event.Answers)
	clone.ALPN = slices.Clone(event.ALPN)
	clone.gzipped = nil
	clone.LatencyMS = cloneFloat64(event.LatencyMS)
	clone.NetworkMS = cloneFloat64(event.NetworkMS)
	clone.ReadDelayMS = cloneFloat64(event.ReadDelayMS)
	if event.MySQL != nil {
		value := *event.MySQL
		value.AffectedRows = cloneUint64(value.AffectedRows)
		clone.MySQL = &value
	}
	if event.ClickHouse != nil {
		value := *event.ClickHouse
		value.Rows = cloneUint64(value.Rows)
		clone.ClickHouse = &value
	}
	if event.Postgres != nil {
		value := *event.Postgres
		value.Rows = cloneUint64(value.Rows)
		clone.Postgres = &value
	}
	if event.Redis != nil {
		value := *event.Redis
		value.ArgPreview = slices.Clone(value.ArgPreview)
		value.Elements = cloneInt64(value.Elements)
		value.BulkLength = cloneInt64(value.BulkLength)
		clone.Redis = &value
	}
	return clone
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// traceEventTitle은 상세 보기와 미리 보기의 제목이다. 시각을 앞에 두어 서버 log와 맞춰 볼 수 있게 한다.
func traceEventTitle(event captureEvent) string {
	destination, name := traceScrollLabels(event)
	return fmt.Sprintf("%s  %s  %s  %s", traceEventClock(event), emptyAs(event.Process, "-"), destination, name)
}

// newTraceDetail은 event의 필드와 payload를 줄로 만든다. payload는 부르는 쪽이 가리기와 gzip 풀기를 마친 글자다.
func newTraceDetail(event captureEvent, payload string, notes []string, number, width int) *traceDetail {
	detail := &traceDetail{title: traceEventTitle(event), number: number, payloadRawStart: -1, payloadStart: -1}
	if event.Protocol == "mysql" && event.MySQL != nil {
		detail.raw = traceMySQLDetailLines(event)
	} else if event.Protocol == "clickhouse" && event.ClickHouse != nil {
		detail.raw = traceClickHouseDetailLines(event)
	} else if event.Protocol == "postgres" && event.Postgres != nil {
		detail.raw = tracePostgresDetailLines(event)
	} else if event.Protocol == "redis" && event.Redis != nil {
		detail.raw = traceRedisDetailLines(event)
	} else {
		fields := event
		fields.Payload = ""
		encoded, err := json.MarshalIndent(fields, "", "  ")
		if err != nil {
			encoded = []byte(err.Error())
		}
		detail.raw = strings.Split(string(encoded), "\n")
	}
	if payload != "" {
		detail.payloadRawStart = len(detail.raw)
		detail.raw = append(append(detail.raw, "", "payload:"), notes...)
		event.Payload = payload
		detail.raw = append(detail.raw, traceHTTPPayloadBlock(event)...)
	}
	detail.wrap(width)
	return detail
}

func traceMySQLDetailLines(event captureEvent) []string {
	info := event.MySQL
	lines := []string{
		"event: " + event.Event,
		"time: " + traceEventClock(event),
		"side: " + emptyAs(event.Side, "-"),
		"process: " + emptyAs(event.Process, "-"),
		"source: " + emptyAs(event.Source, "-"),
		"destination: " + emptyAs(event.Destination, "-"),
		"bytes: " + strconv.FormatUint(event.Bytes, 10),
	}
	if info.Command != "" {
		lines = append(lines, "command: "+info.Command)
	}
	if event.LatencyMS != nil {
		lines = append(lines, "latency: "+traceLatency(event.LatencyMS, "ms"))
	}
	if info.User != "" {
		lines = append(lines, "user: "+info.User)
	}
	if info.Database != "" {
		lines = append(lines, "database: "+info.Database)
	}
	if info.ServerVersion != "" {
		lines = append(lines, "server_version: "+info.ServerVersion)
	}
	if info.StatementID != 0 {
		lines = append(lines, "statement_id: "+strconv.FormatUint(uint64(info.StatementID), 10))
	}
	if info.AffectedRows != nil {
		lines = append(lines, "affected_rows: "+strconv.FormatUint(*info.AffectedRows, 10))
	}
	if info.Columns != 0 {
		lines = append(lines, "columns: "+strconv.FormatUint(info.Columns, 10))
	}
	if info.ErrorCode != 0 || info.SQLState != "" || info.Message != "" {
		lines = append(lines, "error: "+fmt.Sprintf("%d (%s) %s", info.ErrorCode, emptyAs(info.SQLState, "-"), info.Message))
	}
	if info.SQL != "" {
		lines = append(lines, "", "sql:")
		lines = append(lines, strings.Split(info.SQL, "\n")...)
	}
	return lines
}

func traceClickHouseDetailLines(event captureEvent) []string {
	info := event.ClickHouse
	lines := []string{
		"event: " + event.Event,
		"time: " + traceEventClock(event),
		"side: " + emptyAs(event.Side, "-"),
		"process: " + emptyAs(event.Process, "-"),
		"source: " + emptyAs(event.Source, "-"),
		"destination: " + emptyAs(event.Destination, "-"),
		"bytes: " + strconv.FormatUint(event.Bytes, 10),
	}
	if info.Interface != "" {
		lines = append(lines, "interface: "+info.Interface)
	}
	if info.Command != "" {
		lines = append(lines, "command: "+info.Command)
	}
	if info.HTTPStatus != 0 {
		lines = append(lines, "http_status: "+strconv.Itoa(info.HTTPStatus))
	}
	if event.LatencyMS != nil {
		lines = append(lines, "latency: "+traceLatency(event.LatencyMS, "ms"))
	}
	if info.QueryID != "" {
		lines = append(lines, "query_id: "+info.QueryID)
	}
	if info.User != "" {
		lines = append(lines, "user: "+info.User)
	}
	if info.Database != "" {
		lines = append(lines, "database: "+info.Database)
	}
	if info.ClientName != "" {
		lines = append(lines, "client: "+strings.TrimSpace(info.ClientName+" "+info.ClientVersion))
	}
	if info.ServerName != "" {
		lines = append(lines, "server: "+strings.TrimSpace(info.ServerName+" "+info.ServerVersion))
	}
	if info.Rows != nil {
		lines = append(lines, "rows: "+strconv.FormatUint(*info.Rows, 10))
	}
	if info.EndInferred {
		lines = append(lines, "end_inferred: true")
	}
	if info.ErrorCode != 0 || info.ErrorName != "" || info.Message != "" {
		lines = append(lines, "error: "+fmt.Sprintf("%d (%s) %s", info.ErrorCode, emptyAs(info.ErrorName, "-"), info.Message))
	}
	if info.SQL != "" {
		lines = append(lines, "", "sql:")
		lines = append(lines, strings.Split(info.SQL, "\n")...)
		if info.SQLTruncated {
			lines = append(lines, "(truncated)")
		}
	}
	return lines
}

// buildDetail은 번호가 number인 event의 상세 보기를 만든다. 목록에서 빠진 event면 nil이다.
func (model traceScreenModel) buildDetail(number int) *traceDetail {
	if number < model.first || number >= model.first+len(model.events) {
		return nil
	}
	return traceBuildDetail(model.detailSnapshot(number, 0))
}

func (model traceScreenModel) detailSnapshot(number int, generation uint64) traceDetailSnapshot {
	event := cloneTraceEvent(model.events[number-model.first])
	payload, kept := model.payloads.get(number)
	var notes []string
	if !kept {
		payload = event.Payload
		if model.payloadEvicted[number] {
			notes = append(notes, "full payload evicted to keep screen memory within 64 MiB")
		} else if len(payload) >= httpPayloadHead {
			notes = append(notes, fmt.Sprintf("(only the first %d bytes: older payloads are not kept)", httpPayloadHead))
		}
	}
	gzipRaw, _ := model.gzipped.get(number)
	var response *captureEvent
	var responseRevision uint64
	responseNumber := -1
	if traceScreenPairs(model.protocol) {
		for _, row := range model.displayRows() {
			if row.primary == number && row.response >= model.first && row.response < model.first+len(model.events) {
				value := cloneTraceEvent(model.events[row.response-model.first])
				response = &value
				responseNumber = row.response
				responseRevision = model.payloadRevisions[row.response]
				break
			}
		}
	}
	return traceDetailSnapshot{generation: generation, revision: model.payloadRevisions[number], responseRevision: responseRevision, responseNumber: responseNumber, number: number, width: model.width, protocol: model.protocol, filter: model.filter, secrets: model.secrets, decode: model.decode, event: event, response: response, payload: payload, gzipRaw: gzipRaw, notes: slices.Clone(notes), payloadEvicted: model.payloadEvicted[number]}
}

func (model traceScreenModel) detailShell(number int, status string) *traceDetail {
	if number < model.first || number >= model.first+len(model.events) {
		return nil
	}
	event := model.events[number-model.first]
	return &traceDetail{title: traceEventTitle(event), number: number, width: model.width, raw: []string{status}, lines: []string{status}, payloadRawStart: -1, payloadStart: -1, revision: model.payloadRevisions[number], updating: status == "updating detail…"}
}

func (model *traceScreenModel) requestDetailBuild() tea.Cmd {
	if model.detail == nil || model.detail.number < model.first || model.detail.number >= model.first+len(model.events) || model.stopping || model.finished != nil {
		return nil
	}
	snapshot := model.detailSnapshot(model.detail.number, model.detailGeneration)
	snapshot.kind = "detail"
	if model.detailJobActive {
		if model.detailJobCurrent != nil && sameTraceDetailSnapshot(*model.detailJobCurrent, snapshot) {
			return nil
		}
		if model.detailJobPending != nil && sameTraceDetailSnapshot(*model.detailJobPending, snapshot) {
			return nil
		}
		model.detailJobPending = &snapshot
		return nil
	}
	return model.startDetailBuild(snapshot)
}

func (model *traceScreenModel) startDetailBuild(snapshot traceDetailSnapshot) tea.Cmd {
	if model.stopping || model.finished != nil {
		return nil
	}
	model.detailJobActive = true
	model.detailJobCurrent = &snapshot
	return func() tea.Msg {
		return buildTracePayload(snapshot)
	}
}

func sameTraceDetailSnapshot(left, right traceDetailSnapshot) bool {
	return left.kind == right.kind && left.generation == right.generation && left.revision == right.revision && left.responseRevision == right.responseRevision && left.responseNumber == right.responseNumber && left.number == right.number && left.width == right.width && left.protocol == right.protocol && left.filter == right.filter && left.secrets == right.secrets && left.decode == right.decode && left.payloadEvicted == right.payloadEvicted
}

func traceBuildDetail(snapshot traceDetailSnapshot) *traceDetail {
	result := tracePreparePayload(snapshot)
	detail := newTraceDetail(result.event, result.payload, result.notes, snapshot.number, snapshot.width)
	detail.revision = snapshot.revision
	detail.responseRevision = snapshot.responseRevision
	if snapshot.response != nil {
		detail.raw = append(detail.raw, tracePairResponseDetailLines(*snapshot.response)...)
		detail.wrap(snapshot.width)
	}
	return detail
}

type tracePreparedPayload struct {
	event   captureEvent
	payload string
	notes   []string
}

func tracePreparePayload(snapshot traceDetailSnapshot) tracePreparedPayload {
	event, payload, notes := snapshot.event, snapshot.payload, slices.Clone(snapshot.notes)
	if snapshot.decode && payload != "" {
		if snapshot.gzipRaw == "" {
			notes = append(notes, "(gzip: this message is not gzip, or its raw bytes are no longer kept)")
		} else if decoded, note := traceGunzipMessage([]byte(snapshot.gzipRaw)); decoded != "" {
			payload, notes = decoded, append(notes, note, "z raw")
		} else {
			notes = append(notes, note, "z raw")
		}
	} else if snapshot.gzipRaw != "" {
		notes = append(notes, "gzip compressed · z decompress")
	}
	if !snapshot.secrets && snapshot.protocol == "http" {
		payload = string(traceMaskHTTPHeaders([]byte(payload)))
	}
	return tracePreparedPayload{event: event, payload: payload, notes: notes}
}

func buildTracePayload(snapshot traceDetailSnapshot) traceDetailBuiltMsg {
	result := traceDetailBuiltMsg{generation: snapshot.generation, number: snapshot.number, revision: snapshot.revision, responseRevision: snapshot.responseRevision, responseNumber: snapshot.responseNumber, kind: snapshot.kind, protocol: snapshot.protocol, filter: snapshot.filter, width: snapshot.width, secrets: snapshot.secrets, decode: snapshot.decode, payloadEvicted: snapshot.payloadEvicted}
	if snapshot.kind == "preview" {
		prepared := tracePreparePayload(snapshot)
		preview := &tracePreview{number: snapshot.number, secrets: snapshot.secrets, decode: snapshot.decode, width: snapshot.width, revision: snapshot.revision, payloadEvicted: snapshot.payloadEvicted, title: tracePreviewTitle(prepared.event)}
		if prepared.payload != "" {
			prepared.event.Payload = prepared.payload
			preview.lines = append(preview.lines, prepared.notes...)
			preview.lines = append(preview.lines, traceHTTPPayloadBlock(prepared.event)...)
		} else {
			prepared.event.Payload = ""
			encoded, err := json.MarshalIndent(prepared.event, "", "  ")
			if err != nil {
				encoded = []byte(err.Error())
			}
			preview.lines = strings.Split(string(encoded), "\n")
		}
		preview.wrapped = traceWrapAll(preview.lines, snapshot.width)
		preview.wrappedRows = len(preview.wrapped)
		result.preview = preview
		return result
	}
	result.detail = traceBuildDetail(snapshot)
	return result
}

func tracePairResponseDetailLines(response captureEvent) []string {
	result, ok := tracePairResult(response)
	if !ok {
		return nil
	}
	lines := []string{"", "response:", "event: " + response.Event, "result: " + result}
	if response.LatencyMS != nil {
		lines = append(lines, "latency: "+traceLatency(response.LatencyMS, "ms"))
	}
	if response.MySQL != nil && (response.MySQL.ErrorCode != 0 || response.MySQL.SQLState != "" || response.MySQL.Message != "") {
		lines = append(lines, "error: "+fmt.Sprintf("%d (%s) %s", response.MySQL.ErrorCode, emptyAs(response.MySQL.SQLState, "-"), response.MySQL.Message))
	}
	if info := response.ClickHouse; info != nil && info.HTTPStatus != 0 {
		lines = append(lines, "http_status: "+strconv.Itoa(info.HTTPStatus))
	}
	if info := response.ClickHouse; info != nil && (info.ErrorCode != 0 || info.ErrorName != "" || info.Message != "") {
		lines = append(lines, "error: "+fmt.Sprintf("%d (%s) %s", info.ErrorCode, emptyAs(info.ErrorName, "-"), info.Message))
	}
	if info := response.Postgres; info != nil && info.Rows != nil {
		lines = append(lines, "rows: "+strconv.FormatUint(*info.Rows, 10))
	}
	if info := response.Postgres; info != nil && info.Detail != "" {
		lines = append(lines, "detail: "+info.Detail)
	}
	return lines
}

// detailPayload는 상세 보기와 미리 보기가 보일 payload다. 보관한 전체 payload를 쓰고, z면 gzip 본문을 풀고, m이 꺼져
// 있으면 인증 header 값을 가린다. notes는 payload 앞에 붙일 안내다.
func (model traceScreenModel) detailPayload(number int) (captureEvent, string, []string) {
	event := model.events[number-model.first]
	payload, kept := model.payloads.get(number)
	var notes []string
	if !kept {
		payload = event.Payload
		if model.payloadEvicted[number] {
			notes = append(notes, "full payload evicted to keep screen memory within 64 MiB")
		} else if len(payload) >= httpPayloadHead {
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
		if ok {
			notes = append(notes, "z raw")
		}
	} else if _, ok := model.gzipped.get(number); ok {
		notes = append(notes, "gzip compressed · z decompress")
	}
	if !model.secrets && model.protocol == "http" {
		payload = string(traceMaskHTTPHeaders([]byte(payload)))
	}
	return event, payload, notes
}

func traceScreenDetailPage(model traceScreenModel) int {
	page := model.height - 3
	if len(model.tlsNotices) != 0 || model.tlsNoticeOverflow != 0 {
		page--
	}
	return max(1, page)
}

// rebuildDetail은 가리기나 gzip을 바꾼 뒤 같은 event를 다시 그린다. 보던 위치는 그대로 둔다.
func (model *traceScreenModel) rebuildDetail() {
	if detail := model.buildDetail(model.detail.number); detail != nil {
		detail.offset = model.detail.offset
		model.detail = detail
		model.detail.scroll(0, traceScreenDetailPage(*model))
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
	detail.payloadStart = -1
	for index, line := range detail.raw {
		if index == detail.payloadRawStart {
			detail.payloadStart = len(detail.lines)
		}
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
	parts, _ := traceWrapLineUpTo(line, width, math.MaxInt)
	return parts
}

// moveSelection은 고른 event에서 filter에 맞는 event를 step 방향으로 count개 건너간 번호다. 고른 event가 없으면
// 가장 최근 event부터 센다. 더 갈 곳이 없으면 그대로 둔다.
func (model traceScreenModel) moveSelection(step, count int) int {
	if traceScreenPairs(model.protocol) {
		rows := model.displayRows()
		index := len(rows)
		if model.selected >= 0 {
			index = model.displayRowForSelected(rows)
			if index < 0 {
				index = len(rows)
			}
		}
		found := model.selected
		for index += step; index >= 0 && index < len(rows) && count > 0; index += step {
			if model.displayRowMatches(rows[index]) {
				found, count = rows[index].primary, count-1
			}
		}
		return found
	}
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
func (model *traceScreenModel) openDetail() tea.Cmd {
	if model.selected < 0 {
		model.selected = model.moveSelection(-1, 1)
	}
	if model.selected < 0 {
		return nil
	}
	model.detailGeneration++
	model.detail = model.detailShell(model.selected, "preparing detail…")
	return model.requestDetailBuild()
}

// openFollow는 가장 최근 event를 보는 상세 보기를 열고, 새 event가 오면 그 event로 바꾼다.
func (model *traceScreenModel) openFollow() tea.Cmd {
	model.follow, model.selected = true, -1
	model.detailGeneration++
	model.detailJobPending = nil
	return model.followNewest()
}

func (model traceScreenModel) updateDetailKey(key string) (traceScreenModel, tea.Cmd) {
	page := traceScreenDetailPage(model)
	var cmd tea.Cmd
	switch key {
	case "esc", "enter", "q", "backspace":
		model.detail, model.follow = nil, false
		model.detailGeneration++
		model.detailJobPending = nil
	case "f":
		// 따라가기를 끄면 보던 event에서 멈추고, 목록으로 돌아가도 그 event를 고른 채로 둔다.
		if model.follow {
			model.follow, model.selected = false, model.detail.number
			model.detailGeneration++
			model.detailJobPending = nil
		} else {
			cmd = model.openFollow()
		}
	case "m":
		model.secrets = !model.secrets
		model.detailGeneration++
		offset := model.detail.offset
		preserveOffset := len(model.detail.raw) > 1 && !model.detail.updating
		model.detail = model.detailShell(model.detail.number, "preparing detail…")
		model.detail.preserveOffset = preserveOffset
		model.detail.restoreOffset = offset
		cmd = model.requestDetailBuild()
	case "z":
		model.decode = !model.decode
		model.detailGeneration++
		offset := model.detail.offset
		preserveOffset := len(model.detail.raw) > 1 && !model.detail.updating
		model.detail = model.detailShell(model.detail.number, "preparing detail…")
		model.detail.preserveOffset = preserveOffset
		model.detail.restoreOffset = offset
		cmd = model.requestDetailBuild()
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
	return model, cmd
}

func (model *traceScreenModel) followNewest() tea.Cmd {
	newest := model.moveSelection(-1, 1)
	if newest < 0 || (model.detail != nil && model.detail.number == newest) {
		return nil
	}
	if model.detailJobActive && model.detail != nil {
		model.detail.updating = true
		snapshot := model.detailSnapshot(newest, model.detailGeneration)
		snapshot.kind = "detail"
		model.detailJobPending = &snapshot
		return nil
	}
	model.detail = model.detailShell(newest, "preparing detail…")
	return model.requestDetailBuild()
}

func traceScreenDetailView(model traceScreenModel) []string {
	detail := model.detail
	page := max(0, traceScreenDetailPage(model))
	last := min(len(detail.lines), detail.offset+page)
	color := traceScreenColor()
	captureState, captureColor := "live", richGreen
	if model.stopping {
		captureState, captureColor = "stopping", richYellow
	}
	header := append(traceTitleSpans("edc trace "+traceLabel(model.protocol, model.side)), traceSeparator, traceText{text: "capture " + captureState, color: captureColor}, traceSeparator, traceText{text: "detail"}, traceSeparator, traceLabelSpan("target "), traceText{text: fmt.Sprintf("#%d", detail.number+1)}, traceSeparator, traceLabelSpan("lines "),
		traceText{text: fmt.Sprintf("%d-%d", min(detail.offset+1, last), last)}, traceLabelSpan(" of "), traceText{text: fmt.Sprint(len(detail.lines))})
	for _, state := range []struct {
		on    bool
		name  string
		color string
	}{{model.follow, "follow", richGreen}, {model.decode, "gzip decoded", ""}, {model.secrets && model.protocol == "http", "secrets shown", richYellow}} {
		if state.on {
			header = append(header, traceSeparator, traceText{text: state.name, color: state.color})
		}
	}
	if detail.updating {
		header = append(header, traceSeparator, traceText{text: "updating", color: richYellow})
	}
	// Mac 자판에는 PgUp, PgDn, Home, End가 없는 경우가 많아, 어디서나 쓸 수 있는 키를 안내한다. 원래 키도 그대로 된다.
	help := "↑↓ scroll  space/b page  g/G top/end  f follow  esc back  ctrl-c stop"
	if model.protocol == "http" {
		help = "↑↓ scroll  space/b page  g/G top/end  f follow  m secrets  z gzip  esc back  ctrl-c stop"
	}
	var on []string
	for _, state := range []struct {
		on   bool
		hint string
	}{{model.follow, "f follow"}, {model.secrets, "m secrets"}, {model.decode, "z gzip"}} {
		if state.on {
			on = append(on, state.hint)
		}
	}
	rows := []string{traceTexts(header, model.width, color), traceKeyHints(help, model.width, color, on...), traceFit(detail.title, model.width)}
	if len(model.tlsNotices) != 0 || model.tlsNoticeOverflow != 0 {
		notice := strings.Join(model.tlsNotices, " · ")
		if model.tlsNoticeOverflow != 0 {
			notice += fmt.Sprintf(" · %d TLS notices omitted", model.tlsNoticeOverflow)
		}
		rows = append(rows, traceTexts([]traceText{{text: "TLS: " + notice, color: richYellow}}, model.width, color))
	}
	rows = append(rows, detail.lines[detail.offset:last]...)
	return traceScreenPadRows(rows, model.height)
}
