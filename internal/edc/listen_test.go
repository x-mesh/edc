package edc

import "testing"

// lsofFieldsFixture는 실제 macOS 호스트의 `lsof -nP -i -U -FpcntPT` 출력 형태다. 주소만 문서용
// 대역으로 바꿨다. LISTEN, ESTABLISHED, UDP 대기, 연결된 UDP(QUIC), 포트가 없는 *:*, 이름에 공백이
// 있는 프로세스, 경로를 가진 유닉스 소켓과 익명 유닉스 쌍을 모두 담는다.
//
// 유닉스 소켓에는 P(프로토콜) 필드가 없다. t(종류) 필드만 unix라고 알려 준다.
const lsofFieldsFixture = `p733
cssh-agent
f3
tunix
n/Users/example/.ssh/agent/s.sock
f4
tunix
n->0x98a30dc8a2a5fd7d
p947
crapportd
f12
tIPv4
PTCP
n*:50317
TST=LISTEN
f19
tIPv6
PTCP
n[fe80::1]:50317->[fe80::2]:61538
TST=ESTABLISHED
f21
tIPv4
PUDP
n*:3722
p6149
cGoogle Chrome Helper
f88
tIPv4
PUDP
n192.0.2.13:62979->198.51.100.10:443
f90
tIPv4
PUDP
n*:*
p28171
cOrbStack Helper
f7
tIPv4
PTCP
n*:3306
TST=LISTEN
`

// ssRowsFixture는 실제 Ubuntu 호스트의 `ss -Htulxnp` 출력이다. -t와 -u를 함께 주면 맨 앞에 프로토콜
// 열이 생긴다. %iface 표기와 IPv6 [::], 그리고 유닉스 소켓 줄도 그대로 담는다.
const ssRowsFixture = `udp UNCONN 0      0                       192.0.2.54:53    0.0.0.0:* users:(("systemd-resolve",pid=19605,fd=16))
udp UNCONN 0      0                    192.0.2.53%lo:53    0.0.0.0:* users:(("systemd-resolve",pid=19605,fd=14))
udp UNCONN 0      0                          0.0.0.0:41641 0.0.0.0:* users:(("tailscaled",pid=2166,fd=20))
udp UNCONN 0      0                             [::]:41641    [::]:* users:(("tailscaled",pid=2166,fd=19))
tcp LISTEN 0      4096                       0.0.0.0:22    0.0.0.0:* users:(("sshd",pid=21571,fd=3),("systemd",pid=1,fd=219))
tcp LISTEN 0      4096                          [::]:22       [::]:* users:(("sshd",pid=21571,fd=4))
u_str LISTEN 0    4096     /run/systemd/private 14023   * 0 users:(("systemd",pid=1,fd=17))
u_str ESTAB  0    0        /run/dbus/system_bus_socket 18894 * 18893 users:(("dbus-daemon",pid=888,fd=9))
u_dgr UNCONN 0    0        /run/systemd/journal/socket 13998 * 0 users:(("systemd",pid=1,fd=11))
`

func TestParseLsofFieldsKeepsOnlyWaitingSockets(t *testing.T) {
	sockets, unparsed := parseLsofFields(lsofFieldsFixture, listenFamilies{TCP: true})
	if unparsed != 0 {
		t.Fatalf("unparsed = %d, want 0", unparsed)
	}
	if len(sockets) != 2 {
		t.Fatalf("sockets = %#v, want 2 TCP LISTEN entries", sockets)
	}
	// 이름에 공백이 있어도 -F는 필드를 한 줄에 담으므로 잘리지 않는다.
	if sockets[0].Process != "rapportd" || sockets[0].Port != 50317 || sockets[0].PID != "947" {
		t.Fatalf("socket 0 = %#v", sockets[0])
	}
	if sockets[1].Process != "OrbStack Helper" || sockets[1].Port != 3306 {
		t.Fatalf("socket 1 = %#v", sockets[1])
	}
}

// UDP에는 상태가 없어 lsof가 연결된 흐름까지 돌려준다. 주소의 ->로 가려내지 않으면 QUIC 흐름이
// 대기 중인 소켓으로 섞인다.
func TestParseLsofFieldsExcludesConnectedUDP(t *testing.T) {
	sockets, unparsed := parseLsofFields(lsofFieldsFixture, listenFamilies{UDP: true})
	if unparsed != 0 {
		t.Fatalf("unparsed = %d, want 0", unparsed)
	}
	for _, socket := range sockets {
		if socket.Port == 443 {
			t.Fatalf("연결된 UDP 흐름이 섞였다: %#v", socket)
		}
	}
	found := false
	for _, socket := range sockets {
		if socket.Proto == "udp" && socket.Port == 3722 {
			found = true
		}
	}
	if !found {
		t.Fatalf("대기 중인 UDP 소켓이 빠졌다: %#v", sockets)
	}
}

func TestParseSSRowsReadsProtocolColumn(t *testing.T) {
	sockets, unparsed := parseSSRows(ssRowsFixture, listenFamilies{TCP: true, UDP: true})
	if unparsed != 0 {
		t.Fatalf("unparsed = %d, want 0", unparsed)
	}
	byPort := map[int]listenSocket{}
	for _, socket := range sockets {
		byPort[socket.Port] = socket
	}
	if socket := byPort[22]; socket.Proto != "tcp" || socket.Process != "sshd" || socket.PID != "21571" {
		t.Fatalf("22번 포트 = %#v", socket)
	}
	// %iface 표기는 주소에서 떼어 낸다.
	if socket := byPort[53]; socket.Proto != "udp" || socket.Address != "192.0.2.53" && socket.Address != "192.0.2.54" {
		t.Fatalf("53번 포트 = %#v", socket)
	}
	// udp를 끄면 tcp만 남는다.
	tcpOnly, _ := parseSSRows(ssRowsFixture, listenFamilies{TCP: true})
	for _, socket := range tcpOnly {
		if socket.Proto != "tcp" {
			t.Fatalf("udp를 끄면 tcp만 남아야 한다: %#v", socket)
		}
	}
}

// 한 포트를 IPv4와 IPv6로 함께 열면 도구는 두 줄을 내지만 사람에게는 같은 사실이다.
func TestDedupeListenSocketsMergesDualStack(t *testing.T) {
	sockets, _ := parseSSRows(ssRowsFixture, listenFamilies{TCP: true, UDP: true})
	unique := dedupeListenSockets(sockets)
	count := 0
	for _, socket := range unique {
		if socket.Port == 22 {
			count++
		}
	}
	if count != 2 {
		// 0.0.0.0과 [::]는 주소가 달라 각각 남는다. 주소가 같은 중복만 합친다.
		t.Fatalf("22번 포트 항목 = %d, want 2 (0.0.0.0과 ::)", count)
	}
	// 포트 오름차순으로 정렬한다.
	for index := 1; index < len(unique); index++ {
		if unique[index-1].Port > unique[index].Port {
			t.Fatalf("포트 정렬이 깨졌다: %#v", unique)
		}
	}
}

// 포트가 없는 유닉스 소켓이 앞에 오면 --all에서 정작 찾는 포트 목록이 아래로 밀린다.
func TestDedupeListenSocketsPutsUnixLast(t *testing.T) {
	sockets, _ := parseSSRows(ssRowsFixture, listenFamilies{TCP: true, UDP: true, Unix: true})
	unique := dedupeListenSockets(sockets)
	if len(unique) < 2 {
		t.Fatalf("sockets = %#v", unique)
	}
	if unique[0].Port == 0 {
		t.Fatalf("첫 줄이 포트 없는 소켓이다: %#v", unique[0])
	}
	if last := unique[len(unique)-1]; last.Proto != "unix" {
		t.Fatalf("마지막 줄 = %#v, want unix", last)
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		value string
		host  string
		port  int
		ok    bool
	}{
		{"*:50317", "*", 50317, true},
		{"127.0.0.1:45103", "127.0.0.1", 45103, true},
		// 0.0.0.0과 ::를 *로 뭉치지 않는다. IPv4에만 바인드된 서비스를 못 알아보면 진단이 안 된다.
		{"[::]:22", "::", 22, true},
		{"192.0.2.53%lo:53", "192.0.2.53", 53, true},
		{"[fe80::1]:546", "fe80::1", 546, true},
		{"*:*", "", 0, false},
		{"noport", "", 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.value, func(t *testing.T) {
			host, port, ok := splitHostPort(testCase.value)
			if ok != testCase.ok || (ok && (host != testCase.host || port != testCase.port)) {
				t.Fatalf("splitHostPort(%q) = (%q, %d, %v), want (%q, %d, %v)", testCase.value, host, port, ok, testCase.host, testCase.port, testCase.ok)
			}
		})
	}
}

// 유닉스 소켓은 포트가 없고 경로를 가진다. splitHostPort로 보내면 읽지 못한 줄로 세어 경고가 뜬다.
func TestParseSSRowsReadsUnixPaths(t *testing.T) {
	sockets, unparsed := parseSSRows(ssRowsFixture, listenFamilies{Unix: true})
	if unparsed != 0 {
		t.Fatalf("unparsed = %d, want 0", unparsed)
	}
	// u_str LISTEN 하나만 남는다. ESTAB는 연결된 것이고 u_dgr에는 LISTEN이 없다.
	if len(sockets) != 1 {
		t.Fatalf("sockets = %#v, want 1", sockets)
	}
	socket := sockets[0]
	if socket.Proto != "unix" || socket.Address != "/run/systemd/private" || socket.Port != 0 {
		t.Fatalf("unix socket = %#v", socket)
	}
	if socket.Process != "systemd" || socket.PID != "1" {
		t.Fatalf("unix socket process = %#v", socket)
	}
}

// lsof는 유닉스 소켓에 P 필드를 주지 않는다. t 필드를 읽지 않으면 종류를 모른다.
func TestParseLsofFieldsReadsUnixFromTypeField(t *testing.T) {
	sockets, unparsed := parseLsofFields(lsofFieldsFixture, listenFamilies{Unix: true})
	if unparsed != 0 {
		t.Fatalf("unparsed = %d, want 0", unparsed)
	}
	if len(sockets) != 1 {
		// ->0x...는 이름 없는 유닉스 쌍이다. 경로가 없으므로 보여 줄 것이 없다.
		t.Fatalf("sockets = %#v, want 1", sockets)
	}
	if sockets[0].Proto != "unix" || sockets[0].Address != "/Users/example/.ssh/agent/s.sock" {
		t.Fatalf("unix socket = %#v", sockets[0])
	}
}

// 종류 flag는 더하기가 아니라 filter다. 하나라도 고르면 고른 것만 남아야 한다.
func TestSelectedListenFamilies(t *testing.T) {
	cases := []struct {
		name                string
		tcp, udp, unix, all bool
		want                listenFamilies
	}{
		{name: "기본", want: listenFamilies{TCP: true, UDP: true}},
		{name: "tcp만", tcp: true, want: listenFamilies{TCP: true}},
		{name: "udp만", udp: true, want: listenFamilies{UDP: true}},
		{name: "unix만", unix: true, want: listenFamilies{Unix: true}},
		{name: "합집합", tcp: true, unix: true, want: listenFamilies{TCP: true, Unix: true}},
		{name: "all", all: true, want: listenFamilies{TCP: true, UDP: true, Unix: true}},
		// --all은 다른 flag를 이긴다. 전부 보자는 뜻이 좁히자는 뜻보다 분명하다.
		{name: "all이 우선", tcp: true, all: true, want: listenFamilies{TCP: true, UDP: true, Unix: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := selectedListenFamilies(testCase.tcp, testCase.udp, testCase.unix, testCase.all)
			if got != testCase.want {
				t.Fatalf("selectedListenFamilies = %#v, want %#v", got, testCase.want)
			}
		})
	}
}
