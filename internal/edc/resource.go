package edc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type resourceSnapshot struct {
	NetworkHealth   *networkHealth
	TakenAt         time.Time
	CPUUser         uint64
	CPUSystem       uint64
	CPUIOWait       uint64
	CPUIdle         uint64
	CPUTotal        uint64
	NetInBytes      uint64
	NetOutBytes     uint64
	PacketsIn       uint64
	PacketsOut      uint64
	NetErrors       uint64
	NetDrops        uint64
	DiskRead        uint64
	DiskWrite       uint64
	DiskOps         uint64
	DiskWaitMS      uint64
	DiskBusyMS      uint64
	MemoryUsed      uint64
	MemoryTotal     uint64
	Load1           float64
	Cores           []resourceCPU
	PSICPU          float64
	PSIMemory       float64
	PSIIO           float64
	PSIMemoryFull   float64
	PSIIOFull       float64
	PSIValid        bool
	Processes       []topProcess
	ProcessesValid  bool
	ProcessesAt     time.Time
	ProcessTotal    topProcessTotal
	NetHealthValid  bool
	DiskHealthValid bool
	// DiskBusyValid는 busy 시간을 읽었는지다. macOS는 IOPS와 await는 주지만 I/O가 진행 중이던 시간은 주지 않는다.
	DiskBusyValid bool
	// NetMissing과 DiskMissing은 이 sample이 누적 counter를 읽지 못했다는 뜻이다.
	// 0으로 남은 counter를 기준으로 다음 rate를 구하면 부팅 뒤 누적값 전체가 한 구간에 몰린다.
	NetMissing  bool
	DiskMissing bool
	// SwapOutBytes는 부팅 뒤 memory가 모자라 kernel이 swap으로 내보낸 누적 byte다.
	SwapOutBytes uint64
	SwapMissing  bool
	// DiskQueueMS는 진행 중인 I/O 수에 시간을 곱해 쌓은 값이다. 구간 시간으로 나누면 평균 큐 길이다. busy와 같은 diskstats에서 읽는다.
	DiskQueueMS uint64
	// CPUSteal은 hypervisor가 다른 guest에 CPU를 내준 시간이다. macOS는 이 값을 주지 않는다.
	CPUSteal      uint64
	CPUStealValid bool
	// ProcsBlocked는 지금 I/O를 기다리며 멈춘(D state) 작업 수다. 누적값이 아니라 현재 값이다.
	ProcsBlocked      uint64
	ProcsBlockedValid bool
}

type topProcess struct {
	PID     int
	CPU     float64
	RSS     uint64
	Command string
	// Started는 process가 시작한 시각이다. PID가 재사용돼도 (PID, Started)는 process 하나를 가리킨다. 모르면 zero다.
	Started time.Time
	// Threads는 thread 수이고 모르면 0이다.
	Threads int
	// State는 /proc/<pid>/stat의 state 문자다. 모르면 빈 문자열이다. "D"는 I/O를 기다리며 멈춘 상태다.
	State string
	// FDs는 열린 file descriptor 수이고 모르면 0이다. 필터로 고른 process에만 읽는다.
	FDs    int
	Limits *topProcessLimits
	// DiskValid는 DiskRead와 DiskWrite를 구했는지다. 이 값은 storage에 닿은 byte의 초당 rate이고,
	// 읽을 권한이 없거나 직전 기준이 없으면 false다.
	DiskValid           bool
	DiskRead, DiskWrite float64
	DiskStatus          string
	// BPF는 eBPF가 직전 window 동안 센 값이다. --ebpf가 아니거나 아직 기준이 없으면 nil이다.
	BPF *topBPFStats
}

type topLimitStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type topFDLimit struct {
	topLimitStatus
	Used      *int    `json:"used,omitempty"`
	Soft      *uint64 `json:"soft_limit,omitempty"`
	Unlimited bool    `json:"unlimited,omitempty"`
}

type topCgroupMemory struct {
	topLimitStatus
	Used      *uint64 `json:"used_bytes,omitempty"`
	Max       *uint64 `json:"local_max_bytes,omitempty"`
	Unlimited bool    `json:"local_max_unlimited,omitempty"`
}

type topCgroupEvents struct {
	topLimitStatus
	OOM     *uint64 `json:"oom,omitempty"`
	OOMKill *uint64 `json:"oom_kill,omitempty"`
}

type topCgroupCPU struct {
	topLimitStatus
	WindowS          *float64 `json:"window_s,omitempty"`
	Periods          *uint64  `json:"periods,omitempty"`
	ThrottledPeriods *uint64  `json:"throttled_periods,omitempty"`
	ThrottledUS      *uint64  `json:"throttled_usec,omitempty"`
}

type topCgroupLimits struct {
	topLimitStatus
	Path   string          `json:"path,omitempty"`
	Key    string          `json:"-"`
	Memory topCgroupMemory `json:"memory"`
	Events topCgroupEvents `json:"local_events"`
	CPU    topCgroupCPU    `json:"cpu"`
}

type topProcessLimits struct {
	FD     topFDLimit      `json:"fd"`
	Cgroup topCgroupLimits `json:"cgroup"`
}

func processLimitsOf(process topProcess) *topProcessLimits {
	if process.Limits != nil {
		return process.Limits
	}
	status := topLimitStatus{Status: "unsupported", Reason: "resource limits require Linux"}
	if runtime.GOOS == "linux" {
		status = topLimitStatus{Status: "unavailable", Reason: "resource limits were not collected"}
	}
	return &topProcessLimits{FD: topFDLimit{topLimitStatus: status}, Cgroup: topCgroupLimits{
		topLimitStatus: status, Memory: topCgroupMemory{topLimitStatus: status},
		Events: topCgroupEvents{topLimitStatus: status}, CPU: topCgroupCPU{topLimitStatus: status},
	}}
}

// topProcessTotal은 필터에 맞은 process 전체의 합이다. 목록은 CPU 상위만 남기지만 합은 모두 센다.
type topProcessTotal struct {
	Count   int
	CPU     float64
	RSS     uint64
	Threads int
	// BPF는 감시하는 모든 process의 eBPF 값을 더한 것이다.
	BPF *topBPFStats
	// Groups는 같은 실행 파일 이름의 process를 묶은 합이다. 필터가 없을 때 채우고, I/O 합은 모든 process의 I/O를 읽는
	// 디스크 보기에서만 들어간다.
	Groups []topProcessGroup
}

// topProcessGroup은 같은 실행 파일 이름의 process를 묶은 합이다. 하나하나는 작아도 수가 많아 디스크를 채우는 경우를 보인다.
type topProcessGroup struct {
	Name    string
	Count   int
	Blocked int
	CPU     float64
	RSS     uint64
	// DiskValid는 I/O를 읽은 process가 하나라도 있는지다. DiskRead와 DiskWrite는 읽은 process만 더한 값이다.
	DiskValid           bool
	DiskRead, DiskWrite float64
}

// topProcessGroupLimit은 디스크 보기에 보이는 묶음 수다. 후보 목록이 밀려나지 않게 작게 둔다.
const topProcessGroupLimit = 2

// 묶음이 보이는 최소 합이다. 상주 daemon 몇 개의 작은 사용량이 후보 칸을 차지하지 않게 한다.
const (
	// topProcessGroupMinIO는 I/O 합(byte/s)이다.
	topProcessGroupMinIO = 1 << 20
	// topProcessGroupMinCPU는 CPU 합(core 하나가 100%)이다.
	topProcessGroupMinCPU = 10
	// topProcessGroupMinRSS는 RSS 합(byte)이다.
	topProcessGroupMinRSS = 100 << 20
)

// topProcessGroupName은 묶음 기준인 실행 파일 이름이다. 전체 명령줄이면 인자를 떼고, 경로면 마지막 요소만 남긴다.
func topProcessGroupName(command string) string {
	name := strings.TrimSpace(command)
	if topFullCommand {
		if index := strings.IndexByte(name, ' '); index >= 0 {
			name = name[:index]
		}
	}
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndex(name, "/")+1:]
	}
	return name
}

// groupTopProcesses는 process를 실행 파일 이름으로 묶고, I/O·CPU·RSS 기준으로 각각 앞선 묶음을 모은다.
// 보기마다 다른 기준으로 고르므로 세 기준의 상위 묶음을 함께 넘기고, 행마다 남는 묶음 수를 작게 묶어 둔다.
func groupTopProcesses(processes []topProcess) []topProcessGroup {
	indexes := map[string]int{}
	groups := []topProcessGroup{}
	for _, process := range processes {
		name := topProcessGroupName(process.Command)
		index, seen := indexes[name]
		if !seen {
			index = len(groups)
			indexes[name] = index
			groups = append(groups, topProcessGroup{Name: name})
		}
		group := &groups[index]
		group.Count++
		group.CPU += process.CPU
		group.RSS += process.RSS
		if process.State == topProcessStateBlocked {
			group.Blocked++
		}
		if process.DiskValid {
			group.DiskValid = true
			group.DiskRead += process.DiskRead
			group.DiskWrite += process.DiskWrite
		}
	}
	kept, seen := []topProcessGroup{}, map[string]bool{}
	for _, ranked := range [][]topProcessGroup{topProcessGroupsByIO(groups), topProcessGroupsByCPU(groups), topProcessGroupsByRSS(groups)} {
		for _, group := range ranked {
			if !seen[group.Name] {
				kept = append(kept, group)
				seen[group.Name] = true
			}
		}
	}
	return kept
}

// topProcessGroupsBy는 둘 이상 모였고 keep을 만족하는 묶음을 before 순으로 topProcessGroupLimit개 고른다.
func topProcessGroupsBy(groups []topProcessGroup, keep func(topProcessGroup) bool, before func(a, b topProcessGroup) bool) []topProcessGroup {
	kept := []topProcessGroup{}
	for _, group := range groups {
		if group.Count >= 2 && keep(group) {
			kept = append(kept, group)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return before(kept[i], kept[j]) })
	return kept[:min(topProcessGroupLimit, len(kept))]
}

// topProcessGroupsByIO는 I/O를 topProcessGroupMinIO 이상 쓰거나 I/O를 기다리는 묶음을 I/O 합, 대기 수, process 수 순으로 고른다.
func topProcessGroupsByIO(groups []topProcessGroup) []topProcessGroup {
	return topProcessGroupsBy(groups, func(group topProcessGroup) bool {
		return group.DiskRead+group.DiskWrite >= topProcessGroupMinIO || group.Blocked > 0
	}, func(a, b topProcessGroup) bool {
		if before, after := a.DiskRead+a.DiskWrite, b.DiskRead+b.DiskWrite; before != after {
			return before > after
		}
		if a.Blocked != b.Blocked {
			return a.Blocked > b.Blocked
		}
		return a.Count > b.Count
	})
}

func topProcessGroupsByCPU(groups []topProcessGroup) []topProcessGroup {
	return topProcessGroupsBy(groups, func(group topProcessGroup) bool { return group.CPU >= topProcessGroupMinCPU },
		func(a, b topProcessGroup) bool { return a.CPU > b.CPU })
}

func topProcessGroupsByRSS(groups []topProcessGroup) []topProcessGroup {
	return topProcessGroupsBy(groups, func(group topProcessGroup) bool { return group.RSS >= topProcessGroupMinRSS },
		func(a, b topProcessGroup) bool { return a.RSS > b.RSS })
}

func totalTopProcesses(processes []topProcess) topProcessTotal {
	total := topProcessTotal{Count: len(processes)}
	for _, process := range processes {
		total.CPU += process.CPU
		total.RSS += process.RSS
		total.Threads += process.Threads
	}
	return total
}

const (
	// topProcessRefresh는 process 목록을 다시 읽는 최소 간격이다. 관측 주기가 짧아도 host 부담을 묶어 둔다.
	topProcessRefresh = time.Second
	// topProcessLimit은 필터가 없을 때 CPU 순으로 남기는 process 수다.
	topProcessLimit = 5
)

// topProcessSampler는 process 목록을 배경에서 갱신한다. 대시보드 tick은 마지막 결과만 받아 가므로
// process 수집 시간이 관측 주기에 붙지 않는다.
type topProcessSampler struct {
	mutex sync.Mutex
	// refreshing은 refresh를 하나씩만 돌린다. 대시보드가 실행 중에 필터를 걸면 배경 갱신과 refreshNow가 겹칠 수 있고,
	// read의 CPU tick 기록은 동시에 쓰면 깨진다.
	refreshing     sync.Mutex
	processes      []topProcess
	valid, running bool
	updated        time.Time
	read           func() ([]topProcess, bool)
	// enrich는 목록에 남은 process에만 비싼 값(I/O, fd)을 채운다. 없으면 채우지 않는다.
	enrich func([]topProcess)
	// fillIO는 필터가 없을 때 process의 I/O rate만 채운다. enrich보다 싸서 후보마다 매번 돌려도 된다.
	fillIO func([]topProcess)
	// scanIO는 후보를 고르기 전에 모든 process의 I/O를 읽을지다. 디스크 보기에서만 켠다.
	scanIO bool
	// observe는 eBPF로 필터에 맞은 process를 감시한다. 없으면 감시하지 않는다.
	observe   topBPFObserver
	filter    topProcessFilter
	filterSeq int
	total     topProcessTotal
}

// topFullCommand는 process 목록의 Command에 실행 파일 이름 대신 전체 명령줄을 담을지다. runTop이 표본을
// 읽기 전에 한 번 정한다.
var topFullCommand bool

// linuxProcessCommandLine은 /proc/<pid>/cmdline의 NUL로 나뉜 argv를 한 줄로 잇는다. 커널 thread는 cmdline이
// 비어 있어서 빈 문자열을 돌려주고, 부른 쪽이 comm을 그대로 쓴다.
func linuxProcessCommandLine(data []byte) string {
	args := make([]string, 0, 8)
	for _, arg := range strings.Split(string(data), "\x00") {
		if arg != "" {
			args = append(args, arg)
		}
	}
	return strings.Join(args, " ")
}

var processSampler = &topProcessSampler{read: newTopProcessReader(), enrich: newTopProcessEnricher(), fillIO: newTopProcessIOFiller()}

// topProcessIOTracker는 /proc/<pid>/io를 두 번 읽은 차이로 process의 I/O rate를 구한다.
// root가 아니면 다른 사용자의 process를 읽지 못해 그 process는 DiskValid가 false로 남는다.
type topProcessIOTracker struct {
	previous map[int]linuxProcessIOSample
}

func (tracker *topProcessIOTracker) fill(processes []topProcess, now time.Time) {
	current := make(map[int]linuxProcessIOSample, len(processes))
	for index := range processes {
		process := &processes[index]
		data, err := os.ReadFile("/proc/" + strconv.Itoa(process.PID) + "/io")
		if err != nil {
			continue
		}
		counters, ok := parseLinuxProcessIO(string(data))
		if !ok {
			continue
		}
		current[process.PID] = linuxProcessIOSample{io: counters, at: now}
		before, seen := tracker.previous[process.PID]
		seconds := now.Sub(before.at).Seconds()
		if !seen || seconds <= 0 || counters.read < before.io.read || counters.write < before.io.write {
			continue
		}
		process.DiskValid = true
		process.DiskRead = float64(counters.read-before.io.read) / seconds
		process.DiskWrite = float64(counters.write-before.io.write) / seconds
	}
	tracker.previous = current
}

func newTopProcessIOFiller() func([]topProcess) {
	tracker := &topProcessIOTracker{}
	return func(processes []topProcess) { tracker.fill(processes, time.Now()) }
}

// topProcessIOTotal은 process가 storage에 읽고 쓴 byte rate의 합이다. 읽지 못했으면 -1이다.
func topProcessIOTotal(process topProcess) float64 {
	if !process.DiskValid {
		return -1
	}
	return process.DiskRead + process.DiskWrite
}

// setScanIO는 이후 refresh부터 후보를 고르기 전에 모든 process의 I/O를 읽을지 정한다.
func (sampler *topProcessSampler) setScanIO(on bool) {
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	sampler.scanIO = on
}

func (sampler *topProcessSampler) latest() ([]topProcess, bool) {
	processes, _, valid := sampler.latestWithTotal()
	return processes, valid
}

func (sampler *topProcessSampler) latestWithTotal() ([]topProcess, topProcessTotal, bool) {
	processes, total, valid, _ := sampler.latestWithTotalAt()
	return processes, total, valid
}

func (sampler *topProcessSampler) latestWithTotalAt() ([]topProcess, topProcessTotal, bool, time.Time) {
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	if !sampler.running && time.Since(sampler.updated) >= topProcessRefresh {
		sampler.running = true
		go sampler.refresh()
	}
	return append([]topProcess(nil), sampler.processes...), sampler.total, sampler.valid, sampler.updated
}

// refreshNow는 배경 갱신을 기다리지 않고 지금 읽는다. --json은 sample마다 새 값과 정확한 window가 필요하다.
// 대시보드의 latest와 함께 쓰지 않는다.
func (sampler *topProcessSampler) refreshNow() ([]topProcess, topProcessTotal, bool) {
	processes, total, valid, _ := sampler.refreshNowAt()
	return processes, total, valid
}

func (sampler *topProcessSampler) refreshNowAt() ([]topProcess, topProcessTotal, bool, time.Time) {
	sampler.refresh()
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	return append([]topProcess(nil), sampler.processes...), sampler.total, sampler.valid, sampler.updated
}

func (sampler *topProcessSampler) setFilter(filter topProcessFilter) {
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	if sampler.filter.String() == filter.String() {
		return
	}
	sampler.filter = filter
	sampler.filterSeq++
	sampler.processes, sampler.total, sampler.valid, sampler.updated = nil, topProcessTotal{}, false, time.Time{}
}

// setObserver는 이후 refresh부터 필터에 맞은 process를 eBPF로 감시하게 한다.
func (sampler *topProcessSampler) setObserver(observe topBPFObserver) {
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	sampler.observe = observe
}

func (sampler *topProcessSampler) refresh() {
	sampler.refreshing.Lock()
	defer sampler.refreshing.Unlock()
	sampler.mutex.Lock()
	filter, observe, filterSeq, scanIO := sampler.filter, sampler.observe, sampler.filterSeq, sampler.scanIO
	sampler.mutex.Unlock()
	processes, valid := sampler.read()
	// 필터는 CPU 순위를 자르기 전에 건다. 자른 뒤에 걸면 CPU가 낮은 process가 목록에 들지 못해 항상 비어 보인다.
	processes = filter.apply(processes)
	total := topProcessTotal{}
	if filter.active() {
		total = totalTopProcesses(processes)
	}
	// 목록은 CPU가 높은 순이라 감시 개수를 넘으면 가장 바쁜 process부터 감시한다.
	var observed map[int]topBPFStats
	if filter.active() && observe != nil {
		pids := make([]int, 0, len(processes))
		for _, process := range processes {
			pids = append(pids, process.PID)
		}
		observed = observe(pids)
		if len(observed) > 0 {
			merged := topBPFStats{}
			for _, stats := range observed {
				merged.add(stats)
			}
			total.BPF = &merged
		}
	}
	if filter.active() {
		if limit := filter.limit(); len(processes) > limit {
			processes = processes[:limit]
		}
	} else if scanIO && sampler.fillIO != nil {
		// 디스크 대기는 CPU와 메모리에 드러나지 않으므로 I/O가 큰 process를 후보에 넣으려면 전부 읽어야 한다.
		sampler.fillIO(processes)
		total.Groups = groupTopProcesses(processes)
		processes = topProcessCandidatesByIO(processes)
	} else {
		total.Groups = groupTopProcesses(processes)
		processes = topProcessCandidates(processes)
		if sampler.fillIO != nil {
			sampler.fillIO(processes)
		}
	}
	for index := range processes {
		if stats, ok := observed[processes[index].PID]; ok {
			processes[index].BPF = &stats
		}
	}
	// /proc를 읽는 일이라 lock 밖에서 한다. running이 refresh 하나만 돌게 하므로 enrich는 겹치지 않는다.
	if filter.active() && sampler.enrich != nil {
		sampler.enrich(processes)
	}
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	if filterSeq != sampler.filterSeq {
		sampler.running = false
		return
	}
	// 실패해도 시각과 결과를 남긴다. 수집기가 없는 host에서 매 tick 다시 돌지 않고, 낡은 목록이 유효하게 남지 않는다.
	sampler.processes, sampler.total, sampler.valid, sampler.updated, sampler.running = processes, total, valid, time.Now(), false
}

func sortTopProcessesByCPU(processes []topProcess) []topProcess {
	sort.Slice(processes, func(i, j int) bool { return processes[i].CPU > processes[j].CPU })
	return processes
}

func topProcessCandidates(processes []topProcess) []topProcess {
	candidates := append([]topProcess(nil), processes[:min(topProcessLimit, len(processes))]...)
	byMemory := append([]topProcess(nil), processes...)
	sort.SliceStable(byMemory, func(i, j int) bool { return byMemory[i].RSS > byMemory[j].RSS })
	seen := make(map[int]bool, len(candidates))
	for _, process := range candidates {
		seen[process.PID] = true
	}
	for _, process := range byMemory[:min(topProcessLimit, len(byMemory))] {
		if !seen[process.PID] {
			candidates = append(candidates, process)
			seen[process.PID] = true
		}
	}
	// D state process는 CPU를 거의 쓰지 않아 두 목록에서 빠진다. 디스크 대기 원인을 찾으려면 남겨야 한다.
	blocked := 0
	for _, process := range processes {
		if process.State == topProcessStateBlocked && !seen[process.PID] && blocked < topProcessLimit {
			candidates = append(candidates, process)
			seen[process.PID] = true
			blocked++
		}
	}
	return sortTopProcessesByCPU(candidates)
}

// topProcessCandidatesByIO는 기본 후보에 I/O rate 상위 process를 더한다. 값을 읽은 process만 고른다.
func topProcessCandidatesByIO(processes []topProcess) []topProcess {
	candidates := topProcessCandidates(processes)
	seen := make(map[int]bool, len(candidates))
	for _, process := range candidates {
		seen[process.PID] = true
	}
	byIO := append([]topProcess(nil), processes...)
	sort.SliceStable(byIO, func(i, j int) bool { return topProcessIOTotal(byIO[i]) > topProcessIOTotal(byIO[j]) })
	added := 0
	for _, process := range byIO {
		if added == topProcessLimit || topProcessIOTotal(process) <= 0 {
			break
		}
		if !seen[process.PID] {
			candidates = append(candidates, process)
			seen[process.PID] = true
			added++
		}
	}
	return sortTopProcessesByCPU(candidates)
}

// topProcessStateBlocked는 I/O를 기다리며 멈춘 process의 state 문자다.
const topProcessStateBlocked = "D"

// linuxProcessStat은 /proc/<pid>/stat 한 줄에서 CPU tick과 RSS page 수만 뽑은 값이다.
type linuxProcessStat struct {
	PID        int
	Command    string
	Ticks      uint64
	RSSPages   uint64
	Threads    int
	StartTicks uint64
	State      string
}

// parseLinuxProcessStat은 /proc/<pid>/stat을 읽는다. comm에는 공백과 괄호가 들어갈 수 있어 마지막 ')' 뒤를 나눈다.
// ')' 뒤 첫 필드가 3번 state이므로 utime(14)·stime(15)·num_threads(20)·starttime(22)·rss(24)는 11·12·17·19·21번째다.
func parseLinuxProcessStat(pid int, data string) (linuxProcessStat, bool) {
	const utimeField, stimeField, threadsField, startField, rssField = 11, 12, 17, 19, 21
	open, end := strings.IndexByte(data, '('), strings.LastIndexByte(data, ')')
	if open < 0 || end < open {
		return linuxProcessStat{}, false
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) <= rssField {
		return linuxProcessStat{}, false
	}
	utime, e1 := strconv.ParseUint(fields[utimeField], 10, 64)
	stime, e2 := strconv.ParseUint(fields[stimeField], 10, 64)
	rss, e3 := strconv.ParseUint(fields[rssField], 10, 64)
	threads, e4 := strconv.Atoi(fields[threadsField])
	start, e5 := strconv.ParseUint(fields[startField], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
		return linuxProcessStat{}, false
	}
	return linuxProcessStat{PID: pid, Command: data[open+1 : end], Ticks: utime + stime, RSSPages: rss, Threads: threads, StartTicks: start, State: fields[0]}, true
}

// linuxProcessIO는 /proc/<pid>/io에서 storage에 닿은 누적 byte다. rchar와 wchar는 page cache를 거친 byte까지 세므로 쓰지 않는다.
type linuxProcessIO struct{ read, write uint64 }

func parseLinuxProcessIO(data string) (linuxProcessIO, bool) {
	var io linuxProcessIO
	found := 0
	for _, line := range strings.Split(data, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		number, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "read_bytes":
			io.read, found = number, found+1
		case "write_bytes":
			io.write, found = number, found+1
		}
	}
	return io, found == 2
}

// linuxProcessIOSample은 직전 읽기다. 두 읽기의 차이를 시간으로 나눠 rate를 구한다.
type linuxProcessIOSample struct {
	io linuxProcessIO
	at time.Time
}

// topProcessTracker는 두 번 읽은 CPU tick 차이로 최근 CPU%를 구한다. Linux ps의 pcpu는
// process 수명 전체 평균이라 방금 바빠진 오래된 process를 놓친다.
type topProcessTracker struct {
	// boot는 부팅 시각이다. starttime은 부팅 뒤 tick 수라서 이 값을 더해야 시각이 된다. 모르면 zero다.
	boot       time.Time
	clockTicks float64
	pageSize   uint64
	previousAt time.Time
	previous   map[int]uint64
}

func (tracker *topProcessTracker) update(at time.Time, stats []linuxProcessStat) ([]topProcess, bool) {
	current := make(map[int]uint64, len(stats))
	processes := []topProcess{}
	seconds := at.Sub(tracker.previousAt).Seconds()
	for _, stat := range stats {
		current[stat.PID] = stat.Ticks
		before, seen := tracker.previous[stat.PID]
		if !seen || seconds <= 0 || stat.Ticks < before {
			// 새 process나 pid 재사용은 비교할 기준이 없다.
			continue
		}
		cpu := float64(stat.Ticks-before) / tracker.clockTicks / seconds * 100
		process := topProcess{PID: stat.PID, CPU: cpu, RSS: stat.RSSPages * tracker.pageSize, Command: stat.Command, Threads: stat.Threads, State: stat.State}
		if !tracker.boot.IsZero() {
			process.Started = tracker.boot.Add(time.Duration(float64(stat.StartTicks) / tracker.clockTicks * float64(time.Second)))
		}
		processes = append(processes, process)
	}
	hadBaseline := tracker.previous != nil
	tracker.previous, tracker.previousAt = current, at
	if !hadBaseline {
		return nil, false
	}
	return sortTopProcessesByCPU(processes), true
}

// topProcessStartLayout은 LC_ALL=C일 때 ps의 lstart 형식이다. 다섯 필드이고 일이 한 자리면 공백이 하나 더 들어간다.
const topProcessStartLayout = "Mon Jan 2 15:04:05 2006"

// topProcessStartFields는 lstart가 차지하는 필드 수다.
const topProcessStartFields = 5

// parseTopProcesses는 "pid pcpu rss lstart comm" 줄을 읽는다.
func parseTopProcesses(output string) []topProcess {
	processes := []topProcess{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4+topProcessStartFields {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		cpu, e2 := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
		rss, e3 := strconv.ParseUint(fields[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		// lstart를 읽지 못해도 process는 남긴다. 시작 시각만 비운다.
		started, _ := time.ParseInLocation(topProcessStartLayout, strings.Join(fields[3:3+topProcessStartFields], " "), time.Local)
		processes = append(processes, topProcess{PID: pid, CPU: cpu, RSS: rss * 1024, Command: strings.Join(fields[3+topProcessStartFields:], " "), Started: started})
	}
	return sortTopProcessesByCPU(processes)
}

type resourceCPU struct{ Total, Idle uint64 }

type resourceRate struct {
	NetworkHealth                   *networkHealthRate
	NetIn, NetOut                   float64
	PacketsIn, PacketsOut           float64
	NetErrors, NetDrops             float64
	DiskRead, DiskWrite             float64
	DiskIOPS, DiskAwait, DiskBusy   float64
	DiskQueue                       float64
	CPUUser, CPUSystem, CPUIOWait   float64
	CPUSteal, ProcsBlocked          float64
	MemoryPercent, Load1            float64
	SwapOut                         float64
	NetHealthValid, DiskHealthValid bool
	DiskBusyValid                   bool
	CPUStealValid                   bool
	ProcsBlockedValid               bool
	CoreCPU                         []float64
	PSICPU, PSIMemory, PSIIO        float64
	PSIMemoryFull, PSIIOFull        float64
	PSIValid                        bool
}

type hostDetails struct {
	Hostname, System, OS, Version, Release, Machine, Processor, PythonVersion, Model string
	Cores                                                                            int
	MemoryTotal, SwapUsed, SwapTotal                                                 uint64
	Load                                                                             [3]float64
	Uptime                                                                           time.Duration
	RLimitSoft, RLimitHard                                                           uint64
}

type interfaceDetails struct {
	Name, Address, Mask, Gateway string
}

type diskDetails struct {
	Mount, Device string
	Used, Total   uint64
	Percent       float64
}

func calculateRate(previous, current resourceSnapshot) resourceRate {
	seconds := current.TakenAt.Sub(previous.TakenAt).Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	cpuDelta := delta(current.CPUTotal, previous.CPUTotal)
	percent := func(now, before uint64) float64 {
		if cpuDelta == 0 {
			return 0
		}
		return float64(delta(now, before)) / float64(cpuDelta) * 100
	}
	memoryPercent := 0.0
	if current.MemoryTotal > 0 {
		memoryPercent = float64(current.MemoryUsed) / float64(current.MemoryTotal) * 100
	}
	rate := resourceRate{
		NetworkHealth: calculateNetworkHealthRate(previous.NetworkHealth, current.NetworkHealth, current.TakenAt.Sub(previous.TakenAt).Seconds()),
		NetIn:         float64(delta(current.NetInBytes, previous.NetInBytes)) / seconds,
		NetOut:        float64(delta(current.NetOutBytes, previous.NetOutBytes)) / seconds,
		PacketsIn:     float64(delta(current.PacketsIn, previous.PacketsIn)) / seconds,
		PacketsOut:    float64(delta(current.PacketsOut, previous.PacketsOut)) / seconds,
		NetErrors:     float64(delta(current.NetErrors, previous.NetErrors)) / seconds,
		NetDrops:      float64(delta(current.NetDrops, previous.NetDrops)) / seconds,
		DiskRead:      float64(delta(current.DiskRead, previous.DiskRead)) / seconds,
		DiskWrite:     float64(delta(current.DiskWrite, previous.DiskWrite)) / seconds,
		DiskIOPS:      float64(delta(current.DiskOps, previous.DiskOps)) / seconds,
		CPUUser:       percent(current.CPUUser, previous.CPUUser), CPUSystem: percent(current.CPUSystem, previous.CPUSystem), CPUIOWait: percent(current.CPUIOWait, previous.CPUIOWait),
		MemoryPercent: memoryPercent, Load1: current.Load1,
	}
	rate.NetHealthValid = current.NetHealthValid && previous.NetHealthValid
	rate.DiskHealthValid = current.DiskHealthValid && previous.DiskHealthValid
	rate.DiskBusyValid = rate.DiskHealthValid && current.DiskBusyValid && previous.DiskBusyValid
	if operations := delta(current.DiskOps, previous.DiskOps); operations > 0 && rate.DiskHealthValid {
		rate.DiskAwait = float64(delta(current.DiskWaitMS, previous.DiskWaitMS)) / float64(operations)
	}
	if rate.DiskBusyValid {
		rate.DiskBusy = float64(delta(current.DiskBusyMS, previous.DiskBusyMS)) / seconds / 10
		rate.DiskQueue = float64(delta(current.DiskQueueMS, previous.DiskQueueMS)) / seconds / 1000
	}
	rate.CPUStealValid = current.CPUStealValid && previous.CPUStealValid
	if rate.CPUStealValid {
		rate.CPUSteal = percent(current.CPUSteal, previous.CPUSteal)
	}
	rate.ProcsBlocked, rate.ProcsBlockedValid = float64(current.ProcsBlocked), current.ProcsBlockedValid
	// 한쪽 sample이 counter를 읽지 못했으면 이 구간의 rate는 알 수 없다. 0으로 남은 counter와 비교하지 않는다.
	if previous.NetMissing || current.NetMissing {
		rate.NetIn, rate.NetOut, rate.PacketsIn, rate.PacketsOut, rate.NetErrors, rate.NetDrops, rate.NetHealthValid = 0, 0, 0, 0, 0, 0, false
	}
	if previous.DiskMissing || current.DiskMissing {
		rate.DiskRead, rate.DiskWrite, rate.DiskIOPS, rate.DiskAwait, rate.DiskBusy, rate.DiskQueue, rate.DiskHealthValid, rate.DiskBusyValid = 0, 0, 0, 0, 0, 0, false, false
	}
	if !previous.SwapMissing && !current.SwapMissing {
		rate.SwapOut = float64(delta(current.SwapOutBytes, previous.SwapOutBytes)) / seconds
	}
	if len(current.Cores) == len(previous.Cores) {
		rate.CoreCPU = make([]float64, len(current.Cores))
		for index, currentCore := range current.Cores {
			previousCore := previous.Cores[index]
			if total := delta(currentCore.Total, previousCore.Total); total > 0 {
				rate.CoreCPU[index] = float64(delta(currentCore.Total-currentCore.Idle, previousCore.Total-previousCore.Idle)) / float64(total) * 100
			}
		}
	}
	rate.PSICPU, rate.PSIMemory, rate.PSIIO, rate.PSIValid = current.PSICPU, current.PSIMemory, current.PSIIO, current.PSIValid
	rate.PSIMemoryFull, rate.PSIIOFull = current.PSIMemoryFull, current.PSIIOFull
	return rate
}

// parseLinuxSwapOutPages는 /proc/vmstat의 pswpout, 곧 부팅 뒤 swap으로 내보낸 page 수를 읽는다.
func parseLinuxSwapOutPages(input string) (uint64, bool) {
	for _, line := range strings.Split(input, "\n") {
		name, value, found := strings.Cut(line, " ")
		if found && name == "pswpout" {
			pages, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			return pages, err == nil
		}
	}
	return 0, false
}

// parsePressureAvg10은 /proc/pressure/*의 kind 행(some 또는 full)에서 최근 10초 stall 비율을 읽는다.
func parsePressureAvg10(input, kind string) (float64, bool) {
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != kind {
			continue
		}
		for _, field := range fields[1:] {
			key, value, found := strings.Cut(field, "=")
			if key != "avg10" || !found {
				continue
			}
			parsed, err := strconv.ParseFloat(value, 64)
			return parsed, err == nil
		}
	}
	return 0, false
}

func delta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}

// formatRate는 5자를 넘지 않도록 값 크기에 따라 단위와 소수 자릿수를 줄인다.
func formatRate(bytes float64) string {
	value := bytes / (1024 * 1024)
	unit := "M"
	if value >= 1024 {
		value /= 1024
		unit = "G"
	}
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f%s", value, unit)
	case value >= 10:
		return fmt.Sprintf("%.1f%s", value, unit)
	default:
		return fmt.Sprintf("%.2f%s", value, unit)
	}
}
func formatBytes(bytes uint64) string {
	const (
		gib = 1024 * 1024 * 1024
		tib = 1024 * gib
	)
	if bytes >= tib {
		return fmt.Sprintf("%.2f TB", float64(bytes)/tib)
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/gib)
}

func networkInterfaces(defaultInterface, gateway string) ([]interfaceDetails, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []interfaceDetails
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ipnet, ok := address.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			mask := net.IP(ipnet.Mask).String()
			item := interfaceDetails{Name: iface.Name, Address: ipnet.IP.String(), Mask: mask}
			if iface.Name == defaultInterface {
				item.Gateway = gateway
			}
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func runtimeMachine() string { return runtime.GOARCH }

func detectPythonVersion() string {
	for _, name := range []string{"python3", "python"} {
		output, err := exec.Command(name, "--version").CombinedOutput()
		if err == nil {
			return strings.TrimPrefix(strings.TrimSpace(string(output)), "Python ")
		}
	}
	return "not installed"
}

func collectDisksFromDF() ([]diskDetails, error) {
	output, err := exec.Command("df", "-kP").Output()
	if err != nil {
		return nil, err
	}
	return parseDiskUsage(strings.NewReader(string(output)))
}

// parseDiskUsage는 `df -kP`의 POSIX 출력을 읽는다.
// 열은 Filesystem, 1024-blocks, Used, Available, Capacity, Mounted on 순서다.
func parseDiskUsage(reader io.Reader) ([]diskDetails, error) {
	var disks []diskDetails
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 || fields[0] == "Filesystem" {
			continue
		}
		if strings.HasPrefix(fields[0], "devfs") || strings.HasPrefix(fields[0], "map ") {
			continue
		}
		totalKB, err1 := strconv.ParseUint(fields[1], 10, 64)
		availableKB, err2 := strconv.ParseUint(fields[3], 10, 64)
		if err1 != nil || err2 != nil || totalKB == 0 || availableKB > totalKB {
			continue
		}
		// APFS는 container 하나를 여러 volume이 나눠 쓰므로 volume의 Used 열은 그 volume이 쓴 양만 센다.
		// 남은 공간에서 거꾸로 계산해야 disk 전체 사용률이 나온다.
		usedKB := totalKB - availableKB
		disks = append(disks, diskDetails{Mount: strings.Join(fields[5:], " "), Device: fields[0], Total: totalKB * 1024, Used: usedKB * 1024, Percent: float64(usedKB) / float64(totalKB) * 100})
	}
	return disks, scanner.Err()
}
