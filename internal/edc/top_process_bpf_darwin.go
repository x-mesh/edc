//go:build darwin

package edc

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// macOS에는 scheduler와 block I/O event를 주는 공개 hook이 없다. 그래서 rusage_info_v4의 누적 counter 차이로
// window 동안의 CPU 대기 합을 구한다. 대기 하나하나의 값이 없어 분포와 p95는 없고, process별 block I/O 지연은 셀 수 없다.
const topDarwinWatchedMax = 4096

// topDarwinRunqCounters는 한 process의 누적값이다. 시간은 Mach absolute time 단위다.
type topDarwinRunqCounters struct {
	started  uint64
	runnable uint64
	cpu      uint64
	// switches는 proc_taskinfo의 32비트 counter라 넘어갈 수 있다. 차이는 32비트로 구한다.
	switches uint32
}

type topDarwinObserver struct {
	timebase darwinTimebase
	previous map[int]topDarwinRunqCounters
	at       time.Time
}

func startTopProcessBPF() (topBPFObserver, func(), error) {
	// Rosetta는 mach_timebase_info를 번역하지만 proc_pid_rusage는 kernel의 tick을 그대로 준다. 단위가 어긋나 대기를
	// 잘못 셀 수 있으므로 번역된 amd64 binary에서는 세지 않는다.
	if darwinTranslated() {
		return nil, nil, fmt.Errorf("top --detail: this amd64 edc runs under Rosetta; use the arm64 build")
	}
	timebase, err := darwinMachTimebase()
	if err != nil {
		return nil, nil, fmt.Errorf("top --detail: %w", err)
	}
	observer := &topDarwinObserver{timebase: timebase, previous: map[int]topDarwinRunqCounters{}}
	return observer.observe, func() {}, nil
}

// observe는 pid마다 직전 관측 이후의 값을 돌려준다. 처음 본 process와 이미 끝난 process는 빠진다.
// 다른 사용자의 process는 root가 아니면 libproc이 EPERM으로 거부한다. 그 process는 빠뜨리지 않고 Unreadable로 남긴다.
// 빠뜨리면 감시 중인데 event가 없던 것과 구분되지 않는다.
func (observer *topDarwinObserver) observe(pids []int) map[int]topBPFStats {
	now := time.Now()
	window := now.Sub(observer.at)
	current := make(map[int]topDarwinRunqCounters, min(len(pids), topDarwinWatchedMax))
	observed := make(map[int]topBPFStats, len(observer.previous))
	for _, pid := range pids[:min(len(pids), topDarwinWatchedMax)] {
		counters, err := readTopDarwinRunqCounters(pid)
		if errors.Is(err, syscall.EPERM) {
			// 첫 관측에는 window가 없다. 다른 process처럼 다음 관측부터 보고한다.
			if !observer.at.IsZero() {
				observed[pid] = topBPFStats{Window: window, Source: topBPFSourceLibproc, Unreadable: 1}
			}
			continue
		}
		if err != nil {
			continue
		}
		current[pid] = counters
		if before, ok := observer.previous[pid]; ok {
			if stats, ok := topDarwinRunqDelta(before, counters, observer.timebase); ok {
				stats.Window = window
				observed[pid] = stats
			}
		}
	}
	observer.previous, observer.at = current, now
	return observed
}

func readTopDarwinRunqCounters(pid int) (topDarwinRunqCounters, error) {
	usage, err := readDarwinProcessRusage(pid)
	if err != nil {
		return topDarwinRunqCounters{}, err
	}
	task, err := readDarwinProcessTaskInfo(pid)
	if err != nil {
		return topDarwinRunqCounters{}, err
	}
	return topDarwinRunqCounters{started: usage.Started, runnable: usage.RunnableTime, cpu: usage.UserTime + usage.SystemTime, switches: uint32(task.ContextSwitches)}, nil
}

// topDarwinRunqDelta는 runnable 시간에서 CPU 시간을 빼 대기 시간을 구한다. runnable은 실행 시간을 포함한다.
// 개수는 context switch 수다. eBPF처럼 깨어나 CPU를 받은 횟수가 아니므로 평균은 switch 한 번당 대기다.
// PID가 재사용됐거나 counter가 줄었으면 기준이 없는 것으로 본다.
func topDarwinRunqDelta(before, after topDarwinRunqCounters, timebase darwinTimebase) (topBPFStats, bool) {
	if after.started != before.started || after.runnable < before.runnable || after.cpu < before.cpu {
		return topBPFStats{}, false
	}
	runnable := timebase.nanoseconds(after.runnable - before.runnable)
	cpu := timebase.nanoseconds(after.cpu - before.cpu)
	stats := topBPFStats{Source: topBPFSourceLibproc, Measured: 1, RunqCount: uint64(after.switches - before.switches)}
	if runnable > cpu {
		stats.RunqSumNS = runnable - cpu
	}
	return stats, true
}

// darwinTranslated는 이 process가 Rosetta로 번역돼 도는지다. 이 sysctl이 없는 Intel Mac은 번역이 아니다.
func darwinTranslated() bool {
	var translated int32
	return darwinSysctl("sysctl.proc_translated", unsafe.Pointer(&translated), unsafe.Sizeof(translated)) == nil && translated == 1
}
