package edc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

// The run index is a cache of the metadata that edc reads from the command
// logs, so the history and prune are not limited by how many files they can
// parse on each run. The logs stay the source: edc log never opens the index,
// and every failure of the index falls back to reading the logs directly.
const (
	logIndexName      = ".edc-index.db"
	logIndexSchema    = 1
	logIndexRowLimit  = 100000
	logIndexBusyMS    = 200
	sqliteBusy        = 5
	sqliteCorrupt     = 11
	sqliteNotDatabase = 26
)

// logIndexBudget bounds one sync, including the parsing of changed logs. A
// first build of a large folder takes several syncs, and the history reads
// the logs directly until it is complete.
var logIndexBudget = 1500 * time.Millisecond

type logIndexState struct {
	fingerprint string
	rows        []logHistoryAttempt
	invalid     []string
}

// loadIndexedHistory returns the history from the index of the default log
// directory. ok is false when the caller must read the logs directly; notice
// then says why, unless the reason is that there is nothing to index.
func loadIndexedHistory(root string, extra []string, state *logIndexState) (snapshot logHistorySnapshot, notice string, ok bool) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return snapshot, "", false
	}
	families, err := scanHistoryRoot(root)
	if err != nil {
		return snapshot, T("cli.log_history.index_unavailable", reportIdentityValue(err.Error())), false
	}
	for _, path := range extra {
		if _, known := families[path]; !known {
			if parts, err := historyFamily(path); err == nil {
				families[path] = parts
			}
		}
	}
	var unreadable []string
	for path, parts := range families {
		if parts == nil {
			unreadable = append(unreadable, path)
		}
	}
	sort.Strings(unreadable)
	fingerprint := logIndexFingerprint(families)
	if state != nil && state.rows != nil && state.fingerprint == fingerprint {
		return logIndexSnapshot(state.rows, state.invalid, unreadable), "", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), logIndexBudget+time.Second)
	defer cancel()
	index, err := openLogIndex(ctx, filepath.Join(root, logIndexName))
	if err != nil {
		return snapshot, logIndexFailure(root, err), false
	}
	defer index.Close()
	done, total, err := syncLogIndex(ctx, index, families, time.Now().Add(logIndexBudget))
	if err != nil {
		return snapshot, logIndexFailure(root, err), false
	}
	if done < total {
		return snapshot, T("cli.log_history.index_building", done, total), false
	}
	rows, invalid, err := queryLogIndex(ctx, index)
	if err != nil {
		return snapshot, logIndexFailure(root, err), false
	}
	if state != nil {
		state.fingerprint, state.rows, state.invalid = fingerprint, rows, invalid
	}
	snapshot = logIndexSnapshot(rows, invalid, unreadable)
	if len(rows) > logIndexRowLimit {
		snapshot.Rows = snapshot.Rows[:logIndexRowLimit]
		snapshot.issue(root, "limit_error")
	}
	return snapshot, "", true
}

// logIndexSnapshot marks the running attempts again on every call: whether a
// log is still locked is never stored, because it changes without the file
// changing.
func logIndexSnapshot(rows []logHistoryAttempt, invalid, unreadable []string) logHistorySnapshot {
	snapshot := logHistorySnapshot{Rows: make([]logHistoryAttempt, len(rows))}
	copy(snapshot.Rows, rows)
	last := map[string]int{}
	for index, row := range snapshot.Rows {
		if current, seen := last[row.Path]; !seen || row.Ordinal > snapshot.Rows[current].Ordinal {
			last[row.Path] = index
		}
	}
	for path, index := range last {
		if snapshot.Rows[index].open && logFileLocked(path+".lock") {
			snapshot.Rows[index].Outcome = historyOutcomeRunning
		}
	}
	for _, path := range unreadable {
		snapshot.issue(path, "read_error")
	}
	for _, path := range invalid {
		snapshot.issue(path, "invalid_log")
	}
	return snapshot
}

// logIndexFailure turns an index error into the notice. A damaged index is
// moved aside, never repaired in place, and is built again on the next run;
// the logs are never touched from here.
func logIndexFailure(root string, err error) string {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		if code := sqliteErr.Code() & 0xff; code == sqliteCorrupt || code == sqliteNotDatabase {
			path := filepath.Join(root, logIndexName)
			moved := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
			if os.Rename(path, moved) == nil {
				_ = os.Remove(path + "-wal")
				_ = os.Remove(path + "-shm")
				return T("cli.log_history.index_corrupt", reportIdentityValue(moved))
			}
		}
	}
	return T("cli.log_history.index_unavailable", reportIdentityValue(firstLine(err.Error())))
}

func scanHistoryRoot(root string) (historyFamilies, error) {
	families := historyFamilies{}
	examined := 0
	err := scanHistoryDirectory(root, true, families, &examined, func(directory string) error {
		return scanHistoryDirectory(directory, false, families, &examined, nil)
	})
	return families, err
}

// logIndexSignature changes when any part of a family is replaced, grows, or
// is touched, which is what makes the stored rows stale.
func logIndexSignature(parts []historyPart) string {
	var builder strings.Builder
	for _, part := range parts {
		inode := uint64(0)
		if stat, ok := part.info.Sys().(*syscall.Stat_t); ok {
			inode = uint64(stat.Ino)
		}
		fmt.Fprintf(&builder, "%d:%d:%d:%d;", part.index, inode, part.info.Size(), part.info.ModTime().UnixNano())
	}
	return builder.String()
}

func logIndexFingerprint(families historyFamilies) string {
	paths := make([]string, 0, len(families))
	for path := range families {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		parts, _ := families.parts(path)
		fmt.Fprintf(hash, "%s\x00%s\x00", path, logIndexSignature(parts))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func openLogIndex(ctx context.Context, path string) (*sql.DB, error) {
	// The index holds command lines, which can carry secrets, so it gets the
	// mode of the logs. SQLite gives the -wal and -shm files the mode of this file.
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err == nil {
		file.Close()
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := url.Values{"mode": {"rwc"}, "_pragma": {"busy_timeout(" + strconv.Itoa(logIndexBusyMS) + ")"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initializeLogIndex(ctx, db); err != nil {
		db.Close()
		// The index is a cache, so a file from another schema is replaced, not migrated.
		if errors.Is(err, errLogIndexSchema) {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
			if file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err == nil {
				file.Close()
			}
			return openLogIndexFresh(ctx, uri.String())
		}
		return nil, err
	}
	return db, nil
}

var errLogIndexSchema = errors.New("history index schema mismatch")

func openLogIndexFresh(ctx context.Context, uri string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initializeLogIndex(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initializeLogIndex(ctx context.Context, db *sql.DB) error {
	var version, tables int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != logIndexSchema {
		return errLogIndexSchema
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if version == logIndexSchema {
		return nil
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return err
	}
	if tables != 0 {
		return errLogIndexSchema
	}
	_, err := db.ExecContext(ctx, `
CREATE TABLE families (path TEXT PRIMARY KEY, signature TEXT NOT NULL, invalid INTEGER NOT NULL);
CREATE TABLE runs (path TEXT NOT NULL, ordinal INTEGER NOT NULL, started INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (path, ordinal));
CREATE INDEX runs_started ON runs (started);
PRAGMA user_version = `+strconv.Itoa(logIndexSchema))
	return err
}

type logIndexParsed struct {
	path, signature string
	rows            []logHistoryAttempt
	invalid         bool
}

// syncLogIndex parses the families whose signature changed, newest first, until
// the deadline, and writes what it parsed in one transaction. done counts the
// families that are current afterwards.
func syncLogIndex(ctx context.Context, db *sql.DB, families historyFamilies, deadline time.Time) (done, total int, err error) {
	stored := map[string]string{}
	rows, err := db.QueryContext(ctx, "SELECT path, signature FROM families")
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var path, signature string
		if err := rows.Scan(&path, &signature); err != nil {
			rows.Close()
			return 0, 0, err
		}
		stored[path] = signature
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	var changed []string
	modified := map[string]time.Time{}
	for path, parts := range families {
		if parts == nil {
			continue
		}
		total++
		sorted, _ := families.parts(path)
		if stored[path] == logIndexSignature(sorted) {
			done++
			continue
		}
		changed = append(changed, path)
		modified[path] = families.modified(path)
	}
	sortHistoryNewestFirst(changed, modified)
	var parsed []logIndexParsed
	for _, path := range changed {
		if time.Now().After(deadline) {
			break
		}
		parts, _ := families.parts(path)
		familyRows, invalid, err := readHistoryFamily(path, parts)
		if errors.Is(err, errHistoryChanged) {
			// A log that changed while it was read is read again next time.
			continue
		}
		if err != nil {
			// Any other failure, such as the attempt limit, repeats on every
			// sync; recording the log as invalid keeps it from holding the
			// index unfinished forever.
			familyRows, invalid = nil, true
		}
		parsed = append(parsed, logIndexParsed{path: path, signature: logIndexSignature(parts), rows: familyRows, invalid: invalid})
	}
	var removed []string
	for path := range stored {
		if parts, ok := families[path]; !ok || parts == nil {
			removed = append(removed, path)
		}
	}
	if len(parsed) == 0 && len(removed) == 0 {
		return done, total, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return done, total, err
	}
	defer tx.Rollback()
	for _, path := range removed {
		if _, err := tx.ExecContext(ctx, "DELETE FROM runs WHERE path = ?", path); err != nil {
			return done, total, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM families WHERE path = ?", path); err != nil {
			return done, total, err
		}
	}
	for _, family := range parsed {
		if _, err := tx.ExecContext(ctx, "DELETE FROM runs WHERE path = ?", family.path); err != nil {
			return done, total, err
		}
		for _, row := range family.rows {
			data, err := json.Marshal(newLogIndexRow(row))
			if err != nil {
				return done, total, err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO runs (path, ordinal, started, data) VALUES (?, ?, ?, ?)", family.path, row.Ordinal, row.Started.UnixNano(), string(data)); err != nil {
				return done, total, err
			}
		}
		invalid := 0
		if family.invalid {
			invalid = 1
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO families (path, signature, invalid) VALUES (?, ?, ?) ON CONFLICT (path) DO UPDATE SET signature = excluded.signature, invalid = excluded.invalid", family.path, family.signature, invalid); err != nil {
			return done, total, err
		}
	}
	if err := tx.Commit(); err != nil {
		return done, total, err
	}
	return done + len(parsed), total, nil
}

func queryLogIndex(ctx context.Context, db *sql.DB) ([]logHistoryAttempt, []string, error) {
	rows, err := db.QueryContext(ctx, "SELECT data FROM runs ORDER BY started DESC, path, ordinal DESC LIMIT ?", logIndexRowLimit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var result []logHistoryAttempt
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, nil, err
		}
		var row logIndexRow
		if err := json.Unmarshal([]byte(data), &row); err != nil {
			return nil, nil, err
		}
		result = append(result, row.attempt())
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	invalidRows, err := db.QueryContext(ctx, "SELECT path FROM families WHERE invalid = 1 ORDER BY path")
	if err != nil {
		return nil, nil, err
	}
	defer invalidRows.Close()
	var invalid []string
	for invalidRows.Next() {
		var path string
		if err := invalidRows.Scan(&path); err != nil {
			return nil, nil, err
		}
		invalid = append(invalid, path)
	}
	return result, invalid, invalidRows.Err()
}

// logIndexRow is the stored form of an attempt. It names the unexported state
// that the history needs after a reload: a damaged record and an attempt that
// had no end marker when its log was read.
type logIndexRow struct {
	Key, CWD, Path, Display string
	Outcome                 logHistoryOutcome
	Command                 []string
	Started, Ended          time.Time
	Duration                time.Duration
	Exit                    *int
	Attempt, Ordinal        int
	Damaged, Open           bool
	Resources               *logIndexResources `json:",omitempty"`
}

type logIndexResources struct {
	CPU, CPUUser, CPUSys time.Duration
	MaxRSS               uint64
	Samples              int
	MemPeak, MemAvg      uint64
	CPUPeak              float64
	DiskRead, DiskWrite  uint64
	Scope, Container     string
	HasCPU, HasDisk      bool
}

func newLogIndexRow(row logHistoryAttempt) logIndexRow {
	stored := logIndexRow{Key: row.Key, CWD: row.CWD, Path: row.Path, Display: row.Display, Outcome: row.Outcome, Command: row.Command, Started: row.Started, Ended: row.Ended, Duration: row.Duration, Exit: row.Exit, Attempt: row.Attempt, Ordinal: row.Ordinal, Damaged: row.damaged, Open: row.open}
	if resources := row.Resources; resources != nil {
		stored.Resources = &logIndexResources{CPU: resources.CPU, CPUUser: resources.CPUUser, CPUSys: resources.CPUSys, MaxRSS: resources.MaxRSS, Samples: resources.Samples, MemPeak: resources.MemPeak, MemAvg: resources.MemAvg, CPUPeak: resources.CPUPeak, DiskRead: resources.DiskRead, DiskWrite: resources.DiskWrite, Scope: resources.Scope, Container: resources.Container, HasCPU: resources.cpu, HasDisk: resources.disk}
	}
	return stored
}

func (stored logIndexRow) attempt() logHistoryAttempt {
	row := logHistoryAttempt{Key: stored.Key, CWD: stored.CWD, Path: stored.Path, Display: stored.Display, Outcome: stored.Outcome, Command: stored.Command, Started: stored.Started, Ended: stored.Ended, Duration: stored.Duration, Exit: stored.Exit, Attempt: stored.Attempt, Ordinal: stored.Ordinal, damaged: stored.Damaged, open: stored.Open}
	if resources := stored.Resources; resources != nil {
		row.Resources = &logHistoryResources{CPU: resources.CPU, CPUUser: resources.CPUUser, CPUSys: resources.CPUSys, MaxRSS: resources.MaxRSS, Samples: resources.Samples, MemPeak: resources.MemPeak, MemAvg: resources.MemAvg, CPUPeak: resources.CPUPeak, DiskRead: resources.DiskRead, DiskWrite: resources.DiskWrite, Scope: resources.Scope, Container: resources.Container, cpu: resources.HasCPU, disk: resources.HasDisk}
	}
	return row
}
