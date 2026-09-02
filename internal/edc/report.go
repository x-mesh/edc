package edc

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	reportSizeLimit = 20 * 1024 * 1024
	// diffLabelWidth는 SAME, WORSE 같은 변화 label 열의 폭이다.
	diffLabelWidth = 8
)

func loadReport(path string) (Report, error) {
	file, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer file.Close()
	var report Report
	if err := json.NewDecoder(io.LimitReader(file, reportSizeLimit)).Decode(&report); err != nil {
		return Report{}, fmt.Errorf("%s: %w", path, err)
	}
	if report.SchemaVersion != "1.0" {
		return Report{}, errors.New(T("cli.report.unsupported_schema", path, report.SchemaVersion))
	}
	return report, nil
}

func runReportShow(path string) int {
	report, err := loadReport(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if liveTerminal() {
		title := reportViewerTitle("report show", path, summaryLine(report.Results))
		if err := runReportViewer(title, resultEntries(report.Results, true), resultFilters()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return exitCode(report.Results)
	}
	printTerminal(os.Stdout, report.Results, false)
	return exitCode(report.Results)
}

func runReportDiff(args []string) int {
	set := flag.NewFlagSet("report diff", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	jsonPath := set.String("json", "", T("option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 2 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc report diff [--json <path|->] <before> <after>"))
		return 2
	}
	before, err := loadReport(set.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	after, err := loadReport(set.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	diff := diffReports(set.Arg(0), before, set.Arg(1), after)
	switch {
	case *jsonPath != "":
		if err := writeJSONOutput(*jsonPath, diff); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	case liveTerminal():
		title := reportViewerTitle("report diff", set.Arg(0)+" → "+set.Arg(1), diffSummaryLine(diff.Summary))
		if err := runReportViewer(title, diffEntries(diff, true), diffFilters()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	default:
		printReportDiff(os.Stdout, diff, false)
	}
	if diff.Summary.Regressed > 0 {
		return 1
	}
	return 0
}

type reportChange string

const (
	changeSame    reportChange = "same"
	changeChanged reportChange = "changed"
	changeAdded   reportChange = "added"
	changeRemoved reportChange = "removed"
)

type metricDelta struct {
	Key    string      `json:"key"`
	Before interface{} `json:"before"`
	After  interface{} `json:"after"`
	Delta  *float64    `json:"delta,omitempty"`
}

type reportDiffEntry struct {
	Probe         string        `json:"probe"`
	Change        reportChange  `json:"change"`
	Regressed     bool          `json:"regressed"`
	BeforeStatus  Status        `json:"before_status,omitempty"`
	AfterStatus   Status        `json:"after_status,omitempty"`
	BeforeSummary string        `json:"before_summary,omitempty"`
	AfterSummary  string        `json:"after_summary,omitempty"`
	Metrics       []metricDelta `json:"metrics,omitempty"`
}

type reportDiffSide struct {
	Path      string    `json:"path"`
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
	Hostname  string    `json:"hostname,omitempty"`
}

type reportDiffSummary struct {
	Same      int `json:"same"`
	Changed   int `json:"changed"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Regressed int `json:"regressed"`
}

type reportDiff struct {
	SchemaVersion string            `json:"schema_version"`
	Before        reportDiffSide    `json:"before"`
	After         reportDiffSide    `json:"after"`
	Entries       []reportDiffEntry `json:"entries"`
	Summary       reportDiffSummary `json:"summary"`
}

// diffReports는 probe 이름으로 두 report를 맞춰 status 변화와 scalar metric 차이를 만든다.
func diffReports(beforePath string, before Report, afterPath string, after Report) reportDiff {
	diff := reportDiff{SchemaVersion: "1.0", Before: diffSide(beforePath, before), After: diffSide(afterPath, after), Entries: []reportDiffEntry{}}
	beforeResults := indexResults(before.Results)
	afterResults := indexResults(after.Results)
	names := make(map[string]struct{}, len(beforeResults)+len(afterResults))
	for name := range beforeResults {
		names[name] = struct{}{}
	}
	for name := range afterResults {
		names[name] = struct{}{}
	}
	probes := make([]string, 0, len(names))
	for name := range names {
		probes = append(probes, name)
	}
	sort.Strings(probes)
	for _, probe := range probes {
		old, hadBefore := beforeResults[probe]
		current, hasAfter := afterResults[probe]
		entry := reportDiffEntry{Probe: probe}
		switch {
		case hadBefore && hasAfter:
			entry.BeforeStatus, entry.AfterStatus = old.Status, current.Status
			entry.BeforeSummary, entry.AfterSummary = old.Summary, current.Summary
			entry.Metrics = diffMetrics(old.Metrics, current.Metrics)
			// probe 실행 시간은 metrics 밖에 있으므로 따로 앞에 붙인다.
			if old.DurationMS != current.DurationMS {
				delta := float64(current.DurationMS - old.DurationMS)
				entry.Metrics = append([]metricDelta{{Key: "duration_ms", Before: old.DurationMS, After: current.DurationMS, Delta: &delta}}, entry.Metrics...)
			}
			entry.Change = changeSame
			if old.Status != current.Status {
				entry.Change = changeChanged
				entry.Regressed = statusRank(current.Status) > statusRank(old.Status)
			}
		case hadBefore:
			entry.Change = changeRemoved
			entry.BeforeStatus, entry.BeforeSummary = old.Status, old.Summary
		default:
			entry.Change = changeAdded
			entry.AfterStatus, entry.AfterSummary = current.Status, current.Summary
		}
		switch entry.Change {
		case changeSame:
			diff.Summary.Same++
		case changeChanged:
			diff.Summary.Changed++
		case changeAdded:
			diff.Summary.Added++
		case changeRemoved:
			diff.Summary.Removed++
		}
		if entry.Regressed {
			diff.Summary.Regressed++
		}
		diff.Entries = append(diff.Entries, entry)
	}
	return diff
}

func diffSide(path string, report Report) reportDiffSide {
	hostname, _ := report.Host["hostname"].(string)
	return reportDiffSide{Path: path, RunID: report.Run.ID, StartedAt: report.Run.StartedAt, Hostname: hostname}
}

func indexResults(results []Result) map[string]Result {
	indexed := make(map[string]Result, len(results))
	for _, result := range results {
		indexed[result.Probe] = result
	}
	return indexed
}

// statusRank는 악화 판단 기준이다. skip은 pass와 같은 등급으로 본다.
func statusRank(status Status) int {
	switch status {
	case StatusWarn:
		return 1
	case StatusFail:
		return 2
	default:
		return 0
	}
}

// diffMetrics는 양쪽에 있는 scalar metric만 비교한다. 배열과 object는 건너뛴다.
func diffMetrics(before, after map[string]interface{}) []metricDelta {
	keys := make([]string, 0, len(before))
	for key := range before {
		if _, exists := after[key]; exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var deltas []metricDelta
	for _, key := range keys {
		oldValue, newValue := before[key], after[key]
		oldNumber, oldIsNumber := metricNumber(oldValue)
		newNumber, newIsNumber := metricNumber(newValue)
		switch {
		case oldIsNumber && newIsNumber:
			if oldNumber == newNumber {
				continue
			}
			delta := newNumber - oldNumber
			deltas = append(deltas, metricDelta{Key: key, Before: oldValue, After: newValue, Delta: &delta})
		case isScalarMetric(oldValue) && isScalarMetric(newValue):
			if fmt.Sprint(oldValue) == fmt.Sprint(newValue) {
				continue
			}
			deltas = append(deltas, metricDelta{Key: key, Before: oldValue, After: newValue})
		}
	}
	return deltas
}

func metricNumber(value interface{}) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func isScalarMetric(value interface{}) bool {
	switch value.(type) {
	case string, bool, nil:
		return true
	default:
		_, isNumber := metricNumber(value)
		return isNumber
	}
}

func printReportDiff(writer io.Writer, diff reportDiff, color bool) {
	fmt.Fprintf(writer, "diff  %s → %s\n", diff.Before.Path, diff.After.Path)
	fmt.Fprintf(writer, "run   %s %s → %s %s\n\n", diff.Before.RunID, diff.Before.StartedAt.Format(time.RFC3339), diff.After.RunID, diff.After.StartedAt.Format(time.RFC3339))
	for _, entry := range diff.Entries {
		printReportDiffEntry(writer, writer, entry, color)
	}
	fmt.Fprintf(writer, "\n%s\n", diffSummaryLine(diff.Summary))
}

func diffSummaryLine(s reportDiffSummary) string {
	return fmt.Sprintf("%d changed  ·  %d same  ·  %d added  ·  %d removed  ·  %d worse", s.Changed, s.Same, s.Added, s.Removed, s.Regressed)
}

// printReportDiffEntry는 요약 줄과 metric 상세를 나눠 쓴다. 뷰어는 둘을 따로 접고 편다.
func printReportDiffEntry(line, detail io.Writer, entry reportDiffEntry, color bool) {
	label := strings.ToUpper(string(entry.Change))
	if entry.Regressed {
		label = "WORSE"
		if color {
			label = "\033[31m" + label + "\033[0m"
		}
	}
	probe := strings.TrimPrefix(entry.Probe, "remote.")
	// label에 색이 붙으면 byte 폭이 달라지므로 표시 폭으로 채운다. summary는 여러 줄일 수 있어 첫 줄만 쓴다.
	padded := liveCell(label, diffLabelWidth)
	switch entry.Change {
	case changeSame:
		fmt.Fprintf(line, "%s %-24s  %s\n", padded, probe, terminalStatus(entry.AfterStatus, color))
	case changeChanged:
		fmt.Fprintf(line, "%s %-24s  %s → %s  %s\n", padded, probe, terminalStatus(entry.BeforeStatus, color), terminalStatus(entry.AfterStatus, color), firstLine(entry.AfterSummary))
	case changeAdded:
		fmt.Fprintf(line, "%s %-24s  %s  %s\n", padded, probe, terminalStatus(entry.AfterStatus, color), firstLine(entry.AfterSummary))
	case changeRemoved:
		fmt.Fprintf(line, "%s %-24s  %s  %s\n", padded, probe, terminalStatus(entry.BeforeStatus, color), firstLine(entry.BeforeSummary))
	}
	for _, metric := range entry.Metrics {
		if metric.Delta != nil {
			fmt.Fprintf(detail, "         %-24s  %v → %v (%s)\n", metric.Key, metric.Before, metric.After, formatDelta(*metric.Delta))
		} else {
			fmt.Fprintf(detail, "         %-24s  %v → %v\n", metric.Key, metric.Before, metric.After)
		}
	}
}

func formatDelta(delta float64) string {
	if delta == math.Trunc(delta) {
		return fmt.Sprintf("%+d", int64(delta))
	}
	return fmt.Sprintf("%+.2f", delta)
}
