package edc

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
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

// identifyListener는 연결마다 serve를 부르는 local server다.
func identifyListener(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				serve(connection)
			}()
		}
	}()
	return listener.Addr().String()
}

var identifyTestPeerID = []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67}

func goloopTestPacket(subProtocol uint16, src, payload []byte) []byte {
	header := make([]byte, goloopHeaderSize)
	binary.BigEndian.PutUint16(header[2:4], subProtocol)
	copy(header[4:24], src)
	header[24] = goloopDestPeer
	header[25] = 1
	binary.BigEndian.PutUint32(header[26:30], uint32(len(payload)))
	footer := make([]byte, goloopFooterSize)
	binary.BigEndian.PutUint64(footer[:8], goloopPacketHash(header, payload))
	return append(append(header, payload...), footer...)
}

var goloopAllSupported = map[byte]bool{1: true, 2: true, 3: true}

// fakeGoloop은 모든 방식을 지원하는 goloop 노드다.
func fakeGoloop(connection net.Conn) {
	fakeGoloopWith(goloopAllSupported, goloopAllSupported)(connection)
}

// fakeGoloopWith는 goloop 노드처럼 30바이트 header를 기다린다. SecureRequest면 handleSecureRequest와 같은 규칙으로 방식을 골라
// SecureResponse로 답하고, 아니면 끊는다.
func fakeGoloopWith(suites, aeads map[byte]bool) func(net.Conn) {
	return func(connection net.Conn) {
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, goloopHeaderSize)
		if _, err := io.ReadFull(connection, header); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(header[26:30])
		if binary.BigEndian.Uint16(header[0:2]) != 0 || binary.BigEndian.Uint16(header[2:4]) != goloopSecureRequest || length > 4096 {
			return
		}
		rest := make([]byte, int(length)+goloopFooterSize)
		if _, err := io.ReadFull(connection, rest); err != nil {
			return
		}
		if binary.BigEndian.Uint64(rest[length:length+8]) != goloopPacketHash(header, rest[:length]) {
			return
		}
		// edc의 요청은 [channel "", suite 1개, aead 1개, 공개 키]다.
		requestSuite, requestAead := rest[4], rest[7]
		suite, aead, secureError := byte(0), byte(0), ""
		if suites[requestSuite] {
			suite = requestSuite
			if aeads[requestAead] {
				aead = requestAead
			}
			if suite != goloopSuiteNone && aead == 0 {
				secureError = "invalid"
			}
		} else {
			secureError = "invalid"
		}
		response := []byte{0x95, 0xa0, suite, aead, 0xc4, 0x00, 0xa0 | byte(len(secureError))}
		response = append(response, secureError...)
		connection.Write(goloopTestPacket(goloopSecureResponse, identifyTestPeerID, response))
		io.Copy(io.Discard, connection)
	}
}

func TestIdentifyProtocol(t *testing.T) {
	t.Parallel()
	mysqlGreeting := []byte{0x10, 0x00, 0x00, 0x00, 0x0a}
	mysqlGreeting = append(mysqlGreeting, []byte("8.0.36\x00abcdefgh")...)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsServer.Close)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Server", "edc-test")
	}))
	t.Cleanup(httpServer.Close)
	cases := []struct {
		name, address, protocol, detail, basis string
	}{
		{"ssh", identifyListener(t, func(c net.Conn) { io.WriteString(c, "SSH-2.0-OpenSSH_9.6\r\n") }), "ssh", "SSH-2.0-OpenSSH_9.6", protocolBasisBanner},
		{"banner in two pieces", identifyListener(t, func(c net.Conn) {
			io.WriteString(c, "SS")
			time.Sleep(50 * time.Millisecond)
			io.WriteString(c, "H-2.0-OpenSSH_9.6\r\n")
		}), "ssh", "SSH-2.0-OpenSSH_9.6", protocolBasisBanner},
		{"smtp", identifyListener(t, func(c net.Conn) { io.WriteString(c, "220 mail.example ESMTP Postfix\r\n") }), "smtp", "220 mail.example ESMTP Postfix", protocolBasisBanner},
		{"mysql", identifyListener(t, func(c net.Conn) { c.Write(mysqlGreeting) }), "mysql", "8.0.36", protocolBasisBanner},
		{"https over http/1.1", strings.TrimPrefix(tlsServer.URL, "https://"), "https", "HTTP/1.0 200 OK", protocolBasisTLSInner},
		{"http", strings.TrimPrefix(httpServer.URL, "http://"), "http", "HTTP/1.0 200 OK, server edc-test", protocolBasisHTTP},
		{"goloop", identifyListener(t, fakeGoloop), protocolNameGoloopP2P, "node hx0123456789abcdef0123456789abcdef01234567", protocolBasisGoloop},
		{"tls alert", identifyTLSAlertServer(t), "tls", "alert: tls: no application protocol", protocolBasisTLS},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := identifyProtocol(context.Background(), testCase.address, 0)
			if got.Name != testCase.protocol || got.Detail != testCase.detail || got.Basis != testCase.basis {
				t.Fatalf("got %+v, want %s %q %s", got, testCase.protocol, testCase.detail, testCase.basis)
			}
		})
	}
}

// identifyTestTarget은 판별 단계 하나를 직접 부를 때 쓰는 대상이다.
func identifyTestTarget(address string) identifyTarget {
	host, _, _ := net.SplitHostPort(address)
	return identifyTarget{address: address, host: host, wait: identifyStageTimeout, open: func(ctx context.Context, wait time.Duration) (net.Conn, error) {
		return dialIdentify(ctx, address, wait)
	}}
}

// replyServer는 첫 요청 조각을 읽고 reply가 돌려준 바이트로 답한 뒤, 상대가 닫을 때까지 기다린다. nil이면 바로 닫는다.
func replyServer(reply func(request []byte) []byte) func(net.Conn) {
	return func(connection net.Conn) {
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		buffer := make([]byte, 1024)
		count, err := connection.Read(buffer)
		if err != nil {
			return
		}
		answer := reply(buffer[:count])
		if answer == nil {
			return
		}
		connection.Write(answer)
		io.Copy(io.Discard, connection)
	}
}

func fakeRedis(answer string) func(net.Conn) {
	return replyServer(func(request []byte) []byte {
		if string(request) == "*1\r\n$4\r\nPING\r\n" {
			return []byte(answer)
		}
		return []byte("-ERR unknown command\r\n")
	})
}

func fakeMongoDB(connection net.Conn) {
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	header := make([]byte, mongoHeaderSize)
	if _, err := io.ReadFull(connection, header); err != nil {
		return
	}
	length := binary.LittleEndian.Uint32(header[0:4])
	if binary.LittleEndian.Uint32(header[12:16]) != mongoOpMsg || length > 4096 {
		return
	}
	if _, err := io.ReadFull(connection, make([]byte, length-mongoHeaderSize)); err != nil {
		return
	}
	var elements []byte
	elements = append(elements, 0x08)
	elements = append(elements, "isWritablePrimary\x00"...)
	elements = append(elements, 1)
	elements = append(elements, 0x02)
	elements = append(elements, "setName\x00"...)
	elements = binary.LittleEndian.AppendUint32(elements, 4)
	elements = append(elements, "rs0\x00"...)
	elements = append(elements, 0x10)
	elements = append(elements, "maxWireVersion\x00"...)
	elements = binary.LittleEndian.AppendUint32(elements, 21)
	elements = append(elements, 0x00)
	reply := make([]byte, mongoHeaderSize)
	reply = binary.LittleEndian.AppendUint32(reply, 0)
	reply = append(reply, 0x00)
	reply = binary.LittleEndian.AppendUint32(reply, uint32(len(elements)+4))
	reply = append(reply, elements...)
	binary.LittleEndian.PutUint32(reply[0:4], uint32(len(reply)))
	binary.LittleEndian.PutUint32(reply[4:8], 99)
	copy(reply[8:12], header[4:8])
	binary.LittleEndian.PutUint32(reply[12:16], mongoOpMsg)
	connection.Write(reply)
	io.Copy(io.Discard, connection)
}

// tlsListener는 ALPN 없이 TLS를 받고, handshake 뒤 serve를 부르는 local server다.
func tlsListener(t *testing.T, certificates []tls.Certificate, serve func(net.Conn)) string {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certificates})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				if err := connection.(*tls.Conn).Handshake(); err != nil {
					return
				}
				serve(connection)
			}()
		}
	}()
	return listener.Addr().String()
}

func h2Server(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "https://")
}

func TestIdentifyClientFirstAndInsideTLS(t *testing.T) {
	t.Parallel()
	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(certificateServer.Close)
	certificates := certificateServer.TLS.Certificates
	cases := []struct {
		name, address, protocol, detail string
	}{
		{"redis", identifyListener(t, fakeRedis("+PONG\r\n")), "redis", "+PONG"},
		{"redis with password", identifyListener(t, fakeRedis("-NOAUTH Authentication required.\r\n")), "redis", "-NOAUTH Authentication required."},
		{"memcached", identifyListener(t, replyServer(func(request []byte) []byte {
			if string(request) == "version\r\n" {
				return []byte("VERSION 1.6.21\r\n")
			}
			return []byte("ERROR\r\n")
		})), "memcached", "VERSION 1.6.21"},
		{"postgresql", identifyListener(t, replyServer(func(request []byte) []byte {
			if string(request) == string(postgresSSLRequest) {
				return []byte("N")
			}
			return nil
		})), "postgresql", "SSL not offered"},
		{"mongodb", identifyListener(t, fakeMongoDB), "mongodb", "maxWireVersion 21, replica set rs0"},
		{"https over h2", h2Server(t, func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Server", "edc-h2")
			http.NotFound(writer, request)
		}), "https", "HTTP/2 404, server edc-h2"},
		{"grpc", h2Server(t, func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/grpc")
			writer.Header().Set("Grpc-Status", "12")
		}), "grpc", "HTTP/2 200, grpc-status 12"},
		{"imaps", tlsListener(t, certificates, func(c net.Conn) {
			io.WriteString(c, "* OK IMAP4rev1 ready\r\n")
			io.Copy(io.Discard, c)
		}), "imaps", "* OK IMAP4rev1 ready"},
		{"rediss", tlsListener(t, certificates, fakeRedis("+PONG\r\n")), "rediss", "+PONG"},
		// 인사말이 안쪽 대기보다 늦게 오면 HTTP 요청의 응답 자리에 온다.
		{"smtps with a late greeting", tlsListener(t, certificates, func(c net.Conn) {
			time.Sleep(identifyTLSBannerWait + 100*time.Millisecond)
			io.WriteString(c, "220 mail.example ESMTP ready\r\n")
			io.Copy(io.Discard, c)
		}), "smtps", "220 mail.example ESMTP ready"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := identifyProtocol(context.Background(), testCase.address, 0)
			if got.Name != testCase.protocol || got.Detail != testCase.detail {
				t.Fatalf("got %+v, want %s %q", got, testCase.protocol, testCase.detail)
			}
		})
	}
}

func TestIdentifyOrderPutsHintedPortFirst(t *testing.T) {
	stages, hinted := identifyOrder(6379)
	if hinted != 1 || stages[0].name != "redis" || len(stages) != len(identifyStages) {
		t.Fatalf("6379: hinted %v, first %s, %d stages", hinted, stages[0].name, len(stages))
	}
	if stages, hinted := identifyOrder(22); hinted != 0 || stages[0].name != identifyStages[0].name {
		t.Fatalf("22: hinted %v, first %s", hinted, stages[0].name)
	}
}

// port는 순서만 정한다. 6379에 SSH가 있어도 Redis 탐침의 응답으로 온 인사말로 SSH를 찾고, 힌트가 맞으면 배너를 기다리지 않는다.
func TestIdentifyHintOnlyReorders(t *testing.T) {
	t.Parallel()
	redisFirst, _ := identifyOrder(6379)
	ssh := identifyListener(t, func(c net.Conn) {
		io.WriteString(c, "SSH-2.0-OpenSSH_9.6\r\n")
		io.Copy(io.Discard, c)
	})
	if got := identifyWithStages(context.Background(), ssh, 0, redisFirst, 1, nil); got.Name != "ssh" {
		t.Fatalf("ssh behind a redis port: %+v", got)
	}
	redis := identifyListener(t, fakeRedis("+PONG\r\n"))
	started := time.Now()
	if got := identifyWithStages(context.Background(), redis, 0, redisFirst, 1, nil); got.Name != "redis" {
		t.Fatalf("hinted redis: %+v", got)
	}
	if elapsed := time.Since(started); elapsed >= identifyBannerWait {
		t.Fatalf("hinted probe waited for a banner: %v", elapsed)
	}
}

// identifyTLSAlertServer는 ALPN이 맞지 않으면 alert로 거절하는 TLS server다.
func identifyTLSAlertServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{NextProtos: []string{"edc-only"}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "https://")
}

func TestProtocolDetailTextFromHostileBanner(t *testing.T) {
	banner := "SSH-2.0-\x1b[2J\x1b]0;owned\x07" + strings.Repeat("A", 4000)
	address := identifyListener(t, func(c net.Conn) { io.WriteString(c, banner+"\r\n") })
	identified := identifyProtocol(context.Background(), address, 0)
	got := annotateProtocol(Result{Probe: "tcp.check", Status: StatusPass}, identified, true)
	detail := got.Metrics["protocol_detail"].(string)
	if strings.ContainsAny(detail, "\x1b\x07") || strings.ContainsAny(got.Summary, "\x1b\x07") {
		t.Fatalf("control characters left: %q", detail)
	}
	if runes := []rune(detail); len(runes) > identifyDetailMaxRunes+1 || !strings.HasPrefix(detail, "SSH-2.0-") {
		t.Fatalf("detail not capped: %d runes %q", len(runes), detail[:40])
	}
}

func TestIdentifyProtocolUnknown(t *testing.T) {
	t.Parallel()
	t.Run("silent server stops at the context deadline", func(t *testing.T) {
		t.Parallel()
		address := identifyListener(t, func(c net.Conn) { io.Copy(io.Discard, c) })
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		started := time.Now()
		got := identifyProtocol(ctx, address, 0)
		if got.Name != protocolUnknown || len(got.FirstBytes) != 0 {
			t.Fatalf("got %+v", got)
		}
		if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
			t.Fatalf("took %v past the deadline", elapsed)
		}
	})
	t.Run("unknown banner keeps the first bytes", func(t *testing.T) {
		t.Parallel()
		address := identifyListener(t, func(c net.Conn) { c.Write([]byte{0xde, 0xad, 0xbe, 0xef}) })
		got := identifyProtocol(context.Background(), address, 0)
		if got.Name != protocolUnknown || string(got.FirstBytes) != "\xde\xad\xbe\xef" {
			t.Fatalf("got %+v", got)
		}
	})
	// backend 없는 load balancer처럼 연결 직후 닫는 서버다. port가 goloop를 가리키지 않으면 goloop 재시도로 시간을 쓰지 않는다.
	t.Run("immediate close", func(t *testing.T) {
		t.Parallel()
		address := identifyListener(t, func(net.Conn) {})
		started := time.Now()
		if got := identifyProtocol(context.Background(), address, 0); got.Name != protocolUnknown {
			t.Fatalf("got %+v", got)
		}
		if elapsed := time.Since(started); elapsed > identifyGoloopRetryDelay {
			t.Fatalf("immediate close took %v", elapsed)
		}
	})
}

// HTTP 탐침은 Host: header 없이 보낸다. Redis 단계가 놓친 Redis가 HTTP 탐침을 받아도 GET 명령의 오류로 답해 Redis로 찾는다.
func TestIdentifyHTTPProbeFindsMissedRedis(t *testing.T) {
	t.Parallel()
	var request atomic.Value
	address := identifyListener(t, replyServer(func(data []byte) []byte {
		request.Store(string(data))
		return []byte("-ERR wrong number of arguments for 'get' command\r\n")
	}))
	got, _, ok := identifyHTTP(context.Background(), identifyTestTarget(address))
	if !ok || got.Name != "redis" {
		t.Fatalf("got %+v ok %v", got, ok)
	}
	if sent, _ := request.Load().(string); strings.Contains(strings.ToLower(sent), "host:") {
		t.Fatalf("HTTP probe sent a Host header: %q", sent)
	}
}

// naver.com:80처럼 Host: header가 없으면 답하지 않는 HTTP 서버다. Host 없는 요청이 기다리다 끝나면 Host를 넣어 다시 보낸다.
func TestIdentifyHTTPRetriesWithHost(t *testing.T) {
	t.Parallel()
	var connections atomic.Int32
	address := identifyListener(t, func(c net.Conn) {
		connections.Add(1)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		buffer := make([]byte, 1024)
		count, _ := c.Read(buffer)
		if !strings.Contains(string(buffer[:count]), "Host: ") {
			io.Copy(io.Discard, c)
			return
		}
		io.WriteString(c, "HTTP/1.0 400 Bad request\r\n\r\n")
	})
	// 대기는 기본값(identifyStageTimeout, 1초)을 쓴다. 첫 연결은 Host가 없어 서버가 답하지 않으므로 대기가 다 차야 다시 보내고,
	// 그만큼 두 번째 연결의 응답을 기다릴 여유도 생긴다. 300ms로 줄이면 부하가 걸린 CI runner에서 그 응답을 놓친다.
	got, _, ok := identifyHTTP(context.Background(), identifyTestTarget(address))
	if !ok || got.Name != "http" || got.Detail != "HTTP/1.0 400 Bad request" || connections.Load() != 2 {
		t.Fatalf("got %+v ok %v after %d connections", got, ok, connections.Load())
	}
	// 바로 닫는 서버에는 다시 보내지 않는다.
	var closed atomic.Int32
	closing := identifyListener(t, func(net.Conn) { closed.Add(1) })
	if _, _, ok := identifyHTTP(context.Background(), identifyTestTarget(closing)); ok || closed.Load() != 1 {
		t.Fatalf("closing server: ok %v after %d connections", ok, closed.Load())
	}
}

// 버전과 세 자리 상태 코드가 있는 상태 줄만 HTTP로 본다. "HTTP/1."로 시작할 뿐인 배너는 HTTP가 아니다.
func TestIdentifyHTTPNeedsAWholeStatusLine(t *testing.T) {
	t.Parallel()
	for reply, want := range map[string]bool{
		"HTTP/1.1 200 OK\r\n\r\n":         true,
		"HTTP/1.0 400\r\n\r\n":            true,
		"HTTP/2 404 Not Found\r\n\r\n":    true,
		"HTTP/1.\r\n\r\n":                 false,
		"HTTP/1.1\r\n\r\n":                false,
		"HTTP/1.1 OK\r\n\r\n":             false,
		"HTTP/1.1 2000 OK\r\n\r\n":        false,
		"HTTP/1.1 600 Odd\r\n\r\n":        false,
		"HTTP/1.9 200 OK\r\n\r\n":         false,
		"HTTP/1.1 200 OK with no newline": false,
	} {
		address := identifyListener(t, replyServer(func([]byte) []byte { return []byte(reply) }))
		got, _, ok := identifyHTTP(context.Background(), identifyTestTarget(address))
		if ok != want || (want && got.Name != "http") {
			t.Errorf("%q: got %+v ok %v", reply, got, ok)
		}
	}
}

// 연결만 받고 답하지 않는 서버의 판별 시간은 연결 시간에 따라 늘어난다. 실제 네트워크의 값을 보려고 연결 시간을 바꿔 잰다.
// 판별 전체는 identifyTotalTimeout에서 멈추고, 상한 때문에 멈췄다는 것을 결과에 남긴다. 연결 시간 150ms면 상한 없이 10초를 넘었다.
func TestIdentifySilentServerStopsAtTotalTimeout(t *testing.T) {
	t.Parallel()
	address := identifyListener(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	started := time.Now()
	got := identifyProtocol(context.Background(), address, 150*time.Millisecond)
	elapsed := time.Since(started)
	if got.Name != protocolUnknown || !got.TimedOut {
		t.Fatalf("got %+v", got)
	}
	if elapsed > identifyTotalTimeout+500*time.Millisecond {
		t.Fatalf("took %v, past the %v cap", elapsed, identifyTotalTimeout)
	}
	annotated := annotateProtocol(Result{Probe: "tcp.check", Status: StatusPass}, got, true)
	if annotated.Metrics["protocol_timed_out"] != true || !strings.Contains(annotated.Evidence[0].Value, "stopped") {
		t.Fatalf("timeout not visible: %+v", annotated)
	}
}

// 모든 탐침을 바로 끊는 서버는 상한에 걸리지 않고, 시간을 늘려도 결과가 같으므로 next를 안내하지 않는다.
func TestIdentifyUnknownWithoutTimeout(t *testing.T) {
	t.Parallel()
	address := identifyListener(t, func(net.Conn) {})
	got := identifyProtocol(context.Background(), address, 0)
	if got.Name != protocolUnknown || got.TimedOut || !got.NoReply || got.Waited || got.Suggest != 0 {
		t.Fatalf("got %+v", got)
	}
	annotated := annotateProtocol(Result{Probe: "tcp.check", Metrics: map[string]interface{}{"address": address}}, got, true)
	if annotated.Next != "" || !strings.Contains(annotated.Evidence[0].Value, "closed every probe") {
		t.Fatalf("annotated %+v", annotated)
	}
}

// slowRedis는 PING에 600ms 늦게 답하고 다른 탐침에는 답하지 않는다. 기본 대기(200ms)로는 놓치고, 단계마다 1초를 기다리면 찾는다.
func slowRedis(connection net.Conn) {
	connection.SetDeadline(time.Now().Add(10 * time.Second))
	buffer := make([]byte, 1024)
	count, err := connection.Read(buffer)
	if err != nil {
		return
	}
	if string(buffer[:count]) == redisPing {
		time.Sleep(600 * time.Millisecond)
		io.WriteString(connection, "+PONG\r\n")
	}
	io.Copy(io.Discard, connection)
}

func TestIdentifySlowServerNeedsLongerLimit(t *testing.T) {
	t.Parallel()
	address := identifyListener(t, slowRedis)
	got := identifyProtocol(context.Background(), address, 0)
	if got.Name != protocolUnknown || !got.Waited || got.Suggest != identifySuggestedLimit {
		t.Fatalf("default: %+v", got)
	}
	annotated := annotateProtocol(Result{Probe: "tcp.check", Metrics: map[string]interface{}{"address": address}}, got, false)
	if want := "edc tcp check --identify=10s " + address; annotated.Next != want {
		t.Fatalf("next %q, want %q", annotated.Next, want)
	}
	patient := identifyProtocolWith(context.Background(), address, 0, identifyOptions{limit: identifySuggestedLimit, patient: true})
	if patient.Name != "redis" {
		t.Fatalf("--identify=10s: %+v", patient)
	}
}

func TestIdentifyFlagValues(t *testing.T) {
	cases := map[string]identifyFlag{"true": {on: true}, "false": {}, "10s": {on: true, limit: 10 * time.Second}, "7": {on: true, limit: 7 * time.Second}}
	for text, want := range cases {
		var got identifyFlag
		if err := got.Set(text); err != nil || got != want {
			t.Fatalf("%s: %+v %v", text, got, err)
		}
	}
	for _, text := range []string{"abc", "0", "-3s"} {
		var got identifyFlag
		if err := got.Set(text); err == nil {
			t.Fatalf("%s accepted", text)
		}
	}
}

func TestReadGoloopSecureResponseRejects(t *testing.T) {
	valid := goloopTestPacket(goloopSecureResponse, identifyTestPeerID, []byte{0x95, 0xa0, 0x01, 0x00, 0xc4, 0x00, 0xa0})
	if reply, err := readGoloopSecureResponse(strings.NewReader(string(valid))); err != nil || string(reply.peerID) != string(identifyTestPeerID) || reply.suite != goloopSuiteNone {
		t.Fatalf("valid packet: %+v %v", reply, err)
	}
	if _, err := readGoloopSecureResponse(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Fatalf("closed without bytes: %v, want io.EOF so that the probe tries again", err)
	}
	badHash := append([]byte(nil), valid...)
	badHash[len(badHash)-3] ^= 0xff
	huge := goloopTestPacket(goloopSecureResponse, identifyTestPeerID, nil)
	binary.BigEndian.PutUint32(huge[26:30], 0xffffffff)
	cases := map[string][]byte{
		"bad hash":         badHash,
		"huge length":      huge,
		"secure request":   goloopTestPacket(goloopSecureRequest, identifyTestPeerID, []byte{0x90}),
		"http response":    []byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"),
		"short header":     valid[:12],
		"truncated packet": valid[:len(valid)-4],
		"too few fields":   goloopTestPacket(goloopSecureResponse, identifyTestPeerID, []byte{0x93, 0xa0, 0x01, 0x00}),
		"wrong field type": goloopTestPacket(goloopSecureResponse, identifyTestPeerID, []byte{0x95, 0xa0, 0xa1, 0x78, 0x00, 0xc4, 0x00, 0xa0}),
		"string too long":  goloopTestPacket(goloopSecureResponse, identifyTestPeerID, []byte{0x95, 0xbf, 0x01}),
	}
	for name, data := range cases {
		if _, err := readGoloopSecureResponse(strings.NewReader(string(data))); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("%s: %v, want a rejection that is not io.EOF", name, err)
		}
	}
}

// goloop 노드는 연결 수 제한을 넘은 연결을 응답 없이 닫는다. 다음 초에 다시 보내면 답한다.
func TestIdentifyGoloopRetriesAfterRateLimitClose(t *testing.T) {
	t.Parallel()
	var accepted atomic.Int32
	address := identifyListener(t, func(c net.Conn) {
		if accepted.Add(1) == 1 {
			return
		}
		fakeGoloop(c)
	})
	// 첫 교환의 재시도는 port가 goloop를 가리킬 때만 한다.
	target := identifyTestTarget(address)
	target.hinted = true
	got, _, ok := identifyGoloop(context.Background(), target)
	// 끊긴 연결 1개와 방식을 묻는 연결 3개다.
	if !ok || got.Name != protocolNameGoloopP2P || !got.Retried || accepted.Load() != 4 {
		t.Fatalf("got %+v ok %v after %d connections", got, ok, accepted.Load())
	}
	if metrics := annotateProtocol(Result{}, got, false).Metrics; metrics["protocol_retried"] != true {
		t.Fatalf("retry not visible: %v", metrics)
	}
}

func TestGoloopSecureRequestPacketFraming(t *testing.T) {
	packet, err := goloopSecureRequestPacket(goloopSuiteTLS, goloopAeadAES128)
	if err != nil {
		t.Fatal(err)
	}
	length := binary.BigEndian.Uint32(packet[26:30])
	if binary.BigEndian.Uint16(packet[2:4]) != goloopSecureRequest || len(packet) != goloopHeaderSize+int(length)+goloopFooterSize {
		t.Fatalf("framing: %x", packet)
	}
	if binary.BigEndian.Uint64(packet[goloopHeaderSize+length:]) != goloopPacketHash(packet[:goloopHeaderSize], packet[goloopHeaderSize:goloopHeaderSize+length]) {
		t.Fatal("hash does not match")
	}
	// msgpack 배열 4개: 빈 channel, suite [tls], aead [aes128], 65바이트 P-256 공개 키(0x04로 시작하는 비압축 형식).
	payload := packet[goloopHeaderSize : goloopHeaderSize+length]
	if string(payload[:10]) != "\x94\xa0\xc4\x01\x02\xc4\x01\x02\xc4\x41" || payload[10] != 0x04 || len(payload) != 10+65 {
		t.Fatalf("payload: %x", payload)
	}
}

func TestIdentifyGoloopSecureSuites(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		suites, aeads map[byte]bool
		want          string
		wantSuites    []string
		wantAeads     []string
	}{
		{"all", goloopAllSupported, goloopAllSupported, "secure suites  none, tls, ecdhe\naead suites    chacha, aes128, aes256", []string{"none", "tls", "ecdhe"}, []string{"chacha", "aes128", "aes256"}},
		{"ecdhe only, none refused", map[byte]bool{goloopSuiteECDHE: true}, map[byte]bool{goloopAeadAES256: true, goloopAeadChaCha: true}, "secure suites  ecdhe\naead suites    chacha, aes256", []string{"ecdhe"}, []string{"chacha", "aes256"}},
		{"none only", map[byte]bool{goloopSuiteNone: true}, map[byte]bool{goloopAeadAES128: true}, "secure suites  none\naead suites    aes128", []string{"none"}, []string{"aes128"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			address := identifyListener(t, fakeGoloopWith(testCase.suites, testCase.aeads))
			got, _, ok := identifyGoloop(context.Background(), identifyTestTarget(address))
			want := identifyTestNodeLine + "\n" + testCase.want
			if !ok || strings.Join(got.Facts, "\n") != want {
				t.Fatalf("got %+v ok %v, want facts %q", got, ok, want)
			}
			annotated := annotateProtocol(Result{Probe: "tcp.check", Status: StatusPass}, got, true)
			if len(annotated.Evidence) != 1 || annotated.Evidence[0].Value != want+"\nbasis          goloop SecureResponse" {
				t.Fatalf("evidence %+v", annotated.Evidence)
			}
			if !slices.Equal(got.Metrics["goloop_secure_suites"].([]string), testCase.wantSuites) ||
				!slices.Equal(got.Metrics["goloop_aead_suites"].([]string), testCase.wantAeads) || got.Metrics["goloop_unchecked"] != nil {
				t.Fatalf("metrics %v", got.Metrics)
			}
		})
	}
}

// 보안 방식을 하나도 지원하지 않는다고 답해도 goloop다. 암호 방식은 확인할 수 없다.
func TestIdentifyGoloopNoSuiteSupported(t *testing.T) {
	t.Parallel()
	address := identifyListener(t, fakeGoloopWith(map[byte]bool{}, goloopAllSupported))
	got, _, ok := identifyGoloop(context.Background(), identifyTestTarget(address))
	if !ok || strings.Join(got.Facts, "\n") != identifyTestNodeLine+"\nsecure suites  -\naead suites    -\nunchecked      chacha, aes128, aes256" {
		t.Fatalf("got %+v ok %v", got, ok)
	}
}

const identifyTestNodeLine = "node           hx0123456789abcdef0123456789abcdef01234567"

// -v면 결과 줄에는 연결과 프로토콜 이름만 남고, peer와 판별 근거는 상세 패널로 간다. 상태는 바뀌지 않는다.
func TestAnnotateProtocolMovesDetailBelow(t *testing.T) {
	base := func() Result {
		return Result{Probe: "tcp.check", Status: StatusPass, Summary: "old summary, peer 192.0.2.7", Metrics: map[string]interface{}{
			"address": "db.example:5432", "connect_ms": int64(9), "remote_address": "192.0.2.7:5432",
		}}
	}
	connected := T("observe.probe.tcp_connected", "db.example:5432", 9*time.Millisecond)

	// 기본 결과 줄은 프로토콜 열 뒤에 원래 요약(peer 포함)을 둔다. 상세는 -v에서 보인다.
	if got := annotateProtocol(base(), protocolIdentification{Name: "ssh", Detail: "SSH-2.0-x", Basis: protocolBasisBanner}, false); got.Summary != "old summary, peer 192.0.2.7" || len(got.Evidence) != 1 {
		t.Fatalf("default: %+v", got)
	}
	if evidenceShownProbes["tcp.check"] {
		t.Fatal("tcp.check evidence must stay behind -v")
	}

	got := annotateProtocol(base(), protocolIdentification{Name: protocolUnknown, FirstBytes: []byte("0123456789abcdefXYZ")}, true)
	if got.Status != StatusPass || got.Metrics["protocol"] != protocolUnknown || got.Metrics["protocol_first_bytes"] != "3031323334353637383961626364656658595a" {
		t.Fatalf("unknown: %+v", got)
	}
	if got.Summary != connected {
		t.Fatalf("unknown summary: %q", got.Summary)
	}
	if want := "peer           192.0.2.7\nfirst bytes    30 31 32 33 34 35 36 37 38 39 61 62 63 64 65 66"; len(got.Evidence) != 1 || got.Evidence[0].Value != want {
		t.Fatalf("unknown evidence: %+v", got.Evidence)
	}

	got = annotateProtocol(base(), protocolIdentification{Name: "ssh", Detail: "SSH-2.0-x", Basis: protocolBasisBanner}, true)
	if got.Status != StatusPass || got.Metrics["protocol_basis"] != protocolBasisBanner || got.Summary != connected {
		t.Fatalf("ssh: %+v", got)
	}
	if want := "peer           192.0.2.7\ndetail         SSH-2.0-x\nbasis          banner"; got.Evidence[0].Value != want {
		t.Fatalf("ssh evidence: %q", got.Evidence[0].Value)
	}
}

// 결과 줄은 metrics의 protocol을 tcp.check 뒤에 그리고, 설명 열은 다른 probe와 같은 자리에서 시작한다. 저장한 report를
// 다시 그릴 때도 metrics만으로 같은 줄이 나온다.
func TestFormatResultLineShowsProtocolTag(t *testing.T) {
	result := Result{Probe: "tcp.check", Status: StatusPass, Summary: "db:5432 connected (9ms), peer 192.0.2.7", Metrics: map[string]interface{}{"protocol": "postgresql"}}
	plain := formatResultLine(result, false)
	other := formatResultLine(Result{Probe: "dns.lookup", Status: StatusPass, Summary: "x"}, false)
	if !strings.Contains(plain, "tcp.check postgresql") || strings.Index(plain, "db:5432") != strings.Index(other, "x") {
		t.Fatalf("plain line %q does not align with %q", plain, other)
	}
	colored := formatResultLine(result, true)
	if !strings.Contains(colored, richPaint("postgresql", richCyan, false)) {
		t.Fatalf("protocol is not colored: %q", colored)
	}
	result.Metrics["protocol"] = protocolUnknown
	if colored := formatResultLine(result, true); !strings.Contains(colored, richPaint(protocolUnknown, richGray, false)) {
		t.Fatalf("unknown is not gray: %q", colored)
	}
	if line := formatResultLine(Result{Probe: "tcp.check", Status: StatusPass, Summary: "x"}, false); strings.Contains(line, "tcp.check  ") == false {
		t.Fatalf("without a protocol the label stays alone: %q", line)
	}
}

// 프로토콜은 label 열(24자)의 남는 자리에 그린다. tcp.check 뒤에 넘치면 설명 열이 밀리므로 이름이 길어지면 폭을 다시 봐야 한다.
func TestProtocolNamesFitColumn(t *testing.T) {
	names := []string{"ssh", "vnc", "mysql", "tls", "https", "grpc", "rediss", "nats", "clickhouse", protocolUnknown}
	for _, stage := range identifyStages {
		names = append(names, stage.name)
	}
	for banner, secure := range tlsBannerNames {
		names = append(names, banner, secure)
	}
	for _, name := range names {
		if len("tcp.check")+1+len(name) > resultLabelWidth {
			t.Fatalf("tcp.check %s is wider than the label column (%d)", name, resultLabelWidth)
		}
	}
}

func TestRunTCPCheckIdentify(t *testing.T) {
	address := identifyListener(t, fakeGoloop)
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
	// 인자 없이도 판별하고, --identify=false면 판별하지 않는다. JSON 보고서는 한 줄에 하나씩 이어진다.
	plain := runTCP([]string{"check", "--json", "-", address}, "test")
	disabled := runTCP([]string{"check", "--json", "-", "--identify=false", address}, "test")
	conflict := runTCP([]string{"check", "--identify", "-w", address}, "test")
	conflictOff := runTCP([]string{"check", "--identify=false", "-w", address}, "test")
	os.Stdout, os.Stderr = previousOut, previousErr
	write.Close()
	data := <-output
	if conflict != 2 || conflictOff != 2 {
		t.Fatalf("--identify with -w: codes %d %d, want 2", conflict, conflictOff)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	var reports []map[string]interface{}
	for decoder.More() {
		var report struct {
			Results []struct {
				Metrics map[string]interface{} `json:"metrics"`
			} `json:"results"`
		}
		if err := decoder.Decode(&report); err != nil || len(report.Results) != 1 {
			t.Fatalf("%v %s", err, data)
		}
		reports = append(reports, report.Results[0].Metrics)
	}
	if plain != 0 || disabled != 0 || len(reports) != 2 {
		t.Fatalf("codes %d %d, %d reports: %s", plain, disabled, len(reports), data)
	}
	if reports[0]["protocol"] != protocolNameGoloopP2P || reports[0]["protocol_basis"] != protocolBasisGoloop {
		t.Fatalf("default check: %v", reports[0])
	}
	if _, ok := reports[1]["protocol"]; ok {
		t.Fatalf("--identify=false still identified: %v", reports[1])
	}
}
