package edc

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type resourceSnapshot struct {
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
	PSIValid        bool
	Processes       []topProcess
	ProcessesValid  bool
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
}

type topProcess struct {
	PID     int
	CPU     float64
	RSS     uint64
	Command string
}

const (
	// topProcessRefresh는 process 목록을 다시 읽는 최소 간격이다. 관측 주기가 짧아도 host 부담을 묶어 둔다.
	topProcessRefresh = time.Second
	// topProcessLimit은 CPU 순으로 남기는 process 수다.
	topProcessLimit = 5
)

// topProcessSampler는 process 목록을 배경에서 갱신한다. 대시보드 tick은 마지막 결과만 받아 가므로
// process 수집 시간이 관측 주기에 붙지 않는다.
type topProcessSampler struct {
	mutex          sync.Mutex
	processes      []topProcess
	valid, running bool
	updated        time.Time
	read           func() ([]topProcess, bool)
}

var processSampler = &topProcessSampler{read: newTopProcessReader()}

func (sampler *topProcessSampler) latest() ([]topProcess, bool) {
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	if !sampler.running && time.Since(sampler.updated) >= topProcessRefresh {
		sampler.running = true
		go sampler.refresh()
	}
	return append([]topProcess(nil), sampler.processes...), sampler.valid
}

func (sampler *topProcessSampler) refresh() {
	processes, valid := sampler.read()
	sampler.mutex.Lock()
	defer sampler.mutex.Unlock()
	// 실패해도 시각과 결과를 남긴다. 수집기가 없는 host에서 매 tick 다시 돌지 않고, 낡은 목록이 유효하게 남지 않는다.
	sampler.processes, sampler.valid, sampler.updated, sampler.running = processes, valid, time.Now(), false
}

func topProcessesByCPU(processes []topProcess) []topProcess {
	sort.Slice(processes, func(i, j int) bool { return processes[i].CPU > processes[j].CPU })
	if len(processes) > topProcessLimit {
		processes = processes[:topProcessLimit]
	}
	return processes
}

// linuxProcessStat은 /proc/<pid>/stat 한 줄에서 CPU tick과 RSS page 수만 뽑은 값이다.
type linuxProcessStat struct {
	PID      int
	Command  string
	Ticks    uint64
	RSSPages uint64
}

// parseLinuxProcessStat은 /proc/<pid>/stat을 읽는다. comm에는 공백과 괄호가 들어갈 수 있어 마지막 ')' 뒤를 나눈다.
// ')' 뒤 첫 필드가 3번 state이므로 utime(14)·stime(15)·rss(24)는 11·12·21번째다.
func parseLinuxProcessStat(pid int, data string) (linuxProcessStat, bool) {
	const utimeField, stimeField, rssField = 11, 12, 21
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
	if e1 != nil || e2 != nil || e3 != nil {
		return linuxProcessStat{}, false
	}
	return linuxProcessStat{PID: pid, Command: data[open+1 : end], Ticks: utime + stime, RSSPages: rss}, true
}

// topProcessTracker는 두 번 읽은 CPU tick 차이로 최근 CPU%를 구한다. Linux ps의 pcpu는
// process 수명 전체 평균이라 방금 바빠진 오래된 process를 놓친다.
type topProcessTracker struct {
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
		processes = append(processes, topProcess{PID: stat.PID, CPU: cpu, RSS: stat.RSSPages * tracker.pageSize, Command: stat.Command})
	}
	hadBaseline := tracker.previous != nil
	tracker.previous, tracker.previousAt = current, at
	if !hadBaseline {
		return nil, false
	}
	return topProcessesByCPU(processes), true
}

func parseTopProcesses(output string) []topProcess {
	processes := []topProcess{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		cpu, e2 := strconv.ParseFloat(strings.ReplaceAll(fields[1], ",", "."), 64)
		rss, e3 := strconv.ParseUint(fields[2], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		processes = append(processes, topProcess{pid, cpu, rss * 1024, strings.Join(fields[3:], " ")})
	}
	return topProcessesByCPU(processes)
}

type resourceCPU struct{ Total, Idle uint64 }

type resourceRate struct {
	NetIn, NetOut                   float64
	PacketsIn, PacketsOut           float64
	NetErrors, NetDrops             float64
	DiskRead, DiskWrite             float64
	DiskIOPS, DiskAwait, DiskBusy   float64
	CPUUser, CPUSystem, CPUIOWait   float64
	MemoryPercent, Load1            float64
	SwapOut                         float64
	NetHealthValid, DiskHealthValid bool
	DiskBusyValid                   bool
	CoreCPU                         []float64
	PSICPU, PSIMemory, PSIIO        float64
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
		NetIn:      float64(delta(current.NetInBytes, previous.NetInBytes)) / seconds,
		NetOut:     float64(delta(current.NetOutBytes, previous.NetOutBytes)) / seconds,
		PacketsIn:  float64(delta(current.PacketsIn, previous.PacketsIn)) / seconds,
		PacketsOut: float64(delta(current.PacketsOut, previous.PacketsOut)) / seconds,
		NetErrors:  float64(delta(current.NetErrors, previous.NetErrors)) / seconds,
		NetDrops:   float64(delta(current.NetDrops, previous.NetDrops)) / seconds,
		DiskRead:   float64(delta(current.DiskRead, previous.DiskRead)) / seconds,
		DiskWrite:  float64(delta(current.DiskWrite, previous.DiskWrite)) / seconds,
		DiskIOPS:   float64(delta(current.DiskOps, previous.DiskOps)) / seconds,
		CPUUser:    percent(current.CPUUser, previous.CPUUser), CPUSystem: percent(current.CPUSystem, previous.CPUSystem), CPUIOWait: percent(current.CPUIOWait, previous.CPUIOWait),
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
	}
	// 한쪽 sample이 counter를 읽지 못했으면 이 구간의 rate는 알 수 없다. 0으로 남은 counter와 비교하지 않는다.
	if previous.NetMissing || current.NetMissing {
		rate.NetIn, rate.NetOut, rate.PacketsIn, rate.PacketsOut, rate.NetErrors, rate.NetDrops, rate.NetHealthValid = 0, 0, 0, 0, 0, 0, false
	}
	if previous.DiskMissing || current.DiskMissing {
		rate.DiskRead, rate.DiskWrite, rate.DiskIOPS, rate.DiskAwait, rate.DiskBusy, rate.DiskHealthValid, rate.DiskBusyValid = 0, 0, 0, 0, 0, false, false
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

// parsePressureAvg10은 /proc/pressure/*의 some 행에서 최근 10초 stall 비율을 읽는다.
func parsePressureAvg10(input string) (float64, bool) {
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
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
