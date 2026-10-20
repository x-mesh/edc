package edc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// history trend는 한 지표를 시간 구간으로 묶어 개수, 최소, 평균, p95, 최대를 낸다. --probe가 없으면 top 기록의 호스트 지표이고,
// 있으면 스냅샷 결과의 숫자 지표다. 구간은 로컬 시간의 정시, 자정, 월요일 자정에서 시작한다.

var historyTrendPeriods = []string{"hour", "day", "week"}

type historyTrendOptions struct {
	run      string
	from, to time.Time
	probe    string
	metric   string
	by       string
}

type historyTrendBucket struct {
	Start  time.Time `json:"start"`
	By     string    `json:"by"`
	Probe  string    `json:"probe,omitempty"`
	Metric string    `json:"metric"`
	Count  int       `json:"count"`
	Min    float64   `json:"min"`
	Avg    float64   `json:"avg"`
	P95    float64   `json:"p95"`
	Max    float64   `json:"max"`
}

func runHistoryTrend(args []string) int {
	set := newFlagSet("history trend")
	set.SetOutput(os.Stderr)
	var options historyTrendOptions
	set.StringVar(&options.run, "run", "", T("history.option.run"))
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	set.StringVar(&options.metric, "metric", "", T("history.option.metric"))
	set.StringVar(&options.probe, "probe", "", T("history.option.probe"))
	set.StringVar(&options.by, "by", "day", T("history.option.by"))
	jsonPath := set.String("json", "", T("history.option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() > 1 {
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
	if !historyTrendPeriod(options.by) {
		fmt.Fprintln(os.Stderr, T("history.error.trend_by", strings.Join(historyTrendPeriods, ", ")))
		return 2
	}
	if options.metric == "" || (options.probe == "" && !historyHostMetric(options.metric)) {
		fmt.Fprintln(os.Stderr, T("history.error.trend_metric", strings.Join(historyMetricNames(historyHostMetrics), ", ")))
		return 2
	}
	path := set.Arg(0)
	if path == "" {
		if path, err = defaultHistoryPath(); err != nil {
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
	err = writeHistoryTrend(output, db, options, *jsonPath != "", time.Local)
	if file != nil {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func historyTrendPeriod(by string) bool {
	for _, period := range historyTrendPeriods {
		if by == period {
			return true
		}
	}
	return false
}

func historyHostMetric(name string) bool {
	for _, metric := range historyHostMetrics {
		if metric.name == name {
			return true
		}
	}
	return false
}

// historyTrendQuery는 지표 값을 시각순으로 읽는 query다. top 지표 이름은 historyHostMetric으로 확인한 column 이름이다.
func historyTrendQuery(options historyTrendOptions) (string, []any) {
	query := "SELECT time_ns, " + options.metric + " FROM top_samples WHERE " + options.metric + " IS NOT NULL"
	var values []any
	if options.probe != "" {
		query = "SELECT time_ns, value FROM snapshot_metrics WHERE probe=? AND key=?"
		values = append(values, options.probe, options.metric)
	}
	if options.run != "" {
		query += " AND run_id=?"
		values = append(values, options.run)
	}
	if !options.from.IsZero() {
		query += " AND time_ns>=?"
		values = append(values, options.from.UnixNano())
	}
	if !options.to.IsZero() {
		query += " AND time_ns<?"
		values = append(values, options.to.UnixNano())
	}
	return query + " ORDER BY time_ns", values
}

// historyTrendCheckProbe는 스냅샷 지표가 기록에 있는지 본다. 없으면 기록된 이름을 오류에 담아 다시 고를 수 있게 한다.
func historyTrendCheckProbe(db *sql.DB, probe, metric string) error {
	has, err := historyHasSnapshots(db)
	if err != nil {
		return err
	}
	if !has {
		return errors.New(T("history.error.no_snapshots"))
	}
	distinct := func(query string, args ...any) ([]string, error) {
		rows, err := db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			names = append(names, historyTerminalText(name))
		}
		return names, rows.Err()
	}
	keys, err := distinct("SELECT DISTINCT key FROM snapshot_metrics WHERE probe=? ORDER BY key", probe)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		probes, err := distinct("SELECT DISTINCT probe FROM snapshot_metrics ORDER BY probe")
		if err != nil {
			return err
		}
		if len(probes) == 0 {
			return errors.New(T("history.error.no_snapshots"))
		}
		return errors.New(T("history.error.trend_probe", historyTerminalText(probe), strings.Join(probes, ", ")))
	}
	for _, key := range keys {
		if key == metric {
			return nil
		}
	}
	return errors.New(T("history.error.trend_key", historyTerminalText(probe), historyTerminalText(metric), strings.Join(keys, ", ")))
}

// historyBucketStart는 at이 속한 구간의 시작이다. time.Date로 맞춰 30분 단위 시간대와 서머타임 경계에서도 로컬 정시가 된다.
func historyBucketStart(at time.Time, by string, location *time.Location) time.Time {
	at = at.In(location)
	year, month, day := at.Date()
	switch by {
	case "hour":
		return time.Date(year, month, day, at.Hour(), 0, 0, 0, location)
	case "week":
		return time.Date(year, month, day-(int(at.Weekday())+6)%7, 0, 0, 0, 0, location)
	default:
		return time.Date(year, month, day, 0, 0, 0, 0, location)
	}
}

// historyTrendBuckets는 시각순으로 읽은 값을 구간마다 묶는다. 한 구간의 값만 메모리에 둔다.
func historyTrendBuckets(db *sql.DB, options historyTrendOptions, location *time.Location, emit func(historyTrendBucket) error) error {
	if options.probe != "" {
		if err := historyTrendCheckProbe(db, options.probe, options.metric); err != nil {
			return err
		}
	}
	query, values := historyTrendQuery(options)
	rows, err := db.Query(query, values...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var start time.Time
	var bucket []float64
	flush := func() error {
		if len(bucket) == 0 {
			return nil
		}
		sort.Float64s(bucket)
		sum := 0.0
		for _, value := range bucket {
			sum += value
		}
		// p95는 nearest-rank다. 값이 20개 미만이면 최대와 같다.
		rank := int(math.Ceil(0.95*float64(len(bucket)))) - 1
		result := historyTrendBucket{Start: start, By: options.by, Probe: options.probe, Metric: options.metric, Count: len(bucket),
			Min: bucket[0], Avg: sum / float64(len(bucket)), P95: bucket[rank], Max: bucket[len(bucket)-1]}
		bucket = bucket[:0]
		return emit(result)
	}
	for rows.Next() {
		var at int64
		var value float64
		if err := rows.Scan(&at, &value); err != nil {
			return err
		}
		if !historyFinite(value) {
			continue
		}
		current := historyBucketStart(time.Unix(0, at), options.by, location)
		if !current.Equal(start) {
			if err := flush(); err != nil {
				return err
			}
			start = current
		}
		bucket = append(bucket, value)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

func writeHistoryTrend(output io.Writer, db *sql.DB, options historyTrendOptions, jsonOutput bool, location *time.Location) error {
	if jsonOutput {
		encoder := json.NewEncoder(output)
		return historyTrendBuckets(db, options, location, func(bucket historyTrendBucket) error { return encoder.Encode(bucket) })
	}
	layout := "2006-01-02"
	if options.by == "hour" {
		layout = "2006-01-02 15:00"
	}
	name := options.metric
	if options.probe != "" {
		name = historyTerminalText(options.probe) + " " + historyTerminalText(options.metric)
	}
	_, offset := time.Now().In(location).Zone()
	zone := fmt.Sprintf("%+03d:%02d", offset/3600, abs(offset%3600)/60)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', tabwriter.AlignRight)
	count := 0
	err := historyTrendBuckets(db, options, location, func(bucket historyTrendBucket) error {
		if count == 0 {
			if _, err := fmt.Fprintln(output, T("history.trend.title", name, options.by, zone)); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(writer, "START\tCOUNT\tMIN\tAVG\tP95\tMAX\t"); err != nil {
				return err
			}
		}
		count++
		value := func(number float64) string { return historyTrendValue(options.metric, number) }
		_, err := fmt.Fprintf(writer, "%s\t%d\t%s\t%s\t%s\t%s\t\n", bucket.Start.Format(layout), bucket.Count, value(bucket.Min), value(bucket.Avg), value(bucket.P95), value(bucket.Max))
		return err
	})
	if err != nil {
		return err
	}
	if count == 0 {
		_, err = fmt.Fprintln(output, T("history.trend.empty"))
		return err
	}
	return writer.Flush()
}

// historyTrendValue는 이름의 단위를 따라 값을 읽기 쉽게 쓴다. top의 _pct와 스냅샷의 _percent는 이미 백분율이다.
func historyTrendValue(name string, value float64) string {
	switch {
	case value >= 0 && strings.HasSuffix(name, "_bytes_per_s"):
		return formatSizeShort(uint64(math.Round(value))) + "/s"
	case value >= 0 && (strings.HasSuffix(name, "_bytes") || name == "bytes"):
		return formatSizeShort(uint64(math.Round(value)))
	case strings.HasSuffix(name, "_pct") || strings.HasSuffix(name, "_percent"):
		return fmt.Sprintf("%.1f%%", value)
	case value == math.Trunc(value) && math.Abs(value) < 1e15:
		return fmt.Sprintf("%.0f", value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}
