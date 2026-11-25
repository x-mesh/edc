package edc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// debugTextFor는 address를 기록과 함께 판별하고, --debug 줄을 한 문자열로 이어 돌려준다.
func debugTextFor(t *testing.T, address string, raw bool) (protocolIdentification, string) {
	t.Helper()
	recorder := newIdentifyRecorder(time.Now())
	options := identifyOptions{recorder: recorder}
	identified := identifyProtocolWith(context.Background(), address, 0, options)
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
	mysqlGreeting := []byte{0x10, 0x00, 0x00, 0x00, 0x0a}
	mysqlGreeting = append(mysqlGreeting, []byte("8.0.36\x00abcdefgh")...)
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
		{"goloop", identifyListener(t, fakeGoloop), []string{"SecureRequest (auth 0x0000 / 0x0100)", "SecureResponse (auth 0x0000 / 0x0200)", "hash ok", "node hx0123456789abcdef0123456789abcdef01234567", "secure suites [none] · aead suites [chacha]"}, nil},
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
		{"mysql", identifyListener(t, func(c net.Conn) { c.Write(mysqlGreeting) }), []string{"MySQL greeting · protocol 10", "server version 8.0.36"}, []string{"abcdefgh"}},
		{"nats", identifyListener(t, func(c net.Conn) {
			io.WriteString(c, `INFO {"server_name":"n1","version":"2.10.22","proto":1}`+"\r\n")
			io.Copy(io.Discard, c)
		}), []string{"NATS INFO", "server_name n1 · version 2.10.22 · proto 1"}, nil},
		{"http", strings.TrimPrefix(httpServer.URL, "http://"), []string{"GET / HTTP/1.0", "HTTP/1.0 200 OK", "Server: edc-test", "Set-Cookie: (hidden)"}, []string{"secret-value"}},
		{"https", strings.TrimPrefix(tlsServer.URL, "https://"), []string{"handshake TLS 1.3 · cipher", "certificate CN", "not verified here", "inside TLS"}, nil},
		{"grpc over h2c", grpc, []string{"request POST /grpc.health.v1.Health/Check", "grpc-status 12 (UNIMPLEMENTED)", "HTTP/2 connection preface, frames SETTINGS"}, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, text := debugTextFor(t, testCase.address, false)
			for _, want := range testCase.want {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in\n%s", want, text)
				}
			}
			for _, absent := range testCase.absent {
				if strings.Contains(text, absent) {
					t.Errorf("%q must be hidden in\n%s", absent, text)
				}
			}
		})
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
	_, text := debugTextFor(t, identifyListener(t, fakeGoloop), true)
	for _, want := range []string{"header:", "payload (msgpack):", "footer (FNV-64a, ext):", "0000  00 00 01 00"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
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
