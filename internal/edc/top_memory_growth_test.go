package edc

import (
	"strings"
	"testing"
	"time"
)

// feedTopMemory는 분마다 세 번 기록한다. 가운데 값은 GC 직전처럼 튀어 오른 값이라 최저값에 들지 않아야 한다.
func feedTopMemory(tracker *topMemoryGrowthTracker, start time.Time, minutes int, memory func(minute int) uint64) topProcess {
	var process topProcess
	for minute := 0; minute < minutes; minute++ {
		for index, spike := range []uint64{0, 500 << 20, 0} {
			process = topProcess{PID: 7, Started: start, Command: "node", Memory: memory(minute) + spike, MemoryValid: true}
			processes := []topProcess{process}
			tracker.observe(start.Add(time.Duration(minute)*time.Minute+time.Duration(index)*10*time.Second), processes)
			process = processes[0]
		}
	}
	return process
}

func TestTopMemoryGrowthFindsASteadyLeakThroughGCSpikes(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	leak := func(minute int) uint64 { return 1<<30 + uint64(minute)*(30<<20) }
	if process := feedTopMemory(newTopMemoryGrowthTracker(), start, topMemoryGrowthBuckets-1, leak); process.Growth != nil {
		t.Fatalf("growth before %d buckets: %+v", topMemoryGrowthBuckets, process.Growth)
	}
	process := feedTopMemory(newTopMemoryGrowthTracker(), start, topMemoryGrowthBuckets, leak)
	// 처음 세 구간의 최저값은 0분, 마지막 세 구간의 최저값은 12분에 남는다. 증가량과 시간이 같은 12분을 가리켜야 속도가 맞다.
	if process.Growth == nil || process.Growth.Bytes != 12*(30<<20) || process.Growth.Window != 12*time.Minute {
		t.Fatalf("growth = %+v, want 360 MiB over 12m", process.Growth)
	}
}

func TestTopMemoryGrowthIgnoresSawtoothStepsAndSmallGrowth(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	cases := map[string]func(minute int) uint64{
		// GC가 매번 같은 바닥으로 돌아온다.
		"sawtooth": func(int) uint64 { return 2 << 30 },
		// 시작할 때 cache를 한 번에 채우고 멈춘다.
		"step": func(minute int) uint64 {
			if minute < 2 {
				return 1 << 30
			}
			return 2 << 30
		},
		// 꾸준히 늘지만 15분에 256 MiB가 안 된다.
		"small": func(minute int) uint64 { return 1<<30 + uint64(minute)*(10<<20) },
		// 늘었다가 크게 해제한다.
		"released": func(minute int) uint64 {
			if minute%3 == 2 {
				return 1 << 30
			}
			return 1<<30 + uint64(minute)*(60<<20)
		},
	}
	for name, memory := range cases {
		if process := feedTopMemory(newTopMemoryGrowthTracker(), start, topMemoryGrowthBuckets, memory); process.Growth != nil {
			t.Errorf("%s must not count as growth: %+v", name, process.Growth)
		}
	}
}

func TestTopMemoryGrowthNeedsContinuousBucketsAndForgetsGoneProcesses(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	tracker := newTopMemoryGrowthTracker()
	for minute := 0; minute <= topMemoryGrowthBuckets; minute++ {
		if minute == 5 {
			// 일시정지로 한 구간을 건너뛴다.
			continue
		}
		processes := []topProcess{{PID: 7, Started: start, Memory: uint64(minute) * (100 << 20), MemoryValid: true}}
		tracker.observe(start.Add(time.Duration(minute)*time.Minute), processes)
		if processes[0].Growth != nil {
			t.Fatalf("minute %d: a gap must stop the verdict until %d buckets refill: %+v", minute, topMemoryGrowthBuckets, processes[0].Growth)
		}
	}
	// 같은 PID라도 시작 시각이 다르면 다른 process다. 이전 기록은 지워진다.
	tracker.observe(start.Add(20*time.Minute), []topProcess{{PID: 7, Started: start.Add(time.Hour)}})
	if len(tracker.series) != 1 {
		t.Fatalf("series = %d, want only the new process", len(tracker.series))
	}
}

func TestTopMemoryGrowthSkipsSameMinuteRecalculation(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	tracker := newTopMemoryGrowthTracker()
	processes := []topProcess{{PID: 7, Started: start, Memory: 1 << 30, MemoryValid: true}}
	tracker.observe(start, processes)
	series := tracker.series[topProcessKey{pid: 7, started: start}]
	checks := series.checks
	tracker.observe(start.Add(10*time.Second), processes)
	if series.checks != checks {
		t.Fatalf("growth checks = %d, want %d", series.checks, checks)
	}
}

func TestTopProcessCandidatesKeepGrowingProcesses(t *testing.T) {
	processes := []topProcess{}
	for pid := 1; pid <= 2*topProcessLimit; pid++ {
		processes = append(processes, topProcess{PID: pid, CPU: float64(100 - pid), RSS: 1 << 30, Command: "big"})
	}
	processes = append(processes, topProcess{PID: 99, RSS: 300 << 20, Command: "leak", Growth: &topMemoryGrowth{Bytes: 400 << 20, Window: 15 * time.Minute}})
	for _, process := range topProcessCandidates(processes) {
		if process.PID == 99 {
			return
		}
	}
	t.Fatal("a growing process must stay among the candidates")
}

func TestTopMemoryGrowthSignalNamesTheFastestAndRanksByTimeLeft(t *testing.T) {
	growth := topMemoryGrowth{Bytes: 360 << 20, Window: 15 * time.Minute}
	processes := []topProcess{
		{PID: 1, Command: "node", Growth: &growth},
		{PID: 2, Command: "java", Growth: &topMemoryGrowth{Bytes: 300 << 20, Window: 15 * time.Minute}},
		{PID: 3, Command: "busy", CPU: 90},
	}
	item, ok := topMemoryGrowthSignal(processes, 64<<30, 50)
	if !ok || item.text != "node +360.0M/15m" || item.kind != "memory growth 1" || item.view != topViewMemory || item.level() != topLevelWarn {
		t.Fatalf("signal = %+v, %v", item, ok)
	}
	// 남은 memory가 320 MiB이면 이 속도(0.4 MiB/s)로 1시간 안에 바닥난다.
	if item, _ := topMemoryGrowthSignal(processes, 64<<30, 99.5); item.level() != topLevelDanger {
		t.Fatalf("near exhaustion must be danger: %+v", item)
	}
	if _, ok := topMemoryGrowthSignal(processes[2:], 64<<30, 50); ok {
		t.Fatal("no growth, no signal")
	}
	items := topDashboardSignalItems(resourceRate{MemoryPercent: 50}, processes, nil, true, topLimits{memoryTotal: 64 << 30})
	found := false
	for _, item := range items {
		found = found || strings.HasPrefix(item.text, "node +")
	}
	if !found {
		t.Fatalf("dashboard signals = %+v", items)
	}
}

// jw-server에서 증가 이벤트의 원인이 같은 이름 묶음과 더 큰 계단 process로 적혔다.
func TestTopEventCulpritNamesTheGrowingProcessForMemory(t *testing.T) {
	row := topDashboardRow{processesValid: true, processes: []topProcess{
		{PID: 1, Command: "python3", RSS: 1 << 30},
		{PID: 2, Command: "python3", RSS: 545 << 20, Growth: &topMemoryGrowth{Bytes: 355 << 20, Window: 15 * time.Minute}},
		{PID: 3, Command: "claude", RSS: 371 << 20},
	}, processTotal: topProcessTotal{Groups: []topProcessGroup{{Name: "python3", Count: 3, Memory: 1600 << 20}}}}
	if got := topEventCulprit(row, topSignalItem{view: topViewMemory, kind: topMemoryGrowthKind + "2"}); got != "python3 +355.0M 545.0M" {
		t.Fatalf("growth culprit = %q", got)
	}
	// host memory 경고는 크기 순위의 묶음과 process를 그대로 적는다. 늘어나는 작은 process가 큰 process를 가리면 안 된다.
	if got := topEventCulprit(row, topSignalItem{view: topViewMemory, kind: "mem"}); strings.Contains(got, "+355") || !strings.Contains(got, "python3 (3 procs") {
		t.Fatalf("memory culprit = %q", got)
	}
	if got := topEventCulprit(row, topSignalItem{view: topViewCPU, kind: "cpu"}); strings.Contains(got, "+355") {
		t.Fatalf("other views keep their own ranking: %q", got)
	}
}

// 메모리 보기는 크기 순으로 다섯 줄만 보이므로, 늘어나는 작은 process가 signal에만 나오고 줄에서는 빠질 수 있다.
func TestTopMemoryViewShowsTheGrowingProcessBehindLargerOnes(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = false
	processes := []topProcess{}
	for pid := 1; pid <= topProcessLimit; pid++ {
		processes = append(processes, topProcess{PID: pid, RSS: 1 << 30, Command: "big"})
	}
	processes = append(processes, topProcess{PID: 99, RSS: 300 << 20, Command: "leak", Growth: &topMemoryGrowth{Bytes: 400 << 20, Window: 15 * time.Minute}})
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: processes}}
	model = topAfter(t, model, topKey("m"))
	model.width, model.height = 120, 40
	if lines := strings.Join(model.candidateLines(), "\n"); !strings.Contains(lines, "leak") {
		t.Fatalf("the growing process must have a row in the memory view:\n%s", lines)
	}
}

// 같은 일을 하는 worker 여럿이 함께 늘어도 가장 큰 process가 다섯 줄에서 밀려나면 안 된다. 앞에 오는 것은 signal이 가리키는 하나뿐이다.
func TestTopMemoryViewPutsOnlyTheFastestGrowerFirst(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = false
	processes := []topProcess{{PID: 1, RSS: 10 << 30, Command: "postgres"}}
	for pid := 2; pid <= 6; pid++ {
		processes = append(processes, topProcess{PID: pid, RSS: 400 << 20, Command: "worker", Growth: &topMemoryGrowth{Bytes: uint64(300+pid) << 20, Window: 12 * time.Minute}})
	}
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: processes}}
	model = topAfter(t, model, topKey("m"))
	candidates := model.candidates()
	if len(candidates) < 2 || candidates[0].PID != 6 || candidates[1].PID != 1 {
		t.Fatalf("order = %v, want the fastest grower, then postgres", topCandidatePIDs(candidates))
	}
}

// RSS로 대신한 값은 늘어나도 흐린 색으로 남는다. 경고 색으로 칠하면 정확한 footprint로 읽힌다.
func TestTopGrowingProcessKeepsTheDimEstimatedCell(t *testing.T) {
	model := topFixtureModel(nil)
	model.limits.color = true
	processes := []topProcess{{PID: 9, RSS: 600 << 20, Command: "leak", Growth: &topMemoryGrowth{Bytes: 400 << 20, Window: 12 * time.Minute}}}
	model.rows = []topDashboardRow{{at: time.Unix(1, 0), processesValid: true, processes: processes}}
	model = topAfter(t, model, topKey("m"))
	model.width, model.height = 120, 40
	dim := topProcessMemoryCell(formatProcessRSS(600<<20), true, true)
	if lines := strings.Join(model.candidateLines(), "\n"); !strings.Contains(lines, dim) {
		t.Fatalf("the estimated cell must stay dim %q:\n%q", dim, lines)
	}
}

func topCandidatePIDs(processes []topProcess) []int {
	pids := make([]int, 0, len(processes))
	for _, process := range processes {
		pids = append(pids, process.PID)
	}
	return pids
}
