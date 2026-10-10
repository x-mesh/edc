package edc

import "time"

// topProbeBuckets는 top_process_bpf.c의 HIST_BUCKETS다. bucket i는 [2^i, 2^(i+1)) 마이크로초다.
const topProbeBuckets = 24

// topProbeSource는 값을 센 방법이다. 어떤 값을 셀 수 있는지가 방법에 따라 정해진다.
type topProbeSource string

const (
	topProbeSourceEBPF    topProbeSource = "ebpf"
	topProbeSourceLibproc topProbeSource = "libproc"
)

// measuresIO와 hasHistogram은 eBPF만 참이다. macOS libproc은 누적 counter라 대기 분포가 없고 block I/O 지연을 셀 수 없다.
// 방법을 모르는 값도 세지 못한 것으로 둔다. 0건으로 보이면 I/O가 없었다고 잘못 읽는다.
func (source topProbeSource) measuresIO() bool   { return source == topProbeSourceEBPF }
func (source topProbeSource) hasHistogram() bool { return source == topProbeSourceEBPF }

func (source topProbeSource) String() string {
	if source == "" {
		return "unknown"
	}
	return string(source)
}

// topProbeStats는 한 window 동안 observer가 센 값이다. window는 직전 관측부터 이번 관측까지의 시간이다.
type topProbeStats struct {
	Window time.Duration
	Source topProbeSource
	// Measured는 값을 센 process 수이고, Unreadable은 권한이 없어 읽지 못한 process 수다. macOS는 root가 아니면
	// 다른 사용자의 process를 읽지 못한다. 하나도 세지 못했으면 대기 값은 0이 아니라 모르는 값이다.
	Measured, Unreadable int
	RunqCount, RunqSumNS uint64
	RunqHist             [topProbeBuckets]uint64
	IOCount, IOBytes     uint64
	IOSumNS              uint64
	IOHist               [topProbeBuckets]uint64
}

func (stats *topProbeStats) add(other topProbeStats) {
	if other.Window > stats.Window {
		stats.Window = other.Window
	}
	if stats.Source == "" {
		stats.Source = other.Source
	}
	stats.Measured += other.Measured
	stats.Unreadable += other.Unreadable
	stats.RunqCount += other.RunqCount
	stats.RunqSumNS += other.RunqSumNS
	stats.IOCount += other.IOCount
	stats.IOBytes += other.IOBytes
	stats.IOSumNS += other.IOSumNS
	for index := range stats.RunqHist {
		stats.RunqHist[index] += other.RunqHist[index]
		stats.IOHist[index] += other.IOHist[index]
	}
}

// sub는 누적값 current에서 previous를 뺀다. 카운터가 줄었다면 map이 비워졌다는 뜻이므로 current를 그대로 쓴다.
func (current topProbeStats) sub(previous topProbeStats) topProbeStats {
	if current.RunqCount < previous.RunqCount || current.IOCount < previous.IOCount {
		return current
	}
	delta := current
	delta.RunqCount -= previous.RunqCount
	delta.RunqSumNS -= previous.RunqSumNS
	delta.IOCount -= previous.IOCount
	delta.IOBytes -= previous.IOBytes
	delta.IOSumNS -= previous.IOSumNS
	for index := range delta.RunqHist {
		delta.RunqHist[index] -= min(previous.RunqHist[index], delta.RunqHist[index])
		delta.IOHist[index] -= min(previous.IOHist[index], delta.IOHist[index])
	}
	return delta
}

// unmeasured는 읽은 process 없이 권한 거부만 있었다는 뜻이다. 이때 대기 값은 0이 아니라 모르는 값이다.
func (stats topProbeStats) unmeasured() bool {
	return stats.Measured == 0 && stats.Unreadable > 0
}

// topProbeAverageMS는 합과 개수에서 평균 지연(ms)을 구한다. 개수가 0이면 값이 없다.
func topProbeAverageMS(sumNS, count uint64) (float64, bool) {
	if count == 0 {
		return 0, false
	}
	return float64(sumNS) / float64(count) / float64(time.Millisecond), true
}

// topProbePercentileMS는 histogram에서 percentile(0~1)이 든 bucket의 위쪽 경계(ms)다. 실제 값은 그 경계 아래에 있다.
func topProbePercentileMS(hist [topProbeBuckets]uint64, percentile float64) (float64, bool) {
	var total uint64
	for _, count := range hist {
		total += count
	}
	if total == 0 {
		return 0, false
	}
	threshold := uint64(float64(total)*percentile + 0.999999)
	if threshold < 1 {
		threshold = 1
	}
	var seen uint64
	for index, count := range hist {
		seen += count
		if seen >= threshold {
			return float64(uint64(1)<<(index+1)) / 1000, true
		}
	}
	return float64(uint64(1)<<topProbeBuckets) / 1000, true
}

// topProbeObserver는 pid 목록을 감시하고, 직전 관측 이후 pid별로 센 값을 돌려준다. 방금 감시를 시작한 pid는 기준이 없어 빠진다.
type topProbeObserver func(pids []int) map[int]topProbeStats
