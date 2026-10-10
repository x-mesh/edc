package edc

import "time"

// topBPFBuckets는 top_process_bpf.c의 HIST_BUCKETS다. bucket i는 [2^i, 2^(i+1)) 마이크로초다.
const topBPFBuckets = 24

// topBPFStats는 한 window 동안 observer가 센 값이다. window는 직전 관측부터 이번 관측까지의 시간이다.
type topBPFStats struct {
	Window time.Duration
	// Source는 값을 센 방법이다. Linux는 eBPF, macOS는 libproc의 누적 counter다.
	Source string
	// RunqHistUnsupported와 IOUnsupported는 이 platform이 대기 분포나 block I/O 지연을 셀 수 없다는 뜻이다.
	// 0건과 구분해야 I/O가 없었다고 잘못 읽지 않는다.
	RunqHistUnsupported, IOUnsupported bool
	RunqCount, RunqSumNS               uint64
	RunqHist                           [topBPFBuckets]uint64
	IOCount, IOBytes                   uint64
	IOSumNS                            uint64
	IOHist                             [topBPFBuckets]uint64
}

func (stats *topBPFStats) add(other topBPFStats) {
	if other.Window > stats.Window {
		stats.Window = other.Window
	}
	if stats.Source == "" {
		stats.Source = other.Source
	}
	stats.RunqHistUnsupported = stats.RunqHistUnsupported || other.RunqHistUnsupported
	stats.IOUnsupported = stats.IOUnsupported || other.IOUnsupported
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
func (current topBPFStats) sub(previous topBPFStats) topBPFStats {
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

// topBPFAverageMS는 합과 개수에서 평균 지연(ms)을 구한다. 개수가 0이면 값이 없다.
func topBPFAverageMS(sumNS, count uint64) (float64, bool) {
	if count == 0 {
		return 0, false
	}
	return float64(sumNS) / float64(count) / float64(time.Millisecond), true
}

// topBPFPercentileMS는 histogram에서 percentile(0~1)이 든 bucket의 위쪽 경계(ms)다. 실제 값은 그 경계 아래에 있다.
func topBPFPercentileMS(hist [topBPFBuckets]uint64, percentile float64) (float64, bool) {
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
	return float64(uint64(1)<<topBPFBuckets) / 1000, true
}

// topBPFObserver는 pid 목록을 감시하고, 직전 관측 이후 pid별로 센 값을 돌려준다. 방금 감시를 시작한 pid는 기준이 없어 빠진다.
type topBPFObserver func(pids []int) map[int]topBPFStats
