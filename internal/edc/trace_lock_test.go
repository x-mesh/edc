package edc

import (
	"debug/elf"
	"strings"
	"testing"
)

func TestLockTypeNamesFollowPerf(t *testing.T) {
	for flags, want := range map[uint32]string{
		lockFlagSpin: "spinlock", lockFlagSpin | lockFlagRead: "rwlock:R", lockFlagSpin | lockFlagWrite: "rwlock:W",
		lockFlagRead: "rwsem:R", lockFlagWrite: "rwsem:W", lockFlagMutex: "mutex", lockFlagRT: "rtmutex",
		lockFlagPerCPU | lockFlagRead: "pcpu-sem:R", lockFlagPerCPU | lockFlagWrite: "pcpu-sem:W", 0x80: "flags:0x80",
	} {
		if got := lockTypeName(flags); got != want {
			t.Errorf("flags %#x = %q, want %q", flags, got, want)
		}
	}
}

func TestLockCallersSkipLockAndTraceFrames(t *testing.T) {
	kernel := []string{"bpf_prog_9a70_lock_contention_begin", "bpf_trace_run2", "__bpf_trace_contention_begin", "rwsem_down_read_slowpath", "down_read_killable", "lock_mm_and_find_vma", "do_user_addr_fault"}
	if got := lockKernelCaller(kernel); got != "lock_mm_and_find_vma" {
		t.Errorf("kernel caller = %q", got)
	}
	if got := lockKernelCaller([]string{"__raw_spin_lock_irqsave", "queued_spin_lock_slowpath"}); got != "-" {
		t.Errorf("all primitives = %q", got)
	}
	for want, frames := range map[string][]string{"-": nil, "futex": {"futex"}, "futex ← wait": {"futex", "wait", "main"}} {
		if got := lockUserCaller(frames); got != want {
			t.Errorf("user caller of %v = %q, want %q", frames, got, want)
		}
	}
}

func TestLockReportGroupsKernelAndFutexWaits(t *testing.T) {
	path := []string{"rwsem_down_write_slowpath", "down_write_killable", "vm_mmap_pgoff"}
	collection := lockCollection{samples: []lockSample{
		{Kind: lockKindKernel, Lock: 0xffff888000001000, Flags: lockFlagWrite, Process: "api", Frames: path, Count: 3, SumNS: 3_000_000, MaxNS: 2_000_000},
		{Kind: lockKindKernel, Lock: 0xffff888000001000, Flags: lockFlagWrite, Process: "db", Frames: path, Count: 1, SumNS: 500_000, MaxNS: 500_000},
		{Kind: lockKindKernel, Lock: 0xffffffff82000000, LockName: "rtnl_mutex", Flags: lockFlagMutex, Process: "ip", Frames: []string{"__mutex_lock", "rtnl_lock"}, Count: 1, SumNS: 100_000, MaxNS: 100_000},
		{Kind: lockKindFutex, Lock: 0x59a20c, LockName: "main.hotWord", Process: "app", TGID: 42, Frames: []string{"syscall", "worker"}, Count: 5, SumNS: 9_000_000, MaxNS: 2_000_000},
		{Kind: lockKindFutex, Lock: 0x59a20c, LockName: "main.hotWord", Process: "app", TGID: 42, Frames: []string{"syscall", "other"}, Count: 1, SumNS: 1_000_000, MaxNS: 1_000_000},
		{Kind: lockKindFutex, Lock: 0x7f0000001000, LockName: "[anon]", Process: "app", TGID: 42, Count: 2, SumNS: 200_000, MaxNS: 150_000},
	}}
	report := lockReportFor(collection)
	if report.Summary.Waits != 13 || report.Summary.StatsEntries != 6 || report.Summary.TotalMS != 13.8 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if len(report.Kernel) != 2 || len(report.Futex) != 2 {
		t.Fatalf("kernel %+v futex %+v", report.Kernel, report.Futex)
	}
	mm := report.Kernel[0]
	if mm.Lock != "0xffff888000001000" || mm.Type != "rwsem:W" || mm.Caller != "vm_mmap_pgoff" || mm.Waits != 4 || mm.TotalMS != 3.5 || mm.MaxMS != 2 || mm.Processes["api"] != 3 || lockTopProcess(mm.Processes) != "api" {
		t.Fatalf("mmap row = %+v", mm)
	}
	if rtnl := report.Kernel[1]; rtnl.Lock != "rtnl_mutex" || rtnl.Type != "mutex" || rtnl.Caller != "rtnl_lock" {
		t.Fatalf("rtnl row = %+v", rtnl)
	}
	hot := report.Futex[0]
	if hot.Lock != "main.hotWord" || hot.PID != 42 || hot.Waits != 6 || hot.TotalMS != 10 || hot.Caller != "syscall ← worker" || hot.Processes != nil {
		t.Fatalf("futex row = %+v", hot)
	}
	if anon := report.Futex[1]; anon.Lock != "[anon] 0x7f0000001000" || anon.Caller != "-" {
		t.Fatalf("anon row = %+v", anon)
	}
	var out strings.Builder
	report.print(&out)
	for _, want := range []string{"LOCK TRACE", "KERNEL LOCK", "rtnl_mutex", "vm_mmap_pgoff", "main.hotWord", "syscall ← worker"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output misses %q:\n%s", want, out.String())
		}
	}
	report.Summary.Unavailable = map[string]string{lockKindKernel: "kernel BTF has no btf_trace_contention_begin"}
	out.Reset()
	report.print(&out)
	if strings.Contains(out.String(), "KERNEL LOCK") {
		t.Errorf("printed the kernel table without the hook:\n%s", out.String())
	}
}

// .data 변수는 file mapping 안에서, .bss 변수는 그 뒤 익명 mapping에서 load bias로 찾는다.
func TestCPUSymbolizerNamesDataAddresses(t *testing.T) {
	symbolizer := newCPUSymbolizer("/nonexistent")
	file := &cpuSymbolFile{base: "app", loads: []elf.ProgHeader{{Type: elf.PT_LOAD, Off: 0x1000, Vaddr: 0x401000, Filesz: 0x1000, Memsz: 0x3000}},
		objects: cpuSortSymbols([]cpuSymbol{{addr: 0x402800, size: 16, name: "bssVar"}, {addr: 0x401100, size: 8, name: "dataVar"}})}
	symbolizer.files["fd:01:9"] = &cpuFileRef{base: "app", loaded: file}
	key := cpuProcessKey{TGID: 7, Comm: "app"}
	symbolizer.processes[key] = &cpuProcess{mappings: []cpuMapping{
		{start: 0x70001000, end: 0x70002000, offset: 0x1000, path: "/usr/bin/app", key: "fd:01:9"},
		{start: 0x70002000, end: 0x70004000},
		{start: 0x80000000, end: 0x80100000, path: "[heap]"},
	}}
	// 0x70001f00은 file mapping의 마지막 page지만 .data 끝(Filesz)을 넘은 .bss 자리다.
	file.objects = cpuSortSymbols(append(file.objects, cpuSymbol{addr: 0x401f00, size: 4, name: "tailBss"}))
	file.loads[0].Filesz = 0xe00
	for addr, want := range map[uint64]string{0x70001100: "dataVar", 0x70001104: "dataVar+0x4", 0x70001f00: "tailBss", 0x70002808: "bssVar+0x8", 0x70001500: "[app]", 0x80000010: "[heap]", 0x90000000: "[anon]"} {
		if got := symbolizer.userDataName(key, addr); got != want {
			t.Errorf("data %#x = %q, want %q", addr, got, want)
		}
	}
	symbolizer.kernel = []cpuSymbol{{addr: 0xffffffff81000000, name: "_stext"}}
	symbolizer.kernelData = cpuSortSymbols([]cpuSymbol{{addr: 0xffffffff81000000, name: "_stext"}, {addr: 0xffffffff82000000, name: "rtnl_mutex"}})
	for addr, want := range map[uint64]string{0xffffffff82000000: "rtnl_mutex", 0xffffffff82000010: "rtnl_mutex+0x10", 0xffff888000001000: "", 0xffffffff82100000: ""} {
		if got := symbolizer.kernelDataName(addr); got != want {
			t.Errorf("kernel data %#x = %q, want %q", addr, got, want)
		}
	}
}
