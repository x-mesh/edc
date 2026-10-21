package edc

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// history trend --process는 top 기록의 프로세스 표본으로 trend를 낸다. 표본 시각마다 일치하는 프로세스의 값을 더해
// 서비스 전체 값으로 보고, --each면 (pid, started)마다 따로 본다. 필터 없이 기록한 top -w와 스냅샷은 표본마다 CPU와
// 메모리 상위 몇 개만 남기므로, 프로세스가 기록된 표본 가운데 일치한 비율을 함께 내서 빠진 시각이 있음을 드러낸다.

const (
	// historyProcessNameRows는 프로세스 이름 목록을 만들 때 읽는 최근 표본 수다. 몇 주치 기록 전체를 묶으면 선택 화면이
	// 열리기까지 몇 초가 걸린다.
	historyProcessNameRows  = 200000
	historyProcessNameLimit = 50
)

type historyProcessIdentity struct {
	pid     int
	started string
}

type historyProcessName struct {
	Name      string
	Samples   int
	MaxMemory float64
}

// historyProcessOrder는 프로세스 이름 목록의 정렬 기준이다. SQL에 그대로 들어가므로 아래 두 값만 쓴다.
type historyProcessOrder string

const (
	historyProcessBySamples historyProcessOrder = "samples"
	historyProcessByRSS     historyProcessOrder = "rss"
)

// historyProcessMetricNames는 trend로 볼 수 있는 프로세스 지표다. pid는 숫자지만 지표가 아니다.
func historyProcessMetricNames() []string {
	var names []string
	for _, metric := range historyProcessMetrics {
		if metric.name != "pid" {
			names = append(names, metric.name)
		}
	}
	return names
}

func historyProcessMetric(name string) bool {
	for _, candidate := range historyProcessMetricNames() {
		if candidate == name {
			return true
		}
	}
	return false
}

// historyProcessMatch는 history process와 같은 규칙의 조건이다. PID는 같아야 하고, 이름은 소문자 command의 일부면 된다.
func historyProcessMatch(filter topProcessFilter, alias string) (string, []any) {
	var matches []string
	var values []any
	for pid := range filter.pids {
		matches = append(matches, alias+".pid=?")
		values = append(values, pid)
	}
	for _, name := range filter.names {
		matches = append(matches, "instr("+alias+".command_folded, ?)>0")
		values = append(values, name)
	}
	return "(" + strings.Join(matches, " OR ") + ")", values
}

// historyUnfilteredRuns는 --process 없이 기록한 실행이다. 이 실행의 프로세스 표본은 그 순간의 상위 몇 개뿐이다.
func historyUnfilteredRuns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT id FROM runs WHERE COALESCE(json_extract(payload, '$.process_filter'), '')=''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		runs[id] = true
	}
	return runs, rows.Err()
}

// historyProcessNames는 기록된 프로세스 이름을 order가 큰 순서로 낸다. 쉼표가 든 이름은 --process에서 둘로 나뉘므로 뺀다.
func historyProcessNames(db *sql.DB, order historyProcessOrder) ([]historyProcessName, error) {
	memory := "rss_bytes"
	columns := "command, command_folded, rss_bytes"
	if historyHasProcessMemoryColumn(db) {
		memory = "COALESCE(memory_bytes, rss_bytes)"
		columns += ", memory_bytes"
	}
	rows, err := db.Query(`SELECT MIN(command), COUNT(*) AS samples, COALESCE(MAX(`+memory+`), 0) AS rss FROM
		(SELECT `+columns+` FROM top_process_samples ORDER BY time_ns DESC LIMIT ?)
		WHERE instr(command, ',')=0 AND trim(command)<>'' GROUP BY command_folded ORDER BY `+string(order)+` DESC, command_folded LIMIT ?`,
		historyProcessNameRows, historyProcessNameLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []historyProcessName
	for rows.Next() {
		var name historyProcessName
		if err := rows.Scan(&name.Name, &name.Samples, &name.MaxMemory); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// historyProcessTrendQuery는 프로세스가 기록된 표본마다 일치한 프로세스의 값을 읽는다. 일치한 프로세스가 없는 표본도
// NULL 한 줄로 남겨 seen의 분모가 된다. top_process_samples에는 sample_id index가 없어서 (run_id, time_ns)로 잇는다.
func historyProcessTrendQuery(options historyTrendOptions) (string, []any) {
	match, values := historyProcessMatch(options.process, "p")
	query := `SELECT s.id, s.time_ns, s.run_id, p.pid, p.started, p.` + options.metric + `
		FROM top_samples s LEFT JOIN top_process_samples p
		ON p.run_id=s.run_id AND p.time_ns=s.time_ns AND p.sample_id=s.id AND p.` + options.metric + ` IS NOT NULL AND ` + match + `
		WHERE EXISTS(SELECT 1 FROM top_process_samples q WHERE q.run_id=s.run_id AND q.time_ns=s.time_ns AND q.sample_id=s.id)`
	if options.run != "" {
		query += " AND s.run_id=?"
		values = append(values, options.run)
	}
	if !options.from.IsZero() {
		query += " AND s.time_ns>=?"
		values = append(values, options.from.UnixNano())
	}
	if !options.to.IsZero() {
		query += " AND s.time_ns<?"
		values = append(values, options.to.UnixNano())
	}
	return query + " ORDER BY s.time_ns, s.id", values
}

type historyProcessValue struct {
	identity historyProcessIdentity
	value    float64
}

// historyProcessTrend는 표본을 구간으로 묶는다. 한 구간의 값과 직전 표본의 프로세스 집합만 메모리에 둔다.
type historyProcessTrend struct {
	options    historyTrendOptions
	location   *time.Location
	emit       func(historyTrendBucket) error
	unfiltered map[string]bool
	summary    historyTrendSummary

	start      time.Time
	recorded   int
	sums       []float64
	identities map[historyProcessIdentity]bool
	each       map[historyProcessIdentity][]float64
	order      []historyProcessIdentity
	restarts   int
	partial    bool
	// last는 마지막으로 일치한 프로세스가 있던 표본의 집합이다. 중간에 일치한 표본이 없어도 그 앞뒤로 재시작을 센다.
	// gone은 사라졌지만 아직 새 프로세스로 바뀌지 않은 수다. 종료와 시작이 다른 표본에 잡혀도 재시작으로 센다.
	last         map[historyProcessIdentity]bool
	lastFiltered bool
	gone         int
}

func (trend *historyProcessTrend) sample(at int64, run string, values []historyProcessValue) error {
	current := historyBucketStart(time.Unix(0, at), trend.options.by, trend.location)
	if !current.Equal(trend.start) {
		if err := trend.flush(); err != nil {
			return err
		}
		trend.start = current
	}
	trend.recorded++
	unfiltered := trend.unfiltered[run]
	trend.partial = trend.partial || unfiltered
	if len(values) == 0 {
		return nil
	}
	trend.summary.Unfiltered = trend.summary.Unfiltered || unfiltered
	sum := 0.0
	set := map[historyProcessIdentity]bool{}
	for _, value := range values {
		sum += value.value
		set[value.identity] = true
		trend.identities[value.identity] = true
		if trend.options.each {
			if _, seen := trend.each[value.identity]; !seen {
				trend.order = append(trend.order, value.identity)
			}
			trend.each[value.identity] = append(trend.each[value.identity], value.value)
		}
	}
	trend.sums = append(trend.sums, sum)
	// 필터 없이 기록한 표본에서는 순위 밖으로 밀린 프로세스도 사라진 것처럼 보이므로 재시작을 세지 않는다.
	if !trend.lastFiltered || unfiltered {
		trend.gone = 0
	} else {
		started := 0
		for identity := range trend.last {
			if !set[identity] {
				trend.gone++
			}
		}
		for identity := range set {
			if !trend.last[identity] {
				started++
			}
		}
		replaced := min(started, trend.gone)
		trend.restarts, trend.gone = trend.restarts+replaced, trend.gone-replaced
	}
	trend.last, trend.lastFiltered = set, !unfiltered
	return nil
}

func (trend *historyProcessTrend) flush() error {
	defer func() {
		trend.recorded, trend.sums, trend.restarts, trend.partial = 0, trend.sums[:0], 0, false
		trend.identities, trend.each, trend.order = map[historyProcessIdentity]bool{}, map[historyProcessIdentity][]float64{}, nil
	}()
	if len(trend.sums) == 0 {
		return nil
	}
	base := historyTrendBucket{Start: trend.start, By: trend.options.by, Process: trend.options.process.String(), Metric: trend.options.metric, Samples: trend.recorded}
	if !trend.options.each {
		bucket := base
		bucket.Count, bucket.Procs = len(trend.sums), len(trend.identities)
		bucket.Min, bucket.Avg, bucket.P95, bucket.Max = historyTrendStats(trend.sums)
		if !trend.partial {
			restarts := trend.restarts
			bucket.Restarts = &restarts
		}
		return trend.emit(bucket)
	}
	sort.Slice(trend.order, func(i, j int) bool {
		left, right := trend.order[i], trend.order[j]
		return left.pid < right.pid || (left.pid == right.pid && left.started < right.started)
	})
	for _, identity := range trend.order {
		values := trend.each[identity]
		bucket := base
		bucket.PID, bucket.Count, bucket.Procs = identity.pid, len(values), 1
		if started, err := time.Parse(time.RFC3339Nano, identity.started); err == nil {
			bucket.Started = &started
		}
		bucket.Min, bucket.Avg, bucket.P95, bucket.Max = historyTrendStats(values)
		if err := trend.emit(bucket); err != nil {
			return err
		}
	}
	return nil
}

func historyProcessTrendBuckets(db *sql.DB, options historyTrendOptions, location *time.Location, emit func(historyTrendBucket) error) (historyTrendSummary, error) {
	if options.metric == "memory_bytes" && !historyHasProcessMemoryColumn(db) {
		return historyTrendSummary{}, nil
	}
	unfiltered, err := historyUnfilteredRuns(db)
	if err != nil {
		return historyTrendSummary{}, err
	}
	trend := &historyProcessTrend{options: options, location: location, emit: emit, unfiltered: unfiltered,
		identities: map[historyProcessIdentity]bool{}, each: map[historyProcessIdentity][]float64{}}
	query, values := historyProcessTrendQuery(options)
	rows, err := db.Query(query, values...)
	if err != nil {
		return historyTrendSummary{}, err
	}
	defer rows.Close()
	var sampleID, sampleAt int64
	var sampleRun string
	var matched []historyProcessValue
	pending := false
	for rows.Next() {
		var id, at int64
		var run string
		var pid sql.NullInt64
		var started sql.NullString
		var value sql.NullFloat64
		if err := rows.Scan(&id, &at, &run, &pid, &started, &value); err != nil {
			return historyTrendSummary{}, err
		}
		if pending && id != sampleID {
			if err := trend.sample(sampleAt, sampleRun, matched); err != nil {
				return historyTrendSummary{}, err
			}
			matched = matched[:0]
		}
		sampleID, sampleAt, sampleRun, pending = id, at, run, true
		if pid.Valid && value.Valid && historyFinite(value.Float64) {
			matched = append(matched, historyProcessValue{identity: historyProcessIdentity{pid: int(pid.Int64), started: started.String}, value: value.Float64})
		}
	}
	if err := rows.Err(); err != nil {
		return historyTrendSummary{}, err
	}
	if pending {
		if err := trend.sample(sampleAt, sampleRun, matched); err != nil {
			return historyTrendSummary{}, err
		}
	}
	return trend.summary, trend.flush()
}
