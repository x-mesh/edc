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
)

type historyGrowthOptions struct {
	from, to time.Time
	root     string
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
	From, To time.Time
	Disks    []historyGrowthDisk
	Dirs     []historyGrowthDirs
}

func runHistoryGrowth(args []string) int {
	set := newFlagSet("history growth")
	set.SetOutput(os.Stderr)
	var options historyGrowthOptions
	from := set.String("from", "", T("history.option.from"))
	to := set.String("to", "", T("history.option.to"))
	set.StringVar(&options.root, "root", "", T("history.option.root"))
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
	var meanX, meanY float64
	for index := range x {
		meanX += x[index]
		meanY += y[index]
	}
	meanX /= float64(len(x))
	meanY /= float64(len(y))
	var covariance, variance float64
	for index := range x {
		covariance += (x[index] - meanX) * (y[index] - meanY)
		variance += (x[index] - meanX) * (x[index] - meanX)
	}
	if variance == 0 {
		return 0
	}
	return covariance / variance
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
	return nil
}

func writeHistoryGrowth(output io.Writer, growth historyGrowth, location *time.Location) {
	layout := "2006-01-02 15:04"
	fmt.Fprintln(output, T("history.growth.window", growth.From.In(location).Format(layout), growth.To.In(location).Format(layout)))
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
