package edc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/net/http2/hpack"
)

func TestTLSPlaintextPacketPreservesMetadataAndDirection(t *testing.T) {
	record := tlsPlaintextRecord{packet: &httpPacket{pid: 42, socket: 99, sent: true, payload: []byte("GET / HTTP/1.1\r\n\r\n"), captureSource: "openssl_uprobe"}}
	packet := record.packet
	if packet.pid != 42 || packet.socket != 99 || !packet.sent || packet.captureSource != "openssl_uprobe" {
		t.Fatalf("packet = %+v", packet)
	}
}

func TestTLSPlaintextPacketRejectsEmptyAndClosedRecords(t *testing.T) {
	for _, record := range []tlsPlaintextRecord{{}, {closeSocket: 1}} {
		if record.packet != nil {
			t.Fatalf("control record contains packet: %+v", record.packet)
		}
	}
}

func TestTLSPlaintextHTTP2ClientPrefaceIsOutbound(t *testing.T) {
	if !bytes.HasPrefix(http2Preface, []byte("PRI * HTTP/2.0")) {
		t.Fatal("HTTP/2 client preface changed")
	}
}

func TestTLSPlaintextHTTP1DecoderOmitsSecretsAndBody(t *testing.T) {
	const secret = "EDC_SECRET_HTTP1"
	tracker := newHTTPTracker(traceClientSide, false, false)
	packet := httpTestPacket("POST /codex/install.sh?token="+secret+" HTTP/1.1\r\nHost: chatgpt.com\r\nAuthorization: Bearer "+secret+"\r\nCookie: sid="+secret+"\r\nContent-Length: 16\r\n\r\n"+secret, 1, true)
	packet.captureSource = "openssl_uprobe"
	event, ok := tracker.event(packet, 0)
	if !ok || event.Method != "POST" || event.Target != "chatgpt.com" || event.Path != "/codex/install.sh" || event.Payload != "" {
		t.Fatalf("event = %+v, %t", event, ok)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("secret leaked: %s", encoded)
	}
	if destination, _ := traceHTTPScrollLabels(event); destination != "client: POST https://chatgpt.com/codex/install.sh (127.0.0.1:8080)" {
		t.Fatalf("destination = %q", destination)
	}
}

func TestTLSPlaintextHTTP2DecoderOmitsQuery(t *testing.T) {
	tracker := newHTTPTracker(traceClientSide, false, false)
	block := http2TestHeaders(
		hpack.HeaderField{Name: ":method", Value: "GET"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: ":authority", Value: "chatgpt.com"},
		hpack.HeaderField{Name: ":path", Value: "/codex/install.sh?token=EDC_SECRET_H2"},
		hpack.HeaderField{Name: "authorization", Value: "Bearer EDC_SECRET_H2"},
	)
	payload := append(append([]byte{}, http2Preface...), http2TestFrame(1, 4, 1, block)...)
	packet := httpTestPacket(string(payload), 1, true)
	packet.captureSource = "openssl_uprobe"
	events, claimed := tracker.http2Events(packet, 0)
	if !claimed || len(events) != 1 || events[0].Path != "/codex/install.sh" || events[0].Target != "chatgpt.com" {
		t.Fatalf("events = %+v, claimed = %t", events, claimed)
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "EDC_SECRET_H2") {
		t.Fatalf("secret leaked: %s", encoded)
	}
}
