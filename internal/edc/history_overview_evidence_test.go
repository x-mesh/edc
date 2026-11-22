package edc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyEvidenceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openHistoryDB(filepath.Join(t.TempDir(), "history.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addHistoryEvidenceSnapshot(t *testing.T, db *sql.DB, id, host string, captured time.Time, results ...Result) {
	t.Helper()
	run := historyRun{ID: id, Command: historySnapshotCommand, Hostname: host, StartedAt: captured, Since: "6h"}
	payload, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO runs(id,command,hostname,started_ns,status,payload) VALUES (?,?,?,?,?,?)", id, historySnapshotCommand, host, captured.UnixNano(), "finished", string(payload)); err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		payload, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO snapshot_results(run_id,time_ns,probe,status,payload) VALUES (?,?,?,?,?)", id, captured.UnixNano(), result.Probe, result.Status, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
}

func evidenceWindow(at time.Time, signals ...eventsSignal) Result {
	return eventsWindow(eventsFacts{Start: at.Add(-time.Hour), Now: at, Since: time.Hour, Signals: signals})
}

func hasHistoryEvidenceNotice(notices []historyEvidenceNotice, code string) bool {
	for _, notice := range notices {
		if notice.Code == code {
			return true
		}
	}
	return false
}

func TestHistoryOverviewEvidenceMatchesActualTimeAndHost(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Minute)
	signal := eventsSignal{Time: from.Add(time.Minute).In(time.FixedZone("KST", 9*3600)), Kind: "oom", Subject: "worker", Brief: "worker killed"}
	capture := to.Add(30 * time.Minute)
	addHistoryEvidenceSnapshot(t, db, "later", "db-01", capture, evidenceWindow(capture, signal))
	addHistoryEvidenceSnapshot(t, db, "other-host", "db-02", capture, evidenceWindow(capture, signal))
	addHistoryEvidenceSnapshot(t, db, "change", "db-01", capture, Result{Probe: "changes.etc", Status: StatusPass, Metrics: map[string]interface{}{"events": []changeEvent{{Time: from.Add(2 * time.Minute), Kind: "etc", Name: "/etc/app.conf", Detail: "modified", Source: "mtime"}}}})
	view := historyOverview{From: from, To: to, Run: &historyRun{ID: "selected", Hostname: "db-01"}}
	entries, notices := loadHistoryOverviewEvidence(db, view, nil, nil)
	if len(entries) != 2 || len(notices) != 0 {
		t.Fatalf("entries=%+v notices=%+v", entries, notices)
	}
	if entries[0].RunID != "later" || !entries[0].Time.Equal(signal.Time.UTC()) || entries[0].CapturedAt == nil || !entries[0].CapturedAt.Equal(capture) || entries[1].Source != "changes.etc" {
		t.Fatalf("event time/capture/source lost: %+v", entries)
	}
	for _, entry := range entries {
		if entry.Host != "db-01" || entry.HostUnknown {
			t.Fatalf("mixed host: %+v", entry)
		}
	}
	view.Run = nil
	entries, _ = loadHistoryOverviewEvidence(db, view, nil, nil)
	if len(entries) != 3 {
		t.Fatalf("multi-host entries: %+v", entries)
	}
}

func TestHistoryOverviewEvidenceDeduplicatesSnapshots(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	signal := eventsSignal{Time: from.Add(time.Minute), Kind: "link", Subject: "eth0", Brief: "link down"}
	for i := 0; i < 2; i++ {
		at := from.Add(time.Duration(i+2) * time.Minute)
		addHistoryEvidenceSnapshot(t, db, fmt.Sprint(i), "host", at, evidenceWindow(at, signal))
	}
	entries, _ := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if len(entries) != 1 || entries[0].RunID != "0" {
		t.Fatalf("duplicate snapshots not collapsed: %+v", entries)
	}
}

func TestHistoryOverviewEvidenceHandlesLegacyMalformedAndSkipped(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	captured := from.Add(time.Minute)
	addHistoryEvidenceSnapshot(t, db, "legacy", "host", captured, Result{Probe: "events.window", Status: StatusPass, Evidence: []Evidence{{Label: "timeline", Value: "2026-11-23 10:00:00 local time is not a timestamp"}}})
	addHistoryEvidenceSnapshot(t, db, "skip", "host", captured, Result{Probe: "changes.etc", Status: StatusSkip, Summary: "permission denied"})
	addHistoryEvidenceSnapshot(t, db, "broken", "host", captured, Result{Probe: "changes.packages", Status: StatusPass})
	if _, err := db.Exec("UPDATE snapshot_results SET payload='broken JSON' WHERE run_id='broken'"); err != nil {
		t.Fatal(err)
	}
	entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if len(entries) != 0 {
		t.Fatalf("fabricated evidence: %+v", entries)
	}
	for _, code := range []string{"legacy_events", "source_unavailable", "malformed"} {
		if !hasHistoryEvidenceNotice(notices, code) {
			t.Fatalf("missing %s: %+v", code, notices)
		}
	}
	_, notices = loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour), Run: &historyRun{ID: "unknown"}}, nil, nil)
	if !hasHistoryEvidenceNotice(notices, "run_host_unknown") {
		t.Fatalf("unknown run host inferred: %+v", notices)
	}
	if _, err := db.Exec("DROP TABLE snapshot_results"); err != nil {
		t.Fatal(err)
	}
	entries, notices = loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if len(entries) != 0 || !hasHistoryEvidenceNotice(notices, "snapshots_unavailable") {
		t.Fatalf("missing table became observed empty: %+v", notices)
	}
}

func TestHistoryOverviewEvidenceBoundsAndBoundary(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	to := from.Add(10 * time.Minute)
	events := []changeEvent{{Time: from.Add(-time.Nanosecond), Kind: "etc", Name: "before"}, {Time: from, Kind: "etc", Name: "included"}, {Time: to, Kind: "etc", Name: "excluded"}}
	for i := 0; i < historyEvidenceMaxEntries+1; i++ {
		events = append(events, changeEvent{Time: from.Add(time.Duration(i+1) * time.Second), Kind: "etc", Name: fmt.Sprint(i)})
	}
	captured := to.Add(time.Minute)
	addHistoryEvidenceSnapshot(t, db, "many", "host", captured, Result{Probe: "changes.etc", Status: StatusPass, Metrics: map[string]interface{}{"events": events}})
	entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: to}, nil, nil)
	if len(entries) != historyEvidenceMaxEntries || !hasHistoryEvidenceNotice(notices, "entries_limit") || entries[0].Summary != "included" {
		t.Fatalf("entry bound: %d %+v", len(entries), notices)
	}
	for _, entry := range entries {
		if !historyEvidenceInWindow(entry.Time, from, to) || entry.Summary == "excluded" || entry.Summary == "before" {
			t.Fatalf("window boundary: %+v", entry)
		}
	}
	if _, err := db.Exec("UPDATE snapshot_results SET payload=? WHERE run_id='many'", strings.Repeat("界", historyEvidenceMaxPayloadBytes/3+1)); err != nil {
		t.Fatal(err)
	}
	entries, notices = loadHistoryOverviewEvidence(db, historyOverview{From: from, To: to}, nil, nil)
	if len(entries) != 0 || !hasHistoryEvidenceNotice(notices, "payload_limit") {
		t.Fatalf("byte limit: %+v %+v", entries, notices)
	}
}

func TestHistoryOverviewEvidenceLogOverlapIsUnscoped(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	to := from.Add(time.Minute)
	logs := []logHistoryAttempt{
		{Command: []string{"overlap"}, Started: from.Add(-time.Minute), Duration: 2 * time.Minute, Outcome: historyOutcomeSuccess},
		{Command: []string{"ended-at-boundary"}, Started: from.Add(-time.Minute), Duration: time.Minute},
		{Command: []string{"unknown-end"}, Started: from.Add(-time.Minute)},
		{Command: []string{"starts-at-end"}, Started: to, Duration: time.Minute},
	}
	entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: to, Run: &historyRun{Hostname: "remote-host"}}, logs, nil)
	if len(entries) != 1 || entries[0].Summary != "overlap" || !entries[0].HostUnknown || entries[0].Host != "" || !entries[0].Overlap {
		t.Fatalf("log correlation guessed identity: %+v", entries)
	}
	if !hasHistoryEvidenceNotice(notices, "logs_host_unknown") {
		t.Fatalf("missing log identity disclaimer: %+v", notices)
	}
	data, _ := json.Marshal(entries[0])
	if !strings.Contains(string(data), `"host_unknown":true`) || !strings.Contains(string(data), `"overlap":true`) {
		t.Fatalf("JSON conceals scope: %s", data)
	}
}

func TestHistoryOverviewEvidenceRowAndTotalByteLimits(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	capture := from.Add(time.Minute)
	addHistoryEvidenceSnapshot(t, db, "rows", "host", capture)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < historyEvidenceMaxResults+1; i++ {
		if _, err := tx.Exec("INSERT INTO snapshot_results(run_id,time_ns,probe,status,payload) VALUES (?,?,?,?,?)", "rows", capture.UnixNano(), "changes.etc", "pass", `{"metrics":{"events":[]}}`); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if !hasHistoryEvidenceNotice(notices, "results_limit") {
		t.Fatalf("missing row limit: %+v", notices)
	}
	if _, err := db.Exec("DELETE FROM snapshot_results"); err != nil {
		t.Fatal(err)
	}
	payload := `{"metrics":{"events":[]},"padding":"` + strings.Repeat("x", historyEvidenceMaxPayloadBytes-64) + `"}`
	for i := 0; i < 5; i++ {
		if _, err := db.Exec("INSERT INTO snapshot_results(run_id,time_ns,probe,status,payload) VALUES (?,?,?,?,?)", "rows", capture.UnixNano(), "changes.etc", "pass", payload); err != nil {
			t.Fatal(err)
		}
	}
	_, notices = loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if !hasHistoryEvidenceNotice(notices, "total_bytes_limit") {
		t.Fatalf("missing total byte limit: %+v", notices)
	}
}

func TestHistoryOverviewEvidenceRetainsSkippedEventSources(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	captured := from.Add(time.Minute)
	addHistoryEvidenceSnapshot(t, db, "partial", "host", captured,
		evidenceWindow(captured),
		Result{Probe: "events.units", Status: StatusSkip, Summary: "journal unavailable"},
		Result{Probe: "events.oom", Status: StatusFail, Summary: "OOM was observed"},
	)
	entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if len(entries) != 0 || len(notices) != 1 || notices[0].Code != "source_unavailable" || notices[0].Source != "events.units" {
		t.Fatalf("lost coverage or treated health failure as collection failure: %+v %+v", entries, notices)
	}
}

func TestHistoryOverviewEvidencePrefersTargetHostOverUnscopedLogs(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	captured := from.Add(30 * time.Minute)
	addHistoryEvidenceSnapshot(t, db, "target-oom", "db-01", captured, evidenceWindow(captured, eventsSignal{Time: from.Add(time.Minute), Kind: "oom", Subject: "postgres", Brief: "postgres killed"}))
	logs := make([]logHistoryAttempt, historyEvidenceMaxEntries)
	for i := range logs {
		logs[i] = logHistoryAttempt{Started: from.Add(time.Duration(i) * time.Second), Command: []string{fmt.Sprintf("job-%d", i)}, Path: fmt.Sprintf("/archive/%d.log", i)}
	}
	entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour), Run: &historyRun{Hostname: "db-01"}}, logs, nil)
	found := false
	for _, entry := range entries {
		if entry.RunID == "target-oom" && entry.Kind == "oom" && entry.Host == "db-01" && !entry.HostUnknown {
			found = true
		}
	}
	if !found || len(entries) != historyEvidenceMaxEntries || !hasHistoryEvidenceNotice(notices, "entries_limit") {
		t.Fatalf("unscoped archive displaced target-host evidence: %+v %+v", entries, notices)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatal("priority changed final chronological order")
		}
	}
}

func TestHistoryOverviewEvidenceNoticesPreserveRunAndDetail(t *testing.T) {
	acc := newHistoryEvidenceAccumulator()
	for _, notice := range []historyEvidenceNotice{
		{Code: "source_unavailable", Source: "events.units", RunID: "run-a", Detail: "permission denied"},
		{Code: "source_unavailable", Source: "events.units", RunID: "run-b", Detail: "journal missing"},
		{Code: "source_unavailable", Source: "events.units", RunID: "run-a", Detail: "journal rotated"},
		{Code: "source_unavailable", Source: "events.units", RunID: "run-a", Detail: "permission denied"},
	} {
		acc.notice(notice.Code, notice.Source, notice.RunID, notice.Detail)
	}
	if len(acc.notices) != 3 || len(acc.noticeKeys) != 3 {
		t.Fatalf("distinct provenance was collapsed: %+v", acc.notices)
	}
	expected := map[string]bool{"run-a/permission denied": false, "run-b/journal missing": false, "run-a/journal rotated": false}
	for _, notice := range acc.notices {
		expected[notice.RunID+"/"+notice.Detail] = true
	}
	for identity, found := range expected {
		if !found {
			t.Fatalf("missing %s: %+v", identity, acc.notices)
		}
	}
	for i := 0; i < 2; i++ {
		acc.notice("entries_limit", "", "", "200")
	}
	if len(acc.notices) != 4 {
		t.Fatalf("global notice was duplicated: %+v", acc.notices)
	}
}

func TestHistoryOverviewEvidenceNoticesRemainBoundedByCandidateBudget(t *testing.T) {
	db := historyEvidenceDB(t)
	from := time.Unix(1000, 0).UTC()
	capture := from.Add(time.Minute)
	addHistoryEvidenceSnapshot(t, db, "rows", "host", capture)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < historyEvidenceMaxResults+1; i++ {
		payload, err := json.Marshal(Result{Probe: "events.units", Status: StatusSkip, Summary: fmt.Sprintf("distinct failure %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO snapshot_results(run_id,time_ns,probe,status,payload) VALUES (?,?,?,?,?)", "rows", capture.UnixNano(), "events.units", "skip", string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Hour)}, nil, nil)
	if len(notices) != historyEvidenceMaxResults+1 || !hasHistoryEvidenceNotice(notices, "results_limit") {
		t.Fatalf("candidate bound not preserved: %d", len(notices))
	}
}

func TestHistoryOverviewEvidenceIncludesCollectionAcrossWindowStart(t *testing.T) {
	from := time.Unix(1000, 0).UTC()
	for _, tc := range []struct {
		name                                   string
		capture, event, collectedTo, storedEnd time.Time
		want                                   int
	}{
		{"collection crosses start", from.Add(-time.Second), from.Add(500 * time.Millisecond), from.Add(time.Second), from.Add(2 * time.Second), 1},
		{"millisecond end quantization", from.Add(-time.Second - 300*time.Microsecond), from.Add(100 * time.Microsecond), from.Add(300 * time.Microsecond), from.Add(-300 * time.Microsecond), 1},
		{"collection ended before start", from.Add(-2 * time.Second), from.Add(-1500 * time.Millisecond), from.Add(-time.Second), from.Add(-time.Second), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := historyEvidenceDB(t)
			window := eventsWindow(eventsFacts{Start: from.Add(-time.Hour), Now: tc.collectedTo, Since: time.Hour, Signals: []eventsSignal{{Time: tc.event, Kind: "oom", Brief: "observed during collection"}}})
			addHistoryEvidenceSnapshot(t, db, "straddles-start", "host", tc.capture, window)
			addHistoryEvidenceSnapshot(t, db, "other-host", "elsewhere", tc.capture, window)
			if _, err := db.Exec("UPDATE runs SET ended_ns=?", tc.storedEnd.UnixNano()); err != nil {
				t.Fatal(err)
			}
			entries, notices := loadHistoryOverviewEvidence(db, historyOverview{From: from, To: from.Add(time.Minute), Run: &historyRun{Hostname: "host"}}, nil, nil)
			if len(entries) != tc.want || len(notices) != 0 {
				t.Fatalf("boundary evidence: %+v %+v", entries, notices)
			}
			if tc.want > 0 && (entries[0].Host != "host" || !entries[0].Time.Equal(tc.event) || entries[0].CapturedAt == nil || !entries[0].CapturedAt.Equal(tc.capture)) {
				t.Fatalf("time or host identity changed: %+v", entries)
			}
		})
	}
}
