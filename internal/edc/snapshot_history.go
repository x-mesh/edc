package edc

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// 스냅샷 기록은 top 기록과 같은 history.db에 둔다. 결과를 뺀 보고서를 runs.payload에, 결과를 snapshot_results에 나눠 넣어
// 같은 보고서를 다시 만들 수 있게 한다. 기간 통계를 위해 결과의 숫자 지표와 du의 큰 디렉터리는 따로 펼쳐 둔다.

const historySnapshotCommand = "snapshot"

// snapshotPaths는 여러 번 줄 수 있는 --du 값이다.
type snapshotPaths []string

func (paths *snapshotPaths) String() string { return strings.Join(*paths, ",") }

func (paths *snapshotPaths) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New(T("snapshot.error.du_empty"))
	}
	*paths = append(*paths, value)
	return nil
}

// snapshotDuProbe는 du 결과의 이름이다. 경로가 하나면 이전과 같은 du.scan으로 두어 예전 스냅샷과 그대로 비교된다.
func snapshotDuProbe(paths []string, path string) string {
	if len(paths) <= 1 {
		return duProbeID
	}
	return duProbeID + ":" + path
}

func isDuProbe(probe string) bool {
	return probe == duProbeID || strings.HasPrefix(probe, duProbeID+":")
}

// recordSnapshot은 보고서 하나를 한 transaction으로 기록한다. 중간에 실패하면 아무것도 남지 않는다.
func recordSnapshot(path string, report Report, since time.Duration, duPaths []string) error {
	db, err := openHistoryDB(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	envelope := report
	envelope.Results = nil
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	started := report.Run.StartedAt.UTC()
	ended := started.Add(time.Duration(report.Run.DurationMS) * time.Millisecond)
	hostname, _ := report.Host["hostname"].(string)
	run := historyRun{ID: report.Run.ID, Command: historySnapshotCommand, Hostname: hostname, StartedAt: started, EndedAt: &ended,
		Status: "finished", SampleCount: len(report.Results), Version: report.Tool.Version, Since: since.String(), DuPaths: duPaths, Report: envelopeJSON}
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO runs(id, command, hostname, started_ns, ended_ns, status, sample_count, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		run.ID, run.Command, run.Hostname, started.UnixNano(), ended.UnixNano(), run.Status, run.SampleCount, string(payload)); err != nil {
		return err
	}
	at := started.UnixNano()
	for _, result := range report.Results {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO snapshot_results(run_id, time_ns, probe, status, payload) VALUES (?, ?, ?, ?, ?)",
			run.ID, at, result.Probe, string(result.Status), string(data)); err != nil {
			return err
		}
		// 같은 process에서 만든 지표는 uint64 같은 Go 값이라 파일에서 읽은 보고서와 모양이 다르다. JSON으로 한 번 맞춰 report diff와
		// 같은 값을 남긴다.
		var decoded struct {
			Metrics map[string]interface{} `json:"metrics"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return err
		}
		for key, value := range decoded.Metrics {
			number, ok := metricNumber(value)
			if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
				continue
			}
			if _, err := tx.Exec("INSERT INTO snapshot_metrics(run_id, time_ns, probe, key, value) VALUES (?, ?, ?, ?, ?)", run.ID, at, result.Probe, key, number); err != nil {
				return err
			}
		}
		if !isDuProbe(result.Probe) {
			continue
		}
		root, _ := decoded.Metrics["path"].(string)
		list, ok := duLargestOf(decoded.Metrics)
		if !ok || root == "" {
			continue
		}
		for _, dir := range list.Dirs {
			if _, err := tx.Exec("INSERT INTO du_dirs(run_id, time_ns, root, path, bytes) VALUES (?, ?, ?, ?, ?)", run.ID, at, root, dir.Path, int64(dir.Bytes)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// historyHasSnapshots는 스냅샷 표가 있는 DB인지 본다. 버전 1 DB는 읽기로 열면 그대로라 표가 없다.
func historyHasSnapshots(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='snapshot_results'").Scan(&count)
	return count > 0, err
}

// exportSnapshot은 기록한 스냅샷을 edc snapshot이 파일로 쓰는 것과 같은 보고서로 다시 만든다.
func exportSnapshot(db *sql.DB, runID string) (Report, error) {
	has, err := historyHasSnapshots(db)
	if err != nil {
		return Report{}, err
	}
	if !has {
		return Report{}, errors.New(T("history.error.no_snapshots"))
	}
	var command, payload string
	err = db.QueryRow("SELECT command, payload FROM runs WHERE id=?", runID).Scan(&command, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, errors.New(T("history.error.no_run", runID))
	}
	if err != nil {
		return Report{}, err
	}
	if command != historySnapshotCommand {
		return Report{}, errors.New(T("history.error.not_snapshot", runID, command))
	}
	var run historyRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(run.Report, &report); err != nil {
		return Report{}, err
	}
	rows, err := db.Query("SELECT payload FROM snapshot_results WHERE run_id=? ORDER BY id", runID)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	report.Results = []Result{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return Report{}, err
		}
		var result Result
		if err := json.Unmarshal([]byte(data), &result); err != nil {
			return Report{}, err
		}
		report.Results = append(report.Results, result)
	}
	return report, rows.Err()
}

// runHistoryExport는 edc history export --run <id> [--output 파일] [DB]다. 꺼낸 보고서는 edc report diff에 그대로 쓸 수 있다.
func runHistoryExport(args []string) int {
	set := newFlagSet("history export")
	set.SetOutput(os.Stderr)
	runIDValue := set.String("run", "", T("history.option.run"))
	output := set.String("output", "-", T("history.option.output"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	// --run이 없으면 터미널에서 기록된 스냅샷을 고르게 한다. 보고서를 stdout에 쓰는 경우도 화면이 끝난 뒤에 쓴다.
	_, canPrompt := historyCanPrompt()
	if set.NArg() > 1 || (*runIDValue == "" && !canPrompt) || *output == "" {
		fmt.Fprintln(os.Stderr, T("history.usage"))
		return 2
	}
	path := set.Arg(0)
	if path == "" {
		var err error
		if path, err = defaultHistoryPath(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if sameHistoryPath(path, *output) {
		fmt.Fprintln(os.Stderr, T("history.error.path"))
		return 2
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("history.error.read", err))
		return 1
	}
	defer db.Close()
	if *runIDValue == "" {
		id, ok := promptHistoryExport(db, args)
		if !ok {
			fmt.Fprintln(os.Stderr, T("history.usage"))
			return 2
		}
		*runIDValue = id
	}
	report, err := exportSnapshot(db, *runIDValue)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := writeJSONOutput(*output, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
