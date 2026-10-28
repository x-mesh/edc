//go:build linux

package edc

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

type linuxInetDiagFixture struct {
	family         uint8
	port           uint16
	address        net.IP
	rqueue, wqueue uint32
	uid, inode     uint32
	v6only         *bool
}

func (fixture linuxInetDiagFixture) bytes() []byte {
	message := make([]byte, linuxInetDiagMsgSize)
	message[0] = fixture.family
	binary.BigEndian.PutUint16(message[4:], fixture.port)
	if fixture.family == unix.AF_INET {
		copy(message[8:], fixture.address.To4())
	} else {
		copy(message[8:], fixture.address.To16())
	}
	binary.NativeEndian.PutUint32(message[56:], fixture.rqueue)
	binary.NativeEndian.PutUint32(message[60:], fixture.wqueue)
	binary.NativeEndian.PutUint32(message[64:], fixture.uid)
	binary.NativeEndian.PutUint32(message[68:], fixture.inode)
	if fixture.v6only != nil {
		var value byte
		if *fixture.v6only {
			value = 1
		}
		message = append(message, linuxDiagAttribute(linuxInetDiagSkV6Only, []byte{value})...)
	}
	return message
}

func linuxDiagAttribute(kind uint16, payload []byte) []byte {
	length := unix.SizeofRtAttr + len(payload)
	attribute := make([]byte, (length+unix.RTA_ALIGNTO-1)&^(unix.RTA_ALIGNTO-1))
	binary.NativeEndian.PutUint16(attribute[0:], uint16(length))
	binary.NativeEndian.PutUint16(attribute[2:], kind)
	copy(attribute[unix.SizeofRtAttr:], payload)
	return attribute
}

func TestParseLinuxInetDiagMsg(t *testing.T) {
	yes, no := true, false
	root := lookupUserName("0")
	tests := []struct {
		name    string
		fixture linuxInetDiagFixture
		want    *listenSocket
	}{
		{
			name:    "ipv4 listener",
			fixture: linuxInetDiagFixture{family: unix.AF_INET, port: 22, address: net.IPv4zero, wqueue: 4096, inode: 31017},
			want:    &listenSocket{Proto: "tcp", Address: "0.0.0.0", Port: 22, SendQ: 4096, HasQueue: true, User: root, inode: 31017},
		},
		{
			name:    "ipv6 wildcard with v6only",
			fixture: linuxInetDiagFixture{family: unix.AF_INET6, port: 22, address: net.IPv6unspecified, v6only: &yes},
			want:    &listenSocket{Proto: "tcp", Address: "::", Port: 22, HasQueue: true, User: root},
		},
		{
			name:    "ipv6 wildcard that also takes ipv4",
			fixture: linuxInetDiagFixture{family: unix.AF_INET6, port: 8080, address: net.IPv6unspecified, v6only: &no},
			want:    &listenSocket{Proto: "tcp", Address: "*", Port: 8080, HasQueue: true, User: root},
		},
		{
			name:    "ipv6 address",
			fixture: linuxInetDiagFixture{family: unix.AF_INET6, port: 443, address: net.ParseIP("fd7a:115c:a1e0::1"), rqueue: 2, v6only: &no},
			want:    &listenSocket{Proto: "tcp", Address: "fd7a:115c:a1e0::1", Port: 443, RecvQ: 2, HasQueue: true, User: root},
		},
		{
			name:    "unbound",
			fixture: linuxInetDiagFixture{family: unix.AF_INET, address: net.IPv4zero},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLinuxInetDiagMsg("tcp", tt.fixture.bytes())
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

func TestParseLinuxUnixDiagMsg(t *testing.T) {
	message := func(state uint8, attributes ...[]byte) []byte {
		data := make([]byte, linuxUnixDiagMsgSize)
		data[0], data[1], data[2] = unix.AF_UNIX, unix.SOCK_STREAM, state
		binary.NativeEndian.PutUint32(data[4:], 14023)
		for _, attribute := range attributes {
			data = append(data, attribute...)
		}
		return data
	}
	queue := make([]byte, 8)
	binary.NativeEndian.PutUint32(queue[4:], 4096)
	uid := make([]byte, 4)
	tests := []struct {
		name    string
		message []byte
		want    *listenSocket
	}{
		{
			// kernel은 bind할 때 받은 길이 그대로 경로를 돌려주므로 끝에 NUL이 붙을 수 있다.
			name:    "path with queue and owner",
			message: message(linuxTCPListen, linuxDiagAttribute(linuxUnixDiagName, []byte("/run/systemd/private\x00")), linuxDiagAttribute(linuxUnixDiagRQLen, queue), linuxDiagAttribute(linuxUnixDiagUID, uid)),
			want:    &listenSocket{Proto: "unix", Address: "/run/systemd/private", SendQ: 4096, HasQueue: true, User: lookupUserName("0"), inode: 14023},
		},
		{
			name:    "abstract name",
			message: message(linuxTCPListen, linuxDiagAttribute(linuxUnixDiagName, []byte("\x00/tmp/.X11-unix/X0"))),
			want:    &listenSocket{Proto: "unix", Address: "@/tmp/.X11-unix/X0", inode: 14023},
		},
		{
			name:    "unnamed",
			message: message(linuxTCPListen),
			want:    &listenSocket{Proto: "unix", Address: "*", inode: 14023},
		},
		{
			name:    "connected stream",
			message: message(1, linuxDiagAttribute(linuxUnixDiagName, []byte("/run/dbus/system_bus_socket"))),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseLinuxUnixDiagMsg(tt.message)
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

// ss -p와 같은 주인을 골라야 한다. jw-server에서 ss는 dockerd(1701140)를 systemd(1)보다, 같은 snapd의 fd 9를 fd 3보다 먼저 적었다.
func TestAttachLinuxSocketOwnersPicksSameOwnerAsSS(t *testing.T) {
	root := t.TempDir()
	link := func(pid, fd, inode int) {
		directory := filepath.Join(root, strconv.Itoa(pid), "fd")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("socket:["+strconv.Itoa(inode)+"]", filepath.Join(directory, strconv.Itoa(fd))); err != nil {
			t.Fatal(err)
		}
	}
	comm := func(pid int, name string) {
		if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "comm"), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 이름 순서로 읽으면 "1701140"이 "9"보다 앞선다. 숫자로 비교하지 않으면 주인이 바뀐다.
	link(1, 213, 77903)
	link(1701140, 5, 77903)
	link(9, 1, 77903)
	link(2300973, 3, 77647)
	link(2300973, 9, 77647)
	link(1, 221, 77647)
	comm(1, "systemd")
	comm(9, "other")
	comm(1701140, "dockerd")
	comm(2300973, "snapd")
	if err := os.WriteFile(filepath.Join(root, "1", "fd", "pipe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	sockets := []listenSocket{{Proto: "unix", Address: "/run/docker.sock", inode: 77903}, {Proto: "unix", Address: "/run/snapd.socket", inode: 77647}, {Proto: "tcp", Port: 80, inode: 1}}
	attachLinuxSocketOwners(root, sockets)
	if got := sockets[0]; got.Process != "dockerd" || got.PID != "1701140" || got.FD != "5" {
		t.Fatalf("docker.sock 주인 = %#v", got)
	}
	if got := sockets[1]; got.Process != "snapd" || got.PID != "2300973" || got.FD != "9" {
		t.Fatalf("snapd.socket 주인 = %#v", got)
	}
	// 주인을 못 찾은 소켓(다른 계정의 fd)은 process 없이 남는다.
	if got := sockets[2]; got.Process != "" || got.PID != "" || got.FD != "" {
		t.Fatalf("주인이 없는 소켓에 process를 붙였다: %#v", got)
	}
}

// 실제 kernel의 sock_diag 응답을 읽는다. 상태 mask나 offset이 틀리면 포트나 주인이 어긋난다.
func TestCollectLinuxListenSocketsFindsOwnSockets(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "s.sock")
	unixListener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()

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

// container처럼 다른 network namespace에 있는 소켓은 host 덤프에 없고, 그 namespace에 들어가 읽어야 보인다.
func TestCollectNetnsListenSocketsReadsAnotherNamespace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("unshare(CLONE_NEWNET)에는 root가 필요하다")
	}
	type opened struct {
		tid, port int
		err       error
	}
	hostNamespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	ready, release, restored := make(chan opened, 1), make(chan struct{}), make(chan error, 1)
	defer func() {
		close(release)
		if err := <-restored; err != nil {
			t.Error(err)
		}
		// /proc/self는 main thread의 namespace를 보인다. 이 goroutine이 main thread에서 돌았어도 원래대로여야 한다.
		if now, err := os.Readlink("/proc/self/ns/net"); err != nil || now != hostNamespace {
			t.Errorf("/proc/self/ns/net = %s %v after the test, want %s", now, err, hostNamespace)
		}
	}()
	go func() {
		// 끝날 때 원래 namespace로 되돌린다. 이 goroutine이 main thread에서 돌면 runtime이 thread를 끝내지 못해, 되돌리지
		// 않으면 뒤의 시험이 /proc/net에서 이 namespace를 읽는다.
		runtime.LockOSThread()
		original, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			restored <- nil
			ready <- opened{err: err}
			return
		}
		defer original.Close()
		defer func() {
			if err := unix.Setns(int(original.Fd()), unix.CLONE_NEWNET); err != nil {
				restored <- fmt.Errorf("restore the network namespace: %w", err)
				return
			}
			runtime.UnlockOSThread()
			restored <- nil
		}()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			ready <- opened{err: err}
			return
		}
		listener, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			ready <- opened{err: err}
			return
		}
		defer listener.Close()
		ready <- opened{tid: unix.Gettid(), port: listener.Addr().(*net.TCPAddr).Port}
		<-release
	}()
	got := <-ready
	if got.err != nil {
		t.Fatal(got.err)
	}
	pid, want := strconv.Itoa(os.Getpid()), "tcp 0.0.0.0 "+strconv.Itoa(got.port)
	owned := func(sockets []listenSocket) bool {
		for _, socket := range sockets {
			if socket.PID == pid && socket.Proto+" "+socket.Address+" "+strconv.Itoa(socket.Port) == want {
				return true
			}
		}
		return false
	}
	sockets, err := collectNetnsListenSockets(got.tid)
	if err != nil {
		t.Fatal(err)
	}
	if !owned(sockets) {
		t.Fatalf("새 namespace에서 %q를 찾지 못했다: %+v", want, sockets)
	}
	host, err := collectListenSockets(listenFamilies{TCP: true})
	if err != nil {
		t.Fatal(err)
	}
	if owned(host) {
		t.Fatalf("%q가 host namespace에도 보인다", want)
	}
	if _, err := collectNetnsListenSockets(-1); err == nil {
		t.Fatal("없는 process의 namespace를 읽었다")
	}
}

func BenchmarkCollectLinuxListenSockets(b *testing.B) {
	families := listenFamilies{TCP: true, UDP: true, Unix: true}
	for b.Loop() {
		if _, err := collectListenSockets(families); err != nil {
			b.Fatal(err)
		}
	}
}

func TestReadLinuxProcessCommand(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "42")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cmdline"), []byte("node\x00vite\x00--port\x005173\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/srv/web", filepath.Join(directory, "cwd")); err != nil {
		t.Fatal(err)
	}
	args, cwd := readLinuxProcessCommand(root, "42")
	if !slices.Equal(args, []string{"node", "vite", "--port", "5173"}) || cwd != "/srv/web" {
		t.Fatalf("args %q cwd %q", args, cwd)
	}
	// 커널 thread는 cmdline이 비어 있다.
	if err := os.WriteFile(filepath.Join(directory, "cmdline"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if args, _ := readLinuxProcessCommand(root, "42"); args != nil {
		t.Fatalf("kernel thread args %q", args)
	}
}
