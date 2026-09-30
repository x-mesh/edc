package edc

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestSocketRecordOffsetsMatchTheBPFStruct(t *testing.T) {
	var record socketEventsSocketRecord
	for name, offsets := range map[string][2]uintptr{
		"skaddr":      {unsafe.Offsetof(record.Skaddr), 8},
		"peer_skaddr": {unsafe.Offsetof(record.PeerSkaddr), 16},
		"cgroup_id":   {unsafe.Offsetof(record.CgroupId), 24},
		"result":      {unsafe.Offsetof(record.Result), 32},
		"event_type":  {unsafe.Offsetof(record.EventType), 40},
		"pid":         {unsafe.Offsetof(record.Pid), 44},
		"peer_pid":    {unsafe.Offsetof(record.PeerPid), 48},
		"len":         {unsafe.Offsetof(record.Len), 52},
		"offset":      {unsafe.Offsetof(record.Offset), 56},
		"side":        {unsafe.Offsetof(record.Side), 60},
		"flags":       {unsafe.Offsetof(record.Flags), 61},
		"comm":        {unsafe.Offsetof(record.Comm), 64},
		"payload":     {unsafe.Offsetof(record.Payload), socketRecordPayloadOffset},
	} {
		if offsets[0] != offsets[1] {
			t.Fatalf("%s is at %d in the BPF struct, parseSocketRecord reads %d", name, offsets[0], offsets[1])
		}
	}
	if len(record.Payload) != socketRecordPayloadMax {
		t.Fatalf("BPF SOCKET_PAYLOAD_SIZE is %d, the parser expects %d", len(record.Payload), socketRecordPayloadMax)
	}
}

func socketSample(eventType uint32, result int64, offset uint32, server bool, flags byte, payload string) []byte {
	sample := make([]byte, socketRecordPayloadOffset+len(payload))
	binary.LittleEndian.PutUint64(sample[0:8], 1000)
	binary.LittleEndian.PutUint64(sample[8:16], 0xabc)
	binary.LittleEndian.PutUint64(sample[16:24], 0xdef)
	binary.LittleEndian.PutUint64(sample[32:40], uint64(result))
	binary.LittleEndian.PutUint32(sample[40:44], eventType)
	binary.LittleEndian.PutUint32(sample[44:48], 42)
	binary.LittleEndian.PutUint32(sample[48:52], 7)
	binary.LittleEndian.PutUint32(sample[52:56], uint32(len(payload)))
	binary.LittleEndian.PutUint32(sample[56:60], offset)
	if server {
		sample[60] = socketRecordServer
	}
	sample[61] = flags
	copy(sample[64:80], "dockerd")
	copy(sample[socketRecordPayloadOffset:], payload)
	return sample
}

func TestParseSocketRecordReadsTheHeaderAndPayload(t *testing.T) {
	record, ok := parseSocketRecord(socketSample(socketRecordSend, 5, 0, true, 0, "hello"))
	if !ok {
		t.Fatal("record is not parsed")
	}
	if record.socket != 0xabc || record.peer != 0xdef || record.result != 5 || record.eventType != socketRecordSend || record.pid != 42 || record.peerPID != 7 || !record.server || record.unreadable || record.process != "dockerd" || string(record.payload) != "hello" {
		t.Fatalf("record = %+v", record)
	}
	record, _ = parseSocketRecord(socketSample(socketRecordConnect, -111, 0, false, 0, ""))
	if record.result != -111 || record.server {
		t.Fatalf("failed connect = %+v", record)
	}
	if _, ok := parseSocketRecord(make([]byte, socketRecordPayloadOffset-1)); ok {
		t.Fatal("a short sample must not parse")
	}
}

func socketTestRecord(result int64, offset uint32, payload string) socketRecord {
	record, _ := parseSocketRecord(socketSample(socketRecordSend, result, offset, false, 0, payload))
	return record
}

func TestSocketCallsJoinTheChunksOfOneCall(t *testing.T) {
	now := time.Now()
	calls := newSocketCalls(1 << 20)
	if done := calls.add(socketTestRecord(10, 0, "hello"), now); len(done) != 0 {
		t.Fatalf("first chunk finished the call: %+v", done)
	}
	done := calls.add(socketTestRecord(10, 5, "world"), now)
	if len(done) != 1 || string(done[0].payload) != "helloworld" || done[0].truncated {
		t.Fatalf("done = %+v", done)
	}
	if len(calls.open) != 0 {
		t.Fatalf("open calls = %d", len(calls.open))
	}
}

func TestSocketCallsKeepTheHeadWithinTheLimit(t *testing.T) {
	calls := newSocketCalls(4)
	done := calls.add(socketTestRecord(10, 0, "hell"), time.Now())
	if len(done) != 1 || string(done[0].payload) != "hell" || done[0].truncated {
		t.Fatalf("done = %+v", done)
	}
}

func TestSocketCallsMarkLostChunks(t *testing.T) {
	now := time.Now()
	calls := newSocketCalls(1 << 20)
	calls.add(socketTestRecord(15, 0, "hello"), now)
	// 5에서 시작하는 조각을 잃었다.
	if done := calls.add(socketTestRecord(15, 10, "again"), now); len(done) != 1 || !done[0].truncated || string(done[0].payload) != "hello" {
		t.Fatalf("done = %+v", done)
	}
	calls.add(socketTestRecord(15, 0, "hello"), now.Add(time.Millisecond))
	if done := calls.expire(now.Add(socketCallIdle / 2)); len(done) != 0 {
		t.Fatalf("expired too early: %+v", done)
	}
	if done := calls.expire(now.Add(2 * socketCallIdle)); len(done) != 1 || !done[0].truncated {
		t.Fatalf("expire = %+v", done)
	}
}

func TestSocketCallsFinishCallsWithoutPayloadAtOnce(t *testing.T) {
	calls := newSocketCalls(0)
	if done := calls.add(socketTestRecord(10, 0, ""), time.Now()); len(done) != 1 || done[0].truncated || done[0].payload != nil {
		t.Fatalf("bytes only = %+v", done)
	}
	calls = newSocketCalls(4096)
	record, _ := parseSocketRecord(socketSample(socketRecordRecv, 10, 0, false, socketRecordUnreadable, ""))
	if done := calls.add(record, time.Now()); len(done) != 1 || !done[0].truncated {
		t.Fatalf("unreadable = %+v", done)
	}
}

func TestSocketTraceEventNamesTheCall(t *testing.T) {
	for _, test := range []struct {
		eventType uint32
		result    int64
		server    bool
		errorName string
		event     string
		label     string
	}{
		{socketRecordConnect, 0, false, "", "socket_connect", "connect"},
		{socketRecordConnect, -111, false, "ECONNREFUSED", "socket_connect_fail", "connect ECONNREFUSED"},
		{socketRecordSend, 2048, false, "", "socket_send", "send 2.0KiB"},
		{socketRecordSend, -32, false, "EPIPE", "socket_send_fail", "send EPIPE"},
		{socketRecordRecv, 10, true, "", "socket_recv", "recv 10B"},
		{socketRecordRecv, 0, true, "", "socket_eof", "eof"},
		{socketRecordClose, 0, true, "", "socket_close", "close"},
	} {
		record, _ := parseSocketRecord(socketSample(test.eventType, test.result, 0, test.server, 0, ""))
		event := socketTraceEvent(socketCall{record: record}, "/run/docker.sock", 0, test.errorName, "docker")
		destination, label := traceSocketScrollLabels(event)
		if event.Event != test.event || label != test.label || destination != "/run/docker.sock" || event.Protocol != "socket" || event.Source != "docker[7]" || event.PeerPID != 7 {
			t.Fatalf("%s: event = %+v, label %q", test.event, event, label)
		}
		if side := map[bool]string{false: traceClientSide, true: traceServerSide}[test.server]; event.Side != side {
			t.Fatalf("%s: side = %q", test.event, event.Side)
		}
		if (test.errorName != "") != (event.Error != "") {
			t.Fatalf("%s: error = %q", test.event, event.Error)
		}
	}
}

func TestSocketTraceEventEscapesThePayload(t *testing.T) {
	record, _ := parseSocketRecord(socketSample(socketRecordSend, 4, 0, false, 0, ""))
	event := socketTraceEvent(socketCall{record: record, payload: []byte("a\x1b\r\n"), truncated: true}, "/tmp/x.sock", 0, "", "")
	if event.Payload != `a\x1b`+"\r\n" || !event.PayloadTruncated || event.Bytes != 4 {
		t.Fatalf("event = %+v", event)
	}
	if line := traceSocketPayloadLine(event); line != `[truncated] data a\x1b ↵ ` {
		t.Fatalf("payload line = %q", line)
	}
}

func TestSocketTraceSummarizerGroupsByProcessPeerAndSide(t *testing.T) {
	summarizer := newSocketTraceSummarizer()
	for _, event := range []captureEvent{
		{Event: "socket_connect", Process: "docker", Source: "dockerd[812]", Side: traceClientSide, Destination: "/run/docker.sock"},
		{Event: "socket_send", Process: "docker", Source: "dockerd[812]", Side: traceClientSide, Bytes: 100},
		{Event: "socket_recv", Process: "docker", Source: "dockerd[812]", Side: traceClientSide, Bytes: 900},
		{Event: "socket_send", Process: "docker", Source: "dockerd[999]", Side: traceClientSide, Bytes: 50},
		{Event: "socket_connect_fail", Process: "curl", Side: traceClientSide, Error: "ECONNREFUSED"},
	} {
		summarizer.observe(event)
	}
	report := summarizer.summarize(captureSummary{LostEvents: 2}, time.Second).(socketTraceReport)
	if report.Path != "/run/docker.sock" || report.Events != 5 || report.TXBytes != 150 || report.RXBytes != 900 || report.Failures != 1 || report.LostEvents != 2 || len(report.Rows) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if first := report.Rows[0]; first.Process != "curl" || first.Failures != 1 {
		t.Fatalf("the failed row must come first: %+v", report.Rows)
	}
	if second := report.Rows[1]; second.Peer != "dockerd" || second.Connects != 1 || second.Sends != 2 || second.Recvs != 1 || second.Events != 4 {
		t.Fatalf("docker row = %+v", second)
	}
	if strings.Contains(report.Rows[1].Peer, "[") {
		t.Fatal("the peer pid must not split rows")
	}
}
