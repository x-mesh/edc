package edc

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type historyOptions struct {
	run      string
	from, to time.Time
	process  topProcessFilter
	metric   string
	min, max *float64
	limit    int
}

func historyTerminalText(value string) string {
	return strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(traceEscapeText([]byte(value)))
}

func runHistory(args []string) int {
	if len(args) == 0 || (args[0] != "list" && args[0] != "top" && args[0] != "process") {
		fmt.Fprintln(os.Stderr, T("history.usage"))
		return 2
	}
	command := args[0]
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		printCommandHelp(os.Stdout, "history")
		return 0
	}
	set := flag.NewFlagSet("history "+command, flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	var options historyOptions
	set.StringVar(&options.run, "run", "", T("history.option.run"))
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	set.IntVar(&options.limit, "limit", 200, T("history.option.limit"))
	jsonPath := set.String("json", "", T("history.option.json"))
	var process, metric string
	var min, max float64
	if command != "list" {
		set.StringVar(&metric, "metric", "", T("history.option.metric"))
		set.Float64Var(&min, "min", 0, T("history.option.min"))
		set.Float64Var(&max, "max", 0, T("history.option.max"))
	}
	if command == "process" {
		set.StringVar(&process, "process", "", T("command.top.option.process"))
	}
	if err := set.Parse(args[1:]); err != nil {
		return 2
	}
	if set.NArg() > 1 || options.limit < 0 {
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
	options.process, err = parseTopProcessFilter(process)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.process_invalid"))
		return 2
	}
	set.Visit(func(value *flag.Flag) {
		switch value.Name {
		case "min":
			options.min = &min
		case "max":
			options.max = &max
		}
	})
	options.metric = metric
	metrics := historyHostMetrics
	if command == "process" {
		metrics = historyProcessMetrics
	}
	validMetric := false
	for _, candidate := range metrics {
		validMetric = validMetric || candidate.name == metric
	}
	if (metric != "" && (!validMetric || (options.min == nil && options.max == nil))) ||
		(metric == "" && (options.min != nil || options.max != nil)) ||
		(options.min != nil && !historyFinite(min)) || (options.max != nil && !historyFinite(max)) ||
		(options.min != nil && options.max != nil && min > max) {
		fmt.Fprintln(os.Stderr, T("history.error.metric", strings.Join(historyMetricNames(metrics), ", ")))
		return 2
	}
	path := set.Arg(0)
	if set.NArg() == 0 {
		path, err = defaultHistoryPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if sameHistoryPath(path, *jsonPath) {
		fmt.Fprintln(os.Stderr, T("history.error.path"))
		return 2
	}
	db, err := openHistoryDB(path, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, T("history.error.read", err))
		return 1
	}
	defer db.Close()
	output := io.Writer(os.Stdout)
	var file *os.File
	if *jsonPath != "" && *jsonPath != "-" {
		file, err = os.OpenFile(*jsonPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		output = file
	}
	err = printHistory(output, db, command, options, *jsonPath != "")
	if file != nil {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, T("history.error.read", err))
		return 1
	}
	return 0
}

func parseHistoryTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || at.Year() < 1678 || at.Year() > 2261 {
		return time.Time{}, fmt.Errorf("%s", T("history.error.time"))
	}
	return at.UTC(), nil
}

func historyQuery(command string, options historyOptions) (string, []any) {
	table, selection := "top_samples", "payload"
	timeColumn, runColumn := "time_ns", "run_id"
	if command == "list" {
		table, selection, timeColumn, runColumn = "runs", "payload, ended_ns, status, sample_count", "started_ns", "id"
	} else if command == "process" {
		table = "top_process_samples p JOIN top_samples s ON s.id=p.sample_id"
		selection, timeColumn, runColumn = "p.payload, s.payload", "p.time_ns", "p.run_id"
	}
	var conditions []string
	var values []any
	if options.run != "" {
		conditions = append(conditions, runColumn+"=?")
		values = append(values, options.run)
	}
	if !options.from.IsZero() {
		conditions = append(conditions, timeColumn+">=?")
		values = append(values, options.from.UnixNano())
	}
	if !options.to.IsZero() {
		conditions = append(conditions, timeColumn+"<?")
		values = append(values, options.to.UnixNano())
	}
	if command == "process" && options.process.active() {
		var matches []string
		var pids []int
		for pid := range options.process.pids {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		for _, pid := range pids {
			matches = append(matches, "p.pid=?")
			values = append(values, pid)
		}
		for _, name := range options.process.names {
			matches = append(matches, "instr(p.command_folded, ?)>0")
			values = append(values, name)
		}
		conditions = append(conditions, "("+strings.Join(matches, " OR ")+")")
	}
	metric := options.metric
	if command == "process" {
		metric = "p." + metric
	}
	if options.min != nil {
		conditions = append(conditions, metric+">=?")
		values = append(values, *options.min)
	}
	if options.max != nil {
		conditions = append(conditions, metric+"<=?")
		values = append(values, *options.max)
	}
	query := "SELECT " + selection + " FROM " + table
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	id := "id"
	if command == "process" {
		id = "p.id"
	}
	query += " ORDER BY " + timeColumn + " DESC, " + id + " DESC"
	if options.limit > 0 {
		query += " LIMIT ?"
		values = append(values, options.limit)
	}
	return query, values
}

type historyProcessSample struct {
	topProcessSample
	RunID             string     `json:"run_id"`
	Time              time.Time  `json:"time"`
	Hostname          string     `json:"hostname"`
	WindowS           float64    `json:"window_s"`
	ProcessFilter     string     `json:"process_filter"`
	ProcessObservedAt *time.Time `json:"process_observed_at,omitempty"`
}

func printHistory(output io.Writer, db *sql.DB, command string, options historyOptions, jsonOutput bool) error {
	query, values := historyQuery(command, options)
	rows, err := db.Query(query, values...)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(output)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if !jsonOutput {
		header := "RUN\tTIME\tHOST\tCPU%\tMEM%\tLOAD\tPROCESS FILTER"
		if command == "list" {
			header = "RUN\tSTART\tEND\tHOST\tSTATUS\tSAMPLES\tINTERVAL\tPROCESS FILTER"
		} else if command == "process" {
			header = "RUN\tTIME\tPID\tSTARTED\tCPU%\tRSS\tCOMMAND"
		}
		if options.metric != "" {
			header += "\t" + options.metric
		}
		if _, err := fmt.Fprintln(writer, header); err != nil {
			return err
		}
	}
	for rows.Next() {
		var payload string
		var value any
		var line string
		switch command {
		case "list":
			var run historyRun
			var ended sql.NullInt64
			var status string
			var count int
			if err := rows.Scan(&payload, &ended, &status, &count); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(payload), &run); err != nil {
				return err
			}
			run.Status, run.SampleCount = status, count
			end := "-"
			if ended.Valid {
				at := time.Unix(0, ended.Int64).UTC()
				run.EndedAt, end = &at, at.Format(time.RFC3339Nano)
			}
			value = run
			line = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%d\t%gs\t%s", run.ID, run.StartedAt.Format(time.RFC3339Nano), end, historyTerminalText(run.Hostname), historyTerminalText(run.Status), count, run.IntervalS, historyTerminalText(run.Process))
		case "top":
			var sample historyTopSample
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(payload), &sample); err != nil {
				return err
			}
			value = sample
			line = fmt.Sprintf("%s\t%s\t%s\t%.2f\t%.2f\t%.2f\t%s", sample.RunID, sample.Time.Format(time.RFC3339Nano), historyTerminalText(sample.Hostname), sample.CPU, sample.MemoryPct, sample.Load1, historyTerminalText(sample.ProcessFilter))
		case "process":
			var process topProcessSample
			var sample historyTopSample
			var hostPayload string
			if err := rows.Scan(&payload, &hostPayload); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(payload), &process); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(hostPayload), &sample); err != nil {
				return err
			}
			value = historyProcessSample{topProcessSample: process, RunID: sample.RunID, Time: sample.Time, Hostname: sample.Hostname, WindowS: sample.WindowS, ProcessFilter: sample.ProcessFilter, ProcessObservedAt: sample.ProcessObservedAt}
			started := "-"
			if process.Started != nil {
				started = process.Started.Format(time.RFC3339Nano)
			}
			line = fmt.Sprintf("%s\t%s\t%d\t%s\t%.2f\t%s\t%s", sample.RunID, sample.Time.Format(time.RFC3339Nano), process.PID, started, process.CPU, formatBytes(process.RSSBytes), historyTerminalText(process.Command))
		}
		if jsonOutput {
			if err := encoder.Encode(value); err != nil {
				return err
			}
		} else {
			if options.metric != "" {
				line += "\t" + historyDisplayMetric(value, options.metric)
			}
			if _, err := fmt.Fprintln(writer, line); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !jsonOutput {
		return writer.Flush()
	}
	return nil
}

func historyDisplayMetric(value any, name string) string {
	var data any
	var metrics []historyMetric
	var supported func(string) bool
	switch sample := value.(type) {
	case historyTopSample:
		data, metrics, supported = sample.topSample, historyHostMetrics, sample.metricSupported
	case historyProcessSample:
		data, metrics = sample.topProcessSample, historyProcessMetrics
	}
	values := historyMetricValues(data, metrics, supported)
	for index, metric := range metrics {
		if metric.name == name && values[index] != nil {
			return fmt.Sprint(values[index])
		}
	}
	return "-"
}
