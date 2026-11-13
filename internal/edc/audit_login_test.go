package edc

import (
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testUtmp struct {
	kind             int16
	line, user, host string
	at               time.Time
}

func encodeTestUtmp(records ...testUtmp) []byte {
	var data []byte
	for _, record := range records {
		raw := make([]byte, auditLayout.utmpSize)
		binary.LittleEndian.PutUint16(raw[0:2], uint16(record.kind))
		copy(raw[8:40], record.line)
		copy(raw[44:76], record.user)
		copy(raw[76:332], record.host)
		putTestRecordTime(raw[auditLayout.utmpTime:], record.at)
		data = append(data, raw...)
	}
	return data
}

func writeTestLastlog(t *testing.T, root string, entries map[int]auditLastlog) {
	t.Helper()
	full := filepath.Join(root, auditLastlogFile)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(full)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for uid, entry := range entries {
		raw := make([]byte, auditLayout.lastlogSize)
		putTestRecordTime(raw, entry.Time)
		line := auditLayout.timeBytes
		copy(raw[line:line+32], entry.Line)
		copy(raw[line+32:line+288], entry.Host)
		if _, err := file.WriteAt(raw, int64(uid)*int64(auditLayout.lastlogSize)); err != nil {
			t.Fatal(err)
		}
	}
}

func putTestRecordTime(data []byte, at time.Time) {
	if auditLayout.timeBytes == 8 {
		binary.LittleEndian.PutUint64(data, uint64(at.Unix()))
		return
	}
	binary.LittleEndian.PutUint32(data, uint32(at.Unix()))
}

var testLoginBase = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)

func testLoginAt(minutes int) time.Time {
	return testLoginBase.Add(time.Duration(minutes) * time.Minute)
}

func TestReadAuditLoginsPairsSessionsAcrossRotatedFiles(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/var/log/wtmp.1", string(encodeTestUtmp(
		testUtmp{kind: auditUtmpTypeBoot, line: "~", user: "reboot", at: testLoginAt(0)},
		testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "deploy", host: "203.0.113.7", at: testLoginAt(1)},
		testUtmp{kind: auditUtmpTypeUser, line: "pts/1", user: "root", host: "198.51.100.4", at: testLoginAt(2)},
		testUtmp{kind: auditUtmpTypeDead, line: "pts/1", at: testLoginAt(3)},
	)), 0o664)
	// deploy의 pts/0 세션은 회전 뒤 파일에서 끝난다.
	writeAuditFile(t, root, "/var/log/wtmp", string(encodeTestUtmp(
		testUtmp{kind: auditUtmpTypeDead, line: "pts/0", at: testLoginAt(10)},
		testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "ops", host: "10.0.0.5", at: testLoginAt(20)},
	)), 0o664)
	records, err := readAuditLogins(auditSources{root: root})
	if err != nil {
		t.Fatal(err)
	}
	want := []auditLogin{
		{User: "deploy", Line: "pts/0", Host: "203.0.113.7", Start: testLoginAt(1), End: testLoginAt(10)},
		{User: "root", Line: "pts/1", Host: "198.51.100.4", Start: testLoginAt(2), End: testLoginAt(3)},
		{User: "ops", Line: "pts/0", Host: "10.0.0.5", Start: testLoginAt(20)},
	}
	if !reflect.DeepEqual(records.Sessions, want) {
		t.Fatalf("sessions = %+v", records.Sessions)
	}
	if !records.Since.Equal(testLoginAt(0)) {
		t.Fatalf("since = %v", records.Since)
	}
}

func TestReadAuditLoginsBootLeavesOpenSessionsWithoutEnd(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/var/log/wtmp", string(encodeTestUtmp(
		testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "deploy", at: testLoginAt(1)},
		testUtmp{kind: auditUtmpTypeBoot, line: "~", user: "reboot", at: testLoginAt(5)},
		// 재부팅 뒤 같은 터미널의 DEAD_PROCESS는 재부팅 전 세션을 닫지 않는다.
		testUtmp{kind: auditUtmpTypeDead, line: "pts/0", at: testLoginAt(6)},
	)), 0o664)
	records, err := readAuditLogins(auditSources{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(records.Sessions) != 1 || !records.Sessions[0].End.IsZero() {
		t.Fatalf("sessions = %+v", records.Sessions)
	}
}

func TestReadAuditLoginsReportsZeroedTruncatedAndCompressed(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/var/log/wtmp.1", "\x1f\x8b\x08\x00compressed", 0o664)
	data := encodeTestUtmp(testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "deploy", at: testLoginAt(1)})
	data = append(data, make([]byte, auditLayout.utmpSize)...)
	data = append(data, encodeTestUtmp(testUtmp{kind: auditUtmpTypeDead, line: "pts/0", at: testLoginAt(2)})...)
	data = append(data, 1, 2, 3)
	writeAuditFile(t, root, "/var/log/wtmp", string(data), 0o664)
	records, err := readAuditLogins(auditSources{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if records.Zeroed != 1 {
		t.Fatalf("zeroed = %d", records.Zeroed)
	}
	if !reflect.DeepEqual(records.Truncated, []string{"/var/log/wtmp"}) || !reflect.DeepEqual(records.Compressed, []string{"/var/log/wtmp.1"}) {
		t.Fatalf("truncated = %v, compressed = %v", records.Truncated, records.Compressed)
	}
	// 압축 파일은 읽지 않으므로 판정 범위는 wtmp의 첫 레코드부터다.
	if !records.Since.Equal(testLoginAt(1)) || len(records.Sessions) != 1 || !records.Sessions[0].End.Equal(testLoginAt(2)) {
		t.Fatalf("since = %v, sessions = %+v", records.Since, records.Sessions)
	}
}

func TestReadAuditLoginsLimitKeepsRecordBoundary(t *testing.T) {
	previous := auditLoginMaxBytes
	auditLoginMaxBytes = int64(2*auditLayout.utmpSize + 100)
	t.Cleanup(func() { auditLoginMaxBytes = previous })
	root := t.TempDir()
	data := encodeTestUtmp(
		testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "old", at: testLoginAt(1)},
		testUtmp{kind: auditUtmpTypeUser, line: "pts/1", user: "mid", at: testLoginAt(2)},
		testUtmp{kind: auditUtmpTypeUser, line: "pts/2", user: "new", at: testLoginAt(3)},
	)
	writeAuditFile(t, root, "/var/log/wtmp", string(append(data, 9)), 0o664)
	records, err := readAuditLogins(auditSources{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(records.Limited, []string{"/var/log/wtmp"}) || len(records.Sessions) != 2 || records.Sessions[0].User != "mid" || !records.Since.Equal(testLoginAt(2)) {
		t.Fatalf("limited = %v, since = %v, sessions = %+v", records.Limited, records.Since, records.Sessions)
	}
}

func TestReadAuditLoginsWithoutBinaryFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := readAuditLogins(auditSources{root: root}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	writeAuditFile(t, root, auditWtmpDB, "SQLite format 3\x00", 0o644)
	if _, err := readAuditLogins(auditSources{root: root}); !errors.Is(err, errAuditLoginSQLite) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAuditLastlogReadsOnlyRequestedUIDs(t *testing.T) {
	root := t.TempDir()
	writeTestLastlog(t, root, map[int]auditLastlog{
		0:    {Time: testLoginAt(5), Line: "pts/13", Host: "100.65.168.177"},
		1000: {Time: testLoginAt(9), Line: "pts/1", Host: "10.255.10.33"},
	})
	entries, err := readAuditLastlog(auditSources{root: root}, []int{0, 1, 1000, 65534})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]auditLastlog{
		0:    {Time: testLoginAt(5), Line: "pts/13", Host: "100.65.168.177"},
		1000: {Time: testLoginAt(9), Line: "pts/1", Host: "10.255.10.33"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestReadAuditLastlogSQLiteOnly(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, auditLastlogDB, "SQLite format 3\x00", 0o644)
	if _, err := readAuditLastlog(auditSources{root: root}, []int{0}); !errors.Is(err, errAuditLoginSQLite) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAuditPasswdKeepsShell(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\nsvc:x:998:998::/var/lib/svc:/usr/sbin/nologin\n", 0o644)
	entries, err := readAuditPasswd(filepath.Join(root, "/etc/passwd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Shell != "/bin/bash" || entries[1].Shell != "/usr/sbin/nologin" {
		t.Fatalf("entries = %+v", entries)
	}
}

func testLoginFacts(sessions []auditLogin, lastlog map[int]auditLastlog) auditFacts {
	return auditFacts{
		Passwd: []auditPasswd{
			{Name: "root", UID: 0, Home: "/root", Shell: "/bin/bash"},
			{Name: "deploy", UID: 1000, Home: "/home/deploy", Shell: "/bin/bash"},
			{Name: "ops", UID: 1001, Home: "/home/ops", Shell: "/bin/zsh"},
		},
		Logins:  auditLoginRecords{Sessions: sessions, Since: testLoginAt(0)},
		Lastlog: lastlog,
	}
}

func TestAuditLoginMismatchAgreesWithinClockSkew(t *testing.T) {
	facts := testLoginFacts(
		[]auditLogin{{User: "deploy", Line: "pts/0", Host: "203.0.113.7", Start: testLoginAt(10).Add(time.Second)}},
		map[int]auditLastlog{
			1000: {Time: testLoginAt(10), Line: "pts/0", Host: "203.0.113.7"},
			// wtmp가 시작하기 전의 로그인은 비교하지 않는다.
			1001: {Time: testLoginAt(-60), Line: "pts/3", Host: "10.0.0.5"},
		})
	result := auditLoginMismatch(facts)
	if result.Status != StatusPass || !strings.Contains(result.Summary, auditLoginTime(testLoginAt(0))) {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "1") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestAuditLoginMismatchFailsWhenWtmpLacksTheLogin(t *testing.T) {
	for name, session := range map[string]auditLogin{
		"no session":     {User: "root", Line: "pts/9", Start: testLoginAt(30)},
		"other terminal": {User: "deploy", Line: "pts/1", Start: testLoginAt(10)},
		"two seconds":    {User: "deploy", Line: "pts/0", Start: testLoginAt(10).Add(2 * time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			facts := testLoginFacts([]auditLogin{session}, map[int]auditLastlog{1000: {Time: testLoginAt(10), Line: "pts/0", Host: "203.0.113.7"}})
			result := auditLoginMismatch(facts)
			if result.Status != StatusFail || result.Error == nil || len(result.Evidence) != 1 || !strings.Contains(result.Evidence[0].Value, "deploy  lastlog") {
				t.Fatalf("result = %+v", result)
			}
			if want := "journalctl -t sshd -t sshd-session --since '" + auditLoginTime(testLoginAt(9)) + "'"; result.Next != want {
				t.Fatalf("next = %q, want %q", result.Next, want)
			}
		})
	}
}

func TestAuditLoginMismatchMatchesEveryNameOfASharedUID(t *testing.T) {
	facts := testLoginFacts([]auditLogin{{User: "toor", Line: "pts/0", Start: testLoginAt(10)}}, map[int]auditLastlog{0: {Time: testLoginAt(10), Line: "pts/0"}})
	facts.Passwd = append(facts.Passwd, auditPasswd{Name: "toor", UID: 0, Home: "/root"})
	if result := auditLoginMismatch(facts); result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditLoginMismatchWarnings(t *testing.T) {
	zeroed := testLoginFacts(nil, nil)
	zeroed.Logins.Zeroed = 2
	zeroed.Logins.Compressed = []string{"/var/log/wtmp.1"}
	if result := auditLoginMismatch(zeroed); result.Status != StatusWarn || len(result.Warnings) != 1 {
		t.Fatalf("zeroed = %+v", result)
	}
	empty := testLoginFacts(nil, map[int]auditLastlog{1000: {Time: testLoginAt(10), Line: "pts/0"}})
	empty.Logins = auditLoginRecords{}
	if result := auditLoginMismatch(empty); result.Status != StatusWarn {
		t.Fatalf("empty = %+v", result)
	}
}

func TestAuditLoginMismatchSkipsSQLiteOnlyHosts(t *testing.T) {
	facts := testLoginFacts(nil, nil)
	facts.LoginErr = errAuditLoginSQLite
	if result := auditLoginMismatch(facts); result.Status != StatusSkip || result.Summary != T("audit.skip.login_sqlite") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCollectAuditFactsReadsLoginRecords(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\n", 0o644)
	writeAuditFile(t, root, "/var/log/wtmp", string(encodeTestUtmp(testUtmp{kind: auditUtmpTypeUser, line: "pts/0", user: "root", host: "198.51.100.4", at: testLoginAt(5)})), 0o664)
	writeTestLastlog(t, root, map[int]auditLastlog{0: {Time: testLoginAt(5), Line: "pts/0", Host: "198.51.100.4"}})
	sources := auditSources{
		root:     root,
		listen:   func() ([]listenSocket, error) { return nil, nil },
		firewall: func(context.Context) (auditFirewall, error) { return auditFirewall{}, errAuditNoFirewallTool },
		owner:    func(fs.FileInfo) (uint32, bool) { return 0, false },
	}
	if result := auditResultByID(t, evaluateAudit(collectAuditFacts(context.Background(), sources)), "login.mismatch"); result.Status != StatusPass {
		t.Fatalf("result = %+v", result)
	}
}

func TestAuditKeyChangedShowsTheLoginBeforeTheChange(t *testing.T) {
	facts := auditFacts{
		Now: testLoginAt(0).Add(48 * time.Hour),
		KeyFiles: []auditFile{
			{Path: "/home/deploy/.ssh", User: "deploy", ModTime: testLoginAt(20)},
			{Path: "/home/deploy/.ssh/authorized_keys", Keys: true, User: "deploy", ModTime: testLoginAt(20)},
			{Path: "/root/.ssh/authorized_keys", Keys: true, User: "root", ModTime: testLoginAt(0).Add(-10 * 24 * time.Hour)},
		},
		Logins: auditLoginRecords{Since: testLoginAt(0), Sessions: []auditLogin{
			{User: "ops", Line: "pts/1", Host: "10.0.0.5", Start: testLoginAt(1)},
			{User: "root", Line: "pts/0", Host: "203.0.113.7", Start: testLoginAt(18)},
			{User: "deploy", Line: "pts/2", Host: "10.0.0.9", Start: testLoginAt(25)},
		}},
	}
	result := auditKeyChanged(facts)
	if result.Status != StatusWarn || result.Next != "ssh-keygen -lf /home/deploy/.ssh/authorized_keys" {
		t.Fatalf("result = %+v", result)
	}
	want := "deploy  /home/deploy/.ssh/authorized_keys\n" + auditDetailPrefix + T("audit.ssh.key_changed_at", auditLoginTime(testLoginAt(20))) + "\n" +
		auditDetailPrefix + T("audit.ssh.key_login", "root", auditLoginTime(testLoginAt(18)), "pts/0", "203.0.113.7")
	if result.Evidence[0].Value != want {
		t.Fatalf("evidence = %q, want %q", result.Evidence[0].Value, want)
	}
}

func TestAuditKeyChangedWithoutEarlierLogin(t *testing.T) {
	facts := auditFacts{
		Now:      testLoginAt(60),
		KeyFiles: []auditFile{{Path: "/root/.ssh/authorized_keys", Keys: true, User: "root", ModTime: testLoginAt(5)}},
		Logins:   auditLoginRecords{Since: testLoginAt(10), Sessions: []auditLogin{{User: "root", Start: testLoginAt(10)}}},
	}
	if result := auditKeyChanged(facts); result.Status != StatusWarn || !strings.HasSuffix(result.Evidence[0].Value, T("audit.ssh.key_no_login")) {
		t.Fatalf("result = %+v", result)
	}
	facts.Now = testLoginAt(0).Add(30 * 24 * time.Hour)
	if result := auditKeyChanged(facts); result.Status != StatusPass {
		t.Fatalf("old change = %+v", result)
	}
}

func TestCollectAuditKeyFilesFollowsAuthorizedKeysFile(t *testing.T) {
	root := t.TempDir()
	passwd := []auditPasswd{{Name: "root", UID: 0, Home: "/root"}, {Name: "deploy", UID: 1000, Home: "/home/deploy"}}
	writeAuditFile(t, root, "/root/.ssh/authorized_keys2", "ssh-ed25519 AAAA\n", 0o600)
	writeAuditFile(t, root, "/etc/ssh/keys/deploy", "ssh-ed25519 BBBB\n", 0o644)
	owner := func(fs.FileInfo) (uint32, bool) { return 0, false }
	var got []string
	files, _ := collectAuditKeyFiles(auditSources{root: root, owner: owner}, passwd, auditKeyFilePatterns(sshdConfig{}))
	for _, file := range files {
		got = append(got, file.Path+":"+strconv.FormatBool(file.Keys))
	}
	if !reflect.DeepEqual(got, []string{"/root/.ssh:false", "/root/.ssh/authorized_keys2:true"}) {
		t.Fatalf("default = %v", got)
	}
	config := sshdConfig{raw: map[string]string{"authorizedkeysfile": "/etc/ssh/keys/%u .ssh/authorized_keys"}}
	got = nil
	files, _ = collectAuditKeyFiles(auditSources{root: root, owner: owner}, passwd, auditKeyFilePatterns(config))
	for _, file := range files {
		got = append(got, file.Path+":"+strconv.FormatBool(file.Keys))
	}
	if !reflect.DeepEqual(got, []string{"/root/.ssh:false", "/etc/ssh/keys/deploy:true"}) {
		t.Fatalf("configured = %v", got)
	}
	if patterns := auditKeyFilePatterns(sshdConfig{raw: map[string]string{"authorizedkeysfile": "none"}}); patterns != nil {
		t.Fatalf("none = %v", patterns)
	}
	if path := expandAuditKeyFile("%h/.ssh/keys_%U_%%", passwd[1]); path != "/home/deploy/.ssh/keys_1000_%" {
		t.Fatalf("expand = %q", path)
	}
}

func TestAuditKeyChangedWarnsAboutKeysItCannotSee(t *testing.T) {
	facts := auditFacts{Now: testLoginAt(0), ExternalAccounts: "sss",
		SSHD: sshdConfig{raw: map[string]string{"authorizedkeyscommand": "/usr/bin/sss_ssh_authorizedkeys"}}}
	want := []string{T("audit.ssh.keys_command", "/usr/bin/sss_ssh_authorizedkeys"), T("audit.account.external", "sss")}
	if result := auditKeyChanged(facts); !reflect.DeepEqual(result.Warnings, want) {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestReadAuditExternalAccounts(t *testing.T) {
	root := t.TempDir()
	writeAuditFile(t, root, "/etc/nsswitch.conf", "# passwd: ldap\npasswd:         files sss systemd\ngroup: files ldap\n", 0o644)
	if got := readAuditExternalAccounts(filepath.Join(root, "/etc/nsswitch.conf")); got != "sss" {
		t.Fatalf("got %q", got)
	}
	writeAuditFile(t, root, "/etc/nsswitch.conf", "passwd: files systemd\n", 0o644)
	if got := readAuditExternalAccounts(filepath.Join(root, "/etc/nsswitch.conf")); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestCollectAuditKeyFilesForAccountsSharingAHome(t *testing.T) {
	root := t.TempDir()
	passwd := []auditPasswd{{Name: "app1", UID: 1001, Home: "/srv/shared"}, {Name: "app2", UID: 1002, Home: "/srv/shared"}}
	writeAuditFile(t, root, "/etc/ssh/keys/app1", "ssh-ed25519 AAAA\n", 0o644)
	writeAuditFile(t, root, "/etc/ssh/keys/app2", "ssh-ed25519 BBBB\n", 0o644)
	owner := func(fs.FileInfo) (uint32, bool) { return 0, false }
	config := sshdConfig{raw: map[string]string{"authorizedkeysfile": "/etc/ssh/keys/%u"}}
	files, _ := collectAuditKeyFiles(auditSources{root: root, owner: owner}, passwd, auditKeyFilePatterns(config))
	var got []string
	for _, file := range files {
		got = append(got, file.User+":"+file.Path)
	}
	if !reflect.DeepEqual(got, []string{"app1:/etc/ssh/keys/app1", "app2:/etc/ssh/keys/app2"}) {
		t.Fatalf("files = %v", got)
	}
}

func TestCollectAuditKeyFilesReadsOtherPathsWhenTheHomeIsLocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission bits")
	}
	root := t.TempDir()
	writeAuditFile(t, root, "/home/deploy/.ssh/authorized_keys", "ssh-ed25519 AAAA\n", 0o600)
	writeAuditFile(t, root, "/etc/ssh/keys/deploy", "ssh-ed25519 BBBB\n", 0o644)
	locked := filepath.Join(root, "home/deploy")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	owner := func(fs.FileInfo) (uint32, bool) { return 0, false }
	config := sshdConfig{raw: map[string]string{"authorizedkeysfile": ".ssh/authorized_keys /etc/ssh/keys/%u"}}
	files, skipped := collectAuditKeyFiles(auditSources{root: root, owner: owner}, []auditPasswd{{Name: "deploy", UID: 1000, Home: "/home/deploy"}}, auditKeyFilePatterns(config))
	if skipped != 1 || len(files) != 1 || files[0].Path != "/etc/ssh/keys/deploy" {
		t.Fatalf("files = %+v, skipped = %d", files, skipped)
	}
}

// aarch64 glibc는 시각이 8바이트라 utmp가 400바이트, lastlog가 296바이트다. x86_64 배치로 읽으면 wtmp가 1996년부터로 보이고
// lastlog의 터미널이 비어, 정상 로그인이 login.mismatch fail이 된다. okrr-intranet-slackbot-1(Ubuntu 24.04 arm64)에서 본 값이다.
func TestAuditLoginRecordsFollowTheArchitectureLayout(t *testing.T) {
	if auditLayoutFor("amd64") != auditLayoutCompat32 || auditLayoutFor("arm64") != auditLayoutTime64 {
		t.Fatal("layout by architecture")
	}
	at := time.Date(2026, 11, 13, 1, 0, 24, 0, time.UTC)
	for name, layout := range map[string]auditRecordLayout{"compat32": auditLayoutCompat32, "time64": auditLayoutTime64} {
		previous := auditLayout
		auditLayout = layout
		root := t.TempDir()
		wtmp := make([]byte, layout.utmpSize)
		binary.LittleEndian.PutUint16(wtmp[0:2], auditUtmpTypeUser)
		binary.LittleEndian.PutUint32(wtmp[4:8], 1313221)
		copy(wtmp[8:40], "pts/1")
		copy(wtmp[44:76], "root")
		copy(wtmp[76:332], "100.65.168.177")
		lastlog := make([]byte, layout.lastlogSize)
		if layout.timeBytes == 8 {
			binary.LittleEndian.PutUint64(wtmp[344:352], uint64(at.Unix()))
			binary.LittleEndian.PutUint64(lastlog[0:8], uint64(at.Unix()))
		} else {
			binary.LittleEndian.PutUint32(wtmp[340:344], uint32(at.Unix()))
			binary.LittleEndian.PutUint32(lastlog[0:4], uint32(at.Unix()))
		}
		copy(lastlog[layout.timeBytes:], "pts/1")
		copy(lastlog[layout.timeBytes+32:], "100.65.168.177")
		writeAuditFile(t, root, "/var/log/wtmp", string(wtmp), 0o664)
		writeAuditFile(t, root, auditLastlogFile, string(lastlog), 0o664)
		records, err := readAuditLogins(auditSources{root: root})
		last, lastErr := readAuditLastlog(auditSources{root: root}, []int{0})
		auditLayout = previous
		if err != nil || lastErr != nil {
			t.Fatalf("%s: %v %v", name, err, lastErr)
		}
		want := auditLogin{User: "root", Line: "pts/1", Host: "100.65.168.177", PID: 1313221, Start: at}
		if len(records.Truncated) != 0 || !records.Since.Equal(at) || !reflect.DeepEqual(records.Sessions, []auditLogin{want}) {
			t.Errorf("%s: records = %+v", name, records)
		}
		if !reflect.DeepEqual(last[0], auditLastlog{Time: at, Line: "pts/1", Host: "100.65.168.177"}) {
			t.Errorf("%s: lastlog = %+v", name, last[0])
		}
	}
}
