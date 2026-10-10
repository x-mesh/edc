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
