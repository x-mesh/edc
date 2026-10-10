//go:build darwin

package edc

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// 값은 macOS SDK의 offsetof(struct rusage_info_v4, ...)다. 크기는 TestReadDarwinProcessDetails가 본다.
func TestDarwinRusageMatchesRusageInfoV4(t *testing.T) {
	var info darwinProcessRusage
	if offset := unsafe.Offsetof(info.RunnableTime); offset != 288 {
		t.Fatalf("offsetof RunnableTime = %d, want 288", offset)
	}
	if offset := unsafe.Offsetof(info.DiskWrite); offset != 152 {
		t.Fatalf("offsetof DiskWrite = %d, want 152", offset)
	}
}

func TestTopDarwinRunqDeltaSubtractsCPUFromRunnable(t *testing.T) {
	timebase := darwinTimebase{Numer: 125, Denom: 3}
	before := topDarwinRunqCounters{started: 7, runnable: 3_000, cpu: 1_200, switches: ^uint32(0) - 1}
	after := topDarwinRunqCounters{started: 7, runnable: 3_000 + 30_000, cpu: 1_200 + 6_000, switches: 2}
	stats, ok := topDarwinRunqDelta(before, after, timebase)
	if !ok {
		t.Fatal("same process must give a delta")
	}
	// (30000 - 6000) tick * 125/3 = 1,000,000ns다. switch counter는 32비트에서 넘어가도 4번으로 센다.
	if stats.RunqSumNS != 1_000_000 || stats.RunqCount != 4 || stats.Source != topDarwinSource || !stats.IOUnsupported || !stats.RunqHistUnsupported {
		t.Fatalf("stats = %+v", stats)
	}
	if _, ok := topDarwinRunqDelta(before, topDarwinRunqCounters{started: 8, runnable: 9_000, cpu: 2_000}, timebase); ok {
		t.Fatal("a reused PID must not give a delta")
	}
	for name, after := range map[string]topDarwinRunqCounters{
		"runnable went back": {started: 7, runnable: before.runnable - 1, cpu: before.cpu},
		"cpu went back":      {started: 7, runnable: before.runnable, cpu: before.cpu - 1},
	} {
		if _, ok := topDarwinRunqDelta(before, after, timebase); ok {
			t.Errorf("%s must not give a delta", name)
		}
	}
	idle := before
	idle.runnable, idle.cpu = before.runnable+100, before.cpu+120
	if stats, ok := topDarwinRunqDelta(before, idle, timebase); !ok || stats.RunqSumNS != 0 {
		t.Fatalf("CPU above runnable must clamp to zero wait, got %+v, %v", stats, ok)
	}
}

// 이 process에 core보다 많은 바쁜 goroutine을 돌려 CPU를 기다리게 한다. 첫 관측은 기준만 잡는다.
func TestTopDarwinObserverSeesCPUWaitOfThisProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("spins every core")
	}
	if darwinTranslated() {
		t.Skip("-d refuses to run under Rosetta")
	}
	observe, stop, err := startTopProcessBPF()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	pid := os.Getpid()
	if got := observe([]int{pid}); len(got) != 0 {
		t.Fatalf("the first observation has no baseline, got %+v", got)
	}
	previous := runtime.GOMAXPROCS(4 * runtime.NumCPU())
	defer runtime.GOMAXPROCS(previous)
	var done atomic.Bool
	var group sync.WaitGroup
	for range 4 * runtime.NumCPU() {
		group.Add(1)
		go func() {
			defer group.Done()
			for !done.Load() {
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	done.Store(true)
	group.Wait()
	stats, ok := observe([]int{pid})[pid]
	if !ok {
		t.Fatal("the second observation must report this process")
	}
	if stats.Window < 500*time.Millisecond || stats.RunqCount == 0 || stats.RunqSumNS < uint64(100*time.Millisecond) {
		t.Fatalf("stats = %+v", stats)
	}
}

// 읽지 못한 pid는 빠지고, 목록에서 빠졌다 돌아온 pid는 기준을 다시 잡는다. 오래된 기준과 비교하면 빠진 동안의 대기가 한 window에 몰린다.
func TestTopDarwinObserverSkipsUnreadablePIDsAndRebaselines(t *testing.T) {
	observe, stop, err := startTopProcessBPF()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	self := os.Getpid()
	// macOS의 PID는 99999를 넘지 않으므로 이 PID의 process는 없다.
	const unreadable = 999_999
	observe([]int{self, unreadable})
	if got := observe([]int{self, unreadable}); len(got) != 1 || got[self].Source != topDarwinSource {
		t.Fatalf("observed = %+v", got)
	}
	observe(nil)
	if got := observe([]int{self}); len(got) != 0 {
		t.Fatalf("a PID that came back must wait for a new baseline, got %+v", got)
	}
	if got := observe([]int{self}); len(got) != 1 {
		t.Fatalf("the next observation must report it, got %+v", got)
	}
}

// offset 검사는 배치만 본다. flavor를 잘못 넘기면 kernel이 v4 뒤쪽을 채우지 않으므로 실제 값으로 확인한다.
func TestDarwinRusageV4FillsRunnableTime(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	info, err := readDarwinProcessRusage(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if cpu := info.UserTime + info.SystemTime; cpu == 0 || info.RunnableTime < cpu {
		t.Fatalf("runnable %d must include cpu %d", info.RunnableTime, cpu)
	}
}
