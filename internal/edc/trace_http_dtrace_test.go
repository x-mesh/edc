package edc

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2/hpack"
)

func darwinTLSTestDump(record darwinTLSRecord) []string {
	sent, closed := 0, 0
	if record.sent {
		sent = 1
	}
	if record.closed {
		closed = 1
	}
	lines := []string{fmt.Sprintf("EDC_TLS %d %d %d %d %d %d %d %d %d %s", record.pid, record.wallNS, record.object, sent, closed, record.offset, len(record.payload), record.size, record.fd, record.process), "      0  1  2  3  4  5  6  7  8  9  a  b  c  d  e  f  0123456789abcdef"}
	for at := 0; at < len(record.payload); at += 16 {
		var line strings.Builder
		fmt.Fprintf(&line, " %5x:", at)
		for _, value := range record.payload[at:min(at+16, len(record.payload))] {
			fmt.Fprintf(&line, " %02x", value)
		}
		line.WriteString("  ignored ASCII column")
		lines = append(lines, line.String())
	}
	return append(lines, "EDC_END")
}
func TestDarwinTLSDecoderBinaryPayload(t *testing.T) {
	payload := append([]byte("HTTP/2\x00\xff\r\nEDC_END\n"), bytes.Repeat([]byte{0, 255, 10, 13}, 12)...)
	input := darwinTLSRecord{pid: 42, wallNS: 1000000, object: 1234, sent: true, fd: 9, offset: 900, size: uint64(len(payload)), process: "test client", payload: payload}
	var decoder darwinTLSDecoder
	var got *darwinTLSRecord
	for _, line := range darwinTLSTestDump(input) {
		record, err := decoder.line(line)
		if err != nil {
			t.Fatal(err)
		}
		if record != nil {
			got = record
		}
	}
	if got == nil || !bytes.Equal(got.payload, payload) || got.pid != 42 || got.offset != 900 || got.process != "test client" {
		t.Fatalf("record=%#v", got)
	}
}
func TestDarwinTLSDecoderRejectsMalformedRecords(t *testing.T) {
	tests := [][]string{
		{"EDC_END"}, {"EDC_FAULT"},
		{"EDC_TLS 0 100 1 1 0 0 1 1 -1 app"},
		{"EDC_TLS 42 100 0 1 0 0 1 1 -1 app"},
		{"EDC_TLS 42 100 1 1 0 0 1048577 1048577 -1 app"},
		{"EDC_TLS 42 100 1 1 0 0 2 1 -1 app"},
		{"EDC_TLS 42 100 1 1 0 0 1 1 -2 app"},
		{"EDC_TLS 42 100 1 1 1 0 1 1 -1 app"},
		{"EDC_TLS 42 100 1 1 0 0 1 1 -1 app", "EDC_END"},
		{"EDC_TLS 42 100 1 1 0 0 1 1 -1 app", "1: ff"},
		{"EDC_TLS 42 100 1 1 0 0 1 1 -1 app", "0: xx"},
		{"EDC_TLS 42 100 1 1 0 0 1 1 -1 app", "EDC_TLS 42 100 2 1 0 0 1 1 -1 app"},
	}
	for _, lines := range tests {
		var decoder darwinTLSDecoder
		failed := false
		for _, line := range lines {
			if _, err := decoder.line(line); err != nil {
				failed = true
				break
			}
		}
		if !failed {
			t.Fatalf("accepted malformed record %q", lines)
		}
	}
}
func TestDarwinTLSScriptLengthsAndNestedCalls(t *testing.T) {
	script, err := darwinTLSScript([]traceTLSTarget{{path: "/test/libssl.dylib", symbols: darwinTLSFunctions}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"self->edc_sent ? self->edc_wall : (uint64_t)walltimestamp", "#pragma D option temporal", "pid$target:libssl.dylib:SSL_read:entry", "pid$target:libssl.dylib:SSL_write_ex:return", "self->edc_depth == 1", "self->edc_actual <= self->edc_requested", "*(userland uint64_t *)self->edc_out", "trace(*(userland edc_tls_bytes_16 *)(self->edc_buffer+this->offset))", "trace(*(userland edc_tls_bytes_1048576 *)(self->edc_buffer+this->offset))", "(uint32_t)*(userland uint8_t *)(self->edc_buffer+this->offset+0)", "edc_offset[pid,self->edc_object,self->edc_sent] += self->edc_actual", "ERROR { printf(\"EDC_FAULT\\n\"); }"} {
		if !strings.Contains(script, part) {
			t.Errorf("missing %q", part)
		}
	}
	if strings.Contains(script, "%!") {
		t.Fatalf("invalid script format: %s", script)
	}
	for _, path := range []string{"/bad/libssl:entry.dylib", "/bad/libssl*.dylib", "/bad/a\nBEGIN"} {
		if _, err := darwinTLSScript([]traceTLSTarget{{path: path, symbols: darwinTLSFunctions}}); err == nil {
			t.Errorf("accepted unsafe module %q", path)
		}
	}
}
func TestDarwinTLSDecryptedSessionsAndRedaction(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
	now := time.Now()
	packet := pktapTestTCP(true, 0, "GET /first HTTP/1.1\r\nHost: example.com\r\nAuthorization: secret\r\n\r\n")
	packet.decrypted = true
	packet.tlsObject = 100
	got := tracker.add(packet, now)
	if len(got) != 1 || !got[0].TLS || strings.Contains(got[0].Payload, "Authorization: secret") {
		t.Fatalf("first=%#v", got)
	}
	packet.tlsObject = 200
	packet.payload = []byte("GET /second HTTP/1.1\r\nHost: example.com\r\n\r\n")
	if got := tracker.add(packet, now); len(got) != 1 || got[0].Path != "/second" {
		t.Fatalf("second=%#v", got)
	}
	response := pktapTestTCP(false, 0, "HTTP/1.1 204 No Content\r\n\r\n")
	response.decrypted = true
	response.tlsObject = 200
	response.source = "127.0.0.1:50001"
	got = tracker.add(response, now)
	if len(got) != 1 || got[0].Path != "/second" || !got[0].TLS {
		t.Fatalf("response=%#v", got)
	}
}
func TestDarwinTLSDecryptedHTTP2Fragments(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	body := append(bytes.Clone(http2Preface), http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/secure-h2"}))...)
	packet := pktapTestTCP(true, 0, string(body[:9]))
	packet.decrypted = true
	packet.tlsObject = 50
	now := time.Now()
	tracker.add(packet, now)
	packet.sequence = 9
	packet.payload = body[9:]
	got := tracker.add(packet, now)
	if len(got) != 1 || !got[0].TLS || got[0].Path != "/secure-h2" {
		t.Fatalf("request=%#v", got)
	}
	response := pktapTestTCP(false, 0, string(http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "204"}))))
	response.decrypted = true
	response.tlsObject = 50
	got = tracker.add(response, now)
	if len(got) != 1 || !got[0].TLS || got[0].Status != 204 || got[0].Path != "/secure-h2" {
		t.Fatalf("response=%#v", got)
	}
}
func FuzzDarwinTLSDecoder(f *testing.F) {
	f.Add("EDC_TLS 42 100 1 1 0 0 1 1 -1 app\n0: ff\nEDC_END")
	f.Add("EDC_FAULT")
	f.Fuzz(func(t *testing.T, input string) {
		var decoder darwinTLSDecoder
		for _, line := range strings.Split(input, "\n") {
			if _, err := decoder.line(line); err != nil {
				return
			}
		}
	})
}
