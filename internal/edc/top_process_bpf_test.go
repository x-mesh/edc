package edc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTopBPFPercentileReportsTheUpperEdgeOfTheBucket(t *testing.T) {
	var hist [topBPFBuckets]uint64
	if _, ok := topBPFPercentileMS(hist, 0.95); ok {
		t.Fatal("an empty histogram has no percentile")
	}
	// bucket 0은 [1,2)us, bucket 10은 [1024,2048)us다. 100개 중 95개가 bucket 0, 5개가 bucket 10이다.
	hist[0], hist[10] = 95, 5
	if got, _ := topBPFPercentileMS(hist, 0.95); got != 0.002 {
		t.Fatalf("p95 = %v ms, want 0.002", got)
	}
	if got, _ := topBPFPercentileMS(hist, 0.96); got != 2.048 {
		t.Fatalf("p96 = %v ms, want 2.048", got)
	}
	hist[topBPFBuckets-1] = 1000
	if got, _ := topBPFPercentileMS(hist, 1); got != float64(uint64(1)<<topBPFBuckets)/1000 {
		t.Fatalf("the last bucket must report its edge, got %v", got)
	}
}

func TestTopBPFAverageNeedsASample(t *testing.T) {
	if _, ok := topBPFAverageMS(500, 0); ok {
		t.Fatal("no events must not give an average")
	}
	if got, ok := topBPFAverageMS(3_000_000, 2); !ok || got != 1.5 {
		t.Fatalf("average = %v, %v", got, ok)
	}
}

func TestTopBPFStatsSubtractsTheEarlierReadAndAddsAcrossProcesses(t *testing.T) {
	previous := topBPFStats{RunqCount: 10, RunqSumNS: 1000, IOCount: 2, IOBytes: 4096, IOSumNS: 50}
	previous.RunqHist[3], previous.IOHist[5] = 10, 2
	current := topBPFStats{RunqCount: 15, RunqSumNS: 2500, IOCount: 3, IOBytes: 8192, IOSumNS: 90}
	current.RunqHist[3], current.RunqHist[4], current.IOHist[5] = 12, 3, 3
	delta := current.sub(previous)
	if delta.RunqCount != 5 || delta.RunqSumNS != 1500 || delta.IOCount != 1 || delta.IOBytes != 4096 || delta.IOSumNS != 40 || delta.RunqHist[3] != 2 || delta.RunqHist[4] != 3 || delta.IOHist[5] != 1 {
		t.Fatalf("delta = %+v", delta)
	}
	// 카운터가 줄었다면 map이 비워진 것이다. 음수로 돌아가지 않고 현재 값을 쓴다.
	if reset := (topBPFStats{RunqCount: 1}).sub(previous); reset.RunqCount != 1 {
		t.Fatalf("reset = %+v", reset)
	}
	merged := topBPFStats{Window: time.Second}
	merged.add(delta)
	merged.add(topBPFStats{Window: 2 * time.Second, RunqCount: 1, IOCount: 4})
	if merged.RunqCount != 6 || merged.IOCount != 5 || merged.Window != 2*time.Second || merged.RunqHist[3] != 2 {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestTopProcessSamplerObservesEveryMatchAndTotalsTheCounts(t *testing.T) {
	all := []topProcess{{PID: 1, CPU: 90, Command: "worker"}, {PID: 2, CPU: 50, Command: "worker"}, {PID: 3, CPU: 80, Command: "other"}}
	var watched []int
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return append([]topProcess(nil), all...), true }}
	sampler.setObserver(func(pids []int) map[int]topBPFStats {
		watched = pids
		return map[int]topBPFStats{1: {RunqCount: 3, RunqSumNS: 300}, 2: {RunqCount: 1, RunqSumNS: 100}}
	})
	sampler.refresh()
	if watched != nil {
		t.Fatal("eBPF must not watch anything without a filter")
	}
	filter, _ := parseTopProcessFilter("worker")
	sampler.setFilter(filter)
	processes, total, valid := sampler.refreshNow()
	if !valid || len(watched) != 2 || watched[0] != 1 || watched[1] != 2 {
		t.Fatalf("watched %v, valid %v", watched, valid)
	}
	if processes[0].BPF == nil || processes[0].BPF.RunqCount != 3 || processes[1].BPF.RunqCount != 1 {
		t.Fatalf("per process values = %+v", processes)
	}
	if total.BPF == nil || total.BPF.RunqCount != 4 || total.BPF.RunqSumNS != 400 {
		t.Fatalf("total = %+v", total.BPF)
	}
}

func TestTopBPFSampleLeavesOutLatenciesWithoutEvents(t *testing.T) {
	if newTopBPFSample(nil) != nil {
		t.Fatal("no eBPF values must give no field")
	}
	stats := topBPFStats{Window: 1500 * time.Millisecond, Source: "ebpf", RunqCount: 4, RunqSumNS: 6_000_000, IOBytes: 0}
	stats.RunqHist[10] = 4
	data, err := json.Marshal(newTopBPFSample(&stats))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"source":"ebpf"`, `"window_s":1.5`, `"runq_count":4`, `"runq_avg_ms":1.5`, `"runq_p95_ms":2.048`, `"io_ops":0`, `"io_bytes":0`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("sample is missing %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "io_avg_ms") || strings.Contains(string(data), "io_p95_ms") {
		t.Fatalf("no I/O events must leave out the latencies: %s", data)
	}
}

func TestTopBPFDetailShowsTheWindowAndBothDelays(t *testing.T) {
	stats := topBPFStats{Window: 2 * time.Second, Source: "ebpf", RunqCount: 10, RunqSumNS: 20_000_000}
	stats.RunqHist[11] = 10
	got := topBPFDetail(stats)
	if got != "ebpf 2s · runq 10 avg 2.00ms p95 <4.096ms · io 0 —" {
		t.Fatalf("detail = %q", got)
	}
	lines := topMatchDetail([]topProcess{{PID: 1, CPU: 1, Command: "x"}}, topProcessTotal{Count: 1, CPU: 1, BPF: &stats}, true)
	if len(lines) != 3 || lines[2] != "  "+got {
		t.Fatalf("match detail = %q", lines)
	}
}

// macOS의 libproc counter는 분포와 I/O 지연이 없다. 0건으로 보이면 I/O가 없었다고 잘못 읽는다.
func TestTopBPFUnsupportedValuesAreNotShownAsZero(t *testing.T) {
	stats := topBPFStats{Window: time.Second, Source: "libproc", RunqCount: 4, RunqSumNS: 6_000_000, RunqHistUnsupported: true, IOUnsupported: true}
	if got := topBPFDetail(stats); got != "libproc 1s · runq 4 avg 1.50ms · io n/a" {
		t.Fatalf("detail = %q", got)
	}
	data, err := json.Marshal(newTopBPFSample(&stats))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"source":"libproc"`) || !strings.Contains(string(data), `"runq_avg_ms":1.5`) {
		t.Fatalf("sample = %s", data)
	}
	for _, absent := range []string{"io_ops", "io_bytes", "runq_p95_ms"} {
		if strings.Contains(string(data), absent) {
			t.Fatalf("sample must leave out %s: %s", absent, data)
		}
	}
	row := topDashboardRow{processesValid: true, processTotal: topProcessTotal{Count: 1, CPU: 1, BPF: &stats}}
	cells := topProcessViewCells(row)
	if cells[7].text != "1.50" || cells[8].text != "n/a" {
		t.Fatalf("runq and io cells = %q, %q", cells[7].text, cells[8].text)
	}
	merged := topBPFStats{}
	merged.add(stats)
	if merged.Source != "libproc" || !merged.IOUnsupported || !merged.RunqHistUnsupported {
		t.Fatalf("merged = %+v", merged)
	}
}

// 배너는 폭이 좁아 값만 보인다. 셀 수 없는 I/O는 n/a 대신 아예 빼고 runq만 남긴다.
func TestTopProcessBannerLeavesOutUnsupportedIO(t *testing.T) {
	filter, err := parseTopProcessFilter("worker")
	if err != nil {
		t.Fatal(err)
	}
	model := topFixtureModel(nil).withProcessFilter(filter)
	// 폭이 좁으면 뒤의 항목이 폭 때문에 빠져서 io를 뺐는지 알 수 없다.
	model.limits.color, model.width = false, 200
	stats := topBPFStats{Source: "libproc", RunqCount: 4, RunqSumNS: 6_000_000, IOCount: 2, IOSumNS: 500_000, IOUnsupported: true, RunqHistUnsupported: true}
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, filter: "worker", processTotal: topProcessTotal{Count: 1, CPU: 10, RSS: 1 << 20, BPF: &stats}}}
	model.selected = 0
	banner := model.processBanner()[0]
	if !strings.Contains(banner, "runq 1.50ms") || strings.Contains(banner, "io ") {
		t.Fatalf("banner = %q", banner)
	}
}
