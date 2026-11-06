package edc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const tlsKeylogFileLimit = 64 << 20
const tlsKeylogEntryLimit = 65536
const tlsKeylogPendingLimit = 16 << 20

type tlsKeylogKey struct {
	random [32]byte
	label  string
}
type tlsKeylog struct {
	file    *os.File
	keys    map[tlsKeylogKey][]byte
	partial []byte
	read    int64
}

func openTLSKeylog(path string) (*tlsKeylog, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open TLS key log: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("TLS key log must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open TLS key log: %w", err)
	}
	log := &tlsKeylog{file: file, keys: map[tlsKeylogKey][]byte{}}
	if err := log.refresh(); err != nil {
		log.close()
		return nil, err
	}
	return log, nil
}
func (l *tlsKeylog) close() {
	if l.file != nil {
		_ = l.file.Close()
	}
	for _, key := range l.keys {
		clear(key)
	}
	clear(l.partial)
	clear(l.keys)
}
func (l *tlsKeylog) line(line []byte) error {
	fields := bytes.Fields(line)
	if len(fields) == 0 || fields[0][0] == '#' {
		return nil
	}
	label := string(fields[0])
	switch label {
	case "CLIENT_RANDOM", "CLIENT_HANDSHAKE_TRAFFIC_SECRET", "SERVER_HANDSHAKE_TRAFFIC_SECRET", "CLIENT_TRAFFIC_SECRET_0", "SERVER_TRAFFIC_SECRET_0":
	default:
		return nil
	}
	if len(fields) != 3 || len(fields[1]) != 64 {
		return fmt.Errorf("invalid TLS key log entry")
	}
	var random [32]byte
	if _, err := hex.Decode(random[:], fields[1]); err != nil {
		return fmt.Errorf("invalid TLS client random in key log")
	}
	if len(fields[2]) != 64 && len(fields[2]) != 96 {
		return fmt.Errorf("invalid TLS secret length in key log")
	}
	if label == "CLIENT_RANDOM" && len(fields[2]) != 96 {
		return fmt.Errorf("invalid TLS 1.2 master secret length")
	}
	secret, err := hex.DecodeString(string(fields[2]))
	if err != nil {
		return fmt.Errorf("invalid TLS secret in key log")
	}
	key := tlsKeylogKey{random, label}
	if previous, ok := l.keys[key]; ok {
		if !bytes.Equal(previous, secret) {
			clear(secret)
			return fmt.Errorf("conflicting TLS key log entries")
		}
		clear(secret)
		return nil
	}
	if len(l.keys) >= tlsKeylogEntryLimit {
		clear(secret)
		return fmt.Errorf("TLS key log exceeds %d entries", tlsKeylogEntryLimit)
	}
	l.keys[key] = secret
	return nil
}
func (l *tlsKeylog) refresh() error {
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > tlsKeylogFileLimit {
		return fmt.Errorf("TLS key log exceeds %d bytes", tlsKeylogFileLimit)
	}
	if info.Size() < l.read {
		return fmt.Errorf("TLS key log was truncated during capture")
	}
	var buffer [32768]byte
	for {
		n, err := l.file.Read(buffer[:])
		l.read += int64(n)
		if l.read > tlsKeylogFileLimit {
			return fmt.Errorf("TLS key log exceeds capture limit")
		}
		l.partial = append(l.partial, buffer[:n]...)
		for {
			at := bytes.IndexByte(l.partial, '\n')
			if at < 0 {
				break
			}
			if at > 2048 {
				return fmt.Errorf("TLS key log entry is too long")
			}
			if err := l.line(l.partial[:at]); err != nil {
				return err
			}
			l.partial = l.partial[at+1:]
		}
		if len(l.partial) > 2048 {
			return fmt.Errorf("TLS key log entry is too long")
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read TLS key log: %w", err)
		}
	}
}

type tlsWireRecord struct {
	packet httpPacket
	data   []byte
}
type tlsKeylogDirection struct {
	pending                []tlsWireRecord
	encrypted, application bool
	sequence               uint64
	aead                   cipher.AEAD
	iv, secret, handshake  []byte
	plainOffset            uint32
}
type tlsKeylogConnection struct {
	client               int
	haveClient           bool
	random, serverRandom [32]byte
	suite                uint16
	version              uint16
	directions           [2]tlsKeylogDirection
	updated              time.Time
}
type tlsKeylogDecryptor struct {
	log         *tlsKeylog
	tracker     *pktapHTTPTracker
	connections map[uint64]*tlsKeylogConnection
	pending     int
	unmapped    uint64
	err         error
}

func newTLSKeylogDecryptor(log *tlsKeylog, tracker *pktapHTTPTracker) *tlsKeylogDecryptor {
	return &tlsKeylogDecryptor{log: log, tracker: tracker, connections: map[uint64]*tlsKeylogConnection{}}
}
func (d *tlsKeylogDecryptor) record(packet httpPacket, now time.Time) []captureEvent {
	if d.err != nil {
		return nil
	}
	connection := d.connections[packet.socket]
	if connection == nil {
		connection = &tlsKeylogConnection{}
		d.connections[packet.socket] = connection
	}
	connection.updated = now
	index := 0
	if packet.sent {
		index = 1
	}
	direction := &connection.directions[index]
	if d.pending+len(packet.payload) > tlsKeylogPendingLimit {
		d.err = fmt.Errorf("TLS records awaiting keys exceed %d bytes", tlsKeylogPendingLimit)
		return nil
	}
	direction.pending = append(direction.pending, tlsWireRecord{packet: packet, data: bytes.Clone(packet.payload)})
	direction.pending[len(direction.pending)-1].packet.payload = nil
	d.pending += len(packet.payload)
	return d.drain(connection, now)
}
func (d *tlsKeylogDecryptor) drain(c *tlsKeylogConnection, now time.Time) []captureEvent {
	var events []captureEvent
	for progress := true; progress && d.err == nil; {
		progress = false
		for i := range c.directions {
			direction := &c.directions[i]
			for len(direction.pending) > 0 {
				record := direction.pending[0]
				plaintext, kind, ready, err := d.decrypt(c, i, record.data)
				if err != nil {
					d.err = fmt.Errorf("decrypt TLS record for PID %d: %w", record.packet.pid, err)
					return events
				}
				if !ready {
					break
				}
				direction.pending[0] = tlsWireRecord{}
				direction.pending = direction.pending[1:]
				d.pending -= len(record.data)
				progress = true
				if kind == 22 {
					if err := d.handshake(c, i, plaintext); err != nil {
						d.err = err
						return events
					}
				}
				if kind == 23 && len(plaintext) > 0 {
					packet := pktapTCPPacket{httpPacket: record.packet, sequence: direction.plainOffset, tlsObject: record.packet.socket}
					packet.decrypted = true
					packet.payload = plaintext
					direction.plainOffset += uint32(len(plaintext))
					events = append(events, d.tracker.add(packet, now)...)
				}
			}
		}
	}
	return events
}
func (d *tlsKeylogDecryptor) handshake(c *tlsKeylogConnection, index int, data []byte) error {
	direction := &c.directions[index]
	if len(direction.handshake)+len(data) > httpMessageMax {
		return fmt.Errorf("TLS handshake exceeds capture limit")
	}
	direction.handshake = append(direction.handshake, data...)
	for len(direction.handshake) >= 4 {
		message := direction.handshake
		n := 4 + (int(message[1])<<16 | int(message[2])<<8 | int(message[3]))
		if n > httpMessageMax {
			return fmt.Errorf("TLS handshake exceeds capture limit")
		}
		if n > len(message) {
			break
		}
		body := message[4:n]
		switch message[0] {
		case 1:
			if len(body) < 34 {
				return fmt.Errorf("invalid TLS ClientHello")
			}
			if c.haveClient && c.random != [32]byte(body[2:34]) {
				return fmt.Errorf("TLS renegotiation is not supported")
			}
			c.client, c.haveClient = index, true
			copy(c.random[:], body[2:34])
		case 2:
			if len(body) < 38 {
				return fmt.Errorf("invalid TLS ServerHello")
			}
			copy(c.serverRandom[:], body[2:34])
			at := 35 + int(body[34])
			if at+3 > len(body) {
				return fmt.Errorf("invalid TLS ServerHello session")
			}
			c.suite = binary.BigEndian.Uint16(body[at:])
			c.version = binary.BigEndian.Uint16(body)
			extensions := body[at+3:]
			if len(extensions) >= 2 {
				size := int(binary.BigEndian.Uint16(extensions))
				extensions = extensions[2:]
				if size > len(extensions) {
					return fmt.Errorf("invalid TLS ServerHello extensions")
				}
				extensions = extensions[:size]
				for len(extensions) >= 4 {
					kind, size := binary.BigEndian.Uint16(extensions), int(binary.BigEndian.Uint16(extensions[2:]))
					extensions = extensions[4:]
					if size > len(extensions) {
						return fmt.Errorf("invalid TLS ServerHello extension")
					}
					if kind == 43 && size == 2 {
						c.version = binary.BigEndian.Uint16(extensions)
					}
					extensions = extensions[size:]
				}
			}
		case 20:
			if c.version == 0x0304 {
				direction.application = true
				direction.sequence = 0
				direction.aead = nil
				clear(direction.secret)
				direction.secret = nil
			}
		case 24:
			if c.version == 0x0304 && direction.application {
				if len(body) != 1 || body[0] > 1 {
					return fmt.Errorf("invalid TLS KeyUpdate")
				}
				hashFunc, _, _, err := tlsKeylogSuite(c.suite)
				if err != nil {
					return err
				}
				secret, err := tlsExpandLabel(hashFunc, direction.secret, "traffic upd", hashFunc().Size())
				if err != nil {
					return err
				}
				clear(direction.secret)
				direction.secret = secret
				direction.aead = nil
				direction.sequence = 0
			}
		}
		direction.handshake = direction.handshake[n:]
	}
	if len(direction.handshake) == 0 {
		direction.handshake = nil
	}
	return nil
}
func tlsKeylogSuite(suite uint16) (func() hash.Hash, int, bool, error) {
	switch suite {
	case 0x1301, 0xc02f, 0xc02b, 0x009c:
		return sha256.New, 16, false, nil
	case 0x1302, 0xc030, 0xc02c, 0x009d:
		return sha512.New384, 32, false, nil
	case 0x1303, 0xcca8, 0xcca9, 0xccaa:
		return sha256.New, 32, true, nil
	default:
		return nil, 0, false, fmt.Errorf("TLS cipher suite 0x%04x is not supported", suite)
	}
}
func tlsExpandLabel(hashFunc func() hash.Hash, secret []byte, label string, size int) ([]byte, error) {
	label = "tls13 " + label
	info := make([]byte, 0, 4+len(label))
	info = binary.BigEndian.AppendUint16(info, uint16(size))
	info = append(info, byte(len(label)))
	info = append(info, label...)
	info = append(info, 0)
	return hkdf.Expand(hashFunc, secret, string(info), size)
}
func tlsKeylogPRF(hashFunc func() hash.Hash, secret, seed []byte, size int) []byte {
	mac := func(data []byte) []byte { h := hmac.New(hashFunc, secret); _, _ = h.Write(data); return h.Sum(nil) }
	a := mac(seed)
	var result []byte
	for len(result) < size {
		result = append(result, mac(append(bytes.Clone(a), seed...))...)
		a = mac(a)
	}
	return result[:size]
}
func tlsKeylogAEAD(key []byte, chacha bool) (cipher.AEAD, error) {
	if chacha {
		return chacha20poly1305.New(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (d *tlsKeylogDecryptor) decrypt(c *tlsKeylogConnection, index int, record []byte) ([]byte, byte, bool, error) {
	if len(record) < 5 || int(binary.BigEndian.Uint16(record[3:])) != len(record)-5 {
		return nil, 0, false, fmt.Errorf("invalid TLS record length")
	}
	direction := &c.directions[index]
	kind := record[0]
	body := record[5:]
	if kind == 20 {
		if c.version != 0x0304 {
			direction.encrypted = true
			direction.sequence = 0
			direction.aead = nil
		}
		return nil, kind, true, nil
	}
	encrypted := direction.encrypted || c.version == 0x0304 && kind == 23
	if !encrypted {
		if kind == 23 {
			return nil, 0, false, nil
		}
		return body, kind, true, nil
	}
	if !c.haveClient || c.suite == 0 {
		return nil, 0, false, nil
	}
	label := "CLIENT_RANDOM"
	if c.version == 0x0304 {
		label = "CLIENT_HANDSHAKE_TRAFFIC_SECRET"
		if direction.application {
			label = "CLIENT_TRAFFIC_SECRET_0"
		}
		if index != c.client {
			label = strings.Replace(label, "CLIENT_", "SERVER_", 1)
		}
	}
	secret := direction.secret
	if len(secret) == 0 {
		secret = d.log.keys[tlsKeylogKey{c.random, label}]
	}
	if len(secret) == 0 {
		return nil, 0, false, nil
	}
	hashFunc, keySize, chacha, err := tlsKeylogSuite(c.suite)
	if err != nil {
		return nil, 0, false, err
	}
	if direction.aead == nil {
		var key, iv []byte
		if c.version == 0x0304 {
			if len(secret) != hashFunc().Size() {
				return nil, 0, false, fmt.Errorf("TLS traffic secret length does not match cipher suite")
			}
			key, err = tlsExpandLabel(hashFunc, secret, "key", keySize)
			if err != nil {
				return nil, 0, false, err
			}
			iv, err = tlsExpandLabel(hashFunc, secret, "iv", 12)
			if err != nil {
				return nil, 0, false, err
			}
			if len(direction.secret) == 0 {
				direction.secret = bytes.Clone(secret)
			}
		} else {
			ivSize := 4
			if chacha {
				ivSize = 12
			}
			seed := append([]byte("key expansion"), c.serverRandom[:]...)
			seed = append(seed, c.random[:]...)
			block := tlsKeylogPRF(hashFunc, secret, seed, 2*(keySize+ivSize))
			side := 0
			if index != c.client {
				side = 1
			}
			key = bytes.Clone(block[side*keySize : (side+1)*keySize])
			iv = bytes.Clone(block[2*keySize+side*ivSize : 2*keySize+(side+1)*ivSize])
			clear(block)
		}
		direction.aead, err = tlsKeylogAEAD(key, chacha)
		clear(key)
		if err != nil {
			return nil, 0, false, err
		}
		direction.iv = iv
	}
	var nonce, additional []byte
	ciphertext := body
	if c.version == 0x0304 || chacha {
		nonce = bytes.Clone(direction.iv)
		for at := 0; at < 8; at++ {
			nonce[len(nonce)-1-at] ^= byte(direction.sequence >> (8 * at))
		}
	} else {
		if len(body) < 8 {
			return nil, 0, false, fmt.Errorf("truncated TLS explicit nonce")
		}
		nonce = append(bytes.Clone(direction.iv), body[:8]...)
		ciphertext = body[8:]
	}
	if c.version == 0x0304 {
		additional = record[:5]
	} else {
		n := len(ciphertext) - direction.aead.Overhead()
		if n < 0 {
			return nil, 0, false, fmt.Errorf("truncated TLS ciphertext")
		}
		additional = binary.BigEndian.AppendUint64(nil, direction.sequence)
		additional = append(additional, record[:3]...)
		additional = binary.BigEndian.AppendUint16(additional, uint16(n))
	}
	plaintext, err := direction.aead.Open(nil, nonce, ciphertext, additional)
	if err != nil {
		return nil, 0, false, fmt.Errorf("TLS record authentication failed; capture must include the complete handshake and TCP stream")
	}
	direction.sequence++
	if c.version == 0x0304 {
		at := len(plaintext) - 1
		for at >= 0 && plaintext[at] == 0 {
			at--
		}
		if at < 0 {
			return nil, 0, false, fmt.Errorf("TLS inner content type is missing")
		}
		kind = plaintext[at]
		if kind != 21 && kind != 22 && kind != 23 {
			return nil, 0, false, fmt.Errorf("invalid TLS inner content type")
		}
		plaintext = plaintext[:at]
	}
	return plaintext, kind, true, nil
}
func (d *tlsKeylogDecryptor) refresh(now time.Time) []captureEvent {
	if d.err != nil {
		return nil
	}
	if err := d.log.refresh(); err != nil {
		d.err = err
		return nil
	}
	var events []captureEvent
	for _, connection := range d.connections {
		events = append(events, d.drain(connection, now)...)
	}
	return events
}
func (d *tlsKeylogDecryptor) forget(socket uint64, now time.Time) []captureEvent {
	c := d.connections[socket]
	if c == nil {
		return nil
	}
	var events []captureEvent
	if d.err == nil {
		if err := d.log.refresh(); err != nil {
			d.err = err
		}
	}
	events = append(events, d.drain(c, now)...)
	for i := range c.directions {
		direction := &c.directions[i]
		d.unmapped += uint64(len(direction.pending))
		for _, record := range direction.pending {
			d.pending -= len(record.data)
		}
		clear(direction.secret)
		clear(direction.iv)
		clear(direction.handshake)
	}
	delete(d.connections, socket)
	for key, flow := range d.tracker.flows {
		if key.tlsObject == socket {
			events = append(events, d.tracker.closeFlow(key, flow, now)...)
		}
	}
	return events
}
