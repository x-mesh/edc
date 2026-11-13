package edc

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testJournalLine(at time.Time, fields string) string {
	return `{"__REALTIME_TIMESTAMP":"` + strconv.FormatInt(at.UnixMicro(), 10) + `",` + fields + "}\n"
}

func testLogindLine(at time.Time, messageID, message, session string, leader int) string {
	fields := `"SYSLOG_IDENTIFIER":"systemd-logind","_PID":"1217","MESSAGE":"` + message + `","SESSION_ID":"` + session + `","LEADER":"` + strconv.Itoa(leader) + `"`
	if messageID != "" {
		fields += `,"MESSAGE_ID":"` + messageID + `"`
	}
	return testJournalLine(at, fields)
}

func testAcceptedLine(at time.Time, identifier string, pid int, message string) string {
	return testJournalLine(at, `"SYSLOG_IDENTIFIER":"`+identifier+`","_PID":"`+strconv.Itoa(pid)+`","MESSAGE":"`+message+`"`)
}

func TestParseAuditJournalSkipsBrokenLines(t *testing.T) {
	input := testLogindLine(testLoginAt(1), auditLogindNewSession, "New session 7 of user root.", "7", 4019133) +
		"not json\n" + `{"MESSAGE":"no timestamp"}` + "\n" +
		// 출력할 수 없는 바이트가 든 MESSAGE는 숫자 배열로 온다.
		testJournalLine(testLoginAt(2), `"SYSLOG_IDENTIFIER":"sshd-session","_PID":"9","MESSAGE":[65,99]`)
	entries, err := parseAuditJournal(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []auditJournalEntry{
		{Time: testLoginAt(1), Identifier: "systemd-logind", MessageID: auditLogindNewSession, Message: "New session 7 of user root.", PID: 1217, Leader: 4019133, SessionID: "7"},
		{Time: testLoginAt(2), Identifier: "sshd-session", Message: "Ac", PID: 9},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestApplyAuditJournalEndsUsesLogoutBeforeRemoval(t *testing.T) {
	entries, err := parseAuditJournal(strings.NewReader(
		testLogindLine(testLoginAt(10), auditLogindNewSession, "New session 7 of user deploy.", "7", 100) +
			testLogindLine(testLoginAt(15), "", "Session 7 logged out. Waiting for processes to exit.", "7", 100) +
			// nohup으로 남은 process가 끝나야 Removed가 온다.
			testLogindLine(testLoginAt(90), auditLogindRemovedSession, "Removed session 7.", "7", 100) +
			testLogindLine(testLoginAt(20), auditLogindNewSession, "New session 8 of user ops.", "8", 200) +
			testLogindLine(testLoginAt(25), auditLogindRemovedSession, "Removed session 8.", "8", 200) +
			// PID 300은 다른 시각에 재사용된 PID라 wtmp의 로그인과 짝짓지 않는다.
			testLogindLine(testLoginAt(40), auditLogindNewSession, "New session 9 of user qa.", "9", 300) +
			testLogindLine(testLoginAt(45), "", "Session 9 logged out. Waiting for processes to exit.", "9", 300),
	))
	if err != nil {
		t.Fatal(err)
	}
	records := auditLoginRecords{Sessions: []auditLogin{
		{User: "deploy", PID: 100, SSHD: 100, Start: testLoginAt(10).Add(time.Second)},
		{User: "ops", PID: 200, SSHD: 200, Start: testLoginAt(20)},
		{User: "qa", PID: 300, SSHD: 300, Start: testLoginAt(1)},
		{User: "root", PID: 400, SSHD: 400, Start: testLoginAt(2), End: testLoginAt(3)},
	}}
	if filled := applyAuditJournalEnds(&records, entries); filled != 2 {
		t.Fatalf("filled = %d", filled)
	}
	ends := []time.Time{records.Sessions[0].End, records.Sessions[1].End, records.Sessions[2].End, records.Sessions[3].End}
	want := []time.Time{testLoginAt(15), testLoginAt(25), {}, testLoginAt(3)}
	if !reflect.DeepEqual(ends, want) {
		t.Fatalf("ends = %v", ends)
	}
}

func TestAuditNoTTYAccessCountsAcceptedLoginsMissingFromWtmp(t *testing.T) {
	entries, err := parseAuditJournal(strings.NewReader(
		testAcceptedLine(testLoginAt(1), "sshd-session", 100, "Accepted publickey for root from 100.65.168.177 port 55631 ssh2: RSA SHA256:abc") +
			testAcceptedLine(testLoginAt(2), "sshd-session", 101, "Accepted publickey for root from 100.65.168.177 port 55632 ssh2: RSA SHA256:abc") +
			testAcceptedLine(testLoginAt(3), "sshd", 102, "Accepted password for deploy from 203.0.113.7 port 4000 ssh2") +
			testAcceptedLine(testLoginAt(4), "sshd-session", 103, "pam_unix(sshd:session): session opened for user root(uid=0) by root(uid=0)") +
			testAcceptedLine(testLoginAt(5), "cron", 104, "Accepted publickey for root from 1.2.3.4 port 1 ssh2"),
	))
	if err != nil {
		t.Fatal(err)
	}

	want := []auditNoTTY{
		{User: "deploy", Host: "203.0.113.7", Method: "password", Count: 1},
		{User: "root", Host: "100.65.168.177", Method: "publickey", Count: 1},
	}
	if got := auditNoTTYAccess(map[int]bool{1: true}, auditSSHDAcceptedLogins(entries)); !reflect.DeepEqual(got, want) {
		t.Fatalf("accesses = %+v", got)
	}
}

func TestCollectAuditJournal(t *testing.T) {
	now := testLoginAt(0).Add(30 * 24 * time.Hour)
	var asked time.Time
	journal := func(_ context.Context, since time.Time) ([]auditJournalEntry, bool, error) {
		asked = since
		return nil, true, nil
	}
	facts := auditFacts{Root: true, Now: now, Logins: auditLoginRecords{Since: testLoginAt(0)}}
	collectAuditJournal(context.Background(), auditSources{journal: journal}, &facts)
	if !asked.Equal(now.Add(-auditJournalWindow)) || !facts.JournalLimited {
		t.Fatalf("since = %v, facts = %+v", asked, facts)
	}
	// wtmp가 기간보다 늦게 시작하면 wtmp의 시작부터 읽는다.
	facts = auditFacts{Root: true, Now: now, Logins: auditLoginRecords{Since: now.Add(-time.Hour)}}
	collectAuditJournal(context.Background(), auditSources{journal: journal}, &facts)
	if !asked.Equal(now.Add(-time.Hour)) {
		t.Fatalf("since = %v", asked)
	}
	facts = auditFacts{Now: now}
	collectAuditJournal(context.Background(), auditSources{journal: journal}, &facts)
	if !errors.Is(facts.JournalErr, errAuditNeedsRoot) {
		t.Fatalf("err = %v", facts.JournalErr)
	}
}

func TestAuditLoginMismatchShowsJournalFindings(t *testing.T) {
	facts := testLoginFacts(nil, nil)
	facts.NoTTY = []auditNoTTY{{User: "root", Host: "100.65.168.177", Method: "publickey", Count: 2237}}
	facts.JournalLimited = true
	result := auditLoginMismatch(facts)
	if result.Status != StatusPass || len(result.Evidence) != 1 || result.Evidence[0].Value != "root  100.65.168.177  publickey  2237" {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(result.Warnings, []string{T("audit.login.journal_limited", auditJournalMaxEntries)}) {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	for err, want := range map[error]string{
		errAuditNeedsRoot:              T("audit.login.journal_root"),
		errEventsNoJournal:             T("audit.login.journal_none"),
		errors.New("exit status 1\nx"): T("audit.login.journal_error", "exit status 1 x"),
	} {
		facts := testLoginFacts(nil, nil)
		facts.JournalErr = err
		if warnings := auditLoginMismatch(facts).Warnings; !reflect.DeepEqual(warnings, []string{want}) || strings.HasPrefix(want, "audit.") {
			t.Fatalf("%v: warnings = %v", err, warnings)
		}
	}
}

func testCrossFacts(sessions []auditLogin, journal string) auditFacts {
	entries, _ := parseAuditJournal(strings.NewReader(journal))
	facts := testLoginFacts(sessions, map[int]auditLastlog{0: {Time: testLoginAt(10), Line: "pts/0", Host: "203.0.113.7"}})
	facts.Root, facts.Now = true, testLoginAt(120)
	source := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return entries, false, nil }
	collectAuditJournal(context.Background(), auditSources{journal: source}, &facts)
	return facts
}

func TestAuditLoginMismatchFindsRolledBackLastlog(t *testing.T) {
	facts := testCrossFacts([]auditLogin{
		{User: "root", Line: "pts/0", Host: "203.0.113.7", PID: 10, Start: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "198.51.100.4", PID: 11, Start: testLoginAt(30)},
	}, testAcceptedLine(testLoginAt(10), "sshd-session", 10, "Accepted publickey for root from 203.0.113.7 port 1 ssh2")+
		testAcceptedLine(testLoginAt(30), "sshd-session", 11, "Accepted publickey for root from 198.51.100.4 port 2 ssh2"))
	result := auditLoginMismatch(facts)
	if result.Status != StatusFail || !strings.Contains(result.Evidence[0].Value, "198.51.100.4") {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditLoginMismatchWarnsOnWtmpLoginWithoutSSHDLog(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "203.0.113.7", PID: 10, Start: testLoginAt(10)},
		// sshd 기록이 시작되기 전의 로그인과 원격이 아닌 로그인은 비교하지 않는다.
		{User: "root", Line: "pts/5", Host: "203.0.113.7", PID: 5, Start: testLoginAt(1)},
		{User: "root", Line: "tty1", PID: 6, Start: testLoginAt(40)},
		{User: "deploy", Line: "pts/2", Host: "192.0.2.9", PID: 12, Start: testLoginAt(50)},
	}
	facts := testCrossFacts(sessions, testAcceptedLine(testLoginAt(9), "sshd", 99, "Received disconnect")+
		testAcceptedLine(testLoginAt(10), "sshd-session", 10, "Accepted publickey for root from 203.0.113.7 port 1 ssh2"))
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || result.Evidence[0].Value != "deploy  "+auditLoginTime(testLoginAt(50))+" pts/2 192.0.2.9  pid 12" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectAuditJournalDetectsUnjoinablePIDs(t *testing.T) {
	facts := testCrossFacts([]auditLogin{{User: "root", Line: "pts/0", Host: "203.0.113.7", PID: 10, Start: testLoginAt(10)}},
		testAcceptedLine(testLoginAt(10), "sshd", 777, "Accepted publickey for root from 192.0.2.1 port 1 ssh2"))
	if !facts.Unjoinable || facts.Joined || facts.NoTTY != nil {
		t.Fatalf("facts = %+v", facts)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusPass || !slices.Contains(result.Warnings, T("audit.login.unjoinable")) {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditKeyChangedUsesCtime(t *testing.T) {
	now := testLoginAt(0).Add(60 * 24 * time.Hour)
	facts := auditFacts{Now: now, KeyFiles: []auditFile{{Path: "/root/.ssh/authorized_keys", Keys: true, User: "root", ModTime: testLoginAt(0), ChangeTime: now.Add(-time.Hour)}}}
	result := auditKeyChanged(facts)
	if result.Status != StatusWarn || !strings.Contains(result.Evidence[0].Value, T("audit.ssh.key_mtime_older", auditLoginTime(testLoginAt(0)))) {
		t.Fatalf("result = %+v", result)
	}
}

// OpenSSH 8.9에서 잰 모양이다. Accepted는 sshd 세션 process가, wtmp는 그 자식 shell이 남긴다.
func TestCollectAuditJournalJoinsOpenSSH89ByTime(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "::1", PID: 3750, Start: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "10.0.0.5", PID: 3800, Start: testLoginAt(20)},
		{User: "deploy", Line: "pts/2", Host: "192.0.2.9", PID: 3900, Start: testLoginAt(30)},
	}
	journal := testAcceptedLine(testLoginAt(10), "sshd", 3739, "Accepted publickey for root from ::1 port 46202 ssh2: ED25519 SHA256:x") +
		testLogindLine(testLoginAt(10), auditLogindNewSession, "New session 3 of user root.", "3", 3739) +
		testLogindLine(testLoginAt(13), "", "Session 3 logged out. Waiting for processes to exit.", "3", 3739) +
		// 같은 사용자와 출발지가 같은 초에 두 번 들어오면 어느 것이 pty인지 모른다.
		testAcceptedLine(testLoginAt(20), "sshd", 3790, "Accepted publickey for root from 10.0.0.5 port 1 ssh2") +
		testAcceptedLine(testLoginAt(20).Add(time.Second), "sshd", 3791, "Accepted publickey for root from 10.0.0.5 port 2 ssh2") +
		testAcceptedLine(testLoginAt(40), "sshd", 3950, "Accepted publickey for root from ::1 port 5 ssh2")
	facts := testCrossFacts(sessions, journal)
	facts.Lastlog = map[int]auditLastlog{0: {Time: testLoginAt(20).Add(time.Second), Line: "pts/1", Host: "10.0.0.5"}}
	if !reflect.DeepEqual(facts.NoTTY, []auditNoTTY{{User: "root", Host: "::1", Method: "publickey", Count: 1}}) {
		t.Fatalf("no tty = %+v", facts.NoTTY)
	}
	if !facts.Joined || facts.JoinedByTime != 2 || facts.Unjoinable {
		t.Fatalf("facts = %+v", facts)
	}
	got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD, facts.Logins.Sessions[2].SSHD}
	if !reflect.DeepEqual(got, []int{3739, auditSSHDAmbiguous, 0}) {
		t.Fatalf("sshd = %v", got)
	}
	if !facts.Logins.Sessions[0].End.Equal(testLoginAt(13)) || !facts.Logins.Sessions[1].End.IsZero() {
		t.Fatalf("ends = %v, %v", facts.Logins.Sessions[0].End, facts.Logins.Sessions[1].End)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || !strings.HasPrefix(result.Evidence[0].Value, "deploy  ") || !slices.Contains(result.Warnings, T("audit.login.joined_by_time", 2)) {
		t.Fatalf("result = %+v", result)
	}
}

func testClosedLine(at time.Time, pid int, user string) string {
	return testAcceptedLine(at, "sshd-session", pid, auditSSHDSessionClosed+user)
}

// 이 호스트에서 잰 모양이다. ControlMaster 연결 하나가 몇 시간에 걸쳐 pty를 여러 번 열고, wtmp에는 모두 같은 PID가 남는다.
func TestCollectAuditJournalJoinsPIDsOnlyWhileTheConnectionLives(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/13", Host: "203.0.113.7", PID: 100, Start: testLoginAt(10)},
		{User: "root", Line: "pts/15", Host: "203.0.113.7", PID: 100, Start: testLoginAt(40)},
		{User: "root", Line: "pts/21", Host: "203.0.113.7", PID: 100, Start: testLoginAt(70)},
		// 접속이 끝난 뒤에 같은 PID로 온 로그인은 재사용된 PID다.
		{User: "root", Line: "pts/3", Host: "203.0.113.7", PID: 100, Start: testLoginAt(90)},
		// Accepted보다 앞선 로그인도 그 접속의 것이 아니다.
		{User: "root", Line: "pts/4", Host: "203.0.113.7", PID: 200, Start: testLoginAt(5)},
		// 다음 Accepted가 다른 사용자면 앞 접속은 그때 끝났다.
		{User: "root", Line: "pts/6", Host: "203.0.113.7", PID: 300, Start: testLoginAt(60)},
	}
	journal := testAcceptedLine(testLoginAt(1), "sshd-session", 50, "Accepted publickey for root from 203.0.113.7 port 1 ssh2") +
		testAcceptedLine(testLoginAt(10), "sshd-session", 100, "Accepted publickey for root from 203.0.113.7 port 2 ssh2") +
		testClosedLine(testLoginAt(80), 100, "root") +
		testAcceptedLine(testLoginAt(20), "sshd-session", 200, "Accepted publickey for root from 203.0.113.7 port 3 ssh2") +
		testAcceptedLine(testLoginAt(30), "sshd-session", 300, "Accepted publickey for root from 203.0.113.7 port 4 ssh2") +
		testAcceptedLine(testLoginAt(50), "sshd-session", 300, "Accepted password for deploy from 192.0.2.9 port 5 ssh2")
	facts := testCrossFacts(sessions, journal)
	facts.Lastlog = nil
	var got []int
	for _, session := range facts.Logins.Sessions {
		got = append(got, session.SSHD)
	}
	if !reflect.DeepEqual(got, []int{100, 100, 100, 0, 0, 0}) || facts.JoinedByTime != 0 {
		t.Fatalf("sshd = %v, facts = %+v", got, facts)
	}
	// PID 100의 Accepted는 짝을 찾았으므로 pty 없는 접속이 아니다. 나머지 넷은 짝이 없다.
	want := []auditNoTTY{{User: "root", Host: "203.0.113.7", Method: "publickey", Count: 3}, {User: "deploy", Host: "192.0.2.9", Method: "password", Count: 1}}
	if !reflect.DeepEqual(facts.NoTTY, want) {
		t.Fatalf("no tty = %+v", facts.NoTTY)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || !strings.HasPrefix(result.Summary, T("audit.login.unlogged", 3, auditLoginTime(testLoginAt(1)))) {
		t.Fatalf("result = %+v", result)
	}
}

// OpenSSH를 9.8로 올린 호스트에는 두 모양이 섞인다. PID로 짝짓지 못한 로그인만 시각으로 짝짓는다.
func TestCollectAuditJournalJoinsTheRestByTime(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "203.0.113.7", PID: 3750, Start: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "203.0.113.7", PID: 500, Start: testLoginAt(30)},
	}
	journal := testAcceptedLine(testLoginAt(10), "sshd", 3739, "Accepted publickey for root from 203.0.113.7 port 1 ssh2") +
		testAcceptedLine(testLoginAt(30), "sshd-session", 500, "Accepted publickey for root from 203.0.113.7 port 2 ssh2")
	facts := testCrossFacts(sessions, journal)
	if facts.Logins.Sessions[0].SSHD != 3739 || facts.Logins.Sessions[1].SSHD != 500 || facts.JoinedByTime != 1 || len(facts.NoTTY) != 0 {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestCollectAuditJournalWithoutAcceptedComparesFromTheFirstEntry(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "203.0.113.7", PID: 10, Start: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "203.0.113.7", PID: 11, Start: testLoginAt(2)},
	}
	facts := testCrossFacts(sessions, testLogindLine(testLoginAt(5), auditLogindNewSession, "New session 3 of user root.", "3", 7))
	facts.Lastlog = nil
	if !facts.Joined || facts.Unjoinable || !facts.AcceptedSince.Equal(testLoginAt(5)) {
		t.Fatalf("facts = %+v", facts)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || !strings.Contains(result.Evidence[0].Value, "pts/0") || strings.Contains(result.Evidence[0].Value, "pts/1") {
		t.Fatalf("result = %+v", result)
	}
	// journal이 비었으면 조회를 시작한 시각부터 비교한다. 여기서는 기간보다 늦은 wtmp의 시작이다.
	facts = testCrossFacts(sessions, "")
	if !facts.Joined || !facts.AcceptedSince.Equal(testLoginAt(0)) {
		t.Fatalf("facts = %+v", facts)
	}
}

// 이 호스트에서 잰 모양이다. 범위 앞에서 Accepted를 받은 연결이 범위 안에서 pty를 열면, 그 PID의 다른 기록만 범위 안에 있다.
func TestCollectAuditJournalJoinsConnectionsOpenedBeforeTheRange(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/13", Host: "203.0.113.7", PID: 100, Start: testLoginAt(30)},
		// 범위 앞의 연결이 끝난 뒤에 같은 PID로 온 로그인은 짝짓지 않는다.
		{User: "root", Line: "pts/14", Host: "203.0.113.7", PID: 200, Start: testLoginAt(50)},
	}
	journal := testAcceptedLine(testLoginAt(10), "sshd-session", 50, "Accepted publickey for root from 203.0.113.7 port 1 ssh2") +
		testAcceptedLine(testLoginAt(20), "sshd-session", 100, "syslogin_perform_logout: logout() returned an error") +
		testClosedLine(testLoginAt(80), 100, "root") +
		testClosedLine(testLoginAt(40), 200, "root")
	facts := testCrossFacts(sessions, journal)
	if got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD}; !reflect.DeepEqual(got, []int{auditSSHDAmbiguous, 0}) {
		t.Fatalf("sshd = %v", got)
	}
	if !facts.Logins.Sessions[0].End.IsZero() || facts.JoinedByTime != 0 {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestJoinAuditLoginsByPIDChecksTheSource(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "198.51.100.4", PID: 100, Start: testLoginAt(30)},
		// UseDNS yes면 wtmp에 이름이 남는다. 주소와 비교할 수 없어 PID와 사용자로 짝짓는다.
		{User: "root", Line: "pts/1", Host: "admin.example.net", PID: 200, Start: testLoginAt(30)},
		{User: "root", Line: "pts/2", Host: "::ffff:203.0.113.7", PID: 300, Start: testLoginAt(30)},
	}
	journal := testAcceptedLine(testLoginAt(10), "sshd-session", 100, "Accepted publickey for root from 203.0.113.7 port 1 ssh2") +
		testAcceptedLine(testLoginAt(10), "sshd-session", 200, "Accepted publickey for root from 203.0.113.7 port 2 ssh2") +
		testAcceptedLine(testLoginAt(10), "sshd-session", 300, "Accepted publickey for root from 203.0.113.7 port 3 ssh2")
	facts := testCrossFacts(sessions, journal)
	if got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD, facts.Logins.Sessions[2].SSHD}; !reflect.DeepEqual(got, []int{0, 200, 300}) {
		t.Fatalf("sshd = %v", got)
	}
}

func TestCollectAuditJournalChecksTheUserOfAConnectionOpenedBeforeTheRange(t *testing.T) {
	sessions := []auditLogin{{User: "deploy", Line: "pts/13", Host: "203.0.113.7", PID: 100, Start: testLoginAt(30)}}
	journal := testAcceptedLine(testLoginAt(10), "sshd-session", 50, "Accepted publickey for root from 203.0.113.7 port 1 ssh2") +
		testClosedLine(testLoginAt(80), 100, "root")
	if facts := testCrossFacts(sessions, journal); facts.Logins.Sessions[0].SSHD != 0 {
		t.Fatalf("sshd = %d", facts.Logins.Sessions[0].SSHD)
	}
}

// 같은 사용자가 같은 곳에서 같은 초에 두 번 들어오면 두 로그인의 후보가 같다. 앞 로그인이 후보를 모두 가져가도 뒤 로그인은 sshd 기록이 있다.
func TestJoinAuditLoginsByTimeSharesCandidates(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "10.0.0.5", PID: 3800, Start: testLoginAt(20)},
		{User: "root", Line: "pts/1", Host: "10.0.0.5", PID: 3801, Start: testLoginAt(20).Add(time.Second)},
		{User: "root", Line: "pts/2", Host: "10.0.0.6", PID: 3900, Start: testLoginAt(40)},
		{User: "root", Line: "pts/3", Host: "10.0.0.6", PID: 3901, Start: testLoginAt(40)},
	}
	journal := testAcceptedLine(testLoginAt(20), "sshd", 3790, "Accepted publickey for root from 10.0.0.5 port 1 ssh2") +
		testAcceptedLine(testLoginAt(20).Add(time.Second), "sshd", 3791, "Accepted publickey for root from 10.0.0.5 port 2 ssh2") +
		testAcceptedLine(testLoginAt(40), "sshd", 3890, "Accepted publickey for root from 10.0.0.6 port 3 ssh2")
	facts := testCrossFacts(sessions, journal)
	var got []int
	for _, session := range facts.Logins.Sessions {
		got = append(got, session.SSHD)
	}
	if !reflect.DeepEqual(got, []int{auditSSHDAmbiguous, auditSSHDAmbiguous, 3890, auditSSHDAmbiguous}) || facts.JoinedByTime != 4 {
		t.Fatalf("sshd = %v, by time = %d", got, facts.JoinedByTime)
	}
}

func TestAuditSSHDAcceptedLoginsReadsThePort(t *testing.T) {
	entries, err := parseAuditJournal(strings.NewReader(
		testAcceptedLine(testLoginAt(1), "sshd-session", 10, "Accepted publickey for root from 127.0.0.1 port 54424 ssh2: RSA SHA256:abc") +
			testAcceptedLine(testLoginAt(2), "sshd-session", 11, "Accepted publickey for root from 127.0.0.1") +
			testAcceptedLine(testLoginAt(3), "sshd-session", 12, "Accepted publickey for root from 127.0.0.1 port abc ssh2") +
			testAcceptedLine(testLoginAt(4), "sshd-session", 13, "Accepted publickey for root from 127.0.0.1 port 70000 ssh2")))
	if err != nil {
		t.Fatal(err)
	}
	var ports []int
	for _, login := range auditSSHDAcceptedLogins(entries) {
		ports = append(ports, login.Port)
	}
	// port가 없거나 숫자가 아니어도 그 줄은 Accepted로 센다.
	if !reflect.DeepEqual(ports, []int{54424, 0, 0, 0}) {
		t.Fatalf("ports = %v", ports)
	}
}

func TestCollectAuditJournalReadsWithoutWtmp(t *testing.T) {
	now := testLoginAt(0).Add(30 * 24 * time.Hour)
	entries, err := parseAuditJournal(strings.NewReader(
		testAcceptedLine(testLoginAt(10), "sshd-session", 10, "Accepted publickey for root from 127.0.0.1 port 54424 ssh2")))
	if err != nil {
		t.Fatal(err)
	}
	var asked time.Time
	source := func(_ context.Context, since time.Time) ([]auditJournalEntry, bool, error) {
		asked = since
		return entries, false, nil
	}
	// wtmp를 읽지 못하면 Logins.Since가 있어도 쓰지 않고 최근 7일을 읽는다.
	facts := auditFacts{Root: true, Now: now, LoginErr: errors.New("no wtmp"), Logins: auditLoginRecords{Since: now.Add(-time.Hour)}}
	collectAuditJournal(context.Background(), auditSources{journal: source}, &facts)
	if !asked.Equal(now.Add(-auditJournalWindow)) || !facts.JournalRead || facts.JournalErr != nil || len(facts.Accepted) != 1 || facts.Accepted[0].Port != 54424 {
		t.Fatalf("since = %v, facts = %+v", asked, facts)
	}
	if !facts.JournalSince.Equal(testLoginAt(10)) {
		t.Fatalf("journal since = %v", facts.JournalSince)
	}
	// wtmp 짝짓기는 하지 않는다.
	if facts.Joined || facts.NoTTY != nil || !facts.AcceptedSince.IsZero() {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestCollectAuditJournalMarksWhetherItRead(t *testing.T) {
	source := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return nil, false, nil }
	facts := auditFacts{Now: testLoginAt(0), LoginErr: errors.New("no wtmp")}
	collectAuditJournal(context.Background(), auditSources{journal: source}, &facts)
	if !errors.Is(facts.JournalErr, errAuditNeedsRoot) || facts.JournalRead {
		t.Fatalf("not root: %+v", facts)
	}
	facts = auditFacts{Root: true, Now: testLoginAt(0)}
	collectAuditJournal(context.Background(), auditSources{}, &facts)
	if facts.JournalErr != nil || facts.JournalRead {
		t.Fatalf("no source: %+v", facts)
	}
	facts = auditFacts{Root: true, Now: testLoginAt(0)}
	failing := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) {
		return nil, false, errEventsNoJournal
	}
	collectAuditJournal(context.Background(), auditSources{journal: failing}, &facts)
	if !errors.Is(facts.JournalErr, errEventsNoJournal) || facts.JournalRead {
		t.Fatalf("failed: %+v", facts)
	}
}

// OpenSSH 9.6에서 연결 하나에 터미널을 하나 더 열면(ControlMaster) 새 Accepted가 없고 wtmp에는 shell의 PID가 남는다.
// okrr-intranet-slackbot-1에서 두 shell의 부모가 모두 09:46에 Accepted를 남긴 sshd 1312953이었다.
func TestCollectAuditJournalJoinsAMultiplexedTerminalByItsParent(t *testing.T) {
	root := t.TempDir()
	stat := func(pid, parent int) {
		writeAuditFile(t, root, "/proc/"+strconv.Itoa(pid)+"/stat", strconv.Itoa(pid)+" (bash) S "+strconv.Itoa(parent)+" 1 1 0 -1 4194560 1 0 0 0 5 6 0 0 20 0 1 0 100 1000 50 18446744073709551615\n", 0o644)
	}
	stat(1313073, 1312953)
	stat(1313221, 1312953)
	stat(1313300, 1312953)
	stat(1312953, 1261211)
	stat(1261211, 1)
	sessions := []auditLogin{
		{User: "root", Line: "pts/0", Host: "100.65.168.177", PID: 1313073, Start: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "100.65.168.177", PID: 1313221, Start: testLoginAt(24)},
		// 끝난 로그인은 shell PID가 재사용됐을 수 있어 부모로 짝짓지 않는다. 같은 사용자·출발지의 연결이 열려 있던 때라 그 연결의 터미널로 본다.
		{User: "root", Line: "pts/2", Host: "100.65.168.177", PID: 1313300, Start: testLoginAt(30), End: testLoginAt(31)},
		// 다른 사용자는 같은 sshd 아래에 있어도 짝짓지 않는다.
		{User: "deploy", Line: "pts/3", Host: "100.65.168.177", PID: 1313221, Start: testLoginAt(40)},
	}
	entries, err := parseAuditJournal(strings.NewReader(testAcceptedLine(testLoginAt(10), "sshd", 1312953, "Accepted publickey for root from 100.65.168.177 port 65380 ssh2: RSA SHA256:x")))
	if err != nil {
		t.Fatal(err)
	}
	// sshd는 터미널을 열 때마다 lastlog를 고치므로 마지막 root 터미널(pts/2)이 남는다.
	facts := testLoginFacts(sessions, map[int]auditLastlog{0: {Time: testLoginAt(30), Line: "pts/2", Host: "100.65.168.177"}})
	facts.Root, facts.Now = true, testLoginAt(120)
	source := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return entries, false, nil }
	collectAuditJournal(context.Background(), auditSources{root: root, journal: source}, &facts)
	got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD, facts.Logins.Sessions[2].SSHD, facts.Logins.Sessions[3].SSHD}
	if !reflect.DeepEqual(got, []int{1312953, 1312953, auditSSHDAmbiguous, 0}) || facts.JoinedByTime != 1 || facts.JoinedByConnection != 1 || len(facts.NoTTY) != 0 {
		t.Fatalf("sshd = %v, by time = %d, by connection = %d, no tty = %+v", got, facts.JoinedByTime, facts.JoinedByConnection, facts.NoTTY)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || strings.Contains(result.Evidence[0].Value, "pts/1") || strings.Contains(result.Evidence[0].Value, "pts/2") ||
		!strings.Contains(result.Evidence[0].Value, "deploy") || !slices.Contains(result.Warnings, T("audit.login.joined_by_connection", 1)) {
		t.Fatalf("result = %+v", result)
	}
}

// okrr-intranet-1(OpenSSH 8.9)에서 Accepted 하나(10:50) 위에 터미널 셋이 열렸다. 11:08 터미널은 닫혔고 11:37 터미널은 열려 있다.
// 0.86.1은 아무것도 짝짓지 못해 통과했고, 부모 짝짓기만 넣으면 닫힌 11:08 터미널이 Accepted 없는 로그인으로 보였다.
// journal 범위 안에서 OpenSSH를 9.8 이상으로 올렸다. 새 로그인은 PID로 맞고, 올리기 전 연결 위에서 닫힌 터미널은 연결로 짝짓는다.
// 옛 연결이 끝난 뒤의 로그인에 Accepted가 없으면 그대로 남는다.
func TestCollectAuditJournalJoinsOldTerminalsAfterAnUpgrade(t *testing.T) {
	sessions := []auditLogin{
		{User: "root", Line: "pts/1", Host: "100.65.168.177", PID: 3231170, Start: testLoginAt(18), End: testLoginAt(40)},
		{User: "root", Line: "pts/0", Host: "100.65.168.177", PID: 4000000, Start: testLoginAt(60)},
		{User: "root", Line: "pts/2", Host: "100.65.168.177", PID: 4100000, Start: testLoginAt(70)},
	}
	lines := testAcceptedLine(testLoginAt(0), "sshd", 3220182, "Accepted publickey for root from 100.65.168.177 port 54907 ssh2: RSA SHA256:x") +
		testClosedLine(testLoginAt(50), 3220182, "root") +
		testAcceptedLine(testLoginAt(60), "sshd-session", 4000000, "Accepted publickey for root from 100.65.168.177 port 54990 ssh2: RSA SHA256:x")
	entries, err := parseAuditJournal(strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	facts := testLoginFacts(sessions, map[int]auditLastlog{0: {Time: testLoginAt(70), Line: "pts/2", Host: "100.65.168.177"}})
	facts.Root, facts.Now = true, testLoginAt(120)
	source := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return entries, false, nil }
	collectAuditJournal(context.Background(), auditSources{root: t.TempDir(), journal: source}, &facts)
	got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD, facts.Logins.Sessions[2].SSHD}
	if !reflect.DeepEqual(got, []int{auditSSHDAmbiguous, 4000000, 0}) || facts.JoinedByConnection != 1 {
		t.Fatalf("sshd = %v, by connection = %d", got, facts.JoinedByConnection)
	}
}

func TestCollectAuditJournalJoinsClosedMultiplexedTerminals(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/proc/3240000/stat", "3240000 (bash) S 3220182 1 1 0 -1 4194560 1 0 0 0 5 6 0 0 20 0 1 0 100 1000 50 18446744073709551615\n", 0o644)
	sessions := []auditLogin{
		{User: "root", Line: "pts/1", Host: "100.65.168.177", PID: 3231170, Start: testLoginAt(18), End: testLoginAt(40)},
		{User: "root", Line: "pts/0", Host: "100.65.168.177", PID: 3240000, Start: testLoginAt(47)},
		// 다른 출발지의 로그인은 그 연결의 터미널이 아니다.
		{User: "root", Line: "pts/2", Host: "198.51.100.4", PID: 3250000, Start: testLoginAt(50)},
	}
	entries, err := parseAuditJournal(strings.NewReader(testAcceptedLine(testLoginAt(0), "sshd", 3220182, "Accepted publickey for root from 100.65.168.177 port 54907 ssh2: RSA SHA256:x")))
	if err != nil {
		t.Fatal(err)
	}
	facts := testLoginFacts(sessions, map[int]auditLastlog{0: {Time: testLoginAt(47), Line: "pts/0", Host: "100.65.168.177"}})
	facts.Root, facts.Now = true, testLoginAt(120)
	source := func(context.Context, time.Time) ([]auditJournalEntry, bool, error) { return entries, false, nil }
	collectAuditJournal(context.Background(), auditSources{root: root, journal: source}, &facts)
	got := []int{facts.Logins.Sessions[0].SSHD, facts.Logins.Sessions[1].SSHD, facts.Logins.Sessions[2].SSHD}
	if !reflect.DeepEqual(got, []int{auditSSHDAmbiguous, 3220182, 0}) || facts.JoinedByConnection != 1 || facts.Unjoinable {
		t.Fatalf("sshd = %v, by connection = %d, unjoinable = %v", got, facts.JoinedByConnection, facts.Unjoinable)
	}
	result := auditLoginMismatch(facts)
	if result.Status != StatusWarn || !strings.Contains(result.Evidence[0].Value, "198.51.100.4") || strings.Contains(result.Evidence[0].Value, "pts/1") {
		t.Fatalf("result = %+v", result)
	}
}
