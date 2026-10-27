package edc

import (
	"sort"
	"time"
)

// viewRateWindow는 초당 줄 수를 재는 구간이다.
const viewRateWindow = 10 * time.Second

// viewTopRows는 통계 패널의 상위 목록 길이다.
const viewTopRows = 5

// viewStats는 화면에 보관한 줄의 통계다. 줄을 넣고 뺄 때마다 고쳐서, 화면을 그릴 때 보관한 줄 전체를 다시 세지 않는다.
type viewStats struct {
	// lines는 event 수다. stack trace처럼 앞 줄에 이어지는 줄과 구분 줄은 세지 않는다.
	lines int
	// failures는 level이 error나 fatal이거나 status가 5xx인 줄이다. 둘 다인 줄도 한 번만 센다.
	failures int
	levels   map[string]int
	classes  [6]int
	paths    map[string]int
	clients  map[string]int
	programs map[string]int
	// arrivals는 화면을 연 뒤 새로 들어온 줄 수를 초 단위로 모은 것이다. 처음 읽은 줄은 넣지 않는다. 그 줄들은 한꺼번에
	// 읽혀서 초당 줄 수를 부풀린다.
	arrivals  []viewArrival
	lastError viewLine
}

func newViewStats() viewStats {
	return viewStats{levels: map[string]int{}, paths: map[string]int{}, clients: map[string]int{}, programs: map[string]int{}}
}

func (stats *viewStats) add(line viewLine) {
	if line.marker || line.cont {
		return
	}
	stats.apply(line, 1)
	if viewFailed(line) {
		stats.lastError = line
	}
}

func (stats *viewStats) remove(line viewLine) {
	if line.marker || line.cont {
		return
	}
	stats.apply(line, -1)
	// 오래된 줄부터 빠지므로, 가장 최근 오류가 빠졌다면 남은 오류가 없다.
	if stats.failures == 0 {
		stats.lastError = viewLine{}
	}
}

func (stats *viewStats) apply(line viewLine, step int) {
	stats.lines += step
	if viewFailed(line) {
		stats.failures += step
	}
	if line.level != "" {
		viewCount(stats.levels, line.level, step)
	}
	if line.status != 0 {
		stats.classes[min(line.status/100, 5)] += step
	}
	if line.path != "" {
		viewCount(stats.paths, line.path, step)
	}
	if line.client != "" {
		viewCount(stats.clients, line.client, step)
	}
	if line.program != "" {
		viewCount(stats.programs, line.program, step)
	}
}

func viewCount(counts map[string]int, key string, step int) {
	counts[key] += step
	if counts[key] <= 0 {
		delete(counts, key)
	}
}

type viewArrival struct {
	second time.Time
	count  int
}

func (stats *viewStats) arrived(at time.Time, count int) {
	second := at.Truncate(time.Second)
	if last := len(stats.arrivals) - 1; last >= 0 && stats.arrivals[last].second.Equal(second) {
		stats.arrivals[last].count += count
	} else {
		stats.arrivals = append(stats.arrivals, viewArrival{second, count})
	}
	cut := 0
	for cut < len(stats.arrivals) && at.Sub(stats.arrivals[cut].second) > viewRateWindow {
		cut++
	}
	stats.arrivals = stats.arrivals[cut:]
}

// rate는 최근 viewRateWindow 동안 들어온 줄의 초당 수다.
func (stats viewStats) rate(now time.Time) float64 {
	count := 0
	for _, arrival := range stats.arrivals {
		if now.Sub(arrival.second) <= viewRateWindow {
			count += arrival.count
		}
	}
	return float64(count) / viewRateWindow.Seconds()
}

func viewFailed(line viewLine) bool {
	return line.level == "error" || line.level == "fatal" || line.status >= 500
}

type viewCountRow struct {
	label string
	count int
}

// top은 counts에서 많은 순서로 viewTopRows개다. 수가 같으면 이름 순서라 화면을 다시 그려도 순서가 흔들리지 않는다.
func viewTop(counts map[string]int) []viewCountRow {
	rows := make([]viewCountRow, 0, len(counts))
	for label, count := range counts {
		rows = append(rows, viewCountRow{label, count})
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].count != rows[right].count {
			return rows[left].count > rows[right].count
		}
		return rows[left].label < rows[right].label
	})
	return rows[:min(len(rows), viewTopRows)]
}
