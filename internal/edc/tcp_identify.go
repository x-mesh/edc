package edc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// identifyBannerWait는 서버가 먼저 말하는 프로토콜(SSH, SMTP 등)의 첫 바이트를 기다리는 시간이다.
	identifyBannerWait = 500 * time.Millisecond
	// identifyStageTimeout은 탐침 한 단계의 상한이다. 모르는 서버가 답하지 않아도 다음 탐침으로 넘어가게 한다.
	identifyStageTimeout = time.Second
	// identifyTLSBannerWait는 ALPN이 없는 TLS 연결에서 안쪽 인사말(imaps, pop3s, smtps)을 기다리는 시간이다.
	identifyTLSBannerWait = 300 * time.Millisecond
	// identifyReadLimit은 탐침 하나가 읽는 바이트의 상한이다.
	identifyReadLimit = 4096
	// identifyPreviewBytes는 판별하지 못한 응답에서 결과에 남기는 앞부분이다.
	identifyPreviewBytes = 16
	// identifyDetailMaxRunes는 요약 한 줄에 붙이는 판별 근거(배너, Server header 등)의 상한이다.
	identifyDetailMaxRunes = 120
)

const (
	protocolBasisBanner   = "banner"
	protocolBasisTLS      = "tls_handshake"
	protocolBasisHTTP     = "http_response"
	protocolBasisGoloop   = "goloop_secure_response"
	protocolBasisReply    = "probe_reply"
	protocolBasisTLSInner = "tls_inner_reply"
	protocolUnknown       = "unknown"
	protocolNameGoloopP2P = "goloop-p2p"
)

// protocolIdentification은 --identify가 찾은 프로토콜이다. Basis는 무엇을 보고 판정했는지다.
type protocolIdentification struct {
	Name   string
	Detail string
	Basis  string
	// FirstBytes는 판별하지 못했을 때 서버가 보낸 앞부분이다. 아무것도 받지 못하면 비어 있다.
	FirstBytes []byte
	// Retried는 탐침이 응답 없이 끊겨 다시 보낸 적이 있는지다.
	Retried bool
	// Metrics는 프로토콜마다 더 읽은 값이다. JSON 결과의 metrics에 그대로 더한다.
	Metrics map[string]interface{}
	// Facts는 Detail 대신 상세 패널에 보일 줄이다. 비어 있으면 Detail을 한 줄로 보인다.
	Facts []string
}

// identifyTarget은 탐침 한 단계가 쓰는 대상이다. open은 연결을 연다. 배너를 기다리며 아무것도 보내지 않은 연결이 남아 있으면
// 그것을 먼저 내준다. 새 연결을 아끼면 goloop처럼 출발지마다 연결 수를 제한하는 서버에 덜 걸린다.
type identifyTarget struct {
	address string
	host    string
	// wait는 이 단계가 응답을 기다리는 상한이다. driver가 단계마다 정한다(identifyStageWait).
	wait time.Duration
	// hinted는 port가 이 단계를 가리켰는지다.
	hinted bool
	open   func(ctx context.Context, wait time.Duration) (net.Conn, error)
}

// identifyStage는 클라이언트가 먼저 말하는 프로토콜의 탐침이다. ports에 있는 port면 일반 순서보다 먼저 보낸다. port는 순서만
// 정하고 판정은 언제나 응답으로 한다. run이 false를 돌려줄 때 reply는 서버가 보낸 바이트다. 서버가 먼저 말하는 서비스는
// 어떤 탐침에도 인사말을 보내므로 driver가 reply를 배너로 다시 맞춰 본다.
type identifyStage struct {
	name  string
	ports []int
	// slow는 응답이 서버 애플리케이션의 처리 시간에 달린 탐침이다(HTTP의 GET /). 힌트가 없어도 identifySlowWait까지 기다린다.
	slow bool
	run  func(ctx context.Context, target identifyTarget) (identified protocolIdentification, reply []byte, ok bool)
}

// identifyStages는 일반 순서다. 다른 서비스에 보내도 해가 없고 빨리 끝나는 탐침을 앞에 둔다. jw-server의 실제 서버로 잰 결과다.
//   - 짧은 탐침(Redis, Memcached, PostgreSQL)을 TLS보다 앞에 둔다. TLS 서버는 이 바이트를 받으면 바로 끊지만, Redis는
//     ClientHello를 받으면 줄바꿈을 기다리며 대기 상한까지 멈췄다.
//   - Redis PING을 HTTP 탐침보다 앞에 둔다. Redis는 Host: header를 받으면 공격으로 보고 경고를 log에 남긴다.
//   - Memcached를 MongoDB보다 앞에 둔다. MongoDB 탐침은 줄바꿈이 없는 binary라 Memcached가 줄 끝을 기다리며 멈춘다.
//   - goloop는 마지막이다. 서버가 먼저 말하지도, 다른 탐침에 답하지도 않아 앞 단계가 모두 끝나야 남는 후보다.
var identifyStages = []identifyStage{
	{name: "redis", ports: []int{6379, 6380, 26379}, run: identifyRedis},
	{name: "memcached", ports: []int{11211}, run: identifyMemcached},
	{name: "postgresql", ports: []int{5432, 6432}, run: identifyPostgreSQL},
	{name: "tls", ports: []int{443, 465, 636, 853, 993, 995, 6443, 8443, 9443}, run: identifyTLS},
	{name: "mongodb", ports: []int{27017, 27018, 27019}, run: identifyMongoDB},
	{name: "http", ports: []int{80, 3000, 5000, 8000, 8008, 8080, 8888, 9000, 9200}, slow: true, run: identifyHTTP},
	{name: protocolNameGoloopP2P, ports: []int{7100}, run: identifyGoloop},
}

// identifySlowWait는 힌트 없는 slow 탐침의 최소 대기다.
const identifySlowWait = 500 * time.Millisecond

// identifyOrder는 port에 맞는 탐침을 앞에 두고 나머지를 일반 순서로 둔다. hinted는 앞에 둔 탐침의 수다.
func identifyOrder(port int) (ordered []identifyStage, hinted int) {
	var rest []identifyStage
	for _, stage := range identifyStages {
		if slices.Contains(stage.ports, port) {
			ordered = append(ordered, stage)
		} else {
			rest = append(rest, stage)
		}
	}
	return append(ordered, rest...), len(ordered)
}

// identifyStageWait는 단계마다 응답을 기다리는 상한이다. 힌트가 가리킨 탐침은 그 프로토콜일 가능성이 높아 길게 기다린다. 나머지는
// 연결 시간에 맞춘 짧은 상한을 쓴다. 맞지 않는 서버는 길이나 줄 끝을 기다리며 멈추므로 오래 기다려도 시간만 쓴다.
func identifyStageWait(stage identifyStage, hinted bool, connect time.Duration) time.Duration {
	if hinted {
		return identifyStageTimeout
	}
	wait := identifyQuickWait(connect)
	if stage.slow && wait < identifySlowWait {
		wait = identifySlowWait
	}
	return wait
}

// identifyProtocol은 연결이 된 address에 탐침을 차례로 보내 프로토콜을 찾는다. 탐침마다 새 연결을 쓴다.
// 맞지 않는 바이트를 받은 서버는 대개 연결을 끊어서, 한 연결에 이어 보내면 뒤 탐침의 결과가 흐려지기 때문이다.
// connect는 tcp check가 잰 연결 시간이다. 짧은 탐침의 대기 상한을 여기서 정한다.
func identifyProtocol(ctx context.Context, address string, connect time.Duration) protocolIdentification {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return protocolIdentification{Name: protocolUnknown}
	}
	port, _ := strconv.Atoi(portText)
	stages, hinted := identifyOrder(port)
	return identifyWithStages(ctx, address, connect, stages, hinted)
}

// identifyWithStages는 정해진 순서로 탐침을 보낸다. 앞의 hinted개는 port에 맞는 탐침이라 배너를 기다리지 않고 바로 보낸다.
func identifyWithStages(ctx context.Context, address string, connect time.Duration, stages []identifyStage, hinted int) protocolIdentification {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return protocolIdentification{Name: protocolUnknown}
	}
	var idle net.Conn
	defer func() {
		if idle != nil {
			idle.Close()
		}
	}()
	target := identifyTarget{address: address, host: host}
	target.open = func(ctx context.Context, wait time.Duration) (net.Conn, error) {
		if idle != nil {
			connection := idle
			idle = nil
			return connection, connection.SetDeadline(identifyDeadline(ctx, wait))
		}
		return dialIdentify(ctx, address, wait)
	}
	// port가 클라이언트가 먼저 말하는 프로토콜을 가리키면 배너를 기다리지 않고 그 탐침부터 보낸다. 틀려도 서버가 먼저 말하는
	// 서비스의 인사말은 탐침의 응답으로 받으므로 놓치지 않는다.
	if hinted == 0 {
		match, first, connection := identifyBanner(ctx, address)
		if match.Name != "" {
			return match
		}
		if len(first) > 0 {
			// 서버가 먼저 말했는데 아는 배너가 아니면, 클라이언트가 먼저 말하는 탐침은 맞을 가능성이 낮다.
			return protocolIdentification{Name: protocolUnknown, FirstBytes: first}
		}
		idle = connection
	}
	var firstReply []byte
	for index, stage := range stages {
		if ctx.Err() != nil {
			break
		}
		target.hinted = index < hinted
		target.wait = identifyStageWait(stage, target.hinted, connect)
		match, reply, ok := stage.run(ctx, target)
		if ok {
			return match
		}
		if banner, ok := matchBanner(reply); ok {
			return banner
		}
		if firstReply == nil && len(reply) > 0 {
			firstReply = reply
		}
	}
	return protocolIdentification{Name: protocolUnknown, FirstBytes: firstReply}
}

// identifyQuickWait는 짧은 탐침의 대기 상한이다. 연결 시간의 4배에 서버 처리 여유 150ms를 더하고, 200ms와 1초 사이로 묶는다.
func identifyQuickWait(connect time.Duration) time.Duration {
	// 이 package에는 int용 max가 있어 내장 min, max를 쓰지 않는다.
	wait := 4*connect + 150*time.Millisecond
	if wait < 200*time.Millisecond {
		wait = 200 * time.Millisecond
	}
	if wait > identifyStageTimeout {
		wait = identifyStageTimeout
	}
	return wait
}

// identifyDeadline은 단계 상한과 전체 context 중 이른 쪽이다.
func identifyDeadline(ctx context.Context, wait time.Duration) time.Time {
	deadline := time.Now().Add(wait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return deadline
}

// dialIdentify는 탐침 한 단계의 연결을 연다.
func dialIdentify(ctx context.Context, address string, wait time.Duration) (net.Conn, error) {
	stageContext, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(stageContext, "tcp", address)
	if err != nil {
		return nil, err
	}
	if err := connection.SetDeadline(identifyDeadline(ctx, wait)); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

// identifyBanner는 아무것도 보내지 않고 서버가 먼저 보내는 바이트를 읽는다. 받은 바이트는 판별하지 못해도 돌려준다.
// 서버가 아무것도 보내지 않으면 연결을 닫지 않고 돌려준다. 다음 탐침이 그 연결을 쓴다.
func identifyBanner(ctx context.Context, address string) (protocolIdentification, []byte, net.Conn) {
	connection, err := dialIdentify(ctx, address, identifyBannerWait)
	if err != nil {
		return protocolIdentification{}, nil, nil
	}
	buffer := make([]byte, 512)
	count, readErr := connection.Read(buffer)
	first := buffer[:count]
	if match, ok := matchBanner(first); ok {
		connection.Close()
		return match, first, nil
	}
	var timeout net.Error
	if count > 0 || !errors.As(readErr, &timeout) || !timeout.Timeout() {
		// 받은 것이 있거나 서버가 닫았으면 이 연결은 다시 쓸 수 없다.
		connection.Close()
		return protocolIdentification{}, first, nil
	}
	return protocolIdentification{}, first, connection
}

// identifyExchange는 request를 보내고 첫 응답 조각을 읽는다. 짧은 응답의 프로토콜은 한 번 읽으면 판정할 수 있다.
func identifyExchange(ctx context.Context, target identifyTarget, wait time.Duration, request []byte) []byte {
	connection, err := target.open(ctx, wait)
	if err != nil {
		return nil
	}
	defer connection.Close()
	if _, err := connection.Write(request); err != nil {
		return nil
	}
	buffer := make([]byte, 512)
	count, _ := connection.Read(buffer)
	return buffer[:count]
}

// identifyRedis는 RESP 형식의 PING을 보낸다. 비밀번호나 protected mode가 걸린 Redis도 자기 오류로 답하므로 Redis로 판정한다.
// Valkey, KeyDB, Dragonfly처럼 Redis 프로토콜을 쓰는 서버도 redis로 나온다.
func identifyRedis(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	reply := identifyExchange(ctx, target, target.wait, []byte(redisPing))
	if line := bannerFirstLine(reply); isRedisReply(line) {
		return protocolIdentification{Name: "redis", Detail: line, Basis: protocolBasisReply}, reply, true
	}
	return protocolIdentification{}, reply, false
}

const redisPing = "*1\r\n$4\r\nPING\r\n"

// isRedisReply는 PING에 대한 Redis의 답인지 본다. 비밀번호, protected mode, 권한, 적재 중(LOADING), script 실행 중(BUSY)인
// Redis도 자기 오류로 답한다.
func isRedisReply(line string) bool {
	if line == "+PONG" {
		return true
	}
	for _, prefix := range []string{"-NOAUTH ", "-WRONGPASS ", "-DENIED Redis ", "-NOPERM ", "-LOADING Redis ", "-BUSY Redis "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// identifyMemcached는 text 프로토콜의 version 명령을 보낸다.
func identifyMemcached(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	reply := identifyExchange(ctx, target, target.wait, []byte("version\r\n"))
	line := bannerFirstLine(reply)
	if strings.HasPrefix(line, "VERSION ") && strings.HasSuffix(string(reply), "\r\n") {
		return protocolIdentification{Name: "memcached", Detail: line, Basis: protocolBasisReply}, reply, true
	}
	return protocolIdentification{}, reply, false
}

// postgresSSLRequest는 PostgreSQL의 SSLRequest다. 길이 8과 약속된 번호 80877103으로 이루어진다.
var postgresSSLRequest = []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

// identifyPostgreSQL은 SSLRequest를 보낸다. PostgreSQL(과 PgBouncer)은 TLS를 받으면 'S', 아니면 'N' 한 바이트만 보내고
// 다음 메시지를 기다린다. 한 바이트라 신호가 약하므로 정확히 한 바이트만 왔을 때만 판정한다.
func identifyPostgreSQL(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	reply := identifyExchange(ctx, target, target.wait, postgresSSLRequest)
	if len(reply) == 1 && (reply[0] == 'S' || reply[0] == 'N') {
		detail := "SSL supported"
		if reply[0] == 'N' {
			detail = "SSL not offered"
		}
		return protocolIdentification{Name: "postgresql", Detail: detail, Basis: protocolBasisReply}, reply, true
	}
	return protocolIdentification{}, reply, false
}

const (
	mongoOpMsg        = 2013
	mongoHeaderSize   = 16
	mongoReplyMax     = 64 << 10 // hello 응답은 1KB 안팎이다. 이보다 길다고 하면 MongoDB가 아니다.
	mongoHelloRequest = 0x65646321
)

// mongoHelloMessage는 OP_MSG {hello: 1, $db: "admin"}이다. 인증 없이 받는 명령이고, 길이가 30바이트를 넘어 goloop 같은
// 길이 기반 서버가 멈추지 않는다.
func mongoHelloMessage() []byte {
	var elements []byte
	elements = append(elements, 0x10)
	elements = append(elements, "hello\x00"...)
	elements = binary.LittleEndian.AppendUint32(elements, 1)
	elements = append(elements, 0x02)
	elements = append(elements, "$db\x00"...)
	elements = binary.LittleEndian.AppendUint32(elements, uint32(len("admin\x00")))
	elements = append(elements, "admin\x00"...)
	elements = append(elements, 0x00)
	message := make([]byte, mongoHeaderSize)
	message = binary.LittleEndian.AppendUint32(message, 0) // flagBits
	message = append(message, 0x00)                        // section kind 0: body
	message = binary.LittleEndian.AppendUint32(message, uint32(len(elements)+4))
	message = append(message, elements...)
	binary.LittleEndian.PutUint32(message[0:4], uint32(len(message)))
	binary.LittleEndian.PutUint32(message[4:8], mongoHelloRequest)
	binary.LittleEndian.PutUint32(message[12:16], mongoOpMsg)
	return message
}

// identifyMongoDB는 hello를 보내고, 응답의 opCode와 responseTo가 우리 요청에 대한 OP_MSG인지 본다.
func identifyMongoDB(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	defer connection.Close()
	if _, err := connection.Write(mongoHelloMessage()); err != nil {
		return protocolIdentification{}, nil, false
	}
	header := make([]byte, mongoHeaderSize)
	count, err := io.ReadFull(connection, header)
	if err != nil {
		return protocolIdentification{}, header[:count], false
	}
	length := binary.LittleEndian.Uint32(header[0:4])
	if binary.LittleEndian.Uint32(header[8:12]) != mongoHelloRequest || binary.LittleEndian.Uint32(header[12:16]) != mongoOpMsg ||
		length < mongoHeaderSize+5 || length > mongoReplyMax {
		return protocolIdentification{}, header, false
	}
	body := make([]byte, length-mongoHeaderSize)
	if _, err := io.ReadFull(connection, body); err != nil {
		return protocolIdentification{}, header, false
	}
	// body는 flagBits 4바이트, section kind 1바이트, BSON 문서 순이다.
	fields := bsonTopLevel(body[5:])
	var parts []string
	if version, ok := fields["maxWireVersion"].(int32); ok {
		parts = append(parts, fmt.Sprintf("maxWireVersion %d", version))
	}
	if name, ok := fields["setName"].(string); ok {
		parts = append(parts, "replica set "+name)
	}
	if message, ok := fields["msg"].(string); ok && message == "isdbgrid" {
		parts = append(parts, "mongos")
	}
	return protocolIdentification{Name: "mongodb", Detail: strings.Join(parts, ", "), Basis: protocolBasisReply}, nil, true
}

// bsonTopLevel은 BSON 문서의 최상위 string, int32, bool 값을 읽는다. 다른 형식은 건너뛰고, 모르는 형식을 만나면 거기서 멈춘다.
func bsonTopLevel(document []byte) map[string]interface{} {
	fields := map[string]interface{}{}
	if len(document) < 5 {
		return fields
	}
	size := int(binary.LittleEndian.Uint32(document[0:4]))
	if size > len(document) || size < 5 {
		return fields
	}
	data := document[4 : size-1]
	fixed := map[byte]int{0x01: 8, 0x07: 12, 0x08: 1, 0x09: 8, 0x0a: 0, 0x10: 4, 0x11: 8, 0x12: 8, 0x13: 16}
	for len(data) > 0 {
		kind := data[0]
		end := bytes.IndexByte(data[1:], 0)
		if end < 0 {
			return fields
		}
		name := string(data[1 : 1+end])
		data = data[2+end:]
		var width int
		switch kind {
		case 0x02:
			if len(data) < 4 {
				return fields
			}
			width = 4 + int(binary.LittleEndian.Uint32(data[0:4]))
			if width <= 4 || width > len(data) {
				return fields
			}
			fields[name] = string(data[4 : width-1])
		case 0x03, 0x04:
			if len(data) < 4 {
				return fields
			}
			width = int(binary.LittleEndian.Uint32(data[0:4]))
		case 0x05:
			if len(data) < 4 {
				return fields
			}
			width = 5 + int(binary.LittleEndian.Uint32(data[0:4]))
		default:
			known := false
			if width, known = fixed[kind]; !known {
				return fields
			}
			if kind == 0x10 && len(data) >= 4 {
				fields[name] = int32(binary.LittleEndian.Uint32(data[0:4]))
			}
			if kind == 0x08 && len(data) >= 1 {
				fields[name] = data[0] == 1
			}
		}
		if width < 0 || width > len(data) {
			return fields
		}
		data = data[width:]
	}
	return fields
}

// matchBanner는 서버가 먼저 보내는 프로토콜의 첫 바이트를 맞춰 본다.
func matchBanner(first []byte) (protocolIdentification, bool) {
	line := bannerFirstLine(first)
	banner := func(name string) (protocolIdentification, bool) {
		return protocolIdentification{Name: name, Detail: line, Basis: protocolBasisBanner}, true
	}
	switch {
	case strings.HasPrefix(line, "SSH-"):
		return banner("ssh")
	case strings.HasPrefix(line, "RFB "):
		return banner("vnc")
	case strings.HasPrefix(line, "+OK"):
		return banner("pop3")
	case strings.HasPrefix(line, "* OK"):
		return banner("imap")
	case strings.HasPrefix(line, "220"):
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "FTP") {
			return banner("ftp")
		}
		if strings.Contains(upper, "SMTP") {
			return banner("smtp")
		}
		return banner("smtp/ftp")
	}
	if version, ok := identifyMySQLGreeting(first); ok {
		return protocolIdentification{Name: "mysql", Detail: version, Basis: protocolBasisBanner}, true
	}
	return protocolIdentification{}, false
}

// identifyMySQLGreeting은 MySQL 첫 handshake packet을 읽는다. 3바이트 길이, sequence 0, protocol version 10 뒤에 NUL로 끝나는 서버 버전이 온다.
func identifyMySQLGreeting(first []byte) (string, bool) {
	if len(first) < 6 || first[3] != 0 || first[4] != 0x0a {
		return "", false
	}
	length := int(first[0]) | int(first[1])<<8 | int(first[2])<<16
	if length < 2 || length+4 > len(first) {
		return "", false
	}
	end := bytes.IndexByte(first[5:4+length], 0)
	if end <= 0 {
		return "", false
	}
	return string(first[5 : 5+end]), true
}

func bannerFirstLine(data []byte) string {
	if index := bytes.IndexAny(data, "\r\n"); index >= 0 {
		data = data[:index]
	}
	return strings.ToValidUTF8(strings.TrimSpace(string(data)), "?")
}

// identifyTLSConfig는 판별용 TLS 설정이다. 인증서는 검증하지 않는다: 프로토콜만 알면 되고, IP로 연결하면 이름이 맞지 않기
// 때문이다. 인증서 검증은 edc tls check가 한다.
func identifyTLSConfig(host string, protocols []string) *tls.Config {
	config := &tls.Config{InsecureSkipVerify: true, NextProtos: protocols}
	if net.ParseIP(host) == nil {
		config.ServerName = host
	}
	return config
}

// tlsBannerNames는 TLS 안에서 인사말을 보내는 서비스의 이름이다.
var tlsBannerNames = map[string]string{"imap": "imaps", "pop3": "pop3s", "smtp": "smtps", "ftp": "ftps", "smtp/ftp": "smtps/ftps"}

// identifyTLS는 TLS handshake를 시도하고, 성공하면 그 안쪽 프로토콜까지 찾는다.
//   - ALPN h2: HTTP/2 요청 하나로 gRPC와 HTTPS를 가린다.
//   - ALPN http/1.1: 같은 연결에 HTTP 요청을 보낸다.
//   - ALPN 없음: 안쪽 인사말(imaps, pop3s, smtps)을 짧게 기다린 뒤 HTTP를 보내고, 아니면 새 TLS 연결로 Redis PING을 보낸다.
func identifyTLS(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	client := tls.Client(connection, identifyTLSConfig(target.host, []string{"h2", "http/1.1"}))
	defer client.Close()
	if err := client.HandshakeContext(ctx); err != nil {
		// 서버가 TLS alert로 거절하면 handshake는 실패해도 TLS 서버다. crypto/tls는 받은 alert를 Op "remote error"로 돌려준다.
		var operation *net.OpError
		if errors.As(err, &operation) && operation.Op == "remote error" {
			return protocolIdentification{Name: "tls", Detail: "alert: " + operation.Err.Error(), Basis: protocolBasisTLS}, nil, true
		}
		// TLS가 아닌 서버의 첫 5바이트다. SSH 같은 인사말이면 driver가 배너로 맞춘다.
		var header tls.RecordHeaderError
		if errors.As(err, &header) {
			return protocolIdentification{}, header.RecordHeader[:], false
		}
		return protocolIdentification{}, nil, false
	}
	state := client.ConnectionState()
	parts := []string{tlsVersion(state.Version)}
	if state.NegotiatedProtocol != "" {
		parts = append(parts, "ALPN "+state.NegotiatedProtocol)
	}
	if len(state.PeerCertificates) > 0 && state.PeerCertificates[0].Subject.CommonName != "" {
		parts = append(parts, "CN "+state.PeerCertificates[0].Subject.CommonName)
	}
	tlsDetail := strings.Join(parts, ", ")
	inner, ok := identifyInsideTLS(ctx, target, client, state.NegotiatedProtocol)
	if !ok {
		return protocolIdentification{Name: "tls", Detail: tlsDetail, Basis: protocolBasisTLS}, nil, true
	}
	inner.Facts = []string{protocolFactLine("tls", tlsDetail), protocolFactLine("detail", inner.Detail)}
	inner.Metrics = map[string]interface{}{"tls_detail": tlsDetail}
	inner.Basis = protocolBasisTLSInner
	return inner, nil, true
}

func identifyInsideTLS(ctx context.Context, target identifyTarget, client *tls.Conn, alpn string) (protocolIdentification, bool) {
	if err := client.SetDeadline(identifyDeadline(ctx, identifyStageTimeout)); err != nil {
		return protocolIdentification{}, false
	}
	switch alpn {
	case "h2":
		return identifyHTTP2(ctx, target, client)
	case "http/1.1":
		if detail, _, ok, _ := httpExchange(client, target.host); ok {
			return protocolIdentification{Name: "https", Detail: detail}, true
		}
		return protocolIdentification{}, false
	}
	if err := client.SetReadDeadline(identifyDeadline(ctx, identifyTLSBannerWait)); err != nil {
		return protocolIdentification{}, false
	}
	buffer := make([]byte, 512)
	if count, _ := client.Read(buffer); count > 0 {
		if banner, ok := matchBanner(buffer[:count]); ok {
			if name, secure := tlsBannerNames[banner.Name]; secure {
				banner.Name = name
			}
			return banner, true
		}
		return protocolIdentification{}, false
	}
	// Redis PING을 HTTP 요청보다 먼저 보낸다. Redis는 Host: header를 받으면 보안 경고를 log에 남긴다(jw-server의 Redis에서
	// 확인했다). HTTPS 서버는 PING에 400으로 답하므로 그 응답으로 https를 판정한다.
	inner := identifyTarget{open: func(ctx context.Context, wait time.Duration) (net.Conn, error) {
		return client, client.SetDeadline(identifyDeadline(ctx, wait))
	}}
	reply := identifyExchange(ctx, inner, identifyStageTimeout, []byte(redisPing))
	if line := bannerFirstLine(reply); isRedisReply(line) {
		return protocolIdentification{Name: "rediss", Detail: line}, true
	} else if strings.HasPrefix(line, "HTTP/") {
		return protocolIdentification{Name: "https", Detail: line}, true
	}
	// 인사말이 대기 시간보다 늦게 오면 PING의 응답 자리에 온다. smtp.gmail.com:465가 그랬다.
	if banner, ok := matchBanner(reply); ok {
		if name, secure := tlsBannerNames[banner.Name]; secure {
			banner.Name = name
		}
		return banner, true
	}
	return identifyHTTPOverTLS(ctx, target)
}

// identifyHTTPOverTLS는 새 TLS 연결로 GET / 를 보낸다. 앞의 연결은 PING을 받아 HTTPS 서버라도 닫혔을 수 있다.
func identifyHTTPOverTLS(ctx context.Context, target identifyTarget) (protocolIdentification, bool) {
	connection, err := dialIdentify(ctx, target.address, identifyStageTimeout)
	if err != nil {
		return protocolIdentification{}, false
	}
	client := tls.Client(connection, identifyTLSConfig(target.host, nil))
	defer client.Close()
	if err := client.HandshakeContext(ctx); err != nil {
		return protocolIdentification{}, false
	}
	if detail, _, ok, _ := httpExchange(client, target.host); ok {
		return protocolIdentification{Name: "https", Detail: detail}, true
	}
	return protocolIdentification{}, false
}

// identifyHTTP2는 ALPN h2로 연결된 서버에 gRPC health check 요청 하나를 보낸다. gRPC 서버는 구현 여부와 관계없이
// content-type application/grpc나 grpc-status로 답하고, 일반 HTTPS 서버는 404나 405 같은 HTTP 응답을 준다.
// 이 요청은 대상의 access log에 POST로 남는다.
func identifyHTTP2(ctx context.Context, target identifyTarget, client *tls.Conn) (protocolIdentification, bool) {
	transport := &http.Transport{
		DialTLSContext:    func(context.Context, string, string) (net.Conn, error) { return client, nil },
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	// gRPC 메시지 하나: 압축 안 함(0)과 길이 0. health check의 빈 요청이다.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+target.address+"/grpc.health.v1.Health/Check", bytes.NewReader(make([]byte, 5)))
	if err != nil {
		return protocolIdentification{}, false
	}
	request.Header.Set("Content-Type", "application/grpc")
	request.Header.Set("TE", "trailers")
	request.Header.Set("User-Agent", "edc")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return protocolIdentification{}, false
	}
	defer response.Body.Close()
	parts := []string{fmt.Sprintf("HTTP/2 %d", response.StatusCode)}
	if server := response.Header.Get("Server"); server != "" {
		parts = append(parts, "server "+server)
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") || response.Header.Get("Grpc-Status") != "" {
		io.Copy(io.Discard, io.LimitReader(response.Body, identifyReadLimit))
		status := response.Header.Get("Grpc-Status")
		if status == "" {
			status = response.Trailer.Get("Grpc-Status")
		}
		if status != "" {
			parts = append(parts, "grpc-status "+status)
		}
		return protocolIdentification{Name: "grpc", Detail: strings.Join(parts, ", ")}, true
	}
	return protocolIdentification{Name: "https", Detail: strings.Join(parts, ", ")}, true
}

// identifyHTTP는 평문 HTTP 요청을 보낸다. 요청을 30바이트보다 길게 두어, header 길이만큼 기다리는 binary 프로토콜
// (goloop는 30바이트)이 이 단계에서 timeout까지 멈추지 않고 바로 연결을 끊게 한다.
func identifyHTTP(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return protocolIdentification{}, nil, false
	}
	// port가 HTTP를 가리키면 Host: header를 넣는다. naver.com:80은 Host가 없으면 3초 동안 답하지 않았다.
	// 그 밖에는 먼저 Host 없이 보낸다. Redis 단계가 놓친 Redis에 닿아도 보안 경고가 남지 않고(Redis는 Host: header를 받으면
	// 경고를 남긴다), Redis는 GET 명령의 인자 오류로 답하므로 그 응답으로 Redis를 찾는다. 아무 답이 없을 때만 Host를 넣어 다시 보낸다.
	host := ""
	if target.hinted {
		host = target.host
	}
	detail, reply, ok, waited := httpExchange(connection, host)
	connection.Close()
	// 바로 닫은 서버(goloop 등)에는 다시 보내지 않는다. 연결 수 제한이 있는 서버에서 연결을 아낀다.
	if waited && host == "" {
		retry, err := dialIdentify(ctx, target.address, target.wait)
		if err != nil {
			return protocolIdentification{}, nil, false
		}
		defer retry.Close()
		detail, reply, ok, _ = httpExchange(retry, target.host)
	}
	if !ok {
		if line := bannerFirstLine(reply); strings.HasPrefix(line, "-ERR wrong number of arguments for 'get' command") {
			return protocolIdentification{Name: "redis", Detail: line, Basis: protocolBasisReply}, nil, true
		}
		return protocolIdentification{}, reply, false
	}
	return protocolIdentification{Name: "http", Detail: detail, Basis: protocolBasisHTTP}, nil, true
}

// httpExchange는 GET / 를 보내고 상태 줄과 Server header를 읽는다. HTTP가 아니면 받은 첫 줄을 돌려준다. host가 비어 있으면
// Host: header를 넣지 않는다.
// waited는 서버가 닫지 않고 아무것도 보내지 않은 채 대기 상한이 지났는지다.
func httpExchange(connection net.Conn, host string) (detail string, reply []byte, ok bool, waited bool) {
	request := "GET / HTTP/1.0\r\nUser-Agent: edc\r\nConnection: close\r\n\r\n"
	if host != "" {
		request = fmt.Sprintf("GET / HTTP/1.0\r\nHost: %s\r\nUser-Agent: edc\r\nConnection: close\r\n\r\n", host)
	}
	if _, err := io.WriteString(connection, request); err != nil {
		return "", nil, false, false
	}
	reader := bufio.NewReader(io.LimitReader(connection, identifyReadLimit))
	status, err := reader.ReadString('\n')
	if !strings.HasPrefix(status, "HTTP/1.") && !strings.HasPrefix(status, "HTTP/2") {
		var timeout net.Error
		return "", []byte(status), false, status == "" && errors.As(err, &timeout) && timeout.Timeout()
	}
	parts := []string{strings.TrimSpace(status)}
	for err == nil {
		var line string
		line, err = reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "server") {
			parts = append(parts, "server "+strings.TrimSpace(value))
			break
		}
	}
	return strings.Join(parts, ", "), nil, true, false
}

// goloop P2P packet은 header 30바이트(protocol 2, sub protocol 2, 보낸 peer ID 20, dest 1, ttl 1, payload 길이 4),
// payload, footer 10바이트(header와 payload의 FNV-64a 8, 확장 정보 2)로 이루어진다. network/packet.go와 같은 형식이다.
const (
	goloopHeaderSize        = 30
	goloopFooterSize        = 10
	goloopPeerIDSize        = 20
	goloopSecureRequest     = 0x0100
	goloopSecureResponse    = 0x0200
	goloopDestPeer          = 0xff
	goloopResponsePayload   = 1024 // SecureResponse는 100바이트 안팎이다. 이보다 길다고 하면 goloop가 아니다.
	goloopPublicKeyMaxBytes = 0xff
)

// goloop의 SecureSuite와 SecureAeadSuite 값이다(network/secure.go). 0은 둘 다 "고르지 못함"이다.
const (
	goloopSuiteNone  byte = 1
	goloopSuiteTLS   byte = 2
	goloopSuiteECDHE byte = 3
	goloopAeadChaCha byte = 1
	goloopAeadAES128 byte = 2
	goloopAeadAES256 byte = 3
)

var (
	goloopSuiteNames = map[byte]string{goloopSuiteNone: "none", goloopSuiteTLS: "tls", goloopSuiteECDHE: "ecdhe"}
	goloopAeadNames  = map[byte]string{goloopAeadChaCha: "chacha", goloopAeadAES128: "aes128", goloopAeadAES256: "aes256"}
)

// goloopSecureProbes는 연결마다 묻는 보안 방식과 암호 방식의 짝이다. 노드는 요청 목록에서 지원하는 것을 골라 답하므로 하나씩 물어야
// 지원 여부를 안다. 보안 방식이 none이어도 노드는 암호 방식을 골라 답하고(legacy 분기), tls나 ecdhe를 지원하지만 암호 방식이
// 맞지 않으면 보안 방식은 채운 채 "invalid"로 답한다. 그래서 연결 세 번으로 두 목록을 모두 확인한다.
var goloopSecureProbes = [][2]byte{
	{goloopSuiteNone, goloopAeadChaCha},
	{goloopSuiteTLS, goloopAeadAES128},
	{goloopSuiteECDHE, goloopAeadAES256},
}

// goloopSecureReply는 SecureResponse{Channel, SecureSuite, SecureAeadSuite, SecureParam, SecureError}에서 쓰는 값이다.
type goloopSecureReply struct {
	peerID      []byte
	suite       byte
	aead        byte
	secureError string
}

// identifyGoloopRetryDelay는 goloop 노드가 응답 없이 끊은 뒤 다시 시도할 때까지 기다리는 시간이다. 노드는 인증 전 연결을
// 출발지 IP마다 초당 몇 개로 제한하고(preAuthConnRatePerIP), 넘친 연결은 accept 직후 닫는다. 제한은 초가 바뀌면 풀린다.
// 실제 노드에서는 간격을 두고 보내도 20번에 1번꼴로 끊겼고, 끊김이 1초 안에 두 번 이어지기도 했다.
const (
	identifyGoloopRetryDelay = time.Second
	identifyGoloopAttempts   = 3
)

// identifyGoloop은 goloop 노드의 P2P 인증 첫 단계(SecureRequest)를 보낸다. 노드는 어떤 요청에도 SecureResponse로 답하고
// 그 header에 자기 peer ID(지갑 주소)를 담는다. 응답을 받은 뒤 다음 단계(서명)를 보내지 않고 끊으므로 노드에 peer로 붙지 않는다.
// 첫 응답으로 goloop임을 확인한 뒤에만 나머지 짝을 묻는다.
func identifyGoloop(ctx context.Context, target identifyTarget) (protocolIdentification, []byte, bool) {
	var peerID []byte
	suites := map[byte]bool{}
	aeads := map[byte]bool{}
	retried := false
	for index, probe := range goloopSecureProbes {
		// goloop인지 모르는 첫 교환은 port가 goloop를 가리킬 때만 다시 시도한다. 연결 직후 닫는 다른 서버(backend 없는 load
		// balancer 등)에서 재시도로 2초를 쓰지 않게 한다.
		attempts := identifyGoloopAttempts
		if index == 0 && !target.hinted {
			attempts = 1
		}
		reply, again, err := exchangeGoloopSecureWithRetry(ctx, target, probe[0], probe[1], attempts)
		retried = retried || again
		if err != nil {
			if index == 0 {
				return protocolIdentification{}, nil, false
			}
			continue
		}
		if peerID == nil {
			peerID = reply.peerID
		}
		suiteSupported, aeadSupported, aeadKnown := goloopProbeOutcome(probe[0], probe[1], reply)
		suites[probe[0]] = suiteSupported
		if aeadKnown {
			aeads[probe[1]] = aeadSupported
		}
	}
	// 짝지은 보안 방식을 지원하지 않아 확인하지 못한 암호 방식은, 지원이 확인된 보안 방식과 다시 짝지어 묻는다.
	// 방식을 제한한 노드에서만 연결이 늘어난다.
	if suite, ok := goloopSecureSuiteFor(suites); ok {
		for _, probe := range goloopSecureProbes {
			if _, seen := aeads[probe[1]]; seen {
				continue
			}
			reply, again, err := exchangeGoloopSecureWithRetry(ctx, target, suite, probe[1], identifyGoloopAttempts)
			retried = retried || again
			if err != nil {
				continue
			}
			if _, aeadSupported, aeadKnown := goloopProbeOutcome(suite, probe[1], reply); aeadKnown {
				aeads[probe[1]] = aeadSupported
			}
		}
	}
	node := "hx" + hex.EncodeToString(peerID)
	metrics := map[string]interface{}{"goloop_node": node}
	supportedSuites, uncheckedSuites := goloopSupported(goloopSuiteNames, suites)
	supportedAeads, uncheckedAeads := goloopSupported(goloopAeadNames, aeads)
	metrics["goloop_secure_suites"] = supportedSuites
	metrics["goloop_aead_suites"] = supportedAeads
	facts := []string{
		protocolFactLine("node", node),
		protocolFactLine("secure suites", goloopListText(supportedSuites)),
		protocolFactLine("aead suites", goloopListText(supportedAeads)),
	}
	if unchecked := append(uncheckedSuites, uncheckedAeads...); len(unchecked) > 0 {
		metrics["goloop_unchecked"] = unchecked
		facts = append(facts, protocolFactLine("unchecked", goloopListText(unchecked)))
	}
	return protocolIdentification{Name: protocolNameGoloopP2P, Detail: "node " + node, Basis: protocolBasisGoloop, Retried: retried, Metrics: metrics, Facts: facts}, nil, true
}

// goloopProbeOutcome은 한 짝을 물은 응답에서 보안 방식과 암호 방식의 지원 여부를 읽는다. 보안 방식을 지원하지 않으면 노드는
// 암호 방식을 고르지 않으므로 암호 방식은 알 수 없다.
func goloopProbeOutcome(suite, aead byte, reply goloopSecureReply) (suiteSupported, aeadSupported, aeadKnown bool) {
	if reply.suite != suite {
		return false, false, false
	}
	if suite == goloopSuiteNone {
		return true, reply.aead == aead, true
	}
	return true, reply.secureError == "" && reply.aead == aead, true
}

// goloopSecureSuiteFor는 암호 방식을 물을 때 짝지을 보안 방식이다. none도 암호 방식을 고르지만, none을 지원하지 않는 노드가 있어
// 지원이 확인된 쪽을 쓴다.
func goloopSecureSuiteFor(suites map[byte]bool) (byte, bool) {
	for _, suite := range []byte{goloopSuiteNone, goloopSuiteTLS, goloopSuiteECDHE} {
		if suites[suite] {
			return suite, true
		}
	}
	return 0, false
}

// goloopSupported는 지원하는 이름과 확인하지 못한 이름을 값 순서로 돌려준다.
func goloopSupported(names map[byte]string, checked map[byte]bool) (supported, unchecked []string) {
	supported = []string{}
	for value := byte(1); value <= byte(len(names)); value++ {
		ok, seen := checked[value]
		switch {
		case !seen:
			unchecked = append(unchecked, names[value])
		case ok:
			supported = append(supported, names[value])
		}
	}
	return supported, unchecked
}

func goloopListText(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ", ")
}

// exchangeGoloopSecureWithRetry는 응답 없이 끊긴 연결만 다시 시도한다. 연결 수 제한에 걸린 goloop 노드가 이렇게 끊는다.
func exchangeGoloopSecureWithRetry(ctx context.Context, target identifyTarget, suite, aead byte, attempts int) (goloopSecureReply, bool, error) {
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return goloopSecureReply{}, true, ctx.Err()
			case <-time.After(identifyGoloopRetryDelay):
			}
		}
		var reply goloopSecureReply
		reply, err = exchangeGoloopSecure(ctx, target, suite, aead)
		if err == nil {
			return reply, attempt > 0, nil
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
			return goloopSecureReply{}, attempt > 0, err
		}
	}
	return goloopSecureReply{}, true, err
}

func exchangeGoloopSecure(ctx context.Context, target identifyTarget, suite, aead byte) (goloopSecureReply, error) {
	request, err := goloopSecureRequestPacket(suite, aead)
	if err != nil {
		return goloopSecureReply{}, err
	}
	connection, err := target.open(ctx, target.wait)
	if err != nil {
		return goloopSecureReply{}, err
	}
	defer connection.Close()
	if _, err := connection.Write(request); err != nil {
		return goloopSecureReply{}, err
	}
	return readGoloopSecureResponse(connection)
}

// goloopSecureRequestPacket은 SecureRequest{Channel "", SecureSuites [suite], SecureAeadSuites [aead], SecureParam P-256 공개 키}를
// goloop의 msgpack 형식(struct는 field 순서의 배열, []byte 계열은 bin)으로 만든다. 공개 키가 형식에 맞지 않으면 노드가 응답 뒤
// 오류를 log에 남기므로, 쓰지 않을 키라도 올바른 키를 만든다.
func goloopSecureRequestPacket(suite, aead byte) ([]byte, error) {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	publicKey := key.PublicKey().Bytes()
	if len(publicKey) > goloopPublicKeyMaxBytes {
		return nil, fmt.Errorf("public key too long: %d", len(publicKey))
	}
	payload := []byte{0x94, 0xa0, 0xc4, 0x01, suite, 0xc4, 0x01, aead, 0xc4, byte(len(publicKey))}
	payload = append(payload, publicKey...)
	header := make([]byte, goloopHeaderSize)
	binary.BigEndian.PutUint16(header[2:4], goloopSecureRequest)
	header[24] = goloopDestPeer
	header[25] = 1
	binary.BigEndian.PutUint32(header[26:30], uint32(len(payload)))
	footer := make([]byte, goloopFooterSize)
	binary.BigEndian.PutUint64(footer[:8], goloopPacketHash(header, payload))
	return append(append(header, payload...), footer...), nil
}

func goloopPacketHash(header, payload []byte) uint64 {
	hash := fnv.New64a()
	hash.Write(header)
	hash.Write(payload)
	return hash.Sum64()
}

// readGoloopSecureResponse는 protocol 0x0000, sub protocol 0x0200이고 footer의 hash가 맞는 packet인지 본다.
// 방식을 지원하지 않는 노드는 SecureError를 담아 답하지만 그래도 goloop다. 한 바이트도 받지 못하고 끊기면 io.EOF를 돌려준다.
func readGoloopSecureResponse(reader io.Reader) (goloopSecureReply, error) {
	header := make([]byte, goloopHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return goloopSecureReply{}, err
	}
	if binary.BigEndian.Uint16(header[0:2]) != 0 || binary.BigEndian.Uint16(header[2:4]) != goloopSecureResponse {
		return goloopSecureReply{}, errNotGoloop
	}
	length := binary.BigEndian.Uint32(header[26:30])
	if length > goloopResponsePayload {
		return goloopSecureReply{}, errNotGoloop
	}
	rest := make([]byte, int(length)+goloopFooterSize)
	if _, err := io.ReadFull(reader, rest); err != nil {
		return goloopSecureReply{}, errNotGoloop
	}
	if binary.BigEndian.Uint64(rest[length:length+8]) != goloopPacketHash(header, rest[:length]) {
		return goloopSecureReply{}, errNotGoloop
	}
	reply, err := parseGoloopSecureResponse(rest[:length])
	if err != nil {
		return goloopSecureReply{}, errNotGoloop
	}
	reply.peerID = header[4 : 4+goloopPeerIDSize]
	return reply, nil
}

var errNotGoloop = errors.New("not a goloop secure response")

// parseGoloopSecureResponse는 SecureResponse의 msgpack 배열 [Channel, SecureSuite, SecureAeadSuite, SecureParam, SecureError]를 읽는다.
func parseGoloopSecureResponse(payload []byte) (goloopSecureReply, error) {
	cursor := &msgpackCursor{data: payload}
	if count := cursor.arrayLength(); count < 5 {
		return goloopSecureReply{}, fmt.Errorf("secure response has %d fields", count)
	}
	cursor.str()
	reply := goloopSecureReply{suite: byte(cursor.uint()), aead: byte(cursor.uint())}
	cursor.skipBytes()
	reply.secureError = cursor.str()
	return reply, cursor.err
}

// msgpackCursor는 goloop SecureResponse에 나오는 msgpack 형식(배열, 문자열, 부호 없는 정수, bin, nil)만 읽는다.
// 처음 잘못된 값에서 err를 남기고, 그 뒤의 읽기는 0 값을 돌려준다.
type msgpackCursor struct {
	data []byte
	err  error
}

func (cursor *msgpackCursor) take(count int) []byte {
	if cursor.err != nil {
		return nil
	}
	if count < 0 || count > len(cursor.data) {
		cursor.err = io.ErrUnexpectedEOF
		return nil
	}
	taken := cursor.data[:count]
	cursor.data = cursor.data[count:]
	return taken
}

func (cursor *msgpackCursor) lengthAfter(size int) int {
	switch value := cursor.take(size); size {
	case 1:
		if value != nil {
			return int(value[0])
		}
	case 2:
		if value != nil {
			return int(binary.BigEndian.Uint16(value))
		}
	}
	return 0
}

func (cursor *msgpackCursor) kind() byte {
	if value := cursor.take(1); value != nil {
		return value[0]
	}
	return 0
}

func (cursor *msgpackCursor) fail(kind byte) {
	if cursor.err == nil {
		cursor.err = fmt.Errorf("unexpected msgpack type %#x", kind)
	}
}

func (cursor *msgpackCursor) arrayLength() int {
	kind := cursor.kind()
	switch {
	case kind >= 0x90 && kind <= 0x9f:
		return int(kind & 0x0f)
	case kind == 0xdc:
		return cursor.lengthAfter(2)
	}
	cursor.fail(kind)
	return 0
}

func (cursor *msgpackCursor) str() string {
	kind := cursor.kind()
	switch {
	case kind >= 0xa0 && kind <= 0xbf:
		return string(cursor.take(int(kind & 0x1f)))
	case kind == 0xd9:
		return string(cursor.take(cursor.lengthAfter(1)))
	case kind == 0xda:
		return string(cursor.take(cursor.lengthAfter(2)))
	case kind == 0xc0:
		return ""
	}
	cursor.fail(kind)
	return ""
}

func (cursor *msgpackCursor) uint() uint64 {
	kind := cursor.kind()
	switch {
	case kind <= 0x7f:
		return uint64(kind)
	case kind == 0xcc:
		return uint64(cursor.lengthAfter(1))
	case kind == 0xcd:
		return uint64(cursor.lengthAfter(2))
	}
	cursor.fail(kind)
	return 0
}

func (cursor *msgpackCursor) skipBytes() {
	kind := cursor.kind()
	switch kind {
	case 0xc4:
		cursor.take(cursor.lengthAfter(1))
	case 0xc5:
		cursor.take(cursor.lengthAfter(2))
	case 0xc0:
	default:
		cursor.fail(kind)
	}
}

// annotateProtocol은 tcp check 결과에 판별 결과를 붙인다. 판별은 연결 상태를 바꾸지 않으므로 Status는 그대로 둔다.
// 기본 결과 줄은 연결 요약 뒤에 프로토콜 이름만 붙인다. 상세 패널은 -v에서만 보이므로, verbose면 peer를 결과 줄에서 빼고
// 패널에 둔다.
func annotateProtocol(result Result, identified protocolIdentification, verbose bool) Result {
	if result.Metrics == nil {
		result.Metrics = map[string]interface{}{}
	}
	result.Metrics["protocol"] = identified.Name
	var facts []string
	if peer := protocolPeer(result); peer != "" {
		facts = append(facts, protocolFactLine("peer", peer))
	}
	if identified.Name == protocolUnknown {
		if len(identified.FirstBytes) > 0 {
			preview := identified.FirstBytes
			if len(preview) > identifyPreviewBytes {
				preview = preview[:identifyPreviewBytes]
			}
			result.Metrics["protocol_first_bytes"] = hex.EncodeToString(identified.FirstBytes)
			facts = append(facts, protocolFactLine("first bytes", spacedHex(preview)))
		}
	} else {
		detail := protocolDetailText(identified.Detail)
		result.Metrics["protocol_detail"] = detail
		result.Metrics["protocol_basis"] = identified.Basis
		if identified.Retried {
			result.Metrics["protocol_retried"] = true
		}
		for key, value := range identified.Metrics {
			result.Metrics[key] = value
		}
		if len(identified.Facts) > 0 {
			facts = append(facts, identified.Facts...)
		} else {
			facts = append(facts, protocolFactLine("detail", detail))
		}
		facts = append(facts, protocolFactLine("basis", protocolBasisText[identified.Basis]))
	}
	// 프로토콜 이름은 요약에 넣지 않는다. 결과 줄이 metrics의 protocol을 label 열의 남는 자리에 그린다(resultLabelTag).
	if verbose {
		result.Summary = protocolConnectedText(result)
	}
	result.Evidence = append(result.Evidence, Evidence{Label: T("observe.probe.protocol_facts"), Value: strings.Join(facts, "\n")})
	return result
}

var protocolBasisText = map[string]string{
	protocolBasisBanner:   "banner",
	protocolBasisTLS:      "TLS handshake",
	protocolBasisHTTP:     "HTTP response",
	protocolBasisGoloop:   "goloop SecureResponse",
	protocolBasisReply:    "probe reply",
	protocolBasisTLSInner: "TLS handshake and inner reply",
}

// protocolFactLine은 상세 패널의 한 줄이다. 이름 열을 맞춰 값이 한 열로 보이게 한다.
// 값은 대상 서버가 보낸 글자일 수 있어 protocolDetailText로 다듬는다.
func protocolFactLine(name, value string) string {
	return fmt.Sprintf("%-14s %s", name, protocolDetailText(value))
}

// protocolConnectedText는 probeTCP 요약의 앞부분(주소, 연결 시간)이다. peer는 상세 패널로 옮기므로 빼고 다시 만든다.
func protocolConnectedText(result Result) string {
	address, _ := result.Metrics["address"].(string)
	connectMS, _ := result.Metrics["connect_ms"].(int64)
	return T("observe.probe.tcp_connected", address, time.Duration(connectMS)*time.Millisecond)
}

func protocolPeer(result Result) string {
	remote, _ := result.Metrics["remote_address"].(string)
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return ""
	}
	return host
}

// protocolDetailText는 서버가 보낸 배너, header, 인증서 이름을 결과에 넣을 수 있게 다듬는다. 이 값은 대상 서버가 정하므로
// escape가 터미널을 조작하거나 긴 줄이 요약을 밀어내지 못하게 한다.
func protocolDetailText(raw string) string {
	text := viewSanitize(raw)
	if runes := []rune(text); len(runes) > identifyDetailMaxRunes {
		text = string(runes[:identifyDetailMaxRunes]) + "…"
	}
	return text
}

func spacedHex(data []byte) string {
	parts := make([]string, len(data))
	for index, value := range data {
		parts[index] = fmt.Sprintf("%02x", value)
	}
	return strings.Join(parts, " ")
}
