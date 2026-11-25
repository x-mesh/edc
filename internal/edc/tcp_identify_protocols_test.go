package edc

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readExactly는 가짜 서버가 탐침 전체를 받았는지 본다. 다른 탐침을 받으면 false다.
func readExactly(connection net.Conn, want []byte) bool {
	connection.SetDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(want))
	if _, err := io.ReadFull(connection, got); err != nil {
		return false
	}
	return bytes.Equal(got, want)
}

func amqpStartFrame(product, version string) []byte {
	table := []byte{}
	for _, entry := range [][2]string{{"capabilities", ""}, {"product", product}, {"version", version}} {
		table = append(table, byte(len(entry[0])))
		table = append(table, entry[0]...)
		if entry[0] == "capabilities" {
			// 중첩 table은 건너뛰어야 한다.
			table = append(table, 'F')
			inner := append([]byte{byte(len("basic.nack"))}, "basic.nack"...)
			inner = append(inner, 't', 1)
			table = binary.BigEndian.AppendUint32(table, uint32(len(inner)))
			table = append(table, inner...)
			continue
		}
		table = append(table, 'S')
		table = binary.BigEndian.AppendUint32(table, uint32(len(entry[1])))
		table = append(table, entry[1]...)
	}
	payload := []byte{0, 10, 0, 10, 0, 9}
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(table)))
	payload = append(payload, table...)
	payload = binary.BigEndian.AppendUint32(payload, 5)
	payload = append(payload, "PLAIN"...)
	frame := []byte{amqpFrameMethod, 0, 0}
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	return append(frame, amqpFrameEnd)
}

func fakeKafka(connection net.Conn) {
	if !readExactly(connection, kafkaAPIVersionsRequest()) {
		return
	}
	body := binary.BigEndian.AppendUint32(nil, kafkaCorrelationID)
	body = binary.BigEndian.AppendUint16(body, 0) // error_code
	body = binary.BigEndian.AppendUint32(body, 2) // api count
	body = append(body, 0, 18, 0, 0, 0, 3, 0, 0, 0, 0, 0, 9)
	connection.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...))
	io.Copy(io.Discard, connection)
}

func fakeMQTT(returnCode byte, disconnected *atomic.Bool) func(net.Conn) {
	return func(connection net.Conn) {
		connection.SetDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, 2)
		if _, err := io.ReadFull(connection, header); err != nil || header[0] != 0x10 {
			return
		}
		if _, err := io.ReadFull(connection, make([]byte, header[1])); err != nil {
			return
		}
		connection.Write([]byte{0x20, 0x02, 0x00, returnCode})
		rest := make([]byte, 2)
		if _, err := io.ReadFull(connection, rest); err == nil && rest[0] == 0xe0 {
			disconnected.Store(true)
		}
	}
}

func fakeCassandra(opcode byte, body []byte) func(net.Conn) {
	return func(connection net.Conn) {
		if !readExactly(connection, cassandraOptions) {
			return
		}
		header := []byte{0x84, 0x00, 0x00, 0x00, opcode}
		header = binary.BigEndian.AppendUint32(header, uint32(len(body)))
		connection.Write(append(header, body...))
		io.Copy(io.Discard, connection)
	}
}

func cqlSupportedBody() []byte {
	writeString := func(data []byte, text string) []byte {
		return append(binary.BigEndian.AppendUint16(data, uint16(len(text))), text...)
	}
	body := binary.BigEndian.AppendUint16(nil, 2)
	body = writeString(body, "COMPRESSION")
	body = binary.BigEndian.AppendUint16(body, 1)
	body = writeString(body, "lz4")
	body = writeString(body, "CQL_VERSION")
	body = binary.BigEndian.AppendUint16(body, 1)
	return writeString(body, "3.4.7")
}

func TestIdentifyMoreProtocols(t *testing.T) {
	t.Parallel()
	h2c := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Set("Grpc-Status", "12")
	})}
	h2c.Protocols = new(http.Protocols)
	h2c.Protocols.SetUnencryptedHTTP2(true)
	h2cListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go h2c.Serve(h2cListener)
	t.Cleanup(func() { h2c.Close() })
	clickhouseHTTP := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-ClickHouse-Summary", `{"read_rows":"0"}`)
		writer.Header().Set("X-ClickHouse-Exception-Tag", "abc")
		io.WriteString(writer, "Ok.\n")
	}))
	t.Cleanup(clickhouseHTTP.Close)
	cqlError := binary.BigEndian.AppendUint32(nil, 0x000a)
	cqlError = binary.BigEndian.AppendUint16(cqlError, uint16(len("Invalid or unsupported protocol version (4)")))
	cqlError = append(cqlError, "Invalid or unsupported protocol version (4)"...)
	cases := []struct {
		name, address, protocol, detail string
	}{
		{"amqp", identifyListener(t, func(c net.Conn) {
			if readExactly(c, amqpProtocolHeader) {
				c.Write(amqpStartFrame("RabbitMQ", "4.1.0"))
				io.Copy(io.Discard, c)
			}
		}), "amqp", "RabbitMQ 4.1.0"},
		// AMQP 1.0 broker는 0-9-1 header를 받으면 자기 header를 보내고 끊는다.
		{"amqp 1.0", identifyListener(t, func(c net.Conn) {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			c.Read(make([]byte, 64))
			c.Write([]byte("AMQP\x03\x01\x00\x00"))
		}), "amqp", "offers AMQP 1.0"},
		{"kafka", identifyListener(t, fakeKafka), "kafka", "ApiVersions, 2 APIs"},
		{"mqtt refused", identifyListener(t, fakeMQTT(5, new(atomic.Bool))), "mqtt", "CONNACK not authorized"},
		{"mqtt accepted", identifyListener(t, fakeMQTT(0, new(atomic.Bool))), "mqtt", "CONNACK accepted"},
		{"cassandra", identifyListener(t, fakeCassandra(cassandraOpSupported, cqlSupportedBody())), "cassandra", "CQL 3.4.7"},
		{"cassandra without v4", identifyListener(t, fakeCassandra(cassandraOpError, cqlError)), "cassandra", "error: Invalid or unsupported protocol version (4)"},
		{"nats", identifyListener(t, func(c net.Conn) {
			io.WriteString(c, `INFO {"server_id":"N1","server_name":"n1","version":"2.10.22","proto":1,"max_payload":1048576}`+"\r\n")
			io.Copy(io.Discard, c)
		}), "nats", "version 2.10.22"},
		// ClickHouse 26.3의 native port가 HTTP 요청에 보낸 응답 그대로다.
		{"clickhouse native", identifyListener(t, replyServer(func(request []byte) []byte {
			if !bytes.HasPrefix(request, []byte("GET / HTTP/1.0")) {
				return nil
			}
			return []byte("HTTP/1.0 400 Bad Request\r\n\r\nPort 9000 is for clickhouse-client program\nYou must use port 8123 for HTTP.\n")
		})), "clickhouse", "Port 9000 is for clickhouse-client program"},
		// 본문이 header와 다른 조각으로 와도 ClickHouse native port로 본다.
		{"clickhouse native in two writes", identifyListener(t, func(c net.Conn) {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			buffer := make([]byte, 1024)
			if _, err := c.Read(buffer); err != nil || !bytes.HasPrefix(buffer, []byte("GET / HTTP/1.0")) {
				return
			}
			io.WriteString(c, "HTTP/1.0 400 Bad Request\r\n\r\n")
			time.Sleep(50 * time.Millisecond)
			io.WriteString(c, "Port 9000 is for clickhouse-client program\nYou must use port 8123 for HTTP.\n")
			io.Copy(io.Discard, c)
		}), "clickhouse", "Port 9000 is for clickhouse-client program"},
		// 본문 없는 400을 보내고 연결을 열어 둔 서버는 HTTP다.
		{"http 400 with no body", identifyListener(t, replyServer(func(request []byte) []byte {
			if !bytes.HasPrefix(request, []byte("GET / HTTP/1.0")) {
				return nil
			}
			return []byte("HTTP/1.0 400 Bad Request\r\n\r\n")
		})), "http", "HTTP/1.0 400 Bad Request"},
		{"clickhouse http", strings.TrimPrefix(clickhouseHTTP.URL, "http://"), "http", "HTTP/1.0 200 OK, ClickHouse"},
		{"grpc over h2c", h2cListener.Addr().String(), "grpc", "HTTP/2 200, grpc-status 12"},
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

// broker가 CONNECT를 받으면 실제 client session이 열리므로, 판별한 뒤 DISCONNECT로 닫는다.
func TestIdentifyMQTTClosesAcceptedSession(t *testing.T) {
	t.Parallel()
	var disconnected atomic.Bool
	address := identifyListener(t, fakeMQTT(0, &disconnected))
	if got, _, ok := identifyMQTT(context.Background(), identifyTestTarget(address)); !ok || got.Detail != "CONNACK accepted" {
		t.Fatalf("got %+v ok %v", got, ok)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !disconnected.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !disconnected.Load() {
		t.Fatal("accepted MQTT session was not sent DISCONNECT")
	}
}

// MQTT client id는 실행마다 다르다. 같은 id면 broker가 이미 붙은 client를 끊는다.
func TestMQTTClientIDIsRandom(t *testing.T) {
	first, second := mqttConnectPacket(), mqttConnectPacket()
	if bytes.Equal(first, second) || !bytes.Contains(first, []byte("edc-")) {
		t.Fatalf("client ids: %q %q", first, second)
	}
}

// 새 탐침은 goloop 뒤에 둔다. 앞에 두면 30바이트보다 짧은 탐침마다 goloop가 header를 기다리며 멈춘다.
func TestNewProbesComeAfterGoloop(t *testing.T) {
	goloop := -1
	for index, stage := range identifyStages {
		if stage.name == protocolNameGoloopP2P {
			goloop = index
		}
		switch stage.name {
		case "h2c", "amqp", "kafka", "mqtt", "cassandra":
			if goloop < 0 {
				t.Fatalf("%s comes before goloop", stage.name)
			}
		}
	}
	kafka, cassandra := -1, -1
	for index, stage := range identifyStages {
		switch stage.name {
		case "kafka":
			kafka = index
		case "cassandra":
			cassandra = index
		}
	}
	// Cassandra OPTIONS의 앞 4바이트는 약 64MB의 길이로 읽혀 Kafka를 멈추게 한다.
	if cassandra < kafka {
		t.Fatal("cassandra comes before kafka")
	}
}

// h2cServer는 TLS 없이 HTTP/2를 받는 local server다.
func h2cServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := &http.Server{Handler: handler, Protocols: new(http.Protocols)}
	server.Protocols.SetUnencryptedHTTP2(true)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return listener.Addr().String()
}

// gRPC가 아닌 HTTP/2 서버는 h2c로 나온다.
func TestIdentifyH2CWithoutGRPC(t *testing.T) {
	t.Parallel()
	address := h2cServer(t, func(writer http.ResponseWriter, request *http.Request) { http.NotFound(writer, request) })
	if got, _, ok := identifyH2C(context.Background(), identifyTestTarget(address)); !ok || got.Name != "h2c" || got.Detail != "HTTP/2 404" {
		t.Fatalf("got %+v ok %v", got, ok)
	}
}

// grpc-go처럼 연결하자마자 SETTINGS frame을 보내는 서버다. 배너 단계의 첫 연결에는 SETTINGS만 보내고 닫고, 그 뒤의 연결은 그대로
// HTTP/2 서버에 이어 준다. 배너 단계가 SETTINGS를 보면 h2c 탐침으로 넘어가 gRPC를 찾아야 한다.
func TestIdentifyServerFirstSettingsGoesToH2C(t *testing.T) {
	t.Parallel()
	backend := h2cServer(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Set("Grpc-Status", "0")
	})
	settings := []byte{0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x40, 0x00}
	var accepted atomic.Int32
	address := identifyListener(t, func(client net.Conn) {
		if accepted.Add(1) == 1 {
			client.Write(settings)
			return
		}
		upstream, err := net.Dial("tcp", backend)
		if err != nil {
			return
		}
		defer upstream.Close()
		go io.Copy(upstream, client)
		io.Copy(client, upstream)
	})
	if got := identifyProtocol(context.Background(), address, 0); got.Name != "grpc" {
		t.Fatalf("got %+v", got)
	}
}

// AMQP over TLS(amqps)는 TLS 안쪽의 Redis PING에 AMQP protocol header로 답한다.
func TestIdentifyAMQPInsideTLS(t *testing.T) {
	t.Parallel()
	certificateServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(certificateServer.Close)
	address := tlsListener(t, certificateServer.TLS.Certificates, func(c net.Conn) {
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if count, _ := c.Read(make([]byte, 64)); count > 0 {
			c.Write(amqpProtocolHeader)
		}
	})
	if got := identifyProtocol(context.Background(), address, 0); got.Name != "amqps" || got.Detail != "offers AMQP 0-9-1" {
		t.Fatalf("got %+v", got)
	}
}

// grpc-go는 연결하자마자 SETTINGS frame을 보낸다. 배너 단계는 이 frame을 모르는 인사말로 보지 않고 h2c 탐침으로 넘긴다.
func TestHTTP2SettingsFrameCheck(t *testing.T) {
	settings := []byte{0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x40, 0x00}
	empty := []byte{0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00}
	if !isHTTP2SettingsFrame(settings) || !isHTTP2SettingsFrame(empty) {
		t.Fatal("SETTINGS frame not recognized")
	}
	for _, other := range [][]byte{[]byte("SSH-2.0-OpenSSH_9.6\r\n"), {0x00, 0x00, 0x05, 0x04, 0, 0, 0, 0, 0}, {0x00, 0x00, 0x06, 0x04, 0, 0, 0, 0, 1}} {
		if isHTTP2SettingsFrame(other) {
			t.Fatalf("%x taken as SETTINGS", other)
		}
	}
}

func TestParserBoundaries(t *testing.T) {
	if values := amqpTableStrings([]byte{0, 0, 0, 200, 1}); len(values) != 0 {
		t.Fatalf("oversized table: %v", values)
	}
	if values := cqlStringMultimap([]byte{0, 5, 0, 200}); len(values) != 0 {
		t.Fatalf("truncated multimap: %v", values)
	}
	if _, ok := amqpHeaderDetail([]byte("AMQP\x00")); ok {
		t.Fatal("short header accepted")
	}
	if version := natsBannerVersion(`INFO {"server_id":"x"`); version != "" {
		t.Fatalf("version from a cut line: %q", version)
	}
}
