package edc

import "time"

// topProbeBuckets는 top_process_bpf.c의 HIST_BUCKETS다. bucket i는 [2^i, 2^(i+1)) 마이크로초다.
const topProbeBuckets = 24

// topProbeSource는 값을 센 방법이다. 보이는 이름일 뿐이고, 어떤 값이 있는지는 topProbeStats의 nil 여부가 정한다.
type topProbeSource string

const (
	topProbeSourceEBPF    topProbeSource = "ebpf"
	topProbeSourceLibproc topProbeSource = "libproc"
)

func (source topProbeSource) String() string {
	if source == "" {
		return "unknown"
	}
	return string(source)
}

// topProbeHist는 bucket별 개수다.
type topProbeHist [topProbeBuckets]uint64

// topProbeIO는 block I/O 요청 수, byte, 요청부터 완료까지의 시간이다. eBPF만 센다.
type topProbeIO struct {
	Count, Bytes, SumNS uint64
	Hist                topProbeHist
}

// topProbeStats는 한 window 동안 observer가 센 값이다. window는 직전 관측부터 이번 관측까지의 시간이다.
// 셀 수 없는 값은 nil이다. macOS libproc은 누적 counter라 대기 분포(RunqHist)가 없고 block I/O(IO)를 셀 수 없다.
// 0건과 구분해야 I/O가 없었다고 잘못 읽지 않는다.
type topProbeStats struct {
	Window time.Duration
	Source topProbeSource
	// Measured는 값을 센 process 수이고, Unreadable은 권한이 없어 읽지 못한 process 수다. macOS는 root가 아니면
	// 다른 사용자의 process를 읽지 못한다. 하나도 세지 못했으면 대기 값은 0이 아니라 모르는 값이다.
	Measured, Unreadable int
	// RunqCount는 Linux에서는 깨어나 CPU를 받은 횟수, macOS에서는 context switch 수다. RunqSumNS는 그동안의 대기 합이다.
	RunqCount, RunqSumNS uint64
	RunqHist             *topProbeHist
	IO                   *topProbeIO
}

// add와 sub는 포인터 필드를 새로 만든다. 받은 값의 포인터에 쓰면 tracer가 보관한 이전 값이나 process별 값이 바뀐다.
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
	if other.RunqHist != nil {
		hist := topProbeHist{}
		if stats.RunqHist != nil {
			hist = *stats.RunqHist
		}
		for index := range hist {
			hist[index] += other.RunqHist[index]
		}
		stats.RunqHist = &hist
	}
	if other.IO != nil {
		io := topProbeIO{}
		if stats.IO != nil {
			io = *stats.IO
		}
		io.Count += other.IO.Count
		io.Bytes += other.IO.Bytes
		io.SumNS += other.IO.SumNS
		for index := range io.Hist {
			io.Hist[index] += other.IO.Hist[index]
		}
		stats.IO = &io
	}
}

// sub는 누적값 current에서 previous를 뺀다. 카운터가 줄었다면 map이 비워졌다는 뜻이므로 current를 그대로 쓴다.
func (current topProbeStats) sub(previous topProbeStats) topProbeStats {
	if current.RunqCount < previous.RunqCount || (current.IO != nil && previous.IO != nil && current.IO.Count < previous.IO.Count) {
		return current
	}
	delta := current
	delta.RunqCount -= previous.RunqCount
	delta.RunqSumNS -= previous.RunqSumNS
	if current.RunqHist != nil {
		hist := *current.RunqHist
		if previous.RunqHist != nil {
			for index := range hist {
				hist[index] -= min(previous.RunqHist[index], hist[index])
			}
		}
		delta.RunqHist = &hist
	}
	if current.IO != nil {
		io := *current.IO
		if previous.IO != nil {
			io.Count -= previous.IO.Count
			io.Bytes -= previous.IO.Bytes
			io.SumNS -= previous.IO.SumNS
			for index := range io.Hist {
				io.Hist[index] -= min(previous.IO.Hist[index], io.Hist[index])
			}
		}
		delta.IO = &io
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
func topProbePercentileMS(hist topProbeHist, percentile float64) (float64, bool) {
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
