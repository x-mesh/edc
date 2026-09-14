//go:build darwin

package edc

import (
	"encoding/binary"
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"
)

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
