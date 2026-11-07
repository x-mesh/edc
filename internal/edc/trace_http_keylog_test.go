package edc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"golang.org/x/net/http2/hpack"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type tlsKeylogTestBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *tlsKeylogTestBuffer) Write(data []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(data)
}
func (b *tlsKeylogTestBuffer) snapshot() []byte {
	b.Lock()
	defer b.Unlock()
	return bytes.Clone(b.Buffer.Bytes())
}

type tlsKeylogTestWire struct {
	sync.Mutex
	packets  []pktapTCPPacket
	sequence [2]uint32
}
type tlsKeylogTestConn struct {
	net.Conn
	wire *tlsKeylogTestWire
	sent bool
}

func (c *tlsKeylogTestConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	if n > 0 {
		c.wire.Lock()
		index := 0
		if c.sent {
			index = 1
		}
		packet := pktapTestTCP(c.sent, c.wire.sequence[index], string(data[:n]))
		packet.bootTimeNS = uint64(time.Now().UnixNano())
		c.wire.sequence[index] += uint32(n)
		c.wire.packets = append(c.wire.packets, packet)
		c.wire.Unlock()
	}
	return n, err
}
func tlsKeylogTestCapture(t *testing.T, version, suite uint16, h2 bool) ([]pktapTCPPacket, []byte) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer fixture.Close()
	certificate := fixture.TLS.Certificates[0]
	if version == tls.VersionTLS12 {
		if _, ok := certificate.PrivateKey.(*ecdsa.PrivateKey); ok {
			switch suite {
			case tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:
				suite = tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
			case tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:
				suite = tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
			case tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256:
				suite = tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
			}
		}
	}
	first, second := net.Pipe()
	defer first.Close()
	defer second.Close()
	_ = first.SetDeadline(time.Now().Add(5 * time.Second))
	_ = second.SetDeadline(time.Now().Add(5 * time.Second))
	wire := &tlsKeylogTestWire{}
	keys := &tlsKeylogTestBuffer{}
	// The client trusts the generated fixture certificate only within this test.
	client := tls.Client(&tlsKeylogTestConn{Conn: first, wire: wire, sent: true}, &tls.Config{InsecureSkipVerify: true, MinVersion: version, MaxVersion: version, CipherSuites: []uint16{suite}, KeyLogWriter: keys})
	server := tls.Server(&tlsKeylogTestConn{Conn: second, wire: wire}, &tls.Config{MinVersion: version, MaxVersion: version, CipherSuites: []uint16{suite}, Certificates: []tls.Certificate{certificate}})
	request := []byte("GET /secure?token=secret HTTP/1.1\r\nHost: example.com\r\nAuthorization: Bearer secret\r\n\r\n")
	response := []byte("HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nsecure")
	if h2 {
		request = append(bytes.Clone(http2Preface), http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":method", Value: "GET"}, hpack.HeaderField{Name: ":path", Value: "/h2-secure"}))...)
		response = http2TestFrame(1, 5, 1, http2TestHeaders(hpack.HeaderField{Name: ":status", Value: "204"}))
	}
	done := make(chan error, 1)
	go func() {
		if err := server.Handshake(); err != nil {
			done <- err
			return
		}
		buffer := make([]byte, len(request))
		if _, err := io.ReadFull(server, buffer); err != nil {
			done <- err
			return
		}
		_, err := server.Write(response)
		done <- err
	}()
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(response))
	if _, err := io.ReadFull(client, buffer); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wire.Lock()
	packets := append([]pktapTCPPacket{}, wire.packets...)
	wire.Unlock()
	return packets, keys.snapshot()
}
func TestTLSKeylogDecryptsRealTLS(t *testing.T) {
	tests := []struct {
		name           string
		version, suite uint16
	}{
		{"tls13", tls.VersionTLS13, 0},
		{"tls12-aes128", tls.VersionTLS12, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		{"tls12-aes256", tls.VersionTLS12, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384},
		{"tls12-chacha", tls.VersionTLS12, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packets, keys := tlsKeylogTestCapture(t, test.version, test.suite, false)
			path := filepath.Join(t.TempDir(), "fixture.keys")
			if err := os.WriteFile(path, keys, 0600); err != nil {
				t.Fatal(err)
			}
			keylog, err := openTLSKeylog(path)
			if err != nil {
				t.Fatal(err)
			}
			defer keylog.close()
			tracker := newPKTAPHTTPTracker(traceScope{payload: true, payloadAll: true}, 0)
			tracker.tls = newTLSKeylogDecryptor(keylog, tracker)
			var events []captureEvent
			for _, packet := range packets {
				// Split each captured TLS write across TCP packets and repeat its first segment.
				middle := len(packet.payload) / 2
				tail := packet
				tail.payload = packet.payload[middle:]
				tail.sequence += uint32(middle)
				packet.payload = packet.payload[:middle]
				events = append(events, tracker.add(packet, time.Now())...)
				events = append(events, tracker.add(packet, time.Now())...)
				events = append(events, tracker.add(tail, time.Now())...)
			}
			events = append(events, tracker.sweep(time.Now(), true)...)
			if tracker.tls.err != nil {
				t.Fatal(tracker.tls.err)
			}
			var request, response bool
			for _, event := range events {
				if event.TLS && event.Path == "/secure" {
					request = request || event.Method == "GET" && event.Status == 0
					response = response || event.Status == 200
					if strings.Contains(event.Payload, "Bearer secret") {
						t.Fatal("authorization header was not masked")
					}
				}
			}
			if !request || !response || tracker.tls.unmapped != 0 || tracker.buffered != 0 || tracker.tls.pending != 0 {
				t.Fatalf("request=%t response=%t unmapped=%d buffered=%d pending=%d events=%#v", request, response, tracker.tls.unmapped, tracker.buffered, tracker.tls.pending, events)
			}
		})
	}
}
func TestTLSKeylogWaitsForAppendedSecrets(t *testing.T) {
	packets, keys := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, false)
	path := filepath.Join(t.TempDir(), "fixture.keys")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	keylog, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer keylog.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(keylog, tracker)
	for _, packet := range packets {
		tracker.add(packet, time.Now())
	}
	if tracker.tls.pending == 0 {
		t.Fatal("encrypted records were not retained")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(keys[:len(keys)-1]); err != nil {
		t.Fatal(err)
	}
	tracker.tls.refresh(time.Now())
	if _, err := file.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	file.Close()
	events := tracker.tls.refresh(time.Now())
	events = append(events, tracker.sweep(time.Now(), true)...)
	if tracker.tls.err != nil {
		t.Fatal(tracker.tls.err)
	}
	response := false
	for _, event := range events {
		response = response || event.TLS && event.Status == 200
	}
	if !response || tracker.tls.pending != 0 || tracker.tls.buffered != 0 {
		t.Fatalf("response=%t pending=%d events=%#v", response, tracker.tls.pending, events)
	}
}
func TestTLSKeylogRejectsInvalidSecretsWithoutPrintingThem(t *testing.T) {
	secret := strings.Repeat("ff", 32)
	for _, input := range []string{"CLIENT_RANDOM " + strings.Repeat("00", 32) + " " + secret, "CLIENT_TRAFFIC_SECRET_0 invalid " + secret, "SERVER_TRAFFIC_SECRET_0 " + strings.Repeat("00", 32) + " nope"} {
		log := &tlsKeylog{keys: map[tlsKeylogKey][]byte{}}
		err := log.line([]byte(input))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("error=%v", err)
		}
	}
}
func TestTLSKeylogAuthenticatesCiphertext(t *testing.T) {
	packets, keys := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, false)
	path := filepath.Join(t.TempDir(), "fixture.keys")
	if err := os.WriteFile(path, keys, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(log, tracker)
	for _, packet := range packets {
		if len(packet.payload) > 6 && packet.payload[0] == 23 {
			packet.payload = bytes.Clone(packet.payload)
			packet.payload[len(packet.payload)-1] ^= 1
		}
		tracker.add(packet, time.Now())
		if tracker.tls.err != nil {
			break
		}
	}
	if tracker.tls.err == nil || !strings.Contains(tracker.tls.err.Error(), "authentication failed") {
		t.Fatalf("error=%v", tracker.tls.err)
	}
}
func FuzzTLSKeylogLine(f *testing.F) {
	f.Add(fmt.Sprintf("CLIENT_TRAFFIC_SECRET_0 %064x %064x", 1, 2))
	f.Add("# comment")
	f.Fuzz(func(t *testing.T, line string) {
		log := &tlsKeylog{keys: map[tlsKeylogKey][]byte{}}
		_ = log.line([]byte(line))
	})
}

func TestTLSKeylogDecryptsHTTP2(t *testing.T) {
	packets, keys := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, true)
	path := filepath.Join(t.TempDir(), "fixture.keys")
	if err := os.WriteFile(path, keys, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(log, tracker)
	var events []captureEvent
	for _, packet := range packets {
		events = append(events, tracker.add(packet, time.Now())...)
	}
	events = append(events, tracker.sweep(time.Now(), true)...)
	if tracker.tls.err != nil {
		t.Fatal(tracker.tls.err)
	}
	response := false
	for _, event := range events {
		response = response || event.TLS && event.Status == 204 && event.Path == "/h2-secure"
	}
	if !response {
		t.Fatalf("events=%#v", events)
	}
}
func TestTLSKeylogMissingSecretsAreCounted(t *testing.T) {
	packets, _ := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, false)
	path := filepath.Join(t.TempDir(), "fixture.keys")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(log, tracker)
	for _, packet := range packets {
		tracker.add(packet, time.Now())
	}
	events := tracker.sweep(time.Now(), true)
	if tracker.tls.unmapped == 0 || tracker.tls.pending != 0 || tracker.tls.buffered != 0 || len(tracker.tls.connections) != 0 {
		t.Fatalf("missing=%d pending=%d connections=%d", tracker.tls.unmapped, tracker.tls.pending, len(tracker.tls.connections))
	}
	for _, event := range events {
		if event.TLS && event.Status != 0 {
			t.Fatalf("ciphertext was reported as plaintext: %#v", event)
		}
	}
}

func tlsKeylogTestRecord(t *testing.T, suite uint16, secret []byte, sequence uint64, kind byte, plaintext []byte) []byte {
	t.Helper()
	hashFunc, size, chacha, err := tlsKeylogSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	key, err := tlsExpandLabel(hashFunc, secret, "key", size)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := tlsExpandLabel(hashFunc, secret, "iv", 12)
	if err != nil {
		t.Fatal(err)
	}
	for at := 0; at < 8; at++ {
		nonce[len(nonce)-1-at] ^= byte(sequence >> (at * 8))
	}
	aead, err := tlsKeylogAEAD(key, chacha)
	if err != nil {
		t.Fatal(err)
	}
	plaintext = append(bytes.Clone(plaintext), kind)
	header := []byte{23, 3, 3, 0, 0}
	binary.BigEndian.PutUint16(header[3:], uint16(len(plaintext)+aead.Overhead()))
	return aead.Seal(header, nonce, plaintext, header)
}
func TestTLSKeylogTLS13KeyUpdate(t *testing.T) {
	for _, suite := range []uint16{0x1301, 0x1302, 0x1303} {
		t.Run(fmt.Sprintf("%04x", suite), func(t *testing.T) {
			hashFunc, _, _, _ := tlsKeylogSuite(suite)
			secret := bytes.Repeat([]byte{42}, hashFunc().Size())
			random := [32]byte{1}
			log := &tlsKeylog{keys: map[tlsKeylogKey][]byte{{random, "CLIENT_TRAFFIC_SECRET_0"}: secret}}
			decoder := newTLSKeylogDecryptor(log, nil)
			connection := &tlsKeylogConnection{client: 1, haveClient: true, random: random, version: 0x0304, suite: suite}
			connection.directions[1].application = true
			connection.directions[1].sequence = 5
			record := tlsKeylogTestRecord(t, suite, secret, 5, 22, []byte{24, 0, 0, 1, 0})
			plaintext, kind, ready, err := decoder.decrypt(connection, 1, record)
			if err != nil || !ready || kind != 22 {
				t.Fatalf("update: ready=%t kind=%d err=%v", ready, kind, err)
			}
			if err := decoder.handshake(connection, 1, plaintext); err != nil {
				t.Fatal(err)
			}
			next, err := tlsExpandLabel(hashFunc, secret, "traffic upd", hashFunc().Size())
			if err != nil {
				t.Fatal(err)
			}
			record = tlsKeylogTestRecord(t, suite, next, 0, 23, []byte("secure after update"))
			plaintext, kind, ready, err = decoder.decrypt(connection, 1, record)
			if err != nil || !ready || kind != 23 || string(plaintext) != "secure after update" {
				t.Fatalf("after update: %q ready=%t kind=%d err=%v", plaintext, ready, kind, err)
			}
		})
	}
}

func TestTLSKeylogWholeSecretAppendPreservesRequestOrder(t *testing.T) {
	packets, keys := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, false)
	path := filepath.Join(t.TempDir(), "fixture.keys")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	keylog, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer keylog.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(keylog, tracker)
	for _, packet := range packets {
		tracker.add(packet, time.Now())
	}
	if tracker.tls.pending == 0 {
		t.Fatal("encrypted records were not retained")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(keys); err != nil {
		t.Fatal(err)
	}
	file.Close()
	events := tracker.tls.refresh(time.Now())
	events = append(events, tracker.sweep(time.Now(), true)...)
	if tracker.tls.err != nil {
		t.Fatal(tracker.tls.err)
	}
	response := false
	for _, event := range events {
		response = response || event.TLS && event.Status == 200 && event.Method == "GET" && event.Path == "/secure" && event.LatencyMS != nil
	}
	if !response || tracker.tls.pending != 0 || tracker.tls.buffered != 0 {
		t.Fatalf("response=%t pending=%d events=%#v", response, tracker.tls.pending, events)
	}
}

func TestTLSKeylogBoundsIncompleteHandshakesGlobally(t *testing.T) {
	d := newTLSKeylogDecryptor(nil, nil)
	payload := make([]byte, 65540)
	payload[0], payload[1], payload[2] = 22, 3, 3
	binary.BigEndian.PutUint16(payload[3:5], 65535)
	payload[5], payload[6], payload[7], payload[8] = 1, 15, 255, 252
	for socket := uint64(1); socket <= 300; socket++ {
		d.record(httpPacket{socket: socket, payload: payload}, time.Now())
		if d.err != nil {
			if !strings.Contains(d.err.Error(), "TLS buffered data exceeds") || d.buffered > tlsKeylogPendingLimit {
				t.Fatalf("buffered=%d error=%v", d.buffered, d.err)
			}
			d.tracker = newPKTAPHTTPTracker(traceScope{}, 0)
			for socket := range d.connections {
				d.forget(socket, time.Now())
			}
			if d.buffered != 0 || d.pending != 0 {
				t.Fatalf("cleanup buffered=%d pending=%d", d.buffered, d.pending)
			}
			return
		}
	}
	t.Fatal("incomplete TLS handshakes exceeded the global buffer limit")
}

func TestTLSKeylogQueuedPaddingAccounting(t *testing.T) {
	secret := bytes.Repeat([]byte{42}, 32)
	random := [32]byte{1}
	log := &tlsKeylog{keys: map[tlsKeylogKey][]byte{{random, "CLIENT_TRAFFIC_SECRET_0"}: secret}}
	d := newTLSKeylogDecryptor(log, nil)
	c := &tlsKeylogConnection{client: 1, haveClient: true, random: random, version: 0x0304, suite: 0x1301}
	c.directions[0].application = true
	c.directions[1].application = true
	d.connections[1] = c
	d.record(httpPacket{socket: 1, payload: []byte{23, 3, 3, 0, 1, 0}}, time.Now())
	padded := make([]byte, 16383)
	padded[0], padded[1] = 'x', 23
	record := tlsKeylogTestRecord(t, 0x1301, secret, 0, 0, padded)
	d.record(httpPacket{socket: 1, sent: true, payload: record}, time.Now())
	if d.err != nil {
		t.Fatal(d.err)
	}
	if len(c.directions[1].plaintext) != 1 {
		t.Fatalf("queued=%d", len(c.directions[1].plaintext))
	}
	if string(c.directions[1].plaintext[0].packet.payload) != "x" {
		t.Fatalf("plaintext=%q", c.directions[1].plaintext[0].packet.payload)
	}
	retained := cap(c.directions[1].plaintext[0].packet.payload)
	if d.buffered-d.pending < retained {
		t.Fatalf("plaintext charged=%d, retained allocation capacity=%d", d.buffered-d.pending, retained)
	}
}

func TestTLSKeylogQueuedPaddingEnforcesGlobalLimit(t *testing.T) {
	secret := bytes.Repeat([]byte{42}, 32)
	random := [32]byte{1}
	log := &tlsKeylog{keys: map[tlsKeylogKey][]byte{{random, "CLIENT_TRAFFIC_SECRET_0"}: secret}}
	d := newTLSKeylogDecryptor(log, nil)
	c := &tlsKeylogConnection{client: 1, haveClient: true, random: random, version: 0x0304, suite: 0x1301}
	c.directions[0].application = true
	c.directions[1].application = true
	d.connections[1] = c
	now := time.Now()
	d.record(httpPacket{socket: 1, payload: []byte{23, 3, 3, 0, 1, 0}}, now)
	padded := make([]byte, 16383)
	padded[0], padded[1] = 'x', 23
	for sequence := uint64(0); sequence < 1100; sequence++ {
		record := tlsKeylogTestRecord(t, 0x1301, secret, sequence, 0, padded)
		d.record(httpPacket{socket: 1, sent: true, payload: record}, now)
		if d.err != nil {
			if !strings.Contains(d.err.Error(), "TLS buffered data exceeds") || d.buffered > tlsKeylogPendingLimit {
				t.Fatalf("buffered=%d error=%v", d.buffered, d.err)
			}
			d.tracker = newPKTAPHTTPTracker(traceScope{}, 0)
			d.forget(1, now)
			if d.buffered != 0 || d.pending != 0 {
				t.Fatalf("cleanup buffered=%d pending=%d", d.buffered, d.pending)
			}
			return
		}
	}
	t.Fatal("padded plaintext exceeded the global buffer limit")
}

func BenchmarkTLSKeylogBlockedPlaintextQueue(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			d := newTLSKeylogDecryptor(nil, nil)
			c := &tlsKeylogConnection{nextOrder: uint64(count + 1)}
			c.directions[0].pending = []tlsWireRecord{{order: 0, data: []byte{23, 3, 3, 0, 1, 0}}}
			for i := 0; i < count; i++ {
				c.directions[1].plaintext = append(c.directions[1].plaintext, tlsPlainRecord{order: uint64(i + 1)})
			}
			now := time.Now()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				d.drain(c, now)
			}
		})
	}
}

func TestTLSKeylogEarlyDataDoesNotStopOtherConnections(t *testing.T) {
	packets, keys := tlsKeylogTestCapture(t, tls.VersionTLS13, 0, false)
	path := filepath.Join(t.TempDir(), "fixture.log")
	if err := os.WriteFile(path, keys, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := openTLSKeylog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	tracker.tls = newTLSKeylogDecryptor(log, tracker)
	body := make([]byte, 34)
	body[0], body[1] = 3, 3
	body = append(body, 0, 0, 2, 0x13, 1, 1, 0, 0, 4, 0, 42, 0, 0)
	hello := append([]byte{1, 0, 0, byte(len(body))}, body...)
	wire := append([]byte{22, 3, 3, 0, byte(len(hello))}, hello...)
	d := tracker.tls
	d.record(httpPacket{socket: 9999, sent: true, payload: wire}, time.Now())
	d.connections[9999].version = tls.VersionTLS13
	d.connections[9999].suite = tls.TLS_AES_128_GCM_SHA256
	d.log.keys[tlsKeylogKey{label: "CLIENT_HANDSHAKE_TRAFFIC_SECRET"}] = bytes.Repeat([]byte{2}, 32)
	early := tlsKeylogTestRecord(t, tls.TLS_AES_128_GCM_SHA256, bytes.Repeat([]byte{1}, 32), 0, 23, []byte("early"))
	d.record(httpPacket{socket: 9999, sent: true, payload: early}, time.Now())
	if d.err != nil || d.unmapped == 0 || d.pending != 0 {
		t.Fatalf("early data blocked capture: err=%v unmapped=%d pending=%d", d.err, d.unmapped, d.pending)
	}
	var events []captureEvent
	for _, packet := range packets {
		events = append(events, tracker.add(packet, time.Now())...)
	}
	events = append(events, tracker.sweep(time.Now(), true)...)
	request, response := false, false
	for _, event := range events {
		if event.TLS && event.Path == "/secure" {
			request = request || event.Method == "GET" && event.Status == 0
			response = response || event.Status == 200
		}
	}
	if d.err != nil || !request || !response {
		t.Fatalf("healthy connection failed: request=%t response=%t err=%v", request, response, d.err)
	}
	d.forget(9999, time.Now())
	if d.buffered != 0 || d.queued != 0 {
		t.Fatalf("early data retained buffers: bytes=%d records=%d", d.buffered, d.queued)
	}
}

func TestTLSKeylogBoundsTinyRecordMetadata(t *testing.T) {
	tracker := newPKTAPHTTPTracker(traceScope{}, 0)
	d := newTLSKeylogDecryptor(nil, tracker)
	for i := 0; i <= tlsKeylogQueueLimit; i++ {
		d.record(httpPacket{socket: 1, sent: true, payload: []byte{23, 3, 3, 0, 0}}, time.Now())
	}
	if d.err == nil {
		t.Fatal("tiny TLS records bypassed the queue metadata bound")
	}
	if d.buffered <= d.pending || d.queued != tlsKeylogQueueLimit {
		t.Fatalf("queue metadata was not charged: bytes=%d pending=%d records=%d", d.buffered, d.pending, d.queued)
	}
	d.forget(1, time.Now())
	if d.buffered != 0 || d.pending != 0 || d.queued != 0 {
		t.Fatalf("queue cleanup buffered=%d pending=%d", d.buffered, d.pending)
	}
}

func TestTLSKeylogRefreshPropagatesTruncationAndSizeErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    int64
		message string
	}{{"truncate", 0, "truncated"}, {"oversize", tlsKeylogFileLimit + 1, "exceeds"}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.log")
			if err := os.WriteFile(path, []byte("# fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			log, err := openTLSKeylog(path)
			if err != nil {
				t.Fatal(err)
			}
			defer log.close()
			if err := os.Truncate(path, test.size); err != nil {
				t.Fatal(err)
			}
			d := newTLSKeylogDecryptor(log, newPKTAPHTTPTracker(traceScope{}, 0))
			if events := d.refresh(time.Now()); len(events) != 0 || d.err == nil || !strings.Contains(d.err.Error(), test.message) {
				t.Fatalf("refresh error=%v events=%d", d.err, len(events))
			}
		})
	}
}
