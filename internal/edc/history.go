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
	sampleID int64
}

const historyDefaultLimit = 200

var historyLiveTerminal = liveTerminal
var historyListBrowserRunner = runHistoryListBrowser
var historyNavigatorRunner = func() error { return runHistoryNavigator(historyOptions{}) }
var historyScopedNavigatorRunner = runHistoryNavigatorWith

func historyTerminalText(value string) string {
	return strings.NewReplacer("\r", `\r`, "\n", `\n`, "\t", `\t`).Replace(traceEscapeText([]byte(value)))
}

func runHistory(args []string) int {
	if len(args) == 0 {
		if historyLiveTerminal() {
			if err := historyNavigatorRunner(); err != nil {
				fmt.Fprintln(os.Stderr, T("history.error.read", err))
				return 1
			}
			return 0
		}
		choice, ok := promptMissingSubcommand("history")
		if !ok {
			if !printCommandBrief(os.Stderr, "history") {
				fmt.Fprintln(os.Stderr, T("history.usage"))
			}
			return 2
		}
		args = []string{choice}
	}
	noSubcommand := len(args) > 0 && strings.HasPrefix(args[0], "-")
	if noSubcommand {
		args = append([]string{"list"}, args...)
	}
	// log is the same viewer as edc log history; it reads the command logs, not
	// this database, so none of the options below apply to it.
	if args[0] == "log" {
		return runLogWithStreams(append([]string{"history"}, args[1:]...), logStreams{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr})
	}
	if args[0] != "list" && args[0] != "top" && args[0] != "process" && args[0] != "export" && args[0] != "trend" && args[0] != "growth" && args[0] != "overview" && args[0] != "prune" {
		fmt.Fprintln(os.Stderr, T("history.usage"))
		return 2
	}
	command := args[0]
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		if command == "prune" {
			printHistoryPruneHelp(os.Stdout)
			return 0
		}
		printCommandHelp(os.Stdout, "history")
		return 0
	}
	if command == "export" {
		return runHistoryExport(args[1:])
	}
	if command == "trend" {
		return runHistoryTrend(args[1:])
	}
	if command == "growth" {
		return runHistoryGrowth(args[1:])
	}
	if command == "overview" {
		return runHistoryOverview(args[1:])
	}
	if command == "prune" {
		return runHistoryPrune(args[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	set := newFlagSet("history " + command)
	set.SetOutput(os.Stderr)
	var options historyOptions
	set.StringVar(&options.run, "run", "", T("history.option.run"))
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	set.IntVar(&options.limit, "limit", configuredInt(activeConfig.Defaults.History.Limit, historyDefaultLimit), T("history.option.limit"))
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
	if (command == "list" || command == "top" || command == "process") && *jsonPath == "" && historyLiveTerminal() {
		var err error
		switch command {
		case "list":
			if noSubcommand {
				err = historyScopedNavigatorRunner(db, path, options)
			} else {
				err = historyListBrowserRunner(db, path, options)
			}
		case "process":
			err = runHistoryProcessBrowser(db, path, options)
		default:
			err = runHistoryTopBrowser(db, path, options)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, T("history.error.read", err))
			return 1
		}
		return 0
	}
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
	if command == "process" && options.sampleID > 0 {
		conditions = append(conditions, "p.sample_id=?")
		values = append(values, options.sampleID)
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
	file, isFile := output.(*os.File)
	return writeHistory(output, db, command, options, jsonOutput, newTextStyle(!jsonOutput && isFile && richEnabled(file)))
}

// historyPaint는 색 표의 한 칸을 칠한다. nil이면 기본색이다.
type historyPaint func(string) string

// historyColorTable은 색을 쓸 때 모은 표다. tabwriter는 escape도 칸 폭으로 세므로, 색 표는 평문 칸으로 폭을 정한 뒤 칠한다.
type historyColorTable struct {
	style  textStyle
	rows   [][]string
	paints [][]historyPaint
}

func writeHistory(output io.Writer, db *sql.DB, command string, options historyOptions, jsonOutput bool, style textStyle) error {
	query, values := historyQuery(command, options)
	rows, err := db.Query(query, values...)
	if err != nil {
		return err
	}
	defer rows.Close()
	encoder := json.NewEncoder(output)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	table := historyColorTable{style: style}
	writeLine := func(line string, paints []historyPaint) error {
		if style.color {
			table.add(line, paints)
			return nil
		}
		_, err := fmt.Fprintln(writer, line)
		return err
	}
	if !jsonOutput {
		header := "RUN\tTIME\tHOST\tCPU%\tMEM%\tLOAD\tDISK READ/s\tDISK WRITE/s\tNET IN/s\tNET OUT/s\tSWAP OUT/s\tPROCESS FILTER"
		if command == "list" {
			header = "RUN\tCOMMAND\tSTART\tEND\tHOST\tSTATUS\tSAMPLES\tINTERVAL\tPROCESS FILTER\tINTERFACES"
		} else if command == "process" {
			header = "RUN\tTIME\tPID\tSTARTED\tCPU%\tRSS\tCOMMAND"
		}
		if options.metric != "" {
			header += "\t" + options.metric
		}
		if err := writeLine(header, nil); err != nil {
			return err
		}
	}
	for rows.Next() {
		var payload string
		var value any
		var line string
		var paints []historyPaint
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
			// 스냅샷 보고서는 history export로 꺼낸다. 목록에는 실행 정보만 둔다.
			run.Report = nil
			end := "-"
			if ended.Valid {
				at := time.Unix(0, ended.Int64).UTC()
				run.EndedAt, end = &at, at.Format(time.RFC3339Nano)
			}
			value = run
			interval, process, interfaces := fmt.Sprintf("%gs", run.IntervalS), historyTerminalText(run.Process), historyTerminalText(emptyAs(run.Interfaces, "all"))
			// 스냅샷에는 관측 간격과 필터가 없다. 결과 수가 SAMPLES 칸에 들어간다.
			if run.Command == historySnapshotCommand {
				interval, process, interfaces = "-", "-", "-"
			}
			line = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s", run.ID, historyTerminalText(run.Command), run.StartedAt.Format(time.RFC3339Nano), end, historyTerminalText(run.Hostname), historyTerminalText(run.Status), count, interval, process, interfaces)
			paints = []historyPaint{nil, nil, style.dim, style.dim, nil, historyStatusPaint(style, run.Status), nil, historyUnitPaint(style)}
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
			cells := strings.Split(line, "\t")
			line = strings.Join(cells[:6], "\t")
			for _, name := range []string{"disk_read_bytes_per_s", "disk_write_bytes_per_s", "net_in_bytes_per_s", "net_out_bytes_per_s", "swap_out_bytes_per_s"} {
				line += "\t" + historyTopMetricText(sample, name)
			}
			line += "\t" + cells[6]
			limits := newTopLimits(sample.Cores, style.color)
			paints = []historyPaint{nil, style.dim, nil, historyLevelPaint(style, limits.cpu, sample.CPU), historyLevelPaint(style, limits.memory, sample.MemoryPct), historyLevelPaint(style, limits.load, sample.Load1)}
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
			paints = []historyPaint{nil, style.dim, nil, style.dim, nil, historyUnitPaint(style)}
		}
		if jsonOutput {
			if err := encoder.Encode(value); err != nil {
				return err
			}
		} else {
			if options.metric != "" {
				line += "\t" + historyDisplayMetric(value, options.metric)
			}
			if err := writeLine(line, paints); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if jsonOutput {
		return nil
	}
	if style.color {
		_, err := io.WriteString(output, table.String())
		return err
	}
	return writer.Flush()
}

func (table *historyColorTable) add(line string, paints []historyPaint) {
	table.rows = append(table.rows, strings.Split(line, "\t"))
	table.paints = append(table.paints, paints)
}

// String은 tabwriter와 같은 배치(가장 긴 칸 + 2칸, 마지막 칸은 채우지 않음)를 공용 색 표로 그린다.
func (table historyColorTable) String() string {
	// 첫 열이 다른 history 명령에 넣는 run ID라 줄이지 않는다.
	return table.style.colorTableRows(table.rows, false, func(row, column int, _, shown string) string {
		if paints := table.paints[row+1]; column < len(paints) && paints[column] != nil {
			return paints[column](shown)
		}
		return shown
	})
}

// historyStatusPaint는 기록의 상태다. 끝난 기록은 초록, 기록 중이거나 끝내지 못한 기록은 노랑, 오류로 끝난 기록은 빨강이다.
func historyStatusPaint(style textStyle, status string) historyPaint {
	switch status {
	case "finished":
		return style.good
	case "unfinished":
		return style.warn
	case "error":
		return style.bad
	}
	return nil
}

// historyLevelPaint는 top과 같은 임계치를 넘은 값만 칠한다.
func historyLevelPaint(style textStyle, threshold topThreshold, value float64) historyPaint {
	return func(text string) string { return style.level(text, threshold.level(value)) }
}

// historyUnitPaint는 "2s"와 "1.00 GB"의 단위만 회색으로 칠한다. 숫자가 없는 "-"는 모두 회색이다.
func historyUnitPaint(style textStyle) historyPaint {
	return func(text string) string {
		end := strings.LastIndexAny(text, "0123456789") + 1
		return text[:end] + style.dim(text[end:])
	}
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
