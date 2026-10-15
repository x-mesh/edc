package edc

import (
	"math"
	"slices"
	"time"
)

const (
	// topMemoryGrowthBucket은 memory 최저값을 남기는 구간이다. GC는 구간 안에서 오르내리므로 최저값만 남기면
	// 힙이 늘었다 줄어드는 톱니가 걸러지고, 해제되지 않고 쌓이는 양만 남는다.
	topMemoryGrowthBucket = time.Minute
	// topMemoryGrowthBuckets는 판정에 쓰는 연속 구간 수다. 15분을 채우기 전에는 판정하지 않는다.
	topMemoryGrowthBuckets = 15
	// topMemoryGrowthRisingPairs는 이웃한 구간 최저값 14쌍 중 줄지 않아야 하는 쌍의 수다. 한두 번의 큰 해제는 넘긴다.
	topMemoryGrowthRisingPairs = 12
	// topMemoryGrowthGrowingPairs는 그중 실제로 늘어난 쌍의 수다. 시작할 때 cache를 한 번에 채우고 멈춘 계단은 누수가 아니다.
	topMemoryGrowthGrowingPairs = 7
	// topMemoryGrowthEdge는 처음과 마지막 구간의 최저값을 고를 때 묶는 구간 수다. 한 구간의 튀는 값에 흔들리지 않게 한다.
	topMemoryGrowthEdge = 3
	// topMemoryGrowthMin은 처음과 마지막 최저값 사이에 늘어야 하는 최소량이다. 작은 cache가 몇 MB씩 느는 것은 누수로 보지 않는다.
	topMemoryGrowthMin = 256 << 20
	// topMemoryGrowthDangerHorizon은 이 속도로 남은 memory를 다 쓰는 데 걸리는 시간이 이보다 짧으면 위험으로 보는 한도다.
	topMemoryGrowthDangerHorizon = time.Hour
)

// topMemoryGrowth는 꾸준히 늘어난 process의 증가량이다. Bytes는 처음과 마지막 구간 묶음의 최저값 차이이고,
// Window는 그 두 최저값을 남긴 구간 사이의 시간이다. 15분 창 전체가 아니라 실제로 비교한 간격이라 속도가 맞다.
type topMemoryGrowth struct {
	Bytes  uint64
	Window time.Duration
}

// perSecond는 증가 속도(byte/s)다.
func (growth topMemoryGrowth) perSecond() float64 {
	return float64(growth.Bytes) / growth.Window.Seconds()
}

type topProcessKey struct {
	pid     int
	started time.Time
}

// topMemorySeries는 process 하나의 구간별 최저값이다. 구간 번호를 함께 남겨 빠진 구간을 알아본다.
type topMemorySeries struct {
	buckets  []int64
	minimums []uint64
}

// topMemoryGrowthTracker는 모든 process의 memory를 구간마다 기록해 지금은 작아도 계속 늘어나는 process를 찾는다.
// 크기 순위만으로는 큰 process에 밀려 누수를 늦게 알아챈다.
type topMemoryGrowthTracker struct {
	series map[topProcessKey]*topMemorySeries
	// start는 처음 기록한 시각이다. 구간 번호를 여기서 지난 시간으로 정해, 시계를 뒤로 돌려도 번호가 거꾸로 가지 않는다.
	// time.Now()의 monotonic 값으로 빼므로 NTP 보정에 흔들리지 않는다.
	start time.Time
}

func newTopMemoryGrowthTracker() *topMemoryGrowthTracker {
	return &topMemoryGrowthTracker{series: map[topProcessKey]*topMemorySeries{}}
}

// observe는 processes의 memory를 기록하고, 꾸준히 늘어난 process에 Growth를 채운다. 목록에 없는 process의 기록은 지운다.
func (tracker *topMemoryGrowthTracker) observe(at time.Time, processes []topProcess) {
	if tracker.start.IsZero() {
		tracker.start = at
	}
	bucket := int64(at.Sub(tracker.start) / topMemoryGrowthBucket)
	seen := make(map[topProcessKey]bool, len(processes))
	for index := range processes {
		process := &processes[index]
		key := topProcessKey{pid: process.PID, started: process.Started}
		seen[key] = true
		series := tracker.series[key]
		if series == nil {
			series = &topMemorySeries{}
			tracker.series[key] = series
		}
		series.add(bucket, process.memory())
		if growth, ok := series.growth(); ok {
			process.Growth = &growth
		}
	}
	for key := range tracker.series {
		if !seen[key] {
			delete(tracker.series, key)
		}
	}
}

func (series *topMemorySeries) add(bucket int64, memory uint64) {
	if last := len(series.buckets) - 1; last >= 0 && series.buckets[last] == bucket {
		series.minimums[last] = min(series.minimums[last], memory)
		return
	}
	series.buckets = append(series.buckets, bucket)
	series.minimums = append(series.minimums, memory)
	if extra := len(series.buckets) - topMemoryGrowthBuckets; extra > 0 {
		series.buckets, series.minimums = series.buckets[extra:], series.minimums[extra:]
	}
}

// growth는 최근 구간이 연속으로 다 찼고 최저값이 꾸준히 올랐을 때만 증가량을 돌려준다. 일시정지나 긴 --interval로
// 구간이 비면 그만큼 시간이 길어져 속도를 부풀리므로 판정하지 않는다.
func (series *topMemorySeries) growth() (topMemoryGrowth, bool) {
	if len(series.buckets) < topMemoryGrowthBuckets || series.buckets[len(series.buckets)-1]-series.buckets[0] != topMemoryGrowthBuckets-1 {
		return topMemoryGrowth{}, false
	}
	rising, growing := 0, 0
	for index := 1; index < len(series.minimums); index++ {
		if series.minimums[index] >= series.minimums[index-1] {
			rising++
		}
		if series.minimums[index] > series.minimums[index-1] {
			growing++
		}
	}
	head := series.minimums[:topMemoryGrowthEdge]
	tailStart := len(series.minimums) - topMemoryGrowthEdge
	tail := series.minimums[tailStart:]
	first, last := slices.Min(head), slices.Min(tail)
	if rising < topMemoryGrowthRisingPairs || growing < topMemoryGrowthGrowingPairs || last < first || last-first < topMemoryGrowthMin {
		return topMemoryGrowth{}, false
	}
	from := series.buckets[slices.Index(head, first)]
	to := series.buckets[tailStart+slices.Index(tail, last)]
	return topMemoryGrowth{Bytes: last - first, Window: time.Duration(to-from) * topMemoryGrowthBucket}, true
}

// topMemoryGrowthScore는 위험 한도를 이 속도로 남은 memory를 다 쓰기까지의 시간으로 나눈 값이다. 1 이상이면 위험이다.
// 남은 양을 모르면 경고 단계의 값을 돌려준다.
func topMemoryGrowthScore(growth topMemoryGrowth, memoryTotal uint64, memoryPercent float64) float64 {
	const unknown = 0.5
	if memoryTotal == 0 || growth.perSecond() <= 0 {
		return unknown
	}
	available := float64(memoryTotal) * math.Max(0, 100-memoryPercent) / 100
	return topMemoryGrowthDangerHorizon.Seconds() * growth.perSecond() / math.Max(available, 1)
}

// topFastestGrowing은 가장 많이 늘어난 process다. 늘어난 process가 없으면 false다.
func topFastestGrowing(processes []topProcess) (topProcess, bool) {
	var leading *topProcess
	for index := range processes {
		if growth := processes[index].Growth; growth != nil && (leading == nil || growth.Bytes > leading.Growth.Bytes) {
			leading = &processes[index]
		}
	}
	if leading == nil {
		return topProcess{}, false
	}
	return *leading, true
}
