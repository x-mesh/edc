package edc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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

// promptReportPair는 report diff의 두 report를 받는다. 현재 디렉터리에 report가 있으면 한 화면에서 두 개를 고르고,
// 없으면 이전과 이후의 경로를 차례로 입력받는다.
func promptReportPair() ([]string, bool) {
	file, ok := terminalInput(os.Stdin)
	if !ok {
		return nil, false
	}
	reports, err := discoverReports(".")
	if err != nil || len(reports) == 0 {
		return promptReportPaths("edc report diff",
			[]string{T("cli.prompt.report_before_title"), T("cli.prompt.report_after_title")},
			[]string{T("cli.prompt.report_before_label"), T("cli.prompt.report_after_label")})
	}
	model, err := runReportPair(file, os.Stdout, newReportPairModel(reports[:min(len(reports), reportCandidateLimit)], time.Now()))
	if err != nil {
		return nil, false
	}
	reader := bufio.NewReader(file)
	readPath := func(label string) (savedReport, error) {
		path, err := promptRemoteText(reader, os.Stdout, T("cli.report.path_label", label), "")
		if err != nil {
			return savedReport{}, err
		}
		// 읽지 못한 파일은 시각 없이 둔다. 그 오류는 diff가 파일을 열 때 알린다.
		report, _ := loadReport(path)
		return savedReport{path: path, report: report}, nil
	}
	var before, after savedReport
	switch picked := model.pickedReports(); len(picked) {
	case 2:
		before, after = orderReportPair(picked[0], picked[1])
	case 1:
		other, err := readPath(T("cli.report.other_label"))
		if err != nil {
			return nil, false
		}
		before, after = orderReportPair(picked[0], other)
	default:
		if before, err = readPath(T("cli.prompt.report_before_label")); err != nil {
			return nil, false
		}
		if after, err = readPath(T("cli.prompt.report_after_label")); err != nil {
			return nil, false
		}
	}
	printReportPair(os.Stdout, before, after)
	promptEcho(os.Stdout, "edc report diff "+before.path+" "+after.path)
	return []string{before.path, after.path}, true
}

// printReportPair는 선택 화면을 닫은 뒤 어느 파일을 이전과 이후로 비교하는지 남긴다.
func printReportPair(output io.Writer, before, after savedReport) {
	labels := []string{T("cli.prompt.report_before_label"), T("cli.prompt.report_after_label")}
	width := max(liveWidth(labels[0]), liveWidth(labels[1]))
	for index, report := range []savedReport{before, after} {
		started := "-"
		if !report.report.Run.StartedAt.IsZero() {
			started = report.report.Run.StartedAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(output, "%s  %s  %s\n", liveCell(labels[index], width), started, reportIdentityValue(report.path))
	}
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
	color := reportListColor(output)
	fmt.Fprintln(output, T("cli.report.empty", reportIdentityValue(directory)))
	fmt.Fprintln(output, reportHint(T("cli.report.save_hint"), color))
}

func listReports(output io.Writer, directory string) error {
	reports, err := discoverReports(directory)
	if err != nil {
		return err
	}
	color := reportListColor(output)
	description := T("cli.report.description")
	if color {
		description = reportHint(description, true)
	}
	fmt.Fprintln(output, description)
	if len(reports) == 0 {
		printReportEmpty(output, directory)
		return nil
	}
	for _, report := range reports {
		if color {
			fmt.Fprintln(output, savedReportRichLabel(report))
			continue
		}
		fmt.Fprintln(output, savedReportLabel(report))
	}
	fmt.Fprintln(output, reportHint(T("cli.report.show_hint"), color))
	return nil
}

// reportListColor는 목록을 칠할지다. 터미널인 stdout에만 칠하고, 파이프와 NO_COLOR와 다른 writer는 평문 그대로 둔다.
func reportListColor(output io.Writer) bool {
	return output == io.Writer(os.Stdout) && richEnabled(os.Stdout)
}

// savedReportRichLabel은 savedReportLabel과 같은 글자다. 시각과 구분자는 회색, 결과 수는 상태 색이다.
func savedReportRichLabel(report savedReport) string {
	separator := richDim("  ·  ")
	return reportIdentityValue(report.path) + separator + richDim(report.report.Run.StartedAt.Format(time.RFC3339)) + separator +
		reportPaintCounts(summaryLine(report.report.Results), " · ", reportSummaryColors, false)
}

// reportHint는 안내 줄을 칠한다. "이름표: edc ..." 줄은 이름표를 회색, 명령을 굵은 청록으로, 나머지 줄은 회색으로 둔다.
func reportHint(text string, color bool) string {
	if !color {
		return text
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if label, command, found := strings.Cut(line, ": edc "); found {
			lines[index] = richDim(label+": ") + richPaint("edc "+command, richCyan, true)
			continue
		}
		lines[index] = richDim(line)
	}
	return strings.Join(lines, "\n")
}

func loadReport(path string) (Report, error) {
	// O_NONBLOCK keeps a FIFO named *.json from blocking discovery before Stat rejects it.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Report{}, fmt.Errorf("%s: %w\n%s", T("cli.report.not_found", reportIdentityValue(path)), err, T("cli.report.show_hint"))
		}
		return Report{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Report{}, fmt.Errorf("%s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Report{}, errors.New(T("cli.report.not_regular", reportIdentityValue(path)))
	}
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
		title := reportViewerHeading("report show", path, summaryLine(report.Results), " · ", reportSummaryColors)
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
	set := newFlagSet("report diff")
	set.SetOutput(os.Stderr)
	jsonPath := set.String("json", configuredString(activeConfig.Defaults.Common.JSON, ""), T("option.json"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	first, second := set.Arg(0), set.Arg(1)
	if set.NArg() == 0 {
		values, ok := promptReportPair()
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
		title := reportViewerHeading("report diff", first+" → "+second, diffSummaryLine(diff.Summary), "  ·  ", reportDiffColors) + "\n" + reportDiffIdentityHeading(reportDiffIdentity(diff))
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
	// Directories와 DirectoriesNote는 du.scan에만 있다. 배열 metric은 diffMetrics가 건너뛰므로 du의 큰 디렉터리
	// 목록을 경로로 맞춘 결과를 따로 둔다. Note는 비교하지 않은 이유다.
	Directories     []duDirDelta `json:"directories,omitempty"`
	DirectoriesNote string       `json:"directories_note,omitempty"`
	// Opened와 Closed는 listen에만 있다. 열린 socket 목록도 배열이라 diffMetrics가 건너뛰므로 따로 맞춘다.
	Opened []listenSocket `json:"opened,omitempty"`
	Closed []listenSocket `json:"closed,omitempty"`
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
			if isDuProbe(probe) {
				entry.Directories, entry.DirectoriesNote = duCompareDirs(old.Metrics, current.Metrics)
			}
			if probe == listenProbeID {
				entry.Opened, entry.Closed = diffReportSockets(old.Metrics, current.Metrics)
			}
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

// reportSockets는 listen 결과의 socket 목록이다. 파일에서 읽은 보고서는 map 배열이고, 같은 process에서 만든 결과는
// []listenSocket이라 JSON으로 한 번 맞춘다.
func reportSockets(metrics map[string]interface{}) ([]listenSocket, bool) {
	value, ok := metrics["sockets"]
	if !ok {
		return nil, false
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var sockets []listenSocket
	return sockets, json.Unmarshal(data, &sockets) == nil
}

// diffReportSockets는 두 보고서의 열린 socket을 비교한다. 서비스를 다시 시작하면 PID만 바뀌므로 PID는 비교하지 않는다.
func diffReportSockets(before, after map[string]interface{}) (opened, closed []listenSocket) {
	old, okBefore := reportSockets(before)
	current, okAfter := reportSockets(after)
	if !okBefore || !okAfter {
		return nil, nil
	}
	key := func(socket listenSocket) listenSocketKey {
		return listenSocketKey{Proto: socket.Proto, Address: socket.Address, Port: socket.Port, Process: socket.Process}
	}
	seen := map[listenSocketKey]bool{}
	for _, socket := range old {
		seen[key(socket)] = true
	}
	now := map[listenSocketKey]bool{}
	for _, socket := range current {
		now[key(socket)] = true
		if !seen[key(socket)] {
			opened = append(opened, socket)
		}
	}
	for _, socket := range old {
		if !now[key(socket)] {
			closed = append(closed, socket)
		}
	}
	return opened, closed
}

// formatMetricValue는 metric 값을 사람이 읽는 꼴로 쓴다. 바이트는 크기, 퍼센트는 소수 둘째 자리까지 %로 쓰고(첫째
// 자리까지면 작은 변화가 41.3% → 41.3%로 보인다), 나머지 수는 지수 없이 쓴다.
// JSON 출력은 원래 값을 그대로 담는다.
func formatMetricValue(key string, value interface{}) string {
	number, ok := metricNumber(value)
	if !ok {
		return fmt.Sprint(value)
	}
	switch {
	case reportBytesMetric(key) && number >= 0:
		return formatSizeShort(uint64(number))
	case reportPercentMetric(key):
		return strconv.FormatFloat(math.Round(number*100)/100, 'f', -1, 64) + "%"
	default:
		return strconv.FormatFloat(math.Round(number*1000)/1000, 'f', -1, 64)
	}
}

func formatMetricDelta(key string, delta float64) string {
	switch {
	case reportBytesMetric(key):
		sign := "+"
		if delta < 0 {
			sign = "-"
		}
		return sign + formatSizeShort(uint64(math.Abs(delta)))
	case reportPercentMetric(key):
		return fmt.Sprintf("%+.2f%%p", delta)
	default:
		return formatDelta(delta)
	}
}

func reportBytesMetric(key string) bool {
	return key == "bytes" || strings.HasSuffix(key, "_bytes")
}

func reportPercentMetric(key string) bool {
	return strings.HasSuffix(key, "_pct") || strings.HasSuffix(key, "_percent") || key == "percent"
}

func formatReportSocket(socket listenSocket) string {
	text := socket.Proto + " " + socket.Address
	if socket.Port != 0 && !strings.HasSuffix(socket.Address, ":"+strconv.Itoa(socket.Port)) {
		text += ":" + strconv.Itoa(socket.Port)
	}
	if socket.Process != "" {
		text += "  " + socket.Process
	}
	return reportIdentityValue(text)
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
	}
	probe := strings.TrimPrefix(entry.Probe, "remote.")
	// label에 색이 붙으면 byte 폭이 달라지므로 표시 폭으로 채운다. summary는 여러 줄일 수 있어 첫 줄만 쓴다.
	padded := liveCell(reportDiffLabel(label, entry, color), diffLabelWidth)
	status := func(value Status) string {
		if color {
			return richStatusBadge(value)
		}
		return terminalStatus(value, false)
	}
	arrow, note := "→", T("cli.report.delta_count", len(entry.Metrics))
	directories := T("du.diff.count", len(entry.Directories))
	if color {
		arrow, note, directories = richDim(arrow), richDim(note), richDim(directories)
	}
	switch entry.Change {
	case changeSame:
		fmt.Fprintf(line, "%s %-24s  %s\n", padded, probe, status(entry.AfterStatus))
	case changeChanged:
		fmt.Fprintf(line, "%s %-24s  %s %s %s  %s\n", padded, probe, status(entry.BeforeStatus), arrow, status(entry.AfterStatus), firstLine(entry.AfterSummary))
	case changeAdded:
		fmt.Fprintf(line, "%s %-24s  %s  %s\n", padded, probe, status(entry.AfterStatus), firstLine(entry.AfterSummary))
	case changeRemoved:
		fmt.Fprintf(line, "%s %-24s  %s  %s\n", padded, probe, status(entry.BeforeStatus), firstLine(entry.BeforeSummary))
	}
	if len(entry.Metrics) > 0 {
		fmt.Fprintf(line, "         %s\n", note)
	}
	if len(entry.Directories) > 0 {
		fmt.Fprintf(line, "         %s\n", directories)
	}
	if len(entry.Opened)+len(entry.Closed) > 0 {
		sockets := T("cli.report.socket_count", len(entry.Opened), len(entry.Closed))
		if color {
			sockets = richDim(sockets)
		}
		fmt.Fprintf(line, "         %s\n", sockets)
	}
	for _, metric := range entry.Metrics {
		before, after := formatMetricValue(metric.Key, metric.Before), formatMetricValue(metric.Key, metric.After)
		if metric.Delta != nil {
			fmt.Fprintf(detail, "         %-24s  %s → %s (%s)\n", metric.Key, before, after, formatMetricDelta(metric.Key, *metric.Delta))
		} else {
			fmt.Fprintf(detail, "         %-24s  %s → %s\n", metric.Key, before, after)
		}
	}
	for _, socket := range entry.Opened {
		fmt.Fprintf(detail, "         + %s\n", formatReportSocket(socket))
	}
	for _, socket := range entry.Closed {
		fmt.Fprintf(detail, "         - %s\n", formatReportSocket(socket))
	}
	for _, text := range formatDuDirDeltas(entry.Directories) {
		fmt.Fprintf(detail, "       %s\n", text)
	}
	if entry.DirectoriesNote != "" {
		fmt.Fprintf(detail, "         %s\n", entry.DirectoriesNote)
	}
}

// reportDiffLabel은 변화 이름의 색이다. 나빠짐과 사라짐은 빨강, 나아짐과 추가는 초록, 그대로는 회색이다.
// PASS와 SKIP 사이처럼 순위가 같은 변화는 나아진 것이 아니라 기본 색으로 둔다.
func reportDiffLabel(label string, entry reportDiffEntry, color bool) string {
	if !color {
		return label
	}
	switch {
	case entry.Regressed:
		return richPaint(label, richRed, true)
	case entry.Change == changeRemoved:
		return richPaint(label, richRed, false)
	case entry.Change == changeAdded:
		return richPaint(label, richGreen, false)
	case entry.Change == changeChanged && statusRank(entry.AfterStatus) < statusRank(entry.BeforeStatus):
		return richPaint(label, richGreen, false)
	case entry.Change == changeChanged:
		return label
	}
	return richDim(label)
}

func formatDelta(delta float64) string {
	if delta == math.Trunc(delta) {
		return fmt.Sprintf("%+d", int64(delta))
	}
	return fmt.Sprintf("%+.2f", delta)
}
