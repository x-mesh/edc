package edc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTopProbePercentileReportsTheUpperEdgeOfTheBucket(t *testing.T) {
	var hist [topProbeBuckets]uint64
	if _, ok := topProbePercentileMS(hist, 0.95); ok {
		t.Fatal("an empty histogram has no percentile")
	}
	// bucket 0은 [1,2)us, bucket 10은 [1024,2048)us다. 100개 중 95개가 bucket 0, 5개가 bucket 10이다.
	hist[0], hist[10] = 95, 5
	if got, _ := topProbePercentileMS(hist, 0.95); got != 0.002 {
		t.Fatalf("p95 = %v ms, want 0.002", got)
	}
	if got, _ := topProbePercentileMS(hist, 0.96); got != 2.048 {
		t.Fatalf("p96 = %v ms, want 2.048", got)
	}
	hist[topProbeBuckets-1] = 1000
	if got, _ := topProbePercentileMS(hist, 1); got != float64(uint64(1)<<topProbeBuckets)/1000 {
		t.Fatalf("the last bucket must report its edge, got %v", got)
	}
}

func TestTopProbeAverageNeedsASample(t *testing.T) {
	if _, ok := topProbeAverageMS(500, 0); ok {
		t.Fatal("no events must not give an average")
	}
	if got, ok := topProbeAverageMS(3_000_000, 2); !ok || got != 1.5 {
		t.Fatalf("average = %v, %v", got, ok)
	}
}

func TestTopProbeStatsSubtractsTheEarlierReadAndAddsAcrossProcesses(t *testing.T) {
	previous := topProbeStats{RunqCount: 10, RunqSumNS: 1000, RunqHist: &topProbeHist{}, IO: &topProbeIO{Count: 2, Bytes: 4096, SumNS: 50}}
	previous.RunqHist[3], previous.IO.Hist[5] = 10, 2
	current := topProbeStats{RunqCount: 15, RunqSumNS: 2500, RunqHist: &topProbeHist{}, IO: &topProbeIO{Count: 3, Bytes: 8192, SumNS: 90}}
	current.RunqHist[3], current.RunqHist[4], current.IO.Hist[5] = 12, 3, 3
	delta := current.sub(previous)
	if delta.RunqCount != 5 || delta.RunqSumNS != 1500 || delta.IO.Count != 1 || delta.IO.Bytes != 4096 || delta.IO.SumNS != 40 || delta.RunqHist[3] != 2 || delta.RunqHist[4] != 3 || delta.IO.Hist[5] != 1 {
		t.Fatalf("delta = %+v %+v %+v", delta, *delta.RunqHist, *delta.IO)
	}
	// 카운터가 줄었다면 map이 비워진 것이다. 음수로 돌아가지 않고 현재 값을 쓴다.
	if reset := (topProbeStats{RunqCount: 1}).sub(previous); reset.RunqCount != 1 {
		t.Fatalf("reset = %+v", reset)
	}
	merged := topProbeStats{Window: time.Second}
	merged.add(delta)
	merged.add(topProbeStats{Window: 2 * time.Second, RunqCount: 1, RunqHist: &topProbeHist{}, IO: &topProbeIO{Count: 4}})
	if merged.RunqCount != 6 || merged.IO.Count != 5 || merged.Window != 2*time.Second || merged.RunqHist[3] != 2 {
		t.Fatalf("merged = %+v %+v", merged, *merged.IO)
	}
}

// 포인터 필드를 공유하면 합계를 더할 때 process별 값이, 차이를 구할 때 tracer가 보관한 이전 값이 바뀐다.
func TestTopProbeStatsArithmeticLeavesItsInputsAlone(t *testing.T) {
	first := topProbeStats{RunqCount: 1, RunqHist: &topProbeHist{1}, IO: &topProbeIO{Count: 1, Hist: topProbeHist{1}}}
	second := topProbeStats{RunqCount: 2, RunqHist: &topProbeHist{2}, IO: &topProbeIO{Count: 2, Hist: topProbeHist{2}}}
	total := topProbeStats{}
	total.add(first)
	total.add(second)
	if first.IO.Count != 1 || first.RunqHist[0] != 1 || first.IO.Hist[0] != 1 || total.IO.Count != 3 || total.RunqHist[0] != 3 {
		t.Fatalf("first %+v, total %+v", *first.IO, *total.IO)
	}
	previous := topProbeStats{RunqCount: 1, RunqHist: &topProbeHist{1}, IO: &topProbeIO{Count: 1}}
	current := topProbeStats{RunqCount: 3, RunqHist: &topProbeHist{3}, IO: &topProbeIO{Count: 3}}
	delta := current.sub(previous)
	delta.IO.Count, delta.RunqHist[0] = 99, 99
	if current.IO.Count != 3 || current.RunqHist[0] != 3 || previous.IO.Count != 1 || previous.RunqHist[0] != 1 {
		t.Fatalf("sub changed its inputs: current %+v, previous %+v", *current.IO, *previous.IO)
	}
}

// eBPF가 아닌 값은 분포와 I/O가 nil이라 합쳐도 생기지 않는다.
func TestTopProbeStatsKeepMissingValuesMissing(t *testing.T) {
	total := topProbeStats{}
	total.add(topProbeStats{Source: topProbeSourceLibproc, Measured: 1, RunqCount: 4})
	if total.IO != nil || total.RunqHist != nil {
		t.Fatalf("total = %+v", total)
	}
}

func TestTopProcessSamplerObservesEveryMatchAndTotalsTheCounts(t *testing.T) {
	all := []topProcess{{PID: 1, CPU: 90, Command: "worker"}, {PID: 2, CPU: 50, Command: "worker"}, {PID: 3, CPU: 80, Command: "other"}}
	var watched []int
	sampler := &topProcessSampler{read: func() ([]topProcess, bool) { return append([]topProcess(nil), all...), true }}
	sampler.setObserver(func(pids []int) map[int]topProbeStats {
		watched = pids
		return map[int]topProbeStats{1: {RunqCount: 3, RunqSumNS: 300}, 2: {RunqCount: 1, RunqSumNS: 100}}
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
	if processes[0].Probe == nil || processes[0].Probe.RunqCount != 3 || processes[1].Probe.RunqCount != 1 {
		t.Fatalf("per process values = %+v", processes)
	}
	if total.Probe == nil || total.Probe.RunqCount != 4 || total.Probe.RunqSumNS != 400 {
		t.Fatalf("total = %+v", total.Probe)
	}
}

func TestTopProbeSampleLeavesOutLatenciesWithoutEvents(t *testing.T) {
	if newTopProbeSample(nil) != nil {
		t.Fatal("no eBPF values must give no field")
	}
	stats := topProbeStats{Window: 1500 * time.Millisecond, Source: topProbeSourceEBPF, Measured: 1, RunqCount: 4, RunqSumNS: 6_000_000, RunqHist: &topProbeHist{}, IO: &topProbeIO{}}
	stats.RunqHist[10] = 4
	data, err := json.Marshal(newTopProbeSample(&stats))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"source":"ebpf"`, `"window_s":1.5`, `"io_supported":true`, `"p95_supported":true`, `"runq_count":4`, `"runq_avg_ms":1.5`, `"runq_p95_ms":2.048`, `"io_ops":0`, `"io_bytes":0`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("sample is missing %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "io_avg_ms") || strings.Contains(string(data), "io_p95_ms") || strings.Contains(string(data), "context_switches") {
		t.Fatalf("no I/O events must leave out the latencies, and eBPF has no context switches: %s", data)
	}
}

func TestTopProbeDetailShowsTheWindowAndBothDelays(t *testing.T) {
	stats := topProbeStats{Window: 2 * time.Second, Source: topProbeSourceEBPF, Measured: 1, RunqCount: 10, RunqSumNS: 20_000_000, RunqHist: &topProbeHist{}, IO: &topProbeIO{}}
	stats.RunqHist[11] = 10
	got := topProbeDetail(stats)
	if got != "ebpf 2s · runq 10 avg 2.00ms p95 <4.096ms · io 0 —" {
		t.Fatalf("detail = %q", got)
	}
	lines := topMatchDetail([]topProcess{{PID: 1, CPU: 1, Command: "x"}}, topProcessTotal{Count: 1, CPU: 1, Probe: &stats}, true)
	if len(lines) != 3 || lines[2] != "  "+got {
		t.Fatalf("match detail = %q", lines)
	}
}

// macOS의 libproc counter는 분포와 I/O 지연이 없다. 0건으로 보이면 I/O가 없었다고 잘못 읽는다.
func TestTopProbeUnsupportedValuesAreNotShownAsZero(t *testing.T) {
	stats := topProbeStats{Window: time.Second, Source: topProbeSourceLibproc, Measured: 1, RunqCount: 4, RunqSumNS: 6_000_000}
	if got := topProbeDetail(stats); got != "libproc 1s · runq 4 avg 1.50ms · io n/a" {
		t.Fatalf("detail = %q", got)
	}
	data, err := json.Marshal(newTopProbeSample(&stats))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"source":"libproc"`, `"io_supported":false`, `"p95_supported":false`, `"context_switches":4`, `"runq_count":4`, `"runq_avg_ms":1.5`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("sample is missing %s: %s", want, data)
		}
	}
	for _, absent := range []string{"io_ops", "io_bytes", "runq_p95_ms"} {
		if strings.Contains(string(data), absent) {
			t.Fatalf("sample must leave out %s: %s", absent, data)
		}
	}
	row := topDashboardRow{processesValid: true, processTotal: topProcessTotal{Count: 1, CPU: 1, Probe: &stats}}
	cells := topProcessViewCells(row)
	if cells[7].text != "1.50" || cells[8].text != "n/a" {
		t.Fatalf("runq and io cells = %q, %q", cells[7].text, cells[8].text)
	}
	merged := topProbeStats{}
	merged.add(stats)
	if merged.Source != topProbeSourceLibproc || merged.IO != nil || merged.Measured != 1 {
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
	stats := topProbeStats{Source: topProbeSourceLibproc, Measured: 1, RunqCount: 4, RunqSumNS: 6_000_000}
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, filter: "worker", processTotal: topProcessTotal{Count: 1, CPU: 10, RSS: 1 << 20, Probe: &stats}}}
	model.selected = 0
	banner := model.processBanner()[0]
	if !strings.Contains(banner, "runq 1.50ms") || strings.Contains(banner, "io ") {
		t.Fatalf("banner = %q", banner)
	}
}

// 권한이 없어 하나도 읽지 못한 값은 0이나 no ev가 아니라 root로 보이고, JSON은 runq 값을 뺀다.
func TestTopProbeUnreadableProcessesAreNotShownAsNoEvents(t *testing.T) {
	refused := topProbeStats{Window: time.Second, Source: topProbeSourceLibproc, Unreadable: 2}
	if got := topProbeDetail(refused); got != "libproc 1s · runq n/a · io n/a · 2 need root" {
		t.Fatalf("detail = %q", got)
	}
	data, err := json.Marshal(newTopProbeSample(&refused))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"unreadable":2`) || !strings.Contains(string(data), `"io_supported":false`) || strings.Contains(string(data), "runq_count") || strings.Contains(string(data), "context_switches") {
		t.Fatalf("sample = %s", data)
	}
	cells := topProcessViewCells(topDashboardRow{processesValid: true, processTotal: topProcessTotal{Count: 2, CPU: 1, Probe: &refused}})
	if cells[7].text != "root" || cells[8].text != "n/a" {
		t.Fatalf("runq and io cells = %q, %q", cells[7].text, cells[8].text)
	}
	// 하나라도 읽었으면 그 값을 보이고 읽지 못한 수를 덧붙인다. 0 switch도 잰 값이다.
	partly := refused
	partly.add(topProbeStats{Source: topProbeSourceLibproc, Measured: 1})
	if got := topProbeDetail(partly); got != "libproc 1s · runq 0 — · io n/a · 2 need root" {
		t.Fatalf("partly read detail = %q", got)
	}
	if data, _ := json.Marshal(newTopProbeSample(&partly)); !strings.Contains(string(data), `"runq_count":0`) {
		t.Fatalf("partly read sample = %s", data)
	}
}

// source가 빠진 값은 모르는 방법이라 I/O를 0으로 보이지 않고, 상세 줄 앞이 비지 않는다.
func TestTopProbeStatsWithoutSourceDoNotClaimIO(t *testing.T) {
	stats := topProbeStats{Window: time.Second, Measured: 1, RunqCount: 1, RunqSumNS: 1_000_000}
	if got := topProbeDetail(stats); !strings.HasPrefix(got, "unknown 1s") || !strings.Contains(got, "io n/a") {
		t.Fatalf("detail = %q", got)
	}
}
