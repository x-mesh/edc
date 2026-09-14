//go:build darwin

package edc

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// macOS에는 CPU tick을 주는 sysctl이 없고 release는 CGO_ENABLED=0으로 빌드한다.
// 그래서 golang.org/x/sys/unix처럼 libSystem의 Mach 함수를 mach_darwin.s의 trampoline으로 부른다.

//go:linkname darwinSyscall6 syscall.syscall6
func darwinSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var machHostSelfAddr uintptr

//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"

var taskSelfTrapAddr uintptr

//go:cgo_import_dynamic libc_task_self_trap task_self_trap "/usr/lib/libSystem.B.dylib"

var hostProcessorInfoAddr uintptr

//go:cgo_import_dynamic libc_host_processor_info host_processor_info "/usr/lib/libSystem.B.dylib"

var vmDeallocateAddr uintptr

//go:cgo_import_dynamic libc_vm_deallocate vm_deallocate "/usr/lib/libSystem.B.dylib"

const (
	machProcessorCPULoadInfo = 2 // PROCESSOR_CPU_LOAD_INFO
	machCPUStateCount        = 4 // core마다 user, system, idle, nice 순서로 온다
)

// darwinCoreTicks는 core 하나가 부팅 뒤 쌓은 CPU tick이다.
type darwinCoreTicks struct{ User, System, Idle, Nice uint64 }

// port를 돌려주는 Mach 호출은 부를 때마다 port 참조를 하나씩 늘리므로 process당 한 번만 받는다.
// arm64 호출 규약은 32비트 반환값의 상위 비트를 보장하지 않으므로 port와 kern_return_t는 32비트로 자른다.
var (
	darwinHostPort = sync.OnceValue(func() uint32 {
		port, _, _ := darwinSyscall6(machHostSelfAddr, 0, 0, 0, 0, 0, 0)
		return uint32(port)
	})
	// C의 mach_task_self()는 libSystem 변수 mach_task_self_를 읽는 매크로다. Go 1.25 링커는 그 변수 주소를
	// 어셈블리 DATA에 채우지 못해 잘못된 주소를 읽고 멈추므로, 그 변수를 초기화하는 task_self_trap을 직접 부른다.
	darwinTaskPort = sync.OnceValue(func() uint32 {
		port, _, _ := darwinSyscall6(taskSelfTrapAddr, 0, 0, 0, 0, 0, 0)
		return uint32(port)
	})
)

// readDarwinCoreTicks는 host_processor_info로 core별 누적 tick을 읽는다.
func readDarwinCoreTicks() ([]darwinCoreTicks, error) {
	var count, infoCount uint32
	var info *uint32
	result, _, _ := darwinSyscall6(hostProcessorInfoAddr, uintptr(darwinHostPort()), machProcessorCPULoadInfo,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&infoCount)), 0)
	if code := int32(result); code != 0 {
		return nil, fmt.Errorf("host_processor_info: kern_return_t %d", code)
	}
	values := unsafe.Slice(info, infoCount)
	cores := make([]darwinCoreTicks, 0, count)
	for index := 0; index+machCPUStateCount <= len(values); index += machCPUStateCount {
		cores = append(cores, darwinCoreTicks{User: uint64(values[index]), System: uint64(values[index+1]), Idle: uint64(values[index+2]), Nice: uint64(values[index+3])})
	}
	// kernel이 결과 배열을 이 process에 새로 매핑하므로 돌려주지 않으면 호출마다 메모리가 쌓인다.
	address, size := uintptr(unsafe.Pointer(info)), uintptr(infoCount)*unsafe.Sizeof(*info)
	// 해제한 주소를 pointer 변수에 남기지 않는다. 나중에 Go heap이 그 주소를 쓰면 GC가 잘못된 pointer로 멈춘다.
	info = nil
	if result, _, _ := darwinSyscall6(vmDeallocateAddr, uintptr(darwinTaskPort()), address, size, 0, 0, 0); int32(result) != 0 {
		return nil, fmt.Errorf("vm_deallocate: kern_return_t %d", int32(result))
	}
	return cores, nil
}

var hostStatistics64Addr uintptr

//go:cgo_import_dynamic libc_host_statistics64 host_statistics64 "/usr/lib/libSystem.B.dylib"

var hostPageSizeAddr uintptr

//go:cgo_import_dynamic libc_host_page_size host_page_size "/usr/lib/libSystem.B.dylib"

var sysctlbynameAddr uintptr

//go:cgo_import_dynamic libc_sysctlbyname sysctlbyname "/usr/lib/libSystem.B.dylib"

const machHostVMInfo64 = 4 // HOST_VM_INFO64

// darwinVMStatistics64는 <mach/vm_statistics.h>의 vm_statistics64와 같은 순서와 크기(152 byte)다.
// vm_stat 출력과 값을 대조해 offset을 확인했다. vm_stat의 "Pages free"는 FreeCount에서 SpeculativeCount를 뺀 값이다.
type darwinVMStatistics64 struct {
	FreeCount, ActiveCount, InactiveCount, WireCount                                          uint32
	ZeroFillCount, Reactivations, Pageins, Pageouts, Faults, CowFaults, Lookups, Hits, Purges uint64
	PurgeableCount, SpeculativeCount                                                          uint32
	Decompressions, Compressions, Swapins, Swapouts                                           uint64
	CompressorPageCount, ThrottledCount, ExternalPageCount, InternalPageCount                 uint32
	TotalUncompressedPagesInCompressor                                                        uint64
}

// darwinPageSize는 kernel page 크기다. Rosetta의 amd64 process에서 hw.pagesize는 4096이지만
// host_statistics64의 page 수는 kernel page(Apple Silicon 16384) 단위라 host_page_size를 쓴다.
var darwinPageSize = sync.OnceValues(func() (uint64, error) {
	var size uintptr
	result, _, _ := darwinSyscall6(hostPageSizeAddr, uintptr(darwinHostPort()), uintptr(unsafe.Pointer(&size)), 0, 0, 0, 0)
	if code := int32(result); code != 0 {
		return 0, fmt.Errorf("host_page_size: kern_return_t %d", code)
	}
	return uint64(size), nil
})

// readDarwinVMStatistics는 host_statistics64로 page 단위 memory 통계를 읽는다.
func readDarwinVMStatistics() (darwinVMStatistics64, error) {
	var stats darwinVMStatistics64
	count := uint32(unsafe.Sizeof(stats) / unsafe.Sizeof(int32(0)))
	result, _, _ := darwinSyscall6(hostStatistics64Addr, uintptr(darwinHostPort()), machHostVMInfo64, uintptr(unsafe.Pointer(&stats)), uintptr(unsafe.Pointer(&count)), 0, 0)
	if code := int32(result); code != 0 {
		return darwinVMStatistics64{}, fmt.Errorf("host_statistics64: kern_return_t %d", code)
	}
	return stats, nil
}

// darwinSysctl은 sysctlbyname으로 고정 크기 값을 읽는다. kernel이 준 길이가 다르면 구조를 잘못 안 것이므로 실패로 본다.
func darwinSysctl(name string, value unsafe.Pointer, size uintptr) error {
	cname, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	length := size
	if result, _, errno := darwinSyscall6(sysctlbynameAddr, uintptr(unsafe.Pointer(cname)), uintptr(value), uintptr(unsafe.Pointer(&length)), 0, 0, 0); int32(result) != 0 {
		return fmt.Errorf("sysctlbyname %s: %w", name, errno)
	}
	if length != size {
		return fmt.Errorf("sysctlbyname %s: %d bytes, want %d", name, length, size)
	}
	return nil
}

// darwinLoadAverage는 <sys/sysctl.h>의 struct loadavg다. fixpt_t 세 개 뒤에 long fscale이 온다.
type darwinLoadAverage struct {
	Load  [3]uint32
	Scale int64
}

// readDarwinLoad1은 vm.loadavg에서 1분 load average를 읽는다.
func readDarwinLoad1() (float64, error) {
	var load darwinLoadAverage
	if err := darwinSysctl("vm.loadavg", unsafe.Pointer(&load), unsafe.Sizeof(load)); err != nil {
		return 0, err
	}
	if load.Scale == 0 {
		return 0, fmt.Errorf("vm.loadavg: zero fscale")
	}
	return float64(load.Load[0]) / float64(load.Scale), nil
}

// readDarwinMemorySize는 hw.memsize, 곧 물리 memory byte 수를 읽는다.
func readDarwinMemorySize() (uint64, error) {
	var total uint64
	err := darwinSysctl("hw.memsize", unsafe.Pointer(&total), unsafe.Sizeof(total))
	return total, err
}

var sysctlAddr uintptr

//go:cgo_import_dynamic libc_sysctl sysctl "/usr/lib/libSystem.B.dylib"

const (
	// darwinIfmibDataSize는 <net/if_mib.h>의 ifmibdata 크기다. 이름 16 byte와 u_int 9개 뒤에 if_data64(128 byte)가 온다.
	darwinIfmibDataSize = 180
	// darwinIfDataOffset은 if_data64의 시작이다. <net/if.h>의 pack(4) 때문에 8의 배수가 아닌 52에서 시작한다.
	// netstat -ibnd와 MTU, packet, byte, drop을 대조해 확인했다.
	darwinIfDataOffset = 52
)

// readDarwinNetworkCounters는 켜진 non-loopback interface의 kernel 통계를 더한다.
// NET_RT_IFLIST2의 byte counter는 32비트에서 넘어간 뒤 반올림된 값이라 쓰지 않고 net.link.generic.ifdata를 읽는다.
func readDarwinNetworkCounters() (darwinNetwork, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return darwinNetwork{}, err
	}
	var total darwinNetwork
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		data, err := readDarwinInterfaceData(iface.Index)
		if err != nil {
			return darwinNetwork{}, fmt.Errorf("%s: %w", iface.Name, err)
		}
		// 목록을 읽은 뒤 interface가 사라지고 index가 재사용되면 다른 interface를 더하게 된다.
		if name := strings.TrimRight(string(data[:16]), "\x00"); name != iface.Name {
			return darwinNetwork{}, fmt.Errorf("ifdata %d is %q, want %q", iface.Index, name, iface.Name)
		}
		counters := parseDarwinInterfaceData(data)
		total.packetsIn += counters.packetsIn
		total.packetsOut += counters.packetsOut
		total.bytesIn += counters.bytesIn
		total.bytesOut += counters.bytesOut
		total.errors += counters.errors
		total.drops += counters.drops
	}
	return total, nil
}

// readDarwinInterfaceData는 interface 하나의 ifmibdata를 읽는다.
func readDarwinInterfaceData(index int) ([]byte, error) {
	// CTL_NET, PF_LINK, NETLINK_GENERIC, IFMIB_IFDATA, interface index, IFDATA_GENERAL
	mib := [6]int32{4, 18, 0, 2, int32(index), 1}
	data := make([]byte, darwinIfmibDataSize)
	length := uintptr(len(data))
	if result, _, errno := darwinSyscall6(sysctlAddr, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&length)), 0, 0); int32(result) != 0 {
		return nil, fmt.Errorf("sysctl ifdata %d: %w", index, errno)
	}
	if length != darwinIfmibDataSize {
		return nil, fmt.Errorf("sysctl ifdata %d: %d bytes, want %d", index, length, darwinIfmibDataSize)
	}
	return data, nil
}

// parseDarwinInterfaceData는 ifmibdata에서 send queue drop과 if_data64의 packet, error, byte counter를 읽는다.
func parseDarwinInterfaceData(data []byte) darwinNetwork {
	counter := func(offset int) uint64 { return binary.LittleEndian.Uint64(data[darwinIfDataOffset+offset:]) }
	return darwinNetwork{
		packetsIn: counter(24), packetsOut: counter(40),
		bytesIn: counter(64), bytesOut: counter(72),
		errors: counter(32) + counter(48),
		drops:  uint64(binary.LittleEndian.Uint32(data[32:36])),
	}
}
