package edc

import (
	"bytes"
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type httpStreamKey struct {
	socket uint64
	sent   bool
}

// httpOpenMessage는 끝나기를 기다리는 message다. next는 다음 조각이 message 안에서 시작해야 하는 위치다.
type httpOpenMessage struct {
	event   captureEvent
	data    []byte
	next    uint32
	updated time.Time
	// interim은 이 요청이 끝나기 전에 온 1xx 응답이다. 요청 event 뒤에 낸다.
	interim []captureEvent
}

// httpMessages는 --payload=all에서 조각 레코드를 message 하나로 잇는다. 요청과 응답 event는 message가 끝날 때까지
// 두었다가 낸다. 응답 시간은 tracker가 첫 조각의 시각으로 재므로, 늦게 내도 바뀌지 않는다.
type httpMessages struct {
	tracker     *httpTracker
	limit       int
	showSecrets bool
	open        map[httpStreamKey]*httpOpenMessage
	bytes       int
}

func newHTTPMessages(tracker *httpTracker, limit int, showSecrets bool) *httpMessages {
	return &httpMessages{tracker: tracker, limit: limit, showSecrets: showSecrets, open: map[httpStreamKey]*httpOpenMessage{}}
}

// add는 레코드 하나를 받아, 끝난 message의 event를 끝난 순서대로 돌려준다.
func (messages *httpMessages) add(packet httpPacket, clockOffset int64, now time.Time) []captureEvent {
	key := httpStreamKey{socket: packet.socket, sent: packet.sent}
	if packet.continued {
		message := messages.open[key]
		if message == nil {
			return nil
		}
		// 앞 조각을 잃었으면 구멍을 건너뛰어 잇지 않는다. 받은 데까지만 잘린 message로 낸다.
		if packet.offset != message.next {
			return messages.finish(key, true)
		}
		messages.append(message, packet.payload)
		message.next += uint32(len(packet.payload))
		message.updated = now
		return messages.finishIfDone(key)
	}
	// 같은 쪽에서 새 message가 시작되면 앞 message는 끝났다.
	done := messages.finish(key, false)
	event, ok := messages.tracker.event(packet, clockOffset)
	if !ok {
		return done
	}
	// 반대쪽 message도 끝내서 요청 event가 응답 event보다 먼저 나오게 한다. 1xx는 중간 응답이라, 요청 본문이 아직
	// 오는 중일 수 있다(Expect: 100-continue).
	if event.Status < 100 || event.Status >= 200 {
		done = append(done, messages.finish(httpStreamKey{socket: packet.socket, sent: !packet.sent}, false)...)
	}
	// 첫 조각은 복사하지 않고 레코드를 그대로 쓴다. 이어지는 조각이 오면 append가 새 배열로 옮긴다.
	data := slices.Clip(packet.payload[:min(len(packet.payload), messages.limit)])
	message := &httpOpenMessage{event: event, data: data, next: uint32(len(packet.payload)), updated: now}
	messages.open[key] = message
	messages.bytes += len(data)
	finished := messages.finishIfDone(key)
	// 요청 본문이 아직 오는 중에 온 1xx는 요청 event 뒤로 미뤄서, 요청이 응답보다 먼저 나오게 한다.
	if request := messages.open[httpStreamKey{socket: packet.socket, sent: !packet.sent}]; request != nil && event.Status >= 100 && event.Status < 200 {
		request.interim = append(request.interim, finished...)
		finished = nil
	}
	done = append(done, finished...)
	return append(done, messages.trim()...)
}

func (messages *httpMessages) append(message *httpOpenMessage, chunk []byte) {
	chunk = chunk[:min(len(chunk), max(0, messages.limit-len(message.data)))]
	message.data = append(message.data, chunk...)
	messages.bytes += len(chunk)
}

// expire는 httpMessageIdle 동안 조각이 오지 않은 message를 낸다.
func (messages *httpMessages) expire(now time.Time) []captureEvent {
	var done []captureEvent
	for _, key := range messages.oldest() {
		if now.Sub(messages.open[key].updated) >= httpMessageIdle {
			done = append(done, messages.finish(key, false)...)
		}
	}
	return done
}

// flush는 trace를 끝낼 때 남은 message를 모두 낸다.
func (messages *httpMessages) flush() []captureEvent {
	var done []captureEvent
	for _, key := range messages.oldest() {
		done = append(done, messages.finish(key, false)...)
	}
	return done
}

// trim은 기다리는 message가 합계 상한을 넘으면 오래된 것부터 낸다.
func (messages *httpMessages) trim() []captureEvent {
	if len(messages.open) <= httpMessageOpenLimit && messages.bytes <= httpMessageOpenBytes {
		return nil
	}
	var done []captureEvent
	for _, key := range messages.oldest() {
		if len(messages.open) <= httpMessageOpenLimit && messages.bytes <= httpMessageOpenBytes {
			break
		}
		done = append(done, messages.finish(key, false)...)
	}
	return done
}

func (messages *httpMessages) oldest() []httpStreamKey {
	keys := make([]httpStreamKey, 0, len(messages.open))
	for key := range messages.open {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right httpStreamKey) int {
		return cmp.Compare(messages.open[left].event.BootTimeNS, messages.open[right].event.BootTimeNS)
	})
	return keys
}

func (messages *httpMessages) finishIfDone(key httpStreamKey) []captureEvent {
	message := messages.open[key]
	end, known := httpMessageEnd(message.data, message.event)
	if (known && end <= len(message.data)) || len(message.data) >= messages.limit {
		return messages.finishAt(key, false, end, known)
	}
	return nil
}

// finish는 message를 event로 낸다. 길이를 아는 message가 모자라거나, 상한에서 잘렸거나, 조각을 잃었으면 잘린 것으로 표시한다.
func (messages *httpMessages) finish(key httpStreamKey, lost bool) []captureEvent {
	message := messages.open[key]
	if message == nil {
		return nil
	}
	end, known := httpMessageEnd(message.data, message.event)
	return messages.finishAt(key, lost, end, known)
}

func (messages *httpMessages) finishAt(key httpStreamKey, lost bool, end int, known bool) []captureEvent {
	message := messages.open[key]
	delete(messages.open, key)
	messages.bytes -= len(message.data)
	data := message.data
	truncated := lost || (known && end > len(data)) || (!known && len(data) >= messages.limit)
	if known && end < len(data) {
		data = data[:end]
	}
	event := message.event
	event.Payload = traceHTTPPayload(data, messages.showSecrets)
	event.PayloadTruncated = truncated
	event.Bytes = uint64(len(data))
	return append([]captureEvent{event}, message.interim...)
}

// httpMessageEnd는 message가 끝나는 위치다. known이 false면 연결이 닫혀야 끝나는 응답이라 끝을 알 수 없다. header가 아직
// 끝나지 않았거나 chunked 본문이 아직 끝나지 않았으면 end는 data보다 길다.
func httpMessageEnd(data []byte, event captureEvent) (end int, known bool) {
	head := bytes.Index(data, []byte("\r\n\r\n"))
	if head < 0 {
		return len(data) + 1, true
	}
	bodyStart := head + 4
	response := event.Status != 0
	// 1xx, 204, 304 응답과 HEAD의 응답에는 Content-Length가 있어도 본문이 없다.
	if response && (event.Status < 200 || event.Status == 204 || event.Status == 304 || event.Method == "HEAD") {
		return bodyStart, true
	}
	chunked, length, hasLength := false, 0, false
	// 첫 줄은 요청 줄이나 상태 줄이라 건너뛴다. event마다 불리므로 줄을 slice로 나누지 않고 차례로 본다.
	_, headers, _ := bytes.Cut(data[:head], []byte("\r\n"))
	for len(headers) > 0 {
		line, rest, _ := bytes.Cut(headers, []byte("\r\n"))
		headers = rest
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		name = bytes.TrimSpace(name)
		switch {
		case bytes.EqualFold(name, []byte("transfer-encoding")):
			chunked = chunked || bytes.Contains(bytes.ToLower(value), []byte("chunked"))
		case bytes.EqualFold(name, []byte("content-length")):
			if parsed, err := strconv.Atoi(string(bytes.TrimSpace(value))); err == nil && parsed >= 0 {
				length, hasLength = parsed, true
			}
		}
	}
	switch {
	case chunked:
		return httpChunkedEnd(data, bodyStart)
	case hasLength:
		return bodyStart + length, true
	case response:
		return 0, false
	}
	return bodyStart, true
}

// httpChunkedEnd는 chunked 본문의 끝이다. 크기 줄을 읽을 수 없으면 끝을 알 수 없는 message로 본다.
func httpChunkedEnd(data []byte, at int) (int, bool) {
	for {
		line := bytes.Index(data[at:], []byte("\r\n"))
		if line < 0 {
			return len(data) + 1, true
		}
		sizeText, _, _ := strings.Cut(string(data[at:at+line]), ";")
		size, err := strconv.ParseUint(strings.TrimSpace(sizeText), 16, 32)
		if err != nil {
			return 0, false
		}
		at += line + 2
		if size == 0 {
			// 마지막 chunk 뒤에는 trailer header가 오고 빈 줄로 끝난다.
			for {
				trailer := bytes.Index(data[at:], []byte("\r\n"))
				if trailer < 0 {
					return len(data) + 1, true
				}
				at += trailer + 2
				if trailer == 0 {
					return at, true
				}
			}
		}
		if len(data)-at < int(size)+2 {
			return len(data) + 1, true
		}
		at += int(size) + 2
	}
}
