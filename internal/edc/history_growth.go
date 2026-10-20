package edc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// history growth는 기간 안의 스냅샷으로 두 가지를 낸다. 마운트마다 사용량의 하루 증가량과 그 속도로 가득 차기까지의 일수,
// 그리고 --du 경로마다 기간의 첫 스냅샷과 마지막 스냅샷 사이에 크기가 가장 많이 바뀐 디렉터리다.

const (
	historyGrowthWindow = 7 * 24 * time.Hour
	// 점이 적거나 기간이 짧으면 기울기가 우연한 변동을 따라가므로 가득 참을 추정하지 않는다.
	historyGrowthMinSamples = 3
	historyGrowthMinSpan    = time.Hour
	historyDiskProbePrefix  = "info.disk:"
	// 컨테이너나 임시 볼륨의 긴 마운트 경로 하나가 표 전체를 넓히지 않게 자른다. JSON에는 전체 경로가 남는다.
	historyGrowthMountWidth = 40
	// 프로세스 RSS가 이만큼 오래, 이만큼 많이, 직선에 가깝게 늘면 누수를 의심한다. 캐시를 채우는 동안의 증가나 짧은 부하
	// 처럼 처음 몇 시간만 오르는 경우를 걸러내려고 기간과 직선 적합도를 함께 본다.
	historyLeakMinSpan       = 6 * time.Hour
	historyLeakMinSamples    = 12
	historyLeakMinRise       = 0.10
	historyLeakMinFit        = 0.7
	historyGrowthProcessRows = 20
)

type historyGrowthOptions struct {
	from, to time.Time
	root     string
	process  topProcessFilter
}

// historyGrowthProcess는 프로세스 하나(pid와 시작 시각)의 RSS 변화다.
type historyGrowthProcess struct {
	Kind          string     `json:"kind"`
	Command       string     `json:"command"`
	PID           int        `json:"pid"`
	Started       *time.Time `json:"started,omitempty"`
	Samples       int        `json:"samples"`
	First         time.Time  `json:"first"`
	Last          time.Time  `json:"last"`
	FirstRSSBytes float64    `json:"first_rss_bytes"`
	RSSBytes      float64    `json:"rss_bytes"`
	// BytesPerDay는 rss_bytes의 최소제곱 기울기이고 Fit은 그 직선의 결정계수(R²)다. 점이 모자라면 nil이다.
	BytesPerDay *float64 `json:"rss_bytes_per_day,omitempty"`
	Fit         *float64 `json:"fit,omitempty"`
	LeakSuspect bool     `json:"leak_suspect"`
	// Unfiltered는 --process 없이 기록한 표본이 섞였는지다. 그 표본은 상위권에 든 순간뿐이다.
	Unfiltered bool `json:"unfiltered,omitempty"`
}

type historyGrowthDisk struct {
	Kind       string    `json:"kind"`
	Mount      string    `json:"mount"`
	Samples    int       `json:"samples"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	UsedBytes  float64   `json:"used_bytes"`
	TotalBytes float64   `json:"total_bytes"`
	// BytesPerDay는 used_bytes의 최소제곱 기울기다. 점이 모자라면 nil이다.
	BytesPerDay *float64 `json:"bytes_per_day,omitempty"`
	// FullInDays는 BytesPerDay로 남은 용량을 나눈 추정이다. 늘지 않으면 nil이다.
	FullInDays *float64 `json:"full_in_days,omitempty"`
}

type historyGrowthDirs struct {
	Kind      string    `json:"kind"`
	Root      string    `json:"root"`
	Snapshots int       `json:"snapshots"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	// BeforeBytes와 AfterBytes는 경로 전체의 크기다. 경로 바로 아래 파일의 변화는 디렉터리 목록에 없고 여기에만 보인다.
	BeforeBytes float64      `json:"before_bytes"`
	AfterBytes  float64      `json:"after_bytes"`
	Directories []duDirDelta `json:"directories,omitempty"`
	Note        string       `json:"note,omitempty"`
}

type historyGrowth struct {
	From, To  time.Time
	Process   string
	Disks     []historyGrowthDisk
	Dirs      []historyGrowthDirs
	Processes []historyGrowthProcess
}

func runHistoryGrowth(args []string) int {
	set := newFlagSet("history growth")
	set.SetOutput(os.Stderr)
	var options historyGrowthOptions
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	set.StringVar(&options.root, "root", "", T("history.option.root"))
	process := set.String("process", "", T("history.option.growth_process"))
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
	if options.process, err = parseTopProcessFilter(*process); err != nil {
		fmt.Fprintln(os.Stderr, T("observe.top.process_invalid"))
		return 2
	}
	if options.process.active() && options.root != "" {
		fmt.Fprintln(os.Stderr, T("history.error.growth_process_root"))
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
	growth, err := collectHistoryGrowth(db, options, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *jsonPath == "" && liveTerminal() {
		if err := runHistoryGrowthBrowser(db, path, options, growth); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if *jsonPath == "" {
		writeHistoryGrowth(os.Stdout, growth, time.Local)
		return 0
	}
	output := io.Writer(os.Stdout)
	var file *os.File
	if *jsonPath != "-" {
		if file, err = os.OpenFile(*jsonPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		output = file
	}
	err = writeHistoryGrowthJSON(output, growth)
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

// historyGrowthWindowOf는 기간을 정한다. --from이 없으면 끝에서 7일 전부터이고, --to가 없으면 지금까지다.
func historyGrowthWindowOf(options historyGrowthOptions, now time.Time) (time.Time, time.Time) {
	to := options.to
	if to.IsZero() {
		to = now
	}
	from := options.from
	if from.IsZero() {
		from = to.Add(-historyGrowthWindow)
	}
	return from, to
}

func collectHistoryGrowth(db *sql.DB, options historyGrowthOptions, now time.Time) (historyGrowth, error) {
	// --process는 top 기록만 읽으므로 스냅샷이 없어도 된다.
	if options.process.active() {
		from, to := historyGrowthWindowOf(options, now)
		processes, err := historyGrowthProcesses(db, from, to, options.process)
		return historyGrowth{From: from, To: to, Process: options.process.String(), Processes: processes}, err
	}
	has, err := historyHasSnapshots(db)
	if err != nil {
		return historyGrowth{}, err
	}
	if !has {
		return historyGrowth{}, fmt.Errorf("%s", T("history.error.no_snapshots"))
	}
	from, to := historyGrowthWindowOf(options, now)
	growth := historyGrowth{From: from, To: to}
	if options.root == "" {
		if growth.Disks, err = historyGrowthDisks(db, from, to); err != nil {
			return historyGrowth{}, err
		}
	}
	growth.Dirs, err = historyGrowthDirectories(db, from, to, options.root)
	return growth, err
}

func historyGrowthDisks(db *sql.DB, from, to time.Time) ([]historyGrowthDisk, error) {
	rows, err := db.Query(`SELECT probe, time_ns, key, value FROM snapshot_metrics
		WHERE substr(probe, 1, ?)=? AND key IN ('used_bytes', 'total_bytes') AND time_ns>=? AND time_ns<?
		ORDER BY probe, time_ns`, len(historyDiskProbePrefix), historyDiskProbePrefix, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type point struct {
		at   int64
		used float64
	}
	points := map[string][]point{}
	total := map[string]float64{}
	var mounts []string
	for rows.Next() {
		var probe, key string
		var at int64
		var value float64
		if err := rows.Scan(&probe, &at, &key, &value); err != nil {
			return nil, err
		}
		mount := strings.TrimPrefix(probe, historyDiskProbePrefix)
		if _, seen := points[mount]; !seen {
			points[mount] = nil
			mounts = append(mounts, mount)
		}
		if key == "total_bytes" {
			total[mount] = value
		} else {
			points[mount] = append(points[mount], point{at: at, used: value})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var disks []historyGrowthDisk
	for _, mount := range mounts {
		samples := points[mount]
		if len(samples) == 0 {
			continue
		}
		first, last := samples[0], samples[len(samples)-1]
		disk := historyGrowthDisk{Kind: "disk", Mount: mount, Samples: len(samples), First: time.Unix(0, first.at).UTC(), Last: time.Unix(0, last.at).UTC(),
			UsedBytes: last.used, TotalBytes: total[mount]}
		if len(samples) >= historyGrowthMinSamples && time.Duration(last.at-first.at) >= historyGrowthMinSpan {
			days := make([]float64, len(samples))
			used := make([]float64, len(samples))
			for index, sample := range samples {
				days[index] = float64(sample.at-first.at) / float64(24*time.Hour)
				used[index] = sample.used
			}
			slope := historyLeastSquaresSlope(days, used)
			disk.BytesPerDay = &slope
			if slope > 0 && disk.TotalBytes > disk.UsedBytes {
				full := (disk.TotalBytes - disk.UsedBytes) / slope
				disk.FullInDays = &full
			}
		}
		disks = append(disks, disk)
	}
	return disks, nil
}

func historyLeastSquaresSlope(x, y []float64) float64 {
	slope, _ := historyLeastSquaresFit(x, y)
	return slope
}

// historyLeastSquaresFit은 기울기와 결정계수(R²)다. y가 변하지 않으면 직선이 완전히 맞으므로 R²은 1이다.
func historyLeastSquaresFit(x, y []float64) (float64, float64) {
	var meanX, meanY float64
	for index := range x {
		meanX += x[index]
		meanY += y[index]
	}
	meanX /= float64(len(x))
	meanY /= float64(len(y))
	var covariance, varianceX, varianceY float64
	for index := range x {
		covariance += (x[index] - meanX) * (y[index] - meanY)
		varianceX += (x[index] - meanX) * (x[index] - meanX)
		varianceY += (y[index] - meanY) * (y[index] - meanY)
	}
	if varianceX == 0 {
		return 0, 0
	}
	if varianceY == 0 {
		return 0, 1
	}
	return covariance / varianceX, covariance * covariance / (varianceX * varianceY)
}

// historyGrowthProcesses는 일치한 프로세스마다 RSS의 하루 변화를 낸다. 한 프로세스의 점만 메모리에 둔다. 누수 의심을
// 먼저, 그다음 하루 증가량이 큰 순서다.
func historyGrowthProcesses(db *sql.DB, from, to time.Time, filter topProcessFilter) ([]historyGrowthProcess, error) {
	unfiltered, err := historyUnfilteredRuns(db)
	if err != nil {
		return nil, err
	}
	match, values := historyProcessMatch(filter, "p")
	values = append(values, from.UnixNano(), to.UnixNano())
	rows, err := db.Query(`SELECT p.pid, COALESCE(p.started, ''), p.command, p.run_id, p.time_ns, p.rss_bytes FROM top_process_samples p
		WHERE `+match+` AND p.rss_bytes IS NOT NULL AND p.time_ns>=? AND p.time_ns<? ORDER BY p.pid, p.started, p.time_ns`, values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var processes []historyGrowthProcess
	var current *historyGrowthProcess
	var days, rss []float64
	finish := func() {
		if current == nil {
			return
		}
		process := *current
		process.Samples, process.FirstRSSBytes, process.RSSBytes = len(rss), rss[0], rss[len(rss)-1]
		span := process.Last.Sub(process.First)
		if len(rss) >= historyGrowthMinSamples && span >= historyGrowthMinSpan {
			slope, fit := historyLeastSquaresFit(days, rss)
			process.BytesPerDay, process.Fit = &slope, &fit
			rise := slope * span.Hours() / 24
			process.LeakSuspect = span >= historyLeakMinSpan && len(rss) >= historyLeakMinSamples && slope > 0 &&
				rise >= historyLeakMinRise*process.FirstRSSBytes && fit >= historyLeakMinFit
		}
		processes = append(processes, process)
		current, days, rss = nil, days[:0], rss[:0]
	}
	for rows.Next() {
		var pid int
		var started, command, run string
		var at int64
		var value float64
		if err := rows.Scan(&pid, &started, &command, &run, &at, &value); err != nil {
			return nil, err
		}
		if !historyFinite(value) {
			continue
		}
		if current != nil && (current.PID != pid || historyGrowthStarted(current.Started) != started) {
			finish()
		}
		if current == nil {
			current = &historyGrowthProcess{Kind: "process", Command: command, PID: pid, First: time.Unix(0, at).UTC()}
			if parsed, err := time.Parse(time.RFC3339Nano, started); err == nil {
				current.Started = &parsed
			}
		}
		current.Last = time.Unix(0, at).UTC()
		current.Unfiltered = current.Unfiltered || unfiltered[run]
		days = append(days, float64(at-current.First.UnixNano())/float64(24*time.Hour))
		rss = append(rss, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	finish()
	sort.SliceStable(processes, func(i, j int) bool {
		if processes[i].LeakSuspect != processes[j].LeakSuspect {
			return processes[i].LeakSuspect
		}
		return historyGrowthSlope(processes[i]) > historyGrowthSlope(processes[j])
	})
	return processes, nil
}

func historyGrowthStarted(started *time.Time) string {
	if started == nil {
		return ""
	}
	return started.Format(time.RFC3339Nano)
}

func historyGrowthSlope(process historyGrowthProcess) float64 {
	if process.BytesPerDay == nil {
		return math.Inf(-1)
	}
	return *process.BytesPerDay
}

// historyGrowthDirectories는 du 결과를 경로마다 모아 기간의 첫 결과와 마지막 결과를 report diff와 같은 규칙으로 비교한다.
// 일부만 센 결과는 빠진 디렉터리가 줄어든 것처럼 보이므로 고르지 않는다.
func historyGrowthDirectories(db *sql.DB, from, to time.Time, root string) ([]historyGrowthDirs, error) {
	rows, err := db.Query(`SELECT time_ns, payload FROM snapshot_results
		WHERE (probe=? OR substr(probe, 1, ?)=?) AND time_ns>=? AND time_ns<? ORDER BY time_ns, id`,
		duProbeID, len(duProbeID)+1, duProbeID+":", from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scan struct {
		at      int64
		metrics map[string]interface{}
	}
	firsts, lasts := map[string]scan{}, map[string]scan{}
	counts := map[string]int{}
	var roots []string
	for rows.Next() {
		var at int64
		var payload string
		if err := rows.Scan(&at, &payload); err != nil {
			return nil, err
		}
		var result Result
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, err
		}
		path, _ := result.Metrics["path"].(string)
		partial, _ := result.Metrics["partial"].(bool)
		if _, ok := duLargestOf(result.Metrics); !ok || partial || path == "" || (root != "" && path != root) {
			continue
		}
		if _, seen := firsts[path]; !seen {
			firsts[path] = scan{at: at, metrics: result.Metrics}
			roots = append(roots, path)
		}
		lasts[path] = scan{at: at, metrics: result.Metrics}
		counts[path]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(roots)
	var dirs []historyGrowthDirs
	for _, path := range roots {
		first, last := firsts[path], lasts[path]
		entry := historyGrowthDirs{Kind: "directories", Root: path, Snapshots: counts[path], First: time.Unix(0, first.at).UTC(), Last: time.Unix(0, last.at).UTC()}
		entry.BeforeBytes, _ = metricNumber(first.metrics["bytes"])
		entry.AfterBytes, _ = metricNumber(last.metrics["bytes"])
		if counts[path] < 2 {
			entry.Note = T("history.growth.one_snapshot")
		} else {
			entry.Directories, entry.Note = duCompareDirs(first.metrics, last.metrics)
		}
		dirs = append(dirs, entry)
	}
	return dirs, nil
}

func writeHistoryGrowthJSON(output io.Writer, growth historyGrowth) error {
	encoder := json.NewEncoder(output)
	for _, disk := range growth.Disks {
		if err := encoder.Encode(disk); err != nil {
			return err
		}
	}
	for _, dirs := range growth.Dirs {
		if err := encoder.Encode(dirs); err != nil {
			return err
		}
	}
	for _, process := range growth.Processes {
		if err := encoder.Encode(process); err != nil {
			return err
		}
	}
	return nil
}

func writeHistoryGrowth(output io.Writer, growth historyGrowth, location *time.Location) {
	layout := "2006-01-02 15:04"
	fmt.Fprintln(output, T("history.growth.window", growth.From.In(location).Format(layout), growth.To.In(location).Format(layout)))
	if growth.Process != "" {
		writeHistoryGrowthProcesses(output, growth, location)
		return
	}
	if len(growth.Disks) == 0 && len(growth.Dirs) == 0 {
		fmt.Fprintln(output, T("history.growth.empty"))
		return
	}
	if len(growth.Disks) > 0 {
		fmt.Fprintln(output)
		writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "DISK\tUSED\tTOTAL\tCHANGE/DAY\tFULL IN (ESTIMATE)")
		for _, disk := range growth.Disks {
			rate, full := "—", "—"
			if disk.BytesPerDay != nil {
				rate = historyGrowthRate(*disk.BytesPerDay)
			} else if disk.Samples < historyGrowthMinSamples {
				full = T("history.growth.few_samples", disk.Samples)
			} else {
				full = T("history.growth.short_span")
			}
			if disk.FullInDays != nil {
				full = T("history.growth.full_in", historyGrowthDays(*disk.FullInDays))
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", ansi.Truncate(historyTerminalText(disk.Mount), historyGrowthMountWidth, "…"), formatSizeShort(uint64(math.Max(disk.UsedBytes, 0))),
				formatSizeShort(uint64(math.Max(disk.TotalBytes, 0))), rate, full)
		}
		writer.Flush()
	}
	for _, dirs := range growth.Dirs {
		fmt.Fprintln(output)
		span := dirs.Last.Sub(dirs.First)
		fmt.Fprintln(output, T("history.growth.dirs_title", historyTerminalText(dirs.Root), dirs.First.In(location).Format(layout),
			dirs.Last.In(location).Format(layout), span.Hours()/24, dirs.Snapshots))
		if dirs.Snapshots > 1 {
			change := dirs.AfterBytes - dirs.BeforeBytes
			sign := "+"
			if change < 0 {
				sign = "-"
			}
			fmt.Fprintf(output, "  %s\n", T("history.growth.total", formatSizeShort(uint64(math.Max(dirs.BeforeBytes, 0))),
				formatSizeShort(uint64(math.Max(dirs.AfterBytes, 0))), sign+formatSizeShort(uint64(math.Abs(change)))))
		}
		if dirs.Note != "" {
			fmt.Fprintf(output, "  %s\n", dirs.Note)
		}
		if len(dirs.Directories) == 0 && dirs.Note == "" {
			fmt.Fprintf(output, "  %s\n", T("history.growth.no_change"))
		}
		// 하루 증가량은 기간이 하루 이상일 때만 쓴다. 몇 분 사이의 변화를 하루로 늘리면 실제보다 크게 보인다.
		days := span.Hours() / 24
		unlabeled := make([]duDirDelta, len(dirs.Directories))
		for index, delta := range dirs.Directories {
			unlabeled[index] = delta
			unlabeled[index].Label = ""
		}
		for index, line := range formatDuDirDeltas(unlabeled) {
			delta := dirs.Directories[index]
			if days >= 1 {
				line += "  " + historyGrowthRate(float64(delta.Delta)/days)
			}
			if delta.Label != "" {
				line += "  " + delta.Label
			}
			fmt.Fprintf(output, "  %s\n", historyTerminalText(line))
		}
	}
}

func writeHistoryGrowthProcesses(output io.Writer, growth historyGrowth, location *time.Location) {
	filter := historyTerminalText(growth.Process)
	if len(growth.Processes) == 0 {
		fmt.Fprintln(output, T("history.trend.process_empty", filter, quoteLocalShellWord(growth.Process)))
		return
	}
	fmt.Fprintln(output)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "PROCESS\tPID\tSTARTED\tSAMPLES\tRSS\tCHANGE/DAY\tNOTE")
	unfiltered := false
	for index, process := range growth.Processes {
		if index == historyGrowthProcessRows {
			break
		}
		unfiltered = unfiltered || process.Unfiltered
		started, rate := "-", "—"
		if process.Started != nil {
			started = process.Started.In(location).Format("01-02 15:04")
		}
		note := historyGrowthProcessNote(process)
		if process.BytesPerDay != nil {
			rate = historyGrowthRate(*process.BytesPerDay)
		}
		fmt.Fprintf(writer, "%s\t%d\t%s\t%d\t%s\t%s\t%s\n", ansi.Truncate(historyTerminalText(process.Command), historyGrowthMountWidth, "…"), process.PID, started,
			process.Samples, formatSizeShort(uint64(math.Max(process.RSSBytes, 0))), rate, note)
	}
	writer.Flush()
	if more := len(growth.Processes) - historyGrowthProcessRows; more > 0 {
		fmt.Fprintln(output, T("history.growth.more_processes", more))
	}
	if unfiltered {
		fmt.Fprintln(output, T("history.trend.unfiltered", filter, quoteLocalShellWord(growth.Process)))
	}
}

// historyGrowthProcessNote는 표의 NOTE 칸이다. TUI도 같은 문구를 쓴다.
func historyGrowthProcessNote(process historyGrowthProcess) string {
	switch {
	case process.LeakSuspect:
		return T("history.growth.leak", *process.Fit)
	case process.BytesPerDay != nil:
		return ""
	case process.Samples < historyGrowthMinSamples:
		return T("history.growth.few_samples", process.Samples)
	default:
		return T("history.growth.short_span")
	}
}

func historyGrowthRate(bytesPerDay float64) string {
	sign := "+"
	if bytesPerDay < 0 {
		sign = "-"
	}
	return T("history.growth.per_day", sign+formatSizeShort(uint64(math.Round(math.Abs(bytesPerDay)))))
}

func historyGrowthDays(days float64) string {
	if days < 1 {
		return T("history.growth.hours", math.Max(math.Round(days*24), 1))
	}
	return T("history.growth.days", math.Round(days))
}
