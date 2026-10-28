//go:build darwin

package edc

import (
	"encoding/binary"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

type darwinSocketFixture struct {
	kind, protocol, tcpState uint32
	options                  uint16
	qlen, qlimit             uint16
	recvCC, sendCC           uint32
	ipv4                     bool
	local                    net.IP
	localPort, foreignPort   uint16
	path                     string
}

// bytes는 socket_fdinfo를 SDK 헤더의 offset대로 채운다. 파서와 같은 상수를 쓰므로 offset 자체가 아니라
// 고르는 규칙과 주소·포트 해석을 검증한다. offset은 TestCollectDarwinListenSocketsFindsOwnSockets가 실제 kernel 값으로 검증한다.
func (fixture darwinSocketFixture) bytes() []byte {
	info := make([]byte, darwinSocketFDInfoSize)
	binary.LittleEndian.PutUint32(info[darwinSoiKind:], fixture.kind)
	binary.LittleEndian.PutUint32(info[darwinSoiProtocol:], fixture.protocol)
	binary.LittleEndian.PutUint16(info[darwinSoiOptions:], fixture.options)
	binary.LittleEndian.PutUint16(info[darwinSoiQLen:], fixture.qlen)
	binary.LittleEndian.PutUint16(info[darwinSoiQLimit:], fixture.qlimit)
	binary.LittleEndian.PutUint32(info[darwinSoiRecvCC:], fixture.recvCC)
	binary.LittleEndian.PutUint32(info[darwinSoiSendCC:], fixture.sendCC)
	proto := info[darwinSoiProto:]
	if fixture.kind == darwinSocketKindUnix {
		copy(proto[darwinUnixPath:], fixture.path)
		return info
	}
	binary.LittleEndian.PutUint32(proto[darwinTCPState:], fixture.tcpState)
	binary.BigEndian.PutUint16(proto[darwinInLPort:], fixture.localPort)
	binary.BigEndian.PutUint16(proto[darwinInFPort:], fixture.foreignPort)
	if fixture.ipv4 {
		proto[darwinInVFlag] = darwinInIPv4
		copy(proto[darwinInLAddr4:], fixture.local.To4())
	} else {
		proto[darwinInVFlag] = 2
		copy(proto[darwinInLAddr:], fixture.local.To16())
	}
	return info
}

func TestParseDarwinSocketInfo(t *testing.T) {
	all := listenFamilies{TCP: true, UDP: true, Unix: true}
	tests := []struct {
		name     string
		fixture  darwinSocketFixture
		families listenFamilies
		want     *listenSocket
	}{
		{
			name:     "tcp listen loopback",
			fixture:  darwinSocketFixture{kind: darwinSocketKindTCP, tcpState: darwinTCPStateListen, ipv4: true, local: net.ParseIP("127.0.0.1"), localPort: 8080, qlen: 3, qlimit: 128},
			families: all,
			want:     &listenSocket{Proto: "tcp", Address: "127.0.0.1", Port: 8080, RecvQ: 3, SendQ: 128, HasQueue: true},
		},
		{
			name:     "tcp listen any ipv6",
			fixture:  darwinSocketFixture{kind: darwinSocketKindTCP, tcpState: darwinTCPStateListen, local: net.IPv6unspecified, localPort: 5000, qlimit: 128},
			families: all,
			want:     &listenSocket{Proto: "tcp", Address: "*", Port: 5000, SendQ: 128, HasQueue: true},
		},
		{
			name:     "tcp listen v4-mapped",
			fixture:  darwinSocketFixture{kind: darwinSocketKindTCP, tcpState: darwinTCPStateListen, local: net.ParseIP("::ffff:127.0.0.1"), localPort: 80},
			families: all,
			want:     &listenSocket{Proto: "tcp", Address: "::ffff:127.0.0.1", Port: 80, HasQueue: true},
		},
		{
			name:     "tcp established",
			fixture:  darwinSocketFixture{kind: darwinSocketKindTCP, tcpState: 4, ipv4: true, local: net.ParseIP("192.0.2.1"), localPort: 50000, foreignPort: 443},
			families: all,
		},
		{
			name:     "tcp filtered out",
			fixture:  darwinSocketFixture{kind: darwinSocketKindTCP, tcpState: darwinTCPStateListen, ipv4: true, local: net.IPv4zero, localPort: 22},
			families: listenFamilies{UDP: true},
		},
		{
			name:     "udp bound any",
			fixture:  darwinSocketFixture{kind: darwinSocketKindIn, protocol: darwinIPProtoUDP, ipv4: true, local: net.IPv4zero, localPort: 53, recvCC: 512},
			families: all,
			want:     &listenSocket{Proto: "udp", Address: "*", Port: 53, RecvQ: 512, HasQueue: true},
		},
		{
			name:     "udp connected flow",
			fixture:  darwinSocketFixture{kind: darwinSocketKindIn, protocol: darwinIPProtoUDP, ipv4: true, local: net.ParseIP("192.0.2.13"), localPort: 62979, foreignPort: 443},
			families: all,
		},
		{
			name:     "udp without port",
			fixture:  darwinSocketFixture{kind: darwinSocketKindIn, protocol: darwinIPProtoUDP, ipv4: true, local: net.IPv4zero},
			families: all,
		},
		{
			name:     "raw ip socket",
			fixture:  darwinSocketFixture{kind: darwinSocketKindIn, protocol: 1, ipv4: true, local: net.IPv4zero, localPort: 0},
			families: all,
		},
		{
			name:     "unix listener",
			fixture:  darwinSocketFixture{kind: darwinSocketKindUnix, options: darwinSocketOptionAcceptCon, path: "/tmp/app.sock", qlimit: 4},
			families: all,
			want:     &listenSocket{Proto: "unix", Address: "/tmp/app.sock", SendQ: 4, HasQueue: true},
		},
		{
			name:     "unix connected with path",
			fixture:  darwinSocketFixture{kind: darwinSocketKindUnix, path: "/tmp/app.sock"},
			families: all,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseDarwinSocketInfo(tt.fixture.bytes(), tt.families)
			if tt.want == nil {
				if ok {
					t.Fatalf("골라서는 안 되는 소켓을 골랐다: %#v", got)
				}
				return
			}
			if !ok || !reflect.DeepEqual(got, *tt.want) {
				t.Fatalf("got %#v (%v), want %#v", got, ok, *tt.want)
			}
		})
	}
}

// 실제 kernel이 채운 socket_fdinfo를 읽는다. offset이 틀리면 포트, 상태, 경로 중 하나가 어긋난다.
func TestCollectDarwinListenSocketsFindsOwnSockets(t *testing.T) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	connected, err := net.Dial("udp4", "127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	defer connected.Close()
	// sun_path는 104바이트라 t.TempDir()의 긴 경로가 넘칠 수 있다.
	directory, err := os.MkdirTemp("/tmp", "edc-listen")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "s.sock")
	unix, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close()

	sockets, err := collectListenSockets(listenFamilies{TCP: true, UDP: true, Unix: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]listenSocket{}
	for _, socket := range sockets {
		if socket.PID != pid {
			continue
		}
		if socket.Process == "" || socket.FD == "" || socket.User != current.Username {
			t.Fatalf("process 정보가 비었다: %#v", socket)
		}
		found[socket.Proto+" "+socket.Address+" "+strconv.Itoa(socket.Port)] = socket
	}
	wantTCP := "tcp 127.0.0.1 " + strconv.Itoa(tcp.Addr().(*net.TCPAddr).Port)
	wantUDP := "udp ::1 " + strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port)
	wantUnix := "unix " + path + " 0"
	for _, key := range []string{wantTCP, wantUDP, wantUnix} {
		if _, ok := found[key]; !ok {
			t.Fatalf("%q를 찾지 못했다: %v", key, found)
		}
	}
	if !found[wantTCP].HasQueue || found[wantTCP].SendQ <= 0 {
		t.Fatalf("listen backlog를 읽지 못했다: %#v", found[wantTCP])
	}
	connectedPort := connected.LocalAddr().(*net.UDPAddr).Port
	for key, socket := range found {
		if socket.Proto == "udp" && socket.Port == connectedPort {
			t.Fatalf("연결된 UDP 소켓이 섞였다: %s", key)
		}
	}
}

func BenchmarkCollectDarwinListenSockets(b *testing.B) {
	families := listenFamilies{TCP: true, UDP: true, Unix: true}
	for b.Loop() {
		if _, err := collectListenSockets(families); err != nil {
			b.Fatal(err)
		}
	}
}

func TestParseDarwinProcArgsStopsBeforeTheEnvironment(t *testing.T) {
	raw := binary.LittleEndian.AppendUint32(nil, 3)
	raw = append(raw, "/opt/homebrew/bin/node\x00\x00\x00\x00node\x00vite\x00--port\x00SECRET=1\x00HOME=/x\x00"...)
	if got := parseDarwinProcArgs(raw); !slices.Equal(got, []string{"node", "vite", "--port"}) {
		t.Fatalf("args = %q", got)
	}
	for name, raw := range map[string][]byte{
		"short buffer":            {1, 0},
		"zero argc":               append(binary.LittleEndian.AppendUint32(nil, 0), "/bin/x\x00x\x00"...),
		"argc over the limit":     append(binary.LittleEndian.AppendUint32(nil, darwinProcArgsMaxArgs+1), "/bin/x\x00x\x00"...),
		"exec path without a NUL": append(binary.LittleEndian.AppendUint32(nil, 1), "/bin/x"...),
	} {
		if got := parseDarwinProcArgs(raw); got != nil {
			t.Errorf("%s gave %q", name, got)
		}
	}
}

// 테스트 process 자신의 argv와 작업 디렉터리를 읽는다. 구조체 offset이 틀리면 경로가 비거나 어긋난다.
func TestListenProcessCommandReadsOwnProcess(t *testing.T) {
	args, cwd := listenProcessCommand(strconv.Itoa(os.Getpid()))
	if !slices.Equal(args, os.Args) {
		t.Fatalf("args = %q, want %q", args, os.Args)
	}
	want, _ := os.Getwd()
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	if cwd != want {
		t.Fatalf("cwd = %q, want %q", cwd, want)
	}
}
