//go:build darwin

package edc

import (
	"fmt"
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
