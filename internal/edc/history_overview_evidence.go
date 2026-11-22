package edc

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	historyEvidenceMaxResults       = 1000
	historyEvidenceMaxPayloadBytes  = 8 << 20
	historyEvidenceMaxTotalBytes    = 32 << 20
	historyEvidenceMaxEntries       = 200
	historyEvidenceTextLimit        = 1024
	historyEvidenceRunBytes         = 64 << 10
	historyEvidenceCapturePrecision = time.Millisecond
)

type historyOverviewEvidence struct {
	Time        time.Time  `json:"time"`
	End         *time.Time `json:"end,omitempty"`
	Source      string     `json:"source"`
	Kind        string     `json:"kind"`
	Subject     string     `json:"subject,omitempty"`
	Summary     string     `json:"summary"`
	Host        string     `json:"host,omitempty"`
	HostUnknown bool       `json:"host_unknown,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	CapturedAt  *time.Time `json:"captured_at,omitempty"`
	LogKey      string     `json:"log_key,omitempty"`
	LogPath     string     `json:"log_path,omitempty"`
	Overlap     bool       `json:"overlap,omitempty"`
}

type historyEvidenceNotice struct {
	Code   string `json:"code"`
	Source string `json:"source,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type historyEvidenceAccumulator struct {
	entries    []historyOverviewEvidence
	notices    []historyEvidenceNotice
	seen       map[[32]byte]bool
	noticeKeys map[[32]byte]bool
}

func newHistoryEvidenceAccumulator() *historyEvidenceAccumulator {
	return &historyEvidenceAccumulator{entries: []historyOverviewEvidence{}, notices: []historyEvidenceNotice{}, seen: map[[32]byte]bool{}, noticeKeys: map[[32]byte]bool{}}
}

func (acc *historyEvidenceAccumulator) notice(code, source, run, detail string) {
	encoded, _ := json.Marshal([]string{code, source, run, detail})
	key := sha256.Sum256(encoded)
	if acc.noticeKeys[key] {
		return
	}
	acc.noticeKeys[key] = true
	acc.notices = append(acc.notices, historyEvidenceNotice{Code: code, Source: source, RunID: run, Detail: historyEvidenceClip(detail)})
}

func historyEvidenceClip(text string) string {
	runes := []rune(text)
	if len(runes) > historyEvidenceTextLimit {
		return string(runes[:historyEvidenceTextLimit]) + "…"
	}
	return text
}

func (acc *historyEvidenceAccumulator) add(entry historyOverviewEvidence) {
	encoded, _ := json.Marshal([]string{entry.Host, entry.Source, entry.Time.UTC().Format(time.RFC3339Nano), entry.Kind, entry.Subject, entry.Summary, entry.LogKey, entry.LogPath})
	key := sha256.Sum256(encoded)
	if acc.seen[key] {
		return
	}
	if len(acc.entries) >= historyEvidenceMaxEntries {
		acc.notice("entries_limit", "", "", fmt.Sprint(historyEvidenceMaxEntries))
		return
	}
	acc.seen[key] = true
	for _, field := range []*string{&entry.Kind, &entry.Subject, &entry.Summary, &entry.Host} {
		clipped := historyEvidenceClip(*field)
		if clipped != *field {
			acc.notice("text_limit", entry.Source, entry.RunID, fmt.Sprint(historyEvidenceTextLimit))
		}
		*field = clipped
	}
	acc.entries = append(acc.entries, entry)
}

func historyEvidenceInWindow(at, from, to time.Time) bool {
	return !at.IsZero() && !at.Before(from) && at.Before(to)
}

func loadHistoryOverviewEvidence(db *sql.DB, view historyOverview, logs []logHistoryAttempt, logsErr error) ([]historyOverviewEvidence, []historyEvidenceNotice) {
	acc := newHistoryEvidenceAccumulator()
	has, err := historyHasSnapshots(db)
	switch {
	case err != nil:
		acc.notice("snapshots_unavailable", "snapshot", "", err.Error())
	case !has:
		acc.notice("snapshots_unavailable", "snapshot", "", "")
	case view.Run != nil && view.Run.Hostname == "":
		acc.notice("run_host_unknown", "snapshot", view.Run.ID, "")
	default:
		loadHistorySnapshotEvidence(db, view, acc)
	}
	if logsErr != nil {
		acc.notice("logs_unavailable", "log", "", logsErr.Error())
	}
	for _, log := range logs {
		if log.Started.IsZero() {
			continue
		}
		end := log.Ended
		if !end.After(log.Started) && log.Duration > 0 {
			end = log.Started.Add(log.Duration)
		}
		starts := historyEvidenceInWindow(log.Started, view.From, view.To)
		overlaps := log.Started.Before(view.To) && end.After(view.From) && end.After(log.Started)
		if !starts && !overlaps {
			continue
		}
		entry := historyOverviewEvidence{Time: log.Started, Source: "log", Kind: "command", Summary: strings.Join(log.Command, " "), Subject: string(log.Outcome), HostUnknown: true, LogKey: log.Key, LogPath: log.Path, Overlap: !starts}
		if end.After(log.Started) {
			entry.End = &end
		}
		acc.add(entry)
		acc.notice("logs_host_unknown", "log", "", "")
	}
	sort.SliceStable(acc.entries, func(i, j int) bool {
		a, b := acc.entries[i], acc.entries[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		for _, pair := range [][2]string{{a.Host, b.Host}, {a.Source, b.Source}, {a.Kind, b.Kind}, {a.Subject, b.Subject}, {a.Summary, b.Summary}, {a.RunID, b.RunID}, {a.LogPath, b.LogPath}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return acc.entries, acc.notices
}

func loadHistorySnapshotEvidence(db *sql.DB, view historyOverview, acc *historyEvidenceAccumulator) {
	query := `SELECT s.run_id,s.time_ns,s.probe,s.status,
 CASE WHEN length(CAST(s.payload AS BLOB))<=? THEN s.payload ELSE '' END,length(CAST(s.payload AS BLOB)),r.hostname,
 CASE WHEN length(CAST(r.payload AS BLOB))<=? THEN r.payload ELSE '' END
 FROM snapshot_results s JOIN runs r ON r.id=s.run_id
 WHERE r.command='snapshot' AND (s.time_ns>=? OR r.ended_ns>=?) AND (s.probe='events.window' OR (s.probe LIKE 'events.%' AND s.status='skip') OR s.probe LIKE 'changes.%')`
	// Snapshot ends are reconstructed from DurationMS, which truncates sub-millisecond collection time.
	args := []any{historyEvidenceMaxPayloadBytes, historyEvidenceRunBytes, view.From.UnixNano(), view.From.Add(-historyEvidenceCapturePrecision).UnixNano()}
	if view.Run != nil {
		query += " AND r.hostname=?"
		args = append(args, view.Run.Hostname)
	}
	query += " ORDER BY s.time_ns,s.id LIMIT ?"
	args = append(args, historyEvidenceMaxResults+1)
	rows, err := db.Query(query, args...)
	if err != nil {
		acc.notice("snapshots_unavailable", "snapshot", "", err.Error())
		return
	}
	defer rows.Close()
	count, total := 0, 0
	for rows.Next() {
		var runID, probe, status, payload, host, runPayload string
		var capturedNS int64
		var bytes int
		if err := rows.Scan(&runID, &capturedNS, &probe, &status, &payload, &bytes, &host, &runPayload); err != nil {
			acc.notice("snapshots_unavailable", "snapshot", "", err.Error())
			return
		}
		count++
		if count > historyEvidenceMaxResults {
			acc.notice("results_limit", "snapshot", "", fmt.Sprint(historyEvidenceMaxResults))
			break
		}
		captured := time.Unix(0, capturedNS).UTC()
		var run historyRun
		if json.Unmarshal([]byte(runPayload), &run) == nil {
			if since, err := time.ParseDuration(run.Since); err == nil && since > 0 && !captured.Add(-since).Before(view.To) {
				continue
			}
		}
		if bytes > historyEvidenceMaxPayloadBytes {
			acc.notice("payload_limit", probe, runID, fmt.Sprint(historyEvidenceMaxPayloadBytes))
			continue
		}
		total += bytes + len(runPayload)
		if total > historyEvidenceMaxTotalBytes {
			acc.notice("total_bytes_limit", "snapshot", runID, fmt.Sprint(historyEvidenceMaxTotalBytes))
			break
		}
		var result struct {
			Summary  string          `json:"summary"`
			Metrics  json.RawMessage `json:"metrics"`
			Warnings []string        `json:"warnings"`
		}
		if json.Unmarshal([]byte(payload), &result) != nil {
			acc.notice("malformed", probe, runID, "")
			continue
		}
		if status == string(StatusSkip) || status == string(StatusFail) {
			acc.notice("source_unavailable", probe, runID, result.Summary)
			continue
		}
		if status != string(StatusPass) && status != string(StatusWarn) {
			acc.notice("malformed", probe, runID, status)
			continue
		}
		switch {
		case probe == "events.window":
			loadHistoryEventEvidence(result.Metrics, view, host, runID, captured, acc)
		case strings.HasPrefix(probe, "changes."):
			loadHistoryChangeEvidence(result.Metrics, view, host, runID, probe, captured, acc)
		}
		if len(result.Warnings) > 0 {
			acc.notice("stored_warning", probe, runID, strings.Join(result.Warnings, " · "))
		}
		if status == string(StatusWarn) {
			acc.notice("stored_warning", probe, runID, result.Summary)
		}
	}
	if err := rows.Err(); err != nil {
		acc.notice("snapshots_unavailable", "snapshot", "", err.Error())
	}
}

func loadHistoryEventEvidence(data json.RawMessage, view historyOverview, host, runID string, captured time.Time, acc *historyEvidenceAccumulator) {
	if len(data) == 0 || string(data) == "null" {
		acc.notice("legacy_events", "events.window", runID, "")
		return
	}
	var metrics struct {
		From      time.Time              `json:"window_from"`
		To        time.Time              `json:"window_to"`
		Timeline  *[]eventsTimelineEntry `json:"timeline"`
		Truncated int                    `json:"timeline_truncated"`
	}
	if json.Unmarshal(data, &metrics) != nil {
		acc.notice("malformed", "events.window", runID, "")
		return
	}
	if metrics.Timeline == nil {
		acc.notice("legacy_events", "events.window", runID, "")
		return
	}
	if metrics.From.IsZero() || metrics.To.IsZero() || metrics.To.Before(metrics.From) || metrics.Truncated < 0 {
		acc.notice("malformed", "events.window", runID, "")
		return
	}
	if !metrics.From.Before(view.To) || metrics.To.Before(view.From) {
		return
	}
	for _, event := range *metrics.Timeline {
		if event.Time.IsZero() || event.Kind == "" {
			acc.notice("malformed", "events.window", runID, "")
			continue
		}
		if !historyEvidenceInWindow(event.Time, view.From, view.To) {
			continue
		}
		if event.Time.Before(metrics.From) || event.Time.After(metrics.To) {
			acc.notice("malformed", "events.window", runID, "")
			continue
		}
		acc.add(historyOverviewEvidence{Time: event.Time, Source: "events.window", Kind: event.Kind, Subject: event.Subject, Summary: event.Summary, Host: host, HostUnknown: host == "", RunID: runID, CapturedAt: &captured})
	}
	if metrics.Truncated > 0 {
		acc.notice("snapshot_truncated", "events.window", runID, fmt.Sprint(metrics.Truncated))
	}
}

func loadHistoryChangeEvidence(data json.RawMessage, view historyOverview, host, runID, probe string, captured time.Time, acc *historyEvidenceAccumulator) {
	if len(data) == 0 || string(data) == "null" {
		acc.notice("legacy_changes", probe, runID, "")
		return
	}
	var metrics struct {
		Events *[]changeEvent `json:"events"`
	}
	if json.Unmarshal(data, &metrics) != nil {
		acc.notice("malformed", probe, runID, "")
		return
	}
	if metrics.Events == nil {
		acc.notice("legacy_changes", probe, runID, "")
		return
	}
	for _, event := range *metrics.Events {
		if event.Time.IsZero() || event.Kind == "" {
			acc.notice("malformed", probe, runID, "")
			continue
		}
		if !historyEvidenceInWindow(event.Time, view.From, view.To) {
			continue
		}
		summary := event.Name
		if event.Detail != "" {
			summary += " · " + event.Detail
		}
		acc.add(historyOverviewEvidence{Time: event.Time, Source: probe, Kind: event.Kind, Subject: event.Source, Summary: summary, Host: host, HostUnknown: host == "", RunID: runID, CapturedAt: &captured})
	}
}

func historyOverviewEvidenceLines(view historyOverview, loc *time.Location) []string {
	if view.Evidence == nil && view.EvidenceNotices == nil {
		return nil
	}
	lines := []string{"", T("history.evidence.title"), T("history.evidence.time_only")}
	if len(view.Evidence) == 0 {
		lines = append(lines, T("history.evidence.empty"))
	}
	for _, entry := range view.Evidence {
		host := entry.Host
		if entry.HostUnknown || host == "" {
			host = T("history.evidence.host_unknown")
		}
		line := entry.Time.In(loc).Format(time.RFC3339Nano) + "  [" + historyTerminalText(host) + "] " + historyTerminalText(entry.Source) + " / " + historyTerminalText(entry.Kind) + "  " + historyTerminalText(entry.Summary)
		if entry.Overlap && entry.End != nil {
			line += " · " + T("history.evidence.overlap", entry.End.In(loc).Format(time.RFC3339Nano))
		}
		lines = append(lines, line)
	}
	for _, notice := range view.EvidenceNotices {
		source := historyTerminalText(notice.Source)
		if notice.RunID != "" {
			source += " / " + historyTerminalText(notice.RunID)
		}
		message := T("history.evidence.notice." + notice.Code)
		if notice.Detail != "" {
			message += " · " + historyTerminalText(notice.Detail)
		}
		if source != "" {
			message = "[" + source + "] " + message
		}
		lines = append(lines, "  "+message)
	}
	return lines
}
