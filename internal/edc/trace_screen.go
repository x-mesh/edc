package edc

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const traceScreenEventLimit = 10000

// traceScreenGCPercent는 전체 화면 trace의 GC 빈도를 낮춘다. 요약만 쌓으면 heap이 작아 GC가 자주 돌고, 할당이 많은
// group 화면 갱신이 느려져 초당 수만 event에서 ring buffer 유실이 늘었다. 800이면 유실이 모든 event를 두던
// 때와 같고, 메모리는 일정하다.
const traceScreenGCPercent = 800

type traceEventMsg struct{ events []captureEvent }
type traceDetailBuiltMsg struct {
	generation       uint64
	number           int
	revision         uint64
	responseRevision uint64
	responseNumber   int
	kind             string
	protocol         string
	filter           string
	width            int
	secrets          bool
	decode           bool
	payloadEvicted   bool
	detail           *traceDetail
	preview          *tracePreview
}
type traceFinishedMsg struct {
	summary captureSummary
	err     error
}
type traceStopMsg struct{}
type traceStopTickMsg struct {
	sequence uint64
	at       time.Time
}

type traceScreenModel struct {
	protocol    string
	side        string
	groupBy     string
	process     string
	destination string
	duration    time.Duration
	slow        time.Duration
	started     time.Time
	events      []captureEvent
	arrivals    []time.Time
	truncated   bool
	filter      string
	input       textinput.Model
	filtering   bool
	stopping    bool
	stopStarted time.Time
	stopElapsed time.Duration
	stopFrame   int
	stopTick    uint64
	received    uint64
	width       int
	height      int
	stop        func()
	finished    *traceFinishedMsg
	eventCh     <-chan captureEvent
	resultCh    <-chan traceFinishedMsg
	// first는 events[0]의 번호다. 앞의 event를 버려도 번호는 그대로라서, 고른 event와 payload 전체를 번호로 찾는다.
	first          int
	messageNumbers map[uint64]int
	payloadEvicted map[int]bool
	// selected는 고른 event의 번호다. -1이면 고른 event가 없고 목록이 새 event를 따라간다.
	selected         int
	detail           *traceDetail
	detailGeneration uint64
	detailJobActive  bool
	detailJobCurrent *traceDetailSnapshot
	detailJobPending *traceDetailSnapshot
	payloadRevisions map[int]uint64
	payloads         traceFullPayloads
	// gzipped는 gzip message의 원본 byte다. 상세 보기에서 z로 본문을 풀 때 쓴다.
	gzipped traceFullPayloads
	// payloadLines는 목록에서 event 아래에 payload 줄을 보일지다. v로 바꾼다. --payload로 시작하면 켜져 있다.
	payloadLines bool
	// secrets는 인증 header 값을 보일지다. m으로 바꾼다. 화면은 원문을 두었다가 그릴 때 가린다.
	secrets bool
	// follow면 상세 보기가 가장 최근 event를 따라간다. decode면 상세 보기가 gzip 본문을 푼다.
	follow bool
	decode bool
	// split이면 목록 아래에 고른 event나 가장 최근 event의 message를 보인다. i로 바꾼다. previewOffset은 J/K로 옮긴
	// 미리 보기의 위치다.
	split         bool
	preview       *tracePreview
	previewOffset int
	// container는 --container로 고른 container의 이름이다. 목록이 조용해도 거르는 중임을 머리글에 보인다.
	container         string
	tlsNotices        []string
	tlsNoticeOverflow uint64
	mysqlRows         []traceScreenRow
	mysqlRowsHead     int
	mysqlRowIndex     map[int]int
	mysqlPending      map[traceMySQLPairKey][]int
	mysqlPendingKey   map[int]traceMySQLPairKey
}

type traceScreenRow struct {
	primary  int
	response int
	hidden   bool
}

// traceMySQLPairKey는 요청과 응답을 짝짓는 key다. 이름은 mysql에서 왔지만 clickhouse도 같은 key를 쓴다.
type traceMySQLPairKey struct {
	socket  uint64
	side    string
	command string
	sql     string
}

// traceScreenPairs는 요청과 응답을 한 행으로 짝지어 보이는 protocol인지다.
func traceScreenPairs(protocol string) bool {
	return traceDatabaseProtocol(protocol)
}

// tracePairOf는 event의 짝 key와 응답 여부를 꺼낸다. pending이 아닌 event(mysql_tls, clickhouse_chunked)는 답이 오지
// 않으므로 짝을 기다리지 않는다. ok가 false면 짝지을 값이 없는 event다.
func tracePairOf(event captureEvent) (key traceMySQLPairKey, response, pending, ok bool) {
	switch {
	case event.MySQL != nil:
		key = traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.MySQL.Command, sql: event.MySQL.SQL}
		return key, mysqlResponseEvent(event.Event), event.Event != mysqlEventTLS, true
	case event.ClickHouse != nil:
		key = traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.ClickHouse.Command, sql: event.ClickHouse.SQL}
		return key, clickHouseResponseEvent(event.Event), event.Event != clickHouseEventChunked, true
	case event.Postgres != nil:
		key = traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.Postgres.Command, sql: event.Postgres.SQL}
		return key, postgresResponseEvent(event.Event), event.Event != postgresEventTLS, true
	case event.Redis != nil:
		// redis는 SQL 자리에 key를 둔다. push, 끝까지 읽지 못한 명령, 서버가 답하지 않는 명령은 tracker가 짝짓지 않으므로 기다리지
		// 않는다.
		key = traceMySQLPairKey{socket: event.SocketID, side: event.Side, command: event.Redis.Command, sql: event.Redis.Key}
		return key, redisResponseEvent(event.Event), event.Event == redisEventCommand && !event.Redis.Truncated && !event.Redis.NoReply, true
	}
	return traceMySQLPairKey{}, false, false, false
}

// tracePairResult는 짝지은 응답 event의 결과 글자다. 응답 event가 아니면 ok가 false다.
func tracePairResult(response captureEvent) (string, bool) {
	switch {
	case response.MySQL != nil:
		return traceMySQLResultText(response), true
	case response.ClickHouse != nil:
		return traceClickHouseResultText(response), true
	case response.Postgres != nil:
		return tracePostgresResultText(response), true
	case response.Redis != nil:
		return traceRedisResultText(response), true
	}
	return "", false
}

func (model traceScreenModel) displayRows() []traceScreenRow {
	if !traceScreenPairs(model.protocol) {
		rows := make([]traceScreenRow, 0, len(model.events))
		for index := range model.events {
			rows = append(rows, traceScreenRow{primary: model.first + index, response: -1})
		}
		return rows
	}
	if model.mysqlRows == nil && len(model.events) != 0 {
		return traceMySQLReferenceRows(model.events, model.first, model.slow)
	}
	return model.mysqlRows[model.mysqlRowsHead:]
}

// traceMySQLReferenceRows is the test reference for the incremental screen index.
func traceMySQLReferenceRows(events []captureEvent, first int, slow time.Duration) []traceScreenRow {
	rows := make([]traceScreenRow, 0, len(events))
	pending := map[traceMySQLPairKey][]int{}
	rowIndexByPrimary := make(map[int]int, len(events))
	for index, event := range events {
		number := first + index
		key, response, pendingEvent, ok := tracePairOf(event)
		if !ok {
			rows = append(rows, traceScreenRow{primary: number, response: -1})
			rowIndexByPrimary[number] = len(rows) - 1
			continue
		}
		if response {
			queue := pending[key]
			if len(queue) > 0 {
				primary := queue[0]
				pending[key] = queue[1:]
				if rowIndex, ok := rowIndexByPrimary[primary]; ok {
					rows[rowIndex].response = number
				}
				continue
			}
			rows = append(rows, traceScreenRow{primary: number, response: -1})
			rowIndexByPrimary[number] = len(rows) - 1
			continue
		}
		rows = append(rows, traceScreenRow{primary: number, response: -1})
		rowIndexByPrimary[number] = len(rows) - 1
		if pendingEvent {
			pending[key] = append(pending[key], number)
		}
	}
	if slow == 0 {
		return rows
	}
	threshold := float64(slow) / float64(time.Millisecond)
	filtered := rows[:0]
	for _, row := range rows {
		if row.response < first || row.response >= first+len(events) {
			continue
		}
		response := events[row.response-first]
		if response.LatencyMS != nil && *response.LatencyMS >= threshold {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func (model traceScreenModel) displayRowIsSlow(row traceScreenRow) bool {
	if row.response < model.first || row.response >= model.first+len(model.events) {
		return false
	}
	response := model.events[row.response-model.first]
	return response.LatencyMS != nil && *response.LatencyMS >= float64(model.slow)/float64(time.Millisecond)
}

func (model *traceScreenModel) appendMySQLRow(event captureEvent, number int) {
	if model.mysqlRowIndex == nil {
		model.mysqlRowIndex = map[int]int{}
		model.mysqlPending = map[traceMySQLPairKey][]int{}
		model.mysqlPendingKey = map[int]traceMySQLPairKey{}
	}
	appendRow := func(row traceScreenRow) {
		model.mysqlRowIndex[row.primary] = len(model.mysqlRows)
		model.mysqlRows = append(model.mysqlRows, row)
	}
	key, response, pendingEvent, ok := tracePairOf(event)
	if !ok {
		appendRow(traceScreenRow{primary: number, response: -1})
		return
	}
	if response {
		appendRow(traceScreenRow{primary: number, response: -1, hidden: true})
		if pending := model.mysqlPending[key]; len(pending) > 0 {
			primary := pending[0]
			if len(pending) == 1 {
				delete(model.mysqlPending, key)
			} else {
				model.mysqlPending[key] = pending[1:]
			}
			delete(model.mysqlPendingKey, primary)
			if index, ok := model.mysqlRowIndex[primary]; ok {
				model.mysqlRows[index].response = number
			}
			return
		}
		model.mysqlRows[len(model.mysqlRows)-1].hidden = false
		return
	}
	appendRow(traceScreenRow{primary: number, response: -1})
	if pendingEvent {
		model.mysqlPending[key] = append(model.mysqlPending[key], number)
		model.mysqlPendingKey[number] = key
	}
}

func (model *traceScreenModel) evictMySQLRow(number int) {
	index, ok := model.mysqlRowIndex[number]
	if !ok {
		return
	}
	row := model.mysqlRows[index]
	if key, pending := model.mysqlPendingKey[number]; pending {
		queue := model.mysqlPending[key]
		if len(queue) > 0 && queue[0] == number {
			if len(queue) == 1 {
				delete(model.mysqlPending, key)
			} else {
				model.mysqlPending[key] = queue[1:]
			}
		}
		delete(model.mysqlPendingKey, number)
	}
	delete(model.mysqlRowIndex, number)
	if row.response >= 0 {
		if responseIndex, retained := model.mysqlRowIndex[row.response]; retained {
			model.mysqlRows[responseIndex].hidden = false
		}
	}
	if index != model.mysqlRowsHead {
		model.mysqlRows[index].hidden = true
		return
	}
	model.mysqlRowsHead++
	if model.mysqlRowsHead*2 >= len(model.mysqlRows) {
		copy(model.mysqlRows, model.mysqlRows[model.mysqlRowsHead:])
		model.mysqlRows = model.mysqlRows[:len(model.mysqlRows)-model.mysqlRowsHead]
		model.mysqlRowsHead = 0
		for index, current := range model.mysqlRows {
			model.mysqlRowIndex[current.primary] = index
		}
	}
}

func (model traceScreenModel) displayRowLabels(row traceScreenRow) (captureEvent, string, string, bool) {
	event, ok := model.displayRow(row)
	if !ok {
		return captureEvent{}, "", "", false
	}
	destination, label := traceScrollLabels(event)
	if row.response >= model.first && row.response < model.first+len(model.events) && traceScreenPairs(event.Protocol) {
		response := model.events[row.response-model.first]
		if result, ok := tracePairResult(response); ok {
			destination += " → " + result
			label = response.Event
			if response.LatencyMS != nil {
				label += " " + traceLatency(response.LatencyMS, "ms")
			}
		}
	}
	return event, destination, label, true
}

func (model traceScreenModel) displayRow(row traceScreenRow) (captureEvent, bool) {
	if row.primary < model.first || row.primary >= model.first+len(model.events) {
		return captureEvent{}, false
	}
	return model.events[row.primary-model.first], true
}

func (model traceScreenModel) displayRowMatches(row traceScreenRow) bool {
	if row.hidden || (traceScreenPairs(model.protocol) && model.slow > 0 && !model.displayRowIsSlow(row)) {
		return false
	}
	event, ok := model.displayRow(row)
	if !ok {
		return false
	}
	if traceEventMatchesText(event, model.filter) {
		return true
	}
	if row.response < 0 || row.response < model.first || row.response >= model.first+len(model.events) {
		return false
	}
	return traceEventMatchesText(model.events[row.response-model.first], model.filter)
}

func (model traceScreenModel) displayRowForSelected(rows []traceScreenRow) int {
	for index, row := range rows {
		if row.primary == model.selected || row.response == model.selected {
			return index
		}
	}
	return -1
}

func newTraceScreenModel(protocol string, options tcpTraceOptions, eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg, stop func()) traceScreenModel {
	input := textinput.New()
	input.SetStyles(traceInputStyles())
	input.Prompt = "filter / "
	input.Placeholder = "process, destination, event"
	input.CharLimit = 256
	input.SetWidth(48)
	model := traceScreenModel{
		protocol: protocol, side: options.side, groupBy: options.groupBy, process: options.process, destination: options.destination,
		duration: options.duration, slow: options.slow, started: time.Now(), eventCh: eventCh, resultCh: resultCh, stop: stop, input: input,
		width: 80, height: 24, selected: -1, payloads: traceFullPayloads{minimum: httpPayloadHead}, messageNumbers: map[uint64]int{}, payloadEvicted: map[int]bool{}, payloadRevisions: map[int]uint64{},
		payloadLines: options.payloadExplicit && options.payload != "", secrets: options.showSecrets, container: traceContainerName(options.container),
	}
	if options.tlsFinder != nil {
		for _, notice := range options.tlsFinder.notices {
			model.addTLSNotice(notice)
		}
	}
	return model
}

func (model traceScreenModel) Init() tea.Cmd {
	return waitTraceMessage(model.eventCh, model.resultCh)
}

func (model *traceScreenModel) keepEventPayload(number int, event captureEvent) {
	if model.payloadEvicted == nil {
		model.payloadEvicted = map[int]bool{}
	}
	model.payloads.keep(number, event.Payload)
	model.gzipped.keep(number, string(event.gzipped))
	if len(event.Payload) <= httpPayloadHead {
		delete(model.payloadEvicted, number)
	} else if _, ok := model.payloads.get(number); ok {
		delete(model.payloadEvicted, number)
	}
	for model.payloads.bytes+model.gzipped.bytes > traceScreenPayloadBytes {
		payloadNumber, gzipNumber := int(^uint(0)>>1), int(^uint(0)>>1)
		if len(model.payloads.order) > 0 {
			payloadNumber = model.payloads.order[0]
		}
		if len(model.gzipped.order) > 0 {
			gzipNumber = model.gzipped.order[0]
		}
		number := min(payloadNumber, gzipNumber)
		model.payloads.forget(number)
		model.gzipped.forget(number)
		model.payloadEvicted[number] = true
	}
}

func (model *traceScreenModel) addTLSNotice(notice string) {
	notice = traceTLSNoticeText(notice)
	if notice == "" || slices.Contains(model.tlsNotices, notice) {
		return
	}
	if len(model.tlsNotices) >= 64 {
		model.tlsNoticeOverflow++
		return
	}
	model.tlsNotices = append(model.tlsNotices, notice)
}

// traceEventBatchLimit은 한 메시지에 담는 event 수의 상한이다. 한 번의 Update가 너무 길어져 키 입력이 늦어지지 않게 한다.
const traceEventBatchLimit = 4096

// waitTraceMessage는 채널에 쌓인 event를 한 메시지로 묶는다. event마다 메시지를 하나씩 보내면 화면이 초당 약
// 110건만 소화해, 그보다 많은 트래픽에서는 화면이 계속 뒤처지고 수집도 채널에서 막힌다.
func waitTraceMessage(eventCh <-chan captureEvent, resultCh <-chan traceFinishedMsg) tea.Cmd {
	return func() tea.Msg {
		var first captureEvent
		select {
		case first = <-eventCh:
		case result := <-resultCh:
			return result
		}
		events := []captureEvent{first}
		for len(events) < traceEventBatchLimit {
			select {
			case event := <-eventCh:
				events = append(events, event)
			default:
				return traceEventMsg{events: events}
			}
		}
		return traceEventMsg{events: events}
	}
}

// Update는 message를 처리한 뒤 미리 보기를 맞춘다. 고른 event는 키와 새 event 양쪽에서 바뀌므로 한곳에서 맞춘다.
func (model traceScreenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := model.update(msg)
	updated := next.(traceScreenModel)
	previewCmd := updated.requestPreviewBuild()
	return updated, tea.Batch(cmd, previewCmd)
}

func (model traceScreenModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case tea.WindowSizeMsg:
		resized := model.width != value.Width || model.height != value.Height
		model.width, model.height = value.Width, value.Height
		model.input.SetWidth(max(20, min(60, value.Width-12)))
		if model.stopping {
			return model, nil
		}
		if resized {
			model.detailGeneration++
		}
		if model.detail != nil {
			offset := model.detail.offset
			preserveOffset := len(model.detail.raw) > 1 && !model.detail.updating
			model.detail = model.detailShell(model.detail.number, "updating detail…")
			model.detail.preserveOffset = preserveOffset
			model.detail.restoreOffset = offset
			return model, model.requestDetailBuild()
		}
		return model, nil
	case traceEventMsg:
		now := time.Now()
		updatedDetail := false
		updatedPreview := false
		for _, event := range value.events {
			if event.TLSNotice != "" {
				model.addTLSNotice(event.TLSNotice)
				continue
			}
			if event.Update {
				if traceProtocol(event) != model.protocol {
					continue
				}
				number, ok := model.messageNumbers[event.MessageID]
				if !ok || number < model.first || number >= model.first+len(model.events) {
					continue
				}
				index := number - model.first
				model.keepEventPayload(number, event)
				model.payloadRevisions[number]++
				event.Payload, event.gzipped = strings.Clone(traceTrimText(event.Payload, httpPayloadHead)), nil
				model.events[index] = event
				if model.detail != nil && model.detail.number == number {
					updatedDetail = true
				} else if model.detail != nil && traceScreenPairs(model.protocol) {
					for _, row := range model.displayRows() {
						if row.primary == model.detail.number && row.response == number {
							updatedDetail = true
							break
						}
					}
				}
				if model.preview != nil && model.preview.number == number {
					updatedPreview = true
				}
				continue
			}
			model.received++
			if traceProtocol(event) == model.protocol {
				number := model.first + len(model.events)
				if event.MessageID != 0 {
					model.messageNumbers[event.MessageID] = number
				}
				// 화면은 event를 최대 10,000건 보관한다. --payload=all의 1MiB payload를 그대로 두면 메모리가 GB 단위로
				// 커지므로 목록에는 앞부분만 남긴다. 상세 보기에 쓸 전체는 최근 것만 상한 안에서 따로 둔다.
				model.keepEventPayload(number, event)
				model.payloadRevisions[number] = 1
				event.Payload, event.gzipped = strings.Clone(traceTrimText(event.Payload, httpPayloadHead)), nil
				model.events = append(model.events, event)
				model.arrivals = append(model.arrivals, now)
				if traceScreenPairs(model.protocol) {
					model.appendMySQLRow(event, model.first+len(model.events)-1)
				}
			}
		}
		if len(model.events) > traceScreenEventLimit {
			drop := len(model.events) - traceScreenEventLimit
			for number := model.first; number < model.first+drop; number++ {
				delete(model.payloadEvicted, number)
				delete(model.payloadRevisions, number)
				if event := model.events[number-model.first]; event.MessageID != 0 {
					delete(model.messageNumbers, event.MessageID)
				}
			}
			if traceScreenPairs(model.protocol) {
				for number := model.first; number < model.first+drop; number++ {
					model.evictMySQLRow(number)
				}
			}
			model.first += drop
			model.events = model.events[drop:]
			model.arrivals = model.arrivals[drop:]
			model.truncated = true
			model.payloads.drop(model.first)
			model.gzipped.drop(model.first)
			model.normalizeSelection()
			if model.detail != nil && model.detail.number < model.first {
				model.detail = nil
				model.detailGeneration++
				model.detailJobPending = nil
			}
		}
		var detailCmd tea.Cmd
		if updatedDetail && model.detail != nil && !model.stopping {
			model.detail.updating = true
			detailCmd = model.requestDetailBuild()
		}
		if model.follow && !model.stopping {
			if followCmd := model.followNewest(); detailCmd == nil {
				detailCmd = followCmd
			}
		}
		if updatedPreview {
			model.preview = nil
		}
		return model, tea.Batch(waitTraceMessage(model.eventCh, model.resultCh), detailCmd)
	case traceDetailBuiltMsg:
		model.detailJobActive = false
		model.detailJobCurrent = nil
		validSnapshot := value.number >= model.first && value.number < model.first+len(model.events) && value.payloadEvicted == model.payloadEvicted[value.number] && value.protocol == model.protocol && value.filter == model.filter && value.width == model.width && value.secrets == model.secrets && value.decode == model.decode && !model.stopping && model.finished == nil
		if validSnapshot && value.generation == model.detailGeneration && value.kind == "preview" {
			targetMatches := model.preview != nil && (value.number == model.preview.number || (model.selected < 0 && value.number >= model.preview.number))
			revisionMatches := model.preview != nil && (value.number != model.preview.number || value.revision >= model.preview.revision)
			if model.detail == nil && model.split && model.groupBy == "" && targetMatches && revisionMatches {
				value.preview.updating = model.detailJobPending != nil || model.payloadRevisions[value.number] > value.revision
				model.preview = value.preview
				model.clampPreviewOffset()
			}
		} else if validSnapshot && value.generation == model.detailGeneration && value.kind == "detail" {
			targetMatches := model.detail != nil && (model.detail.number == value.number || (model.follow && value.number >= model.detail.number))
			revisionMatches := model.detail != nil && (value.number != model.detail.number || (value.revision >= model.detail.revision && value.responseRevision >= model.detail.responseRevision))
			if targetMatches && revisionMatches {
				value.detail.offset = model.detail.offset
				if model.detail.preserveOffset {
					value.detail.offset = model.detail.restoreOffset
				} else if (model.detail.number != value.number || len(model.detail.raw) == 1) && value.detail.payloadStart >= 0 {
					value.detail.offset = value.detail.payloadStart
				}
				value.detail.revision = value.revision
				value.detail.responseRevision = value.responseRevision
				value.detail.updating = model.detailJobPending != nil || model.payloadRevisions[value.number] > value.revision || (value.responseNumber >= model.first && value.responseNumber < model.first+len(model.events) && model.payloadRevisions[value.responseNumber] > value.responseRevision)
				model.detail = value.detail
				model.detail.scroll(0, traceScreenDetailPage(model))
			}
		}
		if model.detailJobPending != nil {
			pending := *model.detailJobPending
			model.detailJobPending = nil
			pendingVisible := (pending.kind == "detail" && model.detail != nil && (model.detail.number == pending.number || (model.follow && pending.number >= model.detail.number))) || (pending.kind == "preview" && model.detail == nil && model.split && model.groupBy == "")
			if pending.generation == model.detailGeneration && pending.protocol == model.protocol && pending.filter == model.filter && pending.width == model.width && pending.secrets == model.secrets && pending.decode == model.decode && pendingVisible && pending.number >= model.first && pending.number < model.first+len(model.events) && pending.payloadEvicted == model.payloadEvicted[pending.number] && !model.stopping && model.finished == nil {
				return model, model.startDetailBuild(pending)
			}
			if cmd := model.requestVisiblePayload(); cmd != nil {
				return model, cmd
			}
		} else if !validSnapshot && !model.stopping && model.finished == nil {
			if value.kind == "detail" && model.detail != nil && model.detail.number == value.number {
				return model, model.requestDetailBuild()
			}
			if value.kind == "preview" && model.detail == nil && model.split && model.groupBy == "" && model.previewTarget() == value.number {
				return model, model.requestPreviewBuild()
			}
		}
		return model, nil
	case traceFinishedMsg:
		model.finished = &value
		model.detailGeneration++
		model.detailJobPending = nil
		return model, tea.Quit
	case traceStopMsg:
		cmd := model.requestStop()
		return model, cmd
	case traceStopTickMsg:
		if !model.stopping || model.finished != nil || value.sequence != model.stopTick {
			return model, nil
		}
		model.stopElapsed = value.at.Sub(model.stopStarted)
		if model.stopElapsed < 0 {
			model.stopElapsed = 0
		}
		model.stopFrame = (model.stopFrame + 1) % len(liveSpinnerFrames)
		model.stopTick++
		return model, traceStopTick(model.stopTick)
	case tea.KeyPressMsg:
		return model.updateKey(value)
	}
	return model, nil
}

func (model *traceScreenModel) normalizeSelection() {
	if model.selected < 0 {
		return
	}
	if traceScreenPairs(model.protocol) {
		rows := model.displayRows()
		if index := model.displayRowForSelected(rows); index >= 0 && model.displayRowMatches(rows[index]) {
			model.selected = rows[index].primary
			return
		}
		for _, row := range rows {
			if model.displayRowMatches(row) {
				model.selected = row.primary
				return
			}
		}
		model.selected, model.preview, model.previewOffset = -1, nil, 0
		return
	}
	index := model.selected - model.first
	if index >= 0 && index < len(model.events) && traceEventMatchesText(model.events[index], model.filter) {
		return
	}
	for index, event := range model.events {
		if traceEventMatchesText(event, model.filter) {
			model.selected = model.first + index
			return
		}
	}
	model.selected, model.preview, model.previewOffset = -1, nil, 0
}

func (model traceScreenModel) updateKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		cmd := model.requestStop()
		return model, cmd
	}
	if model.stopping {
		return model, nil
	}
	if model.detail != nil {
		return model.updateDetailKey(key.String())
	}
	if model.filtering {
		switch key.String() {
		case "enter":
			model.filter = strings.TrimSpace(model.input.Value())
			model.input.Blur()
			model.filtering = false
			model.detailGeneration++
			model.preview = nil
			// 고른 event가 새 filter에 맞지 않으면 목록에 보이지 않으므로, 고른 것을 풀고 다시 따라간다.
			model.selected = -1
			return model, nil
		case "esc":
			model.input.SetValue(model.filter)
			model.input.Blur()
			model.filtering = false
			model.detailGeneration++
			model.preview = nil
			return model, nil
		}
		var cmd tea.Cmd
		model.input, cmd = model.input.Update(key)
		return model, cmd
	}
	switch key.String() {
	case "/":
		model.input.SetValue(model.filter)
		model.filtering = true
		return model, model.input.Focus()
	case "s", "t", "p", "c", "e", "u", "g":
		if view := traceGroupKeyViews[key.String()]; slices.Contains(traceGroupViews(model.protocol), view) {
			if model.groupBy != view {
				model.detailGeneration++
				model.preview = nil
			}
			model.groupBy = view
		}
		return model, nil
	case "tab":
		if next := nextTraceGroup(traceGroupViews(model.protocol), model.groupBy, 1); next != model.groupBy {
			model.groupBy = next
			model.detailGeneration++
			model.preview = nil
		}
		return model, nil
	case "shift+tab":
		if next := nextTraceGroup(traceGroupViews(model.protocol), model.groupBy, -1); next != model.groupBy {
			model.groupBy = next
			model.detailGeneration++
			model.preview = nil
		}
		return model, nil
	case "esc":
		// Mac 자판에는 End가 없는 경우가 많다. 고른 event가 있으면 Esc가 먼저 선택을 풀고 다시 따라간다.
		if model.selected >= 0 && model.groupBy == "" {
			model.selected = -1
			model.detailGeneration++
			model.preview = nil
			return model, nil
		}
		model.filter = ""
		model.detailGeneration++
		model.preview = nil
		return model, nil
	case "up", "k", "down", "j", "pgup", "pgdown", "b", "space":
		// 고르는 동안에는 목록이 새 event를 따라가지 않는다. end를 누르면 다시 따라간다.
		if model.groupBy == "" {
			step, count, page := 1, 1, max(1, model.listRows()-1)
			switch key.String() {
			case "up", "k":
				step = -1
			case "pgup", "b":
				step, count = -1, page
			case "pgdown", "space":
				count = page
			}
			model.selected = model.moveSelection(step, count)
			model.preview = nil
		}
		return model, nil
	case "end", "l":
		model.selected = -1
		model.detailGeneration++
		model.preview = nil
		return model, nil
	case "v":
		model.payloadLines = !model.payloadLines
		return model, nil
	case "m":
		model.secrets = !model.secrets
		model.detailGeneration++
		return model, nil
	case "f":
		if model.groupBy == "" {
			return model, model.openFollow()
		}
		return model, nil
	case "i":
		if model.groupBy == "" {
			model.split, model.preview, model.previewOffset = !model.split, nil, 0
			model.detailGeneration++
			if model.detailJobPending != nil && model.detailJobPending.kind == "preview" {
				model.detailJobPending = nil
			}
		}
		return model, nil
	case "J", "K":
		if model.split {
			model.scrollPreview(map[string]int{"J": 1, "K": -1}[key.String()])
		}
		return model, nil
	case "z":
		if model.split && model.protocol == "http" {
			model.decode = !model.decode
			model.detailGeneration++
		}
		return model, nil
	case "enter":
		if model.groupBy == "" {
			return model, model.openDetail()
		}
		return model, nil
	case "q":
		cmd := model.requestStop()
		return model, cmd
	}
	return model, nil
}

// traceGroupCycle은 Tab이 넘겨 가는 순서다. 기본 화면인 event 스크롤에서 시작해 s, t, p, c, e 키와 같은 순서로 간다.
var traceGroupCycle = []string{"", traceGroupBySource, traceGroupByTarget, traceGroupByPort, traceGroupByProcess, traceGroupByEvent}

var traceGroupKeyViews = map[string]string{"g": "", "s": traceGroupBySource, "t": traceGroupByTarget, "p": traceGroupByPort, "c": traceGroupByProcess, "e": traceGroupByEvent, "u": traceGroupByPath}

func nextTraceGroup(views []string, current string, step int) string {
	index := 0
	for position, groupBy := range views {
		if groupBy == current {
			index = position
		}
	}
	return views[(index+step+len(views))%len(views)]
}

// traceScreenHelp는 protocol이 쓰는 보기의 키만 보여 준다.
func traceScreenHelp(protocol string) string {
	keys := []string{"/ filter", "tab view"}
	for _, view := range traceGroupViews(protocol) {
		for key, keyView := range traceGroupKeyViews {
			if view != "" && keyView == view {
				keys = append(keys, key+" "+view)
			}
		}
	}
	keys = append(keys, "g scroll", "↑↓ select", "enter detail", "i split", "f follow")
	switch protocol {
	case "http":
		keys = append(keys, "v payload", "m secrets")
	case "socket":
		keys = append(keys, "v payload")
	}
	return strings.Join(append(keys, "l live", "esc clear", "q quit", "ctrl-c stop"), "  ")
}

// helpOn은 키 안내에서 지금 켜진 묶음이다. 지금 보기와 i, f, v, m으로 켠 상태다.
// 미리 보기는 event 목록에서만 그리므로, 다른 보기에서는 split이 켜져 있어도 칠하지 않는다.
func (model traceScreenModel) helpOn() []string {
	on := []string{traceViewOn(traceScreenHelp(model.protocol), model.groupBy)}
	for _, state := range []struct {
		on   bool
		hint string
	}{{model.split && model.groupBy == "", "i split"}, {model.follow, "f follow"}, {model.payloadLines, "v payload"}, {model.secrets, "m secrets"}} {
		if state.on {
			on = append(on, state.hint)
		}
	}
	return on
}

func traceStopTick(sequence uint64) tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(at time.Time) tea.Msg {
		return traceStopTickMsg{sequence: sequence, at: at}
	})
}

func (model *traceScreenModel) requestStop() tea.Cmd {
	if model.stopping || model.finished != nil {
		return nil
	}
	model.stopping = true
	model.stopStarted = time.Now()
	model.stopElapsed = 0
	model.stopTick = 1
	model.detailGeneration++
	model.detailJobPending = nil
	if model.stop != nil {
		model.stop()
	}
	return traceStopTick(model.stopTick)
}

func (model *traceScreenModel) requestVisiblePayload() tea.Cmd {
	if model.detail != nil {
		return model.requestDetailBuild()
	}
	return model.requestPreviewBuild()
}

func (model traceScreenModel) View() tea.View {
	if model.stopping && model.finished == nil {
		content := traceScreenStoppingView(model)
		view := liveFrame(strings.Join(content, "\n")+"\n", model.height)
		view.AltScreen = true
		return view
	}
	if model.detail != nil {
		view := liveFrame(strings.Join(traceScreenDetailView(model), "\n")+"\n", model.height)
		view.AltScreen = true
		return view
	}
	header := traceScreenHeader(model)
	rows := traceScreenRows(model)
	content := strings.Join(append(header, rows...), "\n") + "\n"
	view := liveFrame(content, model.height)
	view.AltScreen = true
	return view
}

func traceScreenStoppingView(model traceScreenModel) []string {
	color := traceScreenColor()
	title := traceTexts(traceTitleSpans("edc trace "+traceLabel(model.protocol, model.side)), model.width, color)
	spinner := traceTexts([]traceText{{text: liveSpinnerFrames[model.stopFrame%len(liveSpinnerFrames)] + " ", color: richMagenta}, {text: "Stopping safely…", color: richYellow, bold: true}}, model.width, color)
	waiting := traceTexts([]traceText{{text: "Waiting for capture cleanup and final results."}}, model.width, color)
	elapsed := traceTexts([]traceText{{text: "Elapsed: "}, {text: model.stopElapsed.Truncate(time.Second).String()}}, model.width, color)
	return []string{title, spinner, waiting, elapsed}
}

func traceScreenHeader(model traceScreenModel) []string {
	status := []traceText{{text: "live", color: richGreen}}
	if model.selected >= 0 && model.groupBy == "" {
		status = []traceText{{text: fmt.Sprintf("live · pinned #%d · %d new", model.selected+1, max(0, model.first+len(model.events)-1-model.selected)), color: richGreen}}
	}
	if model.secrets && model.protocol == "http" {
		status = append(status, traceSeparator, traceText{text: "secrets shown", color: richYellow})
	}
	if model.stopping {
		status = []traceText{{text: "stopping", color: richYellow}}
	}
	filter := "all"
	if model.filter != "" {
		filter = model.filter
	}
	line := traceTitleSpans("edc trace " + traceLabel(model.protocol, model.side))
	if model.container != "" {
		line = append(line, traceLabelSpan(" in container "), traceText{text: model.container})
	}
	var report traceGroupReport
	if model.groupBy != "" {
		report = model.groupReport()
		line = append(line, traceLabelSpan(" grouped by "), traceText{text: model.groupBy}, traceSeparator)
		line = append(line, status...)
		line = append(line, traceSeparator)
		line = append(line, traceField("events ", fmt.Sprint(report.Events), "")...)
		line = append(line, traceSeparator)
		line = append(line, traceField(model.groupBy+" ", fmt.Sprint(len(report.Groups)), "")...)
		line = append(line, traceSeparator)
		line = append(line, traceField("event/s ", fmt.Sprintf("%.1f", report.Rate), "")...)
		if !report.hideTraffic() {
			line = append(line, traceSeparator)
			line = append(line, traceField("", fmt.Sprintf("%.1f", report.BitsPerSecond), " bps")...)
		}
	} else {
		line = append(line, traceSeparator)
		line = append(line, status...)
		line = append(line, traceSeparator)
		if traceScreenPairs(model.protocol) {
			line = append(line, traceField("raw events ", fmt.Sprint(model.received), "")...)
			line = append(line, traceSeparator)
			line = append(line, traceField("shown pairs ", fmt.Sprint(model.shownMySQLPairs()), "")...)
			line = append(line, traceSeparator)
			line = append(line, traceField("slow ", model.slow.String(), "")...)
		} else {
			line = append(line, traceField("events ", fmt.Sprint(model.received), "")...)
		}
	}
	line = append(line, traceSeparator)
	line = append(line, traceField("filter ", filter, "")...)
	color := traceScreenColor()
	help := traceKeyHints(traceScreenHelp(model.protocol), model.width, color, model.helpOn()...)
	if model.filtering {
		help = traceFilterHints(model.input.View(), model.width, color)
	}
	tableWidth := model.width
	if model.groupBy == "" {
		tableWidth = max(1, model.width-2)
	}
	timeColumn, destinationWidth, eventWidth, sourceWidth := traceScreenColumns(tableWidth)
	columns := liveCell("PROCESS", traceScrollProcessWidth) + " " + liveCell("DESTINATION", destinationWidth) + " " + liveCell("EVENT", eventWidth) + " " + liveCell("SOURCE", sourceWidth)
	if timeColumn > 0 {
		columns = liveCell("TIME", traceScrollTimeWidth) + " " + columns
	}
	if model.groupBy != "" {
		layout := traceScreenGroupLayout(model, report)
		names := []any{"EVT", "E/s"}
		if !traceProtocols[model.protocol].hideTraffic {
			names = append(names, "TX", "RX", "TOT", "B/s", "Mbps")
		}
		for _, column := range traceProtocols[model.protocol].groupColumns {
			names = append(names, column.screenTitle)
		}
		columns = liveCell(traceGroupLabel(model.groupBy), layout.labelWidth) + fmt.Sprintf(traceGroupColumns(model.protocol, layout.byteWidth), append(names, "LAST")...)
	} else {
		columns = "  " + columns
	}
	header := []string{traceTexts(line, model.width, color), help, traceColumnTitles(columns, model.width, color)}
	if len(model.tlsNotices) != 0 || model.tlsNoticeOverflow != 0 {
		notice := strings.Join(model.tlsNotices, " · ")
		if model.tlsNoticeOverflow != 0 {
			notice += fmt.Sprintf(" · %d TLS notices omitted", model.tlsNoticeOverflow)
		}
		header = append(header, traceTexts([]traceText{{text: "TLS: " + notice, color: richYellow}}, model.width, color))
	}
	return header
}

func (model traceScreenModel) shownMySQLPairs() int {
	shown := 0
	for _, row := range model.displayRows() {
		if row.response >= model.first && row.response < model.first+len(model.events) && model.displayRowMatches(row) {
			shown++
		}
	}
	return shown
}

// traceScreenRows는 머리글 아래의 줄이다. 화면 나누기가 켜져 있으면 목록 아래에 미리 보기 창을 붙인다.
func traceScreenRows(model traceScreenModel) []string {
	if list, _, ok := traceSplitHeightsFor(model); model.split && model.groupBy == "" && ok {
		available := max(0, model.height-len(traceScreenHeader(model)))
		list = min(list, available)
		return append(traceScreenListRows(model, list), traceScreenPreviewRows(model, max(0, available-list))...)
	}
	return traceScreenListRows(model, max(0, model.height-len(traceScreenHeader(model))))
}

// listRows는 목록에 쓰는 줄 수다. 한 쪽씩 넘길 때 쓴다.
func (model traceScreenModel) listRows() int {
	if list, _, ok := traceSplitHeightsFor(model); model.split && model.groupBy == "" && ok {
		return min(list, max(0, model.height-len(traceScreenHeader(model))))
	}
	return max(0, model.height-len(traceScreenHeader(model)))
}

func traceScreenListRows(model traceScreenModel, available int) []string {
	rows := make([]string, 0, available)
	if model.groupBy != "" {
		report := model.groupReport()
		layout := traceScreenGroupLayout(model, report)
		for _, group := range traceScreenVisibleGroups(model, report) {
			rows = append(rows, formatTraceGroupScreenRow(model.protocol, model.groupBy, group, model.width, layout))
		}
		return traceScreenPadRows(rows, available)
	}
	// 화면에 보이는 줄만 뒤에서부터 서식화한다. 보관한 event 전부(최대 10,000건)를 서식화하면 한 번 그리는 데
	// 300ms가 넘게 걸려, 화면이 event를 따라가지 못하고 키 입력도 늦어진다.
	rowsForDisplay := model.displayRows()
	eventLines := func(rowIndex int) []string {
		row := rowsForDisplay[rowIndex]
		event, destination, name, ok := model.displayRowLabels(row)
		if !ok {
			return nil
		}
		tableWidth := max(1, model.width-2)
		line := formatTraceScreenEventLabels(event, destination, name, tableWidth)
		// 짝이 없는 행의 response는 -1이라, 고른 event가 없을 때(-1) 모든 행이 고른 행으로 보이지 않게 먼저 확인한다.
		if model.selected >= 0 && (row.primary == model.selected || row.response == model.selected) {
			line = traceSelectedScreenEvent(event, destination, name, model.width, model.displayRowSeverity(row, event))
		} else {
			if tableWidth+2 >= 72 && traceScreenColor() {
				line = traceStyledEventLabels(event, destination, name, tableWidth, model.displayRowSeverity(row, event))
			}
			line = "  " + line
		}
		lines := []string{line}
		if model.payloadLines && event.Payload != "" {
			if !model.secrets && model.protocol == "http" {
				event.Payload = string(traceMaskHTTPHeaders([]byte(event.Payload)))
			}
			lines = append(lines, "  "+formatTraceScreenPayload(event, tableWidth))
		}
		return lines
	}
	// 고른 event가 있으면 그 event를 맨 아래에 두고 그보다 오래된 event를 위에 채운다.
	start := len(rowsForDisplay) - 1
	if model.selected >= 0 {
		if selected := model.displayRowForSelected(rowsForDisplay); selected >= 0 {
			start = selected
		}
	}
	for index := start; index >= 0; index-- {
		if !model.displayRowMatches(rowsForDisplay[index]) {
			continue
		}
		lines := eventLines(index)
		// 넘친 채로 두면 traceScreenPadRows가 위를 잘라 event 행 없이 payload 줄만 남으므로, 두 줄이 다 들어가지 않으면 멈춘다.
		if len(rows)+len(lines) > available {
			break
		}
		for line := len(lines) - 1; line >= 0; line-- {
			rows = append(rows, lines[line])
		}
	}
	slices.Reverse(rows)
	// 오래된 event가 모자라 화면이 남으면 고른 event 아래에 더 새 event를 채운다. 멈춘 동안 event가 10,000개 넘게 쌓이면
	// 고른 event가 가장 오래된 event로 밀려서, 전에는 목록이 한 줄만 보였다.
	for index := start + 1; model.selected >= 0 && index < len(rowsForDisplay); index++ {
		if !model.displayRowMatches(rowsForDisplay[index]) {
			continue
		}
		lines := eventLines(index)
		if len(rows)+len(lines) > available {
			break
		}
		rows = append(rows, lines...)
	}
	return traceScreenPadRows(rows, available)
}

// traceSelectedEvent는 고른 행을 chip 바탕에 굵게 쓴다. 바탕이 행 끝까지 이어지도록 폭을 채운다. line에 색이 있으면
// 안의 reset이 바탕을 끊으므로 색을 칠하기 전의 행을 받는다.
func traceSelectedEvent(line string, width int, color bool) string {
	if !color {
		return line
	}
	style := traceStyle().Background(lipgloss.Color(richChipHex())).Foreground(lipgloss.Color(richHex("97"))).Bold(true)
	if width > liveWidth(line) {
		style = style.Width(width)
	}
	return style.Render(line)
}

func traceSelectedEventStatus(line string, width int, color bool, severity, name string) string {
	if !color {
		return traceFit("▸ "+line, width)
	}
	cellWidth := max(0, width-2)
	line = traceFit(line, cellWidth)
	if padding := cellWidth - liveWidth(line); padding > 0 {
		line += strings.Repeat(" ", padding)
	}
	base := traceStyle().Background(lipgloss.Color("#2d1c3e")).Foreground(lipgloss.Color(richHex("97"))).Bold(true)
	selected := base.Render(line)
	if severity != "" && name != "" {
		if index := strings.LastIndex(line, name); index >= 0 {
			semantic := traceStyle().Background(lipgloss.Color("#2d1c3e")).Foreground(lipgloss.Color(richHex(severity))).Bold(true)
			selected = base.Render(line[:index]) + semantic.Render(line[index:index+len(name)]) + base.Render(line[index+len(name):])
		}
	}
	style := base
	if cellWidth > liveWidth(line) {
		style = style.Width(cellWidth)
	}
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(richHex(richMagenta))).Render(liveSelectedBar)
	if severity == "" {
		selected = style.Render(line)
	}
	return bar + " " + selected
}

func traceSelectedScreenEvent(event captureEvent, destination, name string, width int, severity string) string {
	if !traceScreenColor() {
		return traceFit("▸ "+formatTraceScreenEventLabels(event, destination, name, max(1, width-2)), width)
	}
	if width < 72 {
		return traceSelectedEventStatus(formatTraceScreenEventLabels(event, destination, name, width-2), width, true, severity, name)
	}
	cellWidth := max(1, width-2)
	cells := traceScreenEventCells(event, destination, name, cellWidth)
	first := len(cells) - 4
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(richHex(richMagenta))).Render(liveSelectedBar)
	base := traceStyle().Bold(true).Background(lipgloss.Color("#2d1c3e")).Foreground(lipgloss.Color(richHex("97")))
	selected := make([]string, 0, len(cells)*2)
	for index, cell := range cells {
		style := base
		if index == first+2 && severity != "" {
			style = style.Foreground(lipgloss.Color(richHex(severity)))
		}
		selected = append(selected, style.Render(cell))
		if index < len(cells)-1 {
			selected = append(selected, base.Render(" "))
		}
	}
	return bar + " " + strings.Join(selected, "")
}

// displayRowSeverity는 행의 event가 알리는 문제의 색이다. 요청과 응답을 짝지은 행은 응답의 event로 판단한다.
func (model traceScreenModel) displayRowSeverity(row traceScreenRow, event captureEvent) string {
	if row.response >= model.first && row.response < model.first+len(model.events) && traceScreenPairs(event.Protocol) {
		event = model.events[row.response-model.first]
	}
	return traceEventSeverity(event)
}

// traceScreenGroupOrder는 화면에 다 들어가지 않을 때 잘릴 group을 정한다. 이름순으로 자르면 같은
// group이 session 내내 보이지 않으므로, traffic이 많은 group을 위에 둔다.
func traceScreenGroupOrder(groups []traceGroupSummary) []traceGroupSummary {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].TotalBytes != groups[j].TotalBytes {
			return groups[i].TotalBytes > groups[j].TotalBytes
		}
		return groups[i].Events > groups[j].Events
	})
	return groups
}

func traceScreenPadRows(rows []string, available int) []string {
	available = max(0, available)
	if len(rows) > available {
		rows = rows[len(rows)-available:]
	}
	for len(rows) < available {
		rows = append(rows, "")
	}
	return rows
}

func (model traceScreenModel) groupReport() traceGroupReport {
	filtered := make([]captureEvent, 0, len(model.events))
	for _, event := range model.events {
		if traceEventMatchesText(event, model.filter) {
			filtered = append(filtered, event)
		}
	}
	return summarizeTraceGroups(model.protocol, model.groupBy, filtered, captureSummary{}, model.windowDuration(time.Now()), model.process, model.destination)
}

// windowDuration은 model.events가 덮는 시간이다. 창이 잘린 뒤에도 session 시작부터 재면 event 수는
// 고정인데 시간만 늘어서 rate가 0으로 수렴한다.
func (model traceScreenModel) windowDuration(now time.Time) time.Duration {
	end := now
	if limit := model.started.Add(model.duration); model.duration > 0 && limit.Before(end) {
		end = limit
	}
	start := model.started
	if model.truncated && len(model.arrivals) > 0 {
		start = model.arrivals[0]
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func traceScreenVisibleGroups(model traceScreenModel, report traceGroupReport) []traceGroupSummary {
	groups := traceScreenGroupOrder(report.Groups)
	return groups[:min(len(groups), max(0, model.height-3))]
}

const (
	traceGroupMinLabelWidth = 14
	traceGroupLastWidth     = len("tcp_receive_reset")
	// traceBytes는 9999.9GiB까지 9칸 안에 쓰므로 넓은 화면에서 byte 열이 밀리지 않는다.
	traceGroupWideByteWidth = 9
	// 좁은 화면은 예전 6칸을 지켜야 TCP의 CON RET RST가 80칸 안에 남는다. 대신 traceCompactBytes로 짧게 쓴다.
	traceGroupNarrowByteWidth = 6
)

type traceGroupLayout struct {
	labelWidth int
	byteWidth  int
	bytes      func(uint64) string
}

// traceGroupColumns는 헤더와 행이 같은 폭을 쓰도록 label 뒤 열 형식을 만든다.
func traceGroupColumns(protocol string, byteWidth int) string {
	return strings.Join(traceGroupColumnFormats(protocol, byteWidth), "")
}

// traceGroupColumnFormats는 traceGroupColumns를 열마다 나눈 형식이다. 색을 쓰는 화면은 열을 하나씩 채운 뒤 칠한다.
// 칠한 값을 한 번에 채우면 escape까지 폭으로 세어 뒤 열이 밀린다.
func traceGroupColumnFormats(protocol string, byteWidth int) []string {
	columns := []string{" %5v", " %6v"}
	if !traceProtocols[protocol].hideTraffic {
		for range 4 {
			columns = append(columns, fmt.Sprintf(" %%%dv", byteWidth))
		}
		columns = append(columns, " %5v")
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		columns = append(columns, fmt.Sprintf(" %%%dv", column.width))
	}
	return append(columns, " %v")
}

// traceGroupColumnsWidth는 label 뒤 열의 폭이다. LAST 앞의 공백까지 센다.
func traceGroupColumnsWidth(protocol string, byteWidth int) int {
	width := 6 + 7 + 1
	if !traceProtocols[protocol].hideTraffic {
		width += 4*(byteWidth+1) + 6
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		width += column.width + 1
	}
	return width
}

// traceScreenGroupLayout은 넓은 terminal에서 byte 열과 group 값을 자르지 않도록 열을 늘린다.
// 좁은 terminal에서는 예전 폭을 유지한다.
func traceScreenGroupLayout(model traceScreenModel, report traceGroupReport) traceGroupLayout {
	layout := traceGroupLayout{byteWidth: traceGroupWideByteWidth, bytes: traceBytes}
	if model.width < traceGroupMinLabelWidth+traceGroupColumnsWidth(model.protocol, traceGroupWideByteWidth)+traceGroupLastWidth {
		layout = traceGroupLayout{byteWidth: traceGroupNarrowByteWidth, bytes: traceCompactBytes}
	}
	longest := 0
	for _, group := range traceScreenVisibleGroups(model, report) {
		longest = max(longest, liveWidth(traceGroupDisplayValue(model.groupBy, group)))
	}
	layout.labelWidth = max(traceGroupMinLabelWidth, min(longest, model.width-traceGroupColumnsWidth(model.protocol, layout.byteWidth)-traceGroupLastWidth))
	return layout
}

// traceCompactBytes는 byte 값을 4칸 안에 쓴다. 단위는 traceBytes와 같은 1024 배수다.
func traceCompactBytes(bytes uint64) string {
	if bytes < 1000 {
		return fmt.Sprintf("%dB", bytes)
	}
	value := float64(bytes)
	for _, unit := range []string{"K", "M", "G", "T"} {
		value /= 1024
		if value < 9.95 {
			return fmt.Sprintf("%.1f%s", value, unit)
		}
		if value < 999.5 {
			return fmt.Sprintf("%.0f%s", value, unit)
		}
	}
	return fmt.Sprintf("%.0fP", value/1024)
}

// traceScreenEventRate는 E/s 값을 6칸 안에 쓴다. 값이 크면 소수 자리를 버린다.
func traceScreenEventRate(rate float64) string {
	switch {
	case rate < 9999.95:
		return fmt.Sprintf("%.1f", rate)
	case rate < 999999.5:
		return fmt.Sprintf("%.0f", rate)
	}
	return fmt.Sprintf("%.0fk", rate/1000)
}

// traceScreenMegabits는 Mbps 값을 5칸 안에 쓴다. 1 Gbps를 넘는 traffic에서도 뒤 열이 밀리지 않게 소수 자리를 줄인다.
func traceScreenMegabits(megabits float64) string {
	switch {
	case megabits < 99.995:
		return fmt.Sprintf("%.2f", megabits)
	case megabits < 999.95:
		return fmt.Sprintf("%.1f", megabits)
	case megabits < 99999.5:
		return fmt.Sprintf("%.0f", megabits)
	}
	return fmt.Sprintf("%.0fG", megabits/1000)
}

const traceServerSuffix = " (server)"

// traceGroupFitLabel은 첫 열을 자를 때 서버 표시를 남긴다. 표시가 잘리면 같은 주소의 client 행과 구분되지
// 않는다. 주소는 서비스를 가리키는 port 쪽을 남긴다. 앞쪽을 남기면 127.0.0.1:19999와 127.0.0.53:53이
// 똑같이 127.0…으로 보인다.
func traceGroupFitLabel(groupBy string, group traceGroupSummary, width int) string {
	value := traceGroupDisplayValue(groupBy, group)
	if !group.Server || liveWidth(value) <= width || width <= liveWidth(traceServerSuffix)+1 {
		return traceFit(value, width)
	}
	return traceKeepTail(group.Group, width-liveWidth(traceServerSuffix)) + traceServerSuffix
}

func traceKeepTail(value string, width int) string {
	if liveWidth(value) <= width {
		return value
	}
	tail := value
	if index := strings.LastIndex(value, ":"); index >= 0 && liveWidth(value[index:])+1 <= width {
		tail = value[index:]
	}
	runes := []rune(tail)
	for len(runes) > 0 && liveWidth(string(runes))+1 > width {
		runes = runes[1:]
	}
	return "…" + string(runes)
}

func formatTraceGroupScreenRow(protocol, groupBy string, group traceGroupSummary, width int, layout traceGroupLayout) string {
	// liveCell은 폭보다 긴 값을 여러 줄로 감싸므로 먼저 자른다.
	value := liveCell(traceGroupFitLabel(groupBy, group, layout.labelWidth), layout.labelWidth)
	values := []any{group.Events, traceScreenEventRate(group.Rate)}
	if !traceProtocols[protocol].hideTraffic {
		values = append(values, layout.bytes(group.TXBytes), layout.bytes(group.RXBytes), layout.bytes(group.TotalBytes), layout.bytes(uint64(group.BytesPerSecond)), traceScreenMegabits(group.MegabitsPerSecond))
	}
	for _, column := range traceProtocols[protocol].groupColumns {
		values = append(values, column.value(group))
	}
	values = append(values, group.LastEvent)
	line := value + fmt.Sprintf(traceGroupColumns(protocol, layout.byteWidth), values...)
	if !traceScreenColor() {
		return traceFit(line, width)
	}
	// 문제를 세는 열은 0보다 클 때만 칠한다. 다른 값은 기본 색이다.
	colors := make([]string, len(values))
	offset := len(values) - 1 - len(traceProtocols[protocol].groupColumns)
	for index, column := range traceProtocols[protocol].groupColumns {
		if count, err := strconv.ParseFloat(column.value(group), 64); err == nil && count > 0 {
			colors[offset+index] = traceGroupColumnSeverity[column.screenTitle]
		}
	}
	colors[len(colors)-1] = traceEventSeverity(captureEvent{Protocol: protocol, Event: group.LastEvent})
	spans := []traceText{{text: value}}
	for index, format := range traceGroupColumnFormats(protocol, layout.byteWidth) {
		spans = append(spans, traceText{text: fmt.Sprintf(format, values[index]), color: colors[index]})
	}
	return traceTexts(spans, width, true)
}

// traceGroupColumnSeverity는 문제를 세는 group 열의 색이다. 끊김과 오류는 빨강, 재전송과 실패와 답 없음은 노랑이다.
var traceGroupColumnSeverity = map[string]string{
	"RST": richRed, "CHG": richRed, "5XX": richRed, "ERR": richRed,
	"RET": richYellow, "FAIL": richYellow, "4XX": richYellow, "NOANS": richYellow,
}

func traceEventMatchesText(event captureEvent, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	// 화면의 목적지 칸과 event 칸에 보이는 값도 찾는다. HTTP의 method와 path, DNS의 질의 종류는 이 칸에만 보인다.
	destination, name := traceScrollLabels(event)
	text := strings.ToLower(strings.Join([]string{event.Process, event.Target, event.Source, event.Destination, event.Event, destination, name}, " "))
	return strings.Contains(text, filter)
}

const (
	traceScrollProcessWidth = 16
	traceScrollEventWidth   = 20
	// traceScrollEventMaxWidth는 넓은 화면에서 event 칸을 늘리는 상한이다. http_2xx tls 200 162.3ms처럼 결과와 응답 시간이
	// 붙으면 20칸을 넘는다.
	traceScrollEventMaxWidth = 32
	// traceScrollSourceWidth는 목적지 칸을 넓힐 때 source 칸에 남기는 폭이다. 100.83.200.248:52406 같은 값이 들어간다.
	traceScrollSourceWidth = 22
	// TIME 칸은 15:04:05.000을 쓴다. 좁은 화면에서 목적지 칸을 더 줄이지 않도록 traceScrollTimeMinWidth 이상일 때만 둔다.
	traceScrollTimeWidth    = 12
	traceScrollTimeMinWidth = 120
)

// traceScrollTimeColumn은 TIME 칸과 뒤의 빈칸을 더한 폭이다. 좁은 화면에서는 0이다.
func traceScrollTimeColumn(width int) int {
	if width < traceScrollTimeMinWidth {
		return 0
	}
	return traceScrollTimeWidth + 1
}

func traceScreenColumns(tableWidth int) (int, int, int, int) {
	screenWidth := tableWidth + 2
	timeColumn := traceScrollTimeColumn(screenWidth)
	destination, event, source := traceScrollColumns(screenWidth - timeColumn)
	return timeColumn, max(1, destination-2), event, source
}

// traceEventClock은 event의 시각을 이 host의 시간대로 쓴다. 시각이 없는 event는 -다.
func traceEventClock(event captureEvent) string {
	if event.TimestampNS == 0 {
		return "-"
	}
	return time.Unix(0, int64(event.TimestampNS)).Format("15:04:05.000")
}

// traceScrollColumns는 스크롤 화면의 목적지, event, source 칸 폭이다. 목적지에는 도메인과 path가 붙어 길어지므로 넓은
// 화면에서 목적지 칸을 늘린다. 좁은 화면에서는 예전처럼 32칸을 지킨다. 60칸을 넘는 폭은 event 칸을 먼저 늘리고 나머지를
// 목적지 칸에 준다. source 값은 주소 하나라 traceScrollSourceWidth보다 넓혀도 빈칸만 는다.
func traceScrollColumns(width int) (int, int, int) {
	fixed := traceScrollProcessWidth + 3
	event := traceScrollEventWidth
	destination := min(60, max(32, width-fixed-event-traceScrollSourceWidth))
	if spare := width - fixed - event - destination - traceScrollSourceWidth; spare > 0 {
		grow := min(spare, traceScrollEventMaxWidth-event)
		event, destination = event+grow, destination+spare-grow
	}
	return destination, event, max(1, width-fixed-event-destination)
}

func formatTraceScreenEventLabels(event captureEvent, destination, name string, width int) string {
	if width+2 < 72 {
		process, source := emptyAs(event.Process, "-"), emptyAs(event.Source, "-")
		return traceFit(strings.Join([]string{process, destination, name, source}, "  "), width)
	}
	return strings.Join(traceScreenEventCells(event, destination, name, width), " ")
}

// traceScreenEventCells는 넓은 화면의 event 행을 칸마다 채운 값이다. TIME 칸이 있으면 맨 앞이다.
func traceScreenEventCells(event captureEvent, destination, name string, width int) []string {
	process, source := emptyAs(event.Process, "-"), emptyAs(event.Source, "-")
	timeColumn, destinationWidth, eventWidth, sourceWidth := traceScreenColumns(width)
	cell := func(value string, cellWidth int) string { return liveCell(traceFit(value, cellWidth), cellWidth) }
	cells := []string{cell(process, traceScrollProcessWidth), cell(destination, destinationWidth), cell(name, eventWidth), cell(source, sourceWidth)}
	if timeColumn > 0 {
		cells = append([]string{cell(traceEventClock(event), traceScrollTimeWidth)}, cells...)
	}
	return cells
}

// traceStyledEventLabels는 넓은 화면의 event 행을 칠한다. 시각과 source는 회색, event 칸은 severity의 색이다.
func traceStyledEventLabels(event captureEvent, destination, name string, width int, severity string) string {
	cells := traceScreenEventCells(event, destination, name, width)
	first := len(cells) - 4
	if first > 0 {
		cells[0] = traceDim(cells[0])
	}
	cells[first+2] = tracePaint(cells[first+2], severity, false)
	cells[first+3] = traceDim(cells[first+3])
	return strings.Join(cells, " ")
}

// formatTraceScreenEventLine은 색을 칠하기 전의 행이다. 고른 행은 이 행을 chip 바탕에 굵게 쓴다.
func formatTraceScreenEventLine(event captureEvent, width int) string {
	destination, name := traceScrollLabels(event)
	process, source := event.Process, event.Source
	if process == "" {
		process = "-"
	}
	if source == "" {
		source = "-"
	}
	if width < 72 {
		return traceFit(strings.Join([]string{process, destination, name, source}, "  "), width)
	}
	timeColumn := traceScrollTimeColumn(width)
	destinationWidth, eventWidth, sourceWidth := traceScrollColumns(width - timeColumn)
	// liveCell은 칸보다 긴 값을 여러 줄로 감싸므로, 한 행을 지키려고 먼저 자른다.
	cell := func(value string, width int) string { return liveCell(traceFit(value, width), width) }
	line := cell(process, traceScrollProcessWidth) + " " + cell(destination, destinationWidth) + " " + cell(name, eventWidth) + " " + cell(source, sourceWidth)
	if timeColumn > 0 {
		line = cell(traceEventClock(event), traceScrollTimeWidth) + " " + line
	}
	return line
}

// formatTraceScreenPayload는 event 행 아래에 목적지 칸부터 payload를 한 줄로 쓴다.
func formatTraceScreenPayload(event captureEvent, width int) string {
	line := "↳ " + tracePayloadSummary(event)
	if width+2 >= 72 {
		timeColumn, _, _, _ := traceScreenColumns(width)
		line = strings.Repeat(" ", timeColumn+traceScrollProcessWidth+1) + line
	}
	line = traceFit(line, width)
	if !traceScreenColor() {
		return line
	}
	return traceDim(line)
}

// traceEventSeverity는 event가 알리는 문제의 색이다. 문제가 없으면 빈 문자열이고 기본 색으로 쓴다. 한 화면은 한
// protocol만 보이므로 protocol마다 색을 나누지 않고, 끊김과 오류와 버린 패킷은 빨강, 재전송과 실패는 노랑으로 쓴다.
// HTTP 응답은 status code의 색이다.
func traceEventSeverity(event captureEvent) string {
	name := event.Event
	switch {
	case traceProtocol(event) == "http" && event.Status >= 200:
		return richHTTPCodeColor(event.Status)
	case traceProtocol(event) == "drop", strings.Contains(name, "reset"), strings.HasSuffix(name, "_"+traceNeighborMACChange),
		name == "http_5xx", strings.HasSuffix(name, "_error"), name == clickHouseEventException:
		return richRed
	case strings.Contains(name, "retransmit"), strings.Contains(name, "fail"), name == "http_4xx":
		return richYellow
	}
	return ""
}

// traceScreenColor는 전체 화면이 색을 쓸지다. 화면은 NO_COLOR가 없을 때만 뜨지만, model을 직접 그리는 test도 같은 규칙을 따른다.
func traceScreenColor() bool { return os.Getenv("NO_COLOR") == "" }

// traceStyle은 trace 화면의 lipgloss 기본 style이다. 칠하기 전 행과 칠한 행의 글자가 같도록 tab을 바꾸지 않는다.
func traceStyle() lipgloss.Style { return lipgloss.NewStyle().TabWidth(lipgloss.NoTabConversion) }

// tracePaint는 color(ANSI 번호)로 글자를 칠한다. color가 비면 bold만 적용하고, 그것도 없으면 그대로 둔다.
// 색은 rich.go의 고정 팔레트이고, Bubble Tea가 터미널이 받는 색 수로 줄인다.
func tracePaint(text, color string, bold bool) string {
	if text == "" || (color == "" && !bold) {
		return text
	}
	style := traceStyle().Bold(bold)
	if color != "" {
		style = style.Foreground(lipgloss.Color(richHex(color)))
	}
	return style.Render(text)
}

func traceDim(text string) string { return tracePaint(text, richGray, false) }

// traceFitColored는 칠한 줄을 width칸에 맞춘다. traceFit처럼 넘치면 끝을 …로 바꾸되 escape를 자르지 않는다.
func traceFitColored(line string, width int) string {
	if width <= 0 || liveWidth(line) <= width {
		return line
	}
	return ansi.Truncate(line, width, "…")
}

// traceText는 머리글 한 조각이다. 평문 화면은 text만 잇고, 색을 쓰면 color와 bold로 칠한다.
type traceText struct {
	text  string
	color string
	bold  bool
	// on은 지금 켜진 키 안내다. 고른 항목과 같은 chip 바탕에 굵은 글씨로 칠한다.
	on bool
}

var traceSeparator = traceText{text: "  ·  ", color: richGray}

func traceTitleSpans(title string) []traceText {
	return []traceText{{text: title, color: richMagenta, bold: true}}
}

func traceLabelSpan(text string) traceText { return traceText{text: text, color: richGray} }

// traceField는 "events 3"이나 "1.5 bps" 같은 값 한 칸이다. 이름과 단위는 회색, 값은 기본 색이다.
func traceField(label, value, unit string) []traceText {
	spans := []traceText{}
	if label != "" {
		spans = append(spans, traceLabelSpan(label))
	}
	spans = append(spans, traceText{text: value})
	if unit != "" {
		spans = append(spans, traceLabelSpan(unit))
	}
	return spans
}

// traceTexts는 조각을 이은 한 줄을 width칸에 맞춘다. 색을 쓰지 않으면 평문을 traceFit으로 자른 예전 줄과 같다.
// 색을 쓰면 평문을 자른 자리에서 조각을 끊는다. 그래서 칠한 줄의 글자는 평문 줄과 같고, 잘린 뒤의 escape가 남지 않는다.
func traceTexts(spans []traceText, width int, color bool) string {
	var plain strings.Builder
	for _, span := range spans {
		plain.WriteString(span.text)
	}
	fitted := traceFit(plain.String(), width)
	if !color {
		return fitted
	}
	cut := fitted != plain.String()
	keep := fitted
	if cut {
		keep = strings.TrimSuffix(fitted, "…")
	}
	var line strings.Builder
	last := traceText{}
	for _, span := range spans {
		if keep == "" {
			break
		}
		text := keep[:min(len(span.text), len(keep))]
		line.WriteString(traceSpanPaint(text, span))
		keep, last = keep[len(text):], span
	}
	if cut {
		line.WriteString(traceSpanPaint("…", last))
	}
	return line.String()
}

// traceStatusSpans는 exec와 sched 화면의 첫 줄이다. "edc trace exec  ·  live  ·  events 3  ·  filter all"이다.
func traceStatusSpans(title string, stopping bool, events int, filter string) []traceText {
	status := traceText{text: "live", color: richGreen}
	if stopping {
		status = traceText{text: "stopping", color: richYellow}
	}
	spans := append(traceTitleSpans(title), traceSeparator, status, traceSeparator)
	spans = append(spans, traceField("events ", fmt.Sprint(events), "")...)
	return append(append(spans, traceSeparator), traceField("filter ", emptyAs(filter, "all"), "")...)
}

// traceKeySpans는 "/ filter  tab view" 같은 키 안내를 조각으로 나눈다. 키는 굵은 청록, 설명은 회색이다.
// on에 든 묶음(전체 글자나 그 키)은 켜진 항목으로 칠한다.
func traceKeySpans(help string, on ...string) []traceText {
	items := make([]liveHintItem, 0, len(on))
	for _, entry := range on {
		key, _, _ := strings.Cut(entry, " ")
		items = append(items, liveHintItem{key: key, on: key != ""})
	}
	var spans []traceText
	for index, entry := range strings.Split(help, "  ") {
		if index > 0 {
			spans = append(spans, traceText{text: "  "})
		}
		key, action, found := strings.Cut(entry, " ")
		if liveHintOn(key, items) {
			spans = append(spans, traceText{text: entry, on: true})
			continue
		}
		spans = append(spans, traceText{text: key, color: richCyan, bold: true})
		if found {
			spans = append(spans, traceLabelSpan(" "+action))
		}
	}
	return spans
}

// traceKeyHints는 화면 둘째 줄의 키 안내다.
func traceKeyHints(help string, width int, color bool, on ...string) string {
	return traceTexts(traceKeySpans(help, on...), width, color)
}

// traceViewOn은 키 안내에서 지금 보기를 가리키는 묶음이다. 보기 이름이 묶음의 설명과 같고, 묶지 않은 event 목록은 "g scroll"이다.
func traceViewOn(help, groupBy string) string {
	if groupBy == "" {
		return "g scroll"
	}
	for _, entry := range strings.Split(help, "  ") {
		if _, label, found := strings.Cut(entry, " "); found && label == groupBy {
			return entry
		}
	}
	return ""
}

// traceSpanPaint는 조각 하나를 칠한다. 켜진 키 안내는 고른 항목과 같은 chip 바탕에 굵은 글씨다.
func traceSpanPaint(text string, span traceText) string {
	if !span.on || text == "" {
		return tracePaint(text, span.color, span.bold)
	}
	return traceStyle().Bold(true).Background(lipgloss.Color(richChipHex())).Render(text)
}

// traceFilterHints는 filter를 입력하는 동안의 둘째 줄이다. 입력 칸은 textinput이 그린 그대로 둔다.
func traceFilterHints(input string, width int, color bool) string {
	const keys = "  enter apply  esc cancel"
	if !color {
		return traceFit(input+keys, width)
	}
	return traceFitColored(input+"  "+traceTexts(traceKeySpans(strings.TrimPrefix(keys, "  ")), 0, true), width)
}

// traceInputStyles는 filter 입력 칸의 색이다. textinput의 기본 색은 터미널 테마가 정하는 ANSI 번호라서 팔레트로 바꾼다.
// prompt는 키처럼 굵은 청록, placeholder는 회색, 입력한 글자는 기본 색이다.
func traceInputStyles() textinput.Styles {
	styles := textinput.DefaultDarkStyles()
	for _, state := range []*textinput.StyleState{&styles.Focused, &styles.Blurred} {
		state.Prompt = traceStyle().Foreground(lipgloss.Color(richHex(richCyan))).Bold(true)
		state.Placeholder = traceStyle().Foreground(lipgloss.Color(richHex(richGray)))
		state.Suggestion = state.Placeholder
		state.Text = traceStyle()
	}
	styles.Cursor.Color = lipgloss.Color(richHex("97"))
	return styles
}

// traceColumnTitles는 열 이름 줄이다. 열 이름은 회색이다.
func traceColumnTitles(columns string, width int, color bool) string {
	columns = traceFit(columns, width)
	if !color {
		return columns
	}
	return traceDim(columns)
}

func traceFit(value string, width int) string {
	if width <= 0 {
		return value
	}
	// truncateLine은 한 글자씩 줄이며 폭을 다시 재서, 4KiB payload 한 줄에 200ms가 넘게 걸렸다. 화면을 그릴 때마다 이만큼
	// 걸려 키 입력이 밀렸다. 글자 하나는 적어도 한 칸이므로 width+1자에서 먼저 자른다. 색 escape가 있으면 글자 수와 칸 수가
	// 달라서 그대로 둔다.
	cut := false
	if !strings.ContainsRune(value, '\x1b') {
		if index := traceRuneIndex(value, width+1); index < len(value) {
			value, cut = value[:index], true
		}
	}
	if !cut && liveWidth(value) <= width {
		return value
	}
	// truncateLine은 자른 값 끝에 줄바꿈을 붙인다. View가 행을 줄바꿈으로 이으므로 그대로 두면 잘린 행마다 빈 줄이 생긴다.
	return strings.TrimRight(truncateLine(value, width), "\n")
}

// traceRuneIndex는 value에서 count번째 글자가 시작하는 byte 위치다. 글자가 모자라면 len(value)다.
func traceRuneIndex(value string, count int) int {
	for index := range value {
		if count == 0 {
			return index
		}
		count--
	}
	return len(value)
}

// traceScreenScope는 전체 화면이 모을 범위다. trace http와 trace socket은 --payload가 없어도 message나 호출의
// 앞부분을 모은다. 옵션을 늘리지 않고 Enter와 v로 바로 보려는 것이다. 인증 header는 원문으로 두고 그릴 때 가려서
// m으로 풀 수 있게 한다. 파이프와 --raw 출력은 이 범위를 쓰지 않는다.
func traceScreenScope(protocol string, options tcpTraceOptions) traceScope {
	scope := options.scope(protocol)
	switch protocol {
	case "http":
		scope.progressive = true
		if !options.payloadExplicit {
			scope.payload, scope.payloadAll = true, true
		}
		scope.showSecrets, scope.keepGzip = scope.payload, scope.payload
	case "socket":
		// 6.4 전 kernel은 사용자 버퍼를 읽을 field가 없다. 사용자가 고르지 않은 payload 때문에 화면이 멈추지 않게 byte 수만 모은다.
		scope.payload = scope.payload || socketPayloadSupported()
	}
	return scope
}

func runTraceScreen(protocol string, options tcpTraceOptions) int {
	if err := traceProtocolPrerequisites(protocol); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(traceScreenGCPercent)
	}
	eventCh := make(chan captureEvent, 256)
	resultCh := make(chan traceFinishedMsg, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopCapture := func() { stopOnce.Do(func() { close(stop) }) }
	started := time.Now()
	// 끝날 때 어느 보기일지 모르므로 모든 보기의 요약을 쌓는다. 수집 goroutine만 쓰고, resultCh를 받은 뒤에 읽는다.
	aggregate := newTraceAggregate(protocol, traceGroupViews(protocol)...)
	go func() {
		summary, err := collectTraceEventsLive(traceScreenScope(protocol, options), options.duration, func(event captureEvent) error {
			if event.TLSNotice == "" && (traceProtocol(event) != protocol || !options.matches(event)) {
				return nil
			}
			if event.TLSNotice == "" {
				aggregate.observe(event)
			}
			select {
			case eventCh <- event:
				return nil
			case <-stop:
				return nil
			}
		}, stop)
		resultCh <- traceFinishedMsg{summary: summary, err: err}
	}()
	model := newTraceScreenModel(protocol, options, eventCh, resultCh, stopCapture)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// bubbletea의 signal 처리기는 SIGINT를 중단 오류로, SIGTERM을 즉시 종료로 끝내서 수집 결과를 받기 전에
	// 화면이 닫혔다. 위의 NotifyContext가 대신 수집을 멈추고, 화면은 결과를 받은 뒤 요약과 함께 끝난다.
	program := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithoutSignalHandler())
	go func() {
		<-ctx.Done()
		stopCapture()
		program.Send(traceStopMsg{})
	}()
	finalModel, err := program.Run()
	if err != nil {
		stopCapture()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	screen, ok := finalModel.(traceScreenModel)
	if !ok || screen.finished == nil {
		stopCapture()
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", "trace screen ended without a result"))
		return 1
	}
	result := *screen.finished
	if result.err != nil {
		fmt.Fprintln(os.Stderr, T("cli.trace.failed", result.err))
		return 1
	}
	printTraceTLSExecProblem(result.summary)
	duration := options.duration
	if duration == 0 || screen.stopping || ctx.Err() != nil {
		duration = time.Since(started)
	}
	if screen.groupBy != "" {
		printTraceGroupReport(aggregate.groups[screen.groupBy].report(result.summary, duration))
		return 0
	}
	aggregate.summary.summarize(result.summary, duration).print(options.detail)
	return 0
}
