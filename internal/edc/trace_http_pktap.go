package edc

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"net/netip"
	"time"
)

const pktapHTTPFlowLimit = 4096
const pktapHTTPBufferLimit = 64 << 20

type pktapTCPPacket struct {
	tlsObject uint64
	httpPacket
	sequence uint32
	flags    byte
}

// PKTAP v1 and v2 share the flags offset. Layout: xnu bsd/net/pktap.h.
func decodeHTTPPKTAP(data []byte, wallNS uint64, clockOffset int64) (pktapTCPPacket, bool) {
	var result pktapTCPPacket
	if len(data) < 40 {
		return result, false
	}
	flags := binary.LittleEndian.Uint32(data[36:])
	var length, pre, post int
	var family, pid uint32
	var name []byte
	if flags&0x80000 != 0 {
		length = int(data[0])
		if length < 40 || length > len(data) {
			return result, false
		}
		pre, post = int(binary.LittleEndian.Uint16(data[8:])), int(binary.LittleEndian.Uint16(data[10:]))
		family, pid = binary.LittleEndian.Uint32(data[16:]), binary.LittleEndian.Uint32(data[28:])
		at := int(data[4])
		if at != 0 {
			if at < 40 || at >= length {
				return result, false
			}
			name = data[at:length]
		}
	} else {
		length = int(binary.LittleEndian.Uint32(data))
		if length < 80 || length > len(data) || binary.LittleEndian.Uint32(data[4:]) != 1 {
			return result, false
		}
		pre, post = int(binary.LittleEndian.Uint32(data[44:])), int(binary.LittleEndian.Uint32(data[48:]))
		family, pid = binary.LittleEndian.Uint32(data[40:]), binary.LittleEndian.Uint32(data[52:])
		name = data[56:73]
	}
	if pre > len(data)-length || post > len(data)-length-pre || flags&3 == 0 {
		return result, false
	}
	if at := bytes.IndexByte(name, 0); at >= 0 {
		name = name[:at]
	}
	ip := data[length+pre : len(data)-post]
	var source, destination netip.Addr
	var tcp []byte
	switch family {
	case 2:
		if len(ip) < 20 || ip[0]>>4 != 4 || ip[9] != 6 || binary.BigEndian.Uint16(ip[6:])&0x3fff != 0 {
			return result, false
		}
		head, size := int(ip[0]&15)*4, int(binary.BigEndian.Uint16(ip[2:]))
		if head < 20 || size < head || size > len(ip) {
			return result, false
		}
		source, destination = netip.AddrFrom4([4]byte(ip[12:16])), netip.AddrFrom4([4]byte(ip[16:20]))
		tcp = ip[head:size]
	case 30:
		if len(ip) < 40 || ip[0]>>4 != 6 {
			return result, false
		}
		size := 40 + int(binary.BigEndian.Uint16(ip[4:]))
		if size > len(ip) {
			return result, false
		}
		source, destination = netip.AddrFrom16([16]byte(ip[8:24])), netip.AddrFrom16([16]byte(ip[24:40]))
		next, at := ip[6], 40
		for next != 6 {
			if at+2 > size {
				return result, false
			}
			var n int
			switch next {
			case 0, 43, 60:
				n = (int(ip[at+1]) + 1) * 8
			case 51:
				n = (int(ip[at+1]) + 2) * 4
			default:
				return result, false
			}
			if at+n > size {
				return result, false
			}
			next, at = ip[at], at+n
		}
		tcp = ip[at:size]
	default:
		return result, false
	}
	if len(tcp) < 20 {
		return result, false
	}
	head := int(tcp[12]>>4) * 4
	if head < 20 || head > len(tcp) {
		return result, false
	}
	result.sent = flags&2 != 0
	local, remote := netip.AddrPortFrom(source.Unmap(), binary.BigEndian.Uint16(tcp)), netip.AddrPortFrom(destination.Unmap(), binary.BigEndian.Uint16(tcp[2:]))
	if !result.sent {
		local, remote = remote, local
	}
	if int32(pid) < 0 {
		pid = 0
	}
	result.httpPacket = httpPacket{bootTimeNS: uint64(int64(wallNS) - clockOffset), pid: pid, process: string(name), sent: result.sent, source: local.String(), destination: remote.String(), payload: tcp[head:]}
	result.sequence, result.flags = binary.BigEndian.Uint32(tcp[4:]), tcp[13]
	return result, true
}

type pktapHTTPKey struct {
	local, remote string
	pid           uint32
	tlsObject     uint64
}
type pktapHTTPDirection struct {
	mysqlWire                    *mysqlStream
	started, closed, blocked, h2 bool
	next, offset                 uint32
	buffer                       []byte
	packet                       httpPacket
	waiting                      map[uint32]pktapTCPPacket
	waitingBytes                 int
}
type pktapHTTPFlow struct {
	id         uint64
	updated    time.Time
	directions [2]pktapHTTPDirection
}
type pktapHTTPTracker struct {
	mysql        *mysqlTracker
	mysqlPort    uint16
	tls          *tlsKeylogDecryptor
	tracker      *httpTracker
	messages     *httpMessages
	flows        map[pktapHTTPKey]*pktapHTTPFlow
	nextID, lost uint64
	clockOffset  int64
	buffered     int
}

func newPKTAPHTTPTracker(scope traceScope, offset int64) *pktapHTTPTracker {
	tracker := newHTTPTracker(scope.side, scope.payload && !scope.payloadAll, scope.showSecrets)
	tracker.keepGzip, tracker.progressive = scope.keepGzip, scope.progressive
	tracker.h2PayloadLimit = traceHTTP2PayloadLimit(scope)
	result := &pktapHTTPTracker{tracker: tracker, flows: map[pktapHTTPKey]*pktapHTTPFlow{}, clockOffset: offset}
	if scope.protocol == "mysql" {
		result.mysql = newMySQLTracker(scope.side, scope.showSecrets)
		result.mysqlPort = cmp.Or(scope.port, traceMySQLDefaultPort)
	}
	if scope.payloadAll {
		result.messages = newHTTPMessages(tracker, httpMessageMax, scope.showSecrets)
		result.messages.progressive = scope.progressive
		tracker.http1Messages = result.messages
	}
	return result
}
func (t *pktapHTTPTracker) emitPacket(packet httpPacket, now time.Time) []captureEvent {
	if t.messages != nil {
		return t.messages.add(packet, t.clockOffset, now)
	}
	if event, ok := t.tracker.event(packet, t.clockOffset); ok {
		return []captureEvent{event}
	}
	return nil
}
func (t *pktapHTTPTracker) drain(d *pktapHTTPDirection, final bool, now time.Time) []captureEvent {
	before := len(d.buffer)
	defer func() { t.buffered += len(d.buffer) - before }()
	if t.mysql != nil {
		if len(d.buffer) == 0 {
			return nil
		}
		if d.mysqlWire == nil {
			d.mysqlWire = &mysqlStream{}
		}
		packet := d.packet
		packet.payload = d.buffer
		events := t.mysqlEvents(packet, d.mysqlWire)
		d.buffer = nil
		return events
	}
	var events []captureEvent
	for len(d.buffer) > 0 {
		packet := d.packet
		packet.payload = d.buffer
		_, peerH2 := t.tracker.http2[httpStreamKey{socket: packet.socket, sent: packet.sent}]
		if d.h2 || peerH2 || bytes.HasPrefix(d.buffer, http2Preface) {
			packet.offset, packet.continued = d.offset, d.h2 || peerH2
			found, _ := t.tracker.http2Events(packet, t.clockOffset)
			events = append(events, found...)
			d.h2 = true
			d.offset += uint32(len(d.buffer))
			d.buffer = nil
			break
		}
		if !final && len(d.buffer) < len(http2Preface) && bytes.HasPrefix(http2Preface, d.buffer) {
			break
		}
		if !pktapPossibleHTTP(d.buffer) {
			d.buffer = nil
			d.blocked = true
			break
		}
		size := 0
		if tlsRecordHeaderStart(d.buffer) {
			if len(d.buffer) < 5 {
				break
			}
			size = 5 + int(binary.BigEndian.Uint16(d.buffer[3:]))
		} else {
			if !bytes.Contains(d.buffer, []byte("\r\n\r\n")) {
				if !final {
					break
				}
				d.buffer = nil
				break
			}
			method, _, _, request := parseHTTPRequest(d.buffer)
			status, response := parseHTTPStatus(d.buffer)
			if !request && !response {
				d.buffer = nil
				d.blocked = true
				break
			}
			if response {
				if queue := t.tracker.pending[packet.socket]; len(queue) > 0 {
					method = queue[0].method
				}
			}
			end, known := httpMessageEnd(d.buffer, captureEvent{Method: method, Status: status})
			if !known {
				if !final {
					break
				}
				end = len(d.buffer)
			}
			size = end
		}
		if size > len(d.buffer) {
			if !final {
				break
			}
			size = len(d.buffer)
		}
		packet.payload = bytes.Clone(d.buffer[:size])
		events = append(events, t.emitPacket(packet, now)...)
		if t.tls != nil && !packet.decrypted && tlsRecordHeaderStart(packet.payload) {
			events = append(events, t.tls.record(packet, now)...)
		}
		d.buffer = d.buffer[size:]
		if len(d.buffer) == 0 {
			d.buffer = nil
		}
	}
	return events
}
func (t *pktapHTTPTracker) closeFlow(key pktapHTTPKey, flow *pktapHTTPFlow, now time.Time) []captureEvent {
	if t.flows[key] != flow {
		return nil
	}
	delete(t.flows, key)
	var events []captureEvent
	for i := range flow.directions {
		events = append(events, t.drain(&flow.directions[i], true, now)...)
		t.buffered -= len(flow.directions[i].buffer) + flow.directions[i].waitingBytes
		if len(flow.directions[i].waiting) > 0 {
			t.lost++
		}
	}
	if t.tls != nil {
		events = append(events, t.tls.forget(flow.id, now)...)
	}
	if t.messages != nil {
		events = append(events, t.messages.forget(flow.id)...)
	}
	if t.mysql != nil {
		t.mysql.forgetSocket(flow.id)
	}
	events = append(events, t.tracker.forget(flow.id)...)
	delete(t.flows, key)
	return events
}
func (t *pktapHTTPTracker) add(packet pktapTCPPacket, now time.Time) []captureEvent {
	key := pktapHTTPKey{local: packet.source, remote: packet.destination, pid: packet.pid, tlsObject: packet.tlsObject}
	if packet.tlsObject != 0 {
		key.local, key.remote = "", ""
	}
	flow := t.flows[key]
	var events []captureEvent
	if flow == nil {
		if len(packet.payload) == 0 && packet.flags&2 == 0 {
			return nil
		}
		if len(t.flows) >= pktapHTTPFlowLimit {
			var oldest pktapHTTPKey
			var selected *pktapHTTPFlow
			for k, f := range t.flows {
				if selected == nil || f.updated.Before(selected.updated) {
					oldest, selected = k, f
				}
			}
			events = append(events, t.closeFlow(oldest, selected, now)...)
			t.lost++
		}
		t.nextID++
		flow = &pktapHTTPFlow{id: t.nextID}
		t.flows[key] = flow
	}
	if packet.flags&0x12 == 2 {
		index := 0
		if packet.sent {
			index = 1
		}
		d := &flow.directions[index]
		if d.started && d.next != packet.sequence+1 {
			events = append(events, t.closeFlow(key, flow, now)...)
			t.nextID++
			flow = &pktapHTTPFlow{id: t.nextID}
			t.flows[key] = flow
		}
	}
	flow.updated = now
	index := 0
	if packet.sent {
		index = 1
	}
	d := &flow.directions[index]
	packet.socket = flow.id
	if packet.flags&2 != 0 {
		packet.sequence++
	}
	if !d.started {
		d.started = true
		d.next = packet.sequence
	}
	if packet.flags&4 != 0 {
		return append(events, t.closeFlow(key, flow, now)...)
	}
	if d.blocked {
		return events
	}
	if len(packet.payload) == 0 && packet.flags&7 == 0 {
		return events
	}
	if gap := int32(packet.sequence - d.next); gap > 0 {
		if len(d.waiting) >= 64 || d.waitingBytes+len(packet.payload) > httpMessageMax || t.buffered+len(packet.payload) > pktapHTTPBufferLimit {
			t.buffered -= d.waitingBytes + len(d.buffer)
			d.buffer = nil
			d.blocked = true
			d.waiting = nil
			d.waitingBytes = 0
			t.lost++
			return events
		}
		if d.waiting == nil {
			d.waiting = map[uint32]pktapTCPPacket{}
		}
		if _, ok := d.waiting[packet.sequence]; !ok {
			packet.payload = bytes.Clone(packet.payload)
			d.waiting[packet.sequence] = packet
			d.waitingBytes += len(packet.payload)
			t.buffered += len(packet.payload)
		}
		return events
	}
	for {
		overlap := int32(d.next - packet.sequence)
		if overlap >= 0 && int(overlap) < len(packet.payload) {
			payload := packet.payload[overlap:]
			if len(d.buffer) == 0 {
				d.packet = packet.httpPacket
				d.packet.payload = nil
			}
			d.next += uint32(len(payload))
			if len(d.buffer)+len(payload) > httpMessageMax || t.buffered+len(payload) > pktapHTTPBufferLimit {
				t.buffered -= len(d.buffer) + d.waitingBytes
				d.buffer = nil
				d.waiting = nil
				d.waitingBytes = 0
				d.blocked = true
				t.lost++
				break
			}
			d.buffer = append(d.buffer, payload...)
			t.buffered += len(payload)
			events = append(events, t.drain(d, false, now)...)
		}
		if packet.flags&5 != 0 {
			d.closed = true
			events = append(events, t.drain(d, true, now)...)
			if t.messages != nil {
				events = append(events, t.messages.finish(httpStreamKey{socket: flow.id, sent: packet.sent}, false)...)
			}
			if packet.flags&4 != 0 || flow.directions[1-index].closed {
				events = append(events, t.closeFlow(key, flow, now)...)
				return events
			}
		}
		var waiting pktapTCPPacket
		found := false
		for sequence, candidate := range d.waiting {
			if int32(sequence-d.next) <= 0 {
				delete(d.waiting, sequence)
				if int32(sequence+uint32(len(candidate.payload))-d.next) > 0 || candidate.flags&5 != 0 {
					waiting = candidate
					found = true
					break
				}
				d.waitingBytes -= len(candidate.payload)
				t.buffered -= len(candidate.payload)
			}
		}
		if !found {
			break
		}
		d.waitingBytes -= len(waiting.payload)
		t.buffered -= len(waiting.payload)
		packet = waiting
	}
	return events
}
func (t *pktapHTTPTracker) sweep(now time.Time, final bool) []captureEvent {
	var events []captureEvent
	for key, flow := range t.flows {
		if t.flows[key] != flow {
			continue
		}
		if final || now.Sub(flow.updated) > time.Minute {
			events = append(events, t.closeFlow(key, flow, now)...)
		}
	}
	if t.messages != nil {
		events = append(events, t.messages.expire(now)...)
	}
	events = append(events, t.tracker.expireHTTP2Payloads(now)...)
	return events
}

func pktapHTTPPortMatches(packet pktapTCPPacket, port uint16) bool {
	local, err := netip.ParseAddrPort(packet.source)
	if err != nil {
		return false
	}
	remote, err := netip.ParseAddrPort(packet.destination)
	return err == nil && (local.Port() == port || remote.Port() == port)
}

func pktapPossibleHTTP(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, prefix := range []string{"GET ", "POST ", "PUT ", "HEAD ", "DELETE ", "PATCH ", "OPTIONS ", "CONNECT ", "TRACE ", "HTTP/1."} {
		if bytes.HasPrefix(data, []byte(prefix)) || bytes.HasPrefix([]byte(prefix), data) {
			return true
		}
	}
	return data[0] >= 0x14 && data[0] <= 0x17
}
