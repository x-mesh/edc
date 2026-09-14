// mach_darwin.go가 syscall.syscall6으로 부르는 libSystem trampoline이다. amd64와 arm64가 같은 문법을 쓴다.

#include "textflag.h"

TEXT machHostSelfTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·machHostSelfAddr(SB), RODATA, $8
DATA	·machHostSelfAddr(SB)/8, $machHostSelfTrampoline<>(SB)

TEXT hostProcessorInfoTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_processor_info(SB)
GLOBL	·hostProcessorInfoAddr(SB), RODATA, $8
DATA	·hostProcessorInfoAddr(SB)/8, $hostProcessorInfoTrampoline<>(SB)

TEXT vmDeallocateTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_vm_deallocate(SB)
GLOBL	·vmDeallocateAddr(SB), RODATA, $8
DATA	·vmDeallocateAddr(SB)/8, $vmDeallocateTrampoline<>(SB)

TEXT hostStatistics64Trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics64(SB)
GLOBL	·hostStatistics64Addr(SB), RODATA, $8
DATA	·hostStatistics64Addr(SB)/8, $hostStatistics64Trampoline<>(SB)

TEXT hostPageSizeTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_page_size(SB)
GLOBL	·hostPageSizeAddr(SB), RODATA, $8
DATA	·hostPageSizeAddr(SB)/8, $hostPageSizeTrampoline<>(SB)

TEXT sysctlTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sysctl(SB)
GLOBL	·sysctlAddr(SB), RODATA, $8
DATA	·sysctlAddr(SB)/8, $sysctlTrampoline<>(SB)

TEXT sysctlbynameTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_sysctlbyname(SB)
GLOBL	·sysctlbynameAddr(SB), RODATA, $8
DATA	·sysctlbynameAddr(SB)/8, $sysctlbynameTrampoline<>(SB)

TEXT taskSelfTrapTrampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_task_self_trap(SB)
GLOBL	·taskSelfTrapAddr(SB), RODATA, $8
DATA	·taskSelfTrapAddr(SB)/8, $taskSelfTrapTrampoline<>(SB)
