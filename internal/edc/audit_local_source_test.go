package edc

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

func testTCP(local, remote string, inode uint64) auditTCPSocket {
	return auditTCPSocket{Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort(remote), Inode: inode}
}

type testLocalProc struct {
	pid                int
	comm, exe, cmdline string
	inodes             []uint64
}

func writeLocalSourceProcs(t *testing.T, root string, procs ...testLocalProc) {
	t.Helper()
	for _, proc := range procs {
		directory := filepath.Join(root, "proc", strconv.Itoa(proc.pid))
		if err := os.MkdirAll(filepath.Join(directory, "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		for index, inode := range proc.inodes {
			if err := os.Symlink("socket:["+strconv.FormatUint(inode, 10)+"]", filepath.Join(directory, "fd", strconv.Itoa(index+3))); err != nil {
				t.Fatal(err)
			}
		}
		if proc.comm != "" {
			writeAuditFile(t, root, "/proc/"+strconv.Itoa(proc.pid)+"/comm", proc.comm+"\n", 0o644)
		}
		if proc.exe != "" {
			if err := os.Symlink(proc.exe, filepath.Join(directory, "exe")); err != nil {
				t.Fatal(err)
			}
		}
		if proc.cmdline != "" {
			writeAuditFile(t, root, "/proc/"+strconv.Itoa(proc.pid)+"/cmdline", proc.cmdline, 0o644)
		}
	}
}

type testLocalSources struct {
	addrs    []netip.Addr
	addrsErr error
	sockets  []auditTCPSocket
	tcpErr   error
	tcpCalls int
}

func (fake *testLocalSources) sources(root string) auditSources {
	return auditSources{
		root:           root,
		interfaceAddrs: func() ([]netip.Addr, error) { return fake.addrs, fake.addrsErr },
		tcpConnected: func() ([]auditTCPSocket, error) {
			fake.tcpCalls++
			return fake.sockets, fake.tcpErr
		},
	}
}

func testLocalFacts(accepted ...auditAccepted) auditFacts {
	return auditFacts{Root: true, JournalRead: true, JournalSince: testLoginAt(0), Accepted: accepted}
}

func testClosedAccepted(user, host, method string, minutes int) auditAccepted {
	return auditAccepted{PID: 100 + minutes, User: user, Host: host, Method: method, Time: testLoginAt(minutes), Until: testLoginAt(minutes + 1)}
}

func evaluateLocalSource(sources auditSources, facts auditFacts) Result {
	collectAuditLocalSource(sources, &facts)
	return auditLoginLocalSource(facts)
}

func TestAuditLocalSourceJudgesSourceAddresses(t *testing.T) {
	fake := &testLocalSources{addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("fd00::5"), netip.MustParseAddr("fe80::1")}}
	hosts := []string{"127.0.0.1", "127.5.6.7", "::1", "::ffff:127.0.0.1", "10.0.0.5", "fd00::5", "fe80::1%eth0", "::ffff:10.0.0.5", "203.0.113.7", "2001:db8::5"}
	var accepted []auditAccepted
	for index, host := range hosts {
		accepted = append(accepted, testClosedAccepted("root", host, "publickey", index))
	}
	facts := testLocalFacts(accepted...)
	collectAuditLocalSource(fake.sources(t.TempDir()), &facts)
	var got []string
	for _, login := range facts.LocalSource.Logins {
		got = append(got, login.Host)
	}
	if want := hosts[:8]; !slices.Equal(got, want) || facts.LocalSource.Checked != len(hosts) {
		t.Fatalf("logins = %v, checked = %d", got, facts.LocalSource.Checked)
	}
	if fake.tcpCalls != 0 {
		t.Fatalf("tcp calls = %d", fake.tcpCalls)
	}
}

func TestAuditLocalSourcePassesWhenEverySourceIsExternal(t *testing.T) {
	fake := &testLocalSources{addrs: []netip.Addr{netip.MustParseAddr("10.0.0.5")}}
	result := evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts(testClosedAccepted("root", "203.0.113.7", "publickey", 1), testClosedAccepted("deploy", "198.51.100.4", "password", 2)))
	if result.Status != StatusPass || result.Summary != T("audit.login.local_source_ok", 2, auditLoginTime(testLoginAt(0))) || len(result.Evidence) != 0 || len(result.Warnings) != 0 || result.Next != "" {
		t.Fatalf("result = %+v", result)
	}
	result = evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts())
	if result.Status != StatusPass {
		t.Fatalf("empty = %+v", result)
	}
}

func TestAuditLocalSourceCountsSourcesThatAreNotAddresses(t *testing.T) {
	fake := &testLocalSources{}
	result := evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts(testClosedAccepted("root", "admin.example.net", "publickey", 1), testClosedAccepted("root", "127.0.0.1", "publickey", 2)))
	if result.Status != StatusWarn || result.Summary != T("audit.login.local_source_bad", 1, 0, auditLoginTime(testLoginAt(0))) || !slices.Contains(result.Warnings, T("audit.login.local_unparsed", 1)) {
		t.Fatalf("result = %+v", result)
	}
	only := evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts(testClosedAccepted("root", "admin.example.net", "publickey", 1)))
	if only.Status != StatusPass || !slices.Contains(only.Warnings, T("audit.login.local_unparsed", 1)) {
		t.Fatalf("only = %+v", only)
	}
}

// OpenSSH 10.0p2에서 잰 모양이다. 서버 쪽 소켓은 Accepted를 남긴 PID가 쥐고, 상대 소켓은 터널을 나르는 process가 쥔다.
func testOpenTunnel(t *testing.T) (auditSources, *testLocalSources, auditFacts) {
	t.Helper()
	root := t.TempDir()
	writeLocalSourceProcs(t, root,
		testLocalProc{pid: 2546284, inodes: []uint64{1000}},
		testLocalProc{pid: 2546228, inodes: []uint64{1000, 1000}},
		testLocalProc{pid: 2546150, comm: "sshd-session", exe: "/usr/lib/openssh/sshd-session", cmdline: "sshd-session: root@notty\x00", inodes: []uint64{2000, 2000}},
		testLocalProc{pid: 77, comm: "unrelated", inodes: []uint64{3000}},
	)
	fake := &testLocalSources{sockets: []auditTCPSocket{
		testTCP("127.0.0.1:22", "127.0.0.1:54424", 1000),
		testTCP("127.0.0.1:54424", "127.0.0.1:22", 2000),
		testTCP("127.0.0.1:22", "127.0.0.1:54425", 3000),
	}}
	facts := testLocalFacts(auditAccepted{PID: 2546228, User: "root", Host: "127.0.0.1", Method: "publickey", Port: 54424, Time: testLoginAt(5)})
	return fake.sources(root), fake, facts
}

func TestAuditLocalSourceNamesThePeerOfAnOpenConnection(t *testing.T) {
	sources, _, facts := testOpenTunnel(t)
	result := evaluateLocalSource(sources, facts)
	if result.Status != StatusWarn || result.Summary != T("audit.login.local_source_bad", 1, 1, auditLoginTime(testLoginAt(0))) {
		t.Fatalf("result = %+v", result)
	}
	want := "root  127.0.0.1:54424  publickey  " + auditLoginTime(testLoginAt(5)) + "\n" + auditDetailPrefix + T("audit.login.peer") + "  pid 2546150  sshd-session  /usr/lib/openssh/sshd-session  sshd-session: root@notty"
	if len(result.Evidence) != 1 || result.Evidence[0].Label != T("audit.evidence.open_logins") || result.Evidence[0].Value != want {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if result.Next != "ls -l /proc/2546150/exe; cat /proc/2546150/cmdline | tr '\\0' ' '" || len(result.Warnings) != 0 {
		t.Fatalf("next = %q, warnings = %v", result.Next, result.Warnings)
	}
}

func TestAuditLocalSourceIgnoresASocketThatTheAcceptedPIDDoesNotHold(t *testing.T) {
	sources, _, facts := testOpenTunnel(t)
	// 같은 remote 행이 있어도 이 PID는 그 소켓을 쥐지 않는다. 포트를 재사용한 다른 연결이다.
	facts.Accepted[0].PID = 77
	result := evaluateLocalSource(sources, facts)
	if result.Status != StatusWarn || result.Summary != T("audit.login.local_source_bad", 1, 0, auditLoginTime(testLoginAt(0))) {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Label != T("audit.evidence.closed_logins") || strings.Contains(result.Evidence[0].Value, "2546150") {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	if !strings.HasPrefix(result.Next, "journalctl -t sshd -t sshd-session --since '"+auditLoginTime(testLoginAt(4))) {
		t.Fatalf("next = %q", result.Next)
	}
	facts.Accepted[0].PID = 0
	if result := evaluateLocalSource(sources, facts); result.Summary != T("audit.login.local_source_bad", 1, 0, auditLoginTime(testLoginAt(0))) || result.Evidence[0].Label != T("audit.evidence.closed_logins") {
		t.Fatalf("pid 0 = %+v", result)
	}
}

func TestAuditLocalSourceKeepsAnOpenConnectionWithoutPeerProcess(t *testing.T) {
	sources, fake, facts := testOpenTunnel(t)
	notFound := T("audit.login.peer_none")
	line := "root  127.0.0.1:54424  publickey  " + auditLoginTime(testLoginAt(5)) + "\n" + auditNotePrefix + notFound

	// 상대 소켓 행이 이 network namespace의 표에 없다.
	fake.sockets = []auditTCPSocket{fake.sockets[0]}
	result := evaluateLocalSource(sources, facts)
	if result.Status != StatusWarn || len(result.Evidence) != 1 || result.Evidence[0].Value != line || !strings.HasPrefix(result.Next, "journalctl") {
		t.Fatalf("no peer row = %+v", result)
	}

	// 상대 소켓 행은 있지만 그 소켓을 쥔 process가 없다.
	fake.sockets = []auditTCPSocket{fake.sockets[0], testTCP("127.0.0.1:54424", "127.0.0.1:22", 9999)}
	result = evaluateLocalSource(sources, facts)
	if len(result.Evidence) != 1 || result.Evidence[0].Value != line || len(result.Warnings) != 0 {
		t.Fatalf("no peer process = %+v", result)
	}
}

func TestAuditLocalSourceWritesOnlyThePeerValuesItCanRead(t *testing.T) {
	sources, _, facts := testOpenTunnel(t)
	root := sources.root
	if err := os.Remove(filepath.Join(root, "proc", "2546150", "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "proc", "2546150", "comm")); err != nil {
		t.Fatal(err)
	}
	result := evaluateLocalSource(sources, facts)
	if want := "pid 2546150  sshd-session: root@notty"; !strings.HasSuffix(result.Evidence[0].Value, "  "+want) {
		t.Fatalf("evidence = %q", result.Evidence[0].Value)
	}
}

func TestAuditLocalSourceLimitsPeersPerLogin(t *testing.T) {
	root := t.TempDir()
	procs := []testLocalProc{{pid: 50, inodes: []uint64{1000}}}
	for _, pid := range []int{900, 20, 1000, 30, 5} {
		procs = append(procs, testLocalProc{pid: pid, comm: "p" + strconv.Itoa(pid), inodes: []uint64{2000}})
	}
	writeLocalSourceProcs(t, root, procs...)
	fake := &testLocalSources{sockets: []auditTCPSocket{testTCP("[::1]:22", "[::1]:40000", 1000), testTCP("[::1]:40000", "[::1]:22", 2000)}}
	result := evaluateLocalSource(fake.sources(root), testLocalFacts(auditAccepted{PID: 50, User: "root", Host: "::1", Method: "publickey", Port: 40000, Time: testLoginAt(1)}))
	value := result.Evidence[0].Value
	if !strings.Contains(value, "[::1]:40000") || !strings.HasSuffix(value, "pid 5  p5\n"+auditDetailPrefix+T("audit.login.peer")+"  pid 20  p20\n"+auditDetailPrefix+T("audit.login.peer")+"  pid 30  p30\n"+auditDetailPrefix+"+2") || strings.Contains(value, "pid 900") {
		t.Fatalf("value = %q", value)
	}
	if result.Next != "ls -l /proc/5/exe; cat /proc/5/cmdline | tr '\\0' ' '" {
		t.Fatalf("next = %q", result.Next)
	}
}

func TestAuditLocalSourceMasksSecretsInPeerArguments(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("a", auditLocalSourceArgsLimit*2)
	writeLocalSourceProcs(t, root,
		testLocalProc{pid: 50, inodes: []uint64{1000}},
		testLocalProc{pid: 60, comm: "tool", cmdline: "tool\x00--password\x00hunter2\x00--url\x00https://user:pw@example.net/x\x00" + long + "\x00", inodes: []uint64{2000}},
	)
	fake := &testLocalSources{sockets: []auditTCPSocket{testTCP("127.0.0.1:22", "127.0.0.1:40000", 1000), testTCP("127.0.0.1:40000", "127.0.0.1:22", 2000)}}
	value := evaluateLocalSource(fake.sources(root), testLocalFacts(auditAccepted{PID: 50, User: "root", Host: "127.0.0.1", Method: "publickey", Port: 40000, Time: testLoginAt(1)})).Evidence[0].Value
	if strings.Contains(value, "hunter2") || strings.Contains(value, "pw@") || !strings.Contains(value, "--password ***") || !strings.Contains(value, "://user:***@example.net") || strings.Contains(value, long) || strings.Count(value, "\n") != 1 {
		t.Fatalf("value = %q", value)
	}
}

// hidepid나 root 아닌 실행으로 sshd의 fd를 읽지 못하면 끝난 로그인으로 치지 않는다. fd를 파일로 두면 root로 돌려도 읽히지 않는다.
func TestAuditLocalSourceKeepsUnreadableLoginsApart(t *testing.T) {
	root := t.TempDir()
	writeLocalSourceProcs(t, root, testLocalProc{pid: 50})
	fd := filepath.Join(root, "proc", "50", "fd")
	if err := os.Remove(fd); err != nil {
		t.Fatal(err)
	}
	writeAuditFile(t, root, "/proc/50/fd", "", 0o644)
	_, readErr := os.ReadDir(fd)
	fake := &testLocalSources{sockets: []auditTCPSocket{testTCP("127.0.0.1:22", "127.0.0.1:40000", 1000), testTCP("127.0.0.1:40000", "127.0.0.1:22", 2000)}}
	result := evaluateLocalSource(fake.sources(root), testLocalFacts(auditAccepted{PID: 50, User: "root", Host: "127.0.0.1", Method: "publickey", Port: 40000, Time: testLoginAt(1)}))
	if result.Status != StatusWarn || len(result.Evidence) != 1 || result.Evidence[0].Label != T("audit.evidence.unverified_logins") ||
		!reflect.DeepEqual(result.Warnings, []string{T("audit.login.local_fd", 1, infoSingleLine(readErr.Error()))}) {
		t.Fatalf("result = %+v", result)
	}
	// process가 끝나 fd 디렉터리가 없으면 그 연결은 끝난 것이다.
	if err := os.RemoveAll(filepath.Join(root, "proc", "50")); err != nil {
		t.Fatal(err)
	}
	if result := evaluateLocalSource(fake.sources(root), testLocalFacts(auditAccepted{PID: 50, User: "root", Host: "127.0.0.1", Method: "publickey", Port: 40000, Time: testLoginAt(1)})); result.Evidence[0].Label != T("audit.evidence.closed_logins") || len(result.Warnings) != 0 {
		t.Fatalf("gone = %+v", result)
	}
}

// 실행 파일 경로와 명령 인자의 제어 문자는 escape해서, 근거 줄을 꾸며 넣거나 terminal을 바꾸지 못한다.
func TestAuditLocalSourceEscapesControlCharacters(t *testing.T) {
	root := t.TempDir()
	writeLocalSourceProcs(t, root,
		testLocalProc{pid: 50, inodes: []uint64{1000}},
		testLocalProc{pid: 60, comm: "tool", exe: "/tmp/x\nWARN  fake\x1b[2J", cmdline: "tool\x00\x1b]0;title\x07\x00", inodes: []uint64{2000}},
	)
	fake := &testLocalSources{sockets: []auditTCPSocket{testTCP("127.0.0.1:22", "127.0.0.1:40000", 1000), testTCP("127.0.0.1:40000", "127.0.0.1:22", 2000)}}
	value := evaluateLocalSource(fake.sources(root), testLocalFacts(auditAccepted{PID: 50, User: "root", Host: "127.0.0.1", Method: "publickey", Port: 40000, Time: testLoginAt(1)})).Evidence[0].Value
	if strings.ContainsFunc(value, func(char rune) bool { return char != '\n' && unicode.IsControl(char) }) || strings.Count(value, "\n") != 1 ||
		!strings.Contains(value, `/tmp/x\nWARN  fake\x1b[2J`) || !strings.Contains(value, `tool \x1b]0;title\x07`) {
		t.Fatalf("value = %q", value)
	}
}

func TestAuditPrintable(t *testing.T) {
	for in, want := range map[string]string{"plain 한글": "plain 한글", "a\tb\r\n": `a\tb\r\n`, "\x00\x7f\u0085": `\x00\x7f\x85`} {
		if got := auditPrintable(in); got != want {
			t.Errorf("auditPrintable(%q) = %q, want %q", in, got, want)
		}
	}
	if got := auditItem("x\ny", []string{"\x1b[0m"}, nil); got != `x\ny`+"\n"+auditDetailPrefix+`\x1b[0m` {
		t.Fatalf("item = %q", got)
	}
}

func TestAuditLocalSourceWarnsWhenTheTCPSourceIsMissing(t *testing.T) {
	_, _, facts := testOpenTunnel(t)
	login := facts.Accepted[0]
	for name, fake := range map[string]*testLocalSources{"error": {tcpErr: errors.New("open /proc/net/tcp: boom\nx")}, "nil": nil} {
		sources := auditSources{root: t.TempDir(), interfaceAddrs: func() ([]netip.Addr, error) { return nil, nil }}
		want := T("audit.login.local_tcp", "no source")
		if fake != nil {
			sources = fake.sources(t.TempDir())
			want = T("audit.login.local_tcp", "open /proc/net/tcp: boom x")
		}
		result := evaluateLocalSource(sources, testLocalFacts(login))
		if result.Status != StatusWarn || !reflect.DeepEqual(result.Warnings, []string{want}) || result.Evidence[0].Label != T("audit.evidence.closed_logins") || !strings.HasPrefix(result.Next, "journalctl") {
			t.Fatalf("%s: result = %+v", name, result)
		}
	}
}

func TestAuditLocalSourceComparesLoopbackOnlyWithoutInterfaceAddresses(t *testing.T) {
	accepted := []auditAccepted{testClosedAccepted("root", "127.0.0.1", "publickey", 1), testClosedAccepted("root", "100.83.200.248", "publickey", 2)}
	for name, sources := range map[string]auditSources{
		"error": {root: t.TempDir(), interfaceAddrs: func() ([]netip.Addr, error) { return nil, errors.New("netlink: denied") }},
		"nil":   {root: t.TempDir()},
	} {
		result := evaluateLocalSource(sources, testLocalFacts(accepted...))
		reason := "no source"
		if name == "error" {
			reason = "netlink: denied"
		}
		if result.Status != StatusWarn || result.Summary != T("audit.login.local_source_bad", 1, 0, auditLoginTime(testLoginAt(0))) || !reflect.DeepEqual(result.Warnings, []string{T("audit.login.local_addrs", reason)}) {
			t.Fatalf("%s: result = %+v", name, result)
		}
	}
	// 자기 주소를 비교하지 못해도 loopback이 없으면 통과다. skip으로 바꾸지 않고 경고로 알린다.
	result := evaluateLocalSource(auditSources{root: t.TempDir()}, testLocalFacts(accepted[1]))
	if result.Status != StatusPass || len(result.Warnings) != 1 {
		t.Fatalf("pass = %+v", result)
	}
}

func TestAuditLocalSourceSkipsWhenTheJournalWasNotRead(t *testing.T) {
	ctx := context.Background()
	journal := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return nil, false, nil }
	for name, test := range map[string]struct {
		sources auditSources
		facts   auditFacts
		want    string
	}{
		"not root": {auditSources{journal: journal}, auditFacts{Now: testLoginAt(0)}, T("audit.skip.needs_root", "journal")},
		"no journalctl": {auditSources{journal: func(context.Context, time.Time) ([]auditJournalEntry, bool, error) {
			return nil, false, errEventsNoJournal
		}}, auditFacts{Root: true, Now: testLoginAt(0)}, T("audit.skip.unreadable", "journal", errEventsNoJournal.Error())},
		"no source": {auditSources{}, auditFacts{Root: true, Now: testLoginAt(0)}, T("audit.skip.no_journal")},
	} {
		collectAuditJournal(ctx, test.sources, &test.facts)
		collectAuditLocalSource(test.sources, &test.facts)
		result := auditLoginLocalSource(test.facts)
		if result.Status != StatusSkip || result.Summary != test.want {
			t.Errorf("%s: result = %+v", name, result)
		}
	}
	results := evaluateAudit(auditFacts{})
	result := auditResultByID(t, results, "login.local_source")
	if result.Status != StatusSkip || result.Summary != T("audit.skip.no_journal") {
		t.Fatalf("empty facts = %+v", result)
	}
}

func TestAuditLocalSourceShowsOpenConnectionsFirstAndCapsLines(t *testing.T) {
	sources, _, facts := testOpenTunnel(t)
	for index := range 12 {
		facts.Accepted = append(facts.Accepted, testClosedAccepted("user"+strconv.Itoa(index), "127.0.0.1", "publickey", 10+index))
	}
	result := evaluateLocalSource(sources, facts)
	if len(result.Evidence) != 2 || result.Evidence[0].Label != T("audit.evidence.open_logins") || result.Evidence[1].Label != T("audit.evidence.closed_logins") {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	closed := strings.Split(result.Evidence[1].Value, "\n")
	if len(closed) != auditLocalSourceShown || closed[len(closed)-1] != "+3" {
		t.Fatalf("closed = %q", closed)
	}
	// 마지막 시각이 늦은 묶음이 먼저다.
	if !strings.HasPrefix(closed[0], "user11  127.0.0.1  publickey  1  "+auditLoginTime(testLoginAt(21))) || !strings.HasPrefix(closed[8], "user3  ") {
		t.Fatalf("closed = %q", closed)
	}
	if result.Summary != T("audit.login.local_source_bad", 13, 1, auditLoginTime(testLoginAt(0))) {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestAuditLocalSourceGroupsClosedLogins(t *testing.T) {
	fake := &testLocalSources{}
	result := evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts(
		testClosedAccepted("root", "127.0.0.1", "publickey", 1),
		testClosedAccepted("root", "127.0.0.1", "publickey", 7),
		testClosedAccepted("root", "127.0.0.1", "password", 3),
		testClosedAccepted("root", "::1", "publickey", 5),
	))
	want := []string{
		"root  127.0.0.1  publickey  2  " + auditLoginTime(testLoginAt(7)),
		"root  ::1  publickey  1  " + auditLoginTime(testLoginAt(5)),
		"root  127.0.0.1  password  1  " + auditLoginTime(testLoginAt(3)),
	}
	if len(result.Evidence) != 1 || !slices.Equal(strings.Split(result.Evidence[0].Value, "\n"), want) {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
}

func TestAuditLocalSourceCapsOpenConnections(t *testing.T) {
	root := t.TempDir()
	var sockets []auditTCPSocket
	facts := testLocalFacts()
	for index := range auditLocalSourceShown + 2 {
		port := 40000 + index
		server, client := uint64(1000+index), uint64(2000+index)
		writeLocalSourceProcs(t, root, testLocalProc{pid: 500 + index, inodes: []uint64{server}}, testLocalProc{pid: 700 + index, comm: "c", inodes: []uint64{client}})
		sockets = append(sockets, testTCP("127.0.0.1:22", "127.0.0.1:"+strconv.Itoa(port), server), testTCP("127.0.0.1:"+strconv.Itoa(port), "127.0.0.1:22", client))
		facts.Accepted = append(facts.Accepted, auditAccepted{PID: 500 + index, User: "root", Host: "127.0.0.1", Method: "publickey", Port: port, Time: testLoginAt(index)})
	}
	facts.Accepted = append(facts.Accepted, testClosedAccepted("deploy", "::1", "password", 30))
	fake := &testLocalSources{sockets: sockets}
	result := evaluateLocalSource(fake.sources(root), facts)
	if len(result.Evidence) != 1 || result.Evidence[0].Label != T("audit.evidence.open_logins") {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	lines := strings.Split(result.Evidence[0].Value, "\n")
	items := slices.DeleteFunc(slices.Clone(lines), func(line string) bool { return strings.HasPrefix(line, auditDetailPrefix) })
	if len(items) != auditLocalSourceShown+1 || lines[len(lines)-1] != "+3" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestAuditLocalSourceSkipsTheTCPSourceWhenEveryLoginEnded(t *testing.T) {
	fake := &testLocalSources{}
	login := testClosedAccepted("root", "127.0.0.1", "publickey", 1)
	login.Port = 54424
	// Port가 없는 로그인은 연결을 찾을 수 없어 Until이 없어도 후보가 아니다.
	noPort := auditAccepted{PID: 9, User: "root", Host: "127.0.0.1", Method: "publickey", Time: testLoginAt(2)}
	evaluateLocalSource(fake.sources(t.TempDir()), testLocalFacts(login, noPort))
	if fake.tcpCalls != 0 {
		t.Fatalf("tcp calls = %d", fake.tcpCalls)
	}
}

func TestAuditLocalSourceReportsALimitedJournal(t *testing.T) {
	facts := testLocalFacts()
	facts.JournalLimited = true
	result := evaluateLocalSource((&testLocalSources{}).sources(t.TempDir()), facts)
	if !reflect.DeepEqual(result.Warnings, []string{T("audit.login.journal_limited", auditJournalMaxEntries)}) {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

// wtmp가 없는 호스트(wtmpdb만 있는 경우 포함)에서도 journal로 판정한다.
func TestCollectAuditFactsJudgesLocalSourceWithoutWtmp(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\n", 0o644)
	var asked time.Time
	entries, err := parseAuditJournal(strings.NewReader(
		testAcceptedLine(testLoginAt(1), "sshd-session", 2546228, "Accepted publickey for root from 127.0.0.1 port 54424 ssh2: RSA SHA256:x") +
			testAcceptedLine(testLoginAt(2), "sshd-session", 2547307, "Accepted publickey for root from 100.83.200.248 port 39612 ssh2: RSA SHA256:x") +
			testClosedLine(testLoginAt(3), 2546228, "root") +
			testClosedLine(testLoginAt(4), 2547307, "root")))
	if err != nil {
		t.Fatal(err)
	}
	fake := &testLocalSources{addrs: []netip.Addr{netip.MustParseAddr("100.83.200.248")}}
	sources := fake.sources(root)
	sources.euid = 0
	sources.listen = func() ([]listenSocket, error) { return nil, nil }
	sources.firewall = func(context.Context) (auditFirewall, error) { return auditFirewall{}, errAuditNoFirewallTool }
	sources.journal = func(_ context.Context, since time.Time) ([]auditJournalEntry, bool, error) {
		asked = since
		return entries, false, nil
	}
	facts := collectAuditFacts(context.Background(), sources)
	if facts.LoginErr == nil || !facts.JournalRead || !facts.Logins.Since.IsZero() || time.Since(asked) < auditJournalWindow-time.Minute {
		t.Fatalf("login err = %v, read = %v, asked = %v", facts.LoginErr, facts.JournalRead, asked)
	}
	results := evaluateAudit(facts)
	result := auditResultByID(t, results, "login.local_source")
	if result.Status != StatusWarn || result.Summary != T("audit.login.local_source_bad", 2, 0, auditLoginTime(testLoginAt(1))) {
		t.Fatalf("result = %+v", result)
	}
	if mismatch := auditResultByID(t, results, "login.mismatch"); mismatch.Status != StatusSkip {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	if fake.tcpCalls != 0 {
		t.Fatalf("tcp calls = %d", fake.tcpCalls)
	}
	// wtmpdb만 있는 호스트도 같다.
	writeAuditFile(t, root, auditWtmpDB, "", 0o644)
	facts = collectAuditFacts(context.Background(), sources)
	if !errors.Is(facts.LoginErr, errAuditLoginSQLite) || auditResultByID(t, evaluateAudit(facts), "login.local_source").Status != StatusWarn {
		t.Fatalf("wtmpdb: login err = %v", facts.LoginErr)
	}
}

// ssh -L 터널로 들어온 로그인의 상대는 터널을 나르는 sshd-session이다. 그 sshd-session의 바깥 연결이 터널을 연 로그인이다.
func TestAuditLocalSourceNamesTheLoginThatOpenedTheTunnel(t *testing.T) {
	tunnelLogin := auditAccepted{PID: 2546100, User: "root", Host: "100.65.168.177", Method: "publickey", Port: 55306, Time: testLoginAt(4)}
	for name, test := range map[string]struct {
		outer    auditTCPSocket
		accepted []auditAccepted
		want     string
	}{
		"journal has the login": {testTCP("100.83.200.248:22", "100.65.168.177:55306", 4000), []auditAccepted{tunnelLogin},
			T("audit.login.peer_forward", "root", "100.65.168.177:55306", "publickey", auditLoginTime(testLoginAt(4)))},
		"login before the journal range": {testTCP("100.83.200.248:22", "100.65.168.177:55306", 4000), nil,
			T("audit.login.peer_forward_unknown", "100.65.168.177:55306")},
		// 같은 출발지·포트의 Accepted가 이미 끝났으면 지금 연결의 로그인이 아니다.
		"matching login ended": {testTCP("100.83.200.248:22", "100.65.168.177:55306", 4000), []auditAccepted{{PID: 1, User: "old", Host: "100.65.168.177", Method: "password", Port: 55306, Time: testLoginAt(1), Until: testLoginAt(2)}},
			T("audit.login.peer_forward_unknown", "100.65.168.177:55306")},
		// sshd 포트가 아닌 연결은 터널의 출발지가 아니다.
		"other port": {testTCP("10.0.0.5:443", "203.0.113.7:51000", 4000), []auditAccepted{tunnelLogin}, ""},
	} {
		sources, fake, facts := testOpenTunnel(t)
		if err := os.Symlink("socket:[4000]", filepath.Join(sources.root, "proc", "2546150", "fd", "9")); err != nil {
			t.Fatal(err)
		}
		fake.sockets = append(fake.sockets, test.outer)
		facts.Accepted = append(facts.Accepted, test.accepted...)
		result := evaluateLocalSource(sources, facts)
		if result.Summary != T("audit.login.local_source_bad", 1, 1, auditLoginTime(testLoginAt(0))) {
			t.Fatalf("%s: summary = %q", name, result.Summary)
		}
		line := result.Evidence[0].Value
		want := "pid 2546150  sshd-session  /usr/lib/openssh/sshd-session  sshd-session: root@notty"
		if test.want != "" {
			want += "\n" + auditNotePrefix + test.want
		}
		if !strings.HasSuffix(line, want) {
			t.Errorf("%s: line = %q, want suffix %q", name, line, want)
		}
	}
}
