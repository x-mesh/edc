package edc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

type historyOverviewOptions struct {
	run      string
	from, to time.Time
}
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
	Mount       string
	UsedBytes   uint64
	TotalBytes  uint64
	BytesPerDay *float64
}
type historyOverviewLog struct {
	Command, Outcome string
	Started          time.Time
	Duration         time.Duration
}
type historyOverview struct {
	Run       *historyRun              `json:"run,omitempty"`
	From      time.Time                `json:"from"`
	To        time.Time                `json:"to"`
	Runs      int                      `json:"runs"`
	Samples   int                      `json:"samples"`
	Memory    string                   `json:"memory_source"`
	Metrics   []historyOverviewStat    `json:"metrics"`
	Processes []historyOverviewProcess `json:"processes"`
	Series    map[string][]float64     `json:"series,omitempty"`
	Disks     []historyOverviewDisk    `json:"disks"`
	Logs      []historyOverviewLog     `json:"logs"`
}

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
		fmt.Fprintln(os.Stderr, T("history.error.read", err))
		return 1
	}
	if *jsonPath != "" {
		var out io.Writer = os.Stdout
		if *jsonPath != "-" {
			file, e := os.OpenFile(*jsonPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				return 1
			}
			defer file.Close()
			out = file
		}
		json.NewEncoder(out).Encode(view)
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
	var out historyOverview
	where, args := "", []any{}
	if options.run != "" {
		where = " WHERE id=?"
		args = append(args, options.run)
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
	if !options.from.IsZero() {
		out.From = options.from
	}
	if !options.to.IsZero() {
		out.To = options.to
	}
	boundCondition, boundValues := "", []any{}
	if options.run != "" {
		boundCondition = " WHERE run_id=?"
		boundValues = append(boundValues, options.run)
	}
	var first, last sql.NullInt64
	if err := db.QueryRow("SELECT MIN(time_ns), MAX(time_ns) FROM top_samples"+boundCondition, boundValues...).Scan(&first, &last); err != nil {
		return out, err
	}
	if options.from.IsZero() && first.Valid {
		out.From = time.Unix(0, first.Int64).UTC()
	}
	if options.to.IsZero() && last.Valid {
		out.To = time.Unix(0, last.Int64).UTC().Add(time.Nanosecond)
	}
	condition, vals := "time_ns>=? AND time_ns<?", []any{out.From.UnixNano(), out.To.UnixNano()}
	if options.run != "" {
		condition = "run_id=? AND " + condition
		vals = append([]any{options.run}, vals...)
	}
	for _, m := range []string{"cpu_pct", "memory_pct", "load1", "disk_read_bytes_per_s", "disk_write_bytes_per_s", "net_in_bytes_per_s", "net_out_bytes_per_s", "swap_out_bytes_per_s"} {
		r, e := db.Query("SELECT "+m+" FROM top_samples WHERE "+condition+" AND "+m+" IS NOT NULL ORDER BY time_ns", vals...)
		if e != nil {
			return out, e
		}
		var values []float64
		for r.Next() {
			var v float64
			r.Scan(&v)
			values = append(values, v)
		}
		r.Close()
		if len(values) > 0 {
			if out.Series == nil {
				out.Series = map[string][]float64{}
			}
			out.Series[m] = values
			sorted := slices.Clone(values)
			sort.Float64s(sorted)
			sum := 0.0
			for _, v := range values {
				sum += v
			}
			out.Metrics = append(out.Metrics, historyOverviewStat{m, sum / float64(len(values)), sorted[int(float64(len(sorted)-1)*.95)], sorted[len(sorted)-1]})
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
	return out, r.Err()
}
func writeHistoryOverview(out io.Writer, v historyOverview, loc *time.Location) {
	if v.Run != nil {
		fmt.Fprintf(out, "%s: %s  %s  %s\n", T("history.overview.run"), v.Run.ID, v.Run.Command, v.Run.Hostname)
	}
	fmt.Fprintf(out, "%s: %s → %s  %s %d  %s %d\n", T("history.overview.range"), v.From.In(loc).Format(time.DateTime), v.To.In(loc).Format(time.DateTime), T("history.overview.runs"), v.Runs, T("history.overview.samples"), v.Samples)
	fmt.Fprintln(out, T("history.overview.metrics"))
	for _, m := range v.Metrics {
		fmt.Fprintf(out, "  %s  %s %s  p95 %s  %s %s\n", historyOverviewMetricLabel(m.Metric), T("history.overview.avg"), historyTrendValue(m.Metric, m.Avg), historyTrendValue(m.Metric, m.P95), T("history.overview.max"), historyTrendValue(m.Metric, m.Max))
	}
	fmt.Fprintf(out, "%s:\n", T("history.overview.processes", historyOverviewMemoryLabel(v.Memory)))
	for _, p := range v.Processes {
		fmt.Fprintf(out, "  %s  %s %s  %s %.1f%%  %s %d\n", historyTerminalText(p.Command), T("history.overview.peak"), formatSizeShort(uint64(p.Peak)), T("history.overview.cpu"), p.CPU, T("history.overview.restarts"), p.Restarts)
	}
	if len(v.Disks) > 0 {
		fmt.Fprintln(out, T("history.overview.disks"))
		for _, disk := range v.Disks {
			growth := "—"
			if disk.BytesPerDay != nil {
				growth = historyGrowthRate(*disk.BytesPerDay)
			}
			fmt.Fprintf(out, "  %s  %s/%s  %s %s\n", disk.Mount, formatSizeShort(uint64(disk.UsedBytes)), formatSizeShort(uint64(disk.TotalBytes)), T("history.overview.growth"), growth)
		}
	}
	if len(v.Logs) > 0 {
		fmt.Fprintln(out, T("history.overview.logs"))
		fmt.Fprintln(out, T("history.overview.logs_time_only"))
		for _, log := range v.Logs {
			fmt.Fprintf(out, "  %s  %s  %s  %s\n", historyTerminalText(log.Command), log.Started.In(loc).Format(time.DateTime), log.Outcome, log.Duration)
		}
	}
}

var _ = sort.Float64s
