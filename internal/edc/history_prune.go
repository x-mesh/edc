package edc

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// history prune은 기준보다 오래된 top과 snapshot 기록을 지운다. 기준은 실행의 마지막 활동 시각(시작, 종료, 마지막 표본 중
// 가장 늦은 것)이라, 오래전에 시작해 아직 기록 중인 top -w는 지우지 않는다. 행을 지워도 파일은 줄지 않으므로 다시 쓸 수 있는
// 공간을 알리고, --vacuum일 때만 파일을 줄인다.

type historyPruneOptions struct {
	olderThan           time.Duration
	command             string
	dryRun, yes, vacuum bool
}

type historyPrunePlan struct {
	runs          []string
	top, snapshot int
	samples       int
}

// historyRunTables는 runs에 딸린 표다. 외래 키가 켜져 있어 이 표의 행을 runs보다 먼저 지운다. 스냅샷 표가 없는 DB는
// 앞의 두 표만 있다.
var historyRunTables = []string{"top_process_samples", "top_samples", "snapshot_results", "snapshot_metrics", "du_dirs"}

func runHistoryPrune(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	set := newFlagSet("history prune")
	set.SetOutput(stderr)
	var options historyPruneOptions
	age := set.String("older-than", "", T("history.option.older_than"))
	set.StringVar(&options.command, "command", "", T("history.option.prune_command"))
	set.BoolVar(&options.dryRun, "dry-run", false, T("history.option.dry_run"))
	set.BoolVar(&options.yes, "yes", false, T("history.option.yes"))
	set.BoolVar(&options.vacuum, "vacuum", false, T("history.option.vacuum"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	var err error
	options.olderThan, err = parseLogAge(*age)
	if err != nil || set.NArg() > 1 || options.command != "" && options.command != "top" && options.command != historySnapshotCommand {
		fmt.Fprintln(stderr, T("history.prune.usage"))
		return 2
	}
	path := set.Arg(0)
	if path == "" {
		if path, err = defaultHistoryPath(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	// openHistoryDB의 쓰기 모드는 없는 파일을 만들기 때문에, 지울 기록이 없는 경로에 빈 DB를 남기지 않도록 먼저 확인한다.
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, T("history.error.read", err))
		return 1
	}
	db, err := openHistoryDB(path, !options.dryRun)
	if err != nil {
		fmt.Fprintln(stderr, T("history.error.read", err))
		return 1
	}
	defer db.Close()
	plan, err := planHistoryPrune(db, options, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(plan.runs) == 0 {
		fmt.Fprintln(stdout, T("history.prune.none"))
		return 0
	}
	header := "history.prune.plan"
	if options.dryRun {
		header = "history.prune.dry_plan"
	}
	fmt.Fprintln(stdout, T(header, len(plan.runs), plan.top, plan.snapshot, plan.samples))
	if options.dryRun {
		fmt.Fprintln(stdout, T("history.prune.dry_note"))
		return 0
	}
	if !options.yes {
		input, inputOK := stdin.(*os.File)
		output, outputOK := stdout.(*os.File)
		if !inputOK || !outputOK || !isTerminal(input) || !isTerminal(output) {
			fmt.Fprintln(stderr, T("history.prune.yes_required"))
			return 2
		}
		fmt.Fprint(stdout, T("history.prune.confirm"))
		answer, _ := bufio.NewReader(input).ReadString('\n')
		if strings.TrimSpace(strings.ToLower(answer)) != "y" {
			return 0
		}
	}
	if err := deleteHistoryRuns(db, plan.runs); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, T("history.prune.done", len(plan.runs), plan.samples))
	free, err := historyFreeBytes(db)
	if err == nil && !options.vacuum && free > 0 {
		fmt.Fprintln(stdout, T("history.prune.reusable", formatSizeShort(free)))
	}
	if options.vacuum {
		before, _ := os.Stat(path)
		// VACUUM은 DB를 새로 써서 그 크기만큼 여유 공간이 필요하고, 끝날 때까지 다른 기록은 busy_timeout만큼 기다린다.
		if _, err := db.Exec("VACUUM"); err != nil {
			fmt.Fprintln(stderr, T("history.prune.vacuum_failed", err))
			return 1
		}
		if after, err := os.Stat(path); err == nil && before != nil {
			fmt.Fprintln(stdout, T("history.prune.vacuumed", formatSizeShort(uint64(before.Size())), formatSizeShort(uint64(after.Size()))))
		}
	}
	return 0
}

// planHistoryPrune은 마지막 활동 시각이 기준보다 이른 실행을 고른다.
func planHistoryPrune(db *sql.DB, options historyPruneOptions, now time.Time) (historyPrunePlan, error) {
	tables, err := historyPresentTables(db)
	if err != nil {
		return historyPrunePlan{}, err
	}
	activity := "max(r.started_ns, coalesce(r.ended_ns, 0), coalesce((SELECT max(time_ns) FROM top_samples WHERE run_id = r.id), 0)"
	if tables["snapshot_results"] {
		activity += ", coalesce((SELECT max(time_ns) FROM snapshot_results WHERE run_id = r.id), 0)"
	}
	activity += ")"
	query := "SELECT r.id, r.command FROM runs r WHERE " + activity + " < ?"
	values := []any{now.Add(-options.olderThan).UnixNano()}
	if options.command != "" {
		query += " AND r.command = ?"
		values = append(values, options.command)
	}
	rows, err := db.Query(query+" ORDER BY r.started_ns", values...)
	if err != nil {
		return historyPrunePlan{}, err
	}
	defer rows.Close()
	var plan historyPrunePlan
	for rows.Next() {
		var id, command string
		if err := rows.Scan(&id, &command); err != nil {
			return historyPrunePlan{}, err
		}
		plan.runs = append(plan.runs, id)
		if command == historySnapshotCommand {
			plan.snapshot++
		} else {
			plan.top++
		}
	}
	if err := rows.Err(); err != nil {
		return historyPrunePlan{}, err
	}
	for _, table := range []string{"top_samples", "top_process_samples", "snapshot_results"} {
		if !tables[table] || len(plan.runs) == 0 {
			continue
		}
		count := 0
		if err := db.QueryRow("SELECT count(*) FROM "+table+" WHERE run_id IN (SELECT r.id FROM runs r WHERE "+activity+" < ?"+historyPruneCommandFilter(options)+")", values...).Scan(&count); err != nil {
			return historyPrunePlan{}, err
		}
		plan.samples += count
	}
	return plan, nil
}

func historyPruneCommandFilter(options historyPruneOptions) string {
	if options.command == "" {
		return ""
	}
	return " AND r.command = ?"
}

func historyPresentTables(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM sqlite_schema WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

// deleteHistoryRuns는 실행과 딸린 행을 한 transaction으로 지운다. 중간에 실패하면 아무것도 지워지지 않는다.
func deleteHistoryRuns(db *sql.DB, runs []string) error {
	tables, err := historyPresentTables(db)
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range runs {
		for _, table := range historyRunTables {
			if !tables[table] {
				continue
			}
			if _, err := tx.Exec("DELETE FROM "+table+" WHERE run_id = ?", id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM runs WHERE id = ?", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func historyFreeBytes(db *sql.DB) (uint64, error) {
	var pages, size uint64
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&pages); err != nil {
		return 0, err
	}
	if err := db.QueryRow("PRAGMA page_size").Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}
