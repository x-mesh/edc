package edc

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// The stats file keeps a daily summary of the runs that prune deletes, so a
// trend outlives the logs. Unlike the run index it cannot be rebuilt from the
// logs, so edc never deletes or replaces it: a file from another schema or a
// damaged one only turns the trend of archived days off.
const (
	logStatsName   = ".edc-stats.db"
	logStatsSchema = 1
)

var errLogStatsSchema = errors.New("history stats schema mismatch")

// logHistoryDay sums the runs of one command in one directory on one local
// day. Only values that add up across separate prunes of the same day are kept.
type logHistoryDay struct {
	Day                      string
	Runs, Failed, Successes  int
	DurationSum, DurationMax time.Duration
	MemoryMax                uint64
	CPUSum                   time.Duration
}

func (day *logHistoryDay) add(row logHistoryAttempt) {
	day.Runs++
	if row.failed() {
		day.Failed++
	}
	if row.Outcome == historyOutcomeSuccess && !row.Ended.IsZero() && !row.damaged {
		day.Successes++
		day.DurationSum += row.Duration
		if row.Duration > day.DurationMax {
			day.DurationMax = row.Duration
		}
	}
	if resources := row.Resources; resources != nil {
		if memory := resources.memory(); memory > day.MemoryMax {
			day.MemoryMax = memory
		}
		if resources.cpu {
			day.CPUSum += resources.CPU
		}
	}
}

type logStatsKey struct{ key, cwd, day string }

func logHistoryRollup(rows []logHistoryAttempt) map[logStatsKey]*logHistoryDay {
	days := map[logStatsKey]*logHistoryDay{}
	for _, row := range rows {
		if row.Key == "" || row.Started.IsZero() {
			continue
		}
		key := logStatsKey{row.Key, row.CWD, row.Started.Local().Format("2006-01-02")}
		day := days[key]
		if day == nil {
			day = &logHistoryDay{Day: key.day}
			days[key] = day
		}
		day.add(row)
	}
	return days
}

func openLogStats(ctx context.Context, root string) (*sql.DB, error) {
	path := filepath.Join(root, logStatsName)
	if file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err == nil {
		file.Close()
	}
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(" + strconv.Itoa(logIndexBusyMS) + ")"}}.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version, tables int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version == logStatsSchema {
		return db, nil
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil || version != 0 || tables != 0 {
		db.Close()
		if err != nil {
			return nil, err
		}
		return nil, errLogStatsSchema
	}
	if _, err := db.ExecContext(ctx, `
PRAGMA journal_mode=WAL;
CREATE TABLE daily (key TEXT NOT NULL, cwd TEXT NOT NULL, day TEXT NOT NULL, runs INTEGER NOT NULL, failed INTEGER NOT NULL, successes INTEGER NOT NULL, duration_sum INTEGER NOT NULL, duration_max INTEGER NOT NULL, memory_max INTEGER NOT NULL, cpu_sum INTEGER NOT NULL, PRIMARY KEY (key, cwd, day));
PRAGMA user_version = `+strconv.Itoa(logStatsSchema)); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// saveLogHistoryRollup adds the daily summary of runs that are about to be
// deleted. It runs before the deletion; if it fails, prune warns and still
// deletes, because the logs, not the summary, are what the user asked about.
func saveLogHistoryRollup(root string, rows []logHistoryAttempt) error {
	days := logHistoryRollup(rows)
	if len(days) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), logIndexBudget+time.Second)
	defer cancel()
	db, err := openLogStats(ctx, root)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, day := range days {
		_, err := tx.ExecContext(ctx, `INSERT INTO daily (key, cwd, day, runs, failed, successes, duration_sum, duration_max, memory_max, cpu_sum) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (key, cwd, day) DO UPDATE SET runs = runs + excluded.runs, failed = failed + excluded.failed, successes = successes + excluded.successes,
duration_sum = duration_sum + excluded.duration_sum, duration_max = max(duration_max, excluded.duration_max), memory_max = max(memory_max, excluded.memory_max), cpu_sum = cpu_sum + excluded.cpu_sum`,
			key.key, key.cwd, key.day, day.Runs, day.Failed, day.Successes, int64(day.DurationSum), int64(day.DurationMax), int64(day.MemoryMax), int64(day.CPUSum))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadLogHistoryArchive reads the summaries of deleted runs since a day, by
// command key. A missing file means there is nothing archived yet.
func loadLogHistoryArchive(root string, since time.Time) (map[string][]logHistoryDay, error) {
	if _, err := os.Stat(filepath.Join(root, logStatsName)); os.IsNotExist(err) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	db, err := openLogStats(ctx, root)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT key, day, runs, failed, successes, duration_sum, duration_max, memory_max, cpu_sum FROM daily WHERE day >= ? ORDER BY day", since.Local().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	archive := map[string][]logHistoryDay{}
	for rows.Next() {
		var key string
		var day logHistoryDay
		var sum, longest, memory, cpu int64
		if err := rows.Scan(&key, &day.Day, &day.Runs, &day.Failed, &day.Successes, &sum, &longest, &memory, &cpu); err != nil {
			return nil, err
		}
		day.DurationSum, day.DurationMax, day.MemoryMax, day.CPUSum = time.Duration(sum), time.Duration(longest), uint64(memory), time.Duration(cpu)
		archive[key] = append(archive[key], day)
	}
	return archive, rows.Err()
}
