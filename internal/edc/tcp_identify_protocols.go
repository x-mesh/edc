package edc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// 이 파일의 탐침은 identifyStages의 goloop 뒤에 온다. 모두 30바이트보다 짧아 goloop 앞에 두면 goloop가 header를 기다리며
// 단계마다 멈추기 때문이다. 순서는 앞 탐침의 바이트를 길이로 읽는 서버를 멈추지 않게 정했다. Cassandra OPTIONS의 앞 4바이트는
// 약 64MB로 읽혀 Kafka가 그만큼 기다리므로 Cassandra를 Kafka 뒤에 둔다.

// identifyH2C는 TLS 없는 HTTP/2(prior knowledge)로 gRPC health check를 보낸다. 평문 gRPC 서버를 찾는다.
func identifyH2C(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) { return connection, nil },
		Protocols:   &protocols,
	}
	defer transport.CloseIdleConnections()
	match, ok := grpcHealthRoundTrip(ctx, transport, "http://"+target.address)
	if !ok {
		return protocolIdentification{}, nil, false
	}
	if match.Name == "https" {
		match.Name = "h2c"
	}
	match.Basis = protocolBasisReply
	return match, nil, true
}

// isHTTP2SettingsFrame은 서버가 먼저 보낸 바이트가 HTTP/2 SETTINGS frame인지 본다. 9바이트 header의 type이 4이고, stream이
// 0이며, 길이가 설정 하나(6바이트)의 배수다.
func isHTTP2SettingsFrame(first []byte) bool {
	if len(first) < 9 || first[3] != 0x04 || first[4]&^0x01 != 0 || binary.BigEndian.Uint32(first[5:9]) != 0 {
		return false
	}
	length := int(first[0])<<16 | int(first[1])<<8 | int(first[2])
	return length%6 == 0 && length <= 16384
}

// waitWatchConn은 읽기가 답 없이 시간을 다 썼는지 기록하는 연결이다. h2c 탐침은 http.Transport가 다른 goroutine에서 읽으므로
// atomic으로 남긴다.
type waitWatchConn struct {
	net.Conn
	waited *atomic.Bool
}

func (connection *waitWatchConn) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	var timeout net.Error
	if count == 0 && errors.As(err, &timeout) && timeout.Timeout() {
		connection.waited.Store(true)
	}
	return count, err
}

// amqpProtocolHeader는 AMQP 0-9-1 client가 처음 보내는 8바이트다.
var amqpProtocolHeader = []byte("AMQP\x00\x00\x09\x01")

const (
	amqpFrameMethod = 1
	amqpFrameEnd    = 0xce
	amqpFrameMax    = 64 << 10 // Connection.Start는 1KB 안팎이다.
)

// identifyAMQP는 AMQP 0-9-1 protocol header를 보낸다. broker는 Connection.Start로 답하고 그 server-properties에 제품과 버전을
// 담는다. 다른 버전만 받는 broker(AMQP 1.0)는 자기 protocol header를 보내고 끊는데, 그 응답은 matchBanner가 맞춘다.
func identifyAMQP(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	if _, err := connection.Write(amqpProtocolHeader); err != nil {
		return protocolIdentification{}, nil, false
	}
	header := make([]byte, 7)
	count, err := io.ReadFull(connection, header)
	if err != nil {
		return protocolIdentification{}, header[:count], false
	}
	size := binary.BigEndian.Uint32(header[3:7])
	if header[0] != amqpFrameMethod || binary.BigEndian.Uint16(header[1:3]) != 0 || size < 4 || size > amqpFrameMax {
		return protocolIdentification{}, header, false
	}
	frame := make([]byte, int(size)+1)
	if _, err := io.ReadFull(connection, frame); err != nil || frame[size] != amqpFrameEnd {
		return protocolIdentification{}, header, false
	}
	// method payload: class 10(connection), method 10(start), version major, minor, server-properties table.
	payload := frame[:size]
	if binary.BigEndian.Uint16(payload[0:2]) != 10 || binary.BigEndian.Uint16(payload[2:4]) != 10 {
		return protocolIdentification{}, header, false
	}
	detail := "AMQP 0-9-1"
	if len(payload) >= 10 {
		properties := amqpTableStrings(payload[6:])
		if product := properties["product"]; product != "" {
			detail = strings.TrimSpace(product + " " + properties["version"])
		}
	}
	return protocolIdentification{Name: "amqp", Detail: detail, Basis: protocolBasisReply}, nil, true
}

// amqpTableStrings는 AMQP field table에서 long string 값만 읽는다. 다른 형식은 건너뛰고, 모르는 형식을 만나면 거기서 멈춘다.
func amqpTableStrings(data []byte) map[string]string {
	values := map[string]string{}
	if len(data) < 4 {
		return values
	}
	size := int(binary.BigEndian.Uint32(data[0:4]))
	if size > len(data)-4 {
		return values
	}
	table := data[4 : 4+size]
	fixed := map[byte]int{'t': 1, 'b': 1, 'B': 1, 's': 2, 'u': 2, 'I': 4, 'i': 4, 'l': 8, 'L': 8, 'f': 4, 'd': 8, 'D': 5, 'T': 8, 'V': 0}
	for len(table) > 0 {
		nameLength := int(table[0])
		if 1+nameLength+1 > len(table) {
			return values
		}
		name := string(table[1 : 1+nameLength])
		kind := table[1+nameLength]
		table = table[2+nameLength:]
		var width int
		switch kind {
		case 'S', 'x', 'F', 'A':
			if len(table) < 4 {
				return values
			}
			width = 4 + int(binary.BigEndian.Uint32(table[0:4]))
			if width > len(table) {
				return values
			}
			if kind == 'S' {
				values[name] = string(table[4:width])
			}
		default:
			known := false
			if width, known = fixed[kind]; !known || width > len(table) {
				return values
			}
		}
		table = table[width:]
	}
	return values
}

// amqpHeaderDetail은 broker가 거절하며 보낸 protocol header의 버전이다. AMQP 0-9-1은 "AMQP" 0 0 9 1, AMQP 1.0은
// "AMQP" protocol-id 1 0 0이다(protocol-id 0은 AMQP, 2는 TLS, 3은 SASL). broker가 제시한 버전일 뿐 지원하는 버전 전부는
// 아니다. RabbitMQ 4는 0-9-1도 받지만 거절할 때는 1.0 header를 보냈다.
func amqpHeaderDetail(header []byte) (string, bool) {
	if len(header) < 8 || !bytes.HasPrefix(header, []byte("AMQP")) {
		return "", false
	}
	switch {
	case header[4] == 0 && header[5] == 0:
		return fmt.Sprintf("offers AMQP %d-%d-%d", header[5], header[6], header[7]), true
	case header[5] == 1 && header[6] == 0 && header[7] == 0 && header[4] <= 3:
		return "offers AMQP 1.0", true
	}
	return "", false
}

// kafkaCorrelationID는 ApiVersions 요청의 상관 번호다. 응답이 이 번호를 되돌려 주는지로 Kafka를 가린다.
const (
	kafkaCorrelationID = 0x65646301
	kafkaReplyMax      = 64 << 10
)

// kafkaAPIVersionsRequest는 ApiVersions v0 요청이다. 인증 전에 받는 요청이고, 지원하지 않는 버전이면 broker는
// UNSUPPORTED_VERSION(35)을 v0 형식으로 돌려준다.
func kafkaAPIVersionsRequest() []byte {
	body := binary.BigEndian.AppendUint16(nil, 18) // api_key ApiVersions
	body = binary.BigEndian.AppendUint16(body, 0)  // api_version
	body = binary.BigEndian.AppendUint32(body, kafkaCorrelationID)
	body = binary.BigEndian.AppendUint16(body, uint16(len("edc")))
	body = append(body, "edc"...)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

func identifyKafka(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	if _, err := connection.Write(kafkaAPIVersionsRequest()); err != nil {
		return protocolIdentification{}, nil, false
	}
	header := make([]byte, 8)
	count, err := io.ReadFull(connection, header)
	if err != nil {
		return protocolIdentification{}, header[:count], false
	}
	size := binary.BigEndian.Uint32(header[0:4])
	if binary.BigEndian.Uint32(header[4:8]) != kafkaCorrelationID || size < 6 || size > kafkaReplyMax {
		return protocolIdentification{}, header, false
	}
	body := make([]byte, size-4)
	if _, err := io.ReadFull(connection, body); err != nil {
		return protocolIdentification{}, header, false
	}
	errorCode := int16(binary.BigEndian.Uint16(body[0:2]))
	detail := fmt.Sprintf("ApiVersions error %d", errorCode)
	if errorCode == 0 && len(body) >= 6 {
		detail = fmt.Sprintf("ApiVersions, %d APIs", binary.BigEndian.Uint32(body[2:6]))
	}
	return protocolIdentification{Name: "kafka", Detail: detail, Basis: protocolBasisReply}, nil, true
}

// mqttConnectReturnCodes는 MQTT 3.1.1 CONNACK의 return code다.
var mqttConnectReturnCodes = map[byte]string{
	0: "accepted", 1: "unacceptable protocol version", 2: "identifier rejected", 3: "server unavailable",
	4: "bad user name or password", 5: "not authorized",
}

// mqttConnectPacket은 user 이름 없이 보내는 MQTT 3.1.1 CONNECT다. client id는 실행마다 새로 만든다. 같은 id의 client가
// 이미 붙어 있으면 broker가 그 client를 끊기 때문이다.
func mqttConnectPacket() []byte {
	suffix := make([]byte, 4)
	rand.Read(suffix)
	clientID := "edc-" + hex.EncodeToString(suffix)
	variable := []byte{0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04, 0x02, 0x00, 0x3c} // protocol name, level 4, clean session, keep alive 60s
	payload := binary.BigEndian.AppendUint16(nil, uint16(len(clientID)))
	payload = append(payload, clientID...)
	return append([]byte{0x10, byte(len(variable) + len(payload))}, append(variable, payload...)...)
}

// identifyMQTT는 CONNECT를 보내고 CONNACK을 읽는다. broker가 받으면 실제 client session이 열리므로 바로 DISCONNECT를 보낸다.
func identifyMQTT(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	if _, err := connection.Write(mqttConnectPacket()); err != nil {
		return protocolIdentification{}, nil, false
	}
	reply := make([]byte, 4)
	count, err := io.ReadFull(connection, reply)
	if err != nil {
		return protocolIdentification{}, reply[:count], false
	}
	// CONNACK: type 2, remaining length 2(3.1.1), session present flags, return code.
	if reply[0] != 0x20 || reply[1] != 0x02 || reply[2] > 1 {
		return protocolIdentification{}, reply, false
	}
	text, known := mqttConnectReturnCodes[reply[3]]
	if !known {
		return protocolIdentification{}, reply, false
	}
	if reply[3] == 0 {
		connection.Write([]byte{0xe0, 0x00}) // DISCONNECT
	}
	return protocolIdentification{Name: "mqtt", Detail: "CONNACK " + text, Basis: protocolBasisReply}, nil, true
}

// cassandraOptions는 CQL native protocol v4의 OPTIONS frame이다. 인증 전에 받는 요청이다.
var cassandraOptions = []byte{0x04, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00}

const (
	cassandraOpError     = 0x00
	cassandraOpSupported = 0x06
	cassandraReplyMax    = 64 << 10
)

// identifyCassandra는 OPTIONS를 보낸다. 서버는 SUPPORTED로 CQL 버전 목록을 주거나, v4를 받지 않으면 ERROR로 지원 버전을
// 알려 준다. 둘 다 응답 bit(0x80)가 켜진 frame이다. ScyllaDB도 같은 프로토콜이라 cassandra로 나온다.
func identifyCassandra(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	if _, err := connection.Write(cassandraOptions); err != nil {
		return protocolIdentification{}, nil, false
	}
	header := make([]byte, 9)
	count, err := io.ReadFull(connection, header)
	if err != nil {
		return protocolIdentification{}, header[:count], false
	}
	version, opcode, size := header[0], header[4], binary.BigEndian.Uint32(header[5:9])
	if version&0x80 == 0 || version&0x7f < 3 || version&0x7f > 6 || (opcode != cassandraOpSupported && opcode != cassandraOpError) || size > cassandraReplyMax {
		return protocolIdentification{}, header, false
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(connection, body); err != nil {
		return protocolIdentification{}, header, false
	}
	detail := "CQL"
	if opcode == cassandraOpSupported {
		if versions := cqlStringMultimap(body)["CQL_VERSION"]; len(versions) > 0 {
			detail = "CQL " + strings.Join(versions, ", ")
		}
	} else if len(body) >= 6 {
		length := int(binary.BigEndian.Uint16(body[4:6]))
		if 6+length <= len(body) {
			detail = "error: " + string(body[6:6+length])
		}
	}
	return protocolIdentification{Name: "cassandra", Detail: detail, Basis: protocolBasisReply}, nil, true
}

// cqlStringMultimap은 CQL [string multimap]을 읽는다. 형식이 맞지 않으면 그때까지 읽은 값만 돌려준다.
func cqlStringMultimap(data []byte) map[string][]string {
	values := map[string][]string{}
	readString := func() (string, bool) {
		if len(data) < 2 {
			return "", false
		}
		length := int(binary.BigEndian.Uint16(data[0:2]))
		if 2+length > len(data) {
			return "", false
		}
		text := string(data[2 : 2+length])
		data = data[2+length:]
		return text, true
	}
	if len(data) < 2 {
		return values
	}
	count := int(binary.BigEndian.Uint16(data[0:2]))
	data = data[2:]
	for range count {
		key, ok := readString()
		if !ok || len(data) < 2 {
			return values
		}
		items := int(binary.BigEndian.Uint16(data[0:2]))
		data = data[2:]
		for range items {
			item, ok := readString()
			if !ok {
				return values
			}
			values[key] = append(values[key], item)
		}
	}
	return values
}

// natsBannerVersion은 NATS 서버의 INFO 줄에서 version 값을 찾는다. INFO 줄은 배너 buffer보다 길 수 있어 JSON으로 풀지 않는다.
func natsBannerVersion(line string) string {
	_, rest, found := strings.Cut(line, `"version":"`)
	if !found {
		return ""
	}
	version, _, _ := strings.Cut(rest, `"`)
	return version
}
