//go:build darwin

package edc

import (
	"encoding/binary"
	"math"
	"net"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestReadDarwinProcessDetails(t *testing.T) {
	if size := unsafe.Sizeof(darwinProcessTaskInfo{}); size != 96 {
		t.Fatalf("proc_taskinfo size = %d, want 96", size)
	}
	if size := unsafe.Sizeof(darwinProcessRusage{}); size != 296 {
		t.Fatalf("rusage_info_v4 size = %d, want 296", size)
	}
	threads, err := readDarwinProcessThreads(os.Getpid())
	if err != nil || threads <= 0 {
		t.Fatalf("own threads = %d, %v", threads, err)
	}
	stats, err := readDarwinProcessRusage(os.Getpid())
	if err != nil || stats.Started == 0 || stats.ResidentSize == 0 {
		t.Fatalf("own rusage = %+v, %v", stats, err)
	}
	if _, err := readDarwinProcessRusage(-1); err == nil {
		t.Fatal("invalid PID must return an error")
	}
}

func TestDarwinProcessEnricherNeedsTwoSamplesAndReportsErrors(t *testing.T) {
	enrich := newTopProcessEnricher()
	processes := []topProcess{{PID: os.Getpid()}}
	enrich(processes)
	if processes[0].DiskValid || processes[0].DiskStatus != "waiting for I/O baseline" {
		t.Fatalf("first I/O sample = %+v", processes[0])
	}
	enrich(processes)
	if !processes[0].DiskValid || processes[0].DiskRead < 0 || processes[0].DiskWrite < 0 || processes[0].DiskStatus != "" {
		t.Fatalf("second I/O sample = %+v", processes[0])
	}
	processes[0].PID = -1
	enrich(processes)
	if processes[0].DiskValid || !strings.Contains(processes[0].DiskStatus, "proc_pid_rusage") {
		t.Fatalf("failed read must not retain rates: %+v", processes[0])
	}
}

func TestDarwinRusageReadCountReusesReaderSample(t *testing.T) {
	if got := darwinRusageReads(true); got != 0 {
		t.Fatalf("enricher reads after reader = %d, want 0", got)
	}
	if got := darwinRusageReads(false); got != 1 {
		t.Fatalf("enricher reads without reader sample = %d, want 1", got)
	}
}

func TestDarwinReaderAndEnricherReuseRusageSample(t *testing.T) {
	previousReader, previousNow := darwinProcessRusageReader, darwinProcessNow
	t.Cleanup(func() { darwinProcessRusageReader, darwinProcessNow = previousReader, previousNow })
	var calls int
	readAt := time.Unix(100, 0)
	darwinProcessRusageReader = func(int) (darwinProcessRusage, error) {
		calls++
		return darwinProcessRusage{Started: 7, DiskRead: uint64(calls) * 100, DiskWrite: uint64(calls) * 50, Footprint: 1 << 20}, nil
	}
	darwinProcessNow = func() time.Time { return readAt }
	processes, valid := newTopProcessReaderWithRusage(true)()
	if !valid || len(processes) == 0 {
		t.Fatal("reader produced no processes")
	}
	readerCalls := calls
	enrich := newTopProcessEnricher()
	enrich(processes)
	if calls != readerCalls {
		t.Fatalf("rusage calls after first enrich = %d, want %d", calls, readerCalls)
	}
	readAt = readAt.Add(time.Second)
	for index := range processes {
		processes[index].diskReadBytes += 100
		processes[index].diskWriteBytes += 50
		processes[index].diskSampledAt = readAt
	}
	enrich(processes)
	if calls != readerCalls || !processes[0].DiskValid || processes[0].DiskRead != 100 || processes[0].DiskWrite != 50 {
		t.Fatalf("calls %d, process %+v", calls, processes[0])
	}
}

func TestDarwinReaderAddsKernelTaskWithCPUFromRusageDelta(t *testing.T) {
	timebase, err := darwinMachTimebase()
	if err != nil || darwinTranslated() {
		t.Skip("kernel_task CPU needs the native Mach timebase")
	}
	previousReader, previousTaskInfo, previousNow := darwinProcessRusageReader, darwinProcessTaskInfoReader, darwinProcessNow
	t.Cleanup(func() {
		darwinProcessRusageReader, darwinProcessTaskInfoReader, darwinProcessNow = previousReader, previousTaskInfo, previousNow
	})
	var ticks darwinMachTicks
	readAt := time.Unix(100, 0)
	darwinProcessRusageReader = func(pid int) (darwinProcessRusage, error) {
		if pid != 0 {
			return darwinProcessRusage{}, syscall.EPERM
		}
		return darwinProcessRusage{UserTime: ticks, SystemTime: ticks, ResidentSize: 4 << 20, Footprint: 42 << 20}, nil
	}
	darwinProcessTaskInfoReader = func(pid int) (darwinProcessTaskInfo, error) {
		if pid != 0 {
			return darwinProcessTaskInfo{}, syscall.EPERM
		}
		return darwinProcessTaskInfo{Threads: 968}, nil
	}
	darwinProcessNow = func() time.Time { return readAt }
	read := newTopProcessReaderWithRusage(true)
	first := darwinKernelTaskRow(t, read)
	if first.Command != "kernel_task" || first.CPU != 0 || !first.MemoryValid || first.Memory != 42<<20 || !first.RSSMissing || first.Threads != 968 {
		t.Fatalf("first kernel_task row = %+v", first)
	}
	// 1초 동안 user와 system에 0.25초씩 쓰면 core 하나의 50%다.
	ticks = darwinMachTicks(uint64(250*time.Millisecond) * uint64(timebase.Denom) / uint64(timebase.Numer))
	readAt = readAt.Add(time.Second)
	if got := darwinKernelTaskRow(t, read).CPU; math.Abs(got-50) > 0.01 {
		t.Fatalf("kernel_task CPU = %v, want 50", got)
	}
}

func TestDarwinReaderOmitsKernelTaskWhenRusageIsDenied(t *testing.T) {
	previousReader := darwinProcessRusageReader
	t.Cleanup(func() { darwinProcessRusageReader = previousReader })
	darwinProcessRusageReader = func(int) (darwinProcessRusage, error) { return darwinProcessRusage{}, syscall.EPERM }
	processes, valid := newTopProcessReaderWithRusage(true)()
	if !valid || slices.ContainsFunc(processes, func(process topProcess) bool { return process.PID == 0 }) {
		t.Fatalf("valid %v, processes include PID 0 without rusage", valid)
	}
}

func darwinKernelTaskRow(t *testing.T, read func() ([]topProcess, bool)) topProcess {
	t.Helper()
	processes, _ := read()
	index := slices.IndexFunc(processes, func(process topProcess) bool { return process.PID == 0 })
	if index < 0 {
		t.Fatal("no kernel_task row")
	}
	return processes[index]
}

func TestDarwinProcessSampleTimeUsesRusageReadTime(t *testing.T) {
	readAt, enrichedAt := time.Unix(100, 0), time.Unix(101, 0)
	process := topProcess{diskSampled: true, diskSampledAt: readAt}
	if got := darwinProcessSampleTime(process, enrichedAt); !got.Equal(readAt) {
		t.Fatalf("sample time = %v, want %v", got, readAt)
	}
}

func TestDarwinMemoryFromStatistics(t *testing.T) {
	// FreeCount 100에는 speculative 50이 들어 있다. 회수 가능한 page는 free 100과 inactive 250이다.
	stats := darwinVMStatistics64{FreeCount: 100, SpeculativeCount: 50, InactiveCount: 250, ActiveCount: 1000, Swapouts: 7}
	memory := darwinMemoryFromStatistics(stats, 16384, 2000*16384)
	if memory.total != 2000*16384 || memory.used != (2000-350)*16384 {
		t.Fatalf("memory = %#v", memory)
	}
	if !memory.swapOK || memory.swapOut != 7*16384 {
		t.Fatalf("swap = %#v", memory)
	}
	if got := darwinMemoryFromStatistics(stats, 16384, 0); got.total != 0 || got.used != 0 {
		t.Fatalf("unknown total must leave usage empty: %#v", got)
	}
	stats.CompressorPageCount = 3
	if got := darwinMemoryFromStatistics(stats, 16384, 2000*16384); !got.compressorOK || got.compressor != 3*16384 {
		t.Fatalf("compressor = %#v", got)
	}
}

// TestReadDarwinSwapUsage는 xsw_usage의 offset을 잘못 잡으면 used가 total을 넘거나 page 크기가 0이 되는 점으로 확인한다.
func TestReadDarwinSwapUsage(t *testing.T) {
	usage, err := readDarwinSwapUsage()
	if err != nil {
		t.Fatal(err)
	}
	if usage.PageSize == 0 || usage.Used > usage.Total || usage.Available > usage.Total || usage.Used+usage.Available != usage.Total {
		t.Fatalf("swap usage = %+v", usage)
	}
}

func TestInfoMemoryFromDarwinSeparatesPhysicalAndOriginalCompression(t *testing.T) {
	stats := darwinVMStatistics64{FreeCount: 100, InactiveCount: 200, ExternalPageCount: 250, CompressorPageCount: 20, TotalUncompressedPagesInCompressor: 80, WireCount: 50}
	memory, err := infoMemoryFromDarwin(stats, 16384, 1000*16384)
	if err != nil || memory.Available != 300*16384 || memory.Details[0].Bytes != 250*16384 || memory.Details[1].Bytes != 50*16384 || memory.Details[2].Bytes != 20*16384 || memory.Details[3].Bytes != 80*16384 {
		t.Fatalf("memory = %+v, %v", memory, err)
	}
	if _, err := infoMemoryFromDarwin(stats, 16384, 100*16384); err == nil {
		t.Fatal("available memory above total must fail")
	}
	if _, err := infoMemoryFromDarwin(stats, 0, 1000); err == nil {
		t.Fatal("unknown page size must fail")
	}
}

// TestReadDarwinKernelStatistics는 구조체 offset이나 page 크기를 잘못 잡으면 page 합이 물리 memory와 크게 어긋나는 점으로 확인한다.
// Rosetta에서 hw.pagesize를 쓰면 합이 1/4로 줄어든다.
func TestReadDarwinKernelStatistics(t *testing.T) {
	stats, err := readDarwinVMStatistics()
	if err != nil {
		t.Fatal(err)
	}
	pageSize, err := darwinPageSize()
	if err != nil {
		t.Fatal(err)
	}
	total, err := readDarwinMemorySize()
	if err != nil {
		t.Fatal(err)
	}
	pages := uint64(stats.FreeCount) + uint64(stats.ActiveCount) + uint64(stats.InactiveCount) + uint64(stats.WireCount) + uint64(stats.ThrottledCount) + uint64(stats.CompressorPageCount)
	if ratio := float64(pages*pageSize) / float64(total); ratio < 0.5 || ratio > 1.5 {
		t.Fatalf("pages × %d = %.3f of hw.memsize %d: %#v", pageSize, ratio, total, stats)
	}
	if load, err := readDarwinLoad1(); err != nil || load < 0 {
		t.Fatalf("load = %v, %v", load, err)
	}
}

func TestParseDarwinInterfaceData(t *testing.T) {
	data := make([]byte, darwinIfmibDataSize)
	copy(data, "en0")
	binary.LittleEndian.PutUint32(data[32:], 25) // ifmd_snd_drops
	put := func(offset int, value uint64) { binary.LittleEndian.PutUint64(data[darwinIfDataOffset+offset:], value) }
	put(24, 93428974)     // ifi_ipackets
	put(32, 2)            // ifi_ierrors
	put(40, 50000000)     // ifi_opackets
	put(48, 1)            // ifi_oerrors
	put(64, 58454130307)  // ifi_ibytes, 32비트를 넘는 값
	put(72, 157747341361) // ifi_obytes
	want := darwinNetwork{packetsIn: 93428974, packetsOut: 50000000, bytesIn: 58454130307, bytesOut: 157747341361, errors: 3, drops: 25}
	if got := parseDarwinInterfaceData(data); got != want {
		t.Fatalf("network = %#v, want %#v", got, want)
	}
}

// TestReadDarwinInterfaceData는 kernel이 준 ifmibdata의 이름과 MTU로 구조 offset을 확인한다.
func TestReadDarwinInterfaceData(t *testing.T) {
	loopback, err := net.InterfaceByName("lo0")
	if err != nil {
		t.Fatal(err)
	}
	data, err := readDarwinInterfaceData(loopback.Index)
	if err != nil {
		t.Fatal(err)
	}
	if name := strings.TrimRight(string(data[:16]), "\x00"); name != "lo0" {
		t.Fatalf("ifmibdata name = %q", name)
	}
	if mtu := binary.LittleEndian.Uint32(data[darwinIfDataOffset+8:]); int(mtu) != loopback.MTU {
		t.Fatalf("ifmibdata MTU = %d, want %d", mtu, loopback.MTU)
	}
	if _, err := readDarwinNetworkCounters(); err != nil {
		t.Fatal(err)
	}
}

func TestParseDarwinDiskSumsPhysicalDevices(t *testing.T) {
	output := `+-o AppleSDXCBlockStorageDevice  <class AppleSDXCBlockStorageDevice, registered, matched, active>
  | {
  |   "Protocol Characteristics" = {"Physical Interconnect"="Secure Digital","Physical Interconnect Location"="Internal"}
  | }
  |
  +-o IOBlockStorageDriver  <class IOBlockStorageDriver, registered, matched, active>
      {
        "Statistics" = {"Operations (Write)"=0,"Bytes (Read)"=0,"Bytes (Write)"=0,"Operations (Read)"=0}
      }
+-o NS_01@1  <class IOEmbeddedNVMeBlockDevice, registered, matched, active>
  | {
  |   "Protocol Characteristics" = {"Physical Interconnect"="Apple Fabric","Physical Interconnect Location"="Internal"}
  | }
  |
  +-o IOBlockStorageDriver  <class IOBlockStorageDriver, registered, matched, active>
      {
        "Statistics" = {"Operations (Write)"=40,"Total Time (Read)"=6000000,"Bytes (Write)"=300,"Operations (Read)"=60,"Total Time (Write)"=4000000,"Bytes (Read)"=1000}
      }
+-o IOBlockStorageServices  <class IOBlockStorageServices, registered, matched, active>
  | {
  |   "Protocol Characteristics" = {"Physical Interconnect"="USB","Physical Interconnect Location"="External"}
  | }
  |
  +-o IOBlockStorageDriver  <class IOBlockStorageDriver, registered, matched, active>
      {
        "Statistics" = {"Bytes (Read)"=20,"Operations (Read)"=2,"Total Time (Read)"=3000000,"Bytes (Write)"=10,"Operations (Write)"=3,"Total Time (Write)"=2000000}
      }
+-o AppleDiskImageDevice@0  <class AppleDiskImageDevice, registered, matched, active>
  | {
  |   "Protocol Characteristics" = {"Physical Interconnect"="Virtual Interface","Physical Interconnect Location"="File"}
  | }
  |
  +-o IOBlockStorageDriver  <class IOBlockStorageDriver, registered, matched, active>
      {
        "Statistics" = {"Bytes (Read)"=7000,"Bytes (Write)"=7000,"Operations (Read)"=70,"Total Time (Read)"=9000000}
      }
`
	want := darwinDisk{read: 1020, write: 310, operations: 105, waitNS: 15000000}
	if got, ok := parseDarwinDisk(output); !ok || got != want {
		t.Fatalf("disk = %#v, %v", got, ok)
	}
	if _, ok := parseDarwinDisk("unexpected output"); ok {
		t.Fatal("output without device statistics must report a missing read")
	}
}

func TestAddDarwinCoreTicks(t *testing.T) {
	snapshot := resourceSnapshot{}
	addDarwinCoreTicks(&snapshot, []darwinCoreTicks{
		{User: 100, System: 50, Idle: 800, Nice: 10},
		{User: 300, System: 100, Idle: 600},
	})
	if snapshot.CPUUser != 410 || snapshot.CPUSystem != 150 || snapshot.CPUIdle != 1400 || snapshot.CPUTotal != 1960 {
		t.Fatalf("aggregate = %#v", snapshot)
	}
	if want := []resourceCPU{{Total: 960, Idle: 800}, {Total: 1000, Idle: 600}}; !slices.Equal(snapshot.Cores, want) {
		t.Fatalf("cores = %#v, want %#v", snapshot.Cores, want)
	}
}

func TestReadDarwinCoreTicks(t *testing.T) {
	first, err := readDarwinCoreTicks()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != runtime.NumCPU() {
		t.Fatalf("cores = %d, want %d", len(first), runtime.NumCPU())
	}
	// 반복 호출은 매번 결과 배열을 돌려준다. vm_deallocate가 실패하면 여기서 오류로 드러난다.
	var last []darwinCoreTicks
	for range 1000 {
		if last, err = readDarwinCoreTicks(); err != nil {
			t.Fatal(err)
		}
	}
	for index, core := range first {
		before := core.User + core.System + core.Idle + core.Nice
		after := last[index].User + last[index].System + last[index].Idle + last[index].Nice
		if after < before {
			t.Fatalf("core %d ticks went backwards: %d -> %d", index, before, after)
		}
	}
}

func TestReadDarwinMemoryPressure(t *testing.T) {
	level, err := readDarwinMemoryPressure()
	if err != nil || !level.known() || level > topMemoryPressureCritical {
		t.Fatalf("level = %d, %v", level, err)
	}
}

// df는 파일시스템 종류를 주지 않으므로 getfsstat에서 채운다. 루트 마운트는 종류를 가져야 한다.
func TestCollectDisksFillsTheFilesystemTypeOnDarwin(t *testing.T) {
	disks, err := collectDisks()
	if err != nil {
		t.Skip(err)
	}
	for _, disk := range disks {
		if disk.Mount == "/" {
			if disk.FSType == "" {
				t.Fatalf("the root mount has no filesystem type: %+v", disk)
			}
			return
		}
	}
	t.Skip("no root mount in the df output")
}
