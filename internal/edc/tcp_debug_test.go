package edc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// debugTextFor는 address를 기록과 함께 판별하고, --debug 줄을 한 문자열로 이어 돌려준다. 힌트 없는 포트의 전체 흐름이라
// 배너를 기다리고 앞 단계마다 200ms 이상 기다린 뒤에야 뒤쪽 단계에 닿는다. 한 단계의 해석을 보는 시험은 debugTextForStage를 쓴다.
func debugTextFor(t *testing.T, address string, raw bool) (protocolIdentification, string) {
	t.Helper()
	return debugTextWith(t, address, raw, func(recorder *identifyRecorder) protocolIdentification {
		return identifyProtocolWith(context.Background(), address, 0, identifyOptions{recorder: recorder})
	})
}

// debugTextForStage는 stage 하나만 힌트를 받은 것처럼 돌린다. 앞 단계의 대기를 거치지 않고, 이 단계는 응답을 200ms가 아니라
// 1초(identifyStageTimeout) 기다린다. 부하가 걸린 CI runner에서 시험 서버의 응답이 늦어도 놓치지 않는다. 단계 사이의
// 흐름(배너, 앞 단계가 기다린 시간, unknown)을 보는 시험은 debugTextFor를 쓴다.
func debugTextForStage(t *testing.T, address string, raw bool, stage string) (protocolIdentification, string) {
	t.Helper()
	index := slices.IndexFunc(identifyStages, func(candidate identifyStage) bool { return candidate.name == stage })
	if index < 0 {
		t.Fatalf("no identify stage named %q", stage)
	}
	return debugTextWith(t, address, raw, func(recorder *identifyRecorder) protocolIdentification {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return identifyWithStages(ctx, address, 0, identifyStages[index:index+1], 1, recorder)
	})
}

func debugTextWith(t *testing.T, address string, raw bool, identify func(*identifyRecorder) protocolIdentification) (protocolIdentification, string) {
	t.Helper()
	recorder := newIdentifyRecorder(time.Now())
	options := identifyOptions{recorder: recorder}
	identified := identify(recorder)
	_, portText, _ := net.SplitHostPort(address)
	port := 0
	for _, digit := range portText {
		port = port*10 + int(digit-'0')
	}
	lines := tcpDebugLines(debugConnect{local: "127.0.0.1:1", remote: address, connect: time.Millisecond}, recorder, &identified, port, options, raw)
	var texts []string
	for _, line := range lines {
		texts = append(texts, line.Kind+" "+line.Text)
	}
	return identified, strings.Join(texts, "\n")
}

func TestDebugDecodesEachProtocol(t *testing.T) {
	t.Parallel()
	mysqlGreeting := []byte{0x14, 0x00, 0x00, 0x00, 0x0a}
	mysqlGreeting = append(mysqlGreeting, []byte("8.0.36\x00\x07\x00\x00\x00abcdefgh")...)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Server", "edc-test")
		http.SetCookie(writer, &http.Cookie{Name: "session", Value: "secret-value"})
	}))
	t.Cleanup(httpServer.Close)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsServer.Close)
	grpc := h2cServer(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Set("Grpc-Status", "12")
	})
	cases := []struct {
		name    string
		address string
		want    []string
		absent  []string
	}{
		{"redis", identifyListener(t, fakeRedis("+PONG\r\n")), []string{"RESP command PING", "RESP simple string PONG"}, nil},
		{"postgresql", identifyListener(t, replyServer(func(request []byte) []byte {
			if string(request) == string(postgresSSLRequest) {
				return []byte("N")
			}
			return nil
		})), []string{"SSLRequest · length 8 · code 80877103", "'N': the server does not offer SSL"}, nil},
		{"mongodb", identifyListener(t, fakeMongoDB), []string{"OP_MSG hello", "OP_MSG reply to 0x65646321", "maxWireVersion 21", "setName rs0"}, nil},
		{"goloop", identifyListener(t, fakeGoloop), []string{"SecureRequest (auth 0x0000 / 0x0100)", "SecureResponse (auth 0x0000 / 0x0200)", "hash ok", "node hx0123456789abcdef0123456789abcdef01234567", "3 connections, each proposes one secure suite", "proposed none + chacha → answered none + chacha", "proposed ecdhe + aes256 → answered ecdhe + aes256"}, []string{`channel ""`, "error none", "key P-256"}},
		{"amqp", identifyListener(t, func(c net.Conn) {
			if readExactly(c, amqpProtocolHeader) {
				c.Write(amqpStartFrame("RabbitMQ", "4.1.0"))
				io.Copy(io.Discard, c)
			}
		}), []string{"protocol header AMQP 0-9-1", "Connection.Start · AMQP 0-9", "product RabbitMQ · version 4.1.0", "mechanisms PLAIN"}, nil},
		{"kafka", identifyListener(t, fakeKafka), []string{"ApiVersions v0 · correlation 0x65646301", "correlation matches · error none", "2 APIs · ApiVersions v0–3 · Produce v0–9"}, nil},
		{"mqtt", identifyListener(t, fakeMQTT(5, new(atomic.Bool))), []string{"CONNECT · MQTT 3.1.1 · client id edc-", "CONNACK · not authorized (5)"}, nil},
		{"cassandra", identifyListener(t, fakeCassandra(cassandraOpSupported, cqlSupportedBody())), []string{"OPTIONS · CQL native protocol v4", "SUPPORTED · response v4", "CQL_VERSION: 3.4.7", "COMPRESSION: lz4"}, nil},
		{"ssh", identifyListener(t, func(c net.Conn) { io.WriteString(c, "SSH-2.0-OpenSSH_9.6 Ubuntu-3\r\n") }), []string{"protocol 2.0 · software OpenSSH_9.6 · comment Ubuntu-3"}, nil},
		{"mysql", identifyListener(t, func(c net.Conn) { c.Write(mysqlGreeting) }), []string{"MySQL greeting · protocol 10", "server version 8.0.36 · connection id 7"}, []string{"abcdefgh"}},
		{"nats", identifyListener(t, func(c net.Conn) {
			io.WriteString(c, `INFO {"server_name":"n1","version":"2.10.22","proto":1}`+"\r\n")
			io.Copy(io.Discard, c)
		}), []string{"NATS INFO", "server_name n1 · version 2.10.22 · proto 1"}, nil},
		{"http", strings.TrimPrefix(httpServer.URL, "http://"), []string{"GET / HTTP/1.0", "HTTP/1.0 200 OK", "Server: edc-test", "Set-Cookie: (hidden)"}, []string{"secret-value"}},
		{"https", strings.TrimPrefix(tlsServer.URL, "https://"), []string{"handshake TLS 1.3 · cipher", "certificate CN", "not verified here", "inside TLS"}, nil},
		{"grpc over h2c", grpc, []string{"request POST /grpc.health.v1.Health/Check", "grpc-status 12 (UNIMPLEMENTED)", "HTTP/2 connection preface, frames SETTINGS"}, nil},
	}
	// 배너로 맞는 서버(ssh, mysql, nats, grpc)는 연결하자마자 맞아 단계가 없다. 나머지는 그 단계 하나만 돌려, 앞 단계의 대기와
	// 200ms 응답 제한에 걸리지 않게 한다.
	stages := map[string]string{
		"redis": "redis", "postgresql": "postgresql", "mongodb": "mongodb", "goloop": protocolNameGoloopP2P, "amqp": "amqp",
		"kafka": "kafka", "mqtt": "mqtt", "cassandra": "cassandra", "http": "http", "https": "tls",
	}
	identifyCase := func(t *testing.T, name, address string, raw bool) (protocolIdentification, string) {
		if stage, ok := stages[name]; ok {
			return debugTextForStage(t, address, raw, stage)
		}
		return debugTextFor(t, address, raw)
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, text := identifyCase(t, testCase.name, testCase.address, false)
			for _, want := range testCase.want {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in\n%s", want, text)
				}
			}
			// raw hex의 ASCII 열은 16바이트씩 끊기므로, 숨길 값의 앞 4바이트로 본다.
			_, raw := identifyCase(t, testCase.name, testCase.address, true)
			for _, absent := range testCase.absent {
				if strings.Contains(text, absent) {
					t.Errorf("%q must be hidden in\n%s", absent, text)
				}
				if strings.Contains(raw, absent[:4]) || strings.Contains(raw, hex.EncodeToString([]byte(absent[:4]))) {
					t.Errorf("%q must be hidden in the raw lines\n%s", absent, raw)
				}
			}
		})
	}
}

// raw hex도 해석 줄처럼 쿠키와 인증 header의 값, MySQL 인증 salt를 가린다. 나머지 바이트는 그대로 둔다.
func TestDebugRawReceivedHidesSecrets(t *testing.T) {
	t.Parallel()
	reply := "HTTP/1.1 401 Unauthorized\r\nServer: edc-test\r\nSet-Cookie: session=secret-value\r\nWWW-Authenticate: Bearer realm=\"x\"\r\nauthorization: Basic dXNlcjpwYXNz\r\n\r\nbody"
	want := "HTTP/1.1 401 Unauthorized\r\nServer: edc-test\r\nSet-Cookie:" + strings.Repeat("*", 21) + "\r\nWWW-Authenticate:" + strings.Repeat("*", 17) + "\r\nauthorization:" + strings.Repeat("*", 19) + "\r\n\r\nbody"
	if got := string(debugRawReceived([]byte(reply))); got != want {
		t.Errorf("http:\n got %q\nwant %q", got, want)
	}
	// header가 256바이트에서 잘려도 잘린 값은 끝까지 가린다.
	if got := string(debugRawReceived([]byte("HTTP/1.1 200 OK\r\nSet-Cookie: session=sec"))); got != "HTTP/1.1 200 OK\r\nSet-Cookie:"+strings.Repeat("*", 12) {
		t.Errorf("cut header: %q", got)
	}
	greeting := []byte{0x4a, 0x00, 0x00, 0x00, 0x0a}
	greeting = append(greeting, "8.0.36\x00"...)
	greeting = append(greeting, 1, 0, 0, 0)
	greeting = append(greeting, "abcdefgh"...)
	greeting = append(greeting, 0, 0xff, 0xff, 0x21, 0x02, 0x00, 0xff, 0xff, 21)
	greeting = append(greeting, make([]byte, 10)...)
	greeting = append(greeting, "ijklmnopqrst\x00"...)
	greeting = append(greeting, "caching_sha2_password\x00"...)
	masked := debugRawReceived(greeting)
	for _, secret := range []string{"abcdefgh", "ijklmnopqrst"} {
		if bytes.Contains(masked, []byte(secret)) {
			t.Errorf("salt %q is not hidden: %q", secret, masked)
		}
	}
	if !bytes.Contains(masked, []byte("8.0.36")) || !bytes.Contains(masked, []byte("caching_sha2_password")) {
		t.Errorf("greeting lost other bytes: %q", masked)
	}
	if bytes.Contains(greeting, []byte("*")) {
		t.Error("debugRawReceived changed the received bytes")
	}
}

// 선언한 길이가 Connection.Start보다 짧은 AMQP method frame도 panic 없이 풀지 못한 응답으로 둔다.
func TestDebugAMQPRejectsAShortStartFrame(t *testing.T) {
	t.Parallel()
	for size := 4; size <= 9; size++ {
		frame := []byte{amqpFrameMethod, 0, 0, 0, 0, 0, byte(size), 0, 10, 0, 10}
		frame = append(frame, make([]byte, size-4)...)
		decoded := decodeAMQPExchange(&debugExchange{receivedAny: true, received: frame})
		if len(decoded.received) == 0 || strings.HasPrefix(decoded.received[0], "Connection.Start") {
			t.Errorf("size %d: %q", size, decoded.received)
		}
	}
}

// 맞지 않은 단계는 한 줄로 접고, unknown이면 무엇 때문인지 마지막 줄에 쓴다.
func TestDebugExplainsUnknown(t *testing.T) {
	t.Parallel()
	closing := identifyListener(t, func(net.Conn) {})
	identified, text := debugTextFor(t, closing, false)
	if identified.Name != protocolUnknown {
		t.Fatalf("got %+v", identified)
	}
	// 서버가 요청을 읽기 전에 닫으면 RST가 돌아와 reset으로, 읽은 뒤 닫으면 closed로 남는다.
	for _, want := range []string{"no hint for port", "redis sent PING · ", "by the server after", "unknown: 0 steps waited without a reply, 13 closed"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

func TestDebugRawShowsHexRegions(t *testing.T) {
	t.Parallel()
	_, text := debugTextForStage(t, identifyListener(t, fakeGoloop), true, protocolNameGoloopP2P)
	for _, want := range []string{"header:", "payload (msgpack):", "footer (FNV-64a, ext):", "0000  00 00 01 00"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}

// goloop은 방식 후보마다 연결을 열어 같은 모양을 세 번 보낸다. 기본 출력은 모양을 한 번만 쓰고, raw는 연결마다 모두 쓴다.
func TestDebugFoldsGoloopProbes(t *testing.T) {
	t.Parallel()
	address := identifyListener(t, fakeGoloop)
	_, folded := debugTextForStage(t, address, false, protocolNameGoloopP2P)
	if count := strings.Count(folded, "SecureRequest (auth"); count != 1 {
		t.Fatalf("folded output has %d requests:\n%s", count, folded)
	}
	_, full := debugTextForStage(t, identifyListener(t, fakeGoloop), true, protocolNameGoloopP2P)
	if count := strings.Count(full, "SecureRequest (auth"); count != 3 {
		t.Fatalf("raw output has %d requests:\n%s", count, full)
	}
	if !strings.Contains(full, "new connection") {
		t.Fatalf("raw output does not separate the connections:\n%s", full)
	}
}

// 접힌 줄은 노드가 요청한 방식과 다른 방식을 골랐거나 답하지 않은 연결도 보여 준다.
func TestDebugFoldShowsRefusedAndSilentProbes(t *testing.T) {
	t.Parallel()
	only := map[byte]bool{goloopSuiteECDHE: true}
	_, text := debugTextForStage(t, identifyListener(t, fakeGoloopWith(only, goloopAllSupported)), false, protocolNameGoloopP2P)
	for _, want := range []string{"proposed none + chacha → refused (invalid)", "proposed tls + aes128 → refused (invalid)", "proposed ecdhe + aes256 → answered ecdhe + aes256"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "error none") {
		t.Errorf("empty error shown:\n%s", text)
	}
	// 응답마다 payload 크기가 다르면 대표 크기를 쓰지 않는다.
	if strings.Contains(text, "SecureResponse (auth 0x0000 / 0x0200) · dest 0xff · ttl 1 · payload") {
		t.Errorf("response size shown although it differs between connections:\n%s", text)
	}
}

// 접힌 줄에는 channel 자리가 없으므로, 노드가 비어 있지 않은 channel로 답하면 접지 않고 기본 출력에 channel을 쓴다.
func TestDebugDoesNotFoldAwayAChannel(t *testing.T) {
	t.Parallel()
	withChannel := func(connection net.Conn) {
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, goloopHeaderSize)
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		rest := make([]byte, int(binary.BigEndian.Uint32(header[26:30]))+goloopFooterSize)
		if _, err := io.ReadFull(connection, rest); err != nil {
			return
		}
		response := []byte{0x95, 0xa3, 'f', 'o', 'o', rest[4], rest[7], 0xc4, 0x00, 0xa0}
		connection.Write(goloopTestPacket(goloopSecureResponse, identifyTestPeerID, response))
		io.Copy(io.Discard, connection)
	}
	_, text := debugTextForStage(t, identifyListener(t, withChannel), false, protocolNameGoloopP2P)
	if !strings.Contains(text, `channel "foo"`) {
		t.Fatalf("channel missing in\n%s", text)
	}
}

// 맞은 단계 하나뿐이면 result 줄이 단계 줄과 같은 말이라 쓰지 않는다. 앞 단계가 기다렸으면 전체 시간을 위해 쓴다.
func TestDebugResultLineOnlyWhenItAddsSomething(t *testing.T) {
	t.Parallel()
	single := identifyListener(t, func(c net.Conn) { io.WriteString(c, "SSH-2.0-OpenSSH_9.6\r\n") })
	if _, text := debugTextFor(t, single, false); strings.Contains(text, "result ") {
		t.Fatalf("redundant result line:\n%s", text)
	}
	_, silent := debugTextFor(t, identifyListener(t, fakeRedis("+PONG\r\n")), false)
	if !strings.Contains(silent, "result redis in ") {
		t.Fatalf("result line missing after a banner wait:\n%s", silent)
	}
}

// 방향 글머리는 색이 없어도 보낸 줄과 받은 줄을 가른다. 색은 그 위에 얹는다.
func TestFormatDebugLinesMarksDirectionAndColor(t *testing.T) {
	lines := []DebugLine{
		{AtMS: 1, Kind: "redis", Text: "matched redis · 1.0ms"},
		{AtMS: -1, Depth: 1, Text: debugSendMark + "RESP command PING · 14 B"},
		{AtMS: -1, Depth: 1, Text: debugReceiveMark + "RESP simple string PONG · 7 B in 0.4ms"},
		{AtMS: 3, Kind: "mongodb", Text: "sent hello · reset by the server after 0.1ms, no reply"},
		{AtMS: -1, Kind: "result", Text: "unknown: 1 steps waited without a reply, 0 closed, 0 replied with something else"},
	}
	plain := strings.Join(formatDebugLines(lines, false), "\n")
	if strings.Contains(plain, "\033[") {
		t.Fatalf("escape in plain output: %q", plain)
	}
	for _, want := range []string{"→ RESP command PING · 14 B", "← RESP simple string PONG"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in\n%s", want, plain)
		}
	}
	colored := formatDebugLines(lines, true)
	if len(colored) != len(lines)+1 {
		t.Fatalf("got %d lines", len(colored))
	}
	for index, want := range map[int]string{
		1: "matched redis",     // 맞은 단계
		2: "RESP command PING", // 보낸 줄
		3: "PONG",              // 받은 줄
		4: "reset by the server",
		5: "unknown",
	} {
		if !strings.Contains(colored[index], "\033[") || !strings.Contains(colored[index], want) {
			t.Errorf("line %d is not colored or lost %q: %q", index, want, colored[index])
		}
	}
	// 보낸 줄과 받은 줄의 글머리는 서로 다른 색이다.
	sendColor := strings.SplitN(colored[2], "→", 2)[0]
	receiveColor := strings.SplitN(colored[3], "←", 2)[0]
	if sendColor == receiveColor {
		t.Errorf("send and receive share a color: %q", colored[2])
	}
}

// 대상 서버가 보낸 escape와 긴 줄은 기록에 그대로 들어가지 않는다.
func TestDebugSanitizesServerText(t *testing.T) {
	t.Parallel()
	banner := "SSH-2.0-\x1b[2J\x1b]0;owned\x07" + strings.Repeat("A", 4000)
	_, text := debugTextFor(t, identifyListener(t, func(c net.Conn) { io.WriteString(c, banner+"\r\n") }), false)
	if strings.ContainsAny(text, "\x1b\x07") {
		t.Fatalf("control characters left: %q", text[:80])
	}
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > debugTextLimit+40 {
			t.Fatalf("line not capped: %d runes", len([]rune(line)))
		}
	}
}

func TestHexLinesKeepsOneCharacterPerByte(t *testing.T) {
	lines := hexLines([]byte{0xd9, 0x21, 0x34, 0xe2, 0x82, 0xac, 0x41, 0x00}, 8, nil)
	if len(lines) != 1 || !strings.HasSuffix(lines[0].text, "|.!4...A.|") {
		t.Fatalf("hex line %q", lines[0].text)
	}
}

func TestDebugFlagValues(t *testing.T) {
	cases := map[string]debugFlag{"true": {on: true}, "false": {}, "raw": {on: true, raw: true}}
	for text, want := range cases {
		var got debugFlag
		if err := got.Set(text); err != nil || got != want {
			t.Fatalf("%s: %+v %v", text, got, err)
		}
	}
	var got debugFlag
	if err := got.Set("verbose"); err == nil {
		t.Fatal("verbose accepted")
	}
}

// --debug의 기록은 JSON의 debug로 남고, --redact가 그 안의 IP도 가린다. --debug가 없으면 debug가 없다.
func TestRunTCPCheckDebugJSON(t *testing.T) {
	address := identifyListener(t, fakeRedis("+PONG\r\n"))
	run := func(args ...string) map[string]interface{} {
		t.Helper()
		previousOut, previousErr := os.Stdout, os.Stderr
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer devNull.Close()
		os.Stdout, os.Stderr = write, devNull
		output := make(chan []byte, 1)
		go func() {
			data, _ := io.ReadAll(read)
			output <- data
		}()
		code := runTCP(append([]string{"check", "--json", "-"}, append(args, address)...), "test")
		os.Stdout, os.Stderr = previousOut, previousErr
		write.Close()
		data := <-output
		var report struct {
			Results []map[string]interface{} `json:"results"`
		}
		if err := json.Unmarshal(data, &report); err != nil || code != 0 || len(report.Results) != 1 {
			t.Fatalf("%v code %d: %s", err, code, data)
		}
		return report.Results[0]
	}
	if result := run(); result["debug"] != nil {
		t.Fatalf("debug without --debug: %v", result["debug"])
	}
	result := run("--debug")
	lines, ok := result["debug"].([]interface{})
	if !ok || len(lines) == 0 {
		t.Fatalf("no debug lines: %v", result)
	}
	encoded, _ := json.Marshal(lines)
	if !strings.Contains(string(encoded), "RESP simple string PONG") {
		t.Fatalf("debug lines: %s", encoded)
	}
	redacted, _ := json.Marshal(run("-vv", "--redact")["debug"])
	if strings.Contains(string(redacted), "127.0.0.1") {
		t.Fatalf("--redact left an address in debug: %s", redacted)
	}
}
