package edc

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
)

// utmp와 lastlog 레코드의 크기와 위치는 x86_64·arm64 glibc의 struct utmp, struct lastlog다. 시각은 32비트 초다.
const (
	auditUtmpRecordSize    = 384
	auditLastlogRecordSize = 292

	auditUtmpTypeBoot = 2
	auditUtmpTypeUser = 7
	auditUtmpTypeDead = 8
)

// auditLoginMaxBytes는 wtmp 파일마다 읽는 끝부분의 크기다. 오래 회전하지 않은 wtmp가 audit의 1초를 넘기지 않게 한다.
var auditLoginMaxBytes int64 = 64 << 20

var (
	auditWtmpFiles   = []string{"/var/log/wtmp.1", "/var/log/wtmp"}
	auditWtmpDB      = "/var/lib/wtmpdb/wtmp.db"
	auditLastlogFile = "/var/log/lastlog"
	auditLastlogDB   = "/var/lib/lastlog/lastlog2.db"
)

// errAuditLoginSQLite는 기존 binary 파일 없이 SQLite 형식(wtmpdb, lastlog2)만 있다는 뜻이다. 이 형식은 읽지 않는다.
var errAuditLoginSQLite = errors.New("only the SQLite login database is present")

// auditCompressedMagic은 logrotate가 압축한 회전 파일의 앞 바이트다. 압축된 wtmp는 읽지 않고 판정 범위를 줄인다.
var auditCompressedMagic = [][]byte{{0x1f, 0x8b}, {0x28, 0xb5, 0x2f, 0xfd}, {0xfd, '7', 'z', 'X', 'Z'}, []byte("BZh")}

// auditLogin은 wtmp의 로그인 세션 하나다. End가 0이면 끝난 기록이 없다. 아직 접속 중이거나 기록 없이 재부팅됐다.
type auditLogin struct {
	User, Line, Host string
	// PID는 로그인을 기록한 sshd process다. sshd의 Accepted 로그와 logind의 LEADER가 같은 값이다.
	PID int
	// SSHD는 journal에서 이 로그인과 짝지은 sshd의 PID다. 0이면 짝이 없고, auditSSHDAmbiguous면 짝은 있지만 어느 것인지 모른다.
	SSHD       int
	Start, End time.Time
}

type auditLoginRecords struct {
	Sessions []auditLogin
	// Since는 읽은 가장 오래된 레코드의 시각이다. 이보다 앞선 로그인은 wtmp로 판정할 수 없다.
	Since time.Time
	// Zeroed는 모든 바이트가 0인 레코드의 수다. 로그 정리 도구가 레코드를 지운 흔적일 수 있다.
	Zeroed int
	// Truncated는 끝에 레코드 크기에 못 미치는 바이트가 남은 파일이다. 쓰는 중이었거나 잘린 파일이다.
	Truncated []string
	// Compressed는 압축돼 읽지 않은 파일이고, Limited는 auditLoginMaxBytes 때문에 앞부분을 읽지 않은 파일이다.
	Compressed, Limited []string
}

type auditLastlog struct {
	Time       time.Time
	Line, Host string
}

func readAuditLogins(sources auditSources) (auditLoginRecords, error) {
	var records auditLoginRecords
	open := map[string]int{}
	read := 0
	for _, path := range auditWtmpFiles {
		data, err := readAuditWtmpFile(sources.file(path), path, &records)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return auditLoginRecords{}, err
		}
		if data == nil {
			continue
		}
		read++
		records.parse(data, open)
	}
	if read == 0 {
		if _, err := os.Stat(sources.file(auditWtmpDB)); err == nil {
			return auditLoginRecords{}, errAuditLoginSQLite
		}
		return auditLoginRecords{}, fmt.Errorf("%s: %w", auditWtmpFiles[len(auditWtmpFiles)-1], fs.ErrNotExist)
	}
	return records, nil
}

// readAuditWtmpFile은 레코드 경계에 맞춘 바이트를 돌려준다. 압축 파일이면 nil이다.
func readAuditWtmpFile(full, path string, records *auditLoginRecords) ([]byte, error) {
	file, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	head := make([]byte, 6)
	count, _ := io.ReadFull(file, head)
	for _, magic := range auditCompressedMagic {
		if bytes.HasPrefix(head[:count], magic) {
			records.Compressed = append(records.Compressed, path)
			return nil, nil
		}
	}
	whole := size - size%auditUtmpRecordSize
	if whole != size {
		records.Truncated = append(records.Truncated, path)
	}
	offset := int64(0)
	if whole > auditLoginMaxBytes {
		offset = whole - auditLoginMaxBytes/auditUtmpRecordSize*auditUtmpRecordSize
		records.Limited = append(records.Limited, path)
	}
	data := make([]byte, whole-offset)
	if _, err := file.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data, nil
}

// parse는 USER_PROCESS를 터미널마다 열고 같은 터미널의 DEAD_PROCESS로 닫는다. open은 파일 사이에 이어진다.
func (records *auditLoginRecords) parse(data []byte, open map[string]int) {
	for offset := 0; offset+auditUtmpRecordSize <= len(data); offset += auditUtmpRecordSize {
		record := data[offset : offset+auditUtmpRecordSize]
		if isZeroBytes(record) {
			records.Zeroed++
			continue
		}
		kind := int16(binary.LittleEndian.Uint16(record[0:2]))
		line := cString(record[8:40])
		at := time.Unix(int64(int32(binary.LittleEndian.Uint32(record[340:344]))), 0).UTC()
		if records.Since.IsZero() && at.Unix() > 0 {
			records.Since = at
		}
		switch kind {
		case auditUtmpTypeUser:
			open[line] = len(records.Sessions)
			records.Sessions = append(records.Sessions, auditLogin{User: cString(record[44:76]), Line: line, Host: cString(record[76:332]), PID: int(int32(binary.LittleEndian.Uint32(record[4:8]))), Start: at})
		case auditUtmpTypeDead:
			if index, ok := open[line]; ok {
				records.Sessions[index].End = at
				delete(open, line)
			}
		case auditUtmpTypeBoot:
			clear(open)
		}
	}
}

// readAuditLastlog는 UID마다 한 레코드만 읽는다. lastlog는 UID로 위치를 정하는 sparse 파일이라 통째로 읽으면 큰 UID에서 수십 MB가 된다.
func readAuditLastlog(sources auditSources, uids []int) (map[int]auditLastlog, error) {
	file, err := os.Open(sources.file(auditLastlogFile))
	if errors.Is(err, fs.ErrNotExist) {
		if _, dbErr := os.Stat(sources.file(auditLastlogDB)); dbErr == nil {
			return nil, errAuditLoginSQLite
		}
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := map[int]auditLastlog{}
	record := make([]byte, auditLastlogRecordSize)
	for _, uid := range uids {
		if uid < 0 {
			continue
		}
		if _, err := file.ReadAt(record, int64(uid)*auditLastlogRecordSize); err != nil {
			if errors.Is(err, io.EOF) {
				continue
			}
			return nil, err
		}
		seconds := int32(binary.LittleEndian.Uint32(record[0:4]))
		if seconds <= 0 {
			continue
		}
		entries[uid] = auditLastlog{Time: time.Unix(int64(seconds), 0).UTC(), Line: cString(record[4:36]), Host: cString(record[36:292])}
	}
	return entries, nil
}

func isZeroBytes(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func cString(data []byte) string {
	if index := bytes.IndexByte(data, 0); index >= 0 {
		data = data[:index]
	}
	return string(data)
}

// auditLoginTimeLayout은 근거와 요약에 적는 로그인 시각이다.
const auditLoginTimeLayout = "2006-01-02 15:04:05"

// auditLoginClockSkew는 lastlog와 wtmp의 같은 로그인이 다를 수 있는 시각 차이다. 두 기록은 같은 PAM 단계에서 남아 보통 같은 초다.
const auditLoginClockSkew = time.Second

func auditLoginTime(at time.Time) string { return at.Local().Format(auditLoginTimeLayout) }

func auditLoginSkip(id, source string, err error) Result {
	if errors.Is(err, errAuditLoginSQLite) {
		return auditResult(id, StatusSkip, T("audit.skip.login_sqlite"))
	}
	return auditSkip(id, source, err)
}

// auditLoginWarnings는 판정 범위를 줄인 원인이다. pass라도 어디까지 봤는지 알 수 있게 남긴다.
func auditLoginWarnings(records auditLoginRecords) []string {
	var warnings []string
	if len(records.Compressed) > 0 {
		warnings = append(warnings, T("audit.login.compressed", strings.Join(records.Compressed, ", ")))
	}
	if len(records.Limited) > 0 {
		warnings = append(warnings, T("audit.login.limited", strings.Join(records.Limited, ", ")))
	}
	if len(records.Truncated) > 0 {
		warnings = append(warnings, T("audit.login.truncated", strings.Join(records.Truncated, ", ")))
	}
	return warnings
}

func auditLoginMismatch(facts auditFacts) Result {
	const id = "login.mismatch"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	if facts.LoginErr != nil {
		return auditLoginSkip(id, auditWtmpFiles[len(auditWtmpFiles)-1], facts.LoginErr)
	}
	if facts.LastlogErr != nil {
		return auditLoginSkip(id, auditLastlogFile, facts.LastlogErr)
	}
	records := facts.Logins
	// lastlog는 UID로 적히고 wtmp는 이름으로 적힌다. UID를 나눠 쓰는 이름은 모두 같은 계정으로 본다.
	names := map[int][]string{}
	for _, entry := range facts.Passwd {
		names[entry.UID] = append(names[entry.UID], entry.Name)
	}
	uids := make([]int, 0, len(facts.Lastlog))
	for uid := range facts.Lastlog {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	var missing []string
	var firstMissing time.Time
	checked, before := 0, 0
	for _, uid := range uids {
		last := facts.Lastlog[uid]
		if records.Since.IsZero() || last.Time.Before(records.Since) {
			before++
			continue
		}
		checked++
		found := slices.ContainsFunc(records.Sessions, func(session auditLogin) bool {
			gap := session.Start.Sub(last.Time)
			return slices.Contains(names[uid], session.User) && session.Line == last.Line && gap <= auditLoginClockSkew && gap >= -auditLoginClockSkew
		})
		if !found {
			if firstMissing.IsZero() {
				firstMissing = last.Time
			}
			missing = append(missing, fmt.Sprintf("%s  lastlog %s %s %s", strings.Join(names[uid], ","), auditLoginTime(last.Time), last.Line, last.Host))
			continue
		}
		// sshd는 pty 로그인마다 lastlog를 고친다. sshd journal과 wtmp가 모두 확인한 더 늦은 로그인이 있으면 lastlog를 되돌린 것이다.
		if later, ok := auditLatestConfirmedLogin(facts, names[uid]); ok && later.Start.Sub(last.Time) > auditLoginClockSkew {
			if firstMissing.IsZero() {
				firstMissing = last.Time
			}
			missing = append(missing, fmt.Sprintf("%s  lastlog %s, %s", strings.Join(names[uid], ","), auditLoginTime(last.Time), T("audit.login.later_login", auditLoginTime(later.Start), later.Line, later.Host)))
		}
	}
	unlogged := auditUnloggedLogins(facts)
	var result Result
	switch {
	case len(missing) > 0:
		result = auditResult(id, StatusFail, T("audit.login.mismatch_bad", len(missing), auditLoginTime(records.Since)))
		result.Error = &DiagnosticError{Kind: "login", Message: result.Summary}
		result.Evidence = []Evidence{{Label: T("audit.evidence.accounts"), Value: strings.Join(missing, "\n")}}
		// OpenSSH 9.8부터 인증 뒤의 로그는 sshd-session이 남긴다. 두 이름을 함께 찾아야 버전과 관계없이 보인다.
		result.Next = "journalctl -t sshd -t sshd-session --since '" + auditLoginTime(firstMissing.Add(-time.Minute)) + "'"
	case len(unlogged) > 0:
		result = auditResult(id, StatusWarn, T("audit.login.unlogged", len(unlogged), auditLoginTime(facts.AcceptedSince)))
		lines := make([]string, 0, min(len(unlogged), auditNoTTYShown)+1)
		for _, session := range unlogged[:min(len(unlogged), auditNoTTYShown)] {
			lines = append(lines, fmt.Sprintf("%s  %s %s %s  pid %d", session.User, auditLoginTime(session.Start), session.Line, session.Host, session.PID))
		}
		if extra := len(unlogged) - auditNoTTYShown; extra > 0 {
			lines = append(lines, fmt.Sprintf("+%d", extra))
		}
		result.Evidence = []Evidence{{Label: T("audit.evidence.accounts"), Value: strings.Join(lines, "\n")}}
		result.Next = "journalctl -t sshd -t sshd-session --since '" + auditLoginTime(unlogged[0].Start.Add(-time.Minute)) + "'"
	case records.Since.IsZero() && before > 0:
		result = auditResult(id, StatusWarn, T("audit.login.wtmp_empty", before))
	case records.Zeroed > 0:
		result = auditResult(id, StatusWarn, T("audit.login.zeroed", records.Zeroed, auditLoginTime(records.Since)))
	case records.Since.IsZero():
		result = auditResult(id, StatusPass, T("audit.login.none"))
	default:
		result = auditResult(id, StatusPass, T("audit.login.mismatch_ok", checked, auditLoginTime(records.Since)))
	}
	result.Warnings = auditLoginWarnings(records)
	if facts.Unjoinable {
		result.Warnings = append(result.Warnings, T("audit.login.unjoinable"))
	} else if facts.JoinedByTime {
		result.Warnings = append(result.Warnings, T("audit.login.joined_by_time"))
	}
	if facts.JournalErr != nil {
		result.Warnings = append(result.Warnings, auditJournalSkipReason(facts.JournalErr))
	} else if facts.JournalLimited {
		result.Warnings = append(result.Warnings, T("audit.login.journal_limited", auditJournalMaxEntries))
	}
	if len(facts.NoTTY) > 0 {
		lines := make([]string, 0, auditNoTTYShown+1)
		for _, access := range facts.NoTTY[:min(len(facts.NoTTY), auditNoTTYShown)] {
			lines = append(lines, fmt.Sprintf("%s  %s  %s  %d", access.User, access.Host, access.Method, access.Count))
		}
		if extra := len(facts.NoTTY) - auditNoTTYShown; extra > 0 {
			lines = append(lines, fmt.Sprintf("+%d", extra))
		}
		result.Evidence = append(result.Evidence, Evidence{Label: T("audit.evidence.no_tty"), Value: strings.Join(lines, "\n")})
	}
	if before > 0 && !records.Since.IsZero() {
		result.Warnings = append(result.Warnings, T("audit.login.before_since", before))
	}
	return result
}

// auditLatestConfirmedLogin은 sshd journal과 wtmp가 함께 확인한 마지막 pty 로그인이다.
func auditLatestConfirmedLogin(facts auditFacts, names []string) (auditLogin, bool) {
	var latest auditLogin
	found := false
	for _, session := range facts.Logins.Sessions {
		if session.SSHD != 0 && auditRemotePTY(session) && slices.Contains(names, session.User) && (!found || session.Start.After(latest.Start)) {
			latest, found = session, true
		}
	}
	return latest, found
}

// auditUnloggedLogins는 sshd journal을 읽은 범위 안인데 Accepted가 없는 wtmp의 원격 pty 로그인이다.
// journald가 rate limit으로 버렸거나, journal이나 wtmp를 고친 경우다. 어느 쪽인지 알 수 없어 warn이다.
func auditUnloggedLogins(facts auditFacts) []auditLogin {
	if !facts.Joined {
		return nil
	}
	var unlogged []auditLogin
	for _, session := range facts.Logins.Sessions {
		if auditRemotePTY(session) && !session.Start.Before(facts.AcceptedSince) && session.SSHD == 0 {
			unlogged = append(unlogged, session)
		}
	}
	return unlogged
}

// auditKeyChangeWindow는 authorized_keys가 바뀐 것을 알리는 기간이다. 관리자가 키를 넣는 일도 흔해 fail이 아니라 warn이다.
const auditKeyChangeWindow = 7 * 24 * time.Hour

func auditKeyChanged(facts auditFacts) Result {
	const id = "ssh.key_changed"
	if facts.PasswdErr != nil {
		return auditSkip(id, "/etc/passwd", facts.PasswdErr)
	}
	since := facts.Now.Add(-auditKeyChangeWindow)
	var changed []string
	var firstPath string
	keys := 0
	for _, file := range facts.KeyFiles {
		if !file.Keys {
			continue
		}
		keys++
		if auditKeyChangeTime(file).Before(since) {
			continue
		}
		firstPath = cmp.Or(firstPath, file.Path)
		changed = append(changed, file.User+"  "+file.Path+"  "+auditKeyChangeEvidence(facts, file))
	}
	var result Result
	if len(changed) == 0 {
		result = auditResult(id, StatusPass, T("audit.ssh.key_changed_ok", keys))
	} else {
		result = auditResult(id, StatusWarn, T("audit.ssh.key_changed_bad", len(changed)))
		result.Evidence = []Evidence{{Label: T("audit.evidence.files"), Value: strings.Join(changed, "\n")}}
		result.Next = "ssh-keygen -lf " + firstPath
	}
	if facts.KeySkipped > 0 {
		result.Warnings = append(result.Warnings, T("audit.ssh.keys_skipped", facts.KeySkipped))
	}
	if command, ok := facts.SSHD.raw["authorizedkeyscommand"]; ok && !strings.EqualFold(command, "none") {
		result.Warnings = append(result.Warnings, T("audit.ssh.keys_command", command))
	}
	if facts.ExternalAccounts != "" {
		result.Warnings = append(result.Warnings, T("audit.account.external", facts.ExternalAccounts))
	}
	return result
}

// auditKeyChangeTime은 mtime과 ctime 가운데 늦은 쪽이다.
func auditKeyChangeTime(file auditFile) time.Time {
	if file.ChangeTime.After(file.ModTime) {
		return file.ChangeTime
	}
	return file.ModTime
}

// auditKeyChangeEvidence는 키 파일이 바뀌기 바로 전의 로그인이다. 그 로그인에서 키를 넣었을 가능성이 가장 크다.
func auditKeyChangeEvidence(facts auditFacts, file auditFile) string {
	at := auditKeyChangeTime(file)
	changed := T("audit.ssh.key_changed_at", auditLoginTime(at))
	// 내용을 바꾸면 mtime과 ctime이 같이 움직인다. mtime만 크게 앞서면 touch -r로 되돌렸거나 chmod·chown만 한 것이다.
	if at.Sub(file.ModTime) > time.Minute {
		changed += ", " + T("audit.ssh.key_mtime_older", auditLoginTime(file.ModTime))
	}
	if facts.LoginErr != nil {
		return changed
	}
	var previous auditLogin
	found := false
	for _, session := range facts.Logins.Sessions {
		if session.Start.After(at) {
			continue
		}
		if !found || session.Start.After(previous.Start) {
			previous, found = session, true
		}
	}
	if !found {
		return changed + ", " + T("audit.ssh.key_no_login")
	}
	return changed + ", " + T("audit.ssh.key_login", previous.User, auditLoginTime(previous.Start), previous.Line, cmp.Or(previous.Host, "-"))
}
