package edc

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package edc -type socket_record socketEvents socket_events_bpf.c -- -I./bpf

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// traceSocketUsage는 trace socket의 사용법이다. option은 경로 앞이나 뒤에 쓴다.
const traceSocketUsage = "edc trace socket <path> [options]"

// socketPayloadLimit는 호출마다 받는 payload byte 수다. --payload는 앞 4KiB, --payload=all은 1MiB까지다.
func socketPayloadLimit(scope traceScope) int {
	switch {
	case scope.payloadAll:
		return httpMessageMax
	case scope.payload:
		return httpPayloadHead
	}
	return 0
}

// socketRecord의 offset은 socket_events_bpf.c의 struct socket_record와 같다.
const (
	socketRecordPayloadOffset = 88
	// socketRecordPayloadMax는 SOCKET_PAYLOAD_SIZE로, 레코드 하나에 담기는 byte 수다.
	socketRecordPayloadMax = 16384
	socketRecordConnect    = 1
	socketRecordSend       = 2
	socketRecordRecv       = 3
	socketRecordClose      = 4
	socketRecordAccept     = 5
	socketRecordServer     = 1
	socketRecordUnreadable = 1
	// socketCallIdle은 조각을 잃은 호출을 기다리는 시간이다. 한 호출의 조각은 BPF가 한 번에 넘기므로 금방 온다.
	socketCallIdle = time.Second
)

type socketRecord struct {
	bootTimeNS uint64
	socket     uint64
	peer       uint64
	cgroupID   uint64
	result     int64
	waitNS     uint64
	eventType  uint32
	pid        uint32
	peerPID    uint32
	offset     uint32
	server     bool
	unreadable bool
	process    string
	payload    []byte
}

func parseSocketRecord(sample []byte) (socketRecord, bool) {
	if len(sample) < socketRecordPayloadOffset {
		return socketRecord{}, false
	}
	record := socketRecord{
		bootTimeNS: binary.LittleEndian.Uint64(sample[0:8]),
		socket:     binary.LittleEndian.Uint64(sample[8:16]),
		peer:       binary.LittleEndian.Uint64(sample[16:24]),
		cgroupID:   binary.LittleEndian.Uint64(sample[24:32]),
		result:     int64(binary.LittleEndian.Uint64(sample[32:40])),
		waitNS:     binary.LittleEndian.Uint64(sample[40:48]),
		eventType:  binary.LittleEndian.Uint32(sample[48:52]),
		pid:        binary.LittleEndian.Uint32(sample[52:56]),
		peerPID:    binary.LittleEndian.Uint32(sample[56:60]),
		offset:     binary.LittleEndian.Uint32(sample[64:68]),
		server:     sample[68] == socketRecordServer,
		unreadable: sample[69]&socketRecordUnreadable != 0,
		process:    strings.TrimRight(string(sample[72:88]), "\x00"),
		payload:    sample[socketRecordPayloadOffset:],
	}
	if size := int(binary.LittleEndian.Uint32(sample[60:64])); size < len(record.payload) {
		record.payload = record.payload[:size]
	}
	return record, true
}

// socketCall은 조각을 합친 호출 하나다. truncated는 payload를 끝까지 받지 못한 호출이다. 조각을 잃었거나 읽지 못했다.
type socketCall struct {
	record    socketRecord
	payload   []byte
	truncated bool
}

type socketCallKey struct {
	socket     uint64
	bootTimeNS uint64
	eventType  uint32
}

type socketOpenCall struct {
	socketCall
	want int
	seen time.Time
}

// socketCalls는 한 호출의 payload 조각을 합친다. limit는 호출마다 받는 payload byte 수이고, 0이면 payload를 받지 않는다.
type socketCalls struct {
	limit int
	open  map[socketCallKey]*socketOpenCall
}

func newSocketCalls(limit int) *socketCalls {
	return &socketCalls{limit: limit, open: map[socketCallKey]*socketOpenCall{}}
}

func (calls *socketCalls) add(record socketRecord, now time.Time) []socketCall {
	want := 0
	if calls.limit > 0 && record.result > 0 {
		want = int(min(record.result, int64(calls.limit)))
	}
	if want == 0 || record.unreadable {
		return []socketCall{{record: socketRecordHeader(record), truncated: want > 0}}
	}
	key := socketCallKey{socket: record.socket, bootTimeNS: record.bootTimeNS, eventType: record.eventType}
	call := calls.open[key]
	if call == nil {
		call = &socketOpenCall{socketCall: socketCall{record: socketRecordHeader(record), payload: make([]byte, 0, want)}, want: want}
		calls.open[key] = call
	}
	call.seen = now
	if int(record.offset) == len(call.payload) && !call.truncated {
		call.payload = append(call.payload, record.payload[:min(len(record.payload), want-len(call.payload))]...)
	} else {
		call.truncated = true
	}
	if len(call.payload) < want && !call.truncated {
		return nil
	}
	delete(calls.open, key)
	call.truncated = call.truncated || len(call.payload) < want
	return []socketCall{call.socketCall}
}

// expire는 idle보다 오래 조각을 기다린 호출을 잘린 호출로 끝낸다.
func (calls *socketCalls) expire(now time.Time) []socketCall {
	var done []socketCall
	for key, call := range calls.open {
		if now.Sub(call.seen) < socketCallIdle {
			continue
		}
		delete(calls.open, key)
		call.truncated = true
		done = append(done, call.socketCall)
	}
	sort.Slice(done, func(i, j int) bool { return done[i].record.bootTimeNS < done[j].record.bootTimeNS })
	return done
}

func (calls *socketCalls) flush() []socketCall {
	return calls.expire(time.Now().Add(socketCallIdle))
}

func socketRecordHeader(record socketRecord) socketRecord {
	record.payload = nil
	return record
}

// socketTraceEvent는 호출 하나를 event로 바꾼다. errorName은 음수 result의 errno 이름이고, peer는 상대 process의 이름이다.
func socketTraceEvent(call socketCall, path string, clockOffset int64, errorName, peer string) captureEvent {
	record := call.record
	event := captureEvent{
		SocketID:    record.socket,
		TimestampNS: uint64(int64(record.bootTimeNS) + clockOffset),
		BootTimeNS:  record.bootTimeNS,
		Protocol:    "socket",
		PID:         record.pid,
		Process:     record.process,
		CgroupID:    record.cgroupID,
		Destination: path,
		Side:        traceClientSide,
		PeerPID:     record.peerPID,
	}
	if record.server {
		event.Side = traceServerSide
	}
	if record.peerPID != 0 {
		event.Source = fmt.Sprintf("%s[%d]", emptyAs(peer, "?"), record.peerPID)
	}
	if record.result < 0 {
		event.Error = errorName
	}
	switch record.eventType {
	case socketRecordConnect:
		event.Event = socketEventName("socket_connect", record.result)
	case socketRecordSend:
		event.Event = socketEventName("socket_send", record.result)
	case socketRecordRecv:
		event.Event = socketEventName("socket_recv", record.result)
		if record.result == 0 {
			event.Event = "socket_eof"
		}
	case socketRecordClose:
		event.Event = "socket_close"
	case socketRecordAccept:
		event.Event = "socket_accept"
		// connect가 trace를 시작하기 전이면 시각을 몰라 0이다.
		if record.waitNS > 0 {
			wait := float64(record.waitNS) / float64(time.Millisecond)
			event.LatencyMS = &wait
		}
	}
	if record.result > 0 && (record.eventType == socketRecordSend || record.eventType == socketRecordRecv) {
		event.Bytes = uint64(record.result)
	}
	if len(call.payload) > 0 || call.truncated {
		event.Payload = traceEscapeText(call.payload)
		event.PayloadTruncated = call.truncated
	}
	return event
}

func socketEventName(name string, result int64) string {
	if result < 0 {
		return name + "_fail"
	}
	return name
}

// traceSocketScrollLabels는 목적지 칸에 socket 경로를, event 칸에 동작과 byte 수나 errno를 쓴다.
func traceSocketScrollLabels(event captureEvent) (string, string) {
	label := strings.TrimPrefix(strings.TrimSuffix(event.Event, "_fail"), "socket_")
	switch {
	case event.Error != "":
		label += " " + event.Error
	case event.Event == "socket_send" || event.Event == "socket_recv":
		label += " " + traceBytes(event.Bytes)
	case event.Event == "socket_accept" && event.LatencyMS != nil:
		label += " " + socketWaitLabel(*event.LatencyMS)
	}
	return emptyAs(event.Destination, "-"), label
}

// socketWaitLabel은 accept 대기 시간이다. 기다리던 서버는 수 µs 안에 accept하므로 1ms 아래는 µs로 쓴다.
func socketWaitLabel(milliseconds float64) string {
	if milliseconds < 1 {
		return fmt.Sprintf("%.0fµs", milliseconds*1000)
	}
	return fmt.Sprintf("%.1fms", milliseconds)
}

// traceSocketPayloadLine은 payload를 한 줄로 잇는다. socket의 payload는 형식을 모르므로 줄바꿈만 표시로 바꾼다.
func traceSocketPayloadLine(event captureEvent) string {
	line := "data " + strings.NewReplacer("\r\n", " ↵ ", "\n", " ↵ ", "\r", " ", "\t", " ").Replace(event.Payload)
	if event.PayloadTruncated {
		line = "[truncated] " + line
	}
	return line
}

// socketTraceRow는 process, 상대, 쪽마다 한 행이다.
type socketTraceRow struct {
	Process  string `json:"process"`
	Peer     string `json:"peer,omitempty"`
	Side     string `json:"side"`
	Events   uint64 `json:"events"`
	Connects uint64 `json:"connects"`
	Accepts  uint64 `json:"accepts"`
	Sends    uint64 `json:"sends"`
	TXBytes  uint64 `json:"tx_bytes"`
	Recvs    uint64 `json:"recvs"`
	RXBytes  uint64 `json:"rx_bytes"`
	Failures uint64 `json:"failures"`
	Closes   uint64 `json:"closes"`
}

type socketTraceReport struct {
	Path       string           `json:"path"`
	DurationMS int64            `json:"duration_ms"`
	Events     uint64           `json:"events"`
	LostEvents uint64           `json:"lost_events"`
	TXBytes    uint64           `json:"tx_bytes"`
	RXBytes    uint64           `json:"rx_bytes"`
	Failures   uint64           `json:"failures"`
	Rows       []socketTraceRow `json:"rows"`
}

type socketRowKey struct{ process, peer, side string }

// socketTraceSummarizer는 --group-by 없이 끝난 socket trace를 process, 상대, 쪽마다 한 행으로 묶는다.
type socketTraceSummarizer struct {
	path   string
	rows   map[socketRowKey]*socketTraceRow
	events uint64
}

func newSocketTraceSummarizer() *socketTraceSummarizer {
	return &socketTraceSummarizer{rows: map[socketRowKey]*socketTraceRow{}}
}

func (summarizer *socketTraceSummarizer) observe(event captureEvent) {
	summarizer.events++
	if event.Destination != "" {
		summarizer.path = event.Destination
	}
	// 상대 pid는 행을 나누지 않는다. 짧게 사는 client가 많으면 행이 연결마다 생긴다.
	peer := event.Source
	if name, _, found := strings.Cut(peer, "["); found {
		peer = name
	}
	key := socketRowKey{process: event.Process, peer: peer, side: event.Side}
	row := summarizer.rows[key]
	if row == nil {
		row = &socketTraceRow{Process: event.Process, Peer: peer, Side: event.Side}
		summarizer.rows[key] = row
	}
	row.Events++
	switch event.Event {
	case "socket_connect":
		row.Connects++
	case "socket_accept":
		row.Accepts++
	case "socket_send":
		row.Sends++
		row.TXBytes += event.Bytes
	case "socket_recv":
		row.Recvs++
		row.RXBytes += event.Bytes
	case "socket_close":
		row.Closes++
	}
	if strings.HasSuffix(event.Event, "_fail") {
		row.Failures++
	}
}

func (summarizer *socketTraceSummarizer) summarize(summary captureSummary, duration time.Duration) traceReport {
	report := socketTraceReport{Path: summarizer.path, DurationMS: duration.Milliseconds(), Events: summarizer.events, LostEvents: summary.LostEvents, Rows: make([]socketTraceRow, 0, len(summarizer.rows))}
	for _, row := range summarizer.rows {
		report.TXBytes += row.TXBytes
		report.RXBytes += row.RXBytes
		report.Failures += row.Failures
		report.Rows = append(report.Rows, *row)
	}
	// 실패한 행을 먼저, 그다음 많이 주고받은 행을 둔다.
	sort.Slice(report.Rows, func(i, j int) bool {
		left, right := report.Rows[i], report.Rows[j]
		if left.Failures != right.Failures {
			return left.Failures > right.Failures
		}
		if left.TXBytes+left.RXBytes != right.TXBytes+right.RXBytes {
			return left.TXBytes+left.RXBytes > right.TXBytes+right.RXBytes
		}
		if left.Process != right.Process {
			return left.Process < right.Process
		}
		if left.Peer != right.Peer {
			return left.Peer < right.Peer
		}
		return left.Side < right.Side
	})
	return report
}

// -d는 연결마다 한 행을 쓰는 option이다. socket 요약은 이미 process와 상대마다 한 행이라 같은 표를 쓴다.
func (report socketTraceReport) print(bool) {
	fmt.Fprintf(os.Stdout, "SOCKET trace: %s\n\n", (time.Duration(report.DurationMS) * time.Millisecond).String())
	fmt.Fprintf(os.Stdout, "Path: %s\nEvents: %d\nSent: %s\nReceived: %s\nFailures: %d\nLost events: %d\n", emptyAs(report.Path, "-"), report.Events, traceBytes(report.TXBytes), traceBytes(report.RXBytes), report.Failures, report.LostEvents)
	if len(report.Rows) == 0 {
		return
	}
	fmt.Fprintln(os.Stdout, "\nPROCESS\tPEER\tSIDE\tEVENTS\tCONNECT\tACCEPT\tSEND\tSENT\tRECV\tRECEIVED\tFAIL\tCLOSE")
	for _, row := range report.Rows {
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%d\t%s\t%d\t%d\n", emptyAs(row.Process, "-"), emptyAs(row.Peer, "-"), row.Side, row.Events, row.Connects, row.Accepts, row.Sends, traceBytes(row.TXBytes), row.Recvs, traceBytes(row.RXBytes), row.Failures, row.Closes)
	}
}
