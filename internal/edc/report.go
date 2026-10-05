package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	reportSizeLimit = 20 * 1024 * 1024
	// diffLabelWidth는 SAME, WORSE 같은 변화 label 열의 폭이다.
	diffLabelWidth = 15
	// reportCandidateLimit은 목록에 올리는 report 파일 수다. 디렉터리에 JSON이 많아도
	// 고르는 화면이 한 눈에 들어와야 한다.
	reportCandidateLimit = 20
)

// promptReportPaths는 report 경로가 빠졌을 때 terminal에서 받아 온다. 현재 디렉터리에 report가
// 있으면 목록에서 고르고, 없으면 경로를 직접 입력받는다. titles와 labels는 같은 길이다.
func promptReportPaths(command string, titles, labels []string) ([]string, bool) {
	file, ok := terminalInput(os.Stdin)
	if !ok {
		return nil, false
	}
	values := make([]string, 0, len(titles))
	for index, title := range titles {
		value, err := promptReportValue(file, title, labels[index])
		if err != nil {
			return nil, false
		}
		values = append(values, value)
	}
	promptEcho(os.Stdout, command+" "+strings.Join(values, " "))
	return values, true
}

func promptReportValue(file *os.File, title, label string) (string, error) {
	candidates := reportCandidates()
	if len(candidates) > 0 {
		candidates = append(candidates, selectItem{label: T("cli.report.enter_path"), value: ""})
		value, err := runSelect(file, os.Stdout, selectModel{title: title, label: label, items: candidates, color: true})
		if err != nil || value != "" {
			return value, err
		}
	} else {
		printReportEmpty(os.Stdout, ".")
	}
	return promptRemoteText(bufio.NewReader(file), os.Stdout, T("cli.report.path_label", label), "")
}

func promptReportCommand() (string, bool) {
	file, ok := terminalInput(os.Stdin)
	if !ok {
		return "", false
	}
	fmt.Fprintln(os.Stdout, T("cli.report.description"))
	items := []selectItem{
		{label: T("cli.report.command_show"), value: "show"},
		{label: T("cli.report.command_diff"), value: "diff"},
		{label: T("cli.report.command_list"), value: "list"},
	}
	value, err := runSelect(file, os.Stdout, newSelectModel("cli.prompt.subcommand_title", "cli.prompt.subcommand_label", items))
	if err != nil {
		return "", false
	}
	promptEcho(os.Stdout, "edc report "+value)
	return value, true
}

type savedReport struct {
	path   string
	report Report
}

func discoverReports(directory string) ([]savedReport, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var reports []savedReport
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		report, err := loadReport(path)
		if err != nil {
			continue
		}
		reports = append(reports, savedReport{path: path, report: report})
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].report.Run.StartedAt.Equal(reports[j].report.Run.StartedAt) {
			return reports[i].path < reports[j].path
		}
		return reports[i].report.Run.StartedAt.After(reports[j].report.Run.StartedAt)
	})
	return reports, nil
}

func savedReportLabel(report savedReport) string {
	return fmt.Sprintf("%s  ·  %s  ·  %s", reportIdentityValue(report.path), report.report.Run.StartedAt.Format(time.RFC3339), summaryLine(report.report.Results))
}

func reportCandidates() []selectItem {
	reports, err := discoverReports(".")
	if err != nil {
		return nil
	}
	items := make([]selectItem, 0, min(len(reports), reportCandidateLimit))
	for _, report := range reports[:min(len(reports), reportCandidateLimit)] {
		items = append(items, selectItem{label: savedReportLabel(report), value: report.path})
	}
	return items
}

func printReportEmpty(output io.Writer, directory string) {
	fmt.Fprintln(output, T("cli.report.empty", reportIdentityValue(directory)))
	fmt.Fprintln(output, T("cli.report.save_hint"))
}

func listReports(output io.Writer, directory string) error {
	reports, err := discoverReports(directory)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, T("cli.report.description"))
	if len(reports) == 0 {
		printReportEmpty(output, directory)
		return nil
	}
	for _, report := range reports {
		fmt.Fprintln(output, savedReportLabel(report))
	}
	fmt.Fprintln(output, T("cli.report.show_hint"))
	return nil
}

func loadReport(path string) (Report, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Report{}, fmt.Errorf("%s: %w\n%s", T("cli.report.not_found", reportIdentityValue(path)), err, T("cli.report.show_hint"))
		}
		return Report{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, reportSizeLimit+1))
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w", path, err)
	}
	invalid := func(field string) (Report, error) {
		return Report{}, errors.New(T("cli.report.invalid_field", path, field))
	}
	if len(data) > reportSizeLimit {
		return invalid("size")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return invalid("JSON object")
	}
	var schema string
	if raw, ok := fields["schema_version"]; ok {
		if err := json.Unmarshal(raw, &schema); err != nil {
			return invalid("schema_version")
		}
	}
	if schema != "1.0" {
		return Report{}, errors.New(T("cli.report.unsupported_schema", path, schema))
	}
	var rawRun map[string]json.RawMessage
	if err := json.Unmarshal(fields["run"], &rawRun); err != nil {
		return invalid("run")
	}
	if rawTime, ok := rawRun["started_at"]; ok {
		var started time.Time
		if err := json.Unmarshal(rawTime, &started); err != nil {
			return invalid("run.started_at")
		}
	}
	var rawResults []json.RawMessage
	if raw, ok := fields["results"]; ok {
		if err := json.Unmarshal(raw, &rawResults); err != nil {
			return invalid("results")
		}
	}
	for index, raw := range rawResults {
		var result Result
		if err := json.Unmarshal(raw, &result); err != nil {
			field := fmt.Sprintf("results[%d]", index)
			var typeError *json.UnmarshalTypeError
			if errors.As(err, &typeError) && typeError.Field != "" {
				field += "." + typeError.Field
			}
			if strings.Contains(err.Error(), "parsing time") {
				field += ".started_at"
			}
			return invalid(field)
		}
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		field := "JSON"
		var typeError *json.UnmarshalTypeError
		if errors.As(err, &typeError) {
			field = typeError.Field
		}
		if strings.Contains(err.Error(), "parsing time") {
			field = "started_at"
		}
		return invalid(field)
	}
	if report.SchemaVersion != "1.0" {
		return Report{}, errors.New(T("cli.report.unsupported_schema", path, report.SchemaVersion))
	}
	if report.Tool.Name != "edc" {
		return invalid("tool.name")
	}
	if strings.TrimSpace(report.Tool.Version) == "" {
		return invalid("tool.version")
	}
	if strings.TrimSpace(report.Run.ID) == "" {
		return invalid("run.id")
	}
	if report.Run.StartedAt.IsZero() {
		return invalid("run.started_at")
	}
	if report.Run.DurationMS < 0 {
		return invalid("run.duration_ms")
	}
	if _, ok := fields["results"]; !ok {
		return invalid("results")
	}
	if value := strings.TrimSpace(string(fields["summary"])); value == "" || !strings.HasPrefix(value, "{") {
		return invalid("summary")
	}
	for index, result := range report.Results {
		prefix := fmt.Sprintf("results[%d].", index)
		if strings.TrimSpace(result.Probe) == "" {
			return invalid(prefix + "probe")
		}
		switch result.Status {
		case StatusPass, StatusWarn, StatusFail, StatusSkip:
		default:
			return invalid(prefix + "status")
		}
		if result.DurationMS < 0 {
			return invalid(prefix + "duration_ms")
		}
	}
	if report.Summary.Pass < 0 || report.Summary.Warn < 0 || report.Summary.Fail < 0 || report.Summary.Skip < 0 || report.Summary != summarize(report.Results) {
		return invalid("summary")
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
	jsonPath := set.String("json", configuredString(activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	first, second := set.Arg(0), set.Arg(1)
	if set.NArg() == 0 {
		values, ok := promptReportPaths("edc report diff",
			[]string{T("cli.prompt.report_before_title"), T("cli.prompt.report_after_title")},
			[]string{T("cli.prompt.report_before_label"), T("cli.prompt.report_after_label")})
		if !ok {
			fmt.Fprintln(os.Stderr, T("cli.usage", "edc report diff [--json <path|->] <before> <after>"))
			return 2
		}
		first, second = values[0], values[1]
	} else if set.NArg() != 2 {
		fmt.Fprintln(os.Stderr, T("cli.usage", "edc report diff [--json <path|->] <before> <after>"))
		return 2
	}
	before, err := loadReport(first)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	after, err := loadReport(second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	diff := diffReports(first, before, second, after)
	switch {
	case *jsonPath != "":
		if err := writeJSONOutput(*jsonPath, diff); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	case liveTerminal():
		title := reportViewerTitle("report diff", first+" → "+second, diffSummaryLine(diff.Summary)) + "\n" + reportDiffIdentity(diff)
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
	Path       string    `json:"path"`
	RunID      string    `json:"run_id"`
	StartedAt  time.Time `json:"started_at"`
	Hostname   string    `json:"hostname,omitempty"`
	TargetURL  string    `json:"target_url,omitempty"`
	TargetHost string    `json:"target_host,omitempty"`
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
	url, _ := report.Target["url"].(string)
	host, _ := report.Target["host"].(string)
	return reportDiffSide{Path: path, RunID: report.Run.ID, StartedAt: report.Run.StartedAt, Hostname: hostname, TargetURL: url, TargetHost: host}
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
	fmt.Fprintln(writer, reportDiffIdentity(diff))
	for _, entry := range diff.Entries {
		printReportDiffEntry(writer, writer, entry, color)
	}
	fmt.Fprintf(writer, "\n%s\n", diffSummaryLine(diff.Summary))
}

func reportIdentityValue(value string) string {
	if value == "" {
		return T("cli.report.unavailable")
	}
	quoted := strconv.QuoteToGraphic(value)
	return quoted[1 : len(quoted)-1]
}

func reportDiffIdentity(diff reportDiff) string {
	lines := []string{
		fmt.Sprintf("target URL   %s → %s", reportIdentityValue(diff.Before.TargetURL), reportIdentityValue(diff.After.TargetURL)),
		fmt.Sprintf("target host  %s → %s", reportIdentityValue(diff.Before.TargetHost), reportIdentityValue(diff.After.TargetHost)),
		fmt.Sprintf("collected on %s → %s", reportIdentityValue(diff.Before.Hostname), reportIdentityValue(diff.After.Hostname)),
	}
	for _, pair := range [][2]string{{diff.Before.TargetURL, diff.After.TargetURL}, {diff.Before.TargetHost, diff.After.TargetHost}, {diff.Before.Hostname, diff.After.Hostname}} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			lines = append(lines, T("cli.report.identity_differs"))
			break
		}
	}
	lines = append(lines, T("cli.report.status_semantics"))
	return strings.Join(lines, "\n")
}

func diffSummaryLine(s reportDiffSummary) string {
	return fmt.Sprintf("%d status changed  ·  %d status same  ·  %d added  ·  %d removed  ·  %d worse", s.Changed, s.Same, s.Added, s.Removed, s.Regressed)
}

// printReportDiffEntry는 요약 줄과 metric 상세를 나눠 쓴다. 뷰어는 둘을 따로 접고 편다.
func printReportDiffEntry(line, detail io.Writer, entry reportDiffEntry, color bool) {
	label := strings.ToUpper(string(entry.Change))
	if entry.Change == changeSame || entry.Change == changeChanged {
		label = "STATUS " + label
	}
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
	if len(entry.Metrics) > 0 {
		fmt.Fprintf(line, "         %s\n", T("cli.report.delta_count", len(entry.Metrics)))
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
