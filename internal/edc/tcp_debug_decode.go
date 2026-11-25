package edc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// debugRequestLabels는 접힌 단계 한 줄에 쓰는 요청 이름이다.
var debugRequestLabels = map[string]string{
	"redis": "PING", "memcached": "version", "postgresql": "SSLRequest", "tls": "ClientHello", "mongodb": "hello",
	"http": "GET /", protocolNameGoloopP2P: "SecureRequest", "h2c": "HTTP/2 preface + gRPC health check",
	"amqp": "AMQP 0-9-1 header", "kafka": "ApiVersions", "mqtt": "CONNECT", "cassandra": "OPTIONS",
}

type debugTextLine struct {
	depth int
	text  string
}

// hexRegion은 raw hex를 나눠 보일 구간과 그 이름이다.
type hexRegion struct {
	label      string
	start, end int
}

type debugDecoded struct {
	sent, received               []string
	sentRegions, receivedRegions []hexRegion
}

type debugDecoder func(exchange *debugExchange) debugDecoded

var debugDecoders = map[string]debugDecoder{
	"banner": decodeBannerExchange, "redis": decodeRedisExchange, "memcached": decodeMemcachedExchange,
	"postgresql": decodePostgresExchange, "tls": decodeTLSExchange, "mongodb": decodeMongoExchange, "http": decodeHTTPExchange,
	protocolNameGoloopP2P: decodeGoloopExchange, "h2c": decodeHTTP2FramesExchange, "amqp": decodeAMQPExchange,
	"kafka": decodeKafkaExchange, "mqtt": decodeMQTTExchange, "cassandra": decodeCassandraExchange,
}

// debug 줄의 방향 글머리다. 색을 입히는 쪽(formatDebugLines)이 이 글머리로 보낸 줄과 받은 줄을 가른다.
const (
	debugSendMark    = "→ "
	debugReceiveMark = "← "
)

// exchangeLines는 연결 하나를 보낸 것, 받은 것 순으로 풀어 쓴다. raw면 원본 바이트를 구간별 hex로 덧붙인다.
// showConnection은 연결 줄을 쓸지다. 단계에 연결이 하나뿐이고 평범하게 새로 열었다면, 시간은 단계 줄에 이미 있어 줄을 아낀다.
func exchangeLines(stage string, exchange *debugExchange, raw, showConnection bool) []debugTextLine {
	if exchange.dialErr != "" {
		return []debugTextLine{{1, "connect failed: " + exchange.dialErr}}
	}
	var lines []debugTextLine
	switch {
	case exchange.label != "":
		lines = append(lines, debugTextLine{1, exchange.label})
	case exchange.reused:
		lines = append(lines, debugTextLine{1, "reused the banner connection"})
	case showConnection && exchange.dial > 0:
		lines = append(lines, debugTextLine{1, "new connection · " + debugDuration(exchange.dial)})
	case showConnection:
		lines = append(lines, debugTextLine{1, "new connection"})
	}
	decoder := debugDecoders[stage]
	if exchange.label == "inside TLS" || decoder == nil {
		decoder = decodeAutoExchange
	}
	decoded := decoder(exchange)
	if exchange.sentAny {
		lines = append(lines, firstAndRest(decoded.sent, debugSendMark+fmt.Sprintf("%%s · %d B", exchange.sentTotal))...)
		if raw {
			lines = append(lines, hexLines(exchange.sent, exchange.sentTotal, decoded.sentRegions)...)
		}
	}
	switch {
	case exchange.receivedAny:
		header := debugReceiveMark + fmt.Sprintf("%%s · %d B in %s", exchange.receivedTotal, debugDuration(exchange.firstReceive-exchange.firstSendOrOpen()))
		lines = append(lines, firstAndRest(decoded.received, header)...)
		if raw {
			lines = append(lines, hexLines(debugRawReceived(exchange.received), exchange.receivedTotal, decoded.receivedRegions)...)
		}
	case exchange.end != "":
		lines = append(lines, debugTextLine{1, debugReceiveMark + "nothing · " + endText(exchange)})
	}
	return lines
}

func (exchange *debugExchange) firstSendOrOpen() time.Duration {
	if exchange.sentAny {
		return exchange.firstSend
	}
	return exchange.opened
}

func endText(exchange *debugExchange) string {
	after := debugDuration(exchange.endAt - exchange.firstSendOrOpen())
	switch exchange.end {
	case "waited":
		return "waited " + after
	case "closed":
		return "closed by the server after " + after
	case "reset":
		return "reset by the server after " + after
	}
	return exchange.end
}

// firstAndRest는 첫 해석 줄을 header에 넣고, 나머지는 한 단계 더 들여 쓴다.
func firstAndRest(decoded []string, header string) []debugTextLine {
	if len(decoded) == 0 {
		decoded = []string{"(not decoded)"}
	}
	lines := []debugTextLine{{1, fmt.Sprintf(header, decoded[0])}}
	for _, line := range decoded[1:] {
		lines = append(lines, debugTextLine{2, line})
	}
	return lines
}

// debugRawReceived는 raw hex로 보일 받은 바이트의 사본이다. 해석 줄에서 숨기는 값(HTTP 쿠키와 인증 header의 값, MySQL 인증
// salt)은 raw에서도 '*'로 바꾼다.
func debugRawReceived(data []byte) []byte {
	masked := bytes.Clone(data)
	maskHTTPSecretHeaders(masked)
	maskMySQLSalt(masked)
	return masked
}

// maskHTTPSecretHeaders는 HTTP 응답 header 가운데 httpReplyLines가 숨기는 header의 값을 가린다. 잘린 header는 끝까지 가린다.
// 103 Early Hints 같은 1xx 응답 뒤에는 다음 응답이 이어지므로, 다음 묶음도 HTTP/로 시작하면 가린다.
func maskHTTPSecretHeaders(data []byte) {
	for bytes.HasPrefix(data, []byte("HTTP/")) {
		end := bytes.Index(data, []byte("\r\n\r\n"))
		if end < 0 {
			maskHTTPHeaderBlock(data, len(data))
			return
		}
		maskHTTPHeaderBlock(data, end)
		data = data[end+4:]
	}
}

func maskHTTPHeaderBlock(data []byte, end int) {
	start := bytes.IndexByte(data[:end], '\n') + 1
	for start > 0 && start < end {
		lineEnd := bytes.IndexByte(data[start:end], '\n')
		if lineEnd < 0 {
			lineEnd = end
		} else {
			lineEnd += start
		}
		line := data[start:lineEnd]
		if colon := bytes.IndexByte(line, ':'); colon >= 0 {
			switch strings.ToLower(string(line[:colon])) {
			case "set-cookie", "authorization", "www-authenticate":
				value := line[colon+1:]
				value = bytes.TrimSuffix(value, []byte("\r"))
				for index := range value {
					value[index] = '*'
				}
			}
		}
		start = lineEnd + 1
	}
}

// maskMySQLSalt는 MySQL handshake v10의 salt 두 조각을 가린다. 위치는 mysqlGreetingLines와 같다.
func maskMySQLSalt(data []byte) {
	version, ok := identifyMySQLGreeting(data)
	if !ok {
		return
	}
	mask := func(from, to int) {
		for index := from; index < min(to, len(data)); index++ {
			data[index] = '*'
		}
	}
	position := 5 + len(version) + 1 + 4
	mask(position, position+8)
	position += 8 + 1 + 2 + 1 + 2 + 2
	if position >= len(data) {
		return
	}
	saltLength := int(data[position])
	position += 1 + 10
	mask(position, position+max(13, saltLength-8))
}

// hexLines는 offset, 16바이트 hex, ASCII를 한 줄로 쓴다. 구간이 있으면 구간 이름을 먼저 쓴다.
func hexLines(data []byte, total int, regions []hexRegion) []debugTextLine {
	if len(regions) == 0 {
		regions = []hexRegion{{"", 0, len(data)}}
	}
	var lines []debugTextLine
	shown := 0
	for _, region := range regions {
		end := min(region.end, len(data))
		if region.start >= end || shown >= debugHexLimit {
			continue
		}
		if region.label != "" {
			lines = append(lines, debugTextLine{2, region.label + ":"})
		}
		for offset := region.start; offset < end && shown < debugHexLimit; offset += 16 {
			chunk := data[offset:min(offset+16, end)]
			shown += len(chunk)
			// 바이트마다 한 글자로 쓴다. 문자열로 바꾸면 UTF-8 글자로 묶여 열이 어긋난다.
			asciiBytes := make([]byte, len(chunk))
			for index, value := range chunk {
				asciiBytes[index] = '.'
				if value >= 0x20 && value < 0x7f {
					asciiBytes[index] = value
				}
			}
			ascii := string(asciiBytes)
			lines = append(lines, debugTextLine{3, fmt.Sprintf("%04x  %-47s  |%s|", offset, spacedHex(chunk), ascii)})
		}
	}
	if total > shown {
		lines = append(lines, debugTextLine{3, fmt.Sprintf("… %d more bytes", total-shown)})
	}
	return lines
}

// describeBytes는 모르는 응답을 한 줄로 쓴다. 글자면 첫 줄을, 아니면 앞 16바이트 hex를 보인다.
func describeBytes(data []byte) string {
	if len(data) == 0 {
		return "nothing"
	}
	if printableShare(data) >= 0.9 {
		line := bannerFirstLine(data)
		if runes := []rune(line); len(runes) > 80 {
			line = string(runes[:80]) + "…"
		}
		return fmt.Sprintf("%q", line)
	}
	preview := data
	if len(preview) > 16 {
		preview = preview[:16]
	}
	text := "binary " + spacedHex(preview)
	if len(data) > 16 {
		text += " …"
	}
	return text
}

func printableShare(data []byte) float64 {
	printable := 0
	for _, value := range data {
		if value == '\r' || value == '\n' || value == '\t' || (value >= 0x20 && value < 0x7f) {
			printable++
		}
	}
	return float64(printable) / float64(len(data))
}

// decodeAutoExchange는 해석기가 정해지지 않은 연결이다(TLS 안쪽, 모르는 단계). 응답 모양으로 HTTP, RESP, 배너를 가린다.
func decodeAutoExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{}
	if exchange.sentAny {
		decoded.sent = []string{describeBytes(exchange.sent)}
	}
	if exchange.receivedAny {
		decoded.received = autoReply(exchange.received)
	}
	return decoded
}

func autoReply(data []byte) []string {
	switch {
	case httpStatusLine(firstLineWithEnd(data)):
		return httpReplyLines(data)
	case len(data) > 0 && strings.ContainsRune("+-:$*", rune(data[0])) && bytes.Contains(data, []byte("\r\n")):
		return []string{respReply(data)}
	}
	if lines := bannerLines(data); len(lines) > 0 {
		return lines
	}
	return []string{describeBytes(data)}
}

func firstLineWithEnd(data []byte) string {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return string(data[:index+1])
	}
	return string(data)
}

// --- banner ---

func decodeBannerExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{}
	if exchange.receivedAny {
		decoded.received = bannerLines(exchange.received)
		if len(decoded.received) == 0 {
			decoded.received = []string{describeBytes(exchange.received)}
		}
	}
	return decoded
}

// bannerLines는 서버가 먼저 보낸 인사말을 풀어 쓴다. 모르는 인사말이면 nil이다.
func bannerLines(data []byte) []string {
	line := bannerFirstLine(data)
	switch {
	case strings.HasPrefix(line, "SSH-"):
		parts := strings.SplitN(line, "-", 3)
		lines := []string{"SSH greeting " + fmt.Sprintf("%q", line)}
		if len(parts) == 3 {
			software, comment, _ := strings.Cut(parts[2], " ")
			text := fmt.Sprintf("protocol %s · software %s", parts[1], software)
			if comment != "" {
				text += " · comment " + comment
			}
			lines = append(lines, text)
		}
		return lines
	case strings.HasPrefix(line, "INFO {"):
		return natsInfoLines(line)
	case strings.HasPrefix(line, "RFB "):
		return []string{"VNC greeting " + fmt.Sprintf("%q", line)}
	case strings.HasPrefix(line, "+OK"):
		return []string{"POP3 greeting " + fmt.Sprintf("%q", line)}
	case strings.HasPrefix(line, "* OK"):
		return []string{"IMAP greeting " + fmt.Sprintf("%q", line)}
	case strings.HasPrefix(line, "220"):
		return []string{"SMTP or FTP greeting " + fmt.Sprintf("%q", line)}
	}
	if detail, ok := amqpHeaderDetail(data); ok {
		return []string{"AMQP protocol header from the broker: " + detail}
	}
	if isHTTP2SettingsFrame(data) {
		return []string{fmt.Sprintf("HTTP/2 SETTINGS frame first (%d settings): a plaintext HTTP/2 server", (int(data[0])<<16|int(data[1])<<8|int(data[2]))/6)}
	}
	if lines := mysqlGreetingLines(data); len(lines) > 0 {
		return lines
	}
	return nil
}

func natsInfoLines(line string) []string {
	lines := []string{"NATS INFO"}
	// 숫자는 json.Number로 읽는다. float64로 읽으면 max_payload 1048576이 1.048576e+06으로 보인다.
	var info map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, "INFO ")))
	decoder.UseNumber()
	if err := decoder.Decode(&info); err != nil {
		if version := natsBannerVersion(line); version != "" {
			lines = append(lines, "version "+version)
		}
		return lines
	}
	var parts []string
	for _, key := range []string{"server_name", "version", "proto", "max_payload", "auth_required", "tls_required", "jetstream"} {
		if value, ok := info[key]; ok {
			parts = append(parts, fmt.Sprintf("%s %v", key, value))
		}
	}
	return append(lines, strings.Join(parts, " · "))
}

// mysqlGreetingLines는 MySQL handshake v10을 읽는다. 인증용 salt는 보이지 않는다.
func mysqlGreetingLines(data []byte) []string {
	version, ok := identifyMySQLGreeting(data)
	if !ok {
		return nil
	}
	lines := []string{"MySQL greeting · protocol 10", "server version " + version}
	position := 5 + len(version) + 1
	if position+4 <= len(data) {
		lines[1] += fmt.Sprintf(" · connection id %d", binary.LittleEndian.Uint32(data[position:position+4]))
	}
	// 그 뒤로 salt 8, filler 1, capability 2, charset 1, status 2, capability 2, salt 길이 1, 예약 10, salt 나머지, 인증 plugin 이름이 온다.
	position += 4 + 8 + 1 + 2 + 1 + 2 + 2
	if position < len(data) {
		saltLength := int(data[position])
		position += 1 + 10 + max(13, saltLength-8)
		if position < len(data) {
			if end := bytes.IndexByte(data[position:], 0); end > 0 {
				lines = append(lines, "auth plugin "+string(data[position:position+end])+" · salt hidden")
			}
		}
	}
	return lines
}

// --- redis, memcached, postgresql ---

func decodeRedisExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{respCommand(exchange.sent)}}
	if exchange.receivedAny {
		decoded.received = []string{respReply(exchange.received)}
	}
	return decoded
}

// respCommand는 RESP 배열로 보낸 명령을 낱말로 쓴다.
func respCommand(data []byte) string {
	lines := strings.Split(strings.TrimSuffix(string(data), "\r\n"), "\r\n")
	var words []string
	for index := 2; index < len(lines); index += 2 {
		words = append(words, lines[index])
	}
	if len(words) == 0 {
		return describeBytes(data)
	}
	return "RESP command " + strings.Join(words, " ")
}

func respReply(data []byte) string {
	line := bannerFirstLine(data)
	if line == "" {
		return describeBytes(data)
	}
	switch line[0] {
	case '+':
		return "RESP simple string " + line[1:]
	case '-':
		return "RESP error " + line[1:]
	case ':':
		return "RESP integer " + line[1:]
	case '$':
		return "RESP bulk string, length " + line[1:]
	case '*':
		return "RESP array, length " + line[1:]
	}
	return describeBytes(data)
}

func decodeMemcachedExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{"text command " + strings.TrimSpace(string(exchange.sent))}}
	if exchange.receivedAny {
		decoded.received = []string{describeBytes(exchange.received)}
	}
	return decoded
}

func decodePostgresExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{describeBytes(exchange.sent)}}
	if bytes.Equal(exchange.sent, postgresSSLRequest) {
		decoded.sent = []string{"SSLRequest · length 8 · code 80877103"}
	}
	if exchange.receivedAny {
		switch {
		case len(exchange.received) == 1 && exchange.received[0] == 'S':
			decoded.received = []string{"'S': the server offers SSL"}
		case len(exchange.received) == 1 && exchange.received[0] == 'N':
			decoded.received = []string{"'N': the server does not offer SSL"}
		default:
			decoded.received = autoReply(exchange.received)
		}
	}
	return decoded
}

// --- TLS ---

var tlsRecordTypes = map[byte]string{20: "change_cipher_spec", 21: "alert", 22: "handshake", 23: "application_data"}

// decodeTLSExchange는 암호화된 record의 종류만 쓴다. 협상 결과는 TLS 단계가 note로 남긴다.
func decodeTLSExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{tlsRecordSummary(exchange.sent, "ClientHello")}}
	if exchange.receivedAny {
		decoded.received = []string{tlsRecordSummary(exchange.received, "")}
		if _, ok := tlsRecordTypes[exchange.received[0]]; !ok {
			decoded.received = append([]string{"not TLS"}, autoReply(exchange.received)...)
		}
	}
	return decoded
}

func tlsRecordSummary(data []byte, first string) string {
	var kinds []string
	for len(data) >= 5 {
		kind, known := tlsRecordTypes[data[0]]
		if !known {
			break
		}
		kinds = append(kinds, kind)
		length := int(binary.BigEndian.Uint16(data[3:5]))
		if 5+length > len(data) {
			break
		}
		data = data[5+length:]
	}
	if len(kinds) == 0 {
		return describeBytes(data)
	}
	text := "TLS records " + strings.Join(collapseRepeats(kinds), ", ")
	if first != "" {
		text = "TLS " + first + " (" + text + ")"
	}
	return text
}

// collapseRepeats는 이어서 반복되는 이름을 "이름 ×n"으로 묶는다.
func collapseRepeats(names []string) []string {
	var collapsed []string
	for index := 0; index < len(names); {
		run := 1
		for index+run < len(names) && names[index+run] == names[index] {
			run++
		}
		if run > 1 {
			collapsed = append(collapsed, fmt.Sprintf("%s ×%d", names[index], run))
		} else {
			collapsed = append(collapsed, names[index])
		}
		index += run
	}
	return collapsed
}

// --- MongoDB ---

func decodeMongoExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{"OP_MSG hello · $db admin"}}
	if len(exchange.sent) >= mongoHeaderSize {
		decoded.sent[0] += fmt.Sprintf(" · request 0x%08x", binary.LittleEndian.Uint32(exchange.sent[4:8]))
		decoded.sentRegions = []hexRegion{{"header", 0, mongoHeaderSize}, {"body", mongoHeaderSize, len(exchange.sent)}}
	}
	data := exchange.received
	if !exchange.receivedAny {
		return decoded
	}
	if len(data) < mongoHeaderSize+5 || binary.LittleEndian.Uint32(data[12:16]) != mongoOpMsg {
		decoded.received = autoReply(data)
		return decoded
	}
	decoded.received = []string{fmt.Sprintf("OP_MSG reply to 0x%08x", binary.LittleEndian.Uint32(data[8:12]))}
	fields := bsonTopLevel(data[mongoHeaderSize+5:])
	var parts []string
	for _, key := range []string{"maxWireVersion", "minWireVersion", "setName", "isWritablePrimary", "ismaster", "secondary", "msg", "readOnly", "maxBsonObjectSize", "connectionId"} {
		if value, ok := fields[key]; ok {
			parts = append(parts, fmt.Sprintf("%s %v", key, value))
		}
	}
	if len(parts) > 0 {
		decoded.received = append(decoded.received, strings.Join(parts, " · "))
	}
	decoded.receivedRegions = []hexRegion{{"header", 0, mongoHeaderSize}, {"body", mongoHeaderSize, len(data)}}
	return decoded
}

// --- HTTP ---

func decodeHTTPExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{}
	head, _, _ := strings.Cut(string(exchange.sent), "\r\n\r\n")
	decoded.sent = strings.Split(head, "\r\n")
	if exchange.receivedAny {
		decoded.received = autoReply(exchange.received)
	}
	return decoded
}

// httpReplyLines는 상태 줄과 판별에 쓰는 header, 본문 첫 줄을 보인다. 쿠키와 인증 header는 값을 숨긴다.
func httpReplyLines(data []byte) []string {
	head, body, _ := strings.Cut(string(data), "\r\n\r\n")
	headLines := strings.Split(head, "\r\n")
	lines := []string{strings.TrimSpace(headLines[0])}
	for _, header := range headLines[1:] {
		name, value, ok := strings.Cut(header, ":")
		if !ok {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case lower == "set-cookie" || lower == "authorization" || lower == "www-authenticate":
			lines = append(lines, name+": (hidden)")
		case lower == "server" || lower == "content-type" || lower == "content-length" || lower == "location" || strings.HasPrefix(lower, "x-clickhouse-"):
			lines = append(lines, name+": "+strings.TrimSpace(value))
		}
	}
	if first := strings.TrimSpace(bannerFirstLine([]byte(body))); first != "" {
		lines = append(lines, "body "+describeBytes([]byte(first)))
	}
	return lines
}

// --- goloop ---

func decodeGoloopExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{}
	decoded.sent, decoded.sentRegions = goloopPacketLines(exchange.sent, true)
	if exchange.receivedAny {
		decoded.received, decoded.receivedRegions = goloopPacketLines(exchange.received, false)
	}
	return decoded
}

// goloopSecure는 읽은 goloop SecureRequest나 SecureResponse다.
type goloopSecure struct {
	name                  string
	protocol, subProtocol uint16
	destination, ttl      byte
	length                int
	node                  string
	channel               string
	suites, aeads         []byte // 요청이 제안한 방식
	suite, aead           byte   // 응답이 고른 방식
	secureError           string
	cut                   bool // payload가 받은 바이트보다 길다
	body                  bool // msgpack 항목을 읽었다
	footer                bool
	hash                  string // "ok", "mismatch", 확인하지 않았으면 ""
	end                   int
}

// parseGoloopSecure는 packet을 읽는다. 읽을 수 없으면 한 줄 설명을 돌려준다.
func parseGoloopSecure(data []byte, request bool) (goloopSecure, string) {
	if len(data) < goloopHeaderSize {
		return goloopSecure{}, describeBytes(data)
	}
	packet := goloopSecure{
		protocol: binary.BigEndian.Uint16(data[0:2]), subProtocol: binary.BigEndian.Uint16(data[2:4]),
		length: int(binary.BigEndian.Uint32(data[26:30])), destination: data[24], ttl: data[25],
	}
	packet.name = map[uint16]string{goloopSecureRequest: "SecureRequest", goloopSecureResponse: "SecureResponse"}[packet.subProtocol]
	if packet.protocol != 0 || packet.name == "" {
		return goloopSecure{}, fmt.Sprintf("not a goloop secure packet: protocol 0x%04x / 0x%04x", packet.protocol, packet.subProtocol)
	}
	if !request {
		packet.node = "hx" + hex.EncodeToString(data[4:24])
	}
	packet.end = goloopHeaderSize + packet.length
	if packet.end > len(data) {
		packet.cut = true
		return packet, ""
	}
	payload := data[goloopHeaderSize:packet.end]
	cursor := &msgpackCursor{data: payload}
	count := cursor.arrayLength()
	packet.channel = cursor.str()
	if request && count >= 4 {
		packet.suites, packet.aeads = cursor.binary(), cursor.binary()
		packet.body = true
	} else if !request && count >= 5 {
		packet.suite, packet.aead = byte(cursor.uint()), byte(cursor.uint())
		cursor.binary()
		packet.secureError = cursor.str()
		packet.body = true
	}
	if packet.end+goloopFooterSize <= len(data) {
		packet.footer = true
		// 보낸 packet은 edc가 만든 것이라 hash를 따로 쓰지 않는다.
		if !request {
			packet.hash = "ok"
			if binary.BigEndian.Uint64(data[packet.end:packet.end+8]) != goloopPacketHash(data[:goloopHeaderSize], payload) {
				packet.hash = "mismatch"
			}
		}
	}
	return packet, ""
}

func (packet goloopSecure) head() string { return packet.headWith(true) }

// headWith는 packet의 첫 줄이다. withLength가 false면 payload 크기를 뺀다. 접힌 줄은 연결마다 크기가 다르면 대표값을 쓸 수 없다.
func (packet goloopSecure) headWith(withLength bool) string {
	text := fmt.Sprintf("%s (auth 0x%04x / 0x%04x) · dest 0x%02x · ttl %d", packet.name, packet.protocol, packet.subProtocol, packet.destination, packet.ttl)
	if withLength {
		text += fmt.Sprintf(" · payload %d B", packet.length)
	}
	if packet.hash != "" {
		text += " · hash " + packet.hash
	}
	return text
}

// goloopPacketLines는 goloop P2P packet의 header, msgpack payload, footer를 풀어 쓴다. 비어 있거나 정상인 값(빈 channel,
// 오류 없음)은 쓰지 않는다. 공개 키는 해석 줄에 쓰지 않고 raw의 hex로만 보인다.
func goloopPacketLines(data []byte, request bool) ([]string, []hexRegion) {
	packet, problem := parseGoloopSecure(data, request)
	if problem != "" {
		return []string{problem}, nil
	}
	lines := []string{packet.head()}
	if packet.node != "" {
		lines = append(lines, "node "+packet.node)
	}
	regions := []hexRegion{{"header", 0, goloopHeaderSize}}
	if packet.cut {
		return append(lines, "payload cut"), regions
	}
	regions = append(regions, hexRegion{"payload (msgpack)", goloopHeaderSize, packet.end})
	var parts []string
	if packet.channel != "" {
		parts = append(parts, fmt.Sprintf("channel %q", packet.channel))
	}
	switch {
	case packet.body && request:
		parts = append(parts, fmt.Sprintf("secure suites [%s] · aead suites [%s]", goloopNames(goloopSuiteNames, packet.suites), goloopNames(goloopAeadNames, packet.aeads)))
	case packet.body:
		parts = append(parts, fmt.Sprintf("secure suite %s · aead suite %s", goloopName(goloopSuiteNames, packet.suite), goloopName(goloopAeadNames, packet.aead)))
		if packet.secureError != "" {
			parts = append(parts, "error "+packet.secureError)
		}
	}
	if len(parts) > 0 {
		lines = append(lines, strings.Join(parts, " · "))
	}
	if packet.footer {
		regions = append(regions, hexRegion{"footer (FNV-64a, ext)", packet.end, packet.end + goloopFooterSize})
	}
	return lines, regions
}

// foldGoloopStep은 goloop 단계가 방식 후보마다 연결을 열어 같은 모양을 반복한 것을 한 묶음으로 접는다. 요청과 응답의 모양은
// 한 번만 쓰고, 연결마다 제안한 방식과 노드가 고른 방식을 한 줄씩 쓴다. 접을 수 없으면(연결이 하나이거나 읽지 못한 packet이
// 있으면) nil이다. 접힌 줄에는 channel 자리가 없어서, 비어 있지 않은 channel이 있으면 접지 않고 packet마다 쓴다.
func foldGoloopStep(step *debugStep) []debugTextLine {
	if len(step.exchanges) < 2 {
		return nil
	}
	type probe struct {
		request, response goloopSecure
		replied           bool
		exchange          *debugExchange
	}
	probes := make([]probe, 0, len(step.exchanges))
	replies := 0
	for _, exchange := range step.exchanges {
		if exchange.dialErr != "" || !exchange.sentAny {
			return nil
		}
		request, problem := parseGoloopSecure(exchange.sent, true)
		if problem != "" || !request.body || request.channel != "" {
			return nil
		}
		current := probe{request: request, exchange: exchange}
		if exchange.receivedAny {
			response, problem := parseGoloopSecure(exchange.received, false)
			if problem != "" || !response.body || response.channel != "" {
				return nil
			}
			current.response, current.replied = response, true
			replies++
		}
		probes = append(probes, current)
	}
	if replies == 0 {
		return nil
	}
	first := probes[0]
	var firstResponse *probe
	nodes := map[string]bool{}
	hashes := map[string]bool{}
	sameRequestSize, sameResponseSize := true, true
	for index := range probes {
		sameRequestSize = sameRequestSize && probes[index].exchange.sentTotal == first.exchange.sentTotal
		if !probes[index].replied {
			continue
		}
		if firstResponse == nil {
			firstResponse = &probes[index]
		}
		sameResponseSize = sameResponseSize && probes[index].exchange.receivedTotal == firstResponse.exchange.receivedTotal
		nodes[probes[index].response.node] = true
		hashes[probes[index].response.hash] = true
	}
	hash := ""
	switch {
	case hashes["mismatch"]:
		hash = " · hash mismatch"
	case len(hashes) == 1 && hashes["ok"]:
		hash = " · hash ok"
	}
	response := firstResponse.response
	response.hash = ""
	sent, received := first.request.headWith(sameRequestSize), response.headWith(sameResponseSize)
	if sameRequestSize {
		sent += fmt.Sprintf(" · %d B", first.exchange.sentTotal)
	}
	if sameResponseSize {
		received += fmt.Sprintf("%s · %d B", hash, firstResponse.exchange.receivedTotal)
	} else {
		received += hash
	}
	lines := []debugTextLine{{1, debugSendMark + sent}, {1, debugReceiveMark + received}}
	summary := fmt.Sprintf("%d connections, each proposes one secure suite", len(probes))
	if len(nodes) == 1 {
		summary = "node " + response.node + " · " + summary
	}
	lines = append(lines, debugTextLine{1, summary})
	for _, current := range probes {
		proposed := fmt.Sprintf("%s + %s", goloopNames(goloopSuiteNames, current.request.suites), goloopNames(goloopAeadNames, current.request.aeads))
		if !current.replied {
			lines = append(lines, debugTextLine{2, fmt.Sprintf("proposed %s → no reply (%s)", proposed, endText(current.exchange))})
			continue
		}
		took := debugDuration(current.exchange.firstReceive - current.exchange.firstSendOrOpen())
		text := fmt.Sprintf("proposed %s → answered %s + %s · %s", proposed,
			goloopName(goloopSuiteNames, current.response.suite), goloopName(goloopAeadNames, current.response.aead), took)
		if current.response.secureError != "" {
			text = fmt.Sprintf("proposed %s → refused (%s) · %s", proposed, current.response.secureError, took)
		}
		if len(nodes) > 1 {
			text += " · node " + current.response.node
		}
		lines = append(lines, debugTextLine{2, text})
	}
	return lines
}

func goloopName(names map[byte]string, value byte) string {
	if name, ok := names[value]; ok {
		return name
	}
	if value == 0 {
		return "none"
	}
	return fmt.Sprintf("unknown(%d)", value)
}

func goloopNames(names map[byte]string, values []byte) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = goloopName(names, value)
	}
	return strings.Join(parts, ", ")
}

// binary는 msgpack bin 값을 읽는다.
func (cursor *msgpackCursor) binary() []byte {
	kind := cursor.kind()
	switch kind {
	case 0xc4:
		return cursor.take(cursor.lengthAfter(1))
	case 0xc5:
		return cursor.take(cursor.lengthAfter(2))
	case 0xc0:
		return nil
	}
	cursor.fail(kind)
	return nil
}

// --- HTTP/2 (h2c) ---

var http2FrameTypes = map[byte]string{0: "DATA", 1: "HEADERS", 3: "RST_STREAM", 4: "SETTINGS", 6: "PING", 7: "GOAWAY", 8: "WINDOW_UPDATE"}

// decodeHTTP2FramesExchange는 평문 HTTP/2 frame의 종류를 차례로 쓴다. 상태와 grpc-status는 h2c 단계가 note로 남긴다.
func decodeHTTP2FramesExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{}
	sent := exchange.sent
	prefix := ""
	if bytes.HasPrefix(sent, []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")) {
		sent, prefix = sent[24:], "connection preface, "
	}
	decoded.sent = []string{"HTTP/2 " + prefix + "frames " + http2FrameList(sent)}
	if exchange.receivedAny {
		decoded.received = []string{"HTTP/2 frames " + http2FrameList(exchange.received)}
		if !isHTTP2SettingsFrame(exchange.received) {
			decoded.received = autoReply(exchange.received)
		}
	}
	return decoded
}

func http2FrameList(data []byte) string {
	var names []string
	for len(data) >= 9 {
		length := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
		name := http2FrameTypes[data[3]]
		if name == "" {
			name = fmt.Sprintf("type %d", data[3])
		}
		if (data[3] == 4 || data[3] == 6) && data[4]&0x01 != 0 {
			name += " ack"
		}
		names = append(names, name)
		if 9+length > len(data) {
			break
		}
		data = data[9+length:]
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// grpcStatusNames는 gRPC 상태 코드의 이름이다.
var grpcStatusNames = map[string]string{
	"0": "OK", "1": "CANCELLED", "2": "UNKNOWN", "3": "INVALID_ARGUMENT", "4": "DEADLINE_EXCEEDED", "5": "NOT_FOUND",
	"7": "PERMISSION_DENIED", "12": "UNIMPLEMENTED", "13": "INTERNAL", "14": "UNAVAILABLE", "16": "UNAUTHENTICATED",
}

// --- AMQP ---

func decodeAMQPExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{describeBytes(exchange.sent)}}
	if bytes.Equal(exchange.sent, amqpProtocolHeader) {
		decoded.sent = []string{"protocol header AMQP 0-9-1"}
	}
	if !exchange.receivedAny {
		return decoded
	}
	data := exchange.received
	if detail, ok := amqpHeaderDetail(data); ok {
		decoded.received = []string{"protocol header from the broker: " + detail}
		return decoded
	}
	if len(data) < 11 || data[0] != amqpFrameMethod {
		decoded.received = autoReply(data)
		return decoded
	}
	size := int(binary.BigEndian.Uint32(data[3:7]))
	// Connection.Start는 class·method 4, version 2, server-properties 길이 4바이트를 앞에 둔다. 그보다 짧으면 풀지 않는다.
	if size < 10 || 7+size > len(data) || binary.BigEndian.Uint16(data[7:9]) != 10 || binary.BigEndian.Uint16(data[9:11]) != 10 {
		decoded.received = autoReply(data)
		return decoded
	}
	payload := data[7 : 7+size]
	decoded.received = []string{fmt.Sprintf("Connection.Start · AMQP %d-%d", payload[4], payload[5])}
	properties := amqpTableStrings(payload[6:])
	var parts []string
	for _, key := range []string{"product", "version", "platform", "cluster_name"} {
		if value := properties[key]; value != "" {
			parts = append(parts, key+" "+value)
		}
	}
	if len(parts) > 0 {
		decoded.received = append(decoded.received, strings.Join(parts, " · "))
	}
	// server-properties 뒤에 mechanisms와 locales가 long string으로 온다.
	position := 6 + 4 + int(binary.BigEndian.Uint32(payload[6:10]))
	var tail []string
	for _, name := range []string{"mechanisms", "locales"} {
		if position+4 > len(payload) {
			break
		}
		length := int(binary.BigEndian.Uint32(payload[position : position+4]))
		if position+4+length > len(payload) {
			break
		}
		tail = append(tail, name+" "+string(payload[position+4:position+4+length]))
		position += 4 + length
	}
	if len(tail) > 0 {
		decoded.received = append(decoded.received, strings.Join(tail, " · "))
	}
	decoded.receivedRegions = []hexRegion{{"frame header", 0, 7}, {"Connection.Start", 7, 7 + size}, {"frame end", 7 + size, 8 + size}}
	return decoded
}

// --- Kafka ---

var kafkaErrorNames = map[int16]string{0: "none", 35: "UNSUPPORTED_VERSION"}
var kafkaAPINames = map[int16]string{0: "Produce", 1: "Fetch", 3: "Metadata", 18: "ApiVersions"}

func decodeKafkaExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{fmt.Sprintf("ApiVersions v0 · correlation 0x%08x · client edc", kafkaCorrelationID)}}
	data := exchange.received
	if !exchange.receivedAny {
		return decoded
	}
	if len(data) < 10 || binary.BigEndian.Uint32(data[4:8]) != kafkaCorrelationID {
		decoded.received = autoReply(data)
		return decoded
	}
	code := int16(binary.BigEndian.Uint16(data[8:10]))
	name := kafkaErrorNames[code]
	if name == "" {
		name = fmt.Sprintf("code %d", code)
	}
	decoded.received = []string{fmt.Sprintf("ApiVersions response · correlation matches · error %s", name)}
	if len(data) >= 14 {
		count := int(binary.BigEndian.Uint32(data[10:14]))
		var known []string
		for index, position := 0, 14; index < count && position+6 <= len(data); index, position = index+1, position+6 {
			key := int16(binary.BigEndian.Uint16(data[position : position+2]))
			if apiName, ok := kafkaAPINames[key]; ok {
				known = append(known, fmt.Sprintf("%s v%d–%d", apiName, binary.BigEndian.Uint16(data[position+2:position+4]), binary.BigEndian.Uint16(data[position+4:position+6])))
			}
		}
		text := fmt.Sprintf("%d APIs", count)
		if len(known) > 0 {
			text += " · " + strings.Join(known, " · ")
		}
		decoded.received = append(decoded.received, text)
	}
	return decoded
}

// --- MQTT ---

func decodeMQTTExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{describeBytes(exchange.sent)}}
	sent := exchange.sent
	if len(sent) >= 14 && sent[0] == 0x10 {
		length := int(sent[1])
		clientID := ""
		if 2+length <= len(sent) && length >= 12 {
			idLength := int(binary.BigEndian.Uint16(sent[12:14]))
			if 14+idLength <= len(sent) {
				clientID = string(sent[14 : 14+idLength])
			}
		}
		decoded.sent = []string{fmt.Sprintf("CONNECT · MQTT 3.1.1 · client id %s · clean session · keep alive %ds", clientID, binary.BigEndian.Uint16(sent[10:12]))}
		if bytes.HasSuffix(sent, []byte{0xe0, 0x00}) {
			decoded.sent = append(decoded.sent, "then DISCONNECT, to close the session at once")
		}
	}
	data := exchange.received
	if exchange.receivedAny {
		if len(data) >= 4 && data[0] == 0x20 && data[1] == 0x02 {
			text, known := mqttConnectReturnCodes[data[3]]
			if !known {
				text = fmt.Sprintf("code %d", data[3])
			}
			decoded.received = []string{fmt.Sprintf("CONNACK · %s (%d) · session present %t", text, data[3], data[2]&1 == 1)}
		} else {
			decoded.received = autoReply(data)
		}
	}
	return decoded
}

// --- Cassandra ---

func decodeCassandraExchange(exchange *debugExchange) debugDecoded {
	decoded := debugDecoded{sent: []string{"OPTIONS · CQL native protocol v4 · stream 0"}}
	data := exchange.received
	if !exchange.receivedAny {
		return decoded
	}
	if len(data) < 9 || data[0]&0x80 == 0 {
		decoded.received = autoReply(data)
		return decoded
	}
	size := int(binary.BigEndian.Uint32(data[5:9]))
	body := data[9:min(9+size, len(data))]
	switch data[4] {
	case cassandraOpSupported:
		decoded.received = []string{fmt.Sprintf("SUPPORTED · response v%d", data[0]&0x7f)}
		options := cqlStringMultimap(body)
		keys := make([]string, 0, len(options))
		for key := range options {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			decoded.received = append(decoded.received, key+": "+strings.Join(options[key], ", "))
		}
	case cassandraOpError:
		decoded.received = []string{fmt.Sprintf("ERROR · response v%d", data[0]&0x7f)}
		if len(body) >= 6 {
			length := int(binary.BigEndian.Uint16(body[4:6]))
			if 6+length <= len(body) {
				decoded.received = append(decoded.received, fmt.Sprintf("code 0x%04x: %s", binary.BigEndian.Uint32(body[0:4]), body[6:6+length]))
			}
		}
	default:
		decoded.received = []string{fmt.Sprintf("opcode 0x%02x", data[4])}
	}
	decoded.receivedRegions = []hexRegion{{"frame header", 0, 9}, {"body", 9, 9 + size}}
	return decoded
}
