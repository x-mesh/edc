package edc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

const historyApplicationID = 0x45444348
const historySchemaVersion = 1
const historyQueueSize = 16

func defaultHistoryPath() (string, error) {
	home, _ := os.UserHomeDir()
	path := defaultHistoryPathFor(runtime.GOOS, home, os.Getenv("XDG_STATE_HOME"))
	if path == "" {
		return "", fmt.Errorf("%s", T("history.error.default_path"))
	}
	return path, nil
}

func defaultHistoryPathFor(goos, home, stateHome string) string {
	if filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "edc", "history.db")
	}
	if home == "" {
		return ""
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "edc", "history.db")
	}
	return filepath.Join(home, ".local", "state", "edc", "history.db")
}

type historyMetric struct {
	name  string
	field int
	kind  reflect.Kind
}

func historyMetrics(value any, names []string) []historyMetric {
	typeOf := reflect.TypeOf(value)
	var metrics []historyMetric
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if !slices.Contains(names, name) {
			continue
		}
		kind := field.Type.Kind()
		if kind == reflect.Pointer {
			kind = field.Type.Elem().Kind()
		}
		if kind == reflect.Float64 || kind == reflect.Int || kind == reflect.Uint64 {
			metrics = append(metrics, historyMetric{name, index, kind})
		}
	}
	return metrics
}

var historyHostMetrics = append(historyMetrics(topSample{}, strings.Fields(`
cores net_in_bytes_per_s net_out_bytes_per_s packets_in_per_s packets_out_per_s
network_errors_per_s network_drops_per_s load1 cpu_user_pct cpu_system_pct cpu_iowait_pct
disk_read_bytes_per_s disk_write_bytes_per_s disk_iops disk_await_ms disk_busy_pct
psi_cpu_some_avg10_pct psi_memory_some_avg10_pct psi_io_some_avg10_pct memory_pct swap_out_bytes_per_s
`)), historyMetric{name: "cpu_pct", field: -1, kind: reflect.Float64})
var historyProcessMetrics = historyMetrics(topProcessSample{}, strings.Fields(`
pid cpu_pct rss_bytes threads fds disk_read_bytes_per_s disk_write_bytes_per_s
`))

func historyMetricColumns(metrics []historyMetric) string {
	var columns []string
	for _, metric := range metrics {
		kind := "INTEGER"
		if metric.kind == reflect.Float64 {
			kind = "REAL"
		}
		columns = append(columns, metric.name+" "+kind)
	}
	return strings.Join(columns, ", ")
}

func historyMetricNames(metrics []historyMetric) []string {
	names := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		names = append(names, metric.name)
	}
	return names
}

func historyMetricSupported(sample topSample, name string) bool {
	switch name {
	case "network_errors_per_s", "network_drops_per_s":
		return sample.NetHealth
	case "disk_iops", "disk_await_ms":
		return sample.DiskHealth
	case "disk_busy_pct":
		return sample.DiskBusyOK
	case "psi_cpu_some_avg10_pct", "psi_memory_some_avg10_pct", "psi_io_some_avg10_pct":
		return sample.PSIValid
	}
	return true
}

func historyMetricValues(value any, metrics []historyMetric, supported func(string) bool) []any {
	values := make([]any, 0, len(metrics))
	reflected := reflect.ValueOf(value)
	for _, metric := range metrics {
		if supported != nil && !supported(metric.name) {
			values = append(values, nil)
			continue
		}
		if metric.field == -1 {
			sample := value.(topSample)
			values = append(values, roundTopValue(sample.CPUUser+sample.CPUSystem))
			continue
		}
		field := reflected.Field(metric.field)
		if field.Kind() == reflect.Pointer {
			if field.IsNil() {
				values = append(values, nil)
				continue
			}
			field = field.Elem()
		}
		if metric.kind == reflect.Float64 {
			values = append(values, field.Float())
		} else if metric.kind == reflect.Uint64 {
			values = append(values, field.Uint())
		} else {
			values = append(values, field.Int())
		}
	}
	return values
}

type historyTopSample struct {
	topSample
	RunID             string     `json:"run_id"`
	WindowS           float64    `json:"window_s"`
	ProcessFilter     string     `json:"process_filter"`
	ProcessesValid    bool       `json:"processes_valid"`
	ProcessObservedAt *time.Time `json:"process_observed_at,omitempty"`
	CoreCPU           []float64  `json:"core_cpu_pct,omitempty"`
	CPU               float64    `json:"cpu_pct"`
	NetworkValid      bool       `json:"network_valid"`
	DiskValid         bool       `json:"disk_valid"`
	SwapValid         bool       `json:"swap_valid"`
}

func newHistoryTopSample(details hostDetails, previous resourceSnapshot, snapshot resourceSnapshot, rate resourceRate, filter string) historyTopSample {
	sample := historyTopSample{
		topSample: newTopSample(details, snapshot.TakenAt, rate),
		WindowS:   snapshot.TakenAt.Sub(previous.TakenAt).Seconds(), ProcessFilter: filter,
		ProcessesValid: snapshot.ProcessesValid, CoreCPU: append([]float64(nil), rate.CoreCPU...),
		NetworkValid: !previous.NetMissing && !snapshot.NetMissing,
		DiskValid:    !previous.DiskMissing && !snapshot.DiskMissing,
		SwapValid:    !previous.SwapMissing && !snapshot.SwapMissing,
	}
	sample.CPU = roundTopValue(sample.CPUUser + sample.CPUSystem)
	if snapshot.ProcessesValid {
		sample.Processes = newTopProcessSamples(snapshot.Processes)
		for index, process := range snapshot.Processes {
			if !process.Started.IsZero() {
				started := process.Started.UTC()
				(*sample.Processes)[index].Started = &started
			}
		}
		if filter != "" {
			sample.ProcessTotal = newTopProcessTotalSample(snapshot.ProcessTotal)
		}
		if !snapshot.ProcessesAt.IsZero() {
			at := snapshot.ProcessesAt.UTC()
			sample.ProcessObservedAt = &at
		}
	}
	return sample
}

func openHistoryDB(path string, write bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if path == "" || path == "-" {
		return nil, fmt.Errorf("%s", T("history.error.path"))
	}
	info, err := os.Stat(absolute)
	if write && errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(absolute, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NONBLOCK, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			info, err = os.Stat(absolute)
		} else if createErr != nil {
			return nil, createErr
		} else {
			info, err = file.Stat()
			err = errors.Join(err, file.Close())
		}
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s", T("history.error.regular"))
	}
	mode := "ro"
	if write {
		mode = "rw"
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(2000)", "foreign_keys(1)"}}
	if !write {
		query.Add("_pragma", "query_only(1)")
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initializeHistoryDB(db, write); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initializeHistoryDB(db *sql.DB, write bool) error {
	var application, version, tables int
	if err := db.QueryRow("PRAGMA application_id").Scan(&application); err != nil {
		return err
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if application == historyApplicationID && version == historySchemaVersion {
		if write {
			return historyEnableWAL(db)
		}
		return nil
	}
	if application != 0 || version != 0 || !write {
		return fmt.Errorf("%s", T("history.error.schema"))
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables != 0 {
		return fmt.Errorf("%s", T("history.error.schema"))
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE runs (id TEXT PRIMARY KEY, command TEXT NOT NULL, hostname TEXT NOT NULL, started_ns INTEGER NOT NULL, ended_ns INTEGER, status TEXT NOT NULL, sample_count INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL)`,
		`CREATE TABLE top_samples (id INTEGER PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), time_ns INTEGER NOT NULL, payload TEXT NOT NULL, ` + historyMetricColumns(historyHostMetrics) + `)`,
		`CREATE TABLE top_process_samples (id INTEGER PRIMARY KEY, sample_id INTEGER NOT NULL REFERENCES top_samples(id), run_id TEXT NOT NULL REFERENCES runs(id), time_ns INTEGER NOT NULL, started TEXT, command TEXT NOT NULL, command_folded TEXT NOT NULL, payload TEXT NOT NULL, ` + historyMetricColumns(historyProcessMetrics) + `)`,
		`CREATE INDEX runs_time ON runs(started_ns DESC)`,
		`CREATE INDEX top_time ON top_samples(time_ns DESC, id DESC)`,
		`CREATE INDEX top_run_time ON top_samples(run_id, time_ns DESC, id DESC)`,
		`CREATE INDEX process_time ON top_process_samples(time_ns DESC, id DESC)`,
		`CREATE INDEX process_run_time ON top_process_samples(run_id, time_ns DESC, id DESC)`,
		`CREATE INDEX process_identity ON top_process_samples(pid, started, time_ns DESC)`,
		fmt.Sprintf("PRAGMA application_id=%d", historyApplicationID),
		fmt.Sprintf("PRAGMA user_version=%d", historySchemaVersion),
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return historyEnableWAL(db)
}

func historyEnableWAL(db *sql.DB) error {
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("%s", T("history.error.wal"))
	}
	_, err := db.Exec("PRAGMA synchronous=FULL")
	return err
}

type historyRun struct {
	ID          string     `json:"run_id"`
	Command     string     `json:"command"`
	Hostname    string     `json:"hostname"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Status      string     `json:"status"`
	SampleCount int        `json:"sample_count"`
	Version     string     `json:"version"`
	System      string     `json:"system"`
	OS          string     `json:"os"`
	Cores       int        `json:"cores"`
	MemoryTotal uint64     `json:"memory_total_bytes"`
	IntervalS   float64    `json:"interval_s"`
	Process     string     `json:"process_filter"`
	Detail      bool       `json:"detail"`
}

type topRecorder struct {
	db       *sql.DB
	runID    string
	queue    chan []byte
	done     chan struct{}
	failed   chan struct{}
	mutex    sync.Mutex
	err      error
	finish   sync.Once
	closeErr error
}

func newTopRecorder(path, version string, details hostDetails, interval time.Duration, filter string, detail bool) (*topRecorder, error) {
	db, err := openHistoryDB(path, true)
	if err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	run := historyRun{ID: runID(started), Command: "top", Hostname: details.Hostname, StartedAt: started,
		Status: "unfinished", Version: version, System: details.System, OS: details.OS,
		Cores: details.Cores, MemoryTotal: details.MemoryTotal, IntervalS: interval.Seconds(), Process: filter, Detail: detail}
	payload, err := json.Marshal(run)
	if err == nil {
		_, err = db.Exec("INSERT INTO runs(id, command, hostname, started_ns, status, payload) VALUES (?, ?, ?, ?, ?, ?)", run.ID, run.Command, run.Hostname, started.UnixNano(), run.Status, string(payload))
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	recorder := &topRecorder{db: db, runID: run.ID, queue: make(chan []byte, historyQueueSize), done: make(chan struct{}), failed: make(chan struct{})}
	go recorder.writeLoop()
	return recorder, nil
}

func (recorder *topRecorder) Err() error {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return recorder.err
}

func (recorder *topRecorder) fail(err error) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.err == nil {
		recorder.err = err
		close(recorder.failed)
	}
}

func (recorder *topRecorder) Record(sample historyTopSample) error {
	if err := recorder.Err(); err != nil {
		return err
	}
	sample.RunID = recorder.runID
	payload, err := json.Marshal(sample)
	if err != nil {
		recorder.fail(err)
		return err
	}
	select {
	case recorder.queue <- payload:
		return recorder.Err()
	default:
		err := fmt.Errorf("%s", T("history.error.queue"))
		recorder.fail(err)
		return err
	}
}

func (recorder *topRecorder) writeLoop() {
	defer close(recorder.done)
	for payload := range recorder.queue {
		if err := recorder.Err(); err != nil {
			return
		}
		if err := recorder.writeSample(payload); err != nil {
			recorder.fail(err)
			return
		}
	}
}

func historyInsert(table string, columns []string) string {
	return "INSERT INTO " + table + "(" + strings.Join(columns, ", ") + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
}

func (recorder *topRecorder) writeSample(payload []byte) error {
	var sample historyTopSample
	if err := json.Unmarshal(payload, &sample); err != nil {
		return err
	}
	tx, err := recorder.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns := append([]string{"run_id", "time_ns", "payload"}, historyMetricNames(historyHostMetrics)...)
	values := append([]any{recorder.runID, sample.Time.UnixNano(), string(payload)}, historyMetricValues(sample.topSample, historyHostMetrics, sample.metricSupported)...)
	result, err := tx.Exec(historyInsert("top_samples", columns), values...)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if sample.Processes != nil {
		for _, process := range *sample.Processes {
			data, err := json.Marshal(process)
			if err != nil {
				return err
			}
			var started any
			if process.Started != nil {
				started = process.Started.Format(time.RFC3339Nano)
			}
			columns := append([]string{"sample_id", "run_id", "time_ns", "started", "command", "command_folded", "payload"}, historyMetricNames(historyProcessMetrics)...)
			values := append([]any{id, recorder.runID, sample.Time.UnixNano(), started, process.Command, strings.ToLower(process.Command), string(data)}, historyMetricValues(process, historyProcessMetrics, func(name string) bool {
				return (name != "threads" || process.Threads != 0) && (name != "fds" || process.FDs != 0)
			})...)
			if _, err := tx.Exec(historyInsert("top_process_samples", columns), values...); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec("UPDATE runs SET sample_count=sample_count+1 WHERE id=?", recorder.runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (recorder *topRecorder) Close(status string) error {
	recorder.finish.Do(func() {
		close(recorder.queue)
		<-recorder.done
		if recorder.Err() != nil {
			status = "error"
		}
		_, finishErr := recorder.db.Exec("UPDATE runs SET ended_ns=?, status=? WHERE id=?", time.Now().UTC().UnixNano(), status, recorder.runID)
		recorder.closeErr = errors.Join(recorder.Err(), finishErr, recorder.db.Close())
	})
	return recorder.closeErr
}

func historyFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (sample historyTopSample) metricSupported(name string) bool {
	switch name {
	case "net_in_bytes_per_s", "net_out_bytes_per_s", "packets_in_per_s", "packets_out_per_s":
		return sample.NetworkValid
	case "disk_read_bytes_per_s", "disk_write_bytes_per_s":
		return sample.DiskValid
	case "swap_out_bytes_per_s":
		return sample.SwapValid
	}
	return historyMetricSupported(sample.topSample, name)
}

func sameHistoryPath(database, output string) bool {
	if database == "" || output == "" || output == "-" {
		return false
	}
	canonical := func(path string) string {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return path
		}
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			return resolved
		}
		if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
			return filepath.Join(parent, filepath.Base(absolute))
		}
		return absolute
	}
	dbPath, outputPath := canonical(database), canonical(output)
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if path == outputPath {
			return true
		}
		dbInfo, dbErr := os.Stat(path)
		outInfo, outErr := os.Stat(output)
		if dbErr == nil && outErr == nil && os.SameFile(dbInfo, outInfo) {
			return true
		}
	}
	return false
}
