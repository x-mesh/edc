package edc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	if sources.journal == nil || facts.LoginErr != nil {
		return
	}
	if !facts.Root {
		facts.JournalErr = errAuditNeedsRoot
		return
	}
	since := facts.Now.Add(-auditJournalWindow)
	if facts.Logins.Since.After(since) {
		since = facts.Logins.Since
	}
	facts.Journal, facts.JournalLimited, facts.JournalErr = sources.journal(ctx, since)
	if facts.JournalErr != nil {
		return
	}
	accepted := auditSSHDAcceptedLogins(facts.Journal)
	if len(accepted) == 0 {
		return
	}
	facts.AcceptedSince = accepted[0].Time
	for _, login := range accepted {
		if login.Time.Before(facts.AcceptedSince) {
			facts.AcceptedSince = login.Time
		}
	}
	consumed, recorded := joinAuditLoginsByPID(&facts.Logins, accepted, facts.AcceptedSince)
	if recorded > 0 && len(consumed) == 0 {
		consumed = joinAuditLoginsByTime(&facts.Logins, accepted, facts.AcceptedSince)
		if len(consumed) == 0 {
			facts.Unjoinable = true
			return
		}
		facts.JoinedByTime = true
	}
	facts.Joined = true
	applyAuditJournalEnds(&facts.Logins, facts.Journal)
	facts.NoTTY = auditNoTTYAccess(consumed, accepted)
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
	Time               time.Time
}

// auditSSHDAcceptedLogins는 sshd가 받아들인 로그인이다. 형식은 "Accepted <method> for <user> from <host> port <port> ssh2[: <key>]"다.
func auditSSHDAcceptedLogins(entries []auditJournalEntry) []auditAccepted {
	var accepted []auditAccepted
	for _, entry := range entries {
		if !slices.Contains(auditSSHDIdentifiers, entry.Identifier) {
			continue
		}
		fields := strings.Fields(entry.Message)
		if len(fields) < 6 || fields[0] != "Accepted" || fields[2] != "for" || fields[4] != "from" {
			continue
		}
		accepted = append(accepted, auditAccepted{PID: entry.PID, User: fields[3], Host: fields[5], Method: fields[1], Time: entry.Time})
	}
	return accepted
}

// joinAuditLoginsByPID는 OpenSSH 9.8부터 쓰는 짝짓기다. wtmp의 PID, Accepted의 PID, logind의 LEADER가 모두 sshd-session이다.
// 두 짝짓기 모두 wtmp 로그인과 짝지은 Accepted의 PID를 돌려준다. pty 없는 접속은 그 나머지다.
func joinAuditLoginsByPID(records *auditLoginRecords, accepted []auditAccepted, since time.Time) (map[int]bool, int) {
	pids := map[int]bool{}
	for _, login := range accepted {
		pids[login.PID] = true
	}
	consumed := map[int]bool{}
	recorded := 0
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) {
			continue
		}
		recorded++
		if pids[session.PID] {
			session.SSHD = session.PID
			consumed[session.PID] = true
		}
	}
	return consumed, recorded
}

// joinAuditLoginsByTime은 후보가 여럿이면 모두 짝지은 것으로 센다. 그래서 pty 없는 접속 수는 실제보다 작거나 같다.
func joinAuditLoginsByTime(records *auditLoginRecords, accepted []auditAccepted, since time.Time) map[int]bool {
	consumed := map[int]bool{}
	for index := range records.Sessions {
		session := &records.Sessions[index]
		if !auditRemotePTY(*session) || session.Start.Before(since) {
			continue
		}
		var candidates []int
		for _, login := range accepted {
			gap := login.Time.Sub(session.Start)
			if login.User == session.User && login.Host == session.Host && gap <= auditJoinSkew && gap >= -auditJoinSkew {
				candidates = append(candidates, login.PID)
			}
		}
		for _, pid := range candidates {
			consumed[pid] = true
		}
		switch len(candidates) {
		case 0:
		case 1:
			session.SSHD = candidates[0]
		default:
			session.SSHD = auditSSHDAmbiguous
		}
	}
	return consumed
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

func auditNoTTYAccess(recorded map[int]bool, accepted []auditAccepted) []auditNoTTY {
	counts := map[[3]string]int{}
	for _, login := range accepted {
		if !recorded[login.PID] {
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
