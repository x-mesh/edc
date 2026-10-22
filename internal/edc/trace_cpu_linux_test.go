//go:build linux

package edc

import (
	"bufio"
	"debug/elf"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCPUOnlineCPUs(t *testing.T) {
	for text, want := range map[string][]int{"0\n": {0}, "0-3": {0, 1, 2, 3}, "0-1,4,6-7\n": {0, 1, 4, 6, 7}} {
		got, err := cpuOnlineCPUs(text)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %v %v, want %v", text, got, err, want)
		}
	}
	for _, bad := range []string{"", "a-b", "3-1"} {
		if got, err := cpuOnlineCPUs(bad); err == nil {
			t.Errorf("%q = %v, want an error", bad, got)
		}
	}
}

// cpuTestMapping은 이 test binary에서 pc를 덮는 /proc/self/maps의 mapping이다.
func cpuTestMapping(t *testing.T, pc uint64) cpuMapping {
	t.Helper()
	maps, err := os.Open("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	defer maps.Close()
	mappings, err := parseCPUMaps(bufio.NewReader(maps), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, mapping := range mappings {
		if pc >= mapping.start && pc < mapping.end {
			return mapping
		}
	}
	t.Fatalf("no executable mapping covers %x", pc)
	return cpuMapping{}
}

func TestCPUSymbolizerResolvesThisBinary(t *testing.T) {
	pc := uint64(reflect.ValueOf(cpuPercent).Pointer())
	const want = "github.com/x-mesh/edc/internal/edc.cpuPercent"
	comm, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatal(err)
	}
	symbolizer := newCPUSymbolizer("/proc")
	key := cpuProcessKey{TGID: uint32(os.Getpid()), Comm: strings.TrimSpace(string(comm))}
	symbolizer.snapshot(key)
	if got := symbolizer.userFrame(key, pc+1); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
	// -s로 심볼표를 지운 binary와 같은 길이다. ELF 심볼 없이 .gopclntab만으로 같은 이름을 찾는다.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file, err := elf.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	stripped := &cpuSymbolFile{gopcln: cpuGoPCLN(file)}
	for _, program := range file.Progs {
		if program.Type == elf.PT_LOAD {
			stripped.loads = append(stripped.loads, program.ProgHeader)
		}
	}
	if stripped.gopcln == nil {
		t.Fatal("no .gopclntab in the test binary")
	}
	mapping := cpuTestMapping(t, pc)
	if got, ok := stripped.lookup(pc + 1 - mapping.start + mapping.offset); !ok || got != want {
		t.Fatalf("pclntab frame = %q %v, want %q", got, ok, want)
	}
}

//go:noinline
func cpuTestSpin(until time.Time) uint64 {
	var sum uint64
	for time.Now().Before(until) {
		for index := uint64(0); index < 100_000; index++ {
			sum += index * index
		}
	}
	return sum
}

// cpuTestSleep은 10ms nanosleep을 되풀이한다. Go runtime을 거치지 않는 syscall이라 이 thread가 그대로 잠든다.
//
//go:noinline
func cpuTestSleep(until time.Time) {
	request := syscall.Timespec{Nsec: int64(10 * time.Millisecond)}
	for time.Now().Before(until) {
		_ = syscall.Nanosleep(&request, nil)
	}
}

// TestCPUProfileHelper는 아래 시험이 띄우는 process다. spin은 사용자 공간에서, syscall은 kernel 안에서 CPU를 쓴다.
func TestCPUProfileHelper(t *testing.T) {
	until := time.Now().Add(4 * time.Second)
	switch os.Getenv("EDC_CPU_HELPER") {
	case "spin":
		cpuTestSpin(until)
	case "syscall":
		for time.Now().Before(until) {
			for range 10_000 {
				syscall.Getppid()
			}
		}
	case "sleep":
		cpuTestSleep(until)
	default:
		t.Skip("helper process for TestTraceCPUSamplesUserAndKernelStacks")
	}
}

// CI의 tls-smoke job이 root로 이 시험을 돌린다. runner kernel의 verifier가 cpuEvents를 받는지, 샘플 수가 frequency와
// 맞는지, 사용자 stack과 kernel stack에 이름이 붙는지를 본다.
func TestTraceCPUSamplesUserAndKernelStacks(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace cpu needs BPF and perf capabilities")
	}
	if err := cpuTracePrerequisites(); err != nil {
		t.Skip(err)
	}
	const frequency, duration = 199, 2 * time.Second
	for _, mode := range []string{"spin", "syscall"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestCPUProfileHelper$", "-test.count=1")
			command.Env = append(os.Environ(), "EDC_CPU_HELPER="+mode)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
			// helper가 Go runtime을 띄우고 일을 시작할 때까지 기다린다.
			time.Sleep(300 * time.Millisecond)
			stop := make(chan struct{})
			go func() { time.Sleep(duration); close(stop) }()
			stacks, summary, err := collectCPUStacks(cpuTraceOptions{frequency: frequency, pids: []uint32{uint32(command.Process.Pid)}}, stop)
			if err != nil {
				t.Fatal(err)
			}
			report := cpuReportFor(stacks, summary)
			// helper는 thread 하나로 CPU 하나를 채운다. 그 CPU의 샘플이 거의 모두 helper다.
			expected := float64(frequency) * float64(summary.DurationMS) / 1000
			if got := float64(report.Summary.Profiled); got < expected*0.8 || got > expected*1.1 {
				t.Errorf("profiled %v samples, want about %.0f: %+v", got, expected, report.Summary)
			}
			// helper가 CPU 하나를 채우므로 바쁜 비율은 적어도 CPU 하나만큼이다. 쉬는 CPU의 샘플 수는 VM에 따라 모자라 보지 않는다.
			if minimum := 80 / float64(summary.CPUs); report.Summary.BusyPercent == nil || *report.Summary.BusyPercent < minimum || *report.Summary.BusyPercent > 100 {
				t.Errorf("busy = %v on %d CPUs, want at least %.1f%%", report.Summary.BusyPercent, summary.CPUs, minimum)
			}
			var hot, tRunner, kernel, getppid uint64
			for _, stack := range stacks {
				joined := strings.Join(stack.User, ";")
				if mode == "spin" && len(stack.User) > 0 && strings.HasSuffix(stack.User[0], ".cpuTestSpin") {
					hot += stack.Samples
				}
				if strings.Contains(joined, "testing.tRunner") {
					tRunner += stack.Samples
				}
				if len(stack.Kernel) > 0 {
					kernel += stack.Samples
				}
				for _, frame := range stack.Kernel {
					if strings.Contains(frame, "getppid") {
						getppid += stack.Samples
						break
					}
				}
			}
			profiled := report.Summary.Profiled
			switch mode {
			case "spin":
				type other struct {
					top     string
					samples uint64
				}
				var others []other
				var otherTotal uint64
				for _, stack := range stacks {
					if len(stack.User) > 0 && strings.HasSuffix(stack.User[0], ".cpuTestSpin") {
						continue
					}
					frames := stack.User
					if len(frames) > 3 {
						frames = frames[:3]
					}
					top := strings.Join(frames, " < ")
					if top == "" {
						top = "(no user frame)"
					}
					if len(stack.Kernel) > 0 {
						top += " [kernel " + stack.Kernel[0] + "]"
					}
					others = append(others, other{top, stack.Samples})
					otherTotal += stack.Samples
				}
				sort.Slice(others, func(i, j int) bool { return others[i].samples > others[j].samples })
				t.Logf("DIAG hot=%d profiled=%d nonhot=%d ratio=%.1f%%", hot, profiled, otherTotal, float64(hot)*100/float64(profiled))
				for index, entry := range others {
					if index >= 6 {
						break
					}
					t.Logf("DIAG other: %d %s", entry.samples, entry.top)
				}
				if hot*10 < profiled*9 {
					t.Errorf("cpuTestSpin is the innermost frame in %d of %d samples", hot, profiled)
				}
			case "syscall":
				// 이 host의 실측에서 kernel stack이 있는 샘플은 36%였다. 진입과 복귀 구간은 interrupt가 막혀 있어, 그때 온 샘플은
				// 사용자 공간으로 돌아간 뒤 syscall 명령 자리로 잡힌다. getppid 자체는 짧아서 이름이 붙은 샘플은 더 적다.
				if kernel*5 < profiled || getppid == 0 {
					t.Errorf("kernel stacks in %d and a getppid frame in %d of %d samples", kernel, getppid, profiled)
				}
			}
			// frame pointer로 Go stack 전체를 따라가면 test를 부른 testing.tRunner까지 보인다.
			if tRunner*10 < profiled*9 {
				t.Errorf("testing.tRunner is in %d of %d samples", tRunner, profiled)
			}
			if len(report.Functions) > 0 {
				t.Logf("%s: %d samples, %.1f expected, top %s self %.1f%%", mode, profiled, expected, report.Functions[0].Function, report.Functions[0].SelfPercent)
			}
		})
	}
}

// 외부 linker는 C 시작 코드를 .text 앞에 두어 runtime.text가 .text의 시작보다 뒤에 온다. 심볼표 없이 찾은 runtime.text가
// 실제 심볼과 같은지, 그리고 그 값으로 pclntab이 함수 이름을 맞게 찾는지 본다. PIE build는 .gopclntab이 없을 수 있다.
func TestCPUGoTextMatchesRuntimeText(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("needs the go tool")
	}
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("needs gcc for external linking")
	}
	dir := t.TempDir()
	source := "package main\n\n//go:noinline\nfunc hot(n int) int { for i := 0; i < n; i++ { n ^= i }; return n }\n\nfunc main() { println(hot(3)) }\n"
	if err := os.WriteFile(dir+"/main.go", []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/go.mod", []byte("module cputext\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"external":     {"-ldflags=-linkmode=external"},
		"external-pie": {"-buildmode=pie", "-ldflags=-linkmode=external"},
	} {
		t.Run(name, func(t *testing.T) {
			output := dir + "/" + name
			build := exec.Command(goTool, append([]string{"build", "-o", output}, args...)...)
			build.Dir = dir
			build.Env = append(os.Environ(), "CGO_ENABLED=1", "GOFLAGS=")
			if out, err := build.CombinedOutput(); err != nil {
				t.Skipf("build %s: %v\n%s", name, err, out)
			}
			file, err := elf.Open(output)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			symbols, err := file.Symbols()
			if err != nil {
				t.Fatal(err)
			}
			var runtimeText, hot uint64
			for _, symbol := range symbols {
				switch symbol.Name {
				case "runtime.text":
					runtimeText = symbol.Value
				case "main.hot":
					hot = symbol.Value
				}
			}
			text := file.Section(".text")
			candidates := cpuPCLNCandidates(file)
			if runtimeText == 0 || hot == 0 || text == nil || len(candidates) == 0 {
				t.Fatalf("runtime.text %x main.hot %x text %v candidates %d", runtimeText, hot, text, len(candidates))
			}
			if got := cpuGoText(file, text, candidates[0]); got != runtimeText {
				t.Fatalf("runtime.text = %x, want %x (.text starts at %x)", got, runtimeText, text.Addr)
			}
			table := cpuGoPCLN(file)
			if table == nil {
				t.Fatal("no pclntab")
			}
			if function := table.PCToFunc(hot + 1); function == nil || function.Name != "main.hot" {
				t.Fatalf("pclntab at main.hot = %+v", function)
			}
			t.Logf(".text %x runtime.text %x", text.Addr, runtimeText)
		})
	}
}

// 10ms nanosleep을 되풀이하는 thread 하나를 2초 기록한다. 이 thread가 잠든 시간의 합은 기록한 시간과 거의 같아야 하고,
// 처음과 끝에 걸친 두 번은 빠진다. kernel 진입은 nanosleep이어야 한다.
func TestTraceCPUOffCPURecordsSleepTime(t *testing.T) {
	capabilities, err := effectiveCapabilities()
	if err != nil || missingCapabilities(bpfTraceCapabilities, capabilities) != "" {
		t.Skip("trace cpu needs BPF and perf capabilities")
	}
	if err := cpuOffCPUPrerequisites(); err != nil {
		t.Skip(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCPUProfileHelper$", "-test.count=1")
	command.Env = append(os.Environ(), "EDC_CPU_HELPER=sleep")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	time.Sleep(300 * time.Millisecond)
	stop := make(chan struct{})
	go func() { time.Sleep(2 * time.Second); close(stop) }()
	stacks, summary, err := collectCPUStacks(cpuTraceOptions{offCPU: true, pids: []uint32{uint32(command.Process.Pid)}}, stop)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FrequencyHz != 0 || summary.BusyPercent != nil {
		t.Errorf("off-CPU summary has on-CPU fields: frequency %d busy %v", summary.FrequencyHz, summary.BusyPercent)
	}
	var slept, blocks uint64
	for _, stack := range stacks {
		if !strings.Contains(strings.Join(stack.User, ";"), ".cpuTestSleep") {
			continue
		}
		if kernel := cpuWaitKernel(stack.Kernel); kernel != "nanosleep" {
			t.Errorf("cpuTestSleep slept in %q: %v", kernel, stack.Kernel)
		}
		slept += stack.OffCPUNS
		blocks += stack.Samples
	}
	window := time.Duration(summary.DurationMS) * time.Millisecond
	if got := time.Duration(slept); got < window*9/10 || got > window {
		t.Errorf("cpuTestSleep slept %v in a %v trace", got, window)
	}
	// 한 번은 10ms에 timer slack과 깨어나는 시간을 더한 만큼 걸린다.
	if want := uint64(window / (10 * time.Millisecond)); blocks < want*8/10 || blocks > want {
		t.Errorf("cpuTestSleep slept %d times in %v, want about %d", blocks, window, want)
	}
	t.Logf("slept %v in %d blocks over %v", time.Duration(slept), blocks, window)
}
