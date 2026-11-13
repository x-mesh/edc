package edc

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// auditJournalWindow는 journal을 읽는 기간이다. 바쁜 호스트에서 wtmp 전체 기간의 journal을 읽으면 audit의 1초를 넘는다.
const auditJournalWindow = 7 * 24 * time.Hour

// auditJournalMaxEntries는 journalctl -n의 값이다. 이만큼 읽으면 기간의 앞부분을 놓쳤을 수 있다.
// 항목 하나가 약 400바이트라 runHostJournal의 32 MiB 상한 안에 든다.
const auditJournalMaxEntries = 50000

// auditJournalSkew는 wtmp의 로그인과 logind의 새 세션이 같은 로그인으로 볼 시각 차이다. PID가 재사용돼도 다른 로그인과 섞이지 않게 한다.
const auditJournalSkew = 5 * time.Second

// systemd catalog의 MESSAGE_ID다. 문구가 바뀌거나 번역돼도 같다. "logged out"에는 MESSAGE_ID가 없어 문구로 찾는다.
const (
	auditLogindNewSession     = "8d45620c1a4348dbb17410da57c60c66"
	auditLogindRemovedSession = "3354939424b4456d9802ca8333ed424a"
)

// auditSSHDIdentifiers는 sshd의 syslog 이름이다. OpenSSH 9.8부터 인증 뒤의 로그는 sshd-session이 남긴다.
var auditSSHDIdentifiers = []string{"sshd", "sshd-session"}

type auditJournalEntry struct {
	Time                           time.Time
	Identifier, MessageID, Message string
	PID, Leader                    int
	SessionID                      string
}

type auditJournalRaw struct {
	Realtime   json.RawMessage `json:"__REALTIME_TIMESTAMP"`
	Identifier json.RawMessage `json:"SYSLOG_IDENTIFIER"`
	MessageID  json.RawMessage `json:"MESSAGE_ID"`
	Message    json.RawMessage `json:"MESSAGE"`
	PID        json.RawMessage `json:"_PID"`
	Leader     json.RawMessage `json:"LEADER"`
	SessionID  json.RawMessage `json:"SESSION_ID"`
}

func parseAuditJournal(reader io.Reader) ([]auditJournalEntry, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), eventsJournalLineLimit)
	var entries []auditJournalEntry
	for scanner.Scan() {
		var raw auditJournalRaw
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		micros, err := strconv.ParseInt(journalField(raw.Realtime), 10, 64)
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(journalField(raw.PID))
		leader, _ := strconv.Atoi(journalField(raw.Leader))
		entries = append(entries, auditJournalEntry{
			Time: time.UnixMicro(micros).UTC(), Identifier: journalField(raw.Identifier), MessageID: journalField(raw.MessageID),
			Message: journalField(raw.Message), PID: pid, Leader: leader, SessionID: journalField(raw.SessionID),
		})
	}
	return entries, scanner.Err()
}

func collectAuditJournal(ctx context.Context, sources auditSources, facts *auditFacts) {
	if sources.journal == nil {
		return
	}
	if !facts.Root {
		facts.JournalErr = errAuditNeedsRoot
		return
	}
	since := facts.Now.Add(-auditJournalWindow)
	if facts.LoginErr == nil && facts.Logins.Since.After(since) {
		since = facts.Logins.Since
	}
	facts.Journal, facts.JournalLimited, facts.JournalErr = sources.journal(ctx, since)
	if facts.JournalErr != nil {
		return
	}
	facts.JournalRead = true
	facts.JournalSince = auditJournalCoverage(facts.Journal, since)
	accepted := auditSSHDAcceptedLogins(facts.Journal)
	facts.Accepted = accepted
	// wtmp를 읽지 못하면 짝지을 로그인이 없다. login.local_source만 이 journal을 쓴다.
	if facts.LoginErr != nil {
		return
	}
	if len(accepted) == 0 {
		// Accepted가 없으면 범위 안의 원격 pty 로그인은 모두 sshd 기록이 없는 것이다. 범위는 journal이 실제로 가진 첫 기록부터다.
		facts.AcceptedSince = facts.JournalSince
		facts.Joined = true
		applyAuditJournalEnds(&facts.Logins, facts.Journal)
		return
	}
	facts.AcceptedSince = accepted[0].Time
	consumed := map[int]bool{}
	recorded, byPID := joinAuditLoginsByPID(&facts.Logins, accepted, auditSSHDOpenedEarlier(facts.Journal), facts.AcceptedSince, consumed)
	byTime, byParent := 0, 0
	if byPID < recorded {
		byTime = joinAuditLoginsByTime(&facts.Logins, accepted, facts.AcceptedSince, consumed)
	}
	if byPID+byTime < recorded {
		byParent = joinAuditLoginsByParent(&facts.Logins, accepted, sources.file("/proc"), facts.AcceptedSince, consumed)
	}
	// 9.8부터는 wtmp에도 sshd의 PID가 남아 PID로 짝지어진다. 하나라도 PID로 맞았으면 남은 로그인은 재사용된 PID이거나 기록이 없는 것이다.
	byConnection := 0
	if byPID == 0 && byTime+byParent < recorded {
		byConnection = joinAuditLoginsByConnection(&facts.Logins, accepted, facts.AcceptedSince, consumed)
	}
	facts.JoinedByConnection = byConnection
	if recorded > 0 && byPID+byTime+byParent+byConnection == 0 {
		facts.Unjoinable = true
		return
	}
	facts.JoinedByTime = byTime
	facts.Joined = true
	applyAuditJournalEnds(&facts.Logins, facts.Journal)
	facts.NoTTY = auditNoTTYAccess(consumed, accepted)
}

// auditJournalCoverage는 journal이 가진 첫 기록의 시각이다. sshd 기록을 먼저 보고, 없으면 다른 기록, 그것도 없으면 조회를 시작한 시각이다.
func auditJournalCoverage(entries []auditJournalEntry, since time.Time) time.Time {
	var sshd, any time.Time
	for _, entry := range entries {
		if any.IsZero() || entry.Time.Before(any) {
			any = entry.Time
		}
		if slices.Contains(auditSSHDIdentifiers, entry.Identifier) && (sshd.IsZero() || entry.Time.Before(sshd)) {
			sshd = entry.Time
		}
	}
	return cmp.Or(sshd, any, since)
}

// auditRemotePTY는 원격에서 pty를 연 로그인이다. sshd가 남긴 로그인만 journal과 비교할 수 있다.
func auditRemotePTY(session auditLogin) bool {
	return strings.HasPrefix(session.Line, "pts/") && session.Host != ""
}

// auditSSHDAmbiguous는 시각으로 짝지을 때 후보가 둘 이상이라 어느 sshd인지 모르는 로그인이다. 짝은 있으므로 journal에 없다고 보지 않지만, 세션의 끝은 붙이지 않는다.
const auditSSHDAmbiguous = -1

// auditJoinSkew는 시각으로 짝지을 때 wtmp의 로그인과 sshd Accepted가 다를 수 있는 시각이다. OpenSSH 8.9에서 두 기록은 같은 초였다.
const auditJoinSkew = 2 * time.Second

type auditAccepted struct {
	PID                int
	User, Host, Method string
	// Port는 출발지 포트다. 줄에 없거나 숫자가 아니면 0이다.
	Port int
	Time time.Time
	// Until은 이 접속이 끝난 시각이다. 같은 PID의 "session closed"나 그 PID의 다음 Accepted다. 0이면 범위 안에서 끝나지 않았다.
	Until time.Time
}

// auditSSHDSessionClosed는 sshd 접속 하나가 끝날 때 PAM이 남기는 기록이다. UsePAM no면 없고, 그때는 같은 PID의 다음 Accepted로 끝을 정한다.
const auditSSHDSessionClosed = "pam_unix(sshd:session): session closed for user "

// auditSSHDAcceptedLogins는 sshd가 받아들인 로그인을 시각 순으로 돌려준다. 형식은 "Accepted <method> for <user> from <host> port <port> ssh2[: <key>]"다.
func auditSSHDAcceptedLogins(entries []auditJournalEntry) []auditAccepted {
	var accepted []auditAccepted
	var closed []auditJournalEntry
	for _, entry := range entries {
		if !slices.Contains(auditSSHDIdentifiers, entry.Identifier) {
			continue
		}
		if strings.HasPrefix(entry.Message, auditSSHDSessionClosed) {
			closed = append(closed, entry)
			continue
		}
		fields := strings.Fields(entry.Message)
		if len(fields) < 6 || fields[0] != "Accepted" || fields[2] != "for" || fields[4] != "from" {
			continue
		}
		login := auditAccepted{PID: entry.PID, User: fields[3], Host: fields[5], Method: fields[1], Time: entry.Time}
		if len(fields) >= 8 && fields[6] == "port" {
			if port, err := strconv.ParseUint(fields[7], 10, 16); err == nil {
				login.Port = int(port)
			}
		}
		accepted = append(accepted, login)
	}
	slices.SortStableFunc(accepted, func(a, b auditAccepted) int { return a.Time.Compare(b.Time) })
	for index := range accepted {
		login := &accepted[index]
		for _, next := range accepted[index+1:] {
			if next.PID == login.PID {
				login.Until = next.Time
				break
			}
		}
		for _, entry := range closed {
			if entry.PID == login.PID && !entry.Time.Before(login.Time) && (login.Until.IsZero() || entry.Time.Before(login.Until)) {
				login.Until = entry.Time
			}
		}
	}
	return accepted
}

// joinAuditLoginsByPID는 OpenSSH 9.8부터 쓰는 짝짓기다. wtmp의 PID, Accepted의 PID, logind의 LEADER가 모두 sshd-session이다.
// 연결 하나가 pty를 여러 번 열 수 있어(ControlMaster) 로그인은 Accepted부터 그 접속이 끝날 때까지 어느 때나 올 수 있다.
// 그 밖의 로그인은 PID가 같아도 재사용된 PID라 짝짓지 않는다. consumed에는 짝지은 Accepted의 번호를 넣는다. pty 없는 접속은 그 나머지다.
func joinAuditLoginsByPID(records *auditLoginRecords, accepted []auditAccepted, earlier map[int]auditSSHDEarlier, since time.Time, consumed map[int]bool) (int, int) {
	recorded, joined := 0, 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) {
			continue
		}
		recorded++
		found := false
		for number := len(accepted) - 1; number >= 0; number-- {
			login := accepted[number]
			if login.PID != session.PID || login.User != session.User || !auditSameSource(session.Host, login.Host) || login.Time.Sub(session.Start) > auditJoinSkew {
				continue
			}
			found = true
			if login.Until.IsZero() || !session.Start.After(login.Until.Add(auditJoinSkew)) {
				session.SSHD = session.PID
				consumed[number] = true
				joined++
			}
			break
		}
		// 범위 앞에서 Accepted를 받은 연결이 범위 안에서 pty를 연 것이다. 그 PID는 로그인 뒤에도 sshd였으므로 재사용된 PID가 아니다.
		if connection, ok := earlier[session.PID]; !found && ok && !connection.Last.Before(session.Start.Add(-auditJoinSkew)) &&
			(connection.User == "" || connection.User == session.User) {
			session.SSHD = auditSSHDAmbiguous
			joined++
		}
	}
	return recorded, joined
}

// auditSSHDEarlier는 journal 범위보다 먼저 시작한 sshd 연결이다. Last는 그 연결의 마지막 기록 시각이고, 그때까지 연결이 살아 있었다.
// User는 "session closed for user"에서 읽은 사용자이고, 그 기록이 없으면 비어 있다.
type auditSSHDEarlier struct {
	Last time.Time
	User string
}

// auditSSHDOpenedEarlier는 첫 Accepted보다 먼저 기록이 나온 sshd PID다.
func auditSSHDOpenedEarlier(entries []auditJournalEntry) map[int]auditSSHDEarlier {
	acceptedAt := map[int]time.Time{}
	for _, entry := range entries {
		if slices.Contains(auditSSHDIdentifiers, entry.Identifier) && strings.HasPrefix(entry.Message, "Accepted ") {
			if at, ok := acceptedAt[entry.PID]; !ok || entry.Time.Before(at) {
				acceptedAt[entry.PID] = entry.Time
			}
		}
	}
	earlier := map[int]auditSSHDEarlier{}
	for _, entry := range entries {
		if !slices.Contains(auditSSHDIdentifiers, entry.Identifier) || entry.PID == 0 {
			continue
		}
		if accepted, ok := acceptedAt[entry.PID]; ok && !entry.Time.Before(accepted) {
			continue
		}
		connection := earlier[entry.PID]
		if entry.Time.After(connection.Last) {
			connection.Last = entry.Time
		}
		if user, ok := strings.CutPrefix(entry.Message, auditSSHDSessionClosed); ok {
			connection.User = strings.TrimSpace(user)
		}
		earlier[entry.PID] = connection
	}
	return earlier
}

// auditSameSource는 wtmp의 출발지가 Accepted의 주소와 같은지다. UseDNS yes면 wtmp에 이름이 남아 주소와 비교할 수 없으므로 같다고 본다.
func auditSameSource(wtmpHost, acceptedHost string) bool {
	wtmp, err := netip.ParseAddr(wtmpHost)
	if err != nil {
		return true
	}
	accepted, err := netip.ParseAddr(acceptedHost)
	return err != nil || wtmp.Unmap() == accepted.Unmap()
}

// joinAuditLoginsByTime은 PID로 짝짓지 못한 로그인을 사용자, 출발지, 시각으로 짝짓는다. OpenSSH 9.8 미만은 wtmp에 shell의 PID를 남긴다.
// 후보가 여럿이면 모두 짝지은 것으로 센다. 그래서 pty 없는 접속 수는 실제보다 작거나 같다.
// 후보는 PID로 짝지은 Accepted만 뺀다. 앞의 모호한 로그인이 후보를 모두 가져가면, 같은 후보를 가진 뒤 로그인이 sshd 기록 없는 로그인으로 보인다.
func joinAuditLoginsByTime(records *auditLoginRecords, accepted []auditAccepted, since time.Time, consumed map[int]bool) int {
	byPID := maps.Clone(consumed)
	joined := 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) || session.SSHD != 0 {
			continue
		}
		var candidates []int
		for number, login := range accepted {
			gap := login.Time.Sub(session.Start)
			if !byPID[number] && login.User == session.User && login.Host == session.Host && gap <= auditJoinSkew && gap >= -auditJoinSkew {
				candidates = append(candidates, number)
			}
		}
		switch len(candidates) {
		case 0:
			continue
		case 1:
			session.SSHD = accepted[candidates[0]].PID
			// 앞 로그인이 이미 가져간 Accepted면 어느 로그인의 것인지 모른다. 세션의 끝을 붙이지 않는다.
			if consumed[candidates[0]] {
				session.SSHD = auditSSHDAmbiguous
			}
		default:
			session.SSHD = auditSSHDAmbiguous
		}
		for _, number := range candidates {
			consumed[number] = true
		}
		joined++
	}
	return joined
}

// auditParentDepth는 shell에서 sshd까지 올라갈 부모 수다. OpenSSH 9.6은 shell의 부모가 바로 Accepted를 남긴 sshd이고,
// 9.8부터는 sshd-session 사이에 하나가 더 있다.
const auditParentDepth = 4

// joinAuditLoginsByParent는 9.8 미만에서 연결 하나에 터미널을 더 연 로그인(ControlMaster)을 짝짓는다. 그 터미널에는 새 Accepted가 없고
// wtmp에는 shell의 PID가 남아 PID로도 시각으로도 짝지을 수 없다. 아직 열린 로그인이면 shell의 조상에 그 연결의 sshd가 있다.
// 끝난 로그인은 shell PID가 재사용됐을 수 있어 보지 않는다. 사용자와 출발지가 같고 연결이 끝나지 않은 Accepted만 짝짓는다.
func joinAuditLoginsByParent(records *auditLoginRecords, accepted []auditAccepted, procRoot string, since time.Time, consumed map[int]bool) int {
	joined := 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) || session.SSHD != 0 || !session.End.IsZero() || session.PID <= 0 {
			continue
		}
		pid := session.PID
		for range auditParentDepth {
			pid = auditParentPID(procRoot, pid)
			if pid <= 1 {
				break
			}
			number := slices.IndexFunc(accepted, func(login auditAccepted) bool {
				return login.PID == pid && login.Until.IsZero() && login.User == session.User && auditSameSource(session.Host, login.Host) && !login.Time.After(session.Start.Add(auditJoinSkew))
			})
			if number >= 0 {
				session.SSHD = pid
				consumed[number] = true
				joined++
				break
			}
		}
	}
	return joined
}

// joinAuditLoginsByConnection은 9.8 미만에서 이미 닫힌, 연결 하나 위의 다른 터미널을 짝짓는다. shell이 끝나 부모를 따라갈 수 없다.
// 같은 사용자와 출발지의 연결이 그 로그인이 시작할 때 열려 있었으면 그 연결에서 연 터미널로 본다. 어느 터미널인지는 모르므로
// 세션의 끝은 붙이지 않는다(auditSSHDAmbiguous). okrr-intranet-1(OpenSSH 8.9)에서 Accepted 하나 위에 터미널이 셋 열렸다.
func joinAuditLoginsByConnection(records *auditLoginRecords, accepted []auditAccepted, since time.Time, consumed map[int]bool) int {
	joined := 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) || session.SSHD != 0 {
			continue
		}
		number := slices.IndexFunc(accepted, func(login auditAccepted) bool {
			return login.User == session.User && auditSameSource(session.Host, login.Host) && !login.Time.After(session.Start.Add(auditJoinSkew)) &&
				(login.Until.IsZero() || !session.Start.After(login.Until.Add(auditJoinSkew)))
		})
		if number >= 0 {
			session.SSHD = auditSSHDAmbiguous
			consumed[number] = true
			joined++
		}
	}
	return joined
}

// auditParentPID는 /proc/<pid>/stat의 부모 PID다. 읽지 못하면 0이다.
func auditParentPID(procRoot string, pid int) int {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	stat, ok := parseLinuxProcessStat(pid, string(data))
	if !ok {
		return 0
	}
	return stat.PPID
}

// applyAuditJournalEnds는 로그아웃 레코드가 없는 wtmp 세션에 logind의 세션 끝을 채운다. 끝은 sshd가 끝난 "logged out" 시각이다.
// 남은 process가 모두 끝나야 오는 "Removed"는 nohup과 tmux 때문에 늦어질 수 있어 "logged out"이 없을 때만 쓴다.
func applyAuditJournalEnds(records *auditLoginRecords, entries []auditJournalEntry) int {
	started := map[int][]auditJournalEntry{}
	ended := map[string]time.Time{}
	removed := map[string]time.Time{}
	for _, entry := range entries {
		if entry.Identifier != "systemd-logind" || entry.SessionID == "" {
			continue
		}
		switch {
		case entry.MessageID == auditLogindNewSession:
			started[entry.Leader] = append(started[entry.Leader], entry)
		case entry.MessageID == auditLogindRemovedSession:
			if _, ok := removed[entry.SessionID]; !ok {
				removed[entry.SessionID] = entry.Time
			}
		case entry.MessageID == "" && strings.HasPrefix(entry.Message, "Session ") && strings.Contains(entry.Message, " logged out."):
			if _, ok := ended[entry.SessionID]; !ok {
				ended[entry.SessionID] = entry.Time
			}
		}
	}
	filled := 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !session.End.IsZero() || session.SSHD <= 0 {
			continue
		}
		for _, start := range started[session.SSHD] {
			gap := start.Time.Sub(session.Start)
			if gap > auditJournalSkew || gap < -auditJournalSkew {
				continue
			}
			end, ok := ended[start.SessionID]
			if !ok {
				end, ok = removed[start.SessionID]
			}
			if ok {
				session.End = end
				filled++
			}
			break
		}
	}
	return filled
}

// auditNoTTY는 wtmp에 남지 않은 ssh 접속이다. sshd는 pty를 연 세션만 wtmp에 남겨 scp, sftp, ssh host 명령은 여기에만 보인다.
type auditNoTTY struct {
	User, Host, Method string
	Count              int
}

// auditNoTTYShown은 근거에 적는 접속 묶음 수다.
const auditNoTTYShown = 5

func auditNoTTYAccess(consumed map[int]bool, accepted []auditAccepted) []auditNoTTY {
	counts := map[[3]string]int{}
	for number, login := range accepted {
		if !consumed[number] {
			counts[[3]string{login.User, login.Host, login.Method}]++
		}
	}
	accesses := make([]auditNoTTY, 0, len(counts))
	for key, count := range counts {
		accesses = append(accesses, auditNoTTY{User: key[0], Host: key[1], Method: key[2], Count: count})
	}
	slices.SortFunc(accesses, func(a, b auditNoTTY) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return strings.Compare(a.User+a.Host+a.Method, b.User+b.Host+b.Method)
	})
	return accesses
}

// auditJournalSkipReason은 journal을 읽지 못한 이유다. 로그인 점검은 wtmp만으로 판정하고 이 이유를 경고로 남긴다.
func auditJournalSkipReason(err error) string {
	switch {
	case errors.Is(err, errAuditNeedsRoot), errors.Is(err, errEventsNeedsRoot):
		return T("audit.login.journal_root")
	case errors.Is(err, errEventsNoJournal):
		return T("audit.login.journal_none")
	}
	return T("audit.login.journal_error", infoSingleLine(err.Error()))
}
