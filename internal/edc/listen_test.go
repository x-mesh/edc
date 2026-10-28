package edc

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// listenSocketsFixture는 dual-stack 포트, 서로 다른 프로토콜의 같은 포트, 유닉스 소켓을 섞어 둔 목록이다.
var listenSocketsFixture = []listenSocket{
	{Proto: "unix", Address: "/run/systemd/private", Process: "systemd", PID: "1"},
	{Proto: "udp", Address: "0.0.0.0", Port: 41641, Process: "tailscaled", PID: "2166"},
	{Proto: "tcp", Address: "::", Port: 22, Process: "sshd", PID: "21571"},
	{Proto: "udp", Address: "192.0.2.54", Port: 53, Process: "systemd-resolve", PID: "19605"},
	{Proto: "tcp", Address: "0.0.0.0", Port: 22, Process: "sshd", PID: "21571"},
	{Proto: "tcp", Address: "0.0.0.0", Port: 22, Process: "sshd", PID: "21571"},
}

// 한 포트를 IPv4와 IPv6로 함께 열면 도구는 두 줄을 내지만 사람에게는 같은 사실이다.
func TestDedupeListenSocketsMergesDualStack(t *testing.T) {
	// 첫 항목은 유닉스 소켓이다. 포트 정렬만 보려고 뺀다.
	unique := dedupeListenSockets(listenSocketsFixture[1:])
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
	unique := dedupeListenSockets(listenSocketsFixture)
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

// -v는 열을 늘릴 뿐 줄을 늘리지 않는다. fd가 식별에 끼면 같은 소켓이 두 줄이 되어 -v를 준 화면과
// 주지 않은 화면의 개수가 달라진다.
func TestDedupeListenSocketsIgnoresDetailFields(t *testing.T) {
	same := listenSocket{Proto: "tcp", Address: "0.0.0.0", Port: 22, Process: "sshd", PID: "1"}
	other := same
	other.FD, other.RecvQ, other.HasQueue = "9", 3, true
	unique := dedupeListenSockets([]listenSocket{same, other})
	if len(unique) != 1 {
		t.Fatalf("sockets = %#v, want 1", unique)
	}
}

func TestFormatListenTableDetailColumns(t *testing.T) {
	sockets := []listenSocket{
		{Proto: "tcp", Address: "0.0.0.0", Port: 22, Process: "sshd", PID: "1", User: "root", FD: "3", RecvQ: 0, SendQ: 4096, HasQueue: true},
		{Proto: "unix", Address: "/run/docker.sock", Process: "dockerd", PID: "895"},
	}
	plain := formatListenTable(sockets, false, nil)
	if strings.Contains(plain, "4096") {
		t.Fatalf("-v가 없는데 상세 열이 나왔다:\n%s", plain)
	}
	detail := formatListenTable(sockets, true, nil)
	if !strings.Contains(detail, "0/4096") {
		t.Fatalf("큐 열이 없다:\n%s", detail)
	}
	// 값이 없는 칸은 빈 칸이 아니라 없음이다. 빈 칸은 0인지 모르는지를 구별해 주지 않는다.
	if !strings.Contains(detail, "-  ") {
		t.Fatalf("없는 값을 빈 칸으로 두었다:\n%s", detail)
	}
	if len(strings.Split(strings.TrimRight(plain, "\n"), "\n")) != len(strings.Split(strings.TrimRight(detail, "\n"), "\n")) {
		t.Fatalf("-v가 줄 수를 바꿨다")
	}
}

func TestListenBindingScope(t *testing.T) {
	tests := []struct {
		proto, address, want string
	}{
		{"tcp", "*", "all"}, {"udp", "0.0.0.0", "all"},
		{"tcp", "::", "all"}, {"tcp", "::ffff:0.0.0.0", "all"},
		{"tcp", "127.0.0.1", "loopback"}, {"tcp", "127.20.30.40", "loopback"},
		{"tcp", "::1", "loopback"}, {"tcp", "::ffff:127.0.0.1", "loopback"},
		{"udp", "192.0.2.10", "specific"}, {"tcp", "fe80::1%en0", "specific"},
		{"tcp", "::1%lo0", "loopback"}, {"tcp", "", "unknown"},
		{"tcp", "localhost", "unknown"}, {"tcp", "999.0.0.1", "unknown"},
		{"tcp", "127.0.0.1:80", "unknown"}, {"other", "*", "unknown"},
		{"unix", "/run/app.sock", "unix"}, {"unix", "", "unix"},
	}
	for _, tt := range tests {
		t.Run(tt.proto+"/"+tt.address, func(t *testing.T) {
			if got := listenBindingScope(tt.proto, tt.address); got != tt.want {
				t.Fatalf("scope = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatListenBindingScopeLocales(t *testing.T) {
	restore := currentLanguage()
	defer setLanguage(restore)
	sockets := []listenSocket{
		{Proto: "tcp", Address: "::1", Port: 80, Process: "app", PID: "42"},
		{Proto: "udp", Address: "*", Port: 53, Process: "resolver", PID: "7"},
		{Proto: "tcp", Address: "fe80::1%en0", Port: 81, Process: "other"},
	}
	for _, lang := range []string{"en", "ko", "ja"} {
		setLanguage(lang)
		for _, detail := range []bool{false, true} {
			table := formatListenTable(sockets, detail, nil)
			lines := strings.Split(strings.TrimSuffix(table, "\n"), "\n")
			if len(lines) != 4 {
				t.Fatalf("%s rows = %d", lang, len(lines))
			}
			header := lines[0]
			addressAt := strings.Index(header, T("observe.listen.column.address"))
			scopeAt := strings.Index(header, T("observe.listen.column.scope"))
			processAt := strings.Index(header, T("observe.listen.column.process"))
			if !(addressAt < scopeAt && scopeAt < processAt) {
				t.Fatalf("column order: %s", header)
			}
			wantWidth := liveWidth(header[:processAt])
			for i, socket := range sockets {
				line := lines[i+1]
				index := strings.Index(line, socket.Process)
				if index < 0 || liveWidth(line[:index]) != wantWidth {
					t.Fatalf("column alignment: %s", line)
				}
				scope := T("observe.listen.scope." + listenBindingScope(socket.Proto, socket.Address))
				if !strings.Contains(line, scope) {
					t.Fatalf("missing scope: %s", line)
				}
			}
			redacted := redactIPAddresses(table)
			if strings.Contains(redacted, "::1") || strings.Contains(redacted, "fe80::1") || !strings.Contains(redacted, T("observe.listen.scope.loopback")) {
				t.Fatalf("redacted table: %s", redacted)
			}
		}
	}
}

func TestListenBindingSocketJSONUnchanged(t *testing.T) {
	socket := listenSocket{Proto: "tcp", Address: "127.0.0.1", Port: 80, Process: "app", PID: "42"}
	formatListenTable([]listenSocket{socket}, true, nil)
	got, err := json.Marshal(socket)
	want := `{"proto":"tcp","address":"127.0.0.1","port":80,"process":"app","pid":"42","recv_q":0,"send_q":0,"has_queue":false}`
	if err != nil || string(got) != want {
		t.Fatalf("socket JSON = %s, error = %v", got, err)
	}
}

func TestListenCommandLine(t *testing.T) {
	const home = "/home/dev"
	tests := []struct {
		name   string
		socket listenSocket
		want   string
	}{
		{"interpreter shows the script", listenSocket{Process: "node", Args: []string{"node", "/home/dev/web/node_modules/.bin/vite", "--port", "5173"}, Cwd: "/home/dev/web"}, "~/web   vite --port 5173"},
		{"versioned interpreter", listenSocket{Process: "python3.13", Args: []string{"/usr/bin/python3.13", "tools/devserver.py"}}, "devserver.py"},
		{"interpreter flag keeps the interpreter", listenSocket{Process: "node", Args: []string{"node", "--inspect", "app.js"}}, "node --inspect app.js"},
		{"rewritten argv keeps its title", listenSocket{Process: "nginx", Args: []string{"nginx: master process /usr/sbin/nginx"}}, "nginx: master process /usr/sbin/nginx"},
		{"argument with a space is quoted", listenSocket{Process: "app", Args: []string{"/opt/app", "--name", "my app"}}, `app --name "my app"`},
		{"empty arguments are dropped", listenSocket{Process: "node", Args: []string{"Raycast Backend", "", ""}}, "Raycast Backend"},
		{"nothing beyond the process column", listenSocket{Process: "sshd", Args: []string{"/usr/sbin/sshd"}, Cwd: "/"}, ""},
		{"truncated comm", listenSocket{Process: "systemd-resolve", Args: []string{"/usr/lib/systemd/systemd-resolved"}}, ""},
		{"working directory alone", listenSocket{Process: "sshd", Args: []string{"/usr/sbin/sshd"}, Cwd: "/srv"}, "/srv"},
		{"control characters are replaced", listenSocket{Process: "x", Args: []string{"x", "a\x1b[2Jb"}}, "x a?[2Jb"},
		{"no command", listenSocket{Process: "x"}, ""},
	}
	for _, tt := range tests {
		if got := listenCommandLine(tt.socket, home); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestListenMaskArgs(t *testing.T) {
	got := listenMaskArgs([]string{"app", "--password", "server", "--password=a", "--api-key", "k", "--db-token=t", "--verbose", "--secret", "--port", "5432", "postgres://user:pw@db:5432/app", "--name", "token", "--bypass", "cache", "--passthrough=on"})
	want := []string{"app", "--password", "***", "--password=***", "--api-key", "***", "--db-token=***", "--verbose", "--secret", "--port", "5432", "postgres://user:***@db:5432/app", "--name", "token", "--bypass", "cache", "--passthrough=on"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// --full은 행을 늘리지 않고 각 행 아래에 명령줄을 붙인다. 명령줄이 없는 행에는 붙이지 않는다.
func TestFormatListenTableCommandLines(t *testing.T) {
	sockets := []listenSocket{
		{Proto: "tcp", Address: "*", Port: 5173, Process: "node", PID: "7", Args: []string{"node", "vite"}, Cwd: "/srv/web"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 6379, Process: "redis", PID: "8"},
	}
	table := formatListenTable(sockets, false, func(text string) string { return "<" + text + ">" })
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want header, two rows and one command:\n%s", len(lines), table)
	}
	if want := strings.Repeat(" ", len("PORT")+2) + "<↳ /srv/web   vite>"; lines[2] != want {
		t.Fatalf("command line %q, want %q", lines[2], want)
	}
	if plain := formatListenTable(sockets, false, nil); strings.Contains(plain, "vite") {
		t.Fatalf("command lines without --full:\n%s", plain)
	}
}
