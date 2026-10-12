package edc

import (
	"fmt"
	"strings"
)

// HTTPS의 handshake 머리는 평문이다. ClientHello 뒤에 서버가 고른 version과 cipher가 ServerHello로 오고, handshake가
// 실패하면 alert record가 온다. kernel에서 읽으므로 TLS library가 무엇이든 같다. 다만 TLS 1.3은 ServerHello 뒤의 alert를
// 암호문으로 보내므로, client가 서버 인증서를 거절하는 alert는 TLS 1.2에서만 보인다.

const (
	traceTLSServerHelloEvent = "tls_server_hello"
	traceTLSAlertEvent       = "tls_alert"
	tlsAlertRecord           = 0x15
	tlsServerHelloType       = 0x02
	// tlsServerHelloFixedSize는 handshake 머리 4 byte, version 2 byte, random 32 byte다.
	tlsServerHelloFixedSize       = 4 + 2 + 32
	tlsExtensionSupportedVersions = 0x002b
	tlsAlertLevelWarning          = 1
	tlsAlertLevelFatal            = 2
)

type tlsServerHello struct {
	version string
	cipher  string
	alpn    string
}

type tlsAlert struct {
	level       string
	description string
}

func (alert tlsAlert) String() string { return alert.level + " " + alert.description }

var tlsVersionNames = map[uint16]string{0x0300: "SSL3", 0x0301: "TLS1.0", 0x0302: "TLS1.1", 0x0303: "TLS1.2", 0x0304: "TLS1.3"}

// tlsCipherNames는 흔한 cipher suite다. 없는 값은 16진수로 보인다.
var tlsCipherNames = map[uint16]string{
	0x1301: "TLS_AES_128_GCM_SHA256", 0x1302: "TLS_AES_256_GCM_SHA384", 0x1303: "TLS_CHACHA20_POLY1305_SHA256",
	0xc02b: "ECDHE_ECDSA_AES_128_GCM_SHA256", 0xc02c: "ECDHE_ECDSA_AES_256_GCM_SHA384",
	0xc02f: "ECDHE_RSA_AES_128_GCM_SHA256", 0xc030: "ECDHE_RSA_AES_256_GCM_SHA384",
	0xcca8: "ECDHE_RSA_CHACHA20_POLY1305", 0xcca9: "ECDHE_ECDSA_CHACHA20_POLY1305",
	0xc023: "ECDHE_ECDSA_AES_128_CBC_SHA256", 0xc024: "ECDHE_ECDSA_AES_256_CBC_SHA384",
	0xc027: "ECDHE_RSA_AES_128_CBC_SHA256", 0xc028: "ECDHE_RSA_AES_256_CBC_SHA384",
	0xc009: "ECDHE_ECDSA_AES_128_CBC_SHA", 0xc00a: "ECDHE_ECDSA_AES_256_CBC_SHA",
	0xc013: "ECDHE_RSA_AES_128_CBC_SHA", 0xc014: "ECDHE_RSA_AES_256_CBC_SHA",
	0x009e: "DHE_RSA_AES_128_GCM_SHA256", 0x009f: "DHE_RSA_AES_256_GCM_SHA384",
	0x009c: "RSA_AES_128_GCM_SHA256", 0x009d: "RSA_AES_256_GCM_SHA384",
	0x003c: "RSA_AES_128_CBC_SHA256", 0x003d: "RSA_AES_256_CBC_SHA256",
	0x002f: "RSA_AES_128_CBC_SHA", 0x0035: "RSA_AES_256_CBC_SHA",
}

// tlsAlertNames는 RFC 8446과 RFC 5246의 AlertDescription이다.
var tlsAlertNames = map[byte]string{
	0: "close_notify", 10: "unexpected_message", 20: "bad_record_mac", 21: "decryption_failed", 22: "record_overflow",
	30: "decompression_failure", 40: "handshake_failure", 41: "no_certificate", 42: "bad_certificate", 43: "unsupported_certificate",
	44: "certificate_revoked", 45: "certificate_expired", 46: "certificate_unknown", 47: "illegal_parameter", 48: "unknown_ca",
	49: "access_denied", 50: "decode_error", 51: "decrypt_error", 60: "export_restriction", 70: "protocol_version",
	71: "insufficient_security", 80: "internal_error", 86: "inappropriate_fallback", 90: "user_canceled", 100: "no_renegotiation",
	109: "missing_extension", 110: "unsupported_extension", 111: "certificate_unobtainable", 112: "unrecognized_name",
	113: "bad_certificate_status_response", 114: "bad_certificate_hash_value", 115: "unknown_psk_identity",
	116: "certificate_required", 120: "no_application_protocol",
}

func tlsVersionName(value uint16) string {
	if name, ok := tlsVersionNames[value]; ok {
		return name
	}
	return fmt.Sprintf("0x%04x", value)
}

func tlsCipherName(value uint16) string {
	if name, ok := tlsCipherNames[value]; ok {
		return name
	}
	return fmt.Sprintf("0x%04x", value)
}

func tlsAlertName(value byte) string {
	if name, ok := tlsAlertNames[value]; ok {
		return name
	}
	return fmt.Sprintf("alert_%d", value)
}

// parseTLSServerHello는 ServerHello에서 version, cipher suite, 고른 ALPN을 읽는다. TLS 1.3은 version을
// supported_versions 확장에 두고 legacy 자리에는 TLS 1.2를 적는다. ALPN은 TLS 1.2에서만 ServerHello에 있고, TLS 1.3은
// 암호문인 EncryptedExtensions에 둔다.
func parseTLSServerHello(payload []byte, record bool) (tlsServerHello, bool) {
	if record {
		if len(payload) < tlsRecordHeaderSize || !tlsRecordStart(payload) {
			return tlsServerHello{}, false
		}
		payload = payload[tlsRecordHeaderSize:]
	}
	if len(payload) < tlsServerHelloFixedSize || payload[0] != tlsServerHelloType || !tlsKnownVersion(payload[4], payload[5]) {
		return tlsServerHello{}, false
	}
	version := uint16(payload[4])<<8 | uint16(payload[5])
	rest := payload[tlsServerHelloFixedSize:]
	var ok bool
	if _, rest, ok = tlsVector(rest, 1); !ok || len(rest) < 3 {
		return tlsServerHello{}, false
	}
	hello := tlsServerHello{cipher: tlsCipherName(uint16(rest[0])<<8 | uint16(rest[1]))}
	rest = rest[3:]
	extensions, _, ok := tlsVector(rest, 2)
	if !ok && len(rest) >= 2 {
		extensions = rest[2:]
	}
	for len(extensions) >= 4 {
		kind := int(extensions[0])<<8 | int(extensions[1])
		data, next, ok := tlsVector(extensions[2:], 2)
		if !ok {
			break
		}
		switch kind {
		case tlsExtensionSupportedVersions:
			if len(data) >= 2 {
				version = uint16(data[0])<<8 | uint16(data[1])
			}
		case tlsExtensionALPN:
			if protocols := tlsALPN(data); len(protocols) > 0 {
				hello.alpn = protocols[0]
			}
		}
		extensions = next
	}
	hello.version = tlsVersionName(version)
	return hello, true
}

// parseTLSAlert는 alert record를 읽는다. BPF는 alert를 handshake 레코드 모양으로 넘기는데, 그때 payload는 0x15, level,
// description이다. record면 record 머리 5 byte 뒤에 level과 description이 온다.
func parseTLSAlert(payload []byte, record bool) (tlsAlert, bool) {
	if record {
		if len(payload) < tlsRecordHeaderSize+2 || payload[0] != tlsAlertRecord || !tlsKnownVersion(payload[1], payload[2]) || payload[3] != 0 || payload[4] != 2 {
			return tlsAlert{}, false
		}
		payload = []byte{tlsAlertRecord, payload[tlsRecordHeaderSize], payload[tlsRecordHeaderSize+1]}
	}
	if len(payload) < 3 || payload[0] != tlsAlertRecord {
		return tlsAlert{}, false
	}
	alert := tlsAlert{description: tlsAlertName(payload[2])}
	switch payload[1] {
	case tlsAlertLevelWarning:
		alert.level = "warning"
	case tlsAlertLevelFatal:
		alert.level = "fatal"
	default:
		alert.level = fmt.Sprintf("level_%d", payload[1])
	}
	return alert, true
}

// tlsHandshakeEvent는 ClientHello, ServerHello, alert를 모두 가리킨다. 응답과 짝짓지 않고 message를 열어 두지 않는다.
func tlsHandshakeEvent(event string) bool {
	return event == traceTLSHelloEvent || event == traceTLSServerHelloEvent || event == traceTLSAlertEvent
}

// tlsConnection은 socket마다 기억하는 ClientHello다. ServerHello의 handshake 시간과 alert를 보낸 쪽을 정하는 데 쓴다.
type tlsConnection struct {
	helloAt uint64
	side    string
	host    string
}

// tlsServerEvent는 ServerHello 하나를 event로 만든다. client는 ServerHello를 받고, 로컬 서버는 보낸다. LatencyMS는 같은
// socket의 ClientHello부터 잰 시간이다. client 쪽은 network와 서버의 처리가, 서버 쪽은 처리만 들어간다.
func (tracker *httpTracker) tlsServerEvent(packet httpPacket, hello tlsServerHello, clockOffset int64) (captureEvent, bool) {
	side := traceServerSide
	if !packet.sent {
		side = traceClientSide
	}
	if tracker.side != "" && side != tracker.side {
		return captureEvent{}, false
	}
	event := captureEvent{
		SocketID: packet.socket, TimestampNS: uint64(int64(packet.bootTimeNS) + clockOffset), BootTimeNS: packet.bootTimeNS, Protocol: "http", Side: side,
		PID: packet.pid, Process: packet.process, CgroupID: packet.cgroupID, Source: packet.source, Destination: packet.destination, Bytes: uint64(len(packet.payload)),
		Event: traceTLSServerHelloEvent, TLSVersion: hello.version, Cipher: hello.cipher, Target: traceHTTPHost(packet.destination, side == traceServerSide),
	}
	if hello.alpn != "" {
		event.ALPN = []string{hello.alpn}
	}
	// TLS 1.2의 인증서 거절 alert는 ServerHello 뒤에 오므로 연결 정보를 지우지 않는다. socket이 끝날 때 forget이 지운다.
	if connection, ok := tracker.tls[packet.socket]; ok {
		event.Target = emptyAs(connection.host, event.Target)
		event.LatencyMS = traceSpan(connection.helloAt, packet.bootTimeNS)
	}
	return event, true
}

// tlsAlertEvent는 alert record 하나를 event로 만든다. AlertFrom은 보낸 쪽이다. 이 socket의 ClientHello를 봤으면 로컬이
// client인지 서버인지 알고, 못 봤으면 보낸 방향만 안다.
func (tracker *httpTracker) tlsAlertEvent(packet httpPacket, alert tlsAlert, clockOffset int64) (captureEvent, bool) {
	connection, known := tracker.tls[packet.socket]
	side := connection.side
	from := "peer"
	if packet.sent {
		from = "local"
	}
	if known {
		if packet.sent == (side == traceClientSide) {
			from = traceClientSide
		} else {
			from = traceServerSide
		}
	}
	if tracker.side != "" && side != tracker.side {
		return captureEvent{}, false
	}
	event := captureEvent{
		SocketID: packet.socket, TimestampNS: uint64(int64(packet.bootTimeNS) + clockOffset), BootTimeNS: packet.bootTimeNS, Protocol: "http", Side: side,
		PID: packet.pid, Process: packet.process, CgroupID: packet.cgroupID, Source: packet.source, Destination: packet.destination, Bytes: uint64(len(packet.payload)),
		Event: traceTLSAlertEvent, Alert: alert.String(), AlertFrom: from, Target: emptyAs(connection.host, traceHTTPHost(packet.destination, side == traceServerSide)),
	}
	if known {
		event.LatencyMS = traceSpan(connection.helloAt, packet.bootTimeNS)
	}
	return event, true
}

// traceTLSLabel은 event 칸에 붙는 짧은 설명이다. ServerHello는 version과 cipher, alert는 level과 이름이다.
func traceTLSLabel(event captureEvent) string {
	switch event.Event {
	case traceTLSServerHelloEvent:
		return strings.TrimSpace(event.TLSVersion + " " + event.Cipher)
	case traceTLSAlertEvent:
		return strings.TrimSpace(event.Alert + " from " + event.AlertFrom)
	}
	return ""
}
