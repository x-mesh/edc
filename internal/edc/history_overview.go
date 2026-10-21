package edc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type historyOverviewOptions struct {
	run      string
	from, to time.Time
}

var errHistoryOverviewDisjoint = errors.New("history overview range does not overlap run samples")

type historyOverviewStat struct {
	Metric string  `json:"metric"`
	Avg    float64 `json:"avg"`
	P95    float64 `json:"p95"`
	Max    float64 `json:"max"`
}
type historyOverviewProcess struct {
	Command  string  `json:"command"`
	Peak     float64 `json:"peak_memory_bytes"`
	CPU      float64 `json:"avg_cpu_pct"`
	Restarts int     `json:"restarts"`
}
type historyOverviewDisk struct {
	Mount       string   `json:"mount"`
	UsedBytes   float64  `json:"used_bytes"`
	TotalBytes  float64  `json:"total_bytes"`
	BytesPerDay *float64 `json:"bytes_per_day,omitempty"`
}
type historyOverviewLog struct {
	Command  string        `json:"command"`
	Started  time.Time     `json:"started"`
	Outcome  string        `json:"outcome"`
	Duration time.Duration `json:"duration"`
	Key      string
	Path     string
}
type historyOverview struct {
	Run          *historyRun              `json:"run,omitempty"`
	From         time.Time                `json:"from"`
	To           time.Time                `json:"to"`
	Runs         int                      `json:"runs"`
	SnapshotRuns int                      `json:"snapshot_runs"`
	Samples      int                      `json:"samples"`
	Memory       string                   `json:"memory_source"`
	Metrics      []historyOverviewStat    `json:"metrics"`
	Processes    []historyOverviewProcess `json:"processes"`
	Series       map[string][]float64     `json:"series,omitempty"`
	Disks        []historyOverviewDisk    `json:"disks"`
	Logs         []historyOverviewLog     `json:"logs"`
	Warnings     []string                 `json:"warnings,omitempty"`
}

var historyOverviewLogLoader = func() (logHistorySnapshot, error) {
	return loadLogHistory(logHistoryOptions{}, nil)
}
var historyOverviewTempChmod = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }

func historyOverviewMetricLabel(name string) string {
	key := map[string]string{
		"cpu_pct":                "cpu",
		"memory_pct":             "memory",
		"load1":                  "load",
		"disk_read_bytes_per_s":  "disk_read",
		"disk_write_bytes_per_s": "disk_write",
		"net_in_bytes_per_s":     "net_in",
		"net_out_bytes_per_s":    "net_out",
		"swap_out_bytes_per_s":   "swap_out",
	}[name]
	if key == "" {
		return name
	}
	return T("history.overview.metric." + key)
}

func historyOverviewMemoryLabel(source string) string {
	switch strings.ToLower(source) {
	case "memory":
		return T("history.overview.memory")
	case "rss":
		return T("history.overview.rss")
	default:
		return source
	}
}

func runHistoryOverview(args []string) int {
	set := newFlagSet("history overview")
	set.SetOutput(os.Stderr)
	var options historyOverviewOptions
	set.StringVar(&options.run, "run", "", T("history.option.run"))
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	jsonPath := set.String("json", "", T("history.option.json"))
	if err := set.Parse(args); err != nil || set.NArg() > 1 {
		fmt.Fprintln(os.Stderr, T("history.usage"))
		return 2
	}
	var err error
	options.from, err = parseHistoryTime(*from)
	if err == nil {
		options.to, err = parseHistoryTime(*to)
	}
	if err != nil || (!options.from.IsZero() && !options.to.IsZero() && !options.from.Before(options.to)) {
		fmt.Fprintln(os.Stderr, T("history.error.time"))
		return 2
	}
	path := set.Arg(0)
	if path == "" {
		path, err = defaultHistoryPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("history.error.read", err))
		return 1
	}
	defer db.Close()
	view, err := loadHistoryOverview(db, options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, errHistoryOverviewDisjoint) {
			return 2
		}
		return 1
	}
	if *jsonPath != "" {
		if *jsonPath == "-" {
			if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			return 0
		}
		file, err := os.CreateTemp(filepath.Dir(*jsonPath), ".edc-overview-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		temp := file.Name()
		defer os.Remove(temp)
		err = historyOverviewTempChmod(file, 0600)
		if err == nil {
			err = json.NewEncoder(file).Encode(view)
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(temp, *jsonPath)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if liveTerminal() && os.Getenv("NO_COLOR") == "" {
		if err := runHistoryOverviewBrowser(db, path, options); err != nil {
			fmt.Fprintln(os.Stderr, T("history.error.read", err))
			return 1
		}
		return 0
	}
	writeHistoryOverview(os.Stdout, view, time.Local)
	return 0
}

func loadHistoryOverview(db *sql.DB, options historyOverviewOptions) (historyOverview, error) {
	out := historyOverview{Disks: []historyOverviewDisk{}, Logs: []historyOverviewLog{}}
	if options.run == "" && options.from.IsZero() && options.to.IsZero() {
		options.to = time.Now().UTC()
		options.from = options.to.Add(-24 * time.Hour)
	}
	where, args := "", []any{}
	if options.run != "" {
		where, args = " WHERE id=?", []any{options.run}
	} else {
		var conditions []string
		if !options.from.IsZero() {
			conditions, args = append(conditions, "time_ns>=?"), append(args, options.from.UnixNano())
		}
		if !options.to.IsZero() {
			conditions, args = append(conditions, "time_ns<?"), append(args, options.to.UnixNano())
		}
		if len(conditions) > 0 {
			where = " WHERE id IN (SELECT DISTINCT run_id FROM top_samples WHERE " + strings.Join(conditions, " AND ") + ")"
		}
	}
	rows, err := db.Query("SELECT payload,started_ns,ended_ns,sample_count FROM runs"+where+" ORDER BY started_ns", args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var s int64
		var e sql.NullInt64
		var n int
		if err := rows.Scan(&p, &s, &e, &n); err != nil {
			return out, err
		}
		var run historyRun
		if err := json.Unmarshal([]byte(p), &run); err != nil {
			return out, err
		}
		run.StartedAt = time.Unix(0, s).UTC()
		if e.Valid {
			ended := time.Unix(0, e.Int64).UTC()
			run.EndedAt = &ended
		}
		if out.Run == nil {
			if options.run != "" {
				out.Run = &run
			}
			out.From = time.Unix(0, s).UTC()
			out.To = out.From
		}
		out.Runs++
		out.Samples += n
		if e.Valid {
			out.To = time.Unix(0, e.Int64).UTC()
		}
	}
	if out.Runs == 0 {
		return out, fmt.Errorf("%s", T("history.overview.empty"))
	}
	requestedFrom, requestedTo := options.from, options.to
	boundCondition, boundValues := "", []any{}
	if options.run != "" {
		boundCondition = " WHERE run_id=?"
		boundValues = append(boundValues, options.run)
	}
	var first, last sql.NullInt64
	if err := db.QueryRow("SELECT MIN(time_ns), MAX(time_ns) FROM top_samples"+boundCondition, boundValues...).Scan(&first, &last); err != nil {
		return out, err
	}
	if options.run == "" && !requestedFrom.IsZero() {
		out.From = requestedFrom
	} else if first.Valid {
		out.From = time.Unix(0, first.Int64).UTC()
	} else if !requestedFrom.IsZero() {
		out.From = requestedFrom
	}
	if options.run == "" && !requestedTo.IsZero() {
		out.To = requestedTo
	} else if last.Valid {
		out.To = time.Unix(0, last.Int64).UTC().Add(time.Nanosecond)
	} else if !requestedTo.IsZero() {
		out.To = requestedTo
	}
	if !requestedFrom.IsZero() && requestedFrom.After(out.From) {
		out.From = requestedFrom
	}
	if !requestedTo.IsZero() && requestedTo.Before(out.To) {
		out.To = requestedTo
	}
	if !out.From.Before(out.To) {
		return out, fmt.Errorf("%w: %s", errHistoryOverviewDisjoint, T("history.overview.disjoint", time.Unix(0, first.Int64).UTC().Format(time.RFC3339), time.Unix(0, last.Int64).UTC().Add(time.Nanosecond).Format(time.RFC3339), requestedFrom.Format(time.RFC3339), requestedTo.Format(time.RFC3339)))
	}
	condition, vals := "time_ns>=? AND time_ns<?", []any{out.From.UnixNano(), out.To.UnixNano()}
	if options.run != "" {
		condition = "run_id=? AND " + condition
		vals = append([]any{options.run}, vals...)
	}
	if err := db.QueryRow("SELECT COUNT(DISTINCT run_id), COUNT(*) FROM top_samples WHERE "+condition, vals...).Scan(&out.Runs, &out.Samples); err != nil {
		return out, err
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM runs WHERE command='snapshot' AND started_ns>=? AND started_ns<?", out.From.UnixNano(), out.To.UnixNano()).Scan(&out.SnapshotRuns); err != nil {
		return out, err
	}
	for _, m := range []string{"cpu_pct", "memory_pct", "load1", "disk_read_bytes_per_s", "disk_write_bytes_per_s", "net_in_bytes_per_s", "net_out_bytes_per_s", "swap_out_bytes_per_s"} {
		var count int
		var average, maximum sql.NullFloat64
		if err := db.QueryRow("SELECT COUNT("+m+"), AVG("+m+"), MAX("+m+") FROM top_samples WHERE "+condition+" AND "+m+" IS NOT NULL", vals...).Scan(&count, &average, &maximum); err != nil {
			return out, err
		}
		if count == 0 {
			continue
		}
		rank := int(math.Ceil(.95*float64(count))) - 1
		var p95 float64
		p95Args := append(append([]any{}, vals...), rank)
		if err := db.QueryRow("SELECT "+m+" FROM top_samples WHERE "+condition+" AND "+m+" IS NOT NULL ORDER BY "+m+" LIMIT 1 OFFSET ?", p95Args...).Scan(&p95); err != nil {
			return out, err
		}
		out.Metrics = append(out.Metrics, historyOverviewStat{m, average.Float64, p95, maximum.Float64})
		if out.Series == nil {
			out.Series = map[string][]float64{}
		}
		if options.run != "" || !options.from.IsZero() || !options.to.IsZero() {
			seriesRows, err := db.Query("SELECT "+m+" FROM top_samples WHERE "+condition+" AND "+m+" IS NOT NULL ORDER BY time_ns LIMIT 120", vals...)
			if err != nil {
				return out, err
			}
			for seriesRows.Next() {
				var value float64
				if err := seriesRows.Scan(&value); err != nil {
					seriesRows.Close()
					return out, err
				}
				out.Series[m] = append(out.Series[m], value)
			}
			seriesRows.Close()
		}
	}
	memory := "rss_bytes"
	out.Memory = "RSS"
	if historyHasProcessMemoryColumn(db) {
		memory = "COALESCE(memory_bytes,rss_bytes)"
		out.Memory = "memory"
	}
	r, err := db.Query("SELECT MIN(command),MAX("+memory+"),AVG(cpu_pct),COUNT(DISTINCT started)-1 FROM top_process_samples WHERE "+condition+" GROUP BY command_folded ORDER BY MAX("+memory+") DESC LIMIT 10", vals...)
	if err != nil {
		return out, err
	}
	defer r.Close()
	for r.Next() {
		var p historyOverviewProcess
		if err := r.Scan(&p.Command, &p.Peak, &p.CPU, &p.Restarts); err != nil {
			return out, err
		}
		out.Processes = append(out.Processes, p)
	}
	if disks, err := historyGrowthDisks(db, out.From, out.To); err == nil {
		for _, disk := range disks {
			out.Disks = append(out.Disks, historyOverviewDisk{Mount: disk.Mount, UsedBytes: disk.UsedBytes, TotalBytes: disk.TotalBytes, BytesPerDay: disk.BytesPerDay})
		}
	} else {
		out.Warnings = append(out.Warnings, err.Error())
	}
	if logs, err := historyOverviewLogLoader(); err == nil {
		for _, row := range logs.Rows {
			if row.Started.Before(out.From) || !row.Started.Before(out.To) {
				continue
			}
			out.Logs = append(out.Logs, historyOverviewLog{Command: strings.Join(row.Command, " "), Started: row.Started, Outcome: historyOutcome(row), Duration: row.Duration, Key: row.Key, Path: row.Path})
		}
	} else {
		out.Warnings = append(out.Warnings, err.Error())
	}
	return out, r.Err()
}
func writeHistoryOverview(out io.Writer, v historyOverview, loc *time.Location) {
	if v.Run != nil {
		duration := v.To.Sub(v.From)
		if v.Run.EndedAt != nil {
			duration = v.Run.EndedAt.Sub(v.Run.StartedAt)
		}
		fmt.Fprintf(out, "%s: %s  %s  %s  %s %s  %s %.0fs  %s %s\n", T("history.overview.run"), v.Run.ID, v.Run.Command, v.Run.Hostname, T("history.overview.duration"), historyShortDuration(duration), T("history.overview.interval"), v.Run.IntervalS, T("history.overview.filter"), emptyAs(v.Run.Process, "—"))
	}
	fmt.Fprintf(out, "%s: %s → %s  %s %d  %s %d  %s %d\n", T("history.overview.range"), v.From.In(loc).Format(time.DateTime), v.To.In(loc).Format(time.DateTime), T("history.overview.top_runs"), v.Runs, T("history.overview.samples"), v.Samples, T("history.overview.snapshot_runs"), v.SnapshotRuns)
	for _, warning := range v.Warnings {
		fmt.Fprintln(out, warning)
	}
	fmt.Fprintln(out, T("history.overview.metrics"))
	fmt.Fprintf(out, "%-14s %12s %12s %12s\n", T("history.overview.column.metric"), T("history.overview.column.avg"), T("history.overview.column.p95"), T("history.overview.column.max"))
	for _, m := range v.Metrics {
		fmt.Fprintf(out, "%-14s %12s %12s %12s\n", historyOverviewMetricLabel(m.Metric), historyTrendValue(m.Metric, m.Avg), historyTrendValue(m.Metric, m.P95), historyTrendValue(m.Metric, m.Max))
	}
	fmt.Fprintf(out, "%s:\n", T("history.overview.processes", historyOverviewMemoryLabel(v.Memory)))
	fmt.Fprintf(out, "%-32s %12s %12s %12s\n", T("history.overview.column.name"), T("history.overview.column.peak"), T("history.overview.column.avg_cpu"), T("history.overview.column.restarts"))
	for _, p := range v.Processes {
		fmt.Fprintf(out, "%-32s %12s %12.1f%% %12d\n", topProcessName(p.Command, 32), formatSizeShort(uint64(p.Peak)), p.CPU, p.Restarts)
	}
	if len(v.Disks) > 0 {
		fmt.Fprintln(out, T("history.overview.disks"))
		fmt.Fprintln(out, historyOverviewDiskTableHeader())
		for _, disk := range v.Disks {
			growth := "—"
			if disk.BytesPerDay != nil {
				growth = historyGrowthRate(*disk.BytesPerDay)
			}
			fmt.Fprintln(out, historyOverviewDiskRow(disk.Mount, formatSizeShort(uint64(disk.UsedBytes)), formatSizeShort(uint64(disk.TotalBytes)), growth))
		}
	}
	if len(v.Logs) > 0 {
		fmt.Fprintln(out, T("history.overview.logs"))
		fmt.Fprintln(out, T("history.overview.logs_time_only"))
		fmt.Fprintln(out, historyOverviewLogTableHeader())
		for _, log := range v.Logs {
			fmt.Fprintln(out, historyOverviewLogRow(log.Command, log.Started.In(loc).Format(time.DateTime), log.Outcome, historyShortDuration(log.Duration)))
		}
	}
}

const historyOverviewPlainWidth = 120

func historyOverviewDiskTableHeader() string {
	return historyCell(T("history.overview.column.path"), 66, false) + "  " + historyCell(T("history.overview.column.used"), 12, true) + "  " + historyCell(T("history.overview.column.total"), 12, true) + "  " + historyCell(T("history.overview.column.growth_per_day"), 22, true)
}

func historyOverviewDiskRow(path, used, total, growth string) string {
	return historyCell(path, 66, false) + "  " + historyCell(used, 12, true) + "  " + historyCell(total, 12, true) + "  " + historyCell(growth, 22, true)
}

func historyOverviewLogTableHeader() string {
	return historyCell(T("history.overview.column.command"), 58, false) + "  " + historyCell(T("history.overview.column.started"), 19, false) + "  " + historyCell(T("history.overview.column.result"), 14, false) + "  " + historyCell(T("history.overview.column.duration"), 12, true)
}

func historyOverviewLogRow(command, started, result, duration string) string {
	return historyCell(command, 58, false) + "  " + historyCell(started, 19, false) + "  " + historyCell(result, 14, false) + "  " + historyCell(duration, 12, true)
}

var _ = sort.Float64s
